package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// subTreeTarget is LEGION-211 of the tree LEGION-208, beneath LEGION-209, whose architect is a
// sub-architect of the tree, with LEGION-210, a sibling of LEGION-209 whose architect is another
// sub-architect. LEGION-211 walks the plan `implementer acceptance:owner` at generation 3, with its
// open pull request #44, and stands at at: implementing, accepting, or held from implementing.
func subTreeTarget(t *testing.T, pool *pgxpool.Pool, engine *Engine, at phase.Phase) record.Issue {
	t.Helper()
	stepRoot(t, pool, phase.Implementing)
	todoChild(t, pool, "LEGION-209")
	todoChild(t, pool, "LEGION-210")
	target := stepChild(t, pool, "LEGION-211", phase.Admitted, nil)
	parent := "LEGION-209"
	target.Parent = &parent
	seedIssue(t, pool, target)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: target.Key, Repo: "sjawhar/legion", Number: 44,
		Branch: "legion/LEGION-211", HeadSHA: "head-44", Failing: []string{}})
	seedStepsPlan(t, pool, target.Key, target.Generation, "implementer", "acceptance:owner")
	walkTargetTo(t, pool, engine, target.Key, at)
	return issueOf(t, pool, engine, target.Key)
}

// walkTargetTo takes key, admitted, through its first step to at: implementing, accepting, or held
// from implementing.
func walkTargetTo(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, at phase.Phase) {
	t.Helper()
	firstStep(t, pool, engine, key)
	switch at {
	case phase.Accepting:
		move(t, pool, engine, key, TriggerImplementationReady, "", record.PhaseRow{})
	case phase.Held:
		holdFrom(t, pool, key, phase.Implementing)
	}
	if got := issueOf(t, pool, engine, key); got.Phase != at {
		t.Fatalf("premise: %s stands in %s, want %s", key, got.Phase, at)
	}
}

// subArchitect is the claim of the architect of key, a sub-architect of LEGION-208's tree.
func subArchitect(t *testing.T, key string) claim.Token {
	return claim.Token(treeRoleToken(t, key, claim.RoleArchitect))
}

// openStepOf is the run of the step key stands in, as a route reads it (record.Store.OpenStep).
func openStepOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) record.StepEntry {
	t.Helper()
	var entry record.StepEntry
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entry, err = engine.store.OpenStep(ctx, tx, key)
		return err
	})

	return entry
}

// personsTodoOnChild is a person's todo on key, a child of parent, in Dispatch: it re-enters a
// recorded child at its next generation (ReenterChild).
func personsTodoOnChild(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, parent string) {
	t.Helper()
	todo := intake.DispatchIssue{Key: key, ResolvedRepo: "sjawhar/legion", Seq: 5, Type: "issue.updated", Status: "todo",
		Title: key, Parent: parent, Rank: "V"}
	if _, err := intake.ApplyFact(t.Context(), pool, "dispatch", "todo-"+key, todo, engine, admissionStub{}); err != nil {
		t.Fatalf("the person's todo on %s: %v", key, err)
	}
}

// architectsRequest is one of sign_off, request_backward_move and retry_or_escalate as its route
// builds its fact from its read (api's signOff, phaseBackward and phaseRetry): the issue, the
// claim of the architect, the generation and, for a request about accepting, the run of accepting.
type architectsRequest struct {
	name string
	at   phase.Phase
	fact func(by claim.Token, read record.Issue, entry record.StepEntry) intake.Fact
	// moved says whether the request moved the issue from where it stood: the sign-off to retro,
	// the move back to implementing, the retry out of its hold.
	moved func(record.Issue) bool
}

var architectsRequests = []architectsRequest{
	{"sign_off", phase.Accepting, func(by claim.Token, read record.Issue, entry record.StepEntry) intake.Fact {
		return intake.SignOff{Issue: read.Key, By: by, Generation: read.Generation, Entry: entry}
	}, func(issue record.Issue) bool { return issue.Phase == phase.Retro }},
	{"request_backward_move", phase.Accepting, func(by claim.Token, read record.Issue, entry record.StepEntry) intake.Fact {
		return intake.BackwardMove{Issue: read.Key, Requester: claim.RoleArchitect, To: phase.Implementing, Reason: "the evidence does not cover the migration",
			By: by, Generation: read.Generation, Entry: entry}
	}, func(issue record.Issue) bool { return issue.Phase == phase.Implementing }},
	{"retry_or_escalate", phase.Held, func(by claim.Token, read record.Issue, _ record.StepEntry) intake.Fact {
		return intake.RetryOrEscalate{Issue: read.Key, Decision: intake.RetryDecision, Reason: "go on", By: by, Generation: read.Generation}
	}, func(issue record.Issue) bool { return issue.Phase == phase.Implementing }},
}

// requestWindow is what commits between an architect's route's read and its fact (between; nil
// for the control), and the refusal the fact then answers (want), from the record read before the
// fact, the record after it and the run of a step that stands after it.
type requestWindow struct {
	name    string
	between func(t *testing.T, pool *pgxpool.Pool, engine *Engine)
	want    func(read, now record.Issue, nowEntry record.StepEntry, fact intake.Fact) *intake.Refusal
}

// The routes of sign_off, request_backward_move and retry_or_escalate judge their requester on
// reads of the record taken before the fact lock (api's architectForIssue and architectOwns), and
// read the issue's generation, and for accepting the run of its step. Between that read and the
// fact an adoption can take the sub-architect's own issue out of the tree, a person can move the
// issue under a sibling's sub-architect, a person's todo can re-enter the issue at its next
// generation, and a move back can send the work back so that accepting starts again. The fact
// judges each again on the record it reads (Engine.Apply) and refuses, unspent, with nothing moved:
// ISSUE_OUTSIDE_TREE, ISSUE_NOT_OWNED, the request's own STALE_GENERATION, and for accepting its
// STALE_ENTRY. Each control is the same request with no change between the read and the fact, and
// it moves the issue.
func TestAnArchitectsRequestIsJudgedOnTheRecordItsFactReads(t *testing.T) {
	t.Parallel()
	for _, request := range architectsRequests {
		windows := []requestWindow{
			{"control: the request of the record its route read", nil, nil},
			{"an adoption took the sub-architect's own issue out of the tree", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
				adoptInto(t, pool, engine, "LEGION-209", otherRoot(t, pool))
			}, func(read, _ record.Issue, _ record.StepEntry, _ intake.Fact) *intake.Refusal {
				return intake.IssueOutsideTree(read.Key, subArchitect(t, "LEGION-209"), request.name)
			}},
			{"a person's todo re-entered the issue at its next generation and its work came back to the same place", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
				personsTodoOnChild(t, pool, engine, "LEGION-211", "LEGION-209")
				walkTargetTo(t, pool, engine, "LEGION-211", request.at)
			}, func(_, now record.Issue, _ record.StepEntry, fact intake.Fact) *intake.Refusal {
				return fact.(intake.ArchitectRequest).StaleGeneration(now.Generation)
			}},
		}
		if request.at == phase.Accepting {
			windows = append(windows, requestWindow{"a move back sent the work back and accepting started again", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
				move(t, pool, engine, "LEGION-211", TriggerBackward, phase.Implementing, record.PhaseRow{})
				move(t, pool, engine, "LEGION-211", TriggerImplementationReady, "", record.PhaseRow{})
			}, func(_, _ record.Issue, nowEntry record.StepEntry, fact intake.Fact) *intake.Refusal {
				return fact.(intake.EnteredRequest).StaleEntry(nowEntry)
			}})
		}
		if request.name == "sign_off" {
			windows = append(windows, requestWindow{"a person moved the issue under a sibling's sub-architect, the record as admission writes the re-parent", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
				moved := issueOf(t, pool, engine, "LEGION-211")
				sibling := "LEGION-210"
				moved.Parent = &sibling
				seedIssue(t, pool, moved)
			}, func(read, _ record.Issue, _ record.StepEntry, _ intake.Fact) *intake.Refusal {
				return intake.IssueNotOwned(request.name, read.Key, subArchitect(t, "LEGION-209"))
			}})
		}
		for _, window := range windows {
			t.Run(request.name+": "+window.name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				read := subTreeTarget(t, pool, engine, request.at)
				readEntry := openStepOf(t, pool, engine, read.Key)
				fact := request.fact(subArchitect(t, "LEGION-209"), read, readEntry)
				if window.between != nil {
					window.between(t, pool, engine)
				}
				before, standingEntry := standingOf(t, pool, engine, read.Key, true), openStepOf(t, pool, engine, read.Key)
				result := applyFacts(t, pool, engine)("request", fact)
				after := issueOf(t, pool, engine, read.Key)
				if window.between == nil {
					if result.Refusal != nil || !request.moved(after) {
						t.Fatalf("the %s = %+v, and %s stands in %s; want it taken", request.name, result.Refusal, read.Key, after.Phase)
					}
					return
				}
				want := window.want(read, before.issue, standingEntry, fact)
				if !reflect.DeepEqual(result.Refusal, want) || !result.Unspent {
					t.Fatalf("the %s = %+v (unspent %v), want the refusal %+v, unspent", request.name, result.Refusal, result.Unspent, want)
				}
				if now := standingOf(t, pool, engine, read.Key, true); !reflect.DeepEqual(now, before) {
					t.Fatalf("the refused %s changed %s:\n before %+v\n after  %+v", request.name, read.Key, before, now)
				}
			})
		}
	}
}
