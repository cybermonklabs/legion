package supervise

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A session is held to the first claim that recorded it before anything records, launches or
// enrolls it (holdRecorded, plan E's t-e-54): the broker's enrollment takes over the grants of
// every other enrollment of the pod's account and session, so an enrollment of another claim's
// session is that claim's secrets. A registration the hold refuses is answered with the refusal
// and leads to no Enroll call, and records nothing; one it admits leads to exactly one. A claim a
// daemon before the hold recorded on a session the store now holds for no claim (two claims
// recorded it) is never relaunched under it, and, re-adopted live, enrolls nothing on its hello.
//
// The controls (the pull request records each run): the hold after the enrollment (in register,
// below m.ensureEnrolled) answers the same refusal, but only once the stub has recorded the
// enrollment of the other claim's session; no hold in ensureEnrolled enrolls the re-adopted
// claim; no hold in start resumes the relaunched one.
func TestARefusedSessionIsNeverEnrolled(t *testing.T) {
	t.Run("a registration of another claim's session", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.store.setHold(sessionID(session), "legion-legion-LEGION-209-tester")
		h.launch()
		h.helloWithIdentity()
		err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(session) {
			t.Fatalf("registering the tester's session = %v, want %v", err, claim.SessionHeldRefusal(session))
		}
		if enrolled := broker.enrolled(); len(enrolled) != 0 {
			t.Fatalf("the refused registration enrolled %+v, the session of legion-legion-LEGION-209-tester", enrolled)
		}
		if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.CapabilityHash != nil || c.State != StateShimConnected {
			t.Fatalf("the refused registration left the claim %s recording %q, %q and capability %x", c.State, c.Session, c.SessionFile, c.CapabilityHash)
		}

		h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: "ses_fresh", SessionFile: "/state/sessions/fresh.jsonl",
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		if enrolled := broker.enrolled(); len(enrolled) != 1 || enrolled[0].Session != "ses_fresh" {
			t.Fatalf("the admitted registration enrolled %+v, want one enrollment of ses_fresh", enrolled)
		}
		if holder, _ := h.store.holdOf(sessionID("ses_fresh")); holder != h.token {
			t.Fatalf("ses_fresh is held for %q, want %s", holder, h.token)
		}
	})

	// The hold runs whatever `agent_secrets` says: a daemon with no secrets broker refuses the
	// registration of another claim's session exactly as one with a broker does, and records
	// nothing of it (acceptance of #4220, row 12). The control (the pull request records the run):
	// register's hold run only when a broker is set; the registration is admitted.
	t.Run("a registration of another claim's session with no secrets broker", func(t *testing.T) {
		h := newHarness(t)
		if h.deps.Secrets != nil {
			t.Fatal("the harness has a secrets broker; this row needs none")
		}
		h.store.setHold(sessionID(session), "legion-legion-LEGION-209-tester")
		h.launch()
		h.helloWithIdentity()
		err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(session) {
			t.Fatalf("registering the tester's session with no broker = %v, want %v", err, claim.SessionHeldRefusal(session))
		}
		if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.CapabilityHash != nil {
			t.Fatalf("the refused registration left the claim recording %q, %q and capability %x", c.Session, c.SessionFile, c.CapabilityHash)
		}
	})

	// The recorded session registering again (reregister) is held as a first registration is,
	// before anything of it is written: a claim a daemon before the hold recorded on a session the
	// store holds for another claim is refused its re-registration, and its record and the broker
	// are left as they were.
	t.Run("a re-registration of a session another claim holds", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		h.store.setHold(sessionID(session), "legion-legion-LEGION-209-tester")
		h.restart()
		before := h.claim()
		err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
			SessionFile: "/state/sessions/again.jsonl", CapabilityHash: []byte("again"), PluginContract: h.deps.PluginContract})
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(session) {
			t.Fatalf("re-registering the tester's session = %v, want %v", err, claim.SessionHeldRefusal(session))
		}
		if c := h.claim(); c.State != before.State || c.SessionFile != before.SessionFile || !bytes.Equal(c.CapabilityHash, before.CapabilityHash) {
			t.Fatalf("the refused re-registration left the claim %s recording %q and capability %q; want it %s recording %q and %q, as before",
				c.State, c.SessionFile, c.CapabilityHash, before.State, before.SessionFile, before.CapabilityHash)
		}
		if enrolled := broker.enrolled(); len(enrolled) != 0 {
			t.Fatalf("the refused re-registration reached the broker: %+v", enrolled)
		}
	})

	t.Run("a re-adopted claim's session held for no claim", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		// A claim a daemon before the hold launched and recorded: ready on its session, its shim
		// connected without the pod's identity, so nothing was enrolled yet.
		h.reach(StateReady)
		// Two claims recorded the session before the daemon held sessions: the migration holds it
		// for neither, and a daemon restarted onto it re-adopts the claim recording it.
		h.store.setHold(sessionID(session), "")
		h.restart()
		h.helloWithIdentity()
		if enrolled := broker.enrolled(); len(enrolled) != 0 {
			t.Fatalf("enrollments %+v, want none: the re-adopted claim's session is held for no claim", enrolled)
		}
		if c := h.claim(); c.Enrollment != nil {
			t.Fatalf("the re-adopted claim records enrollment %+v", c.Enrollment)
		}
	})

	t.Run("a relaunch of a claim whose session is held for no claim", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.launch()
		h.helloWithIdentity()
		h.register()
		h.ready()
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.store.setHold(sessionID(session), "")
		h.deps.Limits.LaunchFailures = 1
		h.restart()
		resumes := len(h.calls("Resume"))
		err := h.handle(RequestResume{Claim: h.token})
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(session) {
			t.Fatalf("resuming a claim whose session is held for no claim = %v, want %v", err, claim.SessionHeldRefusal(session))
		}
		if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
			t.Fatalf("the runtime was asked to resume the session held for no claim: %+v", calls)
		}
		if enrolled := broker.enrolled(); len(enrolled) != 1 {
			t.Fatalf("enrollments %+v, want only the first process's", enrolled)
		}
		wantRefusedResume(t, h, err)
	})

	// A claim the boot could not suspend keeps its live process (review rounds 12 and 13 of #4220):
	// one a daemon from before the hold enrolled, ready on a session the store now holds for
	// another claim. That process keeps its enrollment's file and renewer across connections, so
	// the enrollment is revoked at the broker rather than only withheld from the new connection; a
	// task for it is held back unsent and a compaction of its session is not taken, since either
	// would put that process to work under the other claim's session. The refusal asked again at
	// each sweep is written at Warn once.
	//
	// The controls (the pull request records the runs): the handover in ensureEnrolled without its
	// holdRecorded check hands the enrollment over again and revokes nothing; one that only
	// withholds it revokes nothing; sendPending without its check sends the task; compactStart
	// without its check compacts the session; holdRecorded writing every refusal writes the Warn
	// line at each sweep.
	t.Run("a live claim whose session another claim holds is sent nothing", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.launch()
		h.helloWithIdentity()
		h.register()
		h.ready()
		if enrolled := broker.enrolled(); len(enrolled) != 1 {
			t.Fatalf("enrollments %+v, want the first process's one", enrolled)
		}
		enrollment := h.claim().Enrollment
		if enrollment == nil {
			t.Fatal("the first process's claim records no enrollment")
		}
		handed := len(h.conn.Enrollments())
		h.store.setHold(sessionID(session), claim.Token("legion-legion-LEGION-209-tester"))
		h.restart()
		h.helloWithIdentity()
		h.m.Wait()
		if got := h.conn.Enrollments(); len(got) != handed {
			t.Fatalf("the new connection was handed enrollments %v, want none beyond the %d before: the claim does not hold %s", got[handed:], handed, session)
		}
		if revoked := broker.revocations(); !slices.Equal(revoked, []string{enrollment.ID}) {
			t.Fatalf("revoked %v, want the enrollment the process still holds, %s: the claim does not hold %s", revoked, enrollment.ID, session)
		}
		if c := h.claim(); c.Enrollment != nil {
			t.Fatalf("the claim still records enrollment %+v after its revocation", *c.Enrollment)
		}
		base := len(h.prompts())
		h.must(RequestDeliver{Claim: h.token, Task: "the task"})
		h.observe(runtime.Alive)
		h.wantPrompts(base)
		if p := h.pending(); p.Task != "the task" || !p.ConfirmedAt.IsZero() {
			t.Fatalf("pending %+v, want the task held back unsent", p)
		}
		if err := h.m.Compact(h.ctx, testCompactInstructions); !errors.Is(err, ErrCompactNotTaken) {
			t.Fatalf("a compaction of the claim's session answered %v, want ErrCompactNotTaken: the claim does not hold %s", err, session)
		}
		if got := h.conn.Compactions(); len(got) != 0 {
			t.Fatalf("compactions %+v were asked of %s, which the claim does not hold", got, session)
		}
		for range 3 {
			h.observe(runtime.Alive)
		}
		h.m.Wait()
		if enrolled := broker.enrolled(); len(enrolled) != 1 {
			t.Fatalf("enrollments %+v, want no new one", enrolled)
		}
		if lines := h.logs.lines("refused a session another claim recorded first", string(h.token)); len(lines) != 1 {
			t.Fatalf("the refusal was written at Warn %d times over the sweeps, want once:\n%s", len(lines), strings.Join(lines, "\n"))
		}
	})

	// A revocation the broker does not answer stays owed: the enrollment is kept recorded, handed
	// to no connection, and each later observation asks the broker again until it confirms
	// (revokeUnheld, reviews rounds 13 and 14 of #4220), across a restart of the daemon too, since
	// the claim's record carries the enrollment. The process renews its lease while it lives, so
	// forgetting an enrollment the broker never revoked would leave it live with nothing left to
	// revoke it.
	//
	// The control (the pull request records the run): the enrollment cleared before the broker
	// answers, the let-go's revoke left to retry it, as round 14 first did; the claim then records
	// no enrollment after the failed revocation, and nothing revokes it.
	revokeLater := func(t *testing.T, restart bool) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.launch()
		h.helloWithIdentity()
		h.register()
		h.ready()
		enrollment := h.claim().Enrollment
		if enrollment == nil {
			t.Fatal("the first process's claim records no enrollment")
		}
		handed := len(h.conn.Enrollments())
		h.store.setHold(sessionID(session), claim.Token("legion-legion-LEGION-209-tester"))
		broker.mu.Lock()
		broker.revokeFails = revokeAttempts
		broker.mu.Unlock()
		h.restart()
		h.helloWithIdentity()
		for attempt := 1; attempt <= revokeAttempts; attempt++ {
			h.m.Wait()
			if revoked := broker.revocations(); len(revoked) != 0 {
				t.Fatalf("revoked %v while the broker failed every revocation", revoked)
			}
			if c := h.claim(); c.Enrollment == nil || c.Enrollment.ID != enrollment.ID {
				t.Fatalf("after %d failed revocations the claim records enrollment %+v, want %s kept for the next try", attempt, c.Enrollment, enrollment.ID)
			}
			if got := h.conn.Enrollments(); len(got) != handed {
				t.Fatalf("the new connection was handed enrollments %v while the revocation waits", got[handed:])
			}
			if attempt < revokeAttempts {
				h.observe(runtime.Alive)
			}
		}
		if restart {
			h.restart()
			h.helloWithIdentity()
		} else {
			h.observe(runtime.Alive)
		}
		h.m.Wait()
		if revoked := broker.revocations(); !slices.Equal(revoked, []string{enrollment.ID}) {
			t.Fatalf("revoked %v once the broker answered, want %s", revoked, enrollment.ID)
		}
		if c := h.claim(); c.Enrollment != nil {
			t.Fatalf("the claim still records enrollment %+v after its revocation", *c.Enrollment)
		}
	}
	t.Run("a revocation the broker fails three times is tried again", func(t *testing.T) { revokeLater(t, false) })
	t.Run("a revocation the broker fails is tried again after a restart", func(t *testing.T) { revokeLater(t, true) })

	// An operator settles an id held for no claim by recording its rightful holder, and that claim's
	// next registration on the same machine is admitted: the refusal of an id held for no claim is
	// never kept, as a claim's hold is.
	t.Run("an id held for no claim, once an operator records this claim as its holder", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.store.setHold(sessionID(session), "")
		h.launch()
		h.helloWithIdentity()
		registration := RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract}
		var refusal claim.Refusal
		if err := h.handle(registration); !errors.As(err, &refusal) {
			t.Fatalf("registering a session held for no claim = %v, want the refusal", err)
		}
		h.store.setHold(sessionID(session), h.token)
		h.must(registration)
		if enrolled := broker.enrolled(); len(enrolled) != 1 || enrolled[0].Session != session {
			t.Fatalf("the settled registration enrolled %+v, want one enrollment of %s", enrolled, session)
		}
	})
}

// sessionRecorder is a Registrar that records the session of every registration a machine
// starts.
type sessionRecorder struct {
	mu       sync.Mutex
	sessions []string
}

func (r *sessionRecorder) KeepPodSession(_ context.Context, s PodSession) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions = append(r.sessions, s.Session)
	return func() {}, nil
}

func (r *sessionRecorder) registered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sessions)
}

// The pod's session registration is held as an enrollment is (ensureRegistered, plan E's t-e-54):
// the broker answers an exchange naming the pod and a registered session with the role's own
// token, so a registration of another claim's session is that session's trade. A session another
// claim holds is never registered — the id the runtime chose for a launch, or a re-adopted claim's
// recorded session — and an accepted one, the runtime's chosen id or the agent's own fresh
// registration, is registered once and held for the claim.
//
// The control (the pull request records the run): the hold after the first write (in
// ensureRegistered, below KeepPodSession) still answers no registration of the claim's own, but
// only once the stub has recorded the other claim's session.
func TestARefusedSessionIsNeverRegistered(t *testing.T) {
	const pod, other = "pod-uid-54", claim.Token("legion-legion-LEGION-209-tester")
	rig := func(t *testing.T) (*harness, *sessionRecorder, func()) {
		t.Helper()
		h := newHarness(t)
		h.withSecrets()
		recorder := &sessionRecorder{}
		h.deps.Registrar = recorder
		h.start(h.store.load(h.token))
		h.rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: runtime.SandboxIncarnation(pod, 1),
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: 1},
		}})
		hello := func() {
			t.Helper()
			h.conns.Register(h.token, h.conn)
			h.must(StreamHello{Claim: h.token, Generation: h.generation(), AgentSecrets: &AgentSecretsIdentity{Thumbprint: testIdentity.Thumbprint, PodToken: testIdentity.PodToken}})
		}
		return h, recorder, hello
	}

	t.Run("a launch's chosen session another claim holds", func(t *testing.T) {
		h, recorder, hello := rig(t)
		h.rt.ScriptLaunchSession(h.token, "ses_chosen")
		h.store.setHold(sessionID("ses_chosen"), other)
		h.launch()
		hello()
		h.observe(runtime.Alive)
		if got := recorder.registered(); len(got) != 0 {
			t.Fatalf("registered %v for the pod to trade, want nothing: ses_chosen is held for %s", got, other)
		}
		if holder, _ := h.store.holdOf(sessionID("ses_chosen")); holder != other {
			t.Fatalf("ses_chosen is held for %q, want it still %s's", holder, other)
		}
	})

	t.Run("a re-adopted claim's recorded session another claim holds", func(t *testing.T) {
		h, recorder, hello := rig(t)
		h.launch()
		hello()
		h.register()
		h.ready()
		if got := recorder.registered(); !slices.Equal(got, []string{session}) {
			t.Fatalf("the first process registered %v, want %s once", got, session)
		}
		// As a daemon before the hold leaves it, or a release another claim then took the id after:
		// the store holds the recorded session for another claim, and a daemon restarted onto the
		// claim re-adopts it, its cache of held names gone.
		h.store.setHold(sessionID(session), other)
		h.restart()
		hello()
		h.observe(runtime.Alive)
		if got := recorder.registered(); !slices.Equal(got, []string{session}) {
			t.Fatalf("registered %v, want only the first process's %s: after the restart it is held for %s", got, session, other)
		}
	})

	// A contained claim keeps its hello identity, so every observation asks the registration again
	// and is refused again; the refusal is written at Error once for the incarnation and session
	// (Machine.registrationRefused, review round 14 of #4220, note 42). The control (the pull
	// request records the run): every refusal written, one line per observation.
	t.Run("a refused registration asked again at each observation is written once", func(t *testing.T) {
		h, recorder, hello := rig(t)
		h.launch()
		hello()
		h.register()
		h.ready()
		h.store.setHold(sessionID(session), other)
		h.restart()
		hello()
		for range 4 {
			h.observe(runtime.Alive)
		}
		if got := recorder.registered(); !slices.Equal(got, []string{session}) {
			t.Fatalf("registered %v, want only the first process's %s", got, session)
		}
		if lines := h.logs.lines("not registering a session this claim may not take"); len(lines) != 1 {
			t.Fatalf("the refused registration was written at Error %d times over the observations, want once:\n%s", len(lines), strings.Join(lines, "\n"))
		}
	})

	t.Run("a launch's chosen session no claim holds", func(t *testing.T) {
		h, recorder, hello := rig(t)
		h.rt.ScriptLaunchSession(h.token, "ses_chosen")
		h.launch()
		hello()
		h.observe(runtime.Alive)
		if got := recorder.registered(); !slices.Equal(got, []string{"ses_chosen"}) {
			t.Fatalf("registered %v, want ses_chosen once", got)
		}
		if holder, _ := h.store.holdOf(sessionID("ses_chosen")); holder != h.token {
			t.Fatalf("ses_chosen is held for %q, want %s", holder, h.token)
		}
	})

	t.Run("an agent's fresh registration", func(t *testing.T) {
		h, recorder, hello := rig(t)
		h.launch()
		hello()
		if got := recorder.registered(); len(got) != 0 {
			t.Fatalf("registered %v before the agent registered a session", got)
		}
		h.register()
		h.ready()
		h.observe(runtime.Alive)
		if got := recorder.registered(); !slices.Equal(got, []string{session}) {
			t.Fatalf("registered %v, want %s once", got, session)
		}
		if holder, _ := h.store.holdOf(sessionID(session)); holder != h.token {
			t.Fatalf("%s is held for %q, want %s", session, holder, h.token)
		}
	})
}

// A Claude Code launch runs under the session its recorded transcript's name gives: a fresh
// launch's own uuid, which its claude-envoy registers with that session's own transcript, and a
// relaunch's `--resume <id>` read from the recorded transcript (claudelaunch.ResumeSession). So the
// session a transcript names is held to the claim as its session is (holdRecorded,
// plan E's t-e-54): a registration whose transcript names another claim's Claude session is
// refused, recording and enrolling nothing, and a claim recorded before the hold whose transcript
// names one is never relaunched into it.
//
// The controls (the pull request records both runs): register holding the registered session
// alone admits the registration and records the other claim's transcript; start resuming without
// holdRecorded calls the runtime's Resume on that transcript.
func TestAClaudeLaunchIsHeldToItsClaim(t *testing.T) {
	const holder = claim.Token("legion-legion-LEGION-209-tester")
	const stolen = "11111111-2222-4333-8444-555555555555"
	transcript := "/legion/state/implementer/claude/projects/-legion-workspaces-acme-widgets-legion-209/" + stolen + ".jsonl"

	t.Run("a registration whose transcript names another claim's Claude session", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.store.setHold(sessionID(stolen), holder)
		h.launch()
		h.helloWithIdentity()
		err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: "22222222-3333-4444-8555-666666666666",
			SessionFile: transcript, CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(stolen) {
			t.Fatalf("registering a transcript of %s's Claude session = %v, want %v", holder, err, claim.SessionHeldRefusal(stolen))
		}
		if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.CapabilityHash != nil {
			t.Fatalf("the refused registration left the claim recording %q, %q and capability %x", c.Session, c.SessionFile, c.CapabilityHash)
		}
		if enrolled := broker.enrolled(); len(enrolled) != 0 {
			t.Fatalf("the refused registration enrolled %+v", enrolled)
		}
	})

	t.Run("a relaunch resuming another claim's Claude session", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		// A claim a daemon before the hold recorded: its own session, and a transcript naming the
		// tester's Claude session, which a resume would continue.
		stored := h.store.load(h.token)
		stored.SessionFile = transcript
		if err := h.store.PutClaim(h.ctx, stored); err != nil {
			t.Fatal(err)
		}
		h.store.setHold(sessionID(stolen), holder)
		h.deps.Limits.LaunchFailures = 1
		h.restart()
		resumes := len(h.calls("Resume"))

		err := h.handle(RequestResume{Claim: h.token})
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(stolen) {
			t.Fatalf("resuming a transcript of %s's Claude session = %v, want %v", holder, err, claim.SessionHeldRefusal(stolen))
		}
		if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
			t.Fatalf("the runtime was asked to resume %s's Claude session: %+v", holder, calls)
		}
		wantRefusedResume(t, h, err)
	})
}

// Where a claim's recorded session file is missing, the tmux runtime resumes a file of the same
// name, or an Oh My Pi file of any timestamp with the same session id, one folder below a sessions
// directory (runtime/tmux's sessionFileOf and newestSession). So a session file's base name and the
// id its Oh My Pi name gives are held to the claim (holdRecorded, plan E's t-e-54): a
// registration of another claim's file under another folder, or of another timestamp of its id, is
// refused, recording and enrolling nothing; a claim recorded before the hold under such a file is
// never relaunched into it; and a claim's own file, carried to another folder, is still its own.
// Each row is refused by one kind of name alone: the file of another folder bears no `_<id>`, and
// the file of another timestamp has another base name.
//
// The controls (the pull request records each run): RecordedHolds without the file-name kind
// admits the registration under another folder and resumes the relaunch; without the file-id
// kind, it admits the one of another timestamp.
func TestATmuxResumeByNameIsHeldToItsClaim(t *testing.T) {
	const holder = claim.Token("legion-legion-LEGION-209-tester")
	// The holder's two sessions, each named so that one kind of name alone holds what a search
	// finds it by.
	const named = "/legion/state/sessions/--legion-workspaces-legion-209--/holder-session.jsonl"
	const stamped = "/legion/state/sessions/--legion-workspaces-legion-209--/2026-11-16T10-00-00-000Z_ses-stamped.jsonl"
	holdFor := func(h *harness, session, file string) {
		h.t.Helper()
		if holders, err := h.store.HoldSessions(h.ctx, "legion", holder, RecordedHolds(session, file)); err != nil || slices.ContainsFunc(holders, func(c claim.Token) bool { return c != holder }) {
			h.t.Fatalf("hold the tester's session: %v, %v", holders, err)
		}
	}

	for _, tc := range []struct {
		name, holderSession, holderFile, file string
		refused                               Hold
	}{
		{
			name:          "another claim's file under another folder",
			holderSession: "ses_named", holderFile: named,
			file:    "/legion/state/home/.omp/agent/sessions/--pod-workspace--/holder-session.jsonl",
			refused: Hold{Kind: HoldFileName, Name: "holder-session.jsonl"},
		},
		{
			name:          "another timestamp of another claim's session id",
			holderSession: "ses_stamped", holderFile: stamped,
			file:    "/legion/state/sessions/--legion-workspaces-legion-209--/2026-11-16T11-00-00-000Z_ses-stamped.jsonl",
			refused: Hold{Kind: HoldFileID, Name: "ses-stamped"},
		},
	} {
		t.Run("a registration of "+tc.name, func(t *testing.T) {
			h := newHarness(t)
			broker := h.withSecrets()
			holdFor(h, tc.holderSession, tc.holderFile)
			h.launch()
			h.helloWithIdentity()
			err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: "ses_fresh",
				SessionFile: tc.file, CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
			var refusal claim.Refusal
			if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(tc.refused.Name) {
				t.Fatalf("registering %s = %v, want %v", tc.file, err, claim.SessionHeldRefusal(tc.refused.Name))
			}
			if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.CapabilityHash != nil {
				t.Fatalf("the refused registration left the claim recording %q, %q and capability %x", c.Session, c.SessionFile, c.CapabilityHash)
			}
			if enrolled := broker.enrolled(); len(enrolled) != 0 {
				t.Fatalf("the refused registration enrolled %+v", enrolled)
			}
			for _, h2 := range RecordedHolds("ses_fresh", tc.file) {
				if got, held := h.store.holdOf(h2); held && got == h.token {
					t.Fatalf("the refused registration left %s %s held for the claim: a call records all of its names or none", h2.Kind, h2.Name)
				}
			}
		})
	}

	t.Run("a relaunch resuming another claim's file by its name", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		// A claim a daemon before the hold recorded: its own session, and a file of the tester's
		// name under another folder, which a resume finds the tester's session by.
		stored := h.store.load(h.token)
		stored.SessionFile = "/legion/state/home/.omp/agent/sessions/--pod-workspace--/holder-session.jsonl"
		if err := h.store.PutClaim(h.ctx, stored); err != nil {
			t.Fatal(err)
		}
		holdFor(h, "ses_named", named)
		h.deps.Limits.LaunchFailures = 1
		h.restart()
		resumes := len(h.calls("Resume"))

		err := h.handle(RequestResume{Claim: h.token})
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal("holder-session.jsonl") {
			t.Fatalf("resuming a file of the tester's name = %v, want the refusal of its name", err)
		}
		if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
			t.Fatalf("the runtime was asked to resume the tester's session: %+v", calls)
		}
		wantRefusedResume(t, h, err)
	})

	// What the hold leaves as it was (R9): a pod's session carried onto a tmux host sits in another
	// folder under the same name (sessionFileOf), and the claim that recorded it registers it there.
	t.Run("this claim's own file, carried to another folder", func(t *testing.T) {
		h := newHarness(t)
		h.launch()
		h.connect()
		h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: "ses_own",
			SessionFile:    "/pod/home/.omp/agent/sessions/--w--/2026-11-16T10-00-00-000Z_ses_own.jsonl",
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: "ses_own"})
		h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: "ses_own",
			SessionFile:    "/legion/state/sessions/--w--/2026-11-16T10-00-00-000Z_ses_own.jsonl",
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		if c := h.claim(); c.SessionFile != "/legion/state/sessions/--w--/2026-11-16T10-00-00-000Z_ses_own.jsonl" {
			t.Fatalf("the claim records %q, want its carried file", c.SessionFile)
		}
	})
}

// A store that does not answer a resume's hold says nothing about the claim or its session
// (errHoldUnanswered): the launch starts nothing, is charged to no budget, and is tried again at
// the probe interval, as a launch whose write the store did not take is; once the store answers,
// the claim resumes its own session.
//
// The control (the pull request records the run): the hold's error sent down the launch-failure
// path, as before this test: one unanswered hold spends the budget of one, and the claim fails.
func TestAnUnansweredHoldAtAResumeChargesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.deps.Limits.LaunchFailures = 1
	h.restart()
	h.store.fail("HoldSessions", errors.New("connection refused"))
	resumes := len(h.calls("Resume"))

	if err := h.handle(RequestResume{Claim: h.token}); !errors.Is(err, errHoldUnanswered) {
		t.Fatalf("resuming with the store not answering the hold = %v, want errHoldUnanswered", err)
	}
	if c := h.claim(); c.State != StateLaunching || c.Budgets.LaunchFailures != 0 {
		t.Fatalf("the claim is %s with %d launch failures charged, want it launching with none", c.State, c.Budgets.LaunchFailures)
	}
	if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
		t.Fatalf("the runtime was asked to resume before the session was held: %+v", calls)
	}

	h.store.fail("HoldSessions", nil)
	h.advance(testProbe)
	if calls := h.calls("Resume")[resumes:]; len(calls) != 1 || calls[0].Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("the retry at the probe interval resumed %+v, want the claim's own session once", calls)
	}
	if c := h.claim(); c.Budgets.LaunchFailures != 0 {
		t.Fatalf("the claim has %d launch failures charged after the retry, want none", c.Budgets.LaunchFailures)
	}
}

// A claim holds at most MaxHeldNamesPerClaim names of sessions (review N2 of #4220): a hold ends
// only by an operator's release, so without a bound one role could grow the daemon's record
// without end. A registration whose new names would take the claim past it is refused 409 with
// claim.HeldNamesCapRefusal, recording, enrolling and holding nothing, and its Warn line names the
// release command; one that fills the cap exactly registers; and a claim at the cap still resumes
// the session it recorded, whose names it holds already, and registers it again.
//
// The control (the pull request records the run): holdRecorded reading the store's cap error as a
// store that did not answer, as before the cap: the registration past the cap answers
// errHoldUnanswered, not the refusal.
func TestAClaimAtTheCapOfHeldNamesIsRefusedANewSession(t *testing.T) {
	register := func(h *harness) error {
		return h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
	}
	names := len(RecordedHolds(session, sessionFile))

	t.Run("a registration one name past the cap", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.store.fillHolds(h.token, MaxHeldNamesPerClaim-names+1)
		h.launch()
		h.helloWithIdentity()
		err := register(h)
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.HeldNamesCapRefusal(MaxHeldNamesPerClaim) || refusal.Status != http.StatusConflict {
			t.Fatalf("registering a session past the cap = %v, want %v", err, claim.HeldNamesCapRefusal(MaxHeldNamesPerClaim))
		}
		if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.CapabilityHash != nil {
			t.Fatalf("the refused registration left the claim recording %q, %q and capability %x", c.Session, c.SessionFile, c.CapabilityHash)
		}
		if enrolled := broker.enrolled(); len(enrolled) != 0 {
			t.Fatalf("the refused registration enrolled %+v", enrolled)
		}
		if got, want := h.store.heldBy(h.token), MaxHeldNamesPerClaim-names+1; got != want {
			t.Fatalf("the claim holds %d names after the refusal, want the %d it held before", got, want)
		}
		// The operator's way out is on the refusal's line: the command that frees the names of its
		// other sessions once the broker carries nothing of them (ReleaseNames), spelled here rather
		// than read from ReleaseNamesCommand, so a wrong command fails the row (side review of
		// #4355).
		lines := h.records.of(heldNamesCapLine)
		if len(lines) != 1 {
			t.Fatalf("the refusal was logged %d times, want once", len(lines))
		}
		want := "legion claims release-names --claim " + string(h.token) + " --operator-token-file <the daemon's operator_token_file>"
		if release, ok := lines[0].attr("release"); !ok || release.String() != want {
			t.Fatalf("the refusal's line gives the release %v, want %q", release, want)
		}
	})

	t.Run("a registration that fills the cap exactly", func(t *testing.T) {
		h := newHarness(t)
		h.store.fillHolds(h.token, MaxHeldNamesPerClaim-names)
		h.launch()
		h.connect()
		if err := register(h); err != nil {
			t.Fatalf("registering a session that fills the cap = %v, want it admitted", err)
		}
		if got := h.store.heldBy(h.token); got != MaxHeldNamesPerClaim {
			t.Fatalf("the claim holds %d names, want %d", got, MaxHeldNamesPerClaim)
		}
	})

	t.Run("a claim at the cap resumes its own session", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.store.fillHolds(h.token, MaxHeldNamesPerClaim-h.store.heldBy(h.token))
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.restart()
		resumes := len(h.calls("Resume"))
		h.must(RequestResume{Claim: h.token})
		if calls := h.calls("Resume")[resumes:]; len(calls) != 1 || calls[0].Spec.ResumeSessionFile != sessionFile {
			t.Fatalf("the claim at the cap resumed %+v, want its own session once", calls)
		}
		h.connect()
		if err := register(h); err != nil {
			t.Fatalf("the claim at the cap registering its own session again = %v, want it admitted", err)
		}
		if got := h.store.heldBy(h.token); got != MaxHeldNamesPerClaim {
			t.Fatalf("the claim holds %d names, want %d: resuming its own session adds none", got, MaxHeldNamesPerClaim)
		}
	})
}

// A machine keeps the names of the session its claim records that the store answered as the
// claim's, and asks the store nothing more of them (holdRecorded). Once the claim forgets that
// session, an operator's release may free those names and another claim take them
// (ReleaseNames), so forgetSession empties the cache (review F2 of #4220's round 4): a later
// registration of the forgotten session on the same machine asks the store again and is refused,
// recording and enrolling nothing.
//
// The control (the pull request records the run): forgetSession keeping the cache; the
// registration is admitted under the other claim's session.
func TestAForgottenSessionIsAskedOfTheStoreAgain(t *testing.T) {
	const taker = claim.Token("legion-legion-LEGION-209-tester")
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.must(RequestForgetSession{Claim: h.token, Session: session})
	// The operator's release freed the forgotten session's names, and another claim registered it.
	for _, name := range RecordedHolds(session, sessionFile) {
		h.store.setHold(name, taker)
	}
	enrolled := len(broker.enrolled())
	h.must(RequestResume{Claim: h.token})
	h.helloWithIdentity()

	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict {
		t.Fatalf("registering the forgotten session, which %s now holds, = %v, want a 409 refusal", taker, err)
	}
	if c := h.claim(); c.Session != "" || c.SessionFile != "" {
		t.Fatalf("the refused registration left the claim recording %q and %q", c.Session, c.SessionFile)
	}
	if got := broker.enrolled(); len(got) != enrolled {
		t.Fatalf("the refused registration enrolled %+v", got[enrolled:])
	}
}

// A session id, and a session file's base name, are at most claim.MaxSessionNameBytes: past it a
// name could not be held, since a btree row of claim_sessions' primary key holds at most 2,704
// bytes and one such name would stop the boot that holds it (review N3 of #4220). And a session
// file is a clean path: one ending in `/.` or `/..` has a base name, `.` or `..`, that is not its
// file's, so the file-name hold would hold the wrong name (review round 11). A registration of
// either is refused 400, recording and enrolling nothing; one at the bound registers.
//
// The controls (the pull request records each run): no length check at registration admits the
// first two rows, since the hold skips a name past the bound rather than refusing it; no clean-path
// check admits the last two.
func TestASessionNamePastTheBoundIsRefused(t *testing.T) {
	long := strings.Repeat("s", claim.MaxSessionNameBytes+1)
	const dotdot, dot = "/state/sessions/2026-11-16T00-00-00-000Z_ses_mine.jsonl/..", "/state/sessions/2026-11-16T00-00-00-000Z_ses_mine.jsonl/."
	for _, tc := range []struct {
		name, session, file string
		want                claim.Refusal
	}{
		{"a session id past the bound", long, sessionFile, claim.SessionNameTooLong("session id", len(long))},
		{"a session file's name past the bound", session, "/state/sessions/" + long, claim.SessionNameTooLong("session file's name", len(long))},
		{"a session file ending in /..", session, dotdot, claim.SessionFileNotClean(dotdot)},
		{"a session file ending in /.", session, dot, claim.SessionFileNotClean(dot)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			broker := h.withSecrets()
			h.launch()
			h.helloWithIdentity()
			err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: tc.session, SessionFile: tc.file,
				CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
			var refusal claim.Refusal
			if !errors.As(err, &refusal) || refusal != tc.want || refusal.Status != http.StatusBadRequest {
				t.Fatalf("registering %s = %v, want %v", tc.name, err, tc.want)
			}
			if c := h.claim(); c.Session != "" || c.SessionFile != "" {
				t.Fatalf("the refused registration left the claim recording %q and %q", c.Session, c.SessionFile)
			}
			if enrolled := broker.enrolled(); len(enrolled) != 0 {
				t.Fatalf("the refused registration enrolled %+v", enrolled)
			}
		})
	}

	t.Run("a session id and a file name at the bound", func(t *testing.T) {
		h := newHarness(t)
		h.launch()
		h.connect()
		atBound := strings.Repeat("s", claim.MaxSessionNameBytes)
		h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: atBound,
			SessionFile: "/state/sessions/" + atBound, CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		if c := h.claim(); c.Session != atBound {
			t.Fatalf("the claim records %q, want the session at the bound", c.Session)
		}
	})
}

// A claim a daemon from before the hold recorded under a name no row can hold — a session id past
// claim.MaxSessionNameBytes, or a session file that is not a clean path — takes nothing under it
// (UnholdableNames): its re-adopted process is not enrolled, and its relaunch is refused before
// anything starts, with claim.SessionNameUnheldRefusal, and fails at its launch budget. Skipped
// rather than refused, as before review round 11 of #4220, two such claims recording one name
// were both enrolled and resumed with no hold at all.
//
// The control (the pull request records the run): holdRecorded without its UnholdableNames check
// enrolls the re-adopted process and resumes both relaunches.
func TestARecordedNameNoHoldCoversIsTakenByNoClaim(t *testing.T) {
	long := strings.Repeat("s", claim.MaxSessionNameBytes+45)
	const unclean = "/state/sessions/2026-11-16T00-00-00-000Z_ses_mine.jsonl/.."
	// record writes the claim's row as an earlier daemon left it, under session and file.
	record := func(h *harness, session, file string) {
		h.t.Helper()
		stored := h.store.load(h.token)
		stored.Session, stored.SessionFile = session, file
		if err := h.store.PutClaim(h.ctx, stored); err != nil {
			h.t.Fatal(err)
		}
	}

	t.Run("a re-adopted claim recording a session id past the bound", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		record(h, long, sessionFile)
		h.restart()
		h.helloWithIdentity()
		if enrolled := broker.enrolled(); len(enrolled) != 0 {
			t.Fatalf("enrollments %+v, want none: the re-adopted claim records a session id no row can hold", enrolled)
		}
	})

	for _, tc := range []struct {
		name, session, file, what string
	}{
		{"a relaunch of a claim recording a session id past the bound", long, sessionFile, "session id"},
		{"a relaunch of a claim recording a session file that is not a clean path", session, unclean, "session file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
			record(h, tc.session, tc.file)
			h.deps.Limits.LaunchFailures = 1
			h.restart()
			resumes := len(h.calls("Resume"))
			err := h.handle(RequestResume{Claim: h.token})
			var refusal claim.Refusal
			if want := claim.SessionNameUnheldRefusal(tc.what); !errors.As(err, &refusal) || refusal != want {
				t.Fatalf("resuming the claim = %v, want %v", err, want)
			}
			if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
				t.Fatalf("the runtime was asked to resume a session no row holds: %+v", calls)
			}
			wantRefusedResume(t, h, err)
		})
	}
}

// A compaction whose session hold the store does not answer asks nothing, and its answer says the
// store did not answer rather than that the claim does not hold its session: the claim may well
// hold it, and the operator reading `legion claims compact`'s 409 would otherwise look for another
// claim holding a session nobody else holds (compactStart, plan E's t-e-54).
//
// The control (the pull request records the run): compactStart with one sentence for both
// answers, as before, answers "the claim's session is not held to it" for the store's outage.
func TestACompactionWhoseHoldTheStoreDoesNotAnswerSaysSo(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.store.fail("HoldSessions", errors.New("the store is down"))
	h.restart()
	h.helloWithIdentity()
	h.m.Wait()

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if !errors.Is(err, ErrCompactNotTaken) || !errors.Is(err, errHoldUnanswered) {
		t.Fatalf("a compaction whose hold the store did not answer answered %v, want ErrCompactNotTaken carrying errHoldUnanswered", err)
	}
	if want := "the store did not answer whether the claim holds its session"; !strings.Contains(err.Error(), want) {
		t.Fatalf("the answer %q does not say %q", err, want)
	}
	if strings.Contains(err.Error(), "is not held to it") {
		t.Fatalf("the answer %q says the claim does not hold its session, which the store did not say", err)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none", got)
	}
}

// A refused hold's Warn line names the claim once: the machine's logger carries it on every line
// (newMachine's log.With), so a line that passes it again writes the key twice, which a reader of
// the JSON log gets as two `claim` members (acceptance of #4220).
//
// The control (the pull request records the run): the refusal's line passing "claim" as it did:
// "the line … names the claim 2 times, want once".
func TestAHoldsLinesNameTheClaimOnce(t *testing.T) {
	h := newHarness(t)
	h.store.setHold(sessionID(session), "legion-legion-LEGION-209-tester")
	h.launch()
	h.helloWithIdentity()
	if err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract}); err == nil {
		t.Fatal("the registration of another claim's session was admitted")
	}
	lines := h.logs.lines("refused a session another claim recorded first")
	if len(lines) != 1 {
		t.Fatalf("the refusal wrote %d lines, want one: %v", len(lines), lines)
	}
	if n := strings.Count(lines[0], " claim="); n != 1 {
		t.Fatalf("the line %q names the claim %d times, want once", lines[0], n)
	}
}

// An operator's resume of a claim recording a session it does not hold is refused before
// anything of the claim changes (resumeRefusal): a RefusedError, which the operator's route answers
// 409 with its sentence, naming the forget-session that gives the claim a new session, and the
// claim stays suspended with nothing launched and nothing charged. Relaunched, its every launch
// would be refused by the hold at start and the claim would fail within its launch budget
// (acceptance of #4220). A rule's resume leaves it suspended the same way. A claim holding its
// session resumes as before (R9).
//
// The control (the pull request records the run): resume with no resumeRefusal: the operator's
// resume answers no refusal and the claim is launched into the hold's refusal.
func TestAResumeOfASessionTheClaimDoesNotHoldIsRefused(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	h := newHarness(t)
	h.reach(StateReady)
	h.store.setHold(sessionID(session), other)
	h.restart()
	h.must(RequestSuspend{Claim: h.token, Reason: "the boot contains it", By: SuspendedByOperator})
	h.wantState(StateSuspended)
	resumes := len(h.calls("Resume"))

	err := h.handle(RequestResume{Claim: h.token})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("the operator's resume of a claim recording %s, which %s holds = %v, want a refusal", session, other, err)
	}
	if want := "legion claims forget-session --claim " + string(h.token); !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal %q does not name %q", err, want)
	}
	h.must(RequestRuleResume{Claim: h.token, Reason: "the outbox starts the claim's phase"})
	h.wantState(StateSuspended)
	if c := h.claim(); c.Budgets.LaunchFailures != 0 || c.Session != session {
		t.Fatalf("the refused resumes left the claim with %d launch failures recording %q, want none and %s", c.Budgets.LaunchFailures, c.Session, session)
	}
	if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
		t.Fatalf("the runtime was asked to resume %d times, want none", len(calls))
	}

	// Settled for this claim, the resume relaunches its session as before.
	h.store.setHold(sessionID(session), h.token)
	h.must(RequestResume{Claim: h.token})
	if calls := h.calls("Resume")[resumes:]; len(calls) != 1 {
		t.Fatalf("the resume of a claim holding its session asked the runtime %d times, want once", len(calls))
	}
}

// A release reports, and marks released, only an abandoned session whose names it could free: an
// abandoned record of the session the claim records again is the current session, none of whose
// names the release frees, so the release names no abandoned session and leaves the record as it
// was (acceptance of #4220).
//
// The control (the pull request records the run): ReleaseNames marking any unreleased abandoned
// session: "the release = {… Abandoned:ses_implementer}, want no abandoned session".
func TestAReleaseReportsNoAbandonedSessionTheClaimRecordsAgain(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.must(RequestForgetSession{Claim: h.token, Session: session})
	h.must(RequestResume{Claim: h.token})
	h.connect()
	h.register()
	if c := h.claim(); c.Session != session || c.Abandoned.Session != session {
		t.Fatalf("the claim records %q and abandoned %q, want %s both", c.Session, c.Abandoned.Session, session)
	}
	released, err := h.m.ReleaseNames(h.ctx)
	if err != nil || released.Abandoned != "" {
		t.Fatalf("the release = %+v, %v; want no abandoned session", released, err)
	}
	if a := h.m.View().Claim.Abandoned; !a.Released.IsZero() {
		t.Fatalf("the claim marks its abandoned session released, %+v, though the release freed none of its names", a)
	}
}

// wantRefusedResume is the outcome of an operator's resume of a claim recording a session it does
// not hold (resumeRefusal): a RefusedError, which the operator's route answers 409, and the claim
// still suspended with no launch charged, so no process ran under the session and the claim keeps
// its whole launch budget for the resume after its forget-session. Before the resume asked the
// hold itself, the relaunch was refused at start and charged to the budget until the claim failed.
func wantRefusedResume(t *testing.T, h *harness, err error) {
	t.Helper()
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("the resume answered %v, want a refusal the operator's route answers 409", err)
	}
	if c := h.claim(); c.State != StateSuspended || c.Budgets.LaunchFailures != 0 {
		t.Fatalf("the claim is %s with %d launch failures charged, want it suspended with none rather than relaunched under a session it does not hold", c.State, c.Budgets.LaunchFailures)
	}
}

// An operator's retry of a stopped claim, and an operator's delivery to a suspended one, relaunch
// the session the claim records, so each is refused one the claim does not hold the way a resume is
// (resumeRefusal): a RefusedError naming the forget-session to run, nothing launched, nothing
// queued, and the claim left in the state it was in (the side review of 47f29c476c).
//
// The control (the pull request records the run): retry and deliverResuming without resumeRefusal:
// each answers no refusal and relaunches the claim into the hold's refusal.
func TestARetryOrDeliveryOfASessionTheClaimDoesNotHoldIsRefused(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	for _, tc := range []struct {
		name  string
		park  func(h *harness)
		state ClaimState
		ask   func(h *harness) Event
	}{
		{"an operator's retry of a stopped claim", func(h *harness) { h.must(RequestStop{Claim: h.token}) }, StateRetired,
			func(h *harness) Event { return RequestOperatorRetry{Claim: h.token} }},
		{"an operator's delivery to a suspended claim", func(h *harness) { h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator}) }, StateSuspended,
			func(h *harness) Event { return RequestDeliver{Claim: h.token, Task: "the next task"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			tc.park(h)
			h.wantState(tc.state)
			h.store.setHold(sessionID(session), other)
			h.restart()
			resumes := len(h.calls("Resume"))

			err := h.handle(tc.ask(h))
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("%s recording %s, which %s holds = %v, want a refusal", tc.name, session, other, err)
			}
			if want := "legion claims forget-session --claim " + string(h.token); !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal %q does not name %q", err, want)
			}
			if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
				t.Fatalf("the runtime was asked to resume %d times, want none", len(calls))
			}
			if c := h.claim(); c.State != tc.state || c.Budgets.LaunchFailures != 0 {
				t.Fatalf("the claim is %s with %d launch failures, want %s with none", c.State, c.Budgets.LaunchFailures, tc.state)
			}
			if _, ok := h.store.delivery(h.token); ok {
				t.Fatal("the refused request left a task queued on the claim")
			}
		})
	}
}

// A death's relaunch of a claim recording a session another claim holds is refused by start's own
// hold (Machine.start) and charged to the launch budget like any refused launch (launchNow), so at
// the budget the claim fails rather than relaunching at the probe interval for ever. No resume
// reaches the runtime. The resume, retry and delivery routes refuse such a claim before start
// (resumeRefusal); a death has no route to refuse, so this row is what reads start's charge.
//
// The control (the pull request records the run): launchNow sending start's refusal down the
// retry that charges nothing, as it sends errHoldUnanswered: the claim is left launching.
func TestADeathsRelaunchIntoAHeldSessionIsChargedToTheLaunchBudget(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	h := newHarness(t)
	h.reach(StateReady)
	h.store.setHold(sessionID(session), other)
	// The death itself is charged one launch failure (relaunchAfterFailure), and the relaunch start
	// refuses is charged the second, which spends the budget (launchNow).
	h.deps.Limits.LaunchFailures = 2
	h.restart()
	resumes := len(h.calls("Resume"))

	// At the budget the refused relaunch fails the claim, and the death's handling answers nil or
	// the refusal: the failure is the claim's, recorded below.
	if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}); err != nil {
		var refusal claim.Refusal
		if !errors.As(err, &refusal) || refusal != claim.SessionHeldRefusal(session) {
			t.Fatalf("the death's relaunch = %v, want nil or %v", err, claim.SessionHeldRefusal(session))
		}
	}
	if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
		t.Fatalf("the runtime was asked to resume %s's session: %+v", other, calls)
	}
	if c := h.claim(); c.State != StateFailed || !strings.Contains(c.Failure, "recorded for another claim") {
		t.Fatalf("the claim is %s (%q), want it failed at its launch budget by the hold's refusal rather than relaunching under %s", c.State, c.Failure, session)
	}
}

// The daemon's own resume of a claim it suspended, recording a session another claim holds, leaves
// the claim suspended (resumeByRule, resumeRefusal): nothing resumed, nothing charged, and the line
// says why. A claim an operator suspended never reaches this check (RuleMayResume).
//
// The control (the pull request records the run): resumeByRule without its resumeRefusal: the rule's
// resume answers the hold's refusal from start.
func TestARuleResumeOfASessionTheClaimDoesNotHoldLeavesItSuspended(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestSuspend{Claim: h.token, Reason: "the boot contains it", By: SuspendedByDaemon})
	h.wantState(StateSuspended)
	h.store.setHold(sessionID(session), other)
	h.restart()
	resumes := len(h.calls("Resume"))

	h.must(RequestRuleResume{Claim: h.token, Reason: "the outbox starts the claim's phase"})
	if c := h.claim(); c.State != StateSuspended || c.Budgets.LaunchFailures != 0 {
		t.Fatalf("the claim is %s with %d launch failures, want it suspended with none", c.State, c.Budgets.LaunchFailures)
	}
	if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
		t.Fatalf("the runtime was asked to resume %d times, want none", len(calls))
	}
	if lines := h.logs.lines("not resumed by rule: the claim records a session it does not hold"); len(lines) != 1 {
		t.Fatalf("the rule's refusal wrote %d lines, want one: %v", len(lines), lines)
	}
}
