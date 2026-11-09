package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// startedAgainRepo is the repository every issue of these subtests is recorded on, which
// repositoriesGitHub answers for and testRepositories serves at main.
const startedAgainRepo = "acme/widgets"

// startedAgainDispatch is the Dispatch board the daemon of each subtest reads: the repository
// mapping and the other calls every boot makes (servedBootCall), no issues listed, and every
// other call unavailable, as a Dispatch whose dependencies are down answers.
//
// It stands in for workflowConfig's board, which fails the test on a call it does not model.
// Each start here moves its issue into a phase whose Dispatch status is another, and a status
// write is a call on the board: the outbox's status executor reads the issue first ((*outbox).
// status). Failing the test on that call would fail it on a write that is not what it is about.
// An unavailable answer leaves the row retrying in the board's own lane, which holds back none of
// the supervise rows these subtests read (record's outboxInOrder), and the status repair queues no
// second row while one stands (workflow's correctStatuses).
func startedAgainDispatch(t *testing.T, project string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case servedBootCall(t, w, r, project):
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]any{}); err != nil {
				t.Errorf("write Dispatch issues: %v", err)
			}
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// startedAgainGitHub is GitHub's REST API as the daemon of these subtests calls it:
// repositoriesGitHub's answer for any repository, at main, and the two calls an issue branch's
// create makes — the base branch's ref, and the create itself, answered as GitHub answers a
// branch that is there already, which an issue that has been implementing has. A start of a
// role waits while its issue's branch row is unfinished (record's outboxInOrder), and a
// re-admitted tree's promotion writes one for every member it brings back (admit's
// startMidPhaseMembers), so without these answers the starts this test reads would wait for
// ever. Every other call is answered 404, as repositoriesGitHub answers one.
func startedAgainGitHub(t *testing.T) string {
	t.Helper()
	return githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(rest, "/")
		switch {
		case !isRepository || len(parts) < 2:
		case r.Method == http.MethodGet && len(parts) == 2:
			_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+parts[0]+"/"+parts[1]+"/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+branchBaseCommit+`","type":"commit"}}`)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/repos/"+parts[0]+"/"+parts[1]+"/git/refs":
			status, answer := branchExists()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+parts[0]+"/"+parts[1]+"/pulls":
			_, _ = io.WriteString(w, `[]`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
}

// startedAgainDaemon is the daemon each subtest boots: the rig the stop's own tests use
// (stoppedWorkerDaemon — its own database, a controller tick every stopTickInterval, and a
// runtime that provisions each claim's workspace in the claim's own pod), with the board and the
// GitHub above, a linger no tree of these subtests outlives, and the shipped production check
// bound.
func startedAgainDaemon(t *testing.T) (*pgxpool.Pool, config.Config, overrides, *fake.Runtime) {
	t.Helper()
	pool, cfg, o, rt := stoppedWorkerDaemon(t)
	cfg.DispatchURL = startedAgainDispatch(t, cfg.Project)
	cfg.Linger = time.Hour
	cfg.ProductionCheckBound = 336 * time.Hour
	o.githubAPI = startedAgainGitHub(t)
	return pool, cfg, o, rt
}

// startedAgainEngine is the workflow engine the daemon of cfg runs, built from cfg as the daemon
// builds it (engineConfig, which is where the project's `production_check` becomes the role a
// merge's check is opened for), over the repositories these tests serve. A fact applied through
// it writes what the daemon's own handlers would.
func startedAgainEngine(cfg config.Config) *workflow.Engine {
	engineCfg := engineConfig(cfg, "")
	engineCfg.Repositories = testRepositories
	return workflow.New(record.NewStore(), engineCfg, quietLogger())
}

// startedAgainIssue is a tree member of cfg's project recorded at stands, on the repository these
// subtests work, showing the Dispatch status of the phase it stands in (workflow.PhaseStatus), so
// that nothing before the start has a status to repair. A phase with no status of its own keeps
// whatever the issue shows, and an issue the daemon works shows in_progress, as a root waiting at
// admitted does (boot_slots_test's bootIssue).
func startedAgainIssue(cfg config.Config, key, tree string, stands phase.Phase) record.Issue {
	status := workflow.PhaseStatus(stands)
	if status == "" {
		status = "in_progress"
	}
	issue := record.Issue{Key: key, Repo: startedAgainRepo, DefaultBranch: "main", GitHubRepo: startedAgainRepo,
		Project: cfg.Project, Tree: tree, Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status,
		HandedOver: true, Rank: key, LastDispatchSeq: 5}
	if key != tree {
		parent := tree
		issue.Parent = &parent
	}
	return issue
}

// stoppableWorker is the claim of role on issue as the operator finds it: at work on the task of
// the phase the issue stands in, confirmed in the turn it is running, on a session of its own,
// with the process a launch left and the budgets an earlier run has spent. The confirmed task is
// what makes every pass before the stop write the claim nothing (supervise.Delivery.StillWorked),
// and the spent budgets are what the retry after the start is read against.
func stoppableWorker(cfg config.Config, project string, token claim.Token, tree, issue string, role claim.Role, stands phase.Phase) supervise.Claim {
	now := time.Now().UTC()
	return supervise.Claim{Token: token, Project: project, Tree: tree, Issue: issue, Role: role, Generation: 1,
		State: supervise.StateWorking, Session: "ses_" + string(role), SessionFile: filepath.Join(cfg.StateDir, string(role)+".jsonl"),
		PluginContract: api.DaemonAPIVersion, Locator: workingPane(token),
		Budgets: supervise.Budgets{LaunchFailures: 2, Deaths: 1, PromptFailures: 2, PromptRetires: 1},
		Pending: &supervise.Delivery{ID: "outbox:11", Task: "Carry on.", Generation: 1, Phase: stands,
			QueuedAt: now, DeliveredAt: now, ConfirmedAt: now}}
}

// seedSlottedMember records member as an issue of a tree seedWorkflowTree has already opened,
// holding an admission slot of its own, with worker's claim and the task that claim holds. The
// tree's lifecycle is the root's, which the claim binds to (store.Store.AdmitClaim), so only the
// issue, its slot and the claim are written here.
func seedSlottedMember(t *testing.T, dsn string, member record.Issue, worker supervise.Claim) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, member); err != nil {
			return err
		}
		taken, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		// Above every slot the store holds, as seedWorkflowTree chooses one, so a member seeded
		// after a root takes a free index whatever the root left.
		index := 0
		for _, slot := range taken {
			if slot.Index >= index {
				index = slot.Index + 1
			}
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: member.Key, Index: index, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("record %s and its slot: %v", member.Key, err)
	}
	if _, err := st.AdmitClaim(ctx, worker); err != nil {
		t.Fatalf("put %s: %v", worker.Token, err)
	}
	if worker.Pending != nil {
		if err := st.PutDelivery(ctx, worker.Token, *worker.Pending); err != nil {
			t.Fatalf("put the pending delivery of %s: %v", worker.Token, err)
		}
	}
}

// lingerTree lingers key's tree in the records as its close leaves it: the root done, out of the
// workflow, with a deadline an hour out. That is the state a re-admission takes a tree back from
// (admit's readmit). It is written here as finish writes the end of a phase — the record the
// close would leave, without the close's own rows, whose one worker this subtest has already
// stopped.
func lingerTree(t *testing.T, pool *pgxpool.Pool, records record.Store, key string) {
	t.Helper()
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		issue, err := records.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		issue.Phase, issue.Status, issue.DispatchStatus, issue.LingerUntil = phase.Done, "done", "done", &until
		return records.PutIssue(ctx, tx, *issue)
	}); err != nil {
		t.Fatalf("linger the tree of %s: %v", key, err)
	}
}

// startedAgain reads that start brought the stopped worker back on the claim it kept: one more
// launch of that claim, and that launch a resume of the session the claim recorded rather than a
// fresh pane, the recorded session and session file untouched, and the budgets its earlier run
// had spent back to zero — which is what the retry of a retired claim does and what nothing else
// of the daemon's does (supervise's RequestRetry, through the outbox's retired branch).
//
// The launch carries the issue's repository and that repository's default branch, which is what
// the claim's pod provisions its workspace from: under a runtime that provisions in the pod the
// daemon provisions nothing on its host, so the provision the retired branch makes before the
// relaunch ((*outbox).provisionWorkspace) returns at its first line and the fake records no call
// of its own for it.
func startedAgain(t *testing.T, cfg config.Config, rt *fake.Runtime, kept supervise.Claim, before int, start string) {
	t.Helper()
	token := kept.Token
	testwait.Eventually(t, start+" to start "+string(token)+" again", func() bool {
		return len(launchesOf(rt, token)) > before
	})
	if launches := launchesOf(rt, token); len(launches) != before+1 {
		t.Fatalf("after %s the runtime was asked for %d launches of %s, want the %d before it and one more: a start brings the stopped worker back once",
			start, len(launches), token, before)
	}
	resumes := resumesOf(rt, token)
	if len(resumes) != before+1 {
		t.Fatalf("after %s %s was launched %d times of which %d were resumes, want its launch a resume: a retired claim is relaunched on the session it kept",
			start, token, before+1, len(resumes))
	}
	resume := resumes[before]
	if resume.Spec.ResumeSessionFile != kept.SessionFile {
		t.Fatalf("after %s %s resumed %q, want the session file the claim recorded, %q", start, token, resume.Spec.ResumeSessionFile, kept.SessionFile)
	}
	if got := resume.Spec.Repository.String(); got != startedAgainRepo || resume.Spec.DefaultBranch != "main" {
		t.Fatalf("after %s %s was launched on %s at %q, want %s at main: the launch carries the repository its pod provisions the workspace from",
			start, token, got, resume.Spec.DefaultBranch, startedAgainRepo)
	}
	stored := storedClaim(t, cfg, token)
	if stored.Session != kept.Session || stored.SessionFile != kept.SessionFile {
		t.Fatalf("after %s %s holds session %q in %q, want the session it kept, %q in %q: the start relaunches that session rather than opening another",
			start, token, stored.Session, stored.SessionFile, kept.Session, kept.SessionFile)
	}
	if stored.Budgets != (supervise.Budgets{}) {
		t.Fatalf("after %s %s holds the budgets %+v, want them all back at zero: the retry gives the role a fresh run, and the budgets it had spent were %+v",
			start, token, stored.Budgets, kept.Budgets)
	}
}

// A start of the workflow's own starts a worker the operator stopped. The repair that used to
// undo an operator's `legion claims stop` passes over a retired claim now, and the repair of a
// start that can no longer act ends the row that would have started it (admit's
// resumeSlottedWorkers and finishFencedStarts), so what holds the stop could hold the issue's
// role shut for good. It does not: every start the workflow makes of its own reaches the stopped
// worker, in the session the claim kept, its workspace provisioned again and its budgets fresh.
//
// There are four such starts, and each is a subtest with a daemon and a fixture of its own, since
// the four need the issue in four different states. Each stops its worker over the operator's own
// route, reads that the stop holds over the daemon's passes (stopTheWorker), makes its start, and
// reads the way back (startedAgain): the relaunch is a resume of the recorded session, the
// recorded session is unchanged, and the budgets the earlier run spent are zero again.
//
// The third is the one the change could break. Admission's promotion of a re-admitted root writes
// the member's start as two rows — the relaunch and the task that waits for the issue's slot
// (workflow.StartOrOwe) — and the claim it writes them for is still retired. The passes that
// subtest makes after the promotion therefore meet the repair of a fenced start with the task
// row owed: while the relaunch is still queued the claim is retired and the second queued start
// is what keeps the row, and once the relaunch has run the claim is retired no longer. Which of
// the two a pass meets is the outbox's to decide, and the row must stand either way. That
// subtest makes those passes and reads the row still there, and then reads it delivered.
func TestAWorkflowStartStartsAStoppedWorkerAgain(t *testing.T) {
	t.Run("a backward move into the worker's phase", func(t *testing.T) {
		pool, cfg, o, rt := startedAgainDaemon(t)
		project, err := claim.ProjectToken(cfg.Project)
		if err != nil {
			t.Fatal(err)
		}
		root := cfg.Project + "-1"
		implementer, err := claim.NewToken(project, root, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		// The retro is the implementer's own phase, as implementing is, so the move back is one
		// between two phases of one role: the worker the move starts is the worker the operator
		// stopped (workflow.RoleFor, TestASameRoleBackwardMoveNeverStopsTheWorkerInItsNewPhase).
		kept := stoppableWorker(cfg, project, implementer, root, root, claim.RoleImplementer, phase.Retro)
		seedWorkflowTree(t, cfg.PostgresDSN, project, startedAgainIssue(cfg, root, root, phase.Retro), kept)
		writePrompt(t, cfg, implementer)
		d := startDaemon(t, cfg, o)

		before := stopTheWorker(t, pool, rt, d, root, implementer)

		applyFact(t, pool, "api", "backward:"+root, intake.BackwardMove{Issue: root, Requester: claim.RoleImplementer,
			To: phase.Implementing, Reason: "the retro found work the change still needs"}, startedAgainEngine(cfg))
		if stands := d.state().Issues[root].Phase; stands != phase.Implementing {
			t.Fatalf("%s stands at %s after the move back, want %s: the move is what starts the phase again", root, stands, phase.Implementing)
		}
		startedAgain(t, cfg, rt, kept, before, "the backward move into the worker's phase")
	})

	t.Run("the held phase's retry", func(t *testing.T) {
		pool, cfg, o, rt := startedAgainDaemon(t)
		project, err := claim.ProjectToken(cfg.Project)
		if err != nil {
			t.Fatal(err)
		}
		root := cfg.Project + "-1"
		implementer, err := claim.NewToken(project, root, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		kept := stoppableWorker(cfg, project, implementer, root, root, claim.RoleImplementer, phase.Implementing)
		seedWorkflowTree(t, cfg.PostgresDSN, project, startedAgainIssue(cfg, root, root, phase.Implementing), kept)
		writePrompt(t, cfg, implementer)
		d := startDaemon(t, cfg, o)

		before := stopTheWorker(t, pool, rt, d, root, implementer)

		heldAndRetried(t, pool, d, startedAgainEngine(cfg), root, claim.RoleImplementer)
		if stands := d.state().Issues[root].Phase; stands != phase.Implementing {
			t.Fatalf("%s stands at %s after the retry, want %s: the retry puts the issue back in the phase its worker was stopped in", root, stands, phase.Implementing)
		}
		startedAgain(t, cfg, rt, kept, before, "the architect's retry of the held phase")
	})

	t.Run("the promotion of a re-admitted root", func(t *testing.T) {
		pool, cfg, o, rt := startedAgainDaemon(t)
		// Two slots, and a root of another tree holds one of them from here to the end of the
		// subtest: the re-admitted root takes the other at its promotion, so the member's task row
		// waits for a slot rather than running the moment the outbox wakes. That wait is what
		// gives the passes below a state to act on.
		cfg.AdmissionCap = 2
		project, err := claim.ProjectToken(cfg.Project)
		if err != nil {
			t.Fatal(err)
		}
		root, member, other := cfg.Project+"-1", cfg.Project+"-2", cfg.Project+"-3"
		implementer, err := claim.NewToken(project, member, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		// Both roots stand where a root's own architect works and no phase worker does: at
		// admitted, before the first phase, which is the one root phase that holds a slot
		// (record.AtWork) and which no pass starts a worker for (workflow.RoleFor names none).
		// So the only worker of this fixture is the member's implementer.
		seedWorkflowTree(t, cfg.PostgresDSN, project, startedAgainIssue(cfg, root, root, phase.Admitted))
		seedWorkflowTree(t, cfg.PostgresDSN, project, startedAgainIssue(cfg, other, other, phase.Admitted))
		kept := stoppableWorker(cfg, project, implementer, root, member, claim.RoleImplementer, phase.Implementing)
		seedSlottedMember(t, cfg.PostgresDSN, startedAgainIssue(cfg, member, root, phase.Implementing), kept)
		writePrompt(t, cfg, implementer)
		d := startDaemon(t, cfg, o)

		before := stopTheWorker(t, pool, rt, d, member, implementer)

		// The tree closes and lingers, which is the state a re-admission takes a tree back from,
		// and its members give their slots back.
		lingerTree(t, pool, record.NewStore(), root)
		passed(t, pool, "after the tree closed")
		if held := slotHolders(t, pool); !slices.Equal(held, []string{other}) {
			t.Fatalf("the admission slots are held by %v after the tree closed, want %s alone: a lingering tree's members give their slots back", held, other)
		}

		// Dispatch takes the tree back. Admission re-admits the root, promotes it into the free
		// slot and writes the member's start as the two rows every start of a phase worker is:
		// the relaunch, claimable at once, and the task, which waits for the member's own slot and
		// so waits for the cap (admit's startMidPhaseMembers through workflow.StartOrOwe).
		engine := startedAgainEngine(cfg)
		handlers := []intake.Handler{engine, admit.New(record.NewStore(), engine, cfg.AdmissionCap, cfg.Project, quietLogger())}
		applyFact(t, pool, "dispatch", "todo-again:"+root, intake.DispatchIssue{Key: root, ResolvedRepo: startedAgainRepo,
			Seq: 6, Type: "issue.updated", Status: "todo", Title: root, Rank: root, HandedOver: true}, handlers...)
		// The row the promotion writes for the member's task, spelled as startRowsOf spells a
		// start row: the implementer's, in the phase the member stands in, waiting for the
		// member's own admission slot.
		const owed = "implementer implementing needsSlot=true"
		if rows := startRowsOf(t, pool, member); !slices.Contains(rows, owed) {
			t.Fatalf("after the re-admission %s owes the start rows %v, want %q among them: the promotion writes the member's task as a row of its own",
				member, rows, owed)
		}

		// Three passes over what the re-admission left. What each of them meets is the outbox's
		// to decide, since its lanes are live here: while the relaunch row is still queued a pass
		// meets the retired claim with the task row owed beside that relaunch, which is the state
		// the stop rule weighs and the second queued start is what keeps the row from; once the
		// relaunch has run the claim is no longer retired, and the rule's first term keeps it.
		// The row stands either way, which is what these passes read. The term that keeps it
		// while the relaunch is still queued is locked on its own by the admit row LEGION-710 (a
		// retired claim whose relaunch row is still queued, slot_repair_test.go).
		passed(t, pool, "after the re-admission")
		if rows := startRowsOf(t, pool, member); !slices.Contains(rows, owed) {
			t.Fatalf("three passes after the re-admission %s owes the start rows %v, want %q still among them: the start the promotion wrote is the way back, not a start the stop outlived",
				member, rows, owed)
		}

		startedAgain(t, cfg, rt, kept, before, "the promotion of the re-admitted root")

		// The task the row carries reaches the worker once the member has a slot, which the other
		// root's work ending frees.
		finish(t, pool, record.NewStore(), other)
		testwait.Eventually(t, "the task the promotion owed "+string(implementer)+" to be delivered", func() bool {
			pending := storedClaim(t, cfg, implementer).Pending
			return pending != nil && pending.Phase == phase.Implementing && pending.Generation == 1
		})
	})

	t.Run("a production check's look after the issue's merge", func(t *testing.T) {
		pool, cfg, o, rt := startedAgainDaemon(t)
		// The role the project's production check is worked by is the role this subtest stops.
		cfg.Projects = map[string]config.Project{cfg.Project: {ProductionCheck: claim.RoleTester}}
		project, err := claim.ProjectToken(cfg.Project)
		if err != nil {
			t.Fatal(err)
		}
		root := cfg.Project + "-1"
		tester, err := claim.NewToken(project, root, claim.RoleTester)
		if err != nil {
			t.Fatal(err)
		}
		kept := stoppableWorker(cfg, project, tester, root, root, claim.RoleTester, phase.Testing)
		seedWorkflowTree(t, cfg.PostgresDSN, project, startedAgainIssue(cfg, root, root, phase.Testing), kept)
		writePrompt(t, cfg, tester)
		seedMergeablePullRequest(t, pool, root)
		d := startDaemon(t, cfg, o)

		before := stopTheWorker(t, pool, rt, d, root, tester)

		// The issue reaches the one phase a merge closes it from, as the rest of its run would
		// leave it, and GitHub reports the merge.
		finish(t, pool, record.NewStore(), root)
		applyFact(t, pool, "api", "merged:"+root, intake.PullRequestMerged{Repo: startedAgainRepo, Number: 42,
			MergeSHA: "0bada55e0bada55e0bada55e0bada55e0bada55e"}, startedAgainEngine(cfg))
		testwait.Eventually(t, "the merge to open "+root+"'s production check", func() bool {
			var open bool
			if err := pool.QueryRow(context.Background(),
				`select exists (select 1 from production_checks where issue = $1)`, root).Scan(&open); err != nil {
				t.Fatalf("read whether %s has a production check: %v", root, err)
			}
			return open
		})
		if check := readProductionCheck(t, pool, record.NewStore(), root); check.Role != claim.RoleTester || check.Looks != 1 || !check.LookOpen {
			t.Fatalf("the check the merge opened is %+v, want look 1 open for the %s, the role the project checks production with", check, claim.RoleTester)
		}

		startedAgain(t, cfg, rt, kept, before, "the first look of the production check the merge opened")

		testwait.Eventually(t, "the look's task to reach "+string(tester), func() bool {
			pending := storedClaim(t, cfg, tester).Pending
			return pending != nil && pending.CheckLook.Look == 1
		})
	})
}

// seedMergeablePullRequest records the open pull request of issue that the merge below closes it
// on: the one GitHub reports merged (intake.PullRequestMerged names the repository and number).
func seedMergeablePullRequest(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: issue,
			Repo: startedAgainRepo, Number: 42, Branch: "legion/" + issue, HeadSHA: "head", Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request of %s: %v", issue, err)
	}
}
