package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The tests of this file are this package's half of dispatch://INFRA-3144. The record of a claim
// a release left on purpose (`claimLeft`) is served by the two routes a role reads its issue from,
// at the contract entry that added it and above, and to no caller below it. And each route of this
// package whose forget leaves a claim's session behind has the claim write that leaves it queue
// the release of the Dispatch claim that session holds (store.Store.ReleaseLeftSessionClaims).

// recordedIssues is the state source of a daemon over the harness's store, cut down to what a
// role's read of `claimLeft` reads: each issue the store records, under its key, carrying the
// entry of a claim left that its record holds, read in the route's own snapshot. The daemon's own
// source projects the whole state (internal/projection), and that package imports this one, so no
// test here can run it: the projection of the field is held by internal/projection's
// TestEveryRecordFieldIsServedOrExcluded and by internal/daemon's
// TestEachClaimAReleaseLeavesIsReadInTheRecordWithItsReason, which reads it through the daemon's
// own source. What this source leaves to the code under test is the record's own statement and
// read of the entry, the two routes, and the shaping of each to the caller's contract.
type recordedIssues struct{}

func (recordedIssues) State(ctx context.Context, tx pgx.Tx) (State, []supervise.Claim, error) {
	issues, err := record.NewStore().Issues(ctx, tx)
	if err != nil {
		return State{}, nil, err
	}
	state := State{Issues: map[string]Issue{}}
	for _, issue := range issues {
		state.Issues[issue.Key] = Issue{ClaimLeft: ClaimLeftViewOf(issue.ClaimLeft)}
	}
	return state, nil, nil
}

func (recordedIssues) ObservedState(context.Context, pgx.Tx) (State, []supervise.Claim, error) {
	return State{}, nil, errors.New("no test of this file reads the observed view")
}

// serveRecords replaces the harness's server with one over the same store, supervisor and boot
// tokens whose state is recordedIssues, read in a snapshot of that store as the daemon's state
// route reads it (Options.StateTransactions). The harness's own server serves no state.
func (h *harness) serveRecords() {
	h.t.Helper()
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, State: recordedIssues{}, StateTransactions: h.store.Pool(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler
}

// leftIssue is key recorded as a root a person moved out of the workflow to the backlog, the issue
// a claim release runs for.
func leftIssue(key string) record.Issue {
	return record.Issue{Key: key, Tree: key, Project: testDispatchProject, Title: key, Phase: phase.Done, Generation: 1,
		Status: "backlog", DispatchStatus: "backlog", HandedOver: true, LastDispatchSeq: 1, Repo: "acme/widgets",
		DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// recordClaimLeft writes left as key's entry with the record's own statement, the one statement a
// claim release writes it with; nil clears it.
func recordClaimLeft(t *testing.T, h *harness, key string, left *record.ClaimLeft) {
	t.Helper()
	if err := store.Transact(h.ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().RecordClaimLeft(ctx, tx, key, left)
	}); err != nil {
		t.Fatalf("record the claim left of %s: %v", key, err)
	}
}

// leftAskedFrom is the address the reads of this file come from, one no pod of the project holds:
// a caller that states no contract from it is served this daemon's own body (contractsFor).
const leftAskedFrom = "192.0.2.1:41000"

// servedIssues is each issue of the state GET /legion/v1/state serves a caller stating contract
// (none where it is 0), decoded as plain JSON, as a plugin's reader of the state decodes it.
func servedIssues(t *testing.T, h *harness, contract int) map[string]any {
	t.Helper()
	recorder := asked(t, h.handler, leftAskedFrom, "/legion/v1/state", contract)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the state at contract %d = %d, want 200; body %s", contract, recorder.Code, recorder.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the state at contract %d: %v", contract, err)
	}
	issues, ok := body["issues"].(map[string]any)
	if !ok {
		t.Fatalf("the state at contract %d serves the issues %#v, want an object", contract, body["issues"])
	}
	return issues
}

// servedRecord is key's record as GET /legion/v1/issues/{key}/record serves a caller stating
// contract, decoded as plain JSON, as read_record hands it on to the role.
func servedRecord(t *testing.T, h *harness, key string, contract int) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal(recordOf(t, h.handler, leftAskedFrom, key, contract), &record); err != nil {
		t.Fatalf("decode the record of %s at contract %d: %v", key, contract, err)
	}
	return record
}

// wantServedClaimLeft holds issue, a body served as where says, to want: a `claimLeft` that names
// want's reason, its holder where it names one and no `holder` key where it names none, and its
// time, and no other key; and no `claimLeft` key at all where want is nil.
func wantServedClaimLeft(t *testing.T, where string, issue map[string]any, want *record.ClaimLeft) {
	t.Helper()
	served, carried := issue["claimLeft"]
	if want == nil {
		if carried {
			t.Errorf("%s carries claimLeft %v, want no claimLeft key", where, served)
		}
		return
	}
	left, ok := served.(map[string]any)
	if !carried || !ok {
		t.Errorf("%s carries claimLeft %#v, want the entry %+v", where, served, *want)
		return
	}
	keys := []string{"at", "reason"}
	if want.Holder != "" {
		keys = append(keys, "holder")
	}
	slices.Sort(keys)
	if got := slices.Sorted(maps.Keys(left)); !slices.Equal(got, keys) {
		t.Errorf("%s carries claimLeft %v with the keys %v, want %v", where, left, got, keys)
	}
	if left["reason"] != string(want.Reason) {
		t.Errorf("%s carries claimLeft.reason %v, want %q", where, left["reason"], want.Reason)
	}
	if want.Holder != "" && left["holder"] != want.Holder {
		t.Errorf("%s carries claimLeft.holder %v, want %q", where, left["holder"], want.Holder)
	}
	written, _ := left["at"].(string)
	if at, err := time.Parse(time.RFC3339Nano, written); err != nil || !at.Equal(want.At) {
		t.Errorf("%s carries claimLeft.at %v, want %s", where, left["at"], want.At.Format(time.RFC3339Nano))
	}
}

// claimLeftHolders is, for each reason a release leaves a claim for, each holder an entry of that
// reason names, as Dispatch names a claim's holder (`<kind>:<id>`): none for a claim Dispatch names
// no actor for, and for a refusal both a read Dispatch refused, which named none, and a release it
// refused, which named the holder it was asked to release.
var claimLeftHolders = map[record.ClaimLeftReason][]string{
	record.ClaimLeftUnread:       {""},
	record.ClaimLeftPerson:       {"user:alice"},
	record.ClaimLeftDaemon:       {"session:ses_daemon"},
	record.ClaimLeftController:   {"session:ses_controller"},
	record.ClaimLeftOtherTree:    {"session:ses_elsewhere"},
	record.ClaimLeftUnattributed: {"session:ses_stranger"},
	record.ClaimLeftOperatorTree: {"session:ses_operator"},
	record.ClaimLeftIssueRuns:    {"session:ses_architect"},
	record.ClaimLeftWorksNow:     {"session:ses_taken_again"},
	record.ClaimLeftHeldAnHour:   {"session:ses_architect"},
	record.ClaimLeftRefused:      {"", "session:ses_architect"},
}

// R2 of dispatch://INFRA-3144, and its Design's "a plugin that is already deployed can still read
// the answer": the reason a release left a claim is in the record a role reads, and a reader whose
// contract predates it is served a body it parses. Each issue holds the entry of one reason of
// record.ClaimLeftReasons, written by the record's own statement (record.Store.RecordClaimLeft),
// and both routes a role reads its issue from, GET /legion/v1/state and GET
// /legion/v1/issues/{key}/record, decoded as plain JSON, serve it: at the entry's contract
// (claimLeftContract), at this daemon's own and to a caller that states none, the issue carries
// `claimLeft` with the reason, the holder where Dispatch named one and no `holder` key where it
// named none, and the time; to a caller at the contract just below the entry, the issue carries no
// `claimLeft` key at all, which a strict reader of that contract would refuse. An issue whose
// record holds no entry carries no `claimLeft` at any contract, and an entry the statement clears
// (nil) is served nowhere again while every other entry stays.
//
// The control is the band taken out of the entry: the row of the contract just below the entry
// then fails, served the key its reader does not know.
func TestEachClaimAReleaseLeavesIsReadInTheRecordWithItsReasonAtEveryContract(t *testing.T) {
	h := newHarness(t)
	h.serveRecords()
	at := time.Date(2026, 11, 27, 18, 4, 5, 0, time.UTC)
	const unleft = "LEGION-399"
	h.recordIssue(leftIssue(unleft))
	entries := map[string]*record.ClaimLeft{unleft: nil}
	var person string
	next := 400
	for _, reason := range record.ClaimLeftReasons() {
		holders, named := claimLeftHolders[reason]
		if !named {
			t.Errorf("no entry here names the reason %q", reason)
			continue
		}
		for _, holder := range holders {
			key := "LEGION-" + strconv.Itoa(next)
			next++
			h.recordIssue(leftIssue(key))
			left := &record.ClaimLeft{Holder: holder, Reason: reason, At: at}
			recordClaimLeft(t, h, key, left)
			entries[key] = left
			if reason == record.ClaimLeftPerson {
				person = key
			}
		}
	}

	entry := entryNumber(claimLeftContract)
	contracts := []struct {
		name     string
		contract int
		served   bool
	}{
		{"a caller stating no contract, served this daemon's own body", 0, true},
		{"this daemon's own contract", DaemonAPIVersion(), true},
		{"the entry's contract", entry, true},
		{"the contract just below the entry", entry - 1, false},
	}
	// wantEntries reads every issue through both routes at contract, each holding the entry its
	// record holds where served, and none where not.
	wantEntries := func(t *testing.T, contract int, served bool) {
		t.Helper()
		issues := servedIssues(t, h, contract)
		for key, left := range entries {
			want := left
			if !served {
				want = nil
			}
			inState, listed := issues[key].(map[string]any)
			if !listed {
				t.Fatalf("the state at contract %d serves no issue %s: %v", contract, key, issues[key])
			}
			wantServedClaimLeft(t, fmt.Sprintf("the state's %s at contract %d", key, contract), inState, want)
			wantServedClaimLeft(t, fmt.Sprintf("the record of %s at contract %d", key, contract), servedRecord(t, h, key, contract), want)
		}
	}
	for _, tc := range contracts {
		t.Run(tc.name, func(t *testing.T) {
			wantEntries(t, tc.contract, tc.served)
		})
	}

	t.Run("an entry the record's own statement clears is served at no contract", func(t *testing.T) {
		if person == "" {
			t.Fatal("no issue here holds a person's entry to clear; this proves nothing")
		}
		recordClaimLeft(t, h, person, nil)
		entries[person] = nil
		for _, tc := range contracts {
			wantEntries(t, tc.contract, tc.served)
		}
	})
}

// leftRelease is one claim_release row the outbox holds: the issue it is keyed by and the session
// its payload names.
type leftRelease struct {
	issue, session string
}

// queuedLeftReleases is each claim_release row the harness's store holds, in id order, each
// payload decoded as the claim lane decodes it (record.DecodeOutboxPayload).
func queuedLeftReleases(t *testing.T, h *harness) []leftRelease {
	t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `select id, kind, issue, payload from outbox where kind = $1 order by id`,
		string(record.OutboxKindClaimRelease))
	if err != nil {
		t.Fatalf("read the queued claim releases: %v", err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var r record.OutboxRow
		var kind string
		err := row.Scan(&r.ID, &kind, &r.Issue, &r.Payload)
		r.Kind = record.OutboxKind(kind)
		return r, err
	})
	if err != nil {
		t.Fatalf("read the queued claim releases: %v", err)
	}
	released := []leftRelease{}
	for _, row := range queued {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode the claim release of %s as the claim lane does: %v", row.Issue, err)
		}
		release, ok := payload.(record.ClaimRelease)
		if !ok {
			t.Fatalf("the claim release row of %s decodes to %#v, want a claim release", row.Issue, payload)
		}
		released = append(released, leftRelease{issue: row.Issue, session: release.Session})
	}
	return released
}

// releaseTold is one issue the harness's store told its wake, with the number of that issue's
// claim_release rows another connection read at that moment.
type releaseTold struct {
	issue   string
	visible int
}

// releasingLeftSessions turns the harness's store's switch on, as the daemon turns it on beside its
// publication of role holders (internal/daemon's daemon.go): from here on, a claim write that
// leaves a session behind queues that session's release. It answers each issue the store has told
// its wake so far, with what another connection read of that issue's rows at each telling: a row
// queued in the write's own transaction is there once that transaction has committed.
func releasingLeftSessions(t *testing.T, h *harness) func() []releaseTold {
	t.Helper()
	var mu sync.Mutex
	told := []releaseTold{}
	h.store.ReleaseLeftSessionClaims(func(issue string) {
		var visible int
		if err := h.store.Pool().QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and issue = $2`,
			string(record.OutboxKindClaimRelease), issue).Scan(&visible); err != nil {
			t.Errorf("read the claim releases of %s when the store told it: %v", issue, err)
		}
		mu.Lock()
		defer mu.Unlock()
		told = append(told, releaseTold{issue: issue, visible: visible})
	})
	return func() []releaseTold {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(told)
	}
}

// operatorOn is one `legion claims <action>` of token's claim (the route below
// /legion/v1/operator/claims/<token>), which must answer one of want.
func operatorOn(t *testing.T, h *harness, token claim.Token, action string, body any, want ...int) {
	t.Helper()
	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/"+action, body)
	if !slices.Contains(want, recorder.Code) {
		t.Fatalf("legion claims %s of %s = %d, want one of %v; body %s", action, token, recorder.Code, want, recorder.Body)
	}
}

// controllerOn is one request of the controller's agent door at route on issue's role, under a
// controller grant the operator's bearer minted (POST /legion/v1/grants, controllerGrant), as
// `legion` mints the grant its controller's calls carry. It must answer one of want.
func controllerOn(t *testing.T, h *harness, route, issue string, role claim.Role, want ...int) {
	t.Helper()
	recorder := h.request(http.MethodPost, route, ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: issue, Role: string(role)}, nil)
	if !slices.Contains(want, recorder.Code) {
		t.Fatalf("the controller's %s of %s's %s = %d, want one of %v; body %s", route, issue, role, recorder.Code, want, recorder.Body)
	}
}

// cyberPolicyFailure is the failure a claim held for OpenAI's cyber-policy refusal records.
const cyberPolicyFailure = "a provider error that cannot clear ended this claim's turn: OpenAI's cyber_policy refusal of this session's content"

// heldArchitect is LEGION-208's root architect failed on OpenAI's cyber-policy refusal of its
// session's content, recording that session: the claim whose retry leaves the session behind
// whoever asks it, since a relaunch of the session would send that content again (supervise's
// freshSession). Its tree is open, as admission's promotion of the root leaves it, and so is the
// issue's own lifecycle, which admission opens for every issue it starts and which each claim of
// the issue binds (store.Store.AdmitClaim).
func heldArchitect(t *testing.T, h *harness) claim.Token {
	t.Helper()
	if _, err := h.store.OpenTreeLifecycle(h.ctx, testProject, "LEGION-208", treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of LEGION-208: %v", err)
	}
	if _, err := h.store.OpenIssueLifecycle(h.ctx, testProject, "LEGION-208", treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the issue lifecycle of LEGION-208: %v", err)
	}
	token := freshSessionToken(t, "LEGION-208", claim.RoleArchitect)
	if _, _, err := h.supervisor.Create(h.ctx, supervise.Claim{
		Token: token, Project: testProject, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect, Generation: 1,
		State: supervise.StateFailed, Session: "ses_architect", SessionFile: "/sessions/ses_architect.jsonl",
		FailureCause: supervise.CauseCyberPolicy, Failure: cyberPolicyFailure,
	}, "role prompt"); err != nil {
		t.Fatalf("create the held architect: %v", err)
	}
	return token
}

// heldController is the project's controller failed on OpenAI's cyber-policy refusal, recording
// its session: the one controller the operator's retry reaches, since its keeper retries it no
// more (supervise.Claim.KeeperRetries). Its claim is on no issue.
func heldController(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := claim.ControllerToken(testProject)
	if _, _, err := h.supervisor.Create(h.ctx, supervise.Claim{
		Token: token, Project: testProject, Role: claim.RoleController, Generation: 1, State: supervise.StateFailed,
		Session: "ses_controller", SessionFile: "/sessions/ses_controller.jsonl", FailureCause: supervise.CauseCyberPolicy,
		Failure: cyberPolicyFailure,
	}, ""); err != nil {
		t.Fatalf("create the held controller: %v", err)
	}
	return token
}

// wantLeftBehind holds token's stored claim, once its machine has settled what the route set
// going, to have left session behind: it records another session or none, and session as the one
// it abandoned. A route that left nothing behind would prove nothing of the release.
func wantLeftBehind(t *testing.T, h *harness, token claim.Token, session string) {
	t.Helper()
	m, _ := h.supervisor.Machine(token)
	m.Wait()
	if stored := h.stored(token); stored.Session == session || stored.Abandoned.Session != session {
		t.Fatalf("%s records session %q and abandoned %q after the route, want %s left behind; this proves nothing",
			token, stored.Session, stored.Abandoned.Session, session)
	}
}

// leftSessionRoute is one route of this package whose forget leaves the session of a claim on an
// issue behind: leave sets the claim up on a session of its own, takes the route, and answers the
// claim, its issue and the session the route left.
type leftSessionRoute struct {
	name  string
	leave func(t *testing.T, h *harness) (token claim.Token, issue, session string)
}

var leftSessionRoutes = []leftSessionRoute{
	{"the operator's legion claims forget-session, of a worker it suspended", func(t *testing.T, h *harness) (claim.Token, string, string) {
		token := readyImplementer(t, h)
		operatorOn(t, h, token, "suspend", nil, http.StatusOK)
		operatorOn(t, h, token, "forget-session", ForgetSessionRequest{Session: "ses_implementer"}, http.StatusOK)
		return token, "LEGION-209", "ses_implementer"
	}},
	{"the operator's legion claims retry --fresh-session, of a ready worker: its suspension, the forget and the resume", func(t *testing.T, h *harness) (claim.Token, string, string) {
		token := readyImplementer(t, h)
		operatorOn(t, h, token, "retry", RetryRequest{FreshSession: true}, http.StatusOK, http.StatusAccepted)
		return token, "LEGION-209", "ses_implementer"
	}},
	{"the operator's legion claims retry --fresh-session, of a worker a stop retired: the forget, then the asked retry with a fresh session", func(t *testing.T, h *harness) (claim.Token, string, string) {
		token := readyImplementer(t, h)
		operatorOn(t, h, token, "stop", nil, http.StatusOK)
		operatorOn(t, h, token, "retry", RetryRequest{FreshSession: true}, http.StatusOK, http.StatusAccepted)
		return token, "LEGION-209", "ses_implementer"
	}},
	{"the controller's fresh_session, of a ready worker: its suspension, the forget and the resume", func(t *testing.T, h *harness) (claim.Token, string, string) {
		token := readyImplementer(t, h)
		controllerOn(t, h, "/legion/v1/claims/fresh-session", "LEGION-209", claim.RoleImplementer, http.StatusOK)
		return token, "LEGION-209", "ses_implementer"
	}},
	{"the controller's fresh_session, of a worker a stop retired: the forget, then the asked retry with a fresh session", func(t *testing.T, h *harness) (claim.Token, string, string) {
		token := readyImplementer(t, h)
		operatorOn(t, h, token, "stop", nil, http.StatusOK)
		controllerOn(t, h, "/legion/v1/claims/fresh-session", "LEGION-209", claim.RoleImplementer, http.StatusOK)
		return token, "LEGION-209", "ses_implementer"
	}},
	{"the operator's legion claims retry, of an architect held for OpenAI's cyber-policy refusal: the asked retry, which leaves the session itself", func(t *testing.T, h *harness) (claim.Token, string, string) {
		token := heldArchitect(t, h)
		operatorOn(t, h, token, "retry", nil, http.StatusOK, http.StatusAccepted)
		return token, "LEGION-208", "ses_architect"
	}},
	{"the controller's retry_agent, of an architect held for OpenAI's cyber-policy refusal: the asked retry, which leaves the session itself", func(t *testing.T, h *harness) (claim.Token, string, string) {
		token := heldArchitect(t, h)
		controllerOn(t, h, "/legion/v1/agents/retry", "LEGION-208", claim.RoleArchitect, http.StatusOK, http.StatusAccepted)
		return token, "LEGION-208", "ses_architect"
	}},
}

// leftControllerRoutes is each route of leftSessionRoutes that reaches the project controller's
// claim, which is on no issue: take takes it on token, the held controller.
var leftControllerRoutes = []struct {
	name string
	take func(t *testing.T, h *harness, token claim.Token)
}{
	{"the operator's legion claims forget-session", func(t *testing.T, h *harness, token claim.Token) {
		operatorOn(t, h, token, "forget-session", ForgetSessionRequest{Session: "ses_controller"}, http.StatusOK)
	}},
	{"the operator's legion claims retry --fresh-session: the forget, then the asked retry with a fresh session", func(t *testing.T, h *harness, token claim.Token) {
		operatorOn(t, h, token, "retry", RetryRequest{FreshSession: true}, http.StatusOK, http.StatusAccepted)
	}},
	{"the operator's legion claims retry: the asked retry, which leaves the session itself", func(t *testing.T, h *harness, token claim.Token) {
		operatorOn(t, h, token, "retry", nil, http.StatusOK, http.StatusAccepted)
	}},
}

// R5 of dispatch://INFRA-3144, at this package's routes: when the daemon leaves a session behind
// for a claim, it releases the Dispatch claim that session holds on the claim's issue, and the
// release is queued by the claim write that leaves it (store.Store.ReleaseLeftSessionClaims, on as
// the daemon turns it on). Every route of this package that reaches a forget of a claim's session
// is a row: the operator's `legion claims forget-session`; `legion claims retry --fresh-session`
// and the controller's fresh_session (POST /legion/v1/claims/fresh-session), each of a ready
// worker, which it suspends, forgets and resumes, and of a worker a stop retired, which it forgets
// and starts with the asked retry of a fresh session (claimstart.go's machineRetry,
// supervise.RequestAskedRetry{FreshSession}); and the asked retry the operator's `legion claims
// retry` and the controller's retry_agent (POST /legion/v1/agents/retry) send for an architect held
// for OpenAI's cyber-policy refusal, which leaves that session itself. Each queues one claim_release
// row keyed by the claim's issue whose payload, decoded as the claim lane decodes it, names the
// session left behind, committed by the time the store tells its wake that issue, once. The
// project controller's claim, on no issue, forgotten through each route that reaches it, queues no
// row and tells nothing.
func TestASessionTheDaemonLeavesBehindHasItsClaimReleasedOnEveryRoute(t *testing.T) {
	for _, route := range leftSessionRoutes {
		t.Run(route.name, func(t *testing.T) {
			h := newHarness(t)
			told := releasingLeftSessions(t, h)

			token, issue, left := route.leave(t, h)

			wantLeftBehind(t, h, token, left)
			if got, want := queuedLeftReleases(t, h), []leftRelease{{issue: issue, session: left}}; !slices.Equal(got, want) {
				t.Errorf("the route that left %s behind queued the claim releases %+v, want %+v", left, got, want)
			}
			if got, want := told(), []releaseTold{{issue: issue, visible: 1}}; !slices.Equal(got, want) {
				t.Errorf("the store told its wake %+v, want %+v: %s once, its row committed", got, want, issue)
			}
		})
	}

	for _, route := range leftControllerRoutes {
		t.Run("the project controller's claim, on no issue: "+route.name, func(t *testing.T) {
			h := newHarness(t)
			told := releasingLeftSessions(t, h)
			token := heldController(t, h)

			route.take(t, h, token)

			wantLeftBehind(t, h, token, "ses_controller")
			if got := queuedLeftReleases(t, h); len(got) != 0 {
				t.Errorf("the controller's claim left ses_controller behind and queued the claim releases %+v, want none: its claim has no issue", got)
			}
			if got := told(); len(got) != 0 {
				t.Errorf("the store told its wake %+v for the controller's claim, want nothing", got)
			}
		})
	}
}
