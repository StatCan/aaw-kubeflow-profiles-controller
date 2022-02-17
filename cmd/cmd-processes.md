# cmd

The cmd package contains source files for a variety of profile controllers for Kubeflow.

For more information about profile controllers, see the documentation for the [client-go](https://github.com/kubernetes/client-go/) library, which contains a variety of mechanisms for use when
developing custom profile controllers. The mechanisms are defined in the
[tools/cache folder](https://github.com/kubernetes/client-go/tree/master/tools/cache) of the library.

In addition, this repository includes some documentation related to controllers found [here](../docs/controller-client-go.md)

# Package components

## authpolicy.go

Responsible for creating, removing and updating authorization policies using the Istio client for a given profile.

## limitrange.go

Responsible for creating, removing and updating limit ranges (resource limits) for a given profile.

## minio.go

Responsible for the configuration of MinIO buckets for new profiles.

## network.go

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

## notebook.go

Responsible for the configuration of notebooks within Kubeflow.

## quotas.go

Responsible for the generation of resource quotas for a given profile.

## rbac.go

Responsible for the generation of roles and role bindings for a given profile.

## root.go

The root interface for the profile controllers.
