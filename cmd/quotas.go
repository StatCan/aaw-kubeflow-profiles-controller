package cmd

import (
	"context"
	"strings"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var quotasCmd = &cobra.Command{
	Use:   "quotas",
	Short: "Configure quotas resources",
	Long: `Configure quota resources for Kubeflow profiles.
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Kubeflow client: %v", err)
		}

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		resourceQuotaInformer := kubeInformerFactory.Core().V1().ResourceQuotas()
		resourceQuotaLister := resourceQuotaInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Generate quotas
				resourceQuotas := generateResourceQuotas(profile)

				// Delete resources no longer needed
				for _, resourceQuotaName := range []string{} {
					_, err := resourceQuotaLister.ResourceQuotas(profile.Name).Get(resourceQuotaName)
					if err == nil {
						klog.Infof("removing resource quota %s/%s", profile.Name, resourceQuotaName)
						err = kubeClient.CoreV1().ResourceQuotas(profile.Name).Delete(context.Background(), resourceQuotaName, metav1.DeleteOptions{})
						if err != nil {
							return err
						}
					}
				}

				// Create
				for _, resourceQuota := range resourceQuotas {
					currentResourceQuota, err := resourceQuotaLister.ResourceQuotas(resourceQuota.Namespace).Get(resourceQuota.Name)
					if errors.IsNotFound(err) {
						klog.Infof("creating resource quota %s/%s", resourceQuota.Namespace, resourceQuota.Name)
						currentResourceQuota, err = kubeClient.CoreV1().ResourceQuotas(resourceQuota.Namespace).Create(context.Background(), resourceQuota, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}

					if !equality.Semantic.DeepDerivative(resourceQuota.Spec, currentResourceQuota.Spec) {
						klog.Infof("updating resource quota %s/%s", resourceQuota.Namespace, resourceQuota.Name)
						currentResourceQuota.Spec = resourceQuota.Spec

						_, err = kubeClient.CoreV1().ResourceQuotas(resourceQuota.Namespace).Update(context.Background(), currentResourceQuota, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				return nil
			},
		)

		resourceQuotaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*corev1.ResourceQuota)
				oldNP := old.(*corev1.ResourceQuota)

				if newNP.ResourceVersion == oldNP.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		// Start informers
		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		// Wait for caches
		klog.Info("Waiting for informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, resourceQuotaInformer.Informer().GetController().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

var defaultResources = corev1.ResourceList{
	// CPU
	"requests.cpu": resource.MustParse("70"),
	"limits.cpu":   resource.MustParse("70"),

	// Memory
	"requests.memory": resource.MustParse("368G"),
	"limits.memory":   resource.MustParse("368G"),

	// Storage
	"requests.storage": resource.MustParse("4T"),

	// GPU
	"requests.nvidia.com/gpu": resource.MustParse("2"),

	// Pods
	"pods": resource.MustParse("100"),

	// Services
	"services.nodeports":     resource.MustParse("0"),
	"services.loadbalancers": resource.MustParse("0"),
}

var quotaLabels = [9]string{
	"quotas.statcan.gc.ca/requests.cpu", "quotas.statcan.gc.ca/limits.cpu",
	"quotas.statcan.gc.ca/requests.memory", "quotas.statcan.gc.ca/limits.memory",
	"quotas.statcan.gc.ca/requests.storage", "quotas.statcan.gc.ca/gpu",
	"quotas.statcan.gc.ca/pods", "quotas.statcan.gc.ca/services.nodeports",
	"quotas.statcan.gc.ca/services.loadbalancers",
}

var quotaPrefixLabel = "quotas.statcan.gc.ca/"

func hasQuotaLabel(profileLabel string, key string, profile *kubeflowv1.Profile) bool {
	if _, ok := profile.Labels[profileLabel]; ok {
		if key == "requests.nvidia.com/gpu" {
			return true
		} else if strings.HasPrefix(profileLabel, quotaPrefixLabel) {
			s := strings.TrimPrefix(profileLabel, quotaPrefixLabel)
			return s == key
		}
	}
	return false
}

func overrideResourceQuotas(profile *kubeflowv1.Profile) {

	for key := range defaultResources {
		// CPU
		if hasQuotaLabel(quotaLabels[0], key.String(), profile) {
			cpuRequest := profile.Labels[quotaLabels[0]]
			defaultResources[key] = resource.MustParse(cpuRequest)
		}
		if hasQuotaLabel(quotaLabels[1], key.String(), profile) {
			cpuLimit := profile.Labels[quotaLabels[1]]
			defaultResources[key] = resource.MustParse(cpuLimit)
		}
		// Memory
		if hasQuotaLabel(quotaLabels[2], key.String(), profile) {
			memoryRequest := profile.Labels[quotaLabels[2]]
			defaultResources[key] = resource.MustParse(memoryRequest)
		}
		if hasQuotaLabel(quotaLabels[3], key.String(), profile) {
			memoryLimit := profile.Labels[quotaLabels[3]]
			defaultResources[key] = resource.MustParse(memoryLimit)
		}
		// Storage
		if hasQuotaLabel(quotaLabels[4], key.String(), profile) {
			storageRequest := profile.Labels[quotaLabels[4]]
			defaultResources[key] = resource.MustParse(storageRequest)
		}
		// GPU
		if hasQuotaLabel(quotaLabels[5], key.String(), profile) {
			gpuRequest := profile.Labels[quotaLabels[5]]
			defaultResources[key] = resource.MustParse(gpuRequest)
		}
		// Pods
		if hasQuotaLabel(quotaLabels[6], key.String(), profile) {
			pods := profile.Labels[quotaLabels[6]]
			defaultResources[key] = resource.MustParse(pods)
		}
		// Services
		if hasQuotaLabel(quotaLabels[7], key.String(), profile) {
			nodeports := profile.Labels[quotaLabels[7]]
			defaultResources[key] = resource.MustParse(nodeports)
		}
		if hasQuotaLabel(quotaLabels[8], key.String(), profile) {
			loadbalancers := profile.Labels[quotaLabels[8]]
			defaultResources[key] = resource.MustParse(loadbalancers)
		}
	}
}

// generateResourceQuotas generates resource quotas for the given profile.
func generateResourceQuotas(profile *kubeflowv1.Profile) []*corev1.ResourceQuota {

	overrideResourceQuotas(profile)
	quotas := []*corev1.ResourceQuota{}
	quotas = append(quotas, &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "quotas",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: defaultResources,
		},
	})

	return quotas
}

func init() {
	rootCmd.AddCommand(quotasCmd)
}
