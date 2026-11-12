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
// drivers lists the drivers the scenario names: an operation's "by", and the
// drivers of its bulk steps.
func (sc *scenario) drivers() []string {
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			if n != "" && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	for _, raw := range sc.Steps {
		var s struct {
			By         string `json:"by"`
			SubmitMany *struct {
				By string `json:"by"`
			} `json:"submit_many"`
			Drain *struct {
				By []string `json:"by"`
			} `json:"drain"`
			EachStatus *struct {
				By string `json:"by"`
			} `json:"each_status"`
		}
		_ = json.Unmarshal(raw, &s)
		add(s.By)
		if s.SubmitMany != nil {
			add(s.SubmitMany.By)
		}
		if s.EachStatus != nil {
			add(s.EachStatus.By)
		}
		if s.Drain != nil {
			add(s.Drain.By...)
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
	rows, err := r.db.Query(ctx, `SELECT event_type, COALESCE(detail, 'null'::jsonb)::text, worker_id IS NOT NULL
		FROM runnerq_events WHERE queue_name = $1 ORDER BY id`, r.queue)
	if err != nil {
		return err
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var typ, detail string
		var hasWorker bool
		if err := rows.Scan(&typ, &detail, &hasWorker); err != nil {
			return err
		}
		if _, ok := r.events.Doc["$defs"].(map[string]any)[typ]; !ok {
			bad = append(bad, fmt.Sprintf("%s: not a known event type", typ))
		} else if err := r.events.CheckJSON(typ, []byte(detail)); err != nil {
			bad = append(bad, fmt.Sprintf("%s %s: %v", typ, detail, err))
		} else if strings.Contains(detail, `"started_at"`) && !hasWorker {
			bad = append(bad, fmt.Sprintf("%s %s: an attempt's end without its worker_id", typ, detail))
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
	case step["backdate_events"] != nil:
		secs, _ := strconv.ParseFloat(string(step["seconds"]), 64)
		return r.exec(ctx, `UPDATE runnerq_events SET created_at = created_at - make_interval(secs => $3)
			WHERE activity_id = $1 AND queue_name = $2`, r.ref(str(step["backdate_events"])), r.queue, secs)
	case step["sleep_ms"] != nil:
		ms, _ := strconv.Atoi(string(step["sleep_ms"]))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return nil
	case step["drain"] != nil:
		return r.drain(step["drain"])
	case step["submit_many"] != nil:
		return r.submitMany(step["submit_many"])
	case step["each_status"] != nil:
		return r.eachStatus(step["each_status"])
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
	for in, at := range map[string]string{"wake_in_s": "wake_at", "at_in_s": "at"} {
		if v, ok := args[in]; ok {
			secs, _ := v.(float64)
			args[at] = time.Now().UTC().Add(time.Duration(secs * float64(time.Second))).Format("2006-01-02T15:04:05.000Z")
			delete(args, in)
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
	if raw := expect["items"]; raw != nil {
		if err := r.matchItems(raw, got["items"]); err != nil {
			return err
		}
	}
	for k, want := range expect {
		if k == "claims" || k == "items" {
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
	case key == "ids":
		var names []string
		if err := json.Unmarshal(v, &names); err != nil {
			return nil, fmt.Errorf("ids: want a list of names")
		}
		ids := make([]string, len(names))
		for i, n := range names {
			ids[i] = r.ref(n)
		}
		return ids, nil
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

// matchItems checks a command reply's items: want maps an activity name to
// the fields its item must have, and names every item the reply may hold.
func (r *runner) matchItems(raw, got json.RawMessage) error {
	var want map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &want); err != nil {
		return fmt.Errorf("expect items: %w", err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(got, &items); err != nil {
		return fmt.Errorf("items = %s: %w", orNull(got), err)
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, it := range items {
		byID[str(it["id"])] = it
	}
	if len(byID) != len(items) {
		return fmt.Errorf("items name an activity twice: %s", got)
	}
	for name, fields := range want {
		it, ok := byID[r.ref(name)]
		if !ok {
			return fmt.Errorf("no item for %s in %s", name, got)
		}
		delete(byID, r.ref(name))
		for k, w := range fields {
			if err := r.match(name+"."+k, w, it[k], time.Now()); err != nil {
				return err
			}
		}
	}
	if len(byID) > 0 {
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		return fmt.Errorf("unexpected items for %v", r.names(ids))
	}
	return nil
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

// submitMany submits n activities named prefix0..prefix(n-1).
func (r *runner) submitMany(raw json.RawMessage) error {
	var m struct {
		Prefix string `json:"prefix"`
		N      int    `json:"n"`
		Type   string `json:"type"`
		By     string `json:"by"`
	}
	if err := strictUnmarshal(raw, &m); err != nil {
		return err
	}
	d := r.first
	if m.By != "" {
		d = r.drivers[m.By]
	}
	for i := range m.N {
		id := r.ref(fmt.Sprintf("%s%d", m.Prefix, i))
		rep, err := d.call("submit", map[string]any{"id": id, "type": m.Type}, 30*time.Second)
		if err != nil {
			return err
		}
		if rep.Error != nil {
			return fmt.Errorf("submit: %s: %s", rep.Error.Kind, rep.Error.Message)
		}
	}
	return nil
}

// statuses are what eachStatus creates, in order.
var statuses = []string{"pending", "scheduled", "retrying", "running", "waiting", "completed", "failed", "dead_letter", "cancelled"}

// eachStatus puts one activity in every stored status through a driver:
// <prefix><status>, of type <prefix><status>. The running one is claimed as
// <prefix>running.
func (r *runner) eachStatus(raw json.RawMessage) error {
	var m struct {
		Prefix string `json:"prefix"`
		By     string `json:"by"`
	}
	if err := strictUnmarshal(raw, &m); err != nil {
		return err
	}
	d := r.first
	if m.By != "" {
		d = r.drivers[m.By]
	}
	call := func(op string, args map[string]any) (map[string]json.RawMessage, error) {
		rep, err := d.call(op, args, 30*time.Second)
		if err == nil && rep.Error != nil {
			err = fmt.Errorf("%s: %s: %s", op, rep.Error.Kind, rep.Error.Message)
		}
		if err != nil {
			return nil, err
		}
		var got map[string]json.RawMessage
		_ = json.Unmarshal(rep.OK, &got)
		return got, nil
	}
	claimOne := func(name string) (claim, error) {
		got, err := call("claim", map[string]any{"types": []string{name}, "limit": 1, "lease_ms": 60000})
		if err != nil {
			return claim{}, err
		}
		var claims []claim
		_ = json.Unmarshal(got["claims"], &claims)
		if len(claims) != 1 || claims[0].ID != r.ref(name) {
			return claim{}, fmt.Errorf("could not claim %s", name)
		}
		return claims[0], nil
	}
	for _, status := range statuses {
		name := m.Prefix + status
		submit := map[string]any{"id": r.ref(name), "type": name}
		switch status {
		case "scheduled":
			submit["delay_s"] = 3600
		case "dead_letter":
			submit["max_attempts"] = 1
		}
		if _, err := call("submit", submit); err != nil {
			return err
		}
		switch status {
		case "pending", "scheduled":
			continue
		case "cancelled":
			if _, err := call("command", map[string]any{"kind": "cancel", "ids": []string{r.ref(name)}}); err != nil {
				return err
			}
			continue
		}
		c, err := claimOne(name)
		if err != nil {
			return err
		}
		switch status {
		case "running":
			r.claims[name] = c
		case "retrying", "dead_letter":
			_, err = call("fail", map[string]any{"claim": c, "reason": "boom", "retryable": true})
		case "failed":
			_, err = call("fail", map[string]any{"claim": c, "reason": "boom", "retryable": false})
		case "completed":
			_, err = call("complete", map[string]any{"claim": c, "value": nil})
		case "waiting":
			_, err = call("park", map[string]any{"claim": c, "kind": "sleep", "step": "sleep:nap",
				"wake_at": time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05.000Z")})
		}
		if err != nil {
			return err
		}
	}
	ids := make([]string, len(statuses))
	for i, s := range statuses {
		ids[i] = r.ref(m.Prefix + s)
	}
	rows, err := r.db.Query(context.Background(), `SELECT status FROM runnerq_activities
		WHERE queue_name = $1 AND id = ANY($2::uuid[]) ORDER BY array_position($2::uuid[], id)`, r.queue, ids)
	if err != nil {
		return err
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	want := slices.Clone(statuses)
	want[slices.Index(want, "running")] = "processing"
	if !slices.Equal(got, want) {
		return fmt.Errorf("each_status made %v, want %v", got, want)
	}
	return nil
}

// drain claims from several drivers at once until the queue is empty, and
// checks every one of the named activities was claimed exactly once.
func (r *runner) drain(raw json.RawMessage) error {
	var m struct {
		Types  []string `json:"types"`
		By     []string `json:"by"`
		Limit  int      `json:"limit"`
		Prefix string   `json:"prefix"`
		N      int      `json:"n"`
	}
	if err := strictUnmarshal(raw, &m); err != nil {
		return err
	}
	type result struct {
		ids []string
		err error
	}
	out := make(chan result, len(m.By))
	for _, name := range m.By {
		d := r.drivers[name]
		go func() {
			var ids []string
			for {
				rep, err := d.call("claim", map[string]any{"types": m.Types, "limit": max(m.Limit, 1), "lease_ms": 60000}, 30*time.Second)
				if err == nil && rep.Error != nil {
					err = fmt.Errorf("%s: claim: %s: %s", d.name, rep.Error.Kind, rep.Error.Message)
				}
				if err != nil {
					out <- result{ids, err}
					return
				}
				var got struct {
					Claims []claim `json:"claims"`
				}
				_ = json.Unmarshal(rep.OK, &got)
				if len(got.Claims) == 0 {
					out <- result{ids, nil}
					return
				}
				for _, c := range got.Claims {
					ids = append(ids, c.ID)
				}
			}
		}()
	}
	seen := map[string]int{}
	var counts []string
	for _, name := range m.By {
		res := <-out
		if res.err != nil {
			return res.err
		}
		counts = append(counts, fmt.Sprint(len(res.ids)))
		for _, id := range res.ids {
			seen[id]++
		}
		_ = name
	}
	for i := range m.N {
		name := fmt.Sprintf("%s%d", m.Prefix, i)
		if n := seen[r.ref(name)]; n != 1 {
			return fmt.Errorf("%s claimed %d times (claims per driver: %s)", name, n, strings.Join(counts, ", "))
		}
	}
	if len(seen) != m.N {
		return fmt.Errorf("claimed %d activities, want %d", len(seen), m.N)
	}
	return nil
}
