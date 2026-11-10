// Package validate checks resources files with the aisix binary, which runs
// the same pipeline the gateway runs at boot and on reload.
package validate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// LoadError is one problem the gateway pipeline reported. Scope is
// `models[2] ("gpt-4o")` for an entry, `provider_keys[0]` when the entry has
// no identity yet, or `(file)`.
type LoadError struct {
	Scope   string
	Message string
}

func (e LoadError) String() string { return e.Scope + ": " + e.Message }

// Validator runs the gateway's resources-file pipeline over a document. A
// non-nil error means the check itself could not run.
type Validator interface {
	Validate(ctx context.Context, document []byte) ([]LoadError, error)
}

// Func adapts a function to Validator.
type Func func(ctx context.Context, document []byte) ([]LoadError, error)

func (f Func) Validate(ctx context.Context, document []byte) ([]LoadError, error) {
	return f(ctx, document)
}

// Binary validates with `aisix validate --resources <file>`.
type Binary struct {
	Path    string
	TempDir string
	Timeout time.Duration
}

var _ Validator = (*Binary)(nil)

// Check validates an empty resources file, so a binary that cannot run
// fails at startup.
func (b *Binary) Check() error {
	info, err := os.Stat(b.Path)
	if err != nil {
		return fmt.Errorf("aisix binary: %w", err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("aisix binary %s is not executable", b.Path)
	}
	errs, err := b.Validate(context.Background(), []byte(`{"_format_version": "1"}`))
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		return fmt.Errorf("aisix binary %s rejects an empty resources file: %s", b.Path, errs[0])
	}
	return nil
}

func (b *Binary) Validate(ctx context.Context, document []byte) ([]LoadError, error) {
	f, err := os.CreateTemp(b.TempDir, "resources-*.yaml")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(document); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	timeout := b.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, b.Path, "validate", "--resources", f.Name())
	// An empty environment: ${VAR} must never resolve from the controller's.
	cmd.Env = []string{}
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		return nil, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || ctx.Err() != nil {
		return nil, fmt.Errorf("running aisix validate: %w", err)
	}
	if errs := ParseReport(stderr.String()); len(errs) > 0 {
		return errs, nil
	}
	return []LoadError{{Scope: "(file)", Message: strings.TrimSpace(stderr.String())}}, nil
}

// ParseReport reads the aggregated error report aisix prints on stderr:
//
//	resources file <path>: 2 error(s):
//	  - models[0] ("chat"): unknown field `x`
//	  - (file): missing mandatory _format_version
func ParseReport(report string) []LoadError {
	var out []LoadError
	for _, line := range strings.Split(report, "\n") {
		rest, ok := strings.CutPrefix(line, "  - ")
		if !ok {
			// A multi-line message continues the previous entry.
			if len(out) > 0 && strings.HasPrefix(line, "    ") {
				out[len(out)-1].Message += "\n" + strings.TrimSpace(line)
			}
			continue
		}
		scope, message := splitScope(rest)
		out = append(out, LoadError{Scope: scope, Message: message})
	}
	return out
}

// splitScope splits `<scope>: <message>`. A scope's identity is a quoted
// Rust debug string that may itself contain ": ".
func splitScope(s string) (string, string) {
	end := -1
	if strings.HasPrefix(s, "(file)") {
		end = len("(file)")
	} else if i := strings.Index(s, "]"); i > 0 {
		end = i + 1
		if strings.HasPrefix(s[end:], ` ("`) {
			if j := closingQuote(s, end+3); j > 0 && j+1 < len(s) && s[j+1] == ')' {
				end = j + 2
			}
		}
	}
	if end > 0 && strings.HasPrefix(s[end:], ": ") {
		return s[:end], s[end+2:]
	}
	if scope, msg, ok := strings.Cut(s, ": "); ok {
		return scope, msg
	}
	return "(file)", s
}

func closingQuote(s string, from int) int {
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}
