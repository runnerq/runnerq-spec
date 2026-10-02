// Command catalog applies the Postgres schema (migrations, then concurrent
// indexes) to a scratch schema and writes or checks schema/postgres/catalog.json.
//
//	go run . -dsn postgres://... -write   # regenerate catalog.json
//	go run . -dsn postgres://...          # fail if catalog.json differs
//
// Run it from tools/catalog. -check compares normalized index definitions and
// defaults (vectors/index_definition.json, vectors/column_default.json), so it
// passes on any supported Postgres version.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Catalog struct {
	Description string  `json:"description"`
	Tables      []Table `json:"tables"`
	Indexes     []Index `json:"indexes"`
	Retired     Retired `json:"retired"`
}

type Table struct {
	Name       string   `json:"name"`
	Columns    []Column `json:"columns"`
	PrimaryKey []string `json:"primary_key"`
}

type Column struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"` // information_schema udt_name: int4, text, _text, ...
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default"`
}

type Index struct {
	Name       string `json:"name"`
	Table      string `json:"table"`
	Definition string `json:"definition"`
}

type Retired struct {
	Columns []string `json:"columns"` // table.column
	Indexes []string `json:"indexes"`
}

const description = "The catalog the migrations and concurrent indexes produce, read from Postgres by tools/catalog. Implementations compare a live database against it: tables, columns (information_schema udt_name, nullability, default normalized as in vectors/column_default.json), primary keys, and indexes other than primary keys (definitions normalized as in vectors/index_definition.json, and valid). Order is not significant. Retired objects must not exist."

func main() {
	dsn := flag.String("dsn", os.Getenv("RUNNERQ_TEST_DSN"), "Postgres connection string")
	write := flag.Bool("write", false, "rewrite catalog.json instead of checking it")
	rootFlag := flag.String("root", filepath.Join("..", "..", "schema", "postgres"), "schema directory")
	flag.Parse()
	if *dsn == "" {
		fail(fmt.Errorf("-dsn or RUNNERQ_TEST_DSN is required"))
	}
	root := *rootFlag
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	got, err := build(ctx, *dsn, root)
	if err != nil {
		fail(err)
	}
	path := filepath.Join(root, "catalog.json")
	if *write {
		if err := os.WriteFile(path, encode(got), 0o644); err != nil {
			fail(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	var want Catalog
	if err := json.Unmarshal(raw, &want); err != nil {
		fail(fmt.Errorf("catalog.json: %w", err))
	}
	if diffs := compare(want, *got); len(diffs) > 0 {
		fail(fmt.Errorf("catalog.json is stale (go run . -write):\n  %s", strings.Join(diffs, "\n  ")))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "catalog:", err)
	os.Exit(1)
}

func build(ctx context.Context, dsn, root string) (*Catalog, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.WithoutCancel(ctx))

	schema := fmt.Sprintf("runnerq_spec_catalog_%d", os.Getpid())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "DROP SCHEMA "+ident+" CASCADE")
	if _, err := conn.Exec(ctx, "SET search_path TO "+ident); err != nil {
		return nil, err
	}

	migrations, err := filepath.Glob(filepath.Join(root, "migrations", "*.sql"))
	if err != nil {
		return nil, err
	}
	slices.Sort(migrations)
	// Twice: every migration must be safe to re-run.
	for range 2 {
		for _, m := range migrations {
			sql, err := os.ReadFile(m)
			if err != nil {
				return nil, err
			}
			if _, err := conn.Exec(ctx, string(sql)); err != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Base(m), err)
			}
		}
	}
	var concurrent struct {
		Indexes []struct {
			Name     string `json:"name"`
			Replaces string `json:"replaces"`
			SQL      string `json:"sql"`
		} `json:"indexes"`
	}
	if err := readJSON(filepath.Join(root, "concurrent_indexes.json"), &concurrent); err != nil {
		return nil, err
	}
	var c Catalog
	for _, idx := range concurrent.Indexes {
		if _, err := conn.Exec(ctx, idx.SQL); err != nil {
			return nil, fmt.Errorf("%s: %w", idx.Name, err)
		}
		if idx.Replaces != "" {
			c.Retired.Indexes = append(c.Retired.Indexes, idx.Replaces)
		}
	}
	var retired struct {
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
	}
	if err := readJSON(filepath.Join(root, "retired.json"), &retired); err != nil {
		return nil, err
	}
	for _, col := range retired.Columns {
		c.Retired.Columns = append(c.Retired.Columns, col.Name)
	}
	c.Description = description

	rows, err := conn.Query(ctx, `
		SELECT table_name, column_name, udt_name, is_nullable = 'YES', column_default
		FROM information_schema.columns WHERE table_schema = current_schema()
		ORDER BY table_name, column_name`)
	if err != nil {
		return nil, err
	}
	tables := map[string]*Table{}
	for rows.Next() {
		var table string
		var col Column
		if err := rows.Scan(&table, &col.Name, &col.Type, &col.Nullable, &col.Default); err != nil {
			return nil, err
		}
		if tables[table] == nil {
			tables[table] = &Table{Name: table}
		}
		tables[table].Columns = append(tables[table].Columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = conn.Query(ctx, `
		SELECT c.relname, array_agg(a.attname::text ORDER BY k.ordinality)
		FROM pg_constraint p
		JOIN pg_class c ON c.oid = p.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL unnest(p.conkey) WITH ORDINALITY k(num, ordinality)
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.num
		WHERE p.contype = 'p' AND n.nspname = current_schema()
		GROUP BY c.relname`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table string
		var key []string
		if err := rows.Scan(&table, &key); err != nil {
			return nil, err
		}
		if tables[table] != nil {
			tables[table].PrimaryKey = key
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range tables {
		c.Tables = append(c.Tables, *t)
	}
	slices.SortFunc(c.Tables, func(a, b Table) int { return strings.Compare(a.Name, b.Name) })

	rows, err = conn.Query(ctx, `
		SELECT c.relname, t.relname, pg_get_indexdef(c.oid), i.indisvalid
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND NOT i.indisprimary
		ORDER BY c.relname`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var idx Index
		var valid bool
		if err := rows.Scan(&idx.Name, &idx.Table, &idx.Definition, &valid); err != nil {
			return nil, err
		}
		if !valid {
			return nil, fmt.Errorf("index %s is invalid", idx.Name)
		}
		idx.Definition = strings.Replace(idx.Definition, "ON "+schema+".", "ON ", 1)
		c.Indexes = append(c.Indexes, idx)
	}
	return &c, rows.Err()
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func encode(c *Catalog) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(c)
	return b.Bytes()
}

var (
	indexSchemaRe = regexp.MustCompile(`\bon\s+(?:"(?:[^"]|"")+"|[a-z_]\w*)\.`)
	indexCastRe   = regexp.MustCompile(`::text(?:\[\])?`)
	indexAnyRe    = regexp.MustCompile(`=\s*any\s*\(\s*array\s*\[`)
	indexAscRe    = regexp.MustCompile(`\s+asc\b`)
	indexStripRe  = regexp.MustCompile(`[\s"()\[\];]`)
	defaultCastRe = regexp.MustCompile(`::(?:text|integer|bigint|smallint)`)
	defaultStrip  = regexp.MustCompile(`[\s()]`)
)

func normIndex(def string) string {
	s := strings.ToLower(def)
	s = indexSchemaRe.ReplaceAllString(s, "on ")
	s = indexCastRe.ReplaceAllString(s, "")
	s = indexAnyRe.ReplaceAllString(s, "in(")
	s = strings.ReplaceAll(s, "using btree", "")
	s = indexAscRe.ReplaceAllString(s, "")
	return indexStripRe.ReplaceAllString(s, "")
}

func normDefault(def *string) string {
	if def == nil {
		return "<null>"
	}
	s := defaultStrip.ReplaceAllString(defaultCastRe.ReplaceAllString(strings.ToLower(*def), ""), "")
	if strings.HasPrefix(s, "nextval") {
		return "nextval"
	}
	return s
}

// compare reports what differs between two catalogs, normalized.
func compare(want, got Catalog) []string {
	flat := func(c Catalog) map[string]string {
		m := map[string]string{}
		for _, t := range c.Tables {
			m["table "+t.Name+" primary key"] = strings.Join(t.PrimaryKey, ",")
			for _, col := range t.Columns {
				m["column "+t.Name+"."+col.Name] = fmt.Sprintf("%s nullable=%v default=%s", col.Type, col.Nullable, normDefault(col.Default))
			}
		}
		for _, idx := range c.Indexes {
			m["index "+idx.Name] = idx.Table + " " + normIndex(idx.Definition)
		}
		for _, r := range c.Retired.Columns {
			m["retired column "+r] = ""
		}
		for _, r := range c.Retired.Indexes {
			m["retired index "+r] = ""
		}
		return m
	}
	w, g := flat(want), flat(got)
	var diffs []string
	for k, v := range g {
		if wv, ok := w[k]; !ok {
			diffs = append(diffs, "missing from catalog.json: "+k)
		} else if wv != v {
			diffs = append(diffs, fmt.Sprintf("%s: catalog.json has %q, database has %q", k, wv, v))
		}
	}
	for k := range w {
		if _, ok := g[k]; !ok {
			diffs = append(diffs, "not in database: "+k)
		}
	}
	slices.Sort(diffs)
	return diffs
}
