package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The cases here hold a task carrying news of the approved design on its claim while the record
// moves under it — the gate closes and opens again, the member leaves the phase the claim holds,
// or the turn the task waits behind ends in a provider error — and read, as the newsPathRig cases
// do, the whole way from the fact to the confirmation.

// pendingNewsOf decodes the news machine's pending task carries.
func pendingNewsOf(t *testing.T, machine *supervise.Machine) record.DeliveryNews {
	t.Helper()
	held := machine.Claim().Pending
	if held == nil || len(held.News) == 0 {
		t.Fatalf("%s holds %+v, want a task carrying news", machine.Claim().Token, held)
	}
	var news record.DeliveryNews
	if err := json.Unmarshal(held.News, &news); err != nil {
		t.Fatalf("decode the news %s holds: %v", machine.Claim().Token, err)
	}
	return news
}

// The architect's condition of one delivery of the opening per holder (workflow's
// openingDelivery), when the opening becomes owed after the task that carries the holder's news
// was composed.
//
// The reviewer took version 3 of the spec and is owed version 4, and owes no opening: an earlier
// task told it the opening while the change list of 3 to 4 was not yet recorded, so that task
// carried the opening alone. Its phase task's turn is over, and it is in a turn of its own when
// the reconcile records the change list and queues its news-only start. That task carries the
// change list and no opening, and waits for the turn's end. A person then requests changes to
// version 4, which closes the gate, and the reviewer is told it closed. The person approves version
// 4 again before the turn ends: the reviewer is owed news, so it is sent no opened message, and its
// row records the opening as owed; the reconcile queues no news-only start beside the task it has
// not taken. So the task, sent at the turn's end, is the one delivery of the opening: it leads with
// the opened sentence, and its confirmation clears the opening with the version it told. A later
// version that Dispatch carries approved, which changes decision blocks alone, then sends nothing:
// the reconcile raises what the reviewer took.
func TestAnOpeningOwedAfterAHoldersTaskWasComposedIsToldByThatTaskOnce(t *testing.T) {
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	r.client.answer = func(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
		changes := dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To, DecisionOnly: true}
		if pair.To == 4 {
			changes = dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To,
				Sections: []dispatch.SectionChange{{Heading: "Design", Change: dispatch.ChangeChanged}}}
		}
		return changes, nil
	}
	token := mustClaimToken(t, pathRoot, claim.RoleReviewer)

	// The reviewer's phase start runs before the reconcile has read the change list of 3 to 4, so
	// its task carries none of it. The reviewer works that task, and its turn ends.
	r.complete(pathRoot)
	r.run(startOf(pathRoot, claim.RoleReviewer, false))
	reviewer, conn := r.connect(pathRoot, claim.RoleReviewer)
	r.delivered(reviewer)
	r.startTurn(reviewer)
	endTurn(r.ctx, t, reviewer, token)
	r.wantTold("after the reviewer's phase task", pathRoot, record.AudienceHolder, 3, false)

	// In a turn of its own, the reviewer is queued the news task, which waits for that turn.
	ownTurn(r.ctx, t, reviewer, token)
	if got := r.run(ofKind(record.OutboxKindDesignChanges)); len(got) != 1 {
		t.Fatalf("design_changes rows after the move = %d, want the one the move queued", len(got))
	}
	if got := r.run(startOf(pathRoot, claim.RoleReviewer, true)); len(got) != 1 {
		t.Fatalf("the reviewer's news-only starts = %d, want the one the reconcile queued", len(got))
	}
	if news := pendingNewsOf(t, reviewer); len(news.Items) != 1 || news.Items[0].To != 4 || len(news.Openings) != 0 {
		t.Fatalf("the reviewer's news task carries %+v, want the item of version 4 and no opening", news)
	}

	// A changes request closes the gate, and the reviewer is told so.
	r.dispatchEvent(intake.DispatchArtifactChangesRequested, 4)
	r.run(ofKind(record.OutboxKindRoleMessage))
	closed := r.publishedTo(token)
	if len(closed) != 1 || !strings.HasPrefix(closed[0], "The design gate of LEGION-800 closed") {
		t.Fatalf("the reviewer's role messages after the changes request = %q, want the one closed message", closed)
	}

	// The person approves version 4 again while the reviewer's turn runs.
	mark, before := r.lastRowID(), len(conn.Prompts())
	r.dispatchEvent(intake.DispatchArtifactApproved, 4)
	r.wantTold("at the opening", pathRoot, record.AudienceHolder, 3, true)
	r.run(ofKind(record.OutboxKindDesignChanges))
	r.run(ofKind(record.OutboxKindRoleMessage))

	// The turn ends, and the news task is sent.
	endTurn(r.ctx, t, reviewer, token)
	sent := r.delivered(reviewer)
	prompted := promptsAfter(conn, before)
	if len(prompted) != 1 || prompted[0].DeliveryID != sent.ID {
		t.Fatalf("the reviewer's prompts after the opening = %+v, want the news task, once", prompted)
	}
	const opened = "The design gate of LEGION-800 opened: a person approved version 4 of its spec document (artifact-800)"
	if strings.Count(prompted[0].Message, opened) != 1 || !strings.Contains(prompted[0].Message, "version 4, since version 3, which your phase last took") {
		t.Fatalf("the reviewer's news task = %q, want it to lead with the opening once, %q, and tell version 4", prompted[0].Message, opened)
	}
	r.startTurn(reviewer)
	r.wantTold("after the news task's confirmation", pathRoot, record.AudienceHolder, 4, false)
	if addressed := r.seenSince(mark, addressedTo(pathRoot, claim.RoleReviewer, token)); len(addressed) != 0 {
		t.Fatalf("outbox rows addressed to the reviewer since the approval = %+v, want none: the task it held is the opening's one delivery", addressed)
	}
	if published := r.publishedTo(token); len(published) != 1 {
		t.Fatalf("the reviewer's role topic carried %q, want the closed message alone", published)
	}
	endTurn(r.ctx, t, reviewer, token)

	// A version that changes decision blocks alone, which Dispatch carries approved, sends nothing.
	mark, before = r.lastRowID(), len(conn.Prompts())
	r.dispatchEvent(intake.DispatchArtifactApproved, 5)
	r.dispatchEvent(intake.DispatchArtifactVersion, 5)
	if got := r.run(ofKind(record.OutboxKindDesignChanges)); len(got) != 1 {
		t.Fatalf("design_changes rows after version 5 = %d, want the one its approval queued", len(got))
	}
	r.run(ofKind(record.OutboxKindRoleMessage))
	if addressed := r.seenSince(mark, addressedTo(pathRoot, claim.RoleReviewer, token)); len(addressed) != 0 {
		t.Fatalf("outbox rows addressed to the reviewer after a carried decision-only version = %+v, want none", addressed)
	}
	if prompted := promptsAfter(conn, before); len(prompted) != 0 {
		t.Fatalf("the reviewer's prompts after a carried decision-only version = %+v, want none", prompted)
	}
	r.wantTold("after the carried decision-only version", pathRoot, record.AudienceHolder, 5, false)
}

// A holder item an architect's task carries is told only while that architect holds the member's
// phase, which it does while the member stands in accepting (Design 6: the merger reads what
// changed before it merges).
//
// A child the root architect owns is owed version 5 when its reviewer's completion moves it into
// accepting, and the reconcile queues the root architect's news-only start, which waits behind a
// turn of the architect's own. In that turn the architect signs the child off; the child's retro
// completes, and the child stands in merging, where the merger's start attaches the same news. The
// architect's task, sent at its turn's end, tells nothing of a member it no longer holds: it is
// retired unsent, and the next turn the architect starts confirms nothing. So the child's holder
// row still says version 4, and the merger's task, sent when the merger comes up, carries the
// change list; its confirmation raises the row.
func TestAMemberThatLeftAcceptingBeforeItsArchitectsSendIsToldByItsMerger(t *testing.T) {
	r := newNewsPathRig(t, 5, pathMember{key: pathRoot, at: phase.Implementing}, pathMember{key: pathChild, parent: pathRoot, at: phase.Reviewing})
	r.told(pathRoot, record.AudienceHolder, 5)
	r.told(pathChild, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 5)
	architectToken := mustClaimToken(t, pathRoot, claim.RoleArchitect)
	enqueueOutbox(t, r.pool, r.records, mustOutboxRow(t, pathRoot, record.SuperviseRequest{Op: "start", Tree: pathRoot,
		Role: claim.RoleArchitect, Generation: 1}, time.Now()))
	r.run(startOf(pathRoot, claim.RoleArchitect, false))
	architect, architectConn := r.connect(pathRoot, claim.RoleArchitect)
	ownTurn(r.ctx, t, architect, architectToken)

	// The child's review ends; it stands in accepting, and the architect's news task waits.
	r.complete(pathChild)
	if got := r.run(ofKind(record.OutboxKindDesignChanges)); len(got) != 1 {
		t.Fatalf("design_changes rows after the move into accepting = %d, want the one the move queued", len(got))
	}
	if got := r.run(startOf(pathRoot, claim.RoleArchitect, true)); len(got) != 1 {
		t.Fatalf("the root architect's news-only starts = %d, want the one the reconcile queued", len(got))
	}
	if news := pendingNewsOf(t, architect); len(news.Items) != 1 || news.Items[0].Issue != pathChild || news.Items[0].Audience != record.AudienceHolder {
		t.Fatalf("the architect's news task carries %+v, want the holder item of %s", news, pathChild)
	}

	// In that turn the architect signs the child off, and the child's retro completes.
	applyFact(t, r.pool, "api", "sign-off-"+pathChild, intake.SignOff{Issue: pathChild}, r.engine, r.admission)
	r.complete(pathChild)
	if issue := readIssue(t, r.pool, pathChild); issue.Phase != phase.Merging {
		t.Fatalf("%s stands in %s after its retro's completion, want merging", pathChild, issue.Phase)
	}
	if got := r.run(startOf(pathChild, claim.RoleMerger, false)); len(got) != 1 {
		t.Fatalf("the merger's start rows = %d, want the one the move wrote", len(got))
	}

	// The architect's turn ends, and its next turn starts.
	endTurn(r.ctx, t, architect, architectToken)
	if err := architect.Handle(r.ctx, supervise.StreamTurnStart{Claim: architectToken}); err != nil {
		t.Fatalf("start the architect's next turn: %v", err)
	}

	merger, mergerConn := r.connect(pathChild, claim.RoleMerger)
	sent := r.delivered(merger)
	prompted := mergerConn.Prompts()
	if len(prompted) != 1 || prompted[0].DeliveryID != sent.ID {
		t.Fatalf("the merger's prompts = %+v, want its task, once", prompted)
	}
	for _, want := range []string{"version 5, since version 4, which your phase last took", `"Design" changed`, `"r2" changed`} {
		if !strings.Contains(prompted[0].Message, want) {
			t.Fatalf("the merger's task = %q, want it to say %q: the architect's task never told the member's news", prompted[0].Message, want)
		}
	}
	if got := architectConn.Prompts(); len(got) != 0 {
		t.Fatalf("the architect's prompts = %+v, want none: its task named only a member it no longer holds", got)
	}
	r.wantTold("before the merger's turn took its task", pathChild, record.AudienceHolder, 4, false)
	r.startTurn(merger)
	r.wantTold("after the merger's turn confirmed its task", pathChild, record.AudienceHolder, 5, false)
}

// A news-only start that reaches a claim whose turn is working its phase task does not take that
// task's place: the task is still the turn's, and a provider error that ends the turn takes it
// back and sends it again. The start waits on its backoff, also while the task is owed again on the
// provider error's ladder, and its news goes once the claim holds no task.
func TestAProviderErrorInTheTurnANewsOnlyStartReachedSendsThePhaseTaskAgain(t *testing.T) {
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	clock := &heldClock{}
	r.sup.deps.Clock = clock
	token := mustClaimToken(t, pathRoot, claim.RoleTester)

	r.complete(pathRoot)
	r.run(startOf(pathRoot, claim.RoleTester, false))
	tester, conn := r.connect(pathRoot, claim.RoleTester)
	phaseTask := r.delivered(tester)
	r.startTurn(tester)

	// Version 5 is approved while the turn runs, and the tester's news-only start reaches it.
	r.dispatchEvent(intake.DispatchArtifactVersion, 5)
	r.dispatchEvent(intake.DispatchArtifactApproved, 5)
	r.run(ofKind(record.OutboxKindDesignChanges))
	newsStarts := r.run(startOf(pathRoot, claim.RoleTester, true))
	if len(newsStarts) != 1 {
		t.Fatalf("the tester's news-only starts = %d, want the one the reconcile queued", len(newsStarts))
	}

	// The turn ends in a provider error.
	if err := tester.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("end the tester's turn: %v", err)
	}
	if err := tester.Handle(r.ctx, supervise.StreamPromptResult{Claim: token, DeliveryID: phaseTask.ID, AgentInvoked: true,
		Error: "overloaded_error", HTTPStatus: 529}); err != nil {
		t.Fatalf("report the provider error of the tester's turn: %v", err)
	}
	owed := tester.Claim().Pending
	if owed == nil || owed.Task != phaseTask.Task || !owed.ConfirmedAt.IsZero() || owed.ProviderErrorRetries != 1 {
		t.Fatalf("the tester holds %+v after the provider error, want its phase task taken back to be sent again", owed)
	}

	// The news-only start runs again while the ladder waits, and still waits.
	r.later(time.Minute)
	r.run(startOf(pathRoot, claim.RoleTester, true))
	if still, _ := r.still(newsStarts[0]); !still {
		t.Fatal("the tester's news-only start finished while the tester owes its phase task, want it waiting")
	}
	if held := tester.Claim().Pending; held == nil || held.ID != owed.ID {
		t.Fatalf("the tester holds %+v after its news-only start ran again, want the phase task %s the ladder sends again", held, owed.ID)
	}

	// The ladder's wait ends, and the phase task is sent again.
	if fired := clock.fire(30 * time.Second); fired != 1 {
		t.Fatalf("the ladder's first wait fired %d timers, want the one the provider error armed", fired)
	}
	testwait.Eventually(t, "the phase task sent again", func() bool { return len(conn.Prompts()) == 2 })
	again := conn.Prompts()[1]
	if again.DeliveryID != owed.ID || !strings.Contains(again.Message, phaseTask.Task) {
		t.Fatalf("the tester's second prompt = %+v, want its phase task again under %s", again, owed.ID)
	}

	// The resent task's turn takes it and ends; the news-only start then reaches a claim that
	// holds no task, and its news is sent.
	r.delivered(tester)
	r.startTurn(tester)
	endTurn(r.ctx, t, tester, token)
	r.later(2 * time.Minute)
	r.run(startOf(pathRoot, claim.RoleTester, true))
	if still, _ := r.still(newsStarts[0]); still {
		t.Fatal("the tester's news-only start still waits once the tester holds no task, want it run")
	}
	news := r.delivered(tester)
	if prompted := conn.Prompts(); len(prompted) != 3 || prompted[2].DeliveryID != news.ID ||
		!strings.Contains(prompted[2].Message, "version 5, since version 4, which your phase last took") {
		t.Fatalf("the tester's prompts = %+v, want the news task third, telling version 5", prompted)
	}
}
