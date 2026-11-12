package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"
)

// Everything random about a run comes from the seed: the workload, who
// enqueues and signals it, and the fault schedule. Process timing is not
// seeded, so a replay injects the same faults at the same moments, not into
// the same instructions.

// Root and child activity options. A sleep longer than the timeout parks; a
// shorter one waits in-process.
const (
	activityTimeoutS = 10
	maxRetryDelayS   = 2
)

type kind string

const (
	kindSteps  kind = "steps"
	kindSleep  kind = "sleep"
	kindSignal kind = "signal"
	kindParent kind = "parent"
)

// flowInput is the payload every <prefix>.flow activity takes (README.md).
type flowInput struct {
	Label    string      `json:"label"`
	Steps    []string    `json:"steps"`
	StepMS   int         `json:"step_ms"`
	SleepMS  int64       `json:"sleep_ms,omitempty"`
	Signal   string      `json:"signal,omitempty"`
	Children []childSpec `json:"children,omitempty"`
	After    []string    `json:"after,omitempty"`
}

type childSpec struct {
	Type           string    `json:"type"`
	Payload        flowInput `json:"payload"`
	TimeoutS       int       `json:"timeout_s"`
	MaxRetryDelayS int       `json:"max_retry_delay_s"`
}

// flowResult is what a flow activity returns.
type flowResult struct {
	Label    string            `json:"label"`
	Steps    map[string]string `json:"steps"`
	Signal   json.RawMessage   `json:"signal,omitempty"`
	Children []json.RawMessage `json:"children,omitempty"`
}

type root struct {
	index    int
	kind     kind
	typ      string // <prefix>.flow
	key      string
	input    flowInput
	enqueue  time.Duration // after the run starts
	client   string        // the SDK whose client enqueues it
	signalAt time.Duration // waiters: after the run starts
	signalBy string
	signal   json.RawMessage
}

type faultKind string

const (
	faultKill  faultKind = "kill"
	faultPause faultKind = "pause"
	faultPG    faultKind = "pg"
)

type fault struct {
	at      time.Duration
	kind    faultKind
	target  int           // worker index, for kill and pause
	restart time.Duration // kill: how long the worker stays down
}

func newRNG(seed int64, stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(seed), stream))
}

func planWorkload(seed int64, sdks []string, roots int, window time.Duration) []root {
	rng := newRNG(seed, 1)
	pick := func() string { return sdks[rng.IntN(len(sdks))] }
	steps := func(prefix string, n int) []string {
		s := make([]string, n)
		for i := range s {
			s[i] = fmt.Sprintf("%s%d", prefix, i+1)
		}
		return s
	}
	plan := make([]root, roots)
	for i := range plan {
		r := root{index: i, typ: pick() + ".flow", client: pick()}
		r.key = fmt.Sprintf("root-%d", i)
		r.enqueue = time.Duration(rng.Int64N(int64(window/2) + 1))
		r.input = flowInput{
			Label:  r.key,
			Steps:  steps("s", 1+rng.IntN(3)),
			StepMS: 40 + rng.IntN(260),
			After:  []string{"after"},
		}
		switch n := rng.IntN(100); {
		case n < 35:
			r.kind = kindSteps
		case n < 55:
			r.kind = kindSleep
			if rng.IntN(4) == 0 {
				r.input.SleepMS = 200 + rng.Int64N(800) // in-process
			} else {
				r.input.SleepMS = (activityTimeoutS+1)*1000 + rng.Int64N(6000)
			}
		case n < 75:
			r.kind = kindSignal
			r.input.Signal = "go-ahead"
			r.signalAt = r.enqueue + time.Duration(rng.Int64N(int64(4*time.Second)))
			r.signalBy = pick()
			r.signal, _ = json.Marshal(map[string]string{"token": fmt.Sprintf("%016x", rng.Uint64())})
		default:
			r.kind = kindParent
			for j := range 2 + rng.IntN(2) {
				child := flowInput{
					Label:  fmt.Sprintf("child-%d", j),
					Steps:  steps("c", 1+rng.IntN(2)),
					StepMS: 40 + rng.IntN(160),
				}
				if rng.IntN(4) == 0 {
					child.SleepMS = (activityTimeoutS+1)*1000 + rng.Int64N(3000)
					child.After = []string{"after"}
				}
				r.input.Children = append(r.input.Children, childSpec{
					Type: pick() + ".flow", Payload: child,
					TimeoutS: activityTimeoutS, MaxRetryDelayS: maxRetryDelayS,
				})
			}
		}
		plan[i] = r
	}
	return plan
}

func planFaults(seed int64, kinds []faultKind, workers int, window, every time.Duration) []fault {
	if len(kinds) == 0 || window <= 0 {
		return nil
	}
	rng := newRNG(seed, 2)
	var faults []fault
	lastPG := -time.Hour
	for at := 2 * time.Second; ; {
		at += max(time.Duration(rng.ExpFloat64()*float64(every)), 500*time.Millisecond)
		if at >= window {
			break
		}
		f := fault{at: at, kind: kinds[rng.IntN(len(kinds))], target: rng.IntN(workers),
			restart: time.Duration(rng.Int64N(int64(1500 * time.Millisecond)))}
		if f.kind == faultPG {
			// Postgres needs time to come back and the fleet to reconnect.
			if at-lastPG < 15*time.Second {
				continue
			}
			lastPG = at
		}
		faults = append(faults, f)
	}
	return faults
}

// scheduled is one client call the run makes at a planned time.
type scheduled struct {
	at   time.Duration
	root int
	op   string // enqueue or signal
}

func schedule(plan []root) []scheduled {
	var s []scheduled
	for i, r := range plan {
		s = append(s, scheduled{at: r.enqueue, root: i, op: "enqueue"})
		if r.kind == kindSignal {
			s = append(s, scheduled{at: r.signalAt, root: i, op: "signal"})
		}
	}
	slices.SortStableFunc(s, func(a, b scheduled) int {
		if a.at != b.at {
			return int(a.at - b.at)
		}
		if a.op != b.op { // a root's enqueue before its signal
			if a.op == "enqueue" {
				return -1
			}
			return 1
		}
		return a.root - b.root
	})
	return s
}
