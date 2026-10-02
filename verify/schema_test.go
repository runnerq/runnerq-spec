package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runnerq/runnerq-spec/schemacheck"
)

func loadSchema(t *testing.T, path string) (map[string]any, *schemacheck.Schema) {
	t.Helper()
	s, err := schemacheck.Load(filepath.Join("..", path))
	if err != nil {
		t.Fatal(err)
	}
	return s.Doc, s
}

var decodeNumbers = schemacheck.Decode

// The validator itself: one value per rule it enforces.
func TestValidator(t *testing.T) {
	v := schemacheck.New(map[string]any{"U": map[string]any{"type": "string", "format": "uuid"}})
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
		err := v.Check(schema(c.schema), value(c.value), "$")
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
		if err := v.Check(args, ex["args"], name+".args"); err != nil {
			t.Error(err)
		}
		if err := v.Check(op["result"], ex["result"], name+".result"); err != nil {
			t.Error(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join("..", "protocol", "storage", "examples", "*.json"))
	if len(files) != len(ops) {
		t.Errorf("%d examples for %d operations", len(files), len(ops))
	}
}

// Every conductor message has an example: its data, and a request's response.
func TestConductorExamples(t *testing.T) {
	doc, v := loadSchema(t, "protocol/conductor/conductor.schema.json")
	msgs, _ := doc["x-messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	for _, m := range msgs {
		msg := m.(map[string]any)
		name := msg["type"].(string)
		raw, err := os.ReadFile(filepath.Join("..", "protocol", "conductor", "examples", name+".json"))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var ex map[string]any
		if err := decodeNumbers(raw, &ex); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ex["type"] != name {
			t.Errorf("%s: example is for %v", name, ex["type"])
		}
		if err := v.Check(map[string]any{"$ref": msg["data"]}, ex["data"], name+".data"); err != nil {
			t.Error(err)
		}
		resp, hasResp := ex["response"]
		switch {
		case msg["kind"] == "req" && !hasResp:
			t.Errorf("%s: a request's example needs a response", name)
		case msg["kind"] == "req":
			if err := v.Check(map[string]any{"$ref": msg["response"]}, resp, name+".response"); err != nil {
				t.Error(err)
			}
		case hasResp:
			t.Errorf("%s: an event has no response", name)
		}
	}
	files, _ := filepath.Glob(filepath.Join("..", "protocol", "conductor", "examples", "*.json"))
	if len(files) != len(msgs) {
		t.Errorf("%d examples for %d messages", len(files), len(msgs))
	}
}

func TestExecutorReportExample(t *testing.T) {
	_, v := loadSchema(t, "protocol/conductor/conductor.schema.json")
	raw, err := os.ReadFile(filepath.Join("..", "protocol", "conductor", "executor_report.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex any
	if err := decodeNumbers(raw, &ex); err != nil {
		t.Fatal(err)
	}
	if err := v.Check(map[string]any{"$ref": "#/$defs/ExecutorReport"}, ex, "report"); err != nil {
		t.Error(err)
	}
}
