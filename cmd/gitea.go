package cmd

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/StatCan/profiles-controller/internal/util"
	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	pq "github.com/lib/pq"
	istionetworkingv1beta1 "istio.io/api/networking/v1beta1"
	istionetworkingclient "istio.io/client-go/pkg/apis/networking/v1beta1"

	istioclientset "istio.io/client-go/pkg/clientset/versioned"
	istioinformers "istio.io/client-go/pkg/informers/externalversions"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"

	argocdv1alph1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	argocdclientset "github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	argocdinformers "github.com/argoproj/argo-cd/v2/pkg/client/informers/externalversions"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	// project packages
)

// Parameters specific to the authentication with managed postgres
type Psqlparams struct {
	hostname string
	port     string
	username string
	passwd   string
	dbname   string
}
// Parameters specific to the deployment of per-namespace gitea applications
type Deploymentparams struct {
	classificationEn string // classification (unclassified or prot-b)
	classificationFr string // classification in french
	giteaServiceUrl string  // The internal Gitea URL is specified in
	                        // https://github.com/StatCan/aaw-argocd-manifests/blob/aaw-dev-cc-00/profiles-argocd-system/template/gitea/manifest.yaml#L350
	giteaUrlPrefix  string  // url prefix used to redirect gitea
	giteaServicePort int // The port exposed by the Gitea service is specified in
	                        // https://github.com/StatCan/aaw-argocd-manifests/blob/aaw-dev-cc-00/profiles-argocd-system/template/gitea/manifest.yaml#L365
	giteaBannerConfigMapName string // gitea banner configmap name (configmap which corresponds to the banner
	                                // at the top of the gitea ui)
	argocdNamespace  string // namespace the argocd instance is in
	sourceControlEnabledLabel string // the label that indicates a user has opted in to using gitea.
									 // one such example is sourcecontrol.statcan.gc.ca/enabled
}
// Configuration struct for gitea controller
type GiteaConfig struct {
	Psqlparams Psqlparams
	Deploymentparams Deploymentparams
}

// Constructs the controller's config based on classification.
func NewGiteaConfig() (*GiteaConfig, error) {
	classification := util.ParseEnvVar("GITEA_CLASSIFICATION")
	cfg := new(GiteaConfig)
	// configure classification specific parameters
	if classification == "unclassified" {
		cfg.Deploymentparams.classificationFr = "non classé"
	} else if classification == "protected-b" {
		cfg.Deploymentparams.classificationFr = "Protégé-b"
	} else {
		klog.Fatalf("no implementation of classification %s exists. terminating.", classification)
	}
	// currently the below configuration is agnostic of the classification
	cfg.Deploymentparams.classificationEn = classification
	// configure psql specific parameters
	cfg.Psqlparams.hostname = util.ParseEnvVar("GITEA_PSQL_HOSTNAME")
	cfg.Psqlparams.port     = util.ParseEnvVar("GITEA_PSQL_PORT")
	cfg.Psqlparams.username = util.ParseEnvVar("GITEA_PSQL_ADMIN_UNAME")
	cfg.Psqlparams.passwd   = util.ParseEnvVar("GITEA_PSQL_ADMIN_PASSWD")
	cfg.Psqlparams.dbname   = util.ParseEnvVar("GITEA_PSQL_MAINTENANCE_DB")
	// configure deployment specific parameters
	cfg.Deploymentparams.giteaServiceUrl    	   = util.ParseEnvVar("GITEA_SERVICE_URL")
	cfg.Deploymentparams.giteaUrlPrefix     	   = util.ParseEnvVar("GITEA_URL_PREFIX")
	cfg.Deploymentparams.giteaServicePort   	   = util.ParseIntegerEnvVar("GITEA_SERVICE_PORT")
	cfg.Deploymentparams.giteaBannerConfigMapName  = util.ParseEnvVar("GITEA_BANNER_CONFIGMAP_NAME")
	cfg.Deploymentparams.argocdNamespace           = util.ParseEnvVar("GITEA_ARGOCD_NAMESPACE")
	cfg.Deploymentparams.sourceControlEnabledLabel = util.ParseEnvVar("GITEA_SOURCE_CONTROL_ENABLED_LABEL")
	return cfg, nil
}

func init() {
	rootCmd.AddCommand(giteaCmd)
}

var giteaCmd = &cobra.Command{
	Use:   "gitea",
	Short: "Configure gitea",
	Long: `Configure gitea for Kubeflow resources.* Statefulset	`,
	Run: func(cmd *cobra.Command, args []string) {
		giteaconfig, err := NewGiteaConfig()
		if err != nil {
			klog.Fatalf("Error building giteaconfig: %v", err)
		}
		psqlparams := giteaconfig.Psqlparams
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

		// Establisth a connection to the db.
		db, err := connect(psqlparams.hostname,
			psqlparams.port, psqlparams.username, psqlparams.passwd, psqlparams.dbname)
		if err != nil {
			klog.Fatalf("Could not establish connection to PSQL!")
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

		secretInformer := kubeInformerFactory.Core().V1().Secrets()
		secretLister := secretInformer.Lister()

		// Setup virtual service informers
		virtualServiceInformer := istioInformerFactory.Networking().V1beta1().VirtualServices()
		virtualServiceLister := virtualServiceInformer.Lister()

		// Setup ServiceEntry informers
		serviceEntryInformer := istioInformerFactory.Networking().V1beta1().ServiceEntries()
		serviceEntryLister := serviceEntryInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Only create gitea instance if a profile has opted in, as determined by sourcecontrol.statcan.gc.ca/enabled=true
				var replicas int32

				if val, ok := profile.Labels[giteaconfig.Deploymentparams.sourceControlEnabledLabel]; ok && val == "true" {
					replicas = 1
				} else {
					replicas = 0
				}
				// Generate argocd application
				giteaArgoCdApp, err := generateGiteaArgoApp(profile, replicas, giteaconfig)

				if err != nil {
					return err
				}

				currentGiteaArgoCdApp, err := argoAppLister.Applications(giteaArgoCdApp.Namespace).Get(giteaArgoCdApp.Name)
				if errors.IsNotFound(err) {
					if replicas != 0 {
						klog.Infof("provisioning postgres for profile '%s'!", profile.Name)
						err := provisionDb(db, profile, &psqlparams, kubeClient, secretLister, replicas)
						if err != nil {
							return err
						}

						klog.Infof("creating argo app %s/%s", giteaArgoCdApp.Namespace, giteaArgoCdApp.Name)
						_, err = argocdClient.ArgoprojV1alpha1().Applications(giteaArgoCdApp.Namespace).Create(
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
				giteaConfigMap, err := generateBannerConfigMap(profile, giteaconfig)
				if err != nil {
					return err
				}
				currentGiteaConfigMap, err := configMapLister.ConfigMaps(giteaConfigMap.Namespace).Get(giteaConfigMap.Name)
				if errors.IsNotFound(err) {
					if replicas != 0 {
						klog.Infof("creating configMap %s/%s", giteaConfigMap.Namespace, giteaConfigMap.Name)
						_, err = kubeClient.CoreV1().ConfigMaps(giteaConfigMap.Namespace).Create(
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
				virtualService, err := generateIstioVirtualService(profile, giteaconfig)
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

				// Create the Istio Service Entry
				serviceEntry, err := generateServiceEntry(profile, psqlparams)
				if err != nil {
					return err
				}
				currentServiceEntry, err := serviceEntryLister.ServiceEntries(serviceEntry.Namespace).Get(serviceEntry.Name)
				// If the ServiceEntry is not found and the user has opted into having source control, create ServiceEntry
				if errors.IsNotFound(err) {
					if replicas != 0 {
						klog.Infof("Creating Istio ServiceEntry %s/%s", serviceEntry.Namespace, serviceEntry.Name)
						_, err = istioClient.NetworkingV1beta1().ServiceEntries(serviceEntry.Namespace).Create(
							context.Background(), serviceEntry, metav1.CreateOptions{},
						)
						if err != nil {
							return err
						}
					}
				} else if !reflect.DeepEqual(serviceEntry.Spec, currentServiceEntry.Spec) {
					klog.Infof("Updating Istio ServiceEntry %s/%s", serviceEntry.Namespace, serviceEntry.Name)
					currentServiceEntry.Spec = serviceEntry.Spec
					_, err = istioClient.NetworkingV1beta1().ServiceEntries(serviceEntry.Namespace).Update(
						context.Background(), currentServiceEntry, metav1.UpdateOptions{},
					)
					if err != nil {
						return err
					}
				}
				return nil
			},
		)
		// Declare Event Handlers for Informers
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

		secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newDep := new.(*corev1.Secret)
				oldDep := old.(*corev1.Secret)

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

		serviceEntryInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newDep := new.(*istionetworkingclient.ServiceEntry)
				oldDep := old.(*istionetworkingclient.ServiceEntry)

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
		klog.Info("Waiting for secret informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, secretInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}
		klog.Info("Waiting for argo informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, argoAppInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}
		klog.Info("Waiting for istio serviceEntry informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, serviceEntryInformer.Informer().HasSynced); !ok {
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

// generates the struct for the argocd application that deploys gitea via the customized manifest
// within https://github.com/StatCan/aaw-argocd-manifests/profiles-argocd-system/template/gitea
func generateGiteaArgoApp(profile *kubeflowv1.Profile, replicas int32, giteaconfig *GiteaConfig) (*argocdv1alph1.Application, error) {
	app := &argocdv1alph1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gitea-" + profile.Name,
			Namespace: giteaconfig.Deploymentparams.argocdNamespace,
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

// Generates a configmap for the banner on the gitea web page
func generateBannerConfigMap(profile *kubeflowv1.Profile, giteaconfig *GiteaConfig) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      giteaconfig.Deploymentparams.giteaBannerConfigMapName,
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Data: map[string]string{
			"body_inner_pre.tmpl": fmt.Sprintf("Welcome to AAW Gitea (%s). ", giteaconfig.Deploymentparams.classificationEn) +
				"To login use username: superuser, password: password | " +
				fmt.Sprintf("Bienvenue à AAW Gitea (%s). Pour vous connecter, utilisez le ", giteaconfig.Deploymentparams.classificationFr) +
				"nom d'utilisateur: superuser, le mot de passe: password",
		},
	}
	return cm, nil
}

// GenerateRandomBytes returns securely generated random bytes.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func generateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	// Note that err == nil only if we read len(b) bytes.
	if err != nil {
		return nil, err
	}

	return b, nil
}

// GenerateRandomStringURLSafe returns a URL-safe, base64 encoded
// securely generated random string.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func generateRandomStringURLSafe(n int) (string, error) {
	b, err := generateRandomBytes(n)
	return base64.URLEncoding.EncodeToString(b), err
}

// Provision postgres db for use with gitea for the given profile
// Responsible for provisioning the db and creating the k8s secret for the db
func provisionDb(db *sql.DB, profile *kubeflowv1.Profile, psqlparams *Psqlparams,
	kubeclient kubernetes.Interface, secretLister v1.SecretLister, replicas int32) error {
	psqlDuplicateErrorCode := pq.ErrorCode("42710")
	pqParseErrorMsg := "Could not cast error '%s' to type pq.Error!"
	// prepare profile specific db items
	profilename := fmt.Sprintf("gitea_%s", strings.Replace(profile.Name, "-", "_", -1))
	dbname := profilename

	profiledbpassword, err := generateRandomStringURLSafe(32)
	if err != nil {
		klog.Errorf("Could not generate password for profile %s!", err)
		return err
	}
	// 1. CREATE SECRET FOR PROFILE, CONTAINING HOSTNAME, DBNAME, USERNAME, PASSWORD!
	secret := generatePsqlSecret(profile, dbname, psqlparams, profilename, profiledbpassword)
	// 2. CREATE ROLE
	query := fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s",
		pq.QuoteIdentifier(profilename), pq.QuoteLiteral(profiledbpassword))
	err = performQuery(db, query)
	if err != nil {
		pqError, ok := err.(*pq.Error)
		if !ok {
			return fmt.Errorf(pqParseErrorMsg, err)
		}
		// check if the role already exists
		if pqError.Code == psqlDuplicateErrorCode {
			query = fmt.Sprintf("ALTER ROLE %s WITH PASSWORD %s", pq.QuoteIdentifier(profilename),
				pq.QuoteLiteral(profiledbpassword))
			err = performQuery(db, query)
			if err != nil {
				return err
			}
			managePsqlSecret(secret, kubeclient, secretLister, replicas)
		} else {
			return err
		}
	} else {
		managePsqlSecret(secret, kubeclient, secretLister, replicas)
	}
	// 3. GRANT ADMIN PERMISSIONS FOR CONFIGURING ROLE
	query = fmt.Sprintf("GRANT %s to %s", pq.QuoteIdentifier(profilename), pq.QuoteIdentifier(psqlparams.username))
	err = performQuery(db, query)
	if err != nil {
		return err
	}
	// 4. CREATE DB
	query = fmt.Sprintf(`CREATE DATABASE %s WITH OWNER %s TEMPLATE template0 ENCODING UTF8 LC_COLLATE "en-US" LC_CTYPE "en-US"`,
		pq.QuoteIdentifier(dbname), pq.QuoteIdentifier(profilename))
	err = performQuery(db, query)
	if err != nil {
		pqError, ok := err.(*pq.Error)
		if !ok {
			return fmt.Errorf(pqParseErrorMsg, err)
		}
		if pqError.Code == psqlDuplicateErrorCode {
			return err
		}
	}

	return nil
}

//Establish a connection wih a database provided the host, port admin user, password, and database name
func connect(host string, port string, username string, passwd string, dbname string) (*sql.DB, error) {
	connStr := fmt.Sprintf("postgres://%s@%s:%s@%s:%s/%s?sslmode=require",
		username, host, url.QueryEscape(passwd), host, port, dbname)
	maskedConnStr := fmt.Sprintf("postgres://%s@%s:%s@%s:%s/%s?sslmode=require",
		"****MASKED****", host, "****MASKED****", host, port, dbname)
	klog.Infof("Attempting to establish connection "+
		"using postgres driver with con. string (secrets masked): %s\n", maskedConnStr)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		klog.Errorf("Connection failed, error: %s\n", err)
		return nil, err
	}
	err = db.Ping()
	if err != nil {
		klog.Errorf("Connection failed, error: %s", err)
		return nil, err
	}
	klog.Infof("Connection successful.")
	return db, err
}

// Wrapper function for submitting a query to a database handle.
// Any queries are printed as is, with PASSWORD parameters masked.
func performQuery(db *sql.DB, query string, args ...any) error {
	// mask any passwords within the query
	r := regexp.MustCompile("PASSWORD *'.*'")
	maskedQuery := r.ReplaceAllString(query, "PASSWORD ****MASKED****")

	klog.Infof("Attempting query '%s'!", maskedQuery)
	_, err := db.Exec(query, args...)
	if err != nil {
		klog.Errorf("Error returned for query %s: %v", maskedQuery, err)
		return err
	}
	klog.Infof("Query '%s' successful", maskedQuery)
	return nil
}

// Creates or updates the secret for the namespaced gitea db.
func managePsqlSecret(secret *corev1.Secret, kubeClient kubernetes.Interface, secretLister v1.SecretLister, replicas int32) error {
	currentSecret, err := secretLister.Secrets(secret.Namespace).Get(secret.Name)
	if errors.IsNotFound(err) {
		if replicas != 0 {
			klog.Infof("creating secret %s/%s", secret.Namespace, secret.Name)
			_, err = kubeClient.CoreV1().Secrets(secret.Namespace).Create(
				context.Background(), secret, metav1.CreateOptions{})
			if err != nil {
				return err
			}
		}
	} else if !reflect.DeepEqual(secret.StringData, currentSecret.StringData) {
		klog.Infof("updating secret %s/%s", secret.Namespace, secret.Name)
		currentSecret.StringData = secret.StringData

		_, err = kubeClient.CoreV1().Secrets(secret.Namespace).Update(context.Background(),
			currentSecret, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

// Generates a secret for the gitea db
func generatePsqlSecret(profile *kubeflowv1.Profile, dbname string, psqlparams *Psqlparams, username string, password string) *corev1.Secret {
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},

		ObjectMeta: metav1.ObjectMeta{
			Name:      "gitea-postgresql-secret",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		StringData: map[string]string{
			"database": fmt.Sprintf("DB_TYPE=postgres\nSSL_MODE=require\nNAME=%s\nHOST=%s:%s\nUSER=%s@%s\nPASSWD=%s",
				dbname, psqlparams.hostname, psqlparams.port, username, psqlparams.hostname, password),
		},
	}
	return secret
}

func generateIstioVirtualService(profile *kubeflowv1.Profile, giteaconfig *GiteaConfig) (*istionetworkingclient.VirtualService, error) {
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
									Prefix: fmt.Sprintf("/%s/%s/", giteaconfig.Deploymentparams.giteaUrlPrefix, namespace),
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
								Host: fmt.Sprintf("%s.%s.svc.cluster.local", giteaconfig.Deploymentparams.giteaServiceUrl, namespace),
								Port: &istionetworkingv1beta1.PortSelector{
									Number: uint32(giteaconfig.Deploymentparams.giteaServicePort),
								},
							},
						},
					},
				},
				{
					Name: "gitea-redirect",
					// TODO: this should be refactored once we upgrade to Kubeflow > 1.4,
					// we will no longer need to check the http referer header once namespaced
					// menu items are supported.
					Match: []*istionetworkingv1beta1.HTTPMatchRequest{
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("https://kubeflow.aaw-dev.cloud.statcan.ca/_/gitea/?ns=%s", namespace),
									},
								},
							},
						},
						{
							Headers: map[string]*istionetworkingv1beta1.StringMatch{
								"referer": {
									MatchType: &istionetworkingv1beta1.StringMatch_Exact{
										Exact: fmt.Sprintf("https://kubeflow.aaw.cloud.statcan.ca/_/gitea/?ns=%s", namespace),
									},
								},
							},
						},
					},
					Redirect: &istionetworkingv1beta1.HTTPRedirect{
						Uri: fmt.Sprintf("/%s/%s/", giteaconfig.Deploymentparams.giteaUrlPrefix, namespace),
					},
				},
			},
		},
	}
	return &virtualService, nil
}

func generateServiceEntry(profile *kubeflowv1.Profile, psqlparams Psqlparams) (*istionetworkingclient.ServiceEntry, error) {
	// Get the namespace from the profile
	namespace := profile.Name

	// Cast port to uint, required by istio
	port, err := strconv.ParseUint(psqlparams.port, 10, 32)
	if err != nil {
		klog.Errorf("Error while generating ServiceEntry: Could not parse port to int: %s", err)
		return nil, err
	}

	// Create the ServiceEntry struct
	serviceEntry := istionetworkingclient.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gitea-postgresql-service-entry",
			Namespace: namespace,
			// Indicate that the profile owns the ServiceEntry resource
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: istionetworkingv1beta1.ServiceEntry{
			ExportTo: []string{
				".",
			},
			Hosts: []string{
				psqlparams.hostname,
			},
			Ports: []*istionetworkingv1beta1.Port{
				{
					Name:     "tcp-pgsql",
					Number:   uint32(port),
					Protocol: "TCP",
				},
			},
			Resolution: istionetworkingv1beta1.ServiceEntry_DNS,
		},
	}
	return &serviceEntry, nil
}

func init() {
	rootCmd.AddCommand(giteaCmd)
}
