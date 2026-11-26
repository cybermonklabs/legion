// The booted-daemon proof of the P0 start and of the P0 bound set live: check c-bound-boot of
// dispatch://INFRA-5636, and booted evidence for its C1, C2 and C4. A P0 issue starts at once while
// the regular cap is full, the bound legion.yaml gives decides the first admission pass, and a bound
// the operator or the controller sets to a number, sets to none or clears decides the next pass,
// with no restart.
//
// It boots the whole daemon through this package's own whole-daemon harness (startDaemonWithin,
// which calls run as `legion start` does) on a real Postgres and JetStream, from the configuration
// legion.yaml loads into with an admission cap of 1 and a P0 bound of 1, over liveProof's Dispatch
// stand-in (liveDispatch), with no agent: every claim is launched by the package's fake runtime.
// Nothing is seeded. Each root reaches the daemon as Dispatch's own issue.updated event of a person
// handing it to Legion, in the shape a Dispatch that resolves priorities writes, as the hand-written
// files of internal/intake/testdata/dispatch-resolved are: `resolved_priority` and `priority_from`
// beside `priority`, and `changed`. The daemon applies the stand-in's detail read of a root it does
// not record yet in place of the event (prepareIssue), so the stand-in answers the same three keys
// for it. Each change of the bound goes through the daemon's own routes: the operator's, which
// `legion project set-p0-bound` and `clear-p0-bound` call under the operator's bearer, and the
// controller's, which the `legion` tool calls under a controller grant. Every step is judged by what
// GET /legion/v1/state answers and by the daemon's own log, never by the store, and the daemon is
// never restarted.
//
// Like admission_issues_live_test.go it carries no build tag and runs in the ordinary
// `go test ./internal/daemon/`, over the broker that package's other tests take (workflowNATS):
// `.github/go-legs.yaml` runs this package in daemon-go / daemon-go-race, the job c-bound-boot names.
package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// p0StartCap is the regular cap this proof runs under: one ordinary root fills it, so every
	// later start is one the cap does not count.
	p0StartCap = 1
	// p0StartBound is legion.yaml's admission_p0_bound this proof boots with: one P0 holder fills it.
	p0StartBound = 1
	// p0StartAncestor is the issue above the inherited roots in Dispatch, in another project, whose
	// own P0 is the one Dispatch resolves them to.
	p0StartAncestor = "OTHER-5"
	// p0StartLine and p0HoldLine are the log lines of a P0 issue's promotion (admit's promote): its
	// start outside the cap, and its refusal at the P0 bound.
	p0StartLine = "admission starts a P0 issue outside the cap"
	p0HoldLine  = "admission holds a P0 issue at the P0 bound"
	// p0BoundOperatorRoute and p0BoundControllerRoute are the two routes that set the bound; each
	// clears it at its own path with /clear after it.
	p0BoundOperatorRoute   = "/legion/v1/operator/project/p0-bound"
	p0BoundControllerRoute = "/legion/v1/project/p0-bound"
)

// The priorities the roots of this proof are handed to Legion at, as Dispatch serves them: an
// ordinary root's own P2; an explicit P0, the root's own; and an inherited P0, a root with no
// priority of its own that Dispatch resolves to P0 from p0StartAncestor, the issue above it.
var (
	p0StartOwnP2       = liveDispatchPriority{own: new(2), resolved: new(2)}
	p0StartOwnP0       = liveDispatchPriority{own: new(0), resolved: new(0)}
	p0StartInheritedP0 = liveDispatchPriority{resolved: new(0), from: p0StartAncestor}
)

// p0StartLive is one run of the proof: the booted daemon, the daemon's own log, the roots handed to
// Legion in the order each step hands one over, and the controller session the controller's
// changes are made as.
type p0StartLive struct {
	liveProof
	// logs is the daemon's own log as JSON lines, from which each step reads the starts and refusals.
	logs *syncBuffer
	// roots is every root handed to Legion so far, in that order; startedAt is the daemon's own
	// start time at the first step, which no restart may move.
	roots     []string
	startedAt time.Time
	// The roots by the role each plays: ordinary fills the cap, explicit is the first P0 start,
	// inherited the P0 root the configured bound holds back, and third to sixth are the P0 roots
	// the bound's changes start or hold.
	ordinary, explicit, inherited, third, fourth, fifth, sixth string
	// controllerSession and controllerSecret are the project's registered controller, whose
	// grants the controller's changes carry.
	controllerSession, controllerSecret string
}

// TestAP0IssueStartsAtAFullCapAndTheBoundChangesLive drives the steps of the proof against one
// booted daemon, in the order each step's state follows from the last: an ordinary root fills the
// cap of 1; an explicit P0 root starts beside it at its own event's pass; an inherited P0 root
// waits first in the line at legion.yaml's bound of 1 from its own first pass; the operator's route
// sets 2, which starts it at the change's pass, sets none, which starts a third P0 root held at 2,
// and clears, which returns to the configured 1, holds a fourth and stops none; and the
// controller's route makes the same three changes, each deciding the next pass. Every change is
// also read back from `admission.p0Bound`: the bound in force, the configured one, and who set the
// live value, when and why (p0BoundReads).
func TestAP0IssueStartsAtAFullCapAndTheBoundChangesLive(t *testing.T) {
	t.Parallel()
	r := newP0StartLive(t)
	r.step("a-an-ordinary-root-fills-the-cap", r.ordinaryRootFillsTheCap)
	r.step("b-an-explicit-p0-root-starts-at-once-at-the-full-cap", r.explicitP0StartsAtTheFullCap)
	r.step("c-an-inherited-p0-root-waits-first-at-the-configured-bound-from-its-first-pass", r.inheritedP0WaitsAtTheConfiguredBound)
	r.step("d-the-operators-set-none-and-clear-each-decide-the-next-pass", r.operatorChangesTheBound)
	r.step("e-the-controllers-set-none-and-clear-each-decide-the-next-pass", r.controllerChangesTheBound)
}

// newP0StartLive boots the daemon over the package's broker with the cap and the P0 bound of this
// proof and no issue recorded, and puts the inherited roots' ancestor on the stand-in's board.
func newP0StartLive(t *testing.T) *p0StartLive {
	t.Helper()
	proof := newLiveProofOn(t, workflowNATS(t))
	proof.cfg.AdmissionCap = p0StartCap
	proof.cfg.AdmissionP0Bound = new(p0StartBound)
	r := &p0StartLive{liveProof: proof, logs: &syncBuffer{}}
	// The issue above the inherited roots: Dispatch's, in another project, with its own P0, and
	// handed to nobody. The daemon reads it when it walks an inherited root's Dispatch chain
	// (chainOf), and finds no live root of its own there.
	r.board.place(p0StartAncestor, "in_progress", "", false)
	r.board.prioritize(p0StartAncestor, p0StartOwnP0)
	rt := fake.NewRuntime()
	// The runtime provisions each claim's workspace itself (fake.Runtime's InPod), so an architect's
	// launch reaches the fake without the daemon cloning acme/widgets, which does not exist.
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = admissionIssuesGitHub(t)
	log := slog.New(slog.NewMultiHandler(r.log().Handler(), slog.NewJSONHandler(r.logs, nil)))
	r.d = startDaemonWithin(t, r.cfg, o, log, 60*time.Second)
	r.note("booted: admission cap %d, legion.yaml's admission_p0_bound %d, no issue recorded; %s on Dispatch's board at its own P0",
		p0StartCap, p0StartBound, p0StartAncestor)
	return r
}

// handOver is a person handing root number n, a new root of the project, to Legion in Dispatch:
// the board shows it todo with Legion's label under parent ("" for none) at priority, and
// Dispatch's issue.updated event of that move is published for the daemon's consumer, which reads
// it as it reads any issue event. The event's issue has the keys of the hand-written events of
// internal/intake/testdata/dispatch-resolved, with the title and the repository a real event
// carries too. It answers the root's key.
func (r *p0StartLive) handOver(n int, parent string, priority liveDispatchPriority) string {
	t := r.t
	t.Helper()
	key := fmt.Sprintf("%s-%d", r.cfg.Project, n)
	r.board.place(key, "todo", parent, true)
	r.board.prioritize(key, priority)
	var parentKey any
	ancestors := []string{}
	if parent != "" {
		parentKey, ancestors = parent, []string{parent}
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	issue := map[string]any{
		"key": key, "title": key, "number": n, "project": r.cfg.Project, "parent": parentKey, "ancestors": ancestors,
		"status": "todo", "changed": []string{"status", "labels"}, "rank": "U", "labels": liveDispatchLabels(true),
		"claim": nil, "blocked_by": []string{}, "last_seq": 2, "resolved_repo": liveRepo, "closed_at": nil,
		"created_at": at, "updated_at": at,
	}
	maps.Copy(issue, priority.fields())
	payload, err := json.Marshal(map[string]any{
		"id": n, "issue_key": key, "artifact_id": nil, "project": r.cfg.Project, "seq": 2, "type": "issue.updated",
		"actor": map[string]string{"kind": "user", "id": "human"}, "notify": true, "created_at": at, "payload": issue,
	})
	if err != nil {
		t.Fatalf("encode the Dispatch event of %s: %v", key, err)
	}
	r.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch", key+"-handed-over", string(payload))
	r.roots = append(r.roots, key)
	fields, err := json.Marshal(priority.fields())
	if err != nil {
		t.Fatal(err)
	}
	r.note("drove Dispatch's issue.updated of a person handing %s to Legion: todo, parent %v, %s", key, parentKey, fields)
	return key
}

// quoted is GET /legion/v1/state now, with its `admission` member printed under the step as the
// route served it and one line per root of the run naming the fields the step turns on. Every
// assertion of this proof reads the value it returns, so what the run prints is the answer the
// step was judged on.
func (r *p0StartLive) quoted(what string) api.State {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET the state = %d %s, want 200", status, body)
	}
	var served struct {
		Admission json.RawMessage `json:"admission"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	r.note("%s: admission %s", what, served.Admission)
	for _, key := range r.roots {
		issue, recorded := state.Issues[key]
		if !recorded {
			r.note("   issues.%s: the state records no such issue", key)
			continue
		}
		line := "none"
		if issue.WaitingLine != nil {
			line = fmt.Sprintf("place %d at %s: %s", issue.WaitingLine.Place, priorityText(issue.WaitingLine.Priority), issue.WaitingLine.Reason)
		}
		r.note("   issues.%s: phase %s, slot %s, priority %s, resolvedPriority %s, priorityFrom %q, waitingLine %s",
			key, issue.Phase, slotOf(issue), priorityText(issue.Priority), priorityText(issue.ResolvedPriority), issue.PriorityFrom, line)
	}
	return state
}

// waitForState polls GET /legion/v1/state until want holds of it, then prints and returns that
// answer under the step's heading. A wait that gives up prints the answer it last had, since that
// is what the step was judged on either way.
func (r *p0StartLive) waitForState(what string, want func(api.State) bool) api.State {
	r.t.Helper()
	held := false
	defer func() {
		if !held {
			r.quoted("the last answer before the wait for " + what + " gave up")
		}
	}()
	testwait.Eventually(r.t, what, func() bool { return want(r.d.state()) })
	held = true
	return r.quoted(what)
}

// holdsSlot says whether the state shows key holding an admission slot: in `admission.active`, with
// its slot on its own view.
func holdsSlot(state api.State, key string) bool {
	return slices.Contains(state.Admission.Active, key) && state.Issues[key].Slot != nil
}

// waitsFirst says whether the state shows key first in the admission line, holding no slot.
func waitsFirst(state api.State, key string) bool {
	waiting := state.Admission.Waiting
	issue := state.Issues[key]
	return len(waiting) > 0 && waiting[0] == key && issue.WaitingLine != nil && issue.Slot == nil
}

// priorityText is a priority of the state as Dispatch names it, P0 to P3, or "none" for a field
// that is null or absent.
func priorityText(priority *api.PriorityView) string {
	if priority == nil || record.Priority(*priority) == record.NoPriority {
		return "none"
	}
	return record.Priority(*priority).String()
}

// boundText is a P0 bound as the daemon's log writes it: the number, or "none".
func boundText(bound *int) string {
	if bound == nil {
		return "none"
	}
	return fmt.Sprint(*bound)
}

// requireHolders fails the step unless the state's `admission.active` is want, sorted, and its
// `admission.p0Holders` is p0: the holders the cap and the bound count.
func (r *p0StartLive) requireHolders(state api.State, p0 int, want ...string) {
	r.t.Helper()
	if got := holders(state); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		r.t.Fatalf("admission.active = %v, want %v", got, slices.Sorted(slices.Values(want)))
	}
	if got := state.Admission.P0Holders; got == nil || *got != p0 {
		r.t.Fatalf("admission.p0Holders = %v, want %d of the %d holders", got, p0, len(want))
	}
}

// requirePriorities fails the step unless the state gives key the own priority, the resolved one
// and the issue it comes from that Dispatch sent.
func (r *p0StartLive) requirePriorities(state api.State, key, own, resolved, from string) {
	r.t.Helper()
	got := state.Issues[key]
	if priorityText(got.Priority) != own || priorityText(got.ResolvedPriority) != resolved || got.PriorityFrom != from {
		r.t.Fatalf("%s's priority %s, resolvedPriority %s, priorityFrom %q; want %s, %s and %q as Dispatch sent them",
			key, priorityText(got.Priority), priorityText(got.ResolvedPriority), got.PriorityFrom, own, resolved, from)
	}
}

// logged waits for the daemon's log to hold a line msg about issue whose fields are want, each
// compared as the JSON line writes it, and returns it. A refusal is logged at every pass that
// refuses the issue, so the line waited for is the one with the counts and the bound the step drove,
// not the last.
func (r *p0StartLive) logged(msg, issue string, want map[string]any) map[string]any {
	t := r.t
	t.Helper()
	var found map[string]any
	testwait.Eventually(t, fmt.Sprintf("the daemon's line %q for %s with %v", msg, issue, want), func() bool {
		for _, line := range loggedMessages(t, r.logs.String(), msg) {
			if line["issue"] != issue {
				continue
			}
			if !slices.ContainsFunc(slices.Collect(maps.Keys(want)), func(field string) bool {
				return fmt.Sprint(line[field]) != fmt.Sprint(want[field])
			}) {
				found = line
				return true
			}
		}
		return false
	})
	r.note("logged: %q issue %s, p0_holders %v, other_holders %v, cap %v, p0_bound %v",
		msg, issue, found["p0_holders"], found["other_holders"], found["cap"], found["p0_bound"])
	return found
}

// requireStarts fails the step unless the daemon's log holds one P0 start for each of keys and
// for no other issue: a P0 start is logged once, at the pass that grants its slot.
func (r *p0StartLive) requireStarts(keys ...string) {
	r.t.Helper()
	var started []string
	for _, line := range loggedMessages(r.t, r.logs.String(), p0StartLine) {
		started = append(started, fmt.Sprint(line["issue"]))
	}
	slices.Sort(started)
	if want := slices.Sorted(slices.Values(keys)); !slices.Equal(started, want) {
		r.t.Fatalf("the daemon logged P0 starts of %v, want one each of %v", started, want)
	}
}

// p0BoundReads is step (f) of the plan, made at every reading of the bound: view, the state's
// `admission.p0Bound` or a change's answer, gives inForce as the bound in force, legion.yaml's
// bound as the configured one, and the live value that stands where live names one (its bound,
// who set it and why), set no earlier than since and no later than now. A nil live is no live
// value: the boot's, and a clear's.
func (r *p0StartLive) p0BoundReads(what string, view *api.P0BoundView, inForce *int, live *api.P0BoundLiveView, since time.Time) {
	t := r.t
	t.Helper()
	if view == nil {
		t.Fatalf("%s gives no P0 bound, want %s in force", what, boundText(inForce))
	}
	if boundText(view.Bound) != boundText(inForce) || boundText(view.Configured) != boundText(new(p0StartBound)) {
		t.Fatalf("%s gives the bound %s in force and %s in legion.yaml, want %s and %d",
			what, boundText(view.Bound), boundText(view.Configured), boundText(inForce), p0StartBound)
	}
	switch {
	case live == nil && view.Live != nil:
		t.Fatalf("%s gives the live value %+v, want none to stand", what, *view.Live)
	case live == nil:
	case view.Live == nil:
		t.Fatalf("%s gives no live value, want %s set by %+v", what, boundText(live.Bound), live.SetBy)
	case boundText(view.Live.Bound) != boundText(live.Bound) || view.Live.SetBy != live.SetBy || view.Live.Reason != live.Reason:
		t.Fatalf("%s gives the live value %s set by %+v for %q, want %s set by %+v for %q",
			what, boundText(view.Live.Bound), view.Live.SetBy, view.Live.Reason, boundText(live.Bound), live.SetBy, live.Reason)
	// The store keeps the time to the microsecond, so the lower end is since to the millisecond.
	case view.Live.SetAt.Before(since.Truncate(time.Millisecond)) || view.Live.SetAt.After(time.Now()):
		t.Fatalf("%s gives the live value set at %s, want a moment from %s to now", what, view.Live.SetAt, since)
	}
	r.note("read back from %s: the bound %s in force, %s in legion.yaml, live %s", what, boundText(view.Bound), boundText(view.Configured), liveText(view.Live))
}

// liveText is a live P0 bound as a step's output names it.
func liveText(live *api.P0BoundLiveView) string {
	if live == nil {
		return "none"
	}
	by := live.SetBy.Kind
	if live.SetBy.Session != "" {
		by += " " + live.SetBy.Session
	}
	return fmt.Sprintf("%s set by %s at %s: %s", boundText(live.Bound), by,
		live.SetAt.Format(time.RFC3339Nano), live.Reason)
}

// change posts one change of the P0 bound to path, under the operator's bearer where operator is
// set, and requires 200, answering the bound the daemon then admits under.
func (r *p0StartLive) change(path string, body any, operator bool) api.P0BoundView {
	t := r.t
	t.Helper()
	status, answer := r.d.request(http.MethodPost, path, body, operator)
	if status != http.StatusOK {
		t.Fatalf("POST %s = %d %s, want 200", path, status, answer)
	}
	var view api.P0BoundView
	if err := json.Unmarshal(answer, &view); err != nil {
		t.Fatalf("decode the answer of POST %s %s: %v", path, answer, err)
	}
	r.note("drove POST %s: %d %s", path, status, strings.TrimSpace(string(answer)))
	return view
}

// Step a. An ordinary root, at its own P2, takes the cap's one slot at its own event's pass, as
// any root does, and the state gives its two priorities and no P0 holder, under legion.yaml's
// bound with no live value.
func (r *p0StartLive) ordinaryRootFillsTheCap() {
	t := r.t
	r.ordinary = r.handOver(1, "", p0StartOwnP2)
	state := r.waitForState(r.ordinary+" holding the cap's one slot", func(state api.State) bool {
		return holdsSlot(state, r.ordinary)
	})
	if state.Admission.Cap != p0StartCap {
		t.Fatalf("admission.cap = %d, want legion.yaml's %d", state.Admission.Cap, p0StartCap)
	}
	r.requireHolders(state, 0, r.ordinary)
	r.requirePriorities(state, r.ordinary, "P2", "P2", "")
	r.p0BoundReads("the state at the first step", state.Admission.P0Bound, new(p0StartBound), nil, time.Time{})
	r.requireStarts()
	r.startedAt = state.Daemon.StartedAt
}

// Step b. An explicit P0 root, its own priority 0 and Dispatch's resolved 0, starts at once at its
// own event's pass, though the ordinary root fills the cap: the cap counts the holders that are not
// P0, so `admission.p0Holders` is 1 beside the full cap, and the start is logged with the issue,
// both holder counts and the cap.
func (r *p0StartLive) explicitP0StartsAtTheFullCap() {
	r.explicit = r.handOver(2, "", p0StartOwnP0)
	state := r.waitForState(r.explicit+" holding a slot beside the full cap", func(state api.State) bool {
		return holdsSlot(state, r.explicit)
	})
	r.requireHolders(state, 1, r.ordinary, r.explicit)
	r.requirePriorities(state, r.explicit, "P0", "P0", "")
	r.logged(p0StartLine, r.explicit, map[string]any{"p0_holders": 1, "other_holders": 1, "cap": p0StartCap})
	r.requireStarts(r.explicit)
}

// Step c. A root with no priority of its own, which Dispatch resolves to P0 from p0StartAncestor,
// the issue above it in another project, is P0 for admission as an explicit one is, and the
// configured bound of 1 holds it from its own first pass: one P0 issue already holds a slot. It
// waits first in the line, in the P0 band, the reason names p0StartAncestor as where its priority
// comes from, and the refusal is logged with the bound.
func (r *p0StartLive) inheritedP0WaitsAtTheConfiguredBound() {
	t := r.t
	r.inherited = r.handOver(3, p0StartAncestor, p0StartInheritedP0)
	state := r.waitForState(r.inherited+" waiting first in the line", func(state api.State) bool {
		return waitsFirst(state, r.inherited)
	})
	r.requireHolders(state, 1, r.ordinary, r.explicit)
	r.requirePriorities(state, r.inherited, "none", "P0", p0StartAncestor)
	line := state.Issues[r.inherited].WaitingLine
	if line.Place != 1 || priorityText(line.Priority) != "P0" {
		t.Fatalf("%s's waitingLine is place %d at %s, want place 1 at P0", r.inherited, line.Place, priorityText(line.Priority))
	}
	if from := "the priority of " + p0StartAncestor; !strings.Contains(line.Reason, from) {
		t.Fatalf("%s's waitingLine reason %q, want it to name %q, where Dispatch's priority_from says its P0 comes from", r.inherited, line.Reason, from)
	}
	r.logged(p0HoldLine, r.inherited, map[string]any{"p0_bound": p0StartBound, "p0_holders": 1, "other_holders": 1, "cap": p0StartCap})
	r.requireStarts(r.explicit)
}

// Step d. The operator changes the bound through the route `legion project set-p0-bound` and
// `clear-p0-bound` call, and each change decides the pass its own fact makes, with no restart. A
// set of 2 starts the inherited root the configured 1 held. A set of none starts a third P0 root
// the bound of 2 held. A clear returns to legion.yaml's 1: it stops none of the three P0 holders,
// though they are more than that bound, and starts no further one, so a fourth waits at its first
// pass.
func (r *p0StartLive) operatorChangesTheBound() {
	operator := api.PauseActorView{Kind: "operator"}
	r.boundStartsTheWaitingRoot(r.inherited, "the operator's set of 2", p0BoundOperatorRoute,
		api.OperatorP0BoundRequest{Bound: new(2), Reason: "two P0 issues at once"}, true,
		&api.P0BoundLiveView{Bound: new(2), SetBy: operator, Reason: "two P0 issues at once"}, 2, r.ordinary, r.explicit, r.inherited)

	r.third = r.handOver(4, "", p0StartOwnP0)
	r.heldAtTheBound(r.third, 2, 2, r.ordinary, r.explicit, r.inherited)
	r.boundStartsTheWaitingRoot(r.third, "the operator's set of none", p0BoundOperatorRoute,
		api.OperatorP0BoundRequest{Unbounded: true, Reason: "start every P0 issue"}, true,
		&api.P0BoundLiveView{SetBy: operator, Reason: "start every P0 issue"}, 3, r.ordinary, r.explicit, r.inherited, r.third)

	r.clearHoldsTheNext("the operator's clear", p0BoundOperatorRoute+"/clear",
		api.OperatorP0BoundClearRequest{Reason: "back to legion.yaml"}, true, func() string {
			r.fourth = r.handOver(5, p0StartAncestor, p0StartInheritedP0)
			return r.fourth
		}, 3, r.ordinary, r.explicit, r.inherited, r.third)
	r.requireStarts(r.explicit, r.inherited, r.third)
}

// Step e. The controller makes the same three changes through the route the `legion` tool calls,
// each under a grant minted for the registered controller session, and each decides its own pass
// as the operator's did: a set of 4 starts the fourth P0 root the configured 1 held, a set of none
// starts a fifth the bound of 4 held, and a clear returns to legion.yaml's 1, stops none of the five
// P0 holders and holds a sixth from its first pass. The state names the controller's session as who
// set each live value, and the daemon is the one boot it was at the first step.
func (r *p0StartLive) controllerChangesTheBound() {
	t := r.t
	r.registerController()
	controller := api.PauseActorView{Kind: "controller", Session: r.controllerSession}
	r.boundStartsTheWaitingRoot(r.fourth, "the controller's set of 4", p0BoundControllerRoute,
		api.ProjectP0BoundRequest{GrantID: r.controllerGrant(), Bound: new(4), Reason: "four P0 issues at once"}, false,
		&api.P0BoundLiveView{Bound: new(4), SetBy: controller, Reason: "four P0 issues at once"}, 4,
		r.ordinary, r.explicit, r.inherited, r.third, r.fourth)

	r.fifth = r.handOver(6, "", p0StartOwnP0)
	r.heldAtTheBound(r.fifth, 4, 4, r.ordinary, r.explicit, r.inherited, r.third, r.fourth)
	r.boundStartsTheWaitingRoot(r.fifth, "the controller's set of none", p0BoundControllerRoute,
		api.ProjectP0BoundRequest{GrantID: r.controllerGrant(), Unbounded: true, Reason: "start every P0 issue again"}, false,
		&api.P0BoundLiveView{SetBy: controller, Reason: "start every P0 issue again"}, 5,
		r.ordinary, r.explicit, r.inherited, r.third, r.fourth, r.fifth)

	state := r.clearHoldsTheNext("the controller's clear", p0BoundControllerRoute+"/clear",
		api.ProjectP0BoundClearRequest{GrantID: r.controllerGrant(), Reason: "back to legion.yaml again"}, false, func() string {
			r.sixth = r.handOver(7, p0StartAncestor, p0StartInheritedP0)
			return r.sixth
		}, 5, r.ordinary, r.explicit, r.inherited, r.third, r.fourth, r.fifth)
	r.requireStarts(r.explicit, r.inherited, r.third, r.fourth, r.fifth)
	if state.Daemon.Boots != 1 || !state.Daemon.StartedAt.Equal(r.startedAt) {
		t.Fatalf("the daemon is boot %d started %s, want the one boot started %s at the first step: no step restarts it",
			state.Daemon.Boots, state.Daemon.StartedAt, r.startedAt)
	}
}

// boundStartsTheWaitingRoot makes the change body of the bound at route, which must leave the
// bound live names in force, and requires the state to show waiting, the P0 root the bound before
// it held first in the line, holding a slot after the change's own pass, with p0 P0 holders among
// holders, and the start logged under that bound.
func (r *p0StartLive) boundStartsTheWaitingRoot(waiting, what, route string, body any, operator bool,
	live *api.P0BoundLiveView, p0 int, holders ...string,
) {
	r.t.Helper()
	since := time.Now()
	answer := r.change(route, body, operator)
	r.p0BoundReads("the answer of "+what, &answer, live.Bound, live, since)
	state := r.waitForState(waiting+" holding a slot after "+what, func(state api.State) bool {
		return holdsSlot(state, waiting)
	})
	r.p0BoundReads("the state after "+what, state.Admission.P0Bound, live.Bound, live, since)
	r.requireHolders(state, p0, holders...)
	r.logged(p0StartLine, waiting, map[string]any{"p0_holders": p0, "other_holders": 1, "cap": p0StartCap, "p0_bound": boundText(live.Bound)})
}

// heldAtTheBound requires the state to show root, a P0 root just handed over, first in the line
// from its own first pass, holding no slot while p0 P0 issues among holders fill the bound in force,
// and the refusal logged naming that bound.
func (r *p0StartLive) heldAtTheBound(root string, bound, p0 int, holders ...string) {
	r.t.Helper()
	state := r.waitForState(fmt.Sprintf("%s waiting first in the line at the bound of %d", root, bound), func(state api.State) bool {
		return waitsFirst(state, root)
	})
	r.requireHolders(state, p0, holders...)
	r.logged(p0HoldLine, root, map[string]any{"p0_bound": bound, "p0_holders": p0, "other_holders": 1, "cap": p0StartCap})
}

// clearHoldsTheNext makes the clear body at route and requires it to return the bound in force to
// legion.yaml's with no live value, stopping none of holders, p0 of them P0, though they are more
// than that bound; then hands over the P0 root next answers, which that bound holds from its first
// pass. It answers the state of that last read.
func (r *p0StartLive) clearHoldsTheNext(what, route string, body any, operator bool, next func() string, p0 int, holders ...string) api.State {
	r.t.Helper()
	since := time.Now()
	answer := r.change(route, body, operator)
	r.p0BoundReads("the answer of "+what, &answer, new(p0StartBound), nil, since)
	state := r.quoted("the state after " + what)
	r.p0BoundReads("the state after "+what, state.Admission.P0Bound, new(p0StartBound), nil, since)
	r.requireHolders(state, p0, holders...)
	root := next()
	r.heldAtTheBound(root, p0StartBound, p0, holders...)
	state = r.quoted("the state once " + root + " waits")
	r.p0BoundReads("the state once "+root+" waits", state.Admission.P0Bound, new(p0StartBound), nil, since)
	return state
}

// registerController registers a controller session for the project, as `legion controller start`
// registers the operator's own: the operator's bearer buys a capability and a session registers
// with it, and its registration secret mints the grants the controller's routes take
// (controllerGrant). No controller process runs: its routes are driven as its plugin drives them.
func (r *p0StartLive) registerController() {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/controller/secret",
		api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion()}, true)
	if status != http.StatusOK {
		t.Fatalf("the controller's boot secret = %d %s, want 200", status, body)
	}
	var capability api.ControllerSecretResponse
	if err := json.Unmarshal(body, &capability); err != nil {
		t.Fatalf("decode the controller's boot secret %s: %v", body, err)
	}
	r.controllerSession = "ses_controller_" + randomSuffix(t)
	status, body = r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability.Secret, SessionID: r.controllerSession, OmpSessionFile: "/sessions/" + r.controllerSession + ".jsonl",
		AgentID: r.controllerSession, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the controller = %d %s, want 200", status, body)
	}
	var registered api.ControllerRegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the controller's registration %s: %v", body, err)
	}
	r.controllerSecret = registered.Secret
	r.note("the project's controller is registered as %s, whose grants the controller's changes carry", r.controllerSession)
}

// controllerGrant mints one controller grant over the daemon's own route, the controller-session
// form, as the `legion` tool mints one per operation.
func (r *p0StartLive) controllerGrant() string {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: r.controllerSession, Secret: r.controllerSecret}, false)
	if status != http.StatusOK {
		t.Fatalf("mint the controller's grant = %d %s, want 200", status, body)
	}
	var minted api.GrantResponse
	if err := json.Unmarshal(body, &minted); err != nil {
		t.Fatalf("decode the controller's grant %s: %v", body, err)
	}
	return minted.GrantID
}
