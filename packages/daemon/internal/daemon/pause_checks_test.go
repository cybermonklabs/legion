package daemon

// The daemon's checks of a project pause, k1 to k4 of dispatch://INFRA-3759's Verification
// document, each over the whole daemon of pause_rig_test.go. Each test does what its check's run
// says, asserts what the check names, and logs the counts the check records in a line of its own
// (`k1: …`), failing when a count it reads is 0.

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// k1: a pause holds every start of the project but the controller's, and stops every role it
// holds the right way. Project P has role A of issue X in a turn whose session reports background
// work, role B of X idle, role C of Y and role F of U released (F's backstop due twenty minutes
// after the pause), role D of W in a turn a provider will refuse, role E of V whose relaunch the
// wake pacing has parked, the controller, and, once the pause stands, issue Z admissible in the
// admission line; the same database holds a queued row of project Q. The test pauses P, then
// delivers a listener wake for B, a CI settlement for C, a notice for A, a handoff of A that moves
// X to implementing and a task for the controller, ends A's turn and D's turn, and advances the
// clock thirty minutes.
//
// The negative control is the pause's check taken out of the admission pass
// (admit.Admission.promoteHolds): Z is then admitted while P is paused, and the test fails at that
// admission.
func TestPauseHoldsEveryStart(t *testing.T) {
	r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.AdmissionCap = 8 })
	X, Y, U, W, V, Z := r.issue(1), r.issue(2), r.issue(3), r.issue(4), r.issue(5), r.issue(6)
	A, B := r.token(X, claim.RolePlanner), r.token(X, claim.RoleArchitect)
	C, F := r.token(Y, claim.RoleImplementer), r.token(U, claim.RoleImplementer)
	D, E := r.token(W, claim.RoleImplementer), r.token(V, claim.RoleImplementer)
	controller := r.controller()
	stopped := []claim.Token{A, B, D, E}

	// A plans X, in a turn of its confirmed planning task; B, X's architect, is idle in the same pod.
	now := r.clock.Now().UTC()
	planning := supervise.Delivery{ID: "outbox:101", Task: "Plan " + X + ".", Phase: phase.Planning, Generation: 1, Entry: 1,
		QueuedAt: now, DeliveredAt: now, ConfirmedAt: now}
	a := r.running(X, claim.RolePlanner, supervise.StateWorking)
	a.Pending = &planning
	r.seed(r.root(X, phase.Planning), a, r.running(X, claim.RoleArchitect, supervise.StateIdle))
	// C's pod was released earlier, and its pull request's head is still running its checks.
	r.seed(r.root(Y, phase.Implementing), r.released(Y, claim.RoleImplementer))
	head := strings.Repeat("c", 40)
	r.pullRequest(Y, 4201, head)
	// F's pod was released earlier, with its backstop due twenty minutes after the pause.
	r.seed(r.root(U, phase.Implementing), r.released(U, claim.RoleImplementer))
	if err := r.st.ArmPodBackstop(r.ctx, store.PodWakeBackstop{Project: r.project, Issue: U, Role: claim.RoleImplementer,
		DueAt: now.Add(20 * time.Minute)}); err != nil {
		t.Fatalf("arm F's backstop: %v", err)
	}
	// D is ready, and the daemon's boot hands it its phase's task once its shim says hello.
	r.seed(r.root(W, phase.Implementing), r.running(W, claim.RoleImplementer, supervise.StateReady))
	// E's last launch was cut off by the daemon's stop, so the boot launches it again.
	r.seed(r.root(V, phase.Implementing), r.unfinished(V, claim.RoleImplementer))
	// Q's row: another project's start on the same database.
	q := "Q" + randomSuffix(t)
	qRow := r.enqueue(q+"-1", record.SuperviseRequest{Op: "start", Tree: q + "-1", Role: claim.RoleImplementer, Generation: 1,
		Task: "Q's own start.", WakeKind: pods.WakeRoleMessage})
	r.boot()
	if known, err := r.st.PutClaimActivity(r.ctx, A, 1, claim.SessionActivity{At: now,
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}); err != nil || !known {
		t.Fatalf("record A's report of background work = %v, %v", known, err)
	}
	controllerShim := r.controllerUp()

	// D takes its task in a turn; E's boot launch is let straight out by the burst, its process
	// dies, and its relaunch waits in the pacing for the interval after it.
	dShim := r.dialShim(pauseRigBootToken(D))
	dPrompt := r.startTurn(dShim, "Resume the existing phase work.")
	r.awaitState(D, supervise.StateWorking, "D's turn has started")
	r.parkRelaunch(E)
	if started := r.linesOf(turnStartedLine(), A); len(started) != 0 {
		t.Fatalf("A's turn started in this daemon's life %d times, want none: A's turn is the one the boot found", len(started))
	}

	launchesBefore := len(r.rt.launches())
	paused := r.mark()
	pause := r.pauseProject()

	// B stops at once, its session kept; E's parked launch is stopped with the launch owed; A and
	// D stay in their turns.
	b := r.awaitState(B, supervise.StateSuspended, "the pause stops an idle role at once")
	if !b.Released() || b.Session != "ses-"+string(B) || b.SessionFile != "/sessions/"+string(B)+".jsonl" {
		t.Fatalf("B after its stop = %s by %s, session %q file %q; want released with its conversation kept",
			b.State, b.SuspendedBy, b.Session, b.SessionFile)
	}
	if got := r.rt.suspendedOf(B); got != 1 {
		t.Fatalf("B's process was stopped %d times, want once", got)
	}
	if e := r.awaitState(E, supervise.StateSuspended, "the pause stops a launch the pacing parked"); !e.Released() {
		t.Fatalf("E after its stop is suspended by %s, want released", e.SuspendedBy)
	}
	for _, token := range []claim.Token{A, D} {
		if c := r.claim(token); c.State != supervise.StateWorking {
			t.Fatalf("%s is %s once the pause asked its stop, want it still in its turn", token, c.State)
		}
	}

	// What reaches the project while it is paused.
	r.applyFact("listener", "wake-b", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(B), At: r.clock.Now(), Report: "report-b"})
	r.applyFact("github", "checks-c", intake.PullRequestChecks{Repo: liveRepo, Number: 4201, HeadSHA: head, Failing: []string{"build"}})
	r.enqueue(X, record.HookPrompt{Hook: "back-to-planning", Role: claim.RolePlanner, From: phase.Implementing, To: phase.Planning,
		Count: 1, Counts: map[string]int{phase.Pair(phase.Implementing, phase.Planning): 1}, Prompt: "Read the thread first."})
	r.applyFact("api", "handoff-a", intake.HandoffComplete{Issue: X, Role: claim.RolePlanner, Claim: A, Generation: 1,
		Summary: "the plan", HandoffRecord: "plan-1", HandoffVersion: 1})
	r.board.show(Z, "todo")
	r.applyFact("dispatch", "issue-z", intake.DispatchIssue{Key: Z, Seq: 1, Type: "task", Status: "todo", Title: Z, Rank: "U",
		HandedOver: true, ResolvedRepo: liveRepo})
	r.deliver(controller, "Read the state and report.")
	r.startTurn(controllerShim, "Read the state and report.")
	r.awaitState(controller, supervise.StateWorking, "the controller's task is delivered while the project is paused")

	// Ten minutes in, no deadline has cut A's or D's turn off.
	r.advance(10 * time.Minute)
	for _, token := range []claim.Token{A, D} {
		if c := r.claim(token); c.State != supervise.StateWorking {
			t.Fatalf("%s is %s ten minutes into the pause, want its turn still running: no deadline cuts it off", token, c.State)
		}
		if got := r.rt.suspendedOf(token); got != 0 {
			t.Fatalf("%s's process was stopped %d times while its turn ran, want none", token, got)
		}
	}

	// A's turn ends, and its process stops although its session reported background work.
	r.handle(A, supervise.StreamTurnEnd{Claim: A})
	if a := r.awaitState(A, supervise.StateSuspended, "A stops when its turn ends"); !a.Released() {
		t.Fatalf("A after its turn is suspended by %s, want released", a.SuspendedBy)
	}
	if got := r.rt.suspendedOf(A); got != 1 {
		t.Fatalf("A's process was stopped %d times, want once at its turn's end", got)
	}
	if background := r.count(`select count(*) from claims where token = $1 and activity_background`, string(A)); background != 1 {
		t.Fatalf("A's claim records no background work, want the report its session made")
	}
	// D's turn ends in the provider's refusal of its prompt. D stops, and keeps its task.
	r.handle(D, supervise.StreamTurnEnd{Claim: D})
	// The prompt's request id carries its delivery's id (the worker stream's promptDelivery), which
	// is what the stream hands the machine.
	dDelivery, _, _ := strings.Cut(dPrompt.ID, ".")
	r.handle(D, supervise.StreamPromptResult{Claim: D, DeliveryID: dDelivery, AgentInvoked: true, Error: "overloaded_error", HTTPStatus: 529})
	d := r.awaitState(D, supervise.StateSuspended, "D stops when its turn ends")
	if !d.Released() || d.Pending == nil || !strings.HasSuffix(d.Pending.Task, "Resume the existing phase work.") {
		t.Fatalf("D after its turn = %s by %s holding %+v; want released and holding its task", d.State, d.SuspendedBy, d.Pending)
	}

	// Twenty minutes more: F's backstop comes due, and every timer the turns' ends armed fires.
	r.advance(20 * time.Minute)

	// D keeps its task, and the supervisor sends it nowhere.
	if later := r.claim(D); later.Pending == nil || later.Pending.Task != d.Pending.Task {
		t.Fatalf("D holds %+v thirty minutes into the pause, want the task it kept at its stop, %q", later.Pending, d.Pending.Task)
	}
	r.noPrompt(dShim, "D's task is not sent again while the project is paused")

	// No role launched since the pause, E's parked launch included.
	if launched := r.rt.launches()[launchesBefore:]; len(launched) != 0 {
		t.Fatalf("%d launches since the pause (%+v), want none: the pause holds every start", len(launched), launched)
	}
	if launched := r.rt.launchesOf(E); len(launched) != 1 {
		t.Fatalf("E was launched %d times, want only its boot launch: its parked relaunch does not run", len(launched))
	}

	// The pods of X and W stay, held by the pause, and no rule deletes them or saves their work.
	deleted, bundled := r.rt.podReads()
	if len(deleted) != 0 || len(bundled) != 0 {
		t.Fatalf("the runtime deleted the pods of %v and bundled %v while the project is paused, want none", deleted, bundled)
	}
	for _, issue := range []string{X, W} {
		if !r.openPodHold(issue) {
			t.Fatalf("no pause holds %s's pod, want the pause's hold over the pod it stopped", issue)
		}
	}
	if saved := r.count(`select count(*) from workspace_bundles`); saved != 0 {
		t.Fatalf("%d workspaces saved while the project is paused, want none", saved)
	}

	// Z is not admitted.
	state := r.state()
	if slices.Contains(state.Admission.Active, Z) || r.count(`select count(*) from slots where issue = $1`, Z) != 0 {
		t.Fatalf("Z (%s) was admitted while the project is paused: the admission slots are %v", Z, state.Admission.Active)
	}
	if !slices.Contains(state.Admission.Waiting, Z) {
		t.Fatalf("Z (%s) is not in the admission line %v, want it waiting there for the resume", Z, state.Admission.Waiting)
	}

	// Each start the pause holds, held and listed under the pause.
	if state.Paused == nil || state.Paused.ID != pause.ID {
		t.Fatalf("the state's pause = %+v, want pause %d", state.Paused, pause.ID)
	}
	held := state.Paused.Held
	implementerOfX := r.token(X, claim.RoleImplementer)
	for _, want := range []struct {
		what, issue, kind string
		claim             claim.Token
	}{
		{"B's wake", X, string(record.OutboxKindSupervise), B},
		{"C's CI settlement", Y, string(record.OutboxKindSupervise), C},
		{"F's backstop", U, string(record.OutboxKindSupervise), F},
		{"A's notice", X, string(record.OutboxKindHookPrompt), A},
		{"the start of X's next phase", X, string(record.OutboxKindSupervise), implementerOfX},
	} {
		if !slices.ContainsFunc(heldIn(held, want.kind, want.issue), func(row api.PauseHeldView) bool { return row.Claim == want.claim }) {
			t.Fatalf("%s (a %s row of %s for %s) is not among the rows the pause holds: %+v", want.what, want.kind, want.issue, want.claim, held)
		}
	}
	r.noneDue(held)
	if starts := r.startRows(Y, claim.RoleImplementer); len(starts) != 1 || starts[0] != fmt.Sprintf("%s/%d", pods.WakeChecks, pause.ID) {
		t.Fatalf("C's starts = %v, want the one CI wake the pause holds", starts)
	}
	if starts := r.startRows(U, claim.RoleImplementer); len(starts) != 1 || starts[0] != fmt.Sprintf("%s/%d", pods.WakeBackstop, pause.ID) {
		t.Fatalf("F's starts = %v, want the one backstop wake the pause holds", starts)
	}

	// Q's row is claimed as before.
	r.claimsRowOf(q, qRow)

	// No stopped role is stranded.
	stranded := strandedRoles(state)
	for _, role := range stranded {
		for _, token := range stopped {
			if c := r.claim(token); role == c.Issue+" "+string(c.Role) {
				t.Fatalf("the state names %s stranded, want no role the pause stopped among them: %v", role, stranded)
			}
		}
	}

	// One line for the pause, and one for each role it stopped, naming its claim.
	if lines := r.linesAfter(paused, pausePausedLine); len(lines) != 1 {
		t.Fatalf("%d `%s` lines, want one", len(lines), pausePausedLine)
	}
	for _, token := range stopped {
		if lines := r.linesOf(pauseStoppedLine, token); len(lines) != 1 {
			t.Fatalf("%d `%s` lines name %s, want one", len(lines), pauseStoppedLine, token)
		}
	}
	if lines := r.linesAfter(paused, pauseStoppedLine); len(lines) != len(stopped) {
		t.Fatalf("%d `%s` lines, want one for each of the %d roles the pause stopped", len(lines), pauseStoppedLine, len(stopped))
	}

	// No turn started after the pause but the controller's, whose task was delivered.
	started := r.linesAfter(paused, turnStartedLine())
	for _, line := range started {
		if line["claim"] != string(controller) {
			t.Fatalf("a turn started after the pause for %v, want none but the controller's: %v", line["claim"], line)
		}
	}
	if len(started) == 0 {
		t.Fatal("no turn of the controller started after the pause, want the one its task started")
	}

	stops := len(r.linesAfter(paused, pauseStoppedLine))
	t.Logf("k1: held rows=%d, stopped roles=%d, stranded checked=%d, turns started after the pause=%d (controller's)",
		len(held), stops, len(stranded), len(started))
	if len(held) == 0 || stops == 0 {
		t.Fatalf("k1 read %d held rows and %d stopped roles, want both above 0", len(held), stops)
	}
}

// k2: a resume releases the held work at the wake pacing, eight a minute in bursts of one. The
// project has 101 roles, each the worker of its own issue's open phase. While it is paused, roles
// 1 to 90 are each given one held wake (a listener's report of a message to the role), roles 1 to
// 3 a held notice as well and role 91 a held notice only (a hook's prompt, as k1's), role 92 stops
// with a task it did not finish (its phase's task, which no shim's hello let it read), role 93
// had a relaunch the pacing parked when the pause began, and roles 94 to 101 are given nothing;
// issue 102 is queued in the admission line. The test resumes, and moves the clock through each
// launch's turn and on to twenty minutes after the resume.
//
// Two negative controls: the resume's launches made exempt from the pacing (supervise's
// reserveWake), where every held launch runs in the same second and the test fails at the second
// launch; and no let-out moment stamped (projectPause.wakeLetOut), where the test fails at a stuck
// warning before the last launch.
func TestResumeReleasesHeldWorkAtThePace(t *testing.T) {
	const (
		roles      = 101
		woken      = 90
		noticeOnly = 91
		unfinished = 92
		parked     = 93
		// launches is every launch the resume owes: the 93 roles with held work, and the first
		// launch of the admitted issue, its architect's.
		launches = 94
	)
	r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.AdmissionCap = roles + 10 })
	worker := func(n int) claim.Token { return r.token(r.issue(n), claim.RoleImplementer) }
	noticed := []int{1, 2, 3, noticeOnly}
	for n := 1; n <= roles; n++ {
		c := r.running(r.issue(n), claim.RoleImplementer, supervise.StateIdle)
		switch n {
		case unfinished:
			c = r.running(r.issue(n), claim.RoleImplementer, supervise.StateReady)
		case parked:
			c = r.unfinished(r.issue(n), claim.RoleImplementer)
		}
		r.seed(r.root(r.issue(n), phase.Implementing), c)
	}
	queued := r.issue(roles + 1)
	queuedArchitect := r.token(queued, claim.RoleArchitect)
	r.boot()

	testwait.Eventually(t, "the boot to hand role 92 its phase's task", func() bool { return r.claim(worker(unfinished)).Pending != nil })
	r.parkRelaunch(worker(parked))

	r.pauseProject()
	testwait.WithinPolling(t, 3*time.Minute, 100*time.Millisecond, "the pause to stop all 101 roles", func() bool {
		return r.count(`select count(*) from claims where project = $1 and state = 'suspended' and suspended_by = 'released'`, r.project) == roles
	})
	for n := 1; n <= woken; n++ {
		r.applyFact("listener", fmt.Sprintf("wake-%d", n), intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(worker(n)),
			At: r.clock.Now(), Report: fmt.Sprintf("report-%d", n)})
	}
	for _, n := range noticed {
		r.enqueue(r.issue(n), record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing,
			To: phase.Implementing, Count: 1, Counts: map[string]int{phase.Pair(phase.Reviewing, phase.Implementing): 1}, Prompt: "Read the review."})
	}
	r.board.show(queued, "todo")
	r.applyFact("dispatch", "issue-queued", intake.DispatchIssue{Key: queued, Seq: 1, Type: "task", Status: "todo", Title: queued, Rank: "U",
		HandedOver: true, ResolvedRepo: liveRepo})
	state := r.state()
	if !slices.Contains(state.Admission.Waiting, queued) || slices.Contains(state.Admission.Active, queued) {
		t.Fatalf("issue %s is not waiting in the admission line while the project is paused: %+v", queued, state.Admission)
	}
	held := state.Paused.Held
	// 90 held wakes, 4 held notices, and the starts the pause owes roles 92 and 93.
	if want := woken + len(noticed) + 2; len(held) != want {
		t.Fatalf("the pause holds %d rows, want %d: %+v", len(held), want, held)
	}

	before, resumedAt, mark := len(r.rt.launches()), r.clock.Now(), r.mark()
	resumed := r.resumeProject()
	if resumed.Released != len(held) {
		t.Fatalf("the resume released %d held rows, want the %d the pause held", resumed.Released, len(held))
	}

	// Every held row runs, and the pacing gives each launch its turn: the burst lets one out, and
	// the state names every other.
	testwait.WithinPolling(t, 3*time.Minute, 200*time.Millisecond, "every held row to run and raise its launch", func() bool {
		return len(r.rt.launches())-before+len(r.state().WakesWaiting) == launches
	})
	// No role is named stranded before ten minutes after the pacing let its launch out: read from
	// every state the test takes, at each launch's turn and each half minute after the last.
	reads := 0
	notStranded := func(state api.State) {
		t.Helper()
		now := r.clock.Now()
		for _, c := range state.StrandedClaims {
			let := r.rt.launchesOf(r.token(c.Issue, c.Role))
			if len(let) == 0 || !let[len(let)-1].at.After(resumedAt) || now.Sub(let[len(let)-1].at) < 10*time.Minute {
				t.Fatalf("%s %s is named stranded %s after the resume, less than ten minutes after the pacing let its launch out (launches %+v)",
					c.Issue, c.Role, now.Sub(resumedAt), let)
			}
		}
		reads++
	}
	// Each launch runs at its own turn. A role with a held notice is brought up at its launch, so
	// its session holds its role topic for the notice to be delivered to.
	r.runPacing(before, launches, func(launch waveLaunch) {
		for _, n := range noticed {
			if launch.claim == worker(n) {
				r.ready(launch.claim)
				r.envoy.hold(launch.claim)
			}
		}
	}, notStranded)
	made := r.rt.launches()[before:]
	if len(made) != launches {
		t.Fatalf("%d launches after the resume, want %d", len(made), launches)
	}
	launchedAt := map[claim.Token]time.Time{}
	count := map[claim.Token]int{}
	for _, launch := range made {
		count[launch.claim]++
		launchedAt[launch.claim] = launch.at
	}
	for n := 1; n <= roles; n++ {
		want := 1
		if n > parked {
			want = 0
		}
		if got := count[worker(n)]; got != want {
			t.Fatalf("role %d launched %d times after the resume, want %d", n, got, want)
		}
	}
	if got := count[queuedArchitect]; got != 1 {
		t.Fatalf("the admitted issue's architect launched %d times, want its first launch", got)
	}
	if !slices.Contains(r.state().Admission.Active, queued) {
		t.Fatalf("issue %s is not admitted after the resume: %+v", queued, r.state().Admission)
	}

	// The pace: the second launch waits its turn after the first, at most nine launches fall in the
	// first minute and eight in each minute after it, and the last leaves more than ten minutes
	// after the resume.
	if !made[1].at.After(made[0].at) {
		t.Fatalf("the second launch after the resume, %s's, is at %s, the moment of the first: want it held to its turn in the pacing",
			made[1].claim, made[1].at)
	}
	minutes := map[int]int{}
	for _, launch := range made {
		minutes[int(launch.at.Sub(resumedAt)/time.Minute)]++
	}
	busiest := 0
	for minute, n := range minutes {
		limit := 8
		if minute == 0 {
			limit = 9
		}
		if n > limit {
			t.Fatalf("minute %d after the resume holds %d launches, want at most %d", minute, n, limit)
		}
		if minute > 0 && n > busiest {
			busiest = n
		}
	}
	last := made[len(made)-1].at.Sub(resumedAt)
	if last <= 10*time.Minute {
		t.Fatalf("the last launch leaves %s after the resume, want more than ten minutes: the stuck clock is then tested past its bound", last)
	}

	// One wake line for each launch, each paced, and one line for each held row released, naming
	// its claim.
	wakes := r.linesAfter(mark, supervise.WakeRanMessage)
	if len(wakes) != launches {
		t.Fatalf("%d `%s` lines after the resume, want one for each of the %d launches", len(wakes), supervise.WakeRanMessage, launches)
	}
	for _, line := range wakes {
		if line["paced"] != true || line["kind"] != string(supervise.WakeLaunch) {
			t.Fatalf("a wake line after the resume says paced %v kind %v, want a paced launch: %v", line["paced"], line["kind"], line)
		}
	}
	released := r.linesAfter(mark, pauseReleasedLine)
	if len(released) != len(held) {
		t.Fatalf("%d `%s` lines, want one for each of the %d rows the pause held", len(released), pauseReleasedLine, len(held))
	}
	for _, row := range held {
		if row.Claim == "" || !slices.ContainsFunc(released, func(line map[string]any) bool {
			return line["row"] == float64(row.ID) && line["claim"] == string(row.Claim)
		}) {
			t.Fatalf("no `%s` line names held row %d and its claim %q", pauseReleasedLine, row.ID, row.Claim)
		}
	}

	// On to twenty minutes after the resume, reading the state each half minute.
	for r.clock.Now().Before(resumedAt.Add(20 * time.Minute)) {
		r.clock.advance(30 * time.Second)
		time.Sleep(3 * pauseRigTick)
		notStranded(r.state())
	}
	// No wake is warned stuck before ten minutes after the pacing let its launch out: the warning
	// names the stuck clock it read and how long after it the warning came. Both are written to the
	// second, so the sum may fall short of the moment by two.
	stuck := r.linesAfter(mark, "wake: a released pod's resume has had no first turn since its wake")
	for _, line := range stuck {
		issue, _ := line["issue"].(string)
		role, _ := line["role"].(string)
		clock, err := time.Parse(time.RFC3339, fmt.Sprint(line["clock"]))
		if err != nil {
			t.Fatalf("a stuck warning names the clock %v: %v", line["clock"], err)
		}
		after, err := time.ParseDuration(fmt.Sprint(line["after"]))
		if err != nil {
			t.Fatalf("a stuck warning names the wait %v: %v", line["after"], err)
		}
		let, launched := launchedAt[r.token(issue, claim.Role(role))]
		if !launched {
			t.Fatalf("%s %s is warned stuck with no launch of it let out: %v", issue, role, line)
		}
		if warned := clock.Add(after); warned.Before(let.Add(10*time.Minute - 2*time.Second)) {
			t.Fatalf("%s %s is warned stuck %s after the pacing let its launch out at %s, want no warning before ten minutes: %v",
				issue, role, warned.Sub(let), let, line)
		}
	}
	if len(stuck) == 0 {
		t.Fatal("no wake was warned stuck in twenty minutes, want the launches let out in the first ten warned ten minutes after each: the stuck pass read nothing")
	}

	// Each held notice is delivered after its role's one launch. A hook's prompt whose role's
	// launch the pacing held waits for that launch (outbox.parkedWait): it is due at the launch's
	// turn, or parkedWakeBound on, whichever comes first. The machines read the test clock and the
	// outbox the wall clock, so here the turn is a moment of the test clock and the bound is five
	// minutes of wall time, either of which can be minutes of wall time away although the test
	// clock has passed every launch. Every launch is made by now, so the test makes each such row
	// due now on both clocks (makeDue), as the bound would, and the outbox's sweep runs it.
	keys := []string{}
	for _, n := range noticed {
		keys = append(keys, r.issue(n))
	}
	r.makeDue(record.OutboxKindHookPrompt, keys...)
	delivered := 0
	for _, n := range noticed {
		token := worker(n)
		testwait.WithinPolling(t, 4*time.Minute, 200*time.Millisecond, fmt.Sprintf("role %d's held notice to be delivered", n), func() bool {
			return len(r.envoy.publishedTo(token)) > 0
		})
		notice := r.envoy.publishedTo(token)[0]
		if notice.at.Before(launchedAt[token]) || !strings.HasPrefix(notice.message, "Legion hook ") {
			t.Fatalf("role %d's notice %q was delivered at %s, want the hook's prompt delivered after its launch at %s",
				n, notice.message, notice.at, launchedAt[token])
		}
		delivered++
	}

	t.Logf("k2: launches=%d (93 roles and the admitted issue's first), released rows=%d, first minute=%d, busiest later minute=%d, last launch %s after the resume, stuck warnings=%d (none before ten minutes), state reads for stranded roles=%d, notices delivered after their launch=%d",
		len(made), len(released), minutes[0], busiest, last.Truncate(time.Second), len(stuck), reads, delivered)
	if len(made) == 0 || len(released) == 0 || reads == 0 {
		t.Fatalf("k2 read %d launches, %d released rows and %d states, want each above 0", len(made), len(released), reads)
	}
}

// runPacing moves the test clock to the turn of each launch the wake pacing holds, earliest first,
// until want launches have been made since launch number since, and hands each launch to made as
// it is read and each state it reads to read. Each turn runs one launch, at its own moment: the
// clock moves to the wake's due time and no further until that launch has reached the runtime.
func (r *pauseDaemonRig) runPacing(since, want int, made func(waveLaunch), read func(api.State)) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	handled := 0
	for {
		launched := r.rt.launches()[since:]
		for ; handled < len(launched); handled++ {
			made(launched[handled])
		}
		if len(launched) >= want {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%d of %d launches made in ten minutes of the wall clock", len(launched), want)
		}
		state := r.d.state()
		read(state)
		waiting := state.WakesWaiting
		slices.SortStableFunc(waiting, func(a, b api.WakeWaiting) int { return a.DueAt.Compare(b.DueAt) })
		if len(waiting) == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		before := len(r.rt.launches())
		r.clock.advanceTo(waiting[0].DueAt)
		testwait.Eventually(r.t, fmt.Sprintf("the launch of %s at its turn, %s", waiting[0].Claim, waiting[0].DueAt), func() bool {
			return len(r.rt.launches()) > before
		})
	}
}

// k3: a pause holds across a restart. The project has role G in a turn, role H with one held start
// row (a listener's wake taken while the project is paused), the worker S of an open phase that
// the pause stopped with nothing held for it, role T that stopped with a task it did not finish
// (the phase's task the boot handed it, which no shim's hello has let it read yet), and role R
// whose relaunch the pacing had parked at the pause. The test pauses, stops the daemon and boots
// it again on the same store, ends G's turn, and resumes.
//
// The negative control is the pause's stops made as the daemon's own suspension (SuspendedByDaemon),
// which the boot resumes by rule: after the restart a start row is queued for S, and the test
// fails there.
func TestPauseHoldsAcrossRestart(t *testing.T) {
	r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.AdmissionCap = 8 })
	issues := []string{r.issue(1), r.issue(2), r.issue(3), r.issue(4), r.issue(5)}
	G, H := r.token(issues[0], claim.RoleImplementer), r.token(issues[1], claim.RoleArchitect)
	S, T, R := r.token(issues[2], claim.RoleImplementer), r.token(issues[3], claim.RoleImplementer), r.token(issues[4], claim.RoleImplementer)
	controller := r.controller()
	now := r.clock.Now().UTC()
	g := r.running(issues[0], claim.RoleImplementer, supervise.StateWorking)
	g.Pending = &supervise.Delivery{ID: "outbox:201", Task: "Implement " + issues[0] + ".", Phase: phase.Implementing, Generation: 1,
		Entry: 1, QueuedAt: now, DeliveredAt: now, ConfirmedAt: now}
	r.seed(r.root(issues[0], phase.Implementing), g)
	r.seed(r.root(issues[1], phase.Implementing), r.running(issues[1], claim.RoleArchitect, supervise.StateIdle))
	r.seed(r.root(issues[2], phase.Implementing), r.running(issues[2], claim.RoleImplementer, supervise.StateIdle))
	r.seed(r.root(issues[3], phase.Implementing), r.running(issues[3], claim.RoleImplementer, supervise.StateReady))
	r.seed(r.root(issues[4], phase.Implementing), r.unfinished(issues[4], claim.RoleImplementer))
	r.boot()

	testwait.Eventually(t, "the boot to hand T its phase's task", func() bool { return r.claim(T).Pending != nil })
	r.parkRelaunch(R)

	pause := r.pauseProject()
	for _, token := range []claim.Token{H, S, T, R} {
		r.awaitState(token, supervise.StateSuspended, "the pause stops it at once")
	}
	if c := r.claim(G); c.State != supervise.StateWorking {
		t.Fatalf("G is %s once the pause asked its stop, want its turn still running", c.State)
	}
	r.applyFact("listener", "wake-h", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(H), At: r.clock.Now(), Report: "report-h"})

	restarted := len(r.rt.launches())
	r.restart()
	testwait.Eventually(t, "the second boot to apply its boot fact", func() bool {
		return r.count(`select count(*) from processed_events where source = 'daemon' and event_id like 'boot:%'`) == 2
	})
	// A second of the wall clock, for a relaunch the boot raised to reach the runtime.
	time.Sleep(time.Second)
	bootLaunches := r.rt.launches()[restarted:]

	// The same pause stands, the boot launched nothing but the controller, and it queued no start
	// for S; the held row is still held.
	state := r.state()
	if p := state.Paused; p == nil || p.ID != pause.ID || !p.Since.Equal(pause.Since) || p.By != pause.By || p.Reason != pause.Reason {
		t.Fatalf("the state's pause after the restart = %+v, want pause %+v", p, pause)
	}
	for _, launch := range bootLaunches {
		if launch.claim != controller {
			t.Fatalf("the restarted daemon launched %s, want no role launched but the controller", launch.claim)
		}
	}
	if starts := r.startRows(issues[2], claim.RoleImplementer); len(starts) != 0 {
		t.Fatalf("the restart queued the starts %v for S, the worker the pause stopped with nothing held, want none", starts)
	}
	if s := r.claim(S); s.State != supervise.StateSuspended || !s.Released() {
		t.Fatalf("S after the restart is %s by %s, want it still stopped by the pause", s.State, s.SuspendedBy)
	}
	if !slices.ContainsFunc(state.Paused.Held, func(row api.PauseHeldView) bool { return row.Claim == H }) {
		t.Fatalf("H's start is not among the rows the pause holds after the restart: %+v", state.Paused.Held)
	}

	// G, in its turn across the restart, stops when that turn ends.
	r.handle(G, supervise.StreamTurnEnd{Claim: G})
	if c := r.awaitState(G, supervise.StateSuspended, "G stops when its turn ends after the restart"); !c.Released() {
		t.Fatalf("G after its turn is suspended by %s, want released", c.SuspendedBy)
	}

	// After the resume, H, T and R each launch once through the pacing, and S does not.
	before := map[claim.Token]int{}
	for _, token := range []claim.Token{H, S, T, R} {
		before[token] = len(r.rt.launchesOf(token))
	}
	r.resumeProject()
	for _, token := range []claim.Token{H, T, R} {
		r.awaitLaunch(token, before[token]+1)
	}
	r.advance(time.Minute)
	launched := 0
	for _, token := range []claim.Token{H, T, R} {
		got := len(r.rt.launchesOf(token)) - before[token]
		if got != 1 {
			t.Fatalf("%s launched %d times after the resume, want once", token, got)
		}
		lines := r.linesOf(supervise.WakeRanMessage, token)
		if len(lines) == 0 || lines[len(lines)-1]["paced"] != true {
			t.Fatalf("%s's launch after the resume wrote %v, want a wake line that says paced", token, lines)
		}
		launched += got
	}
	if got := len(r.rt.launchesOf(S)) - before[S]; got != 0 {
		t.Fatalf("S launched %d times after the resume, want none: nothing was held for it", got)
	}
	t.Logf("k3: launched after the resume=%d, held rows after the restart=%d, launches the restart made=%d (none but the controller's allowed)",
		launched, len(state.Paused.Held), len(bootLaunches))
	if launched == 0 || len(state.Paused.Held) == 0 {
		t.Fatalf("k3 read %d launches and %d held rows, want both above 0", launched, len(state.Paused.Held))
	}
}

// k4: a pause keeps the pods whose roles it stopped. Pod 1 holds an idle worker of an open phase
// with unpushed work; pod 2 an idle worker of an open phase and an open update round; pod 3 an
// idle worker of an open phase and an architect in a turn that ends after the worker has
// stopped; pod 4 only an idle architect; pod 5's issue is closed while the project is paused; and
// pod 6, beyond the check's five (plan task t10), holds an idle worker of an open phase, an
// architect still in a turn and an open update round, so that the replacement's wait for the
// pause's hold (decision 12) is the only thing that keeps it past the round's bound. The test
// pauses, advances the clock past the idle period and past the rounds' bound, resumes, advances
// past the idle period again, and then sends a wake to pod 1's worker.
//
// The negative control is the pause's hold taken out of workflow.UnownedPodRelease: pod 4 is
// deleted after the idle period once the project resumes, and the test fails there.
func TestPauseKeepsThePods(t *testing.T) {
	r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.AdmissionCap = 8 })
	// The update rounds' sweep runs beside the machines (overrides.orphanSweep), reading the test
	// clock at each pass.
	r.o.orphanSweep = 200 * time.Millisecond
	pods := []string{r.issue(1), r.issue(2), r.issue(3), r.issue(4), r.issue(5), r.issue(6)}
	worker := func(n int) claim.Token { return r.token(pods[n-1], claim.RoleImplementer) }
	architect := func(n int) claim.Token { return r.token(pods[n-1], claim.RoleArchitect) }
	opened := r.clock.Now().UTC().Add(-time.Minute)
	r.seed(r.root(pods[0], phase.Implementing), r.running(pods[0], claim.RoleImplementer, supervise.StateIdle))
	r.seed(r.root(pods[1], phase.Implementing), inPodRound(r.running(pods[1], claim.RoleImplementer, supervise.StateIdle), opened))
	r.seed(r.root(pods[2], phase.Implementing), r.running(pods[2], claim.RoleImplementer, supervise.StateIdle),
		r.running(pods[2], claim.RoleArchitect, supervise.StateWorking))
	r.seed(r.root(pods[3], phase.Implementing), r.running(pods[3], claim.RoleArchitect, supervise.StateIdle))
	r.seed(r.root(pods[4], phase.Implementing), r.running(pods[4], claim.RoleImplementer, supervise.StateIdle))
	r.seed(r.root(pods[5], phase.Implementing), inPodRound(r.running(pods[5], claim.RoleImplementer, supervise.StateIdle), opened),
		inPodRound(r.running(pods[5], claim.RoleArchitect, supervise.StateWorking), opened))
	r.boot()

	pause := r.pauseProject()
	for _, token := range []claim.Token{worker(1), worker(2), worker(3), architect(4), worker(5), worker(6)} {
		if c := r.awaitState(token, supervise.StateSuspended, "the pause stops an idle role at once"); !c.Released() {
			t.Fatalf("%s after its stop is suspended by %s, want released", token, c.SuspendedBy)
		}
	}
	for _, token := range []claim.Token{architect(3), architect(6)} {
		if c := r.claim(token); c.State != supervise.StateWorking {
			t.Fatalf("%s is %s once the pause asked its stop, want its turn still running", token, c.State)
		}
	}
	for _, n := range []int{1, 2, 4, 5} {
		if !r.openPodHold(pods[n-1]) {
			t.Fatalf("no pause holds pod %d once the pause stopped its roles", n)
		}
	}
	backstop := r.count(`select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer' and not fired`, pods[2])
	if backstop != 1 {
		t.Fatalf("pod 3's worker has %d backstops armed at its stop, want one: it is the worker of an open phase owed no launch", backstop)
	}

	// Pod 5's issue is closed while the project is paused, and the close deletes its pod.
	r.board.show(pods[4], "backlog")
	r.applyFact("dispatch", "close-5", intake.DispatchIssue{Key: pods[4], Seq: 2, Type: "task", Status: "backlog", Title: pods[4],
		Rank: "U", HandedOver: true, ResolvedRepo: liveRepo})
	testwait.Eventually(t, "the close of pod 5's issue to delete its pod", func() bool {
		deleted, _ := r.rt.podReads()
		return slices.Contains(deleted, pods[4])
	})
	// The row that deleted the pod ends the pause's hold of it once the runtime has answered
	// (outbox.suspendIssue, podDeleted).
	testwait.Eventually(t, "the pause's hold of pod 5 to end with its deletion", func() bool {
		return r.count(`select count(*) from project_pause_pod_holds where issue = $1 and ended_why = 'deleted'`, pods[4]) == 1
	})

	// Pod 3's architect ends its turn after its worker has stopped; the worker keeps its backstop.
	r.handle(architect(3), supervise.StreamTurnEnd{Claim: architect(3)})
	r.awaitState(architect(3), supervise.StateSuspended, "pod 3's architect stops at its turn's end")
	kept := r.count(`select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer' and not fired`, pods[2])
	if kept != 1 {
		t.Fatalf("pod 3's worker has %d backstops armed once its architect stopped, want the one its own stop armed", kept)
	}

	// Past the idle period and past the rounds' bound, with the project paused.
	r.advance(pauseRigPodUpdateGrace + time.Minute)
	r.advance(pauseRigIdleMinutes * time.Minute)
	deleted, bundled := r.rt.podReads()
	if !slices.Equal(deleted, []string{pods[4]}) || len(bundled) != 0 {
		t.Fatalf("while the project is paused the runtime deleted the pods of %v and bundled %v, want pod 5's deletion alone",
			deleted, bundled)
	}
	if saved := r.count(`select count(*) from workspace_bundles`); saved != 0 {
		t.Fatalf("%d workspaces saved while the project is paused, want none", saved)
	}
	roundsHeld := 0
	for _, n := range []int{2, 6} {
		issue := pods[n-1]
		replaced := r.replacedPods(issue)
		if replaced != 0 {
			t.Fatalf("pod %d's update round replaced its pod %d times while the pause held it, want none", n, replaced)
		}
		c := r.claim(worker(n))
		if !c.InPodRound() || c.UpdateToldAt != nil {
			t.Fatalf("pod %d's worker is in round %q told at %v while the pause holds the pod, want the round open and its bound not reached",
				n, c.UpdateRound, c.UpdateToldAt)
		}
		told := r.count(`select count(*) from outbox where kind = 'controller_notice' and payload->>'kind' = 'update-stalled'
			and issue = $1`, issue)
		if told != 0 {
			t.Fatalf("pod %d's round was told past its bound %d times while the pause held the pod, want never", n, told)
		}
		if replaced == 0 && c.InPodRound() && c.UpdateToldAt == nil && told == 0 {
			roundsHeld++
		}
	}
	if c := r.claim(architect(6)); c.State != supervise.StateWorking {
		t.Fatalf("pod 6's architect is %s, want its turn still running in the pod the pause holds", c.State)
	}
	for _, n := range []int{3, 6} {
		if !r.openPodHold(pods[n-1]) {
			t.Fatalf("no pause holds pod %d, whose worker the pause stopped while its architect's turn ran", n)
		}
	}

	// The resume, and the idle period again: no pod none of whose roles ran again is deleted, pod
	// 4's included.
	resumed := r.resumeProject()
	if resumed.Pause.ID != pause.ID {
		t.Fatalf("the resume ended pause %d, want %d", resumed.Pause.ID, pause.ID)
	}
	r.advance(pauseRigIdleMinutes*time.Minute + time.Minute)
	r.advance(time.Minute)
	deleted, bundled = r.rt.podReads()
	if !slices.Equal(deleted, []string{pods[4]}) || len(bundled) != 0 {
		t.Fatalf("after the resume the runtime deleted the pods of %v and bundled %v, want pod 5's deletion alone: pod 4, which holds only an architect, stands too",
			deleted, bundled)
	}
	for _, issue := range []string{pods[0], pods[1], pods[2], pods[3]} {
		if !r.openPodHold(issue) {
			t.Fatalf("no pause holds %s's pod after the resume, want the hold to stand until a role of it runs again", issue)
		}
	}
	held := r.count(`select count(*) from project_pause_pod_holds where issue = any($1) and ended_at is null`, pods[:4])

	// The wake launches pod 1's worker in pod 1, on its own session and disk, and the hold ends at
	// the worker's ready. The wake is a delivery of the daemon for the stopped worker, a hook's
	// prompt, which the outbox runs on its own clock and wakes the role for (wakeForDelivery): a
	// listener's wake would queue its start stamped with the test clock, which the rig has moved
	// past the outbox's wall clock by now.
	launches := len(r.rt.launchesOf(worker(1)))
	r.enqueue(pods[0], record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing,
		To: phase.Implementing, Count: 1, Counts: map[string]int{phase.Pair(phase.Reviewing, phase.Implementing): 1}, Prompt: "Read the review."})
	r.awaitLaunch(worker(1), launches+1)
	launch := lastLaunch(t, r.rt.Runtime, worker(1))
	if launch.ResumeSessionFile != "/sessions/"+string(worker(1))+".jsonl" {
		t.Fatalf("pod 1's worker was launched on session file %q, want its own session", launch.ResumeSessionFile)
	}
	if _, present := r.rt.IssueSandbox(pods[0]); !present {
		t.Fatal("pod 1's Sandbox is gone at the wake, want the worker launched in the pod that holds its work")
	}
	deleted, bundled = r.rt.podReads()
	if slices.Contains(deleted, pods[0]) || len(bundled) != 0 {
		t.Fatalf("pod 1 deleted (%v) or a workspace bundled (%v) at the wake, want the disk kept", deleted, bundled)
	}
	r.ready(worker(1))
	testwait.Eventually(t, "the hold on pod 1 to end at its worker's ready", func() bool {
		return r.count(`select count(*) from project_pause_pod_holds where issue = $1 and ended_why = 'ready'`, pods[0]) == 1
	})
	if r.openPodHold(pods[0]) {
		t.Fatal("the pause still holds pod 1 once its worker is ready")
	}

	t.Logf("k4: pods=%d, deleted=%v, held after the resume=%d, rounds held=%d, backstops kept=%d", len(pods), deleted, held, roundsHeld, kept)
	if held == 0 || roundsHeld == 0 || kept == 0 {
		t.Fatalf("k4 read %d pods held after the resume, %d rounds held and %d backstops kept, want each above 0", held, roundsHeld, kept)
	}
}

// replacedPods is how many times the runtime was asked to replace issue's pod for an update round,
// through a claim of the issue.
func (r *pauseDaemonRig) replacedPods(issue string) int {
	count := 0
	for _, call := range r.rt.CallsOf("ReplacePod") {
		if strings.Contains(string(call.Pod.Claim), "-"+strings.ToLower(issue)+"-") {
			count++
		}
	}
	return count
}

// awaitLaunch waits until token has been launched want times, moving the test clock to the turn
// the wake pacing gave its launch where the pacing holds it.
func (r *pauseDaemonRig) awaitLaunch(token claim.Token, want int) {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("launch %d of %s", want, token), func() bool {
		if len(r.rt.launchesOf(token)) >= want {
			return true
		}
		for _, wake := range r.state().WakesWaiting {
			if wake.Claim == token {
				r.clock.advanceTo(wake.DueAt)
			}
		}
		return false
	})
}

// ready walks token's launched claim through its shim's hello, its agent's registration and its
// ready, at the claim's generation, on the session the claim keeps, as readyClaimOf does.
func (r *pauseDaemonRig) ready(token claim.Token) {
	r.t.Helper()
	c := r.claim(token)
	r.handle(token, supervise.StreamHello{Claim: token, Generation: c.Generation})
	r.handle(token, supervise.RequestRegister{Claim: token, Generation: c.Generation, Session: c.Session, SessionFile: c.SessionFile,
		PluginContract: api.DaemonAPIVersion})
	r.handle(token, supervise.RequestReady{Claim: token, Generation: c.Generation, Session: c.Session})
	r.awaitState(token, supervise.StateReady, "the woken role is ready")
}

// turnStartedLine is the line a machine writes when its claim's turn starts (supervise's
// turnStarted), which k1 counts after the pause.
func turnStartedLine() string { return "supervise: a turn started" }

// pullRequest records issue's open pull request number, its head at head.
func (r *pauseDaemonRig) pullRequest(issue string, number int, head string) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutPullRequest(r.ctx, tx, record.PullRequest{Issue: issue, Repo: liveRepo, Number: number,
			Branch: "legion/" + issue, HeadSHA: head, HeadUpdatedAt: r.clock.Now().Add(-time.Hour).UTC(), State: record.PullRequestOpen})
	}); err != nil {
		r.t.Fatalf("record the pull request of %s: %v", issue, err)
	}
}

// noPrompt is sh reading no prompt from the daemon within a second: whatever else the daemon
// sends on the connection, it sends the agent no prompt. The read ends at its deadline; a
// connection that ends any other way fails the check, since a closed one would read no prompt
// whatever the daemon meant to send.
func (r *pauseDaemonRig) noPrompt(sh *shim, why string) {
	r.t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if err := sh.conn.SetReadDeadline(deadline); err != nil {
			r.t.Fatalf("set the read deadline: %v", err)
		}
		line, err := sh.lines.ReadBytes('\n')
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return
		}
		if err != nil {
			r.t.Fatalf("%s: the connection ended before the second was out: %v", why, err)
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			r.t.Fatalf("decode %q: %v", line, err)
		}
		if prompt, ok := frame.(shimwire.Prompt); ok {
			r.t.Fatalf("%s: the daemon prompted %q", why, prompt.Message)
		}
	}
}

// noneDue is no row of held claimable by the project's own outbox lanes: the outbox's claim query
// (record.Postgres.DueRows) leaves each out while the pause stands, read at the later of the test
// clock, which the engine stamps its rows with, and the wall clock, which the outbox reads.
func (r *pauseDaemonRig) noneDue(held []api.PauseHeldView) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		due, err := r.records.DueRows(r.ctx, tx, r.cfg.Project, "", nil, r.later())
		if err != nil {
			return err
		}
		for _, row := range due {
			if slices.ContainsFunc(held, func(h api.PauseHeldView) bool { return h.ID == row.ID }) {
				return fmt.Errorf("held row %d (%s of %s) is due to the project's lanes", row.ID, row.Kind, row.Issue)
			}
		}
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

// claimsRowOf is row of project, a project no pause names, read by the outbox's claim query and
// claimed by it on the same database, as before any pause.
func (r *pauseDaemonRig) claimsRowOf(project string, id int64) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		now := r.later()
		due, err := r.records.DueRows(r.ctx, tx, project, "", nil, now)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(due, func(row record.OutboxRow) bool { return row.ID == id }) {
			return fmt.Errorf("project %s's row %d is not due to its lanes (due: %+v)", project, id, due)
		}
		if _, claimed, err := r.records.ClaimRow(r.ctx, tx, id, now, time.Minute); err != nil || !claimed {
			return fmt.Errorf("claim project %s's row %d = %v, %v; want it claimed", project, id, claimed, err)
		}
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}
