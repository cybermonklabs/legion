package daemon

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A pause's resume wakes every lane at once (projectPause.Resumed), so two delivery rows of one
// released role can take their wakes at the same moment. One of them takes the wake, as that
// pause's held wake. The other one's take began before the first one's commit, so its own read
// found no open resume, and the first one's resume then refused it. That is the order
// logRefusedByOpenResume calls normal for a pause's held wake ("it comes here only between the
// earlier row's take and its resume. Nothing is wrong in any of these, and the line says so at
// Info"): the refused row says so at Info, naming the pause, writes no Warn that the resume stands
// open with no process behind it, and resumes the role.
//
// The row that takes the wake is this test's own transaction, which holds the claim's row and the
// wake's uncommitted resume until the notice's take waits on it, so the notice's take reads
// before that commit whatever it waits on: the claim's row, or the index that keeps one open
// resume a role (pod_resumes_one_open_per_role).
func TestANoticeOvertakenByAPauseHeldWakeDoesNotWarnOfAStuckResume(t *testing.T) {
	t.Parallel()
	const stuck = `"msg":"wake: a released pod's resume stands open with no process behind it; this wake resumes it"`
	const heldWake = `"msg":"wake: a released role's open resume is its pause's held wake; this wake resumes the role through the wake pacing"`
	r := newPauseRig(t, claim.RoleArchitect)
	// The record holds the architect as an idle release left it, as the machine below does.
	if err := r.st.PutClaim(r.ctx, r.claim(claim.RoleArchitect, supervise.StateSuspended)); err != nil {
		t.Fatalf("record the architect as released: %v", err)
	}
	sup, _ := r.supervisor()
	machine := releasedClaim(t, sup, claim.RoleArchitect)
	architect := machine.Claim().Token
	held := r.pauseLegion()
	ended := r.resume()
	if err := machine.Handle(r.ctx, supervise.ResumePause{Claim: architect, Pause: ended.ID}); err != nil {
		t.Fatalf("the resume's event: %v", err)
	}
	if got := machine.Claim(); !got.Released() {
		t.Fatalf("the architect is %s by %s after the resume's event, want it still released", got.State, got.SuspendedBy)
	}

	logs := &testlog.Buffer{}
	runner := &outbox{pool: r.pool, records: record.NewStore(), supervisor: sup, trees: r.st, project: "legion",
		dispatchProject: "LEGION", notices: &outboxPublisher{}, log: slog.New(slog.NewJSONHandler(logs, nil)), now: time.Now}
	notice := leasedOutboxRow(t, r.pool, runner.records, mustOutboxRow(t, pauseIssue,
		record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}, time.Now()))

	// The other row's take, held open: the claim's row locked, and the pause's held wake taken
	// and not yet committed.
	ctx := testwait.Context(t)
	other, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the other row's take: %v", err)
	}
	defer func() { _ = other.Rollback(r.ctx) }()
	var holder int32
	if err := other.QueryRow(ctx, `select pg_backend_pid() from claims where token = $1 for update`, string(architect)).Scan(&holder); err != nil {
		t.Fatalf("lock the architect's claim for the other row's take: %v", err)
	}
	podRecords, err := r.st.PodRecordsOf("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	if took, err := podRecords.TakePodWake(ctx, other, pauseIssue, claim.RoleArchitect, pods.WakeDelivery, time.Now(), held.ID); err != nil || !took {
		t.Fatalf("the other row's take of the pause's held wake = %t, %v; want it taken", took, err)
	}

	done := make(chan error, 1)
	go func() { done <- runner.execute(ctx, notice) }()
	testwait.Eventually(t, "the notice's take to wait on the other row's take", func() bool {
		var waiting bool
		err := r.pool.QueryRow(ctx, `select exists (select 1 from pg_stat_activity
			where $1 = any(pg_blocking_pids(pid)))`, holder).Scan(&waiting)
		return err == nil && waiting
	})
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit the other row's take: %v", err)
	}
	if err := testwait.Receive(t, "the overtaken notice's result", done); !errors.Is(err, errNoticeWaits) {
		t.Fatalf("the overtaken notice = %v, want it waiting for the architect it woke", err)
	}

	if lines := loggedLines(t, logs.String(), stuck); len(lines) != 0 {
		t.Errorf("the notice the pause's held wake overtook logged %v, want no Warn of a resume no process came behind: the other row took that wake a moment before", lines)
	}
	lines := loggedLines(t, logs.String(), heldWake)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || int64(lines[0]["pause"].(float64)) != held.ID {
		t.Errorf("the held wake's lines = %v, want one at INFO naming pause %d; log:\n%s", lines, held.ID, logs.String())
	}
	if got := machine.Claim(); got.Released() {
		t.Errorf("the architect is %s by %s after the notice's wake, want it resumed", got.State, got.SuspendedBy)
	}
}
