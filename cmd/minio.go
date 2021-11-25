package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	vault "github.com/hashicorp/vault/api"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

func logWarnings(warnings []string) {
	if warnings != nil && len(warnings) > 0 {
		for _, warning := range warnings {
			klog.Warning(warning)
		}
	}
}

// Conf for MinIO
type Conf struct {
	AccessKeyID     string
	Endpoint        string
	SecretAccessKey string
	UseSSL          bool
}

func getMinIOConfig(vc *vault.Client, instance string) (*Conf, error) {

	data, err := vc.Logical().Read(path.Join(instance, "config"))
	if data != nil {
		logWarnings(data.Warnings)
	}
	if err != nil {
		return nil, err
	}

	config := Conf{}

	if val, ok := data.Data["accessKeyId"]; ok {
		config.AccessKeyID = val.(string)
	}

	if val, ok := data.Data["endpoint"]; ok {
		config.Endpoint = val.(string)
	}

	if val, ok := data.Data["secretAccessKey"]; ok {
		config.SecretAccessKey = val.(string)
	}

	if val, ok := data.Data["useSSL"]; ok {
		config.UseSSL = val.(bool)
	}

	return &config, nil
}

// CreateBucketsForProfile creates the profile's buckets in the MinIO instances.
func createBucketsForProfile(client *minio.Client, instance string, profileName string) error {
	for _, bucket := range []string{profileName, "shared"} {
		exists, err := client.BucketExists(context.Background(), bucket)
		if err != nil {
			return err
		}

		if !exists {
			fmt.Printf("making bucket %q in instance %q\n", bucket, instance)
			err = client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{})
			if err != nil {
				return err
			}
		} else {
			fmt.Printf("bucket %q in instance %q already exists\n", bucket, instance)
		}
	}

	// Make shared folder
	_, err := client.PutObject(context.Background(), "shared", path.Join(profileName, ".hold"), bytes.NewReader([]byte{}), 0, minio.PutObjectOptions{})
	if err != nil {
		fmt.Printf("Failed to create shared file for %q in instance %q already exists\n", profileName, instance)
		return err
	}

	return nil
}

var bucketsCmd = &cobra.Command{
	Use:   "buckets",
	Short: "Configure MinIO buckets",
	Long: `Configure MinIO buckets for new Profiles.
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		minioInstances := os.Getenv("MINIO_INSTANCES")
		minioInstancesArray := strings.Split(minioInstances, ",")

		// Vault
		var err error
		var vc *vault.Client
		if os.Getenv("VAULT_AGENT_ADDR") != "" {
			vc, err = vault.NewClient(&vault.Config{
				AgentAddress: os.Getenv("VAULT_AGENT_ADDR"),
			})
		} else {
			// Use the default env vars
			vc, err = vault.NewClient(&vault.Config{})
		}

		if err != nil {
			klog.Fatalf("Error initializing Vault client: %s", err)
		}

		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Kubeflow client: %v", err)
		}

		// Setup informers
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*5)

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {

				for _, instance := range minioInstancesArray {
					conf, err := getMinIOConfig(vc, instance)
					if err != nil {
						return err
					}

					client, err := minio.New(conf.Endpoint, &minio.Options{
						Creds:  credentials.NewStaticV4(conf.AccessKeyID, conf.SecretAccessKey, ""),
						Secure: conf.UseSSL,
					})
					err = createBucketsForProfile(client, instance, profile.Name)
					if err != nil {
						klog.Warningf("error making buckets for profile %s instance %s: %v", profile.Name, instance, err)
						// return err
					}
				}

				return nil
			},
		)

		// Start informers
		kubeflowInformerFactory.Start(stopCh)

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func init() {
	rootCmd.AddCommand(bucketsCmd)
}
