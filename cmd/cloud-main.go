package cmd

import (
	"fmt"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/spf13/cobra"
	istionetworkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingclient "istio.io/client-go/pkg/apis/networking/v1beta1"
	istioclientset "istio.io/client-go/pkg/clientset/versioned"
	istioinformers "istio.io/client-go/pkg/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

const CLOUD_MAIN_GITLAB_HOST = "gitlab.k8s.cloud.statcan.ca"

var cloudMainCmd = &cobra.Command{
	Use:   "cloud-main",
	Short: "Configure cloud-main resources",
	Long: `Configure resources that enable connectivity to certain cloud-main services for StatCan employees.
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Create Kubernetes configuration
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)

		// Get Kubernetes Client
		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
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
		istioInformerFactory := istioinformers.NewSharedInformerFactory(istioClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		// Setup virtual service informers
		virtualServiceInformer := istioInformerFactory.Networking().V1beta1().VirtualServices()
		virtualServiceLister := virtualServiceInformer.Lister()

		// Initialize cloud-main controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Generate the per-namespace virtual service
				virtualService, err := generateCloudMainVirtualService(profile)
			},
		)
	},
}

func generateCloudMainVirtualService(profile *kubeflowv1.Profile) (*istionetworkingclient.VirtualService, error) {
	// Get namespace from profile
	namespace := profile.Name
	// Create virtual service
	virtualService := istionetworkingclient.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gitea-virtualservice",
			Namespace: namespace,
			// Indicate that the profile owns the virtualservice resource
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: istionetworkingv1beta1.VirtualService{
			// Routing rules are applied to all traffic that goes through the kubeflow gateway
			Gateways: []string{
				"kubeflow/kubeflow-gateway",
			},
			Hosts: []string{
				CLOUD_MAIN_GITLAB_HOST,
			},
			Http: []*istionetworkingv1beta1.HTTPRoute{
				{
					Name: "gitea-route",
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: fmt.Sprintf("/%s/%s/", GITEA_URL_PREFIX, namespace),
								},
							},
						},
					},
					Rewrite: &istionetworkingv1beta1.HTTPRewrite{
						Uri: "/",
					},
					Route: []*istionetworkingv1beta1.HTTPRouteDestination{
						{
							Destination: &istionetworkingv1beta1.Destination{
								Host: fmt.Sprintf("%s.%s.svc.cluster.local", GITEA_SERVICE_URL, namespace),
								Port: &istionetworkingv1beta1.PortSelector{
									Number: GITEA_SERVICE_PORT,
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
