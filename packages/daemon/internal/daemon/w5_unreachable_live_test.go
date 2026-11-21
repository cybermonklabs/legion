//go:build e2e

// Check w5 of this change's verification (R5: a claim Envoy cannot reach is marked, reported and
// relaunched; R6: a person is told what no agent can hear), run on the rig (RIG.md) as the check
// states it. scripts/e2e/unreachable-claims.sh builds this driver and runs it; with --base-commit
// it runs the first block against the base's code, the check's negative control.
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
// What a step reads: read_state and read_record (GET /legion/v1/state) and the operator's claim
// listing once a minute (the sampler), the controller's and the architect's notices as the stream
// stored them, the outbox's rows as an audit trigger copies them, the daemon's own log (its
// out-of-reach lines and R6's `LEGION FLEET ALERT` lines), the runtime's launches and the rig's
// relaunch calls, and each claim's stand-in's requests. Every daemon text a step matches is a
// literal here, so the first block builds against the base.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The bounds the check reads against.
const (
	w5Unreachable    = 20 * time.Minute
	w5MarkWindow     = 22 * time.Minute
	w5GuardHeld      = 60 * time.Minute
	w5NoticeSlack    = 2 * time.Minute
	w5FirstRelaunch  = 10 * time.Minute
	w5GuardRelaunch  = 25 * time.Minute
	w5ImplRelaunch   = 30 * time.Minute
	w5NinthBlocked   = 85 * time.Minute
	w5ReadFor        = 25 * time.Minute
	w5AllBlocked     = 30 * time.Minute
	w5RegisterOver   = 2 * time.Minute
	w5ListFailing    = 25 * time.Minute
	w5ActivityReport = 12 * time.Second
	w5PaneReady      = 6 * time.Minute
)

// The daemon's words a step matches.
const (
	w5MarkLine        = "reach: the claim's session has not registered with Envoy for 20 minutes; it is out of reach, and keeps its state"
	w5HeldFinishPhase = "outbox role message held for a claim Envoy cannot reach is finished undelivered: its phase moved on"
	w5HeldFinishEnded = "outbox role message held for a claim Envoy cannot reach is finished undelivered: its claim ended"
	w5NoHolderFinish  = "outbox role message finished undelivered: its role has no live holder"
	w5PhaseChanged    = "workflow: phase changed"
	w5R2Note          = "Your previous turn was interrupted."
	// The cause the daemon's cut-turn note names for a relaunch the controller made of a claim
	// Envoy could not reach, as the resumed session's transcript shows it (main4, 16:24:26Z): "Your
	// process ended because Envoy could not reach its session for 20 minutes, so the project's
	// controller relaunched it on the same session."
	w5RelaunchCause = "Envoy could not reach its session for 20 minutes, so the project's controller relaunched it on the same session"
	w5CasePerson    = "This is a case for a person."
)

// w5Run is one run of w5's main rig.
type w5Run struct {
	*w5Rig
	suffix  string
	names   map[claim.Token]string
	files   string
	t0      time.Time
	blocked map[string]time.Time

	envoyMu  sync.Mutex
	lastSeen map[string]time.Time

	roleRow, hookRow int64
	refusalsMu       sync.Mutex
	refusals         []relaunchAnswer

	// The facts the later steps read of the first block.
	fifthMessageRow, fourthMessageRow int64
	fourthRetired                     time.Time
	ninthFirstRefusal                 time.Time
	fourthCallStarted                 time.Time
	implFrozen                        time.Time
	allBlockedAt, listFailedFrom      time.Time
	listAnswered                      time.Time

	// The stand-ins' answers a step waits on: each the moment the claim's turn was answered with
	// wait, and the eighth's subagent's own request, held until the eighth's step lets it go.
	implWait, fifthWait, seventhWait, eighthWait, implAgainWait chan time.Time
	twelfthWaits                                                chan time.Time
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
		blocked: map[string]time.Time{}, lastSeen: map[string]time.Time{}}
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
	r.startSampler(r.names)
	go r.watchEnvoy()
	for _, c := range r.ordered {
		r.awaitLive(c.token)
	}
	r.awaitLive(r.ctl.token)
	r.names[r.ctl.token] = "controller"
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

// noteRefusal keeps a relaunch answer for the totals' read of the notices around it.
func (r *w5Run) noteRefusal(answer relaunchAnswer) {
	r.refusalsMu.Lock()
	defer r.refusalsMu.Unlock()
	if !answer.reached() {
		r.refusals = append(r.refusals, answer)
	}
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
		r.blocked[name] = at
		r.record("blocked the registrations of %s (%s, session %s) at %s", name, r.c(name).token, session, clockAt(at))
	}
	return at
}

func (r *w5Run) unblock(names ...string) {
	for _, name := range names {
		session := r.session(r.c(name).token)
		r.front.unblock(session)
		delete(r.blocked, name)
		r.record("let the registrations of %s (session %s) through again at %s", name, session, clockAt(time.Now()))
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

func sleepUntil(at time.Time) {
	if d := time.Until(at); d > 0 {
		time.Sleep(d)
	}
}

// ---- the first block -----------------------------------------------------------------------------

func (r *w5Run) firstBlock() {
	before := r.answeredSnapshot("first block, before the block")
	r.t0 = r.block(append([]string{"twelfth"}, w5FirstBlock...)...)
	t0 := r.t0
	impl := r.c("implementer")
	r.roleRow = r.enqueueRow(impl.key, record.RoleMessage{Role: string(impl.token), Body: fmt.Sprintf("W5-ROLEMSG-%s: a role message the daemon publishes to the implementer while Envoy cannot reach it.", r.suffix)})
	r.hookRow = r.enqueueRow(impl.key, record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing,
		Count: 1, Counts: map[string]int{"reviewing-implementing": 1}, Prompt: fmt.Sprintf("W5-HOOK-%s: read the review before you write anything.", r.suffix)})
	r.record("first block: the daemon's outbox holds a role message (row %d) and a hook prompt (row %d) for %s", r.roleRow, r.hookRow, impl.token)

	sleepUntil(t0.Add(time.Minute))
	for _, name := range w5FirstBlock {
		session := r.session(r.c(name).token)
		r.record("first block: %s's last registration as the listener's own list shows it: %s", name, clockAt(r.lastSeenOf(session)))
	}

	sleepUntil(t0.Add(w5FirstRelaunch))
	at10 := r.answeredSnapshot("first block, at 10 minutes")
	var marked []string
	for _, name := range w5FirstBlock {
		if at10.views[r.c(name).token].marked() {
			marked = append(marked, name)
		}
	}
	answer := r.relaunch(impl.token)
	if answer.status == http.StatusNotFound && strings.Contains(answer.body, "no route") {
		r.baseFirstBlock(answer)
		return
	}
	r.check("at-10m-no-blocked-claim-marked", len(marked) == 0, "at %s (%s after the block) the marked blocked claims are %v", clockAt(at10.at), at10.at.Sub(t0).Round(time.Second), marked)
	r.check("at-10m-relaunch-refuses-the-implementer", answer.status == http.StatusConflict && answer.code == "CLAIM_REACHABLE", "relaunch answered %s", answer)
	r.noteRefusal(answer)

	sleepUntil(t0.Add(w5MarkWindow + 30*time.Second))
	at22 := r.answeredSnapshot("first block, at 22 minutes")
	r.recordEmptyReads("first block", t0.Add(-5*time.Minute))
	if at22.err != "" {
		r.check("read-state-answers-at-22m", false, "read_state did not answer from %s for 5 minutes: %s", clockAt(at22.at), at22.err)
	}
	for _, name := range w5FirstBlock {
		r.checkMarked(name, before, at22, t0)
	}
	r.checkListNotices(w5FirstBlock, t0, "first-block")
	r.checkArchitectRows(t0)

	sleepUntil(t0.Add(w5GuardRelaunch))
	r.guardRelaunches()
	r.twelfthRelaunch()

	sleepUntil(t0.Add(w5GuardRelaunch + 30*time.Second))
	r.afterTheRefusals()

	sleepUntil(t0.Add(w5ImplRelaunch))
	r.implementerRelaunch()
}

// checkMarked reads name's mark: between 20 and 22 minutes after its session's last registration,
// with that time, and its state kept.
func (r *w5Run) checkMarked(name string, before, after w5Snapshot, from time.Time) {
	c := r.c(name)
	mark := r.markOf(c.token, from)
	session := r.session(c.token)
	seen := r.lastSeenOf(session)
	v := after.views[c.token]
	if mark == nil {
		r.check("marked-20-to-22m-"+name, false, "the daemon wrote no out-of-reach line for %s; the state reads %s", c.token, v)
		return
	}
	logged := fmt.Sprint(mark.Attrs["lastSeen"])
	since := fmt.Sprint(mark.Attrs["since"])
	gap := mark.at.Sub(seen)
	r.check("marked-20-to-22m-"+name, !seen.IsZero() && gap >= w5Unreachable && gap <= w5MarkWindow,
		"%s marked at %s, %s after its session's last registration %s (the daemon logged since %s, lastSeen %s)",
		c.token, clockAt(mark.at), gap.Round(time.Second), clockAt(seen), since, logged)
	sinceAt, _ := time.Parse(time.RFC3339Nano, v.since)
	r.check("reads-unreachable-with-that-time-"+name, v.marked() && absDuration(sinceAt.Sub(seen)) < 2*time.Second,
		"read_state shows %s %s; its session's last registration was %s", c.token, v, clockAt(seen))
	r.check("keeps-its-state-"+name, v.state == before.views[c.token].state,
		"%s was %s before the block and is %s marked", c.token, before.views[c.token].state, v.state)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// checkListNotices holds that each of names is in exactly one unreachable-claims notice from from.
func (r *w5Run) checkListNotices(names []string, from time.Time, label string) {
	lists := r.listNotices(from)
	for _, n := range lists {
		r.record("%s: unreachable-claims notice %s", label, n)
	}
	for _, name := range names {
		token := string(r.c(name).token)
		count := 0
		for _, n := range lists {
			for _, row := range n.rows {
				if row == token {
					count++
				}
			}
		}
		r.check(label+"-one-list-notice-"+name, count == 1, "%s is named in %d unreachable-claims notices from %s", token, count, clockAt(from))
	}
}

func (r *w5Run) listNotices(from time.Time) []w5Notice {
	var out []w5Notice
	for _, n := range r.controllerNoticesW5() {
		if n.kind == w5NoticeUnreachable && !n.at.Before(from) {
			out = append(out, n)
		}
	}
	return out
}

// checkArchitectRows reads the architect's notice rows the outbox wrote from from: one names the
// implementer's claim, and none names the architect's own.
func (r *w5Run) checkArchitectRows(from time.Time) {
	arch, impl := r.c("architect").token, r.c("implementer").token
	var names []string
	namesImpl, namesOwn := 0, 0
	for _, life := range r.rowLives("reach_notice") {
		var payload struct {
			Kind  string `json:"kind"`
			Claim string `json:"claim"`
		}
		_ = json.Unmarshal([]byte(life.payload), &payload)
		if payload.Kind != w5NoticeClaimOutOfReach || life.written.Before(from) {
			continue
		}
		names = append(names, fmt.Sprintf("row %d %s written %s finished %s", life.id, payload.Claim, clockAt(life.written), clockAt(life.finished)))
		if payload.Claim == string(impl) {
			namesImpl++
		}
		if payload.Claim == string(arch) {
			namesOwn++
		}
	}
	sort.Strings(names)
	r.record("first block: the architect's claim-out-of-reach notice rows: %v", names)
	r.check("architect-row-names-the-implementer", namesImpl == 1, "%d architect rows name %s", namesImpl, impl)
	r.check("no-architect-row-names-its-own-claim", namesOwn == 0, "%d architect rows name the architect's own claim %s", namesOwn, arch)
}

// guardRelaunches is the 25-minute relaunches: the third, fourth, fifth, eighth and ninth refused
// by the guard with their reasons, and the seventh reached, its service stopping with its process.
func (r *w5Run) guardRelaunches() {
	cases := []struct {
		name, want string
	}{
		{"third", "worker stream showed a step"},
		{"fourth", "open call is bash"},
		{"fifth", "background work"},
		{"eighth", "background work"},
		{"ninth", "no activity report"},
	}
	for _, tc := range cases {
		answer := r.relaunch(r.c(tc.name).token)
		r.noteRefusal(answer)
		r.check("at-25m-relaunch-refuses-"+tc.name, answer.status == http.StatusConflict && answer.code == "RELAUNCH_GUARD" && strings.Contains(answer.body, tc.want),
			"relaunch answered %s; want RELAUNCH_GUARD naming %q", answer, tc.want)
		if tc.name == "ninth" {
			r.ninthFirstRefusal = answer.at
		}
		if tc.name == "eighth" {
			r.check("eighth-refusal-names-a-subagent", strings.Contains(strings.ToLower(answer.body), "agent"), "the eighth's refusal: %s", answer.body)
		}
	}
	seventh := r.c("seventh")
	pid := readPid(r.file("seventh-svc.pid"))
	alive := pid > 0 && syscall.Kill(pid, 0) == nil
	before := r.claim(seventh.token)
	oldLaunch := r.runtime.current(seventh.token)
	answer := r.relaunch(seventh.token)
	r.check("at-25m-relaunch-reaches-the-seventh", answer.reached(), "relaunch answered %s", answer)
	r.unblock("seventh")
	gone := false
	var goneAt time.Time
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if pid > 0 && syscall.Kill(pid, 0) != nil {
			gone, goneAt = true, time.Now()
			break
		}
		time.Sleep(time.Second)
	}
	ended := "still alive 2 minutes after the relaunch"
	if gone {
		ended = fmt.Sprintf("gone %s after the relaunch", goneAt.Sub(answer.at).Round(time.Second))
	}
	r.check("seventh-service-stops-with-the-old-process", alive && gone,
		"the seventh's named service (pid %d) ran before the relaunch: %t; it was %s (old process pid %d ended: %t)",
		pid, alive, ended, oldLaunch.pane.pid, !oldLaunch.running())
	after := r.claim(seventh.token)
	r.record("the seventh before the relaunch: %s gen %d, budgets %+v; after: %s gen %d, budgets %+v",
		before.State, before.Generation, before.Budgets, after.State, after.Generation, after.Budgets)
}

// twelfthRelaunch is the amended check's twelfth claim, in a live wait on a named bash service the
// rig never stopped, its registrations stopped with the first block's: its plugin sends its
// activity report in its turn every 5 minutes (and on a build whose wait returns, the wait is
// called again), so relaunch 25 minutes after the block is refused as a claim whose process still
// runs its turn, by either rule of the guard that reads one (twelfthRefusedInItsTurn), its process
// keeps running, and it is charged nothing. It records each report the twelfth sent in its turn and
// each step its worker stream carried, with their times, the step record the daemon stores and its
// mark, then lets its registrations through again.
func (r *w5Run) twelfthRelaunch() {
	c := r.c("twelfth")
	var stepAt, started, ended *time.Time
	var tool string
	_ = r.st.Pool().QueryRow(context.Background(), "select step_at, step_tool, step_started_at, step_ended_at from claims where token = $1", string(c.token)).
		Scan(&stepAt, &tool, &started, &ended)
	mark := r.markOf(c.token, r.t0)
	before := r.claim(c.token)
	launch := r.runtime.current(c.token)
	answer := r.relaunch(c.token)
	r.noteRefusal(answer)
	// What the run saw is read once the answer is in, so a step or a report the daemon stamped while
	// the request was on its way is among them.
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
	time.Sleep(liveSettle)
	after := r.claim(c.token)
	r.record("twelfth (never stopped, in a live wait on its named service, registrations blocked at %s): its turn began at %s; %d activity reports in its turn: %v", clockAt(r.t0), clockAt(turnAt), len(reports), reports)
	r.record("twelfth: %d worker-stream steps from the block: %v", len(steps), steps)
	r.record("twelfth: the daemon's step record: at %s, tool %q, started %s, ended %s; marked unreachable at %s",
		clockAtPtr(stepAt), tool, clockAtPtr(started), clockAtPtr(ended), clockAtLine(mark))
	r.check("at-25m-relaunch-refuses-the-twelfth-whose-process-still-runs-its-turn",
		twelfthRefusedInItsTurn(twelfthAnswer{status: answer.status, code: answer.code, body: answer.body,
			sent: answer.at, answered: answer.answered, steps: stepTimes, reports: reportTimes}),
		"relaunch answered %s", answer)
	r.check("twelfth-keeps-running", launch != nil && launch.running() && r.runtime.current(c.token) == launch,
		"its process (generation %d) still runs: %t, the claim's current launch unchanged: %t", launch.spec.Generation, launch.running(), r.runtime.current(c.token) == launch)
	r.check("twelfth-charged-nothing", after.Budgets == before.Budgets, "budgets before %+v, after %+v", before.Budgets, after.Budgets)
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
	writeProofFile(r.t, r.file("fifth-job.done"), "done\n")
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

// implementerRelaunch is the 30-minute relaunch of the implementer: its old process stopped, the
// claim reached with no charge, and the resumed session given R2's note, the role message, the hook
// prompt, and its owed task behind the daemon's cut-turn note naming the relaunch.
func (r *w5Run) implementerRelaunch() {
	impl := r.c("implementer")
	before := r.claim(impl.token)
	old := r.runtime.current(impl.token)
	impl.stand.captureFrom()
	// The resumed session is put back in a wait loop on its owed task's turn (the next step); the
	// old process's own loop is spent first, since its rules would take the resumed session's
	// turns too, whose conversation still carries the old task's marker.
	impl.stand.dropRules()
	r.implAgainWait = r.scriptBackInAWait(impl, "impl-svc2", w5RelaunchCause)
	answer := r.relaunch(impl.token)
	r.unblock("implementer")
	r.check("at-30m-relaunch-reaches-the-implementer", answer.reached(), "relaunch answered %s", answer)
	ended := false
	select {
	case <-old.pane.done:
		ended = true
	case <-time.After(30 * time.Second):
	}
	r.check("at-30m-relaunch-stops-the-old-process", ended, "the old process (pid %d, generation %d) ended: %t", old.pane.pid, old.spec.Generation, ended)
	r.await("the implementer live on its resumed session", w5PaneReady, func() bool {
		held := r.claim(impl.token)
		return held.Generation > before.Generation && (held.State == string(supervise.StateReady) || held.State == string(supervise.StateIdle) || held.State == string(supervise.StateWorking))
	})
	after := r.claim(impl.token)
	r.check("at-30m-relaunch-charges-nothing", after.Budgets.LaunchFailures == before.Budgets.LaunchFailures && after.Budgets.Deaths == before.Budgets.Deaths,
		"budgets before %+v, after %+v", before.Budgets, after.Budgets)
	r.check("at-30m-relaunch-keeps-the-session", after.Session == before.Session, "session before %s, after %s", before.Session, after.Session)
	var conversation strings.Builder
	r.await("the resumed implementer's turns to carry the role message, the hook prompt and its task", 6*time.Minute, func() bool {
		conversation.Reset()
		for _, seen := range impl.stand.requestsAfter(answer.at) {
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
	r.record("the resumed implementer's requests from the relaunch: %d; the note's context: %s", len(impl.stand.requestsAfter(answer.at)), excerpt(text, w5R2Note, 400))
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

// baseFirstBlock is the first block where the daemon has no relaunch route, which is the base's
// code: at 22 minutes read_state shows the implementer's claim working with no mark, no notice
// names it, fresh_session refuses it CLAIM_IN_A_TURN, and the role message is finished undelivered
// once Envoy reads the session gone. It reads each and fails the step at the mark with all of it,
// which the script's negative control reads back.
func (r *w5Run) baseFirstBlock(at10 relaunchAnswer) {
	impl := r.c("implementer")
	r.record("base: relaunch at 10 minutes answered %s: this daemon has no relaunch route", at10)
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
		return rec.Message == w5NoHolderFinish && fmt.Sprint(rec.Attrs["row"]) == fmt.Sprint(r.roleRow)
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
	treeDone := make(chan struct{}, 2)
	podsDone := make(chan struct{})
	run("implementer-again", func(f *w5Flows) { defer func() { treeDone <- struct{}{} }(); r.implementerAgain(f) })
	run("tenth", func(f *w5Flows) { defer close(tenthDone); r.tenthFlow(f) })
	run("eighth", func(f *w5Flows) { defer func() { treeDone <- struct{}{} }(); r.eighthFlow(f) })
	run("fifth", r.fifthFlow)
	run("ninth", r.ninthFlow)
	// The architect is the claim alone in its pod, and the relaunch guard lets a claim through only
	// after 20 minutes with no step: the pods wait for every flow of the architect's tree (the
	// implementer's and the eighth's), whose notices wake it (main4: the eighth's failure notice
	// woke it at 17:01:33Z, inside its block, and its relaunch was refused RELAUNCH_GUARD).
	run("pods", func(f *w5Flows) {
		<-tenthDone
		<-treeDone
		<-treeDone
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

// noticeFor is the first notice of kind on the controller's topic naming token, from from.
func (r *w5Run) noticeFor(kind string, token claim.Token, from time.Time) (w5Notice, int) {
	var first w5Notice
	count := 0
	for _, n := range r.controllerNoticesW5() {
		if n.kind == kind && n.claim == string(token) && !n.at.Before(from) {
			if count == 0 {
				first = n
			}
			count++
		}
	}
	return first, count
}

// implementerAgain blocks the resumed implementer's registrations again once it is back in a wait
// call, and relaunches it 25 minutes after that block: refused as a second relaunch within 6
// hours, with no launch failure charged, the claim still marked, and a case for a person.
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
	t1 := r.block("implementer")
	sleepUntil(t1.Add(w5GuardRelaunch))
	before := r.held(impl.token)
	answer := r.relaunch(impl.token)
	r.noteRefusal(answer)
	after := r.snapshot()
	r.observe(f, "second-relaunch-within-6h-refused", answer.status == http.StatusConflict && answer.code == "RELAUNCHED_RECENTLY", "relaunch answered %s", answer)
	r.observe(f, "second-relaunch-names-a-case-for-a-person", strings.Contains(answer.body, w5CasePerson), "the answer: %s", answer.body)
	held := r.held(impl.token)
	r.observe(f, "second-relaunch-charges-no-launch-failure", held.Budgets.LaunchFailures == before.Budgets.LaunchFailures,
		"launch failures before %d, after %d", before.Budgets.LaunchFailures, held.Budgets.LaunchFailures)
	r.observe(f, "second-relaunch-claim-stays-marked", after.views[impl.token].marked(), "read_state: %s", after.views[impl.token])
}

// tenthFlow blocks the tenth claim's registrations while it owes no task, kills its process so the
// daemon's own supervision relaunches it, and reads its marks, its relaunches, its charges and the
// controller's notices for 25 minutes.
func (r *w5Run) tenthFlow(f *w5Flows) {
	tenth := r.c("tenth")
	time.Sleep(2 * time.Minute)
	start := r.block("tenth")
	session := r.session(tenth.token)
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
	seen := r.lastSeenOf(session)
	r.record("tenth: %d relaunches by the daemon in %s; its readings: %s", relaunches, w5ReadFor, strings.Join(series, "; "))
	r.observe(f, "tenth-relaunch-count", relaunches > 0, "the daemon relaunched the tenth %d times in the 25 minutes", relaunches)
	r.observe(f, "tenth-nets-no-charge", maxDeaths == 0 && final.launchFailures == 0 && final.state != string(supervise.StateFailed),
		"deaths at most %d; at the end %s", maxDeaths, final)
	if mark == nil {
		r.observe(f, "tenth-marked-20m-after-last-registration", false, "no out-of-reach line for the tenth")
	} else {
		gap := mark.at.Sub(seen)
		r.observe(f, "tenth-marked-20m-after-last-registration", gap >= w5Unreachable && gap <= w5MarkWindow,
			"marked at %s, %s after its last registration %s", clockAt(mark.at), gap.Round(time.Second), clockAt(seen))
	}
	r.observe(f, "tenth-stays-marked-through-relaunches", !markedAt.IsZero() && !unmarkedAfterMark, "first read marked at %s; unmarked again after: %t", clockAt(markedAt), unmarkedAfterMark)
	exitNotice, count := r.noticeFor(w5NoticeExit, tenth.token, start)
	r.observe(f, "tenth-exit-notice-once-a-case-for-a-person", count == 1 && strings.Contains(exitNotice.reason, w5CasePerson) && (mark == nil || exitNotice.at.After(mark.at)),
		"%d unreachable-exit notices name the tenth; the first %s", count, exitNotice)
	r.unblock("tenth")
	waitFor(5*time.Minute, func() bool {
		v := r.snapshot().views[tenth.token]
		return !v.marked() && (v.state == string(supervise.StateIdle) || v.state == string(supervise.StateReady))
	})
	r.record("tenth after its registrations go through again: %s", r.snapshot().views[tenth.token])
}

// eighthFlow lets the eighth's subagent end and puts the claim back in a wait, then relaunches it
// when the controller's notice says the guard would let it through, with its registrations still
// blocked and its task owed: each exit at its role claim is charged a death, it fails at the
// deaths limit with today's failure notice to its architect, and stays marked until then.
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
	// It stays in the wait call of its loop that is open once its subagent has ended.
	waitAt, ok := r.nextOpenWait(eighth.token, r.eighthWait, 10*time.Minute)
	if !ok {
		r.observe(f, "eighth-back-in-a-wait", false, "no wait call open within 10 minutes of its subagent's end")
		return
	}
	r.record("the eighth is in a wait call answered at %s", clockAt(waitAt))
	pid := r.stopProcess(eighth.token)
	r.record("the eighth (Oh My Pi pid %d) stopped inside its wait call", pid)
	backInWait := time.Now()
	r.observe(f, "eighth-back-in-a-wait", true, "stopped inside its wait at %s, its subagent ended", clockAt(backInWait))
	var ready w5Notice
	if !waitFor(45*time.Minute, func() bool {
		n, count := r.noticeFor(w5NoticeRelaunchReady, eighth.token, backInWait)
		ready = n
		return count > 0
	}) {
		r.observe(f, "eighth-ready-notice", false, "no relaunch-ready notice for the eighth within 45 minutes")
		return
	}
	r.record("eighth: relaunch-ready notice %s", ready)
	// Each relaunched process's task turn is open when the process exits at its role claim, as a
	// real agent's is a second into its turn, its model not having answered. The stand-in answers a
	// turn at once, so the turn had ended at each exit and the claim owed no task at it (main5:
	// gen 3's turn ended at 23:12:20.309Z, its process exited at 23:12:20.827Z, no death, and the
	// controller's notice read the eighth as owing no task); its turns are held from here on, each
	// until its process ends.
	eighth.stand.dropRules()
	eighth.stand.holdEvery(anyTurn)
	before := r.held(eighth.token)
	answer := r.relaunch(eighth.token)
	r.observe(f, "eighth-relaunch-reaches-it-on-the-ready-notice", answer.reached(), "relaunch answered %s", answer)
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
			if life.issue == eighth.key && payload.Kind == "worker-died" && len(payload.Failure) > 0 && life.written.After(answer.at) {
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

// fifthFlow waits for the controller's notice that the guard would now let the fifth through, once
// its job has ended and it waits on its named service, and relaunches it: reached with no charge.
// The resumed session's turn on its owed task then moves the issue's phase on, its registrations
// still blocked, and the role message the outbox held for it is finished undelivered then, not
// before.
func (r *w5Run) fifthFlow(f *w5Flows) {
	fifth := r.c("fifth")
	var ready w5Notice
	count := 0
	if !waitFor(50*time.Minute, func() bool {
		ready, count = r.noticeFor(w5NoticeRelaunchReady, fifth.token, r.t0)
		return count > 0
	}) {
		r.observe(f, "fifth-ready-notice-once", false, "no relaunch-ready notice for the fifth within 50 minutes")
		return
	}
	moved := fifth.stand.script(func(seen seenRequest) bool {
		return seen.prompt && anyTurn(seen) && (strings.Contains(seen.last, w5RelaunchCause) || strings.Contains(seen.conversation, w5RelaunchCause))
	}, 0, "legion", map[string]any{"op": "request_backward_move", "to": "implementing", "reason": "w5: the review asks for changes"})
	before := r.held(fifth.token)
	answer := r.relaunch(fifth.token)
	time.Sleep(time.Minute)
	after := r.held(fifth.token)
	_, count = r.noticeFor(w5NoticeRelaunchReady, fifth.token, r.t0)
	r.observe(f, "fifth-ready-notice-once", count == 1, "%d relaunch-ready notices name the fifth; the first %s", count, ready)
	r.observe(f, "fifth-relaunch-reaches-it-with-no-charge", answer.reached() && after.Budgets.LaunchFailures == before.Budgets.LaunchFailures && after.Budgets.Deaths == before.Budgets.Deaths,
		"relaunch answered %s; budgets before %+v, a minute after %+v", answer, before.Budgets, after.Budgets)
	select {
	case at := <-moved:
		r.record("the resumed fifth's turn called request_backward_move at %s", clockAt(at))
	case <-time.After(10 * time.Minute):
		r.observe(f, "fifth-message-finished-at-phase-move", false, "the resumed fifth's turn did not move its phase within 10 minutes of its relaunch")
		return
	}
	// The moment the phase moved is the daemon's own line of the move, not the first read of
	// read_state that shows it: the read came a minute after the relaunch, and main5 took the move
	// of 23:08:18.140Z for 23:09:16.396Z, which put the row's finish at 23:08:38.845Z before it.
	// The line is written once the move has committed (engine.logOnCommit), so a finish the outbox
	// made between the commit and the line reads as up to a second before it.
	var movedAt time.Time
	waitFor(3*time.Minute, func() bool {
		moves := r.logs(func(rec loggedRecord) bool {
			return rec.Message == w5PhaseChanged && fmt.Sprint(rec.Attrs["issue"]) == fifth.key &&
				fmt.Sprint(rec.Attrs["from"]) == string(phase.Reviewing) && !rec.at.Before(answer.at)
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

// ninthFlow waits for the controller's notice, 60 minutes after the ninth's first refusal, that
// names it as a case for a person, then lets its registrations through again at 85 minutes.
func (r *w5Run) ninthFlow(f *w5Flows) {
	ninth := r.c("ninth")
	var held w5Notice
	got := waitFor(time.Until(r.t0.Add(w5NinthBlocked+3*time.Minute)), func() bool {
		n, count := r.noticeFor(w5NoticeGuardHeld, ninth.token, r.t0)
		held = n
		return count > 0
	})
	_, count := r.noticeFor(w5NoticeGuardHeld, ninth.token, r.t0)
	gap := held.at.Sub(r.ninthFirstRefusal)
	r.observe(f, "ninth-guard-held-notice-at-60m", got && count == 1 && gap >= w5GuardHeld && gap <= w5GuardHeld+w5NoticeSlack && strings.Contains(held.reason, w5CasePerson),
		"%d relaunch-guard-held notices name the ninth; the first %s, %s after its first refusal at %s", count, held, gap.Round(time.Second), clockAt(r.ninthFirstRefusal))
	sleepUntil(r.t0.Add(w5NinthBlocked))
	r.unblock("ninth")
}

// podsFlow blocks every claim of one pod of two at once (the sixth and the tenth) and a claim alone
// in its pod (the architect), and once read_state shows them unreachable relaunches one of the pod
// (refused, naming the pod, nothing stopped) and the one alone (reached).
func (r *w5Run) podsFlow(f *w5Flows) {
	sixth, tenth, arch := r.c("sixth"), r.c("tenth"), r.c("architect")
	waitFor(5*time.Minute, func() bool { return !r.snapshot().views[tenth.token].marked() })
	// Every notice the outbox owes the architect is written for an issue of its tree; the block
	// starts once none is left, so nothing of the run wakes the architect inside it. A notice
	// re-held for a session that did not confirm it is copied at most three times
	// (noticeReholdDelays), so the outbox runs dry.
	treeIssues := []string{arch.key, r.c("implementer").key, r.c("eighth").key}
	quiet := waitFor(30*time.Minute, func() bool { return r.pendingRows("notice", treeIssues...) == 0 })
	r.record("pods: the outbox owes the architect no notice of %v: %t; the architect is %s", treeIssues, quiet, r.snapshot().views[arch.token])
	start := r.block("sixth", "tenth", "architect")
	if !waitFor(30*time.Minute, func() bool {
		snap := r.snapshot()
		return snap.views[sixth.token].marked() && snap.views[tenth.token].marked() && snap.views[arch.token].marked()
	}) {
		r.observe(f, "pod-claims-read-unreachable", false, "the three claims were not all marked within 30 minutes of %s", clockAt(start))
		r.unblock("sixth", "tenth", "architect")
		return
	}
	snap := r.snapshot()
	r.record("pods: %s %s, %s %s, %s %s", sixth.token, snap.views[sixth.token], tenth.token, snap.views[tenth.token], arch.token, snap.views[arch.token])
	sixthLaunch, tenthLaunch := r.runtime.current(sixth.token), r.runtime.current(tenth.token)
	callsFrom := time.Now()
	answer := r.relaunch(sixth.token)
	r.noteRefusal(answer)
	nothingStopped := sixthLaunch.running() && tenthLaunch.running() && r.runtime.current(sixth.token) == sixthLaunch &&
		r.runtime.current(tenth.token) == tenthLaunch && len(r.pods.relaunchCalls(sixth.token, callsFrom)) == 0
	r.observe(f, "pod-of-two-relaunch-refused-naming-the-pod", answer.status == http.StatusConflict && answer.code == "POD_UNREACHABLE" &&
		strings.Contains(answer.body, snap.views[sixth.token].pod) && strings.Contains(answer.body, w5CasePerson), "relaunch answered %s", answer)
	r.observe(f, "pod-of-two-nothing-stopped", nothingStopped, "the sixth's process (pid %d) runs: %t; the tenth's (pid %d): %t; the runtime was asked nothing of the sixth: %t",
		sixthLaunch.pane.pid, sixthLaunch.running(), tenthLaunch.pane.pid, tenthLaunch.running(), len(r.pods.relaunchCalls(sixth.token, callsFrom)) == 0)
	alone := r.relaunch(arch.token)
	if !alone.reached() {
		// Name every notice the outbox wrote the architect's tree inside the block, the run's own
		// wakes of it, so a refusal reads as the run's or as the daemon's.
		var woke []string
		for _, life := range r.rowLives("notice") {
			if containsString(treeIssues, life.issue) && life.written.After(start) {
				woke = append(woke, fmt.Sprintf("row %d %s written %s finished %s", life.id, life.issue, clockAt(life.written), clockAt(life.finished)))
			}
		}
		sort.Strings(woke)
		r.record("pods: the notices the outbox wrote the architect's tree after its block at %s: %v", clockAt(start), woke)
	}
	r.observe(f, "claim-alone-in-its-pod-relaunch-reaches-it", alone.reached(), "relaunch answered %s", alone)
	r.unblock("sixth", "tenth", "architect")
	waitFor(10*time.Minute, func() bool {
		snap := r.snapshot()
		return !snap.views[sixth.token].marked() && !snap.views[tenth.token].marked() && !snap.views[arch.token].marked()
	})
	r.record("pods after: %s", describeSnapshot(r.snapshot(), r.names))
}

// sixthFlow blocks the sixth claim's registrations three times, the rig's runtime reporting its
// launcher not connected, then that the launch would replace the pod, then answering the stop
// while the process outlives it, and relaunches it each time once read_state shows it unreachable.
func (r *w5Run) sixthFlow(f *w5Flows) {
	sixth, tenth := r.c("sixth"), r.c("tenth")
	runs := []struct {
		state, code string
		afterStop   bool
	}{
		{w5LauncherNotConnected, "LAUNCHER_NOT_CONNECTED", false},
		{w5LauncherReplacesPod, "RELAUNCH_REPLACES_POD", false},
		{w5LauncherOutlivesStop, "PROCESS_OUTLIVED_STOP", true},
	}
	for i, run := range runs {
		label := fmt.Sprintf("sixth-run-%d-%s", i+1, run.state)
		r.pods.setLauncher(sixth.token, run.state)
		start := r.block("sixth")
		if !waitFor(30*time.Minute, func() bool { return r.snapshot().views[sixth.token].marked() }) {
			r.observe(f, label, false, "the sixth was not marked within 30 minutes of %s", clockAt(start))
			r.unblock("sixth")
			continue
		}
		launch, mate := r.runtime.current(sixth.token), r.runtime.current(tenth.token)
		callsFrom := time.Now()
		answer := r.relaunch(sixth.token)
		r.noteRefusal(answer)
		calls := r.pods.relaunchCalls(sixth.token, callsFrom)
		var described []string
		stopped := false
		for _, c := range calls {
			described = append(described, fmt.Sprintf("%s %s may=%t launcher=%q answered %q running=%t", clockAt(c.at), c.call, c.mayReplacePod, c.launcher, c.answer, c.running))
			if c.call == "StopForRelaunch" {
				stopped = true
			}
		}
		r.record("%s: the runtime's relaunch calls: %v", label, described)
		r.observe(f, label+"-refused-with-its-reason", answer.status == http.StatusConflict && answer.code == run.code && strings.Contains(answer.body, w5CasePerson),
			"relaunch answered %s", answer)
		r.observe(f, label+"-refused-before-or-after-the-stop", stopped == run.afterStop && launch.running() && r.runtime.current(sixth.token) == launch,
			"the stop was asked: %t (want %t); the process (pid %d) still runs: %t", stopped, run.afterStop, launch.pane.pid, launch.running())
		r.observe(f, label+"-no-other-claim-of-the-pod-touched", mate.running() && r.runtime.current(tenth.token) == mate && len(r.pods.relaunchCalls(tenth.token, callsFrom)) == 0,
			"the tenth's process (pid %d) runs: %t, unchanged: %t", mate.pane.pid, mate.running(), r.runtime.current(tenth.token) == mate)
		r.pods.setLauncher(sixth.token, "")
		r.unblock("sixth")
		waitFor(10*time.Minute, func() bool { return !r.snapshot().views[sixth.token].marked() })
	}
}

// ---- the daemon's restart ------------------------------------------------------------------------

// daemonRestart restarts the daemon and reads the marks and the notices for 25 minutes: a claim
// reported before the restart gets no new notice.
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
	stopped, serving := r.restartDaemon()
	r.record("restart: the daemon stopped at %s and served again at %s", clockAt(stopped), clockAt(serving))
	end := serving.Add(w5ReadFor)
	for time.Now().Before(end) {
		time.Sleep(time.Minute)
		r.record("restart: %s", describeSnapshot(r.snapshot(), r.names))
	}
	// The controller's notices and the architect's, each naming a claim by its token or in an
	// unreachable list.
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
	r.check("after-restart-no-new-notice-for-a-claim-reported-before", len(renewed) == 0, "notices to the controller and the architect after the restart at %s naming a claim marked before it: %v", clockAt(stopped), renewed)
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
	for name := range r.blocked {
		r.unblock(name)
	}
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
	for time.Now().Before(blockedAt.Add(w5AllBlocked)) {
		time.Sleep(time.Minute)
		snap := r.snapshot()
		r.record("all blocked: %s", describeSnapshot(snap, r.names))
		for token, v := range snap.views {
			if v.marked() {
				markedDuring = append(markedDuring, string(token))
			}
		}
		if snap.controllerMark() != "" {
			markedDuring = append(markedDuring, "controller")
		}
	}
	r.check("every-session-blocked-none-marked", len(markedDuring) == 0, "marked while every session was blocked: %v", markedDuring)
	r.recordEmptyReads("every session blocked", blockedAt)
	noLive := []w5Notice{}
	for _, n := range r.controllerNoticesW5() {
		if n.kind == w5NoticeNoLive && !n.at.Before(blockedAt) {
			noLive = append(noLive, n)
		}
	}
	r.check("controller-told-once-no-live-session", len(noLive) == 1, "envoy-no-live-session notices: %v", noLive)
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
	r.check("none-marked-or-relaunched-while-they-register-again", len(markedAfter) == 0 && len(r.runtime.Launches()) == launchesBefore,
		"marked %v; launches since the block %d", markedAfter, len(r.runtime.Launches())-launchesBefore)
	// The list fails for 25 minutes, then answers again.
	failFrom := time.Now()
	until := r.envoy.failFor(w5ListFailing)
	r.record("the list fails from %s until %s (registrations all went through at %s)", clockAt(failFrom), clockAt(until), clockAt(registered))
	markedFailing := []string{}
	for time.Now().Before(until) {
		time.Sleep(time.Minute)
		snap := r.snapshot()
		for token, v := range snap.views {
			if v.marked() {
				markedFailing = append(markedFailing, string(token))
			}
		}
	}
	r.check("failed-list-changes-no-mark", len(markedFailing) == 0, "marked while the list failed: %v", markedFailing)
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
	r.check("none-marked-or-relaunched-after-the-list-answers-again", len(markedAfter) == 0 && len(r.runtime.Launches()) == launchesBefore,
		"marked %v; launches since the block %d", markedAfter, len(r.runtime.Launches())-launchesBefore)
	r.allBlockedAt, r.listFailedFrom, r.listAnswered = blockedAt, failFrom, answered
}

// ---- totals --------------------------------------------------------------------------------------

// totals reads what holds over the whole run: each blocked claim named in exactly one
// unreachable-list notice for each time it went out of reach, no controller notice for a
// relaunch's refusal, and R6's lines: one for the list with no live session, one for the 25
// minutes of failed reads, and no other, each starting with the fixed text and naming its case and
// project.
func (r *w5Run) totals() {
	episodes := map[string]int{}
	for _, rec := range r.logs(func(rec loggedRecord) bool { return rec.Message == w5MarkLine }) {
		episodes[fmt.Sprint(rec.Attrs["claim"])]++
	}
	listed := map[string]int{}
	for _, n := range r.listNotices(time.Time{}) {
		for _, row := range n.rows {
			listed[row]++
		}
	}
	for token, count := range episodes {
		if token == string(r.ctl.token) {
			continue
		}
		r.check("one-list-notice-per-episode-"+r.names[claim.Token(token)], listed[token] == count,
			"%s went out of reach %d times and is named in %d unreachable-claims notices", token, count, listed[token])
	}
	for _, n := range r.controllerNoticesW5() {
		r.record("controller notice %s", n)
	}
	r.refusalsMu.Lock()
	refusals := append([]relaunchAnswer{}, r.refusals...)
	r.refusalsMu.Unlock()
	for _, refusal := range refusals {
		var after []string
		for _, n := range r.controllerNoticesW5() {
			if n.at.After(refusal.at) && n.at.Before(refusal.at.Add(45*time.Second)) && strings.Contains(strings.ToLower(n.reason), "refus") && !strings.Contains(n.kind, "ready") && !strings.Contains(n.kind, "held") {
				after = append(after, n.String())
			}
		}
		r.check("no-notice-for-a-relaunch-refusal-at-"+clockAt(refusal.at), len(after) == 0, "the refusal %s; notices of a refusal within 45 s: %v", refusal, after)
	}
	alerts := r.fleetAlerts()
	var described []string
	cases := map[string]int{}
	for _, rec := range alerts {
		described = append(described, w5Describe(rec))
		cases[fmt.Sprint(rec.Attrs["case"])]++
		r.check("r6-line-fixed-text-case-project-"+fmt.Sprint(rec.Attrs["case"]), strings.HasPrefix(rec.Message, w5FleetAlertPrefix) && rec.Attrs["case"] != nil && rec.Attrs["project"] == r.cfg.Project,
			"%s", w5Describe(rec))
	}
	r.record("R6 lines: %v", described)
	r.check("r6-once-for-no-live-session", cases[w5CaseNoLive] == 1, "%d lines for %s", cases[w5CaseNoLive], w5CaseNoLive)
	r.check("r6-once-for-the-failed-reads", cases[w5CaseListUnreadable] == 1, "%d lines for %s", cases[w5CaseListUnreadable], w5CaseListUnreadable)
	r.check("r6-no-other-line", len(alerts) == cases[w5CaseNoLive]+cases[w5CaseListUnreadable], "%d lines in all: %v", len(alerts), described)
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
	sleepUntil(t0.Add(w5GuardRelaunch))
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
	after := f.claim(ctl.token)
	gap := relaunched.Sub(last)
	f.recordEmptyReads("controller with a job", t0)
	f.check("controller-relaunched-20m-after-its-last-step", !relaunched.IsZero() && gap >= w5Unreachable && gap <= w5MarkWindow+time.Minute,
		"the daemon relaunched the controller at %s, %s after its last step %s", clockAt(relaunched), gap.Round(time.Second), clockAt(last))
	// The relaunch's own charge is what its stop charged, which the daemon says in its line; the
	// relaunched controller's boot on the path still blocked is charged after it, and the budgets
	// are recorded beside it.
	relaunchLines := f.logs(func(rec loggedRecord) bool {
		return rec.Attrs["claim"] == string(ctl.token) && !rec.at.Before(t0) && strings.HasPrefix(rec.Message, "supervise: relaunching a claim Envoy cannot reach")
	})
	var relaunchLine string
	if len(relaunchLines) > 0 {
		relaunchLine = w5Describe(relaunchLines[0])
	}
	f.check("controller-relaunch-charges-nothing", len(relaunchLines) == 1 && strings.Contains(relaunchLines[0].Message, "the stop charges nothing"),
		"the daemon's relaunch line: %s; the claim's budgets just after, its boot on the path still blocked included: %+v", relaunchLine, after.Budgets)
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
	session := before.session
	t0 := time.Now()
	f.front.blockRegistrations(session)
	f.record("blocked the controller's registrations (session %s) at %s with its launcher reported not connected; it is %s", session, clockAt(t0), before)
	var relaunched time.Time
	var newPod string
	waitFor(w5GuardRelaunch, func() bool {
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
	time.Sleep(time.Minute)
	held := f.claim(ctl.token)
	calls := f.pods.relaunchCalls(ctl.token, t0)
	var described []string
	for _, c := range calls {
		described = append(described, fmt.Sprintf("%s %s may=%t launcher=%q answered %q", clockAt(c.at), c.call, c.mayReplacePod, c.launcher, c.answer))
	}
	f.record("the controller's relaunch calls: %v", described)
	// The relaunch's own charge is what its stop charged, which the daemon says in its line; the
	// relaunched controller's boots that fail on its blocked path are charged after it, and the
	// budgets a minute on are recorded beside it.
	relaunchLines := f.logs(func(rec loggedRecord) bool {
		return rec.Attrs["claim"] == string(ctl.token) && !rec.at.Before(t0) && strings.HasPrefix(rec.Message, "supervise: relaunching a claim Envoy cannot reach")
	})
	var relaunchLine string
	if len(relaunchLines) > 0 {
		relaunchLine = w5Describe(relaunchLines[0])
	}
	f.check("controller-relaunched-in-a-replaced-pod-of-its-own", !relaunched.IsZero() && newPod != "" && newPod != before.pod,
		"relaunched at %s; its pod was %s and its relaunch runs in %s", clockAt(relaunched), before.pod, newPod)
	f.check("controller-launcher-relaunch-charges-nothing", len(relaunchLines) == 1 && strings.Contains(relaunchLines[0].Message, "the stop charges nothing"),
		"the daemon's relaunch line: %s; the claim's budgets a minute on, after its boots on the blocked path: %+v (state %s)", relaunchLine, held.Budgets, held.State)
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
	sleepUntil(t0.Add(w5GuardRelaunch))
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
