//go:build e2e

// Check w4 of this change's verification (R4: a session's registration and delivery come back after
// each end), run on the rig (RIG.md) as the check states it. scripts/e2e/session-reconnect.sh
// builds this driver and runs it one end at a time; with --base-commit it runs every end against
// the base's code, and its stopped heartbeat is the check's negative control.
//
// Each subtest is one end in one state of the session, on a rig of its own: one implementer's real
// pane, which holds its role on the pinned listener through the rig's front. The session is idle
// when the end starts, or in a `wait` call (Oh My Pi's own tool, which a message ends) waiting on a
// background job it started. The ends, the plain ones first and the stand-in broker's last:
//
//   - listener-outage: the front holds every request unanswered for 10 minutes (rigFront.hold);
//   - nats-outage: the nats-server stops for 10 minutes and starts again on its ports and store;
//   - heartbeat-stopped: the plugin's first heartbeat timer stops firing, through the rig's seam
//     ENVOY_HEARTBEAT_STOP_AFTER_MS, which leaves R4's own restart of it free to run at its own
//     5-minute window (ENVOY_REGISTRATION_WATCHDOG_MS is not set);
//   - heartbeat-hung: the front accepts the session's next registration and does not answer it for
//     10 minutes, while it answers everything else (rigFront.hangNext);
//   - renewal-unanswered: the shim runs the real `agent-secrets renew`, which the rig hands its
//     enrollment over the worker stream as the Kubernetes runtime's enroller does, and the stand-in
//     broker does not answer its first renewal (w4Broker).
//
// After each end a numbered message is published to the session's role every 30 s for 15 minutes,
// from the rig's listener directly, starting when the end is over or, for the stopped and the hung
// heartbeat, when it starts; Envoy's session list is read each minute, and, through the listener
// outage, /connz and every registration that reached the front. Before the end, one message is
// published and must reach the transcript: the control that the transcript reader sees a delivery.
// Then the session is stopped cleanly (`legion claims suspend`) and watched past its plugin's own
// 5-minute restart window for a registration.
//
// Every number and time is printed as a note and appended to w4-<end>-<state>.results beside the
// daemon's log, as it is read, so a run that is killed keeps what it read. Every assertion a step
// fails is printed (`violation <assertion>: …`) before the step ends at the first, so a run over
// the base names everything that base did not survive.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The ends, in the order a whole run takes them: the plain ones first, the stand-in broker's last.
const (
	w4ListenerOutage    = "listener-outage"
	w4NATSOutage        = "nats-outage"
	w4HeartbeatStopped  = "heartbeat-stopped"
	w4HeartbeatHung     = "heartbeat-hung"
	w4RenewalUnanswered = "renewal-unanswered"
)

var w4Ends = []string{w4ListenerOutage, w4NATSOutage, w4HeartbeatStopped, w4HeartbeatHung, w4RenewalUnanswered}

// The two states a session meets each end in.
const (
	w4Idle = "idle"
	w4Wait = "wait"
)

// The check's own figures, and the plugin's that it reads against.
const (
	// w4EndLasts is how long the listener and NATS do not answer, and the hung registration is held.
	w4EndLasts = 10 * time.Minute
	// w4Cadence, w4Messages: a numbered message every 30 s for 15 minutes.
	w4Cadence  = 30 * time.Second
	w4Messages = 30
	// w4AfterAnswer: R4's bound after Envoy and NATS answer again; w4AfterHeartbeatEnd: its bound
	// after a stopped or hung heartbeat's end starts.
	w4AfterAnswer       = 3 * time.Minute
	w4AfterHeartbeatEnd = 6 * time.Minute
	// w4Arrival is how long the last message is given to reach the transcript after it was
	// published, on a loaded host, before the transcript is read for the last time.
	w4Arrival = 2 * time.Minute
	// w4HeartbeatInterval is the plugin's heartbeat (DEFAULT_HEARTBEAT_MS, envoy-client's
	// envoy-settings.ts), and w4Watchdog its restart's window (REGISTRATION_WATCHDOG_MS) read every
	// w4WatchdogCheck (watchdogCheckMs).
	w4HeartbeatInterval = 90 * time.Second
	w4Watchdog          = 5 * time.Minute
	w4WatchdogCheck     = 30 * time.Second
	// w4StopAfter is ENVOY_HEARTBEAT_STOP_AFTER_MS: six heartbeat intervals, past the session's
	// start, its probe and its wait, so its sixth tick is the first that does not fire.
	w4StopAfter = 6 * w4HeartbeatInterval
	// w4StopGap is how long no registration must arrive to read the heartbeat as stopped: an
	// interval and a loaded host's lateness.
	w4StopGap = w4HeartbeatInterval + 20*time.Second
	// w4CallSpread groups a listener call's attempts (LISTENER_CALL_ATTEMPTS of
	// LISTENER_CALL_TIMEOUT_MS, LISTENER_CALL_RETRY_DELAY_MS apart, @legion/contracts) into one call.
	w4CallSpread = 7 * time.Second
	// w4RetryCap is the longest a session that retries its registration between heartbeat ticks
	// may go without a call once its retries back off, as the check states it.
	w4RetryCap = 60 * time.Second
	// w4StoppedWatched is how long a cleanly stopped session is watched for a registration: past
	// the restart's whole window and one more reading of it, and a minute.
	w4StoppedWatched = w4Watchdog + w4WatchdogCheck + time.Minute
	// w4Setup bounds each wait of the session's setup, and w4RenewalTimeout is the renewer's own
	// client timeout (leasetime.RenewClientTimeout), which a renewal made again cannot precede.
	w4Setup          = 6 * time.Minute
	w4RenewalTimeout = 30 * time.Second
)

// TestASessionComesBackAfterEachEndLive runs each end in each state on a rig of its own; the
// script runs one subtest per driver run (`-test.run '^…$/^<end>-<state>$'`).
func TestASessionComesBackAfterEachEndLive(t *testing.T) {
	for _, end := range w4Ends {
		for _, state := range []string{w4Idle, w4Wait} {
			name := end + "-" + state
			t.Run(name, func(t *testing.T) {
				r := newW4Run(t, end, state)
				r.step(name, r.driveEnd)
				r.step(name+"-clean-stop", r.cleanStop)
			})
		}
	}
}

// w4Run is one end in one state.
type w4Run struct {
	*rig
	endName, state, label, tag string
	key                        string
	claimToken                 claim.Token
	sessionID, transcriptPath  string
	broker                     *w4Broker
	pod                        w4PodEnrollment
	results                    *os.File
	resultsMu                  sync.Mutex
	// firstRegistration is when the session's first registration reached the front, from which
	// its heartbeat's ticks are counted; instructedAt is when the wait instruction was published.
	firstRegistration time.Time
	instructedAt      time.Time
	violations        []w4Violation
}

type w4Violation struct{ assertion, detail string }

func newW4Run(t *testing.T, end, state string) *w4Run {
	t.Helper()
	r := &w4Run{rig: startRig(t, rigOptions{script: "scripts/e2e/session-reconnect.sh"}), endName: end, state: state, label: end + "-" + state}
	r.tag = "W4-" + strings.ToLower(randomSuffix(t))
	r.key = r.cfg.Project + "-1"
	r.claimToken = r.token(r.key, claim.RoleImplementer)
	results, err := os.OpenFile(filepath.Join(r.evidence, "w4-"+r.label+".results"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the run's results file: %v", err)
	}
	t.Cleanup(func() { _ = results.Close() })
	r.results = results
	t.Cleanup(r.keepOmpLogs)
	// The listener runs as production's does, under a supervisor that starts it again when it
	// exits (testenvoy's RestartOnExit says why the NATS outage needs that).
	r.listener.RestartOnExit(w4ListenerRestartDelay)
	switch end {
	case w4HeartbeatStopped:
		r.runtime.extraEnv = func(spec runtime.SpawnSpec) []string {
			if spec.Claim != r.claimToken {
				return nil
			}
			return []string{fmt.Sprintf("ENVOY_HEARTBEAT_STOP_AFTER_MS=%d", w4StopAfter.Milliseconds())}
		}
	case w4RenewalUnanswered:
		r.broker = newW4Broker(t)
		r.pod = newW4PodEnrollment(t, proofCheckoutRoot(t))
		r.runtime.extraEnv = func(spec runtime.SpawnSpec) []string {
			if spec.Claim != r.claimToken {
				return nil
			}
			return r.pod.env(r.broker.url)
		}
		r.runtime.extraArgs = func(spec runtime.SpawnSpec) []string {
			if spec.Claim != r.claimToken {
				return nil
			}
			return r.pod.args()
		}
		r.note("the stand-in secrets broker at %s; the shim is started with %s, and agent-secrets is built from this checkout's packages/envoy",
			r.broker.url, strings.Join(r.pod.args(), " "))
	}
	r.record("run", "end", end, "state", state, "tag", r.tag)
	r.seed(rigSeed{issue: r.key, phase: phase.Implementing, role: claim.RoleImplementer,
		task: "Reply with the one word READY and nothing else. Call no tool."})
	r.boot()
	r.awaitReady(r.claimToken)
	r.awaitIdle()
	held := r.claim(r.claimToken)
	r.sessionID, r.transcriptPath = held.Session, held.SessionFile
	if r.sessionID == "" || r.transcriptPath == "" {
		t.Fatalf("%s records no session or no transcript: %+v", r.claimToken, held)
	}
	r.awaitRegistered()
	r.note("%s is idle in session %s, transcript %s; its first registration reached the front at %s",
		r.claimToken, r.sessionID, r.transcriptPath, clockAt(r.firstRegistration))
	r.record("session", "claim", string(r.claimToken), "session", r.sessionID, "transcript", r.transcriptPath, "firstRegistration", r.firstRegistration)
	r.probe()
	if state == w4Wait {
		r.enterWait()
	}
	return r
}

// awaitIdle waits for the claim's agent to answer its start's task and stand idle.
func (r *w4Run) awaitIdle() {
	r.t.Helper()
	r.await(string(r.claimToken)+" to answer its task and stand idle", w4Setup, func() bool {
		c := r.claim(r.claimToken)
		if c.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed; its workers' output is under %s", r.claimToken, r.rt.paneLogs)
		}
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})
}

// awaitRegistered waits for the session's first registration at the front, which its heartbeat's
// ticks are counted from.
func (r *w4Run) awaitRegistered() {
	r.t.Helper()
	r.await("the session's registration to reach the front", w4Setup, func() bool {
		for _, a := range r.front.arrivals(r.sessionID) {
			if a.path == "/v1/interests/subscribe" {
				r.firstRegistration = a.at
				return true
			}
		}
		return false
	})
}

// ---- the session's setup -------------------------------------------------------------------------

// probe publishes one message before the end and waits for it in the transcript: the control that
// the reader below sees a delivery at all, so a count of none after the end is not a blind one.
func (r *w4Run) probe() {
	r.t.Helper()
	tag := r.tag + "-P0"
	sent := r.publish(-1, tag, tag+": this is w4's probe before the end. Reply with the one word OK and nothing else. Call no tool.", time.Now())
	if sent.eventID == "" {
		r.t.Fatalf("the probe was refused by the listener: %d %s", sent.status, sent.answer)
	}
	var reached time.Time
	r.await("the probe to reach the transcript", w4Setup, func() bool {
		cards := r.cardsWith(tag)
		if len(cards) > 0 {
			reached = cards[0].at
			return true
		}
		time.Sleep(time.Second)
		return false
	})
	r.awaitIdle()
	r.note("control: the probe %s, published %s (%d, event %s), reached the transcript at %s; the reader sees a delivery",
		tag, clockAt(sent.at), sent.status, sent.eventID, clockAt(reached))
	r.record("probe", "tag", tag, "published", sent.at, "status", sent.status, "event", sent.eventID, "reached", reached)
}

// enterWait has the session start a background job and wait on it, and waits for its open wait.
func (r *w4Run) enterWait() {
	r.t.Helper()
	tag := r.tag + "-WAIT"
	r.instructedAt = time.Now()
	sent := r.publish(-1, tag, tag+": start the shell command `sleep 7200` as a background job (the bash tool with async true). "+
		"Then call the wait tool. Each time wait returns without an Envoy message, call wait again. Do nothing else, and never end your turn "+
		"while you wait, however long it takes, until an Envoy message reaches you. When one does, reply with the one word OK and end your turn. "+
		"Answer every later message with the one word OK and call no tool.", r.instructedAt)
	if sent.eventID == "" {
		r.t.Fatalf("the wait instruction was refused by the listener: %d %s", sent.status, sent.answer)
	}
	r.await("the session to start its wait", w4Setup, func() bool {
		if open, _ := r.openWait(r.instructedAt); open {
			return true
		}
		time.Sleep(time.Second)
		return false
	})
	var names []string
	for _, c := range r.toolCalls(r.instructedAt) {
		names = append(names, c.name)
	}
	r.note("the session is in a wait call; its tool calls since the instruction at %s: %v", clockAt(r.instructedAt), names)
	r.record("wait", "instructed", r.instructedAt, "toolCalls", names)
	if !slices.Contains(names, "bash") {
		r.t.Fatalf("the session waits with no background job of its own: its tool calls since the instruction are %v", names)
	}
}

// ---- the end -------------------------------------------------------------------------------------

// w4Window is what an end leaves the window of messages with: when the messages start, when R4's
// bound starts, and what to call that moment in the notes.
type w4Window struct {
	start, boundary time.Time
	why             string
}

func (r *w4Run) driveEnd() {
	if r.state == w4Wait {
		open, waits := r.openWait(r.instructedAt)
		r.note("as the end is driven the session is in a wait call: %v (%d wait calls since the instruction)", open, waits)
		r.record("wait-at-end", "open", open, "waitCalls", waits)
	}
	var window w4Window
	var outage w4Outage
	switch r.endName {
	case w4ListenerOutage:
		window, outage = r.listenerOutage()
	case w4NATSOutage:
		window = r.natsOutage()
	case w4HeartbeatStopped:
		window = r.heartbeatStopped()
	case w4HeartbeatHung:
		window = r.heartbeatHung()
	case w4RenewalUnanswered:
		window = r.renewalUnanswered()
	}
	if r.state == w4Wait {
		open, waits := r.openWait(r.instructedAt)
		r.note("as the window begins the session is in a wait call: %v (%d wait calls since the instruction)", open, waits)
		r.record("wait-at-window", "open", open, "waitCalls", waits)
	}
	pubs, reads := r.runWindow(window)
	r.noteListenerRestarts()
	r.judge(window, pubs, reads, outage)
	if r.state == w4Wait {
		r.judgeWait(window, pubs)
	}
	switch r.endName {
	case w4ListenerOutage:
		r.judgeOutage(outage)
	case w4RenewalUnanswered:
		r.judgeRenewal()
	}
	r.verdict()
}

// w4Outage is what was read through the listener outage: the /connz rows of the session's NATS
// connection at each read, and the registration arrivals through it.
type w4Outage struct {
	from, to time.Time
	before   []rigConnz
	connz    []w4ConnzRead
	// lastBefore is the last registration that reached the front before the outage: the phase of
	// the heartbeat's ticks.
	lastBefore time.Time
}

type w4ConnzRead struct {
	at      time.Time
	session []rigConnz
	members int
}

func (r *w4Run) listenerOutage() (w4Window, w4Outage) {
	o := w4Outage{before: r.connzRead().session}
	for _, a := range r.front.arrivals(r.sessionID) {
		if a.path == "/v1/interests/subscribe" {
			o.lastBefore = a.at
		}
	}
	o.from = time.Now()
	r.front.hold()
	r.note("the listener's front holds every request from %s; the session's NATS connection before it: %s; its last registration before it at %s",
		clockAt(o.from), describeConnz(o.before), clockAt(o.lastBefore))
	r.record("end", "kind", r.endName, "from", o.from, "connzBefore", describeConnz(o.before), "lastRegistration", o.lastBefore)
	for k := 1; ; k++ {
		next := o.from.Add(time.Duration(k) * time.Minute)
		if next.After(o.from.Add(w4EndLasts)) {
			break
		}
		w4SleepUntil(next)
		o.connz = append(o.connz, r.connzRead())
		r.readSessions()
	}
	w4SleepUntil(o.from.Add(w4EndLasts))
	r.front.answer()
	o.to = time.Now()
	o.connz = append(o.connz, r.connzRead())
	r.note("the front answers again from %s (%s held)", clockAt(o.to), o.to.Sub(o.from).Round(time.Second))
	r.record("end", "kind", r.endName, "to", o.to)
	return w4Window{start: o.to, boundary: o.to.Add(w4AfterAnswer), why: "3 minutes after the listener answered again"}, o
}

func (r *w4Run) natsOutage() w4Window {
	from := time.Now()
	r.nats.stop()
	stopped := time.Now()
	r.note("the nats-server stopped at %s (asked at %s)", clockAt(stopped), clockAt(from))
	r.record("end", "kind", r.endName, "from", from, "stopped", stopped)
	for k := 1; ; k++ {
		next := from.Add(time.Duration(k) * time.Minute)
		if next.After(from.Add(w4EndLasts)) {
			break
		}
		w4SleepUntil(next)
		r.readSessions()
	}
	w4SleepUntil(from.Add(w4EndLasts))
	r.nats.start()
	natsBack := time.Now()
	r.note("the nats-server is ready again at %s, on the same ports and store", clockAt(natsBack))
	var listenerBack time.Time
	r.await("the listener to answer its session list again", w4Setup, func() bool {
		if read := r.readSessionsQuiet(); read.status == http.StatusOK {
			listenerBack = read.at
			return true
		}
		time.Sleep(time.Second)
		return false
	})
	r.note("the listener answers its session list again at %s", clockAt(listenerBack))
	r.record("end", "kind", r.endName, "natsBack", natsBack, "listenerBack", listenerBack)
	r.noteListenerRestarts()
	return w4Window{start: natsBack, boundary: listenerBack.Add(w4AfterAnswer), why: "3 minutes after NATS and the listener answered again"}
}

func (r *w4Run) heartbeatStopped() w4Window {
	r.t.Helper()
	nominal := r.firstRegistration.Add(w4StopAfter)
	r.note("the seam stops the session's first heartbeat %s after it was armed: about %s, counted from its first registration", w4StopAfter, clockAt(nominal))
	if time.Now().After(nominal.Add(-w4HeartbeatInterval)) {
		r.fails("setup-before-stop", "the session's setup ran until %s, past %s, an interval before its heartbeat stops", clockAt(time.Now()), clockAt(nominal.Add(-w4HeartbeatInterval)))
	}
	var last time.Time
	r.await("the heartbeat to stop registering", w4StopAfter+w4Setup, func() bool {
		last = time.Time{}
		for _, a := range r.front.arrivals(r.sessionID) {
			if a.path == "/v1/interests/subscribe" {
				last = a.at
			}
		}
		now := time.Now()
		if now.Before(nominal.Add(-w4HeartbeatInterval)) || now.Sub(last) < w4StopGap {
			time.Sleep(time.Second)
			return false
		}
		return true
	})
	start := last.Add(w4HeartbeatInterval)
	r.note("the heartbeat stopped: its last registration reached the front at %s, and its next tick, at about %s, registered nothing (%s after the first registration)",
		clockAt(last), clockAt(start), start.Sub(r.firstRegistration).Round(time.Second))
	r.record("end", "kind", r.endName, "lastRegistration", last, "from", start, "nominal", nominal)
	return w4Window{start: start, boundary: start.Add(w4AfterHeartbeatEnd), why: "6 minutes after the heartbeat stopped"}
}

func (r *w4Run) heartbeatHung() w4Window {
	r.t.Helper()
	armed := time.Now()
	r.front.hangNext(r.sessionID, w4EndLasts)
	var hung rigFrontArrival
	r.await("the session's next registration to reach the front and hang", w4HeartbeatInterval+w4Setup, func() bool {
		for _, a := range r.front.arrivals(r.sessionID) {
			if a.hung {
				hung = a
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
		return false
	})
	r.note("the front holds the session's registration that reached it at %s unanswered for up to %s, and answers everything else (armed at %s)",
		clockAt(hung.at), w4EndLasts, clockAt(armed))
	r.record("end", "kind", r.endName, "from", hung.at, "armed", armed)
	return w4Window{start: hung.at, boundary: hung.at.Add(w4AfterHeartbeatEnd), why: "6 minutes after the heartbeat's connection hung"}
}

func (r *w4Run) renewalUnanswered() w4Window {
	r.t.Helper()
	conn, ok := r.conn(r.claimToken)
	if !ok {
		r.t.Fatalf("the daemon holds no worker-stream connection of %s to hand the enrollment over", r.claimToken)
	}
	enrollment := "enr-" + strings.ToLower(randomSuffix(r.t))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := conn.AgentSecretsEnrollment(ctx, enrollment)
	cancel()
	if err != nil {
		r.t.Fatalf("hand %s its enrollment %s: %v", r.claimToken, enrollment, err)
	}
	handed := time.Now()
	var held w4Renewal
	r.await("the renewer's first renewal to reach the stand-in and its renewer to give up on it", w4Setup, func() bool {
		for _, renewal := range r.broker.received() {
			if renewal.held && !renewal.ended.IsZero() {
				held = renewal
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
		return false
	})
	r.note("the rig handed %s its enrollment %s over the worker stream at %s; its first renewal reached the stand-in at %s, which did not answer it; the renewer gave up on it at %s (%s)",
		r.claimToken, enrollment, clockAt(handed), clockAt(held.at), clockAt(held.ended), held.ended.Sub(held.at).Round(100*time.Millisecond))
	r.record("end", "kind", r.endName, "enrollment", enrollment, "handed", handed, "from", held.at, "to", held.ended)
	return w4Window{start: held.ended, boundary: held.ended.Add(w4AfterAnswer), why: "3 minutes after the unanswered renewal ended"}
}

// ---- the window ----------------------------------------------------------------------------------

// w4Publication is one numbered message: when it was meant to go and went, the listener's answer,
// and the event id it was stored under.
type w4Publication struct {
	n               int
	tag             string
	scheduled, at   time.Time
	status          int
	eventID, answer string
	took            time.Duration
}

// runWindow publishes the numbered messages and reads Envoy's session list each minute, and
// answers both once the last message has had its time to arrive.
func (r *w4Run) runWindow(window w4Window) ([]w4Publication, []w4SessionRead) {
	r.note("the window: messages from %s every %s for %s; R4's bound from %s (%s)",
		clockAt(window.start), w4Cadence, w4Cadence*w4Messages, clockAt(window.boundary), window.why)
	r.record("window", "start", window.start, "boundary", window.boundary, "why", window.why)
	pubs := make([]w4Publication, w4Messages)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var each sync.WaitGroup
		for k := range w4Messages {
			scheduled := window.start.Add(time.Duration(k) * w4Cadence)
			w4SleepUntil(scheduled)
			each.Add(1)
			go func(k int) {
				defer each.Done()
				tag := fmt.Sprintf("%s-%02d", r.tag, k+1)
				pubs[k] = r.publish(k+1, tag, fmt.Sprintf("%s: this is w4's numbered message %d of %d. Reply with the one word OK and nothing else. Call no tool.", tag, k+1, w4Messages), scheduled)
			}(k)
		}
		each.Wait()
	}()
	var reads []w4SessionRead
	for k := 0; ; k++ {
		at := window.start.Add(time.Duration(k) * time.Minute)
		if at.After(window.start.Add(w4Cadence * w4Messages)) {
			break
		}
		w4SleepUntil(at)
		reads = append(reads, r.readSessions())
		if r.endName == w4ListenerOutage {
			r.connzRead()
		}
	}
	wg.Wait()
	last := window.start
	for _, p := range pubs {
		if p.at.After(last) {
			last = p.at
		}
	}
	w4SleepUntil(last.Add(w4Arrival))
	reads = append(reads, r.readSessions())
	return pubs, reads
}

// publish posts one message to the session's role at the rig's listener, directly: the
// listener's own publish route, with the listener's bearer, as a peer's envoy_publish reaches it.
func (r *w4Run) publish(n int, tag, text string, scheduled time.Time) w4Publication {
	p := w4Publication{n: n, tag: tag, scheduled: scheduled}
	// No source: the listener stamps "agent", as it does a peer's publish.
	body, _ := json.Marshal(map[string]any{"topic": "notifications.role." + string(r.claimToken), "message": text})
	req, err := http.NewRequest(http.MethodPost, r.listener.BaseURL()+"/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		p.answer = err.Error()
		return p
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.listener.Token())
	client := &http.Client{Timeout: time.Minute, Transport: w4FreshConnections}
	p.at = time.Now()
	resp, err := client.Do(req)
	p.took = time.Since(p.at)
	if err != nil {
		p.answer = err.Error()
	} else {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		p.status = resp.StatusCode
		var envelope struct {
			EventID string `json:"event_id"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.EventID != "" {
			p.eventID = envelope.EventID
		}
		if p.status/100 != 2 || p.eventID == "" {
			p.answer = strings.TrimSpace(string(raw))
		}
	}
	r.record("publish", "n", n, "tag", tag, "scheduled", scheduled, "at", p.at, "status", p.status, "event", p.eventID, "answer", p.answer, "tookMs", p.took.Milliseconds())
	return p
}

// w4SessionRead is one read of Envoy's session list: the session's row, or why there was none.
type w4SessionRead struct {
	at       time.Time
	status   int
	state    string
	lastSeen time.Time
	roles    []string
	topics   []string
	failure  string
}

func (s w4SessionRead) describe() string {
	switch {
	case s.failure != "":
		return "unreadable (" + s.failure + ")"
	case s.state == "":
		return "absent"
	}
	return fmt.Sprintf("%s, last seen %s, roles %v", s.state, clockAt(s.lastSeen), s.roles)
}

// readSessions reads Envoy's session list, unresponsive sessions included, from the listener
// itself, and records and prints the session's row.
func (r *w4Run) readSessions() w4SessionRead {
	read := r.readSessionsQuiet()
	r.note("read %s: the session list says %s", clockAt(read.at), read.describe())
	r.record("sessions", "at", read.at, "status", read.status, "state", read.state, "lastSeen", read.lastSeen, "roles", read.roles, "failure", read.failure)
	return read
}

func (r *w4Run) readSessionsQuiet() w4SessionRead {
	read := w4SessionRead{at: time.Now()}
	req, err := http.NewRequest(http.MethodGet, r.listener.BaseURL()+"/v1/sessions?include=unresponsive", nil)
	if err != nil {
		read.failure = err.Error()
		return read
	}
	req.Header.Set("Authorization", "Bearer "+r.listener.Token())
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: w4FreshConnections}).Do(req)
	if err != nil {
		read.failure = err.Error()
		return read
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	read.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		read.failure = fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		return read
	}
	var rows []struct {
		SessionID string   `json:"session_id"`
		State     string   `json:"state"`
		LastSeen  int64    `json:"last_seen"`
		Roles     []string `json:"roles"`
		Topics    []string `json:"topics"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		read.failure = "decode: " + err.Error()
		return read
	}
	for _, row := range rows {
		if row.SessionID == r.sessionID {
			read.state, read.roles, read.topics = row.State, row.Roles, row.Topics
			read.lastSeen = time.UnixMilli(row.LastSeen)
		}
	}
	return read
}

// connzRead is /connz's rows of the session's own NATS connection (the plugin names it
// omp-<session>), and how many members connections there are, printed and recorded.
func (r *w4Run) connzRead() w4ConnzRead {
	read := w4ConnzRead{at: time.Now()}
	for _, c := range r.nats.connections() {
		if r.nats.userOf(c) == "members" {
			read.members++
		}
		if c.Name == "omp-"+r.sessionID {
			read.session = append(read.session, c)
		}
	}
	r.note("read %s: /connz holds %s for the session, %d members connections", clockAt(read.at), describeConnz(read.session), read.members)
	r.record("connz", "at", read.at, "session", describeConnz(read.session), "members", read.members)
	return read
}

func describeConnz(rows []rigConnz) string {
	if len(rows) == 0 {
		return "no connection"
	}
	parts := make([]string, 0, len(rows))
	for _, c := range rows {
		parts = append(parts, fmt.Sprintf("cid %d (opened %s)", c.CID, c.Start))
	}
	return strings.Join(parts, ", ")
}

// ---- the transcript ------------------------------------------------------------------------------

// w4Entry is one transcript entry as this check reads it: a card the plugin wrote
// (`custom_message` of type `envoy-message`), or a message, whose tool calls and tool results it
// reads.
type w4Entry struct {
	Type       string          `json:"type"`
	CustomType string          `json:"customType"`
	Content    json.RawMessage `json:"content"`
	Details    json.RawMessage `json:"details"`
	Timestamp  time.Time       `json:"timestamp"`
	Message    *struct {
		Role       string          `json:"role"`
		ToolName   string          `json:"toolName"`
		ToolCallID string          `json:"toolCallId"`
		Content    json.RawMessage `json:"content"`
		Details    json.RawMessage `json:"details"`
	} `json:"message"`
}

// w4Card is one card of the transcript: where in it, when, its text and the event it carried.
type w4Card struct {
	index   int
	at      time.Time
	text    string
	eventID string
}

func (r *w4Run) entries() []w4Entry {
	r.t.Helper()
	file, err := os.Open(r.transcriptPath)
	if err != nil {
		r.t.Fatalf("open the session's transcript %s: %v", r.transcriptPath, err)
	}
	defer file.Close()
	var entries []w4Entry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64<<20)
	for scanner.Scan() {
		var entry w4Entry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		r.t.Fatalf("read the session's transcript %s: %v", r.transcriptPath, err)
	}
	return entries
}

// cards is every card of the transcript, in its order.
func (r *w4Run) cards() []w4Card {
	var cards []w4Card
	for i, e := range r.entries() {
		if e.Type != "custom_message" || e.CustomType != "envoy-message" {
			continue
		}
		var details struct {
			EventID string `json:"event_id"`
		}
		_ = json.Unmarshal(e.Details, &details)
		cards = append(cards, w4Card{index: i, at: e.Timestamp, text: w4Text(e.Content), eventID: details.EventID})
	}
	return cards
}

// cardsWith is every card carrying tag.
func (r *w4Run) cardsWith(tag string) []w4Card {
	var out []w4Card
	for _, c := range r.cards() {
		if strings.Contains(c.text, tag+":") {
			out = append(out, c)
		}
	}
	return out
}

// w4Text is an entry's content as text: a string, or the text of each block.
func w4Text(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		b.WriteString(block.Text)
	}
	return b.String()
}

// w4ToolCall is one call of a tool the transcript holds, and its result if it has one.
type w4ToolCall struct {
	index, resultIndex int
	name, id           string
	at, resultAt       time.Time
	interrupted        bool
	resultText         string
}

// toolCalls is every tool call of the transcript after since, in its order.
func (r *w4Run) toolCalls(since time.Time) []w4ToolCall {
	entries := r.entries()
	var calls []w4ToolCall
	byID := map[string]int{}
	for i, e := range entries {
		if e.Type != "message" || e.Message == nil || e.Timestamp.Before(since) {
			continue
		}
		switch e.Message.Role {
		case "assistant":
			var blocks []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal(e.Message.Content, &blocks)
			for _, b := range blocks {
				if b.Type == "toolCall" {
					byID[b.ID] = len(calls)
					calls = append(calls, w4ToolCall{index: i, resultIndex: -1, name: b.Name, id: b.ID, at: e.Timestamp})
				}
			}
		case "toolResult":
			at, ok := byID[e.Message.ToolCallID]
			if !ok {
				continue
			}
			var details struct {
				Interrupted bool `json:"interrupted"`
			}
			_ = json.Unmarshal(e.Message.Details, &details)
			calls[at].resultIndex, calls[at].resultAt, calls[at].interrupted = i, e.Timestamp, details.Interrupted
			calls[at].resultText = w4Text(e.Message.Content)
		}
	}
	return calls
}

// openWait answers whether the session's last tool call since since is a wait with no result yet,
// and how many wait calls it made since.
func (r *w4Run) openWait(since time.Time) (bool, int) {
	calls := r.toolCalls(since)
	waits := 0
	for _, c := range calls {
		if c.name == "wait" {
			waits++
		}
	}
	if len(calls) == 0 {
		return false, 0
	}
	last := calls[len(calls)-1]
	return last.name == "wait" && last.resultIndex < 0, waits
}

// ---- the judgements ------------------------------------------------------------------------------

// violate records an assertion the run failed, which the step ends at once every judgement is in.
func (r *w4Run) violate(assertion, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	r.violations = append(r.violations, w4Violation{assertion, detail})
	fmt.Printf("   violation %s: %s\n", assertion, detail)
	r.record("violation", "assertion", assertion, "detail", detail)
}

// verdict ends the step at its first violation, every one of them printed already.
func (r *w4Run) verdict() {
	if len(r.violations) == 0 {
		return
	}
	first := r.violations[0]
	r.fails(first.assertion, "%s (%d violations in all)", first.detail, len(r.violations))
}

// judge reads the transcript for each message and holds the window's three outcomes: every message
// from the bound reached the session once, under the same session and role, and the session read
// live at every read from the bound.
func (r *w4Run) judge(window w4Window, pubs []w4Publication, reads []w4SessionRead, outage w4Outage) {
	held := r.claim(r.claimToken)
	cards := r.cards()
	var bound, once, never, twice, refused []string
	for _, p := range pubs {
		var reached []w4Card
		for _, c := range cards {
			if strings.Contains(c.text, p.tag+":") {
				reached = append(reached, c)
			}
		}
		first := time.Time{}
		if len(reached) > 0 {
			first = reached[0].at
		}
		events := make([]string, 0, len(reached))
		for _, c := range reached {
			events = append(events, c.eventID)
		}
		r.note("message %02d: published %s (meant %s), answered %d %s in %s, event %s; reached the transcript %s, %d time(s)%s",
			p.n, clockAt(p.at), clockAt(p.scheduled), p.status, w4Short(p.answer), p.took.Round(time.Millisecond), p.eventID,
			clockAt(first), len(reached), w4Latency(p.at, first))
		r.record("message", "n", p.n, "tag", p.tag, "published", p.at, "status", p.status, "event", p.eventID, "reached", first, "times", len(reached), "events", events)
		if p.at.Before(window.boundary) {
			continue
		}
		bound = append(bound, fmt.Sprintf("%02d", p.n))
		switch {
		case p.eventID == "":
			refused = append(refused, fmt.Sprintf("%02d (%d %s)", p.n, p.status, w4Short(p.answer)))
		case len(reached) == 0:
			never = append(never, fmt.Sprintf("%02d", p.n))
		case len(reached) > 1:
			twice = append(twice, fmt.Sprintf("%02d (%d times)", p.n, len(reached)))
		default:
			once = append(once, fmt.Sprintf("%02d", p.n))
			if p.eventID != "" && reached[0].eventID != p.eventID {
				r.violate("same-session-and-role", "message %02d reached the session as event %s, not the event %s it was published to the role under", p.n, reached[0].eventID, p.eventID)
			}
		}
	}
	r.note("of the %d messages published from %s (%s), %d reached the session once, %d never, %d more than once, %d were refused by the listener",
		len(bound), clockAt(window.boundary), window.why, len(once), len(never), len(twice), len(refused))
	r.record("delivery", "bound", len(bound), "once", len(once), "never", never, "twice", twice, "refused", refused)
	if len(bound) == 0 {
		r.violate("delivery", "no message was published from %s, so the window says nothing", clockAt(window.boundary))
	} else if len(once) != len(bound) {
		r.violate("delivery", "of the %d messages published from %s (%s), %d reached the session within the window, %d of them more than once %v; never: %v; refused by the listener: %v",
			len(bound), clockAt(window.boundary), window.why, len(once)+len(twice), len(twice), twice, never, refused)
	}
	if held.Session != r.sessionID || held.SessionFile != r.transcriptPath {
		r.violate("same-session-and-role", "the claim's session is %s (%s) after the end, not %s (%s)", held.Session, held.SessionFile, r.sessionID, r.transcriptPath)
	}
	var notLive []string
	counted := 0
	for _, read := range reads {
		if read.at.Before(window.boundary) {
			continue
		}
		counted++
		if read.state != "live" {
			notLive = append(notLive, clockAt(read.at)+" "+read.describe())
			continue
		}
		if !slices.Contains(read.roles, string(r.claimToken)) && !slices.Contains(read.topics, "notifications.role."+string(r.claimToken)) {
			notLive = append(notLive, clockAt(read.at)+" live without its role: roles "+fmt.Sprint(read.roles))
		}
	}
	if counted == 0 {
		r.violate("live", "no read of the session list fell after %s", clockAt(window.boundary))
	} else if len(notLive) > 0 {
		r.violate("live", "of %d reads from %s, %d did not read the session live holding its role: %s", counted, clockAt(window.boundary), len(notLive), strings.Join(notLive, "; "))
	} else {
		r.note("the session read live, holding its role, at all %d reads from %s", counted, clockAt(window.boundary))
	}
}

// judgeWait holds that the session in its wait call ended that wait on the first message that
// reached it after the end began: its last tool call before that card was a wait, and it made no
// further wait call before the next card.
func (r *w4Run) judgeWait(window w4Window, pubs []w4Publication) {
	cards := r.cards()
	first := -1
	for i, c := range cards {
		if c.at.Before(r.instructedAt) {
			continue
		}
		for _, p := range pubs {
			if strings.Contains(c.text, p.tag+":") {
				first = i
				break
			}
		}
		if first >= 0 {
			break
		}
	}
	if first < 0 {
		r.violate("wait-ended", "no numbered message reached the session, so its wait call did not end on one")
		return
	}
	card := cards[first]
	nextCard := -1
	if first+1 < len(cards) {
		nextCard = cards[first+1].index
	}
	var before *w4ToolCall
	waitsAfter := 0
	calls := r.toolCalls(r.instructedAt)
	for i := range calls {
		c := calls[i]
		if c.index < card.index {
			before = &calls[i]
			continue
		}
		if c.name == "wait" && (nextCard < 0 || c.index < nextCard) {
			waitsAfter++
		}
	}
	switch {
	case before == nil || before.name != "wait":
		name := "none"
		if before != nil {
			name = before.name
		}
		r.violate("wait-ended", "the first message to reach the session (%s, at %s) found it out of its wait loop: its last tool call before it was %s",
			w4CardTag(card.text), clockAt(card.at), name)
	case waitsAfter > 0:
		r.violate("wait-ended", "the session called wait %d more times after the first message reached it (%s, at %s)", waitsAfter, w4CardTag(card.text), clockAt(card.at))
	default:
		how := "it was between two waits, and made no further one"
		if before.resultIndex > card.index || before.resultIndex < 0 || before.interrupted {
			how = fmt.Sprintf("its open wait call (from %s) ended %s: %q", clockAt(before.at), clockAt(before.resultAt), w4Short(before.resultText))
		}
		r.note("the session in its wait call ended its wait on the first message that reached it, %s at %s: %s", w4CardTag(card.text), clockAt(card.at), how)
		r.record("wait-ended", "card", w4CardTag(card.text), "at", card.at, "waitFrom", before.at, "waitEnded", before.resultAt, "interrupted", before.interrupted)
	}
}

// judgeOutage holds the outage-only outcomes: the session's NATS connection kept its id and no new
// one opened; the session's registration calls numbered at most two in each heartbeat interval
// (its heartbeat's and the one extra a failed tick buys); and, where the session retries between
// its ticks, a call came at least every w4RetryCap from its first through the outage's end.
func (r *w4Run) judgeOutage(o w4Outage) {
	cids := map[uint64]bool{}
	for _, c := range o.before {
		cids[c.CID] = true
	}
	if len(o.before) != 1 {
		r.violate("nats-connection-kept", "before the outage /connz held %s for the session, want exactly one connection", describeConnz(o.before))
	}
	var changed []string
	for _, read := range o.connz {
		if len(read.session) != 1 || !cids[read.session[0].CID] {
			changed = append(changed, clockAt(read.at)+" "+describeConnz(read.session))
		}
	}
	if len(changed) > 0 {
		r.violate("nats-connection-kept", "through the listener outage the session's NATS connection was not the one it had (%s) at: %s", describeConnz(o.before), strings.Join(changed, "; "))
	} else {
		r.note("through the listener outage and at its end the session's NATS connection kept its id (%s), at all %d reads, and no new one opened", describeConnz(o.before), len(o.connz))
	}
	// The calls: arrivals of the session's registration through the outage, an attempt and its
	// retry grouped into one call.
	var calls [][]rigFrontArrival
	for _, a := range r.front.arrivals(r.sessionID) {
		if a.path != "/v1/interests/subscribe" || a.at.Before(o.from) || a.at.After(o.to) {
			continue
		}
		if n := len(calls); n > 0 && a.at.Sub(calls[n-1][len(calls[n-1])-1].at) <= w4CallSpread {
			calls[n-1] = append(calls[n-1], a)
			continue
		}
		calls = append(calls, []rigFrontArrival{a})
	}
	phase := o.lastBefore
	if phase.IsZero() {
		phase = o.from
	}
	// Each interval starts a little before the heartbeat's tick, which a loaded host only delays.
	const early = 5 * time.Second
	counts := map[int]int{}
	var lines []string
	var gaps []time.Duration
	for i, call := range calls {
		k := int(call[0].at.Sub(phase.Add(-early)) / w4HeartbeatInterval)
		counts[k]++
		lines = append(lines, fmt.Sprintf("%s (%d attempt(s), interval %d)", clockAt(call[0].at), len(call), k))
		if i > 0 {
			gaps = append(gaps, call[0].at.Sub(calls[i-1][0].at))
		}
	}
	r.note("through the outage (%s to %s) the session made %d registration calls, intervals counted from its last tick before it at %s: %s",
		clockAt(o.from), clockAt(o.to), len(calls), clockAt(phase), strings.Join(lines, ", "))
	longest := time.Duration(0)
	for _, g := range gaps {
		longest = max(longest, g)
	}
	tail := time.Duration(0)
	if n := len(calls); n > 0 {
		tail = o.to.Sub(calls[n-1][0].at)
	}
	r.note("the longest gap between two of those calls was %s, and the last of them came %s before the front answered again",
		longest.Round(100*time.Millisecond), tail.Round(100*time.Millisecond))
	r.record("registrations", "calls", lines, "longestGap", longest.String(), "tail", tail.String())
	if len(calls) == 0 {
		r.violate("registration-cadence", "no registration call reached the front through the outage, so the session tried nothing")
	}
	retries := false
	for k, n := range counts {
		if n > 2 {
			r.violate("registration-cadence", "interval %d of the outage held %d registration calls, more than the heartbeat's and one more: %s", k, n, strings.Join(lines, ", "))
		}
		retries = retries || n > 1
	}
	switch {
	case len(calls) == 0:
	case !retries:
		r.note("no interval of the outage held more than the heartbeat's own call: the session retries nothing between its ticks, so the %s cap on a retry's back-off does not apply", w4RetryCap)
	case longest > w4RetryCap || tail > w4RetryCap:
		r.violate("registration-cap", "the session retries between its heartbeat's ticks, but went longer than %s without a registration call through the outage: its longest gap between two calls was %s, and its last call came %s before the front answered again: %s",
			w4RetryCap, longest.Round(100*time.Millisecond), tail.Round(100*time.Millisecond), strings.Join(lines, ", "))
	default:
		r.note("the session retries between its heartbeat's ticks, and made a registration call at least every %s from its first call through the outage's end", w4RetryCap)
	}
}

// judgeRenewal holds that the shim renewed again after the renewal the stand-in did not answer.
func (r *w4Run) judgeRenewal() {
	renewals := r.broker.received()
	lines := make([]string, 0, len(renewals))
	var held, next *w4Renewal
	for i := range renewals {
		x := &renewals[i]
		lines = append(lines, fmt.Sprintf("%s (%s, answered %v)", clockAt(x.at), x.enrollment, x.answered))
		if x.held {
			held = x
		} else if held != nil && next == nil {
			next = x
		}
	}
	r.note("the stand-in broker got %d renewals: %s", len(renewals), strings.Join(lines, ", "))
	launch := r.runtime.current(r.claimToken)
	shimLines := w4ShimLines(r.t, launch.pane.log)
	r.note("the pane's agent-secrets lines:\n%s", indentOutput(strings.Join(shimLines, "\n")))
	r.record("renewals", "renewals", lines, "shim", shimLines)
	switch {
	case held == nil:
		r.violate("renewed-again", "no renewal reached the stand-in")
	case next == nil:
		r.violate("renewed-again", "no renewal reached the stand-in after the one it did not answer at %s", clockAt(held.at))
	case next.at.Sub(held.at) < w4RenewalTimeout-time.Second:
		r.violate("renewed-again", "the next renewal came %s after the unanswered one, before that one could have timed out", next.at.Sub(held.at))
	default:
		r.note("the shim renewed again at %s, %s after the renewal the stand-in did not answer (which its renewer gave up on at %s), and that renewal was answered: %v",
			clockAt(next.at), next.at.Sub(held.at).Round(100*time.Millisecond), clockAt(held.ended), next.answered)
	}
}

// ---- the clean stop ------------------------------------------------------------------------------

// cleanStop stops the session as an operator does and watches it past its plugin's own restart
// window: nothing registers it again, and Envoy does not list it live.
func (r *w4Run) cleanStop() {
	launch := r.runtime.current(r.claimToken)
	asked := time.Now()
	r.note("%s", r.legionClaims("suspend", "--claim", string(r.claimToken)))
	select {
	case <-launch.pane.done:
	case <-time.After(time.Minute):
		r.fails("stopped", "the session's process (pid %d) still ran a minute after the suspension", launch.pane.pid)
	}
	stopped := time.Now()
	r.note("the session's process (pid %d) ended at %s, %s after the suspension was asked", launch.pane.pid, clockAt(stopped), stopped.Sub(asked).Round(time.Millisecond))
	r.record("stop", "asked", asked, "stopped", stopped)
	for k := 1; ; k++ {
		at := stopped.Add(time.Duration(k) * time.Minute)
		if at.After(stopped.Add(w4StoppedWatched)) {
			break
		}
		w4SleepUntil(at)
		r.readSessions()
	}
	w4SleepUntil(stopped.Add(w4StoppedWatched))
	last := r.readSessions()
	var after []string
	for _, a := range r.front.arrivals(r.sessionID) {
		if a.at.After(stopped) {
			after = append(after, clockAt(a.at)+" "+a.path)
		}
	}
	r.record("stopped", "registrationsAfter", after, "lastRead", last.describe())
	if len(after) > 0 {
		r.fails("no-registration-after-stop", "the cleanly stopped session registered %d times in the %s after its process ended at %s: %s",
			len(after), w4StoppedWatched, clockAt(stopped), strings.Join(after, ", "))
	}
	if last.state == "live" {
		r.fails("no-registration-after-stop", "Envoy lists the cleanly stopped session live %s after it stopped: %s", w4StoppedWatched, last.describe())
	}
	r.note("the cleanly stopped session made no registration in the %s after its process ended at %s, past its plugin's %s restart window; Envoy lists it %s",
		w4StoppedWatched, clockAt(stopped), w4Watchdog, last.describe())
}

// ---- small things --------------------------------------------------------------------------------

// record appends one line of what the run read to its results file, at once, so a run that is
// killed keeps it: the kind, then each name and value.
func (r *w4Run) record(kind string, pairs ...any) {
	fields := map[string]any{"recordedAt": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind, "run": r.label}
	for i := 0; i+1 < len(pairs); i += 2 {
		value := pairs[i+1]
		if at, ok := value.(time.Time); ok {
			if at.IsZero() {
				value = nil
			} else {
				value = at.UTC().Format(time.RFC3339Nano)
			}
		}
		fields[fmt.Sprint(pairs[i])] = value
	}
	line, err := json.Marshal(fields)
	if err != nil {
		line = []byte(fmt.Sprintf(`{"kind":"unencodable","error":%q}`, err.Error()))
	}
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()
	_, _ = r.results.Write(append(line, '\n'))
	_ = r.results.Sync()
}

// keepOmpLogs copies Oh My Pi's own logs of the run's profile, where the plugins' warnings are, to
// the evidence before the run's directory goes.
func (r *w4Run) keepOmpLogs() {
	keep := filepath.Join(r.evidence, "omp-logs-"+r.label)
	_ = filepath.WalkDir(filepath.Join(r.home, ".omp"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".log") || !strings.Contains(path, string(filepath.Separator)+"logs"+string(filepath.Separator)) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		target := filepath.Join(keep, strings.ReplaceAll(strings.TrimPrefix(path, r.home+string(filepath.Separator)), string(filepath.Separator), "_"))
		if os.MkdirAll(keep, 0o700) == nil {
			_ = os.WriteFile(target, raw, 0o600)
		}
		return nil
	})
}

// w4FreshConnections sends each of the check's own calls to the listener on a connection of its
// own. The listener closes a keep-alive connection idle for 60 s (IdleTimeout, cmd/listener/main.go
// at testenvoy.PinnedRevision), and the check calls it on a 30 s and a 60 s clock: on 2026-11-16
// eight publications of four head runs went out on a connection the listener was closing and failed
// `connection reset by peer` or EOF, received by nobody. A connection per call is never one the
// listener is closing.
var w4FreshConnections = &http.Transport{DisableKeepAlives: true}

// w4ListenerRestartDelay is the Kubernetes kubelet's first back-off before it starts an exited
// container again (restartPolicy Always), which production's listener Deployment runs under.
const w4ListenerRestartDelay = 10 * time.Second

// noteListenerRestarts prints and records every exit of the listener and its restart, with the
// lines of its own output that say why it ended.
func (r *w4Run) noteListenerRestarts() {
	restarts := r.listener.Restarts()
	for _, x := range restarts {
		r.note("the listener exited at %s (%s); started again at %s, healthy at %s, error %v",
			clockAt(x.Exited), x.State, clockAt(x.Started), clockAt(x.Healthy), x.Err)
		r.record("listener-restart", "exited", x.Exited, "state", x.State, "started", x.Started, "healthy", x.Healthy, "error", fmt.Sprint(x.Err))
	}
	if len(restarts) == 0 {
		r.note("the listener did not exit")
		return
	}
	var why []string
	for _, line := range strings.Split(r.listener.Output(), "\n") {
		if strings.Contains(line, "terminating for restart") || strings.Contains(line, "fatal error") || strings.Contains(line, "shutting down") {
			why = append(why, line)
		}
	}
	r.note("the listener's own lines on its exits:\n%s", indentOutput(strings.Join(why, "\n")))
	r.record("listener-exit-lines", "lines", why)
}

func w4SleepUntil(at time.Time) {
	if d := time.Until(at); d > 0 {
		time.Sleep(d)
	}
}

func w4Short(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 160 {
		return text[:160] + "…"
	}
	return text
}

func w4Latency(published, reached time.Time) string {
	if published.IsZero() || reached.IsZero() {
		return ""
	}
	return fmt.Sprintf(", %s after it was published", reached.Sub(published).Round(time.Millisecond))
}

// w4CardTag is the numbered message's tag a card carries.
func w4CardTag(text string) string {
	at := strings.Index(text, "W4-")
	if at < 0 {
		return w4Short(text)
	}
	tag, _, _ := strings.Cut(text[at:], ":")
	return tag
}
