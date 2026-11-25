package supervise

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

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
	ends := epoch.Add(12 * time.Hour)

	t.Run("the recovery of a claim at the cap", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.must(RequestForgetSession{Claim: h.token, Session: session})
		const fresh, freshFile = "ses_after_forget", "/state/sessions/after-forget.jsonl"
		h.store.fillHolds(h.token, MaxHeldNamesPerClaim-h.store.heldBy(h.token)-len(RecordedHolds(fresh, freshFile))+1)
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
		var refused *RefusedError
		if !errors.As(err, &carried) || len(carried.Carried) != 1 || carried.Carried[0].EnrollmentID != "enr-1" ||
			!errors.As(err, &refused) || refused.Request != "release-names" {
			t.Fatalf("the release while the broker carries %s = %v, want a release-names RefusedError naming enrollment enr-1", session, err)
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
		h.store.fillHolds(h.token, 3)
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
		h.store.fillHolds(h.token, 3)
		broker.carriedErr = errors.New("broker unreachable")
		held := h.store.heldBy(h.token)
		if _, err := h.m.ReleaseNames(h.ctx); !errors.Is(err, ErrCarryUnanswered) || errors.Is(err, ErrCarryRefused) {
			t.Fatalf("the release with no broker answer = %v, want ErrCarryUnanswered", err)
		}
		if got := h.store.heldBy(h.token); got != held {
			t.Fatalf("the unanswered release left the claim holding %d names, want the %d it held", got, held)
		}
	})

	// A broker that refuses the question for good (a 4xx) is ErrCarryRefused, not an outage, and an
	// answer too long to read is a carry the release cannot list, refused as one (side review of
	// #4355): neither frees anything.
	t.Run("a broker that refuses the question frees nothing", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		h.store.fillHolds(h.token, 3)
		broker.carriedErr = permanentErr{msg: "broker answered 403 SERVICE_LOGIN_REQUIRED"}
		held := h.store.heldBy(h.token)
		if _, err := h.m.ReleaseNames(h.ctx); !errors.Is(err, ErrCarryRefused) || errors.Is(err, ErrCarryUnanswered) {
			t.Fatalf("the release the broker refuses = %v, want ErrCarryRefused", err)
		}
		if got := h.store.heldBy(h.token); got != held {
			t.Fatalf("the refused release left the claim holding %d names, want the %d it held", got, held)
		}
	})
	t.Run("an answer too long to read is a carry the release cannot list", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		h.store.fillHolds(h.token, 3)
		broker.carriedErr = unlistedAnswer{}
		held := h.store.heldBy(h.token)
		_, err := h.m.ReleaseNames(h.ctx)
		var carried *NamesCarriedError
		var refused *RefusedError
		if !errors.As(err, &carried) || carried.Unlisted == nil || len(carried.Carried) != 0 ||
			!errors.As(err, &refused) || errors.Is(err, ErrCarryUnanswered) {
			t.Fatalf("the release whose answer is too long to read = %v, want a RefusedError of a carry it cannot list", err)
		}
		if !strings.Contains(err.Error(), "longer than") {
			t.Fatalf("the refusal %q does not say the answer was too long to list", err)
		}
		if got := h.store.heldBy(h.token); got != held {
			t.Fatalf("the refused release left the claim holding %d names, want the %d it held", got, held)
		}
	})

	t.Run("a daemon with no broker frees with no check and says so", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.store.fillHolds(h.token, 3)
		released, err := h.m.ReleaseNames(h.ctx)
		if err != nil || len(released.Freed) != 3 || released.Checked != CheckedNoBroker {
			t.Fatalf("the release with no broker = %+v, %v; want the 3 earlier names freed, checked %q", released, err, CheckedNoBroker)
		}
	})

	// A release with no name to free asks no broker and says so, rather than reporting a check
	// that never ran (review of #4355, F3).
	t.Run("a release with nothing to free asks no broker", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		released, err := h.m.ReleaseNames(h.ctx)
		if err != nil || len(released.Freed) != 0 || released.Checked != CheckedNothing || released.Abandoned != "" {
			t.Fatalf("the release with nothing to free = %+v, %v; want nothing freed, checked %q", released, err, CheckedNothing)
		}
		if asked := broker.askedCarried(); len(asked) != 0 {
			t.Fatalf("the broker was asked %v about a release that frees nothing", asked)
		}
	})

	// A release whose commit freed the abandoned session's names but whose answer was lost leaves
	// memory without the mark, and the claim's next write erases the stored one (review of #4355,
	// F2). Run again, the release has nothing to free and marks the session all the same, so the
	// boot does not hold its names for this claim again.
	t.Run("a release with nothing to free marks the abandoned session", func(t *testing.T) {
		h := newHarness(t)
		broker := h.withSecrets()
		h.reach(StateReady)
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.must(RequestForgetSession{Claim: h.token, Session: session})
		// The store as a committed release left it: the abandoned session's names freed, the
		// mark that commit wrote erased by the claim's next write, which wrote memory's claim.
		h.store.mu.Lock()
		for name, holder := range h.store.holds {
			if holder == h.token {
				delete(h.store.holds, name)
			}
		}
		h.store.mu.Unlock()
		if a := h.store.load(h.token).Abandoned; a.Session != session || !a.Released.IsZero() {
			t.Fatalf("the stored claim's abandoned session is %+v, want %s not released", a, session)
		}

		released, err := h.m.ReleaseNames(h.ctx)
		if err != nil || len(released.Freed) != 0 || released.Checked != CheckedNothing || released.Abandoned != session {
			t.Fatalf("the release run again = %+v, %v; want nothing freed, checked %q, %s marked released", released, err, CheckedNothing, session)
		}
		if a := h.store.load(h.token).Abandoned; a.Released.IsZero() {
			t.Fatalf("the stored claim's abandoned session is %+v after the release, want it marked released", a)
		}
		if asked := broker.askedCarried(); len(asked) != 0 {
			t.Fatalf("the broker was asked %v about a release that frees nothing", asked)
		}
	})
}

// A release is one store write, the delete of the names and the claim's record of the abandoned
// session it freed together (Store.ReleaseHolds; review of #4355): a claim write the store refuses
// frees no name, and the machine does not mark the abandoned session released, so the claim's
// record and the store's holds still agree and the operator's next run frees the names and marks
// the session once. The names it would have freed leave the machine's cache all the same (a lost
// answer may hide a commit; TestAReleaseWhoseAnswerIsLostLeavesNoFreedNameCached), which costs
// the claim one store call at its next registration of one of them.
//
// The controls (the pull request records each run): the machine marking the session released
// before the write leaves memory marked after the refusal; the store freeing the names before it
// writes the claim (two commits) leaves the claim holding none of them after the refusal.
func TestARefusedClaimWriteFreesNoNameOfARelease(t *testing.T) {
	h := newHarness(t)
	h.withSecrets()
	h.reach(StateReady)
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.must(RequestForgetSession{Claim: h.token, Session: session})
	held := h.store.heldBy(h.token)
	if held == 0 {
		t.Fatal("the claim holds no name of the session it abandoned, so a release would free nothing")
	}

	h.store.fail("PutClaim", errors.New("the database refused the claim write"))
	if released, err := h.m.ReleaseNames(h.ctx); err == nil {
		t.Fatalf("the release with its claim write refused = %+v, want the refusal", released)
	}
	h.store.fail("PutClaim", nil)
	if got := h.store.heldBy(h.token); got != held {
		t.Fatalf("the claim holds %d names after the release's claim write was refused, want the %d it held: the release freed names while its claim's record still says it freed nothing", got, held)
	}
	if a := h.store.load(h.token).Abandoned; !a.Released.IsZero() {
		t.Fatalf("the stored claim's abandoned session is %+v after the refusal, want it not released", a)
	}
	if a := h.m.View().Claim.Abandoned; !a.Released.IsZero() {
		t.Fatalf("the machine's claim marks its abandoned session released, %+v, after the store refused the write", a)
	}

	released, err := h.m.ReleaseNames(h.ctx)
	if err != nil || len(released.Freed) != held || released.Abandoned != session {
		t.Fatalf("the release run again = %+v, %v; want all %d names freed and %s marked released", released, err, held, session)
	}
	if got := h.store.heldBy(h.token); got != 0 {
		t.Fatalf("the claim holds %d names after the release run again, want none", got)
	}
	stored, memory := h.store.load(h.token).Abandoned, h.m.View().Claim.Abandoned
	if stored.Released.IsZero() || !stored.Released.Equal(memory.Released) {
		t.Fatalf("the abandoned session is released at %v in the store and %v in memory, want one time in both", stored.Released, memory.Released)
	}
}

// unlistedAnswer is the broker client's answer that was too long to read whole (cipher's
// CarriedAnswerTooLargeError), as the machine sees it (UnlistedCarryError).
type unlistedAnswer struct{}

func (unlistedAnswer) Error() string {
	return "the broker's answer is longer than the 8388608 bytes the daemon reads of one, so it names more enrollments than it can list"
}
func (unlistedAnswer) UnlistedCarry() bool { return true }

// lostAnswerStore commits a release and then answers an error, as a commit whose answer the
// claim-write deadline or a dropped connection cut off does.
type lostAnswerStore struct{ Store }

func (s lostAnswerStore) ReleaseHolds(ctx context.Context, c Claim, holds []Hold) ([]Hold, error) {
	if _, err := s.Store.ReleaseHolds(ctx, c, holds); err != nil {
		return nil, err
	}
	return nil, errors.New("the commit's answer was lost")
}

// A release whose commit's answer is lost freed its names all the same, so it drops them from the
// machine's cache (heldNames) whatever the write answers (review of #4355, F1): a name left cached
// would let the claim register it again without asking the store, after another claim took it.
// Here the freed name is the base name of an earlier file of the current session, which does not
// contain the id; with the answer lost, the claim's next registration of that file is still
// refused, as it is with the answer delivered.
//
// The control (the pull request records the run): ReleaseNames returning on the error before it
// drops the names: "the re-registration of /state/sessions/implementer.jsonl, whose name
// legion-legion-LEGION-209-tester now holds, = <nil>, want the hold's refusal" in the lost row.
func TestAReleaseWhoseAnswerIsLostLeavesNoFreedNameCached(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	const second = "/state/sessions/second.jsonl"
	earlier := Hold{Kind: HoldFileName, Name: "implementer.jsonl"}
	for _, lost := range []bool{false, true} {
		name := "the release's answer delivered"
		if lost {
			name = "the release's answer lost"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if lost {
				h.deps.Store = lostAnswerStore{h.store}
				h.start(h.store.load(h.token))
			}
			h.reach(StateReady)
			h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: second,
				CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
			if _, err := h.m.ReleaseNames(h.ctx); (err != nil) != lost {
				t.Fatalf("the release = %v, want an error only when its answer is lost", err)
			}
			if holder, held := h.store.holdOf(earlier); held {
				t.Fatalf("%s is still held for %s after the release committed", earlier.Name, holder)
			}
			if holders, err := h.store.HoldSessions(h.ctx, "", other, []Hold{earlier}); err != nil || len(holders) != 1 || holders[0] != other {
				t.Fatalf("another claim's hold of the freed %s = %v, %v; want it %s's", earlier.Name, holders, err, other)
			}
			err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
				CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})
			if refusal := (claim.Refusal{}); !errors.As(err, &refusal) {
				t.Fatalf("the re-registration of %s, whose name %s now holds, = %v, want the hold's refusal", sessionFile, other, err)
			}
		})
	}
}

// Before its agent registers, a Claude Code launch's chosen session id is held and registered for
// the pod to trade under (ensureRegistered) while the claim records no session, so a release keeps
// it as the claim's current session (review of #4355, F2; red team P3-1): another claim cannot then
// take it while the pod's registration renews it.
//
// The control (the pull request records the run): ReleaseNames keeping only the session the claim
// records: "the release freed [{session ses_chosen}], the session the claim's live pod-session
// registration names".
func TestAReleaseKeepsTheSessionALaunchChoseAndRegistered(t *testing.T) {
	const pod = "pod-uid-54"
	h := newHarness(t)
	h.withSecrets()
	recorder := &sessionRecorder{}
	h.deps.Registrar = recorder
	h.start(h.store.load(h.token))
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: runtime.SandboxIncarnation(pod, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: 1},
	}})
	h.rt.ScriptLaunchSession(h.token, "ses_chosen")
	h.launch()
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation(), AgentSecrets: &AgentSecretsIdentity{Thumbprint: testIdentity.Thumbprint, PodToken: testIdentity.PodToken}})
	h.observe(runtime.Alive)
	if got := recorder.registered(); !slices.Equal(got, []string{"ses_chosen"}) {
		t.Fatalf("the pod registered %v, want ses_chosen", got)
	}
	released, err := h.m.ReleaseNames(h.ctx)
	if err != nil || slices.Contains(released.Freed, sessionID("ses_chosen")) {
		t.Fatalf("the release freed %v (%v), the session the claim's live pod-session registration names", released.Freed, err)
	}
	if holder, _ := h.store.holdOf(sessionID("ses_chosen")); holder != h.token {
		t.Fatalf("ses_chosen is held for %q after the release, want %s", holder, h.token)
	}
}

// A release by a daemon that registers pod sessions (Deps.Registrar) has the secrets broker
// release the session names it would free before it frees any (releaseSessions): the names of
// kind session alone, since no registration holds a file's name. A broker that does not answer
// that it released them is asked three times, a second and then two apart, each failure logged at
// Warn with the attempt and the broker's reason, and the release frees nothing: it answers
// ErrReleaseUnanswered for a 5xx, and for a 404 from a broker of a build before the route, and
// ErrReleaseRefused for any other 4xx, a refusal a retry cannot change. A broker that answers at
// the third attempt lets every name free.
//
// The control (the pull request records the run): releaseSessions reading every permanent refusal
// as one for good fails the 404 row, which answers ErrReleaseRefused.
func TestAReleaseFreesNoNameTheBrokerHasNotReleased(t *testing.T) {
	unavailable := &cipher.APIError{Status: http.StatusServiceUnavailable, Code: "UNKNOWN", Message: "no healthy upstream"}
	unserved := &cipher.APIError{Status: http.StatusNotFound, Code: "UNKNOWN", Message: "404 page not found"}
	unlisted := &cipher.APIError{Status: http.StatusForbidden, Code: "ACCOUNT_NOT_LISTED", Message: "the account is not listed"}
	for _, tc := range []struct {
		name      string
		answers   []error
		want, not error
	}{
		{"a broker that does not answer", []error{unavailable, unavailable, unavailable}, ErrReleaseUnanswered, ErrReleaseRefused},
		{"a broker from before the route", []error{unserved, unserved, unserved}, ErrReleaseUnanswered, ErrReleaseRefused},
		{"a broker that refuses the release for good", []error{unlisted, unlisted, unlisted}, ErrReleaseRefused, ErrReleaseUnanswered},
		{"a broker that answers at the third attempt", []error{unavailable, errors.New("dial tcp: connection refused")}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.withSecrets()
			recorder := &sessionRecorder{releaseErrs: slices.Clone(tc.answers)}
			h.deps.Registrar = recorder
			h.start(h.store.load(h.token))
			h.reach(StateReady)
			h.store.fillHolds(h.token, 2)
			h.store.setHold(Hold{Kind: HoldFileName, Name: "earlier.jsonl"}, h.token)
			held := h.store.heldBy(h.token)
			h.clock.mu.Lock()
			sleptBefore := len(h.clock.slept)
			h.clock.mu.Unlock()

			released, err := h.m.ReleaseNames(h.ctx)

			asked := recorder.released()
			if len(asked) != releaseAttempts {
				t.Fatalf("the release asked the broker %d times (%v), want %d", len(asked), asked, releaseAttempts)
			}
			for i, names := range asked {
				if slices.Sort(names); !slices.Equal(names, []string{"ses_earlier_0", "ses_earlier_1"}) {
					t.Fatalf("attempt %d asked the broker to release %v, want the two earlier session ids alone", i+1, names)
				}
			}
			h.clock.mu.Lock()
			slept := slices.Clone(h.clock.slept[sleptBefore:])
			h.clock.mu.Unlock()
			if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(slept, want) {
				t.Fatalf("the release waited %v between its attempts, want %v", slept, want)
			}
			warned := h.logs.lines("level=WARN", "supervise: the secrets broker did not release a claim's session names; they stay held")
			if len(warned) != len(tc.answers) {
				t.Fatalf("the release logged %d Warn lines, want one for each of the %d failed attempts:\n%s", len(warned), len(tc.answers), strings.Join(warned, "\n"))
			}
			for i, line := range warned {
				if !strings.Contains(line, fmt.Sprintf("attempt=%d", i+1)) || !strings.Contains(line, "names=2") || !strings.Contains(line, tc.answers[i].Error()) {
					t.Errorf("the Warn line of attempt %d is %q, want it to name the attempt, the 2 names and the broker's reason %q", i+1, line, tc.answers[i])
				}
			}

			if tc.want == nil {
				if err != nil || len(released.Freed) != 3 || h.store.heldBy(h.token) != held-3 {
					t.Fatalf("the release the broker answered at the third attempt = %+v, %v; want the 3 earlier names freed", released, err)
				}
				return
			}
			if !errors.Is(err, tc.want) || errors.Is(err, tc.not) || !strings.Contains(err.Error(), tc.answers[len(tc.answers)-1].Error()) {
				t.Fatalf("the release = %v, want %v naming the broker's reason, never %v", err, tc.want, tc.not)
			}
			if got := h.store.heldBy(h.token); got != held {
				t.Fatalf("the release the broker did not answer left the claim holding %d names, want the %d it held", got, held)
			}
		})
	}
}
