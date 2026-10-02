// Command conformance runs the conformance scenarios against one or more SDK
// drivers (see conformance/README.md) and checks their expectations against
// the database.
//
//	go run . -dsn "$RUNNERQ_TEST_DSN" -driver go="<command>" [-driver ts="<command>"]
//
// Run it from tools/conformance.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/runnerq/runnerq-spec/schemacheck"
)

type driverFlags []string

func (d *driverFlags) String() string     { return strings.Join(*d, ", ") }
func (d *driverFlags) Set(v string) error { *d = append(*d, v); return nil }

func main() {
	dsn := flag.String("dsn", os.Getenv("RUNNERQ_TEST_DSN"), "Postgres connection string")
	root := flag.String("spec", filepath.Join("..", ".."), "runnerq-spec root")
	dir := flag.String("scenarios", "", "scenario directory (default <spec>/conformance/scenarios)")
	run := flag.String("run", "", "only scenarios whose path matches this regexp")
	verbose := flag.Bool("v", false, "list every scenario")
	var drivers driverFlags
	flag.Var(&drivers, "driver", "name=command, e.g. go=\"go run ./internal/conformancedriver\"; repeatable")
	flag.Parse()
	if *dsn == "" || len(drivers) == 0 {
		fail(fmt.Errorf("-dsn (or RUNNERQ_TEST_DSN) and at least one -driver are required"))
	}
	filter, err := regexp.Compile(*run)
	if err != nil {
		fail(err)
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fail(err)
	}
	defer db.Close()

	var started []*driver
	defer func() {
		for _, d := range started {
			d.stop()
		}
	}()
	byName := map[string]*driver{}
	for _, spec := range drivers {
		name, cmd, ok := strings.Cut(spec, "=")
		if !ok || name == "" || cmd == "" {
			fail(fmt.Errorf("-driver %q: want name=command", spec))
		}
		d, err := startDriver(name, cmd)
		if err != nil {
			fail(err)
		}
		started = append(started, d)
		byName[name] = d
	}

	if *dir == "" {
		*dir = filepath.Join(*root, "conformance", "scenarios")
	}
	events, err := schemacheck.Load(filepath.Join(*root, "schema", "postgres", "events.schema.json"))
	if err != nil {
		fail(err)
	}
	paths, err := scenarioPaths(*dir)
	if err != nil {
		fail(err)
	}
	var failed, skipped, passed int
	for _, path := range paths {
		rel, _ := filepath.Rel(*dir, path)
		if !filter.MatchString(rel) {
			continue
		}
		sc, err := loadScenario(path)
		if err != nil {
			fail(err)
		}
		need := sc.drivers()
		if missing := slices.DeleteFunc(slices.Clone(need), func(n string) bool { return byName[n] != nil }); len(missing) > 0 {
			skipped++
			if *verbose {
				fmt.Printf("SKIP %s (needs drivers %s)\n", rel, strings.Join(missing, ", "))
			}
			continue
		}
		start := time.Now()
		r := &runner{db: db, events: events, drivers: byName, first: started[0], dsn: *dsn}
		if err := r.run(ctx, sc); err != nil {
			failed++
			fmt.Printf("FAIL %s\n     %v\n", rel, err)
			for _, d := range started {
				if log := d.stderrTail(); log != "" {
					fmt.Printf("     %s stderr:\n%s\n", d.name, indent(log, "       "))
				}
			}
			continue
		}
		passed++
		if *verbose {
			fmt.Printf("ok   %s (%s)\n", rel, time.Since(start).Round(time.Millisecond))
		}
	}
	fmt.Printf("%d passed, %d failed, %d skipped\n", passed, failed, skipped)
	if failed > 0 {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "conformance:", err)
	os.Exit(2)
}

func scenarioPaths(dir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".json") {
			paths = append(paths, path)
		}
		return err
	})
	slices.Sort(paths)
	return paths, err
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
