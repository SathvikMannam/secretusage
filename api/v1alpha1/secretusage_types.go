package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// SecretNameLabel carries the tracked Secret name so SecretUsage objects can be
	// selected with a label selector. It is only set when the Secret name is a valid
	// label value (63 characters or fewer); Secret names may be up to 253 characters.
	SecretNameLabel = "usage.secretusage.io/secret-name"

	// UnusedLabel is set to "true" on SecretUsage objects whose Secret exists but has
	// no references, so unused Secrets can be listed without a field selector:
	//   kubectl get su -A -l usage.secretusage.io/unused=true
	UnusedLabel = "usage.secretusage.io/unused"

	// MissingLabel is set to "true" on SecretUsage objects that have references but
	// whose Secret does not exist. These are the references that will fail the next
	// time a workload restarts.
	MissingLabel = "usage.secretusage.io/missing"
)

// SecretUsageSpec is intentionally empty. The controller owns SecretUsage status.
type SecretUsageSpec struct{}

// SecretUsageStatus describes where a namespaced Secret is referenced.
type SecretUsageStatus struct {
	// SecretName is the Secret represented by this SecretUsage object.
	SecretName string `json:"secretName,omitempty"`

	// Exists indicates whether the Secret currently exists in the namespace.
	Exists bool `json:"exists"`

	// UsageCount is the total number of references found, which may be greater than
	// the number of entries in Usages when Truncated is true.
	UsageCount int32 `json:"usageCount"`

	// Truncated indicates that UsageCount exceeded the controller's per-object limit
	// and Usages holds only the first entries in sorted order. The limit keeps the
	// object below the etcd per-object size limit.
	Truncated bool `json:"truncated,omitempty"`

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
// +kubebuilder:printcolumn:name="Truncated",type=boolean,JSONPath=`.status.truncated`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
