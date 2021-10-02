package cmd

import (
	"bytes"
	"context"
	"fmt"
	"path"

	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
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

		// Setup informers
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*5)

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {

				// Generate buckets...
				var vc VaultConfigurer = &VaultConfigurerStruct{}
				m := NewMinIO([]string{"standard", "shared"}, vc)
				m.CreateBucketsForProfile(profile.Name)

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

/*******************
 * Handler
 *******************/

// Conf for MinIO
type Conf struct {
	AccessKeyID     string
	Endpoint        string
	SecretAccessKey string
	UseSSL          bool
}

// VaultConfigurer for MinIO
type VaultConfigurer interface {
	GetMinIOConfiguration(instance string) (Conf, error)
}

// VaultConfigurerStruct for MinIO
type VaultConfigurerStruct struct {
}

// GetMinIOConfiguration gets a MinIO instance.
func (m *VaultConfigurerStruct) GetMinIOConfiguration(instance string) (Conf, error) {
	return Conf{
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
		_, err = client.PutObject(context.Background(), "shared", path.Join(profileName, ".hold"), bytes.NewReader([]byte{}), 0, minio.PutObjectOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}
