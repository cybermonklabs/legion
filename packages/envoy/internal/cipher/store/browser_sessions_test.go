package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// pageIssuer is the provider every approval-page sign-in of these tests is made at.
const pageIssuer = "https://accounts.google.com"

// pageAccount is the Google account login signs in to the approval page with in these tests.
func pageAccount(login string) store.IssuerSubject {
	return store.IssuerSubject{Issuer: pageIssuer, Subject: "sub-" + login}
}

// signIn makes the session whose secret hashes to hash, signed in as login by pageAccount(login)
// until expires, as the approval page's callback does.
func signIn(t *testing.T, st *store.Store, hash [sha256.Size]byte, login string, expires time.Time) {
	t.Helper()
	if err := st.CreateBrowserSession(context.Background(), store.BrowserSignIn{
		Hash: hash, Login: login, Account: pageAccount(login), Expires: expires,
	}); err != nil {
		t.Fatalf("CreateBrowserSession(%s): %v", login, err)
	}
}

// A session is found by its secret's hash, with the login it was made with, live until the last
// instant before the expiry it was made with, until its logout deletes it; a lookup by the hash of
// another secret finds nothing.
func TestBrowserSessionIsFoundByItsHashUntilItsLogout(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := time.Now()
	hash := sha256.Sum256([]byte("session secret " + t.Name()))
	expires := now.Add(time.Hour).Truncate(time.Microsecond)
	signIn(t, st, hash, "ada@example.com", expires)

	for _, at := range []time.Time{now, expires.Add(-time.Microsecond)} {
		if login, err := st.BrowserSession(ctx, hash, at); err != nil || login != "ada@example.com" {
			t.Fatalf("BrowserSession at %v, before its expiry %v = %q, %v; want ada@example.com", at, expires, login, err)
		}
	}
	if _, err := st.BrowserSession(ctx, sha256.Sum256([]byte("another secret")), now); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("BrowserSession(another secret's hash) error = %v, want pgx.ErrNoRows", err)
	}

	if err := st.DeleteBrowserSession(ctx, hash); err != nil {
		t.Fatalf("DeleteBrowserSession: %v", err)
	}
	if _, err := st.BrowserSession(ctx, hash, now); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("BrowserSession after the logout: error = %v, want pgx.ErrNoRows", err)
	}
	if err := st.DeleteBrowserSession(ctx, hash); err != nil {
		t.Fatalf("DeleteBrowserSession of a session already gone: %v", err)
	}
}

// A session whose expiry has come is absent from the lookup though its row is still there, at the
// instant of its expiry as well as after it.
func TestBrowserSessionPastItsExpiryIsAbsent(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte("session secret " + t.Name()))
	expires := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	signIn(t, st, hash, "ada@example.com", expires)
	for _, at := range []time.Time{expires, expires.Add(time.Second)} {
		if login, err := st.BrowserSession(ctx, hash, at); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("BrowserSession at %v, its expiry %v = %q, %v; want pgx.ErrNoRows", at, expires, login, err)
		}
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `select count(*) from browser_sessions`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("browser_sessions rows = %d (%v), want the expired one still there", rows, err)
	}
}

// The sweep deletes every session whose expiry has come, and answers how many, and leaves a live
// one signed in.
func TestDeleteExpiredBrowserSessionsDeletesOnlyTheExpired(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	sessions := []struct {
		secret  string
		expires time.Time
	}{
		{"long gone", now.Add(-time.Hour)},
		{"expiring now", now},
		{"live", now.Add(time.Hour)},
	}
	for _, s := range sessions {
		signIn(t, st, sha256.Sum256([]byte(s.secret)), "ada@example.com", s.expires)
	}

	deleted, err := st.DeleteExpiredBrowserSessions(ctx, now)
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteExpiredBrowserSessions = %d, %v; want 2", deleted, err)
	}
	if login, err := st.BrowserSession(ctx, sha256.Sum256([]byte("live")), now); err != nil || login != "ada@example.com" {
		t.Fatalf("the live session after the sweep = %q, %v; want ada@example.com", login, err)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `select count(*) from browser_sessions`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("browser_sessions rows after the sweep = %d (%v), want only the live one", rows, err)
	}
	if deleted, err := st.DeleteExpiredBrowserSessions(ctx, now); err != nil || deleted != 0 {
		t.Fatalf("a second sweep = %d, %v; want 0", deleted, err)
	}
}

// Ending one login's sessions ends every one of them, live or expired, and leaves another
// person's alone; a login with no session is no error.
func TestDeleteBrowserSessionsForLoginEndsThatLoginsSessionsAlone(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	sessions := []struct {
		secret, login string
		expires       time.Time
	}{
		{"ada's laptop", "ada@example.com", now.Add(time.Hour)},
		{"ada's phone", "ada@example.com", now.Add(2 * time.Hour)},
		{"ada's old session", "ada@example.com", now.Add(-time.Hour)},
		{"ben's laptop", "ben@example.com", now.Add(time.Hour)},
	}
	for _, s := range sessions {
		signIn(t, st, sha256.Sum256([]byte(s.secret)), s.login, s.expires)
	}

	ended, err := st.DeleteBrowserSessionsForLogin(ctx, "ada@example.com")
	if err != nil || ended != 3 {
		t.Fatalf("DeleteBrowserSessionsForLogin(ada) = %d, %v; want 3", ended, err)
	}
	if _, err := st.BrowserSession(ctx, sha256.Sum256([]byte("ada's laptop")), now); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ada's live session after the end = %v, want pgx.ErrNoRows", err)
	}
	if login, err := st.BrowserSession(ctx, sha256.Sum256([]byte("ben's laptop")), now); err != nil || login != "ben@example.com" {
		t.Fatalf("ben's session after ada's sessions ended = %q, %v; want ben@example.com", login, err)
	}
	if ended, err := st.DeleteBrowserSessionsForLogin(ctx, "carol@example.com"); err != nil || ended != 0 {
		t.Fatalf("DeleteBrowserSessionsForLogin of a login with no session = %d, %v; want 0", ended, err)
	}
}

// A sign-in to the approval page records the account that signed in, and ends the session the
// browser held in the transaction that writes the new one. A sign-in by an account recorded as
// replaced under the address is ErrSubjectReplaced: it writes no session and leaves
// the browser's previous one alone, as every other refused sign-in does. The revoke's delete ends
// the replaced account's sessions and those that record no account, and leaves the holder's and
// another person's.
//
// Control: drop the replaced read from CreateBrowserSession, and the replaced account's sign-in
// writes a session.
func TestASignInToThePageRecordsItsAccountAndRefusesAReplacedOne(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	const ada, ben = "ada@example.com", "ben@example.com"
	expires := time.Now().Add(time.Hour)
	replaced, holder := pageAccount(ada), store.IssuerSubject{Issuer: pageIssuer, Subject: "holder-" + ada}
	hashOf := func(secret string) [sha256.Size]byte { return sha256.Sum256([]byte(secret + " " + t.Name())) }
	live := func(secret string) bool {
		t.Helper()
		_, err := st.BrowserSession(ctx, hashOf(secret), time.Now())
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("BrowserSession(%s): %v", secret, err)
		}
		return err == nil
	}

	signIn(t, st, hashOf("replaced"), ada, expires)
	var recorded store.IssuerSubject
	replacedHash, legacyHash := hashOf("replaced"), hashOf("before 0046")
	if err := st.Pool.QueryRow(ctx, `select issuer, subject from browser_sessions where secret_hash = $1`, replacedHash[:]).
		Scan(&recorded.Issuer, &recorded.Subject); err != nil || recorded != replaced {
		t.Fatalf("the session records the account %+v (%v), want %+v", recorded, err, replaced)
	}
	if _, err := st.Pool.Exec(ctx, `insert into browser_sessions (secret_hash, login, expires_at) values ($1, $2, $3)`,
		legacyHash[:], ada, expires); err != nil {
		t.Fatalf("write a session as a broker from before migration 0046 did: %v", err)
	}
	signIn(t, st, hashOf("ben"), ben, expires)
	previous := hashOf("previous")
	if err := st.CreateBrowserSession(ctx, store.BrowserSignIn{Hash: previous, Login: ada, Account: holder, Expires: expires}); err != nil {
		t.Fatalf("the holder's first sign-in: %v", err)
	}

	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := store.LockPerson(ctx, tx, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordReplacedSubjectTx(ctx, tx, ada, pageIssuer, replaced.Subject, holder.Subject, 1); err != nil {
		t.Fatal(err)
	}
	if ended, err := store.DeleteSubjectBrowserSessionsTx(ctx, tx, ada, replaced); err != nil || ended != 2 {
		t.Fatalf("DeleteSubjectBrowserSessionsTx = %d, %v; want 2: the replaced account's and the one recording none", ended, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for secret, want := range map[string]bool{"replaced": false, "before 0046": false, "ben": true, "previous": true} {
		if got := live(secret); got != want {
			t.Errorf("session %q live=%t after the revoke's delete, want %t", secret, got, want)
		}
	}

	err = st.CreateBrowserSession(ctx, store.BrowserSignIn{Hash: hashOf("again"), Login: ada, Account: replaced, Expires: expires, Previous: &previous})
	if !errors.Is(err, store.ErrSubjectReplaced) {
		t.Fatalf("a sign-in of the replaced account = %v, want ErrSubjectReplaced", err)
	}
	if live("again") || !live("previous") {
		t.Fatalf("the refused sign-in wrote a session (%t) or ended the browser's previous one (%t)", live("again"), !live("previous"))
	}
	if err := st.CreateBrowserSession(ctx, store.BrowserSignIn{Hash: hashOf("holder"), Login: ada, Account: holder, Expires: expires, Previous: &previous}); err != nil {
		t.Fatalf("the holder's sign-in: %v", err)
	}
	if !live("holder") || live("previous") {
		t.Fatalf("the holder's sign-in left its session live=%t and the browser's previous one live=%t, want true and false", live("holder"), live("previous"))
	}
}

// A decision's hold on its approval-page session (HoldBrowserSessionTx) answers nil for a live
// session of the login it names, holding that person's lock until its transaction ends, and
// ErrBrowserSessionEnded for a session at its expiry, one logged out, and a live session read as
// another login, since the lock it took is that other login's and not the session's person's.
//
// Controls: drop the login from the hold's read, and the session read as another login holds;
// drop the expiry from it, and the session at its expiry holds; take the lock out of it, and no
// transaction waits for the person's lock behind the hold.
func TestAHoldOnAPageSessionAnswersOnlyALiveOneOfItsPersonAndTakesTheirLock(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	const ada, ben = "ada@example.com", "ben@example.com"
	now := time.Now().Truncate(time.Microsecond)
	hashOf := func(secret string) [sha256.Size]byte { return sha256.Sum256([]byte(secret + " " + t.Name())) }
	signIn(t, st, hashOf("live"), ada, now.Add(time.Hour))
	expires := now.Add(time.Minute)
	signIn(t, st, hashOf("expiring"), ada, expires)
	signIn(t, st, hashOf("logged out"), ada, now.Add(time.Hour))
	if err := st.DeleteBrowserSession(ctx, hashOf("logged out")); err != nil {
		t.Fatalf("DeleteBrowserSession: %v", err)
	}

	for _, c := range []struct {
		name    string
		session store.BrowserSessionRef
		at      time.Time
	}{
		{"a session at its expiry", store.BrowserSessionRef{Hash: hashOf("expiring"), Login: ada}, expires},
		{"a session logged out", store.BrowserSessionRef{Hash: hashOf("logged out"), Login: ada}, now},
		{"a live session read as another login", store.BrowserSessionRef{Hash: hashOf("live"), Login: ben}, now},
	} {
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = store.HoldBrowserSessionTx(ctx, tx, c.session, c.at)
		_ = tx.Rollback(ctx)
		if !errors.Is(err, store.ErrBrowserSessionEnded) {
			t.Errorf("the hold on %s = %v, want ErrBrowserSessionEnded", c.name, err)
		}
	}

	holding, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holding.Rollback(ctx)
	if err := store.HoldBrowserSessionTx(ctx, holding, store.BrowserSessionRef{Hash: hashOf("live"), Login: ada}, expires); err != nil {
		t.Fatalf("the hold on a live session of its own login = %v, want nil", err)
	}
	waiting, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer waiting.Rollback(ctx)
	locked := make(chan error, 1)
	go func() { locked <- store.LockPerson(ctx, waiting, ada) }()
	storetest.AwaitLockWaiters(t, st.Pool, holding, 1)
	if err := holding.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-locked; err != nil {
		t.Fatalf("the person's lock once the hold's transaction ended: %v", err)
	}
}
