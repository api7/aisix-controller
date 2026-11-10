// Package schema turns AISIX's strict draft-07 resource schemas into
// structural OpenAPI v3 schemas a CRD accepts.
//
// The conversion keeps types, required fields, enums, ranges, lengths and
// patterns. It drops what a structural schema cannot carry (exclusive
// oneOf branches, if/then, not, defaults, formats). The gateway still
// enforces those: the controller validates every resources file with the
// aisix binary before writing it.
package schema

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type obj = map[string]any

const (
	preserveKey    = "x-kubernetes-preserve-unknown-fields"
	intOrStringKey = "x-kubernetes-int-or-string"
)

// Keywords copied as-is onto a structural node.
var kept = []string{
	"description", "enum", "minimum", "maximum", "minLength", "maxLength",
	"pattern", "minItems", "maxItems", "minProperties", "maxProperties", "required",
}

func preserve(description any) obj {
	m := obj{preserveKey: true}
	if description != nil {
		m["description"] = description
	}
	return m
}

func clone(m obj) obj {
	out := make(obj, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ToStructural converts a draft-07 schema, with its root definitions, to a
// structural schema.
func ToStructural(root obj) obj {
	defs, _ := root["definitions"].(obj)
	out := convert(root, defs, nil)
	delete(out, preserveKey)
	out["type"] = "object"
	return out
}

// isConstraintOnly: a composition branch that only constrains values and
// adds no shape of its own.
func isConstraintOnly(branch any) bool {
	m, ok := branch.(obj)
	if !ok {
		return false
	}
	for k := range m {
		switch k {
		case "required", "not", "title", "description", "if", "then", "else":
		default:
			return false
		}
	}
	return true
}

func isNullBranch(branch any) bool {
	m, ok := branch.(obj)
	return ok && m["type"] == "null"
}

func contains(stack []string, s string) bool {
	for _, x := range stack {
		if x == s {
			return true
		}
	}
	return false
}

// flatten resolves $ref and single-branch allOf into one schema object. The
// referring node's own keywords win.
func flatten(node obj, defs obj, stack []string) (obj, bool) {
	out := clone(node)
	if r, ok := out["$ref"]; ok {
		delete(out, "$ref")
		s, _ := r.(string)
		name, ok := strings.CutPrefix(s, "#/definitions/")
		if !ok || contains(stack, name) {
			return nil, false
		}
		target, ok := defs[name].(obj)
		if !ok {
			return nil, false
		}
		merged, ok := flatten(target, defs, append(stack, name))
		if !ok {
			return nil, false
		}
		for k, v := range out {
			merged[k] = v
		}
		out = merged
	}
	if all, ok := out["allOf"].([]any); ok {
		var shaped []any
		for _, b := range all {
			if !isConstraintOnly(b) {
				shaped = append(shaped, b)
			}
		}
		if len(shaped) == 1 {
			delete(out, "allOf")
			branch, ok := shaped[0].(obj)
			if !ok {
				return nil, false
			}
			merged, ok := flatten(branch, defs, stack)
			if !ok {
				return nil, false
			}
			for k, v := range out {
				merged[k] = v
			}
			out = merged
		}
	}
	return out, true
}

func intOrString(types []string) bool {
	return len(types) == 2 &&
		((types[0] == "integer" && types[1] == "string") || (types[0] == "string" && types[1] == "integer"))
}

func open(description any, nullable, intOrStr bool) obj {
	p := preserve(description)
	if intOrStr {
		delete(p, preserveKey)
		p[intOrStringKey] = true
	}
	if nullable {
		p["nullable"] = true
	}
	return p
}

func convert(node any, defs obj, stack []string) obj {
	m0, ok := node.(obj)
	if !ok || len(m0) == 0 {
		return preserve(nil)
	}
	m, ok := flatten(m0, defs, stack)
	if !ok {
		return preserve(m0["description"])
	}

	nullable := false

	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := m[key].([]any)
		if !ok {
			continue
		}
		delete(m, key)
		var shaped []any
		for _, b := range branches {
			if isNullBranch(b) {
				nullable = true
			} else if !isConstraintOnly(b) {
				shaped = append(shaped, b)
			}
		}
		if len(shaped) == 0 {
			continue
		}
		converted := make([]obj, 0, len(shaped))
		for _, b := range shaped {
			converted = append(converted, convert(b, defs, stack))
		}
		merged, ok := mergeBranches(converted, key == "allOf")
		if !ok {
			var types []string
			for _, c := range converted {
				if t, ok := c["type"].(string); ok {
					types = append(types, t)
				}
			}
			return open(m["description"], nullable, len(converted) == 2 && intOrString(types))
		}
		for k, v := range merged {
			if k == "description" {
				if _, has := m["description"]; has {
					continue
				}
			}
			if k == "properties" {
				dst, _ := m["properties"].(obj)
				if dst == nil {
					dst = obj{}
				} else {
					dst = clone(dst)
				}
				for pk, pv := range v.(obj) {
					if _, has := dst[pk]; !has {
						dst[pk] = pv
					}
				}
				m["properties"] = dst
				continue
			}
			if _, has := m[k]; !has {
				m[k] = v
			}
		}
	}

	var ty string
	switch t := m["type"].(type) {
	case string:
		if t == "null" {
			nullable = true
		} else {
			ty = t
		}
	case []any:
		var rest []string
		for _, x := range t {
			s, _ := x.(string)
			switch s {
			case "null":
				nullable = true
			case "":
			default:
				rest = append(rest, s)
			}
		}
		switch len(rest) {
		case 0:
		case 1:
			ty = rest[0]
		default:
			return open(m["description"], nullable, intOrString(rest))
		}
	}
	delete(m, "type")

	out := obj{}
	for _, k := range kept {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	if c, ok := m["const"]; ok {
		out["enum"] = []any{c}
	}
	// draft-07 exclusive bounds are numbers; OpenAPI v3.0 uses booleans.
	for _, pair := range [][2]string{{"exclusiveMinimum", "minimum"}, {"exclusiveMaximum", "maximum"}} {
		if n, ok := m[pair[0]]; ok && isNumber(n) {
			out[pair[1]] = n
			out[pair[0]] = true
		}
	}
	if values, ok := out["enum"].([]any); ok {
		var kept []any
		for _, v := range values {
			if v == nil {
				nullable = true
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == 0 {
			delete(out, "enum")
		} else {
			out["enum"] = kept
		}
	}

	props, hasProps := m["properties"].(obj)
	additional, hasAdditional := m["additionalProperties"]
	items, hasItems := m["items"]

	if ty == "" {
		switch {
		case hasProps || hasAdditional:
			ty = "object"
		case hasItems:
			ty = "array"
		default:
			if values, ok := out["enum"].([]any); ok {
				ty = inferEnumType(values)
			}
		}
	}

	switch ty {
	case "object":
		var additionalSchema obj
		switch a := additional.(type) {
		case obj:
			if len(a) > 0 {
				additionalSchema = convert(a, defs, stack)
			} else {
				additionalSchema = preserve(nil)
			}
		case bool:
			if a {
				additionalSchema = preserve(nil)
			}
		}
		// A CRD property name cannot be empty, so a "" entry is left to
		// additionalProperties.
		converted := obj{}
		for k, v := range props {
			if k != "" {
				converted[k] = convert(v, defs, stack)
			}
		}
		switch {
		case len(converted) > 0:
			out["properties"] = converted
		case additionalSchema != nil:
			out["additionalProperties"] = additionalSchema
		default:
			out[preserveKey] = true
		}
		out["type"] = "object"
		pruneRequired(out)
	case "array":
		item := preserve(nil)
		if i, ok := items.(obj); ok {
			item = convert(i, defs, stack)
		}
		out["items"] = item
		out["type"] = "array"
		delete(out, "required")
	case "string", "integer", "number", "boolean":
		out["type"] = ty
		delete(out, "required")
		if values, ok := out["enum"].([]any); ok {
			for _, v := range values {
				if !matchesType(v, ty) {
					delete(out, "enum")
					break
				}
			}
		}
	default:
		return open(m["description"], nullable, false)
	}
	if nullable {
		out["nullable"] = true
	}
	return out
}

// pruneRequired keeps only required entries that name a declared property.
func pruneRequired(out obj) {
	req, ok := out["required"].([]any)
	if !ok {
		return
	}
	props, _ := out["properties"].(obj)
	var keep []any
	for _, r := range req {
		if s, ok := r.(string); ok {
			if _, declared := props[s]; declared {
				keep = append(keep, s)
			}
		}
	}
	if len(keep) == 0 {
		delete(out, "required")
	} else {
		out["required"] = keep
	}
}

func isNumber(v any) bool {
	switch v.(type) {
	case json.Number, float64, int, int64:
		return true
	}
	return false
}

func isInteger(v any) bool {
	switch n := v.(type) {
	case json.Number:
		_, err := strconv.ParseInt(n.String(), 10, 64)
		return err == nil
	case int, int64:
		return true
	case float64:
		return n == float64(int64(n))
	}
	return false
}

func matchesType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		return isInteger(v)
	case "number":
		return isNumber(v)
	case "boolean":
		_, ok := v.(bool)
		return ok
	}
	return false
}

func inferEnumType(values []any) string {
	if len(values) == 0 {
		return ""
	}
	for _, t := range []string{"string", "integer", "number", "boolean"} {
		all := true
		for _, v := range values {
			if !matchesType(v, t) {
				all = false
				break
			}
		}
		if all {
			return t
		}
	}
	return ""
}

// mergeBranches merges converted branches into one shape. It fails when
// their types differ. allOf keeps the union of required; anyOf/oneOf drop it.
func mergeBranches(branches []obj, all bool) (obj, bool) {
	ty := ""
	for i, b := range branches {
		t, ok := b["type"].(string)
		if !ok || (i > 0 && t != ty) {
			return nil, false
		}
		ty = t
	}
	out := obj{"type": ty}
	switch ty {
	case "object":
		props := obj{}
		required := map[string]bool{}
		anyOpen := false
		for _, b := range branches {
			if p, ok := b["properties"].(obj); ok {
				for k, v := range p {
					if _, has := props[k]; !has {
						props[k] = v
					}
				}
			}
			_, hasAdditional := b["additionalProperties"]
			if b[preserveKey] == true || hasAdditional {
				anyOpen = true
			}
			if all {
				if r, ok := b["required"].([]any); ok {
					for _, x := range r {
						if s, ok := x.(string); ok {
							required[s] = true
						}
					}
				}
			}
		}
		if anyOpen {
			// Branch shapes disagree on openness; keep everything.
			out[preserveKey] = true
		}
		if len(props) > 0 {
			out["properties"] = props
		}
		if len(required) > 0 {
			names := make([]string, 0, len(required))
			for s := range required {
				names = append(names, s)
			}
			sort.Strings(names)
			req := make([]any, len(names))
			for i, s := range names {
				req[i] = s
			}
			out["required"] = req
		}
	case "array":
		out["items"] = preserve(nil)
	default:
		var values []any
		allEnum := true
		for _, b := range branches {
			e, ok := b["enum"].([]any)
			if !ok {
				allEnum = false
				continue
			}
			for _, v := range e {
				if !containsValue(values, v) {
					values = append(values, v)
				}
			}
		}
		if allEnum && len(values) > 0 {
			out["enum"] = values
		}
	}
	return out, true
}

func containsValue(values []any, v any) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
