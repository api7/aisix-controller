package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/api7/aisix-controller/api/v1alpha1"
	"github.com/api7/aisix-controller/internal/validate"
)

// fakeValidator stands in for the aisix binary: it rejects an entry when
// reject returns a message, and reports dangling provider_key names the way
// the gateway does.
type fakeValidator struct {
	reject func(v1alpha1.Kind, Entry) string
	calls  int
}

func (f *fakeValidator) Validate(_ context.Context, doc []byte) ([]validate.LoadError, error) {
	f.calls++
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return []validate.LoadError{{Scope: "(file)", Message: err.Error()}}, nil
	}
	if root["_format_version"] != FormatVersion {
		return []validate.LoadError{{Scope: "(file)", Message: "bad _format_version"}}, nil
	}
	providers := map[string]bool{}
	for _, item := range list(root, "provider_keys") {
		providers[item[v1alpha1.IdentityField].(string)] = true
	}
	var errs []validate.LoadError
	for _, k := range v1alpha1.Kinds {
		for i, e := range list(root, k.Collection()) {
			scope := fmt.Sprintf("%s[%d] (%q)", k.Collection(), i, e[v1alpha1.IdentityField])
			if name, ok := e["provider_key"].(string); ok && !providers[name] {
				errs = append(errs, validate.LoadError{Scope: scope, Message: fmt.Sprintf("`provider_key` references unknown provider key %q", name)})
			}
			if f.reject != nil {
				if msg := f.reject(k, e); msg != "" {
					errs = append(errs, validate.LoadError{Scope: scope, Message: msg})
				}
			}
		}
	}
	return errs, nil
}

func list(root map[string]any, c string) []map[string]any {
	items, _ := root[c].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, i := range items {
		out = append(out, i.(map[string]any))
	}
	return out
}

// gatewayValidator prefers the real gateway pipeline.
func gatewayValidator(t *testing.T, fallback *fakeValidator) validate.Validator {
	if p := os.Getenv("AISIX_BIN"); p != "" {
		return &validate.Binary{Path: p}
	}
	if fallback == nil {
		t.Skip("needs the gateway pipeline; set AISIX_BIN to an aisix binary")
	}
	return fallback
}

func provider() Source {
	return src(v1alpha1.KindProviderKey, "mock", map[string]any{
		"provider": "openai", "adapter": "openai", "api_base": "http://mock:8080/v1",
		v1alpha1.APIKeySecretRef: ref("creds", "openai"),
	})
}

func model(name, providerRef string) Source {
	return src(v1alpha1.KindModel, name, map[string]any{
		"provider": "openai", "model_name": "upstream",
		v1alpha1.ProviderKeyRef: map[string]any{"name": providerRef},
	})
}

func apiKey() Source {
	return src(v1alpha1.KindAPIKey, "caller", map[string]any{
		"allowed_models": []any{"*"}, v1alpha1.KeySecretRef: ref("creds", "caller"),
	})
}

func assemble(t *testing.T, v validate.Validator, sources []Source, served []byte) Outcome {
	t.Helper()
	out, err := Assemble(context.Background(), All(sources, testSecrets()), ServedEntries(served), v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func disposition(o Outcome, kind v1alpha1.Kind, name string) Disposition {
	return o.Dispositions[Key{kind, name}]
}

func TestEmptyDocumentIsValid(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	out := assemble(t, v, nil, nil)
	if out.Error != "" || out.Document == nil {
		t.Fatalf("outcome = %+v", out)
	}
	if string(out.Document) != "{\n  \"_format_version\": \"1\"\n}\n" {
		t.Errorf("document = %q", out.Document)
	}
}

func TestValidResourcesProduceALoadableDocument(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	out := assemble(t, v, []Source{provider(), model("chat", "mock"), apiKey()}, nil)
	if out.Error != "" || out.Document == nil {
		t.Fatalf("outcome = %+v", out)
	}
	for k, d := range out.Dispositions {
		if d.State != Applied {
			t.Errorf("%s: %+v", k, d)
		}
	}
	if !bytes.Contains(out.Document, []byte(`"sk-$$abc"`)) {
		t.Errorf("credential not escaped in %s", out.Document)
	}
}

func TestGatewayRejectionIsAttributedToTheResource(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{reject: func(k v1alpha1.Kind, e Entry) string {
		if _, ok := e["no_such_field"]; ok {
			return "unknown field `no_such_field`"
		}
		return ""
	}})
	bad := model("bad", "mock")
	bad.Fields["no_such_field"] = 1
	out := assemble(t, v, []Source{provider(), model("good", "mock"), bad}, nil)
	if out.Error != "" || out.Document == nil {
		t.Fatalf("outcome = %+v", out)
	}
	if d := disposition(out, v1alpha1.KindModel, "bad"); d.State != Rejected || !strings.Contains(d.Reason, "no_such_field") {
		t.Errorf("bad: %+v", d)
	}
	if d := disposition(out, v1alpha1.KindModel, "good"); d.State != Applied {
		t.Errorf("good: %+v", d)
	}
	if bytes.Contains(out.Document, []byte("no_such_field")) {
		t.Error("rejected entry is in the document")
	}
}

func TestRejectedUpdateKeepsTheServedVersion(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{reject: func(k v1alpha1.Kind, e Entry) string {
		if _, ok := e["no_such_field"]; ok {
			return "unknown field `no_such_field`"
		}
		return ""
	}})
	first := assemble(t, v, []Source{provider(), model("chat", "mock")}, nil)
	edited := model("chat", "mock")
	edited.Fields["no_such_field"] = 1
	edited.Fields["model_name"] = "new"
	out := assemble(t, v, []Source{provider(), edited}, first.Document)
	if d := disposition(out, v1alpha1.KindModel, "chat"); d.State != Retained {
		t.Fatalf("chat: %+v", d)
	}
	if !bytes.Equal(out.Document, first.Document) {
		t.Errorf("served document changed:\n%s", out.Document)
	}
}

func TestDanglingReferenceCascadesToDependents(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	pk := provider()
	delete(pk.Fields, v1alpha1.APIKeySecretRef)
	out := assemble(t, v, []Source{pk, model("chat", "mock")}, nil)
	if d := disposition(out, v1alpha1.KindProviderKey, "mock"); d.State != Rejected {
		t.Errorf("provider: %+v", d)
	}
	if d := disposition(out, v1alpha1.KindModel, "chat"); d.State != Rejected || !strings.Contains(d.Reason, "mock") {
		t.Errorf("model: %+v", d)
	}
	if out.Document == nil {
		t.Error("no document")
	}
}

func TestCrossFieldRulesDroppedFromTheCRDAreStillEnforced(t *testing.T) {
	// A direct model that also routes passes the CRD schema; only the
	// gateway's oneOf refuses it.
	v := gatewayValidator(t, nil)
	m := model("chat", "mock")
	m.Fields["routing"] = map[string]any{"targets": []any{map[string]any{"model": "other"}}}
	out := assemble(t, v, []Source{provider(), m}, nil)
	if d := disposition(out, v1alpha1.KindModel, "chat"); d.State != Rejected {
		t.Errorf("chat: %+v", d)
	}
}

func TestDeletedResourcesLeaveTheDocument(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	first := assemble(t, v, []Source{provider(), model("chat", "mock"), apiKey()}, nil)
	out := assemble(t, v, []Source{provider(), model("chat", "mock")}, first.Document)
	if bytes.Contains(out.Document, []byte("api_keys")) {
		t.Errorf("deleted APIKey still served:\n%s", out.Document)
	}
}

func TestWholeDocumentErrorsAreNotAttributed(t *testing.T) {
	broken := validate.Func(func(ctx context.Context, doc []byte) ([]validate.LoadError, error) {
		return []validate.LoadError{{Scope: "(file)", Message: "boom"}}, nil
	})
	out, err := Assemble(context.Background(), All([]Source{provider()}, testSecrets()), Served{}, broken)
	if err != nil {
		t.Fatal(err)
	}
	if out.Document != nil || !strings.Contains(out.Error, "boom") {
		t.Errorf("outcome = %+v", out)
	}
}

func TestScopesParse(t *testing.T) {
	cases := map[string]struct {
		kind v1alpha1.Kind
		i    int
		ok   bool
	}{
		`models[2] ("gpt")`:   {v1alpha1.KindModel, 2, true},
		`provider_keys[0]`:    {v1alpha1.KindProviderKey, 0, true},
		`api_keys[11] ("a]")`: {v1alpha1.KindAPIKey, 11, true},
		`(file)`:              {"", 0, false},
		`guardrails[0]`:       {"", 0, false},
		`models[x]`:           {"", 0, false},
	}
	for scope, want := range cases {
		k, i, ok := ParseScope(scope)
		if k != want.kind || i != want.i || ok != want.ok {
			t.Errorf("%s: got (%s,%d,%v)", scope, k, i, ok)
		}
	}
}

func TestServedIdentitiesAreUnescaped(t *testing.T) {
	doc := Document(map[v1alpha1.Kind][]Entry{v1alpha1.KindModel: {{v1alpha1.IdentityField: "a$$b"}}})
	served := ServedEntries(doc)
	if _, ok := served.get(src(v1alpha1.KindModel, "x", map[string]any{v1alpha1.IdentityField: "a$b"})); !ok {
		t.Error("served entry not found by its literal identity")
	}
	if len(ServedEntries([]byte("not: json"))) != 0 {
		t.Error("unreadable document should count as nothing served")
	}
}
