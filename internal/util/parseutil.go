package util

import (
	"os"
	"strconv"

	"k8s.io/klog"
)

// Parse an environment variable as a string
func ParseEnvVar(envname string) string {
	value, exists := os.LookupEnv(envname)
	if !exists {
		klog.Fatalf("Environment var %s does not exist, and must be set prior to controller startup!",
			envname)
	}
	return value
}

// Parse an integer environment variable
func ParseIntegerEnvVar(envname string) int {
	value := ParseEnvVar(envname)
	intVal, err := strconv.Atoi(envname)
	if err != nil {
		klog.Fatalf("Could not parse environment variable %s with values %s as an integer!", envname, value)
	}
	return intVal
}