package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/ratelimit"
	"github.com/sjawhar/envoy/internal/testport"
)

// socketDoor is GET /ws/doc/{room} on one server, for a room that server's signed-in person can
// open, with the host name its document service named on the 101 it answered that person's
// cookie with: the process every refusal at the door must name too (docs.ProcessHeader).
type socketDoor struct {
	// room is the room the door opens, path the door's path, which a DPoP proof for it is signed
	// for, and url the address an upgrade dials.
	room, path, url string
	process         string
}

// socketAnswer is what one upgrade was answered: whether a socket opened (101), which the dial
// closes at once, and the answer's status, headers and body.
type socketAnswer struct {
	opened bool
	status int
	header http.Header
	body   string
}

// knock sends door one upgrade that carries headers and nothing else, and answers what the
// upgrade was answered.
func (door socketDoor) knock(t *testing.T, headers http.Header) socketAnswer {
	t.Helper()
	connection, response, err := dialLive(t, gws.DefaultDialer, door.url, headers)
	if response == nil {
		t.Fatalf("the upgrade at %s got no answer: %v", door.path, err)
	}
	if connection != nil {
		_ = connection.Close()
		return socketAnswer{opened: true, status: response.StatusCode, header: response.Header}
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to the upgrade at %s: %v", door.path, err)
	}
	return socketAnswer{status: response.StatusCode, header: response.Header, body: string(body)}
}

// openedDoor is the door to artifactID's room on server, read off the 101 the server answers the
// signed-in person's cookie with: the process the document service names there. That 101 is also
// the control of every row sent to the door: the same server opens the same room's socket for a
// sign-in, so a refusal there is the credential's and not the room's.
func openedDoor(t *testing.T, server *httptest.Server, artifactID string, cookie *http.Cookie) socketDoor {
	t.Helper()
	door := socketDoor{room: artifactID, path: "/ws/doc/" + artifactID}
	door.url = "ws" + strings.TrimPrefix(server.URL, "http") + door.path
	signedIn := door.knock(t, http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}})
	if !signedIn.opened {
		t.Fatalf("the signed-in person's cookie at %s answered %d %s, want 101", door.path, signedIn.status, strings.TrimSpace(signedIn.body))
	}
	if door.process = signedIn.header.Get(docs.ProcessHeader); door.process == "" {
		t.Fatalf("the 101 at %s names no process (%s), so a refusal that names none could not be told from one that names it", door.path, docs.ProcessHeader)
	}
	return door
}

// presenting is what caller sends door with token under the DPoP scheme, as presentingAt builds it
// for the door's room.
func (door socketDoor) presenting(t *testing.T, caller *brokerCaller, token string) http.Header {
	t.Helper()
	return presentingAt(t, caller, door.room, token)
}

// credentialsIn is the text of each credential headers carries: an Authorization header's token,
// past its scheme, a DPoP proof, and a cookie's value.
func credentialsIn(headers http.Header) []string {
	var credentials []string
	for _, authorization := range headers.Values("Authorization") {
		if _, token, _ := strings.Cut(authorization, " "); token != "" {
			credentials = append(credentials, token)
		}
	}
	for _, proof := range headers.Values("DPoP") {
		if proof != "" {
			credentials = append(credentials, proof)
		}
	}
	for _, cookie := range (&http.Request{Header: headers}).Cookies() {
		credentials = append(credentials, cookie.Value)
	}
	return credentials
}

// TestADocumentSocketRefusesEachCredentialItDoesNotAdmit sends GET /ws/doc/{room} each credential
// the socket does not admit, alone, as a websocket upgrade: no credential; a cookie whose session
// ended, alone and with the shared bearer beside it; a cookie whose lapsed confirmation the
// sign-in issuer cannot renew, alone and with the shared bearer beside it; the shared bearer, with
// and without X-Dispatch-Actor; a personal token that writes and one that only reads; a
// service-account token on a server that verifies them; a read-only token the broker issued,
// which the socket does not serve; each broker-issued token the API refuses, and a broker-issued
// token on a server with no broker. Each is refused before the upgrade: no socket opens, the
// answer is the row's status with the row's code as JSON, it names the process that server's
// document service named on the 101 it answered a signed-in person's cookie with (openedDoor), and
// it repeats nothing of the credential. The empty bucket's refusal carries Retry-After too.
func TestADocumentSocketRefusesEachCredentialItDoesNotAdmit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const sharedBearer = "Bearer agent-token"

	// The server most rows are sent to: its people sign in with a cookie, and it takes the shared
	// bearer, personal tokens, service-account tokens and the stub broker's tokens.
	served := newDocumentSocketTokenRig(t, time.Hour, nil, verifyingServiceAccounts)
	door := openedDoor(t, served.server, served.artifactID, served.cookie)
	broker := served.tokens.broker
	caller := newBrokerCaller(t)
	writer := mintPersonalToken(t, served.tokens.handler, brokerTestPerson, "a writer", []string{model.ScopePerson}).Token
	reader := readOnlyPersonalTokens(t, served.tokens)[0].token
	serviceAccount := served.serviceTokens.mint(t, serviceTokenAudience)
	// The person signs out once the door is read, so their cookie is one whose session ended.
	if err := served.sessions.RevokeSessions(ctx, documentSocketPerson); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	ended := http.Header{"Cookie": {served.cookie.Name + "=" + served.cookie.Value}}

	// A server whose address bucket holds one introspection and refills more slowly than the row
	// lasts.
	limited := newDocumentSocketTokenRig(t, time.Hour, nil, brokerTokenOptions(func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Minute, Burst: 1}
	}))
	limitedDoor := openedDoor(t, limited.server, limited.artifactID, limited.cookie)

	// A server whose broker is at a port the test holds and nothing listens on.
	refused := "http://" + testport.Refused(t)
	unanswered := newDocumentSocketTokenRig(t, time.Hour, nil, brokerTokenOptions(func(o *ciphertoken.Options) { o.BrokerURL = refused }))
	unansweredDoor := openedDoor(t, unanswered.server, unanswered.artifactID, unanswered.cookie)

	// A server whose person signed in through a sign-in pool, and which has no broker configured.
	// Once the door is read, the person's confirmation is dated two hours back and the issuer
	// answers no refresh, as in TestASignInIssuerOutageAnswersTheAPIAndTheDocumentSocket503AndKeepsTheSession:
	// the cookie's lapsed confirmation cannot be renewed.
	pooled := newDocumentSocketRig(t, time.Hour)
	pooledDoor := openedDoor(t, pooled.server, pooled.artifactID, pooled.cookie)
	if _, err := pooled.database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '2 hours' where email = $1`, pooled.email); err != nil {
		t.Fatalf("date the confirmation two hours back: %v", err)
	}
	pooled.pool.FailToken(http.StatusServiceUnavailable, "")
	lapsed := http.Header{"Cookie": {pooled.cookie.Name + "=" + pooled.cookie.Value}}

	beside := func(cookie http.Header, authorization string) http.Header {
		headers := cookie.Clone()
		headers.Set("Authorization", authorization)
		return headers
	}
	alone := func(headers http.Header) func(*testing.T) http.Header {
		return func(*testing.T) http.Header { return headers }
	}
	// live is token, which the stub broker now holds live, bound to caller's key.
	live := func(token string) string {
		broker.Says(token, liveNamedToken(caller.jkt))
		return token
	}

	for _, row := range []struct {
		name string
		door socketDoor
		// headers is what the row's upgrade carries, built as the row runs and after any request
		// of the row's own that its refusal rests on.
		headers func(t *testing.T) http.Header
		status  int
		code    string
		// retryAfter is the Retry-After the refusal carries; empty is a row that checks none.
		retryAfter string
	}{
		{name: "no credential", door: door, headers: alone(http.Header{}),
			status: http.StatusUnauthorized, code: "NO_IDENTITY"},
		{name: "a cookie whose session ended", door: door, headers: alone(ended),
			status: http.StatusUnauthorized, code: "NO_IDENTITY"},
		{name: "a cookie whose lapsed confirmation the issuer cannot renew", door: pooledDoor, headers: alone(lapsed),
			status: http.StatusServiceUnavailable, code: "SIGNIN_UNAVAILABLE"},
		{name: "the same lapsed cookie with the shared bearer beside it", door: pooledDoor, headers: alone(beside(lapsed, sharedBearer)),
			status: http.StatusUnauthorized, code: "DOC_SOCKET_CREDENTIAL_REFUSED"},
		{name: "a cookie whose session ended, with the shared bearer beside it", door: door, headers: alone(beside(ended, sharedBearer)),
			status: http.StatusUnauthorized, code: "DOC_SOCKET_CREDENTIAL_REFUSED"},
		{name: "the shared bearer", door: door, headers: alone(http.Header{"Authorization": {sharedBearer}}),
			status: http.StatusUnauthorized, code: "DOC_SOCKET_CREDENTIAL_REFUSED"},
		{name: "the shared bearer with X-Dispatch-Actor", door: door, headers: alone(http.Header{
			"Authorization":    {sharedBearer},
			"X-Dispatch-Actor": {`{"kind":"session","id":"ses_the_shared_bearer_names"}`},
		}), status: http.StatusUnauthorized, code: "DOC_SOCKET_CREDENTIAL_REFUSED"},
		{name: "a personal agent token that writes", door: door, headers: alone(http.Header{"Authorization": {"Bearer " + writer}}),
			status: http.StatusUnauthorized, code: "DOC_SOCKET_CREDENTIAL_REFUSED"},
		{name: "a read-only personal token", door: door, headers: alone(http.Header{"Authorization": {"Bearer " + reader}}),
			status: http.StatusUnauthorized, code: "DOC_SOCKET_CREDENTIAL_REFUSED"},
		{name: "a service-account token on a server that verifies them", door: door, headers: alone(http.Header{"Authorization": {"Bearer " + serviceAccount}}),
			status: http.StatusUnauthorized, code: "DOC_SOCKET_CREDENTIAL_REFUSED"},
		{name: "a read-only broker token", door: door, headers: func(t *testing.T) http.Header {
			const token = "a-read-only-token-at-the-door"
			broker.Says(token, readOnlyToken(caller.jkt))
			return door.presenting(t, caller, token)
		}, status: http.StatusForbidden, code: "READ_ONLY_TOKEN"},
		{name: "a broker token that names no session", door: door, headers: func(t *testing.T) http.Header {
			const token = "a-machine-logins-own-token-at-the-door"
			broker.Says(token, machineLoginToken(caller.jkt))
			return door.presenting(t, caller, token)
		}, status: http.StatusUnauthorized, code: "BROKER_TOKEN_NOT_A_SESSION"},
		{name: "a DPoP token with no proof", door: door, headers: func(*testing.T) http.Header {
			return http.Header{"Authorization": {"DPoP " + live("a-live-token-with-no-proof")}}
		}, status: http.StatusUnauthorized, code: "BROKER_TOKEN_PROOF_INVALID"},
		{name: "a spent proof", door: door, headers: func(t *testing.T) http.Header {
			// The proof of an upgrade the door admitted, presented again at the same door.
			spent := door.presenting(t, caller, live("a-token-whose-upgrade-spent-its-proof"))
			if admitted := door.knock(t, spent); !admitted.opened {
				t.Errorf("the upgrade whose proof the row presents again answered %d %s, want 101: only an admitted upgrade spends its proof",
					admitted.status, strings.TrimSpace(admitted.body))
			}
			return spent
		}, status: http.StatusUnauthorized, code: "BROKER_TOKEN_PROOF_INVALID"},
		{name: "a proof naming another target", door: door, headers: func(t *testing.T) http.Header {
			return presentedHeaders(caller.presenting(t, http.MethodGet, liveSocketPattern, live("a-token-proved-for-another-target"), nil))
		}, status: http.StatusUnauthorized, code: "BROKER_TOKEN_PROOF_INVALID"},
		{name: "a proof signed by another key", door: door, headers: func(t *testing.T) http.Header {
			return door.presenting(t, newBrokerCaller(t), live("a-token-bound-to-another-key"))
		}, status: http.StatusUnauthorized, code: "BROKER_TOKEN_KEY_MISMATCH"},
		{name: "a token the broker does not hold live", door: door, headers: func(t *testing.T) http.Header {
			return door.presenting(t, caller, "a-token-the-broker-never-issued")
		}, status: http.StatusUnauthorized, code: "BROKER_TOKEN_NOT_ACTIVE"},
		{name: "a source whose bucket is empty", door: limitedDoor, headers: func(t *testing.T) http.Header {
			// The bucket's one introspection goes on a token the broker does not hold live, which
			// gives nothing back (Verifier.known), sent from this same address through the API, so
			// the bucket is empty whatever the door does with a token.
			spend, err := http.NewRequestWithContext(t.Context(), http.MethodGet, limited.server.URL+"/api/v1/whoami", nil)
			if err != nil {
				t.Fatalf("build the request that empties the bucket: %v", err)
			}
			spend.Header = presentedHeaders(caller.presenting(t, http.MethodGet, "/api/v1/whoami", "a-token-that-empties-the-bucket", nil))
			response, err := limited.server.Client().Do(spend)
			if err != nil {
				t.Fatalf("GET /api/v1/whoami to empty the bucket: %v", err)
			}
			defer response.Body.Close()
			var refusal refusalAnswer
			if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil || response.StatusCode != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_NOT_ACTIVE" {
				t.Fatalf("the request that empties the bucket answered %d %+v (%v), want 401 BROKER_TOKEN_NOT_ACTIVE", response.StatusCode, refusal, err)
			}
			return limitedDoor.presenting(t, caller, "a-token-the-empty-bucket-turns-away")
		}, status: http.StatusTooManyRequests, code: "BROKER_TOKEN_RATE_LIMITED", retryAfter: "60"},
		{name: "a broker that does not answer", door: unansweredDoor, headers: func(t *testing.T) http.Header {
			return unansweredDoor.presenting(t, caller, "a-token-no-broker-answers-for")
		}, status: http.StatusServiceUnavailable, code: "BROKER_UNAVAILABLE"},
		{name: "a DPoP token on a server with no broker configured", door: pooledDoor, headers: func(t *testing.T) http.Header {
			return pooledDoor.presenting(t, caller, "a-token-nobody-here-decides")
		}, status: http.StatusUnauthorized, code: "UNAUTHORIZED"},
	} {
		t.Run(row.name, func(t *testing.T) {
			headers := row.headers(t)
			answer := row.door.knock(t, headers)
			if answer.opened {
				t.Fatalf("the socket opened (%d), want the upgrade refused %d %s", answer.status, row.status, row.code)
			}
			var refusal refusalAnswer
			if err := json.Unmarshal([]byte(answer.body), &refusal); err != nil || answer.status != row.status || refusal.Code != row.code {
				t.Errorf("answered %d %s, want %d %s as JSON", answer.status, strings.TrimSpace(answer.body), row.status, row.code)
			}
			if named := answer.header.Get(docs.ProcessHeader); named != row.door.process {
				t.Errorf("the refusal names the process %q in %s, want %q, the one this server's document service named on its 101", named, docs.ProcessHeader, row.door.process)
			}
			for _, credential := range credentialsIn(headers) {
				if strings.Contains(answer.body, credential) {
					t.Errorf("the refusal repeats the credential %q the upgrade carried: %s", credential, answer.body)
				}
			}
			if row.retryAfter != "" && answer.header.Get("Retry-After") != row.retryAfter {
				t.Errorf("Retry-After = %q, want %q, the bucket's refill in whole seconds", answer.header.Get("Retry-After"), row.retryAfter)
			}
		})
	}
}
