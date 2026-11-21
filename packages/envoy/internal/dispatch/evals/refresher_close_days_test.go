package evals

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// factsMarks is the Legion days the issue_facts changes of issue logged after seq mark, each as a
// pass's report names a day (line): the day of each change's t_old and of its t_new, where set.
// The facts' trigger records its changes in the change log's outbox, so this moves them into the
// log first, as the refresher's pass does before it reads.
func (r *refresherRig) factsMarks(t *testing.T, seq int64, project, issue string) []string {
	t.Helper()
	if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 1000)
	if err != nil {
		t.Fatalf("read the changes after %d: %v", seq, err)
	}
	marked := []string{}
	for _, change := range changes {
		if change.Kind != delivery.ChangeKindIssueFacts || change.Repo != project || change.Ref != issue {
			continue
		}
		for _, at := range []*time.Time{change.TOld, change.TNew} {
			if at != nil {
				marked = append(marked, line(walkLegion, project, measures.Midnight(*at)))
			}
		}
	}
	slices.Sort(marked)
	return slices.Compact(marked)
}

// closeMarkProblems is what is wrong with the days one change of an issue's facts marked, against
// the issue's close days the change moved between, each a pass's report line: a day marked that is
// none of them, which counts no change of the issue, and a close day left unmarked. An open
// issue's change wants no day at all.
func closeMarkProblems(marked, closes []string) []string {
	var problems []string
	for _, day := range marked {
		if !slices.Contains(closes, day) {
			problems = append(problems, "marks "+day+", which is no close day of the issue")
		}
	}
	for _, day := range closes {
		if !slices.Contains(marked, day) {
			problems = append(problems, "leaves the close day "+day+" unmarked")
		}
	}
	return problems
}

// TestAFactsChangeMarksAndRewritesTheIssuesCloseDaysAlone: over the hand-written Legion history of
// EVAL, once every day is walked, each change of an issue's stored facts marks the Legion days
// that count the issue, its close day before the change and after it, and the pass after it
// rewrites exactly those days and no row of any other (expectDays; dispatch://INFRA-2916, R26 and
// check k29):
//   - EVAL-3, closed on the history's third day, gains work on its first and second days: the
//     change marks its close day alone, and that day alone is rewritten;
//   - its close moves to the fifth day: the change marks the third and the fifth, and those two
//     alone are rewritten, neither of its two work days;
//   - EVAL-4, open, gains work on two days: the change marks no day and nothing is rewritten.
//
// The negative control is a change naming EVAL-3's work days in place of its close days: the pass
// rewrites those two days, which count no change of the issue, and the same judgement of the marks
// finds it wrong.
func TestAFactsChangeMarksAndRewritesTheIssuesCloseDaysAlone(t *testing.T) {
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	workFirst, workLast := legionHistoryDay(1).Add(20*time.Hour), history.d1.Add(6*time.Hour)
	closedLater := legionHistoryDay(5).Add(9 * time.Hour)
	legionDay := func(day time.Time) string { return line(walkLegion, "EVAL", measures.Midnight(day)) }

	worked := history.rows["EVAL-3"]
	worked.WorkFirstAt, worked.WorkLastAt, worked.WorkHours = &workFirst, &workLast, 5
	moved := worked
	moved.ClosedAt = &closedLater
	open := history.rows["EVAL-4"]
	open.WorkFirstAt, open.WorkLastAt, open.WorkHours = &workFirst, &workLast, 5
	steps := []struct {
		what   string
		row    string
		write  func()
		closes []string
	}{
		{"EVAL-3 gains work on two days and its close stands", "EVAL-3", func() {
			history.rows["EVAL-3"] = worked
			r.writeFacts(t, history, "EVAL-3")
		}, []string{legionDay(history.d2)}},
		{"EVAL-3's close moves two days on", "EVAL-3", func() {
			history.rows["EVAL-3"] = moved
			r.writeFacts(t, history, "EVAL-3")
		}, []string{legionDay(history.d2), legionDay(closedLater)}},
		{"EVAL-4 gains work on two days and stays open", "EVAL-4", func() {
			history.rows["EVAL-4"] = open
			r.writeFacts(t, history, "EVAL-4")
		}, nil},
	}
	before := r.dayRows(t)
	marks := 0
	for _, step := range steps {
		seq := r.latestSeq(t)
		step.write()
		marked := r.factsMarks(t, seq, "EVAL", step.row)
		marks += len(marked)
		for _, problem := range closeMarkProblems(marked, step.closes) {
			t.Errorf("%s: the change %s (marked %v, the close days are %v)", step.what, problem, marked, step.closes)
		}
		t.Logf("%s: the change marked %v; the close days are %v", step.what, marked, step.closes)
		before = r.expectDays(t, step.what, before, step.closes, nil)
	}
	if marks == 0 {
		t.Fatal("the changes marked no day, so the judgement read nothing")
	}

	// The negative control: a change of EVAL-3 naming its work days, which no day row counts it on.
	seq := r.latestSeq(t)
	r.exec(t, "log a change naming EVAL-3's work days", `select delivery_change_log_pending('issue_facts', null, 'EVAL', 'EVAL-3', $1, $2)`,
		workFirst, workLast)
	marked := r.factsMarks(t, seq, "EVAL", "EVAL-3")
	report, _ := r.pass(t)
	problems := closeMarkProblems(marked, []string{legionDay(closedLater)})
	if len(problems) == 0 {
		t.Errorf("a change marking EVAL-3's work days %v, whose pass rewrote %v, passes as one marking its close day, so the judgement proves nothing",
			marked, report.sorted())
	}
	t.Logf("%d changes judged, %d days marked. The negative control marked %v and its pass rewrote %v, found wrong: %s",
		len(steps), marks, marked, report.sorted(), strings.Join(problems, "; "))
}
