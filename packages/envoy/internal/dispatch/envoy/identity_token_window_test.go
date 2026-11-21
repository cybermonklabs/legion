package envoy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/googleidentity"
)

// A registry read is bounded by the client's window, and so is the fetch of the identity token it
// presents (Client.do, Client.window, authorize). A Google metadata server that never answers
// would otherwise hold the read for a mint's own wait (googleMintTimeout, 10 s) on each of its two
// tries, 20 s against the window. Here it ends within the window as ErrAccountToken, the listener
// never asked: the outbox's thread-rule read of Envoy's registry has a bound of its own, and a
// hung token fetch must not hold it past that bound (registryUnanswered in
// internal/dispatch/outbox).
func TestAHungIdentityTokenFetchEndsWithinTheClientsReadWindowAsErrAccountToken(t *testing.T) {
	const window = 200 * time.Millisecond
	stop := make(chan struct{})
	var asked atomic.Int32
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	// Cleanups run last first: the handler is let go before the server waits for it to return.
	t.Cleanup(metadata.Close)
	t.Cleanup(func() { close(stop) })
	t.Setenv(googleidentity.MetadataHostVar, strings.TrimPrefix(metadata.URL, "http://"))
	var reached atomic.Int32
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte(`{"session_id":"s1","state":"live"}`))
	}))
	t.Cleanup(listener.Close)

	client := New(listener.URL, WithGoogleIdentity("envoy-listener"), WithTimeout(window))
	start := time.Now()
	_, err := client.Session(context.Background(), "s1")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrAccountToken) {
		t.Fatalf("a registry read whose identity token the metadata server never mints = %v, want ErrAccountToken", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("a registry read whose identity token the metadata server never mints took %s, want it to end within the client's %s window: the token fetch is bounded by that window", elapsed, window)
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the listener was asked %d times, want none: no token could be presented", n)
	}
	if n := asked.Load(); n == 0 {
		t.Fatalf("the metadata server was never asked for a token, so the read did not wait on a hung fetch at all")
	}
}
