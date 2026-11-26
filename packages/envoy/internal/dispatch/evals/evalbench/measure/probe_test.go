package main

import (
	"strings"
	"testing"
)

// The statements of a list read as the statement log holds them: evals.ReadStoredFiguresIn's
// range-figures read and day-figures read, each with a compare range, the read of how far the
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
	dayFiguresRead = `select true, eval_id, day, fold_key, figures from eval_day_figures
		where project_key = any($1::text[]) and repo = $2 and day >= $6::date and day < $7::date and eval_id = any($3::text[])
			and deploy_repo = case when eval_id = any($4::text[]) then $5::text else '' end
		union all
		select false, eval_id, day, fold_key, figures from eval_day_figures
		where project_key = any($1::text[]) and repo = $2 and day >= $8::date and day < $9::date and eval_id = any($3::text[])
			and deploy_repo = case when eval_id = any($4::text[]) then $5::text else '' end`
	figuresBuiltRead = `
		select b.project_key, b.through, s.project_key is not null, s.evals, s.ready_through, s.next_day from eval_figures_built b
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
	window, compare, earlier := "'2026-11-22 00:00:00+00'", "'2026-08-30 00:00:00+00'", "'2026-06-07 00:00:00+00'"
	read := listRead(map[int64]string{11: rangeFiguresRead, 12: figuresBuiltRead, 13: dayFiguresRead}, map[int64][]string{
		11: {"'{INFRA}'", "''", "'{INFRA/a,INFRA/b}'", "'{INFRA/b}'", "'owner/deploy'", "'84'", window, "'84'", compare},
		12: {"'{INFRA}'", "''"},
		13: {"'{INFRA}'", "''", "'{INFRA/a}'", "'{INFRA/b}'", "'owner/deploy'", compare, window, earlier, compare},
	})
	rangeBranch := func(columns, end string) string {
		return "select " + columns + " from eval_range_figures where project_key = any('{INFRA}'::text[]) and repo = '' " +
			"and eval_id = any('{INFRA/a,INFRA/b}'::text[]) and days = '84' and end_day = " + end + "::date " +
			"and deploy_repo = case when eval_id = any('{INFRA/b}'::text[]) then 'owner/deploy'::text else '' end"
	}
	rangeRead := func(columns string) string {
		return rangeBranch(columns, window) + " union all " + rangeBranch(columns, compare)
	}
	dayBranch := func(columns, from, to string) string {
		return "select " + columns + " from eval_day_figures where project_key = any('{INFRA}'::text[]) and repo = '' " +
			"and day >= " + from + "::date and day < " + to + "::date and eval_id = any('{INFRA/a}'::text[]) " +
			"and deploy_repo = case when eval_id = any('{INFRA/b}'::text[]) then 'owner/deploy'::text else '' end"
	}
	dayRead := func(columns string) string {
		return dayBranch("true, "+columns, compare, window) + " union all " + dayBranch("false, "+columns, earlier, compare)
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

	found, err := storedFiguresReads(read)
	if err != nil {
		t.Fatalf("a list read that ran both stored-figures reads failed the probe: %v", err)
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
	found, err = storedFiguresReads(dayRows)
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
	if len(checks.r.fails) != 1 {
		t.Errorf("a paint whose list read ran neither stored-figures read was judged %d failures; want 1, so the bench exits 1", len(checks.r.fails))
	}
}

// TestTheProbeFailsAListReadThatRanOneStoredFiguresReadAlone: every logged eval of the fixture has
// a headline series, so a list read of an offered range runs both stored-figures reads, and one
// that ran either alone no longer reads that half of the stored figures. The probe fails it naming
// the read it did not run, where it once printed the missing read as skipped and passed. It fails a
// statement whose select list holds no figures column too, whose narrow variant would measure the
// statement as shipped again, where it once printed that variant as skipped.
func TestTheProbeFailsAListReadThatRanOneStoredFiguresReadAlone(t *testing.T) {
	window, compare := "'2026-11-22 00:00:00+00'", "'2026-08-30 00:00:00+00'"
	rangeParams := []string{"'{INFRA}'", "''", "'{INFRA/a,INFRA/b}'", "'{INFRA/b}'", "'owner/deploy'", "'84'", window, "'84'", compare}
	dayParams := []string{"'{INFRA}'", "''", compare, window, "'{INFRA/a}'", "'{INFRA/b}'", "'owner/deploy'"}
	for _, read := range []struct {
		name       string
		statements map[int64]string
		params     map[int64][]string
		missing    string
	}{
		{"the range-figures read alone", map[int64]string{11: rangeFiguresRead, 12: figuresBuiltRead},
			map[int64][]string{11: rangeParams, 12: {"'{INFRA}'", "''"}}, "from eval_day_figures"},
		{"the day-figures read alone", map[int64]string{12: figuresBuiltRead, 13: dayFiguresRead},
			map[int64][]string{12: {"'{INFRA}'", "''"}, 13: dayParams}, "from eval_range_figures"},
		{"a range-figures read with no figures column", map[int64]string{11: strings.ReplaceAll(rangeFiguresRead, "fold_key, figures,", "fold_key,"), 13: dayFiguresRead},
			map[int64][]string{11: rangeParams, 13: dayParams}, "figures column"},
	} {
		found, err := storedFiguresReads(listRead(read.statements, read.params))
		if err == nil {
			t.Errorf("a list read that ran %s passed the probe, finding %+v", read.name, found)
			continue
		}
		if !strings.Contains(err.Error(), read.missing) {
			t.Errorf("the probe's failure of a list read that ran %s does not name %q: %v", read.name, read.missing, err)
		}
	}
}

// TestEveryReadOfAnOfferedRangeIsHeldToTheStoredFigures: the reads of one repository's cut answer
// from the stored figures as the reads of the project's own rows do (evals.FiguresOffered), so every
// run fails a read of an offered range, with a repository or with none, that ran neither
// stored-figures read, naming the read, where the probe once looked at list-84d-compare alone. A
// Delivery read, which reads no stored figures, is not held to them.
func TestEveryReadOfAnOfferedRangeIsHeldToTheStoredFigures(t *testing.T) {
	window, compare := "'2026-11-22 00:00:00+00'", "'2026-08-30 00:00:00+00'"
	stored := func(name string) measuredCase {
		return measuredCase{paintCase: paintCase{name: name, storedFigures: true},
			statements: map[int64]string{11: rangeFiguresRead, 13: dayFiguresRead}, params: map[int64][]string{
				11: {"'{INFRA}'", "''", "'{INFRA/a,INFRA/b}'", "'{INFRA/b}'", "'owner/deploy'", "'84'", window, "'84'", compare},
				13: {"'{INFRA}'", "''", compare, window, "'{INFRA/a}'", "'{INFRA/b}'", "'owner/deploy'"},
			}}
	}
	dayRows := measuredCase{paintCase: paintCase{name: "list-84d-compare-repo", storedFigures: true},
		statements: map[int64]string{21: dayRowsUnion}, params: map[int64][]string{21: {"'{INFRA}'", `'{"acme/infra"}'`, "'{INFRA/a}'", compare, window, compare, window}}}
	delivery := measuredCase{paintCase: paintCase{name: "timeline-84d", delivery: true}, statements: map[int64]string{31: "select 1"}}
	judged := &report{prefix: "RESULT"}
	checkStoredFiguresReads(judged, []measuredCase{stored("list-84d-compare"), dayRows, stored("list-28d"), delivery})
	if len(judged.fails) != 1 || !strings.Contains(judged.fails[0], "list-84d-compare-repo") {
		t.Errorf("a paint whose list-84d-compare-repo read folded the day rows was judged %q; want one failure naming list-84d-compare-repo", judged.fails)
	}
}
