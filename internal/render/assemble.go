package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/api7/aisix-controller/api/v1alpha1"
	"github.com/api7/aisix-controller/internal/validate"
)

// FormatVersion is the resources-file format the controller writes.
const FormatVersion = "1"

// Disposition is what happened to one custom resource.
type Disposition struct {
	State  State
	Reason string
}

type State int

const (
	// Applied: its current spec is in the document.
	Applied State = iota
	// Retained: rejected; its previously served entry stays.
	Retained
	// Rejected: rejected, and not served at all.
	Rejected
)

// Outcome of assembling one document.
type Outcome struct {
	// Document to serve; nil when no valid one could be made.
	Document []byte
	// Index records which resource owns each entry of Document, keyed
	// "Kind/name", valued with the entry's identity. It is stored next to
	// the document so a resource's served entry is found even after its
	// display_name changes.
	Index        []byte
	Dispositions map[Key]Disposition
	// Error is set when the document as a whole failed.
	Error string
}

// Document renders entries as a resources file. JSON is valid YAML, and
// encoding/json sorts keys, so equal inputs give equal bytes.
func Document(entries map[v1alpha1.Kind][]Entry) []byte {
	root := map[string]any{"_format_version": FormatVersion}
	for _, k := range v1alpha1.Kinds {
		if items := entries[k]; len(items) > 0 {
			list := make([]any, len(items))
			for i, e := range items {
				list[i] = e
			}
			root[k.Collection()] = list
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		panic(fmt.Sprintf("resources document does not encode: %v", err))
	}
	return buf.Bytes()
}

// Index renders a resource-to-identity index. encoding/json sorts keys, so
// equal inputs give equal bytes.
func Index(owners map[string]string) []byte {
	if owners == nil {
		owners = map[string]string{}
	}
	b, err := json.Marshal(owners)
	if err != nil {
		panic(fmt.Sprintf("resources index does not encode: %v", err))
	}
	return append(b, '\n')
}

// Hash is what the gateway reports as applied.config_hash in file mode:
// SHA-256 of the raw file bytes, lowercase hex.
func Hash(document []byte) string {
	sum := sha256.Sum256(document)
	return hex.EncodeToString(sum[:])
}

type servedKey struct {
	kind     v1alpha1.Kind
	identity string
}

// Served is the currently served document, indexed by owning resource.
type Served struct {
	entries map[servedKey]Entry
	owners  map[Key]string
}

// ServedEntries parses a served document and its index. Anything unreadable
// counts as nothing served.
func ServedEntries(document, index []byte) Served {
	out := Served{entries: map[servedKey]Entry{}, owners: map[Key]string{}}
	if len(document) == 0 || len(index) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(document))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return out
	}
	var owners map[string]string
	if err := json.Unmarshal(index, &owners); err != nil {
		return out
	}
	for _, k := range v1alpha1.Kinds {
		items, _ := root[k.Collection()].([]any)
		for _, item := range items {
			e, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := e[v1alpha1.IdentityField].(string); ok {
				out.entries[servedKey{k, strings.ReplaceAll(id, "$$", "$")}] = e
			}
		}
	}
	for key, id := range owners {
		kind, name, ok := strings.Cut(key, "/")
		if !ok {
			continue
		}
		out.owners[Key{v1alpha1.Kind(kind), name}] = id
	}
	return out
}

// get returns the entry a resource is served with, and its identity.
func (s Served) get(src Source) (Entry, string, bool) {
	id, ok := s.owners[src.Key()]
	if !ok {
		return nil, "", false
	}
	e, ok := s.entries[servedKey{src.Kind, id}]
	return e, id, ok
}

// ParseScope maps `models[2] ("gpt")` to (Model, 2).
func ParseScope(scope string) (v1alpha1.Kind, int, bool) {
	collection, rest, ok := strings.Cut(scope, "[")
	if !ok {
		return "", 0, false
	}
	index, _, ok := strings.Cut(rest, "]")
	if !ok {
		return "", 0, false
	}
	k, ok := v1alpha1.KindForCollection(collection)
	if !ok {
		return "", 0, false
	}
	var i int
	if _, err := fmt.Sscanf(index, "%d", &i); err != nil || fmt.Sprint(i) != index {
		return "", 0, false
	}
	return k, i, true
}

type item struct {
	source Source
	state  State
	entry  Entry
	// identity the entry is served under.
	identity string
	reason   string
}

// Assemble builds the document to serve. An entry the gateway rejects is
// replaced by the version currently served, so one bad resource never takes
// the others down. With no served version it is left out.
func Assemble(ctx context.Context, rendered []Rendered, served Served, v validate.Validator) (Outcome, error) {
	items := make([]*item, len(rendered))
	fallback := func(it *item, reason string) {
		if e, id, ok := served.get(it.source); ok {
			it.state, it.entry, it.identity, it.reason = Retained, e, id, reason
		} else {
			it.state, it.entry, it.identity, it.reason = Rejected, nil, "", reason
		}
	}
	for i, r := range rendered {
		it := &item{source: r.Source, state: Applied, entry: r.Entry, identity: r.Source.Identity()}
		if r.Err != "" {
			fallback(it, r.Err)
		}
		items[i] = it
	}

	// Each failing round drops or replaces at least one entry.
	for round := 0; round < 2*len(items)+1; round++ {
		entries := map[v1alpha1.Kind][]Entry{}
		positions := map[servedKey]*item{}
		for _, it := range items {
			if it.state == Rejected {
				continue
			}
			k := it.source.Kind
			positions[servedKey{k, fmt.Sprint(len(entries[k]))}] = it
			entries[k] = append(entries[k], it.entry)
		}
		doc := Document(entries)
		errs, err := v.Validate(ctx, doc)
		if err != nil {
			return Outcome{}, err
		}
		if len(errs) == 0 {
			return finish(items, doc, ""), nil
		}

		faults := map[*item][]string{}
		var order []*item
		for _, e := range errs {
			k, i, ok := ParseScope(e.Scope)
			it := positions[servedKey{k, fmt.Sprint(i)}]
			if !ok || it == nil {
				all := make([]string, len(errs))
				for j, x := range errs {
					all[j] = x.String()
				}
				return finish(items, nil, strings.Join(all, "; ")), nil
			}
			if _, seen := faults[it]; !seen {
				order = append(order, it)
			}
			faults[it] = append(faults[it], e.Message)
		}
		for _, it := range order {
			reason := strings.Join(faults[it], "; ")
			switch it.state {
			case Applied:
				fallback(it, reason)
			case Retained:
				it.state, it.entry, it.identity = Rejected, nil, ""
				it.reason = it.reason + "; the previously served version is no longer valid either: " + reason
			}
		}
	}
	return finish(items, nil, "the resources file did not converge to a valid state"), nil
}

func finish(items []*item, doc []byte, errMsg string) Outcome {
	out := Outcome{Document: doc, Dispositions: map[Key]Disposition{}, Error: errMsg}
	owners := map[string]string{}
	for _, it := range items {
		out.Dispositions[it.source.Key()] = Disposition{State: it.state, Reason: it.reason}
		if it.state != Rejected {
			owners[it.source.Key().String()] = it.identity
		}
	}
	if doc != nil {
		out.Index = Index(owners)
	}
	return out
}
