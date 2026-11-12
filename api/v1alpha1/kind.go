package v1alpha1

import (
	"bytes"
	"io"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Kind is one of the resource kinds the controller reconciles.
type Kind string

const (
	KindProviderKey Kind = "ProviderKey"
	KindModel       Kind = "Model"
	KindAPIKey      Kind = "APIKey"
)

// Kinds in resources-file order.
var Kinds = []Kind{KindProviderKey, KindModel, KindAPIKey}

// Collection is the kind's key in the resources file.
func (k Kind) Collection() string {
	switch k {
	case KindProviderKey:
		return "provider_keys"
	case KindModel:
		return "models"
	case KindAPIKey:
		return "api_keys"
	}
	return ""
}

// SchemaName is the kind's file name stem in AISIX's schemas/resources.
func (k Kind) SchemaName() string {
	switch k {
	case KindProviderKey:
		return "provider_key"
	case KindModel:
		return "model"
	case KindAPIKey:
		return "api_key"
	}
	return ""
}

// Plural is the CRD resource name.
func (k Kind) Plural() string {
	switch k {
	case KindProviderKey:
		return "providerkeys"
	case KindModel:
		return "models"
	case KindAPIKey:
		return "apikeys"
	}
	return ""
}

// ShortName is the CRD short name.
func (k Kind) ShortName() string {
	switch k {
	case KindProviderKey:
		return "aipk"
	case KindModel:
		return "aimodel"
	case KindAPIKey:
		return "aikey"
	}
	return ""
}

// Order is the kind's position in the resources file.
func (k Kind) Order() int {
	for i, kk := range Kinds {
		if kk == k {
			return i
		}
	}
	return len(Kinds)
}

// KindForCollection maps a resources-file collection back to its kind.
func KindForCollection(c string) (Kind, bool) {
	for _, k := range Kinds {
		if k.Collection() == c {
			return k, true
		}
	}
	return "", false
}

const (
	// IdentityField names an entry in the resources file; it defaults to
	// metadata.name.
	IdentityField = "display_name"
	// APIKeySecretRef holds a ProviderKey's upstream credential.
	APIKeySecretRef = "api_key_secret_ref"
	// ProviderKeyRef names the ProviderKey a Model authenticates with.
	ProviderKeyRef = "provider_key_ref"
	// KeySecretRef holds an APIKey's plaintext caller key.
	KeySecretRef = "key_secret_ref"

	// HashAnnotation carries the served resources-file hash.
	HashAnnotation = "aisix.api7.ai/resources-hash"
)

// ProviderKeyRefPaths are the objects in a Model spec that hold a
// ProviderKeyRef: the root, for direct models, and a semantic router's
// classifier.
var ProviderKeyRefPaths = [][]string{nil, {"semantic", "classifier"}}
