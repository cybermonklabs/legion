package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Main's endLook (workflow/check.go at d46ec4e0) queues the retry of a look nobody reported as a
// check_wake row whose reason is "the retry of a look nobody reported: "+cause, and main's tester
// prompt (go/tester.md at d46ec4e0) told such a look apart from the tester's own wake by that
// reason: "for a look your own `wake_at` started, the reason being the summary you sent, or for
// the retry of a look nobody reported, the reason being that retry's sentence".
//
// The change marks every retry it queues and reads a row an earlier release queued, which has no
// mark, by its own rule, which record.ProductionCheck states: "An unmarked check_wake row whose
// reason is an earlier release's sentence of a retry is read as that retry only where it is not
// the wake of the report ReportedAt times: where ReportedAt is nil, where WakeAt is, or where the
// row comes due before WakeAt", and docs/kubernetes.md: "Such a report's own row comes due at the
// check's `wake_at`, so such a row that comes due before it, or on a check with no `wake_at`, is
// the earlier release's retry of the look that the report came late for." The code
// (workflow.queuedRetry) reads the moment the row's fact is applied, not the moment the row came
// due, so the earlier release's retry row that came due before the late report's wake_at but ran
// after it (the outbox holds a check_wake row while the project is paused, and runs no row while
// the daemon is down) is told as the tester's own wake: woken_by: named, with the daemon's retry
// sentence in a summary block by the tester at the late report's time. The input carries the
// fact: the outbox row the daemon runs holds its own due time (record.OutboxRow.NextAt), an hour
// before the check's wake_at.
//
// INFRA-3686's R1: a text that "carries words another author wrote ... puts them in a block of
// their own after its facts, which names the author and the time where the text's own fields do
// not already name them". The daemon's words of a retry are no summary of the tester's, written at
// no report's time, and the look is no wake the tester named.
func TestAnEarlierReleasesRetryRowRunAfterTheLateReportsWakeStaysARetry(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)

	// Look 1 opened at the merge ten hours ago, and an earlier release ended it at its bound with
	// no report, queuing its retry a look bound out, with no mark. After the upgrade, the tester's
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

	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour, ProductionCheckRole: claim.RoleTester}, quietLogger())
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{engine}, log: quietLogger(), now: time.Now}

	// The earlier release's retry row, due an hour before the report's wake_at, runs now, five
	// hours after it came due.
	retry := mustOutboxRow(t, root.Key, record.CheckWakeDue{Look: 1, OpenedAt: opened, Reason: "the retry of a look nobody reported: look bound"}, retryDue)
	retry.ID = 81
	if !retry.NextAt.Before(wake) {
		t.Fatalf("the earlier release's retry row comes due at %s, want before the late report's wake_at %s: this case proves nothing", retry.NextAt, wake)
	}
	if err := runner.execute(ctx, retry); err != nil {
		t.Fatalf("run the earlier release's retry row: %v", err)
	}

	var payload []byte
	if err := pool.QueryRow(ctx, `select payload from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`, root.Key).Scan(&payload); err != nil {
		t.Fatalf("read the start row of the look the retry row started: %v", err)
	}
	var start record.SuperviseRequest
	if err := json.Unmarshal(payload, &start); err != nil {
		t.Fatalf("decode the start row of the look the retry row started: %v", err)
	}
	if start.CheckLook != 2 || start.Role != claim.RoleTester {
		t.Fatalf("the start row = %+v, want look 2 handed to the tester", start)
	}
	t.Logf("the look's task:\n%s", start.Task)
	fields := map[string]string{}
	for line := range strings.SplitSeq(start.Task, "\n") {
		if key, value, ok := strings.Cut(line, ": "); ok {
			fields[key] = value
		}
	}
	if fields["woken_by"] != "retry" || fields["retry_of"] != "look bound" {
		t.Errorf("the look the earlier release's retry row started is woken_by %q with retry_of %q, want retry, naming look bound as why: "+
			"the row came due at %s, before the late report's wake_at %s, and ran after it",
			fields["woken_by"], fields["retry_of"], retryDue.Format(time.RFC3339), wake.Format(time.RFC3339))
	}
	if strings.Contains(start.Task, "the retry of a look nobody reported") {
		t.Errorf("the look carries the daemon's retry sentence as words of the tester's (the late report at %s named a wake at %s, not this one)",
			reported.Format(time.RFC3339), wake.Format(time.RFC3339))
	}
}
