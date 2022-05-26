package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/StatCan/profiles-controller/util"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

const profileLabel = "kubeflow-profile"
const automountLabel = "blob.aaw.statcan.gc.ca/automount"
const classificationLabel = "data.statcan.gc.ca/classification"

const azureNamespace = "azure-blob-csi-system"

var capacity resource.Quantity = *resource.NewScaledQuantity(100, resource.Giga)

/*
	MOUNT OPTIONS
	=============

	allow_other is needed for non-root users to mount storage

	> -o allow_other

	https://github.com/Azure/azure-storage-fuse/issues/496#issuecomment-704406829

	The other mount options come from upstream, and are performance/cost optimizations

	https://raw.githubusercontent.com/kubernetes-sigs/blob-csi-driver/master/deploy/example/storageclass-blobfuse.yaml
*/
var mountOptions = []string{
	"-o allow_other",
	"--file-cache-timeout-in-seconds=120",
	"--use-attr-cache=true",
	"--cancel-list-on-mount-seconds=10", // prevent billing charges on mounting
	"-o attr_timeout=120",
	"-o entry_timeout=120",
	"-o negative_timeout=120",
	"--log-level=LOG_WARNING", // # LOG_WARNING, LOG_INFO, LOG_DEBUG
	"--cache-size-mb=1000",    // # Default will be 80% of available memory, eviction will happen beyond that.
}

// Conf for MinIO
type AzureContainer struct {
	Name           string
	Classification string
	SecretRef      string
	Capacity       string
	ReadOnly       bool
	Owner          string // the owner could be AAW or FDI for example
}

// High level structure for storing OPA responses and metadata
// for querying the opa gateway
type OpaData struct {
	Result map[string]Bucket `json:"result"` // stores the json response from opa gateway as struct
	Classification string // a classification field to describe the classification of the buckets
	Ticker *time.Ticker
	Lock *sync.RWMutex
	KillDaemon chan(bool)
}

// Single Bucket struct for Opa Response
type Bucket struct {
	Readers []string
	Writers []string
}

var instances []AzureContainer

const classificationUnclassified = "unclassified"
const classificationProtb = "protected-b"
const aawContainerOwner = "AAW"
const fdiContainerOwner = "FDI"

var defaultInstances = `
	{"name": "standard", "classification": "unclassified", "secretRef": "azure-secret/azure-blob-csi-system", "capacity": "100Gi", "readOnly": false, "owner": "AAW"}
	{"name": "premium", "classification": "unclassified", "secretRef": "azure-secret-premium/azure-blob-csi-system", "capacity": "100Gi", "readOnly": false, "owner": "AAW"}
	{"name": "standard-ro", "classification": "protected-b", "secretRef": "azure-secret/azure-blob-csi-system", "capacity": "100Gi", "readOnly": true, "owner": "AAW"}
	{"name": "premium-ro", "classification": "protected-b", "secretRef": "azure-secret-premium/azure-blob-csi-system", "capacity": "100Gi", "readOnly": true, "owner": "AAW"}
`

// Performs an Http get request against given url, unmarshal the response as json
// and return the data as []byte
func performHttpGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	return body, nil
}

// This function can be run as a goroutine given a ticker, and it will populate 
// the OpaData struct with data while it runs at the given ticker's interval.
func (data *OpaData) dataDaemon(url string) {
	rqMsg := "(%s) Executing request against url '%s' at time: %s\n"
	errMsg := "(%s) Error while executing request against url '%s': %s\n"
	unmarshallMsg := "(%s) Error unmarshalling http response recv'd from url '%s': %s\n"
	marshalledData := &OpaData{}
	for {
		select {
		// if done channel is true, no longer perform any tasks
		case <- data.KillDaemon:
			klog.Infof("(%s) Recv'd stop from done channel, exiting loop!", data.Classification)
			return
		// as long as the ticker channel is open, execute requests
		// and populate the data struct.
		case t := <- data.Ticker.C:
			klog.Infof(rqMsg, data.Classification, url, t)
			// query the opa gateway
			bytes, err := performHttpGet(url)
			if err != nil {
				klog.Errorf(errMsg, data.Classification, url, err)
				continue
			}
			// parse the response into structured data
			err = json.Unmarshal(bytes, &marshalledData)
			if err != nil {
				klog.Errorf(unmarshallMsg, data.Classification, url, err)
				continue
			}
			delete(marshalledData.Result, "httpapi") // do not need httpapi key returned from opa
			// only update the data if all of above was a success!
			data.Lock.Lock()
			data.Result = marshalledData.Result
			data.Lock.Unlock()
		}
	}
}

func (data *OpaData) generateInstances(namespace string, capacity string) ([]AzureContainer, error) {
	var generated []AzureContainer
	fdiPvNameTemplate := "%s-fdi-%s-%s" // ex. "cboin-fdi-unclassified-bucket101"	
	// read the value stored in data struct
	data.Lock.RLock()
	opaData := data
	data.Lock.RUnlock()
	// only proceed if the daemon has populated opa data
	if opaData.Result != nil {
		return nil, nil
	}
	// build instances based on opa response.
	for bucketName, bucketContents := range data.Result {
		isReader := false
		// determine if the given profile namespace is a reader for the bucket
		for _, readerNamespace := range bucketContents.Readers {
			if readerNamespace == namespace {
				isReader = true
			}
		}
		// determine if the given profile namespace is a writer for the bucket
		isWriter := false
		for _, writerNamespace := range bucketContents.Writers {
		    if writerNamespace == namespace {
				isWriter = true
			}	
		}
		readOnly := false
		if isReader && !isWriter {
			// if reader and not writer, readonly permissions for the given profile
		    readOnly = true	
		} else if isReader && isWriter {
			// if reader and writer, than the user should have read and write permissions.
			readOnly = false
		} else { 
			// if given profile does not satisfy above conditions (is not reader or writer)
			// skip this bucket as they lack permissions
			continue
		}
		instance := AzureContainer{
			Name: fmt.Sprintf(fdiPvNameTemplate, namespace, data.Classification, bucketName),
			Classification: data.Classification,
			SecretRef: "",
			Capacity: capacity,
			ReadOnly: readOnly,
			Owner: fdiContainerOwner,
		}
		generated = append(generated, instance)
		
	}
	return generated, nil
}

// Sets the global instances variable. Expecting json-like structure, exactly like that of the defaultinstances variable.
func configInstances() {
	var config string
	if _, err := os.Stat("instances.json"); os.IsNotExist(err) {
		config = defaultInstances
	} else {
		config_bytes, err := ioutil.ReadFile("instances.json") // just pass the file name
		if err != nil {
			log.Fatal(err)
		}
		config = string(config_bytes)
	}

	dec := json.NewDecoder(strings.NewReader(config))
	for {
		var instance AzureContainer
		err := dec.Decode(&instance)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatal(err)
		}
		fmt.Println(instance)
		instances = append(instances, instance)
	}
}

// name/namespace -> (name, namespace)
func parseSecret(name string) (string, string) {
	// <name>/<namespace> or <name> => <name>/default
	secretName := name
	secretNamespace := azureNamespace
	if strings.Contains(name, "/") {
		split := strings.Split(name, "/")
		secretName = split[0]
		secretNamespace = split[1]
	}
	return secretName, secretNamespace
}

// PV names must be globally unique (they're a cluster resource)
func pvVolumeName(namespace string, instance AzureContainer) string {
	return namespace + "-" + instance.Name
}

// Generate the desired PV Spec
func pvForProfile(profile *kubeflowv1.Profile, instance AzureContainer) *corev1.PersistentVolume {

	namespace := profile.Name

	volumeName := pvVolumeName(namespace, instance)
	secretName, secretNamespace := parseSecret(instance.SecretRef)
	// Create local copy of the global variable
	mountOptions := mountOptions

	var accessMode corev1.PersistentVolumeAccessMode
	if instance.ReadOnly {
		accessMode = corev1.ReadOnlyMany
		// Doesn't work.
		mountOptions = append(mountOptions, "-o ro")
	} else {
		accessMode = corev1.ReadWriteMany
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeName,
			Labels: map[string]string{
				classificationLabel: instance.Classification,
				profileLabel:        namespace,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: capacity,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "blob.csi.azure.com",
					NodeStageSecretRef: &corev1.SecretReference{
						Name:      secretName,
						Namespace: secretNamespace,
					},
					ReadOnly: instance.ReadOnly,
					VolumeAttributes: map[string]string{
						"containerName": namespace,
					},
					VolumeHandle: volumeName,
				},
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions:                  mountOptions,
			// https://kubernetes.io/docs/concepts/storage/persistent-volumes/#reserving-a-persistentvolume
			ClaimRef: &corev1.ObjectReference{
				Name:      instance.Name,
				Namespace: namespace,
			},
		},
	}

	return pv
}

// Generate the desired PVC Spec
func pvcForProfile(profile *kubeflowv1.Profile, instance AzureContainer) *corev1.PersistentVolumeClaim {

	namespace := profile.Name

	volumeName := pvVolumeName(namespace, instance)
	storageClass := ""

	var accessMode corev1.PersistentVolumeAccessMode
	if instance.ReadOnly {
		accessMode = corev1.ReadOnlyMany
	} else {
		accessMode = corev1.ReadWriteMany
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: namespace,
			Labels: map[string]string{
				classificationLabel: instance.Classification,
				automountLabel:      "true",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: capacity,
				},
			},
			StorageClassName: &storageClass,
			VolumeName:       volumeName,
		},
	}

	return pvc
}

func deletePV(client *kubernetes.Clientset, pv *corev1.PersistentVolume) error {
	klog.Infof("Test: deleting pv %s", pv.Name)
	// TODO: UNCOMMENT ONCE DONE TESTING AND DELETE THIS TODO
	//// We **Should** delete the PVCs after, but this is handled in the control loop.
	//return client.CoreV1().PersistentVolumes().Delete(
	//	context.Background(),
	//	pv.Name,
	//	metav1.DeleteOptions{},
	//)
	return nil
}

func deletePVC(client *kubernetes.Clientset, pvc *corev1.PersistentVolumeClaim) error {
	klog.Infof("Test: deleting pvc %s/%s", pvc.Namespace, pvc.Name)

	// TODO: UNCOMMENT ONCE DONE TESTING AND DELETE THIS TODO
	// response := client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(
	// context.Background(),
	// pvc.Name,
	// metav1.DeleteOptions{},
	// )

	// The PVC won't get released if it's attached to pods.
	// killBoundPods(client, pvc)

	// return response
	return nil
}

// The PVC won't get released if it's attached to pods.
func killBoundPods(client *kubernetes.Clientset, pvc *corev1.PersistentVolumeClaim) error {
	pods, err := client.CoreV1().Pods(pvc.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		for _, vol := range pod.Spec.Volumes {
			source := vol.PersistentVolumeClaim
			if source != nil && source.ClaimName == pvc.Name {
				// Pod is bound to our claim. We have to kill it.
				client.CoreV1().Pods(pvc.Namespace).Delete(
					context.Background(),
					pod.Name,
					metav1.DeleteOptions{},
				)
			}
		}
	}

	return nil
}

func createPV(client *kubernetes.Clientset, pv *corev1.PersistentVolume) (*corev1.PersistentVolume, error) {
	klog.Infof("creating pv %s", pv.Name)
	return client.CoreV1().PersistentVolumes().Create(
		context.Background(),
		pv,
		metav1.CreateOptions{},
	)
}

func createPVC(client *kubernetes.Clientset, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	klog.Infof("creating pvc %s/%s", pvc.Namespace, pvc.Name)
	return client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(
		context.Background(),
		pvc,
		metav1.CreateOptions{},
	)
}

func getBlobClient(client *kubernetes.Clientset, instance AzureContainer) (azblob.ServiceClient, error) {

	fmt.Println(instance.SecretRef)
	secretName, secretNamespace := parseSecret(instance.SecretRef)
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(
		context.Background(),
		secretName,
		metav1.GetOptions{},
	)
	if err != nil {
		return azblob.ServiceClient{}, err
	}

	storageAccountName := string(secret.Data["azurestorageaccountname"])
	storageAccountKey := string(secret.Data["azurestorageaccountkey"])

	cred, err := azblob.NewSharedKeyCredential(storageAccountName, storageAccountKey)
	if err != nil {
		return azblob.ServiceClient{}, err
	}
	service, err := azblob.NewServiceClientWithSharedKey(
		fmt.Sprintf("https://%s.blob.core.windows.net/", storageAccountName),
		cred,
		nil,
	)
	if err != nil {
		return azblob.ServiceClient{}, err
	}
	return service, nil
}

func createContainer(service azblob.ServiceClient, containerName string) error {
	container := service.NewContainerClient(containerName)
	_, err := container.Create(context.Background(), nil)
	return err
}

var blobcsiCmd = &cobra.Command{
	Use:   "blob-csi",
	Short: "Configure Blob-Storage backed PV and PVCs",
	Long: `Configure Blob-Storage backed PV and PVCs.
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()
		unclassOpaGatewayUrl := util.ParseEnvVar("BLOB_CSI_UNCLASS_OPA_ENDPOINT")
		protbOpaGatewayUrl := util.ParseEnvVar("BLOB_CSI_PROTECTED_B_OPA_ENDPOINT")
		fdiPvStorageCapacity := util.ParseEnvVar("BLOB_CSI_FDI_PV_STORAGE_CAP")
		// Create Kubernetes config
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
		// initialize OpaData struct for unclassified OPA gateway
		unclassOpaData := &OpaData{Result: nil, 
								   Classification: "unclassified",
								   Ticker: time.NewTicker(500*time.Millisecond),
								   Lock: new(sync.RWMutex)}
		// Spawn thread which will populate the unclassified OpaData struct
		go unclassOpaData.dataDaemon(unclassOpaGatewayUrl)
		
		// initialize OpaData struct for protb OPA gateway
		protbOpaData := &OpaData{Result: nil, 
								 Classification: "protected-b",
								 Ticker: time.NewTicker(500*time.Millisecond),
								 Lock: new(sync.RWMutex)}
		// spawn thread for populating prot-b OpaData struct
		go protbOpaData.dataDaemon(protbOpaGatewayUrl)

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*5)

		// pv v.s. pvc is too tricky to read. Using Volumes v.s. Claims.
		volumeInformer := kubeInformerFactory.Core().V1().PersistentVolumes()
		claimInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims()

		volumeLister := volumeInformer.Lister()
		claimLister := claimInformer.Lister()

		// First, branch off of the service client and create a container client.
		configInstances()
		blobClients := map[string]azblob.ServiceClient{}
		for _, instance := range instances {
			if !instance.ReadOnly {
				client, err := getBlobClient(kubeClient, instance)
				if err != nil {
					panic(err.Error())
				}
				blobClients[instance.Name] = client
			}
		}

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {

				// Get all Volumes associated to this profile
				volumes := []corev1.PersistentVolume{}
				allvolumes, err := volumeLister.List(labels.Everything())
				if err != nil {
					return err
				}
				for _, v := range allvolumes {
					if v.GetOwnerReferences() != nil {
						for _, obj := range v.GetOwnerReferences() {
							if obj.Kind == "Profile" && obj.Name == profile.Name {
								volumes = append(volumes, *v)
							}
						}
					}
				}

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
				// TODO:Append any instances discovered as a result of querying the Opa gateways for
				// unclassified and protected-b
				klog.Infof("Unclass data : %s", unclassOpaData)
				klog.Infof("Protb data : %s", protbOpaData)
				unclassInstances, err := unclassOpaData.generateInstances(profile.Name, fdiPvStorageCapacity)
				protbInstances, err := protbOpaData.generateInstances(profile.Name, fdiPvStorageCapacity)

				// Generate the desired-state Claims and Volumes
				generatedVolumes := []corev1.PersistentVolume{}
				generatedClaims := []corev1.PersistentVolumeClaim{}
				for _, instance := range instances {
					generatedVolumes = append(generatedVolumes, *pvForProfile(profile, instance))
					generatedClaims = append(generatedClaims, *pvcForProfile(profile, instance))
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



				// for _, instance := range instances {
				// 	if !instance.ReadOnly {
				// 		klog.Infof("Creating Container %s/%s... ", instance.Name, profile.Name)
				// 		err := createContainer(blobClients[instance.Name], profile.Name)
				// 		if err == nil {
				// 			klog.Infof("Created Container %s/%s.", instance.Name, profile.Name)
				// 		} else if strings.Contains(err.Error(), "ContainerAlreadyExists") {
				// 			klog.Warningf("Container %s/%s Already Exists.", instance.Name, profile.Name)
				// 		} else {
				// 			klog.Fatalf(err.Error())
				// 			return err
				// 		}
				// 	}
				// }

				// // Create PV if it doesn't exist
				// for _, pv := range generatedVolumes {
				// 	if _, exists := pvExistsMap[pv.Name]; !exists {
				// 		_, err := createPV(kubeClient, &pv)
				// 		if err != nil {
				// 			return err
				// 		}
				// 	}
				// }

				// // Create PVC if it doesn't exist
				// for _, pvc := range generatedClaims {
				// 	if _, exists := pvcExistsMap[pvc.Name]; !exists {
				// 		_, err := createPVC(kubeClient, &pvc)
				// 		if err != nil {
				// 			return err
				// 		}
				// 	}
				// }

				return nil
			},
		)

		volumeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newVol := new.(*corev1.PersistentVolume)
				oldVol := old.(*corev1.PersistentVolume)

				if newVol.ResourceVersion == oldVol.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		claimInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newVol := new.(*corev1.PersistentVolumeClaim)
				oldVol := old.(*corev1.PersistentVolumeClaim)

				if newVol.ResourceVersion == oldVol.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		// Start informers
		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			unclassOpaData.KillDaemon <- true
			unclassOpaData.Ticker.Stop()
			protbOpaData.KillDaemon <- true
			protbOpaData.Ticker.Stop()
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func init() {
	rootCmd.AddCommand(blobcsiCmd)
}
