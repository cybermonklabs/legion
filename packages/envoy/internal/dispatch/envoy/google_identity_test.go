package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// never as ErrNoIdentity and never on the shared token; a send with no role mark - the control -
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
		if !errors.Is(err, ErrAccountToken) || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNoIdentity) {
			t.Fatalf("a send for a role while the metadata server does not answer = %v, want ErrAccountToken (retried), not ErrNoIdentity", err)
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
