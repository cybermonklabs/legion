package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The controller notices of an outage (dispatch://INFRA-1585, R2 (a) to (c)): the idle rig, its
// real NATS and its real Envoy listener, with a controller stand-in beside the pods
// (idleRigController), and the listener's session windows shortened so that a controller session
// the listener stops hearing from is unresponsive, then gone, and its role claim freed within the
// test. In each case the daemon holds the rig's issue, and the issue's architect escalates the
// hold, while the controller session is away (holdIssue); the daemon publishes the escalated hold
// to the controller's role topic (notifications.role.legion-<project>-controller) once the
// registered controller session holds that role live, and the controller is given "held on
// <issue>" exactly once after it comes back and claims its role.
//
// The controls are the code before this change (the pull request records the runs): there the
// daemon publishes the hold to the controller topic (notifications.legion.<project>.controller),
// and each case is given the notice zero times. In (a) and (b) the session that comes back
// registers its agent subject alone, and the controller topic it registers again once it holds the
// role is added to its consumer at the consumer's position, past the stored notice; in (c) the new
// session's consumer reads that topic from the stream's end.

// idleRigControllerListenerEnv is the listener's session windows for the controller cases: a
// session is live for ten seconds after its last registration and gone ten seconds after that,
// and both reapers run every second, so a session that stops registering has its role claim freed
// about 21 s later. The rig's implementer is the issue's pod, which registers again every
// idleRigRegistrationRefresh (ExitsAsReleasedInProduction), inside the live window, so only the
// controller stand-in goes away.
var idleRigControllerListenerEnv = []string{
	"ENVOY_SESSION_LIVE_WINDOW=10s",
	"ENVOY_SESSION_GONE_HORIZON=20s",
	"ENVOY_ROLE_CLAIM_REAP_INTERVAL=1s",
	"ENVOY_SESSION_REAP_INTERVAL=1s",
}

const (
	// idleRigControllerHeartbeat is how often the controller stand-in registers again while it
	// beats, five times inside the shortened live window, as pi-envoy's 90 s heartbeat is inside
	// the five-minute one.
	idleRigControllerHeartbeat = 2 * time.Second
	// idleRigControllerNoticeWait bounds a controller notice reaching the controller once its
	// session holds the role: the outbox's sweep finds a row the rig's own engine wrote within
	// outboxLease (30 s), and a row that has waited retries at most RetryBackoffMost (64 s) later,
	// so three minutes is twice the longest wait the daemon's own clock gives it.
	idleRigControllerNoticeWait = 3 * time.Minute
	// idleRigControllerSettle is how long the count is read again after the first copy arrived:
	// a second copy of one notice is a kept-lane or takeover copy, which the listener hands over
	// within seconds of the claim.
	idleRigControllerSettle = 20 * time.Second
)

// idleRigHeld is the name the controller cases give the rig's one issue.
const idleRigHeld = "held by the controller rig"

// R2 (a): the controller session registers, claims the role and registers its topics, then stops
// its heartbeats until its claim is freed; the daemon holds the issue; the session registers again
// with its agent subject alone, reads its backlog to a message sent after the hold, and then
// claims the role. It is given "held on <issue>" exactly once.
func TestIdleRigAControllerAwayUntilItsClaimIsFreedIsGivenTheHoldOnceItClaimsItsRole(t *testing.T) {
	t.Parallel()
	rig := newIdleRigControllerRig(t)
	issue := rig.issue(idleRigHeld)
	controller := rig.startController("ses-idle-rig-controller-a", true)
	controller.registerWithDaemon()
	controller.holdRole()

	controller.silence()
	rig.awaitControllerRoleFree()
	rig.holdIssue(issue)
	rig.awaitControllerNoticeTried(issue)

	controller.beat(nil)
	after := "sent to the controller after the hold"
	status, body := rig.listener.send(map[string]any{
		"target_session": controller.session, "message": after,
		"source": "agent", "source_session": idleRigSender, "idempotency_key": "controller-after-hold",
	})
	if status/100 != 2 {
		t.Fatalf("send the controller a message after the hold = %d %s, want it stored", status, body)
	}
	idleRigEventually(t, "the controller reading its backlog to the message sent after the hold", func() bool {
		return controller.given(after) == 1
	})
	controller.holdRole()

	controller.awaitGivenOnce("held on " + issue.key)
}

// R2 (b): the controller session keeps registering and reads no frame; the hold is published and
// answered 200; the process ends and its claim is freed; the session comes back under the same
// id with its agent subject alone, and then claims the role. It is given the notice exactly once.
func TestIdleRigAControllerThatReadsNoFrameAndEndsIsGivenTheHoldOnceWhenItComesBack(t *testing.T) {
	t.Parallel()
	rig := newIdleRigControllerRig(t)
	issue := rig.issue(idleRigHeld)
	controller := rig.startController("ses-idle-rig-controller-b", false)
	controller.registerWithDaemon()
	controller.holdRole()

	rig.holdIssue(issue)
	idleRigEventually(t, "the daemon's publish of the hold answered 200 while the controller reads no frame", func() bool {
		return rig.controllerNoticeRows(issue) == 0
	})
	controller.end()
	rig.awaitControllerRoleFree()

	controller = rig.startController(controller.session, true)
	controller.beat(nil)
	controller.holdRole()

	controller.awaitGivenOnce("held on " + issue.key)
}

// R2 (c): as (a), but the session that claims the role after the old one's claim was freed, with
// nothing stored for it, is a controller under a new session id. It is given the notice exactly
// once.
func TestIdleRigANewControllerSessionIsGivenTheHoldItsPredecessorWasAwayFor(t *testing.T) {
	t.Parallel()
	rig := newIdleRigControllerRig(t)
	issue := rig.issue(idleRigHeld)
	old := rig.startController("ses-idle-rig-controller-c-old", true)
	old.registerWithDaemon()
	old.holdRole()

	old.silence()
	rig.awaitControllerRoleFree()
	rig.holdIssue(issue)
	rig.awaitControllerNoticeTried(issue)
	old.end()

	successor := rig.startController("ses-idle-rig-controller-c-new", true)
	successor.registerWithDaemon()
	successor.beat(nil)
	successor.holdRole()

	successor.awaitGivenOnce("held on " + issue.key)
	if n := old.given("held on " + issue.key); n != 0 {
		t.Fatalf("the controller session that went away was given the hold %d times, want none: it was gone before the hold", n)
	}
}

// newIdleRigControllerRig is the idle rig with the controller cases' listener windows and one
// issue, whose implementer registers again inside the shortened live window, as it does in
// production, so its pod stays live while the controller goes away.
func newIdleRigControllerRig(t *testing.T) *idleWakeRig {
	t.Helper()
	return newIdleWakeRigWith(t, idleRigOptions{ListenerEnv: idleRigControllerListenerEnv}, map[string]idleRigPlan{
		idleRigHeld: {TakeTurn: true, ExitsAsReleasedInProduction: true},
	}, idleRigHeld)
}

// controllerRole is the project's controller role token, the role a controller session claims.
func (r *idleWakeRig) controllerRole() string { return string(claim.ControllerToken(r.project)) }

// controllerNoticeTopic is the project's controller topic (legionControllerNoticeSubject,
// packages/contracts/src/subject.ts), which pi-legion's controller registers once it holds its
// role and keeps registering for as long as it holds it.
func (r *idleWakeRig) controllerNoticeTopic() string {
	return "notifications.legion." + r.project + ".controller"
}

// holdIssue holds the issue and hands the hold to the controller, as the daemon's workflow does
// when its implementer's claim fails and the issue's architect escalates the hold: the workflow's
// own engine applies the failure in the daemon's store, which holds the issue and tells its
// architect alone, and then the architect's escalation (retry_or_escalate, decision escalate),
// which records the hold's reason `escalated` and writes the controller's notice row of its own
// (workflow's retryOrEscalate and noticeWithController), "held on <issue>", which the daemon's
// outbox runs from its next sweep.
func (r *idleWakeRig) holdIssue(issue idleRigIssue) {
	r.t.Helper()
	engine := workflow.New(record.NewStore(), engineConfig(r.cfg, "", nil), quietLogger())
	result, err := intake.ApplyFact(context.Background(), r.pool, "supervise", "controller-rig-held-"+issue.key,
		intake.ClaimFailed{Issue: issue.key, Role: issue.role, Reason: "the controller rig's failure"}, engine)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("hold %s = %+v, %v; want the failure applied", issue.key, result.Refusal, err)
	}
	var held string
	if err := r.pool.QueryRow(context.Background(), "select phase from issues where key = $1", issue.key).Scan(&held); err != nil || held != string(phase.Held) {
		r.t.Fatalf("%s stands in %q after its implementer's failure (%v), want held", issue.key, held, err)
	}
	result, err = intake.ApplyFact(context.Background(), r.pool, "api", "controller-rig-escalated-"+issue.key,
		intake.RetryOrEscalate{Issue: issue.key, Decision: intake.EscalateDecision, Reason: "the controller rig's architect hands the hold on"}, engine)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("escalate the hold of %s = %+v, %v; want the escalation applied", issue.key, result.Refusal, err)
	}
	var reason string
	if err := r.pool.QueryRow(context.Background(), "select phase, coalesce(hold_reason, '') from issues where key = $1", issue.key).Scan(&held, &reason); err != nil ||
		held != string(phase.Held) || reason != string(record.HoldEscalated) {
		r.t.Fatalf("%s stands in %q held for %q after its architect's escalation (%v), want held for %q", issue.key, held, reason, err, record.HoldEscalated)
	}
}

// controllerNoticeRows is how many controller notice rows of the issue the daemon's outbox still
// holds: a row is deleted once its publish is answered 200.
func (r *idleWakeRig) controllerNoticeRows(issue idleRigIssue) int {
	r.t.Helper()
	return outboxRowsOfKind(r.t, r.pool, issue.key, record.OutboxKindControllerNotice)
}

// awaitControllerNoticeTried waits until the daemon's outbox has run the issue's controller
// notice row at least once: the row is finished, its publish answered 200, or it is still there
// after an attempt that waited. The rig's own engine writes the row (holdIssue), so the daemon
// finds it at its next sweep, up to outboxLease later; a controller that came back before that
// attempt would meet a notice published after it returned, which is not the outage a case drives.
func (r *idleWakeRig) awaitControllerNoticeTried(issue idleRigIssue) {
	r.t.Helper()
	var rows, attempts int
	idleRigEventually(r.t, "the daemon's first attempt of the hold's controller notice", func() bool {
		if err := r.pool.QueryRow(context.Background(), "select count(*), coalesce(max(attempts), 0) from outbox where issue = $1 and kind = $2",
			issue.key, string(record.OutboxKindControllerNotice)).Scan(&rows, &attempts); err != nil {
			r.t.Fatalf("read the controller notice rows of %s: %v", issue.key, err)
		}
		return rows == 0 || attempts >= 1
	})
	r.t.Logf("the daemon tried the hold's controller notice: %d rows left, %d attempts", rows, attempts)
}

// awaitControllerRoleFree waits until the listener names no holder of the controller role: the
// role claim reaper freed the claim of a session the listener no longer hears from.
func (r *idleWakeRig) awaitControllerRoleFree() {
	r.t.Helper()
	lookup := notify.New(r.listener.url, "")
	var last string
	idleRigEventually(r.t, "the listener freeing the controller role's claim", func() bool {
		holder, state, err := lookup.RoleHolder(context.Background(), r.controllerRole())
		last = fmt.Sprintf("holder %q, state %q, error %v", holder, state, err)
		return err == nil && holder == ""
	})
	r.t.Logf("the controller role is free (%s)", last)
}

// idleRigController is the controller stand-in: a session that does at the listener what
// pi-legion's controller does under `legion controller start` (packages/pi-legion's
// controller-session.ts) and records what its own consumer relays to it. It registers with the
// daemon on the capability the operator's start mints, registers with the listener on its own
// consumer, claims the controller role, and then registers again with the controller topic, as the
// plugin claims the role first and follows the topic after it. Its heartbeats are pi-envoy's
// registrations, sent again every idleRigControllerHeartbeat while it beats.
//
// It reads its relay frames as the role processes of the rig do (idleRigRoleProcess.relayed):
// each stored message once by its stream sequence. What it was given is counted by event id, as
// pi-envoy's delivery dedupe counts what it shows (extensions/envoy.ts): a kept-lane or takeover
// copy of a message keeps the event id of the message it copies.
type idleRigController struct {
	t       *testing.T
	rig     *idleWakeRig
	session string
	client  *http.Client
	bus     *nats.Conn
	// epoch is this process's one NATS connection, as a plugin's registration names it.
	epoch string
	// reads is whether the session answers its relay frames; one that answers none is a session
	// that registers and takes no frame, whose consumer keeps every message unacknowledged.
	reads bool

	mu       sync.Mutex
	recorded map[uint64]bool
	frames   []idleRigEnvelope
	stop     chan struct{}
	beating  sync.WaitGroup
}

// startController starts a controller process of session: its own NATS connection, taken on the
// session's relay subject before anything registers, as openDelivery takes it for a role process.
func (r *idleWakeRig) startController(session string, reads bool) *idleRigController {
	r.t.Helper()
	c := &idleRigController{t: r.t, rig: r, session: session, reads: reads, recorded: map[uint64]bool{},
		client: &http.Client{Timeout: 10 * time.Second}, epoch: fmt.Sprintf("idle-rig-controller-%s-%d", session, time.Now().UnixNano())}
	bus, err := nats.Connect(r.natsURL, nats.Name("idle-rig-controller-"+session), nats.MaxReconnects(-1))
	if err != nil {
		r.t.Fatalf("connect the controller %s to NATS: %v", session, err)
	}
	c.bus = bus
	if _, err := bus.Subscribe(idleRigDeliverSubjectPrefix+session, c.relayed); err != nil {
		r.t.Fatalf("subscribe the controller %s to its relay subject: %v", session, err)
	}
	if err := bus.Flush(); err != nil {
		r.t.Fatalf("flush the controller %s's subscription: %v", session, err)
	}
	r.t.Cleanup(c.end)
	return c
}

// registerWithDaemon is `legion controller start` and the plugin's registration: the operator's
// mint of a controller capability, then the session's registration on the claim route with that
// capability, which records the session as the project's registered controller.
func (c *idleRigController) registerWithDaemon() {
	c.t.Helper()
	status, body := c.rig.api.request(http.MethodPost, "/legion/v1/controller/secret",
		api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion()}, true)
	if status != http.StatusOK {
		c.t.Fatalf("mint the controller's capability = %d %s", status, body)
	}
	var minted api.ControllerSecretResponse
	if err := json.Unmarshal(body, &minted); err != nil || minted.Secret == "" {
		c.t.Fatalf("decode the controller's capability %s: %v", body, err)
	}
	status, body = c.rig.api.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: minted.Secret, SessionID: c.session, OmpSessionFile: "/sessions/" + c.session + ".jsonl",
		AgentID: "idle-rig-controller", PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		c.t.Fatalf("register %s as the controller = %d %s", c.session, status, body)
	}
}

// holdRole is the plugin's claim: the registration that starts the session's heartbeat, the role
// claim, and the registration again with the controller topic, which the session follows for as
// long as it holds the role.
func (c *idleRigController) holdRole() {
	c.t.Helper()
	if !c.isBeating() {
		c.beat(nil)
	}
	if err := c.post("/v1/roles/set", map[string]any{"role": c.rig.controllerRole(), "session_id": c.session}); err != nil {
		c.t.Fatalf("claim the controller role for %s: %v", c.session, err)
	}
	c.beat([]string{c.rig.controllerNoticeTopic()})
}

// beat registers the session with topics beside its agent subject at once, and again every
// idleRigControllerHeartbeat until silence or end.
func (c *idleRigController) beat(topics []string) {
	c.t.Helper()
	c.silence()
	if err := c.register(topics); err != nil {
		c.t.Fatalf("register %s with the listener: %v", c.session, err)
	}
	c.mu.Lock()
	c.stop = make(chan struct{})
	stop := c.stop
	c.mu.Unlock()
	c.beating.Add(1)
	go func() {
		defer c.beating.Done()
		ticker := time.NewTicker(idleRigControllerHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := c.register(topics); err != nil {
					fmt.Fprintf(os.Stderr, "idle rig controller %s: heartbeat: %v\n", c.session, err)
				}
			}
		}
	}()
}

func (c *idleRigController) isBeating() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stop != nil
}

// silence stops the heartbeats and waits for one in flight: from here the listener hears nothing
// of the session.
func (c *idleRigController) silence() {
	c.mu.Lock()
	stop := c.stop
	c.stop = nil
	c.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	c.beating.Wait()
}

// end is the process ending: no heartbeat, and its NATS connection closed.
func (c *idleRigController) end() {
	c.silence()
	c.mu.Lock()
	bus := c.bus
	c.bus = nil
	c.mu.Unlock()
	if bus != nil {
		bus.Close()
	}
}

// register is one registration with the listener, as idleRigRoleProcess.subscribe makes it: the
// session's own consumer, relayed to this process's connection under its epoch.
func (c *idleRigController) register(topics []string) error {
	if topics == nil {
		topics = []string{}
	}
	var answer struct {
		Delivery *struct {
			Mode string `json:"mode"`
		} `json:"delivery"`
	}
	if err := c.postAnswer("/v1/interests/subscribe", map[string]any{
		"session_id": c.session, "dir": "/legion/controller", "topics": topics,
		"host": "127.0.0.1", "title": "controller", "self_subscribed": true,
		"capabilities": []string{"prompt_async"}, "plugin": "idle-rig", "plugin_version": "0",
		"delivery": map[string]any{"consumer": true, "epoch": c.epoch},
	}, &answer); err != nil {
		return err
	}
	if answer.Delivery == nil || answer.Delivery.Mode != idleRigDeliveryConsumer {
		return fmt.Errorf("the listener registered %s on delivery %+v, want its own consumer's", c.session, answer.Delivery)
	}
	return nil
}

func (c *idleRigController) post(path string, body any) error {
	return c.postAnswer(path, body, nil)
}

func (c *idleRigController) postAnswer(path string, body, answer any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	response, err := c.client.Post(c.rig.listener.url+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	read, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s = %d %s", path, response.StatusCode, read)
	}
	if answer == nil {
		return nil
	}
	return json.Unmarshal(read, answer)
}

// relayed takes one frame of the session's consumer as idleRigRoleProcess.relayed does: the
// listener's connection probe is answered done, a frame is answered received, recorded once by
// its stream sequence and answered done. A session that reads no frame answers the probe alone.
func (c *idleRigController) relayed(msg *nats.Msg) {
	if msg.Header.Get(idleRigProbeHeader) == "1" {
		if msg.Reply != "" {
			c.reply(msg, idleRigRelayDone)
		}
		return
	}
	if !c.reads || msg.Reply == "" {
		return
	}
	var item idleRigEnvelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig controller %s: decode a frame: %v\n", c.session, err)
		return
	}
	sequence, err := strconv.ParseUint(msg.Header.Get(idleRigStreamSeqHeader), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idle rig controller %s: a frame carries stream sequence %q: %v\n", c.session, msg.Header.Get(idleRigStreamSeqHeader), err)
		return
	}
	c.reply(msg, idleRigRelayReceived)
	c.mu.Lock()
	if !c.recorded[sequence] {
		c.recorded[sequence] = true
		c.frames = append(c.frames, item)
	}
	c.mu.Unlock()
	c.reply(msg, idleRigRelayDone)
}

func (c *idleRigController) reply(msg *nats.Msg, word string) {
	c.mu.Lock()
	bus := c.bus
	c.mu.Unlock()
	if bus == nil {
		return
	}
	if err := bus.Publish(msg.Reply, []byte(word)); err == nil {
		_ = bus.Flush()
	}
}

// given is how many distinct events the session was given whose summary is summary, as pi-envoy
// shows them: one per event id.
func (c *idleRigController) given(summary string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var events []string
	for _, frame := range c.frames {
		if frame.PayloadSummary == summary && !slices.Contains(events, frame.EventID) {
			events = append(events, frame.EventID)
		}
	}
	return len(events)
}

// framesOf is every frame the session recorded whose summary is summary, by stream sequence.
func (c *idleRigController) framesOf(summary string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, frame := range c.frames {
		if frame.PayloadSummary == summary {
			n++
		}
	}
	return n
}

// summaries is every frame's summary, for a failure to name what the session was given.
func (c *idleRigController) summaries() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var all []string
	for _, frame := range c.frames {
		all = append(all, fmt.Sprintf("%s (%s)", frame.PayloadSummary, frame.EventID))
	}
	return strings.Join(all, "; ")
}

// awaitGivenOnce waits for the session to be given summary and then reads the count again once
// idleRigControllerSettle has passed: it must be one event, given once. A session never given it
// fails naming the count, 0. Pass or fail, it logs where the daemon published the notice, as the
// listener logged each publish of it (idleRigListener.publishesOf): the topic and the answer, so
// a run of the code before this change names the controller topic, and a run of this code the
// role topic.
func (c *idleRigController) awaitGivenOnce(summary string) {
	c.t.Helper()
	defer func() {
		published := c.rig.listener.publishesOf(summary)
		c.t.Logf("the listener logged %d publishes of %q", len(published), summary)
		for _, line := range published {
			c.t.Logf("the listener's publish of %q: %s", summary, line)
		}
		if c.t.Failed() {
			c.t.Logf("the controller %s was given: %s", c.session, c.summaries())
		}
	}()
	if !testwait.Holds(idleRigControllerNoticeWait, idleRigPoll, func() bool { return c.given(summary) != 0 }) {
		c.t.Fatalf("the controller %s was given %q 0 times in %s, want exactly once", c.session, summary, idleRigControllerNoticeWait)
	}
	time.Sleep(idleRigControllerSettle)
	if n := c.given(summary); n != 1 {
		c.t.Fatalf("the controller %s was given %q %d times, want exactly once", c.session, summary, n)
	}
	c.t.Logf("the controller %s was given %q once, in %d relay frames", c.session, summary, c.framesOf(summary))
}
