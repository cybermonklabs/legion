package daemon

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The cases here are R4 of dispatch://INFRA-4480, a design gate's move to a phase holder whose
// claim is released, its cases (g) to (j). Each runs newsPathRig's tree, LEGION-800 standing at
// admitted with its spec approved at version 1 and its child LEGION-801 in testing, whose
// tester's phase took that version (owed no news). The gate moves through the Dispatch events the
// consumer applies (dispatchEvent): a person's changes request on version 1 closes it, and their
// approval of version 1 opens it again, so the opening owes the tester nothing and reaches it as
// the opened role message (workflow's openingDelivery). The wakes are the workflow's own, applied
// through an engine handed the store's pod tables (store.Store.PodRecordsOf), as the daemon's
// engine is. The listener keeps a kept publish for a role no live session holds (holderPublisher
// with keeps), as the listener of cybermonklabs/legion#4079 does.

const gateMergerIssue = "LEGION-803"

// gateMoveRig is newsPathRig's tree and the parts R4's cases read: the listener, the daemon's
// log, the engine that takes the workflow's wakes, and the store.
type gateMoveRig struct {
	*newsPathRig
	publisher *holderPublisher
	logs      *testlog.Buffer
	wakes     *workflow.Engine
	st        *store.Store
}

// newGateMoveRig records the tree, with the merger's issue LEGION-803 in merging beside the
// tester's when merger says so, and the told row of every holder and of the root's architect at
// version 1.
func newGateMoveRig(t *testing.T, merger bool) *gateMoveRig {
	t.Helper()
	members := []pathMember{{key: pathRoot, at: phase.Admitted}, {key: pathChild, parent: pathRoot, at: phase.Testing}}
	if merger {
		members = append(members, pathMember{key: gateMergerIssue, parent: pathRoot, at: phase.Merging})
	}
	r := newNewsPathRig(t, 1, members...)
	logs := &testlog.Buffer{}
	r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	publisher := &holderPublisher{keeps: true}
	r.runner.notices = publisher
	st, ok := r.runner.trees.(*store.Store)
	if !ok {
		t.Fatalf("the rig's tree store is %T, want the daemon's store", r.runner.trees)
	}
	tables, err := st.PodRecordsOf("LEGION")
	if err != nil {
		t.Fatalf("read the pod tables: %v", err)
	}
	wakes := workflow.New(r.records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Pods: tables}, quietLogger())
	for _, member := range members[1:] {
		r.told(member.key, record.AudienceHolder, 1)
	}
	r.told(pathRoot, record.AudienceArchitect, 1)
	return &gateMoveRig{newsPathRig: r, publisher: publisher, logs: logs, wakes: wakes, st: st}
}

// holder creates role's claim of issue as it stands in the case: released, as an idle release
// leaves it, or failed or suspended by an operator. It answers the claim's machine and the
// claim's Envoy topic, which no live session holds.
func (r *gateMoveRig) holder(issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) (*supervise.Machine, string) {
	r.t.Helper()
	token := mustClaimToken(r.t, issue, role)
	held := supervise.Claim{Token: token, Project: "legion", Tree: pathRoot, Issue: issue, Role: role, Generation: 1, ServingGeneration: 1,
		State: state, SuspendedBy: by, Session: fmt.Sprintf("ses-%s-%s", strings.ToLower(issue), role), SessionFile: r.t.TempDir() + "/session.jsonl"}
	if state == supervise.StateFailed {
		held.FailureCause, held.Failure, held.FailedAt = supervise.CauseLaunchRefused, "launch failures ran out", time.Now().UTC()
	}
	machine, _, err := r.sup.Create(r.ctx, held, "")
	if err != nil {
		r.t.Fatalf("create the %s of %s: %v", role, issue, err)
	}
	topic := notify.RoleTopicPrefix + string(token)
	absent := []string{topic}
	for existing := range r.publisher.absent {
		absent = append(absent, existing)
	}
	r.publisher.setAbsent(absent...)
	return machine, topic
}

// gateRows matches every role message to token's topic: a gate move's messages to that holder.
func gateRows(token claim.Token) func(pathRow) bool {
	return func(row pathRow) bool {
		payload, ok := row.payload.(record.RoleMessage)
		return ok && payload.Role == string(token)
	}
}

// gateOpen says whether row is a gate row whose typed mark (record.RoleMessage.Gate) names an
// opening.
func gateOpen(row pathRow) bool {
	msg, ok := row.payload.(record.RoleMessage)
	return ok && msg.Gate != nil && msg.Gate.Open
}

// runOne runs row alone, past the backoff of its last attempt, through the lanes' own claim,
// execute and settle (runRow), so a case decides the order in which rows that wait together run.
func (r *gateMoveRig) runOne(row pathRow) {
	r.t.Helper()
	r.later(2 * time.Minute)
	if err := r.runner.runRow(r.ctx, record.OutboxRow{ID: row.id, Kind: row.kind, Issue: row.issue}); err != nil {
		r.t.Fatalf("run outbox row %d: %v", row.id, err)
	}
}

// pass runs the gate rows of token once more, past the backoff of their last attempt, and answers
// the rows it ran.
func (r *gateMoveRig) pass(token claim.Token) []pathRow {
	r.t.Helper()
	r.later(2 * time.Minute)
	return r.run(gateRows(token))
}

// toTopic is every publish the listener took or kept for topic, in order.
func (r *gateMoveRig) toTopic(topic string) (delivered, kept []outboxPublish) {
	r.t.Helper()
	_, all := r.publisher.snapshot()
	for _, published := range all {
		if published.topic == topic {
			delivered = append(delivered, published)
		}
	}
	for _, published := range r.publisher.keptCopies() {
		if published.topic == topic {
			kept = append(kept, published)
		}
	}
	return delivered, kept
}

// wakesOf is how many wakes the record keeps of issue's pod.
func (r *gateMoveRig) wakesOf(issue string) int {
	r.t.Helper()
	return podResumes(r.t, r.pool, issue)
}

// wokenBySettlement wakes the tester's released pod by a CI settlement of its pull request's head,
// as the daemon applies a settlement (workflow's wakeOnSettlement), and runs the start the wake
// queued, which resumes the claim.
func (r *gateMoveRig) wokenBySettlement(machine *supervise.Machine) {
	r.t.Helper()
	head := pathHead(pathChild)
	applyFact(r.t, r.pool, "github", "settled-"+head, intake.PullRequestChecks{Repo: "acme/widgets", Number: 801, HeadSHA: head,
		Generation: 1, Snapshot: head + "-snapshot", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}}, r.wakes)
	r.resumeWoken(machine, "the CI settlement")
}

// resumeWoken runs the start a wake of the tester queued and waits for the claim to leave
// released.
func (r *gateMoveRig) resumeWoken(machine *supervise.Machine, wake string) {
	r.t.Helper()
	if got := r.wakesOf(pathChild); got != 1 {
		r.t.Fatalf("the tester has %d wakes after %s, want the one it took", got, wake)
	}
	r.later(2 * time.Minute)
	if ran := r.run(startOf(pathChild, claim.RoleTester, false)); len(ran) != 1 {
		r.t.Fatalf("ran %d starts of the tester after %s, want the one its wake queued", len(ran), wake)
	}
	machine.Wait()
	if held := machine.View(); held.Released() {
		r.t.Fatalf("the tester is %s by %q after %s's start ran, want it resumed", held.State, held.SuspendedBy, wake)
	}
}

// R4 (g) of dispatch://INFRA-4480: the gate closes and then opens over the tree while the tester's
// pod is released. While the claim reads released neither row publishes or wakes the tester: the
// close waits, and once the opening has been written the close, which the opening outran, ends
// unpublished. After a CI settlement's wake the opening reaches the tester's session once,
// published with keep, and the close is never published.
//
// At the pull request's base both rows end at their first attempt with "its role's pod is
// released, and the message is not worth a wake", and the tester's session never receives the
// opening; the test fails where it reads the close's row after that attempt.
func TestAGateMoveReachesAReleasedHolderOnceItIsWoken(t *testing.T) {
	t.Parallel()
	r := newGateMoveRig(t, false)
	token := mustClaimToken(t, pathChild, claim.RoleTester)
	tester, topic := r.holder(pathChild, claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByReleased)

	r.dispatchEvent(intake.DispatchArtifactChangesRequested, 1)
	closes := r.held(gateRows(token))
	if len(closes) != 1 || gateOpen(closes[0]) {
		t.Fatalf("the tester's rows after the close = %+v, want the one close message", closes)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		r.pass(token)
		if held, _ := r.still(closes[0]); !held {
			t.Fatalf("attempt %d: the close's row ended (%d lines %q), want it waiting while the tester's claim reads released",
				attempt, strings.Count(r.logs.String(), releasedLine), releasedLine)
		}
		if delivered, kept := r.toTopic(topic); len(delivered)+len(kept) != 0 {
			t.Fatalf("attempt %d: the tester was published %d messages and kept %d, want none while its claim reads released", attempt, len(delivered), len(kept))
		}
		if wakes := r.wakesOf(pathChild); wakes != 0 {
			t.Fatalf("attempt %d: the close took %d wakes of the tester, want none: a gate move is not worth one", attempt, wakes)
		}
	}

	r.dispatchEvent(intake.DispatchArtifactApproved, 1)
	rows := r.held(gateRows(token))
	if len(rows) != 2 || !gateOpen(rows[1]) {
		t.Fatalf("the tester's rows after the opening = %+v, want the close and the opened message: the tester's phase took version 1 and owes no news", rows)
	}
	opening := rows[1]
	r.pass(token)
	if held, _ := r.still(closes[0]); held {
		t.Fatalf("the close's row is still queued after the opening, want it ended unpublished: the opening outran it")
	}
	if held, _ := r.still(opening); !held {
		t.Fatalf("the opening's row ended while the tester reads released, want it waiting")
	}
	if delivered, kept := r.toTopic(topic); len(delivered)+len(kept) != 0 || r.wakesOf(pathChild) != 0 {
		t.Fatalf("the tester was published %d messages, kept %d and woken %d times before its wake, want none", len(delivered), len(kept), r.wakesOf(pathChild))
	}

	r.wokenBySettlement(tester)
	r.pass(token)
	delivered, kept := r.toTopic(topic)
	if len(delivered) != 0 || len(kept) != 1 {
		t.Fatalf("the tester was published %d messages and kept %d after its wake, want the opening once, kept for its woken session", len(delivered), len(kept))
	}
	if body := opening.payload.(record.RoleMessage).Body; kept[0].message != body || kept[0].key != record.OutboxKey(opening.id) {
		t.Fatalf("the listener keeps %#v, want the opened message %q under its row's key", kept[0], body)
	}
	if held, _ := r.still(opening); held {
		t.Fatalf("the opening's row is still queued after its publish, want it finished")
	}
	if wakes := r.wakesOf(pathChild); wakes != 1 {
		t.Fatalf("the tester took %d wakes, want the settlement's alone", wakes)
	}
}

// R4 (h) of dispatch://INFRA-4480: the gate closes and does not open, and the tester's claim is
// released. The close waits, waking nothing, until a backstop of the tester comes due and wakes it
// (workflow's podBackstopDue); then it reaches the tester's session once, with keep. The test arms
// the backstop itself, as a release's armBackstops arms one, since the rig runs no idle release.
func TestAGateMoveReachesAReleasedHolderAfterItsBackstop(t *testing.T) {
	t.Parallel()
	r := newGateMoveRig(t, false)
	token := mustClaimToken(t, pathChild, claim.RoleTester)
	tester, topic := r.holder(pathChild, claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByReleased)

	r.dispatchEvent(intake.DispatchArtifactChangesRequested, 1)
	closes := r.held(gateRows(token))
	if len(closes) != 1 {
		t.Fatalf("the tester's rows after the close = %+v, want the one close message", closes)
	}
	r.pass(token)
	if held, _ := r.still(closes[0]); !held {
		t.Fatalf("the close's row ended (%d lines %q), want it waiting while the tester's claim reads released",
			strings.Count(r.logs.String(), releasedLine), releasedLine)
	}
	if delivered, kept := r.toTopic(topic); len(delivered)+len(kept) != 0 || r.wakesOf(pathChild) != 0 {
		t.Fatalf("the tester was published %d messages, kept %d and woken %d times before its backstop, want none", len(delivered), len(kept), r.wakesOf(pathChild))
	}

	if err := r.st.ArmPodBackstop(r.ctx, store.PodWakeBackstop{Project: "legion", Issue: pathChild, Role: claim.RoleTester,
		DueAt: time.Now().UTC().Add(-time.Minute)}); err != nil {
		t.Fatalf("arm the tester's backstop: %v", err)
	}
	due, err := r.st.DuePodBackstops(r.ctx, "legion", time.Now().UTC())
	if err != nil || len(due) != 1 {
		t.Fatalf("the due backstops = %+v, %v; want the tester's", due, err)
	}
	applyFact(t, r.pool, "wake", fmt.Sprintf("pod-backstop:%d", due[0].ID), intake.PodBackstopDue{ID: due[0].ID, Issue: pathChild,
		Role: claim.RoleTester}, r.wakes)
	r.resumeWoken(tester, "the backstop")
	r.pass(token)
	delivered, kept := r.toTopic(topic)
	if len(delivered) != 0 || len(kept) != 1 || kept[0].message != closes[0].payload.(record.RoleMessage).Body {
		t.Fatalf("the tester was published %d messages and kept %+v after its backstop, want the close once, kept for its woken session", len(delivered), kept)
	}
	if held, _ := r.still(closes[0]); held {
		t.Fatalf("the close's row is still queued after its publish, want it finished")
	}
}

// R4 of dispatch://INFRA-4480, "Only the newest move is published", for a close that no later move
// outran: the gate closes while the tester's pod is released, and a new version of the closed step
// is posted while the gate stays closed. That version moves no gate (workflow's putGateTold writes
// no move while the standing gate stays where it stood), so the close is still the gate's newest
// move and the gate still stands as the close left it. After the tester's wake the close reaches
// its session once, with keep.
//
// At 1103560a the close's next attempt ends it as outrun ("the step the move closed stands at
// another version, or approved"), and the tester is never told; the test fails where it reads the
// close's row after that attempt.
func TestTheNewestGateMoveReachesAReleasedHolderAfterANewVersionOfTheClosedStep(t *testing.T) {
	t.Parallel()
	r := newGateMoveRig(t, false)
	token := mustClaimToken(t, pathChild, claim.RoleTester)
	tester, topic := r.holder(pathChild, claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByReleased)

	r.dispatchEvent(intake.DispatchArtifactChangesRequested, 1)
	closes := r.held(gateRows(token))
	if len(closes) != 1 || gateOpen(closes[0]) {
		t.Fatalf("the tester's rows after the close = %+v, want the one close message", closes)
	}
	r.dispatchEvent(intake.DispatchArtifactVersion, 2)
	if rows := r.held(gateRows(token)); len(rows) != 1 {
		t.Fatalf("the tester's rows after a new version of the closed step = %+v, want the close alone: the version moves no gate", rows)
	}
	r.pass(token)
	if held, _ := r.still(closes[0]); !held {
		t.Fatalf("the close's row ended after a new version while the gate stands closed, want it waiting: no later move outran it")
	}

	r.wokenBySettlement(tester)
	r.pass(token)
	delivered, kept := r.toTopic(topic)
	if len(delivered) != 0 || len(kept) != 1 || kept[0].message != closes[0].payload.(record.RoleMessage).Body {
		t.Fatalf("the tester was published %d messages and kept %+v after its wake, want the close once, kept for its woken session", len(delivered), kept)
	}
}

// R4 of dispatch://INFRA-4480, "Only the newest move is published", for two openings at one
// version: while the tester's pod is released the gate closes, opens, closes and opens again, each
// time on spec version 1, so four gate rows wait. After the tester's wake the lane runs the newest
// opening first, as it does once the older rows have climbed their backoff, then the rest from the
// newest down. Only the newest opening reaches the tester's session; every older row ends
// unpublished.
//
// At 1103560a the older opening is published again once the newest one's row and the close between
// them have finished: it finds no newer row in the outbox and the gate open at version 1. The test
// fails where it counts the kept messages.
func TestOnlyTheNewestGateMoveReachesAReleasedHolderAfterASecondOpeningAtOneVersion(t *testing.T) {
	t.Parallel()
	r := newGateMoveRig(t, false)
	token := mustClaimToken(t, pathChild, claim.RoleTester)
	tester, topic := r.holder(pathChild, claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByReleased)

	for _, move := range []intake.DispatchArtifactKind{intake.DispatchArtifactChangesRequested, intake.DispatchArtifactApproved,
		intake.DispatchArtifactChangesRequested, intake.DispatchArtifactApproved} {
		r.dispatchEvent(move, 1)
	}
	rows := r.held(gateRows(token))
	if len(rows) != 4 || gateOpen(rows[0]) || !gateOpen(rows[1]) || gateOpen(rows[2]) || !gateOpen(rows[3]) {
		t.Fatalf("the tester's rows after four moves = %+v, want close, opening, close, opening", rows)
	}

	r.wokenBySettlement(tester)
	for i := len(rows) - 1; i >= 0; i-- {
		r.runOne(rows[i])
	}
	delivered, kept := r.toTopic(topic)
	if len(delivered) != 0 || len(kept) != 1 {
		t.Fatalf("the tester was published %d messages and kept %d, want the newest opening once: an older move is never published after a newer one",
			len(delivered), len(kept))
	}
	if newest := rows[3]; kept[0].key != record.OutboxKey(newest.id) {
		t.Fatalf("the listener keeps the message of key %q, want the newest opening's, %q", kept[0].key, record.OutboxKey(newest.id))
	}
	if left := r.held(gateRows(token)); len(left) != 0 {
		t.Fatalf("the tester's gate rows left = %+v, want every older row ended unpublished", left)
	}
}

// R4 of dispatch://INFRA-4480, its end rule for an issue that left the phase its holder works, at
// admitted: the gate closes while the tester's pod is released, and the tester's issue then
// stands at admitted, which the architect works (phase.RoleFor answers the architect there). The
// close's row ends undelivered at its next attempt and wakes nothing, since RoleFor no longer
// answers the role the row was written for. No gate row is written for an issue at admitted in
// the first place: tellGateMoved addresses no member whose phase RoleFor gives the architect, and
// gateMoveMark marks no architect. Its negative control is gateMoveEnds without its phase test:
// the row then waits while the tester reads released, and the test fails where it reads the row.
func TestAGateMoveToAReleasedHolderWhoseIssueStandsAtAdmittedEndsAtOnce(t *testing.T) {
	t.Parallel()
	r := newGateMoveRig(t, false)
	token := mustClaimToken(t, pathChild, claim.RoleTester)
	tester, topic := r.holder(pathChild, claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByReleased)

	r.dispatchEvent(intake.DispatchArtifactChangesRequested, 1)
	closes := r.held(gateRows(token))
	if len(closes) != 1 {
		t.Fatalf("the tester's rows after the close = %+v, want the one close message", closes)
	}
	moved := *readIssue(t, r.pool, pathChild)
	moved.Phase = phase.Admitted
	putOutboxIssue(t, r.pool, r.records, moved)

	r.pass(token)
	if held, _ := r.still(closes[0]); held {
		t.Fatalf("the close's row is still queued with the tester's issue at admitted, want it ended undelivered: no phase worker works admitted")
	}
	const left = "outbox role message finished undelivered: its design gate move's issue left the phase its holder works, or the workflow"
	if n := strings.Count(r.logs.String(), left); n != 1 {
		t.Fatalf("the daemon logged %q %d times, want once", left, n)
	}
	if delivered, kept := r.toTopic(topic); len(delivered)+len(kept) != 0 || r.wakesOf(pathChild) != 0 {
		t.Fatalf("the tester was published %d messages, kept %d and woken %d times, want none", len(delivered), len(kept), r.wakesOf(pathChild))
	}
	if held := tester.View(); !held.Released() {
		t.Fatalf("the tester is %s by %q, want it still released", held.State, held.SuspendedBy)
	}
}

// R4 (i) of dispatch://INFRA-4480: the same moves with the tester's claim failed, or suspended by
// an operator, which is a takeover. Each row ends undelivered at its first attempt and wakes
// nothing.
func TestAGateMoveToAHolderNotReleasedButTakenOverEndsAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		by    supervise.SuspendSource
	}{
		{name: "failed", state: supervise.StateFailed},
		{name: "suspended by an operator", state: supervise.StateSuspended, by: supervise.SuspendedByOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newGateMoveRig(t, false)
			token := mustClaimToken(t, pathChild, claim.RoleTester)
			tester, topic := r.holder(pathChild, claim.RoleTester, tc.state, tc.by)
			for _, move := range []intake.DispatchArtifactKind{intake.DispatchArtifactChangesRequested, intake.DispatchArtifactApproved} {
				r.dispatchEvent(move, 1)
				rows := r.held(gateRows(token))
				if len(rows) != 1 {
					t.Fatalf("the tester's rows after the %s = %+v, want the one message of the move", move, rows)
				}
				r.pass(token)
				if held, _ := r.still(rows[0]); held {
					t.Fatalf("the row of the %s is still queued, want it ended undelivered at its first attempt: the tester's claim is %s", move, tc.name)
				}
			}
			if delivered, kept := r.toTopic(topic); len(delivered)+len(kept) != 0 {
				t.Fatalf("the tester was delivered %d messages and kept %d, want none", len(delivered), len(kept))
			}
			if wakes := r.wakesOf(pathChild); wakes != 0 {
				t.Fatalf("the moves took %d wakes of the tester, want none", wakes)
			}
			if held := tester.View(); held.State != tc.state || held.SuspendedBy != tc.by {
				t.Fatalf("the tester is %s by %q, want it %s by %q as the moves found it", held.State, held.SuspendedBy, tc.state, tc.by)
			}
		})
	}
}

// R4 (j) of dispatch://INFRA-4480: a merger of the same tree whose pod is released, beside the
// released tester (g) reads. The merger's gate moves are dispatch://INFRA-2040's
// (cybermonklabs/legion#4030) and dispatch://INFRA-4528's (#4661), so no row of this change waits
// for it, wakes it or adds a delivery: each move's message to the merger ends at its first attempt
// as every role message to a released role does. It guards today's rule for the merger and passes
// at the pull request's base. Its negative control is workflow's gateMoveMark marking the merger's
// message too: the merger's close then waits, and the test fails where it reads that row.
func TestAGateMoveToAReleasedMergerAddsNothing(t *testing.T) {
	t.Parallel()
	r := newGateMoveRig(t, true)
	merger := mustClaimToken(t, gateMergerIssue, claim.RoleMerger)
	r.holder(pathChild, claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByReleased)
	mergerMachine, mergerTopic := r.holder(gateMergerIssue, claim.RoleMerger, supervise.StateSuspended, supervise.SuspendedByReleased)

	for _, move := range []intake.DispatchArtifactKind{intake.DispatchArtifactChangesRequested, intake.DispatchArtifactApproved} {
		before := strings.Count(r.logs.String(), releasedLine)
		r.dispatchEvent(move, 1)
		rows := r.held(gateRows(merger))
		if len(rows) != 1 {
			t.Fatalf("the merger's rows after the %s = %+v, want the one message of the move", move, rows)
		}
		r.pass(merger)
		if held, _ := r.still(rows[0]); held {
			t.Fatalf("the merger's row of the %s is still queued, want it ended at its first attempt: this change waits for no merger", move)
		}
		if n := strings.Count(r.logs.String(), releasedLine) - before; n != 1 {
			t.Fatalf("the merger's row of the %s logged %q %d times, want once", move, releasedLine, n)
		}
	}
	if delivered, kept := r.toTopic(mergerTopic); len(delivered)+len(kept) != 0 {
		t.Fatalf("the merger was delivered %d messages and kept %d, want none", len(delivered), len(kept))
	}
	if wakes := r.wakesOf(gateMergerIssue); wakes != 0 {
		t.Fatalf("the moves took %d wakes of the merger, want none", wakes)
	}
	if held := mergerMachine.View(); !held.Released() {
		t.Fatalf("the merger is %s by %q, want it still released", held.State, held.SuspendedBy)
	}
}
