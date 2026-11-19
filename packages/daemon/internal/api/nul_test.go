package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// PostgreSQL refuses a NUL and a byte that is not UTF-8 in text (SQLSTATE 22021), and a \u0000
// escape in jsonb (22P05). Text the daemon stores from a worker, a runtime or a fact is stored
// with U+FFFD in their place instead, and the write that carries it is taken. A request body that
// holds a NUL is refused by the API's decoder before its route reads it (decodeJSON), so a test of
// such a route hands the fact or the event the route would build to the daemon below the API.
// Every test here writes through the harness's store, which store.Open built.

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

// plantedQueryToken is a credential a far side's text carries in a URL's query, which the claim
// and the store keep the text without (storetext.FarSideText).
const plantedQueryToken = "planted-query-token-5c1e"

// listedClaim is token's claim as the operator's listing answers it: the machine's own claim, as
// GET /legion/v1/operator/claims serves it.
func listedClaim(t *testing.T, h *harness, token claim.Token) OperatorClaim {
	t.Helper()
	recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list the claims = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var list OperatorClaims
	decodeInto(t, recorder, &list)
	for _, c := range list.Claims {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("the listing carries no %s: %s", token, recorder.Body)
	return OperatorClaim{}
}

// A leak-check cause the launcher wrote with a NUL and a URL that carries a token is held by the
// claim as the store holds it: the operator's listing, which reads the machine's claim, answers
// the stored cause, with U+FFFD for the NUL and the URL without its query, before any restart.
func TestALeakCheckCauseWithNULAndAQueryTokenIsHeldInMemoryAsItIsStored(t *testing.T) {
	h := newHarness(t)
	cause := "the shared public leak check could not be installed: the launcher answered \x00 reading " +
		"https://codeload.example.test/legion/tar.gz/v3?token=" + plantedQueryToken + " for its slice"
	const want = "the shared public leak check could not be installed: the launcher answered \uFFFD reading " +
		"https://codeload.example.test/legion/tar.gz/v3 for its slice"
	refusal := fmt.Errorf("start through its authenticated launcher: %w", runtime.LeakCheckRefusal{Cause: cause})

	stored := refusedStart(t, h, "LEGION-209", refusal)

	if stored.LeakCheckCause != want {
		t.Errorf("the stored leak-check cause is %q, want %q", stored.LeakCheckCause, want)
	}
	if listed := listedClaim(t, h, stored.Token).LeakCheckCause; listed != stored.LeakCheckCause {
		t.Errorf("the operator's listing answers the leak-check cause %q, want %q, the cause as stored", listed, stored.LeakCheckCause)
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

// phaseFinishedSummaries is the summary of each phase-finished notice of role's completion on
// issue queued in the outbox, in no order.
func phaseFinishedSummaries(t *testing.T, h *harness, issue string, role claim.Role) []string {
	t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `select payload->>'summary' from outbox
		where issue = $1 and kind = 'notice' and payload->>'kind' = 'phase-finished' and payload->>'role' = $2`,
		issue, string(role))
	if err != nil {
		t.Fatalf("read the %s's phase-finished notices on %s: %v", role, issue, err)
	}
	summaries, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the %s's phase-finished notices on %s: %v", role, issue, err)
	}
	return summaries
}

// A completion whose summary holds a NUL is refused by the decoder of its body, 400 naming
// summary, before the route reads it, and nothing of it is recorded: the issue is still in
// testing, the tester's phase row holds no completion, and no phase-finished notice is queued for
// the architect. The same completion applied below the API stores the summary with U+FFFD
// (TestHandoffCompleteFactBelowTheAPIStoresASummaryWithNULAsReplacementCharacter).
func TestHandoffCompleteRefusesASummaryWithNULAndRecordsNothing(t *testing.T) {
	h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)

	wantRefusal(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass\x00 at the head", Verdict: "pass",
		Commit: headSHA, HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusBadRequest,
		"invalid request body: "+nulError("summary", 11))

	assertIssuePhase(t, h, phase.Testing)
	if row := phaseRowOf(t, h, "LEGION-208", claim.RoleTester); row != nil && (row.Summary != "" || row.HandoffRecord != "") {
		t.Fatalf("the tester's row after the refusal = %+v, want no completion recorded on it", row)
	}
	if notices := phaseFinishedSummaries(t, h, "LEGION-208", claim.RoleTester); len(notices) != 0 {
		t.Fatalf("the tester's phase-finished notices after the refusal carry the summaries %q, want none queued", notices)
	}
}

// The completion of a summary that holds a NUL, applied below the API as the completion route
// applies it (intake.ApplyFact on the harness's pool, with the workflow deciding it), is taken with
// no error: the tester's phase row and the phase-finished notice it queues for the architect hold
// the summary with U+FFFD.
func TestHandoffCompleteFactBelowTheAPIStoresASummaryWithNULAsReplacementCharacter(t *testing.T) {
	h, engine, _ := newEntryHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatalf("no machine for %s", tester.token)
	}
	c := machine.Claim()
	handoff := tester.writeHandoff("test", 1)
	const want = "tests pass\uFFFD at the head"

	// The fact and its key as the completion route builds them (handoff.go): the run the claim
	// serves, and the key's tree generation and generation 1, as LEGION-208 is seeded, and pass 0,
	// since no implementer's row raised it.
	fact := intake.HandoffComplete{Generation: c.ServingRun(), Issue: "LEGION-208", Role: claim.RoleTester, Claim: tester.token,
		Summary: "tests pass\x00 at the head", Verdict: "pass", Commit: headSHA, HandoffRecord: handoff, HandoffVersion: 1,
		MustRate: c.PluginContract >= PlanRatingContract}
	eventID := fmt.Sprintf("handoff:LEGION-208:1:1:%d:%s:%s:0:%s:pass", c.ServingRun(), claim.RoleTester, phase.Testing, handoff)
	result, err := intake.ApplyFact(h.ctx, h.store.Pool(), "api", eventID, fact, engine)
	if err != nil || result.Refusal != nil || result.Duplicate {
		t.Fatalf("apply the tester's completion = %+v, %v; want it taken", result, err)
	}

	if row := phaseRowOf(t, h, "LEGION-208", claim.RoleTester); row == nil || row.Summary != want {
		t.Fatalf("the tester's row = %+v, want its summary stored as %q", row, want)
	}
	if notices := phaseFinishedSummaries(t, h, "LEGION-208", claim.RoleTester); len(notices) != 1 || notices[0] != want {
		t.Fatalf("the tester's phase-finished notices carry the summaries %q, want one of %q", notices, want)
	}
}

// The operator's delivery of a task that holds a NUL is refused by the decoder of its body, 400
// naming task, and queues nothing. The same delivery handed to the claim's machine below the API,
// as the deliver route hands it (operator.go's deliverEvent), is stored with U+FFFD in its place.
func TestDeliverRefusesATaskWithNULThatTheMachineStoresAsReplacementCharacter(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	machine, ok := h.supervisor.Machine(architectToken)
	if !ok {
		t.Fatalf("no machine for %s", architectToken)
	}

	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/deliver",
		DeliverRequest{Task: "Review\x00 the plan."}), http.StatusBadRequest, "invalid request body: "+nulError("task", 7))
	var queued int
	if err := h.store.Pool().QueryRow(h.ctx, `select count(*) from pending_task_deliveries where claim_token = $1`,
		string(architectToken)).Scan(&queued); err != nil {
		t.Fatalf("count the stored deliveries: %v", err)
	}
	if pending := machine.Claim().Pending; queued != 0 || pending != nil {
		t.Fatalf("after the refusal the store holds %d deliveries and the claim holds pending %+v, want neither", queued, pending)
	}

	if err := machine.Handle(h.ctx, supervise.RequestDeliver{Claim: architectToken, Task: "Review\x00 the plan."}); err != nil {
		t.Fatalf("deliver the task below the API: %v", err)
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
// so the operator's listing and the prompt the agent is sent before any restart read it too; the
// deliver route answers a body holding a NUL with its decoder's 400, so the same delivery is handed
// to the claim's machine below the API. A claim that keeps the request's own text hands the agent,
// and the operator's answer, a NUL the store never shows.
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

	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		DeliverRequest{Task: "Review\x00 the plan."}), http.StatusBadRequest, "invalid request body: "+nulError("task", 7))
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if err := machine.Handle(h.ctx, supervise.RequestDeliver{Claim: token, Task: "Review\x00 the plan."}); err != nil {
		t.Fatalf("deliver below the API: %v", err)
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
	if listed := listedClaim(t, h, token); listed.Pending == nil || listed.Pending.Task != want {
		t.Errorf("the operator's listing answers the pending task %+v, want %q, the task as stored", listed.Pending, want)
	}
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].Message != want {
		t.Errorf("the agent was sent %q, want one prompt of %q, the task as stored", prompts, want)
	}
}

// A refused turn's message, the provider's words with a NUL and a URL that carries a token, is
// held in the claim's window of turn outcomes as the store holds it: U+FFFD for the NUL and the
// URL without its query, so the refusal the claim's failure would name is the stored one.
func TestARefusedTurnsMessageWithNULAndAQueryTokenIsHeldInMemoryAsItIsStored(t *testing.T) {
	h := newHarness(t)
	token, _ := h.launch("LEGION-209", claim.RoleImplementer)
	h.ready(token, "ses_implementer", 1)
	m, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	message := "the content filter refused\x00 the turn; see https://provider.example.test/refusals?token=" + plantedQueryToken + " for why"
	const want = "the content filter refused\uFFFD the turn; see https://provider.example.test/refusals for why"

	if err := m.Handle(h.ctx, supervise.RequestTurnOutcome{Claim: token, Generation: m.Claim().Generation, Sequence: 1,
		Refused: true, Message: message}); err != nil {
		t.Fatalf("the refused turn's outcome: %v", err)
	}

	stored, held := h.stored(token).TurnOutcomes, m.Claim().TurnOutcomes
	if len(stored) != 1 || len(held) != 1 {
		t.Fatalf("the store holds the turn outcomes %+v and the claim %+v, want the one refused turn in each", stored, held)
	}
	if held[0].Message != stored[0].Message {
		t.Errorf("the claim holds the refused turn's message %q, want %q, the message as stored", held[0].Message, stored[0].Message)
	}
	if stored[0].Message != want {
		t.Errorf("the stored refused turn's message is %q, want %q", stored[0].Message, want)
	}
}

// An update round's reasons, which quote what the runtime read of the pod, and the head the
// worker answered it with are held by the claim as the store holds them, U+FFFD for each NUL:
// the operator's listing answers the stored head, and the claim holds the stored reasons.
func TestAnUpdateRoundsReasonsAndHeadWithNULAreHeldInMemoryAsTheyAreStored(t *testing.T) {
	h := newHarness(t)
	token, _ := h.launch("LEGION-209", claim.RoleImplementer)
	h.ready(token, "ses_implementer", 1)
	m, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	round := supervise.PodRound("pod-uid-209")
	const reasons, head = "image legion:old\x00, now legion:new", "9f2b7c0d\x001e3a4b5c"
	const wantReasons, wantHead = "image legion:old\uFFFD, now legion:new", "9f2b7c0d\uFFFD1e3a4b5c"

	if err := m.Handle(h.ctx, supervise.PodUpdateOpen{Claim: token, Round: round, Reasons: reasons}); err != nil {
		t.Fatalf("open the update round: %v", err)
	}
	if err := m.Handle(h.ctx, supervise.PodUpdateSignalled{Claim: token, Generation: m.Claim().Generation,
		Session: "ses_implementer", Round: round, Head: head}); err != nil {
		t.Fatalf("the worker's answer to the round: %v", err)
	}

	stored := h.stored(token)
	if stored.UpdateReasons != wantReasons || stored.UpdateHead != wantHead {
		t.Fatalf("the stored round names reasons %q and head %q, want %q and %q", stored.UpdateReasons, stored.UpdateHead, wantReasons, wantHead)
	}
	if held := m.Claim().UpdateReasons; held != stored.UpdateReasons {
		t.Errorf("the claim holds the round's reasons %q, want %q, the reasons as stored", held, stored.UpdateReasons)
	}
	if listed := listedClaim(t, h, token).UpdateHead; listed != stored.UpdateHead {
		t.Errorf("the operator's listing answers the head %q, want %q, the head as stored", listed, stored.UpdateHead)
	}
}
