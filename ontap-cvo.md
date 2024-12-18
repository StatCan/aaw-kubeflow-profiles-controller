# Ontap CVO Controller

This controller is used to automate the onboarding of users onto the NetApp Ontap CVO solution which provides access to legacy filers on our cluster via a list of S3 calls and the use of the [meta-fuse-csi-plugin](https://github.com/pfnet-research/meta-fuse-csi-plugin).

In summary, the controller waits for the creation of a user [`requested-shares` configmap](#requesting-shares-configmap) via the UI and does the following using a mix of kubernetes api calls and [calls to the ontap REST API](https://docs.netapp.com/us-en/ontap-restapi/ontap/getting_started_with_the_ontap_rest_api.html#using-the-ontap-rest-api-online-documentation).
1. Checks if an S3 account for the user exists first in the user namespace and then in the SVM, if it does not, create it and store the credentials in a secret in the user's namespace. [More details in User Account Creation](#user-account-creation)
2. Checks if the path in the filer they want already exists as an S3 bucket, if it does not, create it. [More details in bucket creation](#bucket-creation)
3. Write the `existing-shares` configmap which contains a list of buckets that the [filer-sidecar-injector](https://github.com/StatCan/filer-sidecar-injector) will then use to mount the buckets into the user notebook.

## Setup and Deployment

This controller is built using the existing profiles-controller structure, and as such uses the same `profiles-controller` image that the other controllers use and is distinguished by using the args value of `ontap-cvo` in the container.

### Required Secrets and Permissions
For this to run successfully, the following secrets (see [BTIS-199 for status on persisting these secrets](https://jirab.statcan.ca/browse/BTIS-199) and configmaps (persisted in argocd) must exist in the `das` or chosen `namespace`.

The **netapp-management-information** secret which has;
- The Management IP for the field filers
- The Management IP for the SAS filers
- The Username
- The Password

These values are used when communicating with the NetApp API when creating users or buckets. 

The **microsoft-graph-api-secret** secret which has;
- The Tenant_id
- The Client_id
- The Client_secret

These values are taken from the Azure portal from an sp that has `User.Read.All` permissions on it to interact with Microsoft Graph Api to retrieve the on premises name.

The **filers-list** configmap which has the data containing information on the filers;
```
data:
  filers: |
  [
    {
        "vserver": "fldXfilersvm",
        "name": "Field X Filer",
        "uuid": "uuidHere",
        "url: "https://fldfiler.ca"
    },...
  ]
```

### Required Network Policies
In addition to allowing the flows in the cloud firewall, we must also configure our cluster network policies to allow egress to microsoft graph api and the Netapp ontap cvo solution.

### Deployment
This app is deployed via an [ArgoCD application](https://gitlab.k8s.cloud.statcan.ca/business-transformation/aaw/aaw-argocd-manifests/-/blob/das-prod-cc-00/applications/profiles-controller.yaml?ref_type=heads) that references a [chart](https://gitlab.k8s.cloud.statcan.ca/cloudnative/statcan/charts/-/tree/profiles-controller/stable/profiles-controller?ref_type=heads) that has our above configuration.

### Requesting Shares Configmap
The `requesting-shares` configmap is generated by [kubeflow-central dashboard](https://github.com/StatCan/kubeflow/tree/kubeflow-aaw2.0) and creates a configmap containing the following information;

- In the metadata it must have a `for-ontap` label, this is used by the controller to decide which configmaps to process
- In the annotations it must have the `user-email` field populated, this is used to retrieve the on premises name needed for creating the S3 User
- The data must be a map list of requested shares, as in 
```
data:
  fldXfilersvm: '["test share"]'
```
The key is the `SVM`, which will be used in user account creation to find the UUID needed when performing API calls.


## User Account Creation
Before proceeding to account creation, the controller will always first check if the associated SVM secret already exists in the namespace. The SVM secret is formatted by using the `vserver` value in the `filers-list` configmap and appending `-conn-secret` to the end of it.

If the secret does exist then we skip ahead to [bucket creation](#bucket-creation). If it does not then we must first get the onPremises name for use with the S3 account. We use the annotation value from the `requesting-shares` to query Microsoft Graph Api. This is because the mapping of permissions on the Ontap side uses the onPrem name to allow actions on the underlying S3 bucket which interfaces with the legacy filers.

**NOTE** If an onPremises name is not found, then the control loop exits and processes the next configmap.

After getting the onPremises name, the controller uses the [api endpoint to retrieve an S3 user](https://docs.netapp.com/us-en/ontap-restapi/ontap/get-protocols-s3-services-users-.html#related-ontap-commands) to see if it already exists for the SVM. If we get a `404` then we know we must create the user.

We then create the user with the [create an S3 User configuration](https://docs.netapp.com/us-en/ontap-restapi/ontap/post-protocols-s3-services-users.html#important-notes) endpoint and we store the result in a secret in the user's namespace. 

For the user to interact with the buckets, we must also assign the user to a usergroup. Since we do not know if a user group already exists; we  first attempt [creating the user group](https://docs.netapp.com/us-en/ontap-restapi/ontap/post-protocols-s3-services-groups.html) with the current user which will assign it. If it already exists we get a `409` and must get the [current, full list of users for the group](https://docs.netapp.com/us-en/ontap-restapi/ontap/get-protocols-s3-services-groups-.html). This is because we cannot _append_ a new user to the group.
After retrieving the user group we then [submit and update the list](https://docs.netapp.com/us-en/ontap-restapi/ontap/patch-protocols-s3-services-groups-.html)

## Bucket Creation
The first step taken is verifying the contents of the `requesting-shares` configmap to not allow for duplicate requests. If a duplicate request were to go through this would cause the mounting of the buckets to fail.

We then _hash_ the full path of the requested share, this is to ensure bucket name compliance and to avoid any collisions in bucket names.
With the hashed bucket name, we [check if the bucket exists](https://docs.netapp.com/us-en/ontap-restapi/ontap/get-protocols-s3-services-buckets-.html) and if it does not, we [create it](https://docs.netapp.com/us-en/ontap-restapi/ontap/post-protocols-s3-services-buckets.html)


## Updating User Configmaps
After the user and buckets have been created successfully, the controller must update the `existing-shares` configmap. This configmap is consumed by the sidecar injector when mounting buckets. The `requesting-shares` configmap will also be deleted in this step so that it does not get re-processed.

## Errors
If at any point an error occurs in the controller while configuring user shares, it will exit the current loop, delete the `requesting-shares` configmap and write the error to the `shares-errors` configmap whose contents are shown to the user on the manage filers page.