package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A check_wake row says what woke its look by its mark, never by when it comes due. A row an
// earlier release queued carries no mark, and its reason alone says whether it is the retry of a
// look nobody reported, as that release's checking role read it: the outbox moves a row's next_at
// to the backoff of a failed attempt, so the row keeps no time it was named for. A row this release
// queued carries its mark, so the checking role's own wake stays its own whatever words its summary
// opens with. INFRA-3686's R1: the daemon's words of a retry are a field (`retry_of`), and the
// role's words a block by the role at the time of its report.
func TestACheckWakeRowIsToldByItsMarkNotByWhenItComesDue(t *testing.T) {
	t.Run("an earlier release's retry that a failed attempt backed off past a late report's wake", func(t *testing.T) {
		ctx := context.Background()
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		root := checkRoot()
		putOutboxIssue(t, pool, records, root)

		// Look 1 opened at the merge ten hours ago, and an earlier release ended it at its bound with
		// no report, queuing its retry a look bound out with no mark. After the upgrade the tester's
		// pending report of look 1 landed late, five minutes after that end, and named a wake an hour
		// after the retry's: the check holds that report's time and that wake.
		opened := time.Now().Add(-10 * time.Hour).UTC().Truncate(time.Microsecond)
		ended := opened.Add(workflow.CheckLookBound)
		retryDue := ended.Add(workflow.CheckLookBound)
		reported := ended.Add(5 * time.Minute)
		wake := retryDue.Add(time.Hour)
		check := openCheck(root, "a1b2c3d4")
		check.OpenedAt, check.BoundAt, check.LookWakeAt, check.LastWakeAt = opened, opened.Add(336*time.Hour), &opened, &opened
		check.Role, check.LookOpen, check.ReportedAt, check.WakeAt = claim.RoleTester, false, &reported, &wake
		putProductionCheck(t, pool, records, check)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, root.Key,
			record.CheckWakeDue{Look: 1, OpenedAt: opened, Reason: "the retry of a look nobody reported: look bound"}, retryDue))

		// The row first runs ten minutes after the report's wake_at, and that attempt fails: the
		// outbox's retry counts it and moves the row's next_at to its backoff, past that wake_at.
		failedAt := wake.Add(10 * time.Minute)
		failing := &outbox{pool: pool, dispatchProject: "LEGION", records: records, log: quietLogger(), now: func() time.Time { return failedAt },
			handlers: []intake.Handler{&outboxFactHandler{err: errors.New("the fact's transaction was cut off")}}}
		if err := failing.RunOnce(ctx); err != nil {
			t.Fatalf("the earlier release's retry row's first run: %v", err)
		}
		row := claimDueOutboxRow(t, pool, records, time.Now())
		if row.Kind != record.OutboxKindCheckWake || row.Attempts != 1 || !row.NextAt.After(wake) {
			t.Fatalf("the row after its failed attempt is a %s row with %d attempts, due %s; want the check_wake row's one failed attempt, due past the report's wake_at %s: this case proves nothing",
				row.Kind, row.Attempts, row.NextAt, wake)
		}

		engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour, ProductionCheckRole: claim.RoleTester}, quietLogger())
		runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{engine}, log: quietLogger(), now: time.Now}
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("the earlier release's retry row's second run: %v", err)
		}
		task := checkLookTaskOf(t, pool, root.Key)
		fields := taskFieldsOf(task)
		if fields["woken_by"] != "retry" || fields["retry_of"] != "look bound" || strings.Contains(task, "<summary") {
			t.Errorf("the look an earlier release's retry row started is woken_by %q with retry_of %q (a summary block: %t), want retry, naming look bound as why, with no summary: "+
				"the row is that release's retry, due again at %s after its failed attempt, past the late report's wake_at %s",
				fields["woken_by"], fields["retry_of"], strings.Contains(task, "<summary"), row.NextAt.Format(time.RFC3339), wake.Format(time.RFC3339))
		}
	})

	t.Run("this release's own wake whose summary is word for word an earlier release's retry", func(t *testing.T) {
		ctx := context.Background()
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		root := checkRoot()
		putOutboxIssue(t, pool, records, root)
		check := openCheck(root, "a1b2c3d4")
		check.Role = claim.RoleTester
		putProductionCheck(t, pool, records, check)

		// The tester reports look 1 pending, naming its wake three hours on, with a summary that is
		// word for word the sentence an earlier release wrote on the wake of a retry.
		now := time.Now().UTC().Truncate(time.Microsecond)
		engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour, ProductionCheckRole: claim.RoleTester,
			Clock: func() time.Time { return now }}, quietLogger())
		reported, wake := now, now.Add(3*time.Hour)
		const summary = "the retry of a look nobody reported: look bound"
		result, err := intake.ApplyFact(ctx, pool, "test", "look-1-pending",
			intake.CheckReport{Issue: root.Key, Look: 1, Verdict: "pending", Summary: summary, WakeAt: &wake}, engine)
		if err != nil || result.Refusal != nil {
			t.Fatalf("the tester's pending report = %+v, %v; want it recorded", result.Refusal, err)
		}
		row := checkWakeRowOf(t, pool, root.Key)
		if !row.NextAt.Equal(wake) {
			t.Fatalf("the check_wake row the report queued is due %s, want the %s it named", row.NextAt, wake)
		}

		now = wake
		runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{engine}, log: quietLogger(), now: func() time.Time { return now }}
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("the report's check_wake row: %v", err)
		}
		task := checkLookTaskOf(t, pool, root.Key)
		fields := taskFieldsOf(task)
		block := "\n<summary by=\"" + string(claim.RoleTester) + "\" at=\"" + reported.Format(time.RFC3339Nano) + "\">\n" + summary + "\n</summary>"
		if fields["woken_by"] != "named" || fields["retry_of"] != "" || !strings.Contains(task, block) {
			t.Errorf("the look the tester's own wake started is woken_by %q with retry_of %q, want named, with no retry_of and the summary %q in a block by the tester at %s",
				fields["woken_by"], fields["retry_of"], summary, reported.Format(time.RFC3339Nano))
		}
	})
}

// checkWakeRowOf is issue's one check_wake row as the outbox holds it.
func checkWakeRowOf(t *testing.T, pool *store.Pool, issue string) record.OutboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, payload, attempts, next_at from outbox where issue = $1 and kind = $2`,
		issue, string(record.OutboxKindCheckWake))
	if err != nil {
		t.Fatalf("read the check_wake rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var found []record.OutboxRow
	for rows.Next() {
		row := record.OutboxRow{Kind: record.OutboxKindCheckWake, Issue: issue}
		var payload []byte
		if err := rows.Scan(&row.ID, &payload, &row.Attempts, &row.NextAt); err != nil {
			t.Fatalf("scan a check_wake row of %s: %v", issue, err)
		}
		row.Payload = payload
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the check_wake rows of %s: %v", issue, err)
	}
	if len(found) != 1 {
		t.Fatalf("check_wake rows of %s = %+v, want one", issue, found)
	}
	return found[0]
}

// checkLookTaskOf is the task of issue's one look start, which hands look 2 to the tester.
func checkLookTaskOf(t *testing.T, pool *store.Pool, issue string) string {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(context.Background(), `select payload from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`, issue).Scan(&payload); err != nil {
		t.Fatalf("read the start row of the look the check_wake row started: %v", err)
	}
	var start record.SuperviseRequest
	if err := json.Unmarshal(payload, &start); err != nil {
		t.Fatalf("decode the start row of the look the check_wake row started: %v", err)
	}
	if start.CheckLook != 2 || start.Role != claim.RoleTester {
		t.Fatalf("the start row = %+v, want look 2 handed to the tester", start)
	}
	t.Logf("the look's task:\n%s", start.Task)
	return start.Task
}

// taskFieldsOf is each `key: value` line of task by its key.
func taskFieldsOf(task string) map[string]string {
	fields := map[string]string{}
	for line := range strings.SplitSeq(task, "\n") {
		if key, value, ok := strings.Cut(line, ": "); ok {
			fields[key] = value
		}
	}
	return fields
}
