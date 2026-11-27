package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The schedule TestLeaverHoldBoundByThePass builds, near the worst case that the spec's Limits
// derive for the membership pass's reach: a run's refresh step finds no due row, the next run's tick
// comes a minute after the run before it, its other sweeps and the pass's signed-out step bring it
// to its refresh step about 43 seconds after that tick, and the rows due ahead of the leaver's fill
// that step to just under its 20 seconds. About 120 seconds pass from the row coming due to the
// start of its refresh.
const (
	// passBoundRowsAhead is how many signed-in rows come due just before ada's, each refreshed in
	// about passBoundGoogleWait, so the refresh step claims her row about 17 seconds into its 20.
	passBoundRowsAhead = 17
	// passBoundSignedOut is how many signed-out rows that keep a token the pass's first step
	// checks, each ask of the broker waiting passBoundAskDelay, so its third ask starts just under
	// the 10 seconds after which that step starts none, and the step ends about 13.5 seconds in.
	passBoundSignedOut = 3
	passBoundAskDelay  = 4500 * time.Millisecond
	// passBoundSweepDelay is how long the other sweeps of each run after the schedule's take, so a
	// run reaches its refresh step about 43 seconds after its tick, within R5's 45. It is the
	// sweep delay c-bound's control sets to 0.
	passBoundSweepDelay = 29500 * time.Millisecond
	// passBoundGoogleWait is how long the fake Google waits before it answers each refresh.
	passBoundGoogleWait = time.Second
	// passBoundReach is R5's bound on the pass's reach, from a row coming due to the start of its
	// refresh, and passBoundNearWorst the least a schedule near its worst case takes.
	passBoundReach     = 125 * time.Second
	passBoundNearWorst = 110 * time.Second
	// passBoundWindow is R5's window: no request with a credential the hold refuses is admitted
	// later than this after the start of the refresh that holds the person.
	passBoundWindow = 55 * time.Second
	// passBoundAsksPast is how long the test goes on asking once the window has passed.
	passBoundAsksPast = 10 * time.Second
	// passBoundHoldWritten bounds one whole refresh, the hold's write included (identity's
	// refreshTimeout).
	passBoundHoldWritten = 15 * time.Second
	// passBoundClaimWait is how long the test waits for the pass to claim ada's row once it came
	// due: longer than the 180 seconds a 2-minute sweep interval takes, so that control's run
	// names the time the refresh started rather than only that it had not.
	passBoundClaimWait = 4 * time.Minute
)

// The membership pass holds a leaver within R5's bound on its worst schedule (c-bound's second
// test): a leaver who makes no request is reached by the pass at its real one-minute interval
// (api.MaintenanceSweepInterval) less than 125 seconds after their confirmation lapses, and from 55
// seconds after that refresh began their credentials at the broker are refused. When a run's refresh
// step claims no row, which ends that run's refresh step, the test at once makes 17 signed-in rows
// and then ada's come due, each confirmation a millisecond older than the next, so hers is due
// last; adds 3 signed-out rows whose people hold no dsp_ token, so each check of them asks the
// broker, and each such ask waits 4.5 seconds; makes the other sweeps of each later run take 29.5
// seconds; and makes the fake Google answer each refresh after 1 second, refusing ada's. No request
// uses ada's credentials from then until her row records the hold, which only her refresh's refusal
// at the fake Google writes. Her refresh starts at least 110 seconds after her row came due, which
// shows the schedule was near the worst case, and under 125; her session route at the broker,
// asked each second from then on, answers 403 MEMBERSHIP_HELD at every ask from 55 seconds after
// her refresh began, so under 180 seconds after her row came due.
func TestLeaverHoldBoundByThePass(t *testing.T) {
	t.Parallel()
	var askDelay, sweepDelay atomic.Int64
	sweeps := func(ctx context.Context) error {
		wait := time.Duration(sweepDelay.Load())
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	watch := newPassWatch(adaEmail)
	r := newLeaverRig(t, withBrokerGate(), withPersonSignIns(), withPersonAccessDelay(&askDelay), withMembershipPass(sweeps, watch.observe))
	ada := r.person(t, adaEmail)
	ahead := numbered("due", passBoundRowsAhead)
	for _, email := range ahead {
		r.signIn(t, email)
	}
	expectAnswer(t, "ada@example.com's session route at the broker, her confirmation fresh", http.StatusOK, "").got(r.readAgentGrant(t, ada))
	if t.Failed() {
		t.FailNow()
	}

	watch.arm()
	var emptied time.Time
	select {
	case emptied = <-watch.emptied:
	case <-time.After(3 * api.MaintenanceSweepInterval):
		t.Fatalf("no maintenance run's refresh step claimed no row within %s of the rig's setup", 3*api.MaintenanceSweepInterval)
	}
	watch.log.take()
	cameDue := r.comeDueInOrder(t, append(append([]string(nil), ahead...), adaEmail))
	signedOut := numbered("out", passBoundSignedOut)
	r.signedOutHolders(t, signedOut)
	askDelay.Store(int64(passBoundAskDelay))
	sweepDelay.Store(int64(passBoundSweepDelay))
	r.google.DelayToken(passBoundGoogleWait)
	r.refuse(adaEmail)
	refreshesBefore := r.google.Refreshes()
	t.Logf("a run's refresh step claimed no row; ada@example.com's row and the %d ahead of it came due %s later", passBoundRowsAhead, cameDue.Sub(emptied).Round(time.Millisecond))

	// Her refresh starts where R5 says one does, at the pass's call of Membership.ConfirmFor for her
	// row, which follows at once its report of claiming the row. The hold her row then records,
	// which only the fake Google's refusal of that refresh writes, shows the refresh reached it.
	var started time.Time
	select {
	case started = <-watch.claimed:
	case <-time.After(time.Until(cameDue.Add(passBoundClaimWait))):
		t.Fatalf("the membership pass did not claim ada@example.com's row within %s of its coming due, want under %s (R5's reach)", passBoundClaimWait, passBoundReach)
	}
	reach := started.Sub(cameDue)
	switch {
	case reach >= passBoundReach:
		t.Errorf("the membership pass started ada@example.com's refresh %s after her row came due, want under %s (R5's reach)", reach.Round(time.Millisecond), passBoundReach)
	case reach < passBoundNearWorst:
		t.Errorf("the membership pass started ada@example.com's refresh %s after her row came due, under %s: the schedule was not near the worst case of the spec's Limits (about 120 seconds), so it shows nothing of R5's %s", reach.Round(time.Millisecond), passBoundNearWorst, passBoundReach)
	default:
		t.Logf("the membership pass started ada@example.com's refresh %s after her row came due, at least %s and under %s", reach.Round(time.Millisecond), passBoundNearWorst, passBoundReach)
	}
	events := watch.log.take()
	requireSame(t, "the signed-out step of the runs up to ada@example.com's claim claimed", claimed(events, 1), signedOut)
	requireSame(t, "the refresh step of the runs up to ada@example.com's claim claimed", claimed(events, 2), append(append([]string(nil), ahead...), adaEmail))

	held := r.waitForRecordedHold(t, adaEmail, started.Add(passBoundHoldWritten))
	t.Logf("ada@example.com's row recorded the hold %s after her refresh started; the fake Google answered %d refreshes after the rows came due", held.Sub(started).Round(time.Millisecond), r.google.Refreshes()-refreshesBefore)

	windowEnds := started.Add(passBoundWindow)
	var firstRefused time.Time
	for ask := time.Now(); !ask.After(windowEnds.Add(passBoundAsksPast)); ask = ask.Add(time.Second) {
		time.Sleep(time.Until(ask))
		sent := time.Now()
		status, code := r.readAgentGrant(t, ada)
		refused := status == http.StatusForbidden && code == "MEMBERSHIP_HELD"
		if refused && firstRefused.IsZero() {
			firstRefused = sent
			t.Logf("ada@example.com's session route at the broker was first refused %s after her refresh started, %s after her row came due", sent.Sub(started).Round(time.Millisecond), sent.Sub(cameDue).Round(time.Millisecond))
		}
		if !sent.Before(windowEnds) && !refused {
			t.Errorf("ada@example.com's session route at the broker, asked %s after her refresh started (%s after her row came due), answered %d %q, want 403 MEMBERSHIP_HELD at every ask from %s after it (R5's window)", sent.Sub(started).Round(time.Millisecond), sent.Sub(cameDue).Round(time.Millisecond), status, code, passBoundWindow)
		}
	}
	if !t.Failed() {
		t.Logf("ada@example.com's session route at the broker was refused at every ask from %s after her refresh started, %s after her row came due", passBoundWindow, windowEnds.Sub(cameDue).Round(time.Millisecond))
	}
}

// passWatch is the Observe of TestLeaverHoldBoundByThePass's membership pass. It keeps each claim
// attempt (passLog) and tells the test of the two it waits on: once armed, the first refresh-step
// claim that found no row (emptied), and the refresh step's claim of who's row (claimed). It sends
// on each once, into room kept for it, so the pass's goroutine never waits on the test.
type passWatch struct {
	log     passLog
	who     string
	armed   atomic.Bool
	reached atomic.Bool
	emptied chan time.Time
	claimed chan time.Time
}

func newPassWatch(who string) *passWatch {
	return &passWatch{who: who, emptied: make(chan time.Time, 1), claimed: make(chan time.Time, 1)}
}

// arm makes the next refresh-step claim that finds no row the one emptied reports.
func (w *passWatch) arm() { w.armed.Store(true) }

func (w *passWatch) observe(event identity.MembershipPassEvent) {
	w.log.observe(event)
	at := time.Now()
	switch {
	case event.Step == 2 && !event.Claimed && w.armed.CompareAndSwap(true, false):
		w.emptied <- at
	case event.Step == 2 && event.Claimed && event.Email == w.who && w.reached.CompareAndSwap(false, true):
		w.claimed <- at
	}
}

// comeDueInOrder makes each of emails' confirmations due at once, in one statement: each is dated
// MembershipMaxAge back from the database's clock and a millisecond before the next, so the last
// comes due at that moment and is the last of them the pass claims, and none records an attempt. It
// answers that moment, when the last came due.
func (r *leaverRig) comeDueInOrder(t *testing.T, emails []string) time.Time {
	t.Helper()
	var due time.Time
	var dated int
	if err := r.outside.database.Pool.QueryRow(context.Background(), `
		with dated as (
			update people set
				confirmed_at = now() - make_interval(secs => $2) - make_interval(secs => (cardinality($1::text[]) - listed.ord) / 1000.0),
				confirm_tried_at = null
			from unnest($1::text[]) with ordinality as listed(email, ord)
			where people.email = listed.email and people.refresh_token is not null
			returning people.email
		)
		select now(), count(*) from dated
	`, emails, identity.MembershipMaxAge.Seconds()).Scan(&due, &dated); err != nil {
		t.Fatalf("make the confirmations of %v come due: %v", emails, err)
	}
	if dated != len(emails) {
		t.Fatalf("making the confirmations of %v come due dated %d rows, want %d, each with a refresh token", emails, dated, len(emails))
	}
	return due
}

// signedOutHolders records a sign-in of each of emails, a full member's with a refresh token, and
// then their sign-out with that token kept, as a sign-out does for a person whose access outlives
// the browser (KeepAfterSignOut). None of them holds a dsp_ token, so the membership pass's check
// of each asks the broker.
func (r *leaverRig) signedOutHolders(t *testing.T, emails []string) {
	t.Helper()
	ctx := context.Background()
	for _, email := range emails {
		if err := r.outside.people.SignInSubject(ctx, email, "google-"+email, "refresh-"+email, leaverDomain, model.MemberFull); err != nil {
			t.Fatalf("record %s's sign-in: %v", email, err)
		}
	}
	out, err := r.outside.database.Pool.Exec(ctx, `update people set signed_out_at = clock_timestamp() where email = any($1) and refresh_token is not null`, emails)
	if err != nil {
		t.Fatalf("sign %v out with their refresh tokens kept: %v", emails, err)
	}
	if out.RowsAffected() != int64(len(emails)) {
		t.Fatalf("signing %v out with their refresh tokens kept changed %d rows, want %d", emails, out.RowsAffected(), len(emails))
	}
}

// waitForRecordedHold waits until email's row records a hold (held_since later than signed_in_at),
// reading it every 100 milliseconds, and answers when it first read it. It fails t at deadline.
func (r *leaverRig) waitForRecordedHold(t *testing.T, email string, deadline time.Time) time.Time {
	t.Helper()
	for {
		var held bool
		if err := r.outside.database.Pool.QueryRow(context.Background(), `select coalesce(held_since > signed_in_at, false) from people where email = $1`, email).Scan(&held); err != nil {
			t.Fatalf("read whether %s's row records a hold: %v", email, err)
		}
		if held {
			return time.Now()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's row recorded no hold by %s, the end of the refresh's bound", email, deadline.Format(time.RFC3339Nano))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
