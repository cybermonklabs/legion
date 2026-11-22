//go:build e2e

// Check w1 of this change's verification (R1: a worker waits for the daemon), run on the rig
// (RIG.md) as the check states it. scripts/e2e/daemon-wait.sh builds this driver and runs it; with
// --base-commit it runs step 1 against the base's code, the check's negative control.
//
// The first worker is the merger of the first issue, in merging, whose real pane stands idle after
// its start's task; its workspace is a jj repository, since a merger's completion reports the
// commit `@-` and no handoff record (pi-shared's handoff-record.ts), and the workflow records a
// merger's completion in merging and moves the issue nowhere, so the same worker completes in
// both outages. Its turns go to a scripted model stand-in (late_completion_live_test.go's
// completionStandin), which holds the turn the step delivers until the daemon has stopped and then
// answers it with one `legion` call of `{op: "handoff_complete"}`, so the call is made during the
// outage on cue. The tool's answer comes back to the stand-in as the next request's tool_result.
// Every launch is told LEGION_STALL_CALL_BOUND_MS below the outage (w1Bound against w1Outage).
//
//   - step 1: the daemon is stopped with its port closed (r.d.stop()) at the moment the relay
//     carries a second claim's hello_ack, before its claims/register; the merger's call is made
//     in the outage; 55 minutes later the daemon starts again. From the moment the daemon closes
//     its listener until just before the restart takes the port again, the rig holds the port with
//     a socket bound and never listening (w1PortReservation): a connection is refused as a closed
//     port's is, and no other process on the host can listen on it and answer in the daemon's
//     place.
//   - step 2: the daemon is stopped when it has answered a third claim's claims/register, so its
//     claims/ready meets the outage, and its port is held open and answers nothing
//     (r.holdPortAgain()); the merger's call is made in the outage; 55 minutes later the hung
//     listener is closed, as a hung daemon's socket closes when it is replaced, and the daemon
//     starts again.
//
// After each start the step waits past the registration deadline (testConfig's 120 s × 3), then
// reads `legion claims list --json`, the first issue's record (GET /legion/v1/state), the merger's
// phase row, the daemon's log, every request the daemon's API port was sent (w1Gate, which keeps
// an access log of the port and stops requests from reaching a daemon that is stopping), each
// pane's log and each session's transcript, where the plugin's stall watch writes its records
// (`legion-stall-watch`, packages/pi-legion/src/stall-evidence.ts).
//
// Each claim's charges are read where the daemon records them (w1Charges): every line of the
// daemon's log of that claim, from the outage's stop through the read past the deadline, that
// charges a launch failure or a death, says why, or launches the claim again. The counters
// (`budgets`) are read as well, but they cannot show a launch failure on their own: every ready
// clears it. Each boot logs with the rig's own boot number (w1Boot), so a line that a stopped
// boot's claim machine still writes after its stop is told from the serving daemon's.
//
// The stall watch's all-clear has a control in step 1: a fourth claim's turn holds a foreground
// `bash` call (`pty: true`, `timeout: 0`) open, a call no deadline judges and no daemon wait
// explains, so the watch must write its record once that call has run past the bound, in the same
// outage, under the same environment. A watch the panes never armed, one that stops sweeping while
// the daemon is down, or a transcript this driver reads wrongly, fails the control.
//
// Every daemon text a step reads is written here as a literal, so step 1 builds against the base:
// daemon-wait.sh's negative control lays this file and the rig's over the base's tree.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The check's numbers: the outage, the bound every launch is told (below the outage), the rig's
// registration deadline (testConfig: WorkerBootTimeout 120 s × WorkerBootRegistrationDeadlineIntervals
// 3) and how far past it the step reads, and the bounds of the waits around them.
const (
	w1Outage               = 55 * time.Minute
	w1Bound                = 20 * time.Minute
	w1RegistrationDeadline = 6 * time.Minute
	w1PastDeadline         = 90 * time.Second
	w1LaunchWait           = 4 * time.Minute
	w1AnswerWait           = 10 * time.Minute
	w1IdleWait             = 6 * time.Minute
	w1ReachWait            = 3 * time.Minute
	w1PortProbeLimit       = 5 * time.Second
)

// The environment the driver reads beyond the rig's: a shorter outage for a rehearsal of the
// steps (a run with one never passes), and the jj the merger's pane runs, which must run under the
// pane's own HOME (daemon-wait.sh resolves it).
const (
	w1OutageEnv = "LEGION_E2E_W1_OUTAGE"
	w1JJEnv     = "LEGION_E2E_W1_JJ"
)

// The daemon's and the plugins' texts a step reads, as they write them: the completion route's
// line (api's handoffComplete), the plugin's line when a call begins to wait for the daemon
// (pi-shared's retryWhileDaemonRestarts), and the stall watch's transcript entry and restart
// message (pi-legion's stall-evidence.ts and stall-abort.ts).
const (
	w1CompletionAnswered = "api: handoff completion answered"
	w1Unreachable        = "The Legion daemon is unreachable"
	w1StallWatchEntry    = "legion-stall-watch"
	w1StallRestart       = "legion-stall-restart"
	w1RegisterPath       = "/legion/v1/claims/register"
	w1ReadyPath          = "/legion/v1/claims/ready"
	w1CompletePath       = "/legion/v1/handoff/complete"
	w1ControlProvider    = "rig-control"
	w1StartTask          = "Reply with the one word READY and nothing else. Call no tool."
)

// The daemon's lines that charge a claim, say why, or launch it again, as internal/supervise
// writes them (machine.go's launchNow, backOff, died and fail, budgets.go's chargeDeath and
// table.go's registrationDeadline), which w1Charges reads; and the attribute every boot's log
// carries the rig's own boot number under (bootWith).
const (
	w1Launched      = "supervise: launched"
	w1LaunchFailed  = "supervise: launch failed"
	w1BackingOff    = "supervise: waiting before the next launch"
	w1RetiredSuffix = "; retired its process"
	w1ProcessDied   = "supervise: process died"
	w1DiedWithWork  = "supervise: the agent died with work outstanding"
	w1ClaimFailed   = "supervise: claim failed"
	w1BootAttr      = "w1Boot"
)

const (
	w1Step1 = "step1-a-closed-port-outage-is-waited-out-by-a-completion-and-by-a-start-before-its-register"
	w1Step2 = "step2-a-held-port-outage-is-waited-out-by-a-completion-and-by-a-ready-after-its-register"
)

// w1Live is one run of w1.
type w1Live struct {
	*rig
	outage, bound time.Duration
	// merger is the first issue, worked by the first worker; startA and startB the issues whose
	// claim boots meet the two outages; control the issue of the stall watch's control.
	merger, startA, startB, control string
	stand                           *completionStandin
	controlStand                    *scriptedStandin
	http                            *w1HTTPLog
	jj, workspace                   string
	results                         *os.File
	boots                           int
	// stopped is when each boot's daemon had stopped (stopDaemon), by the rig's boot number: a
	// line its log carries after that is a stopped boot's, not the serving daemon's.
	stopped map[int]time.Time
	// gates is every boot's API gate, whose held connections the end of a held-port outage ends.
	gates []*w1Gate

	// portMu guards the closed-port outage's reservation of the daemon's port, which the daemon's
	// own stop takes (w1Gate.Close) and the step reads once the daemon has stopped.
	portMu     sync.Mutex
	reserved   *w1PortReservation
	reserveErr error
}

// TestAWorkerWaitsForTheDaemonLive runs w1's two outages, the second on the claims the first left.
func TestAWorkerWaitsForTheDaemonLive(t *testing.T) {
	r := newW1Live(t)
	r.step(w1Step1, r.closedPortOutage)
	r.step(w1Step2, r.heldPortOutage)
	if r.outage != w1Outage {
		t.Errorf("this run was a rehearsal with a %s outage (%s), not the check's %s: it proves nothing", r.outage, w1OutageEnv, w1Outage)
	}
}

func newW1Live(t *testing.T) *w1Live {
	t.Helper()
	outage, bound := w1Outage, w1Bound
	if named := os.Getenv(w1OutageEnv); named != "" {
		parsed, err := time.ParseDuration(named)
		if err != nil || parsed <= 0 {
			t.Fatalf("%s=%q is no positive duration", w1OutageEnv, named)
		}
		outage = parsed
		bound = max((outage * 4 / 11).Round(time.Second), time.Minute)
	}
	// Every launch is told the bound (internal/daemon's stallLimitVariables passes the daemon's
	// own environment on), and the daemon holds it as config.Load would have read it.
	t.Setenv("LEGION_STALL_CALL_BOUND_MS", strconv.FormatInt(bound.Milliseconds(), 10))
	r := &w1Live{rig: startRig(t, rigOptions{script: "scripts/e2e/daemon-wait.sh", standin: true}), outage: outage, bound: bound,
		stopped: map[int]time.Time{}}
	r.cfg.StallLimits.CallBound = &bound
	// The loader's own default (config's resolve), which testConfig leaves at zero: the merger's
	// tool reads GET /legion/v1/state through the daemon API contract, which takes a positive cap
	// alone and refuses the whole state otherwise (late_completion_live_test.go sets it too).
	r.cfg.ReviewRoundCap = 3
	results, err := os.OpenFile(filepath.Join(r.evidence, "w1-results.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the results file: %v", err)
	}
	t.Cleanup(func() { results.Close() })
	r.results = results
	r.merger, r.startA, r.startB, r.control = r.cfg.Project+"-1", r.cfg.Project+"-2", r.cfg.Project+"-3", r.cfg.Project+"-4"
	r.http = &w1HTTPLog{resolve: r.runtime.bootTokenOf}
	r.writeStandins()
	r.makeWorkspace()

	r.result("outage %s; LEGION_STALL_CALL_BOUND_MS=%d (%s) told to every launch and held by the daemon; registration deadline %s",
		r.outage, bound.Milliseconds(), bound, w1RegistrationDeadline)
	r.seed(rigSeed{issue: r.merger, phase: phase.Merging, role: claim.RoleMerger, task: w1StartTask},
		rigSeed{issue: r.control, phase: phase.Implementing, role: claim.RoleImplementer, task: w1StartTask})
	r.bootWith(r.newGate(nil))
	for _, token := range []claim.Token{r.token(r.merger, claim.RoleMerger), r.token(r.control, claim.RoleImplementer)} {
		r.awaitReady(token)
		r.awaitIdle(token)
	}
	m := r.claim(r.token(r.merger, claim.RoleMerger))
	r.result("the first worker: %s, issue %s in phase %s (GET /legion/v1/state), claim %s at generation %d in session %s, %s",
		m.Token, r.merger, r.issuePhase(r.merger), m.State, m.Generation, m.Session, w1Counts(m.Budgets))
	return r
}

// ---- the steps ----------------------------------------------------------------------------------

// closedPortOutage is the check's first outage: the daemon stops with its port closed after the
// second claim's shim got its hello_ack and before that claim's claims/register, and the first
// worker calls handoff_complete during the outage.
func (r *w1Live) closedPortOutage() {
	o := r.armOutage("closed-port", "1")
	control := r.armControl()
	second := r.token(r.startA, claim.RoleImplementer)
	launch := r.bootStoppingAtHelloAck(r.startA, second)
	r.runOutage(o, launch, &control)
}

// heldPortOutage is the check's second outage: the daemon stops when it has answered the third
// claim's claims/register, so its claims/ready meets the outage, and its port stays held open and
// answers nothing; the first worker calls handoff_complete during the outage.
func (r *w1Live) heldPortOutage() {
	o := r.armOutage("held-port", "2")
	third := r.token(r.startB, claim.RoleImplementer)
	launch := r.bootStoppingAtRegister(r.startB, third)
	r.runOutage(o, launch, nil)
}

// w1OutageRun is what one outage recorded, which judge reads.
type w1OutageRun struct {
	name, marker, commit string
	merger               claim.Token
	call                 *paneCall
	before               api.OperatorClaim
	httpFrom             int

	mu         sync.Mutex
	answered   bool
	answer     toolResult
	answeredAt time.Time
	releasedAt time.Time
	servingAt  time.Time
	logFrom    int
	ended      map[claim.Token]time.Time
	// paneLaunch and paneOffset are each watched claim's process at the outage's start and the
	// length of its log then, from which its pane's lines are read.
	paneLaunch map[claim.Token]*rigLaunch
	paneOffset map[claim.Token]int64
	// portProbes is every connect the closed-port outage made to the reserved port while it held
	// it, and portAnswered each that was not refused as a closed port's connection is.
	portProbes, portAnswered []string
}

// w1Launch is the boot of a claim the outage met: when the daemon was stopped to seed it and
// started again, the launch, its shim's hello and ack, when the rig decided to stop the daemon and
// when it stopped, the claim as the store held it then, and how many lines the daemon's log held
// when the rig decided to stop it: the outage's charges are read from there on (w1Charges).
type w1Launch struct {
	token                                         claim.Token
	key                                           string
	seededAt, serving, triggeredAt, stoppedAt     time.Time
	heldAt                                        time.Time
	launch                                        *rigLaunch
	helloAt, ackAt, registerAnsweredAt, decidedBy time.Time
	atStop                                        supervise.Claim
	bootNumber, logFrom                           int
	held                                          bool
}

// w1Control is the stall watch's control: a claim whose turn holds a foreground shell call that
// declares no deadline open past the bound, which nothing but the bound judges.
type w1Control struct {
	token   claim.Token
	started time.Time
}

// armOutage readies the first worker's call for one outage: a new commit under its workspace's
// `@-`, the claim's counts before, and its turn delivered and held at the stand-in, so the call is
// made when the step lets it go.
func (r *w1Live) armOutage(name, round string) *w1OutageRun {
	m := r.token(r.merger, claim.RoleMerger)
	o := &w1OutageRun{name: name, merger: m, marker: "W1-OUTAGE-" + strings.ToUpper(name) + "-" + randomSuffix(r.t),
		ended: map[claim.Token]time.Time{}, paneLaunch: map[claim.Token]*rigLaunch{}, paneOffset: map[claim.Token]int64{}}
	o.commit = r.commitHead(round)
	o.before = r.claim(m)
	o.httpFrom = r.http.count()
	r.result("%s: before the outage %s is %s at generation %d, %s; issue %s in phase %s; its workspace's @- is %s",
		name, m, o.before.State, o.before.Generation, w1Counts(o.before.Budgets), r.merger, r.issuePhase(r.merger), o.commit)
	o.call = r.stand.arm(o.marker, map[string]any{
		"op": "handoff_complete", "summary": "Merged at the head this workspace stands on (" + o.marker + ").",
	}, true)
	go func() {
		result := <-o.call.result
		o.mu.Lock()
		o.answered, o.answer, o.answeredAt = true, result, time.Now()
		o.mu.Unlock()
	}()
	r.deliver(m, "Report the merge with the legion tool's handoff_complete. "+o.marker)
	line := o.call.awaitReached(r.t, w1ReachWait)
	r.result("%s: the turn that makes the call reached the stand-in at %s, which holds it: %q", name, clockAt(time.Now()), line)
	return o
}

// armControl starts the stall watch's control: its turn calls `bash` with `pty: true`, which the
// host keeps in the foreground (a plain call is moved to the background at 60 s and closes its
// record, as a `task` call does at once: the stall watch's measurement 1,
// packages/pi-shared/src/stall-watchdog.ts), and `timeout: 0`, which declares no deadline, so the
// call stays open with no daemon wait in it and only the bound judges it.
func (r *w1Live) armControl() w1Control {
	token := r.token(r.control, claim.RoleImplementer)
	marker := "W1-WATCHDOG-CONTROL-" + randomSuffix(r.t)
	r.controlStand.callTool(promptCarrying(marker), "bash", map[string]any{"command": "sleep 14400", "pty": true, "timeout": 0})
	from := w1FileSize(r.runtime.current(token).pane.log)
	r.deliver(token, "Run the long scratch check in the foreground. "+marker)
	var started time.Time
	if !r.within(w1ReachWait, func() bool {
		if !strings.Contains(readFrom(r.t, r.runtime.current(token).pane.log, from), "tool_execution_start bash") {
			return false
		}
		started = time.Now()
		return true
	}) {
		r.fails("watchdog-control", "the control claim %s's pane started no bash call within %s of its task; its log is %s", token, w1ReachWait, r.runtime.current(token).pane.log)
	}
	time.Sleep(liveSettle)
	if c := r.claim(token); c.State != string(supervise.StateWorking) {
		r.fails("watchdog-control", "the control claim %s is %s %s after its bash call started, want working: the call did not stay in the foreground", token, c.State, liveSettle)
	}
	c := w1Control{token: token, started: started}
	r.result("the stall watch's control: %s's turn holds a foreground bash call (pty, timeout 0) open from %s, past whose bound of %s its watch must write %s",
		token, clockAt(c.started), r.bound, w1StallWatchEntry)
	return c
}

// bootStoppingAtHelloAck stops the daemon, seeds key's claim mid-launch, starts the daemon, which
// launches it, and stops the daemon again at the moment the relay carries that claim's
// hello_ack: the API port stops taking requests at that moment (w1Gate), so the claim's
// claims/register meets a daemon that is not serving. The moment that daemon closes its listener
// the rig reserves the port closed (reservePort), and proves it refuses (proveReserved).
func (r *w1Live) bootStoppingAtHelloAck(key string, token claim.Token) w1Launch {
	l := w1Launch{token: token, key: key}
	r.stopDaemon()
	r.seed(rigSeed{issue: key, phase: phase.Implementing, role: claim.RoleImplementer, task: w1StartTask})
	l.seededAt = time.Now()
	r.holdPortAgain()
	g := r.newGate(nil)
	g.afterClose = r.reservePort
	acked := make(chan relayConn, 1)
	go func() {
		deadline := time.Now().Add(w1LaunchWait)
		for time.Now().Before(deadline) {
			if launch := r.runtime.current(token); launch != nil {
				for _, c := range r.relay.connsOf(launch.spec.BootToken) {
					if !c.ackAt.IsZero() {
						g.closeRequests()
						acked <- c
						return
					}
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		close(acked)
	}()
	r.bootWith(g)
	l.bootNumber, l.serving = g.boot, time.Now()
	conn, ok := <-acked
	if !ok {
		r.fails("stop-before-register", "the daemon launched no process of %s whose shim got a hello_ack within %s", token, w1LaunchWait)
	}
	l.helloAt, l.ackAt, l.decidedBy = conn.helloAt, conn.ackAt, g.closedAt()
	l.triggeredAt = time.Now()
	l.logFrom = r.daemon.count()
	l.stoppedAt = r.stopDaemon()
	r.proveReserved()
	l.launch = r.runtime.current(token)
	l.atStop = r.storeClaim(token)
	r.result("%s: seeded mid-launch at %s; the daemon (boot %d) served again at %s and launched generation %d (pid %d) at %s; its shim's hello at %s, hello_ack at %s; the API port stopped taking requests at %s and the daemon stopped at %s, its port closed and reserved from %s; the store then held %s %s at generation %d, %s",
		token, clockAt(l.seededAt), l.bootNumber, clockAt(l.serving), l.launch.spec.Generation, l.launch.pane.pid, clockAt(l.launch.at),
		clockAt(l.helloAt), clockAt(l.ackAt), clockAt(l.decidedBy), clockAt(l.stoppedAt), clockAt(r.reserved.at), token, l.atStop.State, l.atStop.Generation, w1StoreCounts(l.atStop.Budgets))
	return l
}

// bootStoppingAtRegister stops the daemon, seeds key's claim mid-launch, starts the daemon, which
// launches it, and stops the daemon again when it has answered that claim's claims/register: the
// API port holds every request after that answer unanswered (w1Gate), and once the daemon has
// stopped the rig holds its port open and answers nothing on it (holdPortAgain), so the claim's
// claims/ready meets the outage.
func (r *w1Live) bootStoppingAtRegister(key string, token claim.Token) w1Launch {
	l := w1Launch{token: token, key: key, held: true}
	r.stopDaemon()
	r.seed(rigSeed{issue: key, phase: phase.Implementing, role: claim.RoleImplementer, task: w1StartTask})
	l.seededAt = time.Now()
	r.holdPortAgain()
	g := r.newGate(func(ex w1Exchange) bool {
		return ex.path == w1RegisterPath && ex.claim == token && ex.status == http.StatusOK
	})
	r.bootWith(g)
	l.bootNumber, l.serving = g.boot, time.Now()
	select {
	case at := <-g.fired:
		l.registerAnsweredAt = at
	case <-time.After(w1LaunchWait):
		r.fails("stop-after-register", "the daemon answered no claims/register of %s with 200 within %s", token, w1LaunchWait)
	}
	l.triggeredAt = time.Now()
	l.logFrom = r.daemon.count()
	l.stoppedAt = r.stopDaemon()
	r.holdPortAgain()
	r.watchHeldPort()
	l.heldAt = time.Now()
	l.launch = r.runtime.current(token)
	for _, c := range r.relay.connsOf(l.launch.spec.BootToken) {
		if !c.ackAt.IsZero() {
			l.helloAt, l.ackAt = c.helloAt, c.ackAt
			break
		}
	}
	l.atStop = r.storeClaim(token)
	r.result("%s: seeded mid-launch at %s; the daemon (boot %d) served again at %s and launched generation %d (pid %d) at %s; its shim's hello at %s, hello_ack at %s; the daemon answered its claims/register 200 at %s, from when the API port held every request unanswered; the daemon stopped at %s and its port was held open, answering nothing, from %s; the store then held %s %s at generation %d, %s",
		token, clockAt(l.seededAt), l.bootNumber, clockAt(l.serving), l.launch.spec.Generation, l.launch.pane.pid, clockAt(l.launch.at),
		clockAt(l.helloAt), clockAt(l.ackAt), clockAt(l.registerAnsweredAt), clockAt(l.stoppedAt), clockAt(l.heldAt), token, l.atStop.State, l.atStop.Generation, w1StoreCounts(l.atStop.Budgets))
	return l
}

// runOutage lets the first worker's call go, waits the outage out, starts the daemon again, waits
// past the registration deadline, and judges what the outage left.
func (r *w1Live) runOutage(o *w1OutageRun, l w1Launch, control *w1Control) {
	watched := []claim.Token{o.merger, l.token}
	if control != nil {
		watched = append(watched, control.token)
	}
	for _, token := range watched {
		launch := r.runtime.current(token)
		o.paneLaunch[token], o.paneOffset[token] = launch, w1FileSize(launch.pane.log)
	}
	o.call.release()
	o.releasedAt = time.Now()
	r.result("%s: the outage began at %s; the stand-in let the first worker's turn go at %s, answering it with the legion call %s",
		o.name, clockAt(l.stoppedAt), clockAt(o.releasedAt), o.call.id)
	until := l.stoppedAt.Add(r.outage)
	r.waitOut(o, until, watched)

	if l.held {
		r.dropHeldPort()
		r.closeParked()
	} else {
		r.probePort(o, "at the outage's end, before its release")
		r.releaseReservation()
		r.result("%s: the reservation of the daemon's port was released at %s, for the restart to take the port", o.name, clockAt(r.reserved.released))
	}
	r.holdPortAgain()
	o.logFrom = r.daemon.count()
	r.bootWith(r.newGate(nil))
	o.servingAt = time.Now()
	r.result("%s: the daemon (boot %d) served again at %s, %s after it stopped", o.name, r.boots, clockAt(o.servingAt), o.servingAt.Sub(l.stoppedAt).Round(time.Second))
	r.judge(o, l, control)
}

// waitOut watches the outage until until: the processes of the watched claims, the first
// worker's call and whether it was answered, and each pane's own lines about the daemon.
func (r *w1Live) waitOut(o *w1OutageRun, until time.Time, watched []claim.Token) {
	launches := map[claim.Token]*rigLaunch{}
	for _, token := range watched {
		launches[token] = r.runtime.current(token)
	}
	told := false
	progress := time.Now()
	for time.Now().Before(until) {
		for _, token := range watched {
			launch := launches[token]
			if _, ended := o.ended[token]; !ended && !launch.running() {
				o.ended[token] = time.Now()
				r.result("%s: %s's process (generation %d, pid %d) ended at %s, %s into the outage; its log is %s",
					o.name, token, launch.spec.Generation, launch.pane.pid, clockAt(o.ended[token]), time.Since(o.releasedAt).Round(time.Second), launch.pane.log)
			}
		}
		o.mu.Lock()
		answered, answer, at := o.answered, o.answer, o.answeredAt
		o.mu.Unlock()
		if answered && !told {
			told = true
			r.result("%s: the first worker's handoff_complete was answered at %s, %s after its release and during the outage, error=%t: %q",
				o.name, clockAt(at), at.Sub(o.releasedAt).Round(time.Second), answer.isError, w1Short(answer.text))
		}
		if time.Since(progress) >= 5*time.Minute {
			progress = time.Now()
			var running []string
			for _, token := range watched {
				running = append(running, fmt.Sprintf("%s running=%t", token, launches[token].running()))
			}
			r.note("%s: %s into the outage, %s left; %s; the call answered: %t", o.name, time.Since(o.releasedAt).Round(time.Second),
				time.Until(until).Round(time.Second), strings.Join(running, ", "), answered)
			if r.holdsReservation() {
				r.probePort(o, time.Since(o.releasedAt).Round(time.Second).String()+" into the outage")
			}
		}
		time.Sleep(min(10*time.Second, max(time.Until(until), 0)))
	}
}

// judge reads what the outage left once the daemon serves again, records it, and fails the step
// at the first of the check's clauses that does not hold, naming every one that did not.
func (r *w1Live) judge(o *w1OutageRun, l w1Launch, control *w1Control) {
	var problems []w1Problem
	fail := func(assertion, format string, args ...any) {
		problems = append(problems, w1Problem{assertion: assertion, sentence: fmt.Sprintf(format, args...)})
	}

	// The precondition: the outage met the boot where the step meant it to.
	exchanges := r.http.since(o.httpFrom)
	for _, ex := range exchanges {
		if ex.boot != l.bootNumber || ex.claim != l.token || ex.gated != "" || ex.answeredAt.IsZero() {
			continue
		}
		switch {
		case !l.held && ex.path == w1RegisterPath:
			fail("stop-before-register", "boot %d answered %s's claims/register %d at %s, before the daemon stopped at %s: the outage did not meet the registration",
				l.bootNumber, l.token, ex.status, clockAt(ex.answeredAt), clockAt(l.stoppedAt))
		case l.held && ex.path == w1ReadyPath:
			fail("stop-after-register", "boot %d answered %s's claims/ready %d at %s, before the daemon stopped at %s: the outage did not meet the ready",
				l.bootNumber, l.token, ex.status, clockAt(ex.answeredAt), clockAt(l.stoppedAt))
		}
	}
	// A closed-port outage's precondition: every connect the rig made to the reserved port while it
	// held it was refused, as a closed port's connection is, and it made some.
	if !l.held {
		switch {
		case len(o.portProbes) == 0:
			fail("closed-port-held", "the rig made no connect to the reserved port during the outage, so nothing shows the port stayed closed")
		case len(o.portAnswered) > 0:
			fail("closed-port-held", "%d of the %d connects to the reserved port were not refused: %s", len(o.portAnswered), len(o.portProbes), strings.Join(o.portAnswered, "; "))
		}
	}

	// For each outage, the completion is applied after the daemon starts.
	if !r.within(w1AnswerWait, func() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.answered }) {
		fail("completion-after-start", "the first worker's handoff_complete had no answer %s after the daemon served again at %s", w1AnswerWait, clockAt(o.servingAt))
	}
	o.mu.Lock()
	answered, answer, answeredAt := o.answered, o.answer, o.answeredAt
	o.mu.Unlock()
	if answered {
		r.result("%s: the first worker's handoff_complete was answered at %s, %s after its release, %s the daemon served again, error=%t: %q",
			o.name, clockAt(answeredAt), answeredAt.Sub(o.releasedAt).Round(time.Second), w1Relative(answeredAt, o.servingAt), answer.isError, w1Short(answer.text))
		switch {
		case answeredAt.Before(o.servingAt):
			fail("completion-after-start", "the handoff_complete call was answered during the outage, %s after its release and %s before the daemon served again, error=%t: %q",
				answeredAt.Sub(o.releasedAt).Round(time.Second), o.servingAt.Sub(answeredAt).Round(time.Second), answer.isError, w1Short(answer.text))
		case answer.isError:
			fail("completion-after-start", "the handoff_complete call was answered after the daemon served again, as an error: %q", w1Short(answer.text))
		}
	}
	lines := r.daemon.matching(func(rec loggedRecord) bool { return true })
	var answeredLines []loggedRecord
	for i, rec := range lines {
		if i >= o.logFrom && rec.Message == w1CompletionAnswered && rec.Attrs["claim"] == string(o.merger) {
			answeredLines = append(answeredLines, rec)
		}
	}
	for _, rec := range answeredLines {
		r.result("%s: daemon: %s", o.name, w1Describe(rec))
	}
	applied := slices.ContainsFunc(answeredLines, func(rec loggedRecord) bool {
		return fmt.Sprint(rec.Attrs["status"]) == "200" && rec.Attrs["commit"] == o.commit
	})
	row := r.mergerRow()
	r.result("%s: the merger's phase row of %s: commit %s, summary %q, completed at %s",
		o.name, r.merger, row.HandoffCommit, row.Summary, clockAt(row.CompletedAt))
	if answered && !answeredAt.Before(o.servingAt) && !answer.isError {
		if !applied {
			fail("completion-after-start", "the daemon logged no `%s` of %s answered 200 at commit %s after it served again", w1CompletionAnswered, o.merger, o.commit)
		}
		if row.HandoffCommit != o.commit || row.CompletedAt.Before(o.servingAt) {
			fail("completion-after-start", "the merger's phase row reads commit %s completed at %s, want %s completed after %s",
				row.HandoffCommit, clockAt(row.CompletedAt), o.commit, clockAt(o.servingAt))
		}
	}

	// The second claim reaches ready.
	var second api.OperatorClaim
	ready := r.within(w1AnswerWait, func() bool {
		second = r.claim(l.token)
		return second.LastReady != nil && second.LastReady.After(o.servingAt) &&
			(second.State == string(supervise.StateReady) || second.State == string(supervise.StateIdle) || second.State == string(supervise.StateWorking))
	})
	r.result("%s: %s after the start: %s at generation %d, last ready %s, %s", o.name, l.token, second.State, second.Generation, w1Time(second.LastReady), w1Counts(second.Budgets))
	if !ready {
		fail("second-claim-ready", "%s did not reach ready within %s of the daemon serving again: %s at generation %d, last ready %s",
			l.token, w1AnswerWait, second.State, second.Generation, w1Time(second.LastReady))
	}

	// Past the registration deadline, the charges where the daemon records them: every line of
	// its log of each claim from the outage's stop on (charges), from a boot that was serving when
	// it wrote it. A line a stopped boot's claim machine wrote after its stop is recorded and
	// charges nothing: its writes end with its context.
	past := o.servingAt.Add(w1RegistrationDeadline + w1PastDeadline)
	r.note("%s: waiting until %s, %s past the daemon's start, past its registration deadline of %s", o.name, clockAt(past), w1RegistrationDeadline+w1PastDeadline, w1RegistrationDeadline)
	time.Sleep(time.Until(past))
	afterMerger, afterSecond := r.claim(o.merger), r.claim(l.token)
	for _, c := range []struct {
		token                  claim.Token
		generation             uint64
		launchFailures, deaths int
	}{
		{o.merger, o.before.Generation, o.before.Budgets.LaunchFailures, o.before.Budgets.Deaths},
		{l.token, l.atStop.Generation, l.atStop.Budgets.LaunchFailures, l.atStop.Budgets.Deaths},
	} {
		charges, read, untagged := r.charges(c.token, l.logFrom, c.generation, c.launchFailures, c.deaths)
		r.result("%s: the daemon's log of %s from the outage's stop at %s, past generation %d, %d launch failures and %d deaths: %d lines of it read, %d of them with no boot number, %d that charge it, say why, or launch it again",
			o.name, c.token, clockAt(l.stoppedAt), c.generation, c.launchFailures, c.deaths, read, untagged, len(charges))
		// A reading that saw no line of the claim, or a line it cannot place in a boot, cannot say
		// the claim was charged nothing.
		switch {
		case read == 0:
			fail("charges-read", "the daemon's log holds no line of %s from the outage's stop on: the reading of its charges saw nothing", c.token)
		case untagged > 0:
			fail("charges-read", "%d lines of the daemon's log of %s carry no boot number, so the reading cannot tell the serving daemon's from a stopped boot's", untagged, c.token)
		}
		var launches, deaths []string
		for _, charge := range charges {
			r.result("%s:   %s", o.name, charge.describe())
			switch {
			case charge.stale:
			case charge.kind == w1ChargeDeath:
				deaths = append(deaths, charge.describe())
			default:
				launches = append(launches, charge.describe())
			}
		}
		if len(launches) > 0 {
			fail("no-launch-failure", "the daemon's log charges %s a launch failure or launches it again from the outage: %s", c.token, strings.Join(launches, "; "))
		}
		if len(deaths) > 0 {
			fail("no-death", "the daemon's log records a death of %s from the outage: %s", c.token, strings.Join(deaths, "; "))
		}
	}
	listing := r.legionClaims("list", "--json")
	r.writeEvidence("w1-"+o.name+"-claims.json", listing)
	recordJSON := r.issueRecord(r.merger)
	r.writeEvidence("w1-"+o.name+"-record.json", recordJSON)
	r.result("%s: at %s, %s past the start: `legion claims list --json` (evidence w1-%s-claims.json):", o.name, clockAt(time.Now()), time.Since(o.servingAt).Round(time.Second), o.name)
	for _, line := range w1ClaimLines(listing) {
		r.result("%s:   %s", o.name, line)
	}
	r.result("%s: the first issue's record (evidence w1-%s-record.json): %s", o.name, o.name, w1Short(recordJSON))
	// The counters, as further evidence: every ready clears the launch failures they count, so
	// they cannot show a launch failure on their own (the charges above do).
	r.result("%s: counts of %s: before %s, after %s; of %s: at its launch %s, after %s",
		o.name, o.merger, w1Counts(o.before.Budgets), w1Counts(afterMerger.Budgets), l.token, w1StoreCounts(l.atStop.Budgets), w1Counts(afterSecond.Budgets))
	if afterMerger.Budgets.LaunchFailures > o.before.Budgets.LaunchFailures {
		fail("no-launch-failure", "%s was charged %d launch failures, %d before the outage and %d after (last start failure %q)",
			o.merger, afterMerger.Budgets.LaunchFailures-o.before.Budgets.LaunchFailures, o.before.Budgets.LaunchFailures, afterMerger.Budgets.LaunchFailures, afterMerger.LastStartFailure)
	}
	if afterSecond.Budgets.LaunchFailures > l.atStop.Budgets.LaunchFailures {
		fail("no-launch-failure", "%s was charged %d launch failures, %d at its launch and %d after (last start failure %q)",
			l.token, afterSecond.Budgets.LaunchFailures-l.atStop.Budgets.LaunchFailures, l.atStop.Budgets.LaunchFailures, afterSecond.Budgets.LaunchFailures, afterSecond.LastStartFailure)
	}
	if afterMerger.Budgets.Deaths > o.before.Budgets.Deaths {
		fail("no-death", "%s was charged %d deaths (last death cause %q)", o.merger, afterMerger.Budgets.Deaths-o.before.Budgets.Deaths, afterMerger.LastDeathCause)
	}
	if afterSecond.Budgets.Deaths > l.atStop.Budgets.Deaths {
		fail("no-death", "%s was charged %d deaths (last death cause %q)", l.token, afterSecond.Budgets.Deaths-l.atStop.Budgets.Deaths, afterSecond.LastDeathCause)
	}
	for _, token := range []claim.Token{o.merger, l.token} {
		if at, ended := o.ended[token]; ended {
			fail("no-death", "%s's process ended during the outage, at %s", token, clockAt(at))
		}
	}

	// Each pane's own lines about the daemon, from the outage's start.
	for token, launch := range o.paneLaunch {
		for _, line := range w1LegionLines(r.runtime.launchesOf(token), launch, o.paneOffset[token]) {
			r.result("%s: pane of %s: %s", o.name, token, line)
		}
	}

	// Neither worker's stall watch acts during the wait.
	for _, token := range []claim.Token{o.merger, l.token} {
		c := r.claim(token)
		watch, restart := w1StallRecords(c.SessionFile)
		r.result("%s: %s's transcript %s holds %d %s records and %d %s messages", o.name, token, c.SessionFile, len(watch), w1StallWatchEntry, len(restart), w1StallRestart)
		if len(watch) > 0 || len(restart) > 0 {
			fail("watchdog-quiet", "%s's transcript holds %d %s records (first: %s) and %d %s messages", token, len(watch), w1StallWatchEntry, w1First(watch), len(restart), w1StallRestart)
		}
	}
	notices := r.stallNotices(o.merger, l.token)
	r.result("%s: stall notices on the two claims' architects' role topics: %d", o.name, len(notices))
	if len(notices) > 0 {
		fail("watchdog-quiet", "the architects' role topics hold %d stall notices of the two claims: %s", len(notices), w1Short(notices[0]))
	}
	if control != nil {
		c := r.claim(control.token)
		watch, restart := w1StallRecords(c.SessionFile)
		r.result("%s: the control %s's transcript %s holds %d %s records (first: %s) and %d %s messages; its notices: %d; its counts %s",
			o.name, control.token, c.SessionFile, len(watch), w1StallWatchEntry, w1First(watch), len(restart), w1StallRestart, len(r.stallNotices(control.token)), w1Counts(c.Budgets))
		if len(watch) == 0 {
			fail("watchdog-control", "the control %s, whose task call was open from %s and past its bound of %s inside the outage, has no %s record in %s: this run cannot show that the watch the clause reads would act",
				control.token, clockAt(control.started), r.bound, w1StallWatchEntry, c.SessionFile)
		}
	}
	for _, ex := range r.http.since(o.httpFrom) {
		heldPort := ex.boot == 0 && (ex.path == "/legion/v1/state" || ex.path == w1CompletePath || ex.path == w1ReadyPath || ex.path == w1RegisterPath)
		if heldPort || ex.path == w1CompletePath || (ex.claim == l.token && (ex.path == w1RegisterPath || ex.path == w1ReadyPath)) {
			r.result("%s: API port: %s", o.name, ex.describe())
		}
	}
	r.writeEvidence("w1-http.tsv", r.http.tsv())
	if len(problems) > 0 {
		sentences := make([]string, 0, len(problems))
		for _, p := range problems {
			sentences = append(sentences, p.assertion+": "+p.sentence)
		}
		r.failedAt = problems[0].assertion
		r.t.Fatalf("%s", strings.Join(sentences, "; "))
	}
}

// w1Problem is one clause of the check an outage did not meet.
type w1Problem struct{ assertion, sentence string }

// What a line of the daemon's log says of a claim it names (w1ChargeKind).
const (
	w1ChargeDeath         = "death"
	w1ChargeLaunchFailure = "launch failure"
	w1ChargeLaunch        = "launch"
)

// w1Charge is one line of the daemon's log that charges a claim, says why, or launches it again:
// which of those, the boot that wrote it, and whether that boot had stopped when it did.
type w1Charge struct {
	kind   string
	boot   int
	stale  bool
	record loggedRecord
}

func (c w1Charge) describe() string {
	who := fmt.Sprintf("boot %d", c.boot)
	if c.stale {
		who += " after its stop"
	}
	return fmt.Sprintf("%s (%s): %s", who, c.kind, w1Short(w1Describe(c.record)))
}

// charges is every line of the daemon's log of token, from its from-th line on, that charges it a
// launch failure or a death, says why, or launches it past generation (w1ChargeKind), against the
// counts it held before; each with the boot that wrote it, and whether that boot had stopped. read
// is how many lines of token it read, and untagged how many of those carry no boot number.
func (r *w1Live) charges(token claim.Token, from int, generation uint64, launchFailures, deaths int) (charges []w1Charge, read, untagged int) {
	for i, rec := range r.daemon.matching(func(loggedRecord) bool { return true }) {
		if i < from || rec.Attrs["claim"] != string(token) {
			continue
		}
		read++
		boot := int(w1Number(rec.Attrs[w1BootAttr]))
		if boot == 0 {
			untagged++
		}
		kind := w1ChargeKind(rec, generation, launchFailures, deaths)
		if kind == "" {
			continue
		}
		stoppedAt, stopped := r.stopped[boot]
		charges = append(charges, w1Charge{kind: kind, boot: boot, stale: stopped && rec.at.After(stoppedAt), record: rec})
	}
	return charges, read, untagged
}

// w1ChargeKind is what a line of the daemon's log of a claim says of it: a death (a process the
// daemon found gone, or a death it counted past deaths), a launch failure (one it counted past
// launchFailures, a start the runtime refused, a process it retired for never registering or
// never saying it was ready, the backoff that follows a charge, or the claim's failure), a launch
// past generation, or nothing ("").
func w1ChargeKind(rec loggedRecord, generation uint64, launchFailures, deaths int) string {
	switch {
	case rec.Message == w1ProcessDied || rec.Message == w1DiedWithWork || w1Number(rec.Attrs["deaths"]) > float64(deaths):
		return w1ChargeDeath
	case rec.Message == w1LaunchFailed || rec.Message == w1BackingOff || rec.Message == w1ClaimFailed ||
		strings.HasSuffix(rec.Message, w1RetiredSuffix) || w1Number(rec.Attrs["launchFailures"]) > float64(launchFailures):
		return w1ChargeLaunchFailure
	case rec.Message == w1Launched && w1Number(rec.Attrs["generation"]) > float64(generation):
		return w1ChargeLaunch
	}
	return ""
}

// w1Number is a numeric attribute of a logged record, which decodeAttr makes a float64, or 0.
func w1Number(value any) float64 {
	n, _ := value.(float64)
	return n
}

// ---- the rig's model stand-ins and the merger's workspace ---------------------------------------

// writeStandins replaces the rig's one stand-in with two: every pane's turns go to a
// completionStandin under the rig's own provider name, and the control claim's to a
// scriptedStandin of its own, through each claim's role overlay. The profile is the rig's, written
// again before the first launch.
func (r *w1Live) writeStandins() {
	t := r.t
	r.stand = newCompletionStandin(t, rigModelProvider)
	r.controlStand = newScriptedStandin(t, w1ControlProvider)
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	var models strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/w1_daemon_wait_live_test.go.\nproviders:\n")
	for _, s := range []struct{ name, url string }{{rigModelProvider, r.stand.url}, {w1ControlProvider, r.controlStand.url}} {
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			s.name, s.url, s.name+standinModelID, s.name)
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"), "# Written by packages/daemon/internal/daemon/w1_daemon_wait_live_test.go.\nenabledModels:\n"+
		"  - "+standinModel(rigModelProvider)+"\n  - "+standinModel(w1ControlProvider)+"\nstartup:\n  setupWizard: false\n")
	dir := t.TempDir()
	model, control := filepath.Join(dir, "rig-model.yml"), filepath.Join(dir, "rig-control.yml")
	writeRoleOverlay(t, model, standinModel(rigModelProvider))
	writeRoleOverlay(t, control, standinModel(w1ControlProvider))
	controlToken := r.token(r.control, claim.RoleImplementer)
	r.runtime.overlays = func(token claim.Token) []string {
		if token == controlToken {
			return []string{control}
		}
		return []string{model}
	}
	r.note("every pane's turns go to the completion stand-in %s at %s; the control claim %s's to %s at %s", rigModelProvider, r.stand.url, controlToken, w1ControlProvider, r.controlStand.url)
}

// makeWorkspace makes the merger's working directory a jj repository, as a pod's issue workspace
// is, whose `@-` each outage moves (commitHead), and puts the jj the run names first on PATH, so
// the pane's plugin runs that jj under the pane's own HOME.
func (r *w1Live) makeWorkspace() {
	t := r.t
	r.jj = os.Getenv(w1JJEnv)
	if r.jj == "" {
		found, err := exec.LookPath("jj")
		if err != nil {
			t.Skipf("%s is unset and no jj is on PATH; scripts/e2e/daemon-wait.sh names one", w1JJEnv)
		}
		r.jj = found
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.jj, filepath.Join(bin, "jj")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.workspace = r.rt.workDir(r.merger)
	if err := os.MkdirAll(r.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	r.runJJ("git", "init")
	writeProofFile(t, filepath.Join(r.workspace, ".gitignore"), "/tmp/\n")
	version, _ := exec.Command(r.jj, "--version").Output()
	r.note("the merger's workspace %s is a jj repository (%s, %s)", r.workspace, r.jj, strings.TrimSpace(string(version)))
}

// commitHead commits a change in the merger's workspace and answers the commit it now stands on.
func (r *w1Live) commitHead(round string) string {
	writeProofFile(r.t, filepath.Join(r.workspace, "merged.txt"), "the merge of outage "+round+"\n")
	r.runJJ("commit", "-m", "w1: the merge of outage "+round)
	return r.runJJ("log", "-r", "@-", "--no-graph", "-T", "commit_id")
}

// runJJ runs the run's jj in the workspace with an identity of its own and no user configuration.
func (r *w1Live) runJJ(args ...string) string {
	r.t.Helper()
	home := filepath.Join(filepath.Dir(r.workspace), ".w1-jj-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		r.t.Fatal(err)
	}
	empty := filepath.Join(home, "config.toml")
	writeProofFile(r.t, empty, "")
	full := append([]string{"--config", "user.name=w1 proof", "--config", "user.email=w1@legion.invalid"}, args...)
	cmd := exec.Command(r.jj, full...)
	cmd.Dir = r.workspace
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "JJ_CONFIG=" + empty, "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("jj %s in %s: %v\n%s", strings.Join(args, " "), r.workspace, err, out)
	}
	return strings.TrimSpace(string(out))
}

// ---- the daemon's boots and its API port --------------------------------------------------------

// newGate is a gate for the next boot's API listener, whose parked connections the run ends.
func (r *w1Live) newGate(trigger func(w1Exchange) bool) *w1Gate {
	g := &w1Gate{log: r.http, trigger: trigger, fired: make(chan time.Time, 1)}
	r.root.Cleanup(g.release)
	return g
}

// bootWith starts the daemon as r.boot does, serving its API through g. Every line its log writes
// carries the rig's boot number (w1BootAttr), so a line a boot writes after its stop is told from
// the serving daemon's (charges).
func (r *w1Live) bootWith(g *w1Gate) {
	r.t.Helper()
	r.boots++
	g.boot = r.boots
	o := r.overrides()
	o.listen = func(network, address string) (net.Listener, error) {
		inner, err := heldListen(network, address)
		if err != nil {
			return nil, err
		}
		g.Listener = inner
		return g, nil
	}
	r.gates = append(r.gates, g)
	r.d = startDaemonWithin(r.root, r.cfg, o, slog.New(r.daemon).With(w1BootAttr, g.boot), 2*time.Minute)
}

// stopDaemon stops the serving daemon and answers when it had stopped, which it keeps as that
// boot's stop (stopped).
func (r *w1Live) stopDaemon() time.Time {
	r.t.Helper()
	r.d.stop()
	at := time.Now()
	r.stopped[r.boots] = at
	return at
}

// w1PortReservation holds the daemon's API port closed through a closed-port outage: a TCP socket
// bound to the port on 127.0.0.1 that never listens, so a connection to the port is refused as a
// closed port's is and no other process can listen on it. It binds with SO_REUSEADDR, which lets
// it bind beside the connections the stopped daemon's listener left on the port, and clears it at
// once: Linux lets a second SO_REUSEADDR socket bind, and listen, beside a bound socket that keeps
// it, and refuses both beside one that does not.
type w1PortReservation struct {
	fd           int
	at, released time.Time
}

func w1ReservePort(port int) (*w1PortReservation, error) {
	// syscall has no SOCK_CLOEXEC on darwin. Under ForkLock no fork between the socket and
	// CloseOnExec can hand the descriptor to a child, which is what the flag does on Linux.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("set SO_REUSEADDR: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("bind 127.0.0.1:%d: %w", port, err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("clear SO_REUSEADDR: %w", err)
	}
	return &w1PortReservation{fd: fd, at: time.Now()}, nil
}

// release gives the port up, once.
func (p *w1PortReservation) release() {
	if p.released.IsZero() {
		_ = syscall.Close(p.fd)
		p.released = time.Now()
	}
}

// reservePort reserves the daemon's port (w1PortReservation) the moment the daemon has closed its
// listener (w1Gate.afterClose). What it got is read once the daemon has stopped (proveReserved).
func (r *w1Live) reservePort() {
	p, err := w1ReservePort(r.cfg.Port)
	r.portMu.Lock()
	defer r.portMu.Unlock()
	r.reserved, r.reserveErr = p, err
}

// proveReserved fails the step unless the daemon's stop reserved its port, and shows that the
// reservation does what it is for: a connect is refused (ECONNREFUSED), and a listen on the
// loopback address, the IPv4 wildcard or the dual-stack one cannot take the port (EADDRINUSE),
// each of which would take it beside a reservation that kept SO_REUSEADDR.
func (r *w1Live) proveReserved() {
	r.t.Helper()
	r.portMu.Lock()
	p, err := r.reserved, r.reserveErr
	r.portMu.Unlock()
	if p == nil {
		if err == nil {
			err = errors.New("the daemon's stop never closed the listener it was handed")
		}
		r.fails("closed-port-reserved", "the rig could not reserve the daemon's port %d at the daemon's stop: %v", r.cfg.Port, err)
	}
	r.root.Cleanup(p.release)
	refused, got := r.dialPort()
	if !refused {
		r.fails("closed-port-reserved", "a connect to the reserved port %d was not refused as a closed port's is: %s", r.cfg.Port, got)
	}
	port := strconv.Itoa(r.cfg.Port)
	var listens []string
	for _, address := range []string{"127.0.0.1:" + port, "0.0.0.0:" + port, ":" + port} {
		listener, err := net.Listen("tcp", address)
		if err == nil {
			_ = listener.Close()
			r.fails("closed-port-reserved", "a listen on %s took the daemon's port beside its reservation: another process could answer in the daemon's place", address)
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			r.fails("closed-port-reserved", "a listen on %s beside the reservation failed otherwise than as a port in use: %v", address, err)
		}
		listens = append(listens, err.Error())
	}
	r.result("the daemon's port %d is reserved from %s, bound and never listening: a connect gets %q (ECONNREFUSED); a listen gets %s",
		r.cfg.Port, clockAt(p.at), got, strings.Join(listens, "; "))
}

// dialPort connects to the daemon's port once, and says whether the connect was refused as a
// closed port's is (ECONNREFUSED), with what it got.
func (r *w1Live) dialPort() (bool, string) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.cfg.Port)), w1PortProbeLimit)
	if err == nil {
		_ = conn.Close()
		return false, "the connection was taken"
	}
	return errors.Is(err, syscall.ECONNREFUSED), err.Error()
}

// probePort is one connect to the reserved port during the closed-port outage, recorded on o.
func (r *w1Live) probePort(o *w1OutageRun, when string) {
	refused, got := r.dialPort()
	line := fmt.Sprintf("%s, at %s: %s", when, clockAt(time.Now()), got)
	o.portProbes = append(o.portProbes, line)
	if !refused {
		o.portAnswered = append(o.portAnswered, line)
	}
	r.result("%s: a connect to the reserved port %d %s: %s", o.name, r.cfg.Port, when, got)
}

// holdsReservation is whether the closed-port outage's reservation holds the port now.
func (r *w1Live) holdsReservation() bool {
	r.portMu.Lock()
	defer r.portMu.Unlock()
	return r.reserved != nil && r.reserved.released.IsZero()
}

// releaseReservation gives the reserved port up, for the restart to take it.
func (r *w1Live) releaseReservation() {
	r.portMu.Lock()
	defer r.portMu.Unlock()
	r.reserved.release()
}

// watchHeldPort accepts on the listener the rig holds on the daemon's port while no daemon runs,
// records each request it is sent and answers none, keeping every connection open: what a caller
// meets is what a queued connection meets — a connection taken and no answer — and the access log
// shows each attempt, when it came and which claim sent it. Every byte a connection sends is read
// and kept on the request it belongs to, so a body that follows its headers still names its
// claim. Its gate is boot 0, the port with no daemon behind it; dropHeldPort and the gate's
// release end it.
func (r *w1Live) watchHeldPort() {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.cfg.Port))
	heldPorts.Lock()
	listener := heldPorts.byAddress[address]
	heldPorts.Unlock()
	if listener == nil {
		r.t.Fatalf("the rig holds no listener on the daemon's port %s", address)
	}
	g := r.newGate(nil)
	r.gates = append(r.gates, g)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			g.mu.Lock()
			g.conns++
			id := g.conns
			g.parked = append(g.parked, conn)
			g.mu.Unlock()
			go r.http.hold(id, conn)
		}
	}()
}

// hold reads connection id of the held port until it closes, recording each request it sends as
// held, and answers nothing.
func (l *w1HTTPLog) hold(id int, conn net.Conn) {
	buf := make([]byte, 64*1024)
	var last *w1Exchange
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			lines := w1RequestLine.FindAllSubmatchIndex(chunk, -1)
			now := time.Now()
			l.mu.Lock()
			start := len(chunk)
			if len(lines) > 0 {
				start = lines[0][0]
			}
			if last != nil {
				last.append(chunk[:start])
				l.name(last)
			}
			for i, at := range lines {
				end := len(chunk)
				if i+1 < len(lines) {
					end = lines[i+1][0]
				}
				ex := &w1Exchange{conn: id, method: string(chunk[at[2]:at[3]]), path: string(chunk[at[4]:at[5]]), requestAt: now, gated: "held"}
				ex.append(chunk[at[0]:end])
				l.name(ex)
				l.exchanges = append(l.exchanges, ex)
				last = ex
			}
			l.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// dropHeldPort closes the listener the rig holds on the daemon's port, with every connection it
// took or queued unanswered, as the socket of a hung daemon closes when it is replaced.
func (r *w1Live) dropHeldPort() {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.cfg.Port))
	heldPorts.Lock()
	listener := heldPorts.byAddress[address]
	delete(heldPorts.byAddress, address)
	heldPorts.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
}

// closeParked ends every connection a gate held unanswered.
func (r *w1Live) closeParked() {
	for _, g := range r.gates {
		g.release()
	}
}

// w1GateMode is what the API port does with a request.
type w1GateMode int32

const (
	// w1GateOpen carries it to the daemon.
	w1GateOpen w1GateMode = iota
	// w1GateClosing closes its connection unanswered: the daemon is stopping.
	w1GateClosing
	// w1GateHolding keeps its connection open and answers nothing: the daemon has stopped
	// answering.
	w1GateHolding
)

// w1Gate is one boot's API listener: the listener the rig holds for the port, every connection
// accepted on it recorded in the run's access log (w1HTTPLog), and a gate a step closes at the
// moment it decides to stop the daemon, so no request reaches a daemon the rig is stopping. The
// rig's own readiness poll (GET /healthz) always passes. A trigger, when set, is asked at the
// start of every response, and holds every later request from the moment it answers true.
// afterClose, when set before the boot, runs once, the moment the daemon has closed the listener:
// the closed-port outage reserves the port there (reservePort), before any other process can
// take it.
type w1Gate struct {
	net.Listener
	log        *w1HTTPLog
	boot       int
	mode       atomic.Int32
	trigger    func(w1Exchange) bool
	fired      chan time.Time
	afterClose func()

	mu      sync.Mutex
	conns   int
	closing time.Time
	parked  []net.Conn
	closed  bool
}

// Close closes the listener the daemon was handed, then runs afterClose, once.
func (g *w1Gate) Close() error {
	err := g.Listener.Close()
	g.mu.Lock()
	first := !g.closed
	g.closed = true
	g.mu.Unlock()
	if first && g.afterClose != nil {
		g.afterClose()
	}
	return err
}

func (g *w1Gate) Accept() (net.Conn, error) {
	conn, err := g.Listener.Accept()
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.conns++
	id := g.conns
	g.mu.Unlock()
	return &w1Conn{Conn: conn, gate: g, id: id}, nil
}

// closeRequests closes every later request's connection unanswered.
func (g *w1Gate) closeRequests() {
	g.mu.Lock()
	g.closing = time.Now()
	g.mu.Unlock()
	g.mode.Store(int32(w1GateClosing))
}

func (g *w1Gate) closedAt() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closing
}

// release ends every connection the gate held unanswered.
func (g *w1Gate) release() {
	g.mu.Lock()
	parked := g.parked
	g.parked = nil
	g.mu.Unlock()
	for _, conn := range parked {
		_ = conn.Close()
	}
}

// w1Conn is one connection the API port accepted.
type w1Conn struct {
	net.Conn
	gate *w1Gate
	id   int

	mu     sync.Mutex
	open   []*w1Exchange
	last   *w1Exchange
	parked bool
}

var (
	w1RequestLine     = regexp.MustCompile(`(GET|POST|PUT|PATCH|DELETE|HEAD) (/[^ \r\n]*) HTTP/1\.[01]\r\n`)
	w1StatusLine      = regexp.MustCompile(`^HTTP/1\.[01] (\d{3})`)
	w1BootTokenField  = regexp.MustCompile(`"bootToken"\s*:\s*"([^"]+)"`)
	w1ClaimTokenField = regexp.MustCompile(`"claimToken"\s*:\s*"([^"]+)"`)
)

// Read carries a request to the daemon, or, while the gate is not open, ends it there. Its
// error is io.EOF, the one net/http answers nothing for.
func (c *w1Conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n == 0 {
		return n, err
	}
	chunk := p[:n]
	lines := w1RequestLine.FindAllSubmatchIndex(chunk, -1)
	mode := w1GateMode(c.gate.mode.Load())
	if mode != w1GateOpen && len(lines) > 0 && !w1OnlyHealthz(chunk, lines) {
		word := "closed"
		if mode == w1GateHolding {
			word = "held"
		}
		c.gate.log.gated(c, chunk, lines, word)
		if mode == w1GateHolding {
			c.mu.Lock()
			c.parked = true
			c.mu.Unlock()
			c.gate.mu.Lock()
			c.gate.parked = append(c.gate.parked, c.Conn)
			c.gate.mu.Unlock()
			return 0, io.EOF
		}
		_ = c.Conn.Close()
		return 0, io.EOF
	}
	c.gate.log.read(c, chunk, lines)
	return n, err
}

// Write records the answer a response begins, asks the trigger, and writes it.
func (c *w1Conn) Write(p []byte) (int, error) {
	if m := w1StatusLine.FindSubmatch(p); m != nil {
		status, _ := strconv.Atoi(string(m[1]))
		if ex, ok := c.gate.log.answered(c, status); ok && c.gate.trigger != nil && c.gate.trigger(ex) {
			if c.gate.mode.CompareAndSwap(int32(w1GateOpen), int32(w1GateHolding)) {
				c.gate.fired <- ex.answeredAt
			}
		}
	}
	return c.Conn.Write(p)
}

// Close leaves a held connection open: the daemon closing it is not the client's answer.
func (c *w1Conn) Close() error {
	c.mu.Lock()
	parked := c.parked
	c.mu.Unlock()
	if parked {
		return nil
	}
	return c.Conn.Close()
}

func w1OnlyHealthz(chunk []byte, lines [][]int) bool {
	for _, at := range lines {
		if string(chunk[at[2]:at[3]]) != http.MethodGet || string(chunk[at[4]:at[5]]) != "/healthz" {
			return false
		}
	}
	return true
}

// w1HTTPLog is every request the daemon's API port was sent across the run's boots: when it came,
// what it asked, the claim it names (read from its body, never printed), and when and how it was
// answered, or that the gate kept it from the daemon.
type w1HTTPLog struct {
	resolve func(string) (claim.Token, uint64, bool)

	mu        sync.Mutex
	exchanges []*w1Exchange
}

// w1Exchange is one request on the API port.
type w1Exchange struct {
	boot, conn   int
	method, path string
	claim        claim.Token
	generation   uint64
	requestAt    time.Time
	status       int
	answeredAt   time.Time
	gated        string
	raw          []byte
}

func (l *w1HTTPLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.exchanges)
}

// since is a copy of every exchange from index from on.
func (l *w1HTTPLog) since(from int) []w1Exchange {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []w1Exchange
	for _, ex := range l.exchanges[min(from, len(l.exchanges)):] {
		copied := *ex
		copied.raw = nil
		out = append(out, copied)
	}
	return out
}

// read records each request a chunk begins, and the chunk's bytes on the request they belong to.
func (l *w1HTTPLog) read(c *w1Conn, chunk []byte, lines [][]int) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	start := len(chunk)
	if len(lines) > 0 {
		start = lines[0][0]
	}
	if c.last != nil {
		c.last.append(chunk[:start])
	}
	for i, at := range lines {
		end := len(chunk)
		if i+1 < len(lines) {
			end = lines[i+1][0]
		}
		ex := &w1Exchange{boot: c.gate.boot, conn: c.id, method: string(chunk[at[2]:at[3]]), path: string(chunk[at[4]:at[5]]), requestAt: now}
		ex.append(chunk[at[0]:end])
		l.exchanges = append(l.exchanges, ex)
		c.open = append(c.open, ex)
		c.last = ex
	}
}

// gated records each request a chunk begins that the gate kept from the daemon.
func (l *w1HTTPLog) gated(c *w1Conn, chunk []byte, lines [][]int, word string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, at := range lines {
		end := len(chunk)
		if i+1 < len(lines) {
			end = lines[i+1][0]
		}
		ex := &w1Exchange{boot: c.gate.boot, conn: c.id, method: string(chunk[at[2]:at[3]]), path: string(chunk[at[4]:at[5]]), requestAt: now, gated: word}
		ex.append(chunk[at[0]:end])
		l.name(ex)
		l.exchanges = append(l.exchanges, ex)
	}
}

// answered marks the oldest open request of c answered with status, and answers a copy of it.
func (l *w1HTTPLog) answered(c *w1Conn, status int) (w1Exchange, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.open) == 0 {
		return w1Exchange{}, false
	}
	ex := c.open[0]
	c.open = c.open[1:]
	ex.status, ex.answeredAt = status, time.Now()
	l.name(ex)
	copied := *ex
	copied.raw = nil
	return copied, true
}

// name reads the claim a request's body names: a register's boot token, as the launch that minted
// it, or a claim token.
func (l *w1HTTPLog) name(ex *w1Exchange) {
	if ex.claim != "" {
		return
	}
	if m := w1BootTokenField.FindSubmatch(ex.raw); m != nil {
		if token, generation, ok := l.resolve(string(m[1])); ok {
			ex.claim, ex.generation = token, generation
		} else {
			ex.claim = "(a boot token no launch minted)"
		}
		return
	}
	if m := w1ClaimTokenField.FindSubmatch(ex.raw); m != nil {
		ex.claim = claim.Token(m[1])
	}
}

func (ex *w1Exchange) append(b []byte) {
	if room := 64*1024 - len(ex.raw); room > 0 {
		ex.raw = append(ex.raw, b[:min(room, len(b))]...)
	}
}

func (ex w1Exchange) describe() string {
	who := string(ex.claim)
	if who == "" {
		who = "-"
	} else if ex.generation != 0 {
		who += fmt.Sprintf(" generation %d", ex.generation)
	}
	answer := "no answer"
	switch {
	case ex.gated != "":
		answer = "kept from the daemon (" + ex.gated + ")"
	case !ex.answeredAt.IsZero():
		answer = fmt.Sprintf("answered %d at %s", ex.status, clockAt(ex.answeredAt))
	}
	where := fmt.Sprintf("boot %d", ex.boot)
	if ex.boot == 0 {
		where = "the held port (no daemon)"
	}
	return fmt.Sprintf("%s conn %d %s %s (%s) at %s, %s", where, ex.conn, ex.method, ex.path, who, clockAt(ex.requestAt), answer)
}

// tsv is the whole log, one exchange per line.
func (l *w1HTTPLog) tsv() string {
	var b strings.Builder
	b.WriteString("boot\tconn\tmethod\tpath\tclaim\tgeneration\trequest_at\tstatus\tanswered_at\tgated\n")
	for _, ex := range l.since(0) {
		answered := ""
		if !ex.answeredAt.IsZero() {
			answered = ex.answeredAt.UTC().Format(time.RFC3339Nano)
		}
		fmt.Fprintf(&b, "%d\t%d\t%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\n", ex.boot, ex.conn, ex.method, ex.path, ex.claim, ex.generation,
			ex.requestAt.UTC().Format(time.RFC3339Nano), ex.status, answered, ex.gated)
	}
	return b.String()
}

// ---- the reads the steps make -------------------------------------------------------------------

// deliver gives the claim one task of the operator's own (`legion claims deliver`'s route).
func (r *w1Live) deliver(token claim.Token, task string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver", api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK {
		r.t.Fatalf("deliver %q to %s = %d %s, want 200", task, token, status, body)
	}
}

// awaitIdle waits for the claim's agent to answer its start's task and stand idle.
func (r *w1Live) awaitIdle(token claim.Token) {
	r.t.Helper()
	r.await(string(token)+" to answer its task and stand idle", w1IdleWait, func() bool {
		c := r.claim(token)
		if c.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed; its workers' output is under %s", token, r.rt.paneLogs)
		}
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})
}

// within polls want until it holds or within runs out, and says which.
func (r *w1Live) within(within time.Duration, want func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if want() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

// storeClaim is token's claim as the store holds it, which needs no running daemon.
func (r *w1Live) storeClaim(token claim.Token) supervise.Claim {
	r.t.Helper()
	claims, err := r.st.Claims(context.Background())
	if err != nil {
		r.t.Fatalf("read the claims from the store: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	r.t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

// mergerRow is the merger's phase row of the first issue, from the store.
func (r *w1Live) mergerRow() record.PhaseRow {
	r.t.Helper()
	var found record.PhaseRow
	if err := r.st.Tx(context.Background(), func(tx pgx.Tx) error {
		rows, err := r.records.Phases(context.Background(), tx, r.merger)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Role == claim.RoleMerger {
				found = row
			}
		}
		return nil
	}); err != nil {
		r.t.Fatalf("read the phase rows of %s: %v", r.merger, err)
	}
	return found
}

// issueRecord is key's record as GET /legion/v1/state serves it, the record `read_record` reads.
func (r *w1Live) issueRecord(key string) string {
	r.t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		r.t.Fatalf("GET /legion/v1/state = %d %s", status, body)
	}
	var state struct {
		Issues map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		r.t.Fatalf("decode the state: %v", err)
	}
	raw, ok := state.Issues[key]
	if !ok {
		r.t.Fatalf("the state records no issue %s", key)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

// issuePhase is key's phase as the state serves it.
func (r *w1Live) issuePhase(key string) string {
	var record struct {
		Phase string `json:"phase"`
	}
	_ = json.Unmarshal([]byte(r.issueRecord(key)), &record)
	return record.Phase
}

// stallNotices is every message on the role topic of each claim's issue's architect that names a
// stall.
func (r *w1Live) stallNotices(tokens ...claim.Token) []string {
	r.t.Helper()
	var notices []string
	for _, token := range tokens {
		c := r.claim(token)
		architect := r.token(c.Issue, claim.RoleArchitect)
		for _, stored := range r.nats.storedOn(notify.RoleTopicPrefix + string(architect)) {
			text := string(stored.data)
			if strings.Contains(strings.ToLower(text), "stall") || strings.Contains(text, "overdue") {
				notices = append(notices, clockAt(stored.at)+" "+text)
			}
		}
	}
	return notices
}

// result is one line of what the run observed: printed under the step, and appended to the
// evidence directory's w1-results.log with the moment it was written, so it survives a kill.
func (r *w1Live) result(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.note("%s", line)
	if r.results != nil {
		fmt.Fprintf(r.results, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), line)
		_ = r.results.Sync()
	}
}

// writeEvidence writes body under the evidence directory as name.
func (r *w1Live) writeEvidence(name, body string) {
	if err := os.WriteFile(filepath.Join(r.evidence, name), []byte(body), 0o600); err != nil {
		r.note("could not write %s: %v", name, err)
	}
}

// w1StallRecords is every line of the session file at path that the stall watch wrote, and every
// line carrying its restart message.
func w1StallRecords(path string) (watch, restart []string) {
	file, err := os.Open(path)
	if err != nil {
		return []string{"(the transcript could not be read: " + err.Error() + ")"}, nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, w1StallWatchEntry) {
			watch = append(watch, line)
		}
		if strings.Contains(line, w1StallRestart) {
			restart = append(restart, line)
		}
	}
	return watch, restart
}

// w1LegionLines is each `[legion]` line, and each line naming the daemon or a start call, that the
// claim's processes wrote from the outage's start on: from offset of first's log, then the whole
// log of every launch after it. These are the plugin's own words about the daemon.
func w1LegionLines(launches []*rigLaunch, first *rigLaunch, offset int64) []string {
	at := slices.Index(launches, first)
	if at < 0 {
		return []string{"(the outage's process is not among the claim's launches)"}
	}
	var lines []string
	for i, launch := range launches[at:] {
		from := int64(0)
		if i == 0 {
			from = offset
		}
		file, err := os.Open(launch.pane.log)
		if err != nil {
			lines = append(lines, fmt.Sprintf("generation %d: (its log %s could not be read: %v)", launch.spec.Generation, launch.pane.log, err))
			continue
		}
		if _, err := file.Seek(from, io.SeekStart); err == nil {
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, "[legion]") || strings.Contains(line, w1Unreachable) || strings.Contains(line, "claims/register") || strings.Contains(line, "claims/ready") {
					lines = append(lines, fmt.Sprintf("generation %d: %s", launch.spec.Generation, w1Short(line)))
				}
			}
		}
		file.Close()
	}
	if len(lines) > 40 {
		lines = append(lines[:40], fmt.Sprintf("(%d more)", len(lines)-40))
	}
	return lines
}

// w1ClaimLines is each claim of a `legion claims list --json` answer, as one line.
func w1ClaimLines(listing string) []string {
	var answer struct {
		Claims []api.OperatorClaim `json:"claims"`
	}
	if err := json.Unmarshal([]byte(listing), &answer); err != nil {
		return []string{"(the listing did not decode: " + err.Error() + ")"}
	}
	var lines []string
	for _, c := range answer.Claims {
		lines = append(lines, fmt.Sprintf("%s %s generation %d session %s, %s, last ready %s, last start failure %q, last death cause %q",
			c.Token, c.State, c.Generation, c.Session, w1Counts(c.Budgets), w1Time(c.LastReady), c.LastStartFailure, c.LastDeathCause))
	}
	return lines
}

func w1Counts(b api.BudgetsView) string {
	return fmt.Sprintf("%d launch failures, %d deaths, %d prompt failures, %d prompt retires", b.LaunchFailures, b.Deaths, b.PromptFailures, b.PromptRetires)
}

func w1StoreCounts(b supervise.Budgets) string {
	return fmt.Sprintf("%d launch failures, %d deaths, %d prompt failures, %d prompt retires", b.LaunchFailures, b.Deaths, b.PromptFailures, b.PromptRetires)
}

func w1Time(at *time.Time) string {
	if at == nil {
		return "never"
	}
	return clockAt(*at)
}

func w1Relative(at, mark time.Time) string {
	if at.Before(mark) {
		return mark.Sub(at).Round(time.Second).String() + " before"
	}
	return at.Sub(mark).Round(time.Second).String() + " after"
}

func w1FileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func w1First(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return w1Short(lines[0])
}

// w1Short is text cut to a line a note carries.
func w1Short(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 600 {
		return text[:600] + "…"
	}
	return text
}

// w1Describe is one logged record as a note prints it.
func w1Describe(rec loggedRecord) string {
	keys := make([]string, 0, len(rec.Attrs))
	for key := range rec.Attrs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q", clockAt(rec.at), rec.Message)
	for _, key := range keys {
		fmt.Fprintf(&b, " %s=%v", key, rec.Attrs[key])
	}
	return b.String()
}
