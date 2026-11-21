package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// An issuer that accepts the connection and never answers holds a discovery attempt for its own
// 10 s bound, as long as the 10 s the listener's server gives a route to write its answer. A token
// of that issuer sent through that real server (newHTTPServer) must still be answered 503 with
// Retry-After: 1, never with a closed connection: by the request that starts the attempt, and by
// one that joins an attempt already under way, on both identity branches. The request waits for
// discovery only until identityBudget after its check began (identityCheck). A listener that takes
// only Google identity tokens sends every JWT-shaped bearer to that branch.
func TestAnIdentityTokenOfASilentIssuerIsAnswered503(t *testing.T) {
	for _, branch := range []string{"a projected token", "a Google identity token"} {
		for _, joins := range []bool{false, true} {
			name := branch + ", the request starts the attempt"
			if joins {
				name = branch + ", the request joins an attempt under way"
			}
			t.Run(name, func(t *testing.T) {
				issuer, accepted := silentIssuer(t)
				verifier := oidc.NewLazyVerifier(issuer, listenerTestAudience)
				auth := listenerAPIAuth{token: "example-shared-token", logger: logging.NewWithWriter("silent-issuer", &bytes.Buffer{})}
				if branch == "a projected token" {
					auth.verifier = verifier
				} else {
					auth.google = &googleIdentities{audience: listenerTestAudience, verifier: verifier, accounts: []string{listenerGoogleAccount}}
				}
				if joins {
					// The background read dials the issuer, which accepts and then says nothing:
					// once it has accepted, an attempt is under way for the request to join.
					go verifier.Run(t.Context())
					select {
					case <-accepted:
					case <-time.After(5 * time.Second):
						t.Fatal("the background discovery never dialed the silent issuer within 5 s")
					}
				}
				status, retryAfter, elapsed, err := sendThroughTheServer(t, auth, unsignedToken(issuer, ""))
				if err != nil {
					t.Fatalf("%s of a silent issuer: no answer after %s: %v; want 503 with Retry-After: 1", branch, elapsed.Round(time.Millisecond), err)
				}
				if status != http.StatusServiceUnavailable || retryAfter != "1" {
					t.Fatalf("%s of a silent issuer: status %d, Retry-After %q after %s; want 503 with Retry-After: 1", branch, status, retryAfter, elapsed.Round(time.Millisecond))
				}
				if elapsed > identityBudget+2*time.Second {
					t.Fatalf("the 503 came after %s, want it within identityBudget (%s)", elapsed.Round(time.Millisecond), identityBudget)
				}
			})
		}
	}
}

// An issuer whose discovery document answers at once and names a key set that accepts the
// connection and never answers: discovery succeeds, so the request waits for no discovery, and the
// key-set read is what would hold it, for oidc.DefaultKeysFetchTimeout, as long as the write
// deadline. The listener waits for the key set only within identityBudget (oidc.WithKeysDeadline),
// so the token is answered 503 with Retry-After: 1 within the deadline, never with a closed
// connection.
func TestAProjectedTokenOfASilentKeySetIsAnswered503(t *testing.T) {
	keySet, _ := silentIssuer(t)
	var issuer string
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q,"authorization_endpoint":%q,"id_token_signing_alg_values_supported":["RS256"]}`,
			issuer, keySet+"/keys", issuer+"/auth")
	}))
	t.Cleanup(discovery.Close)
	issuer = discovery.URL
	verifier := oidc.NewLazyVerifier(issuer, listenerTestAudience)
	auth := listenerAPIAuth{token: "example-shared-token", verifier: verifier, logger: logging.NewWithWriter("silent-key-set", &bytes.Buffer{})}

	status, retryAfter, elapsed, err := sendThroughTheServer(t, auth, unsignedToken(issuer, "example-key"))
	if err != nil {
		t.Fatalf("a projected token whose key set never answers: no answer after %s: %v (discovered: %t); want 503 with Retry-After: 1",
			elapsed.Round(time.Millisecond), err, verifier.Discovered())
	}
	if status != http.StatusServiceUnavailable || retryAfter != "1" {
		t.Fatalf("a projected token whose key set never answers: status %d, Retry-After %q after %s; want 503 with Retry-After: 1",
			status, retryAfter, elapsed.Round(time.Millisecond))
	}
	if elapsed > identityBudget+2*time.Second {
		t.Fatalf("the 503 came after %s, want it within identityBudget (%s)", elapsed.Round(time.Millisecond), identityBudget)
	}
}

// Discovery and the key read each finish inside their own bounds, 3 s and 7 s, but one after the
// other they pass the 10 s the server gives a route to write its answer: the request must not wait
// for both. Its check ends within identityBudget with the 503, and the key read it started carries
// on in the background and is kept, so a later request verifies against the set it read.
func TestAProjectedTokenOfASlowIssuerIsAnsweredWithinTheWriteDeadline(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "slow-issuer-key")
	issuer.DelayDiscovery(3 * time.Second)
	issuer.DelayKeys(7 * time.Second)
	verifier := oidc.NewLazyVerifier(issuer.URL(), listenerTestAudience)
	auth := listenerAPIAuth{token: "example-shared-token", verifier: verifier, logger: logging.NewWithWriter("slow-issuer", &bytes.Buffer{})}
	token := issuer.Mint(t, key, issuer.Claims(listenerTestSubject, listenerTestAudience))

	status, retryAfter, elapsed, err := sendThroughTheServer(t, auth, token)
	if err != nil {
		t.Fatalf("a projected token of an issuer 3 s slow to discover and 7 s slow to give its keys: no answer after %s: %v; want 503 with Retry-After: 1 within identityBudget",
			elapsed.Round(time.Millisecond), err)
	}
	if status != http.StatusServiceUnavailable || retryAfter != "1" || elapsed > identityBudget+2*time.Second {
		t.Fatalf("a projected token of a slow issuer: status %d, Retry-After %q after %s; want 503 with Retry-After: 1 within identityBudget (%s)",
			status, retryAfter, elapsed.Round(time.Millisecond), identityBudget)
	}
	// The key read the first request started is still under way; once it lands, the token is
	// admitted with no wait for it.
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, _, elapsed, err = sendThroughTheServer(t, auth, token)
		if err == nil && status == http.StatusNoContent {
			break
		}
		if err != nil || status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("a projected token once the slow key set was read: status %d after %s, err %v; want 204 within 15 s", status, elapsed.Round(time.Millisecond), err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// During a key rotation: key A is cached, the issuer publishes key B, and its key set has become
// slow (9 s, more than two budgets and under the read's own 10 s limit). The first token signed by
// B opens the miss window and starts the read that would find B, and is answered 503 within
// identityBudget. A second B token while that read is still under way joins it, and is answered
// 503 at its own budget too, never the 401 of a signature no published key makes: no read has said
// whether B is published. Once the read lands, a B token is admitted.
func TestANewKeyIDDuringASlowReadIsAnswered503(t *testing.T) {
	issuer := oidctest.New(t)
	keyA := issuer.PublishKey(t, "rotation-key-a")
	verifier := oidc.NewLazyVerifier(issuer.URL(), listenerTestAudience)
	auth := listenerAPIAuth{token: "example-shared-token", verifier: verifier, logger: logging.NewWithWriter("rotation", &bytes.Buffer{})}
	claims := issuer.Claims(listenerTestSubject, listenerTestAudience)
	if status, _, _, err := sendThroughTheServer(t, auth, issuer.Mint(t, keyA, claims)); err != nil || status != http.StatusNoContent {
		t.Fatalf("a token signed by the cached key A: status %d, err %v; want 204", status, err)
	}
	keyB := issuer.PublishKey(t, "rotation-key-b")
	issuer.DelayKeys(9 * time.Second)
	tokenB := issuer.Mint(t, keyB, claims)

	for _, which := range []string{"the first token signed by B", "a second token signed by B, the read still under way"} {
		status, retryAfter, elapsed, err := sendThroughTheServer(t, auth, tokenB)
		if err != nil || status != http.StatusServiceUnavailable || retryAfter != "1" {
			t.Fatalf("%s: status %d, Retry-After %q after %s, err %v; want 503 with Retry-After: 1, not 401",
				which, status, retryAfter, elapsed.Round(time.Millisecond), err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, _, elapsed, err := sendThroughTheServer(t, auth, tokenB)
		if err == nil && status == http.StatusNoContent {
			break
		}
		if err != nil || status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("a token signed by B once the read has landed: status %d after %s, err %v; want 204 within 15 s", status, elapsed.Round(time.Millisecond), err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sendThroughTheServer sends GET /v1/sessions with bearer to the listener's real server over
// apiAuth with auth, and answers the status and Retry-After it got and how long it took, or the
// error of a request that got no answer.
func sendThroughTheServer(t *testing.T, auth listenerAPIAuth, bearer string) (status int, retryAfter string, elapsed time.Duration, err error) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	server := newHTTPServer(apiAuth(auth, next))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	request, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/v1/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	start := time.Now()
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	elapsed = time.Since(start)
	if err != nil {
		return 0, "", elapsed, err
	}
	_ = response.Body.Close()
	return response.StatusCode, response.Header.Get("Retry-After"), elapsed, nil
}

// silentIssuer is the URL of a server that accepts every connection and never answers, held open
// until the test ends, and a channel that is closed once it has accepted its first connection.
func silentIssuer(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
			once.Do(func() { close(accepted) })
		}
	}()
	t.Cleanup(func() {
		_ = silent.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return "http://" + silent.Addr().String(), accepted
}

// unsignedToken is a JWT-shaped token naming issuer, the listener's test audience, a subject and
// an hour's expiry, with kid in its header when kid is not empty, signed by nothing: enough to
// reach an identity branch, and to be classified there as a token the issuer could not be read
// for (oidc.ErrIssuerUnavailable) rather than refused for its claims.
func unsignedToken(issuer, kid string) string {
	encode := base64.RawURLEncoding.EncodeToString
	header := `{"alg":"RS256","typ":"JWT"}`
	if kid != "" {
		header = fmt.Sprintf(`{"alg":"RS256","typ":"JWT","kid":%q}`, kid)
	}
	payload := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":"system:serviceaccount:example-ns:example-caller","email":%q,"email_verified":true,"exp":%d}`,
		issuer, listenerTestAudience, listenerGoogleAccount, time.Now().Add(time.Hour).Unix())
	return encode([]byte(header)) + "." + encode([]byte(payload)) + "." + encode([]byte("signature"))
}
