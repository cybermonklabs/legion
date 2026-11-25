// packages/envoy/internal/cipherclient/tokentransport_test.go

package cipherclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAnInactiveTokenIsDroppedAndTradedAgain: a server that answers one 401
// BROKER_TOKEN_NOT_ACTIVE, as Dispatch answers a token the broker ended before it lapsed, makes the
// next request carry a token traded afresh rather than the one it refused, which the client would
// otherwise present until its renewal came due. The refusal reaches the caller as the server
// wrote it, body and all.
func TestAnInactiveTokenIsDroppedAndTradedAgain(t *testing.T) {
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "token-" + strconv.Itoa(n), "token_type": "DPoP", "expires_in": 3600}
	})
	const refusal = `{"code":"BROKER_TOKEN_NOT_ACTIVE","error":"the secrets broker does not hold this access token as live"}`
	var mu sync.Mutex
	var presented []string
	server := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		presented = append(presented, r.Header.Get("Authorization"))
		first := len(presented) == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, refusal)
			return
		}
		_, _ = io.WriteString(w, `{"kind":"agent"}`)
	})
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub()})
	if err != nil {
		t.Fatal(err)
	}
	caller := &http.Client{Transport: NewTokenTransport(client, http.DefaultTransport)}

	if status, body := presentTo(t, caller, server+"/api/v1/whoami"); status != http.StatusUnauthorized || body != refusal {
		t.Fatalf("the first request was answered %d %q, want the server's 401 %q as it wrote it", status, body, refusal)
	}
	if status, body := presentTo(t, caller, server+"/api/v1/whoami"); status != http.StatusOK || body != `{"kind":"agent"}` {
		t.Fatalf("the second request was answered %d %q, want 200", status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(presented) != 2 || presented[0] != "DPoP token-0" || presented[1] != "DPoP token-1" {
		t.Fatalf("the server was presented %q, want DPoP token-0 and then the token traded after its refusal, DPoP token-1", presented)
	}
	if trades := len(broker.Requests()); trades != 2 {
		t.Fatalf("the client traded %d times, want 2: once for each token", trades)
	}
}

// TestAFailedTradeSendsNothing: a request whose token the broker will not trade for answers the
// broker's refusal and never reaches the server, so no request goes out without a token.
func TestAFailedTradeSendsNothing(t *testing.T) {
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_grant", "error_description": "no entry lists this account"}
	})
	var sent atomic.Int64
	server := newRawServer(t, func(http.ResponseWriter, *http.Request) { sent.Add(1) })
	caller := &http.Client{Transport: NewTokenTransport(refusalClient(t, broker), nil)}

	response, err := caller.Get(server + "/api/v1/whoami")
	if err == nil {
		response.Body.Close()
	}
	var refused *RefusedError
	if !errors.As(err, &refused) || !refused.HasCode("invalid_grant") || !refused.DescriptionHasPrefix("no entry lists this account") {
		t.Fatalf("the request answered %v, want the broker's refusal", err)
	}
	if n := sent.Load(); n != 0 {
		t.Fatalf("the server received %d requests, want none: the trade failed", n)
	}
}

// TestATokenTransportPresentsTheLiveTokenWhileARenewalHangs: a client whose token is inside its
// renewal window, and whose renewal loop is stuck on a trade the broker never answers, still sends
// each request at once, presenting the token in hand, which is good for minutes yet, rather than
// queue behind a renewal the request does not need.
func TestATokenTransportPresentsTheLiveTokenWhileARenewalHangs(t *testing.T) {
	hanging, release := make(chan struct{}), make(chan struct{})
	var hang sync.Once
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		if n == 0 {
			// Two minutes: inside DefaultRenewBefore from the moment it lands.
			return 200, map[string]any{"access_token": "token-0", "token_type": "DPoP", "expires_in": 120}
		}
		hang.Do(func() { close(hanging) })
		<-release
		return 503, map[string]any{"error": "unavailable"}
	})
	// The stub's server waits for its handlers when it closes, so the hung trade is released first:
	// this cleanup, registered after the stub's own, runs before it.
	t.Cleanup(func() { close(release) })
	var mu sync.Mutex
	var presented []string
	server := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		presented = append(presented, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"kind":"agent"}`)
	})
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Token(t.Context()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	// The renewal loop trades at once, the token in hand being inside its renewal window, and
	// hangs there.
	go func() { _ = client.Run(t.Context(), func(context.Context, *Token) error { return nil }) }()
	select {
	case <-hanging:
	case <-time.After(10 * time.Second):
		t.Fatal("the renewal loop never asked the broker for the next token")
	}

	caller := &http.Client{Transport: NewTokenTransport(client, nil)}
	const requests = 3
	for range requests {
		answered := make(chan error, 1)
		go func() {
			response, err := caller.Get(server + "/api/v1/whoami")
			if err == nil {
				response.Body.Close()
			}
			answered <- err
		}()
		select {
		case err := <-answered:
			if err != nil {
				t.Fatalf("the request answered %v, want the server's answer", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the request waited on the hung renewal, though the token in hand is live for minutes yet")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(presented) != requests {
		t.Fatalf("the server received %d requests, want %d", len(presented), requests)
	}
	for _, authorization := range presented {
		if authorization != "DPoP token-0" {
			t.Fatalf("the server was presented %q, want each request to carry the token in hand, DPoP token-0", presented)
		}
	}
	if trades := len(broker.Requests()); trades != 2 {
		t.Fatalf("the broker saw %d trades, want 2: the first, and the renewal that hangs", trades)
	}
}

// presentTo sends GET target through caller, answering the status and the body.
func presentTo(t *testing.T, caller *http.Client, target string) (int, string) {
	t.Helper()
	response, err := caller.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer of GET %s: %v", target, err)
	}
	return response.StatusCode, string(body)
}
