// Package render turns custom resources and Secrets into entries of the
// gateway's resources file, and assembles them into one validated document.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/api7/aisix-controller/api/v1alpha1"
)

// Source is one custom resource, reduced to what rendering needs.
type Source struct {
	Kind       v1alpha1.Kind
	Name       string
	Generation int64
	// Created orders duplicate identities: the older resource wins.
	Created time.Time
	Fields  map[string]any
}

// Key identifies a custom resource.
type Key struct {
	Kind v1alpha1.Kind
	Name string
}

func (k Key) String() string { return fmt.Sprintf("%s/%s", k.Kind, k.Name) }

func (s Source) Key() Key { return Key{s.Kind, s.Name} }

// Identity is the entry's name in the resources file: display_name, or
// metadata.name when unset.
func (s Source) Identity() string {
	if v, ok := s.Fields[v1alpha1.IdentityField].(string); ok && v != "" {
		return v
	}
	return s.Name
}

// Secrets holds Secret data by Secret name, then key.
type Secrets map[string]map[string][]byte

// Entry is a rendered resources-file entry, already escaped for ${VAR}
// interpolation.
type Entry = map[string]any

// Rendered is one source and its entry, or the reason it has none.
type Rendered struct {
	Source Source
	Entry  Entry
	Err    string
}

// sortSources orders sources the way the file lists them: by kind, then
// creation time, then name.
func sortSources(sources []Source) []Source {
	out := append([]Source(nil), sources...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind.Order() != b.Kind.Order() {
			return a.Kind.Order() < b.Kind.Order()
		}
		if !a.Created.Equal(b.Created) {
			return a.Created.Before(b.Created)
		}
		return a.Name < b.Name
	})
	return out
}

// All renders every source, in file order. reserved names the Secret the
// resources file is written to; references to it are refused, since
// reading the generated file back as a credential would feed every write
// into the next one.
func All(sources []Source, secrets Secrets, reserved string) []Rendered {
	ordered := sortSources(sources)

	type identity struct {
		kind v1alpha1.Kind
		id   string
	}
	// The oldest resource wins a shared identity; the rest are not served.
	owners := map[identity]string{}
	for _, s := range ordered {
		id := identity{s.Kind, s.Identity()}
		if _, taken := owners[id]; !taken {
			owners[id] = s.Name
		}
	}
	owner := func(s Source) string { return owners[identity{s.Kind, s.Identity()}] }

	// Model references resolve only to ProviderKeys that are served.
	providers := map[string]providerRef{}
	for _, s := range ordered {
		if s.Kind == v1alpha1.KindProviderKey {
			providers[s.Name] = providerRef{identity: s.Identity(), owner: owner(s)}
		}
	}

	out := make([]Rendered, 0, len(ordered))
	for _, s := range ordered {
		r := Rendered{Source: s}
		if o := owner(s); o != s.Name {
			r.Err = fmt.Sprintf("%s %q is already used by %s/%s", v1alpha1.IdentityField, s.Identity(), s.Kind, o)
		} else {
			entry, err := one(s, secrets, reserved, providers)
			if err != nil {
				r.Err = err.Error()
			} else {
				r.Entry = entry
			}
		}
		out = append(out, r)
	}
	return out
}

// providerRef is what a Model's provider_key_ref resolves through.
type providerRef struct {
	identity string
	// owner is the ProviderKey that holds the identity in the file.
	owner string
}

func one(s Source, secrets Secrets, reserved string, providers map[string]providerRef) (Entry, error) {
	fields := make(Entry, len(s.Fields)+1)
	for k, v := range s.Fields {
		fields[k] = v
	}
	fields[v1alpha1.IdentityField] = s.Identity()

	switch s.Kind {
	case v1alpha1.KindProviderKey:
		for _, inline := range []string{"api_key", "secret"} {
			if _, ok := fields[inline]; ok {
				return nil, fmt.Errorf("set the credential through %s, not `%s`", v1alpha1.APIKeySecretRef, inline)
			}
		}
		ref, ok := fields[v1alpha1.APIKeySecretRef]
		if !ok {
			return nil, fmt.Errorf("%s is required", v1alpha1.APIKeySecretRef)
		}
		delete(fields, v1alpha1.APIKeySecretRef)
		value, err := secretValue(ref, secrets, reserved, v1alpha1.APIKeySecretRef)
		if err != nil {
			return nil, err
		}
		fields["api_key"] = value
	case v1alpha1.KindModel:
		if _, ok := fields["provider_key_id"]; ok {
			return nil, fmt.Errorf("reference the provider key through %s", v1alpha1.ProviderKeyRef)
		}
		if ref, ok := fields[v1alpha1.ProviderKeyRef]; ok {
			delete(fields, v1alpha1.ProviderKeyRef)
			m, _ := ref.(map[string]any)
			name, _ := m["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("%s.name is required", v1alpha1.ProviderKeyRef)
			}
			p, ok := providers[name]
			if !ok {
				return nil, fmt.Errorf("%s names ProviderKey %q, which does not exist", v1alpha1.ProviderKeyRef, name)
			}
			if p.owner != name {
				return nil, fmt.Errorf("%s names ProviderKey %q, which is not served: %s %q is already used by ProviderKey/%s",
					v1alpha1.ProviderKeyRef, name, v1alpha1.IdentityField, p.identity, p.owner)
			}
			fields["provider_key"] = p.identity
		}
	case v1alpha1.KindAPIKey:
		if _, ok := fields["key_hash"]; ok {
			return nil, fmt.Errorf("set the key through %s, not `key_hash`", v1alpha1.KeySecretRef)
		}
		ref, ok := fields[v1alpha1.KeySecretRef]
		if !ok {
			return nil, fmt.Errorf("%s is required", v1alpha1.KeySecretRef)
		}
		delete(fields, v1alpha1.KeySecretRef)
		plaintext, err := secretValue(ref, secrets, reserved, v1alpha1.KeySecretRef)
		if err != nil {
			return nil, err
		}
		fields["key_hash"] = HashKey(plaintext)
	}

	return EscapeDollars(fields).(Entry), nil
}

// HashKey is how the gateway stores a caller key: SHA-256, lowercase hex
// (aisix ApiKey::hash_bearer).
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func secretValue(ref any, secrets Secrets, reserved, field string) (string, error) {
	m, _ := ref.(map[string]any)
	name, _ := m["name"].(string)
	key, _ := m["key"].(string)
	if name == "" {
		return "", fmt.Errorf("%s.name is required", field)
	}
	if key == "" {
		return "", fmt.Errorf("%s.key is required", field)
	}
	if reserved != "" && name == reserved {
		return "", fmt.Errorf("%s: Secret %q holds the generated resources file and cannot be referenced", field, name)
	}
	data, ok := secrets[name]
	if !ok {
		return "", fmt.Errorf("%s: Secret %q not found", field, name)
	}
	raw, ok := data[key]
	if !ok {
		return "", fmt.Errorf("%s: Secret %q has no key %q", field, name, key)
	}
	value := strings.TrimRight(string(raw), "\r\n")
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%s: Secret %q key %q is not UTF-8", field, name, key)
	}
	if value == "" {
		return "", fmt.Errorf("%s: Secret %q key %q is empty", field, name, key)
	}
	return value, nil
}

// EscapeDollars writes every `$` as `$$`. The resources file interpolates
// ${VAR} in every string scalar, and values from custom resources are
// literal.
func EscapeDollars(v any) any {
	switch n := v.(type) {
	case string:
		return strings.ReplaceAll(n, "$", "$$")
	case []any:
		out := make([]any, len(n))
		for i, x := range n {
			out[i] = EscapeDollars(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, x := range n {
			out[k] = EscapeDollars(x)
		}
		return out
	}
	return v
}
