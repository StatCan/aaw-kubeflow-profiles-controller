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
const ISTIO_EGRESS_GATEWAY_SVC = "cloud-main-egress-gateway"
const ISTIO_SERVICE_ENTRY_NAME = "cloud-main-hosts"
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

		serviceEntryInformer := istioInformerFactory.Networking().V1beta1().ServiceEntries()
		serviceEntryLister := serviceEntryInformer.Lister()

		destinationRuleInformer := istioInformerFactory.Networking().V1beta1().DestinationRules()
		destinationRuleLister := destinationRuleInformer.Lister()

		// Initialize cloud-main controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// The virtual service should only be created for namespaces consisting only of
				// StatCan employees
				val, labelExists := profile.ObjectMeta.Labels["state.aaw.statcan.gc.ca/exists-non-cloud-main-user"]
				existsNonEmployeeUser, _ := strconv.ParseBool(val)
				if labelExists && !existsNonEmployeeUser {
					//        _      _               _                       _
					// __   _(_)_ __| |_ _   _  __ _| |  ___  ___ _ ____   _(_) ___ ___
					// \ \ / / | '__| __| | | |/ _` | | / __|/ _ \ '__\ \ / / |/ __/ _ \
					//  \ V /| | |  | |_| |_| | (_| | | \__ \  __/ |   \ V /| | (_|  __/
					//   \_/ |_|_|   \__|\__,_|\__,_|_| |___/\___|_|    \_/ |_|\___\___|
					virtualService, err := generateCloudMainVirtualService(profile)
					if err != nil {
						return err
					}
					currentVirtualService, err := virtualServiceLister.VirtualServices(profile.Name).Get(virtualService.Name)
					// If the virtual service is not found, create the virtual service.
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
					// 	                       _                       _
					// 	   ___  ___ _ ____   _(_) ___ ___    ___ _ __ | |_ _ __ _   _
					//    / __|/ _ \ '__\ \ / / |/ __/ _ \  / _ \ '_ \| __| '__| | | |
					//    \__ \  __/ |   \ V /| | (_|  __/ |  __/ | | | |_| |  | |_| |
					//    |___/\___|_|    \_/ |_|\___\___|  \___|_| |_|\__|_|   \__, |
					// 												     		|___/
					serviceEntry, err := generateCloudMainServiceEntry(profile)
					if err != nil {
						return err
					}
					currentServiceEntry, err := serviceEntryLister.ServiceEntries(profile.Name).Get(serviceEntry.Name)
					// If service entry not found, create it
					if errors.IsNotFound(err) {
						klog.Infof("Creating Istio serviceentry %s/%s", serviceEntry.Namespace, serviceEntry.Name)
						currentServiceEntry, err = istioClient.NetworkingV1beta1().ServiceEntries(serviceEntry.Namespace).Create(
							context.Background(), serviceEntry, metav1.CreateOptions{},
						)
					} else if !reflect.DeepEqual(serviceEntry.Spec, currentServiceEntry.Spec) {
						currentServiceEntry = serviceEntry
						_, err = istioClient.NetworkingV1beta1().ServiceEntries(serviceEntry.Namespace).Update(
							context.Background(), currentServiceEntry, metav1.UpdateOptions{},
						)
					}
					// 	      _           _   _             _   _                          _
					// 	   __| | ___  ___| |_(_)_ __   __ _| |_(_) ___  _ __    _ __ _   _| | ___
					//    / _` |/ _ \/ __| __| | '_ \ / _` | __| |/ _ \| '_ \  | '__| | | | |/ _ \
					//   | (_| |  __/\__ \ |_| | | | | (_| | |_| | (_) | | | | | |  | |_| | |  __/
					//    \__,_|\___||___/\__|_|_| |_|\__,_|\__|_|\___/|_| |_| |_|   \__,_|_|\___|
					destinationRule, err := generateCloudMainDestinationRule(profile)
					if err != nil {
						return err
					}
					currentDestinationRule, err := destinationRuleLister.DestinationRules(profile.Name).Get(destinationRule.Name)
					// If destination rule not found, create it
					if errors.IsNotFound(err) {
						klog.Infof("Creating Istio destinationrule %s/%s", destinationRule.Namespace, destinationRule.Name)
						currentDestinationRule, err = istioClient.NetworkingV1beta1().DestinationRules(destinationRule.Namespace).Create(
							context.Background(), destinationRule, metav1.CreateOptions{},
						)
					} else if !reflect.DeepEqual(destinationRule.Spec, currentDestinationRule.Spec) {
						currentDestinationRule = destinationRule
						_, err = istioClient.NetworkingV1beta1().DestinationRules(destinationRule.Namespace).Update(
							context.Background(), currentDestinationRule, metav1.UpdateOptions{},
						)
					}
				} else {
					// If the profile is missing the exists-non-cloud-main-user label or it is set to
					// 'true', then delete any of the per-namespace cloud main system istio components
					// that might be deployed.

					//        _      _               _                       _
					// __   _(_)_ __| |_ _   _  __ _| |  ___  ___ _ ____   _(_) ___ ___
					// \ \ / / | '__| __| | | |/ _` | | / __|/ _ \ '__\ \ / / |/ __/ _ \
					//  \ V /| | |  | |_| |_| | (_| | | \__ \  __/ |   \ V /| | (_|  __/
					//   \_/ |_|_|   \__|\__,_|\__,_|_| |___/\___|_|    \_/ |_|\___\___|
					virtualService, err := generateCloudMainVirtualService(profile)
					currentVirtualService, err := virtualServiceLister.VirtualServices(profile.Name).Get(virtualService.Name)
					if errors.IsNotFound(err) {
						klog.Infof("virtualservice %s/%s doesn't exist.", virtualService.Namespace, virtualService.Name)
					} else {
						klog.Infof("removing virtualservice %s/%s because %s contains a non-employee user.", currentVirtualService.Namespace, currentVirtualService.Name, profile.Name)
						err = istioClient.NetworkingV1beta1().VirtualServices(virtualService.Namespace).Delete(
							context.Background(), currentVirtualService.Name, metav1.DeleteOptions{},
						)
					}
					// 	                       _                       _
					// 	   ___  ___ _ ____   _(_) ___ ___    ___ _ __ | |_ _ __ _   _
					//    / __|/ _ \ '__\ \ / / |/ __/ _ \  / _ \ '_ \| __| '__| | | |
					//    \__ \  __/ |   \ V /| | (_|  __/ |  __/ | | | |_| |  | |_| |
					//    |___/\___|_|    \_/ |_|\___\___|  \___|_| |_|\__|_|   \__, |
					// 												     		|___/
					serviceEntry, err := generateCloudMainServiceEntry(profile)
					currentServiceEntry, err := serviceEntryLister.ServiceEntries(profile.Name).Get(serviceEntry.Name)
					if errors.IsNotFound(err) {
						klog.Infof("serviceentry %s/%s doesn't exist.", serviceEntry.Namespace, serviceEntry.Name)
					} else {
						klog.Infof("removing serviceentry %s/%s because %s contains a non-employee user.", currentServiceEntry.Namespace, currentServiceEntry.Name, profile.Name)
						err = istioClient.NetworkingV1beta1().ServiceEntries(serviceEntry.Namespace).Delete(
							context.Background(), currentServiceEntry.Name, metav1.DeleteOptions{},
						)
					}
					// 	      _           _   _             _   _                          _
					// 	   __| | ___  ___| |_(_)_ __   __ _| |_(_) ___  _ __    _ __ _   _| | ___
					//    / _` |/ _ \/ __| __| | '_ \ / _` | __| |/ _ \| '_ \  | '__| | | | |/ _ \
					//   | (_| |  __/\__ \ |_| | | | | (_| | |_| | (_) | | | | | |  | |_| | |  __/
					//    \__,_|\___||___/\__|_|_| |_|\__,_|\__|_|\___/|_| |_| |_|   \__,_|_|\___|
					destinationRule, err := generateCloudMainDestinationRule(profile)
					currentDestinationRule, err := destinationRuleLister.DestinationRules(profile.Name).Get(destinationRule.Name)
					if errors.IsNotFound(err) {
						klog.Infof("destinationrule %s/%s doesn't exist.", destinationRule.Namespace, destinationRule.Name)
					} else {
						klog.Infof("removing destinationrule %s/%s because %s contains a non-employee user.", currentDestinationRule.Namespace, currentDestinationRule.Name, profile.Name)
						err = istioClient.NetworkingV1beta1().DestinationRules(destinationRule.Namespace).Delete(
							context.Background(), currentDestinationRule.Name, metav1.DeleteOptions{},
						)
					}
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

func generateCloudMainDestinationRule(profile *kubeflowv1.Profile) (*istionetworkingclient.DestinationRule, error) {
	// Get namespace from profile
	namespace := profile.Name
	// Create destination rule
	destinationRule := istionetworkingclient.DestinationRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "direct-through-cloud-main-egress-gateway",
			Namespace: namespace,
			// Indicate that the profile owns the ServiceEntry
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: istionetworkingv1beta1.DestinationRule{
			Host: fmt.Sprintf("%s.%s.svc.cluster.local", ISTIO_EGRESS_GATEWAY_SVC, CLOUD_MAIN_SYSTEM_NAMESPACE),
			Subsets: []*istionetworkingv1beta1.Subset{
				{
					Name: ISTIO_SERVICE_ENTRY_NAME,
				},
			},
			ExportTo: []string{
				namespace,
				CLOUD_MAIN_SYSTEM_NAMESPACE,
			},
		},
	}
	return &destinationRule, nil
}

func generateCloudMainServiceEntry(profile *kubeflowv1.Profile) (*istionetworkingclient.ServiceEntry, error) {
	// Get namespace from profile
	namespace := profile.Name
	// Create service entry
	serviceEntry := istionetworkingclient.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ISTIO_SERVICE_ENTRY_NAME,
			Namespace: namespace,
			// Indicate that the profile owns the ServiceEntry
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: istionetworkingv1beta1.ServiceEntry{
			Hosts: []string{
				CLOUD_MAIN_GITLAB_HOST,
			},
			Ports: []*istionetworkingv1beta1.Port{
				{
					Number:   HTTPS_PORT,
					Name:     "tls",
					Protocol: "TLS",
				},
			},
			Resolution: istionetworkingv1beta1.ServiceEntry_DNS,
			ExportTo: []string{
				namespace,
				CLOUD_MAIN_SYSTEM_NAMESPACE,
			},
		},
	}
	return &serviceEntry, nil
}

func generateCloudMainVirtualService(profile *kubeflowv1.Profile) (*istionetworkingclient.VirtualService, error) {
	// Get namespace from profile
	namespace := profile.Name
	// Create virtual service
	virtualService := istionetworkingclient.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "direct-through-cloud-main-egress-gateway",
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
			Gateways: []string{
				"mesh",
				fmt.Sprintf("%s/cloud-main-egress-gateway", CLOUD_MAIN_SYSTEM_NAMESPACE),
			},
			Tls: []*istionetworkingv1beta1.TLSRoute{
				{
					Match: []*istionetworkingv1beta1.TLSMatchAttributes{
						{
							Gateways: []string{
								"mesh",
							},
							Port: HTTPS_PORT,
							SniHosts: []string{
								CLOUD_MAIN_GITLAB_HOST,
							},
						},
					},
					Route: []*istionetworkingv1beta1.RouteDestination{
						{
							Destination: &istionetworkingv1beta1.Destination{
								Host:   fmt.Sprintf("%s.%s.svc.cluster.local", ISTIO_EGRESS_GATEWAY_SVC, CLOUD_MAIN_SYSTEM_NAMESPACE),
								Subset: ISTIO_SERVICE_ENTRY_NAME,
								Port: &istionetworkingv1beta1.PortSelector{
									Number: HTTPS_PORT,
								},
							},
						},
					},
				},
				{
					Match: []*istionetworkingv1beta1.TLSMatchAttributes{
						{
							Gateways: []string{
								fmt.Sprintf("%s/cloud-main-egress-gateway", CLOUD_MAIN_SYSTEM_NAMESPACE),
							},
							Port: HTTPS_PORT,
							SniHosts: []string{
								CLOUD_MAIN_GITLAB_HOST,
							},
						},
					},
					Route: []*istionetworkingv1beta1.RouteDestination{{
						Destination: &istionetworkingv1beta1.Destination{
							Host: CLOUD_MAIN_GITLAB_HOST,
							Port: &istionetworkingv1beta1.PortSelector{
								Number: HTTPS_PORT,
							},
						},
						Weight: 100,
					}},
				},
			},
			ExportTo: []string{
				namespace,
				CLOUD_MAIN_SYSTEM_NAMESPACE,
			},
		},
	}
	return &virtualService, nil
}

func init() {
	rootCmd.AddCommand(cloudMainCmd)
}
