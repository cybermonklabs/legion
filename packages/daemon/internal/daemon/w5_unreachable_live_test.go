//go:build e2e

// Check w5 of this change's verification (R5: a claim Envoy cannot reach is marked, and the daemon
// relaunches it on its same session; R6: a person is told what no agent can hear or act on), run on
// the rig (RIG.md) as the check states it. scripts/e2e/unreachable-claims.sh builds this driver and
// runs it; with --base-commit it runs the first block against the base's code, the check's negative
// control.
//
// Three runs, each on a rig of its own, as the check's own text has them:
//
//   - TestW5UnreachableClaimsLive: the project's controller as a daemon claim and eleven more
//     claims, every step of the check but the two fresh runs;
//   - TestW5ControllerBackgroundJobLive: the fresh run whose controller runs a background job;
//   - TestW5ControllerLauncherLive: the fresh run whose controller's launcher is not connected,
//     and its second out-of-reach within 6 hours.
//
// Each fresh run holds one worker claim besides the controller that stays reachable, so Envoy's
// list holds a live session while the controller is blocked: with none, R5 marks nothing.
//
// The eleven claims, by what the check names them (w5MainClaims): the tree's architect (alone in
// its pod); the workflow issue's planner, whose phase has passed (the reachable claim of the
// implementer's pod) and its implementer (in a wait call); a third claim that takes a step every 5
// minutes (a pty bash call of `sleep 290` again and again); a fourth in one pty bash call with a
// 3,000 s timeout for 40 minutes; a fifth, a reviewer, in a wait on a shell command Oh My Pi moved
// to the background; a pod of two, a planner whose phase has passed (the tenth) and the issue's
// implementer (the sixth); a seventh in a wait while a named bash service it started runs; an
// eighth in a wait on a subagent it started (a child of the architect's tree, so its failure notice
// has an architect); and a ninth whose activity reports the rig drops from its launch on.
//
// "In a wait call": Oh My Pi's `wait` returns after at most 5 minutes (WAIT_LADDER_MS), so a
// process that sits in one wait call for 20 minutes is one that has stopped, as one production
// implementer stopped after its last wait. The rig stops such a process (SIGSTOP of its Oh My Pi,
// never its shim) once its stand-in has answered with `wait` and the call's start has reached the
// daemon, and lets it run again (SIGCONT) where the check has its job end or its subagent end.
//
// Where the rig's block of a claim's registrations stands for a cause in that claim's own process
// (the implementer and the seventh, each frozen in its wait, and the architect, idle), the daemon's
// relaunch replaces the process, and the rig lets the new process's registrations through at the
// relaunch's stop (podRuntime.liftOnStop). Where it stands for a cause outside the process (the
// eighth's, the fifth's, the implementer's second block), the block stays through the relaunch.
//
// What a step reads: read_state and read_record (GET /legion/v1/state) and the operator's claim
// listing once a minute (the sampler), and every 5 s around the first block's marks
// (watchMarks), the controller's and the architect's notices as the stream stored them, the
// outbox's rows as an audit trigger copies them, the daemon's own log (its out-of-reach lines, its
// relaunch guard's lines and R6's `LEGION FLEET ALERT` lines), the runtime's launches and the
// relaunch calls the daemon made of it, and each claim's stand-in's requests. Every daemon text a
// step matches is a literal here, so the first block builds against the base, where the script
// sets W5_BASE_RUN (w5BaseRunEnv).
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The bounds the check reads against.
const (
	w5Unreachable    = 20 * time.Minute
	w5MarkWindow     = 22 * time.Minute
	w5GuardHeld      = 60 * time.Minute
	w5LineSlack      = 2 * time.Minute
	w5FirstRead      = 10 * time.Minute
	w5GuardRead      = 25 * time.Minute
	w5NinthBlocked   = 85 * time.Minute
	w5ReadFor        = 25 * time.Minute
	w5AllBlocked     = 30 * time.Minute
	w5RegisterOver   = 2 * time.Minute
	w5ListFailing    = 25 * time.Minute
	w5ActivityReport = 12 * time.Second
	w5PaneReady      = 6 * time.Minute
)

// w5BaseRunEnv is set by the script's negative control, which runs the first block over the base.
const w5BaseRunEnv = "W5_BASE_RUN"

// The daemon's words a step matches.
const (
	w5MarkLine        = "reach: the claim's session has not registered with Envoy for 20 minutes; it is out of reach, and keeps its state"
	w5HeldFinishPhase = "outbox role message held for a claim Envoy cannot reach is finished undelivered: its phase moved on"
	w5HeldFinishEnded = "outbox role message held for a claim Envoy cannot reach is finished undelivered: its claim ended"
	// w5NoHolderFinish starts the outbox's line of a role message it finishes for want of a live
	// holder; main's line goes on with ", or its claim records a session it does not hold", so the
	// base block matches its start.
	w5NoHolderFinish = "outbox role message finished undelivered: its role has no live holder"
	w5PhaseChanged   = "workflow: phase changed"
	w5R2Note         = "Your previous turn was interrupted."
	// The cause the daemon's cut-turn note names for its own relaunch of a claim Envoy could not
	// reach (internal/daemon's relaunchedByDaemon), as the resumed session's transcript shows it:
	// "Your process ended because Envoy could not reach its session for 20 minutes, so the daemon
	// relaunched it on the same session."
	w5RelaunchCause = "Envoy could not reach its session for 20 minutes, so the daemon relaunched it on the same session"
	// The reasons R6's line gives for a claim out of reach a second time within 6 hours of its
	// relaunch, and for a pod none of whose claims Envoy reaches.
	w5SecondTime = "a second time within 6 hours"
	w5PodReason  = "Envoy reaches none of them"
)

// w5Run is one run of w5's main rig.
type w5Run struct {
	*w5Rig
	suffix string
	names  map[claim.Token]string
	files  string
	t0     time.Time

	blockMu sync.Mutex
	blocked map[string]time.Time

	envoyMu  sync.Mutex
	lastSeen map[string]time.Time

	// firstMarks is each claim's first view in read_state that shows it marked (watchMarks), and
	// seventhSvcGone when the seventh's named service ended (watchSeventhService).
	marksMu        sync.Mutex
	firstMarks     map[claim.Token]w5View
	seventhSvcGone time.Time

	roleRow, hookRow int64

	// The facts the later steps read of the first block. seenAtBlock is each blocked claim's last
	// registration as the listener's list showed it a minute into the block, before any relaunch
	// let a new process register again.
	seenAtBlock                       map[string]time.Time
	fifthMessageRow, fourthMessageRow int64
	fourthRetired, fifthJobEnded      time.Time
	ninthHeld                         time.Time
	fourthCallStarted                 time.Time
	implFrozen                        time.Time
	allBlockedAt, listFailedFrom      time.Time
	listAnswered                      time.Time

	// The stand-ins' answers a step waits on: each the moment the claim's turn was answered with
	// wait, the moment the resumed fifth's turn moved its issue's phase, and the eighth's
	// subagent's own request, held until the eighth's step lets it go.
	implWait, fifthWait, seventhWait, eighthWait, implAgainWait chan time.Time
	twelfthWaits, fifthMoved                                    chan time.Time
	eighthSubagent                                              *heldRequest
}

// w5MainClaims is the eleven claims of the main run, by the labels the scratch Dispatch's keys
// replace. R is the tree's root, W the workflow issue; X holds the pod of two.
func w5MainClaims(suffix string) []w5Claim {
	task := func(name string) string {
		return fmt.Sprintf("W5-%s-%s: the rig's task for this claim. Do what the turn asks.", strings.ToUpper(name), suffix)
	}
	return []w5Claim{
		{name: "architect", key: "R", tree: "R", phase: phase.Accepting, role: claim.RoleArchitect, provider: "w5arch", task: task("architect")},
		{name: "planner", key: "W", tree: "R", parent: "R", phase: phase.Implementing, role: claim.RolePlanner, provider: "w5wplan"},
		{name: "implementer", key: "W", tree: "R", parent: "R", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5impl", task: task("implementer")},
		{name: "third", key: "C3", tree: "C3", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5third", task: task("third")},
		{name: "fourth", key: "C4", tree: "C4", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5fourth", task: task("fourth")},
		{name: "fifth", key: "C5", tree: "C5", phase: phase.Reviewing, role: claim.RoleReviewer, provider: "w5fifth", task: task("fifth")},
		{name: "tenth", key: "X", tree: "X", phase: phase.Implementing, role: claim.RolePlanner, provider: "w5tenth"},
		{name: "sixth", key: "X", tree: "X", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5sixth", task: task("sixth")},
		{name: "seventh", key: "C7", tree: "C7", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5seventh", task: task("seventh")},
		{name: "eighth", key: "C8", tree: "R", parent: "R", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5eighth", task: task("eighth")},
		{name: "ninth", key: "C9", tree: "C9", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5ninth", task: task("ninth")},
		// The twelfth claim of w5: in a live wait on a named bash service, never stopped, its
		// registrations stopped with the first block's, its plugin sending its activity report in
		// its turn every 5 minutes; relaunch at 25 minutes is refused as a claim whose process still
		// runs its turn. The seventh, stopped in the same kind of wait on the same kind of service,
		// sends no report and is reached: it is the twelfth's control.
		{name: "twelfth", key: "C12", tree: "C12", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5twelfth", task: task("twelfth")},
	}
}

// w5FirstBlock is every claim the first block stops the registrations of.
var w5FirstBlock = []string{"implementer", "architect", "third", "fourth", "fifth", "seventh", "eighth", "ninth"}

// TestW5UnreachableClaimsLive runs w5's steps in the order the check gives them; the steps after
// the first block that wait on claims of their own run beside each other (afterTheFirstBlock).
func TestW5UnreachableClaimsLive(t *testing.T) {
	r := newW5Run(t)
	r.step("setup-every-claim-in-the-state-the-check-names", r.setup)
	r.step("first-block", r.firstBlock)
	r.step("after-the-first-block", r.afterTheFirstBlock)
	r.step("daemon-restart", r.daemonRestart)
	r.step("every-session-blocked-then-the-list-failing", r.everySessionBlocked)
	r.step("totals", r.totals)
}

func newW5Run(t *testing.T) *w5Run {
	t.Helper()
	suffix := randomSuffix(t)[:6]
	w := newW5Rig(t, w5Options{script: "scripts/e2e/unreachable-claims.sh", claims: w5MainClaims(suffix)})
	r := &w5Run{w5Rig: w, suffix: suffix, names: map[claim.Token]string{}, files: t.TempDir(),
		blocked: map[string]time.Time{}, lastSeen: map[string]time.Time{}, firstMarks: map[claim.Token]w5View{}}
	for name, c := range w.claims {
		r.names[c.token] = name
	}
	w.api.dropActivity(w.claims["ninth"].token)
	r.script()
	return r
}

// c is a claim by its name.
func (r *w5Run) c(name string) *w5Claim { return r.claims[name] }

func (r *w5Run) marker(name string) string {
	return fmt.Sprintf("W5-%s-%s", strings.ToUpper(name), r.suffix)
}

func (r *w5Run) file(name string) string { return filepath.Join(r.files, name) }

// serviceCall is the named bash service a claim starts, writing its pid to pidfile.
func serviceCall(name, pidfile string) map[string]any {
	return map[string]any{
		"command": fmt.Sprintf("echo $$ > %s; echo SVC-READY; exec sleep 100000", pidfile),
		"name":    name, "ready": map[string]any{"log": "SVC-READY", "timeout": 60}, "timeout": 120,
	}
}

func carrying(marker string, take func(seenRequest) bool) func(seenRequest) bool {
	return func(seen seenRequest) bool {
		return take(seen) && (strings.Contains(seen.last, marker) || strings.Contains(seen.conversation, marker))
	}
}

// waitLoop answers n turns' requests match takes with a wait call each, the first after the
// activity report's period so the session's report names what it runs, and answers every moment
// a wait was answered on one channel. A wait Oh My Pi returns from (at most 5 minutes on) is
// answered with the next, as an agent waiting on its work calls wait again; the rig stops the
// process inside one of them (freezeFromServed).
func waitLoop(s *w5Standin, n int, match func(seenRequest) bool) chan time.Time {
	all := make(chan time.Time, n+1)
	for i := range n {
		delay := time.Duration(0)
		if i == 0 {
			delay = w5ActivityReport
		}
		served := s.script(match, delay, "wait", map[string]any{})
		go func() {
			if at, ok := <-served; ok {
				all <- at
			}
		}()
	}
	return all
}

// script puts each claim's turns in front of its stand-in, before the daemon boots.
func (r *w5Run) script() {
	impl := r.c("implementer").stand
	impl.script(promptCarrying(r.marker("implementer")), 0, "bash", serviceCall("impl-svc", r.file("impl-svc.pid")))
	r.implWait = waitLoop(impl, 60, carrying(r.marker("implementer"), continuing))

	third := r.c("third").stand
	pty := map[string]any{"command": "sleep 290; date", "pty": true, "timeout": 600}
	third.script(promptCarrying(r.marker("third")), 0, "bash", pty)
	for range 80 {
		third.script(carrying(r.marker("third"), continuing), 0, "bash", pty)
	}

	fourth := r.c("fourth").stand
	fourth.script(promptCarrying(r.marker("fourth")), 0, "bash", map[string]any{"command": "sleep 2400; echo FOURTH-DONE", "pty": true, "timeout": 3000})

	fifth := r.c("fifth").stand
	fifth.script(promptCarrying(r.marker("fifth")), 0, "bash", map[string]any{
		"command": fmt.Sprintf("while [ ! -f %s ]; do sleep 5; done; echo FIFTH-JOB-DONE", r.file("fifth-job.done")), "timeout": 3600,
	})
	r.fifthWait = waitLoop(fifth, 60, func(seen seenRequest) bool {
		return carrying(r.marker("fifth"), continuing)(seen) && !strings.Contains(seen.last, "FIFTH-JOB-DONE")
	})

	seventh := r.c("seventh").stand
	seventh.script(promptCarrying(r.marker("seventh")), 0, "bash", serviceCall("seventh-svc", r.file("seventh-svc.pid")))
	r.seventhWait = waitLoop(seventh, 60, carrying(r.marker("seventh"), continuing))

	eighth := r.c("eighth").stand
	eighth.script(promptCarrying(r.marker("eighth")), 0, "bash", serviceCall("eighth-svc", r.file("eighth-svc.pid")))
	eighth.script(carrying(r.marker("eighth"), continuing), 0, "task", subagentCall(r.marker("eighth")))
	r.eighthSubagent = eighth.holdNext(func(seen seenRequest) bool { return seen.subagent })
	r.eighthWait = waitLoop(eighth, 120, carrying(r.marker("eighth"), continuing))

	twelfth := r.c("twelfth").stand
	twelfth.script(promptCarrying(r.marker("twelfth")), 0, "bash", serviceCall("twelfth-svc", r.file("twelfth-svc.pid")))
	r.twelfthWaits = waitLoop(twelfth, 400, carrying(r.marker("twelfth"), continuing))
}

// ---- setup ---------------------------------------------------------------------------------------

func (r *w5Run) setup() {
	r.boot()
	// The controller is named before the sampler, which reads the names, starts.
	r.names[r.ctl.token] = "controller"
	r.startSampler(r.names)
	go r.watchEnvoy()
	for _, c := range r.ordered {
		r.awaitLive(c.token)
	}
	r.awaitLive(r.ctl.token)
	r.implFrozen = r.freezeFromServed("implementer", r.implWait)
	r.freezeFromServed("fifth", r.fifthWait)
	r.freezeFromServed("seventh", r.seventhWait)
	select {
	case <-r.eighthSubagent.arrived:
		r.record("setup: the eighth's subagent's own request reached its stand-in at %s and is held", clockAt(r.eighthSubagent.at))
	case <-time.After(w5PaneReady):
		r.fails("in-a-wait", "the eighth's subagent sent no request in %s", w5PaneReady)
	}
	r.freezeFromServed("eighth", r.eighthWait)
	select {
	case at := <-r.twelfthWaits:
		r.record("setup: the twelfth's turn called wait on its named service at %s; its process is never stopped", clockAt(at))
	case <-time.After(w5PaneReady):
		r.fails("twelfth-in-a-wait", "the twelfth's stand-in answered no turn with wait in %s", w5PaneReady)
	}
	r.await("the third claim's first pty bash call", 3*time.Minute, func() bool {
		_, open := r.openToolCall(r.c("third").token, time.Time{})
		return open
	})
	r.await("the fourth claim's pty bash call", 3*time.Minute, func() bool {
		_, open := r.openToolCall(r.c("fourth").token, time.Time{})
		return open
	})
	r.fourthCallStarted, _ = r.openToolCall(r.c("fourth").token, time.Time{})
	for _, name := range []string{"architect", "sixth", "ninth"} {
		token := r.c(name).token
		r.await(name+" to answer its task and stand idle", w5PaneReady, func() bool {
			return r.claim(token).State == string(supervise.StateIdle)
		})
	}
	snap := r.snapshot()
	for _, c := range append(append([]*w5Claim{}, r.ordered...), r.ctl) {
		v := snap.views[c.token]
		r.record("setup: %s %s session %s: %s", c.name, c.token, r.claim(c.token).Session, v)
	}
	dropped, passed := r.api.activityCounts(r.c("ninth").token)
	r.record("setup: the ninth's activity reports dropped by the rig: %d, passed: %d", dropped, passed)
	r.check("setup-ninth-reports-dropped", passed == 0, "the rig dropped %d of the ninth's activity reports and passed %d", dropped, passed)
}

// awaitLive waits for token's pane to register and report ready.
func (r *w5Run) awaitLive(token claim.Token) {
	r.await(string(token)+"'s pane to register and report ready", w5PaneReady, func() bool {
		held := r.claimOrNil(token)
		if held == nil {
			return false
		}
		if held.State == string(supervise.StateFailed) {
			r.fails("ready", "%s failed before its pane reported ready; its output is under %s", token, r.rt.paneLogs)
		}
		return held.State == string(supervise.StateReady) || held.State == string(supervise.StateIdle) || held.State == string(supervise.StateWorking)
	})
}

// held is token as the operator's listing shows it, read without failing the step, so a flow of
// afterTheFirstBlock may read it from its own goroutine; the zero claim when it cannot be read.
func (r *w5Run) held(token claim.Token) api.OperatorClaim {
	status, body := r.apiCall(http.MethodGet, "/legion/v1/operator/claims", nil, true, 30*time.Second)
	var list api.OperatorClaims
	if status != http.StatusOK || json.Unmarshal(body, &list) != nil {
		return api.OperatorClaim{}
	}
	for _, c := range list.Claims {
		if c.Token == token {
			return c
		}
	}
	return api.OperatorClaim{}
}

// claimOrNil is token as the operator's listing shows it, nil when the daemon holds none.
func (r *w5Run) claimOrNil(token claim.Token) *struct{ State, Session string } {
	status, body := r.apiCall(http.MethodGet, "/legion/v1/operator/claims", nil, true, 30*time.Second)
	if status != http.StatusOK {
		return nil
	}
	var listing struct {
		Claims []struct {
			Token   claim.Token `json:"token"`
			State   string      `json:"state"`
			Session string      `json:"session"`
		} `json:"claims"`
	}
	if json.Unmarshal(body, &listing) != nil {
		return nil
	}
	for _, c := range listing.Claims {
		if c.Token == token {
			return &struct{ State, Session string }{c.State, c.Session}
		}
	}
	return nil
}

// freezeFromServed stops name's process inside a wait call of its wait loop: the one open now, or
// the next its stand-in answers.
func (r *w5Run) freezeFromServed(name string, served chan time.Time) time.Time {
	at, ok := r.nextOpenWait(r.c(name).token, served, w5PaneReady)
	if !ok {
		r.fails("in-a-wait", "%s's stand-in answered no turn with wait that opened a call in %s", name, w5PaneReady)
	}
	_, stopped := r.freezeInWait(r.c(name).token, at)
	return stopped
}

// nextOpenWait is the moment a wait of token's wait loop was answered whose call is open now: the
// latest one answered, if its call is still open, else the next one answered within within. It
// fails nothing, so a flow may call it from its own goroutine.
func (r *w5Run) nextOpenWait(token claim.Token, served chan time.Time, within time.Duration) (time.Time, bool) {
	var latest time.Time
	for drained := false; !drained; {
		select {
		case at := <-served:
			latest = at
		default:
			drained = true
		}
	}
	if !latest.IsZero() {
		if _, open := r.openToolCall(token, latest); open {
			return latest, true
		}
	}
	deadline := time.After(within)
	for {
		select {
		case at := <-served:
			for poll := time.Now().Add(3 * time.Second); time.Now().Before(poll); time.Sleep(100 * time.Millisecond) {
				if _, open := r.openToolCall(token, at); open {
					return at, true
				}
			}
		case <-deadline:
			return time.Time{}, false
		}
	}
}

// watchEnvoy keeps the latest registration the listener's own list shows of every session.
func (r *w5Run) watchEnvoy() {
	for {
		for session, s := range r.envoySessions() {
			r.envoyMu.Lock()
			if s.lastSeen.After(r.lastSeen[session]) {
				r.lastSeen[session] = s.lastSeen
			}
			r.envoyMu.Unlock()
		}
		time.Sleep(20 * time.Second)
	}
}

func (r *w5Run) lastSeenOf(session string) time.Time {
	r.envoyMu.Lock()
	defer r.envoyMu.Unlock()
	return r.lastSeen[session]
}

// registeredLast is session's last registration as the listener's own list shows it now, read
// straight from the listener (not watchEnvoy's copy, which a slow read leaves behind), every 10 s
// until a read answers with the session, for up to 2 minutes; the zero time where none did. A
// step reads it once the rig has blocked the session's registrations, so it is the last
// registration before the block, which no relaunch's new registration has moved yet.
func (r *w5Run) registeredLast(session string) time.Time {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if s, ok := r.envoySessions()[session]; ok && !s.lastSeen.IsZero() {
			return s.lastSeen
		}
		if time.Now().After(deadline) {
			return time.Time{}
		}
		time.Sleep(10 * time.Second)
	}
}

// session is the session token's claim records now.
func (r *w5Run) session(token claim.Token) string {
	if held := r.claimOrNil(token); held != nil {
		return held.Session
	}
	return ""
}

func (r *w5Run) block(names ...string) time.Time {
	at := time.Now()
	for _, name := range names {
		session := r.session(r.c(name).token)
		r.front.blockRegistrations(session)
		r.blockMu.Lock()
		r.blocked[name] = at
		r.blockMu.Unlock()
		r.record("blocked the registrations of %s (%s, session %s) at %s", name, r.c(name).token, session, clockAt(at))
	}
	return at
}

func (r *w5Run) unblock(names ...string) {
	for _, name := range names {
		session := r.session(r.c(name).token)
		r.front.unblock(session)
		r.blockMu.Lock()
		delete(r.blocked, name)
		r.blockMu.Unlock()
		r.record("let the registrations of %s (session %s) through again at %s", name, session, clockAt(time.Now()))
	}
}

// blockedNames is every claim whose registrations the rig blocks now.
func (r *w5Run) blockedNames() []string {
	r.blockMu.Lock()
	defer r.blockMu.Unlock()
	names := make([]string, 0, len(r.blocked))
	for name := range r.blocked {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// liftOnRelaunch lets each of names' registrations through at the first stop of its process that
// a relaunch makes (podRuntime.liftOnStop): its block stands for a cause in that process.
func (r *w5Run) liftOnRelaunch(names ...string) {
	for _, name := range names {
		session := r.session(r.c(name).token)
		r.pods.liftOnStop(r.c(name).token, func() {
			r.front.unblock(session)
			r.blockMu.Lock()
			delete(r.blocked, name)
			r.blockMu.Unlock()
			r.record("let the registrations of %s (session %s) through at its relaunch's stop, %s", name, session, clockAt(time.Now()))
		})
	}
}

// markOf is the daemon's out-of-reach line for token written at or after from, nil for none.
func (w *w5Rig) markOf(token claim.Token, from time.Time) *loggedRecord {
	for _, rec := range w.logs(func(rec loggedRecord) bool { return rec.Message == w5MarkLine && rec.Attrs["claim"] == string(token) }) {
		if !rec.at.Before(from) {
			found := rec
			return &found
		}
	}
	return nil
}

// guardLineOf is the daemon's line that its relaunch guard refused its relaunch of token, written
// at or after from, nil for none.
func (w *w5Rig) guardLineOf(token claim.Token, from time.Time) *loggedRecord {
	for _, rec := range w.logs(func(rec loggedRecord) bool {
		return rec.Message == w5RefusedLine && rec.Attrs["claim"] == string(token) && strings.HasPrefix(fmt.Sprint(rec.Attrs["reason"]), w5GuardReason)
	}) {
		if !rec.at.Before(from) {
			found := rec
			return &found
		}
	}
	return nil
}

// claimLines is every R6 line of the case word names that names token, written at or after from.
func (w *w5Rig) claimLines(word string, token claim.Token, from time.Time) []loggedRecord {
	return w.logs(func(rec loggedRecord) bool {
		return strings.HasPrefix(rec.Message, w5FleetAlertPrefix) && fmt.Sprint(rec.Attrs["case"]) == word &&
			fmt.Sprint(rec.Attrs["claim"]) == string(token) && !rec.at.Before(from)
	})
}

// reasonOf is the reason a logged line gives, "" for none.
func reasonOf(rec *loggedRecord) string {
	if rec == nil || rec.Attrs["reason"] == nil {
		return ""
	}
	return fmt.Sprint(rec.Attrs["reason"])
}

// describeLine is one logged line as a note prints it, "none" for no line.
func describeLine(rec *loggedRecord) string {
	if rec == nil {
		return "none"
	}
	return w5Describe(*rec)
}

// firstOf is the first of lines, nil for none.
func firstOf(lines []loggedRecord) *loggedRecord {
	if len(lines) == 0 {
		return nil
	}
	return &lines[0]
}

// relaunchStops is every stop of token's process that a relaunch made from from and that ended
// it, as the rig's runtime answered it.
func (r *w5Run) relaunchStops(token claim.Token, from time.Time) []podRelaunchCall {
	var stops []podRelaunchCall
	for _, c := range r.pods.relaunchCalls(token, from) {
		if c.call == "StopForRelaunch" && c.answer == "ok" {
			stops = append(stops, c)
		}
	}
	return stops
}

// describeCalls is each relaunch call as a note prints it.
func describeCalls(calls []podRelaunchCall) []string {
	described := make([]string, 0, len(calls))
	for _, c := range calls {
		described = append(described, fmt.Sprintf("%s %s may=%t launcher=%q answered %q running=%t", clockAt(c.at), c.call, c.mayReplacePod, c.launcher, c.answer, c.running))
	}
	return described
}

// watchMarks reads read_state every 5 s from from to to and keeps each claim's first view that
// shows it marked: a claim the daemon relaunches at its mark, whose new process registers again,
// shows its mark for about a minute only.
func (r *w5Run) watchMarks(from, to time.Time) {
	sleepUntil(from)
	for time.Now().Before(to) {
		snap := r.snapshot()
		r.marksMu.Lock()
		for token, v := range snap.views {
			if _, kept := r.firstMarks[token]; !kept && v.marked() {
				r.firstMarks[token] = v
			}
		}
		r.marksMu.Unlock()
		time.Sleep(5 * time.Second)
	}
}

// firstMark is token's first view watchMarks kept that shows it marked.
func (r *w5Run) firstMark(token claim.Token) (w5View, bool) {
	r.marksMu.Lock()
	defer r.marksMu.Unlock()
	v, ok := r.firstMarks[token]
	return v, ok
}

// watchSeventhService keeps when the seventh's named service, running at pid, ends.
func (r *w5Run) watchSeventhService(pid int) {
	for pid > 0 && syscall.Kill(pid, 0) == nil {
		time.Sleep(time.Second)
	}
	r.marksMu.Lock()
	r.seventhSvcGone = time.Now()
	r.marksMu.Unlock()
}

func sleepUntil(at time.Time) {
	if d := time.Until(at); d > 0 {
		time.Sleep(d)
	}
}

// ---- the first block -----------------------------------------------------------------------------

func (r *w5Run) firstBlock() {
	before := r.answeredSnapshot("first block, before the block")
	impl := r.c("implementer")
	// The resumed implementer is put back in a wait loop on its owed task's turn (implementerAgain);
	// the old process's own loop is spent first, since its rules would take the resumed session's
	// turns too, whose conversation still carries the old task's marker. The daemon relaunches the
	// implementer at its mark, so both are in place before the block.
	impl.stand.captureFrom()
	impl.stand.dropRules()
	r.implAgainWait = r.scriptBackInAWait(impl, "impl-svc2", w5RelaunchCause)
	seventhPid := readPid(r.file("seventh-svc.pid"))
	r.record("first block: the seventh's named service runs as pid %d: %t", seventhPid, seventhPid > 0 && syscall.Kill(seventhPid, 0) == nil)
	go r.watchSeventhService(seventhPid)
	r.t0 = r.block(append([]string{"twelfth"}, w5FirstBlock...)...)
	t0 := r.t0
	r.liftOnRelaunch("implementer", "architect", "seventh")
	r.roleRow = r.enqueueRow(impl.key, record.RoleMessage{Role: string(impl.token), Body: fmt.Sprintf("W5-ROLEMSG-%s: a role message the daemon publishes to the implementer while Envoy cannot reach it.", r.suffix)})
	r.hookRow = r.enqueueRow(impl.key, record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing,
		Count: 1, Counts: map[string]int{"reviewing-implementing": 1}, Prompt: fmt.Sprintf("W5-HOOK-%s: read the review before you write anything.", r.suffix)})
	r.record("first block: the daemon's outbox holds a role message (row %d) and a hook prompt (row %d) for %s", r.roleRow, r.hookRow, impl.token)

	sleepUntil(t0.Add(time.Minute))
	r.seenAtBlock = map[string]time.Time{}
	for _, name := range w5FirstBlock {
		session := r.session(r.c(name).token)
		r.seenAtBlock[name] = r.registeredLast(session)
		r.record("first block: %s's last registration as the listener's own list shows it: %s", name, clockAt(r.seenAtBlock[name]))
	}
	if os.Getenv(w5BaseRunEnv) != "" {
		r.baseFirstBlock()
		return
	}

	sleepUntil(t0.Add(w5FirstRead))
	at10 := r.answeredSnapshot("first block, at 10 minutes")
	var marked, relaunched []string
	for _, name := range w5FirstBlock {
		if at10.views[r.c(name).token].marked() {
			marked = append(marked, name)
		}
		if calls := r.pods.relaunchCalls(r.c(name).token, t0); len(calls) > 0 {
			relaunched = append(relaunched, fmt.Sprintf("%s %v", name, describeCalls(calls)))
		}
	}
	r.check("at-10m-no-blocked-claim-marked", len(marked) == 0, "at %s (%s after the block) the marked blocked claims are %v", clockAt(at10.at), at10.at.Sub(t0).Round(time.Second), marked)
	r.check("at-10m-no-blocked-claim-relaunched", len(relaunched) == 0, "by %s the daemon asked the runtime to relaunch %v", clockAt(at10.at), relaunched)

	r.watchMarks(t0.Add(w5Unreachable-time.Minute), t0.Add(w5MarkWindow+30*time.Second))
	at22 := r.answeredSnapshot("first block, at 22 minutes")
	r.recordEmptyReads("first block", t0.Add(-5*time.Minute))
	if at22.err != "" {
		r.check("read-state-answers-at-22m", false, "read_state did not answer from %s for 5 minutes: %s", clockAt(at22.at), at22.err)
	}
	for _, name := range w5FirstBlock {
		r.checkMarked(name, before, t0)
	}
	r.checkNoControllerNotice(w5FirstBlock, t0, "first-block")
	r.checkRelaunchedAtTheMark(before, t0)
	r.checkNoArchitectNotice(t0)

	sleepUntil(t0.Add(w5GuardRead))
	r.guardHolds()
	r.twelfthHeld(before)

	sleepUntil(t0.Add(w5GuardRead + 30*time.Second))
	r.afterTheRefusals()
	r.implementerRelaunched(before)
}

// checkMarked reads name's mark: between 20 and 22 minutes after its session's last registration,
// shown in read_state with that time, and its state kept, as the daemon's line of the mark names
// it, since a claim the daemon relaunches at its mark leaves that state at once. The last
// registration lies between the one the rig's list read showed a minute into the block and the
// block itself, which no registration passes (markedInTime).
func (r *w5Run) checkMarked(name string, before w5Snapshot, from time.Time) {
	c := r.c(name)
	mark := r.markOf(c.token, from)
	seen := r.seenAtBlock[name]
	if mark == nil {
		r.check("marked-20-to-22m-"+name, false, "the daemon wrote no out-of-reach line for %s", c.token)
		return
	}
	r.check("marked-20-to-22m-"+name, markedInTime(mark.at, seen),
		"%s marked at %s, %s after its session's last registration the rig read, %s, and %s after the block at %s (the daemon logged since %v, lastSeen %v)",
		c.token, clockAt(mark.at), mark.at.Sub(seen).Round(time.Second), clockAt(seen), mark.at.Sub(from).Round(time.Second), clockAt(from), mark.Attrs["since"], mark.Attrs["lastSeen"])
	v, shown := r.firstMark(c.token)
	sinceAt, _ := time.Parse(time.RFC3339Nano, v.since)
	r.check("reads-unreachable-with-that-time-"+name, shown && !sinceAt.Before(seen.Add(-2*time.Second)) && !sinceAt.After(from),
		"read_state first showed %s %s; its session's last registration the rig read was %s, and the block came at %s", c.token, v, clockAt(seen), clockAt(from))
	r.check("keeps-its-state-"+name, fmt.Sprint(mark.Attrs["state"]) == before.views[c.token].state,
		"%s was %s before the block and %v at its mark, as the daemon's line names it", c.token, before.views[c.token].state, mark.Attrs["state"])
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// checkNoControllerNotice holds that no notice to the controller from from names a claim of names:
// the daemon relaunches each, and what it cannot get past goes to R6's line.
func (r *w5Run) checkNoControllerNotice(names []string, from time.Time, label string) {
	var naming []string
	for _, n := range r.controllerNoticesW5() {
		if n.at.Before(from) {
			continue
		}
		for _, name := range names {
			token := string(r.c(name).token)
			if n.claim == token || containsString(n.rows, token) || strings.Contains(n.reason, token) {
				naming = append(naming, n.String())
			}
		}
	}
	r.check(label+"-no-controller-notice-names-a-claim-out-of-reach", len(naming) == 0, "controller notices from %s that name one of %v: %v", clockAt(from), names, naming)
}

// checkNoArchitectNotice reads the architect's notice rows the outbox wrote from from, and the
// forwards the stream stored for the architect's session: no notice of a claim out of reach is
// written for it or reaches it, since the daemon relaunches the claim and a person reads what a
// relaunch cannot fix in R6's line. A notice is of a claim out of reach by its kind, or by a reason
// that says a session has not registered with Envoy. So that none is no blind read, the rig then
// sends the relaunched architect a role message of its own and reads it among those forwards.
func (r *w5Run) checkNoArchitectNotice(from time.Time) {
	arch := r.c("architect")
	ofReach := func(kind, reason string) bool {
		return kind == w5NoticeClaimOutOfReach || strings.Contains(reason, "registered with Envoy")
	}
	var rows []string
	for _, life := range r.rowLives("notice") {
		var payload struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(life.payload), &payload)
		if !life.written.Before(from) && ofReach(payload.Kind, payload.Reason) {
			rows = append(rows, fmt.Sprintf("row %d on %s written %s: %s", life.id, life.issue, clockAt(life.written), truncate(life.payload, 200)))
		}
	}
	sort.Strings(rows)
	session := r.session(arch.token)
	topic := notify.RoleTopicPrefix + string(arch.token)
	var reached []string
	for _, n := range r.forwardedTo(session, topic) {
		reason, _ := n.raw["reason"].(string)
		if !n.at.Before(from) && ofReach(n.kind, reason) {
			reached = append(reached, n.String())
		}
	}
	r.check("no-notice-of-a-claim-out-of-reach-reaches-the-architect", len(rows) == 0 && len(reached) == 0,
		"notice rows of a claim out of reach from %s: %v; forwards of one to the architect's session: %v", clockAt(from), rows, reached)
	canary := "W5-ARCH-CANARY-" + r.suffix
	row := r.enqueueRow(arch.key, record.RoleMessage{Role: string(arch.token),
		Body: canary + ": a role message the rig sends the architect, so that the read of its forwards is seen to read one."})
	seen := waitFor(5*time.Minute, func() bool { return r.forwardHolds(session, canary) })
	r.check("the-architects-forwards-are-read", seen,
		"the rig's role message (row %d) reached the architect's session %s within 5 minutes: %t", row, session, seen)
}

// checkRelaunchedAtTheMark reads the daemon's own relaunch of the implementer, the architect and
// the seventh, each with nothing running in its process: one stop of its process within 2 minutes
// of its mark, a launch at a later generation on the same session, and no charge. The architect is
// a claim alone in its pod. The seventh's named service, which no activity report names, ends with
// its old process.
func (r *w5Run) checkRelaunchedAtTheMark(before w5Snapshot, from time.Time) {
	for _, name := range []string{"implementer", "architect", "seventh"} {
		c := r.c(name)
		b := before.views[c.token]
		mark := r.markOf(c.token, from)
		stops := r.relaunchStops(c.token, from)
		atMark := mark != nil && len(stops) == 1 && !stops[0].at.Before(mark.at) && stops[0].at.Sub(mark.at) <= w5LineSlack
		r.check("relaunched-at-its-mark-"+name, atMark, "%s marked at %s; the stops of its process a relaunch made: %v", c.token, describeLine(mark), describeCalls(stops))
		live := waitFor(w5PaneReady, func() bool {
			held := r.claim(c.token)
			return held.Generation > b.generation && (held.State == string(supervise.StateReady) || held.State == string(supervise.StateIdle) || held.State == string(supervise.StateWorking))
		})
		after := r.claim(c.token)
		r.check("relaunch-reaches-it-on-its-session-with-no-charge-"+name, live && after.Session == b.session &&
			after.Budgets.LaunchFailures == b.launchFailures && after.Budgets.Deaths == b.deaths,
			"before the block %s; after the relaunch %s gen %d on session %s, budgets %+v", b, after.State, after.Generation, after.Session, after.Budgets)
	}
	r.marksMu.Lock()
	gone := r.seventhSvcGone
	r.marksMu.Unlock()
	stops := r.relaunchStops(r.c("seventh").token, from)
	r.check("seventh-service-stops-with-the-old-process", len(stops) > 0 && !gone.IsZero() && !gone.Before(stops[0].at.Add(-time.Second)) && gone.Sub(stops[0].at) <= w5LineSlack,
		"the seventh's named service ended at %s; its relaunch's stops: %v", clockAt(gone), describeCalls(stops))
}

// guardHolds reads, 25 minutes after the block, the claims the daemon's relaunch guard holds: the
// third, whose worker stream shows steps, the fourth, whose open call is not a wait, the fifth,
// whose activity report shows background work, the eighth, whose report shows a subagent, and the
// ninth, which has no activity report since its launch. The daemon asks the runtime nothing of any
// of them, and its guard line names the reason of its first refusal. The fifth and the eighth were
// frozen in their waits just after a step, so that first refusal may be the step rule, naming a
// step from before the block, which lapses within the minute after the mark; that they are then
// held for background work their flows read, each relaunched only after its job or subagent ends.
// The third's registrations then go through again: its process still takes its steps, and nothing
// of the check waits on it.
func (r *w5Run) guardHolds() {
	cases := []struct {
		name, want string
		frozen     bool
	}{
		{"third", "worker stream showed a step", false},
		{"fourth", "open call is bash", false},
		{"fifth", "background work", true},
		{"eighth", "background work", true},
		{"ninth", "no activity report", false},
	}
	for _, tc := range cases {
		c := r.c(tc.name)
		line := r.guardLineOf(c.token, r.t0)
		calls := r.pods.relaunchCalls(c.token, r.t0)
		reason := reasonOf(line)
		named := strings.Contains(reason, tc.want) || (tc.frozen && r.stepBeforeTheBlock(reason))
		r.check("at-25m-guard-holds-"+tc.name, line != nil && len(calls) == 0 && named,
			"the daemon's guard line for %s: %s; its relaunch calls from the block: %v; want none and a reason naming %q%s", c.token, describeLine(line), describeCalls(calls), tc.want,
			map[bool]string{true: ", or the step rule for a step before the block", false: ""}[tc.frozen])
		if tc.name == "ninth" && line != nil {
			r.ninthHeld = line.at
		}
		if tc.name == "eighth" && strings.Contains(reason, tc.want) {
			r.check("eighth-hold-names-a-subagent", strings.Contains(strings.ToLower(reason), "agent"), "the eighth's guard line: %s", describeLine(line))
		}
	}
	r.unblock("third")
}

// stepBeforeTheBlock is whether reason is the guard's step rule naming a step from before the
// first block.
func (r *w5Run) stepBeforeTheBlock(reason string) bool {
	named, ok := namedAt(reason, "its worker stream showed a step at ")
	return ok && named.Before(r.t0)
}

// twelfthHeld reads the twelfth claim, in a live wait on a named bash service the rig never
// stopped, its registrations stopped with the first block's: its plugin sends its activity report
// in its turn every 5 minutes (and on a build whose wait returns, the wait is called again), so the
// daemon's relaunch guard holds it as a claim whose process still runs its turn, by either rule of
// the guard that reads one (twelfthHeldInItsTurn); the daemon asks the runtime nothing of it, its
// process keeps running, and it is charged nothing. It records each report the twelfth sent in its
// turn and each step its worker stream carried, with their times, the step record the daemon stores
// and its mark, then lets its registrations through again.
func (r *w5Run) twelfthHeld(before w5Snapshot) {
	c := r.c("twelfth")
	var stepAt, started, ended *time.Time
	var tool string
	_ = r.st.Pool().QueryRow(context.Background(), "select step_at, step_tool, step_started_at, step_ended_at from claims where token = $1", string(c.token)).
		Scan(&stepAt, &tool, &started, &ended)
	mark := r.markOf(c.token, r.t0)
	line := r.guardLineOf(c.token, r.t0)
	launch := r.runtime.current(c.token)
	var steps []string
	var stepTimes []time.Time
	var turnAt time.Time
	for _, each := range r.runtime.launchesOf(c.token) {
		for _, conn := range r.relay.connsOf(each.spec.BootToken) {
			for _, f := range conn.toDaemon {
				if f.typ == "agent_start" && f.at.After(turnAt) {
					turnAt = f.at
				}
				if !f.at.Before(r.t0) && (f.typ == "tool_execution_start" || f.typ == "tool_execution_end" || f.typ == "agent_start" || f.typ == "agent_end") {
					steps = append(steps, clockAt(f.at)+" "+f.typ)
					stepTimes = append(stepTimes, f.at)
				}
			}
		}
	}
	reportTimes := r.api.reportsFrom(c.token, turnAt)
	var reports []string
	for _, at := range reportTimes {
		reports = append(reports, clockAt(at))
	}
	after := r.snapshot().views[c.token]
	b := before.views[c.token]
	r.record("twelfth (never stopped, in a live wait on its named service, registrations blocked at %s): its turn began at %s; %d activity reports in its turn: %v", clockAt(r.t0), clockAt(turnAt), len(reports), reports)
	r.record("twelfth: %d worker-stream steps from the block: %v", len(steps), steps)
	r.record("twelfth: the daemon's step record: at %s, tool %q, started %s, ended %s; marked unreachable at %s; the guard's line %s",
		clockAtPtr(stepAt), tool, clockAtPtr(started), clockAtPtr(ended), clockAtLine(mark), describeLine(line))
	hold := twelfthHold{reason: reasonOf(line), steps: stepTimes, reports: reportTimes}
	if line != nil {
		hold.at = line.at
	}
	calls := r.pods.relaunchCalls(c.token, r.t0)
	r.check("at-25m-guard-holds-the-twelfth-whose-process-still-runs-its-turn", twelfthHeldInItsTurn(hold) && len(calls) == 0,
		"the guard's line %s; the twelfth's relaunch calls from the block: %v", describeLine(line), describeCalls(calls))
	runs := launch != nil && launch.running() && launch.spec.Generation == b.generation
	r.check("twelfth-keeps-running", runs, "its process of the block's generation %d still runs: %t", b.generation, runs)
	r.check("twelfth-charged-nothing", after.launchFailures == b.launchFailures && after.deaths == b.deaths, "before the block %s; at 25 minutes %s", b, after)
	r.unblock("twelfth")
}

func clockAtPtr(at *time.Time) string {
	if at == nil {
		return "never"
	}
	return clockAt(*at)
}

func readPid(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pid int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid)
	return pid
}

// afterTheRefusals is what the check does after the 25-minute refusals that needs the fourth and
// the fifth still out of reach: a role message to each, the fourth's claim retired, the fifth's job
// ended and its issue's phase moved on by its own turn, and one failed read of the list.
func (r *w5Run) afterTheRefusals() {
	fourth, fifth := r.c("fourth"), r.c("fifth")
	r.fourthMessageRow = r.enqueueRow(fourth.key, record.RoleMessage{Role: string(fourth.token), Body: "W5-FOURTH-MSG-" + r.suffix})
	r.fifthMessageRow = r.enqueueRow(fifth.key, record.RoleMessage{Role: string(fifth.token), Body: "W5-FIFTH-MSG-" + r.suffix})
	r.record("held messages: row %d to the fourth, row %d to the fifth", r.fourthMessageRow, r.fifthMessageRow)
	time.Sleep(90 * time.Second)
	lives := r.rowLives()
	r.check("fourth-message-held-before-its-retire", lives[r.fourthMessageRow] != nil && lives[r.fourthMessageRow].finished.IsZero(),
		"the fourth's row %d: %+v", r.fourthMessageRow, lives[r.fourthMessageRow])
	r.check("fifth-message-held-before-its-phase-moves", lives[r.fifthMessageRow] != nil && lives[r.fifthMessageRow].finished.IsZero(),
		"the fifth's row %d: %+v", r.fifthMessageRow, lives[r.fifthMessageRow])

	// The failed read, while the claims are marked.
	before := r.answeredSnapshot("the failed read, before it")
	noticesBefore := len(r.controllerNoticesW5())
	failedAt := time.Now()
	r.envoy.failReads(1)
	r.await("the daemon's next read of Envoy's list", 3*time.Minute, func() bool {
		for _, read := range r.envoy.readsFrom(failedAt) {
			if read.failed {
				return true
			}
		}
		return false
	})
	time.Sleep(70 * time.Second)
	after := r.answeredSnapshot("the failed read, after it")
	changed := diffMarks(before, after)
	r.check("failed-read-changes-no-mark", len(changed) == 0, "the marks that changed across the failed read at %s: %v", clockAt(failedAt), changed)
	r.check("failed-read-sends-nothing", len(r.controllerNoticesW5()) == noticesBefore, "controller notices before %d, after %d", noticesBefore, len(r.controllerNoticesW5()))

	// The fourth's claim retires.
	status, _ := r.stopClaim(fourth.token)
	r.fourthRetired = time.Now()
	r.check("fourth-retired", status == http.StatusOK, "stop answered %d", status)

	// The fifth's job ends: the file it waits on is written, and its process runs again. Its wait
	// returns the job's end, and its next turn starts a named service — which no activity report
	// names, as the seventh's — and waits on it, so once its report shows no job the rig stops it
	// again inside that wait, as the check says: a wait with nothing to wait on returns at once
	// (Oh My Pi's wait.ts, "Nothing to wait for"), and one on a service blocks.
	// The resumed fifth's turn on its owed task, after the daemon relaunches it, moves its issue's
	// phase on (fifthFlow): only a relaunched session's prompt carries the relaunch's cause.
	r.fifthMoved = fifth.stand.script(func(seen seenRequest) bool {
		return seen.prompt && anyTurn(seen) && (strings.Contains(seen.last, w5RelaunchCause) || strings.Contains(seen.conversation, w5RelaunchCause))
	}, 0, "legion", map[string]any{"op": "request_backward_move", "to": "implementing", "reason": "w5: the review asks for changes"})
	writeProofFile(r.t, r.file("fifth-job.done"), "done\n")
	r.fifthJobEnded = time.Now()
	serviceStarted := fifth.stand.script(func(seen seenRequest) bool {
		return anyTurn(seen) && strings.Contains(seen.last, "FIFTH-JOB-DONE")
	}, 0, "bash", serviceCall("fifth-svc", r.file("fifth-svc.pid")))
	r.thaw(fifth.token)
	select {
	case at := <-serviceStarted:
		r.record("the fifth's job ended; its next turn started a named service at %s", clockAt(at))
	case <-time.After(5 * time.Minute):
		r.check("precondition-fifth-starts-its-service", false, "the fifth's turn did not start its service within 5 minutes of its job's end")
	}
	noJob := waitFor(3*time.Minute, func() bool {
		background, at := r.activityOf(fifth.token)
		return !at.IsZero() && at.After(time.Now().Add(-time.Minute)) && !background
	})
	r.check("precondition-fifth-report-shows-no-job-after-its-end", noJob, "the fifth's activity report shows no background work within 3 minutes of its job's end: %t", noJob)
	r.freezeFromServed("fifth", r.fifthWait)
	time.Sleep(time.Minute)
	lives = r.rowLives()
	fourthLife := lives[r.fourthMessageRow]
	r.record("held messages: the fourth's row %+v (retired at %s)", fourthLife, clockAt(r.fourthRetired))
	r.check("fourth-message-finished-at-retire", fourthLife != nil && !fourthLife.finished.IsZero() && !fourthLife.finished.Before(r.fourthRetired.Add(-5*time.Second)) &&
		len(r.logs(func(rec loggedRecord) bool {
			return rec.Message == w5HeldFinishEnded && fmt.Sprint(rec.Attrs["row"]) == fmt.Sprint(r.fourthMessageRow)
		})) == 1,
		"the fourth's row finished %s, its claim retired at %s", clockAt(fourthLife.finishedOrZero()), clockAt(r.fourthRetired))
	// The fifth's message stays held until its issue's phase moves on (fifthFlow).
	r.check("fifth-message-held-after-its-job-ends", lives[r.fifthMessageRow] != nil && lives[r.fifthMessageRow].finished.IsZero(),
		"the fifth's row %d: %+v", r.fifthMessageRow, lives[r.fifthMessageRow])
}

func (l *rowLife) finishedOrZero() time.Time {
	if l == nil {
		return time.Time{}
	}
	return l.finished
}

// diffMarks is every claim whose mark differs between two readings.
func diffMarks(before, after w5Snapshot) []string {
	var changed []string
	for token, v := range before.views {
		if after.views[token].since != v.since {
			changed = append(changed, fmt.Sprintf("%s %q -> %q", token, v.since, after.views[token].since))
		}
	}
	for token, v := range after.views {
		if _, ok := before.views[token]; !ok && v.marked() {
			changed = append(changed, fmt.Sprintf("%s new mark %q", token, v.since))
		}
	}
	sort.Strings(changed)
	return changed
}

// implementerRelaunched reads the daemon's own relaunch of the implementer at its mark: its old
// process stopped, the claim reached on its same session with no charge, and the resumed session
// given R2's note, the role message, the hook prompt, and its owed task behind the daemon's
// cut-turn note naming the relaunch.
func (r *w5Run) implementerRelaunched(before w5Snapshot) {
	impl := r.c("implementer")
	b := before.views[impl.token]
	stops := r.relaunchStops(impl.token, r.t0)
	if len(stops) == 0 {
		r.check("relaunch-reaches-the-implementer", false, "the daemon made no relaunch of %s from the block at %s", impl.token, clockAt(r.t0))
		return
	}
	stoppedAt := stops[0].at
	var old *rigLaunch
	for _, launch := range r.runtime.launchesOf(impl.token) {
		if launch.spec.Generation == b.generation {
			old = launch
		}
	}
	r.check("relaunch-stops-the-implementers-old-process", old != nil && !old.running(), "the old process of generation %d ended: %t; its relaunch's stop at %s",
		b.generation, old != nil && !old.running(), clockAt(stoppedAt))
	r.await("the implementer live on its resumed session", w5PaneReady, func() bool {
		held := r.claim(impl.token)
		return held.Generation > b.generation && (held.State == string(supervise.StateReady) || held.State == string(supervise.StateIdle) || held.State == string(supervise.StateWorking))
	})
	after := r.claim(impl.token)
	r.check("relaunch-charges-the-implementer-nothing", after.Budgets.LaunchFailures == b.launchFailures && after.Budgets.Deaths == b.deaths,
		"before the block %s; after the relaunch budgets %+v", b, after.Budgets)
	r.check("relaunch-keeps-the-implementers-session", after.Session == b.session, "session before %s, after %s", b.session, after.Session)
	var conversation strings.Builder
	r.await("the resumed implementer's turns to carry the role message, the hook prompt and its task", 6*time.Minute, func() bool {
		conversation.Reset()
		for _, seen := range impl.stand.requestsAfter(stoppedAt) {
			conversation.WriteString(seen.whole)
			conversation.WriteString(seen.last)
		}
		text := conversation.String()
		return strings.Contains(text, "W5-ROLEMSG-"+r.suffix) && strings.Contains(text, "W5-HOOK-"+r.suffix) && strings.Contains(text, w5RelaunchCause)
	})
	text := conversation.String()
	r.check("resumed-session-gets-r2-note", strings.Contains(text, w5R2Note), "the resumed session's requests carry %q: %t", w5R2Note, strings.Contains(text, w5R2Note))
	r.check("resumed-session-gets-the-role-message", strings.Contains(text, "W5-ROLEMSG-"+r.suffix), "carried")
	r.check("resumed-session-gets-the-hook-prompt", strings.Contains(text, "W5-HOOK-"+r.suffix), "carried")
	r.check("resumed-session-gets-its-task-behind-the-cut-turn-note", strings.Contains(text, w5RelaunchCause), "carried %q", w5RelaunchCause)
	r.record("the resumed implementer's requests from the relaunch: %d; the note's context: %s", len(impl.stand.requestsAfter(stoppedAt)), excerpt(text, w5R2Note, 400))
	r.record("the cut-turn note's context: %s", excerpt(text, w5RelaunchCause, 600))
	lives := r.rowLives()
	r.record("the role message row %+v; the hook prompt row %+v", lives[r.roleRow], lives[r.hookRow])
}

func excerpt(text, around string, width int) string {
	i := strings.Index(text, around)
	if i < 0 {
		return "(absent)"
	}
	start := max(0, i-width/2)
	end := min(len(text), i+len(around)+width/2)
	return strings.ReplaceAll(text[start:end], "\n", " ⏎ ")
}

// scriptBackInAWait has claim c's next turn whose prompt carries marker start a named service and
// call wait in that turn once the service is ready, and answers when the wait was served. The wait
// loop takes every role turn's request whose conversation carries the service's ready after
// marker: the call's own result, and the request Oh My Pi sends when a message lands behind that
// result, which ends the request in a text block that `continuing` does not take. The resumed
// implementer's does: the broker's "Supervised process impl-svc exited without an exit code." for
// the old process's service came 5 ms after impl-svc2's result (main5, 22:48:34.739Z), and the
// stand-in answered that turn with text, so no wait was ever called.
func (r *w5Run) scriptBackInAWait(c *w5Claim, service, marker string) chan time.Time {
	c.stand.script(func(seen seenRequest) bool {
		return seen.prompt && anyTurn(seen) && strings.Contains(seen.last, marker)
	}, 0, "bash",
		serviceCall(service, r.file(service+".pid")))
	return waitLoop(c.stand, 120, func(seen seenRequest) bool {
		at := strings.Index(seen.conversation, marker)
		return anyTurn(seen) && at >= 0 && strings.Contains(seen.conversation[at:], "SVC-READY")
	})
}

// baseFirstBlock is the first block where the daemon has no reach keeper, which is the base's
// code: at 22 minutes read_state shows the implementer's claim working with no mark, no notice
// names it, fresh_session refuses it CLAIM_IN_A_TURN, and the role message is finished undelivered
// once Envoy reads the session gone. It reads each and fails the step at the mark with all of it,
// which the script's negative control reads back.
func (r *w5Run) baseFirstBlock() {
	impl := r.c("implementer")
	r.record("base: the first block over the base's daemon, which marks no claim and relaunches none")
	sleepUntil(r.t0.Add(w5MarkWindow))
	snap := r.answeredSnapshot("base, at 22 minutes")
	v := snap.views[impl.token]
	named := 0
	for _, n := range r.controllerNoticesW5() {
		if n.at.Before(r.t0) {
			continue
		}
		if n.claim == string(impl.token) || strings.Contains(n.reason, string(impl.token)) || containsString(n.rows, string(impl.token)) {
			named++
		}
	}
	for _, n := range r.roleNotices(r.c("architect").token) {
		if !n.at.Before(r.t0) && (n.claim == string(impl.token) || strings.Contains(n.reason, string(impl.token))) {
			named++
		}
	}
	fresh := r.freshSession(impl.key, claim.RoleImplementer)
	lives := r.rowLives()
	life := lives[r.roleRow]
	finishedLine := r.logs(func(rec loggedRecord) bool {
		return strings.HasPrefix(rec.Message, w5NoHolderFinish) && fmt.Sprint(rec.Attrs["row"]) == fmt.Sprint(r.roleRow)
	})
	session := r.session(impl.token)
	envoy := r.envoySessions()[session]
	r.record("base: at %s (%s after the block) read_state shows %s %s; %d notices name it; fresh_session %s; the role message row %d %+v, finished-undelivered lines %d; Envoy lists the session as %q (last seen %s)",
		clockAt(snap.at), snap.at.Sub(r.t0).Round(time.Second), impl.token, v, named, fresh, r.roleRow, life, len(finishedLine), envoy.state, clockAt(r.lastSeenOf(session)))
	finished := "not finished"
	if life != nil && !life.finished.IsZero() && len(finishedLine) == 1 {
		finished = fmt.Sprintf("finished undelivered at %s, %s after the session's last registration at %s", clockAt(life.finished),
			life.finished.Sub(r.lastSeenOf(session)).Round(time.Second), clockAt(r.lastSeenOf(session)))
	}
	r.fails("unreachable-mark", "at 22 minutes read_state shows the implementer's claim %s with no mark (%s), %d notices name it, fresh_session refuses it %s (%d), and the role message is %s",
		v.state, map[bool]string{true: "marked", false: "no mark"}[v.marked()], named, fresh.code, fresh.status, finished)
}

// ---- after the first block -----------------------------------------------------------------------

// afterTheFirstBlock runs the check's next steps, each on claims of its own and so beside each
// other: the implementer blocked again, the tenth, the eighth's relaunch, the fifth's relaunch, the
// ninth's 85 minutes, the pods, and the sixth's three runs. Each flow records what it observed, with
// its times, as it observes it; this step reads each clause once every flow has ended.
func (r *w5Run) afterTheFirstBlock() {
	if r.t0.IsZero() {
		r.fails("after-first-block", "the first block did not run")
	}
	r.unblock("architect")
	flows := &w5Flows{}
	var wg sync.WaitGroup
	run := func(name string, flow func(*w5Flows)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					flows.add("flow-"+name, false, "the flow ended early: %v", p)
				}
			}()
			flow(flows)
		}()
	}
	tenthDone := make(chan struct{})
	podsDone := make(chan struct{})
	run("implementer-again", r.implementerAgain)
	run("tenth", func(f *w5Flows) { defer close(tenthDone); r.tenthFlow(f) })
	run("eighth", r.eighthFlow)
	run("fifth", r.fifthFlow)
	run("ninth", r.ninthFlow)
	// The pod of two holds the tenth and the sixth: the pods wait for the tenth's flow, and the
	// sixth's three runs for the pods.
	run("pods", func(f *w5Flows) {
		<-tenthDone
		defer close(podsDone)
		r.podsFlow(f)
	})
	run("sixth", func(f *w5Flows) { <-podsDone; r.sixthFlow(f) })
	wg.Wait()
	for _, clause := range flows.list() {
		r.check(clause.name, clause.ok, "%s", clause.detail)
	}
}

// w5Flows is the clauses the flows of afterTheFirstBlock observed, read by the step once they end.
type w5Flows struct {
	mu      sync.Mutex
	clauses []w5Clause
}

type w5Clause struct {
	name   string
	ok     bool
	detail string
}

func (f *w5Flows) add(name string, ok bool, format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clauses = append(f.clauses, w5Clause{name: name, ok: ok, detail: fmt.Sprintf(format, args...)})
}

func (f *w5Flows) list() []w5Clause {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]w5Clause{}, f.clauses...)
}

// observe records a clause's observation at once and keeps it for the step.
func (r *w5Run) observe(f *w5Flows, name string, ok bool, format string, args ...any) {
	word := "PASS"
	if !ok {
		word = "FAIL"
	}
	detail := fmt.Sprintf(format, args...)
	r.record("OBSERVED %s %s: %s", word, name, detail)
	f.add(name, ok, "%s", detail)
}

// waitFor polls want every 5 s until it holds or within passes.
func waitFor(within time.Duration, want func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if want() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// implementerAgain blocks the resumed implementer's registrations again once it is back in a wait
// call, which the rig freezes, and reads it 25 minutes after that block: out of reach a second time
// within 6 hours of its relaunch, it is not relaunched, R6's line names it once with that reason,
// no launch failure is charged, and the claim stays marked.
func (r *w5Run) implementerAgain(f *w5Flows) {
	impl := r.c("implementer")
	served, ok := r.nextOpenWait(impl.token, r.implAgainWait, 10*time.Minute)
	if !ok {
		r.observe(f, "implementer-back-in-a-wait", false, "the resumed implementer opened no wait call within 10 minutes")
		return
	}
	r.record("the resumed implementer is in a wait call answered at %s", clockAt(served))
	pid := r.stopProcess(impl.token)
	r.record("the resumed implementer (Oh My Pi pid %d) stopped inside its wait call at %s", pid, clockAt(time.Now()))
	before := r.held(impl.token)
	t1 := r.block("implementer")
	sleepUntil(t1.Add(w5GuardRead))
	after := r.snapshot()
	held := r.held(impl.token)
	calls := r.pods.relaunchCalls(impl.token, t1)
	lines := r.claimLines(w5CaseClaimGone, impl.token, t1)
	var described []string
	for _, line := range lines {
		described = append(described, w5Describe(line))
	}
	r.observe(f, "second-out-of-reach-within-6h-not-relaunched", len(calls) == 0 && held.Generation == before.Generation,
		"the implementer's relaunch calls from its second block at %s: %v; generation before %d, at 25 minutes %d", clockAt(t1), describeCalls(calls), before.Generation, held.Generation)
	r.observe(f, "second-out-of-reach-r6-line-once", len(lines) == 1 && strings.Contains(fmt.Sprint(lines[0].Attrs["reason"]), w5SecondTime),
		"%d %s lines name %s from %s: %v", len(lines), w5CaseClaimGone, impl.token, clockAt(t1), described)
	r.observe(f, "second-out-of-reach-charges-no-launch-failure", held.Budgets.LaunchFailures == before.Budgets.LaunchFailures,
		"launch failures before %d, after %d", before.Budgets.LaunchFailures, held.Budgets.LaunchFailures)
	r.observe(f, "second-out-of-reach-claim-stays-marked", after.views[impl.token].marked(), "read_state: %s", after.views[impl.token])
}

// tenthFlow blocks the tenth claim's registrations while it owes no task, kills its process so the
// daemon's own supervision relaunches it, and reads its marks, its launches, its charges and the
// daemon's own relaunch of it for 25 minutes: it is marked once, stays marked, is charged nothing,
// and the daemon relaunches it at most once, at or after its mark.
func (r *w5Run) tenthFlow(f *w5Flows) {
	tenth := r.c("tenth")
	time.Sleep(2 * time.Minute)
	start := r.block("tenth")
	session := r.session(tenth.token)
	// Its last registration before the block, read before its process ends: a session whose
	// process exits leaves the listener's list at once.
	seen := r.registeredLast(session)
	r.record("tenth: its last registration as the listener's own list shows it at the block: %s", clockAt(seen))
	launch := r.runtime.current(tenth.token)
	launchesBefore := len(r.runtime.launchesOf(tenth.token))
	_ = killGroup(launch.pane.pid)
	r.record("tenth: killed its process group (pid %d, generation %d) at %s", launch.pane.pid, launch.spec.Generation, clockAt(time.Now()))
	var series []string
	maxDeaths := 0
	var markedAt time.Time
	unmarkedAfterMark := false
	end := start.Add(w5ReadFor)
	for time.Now().Before(end) {
		v := r.snapshot().views[tenth.token]
		series = append(series, fmt.Sprintf("%s %s", clockAt(time.Now()), v))
		maxDeaths = max(maxDeaths, v.deaths)
		if v.marked() && markedAt.IsZero() {
			markedAt = time.Now()
		}
		if !markedAt.IsZero() && !v.marked() {
			unmarkedAfterMark = true
		}
		time.Sleep(30 * time.Second)
	}
	final := r.snapshot().views[tenth.token]
	relaunches := len(r.runtime.launchesOf(tenth.token)) - launchesBefore
	mark := r.markOf(tenth.token, start)
	r.recordEmptyReads("tenth", start)
	r.record("tenth: the listener's list read %s as its last registration at the end of the 25 minutes", clockAt(r.lastSeenOf(session)))
	r.record("tenth: %d launches by the daemon's supervision in %s; its readings: %s", relaunches, w5ReadFor, strings.Join(series, "; "))
	r.observe(f, "tenth-relaunch-count", relaunches > 0, "the daemon's supervision launched the tenth %d times in the 25 minutes", relaunches)
	r.observe(f, "tenth-nets-no-charge", maxDeaths == 0 && final.launchFailures == 0 && final.state != string(supervise.StateFailed),
		"deaths at most %d; at the end %s", maxDeaths, final)
	if mark == nil {
		r.observe(f, "tenth-marked-20m-after-last-registration", false, "no out-of-reach line for the tenth")
	} else {
		r.observe(f, "tenth-marked-20m-after-last-registration", markedInTime(mark.at, seen),
			"marked at %s, %s after its last registration the rig read, %s, and %s after the block at %s (the daemon logged since %v)",
			clockAt(mark.at), mark.at.Sub(seen).Round(time.Second), clockAt(seen), mark.at.Sub(start).Round(time.Second), clockAt(start), mark.Attrs["since"])
	}
	r.observe(f, "tenth-stays-marked-through-relaunches", !markedAt.IsZero() && !unmarkedAfterMark, "first read marked at %s; unmarked again after: %t", clockAt(markedAt), unmarkedAfterMark)
	stops := r.relaunchStops(tenth.token, start)
	r.observe(f, "tenth-relaunched-by-the-daemon-once-after-its-mark", len(stops) == 1 && mark != nil && !stops[0].at.Before(mark.at),
		"the daemon's relaunch stops of the tenth from the block: %v; its mark %s", describeCalls(stops), describeLine(mark))
	r.unblock("tenth")
	waitFor(5*time.Minute, func() bool {
		v := r.snapshot().views[tenth.token]
		return !v.marked() && (v.state == string(supervise.StateIdle) || v.state == string(supervise.StateReady))
	})
	r.record("tenth after its registrations go through again: %s", r.snapshot().views[tenth.token])
}

// eighthFlow lets the eighth's subagent end and puts the claim back in a wait, which the rig
// freezes, with its registrations still blocked and its task owed: the daemon's guard lets its
// relaunch through once 20 minutes pass with no step, and each relaunched process exits at its
// role claim and is charged a death, so it fails at the deaths limit with today's failure notice to
// its architect, and stays marked until then.
func (r *w5Run) eighthFlow(f *w5Flows) {
	eighth := r.c("eighth")
	time.Sleep(3 * time.Minute)
	r.eighthSubagent.let()
	r.thaw(eighth.token)
	ended := waitFor(5*time.Minute, func() bool {
		background, at := r.activityOf(eighth.token)
		return !at.IsZero() && at.After(time.Now().Add(-time.Minute)) && !background
	})
	r.observe(f, "eighth-subagent-ends", ended, "the eighth's activity report shows no background work: %t", ended)
	// The guard held it for its subagent: no relaunch from the block until its session's first
	// activity report that shows no background work running, read from the reports the front passed.
	quiet := r.api.firstQuietFrom(eighth.token, r.t0)
	held := r.relaunchStops(eighth.token, r.t0)
	r.observe(f, "eighth-held-until-its-subagent-ends", !quiet.IsZero() && len(held) == 0,
		"the eighth's first activity report from the block showing no background work reached the daemon at %s; the daemon's relaunch stops of it from the block until then: %v", clockAt(quiet), describeCalls(held))
	// It stays in the wait call of its loop that is open once its subagent has ended.
	waitAt, ok := r.nextOpenWait(eighth.token, r.eighthWait, 10*time.Minute)
	if !ok {
		r.observe(f, "eighth-back-in-a-wait", false, "no wait call open within 10 minutes of its subagent's end")
		return
	}
	r.record("the eighth is in a wait call answered at %s", clockAt(waitAt))
	// Each relaunched process's task turn is open when the process exits at its role claim, as a
	// real agent's is a second into its turn, its model not having answered. The stand-in answers a
	// turn at once, so the turn had ended at each exit and the claim owed no task at it (main5:
	// gen 3's turn ended at 23:12:20.309Z, its process exited at 23:12:20.827Z, no death); its turns
	// are held from here on, each until its process ends, and the daemon may relaunch it at any
	// sweep from here.
	pid := r.stopProcess(eighth.token)
	eighth.stand.dropRules()
	eighth.stand.holdEvery(anyTurn)
	r.record("the eighth (Oh My Pi pid %d) stopped inside its wait call", pid)
	backInWait := time.Now()
	before := r.held(eighth.token)
	r.observe(f, "eighth-back-in-a-wait", true, "stopped inside its wait at %s, its subagent ended", clockAt(backInWait))
	var stop podRelaunchCall
	if !waitFor(45*time.Minute, func() bool {
		stops := r.relaunchStops(eighth.token, backInWait)
		if len(stops) > 0 {
			stop = stops[0]
		}
		return len(stops) > 0
	}) {
		r.observe(f, "eighth-relaunched-once-its-guard-lets-it-through", false, "the daemon made no relaunch of the eighth within 45 minutes of %s", clockAt(backInWait))
		return
	}
	r.observe(f, "eighth-relaunched-once-its-guard-lets-it-through", !quiet.IsZero() && stop.at.After(quiet),
		"the daemon stopped its process for a relaunch at %s, %s after it was frozen in its wait and after its report showing no background work at %s",
		clockAt(stop.at), stop.at.Sub(backInWait).Round(time.Second), clockAt(quiet))
	var series []string
	failed := waitFor(40*time.Minute, func() bool {
		v := r.snapshot().views[eighth.token]
		series = append(series, fmt.Sprintf("%s %s", clockAt(time.Now()), v))
		return v.state == string(supervise.StateFailed)
	})
	final := r.held(eighth.token)
	r.record("eighth after its relaunch: %s", strings.Join(series, "; "))
	r.observe(f, "eighth-charged-a-death-at-each-exit-fails-at-the-limit", failed && final.Budgets.Deaths > before.Budgets.Deaths,
		"failed: %t; deaths before %d, at the end %d (state %s)", failed, before.Budgets.Deaths, final.Budgets.Deaths, final.State)
	markedThroughout := true
	for _, line := range series {
		if !strings.Contains(line, "unreachable since") && !strings.Contains(line, string(supervise.StateFailed)) {
			markedThroughout = false
		}
	}
	r.observe(f, "eighth-stays-marked-until-it-fails", markedThroughout, "every reading until it failed is marked: %t", markedThroughout)
	// The daemon writes the failure notice in the transaction that fails the claim: a notice row of
	// the eighth's issue, its payload's kind worker-died with the failure's cause and counts. The
	// issue is the row's own column; the payload names neither the issue nor the claim's token.
	var failureRows []string
	waitFor(2*time.Minute, func() bool {
		failureRows = failureRows[:0]
		for _, life := range r.rowLives("notice") {
			var payload struct {
				Kind    string          `json:"kind"`
				Failure json.RawMessage `json:"failure"`
			}
			_ = json.Unmarshal([]byte(life.payload), &payload)
			if life.issue == eighth.key && payload.Kind == "worker-died" && len(payload.Failure) > 0 && life.written.After(stop.at) {
				failureRows = append(failureRows, fmt.Sprintf("row %d written %s finished %s: %s", life.id, clockAt(life.written), clockAt(life.finished), truncate(life.payload, 300)))
			}
		}
		return len(failureRows) > 0
	})
	sort.Strings(failureRows)
	for _, row := range failureRows {
		r.record("eighth: failure notice to its architect, %s", row)
	}
	r.observe(f, "eighth-failure-notice-to-its-architect", len(failureRows) > 0, "%d worker-died notice rows of the eighth's issue %s after its relaunch", len(failureRows), eighth.key)
}

// reachRow is token's row as the store holds it now: its reach column, its state and generation,
// and when the row was last written.
func (r *w5Run) reachRow(token claim.Token) string {
	var reach, state string
	var generation int64
	var updated time.Time
	err := r.st.Pool().QueryRow(context.Background(), "select coalesce(reach::text, ''), state, generation, updated_at from claims where token = $1", string(token)).
		Scan(&reach, &state, &generation, &updated)
	if err != nil {
		return "unread: " + err.Error()
	}
	return fmt.Sprintf("%s gen %d, written %s, reach %s", state, generation, clockAt(updated), reach)
}

// activityOf is token's last activity report as the store holds it: whether it showed background
// work, and when it was made.
func (r *w5Run) activityOf(token claim.Token) (bool, time.Time) {
	var background bool
	var at *time.Time
	err := r.st.Pool().QueryRow(context.Background(), "select coalesce(activity_background, false), activity_at from claims where token = $1", string(token)).Scan(&background, &at)
	if err != nil || at == nil {
		return false, time.Time{}
	}
	return background, *at
}

// fifthFlow waits for the daemon's relaunch of the fifth, which its guard lets through once the
// fifth's job has ended and it waits on its named service, with nothing in its activity report:
// reached with no charge. The resumed session's turn on its owed task then moves the issue's phase
// on, its registrations still blocked, and the role message the outbox held for it is finished
// undelivered then, not before. Its registrations then go through again.
func (r *w5Run) fifthFlow(f *w5Flows) {
	fifth := r.c("fifth")
	defer r.unblock("fifth")
	before := r.held(fifth.token)
	var stop podRelaunchCall
	if !waitFor(50*time.Minute, func() bool {
		stops := r.relaunchStops(fifth.token, r.t0)
		if len(stops) > 0 {
			stop = stops[0]
		}
		return len(stops) > 0
	}) {
		r.observe(f, "fifth-relaunched-with-no-charge-once-its-job-ends", false, "the daemon made no relaunch of the fifth within 50 minutes")
		return
	}
	time.Sleep(time.Minute)
	after := r.held(fifth.token)
	quiet := r.api.firstQuietFrom(fifth.token, r.t0)
	r.observe(f, "fifth-relaunched-with-no-charge-once-its-job-ends", !quiet.IsZero() && stop.at.After(quiet) && stop.at.After(r.fifthJobEnded) && after.Generation > before.Generation &&
		after.Budgets.LaunchFailures == before.Budgets.LaunchFailures && after.Budgets.Deaths == before.Budgets.Deaths,
		"its job ended at %s and its first activity report from the block showing no background work reached the daemon at %s; the daemon stopped its process for a relaunch at %s; generation before %d, a minute after %d; budgets before %+v, a minute after %+v",
		clockAt(r.fifthJobEnded), clockAt(quiet), clockAt(stop.at), before.Generation, after.Generation, before.Budgets, after.Budgets)
	select {
	case at := <-r.fifthMoved:
		r.record("the resumed fifth's turn called request_backward_move at %s", clockAt(at))
	case <-time.After(10 * time.Minute):
		r.observe(f, "fifth-message-finished-at-phase-move", false, "the resumed fifth's turn did not move its phase within 10 minutes of its relaunch")
		return
	}
	// The moment the phase moved is the daemon's own line of the move, not the first read of
	// read_state that shows it: main5 took the move of 23:08:18.140Z for the read at 23:09:16.396Z,
	// which put the row's finish at 23:08:38.845Z before it. The line is written once the move has
	// committed (engine.logOnCommit), so a finish the outbox made between the commit and the line
	// reads as up to a second before it.
	var movedAt time.Time
	waitFor(3*time.Minute, func() bool {
		moves := r.logs(func(rec loggedRecord) bool {
			return rec.Message == w5PhaseChanged && fmt.Sprint(rec.Attrs["issue"]) == fifth.key &&
				fmt.Sprint(rec.Attrs["from"]) == string(phase.Reviewing) && !rec.at.Before(stop.at)
		})
		if len(moves) == 0 {
			return false
		}
		movedAt = moves[0].at
		r.record("the daemon moved the fifth's issue %s: %s", fifth.key, w5Describe(moves[0]))
		return true
	})
	time.Sleep(time.Minute)
	issues, _ := r.snapshot().state["issues"].(map[string]any)
	issue, _ := issues[fifth.key].(map[string]any)
	r.record("the fifth's issue %s stands at %v in read_state at %s", fifth.key, issue["phase"], clockAt(time.Now()))
	life := r.rowLives()[r.fifthMessageRow]
	lines := r.logs(func(rec loggedRecord) bool {
		return rec.Message == w5HeldFinishPhase && fmt.Sprint(rec.Attrs["row"]) == fmt.Sprint(r.fifthMessageRow)
	})
	r.observe(f, "fifth-message-finished-at-phase-move", !movedAt.IsZero() && life != nil && !life.finished.IsZero() && !life.finished.Before(movedAt.Add(-time.Second)) && len(lines) == 1,
		"the fifth's row %d finished %s; its phase moved at %s; finished-at-phase-move lines %d", r.fifthMessageRow, clockAt(life.finishedOrZero()), clockAt(movedAt), len(lines))
}

// ninthFlow waits for R6's line that names the ninth, 60 minutes after the daemon's relaunch guard
// first held its relaunch, then lets its registrations through again at 85 minutes, and reads that
// the line was written once.
func (r *w5Run) ninthFlow(f *w5Flows) {
	ninth := r.c("ninth")
	var lines []loggedRecord
	waitFor(time.Until(r.t0.Add(w5NinthBlocked)), func() bool {
		lines = r.claimLines(w5CaseClaimRefused, ninth.token, r.t0)
		return len(lines) > 0
	})
	sleepUntil(r.t0.Add(w5NinthBlocked))
	lines = r.claimLines(w5CaseClaimRefused, ninth.token, r.t0)
	r.unblock("ninth")
	if len(lines) == 0 {
		r.observe(f, "ninth-r6-line-at-60m", false, "no %s line names the ninth in the 85 minutes of its block; the guard first held it at %s", w5CaseClaimRefused, clockAt(r.ninthHeld))
		return
	}
	gap := lines[0].at.Sub(r.ninthHeld)
	r.observe(f, "ninth-r6-line-at-60m", !r.ninthHeld.IsZero() && len(lines) == 1 && gap >= w5GuardHeld && gap <= w5GuardHeld+w5LineSlack,
		"%d %s lines name the ninth; the first %s, %s after the guard first held it at %s", len(lines), w5CaseClaimRefused, w5Describe(lines[0]), gap.Round(time.Second), clockAt(r.ninthHeld))
}

// podsFlow blocks every claim of one pod of two at once (the sixth and the tenth): once read_state
// shows both unreachable, the daemon relaunches neither, since a relaunch into a pod that reaches
// Envoy for none of its claims recovers none, writes R6's line for each, naming the pod, and stops
// nothing.
func (r *w5Run) podsFlow(f *w5Flows) {
	sixth, tenth := r.c("sixth"), r.c("tenth")
	waitFor(5*time.Minute, func() bool { return !r.snapshot().views[tenth.token].marked() })
	start := r.block("sixth", "tenth")
	if !waitFor(30*time.Minute, func() bool {
		snap := r.snapshot()
		return snap.views[sixth.token].marked() && snap.views[tenth.token].marked()
	}) {
		r.observe(f, "pod-claims-read-unreachable", false, "the two claims were not both marked within 30 minutes of %s", clockAt(start))
		r.unblock("sixth", "tenth")
		return
	}
	sixthLaunch, tenthLaunch := r.runtime.current(sixth.token), r.runtime.current(tenth.token)
	// The sweeps that judge both marks, and the line of each.
	time.Sleep(2 * time.Minute)
	snap := r.snapshot()
	r.record("pods: %s %s, %s %s", sixth.token, snap.views[sixth.token], tenth.token, snap.views[tenth.token])
	// The tenth was relaunched by the daemon in its own flow, less than 6 hours before, so its line
	// is the second-time line alone, since the daemon reads the 6 hours before the pod; the sixth's
	// names the pod.
	for _, c := range []*w5Claim{sixth, tenth} {
		word, want := w5CaseClaimRefused, w5PodReason
		if c.name == "tenth" {
			word, want = w5CaseClaimGone, w5SecondTime
		}
		lines := r.claimLines(word, c.token, start)
		other := r.claimLines(map[string]string{w5CaseClaimRefused: w5CaseClaimGone, w5CaseClaimGone: w5CaseClaimRefused}[word], c.token, start)
		reason := ""
		if len(lines) > 0 {
			reason = reasonOf(&lines[0])
		}
		named := strings.Contains(reason, want)
		if c.name == "sixth" {
			named = named && strings.Contains(reason, snap.views[c.token].pod)
		}
		r.observe(f, "pod-of-two-r6-line-"+c.name, len(lines) == 1 && len(other) == 0 && named,
			"%d %s lines name %s and %d of the other case; the reason: %q, want it to name %q", len(lines), word, c.token, len(other), reason, want)
	}
	sixthCalls, tenthCalls := r.pods.relaunchCalls(sixth.token, start), r.pods.relaunchCalls(tenth.token, start)
	nothingStopped := sixthLaunch.running() && tenthLaunch.running() && r.runtime.current(sixth.token) == sixthLaunch &&
		r.runtime.current(tenth.token) == tenthLaunch && len(sixthCalls) == 0 && len(tenthCalls) == 0
	r.observe(f, "pod-of-two-nothing-relaunched-or-stopped", nothingStopped, "the sixth's process (pid %d) runs: %t; the tenth's (pid %d): %t; the runtime was asked of the sixth %v and of the tenth %v",
		sixthLaunch.pane.pid, sixthLaunch.running(), tenthLaunch.pane.pid, tenthLaunch.running(), describeCalls(sixthCalls), describeCalls(tenthCalls))
	r.unblock("sixth", "tenth")
	waitFor(10*time.Minute, func() bool {
		snap := r.snapshot()
		return !snap.views[sixth.token].marked() && !snap.views[tenth.token].marked()
	})
	r.record("pods after: %s", describeSnapshot(r.snapshot(), r.names))
}

// sixthFlow blocks the sixth claim's registrations three times, the rig's runtime reporting its
// launcher not connected, then that the launch would replace the pod, then answering the stop
// while the process outlives it: each time, once read_state shows it unreachable, the daemon's
// relaunch of it is refused with its reason, which R6's line names once, in the first two before
// anything is stopped and in the third after the stop, and no other claim of the pod is touched.
// The launcher's state is put back only once the claim reads reachable again, so no sweep relaunches
// it in between.
func (r *w5Run) sixthFlow(f *w5Flows) {
	sixth, tenth := r.c("sixth"), r.c("tenth")
	runs := []struct {
		state, reason string
		afterStop     bool
	}{
		{w5LauncherNotConnected, "no launcher of it in pod", false},
		{w5LauncherReplacesPod, "its launch needs a new pod", false},
		{w5LauncherOutlivesStop, "the process still runs", true},
	}
	for i, run := range runs {
		label := fmt.Sprintf("sixth-run-%d-%s", i+1, run.state)
		r.pods.setLauncher(sixth.token, run.state)
		launch, mate := r.runtime.current(sixth.token), r.runtime.current(tenth.token)
		start := r.block("sixth")
		if !waitFor(30*time.Minute, func() bool { return r.snapshot().views[sixth.token].marked() }) {
			r.observe(f, label, false, "the sixth was not marked within 30 minutes of %s", clockAt(start))
			r.unblock("sixth")
			waitFor(10*time.Minute, func() bool { return !r.snapshot().views[sixth.token].marked() })
			r.pods.setLauncher(sixth.token, "")
			continue
		}
		var lines []loggedRecord
		waitFor(3*time.Minute, func() bool {
			lines = r.claimLines(w5CaseClaimRefused, sixth.token, start)
			return len(lines) > 0
		})
		time.Sleep(2 * time.Minute)
		lines = r.claimLines(w5CaseClaimRefused, sixth.token, start)
		calls := r.pods.relaunchCalls(sixth.token, start)
		stopped := false
		for _, c := range calls {
			if c.call == "StopForRelaunch" {
				stopped = true
			}
		}
		reason := ""
		if len(lines) > 0 {
			reason = reasonOf(&lines[0])
		}
		r.record("%s: the runtime's relaunch calls: %v", label, describeCalls(calls))
		r.observe(f, label+"-refused-with-its-reason", len(lines) == 1 && strings.Contains(reason, run.reason),
			"%d %s lines name the sixth; the reason: %q; want it to name %q", len(lines), w5CaseClaimRefused, reason, run.reason)
		r.observe(f, label+"-refused-before-or-after-the-stop", len(calls) > 0 && stopped == run.afterStop && launch.running() && r.runtime.current(sixth.token) == launch,
			"the stop was asked: %t (want %t); the process (pid %d) still runs: %t", stopped, run.afterStop, launch.pane.pid, launch.running())
		r.observe(f, label+"-no-other-claim-of-the-pod-touched", mate.running() && r.runtime.current(tenth.token) == mate && len(r.pods.relaunchCalls(tenth.token, start)) == 0,
			"the tenth's process (pid %d) runs: %t, unchanged: %t", mate.pane.pid, mate.running(), r.runtime.current(tenth.token) == mate)
		r.unblock("sixth")
		waitFor(10*time.Minute, func() bool { return !r.snapshot().views[sixth.token].marked() })
		r.pods.setLauncher(sixth.token, "")
	}
}

// ---- the daemon's restart ------------------------------------------------------------------------

// daemonRestart restarts the daemon and reads the marks and the notices for 25 minutes: a claim
// marked before the restart gets no new notice, no new R6 line and no relaunch after it. The
// implementer, blocked a second time and not relaunched, must be among the claims marked before
// it, so the checks after it read at least that claim rather than an empty set.
func (r *w5Run) daemonRestart() {
	snap := r.answeredSnapshot("restart, before it")
	var reported []claim.Token
	for token, v := range snap.views {
		if v.marked() {
			reported = append(reported, token)
		}
	}
	sort.Slice(reported, func(i, j int) bool { return reported[i] < reported[j] })
	r.record("restart: claims marked before the restart: %v", reported)
	r.check("restart-reads-the-implementers-mark-before-it", r.covers(snap) && slices.Contains(reported, r.c("implementer").token),
		"read_state before the restart answered: %t (%s); marked %v", snap.err == "", snap.err, reported)
	stopped, serving := r.restartDaemon()
	r.record("restart: the daemon stopped at %s and served again at %s", clockAt(stopped), clockAt(serving))
	end := serving.Add(w5ReadFor)
	for time.Now().Before(end) {
		time.Sleep(time.Minute)
		r.record("restart: %s", describeSnapshot(r.snapshot(), r.names))
	}
	// The controller's notices and the architect's, each naming a claim by its token.
	notices := append(r.controllerNoticesW5(), r.roleNotices(r.c("architect").token)...)
	var renewed []string
	for _, n := range notices {
		if n.at.Before(stopped) {
			continue
		}
		for _, token := range reported {
			if n.claim == string(token) || containsString(n.rows, string(token)) {
				renewed = append(renewed, n.String())
			}
		}
	}
	r.check("after-restart-no-new-notice-for-a-claim-marked-before", len(renewed) == 0, "notices to the controller and the architect after the restart at %s naming a claim marked before it: %v", clockAt(stopped), renewed)
	var again []string
	for _, token := range reported {
		for _, word := range []string{w5CaseClaimGone, w5CaseClaimRefused} {
			for _, line := range r.claimLines(word, token, stopped) {
				again = append(again, w5Describe(line))
			}
		}
		for _, call := range describeCalls(r.pods.relaunchCalls(token, stopped)) {
			again = append(again, string(token)+" "+call)
		}
	}
	r.check("after-restart-no-new-line-or-relaunch-for-a-claim-marked-before", len(again) == 0, "R6 lines and relaunch calls after the restart at %s naming a claim marked before it: %v", clockAt(stopped), again)
}

// ---- every session blocked -----------------------------------------------------------------------

// everySessionBlocked lets every blocked session register again, stops the registrations of every
// session the listener holds, the controller's included, for 30 minutes, lets them register again
// one at a time over 2 minutes, then makes the list fail for 25 minutes and answer again.
func (r *w5Run) everySessionBlocked() {
	for _, c := range r.ordered {
		if pid, ok := r.ompPid(c.token); ok && processState(pid) == "T" {
			r.thaw(c.token)
		}
	}
	r.unblock(r.blockedNames()...)
	unmarked := waitFor(10*time.Minute, func() bool {
		snap := r.answeredSnapshot("every session blocked, before the block")
		if snap.err != "" {
			return false
		}
		for _, v := range snap.views {
			if v.marked() {
				return false
			}
		}
		return true
	})
	r.check("precondition-no-claim-marked-before-every-session-is-blocked", unmarked, "no claim marked within 10 minutes of every block's end: %t", unmarked)
	if !unmarked {
		// Where the mark that stayed is read from: each marked claim's reach column as the store
		// holds it, beside what read_state and the operator's listing show of it.
		snap := r.answeredSnapshot("every session blocked, a mark that stayed")
		for token, v := range snap.views {
			if v.marked() {
				r.record("a mark that stayed: %s %s; the store's row: %s", r.names[token], v, r.reachRow(token))
			}
		}
	}
	r.record("every session blocked: before the block %s", describeSnapshot(r.answeredSnapshot("every session blocked, before the block"), r.names))
	launchesBefore := len(r.runtime.Launches())
	sessions := r.envoySessions()
	var all []string
	for session := range sessions {
		all = append(all, session)
		r.front.blockRegistrations(session)
	}
	sort.Strings(all)
	blockedAt := time.Now()
	r.record("blocked every session the listener holds at %s: %v", clockAt(blockedAt), all)
	markedDuring := []string{}
	unread := 0
	for time.Now().Before(blockedAt.Add(w5AllBlocked)) {
		time.Sleep(time.Minute)
		snap := r.snapshot()
		r.record("all blocked: %s", describeSnapshot(snap, r.names))
		if !r.covers(snap) {
			unread++
		}
		for token, v := range snap.views {
			if v.marked() {
				markedDuring = append(markedDuring, string(token))
			}
		}
		if snap.controllerMark() != "" {
			markedDuring = append(markedDuring, "controller")
		}
	}
	r.check("every-session-blocked-none-marked", len(markedDuring) == 0 && unread == 0, "marked while every session was blocked: %v; minutes whose read did not answer with the run's claims: %d", markedDuring, unread)
	r.recordEmptyReads("every session blocked", blockedAt)
	// The empty list is R6's line alone: the controller is told nothing of it, and the line is
	// written once, 20 minutes or more after the block.
	noLive := []w5Notice{}
	for _, n := range r.controllerNoticesW5() {
		if n.kind == w5NoticeNoLive && !n.at.Before(blockedAt) {
			noLive = append(noLive, n)
		}
	}
	r.check("controller-told-nothing-of-the-empty-list", len(noLive) == 0, "envoy-no-live-session notices to the controller: %v", noLive)
	lines := r.logs(func(rec loggedRecord) bool {
		return strings.HasPrefix(rec.Message, w5FleetAlertPrefix) && fmt.Sprint(rec.Attrs["case"]) == w5CaseNoLive && !rec.at.Before(blockedAt)
	})
	r.check("empty-list-r6-line-once-after-20m", len(lines) == 1 && lines[0].at.Sub(blockedAt) >= w5Unreachable,
		"%d %s lines from the block at %s; the first %s", len(lines), w5CaseNoLive, clockAt(blockedAt), describeLine(firstOf(lines)))
	// They register again one at a time over 2 minutes.
	step := w5RegisterOver / time.Duration(max(1, len(all)))
	for _, session := range all {
		r.front.unblock(session)
		r.record("let %s register again at %s", session, clockAt(time.Now()))
		time.Sleep(step)
	}
	registered := time.Now()
	time.Sleep(3 * time.Minute)
	snap := r.answeredSnapshot("every session registering again")
	markedAfter := []string{}
	for token, v := range snap.views {
		if v.marked() {
			markedAfter = append(markedAfter, string(token))
		}
	}
	r.check("none-marked-or-relaunched-while-they-register-again", r.covers(snap) && len(markedAfter) == 0 && len(r.runtime.Launches()) == launchesBefore,
		"read answered with the run's claims: %t (%s); marked %v; launches since the block %d", r.covers(snap), snap.err, markedAfter, len(r.runtime.Launches())-launchesBefore)
	// The list fails for 25 minutes, then answers again.
	failFrom := time.Now()
	until := r.envoy.failFor(w5ListFailing)
	r.record("the list fails from %s until %s (registrations all went through at %s)", clockAt(failFrom), clockAt(until), clockAt(registered))
	markedFailing := []string{}
	unread = 0
	for time.Now().Before(until) {
		time.Sleep(time.Minute)
		snap := r.snapshot()
		if !r.covers(snap) {
			unread++
		}
		for token, v := range snap.views {
			if v.marked() {
				markedFailing = append(markedFailing, string(token))
			}
		}
	}
	r.check("failed-list-changes-no-mark", len(markedFailing) == 0 && unread == 0, "marked while the list failed: %v; minutes whose read did not answer with the run's claims: %d", markedFailing, unread)
	answered := time.Now()
	for time.Now().Before(answered.Add(w5MarkWindow)) {
		time.Sleep(time.Minute)
		r.record("list answers again: %s", describeSnapshot(r.snapshot(), r.names))
	}
	snap = r.answeredSnapshot("the list answering again")
	markedAfter = markedAfter[:0]
	for token, v := range snap.views {
		if v.marked() {
			markedAfter = append(markedAfter, string(token))
		}
	}
	r.check("none-marked-or-relaunched-after-the-list-answers-again", r.covers(snap) && len(markedAfter) == 0 && len(r.runtime.Launches()) == launchesBefore,
		"read answered with the run's claims: %t (%s); marked %v; launches since the block %d", r.covers(snap), snap.err, markedAfter, len(r.runtime.Launches())-launchesBefore)
	r.allBlockedAt, r.listFailedFrom, r.listAnswered = blockedAt, failFrom, answered
}

// covers is whether snap answered and holds a view of each claim of the run that still runs (the
// fourth's claim retired, and the eighth's failed), so a clause that finds no mark in it read
// those claims rather than nothing.
func (r *w5Run) covers(snap w5Snapshot) bool {
	if snap.err != "" {
		return false
	}
	for _, c := range r.ordered {
		if c.name == "fourth" || c.name == "eighth" {
			continue
		}
		if !snap.views[c.token].found {
			return false
		}
	}
	return true
}

// ---- totals --------------------------------------------------------------------------------------

// totals reads what holds over the whole run: no notice to the controller names a claim of the
// run, and R6's lines are exactly these, each starting with the fixed text and naming its case and
// project: one for the list with no live session, one for the 25 minutes of failed reads, the
// ninth's after 60 minutes of the guard's hold, the sixth's for the pod of two and the tenth's
// second-time line, since the daemon relaunched it in its own flow, one for each of the sixth's
// three runs, and the implementer's, out of reach a second time within 6 hours. Last, the control
// of every read of the controller's topic (checkControllerNoticesAreRead).
func (r *w5Run) totals() {
	for _, n := range r.controllerNoticesW5() {
		r.record("controller notice %s", n)
	}
	var names []string
	for _, c := range r.ordered {
		names = append(names, c.name)
	}
	r.checkNoControllerNotice(names, r.t0, "whole-run")
	alerts := r.fleetAlerts()
	var described []string
	got := map[string]int{}
	for _, rec := range alerts {
		described = append(described, w5Describe(rec))
		key := fmt.Sprint(rec.Attrs["case"])
		if rec.Attrs["claim"] != nil {
			key += " " + r.names[claim.Token(fmt.Sprint(rec.Attrs["claim"]))]
		}
		got[key]++
		r.check("r6-line-fixed-text-case-project-"+fmt.Sprint(rec.Attrs["case"]), strings.HasPrefix(rec.Message, w5FleetAlertPrefix) && rec.Attrs["case"] != nil && rec.Attrs["project"] == r.cfg.Project,
			"%s", w5Describe(rec))
	}
	r.record("R6 lines: %v", described)
	tenthCase := w5CaseClaimGone
	want := map[string]int{
		w5CaseNoLive:                     1,
		w5CaseListUnreadable:             1,
		w5CaseClaimRefused + " ninth":    1,
		tenthCase + " tenth":             1,
		w5CaseClaimRefused + " sixth":    4,
		w5CaseClaimGone + " implementer": 1,
	}
	keys := make([]string, 0, len(want))
	for key := range want {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r.check("r6-lines-"+strings.ReplaceAll(key, " ", "-"), got[key] == want[key], "%d lines of %s, want %d", got[key], key, want[key])
	}
	var other []string
	for key, n := range got {
		if _, expected := want[key]; !expected {
			other = append(other, fmt.Sprintf("%s x%d", key, n))
		}
	}
	sort.Strings(other)
	r.check("r6-no-other-line", len(other) == 0, "lines of no expected case and claim: %v (all: %v)", other, described)
	r.checkControllerNoticesAreRead()
}

// checkControllerNoticesAreRead is the control of every check that no notice to the controller
// names a claim of the run (checkNoControllerNotice, failed-read-sends-nothing,
// after-restart-no-new-notice-for-a-claim-marked-before, controller-told-nothing-of-the-empty-list):
// the rig queues a controller notice of its own whose reason names the implementer's claim, and the
// same read of the controller's topic must find it, by the same rule checkNoControllerNotice names
// a claim by. It runs after every such check, so its notice is in none of them.
func (r *w5Run) checkControllerNoticesAreRead() {
	impl := r.c("implementer")
	canary := "W5-CONTROLLER-CANARY-" + r.suffix
	row := r.enqueueRow(impl.key, record.ControllerNotice{Kind: record.TreeStalledNotice,
		Reason: canary + ": a controller notice the rig queues, naming " + string(impl.token) + ", so that the read of the controller's topic is seen to read one."})
	seen := waitFor(5*time.Minute, func() bool {
		for _, n := range r.controllerNoticesW5() {
			if strings.Contains(n.reason, canary) && strings.Contains(n.reason, string(impl.token)) {
				return true
			}
		}
		return false
	})
	r.check("the-controllers-notices-are-read", seen,
		"the rig's controller notice (row %d) naming %s was read on the controller's topic within 5 minutes: %t", row, impl.token, seen)
}

// ---- the fresh runs ------------------------------------------------------------------------------

// w5Fresh is one of the check's two fresh runs: the controller as a daemon claim and one worker
// claim that stays reachable, so Envoy's list holds a live session while the controller is blocked.
type w5Fresh struct {
	*w5Rig
	suffix string
	names  map[claim.Token]string
	files  string
}

func newW5Fresh(t *testing.T) *w5Fresh {
	t.Helper()
	suffix := randomSuffix(t)[:6]
	w := newW5Rig(t, w5Options{script: "scripts/e2e/unreachable-claims.sh", claims: []w5Claim{
		{name: "worker", key: "F1", tree: "F1", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5fresh",
			task: fmt.Sprintf("W5-FRESH-%s: the rig's task for this claim.", suffix)},
	}})
	f := &w5Fresh{w5Rig: w, suffix: suffix, names: map[claim.Token]string{}, files: t.TempDir()}
	f.names[w.claims["worker"].token] = "worker"
	f.names[w.ctl.token] = "controller"
	return f
}

func (f *w5Fresh) setup() {
	f.boot()
	f.startSampler(f.names)
	for _, token := range []claim.Token{f.claims["worker"].token, f.ctl.token} {
		f.await(string(token)+" live", w5PaneReady, func() bool {
			status, body := f.apiCall(http.MethodGet, "/legion/v1/operator/claims", nil, true, 30*time.Second)
			return status == http.StatusOK && strings.Contains(string(body), `"token":"`+string(token)+`"`) &&
				f.snapshot().views[token].state == string(supervise.StateIdle) && !f.snapshot().views[token].pending
		})
	}
	f.record("fresh run: %s", describeSnapshot(f.snapshot(), f.names))
}

// controllerSession is the controller's recorded session.
func (f *w5Fresh) controllerSession() string { return f.snapshot().views[f.ctl.token].session }

// lastStepOf is the latest of token's last step and its turn's start, as the store holds them.
func (f *w5Fresh) lastStepOf(token claim.Token) time.Time {
	var step, turn *time.Time
	if err := f.st.Pool().QueryRow(context.Background(), "select step_at, turn_started_at from claims where token = $1", string(token)).Scan(&step, &turn); err != nil {
		return time.Time{}
	}
	var last time.Time
	for _, at := range []*time.Time{step, turn} {
		if at != nil && at.After(last) {
			last = *at
		}
	}
	return last
}

func (f *w5Fresh) alertsOf(word string, from time.Time) []loggedRecord {
	var out []loggedRecord
	for _, rec := range f.fleetAlerts() {
		if fmt.Sprint(rec.Attrs["case"]) == word && !rec.at.Before(from) {
			out = append(out, rec)
		}
	}
	return out
}

// TestW5ControllerBackgroundJobLive is the check's first fresh run: the controller's registrations
// blocked while it runs a background job, for 25 minutes; the job ended; the block kept until 20
// minutes after the controller's last step that follows the job's end; and whether the daemon
// relaunches it. The controller that runs a background job is not relaunched while the job runs,
// and R6's line is written once, at its first refusal; 20 minutes after its last step that follows
// the job's end the daemon relaunches it with no charge.
func TestW5ControllerBackgroundJobLive(t *testing.T) {
	f := newW5Fresh(t)
	f.step("setup", f.setup)
	f.step("controller-blocked-while-it-runs-a-background-job", f.controllerWithAJob)
}

func (f *w5Fresh) controllerWithAJob() {
	ctl := f.ctl
	marker := "W5-CTL-JOB-" + f.suffix
	done := filepath.Join(f.files, "ctl-job.done")
	ctl.stand.script(promptCarrying(marker), 0, "bash", map[string]any{
		"command": fmt.Sprintf("while [ ! -f %s ]; do sleep 5; done; echo CTL-JOB-DONE", done), "async": true, "timeout": 3600,
	})
	if status, body := f.deliver(ctl.token, marker+": start the rig's background job and end your turn."); status != http.StatusOK {
		f.fails("deliver", "the operator's task to the controller = %d %s", status, body)
	}
	f.await("the controller's activity report to show its background job", 3*time.Minute, func() bool {
		var background bool
		var at *time.Time
		_ = f.st.Pool().QueryRow(context.Background(), "select coalesce(activity_background, false), activity_at from claims where token = $1", string(ctl.token)).Scan(&background, &at)
		return background && at != nil
	})
	f.await("the controller's turn to end", 3*time.Minute, func() bool {
		return f.snapshot().views[ctl.token].state == string(supervise.StateIdle)
	})
	launches := len(f.runtime.launchesOf(ctl.token))
	before := f.snapshot().views[ctl.token]
	session := f.controllerSession()
	t0 := time.Now()
	f.front.blockRegistrations(session)
	f.record("blocked the controller's registrations (session %s) at %s; it is %s with %d launches", session, clockAt(t0), before, launches)
	sleepUntil(t0.Add(w5GuardRead))
	refused := f.alertsOf(w5CaseControllerRefused, t0)
	mark := f.markOf(ctl.token, t0)
	f.check("controller-with-a-job-not-relaunched-while-it-runs", len(f.runtime.launchesOf(ctl.token)) == launches,
		"launches of the controller at the block %d, 25 minutes on %d", launches, len(f.runtime.launchesOf(ctl.token)))
	f.check("controller-with-a-job-r6-at-its-first-refusal", len(refused) == 1 && mark != nil && refused[0].at.Sub(mark.at) < 2*time.Minute,
		"R6 controller_relaunch_refused lines: %d (%s); the controller marked at %s", len(refused), w5FirstAt(refused), clockAtLine(mark))
	for _, rec := range refused {
		f.record("R6: %s", w5Describe(rec))
	}
	ended := time.Now()
	budgets := f.watchBudgets(ctl.token)
	writeProofFile(f.t, done, "done\n")
	f.record("the controller's job ended at %s", clockAt(ended))
	f.await("the controller's turn on its job's end to end", 5*time.Minute, func() bool {
		return f.lastStepOf(ctl.token).After(ended) && f.snapshot().views[ctl.token].state == string(supervise.StateIdle)
	})
	time.Sleep(liveSettle)
	last := f.lastStepOf(ctl.token)
	f.record("the controller's last step after its job's end: %s", clockAt(last))
	var relaunched time.Time
	waitFor(time.Until(last.Add(w5MarkWindow+3*time.Minute)), func() bool {
		if len(f.runtime.launchesOf(ctl.token)) > launches {
			relaunched = f.runtime.current(ctl.token).at
			return true
		}
		return false
	})
	time.Sleep(5 * time.Second)
	beforeLaunch, afterLaunch, moved := budgets.transition()
	after := f.claim(ctl.token)
	gap := relaunched.Sub(last)
	f.recordEmptyReads("controller with a job", t0)
	f.check("controller-relaunched-20m-after-its-last-step", !relaunched.IsZero() && gap >= w5Unreachable && gap <= w5MarkWindow+time.Minute,
		"the daemon relaunched the controller at %s, %s after its last step %s", clockAt(relaunched), gap.Round(time.Second), clockAt(last))
	// The relaunch's own charge is the budgets' move across its launch, read before the relaunched
	// controller's boot on the path still blocked can be charged; the daemon's line says the same.
	relaunchLines := f.logs(func(rec loggedRecord) bool {
		return rec.Attrs["claim"] == string(ctl.token) && !rec.at.Before(t0) && strings.HasPrefix(rec.Message, "supervise: relaunching a claim Envoy cannot reach")
	})
	var relaunchLine string
	if len(relaunchLines) > 0 {
		relaunchLine = w5Describe(relaunchLines[0])
	}
	f.check("controller-relaunch-charges-nothing", moved && chargedNothing(beforeLaunch, afterLaunch),
		"across the relaunch's launch: before %s, after %s (a later generation read: %t); the claim's budgets a few seconds on: %+v", beforeLaunch, afterLaunch, moved, after.Budgets)
	f.check("controller-relaunch-line-says-the-stop-charges-nothing", len(relaunchLines) == 1 && strings.Contains(relaunchLines[0].Message, "the stop charges nothing"),
		"the daemon's relaunch line: %s", relaunchLine)
	f.check("controller-with-a-job-r6-once", len(f.alertsOf(w5CaseControllerRefused, t0)) == 1, "R6 lines: %d", len(f.alertsOf(w5CaseControllerRefused, t0)))
	f.front.unblock(session)
	f.record("let the controller's registrations through again at %s", clockAt(time.Now()))
	waitFor(5*time.Minute, func() bool { return f.snapshot().views[ctl.token].state == string(supervise.StateIdle) })
	for _, rec := range f.fleetAlerts() {
		f.record("R6 line of the run: %s", w5Describe(rec))
	}
}

func clockAtLine(rec *loggedRecord) string {
	if rec == nil {
		return "never"
	}
	return clockAt(rec.at)
}

// TestW5ControllerLauncherLive is the check's second fresh run: the controller's registrations
// blocked with the rig's runtime reporting its launcher as not connected, for 25 minutes; how the
// daemon relaunches it; the relaunched controller's path to the listener kept blocked for 25
// minutes; then, in the same run, its registrations stopped for 25 minutes a second time within 6
// hours.
func TestW5ControllerLauncherLive(t *testing.T) {
	f := newW5Fresh(t)
	f.step("setup", f.setup)
	var relaunchedAt time.Time
	f.step("controller-whose-launcher-is-not-connected", func() { relaunchedAt = f.controllerLauncherGone() })
	f.step("controller-out-of-reach-a-second-time-within-6h", func() { f.controllerAgain(relaunchedAt) })
}

func (f *w5Fresh) controllerLauncherGone() time.Time {
	ctl := f.ctl
	f.pods.setLauncher(ctl.token, w5LauncherNotConnected)
	before := f.snapshot().views[ctl.token]
	launches := len(f.runtime.launchesOf(ctl.token))
	budgets := f.watchBudgets(ctl.token)
	session := before.session
	t0 := time.Now()
	f.front.blockRegistrations(session)
	f.record("blocked the controller's registrations (session %s) at %s with its launcher reported not connected; it is %s", session, clockAt(t0), before)
	var relaunched time.Time
	var newPod string
	waitFor(w5GuardRead, func() bool {
		if len(f.runtime.launchesOf(ctl.token)) > launches {
			launch := f.runtime.current(ctl.token)
			relaunched = launch.at
			if sandbox := f.pods.pod(launch.pane.locator).Sandbox; sandbox != nil {
				newPod = sandbox.PodUID
			}
			return true
		}
		return false
	})
	time.Sleep(5 * time.Second)
	beforeLaunch, afterLaunch, moved := budgets.transition()
	time.Sleep(time.Minute)
	held := f.claim(ctl.token)
	calls := f.pods.relaunchCalls(ctl.token, t0)
	var described []string
	for _, c := range calls {
		described = append(described, fmt.Sprintf("%s %s may=%t launcher=%q answered %q", clockAt(c.at), c.call, c.mayReplacePod, c.launcher, c.answer))
	}
	f.record("the controller's relaunch calls: %v", described)
	// The relaunch's own charge is the budgets' move across its launch, read before the relaunched
	// controller's boots that fail on its blocked path can be charged; the daemon's line says the
	// same, and the budgets a minute on are recorded beside it.
	relaunchLines := f.logs(func(rec loggedRecord) bool {
		return rec.Attrs["claim"] == string(ctl.token) && !rec.at.Before(t0) && strings.HasPrefix(rec.Message, "supervise: relaunching a claim Envoy cannot reach")
	})
	var relaunchLine string
	if len(relaunchLines) > 0 {
		relaunchLine = w5Describe(relaunchLines[0])
	}
	f.check("controller-relaunched-in-a-replaced-pod-of-its-own", !relaunched.IsZero() && newPod != "" && newPod != before.pod,
		"relaunched at %s; its pod was %s and its relaunch runs in %s", clockAt(relaunched), before.pod, newPod)
	f.check("controller-launcher-relaunch-charges-nothing", moved && chargedNothing(beforeLaunch, afterLaunch),
		"across the relaunch's launch: before %s, after %s (a later generation read: %t); a minute on: %+v (state %s)", beforeLaunch, afterLaunch, moved, held.Budgets, held.State)
	f.check("controller-launcher-relaunch-line-says-the-stop-charges-nothing", len(relaunchLines) == 1 && strings.Contains(relaunchLines[0].Message, "the stop charges nothing"),
		"the daemon's relaunch line: %s", relaunchLine)
	f.check("controller-launcher-relaunch-writes-no-r6", len(f.fleetAlerts()) == 0, "R6 lines so far: %d", len(f.fleetAlerts()))
	sleepUntil(relaunched.Add(w5ReadFor))
	gone := f.alertsOf(w5CaseControllerGone, relaunched)
	envoy := f.envoySessions()[session]
	f.check("relaunched-controller-blocked-does-not-reach-ready-with-its-role", envoy.state != "live", "Envoy lists the controller's session as %q (last seen %s)", envoy.state, clockAt(envoy.lastSeen))
	f.recordEmptyReads("relaunched controller", relaunched)
	// stamped is the moment R6 counts its 20 minutes from: the claim's last relaunch as the daemon
	// stamped it (supervise's LastRelaunch, the time of the sweep that asked for the relaunch),
	// which the R6 line carries as relaunchedAt. The daemon stamps it before it logs the relaunch
	// line, and the launch the runtime records comes after both, so neither of those is the
	// reference: a sweep that lands within the gap of the 20 minutes would read as early. The
	// stamp itself is held to that order, after the block and no later than the relaunch line.
	var stamped time.Time
	if len(gone) > 0 {
		stamped = relaunchStampOf(gone[0])
	}
	var lineAt time.Time
	if len(relaunchLines) > 0 {
		lineAt = relaunchLines[0].at
	}
	inOrder := !stamped.IsZero() && !stamped.Before(t0) && !lineAt.IsZero() && !stamped.After(lineAt)
	f.check("relaunched-controller-r6-once-20m-after-the-relaunch", len(gone) == 1 && inOrder && gone[0].at.Sub(stamped) >= w5Unreachable && gone[0].at.Sub(stamped) <= w5MarkWindow+time.Minute,
		"R6 controller_unreachable lines: %d, the first at %s (%s after the relaunch it names at %s; the relaunch line at %s, its launch at %s)",
		len(gone), w5FirstAt(gone), durationAfter(gone, stamped), clockAt(stamped), clockAt(lineAt), clockAt(relaunched))
	for _, rec := range gone {
		f.record("R6: %s", w5Describe(rec))
	}
	f.front.unblock(session)
	f.record("let the controller's registrations through again at %s", clockAt(time.Now()))
	// A controller whose boots failed while its path was blocked waits for the daemon's next retry
	// of it, whose wait grows with each failed retry; the second out-of-reach needs it back first.
	unblocked := time.Now()
	back := waitFor(45*time.Minute, func() bool {
		snap := f.snapshot()
		return snap.controllerMark() == "" && snap.views[ctl.token].state == string(supervise.StateIdle)
	})
	f.check("precondition-controller-back-before-its-second-block", back, "the controller is idle and unmarked %s after its registrations went through again at %s: %t",
		time.Since(unblocked).Round(time.Second), clockAt(unblocked), back)
	f.record("after: %s", describeSnapshot(f.snapshot(), f.names))
	return relaunched
}

func durationAfter(records []loggedRecord, from time.Time) string {
	if len(records) == 0 {
		return "never"
	}
	return records[0].at.Sub(from).Round(time.Second).String()
}

func (f *w5Fresh) controllerAgain(relaunchedAt time.Time) {
	ctl := f.ctl
	session := f.controllerSession()
	launches := len(f.runtime.launchesOf(ctl.token))
	t0 := time.Now()
	f.front.blockRegistrations(session)
	f.record("blocked the controller's registrations a second time (session %s) at %s, %s after the daemon's relaunch of it", session, clockAt(t0), t0.Sub(relaunchedAt).Round(time.Second))
	sleepUntil(t0.Add(w5GuardRead))
	snap := f.answeredSnapshot("second out-of-reach, at 25 minutes")
	gone := f.alertsOf(w5CaseControllerGone, t0)
	f.recordEmptyReads("second out-of-reach", t0)
	f.check("second-out-of-reach-not-relaunched", len(f.runtime.launchesOf(ctl.token)) == launches, "launches at the block %d, 25 minutes on %d", launches, len(f.runtime.launchesOf(ctl.token)))
	f.check("second-out-of-reach-read-state-names-it-unreachable", snap.controllerMark() != "", "read_state's controller locator: %v", snap.controller)
	f.check("second-out-of-reach-r6-written", len(gone) == 1, "R6 controller_unreachable lines since the second block: %d (%s)", len(gone), w5FirstAt(gone))
	for _, rec := range f.fleetAlerts() {
		f.record("R6 line of the run: %s", w5Describe(rec))
	}
	f.front.unblock(session)
}
