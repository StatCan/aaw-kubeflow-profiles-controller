package cmd

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"

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

type KerberosConfig struct {
	NetPolIPs      []string
	SidecarConfigs string
}

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

		kerberosConfig, err := getKerberosConfigmap(kubeClient)
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
				err := createKerberosUserConfigMap(secret.Namespace, kubeClient, kerberosConfig.SidecarConfigs)
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

func getKerberosConfigmap(client *kubernetes.Clientset) (KerberosConfig, error) {
	klog.Infof("Getting Kerberos controller configs")

	configmap, err := client.CoreV1().ConfigMaps("das").Get(context.Background(), "kerberos-config", metav1.GetOptions{})
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

func generateKerberosConfigMap(namespace string, sidecarConfigs string) corev1.ConfigMap {
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

func createKerberosUserConfigMap(namespace string, kubeClient *kubernetes.Clientset, sidecarConfigs string) error {
	// generate the configmap
	masterCM := generateKerberosConfigMap(namespace, sidecarConfigs)

	// find the kerberos configmap for the given namespace
	userCM, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(context.Background(), masterCM.Name, metav1.GetOptions{})

	// if CM is not found, create it in the namespace
	if errors.IsNotFound(err) {
		klog.Infof("creating config map %s/%s", masterCM.Namespace, masterCM.Name)
		_, err = kubeClient.CoreV1().ConfigMaps(namespace).Create(context.Background(), &masterCM, metav1.CreateOptions{})
		if err != nil {
			return err
		}

		return nil
	} else if err != nil {
		return err
	}

	// if CM is found, but not equal to the master CM, update the CM
	if !reflect.DeepEqual(masterCM.Data, userCM.Data) {
		klog.Infof("updating config map %s/%s", masterCM.Namespace, masterCM.Name)
		userCM.Data = masterCM.Data

		_, err = kubeClient.CoreV1().ConfigMaps(namespace).Update(context.Background(), userCM, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func generateKerberosNetworkPolicy(namespace string, netpolCIDRList []string) networkingv1.NetworkPolicy {
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

func createKerberosNetworkPolicy(namespace string, kubeClient *kubernetes.Clientset, ipList []string) error {
	// generate the policy for egress to kerberos
	policy := generateKerberosNetworkPolicy(namespace, ipList)

	// find the egress kerberos policy for the given namespace
	currentPolicy, err := kubeClient.NetworkingV1().NetworkPolicies(namespace).Get(context.Background(), policy.Name, metav1.GetOptions{})

	// if policy is not found, create it in the namespace
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

	// if policy is found, but not equal to the generated policy, update the policy
	if !reflect.DeepEqual(policy.Spec, currentPolicy.Spec) {
		klog.Infof("updating network policy %s/%s", policy.Namespace, policy.Name)
		currentPolicy.Spec = policy.Spec

		_, err = kubeClient.NetworkingV1().NetworkPolicies(policy.Namespace).Update(context.Background(), currentPolicy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func init() {
	rootCmd.AddCommand(kerberosCmd)
}
