package util

import (
	"os"

	"k8s.io/klog"
)

func ParseEnvVar(envname string) string {
	value, exists := os.LookupEnv(envname)
	if !exists {
		klog.Fatalf("Environment var %s does not exist, and must be set prior to controller startup!",
			envname)
	}
	return value
}