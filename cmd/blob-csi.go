package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

const profileLabel = "kubeflow-profile"
const automountLabel = "blob.aaw.statcan.gc.ca/automount"
const classificationLabel = "data.statcan.gc.ca/classification"

const azureNamespace = "azure-blob-csi-system"

var capacityScalar = resource.Tera

// Configuration for FDI containers
// If an AzureContainer is of classification FDI,
// This configuration will be used in the creation of the PV
type FDIConfig struct {
	Classification          string // unclassified or prot-b
	SPNSecretName           string // service principal secret name
	SPNSecretNamespace      string // service principal secret namespace
	PVCapacity              int
	StorageAccount          string // fdi storage account in azure portal
	ResourceGroup           string // fdi resource group in azure portal
	AzureStorageAuthType    string // value of spn dictates service principal auth
	AzureStorageSPNClientID string // fdi client id for service principal in azure portal
	AzureStorageSPNTenantID string // fdi tenant id for service principal in azure portal
}

// General Azure Container Configuration
// This is the bare minimum information for creating PV linked to a container
// Within Azure portal.
// Additional configuration can be custom defined and implemented throughout
// the controller, one such example of configuration is the FDIConfig struct.
type AzureContainerConfig struct {
	Name           string // name of the container
	Classification string // unclassified or protected-b
	SecretRef      string // secret for the container
	Capacity       int
	ReadOnly       bool
	Owner          string // the owner could be AAW or FDI for example
	SPNClientID    string // Client id of the container's service principal
	Subfolder      string // name of the subfolder to mount to PV
	PVName         string // name of PV
}

// High level structure for storing configmap data and metadata
type FdiConnector struct {
	FdiConfig FDIConfig
}

type BucketData struct {
	BucketName string   `json:"bucketName`
	PvName     string   `json:"pvName"`
	SubFolder  string   `json:"subfolder"`
	Readers    []string `json:"readers"`
	Writers    []string `json:"writers"`
	SPN        string   `json:"spn"`
	FdiConfig  FDIConfig
}

const AawContainerOwner = "AAW"
const FdiContainerOwner = "FDI"
const DasNamespaceName = "daaas-system"
const FdiConfigurationCMName = "fdi-aaw-configuration"

// Helper for listing valid container owners
func getValidContainerOwners() []string {
	return []string{AawContainerOwner, FdiContainerOwner}
}

func getFDIConfiMaps() []string {
	return []string{"fdi-unclassified-internal", "fdi-protected-b-internal", "fdi-protected-b-external", "fdi-unclassified-external"}
}

var defaultAawContainerConfigs = `
{"name": "standard", "classification": "unclassified", "secretRef": "azure-secret/azure-blob-csi-system", "capacity": 10, "readOnly": false, "owner": "AAW"}
{"name": "aaw-unclassified-ro", "classification": "protected-b", "secretRef": "aawdevcc00samgprotb/azure-blob-csi-system", "capacity": 10, "readOnly": true, "owner": "AAW"}
{"name": "aaw-protected-b", "classification": "protected-b", "secretRef": "aawdevcc00samgprotb/azure-blob-csi-system", "capacity": 10, "readOnly": false, "owner": "AAW"}
`

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

func (connector *FdiConnector) generateContainerConfigs(namespace string, bucketData []BucketData) []AzureContainerConfig {
	var generated []AzureContainerConfig
	// read the value stored in data struct
	for i, _ := range bucketData {
		isReader := false
		// determine if the given profile namespace is a reader for the bucket
		for _, readerNamespace := range bucketData[i].Readers {
			if readerNamespace == namespace {
				isReader = true
			}
		}
		// determine if the given profile namespace is a writer for the bucket
		isWriter := false
		for _, writerNamespace := range bucketData[i].Writers {
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
		connector.FdiConfig.SPNSecretName = bucketData[i].SPN + "-secret"
		servicePrincipalName := strings.Replace(strings.ToUpper(bucketData[i].SPN), "-", "_", -1)
		connector.FdiConfig.AzureStorageSPNClientID = util.ParseEnvVar(servicePrincipalName + "_AZURE_STORAGE_SPN_CLIENTID")
		connector.FdiConfig.AzureStorageSPNTenantID = util.ParseEnvVar("BLOB_CSI_FDI_GLOBAL_SP_TENANTID")
		containerConfig := AzureContainerConfig{
			Name:           bucketData[i].BucketName,
			Classification: connector.FdiConfig.Classification,
			SecretRef:      fmt.Sprintf("%s/%s", connector.FdiConfig.SPNSecretName, connector.FdiConfig.SPNSecretNamespace),
			Capacity:       connector.FdiConfig.PVCapacity,
			ReadOnly:       readOnly,
			Owner:          FdiContainerOwner,
			SPNClientID:    connector.FdiConfig.AzureStorageSPNClientID,
			Subfolder:      bucketData[i].SubFolder,
			PVName:         bucketData[i].PvName,
		}
		generated = append(generated, containerConfig)
	}
	return generated
}

// Generate container configs for AAW. Expecting json-like structure, exactly like that of the defaultAawContainerConfigs variable.
func generateAawContainerConfigs() []AzureContainerConfig {
	// Holds the container aawContainerConfigs configuration for AAW containers that will created per profile.
	var aawContainerConfigs []AzureContainerConfig

	var config string
	if _, err := os.Stat("instances.json"); os.IsNotExist(err) {
		config = defaultAawContainerConfigs
	} else {
		config_bytes, err := ioutil.ReadFile("instances.json") // just pass the file name
		if err != nil {
			log.Fatal(err)
		}
		config = string(config_bytes)
	}

	dec := json.NewDecoder(strings.NewReader(config))
	for {
		var containerConfig AzureContainerConfig
		err := dec.Decode(&containerConfig)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatal(err)
		}
		fmt.Println(containerConfig)
		aawContainerConfigs = append(aawContainerConfigs, containerConfig)
	}
	return aawContainerConfigs
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
// Names will be built starting with namespace, followed by -{postfix}
// for each postfix in postfixes
func buildPvName(namespace string, postfixes ...string) string {
	pvName := namespace
	for _, postfix := range postfixes {
		pvName = pvName + fmt.Sprintf("-%s", strings.ToLower(postfix))
	}
	return pvName
}

// Generate the desired PV Spec, depending on the type of AzureContainer.
// Currently, types of AAW and FDI are implemented for the Owners field within
// the AzureContainer struct
func pvForProfile(profile *kubeflowv1.Profile, containerConfig AzureContainerConfig, configInterface interface{}) (*corev1.PersistentVolume, error) {
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
		"--log-level=LOG_DEBUG", // # LOG_WARNING, LOG_INFO, LOG_DEBUG
		"--cache-size-mb=1000",  // # Default will be 80% of available memory, eviction will happen beyond that.
	}
	namespace := profile.Name
	secretName, secretNamespace := parseSecret(containerConfig.SecretRef)
	var volumeName string
	var pvcName string
	var accessMode corev1.PersistentVolumeAccessMode

	if containerConfig.ReadOnly {
		accessMode = corev1.ReadOnlyMany
		// Doesn't work.
		mountOptions = append(mountOptions, "-o ro")
	} else {
		accessMode = corev1.ReadWriteMany
	}
	var volumeAttributes map[string]string
	if containerConfig.Owner == AawContainerOwner {
		// for now, aaw is default, and no additional over the base scenario
		volumeName = buildPvName(namespace, containerConfig.Name)
		pvcName = containerConfig.Name
		volumeAttributes = map[string]string{
			"containerName": formattedContainerName(namespace),
		}
	} else if containerConfig.Owner == FdiContainerOwner {
		// Configure PV attributes specific to the Owner of the container the PV will be connected to.
		// FDI authenticates with service principal
		fdiConfig, ok := configInterface.(FDIConfig)
		if !ok {
			return nil, fmt.Errorf("invalid configuration passed for %s owned container", FdiContainerOwner)
		}
		volumeName = buildPvName(namespace, containerConfig.Owner, containerConfig.Classification, containerConfig.PVName)
		pvcName = fmt.Sprintf("%s-%s-%s", strings.ToLower(containerConfig.Owner), containerConfig.PVName, containerConfig.Classification)
		volumeAttributes = map[string]string{
			"containerName":           containerConfig.Name,
			"storageAccount":          fdiConfig.StorageAccount,
			"resourceGroup":           fdiConfig.ResourceGroup,
			"AzureStorageAuthType":    fdiConfig.AzureStorageAuthType,
			"AzureStorageSPNClientId": containerConfig.SPNClientID,
			"AzureStorageSPNTenantId": fdiConfig.AzureStorageSPNTenantID,
			"protocol":                "fuse2",
		}
		// Mount subfolder using blobfuse2 flag
		if containerConfig.Subfolder != "" {
			mountOptions = append(mountOptions, "--subdirectory="+containerConfig.Subfolder)
		}
		// FDI system uses adls storage, so we must provide this flag in mount options
		mountOptions = append(mountOptions, "--use-adls=true")
	} else {
		// Throw error for any other owner, until something is implemented.
		return nil, fmt.Errorf("no PV configuration exists for owner '%s'. Valid owners are: %s", containerConfig.Owner,
			getValidContainerOwners())
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeName,
			Labels: map[string]string{
				classificationLabel: containerConfig.Classification,
				profileLabel:        namespace,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewScaledQuantity(int64(containerConfig.Capacity), capacityScalar),
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "blob.csi.azure.com",
					NodeStageSecretRef: &corev1.SecretReference{
						Name:      secretName,
						Namespace: secretNamespace,
					},
					ReadOnly:         containerConfig.ReadOnly,
					VolumeAttributes: volumeAttributes,
					VolumeHandle:     volumeName,
				},
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions:                  mountOptions,
			// https://kubernetes.io/docs/concepts/storage/persistent-volumes/#reserving-a-persistentvolume
			ClaimRef: &corev1.ObjectReference{
				Name:      pvcName,
				Namespace: namespace,
			},
		},
	}

	return pv, nil
}

// Generate the desired PVC Spec
func pvcForProfile(profile *kubeflowv1.Profile, containerConfig AzureContainerConfig) *corev1.PersistentVolumeClaim {

	namespace := profile.Name
	var volumeName string
	var pvcName string
	if containerConfig.Owner == AawContainerOwner {
		volumeName = buildPvName(namespace, containerConfig.Name)
		pvcName = containerConfig.Name

	} else if containerConfig.Owner == FdiContainerOwner {
		volumeName = buildPvName(namespace, containerConfig.Owner, containerConfig.Classification, containerConfig.PVName)
		pvcName = fmt.Sprintf("%s-%s-%s", strings.ToLower(containerConfig.Owner), containerConfig.PVName, containerConfig.Classification)
	}
	storageClass := ""

	var accessMode corev1.PersistentVolumeAccessMode
	if containerConfig.ReadOnly {
		accessMode = corev1.ReadOnlyMany
	} else {
		accessMode = corev1.ReadWriteMany
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Labels: map[string]string{
				classificationLabel: containerConfig.Classification,
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
					corev1.ResourceStorage: *resource.NewScaledQuantity(int64(containerConfig.Capacity), capacityScalar),
				},
			},
			StorageClassName: &storageClass,
			VolumeName:       volumeName,
		},
	}

	return pvc
}

func deletePV(client *kubernetes.Clientset, pv *corev1.PersistentVolume) error {
	klog.Infof("deleting pv %s", pv.Name)
	// We **Should** delete the PVCs after, but this is handled in the control loop.
	return client.CoreV1().PersistentVolumes().Delete(
		context.Background(),
		pv.Name,
		metav1.DeleteOptions{},
	)
}

func deletePVC(client *kubernetes.Clientset, pvc *corev1.PersistentVolumeClaim) error {
	klog.Infof("deleting pvc %s/%s", pvc.Namespace, pvc.Name)

	response := client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(
		context.Background(),
		pvc.Name,
		metav1.DeleteOptions{},
	)

	killBoundPods(client, pvc)

	return response
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

func getBlobClient(client *kubernetes.Clientset, containerConfig AzureContainerConfig) (azblob.ServiceClient, error) {

	fmt.Println(containerConfig.SecretRef)
	secretName, secretNamespace := parseSecret(containerConfig.SecretRef)
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

// Formats container names to be in accordance with the azure requirement
// https://docs.microsoft.com/en-us/rest/api/storageservices/naming-and-referencing-containers--blobs--and-metadata#container-names
func formattedContainerName(containerName string) string {
	// remove any non-alphanumeric characters, except dashes
	containerName = regexp.MustCompile("[^a-zA-Z0-9-]").ReplaceAllString(containerName, "")
	// reduce multiple dashes to single dashes
	containerName = regexp.MustCompile("[-]+").ReplaceAllString(containerName, "-")
	// enforce lower case
	containerName = strings.ToLower(containerName)
	// confirm first character is not a dash, and pad with 'a' if it is.
	if string(containerName[0]) == "-" {
		containerName = "a" + containerName
	}

	// pad container names that do not meet minimum length
	if len(containerName) < 3 {
		padding := 3 - len(containerName)
		containerName = containerName + strings.Repeat("a", padding)
		// truncate container names that exceed max length
	} else if len(containerName) > 63 {
		containerName = containerName[:63]
	}
	// confirm last character is not a dash, and replace with 'a' if it is
	if string(containerName[len(containerName)-1]) == "-" {
		containerName = containerName[:len(containerName)-1] + "a"
	}
	return containerName
}

func createContainer(service azblob.ServiceClient, containerName string) error {
	container := service.NewContainerClient(containerName)
	_, err := container.Create(context.Background(), nil)
	return err
}

// Generates the PV's and PVC's corresponding to the container
func generateK8sResourcesForContainer(profile *kubeflowv1.Profile, containerConfigs []AzureContainerConfig, configInterface interface{}) ([]corev1.PersistentVolume, []corev1.PersistentVolumeClaim, error) {
	generatedVolumes := []corev1.PersistentVolume{}
	generatedClaims := []corev1.PersistentVolumeClaim{}
	for _, containerConfig := range containerConfigs {

		// iterate through each containerconfig
		// make a list of foldernames from the same container and pass into pvForProfile
		//
		if containerConfig.Owner == "FDI" {
			if strings.Contains(containerConfig.Name, "/") {
				containerConfig.Name = strings.Split(containerConfig.Name, "/")[0]
			}
		}
		pv, err := pvForProfile(profile, containerConfig, configInterface)
		if err != nil {
			return nil, nil, err
		}
		generatedVolumes = append(generatedVolumes, *pv)
		generatedClaims = append(generatedClaims, *pvcForProfile(profile, containerConfig))
	}
	return generatedVolumes, generatedClaims, nil
}

var blobcsiCmd = &cobra.Command{
	Use:   "blob-csi",
	Short: "Configure Blob-Storage backed PV and PVCs",
	Long: `Configure Blob-Storage backed PV and PVCs.
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

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
		// initialize struct for protected-b Internal configuration
		protbInternalFdiConnector := &FdiConnector{
			FdiConfig: FDIConfig{
				Classification:          "protected-b",
				SPNSecretName:           "",
				SPNSecretNamespace:      util.ParseEnvVar("BLOB_CSI_FDI_PROTECTED_B_SPN_SECRET_NAMESPACE"),
				PVCapacity:              util.ParseIntegerEnvVar("BLOB_CSI_FDI_PROTECTED_B_PV_STORAGE_CAP"),
				StorageAccount:          util.ParseEnvVar("BLOB_CSI_FDI_PROTECTED_B_INTERNAL_STORAGE_ACCOUNT"),
				ResourceGroup:           util.ParseEnvVar("BLOB_CSI_FDI_PROTECTED_B_CS_RESOURCE_GROUP"),
				AzureStorageAuthType:    util.ParseEnvVar("BLOB_CSI_FDI_PROTECTED_B_AZURE_STORAGE_AUTH_TYPE"),
				AzureStorageSPNClientID: "",
				AzureStorageSPNTenantID: util.ParseEnvVar("BLOB_CSI_FDI_GLOBAL_SP_TENANTID"),
			},
		}
		// initialize struct for unclassified External configuration
		unclassExternalFdiConnector := &FdiConnector{
			FdiConfig: FDIConfig{
				Classification:          "unclassified",
				SPNSecretName:           "",
				SPNSecretNamespace:      util.ParseEnvVar("BLOB_CSI_FDI_UNCLASS_SPN_SECRET_NAMESPACE"),
				PVCapacity:              util.ParseIntegerEnvVar("BLOB_CSI_FDI_UNCLASS_PV_STORAGE_CAP"),
				StorageAccount:          util.ParseEnvVar("BLOB_CSI_FDI_UNCLASS_EXTERNAL_STORAGE_ACCOUNT"),
				ResourceGroup:           util.ParseEnvVar("BLOB_CSI_FDI_UNCLASS_CS_RESOURCE_GROUP"),
				AzureStorageAuthType:    util.ParseEnvVar("BLOB_CSI_FDI_UNCLASS_AZURE_STORAGE_AUTH_TYPE"),
				AzureStorageSPNClientID: "",
				AzureStorageSPNTenantID: util.ParseEnvVar("BLOB_CSI_FDI_GLOBAL_SP_TENANTID"),
			},
		}
		// initialize struct for protected-b External configuration
		protbExternalFdiConnector := &FdiConnector{
			FdiConfig: FDIConfig{
				Classification:          "protected-b",
				SPNSecretName:           "",
				SPNSecretNamespace:      util.ParseEnvVar("BLOB_CSI_FDI_PROTECTED_B_SPN_SECRET_NAMESPACE"),
				PVCapacity:              util.ParseIntegerEnvVar("BLOB_CSI_FDI_PROTECTED_B_PV_STORAGE_CAP"),
				StorageAccount:          util.ParseEnvVar("BLOB_CSI_FDI_PROTECTED_B_EXTERNAL_STORAGE_ACCOUNT"),
				ResourceGroup:           util.ParseEnvVar("BLOB_CSI_FDI_PROTECTED_B_CS_RESOURCE_GROUP"),
				AzureStorageAuthType:    util.ParseEnvVar("BLOB_CSI_FDI_PROTECTED_B_AZURE_STORAGE_AUTH_TYPE"),
				AzureStorageSPNClientID: "",
				AzureStorageSPNTenantID: util.ParseEnvVar("BLOB_CSI_FDI_GLOBAL_SP_TENANTID"),
			},
		}

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		// pv v.s. pvc is too tricky to read. Using Volumes v.s. Claims.
		volumeInformer := kubeInformerFactory.Core().V1().PersistentVolumes()
		claimInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims()
		// Setup configMap informers
		configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
		volumeLister := volumeInformer.Lister()
		claimLister := claimInformer.Lister()
		configMapLister := configMapInformer.Lister()

		// First, branch off of the service client and create a container client for which
		// AAW containers are stored
		aawContainerConfigs := generateAawContainerConfigs()
		blobClients := map[string]azblob.ServiceClient{}
		for _, instance := range aawContainerConfigs {
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
				// Generate the desired-state Claims and Volumes for FDI Internal prot-b containers
				bucketData = extractConfigMapData(configMapLister, getFDIConfiMaps()[1])
				fdiProtbIntContainerConfigs := protbInternalFdiConnector.generateContainerConfigs(profile.Name, bucketData)
				if fdiProtbIntContainerConfigs != nil {
					generatedFdiProtbIntVolumes, generatedFdiProtbIntClaims, err := generateK8sResourcesForContainer(profile, fdiProtbIntContainerConfigs, protbInternalFdiConnector.FdiConfig)
					if err != nil {
						klog.Fatalf("Error recv'd while generating %s prot-b PV/PVC's structs: %s", FdiContainerOwner, err)
						return err
					}
					generatedVolumes = append(generatedVolumes, generatedFdiProtbIntVolumes...)
					generatedClaims = append(generatedClaims, generatedFdiProtbIntClaims...)
				}

				// Generate the desired-state Claims and Volumes for FDI External prot-b containers
				bucketData = extractConfigMapData(configMapLister, getFDIConfiMaps()[2])
				fdiProtbExtContainerConfigs := protbExternalFdiConnector.generateContainerConfigs(profile.Name, bucketData)
				if fdiProtbExtContainerConfigs != nil {
					generatedFdiProtbExtVolumes, generatedFdiProtbExtClaims, err := generateK8sResourcesForContainer(profile, fdiProtbExtContainerConfigs, protbExternalFdiConnector.FdiConfig)
					if err != nil {
						klog.Fatalf("Error recv'd while generating %s prot-b PV/PVC's structs: %s", FdiContainerOwner, err)
						return err
					}
					generatedVolumes = append(generatedVolumes, generatedFdiProtbExtVolumes...)
					generatedClaims = append(generatedClaims, generatedFdiProtbExtClaims...)
				}

				// Generate the desired-state Claims and Volumes for FDI External unclassified containers
				bucketData = extractConfigMapData(configMapLister, getFDIConfiMaps()[3])
				fdiUnclassExtContainerConfigs := unclassExternalFdiConnector.generateContainerConfigs(profile.Name, bucketData)
				if fdiUnclassExtContainerConfigs != nil {
					generatedFdiUnclassExtVolumes, generatedFdiUnclassExtClaims, err := generateK8sResourcesForContainer(profile, fdiUnclassExtContainerConfigs, unclassExternalFdiConnector.FdiConfig)
					if err != nil {
						klog.Fatalf("Error recv'd while generating %s prot-b PV/PVC's structs: %s", FdiContainerOwner, err)
						return err
					}
					generatedVolumes = append(generatedVolumes, generatedFdiUnclassExtVolumes...)
					generatedClaims = append(generatedClaims, generatedFdiUnclassExtClaims...)
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

				// aaw containers are created by this code for the given profile
				for _, aawContainerConfig := range aawContainerConfigs {
					formattedContainerName := formattedContainerName(profile.Name)
					if !aawContainerConfig.ReadOnly {
						klog.Infof("Creating Container %s/%s... ", aawContainerConfig.Name, formattedContainerName)
						err := createContainer(blobClients[aawContainerConfig.Name], formattedContainerName)
						if err == nil {
							klog.Infof("Created Container %s/%s.", aawContainerConfig.Name, formattedContainerName)
						} else if strings.Contains(err.Error(), "ContainerAlreadyExists") {
							klog.Warningf("Container %s/%s Already Exists.", aawContainerConfig.Name, formattedContainerName)
						} else {
							klog.Fatalf(err.Error())
							return err
						}
					}
				}
				// Create PVC if it doesn't exist
				for _, pvc := range generatedClaims {
					if _, exists := pvcExistsMap[pvc.Name]; !exists {
						_, err := createPVC(kubeClient, &pvc)
						if err != nil {
							return err
						}
					}
				}
				// Create PV if it doesn't exist
				for _, pv := range generatedVolumes {
					if _, exists := pvExistsMap[pv.Name]; !exists {
						_, err := createPV(kubeClient, &pv)
						if err != nil {
							return err
						}
					}
				}

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
