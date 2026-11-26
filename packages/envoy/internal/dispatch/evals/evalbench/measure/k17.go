package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Check k17 (dispatch://INFRA-3206, R15): the first paint of the Evals tab, with 365 days of the
// code survival results stored, reads the stored day rows and figures and nothing of the results,
// makes no request to GitHub (checkGitHubStub), and, at the base before the code evals, holds
// neither of them.

// holdsRegistry is whether the database holds the eval registry's day rows: the base of a run
// whose base is a commit of this branch before the code evals holds them, and the base before the
// registry existed does not.
func holdsRegistry(ctx context.Context, pool *store.Pool) (bool, error) {
	var held bool
	if err := pool.QueryRow(ctx, `select to_regclass('eval_days') is not null`).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether the database holds the eval registry: %w", err)
	}
	return held, nil
}

// registryBaseCases are the first paint of a base that holds the eval registry: the four reads of
// the evals list this head's paint makes (headCases), with no budget, since the base is no
// subject of the first paint's budgets but of the answer: neither code eval is in it.
func registryBaseCases(project string, today time.Time) []paintCase {
	var cases []paintCase
	for _, one := range headCases(project, today, 1) {
		if one.view == nil && !one.delivery {
			cases = append(cases, paintCase{name: "base-" + one.name, path: one.path})
		}
	}
	return cases
}

// codeEvalSlugs are the slugs of the two code evals.
var codeEvalSlugs = []string{evals.SlugCodeSurvival, evals.SlugPostMergeRework}

// listedSeries is one series of a listed eval: its name, and its statistics over the window.
type listedSeries struct {
	Series string              `json:"series"`
	Window map[string]*float64 `json:"window"`
}

// listedEval is one eval of the list read's answer, as the paint reads it: its id,
// `<project_key>/<slug>`, and its series.
type listedEval struct {
	ID     string         `json:"id"`
	Series []listedSeries `json:"series"`
}

// stated is whether the eval states a value of any series over the window.
func (e listedEval) stated() bool {
	for _, series := range e.Series {
		for _, value := range series.Window {
			if value != nil {
				return true
			}
		}
	}
	return false
}

// evalList is the list read's answer as the paint reads it: the evals, and the project they are of,
// from the id of the first.
type evalList struct {
	items   []listedEval
	project string
}

// readEvalList reads the evals of the list read's answer body.
func readEvalList(body []byte) (evalList, error) {
	var answer struct {
		Items []listedEval `json:"items"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return evalList{}, fmt.Errorf("read the evals list: %w", err)
	}
	list := evalList{items: answer.Items}
	if len(answer.Items) > 0 {
		list.project, _, _ = strings.Cut(answer.Items[0].ID, "/")
	}
	return list, nil
}

// eval is the list's eval of slug, and whether the list holds it.
func (l evalList) eval(slug string) (listedEval, bool) {
	index := slices.IndexFunc(l.items, func(item listedEval) bool { return item.ID == evals.EvalID(l.project, slug) })
	if index < 0 {
		return listedEval{}, false
	}
	return l.items[index], true
}

// checkBaseEvals is k17's control at a base that holds the eval registry: each read of the first
// paint's evals list answers, and none of the answers holds either code eval, which this branch adds
// after the base. A read that listed no eval shows nothing of the base, so it fails too.
func checkBaseEvals(r *report, measured []measuredCase) {
	if len(measured) == 0 {
		r.fail("k17-base: no read of the base's first paint was measured")
	}
	for _, one := range measured {
		list, err := readEvalList(one.body)
		if err != nil {
			r.fail("k17-base case=%s: %v", one.name, err)
			continue
		}
		var held []string
		for _, slug := range codeEvalSlugs {
			if _, found := list.eval(slug); found {
				held = append(held, slug)
			}
		}
		r.result("k17-base case=%s evals=%d code_evals=%d", one.name, len(list.items), len(held))
		if len(list.items) == 0 {
			r.fail("k17-base case=%s: the base's answer lists no eval, so the control shows nothing of the base's evals", one.name)
		}
		if len(held) > 0 {
			r.fail("k17-base case=%s: the base's answer holds %s, which come after the base", one.name, strings.Join(held, " and "))
		}
	}
}

// checkCodeEvals is k17's first clause at this head: each read of the evals list holds both code
// evals, and a read whose window holds days they measured (paintCase.codeStated) states a value of
// each. A paint over day rows that were never built is cheap for want of anything to read, so a list
// that lacks the code evals, or states no value of them where days are measured, fails.
func checkCodeEvals(r *report, measured []measuredCase) {
	examined := 0
	for _, one := range measured {
		if !one.storedFigures || one.view != nil {
			continue
		}
		examined++
		list, err := readEvalList(one.body)
		if err != nil {
			r.fail("k17 case=%s: %v", one.name, err)
			continue
		}
		stated := 0
		for _, slug := range codeEvalSlugs {
			item, found := list.eval(slug)
			switch {
			case !found:
				r.fail("k17 case=%s: the evals list holds no %s", one.name, slug)
			case item.stated():
				stated++
			case one.codeStated:
				r.fail("k17 case=%s: the evals list states no value of %s over a window that holds days it measured", one.name, slug)
			}
		}
		r.result("k17 case=%s evals=%d code_evals_stating_a_value=%d", one.name, len(list.items), stated)
	}
	if examined == 0 {
		r.fail("k17: no read of the evals list was examined for the code evals")
	}
}

// codeResultTable matches a statement that names a table of the code survival results.
var codeResultTable = regexp.MustCompile(`\bcode_(?:changes|pairs|days)\b`)

// codeResultTables are the tables of the results whose scans a plan names.
var codeResultTables = []string{"code_changes", "code_pairs", "code_days"}

// checkCodeResultReads is k17's second clause: no statement of the paint, of any read, names
// code_changes, code_pairs or code_days, in its text or in the relations its plan scans. The day
// rows and the stored figures hold what the page shows, so a paint that reads the results is one
// that folds them again on the read (R15). A paint with no statement examined proves nothing, so it
// fails.
func checkCodeResultReads(r *report, measured []measuredCase) {
	examined, reading := 0, 0
	for _, one := range measured {
		for _, statement := range one.own {
			examined++
			named := map[string]bool{}
			for _, table := range codeResultTable.FindAllString(strings.ToLower(statement.query+"\n"+one.statements[statement.queryID]), -1) {
				named[table] = true
			}
			scans := relations(one.plans[statement.queryID])
			for _, table := range codeResultTables {
				if len(scans[table]) > 0 {
					named[table] = true
				}
			}
			if len(named) > 0 {
				reading++
				r.fail("k17 case=%s queryid=%d reads the code survival results (%s), where the paint reads the stored day rows and figures alone: %s",
					one.name, statement.queryID, strings.Join(slices.Sorted(maps.Keys(named)), ", "), oneLine(statement.query))
			}
		}
	}
	r.result("k17 paint_statements_examined=%d statements_naming_the_code_results=%d", examined, reading)
	if examined == 0 {
		r.fail("k17: no statement of the paint examined for a read of the code survival results")
	}
}
