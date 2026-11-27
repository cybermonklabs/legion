package legionfacts

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// factsProject is the project every row below belongs to, and factsOpened when each of its issues
// was created: a row carries an opening whatever its work came to.
const factsProject = "LEGION"

var factsOpened = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// factsDatabase is a database of its own, with no Legion history: these checks state each close
// themselves, so the days a change names are the ones the check wrote and no computation's.
func factsDatabase(t *testing.T) *store.Pool {
	t.Helper()
	return storetest.Open(t).Pool
}

// workedAt is a moment of the history these checks state their rows in: the hour of a UTC day of
// November 2026.
func workedAt(day, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC)
}

// workedRow is one issue's row carrying the span of its measured work and its close, nil while it
// is open. A nil first and last is an issue with no measured work, which still has a row.
func workedRow(issue string, first, last, closed *time.Time) Row {
	return Row{Project: factsProject, Issue: issue, ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
		OpenedAt: factsOpened, WorkFirstAt: first, WorkLastAt: last, ClosedAt: closed,
	}}}
}

// changeLogHead is the newest change the log holds once the outbox is drained, which a check reads
// before its own write so its assertions see only the rows that write logged.
func changeLogHead(t *testing.T, pool *store.Pool) int64 {
	t.Helper()
	if _, err := delivery.DrainPending(t.Context(), pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	head, err := delivery.ChangesHead(t.Context(), pool)
	if err != nil {
		t.Fatalf("read the change log's head: %v", err)
	}
	return head.Seq
}

// changesLogged is every change after seq, oldest first. The facts' triggers record their changes
// in the change log's outbox, and WriteProject is called here outside the pass that drains after
// its commit (writePass), so it drains first, as the live hub drains before it reads.
func changesLogged(t *testing.T, pool *store.Pool, seq int64) []delivery.Change {
	t.Helper()
	if _, err := delivery.DrainPending(t.Context(), pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	changes, err := delivery.ChangesSince(t.Context(), pool, seq, 1000)
	if err != nil {
		t.Fatalf("read the changes after %d: %v", seq, err)
	}
	return changes
}

// sameMoment is whether one of a change's two moments is the moment wanted.
func sameMoment(got *time.Time, want time.Time) bool { return got != nil && got.Equal(want) }

// markedDays is the UTC days a change names, each once and in order: the day of its t_old and the
// day of its t_new, where each is set. They are the days a reader of per-day rows rewrites for
// that change (evals' refresher), so for a facts change they must be the issue's close day before
// the change and after it, and no other.
func markedDays(change delivery.Change) []string {
	days := []string{}
	for _, at := range []*time.Time{change.TOld, change.TNew} {
		if at != nil {
			days = append(days, at.UTC().Format(time.DateOnly))
		}
	}
	slices.Sort(days)
	return slices.Compact(days)
}

// closeMarkProblems is what is wrong with the days a facts change marked, against the close days
// the change moved the issue between: a marked day that is none of them, and one of them left
// unmarked. An open issue's change wants no day at all.
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

// TestOneFactsWriteLogsOneIssueFactsChange: a write of one issue's facts logs exactly one change
// of the new kind, carrying the project in repo and the issue in ref. Neither column holds a
// repository: a Legion fact belongs to a project and an issue.
func TestOneFactsWriteLogsOneIssueFactsChange(t *testing.T) {
	pool := factsDatabase(t)
	first, last, closed := workedAt(4, 9), workedAt(4, 17), workedAt(4, 18)
	head := changeLogHead(t, pool)

	written, err := WriteProject(t.Context(), pool, ProjectWrite{
		Project: factsProject, Rows: []Row{workedRow("LEGION-1", &first, &last, &closed)}, Whole: true,
	})
	if err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	changes := changesLogged(t, pool, head)
	if len(changes) != 1 {
		t.Fatalf("one facts write logged %d changes, want one: %+v", len(changes), changes)
	}
	logged := changes[0]
	if logged.Kind != "issue_facts" || logged.Repo != factsProject || logged.Ref != "LEGION-1" || logged.RunKind != "" {
		t.Fatalf("the change = %+v, want kind issue_facts, repo %s, ref LEGION-1 and no run kind", logged, factsProject)
	}
	if logged.TOld != nil || !sameMoment(logged.TNew, closed) {
		t.Fatalf("the change's moments = %v and %v, want none before (the row is new) and its close %s after",
			logged.TOld, logged.TNew, closed)
	}
	t.Logf("1 row written, %d issue changed, %d change logged: kind %s, repo %s, ref %s, marking %v",
		len(written), len(changes), logged.Kind, logged.Repo, logged.Ref, markedDays(logged))
}

// TestAFactsWriteThatMovesOnlyTheUpdatedTimeLogsNoChange: a pass whose inputs did not move logs
// nothing. WriteProject leaves a row whose facts already match alone, and a rewrite of updated_at
// by itself moves the one column the trigger ignores.
func TestAFactsWriteThatMovesOnlyTheUpdatedTimeLogsNoChange(t *testing.T) {
	pool := factsDatabase(t)
	first, last := workedAt(4, 9), workedAt(4, 17)
	row := workedRow("LEGION-2", &first, &last, nil)
	if _, err := WriteProject(t.Context(), pool, ProjectWrite{Project: factsProject, Rows: []Row{row}, Whole: true}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	head := changeLogHead(t, pool)

	row.UpdatedAt = factsOpened.Add(48 * time.Hour)
	rewritten, err := WriteProject(t.Context(), pool, ProjectWrite{Project: factsProject, Rows: []Row{row}, Whole: true})
	if err != nil {
		t.Fatalf("write the same facts again: %v", err)
	}
	if len(rewritten) != 0 {
		t.Errorf("WriteProject rewrote %v, want nothing: the row's facts already match", rewritten)
	}
	moved, err := pool.Exec(t.Context(), `update issue_facts set updated_at = updated_at + interval '1 hour'`)
	if err != nil {
		t.Fatalf("move the row's updated_at: %v", err)
	}
	if moved.RowsAffected() != 1 {
		t.Fatalf("the update moved the updated_at of %d rows, want the one row written above: an "+
			"update that hit no row proves nothing about what the trigger ignores", moved.RowsAffected())
	}
	changes := changesLogged(t, pool, head)
	if len(changes) != 0 {
		t.Fatalf("a pass that moved only the row's time logged %+v, want nothing", changes)
	}
	t.Logf("%d rows rewritten by WriteProject, %d row's updated_at moved, %d changes logged",
		len(rewritten), moved.RowsAffected(), len(changes))
}

// TestAFactsChangeMarksTheIssuesCloseDaysAndNoWorkDay: a change of an issue's facts marks the day
// rows that count the issue, which are its close day before the change and after it, because a
// Legion day row counts an issue on its close day alone (dispatch://INFRA-2916, R26). The change's
// t_old holds the row's closed_at before and its t_new the closed_at after, each null where the
// issue was or is open (migration 20261117100002), so no day of the issue's work is named.
//
// One closed issue whose work falls on two days is written, its work moved with its close where
// it stood, its close moved two days on, reopened and closed again, and then its row deleted; an
// issue that stays open has its facts move twice. Each change is judged by the days it marks
// against the close days it moved between, and the same judgement over a change naming the
// issue's work days, the negative control, must find it wrong.
func TestAFactsChangeMarksTheIssuesCloseDaysAndNoWorkDay(t *testing.T) {
	pool := factsDatabase(t)
	first, last := workedAt(4, 22), workedAt(5, 3)
	movedFirst, movedLast := workedAt(4, 20), workedAt(5, 6)
	closed, closedLater, closedAgain := workedAt(6, 10), workedAt(8, 9), workedAt(9, 15)
	steps := []struct {
		what   string
		write  ProjectWrite
		closes []string
	}{
		{"a closed issue whose work falls on two days is written",
			ProjectWrite{Rows: []Row{workedRow("LEGION-3", &first, &last, &closed)}, Stated: []string{"LEGION-3"}},
			[]string{"2026-11-06"}},
		{"its work moves and its close stands",
			ProjectWrite{Rows: []Row{workedRow("LEGION-3", &movedFirst, &movedLast, &closed)}, Stated: []string{"LEGION-3"}},
			[]string{"2026-11-06"}},
		{"its close moves two days on",
			ProjectWrite{Rows: []Row{workedRow("LEGION-3", &movedFirst, &movedLast, &closedLater)}, Stated: []string{"LEGION-3"}},
			[]string{"2026-11-06", "2026-11-08"}},
		{"it reopens",
			ProjectWrite{Rows: []Row{workedRow("LEGION-3", &movedFirst, &movedLast, nil)}, Stated: []string{"LEGION-3"}},
			[]string{"2026-11-08"}},
		{"it closes again",
			ProjectWrite{Rows: []Row{workedRow("LEGION-3", &movedFirst, &movedLast, &closedAgain)}, Stated: []string{"LEGION-3"}},
			[]string{"2026-11-09"}},
		{"its row goes",
			ProjectWrite{Stated: []string{"LEGION-3"}},
			[]string{"2026-11-09"}},
		{"an open issue whose work falls on two days is written",
			ProjectWrite{Rows: []Row{workedRow("LEGION-4", &first, &last, nil)}, Stated: []string{"LEGION-4"}},
			nil},
		{"the open issue's work moves",
			ProjectWrite{Rows: []Row{workedRow("LEGION-4", &movedFirst, &movedLast, nil)}, Stated: []string{"LEGION-4"}},
			nil},
	}
	marks := 0
	for _, step := range steps {
		head := changeLogHead(t, pool)
		step.write.Project = factsProject
		if _, err := WriteProject(t.Context(), pool, step.write); err != nil {
			t.Fatalf("%s: write the facts: %v", step.what, err)
		}
		changes := changesLogged(t, pool, head)
		if len(changes) != 1 {
			t.Fatalf("%s: logged %d changes, want one: %+v", step.what, len(changes), changes)
		}
		marked := markedDays(changes[0])
		marks += len(marked)
		for _, problem := range closeMarkProblems(marked, step.closes) {
			t.Errorf("%s: the change %s (it marks %v, the close days are %v)", step.what, problem, marked, step.closes)
		}
		t.Logf("%s: the change carries %v and %v and marks %v; the close days are %v", step.what,
			changes[0].TOld, changes[0].TNew, marked, step.closes)
	}
	if marks == 0 {
		t.Fatal("no change marked a day, so the judgement above read nothing")
	}

	// The negative control: a change naming the issue's work days is judged by the same rule and
	// found wrong.
	workSpan := delivery.Change{Kind: "issue_facts", Repo: factsProject, Ref: "LEGION-3", TOld: &movedFirst, TNew: &movedLast}
	problems := closeMarkProblems(markedDays(workSpan), []string{"2026-11-06", "2026-11-08"})
	if len(problems) == 0 {
		t.Errorf("a mark of the work's days %v passes as the close days, so the judgement proves nothing", markedDays(workSpan))
	}
	t.Logf("%d changes judged, %d days marked; the negative control, a mark of the work's days %v, is found wrong: %v",
		len(steps), marks, markedDays(workSpan), problems)
}
