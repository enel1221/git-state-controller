package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type GitConfigReference struct {
	// +kubebuilder:default=ClusterGitConfig
	// +kubebuilder:validation:Enum=ClusterGitConfig
	Kind string `json:"kind,omitempty"`
	// +kubebuilder:default=default
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name,omitempty"`
}

type Repository struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="repository URL is immutable"
	URL string `json:"url"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="branch is immutable"
	Branch string `json:"branch"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^.+\.ya?ml$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="file path is immutable"
	Path string `json:"path"`
}

type Change struct {
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Message string `json:"message,omitempty"`
}

type GitResourceSpec struct {
	// +optional
	// +kubebuilder:default={kind:ClusterGitConfig,name:default}
	GitConfigRef GitConfigReference `json:"gitConfigRef,omitempty"`
	Repository   Repository         `json:"repository"`
	// +optional
	Change Change `json:"change,omitempty"`
	// +optional
	// +kubebuilder:default=Delete
	// +kubebuilder:validation:Enum=Delete;Orphan
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:EmbeddedResource
	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:XValidation:rule="has(self.apiVersion) && self.apiVersion != '' && has(self.kind) && self.kind != '' && has(self.metadata) && has(self.metadata.name) && self.metadata.name != ''",message="manifest requires apiVersion, kind and metadata.name"
	Manifest runtime.RawExtension `json:"manifest"`
}

type GitResourceStatus struct {
	ObservedGeneration      int64 `json:"observedGeneration,omitempty"`
	LastPublishedGeneration int64 `json:"lastPublishedGeneration,omitempty"`
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	LastPublishedRevision string `json:"lastPublishedRevision,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	LastPublishedContentHash string                `json:"lastPublishedContentHash,omitempty"`
	ApplicationRef           *ApplicationReference `json:"applicationRef,omitempty"`
	PublishedResourceRef     *ResourceReference    `json:"publishedResourceRef,omitempty"`
	ArgoCD                   *ApplicationSnapshot  `json:"argoCD,omitempty"`
	Resource                 *ResourceSnapshot     `json:"resource,omitempty"`
	Cleanup                  *CleanupCheckpoint    `json:"cleanup,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type ApplicationReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type ResourceReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
}
type Observation struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}
type ApplicationSnapshot struct {
	UID               string       `json:"uid,omitempty"`
	ResourceVersion   string       `json:"resourceVersion,omitempty"`
	Generation        *int64       `json:"generation,omitempty"`
	DeletionTimestamp *metav1.Time `json:"deletionTimestamp,omitempty"`
	LastUpdatedAt     *metav1.Time `json:"lastUpdatedAt,omitempty"`
	Observation       Observation  `json:"observation"`
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Source *apiextensionsv1.JSON `json:"source,omitempty"`
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Destination *apiextensionsv1.JSON `json:"destination,omitempty"`
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Status *apiextensionsv1.JSON `json:"status,omitempty"`
}
type ResourceSnapshot struct {
	Ref                     ResourceReference `json:"ref"`
	ResourceVersion         string            `json:"resourceVersion,omitempty"`
	Generation              *int64            `json:"generation,omitempty"`
	DeletionTimestamp       *metav1.Time      `json:"deletionTimestamp,omitempty"`
	ObservedAgainstRevision string            `json:"observedAgainstRevision,omitempty"`
	Exists                  *bool             `json:"exists,omitempty"`
	LastUpdatedAt           *metav1.Time      `json:"lastUpdatedAt,omitempty"`
	Observation             Observation       `json:"observation"`
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Status *apiextensionsv1.JSON `json:"status,omitempty"`
}
type CleanupCheckpoint struct {
	// +kubebuilder:validation:Enum=Delete;Orphan
	Policy   string `json:"policy"`
	Revision string `json:"revision,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Published",type=string,JSONPath=`.status.conditions[?(@.type=="Published")].status`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.lastPublishedGeneration`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.lastPublishedRevision`
type GitResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GitResourceSpec   `json:"spec"`
	Status            GitResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type GitResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitResource `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(GroupVersion, &GitResource{}, &GitResourceList{})
		return nil
	})
}
