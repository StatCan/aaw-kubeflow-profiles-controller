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

var bucketsCmd = &cobra.Command{
	Use:   "buckets",
	Short: "Configure MinIO buckets",
	Long: `Configure MinIO buckets for new Profiles.
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Kubeflow client: %v", err)
		}

		// Vault
		var vc *vault.Client
		if os.Getenv("VAULT_AGENT_ADDR") == "" {
			// VAULT_ADDR and VAULT_TOKEN env vars should be set to http://0.0.0.0:8200 and root token or this will fail
			// We send nil to get the DefaultConfig
			vc, err = vault.NewClient(nil)
		} else {
			config := vault.Config{
				AgentAddress: os.Getenv("VAULT_AGENT_ADDR"),
			}
			vc, err = vault.NewClient(&config)
		}
		if err != nil {
			klog.Fatalf("Error initializing Vault client: %s", err)
		}

		// Setup informers
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*5)

		// Setup controller
		controller := profiles.NewProfilesController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {

				// Create role and generate buckets...
				vaultConfigurer := NewVaultConfigurer(*vc)
				vaultConfigurer.CreateMinioVaultRoleForProfile([]string{"minio"}, profile.Name)
				m := NewMinIO([]string{"minio"}, vaultConfigurer)
				m.CreateBucketsForProfile(profile.Name)

				return nil
			},
		)

		// Start informers
		kubeflowInformerFactory.Start(stopCh)

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatal("error running controller: %v", err)
		}
	},
}

func init() {
	rootCmd.AddCommand(bucketsCmd)
}

/*******************
 * Handler
 *******************/

type VaultConfigurer interface {
	GetMinIOConfiguration(instance string) (MinIOConfiguration, error)
	CreateMinioVaultRoleForProfile(authpaths []string, profileName string) error
}

type VaultConfigurerStruct struct {
	logical vault.Logical
}

// Create a VaultConfigurerStruct that implements the VaultConfigurer
func NewVaultConfigurer(client vault.Client) *VaultConfigurerStruct {
	return &VaultConfigurerStruct{
		logical: *client.Logical(),
	}
}

type MinIOConfiguration struct {
	AccessKeyID     string
	Endpoint        string
	SecretAccessKey string
	UseSSL          bool
}

func (vc *VaultConfigurerStruct) GetMinIOConfiguration(instance string) (MinIOConfiguration, error) {
	// Hardcoded for now
	return MinIOConfiguration{
		Endpoint:        "localhost:9000",
		AccessKeyID:     "minioadmin",
		SecretAccessKey: "minioadmin",
		UseSSL:          false,
	}, nil
}

// NewMinIO creates a MinIO instance.
func NewMinIO(minioInstances []string, vault VaultConfigurer) MinIO {
	return &MinIOStruct{
		MinioInstances:  minioInstances,
		VaultConfigurer: vault,
	}
}

// MinIO is the interface for interacting with a MinIO instance.
type MinIO interface {
	CreateBucketsForProfile(profileName string) error
}

// MinIOStruct is a MinIO implementation.
type MinIOStruct struct {
	VaultConfigurer VaultConfigurer
	MinioInstances  []string
}

// Configures the minio secret stores for the given profile name for each of the minio instances
// described by authpaths
func (vc *VaultConfigurerStruct) CreateMinioVaultRoleForProfile(authpaths []string, profileName string) error {
	prefixedProfileName := fmt.Sprintf("profile-%s", profileName)

	for _, authpath := range authpaths {
		rolePath := fmt.Sprintf("%s/roles/%s", authpath, prefixedProfileName)

		secret, err := vc.logical.Read(rolePath)
		if err != nil && !strings.Contains(err.Error(), "role not found") {
			klog.Warning("Role not found %q", rolePath)
			return err
		}

		if secret == nil {
			klog.Infof("Creating backend role in %q for %q", authpath, prefixedProfileName)

			secret, err = vc.logical.Write(rolePath, map[string]interface{}{
				// Hardcoded policy for now (existing MinIO policy)
				"policy":           "readwrite",
				"user_name_prefix": fmt.Sprintf("%s-", prefixedProfileName),
			})

			if err != nil {
				return err
			}
		} else {
			klog.Infof("Backend role in %q for %q already exists\n", authpath, prefixedProfileName)
		}
	}

	return nil
}

// CreateBucketsForProfile creates the profile's buckets in the MinIO instances.
func (m *MinIOStruct) CreateBucketsForProfile(profileName string) error {
	for _, instance := range m.MinioInstances {
		conf, err := m.VaultConfigurer.GetMinIOConfiguration(instance)
		if err != nil {
			return err
		}

		client, err := minio.New(conf.Endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(conf.AccessKeyID, conf.SecretAccessKey, ""),
			Secure: conf.UseSSL,
		})

		if err != nil {
			return err
		}

		for _, bucket := range []string{profileName, "shared"} {
			exists, err := client.BucketExists(context.Background(), bucket)
			if err != nil {
				return err
			}

			if !exists {
				klog.Infof("making bucket %q in instance %q\n", bucket, instance)
				err = client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{})
				if err != nil {
					return err
				}
			} else {
				klog.Infof("bucket %q in instance %q already exists\n", bucket, instance)
			}
		}

		// Make shared folder
		_, err = client.PutObject(context.Background(), "shared", path.Join(profileName, ".hold"), bytes.NewReader([]byte{}), 0, minio.PutObjectOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}
