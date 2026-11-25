package hold_test

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// The fixed words hold.Reason gives each way a deadline decides nothing: a not-held answer past
// its deadline, a not-held answer naming none where the broker requires one, and a person of whom
// the broker keeps no answer, where it requires one, while Dispatch cannot be asked. The first are
// true of Dispatch's unavailable answer too, which it gives as well for a person it never
// confirmed, whose answer never had a deadline that could pass.
const (
	lapsedReason     = "Dispatch's answer that they are not held has passed its deadline, or Dispatch has never confirmed them"
	noDeadlineReason = "Dispatch's answer that they are not held names no deadline, which this broker requires"
	noAnswerReason   = "the broker could not ask Dispatch, and keeps no answer Dispatch gave for them"
)

// wantUncheckable fails t unless err is hold.ErrUncheckable alone, naming person, saying reason in
// hold.Reason's words, and the check answered no deadline.
func wantUncheckable(t *testing.T, until time.Time, err error, person, reason, when string) {
	t.Helper()
	if !errors.Is(err, hold.ErrUncheckable) || errors.Is(err, hold.ErrHeld) || !strings.Contains(err.Error(), person) || hold.Reason(err) != reason || !until.IsZero() {
		t.Fatalf("CheckUntil(%s) %s = %v, %v (reason %q); want hold.ErrUncheckable naming them, saying %q, and no deadline", person, when, until, err, hold.Reason(err), reason)
	}
}

// wantServedUntil fails t unless the check served person and answered deadline.
func wantServedUntil(t *testing.T, until time.Time, err error, person string, deadline time.Time, when string) {
	t.Helper()
	if err != nil || !until.Equal(deadline) {
		t.Fatalf("CheckUntil(%s) %s = %v, %v; want served until %v", person, when, until, err, deadline)
	}
}

// TestHoldGateEnforcesNotHeldUntil (c-iap-deadline): Dispatch's answer that a person is not held,
// with a deadline (not_held_until), serves until the gate's clock reaches that deadline and not at
// it, whether the answer is fresh, cached or kept. Just before it the check serves ada and answers
// the deadline. At it and past it, with the same answer still in the 30-second cache, the check is
// hold.ErrUncheckable, saying the answer passed its deadline, logged as nothing served. A fresh
// answer whose deadline has passed is the same, and so is Dispatch's unavailable answer, whose
// not_held_until is its as_of. Once Dispatch cannot be asked, the answer person_standing keeps,
// deadline included, is the same too. A new active answer with a later deadline serves again.
//
// Controls: the gate renewing a cached answer's deadline (reading it from when the answer was
// asked, not from Dispatch's not_held_until), or serving a kept not-held answer past its deadline;
// either serves ada at or past the deadline, and the test fails naming that step.
func TestHoldGateEnforcesNotHeldUntil(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
	r := newRig(dispatch.URL, dispatch.Account, st)
	deadline := r.clock.read().Add(10 * time.Second)
	dispatch.SetNotHeldUntil(ada, deadline)

	r.clock.advance(10*time.Second - time.Millisecond)
	until, err := r.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, deadline, "a millisecond before not_held_until, from Dispatch's answer")
	r.clock.advance(time.Millisecond)
	until, err = r.gate.CheckUntil(ctx, ada)
	wantUncheckable(t, until, err, ada, lapsedReason, "at not_held_until, the answer still cached")
	r.clock.advance(time.Second)
	until, err = r.gate.CheckUntil(ctx, ada)
	wantUncheckable(t, until, err, ada, lapsedReason, "a second past not_held_until, the answer still cached")
	if calls := dispatch.Calls(); calls != 1 {
		t.Fatalf("three checks inside the cache's thirty seconds asked Dispatch %d times, want 1: the cached answer is what was judged", calls)
	}
	if _, _, kept, found := standingUntil(t, st, ada); !found || kept == nil || !kept.Equal(deadline) {
		t.Fatalf("person_standing for %s keeps the deadline %v (found %t), want %v", ada, kept, found, deadline)
	}

	r.clock.advance(projectaccess.CacheTTL)
	until, err = r.gate.CheckUntil(ctx, ada)
	wantUncheckable(t, until, err, ada, lapsedReason, "from a fresh answer whose not_held_until has passed")
	if calls := dispatch.Calls(); calls != 2 {
		t.Fatalf("the check after the cache lapsed asked Dispatch %d times in all, want 2", calls)
	}

	asOf := r.clock.read().Format(time.RFC3339Nano)
	unavailable := newRig(answering(t, http.StatusOK, `{"person":"`+ada+`","member_kind":"full","held":false,"as_of":"`+asOf+`","not_held_until":"`+asOf+`"}`),
		projectaccesstest.StaticAccount("token"), storetest.Open(t))
	unavailable.clock.advance(r.clock.read().Sub(unavailable.clock.read()))
	until, err = unavailable.gate.CheckUntil(ctx, ada)
	wantUncheckable(t, until, err, ada, lapsedReason, "from Dispatch's unavailable answer, not_held_until equal to as_of")

	dispatch.SetDown(true)
	r.clock.advance(projectaccess.CacheTTL)
	until, err = r.gate.CheckUntil(ctx, ada)
	wantUncheckable(t, until, err, ada, lapsedReason, "with Dispatch stopped, from the kept answer whose not_held_until has passed")
	if warned := r.logs.at(slog.LevelWarn, servedWarning, ada); len(warned) != 0 {
		t.Fatalf("refusing %s on a kept answer past its deadline logged %+v, want no line saying she was served", ada, warned)
	}

	renewed := r.clock.read().Add(24 * time.Hour)
	dispatch.SetNotHeldUntil(ada, renewed)
	dispatch.SetDown(false)
	r.clock.advance(5 * time.Second)
	until, err = r.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, renewed, "once Dispatch answers a new active answer with a later deadline")
}

// TestHoldGateReadsAnUnavailableAnswerByDispatchsTimes (c-iap-deadline): Dispatch answers a
// person it cannot confirm (no active Workspace Directory answer in 24 hours, or no row) with
// not_held_until equal to as_of, both its database's time. The gate reads that from the answer's
// two times, whatever its own clock says: with the broker's clock five minutes behind Dispatch's,
// the unavailable answer is hold.ErrUncheckable fresh and, Dispatch stopped, kept. A not-held
// answer whose deadline is ahead of its as_of is still judged by the gate's clock: under the same
// skew it is served until that deadline, which the gate answers.
//
// Control: judge the unavailable answer by the gate's clock alone (as before); the broker behind
// Dispatch serves ada until her as_of, and the test fails naming the fresh answer.
func TestHoldGateReadsAnUnavailableAnswerByDispatchsTimes(t *testing.T) {
	ctx := t.Context()
	databaseURL := storetest.URL(t)
	st := openStore(t, databaseURL)
	// Every rig's clock starts at the same instant: Dispatch's as_of is five minutes after it.
	asOf := newRig("http://127.0.0.1:1", projectaccesstest.StaticAccount("token"), st).clock.read().Add(5 * time.Minute)
	answer := func(until time.Time) string {
		return `{"person":"` + ada + `","member_kind":"full","held":false,"as_of":"` + asOf.Format(time.RFC3339Nano) +
			`","not_held_until":"` + until.Format(time.RFC3339Nano) + `"}`
	}

	unavailable := newRig(answering(t, http.StatusOK, answer(asOf)), projectaccesstest.StaticAccount("token"), st)
	until, err := unavailable.gate.CheckUntil(ctx, ada)
	wantUncheckable(t, until, err, ada, lapsedReason, "from Dispatch's unavailable answer, the broker's clock five minutes behind its as_of")
	if _, kept, keptUntil, found := standingUntil(t, st, ada); !found || !kept.Equal(asOf) || keptUntil == nil || !keptUntil.Equal(asOf) {
		t.Fatalf("person_standing for %s keeps as_of %v and deadline %v (found %t), want both %v", ada, kept, keptUntil, found, asOf)
	}

	stopped := newRig(answering(t, http.StatusServiceUnavailable, ""), projectaccesstest.StaticAccount("token"), openStore(t, databaseURL))
	until, err = stopped.gate.CheckUntil(ctx, ada)
	wantUncheckable(t, until, err, ada, lapsedReason, "with Dispatch stopped, from the kept unavailable answer, the broker's clock five minutes behind its as_of")

	ahead := asOf.Add(24 * time.Hour)
	served := newRig(answering(t, http.StatusOK, answer(ahead)), projectaccesstest.StaticAccount("token"), storetest.Open(t))
	until, err = served.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, ahead, "from a not-held answer whose deadline is 24 hours after its as_of, the broker's clock behind")
}

// TestHoldGateKeepsAHeldAnswerPastItsDeadline (c-iap-deadline): a held answer is hold.ErrHeld
// before and after any deadline, fresh or kept, and never hold.ErrUncheckable. Ada, not held until
// T, is then held by Dispatch: her check past T is held from Dispatch's answer and, Dispatch
// stopped, from the answer person_standing keeps, at this gate and at a new one. A held answer that
// carries a not_held_until already past, which Dispatch never sends, and a kept held row with one,
// are held too, and the check answers no deadline for any of them.
//
// Control: the gate judging the deadline before whether the answer is held (a held person answered
// unavailable once a deadline passes); the held answers carrying a past deadline answer
// hold.ErrUncheckable, and the test fails naming them.
func TestHoldGateKeepsAHeldAnswerPastItsDeadline(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
	r := newRig(dispatch.URL, dispatch.Account, st)
	deadline := r.clock.read().Add(10 * time.Second)
	dispatch.SetNotHeldUntil(ada, deadline)
	until, err := r.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, deadline, "before Dispatch holds her")

	held := func(g *hold.Gate, person, when string) {
		t.Helper()
		until, err := g.CheckUntil(ctx, person)
		if !isHeld(err) || !until.IsZero() {
			t.Fatalf("CheckUntil(%s) %s = %v, %v; want hold.ErrHeld alone and no deadline", person, when, until, err)
		}
	}
	dispatch.SetHeld(ada, true)
	r.clock.advance(projectaccess.CacheTTL)
	held(r.gate, ada, "past her old not_held_until, from Dispatch's held answer")
	dispatch.SetDown(true)
	r.clock.advance(projectaccess.CacheTTL)
	held(r.gate, ada, "with Dispatch stopped, from the kept held answer")
	held(newRig(dispatch.URL, dispatch.Account, st).gate, ada, "at a new gate, Dispatch stopped")

	wire := newRig(answering(t, http.StatusOK, `{"person":"`+carol+`","member_kind":"full","held":true,"as_of":"2026-11-15T00:00:00Z","not_held_until":"2026-11-15T00:00:01Z"}`),
		projectaccesstest.StaticAccount("token"), st)
	held(wire.gate, carol, "from a held answer carrying a not_held_until already past")
	if err := st.KeepPersonStanding(ctx, dave, true, time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), new(time.Date(2026, 11, 15, 0, 0, 1, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	stopped := newRig(answering(t, http.StatusServiceUnavailable, `{"code":"UNAVAILABLE","error":"down"}`), projectaccesstest.StaticAccount("token"), st)
	held(stopped.gate, dave, "with Dispatch stopped, from a kept held row carrying a not_held_until already past")
}

// TestHoldGateRequiresADeadlineUnderIAP (c-iap-deadline): with RequireDeadline, which
// BROKER_HOLD_REQUIRES_DEADLINE sets on an IAP deployment's broker, a not-held answer naming no
// deadline is hold.ErrUncheckable, fresh or kept, and so is a person of whom the broker keeps no
// answer while Dispatch cannot be asked, each saying why, and neither is logged or counted as
// served. A not-held answer with a deadline ahead is served, from Dispatch and, Dispatch stopped,
// from the kept row with the WARN line and the counter, and a held one is held. A gate without
// RequireDeadline serves the same people as before the deadline existed.
//
// Control: the gate serving a person with no kept answer while Dispatch cannot be asked under
// RequireDeadline; carol is served, and the test fails naming her.
func TestHoldGateRequiresADeadlineUnderIAP(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull, bob: projectaccess.MemberFull, dave: projectaccess.MemberFull})
	iap := newRigWith(dispatch.URL, dispatch.Account, st, true)
	plain := newRig(dispatch.URL, dispatch.Account, st)
	deadline := iap.clock.read().Add(24 * time.Hour)
	dispatch.SetNotHeldUntil(ada, deadline)
	dispatch.SetHeld(dave, true)

	until, err := iap.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, deadline, "under RequireDeadline, from Dispatch's answer with a deadline ahead")
	until, err = iap.gate.CheckUntil(ctx, bob)
	wantUncheckable(t, until, err, bob, noDeadlineReason, "under RequireDeadline, from Dispatch's not-held answer naming no deadline")
	if err := iap.gate.Check(ctx, dave); !isHeld(err) {
		t.Fatalf("Check(%s) under RequireDeadline, Dispatch holding him = %v, want hold.ErrHeld", dave, err)
	}
	if until, err := plain.gate.CheckUntil(ctx, bob); err != nil || !until.IsZero() {
		t.Fatalf("CheckUntil(%s) without RequireDeadline, from the not-held answer naming no deadline = %v, %v; want served with no deadline", bob, until, err)
	}

	dispatch.SetDown(true)
	iap.clock.advance(projectaccess.CacheTTL)
	plain.clock.advance(projectaccess.CacheTTL)
	until, err = iap.gate.CheckUntil(ctx, carol)
	wantUncheckable(t, until, err, carol, noAnswerReason, "under RequireDeadline, Dispatch stopped and no answer kept for her")
	until, err = iap.gate.CheckUntil(ctx, bob)
	wantUncheckable(t, until, err, bob, noDeadlineReason, "under RequireDeadline, Dispatch stopped, from the kept answer naming no deadline")
	for _, person := range []string{carol, bob} {
		if warned := iap.logs.at(slog.LevelWarn, servedWarning, person); len(warned) != 0 {
			t.Fatalf("refusing %s under RequireDeadline logged %+v, want no line saying they were served", person, warned)
		}
	}
	if got := iap.unchecked(t, "none") + iap.unchecked(t, "not_held"); got != 0 {
		t.Fatalf("broker_hold_unchecked_total after refusing carol and bob under RequireDeadline = %d, want 0: nobody was served", got)
	}
	until, err = iap.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, deadline, "under RequireDeadline, Dispatch stopped, from the kept answer with a deadline ahead")
	if warned := iap.logs.at(slog.LevelWarn, servedWarning, ada); len(warned) != 1 || warned[0].attrs["last_answer"] != "not_held" || iap.unchecked(t, "not_held") != 1 {
		t.Fatalf("serving %s on her kept answer logged %+v and counted %d, want one WARN line with last_answer not_held and a count of 1", ada, warned, iap.unchecked(t, "not_held"))
	}
	if err := iap.gate.Check(ctx, dave); !isHeld(err) {
		t.Fatalf("Check(%s) under RequireDeadline, Dispatch stopped, kept held = %v, want hold.ErrHeld", dave, err)
	}
	for _, person := range []string{carol, bob} {
		if err := plain.gate.Check(ctx, person); err != nil {
			t.Fatalf("Check(%s) without RequireDeadline, Dispatch stopped = %v, want served as before the deadline existed", person, err)
		}
	}
}

// TestHoldGateDeadlineSurvivesARestart (c-iap-deadline): the deadline is kept in person_standing
// with its answer, so a new gate over the same database, Dispatch stopped, serves the kept
// not-held answer until its deadline and not at it, answering that deadline, and holds the kept
// held one; with RequireDeadline it refuses a person of whom no answer is kept, and without it
// serves them.
//
// Control: the gate writing no deadline to person_standing (KeepPersonStanding handed none); the
// new gate serves ada past her deadline, and the test fails naming her.
func TestHoldGateDeadlineSurvivesARestart(t *testing.T) {
	ctx := t.Context()
	databaseURL := storetest.URL(t)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull, bob: projectaccess.MemberFull})
	first := newRig(dispatch.URL, dispatch.Account, openStore(t, databaseURL))
	deadline := first.clock.read().Add(10 * time.Second)
	dispatch.SetNotHeldUntil(ada, deadline)
	dispatch.SetHeld(bob, true)
	until, err := first.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, deadline, "at the first gate, from Dispatch's answer")
	if err := first.gate.Check(ctx, bob); !isHeld(err) {
		t.Fatalf("Check(%s) at the first gate = %v, want hold.ErrHeld", bob, err)
	}
	dispatch.SetDown(true)

	second := newRigWith(dispatch.URL, dispatch.Account, openStore(t, databaseURL), true)
	second.clock.advance(10*time.Second - time.Millisecond)
	until, err = second.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, deadline, "at a new gate, Dispatch stopped, a millisecond before the kept not_held_until")
	second.clock.advance(time.Millisecond)
	until, err = second.gate.CheckUntil(ctx, ada)
	wantUncheckable(t, until, err, ada, lapsedReason, "at a new gate, Dispatch stopped, at the kept not_held_until")
	if err := second.gate.Check(ctx, bob); !isHeld(err) {
		t.Fatalf("Check(%s) at a new gate, Dispatch stopped = %v, want hold.ErrHeld from the kept answer", bob, err)
	}
	until, err = second.gate.CheckUntil(ctx, carol)
	wantUncheckable(t, until, err, carol, noAnswerReason, "at a new gate under RequireDeadline, Dispatch stopped and no answer kept for her")

	third := newRig(dispatch.URL, dispatch.Account, openStore(t, databaseURL))
	if err := third.gate.Check(ctx, carol); err != nil {
		t.Fatalf("Check(%s) at a new gate without RequireDeadline, Dispatch stopped and no answer kept = %v, want served unchecked", carol, err)
	}
	until, err = third.gate.CheckUntil(ctx, ada)
	wantServedUntil(t, until, err, ada, deadline, "at a third gate, Dispatch stopped, before the kept not_held_until")
}

// TestHeldWordsFollowTheDeadlineRule: the words every point that refuses a held person gives name
// the person and what ends the hold. A gate without RequireDeadline says their next Dispatch
// sign-in, or Dispatch confirming them with Google, ends it. Under RequireDeadline, which only an
// IAP deployment's broker sets, a sign-in releases nothing and the Workspace Directory's next
// active answer does, so the words say that, as Dispatch's own refusal under IAP says it.
//
// Control: HeldWords ignoring RequireDeadline; the IAP gate's words tell ada to sign in, and the
// test fails naming them.
func TestHeldWordsFollowTheDeadlineRule(t *testing.T) {
	st := storetest.Open(t)
	signIn := ada + "'s credentials are held by Dispatch; they work again when " + ada + " signs in to Dispatch, or when Dispatch can confirm " + ada + " with Google again"
	directory := "Google's Workspace Directory no longer confirms " + ada + "; their credentials work again once it does"
	if got := newRig("http://127.0.0.1:1", projectaccesstest.StaticAccount("token"), st).gate.HeldWords(ada); got != signIn {
		t.Fatalf("HeldWords(%s) without RequireDeadline = %q, want %q", ada, got, signIn)
	}
	if got := newRigWith("http://127.0.0.1:1", projectaccesstest.StaticAccount("token"), st, true).gate.HeldWords(ada); got != directory {
		t.Fatalf("HeldWords(%s) under RequireDeadline = %q, want %q", ada, got, directory)
	}
}
