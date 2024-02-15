package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/StatCan/profiles-controller/internal/util"
	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

/** Implementation Notes
Currently strongly based on the blob-csi
We will look for a label on the profile, and if it exists we will do the account creation via api call.
That is step 1, eventually may also want the mounting to happen here but scoping to just account creation.

For mounting there are a lot of helpful useful functions in `blob-csi.go` that we can re-use
  like the building of the pv / pvc spec, the creation and deletion of them etc.
*/

const ontapLabel = "ontap-cvo"

//const automountLabel = "blob.aaw.statcan.gc.ca/automount"

// helper for logging errors
// remnant of blob-csi keeping in case useful (this one i think for sure)
func handleError(err error) {
	if err != nil {
		log.Panic(err)
	}
}

// remnant of blob-csi keeping in case useful
func extractConfigMapData(configMapLister v1.ConfigMapLister, cmName string) []BucketData {
	configMapData, err := configMapLister.ConfigMaps(DasNamespaceName).Get(FdiConfigurationCMName)
	if err != nil {
		klog.Info("ConfigMap doesn't exist")
	}
	var bucketData []BucketData
	if err := json.Unmarshal([]byte(configMapData.Data[cmName+".json"]), &bucketData); err != nil {
		klog.Info(err)
	}
	return bucketData
}

// remnant of blob-csi keeping in case useful
// name/namespace -> (name, namespace)
func parseSecret(name string) (string, string) {
	// <name>/<namespace> or <name> => <name>/default
	secretName := name
	secretNamespace := "ontapns"
	if strings.Contains(name, "/") {
		split := strings.Split(name, "/")
		secretName = split[0]
		secretNamespace = split[1]
	}
	return secretName, secretNamespace
}

// Create a new azblob Client using a storage account
//
// returns service azblob Client to the approriate storage account
// returns err if an error is encountered
func getBlobClient(client *kubernetes.Clientset, containerConfig AzureContainerConfig) (*azblob.Client, error) {

	fmt.Println(containerConfig.SecretRef)
	secretName, secretNamespace := parseSecret(containerConfig.SecretRef)
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(
		context.Background(),
		secretName,
		metav1.GetOptions{},
	)
	if err != nil {
		log.Panic(err.Error())
		return nil, err
	}

	storageAccountName := string(secret.Data["azurestorageaccountname"])
	storageAccountKey := string(secret.Data["azurestorageaccountkey"])
	cred, err := azblob.NewSharedKeyCredential(storageAccountName, storageAccountKey)
	if err != nil {
		log.Panic(err.Error())
		return nil, err
	}

	service, err := azblob.NewClientWithSharedKeyCredential(
		fmt.Sprintf("https://%s.blob.core.windows.net/", storageAccountName),
		cred,
		nil,
	)
	if err != nil {
		log.Panic(err.Error())
		return nil, err
	}

	return service, nil
}

/*
AIM: create a user using the API call
This will be called with the information of a
*/
func createUser() {

}

var ontapcvoCmd = &cobra.Command{
	Use:   "ontap-cvo",
	Short: "Configure ontap-cvo credentials",
	Long:  `Configure ontap-cvo credentials`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		// Create Kubernetes c onfig
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
		// initialize struct for unclassified Internal configuration
		unclassInternalFdiConnector := &FdiConnector{
			FdiConfig: FDIConfig{
				Classification:          "unclassified",
				SPNSecretName:           "",
				SPNSecretNamespace:      util.ParseEnvVar("BLOB_CSI_FDI_UNCLASS_SPN_SECRET_NAMESPACE"),
				PVCapacity:              util.ParseIntegerEnvVar("BLOB_CSI_FDI_UNCLASS_PV_STORAGE_CAP"),
				StorageAccount:          util.ParseEnvVar("BLOB_CSI_FDI_UNCLASS_INTERNAL_STORAGE_ACCOUNT"),
				ResourceGroup:           util.ParseEnvVar("BLOB_CSI_FDI_UNCLASS_CS_RESOURCE_GROUP"),
				AzureStorageAuthType:    util.ParseEnvVar("BLOB_CSI_FDI_UNCLASS_AZURE_STORAGE_AUTH_TYPE"),
				AzureStorageSPNClientID: "",
				AzureStorageSPNTenantID: util.ParseEnvVar("BLOB_CSI_FDI_GLOBAL_SP_TENANTID"),
			},
		}

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		// Setup Secret informers
		secretInformer := kubeInformerFactory.Core().V1().Secrets()
		secretLister := secretInformer.Lister()

		// First, branch off of the service client and create a container client for which
		// AAW containers are stored
		aawContainerConfigs := generateAawContainerConfigs()
		blobClients := map[string]*azblob.Client{}
		for _, instance := range aawContainerConfigs {
			if !instance.ReadOnly {
				client, err := getBlobClient(kubeClient, instance)
				handleError(err)
				blobClients[instance.Name] = client
			}
		}

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				/*
					Now that we have the profile what is our order of business?
					List labels and check for a label, if it has the label then check if there is a secret.
					check secret go here, if they have one then can assume they have everything set up.
					implementation below...
				*/

				// Get all the Labels and iterate trying to find label
				allLabels := profile.Labels
				for k, v := range allLabels {
					if k == ontapLabel {
						// check for secret
					}
				}

				/*
					If there is a label, and there is no secret in the namespace, then we need to create a user and make the secret
					https://docs.netapp.com/us-en/ontap-restapi/ontap/protocols_s3_services_svm.uuid_users_endpoint_overview.html#creating-an-s3-user-configuration

					We must still determine what goes into `name` as part of the mapping.
					implementation below....
				*/

				/*
					At this point each profile with a label should have a secret, is this where we should check for the expiration date?
					If so can do it here or whatever
					implementation below...
				*/

				/*
					Expiration date has been checked / secret has been renewed.
					At this point we are done with explicit API calls whose purpose is user management
					https://docs.netapp.com/us-en/ontap-restapi/ontap/protocols_s3_services_svm.uuid_users_endpoint_overview.html
					we could look into deleting users as well based on profile but maybe not for first iteration.
					implementation below...
				*/

				/*
					Now we could possibly do the actual mounting, though this we may need help with, or maybe this should be its own
					controller. Should probably be it's own controller yes that watches NOTEBOOKS.
				*/

				// Get all Claims associated to this profile
				claims := []corev1.PersistentVolumeClaim{}
				nsclaims, err := claimLister.PersistentVolumeClaims(profile.Name).List(labels.Everything())
				if err != nil {
					return err
				}
				for _, c := range nsclaims {
					if c.GetOwnerReferences() != nil {
						for _, obj := range c.GetOwnerReferences() {
							if obj.Kind == "Profile" {
								claims = append(claims, *c)
							}
						}
					}
				}
				generatedVolumes := []corev1.PersistentVolume{}
				generatedClaims := []corev1.PersistentVolumeClaim{}
				// Generate the desired-state Claims and Volumes for AAW containers
				generatedAawVolumes, generatedAawClaims, err := generateK8sResourcesForContainer(profile, aawContainerConfigs, nil)
				if err != nil {
					klog.Fatalf("Error recv'd while generating %s PV/PVC's structs: %s", AawContainerOwner, err)
					return err
				}
				generatedVolumes = append(generatedVolumes, generatedAawVolumes...)
				generatedClaims = append(generatedClaims, generatedAawClaims...)

				// Generate the desired-state Claims and Volumes for FDI Internal unclassified containers
				var bucketData = extractConfigMapData(configMapLister, getFDIConfiMaps()[0])
				fdiUnclassIntContainerConfigs := unclassInternalFdiConnector.generateContainerConfigs(profile.Name, bucketData)
				if fdiUnclassIntContainerConfigs != nil {
					generatedFdiUnclassIntVolumes, generatedFdiUnclassIntClaims, err := generateK8sResourcesForContainer(profile, fdiUnclassIntContainerConfigs, unclassInternalFdiConnector.FdiConfig)
					if err != nil {
						klog.Fatalf("Error recv'd while generating %s unclassified PV/PVC's structs: %s", FdiContainerOwner, err)
						return err
					}
					generatedVolumes = append(generatedVolumes, generatedFdiUnclassIntVolumes...)
					generatedClaims = append(generatedClaims, generatedFdiUnclassIntClaims...)

				}

				// First pass - schedule deletion of PVs no longer desired.
				pvExistsMap := map[string]bool{}
				for _, pv := range volumes {
					delete := true
					for _, desiredPV := range generatedVolumes {
						if pv.Name == desiredPV.Name {
							delete = false
							pvExistsMap[desiredPV.Name] = true
							break
						}
					}
					if delete || pv.Status.Phase == corev1.VolumeReleased || pv.Status.Phase == corev1.VolumeFailed {
						err = deletePV(kubeClient, &pv)
						if err != nil {
							klog.Warningf("Error deleting PV %s: %s", pv.Name, err.Error())
						}
					}
				}

				// First pass - schedule deletion of PVCs no longer desired.
				pvcExistsMap := map[string]bool{}
				for _, pvc := range claims {
					delete := true
					for _, desiredPVC := range generatedClaims {
						if pvc.Name == desiredPVC.Name {
							pvcExistsMap[desiredPVC.Name] = true
							delete = false
							break
						}
					}
					if delete {
						err = deletePVC(kubeClient, &pvc)
						if err != nil {
							klog.Warningf("Error deleting PVC %s/%s: %s", pvc.Namespace, pvc.Name, err.Error())
						}
					}
				}
				return nil
			}, // end controller setup
		)

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

		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		// Wait for caches to sync
		klog.Info("Waiting for configMap informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, configMapInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func init() {
	rootCmd.AddCommand(blobcsiCmd)
}
