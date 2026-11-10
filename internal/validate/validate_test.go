package validate

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Captured from aisix v1.9.0 `validate`.
const report = `resources file /w/bad.yaml: 3 error(s):
  - provider_keys[0]: field ` + "`api_key`" + `: environment variable ` + "`NOPE`" + ` is unset or empty
  - models[0] ("we\"ird: name"): ` + "`provider_key`" + ` references unknown provider key "mock"
  - (file): missing mandatory _format_version (expected "1")

`

func TestParseReport(t *testing.T) {
	got := ParseReport(report)
	want := []LoadError{
		{"provider_keys[0]", "field `api_key`: environment variable `NOPE` is unset or empty"},
		{`models[0] ("we\"ird: name")`, "`provider_key` references unknown provider key \"mock\""},
		{"(file)", `missing mandatory _format_version (expected "1")`},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d errors: %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseReportJoinsContinuationLines(t *testing.T) {
	got := ParseReport("x:\n  - models[1] (\"a\"): first\n    second\n")
	if len(got) != 1 || got[0].Message != "first\nsecond" {
		t.Errorf("got %+v", got)
	}
}

func binary(t *testing.T) *Binary {
	p := os.Getenv("AISIX_BIN")
	if p == "" {
		t.Skip("set AISIX_BIN to an aisix binary")
	}
	b := &Binary{Path: p}
	if err := b.Check(); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBinaryAcceptsAValidDocument(t *testing.T) {
	errs, err := binary(t).Validate(context.Background(), []byte(`{"_format_version": "1"}`))
	if err != nil || len(errs) != 0 {
		t.Fatalf("errs=%v err=%v", errs, err)
	}
}

func TestBinaryReportsScopedErrors(t *testing.T) {
	doc := `{"_format_version": "1", "models": [{"display_name": "m", "provider": "openai", "model_name": "a", "provider_key": "ghost"}]}`
	errs, err := binary(t).Validate(context.Background(), []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 || errs[0].Scope != `models[0] ("m")` || !strings.Contains(errs[0].Message, "ghost") {
		t.Errorf("errs = %+v", errs)
	}
}

func TestBinaryNeverReadsTheControllerEnvironment(t *testing.T) {
	t.Setenv("AISIX_TEST_SECRET", "leaked")
	doc := `{"_format_version": "1", "provider_keys": [{"display_name": "p", "provider": "openai", "api_key": "${AISIX_TEST_SECRET}"}]}`
	errs, err := binary(t).Validate(context.Background(), []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "AISIX_TEST_SECRET") {
		t.Errorf("errs = %+v", errs)
	}
}

func TestMissingBinaryIsAnError(t *testing.T) {
	b := &Binary{Path: "/nonexistent/aisix"}
	if b.Check() == nil {
		t.Error("Check passed")
	}
	if _, err := b.Validate(context.Background(), []byte("{}")); err == nil {
		t.Error("Validate passed")
	}
}
