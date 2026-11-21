package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// waitNow is the moment the wait harness's daemon reads as now (Options.Now), so a declaration's
// time and the bounds measured from it are the test's to name. It carries a fraction, as a real
// clock's reading does, which the record keeps to the microsecond.
var waitNow = time.Date(2026, 11, 14, 6, 0, 0, 123456000, time.UTC)

// waitSpec is the artifact of LEGION-208's spec, the one document the deployment gates.
const waitSpec = "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26"

// waitHarness is the daemon's API over a real store with the workflow deciding every fact, its log
// written to logs, and two holds after the engine that a test arms to keep one fact's transaction
// open, under the fact lock, until it lets it go.
type waitHarness struct {
	*harness
	engine        *workflow.Engine
	logs          *testlog.Buffer
	first, second *factHold
}

func newWaitHarness(t *testing.T) *waitHarness {
	t.Helper()
	h := newHarness(t)
	logs := &testlog.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, log)
	w := &waitHarness{harness: h, engine: engine, logs: logs, first: newFactHold(), second: newFactHold()}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Pool: h.store.Pool(), Record: records, PluginRelaunchable: true,
		Handlers: w.handlers(), Log: log, Now: func() time.Time { return waitNow },
	}).Handler
	return w
}

// handlers is the fact handlers the server applies every fact with, which a test applying a fact
// of its own applies it with too.
func (h *waitHarness) handlers() []intake.Handler {
	return []intake.Handler{h.engine, h.first, h.second}
}

// factHold is a handler that, armed, holds the transaction of the first fact it names open after
// every handler before it has run, until the test lets it go: the fact holds the fact lock the
// whole time (intake.ApplyFact), so a fact applied meanwhile waits for it.
type factHold struct {
	holds   func(intake.Fact) bool
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	let     sync.Once
}

func newFactHold() *factHold {
	return &factHold{entered: make(chan struct{}), release: make(chan struct{})}
}

func (f *factHold) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if f.holds == nil || !f.holds(fact) {
		return intake.Result{}, nil
	}
	held := false
	f.once.Do(func() { held = true })
	if held {
		close(f.entered)
		<-f.release
	}
	return intake.Result{}, nil
}

// letGo lets the held fact commit; a second call does nothing, so a test defers it.
func (f *factHold) letGo() { f.let.Do(func() { close(f.release) }) }

// waitIssue records LEGION-208 as a root running at at, generation 1, on acme/widgets.
func (h *waitHarness) waitIssue(at phase.Phase, edit func(*record.Issue)) {
	h.t.Helper()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: testProject, Title: "LEGION-208", Phase: at, Generation: 1, Status: "in_progress",
		HandedOver: true, Rank: "A"}
	if edit != nil {
		edit(&issue)
	}
	h.recordIssue(issue)
}

// waitingWorker is a live claim of role on LEGION-208 working the task of entry 3 of its phase, in
// a turn of that task, with its phases row on that entry: the worker of the open phase, in the turn
// it declares a wait in.
func (h *waitHarness) waitingWorker(t *testing.T, role claim.Role) liveClaim {
	t.Helper()
	c := newLiveClaim(t, h.harness, "LEGION-208", role)
	seedEntry(t, h.harness, "LEGION-208", role, 3)
	takeTask(t, h.harness, c.token, "outbox:entry-3", "the task of entry 3", 3, 1)
	return c
}

// declare posts c's declaration of a wait on waitingOn until wakeAt.
func (h *waitHarness) declare(c liveClaim, waitingOn string, wakeAt time.Time) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/wait", waitBody(c, waitingOn, wakeAt), nil)
}

func waitBody(c liveClaim, waitingOn string, wakeAt time.Time) WaitRequest {
	return WaitRequest{ClaimToken: c.token, SessionID: c.session, Secret: c.secret, WaitingOn: waitingOn,
		WakeAt: wakeAt.Format(time.RFC3339Nano)}
}

// declared asserts recorder is a recorded wait and answers it.
func declared(t *testing.T, recorder *httptest.ResponseRecorder) WaitView {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("declare = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var response WaitResponse
	decodeInto(t, recorder, &response)
	return response.Wait
}

// waitRows is every wait claim_waits holds for token, ended or not, oldest declaration first, and
// of two declared at one moment (the harness's clock stands still) the ended one first, which is
// the one the other replaced.
func waitRows(t *testing.T, h *harness, token claim.Token) []record.ClaimWait {
	t.Helper()
	rows, err := h.store.Pool().Query(context.Background(), `select id, project, issue, role, generation, phase,
		phase_entry, taken_entry, session, waiting_on, wake_at, declared_at, ended_at, coalesce(end_cause, ''),
		coalesce(ended_by, '') from claim_waits where claim_token = $1 order by declared_at, ended_at is null, id`, string(token))
	if err != nil {
		t.Fatalf("read the waits of %s: %v", token, err)
	}
	defer rows.Close()
	var waits []record.ClaimWait
	for rows.Next() {
		wait := record.ClaimWait{Token: token}
		var ended *time.Time
		if err := rows.Scan(&wait.ID, &wait.Project, &wait.Issue, &wait.Role, &wait.Generation, &wait.Phase,
			&wait.PhaseEntry, &wait.TakenEntry, &wait.Session, &wait.WaitingOn, &wait.WakeAt, &wait.DeclaredAt,
			&ended, &wait.EndCause, &wait.EndedBy); err != nil {
			t.Fatalf("read a wait of %s: %v", token, err)
		}
		if ended != nil {
			wait.EndedAt = *ended
		}
		waits = append(waits, wait)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the waits of %s: %v", token, err)
	}
	return waits
}

// waitStands is what WaitStands answers for c's wait on the record as it stands now, read as every
// reader of a wait reads it: the issue, its root, the role's phases row, the claim and the tree's
// gate under the deployment's steps.
func waitStands(t *testing.T, h *harness, c liveClaim, role claim.Role) bool {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	var stands bool
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		issue, err := records.Issue(ctx, tx, c.issue)
		if err != nil {
			return err
		}
		root, err := records.Issue(ctx, tx, issue.Tree)
		if err != nil {
			return err
		}
		rows, err := records.Phases(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		gates, err := records.GatesOf(ctx, tx, issue.Tree)
		if err != nil {
			return err
		}
		claims, err := records.RoleClaims(ctx, tx, []claim.Token{c.token})
		if err != nil {
			return err
		}
		_, stands = workflow.WaitStands(*issue, *root, record.PhaseRowOf(rows, role), record.RoleRun{Claim: claims[c.token]},
			record.TreeGate{Steps: config.DesignGateRootIssues.Steps(), Rows: gates})
		return nil
	}); err != nil {
		t.Fatalf("read whether the wait of %s stands: %v", c.token, err)
	}
	return stands
}

// putGate writes LEGION-208's spec gate at version, approved at that version when open.
func (h *waitHarness) putGate(t *testing.T, version int, open bool) {
	t.Helper()
	gate := record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: waitSpec, LatestVersion: version}
	if open {
		gate.ApprovedVersion, gate.EverOpened = &version, true
	}
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutGate(context.Background(), tx, gate)
	}); err != nil {
		t.Fatalf("put the spec gate of LEGION-208: %v", err)
	}
}

// logLine is the one line of logs whose message is msg, failing the test when there is none.
func logLine(t *testing.T, logs *testlog.Buffer, msg string) string {
	t.Helper()
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			return line
		}
	}
	t.Fatalf("no %q line was logged; the log is:\n%s", msg, logs.String())
	return ""
}

// wantLogged asserts line carries every one of fields, each written key=value.
func wantLogged(t *testing.T, line string, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if !strings.Contains(line, field) {
			t.Fatalf("the line %q does not carry %s", line, field)
		}
	}
}

// A declared wait is recorded with the id the answer names, the text, the entry of the task the
// claim had taken, the declaration's time and the wait's time, beside the generation, phase,
// phases-row entry and session it was declared at, and the declaration is logged with the
// claim, the issue, the role, the wait's id, its time and the taken entry. The taken entry is
// the entry of the confirmed delivery the claim holds when that delivery carries one, else the
// entry of the last confirmed delivery it retired (ServedEntry), and never the entry of a held
// task the worker has not confirmed, which supervise.Claim.ServingEntry would answer for a claim
// that has retired no entry (dispatch://INFRA-2040 D4).
func TestWaitIsRecordedWithTheEntryTheClaimHadTaken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim func(t *testing.T, h *waitHarness) liveClaim
		taken int64
	}{
		{
			name: "a confirmed delivery that carries an entry",
			claim: func(t *testing.T, h *waitHarness) liveClaim {
				c := newLiveClaim(t, h.harness, "LEGION-208", claim.RoleImplementer)
				seedEntry(t, h.harness, "LEGION-208", claim.RoleImplementer, 3)
				// Entry 2's task is retired, so ServedEntry is 2, and entry 3's task is the one
				// this turn confirmed: the taken entry is the confirmed delivery's.
				takeTask(t, h.harness, c.token, "outbox:entry-2", "the task of entry 2", 2, 1)
				takeTask(t, h.harness, c.token, "outbox:entry-3", "the task of entry 3", 3, 1)
				return c
			},
			taken: 3,
		},
		{
			name: "only a retired ServedEntry",
			claim: func(t *testing.T, h *waitHarness) liveClaim {
				c := newLiveClaim(t, h.harness, "LEGION-208", claim.RoleImplementer)
				seedEntry(t, h.harness, "LEGION-208", claim.RoleImplementer, 3)
				takeTask(t, h.harness, c.token, "outbox:entry-3", "the task of entry 3", 3, 1)
				endTurn(t, h.harness, c.token)
				machine, _ := h.supervisor.Machine(c.token)
				if held := machine.Claim(); held.Pending != nil || held.ServedEntry != 3 {
					t.Fatalf("the claim after its turn = pending %+v, served entry %d; want entry 3's task retired",
						held.Pending, held.ServedEntry)
				}
				return c
			},
			taken: 3,
		},
		{
			name: "a held task the worker never confirmed, on a claim that retired no entry",
			claim: func(t *testing.T, h *waitHarness) liveClaim {
				c := newLiveClaim(t, h.harness, "LEGION-208", claim.RoleImplementer)
				seedEntry(t, h.harness, "LEGION-208", claim.RoleImplementer, 3)
				queueTask(t, h.harness, c.token, "outbox:entry-3", "the task of entry 3", 3, 1)
				machine, _ := h.supervisor.Machine(c.token)
				if served := machine.Claim().ServingEntry(); served != 3 {
					t.Fatalf("ServingEntry = %d, want 3: the case is the claim ServingEntry answers the held task's entry for", served)
				}
				return c
			},
			taken: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWaitHarness(t)
			h.waitIssue(phase.Implementing, nil)
			c := tc.claim(t, h)
			wakeAt := waitNow.Add(2 * time.Hour)

			answer := declared(t, h.declare(c, "  the review of LEGION-207's pull request  ", wakeAt))
			if answer.ID == "" || answer.WaitingOn != "the review of LEGION-207's pull request" ||
				!answer.WakeAt.Equal(wakeAt) || !answer.DeclaredAt.Equal(waitNow) {
				t.Fatalf("the answer = %+v, want the wait's id, its trimmed text, its time %s and the declaration at %s",
					answer, wakeAt, waitNow)
			}
			rows := waitRows(t, h.harness, c.token)
			if len(rows) != 1 {
				t.Fatalf("claim_waits holds %d waits of %s, want the one declared: %+v", len(rows), c.token, rows)
			}
			got := rows[0]
			if got.ID != answer.ID || got.WaitingOn != answer.WaitingOn || got.TakenEntry != tc.taken ||
				!got.DeclaredAt.Equal(waitNow) || !got.WakeAt.Equal(wakeAt) || !got.EndedAt.IsZero() {
				t.Fatalf("the recorded wait = %+v, want id %s, its text, taken entry %d, declared at %s, its time %s and no end",
					got, answer.ID, tc.taken, waitNow, wakeAt)
			}
			if got.Project != testProject || got.Issue != "LEGION-208" || got.Role != claim.RoleImplementer ||
				got.Generation != 1 || got.Phase != phase.Implementing || got.PhaseEntry != 3 || got.Session != c.session {
				t.Fatalf("the recorded wait = %+v, want it declared on LEGION-208's implementer at generation 1, phase "+
					"implementing, entry 3, in session %s", got, c.session)
			}
			if !waitStands(t, h.harness, c, claim.RoleImplementer) {
				t.Fatal("the recorded wait does not stand: a declaration is recorded only where it stands at once")
			}
			wantLogged(t, logLine(t, h.logs, "wait: declared"), "claim="+string(c.token), "issue=LEGION-208",
				"role=implementer", "wait="+answer.ID, "wake_at="+wakeAt.Format(time.RFC3339), "taken_entry="+strconv.FormatInt(tc.taken, 10))
		})
	}
}

// A claim holds one standing wait: a second declaration ends the first as replaced, by the new
// wait's id, at the new declaration's time, and is the one that stands. The end is logged with
// its cause and what ended it.
func TestWaitANewDeclarationReplacesTheStandingOne(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)

	first := declared(t, h.declare(c, "the review of the root's pull request", waitNow.Add(time.Hour)))
	second := declared(t, h.declare(c, "CI on the rebased head", waitNow.Add(3*time.Hour)))

	rows := waitRows(t, h.harness, c.token)
	if len(rows) != 2 || rows[0].ID != first.ID || rows[1].ID != second.ID {
		t.Fatalf("claim_waits holds %+v, want the first wait and the second", rows)
	}
	if old := rows[0]; old.EndCause != record.WaitReplaced || old.EndedBy != second.ID || !old.EndedAt.Equal(waitNow) {
		t.Fatalf("the first wait = %+v, want it ended as %q by %s at %s", old, record.WaitReplaced, second.ID, waitNow)
	}
	if standing := rows[1]; !standing.EndedAt.IsZero() || standing.WaitingOn != "CI on the rebased head" {
		t.Fatalf("the second wait = %+v, want it standing", standing)
	}
	wantLogged(t, logLine(t, h.logs, "wait: ended"), "claim="+string(c.token), "wait="+first.ID,
		"cause="+record.WaitReplaced, "ended_by="+second.ID)
}

// The wait is the claim's, not its process's: a relaunch moves the claim's launch generation
// (store's putClaim, which writes every column of the claim and clears its activity report) and
// leaves the wait row as it was, still standing.
func TestWaitSurvivesARelaunchOfTheClaim(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	answer := declared(t, h.declare(c, "the review of the root's pull request", waitNow.Add(time.Hour)))
	before := h.stored(c.token).Generation

	h.relaunch(c.token)

	if after := h.stored(c.token).Generation; after <= before {
		t.Fatalf("the claim's launch generation is %d after the relaunch, want it past %d", after, before)
	}
	rows := waitRows(t, h.harness, c.token)
	if len(rows) != 1 || rows[0].ID != answer.ID || !rows[0].EndedAt.IsZero() || rows[0].Session != c.session {
		t.Fatalf("claim_waits holds %+v after the relaunch, want wait %s as it was declared, with no end", rows, answer.ID)
	}
	if !waitStands(t, h.harness, c, claim.RoleImplementer) {
		t.Fatal("the wait does not stand after the relaunch, which resumes the session that declared it")
	}
}

// Every refusal of D10 answers its code and a sentence that names the fault, records no wait,
// and is logged with its code. The text and the two times are the declaration's own (400); the
// claim working no open phase and the merger the design gate holds are the record's (409).
func TestEachWaitRefusalAnswersItsCodeAndRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// at is the phase LEGION-208 stands in when the claim is made; role is the claim's.
		at   phase.Phase
		role claim.Role
		// then changes the record once the claim works its task.
		then      func(t *testing.T, h *waitHarness)
		waitingOn string
		wakeAt    time.Time
		status    int
		code      string
		says      []string
	}{
		{
			name: "no text once trimmed", at: phase.Implementing, role: claim.RoleImplementer,
			waitingOn: " \t\n ", wakeAt: waitNow.Add(time.Hour),
			status: http.StatusBadRequest, code: "WAIT_TEXT_REQUIRED", says: []string{"waitingOn is empty"},
		},
		{
			name: "a time less than a minute ahead", at: phase.Implementing, role: claim.RoleImplementer,
			waitingOn: "CI", wakeAt: waitNow.Add(59 * time.Second),
			status: http.StatusBadRequest, code: "WAIT_TOO_SOON", says: []string{"is less than 1 minute after this declaration"},
		},
		{
			name: "a time before the declaration", at: phase.Implementing, role: claim.RoleImplementer,
			waitingOn: "CI", wakeAt: waitNow.Add(-time.Hour),
			status: http.StatusBadRequest, code: "WAIT_TOO_SOON", says: []string{"is less than 1 minute after this declaration"},
		},
		{
			name: "a time more than a day ahead", at: phase.Implementing, role: claim.RoleImplementer,
			waitingOn: "CI", wakeAt: waitNow.Add(25 * time.Hour),
			status: http.StatusBadRequest, code: "WAIT_TOO_FAR", says: []string{"is more than 24 hours after this declaration"},
		},
		{
			name: "another role's phase", at: phase.Implementing, role: claim.RoleImplementer,
			then:      func(t *testing.T, h *waitHarness) { h.waitIssue(phase.Testing, nil) },
			waitingOn: "CI", wakeAt: waitNow.Add(time.Hour),
			status: http.StatusConflict, code: "WAIT_NOT_OPEN_PHASE",
			says: []string{"LEGION-208 stands in testing, which is the tester's phase, not the implementer's"},
		},
		{
			name: "another generation", at: phase.Implementing, role: claim.RoleImplementer,
			then: func(t *testing.T, h *waitHarness) {
				h.waitIssue(phase.Implementing, func(issue *record.Issue) { issue.Generation = 2 })
			},
			waitingOn: "CI", wakeAt: waitNow.Add(time.Hour),
			status: http.StatusConflict, code: "WAIT_NOT_OPEN_PHASE",
			says: []string{"this claim's worker is on run 1 of LEGION-208, and the issue runs 2 now"},
		},
		{
			name: "a completed phases row", at: phase.Implementing, role: claim.RoleImplementer,
			then: func(t *testing.T, h *waitHarness) {
				if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
					return record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208",
						Role: claim.RoleImplementer, Claim: "implementer-claim", Entry: 3, CompletedAt: waitNow.Add(-time.Minute)})
				}); err != nil {
					t.Fatalf("complete the implementer's row: %v", err)
				}
			},
			waitingOn: "CI", wakeAt: waitNow.Add(time.Hour),
			status: http.StatusConflict, code: "WAIT_NOT_OPEN_PHASE",
			says: []string{"the implementer's phases row of LEGION-208 records the completion of its phase"},
		},
		{
			name: "an issue out of the workflow", at: phase.Implementing, role: claim.RoleImplementer,
			then: func(t *testing.T, h *waitHarness) {
				h.waitIssue(phase.Implementing, func(issue *record.Issue) { issue.Status = "backlog" })
			},
			waitingOn: "CI", wakeAt: waitNow.Add(time.Hour),
			status: http.StatusConflict, code: "WAIT_NOT_OPEN_PHASE",
			says: []string{"LEGION-208 is out of the workflow (Dispatch status backlog)"},
		},
		{
			name: "a lingering tree", at: phase.Implementing, role: claim.RoleImplementer,
			then: func(t *testing.T, h *waitHarness) {
				until := waitNow.Add(time.Hour)
				h.waitIssue(phase.Implementing, func(issue *record.Issue) { issue.LingerUntil = &until })
			},
			waitingOn: "CI", wakeAt: waitNow.Add(time.Hour),
			status: http.StatusConflict, code: "WAIT_NOT_OPEN_PHASE",
			says: []string{"the tree of LEGION-208 lingers after its close"},
		},
		{
			name: "a production check's role", at: phase.Testing, role: claim.RoleTester,
			then: func(t *testing.T, h *waitHarness) {
				h.waitIssue(phase.Done, func(issue *record.Issue) { issue.Status = "done" })
			},
			waitingOn: "the deploy", wakeAt: waitNow.Add(time.Hour),
			status: http.StatusConflict, code: "WAIT_NOT_OPEN_PHASE",
			says: []string{"LEGION-208 is done, so no phase of it is open, and a look of its production check is no phase"},
		},
		{
			name: "a merger the design gate holds", at: phase.Merging, role: claim.RoleMerger,
			then:      func(t *testing.T, h *waitHarness) { h.putGate(t, 2, false) },
			waitingOn: "the base branch", wakeAt: waitNow.Add(time.Hour),
			status: http.StatusConflict, code: "WAIT_HELD_BY_DESIGN_GATE",
			says: []string{"the design gate of LEGION-208 holds this merger: version 2 of its spec document (" + waitSpec + ")"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWaitHarness(t)
			h.waitIssue(tc.at, nil)
			c := h.waitingWorker(t, tc.role)
			if tc.then != nil {
				tc.then(t, h)
			}

			message := refused(t, h.declare(c, tc.waitingOn, tc.wakeAt), tc.status, tc.code)
			for _, want := range append(tc.says, "recorded nothing") {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal is %q, want it to say %q", message, want)
				}
			}
			if rows := waitRows(t, h.harness, c.token); len(rows) != 0 {
				t.Fatalf("a refused declaration left %+v in claim_waits", rows)
			}
			wantLogged(t, logLine(t, h.logs, "wait: refused"), "claim="+string(c.token), "issue=LEGION-208",
				"role="+string(tc.role), "code="+tc.code)
		})
	}
}

// A wait is the declaring session's own: a call naming another session than the one the claim
// records is refused as ready-for-update's fence refuses one, and records nothing.
func TestWaitFromAnotherSessionIsRefused(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)
	body := waitBody(c, "CI", waitNow.Add(time.Hour))
	body.SessionID = "ses_someone_else"

	wantRefusal(t, h.request(http.MethodPost, "/legion/v1/claims/wait", body, nil),
		claim.SameAgentRefusal.Status, claim.SameAgentRefusal.Message)
	if rows := waitRows(t, h.harness, c.token); len(rows) != 0 {
		t.Fatalf("a declaration from another session left %+v in claim_waits", rows)
	}
}

// The day is the most a wait looks ahead, measured from the declaration: a minute past it is
// refused WAIT_TOO_FAR and records nothing, and a minute inside it is recorded, which is the
// refusal's negative control.
func TestWaitAMinutePastTheMostIsRefusedAndAMinuteInsideIsRecorded(t *testing.T) {
	h := newWaitHarness(t)
	h.waitIssue(phase.Implementing, nil)
	c := h.waitingWorker(t, claim.RoleImplementer)

	refused(t, h.declare(c, "the person's answer", waitNow.Add(workflow.MaxWaitAhead+time.Minute)), http.StatusBadRequest, "WAIT_TOO_FAR")
	if rows := waitRows(t, h.harness, c.token); len(rows) != 0 {
		t.Fatalf("a wait a minute past the most left %+v in claim_waits", rows)
	}

	inside := waitNow.Add(workflow.MaxWaitAhead - time.Minute)
	answer := declared(t, h.declare(c, "the person's answer", inside))
	if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].ID != answer.ID || !rows[0].WakeAt.Equal(inside) {
		t.Fatalf("claim_waits holds %+v, want the wait a minute inside the most, at %s", rows, inside)
	}
}

// A merger's declaration and the design gate's closing applied at once leave no wait standing
// under the hold, whichever takes the fact lock first (dispatch://INFRA-2040, architect point 8).
// Both apply under the one lock every fact takes (intake.ApplyFact), as
// TestApplyFactSerializesConcurrentFacts holds: the second waits for the first to commit, then
// decides on what it wrote. With the closing first, the declaration finds the gate closed and is
// refused WAIT_HELD_BY_DESIGN_GATE. With the declaration first, the wait is recorded on the open
// gate and stands until the closing commits, and stands no longer once it has (WaitStands); the
// closing's own end of the row is not this test's to read.
func TestWaitAndTheGateClosingAppliedAtOnceLeaveNoWaitStandingUnderTheHold(t *testing.T) {
	closing := intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: waitSpec, Kind: intake.DispatchArtifactVersion, Version: 4}
	isClosing := func(fact intake.Fact) bool { _, ok := fact.(intake.DispatchArtifact); return ok }
	isDeclaration := func(fact intake.Fact) bool { _, ok := fact.(intake.WaitDeclared); return ok }

	arrange := func(t *testing.T) (*waitHarness, liveClaim) {
		t.Helper()
		h := newWaitHarness(t)
		h.waitIssue(phase.Merging, nil)
		h.putGate(t, 3, true)
		c := h.waitingWorker(t, claim.RoleMerger)
		t.Cleanup(h.first.letGo)
		t.Cleanup(h.second.letGo)
		return h, c
	}
	// apply runs on a goroutine of its own, so it reports and never fails the test itself.
	apply := func(h *waitHarness, id string, fact intake.Fact) <-chan error {
		done := make(chan error, 1)
		go func() {
			result, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", id, fact, h.handlers()...)
			if err == nil && result.Refusal != nil {
				err = fmt.Errorf("%s was refused %s: %s", id, result.Refusal.Code, result.Refusal.Message)
			}
			done <- err
		}()
		return done
	}
	declare := func(t *testing.T, h *waitHarness, c liveClaim) <-chan *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(waitBody(c, "the base branch's CI", waitNow.Add(time.Hour)))
		if err != nil {
			t.Fatalf("encode the declaration: %v", err)
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorder := httptest.NewRecorder()
			h.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/legion/v1/claims/wait", bytes.NewReader(encoded)))
			done <- recorder
		}()
		return done
	}
	waitsForTheLock := func(t *testing.T, h *waitHarness, what string) {
		t.Helper()
		testwait.Eventually(t, what, func() bool {
			var waiting int
			if err := h.store.Pool().QueryRow(context.Background(), `select count(*) from pg_stat_activity
				where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&waiting); err != nil {
				return false
			}
			return waiting == 1
		})
	}

	t.Run("the gate closes first", func(t *testing.T) {
		h, c := arrange(t)
		h.first.holds = isClosing
		closed := apply(h, "spec-4", closing)
		<-h.first.entered
		answered := declare(t, h, c)
		waitsForTheLock(t, h, "the declaration to wait for the closing's fact lock")
		h.first.letGo()

		if err := <-closed; err != nil {
			t.Fatalf("close the gate: %v", err)
		}
		message := refused(t, <-answered, http.StatusConflict, "WAIT_HELD_BY_DESIGN_GATE")
		if want := "version 4 of its spec document (" + waitSpec + ") is with its reviewer"; !strings.Contains(message, want) {
			t.Fatalf("the refusal is %q, want it to name the closed gate: %q", message, want)
		}
		if rows := waitRows(t, h.harness, c.token); len(rows) != 0 {
			t.Fatalf("a declaration applied after the closing left %+v in claim_waits", rows)
		}
	})

	t.Run("the declaration first", func(t *testing.T) {
		h, c := arrange(t)
		h.first.holds, h.second.holds = isDeclaration, isClosing
		answered := declare(t, h, c)
		<-h.first.entered
		closed := apply(h, "spec-4", closing)
		waitsForTheLock(t, h, "the closing to wait for the declaration's fact lock")
		h.first.letGo()

		answer := declared(t, <-answered)
		// The closing has the lock now and holds its transaction open: the wait it is about to
		// put under the hold is recorded, on the gate as it stood, and stands.
		<-h.second.entered
		if rows := waitRows(t, h.harness, c.token); len(rows) != 1 || rows[0].ID != answer.ID {
			t.Fatalf("claim_waits holds %+v, want the wait the declaration recorded", rows)
		}
		if !waitStands(t, h.harness, c, claim.RoleMerger) {
			t.Fatal("the wait does not stand before the closing commits")
		}
		h.second.letGo()

		if err := <-closed; err != nil {
			t.Fatalf("close the gate: %v", err)
		}
		if waitStands(t, h.harness, c, claim.RoleMerger) {
			t.Fatal("the merger's wait still stands once the closing committed, under the hold")
		}
	})
}
