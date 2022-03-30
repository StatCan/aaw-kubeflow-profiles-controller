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

		// Setup Kubeflow informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))
		argocdInformerFactory := argocdinformers.NewSharedInformerFactory(argocdClient, time.Minute*(time.Duration(requeue_time)))

		// Setup argocd informers
		argoAppInformer := argocdInformerFactory.Argoproj().V1alpha1().Applications()
		argoAppLister := argoAppInformer.Lister()

		// Setup configMap informers
		configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
		configMapLister := configMapInformer.Lister()

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

				if err == nil {
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
				}

				// Generate configMap
				giteaConfigMap, err := generateBannerConfigMap(profile)
				if err == nil {
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

// generates the struct for argocd application that deploys gitea via helm.
// Currently, as a workaround we are getting the helm chart from a forked repo.
// To use the actual repo once it is fixed, replace the below entries.
// RepoURL: "https://dl.gitea.io/charts/",
// Chart: "gitea",
// TargetRevision: "5.0.4",
// Helm: &argocdv1alph1.ApplicationSourceHelm{
// 	ReleaseName: "gitea",
// 	Values: fmt.Sprintf(giteaHelmValuesYaml,replicas,
// 						giteaBannerConfigMapName),
// },
func generateGiteaArgoApp(profile *kubeflowv1.Profile, replicas int32) (*argocdv1alph1.Application, error){
	app := &argocdv1alph1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gitea-"+ profile.Name,
			Namespace: argocdnamespace,
		},
		Spec: argocdv1alph1.ApplicationSpec{
			Project: "default",
			Destination: argocdv1alph1.ApplicationDestination{
				Namespace: profile.Name,
				Name: "in-cluster",
			},
			Source: argocdv1alph1.ApplicationSource{
				RepoURL: "https://gitea.com/cboin1996/helm-chart.git",
				TargetRevision: "fix-extra-volume-mounts",
				Path: ".",
			},
			SyncPolicy: &argocdv1alph1.SyncPolicy{
				Automated: &argocdv1alph1.SyncPolicyAutomated{
					Prune: true,
					SelfHeal: true,
				},
			},
		},
	}
	
	return app, nil
}


func generateBannerConfigMap(profile *kubeflowv1.Profile) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind: "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: giteaBannerConfigMapName,
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

// Variables must be formatted with fmt.Sprintf() in order to use this
// string.
var giteaHelmValuesYaml string = `
replicaCount: %d

extraVolumes:
  - name: config-volume
    configMap:
      name: %s

extraVolumeMounts:
  - name: config-volume
    mountPath: /data/gitea/templates/custom

service:
  http:
    port: 80
  ssh:
    port: 22
	
gitea:
  admin:
    username: superuser
    password: password
    email: "gitea@local.domain"

  metrics:
    enabled: false
    serviceMonitor:
      enabled: false

  config:
    server:
      DOMAIN: gitea
      PROTOCOL: http
      ROOT_URL: http://gitea
      SSH_DOMAIN: gitea
      ENABLE_PPROF: false
	  HTTP_PORT: 3000

postgresql:
  enabled: true
  global:
    postgresql:
      postgresqlDatabase: gitea
      postgresqlUsername: gitea
      postgresqlPassword: gitea
      servicePort: 5432
  persistence:
    size: 1Gi

memcached:
  enabled: false
  service:
    port: 11211

checkDeprecation: true
`

func init() {
	rootCmd.AddCommand(giteaCmd)
}
