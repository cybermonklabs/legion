package identity

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The bounds of one membership pass (MembershipPass.Run): each step claims at most passRows rows,
// the sign-out step starts no ask passSignedOutWindow or more after the pass began, each of its asks
// is bounded by passAskTimeout, and the refresh step starts no refresh passRefreshWindow or more
// after it began. A refresh in flight runs to its own bound (refreshTimeout), so a pass ends within
// 50 seconds: 10 s and one 5 s ask, then 20 s and one 15 s refresh.
const (
	passRows            = 20
	passSignedOutWindow = 10 * time.Second
	passAskTimeout      = 5 * time.Second
	passRefreshWindow   = 20 * time.Second
	// passSignedOutAge is how long a signed-out row goes between two checks of whether its person
	// still has access that outlives the browser.
	passSignedOutAge = time.Hour
)

// MembershipPass is one run of the membership pass, the last work of each maintenance sweep where
// a Google rule signs people in (api.RunMaintenanceSweep, under its lease, so one process runs a
// pass at a time). It checks every stored refresh token about once an hour, whether or not its
// person makes a request, so Google's refusal holds a person who uses only a personal token or a
// broker credential, and one who signed in before holds were recorded. Under IAP identity
// (Membership.Directory set) it checks every full member's Workspace account in the Directory
// about once an hour instead (checkDirectoryDue).
type MembershipPass struct {
	// Membership refreshes a due row's confirmation with Google (Membership.ConfirmFor), or under
	// IAP checks its Directory account (Membership.ConfirmDirectory): the one the router's Standing
	// asks, so a pass and a request share one refresh, or one check, of a person.
	Membership *Membership
	// People is the store the pass claims rows from.
	People *store.PgPeopleStore
	// Outliving answers whether a signed-out person still has access that outlives the browser,
	// the check the sign-out asks (api.Deps.Outliving). The pass under IAP asks it nothing.
	Outliving cipher.OutlivingAccess
	// Observe, when set, is told of each claim attempt of either step; nil in production.
	Observe func(MembershipPassEvent)
}

// MembershipPassEvent is one claim attempt of a membership pass: its step (1, the signed-out rows;
// 2, the due confirmations, or under IAP the due Directory checks), and the email it claimed, empty
// when it claimed none (Claimed false), which ends that step.
type MembershipPassEvent struct {
	Step    int
	Email   string
	Claimed bool
}

// passCounts are the outcomes of one pass, each in one count, as its log line names them.
type passCounts struct {
	claimed                             bool
	confirmed, held, unasked, forgotten int
}

// Run runs one pass, in two steps, each claiming one row at a time so two passes never claim one
// row, and each write guarded by what its claim or refresh read, so a sign-in or a sign-out since
// is not undone:
//
//  1. Up to passRows times, while under passSignedOutWindow since the pass began and until a claim
//     finds none: claim a signed-out row with a stored token whose last check is null or older than
//     passSignedOutAge (store.PgPeopleStore.ClaimSignedOutHolder), and ask, within passAskTimeout,
//     whether its person still has access that outlives the browser. When they have none, forget
//     the token (ForgetSignedOut); when the ask fails, keep it.
//  2. Then up to passRows times, while under passRefreshWindow since this step began and until a
//     claim finds none: claim the row whose confirmation is due longest (ClaimOverdueConfirmation,
//     which stamps confirm_tried_at, so a row that could not be confirmed is tried again an hour
//     later and not by the next pass), and confirm it (Membership.ConfirmFor), which holds a person
//     Google refuses.
//
// A pass that claimed any row logs one line of its counts: confirmed, a refresh that confirmed the
// person or found them confirmed by another process since the claim; held and forgotten, a refresh
// that ended the sign-in in that mode, and forgotten also each forget of step 1 that forgot the
// token (not one whose guard a sign-in or sign-out since the claim made miss); unasked, every other
// outcome of a refresh. Step 1's checks that keep a token are not counted. A step that cannot claim
// ends, and the other still runs; Run answers both failures.
//
// Under IAP identity Run runs checkDirectoryDue alone, since IAP keeps no refresh token to forget
// or refresh, and counts an Active outcome confirmed, a Held one held, an Active one whose write
// found a later check's hold (the person stays held) held, and a Failed one unasked.
func (p MembershipPass) Run(ctx context.Context) error {
	began := time.Now()
	var counts passCounts
	var err error
	if p.Membership.ChecksDirectory() {
		err = p.checkDirectoryDue(ctx, &counts)
	} else {
		err = errors.Join(p.checkSignedOut(ctx, began, &counts), p.confirmDue(ctx, &counts))
	}
	if counts.claimed {
		slog.Info("dispatch: membership pass", "confirmed", counts.confirmed, "held", counts.held, "unasked", counts.unasked, "forgotten", counts.forgotten)
	}
	return err
}

// checkSignedOut is step 1 of Run, from began, the pass's start.
func (p MembershipPass) checkSignedOut(ctx context.Context, began time.Time, counts *passCounts) error {
	for range passRows {
		if time.Since(began) >= passSignedOutWindow {
			return nil
		}
		email, signedOutAt, signedInAt, ok, err := p.People.ClaimSignedOutHolder(ctx, passSignedOutAge)
		p.observe(1, email, ok)
		if err != nil || !ok {
			return err
		}
		counts.claimed = true
		askCtx, cancel := context.WithTimeout(ctx, passAskTimeout)
		outlives, err := p.Outliving.Holds(askCtx, email)
		cancel()
		if err != nil {
			slog.Warn("dispatch: the membership pass could not ask whether a signed-out person has access that outlives the browser; keeping their refresh token", "email", email, "error", err)
			continue
		}
		if outlives {
			continue
		}
		forgot, err := p.People.ForgetSignedOut(ctx, email, signedOutAt, signedInAt)
		if err != nil {
			return err
		}
		if forgot {
			counts.forgotten++
		}
	}
	return nil
}

// confirmDue is step 2 of Run.
func (p MembershipPass) confirmDue(ctx context.Context, counts *passCounts) error {
	began := time.Now()
	for range passRows {
		if time.Since(began) >= passRefreshWindow {
			return nil
		}
		email, ok, err := p.People.ClaimOverdueConfirmation(ctx, MembershipMaxAge)
		p.observe(2, email, ok)
		if err != nil || !ok {
			return err
		}
		counts.claimed = true
		counts.count(p.Membership.ConfirmFor(ctx, email))
	}
	return nil
}

// checkDirectoryDue is Run under IAP identity: up to passRows times, while under passRefreshWindow
// since it began and until a claim finds none, claim the full member's row whose Directory check is
// due longest (store.PgPeopleStore.ClaimDirectoryDue: held rows and rows with no refresh token
// included, the later of the last Active answer and the last check older than MembershipMaxAge, and
// it stamps directory_checked_at), and check it (Membership.ConfirmDirectory). A check in flight
// runs to its own DirectoryFlight, so the step ends within 35 seconds.
func (p MembershipPass) checkDirectoryDue(ctx context.Context, counts *passCounts) error {
	began := time.Now()
	for range passRows {
		if time.Since(began) >= passRefreshWindow {
			return nil
		}
		email, ok, err := p.People.ClaimDirectoryDue(ctx, MembershipMaxAge)
		p.observe(2, email, ok)
		if err != nil || !ok {
			return err
		}
		counts.claimed = true
		counts.count(p.Membership.ConfirmDirectory(ctx, email))
	}
	return nil
}

// count adds one refresh's outcome to its count.
func (c *passCounts) count(outcome RefreshOutcome, err error) {
	switch {
	case outcome == RefreshConfirmed, outcome == RefreshNone && err == nil:
		c.confirmed++
	case outcome == RefreshHeld:
		c.held++
	case outcome == RefreshForgot:
		c.forgotten++
	default:
		c.unasked++
	}
}

// observe tells Observe, when set, of one claim attempt.
func (p MembershipPass) observe(step int, email string, claimed bool) {
	if p.Observe != nil {
		p.Observe(MembershipPassEvent{Step: step, Email: email, Claimed: claimed})
	}
}
