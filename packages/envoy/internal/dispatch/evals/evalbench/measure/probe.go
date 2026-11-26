package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The probe, after the first paint: where the 84-day compare read's stored-figures statements
// (evals.ReadStoredFiguresIn, as the server ran them) spend their time on the loaded database,
// so the next change to them is chosen from a measurement. The list answers an offered range of the
// project's own rows, or of one repository's, with two of them: the range-figures read, the `union
// all` of the window's and the compare range's rows of eval_range_figures of the read's cut, one
// row an eval each, and the day-figures read, the window's rows of eval_day_figures of the evals
// with a headline series. The probe finds each among the read's captured statements by the text
// the statement log holds of it, `from eval_range_figures` and `from eval_day_figures`. A read that
// ran either of them alone fails the probe, naming the one it did not run: the server skips the
// day-figures read only where no eval has a headline series, and every logged eval of the fixture
// has one (evalbench's `value`), so a read without it no longer reads the stored day figures, and
// one without the range-figures read no longer reads the stored range figures. A statement whose
// select list holds no figures column fails it too, since its narrow variant would measure the
// statement as shipped again. The reads that name a repository (list-84d-compare-repo,
// list-28d-repo) run the same two statements over the cut of that repository
// (evals.FiguresOffered), which every run judges of every read of an offered range
// (checkStoredFiguresReads); the probe measures list-84d-compare, which names none.
//
// Each variant of a statement is run probeRuns times under `explain (analyze, buffers, serialize
// binary)`, which executes it and serialises its rows as the server sends them without sending
// them, and its median execution and serialisation times are printed beside its rows, the bytes it
// serialised and the buffers it read, on a line that carries the statement's query id, as the
// paint's statement lines do. Set against the paint's own database time for the statement, which
// includes sending the rows to the server and the server reading them, the variants tell the index
// and heap pages (the plan's buffers and its time without the figures column), the figures (the
// bytes and the time they add) and the client (what the paint's time holds beyond the explain's)
// apart:
//
//   - shipped: the statement as the server ran it, its parameters written in as the literals the
//     statement log holds, which Postgres plans as it planned the server's first executions;
//   - narrow: the same rows without the figures column, which holds each row's encoded figures, so
//     the time and the bytes it saves are what reading and serialising the figures cost.
//
// It measures no variant of a branch or of the headline filter: the range-figures read's
// `union all` is of two lookups of the same shape, so a branch alone would measure half of one
// lookup, and the day-figures read already reads the evals with a headline series alone.
//
// It then prints eval_days' size on disk and its average row width, split by whether a row holds a
// sample, over the whole table and over the paint's project's listed rows; and the size on disk of
// eval_range_figures and of eval_day_figures, with the count, the bytes and the average width of
// the paint's project's rows of each, and the average width of their figures column.

// probeRuns is how many times each variant runs; the median is printed.
const probeRuns = 5

// The parts of an explain the probe reads.
var (
	topRows      = regexp.MustCompile(`rows=([0-9.]+) loops=`)
	topBuffers   = regexp.MustCompile(`Buffers: shared hit=([0-9]+)(?: read=([0-9]+))?`)
	serialised   = regexp.MustCompile(`Serialization: (?:time=([0-9.]+) ms\s+)?output=([0-9]+)kB`)
	parameterRef = regexp.MustCompile(`\$([0-9]+)`)
)

// figuresColumn matches a select list over a stored-figures table that holds the figures column:
// the list before the column, the column with the comma before it, and the rest of the list through
// the table. The narrow variant keeps the first and the last.
var figuresColumn = regexp.MustCompile(`(\bselect\s[^;]*?),\s*figures\b([^;]*?\bfrom eval_(?:range|day)_figures\b)`)

// storedFiguresStatements are the two statements of evals.ReadStoredFiguresIn the probe
// measures, each by the name its lines carry and the text that marks it in the statement log.
var storedFiguresStatements = []struct{ name, mark string }{
	{"range-figures", "from eval_range_figures"},
	{"day-figures", "from eval_day_figures"},
}

// probedStatement is one statement of the list read the probe measures: its name, the query id the
// paint printed it under, and its variants, each a name and the statement's text with its literals
// written in.
type probedStatement struct {
	name     string
	queryID  int64
	variants [][2]string
}

// probe measures the stored-figures statements of the paint's 84-day compare read (the package
// comment).
func (c *headChecks) probe(measured []measuredCase) {
	ctx := context.Background()
	index := slices.IndexFunc(measured, func(one measuredCase) bool { return one.name == "list-84d-compare" })
	if index < 0 {
		c.r.fail("probe: the paint holds no list-84d-compare read")
		return
	}
	found, err := storedFiguresReads(measured[index])
	if err != nil {
		c.r.fail("probe: %v", err)
		return
	}
	for _, statement := range found {
		for _, variant := range statement.variants {
			c.probeVariant(ctx, statement, variant[0], variant[1])
		}
	}
	c.probeTable(ctx)
}

// storedFiguresReads finds, among the statements read captured, each stored-figures statement
// (storedFiguresStatements) and builds its variants (the package comment): in that order, and by
// query id where several statements hold one mark. A read that ran one of the two statements
// alone, or a statement whose narrow variant cannot be built, is the error, which names what is
// missing, as is a read that ran neither, whose list no longer reads the stored figures.
func storedFiguresReads(read measuredCase) ([]probedStatement, error) {
	var found []probedStatement
	queryIDs := slices.Sorted(maps.Keys(read.statements))
	var missing []string
	for _, kind := range storedFiguresStatements {
		held := false
		for _, queryID := range queryIDs {
			text := read.statements[queryID]
			if !strings.Contains(text, kind.mark) {
				continue
			}
			held = true
			shipped := withLiterals(text, read.params[queryID])
			narrow := figuresColumn.ReplaceAllString(shipped, "$1$2")
			if narrow == shipped {
				missing = append(missing, fmt.Sprintf("the %s read (query id %d) has no select list holding the figures column, so its narrow variant measures nothing",
					kind.name, queryID))
				continue
			}
			found = append(found, probedStatement{name: kind.name, queryID: queryID, variants: [][2]string{{"shipped", shipped}, {"narrow", narrow}}})
		}
		if !held {
			missing = append(missing, fmt.Sprintf("no statement holding %q (the %s read)", kind.mark, kind.name))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s did not run both stored-figures reads of evals.ReadStoredFiguresIn as the probe measures them: %s",
			read.name, strings.Join(missing, "; "))
	}
	return found, nil
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

// probeVariant runs one variant of statement probeRuns times and prints its medians.
func (c *headChecks) probeVariant(ctx context.Context, statement probedStatement, name, text string) {
	var executions, serialisations []float64
	var rows, kilobytes, hit, read string
	for range probeRuns {
		lines, err := c.explainLines(ctx, "explain (analyze, buffers, serialize binary) "+text)
		if err != nil {
			c.r.fail("probe statement=%s queryid=%d variant=%s: %v: %s", statement.name, statement.queryID, name, err, oneLine(text))
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
	c.r.result("probe statement=%s queryid=%d variant=%s runs=%d execution_ms_median=%.2f serialization_ms_median=%.2f rows=%s serialized_kb=%s shared_hit=%s shared_read=%s execution_ms=%s",
		statement.name, statement.queryID, name, probeRuns, median(executions), median(serialisations), rows, kilobytes, hit, read, formatMs(executions))
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

// probeTable prints the size on disk of eval_days, eval_range_figures and eval_day_figures; then
// eval_days' average row width by whether a row holds a sample, over the whole table and over the
// paint's project's listed rows (those a window read reads); then, of each stored-figures table,
// the count, the bytes and the average width of the paint's project's rows (those its list reads
// from), and the average width of their figures column.
func (c *headChecks) probeTable(ctx context.Context) {
	for _, table := range []string{"eval_days", "eval_range_figures", "eval_day_figures"} {
		var heap, total int64
		if err := c.pool.QueryRow(ctx, `select pg_relation_size($1::text::regclass), pg_total_relation_size($1::text::regclass)`, table).Scan(&heap, &total); err != nil {
			c.r.fail("probe table=%s: %v", table, err)
			continue
		}
		c.r.result("probe table=%s relation_bytes=%d total_relation_bytes=%d", table, heap, total)
	}
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
	for _, table := range []string{"eval_range_figures", "eval_day_figures"} {
		var count, bytes int64
		var width, figures float64
		if err := c.pool.QueryRow(ctx, `
			select count(*), coalesce(sum(pg_column_size(t.*)), 0)::int8, coalesce(avg(pg_column_size(t.*)), 0)::float8,
				coalesce(avg(pg_column_size(t.figures)), 0)::float8
			from `+table+` t where project_key = $1`, c.o.project).Scan(&count, &bytes, &width, &figures); err != nil {
			c.r.fail("probe table=%s rows=%s: %v", table, c.o.project, err)
			continue
		}
		c.r.result("probe table=%s rows=%s count=%d row_bytes=%d avg_row_bytes=%.0f avg_figures_bytes=%.0f", table, c.o.project, count, bytes, width, figures)
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
