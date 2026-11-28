package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// releaseRoot is the live tree every release of these tests belongs to.
const releaseRoot = "LEGION-208"

// releaseHarness is release_children over the daemon's whole handler chain (daemon/workflow.go,
// releaseHandlers): the mirror first, so the one fact that records a released child also records
// what Dispatch says it waits on, then the engine, then admission. The tree is live, its design
// gate approved only when open says so — the gate, not a blocker, is what holds every child of a
// tree whose design is not approved yet.
//
// Admission is in the chain because nothing else starts a released child any more: a child is an
// issue the admission cap counts, so its first phase begins when the waiting line gives it a slot
// (record.LineFirstPhase), not when the release records it. A harness without admission would
// answer every release "started nothing", which says nothing about the release. The cap is large
// enough that no wave of these tests queues behind it.
func releaseHarness(t *testing.T, open bool) (*harness, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	statuses := &statusRecorder{}
	// The server is given both names of the one project, and they differ: the claims it reads are
	// filed under the claim token, the issues and the mirror under the Dispatch key, and a route
	// that confuses them reads an empty mirror here.
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken,
		Controller:    h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: releaseHandlers(), Dispatch: statuses,
	}).Handler
	h.recordIssue(record.Issue{Key: releaseRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: releaseRoot,
		Project: testDispatchProject, Title: releaseRoot, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	gate := record.DesignGate{Issue: releaseRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1}
	if open {
		approved := 1
		gate.ApprovedVersion = &approved
	}
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: releaseRoot, Index: 0,
			AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, gate)
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	h.openTree(t, testDispatchProject, releaseRoot)
	statuses.issues = map[string]dispatch.Issue{
		releaseRoot: {Key: releaseRoot, ResolvedRepo: "acme/widgets", Status: "in_progress"},
	}
	return h, statuses
}

// releaseHandlers is the handler chain the release harness's server applies facts with, and the one
// the Dispatch consumer applies a delivered event with: the mirror, then the engine.
func releaseHandlers() []intake.Handler {
	records := record.NewStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	return []intake.Handler{blockers.NewMirror(records, testDispatchProject), engine, admit.New(records, engine, 8, testDispatchProject, log)}
}

// deliverEcho applies the todo event a release's write made, as the Dispatch consumer delivers it
// later: decoded from the event Dispatch published, with the daemon's own session as its actor, at
// the sequence Dispatch answered the write with, through the same handlers.
func deliverEcho(t *testing.T, h *harness, written dispatch.Issue) {
	t.Helper()
	parent := ""
	if written.Parent != nil {
		parent = *written.Parent
	}
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "echo-"+written.Key, intake.DispatchIssue{
		Key: written.Key, Seq: written.LastSeq, Type: "issue.updated", Status: written.Status, Title: written.Title, Parent: parent,
		DaemonActor: true, ResolvedRepo: written.ResolvedRepo, Blockers: written.BlockedBy, ClaimKnown: true,
	}, releaseHandlers()...); err != nil {
		t.Fatalf("deliver the todo event of %s: %v", written.Key, err)
	}
}

// outboxRows is every outbox row queued on issue, by kind: what the workflow started or told for it.
func outboxRows(t *testing.T, h *harness, issue string) map[string]int {
	t.Helper()
	rows := map[string]int{}
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		found, err := tx.Query(context.Background(), `select kind, count(*) from outbox where issue = $1 group by kind`, issue)
		if err != nil {
			return err
		}
		defer found.Close()
		for found.Next() {
			var kind string
			var count int
			if err := found.Scan(&kind, &count); err != nil {
				return err
			}
			rows[kind] = count
		}
		return found.Err()
	}); err != nil {
		t.Fatalf("read the outbox rows of %s: %v", issue, err)
	}
	return rows
}

// todoChild is a child Dispatch already shows todo, so the release writes nothing on it and records
// it from that view, and the workflow decides there and then whether its first phase starts.
func todoChild(key string, waits ...string) dispatch.Issue {
	parent := releaseRoot
	return dispatch.Issue{Key: key, ResolvedRepo: "acme/widgets", Title: key, Status: "todo", Parent: &parent, LastSeq: 7, BlockedBy: waits}
}

// backlogChild is a child Dispatch shows in backlog, so the release queues its todo and counts it
// on the record in the same request, before Dispatch takes the write (decision 5 of
// dispatch://INFRA-4730).
func backlogChild(key string, waits ...string) dispatch.Issue {
	child := todoChild(key, waits...)
	child.Status = "backlog"
	return child
}

// queuedTodos is each issue whose todo the outbox holds queued, in the order the rows were queued:
// what the release_children calls of a test wrote, which the outbox writes to Dispatch once the
// call's request has answered (decision 5 of dispatch://INFRA-4730), so no route writes to Dispatch
// itself (statusRecorder.writes stays empty). The in_progress a started child's first phase writes
// is the workflow's own row, and no write of the call's.
func queuedTodos(t *testing.T, h *harness) []string {
	t.Helper()
	var writes []string
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select issue from outbox where kind = 'dispatch_status' and payload->>'status' = 'todo' order by id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var issue string
			if err := rows.Scan(&issue); err != nil {
				return err
			}
			writes = append(writes, issue)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the queued status writes: %v", err)
	}
	return writes
}

// assertQueuedRows holds each write a status call's answer names to the outbox's status row of
// that id: the row is on that issue and writes that status.
func assertQueuedRows(t *testing.T, h *harness, queued []QueuedStatusWrite) {
	t.Helper()
	for _, write := range queued {
		var issue, status string
		if err := h.store.Pool().QueryRow(context.Background(), `select issue, payload->>'status' from outbox where id = $1 and kind = 'dispatch_status'`,
			write.Row).Scan(&issue, &status); err != nil || issue != write.Issue || status != write.Status {
			t.Fatalf("the outbox's status row %d = %s %s (%v), want the %s of %s the answer names", write.Row, issue, status, err, write.Status, write.Issue)
		}
	}
}

// seedMirrorRow writes what Dispatch says of an issue the release's own children wait on, as the
// mirror handler writes one from that issue's event.
func seedMirrorRow(t *testing.T, h *harness, key, status string) {
	t.Helper()
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: key, Project: testDispatchProject, Seq: 1,
			Status: record.Carry(status), Parent: record.Carry(""), Blockers: record.Carry[[]string](nil)})
	}); err != nil {
		t.Fatalf("seed the mirror row of %s: %v", key, err)
	}
}

// releaseWave posts one accepted release_children and keeps its body, which the answer's arrays
// are read back from unparsed. The answer's queued writes are held to the outbox's rows
// (assertQueuedRows) and left off the answer it returns, so a test reads them from the outbox
// (queuedTodos).
func releaseWave(t *testing.T, h *harness, architect liveClaim, issues ...string) (WaveReleaseResponse, string) {
	t.Helper()
	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": issues}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("wave release = %d: %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	var response WaveReleaseResponse
	decodeInto(t, recorder, &response)
	assertQueuedRows(t, h, response.Queued)
	response.Queued = nil
	return response, body
}

// A release is accepted whatever its children wait on: every one of them is released, and the
// answer says which of them this request actually started and which it held, with the unfinished
// issues each held child waits on. A blocker Dispatch has already finished is no part of the wait.
// Both lists follow the request's own order, so the architect reads them against the wave it sent.
func TestWaveReleaseAnswersWhatItStartedAndWhatItHeld(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	held, started, waiting := "LEGION-209", "LEGION-210", "LEGION-211"
	statuses.issues[held] = todoChild(held, "LEGION-300")
	statuses.issues[started] = todoChild(started)
	statuses.issues[waiting] = todoChild(waiting, "LEGION-301", "LEGION-302")
	seedMirrorRow(t, h, "LEGION-300", "in_progress")
	seedMirrorRow(t, h, "LEGION-301", "done")
	seedMirrorRow(t, h, "LEGION-302", "todo")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, held, started, waiting)

	want := WaveReleaseResponse{
		Released: []string{held, started, waiting},
		Started:  []string{started},
		Held:     []HeldChild{{Issue: held, BlockedBy: []string{"LEGION-300"}}, {Issue: waiting, BlockedBy: []string{"LEGION-302"}}},
	}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	for key, wantPhase := range map[string]phase.Phase{held: phase.Admitted, started: phase.Planning, waiting: phase.Admitted} {
		if recorded := recordedChild(t, h, key); recorded == nil || recorded.Phase != wantPhase {
			t.Fatalf("the record of %s = %#v, want it at %s", key, recorded, wantPhase)
		}
	}
}

// A child waiting on an issue the daemon has never observed has no mirror row to judge that issue
// by, and an unobserved issue is not a finished one: the release holds the child and names the
// issue it waits on, so the architect sees what the wait is even when the daemon has never heard
// of it. The answer's other lists are arrays, never null, for the plugin's reader.
func TestWaveReleaseHoldsAChildOnABlockerTheDaemonHasNeverObserved(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	child := "LEGION-209"
	statuses.issues[child] = todoChild(child, "LEGION-400")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, body := releaseWave(t, h, architect, child)

	want := WaveReleaseResponse{Released: []string{child}, Started: []string{}, Held: []HeldChild{{Issue: child, BlockedBy: []string{"LEGION-400"}}}}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	if !strings.Contains(body, `"started":[]`) {
		t.Fatalf("release body = %s, want an empty started array rather than null", body)
	}
	if recorded := recordedChild(t, h, child); recorded == nil || recorded.Phase != phase.Admitted {
		t.Fatalf("the record of %s = %#v, want it left admitted by the blocker the daemon has never seen", child, recorded)
	}
}

// A tree whose design gate is still closed starts none of the children a release names, and that
// is not a wait on another issue: every child is released, none is started, and none is held. The
// gate's own opening starts them (the workflow's advanceAdmittedTree), so calling them started
// here would be a lie and holding them would name a blocker that does not exist.
func TestWaveReleaseUnderAClosedDesignGateHoldsNoChild(t *testing.T) {
	h, statuses := releaseHarness(t, false)
	first, second := "LEGION-209", "LEGION-210"
	statuses.issues[first] = todoChild(first)
	statuses.issues[second] = todoChild(second)
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, body := releaseWave(t, h, architect, first, second)

	want := WaveReleaseResponse{Released: []string{first, second}, Started: []string{}, Held: []HeldChild{}}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	if !strings.Contains(body, `"started":[]`) || !strings.Contains(body, `"held":[]`) {
		t.Fatalf("release body = %s, want empty started and held arrays rather than null", body)
	}
	for _, key := range []string{first, second} {
		if recorded := recordedChild(t, h, key); recorded == nil || recorded.Phase != phase.Admitted {
			t.Fatalf("the record of %s = %#v, want it admitted behind the closed gate", key, recorded)
		}
	}
}

// landed is issue as Dispatch shows it once the outbox has written status on it: at that status, one
// sequence on from the view the call decided over.
func landed(issue dispatch.Issue, status string) dispatch.Issue {
	issue.Status, issue.LastSeq = status, issue.LastSeq+1
	return issue
}

// assertEchoMovesOnlyDispatchsView delivers the echo of the release's write of key, as the
// consumer delivers it once the outbox's write has landed (landed), and holds the record to what
// the release left, but for the status Dispatch shows and the sequence it has seen: the call
// counted the write already (R16 of dispatch://INFRA-3320), so the echo starts and tells nothing
// more.
func assertEchoMovesOnlyDispatchsView(t *testing.T, h *harness, statuses *statusRecorder, key string) {
	t.Helper()
	before, rows := recordedChild(t, h, key), outboxRows(t, h, key)
	echo := landed(statuses.issues[key], "todo")
	deliverEcho(t, h, echo)
	after := recordedChild(t, h, key)
	if after == nil || before == nil {
		t.Fatalf("the record of %s before its todo event = %#v, after = %#v, want both", key, before, after)
	}
	want := *before
	want.LastDispatchSeq, want.DispatchStatus = echo.LastSeq, "todo"
	if !reflect.DeepEqual(*after, want) {
		t.Fatalf("the record of %s after its todo event = %#v, want %#v: only Dispatch's status and sequence move", key, *after, want)
	}
	if again := outboxRows(t, h, key); !reflect.DeepEqual(again, rows) {
		t.Fatalf("outbox of %s after its todo event = %v, want %v: the event started or told nothing more", key, again, rows)
	}
}

// A child the release moves out of backlog is recorded by the release itself, in its own request,
// from the copy's reading of the child the route made, before Dispatch takes the queued todo (R16 of
// dispatch://INFRA-3320): a consumer running behind leaves no released child unrecorded and
// unstarted. The blocked one is held at admitted and named with its blocker; the free one is
// started. The record still shows the backlog Dispatch shows until the write's todo event, which,
// when the consumer delivers it, moves only that: no second record, no second start.
func TestWaveReleaseRecordsABacklogChildFromItsOwnWrite(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	blocked, free := "LEGION-209", "LEGION-210"
	statuses.issues[blocked] = backlogChild(blocked, "LEGION-300")
	statuses.issues[free] = backlogChild(free)
	seedMirrorRow(t, h, "LEGION-300", "in_progress")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, blocked, free)

	want := WaveReleaseResponse{
		Released: []string{blocked, free},
		Started:  []string{free},
		Held:     []HeldChild{{Issue: blocked, BlockedBy: []string{"LEGION-300"}}},
	}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	if want := []string{blocked, free}; !reflect.DeepEqual(queuedTodos(t, h), want) || len(statuses.writes) != 0 {
		t.Fatalf("queued todos = %v and Dispatch writes = %#v, want %v queued and none written in the request", queuedTodos(t, h), statuses.writes, want)
	}
	// A child the release starts shows its phase's status, in_progress, from its first phase; one
	// held at admitted has started nothing and stays todo.
	wantPhase := map[string]phase.Phase{blocked: phase.Admitted, free: phase.Planning}
	// The held child keeps the todo the release wrote, since admitted has no status of its own;
	// the free child's entry into planning writes in_progress over it.
	wantStatus := map[string]string{blocked: "todo", free: "in_progress"}
	for _, key := range []string{blocked, free} {
		recorded := recordedChild(t, h, key)
		if recorded == nil || recorded.Phase != wantPhase[key] || recorded.Generation != 1 || recorded.LastDispatchSeq != 7 || recorded.Status != wantStatus[key] ||
			recorded.DispatchStatus != "backlog" {
			t.Fatalf("the record of %s as the release answered = %#v, want it at %s, status %s, generation 1, Dispatch still shown backlog at the read's sequence 7",
				key, recorded, wantPhase[key], wantStatus[key])
		}
	}
	if rows := outboxRows(t, h, free); rows[string(record.OutboxKindSupervise)] == 0 {
		t.Fatalf("outbox of %s = %v, want its planner's start queued by the release", free, rows)
	}

	for _, key := range []string{blocked, free} {
		assertEchoMovesOnlyDispatchsView(t, h, statuses, key)
	}
}

// A parked child — its record at done, Dispatch at backlog — released again is re-entered by the
// release's own write in the release's request, as its todo event would re-enter it: a new run,
// held at admitted while it waits on an unfinished issue (so not started), or started at planning
// when it waits on nothing. The event itself, delivered later, moves only the record's view of
// Dispatch.
func TestWaveReleaseReentersAParkedChildFromItsOwnWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		waits   []string
		phase   phase.Phase
		status  string
		started []string
		held    []HeldChild
	}{
		{name: "waiting on an unfinished issue", waits: []string{"LEGION-300"}, phase: phase.Admitted, status: "todo", started: []string{},
			held: []HeldChild{{Issue: "LEGION-210", BlockedBy: []string{"LEGION-300"}}}},
		{name: "waiting on nothing", phase: phase.Planning, status: "in_progress", started: []string{"LEGION-210"}, held: []HeldChild{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, statuses := releaseHarness(t, true)
			child, parent := "LEGION-210", releaseRoot
			h.recordIssue(record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: releaseRoot, Parent: &parent,
				Project: testDispatchProject, Title: child, Phase: phase.Done, Generation: 1, Status: "backlog", DispatchStatus: "backlog", LastDispatchSeq: 7})
			statuses.issues[child] = backlogChild(child, tc.waits...)
			seedMirrorRow(t, h, "LEGION-300", "in_progress")
			architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

			response, _ := releaseWave(t, h, architect, child)

			want := WaveReleaseResponse{Released: []string{child}, Started: tc.started, Held: tc.held}
			if !reflect.DeepEqual(response, want) {
				t.Fatalf("release answered %#v, want %#v", response, want)
			}
			if want := []string{child}; !reflect.DeepEqual(queuedTodos(t, h), want) {
				t.Fatalf("queued todos = %v, want %v", queuedTodos(t, h), want)
			}
			recorded := recordedChild(t, h, child)
			if recorded == nil || recorded.Phase != tc.phase || recorded.Generation != 2 || recorded.Status != tc.status || recorded.DispatchStatus != "backlog" {
				t.Fatalf("the record of %s as the release answered = %#v, want its next run, generation 2, at %s, status %s, Dispatch still shown backlog", child, recorded, tc.phase, tc.status)
			}
			assertEchoMovesOnlyDispatchsView(t, h, statuses, child)
		})
	}
}

// A release names a child started only for a first phase it began: the record after the release
// runs a phase of a run that had none before it. A run already under way, a child still waiting at
// admitted, and a parked child its todo event has not re-entered yet were started by nothing here;
// a parked child that event did re-enter, as a new run, was.
func TestStartedByReleaseIsAFirstPhaseTheReleaseBegan(t *testing.T) {
	at := func(p phase.Phase, generation uint64) *record.Issue {
		return &record.Issue{Key: "LEGION-209", Phase: p, Generation: generation}
	}
	for _, tc := range []struct {
		name          string
		before, after *record.Issue
		want          bool
	}{
		{"unrecorded and still unrecorded", nil, nil, false},
		{"recorded and started", nil, at(phase.Planning, 1), true},
		{"recorded and held at admitted", nil, at(phase.Admitted, 1), false},
		{"admitted and started", at(phase.Admitted, 1), at(phase.Planning, 1), true},
		{"admitted and still admitted", at(phase.Admitted, 1), at(phase.Admitted, 1), false},
		{"already running", at(phase.Implementing, 1), at(phase.Implementing, 1), false},
		{"parked and not yet re-entered", at(phase.Done, 1), at(phase.Done, 1), false},
		{"parked and re-entered as a new run", at(phase.Done, 1), at(phase.Planning, 2), true},
	} {
		if got := startedByRelease(tc.before, tc.after); got != tc.want {
			t.Errorf("%s: startedByRelease = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A child whose detail read Dispatch keeps failing is still released and recorded: the release
// reads it from the project listing, which carries its status, its parent and its sequence. In a
// deployment, children already todo under a live root — released earlier, or given their parent
// link only after their release — were never recorded, and every release_children naming one
// answered 502 DISPATCH_FAILED "Dispatch read of <child> failed", while Dispatch's own reader read
// it. The listing is read once for the whole wave.
func TestWaveReleaseRecordsAChildWhoseDispatchDetailReadFails(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	alreadyTodo, unreleased := "LEGION-209", "LEGION-210"
	statuses.issues[alreadyTodo] = todoChild(alreadyTodo)
	triage := todoChild(unreleased)
	triage.Status = "triage"
	statuses.issues[unreleased] = triage
	readFailure := errors.New("call Dispatch GET /api/v1/issues: context deadline exceeded")
	statuses.readFails = map[string]error{alreadyTodo: readFailure, unreleased: readFailure}
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, alreadyTodo, unreleased)

	want := WaveReleaseResponse{Released: []string{alreadyTodo, unreleased}, Started: []string{alreadyTodo, unreleased}, Held: []HeldChild{}}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	for key, status := range map[string]string{alreadyTodo: "todo", unreleased: "triage"} {
		if recorded := recordedChild(t, h, key); recorded == nil || recorded.Phase != phase.Planning || recorded.LastDispatchSeq != 7 || recorded.DispatchStatus != status {
			t.Fatalf("the record of %s = %#v, want it planning, at the listing's sequence 7, Dispatch shown %s", key, recorded, status)
		}
	}
	if want := []string{unreleased}; !reflect.DeepEqual(queuedTodos(t, h), want) {
		t.Fatalf("queued todos = %v, want only the triage child's release", queuedTodos(t, h))
	}
	if listings := slices.Index(statuses.reads, "listing"); listings < 0 || slices.Contains(statuses.reads[listings+1:], "listing") {
		t.Fatalf("Dispatch reads = %q, want the listing read exactly once", statuses.reads)
	}
}

// A child whose detail read fails and that the listing does not carry either (the listing failed
// too) is not released: the answer names both failures, so the next one says what Dispatch
// answered, and nothing is written or recorded.
func TestWaveReleaseNamesAFailedReadTheListingDoesNotCover(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	child := "LEGION-209"
	statuses.issues[child] = todoChild(child)
	statuses.readFails = map[string]error{child: errors.New("detail read timed out")}
	statuses.listFails = errors.New("listing read timed out")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil)

	body := recorder.Body.String()
	refused(t, recorder, http.StatusBadGateway, "DISPATCH_FAILED")
	if !strings.Contains(body, "detail read timed out") || !strings.Contains(body, "listing read timed out") {
		t.Fatalf("answer = %s, want it to name both Dispatch failures", body)
	}
	if queued := queuedTodos(t, h); len(queued) != 0 || recordedChild(t, h, child) != nil {
		t.Fatalf("queued writes = %#v, record = %#v, want neither", queued, recordedChild(t, h, child))
	}
}

// A release whose write is queued stands even when the record does not take the write in the
// request (here the repository read the workflow needs fails): the release answers the child
// released with its todo queued, and the write's todo event, when the consumer delivers it once the
// write has landed, records it. Failing the call would send the architect to retry a release that
// already happened.
func TestWaveReleaseStandsWhenTheRecordDoesNotTakeALandedWrite(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	child := "LEGION-209"
	statuses.issues[child] = backlogChild(child)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: releaseHandlers(), Dispatch: statuses,
		PrepareRepository: func(context.Context, intake.DispatchIssue) (intake.DispatchIssue, error) {
			return intake.DispatchIssue{}, errors.New("GitHub did not answer the default branch read")
		},
	}).Handler
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, child)

	if want := (WaveReleaseResponse{Released: []string{child}, Started: []string{}, Held: []HeldChild{}}); !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	if want := []string{child}; !reflect.DeepEqual(queuedTodos(t, h), want) {
		t.Fatalf("queued todos = %v, want %v", queuedTodos(t, h), want)
	}
	if recorded := recordedChild(t, h, child); recorded != nil {
		t.Fatalf("the record of %s = %#v, want none before its todo event", child, recorded)
	}
	deliverEcho(t, h, landed(statuses.issues[child], "todo"))
	if recorded := recordedChild(t, h, child); recorded == nil || recorded.LastDispatchSeq != 8 {
		t.Fatalf("the record of %s after its todo event = %#v, want it recorded at sequence 8", child, recorded)
	}
}

// A todo child the release cannot record yet (its parent is outside the wave and unrecorded, so
// the answer is CHILD_NOT_RECORDED) does not hold back the rest of the wave: the call queues every
// other child's todo and counts it on the record before the stuck child answers.
func TestWaveReleaseWritesAndRecordsTheWaveBeforeAStuckTodoChildAnswers(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	stuck, unrecordedParent, free := "LEGION-209", "LEGION-299", "LEGION-210"
	parent := releaseRoot
	statuses.issues[unrecordedParent] = dispatch.Issue{Key: unrecordedParent, ResolvedRepo: "acme/widgets", Title: unrecordedParent, Status: "triage", Parent: &parent, LastSeq: 3}
	child := todoChild(stuck)
	child.Parent = &unrecordedParent
	statuses.issues[stuck] = child
	statuses.issues[free] = backlogChild(free)
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{stuck, free}}, nil)

	refused(t, recorder, http.StatusConflict, "CHILD_NOT_RECORDED")
	if want := []string{free}; !reflect.DeepEqual(queuedTodos(t, h), want) {
		t.Fatalf("queued todos = %v, want %s released whatever %s answered", queuedTodos(t, h), free, stuck)
	}
	if recorded := recordedChild(t, h, free); recorded == nil || recorded.Tree != releaseRoot || recorded.Phase != phase.Planning || recorded.Status != "in_progress" {
		t.Fatalf("the record of %s = %#v, want it recorded from its own write, planning", free, recorded)
	}
	if recorded := recordedChild(t, h, stuck); recorded != nil {
		t.Fatalf("the record of %s = %#v, want none while its parent is unrecorded", stuck, recorded)
	}
}

// A labelled child the release writes todo under a todo child of the wave that the call cannot
// record (CHILD_NOT_RECORDED) is queued with the rest of the wave, and is recorded as no root of its
// own: its Dispatch chain reaches the root in Legion's work, so neither the call, which records a
// child only under a recorded parent at work, nor admission takes it for a root (R15 of
// dispatch://INFRA-3320).
func TestWaveReleaseQueuesAChildUnderATodoChildItCannotRecordAndRecordsNeither(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	stuck, unrecordedParent, under := "LEGION-209", "LEGION-299", "LEGION-210"
	parent := releaseRoot
	statuses.issues[unrecordedParent] = dispatch.Issue{Key: unrecordedParent, ResolvedRepo: "acme/widgets", Title: unrecordedParent, Status: "triage", Parent: &parent, LastSeq: 3}
	child := todoChild(stuck)
	child.Parent = &unrecordedParent
	statuses.issues[stuck] = child
	statuses.issues[under] = labelledChild(under, stuck, "backlog")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{under, stuck}}, nil)

	refused(t, recorder, http.StatusConflict, "CHILD_NOT_RECORDED")
	if want := []string{under}; !reflect.DeepEqual(queuedTodos(t, h), want) {
		t.Fatalf("queued todos = %v, want %v", queuedTodos(t, h), want)
	}
	deliverEcho(t, h, landed(statuses.issues[under], "todo"))
	for _, key := range []string{under, stuck} {
		if recorded := recordedChild(t, h, key); recorded != nil {
			t.Fatalf("the record of %s once the todo of %s landed = %#v, want none: %s is unrecorded", key, under, recorded, stuck)
		}
	}
}

// Children the release writes todo under a child Dispatch already showed todo, and under those in
// turn, are recorded in the request with that todo child: each after its parent, a chain of the
// wave's own children, named in whatever order. Only the lowest starts: a parent whose Dispatch
// child is open starts none of its own work while it stands open (record.HeldChildrenFirst), so the
// two above it wait at admitted.
func TestWaveReleaseRecordsWrittenDescendantsOfATodoChildOfTheWave(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	top, middle, bottom := "LEGION-209", "LEGION-210", "LEGION-211"
	statuses.issues[top] = todoChild(top)
	middleChild := backlogChild(middle)
	middleChild.Parent = &top
	statuses.issues[middle] = middleChild
	bottomChild := backlogChild(bottom)
	bottomChild.Parent = &middle
	statuses.issues[bottom] = bottomChild
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, bottom, middle, top)

	if want := []string{bottom}; !reflect.DeepEqual(response.Started, want) {
		t.Fatalf("release answered started %v, want %v: the lowest child of the chain started, its parents waiting on it", response.Started, want)
	}
	if want := []string{middle, bottom}; !reflect.DeepEqual(queuedTodos(t, h), want) {
		t.Fatalf("queued todos = %v, want %v: each parent's before its child's", queuedTodos(t, h), want)
	}
	for key, at := range map[string]phase.Phase{top: phase.Admitted, middle: phase.Admitted, bottom: phase.Planning} {
		if recorded := recordedChild(t, h, key); recorded == nil || recorded.Tree != releaseRoot || recorded.Phase != at {
			t.Fatalf("the record of %s = %#v, want it in %s's tree at %s", key, recorded, releaseRoot, at)
		}
	}
}

// labelledChild is a child of parent carrying the legion label, in Dispatch status status.
func labelledChild(key, parent, status string) dispatch.Issue {
	child := todoChild(key)
	child.Parent, child.Status, child.HandedOver, child.Labels = &parent, status, true, []string{"legion"}
	return child
}

// A child carrying the legion label that the release writes todo under a child of the wave
// Dispatch already showed todo, and that the daemon has not recorded, joins the tree under that
// parent, which the call records first: recorded before its parent, no handler of the workflow
// would record it. It starts, and its parent waits on it (record.HeldChildrenFirst).
func TestWaveReleaseRecordsALabelledChildUnderAnUnrecordedTodoChildInItsTree(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	parent, labelled := "LEGION-209", "LEGION-210"
	statuses.issues[parent] = todoChild(parent)
	statuses.issues[labelled] = labelledChild(labelled, parent, "backlog")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, labelled, parent)

	if want := []string{labelled}; !reflect.DeepEqual(response.Started, want) {
		t.Fatalf("release answered started %v, want %v", response.Started, want)
	}
	if recorded := recordedChild(t, h, labelled); recorded == nil || recorded.Tree != releaseRoot || recorded.Parent == nil || *recorded.Parent != parent {
		t.Fatalf("the record of %s = %#v, want it under %s in %s's tree, not a root of its own", labelled, recorded, parent, releaseRoot)
	}
	if recorded := recordedChild(t, h, parent); recorded == nil || recorded.Phase != phase.Admitted {
		t.Fatalf("the record of %s = %#v, want it admitted, waiting on its child", parent, recorded)
	}
}

// The same holds a level further down: a labelled child Dispatch already showed todo, under a child
// the release writes todo, under a todo child of the wave, is recorded only after the written child
// is, and joins the tree.
func TestWaveReleaseRecordsATodoChildUnderAWrittenChildOfTheWaveInItsTree(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	top, middle, bottom := "LEGION-209", "LEGION-210", "LEGION-211"
	statuses.issues[top] = todoChild(top)
	written := backlogChild(middle)
	written.Parent = &top
	statuses.issues[middle] = written
	statuses.issues[bottom] = labelledChild(bottom, middle, "todo")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, bottom, middle, top)

	if want := []string{bottom}; !reflect.DeepEqual(response.Started, want) {
		t.Fatalf("release answered started %v, want %v", response.Started, want)
	}
	for _, key := range []string{top, middle, bottom} {
		if recorded := recordedChild(t, h, key); recorded == nil || recorded.Tree != releaseRoot {
			t.Fatalf("the record of %s = %#v, want it in %s's tree", key, recorded, releaseRoot)
		}
	}
}

// A wave that names a written child twice still records that child before the labelled todo child
// under it: named once, it has its own depth in releaseOrder, not that depth counted once per
// naming, which put it after its child and left the child unrecorded.
func TestWaveReleaseNamingAChildTwiceKeepsItAheadOfItsChild(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	top, middle, bottom := "LEGION-209", "LEGION-210", "LEGION-211"
	statuses.issues[top] = todoChild(top)
	written := backlogChild(middle)
	written.Parent = &top
	statuses.issues[middle] = written
	statuses.issues[bottom] = labelledChild(bottom, middle, "todo")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, top, bottom, middle, middle)

	for _, key := range []string{top, middle, bottom} {
		if recorded := recordedChild(t, h, key); recorded == nil || recorded.Tree != releaseRoot {
			t.Errorf("the record of %s = %#v, want it in %s's tree, not a root of its own", key, recorded, releaseRoot)
		}
	}
	if want := []string{top, bottom, middle}; !reflect.DeepEqual(response.Released, want) {
		t.Errorf("release answered released %v, want %v", response.Released, want)
	}
	if want := []string{middle}; !reflect.DeepEqual(queuedTodos(t, h), want) {
		t.Errorf("queued todos = %v, want %s written once", queuedTodos(t, h), middle)
	}
}

// A todo child whose parent is outside the wave and not recorded yet is answered
// CHILD_NOT_RECORDED, and the architect's retry, once the parent's own todo event has recorded it,
// records the child: each attempt is its own fact, so the retry applies Dispatch's view again.
func TestWaveReleaseRetryRecordsATodoChildOnceItsParentIsRecorded(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	child, parentKey, root := "LEGION-210", "LEGION-209", releaseRoot
	statuses.issues[parentKey] = dispatch.Issue{Key: parentKey, ResolvedRepo: "acme/widgets", Title: parentKey, Status: "triage", Parent: &root, LastSeq: 2}
	todo := todoChild(child)
	todo.Parent, todo.LastSeq = &parentKey, 5
	statuses.issues[child] = todo
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)
	release := func() *httptest.ResponseRecorder {
		t.Helper()
		return h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil)
	}

	refused(t, release(), http.StatusConflict, "CHILD_NOT_RECORDED")
	if got := recordedChild(t, h, child); got != nil {
		t.Fatalf("the child before its parent is recorded = %#v, want none", got)
	}
	// The parent's own todo event, from its release by someone else, reaches the daemon.
	statuses.issues[parentKey] = dispatch.Issue{Key: parentKey, ResolvedRepo: "acme/widgets", Title: parentKey, Status: "todo", Parent: &root, LastSeq: 3}
	deliverEcho(t, h, statuses.issues[parentKey])

	if recorder := release(); recorder.Code != http.StatusOK {
		t.Fatalf("the architect's retry = %d: %s", recorder.Code, recorder.Body)
	}
	if got := recordedChild(t, h, child); got == nil || got.Tree != releaseRoot || got.LastDispatchSeq != 5 {
		t.Fatalf("the child after the retry = %#v, want it recorded in %s's tree at Dispatch's sequence 5", got, releaseRoot)
	}
	if queued := queuedTodos(t, h); len(queued) != 0 {
		t.Fatalf("queued writes = %#v, want none: the child was already todo", queued)
	}
}
