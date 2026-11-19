package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// PostgreSQL refuses a NUL and a byte that is not UTF-8 in text (SQLSTATE 22021), and a \u0000
// escape in jsonb (22P05). Text the daemon stores from a worker, a runtime or a request is stored
// with U+FFFD in their place instead, and the write that carries it is taken. Every test here
// writes through the harness's store, which store.Open built.

// storeRefusal is the server's refusal of a statement in err's chain, and "" when err carries
// none. A launch the runtime refused answers an error whatever the store did with the claim; what
// this tells apart is a write of the claim the store refused.
func storeRefusal(err error) string {
	var refused *pgconn.PgError
	if errors.As(err, &refused) {
		return refused.Error()
	}
	return ""
}

// storableText fails the test unless got is text PostgreSQL takes: valid UTF-8 with no NUL.
func storableText(t *testing.T, what, got string) {
	t.Helper()
	if !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
		t.Fatalf("the stored %s is %q, want valid UTF-8 with no NUL", what, got)
	}
}

// refusedStart is a phase worker's claim on issue whose every launch the runtime refused with
// refusal, created and launched as the harness's launchIn does, and the claim the store holds for
// it once the launch budget ran out.
func refusedStart(t *testing.T, h *harness, issue string, refusal error) supervise.Claim {
	t.Helper()
	token, err := claim.NewToken(testProject, issue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.OpenTreeLifecycle(h.ctx, testProject, issue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", issue, err)
	}
	m, _, err := h.supervisor.Create(h.ctx, supervise.Claim{
		Token: token, Project: testProject, Tree: issue, Issue: issue, Role: claim.RoleImplementer, State: supervise.StateQueued,
	}, "role prompt")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	refused := fake.SpawnResult{Err: refusal}
	h.runtime.ScriptSpawn(refused, refused, refused)

	launched := m.Handle(h.ctx, supervise.RequestSpawn{Claim: token})

	if refusedWrite := storeRefusal(launched); refusedWrite != "" {
		t.Fatalf("the store refused a write of %s: %s", token, refusedWrite)
	}
	if launched == nil {
		t.Fatalf("the launches of %s the runtime refused answered no error", token)
	}
	stored := h.stored(token)
	if stored.State != supervise.StateFailed {
		t.Fatalf("%s is %s, want it failed: its launch budget ran out on the refusals", token, stored.State)
	}
	return stored
}

// The agent's tool step is stored with U+FFFD where its tool name and its call id held a NUL.
func TestAStoredStepHoldsReplacementCharacterForNULInItsToolAndCallID(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	conn := fake.NewConn()
	h.conns.Register(architectToken, conn)
	h.ready(architectToken, "ses_architect", 1)
	m, supervised := h.supervisor.Machine(architectToken)
	if !supervised {
		t.Fatal("the spawned claim has no machine: this case is about the step its machine writes")
	}

	if err := m.Handle(h.ctx, supervise.StreamToolStep{Claim: architectToken, Tool: "ba\x00sh",
		CallID: "toolu_turn7\x00_2", ArgsBytes: 128, ArgsHash: "9f2b7c0d1e3a4b5c", ConnSequence: conn.Sequence()}); err != nil {
		t.Fatalf("the agent's tool step: %v", err)
	}

	stored, err := h.store.ClaimStep(h.ctx, architectToken)
	if err != nil {
		t.Fatalf("read the stored step: %v", err)
	}
	if stored.Tool != "ba\uFFFDsh" || stored.CallID != "toolu_turn7\uFFFD_2" {
		t.Fatalf("the stored step names tool %q and call %q, want %q and %q", stored.Tool, stored.CallID, "ba\uFFFDsh", "toolu_turn7\uFFFD_2")
	}
}

// A start the runtime refused with a NUL and a lone 0xff in its text is stored with U+FFFD for
// each, as the claim's last start failure and in the failure its spent budget wrote.
func TestAStartRefusalWithNULAndAnInvalidByteIsStoredWithReplacementCharacters(t *testing.T) {
	h := newHarness(t)
	const cleaned = "the launcher refused the start: it answered \uFFFD and \uFFFD"

	stored := refusedStart(t, h, "LEGION-209", errors.New("the launcher refused the start: it answered \x00 and \xff"))

	storableText(t, "last start failure", stored.LastStartFailure)
	storableText(t, "failure", stored.Failure)
	if stored.LastStartFailure != cleaned {
		t.Errorf("the stored last start failure is %q, want %q", stored.LastStartFailure, cleaned)
	}
	if !strings.Contains(stored.Failure, cleaned) {
		t.Errorf("the stored failure is %q, want it to name the refusal %q", stored.Failure, cleaned)
	}
}

// A start refusal of a megabyte, as a relayed command's whole stderr is, is stored as the bounded
// far-side text: its first words, U+FFFD for its NUL and its lone 0xff, cut to 2048 runes ending
// with an ellipsis.
func TestAMegabyteStartRefusalWithNULIsStoredBoundedTo2048Runes(t *testing.T) {
	h := newHarness(t)
	const line = "the launcher's stderr goes on, line after line.\n"
	const first = "the launcher refused the start: it answered \x00 and \xff\n"
	refusal := first + strings.Repeat(line, (1<<20)/len(line)+1)

	stored := refusedStart(t, h, "LEGION-209", errors.New(refusal))

	got := stored.LastStartFailure
	storableText(t, "last start failure", got)
	if runes := utf8.RuneCountInString(got); runes != 2048 || !strings.HasSuffix(got, "…") {
		t.Errorf("the stored last start failure is %d runes ending %q, want 2048 runes ending with …", runes, got[max(0, len(got)-16):])
	}
	if want := "the launcher refused the start: it answered \uFFFD and \uFFFD\n"; !strings.HasPrefix(got, want) {
		t.Errorf("the stored last start failure starts %q, want the refusal's first words %q", got[:min(len(got), len(want))], want)
	}
}

// A start the shared public leak check refused, with a NUL in its cause, is stored with U+FFFD
// in the claim's leak-check cause.
func TestALeakCheckRefusalWithNULIsStoredWithReplacementCharacter(t *testing.T) {
	h := newHarness(t)
	const cause = "the shared public leak check could not be installed: the launcher answered \x00 for its slice"
	refusal := fmt.Errorf("start through its authenticated launcher: %w", runtime.LeakCheckRefusal{Cause: cause})

	stored := refusedStart(t, h, "LEGION-209", refusal)

	storableText(t, "leak-check cause", stored.LeakCheckCause)
	if want := strings.ReplaceAll(cause, "\x00", "\uFFFD"); stored.LeakCheckCause != want {
		t.Errorf("the stored leak-check cause is %q, want %q", stored.LeakCheckCause, want)
	}
}

// A death whose runtime account holds a NUL and a lone 0xff is stored with U+FFFD for each as the
// claim's last death cause, and the death's own write of the claim is taken.
func TestADeathDetailWithNULAndAnInvalidByteIsStoredWithReplacementCharacters(t *testing.T) {
	h := newHarness(t)
	token, _ := h.launch("LEGION-209", claim.RoleImplementer)
	h.ready(token, "ses_implementer", 1)
	m, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	gone := runtime.Observation{Locator: *m.Claim().Locator, Kind: runtime.Gone,
		Detail: "pod legion-legion-209 ended: the node was drained\nlast log line: \x00 then \xff"}

	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: gone}); err != nil {
		t.Fatalf("the death of %s: %v", token, err)
	}

	stored := h.stored(token)
	storableText(t, "last death cause", stored.LastDeathCause)
	if want := "pod legion-legion-209 ended: the node was drained\nlast log line: \uFFFD then \uFFFD"; !strings.Contains(stored.LastDeathCause, want) {
		t.Errorf("the stored last death cause is %q, want it to hold the runtime's account %q", stored.LastDeathCause, want)
	}
}

// A completion whose summary holds a NUL is answered as a clean one is: the tester's phase row
// and the phase-finished notice it queues for the architect hold the summary with U+FFFD.
func TestHandoffCompleteStoresASummaryWithNULAsReplacementCharacter(t *testing.T) {
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	const want = "tests pass\uFFFD at the head"

	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass\x00 at the head", Verdict: "pass",
		Commit: headSHA, HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))

	if row := phaseRowOf(t, h, "LEGION-208", claim.RoleTester); row == nil || row.Summary != want {
		t.Fatalf("the tester's row = %+v, want its summary stored as %q", row, want)
	}
	var notice string
	if err := h.store.Pool().QueryRow(h.ctx, `select payload->>'summary' from outbox
		where issue = $1 and kind = 'notice' and payload->>'kind' = 'phase-finished' and payload->>'role' = $2`,
		"LEGION-208", string(claim.RoleTester)).Scan(&notice); err != nil {
		t.Fatalf("read the tester's phase-finished notice: %v", err)
	}
	if notice != want {
		t.Fatalf("the phase-finished notice carries summary %q, want %q", notice, want)
	}
}

// The operator's delivery of a task that holds a NUL is answered as a clean one is, and the task
// is stored with U+FFFD in its place.
func TestDeliverStoresATaskWithNULAsReplacementCharacter(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/deliver",
		DeliverRequest{Task: "Review\x00 the plan."})

	if recorder.Code != http.StatusOK {
		t.Fatalf("deliver = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var got OperatorClaim
	decodeInto(t, recorder, &got)
	if got.Pending == nil || !strings.HasPrefix(got.Pending.Task, "Review") || !strings.HasSuffix(got.Pending.Task, " the plan.") {
		t.Fatalf("deliver answered pending %+v, want the task", got.Pending)
	}
	var stored string
	if err := h.store.Pool().QueryRow(h.ctx, `select task from pending_task_deliveries where claim_token = $1`,
		string(architectToken)).Scan(&stored); err != nil {
		t.Fatalf("read the stored delivery: %v", err)
	}
	if want := "Review\uFFFD the plan."; stored != want {
		t.Fatalf("the stored task is %q, want %q", stored, want)
	}
}

// The operator's delivery of a task that holds a NUL reaches the agent as the task is stored. The
// stored delivery reads U+FFFD where the request had the NUL, and a restart reads that copy back,
// so the deliver's answer and the prompt the agent is sent before any restart read it too. A claim
// that keeps the request's own text hands the agent, and the operator's answer, a NUL the store
// never shows.
func TestADeliveredTaskWithNULReachesTheAgentAsItIsStored(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleTester)
	registered := h.registered(boot, "ses_tester")
	conn := connected(h, token)
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_tester", Secret: registered.Secret, Generation: registered.Generation,
	}, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("ready = %d %q, want 204", recorder.Code, recorder.Body)
	}
	settled(h, token)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		DeliverRequest{Task: "Review\x00 the plan."})
	if recorder.Code != http.StatusOK {
		t.Fatalf("deliver = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	settled(h, token)

	const want = "Review\uFFFD the plan."
	var stored string
	if err := h.store.Pool().QueryRow(h.ctx, `select task from pending_task_deliveries where claim_token = $1`,
		string(token)).Scan(&stored); err != nil {
		t.Fatalf("read the stored delivery: %v", err)
	}
	if stored != want {
		t.Fatalf("the stored task is %q, want %q", stored, want)
	}
	var answered OperatorClaim
	decodeInto(t, recorder, &answered)
	if answered.Pending == nil || answered.Pending.Task != want {
		t.Errorf("the deliver answered the pending task %+v, want %q, the task as stored", answered.Pending, want)
	}
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].Message != want {
		t.Errorf("the agent was sent %q, want one prompt of %q, the task as stored", prompts, want)
	}
}
