package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/runnerq/runnerq-spec/schemacheck"
)

type scenario struct {
	Description string            `json:"description"`
	Steps       []json.RawMessage `json:"steps"`
}

func loadScenario(path string) (*scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc scenario
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if sc.Description == "" || len(sc.Steps) == 0 {
		return nil, fmt.Errorf("%s: needs a description and steps", path)
	}
	return &sc, nil
}

// drivers are the named drivers the scenario's steps ask for ("by").
func (sc *scenario) drivers() []string {
	var out []string
	for _, raw := range sc.Steps {
		var s struct {
			By string `json:"by"`
		}
		_ = json.Unmarshal(raw, &s)
		if s.By != "" && !slices.Contains(out, s.By) {
			out = append(out, s.By)
		}
	}
	return out
}

type claim struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

type runner struct {
	db      *pgxpool.Pool
	events  *schemacheck.Schema
	drivers map[string]*driver
	first   *driver
	dsn     string

	queue  string
	ids    map[string]string
	claims map[string]claim
}

func (r *runner) run(ctx context.Context, sc *scenario) error {
	r.queue = "cf_" + randomHex(8)
	r.ids, r.claims = map[string]string{}, map[string]claim{}
	for _, d := range r.drivers {
		rep, err := d.call("open", map[string]any{"dsn": r.dsn, "queue": r.queue}, 2*time.Minute)
		if err != nil {
			return err
		}
		if rep.Error != nil {
			return fmt.Errorf("driver %s: open: %s: %s", d.name, rep.Error.Kind, rep.Error.Message)
		}
	}
	for i, raw := range sc.Steps {
		var step map[string]json.RawMessage
		if err := json.Unmarshal(raw, &step); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		if err := r.step(ctx, step); err != nil {
			return fmt.Errorf("step %d %s: %w", i+1, compactJSON(raw), err)
		}
	}
	return r.checkEvents(ctx)
}

// checkEvents validates every event the scenario wrote against the stored
// events' schema (schema/postgres/events.schema.json): one shape per event
// type, whichever implementation wrote it.
func (r *runner) checkEvents(ctx context.Context) error {
	rows, err := r.db.Query(ctx, `SELECT event_type, COALESCE(detail, 'null'::jsonb)::text FROM runnerq_events WHERE queue_name = $1 ORDER BY id`, r.queue)
	if err != nil {
		return err
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var typ, detail string
		if err := rows.Scan(&typ, &detail); err != nil {
			return err
		}
		if _, ok := r.events.Doc["$defs"].(map[string]any)[typ]; !ok {
			bad = append(bad, fmt.Sprintf("%s: not a known event type", typ))
		} else if err := r.events.CheckJSON(typ, []byte(detail)); err != nil {
			bad = append(bad, fmt.Sprintf("%s %s: %v", typ, detail, err))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("events that break schema/postgres/events.schema.json:\n       %s", strings.Join(bad, "\n       "))
	}
	return nil
}

func (r *runner) step(ctx context.Context, step map[string]json.RawMessage) error {
	switch {
	case step["op"] != nil:
		return r.operation(step)
	case step["expire_lease"] != nil:
		return r.exec(ctx, `UPDATE runnerq_activities SET lease_deadline_ms = (EXTRACT(EPOCH FROM NOW()) * 1000)::bigint - 10000
			WHERE id = $1 AND queue_name = $2`, r.ref(str(step["expire_lease"])), r.queue)
	case step["make_due"] != nil:
		return r.exec(ctx, `UPDATE runnerq_activities SET scheduled_at = NOW() WHERE id = $1 AND queue_name = $2`,
			r.ref(str(step["make_due"])), r.queue)
	case step["backdate"] != nil:
		col := str(step["column"])
		if !slices.Contains([]string{"completed_at", "created_at", "scheduled_at"}, col) {
			return fmt.Errorf("backdate: unknown column %q", col)
		}
		secs, _ := strconv.ParseFloat(string(step["seconds"]), 64)
		return r.exec(ctx, `UPDATE runnerq_activities SET `+col+` = `+col+` - make_interval(secs => $3)
			WHERE id = $1 AND queue_name = $2`, r.ref(str(step["backdate"])), r.queue, secs)
	case step["sleep_ms"] != nil:
		ms, _ := strconv.Atoi(string(step["sleep_ms"]))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return nil
	case step["expect_row"] != nil:
		return r.expectRow(ctx, step["expect_row"])
	case step["expect_events"] != nil:
		return r.expectEvents(ctx, step["expect_events"])
	case step["expect_result"] != nil:
		return r.expectResult(ctx, step["expect_result"])
	case step["expect_absent"] != nil:
		return r.expectAbsent(ctx, str(step["expect_absent"]))
	}
	return fmt.Errorf("unknown step")
}

func (r *runner) exec(ctx context.Context, sql string, args ...any) error {
	tag, err := r.db.Exec(ctx, sql, args...)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("no row")
	}
	return err
}

// operation sends one op to its driver, resolving names in its args, and
// checks the reply against expect.
func (r *runner) operation(step map[string]json.RawMessage) error {
	op := str(step["op"])
	d := r.first
	if by := str(step["by"]); by != "" {
		d = r.drivers[by]
	}
	args := map[string]any{}
	for k, v := range step {
		switch k {
		case "op", "by", "as", "expect":
			continue
		}
		val, err := r.resolveArg(k, v)
		if err != nil {
			return err
		}
		args[k] = val
	}
	if op == "submit" {
		name := str(step["as"])
		if name == "" {
			return fmt.Errorf("submit needs as")
		}
		if r.ids[name] == "" {
			r.ids[name] = newUUID()
		}
		args["id"] = r.ids[name]
	}
	if op == "park" {
		if v, ok := args["wake_in_s"]; ok {
			secs, _ := v.(float64)
			args["wake_at"] = time.Now().UTC().Add(time.Duration(secs * float64(time.Second))).Format("2006-01-02T15:04:05.000Z")
			delete(args, "wake_in_s")
		}
	}
	rep, err := d.call(op, args, 30*time.Second)
	if err != nil {
		return err
	}
	var expect map[string]json.RawMessage
	if raw := step["expect"]; raw != nil {
		if err := json.Unmarshal(raw, &expect); err != nil {
			return fmt.Errorf("expect: %w", err)
		}
	}
	if want := str(expect["error"]); want != "" {
		if rep.Error == nil || rep.Error.Kind != want {
			return fmt.Errorf("want error %s, got %s", want, describe(rep))
		}
		return nil
	}
	if rep.Error != nil {
		return fmt.Errorf("%s: %s", rep.Error.Kind, rep.Error.Message)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(rep.OK, &got)
	if op == "claim" {
		var claims []claim
		_ = json.Unmarshal(got["claims"], &claims)
		var names []string
		_ = json.Unmarshal(step["as"], &names)
		for i, n := range names {
			if i < len(claims) {
				r.claims[n] = claims[i]
			}
		}
		if raw := expect["claims"]; raw != nil {
			var want []string
			_ = json.Unmarshal(raw, &want)
			ids := make([]string, len(claims))
			for i, c := range claims {
				ids[i] = c.ID
			}
			wantIDs := make([]string, len(want))
			for i, n := range want {
				wantIDs[i] = r.ref(n)
			}
			if !slices.Equal(ids, wantIDs) {
				return fmt.Errorf("claims %v, want %v (%v)", r.names(ids), want, wantIDs)
			}
		}
	}
	for k, want := range expect {
		if k == "claims" {
			continue
		}
		if err := r.match(k, want, got[k], time.Now()); err != nil {
			return err
		}
	}
	return nil
}

var activityArgs = []string{"parent", "root", "target", "producer"}

func (r *runner) resolveArg(key string, v json.RawMessage) (any, error) {
	switch {
	case key == "claim" || key == "fence":
		c, ok := r.claims[str(v)]
		if !ok {
			return nil, fmt.Errorf("unknown claim %s", v)
		}
		return c, nil
	case slices.Contains(activityArgs, key):
		return r.ref(str(v)), nil
	case key == "result_id" || key == "id":
		return r.resultID(v)
	case key == "key" && bytes.HasPrefix(bytes.TrimSpace(v), []byte("[")):
		// [key, type]: an encoded business key, for lookup_key.
		var kt []string
		if err := json.Unmarshal(v, &kt); err != nil || len(kt) != 2 {
			return nil, fmt.Errorf("key: want [key, type]")
		}
		return businessKey(kt[0], kt[1]), nil
	}
	var out any
	err := json.Unmarshal(v, &out)
	return out, err
}

// resultID is an activity name, or {"$checkpoint": [name, kind, step]}.
func (r *runner) resultID(v json.RawMessage) (string, error) {
	var m struct {
		Checkpoint []string `json:"$checkpoint"`
	}
	if json.Unmarshal(v, &m) == nil && len(m.Checkpoint) == 3 {
		return checkpointID(r.ref(m.Checkpoint[0]), m.Checkpoint[1], m.Checkpoint[2]), nil
	}
	if s := str(v); s != "" {
		return r.ref(s), nil
	}
	return "", fmt.Errorf("bad id %s", v)
}

// ref is the id a name stands for, choosing one on first use.
func (r *runner) ref(name string) string {
	if r.ids[name] == "" {
		r.ids[name] = newUUID()
	}
	return r.ids[name]
}

func (r *runner) names(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id
		for n, v := range r.ids {
			if v == id {
				out[i] = n
			}
		}
	}
	return out
}

func (r *runner) expectRow(ctx context.Context, raw json.RawMessage) error {
	var want map[string]json.RawMessage
	if err := json.Unmarshal(raw, &want); err != nil {
		return err
	}
	var row map[string]any
	var now time.Time
	err := r.db.QueryRow(ctx, `SELECT to_jsonb(a), NOW() FROM runnerq_activities a WHERE id = $1 AND queue_name = $2`,
		r.ref(str(want["activity"])), r.queue).Scan(&row, &now)
	if err == pgx.ErrNoRows {
		return fmt.Errorf("no activity %s", want["activity"])
	} else if err != nil {
		return err
	}
	for col, w := range want {
		if col == "activity" {
			continue
		}
		v, ok := row[col]
		if !ok {
			return fmt.Errorf("runnerq_activities has no column %s", col)
		}
		if err := r.match(col, w, toJSON(v), now); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) expectEvents(ctx context.Context, raw json.RawMessage) error {
	var want struct {
		Activity string   `json:"activity"`
		Types    []string `json:"types"`
	}
	if err := strictUnmarshal(raw, &want); err != nil {
		return err
	}
	rows, err := r.db.Query(ctx, `SELECT event_type FROM runnerq_events WHERE activity_id = $1 AND queue_name = $2 ORDER BY id`,
		r.resultIDOrRef(want.Activity), r.queue)
	if err != nil {
		return err
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if !slices.Equal(got, want.Types) {
		return fmt.Errorf("events %v, want %v", got, want.Types)
	}
	return nil
}

func (r *runner) resultIDOrRef(name string) string { return r.ref(name) }

func (r *runner) expectResult(ctx context.Context, raw json.RawMessage) error {
	var want map[string]json.RawMessage
	if err := json.Unmarshal(raw, &want); err != nil {
		return err
	}
	id, err := r.resultID(want["id"])
	if err != nil {
		return err
	}
	var row map[string]any
	err = r.db.QueryRow(ctx, `SELECT to_jsonb(x) FROM runnerq_results x WHERE activity_id = $1 AND queue_name = $2`, id, r.queue).Scan(&row)
	if err == pgx.ErrNoRows {
		if string(want["absent"]) == "true" {
			return nil
		}
		return fmt.Errorf("no result %s", want["id"])
	} else if err != nil {
		return err
	}
	for k, w := range want {
		switch k {
		case "id":
			continue
		case "absent":
			return fmt.Errorf("result %s exists: %s", want["id"], toJSON(row))
		case "data_subset":
			if err := subset(w, toJSON(row["data"])); err != nil {
				return fmt.Errorf("data: %w", err)
			}
			continue
		}
		v, ok := row[k]
		if !ok {
			return fmt.Errorf("runnerq_results has no column %s", k)
		}
		if err := r.match(k, w, toJSON(v), time.Now()); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) expectAbsent(ctx context.Context, name string) error {
	id := r.ref(name)
	var n int
	err := r.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM runnerq_activities WHERE id = $1) +
		(SELECT count(*) FROM runnerq_inputs WHERE activity_id = $1) +
		(SELECT count(*) FROM runnerq_events WHERE activity_id = $1) +
		(SELECT count(*) FROM runnerq_results WHERE activity_id = $1 OR owner_activity_id = $1)`, id).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%d rows of %s remain", n, name)
	}
	return nil
}

// match compares a reply field or column with an expected value or matcher.
// SQL NULL and JSON null are the same.
func (r *runner) match(what string, want, got json.RawMessage, now time.Time) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(want, &m) == nil && len(m) == 1 {
		for k, v := range m {
			isNull := len(got) == 0 || string(got) == "null"
			switch k {
			case "$null":
				if !isNull {
					return fmt.Errorf("%s = %s, want null", what, got)
				}
				return nil
			case "$notnull":
				if isNull {
					return fmt.Errorf("%s is null", what)
				}
				return nil
			case "$id":
				want, _ = json.Marshal(r.ref(str(v)))
			case "$token":
				c, ok := r.claims[str(v)]
				if !ok {
					return fmt.Errorf("unknown claim %s", v)
				}
				want, _ = json.Marshal(c.Token)
			case "$checkpoint":
				id, err := r.resultID(want)
				if err != nil {
					return err
				}
				want, _ = json.Marshal(id)
			case "$future", "$past":
				t, err := timeOf(got)
				if err != nil {
					return fmt.Errorf("%s = %s: %w", what, got, err)
				}
				if (k == "$future") != t.After(now) {
					return fmt.Errorf("%s = %s, want %s of %s", what, got, k[1:], now.Format(time.RFC3339Nano))
				}
				return nil
			}
		}
	}
	if (len(got) == 0 || string(got) == "null") && string(want) == "null" {
		return nil
	}
	if !sameJSON(want, got) {
		return fmt.Errorf("%s = %s, want %s", what, orNull(got), want)
	}
	return nil
}

// timeOf reads a timestamp column (RFC 3339) or epoch milliseconds.
func timeOf(raw json.RawMessage) (time.Time, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return time.Parse(time.RFC3339Nano, s)
	}
	var ms int64
	if err := json.Unmarshal(raw, &ms); err != nil {
		return time.Time{}, fmt.Errorf("not a time")
	}
	return time.UnixMilli(ms), nil
}

// subset checks every key in want is in got with an equal value, recursively.
func subset(want, got json.RawMessage) error {
	var w, g map[string]json.RawMessage
	if json.Unmarshal(want, &w) != nil {
		return fmt.Errorf("want is not an object")
	}
	if json.Unmarshal(got, &g) != nil {
		return fmt.Errorf("%s is not an object", orNull(got))
	}
	for k, v := range w {
		gv, ok := g[k]
		if !ok {
			return fmt.Errorf("%s lacks %q", got, k)
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(v, &nested) == nil {
			if err := subset(v, gv); err != nil {
				return err
			}
			continue
		}
		if !sameJSON(v, gv) {
			return fmt.Errorf("%q = %s, want %s", k, gv, v)
		}
	}
	return nil
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func strictUnmarshal(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func toJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func orNull(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

func describe(rep *reply) string {
	if rep.Error != nil {
		return rep.Error.Kind + ": " + rep.Error.Message
	}
	return "ok " + string(rep.OK)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// checkpointID is vectors/checkpoint_id.json's rule.
func checkpointID(activityID, kind, name string) string {
	ns, _ := hex.DecodeString(strings.ReplaceAll(activityID, "-", ""))
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(kind + ":" + name))
	sum := h.Sum(nil)[:16]
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	s := hex.EncodeToString(sum)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// businessKey is vectors/business_key.json's rule.
func businessKey(key, activityType string) string {
	data := strconv.Itoa(len(key)) + ":" + key + activityType
	return "rq:key:v2:" + base64.RawStdEncoding.EncodeToString([]byte(data))
}
