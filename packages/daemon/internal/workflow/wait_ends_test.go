package workflow

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// waitEndsAt is the moment the engine of the tests below reads as now, half an hour after every
// wait was declared (waitNow): an end the workflow writes is written at it.
var waitEndsAt = waitNow.Add(30 * time.Minute)

// The tree every end below stands on: the root and the children it runs.
const waitRoot, waitChild, waitSibling = "LEGION-208", "LEGION-209", "LEGION-210"

// waitRig is a workflow over a database of its own that applies every fact as the daemon does:
// the engine, then admission (a stub here, which writes nothing), then the wait reconciler last
// (internal/daemon's factHandlers). The engine logs to logs.
type waitRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	engine *Engine
	logs   *testlog.Buffer
}

// newWaitRig is a rig whose deployment gates steps.
func newWaitRig(t *testing.T, steps []record.DesignStep) *waitRig {
	t.Helper()
	r := &waitRig{t: t, pool: migratedPool(t), logs: &testlog.Buffer{}}
	r.engine = r.engineGating(steps)
	return r
}

// engineGating is an engine over the rig's database and log whose deployment gates steps, the
// engine a daemon restarted under another `gates.design` runs.
func (r *waitRig) engineGating(steps []record.DesignStep) *Engine {
	return New(record.NewStore(), Config{Project: "LEGION", Steps: steps, ReviewRoundCap: 3, Linger: time.Hour,
		ProductionCheckRole: claim.RoleTester, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock: func() time.Time { return waitEndsAt }}, slog.New(slog.NewTextHandler(r.logs, nil)))
}

// apply applies fact as source's event id through the rig's handlers, failing the test on an
// error, and answers its result.
func (r *waitRig) apply(source, id string, fact intake.Fact) intake.Result {
	r.t.Helper()
	return r.applyWith(r.engine, source, id, fact)
}

// applyWith is apply through engine's handlers.
func (r *waitRig) applyWith(engine *Engine, source, id string, fact intake.Fact) intake.Result {
	r.t.Helper()
	result, err := intake.ApplyFact(context.Background(), r.pool, source, id, fact, engine, admissionStub{}, NewWaitReconciler(engine))
	if err != nil {
		r.t.Fatalf("apply %s/%s: %v", source, id, err)
	}
	return result
}

// tree records the root running at at, generation 1, under an open design gate, with its slot.
func (r *waitRig) tree(at phase.Phase) {
	r.t.Helper()
	r.issue(waitRoot, at)
	seedGate(r.t, r.pool, record.DesignGate{Issue: waitRoot, ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedSlot(r.t, r.pool, record.Slot{Issue: waitRoot, Index: 0, AdmittedAt: waitNow})
}

// issue records key in the root's tree at at, generation 1, with at's own status; a key other
// than the root's is a child of the root.
func (r *waitRig) issue(key string, at phase.Phase) {
	r.t.Helper()
	issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: waitRoot,
		Project: "LEGION", Title: key, Phase: at, Generation: 1, Status: PhaseStatus(at), Rank: "U", HandedOver: true, LastDispatchSeq: 1}
	if key != waitRoot {
		issue.Parent = new(waitRoot)
	}
	seedIssue(r.t, r.pool, issue)
}

// waiting declares a wait of role's worker on issue as the API does (intake.WaitDeclared): its
// claim live in session ses-<role>, its phases row on entry 2, and the declaration applied, failing
// the test on a refusal. It answers the wait recorded.
func (r *waitRig) waiting(issue string, role claim.Role) record.ClaimWait {
	r.t.Helper()
	token := waitToken(r.t, issue, role)
	if _, err := r.pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session,
		session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', $2, $3, $4, 1, $5, '', 'working', 0, 0, 0, 0)`,
		string(token), waitRoot, issue, string(role), "ses-"+string(role)); err != nil {
		r.t.Fatalf("seed the claim of %s's %s: %v", issue, role, err)
	}
	seedPhase(r.t, r.pool, record.PhaseRow{Issue: issue, Role: role, Claim: token, Entry: 2})
	id := "wait-" + strings.ToLower(issue) + "-" + string(role)
	if result := r.apply("api", "wait-declared:"+id, intake.WaitDeclared{Token: token, Issue: issue, Role: role,
		Session: "ses-" + string(role), Generation: 1, WaitID: id, WaitingOn: "the run on the branch's head",
		WakeAt: waitNow.Add(2 * time.Hour), TakenEntry: 2, At: waitNow}); result.Refusal != nil {
		r.t.Fatalf("the declaration of %s was refused: %+v", id, *result.Refusal)
	}
	return r.wait(id)
}

// wait is the wait id as claim_waits holds it, ended or not.
func (r *waitRig) wait(id string) record.ClaimWait {
	r.t.Helper()
	wait := record.ClaimWait{ID: id}
	var ended *time.Time
	if err := r.pool.QueryRow(context.Background(), `select project, claim_token, issue, role, generation, phase,
		phase_entry, taken_entry, session, waiting_on, wake_at, declared_at, ended_at, coalesce(end_cause, ''),
		coalesce(ended_by, '') from claim_waits where id = $1`, id).Scan(&wait.Project, &wait.Token, &wait.Issue,
		&wait.Role, &wait.Generation, &wait.Phase, &wait.PhaseEntry, &wait.TakenEntry, &wait.Session, &wait.WaitingOn,
		&wait.WakeAt, &wait.DeclaredAt, &ended, &wait.EndCause, &wait.EndedBy); err != nil {
		r.t.Fatalf("read wait %s: %v", id, err)
	}
	if ended != nil {
		wait.EndedAt = *ended
	}
	return wait
}

// stands is what WaitStands answers for wait on the record as it stands now, read as every reader
// of a wait reads it: the issue, its root, the role's phases row, the claim and the tree's gate
// under the steps engine gates.
func (r *waitRig) stands(engine *Engine, wait record.ClaimWait) bool {
	r.t.Helper()
	var stands bool
	seedRecord(r.t, r.pool, func(tx pgx.Tx) error {
		ctx, store := context.Background(), record.NewStore()
		issue, err := store.Issue(ctx, tx, wait.Issue)
		if err != nil {
			return err
		}
		root, err := store.Issue(ctx, tx, issue.Tree)
		if err != nil {
			return err
		}
		rows, err := store.Phases(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		gates, err := store.GatesOf(ctx, tx, issue.Tree)
		if err != nil {
			return err
		}
		claims, err := store.RoleClaims(ctx, tx, []claim.Token{wait.Token})
		if err != nil {
			return err
		}
		_, stands = WaitStands(*issue, *root, record.PhaseRowOf(rows, wait.Role), record.RoleRun{Claim: claims[wait.Token]},
			record.TreeGate{Steps: engine.cfg.Steps, Rows: gates})
		return nil
	})
	return stands
}

// assertEnded asserts wait ended with cause, ended by endedBy at the engine's now, and the one
// `wait: ended` line of it names the claim, the issue, the role, the wait, the cause and endedBy.
// Once ended, the wait no longer stands, so every reader of it acts on the claim as before it was
// declared.
func (r *waitRig) assertEnded(wait record.ClaimWait, cause, endedBy string) {
	r.t.Helper()
	ended := r.wait(wait.ID)
	if ended.EndCause != cause || ended.EndedBy != endedBy || !ended.EndedAt.Equal(waitEndsAt) {
		r.t.Fatalf("wait %s ended with cause %q by %q at %s, want cause %q by %q at %s", wait.ID, ended.EndCause,
			ended.EndedBy, ended.EndedAt, cause, endedBy, waitEndsAt)
	}
	line := r.endedLine(wait.ID)
	for _, field := range [][2]string{{"claim", string(wait.Token)}, {"issue", wait.Issue}, {"role", string(wait.Role)},
		{"wait", wait.ID}, {"cause", cause}, {"ended_by", endedBy}} {
		if want := loggedField(field[0], field[1]); !carries(line, want) {
			r.t.Fatalf("the line %q does not carry %s", line, want)
		}
	}
	if r.stands(r.engine, wait) {
		r.t.Fatalf("wait %s still stands after its end", wait.ID)
	}
}

// assertStanding asserts wait has no end, no end of it was logged, and it stands.
func (r *waitRig) assertStanding(engine *Engine, wait record.ClaimWait) {
	r.t.Helper()
	if ended := r.wait(wait.ID); !ended.EndedAt.IsZero() || ended.EndCause != "" {
		r.t.Fatalf("wait %s ended with cause %q by %q, want it standing", wait.ID, ended.EndCause, ended.EndedBy)
	}
	if line := r.endedLineOrNone(wait.ID); line != "" {
		r.t.Fatalf("an end of wait %s was logged: %s", wait.ID, line)
	}
	if !r.stands(engine, wait) {
		r.t.Fatalf("wait %s does not stand", wait.ID)
	}
}

// endedLine is the one `wait: ended` line of wait id, failing the test when there is none.
func (r *waitRig) endedLine(id string) string {
	r.t.Helper()
	line := r.endedLineOrNone(id)
	if line == "" {
		r.t.Fatalf("no end of wait %s was logged; the log is:\n%s", id, r.logs.String())
	}
	return line
}

// endedLineOrNone is the one `wait: ended` line of wait id, "" for none, failing the test on two.
func (r *waitRig) endedLineOrNone(id string) string {
	r.t.Helper()
	var found []string
	for line := range strings.SplitSeq(r.logs.String(), "\n") {
		if strings.Contains(line, `msg="wait: ended"`) && carries(line, loggedField("wait", id)) {
			found = append(found, line)
		}
	}
	if len(found) > 1 {
		r.t.Fatalf("the end of wait %s was logged %d times: %q", id, len(found), found)
	}
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

// loggedField is key=value as the text handler writes it: a value with a space, an equals sign or
// a quote is quoted.
func loggedField(key, value string) string {
	if value == "" || strings.ContainsAny(value, " =\"") {
		value = strconv.Quote(value)
	}
	return key + "=" + value
}

// carries says whether line holds field whole: the text handler writes a space before every field
// and none after the last.
func carries(line, field string) bool { return strings.Contains(line+" ", " "+field+" ") }

// waitToken is the claim token of issue's role.
func waitToken(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("claim token of %s's %s: %v", issue, role, err)
	}
	return token
}

// A wait ends in the transaction of the fact that stops its claim working its issue's open phase,
// or that finds the claim running a fresh session (dispatch://INFRA-2040 R3, its fourth and fifth
// ends): the reconciler, last among the fact's handlers, reads what the fact wrote and ends the
// wait with the cause of the first term that fails (WaitEndCause), ended by the fact's source and
// event id at the engine's now, and logs the end. Each fact below is the one way to each end:
//   - a completion that moves the issue, and one that leaves it in its phase, as a reviewer's
//     completion does while its round waits on a review: the completion is the cause either way;
//   - a backward move, a change of phase (GitHub merging the merger's pull request), and a move to
//     held (Engine.claimFailed), each named by the phase it moved to;
//   - the issue leaving the workflow, which a person's backlog does by moving a child to done;
//   - the tree's linger, a person's close of the root, which holds the child where it stands;
//   - a fresh session of the claim, which the claim's relaunch writes outside any fact, and which
//     the project's next fact (the new session taking its task) finds and ends.
//
// Once ended the wait no longer stands, so the silence sweep and the backstop read the claim as
// they did before it declared one.
func TestAWaitEndsInTheFactThatStopsItsClaimWorkingItsPhase(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed records the tree, declares the wait the fact ends, and answers it.
		seed func(r *waitRig) record.ClaimWait
		// outside is what is written of the claim outside any fact before the fact applies; nil
		// for nothing.
		outside    func(r *waitRig, wait record.ClaimWait)
		source, id string
		fact       func(t *testing.T) intake.Fact
		cause      string
		// phase is where the wait's issue stands after the fact.
		phase phase.Phase
	}{
		{
			name: "a completion that moves the issue",
			seed: func(r *waitRig) record.ClaimWait {
				r.tree(phase.Testing)
				return r.waiting(waitRoot, claim.RoleTester)
			},
			source: "api", id: "handoff:tester-pass",
			fact: func(t *testing.T) intake.Fact {
				return intake.HandoffComplete{Generation: 1, Issue: waitRoot, Role: claim.RoleTester, Claim: waitToken(t, waitRoot, claim.RoleTester),
					Verdict: "pass", HandoffRecord: "test-1", HandoffVersion: 1}
			},
			cause: WaitEndCompleted, phase: phase.Reviewing,
		},
		{
			name: "a completion that leaves the issue in its phase",
			seed: func(r *waitRig) record.ClaimWait {
				r.tree(phase.Reviewing)
				seedPR(r.t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: waitRoot, Repo: "sjawhar/legion", Number: 42,
					Branch: "legion/LEGION-208", HeadSHA: headOne, CheckedHead: headOne, Failing: []string{}})
				return r.waiting(waitRoot, claim.RoleReviewer)
			},
			source: "api", id: "handoff:reviewer-round",
			fact: func(t *testing.T) intake.Fact {
				return intake.HandoffComplete{Generation: 1, Issue: waitRoot, Role: claim.RoleReviewer, Claim: waitToken(t, waitRoot, claim.RoleReviewer),
					Summary: "reviewed", HandoffRecord: "review-1", HandoffVersion: 1}
			},
			cause: WaitEndCompleted, phase: phase.Reviewing,
		},
		{
			name: "a backward move",
			seed: func(r *waitRig) record.ClaimWait {
				r.tree(phase.Testing)
				return r.waiting(waitRoot, claim.RoleTester)
			},
			source: "api", id: "backward:testing-implementing",
			fact: func(*testing.T) intake.Fact {
				return intake.BackwardMove{Issue: waitRoot, Requester: claim.RoleTester, To: phase.Implementing,
					Reason: "the branch fails the test it adds"}
			},
			cause: WaitEndPhaseMoved(phase.Implementing), phase: phase.Implementing,
		},
		{
			name: "a change of phase",
			seed: func(r *waitRig) record.ClaimWait {
				r.tree(phase.Merging)
				seedPR(r.t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: waitRoot, Repo: "sjawhar/legion", Number: 42,
					Branch: "legion/LEGION-208", HeadSHA: headOne, Failing: []string{}})
				return r.waiting(waitRoot, claim.RoleMerger)
			},
			source: "github", id: "merged-42",
			fact: func(*testing.T) intake.Fact {
				return intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}
			},
			cause: WaitEndPhaseMoved(phase.Done), phase: phase.Done,
		},
		{
			name: "a move to held",
			seed: func(r *waitRig) record.ClaimWait {
				r.tree(phase.Testing)
				return r.waiting(waitRoot, claim.RoleTester)
			},
			source: "supervise", id: "claim-failed:tester",
			fact: func(*testing.T) intake.Fact {
				return intake.ClaimFailed{Issue: waitRoot, Role: claim.RoleTester, Reason: "the prompt budget ran out"}
			},
			cause: WaitEndPhaseMoved(phase.Held), phase: phase.Held,
		},
		{
			name: "the issue leaving the workflow",
			seed: func(r *waitRig) record.ClaimWait {
				r.tree(phase.Planning)
				r.issue(waitChild, phase.Testing)
				return r.waiting(waitChild, claim.RoleTester)
			},
			source: "dispatch", id: "child-backlog",
			fact: func(*testing.T) intake.Fact {
				return intake.DispatchIssue{Key: waitChild, Seq: 2, Type: "issue.updated", Status: "backlog", Title: waitChild,
					Parent: waitRoot, Rank: "U", HandedOver: true}
			},
			cause: WaitEndPhaseMoved(phase.Done), phase: phase.Done,
		},
		{
			name: "the tree's linger",
			seed: func(r *waitRig) record.ClaimWait {
				r.tree(phase.Planning)
				r.issue(waitChild, phase.Testing)
				return r.waiting(waitChild, claim.RoleTester)
			},
			source: "dispatch", id: "root-done",
			fact: func(*testing.T) intake.Fact {
				return intake.DispatchIssue{Key: waitRoot, Seq: 2, Type: "issue.updated", Status: "done", Title: waitRoot,
					Rank: "U", HandedOver: true}
			},
			cause: WaitEndTreeLingers, phase: phase.Testing,
		},
		{
			name: "a fresh session of the claim",
			seed: func(r *waitRig) record.ClaimWait {
				r.tree(phase.Testing)
				return r.waiting(waitRoot, claim.RoleTester)
			},
			// The claim's relaunch in a new session writes the session it runs outside any fact
			// (store's putClaim); the new session taking its task is the next fact.
			outside: func(r *waitRig, wait record.ClaimWait) {
				if _, err := r.pool.Exec(context.Background(), `update claims set session = 'ses-tester-fresh' where token = $1`,
					string(wait.Token)); err != nil {
					r.t.Fatalf("relaunch the tester in a fresh session: %v", err)
				}
			},
			source: "supervise", id: "task-confirmed:tester:d-2",
			fact: func(*testing.T) intake.Fact {
				return intake.TaskConfirmed{Issue: waitRoot, Role: claim.RoleTester, Generation: 1}
			},
			cause: WaitEndFreshSession, phase: phase.Testing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitRig(t, config.DesignGateRootIssues.Steps())
			wait := tc.seed(r)
			r.assertStanding(r.engine, wait)
			if tc.outside != nil {
				// A write outside a fact ends nothing, though the wait stops standing the moment it
				// commits (WaitStands): its end is the next fact's to write.
				tc.outside(r, wait)
				if ended := r.wait(wait.ID); !ended.EndedAt.IsZero() {
					t.Fatalf("wait %s ended with cause %q before any fact applied", wait.ID, ended.EndCause)
				}
				if r.stands(r.engine, wait) {
					t.Fatalf("wait %s still stands after what was written of its claim outside a fact", wait.ID)
				}
			}

			if result := r.apply(tc.source, tc.id, tc.fact(t)); result.Refusal != nil {
				t.Fatalf("the fact was refused: %+v", *result.Refusal)
			}
			if got := issueRecord(t, r.pool, wait.Issue).Phase; got != tc.phase {
				t.Fatalf("%s stands in %s after the fact, want %s", wait.Issue, got, tc.phase)
			}
			r.assertEnded(wait, tc.cause, tc.source+"/"+tc.id)
		})
	}
}

// A fact that changes nothing a wait's terms read — the controller's tick, a person's rename of the
// issue, the claim's own session taking a task — ends nothing: the reconciler's read keeps no row
// whose record has not moved under it, so the wait stands, and no end of it is written or logged.
func TestAFactThatChangesNothingOfAWaitLeavesItStanding(t *testing.T) {
	r := newWaitRig(t, config.DesignGateRootIssues.Steps())
	r.tree(phase.Testing)
	wait := r.waiting(waitRoot, claim.RoleTester)

	r.apply("controller", "tick-1", intake.ControllerTick{})
	r.apply("dispatch", "renamed", intake.DispatchIssue{Key: waitRoot, Seq: 2, Type: "issue.updated", Status: PhaseStatus(phase.Testing),
		Title: "a clearer title", Rank: "U", HandedOver: true})
	r.apply("supervise", "task-confirmed:tester:d-1", intake.TaskConfirmed{Issue: waitRoot, Role: claim.RoleTester, Generation: 1})

	r.assertStanding(r.engine, wait)
}

// The design gate's closing ends the wait of every merger of the tree it now holds, in the
// closing fact's own transaction, with the cause that the design gate closed (R3's seventh end),
// ended by that fact at the engine's now, each end logged. A tree holds a merger for each member
// in merging, and both mergers' waits end; the tester of a third member, which the gate holds
// not, still waits.
func TestTheDesignGatesClosingEndsTheWaitOfEveryMergerItHolds(t *testing.T) {
	r := newWaitRig(t, config.DesignGateRootIssues.Steps())
	r.tree(phase.Planning)
	r.issue(waitChild, phase.Merging)
	r.issue(waitSibling, phase.Merging)
	r.issue("LEGION-211", phase.Testing)
	mergers := []record.ClaimWait{r.waiting(waitChild, claim.RoleMerger), r.waiting(waitSibling, claim.RoleMerger)}
	tester := r.waiting("LEGION-211", claim.RoleTester)

	r.apply("dispatch", "spec-4", intake.DispatchArtifact{Key: waitRoot, ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactVersion, Version: 4})

	if gate := gateState(t, r.pool, waitRoot); gate.LatestVersion != 4 || gate.ApprovedVersion == nil || *gate.ApprovedVersion != 3 {
		t.Fatalf("the spec's gate = %+v after its version 4, want it closed at 4 with 3 approved", gate)
	}
	for _, merger := range mergers {
		r.assertEnded(merger, WaitEndDesignGateClosed, "dispatch/spec-4")
	}
	r.assertStanding(r.engine, tester)
}

// A merger the design gate holds with no closing of it — the steps `gates.design` names changed
// across a restart, so a gate row that held nothing holds the merger now — no longer has a wait
// that stands, the moment the daemon reads the gate under the new steps (WaitStands). The
// reconciler does not end it under the name of the next fact, which did not close the gate: it
// judges no wait by the gate, and leaves that wait for its time, which ends it with the cause the
// rule names then, ended by the time's own fact, and writes no task (waitDue): the worker was
// waiting on nothing its time brings, and the gate holds its merge.
func TestAWaitTheGateHoldsWithNoClosingIsLeftForItsTime(t *testing.T) {
	r := newWaitRig(t, config.DesignGateOff.Steps())
	r.issue(waitRoot, phase.Merging)
	seedGate(r.t, r.pool, record.DesignGate{Issue: waitRoot, ArtifactID: "artifact-208", LatestVersion: 4})
	seedSlot(r.t, r.pool, record.Slot{Issue: waitRoot, Index: 0, AdmittedAt: waitNow})
	wait := r.waiting(waitRoot, claim.RoleMerger)

	gated := r.engineGating(config.DesignGateRootIssues.Steps())
	r.applyWith(gated, "supervise", "task-confirmed:merger:d-1", intake.TaskConfirmed{Issue: waitRoot, Role: claim.RoleMerger, Generation: 1})

	if ended := r.wait(wait.ID); !ended.EndedAt.IsZero() {
		t.Fatalf("wait %s ended with cause %q by %q, want no end: no fact closed the gate", wait.ID, ended.EndCause, ended.EndedBy)
	}
	if line := r.endedLineOrNone(wait.ID); line != "" {
		t.Fatalf("an end of wait %s was logged: %s", wait.ID, line)
	}
	if r.stands(gated, wait) {
		t.Fatalf("wait %s stands under a gate that holds its merger", wait.ID)
	}

	starts := len(wakeStarts(t, r.pool))
	r.applyWith(gated, "outbox", "wait-due:"+wait.ID, intake.WaitDue{Token: wait.Token, Issue: wait.Issue, WaitID: wait.ID})
	r.assertEnded(wait, WaitEndDesignGateClosed, "outbox/wait-due:"+wait.ID)
	if after := wakeStarts(t, r.pool); len(after) != starts {
		t.Fatalf("the time of a wait the gate holds wrote the starts %+v, want none: no task follows an end the gate made", after[starts:])
	}
	if lines := waitTimeLines(r.logs); len(lines) != 0 {
		t.Fatalf("the time of a wait the gate holds was logged as a time that came: %q", lines)
	}
}
