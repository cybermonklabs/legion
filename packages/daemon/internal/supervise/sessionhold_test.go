package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
		if c := h.claim(); c.State != StateFailed {
			t.Fatalf("the claim is %s, want it failed at its launch budget rather than running under %s", c.State, session)
		}
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
		if c := h.claim(); c.State != StateFailed {
			t.Fatalf("the claim is %s, want it failed at its launch budget rather than running under %s", c.State, stolen)
		}
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
		if c := h.claim(); c.State != StateFailed {
			t.Fatalf("the claim is %s, want it failed at its launch budget rather than running under the tester's session", c.State)
		}
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
	fill := func(h *harness, n int) {
		h.t.Helper()
		for i := range n {
			h.store.setHold(sessionID(fmt.Sprintf("ses_earlier_%d", i)), h.token)
		}
	}
	register := func(h *harness) error {
		return h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
			CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
	}
	names := len(RecordedHolds(session, sessionFile))

	t.Run("a registration one name past the cap", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		fill(h, MaxHeldNamesPerClaim-names+1)
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
		// other sessions once the broker carries nothing of them (ReleaseNames).
		lines := h.records.of(heldNamesCapLine)
		if len(lines) != 1 {
			t.Fatalf("the refusal was logged %d times, want once", len(lines))
		}
		if release, ok := lines[0].attr("release"); !ok || release.String() != ReleaseNamesCommand(h.token) {
			t.Fatalf("the refusal's line gives the release %v, want %q", release, ReleaseNamesCommand(h.token))
		}
	})

	t.Run("a registration that fills the cap exactly", func(t *testing.T) {
		h := newHarness(t)
		fill(h, MaxHeldNamesPerClaim-names)
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
		fill(h, MaxHeldNamesPerClaim-h.store.heldBy(h.token))
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

// An operator's release (ReleaseNames, `legion claims release-names`) frees every name the claim
// holds but those of the session it records, and only once the secrets broker answers that no
// enrollment of this daemon's login carries anything for them (Enroller.Carried): the broker's own
// rows stand in for the time window the release once estimated. A claim at the cap that needs a
// new session — usually one an operator just gave a new one with forget-session — is refused the
// release while the broker still carries its old session, frees nothing then, and once the carry
// ends the release frees the old session's names and every earlier one, marks the abandoned
// session released, and the claim's next registration of a new session is admitted. The
// recovery needs no second switch and no wait the operator works out.
//
// The controls (the pull request records each run): ReleaseNames asking the broker nothing frees
// the old session while the broker carries it; releasable keeping no current name frees the
// claim's own current session, which it still resumes.
func TestAReleaseFreesAClaimsOtherNamesOnlyOnceTheBrokerCarriesNothingOfThem(t *testing.T) {
	fill := func(h *harness, n int) {
		h.t.Helper()
		for i := range n {
			h.store.setHold(sessionID(fmt.Sprintf("ses_earlier_%d", i)), h.token)
		}
	}
	ends := epoch.Add(12 * time.Hour)

	t.Run("the recovery of a claim at the cap", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.must(RequestForgetSession{Claim: h.token, Session: session})
		const fresh, freshFile = "ses_after_forget", "/state/sessions/after-forget.jsonl"
		fill(h, MaxHeldNamesPerClaim-h.store.heldBy(h.token)-len(RecordedHolds(fresh, freshFile))+1)
		h.must(RequestResume{Claim: h.token})
		h.connect()
		registerFresh := func() error {
			return h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: fresh, SessionFile: freshFile,
				CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
		}
		var refusal claim.Refusal
		if err := registerFresh(); !errors.As(err, &refusal) || refusal != claim.HeldNamesCapRefusal(MaxHeldNamesPerClaim) {
			t.Fatalf("registering a new session at the cap = %v, want %v", err, claim.HeldNamesCapRefusal(MaxHeldNamesPerClaim))
		}

		broker.carry(CarriedEnrollment{SessionID: session, EnrollmentID: "enr-1", State: CarriedDormant, EndsAt: ends})
		held := h.store.heldBy(h.token)
		_, err := h.m.ReleaseNames(h.ctx)
		var carried *NamesCarriedError
		if !errors.As(err, &carried) || len(carried.Carried) != 1 || carried.Carried[0].EnrollmentID != "enr-1" {
			t.Fatalf("the release while the broker carries %s = %v, want a refusal naming enrollment enr-1", session, err)
		}
		if !strings.Contains(err.Error(), session) || !strings.Contains(err.Error(), ends.Format(time.RFC3339)) {
			t.Fatalf("the refusal %q names neither %s nor when its carry ends, %s", err, session, ends.Format(time.RFC3339))
		}
		if got := h.store.heldBy(h.token); got != held {
			t.Fatalf("the refused release left the claim holding %d names, want the %d it held", got, held)
		}
		if asked := broker.askedCarried(); len(asked) != 1 || !slices.Contains(asked[0], session) {
			t.Fatalf("the broker was asked %v, want one question naming %s", asked, session)
		}

		broker.endCarry(session)
		released, err := h.m.ReleaseNames(h.ctx)
		if err != nil {
			t.Fatalf("the release once the broker carries nothing = %v", err)
		}
		if released.Checked != CheckedByBroker || released.Abandoned != session || len(released.Freed) != held {
			t.Fatalf("the release answered %d freed, checked %q, abandoned %q; want all %d, %q, %s",
				len(released.Freed), released.Checked, released.Abandoned, held, CheckedByBroker, session)
		}
		if got := h.store.heldBy(h.token); got != 0 {
			t.Fatalf("the claim holds %d names after the release, want none: it records no session", got)
		}
		if a := h.store.load(h.token).Abandoned; a.Session != session || a.Released.IsZero() {
			t.Fatalf("the stored claim's abandoned session is %+v, want %s marked released", a, session)
		}
		if err := registerFresh(); err != nil {
			t.Fatalf("registering the new session after the release = %v, want it admitted", err)
		}
	})

	t.Run("the current session's names are kept", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		fill(h, 3)
		released, err := h.m.ReleaseNames(h.ctx)
		if err != nil || len(released.Freed) != 3 || released.Abandoned != "" {
			t.Fatalf("the release = %+v, %v; want the 3 earlier names freed and no abandoned session", released, err)
		}
		for _, name := range RecordedHolds(session, sessionFile) {
			if holder, _ := h.store.holdOf(name); holder != h.token {
				t.Fatalf("%s %s, of the session the claim records, is held for %q after the release, want %s", name.Kind, name.Name, holder, h.token)
			}
		}
		for _, ids := range broker.askedCarried() {
			if slices.Contains(ids, session) {
				t.Fatalf("the broker was asked about %v, which names the claim's current session", ids)
			}
		}
	})

	t.Run("a broker that does not answer frees nothing", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		fill(h, 3)
		broker.carriedErr = errors.New("broker unreachable")
		held := h.store.heldBy(h.token)
		if _, err := h.m.ReleaseNames(h.ctx); !errors.Is(err, ErrCarryUnanswered) {
			t.Fatalf("the release with no broker answer = %v, want ErrCarryUnanswered", err)
		}
		if got := h.store.heldBy(h.token); got != held {
			t.Fatalf("the unanswered release left the claim holding %d names, want the %d it held", got, held)
		}
	})

	t.Run("a daemon with no broker frees with no check and says so", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		fill(h, 3)
		released, err := h.m.ReleaseNames(h.ctx)
		if err != nil || len(released.Freed) != 3 || released.Checked != CheckedNoBroker {
			t.Fatalf("the release with no broker = %+v, %v; want the 3 earlier names freed, checked %q", released, err, CheckedNoBroker)
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
			if c := h.claim(); c.State != StateFailed {
				t.Fatalf("the claim is %s, want it failed at its launch budget rather than running under a name no row holds", c.State)
			}
		})
	}
}
