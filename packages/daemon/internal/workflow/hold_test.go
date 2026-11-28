package workflow

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
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
	"github.com/sjawhar/legion/daemon/internal/record/fencetest"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A tree's architect is who every notice of its tree reaches that no child's own architect takes,
// so its own claim failing — its launches or prompts ran out — would reach nobody unless the daemon
// says so: the failure is a worker-died notice naming the architect and the phase the root is in,
// written for the architect that owns the issue and for the controller. Nothing is held: a phase
// is its worker's, and the root's planner here keeps its phase.
func TestATreeArchitectsFailedClaimIsNoticedAndHoldsNoPhase(t *testing.T) {
	t.Parallel()
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

// The architect of a root out of Legion's work — closed as done, or moved out by Dispatch — that
// fails is told to that architect's own role alone: the controller's skill does nothing for a
// failed architect whose root is out of work, which is closed or waits on a person, so no
// controller copy is written (#4065). Nothing is held either way.
func TestAFailedArchitectOfARootOutOfLegionsWorkIsToldToTheArchitectAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status string
		phase        phase.Phase
	}{
		{name: "closed as done", status: "done", phase: phase.Done},
		{name: "moved to backlog", status: "backlog", phase: phase.Planning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := migratedPool(t)
			ctx := context.Background()
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: tc.phase, Generation: 1, Status: tc.status, Rank: "U",
				HoldsGate: true})

			if _, err := intake.ApplyFact(ctx, pool, "supervise", "architect-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleArchitect}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact the architect's failed claim: %v", err)
			}
			died := record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: tc.phase}
			if got, want := noticeRows(t, pool), []record.OutboxPayload{died}; !reflect.DeepEqual(got, want) {
				t.Fatalf("notice rows = %+v, want %+v, to the issue alone and none to the controller", got, want)
			}
		})
	}
}

// A role that finished its phase stays live until its issue closes, so its claim can fail while
// another role works the phase, or while the phase is held for another role's failure. The
// architect is told in a worker-died naming the role and saying the phase is not its, and the
// issue's phase stands: nothing is held for a role that holds no phase, and the controller is not
// told of a phase that is not stalled. A lingering tree and a child that left the workflow tell
// nobody, since their claims were suspended or closed. A child's own architect's failure holds
// nothing either, even at accepting, its own phase, since claimFailed answers an architect before
// it reads the phase: it is told in a worker-died of the architect on the child, which reaches the
// architect above it, and that architect takes the child's notices until the daemon starts the
// child's architect again (TestASubArchitectsFailedClaimTellsTheArchitectAboveIt).
func TestAFinishedRolesFailedClaimIsNoticedAndHoldsNothing(t *testing.T) {
	t.Parallel()
	root := "LEGION-208"
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
			name:  "the tester while the issue is merging",
			root:  record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"},
			child: record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "V"},
			role:  claim.RoleTester, wantPhase: phase.Merging,
			want: []record.OutboxPayload{record.Notice{Kind: "worker-died", Role: claim.RoleTester, Phase: phase.Merging,
				Reason: "the tester does not work LEGION-209's phase merging; nothing is held"}},
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
			want: []record.OutboxPayload{record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Implementing,
				Reason: "LEGION-209's own architect failed; its notices come to you until it runs again, and the daemon starts it again at LEGION-209's next phase start"}},
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

// A phase worker's claim failing holds the issue, and the two notices that say so — the hold and
// the worker-died beside it, both the architect's alone — are all an agent has to decide between
// retrying the phase and escalating it. So each names the failure the fact carries: the one word
// for what failed the claim, the sentence written beside that word, and each of the four counts
// the failure left against the limit it ran out at, deaths against the launch failure limit,
// which is the limit the supervisor measures them by (record.NoticeFailure).
// The record also carries the two causes the claim recorded, its last death and its last failed
// start, which an agent deciding between a retry and an escalation has no other way to read
// (R6); `reason` keeps its one value, `escalated`, which both agents' texts read as the
// architect having already escalated this hold.
//
// Three other shapes of worker-died are here beside it (a fourth, a child's own architect's, is
// TestASubArchitectsFailedClaimTellsTheArchitectAboveIt), because the evidence each carries is a
// decision of its own: the worker-died of a role that does not work the issue's phase names the
// failure too, since that role's architect decides whether to start it again on the same
// evidence; the tree architect's own carries no failure record and names the sentence in its
// reason instead, as R5 has it, and the one that ends a closed issue's production-check look
// carries no failure record either, since its reason already names the cause.
func TestTheHeldNoticeNamesTheFailure(t *testing.T) {
	t.Parallel()
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

	t.Run("the hold and the worker-died, to the architect alone", func(t *testing.T) {
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
		// No controller copy of the hold: the architect retries or escalates it, and the
		// controller's skill does nothing for a hold that is not escalated (#4065).
		// TestAnEscalationIsRecordedOnTheHoldForAControllerThatStartsLater holds that the
		// escalation reaches the controller.
		if want := []record.OutboxPayload{held, died}; !sameNotices(got, want) {
			t.Fatalf("notice rows = %+v, want %+v: the hold and the worker-died to the architect, each naming the failure, and no copy to the controller", got, want)
		}
		// Decoded from the stored rows, so each field above crossed the wire; the hold's `reason`
		// is read off the row's own keys, since a notice that carried one as "" decodes the same.
		for _, payload := range got {
			if _, err := record.NewOutboxRow(root.Key, record.Fenced(payload, record.FirstFence), time.Now()); err != nil {
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

	t.Run("a tree architect's claim names no failure record and its reason names the sentence, quoted", func(t *testing.T) {
		pool := migratedPool(t)
		seedIssue(t, pool, root)

		if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "architect-failed", failed(root.Key, claim.RoleArchitect), testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("ApplyFact the architect's failed claim: %v", err)
		}
		died := record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Planning, Reason: strconv.Quote(sentence)}
		if got, want := noticeRows(t, pool), []record.OutboxPayload{died, record.ControllerNotice(died)}; !sameNotices(got, want) {
			t.Fatalf("notice rows = %+v, want %+v, naming no failure record and the sentence as the reason", got, want)
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
		// The supervisor's sentence is quoted, as every worker-died's reason carries it
		// (deathSentence); this one is a single short line, so nothing of it is cut. The cause and
		// the look that retries the claim follow it (checkRoleFailedReason).
		if len(died) != 1 || died[0].Role != claim.RoleTester || died[0].Failure != nil {
			t.Fatalf("worker-died notices of the ended look = %+v, want one of the tester carrying no failure record: its reason names the failure already", died)
		}
		for _, want := range []string{
			"production check look 1 lost its tester: " + strconv.Quote(sentence) + "; ",
			"its cause is died",
			"look 2 retries the claim at " + checkOpened.Add(CheckLookBound).Format(time.RFC3339) + " with one attempt left",
		} {
			if !strings.Contains(died[0].Reason, want) {
				t.Errorf("the worker-died of the ended look reads %q, want it to say %q", died[0].Reason, want)
			}
		}
	})
}

// An escalation is recorded on the held issue as well as noticed to the architect that owns the
// issue and to the controller, so a controller that starts after it finds it in the state it reads at boot
// (issues.<KEY>.holdReason); the retry that ends the hold clears it.
func TestAnEscalationIsRecordedOnTheHoldForAControllerThatStartsLater(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// A held root a person moves out of Legion's work keeps its run (R20 of dispatch://INFRA-3320): its
// phase, its hold and the escalation on it stay, which the state shows as facts of the root's own
// record, and nothing lingers. A todo on it later releases the hold as on any held issue
// (releaseHeldRoot).
func TestAStoppedRootKeepsItsHoldAndItsEscalation(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Planning}, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "escalate", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.EscalateDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "root-backlog", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("move the root to backlog: %v", err)
	}
	var heldFrom, holdReason, status string
	if err := pool.QueryRow(ctx, "select held_from, hold_reason, status from issues where key = $1", "LEGION-208").Scan(&heldFrom, &holdReason, &status); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if heldFrom != string(phase.Planning) || holdReason != "escalated" || status != "backlog" {
		t.Fatalf("the root's record after the backlog move holds it from %q for %q at %s, want from planning for escalated at backlog", heldFrom, holdReason, status)
	}
	if got, reason := projectedHold(t, pool, "LEGION-208"); got != phase.Held || reason != "escalated" {
		t.Fatalf("after the backlog move the state reads phase %s hold reason %q, want held and escalated", got, reason)
	}
}

// A held child keeps its hold when its root stops, escalation included: a stop of the issue above
// it changes nothing of it (R20 of dispatch://INFRA-3320), so the state the controller reads at
// every start shows it held and escalated, as its record holds it.
func TestAnEscalatedChildKeepsItsHoldWhenItsRootStops(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	root, from := "LEGION-208", phase.Implementing
	seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Held, Hold: &record.Hold{From: from}, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "escalate", intake.RetryOrEscalate{Issue: "LEGION-209", Decision: intake.EscalateDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("escalate the child: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-209"); got != phase.Held || reason != "escalated" {
		t.Fatalf("under a running root the state reads the child's phase %s hold reason %q, want held and escalated", got, reason)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "root-backlog", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("move the root to backlog: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-209"); got != phase.Held || reason != "escalated" {
		t.Fatalf("with its root stopped the state reads the child's phase %s hold reason %q, want held and escalated", got, reason)
	}
	var heldFrom, holdReason string
	if err := pool.QueryRow(ctx, "select held_from, hold_reason from issues where key = $1", "LEGION-209").Scan(&heldFrom, &holdReason); err != nil {
		t.Fatalf("read the child's hold: %v", err)
	}
	if heldFrom != string(from) || holdReason != "escalated" {
		t.Fatalf("the child's record holds it from %q for %q, want from %s for escalated", heldFrom, holdReason, from)
	}
}

// projectedHold is the issue's phase and hold reason as the state view shows them.
func projectedHold(t *testing.T, pool *pgxpool.Pool, key string) (phase.Phase, string) {
	t.Helper()
	var state api.State
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		state, err = projection.Project(t.Context(), projection.Inputs{
			Tx: tx, Store: record.NewStore(), Project: "LEGION",
			Now: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		})
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
		payloads = append(payloads, fencetest.Unfenced(payload))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the notice rows: %v", err)
	}
	return payloads
}

// A retry's task names the issue's pull request, as a backward move's does, so the restarted
// worker is told the pull request it works on.
func TestARetrysTaskNamesThePullRequest(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Generation: 1, Status: "in_progress", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	task := startTask(t, pool, phase.RoleFor(phase.Implementing))
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
// generation, the pass, the rounds, the conflict rounds, the backward moves, the cap count and
// the admission slot all stand as they were.
//
// A person's move in the Dispatch dashboard and the daemon's own write — which the controller's
// set_status and an operator's `legion issue status` both reach the record as — are the same event
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
	t.Parallel()
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
			engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil)))
			apply := applyFacts(t, pool, engine)
			seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION",
				Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Generation: 3, Status: "in_progress", DispatchStatus: "in_progress",
				Rank: "U", HandedOver: true, LastDispatchSeq: 4})
			seedSlot(t, pool, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
			// The counts three review rounds that requested changes, one conflict round and two
			// tester fails leave, the third counted round reaching review_round_cap 3 (recordPass,
			// recordRound). Each release must leave every one of them, the pass among them, where
			// it found it.
			counted := record.ReturnCounts{Pass: 6, Rounds: 4, ConflictRounds: 1,
				BackwardMoves: map[string]int{"reviewing-implementing": 3, "testing-implementing": 2}, CapTold: 3}
			seedPhase(t, pool, record.PhaseRow{Issue: root, Role: claim.RoleImplementer, Pass: counted.Pass, Rounds: counted.Rounds,
				ConflictRounds: counted.ConflictRounds, BackwardMoves: counted.BackwardMoves, CapTold: counted.CapTold})
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
			want := record.OrdinaryStatusWrite("in_progress", "todo", "")
			if got := statusWrites(t, pool, root); len(got) != 1 || got[0] != want {
				t.Fatalf("status writes = %+v, want the one %+v: the status of the phase the issue ends in", got, want)
			}
			starts := superviseRowsOf(t, pool, root, "start")
			if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Phase != phase.Implementing || !strings.Contains(starts[0].Task, reason) {
				t.Fatalf("start rows = %+v, want one implementer start of implementing whose task carries %q", starts, reason)
			}
			if got := taskCounts(t, pool, engine, root, 3); !reflect.DeepEqual(got, counted) {
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
			if got := taskCounts(t, pool, engine, root, 3); !reflect.DeepEqual(got, counted) {
				t.Fatalf("the implementer's counts after the second release = %+v, want the %+v the hold left", got, counted)
			}
		})
	}

	// The release's own path can move the issue, and then the status it writes is of the phase
	// the issue ends in rather than of the phase the hold came from: that is what R7 asks for,
	// and the board would otherwise stand at the todo the release answered until the hourly
	// status repair. A red head is no such release on this contract — a red moves no issue at
	// all, whoever reads it (R1) — so the review round below is the one row left.
	t.Run("a hold from reviewing whose round is decided", func(t *testing.T) {
		pool := migratedPool(t)
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		// The reviewer completed, its claim then failed, and its approval landed while the issue
		// was held: the release finds both halves of the round in and ends it in accepting.
		seedReview(t, pool, "green")
		// GitHub reports the head one it would merge, which is what the settle asks of it: the
		// release below decides the round rather than the merge state.
		apply("merge-state", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: headOne,
			Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
		apply("reviewer-completed", intake.HandoffComplete{Generation: 1, Issue: root, Role: claim.RoleReviewer, Claim: reviewClaim,
			Summary: "reviewed", HandoffRecord: "review-1", HandoffVersion: 1})
		apply("reviewer-failed", intake.ClaimFailed{Issue: root, Role: claim.RoleReviewer})
		apply("approved", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "approved", CommitID: headOne, Author: testReviewApp, Body: reviewBody("the review", headOne)})
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
		want := []record.StatusWrite{record.OrdinaryStatusWrite("needs_review", "todo", "")}
		if got := statusWrites(t, pool, root); !reflect.DeepEqual(got, want) {
			t.Fatalf("status writes = %+v, want the one %+v: the status of the phase the issue ends in", got, want)
		}
	})
}

// A todo a session wrote on a held root whose tree runs is none of the moves that release the hold
// (TestATodoOnAHeldRootReleasesTheHold): the workflow reads who wrote the status before it reads
// the hold (sessionStatusWrite), and a session's lifecycle status is set back. The hold stays from
// the phase it is from, and the root keeps its generation and its counts. The record keeps its own
// status, in_progress, and takes the observation: Dispatch shown at todo, at the event's sequence.
// Legion writes no status of a phase the issue works: it writes its own status back over the todo,
// in a set-back that names the todo and its sequence (record.ReassertStatusWrite). No worker is
// started, so no worker is told anything. A session that holds a claim in the tree, the tree's
// architect here, is an agent, and nobody is told; a session that holds none, from outside the
// tree, has the tree's architect told, in a status-reasserted notice.
func TestASessionsTodoOnAHeldRootKeepsTheHold(t *testing.T) {
	t.Parallel()
	const root = "LEGION-208"
	for _, tc := range []struct {
		name string
		// session is the session that writes the todo, and claims says it holds a claim in the tree.
		session string
		claims  bool
		// told is the kind of the one notice the todo writes for the tree's architect, "" for none.
		told record.NoticeKind
	}{
		{name: "the tree's architect", session: "ses", claims: true},
		{name: "a session outside the tree", session: "ses-outsider", told: "status-reasserted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			apply := applyFacts(t, pool, engine)
			seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION",
				Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Generation: 3, Status: "in_progress", DispatchStatus: "in_progress",
				Rank: "U", HandedOver: true, LastDispatchSeq: 4})
			seedSlot(t, pool, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
			// The counts three review rounds that requested changes, one conflict round and two
			// tester fails leave, the third counted round reaching review_round_cap 3 (recordPass,
			// recordRound).
			counted := record.ReturnCounts{Pass: 6, Rounds: 4, ConflictRounds: 1,
				BackwardMoves: map[string]int{"reviewing-implementing": 3, "testing-implementing": 2}, CapTold: 3}
			seedPhase(t, pool, record.PhaseRow{Issue: root, Role: claim.RoleImplementer, Pass: counted.Pass, Rounds: counted.Rounds,
				ConflictRounds: counted.ConflictRounds, BackwardMoves: counted.BackwardMoves, CapTold: counted.CapTold})
			if tc.claims {
				seedPhaseWorker(t, pool, root, claim.RoleArchitect, "")
			}
			notices := len(noticeRows(t, pool))

			apply("todo", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated", Status: "todo",
				Title: "root", Rank: "U", HandedOver: true, ActorSession: tc.session})

			kept := storedIssue(t, pool, root)
			if kept.Phase != phase.Held || kept.Hold == nil || *kept.Hold != (record.Hold{From: phase.Implementing}) {
				t.Fatalf("the root after the session's todo stands in %s with hold %+v, want it still held from implementing", kept.Phase, kept.Hold)
			}
			if kept.Generation != 3 || kept.Status != "in_progress" || kept.DispatchStatus != "todo" || kept.LastDispatchSeq != 5 {
				t.Fatalf("the root after the session's todo = %#v, want generation 3, in_progress kept, Dispatch shown at todo and the event's sequence 5 recorded", kept)
			}
			want := []record.StatusWrite{record.ReassertStatusWrite("in_progress", "todo", 5)}
			if got := statusWrites(t, pool, root); !reflect.DeepEqual(got, want) {
				t.Fatalf("status writes = %+v, want the one set-back %+v", got, want)
			}
			if starts := superviseRowsOf(t, pool, root, "start"); len(starts) != 0 {
				t.Fatalf("start rows = %+v, want none: no worker starts, so none is told why", starts)
			}
			added := noticeRows(t, pool)[notices:]
			switch {
			case tc.told == "" && len(added) != 0:
				t.Fatalf("the todo wrote notices %+v, want none", added)
			case tc.told != "":
				if len(added) != 1 {
					t.Fatalf("the todo wrote notices %+v, want one %s for the architect", added, tc.told)
				}
				if notice, ok := added[0].(record.Notice); !ok || notice.Kind != tc.told || notice.Role != claim.RoleArchitect {
					t.Fatalf("the todo's notice = %+v, want a %s for the architect", added[0], tc.told)
				}
				// The notice reaches the architect that owns the issue its row names: the root's.
				if onRoot := noticesOf(t, pool, root, tc.told); len(onRoot) != 1 {
					t.Fatalf("the %s notices on %s's rows = %+v, want the one the todo wrote", tc.told, root, onRoot)
				}
			}
			if got := taskCounts(t, pool, engine, root, 3); !reflect.DeepEqual(got, counted) {
				t.Fatalf("the counts after the session's todo = %+v, want the %+v the hold left", got, counted)
			}
		})
	}
}

// A person's todo on a root held from reviewing, by its reviewer's failed claim, ends a hold no plan
// made (releaseHeldRoot, retryPhase). It opens no new generation: the root keeps its generation and
// its one slot. The counts of its returns, which a root with no plan rows keeps on its implementer's
// phases row (record.ImplementerCounts), stand as the hold left them until a review round settles,
// and the round's move back raises them by one pass, one move back under reviewing-implementing
// (recordPass) and one review round (recordRound). The round settles in one of two places.
//
// In the first case the reviewer left its round open, so the release settles nothing and starts the
// reviewer again, and the counts stand after it. The review that requests changes comes after the
// todo and settles the round as it is recorded.
//
// In the second case the review that requests changes is recorded while the root is held: a hold
// from reviewing counts as standing there for the review (record.StandsIn), and a held round decides
// nothing (reviewRound), so the counts stand after the review too. The todo's release settles the
// round itself (retryPhase's settleRound): the counts rise there and the root moves into
// implementing. The release writes the status of the phase it ends in, in_progress over the todo
// Dispatch shows, beside the move's own row, which observes the record's needs_review.
//
// A todo on a root held for its stopped task is no such release either (planHold), and it takes
// one of two roads. A task whose work ran starts again at its first step and raises its pass
// (restartTask; TestATodoOnAStoppedRootStartsItsTaskAtStepOne). A task stopped before its first
// step went out waits for that step again and raises nothing (waitForFirstStep;
// TestAStopBeforeTheFirstStepSettlesThePlanAndStopsItsTask reads where it then stands).
func TestATodoOnARootWithAHoldNoPlanMadeKeepsTheCountsUntilAReviewRoundSettles(t *testing.T) {
	t.Parallel()
	const root = "LEGION-208"
	// The counts one conflict round and three review rounds that requested changes leave, the
	// third counted round reaching review_round_cap 3 (recordPass, recordRound); the settled round
	// is a fourth counted one, which tells the cap no second time.
	before := record.ReturnCounts{Pass: 4, Rounds: 4, ConflictRounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 3}, CapTold: 3}
	raised := record.ReturnCounts{Pass: 5, Rounds: 5, ConflictRounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 4}, CapTold: 3}
	changesRequested := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "changes_requested", CommitID: headOne,
		Author: testReviewApp, Body: reviewBody("Name the rollback path.", headOne)}
	todo := intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 5, Type: "issue.updated", Status: "todo",
		Title: "root", Rank: "U", HandedOver: true}
	// heldFromReviewing is the root in reviewing with the rounds before behind it, held from
	// reviewing by its reviewer's failed claim, with its counts and its one slot read.
	heldFromReviewing := func(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		apply := applyFacts(t, pool, engine)
		seedReview(t, pool, "green")
		seedSlot(t, pool, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
		seedPhase(t, pool, record.PhaseRow{Issue: root, Role: claim.RoleImplementer, Claim: "implement-claim", Pass: before.Pass, Rounds: before.Rounds,
			ConflictRounds: before.ConflictRounds, BackwardMoves: before.BackwardMoves, CapTold: before.CapTold})
		apply("reviewer-failed", intake.ClaimFailed{Issue: root, Role: claim.RoleReviewer})
		held := storedIssue(t, pool, root)
		if held.Phase != phase.Held || held.Hold == nil || *held.Hold != (record.Hold{From: phase.Reviewing}) {
			t.Fatalf("the root after its reviewer failed stands in %s with hold %+v, want held from reviewing", held.Phase, held.Hold)
		}
		if got := taskCounts(t, pool, engine, root, held.Generation); !reflect.DeepEqual(got, before) {
			t.Fatalf("the counts before the todo = %+v, want the %+v seeded", got, before)
		}
		if got := slotCount(t, pool, root); got != 1 {
			t.Fatalf("%s holds %d slots before the todo, want 1", root, got)
		}
		return pool, engine, apply, held
	}

	t.Run("the review that requests changes comes after the todo", func(t *testing.T) {
		pool, engine, apply, held := heldFromReviewing(t)

		apply("todo", todo)

		released := storedIssue(t, pool, root)
		if released.Phase != phase.Reviewing || released.Hold != nil || released.Generation != held.Generation {
			t.Fatalf("the root after the todo stands in %s with hold %+v at generation %d, want reviewing, no hold and generation %d",
				released.Phase, released.Hold, released.Generation, held.Generation)
		}
		if got := slotCount(t, pool, root); got != 1 {
			t.Fatalf("%s holds %d slots after the todo, want the 1 it held", root, got)
		}
		if got := taskCounts(t, pool, engine, root, held.Generation); !reflect.DeepEqual(got, before) {
			t.Fatalf("the counts after the todo = %+v, want the %+v the hold left", got, before)
		}

		apply("changes-requested", changesRequested)

		if moved := storedIssue(t, pool, root); moved.Phase != phase.Implementing || moved.Generation != held.Generation {
			t.Fatalf("the root after the request for changes stands in %s at generation %d, want implementing at generation %d", moved.Phase, moved.Generation, held.Generation)
		}
		if got := taskCounts(t, pool, engine, root, held.Generation); !reflect.DeepEqual(got, raised) {
			t.Fatalf("the counts after the round settled = %+v, want %+v", got, raised)
		}
	})

	t.Run("the review that requests changes comes while the root is held, and the todo settles it", func(t *testing.T) {
		pool, engine, apply, held := heldFromReviewing(t)

		apply("changes-requested", changesRequested)

		if still := storedIssue(t, pool, root); still.Phase != phase.Held || still.Hold == nil || *still.Hold != (record.Hold{From: phase.Reviewing}) {
			t.Fatalf("the root after the request for changes stands in %s with hold %+v, want it still held from reviewing: a held round decides nothing", still.Phase, still.Hold)
		}
		if decision := reviewerDecision(t, pool); decision == nil || decision.State != "changes_requested" {
			t.Fatalf("the reviewer's decision after the request for changes = %+v, want changes_requested recorded on the held round", decision)
		}
		if got := taskCounts(t, pool, engine, root, held.Generation); !reflect.DeepEqual(got, before) {
			t.Fatalf("the counts after the request for changes, before the todo = %+v, want the %+v the hold left: the round has not settled", got, before)
		}

		apply("todo", todo)

		moved := storedIssue(t, pool, root)
		if moved.Phase != phase.Implementing || moved.Hold != nil || moved.Generation != held.Generation {
			t.Fatalf("the root after the todo stands in %s with hold %+v at generation %d, want implementing, no hold and generation %d",
				moved.Phase, moved.Hold, moved.Generation, held.Generation)
		}
		if got := slotCount(t, pool, root); got != 1 {
			t.Fatalf("%s holds %d slots after the todo, want the 1 it held", root, got)
		}
		if got := taskCounts(t, pool, engine, root, held.Generation); !reflect.DeepEqual(got, raised) {
			t.Fatalf("the counts after the todo settled the round = %+v, want %+v", got, raised)
		}
		want := []record.StatusWrite{record.OrdinaryStatusWrite("in_progress", "needs_review", ""), record.OrdinaryStatusWrite("in_progress", "todo", "")}
		if got := statusWrites(t, pool, root); !reflect.DeepEqual(got, want) {
			t.Fatalf("status writes = %+v, want the move's %+v and the release's %+v, the status of the phase the root ends in over the todo", got, want[0], want[1])
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

// phaseCounts is role's phases row of issue with its rounds, conflict rounds, moves back and cap
// mark alone, the pass left out.
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

// R2: an architect's claim failing holds nothing and writes no held notice, at admitted as at
// accepting. Both phases are the architect's own (phase.RoleFor), and claimFailed answers an
// architect before it asks which role works the phase: a tree's own architect is told in a
// worker-died, and a child's own architect's failure, at accepting as anywhere, is told only in a
// worker-died of the architect on the child, which reaches the architect above it, with no
// controller copy (TestASubArchitectsFailedClaimTellsTheArchitectAboveIt). Nothing here reaches
// HeldNotice, which is why no hold is ever recorded from either phase.
func TestAnArchitectsClaimFailedHoldsNoIssueAndWritesNoHeldNotice(t *testing.T) {
	t.Parallel()
	const root = "LEGION-208"
	for _, tc := range []struct {
		name  string
		key   string
		at    phase.Phase
		child bool
		want  []record.OutboxPayload
	}{
		{
			name: "the tree's architect while its root stands at admitted", key: root, at: phase.Admitted,
			want: []record.OutboxPayload{
				record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Admitted},
				record.ControllerNotice(record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Admitted}),
			},
		},
		{
			name: "a child's sub-architect while the child is in accepting", key: "LEGION-209", at: phase.Accepting, child: true,
			want: []record.OutboxPayload{record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Accepting,
				Reason: "LEGION-209's own architect failed; its notices come to you until it runs again, and the daemon starts it again at LEGION-209's next phase start"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			rootPhase := tc.at
			if tc.child {
				rootPhase = phase.Implementing
			}
			seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
				Project: "LEGION", Title: "root", Phase: rootPhase, Generation: 1, Status: "in_progress", Rank: "U"})
			if tc.child {
				parent := root
				seedIssue(t, pool, record.Issue{Key: tc.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
					Parent: &parent, Project: "LEGION", Title: "child", Phase: tc.at, Generation: 1, Status: "needs_review", Rank: "V"})
			}

			if _, err := intake.ApplyFact(ctx, pool, "supervise", "architect-failed", intake.ClaimFailed{Issue: tc.key, Role: claim.RoleArchitect},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact the architect's failed claim: %v", err)
			}

			var gotPhase phase.Phase
			var heldFrom *string
			if err := pool.QueryRow(ctx, "select phase, held_from from issues where key = $1", tc.key).Scan(&gotPhase, &heldFrom); err != nil {
				t.Fatalf("read %s: %v", tc.key, err)
			}
			if gotPhase != tc.at || heldFrom != nil {
				t.Fatalf("after the architect's failure %s is in %s held from %v, want %s and no hold", tc.key, gotPhase, heldFrom, tc.at)
			}
			if got := noticeRows(t, pool); !sameNotices(got, tc.want) {
				t.Fatalf("notice rows = %+v, want %+v: no held notice is written for an architect", got, tc.want)
			}
		})
	}
}

// A root a hand-over of the release before carried in held, from the phase whose worker's claim
// failed, starts no worker at its promotion (StartCarriedRun): the hold's own retry is what starts
// that worker again. Its own architect is told of the hold as a root's (HeldNotice), naming the
// phase the hold came from and the role that works it (phase.RoleFor of a held phase, which is
// never admitted), since the hold's first notice was routed in the tree it left.
func TestACarriedRootHeldFromItsPhaseStartsNoWorkerAndTellsItsArchitectTheHold(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Testing, Reason: record.HoldEscalated},
		Generation: 2, CarriedGeneration: 2, Status: "in_progress", Rank: "U", HandedOver: true, LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.StartCarriedRun(ctx, tx, root)
	})

	want := []record.OutboxPayload{record.Notice{Kind: "held", Role: claim.RoleTester, Phase: phase.Testing, Reason: string(record.HoldEscalated)}}
	if got := noticeRows(t, pool); !sameNotices(got, want) {
		t.Fatalf("notice rows = %+v, want %+v: the root's architect is told the hold, naming the tester's phase", got, want)
	}
	for _, start := range outboxOf(t, pool, string(record.OutboxKindSupervise)) {
		if start.payload["op"] == "start" && start.payload["task"] != nil && start.payload["task"] != "" {
			t.Fatalf("supervise row %+v hands out work, want none: a held root's worker is started by its hold's retry", start)
		}
	}
	if got := storedIssue(t, pool, root.Key); got.Phase != phase.Held || got.Hold == nil || got.Hold.From != phase.Testing {
		t.Fatalf("the carried root = phase %s, hold %+v; want it held from testing still", got.Phase, got.Hold)
	}
}

// A root a hand-over of the release before carried in mid-phase, with an open Dispatch child,
// starts no worker at its promotion (StartCarriedRun): the start of the phase it carries is a
// phase start like any other (dispatch://INFRA-3320 R9), so the root waits in awaiting_children
// holding that start, and no claim of it starts, since no role of a parent that waits on its
// children works (R11). Once the child is done, the return starts the phase's worker
// (ReturnFromChildren), as the admission pass's return does.
func TestACarriedRootWithAnOpenChildWaitsOnItAtItsPromotionAndStartsOnceItIsDone(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := record.Issue{Key: cwParent, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: cwParent,
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 2, CarriedGeneration: 2, Status: "in_progress",
		DispatchStatus: "todo", Rank: "U", HandedOver: true, HoldsGate: true, TakenBack: true, LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	mirrorChild(t, pool, cwChild, cwParent, "todo", 1)
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.StartCarriedRun(ctx, tx, root)
	})

	if got := storedIssue(t, pool, cwParent); got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil ||
		got.ChildrenWait.Start != phase.Implementing || !slices.Equal(got.ChildrenWait.Children, []string{cwChild}) {
		t.Fatalf("the carried root = phase %s, held start %+v; want awaiting_children holding the start of implementing for %s",
			got.Phase, got.ChildrenWait, cwChild)
	}
	if starts := superviseRowsOf(t, pool, cwParent, "start"); len(starts) != 0 {
		t.Fatalf("starts of %s = %+v, want none: no role of a parent that waits on its children works", cwParent, starts)
	}

	returnsFromChildren(t, pool, engine, cwParent)

	if got := storedIssue(t, pool, cwParent); got.Phase != phase.Implementing || got.ChildrenWait != nil {
		t.Fatalf("the carried root once its child is done = phase %s, held start %+v; want implementing", got.Phase, got.ChildrenWait)
	}
	if starts := roleStartsOf(t, pool, cwParent, claim.RoleImplementer); len(starts) != 1 || starts[0].Phase != phase.Implementing {
		t.Fatalf("the implementer's starts = %+v, want the return's one of implementing", starts)
	}
}

// A root a hand-over of the release before carried in whose phase's role stands on an entry a
// move out of retro raised (record.PhaseRow.EntryFrom), with an open Dispatch child, waits in
// awaiting_children at its promotion as any carried root does, and starts no worker
// (carriedStartHeld). The release of a hold restarts such a phase on that entry whatever its
// children are (acceptedEntry, through childrenHoldingSamePhaseStart), but the promotion would
// raise an entry of no move (StartPromotedWorker), which the admission pass after it stops as any
// started parent's (StopForChildren): so the promotion holds the start that pass would hold, and
// no worker starts only to be stopped.
func TestACarriedRootOnAnEntryOutOfRetroWithAnOpenChildWaitsAtItsPromotion(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := record.Issue{Key: cwParent, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: cwParent,
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 2, CarriedGeneration: 2, Status: "in_progress",
		DispatchStatus: "todo", Rank: "U", HandedOver: true, HoldsGate: true, TakenBack: true, LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	seedPhase(t, pool, record.PhaseRow{Issue: cwParent, Role: claim.RoleImplementer, Entry: 3, EntryMovedBack: true, EntryFrom: phase.Retro})
	mirrorChild(t, pool, cwChild, cwParent, "todo", 1)
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.StartCarriedRun(ctx, tx, root)
	})

	if got := storedIssue(t, pool, cwParent); got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil ||
		got.ChildrenWait.Start != phase.Implementing || !slices.Equal(got.ChildrenWait.Children, []string{cwChild}) {
		t.Fatalf("the carried root = phase %s, held start %+v; want awaiting_children holding the start of implementing for %s",
			got.Phase, got.ChildrenWait, cwChild)
	}
	if starts := superviseRowsOf(t, pool, cwParent, "start"); len(starts) != 0 {
		t.Fatalf("starts of %s = %+v, want none: the promotion holds the start rather than launch a worker the next pass stops", cwParent, starts)
	}
	if row := phaseRowOf(t, pool, claim.RoleImplementer); row.Entry != 3 || row.EntryFrom != phase.Retro {
		t.Fatalf("the implementer's row = entry %d from %q, want entry 3 from retro: the hold raises no entry", row.Entry, row.EntryFrom)
	}
}

// A held issue that a stop left and Dispatch shows back in work at a status other than todo comes
// back held (ReturnToWork): the hold's retry is what starts the worker of the phase the hold came
// from (phase.RoleFor of that phase, which is never admitted), so nothing hands that role work,
// and the hold stays as it was.
func TestAHeldIssueBackInWorkStaysHeldAndStartsNoWorkerOfItsHeldPhase(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	hold := &record.Hold{From: phase.Testing, Reason: record.HoldEscalated}
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Held, Hold: hold, Generation: 1, Status: "backlog", DispatchStatus: "backlog",
		Rank: "U", HandedOver: true, LastDispatchSeq: 1})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester"})

	applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))("back", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion",
		Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "root", Rank: "U", HandedOver: true})

	if got := storedIssue(t, pool, "LEGION-208"); got.Phase != phase.Held || got.Hold == nil || *got.Hold != *hold || got.Status != "in_progress" {
		t.Fatalf("the returned issue = phase %s, status %s, hold %+v; want it held from testing, in_progress", got.Phase, got.Status, got.Hold)
	}
	for _, start := range outboxOf(t, pool, string(record.OutboxKindSupervise)) {
		if start.payload["op"] == "start" && start.payload["role"] == string(claim.RoleTester) {
			t.Fatalf("supervise row %+v starts the tester, want none: the hold's retry starts it", start)
		}
	}
}
