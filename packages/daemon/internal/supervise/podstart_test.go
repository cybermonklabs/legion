package supervise

// One session load at a time in an issue pod (dispatch://INFRA-5297): the roles of one issue
// share the issue's one pod and its one memory limit, so a role's process starts only once no
// other role of the pod is starting or loading its session, or once that load has run for
// podLoadBound without its agent registering. These tests build several machines of one issue
// over one harness, which share its pod start gate (podHarness), and hold a role's start in the
// fake runtime (holdStarts) where a pod coming up would hold it. A start the gate holds parks on
// its machine and its decision ends (askPod), so a test posts it as it posts any event and reads
// the wait off what is observable — the gate's queue, the claim launching, the runtime asked
// nothing — and the start the grant runs on a goroutine of its own is waited for
// (wantGrantedStart).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// podIssue is the issue every role of these tests works, and so its pod's key; it is also
	// the pod a line names for a process whose locator names no Sandbox, as the fake's own do.
	podIssue = "LEGION-209"
	// issueSandbox is the issue's Sandbox, which a start scripted into the pod (inIssuePod) comes
	// up in, and which the lines name as the pod.
	issueSandbox = "legion-legion-209"
	// promptBound is how much real time a stop of a claim waiting at the gate, and a read of that
	// claim through its machine's lock, may take: well under a second. Neither may wait for
	// another role's session load, which on the harness's clock, which nothing moves meanwhile,
	// would never end.
	promptBound = 500 * time.Millisecond
)

// podRole is one role's claim in the test's issue pod and its machine, with the session its agent
// registers.
type podRole struct {
	token       claim.Token
	m           *Machine
	session     string
	sessionFile string
}

// podHarness is a harness whose machines start their processes through one pod start gate, as a
// Kubernetes daemon's do (Deps.PodStarts): every test of this file runs against it, and the
// package's own harness builds none. phaseWorker is the role the workflow answers as working the
// issue's current phase (Deps.PhaseRole), "" for a harness that answers none.
func podHarness(t *testing.T, phaseWorker claim.Role) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.PodStarts = NewPodStarts(h.clock)
	if phaseWorker != "" {
		h.deps.PhaseRole = func(context.Context, string) (claim.Role, error) { return phaseWorker, nil }
	}
	return h
}

// podRole builds the machine of role's claim of podIssue in state: queued, which a spawn starts
// with a fresh session, or suspended by the daemon at generation 1 over a session of its own,
// which a resume continues.
func (h *harness) podRole(role claim.Role, state ClaimState) *podRole {
	h.t.Helper()
	return h.podRoleUnder(h.ctx, role, state)
}

// podRoleUnder is podRole with its machine built on ctx, which a test ends for that machine alone
// as a stopping daemon ends every machine's (shutdown).
func (h *harness) podRoleUnder(ctx context.Context, role claim.Role, state ClaimState) *podRole {
	h.t.Helper()
	c := queuedClaim()
	c.Token, c.Role, c.State = claim.Token("legion-"+podIssue+"-"+string(role)), role, state
	r := &podRole{token: c.Token, session: "ses_" + string(role), sessionFile: "/state/sessions/" + string(role) + ".jsonl"}
	if state == StateSuspended {
		c.Generation, c.SuspendedBy = 1, SuspendedByDaemon
		c.Session, c.SessionFile = r.session, r.sessionFile
	}
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		h.t.Fatal(err)
	}
	m, err := NewMachine(ctx, h.deps, c)
	if err != nil {
		h.t.Fatalf("new machine for %s: %v", c.Token, err)
	}
	r.m = m
	return r
}

// postAsync hands m ev on a goroutine of its own, as the daemon's feed does, and answers the
// channel its decision's error comes back on.
func (h *harness) postAsync(m *Machine, ev Event) <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.postTo(m, ev) }()
	return done
}

// promptly is the one receive from ch, which must come within promptBound of real time.
func promptly[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	timer := time.NewTimer(promptBound)
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("%s did not come within %s: it waits on another role's session load", what, promptBound)
	}
	var zero T
	return zero
}

// claimPromptly is r's claim as Machine.Claim reads it, through r's machine's lock, within
// promptBound: no decision of a machine waits for another role's load (askPod).
func (h *harness) claimPromptly(r *podRole) Claim {
	h.t.Helper()
	read := make(chan Claim, 1)
	go func() { read <- r.m.Claim() }()
	return promptly(h.t, string(r.token)+"'s Claim, read through its machine's lock", read)
}

// postPromptly is ev handed to r's machine and answered within promptBound (postAsync).
func (h *harness) postPromptly(r *podRole, ev Event) error {
	h.t.Helper()
	return promptly(h.t, fmt.Sprintf("the answer to %T of %s", ev, r.token), h.postAsync(r.m, ev))
}

// registered is r's agent registering at r's current generation, which settles its process's load:
// the load ends podLoadSettle later (loadEnds).
func (h *harness) registered(r *podRole) {
	h.t.Helper()
	h.mustPostTo(r.m, RequestRegister{Claim: r.token, Generation: r.m.View().Claim.Generation, Session: r.session,
		SessionFile: r.sessionFile, CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
}

// loadEnds is r's agent registering, and its load ending podLoadSettle after (PodStarts.settle).
func (h *harness) loadEnds(r *podRole) {
	h.t.Helper()
	h.registered(r)
	h.clock.Advance(podLoadSettle)
}

// runs brings a suspended r up and through its load: resumed, registered and settled.
func (h *harness) runs(r *podRole) {
	h.t.Helper()
	h.mustPostTo(r.m, RequestResume{Claim: r.token})
	h.loadEnds(r)
}

// died is the runtime reporting r's process gone, on a goroutine of its own: the relaunch it raises
// asks the gate.
func (h *harness) died(r *podRole) <-chan error {
	h.t.Helper()
	loc := r.m.View().Claim.Locator
	if loc == nil {
		h.t.Fatalf("%s records no process to die", r.token)
	}
	return h.postAsync(r.m, RuntimeObservation{Observation: runtime.Observation{Locator: *loc, Kind: runtime.Gone, At: h.clock.Now()}})
}

// podWaiting is every claim whose start waits at the gate for podIssue's pod, in the order asked.
// A harness with no gate has none waiting: that is the harness these tests' negative control runs
// on (podHarness with its gate left nil), which must fail by what started rather than by a nil
// gate.
func (h *harness) podWaiting() []claim.Token {
	g := h.deps.PodStarts
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	q := g.pods[podIssue]
	if q == nil {
		return nil
	}
	tokens := make([]claim.Token, 0, len(q.waiting))
	for _, w := range q.waiting {
		tokens = append(tokens, w.token)
	}
	return tokens
}

// wantWaitsForPod is r's start waiting at the gate, held by what is observable: the start is in
// the gate's queue, the runtime has not been asked for r's process beyond the before starts it
// was asked for earlier, and r's claim is launching, as the try wrote it before it parked.
func (h *harness) wantWaitsForPod(r *podRole, before int) {
	h.t.Helper()
	testwait.Eventually(h.t, string(r.token)+"'s start to wait for its pod or to start", func() bool {
		return slices.Contains(h.podWaiting(), r.token) || h.starts(r.token) > before
	})
	if h.starts(r.token) > before {
		h.t.Fatalf("%s's process started while another role of its pod was starting or loading", r.token)
	}
	if state := r.m.View().Claim.State; state != StateLaunching {
		h.t.Fatalf("%s is %s while its start waits at the gate, want launching", r.token, state)
	}
}

// wantGrantedStart is r's start the gate granted after its wait: the gate tells r's machine on a
// goroutine of its own (askPod), so this waits until the runtime has been asked for r's process
// n times, and then for the decision that asked to end (Machine.Wait). A start the door holds in
// the runtime is read at the door instead (podDoor.at).
func (h *harness) wantGrantedStart(r *podRole, n int) {
	h.t.Helper()
	testwait.Eventually(h.t, fmt.Sprintf("%s's start %d, once the gate granted it", r.token, n), func() bool {
		return h.starts(r.token) >= n
	})
	r.m.Wait()
	h.wantStarts(r.token, n)
}

// waitingStart is a role whose start waits at the gate, and the channel the decision that raised
// it answers on (postAsync).
type waitingStart struct {
	r    *podRole
	done <-chan error
}

// wantStartOrder ends holder's load, and then the load of each start it lets through, and holds
// the starts waiting to order: each starts alone, once the load before it has ended, while the
// rest still wait.
func (h *harness) wantStartOrder(holder *podRole, order ...waitingStart) {
	h.t.Helper()
	for i, next := range order {
		before := map[claim.Token]int{}
		for _, w := range order[i:] {
			before[w.r.token] = h.starts(w.r.token)
		}
		h.loadEnds(holder)
		testwait.Eventually(h.t, "the next start into the pod", func() bool {
			for token, n := range before {
				if h.starts(token) > n {
					return true
				}
			}
			return false
		})
		next.r.m.Wait()
		for token, n := range before {
			if started := h.starts(token) > n; started != (token == next.r.token) {
				h.t.Fatalf("after %s's load ended, %s started: %v; want %s alone to start", holder.token, token, started, next.r.token)
			}
		}
		if err := testwait.Receive(h.t, string(next.r.token)+"'s start", next.done); err != nil {
			h.t.Fatalf("%s's start: %v", next.r.token, err)
		}
		holder = next.r
	}
}

// podDoor holds every process start of the harness's runtime at the door until the test lets it
// through, as a pod coming up holds a Sandbox launch: each start names its claim on entered, and
// waits for the test's answer on release — nil starts it, an error fails it — or for its own
// context's end.
type podDoor struct {
	entered chan claim.Token
	release chan error
}

// holdStarts puts a door in front of every Spawn and Resume of the harness's runtime. The gate lets
// one start of the pod into the runtime at a time, so the start at the door is the runtime's latest.
func (h *harness) holdStarts() *podDoor {
	d := &podDoor{entered: make(chan claim.Token, 8), release: make(chan error, 8)}
	hold := func(ctx context.Context) error {
		d.entered <- h.lastStart()
		select {
		case err := <-d.release:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.rt.OnResume, h.rt.OnSpawn = hold, hold
	return d
}

// lastStart is the claim of the runtime's latest Spawn or Resume.
func (h *harness) lastStart() claim.Token {
	calls := h.rt.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == "Spawn" || calls[i].Method == "Resume" {
			return calls[i].Spec.Claim
		}
	}
	return ""
}

// at is r's start reaching the door.
func (d *podDoor) at(t *testing.T, r *podRole) {
	t.Helper()
	if got := testwait.Receive(t, string(r.token)+"'s start at the pod's door", d.entered); got != r.token {
		t.Fatalf("the start at the pod's door is %s's, want %s's", got, r.token)
	}
}

// inIssuePod is a start of token's process at generation in the issue's Sandbox, as the Sandbox
// runtime answers one.
func inIssuePod(token claim.Token, generation uint64) fake.SpawnResult {
	return fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation("pod-uid-9", generation),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: issueSandbox, PodUID: "pod-uid-9", Container: runtime.IssuePodContainer, Generation: generation},
	}}
}

// podRecords is every record of message the machine of token wrote, in order.
func (h *harness) podRecords(message string, token claim.Token) []loggedRecord {
	var of []loggedRecord
	for _, r := range h.records.of(message) {
		if v, ok := r.attr("claim"); ok && v.String() == string(token) {
			of = append(of, r)
		}
	}
	return of
}

// podWait is what a start line says of the wait it ended: the claim waited for, the wait, and
// what ended it.
type podWait struct {
	claim   claim.Token
	waited  time.Duration
	endedBy string
}

// wantStartLine is token's latest start line: it names the claim and pod, and wait where the start
// waited, or no wait at all where wait is nil.
func (h *harness) wantStartLine(token claim.Token, pod string, wait *podWait) {
	h.t.Helper()
	lines := h.podRecords(podStartedLine, token)
	if len(lines) == 0 {
		h.t.Fatalf("no start line of %s, want one: %s", token, strings.Join(h.logs.lines(podStartedLine), " | "))
	}
	line := lines[len(lines)-1]
	h.wantAttr(line, "pod", pod)
	if wait == nil {
		if v, ok := line.attr("waitedFor"); ok {
			h.t.Fatalf("%s's start line names a wait for %s, want none", token, v)
		}
		return
	}
	h.wantAttr(line, "waitedFor", string(wait.claim))
	h.wantAttr(line, "waitEndedBy", wait.endedBy)
	if v, ok := line.attr("waited"); !ok || v.Kind() != slog.KindDuration || v.Duration() != wait.waited {
		h.t.Fatalf("%s's start line says it waited %v, want %s", token, v, wait.waited)
	}
}

// podLoadWant is what a load-end line says: how long the load ran from its process's start, what
// ended it, how long after that start its agent registered (registeredAfter, where registered),
// and whether the bound had released its hold.
type podLoadWant struct {
	loaded          time.Duration
	endedBy         string
	registered      bool
	registeredAfter time.Duration
	byBound         bool
}

// wantLoadEndLine is token's latest load-end line: the claim and pod, and what want says.
func (h *harness) wantLoadEndLine(token claim.Token, pod string, want podLoadWant) {
	h.t.Helper()
	lines := h.podRecords(podLoadEndedLine, token)
	if len(lines) == 0 {
		h.t.Fatalf("no load-end line of %s, want one: %s", token, strings.Join(h.logs.lines(podLoadEndedLine), " | "))
	}
	line := lines[len(lines)-1]
	h.wantAttr(line, "pod", pod)
	h.wantAttr(line, "endedBy", want.endedBy)
	if v, ok := line.attr("loaded"); !ok || v.Kind() != slog.KindDuration || v.Duration() != want.loaded {
		h.t.Fatalf("%s's load-end line says it loaded %v, want %s", token, v, want.loaded)
	}
	if v, ok := line.attr("registered"); ok != want.registered || ok && (v.Kind() != slog.KindDuration || v.Duration() != want.registeredAfter) {
		h.t.Fatalf("%s's load-end line says registered %v (%v), want %s (%v)", token, v, ok, want.registeredAfter, want.registered)
	}
	if v, ok := line.attr("releasedByBound"); ok != want.byBound || ok && !v.Bool() {
		h.t.Fatalf("%s's load-end line says releasedByBound %v (%v), want %v", token, v, ok, want.byBound)
	}
}

// wantLoadEndLines is token's machine having written n load-end lines: one per load that ended,
// and none for a start whose process never started.
func (h *harness) wantLoadEndLines(token claim.Token, n int) {
	h.t.Helper()
	if lines := h.podRecords(podLoadEndedLine, token); len(lines) != n {
		h.t.Fatalf("%s wrote %d load-end lines, want %d", token, len(lines), n)
	}
}

// wantAttr is line's key holding want.
func (h *harness) wantAttr(line loggedRecord, key, want string) {
	h.t.Helper()
	if v, ok := line.attr(key); !ok || v.String() != want {
		h.t.Fatalf("line %q holds %s=%v (%v), want %q", line.message, key, v, ok, want)
	}
}

// wantClaimState is r's claim in state, read without waiting on its machine's decision.
func (h *harness) wantClaimState(r *podRole, state ClaimState) {
	h.t.Helper()
	if got := r.m.View().Claim.State; got != state {
		h.t.Fatalf("%s is %s, want %s", r.token, got, state)
	}
}

// receive is the decision on done, which must have ended in an error where failed says so, and in
// none where it does not.
func receive(t *testing.T, what string, done <-chan error, failed bool) {
	t.Helper()
	if err := testwait.Receive(t, what, done); (err != nil) != failed {
		t.Fatalf("%s answered %v, want failed=%v", what, err, failed)
	}
}

// wantNoStartsAfter is the pod with nothing left to start: no start waits at the gate, and the
// clock passing a load's settle and its bound asks the runtime for no process of roles beyond the
// starts it had been asked for by now.
func (h *harness) wantNoStartsAfter(roles ...*podRole) {
	h.t.Helper()
	before := make([]int, len(roles))
	for i, r := range roles {
		before[i] = h.starts(r.token)
	}
	h.clock.Advance(podLoadBound + podLoadSettle)
	for i, r := range roles {
		r.m.Wait()
		if got := h.starts(r.token); got != before[i] {
			h.t.Fatalf("the runtime was asked to start %s %d times, want %d: nothing is left to start", r.token, got, before[i])
		}
	}
	if waiting := h.podWaiting(); len(waiting) > 0 {
		h.t.Fatalf("starts waiting for the pod = %v, want none", waiting)
	}
}

// waitingTrio is the pod a stop of a waiting claim is asked in: first's process loads, and the
// starts of second and third wait at the gate behind it, second's asked first.
func (h *harness) waitingTrio() (first, second, third *podRole) {
	h.t.Helper()
	first, second, third = h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended),
		h.podRole(claim.RoleTester, StateSuspended)
	h.mustPostTo(first.m, RequestResume{Claim: first.token})
	h.wantStarts(first.token, 1)
	for _, r := range []*podRole{second, third} {
		h.mustPostTo(r.m, RequestResume{Claim: r.token})
		h.wantWaitsForPod(r, 0)
	}
	return first, second, third
}

// stopsWithoutWaiting is ev, a stop of second while its start waits at the gate behind another
// role's load, waiting for nothing: ev is answered within promptBound of real time, Claim of
// second is read within it before ev and after, and ev leaves second in state, its start out of
// the gate's queue with one line saying its launch was cut short, and its process never asked of
// the runtime.
func (h *harness) stopsWithoutWaiting(second *podRole, ev Event, state ClaimState) {
	h.t.Helper()
	if c := h.claimPromptly(second); c.State != StateLaunching || c.Locator != nil {
		h.t.Fatalf("%s is %s recording %+v while its start waits at the gate, want launching with no process", second.token, c.State, c.Locator)
	}
	if err := h.postPromptly(second, ev); err != nil {
		h.t.Fatalf("%T of %s, whose start waits at the gate: %v", ev, second.token, err)
	}
	if c := h.claimPromptly(second); c.State != state {
		h.t.Fatalf("%s is %s after %T, want %s", second.token, c.State, ev, state)
	}
	h.wantStarts(second.token, 0)
	if waiting := h.podWaiting(); slices.Contains(waiting, second.token) {
		h.t.Fatalf("starts waiting for the pod = %v after %T of %s, want its start out of the queue", waiting, ev, second.token)
	}
	if lines := h.logs.lines("launch cut short while it waited for its issue pod's start", "claim="+string(second.token)); len(lines) != 1 {
		h.t.Fatalf("cut-short lines of %s = %d, want 1", second.token, len(lines))
	}
}

// A second role's start into an issue pod waits until the first role's session load ends,
// podLoadSettle after its agent's registration, and a third waits behind the second rather than
// starting beside it; the LINUX-1301 shape, where the second start was asked while the first
// role's pod was still coming up, loads one at a time. Every start writes the line R4 reads, naming
// the claim and the pod, and what a start that waited waited for, how long and what ended it;
// every load's end writes its own, one per load. No stop of a claim waiting at the gate waits for
// the load it waits for, nor does a read of that claim.
func TestAPodStartsItsRolesOneLoadAtATime(t *testing.T) {
	t.Run("the second start waits for the first role's registration", func(t *testing.T) {
		h := podHarness(t, "")
		first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

		h.mustPostTo(first.m, RequestResume{Claim: first.token})
		h.wantStarts(first.token, 1)
		h.mustPostTo(second.m, RequestResume{Claim: second.token})
		h.wantWaitsForPod(second, 0)
		h.clock.Advance(19 * time.Second)
		h.wantWaitsForPod(second, 0)

		h.loadEnds(first)
		h.wantGrantedStart(second, 1)
		h.wantStartLine(first.token, podIssue, nil)
		h.wantLoadEndLine(first.token, podIssue, podLoadWant{loaded: 19*time.Second + podLoadSettle, endedBy: "registration",
			registered: true, registeredAfter: 19 * time.Second})
		h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 19*time.Second + podLoadSettle, endedBy: "load end"})
	})

	t.Run("the second start waits for the settle after the first role's registration, not for the registration alone", func(t *testing.T) {
		h := podHarness(t, "")
		first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

		h.mustPostTo(first.m, RequestResume{Claim: first.token})
		h.mustPostTo(second.m, RequestResume{Claim: second.token})
		h.wantWaitsForPod(second, 0)
		h.clock.Advance(8 * time.Second)
		h.registered(first)
		h.wantWaitsForPod(second, 0)

		// A second short of the settle the first role's load still holds the pod.
		h.clock.Advance(podLoadSettle - time.Second)
		h.wantWaitsForPod(second, 0)
		h.wantLoadEndLines(first.token, 0)
		h.clock.Advance(time.Second)
		h.wantGrantedStart(second, 1)
		h.wantLoadEndLines(first.token, 1)
		h.wantLoadEndLine(first.token, podIssue, podLoadWant{loaded: 8*time.Second + podLoadSettle, endedBy: "registration",
			registered: true, registeredAfter: 8 * time.Second})
		h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 8*time.Second + podLoadSettle, endedBy: "load end"})
	})

	t.Run("the LINUX-1301 shape starts the second role at the first role's load end, not its bound", func(t *testing.T) {
		h := podHarness(t, "")
		door := h.holdStarts()
		first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

		firstStarted := h.postAsync(first.m, RequestResume{Claim: first.token})
		door.at(t, first)
		secondStarted := h.postAsync(second.m, RequestResume{Claim: second.token})
		h.wantWaitsForPod(second, 0)
		// The pod comes up 118 s after the second start was asked, and the first role's process
		// starts in it.
		h.clock.Advance(118 * time.Second)
		h.rt.ScriptResume(inIssuePod(first.token, 2))
		door.release <- nil
		receive(t, "the first role's start", firstStarted, false)
		h.wantStartLine(first.token, issueSandbox, nil)
		h.wantWaitsForPod(second, 0)

		// Its agent registers 19 s after its process started, and its load ends podLoadSettle
		// after that.
		h.clock.Advance(19 * time.Second)
		h.loadEnds(first)
		door.at(t, second)
		h.rt.ScriptResume(inIssuePod(second.token, 2))
		door.release <- nil
		second.m.Wait()
		receive(t, "the second role's resume, parked at the gate until its grant", secondStarted, false)

		h.wantLoadEndLine(first.token, issueSandbox, podLoadWant{loaded: 19*time.Second + podLoadSettle, endedBy: "registration",
			registered: true, registeredAfter: 19 * time.Second})
		h.wantStartLine(second.token, issueSandbox, &podWait{claim: first.token, waited: 137*time.Second + podLoadSettle, endedBy: "load end"})
	})

	t.Run("a third role asked while the second waits starts after the second, not beside it", func(t *testing.T) {
		h := podHarness(t, "")
		first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)
		third := h.podRole(claim.RoleTester, StateQueued)

		h.mustPostTo(first.m, RequestResume{Claim: first.token})
		h.mustPostTo(second.m, RequestResume{Claim: second.token})
		h.wantWaitsForPod(second, 0)
		h.mustPostTo(third.m, RequestSpawn{Claim: third.token})
		h.wantWaitsForPod(third, 0)
		h.clock.Advance(10 * time.Second)

		h.loadEnds(first)
		h.wantGrantedStart(second, 1)
		h.wantWaitsForPod(third, 0)
		h.clock.Advance(20 * time.Second)
		h.wantWaitsForPod(third, 0)

		h.loadEnds(second)
		h.wantGrantedStart(third, 1)
		h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 10*time.Second + podLoadSettle, endedBy: "load end"})
		h.wantLoadEndLine(second.token, podIssue, podLoadWant{loaded: 20*time.Second + podLoadSettle, endedBy: "registration",
			registered: true, registeredAfter: 20 * time.Second})
		h.wantStartLine(third.token, podIssue, &podWait{claim: second.token, waited: 30*time.Second + 2*podLoadSettle, endedBy: "load end"})
	})

	t.Run("the bound counts from the first role's process start, not from the second's ask", func(t *testing.T) {
		h := podHarness(t, "")
		door := h.holdStarts()
		first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

		firstStarted := h.postAsync(first.m, RequestResume{Claim: first.token})
		door.at(t, first)
		secondStarted := h.postAsync(second.m, RequestResume{Claim: second.token})
		h.wantWaitsForPod(second, 0)
		h.clock.Advance(118 * time.Second)
		door.release <- nil
		receive(t, "the first role's start", firstStarted, false)

		// 237 s after the second start was asked, and a second short of the bound after the first
		// role's process started: the second still waits.
		h.clock.Advance(podLoadBound - time.Second)
		h.wantWaitsForPod(second, 0)
		h.clock.Advance(time.Second)
		door.at(t, second)
		door.release <- nil
		second.m.Wait()
		receive(t, "the second role's resume, parked at the gate until its grant", secondStarted, false)
		h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 118*time.Second + podLoadBound, endedBy: "bound"})

		// The load the bound passed over ends when its settle ends, and its line says the bound
		// released it.
		h.clock.Advance(5 * time.Second)
		h.loadEnds(first)
		h.wantLoadEndLine(first.token, podIssue, podLoadWant{loaded: podLoadBound + 5*time.Second + podLoadSettle, endedBy: "registration",
			registered: true, registeredAfter: podLoadBound + 5*time.Second, byBound: true})
	})

	t.Run("the bound holds only until the first role registers", func(t *testing.T) {
		h := podHarness(t, "")
		first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

		h.mustPostTo(first.m, RequestResume{Claim: first.token})
		h.mustPostTo(second.m, RequestResume{Claim: second.token})
		h.wantWaitsForPod(second, 0)

		// The first role's agent registers 100 s after its process started, inside the bound: its
		// load ends 30 s later, at +130 s, and the bound no longer runs.
		h.clock.Advance(100 * time.Second)
		h.registered(first)
		h.clock.Advance(podLoadBound - 100*time.Second + time.Second)
		h.wantWaitsForPod(second, 0)
		h.clock.Advance(podLoadSettle - (podLoadBound - 100*time.Second) - 2*time.Second)
		h.wantWaitsForPod(second, 0)
		h.wantLoadEndLines(first.token, 0)

		h.clock.Advance(time.Second)
		h.wantGrantedStart(second, 1)
		h.wantLoadEndLine(first.token, podIssue, podLoadWant{loaded: 100*time.Second + podLoadSettle, endedBy: "registration",
			registered: true, registeredAfter: 100 * time.Second})
		h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 100*time.Second + podLoadSettle, endedBy: "load end"})
	})

	t.Run("each end of the first role's start or load ends the wait", func(t *testing.T) {
		t.Run("a failed launch", func(t *testing.T) {
			h := podHarness(t, "")
			door := h.holdStarts()
			first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

			firstStarted := h.postAsync(first.m, RequestResume{Claim: first.token})
			door.at(t, first)
			secondStarted := h.postAsync(second.m, RequestResume{Claim: second.token})
			h.wantWaitsForPod(second, 0)
			h.clock.Advance(4 * time.Second)
			door.release <- errBoom
			door.at(t, second)
			door.release <- nil
			second.m.Wait()
			receive(t, "the second role's resume, parked at the gate until its grant", secondStarted, false)
			h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 4 * time.Second, endedBy: "failed launch"})
			h.wantLoadEndLines(first.token, 0)

			// The first role's launch tries again, and that try waits for the second role's load.
			receive(t, "the first role's launch, parked for its second try", firstStarted, false)
			h.wantWaitsForPod(first, 1)
			h.clock.Advance(7 * time.Second)
			h.loadEnds(second)
			door.at(t, first)
			door.release <- nil
			first.m.Wait()
			h.wantStarts(first.token, 2)
			h.wantStartLine(first.token, podIssue, &podWait{claim: second.token, waited: 7*time.Second + podLoadSettle, endedBy: "load end"})
		})

		t.Run("a process that ended before its registration", func(t *testing.T) {
			h := podHarness(t, "")
			first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

			h.mustPostTo(first.m, RequestResume{Claim: first.token})
			h.mustPostTo(second.m, RequestResume{Claim: second.token})
			h.wantWaitsForPod(second, 0)
			h.clock.Advance(5 * time.Second)
			relaunched := h.died(first)
			h.wantGrantedStart(second, 1)
			h.wantLoadEndLine(first.token, podIssue, podLoadWant{loaded: 5 * time.Second, endedBy: "process end"})
			h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 5 * time.Second, endedBy: "process end"})

			// The dead process's relaunch waits for the second role's load.
			receive(t, "the first role's relaunch, parked", relaunched, false)
			h.wantWaitsForPod(first, 1)
			h.loadEnds(second)
			h.wantGrantedStart(first, 2)
			h.wantStartLine(first.token, podIssue, &podWait{claim: second.token, waited: podLoadSettle, endedBy: "load end"})
		})

		t.Run("a process that ended during its settle", func(t *testing.T) {
			h := podHarness(t, "")
			first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

			h.mustPostTo(first.m, RequestResume{Claim: first.token})
			h.mustPostTo(second.m, RequestResume{Claim: second.token})
			h.wantWaitsForPod(second, 0)
			h.clock.Advance(4 * time.Second)
			h.registered(first)
			h.clock.Advance(10 * time.Second)
			h.wantWaitsForPod(second, 0)

			// The first role's process dies 10 s into its settle: the second starts at once.
			relaunched := h.died(first)
			h.wantGrantedStart(second, 1)
			h.wantLoadEndLine(first.token, podIssue, podLoadWant{loaded: 14 * time.Second, endedBy: "process end",
				registered: true, registeredAfter: 4 * time.Second})
			h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 14 * time.Second, endedBy: "process end"})

			// The settle the death cut short ends nothing more: the load wrote one line.
			receive(t, "the first role's relaunch, parked", relaunched, false)
			h.clock.Advance(podLoadSettle)
			h.wantLoadEndLines(first.token, 1)
			h.wantWaitsForPod(first, 1)
			h.loadEnds(second)
			h.wantGrantedStart(first, 2)
		})

		t.Run("a suspension of the first claim", func(t *testing.T) {
			h := podHarness(t, "")
			door := h.holdStarts()
			first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

			firstStarted := h.postAsync(first.m, RequestResume{Claim: first.token})
			door.at(t, first)
			secondStarted := h.postAsync(second.m, RequestResume{Claim: second.token})
			h.wantWaitsForPod(second, 0)
			h.clock.Advance(6 * time.Second)
			suspended := h.postAsync(first.m, RequestSuspend{Claim: first.token, By: SuspendedByOperator, Reason: "a person stops the role while its pod comes up"})
			door.at(t, second)
			receive(t, "the first claim's suspension", suspended, false)
			receive(t, "the first role's start, cut short", firstStarted, true)
			h.wantClaimState(first, StateSuspended)
			door.release <- nil
			second.m.Wait()
			receive(t, "the second role's resume, parked at the gate until its grant", secondStarted, false)
			h.wantStarts(second.token, 1)
			h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 6 * time.Second, endedBy: "suspension"})
			h.wantLoadEndLines(first.token, 0)
		})

		t.Run("a retirement of the first claim", func(t *testing.T) {
			h := podHarness(t, "")
			first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)

			h.mustPostTo(first.m, RequestResume{Claim: first.token})
			h.mustPostTo(second.m, RequestResume{Claim: second.token})
			h.wantWaitsForPod(second, 0)
			h.clock.Advance(3 * time.Second)
			h.mustPostTo(first.m, RequestStop{Claim: first.token})
			h.wantGrantedStart(second, 1)
			h.wantClaimState(first, StateRetired)
			h.wantLoadEndLine(first.token, podIssue, podLoadWant{loaded: 3 * time.Second, endedBy: "retirement"})
			h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 3 * time.Second, endedBy: "retirement"})
		})
	})

	t.Run("every kind of start waits", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			// paced is whether the daemon sets a wake pace; the first role's spawn takes its one
			// token, so a second start the pacing does not let through would park rather than wait.
			paced  bool
			state  ClaimState
			start  func(claim.Token) Event
			method string
		}{
			{name: "an operator's start, which the wake pacing lets through", paced: true, state: StateQueued,
				start: func(token claim.Token) Event { return RequestSpawn{Claim: token, Exempt: true} }, method: "Spawn"},
			{name: "a resume in a daemon that sets no wake pace", state: StateSuspended,
				start: func(token claim.Token) Event { return RequestResume{Claim: token} }, method: "Resume"},
			{name: "a fresh session's start", state: StateQueued,
				start: func(token claim.Token) Event { return RequestSpawn{Claim: token} }, method: "Spawn"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := podHarness(t, "")
				if tc.paced {
					pacedDeps(wakeRate)(&h.deps)
				}
				first, second := h.podRole(claim.RoleArchitect, StateQueued), h.podRole(claim.RoleImplementer, tc.state)

				h.mustPostTo(first.m, RequestSpawn{Claim: first.token})
				h.wantStarts(first.token, 1)
				h.mustPostTo(second.m, tc.start(second.token))
				h.wantWaitsForPod(second, 0)
				h.clock.Advance(2 * time.Second)

				h.loadEnds(first)
				h.wantGrantedStart(second, 1)
				if calls := h.calls(tc.method); len(calls) == 0 || calls[len(calls)-1].Spec.Claim != second.token {
					t.Fatalf("the second role's start was no %s: %v", tc.method, h.rt.Methods())
				}
				h.wantLoadEndLine(first.token, podIssue, podLoadWant{loaded: 2*time.Second + podLoadSettle, endedBy: "registration",
					registered: true, registeredAfter: 2 * time.Second})
				h.wantStartLine(second.token, podIssue, &podWait{claim: first.token, waited: 2*time.Second + podLoadSettle, endedBy: "load end"})
			})
		}
	})

	t.Run("a suspension of the waiting claim returns at once and cuts its start short", func(t *testing.T) {
		h := podHarness(t, "")
		first, second, third := h.waitingTrio()

		h.stopsWithoutWaiting(second, RequestSuspend{Claim: second.token, By: SuspendedByOperator, Reason: "a person stops the role"}, StateSuspended)

		// The start behind it is the next one the gate grants.
		h.loadEnds(first)
		h.wantGrantedStart(third, 1)
		h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, waited: podLoadSettle, endedBy: "load end"})
		h.wantStarts(second.token, 0)
	})

	t.Run("a retirement of the waiting claim does not wait either and retires it", func(t *testing.T) {
		h := podHarness(t, "")
		first, second, third := h.waitingTrio()

		h.stopsWithoutWaiting(second, RequestStop{Claim: second.token}, StateRetired)

		// Nothing waits on the retired claim to launch it again.
		h.clock.Advance(testProbe)
		second.m.Wait()
		h.wantClaimState(second, StateRetired)
		h.wantStarts(second.token, 0)
		h.loadEnds(first)
		h.wantGrantedStart(third, 1)
		h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, waited: testProbe + podLoadSettle, endedBy: "load end"})
		h.wantStarts(second.token, 0)
	})

	t.Run("a release of the pod during a wait does not wait", func(t *testing.T) {
		h := podHarness(t, "")
		first, second, third := h.waitingTrio()

		// The idle release of the pod suspends every claim of the pod, one role stop at a time:
		// the waiting one, then the loading one.
		release := func(r *podRole) Event {
			return RequestSuspend{Claim: r.token, By: SuspendedByReleased, Reason: "the issue's idle pod is released"}
		}
		h.stopsWithoutWaiting(second, release(second), StateSuspended)

		// The loading claim's agent has not registered, so its release is held until the agent is
		// ready, as a suspension of a launch is: answered at once, without waiting for the load.
		if err := promptly(t, "the loading claim's release", h.postAsync(first.m, release(first))); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the loading claim's release answered %v, want %v", err, ErrSuspendHeld)
		}
		h.wantClaimState(first, StateLaunching)
		if waiting := h.podWaiting(); !slices.Equal(waiting, []claim.Token{third.token}) {
			t.Fatalf("starts waiting for the pod = %v, want the third role's alone", waiting)
		}

		// Its agent comes up and the held release runs: the load ends with the suspension, and
		// the start behind the released one is the next the gate grants.
		h.registered(first)
		h.mustPostTo(first.m, RequestReady{Claim: first.token, Generation: first.m.View().Claim.Generation, Session: first.session})
		h.wantClaimState(first, StateSuspended)
		h.wantLoadEndLine(first.token, podIssue, podLoadWant{endedBy: "suspension", registered: true})
		h.wantGrantedStart(third, 1)
		h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, endedBy: "suspension"})
		for _, r := range []*podRole{first, second} {
			if by := r.m.View().Claim.SuspendedBy; by != SuspendedByReleased {
				t.Fatalf("%s is suspended by %q, want %q", r.token, by, SuspendedByReleased)
			}
		}
		h.wantNoStartsAfter(first, second)
	})

	t.Run("a deletion of the pod during a wait does not wait", func(t *testing.T) {
		h := podHarness(t, "")
		first, second, third := h.waitingTrio()

		// An issue that closes has its pod deleted, and its close retires every claim of the pod,
		// one row at a time: the waiting one, then the loading one.
		closed := func(r *podRole) Event { return RequestIssueClose{Claim: r.token} }
		h.stopsWithoutWaiting(second, closed(second), StateRetired)

		if err := promptly(t, "the loading claim's close", h.postAsync(first.m, closed(first))); err != nil {
			t.Fatalf("the loading claim's close: %v", err)
		}
		h.wantClaimState(first, StateRetired)
		h.wantLoadEndLine(first.token, podIssue, podLoadWant{endedBy: "retirement"})
		h.wantCalls("Release", 2)

		// The start behind the closed one, whose own close has not come yet, is the next the gate
		// grants.
		h.wantGrantedStart(third, 1)
		h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, endedBy: "retirement"})

		// A close keeps each claim's session whole, for a reopen to resume.
		for _, r := range []*podRole{first, second} {
			if c := r.m.View().Claim; c.Session != r.session || c.SessionFile != r.sessionFile || c.Locator != nil {
				t.Fatalf("%s is %+v after the close, want it retired with its session and resume path kept and no locator", r.token, c)
			}
		}
		h.wantNoStartsAfter(first, second)
	})

	t.Run("a close of the tree during a wait does not wait", func(t *testing.T) {
		h := podHarness(t, "")
		first, second, third := h.waitingTrio()

		h.stopsWithoutWaiting(second, RequestTreeClose{Claim: second.token}, StateRetired)

		h.loadEnds(first)
		h.wantGrantedStart(third, 1)
		h.wantStartLine(third.token, podIssue, &podWait{claim: first.token, waited: podLoadSettle, endedBy: "load end"})
		h.wantStarts(second.token, 0)
	})

	t.Run("the daemon's stop takes a parked start out of the gate, a granted one it had not run as a failed launch", func(t *testing.T) {
		h := podHarness(t, "")
		stopping, stop := context.WithCancel(h.ctx)
		t.Cleanup(stop)
		first := h.podRole(claim.RoleArchitect, StateSuspended)
		second := h.podRoleUnder(stopping, claim.RoleImplementer, StateSuspended)
		third := h.podRole(claim.RoleTester, StateSuspended)
		h.mustPostTo(first.m, RequestResume{Claim: first.token})
		for _, r := range []*podRole{second, third} {
			h.mustPostTo(r.m, RequestResume{Claim: r.token})
			h.wantWaitsForPod(r, 0)
		}

		// The first role's load ends and the gate grants the second's start, whose machine has
		// not run it yet: the decision the grant posts waits for the machine's lock.
		second.m.mu.Lock()
		h.loadEnds(first)
		if waiting := h.podWaiting(); !slices.Equal(waiting, []claim.Token{third.token}) {
			second.m.mu.Unlock()
			t.Fatalf("starts waiting for the pod = %v once the second's start was granted, want the third's alone", waiting)
		}

		// The daemon stops the second's machine: its granted start leaves the gate as a failed
		// launch, and the third's start is granted at once.
		stop()
		h.wantGrantedStart(third, 1)
		h.wantStartLine(third.token, podIssue, &podWait{claim: second.token, waited: podLoadSettle, endedBy: "failed launch"})

		// The grant's own decision, once it has the lock, starts nothing.
		second.m.mu.Unlock()
		testwait.Eventually(t, "the second's grant to reach its machine", func() bool {
			second.m.mu.Lock()
			defer second.m.mu.Unlock()
			_, armed := second.m.timers[TimerPodStart]
			return !armed
		})
		h.wantStarts(second.token, 0)
		if lines := h.logs.lines("launch cut short while it waited for its issue pod's start", "claim="+string(second.token)); len(lines) != 1 {
			t.Fatalf("cut-short lines of the stopped role = %d, want 1", len(lines))
		}
		h.wantClaimState(second, StateLaunching)
	})
}

// The starts waiting in one pod go in the spec's order (dispatch://INFRA-5297/spec R3), whatever
// order they were asked in: the role whose task or message caused the pod's start goes first, and
// the others in the order asked, a later request's start included. Where no request raised one —
// relaunches after deaths, as a roles container's restart makes — the first start to reach a pod
// that holds nothing goes first, whatever its role, and the ones that wait behind it go with the
// role working the issue's phase first, then the others in the order of claim.Roles.
func TestThePodsStartsGoInTheDesignsOrder(t *testing.T) {
	t.Run("a start a request raised goes first, then the others in the order asked", func(t *testing.T) {
		// The tester works the phase, which a start a request raised goes ahead of.
		h := podHarness(t, claim.RoleTester)
		holder := h.podRole(claim.RoleImplementer, StateSuspended)
		reviewer, tester := h.podRole(claim.RoleReviewer, StateSuspended), h.podRole(claim.RoleTester, StateSuspended)
		planner := h.podRole(claim.RolePlanner, StateSuspended)
		h.runs(reviewer)
		h.runs(tester)
		h.mustPostTo(holder.m, RequestResume{Claim: holder.token})

		reviewerStarted := h.died(reviewer)
		h.wantWaitsForPod(reviewer, 1)
		testerStarted := h.died(tester)
		h.wantWaitsForPod(tester, 1)
		plannerStarted := h.postAsync(planner.m, RequestResume{Claim: planner.token})
		h.wantWaitsForPod(planner, 0)

		h.wantStartOrder(holder, waitingStart{planner, plannerStarted}, waitingStart{reviewer, reviewerStarted},
			waitingStart{tester, testerStarted})
	})

	t.Run("one request's start goes first, and a later request's waits behind a recovery asked before it", func(t *testing.T) {
		h := podHarness(t, "")
		holder := h.podRole(claim.RoleImplementer, StateSuspended)
		planner, reviewer := h.podRole(claim.RolePlanner, StateSuspended), h.podRole(claim.RoleReviewer, StateSuspended)
		tester := h.podRole(claim.RoleTester, StateSuspended)
		h.runs(reviewer)
		h.mustPostTo(holder.m, RequestResume{Claim: holder.token})

		plannerStarted := h.postAsync(planner.m, RequestResume{Claim: planner.token})
		h.wantWaitsForPod(planner, 0)
		reviewerStarted := h.died(reviewer)
		h.wantWaitsForPod(reviewer, 1)
		testerStarted := h.postAsync(tester.m, RequestResume{Claim: tester.token})
		h.wantWaitsForPod(tester, 0)

		h.wantStartOrder(holder, waitingStart{planner, plannerStarted}, waitingStart{reviewer, reviewerStarted},
			waitingStart{tester, testerStarted})
	})

	t.Run("with no start a request raised, the phase worker goes first, then claim.Roles", func(t *testing.T) {
		h := podHarness(t, claim.RoleImplementer)
		holder := h.podRole(claim.RolePlanner, StateSuspended)
		reviewer, tester := h.podRole(claim.RoleReviewer, StateSuspended), h.podRole(claim.RoleTester, StateSuspended)
		implementer := h.podRole(claim.RoleImplementer, StateSuspended)
		h.runs(reviewer)
		h.runs(tester)
		h.runs(implementer)
		h.mustPostTo(holder.m, RequestResume{Claim: holder.token})

		reviewerStarted := h.died(reviewer)
		h.wantWaitsForPod(reviewer, 1)
		testerStarted := h.died(tester)
		h.wantWaitsForPod(tester, 1)
		implementerStarted := h.died(implementer)
		h.wantWaitsForPod(implementer, 1)

		h.wantStartOrder(holder, waitingStart{implementer, implementerStarted}, waitingStart{tester, testerStarted},
			waitingStart{reviewer, reviewerStarted})
	})

	t.Run("with no role holding the gate, the first start to arrive goes first whatever its role, then the phase worker, then claim.Roles", func(t *testing.T) {
		// The tester works the phase. A roles container's restart ends every role's process, and
		// their relaunches, which no request raised, reach the gate in the reverse of claim.Roles,
		// the phase worker not first: the merger's into a pod that holds nothing, which goes at
		// once, and the reviewer's, the tester's and the implementer's behind it.
		h := podHarness(t, claim.RoleTester)
		merger, reviewer := h.podRole(claim.RoleMerger, StateSuspended), h.podRole(claim.RoleReviewer, StateSuspended)
		tester, implementer := h.podRole(claim.RoleTester, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)
		for _, r := range []*podRole{merger, reviewer, tester, implementer} {
			h.runs(r)
		}
		if waiting := h.podWaiting(); len(waiting) > 0 {
			t.Fatalf("starts waiting for the pod = %v, want none before the restart", waiting)
		}

		mergerStarted := h.died(merger)
		h.wantGrantedStart(merger, 2)
		receive(t, "the merger's relaunch", mergerStarted, false)
		h.wantStartLine(merger.token, podIssue, nil)
		reviewerStarted := h.died(reviewer)
		h.wantWaitsForPod(reviewer, 1)
		testerStarted := h.died(tester)
		h.wantWaitsForPod(tester, 1)
		implementerStarted := h.died(implementer)
		h.wantWaitsForPod(implementer, 1)

		h.wantStartOrder(merger, waitingStart{tester, testerStarted}, waitingStart{implementer, implementerStarted},
			waitingStart{reviewer, reviewerStarted})
	})
}
