package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/api7/aisix-controller/api/v1alpha1"
)

const vendored = "../../third_party/aisix"

func specSchema(t *testing.T, k v1alpha1.Kind) obj {
	t.Helper()
	s, err := LoadSchema(filepath.Join(vendored, "schemas"), k)
	if err != nil {
		t.Fatal(err)
	}
	return ResourceSchema(k, s)["properties"].(obj)["spec"].(obj)
}

func walk(node obj, path string, f func(string, obj)) {
	f(path, node)
	if p, ok := node["properties"].(obj); ok {
		for k, v := range p {
			walk(v.(obj), path+"."+k, f)
		}
	}
	if i, ok := node["items"].(obj); ok {
		walk(i, path+"[]", f)
	}
	if a, ok := node["additionalProperties"].(obj); ok {
		walk(a, path+"{}", f)
	}
}

// assertStructural checks the apiextensions structural rules a generator can
// break.
func assertStructural(t *testing.T, s obj) {
	t.Helper()
	forbidden := []string{"$ref", "$schema", "definitions", "oneOf", "anyOf", "allOf", "not",
		"if", "then", "else", "const", "default", "title", "propertyNames",
		"patternProperties", "uniqueItems", "examples"}
	walk(s, "spec", func(path string, m obj) {
		for _, k := range forbidden {
			if _, ok := m[k]; ok {
				t.Errorf("%s: forbidden keyword %s", path, k)
			}
		}
		open := m[preserveKey] == true || m[intOrStringKey] == true
		if _, ok := m["type"]; !ok && !open {
			t.Errorf("%s: node without type", path)
		}
		props, hasProps := m["properties"].(obj)
		_, hasAdditional := m["additionalProperties"]
		if hasProps && hasAdditional {
			t.Errorf("%s: properties with additionalProperties", path)
		}
		if hasProps && m["type"] != "object" {
			t.Errorf("%s: properties on a non-object", path)
		}
		// An object with nothing declared prunes every key it is given.
		if m["type"] == "object" && !open && len(props) == 0 && !hasAdditional {
			t.Errorf("%s: object that prunes all of its fields", path)
		}
		if m["type"] == "array" {
			if _, ok := m["items"]; !ok {
				t.Errorf("%s: array without items", path)
			}
		}
		if _, ok := m["additionalProperties"].(bool); ok {
			t.Errorf("%s: boolean additionalProperties", path)
		}
		if ex, ok := m["exclusiveMinimum"]; ok {
			if _, isBool := ex.(bool); !isBool {
				t.Errorf("%s: numeric exclusiveMinimum", path)
			}
		}
	})
}

func hasPropertyAnywhere(s obj, name string) bool {
	found := false
	walk(s, "", func(_ string, m obj) {
		if p, ok := m["properties"].(obj); ok {
			if _, ok := p[name]; ok {
				found = true
			}
		}
	})
	return found
}

func required(s obj) []any {
	r, _ := s["required"].([]any)
	return r
}

func has(list []any, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func TestEverySpecIsStructural(t *testing.T) {
	for _, k := range v1alpha1.Kinds {
		t.Run(string(k), func(t *testing.T) { assertStructural(t, specSchema(t, k)) })
	}
}

func TestCredentialsNeverAppearInline(t *testing.T) {
	pk := specSchema(t, v1alpha1.KindProviderKey)
	for _, f := range []string{"api_key", "secret"} {
		if hasPropertyAnywhere(pk, f) {
			t.Errorf("ProviderKey still has %s", f)
		}
	}
	if !has(required(pk), v1alpha1.APIKeySecretRef) {
		t.Errorf("ProviderKey does not require %s", v1alpha1.APIKeySecretRef)
	}

	key := specSchema(t, v1alpha1.KindAPIKey)
	if hasPropertyAnywhere(key, "key_hash") {
		t.Error("APIKey still has key_hash")
	}
	if !has(required(key), v1alpha1.KeySecretRef) {
		t.Errorf("APIKey does not require %s", v1alpha1.KeySecretRef)
	}
}

func TestIDFormReferencesAreRemoved(t *testing.T) {
	model := specSchema(t, v1alpha1.KindModel)
	for _, f := range []string{"provider_key_id", "model_id", "target_id", "embedding_model_id", "default_id"} {
		if hasPropertyAnywhere(model, f) {
			t.Errorf("Model still has %s", f)
		}
	}
	if _, ok := model["properties"].(obj)[v1alpha1.ProviderKeyRef]; !ok {
		t.Errorf("Model lacks %s", v1alpha1.ProviderKeyRef)
	}
	if !hasPropertyAnywhere(model, "routing") {
		t.Error("Model lost routing")
	}

	key := specSchema(t, v1alpha1.KindAPIKey)
	for _, f := range []string{"allowed_model_ids", "mcp_rate_limits_by_id", "allow_ids", "deny_ids"} {
		if hasPropertyAnywhere(key, f) {
			t.Errorf("APIKey still has %s", f)
		}
	}
	if !hasPropertyAnywhere(key, "allowed_models") {
		t.Error("APIKey lost allowed_models")
	}
}

func TestClassifierHasAProviderKeyRef(t *testing.T) {
	classifier := specSchema(t, v1alpha1.KindModel)
	for _, p := range v1alpha1.ProviderKeyRefPaths[1] {
		classifier = classifier["properties"].(obj)[p].(obj)
	}
	if _, ok := classifier["properties"].(obj)[v1alpha1.ProviderKeyRef]; !ok {
		t.Fatalf("classifier lacks %s", v1alpha1.ProviderKeyRef)
	}
	if !has(required(classifier), v1alpha1.ProviderKeyRef) {
		t.Errorf("classifier does not require %s", v1alpha1.ProviderKeyRef)
	}
	if has(required(specSchema(t, v1alpha1.KindModel)), v1alpha1.ProviderKeyRef) {
		t.Errorf("Model requires a root %s", v1alpha1.ProviderKeyRef)
	}
}

func TestIdentityIsOptional(t *testing.T) {
	for _, k := range v1alpha1.Kinds {
		s := specSchema(t, k)
		if has(required(s), v1alpha1.IdentityField) {
			t.Errorf("%s requires %s", k, v1alpha1.IdentityField)
		}
		if _, ok := s["properties"].(obj)[v1alpha1.IdentityField]; !ok {
			t.Errorf("%s lacks %s", k, v1alpha1.IdentityField)
		}
	}
}

func decode(t *testing.T, s string) obj {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var out obj
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestConversionRules(t *testing.T) {
	root := decode(t, `{
		"definitions": {"Inner": {"type": "object", "properties": {"a": {"type": "integer", "minimum": 0.0}}}},
		"type": "object",
		"properties": {
			"opt": {"type": ["string", "null"]},
			"inner": {"anyOf": [{"$ref": "#/definitions/Inner"}, {"type": "null"}]},
			"wrapped": {"allOf": [{"$ref": "#/definitions/Inner"}], "description": "kept"},
			"mixed": {"anyOf": [{"type": "string"}, {"type": "object", "properties": {"x": {"type": "string"}}}]},
			"ios": {"type": ["integer", "string"]},
			"c": {"const": "fixed"},
			"map": {"type": "object", "additionalProperties": {"type": "string"}},
			"empty_key_map": {"type": "object", "properties": {"": {"type": "string"}}, "additionalProperties": {"type": "string"}},
			"free": {"type": "object"},
			"ex": {"type": "number", "exclusiveMinimum": 0.0}
		}
	}`)
	s := ToStructural(root)
	p := s["properties"].(obj)
	cases := map[string]string{
		"opt":           `{"nullable":true,"type":"string"}`,
		"c":             `{"enum":["fixed"],"type":"string"}`,
		"map":           `{"additionalProperties":{"type":"string"},"type":"object"}`,
		"empty_key_map": `{"additionalProperties":{"type":"string"},"type":"object"}`,
		"free":          fmt.Sprintf(`{"type":"object","%s":true}`, preserveKey),
		"mixed":         fmt.Sprintf(`{"%s":true}`, preserveKey),
		"ios":           fmt.Sprintf(`{"%s":true}`, intOrStringKey),
	}
	for field, want := range cases {
		if got := asJSON(p[field]); got != want {
			t.Errorf("%s: got %s, want %s", field, got, want)
		}
	}
	inner := p["inner"].(obj)
	if inner["nullable"] != true || inner["properties"].(obj)["a"].(obj)["type"] != "integer" {
		t.Errorf("inner: %s", asJSON(inner))
	}
	wrapped := p["wrapped"].(obj)
	if wrapped["description"] != "kept" || asJSON(wrapped["properties"].(obj)["a"].(obj)["minimum"]) != "0.0" {
		t.Errorf("wrapped: %s", asJSON(wrapped))
	}
	if ex := p["ex"].(obj); ex["exclusiveMinimum"] != true {
		t.Errorf("ex: %s", asJSON(ex))
	}
	assertStructural(t, s)
}

func TestCommittedCRDsMatchGenerator(t *testing.T) {
	files, err := Generate(vendored)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		path := filepath.Join("../../config/crd", f.Name)
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v; run `make crds`", path, err)
		}
		if !bytes.Equal(committed, f.Body) {
			t.Errorf("%s is stale; run `make crds`", path)
		}
	}
}
