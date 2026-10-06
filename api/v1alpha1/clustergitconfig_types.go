package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type SecretReference struct {
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}
type Credentials struct {
	// +kubebuilder:validation:Enum=Secret
	Source    string          `json:"source"`
	SecretRef SecretReference `json:"secretRef"`
}
type CommitAuthor struct {
	// +kubebuilder:default=git-state-controller
	Name string `json:"name,omitempty"`
	// +kubebuilder:default="git-state-controller@example.invalid"
	Email string `json:"email,omitempty"`
}
type ClusterGitConfigSpec struct {
	Credentials Credentials `json:"credentials"`
	// +optional
	// +kubebuilder:default={name:git-state-controller,email:"git-state-controller@example.invalid"}
	CommitAuthor CommitAuthor `json:"commitAuthor,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type ClusterGitConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterGitConfigSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type ClusterGitConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterGitConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(GroupVersion, &ClusterGitConfig{}, &ClusterGitConfigList{})
		return nil
	})
}
