package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"time"

	azidentity "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	users "github.com/microsoftgraph/msgraph-sdk-go/users"
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
type s3keys struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

/*
AIM: create a user using the API call
This will be called with the information of the Cloud AD to create a good S3 User
https://docs.netapp.com/us-en/ontap-restapi/ontap/protocols_s3_services_svm.uuid_users_endpoint_overview.html#creating-an-s3-user-configuration
In addition to creating a user, this may also need to create the secret from the response given.
*/
func createUser(ownerEmail string, namespaceStr string, client *kubernetes.Clientset) bool {
	// Step 0 Get the App Registration Info
	secret, _ := client.CoreV1().Secrets("jose-matsuda").Get(context.Background(), "netapp-regi-secret", metav1.GetOptions{})
	TENANT_ID, _ := base64.StdEncoding.DecodeString(string(secret.Data["TENANT_ID"]))
	CLIENT_ID, _ := base64.StdEncoding.DecodeString(string(secret.Data["CLIENT_ID"]))
	CLIENT_SECRET, _ := base64.StdEncoding.DecodeString(string(secret.Data["CLIENT_SECRET"]))

	klog.Infof("TENANT ID DECODED:" + string(TENANT_ID))
	// Step 1 is authenticating with Azure to get the `onPremisesSamAccountName` to be used as an S3 user
	cred, _ := azidentity.NewClientSecretCredential( // fill this out with values later
		string(TENANT_ID),
		string(CLIENT_ID),
		string(CLIENT_SECRET),
		nil,
	)

	graphClient, _ := msgraphsdk.NewGraphServiceClientWithCredentials(
		cred, []string{"https://graph.microsoft.com/.default"})

	query := users.UserItemRequestBuilderGetQueryParameters{
		Select: []string{"onPremisesSamAccountName"},
	}

	options := users.UserItemRequestBuilderGetRequestConfiguration{
		QueryParameters: &query,
	}
	result, _ := graphClient.Users().ByUserId(ownerEmail).Get(context.Background(), &options)
	onPremAccountName := result.GetOnPremisesSamAccountName()

	klog.Infof("On Prem account name retrieved:" + *onPremAccountName)

	// Now that we have onPremAccountName we can create an S3 using POST
	//Encode the data
	postBody, _ := json.Marshal(map[string]string{
		"name": *onPremAccountName,
	})
	//Leverage Go's HTTP Post function to make request
	return false
	EARL := "https://<mgmt-ip>/api/protocols/s3/services/{svm.uuid}/users"
	resp, err := http.Post(EARL, "application/json", bytes.NewBuffer(postBody))
	//Handle Error
	if err != nil {
		log.Fatalf("An Error Occured %v", err)
	}
	defer resp.Body.Close()

	post := &s3keys{}
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
			Name:      "netapp-connect-secret", // change to be a const later or something
			Namespace: namespaceStr,
		},
		Data: map[string][]byte{
			"S3_ACCESS": []byte(post.AccessKey),
			"S3_SECRET": []byte(post.SecretKey),
		},
	}

	client.CoreV1().Secrets(namespaceStr).Create(context.Background(), usersecret, metav1.CreateOptions{})
	//fmt.Println(res["json"])
	return true
}

/*
This will check for a specific secret, and if found return true
Needs to take in profile information and then look for our specific secret
*/
func secretExists(client *kubernetes.Clientset, profileName string) bool {
	// We don't actually need secret informers, since informers look at changes in state
	// https://www.macias.info/entry/202109081800_k8s_informers.md
	// We only care about err, so no need for secret, err
	klog.Infof("Searching for secret")
	_, err := client.CoreV1().Secrets(profileName).Get(context.Background(), "netapp-connect-secret", metav1.GetOptions{})
	if err != nil {
		// Then we found it? Confirm this
		klog.Infof("Found the secret")
		return true
	}
	// Not found
	return false
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

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Get all the Labels and iterate trying to find label
				allLabels := profile.Labels
				for k, v := range allLabels {
					if k == ontapLabel {
						if !secretExists(kubeClient, profile.Name) {
							// if the secret does not exist, then do API call to create user
							klog.Infof("Secret for " + profile.Name + " not found. Creating User")
							wasSuccessful := createUser(profile.Spec.Owner.Name, profile.Name, kubeClient)
							if !wasSuccessful {
								klog.Info("Was unable to create S3 user")
							}
						}
						// Secret does exist do nothing, or for future iteration can check for expiration date
						if checkExpired(v) {
							// Do things, but for first iteration may not care.
							klog.Infof("Expired, but not going to do anything yet")
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
