package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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

// assemble builds a document, given what the previous outcome serves.
func assemble(t *testing.T, v validate.Validator, sources []Source, prev *Outcome) Outcome {
	t.Helper()
	served := ServedEntries(nil, nil)
	if prev != nil {
		served = ServedEntries(prev.Document, prev.Index)
	}
	out, err := Assemble(context.Background(), All(sources, testSecrets(), ""), served, v)
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
	out := assemble(t, v, []Source{provider(), edited}, &first)
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
	out := assemble(t, v, []Source{provider(), model("chat", "mock")}, &first)
	if bytes.Contains(out.Document, []byte("api_keys")) {
		t.Errorf("deleted APIKey still served:\n%s", out.Document)
	}
}

func TestWholeDocumentErrorsAreNotAttributed(t *testing.T) {
	broken := validate.Func(func(ctx context.Context, doc []byte) ([]validate.LoadError, error) {
		return []validate.LoadError{{Scope: "(file)", Message: "boom"}}, nil
	})
	out, err := Assemble(context.Background(), All([]Source{provider()}, testSecrets(), ""), Served{}, broken)
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

func rejectUnknownField(_ v1alpha1.Kind, e Entry) string {
	if _, ok := e["no_such_field"]; ok {
		return "unknown field `no_such_field`"
	}
	return ""
}

func TestRenamedAndRejectedKeepsTheServedEntry(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{reject: rejectUnknownField})
	first := assemble(t, v, []Source{provider(), model("chat", "mock")}, nil)
	if string(first.Index) != `{"Model/chat":"chat","ProviderKey/mock":"mock"}`+"\n" {
		t.Fatalf("index = %s", first.Index)
	}
	edited := model("chat", "mock")
	edited.Fields[v1alpha1.IdentityField] = "renamed"
	edited.Fields["no_such_field"] = 1
	out := assemble(t, v, []Source{provider(), edited}, &first)
	if d := disposition(out, v1alpha1.KindModel, "chat"); d.State != Retained {
		t.Fatalf("chat: %+v", d)
	}
	if !bytes.Equal(out.Document, first.Document) || !bytes.Equal(out.Index, first.Index) {
		t.Errorf("served state changed:\n%s\n%s", out.Document, out.Index)
	}
}

func TestServedEntriesBelongToTheirResource(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{reject: rejectUnknownField})
	old := model("old", "mock")
	old.Fields[v1alpha1.IdentityField] = "shared"
	first := assemble(t, v, []Source{provider(), old}, nil)

	// A new resource under the same identity must not inherit old's entry.
	newer := model("new", "mock")
	newer.Fields[v1alpha1.IdentityField] = "shared"
	newer.Fields["no_such_field"] = 1
	out := assemble(t, v, []Source{provider(), newer}, &first)
	if d := disposition(out, v1alpha1.KindModel, "new"); d.State != Rejected {
		t.Errorf("new: %+v", d)
	}
	if bytes.Contains(out.Document, []byte(`"shared"`)) {
		t.Errorf("the deleted resource's entry is still served:\n%s", out.Document)
	}
}

func TestServedEntriesParse(t *testing.T) {
	doc := Document(map[v1alpha1.Kind][]Entry{v1alpha1.KindModel: {{v1alpha1.IdentityField: "a$$b"}}})
	served := ServedEntries(doc, Index(map[string]string{"Model/x": "a$b"}))
	if _, id, ok := served.get(src(v1alpha1.KindModel, "x", map[string]any{v1alpha1.IdentityField: "renamed"})); !ok || id != "a$b" {
		t.Errorf("served entry not found through the index: %q %v", id, ok)
	}
	if _, _, ok := served.get(src(v1alpha1.KindModel, "y", map[string]any{v1alpha1.IdentityField: "a$b"})); ok {
		t.Error("an unindexed resource found an entry by identity")
	}
	for _, bad := range [][2][]byte{{[]byte("not: json"), Index(nil)}, {doc, []byte("nope")}, {doc, nil}} {
		if _, _, ok := ServedEntries(bad[0], bad[1]).get(src(v1alpha1.KindModel, "x", nil)); ok {
			t.Errorf("unreadable input counted as served: %q %q", bad[0], bad[1])
		}
	}
}

const (
	credOne = "sk-$$abc"
	credTwo = "caller-key"
)

// namedProvider is a ProviderKey; secretKey picks credOne ("openai"),
// credTwo ("caller"), or a missing key.
func namedProvider(name, display, secretKey string, age time.Duration) Source {
	s := src(v1alpha1.KindProviderKey, name, map[string]any{
		"provider": "openai", "adapter": "openai", "api_base": "http://" + name + ":8080/v1",
		v1alpha1.IdentityField: display, v1alpha1.APIKeySecretRef: ref("creds", secretKey),
	})
	s.Created = t0.Add(age)
	return s
}

func invalid(s Source) Source {
	s.Fields["no_such_field"] = 1
	return s
}

func entries(t *testing.T, o Outcome, collection string) map[string]map[string]any {
	t.Helper()
	if o.Error != "" || o.Document == nil {
		t.Fatalf("outcome = %+v", o)
	}
	var root map[string]any
	if err := json.Unmarshal(o.Document, &root); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, e := range list(root, collection) {
		out[e[v1alpha1.IdentityField].(string)] = e
	}
	return out
}

// wantModel checks which provider key a Model is served on, and that the
// provider key carries the expected credential.
func wantModel(t *testing.T, o Outcome, model, providerKey, credential string) {
	t.Helper()
	m, ok := entries(t, o, "models")[model]
	if !ok {
		t.Fatalf("model %s is not served:\n%s", model, o.Document)
	}
	if m["provider_key"] != providerKey {
		t.Errorf("model %s: provider_key = %v, want %s", model, m["provider_key"], providerKey)
	}
	p, ok := entries(t, o, "provider_keys")[providerKey]
	if !ok {
		t.Fatalf("provider key %s is not served:\n%s", providerKey, o.Document)
	}
	if p["api_key"] != credential {
		t.Errorf("provider key %s: api_key = %v, want %s", providerKey, p["api_key"], credential)
	}
}

func wantState(t *testing.T, o Outcome, kind v1alpha1.Kind, name string, state State, reason string) {
	t.Helper()
	d := disposition(o, kind, name)
	if d.State != state || !strings.Contains(d.Reason, reason) {
		t.Errorf("%s/%s: %+v, want state %d with reason %q", kind, name, d, state, reason)
	}
}

func TestFailedRenameKeepsModelsOnTheServedName(t *testing.T) {
	for label, edited := range map[string]Source{
		"at render":      namedProvider("p", "B", "typo", 0),
		"by the gateway": invalid(namedProvider("p", "B", "openai", 0)),
	} {
		t.Run(label, func(t *testing.T) {
			v := gatewayValidator(t, &fakeValidator{reject: rejectUnknownField})
			first := assemble(t, v, []Source{namedProvider("p", "A", "openai", 0), model("m", "p")}, nil)
			out := assemble(t, v, []Source{edited, model("m", "p"), model("m2", "p")}, &first)
			wantState(t, out, v1alpha1.KindProviderKey, "p", Retained, "")
			wantState(t, out, v1alpha1.KindModel, "m", Applied, "")
			wantState(t, out, v1alpha1.KindModel, "m2", Applied, "")
			wantModel(t, out, "m", "A", credOne)
			wantModel(t, out, "m2", "A", credOne)
		})
	}
}

func TestRenameOntoAServedNameFails(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	second := namedProvider("p2", "B", "caller", time.Hour)
	first := assemble(t, v, []Source{namedProvider("p1", "A", "openai", 0), second, model("m1", "p1"), model("m2", "p2")}, nil)
	out := assemble(t, v, []Source{namedProvider("p1", "B", "openai", 0), second, model("m1", "p1"), model("m2", "p2")}, &first)
	wantState(t, out, v1alpha1.KindProviderKey, "p1", Retained, "already used by ProviderKey/p2")
	wantState(t, out, v1alpha1.KindProviderKey, "p2", Applied, "")
	wantState(t, out, v1alpha1.KindModel, "m1", Applied, "")
	wantState(t, out, v1alpha1.KindModel, "m2", Applied, "")
	wantModel(t, out, "m1", "A", credOne)
	wantModel(t, out, "m2", "B", credTwo)
	if !bytes.Equal(out.Document, first.Document) {
		t.Errorf("served document changed:\n%s", out.Document)
	}
}

func TestRenameOntoARetainedNameFails(t *testing.T) {
	failures := map[string]func(Source) Source{
		"at render": func(s Source) Source {
			s.Fields[v1alpha1.APIKeySecretRef] = ref("creds", "typo")
			return s
		},
		"by the gateway": invalid,
	}
	ages := map[string][2]time.Duration{
		"holder older":   {time.Hour, 0},
		"claimant older": {0, time.Hour},
	}
	for failure, fail := range failures {
		for age, a := range ages {
			t.Run(failure+"/"+age, func(t *testing.T) {
				v := gatewayValidator(t, &fakeValidator{reject: rejectUnknownField})
				first := assemble(t, v, []Source{
					namedProvider("p1", "A", "openai", a[0]), namedProvider("p2", "B", "caller", a[1]), model("m1", "p1"),
				}, nil)
				out := assemble(t, v, []Source{
					namedProvider("p1", "B", "openai", a[0]), fail(namedProvider("p2", "C", "caller", a[1])), model("m1", "p1"),
				}, &first)
				wantState(t, out, v1alpha1.KindProviderKey, "p2", Retained, "")
				wantState(t, out, v1alpha1.KindProviderKey, "p1", Retained, "already used by ProviderKey/p2")
				wantState(t, out, v1alpha1.KindModel, "m1", Applied, "")
				wantModel(t, out, "m1", "A", credOne)
				if _, ok := entries(t, out, "provider_keys")["B"]; !ok {
					t.Errorf("p2's entry is gone:\n%s", out.Document)
				}
			})
		}
	}
}

func TestSwappedNamesBothApply(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	first := assemble(t, v, []Source{
		namedProvider("p1", "A", "openai", 0), namedProvider("p2", "B", "caller", time.Hour), model("m1", "p1"), model("m2", "p2"),
	}, nil)
	out := assemble(t, v, []Source{
		namedProvider("p1", "B", "openai", 0), namedProvider("p2", "A", "caller", time.Hour), model("m1", "p1"), model("m2", "p2"),
	}, &first)
	wantState(t, out, v1alpha1.KindProviderKey, "p1", Applied, "")
	wantState(t, out, v1alpha1.KindProviderKey, "p2", Applied, "")
	wantModel(t, out, "m1", "B", credOne)
	wantModel(t, out, "m2", "A", credTwo)
}

func TestFallingBackReclaimsTheOldName(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	holder := namedProvider("p2", "B", "caller", time.Hour)
	first := assemble(t, v, []Source{namedProvider("p1", "A", "openai", 0), holder, namedProvider("p3", "X", "caller", 2*time.Hour)}, nil)
	out := assemble(t, v, []Source{namedProvider("p1", "B", "openai", 0), holder, namedProvider("p3", "A", "caller", 2*time.Hour)}, &first)
	wantState(t, out, v1alpha1.KindProviderKey, "p1", Retained, "already used by ProviderKey/p2")
	wantState(t, out, v1alpha1.KindProviderKey, "p3", Retained, "already used by ProviderKey/p1")
	if !bytes.Equal(out.Document, first.Document) {
		t.Errorf("served document changed:\n%s", out.Document)
	}
}

func TestServedModelKeepsItsNameAgainstAnOlderModel(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	older := model("older", "mock")
	older.Fields[v1alpha1.IdentityField] = "one"
	newer := model("newer", "mock")
	newer.Fields[v1alpha1.IdentityField] = "two"
	newer.Created = t0.Add(time.Hour)
	first := assemble(t, v, []Source{provider(), older, newer}, nil)

	renamed := model("older", "mock")
	renamed.Fields[v1alpha1.IdentityField] = "two"
	out := assemble(t, v, []Source{provider(), renamed, newer}, &first)
	wantState(t, out, v1alpha1.KindModel, "older", Retained, "already used by Model/newer")
	wantState(t, out, v1alpha1.KindModel, "newer", Applied, "")
}

func TestOldestWinsANameNobodyIsServedUnder(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	older := model("zzz", "mock")
	older.Fields[v1alpha1.IdentityField] = "same"
	newer := model("aaa", "mock")
	newer.Fields[v1alpha1.IdentityField] = "same"
	newer.Created = t0.Add(time.Minute)
	out := assemble(t, v, []Source{provider(), newer, older}, nil)
	wantState(t, out, v1alpha1.KindModel, "zzz", Applied, "")
	wantState(t, out, v1alpha1.KindModel, "aaa", Rejected, "already used by Model/zzz")
}

func TestModelCannotReferenceALosingProviderKey(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	out := assemble(t, v, []Source{
		namedProvider("prod", "openai", "openai", 0), namedProvider("test", "openai", "caller", time.Minute),
		model("cheap", "test"), model("main", "prod"),
	}, nil)
	wantState(t, out, v1alpha1.KindModel, "cheap", Rejected, `ProviderKey "test", which is not served`)
	wantState(t, out, v1alpha1.KindModel, "cheap", Rejected, "already used by ProviderKey/prod")
	wantState(t, out, v1alpha1.KindModel, "main", Applied, "")
	wantModel(t, out, "main", "openai", credOne)
}

func TestModelResolvesToTheProviderDisplayName(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	m := model("m", "pk")
	m.Fields[v1alpha1.IdentityField] = "openai/gpt"
	out := assemble(t, v, []Source{namedProvider("pk", "Provider One", "openai", 0), m}, nil)
	wantModel(t, out, "openai/gpt", "Provider One", credOne)
}

func TestModelNamingAMissingProviderKeyIsRejected(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	out := assemble(t, v, []Source{model("dangling", "ghost")}, nil)
	wantState(t, out, v1alpha1.KindModel, "dangling", Rejected, `ProviderKey "ghost", which does not exist`)
}

func TestRetainedModelFollowsItsProviderKeyRename(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{reject: rejectUnknownField})
	first := assemble(t, v, []Source{namedProvider("p", "A", "openai", 0), model("m", "p")}, nil)
	out := assemble(t, v, []Source{namedProvider("p", "B", "openai", 0), invalid(model("m", "p"))}, &first)
	wantState(t, out, v1alpha1.KindProviderKey, "p", Applied, "")
	wantState(t, out, v1alpha1.KindModel, "m", Retained, "no_such_field")
	wantModel(t, out, "m", "B", credOne)
}

func TestRetainedModelKeepsItsServedReference(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	first := assemble(t, v, []Source{namedProvider("p", "A", "openai", 0), model("m", "p")}, nil)
	out := assemble(t, v, []Source{namedProvider("p", "A", "openai", 0), model("m", "ghost")}, &first)
	wantState(t, out, v1alpha1.KindModel, "m", Retained, `ProviderKey "ghost", which does not exist`)
	wantModel(t, out, "m", "A", credOne)
}

func TestModelNeverInheritsAReplacementProviderKey(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{})
	first := assemble(t, v, []Source{namedProvider("p", "A", "openai", 0), model("m", "p")}, nil)
	// p is deleted and q takes its display_name.
	out := assemble(t, v, []Source{namedProvider("q", "A", "caller", time.Hour), model("m", "p")}, &first)
	wantState(t, out, v1alpha1.KindProviderKey, "q", Applied, "")
	wantState(t, out, v1alpha1.KindModel, "m", Rejected, `ProviderKey "p", which does not exist`)
	if _, ok := entries(t, out, "models")["m"]; ok {
		t.Errorf("m is still served:\n%s", out.Document)
	}
}

func TestModelIsRejectedWithItsProviderKey(t *testing.T) {
	v := &fakeValidator{}
	first := assemble(t, v, []Source{namedProvider("p", "A", "openai", 0), model("m", "p")}, nil)
	// The gateway now refuses p's spec and its served entry.
	v.reject = func(k v1alpha1.Kind, _ Entry) string {
		if k == v1alpha1.KindProviderKey {
			return "refused"
		}
		return ""
	}
	out := assemble(t, v, []Source{namedProvider("p", "A", "openai", 0), model("m", "p")}, &first)
	wantState(t, out, v1alpha1.KindProviderKey, "p", Rejected, "refused")
	wantState(t, out, v1alpha1.KindModel, "m", Rejected, `ProviderKey "p", which is not served`)
	wantState(t, out, v1alpha1.KindModel, "m", Rejected, "refused")
	if _, ok := entries(t, out, "models")["m"]; ok {
		t.Errorf("m is still served:\n%s", out.Document)
	}
}

func TestDollarsInAProviderDisplayNameStayEscaped(t *testing.T) {
	v := gatewayValidator(t, &fakeValidator{reject: rejectUnknownField})
	first := assemble(t, v, []Source{namedProvider("p", "a$b", "openai", 0), model("m", "p")}, nil)
	wantModel(t, first, "m", "a$$b", credOne)
	out := assemble(t, v, []Source{namedProvider("p", "a$b", "openai", 0), invalid(model("m", "p"))}, &first)
	wantState(t, out, v1alpha1.KindModel, "m", Retained, "no_such_field")
	wantModel(t, out, "m", "a$$b", credOne)
}
