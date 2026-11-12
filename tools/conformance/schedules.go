package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// truncAgo is date_trunc(unit, now in UTC) - interval, from the database clock.
func (r *runner) truncAgo(unit, interval string) (time.Time, error) {
	var at time.Time
	err := r.db.QueryRow(context.Background(),
		`SELECT (date_trunc($1, NOW() AT TIME ZONE 'UTC') - $2::interval) AT TIME ZONE 'UTC'`, unit, interval).Scan(&at)
	return at, err
}

// failWrites makes every insert or update of the table in this scenario's
// queue fail (on), or stops it (off): a write failing midway through a
// transaction.
func (r *runner) failWrites(ctx context.Context, table string, on bool) error {
	if !slices.Contains([]string{"runnerq_schedules", "runnerq_activities", "runnerq_inputs", "runnerq_idempotency"}, table) {
		return fmt.Errorf("fail_writes: unknown table %q", table)
	}
	name := "rq_fail_" + r.queue + "_" + strings.TrimPrefix(table, "runnerq_")
	if !on {
		_, err := r.db.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON %[2]s; DROP FUNCTION IF EXISTS %[1]s()`, name, table))
		return err
	}
	r.failing = append(r.failing, table)
	_, err := r.db.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.queue_name = '%[3]s' THEN RAISE EXCEPTION 'conformance: writes to %[2]s fail'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER %[1]s BEFORE INSERT OR UPDATE ON %[2]s FOR EACH ROW EXECUTE FUNCTION %[1]s();`, name, table, r.queue))
	return err
}

func (r *runner) expectSchedule(ctx context.Context, raw json.RawMessage) error {
	var want map[string]json.RawMessage
	if err := json.Unmarshal(raw, &want); err != nil {
		return err
	}
	var row map[string]any
	var now time.Time
	err := r.db.QueryRow(ctx, `SELECT to_jsonb(s), NOW() FROM runnerq_schedules s WHERE queue_name = $1 AND name = $2`,
		r.queue, str(want["name"])).Scan(&row, &now)
	if err == pgx.ErrNoRows {
		if string(want["absent"]) == "true" {
			return nil
		}
		return fmt.Errorf("no schedule %s", want["name"])
	} else if err != nil {
		return err
	}
	for col, w := range want {
		switch col {
		case "name":
			continue
		case "absent":
			return fmt.Errorf("schedule %s exists", want["name"])
		}
		v, ok := row[col]
		if !ok {
			return fmt.Errorf("runnerq_schedules has no column %s", col)
		}
		if err := r.match(col, w, toJSON(v), now); err != nil {
			return err
		}
	}
	return nil
}

var fireTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// expectFires checks the activities of a type in the queue: each was fired by
// the schedule (its key is schedule:<name>:<fire time>), and together they
// are `count` activities, or exactly the fire times date_trunc(trunc, now) -
// ago. Every one's runnerq_activities columns match `row` and its input
// `payload`.
func (r *runner) expectFires(ctx context.Context, raw json.RawMessage) error {
	var want struct {
		Schedule string                     `json:"schedule"`
		Type     string                     `json:"type"`
		Count    *int                       `json:"count"`
		Trunc    string                     `json:"trunc"`
		Ago      []string                   `json:"ago"`
		Row      map[string]json.RawMessage `json:"row"`
		Payload  json.RawMessage            `json:"payload"`
	}
	if err := strictUnmarshal(raw, &want); err != nil {
		return err
	}
	rows, err := r.db.Query(ctx, `SELECT COALESCE(a.idempotency_key, ''), to_jsonb(a), COALESCE(i.payload, 'null'::jsonb)::text, NOW()
		FROM runnerq_activities a LEFT JOIN runnerq_inputs i ON i.activity_id = a.id AND i.queue_name = a.queue_name
		WHERE a.queue_name = $1 AND a.activity_type = $2`, r.queue, want.Type)
	if err != nil {
		return err
	}
	defer rows.Close()
	var times []string
	for rows.Next() {
		var key, payload string
		var row map[string]any
		var now time.Time
		if err := rows.Scan(&key, &row, &payload, &now); err != nil {
			return err
		}
		app, ok := decodeBusinessKey(key, want.Type)
		at, isFire := strings.CutPrefix(app, "schedule:"+want.Schedule+":")
		if !ok || !isFire || !fireTimeRe.MatchString(at) {
			return fmt.Errorf("activity %v has key %q, not schedule:%s:<RFC 3339 UTC>", row["id"], key, want.Schedule)
		}
		times = append(times, at)
		for col, w := range want.Row {
			v, ok := row[col]
			if !ok {
				return fmt.Errorf("runnerq_activities has no column %s", col)
			}
			if err := r.match(col, w, toJSON(v), now); err != nil {
				return fmt.Errorf("fire %s: %w", at, err)
			}
		}
		if want.Payload != nil && !sameJSON(want.Payload, json.RawMessage(payload)) {
			return fmt.Errorf("fire %s: payload %s, want %s", at, payload, want.Payload)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	slices.Sort(times)
	if len(slices.Compact(slices.Clone(times))) != len(times) {
		return fmt.Errorf("a fire time was enqueued twice: %v", times)
	}
	if want.Count != nil && len(times) != *want.Count {
		return fmt.Errorf("%d fires %v, want %d", len(times), times, *want.Count)
	}
	if want.Ago != nil {
		var expected []string
		for _, ago := range want.Ago {
			at, err := r.truncAgo(want.Trunc, ago)
			if err != nil {
				return err
			}
			expected = append(expected, at.UTC().Format(time.RFC3339))
		}
		slices.Sort(expected)
		if !slices.Equal(times, expected) {
			return fmt.Errorf("fires %v, want %v", times, expected)
		}
	}
	return nil
}

// decodeBusinessKey inverts businessKey for keys of the given type.
func decodeBusinessKey(stored, activityType string) (string, bool) {
	enc, ok := strings.CutPrefix(stored, "rq:key:v2:")
	if !ok {
		return "", false
	}
	data, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	n, rest, ok := strings.Cut(string(data), ":")
	length, err := strconv.Atoi(n)
	if !ok || err != nil || length > len(rest) || rest[length:] != activityType {
		return "", false
	}
	return rest[:length], true
}

// fireRace has every listed driver fire the queue's due schedules `rounds`
// times, all at once.
func (r *runner) fireRace(raw json.RawMessage) error {
	var m struct {
		By     []string `json:"by"`
		Rounds int      `json:"rounds"`
	}
	if err := strictUnmarshal(raw, &m); err != nil {
		return err
	}
	errs := make(chan error, len(m.By))
	for _, name := range m.By {
		d := r.drivers[name]
		go func() {
			for range max(m.Rounds, 1) {
				rep, err := d.call("fire_schedules", map[string]any{}, 30*time.Second)
				if err == nil && rep.Error != nil {
					err = fmt.Errorf("%s: fire_schedules: %s: %s", d.name, rep.Error.Kind, rep.Error.Message)
				}
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for range m.By {
		if err := <-errs; err != nil {
			return err
		}
	}
	return nil
}
