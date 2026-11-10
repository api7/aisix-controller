package render

import (
	"strings"
	"testing"
	"time"

	"github.com/api7/aisix-controller/api/v1alpha1"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func src(kind v1alpha1.Kind, name string, fields map[string]any) Source {
	return Source{Kind: kind, Name: name, Generation: 1, Created: t0, Fields: fields}
}

func testSecrets() Secrets {
	return Secrets{"creds": {
		"openai": []byte("sk-$abc\n"),
		"caller": []byte("caller-key"),
		"empty":  {},
	}}
}

func ref(name, key string) map[string]any { return map[string]any{"name": name, "key": key} }

func byKey(rs []Rendered) map[string]Rendered {
	out := map[string]Rendered{}
	for _, r := range rs {
		out[r.Source.Key().String()] = r
	}
	return out
}

func TestProviderKeyTakesCredentialFromSecretAndEscapesIt(t *testing.T) {
	out := byKey(All([]Source{src(v1alpha1.KindProviderKey, "openai", map[string]any{
		"provider":               "openai",
		"api_base":               "http://x/${not_env}",
		v1alpha1.APIKeySecretRef: ref("creds", "openai"),
	})}, testSecrets(), ""))
	r := out["ProviderKey/openai"]
	if r.Err != "" {
		t.Fatal(r.Err)
	}
	if r.Entry["api_key"] != "sk-$$abc" {
		t.Errorf("api_key = %v", r.Entry["api_key"])
	}
	if r.Entry["api_base"] != "http://x/$${not_env}" {
		t.Errorf("api_base = %v", r.Entry["api_base"])
	}
	if r.Entry[v1alpha1.IdentityField] != "openai" {
		t.Errorf("display_name = %v", r.Entry[v1alpha1.IdentityField])
	}
	if _, ok := r.Entry[v1alpha1.APIKeySecretRef]; ok {
		t.Error("secret ref leaked into the entry")
	}
}

func TestModelReferenceResolvesToProviderIdentity(t *testing.T) {
	out := byKey(All([]Source{
		src(v1alpha1.KindProviderKey, "pk", map[string]any{
			v1alpha1.IdentityField:   "Provider One",
			v1alpha1.APIKeySecretRef: ref("creds", "openai"),
		}),
		src(v1alpha1.KindModel, "m", map[string]any{
			v1alpha1.IdentityField:  "openai/gpt",
			v1alpha1.ProviderKeyRef: map[string]any{"name": "pk"},
		}),
	}, testSecrets(), ""))
	m := out["Model/m"]
	if m.Err != "" {
		t.Fatal(m.Err)
	}
	if m.Entry["provider_key"] != "Provider One" || m.Entry[v1alpha1.IdentityField] != "openai/gpt" {
		t.Errorf("entry = %v", m.Entry)
	}
}

func TestAPIKeyIsHashed(t *testing.T) {
	out := byKey(All([]Source{src(v1alpha1.KindAPIKey, "k", map[string]any{
		v1alpha1.KeySecretRef: ref("creds", "caller"),
	})}, testSecrets(), ""))
	k := out["APIKey/k"]
	if k.Err != "" {
		t.Fatal(k.Err)
	}
	if k.Entry["key_hash"] != HashKey("caller-key") {
		t.Errorf("key_hash = %v", k.Entry["key_hash"])
	}
	// sha256("password"), as the gateway computes it.
	if HashKey("password") != "5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8" {
		t.Error("HashKey does not match the gateway")
	}
	if strings.Contains(k.Entry["key_hash"].(string), "caller-key") {
		t.Error("plaintext leaked")
	}
}

func TestMissingReferencesAreErrors(t *testing.T) {
	out := byKey(All([]Source{
		src(v1alpha1.KindProviderKey, "no-secret", map[string]any{v1alpha1.APIKeySecretRef: ref("nope", "x")}),
		src(v1alpha1.KindProviderKey, "no-key", map[string]any{v1alpha1.APIKeySecretRef: ref("creds", "nope")}),
		src(v1alpha1.KindProviderKey, "empty", map[string]any{v1alpha1.APIKeySecretRef: ref("creds", "empty")}),
		src(v1alpha1.KindProviderKey, "unset", map[string]any{}),
		src(v1alpha1.KindModel, "dangling", map[string]any{v1alpha1.ProviderKeyRef: map[string]any{"name": "ghost"}}),
		src(v1alpha1.KindAPIKey, "keyless", map[string]any{}),
	}, testSecrets(), ""))
	want := map[string]string{
		"ProviderKey/no-secret": `Secret "nope" not found`,
		"ProviderKey/no-key":    `has no key "nope"`,
		"ProviderKey/empty":     "is empty",
		"ProviderKey/unset":     "api_key_secret_ref is required",
		"Model/dangling":        `ProviderKey "ghost", which does not exist`,
		"APIKey/keyless":        "key_secret_ref is required",
	}
	for k, substr := range want {
		if !strings.Contains(out[k].Err, substr) {
			t.Errorf("%s: err %q lacks %q", k, out[k].Err, substr)
		}
	}
}

func TestInlineCredentialsAreRefused(t *testing.T) {
	out := byKey(All([]Source{
		src(v1alpha1.KindProviderKey, "a", map[string]any{"api_key": "x", v1alpha1.APIKeySecretRef: ref("creds", "openai")}),
		src(v1alpha1.KindProviderKey, "b", map[string]any{"secret": "x", v1alpha1.APIKeySecretRef: ref("creds", "openai")}),
		src(v1alpha1.KindModel, "c", map[string]any{"provider_key_id": "x"}),
		src(v1alpha1.KindAPIKey, "d", map[string]any{"key_hash": "x", v1alpha1.KeySecretRef: ref("creds", "caller")}),
	}, testSecrets(), ""))
	for _, k := range []string{"ProviderKey/a", "ProviderKey/b", "Model/c", "APIKey/d"} {
		if out[k].Err == "" {
			t.Errorf("%s was accepted", k)
		}
	}
}

func TestDuplicateIdentityLosesToOlderResource(t *testing.T) {
	older := src(v1alpha1.KindModel, "zzz", map[string]any{v1alpha1.IdentityField: "same"})
	newer := src(v1alpha1.KindModel, "aaa", map[string]any{v1alpha1.IdentityField: "same"})
	newer.Created = t0.Add(time.Minute)
	out := byKey(All([]Source{newer, older}, testSecrets(), ""))
	if out["Model/zzz"].Err != "" {
		t.Errorf("older lost: %s", out["Model/zzz"].Err)
	}
	if !strings.Contains(out["Model/aaa"].Err, "already used by Model/zzz") {
		t.Errorf("newer: %q", out["Model/aaa"].Err)
	}
}

func TestOrderIsKindThenAgeThenName(t *testing.T) {
	b := src(v1alpha1.KindModel, "b", map[string]any{})
	a := src(v1alpha1.KindModel, "a", map[string]any{})
	late := src(v1alpha1.KindModel, "0", map[string]any{})
	late.Created = t0.Add(time.Hour)
	key := src(v1alpha1.KindAPIKey, "k", map[string]any{})
	pk := src(v1alpha1.KindProviderKey, "p", map[string]any{})
	var got []string
	for _, r := range All([]Source{key, late, b, a, pk}, testSecrets(), "") {
		got = append(got, r.Source.Key().String())
	}
	want := "ProviderKey/p Model/a Model/b Model/0 APIKey/k"
	if strings.Join(got, " ") != want {
		t.Errorf("order = %v", got)
	}
}

func TestEscapeDollarsReachesEveryString(t *testing.T) {
	in := map[string]any{"a": "$", "b": []any{"x$y", 1}, "c": map[string]any{"d": "$$"}}
	out := EscapeDollars(in).(map[string]any)
	if out["a"] != "$$" || out["b"].([]any)[0] != "x$$y" || out["c"].(map[string]any)["d"] != "$$$$" {
		t.Errorf("escaped = %v", out)
	}
	if in["a"] != "$" {
		t.Error("input was mutated")
	}
}

func TestReferencesToTheResourcesSecretAreRefused(t *testing.T) {
	secrets := testSecrets()
	secrets["aisix-resources"] = map[string][]byte{"resources.yaml": []byte(`{"_format_version": "1"}`)}
	out := byKey(All([]Source{
		src(v1alpha1.KindAPIKey, "loop", map[string]any{v1alpha1.KeySecretRef: ref("aisix-resources", "resources.yaml")}),
		src(v1alpha1.KindProviderKey, "grow", map[string]any{v1alpha1.APIKeySecretRef: ref("aisix-resources", "other")}),
		src(v1alpha1.KindAPIKey, "fine", map[string]any{v1alpha1.KeySecretRef: ref("creds", "caller")}),
	}, secrets, "aisix-resources"))
	for _, k := range []string{"APIKey/loop", "ProviderKey/grow"} {
		if !strings.Contains(out[k].Err, "holds the generated resources file") {
			t.Errorf("%s: err = %q", k, out[k].Err)
		}
	}
	if out["APIKey/fine"].Err != "" {
		t.Errorf("unrelated reference refused: %s", out["APIKey/fine"].Err)
	}
}

func TestModelCannotReferenceALosingProviderKey(t *testing.T) {
	prod := src(v1alpha1.KindProviderKey, "openai-prod", map[string]any{
		v1alpha1.IdentityField: "openai", v1alpha1.APIKeySecretRef: ref("creds", "openai"),
	})
	test := src(v1alpha1.KindProviderKey, "openai-test", map[string]any{
		v1alpha1.IdentityField: "openai", v1alpha1.APIKeySecretRef: ref("creds", "caller"),
	})
	test.Created = t0.Add(time.Minute)
	out := byKey(All([]Source{
		test, prod,
		src(v1alpha1.KindModel, "cheap", map[string]any{v1alpha1.ProviderKeyRef: map[string]any{"name": "openai-test"}}),
		src(v1alpha1.KindModel, "main", map[string]any{v1alpha1.ProviderKeyRef: map[string]any{"name": "openai-prod"}}),
	}, testSecrets(), ""))

	cheap := out["Model/cheap"]
	if cheap.Entry != nil || !strings.Contains(cheap.Err, `ProviderKey "openai-test", which is not served`) ||
		!strings.Contains(cheap.Err, "ProviderKey/openai-prod") {
		t.Errorf("cheap: entry=%v err=%q", cheap.Entry, cheap.Err)
	}
	if m := out["Model/main"]; m.Err != "" || m.Entry["provider_key"] != "openai" {
		t.Errorf("main: entry=%v err=%q", m.Entry, m.Err)
	}
}
