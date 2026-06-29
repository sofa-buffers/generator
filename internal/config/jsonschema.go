package config

import (
	"fmt"
	"sort"
	"strings"
)

// A tiny draft-07 subset validator — just enough to enforce the config schema
// (schema/sofabgen-config-schema.json): type, enum, properties,
// additionalProperties:false, $ref ("#/..."), anyOf, items, minimum. The
// config schema is deliberately written within this subset so the binary needs
// no third-party JSON-Schema dependency (PLAN: minimal-dependency executable).

type schemaErr struct{ loc, msg string }

type schemaValidator struct {
	root map[string]any
	errs []schemaErr
}

func validateAgainstSchema(schema map[string]any, doc any) []schemaErr {
	v := &schemaValidator{root: schema}
	v.check(schema, doc, "#")
	sort.SliceStable(v.errs, func(i, j int) bool { return v.errs[i].loc < v.errs[j].loc })
	return v.errs
}

func (v *schemaValidator) add(loc, format string, args ...any) {
	v.errs = append(v.errs, schemaErr{loc: loc, msg: fmt.Sprintf(format, args...)})
}

func (v *schemaValidator) check(schema map[string]any, doc any, loc string) {
	// $ref
	if ref, ok := schema["$ref"].(string); ok {
		target := v.deref(ref)
		if target == nil {
			v.add(loc, "internal: unresolved $ref %q", ref)
			return
		}
		v.check(target, doc, loc)
		return
	}
	// anyOf
	if anyOf, ok := schema["anyOf"].([]any); ok {
		for _, sub := range anyOf {
			if sm, ok := sub.(map[string]any); ok {
				if len(validateSub(v.root, sm, doc)) == 0 {
					return
				}
			}
		}
		v.add(loc, "value does not match any allowed variant")
		return
	}
	// type
	if typ, ok := schema["type"].(string); ok {
		if !typeMatches(typ, doc) {
			v.add(loc, "expected %s", typ)
			return
		}
	}
	// enum
	if enum, ok := schema["enum"].([]any); ok {
		if !enumContains(enum, doc) {
			v.add(loc, "value %v is not one of %s", doc, enumList(enum))
		}
	}
	// minimum
	if minRaw, ok := schema["minimum"]; ok {
		if n, ok := toFloat(doc); ok {
			if m, ok := toFloat(minRaw); ok && n < m {
				v.add(loc, "must be >= %v", minRaw)
			}
		}
	}
	// object: properties + additionalProperties
	if props, ok := schema["properties"].(map[string]any); ok {
		obj, ok := doc.(map[string]any)
		if !ok {
			return // type check (if present) already reported a mismatch
		}
		addAllowed := true
		if ap, ok := schema["additionalProperties"].(bool); ok {
			addAllowed = ap
		}
		for k, val := range obj {
			sub, known := props[k]
			if !known {
				if !addAllowed {
					v.add(loc, "unknown key %q (allowed: %s)", k, allowedKeys(props))
				}
				continue
			}
			if sm, ok := sub.(map[string]any); ok {
				v.check(sm, val, loc+"/"+k)
			}
		}
	}
	// array items
	if items, ok := schema["items"].(map[string]any); ok {
		if arr, ok := doc.([]any); ok {
			for i, el := range arr {
				v.check(items, el, fmt.Sprintf("%s/%d", loc, i))
			}
		}
	}
}

// validateSub runs an isolated validation (for anyOf branches) and returns its
// errors without polluting the parent collector.
func validateSub(root, schema map[string]any, doc any) []schemaErr {
	sv := &schemaValidator{root: root}
	sv.check(schema, doc, "#")
	return sv.errs
}

func (v *schemaValidator) deref(ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = v.root
	for _, tok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[tok]
		if !ok {
			return nil
		}
	}
	if m, ok := cur.(map[string]any); ok {
		return m
	}
	return nil
}

func typeMatches(typ string, doc any) bool {
	switch typ {
	case "object":
		_, ok := doc.(map[string]any)
		return ok
	case "array":
		_, ok := doc.([]any)
		return ok
	case "string":
		_, ok := doc.(string)
		return ok
	case "boolean":
		_, ok := doc.(bool)
		return ok
	case "integer":
		switch x := doc.(type) {
		case int, int64, uint64:
			return true
		case float64:
			return x == float64(int64(x))
		}
		return false
	case "number":
		_, ok := toFloat(doc)
		return ok
	}
	return true
}

func enumContains(enum []any, doc any) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(doc) {
			return true
		}
	}
	return false
}

func enumList(enum []any) string {
	parts := make([]string, len(enum))
	for i, e := range enum {
		parts[i] = fmt.Sprint(e)
	}
	return strings.Join(parts, ", ")
}

func allowedKeys(props map[string]any) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}
