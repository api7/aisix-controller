package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/api7/aisix-controller/api/v1alpha1"
)

// idFormFields are the id-form references of a kind. The resources file
// refuses them in favor of the name form (aisix v1.9.0
// crates/aisix-core/src/filesource/mod.rs, model_ref_id_fields and
// mcp_ref_id_fields).
var idFormFields = map[v1alpha1.Kind][]string{
	v1alpha1.KindModel:  {"model_id", "target_id", "embedding_model_id", "default_id"},
	v1alpha1.KindAPIKey: {"allowed_model_ids", "mcp_rate_limits_by_id", "allow_ids", "deny_ids"},
}

// removedFields are dropped from a kind's spec at any depth.
func removedFields(k v1alpha1.Kind) map[string]bool {
	out := map[string]bool{}
	switch k {
	case v1alpha1.KindProviderKey:
		// `secret` is an alias of `api_key`.
		out["api_key"] = true
		out["secret"] = true
	case v1alpha1.KindModel:
		out["provider_key_id"] = true
	case v1alpha1.KindAPIKey:
		out["key_hash"] = true
	}
	for _, f := range idFormFields[k] {
		out[f] = true
	}
	return out
}

func secretKeyRef(description string) obj {
	return obj{
		"type":        "object",
		"description": description,
		"required":    []any{"name", "key"},
		"properties": obj{
			"name": obj{"type": "string", "minLength": 1, "description": "Secret name, in the resource's namespace."},
			"key":  obj{"type": "string", "minLength": 1, "description": "Key within the Secret."},
		},
	}
}

type addedField struct {
	name     string
	schema   obj
	required bool
}

// addedFields are the reference fields the controller resolves.
func addedFields(k v1alpha1.Kind) []addedField {
	switch k {
	case v1alpha1.KindProviderKey:
		return []addedField{{
			v1alpha1.APIKeySecretRef,
			secretKeyRef("Secret key holding the upstream provider credential. The controller writes it as `api_key`."),
			true,
		}}
	case v1alpha1.KindModel:
		return []addedField{{
			v1alpha1.ProviderKeyRef,
			obj{
				"type":        "object",
				"description": "ProviderKey, in the same namespace, this model authenticates with. Direct models need it; routing, ensemble and semantic models leave it unset.",
				"required":    []any{"name"},
				"properties": obj{
					"name": obj{"type": "string", "minLength": 1, "description": "ProviderKey `metadata.name`."},
				},
			},
			false,
		}}
	case v1alpha1.KindAPIKey:
		return []addedField{{
			v1alpha1.KeySecretRef,
			secretKeyRef("Secret key holding the caller's plaintext API key. The controller writes only its SHA-256 hash."),
			true,
		}}
	}
	return nil
}

// LoadSchema reads a vendored AISIX schema.
func LoadSchema(dir string, k v1alpha1.Kind) (obj, error) {
	b, err := os.ReadFile(filepath.Join(dir, k.SchemaName()+".schema.json"))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out obj
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: %w", k.SchemaName(), err)
	}
	return out, nil
}

// Shape adapts an AISIX schema for Kubernetes. The result is still draft-07.
func Shape(k v1alpha1.Kind, root obj) obj {
	root = deepCopy(root).(obj)
	stripFields(root, removedFields(k))

	// Root compositions only encode cross-field rules, which the gateway
	// pipeline enforces.
	for _, key := range []string{"anyOf", "oneOf", "allOf", "if", "then", "else", "not"} {
		delete(root, key)
	}

	var required []any
	if r, ok := root["required"].([]any); ok {
		for _, v := range r {
			if v != v1alpha1.IdentityField {
				required = append(required, v)
			}
		}
	}
	props, _ := root["properties"].(obj)
	if props == nil {
		props = obj{}
		root["properties"] = props
	}
	if identity, ok := props[v1alpha1.IdentityField].(obj); ok {
		base, _ := identity["description"].(string)
		identity["description"] = strings.TrimSpace(base + " Defaults to `metadata.name`.")
	}
	for _, f := range addedFields(k) {
		props[f.name] = f.schema
		if f.required {
			required = append(required, f.name)
		}
	}
	if len(required) == 0 {
		delete(root, "required")
	} else {
		root["required"] = required
	}
	return root
}

func stripFields(node any, removed map[string]bool) {
	switch n := node.(type) {
	case obj:
		if props, ok := n["properties"].(obj); ok {
			for k := range props {
				if removed[k] {
					delete(props, k)
				}
			}
		}
		if req, ok := n["required"].([]any); ok {
			var keep []any
			for _, v := range req {
				if s, ok := v.(string); !ok || !removed[s] {
					keep = append(keep, v)
				}
			}
			n["required"] = keep
		}
		for _, v := range n {
			stripFields(v, removed)
		}
	case []any:
		for _, v := range n {
			stripFields(v, removed)
		}
	}
}

func deepCopy(v any) any {
	switch n := v.(type) {
	case obj:
		out := make(obj, len(n))
		for k, x := range n {
			out[k] = deepCopy(x)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, x := range n {
			out[i] = deepCopy(x)
		}
		return out
	}
	return v
}

func statusSchema() obj {
	return obj{
		"type": "object",
		"properties": obj{
			"observedGeneration": obj{"type": "integer", "format": "int64"},
			"conditions": obj{
				"type":                       "array",
				"x-kubernetes-list-type":     "map",
				"x-kubernetes-list-map-keys": []any{"type"},
				"items": obj{
					"type":     "object",
					"required": []any{"type", "status", "lastTransitionTime", "reason", "message"},
					"properties": obj{
						"type":               obj{"type": "string"},
						"status":             obj{"type": "string", "enum": []any{"True", "False", "Unknown"}},
						"observedGeneration": obj{"type": "integer", "format": "int64"},
						"lastTransitionTime": obj{"type": "string", "format": "date-time"},
						"reason":             obj{"type": "string"},
						"message":            obj{"type": "string"},
					},
				},
			},
		},
	}
}

// ResourceSchema is the full structural schema of a kind's custom resource.
func ResourceSchema(k v1alpha1.Kind, aisixSchema obj) obj {
	return obj{
		"type":        "object",
		"description": fmt.Sprintf("AISIX %s resource.", k),
		"required":    []any{"spec"},
		"properties": obj{
			"apiVersion": obj{"type": "string"},
			"kind":       obj{"type": "string"},
			"metadata":   obj{"type": "object"},
			"spec":       ToStructural(Shape(k, aisixSchema)),
			"status":     statusSchema(),
		},
	}
}

func conditionColumn(name string) apiextensionsv1.CustomResourceColumnDefinition {
	return apiextensionsv1.CustomResourceColumnDefinition{
		Name:     name,
		Type:     "string",
		JSONPath: fmt.Sprintf(`.status.conditions[?(@.type=="%s")].status`, name),
	}
}

// CRD builds a kind's CustomResourceDefinition.
func CRD(k v1alpha1.Kind, aisixSchema obj) (*apiextensionsv1.CustomResourceDefinition, error) {
	raw, err := json.Marshal(ResourceSchema(k, aisixSchema))
	if err != nil {
		return nil, err
	}
	var props apiextensionsv1.JSONSchemaProps
	if err := json.Unmarshal(raw, &props); err != nil {
		return nil, fmt.Errorf("%s: structural schema: %w", k, err)
	}
	group := v1alpha1.GroupVersion.Group
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{Name: k.Plural() + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:       string(k),
				ListKind:   string(k) + "List",
				Plural:     k.Plural(),
				Singular:   strings.ToLower(string(k)),
				ShortNames: []string{k.ShortName()},
				Categories: []string{"aisix"},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    v1alpha1.GroupVersion.Version,
				Served:  true,
				Storage: true,
				Schema:  &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &props},
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
				AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{
					conditionColumn("Accepted"),
					conditionColumn("Programmed"),
					{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
				},
			}},
		},
	}, nil
}

// FileName is the file a kind's CRD is written to.
func FileName(k v1alpha1.Kind) string {
	return k.Plural() + "." + v1alpha1.GroupVersion.Group + ".yaml"
}

// CRDYAML renders a kind's CRD manifest.
func CRDYAML(k v1alpha1.Kind, aisixSchema obj, aisixVersion string) ([]byte, error) {
	crd, err := CRD(k, aisixSchema)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(crd)
	if err != nil {
		return nil, err
	}
	var m obj
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	// Drop server-populated fields.
	delete(m, "status")
	if md, ok := m["metadata"].(obj); ok {
		delete(md, "creationTimestamp")
	}
	body, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# Generated by cmd/crdgen from api7/aisix@%s schemas. Do not edit.\n", aisixVersion)
	return append([]byte(header), body...), nil
}
