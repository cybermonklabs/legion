package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/testwait"
)

// metadataServer stands in for a Google machine's metadata server (googleidentity.MetadataHostVar
// names it): the identity path answers a token for the audience asked, and the email path names
// the machine's service account, each with Metadata-Flavor: Google. It counts the tokens it mints.
type metadataServer struct {
	minted atomic.Int32
	token  string
}

func (m *metadataServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Metadata-Flavor") != "Google" {
		http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
		return
	}
	w.Header().Set("Metadata-Flavor", "Google")
	switch r.URL.Path {
	case "/computeMetadata/v1/instance/service-accounts/default/identity":
		m.minted.Add(1)
		_, _ = w.Write([]byte(m.token))
	case "/computeMetadata/v1/instance/service-accounts/default/email":
		_, _ = w.Write([]byte("dispatch@example-project.iam.gserviceaccount.com"))
	default:
		http.NotFound(w, r)
	}
}

// sendStand is a listener stand-in recording each send's Authorization and role mark.
type sendStand struct {
	mu    sync.Mutex
	calls []identityCall
}

func (s *sendStand) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.calls = append(s.calls, identityCall{authorization: r.Header.Get("Authorization"), role: body["role"]})
	s.mu.Unlock()
	_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1","state":"connected"}`))
}

func (s *sendStand) taken() []identityCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.calls
	s.calls = nil
	return calls
}

// closedHost is a loopback host and port nothing listens on: a machine with no metadata server.
func closedHost(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	host := listener.Addr().String()
	_ = listener.Close()
	return host
}

// On Google Cloud, Dispatch presents its machine's service account (WithGoogleIdentity): the
// metadata server mints an identity token for the listener's audience, which Dispatch keeps until
// shortly before its own exp, and a send for a role is made on it, marked. A metadata server that
// does not answer at a call fails that call as ErrAccountToken, which the delivery tries again,
// never as ErrNoListenerIdentity and never on the shared token; a send with no role mark - the control -
// is made on the shared token. Whether the machine has a metadata server at all is decided at
// Dispatch's start (CheckGoogleIdentity): a machine with none refuses to start, naming the
// setting, and one with a metadata server starts.
func TestDispatchsGoogleIdentityMarksARoleSendAndFailsClosedWithoutOne(t *testing.T) {
	stand := &sendStand{}
	listener := httptest.NewServer(http.HandlerFunc(stand.serve))
	defer listener.Close()
	marked := SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: "reviewer"}
	unmarked := SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k"}

	t.Run("a machine with a metadata server: the send for a role is made on the minted token, marked, and the token is kept", func(t *testing.T) {
		metadata := &metadataServer{token: accountToken(t, "dispatch@example-project.iam.gserviceaccount.com", time.Now().Add(time.Hour))}
		server := httptest.NewServer(http.HandlerFunc(metadata.serve))
		defer server.Close()
		t.Setenv(googleidentity.MetadataHostVar, strings.TrimPrefix(server.URL, "http://"))
		if err := CheckGoogleIdentity(context.Background()); err != nil {
			t.Fatalf("control: the start-time check on a machine with a metadata server: %v", err)
		}
		client := New(listener.URL, WithGoogleIdentity("envoy-listener"), WithToken("shared"))
		for range 2 {
			if _, err := client.Send(context.Background(), marked); err != nil {
				t.Fatalf("a send for a role on the Google identity: %v", err)
			}
		}
		got := stand.taken()
		if len(got) != 2 || got[0].authorization != "Bearer "+metadata.token || got[0].role != "reviewer" || got[1].authorization != "Bearer "+metadata.token {
			t.Fatalf("the listener was called %+v, want two sends on the minted token, marked reviewer", got)
		}
		if n := metadata.minted.Load(); n != 1 {
			t.Fatalf("the metadata server minted %d tokens for two sends, want 1, kept until near its exp", n)
		}
	})

	t.Run("a metadata server that does not answer for a moment: the send for a role fails as ErrAccountToken, never as no identity, and is not made on the shared token", func(t *testing.T) {
		t.Setenv(googleidentity.MetadataHostVar, closedHost(t))
		var logs bytes.Buffer
		client := New(listener.URL, WithGoogleIdentity("envoy-listener"), WithToken("shared"), WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
		_, err := client.Send(context.Background(), marked)
		if !errors.Is(err, ErrAccountToken) || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNoListenerIdentity) {
			t.Fatalf("a send for a role while the metadata server does not answer = %v, want ErrAccountToken (retried), not ErrNoListenerIdentity", err)
		}
		if got := stand.taken(); len(got) != 0 {
			t.Fatalf("the listener was called %+v for a send for a role with no token minted, want no call", got)
		}
		if _, err := client.Send(context.Background(), unmarked); err != nil {
			t.Fatalf("control: a send with no role mark while the metadata server does not answer: %v", err)
		}
		if got := stand.taken(); len(got) != 1 || got[0].authorization != "Bearer shared" || got[0].role != nil {
			t.Fatalf("control: the listener was called %+v, want one unmarked send on the shared token", got)
		}
	})

	t.Run("a machine with no metadata server refuses to start, naming the setting", func(t *testing.T) {
		t.Setenv(googleidentity.MetadataHostVar, closedHost(t))
		err := CheckGoogleIdentity(context.Background())
		if !errors.Is(err, googleidentity.ErrNoMetadataServer) || !strings.Contains(err.Error(), "ENVOY_ACCOUNT_GOOGLE_AUDIENCE") {
			t.Fatalf("the start-time check with no metadata server = %v, want ErrNoMetadataServer naming ENVOY_ACCOUNT_GOOGLE_AUDIENCE", err)
		}
	})
}

// A Google identity token the listener refused (401) is dropped, and the second try presents one
// the metadata server mints again, so a token the listener stopped taking - its key rotated, say -
// is not presented twice. Concurrent callers that find no live token share one mint
// (googleIdentity.token), each waiting on its own context: one whose context ends early gets
// ErrAccountToken without waiting out the mint, and the others get the minted token.
func TestAGoogleIdentityTokenTheListenerRefusedIsMintedAgainAndMintsAreShared(t *testing.T) {
	t.Run("a token refused 401 is dropped and a fresh one is presented on the second try", func(t *testing.T) {
		first := accountToken(t, "dispatch@example-project.iam.gserviceaccount.com", time.Now().Add(time.Hour))
		second := accountToken(t, "dispatch@example-project.iam.gserviceaccount.com", time.Now().Add(2*time.Hour))
		var minted atomic.Int32
		metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Metadata-Flavor", "Google")
			if minted.Add(1) == 1 {
				_, _ = w.Write([]byte(first))
				return
			}
			_, _ = w.Write([]byte(second))
		}))
		defer metadata.Close()
		t.Setenv(googleidentity.MetadataHostVar, strings.TrimPrefix(metadata.URL, "http://"))
		var seen []string
		var mu sync.Mutex
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Header.Get("Authorization"))
			mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+second {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1","state":"connected"}`))
		}))
		defer listener.Close()
		client := New(listener.URL, WithGoogleIdentity("envoy-listener"))
		if _, err := client.Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: "reviewer"}); err != nil {
			t.Fatalf("a send whose first Google token the listener refused: %v", err)
		}
		if len(seen) != 2 || seen[0] != "Bearer "+first || seen[1] != "Bearer "+second {
			t.Fatalf("the listener saw %d tries, want the first token then a freshly minted one", len(seen))
		}
	})

	t.Run("concurrent callers share one mint, and a caller whose context ends stops waiting", func(t *testing.T) {
		token := accountToken(t, "dispatch@example-project.iam.gserviceaccount.com", time.Now().Add(time.Hour))
		release := make(chan struct{})
		var releasing sync.Once
		open := func() { releasing.Do(func() { close(release) }) }
		var minted atomic.Int32
		metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			minted.Add(1)
			<-release
			w.Header().Set("Metadata-Flavor", "Google")
			_, _ = w.Write([]byte(token))
		}))
		defer metadata.Close()
		// Deferred after the server's Close, so it runs first: Close waits for the handler the
		// mint holds, which returns once release closes.
		defer open()
		t.Setenv(googleidentity.MetadataHostVar, strings.TrimPrefix(metadata.URL, "http://"))
		identity := &googleIdentity{audience: "envoy-listener"}
		// The first caller's context ends at the moment the row chooses: once its mint has reached
		// the metadata server, so the caller is waiting on a mint in flight.
		short, cancel := context.WithCancel(context.Background())
		defer cancel()
		shortAnswer := make(chan error, 1)
		go func() {
			_, err := identity.token(short, time.Now())
			shortAnswer <- err
		}()
		testwait.Eventually(t, "the first caller's mint to reach the metadata server", func() bool { return minted.Load() == 1 })
		cancel()
		if err := testwait.Receive(t, "the caller whose context ended during the mint to stop waiting", shortAnswer); !errors.Is(err, ErrAccountToken) {
			t.Fatalf("a caller whose context ended during the mint = %v, want ErrAccountToken", err)
		}
		results := make(chan string, 3)
		for range 3 {
			go func() {
				got, err := identity.token(context.Background(), time.Now())
				if err != nil {
					got = "error: " + err.Error()
				}
				results <- got
			}()
		}
		testwait.Span(t, 100*time.Millisecond, sharedMintWhy)
		open()
		for caller := range 3 {
			if got := testwait.Receive(t, fmt.Sprintf("caller %d of the shared mint to answer once the mint is released", caller+1), results); got != token {
				t.Fatalf("a caller waiting on the shared mint got %q, want the minted token", got)
			}
		}
		if n := minted.Load(); n != 1 {
			t.Fatalf("the metadata server was asked %d times by four callers, want once", n)
		}
	})
}

// sharedMintWhy is why the shared-mint row holds a span before it releases the mint: the three
// callers must be waiting on the mint in flight, so that one which minted on its own would ask the
// metadata server a second time.
const sharedMintWhy = "a caller waiting on the mint in flight leaves no state to read: googleIdentity counts no waiters " +
	"on its mint, the mint does real HTTP I/O to the metadata stand-in, which synctest cannot hold, and no later " +
	"action is ordered behind a caller's wait"
