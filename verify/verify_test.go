// Package verify checks every vector file against a reference implementation
// written from the vector descriptions, using only the standard library.
// Run with -update to fill in outputs after adding cases.
package verify

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite vector outputs from the reference implementation")

type vectorFile struct {
	Description string `json:"description"`
	Cases       []struct {
		Name   string          `json:"name"`
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"output"`
	} `json:"cases"`
}

// check runs fn over every case of a vector file and compares (or, with
// -update, rewrites) the outputs.
func check[In any](t *testing.T, path string, fn func(In) (any, error)) {
	t.Helper()
	full := filepath.Join("..", path)
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	var f vectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if f.Description == "" || len(f.Cases) == 0 {
		t.Fatalf("%s: needs a description and cases", path)
	}
	seen := map[string]bool{}
	for i := range f.Cases {
		c := &f.Cases[i]
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("%s: case %d: missing or duplicate name %q", path, i, c.Name)
		}
		seen[c.Name] = true
		var in In
		dec := json.NewDecoder(bytes.NewReader(c.Input))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			t.Fatalf("%s: %s: input: %v", path, c.Name, err)
		}
		got, err := fn(in)
		if err != nil {
			t.Fatalf("%s: %s: %v", path, c.Name, err)
		}
		gotJSON, _ := json.Marshal(got)
		if *update {
			c.Output = gotJSON
			continue
		}
		if !sameJSON(gotJSON, c.Output) {
			t.Errorf("%s: %s: got %s, vector says %s", path, c.Name, gotJSON, c.Output)
		}
	}
	if *update {
		if err := os.WriteFile(full, encodeVectors(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// encodeVectors writes one case per line, the layout the files are kept in.
func encodeVectors(f vectorFile) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{\n  \"description\": %s,\n  \"cases\": [\n", marshal(f.Description))
	for i, c := range f.Cases {
		fmt.Fprintf(&b, "    { \"name\": %s, \"input\": %s, \"output\": %s }", marshal(c.Name), compact(c.Input), compact(c.Output))
		if i < len(f.Cases)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("  ]\n}\n")
	return b.Bytes()
}

// compact renders JSON on one line, spaced like the hand-written files:
// { "a": 1, "b": [1, 2] }.
func compact(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	s := marshal(v)
	var out strings.Builder
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			out.WriteByte(ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch {
		case ch == '"':
			inString = true
			out.WriteByte(ch)
		case ch == ':' || ch == ',':
			out.WriteByte(ch)
			out.WriteByte(' ')
		case ch == '{' && i+1 < len(s) && s[i+1] != '}':
			out.WriteString("{ ")
		case ch == '}' && i > 0 && s[i-1] != '{':
			out.WriteString(" }")
		default:
			out.WriteByte(ch)
		}
	}
	return out.String()
}

// marshal encodes like json.Marshal but leaves <, > and & literal.
func marshal(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// uuidV5 is RFC 9562 section 5.5.
func uuidV5(namespace, name string) (string, error) {
	ns, err := hex.DecodeString(strings.ReplaceAll(namespace, "-", ""))
	if err != nil || len(ns) != 16 {
		return "", fmt.Errorf("bad namespace %q", namespace)
	}
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name))
	sum := h.Sum(nil)[:16]
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	s := hex.EncodeToString(sum)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

func businessKey(key, activityType string) string {
	data := strconv.Itoa(len(key)) + ":" + key + activityType
	return "rq:key:v2:" + base64.RawStdEncoding.EncodeToString([]byte(data))
}

func decodeBusinessKey(stored string) (key, activityType string, ok bool) {
	rest, found := strings.CutPrefix(stored, "rq:key:v2:")
	if !found {
		return "", "", false
	}
	data, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil {
		return "", "", false
	}
	length, body, found := strings.Cut(string(data), ":")
	n, err := strconv.Atoi(length)
	if !found || err != nil || n < 0 || n > len(body) {
		return "", "", false
	}
	key, activityType = body[:n], body[n:]
	return key, activityType, businessKey(key, activityType) == stored
}

func TestCheckpointID(t *testing.T) {
	check(t, "vectors/checkpoint_id.json", func(in struct {
		ActivityID string `json:"activity_id"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
	}) (any, error) {
		if in.Name == "" {
			return nil, fmt.Errorf("empty name")
		}
		return uuidV5(in.ActivityID, in.Kind+":"+in.Name)
	})
}

func TestBusinessKey(t *testing.T) {
	check(t, "vectors/business_key.json", func(in struct {
		Key          string `json:"key"`
		ActivityType string `json:"activity_type"`
	}) (any, error) {
		if in.Key == "" || in.ActivityType == "" {
			return nil, fmt.Errorf("empty key or type")
		}
		got := businessKey(in.Key, in.ActivityType)
		if k, typ, ok := decodeBusinessKey(got); !ok || k != in.Key || typ != in.ActivityType {
			return nil, fmt.Errorf("%q does not decode back", got)
		}
		return got, nil
	})
}

func TestApplicationKey(t *testing.T) {
	check(t, "vectors/application_key.json", func(in struct {
		Stored       string `json:"stored"`
		ActivityType string `json:"activity_type"`
	}) (any, error) {
		if in.Stored == "" || strings.HasPrefix(in.Stored, "rq:step:") {
			return "", nil
		}
		if key, typ, ok := decodeBusinessKey(in.Stored); ok {
			if typ == in.ActivityType {
				return key, nil
			}
			return in.Stored, nil
		}
		return in.Stored, nil
	})
}

func TestStepKey(t *testing.T) {
	check(t, "vectors/step_key.json", func(in struct {
		RootID   string `json:"root_id"`
		ParentID string `json:"parent_id"`
		Step     string `json:"step"`
	}) (any, error) {
		for _, id := range []string{in.RootID, in.ParentID} {
			if id != strings.ToLower(id) || len(id) != 36 {
				return nil, fmt.Errorf("%q is not a lowercase canonical UUID", id)
			}
		}
		return "rq:step:" + in.RootID + ":" + in.ParentID + ":" + in.Step, nil
	})
}

func TestPlainJSON(t *testing.T) {
	check(t, "serialization/vectors/plain_json.json", func(in struct {
		Serialization string          `json:"serialization"`
		Data          json.RawMessage `json:"data"`
	}) (any, error) {
		if in.Serialization != "superjson-v1" {
			return in.Data, nil
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(in.Data, &envelope) != nil {
			return in.Data, nil
		}
		if part, ok := envelope["json"]; ok {
			return part, nil
		}
		return in.Data, nil
	})
}

func TestAttemptsRemain(t *testing.T) {
	check(t, "vectors/attempts_remain.json", func(in struct {
		RetryCount int64 `json:"retry_count"`
		MaxRetries int64 `json:"max_retries"`
	}) (any, error) {
		if in.RetryCount < 0 || in.MaxRetries < 0 {
			return nil, fmt.Errorf("negative input")
		}
		return in.MaxRetries == 0 || in.RetryCount+1 < in.MaxRetries, nil
	})
}

func TestRetryDelay(t *testing.T) {
	check(t, "vectors/retry_delay.json", func(in struct {
		RetryCount    int64 `json:"retry_count"`
		RetryDelay    int64 `json:"retry_delay_seconds"`
		MaxRetryDelay int64 `json:"max_retry_delay_seconds"`
	}) (any, error) {
		if in.RetryCount < 0 || in.RetryDelay < 0 || in.RetryDelay >= 1<<31 || in.MaxRetryDelay < 0 || in.MaxRetryDelay >= 1<<31 {
			return nil, fmt.Errorf("input outside the spec's range")
		}
		limit := in.MaxRetryDelay
		if limit == 0 {
			limit = 3600
		}
		delay := new(big.Int).Lsh(big.NewInt(in.RetryDelay), uint(in.RetryCount+1))
		if delay.Cmp(big.NewInt(limit)) > 0 {
			return limit, nil
		}
		return delay.Int64(), nil
	})
}

func TestCanonicalStatus(t *testing.T) {
	check(t, "vectors/canonical_status.json", func(in struct {
		Status string `json:"status"`
	}) (any, error) {
		switch in.Status {
		case "processing":
			return "running", nil
		case "retrying":
			return "scheduled", nil
		}
		return in.Status, nil
	})
}

// The event table is the canonical_event cases themselves. These check the
// fallback rule there, and that internal_events is the table's inverse.
func eventTable(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "vectors", "canonical_event.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Input struct {
				EventType string `json:"event_type"`
			} `json:"input"`
			Output string `json:"output"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	table := map[string]string{}
	for _, c := range f.Cases {
		if c.Output != "other."+strings.ToLower(c.Input.EventType) {
			table[c.Input.EventType] = c.Output
		}
	}
	return table
}

func TestCanonicalEvent(t *testing.T) {
	table := eventTable(t)
	check(t, "vectors/canonical_event.json", func(in struct {
		EventType string `json:"event_type"`
	}) (any, error) {
		if c, ok := table[in.EventType]; ok {
			if !strings.Contains(c, ".") || strings.HasPrefix(c, "other.") {
				return nil, fmt.Errorf("%q is not a canonical type", c)
			}
			return c, nil
		}
		return "other." + strings.ToLower(in.EventType), nil
	})
}

func TestInternalEvents(t *testing.T) {
	table := eventTable(t)
	check(t, "vectors/internal_events.json", func(in struct {
		Type string `json:"type"`
	}) (any, error) {
		out := []string{}
		for internal, canonical := range table {
			if canonical == in.Type {
				out = append(out, internal)
			}
		}
		if rest, ok := strings.CutPrefix(in.Type, "other."); ok && len(out) == 0 {
			out = append(out, rest)
		}
		slices.Sort(out)
		return out, nil
	})
}
