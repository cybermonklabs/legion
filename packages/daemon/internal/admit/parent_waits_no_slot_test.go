package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"github.com/sjawhar/legion/daemon/internal/reviewround"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// pnForwardMerge is the words of the forward-merge request no task of a parent carries: merge the
// default branch forward, push, and complete. The return asks for no forward merge
// (dispatch://INFRA-3320 R12).
const pnForwardMerge = "merge the default branch main into the branch, push, and complete with a new handoff record"

// pnBaseMerge is the sentence the testing task of a parent carries once, and the testing task of
// an issue with no Dispatch child carries not at all: the tester's rule for every round of a
// parent (dispatch://INFRA-3320 R12 and R14), where a parent is an issue with a Dispatch child of
// any status in the daemon's copy. The start of a phase worker of a parent in testing ends the
// task it composed with it (workflow's withBaseMergeRun), the phase's resume task among them
// (workflow's Engine.ResumeTask); workflow's task does not add it.
const pnBaseMerge = "Test the head merged with the tip of the pull request's base branch, read at the round's start once that tip carries each needed child's merge, in a checkout of your own outside the issue's workspace that you never push; record each result at the head with an `--observed` line naming that base commit, a carried pass too; size the round over the whole tested tree's difference."

// pnMergedChildren is the words with which the task a return starts names the children that
// merged during the wait, each as pnMerged names it, in the order the wait recorded them
// (record.ChildrenWait.Children): every child the wait was on whose record stands at done with its
// pull request merged, and no other (dispatch://INFRA-3320 R12).
func pnMergedChildren(parent string, merged ...string) string {
	return "These Dispatch children of " + parent + " merged during the wait: " + strings.Join(merged, ", ")
}

// pnMerged is child named with its pull request, as pnMergedChildren lists it.
func pnMerged(child string, number int) string {
	return fmt.Sprintf("%s (pull request #%d)", child, number)
}

// pnChildMerged records child, a child of parent in parent's tree, as a child that ran and merged
// leaves the daemon's record and Dispatch: its record at done, its pull request number merged, and
// Dispatch showing it done at sequence 2.
func pnChildMerged(t *testing.T, pool *pgxpool.Pool, child, parent string, number int) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: child, Tree: parent, Parent: record.ParentOf(parent), Phase: phase.Done, Generation: 1,
		Status: "done", DispatchStatus: "done", Rank: child, HandedOver: true, LastDispatchSeq: 2})
	seedPullRequest(t, pool, child, number, record.PullRequestMerged)
	spMirror(t, pool, child, "done", parent, 2)
}

// pnAsksNoForwardMerge fails unless task carries neither the forward-merge request (pnForwardMerge)
// nor the line that asks for a full test round after one (spMergeLine).
func pnAsksNoForwardMerge(t *testing.T, what, task string) {
	t.Helper()
	if strings.Contains(task, pnForwardMerge) || strings.Contains(task, spMergeLine) {
		t.Fatalf("%s = %q, want it to ask for no forward merge and to carry no %s", what, task, spMergeLine)
	}
}

// pnBaseMergeTimes fails unless task carries the base-merge sentence want times: once for a
// testing task of a parent, none for an issue with no child.
func pnBaseMergeTimes(t *testing.T, what, task string, want int) {
	t.Helper()
	if got := strings.Count(task, pnBaseMerge); got != want {
		t.Fatalf("%s = %q, which carries the base-merge sentence %d times; want %d: %q", what, task, got, want, pnBaseMerge)
	}
}

// pnAdmission is the admission pass of these cases, quiet as quiet builds one, over a workflow
// engine of the test project that holds the daemon's pod tables (workflow.Config.Pods, the
// store's PodRecordsOf), as the daemon builds its engine: the move into awaiting_children cancels
// the parent's backstops through them, and a CI settlement reads the released claims it would wake
// through them. Each call opens a store of its own on pool and builds a new engine and a new
// admission, so a second call over the same pool is the daemon a restart builds. The facts apply
// through this engine and the pass moves parents through it, as in the daemon.
func pnAdmission(t *testing.T, pool *pgxpool.Pool, cap int) *Admission {
	t.Helper()
	pods, err := claimStore(t, pool).PodRecordsOf(testProject)
	if err != nil {
		t.Fatalf("the pod records of %s: %v", testProject, err)
	}
	admission := quiet(t, cap)
	admission.engine = workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Clock: func() time.Time { return fixedNow }, Pods: pods}, nil)
	return admission
}

// pnLingering is a workflow engine of the test project under a one-hour linger, as the daemon's
// engine applies a person's or a hand-over's Dispatch event to a tree that lingers.
func pnLingering() *workflow.Engine {
	return workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// pnArmBackstop arms the backstop an earlier release of key's pod armed for role, due at due.
func pnArmBackstop(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role, due time.Time) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	if _, err := pool.Exec(context.Background(), `insert into pod_wake_backstops (project, issue, role, release_id, due_at)
		values ($1, $2, $3, 0, $4)`, project, key, string(role), due); err != nil {
		t.Fatalf("arm the %s backstop of %s: %v", role, key, err)
	}
}

// pnOpenBackstops is how many backstops of key's pod are armed and not fired.
func pnOpenBackstops(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var open int
	if err := pool.QueryRow(context.Background(), `select count(*) from pod_wake_backstops where issue = $1 and not fired`,
		key).Scan(&open); err != nil {
		t.Fatalf("count the open backstops of %s: %v", key, err)
	}
	return open
}

// pnReleasePod suspends every claim of key with the source `released`, as the idle release of
// its pod leaves them: the claims a wake resumes.
func pnReleasePod(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update claims set state = $2, suspended_by = $3 where issue = $1`,
		key, string(supervise.StateSuspended), string(supervise.SuspendedByReleased)); err != nil {
		t.Fatalf("release the pod of %s: %v", key, err)
	}
}

// pnResumes is how many pod wakes were taken for any role of key (a pod_resumes row each).
func pnResumes(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var resumes int
	if err := pool.QueryRow(context.Background(), `select count(*) from pod_resumes where issue = $1`, key).Scan(&resumes); err != nil {
		t.Fatalf("count the pod wakes of %s: %v", key, err)
	}
	return resumes
}

// pnSession is the session token's claim records.
func pnSession(t *testing.T, pool *pgxpool.Pool, token claim.Token) string {
	t.Helper()
	var session string
	if err := pool.QueryRow(context.Background(), `select session from claims where token = $1`, string(token)).Scan(&session); err != nil {
		t.Fatalf("read the session of %s: %v", token, err)
	}
	return session
}

// pnState is the state `legion state` shows of the test project now (projection.Project over the
// stored records), under the engine's own design-gate steps.
func pnState(t *testing.T, pool *pgxpool.Pool, admission *Admission) api.State {
	t.Helper()
	var state api.State
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if state, err = projection.Project(context.Background(), projection.Inputs{Tx: tx, Store: record.NewStore(), Project: testProject,
			Steps: admission.engine.GatedSteps(), Now: fixedNow}); err != nil {
			t.Fatalf("project the state: %v", err)
		}
	})
	return state
}

// pnBlocked is key's row of `admission.blocked`: what it waits on, and whether the list holds it.
func pnBlocked(state api.State, key string) ([]string, bool) {
	for _, row := range state.Admission.Blocked {
		if row.Issue == key {
			return row.BlockedBy, true
		}
	}
	return nil, false
}

// pnHeldRow is key's row of `admission.held`, and whether the list holds it.
func pnHeldRow(state api.State, key string) (api.HeldIssue, bool) {
	for _, row := range state.Admission.Held {
		if row.Issue == key {
			return row, true
		}
	}
	return api.HeldIssue{}, false
}

// pnInNeitherList fails unless key stands in neither `admission.active` nor `admission.waiting`.
func pnInNeitherList(t *testing.T, state api.State, key string) {
	t.Helper()
	if slices.Contains(state.Admission.Active, key) || slices.Contains(state.Admission.Waiting, key) {
		t.Fatalf("admission.active = %v, admission.waiting = %v; want %s in neither", state.Admission.Active, state.Admission.Waiting, key)
	}
}

// pnSlotted is the keys holding the cap's slots, sorted.
func pnSlotted(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var keys []string
	for _, slot := range slots(t, pool) {
		keys = append(keys, slot.Issue)
	}
	slices.Sort(keys)
	return keys
}

// pnWorkerStart is the newest start of key's role that carries a task, as the outbox holds it.
func pnWorkerStart(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) record.SuperviseRequest {
	t.Helper()
	var found *record.SuperviseRequest
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == key && start.Op == "start" && start.Role == role && start.Task != "" {
			found = &start
		}
	}
	if found == nil {
		t.Fatalf("no start of the %s of %s carries a task; outbox %+v", role, key, effects(t, pool))
	}
	return *found
}

// pnMovedParent is a live tree whose root, the parent, stands in from holding the slot that phase
// keeps, with its pull request #42 open and one child filed and not released, and seed for what
// else the case needs; fact, applied from source, is the move the child holds. It fails unless the
// parent waits in awaiting_children holding the start of start for cause, and answers the pool,
// the admission and the held start.
func pnMovedParent(t *testing.T, from phase.Phase, seed func(*testing.T, *pgxpool.Pool), source string, fact intake.Fact,
	start phase.Phase, cause string,
) (*pgxpool.Pool, *Admission, record.ChildrenWait) {
	t.Helper()
	pool := migratedPool(t)
	admission, wait := pnMoveParent(t, pool, from, seed, source, fact, start, cause)
	return pool, admission, wait
}

// pnMoveParent is pnMovedParent on a pool the caller holds.
func pnMoveParent(t *testing.T, pool *pgxpool.Pool, from phase.Phase, seed func(*testing.T, *pgxpool.Pool), source string, fact intake.Fact,
	start phase.Phase, cause string,
) (*Admission, record.ChildrenWait) {
	t.Helper()
	admission := pnAdmission(t, pool, 2)
	spSeedParent(t, pool, spParent, from, record.PhaseKeepsSlot(from), nil)
	pwPullRequest(t, pool, spParent, record.PullRequestOpen)
	seedMirrorChild(t, pool, spChild, "todo", spParent)
	if seed != nil {
		seed(t, pool)
	}
	pwApplied(t, pool, admission, source, "move", fact)
	return admission, pwHeld(t, pool, spParent, start, cause, spChild+" (todo)")
}

// pnChildDone takes the parent's one child to done with its pull request #51 merged, as a child
// that ran and merged leaves its record and Dispatch (pnChildMerged), runs the next pass, and
// answers the parent's record after it.
func pnChildDone(t *testing.T, pool *pgxpool.Pool, admission *Admission) record.Issue {
	t.Helper()
	pnChildMerged(t, pool, spChild, spParent, 51)
	spPass(t, pool, admission)
	return issue(t, pool, spParent)
}

// pnHeldStart is one start a parent can hold in awaiting_children, and the move of the engine that
// holds it: the phase the parent stands in, what seeds the move beyond the parent, its pull request
// and its open child, the fact that makes it, and what the held start was for.
type pnHeldStart struct {
	start  phase.Phase
	from   phase.Phase
	seed   func(*testing.T, *pgxpool.Pool)
	source string
	fact   intake.Fact
	cause  string
}

// pnArchitect is the claim of the architect of spParent's tree, which the architect's own
// requests name (intake's BackwardMove.Request and SignOff.Request read it).
func pnArchitect(t *testing.T) claim.Token {
	t.Helper()
	return pwRoleToken(t, spParent, claim.RoleArchitect)
}

// pnHeldStarts is every start a parent can hold (record.OpenChildHolds): planning, implementing,
// testing, reviewing and accepting.
func pnHeldStarts(t *testing.T) []pnHeldStart {
	t.Helper()
	const replan = "the plan missed the empty list"
	return []pnHeldStart{
		{start: phase.Planning, from: phase.Accepting, source: "api",
			fact:  intake.BackwardMove{Issue: spParent, Requester: claim.RoleArchitect, To: phase.Planning, Reason: replan, By: pnArchitect(t)},
			cause: "moved back to planning by the architect: " + replan},
		{start: phase.Implementing, from: phase.Testing, source: "api",
			fact: intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleTester, Claim: "tester", Summary: "the empty list panics",
				Verdict: "fail", Commit: "head-42", HandoffRecord: "test-1", HandoffVersion: 1},
			cause: "the tester completed testing"},
		{start: phase.Testing, from: phase.Implementing, source: "api",
			fact: intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleImplementer, Claim: "implementer",
				Summary: "implemented", Commit: "head-42", HandoffRecord: "implement-1", HandoffVersion: 1},
			cause: "the implementer completed implementing"},
		{start: phase.Reviewing, from: phase.Testing, source: "api",
			fact: intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleTester, Claim: "tester", Summary: "every check passed",
				Verdict: "pass", Commit: "head-42", HandoffRecord: "test-1", HandoffVersion: 1},
			cause: "the tester completed testing"},
		{start: phase.Accepting, from: phase.Reviewing, seed: func(t *testing.T, pool *pgxpool.Pool) { pwReviewed(t, pool) }, source: "github",
			fact: pwReview("APPROVED", "the change is right"), cause: "a review approved the head"},
	}
}

// pnReturnsAs is the phase the return takes a held start to (workflow's ReturnFromChildren): a
// held planning or implementing to that phase, a held testing, reviewing or accepting to testing,
// and every held start to itself where the pull request merged during the wait.
func pnReturnsAs(held phase.Phase, merged bool) phase.Phase {
	if merged || held == phase.Planning || held == phase.Implementing {
		return held
	}
	return phase.Testing
}

// pnReturnsWhereItTakes reads one row of the inventory of held starts: the parent holds held's
// start, its pull request open or, where merged, merged during the wait, and its one child reaches
// done with its own pull request #51 merged. The return writes a new entry raised as a move back
// out of awaiting_children for the role it starts; with the pull request open a held planning or
// implementing starts that phase on the held task, which carries the reason its transition wrote,
// and a held testing, reviewing or accepting starts testing with a task that names the held start
// and its reason and carries the base-merge sentence once; each of those names the child that
// merged with its pull request. With the pull request merged the parent comes back at the start it
// held, on the held task, and a held accepting starts no role, tells the architect under the wait's
// own notice kind and raises no entry. No task asks for a forward merge.
func pnReturnsWhereItTakes(t *testing.T, held pnHeldStart, merged bool) {
	pool, admission, wait := pnMovedParent(t, held.from, held.seed, held.source, held.fact, held.start, held.cause)
	if merged {
		pwApplied(t, pool, admission, "github", "merged", intake.PullRequestMerged{Repo: "acme/widgets", Number: 42, MergeSHA: "merge-42"})
		if got := issue(t, pool, spParent); got.Phase != phase.AwaitingChildren {
			t.Fatalf("the parent after its merge = %s, want awaiting_children still", got.Phase)
		}
	}
	rows := map[claim.Role]record.PhaseRow{}
	for _, role := range claim.Roles {
		rows[role] = spPhaseRow(t, pool, spParent, role)
	}

	back := pnChildDone(t, pool, admission)

	to := pnReturnsAs(held.start, merged)
	if back.Phase != to || back.ChildrenWait != nil {
		t.Fatalf("the parent after its child is done = phase %s, held start %+v; want %s with nothing held", back.Phase, back.ChildrenWait, to)
	}
	role := phase.RoleFor(to)
	starts := pwWorkerStarts(t, pool, spParent)
	if role == claim.RoleArchitect {
		if len(starts) != 0 {
			t.Fatalf("starts of %s = %+v, want none: no worker starts in accepting", spParent, starts)
		}
		if told := spNotices(t, pool, spParent); len(told) != 2 || !strings.Contains(told[1].Reason, "It waits on your sign_off") {
			t.Fatalf("awaiting-children notices = %+v, want the move's and the return's, which tells the architect the issue waits on its sign_off", told)
		}
		for _, role := range claim.Roles {
			if got := spPhaseRow(t, pool, spParent, role); got.Entry != rows[role].Entry {
				t.Fatalf("the %s's phases row after the return = %+v, want it on entry %d still: a return that starts no role raises no entry", role, got, rows[role].Entry)
			}
		}
		return
	}
	if len(starts) != 1 || starts[0].role != role {
		t.Fatalf("starts of %s = %+v, want the %s's one start", spParent, starts, role)
	}
	work := starts[0].task
	pnAsksNoForwardMerge(t, "the "+string(role)+"'s task", work)
	if row := spPhaseRow(t, pool, spParent, role); row.Entry != rows[role].Entry+1 || !row.EntryMovedBack || row.EntryFrom != phase.AwaitingChildren {
		t.Fatalf("the %s's phases row after the return = %+v, want a new entry above %d raised as a move back out of awaiting_children", role, row, rows[role].Entry)
	}
	named := pnMergedChildren(spParent, pnMerged(spChild, 51))
	switch {
	case merged:
		want := 0
		if to == phase.Testing {
			want = 1
		}
		if !strings.HasPrefix(work, wait.Task) || !strings.Contains(work, named) {
			t.Fatalf("the %s's task = %q, want the held task %q followed by %q", role, work, wait.Task, named)
		}
		pnBaseMergeTimes(t, "the "+string(role)+"'s task", work, want)
	case to == held.start:
		if !strings.HasPrefix(work, wait.Task) || !strings.Contains(work, named) {
			t.Fatalf("the %s's task = %q, want the held task %q followed by %q", role, work, wait.Task, named)
		}
	default:
		for _, want := range []string{"Phase: testing.", "start of " + string(held.start), wait.Reason, named} {
			if !strings.Contains(work, want) {
				t.Fatalf("the tester's task = %q, want it to say %q", work, want)
			}
		}
		pnBaseMergeTimes(t, "the tester's task", work, 1)
	}
}

// pnParentKind is the issue a testing task is written for: a parent that waited on its children
// (its record carries the moment of its last wait, record.Issue.ChildrenWaitedAt), a parent whose
// children all finished before its first phase, and an issue with no Dispatch child. seed records
// the kind on key, whose child, where it has one, is child, done in Dispatch.
type pnParentKind struct {
	name   string
	parent bool
	seed   func(t *testing.T, pool *pgxpool.Pool, key, child string)
}

// pnParentKinds is the three kinds, the parent that waited first: it is the one kind a return is
// written for, since a return is the end of a wait.
func pnParentKinds() []pnParentKind {
	done := func(t *testing.T, pool *pgxpool.Pool, key, child string) {
		seedMirrorChild(t, pool, child, "done", key)
	}
	return []pnParentKind{
		{name: "a parent that waited on its children", parent: true, seed: func(t *testing.T, pool *pgxpool.Pool, key, child string) {
			done(t, pool, key, child)
			waited := fixedNow.Add(-3 * time.Hour)
			waiting := issue(t, pool, key)
			waiting.ChildrenWaitedAt = &waited
			putIssue(t, pool, waiting)
		}},
		{name: "a parent whose children all finished before its first phase", parent: true, seed: done},
		{name: "an issue with no child", seed: func(*testing.T, *pgxpool.Pool, string, string) {}},
	}
}

// pnTestingPath is one path that writes the task of a tester in testing: key is the issue whose
// tester it starts and child the key its Dispatch child takes where the kind gives it one; seed
// records key where the path finds it and answers the admission pass the case runs under, and move
// makes the path's move. A return is run only for a parent that waited, which its seed makes by
// the move into awaiting_children itself. A resume path sends the phase's resume task
// (workflow's Engine.ResumeTask) rather than a task of task's.
type pnTestingPath struct {
	name    string
	key     string
	child   string
	returns bool
	resume  bool
	seed    func(*testing.T, *pgxpool.Pool) *Admission
	move    func(*testing.T, *pgxpool.Pool, *Admission)
}

// pnTestingPaths is every path that writes a testing task, 12 in all. Seven reach workflow's task:
// the transition after the implementer's completion; the architect's request_backward_move from
// accepting to testing, which stands for every move back into testing, since each reaches the same
// task call of the transition; the architect's retry of a held testing; the operator's restart of
// the tester; a todo on a root held from testing; and the return of a held testing, with its pull
// request open and with it merged during the wait, whose task workflow's task wrote for the held
// phase before the wait. Five send the phase's resume task to a tester in testing: the catch-up of a
// ready claim (readyWorkerTask), the silence repair, the boot's resume, a re-admitted tree's
// promotion (StartPromotedWorker) and the admission pass's send of an entry whose row names no
// task (sendOwedEntry's resume task).
// Each is driven here through the fact or the pass the daemon drives it with: the engine handles
// the ready, the silence and the boot as facts.
func pnTestingPaths() []pnTestingPath {
	slotted := func(t *testing.T, pool *pgxpool.Pool, at phase.Phase) {
		spSeedParent(t, pool, spParent, at, record.PhaseKeepsSlot(at), nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
	}
	// testerOnItsEntry is the tester's claim in state, suspended by by, level with the entry 3 its
	// phases row names, on whose work it is.
	testerOnItsEntry := func(t *testing.T, pool *pgxpool.Pool, state supervise.ClaimState, by supervise.SuspendSource) claim.Token {
		token := putWorkerClaim(t, pool, spParent, spParent, claim.RoleTester, 1, state, by)
		markGivenEntry(t, pool, token, 3)
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleTester, Claim: token, Entry: 3,
			EntryTask: pwTask(spParent, phase.Testing), EntryRaisedAt: fixedNow.Add(-time.Hour)})
		return token
	}
	implemented := intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleImplementer, Claim: "implementer",
		Summary: "implemented", Commit: "head-42", HandoffRecord: "implement-1", HandoffVersion: 1}
	returned := func(merged bool) func(*testing.T, *pgxpool.Pool) *Admission {
		return func(t *testing.T, pool *pgxpool.Pool) *Admission {
			admission, _ := pnMoveParent(t, pool, phase.Implementing, nil, "api", implemented, phase.Testing, "the implementer completed implementing")
			if merged {
				pwApplied(t, pool, admission, "github", "merged", intake.PullRequestMerged{Repo: "acme/widgets", Number: 42, MergeSHA: "merge-42"})
			}
			return admission
		}
	}
	theChildDone := func(t *testing.T, pool *pgxpool.Pool, admission *Admission) { pnChildDone(t, pool, admission) }
	silent := fixedNow.Add(-2 * time.Hour)
	const root, member, memberChild = "LEGION-208", "LEGION-209", "LEGION-210"
	return []pnTestingPath{
		{name: "the transition after the implementer's completion", key: spParent, child: spChild,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				slotted(t, pool, phase.Implementing)
				return pwAdmission(t, 2)
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "api", "implemented", implemented)
			}},
		{name: "the architect's request_backward_move from accepting to testing", key: spParent, child: spChild,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				slotted(t, pool, phase.Accepting)
				return pwAdmission(t, 2)
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "api", "back", intake.BackwardMove{Issue: spParent, Requester: claim.RoleArchitect,
					To: phase.Testing, Reason: "the checks must run again", By: pnArchitect(t)})
			}},
		{name: "the architect's retry of a held testing", key: spParent, child: spChild, seed: pwHeldFromTesting,
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "api", "retry", intake.RetryOrEscalate{Issue: spParent, Decision: intake.RetryDecision,
					Reason: "the runner is back"})
			}},
		{name: "the operator's restart of the tester", key: spParent, child: spChild, seed: pwHeldFromTesting,
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "api", "restart", intake.RestartPhase{Issue: spParent, Role: claim.RoleTester})
			}},
		{name: "a todo on a root held from testing", key: spParent, child: spChild,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				spSeedParent(t, pool, spParent, phase.Held, true, func(held *record.Issue) {
					held.Hold, held.Status, held.DispatchStatus, held.LastDispatchSeq = &record.Hold{From: phase.Testing}, "testing", "testing", 4
				})
				pwPullRequest(t, pool, spParent, record.PullRequestOpen)
				return pwAdmission(t, 2)
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "dispatch", "todo", intake.DispatchIssue{Key: spParent, ResolvedRepo: "acme/widgets", Seq: 5,
					Type: "issue.updated", Status: "todo", Title: spParent, Rank: spParent, HandedOver: true})
			}},
		{name: "the return of a held testing with its pull request open", key: spParent, returns: true, seed: returned(false), move: theChildDone},
		{name: "the return of a held testing whose pull request merged during the wait", key: spParent, returns: true, seed: returned(true),
			move: theChildDone},
		{name: "the catch-up of the tester's ready claim", key: spParent, child: spChild, resume: true,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				slotted(t, pool, phase.Testing)
				testerOnItsEntry(t, pool, supervise.StateReady, "")
				return pwAdmission(t, 2)
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "supervisor", "ready", intake.ClaimReady{Issue: spParent, Role: claim.RoleTester, Launch: 1})
			}},
		{name: "the silence repair of the tester", key: spParent, child: spChild, resume: true,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				slotted(t, pool, phase.Testing)
				steppedAt(t, pool, testerOnItsEntry(t, pool, supervise.StateReady, ""), silent)
				return pwAdmission(t, 2)
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "supervise", "silent", intake.WorkerSilent{Issue: spParent, Role: claim.RoleTester, Generation: 1,
					LastStep: silent, Bound: silenceBound})
			}},
		{name: "the boot's resume of the tester the daemon suspended", key: spParent, child: spChild, resume: true,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				slotted(t, pool, phase.Testing)
				testerOnItsEntry(t, pool, supervise.StateSuspended, supervise.SuspendedByDaemon)
				return pwAdmission(t, 2)
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "daemon", "boot", intake.DaemonBooted{Boot: 1})
			}},
		{name: "a re-admitted tree's promotion of its tester", key: member, child: memberChild, resume: true,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
					Title: "root", Tree: root, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", LastDispatchSeq: 1})
				putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
					Title: "member", Tree: root, Parent: record.ParentOf(root), Phase: phase.Testing, Generation: 1, Status: "in_progress",
					DispatchStatus: "in_progress", Rank: "B", LastDispatchSeq: 1})
				putWorkerClaim(t, pool, root, member, claim.RoleTester, 1, supervise.StateSuspended, supervise.SuspendedByDaemon)
				return newAdmission(t, 1, nil)
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
					Status: "todo", Title: "root", Rank: "A", HandedOver: handed},
					workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Linger: time.Hour,
						Clock: func() time.Time { return fixedNow }}, nil))
			}},
		{name: "the pass's send of an entry whose phases row names no task", key: spParent, child: spChild, resume: true,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				slotted(t, pool, phase.Testing)
				spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleTester, Claim: pwRoleToken(t, spParent, claim.RoleTester),
					Entry: 3, EntryRaisedAt: fixedNow.Add(-time.Hour)})
				return pwAdmission(t, 2)
			},
			move: spPass},
	}
}

// pnAsksTheBaseMergeRun runs path for kind and reads the tester's task it wrote: the issue stands
// in testing, a resume path's task is the phase's resume task, and the task carries the base-merge
// sentence once for a parent and not at all for an issue with no child.
func pnAsksTheBaseMergeRun(t *testing.T, path pnTestingPath, kind pnParentKind) {
	pool := migratedPool(t)
	admission := path.seed(t, pool)
	if !path.returns {
		kind.seed(t, pool, path.key, path.child)
	}

	path.move(t, pool, admission)

	if got := issue(t, pool, path.key); got.Phase != phase.Testing {
		t.Fatalf("%s after %s = phase %s, want testing", path.key, path.name, got.Phase)
	}
	work := pnWorkerStart(t, pool, path.key, claim.RoleTester).Task
	if path.resume && !strings.Contains(work, "Resume the existing phase work.") {
		t.Fatalf("the tester's task after %s = %q, want the phase's resume task", path.name, work)
	}
	want := 0
	if kind.parent {
		want = 1
	}
	pnBaseMergeTimes(t, "the tester's task after "+path.name, work, want)
}

// A parent waiting on its open Dispatch children holds no admission slot and wakes no worker, keeps
// its issue's run, and comes back with nobody acting where R12 takes it: a held planning or
// implementing to that start, a held testing, reviewing or accepting to testing, every held start
// to itself where its pull request merged during the wait, with no forward merge asked and the
// children that merged named (dispatch://INFRA-3320 R11, R12, R13, and its check
// dispatch://INFRA-3320/artifact/verification-md#b-k13, of which this package proves the wait, the
// returns, the tester's rule for every round of a parent, the blocked_by order, the grandparent,
// the hand-over, the held list and the stalled measure). Every case runs through the workflow
// engine and the admission pass as the daemon runs them.
func TestParentWaitsHoldsNoSlotAndComesBack(t *testing.T) {
	// The first case is the check's own run of the wait. Its first read is what the negative
	// control fails at: with no child open, the implementer's completion moves the parent to
	// testing, which keeps its slot.
	t.Run("the wait holds no slot and wakes no worker, keeps the run through a restart, and ends with nobody acting", func(t *testing.T) {
		const (
			promoted    = "LEGION-396"
			above       = "LEGION-395"
			second      = "LEGION-310"
			secondChild = "LEGION-311"
			blocker     = "LEGION-900"
		)
		pool := migratedPool(t)
		admission := pnAdmission(t, pool, 2)
		// The cap is full: another tree's issue and the parent hold its two slots, and a root
		// waits in the line for one.
		seedStarted(t, pool, spFiller, phase.Implementing, nil)
		spSeedParent(t, pool, spParent, phase.Implementing, true, func(parent *record.Issue) {
			parent.DispatchStatus, parent.LastDispatchSeq = "in_progress", 1
		})
		seedWaiting(t, pool, promoted, "B")
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		// The plan record the planner completed with, and the implementer at work on its third
		// entry after one review round and one move back, its tester's claim beside it.
		plan := record.PhaseRow{Issue: spParent, Role: claim.RolePlanner, Claim: "planner", Summary: "planned", HandoffCommit: "head-41",
			HandoffRecord: "plan-1", HandoffVersion: 1, LastHandoff: "plan-1", CompletedAt: fixedNow.Add(-4 * time.Hour), Entry: 1,
			EntryTask: "Continue LEGION-300. Issue: LEGION-300. Phase: planning.", EntryRaisedAt: fixedNow.Add(-5 * time.Hour)}
		spSeedPhase(t, pool, plan)
		implementer := putWorkerClaim(t, pool, spParent, spParent, claim.RoleImplementer, 1, supervise.StateIdle, "")
		markGivenEntry(t, pool, implementer, 3)
		tester := putWorkerClaim(t, pool, spParent, spParent, claim.RoleTester, 1, supervise.StateIdle, "")
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: implementer, Entry: 3,
			EntryTask: pwTask(spParent, phase.Implementing), EntryRaisedAt: fixedNow.Add(-time.Hour), EntryMovedBack: true,
			LastHandoff: "implement-1", Pass: 1, Rounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 1}})
		// The backstop an earlier release of the parent's pod armed for its implementer.
		pnArmBackstop(t, pool, spParent, claim.RoleImplementer, fixedNow.Add(time.Hour))
		seedMirrorChild(t, pool, spChild, "todo", spParent)
		// A second parent, waiting on its own open child, that a blocked_by link holds as well.
		spWaiting(t, pool, second, phase.Implementing, "Continue LEGION-310. Issue: LEGION-310. Phase: implementing.",
			"a review asked for changes, while these Dispatch children of LEGION-310 were open: LEGION-311 (todo)", nil)
		seedMirror(t, pool, second, "in_progress", blocker)
		seedMirror(t, pool, blocker, "todo")
		seedMirrorChild(t, pool, secondChild, "todo", second)
		if open := pnOpenBackstops(t, pool, spParent); open != 1 {
			t.Fatalf("open backstops of %s before the move = %d, want the earlier release's one", spParent, open)
		}

		pwApplied(t, pool, admission, "api", "implemented", intake.HandoffComplete{Generation: 1, Issue: spParent,
			Role: claim.RoleImplementer, Claim: implementer, Summary: "implemented", Commit: "head-42", HandoffRecord: "implement-2", HandoffVersion: 2})

		if got := issue(t, pool, spParent); got.Phase != phase.AwaitingChildren || holdsSlot(t, pool, spParent) {
			t.Fatalf("%s after its implementer's completion = phase %s, holding a slot %t (slots %v); want awaiting_children holding none while %s is open",
				spParent, got.Phase, holdsSlot(t, pool, spParent), pnSlotted(t, pool), spChild)
		}
		wait := pwHeld(t, pool, spParent, phase.Testing, "the implementer completed implementing", spChild+" (todo)")
		// The slot the parent gave up went to the root waiting in the line, so the cap is full
		// again without it.
		if got, want := pnSlotted(t, pool), []string{spFiller, promoted}; !slices.Equal(got, want) {
			t.Fatalf("slots = %v, want %v: the parent's slot went to the next in the line", got, want)
		}

		// The state: each waiting parent stands in blocked naming its open child, in neither the
		// active set nor the line, a blocked_by link or not, and the held list names it
		// awaiting_children; the parent shows in_progress, as Dispatch does.
		state := pnState(t, pool, admission)
		for key, child := range map[string]string{spParent: spChild, second: secondChild} {
			if by, listed := pnBlocked(state, key); !listed || !slices.Equal(by, []string{child}) {
				t.Fatalf("admission.blocked = %+v, want %s blocked by %s alone", state.Admission.Blocked, key, child)
			}
			pnInNeitherList(t, state, key)
			if row, listed := pnHeldRow(state, key); !listed || row.Reason != record.HeldAwaitingChildren || !slices.Equal(row.HeldBy, []string{child}) {
				t.Fatalf("admission.held names %s as %+v (listed %t), want awaiting_children held by %s", key, row, listed, child)
			}
		}
		if view := state.Issues[spParent]; view.Phase != phase.AwaitingChildren || view.Status != "in_progress" {
			t.Fatalf("the state's %s = phase %s, status %s; want awaiting_children, in_progress", spParent, view.Phase, view.Status)
		}
		if got := issue(t, pool, spParent); got.DispatchStatus != "in_progress" {
			t.Fatalf("Dispatch shows %s as %s, want in_progress", spParent, got.DispatchStatus)
		}
		pwStatuses(t, pool, spParent, nil)

		// No worker is woken for its work: the backstop armed before the move is gone, and a CI
		// settlement on its head, with every claim of its pod released, queues no role message
		// and takes no pod wake, though the settlement is recorded.
		if open := pnOpenBackstops(t, pool, spParent); open != 0 {
			t.Fatalf("open backstops of %s after the move = %d, want none", spParent, open)
		}
		pnReleasePod(t, pool, spParent)
		pwApplied(t, pool, admission, "github", "settled", intake.PullRequestChecks{Repo: "acme/widgets", Number: 42, HeadSHA: "head-42",
			Generation: 1, Snapshot: "settled-head-42", CheckRuns: []record.AttemptRun{{Name: "unit", ID: 70}}, Failing: []string{"unit"},
			Cancelled: []string{}})
		inTx(t, pool, func(tx pgx.Tx) {
			pr, err := record.NewStore().MergePullRequest(context.Background(), tx, spParent)
			if err != nil || pr == nil || pr.CheckedHead != "head-42" {
				t.Fatalf("the pull request after the settlement = %+v, %v; want the settlement of head-42 recorded", pr, err)
			}
		})
		if messages := pwPayloads[record.RoleMessage](t, pool, spParent); len(messages) != 0 {
			t.Fatalf("role messages of %s = %+v, want none: no role works a waiting parent", spParent, messages)
		}
		if resumes := pnResumes(t, pool, spParent); resumes != 0 {
			t.Fatalf("pod wakes of %s = %d, want none", spParent, resumes)
		}
		for _, token := range []claim.Token{implementer, tester} {
			if state, by := pwClaimState(t, pool, token); state != supervise.StateSuspended || by != supervise.SuspendedByReleased {
				t.Fatalf("the claim %s = %s by %q, want it released still", token, state, by)
			}
		}
		// A pass and the entry sweep, which bring back the roles a slot or an entry is owed,
		// start no role of it.
		spPass(t, pool, admission)
		applySweep(t, pool, admission, "sweep")
		if starts := startsOf(t, pool, spParent); len(starts) != 0 {
			t.Fatalf("starts of %s during the wait = %+v, want none", spParent, starts)
		}

		// close_root, sign_off and request_backward_move are refused, the last two naming the
		// wait, the API's own engine calls; the issue carries its run.
		pwRefused(t, pwApply(t, pool, admission, "api", "close", intake.CloseRoot{Issue: spParent, Reason: "the change is moot"}),
			"CLOSE_AFTER_PHASE_STARTED", "close_root on the waiting parent")
		for _, act := range []struct {
			name string
			fact intake.Fact
		}{
			{name: "sign_off", fact: intake.SignOff{Issue: spParent, By: pnArchitect(t)}},
			{name: "request_backward_move", fact: intake.BackwardMove{Issue: spParent, Requester: claim.RoleArchitect, To: phase.Implementing,
				Reason: "the checks must change", By: pnArchitect(t)}},
		} {
			result := pwApply(t, pool, admission, "api", act.name, act.fact)
			pwRefused(t, result, "WAITING_ON_CHILDREN", act.name+" on the waiting parent")
			if !strings.Contains(result.Refusal.Message, spParent+" waits in awaiting_children") {
				t.Fatalf("the refusal of %s = %q, want it to name the wait", act.name, result.Refusal.Message)
			}
		}
		if got := issue(t, pool, spParent); !got.CarriesRun() || got.Phase != phase.AwaitingChildren || !sameHeldStart(got.ChildrenWait, &wait) {
			t.Fatalf("%s after the refusals = phase %s, held start %+v, carries its run %t; want it waiting as the move left it, carrying its run",
				spParent, got.Phase, got.ChildrenWait, got.CarriesRun())
		}

		// The restart: a new store, a new engine and a new admission on the same database, whose
		// first pass reads the parent back exactly as the move left it.
		restarted := pnAdmission(t, pool, 2)
		spPass(t, pool, restarted)
		if got := issue(t, pool, spParent); got.Phase != phase.AwaitingChildren || !sameHeldStart(got.ChildrenWait, &wait) {
			t.Fatalf("%s after the restarted daemon's pass = phase %s, held start %+v; want the start it holds, %+v",
				spParent, got.Phase, got.ChildrenWait, wait)
		}

		// A root whose rank is above the parent's tree joins the line while the cap is full, and
		// the child reaches done with its pull request merged.
		seedWaiting(t, pool, above, "A")
		counted := spPhaseRow(t, pool, spParent, claim.RoleImplementer)
		back := pnChildDone(t, pool, restarted)

		if back.Phase != phase.Testing || back.ChildrenWait != nil || back.Status != workflow.PhaseStatus(phase.Testing) {
			t.Fatalf("%s at the pass after its child is done = phase %s, held start %+v, status %s; want testing, the start its implementer's completion held, nothing held",
				spParent, back.Phase, back.ChildrenWait, back.Status)
		}
		starts := pwWorkerStarts(t, pool, spParent)
		if len(starts) != 1 || starts[0].role != claim.RoleTester || !starts[0].needsSlot || starts[0].claimable {
			t.Fatalf("starts of %s = %+v, want the tester's one start waiting for a slot", spParent, starts)
		}
		work := starts[0].task
		for _, want := range []string{"Phase: testing.", "start of testing", wait.Reason, pnMergedChildren(spParent, pnMerged(spChild, 51)),
			"Handoff: implement version 2 (dispatch handoff-read --issue LEGION-300 --phase implement --version 2)."} {
			if !strings.Contains(work, want) {
				t.Fatalf("the tester's task = %q, want it to say %q", work, want)
			}
		}
		pnAsksNoForwardMerge(t, "the tester's task", work)
		pnBaseMergeTimes(t, "the tester's task", work, 1)
		if entry := spEntry(spLine(t, pool), spParent); entry == nil || entry.Kind != record.LineReturning || entry.Part != record.PartReturned {
			t.Fatalf("the parent's place in the line = %+v, want it among the issues whose work came back", entry)
		}
		if got, want := pnState(t, pool, restarted).Admission.Waiting, []string{spParent, above}; !slices.Equal(got, want) {
			t.Fatalf("admission.waiting = %v, want %v: the returned start before the root ranked above the parent's tree", got, want)
		}
		// The return counts no review round and no move back: the implementer's row, which holds
		// the counts, is as it was, and the tester's is on a new entry raised as a move back.
		if row := spPhaseRow(t, pool, spParent, claim.RoleImplementer); row.Pass != counted.Pass || row.Rounds != counted.Rounds ||
			!maps.Equal(row.BackwardMoves, counted.BackwardMoves) || row.Entry != counted.Entry {
			t.Fatalf("the implementer's row after the return = %+v, want the entry and the counts of %+v: the return counts no round and no move back", row, counted)
		}
		if row := spPhaseRow(t, pool, spParent, claim.RoleTester); row.Entry != 1 || !row.EntryMovedBack || row.EntryFrom != phase.AwaitingChildren || row.EntryTask != work {
			t.Fatalf("the tester's row after the return = %+v, want entry 1 raised on its task as a move back out of awaiting_children", row)
		}
		if told := spNotices(t, pool, spParent); len(told) != 1 {
			t.Fatalf("awaiting-children notices = %+v, want the move's one: the return tells nobody", told)
		}

		// The first slot that comes free is the parent's, and its start goes to the tester session
		// the parent had.
		freed := issue(t, pool, spFiller)
		freed.Phase, freed.Status = phase.Accepting, "needs_review"
		putIssue(t, pool, freed)
		spPass(t, pool, restarted)
		if !holdsSlot(t, pool, spParent) || holdsSlot(t, pool, above) || issue(t, pool, above).Phase != phase.Admitted {
			t.Fatalf("slots = %v, %s at %s; want the freed slot the parent's and %s still waiting", pnSlotted(t, pool), above, issue(t, pool, above).Phase, above)
		}
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || !starts[0].claimable {
			t.Fatalf("starts of %s = %+v, want its start released by the slot", spParent, starts)
		}
		project, err := claim.ProjectToken(testProject)
		if err != nil {
			t.Fatalf("project token: %v", err)
		}
		start := pnWorkerStart(t, pool, spParent, claim.RoleTester)
		if token, err := claim.NewToken(project, start.Tree, start.Role); err != nil || token != tester || start.FreshSession ||
			start.Generation != 1 || start.Phase != phase.Testing || start.Task != work {
			t.Fatalf("the start = %+v, naming claim %s (%v); want the tester's claim %s at generation 1, its session kept", start, token, err, tester)
		}
		if session := pnSession(t, pool, tester); session != "ses_worker" {
			t.Fatalf("the tester's claim holds session %q, want the session it had, ses_worker", session)
		}
		for _, effect := range effects(t, pool) {
			if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == spParent && request.Op != "start" {
				t.Fatalf("supervise request %+v of %s, want none but starts: nothing ended a claim of the run", request, spParent)
			}
		}

		// The run is as it was: the plan record, the pull request and the generation.
		if got := spPhaseRow(t, pool, spParent, claim.RolePlanner); got.HandoffRecord != plan.HandoffRecord || got.HandoffVersion != plan.HandoffVersion ||
			got.Summary != plan.Summary || got.HandoffCommit != plan.HandoffCommit || !got.CompletedAt.Equal(plan.CompletedAt) {
			t.Fatalf("the planner's row = %+v, want the plan record %+v kept", got, plan)
		}
		inTx(t, pool, func(tx pgx.Tx) {
			pr, err := record.NewStore().MergePullRequest(context.Background(), tx, spParent)
			if err != nil || pr == nil || pr.Number != 42 || pr.HeadSHA != "head-42" || pr.State != record.PullRequestOpen || pr.Branch != "legion/"+spParent {
				t.Fatalf("the pull request = %+v, %v; want #42 open at head-42 on its branch", pr, err)
			}
		})
		if got := issue(t, pool, spParent); got.Generation != 1 || !got.CarriesRun() {
			t.Fatalf("%s = generation %d, carries its run %t; want generation 1, carrying it", spParent, got.Generation, got.CarriesRun())
		}
	})

	// The other returns, each from a move the engine makes.
	tested := func(verdict, summary string) intake.HandoffComplete {
		return intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleTester, Claim: "tester", Summary: summary,
			Verdict: verdict, Commit: "head-42", HandoffRecord: "test-1", HandoffVersion: 1}
	}
	implemented := intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleImplementer, Claim: "implementer",
		Summary: "implemented", Commit: "head-42", HandoffRecord: "implement-1", HandoffVersion: 1}
	merged := pnMergedChildren(spParent, pnMerged(spChild, 51))

	// R12's held starts, each the return takes where the rule says (pnReturnsWhereItTakes): the
	// five starts a parent can hold, each with its pull request open and with it merged during the
	// wait.
	t.Run("every held start comes back where the return takes it", func(t *testing.T) {
		held := pnHeldStarts(t)
		if len(held) != 5 {
			t.Fatalf("the held starts = %d, want the five a parent can hold", len(held))
		}
		for _, start := range held {
			for _, pr := range []struct {
				name   string
				merged bool
			}{{name: "its pull request open"}, {name: "its pull request merged during the wait", merged: true}} {
				t.Run("a held "+string(start.start)+", "+pr.name, func(t *testing.T) { pnReturnsWhereItTakes(t, start, pr.merged) })
			}
		}
	})

	// The children the return names: a parent waits on three children, and a fourth is filed and
	// released under it during the wait, which the next pass adds to the wait's children. The first
	// reaches done with its pull request merged, the second moves to icebox, the third moves to the
	// backlog by its owning architect's status write (its record leaving the workflow), and the
	// fourth reaches done with its pull request merged. The testing task the return starts names the
	// first and the fourth, each with its pull request, and neither of the others, and it carries the
	// Handoff line of the implementer's last handoff, which roles/tester.md tells the tester to read.
	t.Run("the return names the children that merged during the wait and no other", func(t *testing.T) {
		const (
			iceboxed = "LEGION-302"
			parked   = "LEGION-303"
			late     = "LEGION-304"
		)
		pool := migratedPool(t)
		admission := pnAdmission(t, pool, 4)
		spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "todo", spParent)
		seedMirrorChild(t, pool, iceboxed, "todo", spParent)
		// The child its architect moves to the backlog is a released one, which the daemon records in the tree.
		putIssue(t, pool, record.Issue{Key: parked, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: parked, Tree: spParent, Parent: record.ParentOf(spParent), Phase: phase.Admitted, Generation: 1,
			Status: "todo", DispatchStatus: "todo", Rank: parked, HandedOver: true, LastDispatchSeq: 1})
		seedMirrorChild(t, pool, parked, "todo", spParent)

		pwApplied(t, pool, admission, "api", "implemented", intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleImplementer,
			Claim: "implementer", Summary: "implemented", Commit: "head-42", HandoffRecord: "implement-2", HandoffVersion: 2})
		wait := pwHeld(t, pool, spParent, phase.Testing, "the implementer completed implementing",
			spChild+" (todo)", iceboxed+" (todo)", parked+" (todo)")

		// The fourth child is filed and released under the waiting parent.
		pwApplied(t, pool, admission, "dispatch", "released", intake.DispatchIssue{Key: late, ResolvedRepo: "acme/widgets", Seq: 1,
			Type: "issue.updated", Status: "todo", Title: late, Parent: spParent, Rank: late, HandedOver: true})
		if got := issue(t, pool, spParent); got.Phase != phase.AwaitingChildren {
			t.Fatalf("the parent after its fourth child's release = %s, want awaiting_children still", got.Phase)
		}

		pnChildMerged(t, pool, spChild, spParent, 51)
		spMirror(t, pool, iceboxed, "icebox", spParent, 2)
		putIssue(t, pool, record.Issue{Key: parked, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: parked, Tree: spParent, Parent: record.ParentOf(spParent), Phase: phase.Done, Generation: 1,
			Status: "backlog", DispatchStatus: "backlog", Rank: parked, HandedOver: true, LastDispatchSeq: 2})
		// The child on the backlog had opened a pull request, which did not merge.
		seedPullRequest(t, pool, parked, 53, record.PullRequestOpen)
		spMirror(t, pool, parked, "backlog", spParent, 2)
		pnChildMerged(t, pool, late, spParent, 54)
		// Before the pass that takes it back, the wait holds all four children.
		got := issue(t, pool, spParent)
		if got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil ||
			!slices.Equal(slices.Sorted(slices.Values(got.ChildrenWait.Children)), []string{spChild, iceboxed, parked, late}) {
			t.Fatalf("the parent before its return = phase %s, held start %+v; want awaiting_children holding the children %v",
				got.Phase, got.ChildrenWait, []string{spChild, iceboxed, parked, late})
		}

		spPass(t, pool, admission)

		if back := issue(t, pool, spParent); back.Phase != phase.Testing || back.ChildrenWait != nil {
			t.Fatalf("the parent after its children are finished = phase %s, held start %+v; want testing", back.Phase, back.ChildrenWait)
		}
		starts := pwWorkerStarts(t, pool, spParent)
		if len(starts) != 1 || starts[0].role != claim.RoleTester {
			t.Fatalf("starts of %s = %+v, want the tester's", spParent, starts)
		}
		work := starts[0].task
		for _, want := range []string{"Phase: testing.", "start of testing", wait.Reason,
			pnMergedChildren(spParent, pnMerged(spChild, 51), pnMerged(late, 54)) + ".",
			"Handoff: implement version 2 (dispatch handoff-read --issue LEGION-300 --phase implement --version 2)."} {
			if !strings.Contains(work, want) {
				t.Fatalf("the tester's task = %q, want it to say %q", work, want)
			}
		}
		pnAsksNoForwardMerge(t, "the tester's task", work)
		pnBaseMergeTimes(t, "the tester's task", work, 1)
	})

	// The tester's rule for every round of a parent: each path that writes a testing task
	// (pnTestingPaths), run for a parent that waited, for a parent whose children all finished before
	// its first phase, and for an issue with no child, and each return for a parent that waited.
	t.Run("every testing task of a parent asks for the base-merge run", func(t *testing.T) {
		paths, kinds, rows := pnTestingPaths(), pnParentKinds(), 0
		if len(paths) != 12 {
			t.Fatalf("the paths that write a testing task = %d, want the 12 there are", len(paths))
		}
		for _, path := range paths {
			for i, kind := range kinds {
				if path.returns && i != 0 {
					continue
				}
				rows++
				t.Run(path.name+", "+kind.name, func(t *testing.T) { pnAsksTheBaseMergeRun(t, path, kind) })
			}
		}
		if rows != 32 {
			t.Fatalf("the rows of the inventory = %d, want 32", rows)
		}
	})

	t.Run("a held implementing after a tester's fail comes back with the tester's reason in the task", func(t *testing.T) {
		pool, admission, wait := pnMovedParent(t, phase.Testing, nil, "api", tested("fail", "the empty list panics"),
			phase.Implementing, "the tester completed testing")

		back := pnChildDone(t, pool, admission)

		if back.Phase != phase.Implementing || back.ChildrenWait != nil {
			t.Fatalf("the parent = phase %s, held start %+v; want implementing", back.Phase, back.ChildrenWait)
		}
		starts := pwWorkerStarts(t, pool, spParent)
		if len(starts) != 1 || starts[0].role != claim.RoleImplementer {
			t.Fatalf("starts of %s = %+v, want the implementer's", spParent, starts)
		}
		work := starts[0].task
		pnAsksNoForwardMerge(t, "the implementer's task", work)
		if !strings.HasPrefix(work, wait.Task) {
			t.Fatalf("the implementer's task = %q, want it to open with the held task %q", work, wait.Task)
		}
		for _, want := range []string{"Tester's verdict: fail.", "Handoff: test version 1", merged} {
			if !strings.Contains(work, want) {
				t.Fatalf("the implementer's task = %q, want it to carry %q", work, want)
			}
		}
	})

	// The architect's last lever during the wait (R11): it moves a child out of the parent's tree.
	// A child Dispatch shows under no parent now is no open child of the parent (blockers.Mirror's
	// readingOf copies the parent Dispatch shows, none among them), so the next pass ends the wait
	// and the parent comes back at the start it held, on the task it held, naming no child as
	// merged: the child did not merge.
	t.Run("a child moved out from under the waiting parent to no parent ends the wait, and the parent comes back at the start it held", func(t *testing.T) {
		pool, admission, wait := pnMovedParent(t, phase.Testing, nil, "api", tested("fail", "the empty list panics"),
			phase.Implementing, "the tester completed testing")

		pwApplied(t, pool, admission, "dispatch", "moved-out", intake.DispatchIssue{Key: spChild, ResolvedRepo: "acme/widgets", Seq: 2,
			Type: "issue.updated", Status: "todo", Title: spChild, Rank: spChild})
		spPass(t, pool, admission)

		back := issue(t, pool, spParent)
		if back.Phase != phase.Implementing || back.ChildrenWait != nil {
			t.Fatalf("the parent after its one child moved to no parent = phase %s, held start %+v; want implementing, the start it held",
				back.Phase, back.ChildrenWait)
		}
		starts := pwWorkerStarts(t, pool, spParent)
		if len(starts) != 1 || starts[0].role != claim.RoleImplementer || !strings.HasPrefix(starts[0].task, wait.Task) ||
			strings.Contains(starts[0].task, "merged during the wait") {
			t.Fatalf("starts of %s = %+v, want the implementer's on the held task %q, naming no child as merged", spParent, starts, wait.Task)
		}
		pnAsksNoForwardMerge(t, "the implementer's task", starts[0].task)
	})

	// The parent that held planning after the architect's backward move comes back at planning with
	// that reason, and no task of its later rounds asks for a forward merge: the planner's, the
	// implementer's after the plan, the tester's after it, the implementer's after the tester
	// fails that round, the tester's of the next round, and the reviewer's once the tester passes
	// it.
	t.Run("a held planning after the architect's backward move comes back at planning, and no task of its later rounds asks for a forward merge", func(t *testing.T) {
		const reason = "the plan missed the empty list"
		pool, admission, wait := pnMovedParent(t, phase.Accepting, nil, "api",
			intake.BackwardMove{Issue: spParent, Requester: claim.RoleArchitect, To: phase.Planning, Reason: reason, By: pnArchitect(t)},
			phase.Planning, "moved back to planning by the architect: "+reason)

		back := pnChildDone(t, pool, admission)

		if back.Phase != phase.Planning || back.ChildrenWait != nil {
			t.Fatalf("the parent = phase %s, held start %+v; want planning", back.Phase, back.ChildrenWait)
		}
		starts := pwWorkerStarts(t, pool, spParent)
		if len(starts) != 1 || starts[0].role != claim.RolePlanner || !strings.HasPrefix(starts[0].task, wait.Task) ||
			!strings.Contains(starts[0].task, reason) || !strings.Contains(starts[0].task, merged) {
			t.Fatalf("starts of %s = %+v, want the planner's on the held task %q, which carries %q, naming the child that merged: %q",
				spParent, starts, wait.Task, reason, merged)
		}
		pnAsksNoForwardMerge(t, "the planner's task", starts[0].task)

		pwApplied(t, pool, admission, "api", "planned", intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RolePlanner,
			Claim: "planner", Summary: "planned again", Commit: "head-42", HandoffRecord: "plan-2", HandoffVersion: 2})

		if got := issue(t, pool, spParent); got.Phase != phase.Implementing {
			t.Fatalf("the parent after its planner's completion = phase %s, want implementing", got.Phase)
		}
		pnAsksNoForwardMerge(t, "the first implementing task after the wait", pnWorkerStart(t, pool, spParent, claim.RoleImplementer).Task)

		for _, round := range []struct {
			what string
			fact intake.HandoffComplete
			at   phase.Phase
		}{
			{what: "the first testing task after the wait", at: phase.Testing, fact: intake.HandoffComplete{Generation: 1, Issue: spParent,
				Role: claim.RoleImplementer, Claim: "implementer", Summary: "implemented", Commit: "head-42", HandoffRecord: "implement-2", HandoffVersion: 2}},
			{what: "the implementing task after the tester's fail", at: phase.Implementing, fact: intake.HandoffComplete{Generation: 1, Issue: spParent,
				Role: claim.RoleTester, Claim: "tester", Summary: "the empty list panics", Verdict: "fail", Commit: "head-42", HandoffRecord: "test-2", HandoffVersion: 2}},
			{what: "the testing task of the next round", at: phase.Testing, fact: intake.HandoffComplete{Generation: 1, Issue: spParent,
				Role: claim.RoleImplementer, Claim: "implementer", Summary: "the empty list handled", Commit: "head-42", HandoffRecord: "implement-3", HandoffVersion: 3}},
			{what: "the reviewing task once the tester passes the round", at: phase.Reviewing, fact: intake.HandoffComplete{Generation: 1, Issue: spParent,
				Role: claim.RoleTester, Claim: "tester", Summary: "every check passed", Verdict: "pass", Commit: "head-42", HandoffRecord: "test-3", HandoffVersion: 3}},
		} {
			pwApplied(t, pool, admission, "api", round.what, round.fact)
			if got := issue(t, pool, spParent); got.Phase != round.at {
				t.Fatalf("the parent before %s = phase %s, want %s", round.what, got.Phase, round.at)
			}
			pnAsksNoForwardMerge(t, round.what, pnWorkerStart(t, pool, spParent, phase.RoleFor(round.at)).Task)
		}
	})

	// k13's parent that comes back at implementing on a pull request that does not conflict: the
	// return's task, the operator's retry of its implementer in a fresh session (`legion claims
	// retry --fresh-session`, intake.RestartPhase), and the boot's resume of that implementer after a
	// daemon restart, each a task a session that may have read none of the one before it is given,
	// ask for no forward merge.
	t.Run("a parent that comes back at implementing, retried with a fresh session and resumed after a daemon restart, gets tasks that ask for no forward merge", func(t *testing.T) {
		pool, admission, _ := pnMovedParent(t, phase.Testing, nil, "api", tested("fail", "the empty list panics"),
			phase.Implementing, "the tester completed testing")
		if back := pnChildDone(t, pool, admission); back.Phase != phase.Implementing {
			t.Fatalf("the parent after its child's close = phase %s, want implementing", back.Phase)
		}
		first := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
		pnAsksNoForwardMerge(t, "the return's implementer task", first.Task)

		pwApplied(t, pool, admission, "api", "fresh-session", intake.RestartPhase{Issue: spParent, Role: claim.RoleImplementer, FreshSession: true})
		restarted := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
		if restarted.Task == first.Task || !restarted.FreshSession || !strings.Contains(restarted.Task, "Reason: the operator retried this phase's worker") {
			t.Fatalf("the newest implementer start = %+v, want the restart's own, in a new session, naming the operator's retry", restarted)
		}
		pnAsksNoForwardMerge(t, "the restarted implementer's task", restarted.Task)

		// The lane delivered the starts, the claim the daemon suspended at its shutdown stands on
		// the entry the return raised, and a daemon on the same database boots.
		for range startsOf(t, pool, spParent) {
			runStart(t, pool, spParent)
		}
		token := putWorkerClaim(t, pool, spParent, spParent, claim.RoleImplementer, 1, supervise.StateSuspended, supervise.SuspendedByDaemon)
		markGivenEntry(t, pool, token, spPhaseRow(t, pool, spParent, claim.RoleImplementer).Entry)
		booted := pnAdmission(t, pool, 2)
		pwApplied(t, pool, booted, "daemon", "boot", intake.DaemonBooted{Boot: 2})

		resumed := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
		if !resumed.ResumeTask || !strings.Contains(resumed.Task, "Resume the existing phase work.") {
			t.Fatalf("the newest implementer start after the boot = %+v, want the boot's resume of its phase", resumed)
		}
		pnAsksNoForwardMerge(t, "the implementer's task after the daemon restart", resumed.Task)
	})

	// A parent whose head conflicted with its base before the wait: the implementer had been asked
	// to merge the base forward in testing and had not landed it when the tester failed the round
	// and an open child held the implementing that fail raised, so the move dropped the merge
	// forward (R10). The return asks for no forward merge: the tester's merge with the base finds
	// a conflict that is still there, and the implementer merges forward once GitHub reads the pull
	// request CONFLICTING.
	t.Run("a parent whose head conflicted with its base before the wait comes back as R12 says, with no forward merge asked", func(t *testing.T) {
		pool, admission, wait := pnMovedParent(t, phase.Testing, func(t *testing.T, pool *pgxpool.Pool) {
			pwSeedForward(t, pool, pwAsked(phase.Testing, false))
		}, "api", tested("fail", "the empty list panics"), phase.Implementing, "the tester completed testing")
		if left := pwForwardOf(t, pool); left != nil {
			t.Fatalf("the merge forward of %s after the move = %+v, want it dropped", spParent, left)
		}

		back := pnChildDone(t, pool, admission)

		if back.Phase != phase.Implementing || back.ChildrenWait != nil {
			t.Fatalf("the parent = phase %s, held start %+v; want implementing, the start it held", back.Phase, back.ChildrenWait)
		}
		start := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
		if !strings.HasPrefix(start.Task, wait.Task) || !strings.Contains(start.Task, merged) {
			t.Fatalf("the implementer's task = %q, want the held task %q naming the child that merged: %q", start.Task, wait.Task, merged)
		}
		pnAsksNoForwardMerge(t, "the implementer's task", start.Task)
		if asked := pwForwardOf(t, pool); asked != nil {
			t.Fatalf("the merge forward of %s after the return = %+v, want none asked", spParent, asked)
		}
	})

	t.Run("a parent in a tree whose design gate is closed at a newer version comes back without an approval", func(t *testing.T) {
		approved := 2
		gate := record.DesignGate{Issue: spParent, ArtifactID: "5b0c2d7e-1f43-4a8e-9c6d-2e7f1a3b8d40", LatestVersion: 3,
			ApprovedVersion: &approved, EverOpened: true}
		pool, admission, _ := pnMovedParent(t, phase.Implementing, func(t *testing.T, pool *pgxpool.Pool) { seedGate(t, pool, gate) },
			"api", implemented, phase.Testing, "the implementer completed implementing")

		back := pnChildDone(t, pool, admission)
		spPass(t, pool, admission)

		if back.Phase != phase.Testing || back.ChildrenWait != nil {
			t.Fatalf("the parent = phase %s, held start %+v; want testing", back.Phase, back.ChildrenWait)
		}
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleTester || !starts[0].claimable ||
			!holdsSlot(t, pool, spParent) {
			t.Fatalf("starts of %s = %+v, slots %v; want the tester's released by a slot, the closed gate holding nothing", spParent, starts, pnSlotted(t, pool))
		}
		inTx(t, pool, func(tx pgx.Tx) {
			gates, err := record.NewStore().GatesOf(context.Background(), tx, spParent)
			got := record.SpecGate(gates)
			if err != nil || got == nil || got.LatestVersion != 3 || got.ApprovedVersion == nil || *got.ApprovedVersion != 2 {
				t.Fatalf("the design gate after the return = %+v, %v; want version 3 still unapproved, approved at 2", got, err)
			}
		})
	})

	t.Run("a parent a blocked_by link holds at the return waits on it in the line, behind one whose wait ended earlier, until the link's issue is done", func(t *testing.T) {
		const (
			blocker    = "LEGION-900"
			early      = "LEGION-360"
			earlyChild = "LEGION-361"
			late       = "LEGION-350"
			lateChild  = "LEGION-351"
		)
		pool := migratedPool(t)
		admission := pnAdmission(t, pool, 2)
		seedMirror(t, pool, blocker, "todo")
		for parent, child := range map[string]string{early: earlyChild, late: lateChild} {
			spWaiting(t, pool, parent, phase.Implementing, "Continue "+parent+". Issue: "+parent+". Phase: implementing.", "a review asked for changes", nil)
			seedMirror(t, pool, parent, "in_progress", blocker)
			seedMirrorChild(t, pool, child, "todo", parent)
		}

		spMirror(t, pool, earlyChild, "done", early, 2)
		spPass(t, pool, admission)
		spMirror(t, pool, lateChild, "done", late, 2)
		spPass(t, pool, admission)

		state := pnState(t, pool, admission)
		if want := []string{early, late}; !slices.Equal(state.Admission.Waiting, want) {
			t.Fatalf("admission.waiting = %v, want %v: ordered by the moment each wait on the children ended", state.Admission.Waiting, want)
		}
		for _, key := range []string{early, late} {
			if got := issue(t, pool, key); got.Phase != phase.Implementing || got.ChildrenWait != nil {
				t.Fatalf("%s after its child is done = phase %s, held start %+v; want implementing", key, got.Phase, got.ChildrenWait)
			}
			if line := state.Issues[key].WaitingLine; line == nil || line.Kind != record.LineWaitingOnIssue.String() {
				t.Fatalf("the state's place of %s in the line = %+v, want the waiting kind %s", key, line, record.LineWaitingOnIssue)
			}
			if starts := startsOf(t, pool, key); len(starts) != 1 || starts[0].claimable {
				t.Fatalf("starts of %s = %+v, want one held while %s is open", key, starts, blocker)
			}
		}
		assertSlots(t, pool, nil)

		spMirror(t, pool, blocker, "done", "", 2)
		spPass(t, pool, admission)

		for _, key := range []string{early, late} {
			if starts := startsOf(t, pool, key); len(starts) != 1 || !starts[0].claimable || !holdsSlot(t, pool, key) {
				t.Fatalf("starts of %s once %s is done = %+v, slots %v; want its start delivered with a slot", key, blocker, starts, pnSlotted(t, pool))
			}
		}
	})

	t.Run("a grandparent comes back only after its waiting child has come back and reached done", func(t *testing.T) {
		const (
			grandparent = "LEGION-370"
			parent      = "LEGION-371"
			grandchild  = "LEGION-372"
		)
		pool := migratedPool(t)
		admission := pnAdmission(t, pool, 3)
		spSeedParent(t, pool, grandparent, phase.Implementing, true, nil)
		putIssue(t, pool, record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: parent, Tree: grandparent, Parent: record.ParentOf(grandparent), Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", Rank: parent})
		takeSlot(t, pool, parent)
		seedPullRequest(t, pool, grandparent, 43, record.PullRequestOpen)
		seedMirrorChild(t, pool, parent, "in_progress", grandparent)
		seedMirrorChild(t, pool, grandchild, "todo", parent)
		spSeedPhase(t, pool, record.PhaseRow{Issue: parent, Role: claim.RoleImplementer, Claim: "implementer", Entry: 1,
			EntryTask: pwTask(parent, phase.Implementing), EntryRaisedAt: fixedNow.Add(-time.Hour)})

		// The grandparent's implementer completes while its child is open; the pass that fact runs
		// stops the child's own implementing, since a child of it is open too.
		pwApplied(t, pool, admission, "api", "implemented", intake.HandoffComplete{Generation: 1, Issue: grandparent, Role: claim.RoleImplementer,
			Claim: "implementer", Summary: "implemented", Commit: "head-43", HandoffRecord: "implement-1", HandoffVersion: 1})
		for key, start := range map[string]phase.Phase{grandparent: phase.Testing, parent: phase.Implementing} {
			if got := issue(t, pool, key); got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil || got.ChildrenWait.Start != start {
				t.Fatalf("%s = phase %s, held start %+v; want awaiting_children holding %s", key, got.Phase, got.ChildrenWait, start)
			}
		}

		spMirror(t, pool, grandchild, "done", parent, 2)
		spPass(t, pool, admission)
		if got := issue(t, pool, parent); got.Phase != phase.Implementing {
			t.Fatalf("%s once its child is done = %s, want implementing", parent, got.Phase)
		}
		if got := issue(t, pool, grandparent); got.Phase != phase.AwaitingChildren {
			t.Fatalf("%s while %s works after its return = %s, want awaiting_children still", grandparent, parent, got.Phase)
		}

		closed := issue(t, pool, parent)
		closed.Phase, closed.Status = phase.Done, "done"
		putIssue(t, pool, closed)
		spMirror(t, pool, parent, "done", grandparent, 2)
		spPass(t, pool, admission)
		if got := issue(t, pool, grandparent); got.Phase != phase.Testing || got.ChildrenWait != nil {
			t.Fatalf("%s once %s reached done = phase %s, held start %+v; want testing, the start it held", grandparent, parent, got.Phase, got.ChildrenWait)
		}
	})

	// A child parent that held accepting and whose pull request merged during the wait goes back to
	// accepting on today's path into it (dispatch://INFRA-3320 R12): its own architect, whose claim
	// failed during the wait, is started again, as every phase start of a child starts it
	// (workflow's reviveArchitect), and the accepting step's run opens under that architect
	// (workflow's startNamedStep), so the architect's sign_off ends that run, passed, before the
	// retro's run starts.
	t.Run("a child parent back in accepting after its pull request merged during the wait starts its failed architect again and opens the accepting run the sign_off ends", func(t *testing.T) {
		const (
			root   = "LEGION-380"
			parent = "LEGION-381"
			child  = "LEGION-382"
		)
		pool := migratedPool(t)
		admission := pnAdmission(t, pool, 3)
		spSeedParent(t, pool, root, phase.Implementing, true, nil)
		waited := fixedNow.Add(-2 * time.Hour)
		putIssue(t, pool, record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: parent, Tree: root, Parent: record.ParentOf(root), Phase: phase.AwaitingChildren,
			Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: parent, LastDispatchSeq: 1,
			ChildrenWait: &record.ChildrenWait{Start: phase.Accepting, Task: "Continue " + parent + ". Issue: " + parent + ". Phase: accepting. Pull request: #44.",
				Reason: "a review approved the head, while these Dispatch children of " + parent + " were open: " + child + " (todo)",
				Since:  waited, Children: []string{child}},
			ChildrenWaitedAt: &waited})
		seedPullRequest(t, pool, parent, 44, record.PullRequestMerged)
		seedMirrorChild(t, pool, parent, "in_progress", root)
		seedMirrorChild(t, pool, child, "done", parent)
		architect := putWorkerClaim(t, pool, root, parent, claim.RoleArchitect, 1, supervise.StateFailed, "")

		spPass(t, pool, admission)

		if got := issue(t, pool, parent); got.Phase != phase.Accepting || got.ChildrenWait != nil {
			t.Fatalf("%s once its child is done = phase %s, held start %+v; want accepting, the start it held", parent, got.Phase, got.ChildrenWait)
		}
		revived := architectStartsOf(t, pool, parent)
		if want := []record.SuperviseRequest{{Op: "start", Tree: root, Role: claim.RoleArchitect, Generation: 1}}; !reflect.DeepEqual(revived, want) {
			t.Fatalf("starts of %s's architect = %+v, want the one plain start of its failed claim %+v", parent, revived, want)
		}
		assertStepWrites(t, pool, parent, start("step", 5, 1, 1))
		if opened := stepWrites(t, pool, parent)[0]; opened.Agent != string(architect) || opened.Role != "acceptance" {
			t.Fatalf("the start of %s's accepting run = %+v, want it the acceptance of its own architect %s", parent, opened, architect)
		}

		// The architect's sign_off names its claim and the run of the step it reads, as the route
		// does (rootArchitectsRead reads that run; the claim is the child's own architect's).
		_, generation, entry := rootArchitectsRead(t, pool, parent)
		pwApplied(t, pool, admission, "api", "sign-off", intake.SignOff{Issue: parent, By: architect, Generation: generation, Entry: entry})

		if got := issue(t, pool, parent); got.Phase != phase.Retro {
			t.Fatalf("%s after the sign_off = %s, want retro", parent, got.Phase)
		}
		assertStepWrites(t, pool, parent, start("step", 5, 1, 1), end("passed", "step", 5, 1, 1), start("retro", 0, 1, 1))
	})

	// A reviewer the admission pass stops in the middle of reviewing, whose pull request then
	// merges during the wait, goes back to reviewing on the task it was given, and that task is
	// named its round once, by the raise of the return's own entry (dispatch://INFRA-3320 R12,
	// workflow's namingTheReviewRound): the return's task names one commit and one entry, the
	// return's. The hold keeps the task as it was composed, with none of the round's sentences.
	// Two entries are read: one this release raised, at the tester's pass, whose phases row keeps
	// the task the reviewer was given as it was composed, and one the release before raised, whose
	// stored task carries the round's sentences and which is held as the reviewer's resume work.
	t.Run("a reviewer stopped in the middle of reviewing whose pull request merged during the wait comes back naming one commit and one entry", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			// stop records the parent with its reviewer in the middle of reviewing, its pull
			// request #42 open, and answers the task the hold is to keep.
			stop func(*testing.T, *pgxpool.Pool, *Admission) string
		}{
			{name: "on an entry this release raised", stop: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) string {
				spSeedParent(t, pool, spParent, phase.Testing, true, nil)
				pwPullRequest(t, pool, spParent, record.PullRequestOpen)
				pwApplied(t, pool, admission, "api", "tested", intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleTester,
					Claim: "tester", Summary: "every check passed", Verdict: "pass", Commit: "head-42", HandoffRecord: "test-1", HandoffVersion: 1})
				reviewing := pnWorkerStart(t, pool, spParent, claim.RoleReviewer)
				named := reviewround.Sentences("", reviewing.Entry)
				if !strings.HasSuffix(reviewing.Task, named) {
					t.Fatalf("the reviewer's start after the tester's pass = %+v, want its task ending with its round's sentences %q", reviewing, named)
				}
				putWorkerClaim(t, pool, spParent, spParent, claim.RoleReviewer, 1, supervise.StateWorking, "")
				pwDeliver(t, pool, spParent, pwRoleToken(t, spParent, claim.RoleReviewer), reviewing)
				return strings.TrimSuffix(reviewing.Task, named)
			}},
			{name: "on an entry the release before raised", stop: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) string {
				token := pwMidPhase(t, pool, spParent, phase.Reviewing, supervise.StateWorking, "")
				spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleReviewer, Claim: token, Entry: 3,
					EntryTask: pwTask(spParent, phase.Reviewing) + reviewround.Sentences("", 3), EntryRaisedAt: fixedNow.Add(-time.Hour)})
				return resumeTaskOf(t, pool, admission.engine, spParent)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				admission := pnAdmission(t, pool, 2)
				composed := tc.stop(t, pool, admission)
				seedMirrorChild(t, pool, spChild, "todo", spParent)

				spPass(t, pool, admission)

				wait := pwHeld(t, pool, spParent, phase.Reviewing, "the admission pass stopped the reviewer's reviewing phase", spChild+" (todo)")
				pwApplied(t, pool, admission, "github", "merged", intake.PullRequestMerged{Repo: "acme/widgets", Number: 42, MergeSHA: "merge-42"})
				stopped := spPhaseRow(t, pool, spParent, claim.RoleReviewer)
				if back := pnChildDone(t, pool, admission); back.Phase != phase.Reviewing || back.ChildrenWait != nil {
					t.Fatalf("the parent after its child is done = phase %s, held start %+v; want reviewing, the start it held", back.Phase, back.ChildrenWait)
				}

				work := pnWorkerStart(t, pool, spParent, claim.RoleReviewer).Task
				for _, label := range []string{reviewround.CommitLabel + ":", reviewround.EntryLabel + ":"} {
					if got := strings.Count(work, label); got != 1 {
						t.Fatalf("the reviewer's task after the return = %q, which says %q %d times; want once", work, label, got)
					}
				}
				row := spPhaseRow(t, pool, spParent, claim.RoleReviewer)
				if entry := fmt.Sprintf(" %s: %d.", reviewround.EntryLabel, row.Entry); row.Entry != stopped.Entry+1 || !strings.HasSuffix(work, entry) {
					t.Fatalf("the reviewer's task after the return = %q on its row's entry %d; want it to end with the return's entry %d, above the stopped %d: %q",
						work, row.Entry, stopped.Entry+1, stopped.Entry, entry)
				}
				if named := pnMergedChildren(spParent, pnMerged(spChild, 51)); !strings.HasPrefix(work, wait.Task) || !strings.Contains(work, named) {
					t.Fatalf("the reviewer's task after the return = %q, want the held task %q followed by %q", work, wait.Task, named)
				}
				if wait.Task != composed {
					t.Fatalf("the held task = %q, want the task the reviewer was given as it was composed, %q, with none of its round's sentences", wait.Task, composed)
				}
			})
		}
	})

	// The linger's hand-over of a closed tree's child in awaiting_children carries its run and the
	// start it holds; the handed-over root is promoted as any is, and comes back as above.
	t.Run("a linger's hand-over carries a waiting child whole, which is promoted holding no slot and comes back as above", func(t *testing.T) {
		const (
			root       = "LEGION-208"
			child      = "LEGION-209"
			grandchild = "LEGION-210"
			artifact   = "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26"
			task       = "Continue LEGION-209. Issue: LEGION-209. Phase: implementing."
			reason     = "a review asked for changes, while these Dispatch children of LEGION-209 were open: LEGION-210 (backlog)"
		)
		pool := migratedPool(t)
		admission := pnAdmission(t, pool, 1)
		seedStarted(t, pool, spFiller, phase.Implementing, nil)
		until, waited := fixedNow.Add(time.Hour), fixedNow.Add(-2*time.Hour)
		held := record.ChildrenWait{Start: phase.Implementing, Task: task, Reason: reason, Since: waited}
		parent := root
		inTx(t, pool, func(tx pgx.Tx) {
			records, ctx := record.NewStore(), context.Background()
			for _, put := range []error{
				records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
					Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
				records.PutIssue(ctx, tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
					Project: testProject, Title: child, Parent: &parent, Phase: phase.AwaitingChildren, Generation: 1, Status: "in_progress", Rank: "B",
					LastDispatchSeq: 1, ChildrenWait: &held, ChildrenWaitedAt: &waited}),
				records.PutGate(ctx, tx, record.DesignGate{Issue: root, ArtifactID: artifact, LatestVersion: 2, ApprovedVersion: new(2)}),
				records.PutPullRequest(ctx, tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209",
					HeadSHA: "head-42", Generation: 1, State: record.PullRequestOpen}),
				records.PutPhase(ctx, tx, record.PhaseRow{Issue: child, Role: claim.RoleImplementer, Claim: "implementer", Entry: 2,
					EntryTask: task, EntryRaisedAt: waited.Add(-time.Hour)}),
				records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
			} {
				if put != nil {
					t.Fatalf("seed: %v", put)
				}
			}
		})
		seedMirrorChild(t, pool, grandchild, "backlog", child)

		apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
			Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, pnLingering())

		got := issue(t, pool, child)
		if got.Tree != child || got.Generation != 2 || got.Phase != phase.AwaitingChildren || !sameHeldStart(got.ChildrenWait, &held) ||
			got.ChildrenWaitedAt == nil || !got.ChildrenWaitedAt.Equal(waited) {
			t.Fatalf("the handed-over child = %+v; want its own tree at generation 2, waiting still with %+v and the wait's moment", got, held)
		}
		if got.Status != "in_progress" || got.HandedOverFrom != "" || holdsSlot(t, pool, child) {
			t.Fatalf("the handed-over child = status %s, handed over from %q, slots %v; want it promoted in_progress holding no slot",
				got.Status, got.HandedOverFrom, pnSlotted(t, pool))
		}
		written := false
		for _, effect := range effects(t, pool) {
			written = written || (effect.issue == child && reflect.DeepEqual(effect.payload, record.OrdinaryStatusWrite("in_progress", "todo", "")))
		}
		if !written {
			t.Fatalf("outbox effects = %+v, want %s written in_progress at its promotion", effects(t, pool), child)
		}
		if starts := pwWorkerStarts(t, pool, child); len(starts) != 0 || architectStarts(t, pool, child) != 1 {
			t.Fatalf("starts of %s = %+v, want its architect's alone", child, startsOf(t, pool, child))
		}
		inTx(t, pool, func(tx pgx.Tx) {
			records, ctx := record.NewStore(), context.Background()
			pr, err := records.MergePullRequest(ctx, tx, child)
			if err != nil || pr == nil || pr.HeadSHA != "head-42" || pr.Generation != 1 {
				t.Fatalf("carried pull request = %+v, %v; want head-42 with its settlement's generation 1 kept", pr, err)
			}
			gates, err := records.GatesOf(ctx, tx, child)
			if gate := record.SpecGate(gates); err != nil || gate == nil || gate.ArtifactID != artifact {
				t.Fatalf("carried gate = %+v, %v; want the old root's", gates, err)
			}
		})
		if row := spPhaseRow(t, pool, child, claim.RoleImplementer); row.Entry != 2 || row.EntryTask != task {
			t.Fatalf("the implementer's phases row = %+v, want it carried at entry 2", row)
		}

		spMirror(t, pool, grandchild, "done", child, 2)
		spPass(t, pool, admission)

		if back := issue(t, pool, child); back.Phase != phase.Implementing || back.ChildrenWait != nil {
			t.Fatalf("the carried root after its child is done = phase %s, held start %+v; want implementing", back.Phase, back.ChildrenWait)
		}
		starts := pwWorkerStarts(t, pool, child)
		if len(starts) != 1 || starts[0].role != claim.RoleImplementer || !starts[0].needsSlot || starts[0].claimable ||
			!strings.HasPrefix(starts[0].task, task) {
			t.Fatalf("starts of %s = %+v, want the implementer's on the held task, waiting for a slot under the full cap", child, starts)
		}
		pnAsksNoForwardMerge(t, "the carried root's implementer task", starts[0].task)
		if entry := spEntry(spLine(t, pool), child); entry == nil || entry.Kind != record.LineReturning {
			t.Fatalf("the carried root's place in the line = %+v, want it among the issues whose work came back", entry)
		}
	})

	// A promotion of a re-admitted tree brings back the roles of its members that have run, and
	// none of a member waiting on its children.
	t.Run("a re-admission of its tree starts no role of a waiting parent", func(t *testing.T) {
		const root, member, grandchild = "LEGION-208", "LEGION-209", "LEGION-210"
		pool := migratedPool(t)
		admission := pnAdmission(t, pool, 1)
		putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: "root", Tree: root, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", LastDispatchSeq: 1})
		parent, waited := root, fixedNow.Add(-2*time.Hour)
		held := record.ChildrenWait{Start: phase.Implementing, Task: "Continue member. Issue: LEGION-209. Phase: implementing.",
			Reason: "a review asked for changes, while these Dispatch children of LEGION-209 were open: LEGION-210 (backlog)", Since: waited}
		putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: "member", Tree: root, Parent: &parent, Phase: phase.AwaitingChildren, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "B", LastDispatchSeq: 1, ChildrenWait: &held, ChildrenWaitedAt: &waited})
		seedMirrorChild(t, pool, grandchild, "backlog", member)
		var tokens []claim.Token
		for _, role := range claim.Roles {
			tokens = append(tokens, putWorkerClaim(t, pool, root, member, role, 1, supervise.StateSuspended, supervise.SuspendedByDaemon))
		}

		apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
			Status: "todo", Title: "root", Rank: "A", HandedOver: true}, pnLingering())

		runBranchRows(t, pool, member)
		if starts := startsOf(t, pool, member); len(starts) != 0 {
			t.Fatalf("the starts of %s = %+v, want none: no role of a waiting parent is brought back", member, starts)
		}
		if got := issue(t, pool, member); got.Phase != phase.AwaitingChildren || !sameHeldStart(got.ChildrenWait, &held) {
			t.Fatalf("%s after the re-admission = phase %s, held start %+v; want it waiting still with the start it held", member, got.Phase, got.ChildrenWait)
		}
		if architectStarts(t, pool, root) != 1 {
			t.Fatalf("architect starts of %s = %d, want the promotion's one", root, architectStarts(t, pool, root))
		}
		states := claimStates(t, pool)
		for _, token := range tokens {
			if state := states[string(token)]; state != string(supervise.StateSuspended) {
				t.Fatalf("the claim %s = %q after the re-admission, want it suspended still", token, state)
			}
		}
	})

	// The held list of dispatch://INFRA-2964, on main: a waiting parent in a tree a person moved to
	// the backlog reads its tree's reason, which the ladder reads before awaiting_children.
	t.Run("a waiting parent in a tree on the backlog reads its tree's reason in the held list", func(t *testing.T) {
		const root, member, grandchild = "LEGION-420", "LEGION-421", "LEGION-422"
		pool := migratedPool(t)
		admission := pnAdmission(t, pool, 2)
		putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: root, Tree: root, Phase: phase.Admitted, Generation: 1, Status: "backlog", Rank: "A", HandedOver: true})
		seedLifecycle(t, pool, root)
		parent, waited := root, fixedNow.Add(-2*time.Hour)
		putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: member, Tree: root, Parent: &parent, Phase: phase.AwaitingChildren, Generation: 1, Status: "in_progress", Rank: "M",
			HandedOver: true, ChildrenWait: &record.ChildrenWait{Start: phase.Implementing, Task: "Continue LEGION-421. Issue: LEGION-421. Phase: implementing.",
				Reason: "a review asked for changes, while these Dispatch children of LEGION-421 were open: LEGION-422 (backlog)", Since: waited},
			ChildrenWaitedAt: &waited})
		seedMirrorChild(t, pool, member, "in_progress", root)
		seedMirrorChild(t, pool, grandchild, "backlog", member)

		state := pnState(t, pool, admission)

		if row, listed := pnHeldRow(state, member); !listed || row.Reason != record.HeldTreeParked || !slices.Equal(row.HeldBy, []string{root}) {
			t.Fatalf("admission.held names %s as %+v (listed %t), want tree_parked held by %s", member, row, listed, root)
		}
		pnInNeitherList(t, state, member)
	})

	// The stalled-tree measure of dispatch://INFRA-2560, on main, reads a parent in awaiting_children
	// as it reads a parent whose first phase waits on its children: a wait on a child filed and not
	// released is a wait by design while the project's line can move, and the line moves here
	// because a claim of another tree is at work.
	t.Run("a tree whose root waits only on a child filed and not released, quiet past the measure's bound, raises no stalled notice", func(t *testing.T) {
		const (
			root    = "LEGION-950"
			child   = "LEGION-951"
			working = "LEGION-960"
		)
		pool, admission := awaitingRun(t)
		seedMirrorChild(t, pool, child, "backlog", root)
		seedQuietRoot(t, pool, working, "B", phase.Implementing)
		seedMirror(t, pool, working, "in_progress")
		seedClaim(t, claimStore(t, pool), working, working, claim.RoleImplementer, supervise.StateWorking)
		for pass, roots := range waitPasses(t, pool, admission, nil, root, working) {
			if roots != nil {
				t.Fatalf("pass %d told the controller of %v, want nothing: %s waits by design on its child", pass+1, roots, root)
			}
		}
		if got := issue(t, pool, root); got.Phase != phase.AwaitingChildren {
			t.Fatalf("%s after the passes = %s, want it waiting still", root, got.Phase)
		}
	})
}
