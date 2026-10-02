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

func TestGenerateSchema(t *testing.T) {
	s, err := loadSchema("../..")
	if err != nil {
		t.Fatal(err)
	}
	goSrc, err := goSchema(s, "spec")
	if err != nil {
		t.Fatalf("Go output does not format: %v", err)
	}
	tsSrc, err := tsSchema(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`{Name: "0001_baseline", SQL: `, `Name: "idx_runnerq_dequeue_order_v2"`, `RetiredColumns: []string{"runnerq_activities.payload"}`} {
		if !strings.Contains(string(goSrc), want) {
			t.Errorf("Go output lacks %q", want)
		}
	}
	for _, want := range []string{`"name": "0001_baseline"`, `export const postgresCatalog: Catalog = {`} {
		if !strings.Contains(string(tsSrc), want) {
			t.Errorf("TypeScript output lacks %q", want)
		}
	}
}

func TestGenerateStorage(t *testing.T) {
	p, err := loadProtocol("../..", "protocol/storage/storage.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, gen := range []func(*protocolDoc, string, string) ([]byte, error){goProtocol, goClient, goServer} {
		if _, err := gen(p, "x", "github.com/alob-mtc/runnerq-go/storage"); err != nil {
			t.Fatalf("Go output does not format: %v", err)
		}
	}
	src, _ := goServer(p, "rpc", "github.com/alob-mtc/runnerq-go/storage")
	for _, want := range []string{"len(a.WorkerID) < 1 || len(a.WorkerID) > 512", "min(a.Timeout, time.Duration(25000000000))", "func validateQueuedActivity(v *storage.QueuedActivity) error", "v.ID == uuid.Nil"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("server lacks %q", want)
		}
	}
	ts, _ := tsProtocol(p, "storage")
	if !strings.Contains(string(ts), "Dequeue: { args: DequeueArgs; result: QueuedActivity | null };") {
		t.Error("TypeScript lacks the Dequeue operation")
	}
	r, err := loadProtocol("../..", "protocol/storage/executor_report.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if src, err := goTypes(r, "protocol"); err != nil || !strings.Contains(string(src), "HeartbeatFailures uint64") {
		t.Fatalf("executor report: %v", err)
	}
}
