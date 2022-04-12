# profiles-controller

This repository implements custom controllers for watching `Profile` resources from Kubeflow.

## Terminology

Helpful links to k8s resources and other terminologies related to this project are provided below.

- [Profile](https://www.kubeflow.org/docs/components/multi-tenancy/getting-started/)
- [Istio AuthorizationPolicy](https://istio.io/latest/docs/reference/config/security/authorization-policy/#Source)
- [LimitRange](https://kubernetes.io/docs/concepts/policy/limit-range/)
- [ResourceQuotas](https://kubernetes.io/docs/concepts/policy/resource-quotas/)
- [Kubeflow Notebook](https://www.kubeflow.org/docs/components/notebooks/overview/)
- [Roles and RoleBinding (RBAC)](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
- [Labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/)

## Details

The profiles controller uses [client-go library](https://github.com/kubernetes/client-go/tree/master/tools/cache) extensively.
The details of interaction points of the profiles controller with various mechanisms from this library are
explained [here](docs/controller-client-go.md).

## Controllers

The [cmd](./cmd) package contains source files for a variety of profile controllers for Kubeflow.

For more information about profile controllers, see the documentation for the [client-go](https://github.com/kubernetes/client-go/) library, which contains a variety of mechanisms for use when
developing custom profile controllers. The mechanisms are defined in the
[tools/cache folder](https://github.com/kubernetes/client-go/tree/master/tools/cache) of the library.

### [authpolicy.go](./cmd/authpolicy.go)

Responsible for creating, removing and updating [Istio Authorization Policies](https://istio.io/latest/docs/reference/config/security/authorization-policy/#Source) using the [Istio client](https://github.com/istio/client-go) for a given `Profile`. Currently, the only `AuthorizationPolicy` is to block upload/download from protected-b `Notebook`'s.

### [gitea.go](./cmd/gitea.go)

Responsible for deploying [gitea](https://github.com/go-gitea/gitea) as [argocd](https://github.com/argoproj/argo-cd) applications per `Profile`. Currently, [argocd](https://github.com/argoproj/argo-cd) applications are deployed by the gitea controller based on the customized gitea manifest found [here](https://github.com/StatCan/aaw-argocd-manifests/tree/aaw-dev-cc-00/profiles-argocd-system/template/gitea). 

### [limitrange.go](./cmd/limitrange.go)

Responsible for creating, removing and updating `LimitRange` resources for a given profile. `LimitRange` resources are generated to limit the cpu and memory resources for the kubeflow profile's default container. `LimitRange` resources require the implementation of a controller managing `ResourceQuotas`, which is provided in this package (see [quotas.go](./cmd/quotas.go)). Implementing `LimitRange` resources allows any Pod to run associated with the `Profile`, restricted by a `ResourceQuota`.

### [minio.go](./cmd/minio.go)

Responsible for the configuration of MinIO buckets for the given `Profile`. Configured MinIO buckets may be used by the associated `Profile` for object storage accessible directly within their notebooks.

### [network.go](./cmd/network.go)

Responsible for the following networking policies:

- Ingress from the ingress gateway
- Ingress from knative-serving
- Egress to the cluster local gateway
- Egress from unclassified workloads
- Egress from unclassified workloads to the ingress gateway
- Egress to port 443 from protected-b workloads
- Egress to the daaas-system
- Egress to vault
- Egress to pipelines
- Egress to MinIO
- Egress to Elasticsearch
- Egress to Artifactory

### [notebook.go](./cmd/notebook.go)

Responsible for the configuration of `Notebook` resources within Kubeflow. This controller adds a default option for running a protected-b `Notebook`.

### [quotas.go](./cmd/quotas.go)

Responsible for the generation of `ResourceQuotas` for a given profile. Management of `ResourceQuotas` is essential, as it provides the constraint for total amount of compute resources that are consumable within the `Profile`. Since the `ResourceQuotas` definition included in [quotas.go](./cmd/quotas.go) provides constraints for cpu and memory, the limits for the values must be defined. These limits are defined as `LimitRange` resources and are managed by [limitrange.go](./cmd/limitrange.go).

#### [Quota Labels](https://github.com/StatCan/aaw-kubeflow-profiles-controller/issues/16)

In order for ArgoCD to sync `Profile` resources, the `/metadata/labels/` field needed to be ignored. However, this field is required in `ResourceQuota` resource generation. A `Label` is provided for each type of quota below, which allows `ResourceQuotas` to be overidden by the controller for each resource type:

- quotas.statcan.gc.ca/requests.cpu
- quotas.statcan.gc.ca/limits.cpu
- quotas.statcan.gc.ca/requests.memory
- quotas.statcan.gc.ca/limits.memory
- quotas.statcan.gc.ca/requests.storage
- quotas.statcan.gc.ca/pods
- quotas.statcan.gc.ca/services.nodeports
- quotas.statcan.gc.ca/services.loadbalancers

A special case is considered for overriding gpu resources. Although the label `quotas.statcan.gc.ca/gpu` exists for the given `Profile`, the label `requests.nvidia.com/gpu` is overidden.

### [rbac.go](./cmd/rbac.go)

Responsible for the generation of `Roles` and `RoleBinding` resources for a given profile.

A `Role` is created for each `Profile`, the following `RoleBinding`'s are created:

- `ml-pipeline` role binding for the `Profile`.
- `DAaas-AAW-Support` is granted a profile-support cluster role in the namespace for support purposes.

### [root.go](./cmd/root.go)

The root interface for the profile controllers.

## Deployment

A helm chart for deploying the profile controllers can be found [here](https://github.com/StatCan/charts/tree/master/stable/profiles-controller).
Each controller has a corresponding k8s manifest.
