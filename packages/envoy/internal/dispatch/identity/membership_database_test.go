package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// TestMain gives the package's Postgres-backed tests a database each (storetest.Main).
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// laggingDatabaseClock opens a pool on database whose now() runs lag behind this process's clock,
// as a database server's clock may run behind a server process's: a schema of the test's own holds
// a now() answering pg_catalog.now() - lag, and the pool's search_path names that schema ahead of
// pg_catalog, so every unqualified now() a statement on the pool runs reads the lagging clock.
func laggingDatabaseClock(t *testing.T, database *store.Store, lag time.Duration) *store.Pool {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("lagging_clock_%d", time.Now().UnixNano())
	if _, err := database.Pool.Exec(ctx, fmt.Sprintf(`
		create schema %[1]s;
		create function %[1]s.now() returns timestamptz language sql stable as $$ select pg_catalog.now() - interval '%[2]d microseconds' $$;
	`, schema, lag.Microseconds())); err != nil {
		t.Fatalf("make the database's clock lag: %v", err)
	}
	config := database.Pool.Config()
	config.ConnConfig.RuntimeParams["search_path"] = schema + ", public, pg_catalog"
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool on the lagging clock: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return pool
}

// A confirmation's age is the database's own, and Membership reads no clock of its own: with this
// process's clock two hours ahead of the database's, a confirmation the database dates a minute
// short of MembershipMaxAge still stands and asks the issuer nothing, and one it dates a minute
// past it lapses and is refreshed. A process clock in the reckoning would date the first one two
// hours older than the database does and refresh it too.
func TestMembershipAgesAConfirmationByTheDatabasesClock(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := laggingDatabaseClock(t, database, 2*time.Hour)
	people := store.NewPgPeopleStore(pool, testSigningKey, nil)
	if err := people.SignIn(ctx, memberEmail, "refresh-1"); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	var refreshes atomic.Int32
	membership := &Membership{
		People: people,
		Rule:   GroupRule{Group: memberGroup},
		SignIn: refresherFunc(func(context.Context, string) (oidc.Session, error) {
			refreshes.Add(1)
			return oidc.Session{}, fmt.Errorf("oidc: refresh: %w: the test answers none", oidc.ErrIssuerUnavailable)
		}),
	}
	confirmedAgo := func(age time.Duration) {
		t.Helper()
		if _, err := pool.Exec(ctx, `update people set confirmed_at = now() - make_interval(secs => $2) where email = $1`, memberEmail, age.Seconds()); err != nil {
			t.Fatalf("date the confirmation %s back: %v", age, err)
		}
	}

	if err := membership.Confirm(ctx, memberEmail); err != nil || refreshes.Load() != 0 {
		t.Fatalf("Confirm right after the sign-in: err=%v refreshes=%d, want nil and none", err, refreshes.Load())
	}
	confirmedAgo(MembershipMaxAge - time.Minute)
	if err := membership.Confirm(ctx, memberEmail); err != nil || refreshes.Load() != 0 {
		t.Fatalf("Confirm of a confirmation the database dates %s old: err=%v refreshes=%d, want nil and none", MembershipMaxAge-time.Minute, err, refreshes.Load())
	}
	confirmedAgo(MembershipMaxAge + time.Minute)
	if err := membership.Confirm(ctx, memberEmail); !errors.Is(err, ErrSignInUnavailable) || refreshes.Load() != 1 {
		t.Fatalf("Confirm of a confirmation the database dates %s old: err=%v refreshes=%d, want ErrSignInUnavailable after one refresh", MembershipMaxAge+time.Minute, err, refreshes.Load())
	}
}

// Under a sign-in pool's group, a refresh that read the row before the person signed in again ends
// or confirms nothing of that newer sign-in. The person's confirmation has lapsed, the refresh of
// refresh-1 begins, and before the pool answers it the person signs in again through the pool,
// which stores refresh-2. The pool's refusal of refresh-1 (invalid_grant) leaves the newer
// sign-in's sessions and refresh-2 as they are, and its success leaves refresh-2 rather than
// writing the stale refresh's own token. A refresh that read the row as it stands ends, or confirms,
// as before.
func TestGroupMembershipEndsAndConfirmsOnlyTheSignInItRead(t *testing.T) {
	claims := oidc.Claims{Username: "Google_" + memberEmail, Providers: []string{"Google"}, Groups: []string{memberGroup}}
	for _, test := range []struct {
		name   string
		answer func(token string) (oidc.Session, error)
		// after is the refresh token the row holds once a refresh that read the row as it stands
		// answered, and ended whether that refresh ends the sessions.
		after string
		ended bool
	}{
		{"the pool refuses the stale refresh", func(string) (oidc.Session, error) {
			return oidc.Session{}, fmt.Errorf("oidc: refresh: %w: invalid_grant", oidc.ErrGrantRefused)
		}, "", true},
		{"the pool confirms the stale refresh", func(token string) (oidc.Session, error) {
			return oidc.Session{Claims: claims, RefreshToken: token + "-next"}, nil
		}, "refresh-2-next", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
			sessions := store.NewPgSessionStore(database.Pool)
			if err := people.SignIn(ctx, memberEmail, "refresh-1"); err != nil {
				t.Fatal(err)
			}
			lapse := func() {
				t.Helper()
				if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '2 hours' where email = $1`, memberEmail); err != nil {
					t.Fatal(err)
				}
			}
			generation := func() int64 {
				t.Helper()
				var generation int64
				if err := database.Pool.QueryRow(ctx, `select generation from user_sessions where login = $1`, memberEmail).Scan(&generation); err != nil {
					t.Fatal(err)
				}
				return generation
			}
			lapse()
			var signedIn int64
			membership := &Membership{
				People: people,
				Rule:   GroupRule{Group: memberGroup},
				SignIn: refresherFunc(func(ctx context.Context, token string) (oidc.Session, error) {
					if token == "refresh-1" {
						if err := people.SignIn(ctx, memberEmail, "refresh-2"); err != nil {
							return oidc.Session{}, err
						}
						var err error
						if signedIn, err = sessions.EnsureSession(ctx, memberEmail); err != nil {
							return oidc.Session{}, err
						}
					}
					return test.answer(token)
				}),
			}
			_ = membership.Confirm(ctx, memberEmail)
			before := generation()
			read, _, err := people.Membership(ctx, memberEmail)
			if err != nil || read.RefreshToken != "refresh-2" || before != signedIn {
				t.Fatalf("%s: after a refresh of refresh-1 answered once the person signed in again, the row holds %q (%v) and the session generation went from %d to %d; want the newer sign-in's refresh-2 and its sessions kept", test.name, read.RefreshToken, err, signedIn, before)
			}
			lapse()
			_ = membership.Confirm(ctx, memberEmail)
			read, _, err = people.Membership(ctx, memberEmail)
			if err != nil || read.RefreshToken != test.after {
				t.Fatalf("%s: the row after a refresh of the token it holds holds %q (%v), want %q", test.name, read.RefreshToken, err, test.after)
			}
			if got := generation(); (got != before) != test.ended {
				t.Fatalf("%s: the session generation went from %d to %d, want it advanced %t", test.name, before, got, test.ended)
			}
		})
	}
}
