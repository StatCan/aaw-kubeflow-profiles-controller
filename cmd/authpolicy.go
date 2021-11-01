package cmd

import (
	"context"
	"fmt"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	rbacv1listers "k8s.io/client-go/listers/rbac/v1"

	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	istiosecurity "istio.io/api/security/v1beta1"
	istiosecurityclient "istio.io/client-go/pkg/apis/security/v1beta1"
	istioclientset "istio.io/client-go/pkg/clientset/versioned"
	istioinformers "istio.io/client-go/pkg/informers/externalversions"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

const useridheader = "kubeflow-userid"
const useridprefix = ""

// Get all contributors on a namespace
func generateAccessList(profileName string, roleBindingsLister rbacv1listers.RoleBindingLister) ([]string, error) {
	access := []string{}

	// Find contributors in namespaces
	roleBindings, err := roleBindingsLister.RoleBindings(profileName).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, roleBinding := range roleBindings {
		if val, ok := roleBinding.Annotations["role"]; !ok || val != "edit" {
			continue
		}

		for _, subject := range roleBinding.Subjects {
			if subject.APIGroup != "rbac.authorization.k8s.io" || subject.Kind != "User" {
				klog.Warningf("skipping non-user membership on role binding %s in namespace %s", roleBinding.Name, roleBinding.Namespace)
				continue
			}

			access = append(access, subject.Name)
		}
	}

	return access, nil
}

var authPoliciesCmd = &cobra.Command{
	Use:   "auth-policies",
	Short: "Configure Authorization Policies",
	Long: `Configure Authorization Policies for Kubeflow profiles.
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

		istioClient, err := istioclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Istio client: %v", err)
		}

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*5)
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*5)
		istioInformerFactory := istioinformers.NewSharedInformerFactory(istioClient, time.Minute*5)
		authPolicyInformer := istioInformerFactory.Security().V1beta1().AuthorizationPolicies()
		authPolicyLister := authPolicyInformer.Lister()
		rbacInformer := kubeInformerFactory.Rbac().V1().RoleBindings()
		rbacLister := rbacInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {

				collaborators, err := generateAccessList(profile.Name, rbacLister)
				if err != nil {
					return err
				}

				// Generate Authorization Policies
				authPolicies := generateauthPolicies(profile, collaborators)

				// Delete s no longer needed
				for _, authPolicyName := range []string{} {
					_, err := authPolicyLister.AuthorizationPolicies(profile.Name).Get(authPolicyName)
					if err == nil {
						klog.Infof("removing authorization policy %s/%s", profile.Name, authPolicyName)
						err = istioClient.SecurityV1beta1().AuthorizationPolicies(profile.Name).Delete(context.Background(), authPolicyName, metav1.DeleteOptions{})
						if err != nil {
							return err
						}
					}
				}

				// Create
				for _, AuthPolicy := range authPolicies {
					currentAuthPolicy, err := authPolicyLister.AuthorizationPolicies(AuthPolicy.Namespace).Get(AuthPolicy.Name)
					if errors.IsNotFound(err) {
						klog.Infof("creating authorization policy %s/%s", AuthPolicy.Namespace, AuthPolicy.Name)
						currentAuthPolicy, err = istioClient.SecurityV1beta1().AuthorizationPolicies(AuthPolicy.Namespace).Create(context.Background(), AuthPolicy, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}

					if !equality.Semantic.DeepDerivative(AuthPolicy.Spec, currentAuthPolicy.Spec) {
						klog.Infof("updating authorization policy %s/%s", AuthPolicy.Namespace, AuthPolicy.Name)
						currentAuthPolicy.Spec = AuthPolicy.Spec

						_, err = istioClient.SecurityV1beta1().AuthorizationPolicies(AuthPolicy.Namespace).Update(context.Background(), currentAuthPolicy, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				return nil
			},
		)

		authPolicyInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*istiosecurityclient.AuthorizationPolicy)
				oldNP := old.(*istiosecurityclient.AuthorizationPolicy)

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
		istioInformerFactory.Start(stopCh)

		// Wait for caches
		klog.Info("Waiting for informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, authPolicyInformer.Informer().GetController().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

// generateauthPolicies generates  authPolicies for the given profile.
// TODO: Allow overrides in a namespace
func generateauthPolicies(profile *kubeflowv1.Profile, collaborators []string) []*istiosecurityclient.AuthorizationPolicy {

	// Users of the namespace
	users := []string{useridprefix + profile.Spec.Owner.Name}
	for _, user := range collaborators {
		users = append(users, useridprefix+user)
	}

	authPolicies := []*istiosecurityclient.AuthorizationPolicy{}
	authPolicies = append(authPolicies, &istiosecurityclient.AuthorizationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "namespace-owner-access-istio",
			Namespace: profile.Name,
		},
		Spec: istiosecurity.AuthorizationPolicy{
			Action: istiosecurity.AuthorizationPolicy_ALLOW,
			// Empty selector == match all workloads in namespace
			Selector: nil,
			Rules: []*istiosecurity.Rule{
				{
					When: []*istiosecurity.Condition{
						{
							Key:    fmt.Sprintf("request.headers[%v]", useridheader),
							Values: users,
						},
					},
				},
				{
					When: []*istiosecurity.Condition{
						{
							Key:    fmt.Sprintf("source.namespace"),
							Values: []string{profile.Name},
						},
					},
				},
				{
					To: []*istiosecurity.Rule_To{
						{
							Operation: &istiosecurity.Operation{
								Paths: []string{
									"/healthz",
									"/metrics",
									"/wait-for-drain",
								},
							},
						},
					},
				},
				{
					From: []*istiosecurity.Rule_From{
						{
							Source: &istiosecurity.Source{
								Principals: []string{
									"cluster.local/ns/kubeflow/sa/notebook-controller-service-account",
								},
							},
						},
					},
					To: []*istiosecurity.Rule_To{
						{
							Operation: &istiosecurity.Operation{
								Methods: []string{
									"GET",
								},
								Paths: []string{
									"*/api/status",
								},
							},
						},
					},
				},
			},
		},
	})

	return authPolicies
}

func init() {
	rootCmd.AddCommand(authPoliciesCmd)
}
