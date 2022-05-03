package cmd

import (
	"context"
	"fmt"
	"reflect"
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
	corev1 "k8s.io/api/core/v1"

	argocdv1alph1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	argocdclientset "github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	argocdinformers "github.com/argoproj/argo-cd/v2/pkg/client/informers/externalversions"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

// The internal Gitea URL is specified in https://github.com/StatCan/aaw-argocd-manifests/blob/aaw-dev-cc-00/profiles-argocd-system/template/gitea/manifest.yaml#L350
const GITEA_SERVICE_URL = "gitea-http"

// The url prefix that is used to redirect to Gitea
const GITEA_URL_PREFIX = "gitea"

// The port exposed by the Gitea service is specified in https://github.com/StatCan/aaw-argocd-manifests/blob/aaw-dev-cc-00/profiles-argocd-system/template/gitea/manifest.yaml#L365
const GITEA_SERVICE_PORT = 80

var sourceControlEnabledLabel = "sourcecontrol.statcan.gc.ca/enabled"
var giteaBannerConfigMapName = "gitea-banner"
var argocdnamespace string

func init() {
	rootCmd.AddCommand(giteaCmd)
	giteaCmd.PersistentFlags().StringVar(&argocdnamespace, "argocdnamespace", "argocd",
		"The namespace of argocd")
}

var giteaCmd = &cobra.Command{
	Use:   "gitea",
	Short: "Configure gitea",
	Long: `Configure gitea for Kubeflow resources.* Statefulset	`,
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

		argocdClient, err := argocdclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building argcd client: %v", err)
		}

		istioClient, err := istioclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Istio client: %v", err)
		}

		// Setup Kubeflow informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))
		argocdInformerFactory := argocdinformers.NewSharedInformerFactory(argocdClient, time.Minute*(time.Duration(requeue_time)))
		istioInformerFactory := istioinformers.NewSharedInformerFactory(istioClient, time.Minute*(time.Duration(requeue_time)))

		// Setup argocd informers
		argoAppInformer := argocdInformerFactory.Argoproj().V1alpha1().Applications()
		argoAppLister := argoAppInformer.Lister()

		// Setup configMap informers
		configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
		configMapLister := configMapInformer.Lister()

		// Setup virtual service informers
		virtualServiceInformer := istioInformerFactory.Networking().V1beta1().VirtualServices()
		virtualServiceLister := virtualServiceInformer.Lister()
		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Only create nginx if a profile has opted in, as determined by sourcecontrol.statcan.gc.ca/enabled=true
				var replicas int32

				if val, ok := profile.Labels[sourceControlEnabledLabel]; ok && val == "true" {
					replicas = 1
				} else {
					replicas = 0
				}
				// Generate argocd application
				giteaArgoCdApp, err := generateGiteaArgoApp(profile, replicas)
				if err != nil {
					return err
				}
				currentGiteaArgoCdApp, err := argoAppLister.Applications(giteaArgoCdApp.Namespace).Get(giteaArgoCdApp.Name)
				if errors.IsNotFound(err) {
					if replicas != 0 {
						klog.Infof("creating argo app %s/%s", giteaArgoCdApp.Namespace, giteaArgoCdApp.Name)
						currentGiteaArgoCdApp, err = argocdClient.ArgoprojV1alpha1().Applications(giteaArgoCdApp.Namespace).Create(
							context.Background(), giteaArgoCdApp, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}
				} else if !reflect.DeepEqual(giteaArgoCdApp.Spec, currentGiteaArgoCdApp.Spec) {
					klog.Infof("updating argo app %s/%s", giteaArgoCdApp.Namespace, giteaArgoCdApp.Name)
					currentGiteaArgoCdApp.Spec = giteaArgoCdApp.Spec

					_, err = argocdClient.ArgoprojV1alpha1().Applications(giteaArgoCdApp.Namespace).Update(context.Background(),
						currentGiteaArgoCdApp, metav1.UpdateOptions{})
					if err != nil {
						return err
					}
				}

				// Generate configMap
				giteaConfigMap, err := generateBannerConfigMap(profile)
				if err != nil {
					return err
				}
				currentGiteaConfigMap, err := configMapLister.ConfigMaps(giteaConfigMap.Namespace).Get(giteaConfigMap.Name)
				if errors.IsNotFound(err) {
					if replicas != 0 {
						klog.Infof("creating configMap %s/%s", giteaConfigMap.Namespace, giteaConfigMap.Name)
						currentGiteaConfigMap, err = kubeClient.CoreV1().ConfigMaps(giteaConfigMap.Namespace).Create(
							context.Background(), giteaConfigMap, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}
				} else if !reflect.DeepEqual(giteaConfigMap.Data, currentGiteaConfigMap.Data) {
					klog.Infof("updating configMap %s/%s", giteaConfigMap.Namespace, giteaConfigMap.Name)
					currentGiteaConfigMap.Data = giteaConfigMap.Data

					_, err = kubeClient.CoreV1().ConfigMaps(giteaConfigMap.Namespace).Update(context.Background(),
						currentGiteaConfigMap, metav1.UpdateOptions{})
					if err != nil {
						return err
					}
				}
				// Create the Istio virtual service
				virtualService, err := generateIstioVirtualService(profile)
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
				return nil
			},
		)

		argoAppInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newDep := new.(*argocdv1alph1.Application)
				oldDep := old.(*argocdv1alph1.Application)

				if newDep.ResourceVersion == oldDep.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		configMapInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newDep := new.(*corev1.ConfigMap)
				oldDep := old.(*corev1.ConfigMap)

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
		argocdInformerFactory.Start(stopCh)

		// Wait for caches
		klog.Info("Waiting for informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, argoAppInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

// generates the struct for the argocd application that deploys gitea via the customized manifest
// within https://github.com/StatCan/aaw-argocd-manifests/profiles-argocd-system/template/gitea
func generateGiteaArgoApp(profile *kubeflowv1.Profile, replicas int32) (*argocdv1alph1.Application, error) {
	app := &argocdv1alph1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gitea-" + profile.Name,
			Namespace: argocdnamespace,
		},
		Spec: argocdv1alph1.ApplicationSpec{
			Project: "default",
			Destination: argocdv1alph1.ApplicationDestination{
				Namespace: profile.Name,
				Name:      "in-cluster",
			},
			Source: argocdv1alph1.ApplicationSource{
				RepoURL:        "https://github.com/StatCan/aaw-argocd-manifests.git",
				TargetRevision: "aaw-dev-cc-00",
				Path:           "profiles-argocd-system/template/gitea",
			},
			SyncPolicy: &argocdv1alph1.SyncPolicy{},
		},
	}

	return app, nil
}

func generateBannerConfigMap(profile *kubeflowv1.Profile) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      giteaBannerConfigMapName,
			Namespace: profile.Name,
		},
		Data: map[string]string{
			"body_inner_pre.tmpl": "Welcome to AAW Gitea. " +
				"To login use username: superuser, password: password | " +
				"Bienvenue à AAW Gitea. Pour vous connecter, utilisez le " +
				"nom d'utilisateur: superuser, le mot de passe: password",
		},
	}
	return cm, nil
}

func generateIstioVirtualService(profile *kubeflowv1.Profile) (*istionetworkingclient.VirtualService, error) {
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
				"*",
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
				{
					Name: "gitea-redirect",
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: fmt.Sprintf("/%s/?ns=%s", GITEA_SERVICE_URL, namespace),
								},
							},
						},
					},
					Redirect: &istionetworkingv1beta1.HTTPRedirect{
						Uri: fmt.Sprintf("/%s/%s/", GITEA_URL_PREFIX, namespace),
					},
				},
			},
		},
	}
	return &virtualService, nil
}

func init() {
	rootCmd.AddCommand(giteaCmd)
}
