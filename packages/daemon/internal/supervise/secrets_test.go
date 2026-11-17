package supervise

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

func TestAFreshLaunchEnrollsAtRegistrationWithTheSession(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.wantState(StateShimConnected)
	if n := len(broker.enrolled()); n != 0 {
		t.Fatalf("%d enrollments before the agent registered; the session id is the wake address", n)
	}
	h.register()
	enrollments := broker.enrolled()
	if len(enrollments) != 1 {
		t.Fatalf("enrollments %+v, want one at registration", enrollments)
	}
	loc := h.locator()
	want := PodEnrollment{PodUID: loc.Incarnation, Thumbprint: testIdentity.Thumbprint, PodToken: testIdentity.PodToken, Session: session}
	if enrollments[0] != want {
		t.Fatalf("enrolled %+v, want %+v", enrollments[0], want)
	}
	stored := h.store.load(testToken)
	if stored.Enrollment == nil || stored.Enrollment.ID != "enr-1" || stored.Enrollment.Incarnation != loc.Incarnation {
		t.Fatalf("stored enrollment %+v", stored.Enrollment)
	}
	if got := h.conn.Enrollments(); len(got) != 1 || got[0] != "enr-1" {
		t.Fatalf("the shim was told %v, want [enr-1]", got)
	}
}

// In an issue pod six role processes share one pod uid, so the broker tells their enrollments
// apart by slot: the pod's real uid stays the identity the broker verifies against the projected
// token, and the slot is the daemon's own `<role>-g<generation>`, never the composed incarnation.
func TestAnIssuePodProcessEnrollsWithItsPodUIDAndRoleSlot(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: testToken, Incarnation: runtime.SandboxIncarnation("pod-uid-9", 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: "pod-uid-9", Container: "roles", Generation: 1},
	}})
	h.launch()
	h.helloWithIdentity()
	h.register()
	enrollments := broker.enrolled()
	want := PodEnrollment{PodUID: "pod-uid-9", Slot: "implementer-g1", Thumbprint: testIdentity.Thumbprint, PodToken: testIdentity.PodToken, Session: session}
	if len(enrollments) != 1 || enrollments[0] != want {
		t.Fatalf("enrolled %+v, want %+v", enrollments, want)
	}
	if stored := h.store.load(testToken); stored.Enrollment == nil || stored.Enrollment.Incarnation != "pod-uid-9/1" {
		t.Fatalf("stored enrollment %+v, want it bound to the process incarnation", stored.Enrollment)
	}
}

func TestAResumedClaimEnrollsOnItsHello(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	first := h.claim().Enrollment.ID
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.wantState(StateSuspended)
	if got := broker.revocations(); len(got) != 1 || got[0] != first {
		t.Fatalf("the suspension revoked %v, want [%s]", got, first)
	}
	h.must(RequestResume{Claim: h.token})
	h.wantState(StateLaunching)
	h.helloWithIdentity() // the resumed pod's hello: the claim already records its session
	enrollments := broker.enrolled()
	if len(enrollments) != 2 || enrollments[1].Session != session || enrollments[1].PodUID != h.locator().Incarnation ||
		enrollments[1].PodUID == enrollments[0].PodUID {
		t.Fatalf("enrollments %+v, want the resumed pod enrolled on its hello with the recorded session and its own uid", enrollments)
	}
	if c := h.claim(); c.Enrollment == nil || c.Enrollment.ID == first || c.Enrollment.Incarnation != h.locator().Incarnation {
		t.Fatalf("the resumed claim holds %+v", c.Enrollment)
	}
	if got := broker.revocations(); len(got) != 1 {
		t.Fatalf("the resume revoked %v", got[1:])
	}
}

func TestEveryEndOfAProcessRevokesItsEnrollment(t *testing.T) {
	for name, end := range map[string]func(h *harness){
		"found gone":           func(h *harness) { h.observe(runtime.Gone) },
		"not the recorded pod": func(h *harness) { h.observe(runtime.NotRecordedProcess) },
		"suspended":            func(h *harness) { h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator}) },
		"stopped":              func(h *harness) { h.must(RequestStop{Claim: h.token}) },
		"the agent exited":     func(h *harness) { h.exit() },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			broker := h.withSecrets()
			h.launch()
			h.helloWithIdentity()
			h.register()
			h.ready()
			id := h.claim().Enrollment.ID
			end(h)
			if got := broker.revocations(); len(got) != 1 || got[0] != id {
				t.Fatalf("revocations %v, want [%s]", got, id)
			}
			if c := h.claim(); c.Enrollment != nil {
				t.Fatalf("the claim still records enrollment %+v", c.Enrollment)
			}
		})
	}
}

func TestTheRegistrationDeadlineRevokesAnUnregisteredPodsEnrollment(t *testing.T) {
	// A pod whose agent never registers is enrolled only if its claim already had a session (a
	// resume); a fresh launch holds the identity and enrolls nothing, so the deadline revokes
	// nothing and discards the identity with the process.
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.advance(deadline)
	if got := broker.revocations(); len(got) != 0 {
		t.Fatalf("revocations %v for a pod that was never enrolled", got)
	}
	if n := len(broker.enrolled()); n != 0 {
		t.Fatalf("%d enrollments", n)
	}
	// The relaunched pod's hello carries its own identity; registration enrolls it, not the old one.
	h.helloWithIdentity()
	h.register()
	if e := broker.enrolled(); len(e) != 1 || e[0].PodUID != h.locator().Incarnation {
		t.Fatalf("enrolled %+v, want the relaunched pod", e)
	}
}

// A session is held to the first claim that recorded it before anything records, launches or
// enrolls it (holdRecorded, dispatch://INFRA-840): the broker's enrollment takes over the grants of
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

// A Claude Code launch runs under the session its recorded transcript's name gives: a fresh
// launch's own uuid, which its claude-envoy registers with that session's own transcript, and a
// relaunch's `--resume <id>` read from the recorded transcript (claudelaunch.ResumeSession). So the
// session a transcript names is held to the claim as its session is (holdRecorded,
// dispatch://INFRA-840): a registration whose transcript names another claim's Claude session is
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
// id its Oh My Pi name gives are held to the claim (holdRecorded, dispatch://INFRA-840): a
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
// bytes and one such name would stop the boot that holds it (review N3 of #4220). A registration of
// one is refused 400, recording and enrolling nothing; one at the bound registers.
//
// The control (the pull request records the run): no check at registration admits both, since the
// hold skips a name past the bound rather than refusing it.
func TestASessionNamePastTheBoundIsRefused(t *testing.T) {
	long := strings.Repeat("s", claim.MaxSessionNameBytes+1)
	for _, tc := range []struct {
		name, session, file string
		want                claim.Refusal
	}{
		{"a session id past the bound", long, sessionFile, claim.SessionNameTooLong("session id", len(long))},
		{"a session file's name past the bound", session, "/state/sessions/" + long, claim.SessionNameTooLong("session file's name", len(long))},
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

func TestAFailedRevocationIsRetriedThenLeftToTheLease(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.revokeFails = 2
	h.launch()
	h.helloWithIdentity()
	h.register()
	id := h.claim().Enrollment.ID
	h.observe(runtime.Gone) // the first attempt runs on the machine's goroutine and fails
	if got := broker.revocations(); len(got) != 0 {
		t.Fatalf("revoked %v before any retry", got)
	}
	h.advance(revokeRetryDelay) // the second attempt, from the clock; fails
	h.advance(revokeRetryDelay) // the third succeeds
	if got := broker.revocations(); len(got) != 1 || got[0] != id {
		t.Fatalf("revocations %v after two failures, want [%s] on the third attempt", got, id)
	}
	h = newHarness(t)
	broker = h.withSecrets()
	broker.revokeFails = revokeAttempts
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.observe(runtime.Gone)
	for i := 0; i < revokeAttempts; i++ {
		h.advance(revokeRetryDelay)
	}
	if lines := h.logs.lines("agent-secrets: enrollment not revoked", "the lease ends it"); len(lines) != 1 {
		t.Fatalf("log %q, want the give-up line once", h.logs.lines("agent-secrets"))
	}
}

// TestARevocationTheBrokerRefusesIsNotRetried: a daemon cannot end an enrollment another launcher
// credential made (the broker answers 403 NOT_ENROLLING_CREDENTIAL, a refusal no retry changes), so
// the machine makes one attempt, retries nothing, and logs that the enrollment's lease ends it once
// the pod stops renewing.
func TestARevocationTheBrokerRefusesIsNotRetried(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.revokeErr = permanentErr{"broker answered 403 NOT_ENROLLING_CREDENTIAL: only the launcher credential that enrolled a pod may end its enrollment"}
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.observe(runtime.Gone)
	for range revokeAttempts {
		h.advance(revokeRetryDelay)
	}
	if broker.revokeCalls() != 1 {
		t.Fatalf("%d revocation attempts for a refusal no retry changes, want 1", broker.revokeCalls())
	}
	if lines := h.logs.lines("agent-secrets: enrollment revocation refused", "NOT_ENROLLING_CREDENTIAL", "its lease ends it"); len(lines) != 1 {
		t.Fatalf("log %q, want the refusal line once", h.logs.lines("agent-secrets"))
	}
	if lines := h.logs.lines("agent-secrets: enrollment not revoked after every attempt"); len(lines) != 0 {
		t.Fatalf("log %q: a refusal is not a run of failed attempts", lines)
	}
}

func TestATransientEnrollmentFailureIsRetriedOnTheNextAliveObservation(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.enrollErr = errors.New("broker unreachable")
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.wantState(StateRegistered)
	if c := h.claim(); c.Enrollment != nil {
		t.Fatalf("enrolled %+v through a failure", c.Enrollment)
	}
	broker.enrollErr = nil
	h.observe(runtime.Alive)
	if c := h.claim(); c.Enrollment == nil {
		t.Fatal("the alive observation did not retry the enrollment")
	}
	if got := h.conn.Enrollments(); len(got) != 1 {
		t.Fatalf("the shim was told %v", got)
	}
}

// A projected token is valid for an hour at most, and the shim reads it once per hello. An
// enrollment held up past the hello token's expiry (the daemon's token login failing and retrying)
// presents a token the shim reads again then, never the expired one, which the broker refuses as
// POD_TOKEN_EXPIRED.
func TestAnEnrollmentHeldPastTheHelloTokensExpiryPresentsAFreshToken(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.enrollErr = errors.New("broker answered 0 NO_MACHINE_CREDENTIAL: token login failed: broker answered 503 DATABASE: down")
	h.launch()
	h.helloWithToken(podToken(h.clock.Now().Add(time.Hour)))
	h.register()
	h.ready()
	if c := h.claim(); c.Enrollment != nil {
		t.Fatalf("enrolled %+v while the login was pending", c.Enrollment)
	}

	h.clock.Advance(80 * time.Minute) // the login is approved long after the hello
	broker.enrollErr = nil
	fresh := podToken(h.clock.Now().Add(time.Hour))
	h.conn.SetAgentSecretsToken(fresh)
	h.observe(runtime.Alive)

	if refused := h.logs.lines("agent-secrets: enrollment refused"); len(refused) != 0 {
		t.Fatalf("the enrollment was refused: %q", refused)
	}
	enrollments := broker.enrolled()
	if len(enrollments) != 1 || enrollments[0].PodToken != fresh {
		t.Fatalf("enrolled %+v, want one enrollment presenting the token the shim read after the hello's expired", enrollments)
	}
	if c := h.claim(); c.Enrollment == nil || c.Enrollment.ID != "enr-1" {
		t.Fatalf("stored enrollment %+v", c.Enrollment)
	}
	if h.m.identity.PodToken != "" {
		t.Fatal("the machine kept the pod token past the enrollment")
	}
}

// The machine asks the shim for its token again only when the token it holds could expire before
// the broker verifies it: within the Enroll call's RPC bound plus a minute of clock skew. A token
// that outlives that is presented as the hello carried it.
func TestAHeldTokenIsReadAgainOnlyWhenItCouldExpireBeforeTheBrokerVerifiesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		left  time.Duration // the hello token's life left when the agent registers
		reads int
	}{
		"expired":                    {-time.Minute, 1},
		"expiring within the margin": {testRPC + 30*time.Second, 1},
		"outliving the margin":       {testRPC + 2*time.Minute, 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			broker := h.withSecrets()
			fresh := podToken(h.clock.Now().Add(time.Hour))
			h.conn.SetAgentSecretsToken(fresh)
			hello := podToken(h.clock.Now().Add(tc.left))
			h.launch()
			h.helloWithToken(hello)
			h.register()
			want := hello
			if tc.reads > 0 {
				want = fresh
			}
			if enrollments := broker.enrolled(); len(enrollments) != 1 || enrollments[0].PodToken != want {
				t.Fatalf("enrolled %+v, want one enrollment presenting %s", enrollments, want)
			}
			if reads := h.conn.TokenReads(); reads != tc.reads {
				t.Fatalf("the shim was asked for its token %d times, want %d", reads, tc.reads)
			}
		})
	}
}

// A shim built before the token request forwards it to OMP, which refuses it as an unknown
// command. The machine presents the broker nothing it would refuse: it drops the identity and
// waits for the shim's next hello, whose token is read then, as it does after a refusal.
func TestAShimThatCannotReadItsTokenAgainWaitsForItsNextHello(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.conn.FailAgentSecretsToken(fmt.Errorf("worker-stream: %s refused agent-secrets-token: Unknown command: agent-secrets-token: %w", h.token, runtime.ErrTokenRefused))
	h.launch()
	h.helloWithToken(podToken(h.clock.Now().Add(-time.Minute)))
	h.register()
	h.ready()
	h.observe(runtime.Alive)
	if c := h.claim(); c.Enrollment != nil || len(broker.enrolled()) != 0 {
		t.Fatalf("enrolled %+v with no token to present", c.Enrollment)
	}
	if reads := h.conn.TokenReads(); reads != 1 {
		t.Fatalf("the shim was asked for its token %d times, want once: its refusal stands until its next hello", reads)
	}
	if lines := h.logs.lines("agent-secrets: the shim did not read its projected token again", "Unknown command"); len(lines) != 1 {
		t.Fatalf("log %q", h.logs.lines("agent-secrets"))
	}
	if refused := h.logs.lines("agent-secrets: enrollment refused"); len(refused) != 0 {
		t.Fatalf("the expired token reached the broker: %q", refused)
	}
	h.helloWithToken(podToken(h.clock.Now().Add(time.Hour))) // the shim reconnected
	if c := h.claim(); c.Enrollment == nil {
		t.Fatal("the fresh hello did not enroll")
	}
}

func TestATokenRequestTheTransportLosesIsRetriedOnTheNextAliveObservation(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.conn.FailAgentSecretsToken(errors.New("worker-stream: agent-secrets-token: no answer on the worker stream within the RPC timeout"))
	h.launch()
	h.helloWithToken(podToken(h.clock.Now().Add(-time.Minute)))
	h.register()
	h.ready()
	if c := h.claim(); c.Enrollment != nil {
		t.Fatalf("enrolled %+v with no token to present", c.Enrollment)
	}
	h.conn.FailAgentSecretsToken(nil)
	fresh := podToken(h.clock.Now().Add(time.Hour))
	h.conn.SetAgentSecretsToken(fresh)
	h.observe(runtime.Alive)
	if enrollments := broker.enrolled(); len(enrollments) != 1 || enrollments[0].PodToken != fresh {
		t.Fatalf("enrolled %+v, want the alive observation to ask again and present what the shim read", enrollments)
	}
}

func TestAPermanentEnrollmentRefusalWaitsForTheNextHello(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.enrollErr = permanentErr{"broker answered 403 POD_IDENTITY_MISMATCH: the token names another pod"}
	h.launch()
	h.helloWithIdentity()
	h.register()
	broker.enrollErr = nil
	h.observe(runtime.Alive)
	if c := h.claim(); c.Enrollment != nil {
		t.Fatal("a permanent refusal was retried with the same identity")
	}
	if lines := h.logs.lines("agent-secrets: enrollment refused", "POD_IDENTITY_MISMATCH"); len(lines) != 1 {
		t.Fatalf("log %q", h.logs.lines("agent-secrets"))
	}
	h.helloWithIdentity() // the shim reconnected with a fresh token
	if c := h.claim(); c.Enrollment == nil {
		t.Fatal("the fresh hello did not enroll")
	}
}

func TestTheEnrollmentIsResentToAReconnectedShim(t *testing.T) {
	h := newHarness(t)
	h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	h.must(StreamClosed{Claim: h.token})
	h.observe(runtime.Alive)
	h.helloWithIdentity()
	if got := h.conn.Enrollments(); len(got) != 2 || got[0] != got[1] {
		t.Fatalf("the shim was told %v, want the same id twice", got)
	}
	// The claim was already enrolled for this incarnation when the reconnected shim's hello
	// arrived, so the fresh identity the hello carried must not keep a live pod token in memory.
	if h.m.identity == nil || h.m.identity.PodToken != "" {
		t.Fatalf("the held identity kept a live pod token after an already-enrolled hello: %+v", h.m.identity)
	}
}

func TestARestartKeepsTheEnrollmentAndTellsTheReconnectedShim(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	id := h.claim().Enrollment.ID
	h.restart()
	h.helloWithIdentity()
	if e := broker.enrolled(); len(e) != 1 {
		t.Fatalf("the restarted daemon enrolled again: %+v", e)
	}
	if c := h.claim(); c.Enrollment == nil || c.Enrollment.ID != id {
		t.Fatalf("the restarted claim holds %+v, want %s", c.Enrollment, id)
	}
	if got := h.conn.Enrollments(); len(got) != 2 || got[1] != id {
		t.Fatalf("the shim was told %v", got)
	}
}

func TestWithoutABrokerNothingIsEnrolledOrRevoked(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	if c := h.claim(); c.Enrollment != nil {
		t.Fatalf("enrolled %+v with no broker", c.Enrollment)
	}
	h.observe(runtime.Gone)
	if got := h.conn.Enrollments(); len(got) != 0 {
		t.Fatalf("the shim was told %v", got)
	}
}

// expiredTokenErr is the broker refusing the presented pod token as expired (POD_TOKEN_EXPIRED).
type expiredTokenErr struct{ msg string }

func (e expiredTokenErr) Error() string      { return e.msg }
func (e expiredTokenErr) TokenExpired() bool { return true }

// A broker whose clock runs further ahead of the daemon's than podTokenSkew refuses as expired a
// token the daemon still holds good. The key stands: the machine drops the token, keeps the
// identity, and at its next observation asks the shim for the pod's current token and enrolls the
// same key with it, rather than waiting for a hello.
func TestAnExpiredTokenRefusalReadsTheTokenAgainAtTheNextObservation(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.enrollErr = expiredTokenErr{"broker answered 403 POD_TOKEN_EXPIRED: the projected token has expired (oidc: token expired at 2026-10-11T04:54:28Z)"}
	h.launch()
	h.helloWithIdentity() // a token the daemon's clock holds good for an hour
	h.register()
	h.ready()
	if lines := h.logs.lines("agent-secrets: the broker found the pod token expired", "POD_TOKEN_EXPIRED"); len(lines) != 1 {
		t.Fatalf("log %q", h.logs.lines("agent-secrets"))
	}
	if refused := h.logs.lines("agent-secrets: enrollment refused"); len(refused) != 0 {
		t.Fatalf("an expired token was read as a refusal of the key: %q", refused)
	}
	if h.m.identity == nil || h.m.identity.Thumbprint != testIdentity.Thumbprint || h.m.identity.PodToken != "" {
		t.Fatalf("held identity %+v, want the key kept and the expired token dropped", h.m.identity)
	}
	if reads := h.conn.TokenReads(); reads != 0 {
		t.Fatalf("the shim was asked for its token %d times before the refusal, want none: the daemon held it good", reads)
	}

	broker.enrollErr = nil
	fresh := podToken(h.clock.Now().Add(time.Hour + time.Minute))
	h.conn.SetAgentSecretsToken(fresh)
	h.observe(runtime.Alive)
	if reads := h.conn.TokenReads(); reads != 1 {
		t.Fatalf("the shim was asked for its token %d times at the next observation, want once", reads)
	}
	enrollments := broker.enrolled()
	if len(enrollments) != 1 || enrollments[0].PodToken != fresh || enrollments[0].Thumbprint != testIdentity.Thumbprint {
		t.Fatalf("enrolled %+v, want the same key enrolled once, with the token the shim read", enrollments)
	}
	if c := h.claim(); c.Enrollment == nil {
		t.Fatal("the claim records no enrollment")
	}
}
