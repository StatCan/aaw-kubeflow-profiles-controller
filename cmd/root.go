package cmd

import (
	"github.com/spf13/cobra"
)

var apiserver string
var kubeconfig string

var rootCmd = &cobra.Command{
	Use:   "profiles-controller",
	Short: "A series of controllers for configuring Kubeflow profiles",
	Long:  `A series of controllers for configuring Kubeflow profiles`,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&apiserver, "apiserver", "", "URL to the Kubernetes API server")
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the Kubeconfig file")
}

// Execute executes the root command.
func Execute() error {
	return rootCmd.Execute()
}
