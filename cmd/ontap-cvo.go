package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	azidentity "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/users"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
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

/*
This will become the good function
Requires the onPremname, the namespace to create the secret in, the current k8s client, the svmName, the svmUuId
and the username and password for the management
Returns true if successful
*/
func createS3User(onPremName string, namespaceStr string, client *kubernetes.Clientset, svmName string, svmUuId string, username string, password string) bool {
	// performHttpPost(username string, password string, url string, requestBody []byte) (statusCode int, responseBody []byte) {
	postBody, _ := json.Marshal(map[string]interface{}{
		"name": onPremName,
		"svm": map[string]interface{}{
			"uuid": svmUuId,
		},
	})
	url := ""
	statusCode, response := performHttpPost(username, password, url, postBody)

	if statusCode == 201 {
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
				Name:      svmName + "-conn-secret", // change to be a const later or something
				Namespace: namespaceStr,
			},
			Data: map[string][]byte{
				// this [0] seems a bit suspect but we will see how it works for now I don't know
				// I don't think the S3 account will be used multiple times or anything
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
	} else {
		klog.Infof("An Error Occured while creating the S3 User")
		return false
	}
}

/*
Send a request to the NetApp API to create an S3 User and store the result in a k8s secret
Uses the onPremName, the namespace, and filer to create and specify the secret.
DELETE IN FAVOUR OF CREATES3USER!!!! Just keep until pr is ready
*/
func createUser(onPremName string, namespaceStr string, client *kubernetes.Clientset, filerStr string) bool {
	//Encode the data
	postBody, _ := json.Marshal(map[string]string{
		"name": onPremName,
	})
	//Leverage Go's HTTP Post function to make request

	// Retrieve cm containting SVM uuids and IPs
	netappCm, err := client.CoreV1().ConfigMaps("netapp").Get(context.Background(), "netapp-filer-connection-info", metav1.GetOptions{})
	if err != nil {
		klog.Infof("An Error Occured while getting netapp filer connection info %v", err)
		return false
	}
	filerUUID := netappCm.Data[filerStr+"svm"]
	// ^ this will fail if it doesnt follow the filerStr+svm, which is the case for some SAS filers.
	// Retrieve the management IP and user login information to authenticate with netapp API
	secret, err := client.CoreV1().Secrets("netapp").Get(context.Background(), "netapp-management-information", metav1.GetOptions{})
	if err != nil {
		klog.Infof("An Error Occured while getting netapp management information secret %v", err)
		return false
	}

	managementIP := string(secret.Data["management_ip"])
	netappUser := string(secret.Data[filerStr+"_user"])
	// netappUserAccess := string(secret.Data[filerStr+"_access"])
	netappUserSecret := string(secret.Data[filerStr+"_secret"])

	url := "https://" + managementIP + "/api/protocols/s3/services/" + filerUUID + "/users"
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(postBody))
	req.Header.Set("Content-Type", "application/json")
	// req.Header. // need to set other information
	authorization := base64.StdEncoding.EncodeToString([]byte(netappUser + ":" + netappUserSecret)) // this must be confirmed)
	req.Header.Set("Authorization", "Basic "+authorization)
	// resp, err := http.Post(url, "application/json", bytes.NewBuffer(postBody)) // cant use this because http.post does not allow for additional headers
	//Handle Error
	/*if err != nil {
		klog.Infof("An Error Occured while calling s3 services %v", err)
	}*/
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		klog.Fatalf("error sending and returning HTTP response  : %v", err)
	}
	// defer resp.Body.Close() // unsure if needed?

	//post := &s3keys{}
	post := &createUserResponse{}
	derr := json.NewDecoder(resp.Body).Decode(post)
	if derr != nil {
		panic(derr)
	}
	if resp.StatusCode != http.StatusCreated {
		panic(resp.Status)
	}
	// Now that we have the values for the keys put it into a secret in the namespace
	usersecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      filerStr + "-conn-secret", // change to be a const later or something
			Namespace: namespaceStr,
		},
		Data: map[string][]byte{
			// this [0] seems a bit suspect but we will see how it works for now I don't know
			// I don't think the S3 account will be used multiple times or anything
			"S3_ACCESS": []byte(post.records[0].AccessKey),
			"S3_SECRET": []byte(post.records[0].SecretKey),
		},
	}

	_, err = client.CoreV1().Secrets(namespaceStr).Create(context.Background(), usersecret, metav1.CreateOptions{})
	if err != nil {
		klog.Infof("An Error Occured while creating the secret %v", err)
		return false
	}

	return true
}

/*
This will get the onPremName given the owner email
*/
func getOnPrem(ownerEmail string, client *kubernetes.Clientset) (string, bool) {
	klog.Infof("Retrieving onpremisis Name")
	// Step 0 Get the App Registration Info
	// Don't forget to create a secret in the namespace for authentication with azure in the namespace
	// for me in aaw-dev its under jose-matsuda
	secret, err := client.CoreV1().Secrets("netapp").Get(context.Background(), "netapp-regi-secret", metav1.GetOptions{})
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

/*
Using the profile namespace, will use the configmap to retrieve a list of filers attached to the profile
It will then iterate over the list and search for a constructed secret and if that secret is not found then we create
the S3 user (and as a result the secret)
*/
func checkSecrets(client *kubernetes.Clientset, profileName string, profileEmail string) bool {
	// We don't actually need secret informers, since informers look at changes in state
	// https://www.macias.info/entry/202109081800_k8s_informers.md
	// Get a list of secrets the user namespace should have accounts for using the configmap
	klog.Infof("Searching for secrets for " + profileName)
	filers, _ := client.CoreV1().ConfigMaps(profileName).Get(context.Background(), "user-filers-cm", metav1.GetOptions{})
	for k, _ := range filers.Data {
		// have to iterate and check secrets
		klog.Infof("Searching for: " + k + "-conn-secret")
		_, err := client.CoreV1().Secrets(profileName).Get(context.Background(), k+"-conn-secret", metav1.GetOptions{})
		if err != nil {
			klog.Infof("Error found, possbily secret not found, creating secret")
			// Get the OnPremName
			onPremName, foundOnPrem := getOnPrem(profileEmail, client)
			if foundOnPrem {
				// Create the user
				wasSuccessful := createUser(onPremName, profileName, client, k) // eventually change to createS3User
				if !wasSuccessful {
					klog.Info("Unable to create S3 user")
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
This will check for the existence of an S3 user
https://docs.netapp.com/us-en/ontap-restapi/ontap/get-protocols-s3-services-users-.html
Requires: managementIP, svm.uuid, name, password and username for authentication
Returns true if it does exist
*/
func checkIfS3UserExists(managementIP string, svmUuid string, onPremName string, username string, password string) bool {
	// Build the request
	//Encode the data
	urlString := "https://" + managementIP + "/api/protocols/s3/services/" + svmUuid + "/users/" + onPremName
	statusCode, _ := performHttpGet(username, password, urlString)
	// if its 200
	if statusCode == 200 {
		return true
	}
	return false
}

/*
Provides basic authentication
*/
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

/*
Does basic get for requests to the API. Returns the code and a json formatted response
Requires username, password, and url.
https://www.makeuseof.com/go-make-http-requests/
apiPath should probably be /apiPath/
*/
func performHttpGet(username string, password string, url string) (statusCode int, responseBody []byte) {
	// url := "https://" + managementIP + apiPath + filerUUID + "/users"
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Content-Type", "application/json")
	// req.Header. // need to set other information
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

/*
Does basic POST for requests to the API. Returns the code and a json formatted response
Requires username, password, url, and the requestBody.
An example requestBody assignment can look like: https://zetcode.com/golang/getpostrequest/
*/
func performHttpPost(username string, password string, url string, requestBody []byte) (statusCode int, responseBody []byte) {
	// url := "https://" + managementIP + apiPath + filerUUID + "/users"
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(requestBody))
	req.Header.Set("Content-Type", "application/json")
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

// Format JSON data helper function
func formatJSON(data []byte) string {
	var out bytes.Buffer
	err := json.Indent(&out, data, "", " ")

	if err != nil {
		fmt.Println(err)
	}
	d := out.Bytes()
	return string(d)
}

var ontapcvoCmd = &cobra.Command{
	Use:   "ontap-cvo",
	Short: "Configure ontap-cvo credentials",
	Long:  `Configure ontap-cvo credentials`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

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

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Kubeflow client: %v", err)
		}

		// Setup informers
		// kubeflow informer is necessary for watching profile updates
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		// Retrieve Information from configmaps
		// I don't think I need informers, im not watching for updates, this thing watches on profiles anyways and can just
		// get the information when I need it
		//configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
		//configMapLister := configMapInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				allLabels := profile.Labels
				for k, v := range allLabels {
					// If the label we specify exists then look for the secret
					if k == ontapLabel {
						checkSecrets(kubeClient, profile.Name, profile.Spec.Owner.Name)
						if checkExpired(v) {
							// Do things, but for first iteration may not care.
							//klog.Infof("Expired, but not going to do anything yet")
						}
					}
				} // End iterating through labels on profile
				// Could also check for deleting S3 users + secret clean up but maybe not for first iteration.
				return nil
			}, // end controller setup
		)

		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func init() {
	rootCmd.AddCommand(ontapcvoCmd)
}
