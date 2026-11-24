package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The tests of dispatch://INFRA-5649: what the operator's and the controller's claim routes answer
// when the start they ask for waits for an admission slot (R1), when the start is refused after a
// fresh session has left the claim's session behind (R2), when the caller is below the contract
// that changed those answers (R3), and while the start a retry or a resume runs has not ended (R4).

// slotBaseContract is the contract main declared before dispatch://INFRA-5649 (version.go's 220):
// a caller there reads the answers it read before.
const slotBaseContract = 220

// slotNewContract is the contract this change raises the daemon to (claimStartContract), written
// as its number so a caller stating it reads the same at the base, where it is above that
// daemon's own contract and is served the base's answers. A renumber of the change moves this
// number with the constant; one that forgets to fails TestACallerBelowTheNewContractIsAnsweredAsBefore,
// since a caller here would then be below the new contract.
const slotNewContract = 266

// waitingTree records LEGION-208, the root, standing in planning, so the workflow's restart of a
// worker on its child LEGION-209 is not refused for a tree that lingers.
func waitingTree(t *testing.T, h *harness) {
	t.Helper()
	root := "LEGION-208"
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
		Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})
}

// failedPhaseWorker is LEGION-209's worker on role, registered and ready, with the issue standing in the
// phase at, then failed: its process was found gone and the runtime refused every relaunch. The
// issue holds no admission slot unless the case puts one there (slotOf).
func failedPhaseWorker(t *testing.T, h *harness, role claim.Role, at phase.Phase) claim.Token {
	t.Helper()
	waitingTree(t, h)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", role)
	h.ready(token, "ses_"+string(role), 1)
	workingIssue(t, h, at)
	refused := fake.SpawnResult{Err: errors.New("the runtime refused the launch")}
	h.runtime.ScriptResume(refused, refused, refused)
	m, _ := h.supervisor.Machine(token)
	loc := m.Claim().Locator
	if loc == nil {
		t.Fatalf("the ready %s runs no process", role)
	}
	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *loc, Kind: runtime.Gone}}); err == nil {
		t.Fatal("the relaunches the runtime refused answered no error")
	}
	if got := h.stored(token); got.State != supervise.StateFailed || got.Session != "ses_"+string(role) {
		t.Fatalf("the %s is %s recording session %q, want it failed on ses_%s", role, got.State, got.Session, role)
	}
	return token
}

// slotOf gives issue an admission slot, as admission's promotion does.
func slotOf(t *testing.T, h *harness, issue string) {
	t.Helper()
	if err := store.Transact(h.ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutSlot(ctx, tx, record.Slot{Issue: issue, Index: 0, AdmittedAt: time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)})
	}); err != nil {
		t.Fatalf("give %s a slot: %v", issue, err)
	}
}

// blockedBy has the mirror of Dispatch say issue is blocked by blocker, which is in progress.
func blockedBy(t *testing.T, h *harness, issue, blocker string) {
	t.Helper()
	if err := store.Transact(h.ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: blocker, Project: testDispatchProject, Seq: 1,
			Status: record.Carry("in_progress"), Parent: record.Carry(""), Blockers: record.Carry[[]string](nil)}); err != nil {
			return err
		}
		return records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: issue, Project: testDispatchProject, Seq: 1,
			Status: record.Carry("in_progress"), Parent: record.Carry("LEGION-208"), Blockers: record.Carry([]string{blocker})})
	}); err != nil {
		t.Fatalf("mirror %s blocked by %s: %v", issue, blocker, err)
	}
}

// claimableStarts is every start row of issue that a lane may run now (record.Store.DueRows).
func claimableStarts(t *testing.T, h *harness, issue string) []record.OutboxRow {
	t.Helper()
	var due []record.OutboxRow
	if err := store.Transact(h.ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		due, err = record.NewStore().DueRows(ctx, tx, testDispatchProject, issue, []record.OutboxKind{record.OutboxKindSupervise}, time.Now().Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("read the due rows of %s: %v", issue, err)
	}
	var starts []record.OutboxRow
	for _, row := range due {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode the due row %d of %s: %v", row.ID, issue, err)
		}
		if request, ok := payload.(record.SuperviseRequest); ok && request.Op == "start" {
			starts = append(starts, row)
		}
	}
	return starts
}

// stating is the header of a caller that states contract as its plugin's.
func stating(contract int) http.Header {
	return http.Header{pluginContractHeader: {strconv.Itoa(contract)}}
}

// R1: a fresh session of a failed tester whose issue stands in testing, a phase that keeps an
// admission slot, while the issue holds none, queues the phase's start behind that slot and runs
// nothing. Both doors name the wait, the claim stays failed with no session, and a caller at the
// base contract reads no sentence, as before.
func TestAFreshSessionWhoseStartWaitsForASlotNamesTheWait(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			h, _ := freshSessionWorkflow(t)
			token := failedPhaseWorker(t, h, claim.RoleTester, phase.Testing)
			spawns, resumes := len(h.runtime.CallsOf("Spawn")), len(h.runtime.CallsOf("Resume"))

			recorder := door.ask(t, h, "LEGION-209", claim.RoleTester, nil)

			if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
				t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
			}
			want := string(token) + " stays failed with its recorded session ses_tester left behind, and the start of its phase testing " +
				"waits for an admission slot of LEGION-209, which holds none; `read_state` (`legion claims list`) shows its state"
			if got := freshSessionNote(t, recorder); got != want {
				t.Errorf("the answer says %q, want %q", got, want)
			}
			if after := h.stored(token); after.State != supervise.StateFailed || after.Session != "" {
				t.Errorf("the claim is %s recording session %q, want it failed with no session", after.State, after.Session)
			}
			if got := len(h.runtime.CallsOf("Spawn")) + len(h.runtime.CallsOf("Resume")); got != spawns+resumes {
				t.Errorf("the runtime was asked for %d launches, want the %d made before: the start waits for the slot", got, spawns+resumes)
			}
		})
		t.Run(door.name+" stating the base contract", func(t *testing.T) {
			h, _ := freshSessionWorkflow(t)
			failedPhaseWorker(t, h, claim.RoleTester, phase.Testing)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleTester, stating(slotBaseContract))

			if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
				t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
			}
			if got := freshSessionNote(t, recorder); got != "" {
				t.Errorf("the answer to a caller at %d says %q, want nothing, as before", slotBaseContract, got)
			}
		})
	}
}

// R1: the plain `legion claims retry` of a failed tester whose issue holds no slot answers 202, as
// it did, and names the wait, with the claim still failed on the session it recorded.
func TestAPlainRetryWhoseStartWaitsForASlotNamesTheWait(t *testing.T) {
	h, _ := freshSessionWorkflow(t)
	token := failedPhaseWorker(t, h, claim.RoleTester, phase.Testing)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("the retry = %d, want 202; body %s", recorder.Code, recorder.Body)
	}
	want := string(token) + " stays failed, and the start of its phase testing " +
		"waits for an admission slot of LEGION-209, which holds none; `read_state` (`legion claims list`) shows its state"
	if got := freshSessionNote(t, recorder); got != want {
		t.Errorf("the answer says %q, want %q", got, want)
	}
	if after := h.stored(token); after.State != supervise.StateFailed || after.Session != "ses_tester" {
		t.Errorf("the claim is %s recording session %q, want it failed on the session it recorded", after.State, after.Session)
	}
}

// R1: the wait names the issues the issue waits on, read from the mirror of Dispatch.
func TestAFreshSessionWhoseStartWaitsNamesTheIssueItWaitsOn(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			h, _ := freshSessionWorkflow(t)
			token := failedPhaseWorker(t, h, claim.RoleImplementer, phase.Implementing)
			blockedBy(t, h, "LEGION-209", "LEGION-300")

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer, nil)

			if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
				t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
			}
			want := string(token) + " stays failed with its recorded session ses_implementer left behind, and the start of its phase implementing " +
				"waits for an admission slot of LEGION-209, which holds none and waits on LEGION-300; `read_state` (`legion claims list`) shows its state"
			if got := freshSessionNote(t, recorder); got != want {
				t.Errorf("the answer says %q, want %q", got, want)
			}
		})
	}
}

// R1: a suspended worker runs again at once, in a new session, and only its phase's task waits
// for the slot; the sentence says both.
func TestAFreshSessionOfASuspendedWorkerWhoseTaskWaitsForASlotSaysItRuns(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			h, _ := freshSessionWorkflow(t)
			waitingTree(t, h)
			token := readyImplementer(t, h)
			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusOK {
				t.Fatalf("suspend = %d, want 200; body %s", recorder.Code, recorder.Body)
			}

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer, nil)

			if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
				t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
			}
			want := string(token) + " is back in a new session, and the task of its phase implementing " +
				"waits for an admission slot of LEGION-209, which holds none; `read_state` (`legion claims list`) shows its state"
			if got := freshSessionNote(t, recorder); got != want {
				t.Errorf("the answer says %q, want %q", got, want)
			}
			m, _ := h.supervisor.Machine(token)
			if after := m.Claim(); after.State != supervise.StateLaunching || after.Session != "" {
				t.Errorf("the claim is %s recording session %q, want it launching in a new session", after.State, after.Session)
			}
		})
	}
}

// R1: where the issue holds its slot the start is claimable at once, and no sentence is written.
func TestAFreshSessionOfAWorkerWhoseIssueHoldsItsSlotNamesNoWait(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			h, _ := freshSessionWorkflow(t)
			failedPhaseWorker(t, h, claim.RoleTester, phase.Testing)
			slotOf(t, h, "LEGION-209")

			recorder := door.ask(t, h, "LEGION-209", claim.RoleTester, nil)

			if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
				t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
			}
			if got := freshSessionNote(t, recorder); got != "" {
				t.Errorf("the answer says %q, want nothing: the issue holds its slot", got)
			}
			if starts := claimableStarts(t, h, "LEGION-209"); len(starts) != 1 {
				t.Errorf("LEGION-209 has %d start rows a lane may run now, want the tester's one: %+v", len(starts), starts)
			}
		})
	}
}

// refusedAfterTheForget is the sentence of a fresh session whose start was refused once the
// claim's recorded session had been left behind (R2).
func refusedAfterTheForget(token claim.Token, session, refusal string, state supervise.ClaimState) string {
	return string(token) + " left its recorded session " + session + " behind for a fresh session, and its start was refused: " +
		refusal + ". It is " + string(state) + " now, with no session recorded; `read_state` (`legion claims list`) shows its state"
}

// R2: once the fresh session has left the session behind, the workflow's refusal of the restart
// changes nothing of that: the claim has changed, so both doors answer 200 with the refusal, and
// the claim stays retired with no session. It takes the place of
// TestTheOperatorsRefusedFreshSessionRetryHasAlreadyForgottenTheSession, which held the 409.
func TestARestartRefusedAfterTheForgetAnswersTheRefusal(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			facts := &heldRetrier{lingers: true}
			h, token := stoppedImplementer(t, facts)
			heldIssue(t, h, phase.Implementing)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer, nil)

			if recorder.Code != http.StatusOK {
				t.Fatalf("the fresh session = %d, want 200: the forget changed the claim; body %s", recorder.Code, recorder.Body)
			}
			want := refusedAfterTheForget(token, "ses_implementer", "LEGION-209 is still held after the retry of its phase implementing, so nothing relaunched "+
				string(token)+": its tree lingers, and a lingering tree's members stay where they stood", supervise.StateRetired)
			if got := freshSessionNote(t, recorder); got != want {
				t.Errorf("the answer says %q, want %q", got, want)
			}
			if after := h.stored(token); after.State != supervise.StateRetired || after.Session != "" || after.SessionFile != "" {
				t.Errorf("the claim is %s recording session %q (%q), want it retired with no session", after.State, after.Session, after.SessionFile)
			}
		})
	}
}

// failedRoot is the tree's root architect, spawned by the operator, registered and ready, then
// failed: its process was found gone and the runtime refused every relaunch.
func failedRoot(t *testing.T, h *harness) claim.Token {
	t.Helper()
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn the root = %d, want 201; body %s", recorder.Code, recorder.Body)
	}
	h.ready(architectToken, "ses_architect", 1)
	refused := fake.SpawnResult{Err: errors.New("the runtime refused the relaunch")}
	h.runtime.ScriptResume(refused, refused)
	m, _ := h.supervisor.Machine(architectToken)
	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *m.Claim().Locator, Kind: runtime.Gone}}); err == nil {
		t.Fatal("the relaunches the runtime refused answered no error")
	}
	if got := h.stored(architectToken); got.State != supervise.StateFailed || got.Session != "ses_architect" {
		t.Fatalf("the root is %s recording session %q, want it failed on ses_architect", got.State, got.Session)
	}
	return architectToken
}

// R2: the machine's own refusal of the retry that would start a failed architect, met after the
// fresh session left its session behind, is answered as the workflow's refusal is: 200 at both
// doors with the refusal in the sentence, and the claim failed with no session.
func TestAMachineRetryRefusedAfterTheForgetAnswersTheRefusal(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := failedRoot(t, h)
			// The tree's cleanup is reserved, so the retry's admission of the claim is refused
			// (supervise's revive, store.AdmitClaim).
			if _, err := h.store.Pool().Exec(h.ctx, "update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208'"); err != nil {
				t.Fatalf("reserve the tree's cleanup: %v", err)
			}

			recorder := door.ask(t, h, "LEGION-208", claim.RoleArchitect, nil)

			if recorder.Code != http.StatusOK {
				t.Fatalf("the fresh session = %d, want 200: the forget changed the claim; body %s", recorder.Code, recorder.Body)
			}
			note := freshSessionNote(t, recorder)
			prefix := string(token) + " left its recorded session ses_architect behind for a fresh session, and its start was refused: "
			suffix := ". It is failed now, with no session recorded; `read_state` (`legion claims list`) shows its state"
			if !strings.HasPrefix(note, prefix) || !strings.HasSuffix(note, suffix) || len(note) == len(prefix)+len(suffix) {
				t.Errorf("the answer says %q, want %q, the machine's refusal, then %q", note, prefix, suffix)
			}
			if after := h.stored(token); after.State != supervise.StateFailed || after.Session != "" {
				t.Errorf("the claim is %s recording session %q, want it failed with no session", after.State, after.Session)
			}
		})
	}
}

// R2: a claim the operator had suspended before the request is not stopped by the fresh session,
// only left without its session, and then its resume is the start: once the forget ran, the
// machine's refusal of that resume is answered as the other refusals after the forget are, 200
// at both doors with the refusal in the sentence, and the claim stays suspended with no session.
func TestAResumeRefusedAfterTheForgetOnASuspendedClaimAnswersTheRefusal(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := readyImplementer(t, h)
			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusOK {
				t.Fatalf("suspend = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
			// The tree's cleanup is reserved, so the resume's admission of the claim is refused
			// (supervise's revive, store.AdmitClaim).
			if _, err := h.store.Pool().Exec(h.ctx, "update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208'"); err != nil {
				t.Fatalf("reserve the tree's cleanup: %v", err)
			}

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer, nil)

			if recorder.Code != http.StatusOK {
				t.Fatalf("the fresh session = %d, want 200: the forget changed the claim; body %s", recorder.Code, recorder.Body)
			}
			note := freshSessionNote(t, recorder)
			prefix := string(token) + " left its recorded session ses_implementer behind for a fresh session, and its start was refused: "
			suffix := ". It is suspended now, with no session recorded; `read_state` (`legion claims list`) shows its state"
			if !strings.HasPrefix(note, prefix) || !strings.HasSuffix(note, suffix) || len(note) == len(prefix)+len(suffix) {
				t.Errorf("the answer says %q, want %q, the machine's refusal, then %q", note, prefix, suffix)
			}
			if after := h.stored(token); after.State != supervise.StateSuspended || after.Session != "" {
				t.Errorf("the claim is %s recording session %q, want it suspended with no session", after.State, after.Session)
			}
		})
	}
}

// R3: a caller below the contract that changed these answers is answered as before: no sentence
// for a start that waits for a slot, and the refusal itself, as an error, for a start refused after
// the forget. A caller at this daemon's own contract gets the new answers.
func TestACallerBelowTheNewContractIsAnsweredAsBefore(t *testing.T) {
	for _, door := range freshSessionDoors() {
		for _, contract := range []int{slotBaseContract, slotNewContract} {
			t.Run(door.name+": a start that waits for a slot, stating "+strconv.Itoa(contract), func(t *testing.T) {
				h, _ := freshSessionWorkflow(t)
				failedPhaseWorker(t, h, claim.RoleTester, phase.Testing)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleTester, stating(contract))

				if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
					t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
				}
				note := freshSessionNote(t, recorder)
				if contract == slotBaseContract && note != "" {
					t.Errorf("the answer to a caller at %d says %q, want nothing, as before", contract, note)
				}
				if contract != slotBaseContract && !strings.Contains(note, "waits for an admission slot of LEGION-209") {
					t.Errorf("the answer to a caller at %d says %q, want the wait named", contract, note)
				}
			})
			t.Run(door.name+": a restart refused after the forget, stating "+strconv.Itoa(contract), func(t *testing.T) {
				facts := &heldRetrier{lingers: true}
				h, token := stoppedImplementer(t, facts)
				heldIssue(t, h, phase.Implementing)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer, stating(contract))

				refusal := "LEGION-209 is still held after the retry of its phase implementing, so nothing relaunched " + string(token) +
					": its tree lingers, and a lingering tree's members stay where they stood"
				if contract == slotBaseContract {
					wantRefusal(t, recorder, http.StatusConflict, refusal)
					return
				}
				if recorder.Code != http.StatusOK {
					t.Fatalf("the fresh session asked at %d = %d, want 200; body %s", contract, recorder.Code, recorder.Body)
				}
				if got, want := freshSessionNote(t, recorder), refusedAfterTheForget(token, "ses_implementer", refusal, supervise.StateRetired); got != want {
					t.Errorf("the answer says %q, want %q", got, want)
				}
			})
		}
	}
}

// hungStart is a runtime whose every start of a claim hangs until the test lets it go: the start
// under Kubernetes that waits minutes on a pod. fail is what each start answers once let go, nil
// for a start that comes up.
type hungStart struct {
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func hangStarts(t *testing.T, h *harness, fail error) *hungStart {
	t.Helper()
	hung := &hungStart{entered: make(chan struct{}, 64), gate: make(chan struct{})}
	hook := func(ctx context.Context) error {
		select {
		case hung.entered <- struct{}{}:
		default:
		}
		select {
		case <-hung.gate:
			return fail
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.runtime.OnSpawn, h.runtime.OnResume = hook, hook
	t.Cleanup(hung.release)
	return hung
}

func (s *hungStart) release() { s.once.Do(func() { close(s.gate) }) }

// answerWithin is ask's answer, or a failure of the test when none comes within one second.
func answerWithin(t *testing.T, what string, ask func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- ask() }()
	select {
	case recorder := <-answered:
		return recorder
	case <-time.After(time.Second):
		t.Fatalf("%s did not answer within one second while its start hung", what)
		return nil
	}
}

// startEnded waits for the decision that runs the claim's start to end: Machine.Claim waits for
// the decision in flight.
func startEnded(h *harness, token claim.Token) supervise.Claim {
	m, _ := h.supervisor.Machine(token)
	return m.Claim()
}

// operatorLaunching is the claim an operator route answered, held to the claim written launching
// at a generation above before.
func operatorLaunching(t *testing.T, recorder *httptest.ResponseRecorder, before uint64) OperatorClaim {
	t.Helper()
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("the answer = %d, want 202: the start is under way; body %s", recorder.Code, recorder.Body)
	}
	var got OperatorClaim
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body, err)
	}
	if got.State != string(supervise.StateLaunching) || got.Generation <= before {
		t.Fatalf("the answer is %s at generation %d, want launching above generation %d", got.State, got.Generation, before)
	}
	return got
}

// R4: the operator's retry of a failed architect is the machine's own, and answers once the
// claim is written launching, while its start still hangs; the claim reaches ready once the start
// ends. At main the route waits in Machine.Handle for the whole start.
func TestARetryOfAFailedArchitectAnswersWhileItsStartRuns(t *testing.T) {
	h := newHarness(t)
	token := failedRoot(t, h)
	before := h.stored(token).Generation
	hung := hangStarts(t, h, nil)

	recorder := answerWithin(t, "the retry of the failed architect", func() *httptest.ResponseRecorder {
		return h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", nil)
	})

	got := operatorLaunching(t, recorder, before)
	hung.release()
	startEnded(h, token)
	h.ready(token, "ses_architect", got.Generation)
	if after := h.stored(token); after.State != supervise.StateReady {
		t.Errorf("the architect is %s once its start ended, want ready", after.State)
	}
}

// R4: the operator's resume of a suspended architect answers once the claim is written launching.
func TestAResumeOfASuspendedArchitectAnswersWhileItsStartRuns(t *testing.T) {
	h := newHarness(t)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn the root = %d, want 201; body %s", recorder.Code, recorder.Body)
	}
	h.ready(architectToken, "ses_architect", 1)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/suspend", nil); recorder.Code != http.StatusOK {
		t.Fatalf("suspend = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	before := h.stored(architectToken).Generation
	hung := hangStarts(t, h, nil)

	recorder := answerWithin(t, "the resume of the suspended architect", func() *httptest.ResponseRecorder {
		return h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/resume", nil)
	})

	got := operatorLaunching(t, recorder, before)
	hung.release()
	startEnded(h, architectToken)
	h.ready(architectToken, "ses_architect", got.Generation)
	if after := h.stored(architectToken); after.State != supervise.StateReady {
		t.Errorf("the architect is %s once its start ended, want ready", after.State)
	}
}

// R4: a fresh session of an architect answers once the claim is written launching, through both
// doors and both of its starts: the retry of a failed one, and the resume of an idle one the
// request suspended itself.
func TestAFreshSessionOfAnArchitectAnswersWhileItsStartRuns(t *testing.T) {
	idle := freshSessionCases()[4]
	if idle.role != claim.RoleArchitect {
		t.Fatalf("the fifth fresh-session case is %q, want the idle root architect", idle.name)
	}
	for _, door := range freshSessionDoors() {
		for _, arm := range []struct {
			name  string
			start func(t *testing.T, h *harness) claim.Token
		}{
			{"a failed root architect", failedRoot},
			{idle.name, idle.start},
		} {
			t.Run(door.name+": "+arm.name, func(t *testing.T) {
				h := freshSessionHarness(t, &phaseRestarter{})
				token := arm.start(t, h)
				before := h.stored(token).Generation
				hung := hangStarts(t, h, nil)

				recorder := answerWithin(t, "the fresh session of the architect", func() *httptest.ResponseRecorder {
					return door.ask(t, h, "LEGION-208", claim.RoleArchitect, nil)
				})

				if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
					t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
				}
				if note := freshSessionNote(t, recorder); note != "" {
					t.Errorf("the answer says %q, want nothing: the start is under way", note)
				}
				m, _ := h.supervisor.Machine(token)
				view := m.View()
				if view.State != supervise.StateLaunching || view.Generation <= before || view.Session != "" {
					t.Fatalf("the claim is %s at generation %d recording session %q, want it launching above %d in a new session",
						view.State, view.Generation, view.Session, before)
				}
				hung.release()
				startEnded(h, token)
				h.ready(token, "ses_architect_fresh", view.Generation)
				if after := h.stored(token); after.State != supervise.StateReady || after.Session != "ses_architect_fresh" {
					t.Errorf("the architect is %s on session %q once its start ended, want ready on ses_architect_fresh", after.State, after.Session)
				}
			})
		}
	}
}

// R4: an operator's task for a suspended claim revives it (supervise's deliverResuming), and the
// deliver is answered once the claim is written launching, 202, as the resume is, while the start
// hangs; the claim reaches ready once the start ends, holding the task. At main the route waited
// in Machine.Handle for the whole start.
func TestAnOperatorsDeliverToASuspendedClaimAnswersWhileItsStartRuns(t *testing.T) {
	h := newHarness(t)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn the root = %d, want 201; body %s", recorder.Code, recorder.Body)
	}
	h.ready(architectToken, "ses_architect", 1)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/suspend", nil); recorder.Code != http.StatusOK {
		t.Fatalf("suspend = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	before := h.stored(architectToken).Generation
	hung := hangStarts(t, h, nil)

	recorder := answerWithin(t, "the deliver to the suspended architect", func() *httptest.ResponseRecorder {
		return h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/deliver", DeliverRequest{Task: "Look at the tree again."})
	})

	got := operatorLaunching(t, recorder, before)
	if got.Pending == nil || got.Pending.Task != "Look at the tree again." {
		t.Errorf("the deliver answered the pending task %+v, want the operator's task", got.Pending)
	}
	hung.release()
	startEnded(h, architectToken)
	h.ready(architectToken, "ses_architect", got.Generation)
	if after := h.stored(architectToken); after.State != supervise.StateReady && after.State != supervise.StateWorking {
		t.Errorf("the architect is %s once its start ended, want it ready or at work on the task", after.State)
	}
}

// R4: a start that fails after the route answered leaves its failure in the claim's state, which
// `legion claims list` reads: the answer said the start was under way, and the state says how it
// ended.
func TestAStartThatFailsAfterTheAnswerReachesTheClaimsState(t *testing.T) {
	h := newHarness(t)
	token := failedRoot(t, h)
	before := h.stored(token).Generation
	hung := hangStarts(t, h, errors.New("the runtime refused the start"))

	recorder := answerWithin(t, "the retry of the failed architect", func() *httptest.ResponseRecorder {
		return h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", nil)
	})

	operatorLaunching(t, recorder, before)
	hung.release()
	startEnded(h, token)
	listed := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body %s", listed.Code, listed.Body)
	}
	var list OperatorClaims
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode the list %s: %v", listed.Body, err)
	}
	for _, c := range list.Claims {
		if c.Token != token {
			continue
		}
		if c.State != string(supervise.StateFailed) || !strings.Contains(c.LastStartFailure, "the runtime refused the start") || c.Failure == nil {
			t.Fatalf("the list shows %s with start failure %q and failure %+v, want it failed by the start's refusal", c.State, c.LastStartFailure, c.Failure)
		}
		return
	}
	t.Fatalf("the list holds no claim %s: %s", token, listed.Body)
}
