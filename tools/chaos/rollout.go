package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The two versions of a rolling deploy (-rolling).
const (
	versionA = "A"
	versionB = "B"
)

type rolloutLog struct {
	mu       sync.Mutex
	bStarted time.Duration // when the B workers were up, after the run started
	aRetired time.Duration // when the A workers were stopped; 0 if never
	started  bool
}

func (l *rolloutLog) startedB() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.started
}

// rollOut deploys B after `after`: it starts the B workers, moves the clients
// to B, then waits until no unfinished activity is pinned to A and stops the
// A workers. switched is closed once B is up and the clients have moved.
func (r *run) rollOut(n int, after, limit time.Duration, switched chan<- struct{}) error {
	time.Sleep(time.Until(r.start.Add(after)))
	err := startAll(r.workers[n:])
	r.rollout.mu.Lock()
	r.rollout.started = true
	r.rollout.bStarted = time.Since(r.start)
	r.rollout.mu.Unlock()
	if err != nil {
		close(switched)
		return err
	}
	for _, c := range r.clients {
		c.setVersion(versionB)
	}
	fmt.Printf("chaos: version B up at %.0fs; clients enqueue as B\n", time.Since(r.start).Seconds())
	close(switched)

	for {
		var open int
		err := r.db.QueryRow(context.Background(), `
			SELECT count(*) FROM runnerq_activities
			WHERE queue_name = $1 AND code_version = $2
			  AND status NOT IN ('completed', 'failed', 'dead_letter', 'cancelled')`, r.queue, versionA).Scan(&open)
		if err == nil && open == 0 {
			break
		}
		if time.Since(r.start) > limit {
			return fmt.Errorf("version A did not drain within %s: %d activities unfinished", limit, open)
		}
		time.Sleep(time.Second)
	}
	var wg sync.WaitGroup
	for _, w := range r.workers[:n] {
		wg.Go(func() { w.retire(20 * time.Second) })
	}
	wg.Wait()
	r.rollout.mu.Lock()
	r.rollout.aRetired = time.Since(r.start)
	r.rollout.mu.Unlock()
	fmt.Printf("chaos: version A drained; A workers stopped at %.0fs\n", time.Since(r.start).Seconds())
	return nil
}
