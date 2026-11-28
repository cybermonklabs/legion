package daemon

// The move of a project's pods between two clusters (dispatch://INFRA-3583 R9), run on the idle
// rig with each of the abilities the move needs: two stand-ins for two
// clusters, each with a namespace of the same name; a daemon whose runtime reaches the cluster its
// kubeconfig names; restarts on one database that keep the worker stream's and the API's ports,
// so a pod a restart did not end is adopted; the wall clock, so a turn the bound cuts is cut;
// several roles in one pod, the project controller's own pod among the pods; and a workspace in
// every issue pod made from a remote where a pod's provisioning puts one (idleRigWorkspaces),
// which the role processes read with the shim's own read and push to.

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// checkMove holds a release that moves the project to what the move reads of it: each
// case's pod's line, the asks and their answers, each stopped claim's line and its stored
// suspension, the remote's branch of the role that pushed, the exit and the Sandboxes left in the
// cluster, and no charge and no pod the released daemon made. left names the pods the
// release leaves, and refusedImage whether the pod whose shim refuses the read runs another image.
func (m *projectMove) checkMove(t *testing.T, rd *moveRelease, left []idleRigIssue, refusedImage bool) {
	t.Helper()
	r := m.rig
	release := rd.release
	if release.FinishedAt == nil || release.ID == 0 {
		t.Fatalf("the %s release %+v did not finish", rd.name, release)
	}
	wantCode := 0
	if len(left) > 0 {
		wantCode = 1
	}
	if rd.command.code != wantCode {
		t.Errorf("the %s release exited %d, want %d", rd.name, rd.command.code, wantCode)
	}
	bound := release.BoundAt
	asked := func(issue idleRigIssue, line api.ReleasedSandbox, answered bool) {
		t.Helper()
		if !sameRoles(askedRoles(line), rd.running[issue.key]) {
			t.Errorf("the line of %s asked %v, want each role running at the release, %v", issue.name, askedRoles(line), rd.running[issue.key])
		}
		for _, a := range line.Asked {
			switch {
			case answered && (a.AnsweredAt == nil || a.Bound || a.AnsweredAt.After(bound)):
				t.Errorf("the %s of %s is recorded %+v, want its answer's time before the bound %s", a.Role, issue.name, a, bound)
			case !answered && (a.AnsweredAt != nil || !a.Bound):
				t.Errorf("the %s of %s is recorded %+v, want the bound passed with no answer", a.Role, issue.name, a)
			}
		}
	}
	work := func(issue idleRigIssue, outcome api.ReleaseOutcome, commits int, changed bool) api.ReleasedSandbox {
		t.Helper()
		line := releaseLine(t, release, issue.pod)
		if line.Outcome != outcome || line.Commits == nil || *line.Commits != commits ||
			line.WorkingCopyChanged == nil || *line.WorkingCopyChanged != changed || line.Issue != issue.key {
			t.Errorf("the %s release's line of %s is %s, want %s naming %s with %d commits, changed=%v",
				rd.name, issue.name, lineJSON(line), outcome, issue.key, commits, changed)
		}
		return line
	}
	// The working copy's change is no longer a commit the remote lacks once its role pushed it, and
	// the empty working copy the push left counts for nothing.
	asked(m.push, work(m.push, api.ReleaseDeletedClean, 0, false), true)
	asked(m.answer, work(m.answer, api.ReleaseDeletedLost, 1, false), true)
	asked(m.silent, work(m.silent, api.ReleaseDeletedLost, 1, false), false)
	turn := releaseLine(t, release, m.turn.pod)
	if turn.Outcome != api.ReleaseDeletedClean {
		t.Errorf("the line of the pod in a turn is %s, want it deleted with nothing unpushed", lineJSON(turn))
	}
	asked(m.turn, turn, false)
	old := releaseLine(t, release, m.old.pod)
	if old.Outcome != api.ReleaseDeletedReadRefused || !strings.Contains(old.Refusal, moveRefusal) || old.Commits != nil || old.OtherImage != refusedImage {
		t.Errorf("the line of the pod whose shim refuses the read is %s, want deleted_read_refused with the refusal %q, unread, another image=%v",
			lineJSON(old), moveRefusal, refusedImage)
	}
	asked(m.old, old, true)
	if line := releaseLine(t, release, m.clean.pod); line.Outcome != api.ReleaseDeletedClean || len(line.Asked) != 0 ||
		line.Commits == nil || *line.Commits != 0 || line.WorkingCopyChanged == nil || *line.WorkingCopyChanged {
		t.Errorf("the line of the clean pod is %+v, want deleted_clean with nothing unpushed and nobody asked", line)
	}
	controllers := 0
	for _, line := range release.Result.Sandboxes {
		if line.Kind == runtime.ProjectSandboxController {
			controllers++
			if line.Outcome != api.ReleaseDeleted {
				t.Errorf("the controller's line is %+v, want deleted", line)
			}
		}
	}
	if controllers != 1 {
		t.Errorf("the %s release names %d controller Sandboxes, want the one", rd.name, controllers)
	}
	var wantLeft []string
	for _, issue := range left {
		wantLeft = append(wantLeft, issue.pod)
		if line := releaseLine(t, release, issue.pod); line.Outcome != api.ReleaseLeft || !strings.Contains(line.Reason, "operator") {
			t.Errorf("the line of %s is %+v, want left for the operator's suspension", issue.name, line)
		}
		if !strings.Contains(rd.command.stderr, issue.pod) {
			t.Errorf("the %s release's command said %q, which names no left pod %s", rd.name, rd.command.stderr, issue.pod)
		}
	}
	slices.Sort(wantLeft)
	var gotLeft []string
	for _, line := range release.Result.Sandboxes {
		if line.Outcome == api.ReleaseLeft {
			gotLeft = append(gotLeft, line.Name)
		}
	}
	slices.Sort(gotLeft)
	if !slices.Equal(gotLeft, wantLeft) {
		t.Errorf("the %s release left %v, want %v", rd.name, gotLeft, wantLeft)
	}
	if got := r.aimed().sandboxNames(); !slices.Equal(got, wantLeft) {
		t.Errorf("the cluster holds the Sandboxes %v after the %s release, want only the left %v", got, rd.name, wantLeft)
	}

	m.checkAsks(t, rd)
	for _, file := range []string{"push-one-" + rd.name + ".txt", "push-two-" + rd.name + ".txt", "push-change-" + rd.name + ".txt"} {
		if !r.work.remoteHolds(t, m.push.key, file) {
			t.Errorf("the remote's legion/%s lacks %s, which its role pushed when asked", m.push.key, file)
		}
	}
	for _, lost := range []struct {
		issue idleRigIssue
		file  string
	}{{m.answer, "answer-" + rd.name + ".txt"}, {m.silent, "silent-" + rd.name + ".txt"}} {
		if r.work.remoteHolds(t, lost.issue.key, lost.file) {
			t.Errorf("the remote's legion/%s holds %s, which nobody pushed", lost.issue.key, lost.file)
		}
	}

	stored := r.claimsByToken()
	if len(release.Result.Claims) != len(rd.stopped) {
		t.Errorf("the %s release names %d stopped claims, want the %d running or starting at its start: %+v", rd.name, len(release.Result.Claims), len(rd.stopped), release.Result.Claims)
	}
	for _, line := range release.Result.Claims {
		was, ok := rd.before[line.Token]
		if !ok || !slices.Contains(rd.stopped, line.Token) {
			t.Errorf("the %s release names the claim %+v, which neither ran nor was starting at its start", rd.name, line)
			continue
		}
		c := stored[line.Token]
		// A claim that ran names its session. One whose first launch was in flight never
		// registered, and names none.
		if line.Session != was.session || (line.Session == "" && idleRigWorking(was.state)) ||
			line.Ended != api.ReleasedClaimEnd(supervise.SuspendedByProjectRelease) || !line.BootStarts {
			t.Errorf("the %s release's line of %s is %+v, want its session %q, ended project_release, which the boot starts", rd.name, line.Token, line, was.session)
		}
		if c.State != string(supervise.StateSuspended) || c.SuspendedBy != string(supervise.SuspendedByProjectRelease) || c.UpdateRound != "" {
			t.Errorf("the claim %s is stored %s by %q in round %q, want suspended by project_release in no round", line.Token, c.State, c.SuspendedBy, c.UpdateRound)
		}
	}
	m.checkNoCharge(t, rd.before, "the "+rd.name+" release")
}

// checkAsks holds the release's asks: each running role of the five pods with work, a
// refused read or a turn got one ask, and no other role got one; the ask gives the bound's time
// and the ask's id, names legion/<KEY>, and says that work not pushed by then is lost with the
// pod.
func (m *projectMove) checkAsks(t *testing.T, rd *moveRelease) {
	t.Helper()
	asked := map[string]bool{}
	for _, issue := range []idleRigIssue{m.push, m.answer, m.silent, m.turn, m.old} {
		for _, role := range rd.running[issue.key] {
			asked[idleRigKey(issue.key, string(role))] = true
		}
	}
	for key, after := range rd.asksAfter {
		want := rd.asksBefore[key]
		if asked[key] {
			want++
		}
		if after != want {
			t.Errorf("%s was given %d release asks by the end of the %s release, want %d", key, after, rd.name, want)
		}
	}
	round := projectReleaseRound(rd.release.ID)
	for _, issue := range []idleRigIssue{m.push, m.answer, m.silent, m.turn, m.old} {
		for _, role := range rd.running[issue.key] {
			asks := m.rig.releaseAsks(issue.key, role)
			if len(asks) == 0 {
				continue
			}
			text := asks[len(asks)-1].Text
			for _, want := range []string{round, "legion/" + issue.key, "ready_for_update", "is lost with the pod", rd.release.BoundAt.UTC().Format(time.RFC3339)} {
				if !strings.Contains(text, want) {
					t.Errorf("the ask to %s's %s, %q, does not say %q", issue.name, role, text, want)
				}
			}
		}
	}
}

// checkNoCharge holds every claim to the launch failures and deaths it had in before.
func (m *projectMove) checkNoCharge(t *testing.T, before map[claim.Token]moveClaim, when string) {
	t.Helper()
	for token, now := range m.claimsNow() {
		was, ok := before[token]
		if !ok {
			continue
		}
		if now.launchFailures != was.launchFailures || now.deaths != was.deaths {
			t.Errorf("the claim %s has %d launch failures and %d deaths after %s, want the %d and %d it had",
				token, now.launchFailures, now.deaths, when, was.launchFailures, was.deaths)
		}
	}
}

// restartOn stops the daemon and boots it again on the same database and state directory with
// its runtime aimed at cluster, and waits until each claim the release stopped runs again there:
// in a pod of that cluster, on a process that registered after the restart. It records the newest
// start written before the daemon stopped (heldThrough) and the most wakes the pacing held at once.
func (m *projectMove) restartOn(t *testing.T, cluster *issueSandboxAPI, rd *moveRelease) (wakesHeld int) {
	t.Helper()
	r := m.rig
	r.stopDaemon()
	// Each start the stopped daemon wrote is recorded by now, and nothing writes another before
	// the boot, so the newest of them parts the starts written before the restart from the boot's.
	held, err := rd.starts.seen()
	if err != nil {
		t.Fatalf("read the outbox's start rows as the daemon stopped: %v", err)
	}
	for _, rows := range held {
		for _, start := range rows {
			rd.heldThrough = max(rd.heldThrough, start.id)
		}
	}
	r.aimAt(cluster)
	rd.restarted = time.Now()
	r.restart()
	idleRigEventually(t, "each claim the "+rd.name+" release stopped running again in the cluster "+cluster.prefix, func() bool {
		if waiting := len(r.api.state().WakesWaiting); waiting > wakesHeld {
			wakesHeld = waiting
		}
		now := m.claimsNow()
		for _, token := range rd.stopped {
			c := now[token]
			if !idleRigWorking(c.state) || !strings.HasPrefix(c.pod, cluster.prefix) || c.registered <= rd.before[token].registered {
				return false
			}
		}
		return true
	})
	return wakesHeld
}

// checkStarts holds the boot after a release to dispatch://INFRA-3583 R8: each stopped claim runs
// again on its session in a pod of cluster, or, for a claim stopped in its first launch, on the
// session its first role process makes, with no charge; each got one start, every start the
// boot wrote carries the ruleResume mark, and they went through the wake pace; no wake from
// outside the daemon started any; the claim whose turn the release cut is sent that turn's task
// again, no claim is sent the ask again, and no claim holds an open update round; and each new
// pod's workspace holds the issue's pushed branch and nothing that was not pushed, or, with
// cybermonklabs/legion#3831, nothing left after the issue's running copy was taken.
func (m *projectMove) checkStarts(t *testing.T, cluster *issueSandboxAPI, rd *moveRelease, wakesHeld int) {
	t.Helper()
	r := m.rig
	now := m.claimsNow()
	for _, token := range rd.stopped {
		switch was := rd.before[token].session; {
		case was == "" && now[token].session == "":
			t.Errorf("the claim %s, which the release stopped before it had a session, runs on none after the restart", token)
		case was != "" && now[token].session != was:
			t.Errorf("the claim %s runs on session %q after the restart, want the %q it had", token, now[token].session, was)
		}
	}
	m.checkNoCharge(t, rd.before, "the restart after the "+rd.name+" release")
	if wakesHeld < 2 {
		t.Errorf("the pacing held at most %d of the boot's starts at once, want them waiting their turn at one a second", wakesHeld)
	}
	if woke := r.logged("wake: a released pod is coming back", nil); woke != 0 {
		t.Errorf("the daemon logged %d wakes of a released pod, want none: the boot starts each claim the release stopped", woke)
	}

	// One start each, the boot's carrying its mark. The starts are read once the claims run, so
	// each has been written, and the rig's database recorded each as it was written.
	time.Sleep(time.Second)
	starts, err := rd.starts.seen()
	if err != nil {
		t.Fatalf("read the outbox's start rows: %v", err)
	}
	for _, token := range rd.stopped {
		c := rd.before[token]
		if c.issue == "" {
			continue
		}
		role, _ := token.Role()
		key := idleRigKey(c.issue, string(role))
		rows := starts[key]
		if len(rows) != 1 {
			t.Errorf("%s was given %d starts from the %s release to the boot's, want one: %+v", key, len(rows), rd.name, rows)
			continue
		}
		start := rows[0]
		if start.id > rd.heldThrough && !start.request.RuleResume {
			t.Errorf("the boot's start of %s is %+v, which carries no ruleResume mark", key, start.request)
		}
		behind := key == idleRigKey(m.silent.key, string(m.silent.role)) && rd.entryTask != ""
		if behind && start.request.Task != rd.entryTask {
			t.Errorf("the one start of the claim behind its entry carries the task %q, want the entry's work %q", start.request.Task, rd.entryTask)
		}
		// A phase owner level with its entry starts with its phase's resume task, so its ready
		// writes no second start; every other role starts with no task.
		resume := m.resumeTask(t, c.issue)
		switch {
		case behind:
		case role == claim.RoleImplementer && (start.request.Task != resume || !start.request.ResumeTask):
			t.Errorf("the boot's start of the level phase owner %s is %+v, want its phase's resume task %q", key, start.request, resume)
		case role != claim.RoleImplementer && start.request.Task != "":
			t.Errorf("the boot's start of %s carries the task %q, want none", key, start.request.Task)
		}
	}
	if rd.entryTask != "" {
		token := r.token(m.silent)
		idleRigEventually(t, "the claim behind its entry given that entry's work", func() bool {
			return idleRigCount(r.agents.prompts(m.silent.key, string(m.silent.role))[rd.before[token].prompts:], rd.entryTask) > 0
		})
		if given := idleRigCount(r.agents.prompts(m.silent.key, string(m.silent.role)), rd.entryTask); given != 1 {
			t.Errorf("the claim behind its entry was given the entry's work %d times, want once", given)
		}
	}
	turn := rd.before[r.token(m.turn)]
	if turn.pending == "" {
		t.Errorf("the claim in a turn held no task when the %s release cut its turn", rd.name)
	} else {
		// The task is sent again on the claim's resume, which may deliver it under a delivery of
		// its own, so it is read by its words.
		idleRigEventually(t, "the claim whose turn the release cut given that turn's task again", func() bool {
			return idleRigCount(r.agents.prompts(m.turn.key, string(m.turn.role))[turn.prompts:], turn.pendingTask) > 0
		})
	}
	if asks := m.askCounts(); !maps.Equal(asks, rd.asksAfter) {
		t.Errorf("the release asks after the restart are %v, want the %v the release sent: no claim is sent the ask again", asks, rd.asksAfter)
	}
	for token, c := range r.claimsByToken() {
		if c.UpdateRound != "" {
			t.Errorf("the claim %s holds the update round %q after the restart, want none", token, c.UpdateRound)
		}
	}

	// Each new pod's workspace holds the issue's pushed branch: the role that pushed finds its
	// work there, working-copy change and all, and the two whose work was lost find none of it.
	for _, issue := range []idleRigIssue{m.push, m.answer, m.silent, m.clean, m.turn, m.old, m.unstarted, m.restarting} {
		live, ok := r.work.running(issue.pod)
		if !ok || live.cluster != cluster.prefix {
			t.Errorf("the pod of %s runs the workspace %+v, want one in the cluster %s", issue.name, live, cluster.prefix)
			continue
		}
		if !r.work.holds(t, issue.pod, idleRigBranchFile(issue.key)) {
			t.Errorf("the new workspace of %s lacks %s, which its pushed branch holds", issue.name, idleRigBranchFile(issue.key))
		}
	}
	for _, file := range []string{"push-one-" + rd.name + ".txt", "push-two-" + rd.name + ".txt", "push-change-" + rd.name + ".txt"} {
		if !r.work.holds(t, m.push.pod, file) {
			t.Errorf("the new workspace of the role that pushed lacks %s", file)
		}
	}
	if !strings.Contains(r.work.jj(t, m.push.pod, "log", "--no-graph", "-r", "legion/"+m.push.key, "-T", "description"), "pushed when the project release asked") {
		t.Errorf("the new workspace's legion/%s is not the commit its role pushed", m.push.key)
	}
	// With cybermonklabs/legion#3831 a new pod restores the issue's running copy, so the work is
	// lost only where no copy was taken after it was left: the new workspace may hold work a copy
	// taken later carried.
	copies := r.holdsRunningCopies()
	for _, lost := range []struct {
		issue idleRigIssue
		file  string
	}{{m.answer, "answer-" + rd.name + ".txt"}, {m.silent, "silent-" + rd.name + ".txt"}} {
		if copies {
			if at, ok := r.runningCopyAt(lost.issue.key); ok && at.After(m.workLeft[rd.name]) {
				continue
			}
		}
		if r.work.holds(t, lost.issue.pod, lost.file) {
			t.Errorf("the new workspace of %s holds %s, which its role never pushed", lost.issue.name, lost.file)
		}
	}
}

// checkLeft holds the pod the release left in cluster: it keeps its disk, and its claims
// stay held as an operator's suspension, each in no update round, the claim an operator suspended
// before the release among them (dispatch://INFRA-3583 R6: the release leaves no round open on
// any claim). A round left on a claim of that pod could replace the pod, and the disk the release
// left it to keep, once the operator resumed its claims.
func (m *projectMove) checkLeft(t *testing.T, cluster *issueSandboxAPI) {
	t.Helper()
	if cluster.sandbox(m.held.pod) == nil || !cluster.disk(m.held.pod) {
		t.Errorf("the left pod %s is gone from the cluster %s or lost its disk", m.held.pod, cluster.prefix)
	}
	claims := m.rig.claimsByToken()
	if c := claims[m.rig.token(m.held)]; c.State != string(supervise.StateSuspended) || c.SuspendedBy != string(supervise.SuspendedByOperator) {
		t.Errorf("the left pod's claim is %s by %q, want held as an operator's suspension", c.State, c.SuspendedBy)
	}
	for token, c := range claims {
		if c.Issue != m.held.key {
			continue
		}
		if c.UpdateRound != "" || c.UpdateHead != "" || c.State != string(supervise.StateSuspended) || c.SuspendedBy != string(supervise.SuspendedByOperator) {
			t.Errorf("the left pod's claim %s is %s by %q in the update round %q with the head %q, want held as an operator's suspension in no round",
				token, c.State, c.SuspendedBy, c.UpdateRound, c.UpdateHead)
		}
	}
}

// TestAProjectMovesToAnotherClusterAndBack is the move of a project's pods to another cluster with
// `legion project release` and a restart, and back, on one database, each case of
// dispatch://INFRA-3583 R9 a pod of its own (projectMove). Each step is a named subtest, in the
// order the move happens, and each later step reads what the earlier ones left, so a failed step
// stops the run.
//
// The controls a tester makes of scratch copies of the release, each of which must fail this test:
//   - a release that deletes each pod with unpushed work as soon as its ask is sent fails on the
//     pod whose role pushes and answers, because the issue's branch on the remote then lacks that
//     commit (checkMove);
//   - a release that sends no ask fails on the same pod.
//
// A boot that gives the claim behind its phase entry two starts fails on that claim's starts
// (checkStarts). The third control, the hand deletion with no release, is the last step of this
// test, and it expects the charge it must see.
//
// Three more controls fail the first release's step:
//   - a release that leaves the claims of a pod it leaves in their update round fails on the
//     operator's pod's claim, read before the restart (checkLeft);
//   - an outbox that waits for a claim the release stopped only until the boot has started it,
//     and not until its session holds its role again, fails on the role message, which it then
//     publishes while no session holds the role, so that it ends undelivered and is lost
//     (checkRoleMessage);
//   - a machine that holds the release's suspension of a claim whose launch that suspension cut
//     short, where it suspends the claim at once, fails on the pod no role process started in
//     and on the pod whose role starts again, each of which the release then leaves
//     (checkCutShort).
func TestAProjectMovesToAnotherClusterAndBack(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newProjectMove).
	began := time.Now()
	m := newProjectMove(t)
	r := m.rig
	first, second := r.kube, r.second
	t.Logf("the project stood ready on the first cluster after %s", time.Since(began).Round(time.Second))
	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"the dry run names what a release would do and does nothing", m.dryRun},
		{"the first release in the first cluster and the restart on the second cluster's configuration move the project", func(t *testing.T) {
			rd := m.release(t, "first", func(rd *moveRelease) {
				// The deliveries arrive during the release, once the clean pod is gone: a notice for
				// its tree's architect, as the workflow writes one of a child's status, and a role
				// message for its implementer, as the workflow writes one when a head's checks
				// settle. The claim that stands behind its entry at the boot has that entry raised,
				// and the outbox holds the start it is owed.
				m.deliverNotice(t)
				m.deliverRoleMessage(t)
				rd.entryTask = m.raiseEntry(t, m.silent)
			})
			m.checkMove(t, rd, []idleRigIssue{m.held}, true)
			m.checkCutShort(t, rd, m.unstarted, false)
			m.checkCutShort(t, rd, m.restarting, true)
			// The left pod's claims are read under the daemon that left the pod as well as after
			// the restart: a daemon aimed at another cluster finds no pod of a round and ends it.
			m.checkLeft(t, first)
			m.awaitHeldStart(t, rd)
			m.awaitHeldMessage(t)
			// An operator's restart comes minutes after the release, by when each row the release
			// held is due again (releaseHoldSpacing). This one comes within seconds, so the role
			// message is made due while the daemon is down, and runs as the boot's outbox starts,
			// before its claim has come back.
			r.stopDaemon()
			if due := r.dueNow(m.clean.key, record.OutboxKindRoleMessage); due != 1 {
				t.Fatalf("the outbox holds %d role-message rows for %s as the daemon stopped, want the one the release held", due, m.clean.key)
			}
			// The pod no role process started in is gone, so the rig holds its role back no
			// longer: the boot's start of its claim runs its first role process.
			r.letStart(m.unstarted)
			wakes := m.restartOn(t, second, rd)
			m.checkStarts(t, second, rd, wakes)
			m.checkStartedAgain(t, second, rd, m.unstarted)
			m.checkStartedAgain(t, second, rd, m.restarting)
			m.checkDelivery(t, rd)
			m.checkRoleMessage(t, rd)
			m.checkLeft(t, first)
			if names := first.sandboxNames(); !slices.Equal(names, []string{m.held.pod}) {
				t.Errorf("the first cluster holds %v after the move, want only the left pod %s", names, m.held.pod)
			}
		}},
		{"the move back recovers the left pod: a release in the second cluster and a restart on the first's give the move's outcomes there", func(t *testing.T) {
			m.leaveWork(t, "back")
			rd := m.release(t, "back", nil)
			m.checkMove(t, rd, nil, false)
			wakes := m.restartOn(t, first, rd)
			m.checkStarts(t, first, rd, wakes)
			m.checkLeft(t, first)
			if names := second.sandboxNames(); len(names) != 0 {
				t.Errorf("the second cluster holds %v after the move back, want none", names)
			}
		}},
		{"the operator resumes the left pod's claims, and a release in the first cluster asks its roles and deletes it", m.recover},
		{"a restart on the first cluster's configuration ends that release and starts its claims there", func(t *testing.T) {
			rd := m.recovered
			if rd == nil {
				t.Fatal("the recovery's step made no release for this restart to end")
			}
			m.restartOn(t, first, rd)
			ended, _ := r.readRelease()
			if ended.ID != rd.release.ID || ended.EndedBy != store.ProjectReleaseEndedByBoot {
				t.Errorf("the recovery's release reads %+v after the restart, want it ended by a boot", ended)
			}
			m.checkNoCharge(t, rd.before, "the restart after the recovery")
			now := m.claimsNow()
			for _, token := range rd.stopped {
				if now[token].session != rd.before[token].session {
					t.Errorf("the claim %s runs on session %q, want %q", token, now[token].session, rd.before[token].session)
				}
			}
		}},
		{"a release in the first cluster and a restart on the same configuration give the move's outcomes with each claim again there", func(t *testing.T) {
			m.leaveWork(t, "again")
			rd := m.release(t, "again", nil)
			m.checkMove(t, rd, nil, false)
			wakes := m.restartOn(t, first, rd)
			m.checkStarts(t, first, rd, wakes)
		}},
		{"a restart on the same configuration during an unfinished release starts the stopped claims and adopts the pods it had not reached", m.restartDuringRelease},
		{"the control: a hand deletion with no release, then a restart on the second cluster's configuration, is charged and loses the work", m.handDeletion},
	}
	for _, step := range steps {
		if !r.step(step.name, step.run) {
			return
		}
	}
	t.Logf("the move test ran %s", time.Since(began).Round(time.Second))
}

// checkCutShort holds the first release to dispatch://INFRA-3583 R6 for a pod in which no role
// process ran because a launch of its claim was in flight: the pod no role process ever started
// in, whose claim never ran, and the pod whose role was starting again, whose claim ran before
// (ran). The release stopped that claim for the boot to start, naming the session it had, which
// is none for a claim that never ran, and deleted the pod with no read and nobody asked, no role
// process of it having registered since and no launcher of it running to read it either. The
// operator's resume, whose launch that stop cut short, has been answered.
func (m *projectMove) checkCutShort(t *testing.T, rd *moveRelease, issue idleRigIssue, ran bool) {
	t.Helper()
	r := m.rig
	token := r.token(issue)
	was := rd.before[token]
	if was.state != string(supervise.StateLaunching) || (was.session != "") != ran || (was.registered > 0) != ran {
		t.Errorf("the claim of %s stood %+v at the %s release, want a launch in flight of a claim that ran before=%v", issue.name, was, rd.name, ran)
	}
	if line := releaseLine(t, rd.release, issue.pod); line.Outcome != api.ReleaseDeletedUnread || line.Commits != nil || line.Refusal != "" || len(line.Asked) != 0 {
		t.Errorf("the line of the pod of %s is %s, want deleted_unread with no read and nobody asked: no role process and no launcher of it runs", issue.name, lineJSON(line))
	}
	if line := releaseClaimLine(t, rd.release, token); line.Session != was.session || line.Ended != api.ReleasedClaimEnd(supervise.SuspendedByProjectRelease) || !line.BootStarts {
		t.Errorf("the %s release's line of the claim of %s, whose launch it cut short, is %+v, want the session %q, ended project_release, which the boot starts",
			rd.name, issue.name, line, was.session)
	}
	if seen := r.agents.role(issue.key, string(issue.role)); seen.Registered != was.registered {
		t.Errorf("the role process of %s registered %d times by the end of the %s release, want the %d before it", issue.name, seen.Registered, rd.name, was.registered)
	}
	select {
	case answer := <-m.resumes[issue.name]:
		t.Logf("the operator's resume of %s, whose launch the %s release cut short, exited %d; stdout %q; stderr %q", token, rd.name, answer.code, answer.stdout, answer.stderr)
	case <-time.After(testwait.Remaining(t)):
		t.Errorf("the operator's resume of %s was not answered by the test binary's deadline after the %s release, which stopped its claim", token, rd.name)
	}
}

// checkStartedAgain holds the boot to dispatch://INFRA-3583 R8 for a claim whose launch the
// release cut short: it runs in a pod of cluster on one role process more than it had run, which
// registered after the restart. For the claim that never ran that process is its first.
func (m *projectMove) checkStartedAgain(t *testing.T, cluster *issueSandboxAPI, rd *moveRelease, issue idleRigIssue) {
	t.Helper()
	r := m.rig
	token := r.token(issue)
	c := m.claimsNow()[token]
	seen := r.agents.role(issue.key, string(issue.role))
	if want := rd.before[token].registered + 1; !idleRigWorking(c.state) || !strings.HasPrefix(c.pod, cluster.prefix) ||
		seen.Registered != want || seen.RegisteredAt.Before(rd.restarted) {
		t.Errorf("the claim of %s, whose launch the %s release cut short, stands %+v, its role process registered %d times, last at %s; want it running in the cluster %s on registration %d, made after the restart at %s",
			issue.name, rd.name, c, seen.Registered, seen.RegisteredAt, cluster.prefix, want, rd.restarted)
	}
}

// dryRun is the dry run's step: before any release, `legion project release --dry-run` names the
// roles a release would ask in each pod whose workspace holds unpushed work, whose shim refuses
// the read, or that has a role in a turn, with each count or the refusal; it names the clean pod
// as one a release deletes with nothing unpushed, the pod no role runs in as one its launcher
// reads clean, the pod no role process started in and the pod whose role starts again, in which
// no launcher runs either, as ones it deletes unread, the claims
// of the last two among the claims a release would stop, the operator's pod as left, and the
// older daemon's pod as running another image. It asks nothing, stops nothing and records
// nothing.
func (m *projectMove) dryRun(t *testing.T) {
	r := m.rig
	before := m.claimsNow()
	sandboxes := r.kube.sandboxNames()
	asks := m.askCounts()
	ran, dry := r.projectRelease(m.legion, "--dry-run")
	if ran.code != 0 || !dry.DryRun {
		t.Fatalf("the dry run exited %d with %+v (stderr %q), want 0 and a dry run", ran.code, dry, ran.stderr)
	}
	t.Logf("the dry run printed %s", ran.stdout)

	unpushed := func(issue idleRigIssue, commits int, changed bool) {
		t.Helper()
		line := releaseLine(t, dry, issue.pod)
		if line.Outcome != api.ReleaseDeletedLost || line.Commits == nil || *line.Commits != commits ||
			line.WorkingCopyChanged == nil || *line.WorkingCopyChanged != changed || !sameRoles(askedRoles(line), m.runningRoles(issue)) {
			t.Errorf("the dry run's line of %s is %s, want deleted_lost with %d commits, changed=%v, asking %v",
				issue.name, lineJSON(line), commits, changed, m.runningRoles(issue))
		}
	}
	// The working copy's change is a commit no remote holds, counted beside the two below it
	// (shim.ReadUnpushed, the shim's own test of a working-copy change).
	unpushed(m.push, 3, true)
	unpushed(m.answer, 1, false)
	unpushed(m.silent, 1, false)
	if line := releaseLine(t, dry, m.old.pod); line.Outcome != api.ReleaseDeletedReadRefused || !strings.Contains(line.Refusal, moveRefusal) ||
		!line.OtherImage || !sameRoles(askedRoles(line), m.runningRoles(m.old)) {
		t.Errorf("the dry run's line of the older daemon's pod is %+v, want deleted_read_refused with the shim's refusal %q, marked as another image, asking its roles",
			line, moveRefusal)
	}
	if line := releaseLine(t, dry, m.turn.pod); line.Commits == nil || *line.Commits != 0 || !sameRoles(askedRoles(line), m.runningRoles(m.turn)) {
		t.Errorf("the dry run's line of the pod in a turn is %+v, want nothing unpushed and its roles asked", line)
	}
	if line := releaseLine(t, dry, m.clean.pod); line.Outcome != api.ReleaseDeletedClean || line.Commits == nil || *line.Commits != 0 ||
		line.WorkingCopyChanged == nil || *line.WorkingCopyChanged || len(line.Asked) != 0 {
		t.Errorf("the dry run's line of the clean pod is %+v, want deleted_clean with nothing unpushed and nobody asked", line)
	}
	if line := releaseLine(t, dry, m.stopped.pod); line.Outcome != api.ReleaseDeletedClean || line.Commits == nil || *line.Commits != 0 ||
		line.Reason != releaseLauncherRead || len(line.Asked) != 0 {
		t.Errorf("the dry run's line of the pod no role runs in is %s, want deleted_clean with no commit its launcher read, that read as its reason, and nobody asked",
			lineJSON(line))
	}
	for _, issue := range []idleRigIssue{m.unstarted, m.restarting} {
		if line := releaseLine(t, dry, issue.pod); line.Outcome != api.ReleaseDeletedUnread || line.Commits != nil || len(line.Asked) != 0 {
			t.Errorf("the dry run's line of the pod of %s is %+v, want deleted_unread with no read and nobody asked: no launcher of it runs", issue.name, line)
		}
		if token := r.token(issue); !slices.ContainsFunc(dry.Result.Claims, func(c api.ReleasedClaim) bool { return c.Token == token }) {
			t.Errorf("the dry run names the claims %+v, want among them %s, whose launch is in flight", dry.Result.Claims, token)
		}
	}
	if line := releaseLine(t, dry, m.held.pod); line.Outcome != api.ReleaseLeft || !strings.Contains(line.Reason, "operator") {
		t.Errorf("the dry run's line of the operator's pod is %+v, want left for the operator's suspension", line)
	}
	for _, line := range dry.Result.Sandboxes {
		if line.Name != m.old.pod && line.OtherImage {
			t.Errorf("the dry run marks %s as running another image: %+v", line.Name, line)
		}
	}
	if got := m.askCounts(); !maps.Equal(got, asks) {
		t.Errorf("the roles' asks are %v after the dry run, want %v: a dry run asks nothing", got, asks)
	}
	if got := r.kube.sandboxNames(); !slices.Equal(got, sandboxes) {
		t.Errorf("the first cluster holds %v after the dry run, want %v", got, sandboxes)
	}
	now := m.claimsNow()
	for token, was := range before {
		if c := now[token]; c.state != was.state || c.session != was.session || c.registered != was.registered {
			t.Errorf("the claim %s reads %+v after the dry run, want %+v: a dry run stops nothing", token, c, was)
		}
	}
	if status, _ := r.api.request(http.MethodGet, api.ProjectReleaseRoute, nil, true); status != http.StatusNotFound {
		t.Errorf("GET %s after the dry run = %d, want 404: a dry run records no release", api.ProjectReleaseRoute, status)
	}
}

// recover is the operator's recovery of the left pod, as docs/kubernetes.md tells it: with the
// daemon restarted on the configuration of the cluster that holds the pod, `legion claims resume`
// of its operator-suspended claims, and a release in that cluster, which now asks the pod's roles
// and deletes it, leaving no Sandbox (exit 0).
func (m *projectMove) recover(t *testing.T) {
	r := m.rig
	token := r.token(m.held)
	resumed := r.legion(m.legion, append([]string{"claims", "resume", "--claim", string(token)}, r.operatorFlags()...)...)
	if resumed.code != 0 {
		t.Fatalf("legion claims resume %s exited %d: %s %s", token, resumed.code, resumed.stdout, resumed.stderr)
	}
	idleRigEventually(t, "the left pod's claim running again in its pod", func() bool {
		return idleRigWorking(r.api.claim(token).State)
	})
	rd := m.release(t, "recovery", nil)
	if rd.command.code != 0 || rd.release.FinishedAt == nil {
		t.Fatalf("the recovery's release exited %d with %+v, want 0 and finished", rd.command.code, rd.release)
	}
	line := releaseLine(t, rd.release, m.held.pod)
	if line.Outcome != api.ReleaseDeletedClean || len(line.Asked) != 1 || line.Asked[0].AnsweredAt == nil {
		t.Errorf("the recovery's line of the once-left pod is %+v, want its role asked, answered, and the pod deleted with nothing unpushed", line)
	}
	if !r.work.remoteHolds(t, m.held.key, "held-unpushed.txt") {
		t.Errorf("the remote's legion/%s lacks the work its role pushed when the recovery's release asked", m.held.key)
	}
	if names := r.kube.sandboxNames(); len(names) != 0 {
		t.Errorf("the first cluster holds %v after the recovery's release, want none", names)
	}
	m.checkNoCharge(t, rd.before, "the recovery's release")
	m.recovered = rd
}

// restartDuringRelease restarts the daemon on the same configuration while a release still waits
// for two pods, the one whose role does not answer and the one in a turn: the boot ends the
// release, starts every claim it had stopped, and adopts the two pods it had not reached.
func (m *projectMove) restartDuringRelease(t *testing.T) {
	r := m.rig
	m.leaveWork(t, "late")
	rd := m.beginRelease(t, "late")
	previous, _ := r.readRelease()
	done := m.runRelease(10 * time.Minute)
	// The GET answers the newest release, which is the last one until this one is recorded.
	idleRigEventually(t, "the unfinished release recorded", func() bool {
		release, _ := r.readRelease()
		return release.ID > previous.ID
	})
	for _, issue := range []idleRigIssue{m.clean, m.push, m.answer, m.held, m.old, m.unstarted, m.restarting} {
		m.awaitLine(t, issue.pod, api.ReleaseDeleted)
	}
	// The release has stopped the claims of the pods it deleted, each stored suspended by it, and
	// not yet the rest: the two pods that wait and the controller's, which goes after every issue
	// pod. Those still run, and the restart adopts them.
	now := m.claimsNow()
	for _, issue := range []idleRigIssue{m.silent, m.turn} {
		token := r.token(issue)
		if !idleRigWorking(now[token].state) || r.kube.sandbox(issue.pod) == nil {
			t.Fatalf("the pod of %s is not still running under the release: %+v", issue.name, now[token])
		}
	}
	unreached := map[claim.Token]moveClaim{}
	var stopped []claim.Token
	for _, token := range rd.stopped {
		switch c := now[token]; {
		case c.state == string(supervise.StateSuspended) && c.suspendedBy == string(supervise.SuspendedByProjectRelease):
			stopped = append(stopped, token)
		case idleRigWorking(c.state):
			unreached[token] = c
		default:
			t.Fatalf("the claim %s stands %s by %q under the unfinished release, neither stopped by it nor running", token, c.state, c.suspendedBy)
		}
	}
	if len(stopped) == 0 || len(unreached) < 2 {
		t.Fatalf("the unfinished release had stopped %v and left running %v, want claims of both", stopped, unreached)
	}
	running, _ := r.readRelease()
	// The restart comes before the release's command is drained. The rig holds the API's socket
	// while no daemon runs (idleRigOptions.holdStream), so the command's next read waits in that
	// socket's queue rather than being refused, and it ends only once the restarted daemon answers
	// it. Drained first, the daemon would stay down until the command's own wait ran out, and a run
	// that waited so long found the role processes of the pods the release had not reached no
	// longer all there to adopt.
	r.restart()
	<-done
	ended, _ := r.readRelease()
	if ended.ID != running.ID || ended.EndedBy != store.ProjectReleaseEndedByBoot || ended.FinishedAt != nil {
		t.Errorf("the unfinished release reads %+v after the restart, want release %d ended by a boot before it finished", ended, running.ID)
	}
	defer func() {
		if t.Failed() {
			now := m.claimsNow()
			for _, token := range stopped {
				t.Logf("stopped %s: before %+v, now %+v", token, rd.before[token], now[token])
			}
			for token, was := range unreached {
				t.Logf("unreached %s: before the restart %+v, now %+v", token, was, now[token])
			}
		}
	}()
	idleRigEventually(t, "the claims the unfinished release stopped running again, and the pods it had not reached adopted", func() bool {
		now := m.claimsNow()
		for token, was := range unreached {
			if c := now[token]; !idleRigWorking(c.state) || c.pod != was.pod {
				return false
			}
		}
		for _, token := range stopped {
			c := now[token]
			if !idleRigWorking(c.state) || c.registered <= rd.before[token].registered {
				return false
			}
		}
		return true
	})
	now = m.claimsNow()
	for token, was := range unreached {
		if c := now[token]; c.registered != was.registered || c.session != was.session {
			t.Errorf("the claim %s of a pod the release had not reached reads %+v after the restart, want the process it had adopted, %+v", token, c, was)
		}
	}
	for _, token := range stopped {
		if now[token].session != rd.before[token].session {
			t.Errorf("the stopped claim %s runs on session %q, want %q", token, now[token].session, rd.before[token].session)
		}
	}
	m.checkNoCharge(t, rd.before, "the restart during the unfinished release")
}

// handDeletion is the move's control: the daemon stopped, every Sandbox of the first cluster
// deleted by hand with no release, and the daemon restarted on the second cluster's configuration.
// A move made so is charged a launch failure or a death, and loses the unpushed commit the release
// would have asked its role to push.
//
// With cybermonklabs/legion#3831 the daemon keeps a copy of each running workspace and continues a
// pod the cluster ended uncharged, so the control reads, in place of a charge, the loss of the work
// newer than that copy: work left once the daemon has stopped, which no copy follows.
func (m *projectMove) handDeletion(t *testing.T) {
	r := m.rig
	copies := r.holdsRunningCopies()
	if !copies {
		m.leaveWork(t, "hand")
	}
	before := m.claimsNow()
	r.stopDaemon()
	if copies {
		m.leaveWork(t, "hand")
	}
	if deleted := r.kube.deleteEverySandbox(); len(deleted) == 0 {
		t.Fatal("the first cluster held no Sandbox to delete by hand")
	}
	r.aimAt(r.second)
	r.restart()
	if !copies {
		charged := ""
		idleRigEventually(t, "a launch failure or a death charged for a pod deleted by hand", func() bool {
			for token, now := range m.claimsNow() {
				if was, ok := before[token]; ok && (now.launchFailures > was.launchFailures || now.deaths > was.deaths) {
					charged = fmt.Sprintf("%s: launch failures %d → %d, deaths %d → %d", token, was.launchFailures, now.launchFailures, was.deaths, now.deaths)
					return true
				}
			}
			return false
		})
		t.Logf("the hand deletion was charged: %s", charged)
	}
	token := r.token(m.silent)
	idleRigEventually(t, "the claim of the pod with an unpushed commit running again in the second cluster", func() bool {
		live, ok := r.work.running(m.silent.pod)
		c := m.claimsNow()[token]
		return ok && live.cluster == r.second.prefix && idleRigWorking(c.state) && strings.HasPrefix(c.pod, r.second.prefix)
	})
	if r.work.holds(t, m.silent.pod, "silent-hand.txt") {
		t.Errorf("the workspace of %s holds its unpushed commit after the hand deletion, which no move made so keeps", m.silent.name)
	}
}

// TestAProjectMovesBackToTheFirstCluster is the move back to the cluster a project left: a daemon
// whose runtime is aimed at the stand-in for the second cluster, with issue pods there and the
// controller's pod, runs `legion project release --operator-token-file <file> --json` and keeps the
// claims it prints; it is stopped, and booted again on the same database and state directory aimed
// at the stand-in for the first cluster. The release exits 0 and leaves no Sandbox of the project
// in the second cluster, and each printed claim starts in the first at the boot, on the session the
// release printed, with no death and no launch failure.
//
// Its two controls are a tester's scratch copies, each of which must fail it: a copy that restarts
// the daemon still aimed at the second cluster, where a claim then starts in the cluster the
// release emptied; and a copy of the boot that starts a stopped claim on a new session.
func TestAProjectMovesBackToTheFirstCluster(t *testing.T) {
	t.Parallel()
	rig := newIdleRigWith(t, idleRigOptions{holdStream: true, wallClock: true, secondCluster: true, aimSecond: true, configure: moveConfiguration},
		idleRigSeed{name: "tree", roles: []claim.Role{claim.RoleImplementer, claim.RoleArchitect}},
		idleRigSeed{name: "worker", roles: []claim.Role{claim.RoleImplementer}},
	)
	legion := buildLegion(t)
	rig.launchAll()
	m := &projectMove{rig: rig}
	m.awaitController(t)
	before := m.claimsNow()
	for token, c := range before {
		if !strings.HasPrefix(c.pod, rig.second.prefix) {
			t.Fatalf("the claim %s runs in the pod %q before the release, want one of the second cluster", token, c.pod)
		}
	}

	ran, release := rig.projectRelease(legion)
	if ran.code != 0 || release.FinishedAt == nil {
		t.Fatalf("legion project release exited %d with %+v (stderr %q), want 0 and the release finished", ran.code, release, ran.stderr)
	}
	if names := rig.second.sandboxNames(); len(names) != 0 {
		t.Fatalf("the second cluster holds %v after the release, want no Sandbox of the project", names)
	}
	printed := release.Result.Claims
	if len(printed) != len(before) {
		t.Fatalf("the release printed the claims %+v, want each of the %d that ran", printed, len(before))
	}
	for _, line := range printed {
		if line.Session == "" || !line.BootStarts {
			t.Fatalf("the release printed %+v, want a session the next boot starts", line)
		}
	}

	rig.stopDaemon()
	rig.aimAt(rig.kube)
	rig.restart()
	idleRigEventually(t, "each claim the release printed running again in the first cluster", func() bool {
		now := m.claimsNow()
		for _, line := range printed {
			c := now[line.Token]
			if !idleRigWorking(c.state) || !strings.HasPrefix(c.pod, rig.kube.prefix) || c.registered <= before[line.Token].registered {
				return false
			}
		}
		return true
	})
	now := m.claimsNow()
	for _, line := range printed {
		c := now[line.Token]
		if c.session != line.Session {
			t.Errorf("the claim %s runs on session %q in the first cluster, want the %q the release printed", line.Token, c.session, line.Session)
		}
		if was := before[line.Token]; c.launchFailures != was.launchFailures || c.deaths != was.deaths {
			t.Errorf("the claim %s has %d launch failures and %d deaths after the move back, want the %d and %d it had",
				line.Token, c.launchFailures, c.deaths, was.launchFailures, was.deaths)
		}
	}
	if names := rig.second.sandboxNames(); len(names) != 0 {
		t.Errorf("the second cluster holds %v after the boot, want none", names)
	}
}
