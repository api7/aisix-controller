package v1alpha1

import (
	"encoding/json"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The spec of every kind is the AISIX resource document with its own
// snake_case field names, plus reference fields the controller resolves.
// Its schema is generated from AISIX (cmd/crdgen), so Go keeps it as raw
// JSON.

// ResourceStatus is shared by every kind.
type ResourceStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ProviderKey is an upstream provider endpoint and its credential.
type ProviderKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   apiextensionsv1.JSON `json:"spec"`
	Status ResourceStatus       `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ProviderKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProviderKey `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Model is a model clients call by name.
type Model struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   apiextensionsv1.JSON `json:"spec"`
	Status ResourceStatus       `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// APIKey is a caller credential.
type APIKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   apiextensionsv1.JSON `json:"spec"`
	Status ResourceStatus       `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type APIKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIKey `json:"items"`
}

// Object is implemented by every kind.
// +kubebuilder:object:generate=false
type Object interface {
	metav1.Object
	SpecRaw() []byte
	ResourceStatus() *ResourceStatus
}

func (o *ProviderKey) SpecRaw() []byte                 { return o.Spec.Raw }
func (o *ProviderKey) ResourceStatus() *ResourceStatus { return &o.Status }
func (o *Model) SpecRaw() []byte                       { return o.Spec.Raw }
func (o *Model) ResourceStatus() *ResourceStatus       { return &o.Status }
func (o *APIKey) SpecRaw() []byte                      { return o.Spec.Raw }
func (o *APIKey) ResourceStatus() *ResourceStatus      { return &o.Status }

// SpecFields decodes a raw spec. Numbers stay json.Number so they round-trip
// exactly.
func SpecFields(raw []byte) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) == 0 {
		return out, nil
	}
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
