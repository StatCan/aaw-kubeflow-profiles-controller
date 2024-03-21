package cmd

import (
	"context"
	"fmt"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var artifactorycredinjectorCmd = &cobra.Command{
	Use:   "artifactory-cred-injector",
	Short: "Configure artifactory-cred-injector credentials",
	Long:  `Configure artifactory-cred-injector credentials`,
	Run: func(cmd *cobra.Command, args []string) {

		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		// Builds k8s client for us to use, pass this in to functions for us to use
		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Kubeflow client: %v", err)
		}

		namespace := "das"
		secretName := "artifactory-creds"

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		secretInformer := kubeInformerFactory.Core().V1().Secrets()
		secretLister := secretInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {

				// Check if the secret exists in the namespace
				_, err := kubeClient.CoreV1().Secrets(namespace).Get(context.TODO(), secretName, metav1.GetOptions{})
				if err == nil {
					fmt.Printf("Secret %s already exists in namespace %s\n", secretName, namespace)
					return
				} else if !errors.IsNotFound(err) {
					fmt.Printf("Error getting secret %s in namespace %s: %v\n", secretName, namespace, err)
				}

				copy := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      secretName,
						Namespace: namespace,
					},
					Username: secret.Username,
					Token:    secret.Tokenm,
				}

				_, err := kubeClient.CoreV1().Secrets(ns.Name).Create(context.TODO(), copy, metav1.CreateOptions{})
				if err != nil {
					fmt.Printf("Error creating secret in namespace %s: %v\n", profile.Name, err)
				} else {
					fmt.Printf("Created secret in namespace %s\n", profile.Name)
				}
			},
		)
	},
}

func init() {
	rootCmd.AddCommand(artifactorycredinjectorCmd)
}
