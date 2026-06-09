package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretUsageSpec is intentionally empty. The controller owns SecretUsage status.
type SecretUsageSpec struct{}

// SecretUsageStatus describes where a namespaced Secret is referenced.
type SecretUsageStatus struct {
	// SecretName is the Secret represented by this SecretUsage object.
	SecretName string `json:"secretName,omitempty"`

	// Exists indicates whether the Secret currently exists in the namespace.
	Exists bool `json:"exists"`

	// UsageCount is the number of entries in Usages.
	UsageCount int32 `json:"usageCount"`

	// Usages lists Kubernetes objects and fields that reference the Secret.
	Usages []SecretUsageReference `json:"usages,omitempty"`
}

// SecretUsageReference identifies a single reference from an object to a Secret.
type SecretUsageReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	FieldPath  string `json:"fieldPath"`
	Container  string `json:"container,omitempty"`
	Key        string `json:"key,omitempty"`
	Optional   *bool  `json:"optional,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=su
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.status.secretName`
// +kubebuilder:printcolumn:name="Exists",type=boolean,JSONPath=`.status.exists`
// +kubebuilder:printcolumn:name="Usages",type=integer,JSONPath=`.status.usageCount`

// SecretUsage is a namespaced inventory object for one Secret's references.
type SecretUsage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SecretUsageSpec   `json:"spec,omitempty"`
	Status SecretUsageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SecretUsageList contains a list of SecretUsage objects.
type SecretUsageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecretUsage `json:"items"`
}
