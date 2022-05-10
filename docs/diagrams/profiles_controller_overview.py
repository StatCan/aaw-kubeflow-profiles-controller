import os

from diagrams import Cluster
from diagrams import Diagram
from diagrams import Edge

from diagrams.azure.compute import ContainerRegistries

from diagrams.k8s.compute import Deployment

from diagrams.custom import Custom


def myself() -> str:
    f = os.path.basename(__file__)
    no_ext = ".".join(f.split(".")[:-1])
    return no_ext


with Diagram(myself(), show=False):
    with Cluster("Azure Container Registry (azurecr)"):
        azure_cr = ContainerRegistries("profiles-controller:<SHA>")
    with Cluster("gitlab.k8s"):
        with Cluster("aaw-dev-cc-00 repo"):
            gitlab_tf = Custom("argocd_operator.tf", icon_path="icons/gitlab.png")

    with Cluster("github.com"):
        with Cluster("aaw-argocd-manifests repo"):
            with Cluster("/daaas-system/profile-controllers/"):
                github_daaas_system = Custom(
                    "profiles-controller/application.jsonnet",
                    icon_path="icons/github.png",
                )
        with Cluster("charts repo"):
            with Cluster("/stable/profiles-controller/"):
                github_profiles_controller_chart = Custom(
                    "profiles-controller chart", icon_path="icons/github.png"
                )
        with Cluster("aaw-kubeflow-profiles-controller repo"):
            github_aaw_kubeflow_profiles_controller_build_action = Custom(
                "github build action", icon_path="icons/github.png"
            )
            aaw_kubeflow_profiles_controller_image = Custom(
                "profiles-controller image", icon_path="icons/docker.png"
            )

    with Cluster("Kubernetes Cluster (aaw-dec-cc-00-aks)"):
        with Cluster("daaas-system namespace"):
            # aaw-daaas-system ArgoCD Application
            daaas_system_argocd = Custom("aaw-daaas-system", icon_path="icons/argo.png")
            # profiles-controller ArgoCD Application
            profiles_controller_argocd = Custom(
                "profiles-controller", icon_path="icons/argo.png"
            )
            auth_policies_deployment = Deployment("profiles-controller-authpolicies")
            buckets_deployment = Deployment("profiles-controller-buckets")
            gitea_deployment = Deployment("profiles-controller-gitea")
            limitranges_deployment = Deployment("profiles-controller-limitranges")
            network_deployment = Deployment("profiles-controller-network")
            notebook_deployment = Deployment("profiles-controller-notebook")
            quotas_deployment = Deployment("profiles-controller-quotas")
            rbac_deployment = Deployment("profiles-controller-rbac")

    # argocd_operator.tf bootstraps root daaas-system-argocd application (raw kubernetes
    # manifest for ArgoCD application is included in terraform code)
    gitlab_tf >> Edge(color="red") >> daaas_system_argocd
    # aaw-daaas-system controller watches the /daaas-system/profilecontrollers/profiles/controller/application.jsonnet
    # file in the aaw-argocd-manifests repo on the `aaw-dev-cc-00` target revision (branch), which deploys the
    # profiles-controller ArgoCD application.
    daaas_system_argocd >> Edge(style="dotted", color="red") >> github_daaas_system
    github_daaas_system >> Edge(
        style="dotted", color="red"
    ) >> profiles_controller_argocd
    # The profiles-controller ArgoCD application watches the /stable/profiles-controller chart in the StatCan
    # charts repository on a fixed target revision, and passes the image SHA of the profiles-controller image build
    # to the values.yaml file of the profiles-controller chart.
    daaas_system_argocd >> Edge(color="red") >> profiles_controller_argocd
    profiles_controller_argocd >> Edge(
        style="dotted", color="blue"
    ) >> github_profiles_controller_chart
    # The Github build action in the aaw-kubeflow-profiles-controller repo builds a container image that contains the
    # binary to run each "sub-controller" in the repo. This image is tagged with the commit SHA of the commit that
    # is merged into the main branch of the repo, and then pushed to Azure container registry. This is the image from
    # which each controller deployment is created. Each controller has the same base image, but differs on the
    # entrypoint. E.g. the Gitea controller runs with the entrypoint `./profiles-controller gitea` while the network
    # controller runs with the entrypoint `./profiles-controller network`.
    github_aaw_kubeflow_profiles_controller_build_action >> Edge(
        color="green"
    ) >> aaw_kubeflow_profiles_controller_image
    aaw_kubeflow_profiles_controller_image >> Edge(color="green") >> azure_cr

    azure_cr >> Edge(style="dotted", color="green") >> auth_policies_deployment
    azure_cr >> Edge(style="dotted", color="green") >> buckets_deployment
    azure_cr >> Edge(style="dotted", color="green") >> gitea_deployment
    azure_cr >> Edge(style="dotted", color="green") >> limitranges_deployment
    azure_cr >> Edge(style="dotted", color="green") >> network_deployment
    azure_cr >> Edge(style="dotted", color="green") >> notebook_deployment
    azure_cr >> Edge(style="dotted", color="green") >> quotas_deployment
    azure_cr >> Edge(style="dotted", color="green") >> rbac_deployment

    # The StatCan helm chart specifies how to deploy each "sub-controller"
    github_profiles_controller_chart >> Edge(
        style="dotted", color="blue"
    ) >> auth_policies_deployment
    github_profiles_controller_chart >> Edge(
        style="dotted", color="blue"
    ) >> buckets_deployment
    github_profiles_controller_chart >> Edge(
        style="dotted", color="blue"
    ) >> gitea_deployment
    github_profiles_controller_chart >> Edge(
        style="dotted", color="blue"
    ) >> limitranges_deployment
    github_profiles_controller_chart >> Edge(
        style="dotted", color="blue"
    ) >> network_deployment
    github_profiles_controller_chart >> Edge(
        style="dotted", color="blue"
    ) >> notebook_deployment
    github_profiles_controller_chart >> Edge(
        style="dotted", color="blue"
    ) >> quotas_deployment
    github_profiles_controller_chart >> Edge(
        style="dotted", color="blue"
    ) >> rbac_deployment

    # The profiles-controller ArgoCD application is actually responsible for watching and creating each
    # "sub-controller" deployment
    profiles_controller_argocd >> Edge(color="blue") >> auth_policies_deployment
    profiles_controller_argocd >> Edge(color="blue") >> buckets_deployment
    profiles_controller_argocd >> Edge(color="blue") >> gitea_deployment
    profiles_controller_argocd >> Edge(color="blue") >> limitranges_deployment
    profiles_controller_argocd >> Edge(color="blue") >> network_deployment
    profiles_controller_argocd >> Edge(color="blue") >> notebook_deployment
    profiles_controller_argocd >> Edge(color="blue") >> quotas_deployment
    profiles_controller_argocd >> Edge(color="blue") >> rbac_deployment
