package identity

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/identity/directorytest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The IAP Directory tests (R9, c-iap-directory): Standing and the membership pass under IAP
// identity, on the real people store, with the Directory reader reaching t-23's fake Google hosts.
// Ada's address is directoryAda, the address the fake's rows answer for.

// directoryStandingRig is the people store on a database of its own, the fake Google hosts, and a
// Membership that follows the Directory as one IAP Dispatch process holds it. outliving is the
// question a sign-out asks that each Membership is handed, as cmd/dispatch hands it the router's:
// over the rig's database, with no broker unless a test gives it one.
type directoryStandingRig struct {
	t          *testing.T
	database   *store.Store
	people     *store.PgPeopleStore
	google     *directorytest.Google
	outliving  cipher.OutlivingAccess
	membership *Membership
}

func newDirectoryStandingRig(t *testing.T) *directoryStandingRig {
	t.Helper()
	database := storetest.Open(t)
	r := &directoryStandingRig{
		t:         t,
		database:  database,
		people:    store.NewPgPeopleStore(database.Pool, testSigningKey, nil),
		google:    directorytest.ServeGoogle(t),
		outliving: cipher.OutlivingAccess{Pool: database.Pool},
	}
	r.newProcess()
	return r
}

// newProcess is another Dispatch process over the same database and Google: a reader holding no
// minted token, and a Membership with no check in flight and no pause.
func (r *directoryStandingRig) newProcess() {
	r.t.Helper()
	reader, err := NewDirectoryReader(r.t.Context(), directorySigner, directoryAdmin, r.google.ClientOptions()...)
	if err != nil {
		r.t.Fatalf("NewDirectoryReader: %v", err)
	}
	r.membership = &Membership{People: r.people, Rule: workspace, Directory: reader, Outliving: r.outliving}
}

// pass is one membership pass over the rig's Membership, telling observe of each claim attempt.
func (r *directoryStandingRig) pass(observe func(MembershipPassEvent)) MembershipPass {
	return MembershipPass{Membership: r.membership, People: r.people, Observe: observe}
}

// record records email as IAP's sign-in does (IAPIdentity.Login, PeopleStore.Record): a full
// member's row with no refresh token and no Directory answer.
func (r *directoryStandingRig) record(email string) {
	r.t.Helper()
	if err := r.people.Record(context.Background(), email); err != nil {
		r.t.Fatalf("record %s: %v", email, err)
	}
}

// active records an Active outcome for email under the Directory user id, resting on a read of the
// row made now, as a check's write does, and dates every Directory column age back, so the row's
// confirmation is age old and its deadline 24 hours after.
func (r *directoryStandingRig) active(email, id string, age time.Duration) {
	r.t.Helper()
	read, _, err := r.people.Membership(context.Background(), email)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.people.RecordDirectoryActive(context.Background(), email, id, read.ReadAt); err != nil {
		r.t.Fatal(err)
	}
	r.age(email, age)
}

// held records a Held outcome for email after its Active one, as a suspension does.
func (r *directoryStandingRig) held(email string) {
	r.t.Helper()
	if err := r.people.RecordDirectoryHeld(context.Background(), email); err != nil {
		r.t.Fatal(err)
	}
}

// age dates every Directory column of email, and its hold, age back, keeping their order.
func (r *directoryStandingRig) age(email string, age time.Duration) {
	r.t.Helper()
	if _, err := r.database.Pool.Exec(context.Background(), `
		update people set directory_confirmed_at = directory_confirmed_at - make_interval(secs => $2),
			directory_checked_at = directory_checked_at - make_interval(secs => $2),
			not_held_until = not_held_until - make_interval(secs => $2),
			held_since = held_since - make_interval(secs => $2)
		where email = $1`, email, age.Seconds()); err != nil {
		r.t.Fatalf("date %s's Directory columns %s back: %v", email, age, err)
	}
}

// directoryRow is what people holds of email's Directory checks.
type directoryRow struct {
	confirmedAt, checkedAt, notHeldUntil, heldSince *time.Time
	userID                                          *string
}

func (r *directoryStandingRig) row(email string) directoryRow {
	r.t.Helper()
	var row directoryRow
	if err := r.database.Pool.QueryRow(context.Background(), `
		select directory_confirmed_at, directory_checked_at, not_held_until, held_since, directory_user_id
		from people where email = $1`, email).Scan(&row.confirmedAt, &row.checkedAt, &row.notHeldUntil, &row.heldSince, &row.userID); err != nil {
		r.t.Fatalf("read %s's Directory columns: %v", email, err)
	}
	return row
}

// standing is the rig's Standing of email, failing the test on an error.
func (r *directoryStandingRig) standing(email string) Standing {
	r.t.Helper()
	answer, err := r.membership.Standing(context.Background(), email)
	if err != nil {
		r.t.Fatalf("Standing(%s): %v", email, err)
	}
	return answer
}

// reads is how many users.get requests reached the fake Directory.
func (r *directoryStandingRig) reads() int { return len(r.google.Reads()) }

// sameTime reports whether two stored times are one instant, both absent included.
func sameTime(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

// stamp is a stored time as a test failure names it.
func stamp(at *time.Time) string {
	if at == nil {
		return "null"
	}
	return at.Format(time.RFC3339Nano)
}

// adaActive makes the fake Directory answer ada's own account, active.
func adaActive(g *directorytest.Google) {
	g.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
}

// TestIAPStandingFollowsTheDirectory: under IAP identity Standing answers from the Workspace
// Directory and never refreshes. A login with no people row is unavailable and asks the Directory
// nothing. A recorded hold is held, whatever its deadline says, and asks nothing. An Active answer
// younger than an hour is not held with its deadline and asks nothing. An older one asks once, and
// an Active outcome answers not held with the deadline it fixed, 24 hours after it. Concurrent calls
// share one check. A check that has not ended answers by the deadline within the 2-second wait,
// and goes on to write its outcome. After a Failed check, Standing asks nothing for a minute.
func TestIAPStandingFollowsTheDirectory(t *testing.T) {
	t.Run("no row", func(t *testing.T) {
		r := newDirectoryStandingRig(t)
		adaActive(r.google)
		answer := r.standing(directoryAda)
		if !answer.Unavailable() || answer.Held || !answer.NotHeldUntil.Equal(answer.AsOf) || r.reads() != 0 {
			t.Fatalf("Standing of a login with no row = %+v after %d Directory reads; want unavailable (NotHeldUntil = AsOf) and no read", answer, r.reads())
		}
	})
	t.Run("a recorded hold", func(t *testing.T) {
		r := newDirectoryStandingRig(t)
		adaActive(r.google)
		r.record(directoryAda)
		r.active(directoryAda, "u-ada", 0)
		r.held(directoryAda)
		r.age(directoryAda, 30*time.Hour)
		if answer := r.standing(directoryAda); !answer.Held || !answer.NotHeldUntil.IsZero() || r.reads() != 0 {
			t.Fatalf("Standing of a recorded hold past its deadline = %+v after %d reads; want held, no deadline, no read", answer, r.reads())
		}
	})
	t.Run("a fresh Active answer", func(t *testing.T) {
		r := newDirectoryStandingRig(t)
		adaActive(r.google)
		r.record(directoryAda)
		r.active(directoryAda, "u-ada", 50*time.Minute)
		stored := r.row(directoryAda).notHeldUntil
		answer := r.standing(directoryAda)
		if answer.Held || answer.Unavailable() || !answer.NotHeldUntil.Equal(*stored) || r.reads() != 0 {
			t.Fatalf("Standing of an Active answer 50 minutes old = %+v after %d reads; want not held until the stored %s, and no read", answer, r.reads(), stamp(stored))
		}
	})
	t.Run("an older Active answer", func(t *testing.T) {
		r := newDirectoryStandingRig(t)
		adaActive(r.google)
		r.record(directoryAda)
		r.active(directoryAda, "u-ada", 2*time.Hour)
		before := r.row(directoryAda)
		// The signer holds the check until every call has started or joined one and stopped
		// waiting, so the calls overlap whatever the machine's load: one check reads the Directory
		// once, and a check of each call's own would read it five times once released.
		release := r.google.HoldSigner()
		var wg sync.WaitGroup
		answers := make([]Standing, 5)
		for i := range answers {
			wg.Go(func() { answers[i] = r.standing(directoryAda) })
		}
		wg.Wait()
		for _, answer := range answers {
			if answer.Held || !answer.NotHeldUntil.Equal(*before.notHeldUntil) {
				t.Fatalf("Standing while the check is held = %+v; want not held until the old deadline %s", answer, stamp(before.notHeldUntil))
			}
		}
		release()
		waitFor(t, "the shared check's Active outcome", func() bool {
			return r.row(directoryAda).confirmedAt.After(*before.confirmedAt)
		})
		time.Sleep(time.Second)
		after := r.row(directoryAda)
		if r.reads() != 1 {
			t.Fatalf("five concurrent Standings of an Active answer two hours old read the Directory %d times; want one shared check", r.reads())
		}
		if answer := r.standing(directoryAda); answer.Held || !answer.NotHeldUntil.Equal(*after.notHeldUntil) || !after.notHeldUntil.Equal(after.confirmedAt.Add(24*time.Hour)) {
			t.Fatalf("Standing after the check = %+v; want not held until the new deadline %s, 24 hours after the confirmation %s", answer, stamp(after.notHeldUntil), stamp(after.confirmedAt))
		}
	})
	t.Run("a check longer than the wait", func(t *testing.T) {
		r := newDirectoryStandingRig(t)
		r.record(directoryAda)
		r.active(directoryAda, "u-ada", 2*time.Hour)
		before := r.row(directoryAda)
		release := r.google.HoldSigner()
		started := time.Now()
		answer := r.standing(directoryAda)
		if took := time.Since(started); took > standingWait+time.Second || answer.Held || !answer.NotHeldUntil.Equal(*before.notHeldUntil) {
			t.Fatalf("Standing while the check is held = %+v after %s; want not held until the old deadline %s within the %s wait", answer, took, stamp(before.notHeldUntil), standingWait)
		}
		if again := r.standing(directoryAda); r.reads() != 0 || again.Held {
			t.Fatalf("a second Standing while the held check runs = %+v; want the deadline's answer at once, and no read", again)
		}
		adaActive(r.google)
		release()
		waitFor(t, "the held check's Active outcome", func() bool {
			return r.row(directoryAda).confirmedAt.After(*before.confirmedAt)
		})
	})
	t.Run("a minute after a Failed check", func(t *testing.T) {
		r := newDirectoryStandingRig(t)
		directoryRefusal(http.StatusInternalServerError)(r.google)
		r.record(directoryAda)
		r.active(directoryAda, "u-ada", 2*time.Hour)
		if first := r.standing(directoryAda); first.Held || first.Unavailable() || r.reads() != 1 {
			t.Fatalf("Standing over a failing Directory = %+v after %d reads; want not held before the deadline, after one read", first, r.reads())
		}
		adaActive(r.google)
		if second := r.standing(directoryAda); second.Held || r.reads() != 1 {
			t.Fatalf("Standing a moment after a Failed check = %+v after %d reads; want the deadline's answer and no new read", second, r.reads())
		}
	})
}

// TestIAPDirectoryFailureChangesNothing (c-iap-directory): each Failed kind of a Directory check -
// a reply later than the check's 15 seconds, 429, 500, 401, 403, a body that does not decode, an
// answer with no suspended, a signJwt refusal and a token-exchange refusal - run by the membership
// pass, leaves directory_confirmed_at, not_held_until and the hold as they were, and moves
// directory_checked_at. A held person whose deadline passes during the failures stays held.
//
// Control: let a Failed outcome set directory_confirmed_at or clear the hold (record a Failed
// outcome as an Active one); the test fails naming the kind.
func TestIAPDirectoryFailureChangesNothing(t *testing.T) {
	stalled := directorytest.Answer{Stall: true}
	kinds := []struct {
		name string
		fail func(*directorytest.Google)
	}{
		{"a reply later than the check's 15 seconds", func(g *directorytest.Google) { g.AnswerUser(directoryAda, stalled) }},
		{"429", directoryRefusal(http.StatusTooManyRequests)},
		{"500", directoryRefusal(http.StatusInternalServerError)},
		{"401", directoryRefusal(http.StatusUnauthorized)},
		{"403", directoryRefusal(http.StatusForbidden)},
		{"a body that does not decode", func(g *directorytest.Google) {
			g.AnswerUser(directoryAda, directorytest.Answer{Status: http.StatusOK, Body: "<html>" + directorytest.Planted + "</html>"})
		}},
		{"an answer with no suspended", func(g *directorytest.Google) {
			g.AnswerUser(directoryAda, directorytest.Answer{Status: http.StatusOK, Body: `{"id":"u-ada","primaryEmail":"ada@example.com"}`})
		}},
		{"a signJwt refusal", func(g *directorytest.Google) {
			g.AnswerSigner(&directorytest.Answer{Status: http.StatusForbidden, Body: `{"error":{"code":403,"message":"` + directorytest.Planted + `"}}`})
		}},
		{"a token-exchange refusal", func(g *directorytest.Google) {
			g.AnswerScope(directorytest.DirectoryScope, &directorytest.Answer{Status: http.StatusBadRequest, Body: `{"error":"invalid_grant"}`})
		}},
	}
	for _, held := range []bool{false, true} {
		name := "a confirmed person"
		if held {
			name = "a held person"
		}
		t.Run(name, func(t *testing.T) {
			r := newDirectoryStandingRig(t)
			r.record(directoryAda)
			r.active(directoryAda, "u-ada", 0)
			if held {
				r.held(directoryAda)
			}
			for _, kind := range kinds {
				// A process of its own, holding no minted token, so the signer and the token
				// exchange are asked, and the row due again.
				r.newProcess()
				r.google.AnswerSigner(nil)
				r.google.AnswerScope(directorytest.DirectoryScope, nil)
				adaActive(r.google)
				kind.fail(r.google)
				r.age(directoryAda, 2*time.Hour)
				before := r.row(directoryAda)
				var counted []MembershipPassEvent
				if err := r.pass(func(e MembershipPassEvent) { counted = append(counted, e) }).Run(context.Background()); err != nil {
					t.Fatalf("%s: the pass: %v", kind.name, err)
				}
				after := r.row(directoryAda)
				if len(counted) == 0 || !counted[0].Claimed || counted[0].Email != directoryAda {
					t.Fatalf("%s: the pass claimed %+v; want ada's row first", kind.name, counted)
				}
				if !sameTime(after.confirmedAt, before.confirmedAt) || !sameTime(after.notHeldUntil, before.notHeldUntil) || !sameTime(after.heldSince, before.heldSince) {
					t.Fatalf("%s: confirmed %s -> %s, deadline %s -> %s, held since %s -> %s; want each as it was",
						kind.name, stamp(before.confirmedAt), stamp(after.confirmedAt), stamp(before.notHeldUntil), stamp(after.notHeldUntil), stamp(before.heldSince), stamp(after.heldSince))
				}
				if after.checkedAt == nil || !after.checkedAt.After(*before.checkedAt) {
					t.Fatalf("%s: directory_checked_at %s -> %s; want it moved", kind.name, stamp(before.checkedAt), stamp(after.checkedAt))
				}
			}
			// The failures above aged the row 18 hours; 7 more put it past its deadline.
			r.age(directoryAda, 7*time.Hour)
			r.newProcess()
			if answer := r.standing(directoryAda); held && !answer.Held {
				t.Fatalf("a held person whose deadline passed during Directory failures = %+v; want held, never unavailable", answer)
			} else if !held && !answer.Unavailable() {
				t.Fatalf("a confirmed person whose deadline passed during Directory failures = %+v; want unavailable", answer)
			}
		})
	}
}

// TestIAPDirectoryDeadlineIncludesTheWrite (c-iap-directory): a check and the write of its outcome
// share one 15-second deadline. While a transaction holds ada's row locked past it, an Active
// answer's write cannot commit: the check ends Failed at the deadline, the lock's release finds no
// write waiting, and the row holds no confirmation, no deadline and no stored id. Standing does not
// wait on it past its 2 seconds.
//
// Control: leave the write out of the 15-second deadline (record the outcome on a context without
// it); the write commits once the lock is released, and the test fails naming the confirmation.
func TestIAPDirectoryDeadlineIncludesTheWrite(t *testing.T) {
	r := newDirectoryStandingRig(t)
	adaActive(r.google)
	r.record(directoryAda)
	ctx := context.Background()
	lock, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `select 1 from people where email = $1 for update`, directoryAda); err != nil {
		t.Fatal(err)
	}
	held := DirectoryFlight + 3*time.Second
	go func() {
		time.Sleep(held)
		_ = lock.Rollback(ctx)
	}()
	started := time.Now()
	waited := make(chan Standing, 1)
	go func() {
		answer, _ := r.membership.Standing(ctx, directoryAda)
		waited <- answer
	}()
	select {
	case answer := <-waited:
		if took := time.Since(started); took > standingWait+time.Second || !answer.Unavailable() {
			t.Fatalf("Standing while the write waits on the lock = %+v after %s; want unavailable within the %s wait", answer, took, standingWait)
		}
	case <-time.After(standingWait + 5*time.Second):
		t.Fatalf("Standing waited on the check past %s", standingWait+5*time.Second)
	}
	outcome, err := r.membership.ConfirmDirectory(ctx, directoryAda)
	took := time.Since(started)
	if outcome != RefreshFailed || err == nil || took > DirectoryFlight+2*time.Second {
		t.Fatalf("the check whose write waits on the lock = %v, %v after %s; want Failed by the %s deadline", outcome, err, took, DirectoryFlight)
	}
	time.Sleep(time.Until(started.Add(held + time.Second)))
	if row := r.row(directoryAda); row.confirmedAt != nil || row.notHeldUntil != nil || row.userID != nil {
		t.Fatalf("after the lock's release ada's row holds confirmation %s, deadline %s, id %v; want none: the Active write missed its deadline",
			stamp(row.confirmedAt), stamp(row.notHeldUntil), row.userID)
	}
}

// TestIAPNeverConfirmedRowIsUnavailable (c-iap-directory): a row the Directory never answered
// Active for, whose checks fail, is unavailable: not held, with NotHeldUntil equal to AsOf. A
// first-ever check that is Held records a hold on that row, and it answers held, not unavailable.
func TestIAPNeverConfirmedRowIsUnavailable(t *testing.T) {
	r := newDirectoryStandingRig(t)
	directoryRefusal(http.StatusInternalServerError)(r.google)
	r.record(directoryAda)
	if answer := r.standing(directoryAda); !answer.Unavailable() || r.reads() != 1 {
		t.Fatalf("a never-confirmed row whose check fails = %+v after %d reads; want unavailable after one check", answer, r.reads())
	}
	r.newProcess()
	r.google.AnswerUser(directoryAda, directorytest.Answer{Status: http.StatusNotFound})
	answer := r.standing(directoryAda)
	if !answer.Held || answer.Unavailable() || r.row(directoryAda).heldSince == nil {
		t.Fatalf("a never-confirmed row whose first check is Held = %+v, held since %s; want held, with the hold recorded", answer, stamp(r.row(directoryAda).heldSince))
	}
}

// TestIAPMembershipPassChecksHeldIdleAndTokenlessRows (c-iap-directory): under IAP the membership
// pass checks every full member's row that is due, a held row, an idle row (an Active answer two
// hours old whose person made no request, here one whose row still holds a refresh token from an
// earlier cookie sign-in) and a row with no refresh token that was never checked, and runs no
// sign-out step. An Active outcome releases the held row, with no request from the person. A
// restricted member's row is not claimed. The pass counts each outcome, K9's confirmed for Active.
//
// Control: skip held rows in the pass; the held row is not checked, and the test fails naming it.
func TestIAPMembershipPassChecksHeldIdleAndTokenlessRows(t *testing.T) {
	r := newDirectoryStandingRig(t)
	ctx := context.Background()
	const (
		heldEmail      = directoryAda
		idleEmail      = directoryBob
		tokenlessEmail = "carol@example.com"
		restricted     = "dana@outside.example"
	)
	for _, email := range []string{heldEmail, idleEmail, tokenlessEmail} {
		r.google.SetUser(email, directorytest.User{ID: "u-" + email, PrimaryEmail: email})
	}
	r.record(heldEmail)
	r.active(heldEmail, "u-"+heldEmail, 0)
	r.held(heldEmail)
	r.age(heldEmail, 2*time.Hour)
	if err := r.people.SignInSubject(ctx, idleEmail, "g-bob", "bob-refresh", "example.com", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	r.active(idleEmail, "u-"+idleEmail, 2*time.Hour)
	r.record(tokenlessEmail)
	if err := r.people.SignInSubject(ctx, restricted, "g-dana", "dana-refresh", "", model.MemberRestricted); err != nil {
		t.Fatal(err)
	}
	if !r.standing(heldEmail).Held {
		t.Fatal("ada is not held before the pass")
	}

	var events []MembershipPassEvent
	if err := r.pass(func(e MembershipPassEvent) { events = append(events, e) }).Run(ctx); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	claimed := map[string]bool{}
	for _, e := range events {
		if e.Step != 2 {
			t.Errorf("the pass under IAP made a claim attempt of step %d (%+v); want the Directory step alone", e.Step, e)
		}
		if e.Claimed {
			claimed[e.Email] = true
		}
	}
	read := map[string]bool{}
	for _, got := range r.google.Reads() {
		read[got.UserKey] = true
	}
	for _, email := range []string{heldEmail, idleEmail, tokenlessEmail} {
		if !claimed[email] || !read[email] {
			t.Errorf("the pass claimed %s %t and read it in the Directory %t; want both", email, claimed[email], read[email])
		}
	}
	if claimed[restricted] || read[restricted] {
		t.Errorf("the pass claimed the restricted member %s %t, read %t; want neither", restricted, claimed[restricted], read[restricted])
	}
	if answer := r.standing(heldEmail); answer.Held || answer.Unavailable() || r.row(heldEmail).heldSince != nil {
		t.Errorf("ada after the pass's Active outcome = %+v, held since %s; want released, with no request from her", answer, stamp(r.row(heldEmail).heldSince))
	}
	for _, email := range []string{idleEmail, tokenlessEmail} {
		if row := r.row(email); row.confirmedAt == nil || row.notHeldUntil == nil || row.userID == nil || *row.userID != "u-"+email {
			t.Errorf("%s after the pass: confirmed %s, deadline %s, id %v; want an Active outcome recorded", email, stamp(row.confirmedAt), stamp(row.notHeldUntil), row.userID)
		}
	}
	events = nil
	if err := r.pass(func(e MembershipPassEvent) { events = append(events, e) }).Run(ctx); err != nil || len(events) != 1 || events[0].Claimed {
		t.Errorf("a second pass at once claimed %+v, %v; want none due", events, err)
	}
}

// personAccessStandIn is the secrets broker's GET /v1/person-access (K8) for the rebind tests: it
// answers each ask with answer, and counts the asks.
type personAccessStandIn struct {
	mu     sync.Mutex
	asks   int
	answer func(w http.ResponseWriter, r *http.Request)
}

// serve starts the stand-in, stopped when t ends, and answers the outliving question over database
// and it, under a token file of the test's own, as Dispatch's broker client asks it.
func (s *personAccessStandIn) serve(t *testing.T, database *store.Store) cipher.OutlivingAccess {
	t.Helper()
	gone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/person-access" || r.URL.Query().Get("person") != directoryAda {
			t.Errorf("the broker was asked %s %s, want only the person-access route for %s", r.Method, r.URL.RequestURI(), directoryAda)
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.asks++
		answer := s.answer
		s.mu.Unlock()
		if answer == nil {
			t.Error("the broker was asked, and the test gave it no answer")
			http.Error(w, "no answer", http.StatusTeapot)
			return
		}
		select {
		case <-gone:
		default:
			answer(w, r)
		}
	}))
	// Cleanups run last first: a stand-in still waiting lets go before the server waits for it.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(gone) })
	tokenFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(tokenFile, []byte("account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cipher.OutlivingAccess{Pool: database.Pool, Broker: cipher.New(server.URL, tokenFile)}
}

// asked is how many asks the stand-in counted.
func (s *personAccessStandIn) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asks
}

// counts answers the person-access route with the four counts, as the broker does.
func counts(machineLogins, signIns, exchangeEntries, podOperatorMappings int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"person":"` + directoryAda + `","machine_logins":` + strconv.Itoa(machineLogins) +
			`,"signins":` + strconv.Itoa(signIns) + `,"exchange_entries":` + strconv.Itoa(exchangeEntries) +
			`,"pod_operator_mappings":` + strconv.Itoa(podOperatorMappings) + `}`))
	}
}

// storePersonalToken stores one personal token of owner, revoked when revoked is set.
func storePersonalToken(t *testing.T, database *store.Store, owner, name string, revoked bool) {
	t.Helper()
	hash := sha256.Sum256([]byte(name))
	if _, err := database.Pool.Exec(context.Background(), `
		insert into agent_tokens (owner, name, token_hash, prefix, revoked_at)
		values ($1, $2, $3, 'test', case when $4::boolean then now() end)`, owner, name, hash[:], revoked); err != nil {
		t.Fatalf("store the personal token %q of %q: %v", name, owner, err)
	}
}

// TestARecreatedAddressConfirmsOnlyOnceItsEarlierAccessIsGone (c-iap-rebind): each case starts
// from ada's row, which stores her Directory user id u-ada, and the Directory answers her address
// itself (its primaryEmail) with suspended false and another account's id, u-ada-2, as it does for
// an address deleted and created again. Dispatch asks the question a sign-out asks
// (cipher.OutlivingAccess.Holds), within the check's 15 seconds:
//
//   - an unrevoked personal token of the address, nothing at the broker: Held, the stored id kept,
//     and the broker not asked, since the token answers first;
//   - one machine login, one sign-in on a machine, one exchange entry naming the address, one
//     account BROKER_POD_OPERATORS maps to it, each alone at the broker: Held, the stored id kept,
//     the broker asked once;
//   - the broker answering 500, or not answering within its client's 5 seconds: Held, the stored
//     id kept;
//   - the Directory's answer delayed until less than five of the 15 seconds remain, and the broker
//     waiting past the 15: Failed at the 15-second mark, not after the question's own 5 seconds,
//     with the stored id, the confirmation, not_held_until and the hold unchanged;
//   - a revoked personal token, the broker answering all four counts zero, on a held row: the
//     membership pass, with no request from the person, confirms the new account, stores u-ada-2,
//     fixes a new not_held_until and releases the hold; the next answer, carrying the stored
//     u-ada-2, is Active and asks the broker nothing;
//   - an answer whose primaryEmail is another address (ada's address made an alias of bob's
//     account), all counts zero: Held, the stored id kept, the broker not asked.
//
// Controls, each run alone: confirming another id without asking fails the personal-token case;
// reading a failed question as no access fails the broker-failing case; confirming an alias answer
// as a recreated address fails the alias case; keeping the earlier id after a confirmation fails
// the stored-id assertion; giving the question a context of its own fails the deadline case,
// which then returns later than the 15-second mark.
func TestARecreatedAddressConfirmsOnlyOnceItsEarlierAccessIsGone(t *testing.T) {
	ctx := context.Background()
	// start is ada's row as each case starts: confirmed by u-ada two hours ago, so due a check,
	// and the Directory answering her address with primaryEmail and id as given. The stand-in
	// answers the broker's part of the question.
	start := func(t *testing.T, primaryEmail string, broker func(http.ResponseWriter, *http.Request)) (*directoryStandingRig, *personAccessStandIn) {
		t.Helper()
		r := newDirectoryStandingRig(t)
		standIn := &personAccessStandIn{answer: broker}
		r.outliving = standIn.serve(t, r.database)
		r.newProcess()
		r.record(directoryAda)
		r.active(directoryAda, "u-ada", 2*time.Hour)
		r.google.SetUser(directoryAda, directorytest.User{ID: "u-ada-2", PrimaryEmail: primaryEmail})
		return r, standIn
	}
	// held runs one check and requires it Held, with the stored id u-ada kept and a hold recorded,
	// and the broker asked asks times.
	held := func(t *testing.T, r *directoryStandingRig, standIn *personAccessStandIn, asks int, what string) {
		t.Helper()
		before := r.row(directoryAda)
		outcome, err := r.membership.ConfirmDirectory(ctx, directoryAda)
		after := r.row(directoryAda)
		if outcome != RefreshHeld || err != nil {
			t.Fatalf("%s: the check = %v, %v; want Held", what, outcome, err)
		}
		if after.userID == nil || *after.userID != "u-ada" {
			t.Fatalf("%s: the row stores the Directory user id %s after the check; want u-ada kept", what, storedID(after.userID))
		}
		if after.heldSince == nil || !sameTime(after.confirmedAt, before.confirmedAt) || !sameTime(after.notHeldUntil, before.notHeldUntil) {
			t.Fatalf("%s: held since %s, confirmed %s -> %s, deadline %s -> %s; want a hold recorded and the confirmation and deadline as they were",
				what, stamp(after.heldSince), stamp(before.confirmedAt), stamp(after.confirmedAt), stamp(before.notHeldUntil), stamp(after.notHeldUntil))
		}
		if got := standIn.asked(); got != asks {
			t.Fatalf("%s: the broker was asked %d times; want %d", what, got, asks)
		}
		if !r.standing(directoryAda).Held {
			t.Fatalf("%s: ada's Standing after the check is not held", what)
		}
	}

	t.Run("an unrevoked personal token", func(t *testing.T) {
		r, standIn := start(t, directoryAda, counts(0, 0, 0, 0))
		storePersonalToken(t, r.database, "Ada@Example.com", "ada's agent", false)
		held(t, r, standIn, 0, "an unrevoked personal token, nothing at the broker")
	})
	for _, at := range []struct {
		name   string
		answer func(http.ResponseWriter, *http.Request)
	}{
		{"one machine login", counts(1, 0, 0, 0)},
		{"one sign-in on a machine", counts(0, 1, 0, 0)},
		{"one exchange entry", counts(0, 0, 1, 0)},
		{"one account BROKER_POD_OPERATORS maps to the address", counts(0, 0, 0, 1)},
		{"the broker answering 500", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"code":"INTERNAL","error":"boom"}`, http.StatusInternalServerError)
		}},
		{"the broker not answering within its 5 seconds", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }},
	} {
		t.Run(at.name, func(t *testing.T) {
			r, standIn := start(t, directoryAda, at.answer)
			held(t, r, standIn, 1, at.name+" at the broker")
		})
	}

	t.Run("the question outlasting the check's 15 seconds", func(t *testing.T) {
		r, standIn := start(t, directoryAda, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		before := r.row(directoryAda)
		release := r.google.HoldSigner()
		type result struct {
			outcome RefreshOutcome
			err     error
			took    time.Duration
		}
		done := make(chan result, 1)
		began := time.Now()
		go func() {
			outcome, err := r.membership.ConfirmDirectory(ctx, directoryAda)
			done <- result{outcome, err, time.Since(began)}
		}()
		waitFor(t, "the check's signJwt", func() bool { return len(r.google.Signs()) > 0 })
		// The Directory answers once less than five of the check's 15 seconds remain.
		time.Sleep(time.Until(began.Add(DirectoryFlight - 3*time.Second)))
		release()
		var got result
		select {
		case got = <-done:
		case <-time.After(DirectoryFlight + 15*time.Second):
			t.Fatalf("the check had not returned %s after it began", DirectoryFlight+15*time.Second)
		}
		if got.outcome != RefreshFailed || got.err == nil {
			t.Fatalf("the check whose question outlasted its 15 seconds = %v, %v after %s; want Failed", got.outcome, got.err, got.took)
		}
		if got.took < DirectoryFlight-time.Second || got.took > DirectoryFlight+time.Second {
			t.Fatalf("the check returned Failed after %s; want it at the %s mark, the question bounded by the check's deadline, not by its own 5 seconds", got.took, DirectoryFlight)
		}
		if standIn.asked() != 1 {
			t.Fatalf("the broker was asked %d times; want once, after the delayed Directory answer", standIn.asked())
		}
		after := r.row(directoryAda)
		if after.userID == nil || *after.userID != "u-ada" || !sameTime(after.confirmedAt, before.confirmedAt) ||
			!sameTime(after.notHeldUntil, before.notHeldUntil) || !sameTime(after.heldSince, before.heldSince) {
			t.Fatalf("after the Failed check the row stores id %v, confirmed %s, deadline %s, held since %s; want u-ada, %s, %s, %s as before",
				storedID(after.userID), stamp(after.confirmedAt), stamp(after.notHeldUntil), stamp(after.heldSince),
				stamp(before.confirmedAt), stamp(before.notHeldUntil), stamp(before.heldSince))
		}
	})

	t.Run("no access left, a held row", func(t *testing.T) {
		r, standIn := start(t, directoryAda, counts(0, 0, 0, 0))
		storePersonalToken(t, r.database, directoryAda, "ada's revoked agent", true)
		r.held(directoryAda)
		r.age(directoryAda, 2*time.Hour)
		before := r.row(directoryAda)
		if !r.standing(directoryAda).Held {
			t.Fatal("ada is not held before the pass")
		}
		if err := r.pass(nil).Run(ctx); err != nil {
			t.Fatalf("the membership pass: %v", err)
		}
		after := r.row(directoryAda)
		if after.userID == nil || *after.userID != "u-ada-2" {
			t.Fatalf("after the pass the row stores the Directory user id %s; want the new account's u-ada-2 in place of u-ada", storedID(after.userID))
		}
		if after.confirmedAt == nil || !after.confirmedAt.After(*before.confirmedAt) || !sameTime(after.notHeldUntil, new(after.confirmedAt.Add(24*time.Hour))) || after.heldSince != nil {
			t.Fatalf("after the pass ada's row reads confirmed %s, deadline %s, held since %s; want a new confirmation, its deadline 24 hours on, and the hold released",
				stamp(after.confirmedAt), stamp(after.notHeldUntil), stamp(after.heldSince))
		}
		if answer := r.standing(directoryAda); answer.Held || answer.Unavailable() {
			t.Fatalf("ada's Standing after the pass = %+v; want served, with no request from her", answer)
		}
		if standIn.asked() != 1 {
			t.Fatalf("the broker was asked %d times; want once", standIn.asked())
		}

		// The next answer carries the stored id: Active, and the broker is asked nothing.
		r.age(directoryAda, 2*time.Hour)
		if outcome, err := r.membership.ConfirmDirectory(ctx, directoryAda); outcome != RefreshConfirmed || err != nil {
			t.Fatalf("the next check, answered with the stored u-ada-2 = %v, %v; want Active", outcome, err)
		}
		if standIn.asked() != 1 {
			t.Fatalf("an answer with the stored id asked the broker; it was asked %d times in all, want once", standIn.asked())
		}
	})

	t.Run("an alias of a colleague's account", func(t *testing.T) {
		r, standIn := start(t, directoryBob, counts(0, 0, 0, 0))
		held(t, r, standIn, 0, "an answer whose primaryEmail is another address, all counts zero")
	})
}

// storedID is a stored Directory user id as a failure names it: null, or the id quoted.
func storedID(id *string) string {
	if id == nil {
		return "null"
	}
	return strconv.Quote(*id)
}
