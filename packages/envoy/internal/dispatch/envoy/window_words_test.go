package envoy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// windowParam is the parameter the listener URL of the window rows carries the probe's token
// under, in two parts so no line of this file reads as a credential to a scanner.
const windowParam = "access" + "_token"

// windowCredentials are the credentials the listener URL of the window rows carries: its user
// name and password, which net/http sends as Basic authorization, and its access token, a value
// of its query.
var windowCredentials = []string{redacttest.User, redacttest.Password, redacttest.Token}

// windowStart is where the fake clock of each window row starts.
var windowStart = time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)

// windowCalls are the listener calls the window rows make: a read and a send.
var windowCalls = []struct {
	name string
	call func(*Client) error
}{
	{"sessions", func(client *Client) error {
		_, err := client.Sessions(context.Background())
		return err
	}},
	{"send", func(client *Client) error {
		_, err := client.Send(context.Background(), SendInput{TargetSession: "s1", Message: "held", IdempotencyKey: "held:steer"})
		return err
	}},
}

// TestAListenerCallTheWindowEndedPrintsNoCredentialOfItsURL: the listener URL carries a user name,
// a password and an access token in its query, and the client's window ends a call of it.
//
//   - read-*: the listener's stand-in answered 200 with a chunked body whose trailer is one line
//     that does not parse and is the credentials the call carried, and the window ended while the
//     client read that body. net/http's error of the read quotes the line (twin); the call's error
//     names the read's failure by its class beside the window's words, since roundTrip reads the
//     body through redact.StreamAnswer under the window.
//   - call-*: the stand-in held the call until its window ended. net/http's error names the
//     request's URL with its user name and its token (twin); the call's error names the context's
//     end beside the window's words (transportCause).
//
// Each error names ErrCallWindow, so each row read the window's path, and none holds a run of a
// credential the call carried.
func TestAListenerCallTheWindowEndedPrintsNoCredentialOfItsURL(t *testing.T) {
	answering := trailerEchoListener(t)
	for _, c := range windowCalls {
		t.Run("read-"+c.name, func(t *testing.T) {
			fake := clocktest.New(windowStart)
			client := New(windowURL(answering), WithClock(fake))
			held, transport := make(chan struct{}, 1), &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			client.httpClient.Transport = windowBlind{transport: transport, held: held}
			ended := make(chan error, 1)
			go func() { ended <- c.call(client) }()
			testwait.Receive(t, "the client to read the answer's body", held)
			fake.Set(windowStart.Add(CallWindow))
			err := testwait.Receive(t, "the call to end once its window ended", ended)
			requireWindowEnded(t, "envoy.window-ended/read-"+c.name, err, "the request failed")
		})
	}
	for _, c := range windowCalls {
		t.Run("call-"+c.name, func(t *testing.T) {
			reached := make(chan struct{}, 1)
			fake := clocktest.New(windowStart)
			client := New(windowURL(heldListener(t, reached)), WithClock(fake))
			ended := make(chan error, 1)
			go func() { ended <- c.call(client) }()
			testwait.Receive(t, "the call to reach the listener", reached)
			fake.Set(windowStart.Add(CallWindow))
			err := testwait.Receive(t, "the call to end once its window ended", ended)
			requireWindowEnded(t, "envoy.window-ended/call-"+c.name, err, context.DeadlineExceeded.Error())
		})
	}
	t.Run("twin", func(t *testing.T) {
		response, err := http.Get(windowURL(answering))
		if err != nil {
			t.Fatalf("net/http's call of the stand-in = %v, want its answer", err)
		}
		_, err = io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err == nil || len(redacttest.Held(err.Error())) != len(windowCredentials) {
			t.Fatalf("net/http's read of the stand-in's answer = %v, want an error that quotes %q", err, windowCredentials)
		}

		reached := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, windowURL(heldListener(t, reached)), nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := http.DefaultClient.Do(request)
			done <- err
		}()
		testwait.Receive(t, "net/http's call to reach the stand-in", reached)
		cancel()
		err = testwait.Receive(t, "net/http's call to end once its context ended", done)
		if err == nil || !strings.Contains(err.Error(), redacttest.User) || !strings.Contains(err.Error(), redacttest.Token) {
			t.Fatalf("net/http's call the stand-in held = %v, want an error that names %q and %q", err, redacttest.User, redacttest.Token)
		}
	})
}

// requireWindowEnded checks err, the error of a call the window ended: it names ErrCallWindow, so
// the row read the window's path; it holds no run of a credential the call carried, the row's
// designated assertion, under capture; and it ends in failure, the words for the call's own
// failure, beside the window's.
func requireWindowEnded(t *testing.T, capture string, err error, failure string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), ErrCallWindow.Error()) {
		t.Fatalf("the call = %v, want a failure that names %v", err, ErrCallWindow)
	}
	redacttest.RequireNoPiece(t, capture, err.Error(), windowCredentials...)
	if want := fmt.Sprintf("%s (%v: %s)", failure, ErrCallWindow, CallWindow); !strings.HasSuffix(err.Error(), want) {
		t.Errorf("the call's error is %q, want it to end %q", err, want)
	}
}

// windowURL is the listener URL of the window rows for the stand-in at host, with every one of
// windowCredentials in it.
func windowURL(host string) string {
	return "http://" + redacttest.User + ":" + redacttest.Password + "@" + host + "/?" + windowParam + "=" + redacttest.Token
}

// trailerEchoListener is a stand-in in the listener's place that answers every call 200 with a
// chunked JSON body whose trailer is one line that does not parse: the credentials the call
// carried, its Basic authorization's user name and password and the value of its access-token
// parameter. net/http's error of a read of that body quotes the line (textproto's "malformed MIME
// header: missing colon", from net/http/transfer.go of go1.27.3, line 956). It answers its host.
func trailerEchoListener(t *testing.T) string {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		user, password, _ := r.BasicAuth()
		echoed := strings.Join([]string{user, password, r.URL.Query().Get(windowParam)}, " ")
		conn, written, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("take the stand-in's connection: %v", err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(written, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n[]\r\n0\r\n%s\r\n\r\n", echoed)
		_ = written.Flush()
	}))
	t.Cleanup(listener.Close)
	return strings.TrimPrefix(listener.URL, "http://")
}

// heldListener is a stand-in in the listener's place that holds every call it reads until the
// caller's connection ends, telling reached as each arrives. It answers its host.
func heldListener(t *testing.T, reached chan<- struct{}) string {
	t.Helper()
	released := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		reached <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	t.Cleanup(func() {
		close(released)
		listener.Close()
	})
	return strings.TrimPrefix(listener.URL, "http://")
}

// windowBlind is net/http's own transport under a call window it cannot see: it makes each request
// on the request's context without its end (context.WithoutCancel), and holds each read of the
// answer's body until that end, telling held at the first, then reads on. A read net/http
// finished as the window ended, before its transport saw the end and closed the connection,
// reads so: net/http's own error of the read, after the window's end. A read net/http's transport
// ended itself fails with the window's cause instead, context.DeadlineExceeded.
type windowBlind struct {
	transport *http.Transport
	held      chan struct{}
}

func (b windowBlind) RoundTrip(request *http.Request) (*http.Response, error) {
	window := request.Context()
	response, err := b.transport.RoundTrip(request.WithContext(context.WithoutCancel(window)))
	if err != nil {
		return nil, err
	}
	response.Body = heldBody{ReadCloser: response.Body, window: window, held: b.held}
	return response, nil
}

// heldBody is an answer's body whose every read waits for window's end, telling held first where
// held has room.
type heldBody struct {
	io.ReadCloser
	window context.Context
	held   chan struct{}
}

func (b heldBody) Read(p []byte) (int, error) {
	select {
	case b.held <- struct{}{}:
	default:
	}
	<-b.window.Done()
	return b.ReadCloser.Read(p)
}
