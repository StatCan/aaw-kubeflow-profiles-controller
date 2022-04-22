from diagrams import Cluster
from diagrams import Diagram
from diagrams import Edge

from diagrams.azure.database import DatabaseForPostgresqlServers

from diagrams.k8s.compute import Pod
from diagrams.k8s.compute import StatefulSet
from diagrams.k8s.compute import Deployment

from diagrams.k8s.network import NetworkPolicy
from diagrams.k8s.network import Service

from diagrams.custom import Custom

with Diagram("Gitea Controller", show=False):
    with Cluster("Azure"):
        postgres = DatabaseForPostgresqlServers("Azure Managed Postgres")
    with Cluster("Kubernetes Cluster"):
        with Cluster("profiles-argocd-system"):
            # TODO: update this to reflect current setup
            argo_cd = Deployment("argo-cd")
        with Cluster("daaas-system"):
            gitea_controller = Pod("profiles-controller-gitea")
            network_controller = Pod("profiles-controller-network")
        with Cluster("user-namespace"):
            gitea_ss = StatefulSet("gitea-0")
            gitea_pod = Pod("gitea-0")
            gitea_postgres = StatefulSet("gitea-postgres-0")
            gitea_http_service = Service("gitea-http")
            istio_vs = Custom("gitea-virtualservice", icon_path="icons/istio.png")
            network_policy = NetworkPolicy("gitea-ingress")
        with Cluster("kubeflow"):
            kubeflow_gateway = Custom("kubeflow-gateway", icon_path="icons/istio.png")

        # Gitea Statefulset manages a Gitea pod
        gitea_ss >> Edge(style="dotted", color="blue") >> gitea_pod

        # Network policy permits kubeflow gateway pods to communicate with pods
        # exposed by the gitea-http service.
        kubeflow_gateway - Edge(style="dotted") - network_policy - Edge(
            style="dotted"
        ) - gitea_http_service

        # Gitea controller creates various components in user's namespace
        gitea_controller >> Edge(style="dotted", color="blue") >> gitea_ss
        gitea_controller >> Edge(style="dotted", color="blue") >> gitea_postgres
        gitea_controller >> Edge(style="dotted", color="blue") >> gitea_http_service
        gitea_controller >> Edge(style="dotted", color="blue") >> istio_vs

        # Network policy controller creates the network policy that allows kubeflow
        # gateway traffic to be sent to gitea pods.
        gitea_controller >> Edge(style="dotted", color="blue") >> network_policy

        # Network traffic enters through the kubeflow gateway and is routed to the
        # gitea-http service by the Istio VirtualService.
        kubeflow_gateway >> Edge(color="red") >> istio_vs >> Edge(
            color="red"
        ) >> gitea_http_service >> Edge(color="red") >> gitea_pod

        # TODO: Update this with plans for managed postgres
        gitea_ss >> postgres
