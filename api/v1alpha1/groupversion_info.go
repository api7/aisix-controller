// Package v1alpha1 holds the aisix.api7.ai/v1alpha1 custom resource types.
// +kubebuilder:object:generate=true
// +groupName=aisix.api7.ai
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "aisix.api7.ai", Version: "v1alpha1"}

	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&ProviderKey{}, &ProviderKeyList{},
		&Model{}, &ModelList{},
		&APIKey{}, &APIKeyList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
