package cmd

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"

	utils "github.com/StatCan/profiles-controller/util"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	toolsWatch "k8s.io/client-go/tools/watch"
	"k8s.io/klog"
)

// KerberosConfig represents the data found in the kerberos-config configmap
type KerberosConfig struct {
	NetPolIPs      []string // list of CIDR blocks for NetworkPolicy
	SidecarConfigs string   // string block for configs to add to ConfigMap
}

// The kerberos controller creates the resources necessary
// for the execution of the kerberos sidecar container in desired namespaces.
// It watches for the kerberos-keytab named secret and on its creation or modification,
// the controller will create a NetworkPolicy and a ConfigMap
//
//	in the same namespace as the watched secret.
var kerberosCmd = &cobra.Command{
	Use:   "kerberos",
	Short: "Configure kerberos sidecar resources",
	Long:  "Configure kerberos sidecar resources",
	Run: func(cmd *cobra.Command, args []string) {
		var wg sync.WaitGroup
		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}

		// gets the configs for this controller
		kerberosConfig, err := getKerberosConfigs(kubeClient)
		if err != nil {
			klog.Fatalf("Error getting configmap: %s", err.Error())
		}

		watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
			timeOut := int64(60)
			// Watches all namespaces, hence the Secrets("")
			// Watches for all secrets named "kerberos-keytab"
			return kubeClient.CoreV1().Secrets("").Watch(context.Background(), metav1.ListOptions{TimeoutSeconds: &timeOut,
				FieldSelector: "metadata.name=kerberos-keytab"})
		}
		watcher, _ := toolsWatch.NewRetryWatcher("1", &cache.ListWatch{WatchFunc: watchFunc})
		for event := range watcher.ResultChan() {
			secret := event.Object.(*corev1.Secret)
			switch event.Type {
			case watch.Modified, watch.Added:
				err := createKerberosSidecarConfigMap(secret.Namespace, kubeClient, kerberosConfig.SidecarConfigs)
				if err != nil {
					klog.Errorf("Error occurred while creating the ConfigMap for namespace %s: %s", secret.Namespace, err.Error())
				}

				err = createKerberosNetworkPolicy(secret.Namespace, kubeClient, kerberosConfig.NetPolIPs)
				if err != nil {
					klog.Errorf("Error occurred while creating the NetworkPolicy for namespace %s: %s", secret.Namespace, err.Error())
				}
			case watch.Error:
				klog.Errorf("Kerberos secret in namespace %s contains an error.", secret.Namespace)
			}
		}

		wg.Add(1)
		wg.Wait()
	},
}

// getKerberosConfigs returns a KerberosConfig which contains
// the configurations needed for the proper execution of the controller.
// NetPolIPs contains the list of CIDR values to include in the NetworkPolicy created by this controller.
// SidecarConfigs contains the value used in the ConfigMap created by this controller.
// Returns a nil error on success.
// Will return an error on failure to retrieve the source ConfigMap, or on failure to process the ConfigMap data.
func getKerberosConfigs(client *kubernetes.Clientset) (KerberosConfig, error) {
	klog.Infof("Getting Kerberos controller configs")

	configmap, err := client.CoreV1().ConfigMaps(utils.PodNamespace()).Get(context.Background(), "kerberos-config", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error occured while getting the kerberos configmap: %v", err)
		return KerberosConfig{}, err
	}

	netpolIPs := &[]string{}
	err = json.Unmarshal([]byte(configmap.Data["ipList"]), netpolIPs)
	if err != nil {
		klog.Errorf("error occured while unmarshalling the kerberos configmap: %v", err)
		return KerberosConfig{}, err
	}

	config := KerberosConfig{
		NetPolIPs:      *netpolIPs,
		SidecarConfigs: configmap.Data["sidecarConfigs"],
	}
	return config, nil
}

// generateKerberosSidecarConfigMap returns a corev1.ConfigMap object
// for the given namespace, which will contain the given sidecarConfigs as its only data.
func generateKerberosSidecarConfigMap(namespace string, sidecarConfigs string) corev1.ConfigMap {
	configmap := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kerberos-sidecar-config",
			Namespace: namespace,
		},
		Data: map[string]string{
			"krb5.conf": sidecarConfigs,
		},
	}

	return configmap
}

// createKerberosSidecarConfigMap creates a ConfigMap resource in the given namespace , with the given sidecarConfigs,
// which will be mounted to any Kerberos sidecar container in the namespace by the Kerberos sidecar injector.
//
// A successful creation of this ConfigMap will return nil.
// An Error will be returned if the creation of this ConfigMap fails.
func createKerberosSidecarConfigMap(namespace string, kubeClient *kubernetes.Clientset, sidecarConfigs string) error {
	// generate the configmap to be applied in the namespace
	configMap := generateKerberosSidecarConfigMap(namespace, sidecarConfigs)

	// find the kerberos sidecar configmap for the given namespace
	existingCM, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(context.Background(), configMap.Name, metav1.GetOptions{})

	// if the configmap is not found in the namespace, create it
	if errors.IsNotFound(err) {
		klog.Infof("creating config map %s/%s", configMap.Namespace, configMap.Name)
		_, err = kubeClient.CoreV1().ConfigMaps(namespace).Create(context.Background(), &configMap, metav1.CreateOptions{})
		if err != nil {
			return err
		}

		return nil
	} else if err != nil {
		return err
	}

	// if the configmap is found in the namespace,
	// but the data does not equal the configmap to be applied,
	// then reconcile the configmap in the namespace
	if !reflect.DeepEqual(configMap.Data, existingCM.Data) {
		klog.Infof("updating config map %s/%s", configMap.Namespace, configMap.Name)
		existingCM.Data = configMap.Data

		_, err = kubeClient.CoreV1().ConfigMaps(namespace).Update(context.Background(), existingCM, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

// generateKerberosSidecarNetworkPolicy returns an networkingv1.NetworkPolicy
// for the given namespace, which will contain the given netpolCIDRList as its Egress rules.
func generateKerberosSidecarNetworkPolicy(namespace string, netpolCIDRList []string) networkingv1.NetworkPolicy {
	portKDC := intstr.FromInt(88)
	protocolTCP := corev1.ProtocolTCP

	// generate the object list of CIDR blocks for the netpol
	policyPeerList := []networkingv1.NetworkPolicyPeer{}
	for _, val := range netpolCIDRList {
		policyPeerList = append(policyPeerList,
			networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{
					CIDR: val,
				},
			},
		)
	}

	policy := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-egress-to-kerberos-kdc",
			Namespace: namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "notebook-name",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &protocolTCP,
							Port:     &portKDC,
						},
					},
					To: policyPeerList,
				},
			},
		},
	}

	return policy
}

// createKerberosNetworkPolicy creates a NetworkPolicy resource in the given namespace.
// This will be an Egress NetworkPolicy to allow the connection from a Kerberos sidecar container
// to the Kerberos KDC. This NetworkPolicy will contain the given ipList for its Egress rules.
//
// A successful creation of this NetworkPolicy will return nil.
// An Error will be returned if the creation of this NetworkPolicy fails.
func createKerberosNetworkPolicy(namespace string, kubeClient *kubernetes.Clientset, ipList []string) error {
	// generate the policy for egress to kerberos
	policy := generateKerberosSidecarNetworkPolicy(namespace, ipList)

	// find the egress kerberos policy for the given namespace
	existingPolicy, err := kubeClient.NetworkingV1().NetworkPolicies(namespace).Get(context.Background(), policy.Name, metav1.GetOptions{})

	// if the policy is not found, create it in the namespace
	if errors.IsNotFound(err) {
		klog.Infof("creating network policy %s/%s", policy.Namespace, policy.Name)
		_, err = kubeClient.NetworkingV1().NetworkPolicies(policy.Namespace).Create(context.Background(), &policy, metav1.CreateOptions{})
		if err != nil {
			return err
		}

		return nil
	} else if err != nil {
		return err
	}

	// if policy is found, but not equal to the desired policy, update the existing policy
	if !reflect.DeepEqual(policy.Spec, existingPolicy.Spec) {
		klog.Infof("updating network policy %s/%s", policy.Namespace, policy.Name)
		existingPolicy.Spec = policy.Spec

		_, err = kubeClient.NetworkingV1().NetworkPolicies(policy.Namespace).Update(context.Background(), existingPolicy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func init() {
	rootCmd.AddCommand(kerberosCmd)
}
