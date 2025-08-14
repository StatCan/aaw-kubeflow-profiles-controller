package util

import (
	"os"
	"strings"

	"k8s.io/klog/v2"
)

// Returns the namespace the pod is running in
func PodNamespace() string {
	// First check if the environment variable is set, this should be in the helm chart
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	// If the environment variable is not set, read the namespace from the file
	ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return strings.TrimSpace(string(ns))
	}
	// If the file cannot be read, log a fatal error
	klog.Fatalf("Error reading namespace: %v", err)
	// Default to "default" namespace if all else fails
	return "default"
}
