package supervise

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// recordedRegistration is one call of Deps.PodRegistered: the issue and pod it named, and what
// the store held of the claim at that moment.
type recordedRegistration struct {
	issue, pod string
	stored     Claim
}

// registrationRecords is a Deps.PodRegistered that records each call and answers fail, nil to
// succeed.
type registrationRecords struct {
	h     *harness
	calls []recordedRegistration
	fail  error
}

func (r *registrationRecords) record(_ context.Context, issue, pod string) error {
	r.calls = append(r.calls, recordedRegistration{issue: issue, pod: pod, stored: r.h.store.load(r.h.token)})
	return r.fail
}

// withPodRegistrations gives the harness's machine a record of each registration in a pod.
func (h *harness) withPodRegistrations() *registrationRecords {
	h.t.Helper()
	r := &registrationRecords{h: h}
	h.deps.PodRegistered = r.record
	h.start(h.store.load(h.token))
	return r
}

// launchInPod launches the harness's claim into pod, a role of an issue pod.
func (h *harness) launchInPod(pod string) {
	h.t.Helper()
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: runtime.SandboxIncarnation(pod, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: 1},
	}})
	h.launch()
	h.connect()
}

// A role of an issue pod registering is recorded against its issue and its pod before the
// registration is persisted and answered: a saved copy of the workspace sent to that pod stays
// unspent until then, and the role may change the workspace from its registration on. Each later
// registration of the same session is recorded again, so whichever registration the daemon
// answers, the store has the pod's registration first.
func TestUnspentCopyRegistrationInAPodIsRecordedBeforeItIsAnswered(t *testing.T) {
	h := newHarness(t)
	recorded := h.withPodRegistrations()
	h.launchInPod("pod-uid-9")
	h.register()
	if len(recorded.calls) != 1 || recorded.calls[0].issue != "LEGION-209" || recorded.calls[0].pod != "pod-uid-9" {
		t.Fatalf("the registration recorded %+v, want one record of LEGION-209 in pod-uid-9", recorded.calls)
	}
	if before := recorded.calls[0].stored; before.State == StateRegistered || before.Session != "" {
		t.Fatalf("the store held the claim %s with session %q when the pod's registration was recorded, want it not yet registered",
			before.State, before.Session)
	}
	h.wantState(StateRegistered)

	h.register()
	h.ready()
	h.register()
	if len(recorded.calls) != 3 {
		t.Fatalf("three registrations recorded %d times, want each recorded: %+v", len(recorded.calls), recorded.calls)
	}
	for _, call := range recorded.calls[1:] {
		if call.issue != "LEGION-209" || call.pod != "pod-uid-9" {
			t.Fatalf("a registration again recorded %+v, want LEGION-209 in pod-uid-9", call)
		}
	}
}

// A registration whose record the store refuses is refused, and leaves the claim as it was, in
// memory and in the store: answered, its role would run while the store still read a copy sent
// to the pod as unspent, so a restarted roles container would be sent that copy over the role's
// work. The agent is left to its registration deadline, and its next registration, recorded, is
// taken. A registration again whose record fails is refused the same way.
func TestUnspentCopyRegistrationWhoseRecordFailsIsRefused(t *testing.T) {
	h := newHarness(t)
	recorded := h.withPodRegistrations()
	h.launchInPod("pod-uid-9")
	recorded.fail = errors.New("the store cannot be written")
	if err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract}); err == nil {
		t.Fatal("a registration whose pod's record failed was taken")
	}
	if c := h.claim(); c.State != StateShimConnected || c.Session != "" {
		t.Fatalf("the refused registration left the claim %s with session %q, want it as it was", c.State, c.Session)
	}
	if stored := h.store.load(h.token); stored.State == StateRegistered || stored.Session != "" {
		t.Fatalf("the refused registration was persisted: %s with session %q", stored.State, stored.Session)
	}

	recorded.fail = nil
	h.register()
	h.wantState(StateRegistered)

	recorded.fail = errors.New("the store cannot be written")
	if err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: "/state/sessions/moved.jsonl",
		CapabilityHash: []byte("capability-again"), PluginContract: h.deps.PluginContract}); err == nil {
		t.Fatal("a registration again whose pod's record failed was taken")
	}
	if stored := h.store.load(h.token); stored.SessionFile != sessionFile || string(stored.CapabilityHash) != "capability" {
		t.Fatalf("the refused registration again was persisted: session file %q, capability %q", stored.SessionFile, stored.CapabilityHash)
	}
}

// A claim whose process runs in no pod — the tmux runtime's — records nothing at its
// registration: no copy is ever sent to it.
func TestUnspentCopyRegistrationOfAClaimInNoPodRecordsNothing(t *testing.T) {
	h := newHarness(t)
	recorded := h.withPodRegistrations()
	h.launch()
	h.connect()
	h.register()
	h.ready()
	if len(recorded.calls) != 0 {
		t.Fatalf("a claim in no pod recorded %+v, want nothing", recorded.calls)
	}
}

// A registration refused because another claim holds its session records no registration of the
// pod: the session check runs before the record (holdRegistration), so the refused role spends no
// copy sent to its pod and the pod's next role still restores it. The negative control is the
// record moved ahead of holdRecorded in holdRegistration, where the refused registration records
// the pod.
func TestUnspentCopyRegistrationRefusedForItsSessionRecordsNothing(t *testing.T) {
	h := newHarness(t)
	recorded := h.withPodRegistrations()
	h.launchInPod("pod-uid-9")
	other := claim.Token("legion-legion-legion-999-tester")
	if holders, err := h.store.HoldSessions(h.ctx, "", other, []Hold{{Kind: HoldSessionID, Name: session}}); err != nil || len(holders) != 1 || holders[0] != other {
		t.Fatalf("another claim's hold of %s = %v, %v, want it held for %s", session, holders, err, other)
	}
	err := h.handle(RequestRegister{
		Claim:          h.token,
		Generation:     h.generation(),
		Session:        session,
		SessionFile:    sessionFile,
		CapabilityHash: []byte("capability"),
		PluginContract: h.deps.PluginContract,
	})
	if refusal := (claim.Refusal{}); err == nil || !errors.As(err, &refusal) {
		t.Fatalf("the registration of a session another claim holds returned %v, want the hold's refusal", err)
	}
	if len(recorded.calls) != 0 {
		t.Fatalf("a registration refused for its session recorded %+v, want no registration of its pod", recorded.calls)
	}
}

// A registration again refused because another claim holds its session records no registration of
// the pod either: reregister holds the session before it records, as register does
// (holdRegistration). The claim registered its session in the pod, and the store now holds that
// session for another claim, as it can for a session a daemon from before the hold recorded (the
// claim_sessions case reregister names); the daemon's restart drops what the machine held of the
// session, so the registration again asks the store. The negative control is the record moved
// ahead of holdRecorded in holdRegistration, where this test and the one above both record the pod.
func TestUnspentCopyReregistrationRefusedForItsSessionRecordsNothing(t *testing.T) {
	h := newHarness(t)
	recorded := h.withPodRegistrations()
	h.launchInPod("pod-uid-9")
	h.register()
	if len(recorded.calls) != 1 {
		t.Fatalf("the first registration recorded %+v, want one registration of its pod", recorded.calls)
	}
	h.store.setHold(sessionID(session), "legion-legion-legion-999-tester")
	h.restart()
	h.wantState(StateRegistered)
	err := h.handle(RequestRegister{
		Claim:          h.token,
		Generation:     h.generation(),
		Session:        session,
		SessionFile:    sessionFile,
		CapabilityHash: []byte("capability-again"),
		PluginContract: h.deps.PluginContract,
	})
	if refusal := (claim.Refusal{}); err == nil || !errors.As(err, &refusal) {
		t.Fatalf("the registration again of a session another claim holds returned %v, want the hold's refusal", err)
	}
	if len(recorded.calls) != 1 {
		t.Fatalf("a registration again refused for its session recorded %+v, want only the first registration's record of its pod", recorded.calls)
	}
}
