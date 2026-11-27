package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
// sequence 4. Each of configure sets an option of the server first, such as a test's seam.
func statusHarness(t *testing.T, configure ...func(*Options)) (*harness, []intake.Handler, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quiet)
	handlers := []intake.Handler{blockers.NewMirror(records, testDispatchProject), engine, admit.New(records, engine, 2, testDispatchProject, quiet)}
	statuses := &statusRecorder{}
	opts := Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: handlers, Dispatch: statuses,
	}
	for _, set := range configure {
		set(&opts)
	}
	h.handler = NewServer("127.0.0.1", 8437, opts).Handler
	root := statusRoot
	h.recordIssue(record.Issue{Key: statusRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot,
		Project: testDispatchProject, Title: statusRoot, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "A", HandedOver: true, LastDispatchSeq: 3})
	h.recordIssue(record.Issue{Key: statusChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot, Parent: &root,
		Project: testDispatchProject, Title: "the child", Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "B", LastDispatchSeq: 4})
	approved := 1
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: statusRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		if err := records.PutGate(context.Background(), tx, record.DesignGate{Issue: statusRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			return err
		}
		// The copy of Dispatch's issues holds the chain as Dispatch shows it (statuses.issues below),
		// as the mirror of a running daemon does: a chain the copy cannot show to its top is unknown,
		// and a session's status write on it is held until it is known (R16 of dispatch://INFRA-3320).
		for _, reading := range []record.DispatchReading{
			{Key: statusRoot, Project: testDispatchProject, Seq: 3, Status: record.Carry("in_progress"), Parent: record.Carry(""), Labels: record.Carry([]string{"legion"})},
			{Key: statusChild, Project: testDispatchProject, Seq: 4, Status: record.Carry("in_progress"), Parent: record.Carry(statusRoot), Labels: record.Carry([]string{})},
		} {
			if err := records.PutDispatchIssue(context.Background(), tx, reading); err != nil {
				return err
			}
		}
		return nil
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

// No child route stands but the one that refuses an older plugin (cybermonklabs/legion#4573,
// spec v13 of dispatch://INFRA-4730). The removed child operation stays only as a route that
// answers 410 ROUTE_REMOVED with the rules for ending a child (record.EndChildRule), whatever
// body its caller sends: the architect's request of contract 205 and before, the status and
// children a later plugin would have sent, none, and one that does not decode. Its path stays the
// one main serves until main renames it. The daemon serves no route for the other removed child
// operation: its path answers the router's no-route 404. And neither the legion tool's operations
// (packages/pi-legion/src/tools.ts) nor @legion/pi-shared's daemon client
// (packages/pi-shared/src/daemon-client.ts) names either operation or its route, so no agent is
// offered a call that cannot work.
//
// Negative control, run when this was written: with removedChildRoute answering 404 in place of
// 410, the removed child route's rows fail.
func TestTheRemovedChildOperationsAnswerGone(t *testing.T) {
	h, _, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	grant := architect.grant(t)
	for _, tc := range []struct {
		name string
		body any
	}{
		{name: "an architect's backlog of its running child", body: map[string]any{"grantId": grant, "issue": statusChild}},
		{name: "a request with a status and a children argument", body: map[string]any{"grantId": grant, "issue": statusChild, "status": "icebox", "children": "include"}},
		{name: "no body"},
		{name: "a body that does not decode", body: `{"grantId":`},
	} {
		t.Run("removed child route: "+tc.name, func(t *testing.T) {
			if reason := refused(t, h.request(http.MethodPost, "/legion/v1/children/park", tc.body, nil), http.StatusGone, "ROUTE_REMOVED"); !strings.HasSuffix(reason, record.EndChildRule) {
				t.Fatalf("the removed child route's refusal says %q, want it to end with the rules for ending a child", reason)
			}
		})
	}
	t.Run("close_child", func(t *testing.T) {
		if recorder := h.request(http.MethodPost, "/legion/v1/children/close", map[string]any{"grantId": grant, "issue": statusChild, "reason": "moot"}, nil); recorder.Code != http.StatusNotFound {
			t.Fatalf("POST /legion/v1/children/close = %d %s, want the router's 404: the daemon serves no close_child route", recorder.Code, recorder.Body)
		}
	})
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none", statuses.writes)
	}
	for _, source := range []string{
		filepath.Join("..", "..", "..", "pi-legion", "src", "tools.ts"),
		filepath.Join("..", "..", "..", "pi-shared", "src", "daemon-client.ts"),
	} {
		text, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		for _, name := range []string{"close_child", "park_child", "/legion/v1/children/close", "/legion/v1/children/park"} {
			if strings.Contains(string(text), name) {
				t.Errorf("%s names %s, which the daemon does not serve", source, name)
			}
		}
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

// statusRows is every status row of issue the outbox holds, by id, in id order.
func statusRows(t *testing.T, h *harness, issue string) map[int64]record.StatusWrite {
	t.Helper()
	rows := map[int64]record.StatusWrite{}
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		queued, err := tx.Query(ctx, `select id, payload from outbox where issue = $1 and kind = 'dispatch_status' order by id`, issue)
		if err != nil {
			return err
		}
		defer queued.Close()
		for queued.Next() {
			var id int64
			var payload []byte
			if err := queued.Scan(&id, &payload); err != nil {
				return err
			}
			var write record.StatusWrite
			if err := json.Unmarshal(payload, &write); err != nil {
				return err
			}
			rows[id] = write
		}
		return queued.Err()
	}); err != nil {
		t.Fatalf("read the status rows of %s: %v", issue, err)
	}
	return rows
}

// queuedWrite is the one write a status call's answer names, held to issue and status, and the
// status row of that id the outbox holds for issue.
func queuedWrite(t *testing.T, h *harness, recorder *httptest.ResponseRecorder, issue, status string) record.StatusWrite {
	t.Helper()
	var answer StatusCallResponse
	decodeInto(t, recorder, &answer)
	if len(answer.Queued) != 1 || answer.Queued[0].Issue != issue || answer.Queued[0].Status != status {
		t.Fatalf("the call answered %+v, want one queued write of %s on %s", answer, status, issue)
	}
	row, held := statusRows(t, h, issue)[answer.Queued[0].Row]
	if !held || row.Status != status {
		t.Fatalf("the outbox's status row %d of %s = %+v (held %v), want the write the answer named", answer.Queued[0].Row, issue, row, held)
	}
	return row
}

// rerun_child on a child Dispatch shows in backlog or icebox opens a new run of it, whoever wrote
// that status (decision 8 of dispatch://INFRA-4730, R20 of dispatch://INFRA-3320): the backlog of
// the architect of the issue above it, written as its own session, which the daemon takes as a
// person's move (the repair, decision 24), and a person's icebox alike. So the daemon keeps no
// mark of who stopped a child. The call writes nothing to Dispatch in its request: it queues the
// child's todo as a status row (decision 5), which its answer names, and counts it on the record
// before Dispatch echoes it, the new run, its generation raised, starting from its first phase. The
// todo's echo, once it arrives, opens no second run.
func TestRerunChildAfterTheArchitectsBacklogOpensANewRun(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		byArchitect  bool
	}{
		{"the architect above writes backlog as its own session", "backlog", true},
		{"a person writes icebox", "icebox", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, handlers, statuses := statusHarness(t)
			architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
			stop := intake.DispatchIssue{Key: statusChild, Seq: 5, Type: "issue.updated", Status: tc.status, Title: "the child", Parent: statusRoot, Rank: "B",
				ResolvedRepo: "acme/widgets"}
			if tc.byArchitect {
				stop.ActorSession = architect.session
			}
			if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "stop", stop, handlers...); err != nil {
				t.Fatalf("apply the %s of the child: %v", tc.status, err)
			}
			shown := statuses.issues[statusChild]
			shown.Status, shown.LastSeq = tc.status, 5
			statuses.issues[statusChild] = shown
			out := recordedChild(t, h, statusChild)
			if out == nil || record.InLegionsWork(*out) || out.Phase != phase.Planning || out.Status != tc.status || out.Generation != 1 {
				t.Fatalf("the child after the %s = %#v, want it out of Legion's work, its phase planning and its run kept", tc.status, out)
			}

			recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", RerunChildRequest{GrantID: architect.grant(t), Issue: statusChild}, nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("rerun = %d %s, want 200", recorder.Code, recorder.Body)
			}
			if len(statuses.writes) != 0 {
				t.Fatalf("Dispatch writes = %#v, want none in the request: the todo is queued", statuses.writes)
			}
			if row := queuedWrite(t, h, recorder, statusChild, "todo"); row.ObservedStatus != tc.status || row.Generation != 2 || row.HasOption() {
				t.Fatalf("the queued todo = %+v, want it observing %s, fenced on the new run 2, with no option", row, tc.status)
			}
			rerun := recordedChild(t, h, statusChild)
			if rerun == nil || rerun.Generation != 2 || !record.InLegionsWork(*rerun) || (rerun.Phase != phase.Admitted && rerun.Phase != phase.Planning) {
				t.Fatalf("the child once rerun_child answered = %#v, want a new run, generation 2, in Legion's work from its first phase", rerun)
			}

			echo := statusEcho(6, "todo")
			if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "echo", echo, handlers...); err != nil {
				t.Fatalf("apply the echo of the todo: %v", err)
			}
			if after := recordedChild(t, h, statusChild); after == nil || after.Generation != 2 || after.Phase != rerun.Phase {
				t.Fatalf("the child after the todo's echo = %#v, want the run the rerun opened, generation 2 at %s", after, rerun.Phase)
			}
		})
	}
}

// The controller's set_status of todo on a root in Legion's work whose architect claim failed
// starts that claim again in its session, as `legion claims retry` does, and writes no status (R20
// of dispatch://INFRA-3320: a failed role is not restarted through a status): its answer names no
// write, no status row is queued for the root, and nothing of the child under it moves.
func TestTodoOnARootWithAFailedArchitectRestartsThatClaim(t *testing.T) {
	h, _, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	failed := h.stored(architect.token)
	failed.State = supervise.StateFailed
	if err := h.store.PutClaim(context.Background(), failed); err != nil {
		t.Fatalf("fail the root's architect claim: %v", err)
	}
	child, childRows := recordedChild(t, h, statusChild), outboxRows(t, h, statusChild)
	starts := func() int {
		var count int
		if err := h.store.Pool().QueryRow(context.Background(),
			`select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'architect'`,
			statusRoot).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := starts()

	grant := controllerGrant(t, h)
	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: statusRoot, Status: "todo"}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("set_status = %d %s, want 200", recorder.Code, recorder.Body)
	}
	var answer StatusCallResponse
	decodeInto(t, recorder, &answer)
	if len(answer.Queued) != 0 || len(statusRows(t, h, statusRoot)) != 0 || len(statuses.writes) != 0 {
		t.Fatalf("the call queued %+v, the outbox holds %v and Dispatch took %v, want no status written", answer.Queued, statusRows(t, h, statusRoot), statuses.writes)
	}
	if got := starts(); got != before+1 {
		t.Fatalf("architect start rows of %s = %d, want one more than the %d before: the failed claim starts again", statusRoot, got, before)
	}
	if after, rows := recordedChild(t, h, statusChild), outboxRows(t, h, statusChild); !reflect.DeepEqual(after, child) || !reflect.DeepEqual(rows, childRows) {
		t.Fatalf("the child after the root's todo = %#v with rows %v, want it as it was, %#v with %v", after, rows, child, childRows)
	}
}

// The controller's set_status moves the record within its own request: the controller parking a
// running child stops it at once, its phase kept (R20 of dispatch://INFRA-3320), and its backlog
// is queued as a status row the controller is told of should Dispatch refuse it (decision 12),
// not written in the request. The echo moves the record's view of Dispatch alone.
func TestControllerStatusParksTheRecordWithoutWaitingForItsDispatchEvent(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: statusChild, Status: "backlog"}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("set_status = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none in the request", statuses.writes)
	}
	if row := queuedWrite(t, h, recorder, statusChild, "backlog"); !row.Controller || row.ObservedStatus != "in_progress" {
		t.Fatalf("the queued backlog = %+v, want it the controller's, observing in_progress", row)
	}
	parked := recordedChild(t, h, statusChild)
	if parked == nil || record.InLegionsWork(*parked) || parked.Phase != phase.Planning || parked.Status != "backlog" || parked.DispatchStatus != "in_progress" {
		t.Fatalf("the child's record once set_status answered = %#v, want it parked, its phase planning kept, Dispatch still shown in_progress", parked)
	}
	if got := superviseRows(t, h, "suspend", claim.RolePlanner); got != 1 {
		t.Fatalf("planner suspend rows = %d, want the move to stop the planner once", got)
	}

	rows := outboxRows(t, h, statusChild)
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "echo", statusEcho(5, "backlog"), handlers...); err != nil {
		t.Fatalf("apply the echo of the backlog: %v", err)
	}
	echoed := recordedChild(t, h, statusChild)
	if echoed == nil || echoed.Status != "backlog" || echoed.Phase != phase.Planning || echoed.DispatchStatus != "backlog" || echoed.Generation != 1 {
		t.Fatalf("the child after the echo = %#v, want it parked as the call left it, Dispatch now shown backlog", echoed)
	}
	if after := outboxRows(t, h, statusChild); !reflect.DeepEqual(after, rows) {
		t.Fatalf("the child's outbox rows after the echo = %v, want the %v the call left", after, rows)
	}
}

// The controller's set_status naming an issue of another Dispatch project, whose events the
// consumer drops, is refused before anything is read or queued: 422 ISSUE_OF_ANOTHER_PROJECT,
// whose sentence names that project, and no outbox row keyed by the issue, since only that
// project's daemon writes its status. And should Dispatch's answer to a write of a foreign issue
// reach the daemon all the same, the outbox applies it as the daemon's own write of an issue it
// does not record (ApplyOwnStatusWrite), which records nothing, so the daemon never admits a
// foreign issue.
func TestControllerStatusOnAnotherProjectsIssueRecordsNothing(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	const foreign = "OTHER-12"
	statuses.issues[foreign] = dispatch.Issue{Key: foreign, ResolvedRepo: "acme/widgets", Title: foreign, Status: "backlog", Labels: []string{"legion"}, HandedOver: true, LastSeq: 7}
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: foreign, Status: "todo"}, nil)
	if text := refused(t, recorder, http.StatusUnprocessableEntity, "ISSUE_OF_ANOTHER_PROJECT"); !strings.Contains(text, "project OTHER") {
		t.Fatalf("the refusal of set_status on %s says %q, want it to name project OTHER", foreign, text)
	}
	if rows := outboxRows(t, h, foreign); len(rows) != 0 || len(statuses.writes) != 0 {
		t.Fatalf("the refused set_status queued %v on %s and wrote %v to Dispatch, want nothing", rows, foreign, statuses.writes)
	}
	written := statuses.issues[foreign]
	written.Status, written.LastSeq = "todo", 8
	own := OwnStatusWrite{DispatchProject: testDispatchProject, Apply: func(ctx context.Context, eventID string, fact intake.Fact) (intake.Result, error) {
		return intake.ApplyFact(ctx, h.store.Pool(), "outbox", eventID, fact, handlers...)
	}}
	if failure := ApplyOwnStatusWrite(context.Background(), own, "foreign-write", written); failure != nil {
		t.Fatalf("apply the foreign write's answer: %v", failure)
	}
	if got := recordedChild(t, h, foreign); got != nil {
		t.Fatalf("the record of %s = %#v, want none: it is another Dispatch project's", foreign, got)
	}
}

// close_root ends only a tree root. Its refusal of a child tells the architect how a child leaves
// the workflow instead: by the status write of the architect that owns it, in Dispatch
// (record.EndChildRule), and never by an operation the daemon no longer serves (dispatch://INFRA-3178
// R6). Each check that refuses it says so: the route's check of the issue the request names
// (rootArchitectOf), and the workflow's check of the close fact on the Dispatch chain it commits on
// (workflow's closeRoot, architectCallRefusal), which the copy of Dispatch's issues holds here
// (statusHarness; R18 of dispatch://INFRA-3320). Each row holds only that much of the wording.
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
