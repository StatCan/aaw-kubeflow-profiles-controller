package cmd

import (
	"context"
	"reflect"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	kubeflowv1alpha1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1alpha1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var notebookCmd = &cobra.Command{
	Use:   "notebook",
	Short: "Configure notebook related items",
	Long: `Configure notebook related items.
* PodDefaults
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Kubeflow client: %v", err)
		}

		// Setup informers
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*5)

		podDefaultInformer := kubeflowInformerFactory.Kubeflow().V1alpha1().PodDefaults()
		podDefaultLister := podDefaultInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Generate pod defaults
				podDefaults := generatePodDefaults(profile)

				for _, podDefault := range podDefaults {
					currentPodDefault, err := podDefaultLister.PodDefaults(podDefault.Namespace).Get(podDefault.Name)
					if errors.IsNotFound(err) {
						klog.Infof("creating pod default %s/%s", podDefault.Namespace, podDefault.Name)
						currentPodDefault, err = kubeflowClient.KubeflowV1alpha1().PodDefaults(podDefault.Namespace).Create(context.Background(), podDefault, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}

					if !reflect.DeepEqual(podDefault.Spec, currentPodDefault.Spec) {
						klog.Infof("updating network policy %s/%s", podDefault.Namespace, podDefault.Name)
						currentPodDefault.Spec = podDefault.Spec

						_, err = kubeflowClient.KubeflowV1alpha1().PodDefaults(podDefault.Namespace).Update(context.Background(), currentPodDefault, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				return nil
			},
		)

		podDefaultInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*kubeflowv1alpha1.PodDefault)
				oldNP := old.(*kubeflowv1alpha1.PodDefault)

				if newNP.ResourceVersion == oldNP.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		// Start informers
		kubeflowInformerFactory.Start(stopCh)

		// Wait for caches
		klog.Info("Waiting for informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, podDefaultInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func generatePodDefaults(profile *kubeflowv1.Profile) []*kubeflowv1alpha1.PodDefault {
	policies := []*kubeflowv1alpha1.PodDefault{}

	// Default Protected B deny
	policies = append(policies, &kubeflowv1alpha1.PodDefault{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "protected-b",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: kubeflowv1alpha1.PodDefaultSpec{
			Desc: "Run a Protected B notebook",
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"notebook.statcan.gc.ca/protected-b": "true",
				},
			},
			Labels: map[string]string{
				"data.statcan.gc.ca/classification": "protected-b",
			},
		},
	})

	return policies
}

func init() {
	rootCmd.AddCommand(notebookCmd)
}
