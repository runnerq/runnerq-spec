package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// backdateLimit moves a limit's refilled_at into the past, as if that many
// seconds had passed since its bucket was last brought up to date.
func (r *runner) backdateLimit(ctx context.Context, activityType string, seconds json.RawMessage) error {
	secs, err := strconv.ParseFloat(string(seconds), 64)
	if err != nil {
		return fmt.Errorf("backdate_limit: seconds: %w", err)
	}
	return r.exec(ctx, `UPDATE runnerq_limits SET refilled_at = refilled_at - make_interval(secs => $3)
		WHERE queue_name = $1 AND activity_type = $2`, r.queue, activityType, secs)
}

// expectLimit checks a runnerq_limits row's columns (type names it), or that
// it is absent.
func (r *runner) expectLimit(ctx context.Context, raw json.RawMessage) error {
	var want map[string]json.RawMessage
	if err := json.Unmarshal(raw, &want); err != nil {
		return err
	}
	var row map[string]any
	var now time.Time
	err := r.db.QueryRow(ctx, `SELECT to_jsonb(l), NOW() FROM runnerq_limits l WHERE queue_name = $1 AND activity_type = $2`,
		r.queue, str(want["type"])).Scan(&row, &now)
	if err == pgx.ErrNoRows {
		if string(want["absent"]) == "true" {
			return nil
		}
		return fmt.Errorf("no limit for %s", want["type"])
	} else if err != nil {
		return err
	}
	for col, w := range want {
		switch col {
		case "type":
			continue
		case "absent":
			return fmt.Errorf("limit for %s exists", want["type"])
		}
		v, ok := row[col]
		if !ok {
			return fmt.Errorf("runnerq_limits has no column %s", col)
		}
		if err := r.match(col, w, toJSON(v), now); err != nil {
			return err
		}
	}
	return nil
}

// limitRace has every listed driver claim the type and complete what it
// claimed, all at once, until all n activities named prefix0..prefix(n-1)
// have completed. Meanwhile it samples the type's running count in SQL and
// fails if it ever exceeds max_running.
func (r *runner) limitRace(ctx context.Context, raw json.RawMessage) error {
	var m struct {
		Type       string   `json:"type"`
		By         []string `json:"by"`
		Limit      int      `json:"limit"`
		Prefix     string   `json:"prefix"`
		N          int      `json:"n"`
		MaxRunning int      `json:"max_running"`
	}
	if err := strictUnmarshal(raw, &m); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var done atomic.Int64
	var peak, samples atomic.Int64
	sampleErr := make(chan error, 1)
	sampling, stopSampling := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		for sampling.Err() == nil {
			var n int64
			err := r.db.QueryRow(sampling, `SELECT count(*) FROM runnerq_activities
				WHERE queue_name = $1 AND activity_type = $2 AND status = 'processing'`, r.queue, m.Type).Scan(&n)
			if err != nil {
				if sampling.Err() == nil {
					sampleErr <- err
				}
				return
			}
			samples.Add(1)
			if n > peak.Load() {
				peak.Store(n)
			}
		}
	})

	type result struct {
		ids []string
		err error
	}
	out := make(chan result, len(m.By))
	for _, name := range m.By {
		d := r.drivers[name]
		go func() {
			var ids []string
			call := func(op string, args map[string]any) (json.RawMessage, error) {
				rep, err := d.call(op, args, 30*time.Second)
				if err == nil && rep.Error != nil {
					err = fmt.Errorf("%s: %s: %s: %s", d.name, op, rep.Error.Kind, rep.Error.Message)
				}
				if err != nil {
					return nil, err
				}
				return rep.OK, nil
			}
			for done.Load() < int64(m.N) {
				if ctx.Err() != nil {
					out <- result{ids, fmt.Errorf("%s: %d of %d completed: %w", d.name, done.Load(), m.N, ctx.Err())}
					return
				}
				ok, err := call("claim", map[string]any{"types": []string{m.Type}, "limit": max(m.Limit, 1), "lease_ms": 60000})
				if err != nil {
					out <- result{ids, err}
					return
				}
				var got struct {
					Claims []claim `json:"claims"`
				}
				_ = json.Unmarshal(ok, &got)
				if len(got.Claims) == 0 {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				for _, c := range got.Claims {
					ids = append(ids, c.ID)
					if _, err := call("complete", map[string]any{"claim": c, "value": nil}); err != nil {
						out <- result{ids, err}
						return
					}
					done.Add(1)
				}
			}
			out <- result{ids, nil}
		}()
	}
	seen := map[string]int{}
	var counts []string
	var firstErr error
	for range m.By {
		res := <-out
		if res.err != nil && firstErr == nil {
			firstErr = res.err
			cancel()
		}
		counts = append(counts, fmt.Sprint(len(res.ids)))
		for _, id := range res.ids {
			seen[id]++
		}
	}
	stopSampling()
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	select {
	case err := <-sampleErr:
		return fmt.Errorf("sampling the running count: %w", err)
	default:
	}
	if p := peak.Load(); p > int64(m.MaxRunning) {
		return fmt.Errorf("%d activities of %s were running at once, over the limit of %d (%d samples)", p, m.Type, m.MaxRunning, samples.Load())
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
