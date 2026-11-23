package store

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// agentComment is a writing_check_events row of an agent's comment read by alice, with outcome.
func agentComment(outcome string) WritingCheckEvent {
	return WritingCheckEvent{Route: "/api/v1/issues/{key}/comments", Method: "POST", ActorKind: "session", ActorID: "s-agent", Outcome: outcome, Reader: "alice"}
}

// insertWritingCheckEvents records events, each dated at its time.
func insertWritingCheckEvents(t *testing.T, store *Store, at map[time.Time][]WritingCheckEvent) {
	t.Helper()
	ctx := context.Background()
	for when, events := range at {
		for _, event := range events {
			if err := store.InsertWritingCheckEvent(ctx, event); err != nil {
				t.Fatalf("record %s: %v", event.Outcome, err)
			}
			if _, err := store.Pool.Exec(ctx, `update writing_check_events set created_at = $1 where id = (select max(id) from writing_check_events)`, when); err != nil {
				t.Fatalf("date %s: %v", event.Outcome, err)
			}
		}
	}
}

// A text posted over a finding is counted once, whatever rows record a finding of it: the writing
// check's findings row of a comment edit and the reviewer's reviewed_after of it, which name one
// write, count one text, on the project the reviewer's row names; so do a held text's
// posted_as_written beside the writing check's row of its write. A row of a write no other row
// names is a text of its own, and a review after the post that found nothing posts over no
// finding.
func TestTheWritingCheckCountsCountATextWithBothRowsOnce(t *testing.T) {
	store := openWordRecordsStore(t)
	day := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	checked, reviewed := agentComment("findings"), agentComment("reviewed_after")
	checked.Rules, checked.WriteID = []string{"phrasal-verb"}, "write-1"
	reviewed.Rules, reviewed.Project, reviewed.WriteID = []string{"noslop"}, "WCK", "write-1"
	heldCheck, held, posted := agentComment("findings"), agentComment("held"), agentComment("posted_as_written")
	heldCheck.Rules, heldCheck.WriteID = []string{"phrasal-verb"}, "held-1"
	held.Project, held.WriteID = "WCK", "held-1"
	posted.Rules, posted.Project, posted.WriteID = []string{"phrases"}, "WCK", "held-1"
	alone := agentComment("findings")
	alone.Rules = []string{"phrasal-verb"}
	clean := agentComment("reviewed_after")
	clean.Project, clean.WriteID = "WCK", "write-2"
	insertWritingCheckEvents(t, store, map[time.Time][]WritingCheckEvent{
		day:                      {checked, heldCheck, held, alone},
		day.Add(time.Minute):     {reviewed, clean},
		day.Add(2 * time.Minute): {posted},
	})
	counts, err := store.CountWritingCheck(context.Background(), day, day, 50, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	want := []WritingCheckRow{
		{Day: "2026-10-20", Project: "", Person: "alice", PostedOverFinding: 1},
		{Day: "2026-10-20", Project: "WCK", Person: "alice", ReviewerHeld: 1, PostedOverFinding: 2},
	}
	if !slices.Equal(counts.Items, want) {
		t.Errorf("rows = %+v\nwant %+v: the comment and the held text once each, and the write no other row names", counts.Items, want)
	}
	if got := counts.Days[0].Outcomes; got["findings"] != 3 || got["reviewed_after"] != 2 || got["posted_as_written"] != 1 {
		t.Errorf("the day's outcomes = %v, want every row counted", got)
	}
}

// A text whose rows lie on either side of a midnight is counted once, on the day of the row the
// counts pick for it, the first that names a project, whichever range is asked: each day asked
// alone gives it the day both days asked together give it. The writing check's row of write-1
// names no project, so the reviewer's row after midnight is picked; held-1's first row names one,
// so it is picked before midnight.
func TestTheWritingCheckCountsCountATextAcrossMidnightOnce(t *testing.T) {
	store := openWordRecordsStore(t)
	midnight := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	checked, reviewed := agentComment("findings"), agentComment("reviewed_after")
	checked.Rules, checked.WriteID = []string{"phrasal-verb"}, "write-1"
	reviewed.Rules, reviewed.Project, reviewed.WriteID = []string{"noslop"}, "WCK", "write-1"
	heldCheck, posted := agentComment("findings"), agentComment("posted_as_written")
	heldCheck.Rules, heldCheck.Project, heldCheck.WriteID = []string{"phrasal-verb"}, "WCK", "held-1"
	posted.Rules, posted.Project, posted.WriteID = []string{"phrases"}, "WCK", "held-1"
	insertWritingCheckEvents(t, store, map[time.Time][]WritingCheckEvent{
		midnight.Add(-30 * time.Second): {checked, heldCheck},
		midnight.Add(30 * time.Second):  {reviewed, posted},
	})
	overFinding := func(into map[string]int, first, last time.Time) {
		t.Helper()
		counts, err := store.CountWritingCheck(context.Background(), first, last, 50, 0)
		if err != nil {
			t.Fatalf("count %s to %s: %v", first, last, err)
		}
		for _, row := range counts.Items {
			into[row.Day] += row.PostedOverFinding
		}
	}
	before, after := midnight.AddDate(0, 0, -1), midnight
	want := map[string]int{"2026-10-20": 1, "2026-10-21": 1}
	both := map[string]int{}
	overFinding(both, before, after)
	if !maps.Equal(both, want) {
		t.Errorf("the two days together count %v, want %v", both, want)
	}
	apart := map[string]int{}
	overFinding(apart, before, before)
	overFinding(apart, after, after)
	if !maps.Equal(apart, want) {
		t.Errorf("the two days asked one by one count %v, want %v, as both together do", apart, want)
	}
}

// The sweep deletes a row R8's counts read once it is older than WritingCheckCountedRetention, and
// every other row once it is older than WritingCheckRetention. Every outcome the table's check
// admits is weighed, each row its own project: on a day older than WritingCheckRetention the counts
// read a row of each outcome the sweep keeps, and of none it deletes, so the counts and the sweep
// read their outcomes from one list. A reviewed_after row is read, and kept, only where it names a
// rule. No row older than WritingCheckCountedRetention stays, and every row inside
// WritingCheckRetention does.
func TestTheWritingCheckSweepKeepsEachRowTheCountsReadForItsRetention(t *testing.T) {
	store := openWordRecordsStore(t)
	ctx := context.Background()
	var check string
	if err := store.Pool.QueryRow(ctx, `select pg_get_constraintdef(oid) from pg_constraint where conname = 'writing_check_events_outcome_check'`).Scan(&check); err != nil {
		t.Fatalf("read the outcome check: %v", err)
	}
	var outcomes []string
	for _, match := range regexp.MustCompile(`'([a-z_]+)'::text`).FindAllStringSubmatch(check, -1) {
		outcomes = append(outcomes, match[1])
	}
	if !slices.Contains(outcomes, "clean") || !slices.Contains(outcomes, "rewrite_requested") {
		t.Fatalf("the outcome check %q admits %q", check, outcomes)
	}
	const namedRule = "reviewed_after naming a rule"
	now := time.Now().UTC().Truncate(time.Microsecond)
	aged, past, fresh := now.Add(-WritingCheckRetention-48*time.Hour), now.Add(-WritingCheckCountedRetention-48*time.Hour), now.Add(-time.Hour)
	rows := map[time.Time][]WritingCheckEvent{}
	for _, when := range []time.Time{aged, past, fresh} {
		for _, outcome := range outcomes {
			event := agentComment(outcome)
			event.Project = outcome
			rows[when] = append(rows[when], event)
		}
		named := agentComment("reviewed_after")
		named.Rules, named.Project = []string{"noslop"}, namedRule
		rows[when] = append(rows[when], named)
	}
	insertWritingCheckEvents(t, store, rows)
	every := len(outcomes) + 1
	read := []string{"findings", "held", "passed", "posted_as_written", "posted_unreviewed", namedRule, "rewrite_posted", "rewrite_requested"}
	slices.Sort(read)
	counted := func(what string) {
		t.Helper()
		counts, err := store.CountWritingCheck(ctx, aged, aged, 200, 0)
		if err != nil {
			t.Fatalf("count the aged day: %v", err)
		}
		var got []string
		for _, row := range counts.Items {
			if row.RewriteUses+row.ReviewerHeld+row.ReviewerRewrote+row.ReviewerPassed+row.ReviewerPostedUnreviewed+row.PostedOverFinding == 1 {
				got = append(got, row.Project)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, read) {
			t.Errorf("%s, the counts of the aged day read the rows of %q, want one each of %q", what, got, read)
		}
	}
	counted("before the sweep")
	deleted, err := store.SweepWritingCheckEvents(ctx)
	if want := int64(every + every - len(read)); err != nil || deleted != want {
		t.Fatalf("sweep = %d, %v; want %d deleted: every row past the counted retention, and each the counts do not read past the other", deleted, err, want)
	}
	kept := map[time.Time][]string{}
	result, err := store.Pool.Query(ctx, `select created_at, project from writing_check_events`)
	if err != nil {
		t.Fatalf("read the rows kept: %v", err)
	}
	defer result.Close()
	for result.Next() {
		var at time.Time
		var project string
		if err := result.Scan(&at, &project); err != nil {
			t.Fatalf("read a row kept: %v", err)
		}
		kept[at.UTC()] = append(kept[at.UTC()], project)
	}
	slices.Sort(kept[aged])
	if !slices.Equal(kept[aged], read) || len(kept[past]) != 0 || len(kept[fresh]) != every {
		t.Errorf("the sweep kept %q of the aged day, want %q; %q past the counted retention, want none; %d of the fresh rows, want all %d",
			kept[aged], read, kept[past], len(kept[fresh]), every)
	}
	counted("after the sweep")
}

// planOf is the plan, without costs, of sql run with args in tx: a custom plan, made for args.
func planOf(t *testing.T, tx pgx.Tx, sql string, args ...any) string {
	t.Helper()
	var plan strings.Builder
	rows, err := tx.Query(context.Background(), `explain (costs off) `+sql, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", sql, err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	return plan.String()
}

// genericPlanOf is the generic plan, without costs, of sql, the plan made for any value of its
// parameters, which a statement prepared and run again may take.
func genericPlanOf(t *testing.T, tx pgx.Tx, sql string) string {
	t.Helper()
	results, err := tx.Conn().PgConn().Exec(context.Background(), `explain (generic_plan, costs off) `+sql).ReadAll()
	if err != nil {
		t.Fatalf("explain the generic plan of %s: %v", sql, err)
	}
	var plan strings.Builder
	for _, result := range results {
		for _, row := range result.Rows {
			plan.WriteString(string(row[0]) + "\n")
		}
	}
	return plan.String()
}

// Each delete of the sweep reads the rows it deletes by an index, on a table of a few thousand
// rows most of which it keeps: the delete of the rows the counts do not read reads
// writing_check_events_uncounted_idx, the index of those rows alone, in its plan for the sweep's
// value and in its generic plan, and the delete of every row past the counted retention reads
// writing_check_events_created_at_idx. Neither reads the counted rows it keeps. The counts of one
// day read the other rows of a write with a row that day by writing_check_events_write_id_idx, not
// every row of the table.
func TestTheWritingCheckSweepAndCountsReadTheirRowsByIndex(t *testing.T) {
	store := openWordRecordsStore(t)
	ctx := context.Background()
	if _, err := store.Pool.Exec(ctx, `
		insert into writing_check_events (created_at, route, method, actor_kind, actor_id, outcome, rules, write_id)
		select now() - (91 + g % 270) * interval '1 day', '/api/v1/issues/{key}/comments', 'POST', 'session', 's-agent',
			(array['findings', 'held', 'posted_as_written', 'reviewed_after'])[1 + g % 4], '{noslop}', 'write-' || g
		from generate_series(1, 4000) g;
		insert into writing_check_events (created_at, route, method, actor_kind, actor_id, outcome)
		select now() - (g % 89) * interval '1 day', '/api/v1/issues/{key}/comments', 'POST', 'session', 's-agent', 'clean'
		from generate_series(1, 1000) g;
		insert into writing_check_events (created_at, route, method, actor_kind, actor_id, outcome)
		select now() - (91 + g) * interval '1 day', '/api/v1/issues/{key}/comments', 'POST', 'session', 's-agent', 'clean'
		from generate_series(1, 20) g;
		analyze writing_check_events`); err != nil {
		t.Fatalf("seed the table: %v", err)
	}
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for what, plan := range map[string]string{
		"its plan for the sweep's retention": planOf(t, tx, sweepUncountedRows, fmt.Sprintf("%d seconds", int64(WritingCheckRetention/time.Second))),
		"its generic plan":                   genericPlanOf(t, tx, sweepUncountedRows),
	} {
		if !strings.Contains(plan, "writing_check_events_uncounted_idx") {
			t.Errorf("the delete of the rows the counts do not read, %s, reads no writing_check_events_uncounted_idx:\n%s", what, plan)
		}
	}
	if plan := planOf(t, tx, sweepPastCountedRetention, fmt.Sprintf("%d seconds", int64(WritingCheckCountedRetention/time.Second))); !strings.Contains(plan, "writing_check_events_created_at_idx") {
		t.Errorf("the delete past the counted retention reads no writing_check_events_created_at_idx:\n%s", plan)
	}
	day := time.Now().UTC().AddDate(0, 0, -100).Truncate(24 * time.Hour)
	if plan := planOf(t, tx, countWritingCheckRows, day, day, day, day.AddDate(0, 0, 1)); !strings.Contains(plan, "writing_check_events_write_id_idx") {
		t.Errorf("the counts of one day read no writing_check_events_write_id_idx:\n%s", plan)
	}
}
