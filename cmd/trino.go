package cmd

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"

	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	"k8s.io/klog"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	kubeinformers "k8s.io/client-go/informers"
	clientv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	// project packages
)

// Trino Schema format
type Schema struct {
	User   string `json:"user"`
	Schema string `json:"schema"`
	Owner  bool   `json:"owner"`
}

// Trino Schema format
type Table struct {
	User   string   `json:"user"`
	Schema string   `json:"schema"`
	Table  string   `json:"table"`
	Priv   []string `json:"privileges"`
}

type Rules struct {
	Schema []Schema `json:"schemas"`
	Table  []Table  `json:"tables"`
}
type config struct {
	namespace string
	configmap string
}

var trinoAdmins = []string{"rohan.katkar@cloud.statcan.ca", "pat.ledgerwood@cloud.statcan.ca", "wendy.gaultier2@cloud.statcan.ca"}

var sch = []Schema{}
var tbl = []Table{}

var protbSchema = []Schema{}
var protbTable = []Table{}

var trino = &cobra.Command{
	Use:   "Trino",
	Short: "Configure Trino RBAC",
	Long:  `Configure Trino Rules in ConfigMap`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()
		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("Error building kubeconfig: %v", err)
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building Kubeflow client: %v", err)
		}

		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}
		// Setup Kubeflow informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		// Setup configMap informers
		configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()
		configMapLister := configMapInformer.Lister()

		// Setup roleBinding informers
		roleBindingInformer := kubeInformerFactory.Rbac().V1().RoleBindings()
		roleBindingLister := roleBindingInformer.Lister()

		// Setup controller
		// For all profiles in the cluster, edit role-bindings from each profile is extracted
		// and a configmap is updated for each profile using the Trino rules with the updated role-bindings
		// to build Trino rules
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				var contributors = []string{}
				var profileOwnerName string
				allRoleBindings, err := roleBindingLister.RoleBindings(profile.Name).List(labels.Everything())
				if err != nil {
					return err
				}
				profileOwnerName = profile.Spec.Owner.Name
				for _, v := range allRoleBindings {
					if v.RoleRef.Name == "kubeflow-edit" {
						for _, sub := range v.Subjects {
							if sub.Kind == "User" {
								//extract kubeflow contributors that have edit role-bindings
								profileOwnerName = strings.Split(sub.Name, "@")[0]
								contributors = append(contributors, strings.Replace(profileOwnerName, ".", "", -1))
							}
						}
					}
				}
				//unclassified rules
				createInstance(append(contributors, strings.Replace(profile.Name, "-", "", -1)), profile.Spec.Owner.Name, configMapLister, kubeClient, "trino-system", "trino-unclassified-rules")
				// protected-b rules
				createInstance(append(contributors, strings.Replace(profile.Name, "-", "", -1)), profile.Spec.Owner.Name, configMapLister, kubeClient, "trino-protb-system", "trino-protb-rules")

				return nil
			},
		)

		// Declare Event Handlers for Informers
		roleBindingInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newDep := new.(*v1.RoleBinding)
				oldDep := old.(*v1.RoleBinding)

				if newDep.ResourceVersion == oldDep.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})
		configMapInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newDep := new.(*corev1.ConfigMap)
				oldDep := old.(*corev1.ConfigMap)

				if newDep.ResourceVersion == oldDep.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		// Start informers
		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		// Wait for caches to sync
		klog.Info("Waiting for Rolebinding informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, roleBindingInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}
		klog.Info("Waiting for configMap informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, configMapInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

// Append profile owner as a contributor
func containsOwner(contributors []string, profileOwner string) bool {
	for _, a := range contributors {
		if a == profileOwner {
			return true
		}
	}
	return false
}

// Append trino rules in appropriate configmap and namespace
func createInstance(contributors []string, profileOwner string, configMapLister clientv1.ConfigMapLister, kubeClient kubernetes.Interface, configNamespace string, configName string) {
	if strings.Contains(configName, "protb") {
		createProtbRule(contributors, profileOwner)
	} else {
		createRule(contributors, profileOwner)
	}
	// Create cm if it does not exist, update trino rule data to confimap if it exists
	var trinoConfigMap *corev1.ConfigMap
	c, _ := configMapLister.ConfigMaps(configNamespace).Get(configName)
	if c == nil {
		var trinoConfigMap, err = generateTrinoConfigMap(configName, configNamespace)
		klog.Infof("creating configMap %s/%s", configNamespace, configName)
		_, err = kubeClient.CoreV1().ConfigMaps(configNamespace).Create(
			context.Background(), trinoConfigMap, metav1.CreateOptions{})
		if err != nil {
			klog.Fatalf("error creating configmap: %v", err)
			return
		}
	} else {
		trinoConfigMap, err = generateTrinoConfigMap(configName, configNamespace)
		updateTrinoConfigMap(trinoConfigMap, configMapLister, kubeClient, configName, configNamespace)
	}
}

// Update Trino configmap on each profile to pick up updated role bindings
func updateTrinoConfigMap(cm *corev1.ConfigMap, configMapLister clientv1.ConfigMapLister, kubeClient kubernetes.Interface, cmName string, namespace string) error {
	//Update configmap for each profile
	currentStandardConfigMap, err := configMapLister.ConfigMaps(namespace).Get(cmName)
	if !reflect.DeepEqual(cm.Data, currentStandardConfigMap.Data) {
		klog.Infof("updating configMap %s/%s", cm.Namespace, cmName)
		currentStandardConfigMap.Data = cm.Data
		_, err = kubeClient.CoreV1().ConfigMaps(cm.Namespace).Update(context.Background(),
			currentStandardConfigMap, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

// Create Trino configmap using schema and table rules in the trino-system ns
// Convert Rules slice into json format
func generateTrinoConfigMap(fileName string, namespace string) (*corev1.ConfigMap, error) {
	var rules = []Rules{}
	if strings.Contains(fileName, "protb") {
		rules = append(rules, Rules{Schema: protbSchema, Table: protbTable})
	} else {
		rules = append(rules, Rules{Schema: sch, Table: tbl})
	}
	data, _ := json.MarshalIndent(rules, "", "  ")
	var output = string(data)
	output = strings.TrimPrefix(output, "[")
	output = strings.TrimSuffix(output, "]")

	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fileName,
			Namespace: namespace,
		},
		Data: map[string]string{
			fileName + ".json": output,
		},
	}
	return cm, nil
}

// Retrieve name of namespace from edit rolebinding
// rolebinding name: user-rohan-katkar-cloud-statcan-ca-clusterrole-edit
// return: rohan-katkar
func extractName(name string) string {
	var s []string = strings.Split(name, "-")
	var split = s[1:3]
	return split[0] + "-" + split[1]
}

// Create table and schema rules for each profile
// Ex. Format
// Schema Rule:
//{
//	"user": "Jose Matsuda"	                                                                                                                                                                                    ││
//  "schema": "(josematsuda)",                                                                                                                                                         │
//  "owner": true                                                                                                                                                                      │
//},
// Table Rule:
// {                                                                                                                                                                                    │
//     "user": "Jose Matsuda",                                                                                                                                                         │
//     "schema": "(josematsuda)",                                                                                                                                                     │
//     "table": ".*",                                                                                                                                                                     │
//     "privileges": [                                                                                                                                                                    │
//     "SELECT",                                                                                                                                                                        │
//     "INSERT",                                                                                                                                                                        │
//     "DELETE",                                                                                                                                                                        │
//     "UPDATE",                                                                                                                                                                        │
//     "OWNERSHIP"                                                                                                                                                                      │
//     ]                                                                                                                                                                                  │
//},
func createRule(contributors []string, profile string) error {
	var s Schema = initializeSchema(profile)
	var t Table = initializeTable(profile)
	t.Priv = append(t.Priv, "SELECT", "INSERT", "DELETE", "UPDATE", "OWNERSHIP")
	for i := 0; i < len(contributors); i++ {

		// format the schema field, removing dashes and appending brackets
		if i == len(contributors)-1 {
			s.Schema += strings.Replace(strings.ToLower(contributors[i]), " ", "", -1)
		} else {
			s.Schema += strings.Replace(strings.ToLower(contributors[i]), " ", "", -1) + "|"
		}
	}
	if containsOwner(trinoAdmins, profile) {
		t.Schema = ".*"
		s.Schema = ".*"
	} else {
		t.Schema = "(" + s.Schema + ")"
		s.Schema = "(" + s.Schema + ")"
	}
	tbl = append(tbl, t)
	sch = append(sch, s)
	if err != nil {
		return err
	}
	return nil
}

// Create protb schema and table rules. 2 table rules are created:
// 1. Readonly on unclassified schema 2. All privledge access on protb schema
func createProtbRule(contributors []string, profile string) error {

	var s Schema = initializeSchema(profile)
	var t Table = initializeTable(profile)
	t.Priv = append(t.Priv, "SELECT", "INSERT", "DELETE", "UPDATE", "OWNERSHIP")

	var readOnlyTable Table = initializeTable(profile)
	readOnlyTable.Priv = append(readOnlyTable.Priv, "SELECT")
	for i := 0; i < len(contributors); i++ {
		// format the schema field, removing dashes and appending brackets
		if i == len(contributors)-1 {
			s.Schema += strings.Replace(contributors[i], " ", "", -1) + "protb"
			readOnlyTable.Schema += strings.Replace(contributors[i], " ", "-", -1)
		} else {
			s.Schema += strings.Replace(contributors[i], " ", "", -1) + "protb" + "|"
			readOnlyTable.Schema += strings.Replace(contributors[i], " ", "", -1) + "|"
		}
	}
	// Give trino admin access to all schemas and tables
	if containsOwner(trinoAdmins, profile) {
		t.Schema = ".*"
		s.Schema = ".*"
	} else {
		t.Schema = "(" + s.Schema + ")"
		s.Schema = "(" + s.Schema + ")"
		readOnlyTable.Schema = "(" + readOnlyTable.Schema + ")"
	}
	protbTable = append(protbTable, t, readOnlyTable)
	protbSchema = append(protbSchema, s)
	if err != nil {
		return err
	}

	return nil
}

// Generate default Schema rule
func initializeSchema(profileOwner string) Schema {
	var s *Schema
	s = new(Schema)
	s.Schema = ""
	s.User = profileOwner
	s.Owner = true
	return *s
}

// Generate default Table rule
func initializeTable(profileOwner string) Table {
	var t *Table
	t = new(Table)
	t.User = profileOwner
	t.Table = ".*"
	t.Schema = ""
	t.Priv = nil
	return *t
}

func init() {
	rootCmd.AddCommand(trino)
}
