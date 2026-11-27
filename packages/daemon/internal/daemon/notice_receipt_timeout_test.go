package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// wantGivenUpLine is the line the daemon wrote when it gave a notice up after its last re-hold,
// which no count writes any more (dispatch://INFRA-4131/spec#b-r-never-given-up).
const wantGivenUpLine = "outbox notice not re-held again"

// wantArchitectEndedLine is the one line a notice row writes when it finishes undelivered because
// its architect will not hold its role again (noticeArchitectEnded).
const wantArchitectEndedLine = "outbox notice finished undelivered: its architect will not hold its role again"

// exceptionReport is an earlier listener's report (rehold) that its forward of h's notice to the
// architect's session was not confirmed (`receipt_timeout`) after it answered the publish 200:
// the publish went out under the key of row, with resends copies before it, and was forwarded to
// the architect's own session.
func (h *roleWaitHarness) exceptionReport(reason string, row int64, resends int) []byte {
	h.t.Helper()
	sent, ok := h.payload().(record.Notice)
	if !ok {
		h.t.Fatalf("the harness's payload is %T, want a notice", h.payload())
	}
	sent.Resends = resends
	return laneReport{fmt.Sprintf("evt-%s-%d-%d", reason, row, resends), reason, h.topic, noticeSummary(sent.Kind, "LEGION-208"), sent,
		record.OutboxKey(row), "ses-" + string(h.tc.role)}.envelope(h.t)
}

// rehold hands the runner one report of an earlier listener, as the exception subscription does.
func (h *roleWaitHarness) rehold(report []byte) {
	h.t.Helper()
	if err := h.runner.rehold(context.Background(), report); err != nil {
		h.t.Fatalf("rehold: %v", err)
	}
}

// noticeCopy is the one copy of notice row original the outbox holds (record.Notice.ResendOf):
// its id, the copies of the notice it counts (record.Notice.Resends), when it is due and what it
// waits for (record.OutboxWait.WaitFor).
type noticeCopy struct {
	id      int64
	resends int
	due     time.Time
	waitFor string
}

// copiesOfNotice is every copy of notice row original the outbox holds, oldest first.
func copiesOfNotice(t *testing.T, pool *store.Pool, original int64) []noticeCopy {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, coalesce((payload->>'resends')::int, 0), next_at, wait_for from outbox
		where kind = $1 and (payload->>'resend_of')::bigint = $2 order by id`, string(record.OutboxKindNotice), original)
	if err != nil {
		t.Fatalf("read the copies of notice row %d: %v", original, err)
	}
	defer rows.Close()
	var copies []noticeCopy
	for rows.Next() {
		var held noticeCopy
		if err := rows.Scan(&held.id, &held.resends, &held.due, &held.waitFor); err != nil {
			t.Fatalf("scan a copy of notice row %d: %v", original, err)
		}
		copies = append(copies, held)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the copies of notice row %d: %v", original, err)
	}
	return copies
}

// oneCopyOfNotice is the one copy of notice row original the outbox holds, failing the test where
// it holds none or more than one.
func oneCopyOfNotice(t *testing.T, pool *store.Pool, original int64) noticeCopy {
	t.Helper()
	copies := copiesOfNotice(t, pool, original)
	if len(copies) != 1 {
		t.Fatalf("the outbox holds %d copies of notice row %d (%+v), want one", len(copies), original, copies)
	}
	return copies[0]
}

// retireArchitect retires the harness's root architect, as its tree's close does: a root claim
// refuses an operator's stop (supervise.RequestStop).
func (h *roleWaitHarness) retireArchitect(machine *supervise.Machine) {
	h.t.Helper()
	if err := machine.Handle(context.Background(), supervise.RequestTreeClose{Claim: h.token}); err != nil {
		h.t.Fatalf("retire %s: %v", h.token, err)
	}
	machine.Wait()
	if state := machine.Claim().State; state != supervise.StateRetired {
		h.t.Fatalf("after its tree's close %s is %s, want it retired", h.token, state)
	}
}

// R3: a notice whose every publish its architect's session does not confirm (504 receipt_timeout)
// is not given up. Its row is kept and published again on the capped backoff of a row whose role
// said it was ready (dispatch://INFRA-4131/spec#b-r-capped), under its own id and its own key, past
// the fourth publish at which the daemon once gave a notice up: twelve publishes here, at 0, 1, 3,
// 7, 15, 31, 63, 127, 255 and 511 s and then every 300 s. No copy of it is queued, and no line says
// it was given up. Once its architect's session takes a publish, the row ends at that publish.
func TestNoticeReceiptTimeoutKeepsItsRowOnTheCappedWaits(t *testing.T) {
	t.Parallel()
	h := newRoleWaitHarness(t, roleWaitCases[0], refusalError("receipt_timeout"))
	h.claim(supervise.StateWorking, nil)
	rig := h.queue()

	rig.runTo(12)
	want := []time.Duration{0, 1 * time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 31 * time.Second,
		63 * time.Second, 127 * time.Second, 255 * time.Second, 511 * time.Second, 811 * time.Second, 1111 * time.Second}
	if got := publishOffsets(h.publisher.publishes, h.start); !slices.Equal(got, want) {
		t.Fatalf("the notice nobody confirmed published at %v, want %v", got, want)
	}
	for i, published := range h.publisher.publishes {
		if published.key != record.OutboxKey(rig.id) || published.topic != h.topic {
			t.Fatalf("publish %d went to %s under key %q, want %s under its own row's key %q",
				i, published.topic, published.key, h.topic, record.OutboxKey(rig.id))
		}
	}
	if got := queuedNotices(t, h.pool); len(got) != 1 || len(copiesOfNotice(t, h.pool, rig.id)) != 0 {
		t.Fatalf("the outbox holds the notices %v, want the row alone and no copy of it", got)
	}

	h.publisher.err = nil
	rig.endsAtItsNextRun()
	if got := h.publisher.count(); got != len(want)+1 {
		t.Fatalf("the notice published %d times in all, want %d: every one nobody confirmed and the one taken", got, len(want)+1)
	}
	logs := rig.logs.String()
	if strings.Contains(logs, wantGivenUpLine) || strings.Contains(logs, "outbox notice re-held") {
		t.Fatalf("the notice nobody confirmed was given up or copied:\n%s", logs)
	}
	if lines := loggedMessages(t, logs, "outbox notice published"); len(lines) != 1 || lines[0]["row"] != float64(rig.id) {
		t.Fatalf("the lines of the publish taken are %v, want one naming row %d", lines, rig.id)
	}
}

// R3: a notice nobody confirms ends when its architect's claim retires, undelivered and with no
// copy, as a notice to a role nobody will hold again ends (noticeArchitectEnded). A retirement
// between two publishes ends the row at its next run, with no publish; one that lands while the
// listener waits for the receipt of a publish ends the row at that publish's answer.
func TestNoticeReceiptTimeoutEndsWhenTheArchitectRetires(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// inPublish is whether the architect retires while the run's publish waits for its receipt.
		inPublish bool
	}{
		{name: "retired between its publishes"},
		{name: "retired while its publish waits for the receipt", inPublish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, roleWaitCases[0], refusalError("receipt_timeout"))
			machine := h.claim(supervise.StateWorking, nil)
			rig := h.queue()

			rig.runTo(6)
			before := h.publisher.count()
			wantPublishes := before
			if tc.inPublish {
				h.publisher.during = func() {
					h.publisher.during = nil
					h.retireArchitect(machine)
				}
				wantPublishes++
			} else {
				h.retireArchitect(machine)
			}
			rig.endsAtItsNextRun()
			if got := h.publisher.count(); got != wantPublishes {
				t.Fatalf("the notice published %d times in all, want %d", got, wantPublishes)
			}
			if got := queuedNotices(t, h.pool); len(got) != 0 {
				t.Fatalf("the outbox holds the notices %v once the architect retired, want none", got)
			}
			if lines := loggedMessages(t, rig.logs.String(), wantArchitectEndedLine); len(lines) != 1 || lines[0]["row"] != float64(rig.id) {
				t.Fatalf("the lines of the notice's end are %v, want one naming row %d:\n%s", lines, rig.id, rig.logs.String())
			}
		})
	}
}

// R3, for the role-lane exceptions of a listener before the confirmed publish route: a notice
// whose every forward an earlier listener reports unconfirmed after answering it 200 is copied
// without limit, each copy a row of its own, published under the key of the row it copies. The
// first copy goes at once, its architect having said it is ready (copyDueAtOnce); each later copy,
// whose report names the architect's own ready session, waits the capped backoff of the copies
// sent before it (record.RoleWaitBackoff of Resends), 1 s doubling to 5 minutes, and waits for its
// architect's ready (record.WaitForReady). Fourteen copies go, past the three a notice was once
// held to and on to the cap, and no line says the notice was given up.
func TestNoticeReceiptTimeoutExceptionCopiesHaveNoLimit(t *testing.T) {
	t.Parallel()
	h := newRoleWaitHarness(t, roleWaitCases[0], nil)
	h.claim(supervise.StateWorking, nil)
	logs := &syncBuffer{}
	h.runner.now, h.runner.log = h.clock.Now, debugLogTo(logs)
	// The row the copies copy: its publish was answered 200, so it finished before the report.
	const original = 7

	const copies = 14
	h.rehold(h.exceptionReport("receipt_timeout", original, 0))
	for resends := 1; resends <= copies; resends++ {
		reported := h.clock.Now()
		held := oneCopyOfNotice(t, h.pool, original)
		wantDue := reported.Add(record.RoleWaitBackoff(resends - 1))
		if resends == 1 {
			wantDue = reported
		}
		if held.resends != resends || !held.due.Equal(wantDue) || held.waitFor != record.WaitForReady(h.token) {
			t.Fatalf("copy %d is %+v, want copy %d due at %s (%s after the report), waiting for %q",
				resends, held, resends, wantDue, wantDue.Sub(reported), record.WaitForReady(h.token))
		}
		h.clock.advance(held.due.Sub(h.clock.Now()))
		if err := h.runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run copy %d: %v", resends, err)
		}
		if got := h.publisher.count(); got != resends {
			t.Fatalf("after copy %d the notice published %d times, want %d", resends, got, resends)
		}
		if sent := h.publisher.publishes[resends-1]; sent.key != record.OutboxKey(original) || sent.topic != h.topic {
			t.Fatalf("copy %d went to %s under key %q, want %s under the key of row %d", resends, sent.topic, sent.key, h.topic, original)
		}
		if got := queuedNotices(t, h.pool); len(got) != 0 {
			t.Fatalf("after its publish was taken copy %d left the notices %v, want none", resends, got)
		}
		h.rehold(h.exceptionReport("receipt_timeout", original, resends))
	}
	if held := oneCopyOfNotice(t, h.pool, original); held.resends != copies+1 || !held.due.Equal(h.clock.Now().Add(record.RoleWaitBackoffMost)) {
		t.Fatalf("after the report of copy %d the outbox holds %+v, want copy %d due at the cap, %s on", copies, held, copies+1, record.RoleWaitBackoffMost)
	}
	for _, line := range loggedLines(t, logs.String(), `"msg"`) {
		if line["level"] == slog.LevelWarn.String() || line["level"] == slog.LevelError.String() || line["msg"] == wantGivenUpLine {
			t.Fatalf("the copies wrote %v, want no line above Info and none giving the notice up:\n%s", line, logs.String())
		}
	}
}

// R3, for the role-lane exceptions: a copy an earlier listener's report queued ends when its
// architect's claim retires, as every notice to a role nobody will hold again ends: undelivered,
// with no publish, one line, and no copy after it.
func TestNoticeReceiptTimeoutExceptionCopyEndsWhenTheArchitectRetires(t *testing.T) {
	t.Parallel()
	h := newRoleWaitHarness(t, roleWaitCases[0], nil)
	machine := h.claim(supervise.StateWorking, nil)
	logs := &syncBuffer{}
	h.runner.now, h.runner.log = h.clock.Now, debugLogTo(logs)
	const original = 7

	h.rehold(h.exceptionReport("receipt_timeout", original, 0))
	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the first copy: %v", err)
	}
	h.rehold(h.exceptionReport("receipt_timeout", original, 1))
	held := oneCopyOfNotice(t, h.pool, original)
	h.retireArchitect(machine)

	h.clock.advance(held.due.Sub(h.clock.Now()))
	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the second copy: %v", err)
	}
	if got := h.publisher.count(); got != 1 {
		t.Fatalf("the copies published %d times, want once: the first copy, before the architect retired", got)
	}
	if got := queuedNotices(t, h.pool); len(got) != 0 {
		t.Fatalf("the outbox holds the notices %v once the architect retired, want none", got)
	}
	if lines := loggedMessages(t, logs.String(), wantArchitectEndedLine); len(lines) != 1 || lines[0]["row"] != float64(held.id) {
		t.Fatalf("the lines of the copy's end are %v, want one naming row %d:\n%s", lines, held.id, logs.String())
	}
}

// R3 and R1, for the role-lane exceptions: a copy waits for its architect's ready alone
// (record.WaitForReady), so the claim's next ready makes it due at once while a turn start of the
// ready architect, a change of its claim, does not cut the copy's wait short: a session that took
// nothing at its last copy is not sent the next one at each of its turns.
func TestNoticeReceiptTimeoutExceptionCopyIsDueAtTheArchitectsReady(t *testing.T) {
	t.Parallel()
	tc := roleWaitCases[0]
	h := newRoleWaitHarness(t, tc, nil)
	w := h.readyHooks(true)
	machine := h.claim(supervise.StateIdle, nil)
	h.release(w)
	h.runner.now = h.clock.Now
	const original = 7

	// The report of the sixth copy, which failed on the architect's own ready session: the
	// seventh waits the sixth step's wait, 64 s.
	h.rehold(h.exceptionReport("receipt_timeout", original, 6))
	held := oneCopyOfNotice(t, h.pool, original)
	if want := h.start.Add(record.RoleWaitBackoff(6)); !held.due.Equal(want) || held.waitFor != record.WaitForReady(h.token) {
		t.Fatalf("the copy is %+v, want it due at %s, waiting for %q", held, want, record.WaitForReady(h.token))
	}

	h.clock.advance(5 * time.Second)
	for _, ev := range []supervise.Event{supervise.StreamTurnStart{Claim: h.token}, supervise.StreamTurnEnd{Claim: h.token}} {
		if err := machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	machine.Wait()
	h.release(w)
	if due, _, _, _ := rowWait(t, h.pool, held.id); !due.Equal(held.due) {
		t.Fatalf("after the architect's turn the copy is due at %s, want still at its wait (%s)", due, held.due)
	}

	relaunchedReady(t, machine, tc)
	readyAt := h.ready(w, machine)
	h.release(w)
	if due, step, _, _ := rowWait(t, h.pool, held.id); !due.Equal(readyAt) || step != 0 {
		t.Fatalf("after the architect's ready the copy is due at %s at step %d, want at the ready (%s) at step 0", due, step, readyAt)
	}
	if err := h.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the copy: %v", err)
	}
	if got := publishOffsets(h.publisher.publishes, h.start); !slices.Equal(got, []time.Duration{readyAt.Sub(h.start)}) ||
		h.publisher.publishes[0].key != record.OutboxKey(original) {
		t.Fatalf("the copy published at %v (%+v), want once, at the ready (%s), under the key of row %d",
			got, h.publisher.publishes, readyAt.Sub(h.start), original)
	}
}

// R3: a role-lane exception whose key names a row the outbox still holds queues no copy. That row
// runs again by itself: here its own publish was answered receipt_timeout by a listener of the
// confirmed route, while an earlier listener of the same deployment reported the forward it made
// of that publish. Each report says so in one line, and the row ends at the publish its architect
// takes. Once the row has finished, a report of its last publish's forward is copied as ever.
func TestNoticeReceiptTimeoutExceptionForAKeptRowQueuesNoCopy(t *testing.T) {
	t.Parallel()
	h := newRoleWaitHarness(t, roleWaitCases[0], refusalError("receipt_timeout"))
	h.claim(supervise.StateWorking, nil)
	rig := h.queue()

	rig.run()
	reasons := []string{"receipt_timeout", "delivery_failed", "no_holder"}
	for _, reason := range reasons {
		h.rehold(h.exceptionReport(reason, rig.id, 0))
	}
	if got := queuedNotices(t, h.pool); len(got) != 1 || len(copiesOfNotice(t, h.pool, rig.id)) != 0 {
		t.Fatalf("after the reports of a row the outbox still holds, the notices are %v, want the row alone", got)
	}
	if lines := loggedMessages(t, rig.logs.String(), "outbox notice not re-held: its row is still in the outbox"); len(lines) != len(reasons) {
		t.Fatalf("the reports of the kept row wrote %d lines saying it runs again by itself, want %d:\n%s", len(lines), len(reasons), rig.logs.String())
	}

	h.publisher.err = nil
	rig.endsAtItsNextRun()
	for i, published := range h.publisher.publishes {
		if published.key != record.OutboxKey(rig.id) {
			t.Fatalf("publish %d went out under key %q, want the row's own, %q", i, published.key, record.OutboxKey(rig.id))
		}
	}
	if got := h.publisher.count(); got != 2 {
		t.Fatalf("the row published %d times, want twice: the publish nobody confirmed and the one taken", got)
	}

	h.rehold(h.exceptionReport("receipt_timeout", rig.id, 0))
	if held := oneCopyOfNotice(t, h.pool, rig.id); held.resends != 1 {
		t.Fatalf("after the report of the finished row's publish the outbox holds %+v, want its first copy", held)
	}
}

// R3: a catch-up whose publishes its architect's session does not confirm keeps its claims on the
// decisions it names, on its own row, through every publish nobody confirmed: no copy takes them
// over and nothing hands them to another catch-up. The publish its architect takes tells them, once:
// the telling is marked under the row's key and the decisions are forgotten (toldKept,
// settleTold), and no other catch-up names them.
func TestNoticeReceiptTimeoutCatchUpKeepsItsClaimsAndTellsThemOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := roleWaitClock()
	start := clock.Now()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Clock = clock
	architect := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
	roleWaitClaim(t, sup, supervise.Claim{Token: architect, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateWorking, Session: "ses_architect", SessionFile: t.TempDir() + "/session.jsonl",
		LastReady: start.Add(-10 * time.Minute), LastTurnStart: start.Add(-5 * time.Minute)})
	ask := "dispatch://LEGION-1/ask/ask-1"
	keepDecision(t, pool, records, "LEGION-1", ask, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	keptCatchUp(t, pool, records, "LEGION-1", 1, start)
	held := catchUpRows(t, pool, "LEGION-1")
	if len(held) != 1 {
		t.Fatalf("%d catch-ups of LEGION-1 queued, want one", len(held))
	}
	id, key := held[0].ID, record.OutboxKey(held[0].ID)
	publisher := &outboxPublisher{err: confirmedAnswer(notify.ReasonReceiptTimeout, "ses_architect")}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
	rig := newRoleWaitRig(t, runner, pool, clock, id)
	told := func() int {
		t.Helper()
		var marks int
		if err := pool.QueryRow(ctx, "select count(*) from processed_events where source = $1 and event_id = $2",
			record.KeptToldSource, key).Scan(&marks); err != nil {
			t.Fatalf("count the tellings under %s: %v", key, err)
		}
		return marks
	}

	rig.runTo(6)
	if rows := catchUpRows(t, pool, "LEGION-1"); len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("after six publishes nobody confirmed the catch-ups of LEGION-1 are %v, want row %d alone", rows, id)
	}
	if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != 1 || left[0].ClaimedBy != id || told() != 0 {
		t.Fatalf("after six publishes nobody confirmed the kept decisions are %s with %d tellings, want %s still claimed by row %d and told to nobody",
			keptRefs(left), told(), ask, id)
	}

	publisher.refuseWith(nil)
	rig.endsAtItsNextRun()
	published := publisher.publishes()
	if len(published) != 7 {
		t.Fatalf("the catch-up published %d times, want 7: six nobody confirmed and the one taken", len(published))
	}
	for i, sent := range published {
		notice, ok := sent.payload.(record.Notice)
		if sent.key != key || !ok || notice.CatchUp == nil || !strings.Contains(notice.CatchUp.Decided, ask) {
			t.Fatalf("publish %d went out under key %q naming %+v, want the row's own key %q, naming %s", i, sent.key, sent.payload, key, ask)
		}
	}
	if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != 0 || told() != 1 {
		t.Fatalf("after the publish taken the kept decisions are %s with %d tellings, want none kept and one telling", keptRefs(left), told())
	}
	if rows := catchUpRows(t, pool, "LEGION-1"); len(rows) != 0 {
		t.Fatalf("after the publish taken the catch-ups of LEGION-1 are %v, want none naming %s again", rows, ask)
	}
}
