// Command chaos runs a seeded workload on a fleet of SDK workers while it
// kills, pauses and restarts things, then checks in SQL that nothing was lost
// and no recorded step ran twice. See README.md for the worker contract.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type commands map[string]string

func (c commands) String() string { return fmt.Sprint(map[string]string(c)) }
func (c commands) Set(v string) error {
	name, cmd, ok := strings.Cut(v, "=")
	if !ok || name == "" || cmd == "" {
		return fmt.Errorf("want name=command, got %q", v)
	}
	c[name] = cmd
	return nil
}

type run struct {
	seed        int64
	dsn         string
	queue       string
	logs        string
	concurrency int
	lease       time.Duration
	pause       time.Duration
	pgRestart   string
	verbose     bool

	db      *pgxpool.Pool
	sdks    []string
	workers []*worker
	clients map[string]*client
	plan    []root
	faults  []fault

	mu       sync.Mutex
	ids      map[int]string // root index → activity id
	signals  map[int]int    // root index → signal sends attempted
	paused   map[string]bool
	injected map[faultKind]int
	start    time.Time
}

func main() {
	workerCmds := commands{}
	seed := flag.Int64("seed", 0, "run seed (default: random); a failing run prints its seed")
	dsn := flag.String("dsn", os.Getenv("RUNNERQ_TEST_DSN"), "Postgres connection string")
	flag.Var(workerCmds, "worker", "sdk=command, e.g. go=./chaosworker; repeatable, one per SDK")
	n := flag.Int("n", 3, "worker processes in all, spread over the SDKs")
	roots := flag.Int("roots", 40, "root activities in the workload")
	duration := flag.Duration("duration", 60*time.Second, "how long faults are injected; the workload is enqueued in its first half")
	faultList := flag.String("faults", "kill", "faults to inject, comma-separated: kill, pause, pg ('' for none)")
	every := flag.Duration("fault-every", 4*time.Second, "mean time between faults")
	pause := flag.Duration("pause", 75*time.Second, "how long a paused worker stays stopped; longer than any SDK's lease")
	pgRestart := flag.String("pg-restart", "", "shell command that restarts Postgres (fault pg), e.g. 'docker restart runnerq-test-pg'")
	settle := flag.Duration("settle", 5*time.Minute, "how long the workload may take to finish after the fault window")
	concurrency := flag.Int("concurrency", 8, "activities each worker runs at once")
	lease := flag.Duration("lease", 15*time.Second, "lease asked of workers (RUNNERQ_CHAOS_LEASE_MS), where the SDK lets it be set")
	queue := flag.String("queue", "", "queue name (default: a fresh one per run)")
	logs := flag.String("logs", "", "directory for worker logs (default: a temporary one)")
	verbose := flag.Bool("v", false, "log every fault and client call")
	flag.Parse()

	if *seed == 0 {
		*seed = time.Now().UnixNano() & (1<<62 - 1)
	}
	r := &run{
		seed: *seed, dsn: *dsn, queue: *queue, logs: *logs, concurrency: *concurrency,
		lease: *lease, pause: *pause, pgRestart: *pgRestart, verbose: *verbose,
		clients: map[string]*client{}, ids: map[int]string{}, signals: map[int]int{},
		paused: map[string]bool{}, injected: map[faultKind]int{},
	}
	fmt.Printf("chaos: seed %d\n", r.seed)
	err := r.main(workerCmds, *n, *roots, *duration, *faultList, *every, *settle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nchaos: FAIL (seed %d)\n%v\n", r.seed, err)
		fmt.Fprintf(os.Stderr, "worker logs: %s\nreplay: %s\n", r.logs, replayCommand(r.seed))
		os.Exit(1)
	}
	fmt.Printf("chaos: PASS (seed %d)\n", r.seed)
}

func replayCommand(seed int64) string {
	args := []string{"go", "run", "."}
	skip := false
	for _, a := range os.Args[1:] {
		if skip {
			skip = false
			continue
		}
		if a == "-seed" || a == "--seed" {
			skip = true
			continue
		}
		if strings.HasPrefix(a, "-seed=") || strings.HasPrefix(a, "--seed=") {
			continue
		}
		args = append(args, shellQuote(a))
	}
	return strings.Join(append(args, fmt.Sprintf("-seed %d", seed)), " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"$`\\;&|<>()*?[]{}!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (r *run) logf(format string, args ...any) {
	if r.verbose {
		fmt.Printf("%7.1fs  %s\n", time.Since(r.start).Seconds(), fmt.Sprintf(format, args...))
	}
}

func (r *run) main(cmds commands, n, roots int, window time.Duration, faultList string, every, settle time.Duration) error {
	if r.dsn == "" {
		return errors.New("-dsn (or RUNNERQ_TEST_DSN) is required")
	}
	if len(cmds) == 0 {
		return errors.New("at least one -worker is required")
	}
	var kinds []faultKind
	for f := range strings.SplitSeq(faultList, ",") {
		switch f := faultKind(strings.TrimSpace(f)); f {
		case "":
		case faultKill, faultPause, faultPG:
			kinds = append(kinds, f)
		default:
			return fmt.Errorf("unknown fault %q", f)
		}
	}
	if slices.Contains(kinds, faultPG) && r.pgRestart == "" {
		return errors.New("fault pg needs -pg-restart")
	}
	for sdk := range cmds {
		r.sdks = append(r.sdks, sdk)
	}
	slices.Sort(r.sdks)
	n = max(n, len(r.sdks))
	if r.queue == "" {
		r.queue = fmt.Sprintf("chaos_%d_%d", r.seed%1_000_000_000_000, time.Now().Unix()%100_000_000)
	}
	if r.logs == "" {
		dir, err := os.MkdirTemp("", "runnerq-chaos-")
		if err != nil {
			return err
		}
		r.logs = dir
	} else if err := os.MkdirAll(r.logs, 0o755); err != nil {
		return err
	}
	r.plan = planWorkload(r.seed, r.sdks, roots, window)
	r.faults = planFaults(r.seed, kinds, n, window, every)
	fmt.Printf("chaos: queue %s, %d workers (%s), %d roots, %d faults over %s, logs in %s\n",
		r.queue, n, strings.Join(r.sdks, "+"), roots, len(r.faults), window, r.logs)

	ctx := context.Background()
	db, err := pgxpool.New(ctx, r.dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	r.db = db
	if _, err := db.Exec(ctx, ledgerDDL); err != nil {
		return fmt.Errorf("create ledger: %w", err)
	}

	// Clients first: they create the schema.
	for _, sdk := range r.sdks {
		c := &client{sdk: sdk, command: cmds[sdk], run: r}
		if err := c.start(); err != nil {
			return err
		}
		r.clients[sdk] = c
		defer c.stop()
	}
	for i := range n {
		sdk := r.sdks[i%len(r.sdks)]
		w := &worker{index: i / len(r.sdks), sdk: sdk, command: cmds[sdk], run: r, readyWithin: 2 * time.Minute}
		r.workers = append(r.workers, w)
	}
	defer func() {
		for _, w := range r.workers {
			w.stop(20 * time.Second)
		}
	}()
	var wg sync.WaitGroup
	errs := make([]error, len(r.workers))
	for i, w := range r.workers {
		wg.Go(func() { errs[i] = w.start() })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}

	r.start = time.Now()
	sampling, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	var sampler sync.WaitGroup
	peaks := map[string]int{}
	var samples int
	sampler.Go(func() { samples = r.sampleLimited(sampling, peaks) })
	var clientErr error
	var calls, faults sync.WaitGroup
	calls.Go(func() { clientErr = r.runSchedule(window + settle) })
	faults.Go(func() { r.injectFaults() })
	faults.Wait()
	calls.Wait()
	if clientErr != nil {
		return clientErr
	}
	// Every worker back up, whatever the faults left.
	for _, w := range r.workers {
		if !w.alive() {
			if err := w.start(); err != nil {
				return err
			}
		}
	}
	fmt.Printf("chaos: faults done at %.0fs (%s); waiting for the workload to finish\n",
		time.Since(r.start).Seconds(), r.faultSummary())
	if err := r.awaitQuiescence(settle); err != nil {
		return err
	}
	stopSampling()
	sampler.Wait()
	for _, w := range r.workers {
		w.stop(20 * time.Second)
	}
	var unexpected []string
	for _, w := range r.workers {
		unexpected = append(unexpected, w.unexpected...)
	}
	rep, err := r.check(ctx)
	if err != nil {
		return err
	}
	fmt.Print(rep.summary())
	fmt.Printf("chaos: limited types' peak running count %v over %d samples (limit %d)\n", peaks, samples, limitedMax)
	for typ, peak := range peaks {
		if peak > limitedMax {
			rep.failures = append(rep.failures, fmt.Sprintf("%d activities of %s were running at once, over its limit of %d", peak, typ, limitedMax))
		}
	}
	if len(unexpected) > 0 {
		rep.failures = append(rep.failures, "workers exited without being killed: "+strings.Join(unexpected, "; "))
	}
	return rep.err()
}

// sampleLimited records the peak running count of each <prefix>.limited type
// until ctx ends, and returns how many samples it took. A failed sample (a
// Postgres restart) is skipped.
func (r *run) sampleLimited(ctx context.Context, peaks map[string]int) int {
	samples := 0
	for ctx.Err() == nil {
		rows, err := r.db.Query(ctx, `SELECT activity_type, count(*) FROM runnerq_activities
			WHERE queue_name = $1 AND status = 'processing' AND activity_type LIKE '%.limited'
			GROUP BY activity_type`, r.queue)
		if err == nil {
			for rows.Next() {
				var typ string
				var n int
				if rows.Scan(&typ, &n) == nil && n > peaks[typ] {
					peaks[typ] = n
				}
			}
			rows.Close()
			if rows.Err() == nil {
				samples++
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond):
		}
	}
	return samples
}

func (r *run) faultSummary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("%d kills, %d pauses, %d Postgres restarts", r.injected[faultKill], r.injected[faultPause], r.injected[faultPG])
}

// runSchedule makes the planned enqueue and signal calls at their times,
// retrying each until it succeeds; a retried enqueue returns the same
// activity by its idempotency key.
func (r *run) runSchedule(limit time.Duration) error {
	for _, s := range schedule(r.plan) {
		time.Sleep(time.Until(r.start.Add(s.at)))
		root := r.plan[s.root]
		var req map[string]any
		var sdk string
		switch s.op {
		case "enqueue":
			sdk = root.client
			req = map[string]any{"op": "enqueue", "type": root.typ, "key": root.key, "payload": root.input,
				"timeout_s": activityTimeoutS, "max_retry_delay_s": maxRetryDelayS}
		case "signal":
			sdk = root.signalBy
			r.mu.Lock()
			id := r.ids[s.root]
			r.mu.Unlock()
			req = map[string]any{"op": "signal", "id": id, "name": root.input.Signal, "payload": root.signal}
		}
		for attempt := 1; ; attempt++ {
			if s.op == "signal" {
				r.mu.Lock()
				r.signals[s.root]++
				r.mu.Unlock()
			}
			reply, err := r.clients[sdk].call(req)
			if err == nil {
				if s.op == "enqueue" {
					var out struct{ ID string }
					if err := json.Unmarshal(reply, &out); err != nil || out.ID == "" {
						return fmt.Errorf("enqueue %s: bad reply %s", root.key, reply)
					}
					r.mu.Lock()
					r.ids[s.root] = out.ID
					r.mu.Unlock()
				}
				r.logf("%s %s via %s", s.op, root.key, sdk)
				break
			}
			if time.Since(r.start) > limit {
				return fmt.Errorf("%s %s via %s: %w", s.op, root.key, sdk, err)
			}
			r.logf("%s %s via %s failed (attempt %d): %v", s.op, root.key, sdk, attempt, err)
			time.Sleep(time.Second)
		}
	}
	return nil
}

func (r *run) injectFaults() {
	var resumes sync.WaitGroup
	defer resumes.Wait()
	for _, f := range r.faults {
		time.Sleep(time.Until(r.start.Add(f.at)))
		switch f.kind {
		case faultKill:
			w := r.workers[f.target]
			if w.isPaused() || !w.alive() {
				continue
			}
			w.mu.Lock()
			label := w.label
			w.mu.Unlock()
			w.kill()
			r.count(f.kind)
			r.logf("SIGKILL %s; restart in %s", label, f.restart.Round(time.Millisecond))
			time.Sleep(f.restart)
			if err := w.start(); err != nil {
				fmt.Printf("chaos: restart %s: %v\n", w.name(), err)
			}
		case faultPause:
			w := r.workers[f.target]
			if w.isPaused() || !w.alive() || r.running() < 2 {
				continue // keep one worker running
			}
			w.mu.Lock()
			w.paused = true
			label := w.label
			w.mu.Unlock()
			r.mu.Lock()
			r.paused[label] = true
			r.mu.Unlock()
			_ = w.signal(syscall.SIGSTOP)
			r.count(f.kind)
			r.logf("SIGSTOP %s for %s", label, r.pause)
			resumes.Go(func() {
				time.Sleep(r.pause)
				_ = w.signal(syscall.SIGCONT)
				w.mu.Lock()
				w.paused = false
				w.mu.Unlock()
				r.logf("SIGCONT %s", label)
			})
		case faultPG:
			r.count(f.kind)
			r.logf("restarting Postgres")
			if err := r.restartPostgres(); err != nil {
				fmt.Printf("chaos: restart Postgres: %v\n", err)
			}
			r.logf("Postgres is back")
		}
	}
}

func (r *run) running() int {
	n := 0
	for _, w := range r.workers {
		if w.alive() && !w.isPaused() {
			n++
		}
	}
	return n
}

func (w *worker) isPaused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused
}

func (r *run) count(k faultKind) {
	r.mu.Lock()
	r.injected[k]++
	r.mu.Unlock()
}

func (r *run) restartPostgres() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "sh", "-c", r.pgRestart).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	for ctx.Err() == nil {
		if _, err := r.db.Exec(ctx, "SELECT 1"); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ctx.Err()
}

// awaitQuiescence waits until every planned root exists and every activity
// in the queue is terminal.
func (r *run) awaitQuiescence(settle time.Duration) error {
	deadline := time.Now().Add(settle)
	lastReport := time.Now()
	for {
		var open, total int
		err := r.db.QueryRow(context.Background(), `
			SELECT count(*) FILTER (WHERE status NOT IN ('completed', 'failed', 'dead_letter', 'cancelled')), count(*)
			FROM runnerq_activities WHERE queue_name = $1`, r.queue).Scan(&open, &total)
		r.mu.Lock()
		enqueued := len(r.ids)
		r.mu.Unlock()
		if err == nil && open == 0 && enqueued == len(r.plan) {
			fmt.Printf("chaos: quiescent at %.0fs, %d activities\n", time.Since(r.start).Seconds(), total)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not quiescent after %s: %d of %d activities unfinished\n%s", settle, open, total, r.stuck())
		}
		if time.Since(lastReport) > 15*time.Second {
			fmt.Printf("chaos: %d of %d activities unfinished\n", open, total)
			lastReport = time.Now()
		}
		time.Sleep(time.Second)
	}
}

func (r *run) stuck() string {
	rows, err := r.db.Query(context.Background(), `
		SELECT id, activity_type, status, retry_count, coalesce(current_worker_id, ''),
		       coalesce(to_char(scheduled_at, 'HH24:MI:SS'), ''), coalesce(last_error, '')
		FROM runnerq_activities
		WHERE queue_name = $1 AND status NOT IN ('completed', 'failed', 'dead_letter', 'cancelled')
		ORDER BY created_at LIMIT 20`, r.queue)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, typ, status, worker, sched, lastErr string
		var retries int
		if err := rows.Scan(&id, &typ, &status, &retries, &worker, &sched, &lastErr); err != nil {
			return err.Error()
		}
		fmt.Fprintf(&b, "  %s %s %s retries=%d worker=%q scheduled=%s error=%q\n", id, typ, status, retries, worker, sched, lastErr)
	}
	return b.String()
}
