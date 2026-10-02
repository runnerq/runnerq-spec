package verify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// validator checks JSON values against the subset of JSON Schema 2020-12
// the protocol schemas use. String lengths are UTF-8 bytes, as the
// protocols say.
type validator struct {
	defs map[string]any
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func loadSchema(t *testing.T, path string) (map[string]any, *validator) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", path))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := decodeNumbers(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("%s: not a 2020-12 schema", path)
	}
	defs, _ := doc["$defs"].(map[string]any)
	return doc, &validator{defs: defs}
}

func decodeNumbers(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

func (v *validator) check(schema any, value any, path string) error {
	s, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: schema is not an object", path)
	}
	if ref, ok := s["$ref"].(string); ok {
		def, ok := v.defs[strings.TrimPrefix(ref, "#/$defs/")]
		if !ok {
			return fmt.Errorf("%s: unknown $ref %s", path, ref)
		}
		if err := v.check(def, value, path); err != nil {
			return err
		}
	}
	if anyOf, ok := s["anyOf"].([]any); ok {
		var errs []string
		for _, branch := range anyOf {
			err := v.check(branch, value, path)
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
	if not, ok := s["not"]; ok && v.check(not, value, path) == nil {
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
				if err := v.check(items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
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
				if err := v.check(ps, item, path+"."+k); err != nil {
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
				if err := v.check(ap, item, path+"."+k); err != nil {
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

// The validator itself: one value per rule it enforces.
func TestValidator(t *testing.T) {
	v := &validator{defs: map[string]any{"U": map[string]any{"type": "string", "format": "uuid"}}}
	schema := func(s string) any {
		var out any
		if err := decodeNumbers([]byte(s), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	value := schema
	for _, c := range []struct {
		schema, value string
		ok            bool
	}{
		{`{"type":"integer","minimum":1,"maximum":3}`, `2`, true},
		{`{"type":"integer","minimum":1,"maximum":3}`, `4`, false},
		{`{"type":"integer"}`, `1.5`, false},
		{`{"type":"string","maxLength":2}`, `"é"`, true},
		{`{"type":"string","maxLength":1}`, `"é"`, false},
		{`{"$ref":"#/$defs/U"}`, `"0192f3a4-5b6c-7d8e-9f01-23456789abcd"`, true},
		{`{"$ref":"#/$defs/U"}`, `"0192F3A4-5B6C-7D8E-9F01-23456789ABCD"`, false},
		{`{"$ref":"#/$defs/U","not":{"const":"00000000-0000-0000-0000-000000000000"}}`, `"00000000-0000-0000-0000-000000000000"`, false},
		{`{"anyOf":[{"type":"string"},{"type":"null"}]}`, `null`, true},
		{`{"anyOf":[{"type":"string"},{"type":"null"}]}`, `1`, false},
		{`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`, `{"a":"x"}`, true},
		{`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`, `{}`, false},
		{`{"type":"object","properties":{},"additionalProperties":false}`, `{"b":1}`, false},
		{`{"type":"object","additionalProperties":{"type":"string"}}`, `{"b":1}`, false},
		{`{"type":"array","items":{"type":"string"},"maxItems":1}`, `["a","b"]`, false},
		{`{"type":"string","format":"date-time"}`, `"2026-10-02T12:00:00.123456789Z"`, true},
		{`{"type":"string","format":"date-time"}`, `"yesterday"`, false},
		{`{"enum":[1,2]}`, `3`, false},
	} {
		err := v.check(schema(c.schema), value(c.value), "$")
		if (err == nil) != c.ok {
			t.Errorf("%s against %s: got %v, want ok=%v", c.value, c.schema, err, c.ok)
		}
	}
}

// Every operation has an example, encoded by the Go SDK; each matches the
// schema, so the schema describes what is really on the wire.
func TestStorageExamples(t *testing.T) {
	doc, v := loadSchema(t, "protocol/storage/storage.schema.json")
	ops, _ := doc["x-operations"].([]any)
	if len(ops) == 0 {
		t.Fatal("no operations")
	}
	for _, o := range ops {
		op := o.(map[string]any)
		name := op["name"].(string)
		raw, err := os.ReadFile(filepath.Join("..", "protocol", "storage", "examples", name+".json"))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var ex map[string]any
		if err := decodeNumbers(raw, &ex); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ex["operation"] != name {
			t.Errorf("%s: example is for %v", name, ex["operation"])
		}
		args := map[string]any{"$ref": op["args"]}
		if err := v.check(args, ex["args"], name+".args"); err != nil {
			t.Error(err)
		}
		if err := v.check(op["result"], ex["result"], name+".result"); err != nil {
			t.Error(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join("..", "protocol", "storage", "examples", "*.json"))
	if len(files) != len(ops) {
		t.Errorf("%d examples for %d operations", len(files), len(ops))
	}
}

func TestExecutorReportExample(t *testing.T) {
	doc, v := loadSchema(t, "protocol/storage/executor_report.schema.json")
	raw, err := os.ReadFile(filepath.Join("..", "protocol", "storage", "executor_report.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex any
	if err := decodeNumbers(raw, &ex); err != nil {
		t.Fatal(err)
	}
	if err := v.check(map[string]any{"$ref": doc["$ref"]}, ex, "report"); err != nil {
		t.Error(err)
	}
}
