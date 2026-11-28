package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
)

// k14's first paints: `measure paint -mode k14 -project <key>` after a fresh start of the server,
// one run a start, measures k14Cases, the Evals tab's list read at 84 days with a compare range and
// at 28 days with none, of a k14 project, each held to the first paint's server budget (0.4 s) and
// to its database budget (150 ms and 60 ms) at both rates (paintCase.strict), and each judged here
// (judgeK14Paint) by what it answered:
//
//   - it read the stored figures, both statements of evals.ReadStoredFiguresIn (storedFiguresReads,
//     the rule checkStoredFiguresReads holds the head's paint to), and the range figures read
//     served the project's deploy-frequency, which is keyed by the deploy repository: the fold of
//     the union cut of all the project's deploy repositories, where the code before it folded the
//     day rows of a project with two deploy sources on each read;
//   - its items are, item for item, the ones the warm server answered the same read before the
//     restart (`measure k14`'s -expect-file), and its deploy-frequency holds a value over the
//     window: a first paint that answered other numbers than the page had, or none, measured
//     nothing of the page.
//
// Each run appends its reads to -runs-file, and `measure k14-paints` then prints each project's
// reads over the runs and fails where fewer than both k14 projects' two reads were measured.

// k14PaintRecord is one run's reads of one project as the runs file holds them.
type k14PaintRecord struct {
	Project string         `json:"project"`
	Run     int            `json:"run"`
	Reads   []k14PaintRead `json:"reads"`
}

// k14PaintRead is one read of a k14 paint run: its database time and server time, their budgets,
// whether it was the first read after the restart, and whether it passed every rule of
// judgeK14Paint.
type k14PaintRead struct {
	Name           string  `json:"name"`
	DBMs           float64 `json:"db_ms"`
	ServerMs       float64 `json:"server_ms"`
	BudgetMs       float64 `json:"budget_ms"`
	ServerBudgetMs float64 `json:"server_budget_ms"`
	First          bool    `json:"first"`
	Answered       bool    `json:"answered"`
}

// judgeK14Paint judges a k14 paint's reads by what they answered (the comment above), prints one
// line each, and appends the run's reads to -runs-file.
func judgeK14Paint(r *report, o options, measured []measuredCase) error {
	var expected []k14Expected
	if o.expectFile != "" {
		var err error
		if expected, err = readExpected(o.expectFile); err != nil {
			return fmt.Errorf("read the warm reads %s: %w", o.expectFile, err)
		}
	}
	record := k14PaintRecord{Project: o.project, Run: o.run}
	deployFrequency := evals.EvalID(o.project, evals.SlugDeployFrequency)
	stamp := k14Stamp(r, o)
	for index, one := range measured {
		problems := k14PaintProblems(one, expected, o.project, deployFrequency)
		for _, problem := range problems {
			r.fail("k14-paint project=%s case=%s: %s", o.project, one.name, problem)
		}
		var reads []string
		if found, err := storedFiguresReads(one); err == nil {
			for _, statement := range found {
				reads = append(reads, fmt.Sprintf("%s=%d", statement.name, statement.queryID))
			}
		}
		read := k14PaintRead{Name: one.name, DBMs: dbMs(one.own), ServerMs: one.serverMs, BudgetMs: one.budgetMs, ServerBudgetMs: one.serverBudgetMs,
			First: index == 0, Answered: len(problems) == 0 && one.status == 200}
		record.Reads = append(record.Reads, read)
		r.result("k14-paint project=%s run=%d case=%s first_after_restart=%t status=%d db_ms=%.2f budget_ms=%.0f server_ms=%.0f server_budget_ms=%.0f stored_figures=[%s] answered_as_warm=%t %s",
			o.project, o.run, one.name, read.First, one.status, read.DBMs, read.BudgetMs, read.ServerMs, read.ServerBudgetMs, strings.Join(reads, " "), read.Answered, stamp)
	}
	if o.runsFile == "" {
		return nil
	}
	return recordRun(o.runsFile, record)
}

// servedEvals matches the range figures read's condition on the evals it serves
// (evals.ReadStoredFiguresIn: `eval_id = any($3::text[])`), whose parameter names them.
var servedEvals = regexp.MustCompile(`\beval_id = any\(\$([0-9]+)::text\[\]\)`)

// k14PaintProblems is each rule of judgeK14Paint one read breaks.
func k14PaintProblems(one measuredCase, expected []k14Expected, project, deployFrequency string) []string {
	var problems []string
	found, err := storedFiguresReads(one)
	if err != nil {
		problems = append(problems, err.Error())
	} else if !slices.ContainsFunc(found, func(statement probedStatement) bool {
		match := servedEvals.FindStringSubmatch(one.statements[statement.queryID])
		if statement.name != "range-figures" || match == nil {
			return false
		}
		n, _ := strconv.Atoi(match[1])
		params := one.params[statement.queryID]
		return n >= 1 && n <= len(params) && strings.Contains(params[n-1], deployFrequency)
	}) {
		problems = append(problems, fmt.Sprintf("its range figures read served no %s, so the project's deploy evals were folded from the day rows, not read from the stored figures of its union cut", deployFrequency))
	}
	items, err := itemsOf(one.body)
	if err != nil {
		return append(problems, fmt.Sprintf("its answer holds no items: %v", err))
	}
	if !windowValued(items[deployFrequency]) {
		problems = append(problems, fmt.Sprintf("its answer holds no value of %s over the window", deployFrequency))
	}
	at := slices.IndexFunc(expected, func(warm k14Expected) bool { return warm.Project == project && warm.Case == one.name })
	switch {
	case expected == nil:
	case at < 0:
		problems = append(problems, "the warm reads hold none of it, so its answer was compared with nothing")
	default:
		if differing := differingItems(expected[at].Items, items); len(differing) > 0 {
			problems = append(problems, fmt.Sprintf("its items differ from the warm read's before the restart: %s", strings.Join(differing, ", ")))
		}
	}
	return problems
}

// windowValued reports whether an eval list item holds a value over the window in a series.
func windowValued(item string) bool {
	var decoded struct {
		Series []struct {
			Window map[string]*float64 `json:"window"`
		} `json:"series"`
	}
	if item == "" || json.Unmarshal([]byte(item), &decoded) != nil {
		return false
	}
	for _, series := range decoded.Series {
		for _, value := range series.Window {
			if value != nil {
				return true
			}
		}
	}
	return false
}

// k14Paints is `measure k14-paints`: it reads the k14 paints' runs file and prints, for each k14
// project and each of its two reads, the runs, the most database and server time of any run, and
// whether every run answered as the warm read did; it fails where a project or a read has no run,
// since then fewer than both projects' first paints were measured.
func k14Paints(r *report, o options) error {
	file, err := os.Open(o.runsFile)
	if err != nil {
		return fmt.Errorf("read the k14 paints file: %w", err)
	}
	defer file.Close()
	var records []k14PaintRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var record k14PaintRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("read the k14 paints file %s: line %d: %w", o.runsFile, len(records)+1, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	judgeK14Paints(r, records)
	return nil
}

// judgeK14Paints is k14Paints' judgement of records.
func judgeK14Paints(r *report, records []k14PaintRecord) {
	projects := 0
	for _, project := range evalbench.K14Projects {
		whole := true
		for _, name := range []string{"list-84d-compare", "list-28d"} {
			runs, first, answered := 0, 0, true
			db, server := 0.0, 0.0
			for _, record := range records {
				if record.Project != project {
					continue
				}
				for _, read := range record.Reads {
					if read.Name != name {
						continue
					}
					runs++
					db, server = max(db, read.DBMs), max(server, read.ServerMs)
					answered = answered && read.Answered
					if read.First {
						first++
					}
				}
			}
			r.result("k14-paints project=%s case=%s runs=%d first_after_restart=%d max_db_ms=%.2f max_server_ms=%.0f answered_as_warm=%t",
				project, name, runs, first, db, server, answered && runs > 0)
			if runs == 0 || first == 0 {
				r.fail("k14-paints project=%s case=%s: no run measured it as the first read after a restart", project, name)
				whole = false
			}
		}
		if whole {
			projects++
		}
	}
	r.result("k14-paints projects_measured=%d of=%d", projects, len(evalbench.K14Projects))
	if projects < 2 {
		r.fail("k14-paints: the first paints of %d projects were measured, fewer than two", projects)
	}
}
