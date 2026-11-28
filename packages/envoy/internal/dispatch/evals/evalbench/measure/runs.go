package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// The first paint's runs (k8): run.sh paints this head's first paint EVALBENCH_PAINT_RUNS times, 8
// by default, each as the first requests after a fresh start of the server, and each run's paint
// (`measure paint -run <n> -runs-file <file>`) appends its reads to the file, one line a run, once
// it has judged them. `measure runs` then reads the file and prints, for each read and each group
// of reads, its database time in every run with the median and the maximum of the runs, and fails
// a read or a group that is over its budget in any run, naming the runs; at ten times the stated
// run rate, where no check gates the database time, that line is a RECORD line
// (report.overStated). The budget binds every run, not the median: k8 holds each of the runs to
// it. A file that does not hold each of the runs 1 to -runs once, or a run that lacks a read
// another run made, fails too, at any rate, since a run that ended before it recorded its reads
// would otherwise leave the median and the maximum to the runs that passed.

// paintRun is one run's reads as the runs file holds them.
type paintRun struct {
	Run   int         `json:"run"`
	Reads []paintRead `json:"reads"`
}

// paintRead is one read of a run: its database time, its own budget and its group's, none where
// zero.
type paintRead struct {
	Name          string  `json:"name"`
	DBMs          float64 `json:"db_ms"`
	BudgetMs      float64 `json:"budget_ms,omitempty"`
	Group         string  `json:"group,omitempty"`
	GroupBudgetMs float64 `json:"group_budget_ms,omitempty"`
}

// runOf is the run's reads as the runs file holds them: each measured read's own database time
// (dbMs of its own statements, the time judgePaint judges) and its budgets.
func runOf(run int, measured []measuredCase) paintRun {
	line := paintRun{Run: run, Reads: make([]paintRead, len(measured))}
	for index, one := range measured {
		line.Reads[index] = paintRead{Name: one.name, DBMs: dbMs(one.own), BudgetMs: one.budgetMs, Group: one.group, GroupBudgetMs: one.groupBudgetMs}
	}
	return line
}

// recordRun appends one run's line, line as JSON, to path: a first paint's run (paintRun) or a
// k14 paint's (k14PaintRecord).
func recordRun(path string, line any) error {
	encoded, err := json.Marshal(line)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// readRuns is every run path holds, in the order they were appended.
func readRuns(path string) ([]paintRun, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read the runs file: %w", err)
	}
	defer file.Close()
	var runs []paintRun
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var run paintRun
		if err := json.Unmarshal(scanner.Bytes(), &run); err != nil {
			return nil, fmt.Errorf("read the runs file %s: line %d: %w", path, len(runs)+1, err)
		}
		runs = append(runs, run)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the runs file %s: %w", path, err)
	}
	return runs, nil
}

// runSeries is one read's or one group's database time over the runs, in run order.
type runSeries struct {
	name     string
	budgetMs float64
	runs     []int
	values   []float64
}

// judgeRuns prints each read's and each group's database time over runs, with the median and the
// maximum, and fails one over its budget in any run (judgeSeries), a file that does not hold each of
// the runs 1 to want once, and a read or a group missing from a run.
func judgeRuns(r *report, runs []paintRun, want int) {
	counts := map[int]int{}
	for _, run := range runs {
		counts[run.Run]++
	}
	var held []int
	for number := range counts {
		held = append(held, number)
	}
	slices.Sort(held)
	complete := len(runs) == want
	for number := 1; number <= want; number++ {
		complete = complete && counts[number] == 1
	}
	if !complete {
		r.fail("k8 the runs file holds %d runs, numbered %v, not each of the runs 1 to %d once", len(runs), held, want)
	}
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(a, b paintRun) int { return a.Run - b.Run })
	var reads, groups []*runSeries
	find := func(list *[]*runSeries, name string, budget float64) *runSeries {
		for _, one := range *list {
			if one.name == name {
				return one
			}
		}
		one := &runSeries{name: name, budgetMs: budget}
		*list = append(*list, one)
		return one
	}
	for _, run := range runs {
		sums := map[string]float64{}
		for _, read := range run.Reads {
			one := find(&reads, read.Name, read.BudgetMs)
			one.runs, one.values = append(one.runs, run.Run), append(one.values, read.DBMs)
			if read.Group != "" {
				find(&groups, read.Group, read.GroupBudgetMs)
				sums[read.Group] += read.DBMs
			}
		}
		for _, group := range groups {
			if sum, measured := sums[group.name]; measured {
				group.runs, group.values = append(group.runs, run.Run), append(group.values, sum)
			}
		}
	}
	for _, one := range reads {
		judgeSeries(r, "case", one, len(runs))
	}
	for _, one := range groups {
		judgeSeries(r, "group", one, len(runs))
	}
}

// judgeSeries prints one read's or group's line over the runs and fails it where it lacks a run, or
// where a run is over its budget, which k8 states at the stated run rate (report.overStated).
func judgeSeries(r *report, kind string, one *runSeries, runs int) {
	budget := "none"
	if one.budgetMs > 0 {
		budget = fmt.Sprintf("%.0f", one.budgetMs)
	}
	r.result("k8 %s=%s runs=%d db_ms=%s median_db_ms=%.2f max_db_ms=%.2f budget_ms=%s", kind, one.name, len(one.values),
		formatMs(one.values), median(one.values), slices.Max(one.values), budget)
	if len(one.values) != runs {
		r.fail("k8 %s=%s was measured in runs %v alone, of %d", kind, one.name, one.runs, runs)
	}
	if one.budgetMs <= 0 {
		return
	}
	var over []string
	for index, value := range one.values {
		if value > one.budgetMs {
			over = append(over, fmt.Sprintf("%d (%.2f ms)", one.runs[index], value))
		}
	}
	if len(over) > 0 {
		r.overStated("k8 %s=%s is over its budget of %.0f ms in %d of %d runs: run %s", kind, one.name, one.budgetMs, len(over), len(one.values),
			strings.Join(over, ", run "))
	}
}
