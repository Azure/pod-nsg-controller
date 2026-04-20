package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodASGMapping is the Schema for the podasgmappings API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mappings",type=integer,JSONPath=`.status.mappingCount`,description="Number of mapping rules"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PodASGMapping struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodASGMappingSpec   `json:"spec"`
	Status PodASGMappingStatus `json:"status,omitempty"`
}

// PodASGMappingList contains a list of PodASGMapping.
// +kubebuilder:object:root=true
type PodASGMappingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodASGMapping `json:"items"`
}

// PodASGMappingSpec defines the desired state of PodASGMapping.
type PodASGMappingSpec struct {
	// +kubebuilder:validation:MinItems=1
	Mappings []Mapping `json:"mappings"`
}

// Mapping maps a pod selector to ASG references.
type Mapping struct {
	PodSelector PodSelector `json:"podSelector"`

	// +kubebuilder:validation:MinItems=1
	ApplicationSecurityGroups []ASGReference `json:"applicationSecurityGroups"`
}

// PodSelector selects pods by labels.
type PodSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

// ASGReference references an Azure Application Security Group.
type ASGReference struct {
	// +kubebuilder:validation:Pattern=`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/applicationSecurityGroups/[^/]+$`
	ResourceID     string `json:"resourceId"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
	Description    string `json:"description,omitempty"`
}

// PodASGMappingStatus defines the observed state of PodASGMapping.
type PodASGMappingStatus struct {
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
	MappingCount    int                `json:"mappingCount,omitempty"`
	MappingStatuses []MappingStatus    `json:"mappingStatuses,omitempty"`
}

// MappingStatus tracks the status of a single mapping rule.
type MappingStatus struct {
	SelectorHash string      `json:"selectorHash"`
	MatchedPods  int         `json:"matchedPods"`
	ASGSyncState string      `json:"asgSyncState"`
	LastSyncTime metav1.Time `json:"lastSyncTime,omitempty"`
	Error        string      `json:"error,omitempty"`
}

func init() {
	SchemeBuilder.Register(&PodASGMapping{}, &PodASGMappingList{})
}
