package main

import (
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	s, err := load("../..")
	if err != nil {
		t.Fatal(err)
	}
	goSrc, err := goSource(s, "spec")
	if err != nil {
		t.Fatalf("Go output does not format: %v", err)
	}
	tsSrc, err := tsSource(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"const SchemaAdvisoryLockKey int64 = 5932734182207934753", `const SerializationJSON = "json-v1"`, "const DefaultMaxRetryDelaySeconds = 3600", "StorageErrorKindUnsupported         = 12"} {
		if !strings.Contains(string(goSrc), want) {
			t.Errorf("Go output lacks %q", want)
		}
	}
	for _, want := range []string{`export const schemaAdvisoryLockKey = "5932734182207934753";`, `export const stepKeyPrefix = "rq:step:";`, "export const defaultMaxRetryDelaySeconds = 3600;", "  unsupported: 12,"} {
		if !strings.Contains(string(tsSrc), want) {
			t.Errorf("TypeScript output lacks %q", want)
		}
	}
}
