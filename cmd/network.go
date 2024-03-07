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
									"namespace.statcan.gc.ca/purpose": "das",
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

	// Allow egress to 443 from protected-b workloads
	// This is need for Azure authentication
	portHTTPS := intstr.FromInt(443)
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebooks-allow-https-egress",
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


	// allow ingress from kubeflow
	policies = append(policies, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "allow-ingress-kubeflow-gateway",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: *&metav1.LabelSelector{},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kubernetes.io/metadata.name": "ingress-kubeflow",
								},
							},
						}
					},
				}
			},
		}
	})

	return policies
}

func init() {
	rootCmd.AddCommand(networkCmd)
}
