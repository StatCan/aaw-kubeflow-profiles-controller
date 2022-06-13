package cmd

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	istionetworkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingclient "istio.io/client-go/pkg/apis/networking/v1beta1"

	istioclientset "istio.io/client-go/pkg/clientset/versioned"
	istioinformers "istio.io/client-go/pkg/informers/externalversions"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	// project packages
)

const CLOUD_MAIN_GITLAB_HOST = "gitlab.k8s.cloud.statcan.ca"
const ISTIO_EGRESS_GATEWAY_SVC = "egress-gateway"
const CLOUD_MAIN_SYSTEM_NAMESPACE = "cloud-main-system"

const HTTPS_PORT = 443
const SSH_PORT = 22

var cloudMainCmd = &cobra.Command{
	Use:   "cloud-main",
	Short: "Configure cloud-main resources",
	Long:  `Configure resources that enable connectivity to certain cloud-main services for StatCan employees.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()
		// Create Kubernetes configuration
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("Error building kubeconfig: %v", err)
		}
		// Get clients
		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building Kubeflow client: %v", err)
		}

		istioClient, err := istioclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Istio client: %v", err)
		}

		// Setup Informer Factories
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))
		istioInformerFactory := istioinformers.NewSharedInformerFactory(istioClient, time.Minute*(time.Duration(requeue_time)))

		// Setup informers
		virtualServiceInformer := istioInformerFactory.Networking().V1beta1().VirtualServices()
		virtualServiceLister := virtualServiceInformer.Lister()

		// Initialize cloud-main controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// The virtual service should only be created for namespaces consisting only of
				// StatCan employees
				val, labelExists := profile.ObjectMeta.Labels["state.aaw.statcan.gc.ca/non-employee-users"]
				existsNonEmployeeUser, _ := strconv.ParseBool(val)
				if labelExists && !existsNonEmployeeUser {
					// Create the Istio virtual service
					virtualService, err := generateCloudMainVirtualService(profile)
					if err != nil {
						return err
					}
					currentVirtualService, err := virtualServiceLister.VirtualServices(profile.Name).Get(virtualService.Name)
					// If the virtual service is not found and the user has opted into having source control, create the virtual service.
					if errors.IsNotFound(err) {
						// Always create the virtual service
						klog.Infof("Creating Istio virtualservice %s/%s", virtualService.Namespace, virtualService.Name)
						currentVirtualService, err = istioClient.NetworkingV1beta1().VirtualServices(virtualService.Namespace).Create(
							context.Background(), virtualService, metav1.CreateOptions{},
						)
					} else if !reflect.DeepEqual(virtualService.Spec, currentVirtualService.Spec) {
						klog.Infof("Updating Istio virtualservice %s/%s", virtualService.Namespace, virtualService.Name)
						currentVirtualService = virtualService
						_, err = istioClient.NetworkingV1beta1().VirtualServices(virtualService.Namespace).Update(
							context.Background(), currentVirtualService, metav1.UpdateOptions{},
						)
					}
				} else {
					fmt.Println("Non Employee User")
				}

				return nil
			},
		)
		// Declare Event Handlers for Informers
		virtualServiceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newDep := new.(*istionetworkingclient.VirtualService)
				oldDep := old.(*istionetworkingclient.VirtualService)

				if newDep.ResourceVersion == oldDep.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})
		// Start informers
		kubeflowInformerFactory.Start(stopCh)
		istioInformerFactory.Start(stopCh)

		// Wait for caches to sync
		klog.Info("Waiting for istio VirtualService informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, virtualServiceInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}
		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func generateCloudMainVirtualService(profile *kubeflowv1.Profile) (*istionetworkingclient.VirtualService, error) {
	// Get namespace from profile
	namespace := profile.Name
	// Create virtual service
	virtualService := istionetworkingclient.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloud-main-virtualservice",
			Namespace: namespace,
			// Indicate that the profile owns the virtualservice resource
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: istionetworkingv1beta1.VirtualService{
			Hosts: []string{
				CLOUD_MAIN_GITLAB_HOST,
			},
			Http: []*istionetworkingv1beta1.HTTPRoute{
				{
					Name: "cloud-main-https-route",
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Port: HTTPS_PORT,
						},
					},
					Route: []*istionetworkingv1beta1.HTTPRouteDestination{
						{
							Destination: &istionetworkingv1beta1.Destination{
								Host: fmt.Sprintf("%s.%s.svc.cluster.local", ISTIO_EGRESS_GATEWAY_SVC, CLOUD_MAIN_SYSTEM_NAMESPACE),
								Port: &istionetworkingv1beta1.PortSelector{
									Number: HTTPS_PORT,
								},
							},
						},
					},
				},
				{
					Name: "cloud-main-ssh-route",
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Port: SSH_PORT,
						},
					},
					Route: []*istionetworkingv1beta1.HTTPRouteDestination{
						{
							Destination: &istionetworkingv1beta1.Destination{
								Host: fmt.Sprintf("%s.%s.svc.cluster.local", ISTIO_EGRESS_GATEWAY_SVC, CLOUD_MAIN_SYSTEM_NAMESPACE),
								Port: &istionetworkingv1beta1.PortSelector{
									Number: SSH_PORT,
								},
							},
						},
					},
				},
			},
		},
	}
	return &virtualService, nil
}

func init() {
	rootCmd.AddCommand(cloudMainCmd)
}
