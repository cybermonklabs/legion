//go:build twobuild

package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The hold of a person whom Google refused, across a roll (c-two-builds). The previous release and
// this tree serve one database at once, against one fake issuer (withSignInIssuer). The previous
// release knows no hold: its sign-in sets signed_in_at and leaves held_since as it is, and its
// sign-out and its hourly end forget the refresh token and record nothing. This tree holds a person
// while held_since is later than signed_in_at, so a sign-in by either build releases them, and it
// answers a row with no refresh token not held. Each row reads this tree's answer where the broker
// reads it, on the person route (GET /api/v1/access/person), whose held is Standing's.
//
// Control: make the hold read held_since alone, without comparing it with signed_in_at
// (store.PgPeopleStore.Membership). The row where the previous build signs ada in fails: ada stays
// held.

// holdAccountKID is the key the issuer signs the broker's service-account tokens with, beside the
// key it signs sign-ins with.
const holdAccountKID = "account-key"

func TestTwoBuildsHoldAcrossARoll(t *testing.T) {
	builds := newTwoBuilds(t, withSignInIssuer())
	issuer := builds.issuer
	reader := holdReader{builds: builds, bearer: issuer.Mint(t, issuer.PublishKey(t, holdAccountKID),
		issuer.Claims(leaverBrokerAccount, leaverAccountAudience))}
	previous := builds.startPrevious(t, nil)
	current := builds.startCurrent(t, map[string]string{
		// The broker's service account, the one account that reads the person answer.
		"DISPATCH_OIDC_ISSUER":     issuer.URL(),
		"DISPATCH_OIDC_AUDIENCE":   leaverAccountAudience,
		"DISPATCH_OIDC_ACCOUNTS":   leaverBrokerAccount,
		"DISPATCH_ACCESS_ACCOUNTS": leaverBrokerAccount,
		// No maintenance pass of this tree's runs within the test: its membership pass would
		// refresh the rows whose confirmations a row dates back, and could end one before the
		// previous build does.
		"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL": "1h",
	})
	previous.waitForHealth(t, discoveryRetryWindow, "signin true", func(health healthBody) bool {
		return health.SignIn != nil && *health.SignIn
	})
	current.waitForHealth(t, discoveryRetryWindow, "signin and service_tokens true", func(health healthBody) bool {
		return health.SignIn != nil && *health.SignIn && health.ServiceTokens != nil && *health.ServiceTokens
	})

	t.Run("this tree holds ada, and the previous build's sign-in releases her", func(t *testing.T) {
		signInWith(t, current, issuer, rollClaims(adaEmail))
		reader.requireHeld(t, adaEmail, false, "after ada signed in on the "+builds.currentName())
		reader.dateConfirmationBack(t, adaEmail)
		issuer.RefuseRefresh(adaEmail, true)
		reader.waitHeld(t, adaEmail, "once Google refuses ada's refresh")
		held := reader.row(t, adaEmail)
		if !held.held() {
			t.Fatalf("the %s answers ada held, but her row records no hold: %+v", builds.currentName(), held)
		}

		// Google confirms ada again, and she signs in on the previous build.
		issuer.RefuseRefresh(adaEmail, false)
		signInWith(t, previous, issuer, rollClaims(adaEmail))
		released := reader.row(t, adaEmail)
		if released.heldSince == nil || !released.heldSince.Equal(*held.heldSince) || released.held() {
			t.Fatalf("after ada signed in on the %s her row = %+v, want held_since kept at %s and signed_in_at after it: that build writes signed_in_at alone",
				builds.previousName(), released, held.heldSince)
		}
		if !released.refreshToken {
			t.Fatalf("after ada signed in on the %s her row stores no refresh token: %+v", builds.previousName(), released)
		}
		reader.requireHeld(t, adaEmail, false, "after ada signed in on the "+builds.previousName()+" (ada stays held)")
	})

	t.Run("the previous build's sign-out of bob forgets his token, and this tree answers him not held", func(t *testing.T) {
		session := signInWith(t, previous, issuer, rollClaims(bobEmail))
		if row := reader.row(t, bobEmail); !row.refreshToken {
			t.Fatalf("after bob signed in on the %s his row stores no refresh token: %+v", builds.previousName(), row)
		}
		signOutOn(t, previous, session)
		if row := reader.row(t, bobEmail); row.refreshToken || row.heldSince != nil || row.signedOutAt != nil {
			t.Fatalf("after bob signed out on the %s his row = %+v, want no refresh token, no held_since and no signed_out_at", builds.previousName(), row)
		}
		reader.requireHeld(t, bobEmail, false, "after bob signed out on the "+builds.previousName())
	})

	t.Run("the previous build's hourly end of a refused person records no hold, and this tree answers them not held", func(t *testing.T) {
		session := signInWith(t, previous, issuer, rollClaims(carolEmail))
		if row := reader.row(t, carolEmail); !row.refreshToken || row.sub != "" {
			t.Fatalf("after carol signed in on the %s her row = %+v, want a refresh token and no Google account (sub): a group's sign-in records none",
				builds.previousName(), row)
		}
		reader.dateConfirmationBack(t, carolEmail)
		issuer.RefuseRefresh(carolEmail, true)
		refreshes := issuer.Refreshes()
		if body, status := cookieGet(t, previous, "/auth/whoami", session); status != http.StatusUnauthorized {
			t.Fatalf("/auth/whoami on the %s with carol's cookie, her confirmation two hours old and her refresh refused, answered %d %s, want 401: the hourly check ends her sign-in",
				builds.previousName(), status, body)
		}
		if issuer.Refreshes() == refreshes {
			t.Fatalf("the %s answered carol's request without asking the issuer to refresh her sign-in", builds.previousName())
		}
		// The end's line names carol as the previous build logs a person: by her address up to the
		// release before the one that logs a login's hash in its place (model.LoginHash), and by her
		// hash from then on. The same-build job runs this tree as the previous build, so either
		// form is the previous build's own.
		const ended = `msg="dispatch: sign-in ended: the sign-in pool did not confirm the person's membership"`
		if !previous.logged(ended, "email="+carolEmail) && !previous.logged(ended, "login_hash="+model.LoginHash(carolEmail)) {
			t.Fatalf("the %s did not log %s with email=%s or login_hash=%s:\n%s", builds.previousName(), ended, carolEmail, model.LoginHash(carolEmail), previous.Output.String())
		}
		if row := reader.row(t, carolEmail); row.refreshToken || row.heldSince != nil {
			t.Fatalf("after the %s ended carol's sign-in her row = %+v, want no refresh token and no held_since", builds.previousName(), row)
		}
		reader.requireHeld(t, carolEmail, false, "after the "+builds.previousName()+" ended carol's sign-in")
	})
}

// holdReader reads a person's people row and this tree's person answer, as the broker's service
// account, whose projected token bearer is.
type holdReader struct {
	builds *twoBuilds
	bearer string
}

// holdRow is what a row reads of a person's people row.
type holdRow struct {
	refreshToken bool
	sub          string
	heldSince    *time.Time
	signedInAt   *time.Time
	signedOutAt  *time.Time
}

// held is whether the row records a hold: held_since later than signed_in_at.
func (r holdRow) held() bool {
	return r.heldSince != nil && r.signedInAt != nil && r.heldSince.After(*r.signedInAt)
}

func (h holdReader) row(t *testing.T, email string) holdRow {
	t.Helper()
	var row holdRow
	if err := h.builds.database.Pool.QueryRow(context.Background(), `
		select refresh_token is not null, coalesce(sub, ''), held_since, signed_in_at, signed_out_at
		from people where email = $1
	`, email).Scan(&row.refreshToken, &row.sub, &row.heldSince, &row.signedInAt, &row.signedOutAt); err != nil {
		t.Fatalf("read the people row of %s: %v", email, err)
	}
	return row
}

// dateConfirmationBack dates email's confirmation two hours back, past the hour each build's
// confirmation stands for, so the next check of the person refreshes with the issuer.
func (h holdReader) dateConfirmationBack(t *testing.T, email string) {
	t.Helper()
	if _, err := h.builds.database.Pool.Exec(context.Background(),
		`update people set confirmed_at = now() - interval '2 hours' where email = $1`, email); err != nil {
		t.Fatalf("date %s's confirmation two hours back: %v", email, err)
	}
}

// held is this tree's person answer for email: whether it holds them, and the answer as sent. It
// fails t unless the route answers 200 with held.
func (h holdReader) held(t *testing.T, email string) (bool, string) {
	t.Helper()
	current := h.builds.current()
	request, err := http.NewRequest(http.MethodGet, current.url("/api/v1/access/person?person="+url.QueryEscape(email)), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.bearer)
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET /api/v1/access/person for %s on the %s: %v", email, h.builds.currentName(), err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var answer struct {
		Held *bool `json:"held"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &answer) != nil || answer.Held == nil {
		t.Fatalf("GET /api/v1/access/person for %s on the %s answered %d %s, want 200 with held", email, h.builds.currentName(), response.StatusCode, body)
	}
	return *answer.Held, string(body)
}

// requireHeld fails t unless this tree's person answer for email is want, saying when.
func (h holdReader) requireHeld(t *testing.T, email string, want bool, when string) {
	t.Helper()
	if held, answer := h.held(t, email); held != want {
		t.Errorf("%s the %s answers %s held %t, want %t: %s", when, h.builds.currentName(), email, held, want, answer)
	}
}

// waitHeld waits until this tree's person answer for email is held. Standing waits 2 seconds for a
// refresh, then answers by the 24-hour rule while the refresh records the hold within its own 15,
// so the first answer may still be not held.
func (h holdReader) waitHeld(t *testing.T, email, when string) {
	t.Helper()
	var answer string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var held bool
		if held, answer = h.held(t, email); held {
			return
		}
	}
	t.Fatalf("%s the %s still answers %s not held after 30s: %s", when, h.builds.currentName(), email, answer)
}

// rollClaims are the claims that name email both ways a build reads them, under the one Google
// account "google-<email>": the verified address of the Workspace account this tree's
// DISPATCH_SIGNIN_DOMAIN reads (workspaceClaims), and the pool username and group the previous
// build's DISPATCH_SIGNIN_GROUP reads (poolClaims).
func rollClaims(email string) map[string]any {
	claims := workspaceClaims(email)
	maps.Copy(claims, poolClaims("google-"+email, email))
	return claims
}

// signOutOn signs the browser holding session out of p (POST /auth/logout), from p's own origin as
// its page does, and requires 200.
func signOutOn(t *testing.T, p *dispatchProcess, session *http.Cookie) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, p.url("/auth/logout"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(session)
	request.Header.Set("Origin", p.url(""))
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("POST /auth/logout: %v", err)
	}
	defer response.Body.Close()
	if body, _ := io.ReadAll(response.Body); response.StatusCode != http.StatusOK {
		t.Fatalf("POST /auth/logout answered %d %s, want 200", response.StatusCode, body)
	}
}
