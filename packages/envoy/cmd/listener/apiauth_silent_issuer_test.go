package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
)

// An issuer that accepts the connection and never answers holds a discovery attempt for its own
// 10 s bound, as long as the 10 s the listener's server gives a route to write its answer. A
// projected token sent through that real server (newHTTPServer) must still be answered 503 with
// Retry-After: 1, never with a closed connection, both by the request that starts the attempt and
// by one that joins an attempt already under way: the request waits for discovery only
// discoveryWait.
func TestAProjectedTokenOfASilentIssuerIsAnswered503(t *testing.T) {
	for _, joinEarly := range []bool{false, true} {
		name := "the request starts the attempt"
		if joinEarly {
			name = "the request joins an attempt under way"
		}
		t.Run(name, func(t *testing.T) {
			issuer := silentIssuer(t)
			verifier := oidc.NewLazyVerifier(issuer, listenerTestAudience)
			if joinEarly {
				go verifier.Run(t.Context())
				time.Sleep(time.Second)
			}
			var logged bytes.Buffer
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			auth := listenerAPIAuth{token: "example-shared-token", verifier: verifier, logger: logging.NewWithWriter("silent-issuer", &logged)}
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
			request.Header.Set("Authorization", "Bearer "+unsignedProjectedToken(issuer))
			start := time.Now()
			response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("a projected token of a silent issuer: no answer after %s: %v; want 503 with Retry-After: 1", elapsed.Round(time.Millisecond), err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" {
				t.Fatalf("a projected token of a silent issuer: status %d, Retry-After %q after %s; want 503 with Retry-After: 1",
					response.StatusCode, response.Header.Get("Retry-After"), elapsed.Round(time.Millisecond))
			}
			if elapsed > discoveryWait+2*time.Second {
				t.Fatalf("the 503 came after %s, want it within discoveryWait (%s)", elapsed.Round(time.Millisecond), discoveryWait)
			}
		})
	}
}

// silentIssuer is the URL of an issuer that accepts every connection and never answers, held open
// until the test ends.
func silentIssuer(t *testing.T) string {
	t.Helper()
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
	return "http://" + silent.Addr().String()
}

// unsignedProjectedToken is a JWT-shaped token naming issuer and the listener's audience, signed
// by nothing: enough to reach the projected-token branch, which cannot judge it before discovery.
func unsignedProjectedToken(issuer string) string {
	encode := base64.RawURLEncoding.EncodeToString
	payload := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":"system:serviceaccount:example-ns:example-caller","exp":%d}`,
		issuer, listenerTestAudience, time.Now().Add(time.Hour).Unix())
	return encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode([]byte(payload)) + "." + encode([]byte("signature"))
}
