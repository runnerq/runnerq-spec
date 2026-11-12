package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ledgerDDL is the table workers write the workload's effects to. It has no
// unique constraint: a step that runs twice must leave two rows to count.
const ledgerDDL = `
CREATE TABLE IF NOT EXISTS runnerq_chaos_ledger (
    id          BIGSERIAL PRIMARY KEY,
    queue_name  TEXT NOT NULL,
    activity_id UUID NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('invoke', 'effect')),
    step        TEXT,
    nonce       UUID,
    worker      TEXT NOT NULL,
    begun_at    TIMESTAMPTZ,
    at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS runnerq_chaos_ledger_activity ON runnerq_chaos_ledger (queue_name, activity_id);
`

// wakeSkew allows for the worker's clock, which sets a sleep's wake time,
// differing from the database's, which times the step after it.
const wakeSkew = time.Second

type activityRow struct {
	id, typ, status string
	parent          string
	completedAt     *time.Time
	input           flowInput
	result          *storedValue
}

type storedValue struct {
	state string
	data  json.RawMessage
	at    time.Time
}

type ledgerRow struct {
	kind, step, nonce, worker string
	begun                     *time.Time
	at                        time.Time
}

type eventRow struct {
	typ string
	at  time.Time
}

type report struct {
	activities, effects, invocations int
	reexecuted                       int // a step ran again before its result was recorded
	stale                            int // a paused worker's step finished after another's was recorded
	requeued, retried, yielded       int
	faults                           string
	failures                         []string
}

func (rep *report) fail(format string, args ...any) {
	rep.failures = append(rep.failures, fmt.Sprintf(format, args...))
}

func (rep *report) summary() string {
	return fmt.Sprintf(`chaos: %d activities, %d invocations, %d step effects
chaos: faults: %s
chaos: storage saw %d lease expiries, %d retries, %d parks
chaos: %d steps re-ran before their result was recorded (allowed: at least once)
chaos: %d effects from paused workers landed after the step was recorded (stale execution)
`, rep.activities, rep.invocations, rep.effects, rep.faults, rep.requeued, rep.retried, rep.yielded, rep.reexecuted, rep.stale)
}

func (rep *report) err() error {
	if len(rep.failures) == 0 {
		return nil
	}
	const show = 40
	lines := rep.failures
	more := ""
	if len(lines) > show {
		more = fmt.Sprintf("\n  … and %d more", len(lines)-show)
		lines = lines[:show]
	}
	return fmt.Errorf("%d invariant violations:\n  %s%s", len(rep.failures), strings.Join(lines, "\n  "), more)
}

// decode returns a stored value as plain JSON.
func decode(data []byte, serialization string) (json.RawMessage, error) {
	switch serialization {
	case "", "json-v1":
		return data, nil
	case "superjson-v1":
		var env struct {
			JSON json.RawMessage `json:"json"`
			Meta json.RawMessage `json:"meta"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
		if len(env.JSON) == 0 {
			return json.RawMessage("null"), nil
		}
		return env.JSON, nil
	}
	return nil, fmt.Errorf("unknown serialization %q", serialization)
}

func (r *run) check(ctx context.Context) (*report, error) {
	rep := &report{faults: r.faultSummary()}
	acts := map[string]*activityRow{}
	var order []string
	rows, err := r.db.Query(ctx, `
		SELECT a.id::text, a.activity_type, a.status, coalesce(a.parent_activity_id::text, ''), a.completed_at,
		       i.payload, i.serialization, res.state, res.data, res.serialization, res.created_at
		FROM runnerq_activities a
		LEFT JOIN runnerq_inputs i ON i.activity_id = a.id
		LEFT JOIN runnerq_results res ON res.activity_id = a.id
		WHERE a.queue_name = $1 ORDER BY a.created_at, a.id`, r.queue)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		a := &activityRow{}
		var payload, resData []byte
		var paySer, resState, resSer *string
		var resAt *time.Time
		if err := rows.Scan(&a.id, &a.typ, &a.status, &a.parent, &a.completedAt, &payload, &paySer, &resState, &resData, &resSer, &resAt); err != nil {
			rows.Close()
			return nil, err
		}
		if payload == nil {
			rep.fail("%s: no input row", a.id)
		} else if in, err := decode(payload, deref(paySer)); err != nil {
			rep.fail("%s: input: %v", a.id, err)
		} else if err := json.Unmarshal(in, &a.input); err != nil {
			rep.fail("%s: input %s: %v", a.id, in, err)
		}
		if resState != nil {
			data, err := decode(resData, deref(resSer))
			if err != nil {
				rep.fail("%s: result: %v", a.id, err)
			}
			a.result = &storedValue{state: *resState, data: data, at: *resAt}
		}
		acts[a.id] = a
		order = append(order, a.id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rep.activities = len(acts)

	// Step checkpoints, by owner and "run:<name>" / "sleep:<name>".
	checkpoints := map[[2]string]storedValue{}
	rows, err = r.db.Query(ctx, `
		SELECT res.owner_activity_id::text, res.step, res.state, res.data, res.serialization, res.created_at
		FROM runnerq_results res JOIN runnerq_activities a ON a.id = res.owner_activity_id
		WHERE a.queue_name = $1 AND res.step IS NOT NULL`, r.queue)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var owner, step, state, ser string
		var data []byte
		var at time.Time
		if err := rows.Scan(&owner, &step, &state, &data, &ser, &at); err != nil {
			rows.Close()
			return nil, err
		}
		v, err := decode(data, ser)
		if err != nil {
			rep.fail("%s: checkpoint %s: %v", owner, step, err)
		}
		checkpoints[[2]string{owner, step}] = storedValue{state: state, data: v, at: at}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ledger := map[string][]ledgerRow{}
	rows, err = r.db.Query(ctx, `
		SELECT activity_id::text, kind, coalesce(step, ''), coalesce(nonce::text, ''), worker, begun_at, at
		FROM runnerq_chaos_ledger WHERE queue_name = $1 ORDER BY id`, r.queue)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var l ledgerRow
		if err := rows.Scan(&id, &l.kind, &l.step, &l.nonce, &l.worker, &l.begun, &l.at); err != nil {
			rows.Close()
			return nil, err
		}
		ledger[id] = append(ledger[id], l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	events := map[string][]eventRow{}
	rows, err = r.db.Query(ctx, `
		SELECT activity_id::text, event_type, created_at FROM runnerq_events
		WHERE queue_name = $1 AND event_type IN ('Yielded', 'Requeued', 'Retrying', 'Signaled', 'Failed', 'DeadLetter')
		ORDER BY id`, r.queue)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var e eventRow
		if err := rows.Scan(&id, &e.typ, &e.at); err != nil {
			rows.Close()
			return nil, err
		}
		events[id] = append(events[id], e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	paused := r.paused
	ids := r.ids
	signals := r.signals
	r.mu.Unlock()

	// Every planned root exists, once.
	rootOf := map[string]int{}
	for i, id := range ids {
		rootOf[id] = i
		if a, ok := acts[id]; !ok {
			rep.fail("root %s (%s): activity %s is missing", r.plan[i].key, r.plan[i].kind, id)
		} else if a.parent != "" {
			rep.fail("root %s: activity %s has a parent", r.plan[i].key, id)
		}
	}
	children := map[string][]*activityRow{}
	for _, id := range order {
		a := acts[id]
		if a.parent == "" {
			if _, ok := rootOf[id]; !ok {
				rep.fail("%s: a root the workload never enqueued", id)
			}
			continue
		}
		if _, ok := acts[a.parent]; !ok {
			rep.fail("%s: parent %s is not in the queue", id, a.parent)
		}
		children[a.parent] = append(children[a.parent], a)
	}

	for _, id := range order {
		a := acts[id]
		r.checkActivity(rep, a, children[id], checkpoints, ledger[id], events[id], paused, rootOf, signals)
	}
	return rep, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (r *run) checkActivity(rep *report, a *activityRow, kids []*activityRow, checkpoints map[[2]string]storedValue,
	ledger []ledgerRow, events []eventRow, paused map[string]bool, rootOf map[string]int, signals map[int]int) {
	label := fmt.Sprintf("%s (%s %s)", a.id, a.typ, a.input.Label)
	failf := func(format string, args ...any) { rep.fail(label+": "+format, args...) }

	// 1. Terminal, and terminal the way the workload ends: completed.
	if a.status != "completed" {
		failf("status %s, want completed", a.status)
		return
	}
	if a.result == nil || a.result.state != "Ok" {
		failf("completed without an Ok result")
		return
	}

	var invocations []ledgerRow
	effects := map[string][]ledgerRow{}
	for _, l := range ledger {
		if l.kind == "invoke" {
			invocations = append(invocations, l)
		} else {
			effects[l.step] = append(effects[l.step], l)
			rep.effects++
		}
	}
	rep.invocations += len(invocations)
	explained := 1
	var signaled []time.Time
	for _, e := range events {
		switch e.typ {
		case "Yielded":
			rep.yielded++
			explained++
		case "Requeued":
			rep.requeued++
			explained++
		case "Retrying":
			rep.retried++
			explained++
		case "Signaled":
			signaled = append(signaled, e.at)
		}
	}

	// 2. No recorded step's effect happens twice.
	stepNonce := map[string]string{}
	stepTime := map[string]time.Time{}
	for _, name := range slices.Concat(a.input.Steps, a.input.After) {
		cp, ok := checkpoints[[2]string{a.id, "run:" + name}]
		if !ok {
			failf("step %s has no checkpoint", name)
			continue
		}
		var nonce string
		if cp.state != "Ok" || json.Unmarshal(cp.data, &nonce) != nil {
			failf("step %s checkpoint is %s %s", name, cp.state, cp.data)
			continue
		}
		stepNonce[name], stepTime[name] = nonce, cp.at
		found := false
		for _, e := range effects[name] {
			switch {
			case e.nonce == nonce:
				found = true
			case !e.at.After(cp.at):
				rep.reexecuted++
			case paused[e.worker]:
				rep.stale++
			case e.begun != nil && e.begun.After(cp.at):
				failf("step %s ran again on %s at %s, after its result was recorded at %s",
					name, e.worker, e.begun.Format(time.RFC3339Nano), cp.at.Format(time.RFC3339Nano))
			default:
				failf("step %s's effect on %s landed at %s, after another execution's result was recorded at %s",
					name, e.worker, e.at.Format(time.RFC3339Nano), cp.at.Format(time.RFC3339Nano))
			}
		}
		if !found {
			failf("step %s recorded %s, which no execution of it produced", name, nonce)
		}
	}

	// 3. The result is what the recorded steps, signal and children say.
	var res flowResult
	if err := json.Unmarshal(a.result.data, &res); err != nil {
		failf("result %s: %v", a.result.data, err)
		return
	}
	if res.Label != a.input.Label {
		failf("result label %q, want %q", res.Label, a.input.Label)
	}
	for name, nonce := range stepNonce {
		if res.Steps[name] != nonce {
			failf("result has step %s = %q, but %q was recorded", name, res.Steps[name], nonce)
		}
	}

	// 4. Parked work woke exactly once: every invocation is explained by the
	// first claim, a park, a lease expiry or a retry, and nothing ran it
	// after it finished.
	if len(invocations) > explained {
		failf("the handler was invoked %d times, but only %d attempts are explained by parks, lease expiries and retries", len(invocations), explained)
	}
	for _, inv := range invocations {
		if a.completedAt != nil && inv.at.After(*a.completedAt) && !paused[inv.worker] {
			failf("invoked on %s at %s, after it completed at %s", inv.worker, inv.at.Format(time.RFC3339Nano), a.completedAt.Format(time.RFC3339Nano))
		}
	}
	// The steps after a wait began only once the wait was over.
	waitOver := func(what string, at time.Time, skew time.Duration) {
		for _, name := range a.input.After {
			for _, e := range effects[name] {
				if e.begun != nil && e.begun.Add(skew).Before(at) && !paused[e.worker] {
					failf("step %s began at %s, before %s at %s", name, e.begun.Format(time.RFC3339Nano), what, at.Format(time.RFC3339Nano))
				}
			}
		}
	}
	switch {
	case a.input.SleepMS > 0:
		cp, ok := checkpoints[[2]string{a.id, "sleep:nap"}]
		var wake struct {
			WakeAt time.Time `json:"wake_at"`
		}
		if !ok || json.Unmarshal(cp.data, &wake) != nil {
			failf("no sleep checkpoint for nap")
			break
		}
		waitOver("the sleep's wake time", wake.WakeAt, wakeSkew)
	case a.input.Signal != "":
		i, isRoot := rootOf[a.id]
		if !isRoot {
			break
		}
		sent := signals[i]
		if len(signaled) == 0 || len(signaled) > sent {
			failf("%d Signaled events for %d sends", len(signaled), sent)
		}
		if len(signaled) > 0 {
			waitOver("the signal", signaled[0], 0)
		}
		if !jsonEqual(res.Signal, r.plan[i].signal) {
			failf("received signal %s, sent %s", res.Signal, r.plan[i].signal)
		}
	case len(a.input.Children) > 0:
		if len(kids) != len(a.input.Children) {
			failf("%d children, want %d", len(kids), len(a.input.Children))
		}
		var last time.Time
		byLabel := map[string]*activityRow{}
		for _, k := range kids {
			if _, dup := byLabel[k.input.Label]; dup {
				failf("child %s spawned twice", k.input.Label)
			}
			byLabel[k.input.Label] = k
			if k.completedAt != nil && k.completedAt.After(last) {
				last = *k.completedAt
			}
		}
		if len(res.Children) != len(a.input.Children) {
			failf("result has %d children, want %d", len(res.Children), len(a.input.Children))
			break
		}
		for i, spec := range a.input.Children {
			k := byLabel[spec.Payload.Label]
			if k == nil {
				failf("child %s was never spawned", spec.Payload.Label)
				continue
			}
			if k.typ != spec.Type {
				failf("child %s has type %s, want %s", spec.Payload.Label, k.typ, spec.Type)
			}
			if k.result != nil && !jsonEqual(res.Children[i], k.result.data) {
				failf("child %s's result in the parent %s differs from its own %s", spec.Payload.Label, res.Children[i], k.result.data)
			}
		}
		waitOver("its last child completed", last, 0)
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
