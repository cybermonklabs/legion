package daemon

// The project move tests' shared fixture (dispatch://INFRA-3583 R9, project_move_test.go): the
// project they move on the idle rig, one issue for each case (projectMove, newProjectMove), the
// daemon's configuration in both tests (moveConfiguration), what they read of its claims
// (moveClaim, claimsNow), each release they run and what they read around it (moveRelease,
// release, runRelease, awaitLine), and a release's line as the daemon serves it (lineJSON).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// moveBound is the bound of every release of the move test that runs to its end: long enough for
// the role that pushes to commit, push and answer, and short enough that the pods that wait for it
// cost the run little.
const moveBound = 20 * time.Second

// moveOlderImage is the image of the pod an older daemon made: another digest of the worker image.
const moveOlderImage = "ghcr.io/example/worker@sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// moveOtherDisk is the disk size the Sandbox of the operator's pod records it was built for:
// another than the rig's runtime.kubernetes.issue_volume, so a pod update round opens over that
// pod while it runs the daemon's own image (builtForAnotherDisk).
const moveOtherDisk = "2Gi"

// moveRefusal is what a shim older than the read answers it: it forwards the frame to Oh My Pi,
// which knows no such command.
const moveRefusal = "Unknown command: read-unpushed"

// projectMove is one project the move tests move, on the idle rig: its issues, one for each case
// of dispatch://INFRA-3583 R9, and the `legion` the operator runs.
type projectMove struct {
	rig    *idleWakeRig
	legion string
	// The issues, one pod each:
	//   - push: unpushed commits and a working-copy change, whose role pushes them and answers;
	//   - answer: an unpushed commit, whose role answers with no push;
	//   - silent: an unpushed commit, whose role does not answer, and whose phase entry is raised
	//     during the first release, so its claim stands behind its entry at the boot;
	//   - clean: only a working-copy commit with no change, holding the tree's architect, a role
	//     of an earlier phase (the planner) and its phase's owner level with its entry; during
	//     the first release a notice arrives for its architect, which is the delivery, and a
	//     role message for its implementer;
	//   - turn: a role in a turn at the bound;
	//   - stopped: a pod in which no role process runs, since an operator stopped its one role,
	//     whose claim retired;
	//   - held: a claim an operator suspended in an open update round, with an unpushed commit
	//     on the pod's disk;
	//   - old: a pod an older daemon made, running another image, in an open update round, whose
	//     shim refuses the read;
	//   - unstarted: a pod in which no role process ever started, its claim's first launch in
	//     flight at the first release;
	//   - restarting: a pod whose role is starting again, its claim's launch on its session in
	//     flight at the first release. From the boot after that release its claim and the
	//     unstarted pod's each run as the clean pod's implementer does.
	push, answer, silent, clean, turn, stopped, held, old, unstarted, restarting idleRigIssue
	// recovered is the release of the operator's recovery, which the next step's restart ends.
	recovered *moveRelease
	// resumes answers each operator's resume the setup leaves in flight, by its issue's name, once
	// the daemon has answered that command (resumeInFlight).
	resumes map[string]chan idleRigCommand
	// workLeft is when each move's work was left in its workspaces (leaveWork), by move: the work
	// a running copy taken after it holds (cybermonklabs/legion#3831, runningCopyAt).
	workLeft map[string]time.Time
}

// newProjectMove readies the project on the first cluster: each case's pod running, its workspace
// holding its work, a pod update round open over the older daemon's pod and over the operator's
// pod, and in flight the first launch of the pod no role process starts in and the launch of the
// role that starts again.
func newProjectMove(t *testing.T) *projectMove {
	t.Helper()
	implementer := []claim.Role{claim.RoleImplementer}
	rig := newIdleRigWith(t, idleRigOptions{
		holdStream: true, wallClock: true, workspaces: true, secondCluster: true,
		// The pod update round over the older daemon's pod opens at a sweep, and a phase entry
		// raised with no fact is converged at another; each is a minute apart in production.
		orphanSweep: 2 * time.Second, entrySweep: 2 * time.Second,
		configure: func(cfg *config.Config) {
			moveConfiguration(cfg)
			// A round past its bound replaces its pod at the next sweep, whatever its roles
			// answered, and the rig's configuration leaves pod_update_grace_minutes at zero, so
			// a round's bound has come as it opens. The rounds of the move stay open until a
			// release ends them, as they do under that key's default.
			cfg.PodUpdateGrace = 30 * time.Minute
		},
	},
		idleRigSeed{name: "push", roles: implementer, plan: &idleRigPlan{TakeTurn: true, AnswerRelease: true, PushOnRelease: true}},
		idleRigSeed{name: "answer", roles: implementer, plan: &idleRigPlan{TakeTurn: true, AnswerRelease: true}},
		idleRigSeed{name: "silent", roles: implementer},
		idleRigSeed{name: "clean", roles: []claim.Role{claim.RoleImplementer, claim.RoleArchitect, claim.RolePlanner}},
		idleRigSeed{name: "turn", roles: implementer, plan: &idleRigPlan{StayInTurn: true}},
		idleRigSeed{name: "stopped", roles: implementer},
		idleRigSeed{name: "held", roles: implementer, plan: &idleRigPlan{TakeTurn: true, AnswerRelease: true, PushOnRelease: true}},
		idleRigSeed{name: "old", roles: implementer, plan: &idleRigPlan{TakeTurn: true, RefuseRead: moveRefusal, AnswerRelease: true}},
		idleRigSeed{name: "unstarted", roles: implementer, heldBack: true},
		idleRigSeed{name: "restarting", roles: implementer},
	)
	m := &projectMove{rig: rig, legion: buildLegion(t),
		push: rig.issue("push"), answer: rig.issue("answer"), silent: rig.issue("silent"), clean: rig.issue("clean"),
		turn: rig.issue("turn"), stopped: rig.issue("stopped"), held: rig.issue("held"), old: rig.issue("old"),
		unstarted: rig.issue("unstarted"), restarting: rig.issue("restarting"), resumes: map[string]chan idleRigCommand{}}
	rig.launchAll()
	m.awaitController(t)

	m.leaveWork(t, "first")
	rig.work.work(t, m.held.pod, []string{"held-unpushed.txt"}, "")
	// A round opens over a pod one of its claims records a process in, so the operator's pod
	// drifts while its role still runs, and the operator's suspension then holds a claim in its
	// round.
	if !rig.kube.madeByAnOlderDaemon(m.old.pod, moveOlderImage) {
		t.Fatalf("the pod of %s is not in the first cluster to stand for an older daemon's", m.old.key)
	}
	if !rig.kube.builtForAnotherDisk(m.held.pod, moveOtherDisk) {
		t.Fatalf("the pod of %s is not in the first cluster to stand for one built for another disk", m.held.key)
	}
	idleRigEventually(t, "the update rounds open over the older daemon's pod and over the operator's pod", func() bool {
		claims := rig.claimsByToken()
		return claims[rig.token(m.old)].UpdateRound != "" && claims[rig.token(m.held)].UpdateRound != ""
	})
	m.operator(t, "stop", m.stopped, m.stopped.role)
	m.operator(t, "suspend", m.held, m.held.role)
	idleRigEventually(t, "the stopped claim retired, the operator's suspension stored in its update round and the update round open over the older daemon's pod", func() bool {
		claims := rig.claimsByToken()
		return claims[rig.token(m.stopped)].State == string(supervise.StateRetired) &&
			claims[rig.token(m.held)].State == string(supervise.StateSuspended) && claims[rig.token(m.held)].UpdateRound != "" &&
			claims[rig.token(m.old)].UpdateRound != ""
	})
	// The role that is starting again ran, an operator suspended it, and the launchers of its pod
	// stopped, as when the pod's one container ends and has not started again. The operator's
	// resume of it then waits for a launcher, as the resume of the pod the rig holds back does.
	m.operator(t, "suspend", m.restarting, m.restarting.role)
	idleRigEventually(t, "the operator's suspension of the role that starts again stored", func() bool {
		c := rig.claimsByToken()[rig.token(m.restarting)]
		return c.State == string(supervise.StateSuspended) && c.SuspendedBy == string(supervise.SuspendedByOperator)
	})
	rig.stopLaunchers(rig.kube, m.restarting)
	m.resumeInFlight(t, m.restarting)
	m.resumeInFlight(t, m.unstarted)
	return m
}

// resumeInFlight has the operator resume issue's one claim with `legion claims resume`, and waits
// until issue's pod stands in the first cluster with the claim launching: its launch in flight,
// in a pod with no launcher to start a role process through. The daemon answers a resume once its
// launch has ended, so the command runs on a goroutine of its own and is read after the release
// that stops the claim (checkCutShort). A launch waits the boot timeout for its launcher and is
// then charged a failure (sandbox's awaitRoleLauncher), so these resumes are the last acts of the
// setup, and the first release comes well inside that wait.
func (m *projectMove) resumeInFlight(t *testing.T, issue idleRigIssue) {
	t.Helper()
	r := m.rig
	token := r.token(issue)
	args := append([]string{"claims", "resume", "--claim", string(token), "--wait", testwait.Remaining(t).Round(time.Second).String()}, r.operatorFlags()...)
	answered := make(chan idleRigCommand, 1)
	m.resumes[issue.name] = answered
	go func() { answered <- legionRun(m.legion, args...) }()
	idleRigEventually(t, "the pod of "+issue.name+" standing with its claim's launch in flight", func() bool {
		return r.kube.pod(issue.pod) && r.claimsByToken()[token].State == string(supervise.StateLaunching)
	})
}

// moveConfiguration is the daemon's configuration in both move tests: it launches the project's
// controller itself, so the controller's pod moves with the issues' pods; its wakes are paced one a
// second, so the starts of a boot wait their turn where the state shows them (wakesWaiting); and
// it releases no idle pod, since a pod the idle rule took would be no case of the move.
func moveConfiguration(cfg *config.Config) {
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	cfg.WorkerWakesPerMinute, cfg.WorkerWakeBurst = 60, 1
	cfg.Runtime.Kubernetes.IdleRelease = config.IdleReleaseConfig{}
}

// leaveWork leaves the work of one move in the three workspaces whose roles hold work: two
// commits and a working-copy change for the role that pushes, and one commit each for the role
// that answers with no push and the one that does not answer. Each file is named for the move.
func (m *projectMove) leaveWork(t *testing.T, move string) {
	t.Helper()
	work := m.rig.work
	work.work(t, m.push.pod, []string{"push-one-" + move + ".txt", "push-two-" + move + ".txt"}, "push-change-"+move+".txt")
	work.work(t, m.answer.pod, []string{"answer-" + move + ".txt"}, "")
	work.work(t, m.silent.pod, []string{"silent-" + move + ".txt"}, "")
	if m.workLeft == nil {
		m.workLeft = map[string]time.Time{}
	}
	m.workLeft[move] = time.Now()
}

// operator has the operator act on one role's claim over its route: stop or suspend.
func (m *projectMove) operator(t *testing.T, act string, issue idleRigIssue, role claim.Role) {
	t.Helper()
	token := m.rig.roleToken(issue, role)
	if status, body := m.rig.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/"+act, nil, true); status/100 != 2 {
		t.Fatalf("%s %s = %d; body %s", act, token, status, body)
	}
}

// controllerToken is the project controller's claim, which the daemon launches in a pod of its own.
func (m *projectMove) controllerToken() claim.Token { return claim.ControllerToken(m.rig.project) }

// resumeTask is the resume task the daemon sends the worker of issue's phase, as the record holds
// the issue now: workflow.Engine.ResumeTask, the one place every resume is composed, on the
// daemon's own engine configuration (engineConfig), read in a transaction that writes nothing.
func (m *projectMove) resumeTask(t *testing.T, issue string) string {
	t.Helper()
	ctx := t.Context()
	tx, err := m.rig.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the read of the resume task of %s: %v", issue, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	records := record.NewStore()
	stored, err := records.Issue(ctx, tx, issue)
	if err != nil || stored == nil {
		t.Fatalf("read the record of %s: %+v, %v", issue, stored, err)
	}
	task, err := workflow.New(records, engineConfig(m.rig.cfg, "", nil), quietLogger()).ResumeTask(ctx, tx, *stored)
	if err != nil {
		t.Fatalf("compose the resume task of %s: %v", issue, err)
	}
	return task
}

// awaitController waits until the project controller the daemon launched runs, registered.
func (m *projectMove) awaitController(t *testing.T) {
	t.Helper()
	r := m.rig
	idleRigEventually(t, "the daemon's controller running, registered and ready", func() bool {
		c, ok := r.claimsByToken()[m.controllerToken()]
		return ok && idleRigWorking(c.State) && r.agents.role(idleRigControllerPod, string(claim.RoleController)).Registered > 0
	})
}

// issues is every issue of the move, in seed order.
func (m *projectMove) issues() []idleRigIssue {
	return []idleRigIssue{m.push, m.answer, m.silent, m.clean, m.turn, m.stopped, m.held, m.old, m.unstarted, m.restarting}
}

// askCounts is how many release asks each role of the move was given so far, by issue and role.
func (m *projectMove) askCounts() map[string]int {
	counts := map[string]int{}
	for _, issue := range m.issues() {
		for _, role := range issue.roles {
			counts[idleRigKey(issue.key, string(role))] = len(m.rig.releaseAsks(issue.key, role))
		}
	}
	counts[idleRigControllerPod] = len(m.rig.releaseAsks(idleRigControllerPod, claim.RoleController))
	return counts
}

// moveClaim is what the move test reads of one claim, to hold the claim's next run to: its state
// and who suspended it, its session, its launch failures and deaths, the pod its process runs in,
// its pending task, and how many processes of its role have registered and tasks it was given.
type moveClaim struct {
	issue, state, suspendedBy, session, pod, pending, pendingTask string
	launchFailures, deaths                                        int
	registered, prompts                                           int
	registeredAt                                                  time.Time
}

// claimsNow is every claim of the project as moveClaim reads it, by token.
func (m *projectMove) claimsNow() map[claim.Token]moveClaim {
	r := m.rig
	now := map[claim.Token]moveClaim{}
	for token, c := range r.claimsByToken() {
		role := r.agents.role(agentKey(c), string(c.Role))
		pending, pendingTask := "", ""
		if c.Pending != nil {
			pending, pendingTask = c.Pending.ID, c.Pending.Task
		}
		now[token] = moveClaim{issue: c.Issue, state: c.State, suspendedBy: c.SuspendedBy, session: c.Session, pod: podOf(c.Locator),
			pending: pending, pendingTask: pendingTask, launchFailures: c.Budgets.LaunchFailures, deaths: c.Budgets.Deaths,
			registered: role.Registered, prompts: len(role.Prompts), registeredAt: role.RegisteredAt}
	}
	return now
}

// agentKey is the key the rig's service files a claim's role processes under: its issue, or the
// controller's pod.
func agentKey(c api.OperatorClaim) string {
	if c.Issue == "" {
		return idleRigControllerPod
	}
	return c.Issue
}

// askedRoles is the roles a line names as asked, in its order.
func askedRoles(line api.ReleasedSandbox) []claim.Role {
	roles := make([]claim.Role, len(line.Asked))
	for i, asked := range line.Asked {
		roles[i] = asked.Role
	}
	return roles
}

// sameRoles is whether got names exactly want, in any order.
func sameRoles(got, want []claim.Role) bool {
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

// runningRoles is the roles of issue whose claim runs now, the roles a release asks.
func (m *projectMove) runningRoles(issue idleRigIssue) []claim.Role {
	claims := m.rig.claimsByToken()
	var running []claim.Role
	for _, role := range issue.roles {
		if c := claims[m.rig.roleToken(issue, role)]; idleRigWorking(c.State) {
			running = append(running, role)
		}
	}
	return running
}

// raiseEntry raises the entry the phases row of issue's phase owner names, with the work of that
// new entry, as a backward move into the role's phase raises it, so the owner's claim stands
// behind its entry: what its phase owes it is work it was never sent. The rig seeds no phases row,
// so the claim was given no entry, and a row naming entry 1 is the first it is behind. It answers
// the new entry's work.
func (m *projectMove) raiseEntry(t *testing.T, issue idleRigIssue) string {
	t.Helper()
	r := m.rig
	ctx := context.Background()
	task := "Implement " + issue.key + " again: the reviewer asked for a change after the release began."
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := r.records.Phases(ctx, tx, issue.key)
		if err != nil {
			return err
		}
		row := record.PhaseRow{Issue: issue.key, Role: issue.role, Claim: r.token(issue)}
		for _, held := range rows {
			if held.Role == issue.role {
				row = held
			}
		}
		row.Entry++
		row.EntryTask, row.EntryRaisedAt = task, time.Now().UTC()
		return r.records.PutPhase(ctx, tx, row)
	}); err != nil {
		t.Fatalf("raise the entry of %s's %s: %v", issue.key, issue.role, err)
	}
	return task
}

// moveRelease is one release of the move and what the test read around it.
type moveRelease struct {
	name    string
	command idleRigCommand
	release api.ProjectReleaseResponse
	// before is every claim as the release found it, and running each issue's running roles.
	before  map[claim.Token]moveClaim
	running map[string][]claim.Role
	// stopped is the claims the release stops: every claim running or starting at its start.
	stopped    []claim.Token
	asksBefore map[string]int
	asksAfter  map[string]int
	created    int
	starts     *startWatcher
	restarted  time.Time
	// entryTask is the work of the entry raised during the release, "" when none was.
	entryTask string
	// heldThrough is the newest start row written before the daemon stopped for its restart: a
	// start above it is one the boot wrote.
	heldThrough int64
}

// beginRelease reads what a release is checked against before it starts, once every role whose
// report the release reads has reported since its last turn (awaitReportsSinceLastTurn): what it
// reads is then what the release finds, and the clean pod is asked nothing. The wait passes over
// the pod in a turn, whose plan keeps it there. Every release runRelease runs begins here.
func (m *projectMove) beginRelease(t *testing.T, name string) *moveRelease {
	t.Helper()
	r := m.rig
	r.awaitReportsSinceLastTurn()
	rd := &moveRelease{name: name, before: m.claimsNow(), running: map[string][]claim.Role{}, asksBefore: m.askCounts(),
		created: len(r.aimed().createdNames()), starts: r.watchStarts()}
	for _, issue := range m.issues() {
		rd.running[issue.key] = m.runningRoles(issue)
	}
	for token, c := range rd.before {
		if idleRigWorking(c.state) || moveStarting(c.state) {
			rd.stopped = append(rd.stopped, token)
		}
	}
	slices.Sort(rd.stopped)
	return rd
}

// moveStarting is whether a claim's state is one a role process coming up is behind: launching,
// its shim connected, or registered and not yet ready. With the states a live role process is
// behind (idleRigWorking) these are the states of the claims a release stops, "those whose role
// process runs or starts at the release" (dispatch://INFRA-3583 R6).
func moveStarting(state string) bool {
	switch supervise.ClaimState(state) {
	case supervise.StateLaunching, supervise.StateShimConnected, supervise.StateRegistered:
		return true
	}
	return false
}

// release runs `legion project release --bound <bound> --json` to its end, running during while
// the release runs once it has deleted the clean pod, and answers what it read.
func (m *projectMove) release(t *testing.T, name string, during func(rd *moveRelease)) *moveRelease {
	t.Helper()
	r := m.rig
	rd := m.beginRelease(t, name)
	done := m.runRelease(moveBound)
	if during != nil {
		m.awaitLine(t, m.clean.pod, api.ReleaseDeletedClean)
		during(rd)
	}
	ran := testwait.Receive(t, "the end of the "+name+" run of legion project release", done)
	rd.command, rd.release = ran.command, ran.release
	t.Logf("the %s release exited %d; stdout %s; stderr %q", name, rd.command.code, rd.command.stdout, rd.command.stderr)
	rd.asksAfter = m.askCounts()
	if got := len(r.aimed().createdNames()); got != rd.created {
		t.Errorf("the cluster's Sandboxes were made %d times by the end of the %s release, want the %d before it: the released daemon made a pod",
			got, name, rd.created)
	}
	return rd
}

// moveCommand is one run of the release command, as runRelease answers it.
type moveCommand struct {
	command idleRigCommand
	release api.ProjectReleaseResponse
}

// runRelease starts `legion project release --bound <bound> --json` on a goroutine of its own,
// since the command waits for the release to finish while the test acts on it, and answers what it
// printed once it ends. It fails nothing itself.
func (m *projectMove) runRelease(bound time.Duration) <-chan moveCommand {
	args := m.rig.releaseArgs("--bound", bound.String())
	done := make(chan moveCommand, 1)
	go func() {
		command := legionRun(m.legion, args...)
		var release api.ProjectReleaseResponse
		_ = json.Unmarshal([]byte(command.stdout), &release)
		done <- moveCommand{command, release}
	}()
	return done
}

// awaitLine waits until the running release's line of the Sandbox name reads outcome, or any
// deleted outcome when outcome is api.ReleaseDeleted.
func (m *projectMove) awaitLine(t *testing.T, name string, outcome api.ReleaseOutcome) {
	t.Helper()
	r := m.rig
	idleRigEventually(t, "the running release's line of "+name+" reading "+string(outcome), func() bool {
		status, body := r.api.request(http.MethodGet, api.ProjectReleaseRoute, nil, true)
		if status != http.StatusOK {
			return false
		}
		var running api.ProjectReleaseResponse
		if json.Unmarshal(body, &running) != nil {
			return false
		}
		line, ok := releaseLineNow(running, name)
		if !ok {
			return false
		}
		return line.Outcome == outcome || outcome == api.ReleaseDeleted && line.Deleted()
	})
}

// lineJSON is a release's line as the daemon serves it, its counts written out rather than as the
// pointers a %+v prints.
func lineJSON(line api.ReleasedSandbox) string {
	encoded, err := json.Marshal(line)
	if err != nil {
		return fmt.Sprintf("%+v", line)
	}
	return string(encoded)
}
