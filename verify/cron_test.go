package verify

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The reference cron implementation, written from schedules.md. Instants and
// wall-clock times are both whole seconds since the Unix epoch: a wall time
// is the instant it would be if the zone were UTC.

type cronField struct {
	bits uint64
	star bool // the field's text begins with "*"
}

func (f cronField) has(v int) bool { return f.bits&(1<<uint(v)) != 0 }

type cronExpr struct {
	sec, min, hour, dom, month, dow cronField
}

var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func parseCron(text string) (*cronExpr, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "@") {
		expanded, ok := cronMacros[strings.ToLower(text)]
		if !ok {
			return nil, fmt.Errorf("unknown macro %q", text)
		}
		text = expanded
	}
	fields := strings.Fields(text)
	switch len(fields) {
	case 5:
		fields = append([]string{"0"}, fields...)
	case 6:
	default:
		return nil, fmt.Errorf("want 5 or 6 fields, got %d", len(fields))
	}
	var e cronExpr
	var err error
	parsers := []struct {
		dst      *cronField
		min, max int
		names    []string
		nameBase int
	}{
		{&e.sec, 0, 59, nil, 0},
		{&e.min, 0, 59, nil, 0},
		{&e.hour, 0, 23, nil, 0},
		{&e.dom, 1, 31, nil, 0},
		{&e.month, 1, 12, monthNames, 1},
		{&e.dow, 0, 7, dayNames, 0},
	}
	for i, p := range parsers {
		if *p.dst, err = parseCronField(fields[i], p.min, p.max, p.names, p.nameBase); err != nil {
			return nil, fmt.Errorf("field %d %q: %w", i+1, fields[i], err)
		}
	}
	if e.dow.has(7) {
		e.dow.bits = e.dow.bits&^(1<<7) | 1
	}
	if !e.matchesSomeDate() {
		return nil, errors.New("matches no date")
	}
	return &e, nil
}

func parseCronField(text string, lo, hi int, names []string, nameBase int) (cronField, error) {
	f := cronField{star: strings.HasPrefix(text, "*")}
	value := func(s string) (int, error) {
		for i, n := range names {
			if strings.EqualFold(s, n) {
				return i + nameBase, nil
			}
		}
		if s == "" || strings.TrimLeft(s, "0123456789") != "" || len(s) > 2 {
			return 0, fmt.Errorf("bad value %q", s)
		}
		v, _ := strconv.Atoi(s)
		if v < lo || v > hi {
			return 0, fmt.Errorf("%d out of range %d-%d", v, lo, hi)
		}
		return v, nil
	}
	for _, item := range strings.Split(text, ",") {
		rng, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			if stepText == "" || strings.TrimLeft(stepText, "0123456789") != "" || len(stepText) > 2 {
				return f, fmt.Errorf("bad step %q", stepText)
			}
			step, _ = strconv.Atoi(stepText)
			if step == 0 {
				return f, errors.New("step 0")
			}
		}
		var start, end int
		switch {
		case rng == "*":
			start, end = lo, hi
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if start, err = value(a); err != nil {
				return f, err
			}
			if end, err = value(b); err != nil {
				return f, err
			}
			if start > end {
				return f, fmt.Errorf("range %q runs backwards", rng)
			}
		default:
			v, err := value(rng)
			if err != nil {
				return f, err
			}
			start, end = v, v
			if hasStep {
				end = hi
			}
		}
		for v := start; v <= end; v += step {
			f.bits |= 1 << uint(v)
		}
	}
	return f, nil
}

var daysInMonth = []int{31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

// matchesSomeDate: with the OR rule any weekday matches, so only the AND rule
// can rule out every date (day 30 in February, say).
func (e *cronExpr) matchesSomeDate() bool {
	if !e.dom.star && !e.dow.star {
		return true
	}
	for m := 1; m <= 12; m++ {
		for d := 1; d <= daysInMonth[m-1]; d++ {
			if e.month.has(m) && e.dom.has(d) {
				return true
			}
		}
	}
	return false
}

func (e *cronExpr) dayMatches(t time.Time) bool {
	dom, dow := e.dom.has(t.Day()), e.dow.has(int(t.Weekday()))
	if e.dom.star || e.dow.star {
		return dom && dow
	}
	return dom || dow
}

// firstMatch is the earliest wall time at or after w that matches, searching
// cronSearchYears ahead.
func (e *cronExpr) firstMatch(w int64) (int64, bool) {
	t := time.Unix(w, 0).UTC()
	limit := t.Year() + cronSearchYears
	for t.Year() <= limit {
		switch {
		case !e.month.has(int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !e.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case !e.hour.has(t.Hour()):
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
		case !e.min.has(t.Minute()):
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, time.UTC)
		case !e.sec.has(t.Second()):
			t = t.Add(time.Second)
		default:
			return t.Unix(), true
		}
	}
	return 0, false
}

const (
	cronSearchYears = 50
	day             = 86400
	week            = 7 * day
)

type zone struct{ loc *time.Location }

func (z zone) offset(t int64) int64 {
	_, off := time.Unix(t, 0).In(z.loc).Zone()
	return int64(off)
}

// transition is the first instant in (lo, hi] whose offset differs from lo's.
func (z zone) transition(lo, hi int64) int64 {
	o := z.offset(lo)
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if z.offset(mid) == o {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// nextAtOrAfter is the schedule's first fire instant at or after t.
func (e *cronExpr) nextAtOrAfter(z zone, t int64) (int64, bool) {
	fixed := !e.hour.star
	if fixed {
		// Inside the repeated hour after a fall-back, wall times have fired once
		// already: move to its end.
		if before := z.offset(t - day); before > z.offset(t) {
			T := z.transition(t-day, t)
			if end := T + before - z.offset(T); t < end {
				t = end
			}
		}
	}
	for range 100000 {
		o := z.offset(t)
		w, ok := e.firstMatch(t + o)
		if !ok {
			return 0, false
		}
		c := w - o
		lim := min(c, t+week)
		if z.offset(lim) != o {
			T := z.transition(t, lim)
			after := z.offset(T)
			if after > o {
				// Spring forward: wall times [T+o, T+after) never happen. A
				// fixed-hour schedule fires once at T for any of them.
				if fixed {
					if m, ok := e.firstMatch(T + o); ok && m < T+after {
						return T, true
					}
				}
				t = T
			} else {
				// Fall back: wall times [T+after, T+o) happen again. A
				// fixed-hour schedule skips the second pass.
				t = T
				if fixed {
					t = T + o - after
				}
			}
			continue
		}
		if c == lim {
			return c, true
		}
		t = lim
	}
	return 0, false
}

func (e *cronExpr) next(z zone, from int64) (int64, bool) { return e.nextAtOrAfter(z, from+1) }

// lastTicks are the last n fire times in [a, b], oldest first.
func (e *cronExpr) lastTicks(z zone, a, b int64, n int) []int64 {
	window := int64(60)
	for {
		start := max(a, b-window)
		var ticks []int64
		for t, ok := e.nextAtOrAfter(z, start); ok && t <= b; t, ok = e.next(z, t) {
			ticks = append(ticks, t)
			if len(ticks) > n {
				ticks = ticks[1:]
			}
		}
		if len(ticks) >= n || start == a {
			return ticks
		}
		switch {
		case window < 3600:
			window = 3600
		case window < day:
			window = day
		default:
			window *= 8
		}
	}
}

func loadZone(name string) (zone, error) {
	loc, err := time.LoadLocation(name)
	if err != nil || name == "" || name == "Local" {
		return zone{}, fmt.Errorf("unknown time zone %q", name)
	}
	return zone{loc}, nil
}

func rfc3339(t int64) string { return time.Unix(t, 0).UTC().Format(time.RFC3339) }

func parseInstant(s string) (int64, int64, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, 0, err
	}
	return t.Unix(), int64(t.Nanosecond()), nil
}

func TestCronNextFire(t *testing.T) {
	check(t, "vectors/cron_next_fire.json", func(in struct {
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
		From     string `json:"from"`
		Count    int    `json:"count"`
	}) (any, error) {
		e, err := parseCron(in.Cron)
		if err != nil {
			return "invalid", nil
		}
		z, err := loadZone(in.Timezone)
		if err != nil {
			return "invalid", nil
		}
		from, _, err := parseInstant(in.From)
		if err != nil {
			return nil, err
		}
		out := []string{}
		for range in.Count {
			next, ok := e.next(z, from)
			if !ok {
				break
			}
			out = append(out, rfc3339(next))
			from = next
		}
		return out, nil
	})
}

func TestScheduleFire(t *testing.T) {
	grace := constantInt(t, "schedule_catch_up_grace_seconds")
	check(t, "vectors/schedule_fire.json", func(in struct {
		Cron       string `json:"cron"`
		Timezone   string `json:"timezone"`
		NextFireAt string `json:"next_fire_at"`
		Now        string `json:"now"`
		CatchUp    string `json:"catch_up"`
		Limit      int    `json:"limit"`
	}) (any, error) {
		e, err := parseCron(in.Cron)
		if err != nil {
			return nil, err
		}
		z, err := loadZone(in.Timezone)
		if err != nil {
			return nil, err
		}
		due, dueNanos, err := parseInstant(in.NextFireAt)
		if err != nil {
			return nil, err
		}
		if dueNanos > 0 {
			due++
		}
		now, _, err := parseInstant(in.Now)
		if err != nil {
			return nil, err
		}
		type out struct {
			Fire       []string `json:"fire"`
			NextFireAt string   `json:"next_fire_at"`
		}
		if due > now {
			return out{Fire: []string{}, NextFireAt: in.NextFireAt}, nil
		}
		n := 1
		if in.CatchUp == "all" {
			n = in.Limit
		}
		ticks := e.lastTicks(z, due, now, n)
		if in.CatchUp == "skip" && len(ticks) > 0 && now-ticks[len(ticks)-1] >= grace {
			ticks = nil
		}
		next, _ := e.next(z, now)
		o := out{Fire: []string{}, NextFireAt: rfc3339(next)}
		for _, t := range ticks {
			o.Fire = append(o.Fire, rfc3339(t))
		}
		return o, nil
	})
}
