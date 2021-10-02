package cmd

import (
	"context"
	"reflect"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var rbacSupportGroups []string

var rbacCmd = &cobra.Command{
	Use:   "rbac",
	Short: "Configure role-based access control resources",
	Long: `Configure role-based access control (RBAC) resources for Kubeflow profiles.
* DAaaS-Support
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
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*5)
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*5)

		roleInformer := kubeInformerFactory.Rbac().V1().Roles()
		roleLister := roleInformer.Lister()

		roleBindingInformer := kubeInformerFactory.Rbac().V1().RoleBindings()
		roleBindingLister := roleBindingInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Generate network policies
				roles := generateRoles(profile)
				roleBindings := generateRoleBindings(profile)

				// Delete resources no longer needed
				deleteRoleBindings := []string{}
				if len(rbacSupportGroups) == 0 {
					deleteRoleBindings = append(deleteRoleBindings, "profile-support")
				}

				for _, roleBindingName := range deleteRoleBindings {
					_, err := roleBindingLister.RoleBindings(profile.Name).Get(roleBindingName)
					if err == nil {
						klog.Infof("removing role binding %s/%s", profile.Name, roleBindingName)
						err = kubeClient.RbacV1().RoleBindings(profile.Name).Delete(context.Background(), roleBindingName, metav1.DeleteOptions{})
						if err != nil {
							return err
						}
					}
				}

				// Create
				for _, role := range roles {
					currentRole, err := roleLister.Roles(role.Namespace).Get(role.Name)
					if errors.IsNotFound(err) {
						klog.Infof("creating role %s/%s", role.Namespace, role.Name)
						currentRole, err = kubeClient.RbacV1().Roles(role.Namespace).Create(context.Background(), role, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}

					if !reflect.DeepEqual(role.Rules, currentRole.Rules) {
						klog.Infof("updating role %s/%s", role.Namespace, role.Name)
						currentRole.Rules = role.Rules

						_, err = kubeClient.RbacV1().Roles(role.Namespace).Update(context.Background(), currentRole, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				for _, roleBinding := range roleBindings {
					currentRoleBinding, err := roleBindingLister.RoleBindings(roleBinding.Namespace).Get(roleBinding.Name)
					if errors.IsNotFound(err) {
						klog.Infof("creating role binding %s/%s", roleBinding.Namespace, roleBinding.Name)
						currentRoleBinding, err = kubeClient.RbacV1().RoleBindings(roleBinding.Namespace).Create(context.Background(), roleBinding, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}

					if !reflect.DeepEqual(roleBinding.RoleRef, currentRoleBinding.RoleRef) || !reflect.DeepEqual(roleBinding.Subjects, currentRoleBinding.Subjects) {
						klog.Infof("updating role %s/%s", roleBinding.Namespace, roleBinding.Name)
						currentRoleBinding.RoleRef = roleBinding.RoleRef
						currentRoleBinding.Subjects = roleBinding.Subjects

						_, err = kubeClient.RbacV1().RoleBindings(roleBinding.Namespace).Update(context.Background(), currentRoleBinding, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				return nil
			},
		)

		roleInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*rbacv1.Role)
				oldNP := old.(*rbacv1.Role)

				if newNP.ResourceVersion == oldNP.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		roleBindingInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*rbacv1.RoleBinding)
				oldNP := old.(*rbacv1.RoleBinding)

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
		if ok := cache.WaitForCacheSync(stopCh, roleInformer.Informer().HasSynced, roleBindingInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

// generateRoles generates roles for the given profile.
func generateRoles(profile *kubeflowv1.Profile) []*rbacv1.Role {
	roles := []*rbacv1.Role{}
	return roles
}

// generateRoleBindings generates role bindings for the given profile.
func generateRoleBindings(profile *kubeflowv1.Profile) []*rbacv1.RoleBinding {
	roleBindings := []*rbacv1.RoleBinding{}

	// DAaaS-AAW-Support is granted "profile-support" cluster role in this namespace
	if len(rbacSupportGroups) > 0 {
		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "profile-support",
				Namespace: profile.Name,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.SchemeGroupVersion.Group,
				Kind:     "ClusterRole",
				Name:     "profile-support",
			},
			Subjects: []rbacv1.Subject{},
		}

		for _, group := range rbacSupportGroups {
			roleBinding.Subjects = append(roleBinding.Subjects, rbacv1.Subject{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Group",
				Name:     group,
			})
		}

		roleBindings = append(roleBindings, roleBinding)
	}

	return roleBindings
}

func init() {
	rbacCmd.Flags().StringSliceVar(&rbacSupportGroups, "support-groups", []string{}, "List of groups assigned support permissions (comma separated or multiple args).")

	rootCmd.AddCommand(rbacCmd)
}
