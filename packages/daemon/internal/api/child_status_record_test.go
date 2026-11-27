package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"
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

// statusRoot and statusChild are the live tree and the running child these tests move.
const (
	statusRoot  = "LEGION-208"
	statusChild = "LEGION-209"
)

// statusHarness is the architect's and the controller's status routes over the daemon's own handler
// chain (daemon/workflow.go: the mirror, the engine, admission), with no Dispatch event consumer:
// whatever moves the record moves it inside the route's own request. The tree LEGION-208 is live
// (slotted, its design gate approved) and its child LEGION-209 is in planning at Dispatch event
// sequence 4.
func statusHarness(t *testing.T) (*harness, []intake.Handler, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quiet)
	handlers := []intake.Handler{blockers.NewMirror(records, testDispatchProject), engine, admit.New(records, engine, 2, testDispatchProject, quiet)}
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: handlers, Dispatch: statuses, PrepareRepository: testRepositories.prepare,
	}).Handler
	root := statusRoot
	h.recordIssue(record.Issue{Key: statusRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot,
		Project: testDispatchProject, Title: statusRoot, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "A", HandedOver: true, LastDispatchSeq: 3})
	h.recordIssue(record.Issue{Key: statusChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot, Parent: &root,
		Project: testDispatchProject, Title: "the child", Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "B", LastDispatchSeq: 4})
	// The child's planner runs, so its claim is stored: a park writes one stop for each stored claim
	// of the child (the workflow's everyClaim).
	if _, err := h.store.Pool().Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', $2, $3, 'planner', 1, '', '', 'working', 0, 0, 0, 0)`,
		"legion-legion-legion-209-planner", statusRoot, statusChild); err != nil {
		t.Fatalf("store the child's planner claim: %v", err)
	}
	approved := 1
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: statusRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, record.DesignGate{Issue: statusRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	h.openTree(t, testDispatchProject, statusRoot)
	statuses.issues = map[string]dispatch.Issue{
		statusRoot:  {Key: statusRoot, ResolvedRepo: "acme/widgets", Title: statusRoot, Status: "in_progress", Rank: "A", Labels: []string{"legion"}, HandedOver: true, LastSeq: 3},
		statusChild: {Key: statusChild, ResolvedRepo: "acme/widgets", Title: "the child", Status: "in_progress", Parent: &root, Rank: "B", LastSeq: 4},
	}
	return h, handlers, statuses
}

// statusEcho is the Dispatch event the daemon's own status write of the child makes, as the
// consumer decodes it (intake's decodeDispatchEvent): it arrives after the route has answered,
// behind every event queued before it.
func statusEcho(seq int64, status string) intake.DispatchIssue {
	return intake.DispatchIssue{Key: statusChild, Seq: seq, Type: "issue.updated", Status: status, Title: "the child", Parent: statusRoot, Rank: "B",
		ResolvedRepo: "acme/widgets", DaemonActor: true, ClaimKnown: true}
}

// superviseRows counts the child's queued supervise rows of op for role. A phase worker's start
// is one row, which carries the task and waits for the issue's admission slot
// (workflow.StartOrOwe); a start with no task at all is a role admission brings back beside it,
// which these tests do not count, as every other assertion about a start in this tree does not.
func superviseRows(t *testing.T, h *harness, op string, role claim.Role) int {
	t.Helper()
	var rows int
	if err := h.store.Pool().QueryRow(context.Background(),
		`select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = $2 and payload->>'role' = $3
			and (payload->>'op' <> 'start' or coalesce(payload->>'task', '') <> '')`,
		statusChild, op, string(role)).Scan(&rows); err != nil {
		t.Fatalf("count the %s rows of %s: %v", op, role, err)
	}
	return rows
}

// assertEchoChangesNothing applies the route's own Dispatch event once the route has answered, as
// the lagging consumer finally delivers it, and holds the child's record and its queued rows to what
// the route left.
func assertEchoChangesNothing(t *testing.T, h *harness, handlers []intake.Handler, echo intake.DispatchIssue) {
	t.Helper()
	before, rows := recordedChild(t, h, statusChild), outboxRows(t, h, statusChild)
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "echo", echo, handlers...); err != nil {
		t.Fatalf("apply the echo of the route's own write: %v", err)
	}
	if after := recordedChild(t, h, statusChild); !reflect.DeepEqual(after, before) {
		t.Fatalf("the child after its own write's echo = %#v, want it as the route left it, %#v", after, before)
	}
	if after := outboxRows(t, h, statusChild); !reflect.DeepEqual(after, rows) {
		t.Fatalf("the child's outbox rows after its own write's echo = %v, want the %v the route left", after, rows)
	}
}

// An older plugin still offers its agents an operation that called a child route this daemon no
// longer serves. The route stays registered only to refuse that call: 410 ROUTE_REMOVED, with a
// reason that says which status to write on the child in Dispatch, followed by the rules of that
// write (record.EndChildRule), so the older plugin's tool shows its agent what to do instead. The
// refusal reads no body, so a call with no body or with a body that does not decode gets the same
// answer. It changes nothing: no Dispatch write, no change to the child's record, no outbox row
// and no fact applied.
func TestTheRemovedChildRouteAnswersWithTheStatusWrite(t *testing.T) {
	h, _, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	grant := architect.grant(t)
	const reason = "This route is removed. Write backlog, icebox or done on the child in Dispatch (dispatch issue-update --issue <KEY> --status <status>). " +
		"Add --reason when the status is done. The daemon takes the move from the Dispatch event of that write. " + record.EndChildRule
	count := func(table string) int {
		t.Helper()
		var rows int
		if err := h.store.Pool().QueryRow(context.Background(), "select count(*) from "+table).Scan(&rows); err != nil {
			t.Fatalf("count the rows of %s: %v", table, err)
		}
		return rows
	}
	child, outbox, facts := recordedChild(t, h, statusChild), count("outbox"), count("processed_events")

	for _, tc := range []struct {
		name string
		body any
	}{
		{name: "an architect's request for its running child", body: map[string]any{"grantId": grant, "issue": statusChild}},
		{name: "no body"},
		{name: "a body that does not decode", body: `{"grantId":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusedSaying(t, h.request(http.MethodPost, "/legion/v1/children/park", tc.body, nil), http.StatusGone, "ROUTE_REMOVED", reason)
		})
	}
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none", statuses.writes)
	}
	if after := recordedChild(t, h, statusChild); !reflect.DeepEqual(after, child) {
		t.Fatalf("the child after the refusals = %#v, want it as before, %#v", after, child)
	}
	if after := count("outbox"); after != outbox {
		t.Fatalf("outbox rows after the refusals = %d, want the %d before them", after, outbox)
	}
	if after := count("processed_events"); after != facts {
		t.Fatalf("applied facts after the refusals = %d, want the %d before them", after, facts)
	}
}

// The rules for ending a child, and the sentence on a status the daemon has not taken yet, are
// each one text in two languages. The daemon's refusals and notices over children carry
// record.EndChildRule and record.StatusNotTakenYet, and the legion tool's description
// (`packages/pi-legion/src/tools.ts`) carries the same words. This golden is where the two meet,
// as stale-entry.json is for HANDOFF_STALE_ENTRY: `packages/pi-legion/src/tools.test.ts` holds the
// description to the file. So a word changed on the Go side fails here until the fixture is
// written again (`go test ./internal/api/ -update`), and then fails there until the tool's text
// says it too.
func TestTheEndChildRuleGolden(t *testing.T) {
	golden(t, "end-child-rule.json", map[string]string{"rule": record.EndChildRule, "notTakenYet": record.StatusNotTakenYet})
}

// The restart of a running child is its owning architect's backlog, then rerun_child
// (dispatch://INFRA-3178/spec R5). The root architect writes backlog on the child in Dispatch, and
// the daemon takes that write from its event as it takes a person's move: the child leaves the
// workflow at the write's own sequence. rerun_child then starts the child's next run in the record
// within its own request, as the todo move's event would, without waiting for that event: the
// rerun writes todo to Dispatch, and the child's entry into planning writes in_progress, the
// status of that phase, over it. The write's echo, once it arrives, changes nothing.
func TestTheOwningArchitectsBacklogThenRerunChildRestartsTheChild(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	backlog := intake.DispatchIssue{Key: statusChild, Seq: 5, Type: "issue.updated", Status: "backlog", Title: "the child", Parent: statusRoot, Rank: "B",
		ResolvedRepo: "acme/widgets", ActorSession: architect.session}
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "owner-backlog", backlog, handlers...); err != nil {
		t.Fatalf("apply the root architect's backlog of the child: %v", err)
	}
	shown := statuses.issues[statusChild]
	shown.Status, shown.LastSeq = "backlog", 5
	statuses.issues[statusChild] = shown
	out := recordedChild(t, h, statusChild)
	if out == nil || out.Phase != phase.Done || out.Status != "backlog" || out.LastDispatchSeq != 5 || out.Generation != 1 {
		t.Fatalf("the child after its owning architect's backlog = %#v, want it out of the workflow (phase done, status backlog) at the write's sequence 5", out)
	}
	planners := superviseRows(t, h, "start", claim.RolePlanner)

	recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("rerun = %d %s, want 200 {}", recorder.Code, recorder.Body)
	}
	if want := []statusWrite{{issue: statusChild, status: "todo"}}; !reflect.DeepEqual(statuses.writes, want) {
		t.Fatalf("Dispatch writes = %#v, want %#v: the architect's own backlog is no write of the daemon", statuses.writes, want)
	}
	rerun := recordedChild(t, h, statusChild)
	if rerun == nil || rerun.Phase != phase.Planning || rerun.Status != "in_progress" || rerun.LastDispatchSeq != 6 || rerun.Generation != 2 {
		t.Fatalf("the child's record once rerun_child answered = %#v, want its next run (generation 2) planning, in_progress as planning's status, at its write's sequence 6", rerun)
	}
	if got := superviseRows(t, h, "start", claim.RolePlanner); got != planners+1 {
		t.Fatalf("planner start rows = %d, want the rerun to start the planner once more than the %d before it", got, planners)
	}

	assertEchoChangesNothing(t, h, handlers, statusEcho(6, "todo"))
}

// The controller's set_status moves the record within its own request too: the controller parking
// a running child stops it at once, and the echo changes nothing.
func TestControllerStatusParksTheRecordWithoutWaitingForItsDispatchEvent(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: statusChild, Status: "backlog"}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("set_status = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if len(statuses.writes) != 1 || statuses.writes[0] != (statusWrite{issue: statusChild, status: "backlog"}) {
		t.Fatalf("Dispatch writes = %#v, want the child's backlog", statuses.writes)
	}
	parked := recordedChild(t, h, statusChild)
	if parked == nil || parked.Phase != phase.Done || parked.Status != "backlog" || parked.LastDispatchSeq != 5 {
		t.Fatalf("the child's record once set_status answered = %#v, want it parked at its write's sequence 5", parked)
	}
	if got := superviseRows(t, h, "suspend", claim.RolePlanner); got != 1 {
		t.Fatalf("planner suspend rows = %d, want the move to stop the planner once", got)
	}

	assertEchoChangesNothing(t, h, handlers, statusEcho(5, "backlog"))
}

// The controller's set_status can name an issue of another Dispatch project, whose events the
// consumer drops: the route's own write of it records nothing either, so the daemon never admits a
// foreign issue.
func TestControllerStatusOnAnotherProjectsIssueRecordsNothing(t *testing.T) {
	h, _, statuses := statusHarness(t)
	const foreign = "OTHER-12"
	statuses.issues[foreign] = dispatch.Issue{Key: foreign, ResolvedRepo: "acme/widgets", Title: foreign, Status: "backlog", Labels: []string{"legion"}, HandedOver: true, LastSeq: 7}
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: foreign, Status: "todo"}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("set_status = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if len(statuses.writes) != 1 || statuses.writes[0] != (statusWrite{issue: foreign, status: "todo"}) {
		t.Fatalf("Dispatch writes = %#v, want the foreign issue's todo", statuses.writes)
	}
	if got := recordedChild(t, h, foreign); got != nil {
		t.Fatalf("the record of %s = %#v, want none: it is another Dispatch project's", foreign, got)
	}
	if got := outboxRows(t, h, foreign); len(got) != 0 {
		t.Fatalf("outbox rows of %s = %v, want none", foreign, got)
	}
}

// close_root ends only a tree root. Its refusal of a child tells the architect how a child leaves
// the workflow instead: by the status write of the architect that owns it, in Dispatch
// (record.EndChildRule), and never by an operation the daemon no longer serves (dispatch://INFRA-3178
// R6). Each check that refuses it says so: the route's check of the issue the request names
// (rootArchitectOf), and the workflow's check of the record the close fact reads (workflow's
// closeRoot). Each row holds only that much of the wording.
func TestTheRootRequiredRefusalNamesTheStatusWrite(t *testing.T) {
	h, handlers, _ := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	for _, tc := range []struct {
		name string
		text func(t *testing.T) string
	}{
		{"the API", func(t *testing.T) string {
			return refused(t, h.request(http.MethodPost, "/legion/v1/roots/close", map[string]any{
				"grantId": architect.grant(t), "issue": statusChild, "reason": "moot",
			}, nil), http.StatusForbidden, "ROOT_REQUIRED")
		}},
		{"the workflow", func(t *testing.T) string {
			result, err := intake.ApplyFact(context.Background(), h.store.Pool(), "api", "close-a-child", intake.CloseRoot{Issue: statusChild, Reason: "moot"}, handlers...)
			if err != nil || result.Refusal == nil || result.Refusal.Code != "ROOT_REQUIRED" {
				t.Fatalf("the close fact of %s = %+v, %v; want ROOT_REQUIRED", statusChild, result.Refusal, err)
			}
			return result.Refusal.Message
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text(t)
			if !strings.Contains(text, "dispatch issue-update") || !strings.Contains(text, "--status done") {
				t.Errorf("ROOT_REQUIRED from %s says %q, want it to name the architect's status write (dispatch issue-update … --status done)", tc.name, text)
			}
			if strings.Contains(strings.ToLower(text), "park") {
				t.Errorf("ROOT_REQUIRED from %s says %q, which names a removed operation or the old word for the move", tc.name, text)
			}
		})
	}
}
