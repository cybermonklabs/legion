package main

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The probe, after the first paint: where the 84-day compare read's day-row statement
// (evals.Store.DayWindow, as the server ran it) spends its time on the loaded database, so the next
// change to it is chosen from a measurement. Each variant of the statement is run probeRuns times
// under `explain (analyze, buffers, serialize binary)`, which executes it and serialises its rows
// as the server sends them without sending them, and its median execution and serialisation times
// are printed beside its rows, the bytes it serialised and the buffers it read. Set against the
// paint's own database time for the statement, which includes sending the rows to the server and
// the server reading them, the variants tell heap pages (the plan's buffers and its time with the
// narrow column list), serialisation (the bytes, and the serialisation time) and the client (what
// the paint's time holds beyond the explain's) apart:
//
//   - shipped: the statement as the server ran it, its parameters written in as the literals the
//     statement log holds, which Postgres plans as it planned the server's first executions;
//   - window and compare: each branch of its union alone;
//   - narrow: the same rows with the columns series, day and n alone;
//   - headline: the rows of the headline series alone, those a per-day array is answered for, kept
//     by a filter on a literal list of `<eval id> <series>`, so the plan keeps the shipped one's
//     scans (a join to eval_series_current would let Postgres plan another).
//
// It then prints eval_days' size on disk and its average row width, split by whether a row holds a
// sample, over the whole table and over the paint's project's listed rows.

// probeRuns is how many times each variant runs; the median is printed.
const probeRuns = 5

// The parts of an explain the probe reads.
var (
	topRows      = regexp.MustCompile(`rows=([0-9.]+) loops=`)
	topBuffers   = regexp.MustCompile(`Buffers: shared hit=([0-9]+)(?: read=([0-9]+))?`)
	serialised   = regexp.MustCompile(`Serialization: (?:time=([0-9.]+) ms\s+)?output=([0-9]+)kB`)
	selectList   = regexp.MustCompile(`(?s)select\s.*?\bfrom eval_days\b`)
	parameterRef = regexp.MustCompile(`\$([0-9]+)`)
)

// probe measures the day-row statement of the paint's 84-day compare read (the package comment).
func (c *headChecks) probe(measured []measuredCase) {
	ctx := context.Background()
	index := slices.IndexFunc(measured, func(one measuredCase) bool { return one.name == "list-84d-compare" })
	if index < 0 {
		c.r.fail("probe: the paint holds no list-84d-compare read")
		return
	}
	read := measured[index]
	var text string
	var params []string
	for queryID, statement := range read.statements {
		if strings.Contains(statement, "from eval_days") && strings.Contains(statement, "union all") {
			text, params = statement, read.params[queryID]
		}
	}
	if text == "" {
		c.r.fail("probe: list-84d-compare ran no statement reading eval_days with a union, so the day-row read was not found")
		return
	}
	shipped := withLiterals(text, params)
	variants := [][2]string{{"shipped", shipped}}
	if branches := strings.Split(shipped, "union all"); len(branches) == 2 {
		variants = append(variants, [2]string{"window", branches[0]}, [2]string{"compare", branches[1]})
	} else {
		c.r.result("probe variant=window,compare skipped: the statement holds %d branches, not 2", len(branches))
	}
	if narrow := selectList.ReplaceAllString(shipped, "select series, day, n from eval_days"); narrow != shipped {
		variants = append(variants, [2]string{"narrow", narrow})
	} else {
		c.r.result("probe variant=narrow skipped: no select list over eval_days found")
	}
	var headlineSeries string
	if err := c.pool.QueryRow(ctx, `
		select quote_literal(coalesce(array_agg(eval_id || ' ' || series), '{}')::text) from eval_series_current
		where role = 'headline' and listed and starts_with(eval_id, $1 || '/')
	`, c.o.project).Scan(&headlineSeries); err != nil {
		c.r.fail("probe: read the headline series of %s: %v", c.o.project, err)
	} else if headline := strings.ReplaceAll(shipped, "where project_key",
		"where eval_id || ' ' || series = any("+headlineSeries+"::text[]) and project_key"); headline != shipped {
		variants = append(variants, [2]string{"headline", headline})
	} else {
		c.r.result("probe variant=headline skipped: no where clause on project_key found")
	}
	for _, variant := range variants {
		c.probeVariant(ctx, variant[0], variant[1])
	}
	c.probeTable(ctx)
}

// withLiterals is text with each parameter $n replaced by params[n-1], the literal the statement
// log holds for it.
func withLiterals(text string, params []string) string {
	return parameterRef.ReplaceAllStringFunc(text, func(ref string) string {
		n, err := strconv.Atoi(ref[1:])
		if err != nil || n < 1 || n > len(params) {
			return ref
		}
		return params[n-1]
	})
}

// probeVariant runs one variant probeRuns times and prints its medians.
func (c *headChecks) probeVariant(ctx context.Context, name, text string) {
	var executions, serialisations []float64
	var rows, kilobytes, hit, read string
	for range probeRuns {
		lines, err := c.explainLines(ctx, "explain (analyze, buffers, serialize binary) "+text)
		if err != nil {
			c.r.fail("probe variant=%s: %v: %s", name, err, oneLine(text))
			return
		}
		plan := strings.Join(lines, "\n")
		executions = append(executions, executionTime(plan))
		if match := serialised.FindStringSubmatch(plan); match != nil {
			ms, _ := strconv.ParseFloat(match[1], 64)
			serialisations = append(serialisations, ms)
			kilobytes = match[2]
		}
		if match := topRows.FindStringSubmatch(plan); match != nil {
			rows = match[1]
		}
		if match := topBuffers.FindStringSubmatch(plan); match != nil {
			hit, read = match[1], cmp.Or(match[2], "0")
		}
	}
	c.r.result("probe variant=%s runs=%d execution_ms_median=%.2f serialization_ms_median=%.2f rows=%s serialized_kb=%s shared_hit=%s shared_read=%s execution_ms=%s",
		name, probeRuns, median(executions), median(serialisations), rows, kilobytes, hit, read, formatMs(executions))
}

// explainLines is the lines of one explain, run on the explainer's connection.
func (c *headChecks) explainLines(ctx context.Context, query string) ([]string, error) {
	rows, err := c.explain.conn.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

// probeTable prints eval_days' size and its average row width by whether a row holds a sample:
// over the whole table, and over the paint's project's listed rows (those a window read reads).
func (c *headChecks) probeTable(ctx context.Context) {
	var heap, total int64
	if err := c.pool.QueryRow(ctx, `select pg_relation_size('eval_days'), pg_total_relation_size('eval_days')`).Scan(&heap, &total); err != nil {
		c.r.fail("probe table: %v", err)
		return
	}
	c.r.result("probe table=eval_days relation_bytes=%d total_relation_bytes=%d", heap, total)
	for _, scope := range []struct{ name, filter string }{
		{"all", "true"},
		{c.o.project + "-listed", "project_key = $1 and listed"},
	} {
		var args []any
		if strings.Contains(scope.filter, "$1") {
			args = append(args, c.o.project)
		}
		rows, err := c.pool.Query(ctx, `
			select coalesce(cardinality(t.samples), 0) > 0, count(*), avg(pg_column_size(t.*))::float8, avg(coalesce(cardinality(t.samples), 0))::float8
			from eval_days t where `+scope.filter+` group by 1 order by 1`, args...)
		if err != nil {
			c.r.fail("probe table rows=%s: %v", scope.name, err)
			continue
		}
		for rows.Next() {
			var sampled bool
			var count int64
			var width, samples float64
			if err := rows.Scan(&sampled, &count, &width, &samples); err != nil {
				c.r.fail("probe table rows=%s: %v", scope.name, err)
				break
			}
			c.r.result("probe table=eval_days rows=%s with_samples=%t count=%d avg_row_bytes=%.0f avg_samples=%.1f", scope.name, sampled, count, width, samples)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			c.r.fail("probe table rows=%s: %v", scope.name, err)
		}
	}
}

// median is the middle of values, the mean of the two middle ones for an even count; -1 for none.
func median(values []float64) float64 {
	if len(values) == 0 {
		return -1
	}
	sorted := slices.Sorted(slices.Values(values))
	middle := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[middle-1] + sorted[middle]) / 2
	}
	return sorted[middle]
}

// formatMs is values as a bracketed list of milliseconds.
func formatMs(values []float64) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprintf("%.2f", value)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
