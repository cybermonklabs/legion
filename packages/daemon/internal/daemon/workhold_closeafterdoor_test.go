package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Decision 16 of dispatch://INFRA-4528's plan at the daemon, for a gate that closes under the
// controller's request: the agent door reads the tree's work hold row and finds the gate open, and
// the read made again where the request lands finds it closed (api's agentDoor, then
// restartWorkedPhase, supervise's workHoldWaits and forgetSession, each through
// supervise.Machine.WorkHoldRefusal or workHoldRefuses). That later refusal is the hold's, and the
// controller is answered as the door answers the hold: 409 HELD_BY_WORK_HOLD with the hold's two
// ways out, the gate's opening and an operator's own act (api's refuseHold). The one exception is
// a refusal met once a fresh session's forget has left the claim's session behind: the claim has
// changed, so a caller at this daemon's contract is answered 200 with the hold and its two ways
// out in phaseNotRestarted, and a caller below the claim start entry is answered the hold's 409
// as before (api's refusedAfterTheForget, dispatch://INFRA-5649 R2). The cases run over the
// work-hold rig and the controller's door of workhold_agentdoor_test.go and
// workhold_controller_test.go:
//
//   - (a) the controller's retry of a claim it suspended, which reaches the machine's retry
//     (api's machineRetry);
//   - (b) its retry of the held worker its stop retired, which reaches the restart of the worker's
//     phase (api's restartWorkedPhase);
//   - (c) its fresh session of that retired worker, whose start is that same restart once the
//     forget has run;
//   - (d) its fresh session of a claim it suspended, whose start is the resume of a claim the
//     request found suspended once the forget has run (api's freshSession);
//   - (e) its fresh session of a live worker, ready in its session, whose own suspension of the
//     agent runs before the forget, which the close then refuses (api's freshSession,
//     itsSuspensionStands).
//
// A claim the hold stopped is in none of them: the door refuses it from the claim alone, with no
// read of the row to come before the close (supervise.Claim.WorkHeld).

// gateClosingUnderTheRequest is the rig's store with the tree's gate closing under a request of
// the controller's. The close is in the store before the request. openFor has the next reads of a
// tree's work hold row answer that no hold stands, as they would have before the close, and every
// read after them goes to the store, which finds the hold standing; it counts those (closedReads).
// Every other call goes straight to the store.
type gateClosingUnderTheRequest struct {
	supervise.Store
	mu          sync.Mutex
	open        int
	closedReads int
}

func (g *gateClosingUnderTheRequest) WorkHoldStands(ctx context.Context, tree string) (bool, error) {
	g.mu.Lock()
	if g.open > 0 {
		g.open--
		g.mu.Unlock()
		return false, nil
	}
	g.closedReads++
	g.mu.Unlock()
	return g.Store.WorkHoldStands(ctx, tree)
}

// openFor has the next reads reads of a tree's work hold row find the gate open, and counts the
// reads after them from none.
func (g *gateClosingUnderTheRequest) openFor(reads int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open, g.closedReads = reads, 0
}

// reads is how many of the reads openFor named are still to come, and how many the store answered
// after them.
func (g *gateClosingUnderTheRequest) reads() (open, closed int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open, g.closedReads
}

// closingGate puts gateClosingUnderTheRequest in the rig's store, with no read answered open yet.
// A machine keeps the store it was built with, so a case calls this before its walk creates the
// claim.
func closingGate(r *workHoldRig) *gateClosingUnderTheRequest {
	r.t.Helper()
	gate := &gateClosingUnderTheRequest{Store: r.trees}
	r.sup.deps.Store = gate
	return gate
}

// askedOnceTheGateCloses is the controller's request on route for role's agent, from a caller
// stating contract (askTheControllerDoorStating), with the gate open to the first open reads of
// the tree's work hold row the request makes — the agent door's, and the forget's where a case
// says so — and closed at the next. The request makes each of those reads and one more, the one
// that refuses its start. It returns the answer.
func askedOnceTheGateCloses(r *workHoldRig, gate *gateClosingUnderTheRequest, route string, role claim.Role, open, contract int) *httptest.ResponseRecorder {
	r.t.Helper()
	gate.openFor(open)
	recorder := askTheControllerDoorStating(r, route, role, contract)
	if left, closed := gate.reads(); left != 0 || closed != 1 {
		r.t.Fatalf("the controller's %s of the %s made %d reads of the tree's work hold row with the gate open and %d once it closed, want %d and one, the read that refuses it; body %s",
			route, role, open-left, closed, open, recorder.Body)
	}
	return recorder
}

// refusedOnceTheGateCloses is askedOnceTheGateCloses from a caller that states no contract,
// answered 409 HELD_BY_WORK_HOLD. It returns the refusal's sentence.
func refusedOnceTheGateCloses(r *workHoldRig, gate *gateClosingUnderTheRequest, route string, role claim.Role, open int) string {
	r.t.Helper()
	return wantHeldByTheWorkHold(r, route, role, askedOnceTheGateCloses(r, gate, route, role, open, 0))
}

// wantHeldByTheWorkHold fails the test where recorder, the answer to the controller's request on
// route for role's agent, the gate closing after the door's read, is not 409 HELD_BY_WORK_HOLD.
// It returns the refusal's sentence.
func wantHeldByTheWorkHold(r *workHoldRig, route string, role claim.Role, recorder *httptest.ResponseRecorder) string {
	r.t.Helper()
	var failure api.Failure
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil || recorder.Code != http.StatusConflict || failure.Code != "HELD_BY_WORK_HOLD" {
		r.t.Fatalf("the controller's %s of the %s, the gate closing after the door's read, = %d %q, want 409 HELD_BY_WORK_HOLD; body %s",
			route, role, recorder.Code, failure.Code, recorder.Body)
	}
	return failure.Error
}

// workHoldWords, heldByTheWorkHold and theTwoWaysOut are the hold's refusal of the controller's
// start of token: what holds it, that refusal before what the request did to the agent, and the
// two ways out after it, written out here, never read from the code under test (api's
// holdSentences, refuseHold).
const workHoldWords = "a phase worker of a tree whose work hold stands (`gates.hold: work`), and the controller starts no phase worker of that tree but the merger while its standing design gate is closed"

func heldByTheWorkHold(token claim.Token) string {
	return string(token) + " is " + workHoldWords + ", so "
}

func theTwoWaysOut(token claim.Token) string {
	return ". The gate's opening starts each agent the hold stopped, or an operator's own act starts this one: `legion claims resume --claim " +
		string(token) + "` or `legion claims retry --claim " + string(token) + "` is theirs to run"
}

// wantRefusedWithNothingChanged fails the test where said, the refusal of the controller's action
// on the implementer, is not the hold's refusal of that action saying that nothing of the claim
// changed, with the two ways out.
func wantRefusedWithNothingChanged(r *workHoldRig, said, action string) {
	r.t.Helper()
	token := r.token(claim.RoleImplementer)
	if want := heldByTheWorkHold(token) + action + " by the controller is refused and nothing of the claim changed" + theTwoWaysOut(token); said != want {
		r.t.Fatalf("the refusal says\n%q\nwant\n%q", said, want)
	}
}

// wantNothingChangedSince fails the test where what left role's claim other than as before, as its
// machine and as the store hold it, wrote an outbox row, a start row of the role among them,
// launched the role's agent, or moved the root from root's phase and run.
func wantNothingChangedSince(r *workHoldRig, what string, role claim.Role, before claimAsItStands, root record.Issue) {
	r.t.Helper()
	after := standing(r, role)
	sameClaim(r, what, "its machine", before.machine, after.machine)
	sameClaim(r, what, "the store", before.stored, after.stored)
	if got := r.startsSince(role, before.newestRow); got != 0 {
		r.t.Fatalf("%s wrote %d start rows of the %s, want none", what, got, role)
	}
	if after.newestRow != before.newestRow {
		r.t.Fatalf("%s wrote outbox rows up to %d past %d, want none", what, after.newestRow, before.newestRow)
	}
	if after.launches != before.launches {
		r.t.Fatalf("%s launched the %s %d times, want none", what, role, after.launches-before.launches)
	}
	if got := r.issue(); got.Phase != root.Phase || got.Generation != root.Generation {
		r.t.Fatalf("after %s the root is in %s at generation %d, want it in %s at generation %d: no phase restart",
			what, got.Phase, got.Generation, root.Phase, root.Generation)
	}
}

// wantNothingStartedByTheOutbox runs the outbox's passes after what, and fails the test where they
// change role's claim from refused, as it stood after what, queue a start of the role, or launch
// its agent: the gate stays closed, so nothing starts the claim.
func wantNothingStartedByTheOutbox(r *workHoldRig, what string, role claim.Role, refused claimAsItStands) {
	r.t.Helper()
	r.drain()
	after := standing(r, role)
	sameClaim(r, "the outbox's passes after "+what, "its machine", refused.machine, after.machine)
	sameClaim(r, "the outbox's passes after "+what, "the store", refused.stored, after.stored)
	if got := r.rows(record.OutboxKindSupervise, "start", role); got != 0 {
		r.t.Fatalf("the outbox holds %d starts of the %s after %s, want none", got, role, what)
	}
	if after.launches != refused.launches {
		r.t.Fatalf("the runtime launched the %s %d times after %s, want none", role, after.launches-refused.launches, what)
	}
}

// withoutSession is c with the session it recorded left behind, as the forget leaves it.
func withoutSession(c supervise.Claim) supervise.Claim {
	c.Session, c.SessionFile = "", ""
	return c
}

// Decision 16, (a): the controller's retry of a claim it suspended — before the close, or over the
// hold's stop after it — reaches the machine's retry (api's machineRetry), since the claim neither
// failed nor retired. With the gate open at the agent door's read and closed at the machine's
// (supervise's workHoldWaits), the retry is refused 409 HELD_BY_WORK_HOLD with the hold's two ways
// out, as the door refuses it, and nothing of the claim changes, then or by the outbox's passes
// after.
func TestWorkHoldRefusesTheControllersRetryWhenTheGateClosesAfterTheDoorsRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		walk func(r *workHoldRig)
	}{
		{"a claim the controller suspended before the close", suspendedBeforeTheClose},
		{"a held claim the controller's suspension took over", takenOverAfterTheClose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			gate := closingGate(r)
			tc.walk(r)
			before, root := standing(r, claim.RoleImplementer), r.issue()

			said := refusedOnceTheGateCloses(r, gate, agentRetry, claim.RoleImplementer, 1)

			wantRefusedWithNothingChanged(r, said, "a retry")
			wantNothingChangedSince(r, "the controller's refused retry", claim.RoleImplementer, before, root)
			wantNothingStartedByTheOutbox(r, "the refused retry", claim.RoleImplementer, before)
		})
	}
}

// Decision 16, (b): the controller's retry of the held worker its stop retired reaches the restart
// of the worker's phase (api's restartWorkedPhase). With the gate open at the agent door's read and
// closed at the restart's, the retry is refused 409 HELD_BY_WORK_HOLD with the hold's two ways out,
// as the door refuses it: no phase restart is applied, no start row of the implementer is written,
// the root stands in its phase at its run, and the claim stays retired with its session, then and
// after the outbox's passes.
func TestWorkHoldRefusesTheControllersRetryOfARetiredWorkerWhenTheGateClosesAfterTheDoorsRead(t *testing.T) {
	t.Parallel()
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	gate := closingGate(r)
	retiredByTheController(r)
	before, root := standing(r, claim.RoleImplementer), r.issue()

	said := refusedOnceTheGateCloses(r, gate, agentRetry, claim.RoleImplementer, 1)

	wantRefusedWithNothingChanged(r, said, "a retry")
	wantNothingChangedSince(r, "the controller's refused retry", claim.RoleImplementer, before, root)
	wantNothingStartedByTheOutbox(r, "the refused retry", claim.RoleImplementer, before)
}

// Decision 16, (c): the controller's fresh session of the held worker its stop retired, with the
// gate closing after the agent door's read, and nothing of the claim started, then or by the
// outbox's passes after:
//
//   - closed at the forget's read (supervise's forgetSession), it is refused 409
//     HELD_BY_WORK_HOLD with the hold's two ways out before anything changes, so the claim keeps
//     its session;
//   - closed after the forget's read, at the restart of the worker's phase the start of a retired
//     worker runs (api's retryStart, restartWorkedPhase), its start is refused once the forget has
//     left the session behind, and answered as what the request did (startRefusedAfterTheForget).
func TestWorkHoldRefusesTheControllersFreshSessionOfARetiredWorkerWhenTheGateClosesAfterTheDoorsRead(t *testing.T) {
	t.Parallel()
	t.Run("closed at the forget's read", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
		gate := closingGate(r)
		retiredByTheController(r)
		before, root := standing(r, claim.RoleImplementer), r.issue()

		said := refusedOnceTheGateCloses(r, gate, agentFreshSession, claim.RoleImplementer, 1)

		wantRefusedWithNothingChanged(r, said, "a fresh session")
		wantNothingChangedSince(r, "the controller's refused fresh_session", claim.RoleImplementer, before, root)
		wantNothingStartedByTheOutbox(r, "the refused fresh_session", claim.RoleImplementer, before)
	})
	t.Run("closed at the phase restart's read, after the forget", func(t *testing.T) {
		startRefusedAfterTheForget(t, retiredByTheController)
	})
}

// Decision 16, (d): the controller's fresh session of a claim it suspended before the close, with
// the gate open at the agent door's read and at the forget's, and closed at the read made where
// the resume of a claim the request found suspended lands (api's freshSession, supervise's
// workHoldWaits): the resume is refused once the forget has left the session behind, and
// answered as what the request did (startRefusedAfterTheForget).
func TestWorkHoldRefusesTheControllersFreshSessionOfASuspendedWorkerWhenTheGateClosesAfterTheForget(t *testing.T) {
	t.Parallel()
	startRefusedAfterTheForget(t, suspendedBeforeTheClose)
}

// Decision 16, (e): the controller's fresh session of a live worker, the implementer ready in its
// session with the close in the store and its stop not yet run, the gate open at the agent door's
// read and closed at the forget's (supervise's forgetSession). The request's own suspension of the
// agent ran between those two reads, and nothing takes it over: the controller's suspension
// outranks the hold's stop (supervise's suspensionRank), and the gate's opening starts only the
// claims the hold stopped (workflow's openHeldClaims). So the refusal is 409 HELD_BY_WORK_HOLD
// saying that the suspension stands with the session kept, that no fresh session was made, and
// that the controller's own fresh_session or resume_agent starts the agent once the gate opens
// (api's itsSuspensionStands), and not that the suspension was taken over. The claim stays
// suspended by the controller with its session through the outbox's passes, which run the close's
// stop, and through the opening, which launches nothing; the controller's fresh_session after the
// opening then starts the agent and leaves that session behind.
func TestWorkHoldAnswersTheControllersFreshSessionOfALiveWorkerWhoseForgetTheCloseRefuses(t *testing.T) {
	t.Parallel()
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	gate := closingGate(r)
	r.live(claim.RoleImplementer)
	r.close(4)
	if c := r.machine(claim.RoleImplementer).Claim(); (c.State != supervise.StateReady && c.State != supervise.StateIdle) || c.Session != heldSession {
		t.Fatalf("before the controller's fresh_session the implementer is %s on session %q, want it ready or idle on %q",
			c.State, c.Session, heldSession)
	}

	said := refusedOnceTheGateCloses(r, gate, agentFreshSession, claim.RoleImplementer, 1)

	token := r.token(claim.RoleImplementer)
	stands := "a fresh session by the controller went no further than its own suspension of the agent, which stands, " +
		"with the session its claim recorded, and no fresh session was made: the hold's stop does not take that " +
		"suspension over, so the gate's opening does not start the agent, and the controller's own fresh_session or " +
		"resume_agent, each refused while the gate is closed, starts it once the gate opens"
	if want := heldByTheWorkHold(token) + stands + theTwoWaysOut(token); said != want {
		t.Fatalf("the refusal says\n%q\nwant\n%q", said, want)
	}
	wantSuspendedByTheController(r, "after the refused fresh_session")
	r.drain()
	wantSuspendedByTheController(r, "after the outbox's passes that run the close's stop")

	launched := r.launches(claim.RoleImplementer)
	r.approve(4, 12)
	r.drain()
	wantSuspendedByTheController(r, "after the opening")
	if got := r.launches(claim.RoleImplementer); got != launched {
		t.Fatalf("the opening launched the implementer %d times, want none", got-launched)
	}

	recorder := askTheControllerDoor(r, agentFreshSession, claim.RoleImplementer)
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
		t.Fatalf("the controller's fresh_session once the gate opened = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
	}
	// The answer came once the start was under way: the machine's claim is read through the
	// decision that runs it, so the start has ended before anything is counted.
	r.machine(claim.RoleImplementer).Claim()
	r.drain()
	if got := r.launches(claim.RoleImplementer); got <= launched {
		t.Fatalf("the controller's fresh_session once the gate opened launched the implementer %d times, want it started", got-launched)
	}
	c := r.machine(claim.RoleImplementer).Claim()
	if c.State == supervise.StateSuspended || supervise.FailedOrRetired(c.State) || c.Abandoned.Session != heldSession {
		t.Fatalf("after the controller's fresh_session once the gate opened the implementer is %s by %q, having left behind %q; want it started, having left %q behind",
			c.State, c.SuspendedBy, c.Abandoned.Session, heldSession)
	}
}

// wantSuspendedByTheController fails the test where the implementer's claim, in its machine or in
// the store, is not suspended by the controller on the session it was ready in, or is held by the
// work hold; when names the moment the claim is read at.
func wantSuspendedByTheController(r *workHoldRig, when string) {
	r.t.Helper()
	for _, held := range []struct {
		where string
		claim supervise.Claim
	}{
		{"its machine", r.machine(claim.RoleImplementer).Claim()},
		{"the store", r.storedClaimOf(claim.RoleImplementer)},
	} {
		c := held.claim
		if c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByController || c.WorkHeld() ||
			c.Session != heldSession || c.SessionFile != heldSessionFile {
			r.t.Fatalf("%s the implementer's claim in %s is %s by %q on session %q (%s), WorkHeld %v; want it suspended by the controller on %q (%s)",
				when, held.where, c.State, c.SuspendedBy, c.Session, c.SessionFile, c.WorkHeld(), heldSession, heldSessionFile)
		}
	}
}

// startRefusedAfterTheForget is the controller's fresh session of the implementer walk leaves
// holding its session, with the gate open at the agent door's read and at the forget's, and
// closed at the read the start makes once the forget has left that session behind. The forget has
// changed the claim, so the refused start is answered as what the request did (api's
// refusedAfterTheForget, dispatch://INFRA-5649 R2): to a caller at this daemon's contract, 200
// with phaseNotRestarted naming the session left behind, what holds the claim and the hold's two
// ways out; to a caller below the claim start entry, 20261127094305, as before, 409
// HELD_BY_WORK_HOLD with those two ways out, saying the session was left behind. Either way the
// claim stands as it stood but with no session, recording the one it left behind, no start row
// is written, and the outbox's passes after start nothing.
func startRefusedAfterTheForget(t *testing.T, walk func(r *workHoldRig)) {
	t.Helper()
	for _, caller := range []struct {
		name     string
		contract int
	}{
		{"at this daemon's contract", 0},
		{"below the claim start entry", belowClaimStart(t)},
	} {
		t.Run(caller.name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			gate := closingGate(r)
			walk(r)
			before, root := standing(r, claim.RoleImplementer), r.issue()
			if before.machine.Session != heldSession || before.stored.Session != heldSession {
				t.Fatalf("the implementer records the session %q, stored %q, want %q for the forget to leave behind",
					before.machine.Session, before.stored.Session, heldSession)
			}

			recorder := askedOnceTheGateCloses(r, gate, agentFreshSession, claim.RoleImplementer, 2, caller.contract)

			token := r.token(claim.RoleImplementer)
			if caller.contract == 0 {
				var answer api.FreshSessionResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK {
					t.Fatalf("the controller's fresh_session, its start refused after the forget, = %d, want 200; body %s", recorder.Code, recorder.Body)
				}
				left := string(token) + " left its recorded session " + heldSession + " behind"
				hold := workHoldWords + theTwoWaysOut(token)
				if said := answer.PhaseNotRestarted; !strings.Contains(said, left) || !strings.Contains(said, hold) {
					t.Fatalf("phaseNotRestarted says\n%q\nwant it to name the session left behind\n%q\nand the hold with its two ways out\n%q", said, left, hold)
				}
			} else {
				said := wantHeldByTheWorkHold(r, agentFreshSession, claim.RoleImplementer, recorder)
				// The words between these name what the session was left behind before, which are
				// the outcome's own (api's sessionLeftBehind) and not this case's to pin.
				head := heldByTheWorkHold(token) + "a fresh session by the controller went no further than leaving behind the session the claim recorded"
				tail := "so the agent starts in a new session once it is started" + theTwoWaysOut(token)
				if !strings.HasPrefix(said, head) || !strings.HasSuffix(said, tail) {
					t.Fatalf("the refusal says\n%q\nwant it to start\n%q\nand end\n%q", said, head, tail)
				}
			}
			leftBehind := before
			leftBehind.machine, leftBehind.stored = withoutSession(before.machine), withoutSession(before.stored)
			wantNothingChangedSince(r, "the controller's fresh_session refused after its forget", claim.RoleImplementer, leftBehind, root)
			for _, held := range []struct {
				where string
				claim supervise.Claim
			}{
				{"its machine", r.machine(claim.RoleImplementer).Claim()},
				{"the store", r.storedClaimOf(claim.RoleImplementer)},
			} {
				if got := held.claim.Abandoned; got.Session != heldSession || got.SessionFile != heldSessionFile {
					t.Fatalf("after the refused fresh_session the implementer's claim in %s records it left behind the session %q (%s), want %q (%s)",
						held.where, got.Session, got.SessionFile, heldSession, heldSessionFile)
				}
			}
			wantNothingStartedByTheOutbox(r, "the fresh_session refused after its forget", claim.RoleImplementer, leftBehind)
		})
	}
}

// belowClaimStart is the contract of a caller below the claim start entry, 20261127094305 (api's
// claimStartContract): the number before the one this build gives that entry
// (api.NumberedEntries).
func belowClaimStart(t *testing.T) int {
	t.Helper()
	for _, entry := range api.NumberedEntries() {
		if entry.ID == 20261127094305 {
			return entry.Number - 1
		}
	}
	t.Fatal("this build numbers no claim start entry, 20261127094305")
	return 0
}
