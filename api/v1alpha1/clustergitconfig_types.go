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

type CredentialReference struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}
type GitAccessObservation struct {
	RepositoryURL         string      `json:"repositoryURL"`
	Operation             string      `json:"operation"`
	Success               bool        `json:"success"`
	Reason                string      `json:"reason"`
	Message               string      `json:"message,omitempty"`
	ConfigGeneration      int64       `json:"configGeneration"`
	SecretUID             string      `json:"secretUID"`
	SecretResourceVersion string      `json:"secretResourceVersion"`
	LastUpdatedAt         metav1.Time `json:"lastUpdatedAt"`
}
type ClusterGitConfigStatus struct {
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	CredentialsRef     *CredentialReference `json:"credentialsRef,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition    `json:"conditions,omitempty"`
	LastAccess *GitAccessObservation `json:"lastAccess,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
type ClusterGitConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterGitConfigSpec   `json:"spec"`
	Status            ClusterGitConfigStatus `json:"status,omitempty"`
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
