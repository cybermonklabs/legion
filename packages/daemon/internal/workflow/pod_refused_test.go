package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// refusedTesterTask is the task the tester's phases row holds for its entry 3 in these tests, and
// refusedTesterClaim the claim of the tester's step whose launch the pod refused: the tester's own,
// since these issues hold no plan rows.
const (
	refusedTesterTask              = "Test the change. Issue: LEGION-209. Phase: testing."
	refusedTesterClaim claim.Token = "legion-legion-legion-209-tester"
)

// reportRefusal applies one report of a launch of claim, the tester's, that LEGION-209's pod pod-1
// refused, under event id.
func reportRefusal(t *testing.T, pool *pgxpool.Pool, engine *Engine, id string, token claim.Token) {
	t.Helper()
	if _, err := intake.ApplyFact(t.Context(), pool, "supervise", id, intake.PodRefusedLaunch{Issue: "LEGION-209", Role: claim.RoleTester,
		Claim: token, Pod: "pod-1", Detail: "the pod pod-1 of LEGION-209 was built without checker"}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the refusal's report: %v", err)
	}
}

// queuedStarts is every supervise start the outbox holds, in order.
func queuedStarts(t *testing.T, pool *pgxpool.Pool) []record.SuperviseRequest {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload from outbox where kind = 'supervise' and payload->>'op' = 'start' order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []record.SuperviseRequest
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var start record.SuperviseRequest
		if err := json.Unmarshal(payload, &start); err != nil {
			t.Fatal(err)
		}
		got = append(got, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// queueStart writes start as a row of LEGION-209's outbox, as the workflow queues one.
func queueStart(t *testing.T, pool *pgxpool.Pool, start record.SuperviseRequest) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		row, err := record.NewOutboxRow("LEGION-209", start, time.Now().UTC())
		if err != nil {
			return err
		}
		return record.NewStore().Enqueue(t.Context(), tx, row)
	})
}

// wantOwedEntryStart fails t unless start is the tester's owed entry 3 of testing, naming the
// tester's claim: the task its phases row stores, marked as a resend of that entry, and marked with
// pod, the pod whose refusal queued it (record.SuperviseRequest.RefusedPod).
func wantOwedEntryStart(t *testing.T, start record.SuperviseRequest, pod string) {
	t.Helper()
	if start.Role != claim.RoleTester || start.Claim != refusedTesterClaim || start.Phase != phase.Testing || start.Entry != 3 ||
		!strings.HasPrefix(start.Task, refusedTesterTask) || !start.ResendEntry || start.RefusedPod != pod {
		t.Fatalf("the queued start is %+v, want the tester's owed entry 3 of testing naming %s, its stored task %q, marked as a resend of that entry and with the refused pod %s",
			start, refusedTesterClaim, refusedTesterTask, pod)
	}
}

// A launch its issue's pod refused is told on the issue and to the controller at each report, and
// the workflow queues a start of the claim wherever no queued start stands that brings the claim
// back once another pod stands, read in the report's own transaction. Which launch the pod
// refused is no part of the rule: an outbox start's own launch, whose row then waits for the pod;
// the turn of a launch the wake pacing held, whose start row had finished when it delivered its
// task; a retry of a launch whose write failed; a crash relaunch; an operator's resume. The
// suspension each ended in retired the task the claim held, so the start queued is the owed
// entry's: the entry and the task text the tester's phases row holds, marked as a resend of that
// entry, so the claim takes that same work again.
//
// A start of the claim's own stands behind it, and so does a start of its role that names no
// claim, which reaches the role's worker; each, run while the pod stands, waits for the pod. A
// wake's start does not: it finishes without acting on a claim the daemon suspended, which every
// claim whose launch its pod refused is. Nor does a start of another claim of the role, or one of
// an earlier run of the issue, which finishes without acting. The report an hour later, while the
// start the first one queued stands, queues none.
func TestALaunchItsPodRefusedQueuesAStartWhereNoQueuedStartBringsTheClaimBack(t *testing.T) {
	token := claim.Token("legion-legion-legion-209-tester")
	for _, tc := range []struct {
		name   string
		queued *record.SuperviseRequest
		queues bool
	}{
		{name: "no start queued", queues: true},
		{name: "the claim's own start", queued: &record.SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleTester, Claim: token,
			Generation: 1, Phase: phase.Testing, Entry: 3, Task: refusedTesterTask}},
		{name: "a start of its role naming no claim", queued: &record.SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleTester,
			Generation: 1, Phase: phase.Testing, Entry: 3, Task: refusedTesterTask}},
		{name: "a wake's start of the claim", queued: &record.SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleTester, Claim: token,
			Generation: 1, WakeKind: pods.WakeChecks}, queues: true},
		{name: "a start of another claim of its role", queued: &record.SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleTester,
			Claim: "legion-legion-legion-209-implementer", Generation: 1}, queues: true},
		{name: "a start of an earlier run", queued: &record.SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleTester, Claim: token}, queues: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedTestingIssue(t, pool, "LEGION-209")
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: token, Entry: 3, EntryTask: refusedTesterTask,
				EntryRaisedAt: time.Now().UTC().Add(-time.Hour)})
			engine := testEngine(config.DesignGateRootIssues, nil)
			seeded := 0
			if tc.queued != nil {
				queueStart(t, pool, *tc.queued)
				seeded = 1
			}

			reportRefusal(t, pool, engine, "pod-refused:legion-legion-legion-209-tester:1", token)
			got := queuedStarts(t, pool)
			if !tc.queues {
				if len(got) != seeded {
					t.Fatalf("the report beside a start that brings the claim back left the starts %+v, want only the one queued before it", got)
				}
			} else {
				if len(got) != seeded+1 {
					t.Fatalf("the report left the starts %+v, want one more than the %d queued before it: nothing queued brings the claim back", got, seeded)
				}
				wantOwedEntryStart(t, got[len(got)-1], "pod-1")
			}
			reportRefusal(t, pool, engine, "pod-refused:legion-legion-legion-209-tester:2", token)
			if again := queuedStarts(t, pool); len(again) != len(got) {
				t.Fatalf("the report an hour later left the starts %+v, want the %d the first report left", again, len(got))
			}
			if posted := messageBodies(t, pool); len(posted) != 2 || !strings.Contains(posted[0], "pod-1") {
				t.Fatalf("the issue was told %v, want one message per report, naming the pod", posted)
			}
			if kinds := controllerNoticeKinds(t, pool); len(kinds) != 2 || kinds[0] != string(record.PodWaitHeldNotice) || kinds[1] != string(record.PodWaitHeldNotice) {
				t.Fatalf("the controller was told %v, want one %s notice per report", kinds, record.PodWaitHeldNotice)
			}
		})
	}
}

// The claim whose start a refused launch queues is the worker of its role's step by the one rule
// every reader of that claim reads (record.WorkerClaim), not the claim the role's phases row
// names: a row whose step a new generation cleared keeps the claim of the agent that last worked
// it, here another key's, and the worker of the role is then the role's own claim, whose launch
// the pod refused. So that claim is sent the owed entry's start, the entry and the task the row
// holds, as a resend.
func TestARefusedLaunchOfTheRolesWorkerIsSentTheOwedEntryWhateverClaimItsRowNames(t *testing.T) {
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: "legion-legion-legion-209-implementer", Entry: 3,
		EntryTask: refusedTesterTask, EntryRaisedAt: time.Now().UTC().Add(-time.Hour)})
	engine := testEngine(config.DesignGateRootIssues, nil)

	reportRefusal(t, pool, engine, "pod-refused:legion-legion-legion-209-tester:1", "legion-legion-legion-209-tester")
	got := queuedStarts(t, pool)
	if len(got) != 1 {
		t.Fatalf("the report queued %+v, want one start", got)
	}
	wantOwedEntryStart(t, got[0], "pod-1")
}

// A refused launch of a claim of a role the deployment no longer defines, or of a claim that runs
// in the slot of such a role whatever its step's role (a keyed agent named for the checker, on the
// tester's step), queues no start: the boot stopped its task, no pod the deployment builds has its
// slot, and the start would wait for one for ever. The issue and the controller are still told.
// The engine's deployment is Legion's own roles, which define no checker.
func TestARefusedLaunchInTheSlotOfARoleTheDeploymentLostQueuesNoStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  claim.Role
		token claim.Token
	}{
		{name: "the checker's own claim", role: "checker", token: "legion-legion-legion-209-checker"},
		{name: "a keyed agent named checker on the tester's step", role: claim.RoleTester, token: "legion-legion-legion-209-checker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedTestingIssue(t, pool, "LEGION-209")
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: tc.token, Entry: 3, EntryTask: refusedTesterTask,
				EntryRaisedAt: time.Now().UTC().Add(-time.Hour)})
			engine := testEngine(config.DesignGateRootIssues, nil)
			if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "pod-refused:1", intake.PodRefusedLaunch{Issue: "LEGION-209", Role: tc.role,
				Claim: tc.token, Pod: "pod-1", Detail: "the pod pod-1 of LEGION-209 was built without checker"}, engine, admissionStub{}); err != nil {
				t.Fatalf("apply the refusal's report: %v", err)
			}
			if got := queuedStarts(t, pool); len(got) != 0 {
				t.Fatalf("the report queued %+v, want no start", got)
			}
			if kinds := controllerNoticeKinds(t, pool); len(kinds) != 1 || kinds[0] != string(record.PodWaitHeldNotice) {
				t.Fatalf("the controller was told %v, want one %s notice", kinds, record.PodWaitHeldNotice)
			}
		})
	}
}

// A refused launch queues no start the outbox would finish without acting, which is the start of
// the role of a stopped task's step (StoppedStepRole) and any start of a member of a lingering
// tree (the daemon outbox's start): such a start would bring the claim back no more than none, and
// its end would leave the claim as it found it. The issue and the controller are still told. The
// twins are queued their start as before: a task stopped at another role's step, whose start of no
// phase reaches the tester, and the same child under a root that does not linger, whose tester is
// sent its owed entry.
func TestARefusedLaunchIsQueuedNoStartTheOutboxWouldFinishWithoutActing(t *testing.T) {
	token := claim.Token("legion-legion-legion-209-tester")
	until := time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	stopped := func(from phase.Phase) func(t *testing.T, pool *pgxpool.Pool) {
		return func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-209",
				Project: "LEGION", Title: "LEGION-209", Phase: phase.Held, Hold: &record.Hold{From: from, Reason: record.HoldTaskStopped}, Generation: 1,
				Status: "in_progress", Rank: "U", HandedOver: true})
		}
	}
	member := func(linger *time.Time) func(t *testing.T, pool *pgxpool.Pool) {
		return func(t *testing.T, pool *pgxpool.Pool) {
			root := "LEGION-208"
			status, at := "in_progress", phase.Implementing
			if linger != nil {
				status, at = "done", phase.Done
			}
			seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION",
				Title: "root", Phase: at, Generation: 1, Status: status, Rank: "U", LingerUntil: linger})
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root,
				Project: "LEGION", Title: "child", Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "V"})
		}
	}
	for _, tc := range []struct {
		name  string
		stand func(t *testing.T, pool *pgxpool.Pool)
		// want checks the starts the report queued; nil wants none.
		want func(t *testing.T, starts []record.SuperviseRequest)
	}{
		{name: "the tester's own step stopped", stand: stopped(phase.Testing)},
		{name: "a member of a lingering tree", stand: member(&until)},
		{name: "twin: a task stopped at the implementer's step", stand: stopped(phase.Implementing), want: func(t *testing.T, starts []record.SuperviseRequest) {
			if len(starts) != 1 || starts[0].Role != claim.RoleTester || starts[0].Claim != token || starts[0].Phase != "" || starts[0].RefusedPod != "pod-1" {
				t.Fatalf("the report queued %+v, want one start of the tester's claim, of no phase, marked with the refused pod pod-1", starts)
			}
		}},
		{name: "twin: a member of a tree that does not linger", stand: member(nil), want: func(t *testing.T, starts []record.SuperviseRequest) {
			if len(starts) != 1 {
				t.Fatalf("the report queued %+v, want one start", starts)
			}
			wantOwedEntryStart(t, starts[0], "pod-1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			tc.stand(t, pool)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: token, Entry: 3, EntryTask: refusedTesterTask,
				EntryRaisedAt: time.Now().UTC().Add(-time.Hour)})
			engine := testEngine(config.DesignGateRootIssues, nil)

			reportRefusal(t, pool, engine, "pod-refused:legion-legion-legion-209-tester:1", token)
			got := queuedStarts(t, pool)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("the report queued %+v, want no start: the outbox would finish it without acting", got)
				}
			} else {
				tc.want(t, got)
			}
			if kinds := controllerNoticeKinds(t, pool); len(kinds) != 1 || kinds[0] != string(record.PodWaitHeldNotice) {
				t.Fatalf("the controller was told %v, want one %s notice", kinds, record.PodWaitHeldNotice)
			}
		})
	}
}

// A refused launch's start names the claim the refusal is of, whichever claim the role's row names.
// In `implementer:a tester:b implementer:b`, standing at step 3, key b's: a refusal of key b's claim
// queues step 3's owed entry naming key b, the entry and the task its row holds, as a resend; one
// of key a's claim, which works no step the task stands at, queues a start of no phase naming key
// a, which resumes it with whatever it holds.
func TestARefusedLaunchQueuesTheStartOfTheClaimItIsOf(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "tester:b", "implementer:b")
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "tester")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleTester, keyB, "test", 1, "head")
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))
	third := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[3]
	if third.Claim != keyB || third.Entry == 0 || third.EntryTask == "" {
		t.Fatalf("premise: step 3's row = %+v, want key b's, raised, with its task", third)
	}
	// The outbox ran the transitions' starts, so no start stands behind a refused launch, the case
	// the report asks a start for; a finished row leaves the outbox.
	if _, err := pool.Exec(t.Context(), `delete from outbox where issue = $1 and kind = 'supervise'`, root.Key); err != nil {
		t.Fatalf("finish the transitions' starts: %v", err)
	}
	refusedStart := func(id string, token claim.Token) record.SuperviseRequest {
		t.Helper()
		before := len(roleStartsOf(t, pool, root.Key, claim.RoleImplementer))
		if _, err := intake.ApplyFact(t.Context(), pool, "supervise", id, intake.PodRefusedLaunch{Issue: root.Key, Role: claim.RoleImplementer,
			Claim: token, Pod: "pod-1", Detail: "the pod pod-1 of LEGION-208 was built without that slot"}, engine, admissionStub{}); err != nil {
			t.Fatalf("apply the refusal of %s: %v", token, err)
		}
		starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
		if len(starts) != before+1 {
			t.Fatalf("the refusal of %s queued %d starts, want one", token, len(starts)-before)
		}
		return starts[len(starts)-1]
	}
	if s := refusedStart("pod-refused:key-b", keyB); s.Claim != keyB || s.Phase != phase.Implementing || s.Entry != third.Entry ||
		!strings.HasPrefix(s.Task, third.EntryTask) || !s.ResendEntry {
		t.Fatalf("the start a refusal of key b queued = %+v, want step 3's owed entry %d naming %s, its task, as a resend", s, third.Entry, keyB)
	}
	if s := refusedStart("pod-refused:key-a", keyA); s.Claim != keyA || s.Phase != "" || s.Entry != 0 || s.ResendEntry {
		t.Fatalf("the start a refusal of key a queued = %+v, want a start of no phase naming %s", s, keyA)
	}
}

// The end of a start row of a claim whose last launch its pod refused (intake.RefusedStartEnded)
// queues the claim's start where none stands, marked with the pod the claim's machine reported
// refusing the claim when the row ended, which the fact carries: the end of that start then asks
// again only once that pod no longer refuses the claim (the daemon outbox's startRowEnded). It
// tells nobody: the refusal's own report did.
func TestAStartTheEndOfARowQueuesIsMarkedWithThePodThatRefusesTheClaim(t *testing.T) {
	token := claim.Token("legion-legion-legion-209-tester")
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: token, Entry: 3, EntryTask: refusedTesterTask,
		EntryRaisedAt: time.Now().UTC().Add(-time.Hour)})
	engine := testEngine(config.DesignGateRootIssues, nil)
	if _, err := intake.ApplyFact(t.Context(), pool, "outbox", "refused-start-ended:legion-legion-legion-209-tester:7", intake.RefusedStartEnded{
		Issue: "LEGION-209", Role: claim.RoleTester, Claim: token, Pod: "pod-2"}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the end of the start row: %v", err)
	}
	got := queuedStarts(t, pool)
	if len(got) != 1 {
		t.Fatalf("the end of the start row queued %+v, want one start", got)
	}
	wantOwedEntryStart(t, got[0], "pod-2")
	if posted, kinds := messageBodies(t, pool), controllerNoticeKinds(t, pool); len(posted) != 0 || len(kinds) != 0 {
		t.Fatalf("the end of the start row told the issue %v and the controller %v, want nobody told", posted, kinds)
	}
}

// seedRefusedClaim stores the tester's claim of LEGION-209 at generation, suspended by the daemon,
// as the claim store holds a claim whose launch at that generation its pod refused, or one that has
// launched since at a later generation.
func seedRefusedClaim(t *testing.T, pool *pgxpool.Pool, token claim.Token, generation uint64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, 'legion', 'LEGION-209', 'LEGION-209', 'tester', $2, 'ses', '/sessions/ses.jsonl', 'suspended', 0, 0, 0, 0, 'daemon')`,
		string(token), int64(generation)); err != nil {
		t.Fatalf("seed the tester's claim: %v", err)
	}
}

// A fact of a refused launch is judged, in the fact's own transaction, against the claim the store
// holds: the fact names the generation of the claim whose launch the pod refused
// (intake.PodRefusedLaunch.Generation, intake.RefusedStartEnded.Generation), and a claim the store
// holds at a later generation has launched since, on the pod that replaced the refusing one. Such a
// fact tells nobody that the claim cannot be started, which is no longer true, and queues no start,
// whose owed entry would hand the running worker its task again. The two orders are set by the
// stored claim, not by time: the claim at the refused launch's generation (the fact came first) is
// told of and sent its owed entry, and the claim a generation on (the launch came first) is not.
// The claim's row is read for update, so a launch whose write comes after the fact's read waits for
// the fact's commit, and the fact's effects stand for a claim that had not launched.
func TestAFactOfARefusedLaunchAfterItsClaimLaunchedTellsNothingAndQueuesNothing(t *testing.T) {
	token := claim.Token("legion-legion-legion-209-tester")
	for _, tc := range []struct {
		name   string
		ended  bool
		stored uint64
		acts   bool
	}{
		{name: "the report comes first: the claim stands at the refused launch's generation", stored: 4, acts: true},
		{name: "the claim launched first: it stands a generation on", stored: 5},
		{name: "the end of a start row comes first", ended: true, stored: 4, acts: true},
		{name: "the claim launched before the end of a start row's fact", ended: true, stored: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedTestingIssue(t, pool, "LEGION-209")
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: token, Entry: 3, EntryTask: refusedTesterTask,
				EntryRaisedAt: time.Now().UTC().Add(-time.Hour)})
			seedRefusedClaim(t, pool, token, tc.stored)
			engine := testEngine(config.DesignGateRootIssues, nil)
			var fact intake.Fact = intake.PodRefusedLaunch{Issue: "LEGION-209", Role: claim.RoleTester, Claim: token, Pod: "pod-1", Generation: 4,
				Detail: "the pod pod-1 of LEGION-209 was built without checker"}
			if tc.ended {
				fact = intake.RefusedStartEnded{Issue: "LEGION-209", Role: claim.RoleTester, Claim: token, Pod: "pod-1", Generation: 4}
			}
			if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "refusal:1", fact, engine, admissionStub{}); err != nil {
				t.Fatalf("apply the fact: %v", err)
			}
			starts, kinds, posted := queuedStarts(t, pool), controllerNoticeKinds(t, pool), messageBodies(t, pool)
			if !tc.acts {
				if len(starts) != 0 || len(kinds) != 0 || len(posted) != 0 {
					t.Fatalf("the fact applied after the claim launched queued %+v, told the controller %v and the issue %v, want nothing", starts, kinds, posted)
				}
				return
			}
			if len(starts) != 1 {
				t.Fatalf("the fact queued %+v, want the owed entry", starts)
			}
			wantOwedEntryStart(t, starts[0], "pod-1")
			if told := len(kinds) == 1 && len(posted) == 1; told == tc.ended {
				t.Fatalf("the fact told the controller %v and the issue %v; want the report to tell both and the end of a start row neither", kinds, posted)
			}
		})
	}
}

// A queued start brings a refused claim back whatever role its payload names, since it reaches the
// claim's token: key k's claim is refused while it holds the tester's role, and the start of its
// next step, the reviewer's, is queued naming that claim. The report reads that start as the claim's
// start and queues none of its own. Read by the tester's run alone, it queued a start of no phase,
// which would have launched the claim with no task.
func TestAQueuedStartOfAnotherRoleOfTheClaimBringsItBack(t *testing.T) {
	token := claim.Token("legion-legion-legion-209-tester")
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: token, Entry: 3, EntryTask: refusedTesterTask,
		EntryRaisedAt: time.Now().UTC().Add(-time.Hour)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleReviewer, Claim: token, Entry: 4, EntryTask: "Review the change.",
		EntryRaisedAt: time.Now().UTC()})
	queueStart(t, pool, record.SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleReviewer, Claim: token, Phase: phase.Reviewing,
		Entry: 4, Task: "Review the change.", Generation: 1})
	engine := testEngine(config.DesignGateRootIssues, nil)

	reportRefusal(t, pool, engine, "pod-refused:legion-legion-legion-209-tester:1", token)
	if got := queuedStarts(t, pool); len(got) != 1 || got[0].Role != claim.RoleReviewer {
		t.Fatalf("after the report the outbox holds the starts %+v, want the reviewer's start of the claim alone: it brings the claim back", got)
	}
}
