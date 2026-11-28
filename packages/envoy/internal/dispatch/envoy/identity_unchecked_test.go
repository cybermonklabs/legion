package envoy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// uncheckedIdentityAnswer is the 503 the listener gives an identity token it could not check
// (cmd/listener's refuseIdentityToken): its issuer is not read yet.
const uncheckedIdentityAnswer = `{"code":"identity_unchecked","error":"this identity token could not be checked; try again in a minute"}`

// uncheckedListener is a listener stand-in that answers the first unchecked sends 503 with
// Retry-After: 1 and body, then answers every send as one the stream stored. It records each
// send's Authorization and when it came.
type uncheckedListener struct {
	unchecked int
	body      string

	mu        sync.Mutex
	presented []string
	at        []time.Time
}

func (l *uncheckedListener) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.presented = append(l.presented, r.Header.Get("Authorization"))
	l.at = append(l.at, time.Now())
	tries := len(l.presented)
	l.mu.Unlock()
	if tries <= l.unchecked {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(l.body))
		return
	}
	_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1","state":"connected"}`))
}

func (l *uncheckedListener) tries() ([]string, []time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.presented...), append([]time.Time(nil), l.at...)
}

// A listener that has not read the issuer of Dispatch's identity token answers each call on it 503
// with Retry-After: 1 and the code identity_unchecked, before any route runs, until it has read
// the issuer (cmd/listener, refuseIdentityToken). The client makes such a call again on the same
// token once each Retry-After has passed, for as long as its window holds the wait: a send for a
// role that the listener answers so twice and then takes is delivered, about two seconds after it
// began, with all three tries on Dispatch's identity and none on the shared token. A listener
// that keeps answering so ends the call inside the window with that 503, as ErrUnavailable. A 503
// without that code - one a route gave after it ran - is answered after one try, as before.
func TestACallTheListenerCouldNotCheckIsMadeAgainAfterItsRetryAfter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listener-account-token")
	identity := accountToken(t, "system:serviceaccount:dispatch:example-dispatch", time.Now().Add(time.Hour))
	writeAccountToken(t, path, identity)
	send := func(t *testing.T, stand *uncheckedListener, window time.Duration) (SendResult, time.Duration, error) {
		t.Helper()
		listener := httptest.NewServer(http.HandlerFunc(stand.serve))
		t.Cleanup(listener.Close)
		client := New(listener.URL, WithAccountTokenFile(path), WithToken("shared"), WithTimeout(window),
			WithLogger(slog.New(slog.DiscardHandler)))
		began := time.Now()
		result, err := client.Send(context.Background(), SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k", Role: "example-role"})
		return result, time.Since(began), err
	}
	onTheIdentity := func(t *testing.T, presented []string) {
		t.Helper()
		for i, authorization := range presented {
			if authorization != "Bearer "+identity {
				t.Fatalf("try %d presented %q, want every try on Dispatch's identity token and none on the shared token", i+1, authorization)
			}
		}
	}

	t.Run("answered twice then taken: delivered after two waits", func(t *testing.T) {
		stand := &uncheckedListener{unchecked: 2, body: uncheckedIdentityAnswer}
		result, took, err := send(t, stand, CallWindow)
		presented, at := stand.tries()
		t.Logf("the send ended %s after it began, on try %d", took.Round(time.Millisecond), len(presented))
		if err != nil || result.EnvelopeID != "envelope-1" {
			t.Fatalf("the send = %+v, %v after %d tries in %s; want it delivered once the listener checked the token", result, err, len(presented), took)
		}
		if len(presented) != 3 {
			t.Fatalf("the listener was tried %d times, want 3: two answered 503 and the third taken", len(presented))
		}
		onTheIdentity(t, presented)
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < time.Second {
				t.Fatalf("try %d came %s after the 503 before it, want at least its Retry-After of 1s", i+1, gap)
			}
		}
		if took >= CallWindow {
			t.Fatalf("the send took %s, want it inside the client's window of %s", took, CallWindow)
		}
	})

	t.Run("answered so throughout: the call ends inside its window with the 503", func(t *testing.T) {
		const window = 3 * time.Second
		stand := &uncheckedListener{unchecked: 1 << 30, body: uncheckedIdentityAnswer}
		_, took, err := send(t, stand, window)
		presented, _ := stand.tries()
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrUnreachable) {
			t.Fatalf("a send the listener never checked = %v after %d tries, want ErrUnavailable and ErrUnreachable", err, len(presented))
		}
		if len(presented) < 2 || took >= window {
			t.Fatalf("the listener was tried %d times in %s, want at least twice, each a Retry-After apart, inside the window of %s", len(presented), took, window)
		}
		onTheIdentity(t, presented)
	})

	t.Run("a 503 without the code is answered after one try", func(t *testing.T) {
		stand := &uncheckedListener{unchecked: 1, body: `{"error":"envoy: the message bus did not answer"}`}
		_, _, err := send(t, stand, CallWindow)
		if presented, _ := stand.tries(); len(presented) != 1 || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a send a route answered 503 = %v after %d tries, want ErrUnavailable after one", err, len(presented))
		}
	})
}
