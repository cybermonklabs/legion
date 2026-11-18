// session_release_test.go drives POST /v1/oauth/exchange/pod-sessions/release on the real broker
// api_test.go mounts: a daemon releasing the session names it registered stops its pods' trades
// under them, while each name stays bound to it, and touches no other daemon's registration.
package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// releasePodSessions posts body to POST /v1/oauth/exchange/pod-sessions/release with bearer as the
// calling service's own ServiceAccount token.
func (ts *testServer) releasePodSessions(t *testing.T, bearer string, body any) (int, []byte) {
	t.Helper()
	return ts.req(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions/release", map[string]string{"Authorization": "Bearer " + bearer}, body)
}

// releases reports, without stopping the test, unless the registrar whose login token bearer is,
// releasing sessions, is answered 200 with exactly {"sessions": sessionCount, "released":
// released}; what names the release.
func (ts *testServer) releases(t *testing.T, what, bearer string, sessions []string, sessionCount, released int) {
	t.Helper()
	status, body := ts.releasePodSessions(t, bearer, map[string]any{"sessions": sessions})
	want := map[string]any{"sessions": float64(sessionCount), "released": float64(released)}
	var got map[string]any
	if err := json.Unmarshal(body, &got); status != http.StatusOK || err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %d %s, want 200 with exactly %v", what, status, strings.TrimSpace(string(body)), want)
	}
}

// TestAReleasedNameIsNotHonoured: daemon A registers pod P for session S, bound to the key k of
// S's role, and P trades under S proved with k. A releases S at the route and is answered 200
// with released: 1. The row stays in oauth_pod_sessions, marked released, and P's trade under S,
// proved with k, is refused invalid_grant naming S, with a description that does not begin
// pod_session_not_registered:, which both clients retry: the release is final for that
// registration. The released row still holds S for A, so daemon B, listed for the same pod
// account, is still refused S. A's next write of P and S takes the row back, and P trades again.
func TestAReleasedNameIsNotHonoured(t *testing.T) {
	ts := newTestServer(t)
	const session, pod, podB = "implementer-g1", "pod-uid-released", "pod-uid-of-b"
	daemonA, daemonB := ts.accountToken(t, daemonAccount, "", time.Hour), ts.accountToken(t, otherDaemonAccount, "", time.Hour)
	key := newSigningKey(t)
	k := thumbprintOf(t, key)
	register := func(what string) {
		t.Helper()
		status, body := ts.req(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions", map[string]string{"Authorization": "Bearer " + daemonA},
			map[string]any{"pod_token": ts.rolesPodToken(t, pod), "session": session, "thumbprint": k})
		if got := decode[wirePodSession](t, body); status != http.StatusOK || got.PodUID != pod || got.Session != session || got.Thumbprint != k {
			t.Fatalf("%s = %d %s, want 200 naming pod %s, session %s and the thumbprint %s", what, status, body, pod, session, k)
		}
	}
	trade := func() exchanged {
		t.Helper()
		return ts.exchangeProvedBy(t, key, ts.rolesTradeToken(t, pod), session)
	}

	register("A's registration of " + pod + " for " + session + " bound to k")
	if answer := trade(); answer.status != http.StatusOK || !answer.issued {
		t.Fatalf("%s's trade under %s proved with k, which %s registered it for = %+v, want 200 with an access token", pod, session, daemonAccount, answer)
	}

	ts.releases(t, daemonAccount+"'s release of "+session, daemonA, []string{session}, 1, 1)
	if rows, want := ts.column(t, `select pod_uid || ' of ' || registrar || ' released ' || (released_at is not null)::text
		from oauth_pod_sessions where session = $1`, session), []string{pod + " of " + daemonAccount + " released true"}; !slices.Equal(rows, want) {
		t.Fatalf("oauth_pod_sessions holds %v for %s after %s released it, want %v: a release deletes nothing", rows, session, daemonAccount, want)
	}
	answer := trade()
	if answer.status != http.StatusBadRequest || answer.code != "invalid_grant" || answer.issued ||
		strings.HasPrefix(answer.description, oauthwire.PodSessionNotRegistered+":") || !strings.Contains(answer.description, session) {
		t.Fatalf("%s's trade under %s proved with k, after %s released %s = %+v; want 400 invalid_grant naming %s, its description not beginning %s:, and no token",
			pod, session, daemonAccount, session, answer, session, oauthwire.PodSessionNotRegistered)
	}

	status, body := ts.registerPodSession(t, daemonB, ts.rolesPodToken(t, podB), session)
	if refusal := decode[wireError](t, body); status != http.StatusConflict || refusal.Code != "SESSION_HELD_BY_ANOTHER_REGISTRAR" ||
		!strings.Contains(refusal.Error, session) || !strings.Contains(refusal.Error, daemonAccount) {
		t.Fatalf("%s registering %s for %s, which %s released = %d %s; want 409 SESSION_HELD_BY_ANOTHER_REGISTRAR naming %s and %s: a released registration holds its name until it lapses",
			otherDaemonAccount, podB, session, daemonAccount, status, strings.TrimSpace(string(body)), session, daemonAccount)
	}

	register("A's registration of " + pod + " for " + session + " again, after its release")
	if answer := trade(); answer.status != http.StatusOK || !answer.issued {
		t.Fatalf("%s's trade under %s proved with k, once %s wrote its registration again = %+v, want 200 with an access token", pod, session, daemonAccount, answer)
	}
}

// TestTheReleaseRouteReleasesOnlyTheCallersRegistrations: daemon A holds S1, registered for two
// pods, and S2; daemon B holds S3. A release the route refuses releases nothing: 0 names, 4097
// (one name sent 4097 times, since the bound counts entries as sent), a missing sessions, a null
// one and a body of null are each 400 SESSIONS_INVALID naming the count, an unknown field is 400
// INVALID_REQUEST, a list of one null and B's list of its own S3 and a null are each 400
// INVALID_REQUEST naming the null's index, since a null is no name, an entry that is a number
// and a body that is a string are each 400 INVALID_REQUEST with encoding/json's whole text, which
// names podSessionReleaseBody, and for the number its sessions field too, and an account
// BROKER_SERVICE_LOGINS does not list is 403 ACCOUNT_NOT_LISTED; 4096 entries of a name B holds
// no registration of are admitted and release nothing. B releasing [S1, S3, S3] is answered
// {sessions: 2, released: 1}: its own S3, and none of A's rows of S1, whose pods still trade. A,
// once the list no longer names it, releasing [S2, S1, "has space", "", "\u0000"] is answered
// {sessions: 5, released: 3}, and the same again, which leaves each release mark where the first
// call put it: a release is not gated by BROKER_POD_REGISTRARS, and a name no registration could
// have taken is released with nothing to release, a NUL byte, which Postgres refuses in text,
// included. No row's expires_at moved.
func TestTheReleaseRouteReleasesOnlyTheCallersRegistrations(t *testing.T) {
	var registrars api.PodRegistrars
	ts := newTestServerWith(t, func(d *api.Deps) { registrars = d.PodRegistrars })
	const s1, s2, s3 = "implementer-g1", "reviewer-g1", "tester-g1"
	const p1, p1Sibling, p2, p3 = "pod-uid-a-1", "pod-uid-a-1-sibling", "pod-uid-a-2", "pod-uid-b-3"
	daemonA, daemonB := ts.accountToken(t, daemonAccount, "", time.Hour), ts.accountToken(t, otherDaemonAccount, "", time.Hour)
	registrations := func() []string {
		return ts.column(t, `select pod_uid || ' ' || session || ' of ' || registrar || ' released ' || (released_at is not null)::text
			from oauth_pod_sessions order by pod_uid`)
	}
	marks := func() []string {
		return ts.column(t, `select pod_uid || ' expires ' || expires_at::text || ' released ' || coalesce(released_at::text, 'never')
			from oauth_pod_sessions order by pod_uid`)
	}
	held := []struct{ registrar, pod, session string }{
		{daemonA, p1, s1}, {daemonA, p1Sibling, s1}, {daemonA, p2, s2}, {daemonB, p3, s3},
	}
	for _, h := range held {
		ts.admits(t, "the registration of "+h.pod+" for "+h.session, h.registrar, ts.rolesPodToken(t, h.pod), h.pod, h.session)
	}
	unreleased := []string{
		p1 + " " + s1 + " of " + daemonAccount + " released false",
		p1Sibling + " " + s1 + " of " + daemonAccount + " released false",
		p2 + " " + s2 + " of " + daemonAccount + " released false",
		p3 + " " + s3 + " of " + otherDaemonAccount + " released false",
	}
	slices.Sort(unreleased)
	if got := registrations(); !slices.Equal(got, unreleased) {
		t.Fatalf("oauth_pod_sessions holds %v after the four registrations, want %v", got, unreleased)
	}
	expiries := ts.column(t, `select pod_uid || ' expires ' || expires_at::text from oauth_pod_sessions order by pod_uid`)

	for _, c := range []struct {
		name   string
		bearer string
		body   any
		status int
		code   string
		names  string // what the refusal's error must name, "" for nothing
	}{
		{"no names", daemonB, map[string]any{"sessions": []string{}}, http.StatusBadRequest, "SESSIONS_INVALID", "holds 0 names"},
		{"4097 entries of one name", daemonB, map[string]any{"sessions": slices.Repeat([]string{s3}, 4097)}, http.StatusBadRequest, "SESSIONS_INVALID", "holds 4097 names"},
		{"a missing sessions", daemonB, map[string]any{}, http.StatusBadRequest, "SESSIONS_INVALID", "holds 0 names"},
		{"a null sessions", daemonB, map[string]any{"sessions": nil}, http.StatusBadRequest, "SESSIONS_INVALID", "holds 0 names"},
		{"a body of null", daemonB, json.RawMessage("null"), http.StatusBadRequest, "SESSIONS_INVALID", "holds 0 names"},
		{"an unknown field", daemonB, map[string]any{"sessions": []string{s3}, "pods": []string{p3}}, http.StatusBadRequest, "INVALID_REQUEST", ""},
		{"a list of one null", daemonB, map[string]any{"sessions": []any{nil}}, http.StatusBadRequest, "INVALID_REQUEST", "sessions[0] is null"},
		{"B's own " + s3 + " and a null", daemonB, map[string]any{"sessions": []any{s3, nil}}, http.StatusBadRequest, "INVALID_REQUEST", "sessions[1] is null"},
		{"an account BROKER_SERVICE_LOGINS does not list", ts.accountToken(t, unlistedAccount, "", time.Hour),
			map[string]any{"sessions": []string{s1, s2, s3}}, http.StatusForbidden, "ACCOUNT_NOT_LISTED", ""},
	} {
		status, body := ts.releasePodSessions(t, c.bearer, c.body)
		refusal := decode[wireError](t, body)
		if status != c.status || refusal.Code != c.code {
			t.Errorf("a release with %s = %d %s, want %d %s", c.name, status, strings.TrimSpace(string(body)), c.status, c.code)
		}
		if c.names != "" && !strings.Contains(refusal.Error, c.names) {
			t.Errorf("a release with %s is refused %q, want it to name %q", c.name, refusal.Error, c.names)
		}
	}
	// encoding/json's error is the text of these two refusals. It names the body's type, and for
	// the number its sessions field as well; a client reads it, so the test holds each refusal to
	// its whole text.
	for _, c := range []struct{ name, body, refusal string }{
		{"an entry that is a number", `{"sessions":[1]}`,
			"body must be one valid JSON object: json: cannot unmarshal number into podSessionReleaseBody.sessions.0 of type string"},
		{"a body that is a string", `"x"`,
			"body must be one valid JSON object: json: cannot unmarshal string into Go value of type api.podSessionReleaseBody"},
	} {
		status, body := ts.releasePodSessions(t, daemonB, json.RawMessage(c.body))
		if refusal := decode[wireError](t, body); status != http.StatusBadRequest || refusal.Code != "INVALID_REQUEST" || refusal.Error != c.refusal {
			t.Errorf("a release with %s = %d %s %q, want 400 INVALID_REQUEST %q", c.name, status, refusal.Code, refusal.Error, c.refusal)
		}
	}
	ts.releases(t, "B's release of 4096 entries of a name it holds no registration of", daemonB, slices.Repeat([]string{"unheld-g1"}, 4096), 1, 0)
	if got := registrations(); !slices.Equal(got, unreleased) {
		t.Fatalf("oauth_pod_sessions holds %v after the refused releases and one of a name no one holds, want %v: they release nothing", got, unreleased)
	}

	// Each check below reports and goes on, so a release that reached A's rows also shows whether
	// A's pods can still trade.
	ts.releases(t, "B's release of ["+s1+", "+s3+", "+s3+"], of which it holds "+s3+" alone", daemonB, []string{s1, s3, s3}, 2, 1)
	afterB := slices.Clone(unreleased)
	afterB[slices.Index(afterB, p3+" "+s3+" of "+otherDaemonAccount+" released false")] = p3 + " " + s3 + " of " + otherDaemonAccount + " released true"
	if got := registrations(); !slices.Equal(got, afterB) {
		t.Errorf("oauth_pod_sessions holds %v after B released [%s, %s, %s], want %v: B releases its own %s and none of %s's rows", got, s1, s3, s3, afterB, s3, daemonAccount)
	}
	for _, h := range held[:3] {
		if answer := ts.exchangePodToken(t, ts.rolesTradeToken(t, h.pod), h.session); answer.status != http.StatusOK || !answer.issued {
			t.Errorf("%s's trade under %s, which %s registered and B released nothing of = %+v, want 200 with an access token", h.pod, h.session, daemonAccount, answer)
		}
	}

	for pair := range registrars {
		if pair.Registrar == daemonAccount {
			delete(registrars, pair)
		}
	}
	status, body := ts.registerPodSession(t, daemonA, ts.rolesPodToken(t, p2), s2)
	refusedNotListed(t, daemonAccount+" registering once the list no longer names it", status, body, daemonAccount, brokertest.ExchangeRolesAccount)
	const nul = "\x00" // a NUL byte, which JSON carries as "\u0000"
	ts.releases(t, "A's release of ["+s2+", "+s1+", \"has space\", \"\", \"\\u0000\"], once the list no longer names it", daemonA, []string{s2, s1, "has space", "", nul}, 5, 3)
	released := ts.column(t, `select pod_uid || ' released ' || (released_at is not null)::text from oauth_pod_sessions order by pod_uid`)
	if want := []string{p1 + " released true", p1Sibling + " released true", p2 + " released true", p3 + " released true"}; !slices.Equal(released, want) {
		t.Errorf("after A released its names, oauth_pod_sessions holds %v, want %v", released, want)
	}
	first := marks()
	ts.releases(t, "A's same release again", daemonA, []string{s2, s1, "has space", "", nul}, 5, 3)
	if again := marks(); !slices.Equal(again, first) {
		t.Errorf("A's repeated release left %v, want %v: a repeat moves no release mark", again, first)
	}
	if got := ts.column(t, `select pod_uid || ' expires ' || expires_at::text from oauth_pod_sessions order by pod_uid`); !slices.Equal(got, expiries) {
		t.Errorf("the releases left the expiries %v, want %v: a release moves no expiry", got, expiries)
	}
}
