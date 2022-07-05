import os

from diagrams import Cluster, Diagram, Edge
from diagrams.azure.database import DatabaseForPostgresqlServers
from diagrams.custom import Custom
from diagrams.k8s.compute import Deployment, Pod, StatefulSet
from diagrams.k8s.network import NetworkPolicy, Service
from diagrams.azure.network import DNSPrivateZones
from diagrams.azure.network import VirtualNetworks
from diagrams.k8s.podconfig import Secret
from diagrams.k8s.storage import PersistentVolume, PersistentVolumeClaim
from diagrams.azure.storage import BlobStorage

def myself() -> str:
    f = os.path.basename(__file__)
    no_ext = ".".join(f.split(".")[:-1])
    return no_ext

with Diagram(myself(), show=False):
    with Cluster("aaw-argocd-manifests repo"):
        # blob-csi controller deployment
        with Cluster("/daaas-system/profile-controllers/profiles-controller"):
            argman_profiles_controller = Custom("profiles-controller", icon_path="icons/github.png")
        # blob-csi driver deployment
        with Cluster("/daaas-system/blob-csi-driver"):
            argman_blob_csi_driver = Custom("blob-csi-driver", icon_path="icons/github.png") 

        # blob-csi injector
        # with Cluster("/daaas-system/blob-csi-injector"):
            # argman_blob_csi_injector = Custom("blob-csi-injector", icon_path="icons/github.png") 

    # Terraform declarations
    with Cluster("gitlab.k8s"):
        # https://gitlab.k8s.cloud.statcan.ca/cloudnative/aaw/terraform-advanced-analytics-workspaces-infrastructure/-/tree/main/
        with Cluster("Advanced Analytics Workspace Infrastructure"):
            with Cluster("dns.tf"):
                # private dns zone, link and record     
                tf_private_dns = Custom("azurerm_private_dns_zone", icon_path="icons/terraform.png") 
                tf_vnet_link = Custom("azurerm_private_dns_zone_virtual_network_link", icon_path="icons/terraform.png") 
                tf_dns_record = Custom("azurerm_private_dns_a_record", icon_path="icons/terraform.png") 
            with Cluster("dev_cc_00.tf"):
                # firewall route and rule
                tf_aaw_to_fdi_protb = Custom("azurerm_route", icon_path="icons/terraform.png")
                with Cluster("azurerm_firewall_policy_rule_collection_group"):
                    tf_aaw_fdi_protb_firewall_rule = Custom("rule", icon_path="icons/terraform.png")

        # https://gitlab.k8s.cloud.statcan.ca/cloudnative/aaw/daaas-infrastructure/aaw-dev-cc-00
        with Cluster("aaw-dev-cc-00 repo"):
            with Cluster("azure-blob-csi-system.tf"):
                tf_aaw_premium_secret = Custom("kubernetes_secret", icon_path="icons/terraform.png")
                tf_aaw_protb_secret = Custom("kubernetes_secret", icon_path="icons/terraform.png")
                tf_aaw_standard_secret = Custom("kubernetes_secret", icon_path="icons/terraform.png")
                tf_azure_blob_prot_b_secret = Custom("kubernetes_secret", icon_path="icons/terraform.png")
                tf_azure_blob_prot_b_spn_secret = Custom("kubernetes_secret", icon_path="icons/terraform.png")
                tf_azure_blob_unclass_secret = Custom("kubernetes_secret", icon_path="icons/terraform.png")
                tf_azure_blob_unclass_spn_secret = Custom("kubernetes_secret", icon_path="icons/terraform.png")
            with Cluster("fdi_gateway.tf"):
                tf_opa_gateway_unclassified = Custom("fdi_minio_gateway_unclassified", icon_path="icons/terraform.png")
                tf_opa_gateway_protb = Custom("fdi_minio_gateway_protb", icon_path="icons/terraform.png")

    with Cluster("github.com"):
        # https://github.com/StatCan/aaw-network-policies
        with Cluster("aaw-network-policies repo"):
            with Cluster("azure-blob-csi-system.yaml"):
                allow_blob_csi_to_internet = NetworkPolicy("allow-azure-blob-csi-to-internet")
                allow_egress_pcont_opa_protb = NetworkPolicy("allow-egress-profiles-controller-to-fdi-opa-gateway-protected-b")
                allow_egress_pcont_opa_unclass = NetworkPolicy("allow-egress-profiles-controller-to-fdi-opa-gateway-unclassified")
                allow_ingress_pcont_opa_protb = NetworkPolicy("allow-ingress-profiles-controller-to-fdi-opa-gateway-protected-b")
                allow_ingress_pcont_opa_unclass = NetworkPolicy("allow-ingress-profiles-controller-to-fdi-opa-gateway-unclassified")

    with Cluster("Kubernetes Cluster"):
        # PV
        aaw_pv_user_standard = PersistentVolume("user-standard")
        aaw_pv_user_standard_ro = PersistentVolume("user-standard-ro")
        aaw_pv_user_premium = PersistentVolume("user-premium")
        aaw_pv_user_premium_ro = PersistentVolume("user-premium-ro")
        aaw_pv_user_protected_b = PersistentVolume("user-protected-b")
        fdi_pv_user = PersistentVolume("fdi-user-pv")
        with Cluster("daaas-system"):
            # profiles controller
            profiles_controller = Pod("blob-csi.go")
            # blob_csi_injector = Pod("blob-csi-injector")

        with Cluster("user namespace"):
            # PVC
            aaw_pvc_user = PersistentVolumeClaim("aaw-user-pvc")
            fdi_pvc_user = PersistentVolumeClaim("fdi-user-pvc")
        with Cluster("fdi-gateway-unclassified-system"):
            # minio-gateway-opa pod (opa gateway that controller queries)
            minio_gateway_opa_unclassified = Pod("minio-gateway-opa")
            bundle_secret_unclassified     = Secret("bundle-secret")
        with Cluster("fdi-gateway-protected-b-system"):
            # minio-gateway-opa pod (opa gateway that controller queries)
            minio_gateway_opa_protb = Pod("minio-gateway-opa")
            bundle_secret_protb     = Secret("bundle-secret")
        with Cluster("azure-blob-csi-system"):
            # pods
            # blob-csi-controller
            blob_csi_controller = Pod("blob-csi-controller")
            # secrets
            aaw_premium_secret = Secret("aawdevcc00samgpremium")
            aaw_protb_secret = Secret("aawdevcc00samgprotb")
            aaw_standard_secret = Secret("aawdevccsamgstandard")
            azure_blob_prot_b_secret = Secret("azure-blob-csi-fdi-protected-b")
            azure_blob_prot_b_spn_secret = Secret("azure-blob-csi-fdi-protected-b-spn")
            azure_blob_unclass_secret = Secret("azure-blob-csi-fdi-unclassified")
            azure_blob_unclass_spn_secret = Secret("azure-blob-csi-fdi-unclassified-spn")

    with Cluster("Azure"):
        with Cluster("FDI-DEV-RESEARCH"):
            with Cluster("fdiunclassdev"):
                # unclass fdi containers
                fdi_unclass_blob = BlobStorage("fdi-user-unclassified-container")
            with Cluster("fdiprotbdev"):
                # prot-b fdi containers
                fdi_protb_blob = BlobStorage("fdi-user-protb-container")
            with Cluster("fdiminioextmeta"):
                #user-protb-containeo internal and external-opa containers store opa bundles
                fdi_protb_bundle = BlobStorage("fdi-protb-bundle")
                fdi_unclass_bundle = BlobStorage("fdi-unclass-bundle")
        # User containers are created in each storage account for AAW.
        with Cluster("aawdevcc00samgpremium"):
            azure_premium_ro = BlobStorage("user-premium-ro")
            azure_premium = BlobStorage("user-premium")
        with Cluster("aawdevcc00samgstandard"):
            azure_standard_ro = BlobStorage("user-standard-ro")
            azure_standard = BlobStorage("user-standard")
        with Cluster("aawdevcc00samgprotb"):
            azure_protcted_b = BlobStorage("user-protcted-b")