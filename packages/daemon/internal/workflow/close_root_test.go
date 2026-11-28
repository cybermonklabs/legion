package workflow

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root architect whose human decided no change at the design gate ends its root while the root
// is admitted: the daemon queues done to Dispatch with the architect's reason, which the write
// posts first, and the root keeps its phase and its status until Dispatch shows it done (R24 of
// dispatch://INFRA-3320: a close Dispatch may refuse changes nothing). That done, the echo of the
// write, ends the run: the root moves to phase done and leaves Legion's work as any close does
// (leave), its claims close, and no deadline is armed for anything. The journal follows the close.
func TestAnArchitectClosesItsAdmittedRootBeforeItsFirstPhase(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	var logged bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil)))
	apply := func(id string, fact intake.Fact) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
		return result
	}
	rootIs := func(wantStatus string, wantPhase phase.Phase) {
		t.Helper()
		if root := storedIssue(t, pool, "LEGION-1"); root.Status != wantStatus || root.Phase != wantPhase {
			t.Fatalf("root status=%s phase=%s; want %s, %s", root.Status, root.Phase, wantStatus, wantPhase)
		}
	}

	if result := apply("close", intake.CloseRoot{Issue: "LEGION-1", Reason: "the human decided no change is needed"}); result.Refusal != nil {
		t.Fatalf("close refused: %+v", result.Refusal)
	}
	rootIs("in_progress", phase.Admitted)
	if got, want := statusWrites(t, pool, "LEGION-1"), []record.StatusWrite{record.CloseStatusWrite("done", "in_progress", "Closed by its architect before its first phase: the human decided no change is needed", 2)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("status writes %+v; want the close of generation 2, done over in_progress with the architect's reason, %+v", got, want)
	}
	assertOutboxCount(t, pool, "issue_suspend", 0)

	apply("done-event", intake.DispatchIssue{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done", Title: "Root", Rank: "U"})
	rootIs("done", phase.Done)
	assertOutboxCount(t, pool, "issue_suspend", 1)
	want := []string{
		`msg="workflow: the daemon's close of an issue is queued" issue=LEGION-1 kind=close_root generation=2 entry=1`,
		`msg="workflow: phase changed" issue=LEGION-1 tree=LEGION-1 from=admitted to=done trigger=close_root status=done`,
		`msg="workflow: issue left Legion's work" issue=LEGION-1 status=done phase=done`,
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		got = append(got, line[strings.Index(line, "msg="):])
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A root waiting on a person has started no phase either — it reaches awaiting_person only from
// admitted — so the answer "close this" ends it the same way: the reason posted, done written over
// the status the record kept, and the root leaves its wait once Dispatch shows it done. Before,
// close_root refused it as a tree past its first phase, and the only way out was a person's own
// status write (dispatch://INFRA-545).
func TestAnArchitectClosesItsRootThatWaitsOnAPerson(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	since := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.AwaitingPerson, Generation: 1, Status: "in_progress",
		Rank: "U", Awaiting: &record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 3, At: since}}})
	engine := testEngine(config.DesignGateRootIssues, nil)

	result, err := intake.ApplyFact(context.Background(), pool, "api", "close",
		intake.CloseRoot{Issue: "LEGION-1", Reason: "the person answered: close this"}, engine, admissionStub{})
	if err != nil || result.Refusal != nil {
		t.Fatalf("close = %+v, %v; want it accepted for a root waiting on a person", result.Refusal, err)
	}
	if root := storedIssue(t, pool, "LEGION-1"); root.Phase != phase.AwaitingPerson || root.Status != "in_progress" || root.Awaiting == nil {
		t.Fatalf("the root before Dispatch shows done = phase %s, status %s, awaiting %+v; want its wait kept", root.Phase, root.Status, root.Awaiting)
	}
	if got, want := statusWrites(t, pool, "LEGION-1"), []record.StatusWrite{record.CloseStatusWrite("done", "in_progress", "Closed by its architect before its first phase: the person answered: close this", 1)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("status writes %+v; want the close of generation 1, done over in_progress with the architect's reason, %+v", got, want)
	}

	landClose(t, pool, engine, "LEGION-1")
	if root := storedIssue(t, pool, "LEGION-1"); root.Phase != phase.Done || root.Status != "done" || root.Awaiting != nil {
		t.Fatalf("the root once Dispatch shows done = phase %s, status %s, awaiting %+v; want done with no wait", root.Phase, root.Status, root.Awaiting)
	}
}

// close_root ends a root only while it has started no phase, and only a root: past that the root
// ends through its workflow or a human, and a child leaves by the status its owning architect
// writes on it in Dispatch.
func TestCloseRootIsRefusedOnceATreeHasStartedOrForAChild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		issue record.Issue
		code  string
	}{
		{"a root in planning", record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Phase: phase.Planning, Status: "in_progress"}, "CLOSE_AFTER_PHASE_STARTED"},
		{"a root moved to backlog", record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Phase: phase.Admitted, Status: "backlog"}, "CLOSE_AFTER_PHASE_STARTED"},
		{"a child", record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Parent: new("LEGION-1"), Phase: phase.Admitted, Status: "in_progress"}, "ROOT_REQUIRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			if tc.issue.Key != "LEGION-1" {
				seedIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
			}
			tc.issue.Project, tc.issue.Title, tc.issue.Generation, tc.issue.Rank = "LEGION", tc.issue.Key, 1, "V"
			seedIssue(t, pool, tc.issue)
			result, err := intake.ApplyFact(context.Background(), pool, "api", "close", intake.CloseRoot{Issue: tc.issue.Key, Reason: "moot"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil || result.Refusal == nil || result.Refusal.Code != tc.code {
				t.Fatalf("close = %+v, %v; want refused %s", result.Refusal, err, tc.code)
			}
			assertOutboxCount(t, pool, "dispatch_status", 0)
		})
	}
}

// Every phase change is one journal line once its fact commits: the issue, its tree, the phases it
// moved between, the trigger, and the status it now holds. Here the design gate is off, so the
// architect's registration opens it and the admitted root moves to planning.
func TestAPhaseChangeIsOneJournalLine(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
	var logged bytes.Buffer
	engine := testEngine(config.DesignGateOff, slog.New(slog.NewTextHandler(&logged, nil)))
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "gate", intake.GateRegistered{Issue: "LEGION-1", ArtifactID: "art-1", Version: 1}, engine, admissionStub{}); err != nil {
		t.Fatalf("register the gate: %v", err)
	}
	// The gate's opening releases the root; the waiting line starts its first phase.
	startUnblockedChildren(t, pool, engine)
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.Contains(line, `msg="workflow: phase changed"`) {
			got = append(got, line[strings.Index(line, "msg="):])
		}
	}
	if want := `msg="workflow: phase changed" issue=LEGION-1 tree=LEGION-1 from=admitted to=planning trigger=gate_opened status=in_progress`; fmt.Sprint(got) != "["+want+"]" {
		t.Fatalf("phase lines %q, want %q", got, want)
	}
}
