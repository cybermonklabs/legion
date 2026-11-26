package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A fix beside a review, through the real surface: the real Dispatch this checkout builds, the
// whole daemon over the fake runtime, GitHub's events on the notification stream, the listener's
// settlements, Dispatch's result, steps and plans routes, and the daemon's API as each claim
// (fix_rig_test.go). Each wait waits for a fact that comes whatever the verdict after it (a
// barrier: a processed_events row, the record's head, a held check state, a finished outbox row,
// Dispatch's owed rows), and the verdict is read at once after it.

// fixBesideIssues are the issues of TestFixBesideReviewThroughDispatch, each on a pull request of
// its own: the walk of a fix beside a review, the control with no fix flag, the control of a
// review whose footer names another head, the fix that takes the merge forward its implementer
// owes, and that merge's control, which no fix takes. No two titles share a word that counts
// (walkTitles says why).
var fixBesideIssues = []fixRigIssue{
	{title: "Golf ledger balances accounts", number: 7, verification: fixRigVerification},
	{title: "Hotel indexer skips dotfiles", number: 8, verification: fixRigVerification},
	{title: "India mailer batches sends", number: 9, verification: fixRigVerification},
	{title: "Juliet cropper keeps ratios", number: 10, verification: fixRigVerification},
	{title: "Kilo tracer samples spans", number: 11, verification: fixRigVerification},
}

// fixRigLogged is one write of a row of the daemon's outbox, as the rig's log of the outbox kept
// it (logOutbox): the row's id and its payload at that write.
type fixRigLogged struct {
	id      int64
	payload []byte
}

// logOutbox makes the daemon's database keep a copy of each row written to the outbox, and of each
// later write of its payload, in a table of the rig's own (fix_rig_outbox_log), so a check reads a
// row the outbox ran and finished before any wait could see it. The trigger only copies: the
// daemon's own writes stand as they were.
func (r *fixRig) logOutbox() {
	r.t.Helper()
	if _, err := r.st.Pool().Exec(context.Background(), `
create table fix_rig_outbox_log (seq bigserial primary key, id bigint not null, issue text not null, kind text not null, payload jsonb not null);
create function fix_rig_log_outbox() returns trigger language plpgsql as $$
begin
	insert into fix_rig_outbox_log (id, issue, kind, payload) values (new.id, new.issue, new.kind, new.payload);
	return new;
end $$;
create trigger fix_rig_log_outbox after insert or update of payload on outbox for each row execute function fix_rig_log_outbox();`); err != nil {
		r.t.Fatalf("make the rig's log of the outbox: %v", err)
	}
}

// logged is each row of kind the outbox was given for the issue, by id, with its latest payload.
func (e *fixEnd) logged(kind record.OutboxKind) []fixRigLogged {
	r := e.r
	r.t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), `select distinct on (id) id, payload::text from fix_rig_outbox_log
		where issue = $1 and kind = $2 order by id, seq desc`, e.key, string(kind))
	if err != nil {
		r.t.Fatalf("read the rig's log of %s's %s rows: %v", e.key, kind, err)
	}
	var logged []fixRigLogged
	var id int64
	var payload string
	if _, err := pgx.ForEachRow(rows, []any{&id, &payload}, func() error {
		logged = append(logged, fixRigLogged{id: id, payload: []byte(payload)})
		return nil
	}); err != nil {
		r.t.Fatalf("read the rig's log of %s's %s rows: %v", e.key, kind, err)
	}
	return logged
}

// fixRigStart is one supervise row the outbox was given: its id and its request.
type fixRigStart struct {
	id      int64
	request record.SuperviseRequest
}

// starts is each start the outbox was given for the issue for role, by id.
func (e *fixEnd) starts(role claim.Role) []fixRigStart {
	e.r.t.Helper()
	var starts []fixRigStart
	for _, row := range e.logged(record.OutboxKindSupervise) {
		var request record.SuperviseRequest
		if err := json.Unmarshal(row.payload, &request); err != nil {
			e.r.t.Fatalf("decode %s's supervise row %d: %v", e.key, row.id, err)
		}
		if string(request.Op) == "start" && request.Role == role {
			starts = append(starts, fixRigStart{id: row.id, request: request})
		}
	}
	return starts
}

// standingRows is how many of ids the outbox still holds.
func (r *fixRig) standingRows(ids []int64) int {
	r.t.Helper()
	var count int
	if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from outbox where id = any($1)`, ids).Scan(&count); err != nil {
		r.t.Fatalf("read whether the outbox still holds rows %v: %v", ids, err)
	}
	return count
}

// publishedRow is whether the listener's stand-in took a publish on topic under the key of the
// outbox row id, the one key the outbox publishes a row under (record.OutboxKey).
func (r *fixRig) publishedRow(topic string, id int64) bool {
	for _, published := range r.notices.acceptedOn(topic) {
		if published.DedupeKey == record.OutboxKey(id) {
			return true
		}
	}
	return false
}

// dispatchSeq is the newest sequence of the issue's event log in Dispatch's own database.
func (e *fixEnd) dispatchSeq() int64 {
	r := e.r
	r.t.Helper()
	var seq int64
	if err := r.dispatchDB.QueryRow(context.Background(), `select coalesce(max(seq), 0) from events where issue_key = $1`, e.key).Scan(&seq); err != nil {
		r.t.Fatalf("read the newest event of %s in Dispatch: %v", e.key, err)
	}
	return seq
}

// review is the review App's review of state of the issue's pull request at head, its footer
// naming the reviewer's own session, footerHead and entry, as a review the reviewer's session
// submits; it returns once the daemon's processed_events row of it stands, which is written
// whether the review is taken or refused.
func (e *fixEnd) review(state, head, footerHead string, entry int64) {
	r := e.at()
	r.t.Helper()
	reviewer := e.worker(claim.RoleReviewer)
	r.awaitProcessed(r.publishReview(state, head, legionEntryFooter(reviewer.session, footerHead, entry)))
}

// decision is the review decision the reviewer's phases row holds now.
func (e *fixEnd) decision() record.ReviewDecision {
	r := e.at()
	r.t.Helper()
	if decision := r.phaseOf(claim.RoleReviewer).Decision; decision != nil {
		return *decision
	}
	return record.ReviewDecision{}
}

// completes is role's completion at commit through the daemon's handoff route, with its handoff
// record written as the plugin writes it; it fails unless the route answers 200, and ends the
// turn the completion was in.
func (e *fixEnd) completes(role claim.Role, word, commit string) {
	r := e.at()
	r.t.Helper()
	c := e.worker(role)
	handoff, version := r.writeHandoff(c.session, word)
	if status, body := r.complete(c, api.HandoffCompleteRequest{Summary: word + " done", Commit: commit, HandoffID: handoff, HandoffVersion: version}); status != http.StatusOK {
		r.t.Fatalf("%s's %s's completion at %s = %d %s, want 200", e.key, role, commit, status, body)
	}
	c.endTurn()
}

// works is role's agent starting the turn of the task prompt carried, its model answering first.
func (e *fixEnd) works(role claim.Role, prompt shimwire.Prompt) {
	e.r.t.Helper()
	c := e.worker(role)
	c.answer()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
}

// reviewBesideCI is the setup of each issue up to its review while unit runs at X: the plan and the
// gate, the implementer completing at X once the pull request is open there, the tester recording
// k1 and completing at X with no run of unit concluded, and the issue in reviewing, which needs
// the testing step that leaves each check a CI job runs to the fix. The reviewer then takes its
// task, whose last sentence names its entry, and its turn stays open. It answers that entry.
func (e *fixEnd) reviewBesideCI() int64 {
	e.r.t.Helper()
	e.planAndGate(phase.Implementing, "implementer", "tester", "reviewer")
	e.implementsAtX()
	e.testerRecordsK1()
	e.testerCompletes()
	r := e.at()
	r.waitPhase(phase.Reviewing)
	task := e.worker(claim.RoleReviewer).take()
	return fixRigEntry(r.t, task.Message)
}

// fixStartRows is the id of each start of fix 1 the outbox was given for the issue.
func (e *fixEnd) fixStartRows() []int64 {
	var ids []int64
	for _, start := range e.starts(claim.RoleImplementer) {
		if start.request.Fix == 1 {
			ids = append(ids, start.id)
		}
	}
	return ids
}

// fixStartsToItsStartRow is a fix's start up to its start row, once unit failed at X and the
// daemon holds k-ci's failure there: the fix row stands for k-ci at X, the read of its job is
// finished, the fix runs with the job and its page, and its start row is finished by the start
// executor. It answers the ids of the fix's start rows.
func (e *fixEnd) fixStartsToItsStartRow(t *testing.T, run githubtest.CheckRun) []int64 {
	t.Helper()
	if fixes := e.fixes(); len(fixes) != 1 || fixes[0].fix != 1 || fixes[0].check != scratchCICheck || fixes[0].head != e.x {
		t.Fatalf("the fixes of %s once the daemon holds k-ci's failure at X = %+v, want fix 1 for k-ci at %s", e.key, fixes, e.x)
	}
	e.awaitRowsFinished(record.OutboxKindFixJobRead)
	if fixes := e.fixes(); len(fixes) != 1 || fixes[0].state != string(record.FixRunning) || fixes[0].jobName != scratchCIJob || fixes[0].jobURL != run.HTMLURL {
		t.Fatalf("the fix of %s once its job's read finished = %+v, want it running with the job %s at %s", e.key, fixes, scratchCIJob, run.HTMLURL)
	}
	ids := e.fixStartRows()
	if len(ids) == 0 {
		t.Fatalf("the outbox of %s was given no start of fix 1 once the fix ran", e.key)
	}
	r := e.at()
	r.await(fmt.Sprintf("the start rows %v of %s's fix finished", ids, e.key), func() bool { return r.standingRows(ids) == 0 })
	return ids
}

// assertFixStartedTold fails unless the outbox gave role's step one ci_news row of purpose
// fix-started whose Assigned names fix 1, k-ci and X, and the listener's stand-in took that row on
// role's topic. It finds the row by its kind, its purpose and its receiver among the issue's other
// ci_news rows, once every ci_news row of the issue is finished.
func (e *fixEnd) assertFixStartedTold(t *testing.T, role claim.Role) {
	t.Helper()
	e.awaitRowsFinished(record.OutboxKindCINews)
	token := e.r.tokenOf(e.key, role)
	var told []int64
	for _, row := range e.logged(record.OutboxKindCINews) {
		var news record.CINews
		if err := json.Unmarshal(row.payload, &news); err != nil {
			t.Fatalf("decode %s's ci_news row %d: %v", e.key, row.id, err)
		}
		if news.Purpose != record.CINewsFixStarted || news.Role != string(token) {
			continue
		}
		if news.Assigned == nil || news.Assigned.Fix != 1 || news.Assigned.Check != scratchCICheck || news.Assigned.Head != e.x {
			t.Fatalf("%s's fix-started row %d to its %s assigns %+v, want fix 1 of %s at %s", e.key, row.id, role, news.Assigned, scratchCICheck, e.x)
		}
		told = append(told, row.id)
	}
	if len(told) != 1 {
		t.Fatalf("%s's fix-started rows to its %s = %v, want one: the step beside the fix is told of it", e.key, role, told)
	}
	if topic := notify.RoleTopicPrefix + string(token); !e.r.publishedRow(topic, told[0]) {
		t.Fatalf("the listener's stand-in took no publish of %s's fix-started row %d on %s", e.key, told[0], topic)
	}
}

// refuseRoles makes the listener's stand-in refuse each publish to the role topics of roles of the
// issue, as a stream outage refuses them (refused), or take them again (not refused). The outbox
// keeps a refused row and runs it again on its backoff.
func (e *fixEnd) refuseRoles(refused bool, roles ...claim.Role) {
	n := 0
	if refused {
		n = 1 << 30
	}
	for _, role := range roles {
		e.r.notices.refuseFirst(notify.RoleTopicPrefix+string(e.r.tokenOf(e.key, role)), n)
	}
}

// conflictsInTesting is the setup of an issue whose pull request opens at X conflicting with its
// base: the plan and the gate, the stand-in answering X dirty, the implementer taking its task, the
// pull request opening at X, and the implementer completing there; then the tester takes its task
// in testing, and its turn stays open. The daemon reads X conflicting at the opening and queues its
// second read of X for a minute later (conflict_confirm), which the test makes due at once
// (secondReadNow); the issue stands in testing then, and the base's rules the daemon cannot read
// (the stand-in answers 404), so it asks the implementer to merge the base forward.
func (e *fixEnd) conflictsInTesting() {
	e.r.t.Helper()
	e.planAndGate(phase.Implementing, "implementer", "tester", "reviewer")
	r := e.at()
	r.pulls.conflicts(r.number, e.x)
	e.implementsAtX()
	e.worker(claim.RoleTester).take()
}

// secondReadNow makes the daemon's second read of X due now, as though its first read had been
// made the daemon's wait earlier. The daemon reads X conflicting when the pull request opens at X:
// it records that first read (record.PullRequest.ConflictRead) and queues the second read
// (record.ConflictConfirm) due the wait after it (workflow's conflictConfirmWait, a minute), the
// time GitHub is given to compute mergeability, and only a read made that wait or more after the
// first confirms the conflict. Once the record holds the first read, the test moves the read's
// time, the time the row names it by and the row's due time back by the row's due time less the
// first read's time, which is that wait. It writes under the lock every apply of a fact takes
// (serializedTx), so no apply writes the pull request between this read of it and this write, and
// then wakes the row's lane, as the commit of a row the workflow writes does. The second read then
// runs at once, reads GitHub, and the workflow decides on its answer as it would have a minute
// later. next_at keeps microseconds, so the wait read back can fall short of the daemon's by less
// than one, and the second read, made after this write, comes far later than that after the first.
// A row already due, or a first read a later read confirmed or released, is left to the daemon.
func (e *fixEnd) secondReadNow() {
	r := e.at()
	r.t.Helper()
	r.await(e.key+"'s first conflicting read of X, or its conflict confirmed", func() bool {
		pr := r.pullRequest()
		return pr != nil && (pr.ConflictRead != nil && pr.ConflictRead.Head == e.x || pr.MergeForward != nil && pr.MergeForward.Head == e.x)
	})
	var id int64
	var moved time.Duration
	if err := serializedTx(context.Background(), r.st.Pool(), "make the second read due now", func(ctx context.Context, tx pgx.Tx) error {
		pr, err := r.records.PullRequest(ctx, tx, e.key)
		if err != nil || pr == nil || pr.ConflictRead == nil || pr.ConflictRead.Head != e.x {
			return err
		}
		first := *pr.ConflictRead
		rows, err := tx.Query(ctx, `select id, next_at, payload::text from outbox where issue = $1 and kind = $2`,
			e.key, string(record.OutboxKindConflictConfirm))
		if err != nil {
			return err
		}
		var read record.ConflictConfirm
		var due time.Time
		var rowID int64
		var payload string
		if _, err := pgx.ForEachRow(rows, []any{&rowID, &due, &payload}, func() error {
			decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: rowID, Kind: record.OutboxKindConflictConfirm, Payload: []byte(payload)})
			if err != nil {
				return err
			}
			if of := decoded.(record.ConflictConfirm); of.Head == first.Head && of.FirstReadAt.Equal(first.At) && due.After(time.Now()) {
				id, read, moved = rowID, of, due.Sub(of.FirstReadAt)
			}
			return nil
		}); err != nil {
			return err
		}
		if id == 0 {
			return nil
		}
		first.At = first.At.Add(-moved)
		encoded, err := json.Marshal(first)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update pull_requests set conflict_read = $2 where issue = $1`, e.key, encoded); err != nil {
			return err
		}
		read.FirstReadAt = first.At
		row, err := record.NewOutboxRow(e.key, read, due.Add(-moved))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update outbox set payload = $2, next_at = $3 where id = $1`, id, row.Payload, row.NextAt)
		return err
	}); err != nil {
		r.t.Fatalf("make %s's second read of X due now: %v", e.key, err)
	}
	if id == 0 {
		r.fact("second-read", "%s's second read of X was due or had run when the test came to it; the daemon runs it as it stands", e.key)
		return
	}
	r.fact("second-read", "%s's first read of X and its second read, row %d, moved %s back; the row is due now", e.key, id, moved)
	r.wake(e.key, record.OutboxKindConflictConfirm)
}

// awaitAsk makes the daemon's second read of X due now (secondReadNow), waits for the record's
// merge forward of X asked of the implementer, which the daemon writes at that read, and answers
// it.
func (e *fixEnd) awaitAsk() record.MergeForward {
	e.secondReadNow()
	r := e.at()
	r.t.Helper()
	var asked record.MergeForward
	r.await(e.key+"'s merge forward of X asked of the implementer", func() bool {
		pr := r.pullRequest()
		if pr == nil || pr.MergeForward == nil {
			return false
		}
		asked = *pr.MergeForward
		return asked.Head == e.x && asked.Asked() == claim.RoleImplementer
	})
	return asked
}

// mergeForward is the record's merge forward of the issue now, the zero value for none.
func (e *fixEnd) mergeForward() record.MergeForward {
	r := e.at()
	r.t.Helper()
	if pr := r.pullRequest(); pr != nil && pr.MergeForward != nil {
		return *pr.MergeForward
	}
	return record.MergeForward{}
}

// mergeMessages is each merge_forward row the outbox was given for the issue, by id.
func (e *fixEnd) mergeMessages() ([]int64, []record.MergeForwardMessage) {
	e.r.t.Helper()
	var ids []int64
	var messages []record.MergeForwardMessage
	for _, row := range e.logged(record.OutboxKindMergeForward) {
		var message record.MergeForwardMessage
		if err := json.Unmarshal(row.payload, &message); err != nil {
			e.r.t.Fatalf("decode %s's merge_forward row %d: %v", e.key, row.id, err)
		}
		ids, messages = append(ids, row.id), append(messages, message)
	}
	return ids, messages
}

// testerCompletesAt is the tester's completion at commit through the daemon's handoff route, with
// its handoff record written as the plugin writes it. It answers the route's status, the code of
// a refusal, and the handoff record's id.
func (e *fixEnd) testerCompletesAt(commit string) (int, string, string) {
	r := e.at()
	r.t.Helper()
	tester := e.worker(claim.RoleTester)
	handoff, version := r.writeHandoff(tester.session, "test")
	status, body := r.complete(tester, api.HandoffCompleteRequest{Summary: "k1 holds", Commit: commit, HandoffID: handoff, HandoffVersion: version})
	var refusal struct {
		Code string `json:"code"`
	}
	if status != http.StatusOK {
		if err := json.Unmarshal([]byte(body), &refusal); err != nil {
			r.t.Fatalf("decode the refusal of %s's tester's completion at %s (%d %s): %v", e.key, commit, status, body, err)
		}
	}
	return status, refusal.Code, handoff
}

// completionApplied is whether the daemon's processed_events row of the tester's completion naming
// handoff stands: the route's apply writes it in the transaction that applies the completion
// (api's handoff key, `handoff:<issue>:…:tester:…:<record>:`).
func (e *fixEnd) completionApplied(handoff string) bool {
	r := e.r
	r.t.Helper()
	var applied bool
	if err := r.st.Pool().QueryRow(context.Background(), `select exists (select 1 from processed_events where source = 'api'
		and event_id like 'handoff:' || $1 || ':%:tester:%:' || $2 || ':%')`, e.key, handoff).Scan(&applied); err != nil {
		r.t.Fatalf("read the processed completion of %s's tester: %v", e.key, err)
	}
	return applied
}

// k1PassesAt is the tester recording its pass of k1 at commit, once the daemon holds a state of
// k1 there; it returns once the daemon holds k1 yes at commit.
func (e *fixEnd) k1PassesAt(commit string) {
	r := e.at()
	r.t.Helper()
	r.awaitState("k1", commit, "of any word", func(scratchCIState) bool { return true })
	r.recordResult(e.worker(claim.RoleTester), "k1", commit)
	r.awaitState("k1", commit, "yes", func(held scratchCIState) bool { return held.State == "yes" })
}

// TestFixBesideReviewThroughDispatch drives a fix beside a review through the real surface, on
// one daemon and one Dispatch, each issue on a pull request of its own:
//
//   - plan-and-check, review-beside-ci (setup, each issue): the plan and the gate, and the issue in
//     reviewing while unit runs at X, the reviewer's turn open.
//   - fix-starts (issue 1): unit fails at X and the listener's settlement is published; the fix
//     row, its job read, its start row and its pending delivery, each after its barrier; the fix's
//     task reaches the implementer while the reviewer's turn stays open, its adoption naming X and
//     the fix's own branch; and the reviewer is told of the fix by its fix-started row.
//     control-no-settlement, read inside it: no read of X the fake GitHub answered before the
//     publish served the failure, and one after it did.
//   - fix-pushes (issue 1): the fix's one push, Y, and its completion at Y; the reviewer's approval
//     of X then decides its step.
//   - way-two (issue 1): the fix's way is 2, the tester starts again at Y with one return, and
//     Dispatch's steps hold the fix's run from X to Y naming k-ci and the review's run at X.
//   - end-waits (issue 1): the tester passes at Y and the reviewer approves Y while unit runs
//     there: the task is held for its CI; unit passes at Y and the retro starts; the merge; the
//     project's plans give the issue one fix.
//   - control-no-fix-flag (issue 2): the task's fixes_ci set false: the failure starts no fix, and
//     the owner is told once by ci-failed.
//   - control-other-footer (issue 3): after the fix's push of Y, a review whose footer names Z is
//     refused and decides nothing.
//   - fix-owns-owed-merge (issue 4): in testing at X, with the merge forward of X asked of the
//     implementer and its ask and the tester's hold still queued, unit fails: the fix takes the
//     ask, and neither queued message is published. The fix's push of Y lands the merge as the
//     fix's; the tester's completion at X is taken, and the fix's way starts it again at Y once,
//     with one return and no other merge message. Its control (issue 5): the implementer's own
//     merge of X into Y, which no fix took, and the tester's completion at X is refused.
//
// The issues of the merge forward are set up first. The daemon queues its second read of each
// one's X for a minute after the first; the test makes it due at once when its check begins
// (secondReadNow), so no check waits out that minute.
func TestFixBesideReviewThroughDispatch(t *testing.T) {
	t.Parallel()
	r := newFixRig(t, nil, fixBesideIssues...)
	r.logOutbox()
	one, two, three, four, five := r.walking(0), r.walking(1), r.walking(2), r.walking(3), r.walking(4)
	entries := map[*fixEnd]int64{}

	t.Run("the setup of the merge forward", func(t *testing.T) {
		// The implementer's and the tester's topics of issue 4 refuse every publish until the fix
		// takes the ask, so its ask and the tester's hold stay queued.
		four.refuseRoles(true, claim.RoleImplementer, claim.RoleTester)
		four.conflictsInTesting()
		five.conflictsInTesting()
	})

	t.Run("plan-and-check and review-beside-ci", func(t *testing.T) {
		for _, e := range []*fixEnd{one, two, three} {
			entries[e] = e.reviewBesideCI()
		}
	})

	// walked is the last check of issue 1's walk that ran to its end: each later check stands on
	// it, and fails at once, naming it, where it did not end.
	walked := ""
	standsOn := func(t *testing.T, check string) {
		t.Helper()
		if walked != check {
			t.Fatalf("issue 1's walk did not reach the end of %s (its last check to end: %q), so this check cannot run", check, walked)
		}
	}
	var fixTask shimwire.Prompt
	t.Run("fix-starts", func(t *testing.T) {
		e := one
		er := e.at()
		er.await("Dispatch's check_states_owed holds no row of "+e.key, func() bool { return er.owed() == "none" })
		er.awaitState(scratchCICheck, e.x, "that says no run concluded there", func(held scratchCIState) bool {
			return held.CICase == scratchCINotCompleted || held.CICase == scratchCIAbsent
		})

		run := e.unit("completed", "failure")
		er.setRun(e.x, run)
		// From the conclusion until the last barrier of this check the test writes no result, no
		// check block and no head of the issue.
		if owed := er.owed(); owed != "none" {
			t.Fatalf("Dispatch owes %s a settle once unit concluded at X (%s), so a settle other than the settlement's could read the failure", e.key, owed)
		}
		before := e.dispatchSeq()
		// The fake's reads up to the publish: control-no-settlement judges each by what it served.
		reads := len(r.checks.Reads())
		er.settle(e.x, run)

		held := er.awaitState(scratchCICheck, e.x, "that says its run failed", func(held scratchCIState) bool { return held.CICase == scratchCIFailure })
		if held.Seq <= before {
			t.Fatalf("the daemon holds k-ci's failure at X at Dispatch sequence %d, want one above %d, the issue's newest before the publish", held.Seq, before)
		}
		if fixes := e.fixes(); len(fixes) != 1 || fixes[0].fix != 1 || fixes[0].check != scratchCICheck || fixes[0].head != e.x {
			t.Fatalf("the fixes of %s once the daemon holds k-ci's failure at X = %+v, want fix 1 for k-ci at %s", e.key, fixes, e.x)
		} else if fixes[0].state == string(record.FixReading) && er.unfinishedRows(record.OutboxKindFixJobRead) == 0 {
			t.Fatalf("%s's fix 1 reads its job and no fix_job_read row of the issue stands: its read was never queued", e.key)
		}

		e.fixStartsToItsStartRow(t, run)
		if fix, from, branch := e.pendingFix(); fix != 1 || from != e.x || branch != e.fixBranch() {
			t.Fatalf("the pending delivery of %s's implementer names fix %d from %q on %q, want fix 1 from X %s on %s", e.key, fix, from, branch, e.x, e.fixBranch())
		}

		prompt, holdLine := e.awaitFixTask()
		fixTask = prompt
		e.assertFixTask(t, prompt, holdLine, run)
		if state := er.d.claim(e.worker(claim.RoleReviewer).token).State; state != string(supervise.StateWorking) {
			t.Fatalf("%s's reviewer is %s when the fix's task reached the implementer, want its turn still open", e.key, state)
		}
		e.assertFixAdopted(t)
		e.assertFixStartedTold(t, claim.RoleReviewer)

		t.Run("control-no-settlement", func(t *testing.T) {
			all := r.checks.Reads()
			concluded := func(read githubtest.CheckRunsRead) bool {
				for _, served := range read.Runs {
					if served.Name == scratchCIJob && served.Status == "completed" && served.Conclusion == "failure" {
						return true
					}
				}
				return false
			}
			for i, read := range all[:reads] {
				if read.SHA == e.x && concluded(read) {
					t.Fatalf("read %d of X, at %s, served unit concluded failure before the settlement was published: a trigger other than the settlement made Dispatch read the failed run",
						i+1, read.At.UTC().Format("15:04:05.000"))
				}
			}
			after := 0
			for _, read := range all[reads:] {
				if read.SHA == e.x && concluded(read) {
					after++
				}
			}
			if after == 0 {
				t.Fatalf("no read of X after the settlement's publish served unit concluded failure; the reads: %+v", all)
			}
		})
		walked = "fix-starts"
	})

	y := scratchCIHead(one.key, "y")
	t.Run("fix-pushes", func(t *testing.T) {
		standsOn(t, "fix-starts")
		e := one
		er := e.at()
		e.works(claim.RoleImplementer, fixTask)
		er.setRun(y, e.unit("in_progress", ""))
		if pushed := e.pushesY(); pushed != y {
			t.Fatalf("the fix pushed %s, want %s", pushed, y)
		}
		e.completes(claim.RoleImplementer, "implement", y)
		e.review("approved", e.x, e.x, entries[e])
		if decision := e.decision(); decision.State != "approved" || decision.Head != e.x || decision.Refused != "" {
			t.Fatalf("the reviewer's decision after its approval of X while the head is Y = %+v, want approved at %s, not refused", decision, e.x)
		}
		walked = "fix-pushes"
	})

	t.Run("way-two", func(t *testing.T) {
		standsOn(t, "fix-pushes")
		e := one
		er := e.at()
		if fixes := e.fixes(); len(fixes) != 1 || fixes[0].way != 2 || fixes[0].endHead != y {
			t.Fatalf("the fixes of %s once the review of X ended = %+v, want fix 1 ended at Y %s taking way 2", e.key, fixes, y)
		}
		var atY []record.SuperviseRequest
		for _, start := range e.starts(claim.RoleTester) {
			if start.request.MoveTo == y {
				atY = append(atY, start.request)
			}
		}
		if len(atY) != 1 {
			t.Fatalf("the tester's starts of %s moving to Y = %+v, want one", e.key, atY)
		}
		if returns := e.returns(); returns != 1 {
			t.Fatalf("%s's task counts %d returns, want one", e.key, returns)
		}
		e.completes(claim.RoleReviewer, "review", e.x)
		records := e.records()
		assertRecord(t, e.key, records, 1, 2, e.x, y, scratchCICheck)
		assertRecord(t, e.key, records, 3, 1, e.x, e.x, "")
		er.fact("way-two", "%s's records after the review of X: %+v", e.key, records)
		walked = "way-two"
	})

	t.Run("end-waits", func(t *testing.T) {
		standsOn(t, "way-two")
		e := one
		er := e.at()
		tester := e.worker(claim.RoleTester)
		tester.take()
		er.awaitState("k1", y, "of any word", func(scratchCIState) bool { return true })
		er.recordResult(tester, "k1", y)
		er.awaitState("k1", y, "yes", func(held scratchCIState) bool { return held.State == "yes" })
		e.completes(claim.RoleTester, "test", y)
		er.waitPhase(phase.Reviewing)
		task := e.worker(claim.RoleReviewer).take()
		if !strings.Contains(task.Message, e.x) {
			t.Fatalf("the reviewer's task of %s at Y does not name X %s, the commit it last reviewed", e.key, e.x)
		}
		e.review("approved", y, y, fixRigEntry(t, task.Message))
		if stands := e.awaitEndOrHold(); stands.Phase != phase.Held || stands.HoldReason != string(record.HoldForCI) {
			t.Fatalf("%s stands in %s held for %q after the approval of Y while unit runs there, want held for %q", e.key, stands.Phase, stands.HoldReason, record.HoldForCI)
		}
		run := e.unit("completed", "success")
		er.setRun(y, run)
		er.settle(y, run)
		er.awaitState(scratchCICheck, y, "yes", func(held scratchCIState) bool { return held.State == "yes" })
		if stands := e.standing(); stands.Phase != phase.Retro {
			t.Fatalf("%s stands in %s once k-ci passed at Y, want its retro, which the hold's release starts in that fact", e.key, stands.Phase)
		}
		e.completes(claim.RoleReviewer, "review", y)
		e.worker(claim.RoleImplementer).take()
		e.retroAndMerge(y)
		var plans struct {
			Issues []fixEndPlan `json:"issues"`
		}
		if err := er.personRead("/api/v1/projects/"+er.cfg.Project+"/plans", &plans); err != nil {
			t.Fatalf("read the plans of %s: %v", er.cfg.Project, err)
		}
		for _, finished := range plans.Issues {
			if finished.Issue == e.key {
				if finished.Fixes != 1 {
					t.Fatalf("the plans of %s give %s %d fixes, want 1", er.cfg.Project, e.key, finished.Fixes)
				}
				return
			}
		}
		t.Fatalf("the plans of %s = %+v, want %s finished", er.cfg.Project, plans, e.key)
	})

	t.Run("control-no-fix-flag", func(t *testing.T) {
		e := two
		er := e.at()
		er.exec(`update plan_tasks set fixes_ci = false where issue = $1`, e.key)
		e.unitFails()
		if fixes := e.fixes(); len(fixes) != 0 {
			t.Fatalf("the fixes of %s with no fix flag = %+v, want none", e.key, fixes)
		}
		if fix, _, _ := e.pendingFix(); fix != 0 {
			t.Fatalf("%s's implementer holds a pending delivery of fix %d with no fix flag, want none", e.key, fix)
		}
		e.review("approved", e.x, e.x, entries[e])
		if stands := e.awaitEndOrHold(); stands.Phase != phase.Held || stands.HoldReason != string(record.HoldForCI) {
			t.Fatalf("%s stands in %s held for %q after the approval of X while k-ci fails there, want held for %q", e.key, stands.Phase, stands.HoldReason, record.HoldForCI)
		}
		e.awaitRowsFinished(record.OutboxKindNotice)
		if told := e.toldOwner("ci-failed"); len(told) != 1 || !strings.Contains(told[0].Reason, scratchCICheck) {
			t.Fatalf("the ci-failed notices of %s's owner = %+v, want one naming %s", e.key, told, scratchCICheck)
		}
	})

	t.Run("control-other-footer", func(t *testing.T) {
		e := three
		er := e.at()
		run := e.unitFails()
		e.fixStartsToItsStartRow(t, run)
		other := scratchCIHead(e.key, "z")
		er.setRun(scratchCIHead(e.key, "y"), e.unit("in_progress", ""))
		pushed := e.pushesY()
		e.review("approved", pushed, other, entries[e])
		if decision := e.decision(); decision.Refused == "" || decision.State != "" {
			t.Fatalf("the reviewer's decision after a review whose footer names Z while the head is Y = %+v, want it refused and deciding nothing", decision)
		}
		if stands := e.standing(); stands.Phase != phase.Reviewing {
			t.Fatalf("%s stands in %s after the review naming Z, want reviewing", e.key, stands.Phase)
		}
	})

	t.Run("fix-owns-owed-merge", func(t *testing.T) {
		e := four
		er := e.at()
		if asked := e.awaitAsk(); asked.Fix != nil || asked.Landed != "" {
			t.Fatalf("%s's merge forward = %+v, want the implementer's ask of X, not landed and taken by no fix", e.key, asked)
		}
		ids, messages := e.mergeMessages()
		var ask, hold bool
		for _, message := range messages {
			ask = ask || message.Role == claim.RoleImplementer && message.Step == record.MergeForwardAsk && message.Head == e.x
			hold = hold || message.Role == claim.RoleTester && message.Step == record.MergeForwardHold && message.Head == e.x
		}
		if !ask || !hold || er.standingRows(ids) != len(ids) {
			t.Fatalf("%s's merge-forward rows %v = %+v, %d of them queued; want the implementer's ask and the tester's hold of X, both queued", e.key, ids, messages,
				er.standingRows(ids))
		}

		run := e.unitFails()
		owner := record.MergeForwardFix{Generation: 1, Task: "a", Number: 1}
		if taken := e.mergeForward(); taken.Fix == nil || *taken.Fix != owner || taken.Landed != "" {
			t.Errorf("%s's merge forward once k-ci failed at X = %+v, want the ask of X taken by fix 1, not landed", e.key, taken)
		}
		// The queued rows are made due now and the topics take publishes again: each row runs once
		// more, and the rows of an ask a fix took finish with nothing published.
		e.refuseRoles(false, claim.RoleImplementer, claim.RoleTester)
		er.exec(`update outbox set next_at = now() where id = any($1)`, ids)
		er.await(fmt.Sprintf("%s's merge-forward rows %v finished", e.key, ids), func() bool { return er.standingRows(ids) == 0 })
		for _, id := range ids {
			for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
				if er.publishedRow(notify.RoleTopicPrefix+string(er.tokenOf(e.key, role)), id) {
					t.Errorf("%s's merge-forward row %d was published to its %s once fix 1 took the ask", e.key, id, role)
				}
			}
		}

		e.fixStartsToItsStartRow(t, run)
		prompt, holdLine := e.awaitFixTask()
		e.assertFixTask(t, prompt, holdLine, run)
		e.assertFixAdopted(t)
		e.works(claim.RoleImplementer, prompt)
		y := e.pushesY()
		if landed := e.mergeForward(); landed.Landed != y || landed.ReviewAsked || landed.Fix == nil || *landed.Fix != owner {
			t.Errorf("%s's merge forward once the fix pushed Y = %+v, want it landed at %s by fix 1 with no review asked", e.key, landed, y)
		}
		e.completes(claim.RoleImplementer, "implement", y)

		e.k1PassesAt(e.x)
		entry := er.phaseOf(claim.RoleTester).Entry
		status, code, handoff := e.testerCompletesAt(e.x)
		if status != http.StatusOK {
			t.Fatalf("%s's tester's completion at X beside the fix = %d %s, want it taken", e.key, status, code)
		}
		e.worker(claim.RoleTester).endTurn()
		if !e.completionApplied(handoff) {
			t.Fatalf("the daemon holds no processed_events row of %s's tester's completion naming %s", e.key, handoff)
		}
		records := e.records()
		if tested, ok := fixEndRecordOf(records, "step", 2, 1); !ok || tested.Result != "passed" || fixEndCommit(tested.EndCommit) != e.x {
			t.Fatalf("%s's tester's record = %+v (found %t), want it passed at X %s", e.key, tested, ok, e.x)
		}
		if returns := e.returns(); returns != 1 {
			t.Fatalf("%s's task counts %d returns, want one", e.key, returns)
		}
		var again []record.SuperviseRequest
		for _, start := range e.starts(claim.RoleTester) {
			if start.request.Entry > entry {
				again = append(again, start.request)
			}
		}
		if len(again) != 1 || again[0].MoveTo != y {
			t.Fatalf("%s's tester's starts after its completion at X = %+v, want one, moving to Y %s", e.key, again, y)
		}
		if after, messages := e.mergeMessages(); len(after) != len(ids) {
			t.Fatalf("%s's merge-forward rows after the fix's landing = %+v, want only the %d queued before the fix took the ask", e.key, messages, len(ids))
		}
	})

	t.Run("control: an ordinary merge forward refuses the tester's completion at X", func(t *testing.T) {
		e := five
		if asked := e.awaitAsk(); asked.Fix != nil {
			t.Fatalf("%s's merge forward = %+v, want an ask no fix took", e.key, asked)
		}
		y := e.pushesY()
		if landed := e.mergeForward(); landed.Landed != y || landed.Fix != nil {
			t.Fatalf("%s's merge forward once the implementer pushed Y = %+v, want it landed at %s by no fix", e.key, landed, y)
		}
		e.k1PassesAt(e.x)
		if status, code, _ := e.testerCompletesAt(e.x); status == http.StatusOK || code != "HANDOFF_HEAD_MERGED_FORWARD" {
			t.Fatalf("%s's tester's completion at X after an ordinary merge forward of X = %d %q, want HANDOFF_HEAD_MERGED_FORWARD", e.key, status, code)
		}
	})
}
