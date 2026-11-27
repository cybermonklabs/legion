// packages/envoy/internal/cipherclient/device_test.go
//
// A person's sign-in, which `agent-secrets login` runs: Google's device authorization grant
// against a real OpenID issuer (internal/oidc/oidctest, the issuer the broker's own tests mint
// against and the one cmd/dev-oidc-issuer serves), asking for openid and email and nothing else,
// and the refresh token it leaves minting the next identity token with no step by the person.

package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

func TestDeviceSignInAsksForWhoTheyAreAndNothingElse(t *testing.T) {
	const (
		clientID     = "example-device-client.apps.googleusercontent.example"
		clientSecret = "ex" + "ample-device-client-secret"
		person       = "ada@example.com"
	)
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-key")
	issuer.EnableDeviceFlow(key, clientID, clientSecret)
	issuer.SignInByDefault(map[string]any{
		"sub": person, "email": person, "email_verified": true, "hd": "example.com",
	})

	asked := &recordingTransport{}
	flow := &DeviceFlow{
		Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientSecret,
		HTTP: &http.Client{Transport: asked},
	}
	ctx := context.Background()
	authorization, err := flow.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if authorization.UserCode == "" || authorization.VerificationURI == "" {
		t.Fatalf("the sign-in answered %+v, want a code for the person to type and the page to type it on", authorization)
	}
	if scopes := asked.scopes(); !slices.Equal(scopes, []string{"openid", "email"}) {
		t.Errorf("the sign-in asked for the scopes %v, want openid and email alone", scopes)
	}
	if err := issuer.ApproveDeviceCode(authorization.UserCode); err != nil {
		t.Fatalf("the person types the code in: %v", err)
	}
	authorization.Interval = 1
	signIn, err := flow.Wait(ctx, authorization)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if signIn.Person != person {
		t.Errorf("the sign-in names %q, want %q: the person the identity token's email claim names", signIn.Person, person)
	}
	if signIn.IDToken == "" || signIn.RefreshToken == "" {
		t.Fatal("the sign-in left no identity token or no refresh token, so this machine could not renew it without the person")
	}

	// The record `agent-secrets login` keeps is the refresh token, and what it buys is a fresh
	// identity token at every trade.
	identity := &PersonIdentity{Flow: flow, RefreshToken: signIn.RefreshToken, Person: person}
	token, tokenType, err := identity.Token(ctx)
	if err != nil {
		t.Fatalf("the person's identity at the next trade: %v", err)
	}
	if tokenType != oauthwire.SubjectTokenTypePerson {
		t.Errorf("a person's token is presented as %q, want %q", tokenType, oauthwire.SubjectTokenTypePerson)
	}
	if token == "" {
		t.Error("the refresh answered no identity token")
	}
	if got, err := PersonOf(token); err != nil || got != person {
		t.Errorf("the renewed token names %q (%v), want %q", got, err, person)
	}
}

// TestASignInTheIssuerNoLongerHonoursNamesTheCommandThatMakesAnother: a person who revoked the
// grant, or a refresh token Google retired, stops the machine with a sentence that says what to
// run, which is the rule every refusal a sign-in cures follows.
func TestASignInTheIssuerNoLongerHonoursNamesTheCommandThatMakesAnother(t *testing.T) {
	const clientID, clientSecret = "ex" + "ample-device-client", "example-device-client-secret"
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-key")
	issuer.EnableDeviceFlow(key, clientID, clientSecret)
	issuer.SignInByDefault(map[string]any{"sub": "ada", "email": "ada@example.com", "email_verified": true})
	flow := &DeviceFlow{Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientSecret}
	_, err := flow.Refresh(context.Background(), "a-refresh-token-this-issuer-never-issued")
	if err == nil || !strings.Contains(err.Error(), "agent-secrets login") {
		t.Fatalf("Refresh with a retired refresh token = %v, want a refusal naming `agent-secrets login`", err)
	}
}

// TestOnlyTheIssuersRefusalEndsASignIn: a refresh ends the sign-in (ErrSignInEnded) only when the
// issuer's token endpoint answers 400 or 401 with the OAuth error invalid_grant (RFC 6749 section
// 5.2), the rule oauthrefusal.EndsGrant states for every reader that ends a grant. A 429 or a 5xx
// is the issuer being busy or down, whatever its body names, and a 403 is no end of a grant, and
// none of them says the person's grant is over: reading one as the end of the sign-in would drop
// the token in hand, stop the renewal loop and fall the box back to its machine over an outage.
// Every row renews a refresh token the issuer issued.
//
// The named reverts: read invalid_grant at any status as ErrSignInEnded, as Refresh once did, and
// the 403, 429, 500 and 503 rows fail; read it by oauthrefusal.Refusal, which takes a 403 as a
// refusal's code, and the 403 row fails.
func TestOnlyTheIssuersRefusalEndsASignIn(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		ended  bool
	}{
		{http.StatusBadRequest, "invalid_grant", true},
		{http.StatusUnauthorized, "invalid_grant", true},
		{http.StatusForbidden, "invalid_grant", false},
		{http.StatusTooManyRequests, "invalid_grant", false},
		{http.StatusInternalServerError, "invalid_grant", false},
		{http.StatusServiceUnavailable, "invalid_grant", false},
		{http.StatusBadRequest, "invalid_request", false},
	} {
		t.Run(fmt.Sprintf("%d %s", tc.status, tc.code), func(t *testing.T) {
			rig := newSignInRig(t, nil)
			record := rig.signIn(t, "ada@example.com")
			rig.issuer.FailToken(tc.status, tc.code)
			signIn, err := rig.flow.Refresh(context.Background(), record.RefreshToken)
			if signIn != nil || err == nil {
				t.Fatalf("Refresh answered %+v, %v; want the failure", signIn, err)
			}
			if ended := errors.Is(err, ErrSignInEnded); ended != tc.ended {
				t.Errorf("Refresh = %v, ErrSignInEnded %t; want %t", err, ended, tc.ended)
			}
			if named := strings.Contains(err.Error(), SignInCommand); named != tc.ended {
				t.Errorf("Refresh = %v, naming %s %t; want %t: only an ended sign-in names the command that makes another", err, SignInCommand, named, tc.ended)
			}
		})
	}
}

// recordingTransport keeps the form of every request it carries, so a test reads what the
// sign-in asked the issuer for.
type recordingTransport struct {
	forms []url.Values
}

func (r *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil && request.GetBody != nil {
		if body, err := request.GetBody(); err == nil {
			defer body.Close()
			buffer := make([]byte, 1<<16)
			n, _ := body.Read(buffer)
			if form, err := url.ParseQuery(string(buffer[:n])); err == nil {
				r.forms = append(r.forms, form)
			}
		}
	}
	return http.DefaultTransport.RoundTrip(request)
}

// scopes is the scope the device authorization asked for, as its own words.
func (r *recordingTransport) scopes() []string {
	for _, form := range r.forms {
		if scope := form.Get("scope"); scope != "" {
			return strings.Fields(scope)
		}
	}
	return nil
}

// TestASignInHoldsTheIssuersIntervalAndLifetime: Wait polls a copy of the device authorization
// held to the device grant's bounds (BoundDeviceAuth), as the broker helper's machine login holds
// its own. A code that names no lifetime, or one past the hour, is polled under a deadline an hour
// away at most, where x/oauth2 polls one that names none under no deadline at all, and one of two
// hours under that deadline. A code of ten minutes keeps its own, a negative interval beside it.
func TestASignInHoldsTheIssuersIntervalAndLifetime(t *testing.T) {
	const (
		clientID     = "example-device-client.apps.googleusercontent.example"
		clientSecret = "ex" + "ample-device-client-secret"
		person       = "ada@example.com"
	)
	for _, c := range []struct {
		name     string
		interval int64         // the device authorization's interval
		life     time.Duration // its lifetime from the sign-in's start; 0 names none
		want     time.Duration // how far past that start the polls' deadline is
	}{
		{"no lifetime", 1, 0, time.Hour},
		{"a lifetime of two hours", 1, 2 * time.Hour, time.Hour},
		// Control: a lifetime inside the bound is kept.
		{"a lifetime of ten minutes and a negative interval", -3, 10 * time.Minute, 10 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer := oidctest.New(t)
			key := issuer.PublishKey(t, "google-key")
			issuer.EnableDeviceFlow(key, clientID, clientSecret)
			issuer.SignInByDefault(map[string]any{"sub": person, "email": person, "email_verified": true, "hd": "example.com"})
			polls := &deadlineTransport{}
			flow := &DeviceFlow{Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientSecret, HTTP: &http.Client{Transport: polls}}
			ctx := context.Background()
			authorization, err := flow.Start(ctx)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if err := issuer.ApproveDeviceCode(authorization.UserCode); err != nil {
				t.Fatalf("the person types the code in: %v", err)
			}
			started := time.Now()
			authorization.Interval, authorization.Expiry = c.interval, time.Time{}
			if c.life != 0 {
				authorization.Expiry = started.Add(c.life)
			}
			signIn, err := flow.Wait(ctx, authorization)
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if signIn.Person != person {
				t.Errorf("the sign-in names %q, want %q", signIn.Person, person)
			}
			// The deadline is set between the start and now, so it lies within want of each.
			if len(polls.deadlines) == 0 || polls.deadlines[0].IsZero() || polls.deadlines[0].Before(started.Add(c.want)) || polls.deadlines[0].After(time.Now().Add(c.want)) {
				t.Fatalf("the polls ran under the deadlines %v (zero: none), want one %v after the start", polls.deadlines, c.want)
			}
			if authorization.Interval != c.interval {
				t.Errorf("Wait changed the caller's interval to %d", authorization.Interval)
			}
		})
	}
}

// TestASignInWhoseCodeLapsesBeforeItsFirstPollTimesOut: a code that lapses no later than its first
// poll, the issuer's interval on, is never polled. x/oauth2's ticker does not fire before the
// code's end, its context's deadline, so Wait answers that deadline, which prints as "timed out".
// x/oauth2 keeps its own time here, so the code lives 200 ms against an interval of 60 s.
func TestASignInWhoseCodeLapsesBeforeItsFirstPollTimesOut(t *testing.T) {
	const (
		clientID     = "example-device-client.apps.googleusercontent.example"
		clientSecret = "ex" + "ample-device-client-secret"
	)
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-key")
	issuer.EnableDeviceFlow(key, clientID, clientSecret)
	polls := &deadlineTransport{}
	flow := &DeviceFlow{Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientSecret, HTTP: &http.Client{Transport: polls}}
	ctx := context.Background()
	authorization, err := flow.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	authorization.Interval, authorization.Expiry = 60, time.Now().Add(200*time.Millisecond)
	signIn, err := flow.Wait(ctx, authorization)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.HasSuffix(err.Error(), ": timed out") {
		t.Fatalf("Wait answered %+v, %v, want the code's deadline, printed as timed out", signIn, err)
	}
	if len(polls.deadlines) != 0 {
		t.Fatalf("the sign-in polled %d times, want none", len(polls.deadlines))
	}
}

// TestTheDeviceGrantsBoundsHoldTheIntervalAndTheLifetime: BoundDeviceAuth holds the interval to 1 s
// at least, 5 s where none is named, and the lifetime to an hour from now, one that names none
// included, as the TypeScript device login holds them. An interval past a minute is kept, as RFC
// 8628 section 3.5 has a client wait at least the interval named, up to the hour, past which no
// code could be polled before it lapses. A lifetime inside the hour, or one already past, is kept.
// It reports the hour as what ends the code where the server named a longer lifetime or none, so
// a refusal names that limit and not the server's. A lifetime x/oauth2 wraps to one under the hour
// is kept as that.
func TestTheDeviceGrantsBoundsHoldTheIntervalAndTheLifetime(t *testing.T) {
	now := time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		interval, wantInterval int64
		expiry, wantExpiry     time.Time
		hourEnds               bool
	}{
		{-3, 1, now.Add(10 * time.Minute), now.Add(10 * time.Minute), false},
		{0, 5, time.Time{}, now.Add(time.Hour), true},
		{30, 30, now.Add(2 * time.Hour), now.Add(time.Hour), true},
		{90, 90, now.Add(time.Hour), now.Add(time.Hour), false},
		{86400, 3600, now.Add(-time.Minute), now.Add(-time.Minute), false},
		{1e12, 3600, now.Add(time.Hour), now.Add(time.Hour), false},
	} {
		da := &oauth2.DeviceAuthResponse{Interval: c.interval, Expiry: c.expiry}
		hourEnds := BoundDeviceAuth(da, now)
		if da.Interval != c.wantInterval || !da.Expiry.Equal(c.wantExpiry) || hourEnds != c.hourEnds {
			t.Errorf("an interval of %d and an end %v held to %d and %v, the hour ending it %v, want %d and %v, %v", c.interval, c.expiry, da.Interval, da.Expiry, hourEnds, c.wantInterval, c.wantExpiry, c.hourEnds)
		}
	}
	// The end is one reading of the monotonic clock, though x/oauth2 dates it from the wall clock
	// alone (time.Now().UTC()), so a comparison with it, a context's deadline and a timer read one
	// clock. Round(0) drops that reading, so the end differs from it only where it carries one.
	da := &oauth2.DeviceAuthResponse{Expiry: time.Now().UTC().Add(10 * time.Minute)}
	BoundDeviceAuth(da, time.Now())
	if da.Expiry == da.Expiry.Round(0) {
		t.Error("the bounded end carries no monotonic reading, so a comparison with it reads the wall clock")
	}
	// x/oauth2 counts a lifetime's nanoseconds modulo 2^64, so one past about 584 years can wrap
	// forward, to one under the hour: 18446744974 s comes out as about 900 s, which the bounds
	// keep, as though the server had named it.
	var wrapped oauth2.DeviceAuthResponse
	if err := json.Unmarshal([]byte(`{"device_code": "d", "user_code": "u", "expires_in": 18446744974}`), &wrapped); err != nil {
		t.Fatalf("read a device authorization whose lifetime wraps: %v", err)
	}
	hourEnds := BoundDeviceAuth(&wrapped, time.Now())
	if left := time.Until(wrapped.Expiry); hourEnds || left > 901*time.Second || left < 800*time.Second {
		t.Errorf("a lifetime of 18446744974 s left %v of the code, the hour ending it %v, want about 900 s, the hour not ending it", left, hourEnds)
	}
}

// deadlineTransport carries every request to the issuer and keeps, for each device code poll, the
// deadline of its context (the zero time for none): x/oauth2 polls under the code's lifetime.
type deadlineTransport struct {
	deadlines []time.Time
}

func (d *deadlineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.GetBody != nil {
		if body, err := request.GetBody(); err == nil {
			buffer := make([]byte, 1<<16)
			n, _ := body.Read(buffer)
			body.Close()
			if form, err := url.ParseQuery(string(buffer[:n])); err == nil && form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" {
				deadline, _ := request.Context().Deadline()
				d.deadlines = append(d.deadlines, deadline)
			}
		}
	}
	return http.DefaultTransport.RoundTrip(request)
}
