// Package schemacheck validates JSON values against the subset of JSON
// Schema 2020-12 runnerq-spec's protocol schemas use, so implementations can
// check what they send against the spec in their own tests. String lengths
// are UTF-8 bytes, as the protocols say.
package schemacheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Schema is a protocol schema document.
type Schema struct {
	// Doc is the whole document, decoded with json.Number numbers.
	Doc  map[string]any
	defs map[string]any
}

// Load reads a schema document (e.g. spec/protocol/conductor/conductor.schema.json).
func Load(path string) (*Schema, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := Decode(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		return nil, fmt.Errorf("%s: not a 2020-12 schema", path)
	}
	defs, _ := doc["$defs"].(map[string]any)
	return &Schema{Doc: doc, defs: defs}, nil
}

// New is a schema with only these definitions, for tests of the validator.
func New(defs map[string]any) *Schema { return &Schema{defs: defs} }

// Decode decodes JSON keeping numbers as json.Number, as Check expects.
func Decode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

// CheckJSON validates raw JSON against the definition named def.
func (s *Schema) CheckJSON(def string, raw []byte) error {
	var v any
	if err := Decode(raw, &v); err != nil {
		return err
	}
	return s.Check(map[string]any{"$ref": "#/$defs/" + def}, v, def)
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Check validates value (decoded by Decode) against schema; path names the
// value in errors.
func (v *Schema) Check(schema any, value any, path string) error {
	s, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: schema is not an object", path)
	}
	if ref, ok := s["$ref"].(string); ok {
		def, ok := v.defs[strings.TrimPrefix(ref, "#/$defs/")]
		if !ok {
			return fmt.Errorf("%s: unknown $ref %s", path, ref)
		}
		if err := v.Check(def, value, path); err != nil {
			return err
		}
	}
	if anyOf, ok := s["anyOf"].([]any); ok {
		var errs []string
		for _, branch := range anyOf {
			err := v.Check(branch, value, path)
			if err == nil {
				errs = nil
				break
			}
			errs = append(errs, err.Error())
		}
		if errs != nil {
			return fmt.Errorf("%s: matches no anyOf branch (%s)", path, strings.Join(errs, "; "))
		}
	}
	if not, ok := s["not"]; ok && v.Check(not, value, path) == nil {
		return fmt.Errorf("%s: matches a forbidden schema", path)
	}
	if c, ok := s["const"]; ok && fmt.Sprint(c) != fmt.Sprint(value) {
		return fmt.Errorf("%s: %v is not %v", path, value, c)
	}
	if enum, ok := s["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(value) }) {
		return fmt.Errorf("%s: %v is not one of %v", path, value, enum)
	}
	if typ, ok := s["type"].(string); ok {
		if err := checkType(typ, value, path); err != nil {
			return err
		}
	}
	switch val := value.(type) {
	case string:
		if n, ok := s["minLength"].(json.Number); ok && int64(len(val)) < mustInt(n) {
			return fmt.Errorf("%s: shorter than %s bytes", path, n)
		}
		if n, ok := s["maxLength"].(json.Number); ok && int64(len(val)) > mustInt(n) {
			return fmt.Errorf("%s: longer than %s bytes", path, n)
		}
		switch s["format"] {
		case "uuid":
			if !uuidRe.MatchString(val) {
				return fmt.Errorf("%s: %q is not a lowercase UUID", path, val)
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339Nano, val); err != nil {
				return fmt.Errorf("%s: %q is not RFC 3339", path, val)
			}
		}
	case json.Number:
		x, _ := new(big.Float).SetString(val.String())
		if n, ok := s["minimum"].(json.Number); ok {
			if m, _ := new(big.Float).SetString(n.String()); x.Cmp(m) < 0 {
				return fmt.Errorf("%s: %s is below %s", path, val, n)
			}
		}
		if n, ok := s["maximum"].(json.Number); ok {
			if m, _ := new(big.Float).SetString(n.String()); x.Cmp(m) > 0 {
				return fmt.Errorf("%s: %s is above %s", path, val, n)
			}
		}
	case []any:
		if n, ok := s["maxItems"].(json.Number); ok && int64(len(val)) > mustInt(n) {
			return fmt.Errorf("%s: more than %s items", path, n)
		}
		if items, ok := s["items"]; ok {
			for i, item := range val {
				if err := v.Check(items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for _, r := range asStrings(s["required"]) {
			if _, ok := val[r]; !ok {
				return fmt.Errorf("%s: missing required %s", path, r)
			}
		}
		for k, item := range val {
			if ps, ok := props[k]; ok {
				if err := v.Check(ps, item, path+"."+k); err != nil {
					return err
				}
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					return fmt.Errorf("%s: unknown property %s", path, k)
				}
			case map[string]any:
				if err := v.Check(ap, item, path+"."+k); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkType(typ string, value any, path string) error {
	ok := false
	switch typ {
	case "null":
		ok = value == nil
	case "boolean":
		_, ok = value.(bool)
	case "string":
		_, ok = value.(string)
	case "number":
		_, ok = value.(json.Number)
	case "integer":
		if n, isNum := value.(json.Number); isNum {
			_, ok = new(big.Int).SetString(n.String(), 10)
		}
	case "array":
		_, ok = value.([]any)
	case "object":
		_, ok = value.(map[string]any)
	}
	if !ok {
		return fmt.Errorf("%s: %v is not %s", path, value, typ)
	}
	return nil
}

func mustInt(n json.Number) int64 {
	i, err := n.Int64()
	if err != nil {
		panic(err)
	}
	return i
}

func asStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, s := range list {
		out = append(out, s.(string))
	}
	return out
}
