package workflow

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A tree's architect is who every notice of its tree reaches, so its own claim failing — its
// launches or prompts ran out — would reach nobody unless the daemon says so: the failure is a
// worker-died notice naming the architect and the phase the root is in, written for the architect
// that owns the issue and for the controller. Nothing is held: a phase is its worker's, and the root's planner here
// keeps its phase.
func TestATreeArchitectsFailedClaimIsNoticedAndHoldsNoPhase(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})

	if _, err := intake.ApplyFact(ctx, pool, "supervise", "architect-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleArchitect}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the architect's failed claim: %v", err)
	}
	var gotPhase phase.Phase
	var heldFrom *string
	if err := pool.QueryRow(ctx, "select phase, held_from from issues where key = $1", "LEGION-208").Scan(&gotPhase, &heldFrom); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if gotPhase != phase.Planning || heldFrom != nil {
		t.Fatalf("root phase = %s held from %v, want planning and not held", gotPhase, heldFrom)
	}
	died := record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Planning}
	if got, want := noticeRows(t, pool), []record.OutboxPayload{died, record.ControllerNotice(died)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("notice rows = %+v, want %+v, to the issue and then to the controller", got, want)
	}
}

// A role that finished its phase stays live until its issue closes, so its claim can fail while
// another role works the phase, or while the phase is held for another role's failure. The
// architect is told in a worker-died naming the role and saying the phase is not its, and the
// issue's phase stands: nothing is held for a role that holds no phase, and the controller is not
// told of a phase that is not stalled. A lingering tree and a child that left the workflow tell
// nobody, since their claims were suspended. A child's sub-architect works no phase and nothing in
// the workflow starts it, so its failure holds nothing and tells nobody: the child's notices go to
// the architect above it, and the operator who started it relaunches it.
func TestAFinishedRolesFailedClaimIsNoticedAndHoldsNothing(t *testing.T) {
	root := "LEGION-208"
	until := time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		root      record.Issue
		child     record.Issue
		role      claim.Role
		wantPhase phase.Phase
		want      []record.OutboxPayload
	}{
		{
			name:  "the planner while the implementer works",
			root:  record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"},
			child: record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"},
			role:  claim.RolePlanner, wantPhase: phase.Implementing,
			want: []record.OutboxPayload{record.Notice{Kind: "worker-died", Role: claim.RolePlanner, Phase: phase.Implementing,
				Reason: "the planner does not work LEGION-209's phase implementing; nothing is held"}},
		},
		{
			name:  "the merger while the issue awaits its merge",
			root:  record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"},
			child: record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "V"},
			role:  claim.RoleMerger, wantPhase: phase.AwaitingMerge,
			want: []record.OutboxPayload{record.Notice{Kind: "worker-died", Role: claim.RoleMerger, Phase: phase.AwaitingMerge,
				Reason: "the merger does not work LEGION-209's phase awaiting_merge; nothing is held"}},
		},
		{
			name: "the tester while the implementer's phase is held",
			root: record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"},
			child: record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing},
				Generation: 1, Status: "in_progress", Rank: "V"},
			role: claim.RoleTester, wantPhase: phase.Held,
			want: []record.OutboxPayload{record.Notice{Kind: "worker-died", Role: claim.RoleTester, Phase: phase.Held,
				Reason: "the tester does not work LEGION-209's phase implementing; nothing is held"}},
		},
		{
			name:  "the planner of a lingering tree",
			root:  record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until},
			child: record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"},
			role:  claim.RolePlanner, wantPhase: phase.Implementing,
		},
		{
			name:  "the tester of a child that left the workflow",
			root:  record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"},
			child: record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Done, Generation: 1, Status: "backlog", Rank: "V"},
			role:  claim.RoleTester, wantPhase: phase.Done,
		},
		{
			name:  "a child's sub-architect while the child's implementer works",
			root:  record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"},
			child: record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"},
			role:  claim.RoleArchitect, wantPhase: phase.Implementing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedIssue(t, pool, tc.root)
			seedIssue(t, pool, tc.child)

			if _, err := intake.ApplyFact(ctx, pool, "supervise", "finished-role-failed", intake.ClaimFailed{Issue: "LEGION-209", Role: tc.role}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact the %s's failed claim: %v", tc.role, err)
			}
			var gotPhase phase.Phase
			var heldFrom *string
			if err := pool.QueryRow(ctx, "select phase, held_from from issues where key = $1", "LEGION-209").Scan(&gotPhase, &heldFrom); err != nil {
				t.Fatalf("read the child: %v", err)
			}
			if wantHeld := tc.child.Hold != nil; gotPhase != tc.wantPhase || (heldFrom != nil) != wantHeld {
				t.Fatalf("after the %s's failure the child is in %s held from %v, want %s held %t", tc.role, gotPhase, heldFrom, tc.wantPhase, wantHeld)
			}
			if got := noticeRows(t, pool); !sameNotices(got, tc.want) {
				t.Fatalf("notice rows = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A phase worker's claim failing holds the issue, and the two notices that say so — the hold the
// architect and the controller read, and the worker-died beside it — are all an agent has to
// decide between retrying the phase and escalating it. So each names the failure the fact
// carries: the one word for what failed the claim, the sentence written beside that word, and
// each of the four counts the failure left against the limit it ran out at, deaths against the
// launch failure limit, which is the limit the supervisor measures them by (record.NoticeFailure).
// The hold carries no `reason`: that field keeps its one value, `escalated`, which both agents'
// texts read as the architect having already escalated this hold.
//
// The other three shapes of worker-died are here beside it, because the evidence each carries is
// a decision of its own: the worker-died of a role that does not work the issue's phase names the
// failure too, since that role's architect decides whether to start it again on the same
// evidence; the tree architect's own names none, as R5 has it, and neither does the one that ends
// a closed issue's production-check look, whose reason already names the cause.
func TestTheHeldNoticeNamesTheFailure(t *testing.T) {
	const sentence = "the planner's process died with its task outstanding, and its relaunches ran out"
	failed := func(issue string, role claim.Role) intake.ClaimFailed {
		return intake.ClaimFailed{Issue: issue, Role: role, Reason: sentence, Cause: supervise.CauseDied,
			Budgets: supervise.Budgets{LaunchFailures: 3, Deaths: 2, PromptFailures: 1, PromptRetires: 2},
			Limits:  supervise.Limits{LaunchFailures: 3, PromptFailures: 5, PromptRetires: 2}}
	}
	// Every count against its own limit, and no two pairs alike, so a notice that paired a count
	// with another's limit reads differently here.
	named := &record.NoticeFailure{Cause: string(supervise.CauseDied), Detail: sentence,
		LaunchFailures: record.FailureBudget{Count: 3, Limit: 3},
		Deaths:         record.FailureBudget{Count: 2, Limit: 3},
		PromptFailures: record.FailureBudget{Count: 1, Limit: 5},
		PromptRetires:  record.FailureBudget{Count: 2, Limit: 2}}
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"}

	t.Run("the hold, its controller copy and the worker-died", func(t *testing.T) {
		pool := migratedPool(t)
		ctx := context.Background()
		seedIssue(t, pool, root)

		if _, err := intake.ApplyFact(ctx, pool, "supervise", "planner-failed", failed(root.Key, claim.RolePlanner), testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("ApplyFact the planner's failed claim: %v", err)
		}
		var gotPhase phase.Phase
		var heldFrom *string
		if err := pool.QueryRow(ctx, "select phase, held_from from issues where key = $1", root.Key).Scan(&gotPhase, &heldFrom); err != nil {
			t.Fatalf("read the root: %v", err)
		}
		if gotPhase != phase.Held || heldFrom == nil || *heldFrom != string(phase.Planning) {
			t.Fatalf("root phase = %s held from %v, want held from planning", gotPhase, heldFrom)
		}
		held := record.Notice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning, Failure: named}
		died := record.Notice{Kind: "worker-died", Role: claim.RolePlanner, Phase: phase.Planning, Failure: named}
		got := noticeRows(t, pool)
		if want := []record.OutboxPayload{held, record.ControllerNotice(held), died}; !sameNotices(got, want) {
			t.Fatalf("notice rows = %+v, want %+v: the hold, its controller copy and the worker-died, each naming the failure", got, want)
		}
		// Decoded from the stored rows, so each field above crossed the wire; the hold's `reason`
		// is read off the row's own keys, since a notice that carried one as "" decodes the same.
		for _, payload := range got {
			if _, err := record.NewOutboxRow(root.Key, payload, time.Now()); err != nil {
				t.Fatalf("the %T the failure wrote is no valid outbox payload: %v", payload, err)
			}
		}
		var keys []string
		if err := pool.QueryRow(ctx, `select array(select jsonb_object_keys(payload))
			from outbox where kind = $1 and payload->>'kind' = 'held'`, string(record.OutboxKindNotice)).Scan(&keys); err != nil {
			t.Fatalf("read the held notice's keys: %v", err)
		}
		if slices.Contains(keys, "reason") || !slices.Contains(keys, "failure") {
			t.Fatalf("the held notice's keys = %v, want the failure and no reason: the reason is the escalation's word alone", keys)
		}
	})

	t.Run("a tree architect's claim names none", func(t *testing.T) {
		pool := migratedPool(t)
		seedIssue(t, pool, root)

		if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "architect-failed", failed(root.Key, claim.RoleArchitect), testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("ApplyFact the architect's failed claim: %v", err)
		}
		died := record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Planning}
		if got, want := noticeRows(t, pool), []record.OutboxPayload{died, record.ControllerNotice(died)}; !sameNotices(got, want) {
			t.Fatalf("notice rows = %+v, want %+v, naming no failure", got, want)
		}
	})

	t.Run("a role that does not work the phase names it beside its own reason", func(t *testing.T) {
		pool := migratedPool(t)
		working := root
		working.Phase = phase.Implementing
		child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root.Key, Parent: &root.Key, Project: "LEGION", Title: "child", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"}
		seedIssue(t, pool, working)
		seedIssue(t, pool, child)

		if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "planner-failed", failed(child.Key, claim.RolePlanner), testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("ApplyFact the finished planner's failed claim: %v", err)
		}
		died := record.Notice{Kind: "worker-died", Role: claim.RolePlanner, Phase: phase.Implementing, Failure: named,
			Reason: "the planner does not work LEGION-209's phase implementing; nothing is held"}
		if got, want := noticeRows(t, pool), []record.OutboxPayload{died}; !sameNotices(got, want) {
			t.Fatalf("notice rows = %+v, want %+v: the sentence it had, and the failure beside it", got, want)
		}
	})

	t.Run("a production check look names none", func(t *testing.T) {
		rig := childCheckRig(t)
		rig.apply("tester-failed", failed("LEGION-209", claim.RoleTester))
		var died []record.Notice
		for _, payload := range noticeRows(t, rig.pool) {
			if notice, ok := payload.(record.Notice); ok && notice.Kind == "worker-died" {
				died = append(died, notice)
			}
		}
		want := record.Notice{Kind: "worker-died", Role: claim.RoleTester,
			Reason: "production check look 1 lost its tester: " + sentence}
		if len(died) != 1 || !reflect.DeepEqual(died[0], want) {
			t.Fatalf("worker-died notices of the ended look = %+v, want %+v once: its reason names the failure already", died, want)
		}
	})
}

// An escalation is recorded on the held issue as well as noticed to the architect that owns the
// issue and to the controller, so a controller that starts after it finds it in the state it reads at boot
// (issues.<KEY>.holdReason); the retry that ends the hold clears it.
func TestAnEscalationIsRecordedOnTheHoldForAControllerThatStartsLater(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Planning}, Generation: 1, Status: "in_progress", Rank: "U"})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "escalate", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.EscalateDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-208"); got != phase.Held || reason != "escalated" {
		t.Fatalf("after the escalation the state reads phase %s hold reason %q, want held and escalated", got, reason)
	}
	escalated := record.Notice{Kind: "held", Phase: phase.Planning, Reason: "escalated"}
	if got, want := noticeRows(t, pool), []record.OutboxPayload{escalated, record.ControllerNotice(escalated)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("notice rows = %+v, want %+v, to the issue and then to the controller", got, want)
	}
	if _, err := intake.ApplyFact(ctx, pool, "architect", "retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-208"); got != phase.Planning || reason != "" {
		t.Fatalf("after the retry the state reads phase %s hold reason %q, want planning and none", got, reason)
	}
}

// An escalation carries the architect's own words where its retry_or_escalate named any: the
// controller must act on the hold, and why the architect escalated it is what it acts on. They
// ride a field of their own, since `reason` keeps its one value `escalated`, which both agents'
// texts read as the hold having been escalated at all. An escalation that named no reason
// carries no such key.
func TestAnEscalationCarriesTheArchitectsReason(t *testing.T) {
	const said = "the tester has failed twice on the same flake; a person should look at the runner"
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{name: "the architect names a reason", reason: said},
		{name: "the architect names none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Testing}, Generation: 1, Status: "testing", Rank: "U"})
			if _, err := intake.ApplyFact(context.Background(), pool, "architect", "escalate",
				intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.EscalateDecision, Reason: tc.reason},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("escalate: %v", err)
			}
			escalated := record.Notice{Kind: "held", Phase: phase.Testing, Reason: "escalated", Escalation: tc.reason}
			if got, want := noticeRows(t, pool), []record.OutboxPayload{escalated, record.ControllerNotice(escalated)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("notice rows = %+v, want %+v: the architect's words beside the escalation, to the issue and to the controller", got, want)
			}
			// Read off the stored row's own keys, since a notice carrying "" decodes the same.
			var keys []string
			if err := pool.QueryRow(t.Context(), `select array(select jsonb_object_keys(payload))
				from outbox where kind = $1 and payload->>'kind' = 'held'`, string(record.OutboxKindNotice)).Scan(&keys); err != nil {
				t.Fatalf("read the held notice's keys: %v", err)
			}
			if slices.Contains(keys, "escalation") != (tc.reason != "") {
				t.Fatalf("the held notice's keys = %v, want the escalation key only where the architect named a reason", keys)
			}
		})
	}
}

// A held root moved out of the workflow ends its hold, and an escalation with it: its tree lingers
// with the root's phase done and no hold reason, so the state never shows a closed root escalated.
func TestAClosedRootEndsItsHoldAndItsEscalation(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Planning}, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "escalate", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.EscalateDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "root-backlog", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("move the root to backlog: %v", err)
	}
	var held bool
	if err := pool.QueryRow(ctx, "select held_from is not null from issues where key = $1", "LEGION-208").Scan(&held); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-208"); got != phase.Done || reason != "" || held {
		t.Fatalf("after the backlog move the state reads phase %s hold reason %q, held from a phase %t; want done, none, and not held", got, reason, held)
	}
	assertOutboxCount(t, pool, "linger_close", 1)
}

// A held child keeps its hold when its tree closes, escalation included, since the tree's
// re-admission leaves it held and the escalation still unanswered. While the tree lingers or is
// closed, the escalation waits on nobody, so the state the controller reads at every start shows
// the child held with no hold reason.
func TestAnEscalatedChildOfAClosedTreeKeepsItsHoldButShowsNoEscalation(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	root, from := "LEGION-208", phase.Implementing
	seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Held, Hold: &record.Hold{From: from}, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "escalate", intake.RetryOrEscalate{Issue: "LEGION-209", Decision: intake.EscalateDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("escalate the child: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-209"); got != phase.Held || reason != "escalated" {
		t.Fatalf("in a running tree the state reads the child's phase %s hold reason %q, want held and escalated", got, reason)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "root-backlog", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("move the root to backlog: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-209"); got != phase.Held || reason != "" {
		t.Fatalf("with its tree closed the state reads the child's phase %s hold reason %q, want held and none", got, reason)
	}
	var heldFrom, holdReason string
	if err := pool.QueryRow(ctx, "select held_from, hold_reason from issues where key = $1", "LEGION-209").Scan(&heldFrom, &holdReason); err != nil {
		t.Fatalf("read the child's hold: %v", err)
	}
	if heldFrom != string(from) || holdReason != "escalated" {
		t.Fatalf("the child's record holds it from %q for %q, want from %s for escalated", heldFrom, holdReason, from)
	}
}

// A lingering tree holds its members where they stood (TestNoFactMovesAMemberOfALingeringTree), so a
// phase worker's failed claim there holds nothing and tells neither the architect nor the
// controller. The tree architect's own failed claim holds no phase either, and is still told to the
// issue's topic and to the controller, lingering or not: nobody inside the tree can act on it.
func TestALingeringTreesFailedClaimHoldsNothingAndItsArchitectsIsStillTold(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	root, until := "LEGION-208", time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "V"})

	if _, err := intake.ApplyFact(ctx, pool, "supervise", "tester-failed", intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleTester}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the tester's failed claim: %v", err)
	}
	var gotPhase phase.Phase
	var held bool
	if err := pool.QueryRow(ctx, "select phase, held_from is not null from issues where key = $1", "LEGION-209").Scan(&gotPhase, &held); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if gotPhase != phase.Testing || held {
		t.Fatalf("the lingering tree's child is in %s, held %t; want testing and not held", gotPhase, held)
	}
	if got := noticeRows(t, pool); len(got) != 0 {
		t.Fatalf("notice rows after the child's failed claim = %+v, want none", got)
	}

	if _, err := intake.ApplyFact(ctx, pool, "supervise", "architect-failed", intake.ClaimFailed{Issue: root, Role: claim.RoleArchitect}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the architect's failed claim: %v", err)
	}
	died := record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Done}
	if got, want := noticeRows(t, pool), []record.OutboxPayload{died, record.ControllerNotice(died)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("notice rows = %+v, want %+v, to the issue and then to the controller", got, want)
	}
}

// A held member of a lingering tree keeps its hold, escalation included, when its architect retries
// it: the retry would start the held phase's worker inside a tree that has left the workflow, so it
// changes nothing, and re-admission finds the member held from the same phase for the same reason.
func TestARetryInALingeringTreeKeepsTheHoldAndItsReason(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	root, until := "LEGION-208", time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Held,
		Hold: &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}, Generation: 1, Status: "in_progress", Rank: "V"})

	if _, err := intake.ApplyFact(ctx, pool, "architect", "retry", intake.RetryOrEscalate{Issue: "LEGION-209", Decision: intake.RetryDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var gotPhase phase.Phase
	var heldFrom, holdReason *string
	var starts int
	if err := pool.QueryRow(ctx, `select phase, held_from, hold_reason,
		(select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start' and coalesce(payload->>'task', '') <> '')
		from issues where key = $1`, "LEGION-209").Scan(&gotPhase, &heldFrom, &holdReason, &starts); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if gotPhase != phase.Held || heldFrom == nil || *heldFrom != string(phase.Implementing) || holdReason == nil || *holdReason != string(record.HoldEscalated) || starts != 0 {
		t.Fatalf("after the retry the child is in %s held from %v for %v with %d starts; want held from implementing, escalated, and no start", gotPhase, heldFrom, holdReason, starts)
	}
}

// projectedHold is the issue's phase and hold reason as the state view shows them.
func projectedHold(t *testing.T, pool *pgxpool.Pool, key string) (phase.Phase, string) {
	t.Helper()
	var state api.State
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		state, err = projection.Project(t.Context(), tx, record.NewStore(), "LEGION", nil, nil, nil, nil, nil, time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatalf("project the state: %v", err)
	}
	return state.Issues[key].Phase, state.Issues[key].HoldReason
}

// sameNotices compares two notice-row lists as slices.Equal would — an empty list equals a nil
// want — through reflect.DeepEqual, since record.Notice holds a slice and is not comparable.
func sameNotices(got, want []record.OutboxPayload) bool {
	return len(got) == 0 && len(want) == 0 || reflect.DeepEqual(got, want)
}

// noticeRows is every notice and controller notice row's payload, oldest first.
func noticeRows(t *testing.T, pool *pgxpool.Pool) []record.OutboxPayload {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select id, kind, issue, payload from outbox where kind in ($1, $2) order by id",
		string(record.OutboxKindNotice), string(record.OutboxKindControllerNotice))
	if err != nil {
		t.Fatalf("read the notice rows: %v", err)
	}
	defer rows.Close()
	payloads := []record.OutboxPayload{}
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a notice row: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the notice rows: %v", err)
	}
	return payloads
}

// A retry's task names the issue's pull request, as a backward move's does, so the restarted
// worker is told the pull request it works on.
func TestARetrysTaskNamesThePullRequest(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Generation: 1, Status: "in_progress", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	task := startTask(t, pool, RoleFor(phase.Implementing))
	if !strings.Contains(task, "Pull request: #42.") {
		t.Fatalf("task = %q, want it to name pull request #42", task)
	}
}

// A todo written on a held root whose tree does not linger releases the hold, which is the
// effect of its architect's retry_or_escalate with retry (R7): the issue returns to the phase
// the hold came from, the hold is cleared, and that phase's role is started with its task,
// carrying the daemon's own reason, which names the status write that made the release — no
// caller of the status route names one. The release writes the Dispatch status of the phase the
// issue ends in, as a phase transition writes it, so the board shows the phase the issue works
// and a later todo on the same issue is a status change again. Nothing else moves: the
// generation, the rounds, the conflict rounds, the backward moves, the cap count and the
// admission slot all stand as they were.
//
// A person's move in the Dispatch dashboard and the daemon's own write — which the controller's
// set_status and an operator's `legion status` both reach the record as — are the same event
// here, so each is a row. Each row then fails the worker again, which holds the issue a second
// time, and writes todo again: the second release runs as the first did, because the first
// recorded the event it took with the recorded status kept rather than leaving the record to
// say the issue is todo.
//
// The last two rows are releases whose own path moves the issue — a hold from testing whose
// head is red, and a hold from reviewing whose round is decided — where the phase the issue
// ends in is not the phase the hold came from. The status written is that phase's either way,
// which is the whole of R7: the move's own write reaches Dispatch at a status it does not show,
// or is never queued at all, and the release's is the write the board stands on.
func TestATodoOnAHeldRootReleasesTheHold(t *testing.T) {
	const root = "LEGION-208"
	const reason = "Reason: the issue was set to todo while its phase was held."
	for _, tc := range []struct {
		name string
		own  bool
	}{
		{name: "a person's move in the Dispatch dashboard"},
		{name: "the daemon's own write", own: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			var logged bytes.Buffer
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil))))
			seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION",
				Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Generation: 3, Status: "in_progress", DispatchStatus: "in_progress",
				Rank: "U", HandedOver: true, LastDispatchSeq: 4})
			seedSlot(t, pool, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
			counted := record.PhaseRow{Issue: root, Role: claim.RoleImplementer, Rounds: 3, ConflictRounds: 1,
				BackwardMoves: map[string]int{"testing-implementing": 2}, CapTold: 3}
			seedPhase(t, pool, counted)
			todo := func(seq int64, own bool) intake.DispatchIssue {
				return intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: "todo",
					Title: "root", Rank: "U", HandedOver: true, DaemonActor: own, StatusMove: own}
			}

			apply("todo", todo(5, tc.own))

			released := storedIssue(t, pool, root)
			if released.Phase != phase.Implementing || released.Hold != nil || released.Generation != 3 {
				t.Fatalf("the root after the todo = %#v, want it back in implementing, its hold cleared and its generation 3", released)
			}
			// The record keeps the status of the phase it works while Dispatch shows the todo,
			// which is what the queued write sets back.
			if released.Status != "in_progress" || released.DispatchStatus != "todo" || released.LastDispatchSeq != 5 {
				t.Fatalf("the root after the todo = %#v, want in_progress kept, Dispatch shown at todo and the event's sequence 5 recorded", released)
			}
			want := record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}
			if got := statusWrites(t, pool, root); len(got) != 1 || got[0] != want {
				t.Fatalf("status writes = %+v, want the one %+v: the status of the phase the issue ends in", got, want)
			}
			starts := superviseRowsOf(t, pool, root, "start")
			if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Phase != phase.Implementing || !strings.Contains(starts[0].Task, reason) {
				t.Fatalf("start rows = %+v, want one implementer start of implementing whose task carries %q", starts, reason)
			}
			if got := phaseCounts(t, pool, root, claim.RoleImplementer); !reflect.DeepEqual(got, counted) {
				t.Fatalf("the implementer's counts after the release = %+v, want the %+v the hold left", got, counted)
			}
			if got := slotCount(t, pool, root); got != 1 {
				t.Fatalf("%s holds %d slots, want the one it held while it was held", root, got)
			}
			if line := logged.String(); !strings.Contains(line, "workflow: a held phase is released") ||
				!strings.Contains(line, "the issue was set to todo while its phase was held") {
				t.Fatalf("the log of the release = %q, want the released line naming the daemon's own reason", line)
			}

			// The same issue held again and written to todo again: the second release runs as the
			// first did. The second todo is the daemon's own write whichever way the first
			// arrived, because the in_progress the first release queued has not echoed back from
			// Dispatch — the record still shows Dispatch at todo — and a route's own write is a
			// status move whatever the record last saw Dispatch show.
			apply("worker-failed-again", intake.ClaimFailed{Issue: root, Role: claim.RoleImplementer})
			if held := storedIssue(t, pool, root); held.Phase != phase.Held || held.Hold == nil || held.Hold.From != phase.Implementing {
				t.Fatalf("the root after its worker failed again = %#v, want it held from implementing", held)
			}

			apply("todo-again", todo(6, true))

			again := storedIssue(t, pool, root)
			if again.Phase != phase.Implementing || again.Hold != nil || again.Generation != 3 || again.LastDispatchSeq != 6 {
				t.Fatalf("the root after the second todo = %#v, want it back in implementing at generation 3, its hold cleared, the second event recorded", again)
			}
			if got := statusWrites(t, pool, root); len(got) != 2 || got[1] != want {
				t.Fatalf("status writes after the second release = %+v, want a second %+v", got, want)
			}
			if got := startTask(t, pool, claim.RoleImplementer); !strings.Contains(got, reason) {
				t.Fatalf("the implementer the second release started is told %q, want the daemon's own reason %q", got, reason)
			}
			if got := phaseCounts(t, pool, root, claim.RoleImplementer); !reflect.DeepEqual(got, counted) {
				t.Fatalf("the implementer's counts after the second release = %+v, want the %+v the hold left", got, counted)
			}
		})
	}

	// The release's own path can move the issue, and then the status it writes is of the phase
	// the issue ends in rather than of the phase the hold came from: that is what R7 asks for,
	// and the board would otherwise stand at the todo the release answered until the hourly
	// status repair. Each of these two rows is such a release.
	t.Run("a hold from testing whose head is red", func(t *testing.T) {
		pool := migratedPool(t)
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root, Project: "LEGION",
			Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Testing}, Generation: 3, Status: "testing", DispatchStatus: "testing",
			Rank: "U", HandedOver: true, LastDispatchSeq: 4})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root, Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/" + root, HeadSHA: "c0ffee", Required: []string{"lint"}})
		seedPhase(t, pool, record.PhaseRow{Issue: root, Role: claim.RoleImplementer, Claim: "implement-claim"})
		seedPhase(t, pool, record.PhaseRow{Issue: root, Role: claim.RoleTester, Claim: "test-claim"})
		// Held, the issue has no checks_red row, so the red moves nothing until the release.
		apply("red", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "c0ffee", CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}},
			Generation: 1, Snapshot: "red-c0ffee", Failing: []string{"lint"}})
		if held := storedIssue(t, pool, root); held.Phase != phase.Held {
			t.Fatalf("the root after the red = %#v, want it still held: a held issue has no checks_red row", held)
		}

		apply("todo", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 5, Type: "issue.updated", Status: "todo",
			Title: "root", Rank: "U", HandedOver: true})

		released := storedIssue(t, pool, root)
		if released.Phase != phase.Implementing || released.Hold != nil || released.DispatchStatus != "todo" {
			t.Fatalf("the root after the todo = %#v, want the red to have sent it back to implementing, its hold cleared and the todo recorded", released)
		}
		// Two rows, of which the release's is the one that writes: the move's own names the
		// status the record held, which Dispatch does not show, so the executor finishes it
		// without a write.
		want := []record.StatusWrite{{Status: "in_progress", ObservedStatus: "testing"}, {Status: "in_progress", ObservedStatus: "todo"}}
		if got := statusWrites(t, pool, root); !reflect.DeepEqual(got, want) {
			t.Fatalf("status writes = %+v, want %+v: the move's own row, then the status of the phase the issue ends in", got, want)
		}
		starts := superviseRowsOf(t, pool, root, "start")
		if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Phase != phase.Implementing {
			t.Fatalf("start rows = %+v, want the one implementer start the red's move made: the restored phase's worker is not started", starts)
		}
	})

	t.Run("a hold from reviewing whose round is decided", func(t *testing.T) {
		pool := migratedPool(t)
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		// The reviewer completed, its claim then failed, and its approval landed while the issue
		// was held: the release finds both halves of the round in and ends it in accepting.
		seedReview(t, pool, "green")
		apply("reviewer-completed", intake.HandoffComplete{Generation: 1, Issue: root, Role: claim.RoleReviewer, Claim: "review-claim",
			Summary: "reviewed", HandoffRecord: "review-1", HandoffVersion: 1})
		apply("reviewer-failed", intake.ClaimFailed{Issue: root, Role: claim.RoleReviewer})
		apply("approved", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "approved", CommitID: "head", Author: testReviewApp, Body: "the review"})
		if held := storedIssue(t, pool, root); held.Phase != phase.Held || held.Hold == nil || held.Hold.From != phase.Reviewing {
			t.Fatalf("the root after the review = %#v, want it still held from reviewing: a held round decides nothing", held)
		}

		apply("todo", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 5, Type: "issue.updated", Status: "todo",
			Title: "root", Rank: "U", HandedOver: true})

		released := storedIssue(t, pool, root)
		if released.Phase != phase.Accepting || released.Hold != nil || released.DispatchStatus != "todo" {
			t.Fatalf("the root after the todo = %#v, want the round to have settled it into accepting, its hold cleared and the todo recorded", released)
		}
		// The move itself wrote nothing: reviewing and accepting show the same status, and
		// Engine.status drops a write of the status the record already holds. This row is the
		// only one, and without it the board would stand at todo.
		want := []record.StatusWrite{{Status: "needs_review", ObservedStatus: "todo"}}
		if got := statusWrites(t, pool, root); !reflect.DeepEqual(got, want) {
			t.Fatalf("status writes = %+v, want the one %+v: the status of the phase the issue ends in", got, want)
		}
	})
}

// storedIssue is the record of key.
func storedIssue(t *testing.T, pool *pgxpool.Pool, key string) record.Issue {
	t.Helper()
	var got *record.Issue
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		got, err = record.NewStore().Issue(t.Context(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	if got == nil {
		t.Fatalf("%s is not recorded", key)
	}
	return *got
}

// phaseCounts is role's phases row of issue with the counts alone: what a release must leave
// where it found them.
func phaseCounts(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) record.PhaseRow {
	t.Helper()
	got := record.PhaseRow{Issue: issue, Role: role}
	if err := pool.QueryRow(t.Context(), "select rounds, conflict_rounds, backward_moves, cap_told from phases where issue = $1 and role = $2",
		issue, string(role)).Scan(&got.Rounds, &got.ConflictRounds, &got.BackwardMoves, &got.CapTold); err != nil {
		t.Fatalf("read the %s's phases row of %s: %v", role, issue, err)
	}
	return got
}

// slotCount is how many admission slots issue holds.
func slotCount(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), "select count(*) from slots where issue = $1", issue).Scan(&got); err != nil {
		t.Fatalf("count the slots of %s: %v", issue, err)
	}
	return got
}
