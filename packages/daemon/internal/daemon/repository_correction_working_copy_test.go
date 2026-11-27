package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// workingCopyIssue is the issue these cases correct, its implementer the worker whose task is
	// sent across the correction, and workingCopyOwnersLine the line of the owner's text of its
	// task's block that reads like the daemon's working copy line.
	workingCopyIssue      = "LEGION-208"
	workingCopyOwnersLine = " Working copy: branch legion/LEGION-208, directory /owners/own/LEGION-208."
)

// workingCopyRig is one issue's implementer under the parts main wires together for a worker's
// task: the record over a database of its own with the issue's plan, the workflow engine that
// composes the task under the directory the runtime gives the record's repository
// (engineWorkspaceDir), the outbox runner that runs the start it writes, and a claim supervisor
// whose Deps.WorkingCopy is the daemon's own answer (workflowRuntime.workingCopy), as daemon.go
// wires it, and whose every launch is built from the issue's record as it stands at that launch,
// as the daemon's own spec builder builds one (recordSpecs).
type workingCopyRig struct {
	t        *testing.T
	ctx      context.Context
	pool     *store.Pool
	records  record.Store
	workflow *workflowRuntime
	sup      *supervisor
	rt       *fake.Runtime
	runner   *outbox
	conn     *fake.Conn
	token    claim.Token
	runtime  string
	stateDir string
}

func newWorkingCopyRig(t *testing.T, runtimeName string) *workingCopyRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: workingCopyIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: workingCopyIssue, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		version := record.PlanVersion{Issue: issue.Key, Generation: 1, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: mustClaimToken(t, issue.Key, claim.RoleArchitect), GivenAt: time.Now().UTC().Add(-time.Hour)}
		task := record.PlanTask{Issue: issue.Key, Generation: 1, Task: "a", PlanVersion: 2, BlockText: "Build the widget.\n" + workingCopyOwnersLine,
			State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1}}
		steps := []record.PlanStep{
			{Issue: issue.Key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: issue.Key, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
		}
		if err := records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		return records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps)
	}); err != nil {
		t.Fatalf("seed %s with its plan: %v", issue.Key, err)
	}
	stateDir := t.TempDir()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		WorkspaceDir: engineWorkspaceDir(config.Config{Runtime: config.Runtime{Name: runtimeName}, StateDir: stateDir})}, quietLogger())
	w := &workflowRuntime{pool: pool, records: records, engine: engine}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.WorkingCopy = w.workingCopy
	sup.deps.Specs = recordSpecs{read: w.recordedIssue}
	token := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	return &workingCopyRig{t: t, ctx: ctx, pool: pool, records: records, workflow: w, sup: sup, rt: rt, conn: conn,
		token: token, runtime: runtimeName, stateDir: stateDir,
		runner: &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
			tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
			provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: claim.IssueBranch(workingCopyIssue)}, nil
			},
		},
	}
}

// line is the working copy line of a task of the issue on repo (lineOn).
func (r *workingCopyRig) line(repo string) string {
	r.t.Helper()
	return r.lineOn(ghrepo.MustParse(repo))
}

// lineOn is the working copy line of a task of the issue on repo, under the directory the rig's
// runtime places that repository's workspace of the issue in: the pod's under Kubernetes, the
// pane's under tmux. Every phase task opens with it.
func (r *workingCopyRig) lineOn(repo ghrepo.Repository) string {
	r.t.Helper()
	place := func(repo ghrepo.Repository, issue string) (string, error) {
		return tmux.WorkspaceDir(r.stateDir, repo, issue)
	}
	if r.runtime == "kubernetes" {
		place = sandbox.WorkspaceDir
	}
	dir, err := place(repo, workingCopyIssue)
	if err != nil {
		r.t.Fatal(err)
	}
	return "Working copy: branch legion/" + workingCopyIssue + ", directory " + dir + "."
}

// start is the implementer's start the engine writes for the promotion's resume of its step
// (workflow.Engine.StartPromotedWorker), composed from the record as it stands now, on composedOn:
// the task the phases row keeps (record.PhaseRow.EntryTask) and the outbox row carries.
func (r *workingCopyRig) start(composedOn string) entryStartRow {
	r.t.Helper()
	issue := readIssue(r.t, r.pool, workingCopyIssue)
	if err := startPromoted(r.ctx, r.pool, r.workflow.engine, *issue, claim.RoleImplementer); err != nil {
		r.t.Fatalf("write the implementer's start: %v", err)
	}
	rows := roleStartRows(r.t, r.pool, workingCopyIssue, claim.RoleImplementer)
	if len(rows) == 0 {
		r.t.Fatal("the promotion wrote no start of the implementer")
	}
	start := rows[len(rows)-1]
	if task := start.request.Task; !strings.HasPrefix(task, r.line(composedOn)+"\nContinue ") || !strings.Contains(task, workingCopyOwnersLine) {
		r.t.Fatalf("the implementer's start carries %q, want it composed opening with %q on a line of its own before its continue line, and holding the owner's line %q: the case's own premise",
			task, r.line(composedOn), workingCopyOwnersLine)
	}
	return start
}

// run runs the start row as the supervise lane runs it, and waits for every send it started.
func (r *workingCopyRig) run(start entryStartRow) {
	r.t.Helper()
	if err := r.runner.execute(r.ctx, start.row); err != nil {
		r.t.Fatalf("run the implementer's start row %d: %v", start.row.ID, err)
	}
	r.wait()
}

// launch starts the implementer's process, built from the record as it stands now, and makes its
// agent ready.
func (r *workingCopyRig) launch() {
	r.t.Helper()
	machine := startedClaim(r.t, r.sup, claim.RoleImplementer)
	if err := machine.Handle(r.ctx, supervise.RequestSpawn{Claim: r.token}); err != nil {
		r.t.Fatalf("spawn the implementer: %v", err)
	}
	readyClaimOf(r.t, r.sup, r.token, claim.RoleImplementer)
	r.wait()
}

// relaunch is the claim's next launch: its process ends by the daemon's own act, as the update
// round's replacement ends every role of a pod, the claim is launched again on its session, built
// from the record as it stands then, and its agent comes up ready, which sends it the task it
// still holds.
func (r *workingCopyRig) relaunch() {
	r.t.Helper()
	machine := r.machine()
	ended := machine.Claim().Locator
	if ended == nil {
		r.t.Fatal("the implementer records no process to end")
	}
	if err := machine.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: time.Now(), Displaced: true,
	}}); err != nil {
		r.t.Fatalf("end the implementer's process: %v", err)
	}
	r.wait()
	if loc := machine.Claim().Locator; loc == nil || loc.Incarnation == ended.Incarnation {
		r.t.Fatalf("the implementer records the process %+v after the relaunch, want another than %s: the case's own premise", loc, ended.Incarnation)
	}
	readyClaimOf(r.t, r.sup, r.token, claim.RoleImplementer)
	r.wait()
}

func (r *workingCopyRig) machine() *supervise.Machine {
	r.t.Helper()
	machine, found := r.sup.Machine(r.token)
	if !found {
		r.t.Fatalf("%s has no implementer claim", workingCopyIssue)
	}
	return machine
}

func (r *workingCopyRig) wait() {
	if machine, found := r.sup.Machine(r.token); found {
		machine.Wait()
	}
}

// corrects moves the issue's record onto zeta/tools, as the workflow's own pass does when Dispatch
// names another repository for the issue.
func (r *workingCopyRig) corrects() {
	r.t.Helper()
	correctRecord(r.t, r.pool, r.records, workingCopyIssue, "zeta/tools", "release/v9", time.Now())
}

// prompt is the nth prompt the implementer was sent, failing unless it was sent exactly n.
func (r *workingCopyRig) prompt(n int) string {
	r.t.Helper()
	prompts := r.conn.Prompts()
	if len(prompts) != n {
		r.t.Fatalf("the implementer was prompted %+v, want %d prompts", prompts, n)
	}
	return prompts[n-1].Message
}

// wantSent fails unless sent, a prompt of the implementer's task, and the task the claim holds
// after it name the workspace of works in their working copy line and never the workspace of
// other, with the owner's line inside the owner's text as the owner wrote it. The held task opens
// with that line, then a line break and its continue line, and the prompt holds the same: notes
// can stand before the task in a prompt, so the prompt is read for that text anywhere. A send that
// left part of the composed directory after the line would hold text between the line and its
// line break, and fails here.
func (r *workingCopyRig) wantSent(sent, works, other string) {
	r.t.Helper()
	want, wrong := r.line(works), r.line(other)
	opening := want + "\n" + "Continue "
	held := r.machine().Claim().Pending
	if held == nil {
		r.t.Fatal("the implementer holds no task after its send")
	}
	if !strings.HasPrefix(held.Task, opening) {
		r.t.Fatalf("the task the claim holds after its send does not open with %q, its working copy line on a line of its own and then its continue line:\n%s", opening, held.Task)
	}
	for _, read := range []struct{ what, text string }{
		{"the prompt", sent},
		{"the task the claim holds after it", held.Task},
	} {
		if strings.Contains(read.text, wrong) || !strings.Contains(read.text, opening) {
			r.t.Fatalf("%s names the working copy %q, the workspace of %s, which the process it went to does not work in, or holds no %q, the working copy line of the process's workspace followed by a line break and the continue line:\n%s",
				read.what, wrong, other, opening, read.text)
		}
		if !strings.Contains(read.text, "\n"+workingCopyOwnersLine+"\n") {
			r.t.Fatalf("%s holds no owner's line %q inside the owner's text as the owner wrote it:\n%s", read.what, workingCopyOwnersLine, read.text)
		}
	}
}

// A worker's task is composed from the issue's record when its start is written and held until a
// send, and its working copy line tells the worker the directory its work is in. A correction of
// the issue's repository moves the record at once, and the process that receives the task only at
// its next launch: a tmux pane goes on in the workspace it was started in, the only one there is
// until that launch provisions the new repository's, and a pod's roles go on in its clone until the
// update round replaces the pod. So every send writes the line from the repository the receiving
// process was launched on, which its launch recorded (supervise.Claim.WorkingRepository; supervise's
// startSend, Deps.WorkingCopy, the daemon's workingCopy), under each runtime's own directory: a task
// sent before the claim's next launch names the workspace of the repository the issue left, and one
// sent after it names the workspace of the repository the issue moved to. A role launched into a
// live pod after the correction works that pod's clone whatever repository its spec names, so it
// is sent that clone's directory. A claim no launch of this release recorded a repository for is
// sent the line its task was composed with, and a launch that named no repository the directory
// the runtime gives the zero Repository: a tmux pane's `<state_dir>/workspaces/<issue>`. The
// task's owner's text holds a line that reads like the daemon's working copy line, inside its
// fence, and every send leaves it as the owner wrote it.
func TestAWorkerTaskNamesTheWorkspaceOfTheProcessItIsSentTo(t *testing.T) {
	t.Parallel()
	const left, moved = "acme/widgets", "zeta/tools"

	t.Run("tmux: a pane launched before the correction is sent its own workspace until its next launch", func(t *testing.T) {
		r := newWorkingCopyRig(t, "tmux")
		r.launch()
		start := r.start(left)
		r.corrects()

		r.run(start)
		r.wantSent(r.prompt(1), left, moved)

		r.relaunch()
		r.wantSent(r.prompt(2), moved, left)
	})

	t.Run("kubernetes: a process of the pod built from the old repository is sent that pod's workspace until it is relaunched in a new pod", func(t *testing.T) {
		r := newWorkingCopyRig(t, "kubernetes")
		r.rt.ScriptPodRepository(r.token, left)
		r.launch()
		r.corrects()

		r.run(r.start(moved))
		r.wantSent(r.prompt(1), left, moved)

		// The pod the update round builds in the old one's stead is built from the record.
		r.rt.ScriptPodRepository(r.token, moved)
		r.relaunch()
		r.wantSent(r.prompt(2), moved, left)
	})

	t.Run("kubernetes: a role launched into the live pod after the correction is sent that pod's workspace", func(t *testing.T) {
		r := newWorkingCopyRig(t, "kubernetes")
		r.rt.ScriptPodRepository(r.token, left)
		r.corrects()
		r.launch()
		if spawned := r.rt.CallsOf("Spawn"); len(spawned) != 1 || spawned[0].Spec.Repository.String() != moved {
			t.Fatalf("the launch after the correction was %+v, want one spawn built for %s: the case's own premise", spawned, moved)
		}

		r.run(r.start(moved))
		r.wantSent(r.prompt(1), left, moved)
	})

	t.Run("a claim that records no working repository is sent the line as composed", func(t *testing.T) {
		r := newWorkingCopyRig(t, "tmux")
		composed := r.start(left).request.Task
		r.corrects()
		// The claim as the store reads one whose process an earlier release launched: a process,
		// and no repository recorded for it.
		earlier := supervise.Claim{Token: r.token, Project: "legion", Tree: workingCopyIssue, Issue: workingCopyIssue,
			Role: claim.RoleImplementer, State: supervise.StateReady, Locator: &runtime.Locator{
				Runtime: runtime.RuntimeTmux, Claim: r.token, Incarnation: "31001:900001",
				Tmux: &runtime.TmuxLocator{Window: "@3", Pane: "%41"},
			}}
		held := supervise.Delivery{ID: "outbox:7", Task: composed, Phase: phase.Implementing, Generation: 1}
		task, news, err := r.workflow.workingCopy(r.ctx, earlier, held)
		if err != nil || task != composed || news != nil {
			t.Fatalf("the earlier release's claim is sent %q (news %q, error %v), want the task as composed, naming %q:\n%s",
				task, news, err, r.line(left), composed)
		}

		earlier.WorkingRepository = &ghrepo.Repository{}
		task, _, err = r.workflow.workingCopy(r.ctx, earlier, held)
		unnamed := r.lineOn(ghrepo.Repository{})
		if want := unnamed + strings.TrimPrefix(composed, r.line(left)); err != nil || task != want {
			t.Fatalf("a claim whose launch named no repository is sent %q (error %v), want its line to name the pane's workspace of no repository, %q:\n%s",
				task, err, unnamed, want)
		}
	})
}
