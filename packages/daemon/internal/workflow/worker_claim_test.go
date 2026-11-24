package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// In the plan `implementer tester:a reviewer:a` the tester's and the reviewer's steps are one agent,
// key a, named `tester`, so while step 3 runs the reviewer's phases row names the claim
// `…-tester`. What the engine tells the reviewer reaches that claim: the stuck message of a round
// GitHub will not merge from is a role message to it, and a role message the engine addresses to
// the reviewer is addressed to it. A reviewer's token derived from the role named `…-reviewer`, a
// claim that does not exist, so the message reached no session.
func TestAReviewStuckNoticeNamesTheReviewersAgent(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	keyed := claim.Token(token(t, root.Key, claim.RoleTester))
	reviewer := record.PhaseRow{Issue: root.Key, Role: claim.RoleReviewer, Claim: keyed, StepTask: "a", StepPlace: 3, Entry: 2,
		Decision: &record.ReviewDecision{State: "approved", Head: "head-42"}}
	seedPhase(t, pool, reviewer)

	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		addressed, err := engine.addressRoles(ctx, tx, root.Project, []roleRecipient{{issue: root.Key, role: claim.RoleReviewer, row: root.Key}}, nil)
		if err != nil {
			return err
		}
		if len(addressed) != 1 || addressed[0].token != keyed {
			t.Errorf("a role message to the reviewer is addressed to %+v, want %s", addressed, keyed)
		}
		return engine.tellStuckMergeState(ctx, tx, root, reviewer, round{outcome: roundStuck, cause: stuckMergeState, head: "head-42",
			state: "blocked", reason: "GitHub answers blocked for head head-42"}, "the reviewer")
	})

	var told string
	if err := pool.QueryRow(ctx, `select payload->>'role' from outbox where issue = $1 and kind = 'role_message'`, root.Key).Scan(&told); err != nil {
		t.Fatalf("read the stuck round's role message: %v", err)
	}
	if told != string(keyed) {
		t.Fatalf("the stuck round's role message went to %s, want the reviewer's agent %s", told, keyed)
	}
}

// In the plan `implementer:a tester:a` the tester's step is key a's agent, named `implementer`,
// so while step 2 runs the tester's phases row names the claim `…-implementer`, whose stored role
// is tester. The three reads of a phase worker's claim by the engine reach that claim: the boot's
// resume of a suspended phase owner starts it, the silence watch reads its run, and the catch-up
// of its ready sends it the work of the entry it stands behind. A token derived from the role
// names `…-tester`, a claim that does not exist, so none of the three finds a run.
func TestASuspendedAgentOnItsSecondRoleIsResumedAndWatched(t *testing.T) {
	issue := record.Issue{Key: entryIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: entryIssue, Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true}
	keyed := claim.Token(token(t, entryIssue, claim.RoleImplementer))
	seed := func(t *testing.T, state, suspendedBy string) *pgxpool.Pool {
		t.Helper()
		pool := migratedPool(t)
		seedIssue(t, pool, issue)
		seedSlot(t, pool, record.Slot{Issue: entryIssue, Index: 0})
		seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleImplementer, Claim: keyed, StepTask: "a", StepPlace: 1, Entry: 1})
		seedPhase(t, pool, record.PhaseRow{Issue: entryIssue, Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2,
			Entry: seededEntry, EntryTask: seededEntryTask, EntryRaisedAt: seededRaisedAt})
		if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by, step_at)
			values ($1, 'legion', $2, $2, 'tester', 1, 'ses', '/sessions/ses.jsonl', $3, 0, 0, 0, 0, $4, $5)`,
			string(keyed), entryIssue, state, suspendedBy, silenceStep()); err != nil {
			t.Fatalf("seed key a's claim on its tester step: %v", err)
		}
		seedGivenEntry(t, pool, keyed, seededEntry-1)
		return pool
	}
	sentTheEntry := func(t *testing.T, pool *pgxpool.Pool, what string) {
		t.Helper()
		starts := superviseRowsOf(t, pool, entryIssue, "start")
		if len(starts) != 1 || starts[0].Role != claim.RoleTester || starts[0].Task != seededEntryTask || starts[0].Entry != seededEntry {
			t.Fatalf("%s queued %+v, want the one start of the tester's step on entry %d's work, which the runner gives %s",
				what, starts, seededEntry, keyed)
		}
	}

	t.Run("the boot resumes it", func(t *testing.T) {
		pool := seed(t, "suspended", "daemon")
		engine := testEngine(config.DesignGateRootIssues, nil)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			owners, err := engine.suspendedPhaseOwners(ctx, tx)
			if err != nil {
				return err
			}
			if len(owners.suspended) != 1 || owners.suspended[0].token != keyed {
				t.Errorf("the suspended phase owners = %+v, want the tester's step's agent %s", owners.suspended, keyed)
			}
			return nil
		})
		if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, engine, admissionStub{}); err != nil {
			t.Fatalf("apply the boot: %v", err)
		}
		sentTheEntry(t, pool, "the boot")
	})

	t.Run("the silence watch reads its run", func(t *testing.T) {
		pool := seed(t, "ready", "")
		engine := testEngine(config.DesignGateRootIssues, nil)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			read, err := engine.phaseWorkerRecord(ctx, tx, entryIssue, 1, claim.RoleTester)
			if err != nil {
				return err
			}
			switch {
			case read == nil:
				t.Errorf("the silence watch read nothing, want the run of %s", keyed)
			case read.run.Claim == nil || read.run.Claim.State != "ready":
				t.Errorf("the silence watch read the run %+v, want the run of %s, ready", read.run, keyed)
			}
			return nil
		})
	})

	t.Run("its ready is sent the entry it stands behind", func(t *testing.T) {
		pool := seed(t, "ready", "")
		if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "supervise:tester:2:ready",
			intake.ClaimReady{Issue: entryIssue, Role: claim.RoleTester, Launch: 2},
			testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("apply the ready: %v", err)
		}
		sentTheEntry(t, pool, "the ready")
	})
}

// Version 1 of the plan, `implementer tester reviewer`, ran to its review and was stopped, so the
// claim `…-reviewer` is left from it. Version 2, `implementer tester:a reviewer:a`, names key a
// `tester`, so its agent `…-tester` works the reviewer's step, with the stored role reviewer, and
// the reviewer's phases row names it. The pod was released, so both claims stand released, and
// `…-reviewer` sorts first. A wake of the reviewer's worker wakes the claim that works the step and
// names it on its start row: a CI settlement, and the backstop armed for the reviewer. The other
// claim of the role stays released with no row. While the step's agent runs, the settlement wakes
// nothing, though a claim of the role stands released. A wake that took the first released claim
// of the role would start `…-reviewer` with the settlement's task, take the role's wake and cancel
// its backstop, and so leave the step's agent told nothing and woken by nothing.
func TestAWakeStartsTheWorkerOfTheRolesStep(t *testing.T) {
	agent := claim.Token(token(t, "LEGION-208", claim.RoleTester))
	stale := wakeToken(t, claim.RoleReviewer)
	seed := func(t *testing.T, agentState string) wakeRig {
		t.Helper()
		rig := newWakeRig(t)
		setWakeIssuePhase(t, rig.pool, phase.Reviewing)
		seedReleasedClaim(t, rig.pool, claim.RoleReviewer, "session-reviewer")
		suspendedBy := "released"
		if agentState != "suspended" {
			suspendedBy = ""
		}
		if _, err := rig.pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, suspended_by, serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, 'LEGION-208', 'LEGION-208', 'reviewer', 1, 'session-tester', '', $3, $4, $5, 0, 0, 0, 0)`,
			string(agent), wakeProjectToken(t), agentState, suspendedBy, int64(wakeIssueGeneration)); err != nil {
			t.Fatalf("seed key a's claim on its reviewer step: %v", err)
		}
		seedPhase(t, rig.pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: agent, StepTask: "a", StepPlace: 3, Entry: 1})
		return rig
	}
	wokeTheAgent := func(t *testing.T, rig wakeRig, kind string) {
		t.Helper()
		starts := superviseRowsOf(t, rig.pool, "LEGION-208", "start")
		if len(starts) != 1 || starts[0].Claim != agent || starts[0].Role != claim.RoleReviewer {
			t.Fatalf("the wake queued %+v, want the one start of the reviewer's step naming its agent %s", starts, agent)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 1 || resumes[0] != [2]string{string(claim.RoleReviewer), kind} {
			t.Fatalf("pod resumes = %v, want the one %s wake of the reviewer", resumes, kind)
		}
		stillReleased(t, rig.pool, stale)
	}

	t.Run("a settlement", func(t *testing.T) {
		rig := seed(t, "suspended")
		rig.apply("settled", settlement("head1", "build"))
		wokeTheAgent(t, rig, pods.WakeChecks)
	})

	t.Run("the reviewer's backstop", func(t *testing.T) {
		rig := seed(t, "suspended")
		id := armBackstop(t, rig.pool, claim.RoleReviewer, wakeAt.Add(-1))
		rig.apply("backstop", intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleReviewer})
		wokeTheAgent(t, rig, pods.WakeBackstop)
	})

	t.Run("a settlement while the step's agent runs", func(t *testing.T) {
		rig := seed(t, "working")
		rig.apply("settled", settlement("head1", "build"))
		if starts := superviseRowsOf(t, rig.pool, "LEGION-208", "start"); len(starts) != 0 {
			t.Fatalf("the settlement queued %+v, want no wake: the reviewer's step's agent runs", starts)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
			t.Fatalf("pod resumes = %v, want none", resumes)
		}
		stillReleased(t, rig.pool, stale)
	})
}

// stillReleased fails the test unless token stands suspended by the release and no supervise row
// names it.
func stillReleased(t *testing.T, pool *pgxpool.Pool, token claim.Token) {
	t.Helper()
	var state, by string
	if err := pool.QueryRow(context.Background(), `select state, coalesce(suspended_by, '') from claims where token = $1`, string(token)).Scan(&state, &by); err != nil {
		t.Fatalf("read the claim %s: %v", token, err)
	}
	if state != "suspended" || by != "released" {
		t.Fatalf("the claim %s stands %s by %q, want released", token, state, by)
	}
	var named int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'supervise' and payload->>'claim' = $1`, string(token)).Scan(&named); err != nil {
		t.Fatalf("read the rows naming %s: %v", token, err)
	}
	if named != 0 {
		t.Fatalf("%d supervise rows name %s, want none", named, token)
	}
}
