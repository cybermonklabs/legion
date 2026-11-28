package accesslog

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// timings is the run of the line's fields from status on: status, elapsed_ms and bytes next to
// each other, as a production look reads them
// (packages/dispatch/e2e/acceptance/priorities-production.sh), then the four timings, then the
// line's end or its next field.
var timings = regexp.MustCompile(` status=\d+ elapsed_ms=\d+ bytes=\d+ db_ms=(\d+) db_calls=(\d+) db_wait_ms=(\d+) roster_wait_ms=(\d+)(?: |$)`)

// timingsOf is the four timings got carries where the line writes them, in their order: db_ms,
// db_calls, db_wait_ms and roster_wait_ms. A line that does not carry them there fails the test.
func timingsOf(t *testing.T, got string) [4]int64 {
	t.Helper()
	match := timings.FindStringSubmatch(got)
	if match == nil {
		t.Fatalf("access-log line %q does not carry status, elapsed_ms, bytes, db_ms, db_calls, db_wait_ms and roster_wait_ms in one run", got)
	}
	var read [4]int64
	for index := range read {
		read[index], _ = strconv.ParseInt(match[index+1], 10, 64)
	}
	return read
}

// A request that waits on nothing writes its four timings as 0: they are on every line, right
// after bytes and before the fields a line may leave out, so status, elapsed_ms and bytes stay
// next to each other.
func TestARequestThatWaitsOnNothingWritesItsTimingsAsZero(t *testing.T) {
	logged := capture(t)
	Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), KindSharedToken, "")
		Note(r.Context(), KindSession, "session-0123456789abcdef")
		_, _ = w.Write([]byte("hello"))
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	got := line(t, logged)
	if read := timingsOf(t, got); read != [4]int64{} {
		t.Fatalf("a request that waited on nothing wrote db_ms, db_calls, db_wait_ms and roster_wait_ms %v, want all 0", read)
	}
	carries(t, got, ` bytes=5 db_ms=0 db_calls=0 db_wait_ms=0 roster_wait_ms=0 credential=shared-token`)
}

// The line writes what its request's Timing was given, from any goroutine of the request: each sum
// is kept in nanoseconds and cut to milliseconds once, so eight adds of 1.5 ms write 12, where
// eight cuts would write 8. A context that carries no line, the server's own or one Detached masks,
// has no Timing, and an add to that nil Timing reaches no line.
func TestTheLineSumsWhatItsTimingWasGivenFromEveryGoroutine(t *testing.T) {
	logged := capture(t)
	if TimingOf(context.Background()) != nil {
		t.Fatal("a context no request made has a Timing")
	}
	Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if TimingOf(Detached(r.Context())) != nil {
			t.Error("a detached context has its request's Timing")
		}
		var adds sync.WaitGroup
		for range 8 {
			adds.Go(func() {
				timing := TimingOf(r.Context())
				timing.AddDatabase(1500*time.Microsecond, 2)
				timing.AddPoolWait(1250 * time.Microsecond)
				timing.AddRosterWait(750 * time.Microsecond)
			})
		}
		adds.Wait()
		TimingOf(Detached(r.Context())).AddDatabase(time.Second, 1)
		TimingOf(context.Background()).AddRosterWait(time.Second)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	if read, want := timingsOf(t, line(t, logged)), [4]int64{12, 16, 10, 6}; read != want {
		t.Fatalf("db_ms, db_calls, db_wait_ms and roster_wait_ms = %v, want %v", read, want)
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

// The line names the Host the request was sent to as the client wrote it, its case and its port
// kept, so an operator reads which of the server's addresses each caller uses, and a Host is the
// caller's own text, so the line keeps at most fieldBytes of it.
func TestTheLineNamesTheHostAsWritten(t *testing.T) {
	long := strings.Repeat("h", 4*fieldBytes) + ".example.test"
	for _, row := range []struct{ name, host, want string }{
		{"a mixed-case host", "B.Example.Test", "B.Example.Test"},
		{"a host with a port", "b.example.test:8443", "b.example.test:8443"},
		{"a very long host", long, long[:fieldBytes]},
	} {
		t.Run(row.name, func(t *testing.T) {
			logged := capture(t)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
			request.Host = row.host
			Handler("/api/v1/whoami", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
				ServeHTTP(httptest.NewRecorder(), request)
			got := line(t, logged)
			carries(t, got, ` host=`+row.want+` `)
		})
	}
}

// A request refused before any route read it writes the line every request writes, under the
// route RefusedRoute, with what its refusal answered, and with the connection's own address: the
// X-Forwarded-For that would name another is the header the refusal is about.
func TestARefusedRequestWritesTheLineWithTheConnectionsAddress(t *testing.T) {
	logged := capture(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues", nil)
	request.Host = "B.Example.Test:8443"
	request.RemoteAddr = "127.0.0.1:40000"
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	request.Header.Set("User-Agent", "dispatch-cli/1.0")
	response := httptest.NewRecorder()
	Refused(response, request, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "refused")
	})
	if response.Code != http.StatusBadRequest || response.Body.String() != "refused" {
		t.Fatalf("the refusal answered %d %q, want the 400 it wrote", response.Code, response.Body.String())
	}
	got := line(t, logged)
	carries(t, got, `method=POST`, `route=`+RefusedRoute+` `, `path=/api/v1/issues `, `caller_kind=anonymous`, `host=B.Example.Test:8443 `,
		`address=127.0.0.1 `, `peer=127.0.0.1 `, `user_agent=dispatch-cli/1.0`, `status=400`, `bytes=7`)
	if strings.Contains(got, "198.51.100.8") {
		t.Fatalf("a refused request's line %q names the address its X-Forwarded-For wrote", got)
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
