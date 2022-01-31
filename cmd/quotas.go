package cmd

import (
	"context"
	"strings"
	"time"

	"strconv"

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
	"requests.cpu": *resource.NewQuantity(70, resource.DecimalSI),
	"limits.cpu":   *resource.NewQuantity(70, resource.DecimalSI),

	// Memory
	"requests.memory": *resource.NewScaledQuantity(368, resource.Giga),
	"limits.memory":   *resource.NewScaledQuantity(368, resource.Giga),

	// Storage
	"requests.storage": *resource.NewScaledQuantity(4, resource.Tera),

	// GPU
	"requests.nvidia.com/gpu": *resource.NewQuantity(2, resource.DecimalSI),

	// Pods
	"pods": *resource.NewQuantity(100, resource.DecimalSI),

	// Services
	"services.nodeports":     *resource.NewQuantity(0, resource.DecimalSI),
	"services.loadbalancers": *resource.NewQuantity(0, resource.DecimalSI),
}

func overrideResourceQuotas(profile *kubeflowv1.Profile) {

	// if we iterate over defaultResources, won't be able to find the key since default key is requests.storage and profileLabels key: quotas.statcan.gc.ca/requests.storage
	// used strings.Contains()
	// Also defaultResources has different resource dataTypes (for ex. resource.Tera, resource.Giga etc.) ; need checks on which resource quota you're overriding

	for key, _ := range defaultResources { // or can iterate over profile.labels to avoid hard code of profiles label
		// CPU
		if strings.Contains(profile.Labels["quotas.statcan.gc.ca/requests.cpu"], key.String()) {
			cpuRequest, _ := strconv.Atoi(profile.Labels["quotas.statcan.gc.ca/requests.cpu"])
			defaultResources[key] = *resource.NewQuantity(int64(cpuRequest), resource.DecimalSI)
		}
		if strings.Contains(profile.Labels["quotas.statcan.gc.ca/limits.cpu"], key.String()) {
			cpuLimit, _ := strconv.Atoi(profile.Labels["quotas.statcan.gc.ca/limits.cpu"])
			defaultResources[key] = *resource.NewQuantity(int64(cpuLimit), resource.DecimalSI)
		}
		// Memory
		if strings.Contains(profile.Labels["quotas.statcan.gc.ca/requests.memory"], key.String()) {
			memoryRequest, _ := strconv.Atoi(profile.Labels["quotas.statcan.gc.ca/requests.memory"])
			defaultResources[key] = *resource.NewScaledQuantity(int64(memoryRequest), resource.Giga)
		}
		if strings.Contains(profile.Labels["quotas.statcan.gc.ca/limits.memory"], key.String()) {
			memoryLimit, _ := strconv.Atoi(profile.Labels["quotas.statcan.gc.ca/limits.memory"])
			defaultResources[key] = *resource.NewScaledQuantity(int64(memoryLimit), resource.Giga)
		}
		// Storage
		if strings.Contains(profile.Labels["quotas.statcan.gc.ca/requests.storage"], key.String()) {
			storageRequest, _ := strconv.Atoi(profile.Labels["quotas.statcan.gc.ca/requests.storage"])
			defaultResources[key] = *resource.NewScaledQuantity(int64(storageRequest), resource.Tera)
		}
		// Pods
		if strings.Contains(profile.Labels["quotas.statcan.gc.ca/pods"], key.String()) {
			pods, _ := strconv.Atoi(profile.Labels["quotas.statcan.gc.ca/pods"])
			defaultResources[key] = *resource.NewQuantity(int64(pods), resource.DecimalSI)
		}
		// Services
		if strings.Contains(profile.Labels["quotas.statcan.gc.ca/services.nodeports"], key.String()) {
			servicesNodeports, _ := strconv.Atoi(profile.Labels["quotas.statcan.gc.ca/services.nodeports"])
			defaultResources[key] = *resource.NewQuantity(int64(servicesNodeports), resource.DecimalSI)
		}
		if strings.Contains(profile.Labels["quotas.statcan.gc.ca/services.loadbalancers"], key.String()) {
			servicesLoadbalancers, _ := strconv.Atoi(profile.Labels["quotas.statcan.gc.ca/services.loadbalancers"])
			defaultResources[key] = *resource.NewQuantity(int64(servicesLoadbalancers), resource.DecimalSI)
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
