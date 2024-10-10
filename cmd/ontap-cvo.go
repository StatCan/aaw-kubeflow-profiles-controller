package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

const isForOntapLabel = "for-ontap"
const ontapEmailAnnotation = "user-email"
const userSvmSecretSuffix = "-conn-secret"

const existingSharesConfigMapName = "existing-shares"
const requestingSharesConfigMapName = "requesting-shares"
const sharesErrorsConfigMapName = "shares-errors"

type createUserResponse struct {
	Records []s3KeysObj `json:"records"`
}

type s3KeysObj struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

type SvmInfo struct {
	Vserver string `json:"vserver"`
	Name    string `json:"name"`
	Uuid    string `json:"uuid"`
	Url     string `json:"url"`
}

type ManagementInfo struct {
	ManagementIPField string
	ManagementIPSas   string
	Username          string
	Password          string
}

type S3Bucket struct {
	Name    string `json:"name"`
	NasPath string `json:"nas_path"`
}

type GetS3Buckets struct {
	NumRecords int `json:"num_records"`
}

type GetCifsShare struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ShareError struct {
	ErrorMessage string
	Svm          string
	Share        string
	Timestamp    time.Time
}

type ResponseError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Target  string `json:"target"`
	} `json:"error"`
}

// makes the customError implement the error interface
func (e *ShareError) Error() string {
	return e.ErrorMessage
}

type GetUserGroup struct {
	Records []struct {
		Users []struct {
			Name string `json:"name"`
		} `json:"users"`
	} `json:"records"`
}

type GetUserGroupId struct {
	Records []struct {
		ID int `json:"id"`
	} `json:"records"`
}

/*
Creates the S3 user using the net app API
Requires the onPremname, the namespace to create the secret in, the current k8s client, the svmInfo and the managementInfo
*/
func createS3User(onPremName string, managementIP string, namespace string, client *kubernetes.Clientset, svmInfo SvmInfo, mgmInfo ManagementInfo) error {
	postBody, _ := json.Marshal(map[string]interface{}{
		"name": onPremName,
	})
	url := "https://" + managementIP + "/api/protocols/s3/services/" + svmInfo.Uuid + "/users"
	statusCode, response := performHttpCall("POST", mgmInfo.Username, mgmInfo.Password, url, bytes.NewBuffer(postBody))

	if statusCode != 201 {
		errorStruct := ResponseError{}
		err := json.Unmarshal(response, &errorStruct)
		if err != nil {
			return err
		}
		return fmt.Errorf("an error cccured while creating the S3 User for onpremname %s: %s", onPremName, errorStruct.Error.Message)
	}
	klog.Infof("The S3 user was created. Proceeding to store SVM credentials")

	postResponseFormatted := createUserResponse{}

	err := json.Unmarshal(response, &postResponseFormatted)
	if err != nil {
		return fmt.Errorf("error in JSON unmarshalling for creating S3 user with onprem %s: %v", onPremName, err)
	}
	// Create the secret
	usersecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svmInfo.Vserver + userSvmSecretSuffix,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			// All S3 buckets under the same SVM use the same ACCESS and SECRET to access them
			"S3_ACCESS": []byte(postResponseFormatted.Records[0].AccessKey),
			"S3_SECRET": []byte(postResponseFormatted.Records[0].SecretKey),
		},
	}
	_, err = client.CoreV1().Secrets(namespace).Create(context.Background(), usersecret, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("an error cccured while creating the s3 user secret in ns %s: %v", namespace, err)
	}

	return nil
}

// Applies a hash function to the bucketname to make it S3 compliant
func hashBucketName(name string) string {
	h := fnv.New64a()
	h.Write([]byte(name))
	return strconv.FormatUint(h.Sum64(), 10)
}

/*
This will create the S3 bucket. Requires the bucketName to be hashed, the nasPath and relevant management and svm information
https://docs.netapp.com/us-en/ontap-restapi/ontap/post-protocols-s3-buckets.html
*/
func createS3Bucket(svmInfo SvmInfo, managementIP string, mgmInfo ManagementInfo, hashedbucketName string, nasPath string) error {
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
		hashedbucketName, nasPath, hashedbucketName, hashedbucketName)

	urlString := "https://" + managementIP + "/api/protocols/s3/services/" + svmInfo.Uuid + "/buckets"
	statusCode, responseBody := performHttpCall("POST", mgmInfo.Username, mgmInfo.Password, urlString, bytes.NewBuffer([]byte(jsonString)))
	if statusCode == 201 {
		// https://docs.netapp.com/us-en/ontap-restapi/ontap/post-protocols-s3-buckets.html#response
		klog.Infof("S3 Bucket for nas path %s in svm %s has been created: %v", nasPath, svmInfo.Vserver, string(responseBody))
		return nil
	} else if statusCode == 202 {
		klog.Infof("S3 Bucket job for nas path %s in svm %s has been created: %v", nasPath, svmInfo.Vserver, string(responseBody))
		return nil
	}
	errorStruct := ResponseError{}
	err := json.Unmarshal(responseBody, &errorStruct)
	if err != nil {
		return err
	}
	return fmt.Errorf("error when submitting the request to create a bucket with nas path %s: %s", nasPath, errorStruct.Error.Message)
}

/*
This will get the onPremName given the owner email
*/
func getOnPrem(ownerEmail string, client *kubernetes.Clientset) (string, error) {
	klog.Infof("Retrieving onprem Name")
	// Get the App Registration Info
	secret, err := client.CoreV1().Secrets("das").Get(context.Background(), "microsoft-graph-api-secret", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("an error occured while getting registration secret %v", err)
		return "", err
	}

	TENANT_ID := string(secret.Data["TENANT_ID"])
	CLIENT_ID := string(secret.Data["CLIENT_ID"])
	CLIENT_SECRET := string(secret.Data["CLIENT_SECRET"])

	// Authenticating with Azure to get the `onPremisesSamAccountName` to be used as an S3 user
	cred, err := azidentity.NewClientSecretCredential(
		TENANT_ID,
		CLIENT_ID,
		CLIENT_SECRET,
		nil,
	)
	if err != nil {
		klog.Errorf("client credential error: %v", err)
		return "", err
	}

	// Creating graph client object
	graphClient, err := msgraphsdk.NewGraphServiceClientWithCredentials(
		cred, []string{"https://graph.microsoft.com/.default"})
	if err != nil {
		klog.Errorf("graph client error: %v", err)
		return "", err
	}

	query := users.UserItemRequestBuilderGetQueryParameters{
		Select: []string{"onPremisesSamAccountName"},
	}

	options := users.UserItemRequestBuilderGetRequestConfiguration{
		QueryParameters: &query,
	}

	// Calling graph api
	result, err := graphClient.Users().ByUserId(ownerEmail).Get(context.Background(), &options)
	if err != nil {
		klog.Errorf("An Error Occured while trying to retrieve on prem name: %v", err)
		return "", err
	}

	onPremAccountName := result.GetOnPremisesSamAccountName()
	// TODO How to handle users without onprem name?
	if onPremAccountName == nil {
		return "", fmt.Errorf("no on prem name found for user: %s", ownerEmail)
	}
	return *onPremAccountName, nil
}

func getManagementInfo(client *kubernetes.Clientset) (ManagementInfo, error) {
	klog.Infof("Getting secret containing the management information...")

	secret, err := client.CoreV1().Secrets("das").Get(context.Background(), "netapp-management-information", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error occured while getting the management api secret: %v", err)
		return ManagementInfo{}, err
	}

	management_ip_field := string(secret.Data["MANAGEMENT_IP_FIELD"])
	username := string(secret.Data["USERNAME"])
	password := string(secret.Data["PASSWORD"])
	management_ip_sas := string(secret.Data["MANAGEMENT_IP_SAS"])
	mgmInfo := ManagementInfo{
		ManagementIPField: management_ip_field,
		Username:          username,
		Password:          password,
		ManagementIPSas:   management_ip_sas,
	}
	return mgmInfo, nil
}

func unmarshalSharesMap(sharesmap map[string]string) (map[string][]string, error) {
	output := map[string][]string{}
	for k, v := range sharesmap {
		arrayValue := []string{}
		err = json.Unmarshal([]byte(v), &arrayValue)
		if err != nil {
			return nil, err
		}
		output[k] = arrayValue
	}

	return output, nil
}

/*
When a "share-requests" labbeled configmap gets generated by a user in the frontend, process that configmap to
generate the S3 user and S3 buckets as necessary.
*/
func processConfigmap(client *kubernetes.Clientset, namespace string, email string, mgmInfo ManagementInfo, svmInfoMap map[string]SvmInfo) error {
	// Getting the requesting shares CM for user
	shares, _ := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), requestingSharesConfigMapName, metav1.GetOptions{})

	// Default to field filers IP
	managementIP := mgmInfo.ManagementIPField

	sharesData, err := unmarshalSharesMap(shares.Data)
	if err != nil {
		klog.Errorf("Error unmarshalling requesting shares for namespace %s", namespace)
		return &ShareError{
			ErrorMessage: err.Error(),
			Svm:          "",
			Share:        "",
			Timestamp:    time.Now(),
		}
	}

	for k := range sharesData {
		svmInfo := svmInfoMap[k]
		// have to iterate and check secrets
		klog.Infof("Searching for: " + k + userSvmSecretSuffix)
		_, err := client.CoreV1().Secrets(namespace).Get(context.Background(), k+userSvmSecretSuffix, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			// Create the secret for that filer
			klog.Infof("Secret not found for filer %s in ns: %s, creating secret", k, namespace)
			// Get the OnPremName
			onPremName, err := getOnPrem(email, client)
			if err != nil {
				klog.Errorf("Error occurred while getting onprem name: %v", err)
				return &ShareError{
					ErrorMessage: err.Error(),
					Svm:          svmInfo.Vserver,
					Share:        "",
					Timestamp:    time.Now(),
				}
			}

			// If it's a SASfs filer we need to use the sasmanagement ip
			if strings.Contains(svmInfo.Vserver, "sasfs") {
				managementIP = mgmInfo.ManagementIPSas
			}
			// Create the user
			// Check if user exists
			isS3UserExists, err := checkIfS3UserExists(mgmInfo, managementIP, svmInfo.Uuid, onPremName)
			if err != nil {
				klog.Errorf("Error while checking S3 User existence in namespace %s", namespace)
				return &ShareError{
					ErrorMessage: err.Error(),
					Svm:          svmInfo.Vserver,
					Share:        "",
					Timestamp:    time.Now(),
				}
			}

			if !isS3UserExists {
				// if user does not exist, create it
				err = createS3User(onPremName, managementIP, namespace, client, svmInfo, mgmInfo)
				if err != nil {
					klog.Errorf("Unable to create S3 user: %s", onPremName)
					return &ShareError{
						ErrorMessage: err.Error(),
						Svm:          svmInfo.Vserver,
						Share:        "",
						Timestamp:    time.Now(),
					}
				}
				// Assign the user to a group. A newly created user will not have been added
				err = manageUserGroups(onPremName, managementIP, namespace, svmInfo, mgmInfo)
				if err != nil {
					klog.Errorf("Unable to assign to a user group: %s", onPremName)
					return &ShareError{
						ErrorMessage: err.Error(),
						Svm:          svmInfo.Vserver,
						Share:        "",
						Timestamp:    time.Now(),
					}
				}
			}
		} else if err != nil {
			klog.Errorf("Error occurred while searching for %s secret in ns %s: %v", k, namespace, err)
			return &ShareError{
				ErrorMessage: err.Error(),
				Svm:          svmInfo.Vserver,
				Share:        "",
				Timestamp:    time.Now(),
			}
		}

		// Create the s3 buckets
		s3BucketsList := sharesData[k]
		for _, s := range s3BucketsList {
			// First verify that the share is not a duplicate
			err = validateUserInput(client, namespace, svmInfo.Vserver, s)
			if err != nil {
				klog.Errorf("Error in requesting shares for: %s", namespace)
				return &ShareError{
					ErrorMessage: err.Error(),
					Svm:          "",
					Share:        "",
					Timestamp:    time.Now(),
				}
			}

			hashedBucketName := hashBucketName(s)
			klog.Infof("Checking if the following bucket exists: %s", s)
			isBucketExists, err := checkIfS3BucketExists(mgmInfo, managementIP, svmInfo.Uuid, hashedBucketName)
			if err != nil {
				klog.Errorf("Error while checking bucket existence in namespace %s", namespace)
				return &ShareError{
					ErrorMessage: err.Error(),
					Svm:          svmInfo.Vserver,
					Share:        s,
					Timestamp:    time.Now(),
				}
			}

			if !isBucketExists {
				//Get CIFS Share path
				path, err := getCifsShare(mgmInfo, managementIP, svmInfo.Uuid, s)
				if err != nil {
					klog.Errorf("Error while getting cifs share in namespace %s", namespace)
					return &ShareError{
						ErrorMessage: err.Error(),
						Svm:          svmInfo.Vserver,
						Share:        s,
						Timestamp:    time.Now(),
					}
				}

				//create the bucket
				err = createS3Bucket(svmInfo, managementIP, mgmInfo, hashedBucketName, path)
				if err != nil {
					klog.Errorf("Error while creating s3 bucket in namespace %s", namespace)
					return &ShareError{
						ErrorMessage: err.Error(),
						Svm:          svmInfo.Vserver,
						Share:        s,
						Timestamp:    time.Now(),
					}
				}
			}
		}
	}

	// updates the config maps in the user namespace
	err = updateUserSharesConfigMaps(client, namespace, sharesData)
	if err != nil {
		klog.Errorf("Error while updating the user shares configmaps in namespace %s: %v", namespace, err)
		return &ShareError{
			ErrorMessage: err.Error(),
			Svm:          "",
			Share:        "",
			Timestamp:    time.Now(),
		}
	}
	klog.Infof("Finished processing configmap for:" + namespace)
	return nil
}

func manageUserGroups(onPremName, managementIP, namespace string, svmInfo SvmInfo, mgmInfo ManagementInfo) error {
	// We will just create the user group if the user group already exists we get
	//// "message": "Group name \"test-jose-group\" already exists for SVM \"fld9filersvm\".",
	jsonString := fmt.Sprintf(
		`{
			"comment": "s3access created by Zone controller",
			"name": "s3access",
			"policies": [
			{ "name": "FullAccess" }
			],
			"users": [
			{ "name": "%s" }
			]
		}`, onPremName)

	urlString := "https://" + managementIP + "/api/protocols/s3/services/" + svmInfo.Uuid + "/groups"
	statusCode, responseBody := performHttpCall("POST", mgmInfo.Username, mgmInfo.Password, urlString, bytes.NewBuffer([]byte(jsonString)))
	if statusCode == 201 {
		// The user group was created and the current user was given to the user group
		klog.Infof("Newly created user group s3access for svm: %s for the user: %s", svmInfo.Name, onPremName)
		return nil
	} else if statusCode == 409 {
		// for a 409 status code we get 'conflict' it already exists
		// If it does exist then we need to grab the full list of users and patch it with the new onPremName user
		// Get user list for s3access in svm
		urlString = "https://" + managementIP + "/api/protocols/s3/services/" + svmInfo.Uuid + "/groups/?name=s3access&fields=users.name"
		statusCode, responseBody := performHttpCall("GET", mgmInfo.Username, mgmInfo.Password, urlString, nil)
		if statusCode != 200 {
			errorStruct := ResponseError{}
			err := json.Unmarshal(responseBody, &errorStruct)
			if err != nil {
				return err
			}
			return fmt.Errorf("Error while retrieving list of users on group: %s", errorStruct.Error.Message)
		}
		getUsers := GetUserGroup{}
		userGroupId := GetUserGroupId{}
		// unmarshal so that we can edit https://goplay.tools/snippet/Vl_wsc9tACN
		err := json.Unmarshal(responseBody, &getUsers)
		if err != nil {
			return err
		}
		_ = json.Unmarshal(responseBody, &userGroupId)
		addUser := struct {
			Name string `json:"name"`
		}{
			Name: onPremName,
		}
		getUsers.Records[0].Users = append(getUsers.Records[0].Users, addUser)
		listToSubmit, err := json.Marshal(getUsers.Records[0])
		if err != nil {
			return fmt.Errorf("error while marshaling the new user: %v", err)
		}
		// Submit it
		urlString = "https://" + managementIP + "/api/protocols/s3/services/" + svmInfo.Uuid + "/groups/" + strconv.Itoa(userGroupId.Records[0].ID)
		statusCode, responseBody = performHttpCall("PATCH", mgmInfo.Username, mgmInfo.Password, urlString, bytes.NewBuffer(listToSubmit))
		if statusCode != 200 {
			errorStruct := ResponseError{}
			err := json.Unmarshal(responseBody, &errorStruct)
			if err != nil {
				return err
			}
			return fmt.Errorf("error while patching the new user to the user group: %s", errorStruct.Error.Message)
		}
		klog.Infof("User has been added to the user group")
		return nil
	}
	// If we get here there was another error
	errorStruct := ResponseError{}
	err := json.Unmarshal(responseBody, &errorStruct)
	if err != nil {
		return err
	}
	return fmt.Errorf("error while managing user groups %s: %s", namespace, errorStruct.Error.Message)
}

/*
This will check for the existence of an S3 user.
https://docs.netapp.com/us-en/ontap-restapi/ontap/get-protocols-s3-services-users-.html
Requires: managementIP, svm.uuid, name, password and username for authentication
Returns true if it does exist
*/
func checkIfS3UserExists(mgmInfo ManagementInfo, managementIP string, uuid string, onPremName string) (bool, error) {
	// Build the request
	urlString := "https://" + managementIP + "/api/protocols/s3/services/" + uuid + "/users/" + onPremName
	statusCode, responseBody := performHttpCall("GET", mgmInfo.Username, mgmInfo.Password, urlString, nil)
	if statusCode == 404 {
		klog.Infof("User does not exist. Must create a user")
		return false, nil
	} else if statusCode != 200 {
		errorStruct := ResponseError{}
		err := json.Unmarshal(responseBody, &errorStruct)
		if err != nil {
			return false, fmt.Errorf("Error when unmarshalling response to check if user exists: %s", err.Error())
		}
		return false, fmt.Errorf("Error when checking if user exists: %s", errorStruct.Error.Message)
	}
	klog.Infof("User for onPremName:" + onPremName + " already exists. Will not create a new S3 User")
	return true, nil
}

/*
This will check for the existence of an S3 bucket
https://docs.netapp.com/us-en/ontap-restapi/ontap/get-protocols-s3-services-buckets-.html
Requires: managementInfo, svm.uuid and the hashed bucket Name
Returns true if it does exist
*/
func checkIfS3BucketExists(mgmInfo ManagementInfo, managementIP string, uuid string, requestedBucket string) (bool, error) {
	// Build the request
	urlString := fmt.Sprintf("https://"+managementIP+"/api/protocols/s3/services/"+uuid+"/buckets?fields=**&name=%s", requestedBucket)
	statusCode, responseBody := performHttpCall("GET", mgmInfo.Username, mgmInfo.Password, urlString, nil)
	if statusCode != 200 {
		errorStruct := ResponseError{}
		err := json.Unmarshal(responseBody, &errorStruct)
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("error when checking if bucket exists: %s", errorStruct.Error.Message)
	}

	// Check the response and go through it.
	data := GetS3Buckets{}
	err := json.Unmarshal(responseBody, &data)
	if err != nil {
		return false, err
	}

	if data.NumRecords == 0 {
		// if no records, it means no buckets was found for that name
		return false, nil
	}
	klog.Infof("Hashed bucket:" + requestedBucket + " already exists.")
	return true, nil
}

/*
This will check for a CIFS share to retrieve the path for the desired bucket
https://docs.netapp.com/us-en/ontap-restapi/ontap/protocols_cifs_shares_endpoint_overview.html#retrieving-a-specific-cifs-share-configuration-for-an-svm
Requires: managementInfo, svm.uuid and the hashed bucket Name
Returns true if it does exist
*/
func getCifsShare(mgmInfo ManagementInfo, managementIP string, uuid string, requestedBucket string) (string, error) {
	// splits the bucket name if needed
	bucketPaths := strings.Split(requestedBucket, "/")
	parentPath := bucketPaths[0]
	// Build the request
	urlString := fmt.Sprintf("https://%s/api/protocols/cifs/shares/%s/%s?fields=**", managementIP, uuid, parentPath)
	statusCode, responseBody := performHttpCall("GET", mgmInfo.Username, mgmInfo.Password, urlString, nil)
	if statusCode != 200 {
		errorStruct := ResponseError{}
		err := json.Unmarshal(responseBody, &errorStruct)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("error when retrieving cifs share: %s", errorStruct.Error.Message)
	}

	// Check the response and go through it.
	data := GetCifsShare{}
	err := json.Unmarshal(responseBody, &data)
	if err != nil {
		return "", err
	}

	return data.Path, nil
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

// This will check for duplicate shares
func validateUserInput(client *kubernetes.Clientset, namespace string, svm string, shareToCheck string) error {
	// Retrieve the users existing-cm
	klog.Infof("Validating that user did not submit duplicate request " + namespace)
	existingSharesCM, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), existingSharesConfigMapName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		klog.Infof("User does not have an existing-shares configmap.")
		return nil
	}
	userSharesData, err := unmarshalSharesMap(existingSharesCM.Data)
	if err != nil {
		return fmt.Errorf("error while unmarshalling existing shares configmap in %s: %v", namespace, err)
	}
	for share := range userSharesData[svm] {
		if userSharesData[svm][share] == shareToCheck {
			klog.Errorf("Duplicate share:%s already exists for svm:%s in namespace: %s", shareToCheck, svm, namespace)
			return &ShareError{
				ErrorMessage: "The requested share already exists for the filer",
				Svm:          svm,
				Share:        shareToCheck,
				Timestamp:    time.Now(),
			}
		}
	}
	return nil
}

/*
Updates the filer shares ConfigMaps for a given namespace
- newShares is the map of filer shares that are have been processed(meaning the s3bucket got created)
and that need to be both removed from the requesting filer shares CM and added to the user's existing shares CM
*/
func updateUserSharesConfigMaps(client *kubernetes.Clientset, namespace string, newShares map[string][]string) error {
	klog.Infof("Updating user configmaps for namespace:" + namespace)
	existingSharesCM, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), existingSharesConfigMapName, metav1.GetOptions{})

	if k8serrors.IsNotFound(err) {
		klog.Infof("Creating user existing share config map")

		newUserShares := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      existingSharesConfigMapName,
				Namespace: namespace,
			},
			Data: formatSharesMap(newShares),
		}

		_, err := client.CoreV1().ConfigMaps(namespace).Create(context.Background(), &newUserShares, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("error creating new user existing shares config map in %s: %v", namespace, err)
		}
	} else if err != nil {
		return fmt.Errorf("error while retrieving existing shares configmap in %s: %v", namespace, err)
	} else {
		// format the CM data
		userSharesData, err := unmarshalSharesMap(existingSharesCM.Data)
		if err != nil {
			return fmt.Errorf("error while unmarshalling existing shares configmap in %s: %v", namespace, err)
		}

		// updates the userSharesData CM data with the new shares values
		sharesMapConcat(userSharesData, newShares)

		// update the existing-shares CM
		newUserShares := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      existingSharesConfigMapName,
				Namespace: namespace,
			},
			Data: formatSharesMap(userSharesData),
		}
		_, err = client.CoreV1().ConfigMaps(namespace).Update(context.Background(), &newUserShares, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update the existing shares configmap in %s: %v", namespace, err)
		}
	}

	// delete the requesting CM
	err = client.CoreV1().ConfigMaps(namespace).Delete(context.Background(), requestingSharesConfigMapName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete the requesting shares configmap in %s. Reason: %v", namespace, err)
	}

	return nil
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
	// Set up connecting: https://stackoverflow.com/a/59738724
	customTransport := http.DefaultTransport.(*http.Transport).Clone()
	customTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: customTransport}
	req, _ := http.NewRequest(requestType, url, requestBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("accept", "application/json")
	authorization := basicAuth(username, password)
	req.Header.Set("Authorization", "Basic "+authorization)
	//resp, err := http.DefaultClient.Do(req)
	resp, err := client.Do(req)
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

func createErrorUserConfigMap(client *kubernetes.Clientset, namespace string, error ShareError) {
	// Delete the requesting configmap
	err = client.CoreV1().ConfigMaps(namespace).Delete(context.Background(), requestingSharesConfigMapName, metav1.DeleteOptions{})
	if err != nil {
		klog.Errorf("failed to delete the requesting shares configmap in %s. Reason: %v", namespace, err)
	}
	// Logs the error message for the pod logs
	klog.Errorf("Error occured for ns %s: %v", namespace, error.Error())

	errorCM, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), sharesErrorsConfigMapName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		//If the error CM doesn't exist, we create it
		errorData := []ShareError{error}
		newErrors, err := json.Marshal(errorData)
		if err != nil {
			klog.Errorf("Error while marshalling error configmap for %s: %v", namespace, err)
			return
		}

		errorCM := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sharesErrorsConfigMapName,
				Namespace: namespace,
			},
			Data: map[string]string{"errors": string(newErrors)},
		}
		_, err = client.CoreV1().ConfigMaps(namespace).Create(context.Background(), &errorCM, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("Error while creating error configmap for %s: %v", namespace, err)
		}

		return
	} else if err != nil {
		klog.Errorf("Error while retrieving error configmap for %s: %v", namespace, err)
		return
	}

	//If the error CM does exist, we update it
	errorData := []ShareError{}
	json.Unmarshal([]byte(errorCM.Data["errors"]), &errorData)

	errorData = append(errorData, error)

	//keep only 5 latest errors
	sort.Slice(errorData[:], func(i, j int) bool {
		return errorData[i].Timestamp.After(errorData[j].Timestamp)
	})
	if len(errorData) > 5 {
		errorData = errorData[:5]
	}

	newErrors, err := json.Marshal(errorData)
	if err != nil {
		klog.Errorf("Error while marshalling error configmap for %s: %v", namespace, err)
		return
	}

	newErrorCM := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sharesErrorsConfigMapName,
			Namespace: namespace,
		},
		Data: map[string]string{"errors": string(newErrors)},
	}
	_, err = client.CoreV1().ConfigMaps(namespace).Update(context.Background(), &newErrorCM, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("Error while updating error configmap for %s: %v", namespace, err)
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
		klog.Infof("Management Info and SVM Info have been retrieved")

		watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
			timeOut := int64(60)
			// Watches all namespaces, hence the ConfigMaps("")
			return kubeClient.CoreV1().ConfigMaps("").Watch(context.Background(), metav1.ListOptions{TimeoutSeconds: &timeOut,
				LabelSelector: isForOntapLabel})
		}
		watcher, _ := toolsWatch.NewRetryWatcher("1", &cache.ListWatch{WatchFunc: watchFunc})
		for event := range watcher.ResultChan() {
			configmap := event.Object.(*corev1.ConfigMap)
			switch event.Type {
			case watch.Modified, watch.Added:
				klog.Infof("Configmap %s", event.Type)
				err := processConfigmap(kubeClient, configmap.Namespace, configmap.Annotations[ontapEmailAnnotation], mgmInfo, svmInfoMap)
				if err != nil {
					var shareErr *ShareError
					//if this is a custom error or not
					if errors.As(err, &shareErr) {
						// klogs the error and also updates the user's error configmap
						createErrorUserConfigMap(kubeClient, configmap.Namespace, *shareErr)
					} else {
						//else is if this isn't our custom shareError. Generic error
						klog.Errorf("Error occurred while processing configmap for namespace %s", configmap.Namespace)
					}
				}
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
