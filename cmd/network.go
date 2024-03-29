package cmd

import (
	"context"
	"reflect"
	"strconv"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

// Does the network policy have a Profile object as an owner?
// If yes, we own it.
func isOwnedByUs(netpol *networkingv1.NetworkPolicy) bool {
	owners := netpol.GetOwnerReferences()
	if owners != nil {
		for _, obj := range owners {
			if obj.Kind == "Profile" {
				return true
			}
		}
	}
	return false
}

var networkCmd = &cobra.Command{
	Use:   "network",
	Short: "Configure network resources",
	Long: `Configure network resources for Kubeflow resources.
* Network policies
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Kubeflow client: %v", err)
		}

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		networkPolicyInformer := kubeInformerFactory.Networking().V1().NetworkPolicies()
		networkPolicyLister := networkPolicyInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Generate network policies
				policies := generateNetworkPolicies(profile)

				// get network polices currently owned by the profile
				existingPolicies, _ := networkPolicyLister.NetworkPolicies(profile.Name).List(labels.Everything())

				// Cross-check against the new policies. Delete if
				// an existing policy owned by the Profile is not in the new list.
				for _, policy := range existingPolicies {
					if isOwnedByUs(policy) {

						// Delete if the existing policy is not in the new list
						queueDeletion := true
						for _, newPolicy := range policies {
							if policy.Name == newPolicy.Name {
								queueDeletion = false
								break
							}
						}

						if queueDeletion {
							klog.Infof("removing network policy %s/%s", profile.Name, policy.Name)
							err = kubeClient.NetworkingV1().NetworkPolicies(profile.Name).Delete(context.Background(), policy.Name, metav1.DeleteOptions{})
							if err != nil {
								return err
							}
						}
					}
				}

				for _, policyName := range []string{"notebooks-allow-ingress-gateway", "protected-b-notebooks-allow-ingress", "protected-b-allow-system", "protected-b-default-deny", "notebooks-allow-ingress"} {
					_, err := networkPolicyLister.NetworkPolicies(profile.Name).Get(policyName)
					if err == nil {
						klog.Infof("removing network policy %s/%s", profile.Name, policyName)
						err = kubeClient.NetworkingV1().NetworkPolicies(profile.Name).Delete(context.Background(), policyName, metav1.DeleteOptions{})
						if err != nil {
							return err
						}
					}
				}

				for _, policy := range policies {
					currentPolicy, err := networkPolicyLister.NetworkPolicies(policy.Namespace).Get(policy.Name)
					if errors.IsNotFound(err) {
						klog.Infof("creating network policy %s/%s", policy.Namespace, policy.Name)
						currentPolicy, err = kubeClient.NetworkingV1().NetworkPolicies(policy.Namespace).Create(context.Background(), policy, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}

					if !reflect.DeepEqual(policy.Spec, currentPolicy.Spec) {
						klog.Infof("updating network policy %s/%s", policy.Namespace, policy.Name)
						currentPolicy.Spec = policy.Spec

						_, err = kubeClient.NetworkingV1().NetworkPolicies(policy.Namespace).Update(context.Background(), currentPolicy, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				return nil
			},
		)

		networkPolicyInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*networkingv1.NetworkPolicy)
				oldNP := old.(*networkingv1.NetworkPolicy)

				if newNP.ResourceVersion == oldNP.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		// Start informers
		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		// Wait for caches
		klog.Info("Waiting for informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, networkPolicyInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func generateNetworkPolicies(profile *kubeflowv1.Profile) []*networkingv1.NetworkPolicy {
	policies := []*networkingv1.NetworkPolicy{}

	protocolTCP := corev1.ProtocolTCP
	portNotebook := intstr.FromString("notebook-port")

	// Allow ingress from the ingress gateway
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebooks-allow-system-to-notebook",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "notebook-name",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &protocolTCP,
							Port:     &portNotebook,
						},
					},
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"namespace.statcan.gc.ca/purpose": "system",
								},
							},
						},
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"namespace.statcan.gc.ca/purpose": "daaas",
								},
							},
						},
					},
				},
			},
		},
	})

	// Allow ingress from knative-serving
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-ingress-from-knative-serving",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "serving.knative.dev/service",
						Operator: metav1.LabelSelectorOpExists,
					},
					{
						Key:      "data.statcan.gc.ca/classification",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"protected-b"},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app.kubernetes.io/component": "knative-serving-install",
								},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress to the cluster local gateway
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-egress-to-cluster-local-gateway",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "data.statcan.gc.ca/classification",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"protected-b"},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"install.operator.istio.io/owner-name": "istio",
									"namespace.statcan.gc.ca/purpose":      "system",
								},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"istio": "ingressgateway-kfserving",
								},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress from unclassified workloads
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unclassified-allow-same-namespace",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "data.statcan.gc.ca/classification",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"protected-b"},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "data.statcan.gc.ca/classification",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"protected-b"},
									},
								},
							},
						},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "data.statcan.gc.ca/classification",
										Operator: metav1.LabelSelectorOpNotIn,
										Values:   []string{"protected-b"},
									},
								},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress from unclassified workloads
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unclassified-allow-egress",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "data.statcan.gc.ca/classification",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"protected-b"},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR:   "0.0.0.0/0",
								Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress from unclassified workloads
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unclassified-allow-egress-to-ingress-gateway",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "data.statcan.gc.ca/classification",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"protected-b"},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"install.operator.istio.io/owner-name": "istio",
									"namespace.statcan.gc.ca/purpose":      "system",
								},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"istio": "ingressgateway",
								},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress to 443 from protected-b workloads
	// This is need for Azure authentication
	portHTTPS := intstr.FromInt(443)
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebooks-protected-b-allow-https-egress",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "notebook-name",
						Operator: metav1.LabelSelectorOpExists,
					},
					{
						Key:      "data.statcan.gc.ca/classification",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"protected-b"},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &protocolTCP,
							Port:     &portHTTPS,
						},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR:   "0.0.0.0/0",
								Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress to Vault
	port8200 := intstr.FromInt(8200)
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vault-egress",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &protocolTCP,
							Port:     &port8200,
						},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"namespace.statcan.gc.ca/purpose": "daaas",
								},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app.kubernetes.io/instance": "vault",
									"component":                  "server",
								},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress to Elasticsearch
	port9200 := intstr.FromInt(9200)
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unclassified-elasticsearch-egress",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "data.statcan.gc.ca/classification",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"protected-b"},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &protocolTCP,
							Port:     &port9200,
						},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"namespace.statcan.gc.ca/purpose": "daaas",
								},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"common.k8s.elastic.co/type": "elasticsearch",
								},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress to Artifactory
	port8081 := intstr.FromInt(8081)
	port8082 := intstr.FromInt(8082)
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-jfrog-egress",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &protocolTCP,
							Port:     &port8081,
						},
						{
							Protocol: &protocolTCP,
							Port:     &port8082,
						},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"namespace.statcan.gc.ca/purpose": "daaas",
								},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "artifactory",
								},
							},
						},
					},
				},
			},
		},
	})
	portHttp := intstr.FromString("http")

	// Allow egress to postgres database for kubeflow profiles that opt into using Gitea.
	val, labelExists := profile.ObjectMeta.Labels["sourcecontrol.statcan.gc.ca/enabled"]
	profileOptsIntoSourceControl, _ := strconv.ParseBool(val)

	portPostgres := intstr.FromInt(5432)

	if labelExists && profileOptsIntoSourceControl {
		policies = append(policies, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-gitea-to-postgres",
				Namespace: profile.Name,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
				},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":                        "gitea",
						"app.kubernetes.io/instance": "gitea-protected-b",
					},
				},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						Ports: []networkingv1.NetworkPolicyPort{
							{
								Protocol: &protocolTCP,
								Port:     &portPostgres,
							},
						},
						To: []networkingv1.NetworkPolicyPeer{
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR: "0.0.0.0/0",
								},
							},
						},
					},
				},
			},
		})

		giteaPort80 := intstr.FromInt(80)
		giteaPort22 := intstr.FromInt(22)
		giteaPort3000 := intstr.FromInt(3000)

		// Allow ingress from the kubeflow gateway
		policies = append(policies, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gitea-allow-system-to-gitea",
				Namespace: profile.Name,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
				},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":                        "gitea",
						"app.kubernetes.io/instance": "gitea-unclassified",
					},
				},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						Ports: []networkingv1.NetworkPolicyPort{
							{
								Protocol: &protocolTCP,
								Port:     &portHttp,
							},
						},
						From: []networkingv1.NetworkPolicyPeer{
							{
								NamespaceSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"namespace.statcan.gc.ca/purpose": "system",
									},
								},
							},
							{
								NamespaceSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"namespace.statcan.gc.ca/purpose": "daaas",
									},
								},
							},
						},
					},
				},
			},
		})
		policies = append(policies, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-gitea-http-egress-prot-b",
				Namespace: profile.Name,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
				},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":                        "gitea",
						"app.kubernetes.io/instance": "gitea-protected-b",
					},
				},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						Ports: []networkingv1.NetworkPolicyPort{
							{
								Protocol: &protocolTCP,
								Port:     &giteaPort80,
							},
							{
								Protocol: &protocolTCP,
								Port:     &giteaPort3000,
							},
							{
								Protocol: &protocolTCP,
								Port:     &giteaPort22,
							},
						},
						To: []networkingv1.NetworkPolicyPeer{
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR: "0.0.0.0/0",
								},
							},
						},
					},
				},
			},
		})
		policies = append(policies, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-gitea-http-ingress-prot-b",
				Namespace: profile.Name,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
				},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":                        "gitea",
						"app.kubernetes.io/instance": "gitea-protected-b",
					},
				},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						Ports: []networkingv1.NetworkPolicyPort{
							{
								Protocol: &protocolTCP,
								Port:     &giteaPort3000,
							},
							{
								Protocol: &protocolTCP,
								Port:     &giteaPort22,
							},
						},
						From: []networkingv1.NetworkPolicyPeer{
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR: "0.0.0.0/0",
								},
							},
						},
					},
				},
			},
		})
	}

	// Employee-only namespaces are allowed egress to pods in the cloud-main-system namespace.
	// This network policy should only be created for namespaces whose state.aaw.statcan.gc.ca/non-employee-users
	// label indicates that the namespace does not contain any non-employee users.
	val, labelExists = profile.ObjectMeta.Labels["state.aaw.statcan.gc.ca/exists-non-cloud-main-user"]
	existsNonEmployeeUser, _ := strconv.ParseBool(val)

	portHttps := intstr.FromInt(443)
	altPortHttps := intstr.FromInt(8443)
	portSsh := intstr.FromInt(22)
	if labelExists && !existsNonEmployeeUser {
		policies = append(policies, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-employee-egress-to-cloud-main-system",
				Namespace: profile.Name,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
				},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "notebook-name",
							Operator: metav1.LabelSelectorOpExists,
						},
					},
				},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						Ports: []networkingv1.NetworkPolicyPort{
							{
								Protocol: &protocolTCP,
								Port:     &portHttps,
							},
							{
								Protocol: &protocolTCP,
								Port:     &altPortHttps,
							},
							{
								Protocol: &protocolTCP,
								Port:     &portSsh,
							},
						},
						To: []networkingv1.NetworkPolicyPeer{
							{
								NamespaceSelector: &metav1.LabelSelector{ // TODO: double check that these label selectors work as expected.
									MatchLabels: map[string]string{
										"kubernetes.io/metadata.name": "cloud-main-system",
									},
								},
							},
						},
					},
				},
			},
		})
	}

	// Allow ingress from system pods to s3proxy pods - this is necessary so that users can access
	// the s3-explorer UI from within the Kubeflow interface. Only allow this if the user has
	// opted into per-namespace source control.
	val, labelExists = profile.ObjectMeta.Labels["s3.statcan.gc.ca/enabled"]
	s3proxyEnabled, _ := strconv.ParseBool(val)

	if labelExists && s3proxyEnabled {
		policies = append(policies, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "s3proxy-allow-ingress",
				Namespace: profile.Name,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
				},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "s3proxy",
					},
				},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						Ports: []networkingv1.NetworkPolicyPort{
							{
								Protocol: &protocolTCP,
								Port:     &portHttp,
							},
						},
						From: []networkingv1.NetworkPolicyPeer{
							{
								NamespaceSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"namespace.statcan.gc.ca/purpose": "system",
									},
								},
							},
							{
								NamespaceSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"namespace.statcan.gc.ca/purpose": "daaas",
									},
								},
							},
						},
					},
				},
			},
		})
	}

	// Allow egress from protb notebooks to the trino protb instance in trino-protb-system
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-egress-protb-notebook-to-trino-protb-system",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"data.statcan.gc.ca/classification": "protected-b",
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"component": "coordinator",
								},
							},
						},
					},
				},
			},
		},
	})

	// Allow egress from unclassified notebooks to the trino unclassified instance in trino-system
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-egress-notebook-to-trino-system",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "notebook-name",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"release": "trino",
								},
							},
						},
					},
				},
			},
		},
	})

	return policies
}

func init() {
	rootCmd.AddCommand(networkCmd)
}
