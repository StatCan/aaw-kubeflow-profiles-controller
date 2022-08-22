package cmd

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/StatCan/profiles-controller/internal/util"
	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	argocdv1alph1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	argocdclientset "github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	argocdinformers "github.com/argoproj/argo-cd/v2/pkg/client/informers/externalversions"
	"github.com/spf13/cobra"
	istionetworkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingclient "istio.io/client-go/pkg/apis/networking/v1beta1"
	istioclientset "istio.io/client-go/pkg/clientset/versioned"
	istioinformers "istio.io/client-go/pkg/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

/*
This controller creates the following s3proxy components for each user:
- argocd app
- nginx configmap
- s3proxy virtualservice
*/

// Configuration struct for s3proxy controller
type S3ProxyConfig struct {
	argocdNamespace            string // namespace the argocd instance is in
	argocdSourceRepoUrl        string // the repository url containing the s3proxy deployment manifest
	argocdSourceTargetRevision string // the git branch to deploy from
	argocdSourcePath           string // the path from the root of the git source repo
	argocdProject              string // argocd instances's project to deploy applications within
	sourceControlEnabledLabel  string // the label that indicates a user has opted in to using s3proxy.
	// one such example is sourcecontrol.statcan.gc.ca/enabled
	kubeflowUrl string // the url for kubeflow's central dashboard
	// (for dev: https://kubeflow.aaw-dev.cloud.statcan.ca, for production:
	// https://kubeflow.aaw.cloud.statcan.ca
	kubeflowPrefix string // the prefix where s3-explorer can be accessed from the kubeflow dashboard.
}

// Constructs the controller's config based on classification.
func NewS3ProxyConfig() (*S3ProxyConfig, error) {
	cfg := new(S3ProxyConfig)
	// ArgoCD Application Params
	cfg.argocdNamespace = util.ParseEnvVar("S3PROXY_ARGOCD_NAMESPACE")
	cfg.argocdSourceRepoUrl = util.ParseEnvVar("S3PROXY_ARGOCD_SOURCE_REPO_URL")
	cfg.argocdSourceTargetRevision = util.ParseEnvVar("S3PROXY_ARGOCD_SOURCE_TARGET_REVISION")
	cfg.argocdSourcePath = util.ParseEnvVar("S3PROXY_ARGOCD_SOURCE_PATH")
	cfg.argocdProject = util.ParseEnvVar("S3PROXY_ARGOCD_PROJECT")
	// S3Proxy Configuration Options
	cfg.kubeflowUrl = util.ParseEnvVar("S3PROXY_KUBEFLOW_ROOT_URL")
	cfg.kubeflowPrefix = util.ParseEnvVar("S3PROXY_KUBEFLOW_PREFIX")
	return cfg, nil
}

func init() {
	rootCmd.AddCommand(s3proxyCmd)
}

var s3proxyCmd = &cobra.Command{
	Use:   "s3proxy",
	Short: "Configure s3proxy",
	Long:  "Configure all resources necessary for user to access s3-explorer/s3proxy.",
	Run: func(cmd *cobra.Command, args []string) {
		s3proxyconfig, err := NewS3ProxyConfig()
		if err != nil {
			klog.Fatalf("Error building s3proxyconfig: %v", err)
		}
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()
		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("Error building kubeconfig: %v", err)
		}

		// 		   _ _            _              _
		// 		__| (_) ___ _ __ | |_   ___  ___| |_ _   _ _ __
		//    / __| | |/ _ \ '_ \| __| / __|/ _ \ __| | | | '_ \
		//   | (__| | |  __/ | | | |_  \__ \  __/ |_| |_| | |_) |
		//    \___|_|_|\___|_| |_|\__| |___/\___|\__|\__,_| .__/
		// 											      |_|
		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building Kubeflow client: %v", err)
		}

		istioClient, err := istioclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Istio client: %v", err)
		}

		argocdClient, err := argocdclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building argcd client: %v", err)
		}

		//  _        __                                           _
		// (_)_ __  / _| ___  _ __ _ __ ___   ___ _ __   ___  ___| |_ _   _ _ __
		// | | '_ \| |_ / _ \| '__| '_ ` _ \ / _ \ '__| / __|/ _ \ __| | | | '_ \
		// | | | | |  _| (_) | |  | | | | | |  __/ |    \__ \  __/ |_| |_| | |_) |
		// |_|_| |_|_|  \___/|_|  |_| |_| |_|\___|_|    |___/\___|\__|\__,_| .__/
		// 											     				   |_|
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))
		argocdInformerFactory := argocdinformers.NewSharedInformerFactory(argocdClient, time.Minute*(time.Duration(requeue_time)))
		istioInformerFactory := istioinformers.NewSharedInformerFactory(istioClient, time.Minute*(time.Duration(requeue_time)))

		argoAppInformer := argocdInformerFactory.Argoproj().V1alpha1().Applications()
		argoAppLister := argoAppInformer.Lister()

		configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
		configMapLister := configMapInformer.Lister()

		virtualServiceInformer := istioInformerFactory.Networking().V1beta1().VirtualServices()
		virtualServiceLister := virtualServiceInformer.Lister()

		// Setup Controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Generate argocd application
				s3ArgoCdApp, err := generateS3ProxyArgoApp(profile, s3proxyconfig)

				if err != nil {
					return err
				}
				//   	_                     ____ ____       _
				//     / \   _ __ __ _  ___  / ___|  _ \     / \   _ __  _ __
				//    / _ \ | '__/ _` |/ _ \| |   | | | |   / _ \ | '_ \| '_ \
				//   / ___ \| | | (_| | (_) | |___| |_| |  / ___ \| |_) | |_) |
				//  /_/   \_\_|  \__, |\___/ \____|____/  /_/   \_\ .__/| .__/
				// 			     |___/                            |_|   |_|
				currents3ArgoCdApp, err := argoAppLister.Applications(s3ArgoCdApp.Namespace).Get(s3ArgoCdApp.Name)
				if errors.IsNotFound(err) {
					klog.Infof("creating argo app %s/%s", s3ArgoCdApp.Namespace, s3ArgoCdApp.Name)
					_, err = argocdClient.ArgoprojV1alpha1().Applications(s3ArgoCdApp.Namespace).Create(
						context.Background(), s3ArgoCdApp, metav1.CreateOptions{})
					if err != nil {
						return err
					}
				} else if !reflect.DeepEqual(s3ArgoCdApp.Spec, currents3ArgoCdApp.Spec) {
					klog.Infof("updating argo app %s/%s", s3ArgoCdApp.Namespace, s3ArgoCdApp.Name)

					currents3ArgoCdApp.Spec = s3ArgoCdApp.Spec

					_, err = argocdClient.ArgoprojV1alpha1().Applications(s3ArgoCdApp.Namespace).Update(context.Background(),
						currents3ArgoCdApp, metav1.UpdateOptions{})
					if err != nil {
						return err
					}
				}

				//  _   _  ____ ___ _   ___  __
				// | \ | |/ ___|_ _| \ | \ \/ /   ___ _ __ ___
				// |  \| | |  _ | ||  \| |\  /   / __| '_ ` _ \
				// | |\  | |_| || || |\  |/  \  | (__| | | | | |
				// |_| \_|\____|___|_| \_/_/\_\  \___|_| |_| |_|
				nginxConfigMap, err := generateNginxConfigMap(profile, s3proxyconfig)
				if err != nil {
					return err
				}
				currentGiteaConfigMap, err := configMapLister.ConfigMaps(nginxConfigMap.Namespace).Get(nginxConfigMap.Name)
				if errors.IsNotFound(err) {
					klog.Infof("creating configMap %s/%s", nginxConfigMap.Namespace, nginxConfigMap.Name)
					_, err = kubeClient.CoreV1().ConfigMaps(nginxConfigMap.Namespace).Create(
						context.Background(), nginxConfigMap, metav1.CreateOptions{})
					if err != nil {
						return err
					}
				} else if !reflect.DeepEqual(nginxConfigMap.Data, currentGiteaConfigMap.Data) {
					klog.Infof("updating configMap %s/%s", nginxConfigMap.Namespace, nginxConfigMap.Name)
					currentGiteaConfigMap.Data = nginxConfigMap.Data

					_, err = kubeClient.CoreV1().ConfigMaps(nginxConfigMap.Namespace).Update(context.Background(),
						currentGiteaConfigMap, metav1.UpdateOptions{})
					if err != nil {
						return err
					}
				}

				//  ___     _   _        __     ______
				// |_ _|___| |_(_) ___   \ \   / / ___|
				//  | |/ __| __| |/ _ \   \ \ / /\___ \
				//  | |\__ \ |_| | (_) |   \ V /  ___) |
				// |___|___/\__|_|\___/     \_/  |____/
				// Create the Istio virtual service
				virtualService, err := generateS3ProxyVirtualService(profile, s3proxyconfig)
				if err != nil {
					return err
				}
				currentVirtualService, err := virtualServiceLister.VirtualServices(profile.Name).Get(virtualService.Name)
				// If the virtual service is not found and the user has opted into having source control, create the virtual service.
				if errors.IsNotFound(err) {
					// Always create the virtual service
					klog.Infof("Creating Istio virtualservice %s/%s", virtualService.Namespace, virtualService.Name)
					_, err = istioClient.NetworkingV1beta1().VirtualServices(virtualService.Namespace).Create(
						context.Background(), virtualService, metav1.CreateOptions{},
					)
					if err != nil {
						return err
					}
				} else if !reflect.DeepEqual(virtualService.Spec, currentVirtualService.Spec) {
					klog.Infof("Updating Istio virtualservice %s/%s", virtualService.Namespace, virtualService.Name)
					currentVirtualService = virtualService
					_, err = istioClient.NetworkingV1beta1().VirtualServices(virtualService.Namespace).Update(
						context.Background(), currentVirtualService, metav1.UpdateOptions{},
					)
					if err != nil {
						return err
					}
				}
				return nil
			},
		)

		//  _        __
		// (_)_ __  / _| ___  _ __ _ __ ___   ___ _ __ ___
		// | | '_ \| |_ / _ \| '__| '_ ` _ \ / _ \ '__/ __|
		// | | | | |  _| (_) | |  | | | | | |  __/ |  \__ \
		// |_|_| |_|_|  \___/|_|  |_| |_| |_|\___|_|  |___/

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
		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)
		argocdInformerFactory.Start(stopCh)
		istioInformerFactory.Start(stopCh)

		// Wait for caches to sync
		klog.Info("Waiting for configMap informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, configMapInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}
		klog.Info("Waiting for argo informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, argoAppInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}
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

func generateS3ProxyArgoApp(profile *kubeflowv1.Profile, s3proxyconfig *S3ProxyConfig) (*argocdv1alph1.Application, error) {
	app := &argocdv1alph1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("s3proxy-%s", profile.Name),
			Namespace: s3proxyconfig.argocdNamespace,
		},
		Spec: argocdv1alph1.ApplicationSpec{
			Project: s3proxyconfig.argocdProject,
			Destination: argocdv1alph1.ApplicationDestination{
				Namespace: profile.Name,
				Name:      "in-cluster",
			},
			Source: argocdv1alph1.ApplicationSource{
				RepoURL:        s3proxyconfig.argocdSourceRepoUrl,
				TargetRevision: s3proxyconfig.argocdSourceTargetRevision,
				Path:           s3proxyconfig.argocdSourcePath,
				Directory: &argocdv1alph1.ApplicationSourceDirectory{
					Recurse: true,
					Jsonnet: argocdv1alph1.ApplicationSourceJsonnet{
						ExtVars: []argocdv1alph1.JsonnetVar{
							{
								Name:  "userNamespace",
								Value: profile.Name,
							},
						},
					},
				},
			},
			SyncPolicy: &argocdv1alph1.SyncPolicy{
				Automated: &argocdv1alph1.SyncPolicyAutomated{},
			},
		},
	}

	return app, nil
}

func generateNginxConfigMap(profile *kubeflowv1.Profile, s3proxyconfig *S3ProxyConfig) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3proxy-nginx-conf",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Data: map[string]string{
			"nginx.conf": fmt.Sprintf(`
			worker_processes  3;
			pid /tmp/nginx.pid; # Changed from /var/run/nginx.pid
			error_log  /var/log/nginx/error.log;
			events {
			  worker_connections  1024;
			}
			http {
			  client_max_body_size 1G;
			  server {
				  listen       8080;
				  server_name  _;

				  location ~ ^/%s/fonts/.*$ {
					  root html;
				  }

				  location ~ ^/%s/webfonts/.*$ {
					  root html;
				  }

				  location ~ ^/%s/%s/.*$ {
					  include  /etc/nginx/mime.types;
					  root   html;
					  index  index.html index.htm;
				  }

				  location / {
					 proxy_set_header X-Real-IP $remote_addr;
					 proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
					 proxy_set_header X-Forwarded-Proto $scheme;
					 proxy_set_header Host $http_host;

					 proxy_connect_timeout 300;
					 # Default is HTTP/1, keepalive is only enabled in HTTP/1.1
					 proxy_http_version 1.1;
					 proxy_set_header Connection "";
					 chunked_transfer_encoding off;
					 proxy_pass http://0.0.0.0:9000;
				  }
			  }
			}
			`, s3proxyconfig.kubeflowPrefix,
				s3proxyconfig.kubeflowPrefix,
				s3proxyconfig.kubeflowPrefix,
				profile.Name,
			),
		},
	}
	return cm, nil
}

func generateS3ProxyVirtualService(profile *kubeflowv1.Profile, s3proxyconfig *S3ProxyConfig) (*istionetworkingclient.VirtualService, error) {
	// Get namespace from profile
	namespace := profile.Name
	// Create virtual service
	virtualService := istionetworkingclient.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3-virtualservice",
			Namespace: namespace,
			// Indicate that the profile owns the virtualservice resource
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: istionetworkingv1beta1.VirtualService{
			ExportTo: []string{namespace, "istio-system", "ingress-general-system"},
			// Routing rules are applied to all traffic that goes through the kubeflow gateway
			Gateways: []string{
				"kubeflow/kubeflow-gateway",
			},
			Hosts: []string{
				"kubeflow.aaw-dev.cloud.statcan.ca",
			},
			Http: []*istionetworkingv1beta1.HTTPRoute{
				// 		   _        _   _         __ _ _
				//     ___| |_ __ _| |_(_) ___   / _(_) | ___  ___
				//    / __| __/ _` | __| |/ __| | |_| | |/ _ \/ __|
				//    \__ \ || (_| | |_| | (__  |  _| | |  __/\__ \
				//    |___/\__\__,_|\__|_|\___| |_| |_|_|\___||___/
				{
					Name: "s3-route-s3proxy-requests",
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: fmt.Sprintf("/%s/%s/", s3proxyconfig.kubeflowPrefix, namespace),
								},
							},
						},
					},
					Route: []*istionetworkingv1beta1.HTTPRouteDestination{
						{
							Destination: &istionetworkingv1beta1.Destination{
								Host: fmt.Sprintf("s3proxy-web.%s.svc.cluster.local", namespace),
								Port: &istionetworkingv1beta1.PortSelector{
									Number: uint32(80),
								},
							},
						},
					},
				},
				{
					Name: "s3-uri-redirect",
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("%s/_/%s/?ns=%s",
											s3proxyconfig.kubeflowUrl,
											s3proxyconfig.kubeflowPrefix,
											namespace),
									},
								},
							},
						},
					},
					Redirect: &istionetworkingv1beta1.HTTPRedirect{
						Uri: fmt.Sprintf("/%s/%s/index.html", s3proxyconfig.kubeflowPrefix, namespace),
					},
				},
				//  ____ _____ ____                                                       _
				// / ___|___ /|  _ \ _ __ _____  ___   _   _ __ ___  __ _ _   _  ___  ___| |_ ___
				// \___ \ |_ \| |_) | '__/ _ \ \/ / | | | | '__/ _ \/ _` | | | |/ _ \/ __| __/ __|
				//  ___) |__) |  __/| | | (_) >  <| |_| | | | |  __/ (_| | |_| |  __/\__ \ |_\__ \
				// |____/____/|_|   |_|  \___/_/\_\\__, | |_|  \___|\__, |\__,_|\___||___/\__|___/
				//                                 |___/               |_|
				{
					Name: "s3-unclassified-bucket",
					// TODO: this should be refactored once we upgrade to Kubeflow > 1.4,
					// we will no longer need to check the http referer header once namespaced
					// menu items are supported.
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("%s/%s/%s/index.html",
											s3proxyconfig.kubeflowUrl,
											s3proxyconfig.kubeflowPrefix,
											namespace),
									},
								},
							},
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: "/unclassified",
								},
							},
						},
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("%s/%s/%s/sw_modify_header.js",
											s3proxyconfig.kubeflowUrl,
											s3proxyconfig.kubeflowPrefix,
											namespace),
									},
								},
							},
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: "/unclassified",
								},
							},
						},
					},
					Route: []*istionetworkingv1beta1.HTTPRouteDestination{
						{
							Destination: &istionetworkingv1beta1.Destination{
								Host: fmt.Sprintf("s3proxy-web.%s.svc.cluster.local", namespace),
								Port: &istionetworkingv1beta1.PortSelector{
									Number: uint32(80),
								},
							},
						},
					},
				},
				{
					Name: "s3-unclassified-ro-bucket",
					// TODO: this should be refactored once we upgrade to Kubeflow > 1.4,
					// we will no longer need to check the http referer header once namespaced
					// menu items are supported.
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("%s/%s/%s/index.html",
											s3proxyconfig.kubeflowUrl,
											s3proxyconfig.kubeflowPrefix,
											namespace),
									},
								},
							},
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: "/unclassified-ro",
								},
							},
						},
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("%s/%s/%s/sw_modify_header.js",
											s3proxyconfig.kubeflowUrl,
											s3proxyconfig.kubeflowPrefix,
											namespace),
									},
								},
							},
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: "/unclassified-ro",
								},
							},
						},
					},
					Route: []*istionetworkingv1beta1.HTTPRouteDestination{
						{
							Destination: &istionetworkingv1beta1.Destination{
								Host: fmt.Sprintf("s3proxy-web.%s.svc.cluster.local", namespace),
								Port: &istionetworkingv1beta1.PortSelector{
									Number: uint32(80),
								},
							},
						},
					},
				},
				{
					Name: "s3-protected-b-bucket",
					// TODO: this should be refactored once we upgrade to Kubeflow > 1.4,
					// we will no longer need to check the http referer header once namespaced
					// menu items are supported.
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("%s/%s/%s/index.html",
											s3proxyconfig.kubeflowUrl,
											s3proxyconfig.kubeflowPrefix,
											namespace),
									},
								},
							},
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: "/protected-b",
								},
							},
						},
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("%s/%s/%s/sw_modify_header.js",
											s3proxyconfig.kubeflowUrl,
											s3proxyconfig.kubeflowPrefix,
											namespace),
									},
								},
							},
							Uri: &istionetworkingv1beta1.StringMatch{
								MatchType: &istionetworkingv1beta1.StringMatch_Prefix{
									Prefix: "/protected-b",
								},
							},
						},
					},
					Route: []*istionetworkingv1beta1.HTTPRouteDestination{
						{
							Destination: &istionetworkingv1beta1.Destination{
								Host: fmt.Sprintf("s3proxy-web.%s.svc.cluster.local", namespace),
								Port: &istionetworkingv1beta1.PortSelector{
									Number: uint32(80),
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
