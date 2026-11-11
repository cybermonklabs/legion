package legionfacts

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// factsProject is the project every row below belongs to, and factsOpened when each of its issues
// was created: a row carries an opening whatever its work came to.
const factsProject = "LEGION"

var factsOpened = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// factsDatabase is a database of its own, with no Legion history: these checks state each span
// themselves, so the span a change carries is the one the check wrote and no computation's.
func factsDatabase(t *testing.T) *store.Pool {
	t.Helper()
	return storetest.Open(t).Pool
}

// workedAt is a moment of the history these checks state their spans in: the hour of a UTC day of
// November 2026.
func workedAt(day, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC)
}

// workedRow is one issue's row carrying the span of its measured work. A nil first and last is an
// issue with no measured work, which still has a row.
func workedRow(issue string, first, last *time.Time) Row {
	return Row{Project: factsProject, Issue: issue, OpenedAt: factsOpened, WorkFirstAt: first, WorkLastAt: last}
}

// changeLogHead is the newest change the log holds, which a check reads before its own write so
// its assertions see only the rows that write logged.
func changeLogHead(t *testing.T, pool *store.Pool) int64 {
	t.Helper()
	seq, err := delivery.LatestChangeSeq(t.Context(), pool)
	if err != nil {
		t.Fatalf("read the change log's head: %v", err)
	}
	return seq
}

// changesLogged is every change after seq, oldest first.
func changesLogged(t *testing.T, pool *store.Pool, seq int64) []delivery.Change {
	t.Helper()
	changes, err := delivery.ChangesSince(t.Context(), pool, seq, 1000)
	if err != nil {
		t.Fatalf("read the changes after %d: %v", seq, err)
	}
	return changes
}

// sameMoment is whether a change's end of a span is the moment wanted.
func sameMoment(got *time.Time, want time.Time) bool { return got != nil && got.Equal(want) }

// spanDays is the UTC days a change's span covers, both ends counted: the days a reader of
// per-day rows rewrites for that change.
func spanDays(change delivery.Change) []string {
	if change.TOld == nil || change.TNew == nil {
		return nil
	}
	days := []string{}
	for day := change.TOld.UTC().Truncate(24 * time.Hour); !day.After(change.TNew.UTC()); day = day.AddDate(0, 0, 1) {
		days = append(days, day.Format(time.DateOnly))
	}
	return days
}

// changesByIssue is the changes read, by the issue each names in its ref.
func changesByIssue(changes []delivery.Change) map[string]delivery.Change {
	by := make(map[string]delivery.Change, len(changes))
	for _, change := range changes {
		by[change.Ref] = change
	}
	return by
}

// TestOneFactsWriteLogsOneIssueFactsChange: a write of one issue's facts logs exactly one change
// of the new kind, carrying the project in repo and the issue in ref. Neither column holds a
// repository: a Legion fact belongs to a project and an issue.
func TestOneFactsWriteLogsOneIssueFactsChange(t *testing.T) {
	pool := factsDatabase(t)
	first, last := workedAt(4, 9), workedAt(4, 17)
	head := changeLogHead(t, pool)

	written, err := Write(t.Context(), pool, []Row{workedRow("LEGION-1", &first, &last)})
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
	if !sameMoment(logged.TOld, first) || !sameMoment(logged.TNew, last) {
		t.Fatalf("the change's span = %v to %v, want %s to %s", logged.TOld, logged.TNew, first, last)
	}
	t.Logf("1 row written, %d issue changed, %d change logged: kind %s, repo %s, ref %s, %d day",
		len(written), len(changes), logged.Kind, logged.Repo, logged.Ref, len(spanDays(logged)))
}

// TestAFactsWriteThatMovesOnlyTheUpdatedTimeLogsNoChange: a sweep that found nothing new logs
// nothing. Write leaves a row whose facts already match alone, and a rewrite of updated_at by
// itself moves the one column the trigger ignores.
func TestAFactsWriteThatMovesOnlyTheUpdatedTimeLogsNoChange(t *testing.T) {
	pool := factsDatabase(t)
	first, last := workedAt(4, 9), workedAt(4, 17)
	row := workedRow("LEGION-2", &first, &last)
	if _, err := Write(t.Context(), pool, []Row{row}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	head := changeLogHead(t, pool)

	row.UpdatedAt = factsOpened.Add(48 * time.Hour)
	rewritten, err := Write(t.Context(), pool, []Row{row})
	if err != nil {
		t.Fatalf("write the same facts again: %v", err)
	}
	if len(rewritten) != 0 {
		t.Errorf("Write rewrote %v, want nothing: the row's facts already match", rewritten)
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
	t.Logf("%d rows rewritten by Write, %d row's updated_at moved, %d changes logged",
		len(rewritten), moved.RowsAffected(), len(changes))
}

// TestAFactsChangesSpanNamesTheDaysTheWorkFallsIn: the span a change carries names the days a
// reader of per-day rows rewrites. An issue whose work falls in two UTC days names both days, and
// one whose work falls in a single day names that day alone.
func TestAFactsChangesSpanNamesTheDaysTheWorkFallsIn(t *testing.T) {
	pool := factsDatabase(t)
	for _, test := range []struct {
		issue       string
		first, last time.Time
		wantDays    []string
	}{
		{"LEGION-3", workedAt(4, 22), workedAt(5, 3), []string{"2026-11-04", "2026-11-05"}},
		{"LEGION-4", workedAt(6, 1), workedAt(6, 23), []string{"2026-11-06"}},
	} {
		head := changeLogHead(t, pool)
		if _, err := Write(t.Context(), pool, []Row{workedRow(test.issue, &test.first, &test.last)}); err != nil {
			t.Fatalf("write the facts of %s: %v", test.issue, err)
		}
		changes := changesLogged(t, pool, head)
		if len(changes) != 1 {
			t.Fatalf("the facts of %s logged %d changes, want one: %+v", test.issue, len(changes), changes)
		}
		days := spanDays(changes[0])
		if !slices.Equal(days, test.wantDays) {
			t.Errorf("%s spans the days %v, want %v", test.issue, days, test.wantDays)
		}
		t.Logf("%s worked %s to %s: %d change logged over %d day(s), %v", test.issue,
			test.first.Format(time.RFC3339), test.last.Format(time.RFC3339), len(changes), len(days), days)
	}
}

// TestAFactsUpdateThatMovesTheWorkCoversBothDays: an update carries the union of the old row's
// span and the new row's, so the day that held the work and the day that holds it now are both
// rewritten. A row that had no measured work carries null at both ends, and the union of that
// with a real span is the real span.
func TestAFactsUpdateThatMovesTheWorkCoversBothDays(t *testing.T) {
	pool := factsDatabase(t)
	wasFirst, wasLast := workedAt(4, 9), workedAt(4, 17)
	nowFirst, nowLast := workedAt(5, 9), workedAt(5, 17)
	if _, err := Write(t.Context(), pool, []Row{
		workedRow("LEGION-5", &wasFirst, &wasLast),
		workedRow("LEGION-6", nil, nil),
	}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	head := changeLogHead(t, pool)

	written, err := Write(t.Context(), pool, []Row{
		workedRow("LEGION-5", &nowFirst, &nowLast),
		workedRow("LEGION-6", &nowFirst, &nowLast),
	})
	if err != nil {
		t.Fatalf("move the work of both issues: %v", err)
	}
	changes := changesLogged(t, pool, head)
	if len(changes) != 2 {
		t.Fatalf("two moved rows logged %d changes, want two: %+v", len(changes), changes)
	}
	byIssue := changesByIssue(changes)
	moved, measured := byIssue["LEGION-5"], byIssue["LEGION-6"]
	if !sameMoment(moved.TOld, wasFirst) || !sameMoment(moved.TNew, nowLast) {
		t.Fatalf("the moved issue's span = %v to %v, want %s to %s, which covers both days",
			moved.TOld, moved.TNew, wasFirst, nowLast)
	}
	movedDays := spanDays(moved)
	if want := []string{"2026-11-04", "2026-11-05"}; !slices.Equal(movedDays, want) {
		t.Errorf("the moved issue spans the days %v, want %v", movedDays, want)
	}
	if !sameMoment(measured.TOld, nowFirst) || !sameMoment(measured.TNew, nowLast) {
		t.Fatalf("the newly measured issue's span = %v to %v, want %s to %s, the real span of the two",
			measured.TOld, measured.TNew, nowFirst, nowLast)
	}
	measuredDays := spanDays(measured)
	if want := []string{"2026-11-05"}; !slices.Equal(measuredDays, want) {
		t.Errorf("the newly measured issue spans the days %v, want %v", measuredDays, want)
	}
	t.Logf("%d rows changed, %d changes logged: the moved issue spans %d days %v, the newly measured one %d day %v",
		len(written), len(changes), len(movedDays), movedDays, len(measuredDays), measuredDays)
}
