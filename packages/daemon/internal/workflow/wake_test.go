package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// wakeAt is when every wake of these tests reaches the daemon, and wakeIssueGeneration the run
// the released pod was serving.
var wakeAt = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

const wakeIssueGeneration = 7

// wakeRig is a released pod the tests wake: a root LEGION-208 implementing at generation 7, its
// pull request sjawhar/legion#42 open at head1, and its implementer and architect claims both
// suspended with the source `released`, which is the state a release leaves a pod in.
type wakeRig struct {
	pool   *pgxpool.Pool
	engine *Engine
	logged *bytes.Buffer
	apply  func(id string, fact intake.Fact) intake.Result
}

func newWakeRig(t *testing.T) wakeRig {
	t.Helper()
	pool := migratedPool(t)
	logged := &bytes.Buffer{}
	clock := &checkClock{now: wakeAt}
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Pods: wakePods(t, pool), Linger: time.Hour, ProductionCheckRole: claim.RoleTester, Clock: clock.Now},
		slog.New(slog.NewTextHandler(logged, nil)))
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: wakeIssueGeneration,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head1", Failing: []string{}})
	seedReleasedClaim(t, pool, claim.RoleImplementer, "session-implementer")
	seedReleasedClaim(t, pool, claim.RoleArchitect, "session-architect")
	return wakeRig{pool: pool, engine: engine, logged: logged, apply: applyFacts(t, pool, engine)}
}

// wakePods is the project's pod records as the daemon hands them to the engine
// (store.Store.PodRecordsOf, over the project key alone): the engine carries no spelling of the
// project of its own, and the store derives the one its claim-keyed tables hold.
func wakePods(t *testing.T, pool *pgxpool.Pool) Pods {
	t.Helper()
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	records, err := st.PodRecordsOf("LEGION")
	if err != nil {
		t.Fatalf("read the project's pod records: %v", err)
	}
	return records
}

// wakeProjectToken is the project as a claim token spells it, which is what `claims.project` and
// the pod tables of the release hold — never the key "LEGION", which only the record's own tables
// hold. The two differ (claim.ProjectToken lower-cases), and a fixture that seeded the key on both
// sides is what hid the engine reading those tables under the key.
func wakeProjectToken(t *testing.T) string {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatalf("the project token: %v", err)
	}
	return project
}

// wakeToken is the claim token of LEGION-208's role, which a role topic is named by.
func wakeToken(t *testing.T, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken(wakeProjectToken(t), "LEGION-208", role)
	if err != nil {
		t.Fatalf("derive the %s token: %v", role, err)
	}
	return token
}

// seedReleasedClaim records one claim of LEGION-208's pod as a release leaves it: suspended, with
// the source `released`, its session kept and the generation it was serving recorded.
func seedReleasedClaim(t *testing.T, pool *pgxpool.Pool, role claim.Role, session string) {
	t.Helper()
	token := wakeToken(t, role)
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, suspended_by, serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $5, 'LEGION-208', 'LEGION-208', $2, 1, $3, '', 'suspended', 'released', $4, 0, 0, 0, 0)`,
		string(token), string(role), session, int64(wakeIssueGeneration), wakeProjectToken(t)); err != nil {
		t.Fatalf("seed the released %s claim: %v", role, err)
	}
}

// claimSuspendedBy re-stamps a released claim's suspension source, which is what every path that
// takes a released claim over does (supervise's suspendedAgain).
func claimSuspendedBy(t *testing.T, pool *pgxpool.Pool, role claim.Role, source string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update claims set suspended_by = $2 where token = $1`,
		string(wakeToken(t, role)), source); err != nil {
		t.Fatalf("take the %s claim over: %v", role, err)
	}
}

// wakeStart is one supervise start row a wake wrote: the role it starts, the task it carries, and
// whether the row says its task holds the daemon's two sentences of the review round
// (record.SuperviseRequest.RoundNamed).
type wakeStart struct {
	Role       claim.Role
	Task       string
	RoundNamed bool
}

// wakeStarts is every supervise `start` row in the outbox, oldest first.
func wakeStarts(t *testing.T, pool *pgxpool.Pool) []wakeStart {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where kind = 'supervise' order by id`)
	if err != nil {
		t.Fatalf("read the supervise rows: %v", err)
	}
	defer rows.Close()
	var starts []wakeStart
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a supervise row: %v", err)
		}
		var request record.SuperviseRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Fatalf("decode a supervise row: %v", err)
		}
		if request.Op != "start" {
			continue
		}
		starts = append(starts, wakeStart{Role: request.Role, Task: request.Task, RoundNamed: request.RoundNamed})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the supervise rows: %v", err)
	}
	return starts
}

// wakeResumes is every pod_resumes row of LEGION-208, oldest first, as (role, kind) pairs.
func wakeResumes(t *testing.T, pool *pgxpool.Pool) [][2]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select role, kind from pod_resumes where issue = 'LEGION-208' order by id`)
	if err != nil {
		t.Fatalf("read the pod resumes: %v", err)
	}
	defer rows.Close()
	var resumes [][2]string
	for rows.Next() {
		var role, kind string
		if err := rows.Scan(&role, &kind); err != nil {
			t.Fatalf("read a pod resume: %v", err)
		}
		resumes = append(resumes, [2]string{role, kind})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the pod resumes: %v", err)
	}
	return resumes
}

// oneWake fails the test unless exactly one start row woke role with a task naming each of names.
func oneWake(t *testing.T, pool *pgxpool.Pool, role claim.Role, kind string, names ...string) {
	t.Helper()
	starts := wakeStarts(t, pool)
	if len(starts) != 1 {
		t.Fatalf("supervise start rows = %+v, want the one wake of the %s", starts, role)
	}
	if starts[0].Role != role {
		t.Fatalf("the wake started %s, want %s", starts[0].Role, role)
	}
	for _, name := range names {
		if !strings.Contains(starts[0].Task, name) {
			t.Fatalf("the woken role's task = %q, want it to name %q", starts[0].Task, name)
		}
	}
	resumes := wakeResumes(t, pool)
	if len(resumes) != 1 || resumes[0] != [2]string{string(role), kind} {
		t.Fatalf("pod resumes = %v, want the one %s wake of the %s", resumes, kind, role)
	}
}

// settlement is one CI settlement of LEGION-208's pull request at head.
func settlement(head string, failing ...string) intake.PullRequestChecks {
	return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, Generation: 1,
		Snapshot: head + "-snapshot", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}, Failing: failing}
}

// laterSettlement is the listener's settlement of head at a later generation of the head's record,
// as it settles a head again when another of its checks finishes. It is newer than settlement's
// (classify.ClassifySettlement), so it applies, and a wake reads it as a settlement of the head
// rather than as a redelivery, which wakes nobody (wakeOnSettlement).
func laterSettlement(head string, generation int64, failing ...string) intake.PullRequestChecks {
	fact := settlement(head, failing...)
	fact.Generation, fact.Snapshot = generation, fmt.Sprintf("%s-snapshot-%d", head, generation)
	return fact
}

// R2: a settlement on the head of the issue's pull request wakes the released phase worker whose
// phase is open, and the woken role's task names the head and what the settlement came to.
func TestACISettlementWakesTheReleasedPhaseWorker(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	rig.apply("settled", settlement("head1", "build"))
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head1", "build")
}

// R2: one head settles again as each of its checks finishes, and each settlement of that head
// wakes the pod at most once. The second settlement is of a later generation, so it applies and is
// turned away by the role's open resume, not as the redelivery of the first.
func TestASecondSettlementOfTheSameHeadWakesThePodNoSecondTime(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	rig.apply("settled", settlement("head1", "build"))
	rig.apply("settled-again", laterSettlement("head1", 2))
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head1")
	if resumes := wakeResumes(t, rig.pool); len(resumes) != 1 {
		t.Fatalf("pod resumes after a second settlement of the same head = %v, want the one", resumes)
	}
}

// R2 and the late-apply rule of dispatch://INFRA-1575: a settlement wakes a released worker only
// when it is one the record applies for the pull request's head now. A settlement of a head the
// pull request has left, or one of the head that is older than the settlement the record holds
// (classify.SettlementStale), wakes nobody, however late it arrives: a webhook delivered late, or a
// message the intake gave up on and replayed. The woken worker's task would name a verdict that is
// no longer the head's. In every case after the first the newer settlement woke the pod, the worker
// came back and its pod was released again, so the late settlement finds the role released with no
// open resume, which is the one state a wake can be taken in. The pull request closed and reopened
// at the head between the two empties its check fence; the late settlement is then weighed against
// the fence the record remembers for the head (classify.SettlementFor), so one that names fewer
// checks than the newer one, or an older commit status, is stale as it is with the fence standing.
func TestASettlementOfAnotherHeadWakesNoReleasedWorker(t *testing.T) {
	t.Parallel()
	ofRun := func(head string, id, generation int64, failing ...string) intake.PullRequestChecks {
		return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, Generation: generation,
			Snapshot: fmt.Sprintf("%s-%d", head, generation), CheckRuns: []record.AttemptRun{{Name: "build", ID: id}}, Failing: failing}
	}
	// wokeBy applies newer, which wakes the implementer with a task naming each of names, and has
	// the worker take the resume up, after which a later idle release leaves its claim released
	// again; reopened then closes and reopens the pull request at head1.
	wokeBy := func(newer intake.PullRequestChecks, reopened bool, names ...string) func(t *testing.T, rig wakeRig) {
		return func(t *testing.T, rig wakeRig) {
			rig.apply("settled", newer)
			oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, names...)
			if _, err := rig.pool.Exec(context.Background(), `update pod_resumes set first_turn_at = $1 where issue = 'LEGION-208'`, wakeAt); err != nil {
				t.Fatalf("take the resume up: %v", err)
			}
			if !reopened {
				return
			}
			rig.apply("closed", intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head1", UpdatedAt: wakeAt.Add(time.Minute)})
			rig.apply("reopened", intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head1",
				URL: "https://github.com/sjawhar/legion/pull/42", UpdatedAt: wakeAt.Add(2 * time.Minute), Reopened: true})
			if pr := latePullRequest(t, rig.pool); pr.State != record.PullRequestOpen || pr.HeadSHA != "head1" || pr.CheckedHead != "" {
				t.Fatalf("after the reopen the pull request is %s at %s, its fence at %q; want it open at head1 with its fence emptied", pr.State, pr.HeadSHA, pr.CheckedHead)
			}
		}
	}
	withLint := ofRun("head1", 5, 2, "lint")
	withLint.CheckRuns = append(withLint.CheckRuns, record.AttemptRun{Name: "lint", ID: 7})
	withStatus := func(generation int64, state record.CommitStatusState, at time.Time) intake.PullRequestChecks {
		settlement := ofRun("head1", 5, generation)
		settlement.Statuses = []record.CommitStatus{{Context: "deploy", State: state, CreatedAt: at}}
		return settlement
	}
	for _, tc := range []struct {
		name string
		// newer is what the record applied before the late settlement.
		newer func(t *testing.T, rig wakeRig)
		// wakes is how many wakes the outbox holds at the end: the newer settlement's own, if any.
		wakes int
		late  intake.PullRequestChecks
	}{
		{name: "a settlement of the head before a push", newer: func(t *testing.T, rig wakeRig) {
			rig.apply("pushed", latePush("head1", "head2", "legion-implementer[bot]"))
			rig.apply("moved", lateSync("head2", wakeAt))
			if pr := latePullRequest(t, rig.pool); pr.HeadSHA != "head2" {
				t.Fatalf("after the push the pull request is at %s, want head2", pr.HeadSHA)
			}
		}, late: ofRun("head1", 1, 1, "build")},
		{name: "a settlement of the head older than the one that woke the pod",
			newer: wokeBy(ofRun("head1", 5, 2), false, "head1", "passed every check"), wakes: 1, late: ofRun("head1", 4, 1, "build")},
		{name: "a settlement of the head naming fewer checks than the one that woke the pod, after a reopen",
			newer: wokeBy(withLint, true, "head1", "lint"), wakes: 1, late: ofRun("head1", 5, 1)},
		{name: "a settlement of the head with an older commit status than the one that woke the pod, after a reopen",
			newer: wokeBy(withStatus(2, record.CommitStatusSuccess, wakeAt.Add(-time.Minute)), true, "head1", "passed every check"),
			wakes: 1, late: withStatus(1, record.CommitStatusFailure, wakeAt.Add(-2*time.Minute))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			tc.newer(t, rig)
			rig.apply("late-settled", tc.late)
			if starts := wakeStarts(t, rig.pool); len(starts) != tc.wakes {
				t.Fatalf("supervise start rows after the late settlement = %+v, want %d: the late settlement wakes nobody", starts, tc.wakes)
			}
		})
	}
}

// seedRelease records one release of LEGION-208's pod as the idle rule and the unowned rule write
// them: roles names the roles it suspended, empty for a release of a pod no claim of the issue can
// use, which suspends none. It answers the row's id, which is what a resume is recorded against.
func seedRelease(t *testing.T, pool *pgxpool.Pool, roles string, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into pod_releases (project, issue, generation, roles, at, bundle_bytes)
		values ($1, 'LEGION-208', $2, $3, $4, 0) returning id`,
		wakeProjectToken(t), int64(wakeIssueGeneration), roles, at).Scan(&id); err != nil {
		t.Fatalf("seed the release of roles %q: %v", roles, err)
	}
	return id
}

// seedOpenResume records a wake of role's own claim that no first turn ever stamped, against
// release: the row that says a pod is coming back for that claim, which the claim's next wake is
// refused by.
//
// seen_release is read here as the take reads it (store's newestPodRelease): the issue's newest
// release of whatever kind at this moment. So a resume seeded before a release is one the heal
// finds stale and a resume seeded after every release there is, is one it leaves alone, which is
// the difference the cases below turn on.
func seedOpenResume(t *testing.T, pool *pgxpool.Pool, release int64, role claim.Role, kind string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into pod_resumes (release_id, project, issue, role, claim, kind, at, seen_release)
		values ($1, $2, 'LEGION-208', $3, $6, $4, $5,
			coalesce((select max(id) from pod_releases where project = $2 and issue = 'LEGION-208'), 0))`,
		release, wakeProjectToken(t), string(role), kind, at, string(wakeToken(t, role))); err != nil {
		t.Fatalf("seed the open resume of the %s: %v", role, err)
	}
}

// openResumes is how many resumes of LEGION-208's role no first turn has stamped and nothing has
// closed: what the one-open-resume index counts, and what a refused wake reads.
func openResumes(t *testing.T, pool *pgxpool.Pool, role claim.Role) int {
	t.Helper()
	var open int
	if err := pool.QueryRow(context.Background(), `select count(*) from pod_resumes
		where issue = 'LEGION-208' and role = $1 and first_turn_at is null and closed_at is null`,
		string(role)).Scan(&open); err != nil {
		t.Fatalf("count the open resumes of the %s: %v", role, err)
	}
	return open
}

// R2 and R5: a wake of a role whose open resume is of a pod that has gone away arrives rather
// than being refused, and the wake after it is refused as any second wake is. The state is the
// one an unowned release leaves (internal/daemon's
// TestAnIssueWithNoLiveClaimHasItsSandboxReleased): the idle release that suspended the
// implementer, a wake of it that no first turn ever stamped, and then the release of the pod no
// claim of the issue could use, which suspends no role and so closes no resume. Without the heal
// that open row refuses every later wake of the role for as long as the issue lives — the
// settlement below queues nothing, and the backstop is spent on the refusal, since podBackstopDue
// fires the row first.
//
// The second wake is what tells a sound heal from one that reads the resume it has just taken as
// stale: the row this wake wrote was written after every release there is, so nothing stands
// above it, and a heal keyed on the release a wake pairs with would close it and queue a second
// start row against R6's one task per wake.
func TestAWakeTakesOverAResumeOfAPodSinceReleased(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		wake func(rig wakeRig)
		kind string
		task string
	}{
		{
			name: "a CI settlement",
			wake: func(rig wakeRig) { rig.apply("settled", settlement("head1", "build")) },
			kind: pods.WakeChecks,
			task: "head1",
		},
		{
			name: "the backstop",
			wake: func(rig wakeRig) {
				rig.apply("backstop", armBackstop(t, rig.pool, wakeToken(t, claim.RoleImplementer), claim.RoleImplementer, wakeAt.Add(-time.Minute)))
			},
			kind: pods.WakeBackstop,
			task: "backstop",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			idle := seedRelease(t, rig.pool, "implementer", wakeAt.Add(-2*time.Hour))
			seedOpenResume(t, rig.pool, idle, claim.RoleImplementer, pods.WakeRoleMessage, wakeAt.Add(-90*time.Minute))
			seedRelease(t, rig.pool, "", wakeAt.Add(-time.Hour))

			tc.wake(rig)

			starts := wakeStarts(t, rig.pool)
			if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || !strings.Contains(starts[0].Task, tc.task) {
				t.Fatalf("supervise start rows = %+v, want the one wake of the implementer naming %q: the resume it met is of a pod the unowned release took away",
					starts, tc.task)
			}
			if open := openResumes(t, rig.pool, claim.RoleImplementer); open != 1 {
				t.Fatalf("open resumes of the implementer = %d, want the one this wake took: the row of the released pod is closed and this wake's own stands", open)
			}
			if resumes := wakeResumes(t, rig.pool); len(resumes) != 2 || resumes[1] != [2]string{string(claim.RoleImplementer), tc.kind} {
				t.Fatalf("pod resumes = %v, want the stale one and then this wake's %s", resumes, tc.kind)
			}

			// The pod this wake brought back is coming, and the next wake of the role writes
			// nothing: not a start row, not a resume, and it does not close the resume that is
			// bringing the pod. The settlement is of a later generation of the head than the first
			// wake of the CI settlement row, so in each row it applies and reaches the take, where a
			// redelivery of that first settlement would wake nobody before it (wakeOnSettlement).
			rig.apply("settled-again", laterSettlement("head1", 2))

			if starts := wakeStarts(t, rig.pool); len(starts) != 1 {
				t.Fatalf("supervise start rows after the second wake = %+v, want the one the first wake queued: the pod is already coming back", starts)
			}
			if open := openResumes(t, rig.pool, claim.RoleImplementer); open != 1 {
				t.Fatalf("open resumes of the implementer after the second wake = %d, want the one the first wake took, untouched", open)
			}
			if resumes := wakeResumes(t, rig.pool); len(resumes) != 2 {
				t.Fatalf("pod resumes after the second wake = %v, want the stale one and the first wake's", resumes)
			}
		})
	}
}

// R2: a wake of a role whose open resume is of the pod now coming back is refused, which is the
// case the heal must not swallow. The resume is the newest fact of the issue in both of these:
// in the first no release followed it, and in the second a release of a pod no claim could use
// came before the wake, which is the state that fools a heal reading the release a wake pairs
// with — that release is roleless, so the pairing passes over it and the resume's own pairing
// release stays older than it for ever.
func TestAWakeIsRefusedWhileThePodOfTheOpenResumeIsComingBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seed func(rig wakeRig)
	}{
		{
			name: "no release since the wake",
			seed: func(rig wakeRig) {
				idle := seedRelease(t, rig.pool, "implementer", wakeAt.Add(-2*time.Hour))
				seedOpenResume(t, rig.pool, idle, claim.RoleImplementer, pods.WakeRoleMessage, wakeAt.Add(-90*time.Minute))
			},
		},
		{
			name: "a release of a pod no claim could use before the wake",
			seed: func(rig wakeRig) {
				idle := seedRelease(t, rig.pool, "implementer", wakeAt.Add(-3*time.Hour))
				seedRelease(t, rig.pool, "", wakeAt.Add(-2*time.Hour))
				seedOpenResume(t, rig.pool, idle, claim.RoleImplementer, pods.WakeRoleMessage, wakeAt.Add(-90*time.Minute))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			tc.seed(rig)

			rig.apply("settled", settlement("head1", "build"))

			if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
				t.Fatalf("supervise start rows = %+v, want none: the pod is already coming back for the implementer", starts)
			}
			if open := openResumes(t, rig.pool, claim.RoleImplementer); open != 1 {
				t.Fatalf("open resumes of the implementer = %d, want the one the wake was refused by, untouched", open)
			}
			if resumes := wakeResumes(t, rig.pool); len(resumes) != 1 {
				t.Fatalf("pod resumes = %v, want the seeded one alone: a refused wake records none", resumes)
			}
		})
	}
}

// R3: a message published to the role topic of a role of the pod wakes that role, whatever the
// publish kept, and the woken role's task says a message was sent to its topic.
func TestARoleMessageWakesTheReleasedRole(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	token := wakeToken(t, claim.RoleImplementer)
	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeRoleMessage, string(token), "role topic")
}

// R3: the same report delivered twice wakes the pod once; the mark is keyed on the listener's own
// report id.
func TestARoleMessageReportedTwiceWakesThePodOnce(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	token := wakeToken(t, claim.RoleImplementer)
	fact := intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"}
	rig.apply("role-message", fact)
	rig.apply("role-message-redelivered", fact)
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeRoleMessage, string(token))
}

// R3: a role message for a released sub-architect wakes the architect, which stays the message's
// owner.
func TestARoleMessageWakesAReleasedArchitect(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	token := wakeToken(t, claim.RoleArchitect)
	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})
	oneWake(t, rig.pool, claim.RoleArchitect, pods.WakeRoleMessage, string(token))
}

// R4: a message sent to the agent subject of a released session wakes the role that kept that
// session, and the woken role's task names the session.
func TestAnAgentMessageWakesTheReleasedSession(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	rig.apply("agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-2"})
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeAgentMessage, "session-implementer", "sent to your session")
}

// R4: a report naming a session no released claim of this daemon's project kept wakes nothing.
// Every daemon consumes the subject, and only the one that owns the session acts.
func TestAnAgentMessageForAnotherDaemonsSessionWakesNothing(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	rig.apply("agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "somebody-elses-session", At: wakeAt, Report: "report-2"})
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows = %+v, want none for another daemon's session", starts)
	}
}

// R7: the listener's liveness report is recorded and wakes nothing.
func TestAListenerReadyReportIsRecordedAndWakesNothing(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	rig.apply("ready", intake.ListenerWake{Kind: intake.ListenerWakeReady, At: wakeAt, Report: "report-3"})
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows = %+v, want none for a liveness report", starts)
	}
	var at time.Time
	if err := rig.pool.QueryRow(context.Background(), `select ready_at from listener_reports where project = $1`, wakeProjectToken(t)).Scan(&at); err != nil {
		t.Fatalf("read the listener's report: %v", err)
	}
	if !at.UTC().Equal(wakeAt) {
		t.Fatalf("the listener's report is stamped %s, want the listener's own %s", at.UTC(), wakeAt)
	}
}

// armBackstop arms one backstop of LEGION-208's claim token, which holds role, due at due, and
// answers the fact of it coming due as the daemon's pass reads it off the row (internal/daemon's
// fireDueBackstops).
func armBackstop(t *testing.T, pool *pgxpool.Pool, token claim.Token, role claim.Role, due time.Time) intake.PodBackstopDue {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into pod_wake_backstops (project, issue, claim, role, release_id, due_at)
		values ($4, 'LEGION-208', $1, $2, 0, $3) returning id`, string(token), string(role), due, wakeProjectToken(t)).Scan(&id); err != nil {
		t.Fatalf("arm the %s backstop of %s: %v", role, token, err)
	}
	return intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Claim: token, Role: role}
}

// R5: the backstop wakes the phase worker it was armed for, and its task says the backstop ran
// out.
func TestTheBackstopWakesThePhaseWorker(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	rig.apply("backstop", armBackstop(t, rig.pool, wakeToken(t, claim.RoleImplementer), claim.RoleImplementer, wakeAt.Add(-time.Minute)))
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeBackstop, "backstop")
}

// R5: a backstop two passes read fires once. The row's conditional fire is what decides it, so
// the second fact changes nothing even under a different event id.
func TestABackstopReadTwiceFiresOnce(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	fact := armBackstop(t, rig.pool, wakeToken(t, claim.RoleImplementer), claim.RoleImplementer, wakeAt.Add(-time.Minute))
	rig.apply("backstop", fact)
	rig.apply("backstop-again", fact)
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeBackstop, "backstop")
	var fired bool
	if err := rig.pool.QueryRow(context.Background(), `select fired from pod_wake_backstops where id = $1`, fact.ID).Scan(&fired); err != nil {
		t.Fatalf("read the backstop: %v", err)
	}
	if !fired {
		t.Fatalf("the backstop is not marked fired")
	}
}

// R5: an architect gets no backstop. OpenRoleAt is OpenRoleOf of an issue at a phase whose running
// step's role is given. It is the role whose live worker claim has a phase reminder and whose
// claim a release or pause's stop arms a backstop for (OpenWorker). It answers none for a phase
// the architect works and none for a phase no role works, and at step the running step's role.
// Admitted and accepting are the two the architect works (phase.RoleFor).
func TestAnArchitectGetsNoBackstop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p    phase.Phase
		step claim.Role
		want claim.Role
	}{
		{name: "implementing", p: phase.Implementing, want: claim.RoleImplementer},
		{name: "testing", p: phase.Testing, want: claim.RoleTester},
		{name: "admitted is the architect's", p: phase.Admitted},
		{name: "accepting is the architect's", p: phase.Accepting},
		{name: "awaiting a person", p: phase.AwaitingPerson},
		{name: "done", p: phase.Done},
		{name: "step is its running step's role", p: phase.Step, step: "checker", want: "checker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OpenRoleAt(tc.p, tc.step); got != tc.want {
				t.Fatalf("OpenRoleAt(%s, %q) = %q, want %q", tc.p, tc.step, got, tc.want)
			}
		})
	}
}

// R8: a linger, a park and a close each take the issue out of the workflow or stop its tree, and
// a wake of any kind then does nothing.
func TestAWakeDoesNothingOnceTheIssueHasLeftTheWorkflow(t *testing.T) {
	t.Parallel()
	lingerUntil := wakeAt.Add(time.Hour)
	for _, tc := range []struct {
		name  string
		issue record.Issue
	}{
		{name: "a park", issue: record.Issue{Status: "backlog", Phase: phase.Implementing}},
		{name: "a close", issue: record.Issue{Status: "done", Phase: phase.Done}},
		{name: "a linger", issue: record.Issue{Status: "in_progress", Phase: phase.Done, LingerUntil: &lingerUntil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			issue := tc.issue
			issue.Key, issue.Tree, issue.Project, issue.Title = "LEGION-208", "LEGION-208", "LEGION", "root"
			issue.Repo, issue.GitHubRepo, issue.DefaultBranch = "sjawhar/legion", "sjawhar/legion", "main"
			issue.Generation, issue.Rank, issue.LastDispatchSeq = wakeIssueGeneration, "U", 2
			seedIssue(t, rig.pool, issue)
			token := wakeToken(t, claim.RoleImplementer)
			rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})
			rig.apply("agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-2"})
			rig.apply("backstop", armBackstop(t, rig.pool, token, claim.RoleImplementer, wakeAt.Add(-time.Minute)))
			if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
				t.Fatalf("supervise start rows after %s = %+v, want none", tc.name, starts)
			}
			if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
				t.Fatalf("pod resumes after %s = %v, want none", tc.name, resumes)
			}
		})
	}
}

// R8: an operator's suspend of a released claim re-stamps its suspension source, and a wake then
// finds no released claim to act on.
func TestAWakeDoesNothingOnceAnOperatorTookTheClaimOver(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	claimSuspendedBy(t, rig.pool, claim.RoleImplementer, "operator")
	token := wakeToken(t, claim.RoleImplementer)
	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})
	rig.apply("settled", settlement("head1", "build"))
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows after an operator suspend = %+v, want none", starts)
	}
}

// R8: a daemon suspension — a linger, a park, a close or a hand-over — takes a released claim
// over the same way, and a wake then does nothing either.
func TestAWakeDoesNothingOnceTheDaemonTookTheClaimOver(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	claimSuspendedBy(t, rig.pool, claim.RoleImplementer, "daemon")
	rig.apply("settled", settlement("head1", "build"))
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows after a daemon suspend = %+v, want none", starts)
	}
}

// R6: every wake's task names the wake, so the woken agent's first turn reads why its pod came
// back without reading anything else.
func TestWakeTaskNamesItsWake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind, detail, want string
	}{
		{kind: pods.WakeChecks, detail: "head1 failed build", want: "head1 failed build"},
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer", want: "role topic"},
		{kind: pods.WakeAgentMessage, detail: "session-1", want: "sent to your session"},
		{kind: pods.WakeBackstop, detail: "the backstop came due", want: "backstop"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			task := WakeTask(tc.kind, tc.detail, StoppedByRelease)
			if !strings.Contains(task, tc.want) || !strings.Contains(task, "released") {
				t.Fatalf("WakeTask(%s) = %q, want it to name the release and %q", tc.kind, task, tc.want)
			}
		})
	}
}

// Decision 16 of dispatch://INFRA-3759: a wake of a session a project's pause stopped says so.
// The project was paused, its pause stopped the session, and the pod, the workspace and the
// unpushed work are as the session left them; the pod was never released. The lines about the
// message that woke it stay.
func TestTheWakeTaskOfASessionAPauseStoppedSaysThePauseStoppedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind, detail, want string
	}{
		{kind: pods.WakeChecks, detail: "head1 failed build", want: "head1 failed build"},
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer", want: "role topic (legion-LEGION-208-implementer) while your session was stopped. Envoy keeps a role message for one hour"},
		{kind: pods.WakeAgentMessage, detail: "session-1", want: "sent to your session (session-1) while your session was stopped. A message that Envoy or Dispatch stored for your session reaches you in this session."},
		{kind: pods.WakeBackstop, detail: "the backstop came due", want: "your backstop running out"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			task := WakeTask(tc.kind, tc.detail, StoppedByPause)
			for _, want := range []string{"Your project was paused, and its pause stopped your session.",
				"Your pod, your workspace and any work you had not pushed are as you left them.", tc.want} {
				if !strings.Contains(task, want) {
					t.Fatalf("WakeTask(%s) after a pause = %q, want it to say %q", tc.kind, task, want)
				}
			}
			if strings.Contains(task, "released") || strings.Contains(task, "pod was gone") {
				t.Fatalf("WakeTask(%s) after a pause = %q, want no word of a release: the pause kept the pod", tc.kind, task)
			}
		})
	}
}

// seedWakePause opens a pause of project LEGION at at, as the pause's own transaction writes it,
// and answers its id.
func seedWakePause(t *testing.T, pool *pgxpool.Pool, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into project_pauses (project, began_at, by_kind, reason)
		values ('LEGION', $1, 'operator', 'the model provider is down') returning id`, at).Scan(&id); err != nil {
		t.Fatalf("pause LEGION: %v", err)
	}
	return id
}

// Decision 15 of dispatch://INFRA-3759: a wake taken while the project is paused is held by the
// pause. Its resume records the pause and pairs with release 0, though the issue has a release
// recorded, and its start row names the wake's kind and the pause. A wake taken once the pause has
// ended is an ordinary wake again.
func TestAWakeTakenWhileThePausedProjectHoldsItRecordsThePauseAndNoRelease(t *testing.T) {
	t.Parallel()
	rig := newWakeRig(t)
	ctx := context.Background()
	if _, err := rig.pool.Exec(ctx, `insert into pod_releases (project, issue, generation, roles, at)
		values ($1, 'LEGION-208', 7, 'architect,implementer', $2)`, wakeProjectToken(t), wakeAt.Add(-time.Hour)); err != nil {
		t.Fatalf("record the release: %v", err)
	}
	pause := seedWakePause(t, rig.pool, wakeAt.Add(-time.Minute))

	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(wakeToken(t, claim.RoleImplementer)), At: wakeAt, Report: "report-1"})

	var release, held int64
	if err := rig.pool.QueryRow(ctx, `select release_id, coalesce(pause_id, 0) from pod_resumes
		where issue = 'LEGION-208' and role = 'implementer'`).Scan(&release, &held); err != nil {
		t.Fatalf("read the implementer's resume: %v", err)
	}
	if release != 0 || held != pause {
		t.Fatalf("the held resume pairs with release %d and pause %d, want release 0 and pause %d", release, held, pause)
	}
	var kind string
	var startPause int64
	if err := rig.pool.QueryRow(ctx, `select payload->>'wakeKind', (payload->>'pause')::bigint from outbox
		where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer'`).Scan(&kind, &startPause); err != nil {
		t.Fatalf("read the implementer's start row: %v", err)
	}
	if kind != pods.WakeRoleMessage || startPause != pause {
		t.Fatalf("the start row names wake %q and pause %d, want %q and pause %d", kind, startPause, pods.WakeRoleMessage, pause)
	}

	if _, err := rig.pool.Exec(ctx, `update project_pauses set ended_at = $2, ended_by_kind = 'operator', ended_by_session = ''
		where id = $1`, pause, wakeAt.Add(time.Minute)); err != nil {
		t.Fatalf("resume LEGION: %v", err)
	}
	rig.apply("role-message-architect", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(wakeToken(t, claim.RoleArchitect)), At: wakeAt.Add(2 * time.Minute), Report: "report-2"})
	if err := rig.pool.QueryRow(ctx, `select release_id, coalesce(pause_id, 0) from pod_resumes
		where issue = 'LEGION-208' and role = 'architect'`).Scan(&release, &held); err != nil {
		t.Fatalf("read the architect's resume: %v", err)
	}
	if release == 0 || held != 0 {
		t.Fatalf("the wake after the resume pairs with release %d and pause %d, want the issue's release and no pause", release, held)
	}
}

// Decision 16 of dispatch://INFRA-3759, through the engine: the task of a wake says the pause
// stopped the session when the role's newest stop is a pause's stop that ended as `stopped` after
// both the issue's newest pod release and the role's last ready, and says the release did once a
// release is recorded after that stop. The same read decides whose wake it is (decision 15): a
// wake of a role the ended pause stopped last is that pause's held wake, which pairs with no
// release, and the wake of a role a later release stopped pairs with that release and names no
// pause. A role whose agent was ready after the pause's stop was launched since, by the resume:
// an idle release that stops it suspends it before its teardown records the release, and a wake
// taken in between is the release's, which names no pause and pairs with the issue's newest
// release.
func TestAWakeOfARoleThePauseStoppedLastIsToldThePauseStoppedIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		releaseAt time.Duration
		// readyAt is when the role's agent was last ready, relative to the wake; 0 for never.
		readyAt time.Duration
		paused  bool
	}{
		{name: "the pause's stop is newer than the release", releaseAt: -2 * time.Hour, paused: true},
		{name: "a release is newer than the pause's stop", releaseAt: -10 * time.Minute, paused: false},
		{name: "the role was ready after the pause's stop, and an idle release stopped it before its teardown",
			releaseAt: -2 * time.Hour, readyAt: -4 * time.Minute, paused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			if _, err := rig.pool.Exec(ctx, `insert into pod_releases (project, issue, generation, roles, at)
				values ($1, 'LEGION-208', 7, 'implementer', $2)`, wakeProjectToken(t), wakeAt.Add(tc.releaseAt)); err != nil {
				t.Fatalf("record the release: %v", err)
			}
			pause := seedWakePause(t, rig.pool, wakeAt.Add(-time.Hour))
			if _, err := rig.pool.Exec(ctx, `insert into project_pause_stops (pause_id, project, claim, issue, role, asked_at, ended_at, ended_as)
				values ($1, 'LEGION', $2, 'LEGION-208', 'implementer', $3, $3, 'stopped')`,
				pause, string(wakeToken(t, claim.RoleImplementer)), wakeAt.Add(-30*time.Minute)); err != nil {
				t.Fatalf("record the pause's stop: %v", err)
			}
			if _, err := rig.pool.Exec(ctx, `update project_pauses set ended_at = $2, ended_by_kind = 'operator', ended_by_session = ''
				where id = $1`, pause, wakeAt.Add(-5*time.Minute)); err != nil {
				t.Fatalf("resume LEGION: %v", err)
			}
			if tc.readyAt != 0 {
				if _, err := rig.pool.Exec(ctx, `update claims set last_ready = $2 where token = $1`,
					string(wakeToken(t, claim.RoleImplementer)), wakeAt.Add(tc.readyAt)); err != nil {
					t.Fatalf("record the implementer's ready after the resume: %v", err)
				}
			}

			rig.apply("agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-1"})

			starts := wakeStarts(t, rig.pool)
			if len(starts) != 1 {
				t.Fatalf("start rows = %+v, want the implementer's one wake", starts)
			}
			want, other := WakeTask(pods.WakeAgentMessage, "session-implementer", StoppedByRelease), WakeTask(pods.WakeAgentMessage, "session-implementer", StoppedByPause)
			if tc.paused {
				want, other = other, want
			}
			if starts[0].Task != want {
				t.Fatalf("the wake's task = %q, want %q (not %q)", starts[0].Task, want, other)
			}
			var release, held int64
			if err := rig.pool.QueryRow(ctx, `select release_id, coalesce(pause_id, 0) from pod_resumes
				where issue = 'LEGION-208' and role = 'implementer'`).Scan(&release, &held); err != nil {
				t.Fatalf("read the implementer's resume: %v", err)
			}
			if tc.paused && (held != pause || release != 0) {
				t.Fatalf("the wake of the role the pause stopped last pairs with release %d and pause %d, want release 0 and pause %d", release, held, pause)
			}
			if !tc.paused && (held != 0 || release == 0) {
				t.Fatalf("the wake of the role a release stopped last pairs with release %d and pause %d, want the release and no pause", release, held)
			}
		})
	}
}

// A role message wake tells the woken role what Envoy kept for it (dispatch://INFRA-2846/spec
// R7): a message kept for the role reaches the woken session as an Envoy card that shows kept_at,
// possibly before the wake's task, unless its hour ended first; Envoy keeps a role message for one
// hour and tells its sender when it drops one; and a message sent with keep: false is not kept, so
// its sender sends it again. Each clause is matched on its own, so a sentence that drops one fails
// and names it. The task says the same whether an idle release or a project's pause stopped the
// session.
func TestTheRoleMessageWakeTaskSaysWhatWasKept(t *testing.T) {
	t.Parallel()
	for _, stop := range []WakeStop{StoppedByRelease, StoppedByPause} {
		task := WakeTask(pods.WakeRoleMessage, "legion-LEGION-208-implementer", stop)
		for _, clause := range []struct{ rule, want string }{
			{rule: "a message kept for the role reaches the woken session as a card", want: "a message kept for your role reaches this session as an Envoy card"},
			{rule: "the card shows kept_at", want: "shows kept_at"},
			{rule: "the card can arrive before the task", want: "possibly before this task"},
			{rule: "a copy whose hour ended before the role came back does not reach it", want: "unless its hour ended before you came back"},
			{rule: "Envoy keeps a role message for one hour", want: "Envoy keeps a role message for one hour"},
			{rule: "Envoy tells the sender when it drops one", want: "Envoy tells its sender when it drops one"},
			{rule: "a message sent with keep: false is not kept", want: "A message sent with keep: false is not kept"},
			{rule: "the sender of such a message sends it again", want: "its sender sends it again"},
		} {
			if !strings.Contains(task, clause.want) {
				t.Errorf("WakeTask(%s, stop %d) = %q, want it to say %s (%q)", pods.WakeRoleMessage, stop, task, clause.rule, clause.want)
			}
		}
		if strings.Contains(task, "which nothing kept") {
			t.Errorf("WakeTask(%s, stop %d) = %q, want no %q: Envoy keeps a role message for the role's next holder", pods.WakeRoleMessage, stop, task, "which nothing kept")
		}
	}
}

// A settlement that wakes a released worker also tells the roles what it says (tellChecks), and
// one of those roles is the very worker it woke. Its message's listener report then arrives while
// the claim is still released, and the mark's key, the wake's own identity, does not hold it: a
// second kind is a second wake. Two things keep R6's one task per wake — the woken role is told no
// message of the settlement that woke it, and a release takes one resume whatever else reaches it
// — so the record names the cause that brought the pod back, `checks`, rather than describing the
// daemon's own settlement to the worker as a message somebody published to its topic. The
// architect is told as before, since no settlement wakes it.
func TestASettlementTellsTheRoleItWokeNoMessageOfItself(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		phase phase.Phase
		woken claim.Role
		// told is every role the settlement must still reach, each exactly once: the roles
		// tellChecks addresses less the one this settlement woke.
		told []claim.Role
	}{
		{"the implementer holds the phase", phase.Implementing, claim.RoleImplementer,
			[]claim.Role{claim.RoleArchitect}},
		{"the tester holds it, and the implementer is told", phase.Testing, claim.RoleTester,
			[]claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			setWakeIssuePhase(t, rig.pool, tc.phase)
			if tc.woken != claim.RoleImplementer {
				seedReleasedClaim(t, rig.pool, tc.woken, "session-"+string(tc.woken))
			}
			rig.apply("settled", settlement("head1", "build"))

			told := roleMessageTokens(t, rig.pool)
			want := make([]string, 0, len(tc.told))
			for _, role := range tc.told {
				want = append(want, string(wakeToken(t, role)))
			}
			slices.Sort(told)
			slices.Sort(want)
			if !slices.Equal(told, want) {
				t.Errorf("the settlement's role messages = %v, want %v: the %s it woke is told by its wake task alone",
					told, want, tc.woken)
			}

			// The listener reports that very message, which is the sequence the defect was in.
			rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole,
				Role: string(wakeToken(t, tc.woken)), At: wakeAt, Report: "report-1"})
			oneWake(t, rig.pool, tc.woken, pods.WakeChecks, "head1", "build")
		})
	}
}

// setWakeIssuePhase moves the rig's issue to the phase whose worker the settlement wakes.
func setWakeIssuePhase(t *testing.T, pool *pgxpool.Pool, p phase.Phase) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update issues set phase = $1 where key = 'LEGION-208'`, string(p)); err != nil {
		t.Fatalf("set the issue's phase to %s: %v", p, err)
	}
}

// roleMessageTokens is the role topic of every role message queued, in the order they were.
func roleMessageTokens(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload->>'role' from outbox where kind = 'role_message' order by id`)
	if err != nil {
		t.Fatalf("read the role messages: %v", err)
	}
	defer rows.Close()
	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatalf("read a role message: %v", err)
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the role messages: %v", err)
	}
	return tokens
}
