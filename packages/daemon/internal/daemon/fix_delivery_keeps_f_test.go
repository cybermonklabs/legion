package daemon

// Every delivery of a fix names the fix's own branch F, so a fix whose pod an update round replaced
// goes on from the commits it pushed to F, whatever the daemon has heard of that push. The
// daemon's parts run here as the pod-update rig composes them (podupdate_test.go): the real
// supervision and its update-round sweep, real machines over a real Postgres, the workflow engine
// each fact and each ready is applied through, and the outbox's own start executor, with the
// runtime faked. The fake runtime records each adoption a machine makes, and each recorded
// adoption is sent, as the frame the worker stream builds of it, to the real shim in a pod made
// with the real git and jj from a bare repository that stands for GitHub.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	workershim "github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// keepsFTesterQ is the head the tester's push of its own branch B leaves, which only a push
	// event carries to the daemon: no frame of the tester's is sent to a real jj here.
	keepsFTesterQ = "5eed00000000000000000000000000000000b0b1"
	// keepsFSecondPod and keepsFThirdPod are the pods the tester comes back in after the one the
	// round's replacement made, once for each push event of its branch.
	keepsFSecondPod = "0b5e8d42-6c1a-4f7e-9d3b-2a8c4e6f1b07"
	keepsFThirdPod  = "7d2c9a1e-3b5f-4e80-a6d4-9c1b8e2f5a63"
	// keepsFAdoptTimeout is the shim's own bound on the commands of one adoption, and keepsFWait
	// the test's bound on the shim's connection and its answer.
	keepsFAdoptTimeout = time.Minute
	keepsFWait         = 2 * time.Minute
)

// keepsFRoles are the claims of the issue's pod: the implementer, whose fix runs, and the tester,
// whose step the issue stands in.
var keepsFRoles = []claim.Role{claim.RoleImplementer, claim.RoleTester}

// keepsFHead is a full commit id, which is the form every head the daemon names takes.
var keepsFHead = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

// keepsFRig is the pod-update rig over the issue in testing, its task a beside fix 1, with a
// machine of each role of keepsFRoles ready in the issue's pod. Its machines adopt a working copy as
// the daemon's do (adoptingAsTheDaemonDoes), drop a task whose phase the issue left
// (workflowRuntime.phaseHolds), and apply each claim's ready through the engine with the daemon's
// own callback (workflowRuntime.claimDurableState). runner is the outbox's start executor over the
// same supervisor, so
// a start the engine queues reaches its claim as the outbox's lane takes it there.
type keepsFRig struct {
	*podUpdateRig
	w       *workflowRuntime
	runner  *outbox
	head    string // X, the head of the task's pull request, where the check failed
	applied int
}

// keepsFStart is one start row the outbox holds for a role, with its request.
type keepsFStart struct {
	row     record.OutboxRow
	request record.SuperviseRequest
}

func newKeepsFRig(t *testing.T, head string) *keepsFRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	dsn := pool.Config().ConnString()
	tokens := map[claim.Role]claim.Token{}
	claims := make([]supervise.Claim, 0, len(keepsFRoles))
	for _, role := range keepsFRoles {
		token := mustClaimToken(t, podUpdateIssue, role)
		tokens[role] = token
		claims = append(claims, supervise.Claim{Token: token, Project: podUpdateProject, Tree: podUpdateIssue, Issue: podUpdateIssue,
			Role: role, Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion, GivenEntry: 1,
			Session: "ses_" + string(role), SessionFile: "/sessions/" + string(role) + ".jsonl",
			Locator: podUpdateLocator(token, podUpdateIssue, podUpdatePod)})
	}
	seedWorkflowTree(t, dsn, podUpdateProject, record.Issue{
		Key: podUpdateIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: podUpdateIssue, Title: "A fix beside the tests", Phase: phase.Testing,
		Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
	}, claims...)

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	clock := &heldClock{at: podUpdateStart}
	rt := fake.NewRuntime()
	rt.Now = clock.Now
	conns := fake.NewConns()
	records := record.NewStore()
	sup := newSupervisor(ctx, st, podUpdateProject, t.TempDir(), quietLogger())
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	w := podUpdateWorkflow(pool, records, clock, log)
	s := &supervision{
		cfg: config.Config{Project: "LEGION", Runtime: config.Runtime{Name: "kubernetes"},
			PodUpdateGrace: podUpdateGrace, WorkerSilence: podUpdateSilence},
		log:     log,
		plan:    plan{clock: clock, project: podUpdateProject, pluginRelaunchable: true, orphanSweep: orphanSweepInterval},
		runtime: rt, supervisor: sup,
		enqueue:         w.enqueueOutbox,
		applySweepFacts: w.applySweepFacts,
		updatePushes:    w.updatePushes,
		refreshGate:     supervise.NewRefreshGate(0),
	}
	rt.RoundPodEnds = s.roundPodEndsAtLaunch
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: conns, Store: st, Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
		ClaimActivity:      st.ClaimActivity,
		Limits:             supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:           supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract:     api.DaemonAPIVersion,
		PluginRelaunchable: true,
		NoPluginInstall:    func(c supervise.Claim) bool { return noPluginInstall(s.cfg, c) },
		RefreshGate:        supervise.NewRefreshGate(0),
		Worker:             store.ForConcurrentWorker,
		PhaseHolds:         w.phaseHolds,
	}
	adoptingAsTheDaemonDoes(sup)
	sup.OnDurableState(w.claimDurableState)
	t.Cleanup(sup.stop)
	stored, err := sup.Claims(ctx)
	if err != nil {
		t.Fatalf("read the seeded claims: %v", err)
	}
	if _, err := sup.restore(ctx, stored); err != nil {
		t.Fatalf("restore the seeded claims: %v", err)
	}
	rig := &podUpdateRig{t: t, ctx: store.ForConcurrentWorker(ctx), s: s, sup: sup, rt: rt, pool: pool, records: records, clock: clock,
		conns: conns, workers: map[claim.Role]*fake.Conn{}, tokens: tokens, st: st, logs: logs, log: log, engine: w.engine,
		listener: &holderPublisher{}, asked: map[int64]record.UpdateAsk{}}
	rig.lane = &outbox{project: podUpdateProject, notices: rig.listener, supervisor: sup, log: log}
	for role, token := range tokens {
		conn := fake.NewConn()
		conns.Register(token, conn)
		rig.workers[role] = conn
	}
	runner := &outbox{log: log, pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, trees: st,
		tokens: outboxTokens{}, project: podUpdateProject, stateDir: t.TempDir(), handlers: w.handlers, now: clock.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: claim.IssueBranch(podUpdateIssue)}, nil
		},
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the daemon's log:\n%s", logs.String())
		}
	})
	return &keepsFRig{podUpdateRig: rig, w: w, runner: runner, head: head}
}

// seed writes the plan of the issue: task a, which fixes its CI, with the steps implementer,
// tester, reviewer and the owner's acceptance, standing at the tester's step; its pull request
// open at the rig's head X; each role's phases row at entry 1; fix 1 of task a for the check k-ci
// failed at X, reading its failed job at the implementer's place; and the tester's start, counted
// before the fix opened and open since.
func (r *keepsFRig) seed() {
	r.t.Helper()
	r.inTx("seed task a beside fix 1", func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: podUpdateIssue, Generation: 1, Version: 1, ArtifactID: "plan-" + podUpdateIssue,
			Source: record.PlanSourceOwner, GivenBy: "owner", GivenAt: podUpdateStart.Add(-time.Hour)}
		tasks := []record.PlanTask{{Issue: podUpdateIssue, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.",
			State: record.TaskWaiting, FixesCI: true}}
		steps := []record.PlanStep{
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 2, Role: "tester", Type: "testing"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 3, Role: "reviewer", Type: "review"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 4, Role: "acceptance", AgentKey: "owner", Type: "review"},
		}
		if err := r.records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		if err := r.records.MoveTask(ctx, tx, podUpdateIssue, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning); err != nil {
			return err
		}
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: podUpdateIssue,
			Repo: "acme/widgets", Number: 42, Branch: claim.IssueBranch(podUpdateIssue), HeadSHA: r.head, Failing: []string{}}); err != nil {
			return err
		}
		for _, role := range keepsFRoles {
			if err := r.records.PutPhase(ctx, tx, record.PhaseRow{Issue: podUpdateIssue, Role: role, Claim: r.tokens[role], Entry: 1,
				EntryTask: "the " + string(role) + "'s step", EntryRaisedAt: podUpdateStart.Add(-time.Hour)}); err != nil {
				return err
			}
		}
		return r.records.PutFix(ctx, tx, record.TaskFix{Issue: podUpdateIssue, Generation: 1, Task: "a", Fix: 1,
			State: record.FixReading, Check: "k-ci", Head: r.head, Place: 1})
	})
	r.seededStart(2, "tester", "testing", claim.RoleTester, "")
}

// apply applies fact through the workflow's handlers under source, as the daemon applies a fact
// of its reads or its intake, and fails the test on an error or a refusal.
func (r *keepsFRig) apply(source string, fact intake.Fact) {
	r.t.Helper()
	r.applied++
	result, err := intake.ApplyFact(r.ctx, r.pool, source, fmt.Sprintf("keeps-f:%d", r.applied), fact, r.w.handlers...)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply %T = %+v, %v", fact, result.Refusal, err)
	}
}

// startsOf is each start of role the outbox holds for the issue, oldest first.
func (r *keepsFRig) startsOf(role claim.Role) []keepsFStart {
	r.t.Helper()
	var starts []keepsFStart
	for _, row := range r.rows(record.OutboxKindSupervise) {
		var request record.SuperviseRequest
		if err := json.Unmarshal(row.Payload, &request); err != nil {
			r.t.Fatalf("decode the supervise row %d: %v", row.ID, err)
		}
		if row.Issue == podUpdateIssue && request.Op == "start" && request.Role == role {
			starts = append(starts, keepsFStart{row: row, request: request})
		}
	}
	return starts
}

// onlyStart waits for a start of role in the outbox and answers it, failing the test where the
// outbox then holds another number of them than one, once each machine is deciding nothing.
func (r *keepsFRig) onlyStart(role claim.Role, why string) keepsFStart {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("a start of the %s: %s", role, why), func() bool { return len(r.startsOf(role)) > 0 })
	r.settleAll()
	starts := r.startsOf(role)
	if len(starts) != 1 {
		r.t.Fatalf("%s: the outbox holds %d starts of the %s, want one: %+v", why, len(starts), role, starts)
	}
	return starts[0]
}

// run runs start through the outbox's start executor and finishes its row, as the outbox's lane
// deletes each row it finishes.
func (r *keepsFRig) run(start keepsFStart) {
	r.t.Helper()
	if err := r.runner.execute(r.ctx, start.row); err != nil {
		r.t.Fatalf("run the %s's start %d: %v", start.request.Role, start.row.ID, err)
	}
	if _, err := r.pool.Exec(r.ctx, "delete from outbox where id = $1", start.row.ID); err != nil {
		r.t.Fatalf("finish the start %d: %v", start.row.ID, err)
	}
}

// fixAdoptions is every adoption the runtime was asked to make in the implementer's process that
// names a fix's working copy, oldest first.
func (r *keepsFRig) fixAdoptions() []fake.Call {
	var made []fake.Call
	for _, call := range r.rt.CallsOf("AdoptWorkingCopy") {
		if call.Locator.Claim == r.tokens[claim.RoleImplementer] && call.Adoption.FixFrom != "" {
			made = append(made, call)
		}
	}
	return made
}

// awaitFixAdoptions waits for the implementer's n-th adoption of a fix's working copy and answers
// every one, holding them to exactly n once each machine is deciding nothing.
func (r *keepsFRig) awaitFixAdoptions(n int, why string) []fake.Call {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("adoption %d of the fix's working copy: %s", n, why), func() bool {
		return len(r.fixAdoptions()) >= n
	})
	r.settleAll()
	made := r.fixAdoptions()
	if len(made) != n {
		r.t.Fatalf("%s: the implementer's fix adoptions are %+v, want %d", why, made, n)
	}
	return made
}

// pushed is what the record holds of each role's push of its branch beside fix 1
// (record.TaskFix.Pushed).
func (r *keepsFRig) pushed() map[string]string {
	r.t.Helper()
	var fix *record.TaskFix
	r.inTx("read fix 1", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fix, err = r.records.FixAt(ctx, tx, podUpdateIssue, 1, "a", 1)
		return err
	})
	if fix == nil {
		r.t.Fatal("the record holds no fix 1 of task a")
	}
	return maps.Clone(fix.Pushed)
}

// testerComesBack is the tester's process ending with its pod and coming back in pod, holding no
// work: its ready gives it the testing phase's resume task through the engine (readyWorkerTask),
// whose row the outbox's executor runs. It answers the heads that task names, and then works the
// turn the task starts and ends it, so the tester holds nothing for its next come-back. The
// tester reports first that nothing of its own runs, so no delivery of what its pod's end took
// stands in front of the task.
func (r *keepsFRig) testerComesBack(pod string) []string {
	r.t.Helper()
	tester := claim.RoleTester
	r.reportsNothingRunning(tester)
	r.displacedInto(tester, pod)
	resume := r.onlyStart(tester, "the tester's ready with no work held")
	if !resume.request.ResumeTask || resume.request.Fix != 0 {
		r.t.Fatalf("the tester's start after its ready is %+v, want its phase's resume task", resume.request)
	}
	r.run(resume)
	conn := r.workers[tester]
	testwait.Eventually(r.t, "the tester's resume task to reach its agent", func() bool { return len(conn.Prompts()) == 1 })
	sent := conn.Prompts()[0]
	r.awaitClaim(tester, "the resume task to be acknowledged", func(c supervise.Claim) bool {
		return c.Pending != nil && c.Pending.ID == sent.DeliveryID && !c.Pending.DeliveredAt.IsZero()
	})
	r.handle(tester, supervise.StreamTurnStart{Claim: r.tokens[tester]})
	r.endTurn(tester, sent.DeliveryID)
	r.awaitClaim(tester, "the resume task's turn to retire it", func(c supervise.Claim) bool { return c.Pending == nil })
	return keepsFHead.FindAllString(sent.Message, -1)
}

// keepsFGit is the issue's repository on GitHub as a bare git repository, origin, its default
// branch main the base and the issue's branch a commit of the task over it, X; and a bare copy of
// it taken before any push of a branch beside the fix, noF, which never holds F.
type keepsFGit struct {
	jj     string
	origin string
	noF    string
	head   string
}

// newKeepsFGit makes the repositories. jj is required: the frames go to a real jj, and a run
// without one fails here rather than skipping.
func newKeepsFGit(t *testing.T) *keepsFGit {
	t.Helper()
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("jj is required: each frame goes to a real jj: %v", err)
	}
	dir := t.TempDir()
	origin, seed, noF := filepath.Join(dir, "widgets.git"), filepath.Join(dir, "seed"), filepath.Join(dir, "widgets-before.git")
	git := fixturegit.Isolated(t, t.TempDir())
	git.Run(dir, fixturegit.InitBare(origin, "main")...)
	git.Run(dir, fixturegit.InitWorkTree(seed, "main")...)
	for _, file := range []struct{ name, body, message string }{
		{"README.md", "widgets\n", "chore: the base"},
		{"feature.txt", "the task's change\n", "feat: the task's change"},
	} {
		if err := os.WriteFile(filepath.Join(seed, file.name), []byte(file.body), 0o644); err != nil {
			t.Fatal(err)
		}
		git.Run(seed, "git", "add", file.name)
		git.Run(seed, "git", "commit", "--quiet", "-m", file.message)
	}
	branch := claim.IssueBranch(podUpdateIssue)
	git.Run(seed, "git", "push", "--quiet", origin, "HEAD~1:refs/heads/main", "HEAD:refs/heads/"+branch)
	git.Run(dir, "git", "clone", "--quiet", "--bare", origin, noF)
	head := git.Run(origin, "git", "rev-parse", "refs/heads/"+branch)
	if before := git.Run(noF, "git", "rev-parse", "refs/heads/"+branch); before != head {
		t.Fatalf("the copy of origin holds the issue's branch at %s, want %s", before, head)
	}
	return &keepsFGit{jj: jj, origin: origin, noF: noF, head: head}
}

// keepsFPod is a pod of the issue as a node provisions it from a repository: a clone that holds
// its default branch and the issue's branch and nothing else of it, colocated with jj, and the
// issue's workspace of it at the issue's branch, LEGION_WORKSPACE, beside which the shim makes the
// fix's working copy (workspace.FixDir).
type keepsFPod struct {
	t         *testing.T
	jj        string
	home      string
	workspace string
	fixDir    string
}

func (g *keepsFGit) pod(t *testing.T, from string) *keepsFPod {
	t.Helper()
	state := t.TempDir()
	clone := filepath.Join(state, "repos", "github.com", "acme", "widgets")
	p := &keepsFPod{t: t, jj: g.jj, home: t.TempDir(),
		workspace: filepath.Join(state, "workspaces", "acme", "widgets", strings.ToLower(podUpdateIssue))}
	p.fixDir = workspace.FixDir(p.workspace)
	for _, dir := range []string{clone, filepath.Dir(p.workspace)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	branch := claim.IssueBranch(podUpdateIssue)
	git := fixturegit.Isolated(t, p.home)
	git.Run(clone, fixturegit.InitWorkTree("", "main")...)
	git.Run(clone, "git", "remote", "add", "origin", from)
	git.Run(clone, "git", "fetch", "--quiet", "origin", "main", branch)
	git.With("JJ_USER=setup", "JJ_EMAIL=setup@example.test").Run(clone, append([]string{g.jj}, fixturegit.JJInit(clone, true)[1:]...)...)
	p.jjIn(clone, "workspace", "add", p.workspace, "--name", filepath.Base(p.workspace), "--revision", branch+"@origin")
	if at := p.commitOf(p.workspace, "@-"); at != g.head {
		t.Fatalf("the pod's workspace stands on %s, want the issue's branch's head %s", at, g.head)
	}
	return p
}

// jjIn runs the real jj in dir on dir's own repository as the test's own identity, failing the
// test where jj fails. It runs in dir, since jj reads the repository of the directory it runs in
// whatever -R names.
func (p *keepsFPod) jjIn(dir string, args ...string) string {
	p.t.Helper()
	command := exec.Command(p.jj, append(args, "-R", dir)...)
	command.Dir = dir
	command.Env = append(fixtureenv.Isolated(p.home), "JJ_USER=setup", "JJ_EMAIL=setup@example.test")
	output, err := command.CombinedOutput()
	if err != nil {
		p.t.Fatalf("jj %v in %s: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (p *keepsFPod) commitOf(dir, rev string) string {
	p.t.Helper()
	return p.jjIn(dir, "log", "-r", rev, "--no-graph", "-T", "commit_id")
}

func (p *keepsFPod) filesOf(dir, rev string) []string {
	p.t.Helper()
	return strings.Fields(p.jjIn(dir, "file", "list", "-r", rev))
}

// adopt runs the real shim on the pod's workspace, as the shim of a pod's role runs there, sends
// it frame, and answers its reply.
func (p *keepsFPod) adopt(frame shimwire.AdoptWorkingCopy) shimwire.AdoptWorkingCopyResult {
	t := p.t
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, _ = workershim.Run(ctx, workershim.Config{
			Network: "tcp", Address: listener.Addr().String(), BootToken: "boot",
			Argv: []string{"/bin/sh", "-c", "exec cat"},
			Env:  append(fixtureenv.Isolated(p.home), "LEGION_WORKSPACE="+p.workspace),
		})
	}()
	defer func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(keepsFWait):
			t.Error("the shim did not leave after its context ended")
		}
	}()
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(keepsFWait)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("the shim's connection: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(keepsFWait)); err != nil {
		t.Fatal(err)
	}
	reader := shimwire.NewReader(bufio.NewReader(conn))
	if line, err := reader.ReadLine(); err != nil {
		t.Fatalf("the shim's hello: %v", err)
	} else if hello, err := shimwire.Decode(line); err != nil || hello.FrameType() != shimwire.TypeHello2 {
		t.Fatalf("the shim opened with %v (%v)", hello, err)
	}
	writer := shimwire.NewWriter(conn)
	if err := writer.WriteFrame(shimwire.HelloAck{}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	for {
		line, err := reader.ReadLine()
		if err != nil {
			t.Fatalf("read the shim's answer to the adoption: %v", err)
		}
		if answer, err := shimwire.Decode(line); err == nil {
			if result, ok := answer.(shimwire.AdoptWorkingCopyResult); ok && result.ID == frame.ID {
				return result
			}
		}
	}
}

// keepsFCopy is what an adoption left in a pod: the shim's answer, and, where it made the fix's
// working copy where it said, the commit under that copy and that commit's files.
type keepsFCopy struct {
	answer shimwire.AdoptWorkingCopyResult
	parent string
	files  []string
}

// adoptInto sends frame to the pod's shim and answers what the adoption left.
func (p *keepsFPod) adoptInto(frame shimwire.AdoptWorkingCopy) keepsFCopy {
	p.t.Helper()
	made := keepsFCopy{answer: p.adopt(frame)}
	if made.answer.OK && made.answer.FixDir == p.fixDir {
		made.parent, made.files = p.commitOf(p.fixDir, "@-"), p.filesOf(p.fixDir, "@-")
	}
	return made
}

// standsOn is what made lacks of a fix's working copy made where it should be, standing on
// parent, with fix.txt there where withFix says and without it where not; nil where it lacks
// nothing.
func (made keepsFCopy) standsOn(fixDir, parent string, withFix bool) []string {
	var problems []string
	if !made.answer.OK || made.answer.FixDir != fixDir {
		return []string{fmt.Sprintf("the shim answered %+v, want ok with the fix's working copy %s", made.answer, fixDir)}
	}
	if made.parent != parent {
		problems = append(problems, fmt.Sprintf("the fix's working copy stands on %s, want %s", made.parent, parent))
	}
	if has := slices.Contains(made.files, "fix.txt"); has != withFix {
		problems = append(problems, fmt.Sprintf("the commit under the fix's working copy holds %v, want fix.txt there %t", made.files, withFix))
	}
	return problems
}

// keepsFFrame is the frame the worker stream sends for an adoption the runtime was asked to make, as
// stream.Conn.AdoptWorkingCopy builds it: the identity the adoption names, the shim's bound, and
// the adoption's members.
func keepsFFrame(t *testing.T, id string, call fake.Call) shimwire.AdoptWorkingCopy {
	t.Helper()
	frame := shimwire.AdoptWorkingCopy{ID: id, JJUser: call.Identity.Name, JJEmail: call.Identity.Email,
		TimeoutMs: int(keepsFAdoptTimeout.Milliseconds()), FixFrom: call.Adoption.FixFrom, MoveTo: call.Adoption.MoveTo,
		FixBranch: call.Adoption.FixBranch}
	if err := frame.Validate(); err != nil {
		t.Fatalf("the frame of the adoption %+v: %v", call.Adoption, err)
	}
	return frame
}

// keepsFCase is one way a fix's delivery reaches the pod an update round put in place of its own.
// interrupted is a turn of the fix's task that the round's bound cut, so the fix's claim still
// holds that task at its ready; else the fix's turn ended before the round and the claim holds no
// work at its ready. delayed is the push event of F arriving after the ready; else it never
// arrives. recorded runs the daemon with the engine's seam that names F only where a push of it is
// recorded (workflow.(*Engine).FixBranchFromRecordedPush).
type keepsFCase struct {
	interrupted, delayed, recorded bool
}

func (c keepsFCase) String() string {
	way := "a ready with no held work"
	if c.interrupted {
		way = "an interrupted turn"
	}
	event := "the push event of F absent"
	if c.delayed {
		event = "the push event of F after the ready"
	}
	name := way + ", " + event
	if c.recorded {
		name = "control: F named only where a push of it is recorded, " + name
	}
	return name
}

// An update round replaces the pod of an issue whose fix runs beside its tester. The fix pushed
// its own branch F at P before the round ended, at the round's ask, and the daemon has heard
// nothing of that push when the fix's claim comes back ready in the new pod: the push event comes
// after the ready, or never. Every delivery of the fix carries F all the same:
//
//   - an interrupted turn: the round's bound cut the turn of the fix's task, so the claim still
//     holds that task, and its ready gives it nothing new (readyWorkerTask returns at
//     supervise.HoldsWork); the task the fix's start delivered, which named F, is sent again;
//   - a ready with no held work: the fix's turn had ended, and its ready sends the fix's task again
//     through the engine (resumeFix), which names F before any push event.
//
// Each adoption the fix's claim was given, before the round and in the new pod, names F, and each
// is sent as its frame to the real shim of a fresh pod: where origin holds F the fix's working
// copy stands on P with the fix's fix.txt, and the same frame where origin holds no F makes the
// copy at X. Beside the fix, the tester, which never pushed its branch B, is told no pushed head at
// its ready; a push event of B at Q tells it Q at the next, and the event of B's deletion takes Q
// out of the record and out of the next task, leaving the fix's own push as it was.
//
// The controls, kept: the same cases with the engine naming F only where a push of it is recorded,
// as the daemon did before. Each delivery then names no F, and the frame in a fresh pod whose origin
// holds F makes the copy at X without fix.txt: the fix's pushed work is lost, which the real jj
// shows.
func TestEveryFixDeliveryKeepsFThroughAnUpdateRound(t *testing.T) {
	t.Parallel()
	for _, recorded := range []bool{false, true} {
		for _, interrupted := range []bool{true, false} {
			for _, delayed := range []bool{true, false} {
				c := keepsFCase{interrupted: interrupted, delayed: delayed, recorded: recorded}
				t.Run(c.String(), func(t *testing.T) {
					t.Parallel()
					keepsFThroughARound(t, c)
				})
			}
		}
	}
}

// keepsFThroughARound drives one case of TestEveryFixDeliveryKeepsFThroughAnUpdateRound, on a rig and
// repositories of its own.
func keepsFThroughARound(t *testing.T, tc keepsFCase) {
	g := newKeepsFGit(t)
	r := newKeepsFRig(t, g.head)
	r.seed()
	implementer, tester := claim.RoleImplementer, claim.RoleTester
	fixBranch, testerBranch := besideBranch(t, implementer), besideBranch(t, tester)
	named := fixBranch
	if tc.recorded {
		r.engine.FixBranchFromRecordedPush()
		named = ""
	}

	// The read of fix 1's failed job starts the fix: its start names X and F, and the outbox's
	// executor delivers it to the implementer's ready claim, adopting the fix's working copy first.
	r.apply("outbox", intake.FixJobRead{Key: podUpdateIssue, Generation: 1, Task: "a", Fix: 1, Name: "unit", URL: "https://ci.example/run/7"})
	start := r.onlyStart(implementer, "the fix's start")
	if start.request.Fix != 1 || start.request.FixFrom != g.head || start.request.FixBranch != named {
		t.Errorf("the fix's start names fix %d from %q on %q, want fix 1 from %s on %q", start.request.Fix,
			start.request.FixFrom, start.request.FixBranch, g.head, named)
	}
	r.run(start)
	conn := r.workers[implementer]
	testwait.Eventually(t, "the fix's task to reach its agent", func() bool { return len(conn.Prompts()) == 1 })
	first := r.awaitFixAdoptions(1, "the fix's start")[0]
	task := conn.Prompts()[0]
	r.awaitClaim(implementer, "the fix's task to be acknowledged", func(c supervise.Claim) bool {
		return c.Pending != nil && c.Pending.ID == task.DeliveryID && !c.Pending.DeliveredAt.IsZero()
	})

	// The pod before the round: the adoption makes the fix's copy at X, since origin holds no F
	// yet; the fix commits fix.txt there, and at the round's ask pushes F with the commands the
	// ask's text gives it. P is where origin then holds F.
	before := g.pod(t, g.origin)
	if problems := before.adoptInto(keepsFFrame(t, "adopt-first", first)).standsOn(before.fixDir, g.head, false); len(problems) != 0 {
		t.Fatalf("the adoption before the fix's task, in the pod before the round: %s", strings.Join(problems, "; "))
	}
	if err := os.WriteFile(filepath.Join(before.fixDir, "fix.txt"), []byte("the fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before.jjIn(before.fixDir, "commit", "-m", "fix: the failure")
	before.jjIn(before.fixDir, "bookmark", "set", fixBranch, "-r", "@-")
	before.jjIn(before.fixDir, "git", "push", "--remote", "origin", "--bookmark", fixBranch)
	p := fixturegit.Isolated(t, t.TempDir()).Run(g.origin, "git", "rev-parse", "refs/heads/"+fixBranch)
	if p == g.head {
		t.Fatalf("origin holds F at X %s, want the fix's own commit", p)
	}

	// The fix's turn: cut by the round below, or over before it.
	r.handle(implementer, supervise.StreamTurnStart{Claim: r.tokens[implementer]})
	if !tc.interrupted {
		r.endTurn(implementer, task.DeliveryID)
		r.awaitClaim(implementer, "the fix's turn to retire its task", func(c supervise.Claim) bool { return c.Pending == nil })
		r.reportsNothingRunning(implementer)
	}

	// The round: the fix is asked to push F and the tester B, and the pod is replaced at the
	// round's bound, whatever either answered.
	r.drifts(podUpdateDrift)
	for _, role := range keepsFRoles {
		r.awaitAsked(role, podUpdateRound)
	}
	asked := map[claim.Role]supervise.UpdatePush{}
	for _, ask := range r.asks() {
		asked[ask.Role] = pushOf(ask)
	}
	if want := (supervise.UpdatePush{Branch: fixBranch, OwnFix: true}); asked[implementer] != want {
		t.Errorf("the round asked the fix to push %+v, want %+v", asked[implementer], want)
	}
	if want := (supervise.UpdatePush{Branch: testerBranch}); asked[tester] != want {
		t.Errorf("the round asked the tester to push %+v, want %+v", asked[tester], want)
	}
	r.clockAt(podUpdateBound)
	r.awaitReplacement(1, "at the round's bound")
	r.drifts()
	for _, role := range keepsFRoles {
		r.awaitClaim(role, "its round to end with the replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	if held := r.pushed(); len(held) != 0 {
		t.Fatalf("before the fix's ready the record holds the pushes %v, want none: the daemon has heard of no push", held)
	}

	// The fix in the new pod.
	r.displacedInto(implementer, podUpdateReplacementPod)
	var second fake.Call
	if tc.interrupted {
		second = r.awaitFixAdoptions(2, "the cut task sent again in the new pod")[1]
		if starts := r.startsOf(implementer); len(starts) != 0 {
			t.Errorf("the ready of the fix's claim holding its task queued %+v, want nothing: the held task is the work", starts)
		}
		held := r.claimOf(implementer).Pending
		if held == nil || held.Fix != 1 || held.FixFrom != g.head || held.FixBranch != named || held.Task != start.request.Task {
			t.Errorf("in the new pod the fix's claim holds %+v, want the fix's own task again, fix 1 from %s on %q", held, g.head, named)
		}
	} else {
		resume := r.onlyStart(implementer, "the ready of the fix's claim holding no work")
		if !resume.request.ResumeTask || resume.request.Fix != 1 || resume.request.FixFrom != g.head || resume.request.FixBranch != named {
			t.Errorf("the fix's resume names fix %d from %q on %q (resume task %t), want fix 1 from %s on %q",
				resume.request.Fix, resume.request.FixFrom, resume.request.FixBranch, resume.request.ResumeTask, g.head, named)
		}
		r.run(resume)
		second = r.awaitFixAdoptions(2, "the fix's resume in the new pod")[1]
	}
	if tc.delayed {
		r.apply("github", intake.BasePush{Repo: "acme/widgets", Branch: fixBranch, After: p})
		if held := r.pushed(); len(held) != 1 || held[string(implementer)] != p {
			t.Fatalf("after the push event of F the record holds the pushes %v, want the fix's at %s alone", held, p)
		}
	}

	// Each frame, sent to a fresh pod: where origin holds F, the copy stands on P with fix.txt;
	// where it holds none, the same frame makes the copy at X.
	for i, call := range []fake.Call{first, second} {
		if call.Adoption.FixFrom != g.head || call.Adoption.FixBranch != named || call.Adoption.MoveTo != "" {
			t.Errorf("adoption %d names %+v, want the fix's working copy from %s on %q", i+1, call.Adoption, g.head, named)
		}
		frame := keepsFFrame(t, fmt.Sprintf("adopt-%d", i+1), call)
		after := g.pod(t, g.origin)
		parent, withFix := p, true
		if tc.recorded {
			parent, withFix = g.head, false
		}
		if problems := after.adoptInto(frame).standsOn(after.fixDir, parent, withFix); len(problems) != 0 {
			t.Errorf("frame %d in a fresh pod whose origin holds F at %s: %s", i+1, p, strings.Join(problems, "; "))
		}
		without := g.pod(t, g.noF)
		if problems := without.adoptInto(frame).standsOn(without.fixDir, g.head, false); len(problems) != 0 {
			t.Errorf("frame %d in a fresh pod whose origin holds no F: %s", i+1, strings.Join(problems, "; "))
		}
	}
	if tc.recorded {
		return
	}

	// The tester beside the fix: no pushed head while it pushed nothing, Q once a push event
	// carries it, and none again once B's deletion takes it out, the fix's own push left as it was.
	fixPushed := map[string]string{}
	if tc.delayed {
		fixPushed[string(implementer)] = p
	}
	if heads := r.testerComesBack(podUpdateReplacementPod); slices.ContainsFunc(heads, func(head string) bool { return head != g.head }) {
		t.Errorf("the tester, which pushed nothing, was told the heads %v, want X %s alone", heads, g.head)
	}
	r.apply("github", intake.BasePush{Repo: "acme/widgets", Branch: testerBranch, After: keepsFTesterQ})
	if held, want := r.pushed(), withPush(fixPushed, string(tester), keepsFTesterQ); !maps.Equal(held, want) {
		t.Fatalf("after the push event of B the record holds the pushes %v, want %v", held, want)
	}
	if heads := r.testerComesBack(keepsFSecondPod); !slices.Contains(heads, keepsFTesterQ) ||
		slices.ContainsFunc(heads, func(head string) bool { return head != g.head && head != keepsFTesterQ }) {
		t.Errorf("after its push of B the tester was told the heads %v, want its pushed head %s beside X %s", heads, keepsFTesterQ, g.head)
	}
	r.apply("github", intake.BasePush{Repo: "acme/widgets", Branch: testerBranch, After: record.NoCommit})
	if held := r.pushed(); !maps.Equal(held, fixPushed) {
		t.Fatalf("after the deletion of B the record holds the pushes %v, want %v", held, fixPushed)
	}
	if heads := r.testerComesBack(keepsFThirdPod); slices.ContainsFunc(heads, func(head string) bool { return head != g.head }) {
		t.Errorf("after B's deletion the tester was told the heads %v, want X %s alone", heads, g.head)
	}
}

// withPush is pushes with role's push at head added, pushes itself left as it was.
func withPush(pushes map[string]string, role, head string) map[string]string {
	with := maps.Clone(pushes)
	with[role] = head
	return with
}
