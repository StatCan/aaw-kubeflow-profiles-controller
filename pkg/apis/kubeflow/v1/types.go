/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient:nonNamespaced

// Profile is a specification for a Profile resource
type Profile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProfileSpec   `json:"spec"`
	Status ProfileStatus `json:"status"`
}

// Plugin is for customize actions on different platform.
type Plugin struct {
	metav1.TypeMeta `json:",inline"`
	Spec            *runtime.RawExtension `json:"spec,omitempty"`
}

type ProfileCondition struct {
	Type    string `json:"type,omitempty"`
	Status  string `json:"status,omitempty" description:"status of the condition, one of True, False, Unknown"`
	Message string `json:"message,omitempty"`
}

// ProfileSpec defines the desired state of Profile
type ProfileSpec struct {
	// The profile owner
	Owner   rbacv1.Subject `json:"owner,omitempty"`
	Plugins []Plugin       `json:"plugins,omitempty"`
	// Resourcequota that will be applied to target namespace
	ResourceQuotaSpec v1.ResourceQuotaSpec `json:"resourceQuotaSpec,omitempty"`
}

const (
	ProfileSucceed = "Successful"
	ProfileFailed  = "Failed"
	ProfileUnknown = "Unknown"
)

// ProfileStatus defines the observed state of Profile
type ProfileStatus struct {
	Conditions []ProfileCondition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ProfileList is a list of Profile resources
type ProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []Profile `json:"items"`
}
