# Ontap CVO Controller

This controller is used to automate the onboarding of users onto the NetApp Ontap CVO solution which provides access to legacy filers on our cluster via a list of S3 calls and the use of the [meta-fuse-csi-plugin](https://github.com/pfnet-research/meta-fuse-csi-plugin).

In summary, the controller waits for the creation of a user [`requested-shares` configmap](#Requesting-Shares-Configmap) via the UI and does the following using a mix of kubernetes api calls and [calls to the ontap REST API](https://docs.netapp.com/us-en/ontap-restapi/ontap/getting_started_with_the_ontap_rest_api.html#using-the-ontap-rest-api-online-documentation).
1. Checks if an S3 account for the user exists in the SVM, if it does not, create it and store the credentials in a secret in the user's namespace. [More details in User Account Creation](#User-Account-Creation)
2. Checks if the path in the filer they want already exists as an S3 bucket, if it does not, create it. [More details in bucket creation](#Bucket-Creation)
3. Write the `existing-shares` configmap which contains a list of buckets that the [filer-sidecar-injector](https://github.com/StatCan/filer-sidecar-injector) will then use to mount the buckets into the user notebook.

## Setup and Deployment

This controller is built using the existing profiles-controller structure, and as such uses the same `profiles-controller` image that the other controllers use and is distinguished by using the args value of `ontap-cvo` in the container.

### Required Secrets and Permissions
For this to run successfully, the following secrets must exist in the `das` or chosen `namespace`.

The **netapp-management-information** secret which has;
- The Management IP for the field filers
- The Management IP for the SAS filers
- The Username
- The Password

These values are used when communicating with the NetApp API when creating users or buckets. 

The **

### Requesting Shares Configmap



## User Account Creation

## Bucket Creation

