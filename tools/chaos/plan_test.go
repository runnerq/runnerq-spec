package main

import (
	"reflect"
	"testing"
	"time"
)

func TestPlanIsSeeded(t *testing.T) {
	sdks := []string{"go", "ts"}
	a := planWorkload(9, sdks, 50, time.Minute)
	if !reflect.DeepEqual(a, planWorkload(9, sdks, 50, time.Minute)) {
		t.Fatal("same seed, different workload")
	}
	if reflect.DeepEqual(a, planWorkload(10, sdks, 50, time.Minute)) {
		t.Fatal("different seeds, same workload")
	}
	kinds := map[kind]int{}
	for _, r := range a {
		kinds[r.kind]++
		if r.enqueue > 30*time.Second || (r.kind == kindSignal && r.signalAt < r.enqueue) {
			t.Fatalf("%s: enqueue %s, signal %s", r.key, r.enqueue, r.signalAt)
		}
	}
	if len(kinds) != 4 {
		t.Fatalf("kinds %v", kinds)
	}
	f := planFaults(9, []faultKind{faultKill, faultPG}, 3, time.Minute, 2*time.Second)
	if !reflect.DeepEqual(f, planFaults(9, []faultKind{faultKill, faultPG}, 3, time.Minute, 2*time.Second)) {
		t.Fatal("same seed, different faults")
	}
	var lastPG time.Duration = -time.Hour
	for _, x := range f {
		if x.at >= time.Minute || x.target >= 3 {
			t.Fatalf("fault %+v", x)
		}
		if x.kind == faultPG {
			if x.at-lastPG < 15*time.Second {
				t.Fatalf("Postgres restarts %s apart", x.at-lastPG)
			}
			lastPG = x.at
		}
	}
}

func TestScheduleEnqueuesBeforeSignalling(t *testing.T) {
	seen := map[int]bool{}
	for _, s := range schedule(planWorkload(3, []string{"go"}, 40, time.Minute)) {
		if s.op == "enqueue" {
			seen[s.root] = true
		} else if !seen[s.root] {
			t.Fatalf("root %d signalled before it was enqueued", s.root)
		}
	}
}

func TestDecode(t *testing.T) {
	for _, c := range []struct{ data, ser, want string }{
		{`"x"`, "json-v1", `"x"`},
		{`"x"`, "", `"x"`},
		{`{"json":"x"}`, "superjson-v1", `"x"`},
		{`{"json":{"a":1},"meta":{}}`, "superjson-v1", `{"a":1}`},
	} {
		got, err := decode([]byte(c.data), c.ser)
		if err != nil || string(got) != c.want {
			t.Errorf("decode(%s, %s) = %s, %v; want %s", c.data, c.ser, got, err, c.want)
		}
	}
	if _, err := decode([]byte(`1`), "msgpack"); err == nil {
		t.Error("unknown serialization decoded")
	}
}
