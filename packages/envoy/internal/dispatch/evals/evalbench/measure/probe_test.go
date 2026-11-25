package main

import (
	"strings"
	"testing"
)

// The statements of a list read as the statement log holds them: evals.ReadStoredFiguresIn's
// range-figures read with a compare range and its day-figures read, the read of how far the
// figures are built beside them, and evals.DayWindowIn's union of the day rows, which the list
// read before the stored figures.
const (
	rangeFiguresRead = `select days, end_day, eval_id, fold_key, figures, oldest_updated_at from eval_range_figures
		where project_key = any($1::text[]) and repo = $2 and eval_id = any($3::text[]) and days = $6 and end_day = $7::date
			and deploy_repo = case when eval_id = any($4::text[]) then $5::text else '' end
		union all
		select days, end_day, eval_id, fold_key, figures, oldest_updated_at from eval_range_figures
		where project_key = any($1::text[]) and repo = $2 and eval_id = any($3::text[]) and days = $8 and end_day = $9::date
			and deploy_repo = case when eval_id = any($4::text[]) then $5::text else '' end`
	dayFiguresRead = `
				select eval_id, day, fold_key, figures from eval_day_figures
				where project_key = any($1::text[]) and repo = $2 and day >= $3::date and day < $4::date and eval_id = any($5::text[])
					and deploy_repo = case when eval_id = any($6::text[]) then $7::text else '' end
			`
	figuresBuiltRead = `
		select b.project_key, b.through, s.project_key is not null, s.evals from eval_figures_built b
		left join eval_figure_cuts_stale s on s.project_key = b.project_key and s.repo = $2
		where b.project_key = any($1)
	`
	dayRowsUnion = `select true, eval_id, series, day, n, sum, num, den, max
			from eval_days
			where project_key = any($1::text[]) and repo = any($2) and listed and eval_id = any($3)
			  and day >= $4::date and day < $5::date
		union all
		select false, eval_id, series, day, n, sum, num, den, max
			from eval_days
			where project_key = any($1::text[]) and repo = any($2) and listed and eval_id = any($3)
			  and day >= $6::date and day < $7::date`
)

// listRead is the paint's list-84d-compare read with the statements it captured and the literals
// the statement log holds for each, by query id.
func listRead(statements map[int64]string, params map[int64][]string) measuredCase {
	return measuredCase{paintCase: paintCase{name: "list-84d-compare"}, statements: statements, params: params}
}

// TestTheProbeMeasuresTheListsStoredFiguresReads: at this head the list answers an offered range
// from the stored figures (evals.ReadStoredFiguresIn), so the probe finds the range-figures and
// day-figures reads among the read's statements, and runs each with the literals the statement
// log holds written in, and again without its figures column. A read that ran neither of them, as
// one that reads the day rows' union does, fails the probe with a message naming both, so a bench
// whose list stopped reading the stored figures exits 1, and a probe that looked for the day rows'
// union alone would fail every run of the head.
func TestTheProbeMeasuresTheListsStoredFiguresReads(t *testing.T) {
	window, compare := "'2026-11-22 00:00:00+00'", "'2026-08-30 00:00:00+00'"
	read := listRead(map[int64]string{11: rangeFiguresRead, 12: figuresBuiltRead, 13: dayFiguresRead}, map[int64][]string{
		11: {"'{INFRA}'", "''", "'{INFRA/a,INFRA/b}'", "'{INFRA/b}'", "'owner/deploy'", "'84'", window, "'84'", compare},
		12: {"'{INFRA}'", "''"},
		13: {"'{INFRA}'", "''", compare, window, "'{INFRA/a}'", "'{INFRA/b}'", "'owner/deploy'"},
	})
	rangeBranch := func(columns, end string) string {
		return "select " + columns + " from eval_range_figures where project_key = any('{INFRA}'::text[]) and repo = '' " +
			"and eval_id = any('{INFRA/a,INFRA/b}'::text[]) and days = '84' and end_day = " + end + "::date " +
			"and deploy_repo = case when eval_id = any('{INFRA/b}'::text[]) then 'owner/deploy'::text else '' end"
	}
	rangeRead := func(columns string) string {
		return rangeBranch(columns, window) + " union all " + rangeBranch(columns, compare)
	}
	dayRead := func(columns string) string {
		return "select " + columns + " from eval_day_figures where project_key = any('{INFRA}'::text[]) and repo = '' " +
			"and day >= " + compare + "::date and day < " + window + "::date and eval_id = any('{INFRA/a}'::text[]) " +
			"and deploy_repo = case when eval_id = any('{INFRA/b}'::text[]) then 'owner/deploy'::text else '' end"
	}
	want := []struct {
		name            string
		queryID         int64
		shipped, narrow string
	}{
		{"range-figures", 11, rangeRead("days, end_day, eval_id, fold_key, figures, oldest_updated_at"),
			rangeRead("days, end_day, eval_id, fold_key, oldest_updated_at")},
		{"day-figures", 13, dayRead("eval_id, day, fold_key, figures"), dayRead("eval_id, day, fold_key")},
	}

	found, skipped, err := storedFiguresReads(read)
	if err != nil {
		t.Fatalf("a list read that ran both stored-figures reads failed the probe: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("a list read that ran both stored-figures reads skipped %q; want nothing skipped", skipped)
	}
	if len(found) != len(want) {
		t.Fatalf("the probe found %d stored-figures statements, want %d: %+v", len(found), len(want), found)
	}
	for index, statement := range found {
		expected := want[index]
		if statement.name != expected.name || statement.queryID != expected.queryID {
			t.Errorf("statement %d is %s, query id %d; want %s, query id %d", index, statement.name, statement.queryID, expected.name, expected.queryID)
		}
		variants := map[string]string{}
		for _, variant := range statement.variants {
			variants[variant[0]] = oneLine(variant[1])
		}
		if len(variants) != 2 || variants["shipped"] != expected.shipped || variants["narrow"] != expected.narrow {
			t.Errorf("the %s read's variants are\n%q\nwant shipped\n%q\nand narrow\n%q", expected.name, variants, expected.shipped, expected.narrow)
		}
	}

	dayRows := listRead(map[int64]string{21: dayRowsUnion, 22: figuresBuiltRead}, map[int64][]string{
		21: {"'{INFRA}'", `'{""}'`, "'{INFRA/a,INFRA/b}'", compare, window, "'2026-06-07 00:00:00+00'", compare},
		22: {"'{INFRA}'", "''"},
	})
	found, _, err = storedFiguresReads(dayRows)
	if err == nil {
		t.Fatalf("a list read that ran the day rows' union and neither stored-figures read passed the probe, finding %+v", found)
	}
	for _, mark := range []string{"from eval_range_figures", "from eval_day_figures"} {
		if !strings.Contains(err.Error(), mark) {
			t.Errorf("the probe's failure does not name the statement holding %q: %v", mark, err)
		}
	}
	checks := &headChecks{r: &report{prefix: "RESULT"}}
	checks.probe([]measuredCase{dayRows})
	if checks.r.failed != 1 {
		t.Errorf("a paint whose list read ran neither stored-figures read was judged %d failures; want 1, so the bench exits 1", checks.r.failed)
	}
}
