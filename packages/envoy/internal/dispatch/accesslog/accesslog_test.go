package accesslog

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
)

// lockedBuffer is a log sink a handler's goroutines may write while the test reads it.
type lockedBuffer = logtest.Buffer

// capture sends the default logger's lines to a buffer for the test's lifetime.
func capture(t *testing.T) *lockedBuffer {
	t.Helper()
	logged := &lockedBuffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	return logged
}

// line is the one access-log line logged holds.
func line(t *testing.T, logged *lockedBuffer) string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, `msg="`+Message+`"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want one access-log line, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	return lines[0]
}

func carries(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("access-log line %q does not carry %q", got, w)
		}
	}
}

// A handler that panics, as an artifact download aborts with http.ErrAbortHandler when its store
// fails mid-stream, still writes its line, marked aborted, and the panic still reaches net/http.
func TestAPanickingHandlerStillWritesItsLine(t *testing.T) {
	logged := capture(t)
	handler := Handler("/api/v1/artifacts/{id}/files", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), KindUser, "alice")
		_, _ = w.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("the panic reaching net/http = %v, want http.ErrAbortHandler", recovered)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/x/files", nil))
	}()
	carries(t, line(t, logged), `caller_kind=user`, `caller=alice`, `status=200`, `bytes=7`, `aborted=true`)
}

// A handler that panics before it writes anything is logged as 500, though the client gets a
// closed connection rather than a status.
func TestAHandlerThatPanicsBeforeWritingIsLoggedAsA500(t *testing.T) {
	logged := capture(t)
	handler := Handler("/r", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	}()
	carries(t, line(t, logged), `status=500`, `aborted=true`)
}

// A request that completes carries no aborted field.
func TestACompletedRequestIsNotMarkedAborted(t *testing.T) {
	logged := capture(t)
	Handler("/r", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	if got := line(t, logged); strings.Contains(got, "aborted") {
		t.Fatalf("access-log line %q is marked aborted", got)
	}
}

// unwrapping is a writer layer that hands its writer on through Unwrap, as api's layers do.
type unwrapping struct{ http.ResponseWriter }

func (u unwrapping) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// hiding is a writer layer that hides what it wraps, as api.keyedAnswer does.
type hiding struct{ http.ResponseWriter }

// A request whose own context its client ended is marked closed through every layer that unwraps
// to the recorder, and its line reads 499 in place of the 500 its handler answered, which the
// client is still sent. A request whose context is live, or ended at a deadline rather than by
// its client, and a writer that hides the recorder, are not marked, and their lines keep the 500.
func TestARequestItsClientClosedIsLoggedAs499(t *testing.T) {
	for _, test := range []struct {
		name   string
		end    func(ctx context.Context) context.Context
		wrap   func(http.ResponseWriter) http.ResponseWriter
		marked bool
	}{
		{name: "its client closed it, through a layer that unwraps", end: func(ctx context.Context) context.Context {
			closed, closeClient := context.WithCancel(ctx)
			closeClient()
			return closed
		}, wrap: func(w http.ResponseWriter) http.ResponseWriter { return unwrapping{w} }, marked: true},
		{name: "its context is live", end: func(ctx context.Context) context.Context { return ctx },
			wrap: func(w http.ResponseWriter) http.ResponseWriter { return unwrapping{w} }},
		{name: "its context reached a deadline", end: func(ctx context.Context) context.Context {
			ended, cancel := context.WithDeadline(ctx, time.Unix(0, 0))
			t.Cleanup(cancel)
			return ended
		}, wrap: func(w http.ResponseWriter) http.ResponseWriter { return unwrapping{w} }},
		{name: "its client closed it, behind a layer that hides the recorder", end: func(ctx context.Context) context.Context {
			closed, closeClient := context.WithCancel(ctx)
			closeClient()
			return closed
		}, wrap: func(w http.ResponseWriter) http.ResponseWriter { return hiding{w} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			logged := capture(t)
			marked := false
			response := httptest.NewRecorder()
			Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				marked = MarkClosedByClient(test.wrap(w))
				w.WriteHeader(http.StatusInternalServerError)
			})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/r", nil).WithContext(test.end(context.Background())))
			if marked != test.marked {
				t.Errorf("MarkClosedByClient = %v, want %v", marked, test.marked)
			}
			want := "status=500 "
			if test.marked {
				want = "status=499 "
			}
			carries(t, line(t, logged), " "+want)
			if response.Code != http.StatusInternalServerError {
				t.Errorf("the client was sent %d, want the 500 the handler wrote", response.Code)
			}
		})
	}
}

// writerWithoutReadFrom is a response writer with no ReadFrom of its own.
type writerWithoutReadFrom struct {
	header http.Header
	body   bytes.Buffer
}

func (w *writerWithoutReadFrom) Header() http.Header         { return w.header }
func (w *writerWithoutReadFrom) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *writerWithoutReadFrom) WriteHeader(int)             {}

// A body streamed with io.Copy counts its bytes even when the server's writer has no ReadFrom.
func TestACopiedBodyIsCountedWithoutTheServersReadFrom(t *testing.T) {
	logged := capture(t)
	w := &writerWithoutReadFrom{header: http.Header{}}
	Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, strings.NewReader("twelve bytes"))
	})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/r", nil))
	if w.body.String() != "twelve bytes" {
		t.Fatalf("the response body = %q", w.body.String())
	}
	carries(t, line(t, logged), `bytes=12`, `status=200`)
}

// A caller resolved on a detached context names no one, so a heartbeat that resolves only the
// bearer's credential leaves the session the request named on the line.
func TestADetachedResolutionLeavesTheLinesCaller(t *testing.T) {
	logged := capture(t)
	Handler("/ws/doc/{room}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), KindSharedToken, "")
		Note(r.Context(), KindSession, "session-0123456789abcdef")
		Note(Detached(r.Context()), KindSharedToken, "")
		Note(Detached(context.Background()), KindUser, "nobody")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ws/doc/x", nil))
	carries(t, line(t, logged), `caller_kind=session`, `caller=session-0123456789abcdef`)
}

// A session named after a bearer credential keeps that credential on the line, since the session
// id is text the caller wrote; a request naming no session carries no credential field.
func TestASessionKeepsTheCredentialThatNamedIt(t *testing.T) {
	for name, test := range map[string]struct {
		first, firstID string
		session        bool
		want           []string
		absent         string
	}{
		"personal token":    {first: KindToken, firstID: "ada@example.com", session: true, want: []string{`caller_kind=session`, `credential=token:ada@example.com`}},
		"service":           {first: KindService, firstID: "system:serviceaccount:ns:sa", session: true, want: []string{`caller_kind=session`, `credential=service:system:serviceaccount:ns:sa`}},
		"shared token":      {first: KindSharedToken, session: true, want: []string{`caller_kind=session`, `credential=shared-token`}},
		"token, no session": {first: KindToken, firstID: "ada@example.com", want: []string{`caller_kind=token`, `caller=ada@example.com`}, absent: "credential="},
	} {
		t.Run(name, func(t *testing.T) {
			logged := capture(t)
			Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Note(r.Context(), test.first, test.firstID)
				if test.session {
					Note(r.Context(), KindSession, "session-0123456789abcdef")
				}
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
			got := line(t, logged)
			carries(t, got, test.want...)
			if test.absent != "" && strings.Contains(got, test.absent) {
				t.Fatalf("access-log line %q carries %q", got, test.absent)
			}
		})
	}
}

// The line names the address the load balancer saw and the user agent, so callers sharing one
// credential can still be told apart; it keeps the path escaped and never its query string.
func TestTheLineNamesTheAddressAndUserAgentAndKeepsThePathEscaped(t *testing.T) {
	logged := capture(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/a%2Fb%3Ftoken%3Dsecret?q=hidden-query", nil)
	request.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	request.Header.Set("User-Agent", "dispatch-cli/1.0")
	Handler("/api/v1/issues/{key}", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), request)
	got := line(t, logged)
	carries(t, got, `path=/api/v1/issues/a%2Fb%3Ftoken%3Dsecret `, `address=203.0.113.9`, `peer=192.0.2.1 `, `user_agent=dispatch-cli/1.0`)
	for _, forbidden := range []string{"hidden-query", "token=secret"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("access-log line %q carries %q", got, forbidden)
		}
	}
}

// A search's line names the keyed hash and the shape it noted, the last noting winning, as a
// broadened second run replaces the first run's shape; a search noted on a detached context names
// nothing on the line.
func TestASearchsLineNamesTheKeyAndShapeItNoted(t *testing.T) {
	logged := capture(t)
	Handler("/api/v1/search", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		NoteSearch(r.Context(), "0123456789abcdef", "meaning-every")
		NoteSearch(r.Context(), "0123456789abcdef", "meaning-every+broadened")
		NoteSearch(Detached(r.Context()), "fedcba9876543210", "keyword")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/search?q=hidden-query", nil))
	got := line(t, logged)
	carries(t, got, ` search_key=0123456789abcdef `, ` search_shape=meaning-every+broadened`)
	for _, forbidden := range []string{"fedcba9876543210", "search_shape=keyword", "hidden-query"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("access-log line %q carries %q", got, forbidden)
		}
	}
}

// A request that notes no search carries neither search field.
func TestARequestThatNotesNoSearchCarriesNoSearchField(t *testing.T) {
	logged := capture(t)
	Handler("/r", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	if got := line(t, logged); strings.Contains(got, "search_") {
		t.Fatalf("access-log line %q carries a search field", got)
	}
}

// A method is the caller's own text on a route mounted for every method, so it is clipped too.
func TestAnUnknownMethodIsClipped(t *testing.T) {
	logged := capture(t)
	request := httptest.NewRequest(http.MethodGet, "/r", nil)
	request.Method = strings.Repeat("M", 4*fieldBytes)
	Handler("/r", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), request)
	if got := line(t, logged); strings.Contains(got, strings.Repeat("M", fieldBytes+1)) {
		t.Fatalf("access-log line keeps more than %d bytes of the method", fieldBytes)
	}
}

// The session id a bearer names is the caller's own text, so the line keeps at most fieldBytes of
// it, as it keeps of the path.
func TestANamedSessionIsClipped(t *testing.T) {
	logged := capture(t)
	long := strings.Repeat("s", 4*fieldBytes)
	Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), KindSession, long)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	got := line(t, logged)
	carries(t, got, `caller=`+strings.Repeat("s", fieldBytes)+" ")
	if strings.Contains(got, strings.Repeat("s", fieldBytes+1)) {
		t.Fatalf("access-log line keeps more than %d bytes of the named session", fieldBytes)
	}
}
