package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	hiddenLookID     = "LOOKIDSECRET77777777777777"
	hiddenCheckRoot  = "LEGION-208"
	hiddenCheckChild = "LEGION-209"
	// hiddenLookTaskFmt is a look's task as workflow's checkTask writes it: the issue, the check's
	// opening time twice and, on a line of its own, the look-id line (intake.LookIDLine).
	hiddenLookTaskFmt = "Production check of %s: child.\nRepository acme/widgets, pull request #42, merge commit merge-42. Look 1, woken by the merge. The check opened at `%s`: your report names it with check_opened_at `%s`.\n%s\nDo what the deployment instructions say to reach production."
)

// lookIDFixture is a live API server backed by a real Postgres store whose production check on
// LEGION-209 and tester claim's pending delivery both carry hiddenLookID.
type lookIDFixture struct {
	h        *harness
	openedAt time.Time
	token    claim.Token
	session  string
	secret   string
	task     string
}

// projectedLookState builds State directly from the store's real production check and claims, so
// GET /legion/v1/state (both default and ?view=observed) and GET /legion/v1/issues/{key}/record
// exercise ClaimViewOf and PendingTaskViewOf over the fixture's stored records. Its CheckView is
// the test's own copy of the projection's check view (projection's checkView), since api cannot
// import projection; internal/projection's TestTheProjectedCheckHoldsNoLookID holds the real one
// to the same search.
type projectedLookState struct {
	st      *store.Store
	records record.Store
}

func (p projectedLookState) State(ctx context.Context, tx pgx.Tx) (State, []supervise.Claim, error) {
	claims, err := p.st.ClaimsTx(ctx, tx)
	if err != nil {
		return State{}, nil, err
	}
	check, err := p.records.ProductionCheck(ctx, tx, hiddenCheckChild)
	if err != nil {
		return State{}, nil, err
	}
	issueRow, err := p.records.Issue(ctx, tx, hiddenCheckChild)
	if err != nil {
		return State{}, nil, err
	}
	workers := map[claim.Role]PhaseView{}
	for _, c := range claims {
		if c.Issue != hiddenCheckChild {
			continue
		}
		cv := ClaimViewOf(c)
		workers[c.Role] = PhaseView{Claim: cv}
	}
	var checkView *CheckView
	if check != nil && check.Open() {
		next := check.BoundAt
		if check.WakeAt != nil {
			next = *check.WakeAt
		}
		checkView = &CheckView{
			Repo: check.Repo, PullRequest: check.Number, MergeCommit: check.MergeSHA,
			OpenedAt: check.OpenedAt, Role: string(check.Role), Looks: check.Looks,
			LookOpen: check.LookOpen, NextWakeAt: next, Last: check.Last,
		}
		if check.LastWakeAt != nil {
			checkView.LastWake = &CheckWakeView{Kind: check.LastWakeKind, Detail: check.LastWakeDetail, At: *check.LastWakeAt}
		}
	}
	issues := map[string]Issue{}
	if issueRow != nil {
		issues[hiddenCheckChild] = Issue{
			Key:        issueRow.Key,
			Tree:       issueRow.Tree,
			Phase:      issueRow.Phase,
			Generation: issueRow.Generation,
			Status:     issueRow.Status,
			Workers:    workers,
			Check:      checkView,
		}
	}
	return State{
		Daemon: DaemonInfo{Project: testProject},
		Issues: issues,
	}, claims, nil
}

func (p projectedLookState) ObservedState(ctx context.Context, tx pgx.Tx) (State, []supervise.Claim, error) {
	return p.State(ctx, tx)
}

// newLookIDFixture builds a fixture with an open production check carrying hiddenLookID and a
// pending look task whose Delivery.LookID and Delivery.Task both hold hiddenLookID.
// When sent is false, the claim is launching with the task queued and unsent (DeliveredAt zero).
// When sent is true, the claim has registered, connected and readied so the task has been sent
// onto its fake connection (DeliveredAt non-zero, still pending until a turn ends).
func newLookIDFixture(t *testing.T, sent bool) *lookIDFixture {
	t.Helper()
	h := newHarness(t)
	h.handoffs = &statusRecorder{}
	github := newHandoffGitHub(t)
	opened := checkOpenedAt.UTC().Truncate(time.Microsecond)
	cfg := workflow.Config{
		Project:              testDispatchProject,
		Repositories:         testRepositories,
		Steps:                config.DesignGateRootIssues.Steps(),
		ReviewRoundCap:       3,
		ProductionCheckRole:  claim.RoleTester,
		ProductionCheckBound: 48 * time.Hour,
		Clock:                func() time.Time { return opened },
	}
	engine := workflow.New(record.NewStore(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	src := projectedLookState{st: h.store, records: record.NewStore()}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		State:             src,
		StateTransactions: h.store.Pool(),
		Steps:             cfg.Steps,
		Supervisor:        h.supervisor,
		BootTokens:        h.tokens,
		Project:           testProject,
		DispatchProject:   testDispatchProject,
		OperatorToken:     testOperatorToken,
		Controller:        h.store,
		Grants:            credential.New(nil),
		Pool:              h.store.Pool(),
		Record:            record.NewStore(),
		Releaser:          h.runtime,
		Trees:             h.store,
		ReleaseRecords:    h.store,
		ClaimActivity:     h.store,
		Handlers:          []intake.Handler{engine},
		Dispatch:          h.handoffs,
		RoleFor:           workflow.RoleFor,
		Tokens:            handoffTokens{},
		GitHubAPI:         github.url,
	}).Handler

	parent := hiddenCheckRoot
	h.recordIssue(record.Issue{
		Key: hiddenCheckRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: hiddenCheckRoot, Project: testDispatchProject, Title: "root",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress",
	})
	h.recordIssue(record.Issue{
		Key: hiddenCheckChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: hiddenCheckRoot, Parent: &parent, Project: testDispatchProject, Title: "child",
		Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done",
	})

	check := record.ProductionCheck{
		Issue: hiddenCheckChild, Project: testDispatchProject, Tree: hiddenCheckRoot,
		Repo: "acme/widgets", Number: 42, MergeSHA: "merge-42", Role: claim.RoleTester,
		OpenedAt: opened, BoundAt: opened.Add(48 * time.Hour),
		Looks: 1, LookOpen: true, LookWakeAt: &opened,
		LastWakeKind: "merge", LastWakeDetail: "merge-42", LastWakeAt: &opened,
		LookID: hiddenLookID, LookIDLook: 1, LookIDOpenedAt: opened, OpenedGeneration: 1,
	}
	if check.CurrentLookID() != hiddenLookID {
		t.Fatalf("fixture check.CurrentLookID() = %q, want %q", check.CurrentLookID(), hiddenLookID)
	}
	ctx := context.Background()
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(ctx, tx, check)
	}); err != nil {
		t.Fatalf("seed production check: %v", err)
	}

	token, boot := h.launchIn(hiddenCheckRoot, hiddenCheckChild, claim.RoleTester)
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	openedStr := opened.Format(time.RFC3339Nano)
	taskText := fmt.Sprintf(hiddenLookTaskFmt, hiddenCheckChild, openedStr, openedStr, intake.LookIDLine(hiddenLookID))
	if err := machine.Handle(ctx, supervise.RequestDeliver{
		Claim:      token,
		ID:         "outbox:look-1",
		Task:       taskText,
		Generation: 1,
		CheckLook:  supervise.CheckLook{Look: 1, OpenedAt: opened},
		LookID:     hiddenLookID,
	}); err != nil {
		t.Fatalf("queue look task on %s: %v", token, err)
	}

	f := &lookIDFixture{
		h:        h,
		openedAt: opened,
		token:    token,
		session:  "ses_tester_" + hiddenCheckChild,
		task:     taskText,
	}

	if !sent {
		c := machine.Claim()
		if c.Pending == nil || !c.Pending.DeliveredAt.IsZero() || c.Pending.LookID != hiddenLookID {
			t.Fatalf("unsent fixture pending = %+v, want queued unsent delivery carrying %q", c.Pending, hiddenLookID)
		}
		return f
	}

	// Register, connect and ready so the pending look task is sent over the fake connection.
	conn := fake.NewConn()
	h.conns.Register(token, conn)
	if err := machine.Handle(ctx, supervise.StreamHello{Claim: token, Generation: machine.Claim().Generation}); err != nil {
		t.Fatalf("hello %s: %v", token, err)
	}
	reg := h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: boot, SessionID: f.session, OmpSessionFile: "/sessions/tester.jsonl", AgentID: "agent-tester", PluginContract: DaemonAPIVersion(),
	}, nil)
	if reg.Code != http.StatusOK {
		t.Fatalf("register %s = %d: %s", token, reg.Code, reg.Body)
	}
	var regResp claim.RegisterResponse
	decodeInto(t, reg, &regResp)
	f.secret = regResp.Secret

	ready := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: f.session, Secret: f.secret, Generation: machine.Claim().Generation,
	}, nil)
	if ready.Code != http.StatusNoContent {
		t.Fatalf("ready %s = %d: %s", token, ready.Code, ready.Body)
	}
	machine.Wait()
	c := machine.Claim()
	if c.Pending == nil || c.Pending.DeliveredAt.IsZero() || c.Pending.LookID != hiddenLookID {
		t.Fatalf("sent fixture pending = %+v, want sent delivery carrying %q", c.Pending, hiddenLookID)
	}
	if len(conn.Prompts()) == 0 {
		t.Fatalf("sent fixture sent no prompt over the connection")
	}
	return f
}

// assertBodyHidesLookID fails if rec's response body carries the fixture's secret look id.
func assertBodyHidesLookID(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	if strings.Contains(body, hiddenLookID) {
		t.Errorf("%s (%d) leaked look id %q in response body:\n%s", label, rec.Code, hiddenLookID, body)
	}
}

type lookIDRouteEntry struct {
	// notRead states why this route is not a read of check/claim state (empty when searched).
	notRead string
	// search calls a searched route over a fixture whose claim holds the look's task, after that
	// task is sent and, where the row says so, before it, and on the refusals the row names. It
	// fails the row on any response body that holds hiddenLookID.
	search func(t *testing.T)
}

// TestEveryRouteIsSearchedForTheLookID holds the R3 surface inventory: every route NewServer
// registers is either searched against a fixture carrying a known look id (after the look task is
// sent, before it is sent where the row says so, and on refusal answers) or explicitly classified
// as not a read with its reason. Any unregistered or unclassified route fails the test, and any
// body carrying the look id fails the route's row.
//
// The operator claim routes render OperatorClaim through operatorView (api/operator_claim.go),
// which writes the pending task with its look id withheld (withheldTask), so their rows pass with
// the rest; TestTheOperatorClaimViewWithholdsTheLookID holds what that view still shows.
func TestEveryRouteIsSearchedForTheLookID(t *testing.T) {
	patterns := registeredPatterns(t)

	// Shared read-only fixtures (unsent and sent) keep non-mutating route checks fast while
	// mutating routes build fresh fixtures so no route's side effects taint another.
	readOnlyUnsent := newLookIDFixture(t, false)
	readOnlySent := newLookIDFixture(t, true)

	checkReadBoth := func(t *testing.T, call func(f *lookIDFixture, phase string) *httptest.ResponseRecorder) {
		t.Helper()
		for _, pair := range []struct {
			name string
			f    *lookIDFixture
		}{
			{"before send", readOnlyUnsent},
			{"after send", readOnlySent},
		} {
			rec := call(pair.f, pair.name)
			assertBodyHidesLookID(t, pair.name, rec)
		}
	}

	classified := map[string]lookIDRouteEntry{
		// --- Searched read & answer-bearing surfaces ---
		"GET /legion/v1/state": {
			search: func(t *testing.T) {
				for _, target := range []string{"/legion/v1/state", "/legion/v1/state?view=observed", "/legion/v1/state?view=unknown"} {
					checkReadBoth(t, func(f *lookIDFixture, stage string) *httptest.ResponseRecorder {
						rec := f.h.request(http.MethodGet, target, nil, nil)
						if rec.Code != http.StatusOK {
							t.Fatalf("%s (%s) = %d, want 200: %s", target, stage, rec.Code, rec.Body)
						}
						if !strings.Contains(rec.Body.String(), hiddenCheckChild) {
							t.Fatalf("%s (%s) did not serve fixture issue %s: %s", target, stage, hiddenCheckChild, rec.Body)
						}
						return rec
					})
				}
			},
		},
		"GET /legion/v1/issues/{key}/record": {
			search: func(t *testing.T) {
				checkReadBoth(t, func(f *lookIDFixture, stage string) *httptest.ResponseRecorder {
					rec := f.h.request(http.MethodGet, "/legion/v1/issues/"+hiddenCheckChild+"/record", nil, nil)
					if rec.Code != http.StatusOK {
						t.Fatalf("record (%s) = %d, want 200: %s", stage, rec.Code, rec.Body)
					}
					var got Issue
					if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
						t.Fatalf("decode record (%s): %v", stage, err)
					}
					if got.Check == nil || got.Check.Looks != 1 || got.Workers[claim.RoleTester].Claim.PendingTask == nil {
						t.Fatalf("record (%s) missing expected check/pendingTask: %+v", stage, got)
					}
					return rec
				})
				// Refusal answer for an unknown issue.
				assertBodyHidesLookID(t, "unknown issue refusal", readOnlySent.h.request(http.MethodGet, "/legion/v1/issues/LEGION-999/record", nil, nil))
			},
		},
		"GET /healthz": {
			search: func(t *testing.T) {
				checkReadBoth(t, func(f *lookIDFixture, stage string) *httptest.ResponseRecorder {
					rec := f.h.request(http.MethodGet, "/healthz", nil, nil)
					if rec.Code != http.StatusOK {
						t.Fatalf("healthz (%s) = %d: %s", stage, rec.Code, rec.Body)
					}
					return rec
				})
			},
		},
		"GET /readyz": {
			search: func(t *testing.T) {
				checkReadBoth(t, func(f *lookIDFixture, stage string) *httptest.ResponseRecorder {
					return f.h.request(http.MethodGet, "/readyz", nil, nil)
				})
			},
		},
		"POST /legion/v1/claims/owed": {
			search: func(t *testing.T) {
				rec := readOnlySent.h.request(http.MethodPost, "/legion/v1/claims/owed", OwedRequest{
					ClaimToken: readOnlySent.token, SessionID: readOnlySent.session, Secret: readOnlySent.secret,
				}, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("claims/owed = %d, want 200: %s", rec.Code, rec.Body)
				}
				assertBodyHidesLookID(t, "after send 200", rec)
				// Refusal with wrong secret (before and after send).
				checkReadBoth(t, func(f *lookIDFixture, _ string) *httptest.ResponseRecorder {
					return f.h.request(http.MethodPost, "/legion/v1/claims/owed", OwedRequest{
						ClaimToken: f.token, SessionID: f.session, Secret: "wrong-secret",
					}, nil)
				})
			},
		},
		"POST /legion/v1/claims/release-records": {
			search: func(t *testing.T) {
				rec := readOnlySent.h.request(http.MethodPost, "/legion/v1/claims/release-records", ReleaseRecordsRequest{
					ClaimToken: readOnlySent.token, Secret: readOnlySent.secret, Since: readOnlySent.openedAt.Add(-time.Hour).Format(time.RFC3339),
				}, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("claims/release-records = %d, want 200: %s", rec.Code, rec.Body)
				}
				assertBodyHidesLookID(t, "after send 200", rec)
				checkReadBoth(t, func(f *lookIDFixture, _ string) *httptest.ResponseRecorder {
					return f.h.request(http.MethodPost, "/legion/v1/claims/release-records", ReleaseRecordsRequest{
						ClaimToken: f.token, Secret: "wrong-secret", Since: f.openedAt.Format(time.RFC3339),
					}, nil)
				})
			},
		},
		"POST /legion/v1/handoff/complete": {
			search: func(t *testing.T) {
				f := newLookIDFixture(t, true)
				grant := liveClaim{h: f.h, token: f.token, session: f.session, secret: f.secret, tree: hiddenCheckRoot, issue: hiddenCheckChild}.grant(t)
				opened := f.openedAt
				wrongOpened := opened.Add(-time.Hour)
				// Each row is a refusal. The missing verdict gives the current look's id, so it is
				// refused after the route takes that id as the current look's.
				for _, tc := range []struct {
					name   string
					body   HandoffCompleteRequest
					status int
					code   string
				}{
					{"phase completion on done issue", HandoffCompleteRequest{GrantID: grant, Verdict: "pass", Summary: "done"},
						http.StatusConflict, "CHECK_LOOK_NOT_SERVED"},
					{"wrong checkOpenedAt", HandoffCompleteRequest{GrantID: grant, CheckOpenedAt: &wrongOpened, Verdict: "pass", Summary: "done"},
						http.StatusConflict, "CHECK_NOT_OPEN"},
					{"missing verdict", HandoffCompleteRequest{GrantID: grant, CheckOpenedAt: &opened, LookID: hiddenLookID, Summary: "done"},
						http.StatusBadRequest, "CHECK_VERDICT_REQUIRED"},
					{"check report without lookId", HandoffCompleteRequest{GrantID: grant, CheckOpenedAt: &opened, Verdict: "pass", Summary: "done"},
						http.StatusBadRequest, "CHECK_LOOK_ID_REQUIRED"},
				} {
					rec := f.h.request(http.MethodPost, "/legion/v1/handoff/complete", tc.body, nil)
					assertBodyHidesLookID(t, tc.name, rec)
					refused(t, rec, tc.status, tc.code)
				}
				// Before send (invalid grant refusal).
				assertBodyHidesLookID(t, "before send refusal", readOnlyUnsent.h.request(http.MethodPost, "/legion/v1/handoff/complete",
					HandoffCompleteRequest{GrantID: "no-such-grant", Verdict: "pass", Summary: "done"}, nil))
			},
		},
		"GET /legion/v1/operator/claims": {
			search: func(t *testing.T) {
				checkReadBoth(t, func(f *lookIDFixture, stage string) *httptest.ResponseRecorder {
					rec := f.h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
					if rec.Code != http.StatusOK {
						t.Fatalf("operator claims (%s) = %d, want 200: %s", stage, rec.Code, rec.Body)
					}
					var got OperatorClaims
					if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
						t.Fatalf("decode operator claims (%s): %v", stage, err)
					}
					foundPending := false
					for _, c := range got.Claims {
						if c.Token == f.token && c.Pending != nil {
							foundPending = true
						}
					}
					if !foundPending {
						t.Fatalf("operator claims (%s) did not list pending delivery for %s: %s", stage, f.token, rec.Body)
					}
					return rec
				})
				assertBodyHidesLookID(t, "unauthenticated refusal", readOnlySent.h.request(http.MethodGet, "/legion/v1/operator/claims", nil, nil))
			},
		},
		"POST /legion/v1/operator/claims": {
			search: func(t *testing.T) {
				// Refusal when spawning an already-existing claim carrying a pending look task.
				checkReadBoth(t, func(f *lookIDFixture, _ string) *httptest.ResponseRecorder {
					return f.h.operator(http.MethodPost, "/legion/v1/operator/claims", SpawnRequest{
						Tree: hiddenCheckRoot, Issue: hiddenCheckChild, Role: claim.RoleTester, Prompt: "prompt",
					})
				})
			},
		},
		"POST /legion/v1/operator/claims/{token}/deliver": {
			search: func(t *testing.T) {
				// Refused behind the pending look task (409) before and after send.
				checkReadBoth(t, func(f *lookIDFixture, _ string) *httptest.ResponseRecorder {
					return f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/deliver", DeliverRequest{Task: "another task"})
				})
			},
		},
		"POST /legion/v1/operator/claims/{token}/suspend": {
			search: func(t *testing.T) {
				for _, sent := range []bool{false, true} {
					stage := "before send"
					if sent {
						stage = "after send"
					}
					f := newLookIDFixture(t, sent)
					rec := f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/suspend", nil)
					assertBodyHidesLookID(t, stage, rec)
				}
			},
		},
		"POST /legion/v1/operator/claims/{token}/resume": {
			search: func(t *testing.T) {
				for _, sent := range []bool{false, true} {
					stage := "before send"
					if sent {
						stage = "after send"
					}
					f := newLookIDFixture(t, sent)
					// Refusal while not suspended.
					assertBodyHidesLookID(t, stage+" refusal", f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/resume", nil))
					if sent {
						// Suspend first, then resume to inspect the OperatorClaim body.
						if suspended := f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/suspend", nil); suspended.Code != http.StatusOK && suspended.Code != http.StatusAccepted {
							t.Fatalf("suspend (%s) = %d, want 200 or 202: %s", stage, suspended.Code, suspended.Body)
						}
						rec := f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/resume", nil)
						if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
							t.Fatalf("resume (%s) = %d, want 200 or 202: %s", stage, rec.Code, rec.Body)
						}
						assertBodyHidesLookID(t, stage+" resumed", rec)
					}
				}
			},
		},
		"POST /legion/v1/operator/claims/{token}/stop": {
			search: func(t *testing.T) {
				for _, sent := range []bool{false, true} {
					stage := "before send"
					if sent {
						stage = "after send"
					}
					f := newLookIDFixture(t, sent)
					rec := f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/stop", nil)
					assertBodyHidesLookID(t, stage, rec)
				}
			},
		},
		"POST /legion/v1/operator/claims/{token}/retry": {
			search: func(t *testing.T) {
				// Refusal on launching/ready claim carrying a pending look task.
				checkReadBoth(t, func(f *lookIDFixture, _ string) *httptest.ResponseRecorder {
					return f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/retry", RetryRequest{})
				})
			},
		},
		"POST /legion/v1/operator/claims/{token}/compact": {
			search: func(t *testing.T) {
				for _, sent := range []bool{false, true} {
					stage := "before send"
					if sent {
						stage = "after send"
					}
					f := newLookIDFixture(t, sent)
					rec := f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/compact", nil)
					assertBodyHidesLookID(t, stage, rec)
				}
			},
		},
		"POST /legion/v1/operator/claims/{token}/forget-session": {
			search: func(t *testing.T) {
				// Refusal on live claim, and 200 OperatorClaim on suspended claim carrying pending look task.
				assertBodyHidesLookID(t, "before send refusal", readOnlyUnsent.h.operator(http.MethodPost,
					"/legion/v1/operator/claims/"+string(readOnlyUnsent.token)+"/forget-session", ForgetSessionRequest{Session: readOnlyUnsent.session}))
				f := newLookIDFixture(t, true)
				_ = f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/suspend", nil)
				rec := f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/forget-session", ForgetSessionRequest{Session: f.session})
				if rec.Code != http.StatusOK {
					t.Fatalf("forget-session on suspended claim = %d, want 200: %s", rec.Code, rec.Body)
				}
				assertBodyHidesLookID(t, "after send suspended", rec)
			},
		},
		"POST /legion/v1/operator/claims/{token}/release-names": {
			search: func(t *testing.T) {
				checkReadBoth(t, func(f *lookIDFixture, _ string) *httptest.ResponseRecorder {
					return f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/release-names", nil)
				})
			},
		},
		"POST /legion/v1/agents/suspend": {
			search: func(t *testing.T) {
				for _, sent := range []bool{false, true} {
					stage := "before send"
					if sent {
						stage = "after send"
					}
					f := newLookIDFixture(t, sent)
					grant := controllerGrant(t, f.h)
					rec := f.h.request(http.MethodPost, "/legion/v1/agents/suspend", ClaimRequest{
						GrantID: grant.GrantID, Issue: hiddenCheckChild, Role: string(claim.RoleTester),
					}, nil)
					assertBodyHidesLookID(t, stage, rec)
				}
			},
		},
		"POST /legion/v1/agents/resume": {
			search: func(t *testing.T) {
				f := newLookIDFixture(t, true)
				grant := controllerGrant(t, f.h)
				assertBodyHidesLookID(t, "refusal while live", f.h.request(http.MethodPost, "/legion/v1/agents/resume", ClaimRequest{
					GrantID: grant.GrantID, Issue: hiddenCheckChild, Role: string(claim.RoleTester),
				}, nil))
				if suspended := f.h.request(http.MethodPost, "/legion/v1/agents/suspend", ClaimRequest{
					GrantID: grant.GrantID, Issue: hiddenCheckChild, Role: string(claim.RoleTester),
				}, nil); suspended.Code != http.StatusOK && suspended.Code != http.StatusAccepted {
					t.Fatalf("suspend = %d, want 200 or 202: %s", suspended.Code, suspended.Body)
				}
				resumed := f.h.request(http.MethodPost, "/legion/v1/agents/resume", ClaimRequest{
					GrantID: grant.GrantID, Issue: hiddenCheckChild, Role: string(claim.RoleTester),
				}, nil)
				if resumed.Code != http.StatusOK && resumed.Code != http.StatusAccepted {
					t.Fatalf("resume = %d, want 200 or 202: %s", resumed.Code, resumed.Body)
				}
				assertBodyHidesLookID(t, "resumed", resumed)
			},
		},
		"POST /legion/v1/agents/stop": {
			search: func(t *testing.T) {
				f := newLookIDFixture(t, true)
				grant := controllerGrant(t, f.h)
				assertBodyHidesLookID(t, "after send", f.h.request(http.MethodPost, "/legion/v1/agents/stop", ClaimRequest{
					GrantID: grant.GrantID, Issue: hiddenCheckChild, Role: string(claim.RoleTester),
				}, nil))
			},
		},
		"POST /legion/v1/agents/retry": {
			search: func(t *testing.T) {
				checkReadBoth(t, func(f *lookIDFixture, _ string) *httptest.ResponseRecorder {
					grant := controllerGrant(t, f.h)
					return f.h.request(http.MethodPost, "/legion/v1/agents/retry", ClaimRequest{
						GrantID: grant.GrantID, Issue: hiddenCheckChild, Role: string(claim.RoleTester),
					}, nil)
				})
			},
		},
		"POST /legion/v1/claims/fresh-session": {
			search: func(t *testing.T) {
				f := newLookIDFixture(t, true)
				grant := controllerGrant(t, f.h)
				assertBodyHidesLookID(t, "after send", f.h.request(http.MethodPost, "/legion/v1/claims/fresh-session", ClaimRequest{
					GrantID: grant.GrantID, Issue: hiddenCheckChild, Role: string(claim.RoleTester),
				}, nil))
			},
		},
		"POST /legion/v1/claims/compact": {
			search: func(t *testing.T) {
				f := newLookIDFixture(t, true)
				grant := controllerGrant(t, f.h)
				assertBodyHidesLookID(t, "after send", f.h.request(http.MethodPost, "/legion/v1/claims/compact", ClaimRequest{
					GrantID: grant.GrantID, Issue: hiddenCheckChild, Role: string(claim.RoleTester),
				}, nil))
			},
		},

		// --- Non-read routes (each explicitly classified with its reason) ---
		"POST /legion/v1/claims/register":              {notRead: "registers a launching process and returns only its claim's token, tree, issue, role and generation, its session secret, the task agents its role's prompts dispatch and the session it replaces"},
		"POST /legion/v1/claims/ready":                 {notRead: "marks a claim ready for prompts and answers 204 with no body"},
		"POST /legion/v1/claims/ready-for-update":      {notRead: "signals a pod-update readiness ack and answers 204 with no body"},
		"POST /legion/v1/claims/exit":                  {notRead: "records a claim's own exit report and answers 204 with no body"},
		"POST /legion/v1/claims/turn-outcome":          {notRead: "records a turn-outcome report and returns only the claim's refused run and whether the report failed the claim"},
		"POST /legion/v1/claims/activity":              {notRead: "records a claim's background activity heartbeat and answers 204 with no body"},
		"POST /legion/v1/grants":                       {notRead: "mints a short-lived credential grant and returns only its grantId and expiry"},
		"POST /legion/v1/controller/secret":            {notRead: "serves a controller startup secret from the daemon's secrets store, reading no claim or check"},
		"POST /legion/v1/controller/github-credential": {notRead: "mints the controller's GitHub App credential, reading no claim or check"},
		"POST /legion/v1/issues/status":                {notRead: "writes an issue's Dispatch status and answers an empty object or refusal"},
		"POST /legion/v1/issues/pod-update":            {notRead: "opens an issue's pod-update round and returns only the round's id, its reasons and whether this request opened it"},
		"POST /legion/v1/gates/register":               {notRead: "registers a design-gate artifact version and answers an empty object or refusal"},
		"POST /legion/v1/plans/register":               {notRead: "registers a plan artifact version and answers an empty object or refusal"},
		"POST /legion/v1/tasks/stop":                   {notRead: "stops a parent's phase task for open children and answers an empty object or refusal"},
		"POST /legion/v1/waves/release":                {notRead: "releases a wave of child issues and returns only the keys of the children released and started, and of each child held with the issues it waits on"},
		"POST /legion/v1/phase/backward":               {notRead: "moves an issue backward to an earlier phase and answers an empty object or refusal"},
		"POST /legion/v1/phase/retry":                  {notRead: "retries a held phase and answers an empty object or refusal"},
		"POST /legion/v1/signoff":                      {notRead: "records an architect's sign-off and answers an empty object or refusal"},
		"POST /legion/v1/roots/close":                  {notRead: "closes a root issue and answers an empty object or refusal"},
		"POST /legion/v1/children/park":                {notRead: "removed route that always refuses with a static message"},
		"POST /legion/v1/children/rerun":               {notRead: "re-enters a done child issue and answers an empty object or refusal"},
		"POST /legion/v1/intake/replay":                {notRead: "replays intake give-ups and returns only each replayed record's id, consumer, stream sequence, outcome and error, and how many remain"},
		"POST /legion/v1/intake/discard":               {notRead: "discards intake give-ups and returns only the id of each record it closed"},
		"POST /legion/v1/project/pause":                {notRead: "pauses the project and returns only the pause record"},
		"POST /legion/v1/project/resume":               {notRead: "resumes the paused project and returns only the pause record"},
		"POST /legion/v1/project/p0-bound":             {notRead: "sets the project's P0 bound and returns only the bound"},
		"POST /legion/v1/project/p0-bound/clear":       {notRead: "clears the project's P0 bound and returns only the bound"},
		"POST /legion/v1/operator/claims/{token}/close": {
			search: func(t *testing.T) {
				// Refusal on a non-root workflow claim carrying a pending look task.
				checkReadBoth(t, func(f *lookIDFixture, _ string) *httptest.ResponseRecorder {
					return f.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token)+"/close", nil)
				})
			},
		},
		"POST /legion/v1/operator/intake/replay":    {notRead: "operator replay of a failed intake event; reads no claim or check"},
		"POST /legion/v1/operator/intake/discard":   {notRead: "operator discard of a failed intake event; reads no claim or check"},
		"POST /legion/v1/operator/project/pause":    {notRead: "operator project pause; returns only the pause record"},
		"POST /legion/v1/operator/project/resume":   {notRead: "operator project resume; returns only the pause record"},
		"POST /legion/v1/operator/project/p0-bound": {notRead: "operator P0 bound write; returns only the bound"},
		"POST /legion/v1/operator/project/p0-bound/clear": {
			notRead: "operator P0 bound clear; returns only the bound",
		},
		"/": {notRead: "catch-all 404 handler answering a static 'no route' refusal"},
	}

	seen := make(map[string]bool, len(patterns))
	for _, pattern := range patterns {
		seen[pattern] = true
		entry, ok := classified[pattern]
		if !ok {
			t.Errorf("registered route %q has no row in TestEveryRouteIsSearchedForTheLookID", pattern)
			continue
		}
		if entry.search == nil && strings.TrimSpace(entry.notRead) == "" {
			t.Errorf("route %q has neither a search function nor a notRead reason", pattern)
			continue
		}
		if entry.search != nil && entry.notRead != "" {
			t.Errorf("route %q sets both search and notRead (%q); choose one", pattern, entry.notRead)
			continue
		}
		if entry.search != nil {
			t.Run(pattern, entry.search)
		}
	}

	for pattern := range classified {
		if !seen[pattern] {
			t.Errorf("classified route %q is no longer registered by NewServer", pattern)
		}
	}

	// Guard: the state route serves the fixture's check, open at look 1, so the state rows above
	// searched a body that holds the check; that body holds no look id either.
	var stateBody State
	rec := readOnlySent.h.request(http.MethodGet, "/legion/v1/state", nil, nil)
	assertBodyHidesLookID(t, "state", rec)
	if err := json.Unmarshal(rec.Body.Bytes(), &stateBody); err != nil {
		t.Fatalf("decode state body: %v", err)
	}
	if got := stateBody.Issues[hiddenCheckChild].Check; got == nil || got.Looks != 1 || !got.LookOpen {
		t.Fatalf("state fixture check = %+v, want open check at look 1", got)
	}
}
