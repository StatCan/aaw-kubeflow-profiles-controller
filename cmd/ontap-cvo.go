package cmd

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
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

/*
AIM: create a user using the API call
This will be called with the information of the Cloud AD to create a good S3 User
https://docs.netapp.com/us-en/ontap-restapi/ontap/protocols_s3_services_svm.uuid_users_endpoint_overview.html#creating-an-s3-user-configuration
In addition to creating a user, this may also need to create the secret from the response given.
*/
func createUser(ownerEmail string, namespaceStr string) {

}

/*
This will check for a specific secret, and if found return true
Needs to take in profile information and then look for our specific secret
*/
func secretExists(client *kubernetes.Clientset) bool {
	// We don't actually need secret informers, since informers look at changes in state
	// https://www.macias.info/entry/202109081800_k8s_informers.md
	// We only care about err, so no need for secret, err
	_, err := client.CoreV1().Secrets("namespacehere").Get(context.Background(), "secretName", metav1.GetOptions{})
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

		// Create Kubernetes c onfig
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
						if !secretExists(kubeClient) {
							// if the secret does not exist, then do API call to create user
							klog.Infof("Secret for " + profile.Name + " not found. Creating User")
							createUser(profile.Spec.Owner.Name, profile.Name)
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
