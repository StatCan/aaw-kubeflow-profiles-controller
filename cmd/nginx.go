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
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var nginxEnabledLabel = "nginx.statcan.gc.ca/enabled"


var nginxCmd = &cobra.Command{
	Use:   "nginx",
	Short: "Configure nginx",
	Long: `Configure nginx for Kubeflow resources.* Statefulset	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("Error building kubeconfig: %v", err)
		}

		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building Kubeflow client: %v", err)
		}

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		deploymentInformer := kubeInformerFactory.Apps().V1().Deployments()
		deploymentLister := deploymentInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Only create nginx if a profile has opted in, as determined by sourcecontrol.statcan.gc.ca/enabled=true
				var replicas int32
				
				if val, ok := profile.Labels[nginxEnabledLabel]; ok && val == "true" {
					replicas = 1
				} else {
					replicas = 0
				}

				// Generate statefulset 
				deployment, err := generateNginxDeployment(profile, replicas)
				if err == nil {
					currentDeployment, err := deploymentLister.Deployments(deployment.Namespace).Get(deployment.Name)
					if errors.IsNotFound(err) {
						if replicas != 0 {
							klog.Infof("creating nginx deployment %s/%s", deployment.Namespace, deployment.Name)
							currentDeployment, err = kubeClient.AppsV1().Deployments(deployment.Namespace).Create(context.Background(), deployment, metav1.CreateOptions{})
							if err != nil {
								return err
							}
						}
					} else if !reflect.DeepEqual(deployment.Spec, currentDeployment.Spec) {
						klog.Infof("updating nginx deployment %s/%s", deployment.Namespace, deployment.Name)
						currentDeployment.Spec = deployment.Spec

						_, err = kubeClient.AppsV1().Deployments(deployment.Namespace).Update(context.Background(), currentDeployment, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				return nil
			},
		)

		deploymentInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newDep := new.(*appsv1.Deployment)
				oldDep := old.(*appsv1.Deployment)

				if newDep.ResourceVersion == oldDep.ResourceVersion {
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
		if ok := cache.WaitForCacheSync(stopCh, deploymentInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func generateNginxDeployment(profile *kubeflowv1.Profile, replicas int32) (*appsv1.Deployment, error) {

	containerPort := int32(80)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nginx",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string {
					"app": "nginx",
				},
			},
			Replicas: &replicas,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta {
					Labels: map[string]string {
						"app": "nginx",
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container {
						{
							Name: "nginx",
							Image: "nginx:1.14.2",
							Ports: []v1.ContainerPort {
								{
									ContainerPort: containerPort,
								},
							},
						},
					},
				},
			},
		},
	}

	return deployment, nil
}

func init() {
	rootCmd.AddCommand(nginxCmd)
}
