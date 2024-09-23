package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"slices"
	"sync"

	azidentity "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/users"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	toolsWatch "k8s.io/client-go/tools/watch"
	"k8s.io/klog"
)

/** Implementation Notes
Currently strongly based on the blob-csi
We will look for a label on the profile, and if it exists we will do the account creation via api call.
That is step 1, eventually may also want the mounting to happen here but scoping to just account creation.

For mounting there are a lot of helpful useful functions in `blob-csi.go` that we can re-use
  like the building of the pv / pvc spec, the creation and deletion of them etc.
*/

const requestConfigMapName = "share-requests"

// const automountLabel = "blob.aaw.statcan.gc.ca/automount"
type s3keys struct { // i doubt this works
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

type createUserResponse struct {
	numRecords int         `json:"num_records"`
	records    []s3KeysObj `json:"records"`
}

type s3KeysObj struct {
	name      string `json:"name"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

type SvmInfo struct {
	Vserver string `json:"vserver"`
	Name    string `json:"name"`
	Uuid    string `json:"uuid"`
	Url     string `json:"url"`
}

type managementInfo struct {
	managementIP string
	username     string
	password     string
}

type S3Bucket struct {
	Name    string `json:"name"`
	NasPath string `json:"nas_path"`
}

type getS3Buckets struct {
	Records []S3Bucket `json:"records"`
}

type ShareError struct {
	Share string
	Error string
}

/*
Requires the onPremname, the namespace to create the secret in, the current k8s client, the svmInfo and the managementInfo
Returns true if successful
*/
func createS3User(onPremName string, namespaceStr string, client *kubernetes.Clientset, svmInfo SvmInfo, mgmInfo managementInfo) bool {
	postBody, _ := json.Marshal(map[string]interface{}{
		"name": onPremName,
		"svm": map[string]string{
			"uuid": svmInfo.Uuid,
		},
	})
	url := "https://" + mgmInfo.managementIP + "/api/protocols/s3/services/" + svmInfo.Uuid + "/users"
	statusCode, response := performHttpCall("POST", mgmInfo.username, mgmInfo.password, url, bytes.NewBuffer(postBody))

	if statusCode != 201 {
		klog.Infof("An Error Occured while creating the S3 User")
		return false
	}
	klog.Infof("The S3 user was created. Proceeding to store SVM credentials")
	// right now this is the only place we will create the secret, so I will just have it in here
	postResponseFormatted := &createUserResponse{} // must decode the []byte response into something i can mess with
	// need to determine if this unmarshals / converts to the struct correctly
	err := json.Unmarshal(response, &postResponseFormatted)
	if err != nil {
		fmt.Println("Error in JSON unmarshalling from json marshalled object:", err)
		return false
	}
	// Create the secret
	usersecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svmInfo.Name + "-conn-secret", // change to be a const later or something
			Namespace: namespaceStr,
		},
		Data: map[string][]byte{
			// Nothing else needs to be in here; as the S3_BUCKET value should be somewhere else.
			// All S3 buckets under the same SVM use the same ACCESS and SECRET to access them
			"S3_ACCESS": []byte(postResponseFormatted.records[0].AccessKey),
			"S3_SECRET": []byte(postResponseFormatted.records[0].SecretKey),
		},
	}
	_, err = client.CoreV1().Secrets(namespaceStr).Create(context.Background(), usersecret, metav1.CreateOptions{})
	if err != nil {
		klog.Infof("An Error Occured while creating the secret %v", err)
		return false
	}
	return true
}

// Applies a hash function to the bucketname to make it S3 compliant
func hashBucketName(name string) string {
	h := fnv.New64a()
	h.Write([]byte(name))
	return string(h.Sum64())
}

/*
This will create the S3 bucket. Requires the bucketName to be hashed, the nasPath and relevant management and svm information
https://docs.netapp.com/us-en/ontap-restapi/ontap/post-protocols-s3-buckets.html
*/
func createS3Bucket(svmInfo SvmInfo, mgmInfo managementInfo, bucketName string, nasPath string) bool {
	hashedName := hashBucketName(bucketName)
	// Create a string that is valid json, as thats the simplest way of working with this request
	// https://go.dev/play/p/xs_B0l3HsBw
	jsonString := fmt.Sprintf(
		`{
			"comment": "created via the ZONE controller",
			"name": "%s",
			"nas_path": "%s",
			"type": "nas",
			"policy" : {
				"statements": [
					{
						"effect": "allow",
						"actions": [
							"GetObject",
							"PutObject",
							"ListBucket",
							"GetBucketAcl",
							"GetObjectAcl"
						],
						"resources": [
							"%s",
							"%s/*"
						]
					}
				]
			}
		}`,
		hashedName, nasPath, hashedName, hashedName)
	// https://discourse.gohugo.io/t/use-same-argument-twice-in-a-printf-clause/20398
	urlString := "https://" + mgmInfo.managementIP + "/api/protocols/s3/services/" + svmInfo.Uuid + "/buckets"
	statusCode, _ := performHttpCall("POST", mgmInfo.username, mgmInfo.password, urlString, bytes.NewBuffer([]byte(jsonString)))
	if statusCode == 201 {
		klog.Infof("S3 Bucket has been created: https://docs.netapp.com/us-en/ontap-restapi/ontap/post-protocols-s3-buckets.html#response")
		return true
	} else if statusCode == 202 {
		klog.Infof("S3 Bucket job has been created: https://docs.netapp.com/us-en/ontap-restapi/ontap/post-protocols-s3-buckets.html#response")
		// In this case we may still want to check if the bucket exists after maybe 5 seconds?
		// checkIfS3BucketExists()...
		return true
	}
	klog.Errorf("Error when submitting the request to create a bucket") // TODO add error string
	return false
}

/*
This will get the onPremName given the owner email
*/
func getOnPrem(ownerEmail string, client *kubernetes.Clientset) (string, bool) {
	klog.Infof("Retrieving onpremisis Name")
	// Step 0 Get the App Registration Info
	// Don't forget to create a secret in the namespace for authentication with azure in the namespace
	// for me in aaw-dev its under jose-matsuda
	// TODO change to das? for the location of secrets
	secret, err := client.CoreV1().Secrets("netapp").Get(context.Background(), "microsoft-graph-api-secret", metav1.GetOptions{})
	if err != nil {
		klog.Infof("An Error Occured while getting registration secret %v", err)
		return "", false
	}

	TENANT_ID := string(secret.Data["TENANT_ID"])
	CLIENT_ID := string(secret.Data["CLIENT_ID"])
	CLIENT_SECRET := string(secret.Data["CLIENT_SECRET"])

	// Step 1 is authenticating with Azure to get the `onPremisesSamAccountName` to be used as an S3 user
	cred, err := azidentity.NewClientSecretCredential(
		TENANT_ID,
		CLIENT_ID,
		CLIENT_SECRET,
		nil,
	)
	if err != nil {
		klog.Infof("client credential error: %v", err)
		return "", false
	}

	graphClient, err := msgraphsdk.NewGraphServiceClientWithCredentials(
		cred, []string{"https://graph.microsoft.com/.default"})
	if err != nil {
		klog.Infof("graph client error: %v", err)
		return "", false
	}

	query := users.UserItemRequestBuilderGetQueryParameters{
		Select: []string{"onPremisesSamAccountName"},
	}

	options := users.UserItemRequestBuilderGetRequestConfiguration{
		QueryParameters: &query,
	}

	result, err := graphClient.Users().ByUserId(ownerEmail).Get(context.Background(), &options)
	if err != nil {
		klog.Infof("An Error Occured while trying to retrieve on prem name: %v", err)
		return "", false
	}

	onPremAccountName := result.GetOnPremisesSamAccountName()
	if onPremAccountName == nil {
		klog.Infof("No on prem name found for user: %s", ownerEmail)
		return "", false
	}
	return *onPremAccountName, true
}

func getManagementInfo(client *kubernetes.Clientset) (managementInfo, error) {
	klog.Infof("Getting secret containing the management information...")

	secret, err := client.CoreV1().Secrets("das").Get(context.Background(), "netapp-management-information", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Error occured while getting the management api secret: %v", err)
		return managementInfo{}, err
	}

	management_ip := string(secret.Data["MANAGEMENT_IP"])
	username := string(secret.Data["USERNAME"])
	password := string(secret.Data["PASSWORD"])
	mgmInfo := managementInfo{
		managementIP: management_ip,
		username:     username,
		password:     password,
	}
	return mgmInfo, nil
}

/*
TODO CHANGE
Using the profile namespace, will use the configmap to retrieve a list of filer shares attached to the profile
It will then iterate over the list and search for a constructed secret and if that secret is not found then we create
the S3 user (and as a result the secret)
*/
func processConfigmap(client *kubernetes.Clientset, namespace string, email string, mgmInfo managementInfo, svmInfoMap map[string]SvmInfo) bool {
	// We don't actually need secret informers, since informers look at changes in state
	// https://www.macias.info/entry/202109081800_k8s_informers.md
	// Get a list of secrets the user namespace should have accounts for using the configmap
	klog.Infof("Searching for secrets for " + namespace)
	shares, _ := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), "requesting-shares", metav1.GetOptions{})
	for k, _ := range shares.Data {
		// have to iterate and check secrets
		klog.Infof("Searching for: " + k + "-conn-secret")
		_, err := client.CoreV1().Secrets(namespace).Get(context.Background(), k+"-conn-secret", metav1.GetOptions{})
		if err != nil {
			klog.Infof("Error found, possbily secret not found, creating secret")
			// Get the OnPremName
			onPremName, foundOnPrem := getOnPrem(email, client)
			if foundOnPrem {
				// Get the svmInfo from the master list
				svmInfo := svmInfoMap[k]
				// Create the user
				wasSuccessful := createS3User(onPremName, namespace, client, svmInfo, mgmInfo)
				if !wasSuccessful {
					klog.Info("Unable to create S3 user:" + onPremName)
					return false
				}
			}
		}
	}
	return true
}

/*
This will if the secret has expired and then
*/
func checkExpired(labelValue string) bool {
	// If expired
	return true
	// Not found
	return false
}

/*
This will check for the existence of an S3 user. TODO must be called
https://docs.netapp.com/us-en/ontap-restapi/ontap/get-protocols-s3-services-users-.html
Requires: managementIP, svm.uuid, name, password and username for authentication
Returns true if it does exist
*/
func checkIfS3UserExists(mgmInfo managementInfo, uuid string, onPremName string) bool {
	// Build the request
	urlString := "https://" + mgmInfo.managementIP + "/api/protocols/s3/services/" + uuid + "/users/" + onPremName
	statusCode, _ := performHttpCall("GET", mgmInfo.username, mgmInfo.password, urlString, nil)
	if statusCode != 200 {
		klog.Errorf("Error when checking if user exists:") // TODO add error message
		return false
	}
	return true
}

/*
This will check for the existence of an S3 bucket
https://docs.netapp.com/us-en/ontap-restapi/ontap/get-protocols-s3-services-buckets-.html
Requires: managementInfo, svm.uuid and the bucketName
https://docs.netapp.com/us-en/ontap-restapi/ontap/protocols_s3_services_svm.uuid_buckets_endpoint_overview.html#retrieving-all-fields-for-all-s3-buckets-of-an-svm
^ is the best we can do, given that we cannot search a bucket by its name (can search by UUID though)
We'd need to re-use that hash function here when looking too.
Returns true if it does exist
*/
func checkIfS3BucketExists(mgmInfo managementInfo, uuid string, requestedBucket string) (bool, error) {
	// Build the request
	urlString := "https://" + mgmInfo.managementIP + "/api/protocols/s3/services/" + uuid + "/buckets?fields=**&return_records=true"
	statusCode, responseBody := performHttpCall("GET", mgmInfo.username, mgmInfo.password, urlString, nil)
	if statusCode != 200 {
		return true, errors.New("error interacting with Netapp API for checking if S3 bucket exists")
	}
	// Check the response and go through it.
	data := getS3Buckets{}
	err := json.Unmarshal(responseBody, &data)
	if err != nil {
		return true, err
	}

	for _, bucket := range data.Records {
		if bucket.Name == requestedBucket {
			// return true if the bucket is already in the svm
			return true, nil
		}
	}

	// returns false since the bucket with the requested name was not found
	return false, nil
}

// concats the values of modifierMap into the given sourceMap
func sharesMapConcat(sourceMap map[string][]string, modifierMap map[string][]string) {
	for k := range modifierMap {
		sourceMap[k] = slices.Concat(sourceMap[k], modifierMap[k])
	}
}

// formats the shares data to be compliant with a config map data's datatype
func formatSharesMap(shares map[string][]string) map[string]string {
	result := map[string]string{}
	for k := range shares {
		val, err := json.Marshal(shares[k])
		if err != nil {
			klog.Infof("Failed to format filer shares data")
		}
		result[k] = string(val)
	}
	return result
}

/*
Updates the filer shares ConfigMaps for a given namespace
- newShares is the map of filer shares that are have been processed(meaning the s3bucket got created)
and that need to be both removed from the requesting filer shares CM and added to the user's existing shares CM
- failedShares is the map of filer shares that failed being processed, for whatever reason,
and that will stay in the requesting shares CM
*/
func updateUserSharesConfigMaps(client *kubernetes.Clientset, namespace string, newShares map[string][]string, failedShares map[string][]string) {
	existingSharesCM, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), "existing-shares", metav1.GetOptions{})
	// if it can't find the configmap, it errors
	// TODO: look into if we can differenciate between a missing CM and a real error
	if err != nil {
		klog.Infof("Unable to get user existing share in %s. Reason: %v", namespace, err)
		klog.Infof("Creating user existing share config map")

		newUserShares := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-shares",
				Namespace: namespace,
			},
			Data: formatSharesMap(newShares),
		}

		_, err := client.CoreV1().ConfigMaps(namespace).Create(context.Background(), &newUserShares, metav1.CreateOptions{})
		if err != nil {
			klog.Infof("Error creating new user existing shares config map in %s. Reason: %v", namespace, err)
		}
	} else {
		// format the CM data
		userSharesData := map[string][]string{}
		for k := range existingSharesCM.Data {
			val := []string{}
			err := json.Unmarshal([]byte(existingSharesCM.Data[k]), &val)
			if err != nil {
				klog.Infof("Error creating new existing shares config map in %s. Reason: %v", namespace, err)
			}
			userSharesData[k] = val
		}

		// updates the userSharesData CM data with the new shares values
		sharesMapConcat(userSharesData, newShares)

		// update the existing-shares CM
		newUserShares := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-shares",
				Namespace: namespace,
			},
			Data: formatSharesMap(userSharesData),
		}
		_, err = client.CoreV1().ConfigMaps(namespace).Update(context.Background(), &newUserShares, metav1.UpdateOptions{})
		if err != nil {
			klog.Infof("Failed to update the existing shares configmap in %s. Reason: %v", namespace, err)
		}
	}

	if len(failedShares) == 0 {
		// delete the requesting CM
		err := client.CoreV1().ConfigMaps(namespace).Delete(context.Background(), "requesting-shares", metav1.DeleteOptions{})
		if err != nil {
			klog.Infof("Failed to delete the requesting shares configmap in %s. Reason: %v", namespace, err)
		}
	} else {
		// update the requesting CM
		newUserShares := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "requesting-shares",
				Namespace: namespace,
			},
			Data: formatSharesMap(failedShares), //TODO: fix this to be the diff between newShares and the requestingSharesCM.data
		}
		_, err := client.CoreV1().ConfigMaps(namespace).Update(context.Background(), &newUserShares, metav1.UpdateOptions{})
		if err != nil {
			klog.Infof("Failed to update the requesting shares configmap in %s. Reason: %v", namespace, err)
		}
	}
}

/*
Provides basic authentication
*/
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

/*
Does basic POST for requests to the API. Returns the code and a json formatted response
Requires requestType, username, password, url, and the requestBody.
requestType is either "GET" or "POST".
requestBody should be nil for GET requests.
https://www.makeuseof.com/go-make-http-requests/
An example requestBody assignment can look like: https://zetcode.com/golang/getpostrequest/
*/
func performHttpCall(requestType string, username string, password string, url string, requestBody io.Reader) (statusCode int, responseBody []byte) {
	req, _ := http.NewRequest(requestType, url, requestBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("accept", "application/json")
	authorization := basicAuth(username, password)
	req.Header.Set("Authorization", "Basic "+authorization)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		klog.Fatalf("error sending and returning HTTP response  : %v", err)
	}
	responseBody, err = io.ReadAll(resp.Body)
	if err != nil {
		klog.Fatalf("error reading HTTP response  : %v", err)
	}
	defer resp.Body.Close() // clean up memory
	return resp.StatusCode, responseBody
}

func getSvmInfoList(client *kubernetes.Clientset) (map[string]SvmInfo, error) {
	klog.Infof("Getting filers list...")

	filerListCM, err := client.CoreV1().ConfigMaps("das").Get(context.Background(), "filers-list", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Error occured while getting the filers list: %v", err)
		return nil, err
	}

	var svmInfoList []SvmInfo
	err = json.Unmarshal([]byte(filerListCM.Data["filers"]), &svmInfoList)
	if err != nil {
		klog.Errorf("Error occured while unmarshalling the filers list: %v", err)
		return nil, err
	}

	//format the data into something a bit more usable
	filerList := map[string]SvmInfo{}
	for _, svm := range svmInfoList {
		filerList[svm.Vserver] = svm
	}

	return filerList, nil
}

func createErrorUserConfigMap(client *kubernetes.Clientset, namespace string, share string, error error) {
	// Logs the error message for the pod logs
	klog.Errorf("Error occured for ns %s: %s", namespace, error.Error())

	errorCM, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), "shares-error", metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		//If the error CM doesn't exist, we create it
		errorData := []ShareError{{
			Share: share,
			Error: error.Error(),
		}}
		newErrors, err := json.Marshal(errorData)
		if err != nil {
			klog.Errorf("Error while mashalling error configmap for %s", namespace)
		}

		errorCM := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "shares-error",
				Namespace: namespace,
			},
			Data: map[string]string{"errors": string(newErrors)},
		}
		_, err = client.CoreV1().ConfigMaps(namespace).Create(context.Background(), &errorCM, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("Error while creating error configmap for %s", namespace)
		}

		return
	} else if err != nil {
		klog.Errorf("Error while retrieving error configmap for %s", namespace)
	}
	//If the error CM does exist, we update it
	errorData := []ShareError{}
	json.Unmarshal([]byte(errorCM.Data["errors"]), &errorData)

	errorData = append(errorData, ShareError{
		Share: share,
		Error: error.Error(),
	})

	newErrors, err := json.Marshal(errorData)
	if err != nil {
		klog.Errorf("Error while mashalling error configmap for %s", namespace)
	}

	newErrorCM := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shares-error",
			Namespace: namespace,
		},
		Data: map[string]string{"errors": string(newErrors)},
	}
	_, err = client.CoreV1().ConfigMaps(namespace).Update(context.Background(), &newErrorCM, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("Error while updating error configmap for %s", namespace)
	}
}

var ontapcvoCmd = &cobra.Command{
	Use:   "ontap-cvo",
	Short: "Configure ontap-cvo credentials",
	Long:  `Configure ontap-cvo credentials`,
	Run: func(cmd *cobra.Command, args []string) {
		var wg sync.WaitGroup
		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		// Builds k8s client for us to use, pass this in to functions for us to use
		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}

		// Obtain Management Info and svm Info, as this will not change often
		mgmInfo, err := getManagementInfo(kubeClient)
		if err != nil {
			klog.Fatalf("Error retrieving management info: %s", err.Error())
		}
		svmInfoMap, err := getSvmInfoList(kubeClient)
		if err != nil {
			klog.Fatalf("Error retrieving SVM map: %s", err.Error())
		}

		watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
			timeOut := int64(60)
			// Watches all namespaces, hence the ConfigMaps("")
			return kubeClient.CoreV1().ConfigMaps("").Watch(context.Background(), metav1.ListOptions{TimeoutSeconds: &timeOut,
				LabelSelector: requestConfigMapName})
		}
		watcher, _ := toolsWatch.NewRetryWatcher("1", &cache.ListWatch{WatchFunc: watchFunc})
		for event := range watcher.ResultChan() {
			configmap := event.Object.(*corev1.ConfigMap)
			switch event.Type {
			case watch.Modified, watch.Added:
				klog.Infof("Configmap %s", event.Type)
				processConfigmap(kubeClient, configmap.Namespace, configmap.Labels["email"], mgmInfo, svmInfoMap)
			case watch.Error:
				klog.Errorf("Configmap for requested shares in namespace:%s contains an error.", configmap.Namespace)
			}
		}
		wg.Add(1)
		wg.Wait()
	},
}

func init() {
	rootCmd.AddCommand(ontapcvoCmd)
}
