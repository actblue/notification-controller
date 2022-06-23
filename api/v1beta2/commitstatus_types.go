/*
Copyright 2020 The Flux authors

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

package v1beta2

import (
	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CommitStatusSpec defines an alerting rule for events involving a list of objects
type CommitStatusSpec struct {
	// Send events using this provider.
	// +required
	ProviderRef meta.LocalObjectReference `json:"providerRef"`

	// Filter events based on severity, defaults to ('info').
	// If set to 'info' no events will be filtered.
	// +kubebuilder:validation:Enum=info;error
	// +kubebuilder:default:=info
	// +optional
	EventSeverity string `json:"eventSeverity,omitempty"`

	// Filter events based on the involved objects.
	// Only accepts Kustomization resources, since it's the only
	// object that reconciles against Git.
	// +required
	EventSources []CrossNamespaceObjectReference `json:"eventSources"`

	// A list of Golang regular expressions to be used for excluding messages.
	// +optional
	ExclusionList []string `json:"exclusionList,omitempty"`

	// Template specific to the configuration of commit statuses
	// +optional
	Template CommitStatusTemplate `json:"template,omitempty"`

	// This flag tells the controller to suspend subsequent events dispatching.
	// Defaults to false.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

type CommitStatusTemplate struct {
	// A string label to differentiate statuses from one another on a commit.
	// GitHub calls this the commit status's "context".
	// +required
	Key string `json:"key"`

	// A short description of the status.
	// +optional
	Description string `json:"description,omitempty"`

	// A URL to associate with this status allowing users
	// to easily see the source of the status from a link on the commit status.
	// For example, if your continuous integration system is posting build status,
	// you would want to provide the deep link for the build output for this specific SHA.
	// +optional
	TargetURL string `json:"targetUrl,omitempty"`

	// List of ConfigMaps whose content contains additional parameters to be used in the rest of the template
	// +optional
	AdditionalParameters []CrossNamespaceConfigMapReference `json:"additionalParameters,omitempty"`
}

// CommitStatusStatus defines the observed state of CommitStatus
type CommitStatusStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last observed generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +genclient
// +genclient:Namespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].message",description=""

// CommitStatus is the Schema for the commit status API
type CommitStatus struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CommitStatusSpec `json:"spec,omitempty"`
	// +kubebuilder:default:={"observedGeneration":-1}
	Status CommitStatusStatus `json:"status,omitempty"`
}

// GetStatusConditions returns a pointer to the Status.Conditions slice
// Deprecated: use GetConditions instead.
func (in *CommitStatus) GetStatusConditions() *[]metav1.Condition {
	return &in.Status.Conditions
}

// GetConditions returns the status conditions of the object.
func (in *CommitStatus) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (in *CommitStatus) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// CommitStatusList contains a list of CommitStatus
type CommitStatusList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CommitStatus `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CommitStatus{}, &CommitStatusList{})
}
