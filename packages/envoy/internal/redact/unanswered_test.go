package redact

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// unansweredToken is the bearer the requests below carry, which each stand-in echoes.
const unansweredToken = "kd7vq-not-a-secret-RDX-e52a"

// rawAnswer serves, until t ends, a stand-in that reads each request on a connection of its own
// and writes answer of it on the connection itself, then closes it: an answer net/http's own
// server never writes. It answers the stand-in's base URL.
func rawAnswer(t *testing.T, answer func(request *http.Request) string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.WriteString(conn, answer(request))
			}(conn)
		}
	}()
	return "http://" + listener.Addr().String()
}

// bearerOf is the credential request carried in its Authorization, as a stand-in echoes it.
func bearerOf(request *http.Request) string {
	return strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
}

// ownText reports whether text is the own text of an error err wraps, down its chain
// (errors.Unwrap): words net/http or the system wrote, never a copy the printer made up.
func ownText(err error, text string) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e.Error() == text {
			return true
		}
	}
	return false
}

// TestUnansweredNamesTheCallAndKeepsOnlyATransportFailuresOwnWords: each row makes a request that
// gets no answer it can read, through a client that follows no redirect (refusing), one that
// follows them only on the request's own origin (sameOrigin) or one that follows every redirect
// (following), and reads Unanswered of net/http's error. A failure whose text net/http writes from
// the answer - a Location that does not parse, a status line or a header line that does not
// parse, a chunked body's trailer line that does not parse, the address a redirect followed to
// another host named - prints as its class and holds no run of the token net/http's own text
// quotes (each row's twin: that text holds it). A transport failure on the request's own host - a
// refused connection, a deadline, a cancellation, a peer that closed the connection after a
// redirect on the request's own origin - prints net/http's text of it, as it did. The call is
// named shown, or the URL it asked as URL prints it; and errors.Is and errors.As find what they
// found before, a deadline's net.Error timeout among them.
func TestUnansweredNamesTheCallAndKeepsOnlyATransportFailuresOwnWords(t *testing.T) {
	refusing := redirects.Refused(&http.Client{})
	sameOrigin := redirects.SameOrigin(&http.Client{})
	following := &http.Client{}
	dead := redirectstest.Dead(t)
	echo := func(location func(credential string) string) string {
		return redirectstest.Echo(t, "Authorization", func(credential string) string {
			return location(strings.TrimPrefix(credential, "Bearer "))
		})
	}
	// closing answers /start with a redirect on its own origin, to /closed, where it closes the
	// connection with no answer.
	closing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/closed", http.StatusFound)
			return
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack the connection: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(closing.Close)
	ended, cancelEnded := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelEnded()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name, base, path, shown string
		// want is the failure's whole text, its class; or with ownWords the end of the text of a
		// transport failure, which must be the own text of an error net/http's error wraps.
		want     string
		ownWords bool
		ctx      context.Context
		client   *http.Client
		// twinLeaks is whether net/http's own text of the failure holds the token.
		twinLeaks bool
		// body is whether the failure is the body's read's, after an answer, rather than Do's.
		body bool
	}{
		{name: "location-unparsed", base: echo(redirectstest.Unparsed), path: "/repos/acme/widgets", shown: "/pulls/7", want: ClassLocationUnparsed, client: sameOrigin, twinLeaks: true},
		{name: "status-line-unparsed", base: rawAnswer(t, func(r *http.Request) string {
			return "XTTP/1.1 " + bearerOf(r) + "\r\n\r\n"
		}), path: "/repos/acme/widgets", want: ClassRequestFailed, client: refusing, twinLeaks: true},
		{name: "header-line-unparsed", base: rawAnswer(t, func(r *http.Request) string {
			return "HTTP/1.1 200 OK\r\n" + bearerOf(r) + "\r\n\r\n"
		}), path: "/repos/acme/widgets", want: ClassRequestFailed, client: sameOrigin, twinLeaks: true},
		{name: "trailer-line-unparsed", base: rawAnswer(t, func(r *http.Request) string {
			return "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n" + bearerOf(r) + "\r\n\r\n"
		}), path: "/repos/acme/widgets", want: ClassRequestFailed, client: sameOrigin, twinLeaks: true, body: true},
		{name: "followed-elsewhere", base: echo(func(credential string) string {
			return redirectstest.To(dead, credential)
		}), path: "/repos/acme/widgets?per_page=100", want: ClassRedirected, client: following, twinLeaks: true},
		{name: "followed-on-origin", base: closing.URL, path: "/start", want: "EOF", ownWords: true, client: sameOrigin},
		{name: "refused-connection", base: "http://" + dead, path: "/repos/acme/widgets", want: "connect: connection refused", ownWords: true, client: refusing},
		{name: "deadline", base: "http://" + dead, path: "/repos/acme/widgets", want: "context deadline exceeded", ownWords: true, ctx: ended, client: sameOrigin},
		{name: "canceled", base: "http://" + dead, path: "/repos/acme/widgets", want: "context canceled", ownWords: true, ctx: canceled, client: sameOrigin},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := c.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+c.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+unansweredToken)
			response, err := c.client.Do(request)
			if c.body {
				if err != nil {
					t.Fatalf("the request got no answer: %v", err)
				}
				_, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			} else if err == nil {
				_ = response.Body.Close()
				t.Fatalf("the request got an answer, %d", response.StatusCode)
			}
			if err == nil {
				t.Fatal("the answer's body read whole")
			}
			if leaked := strings.Contains(err.Error(), unansweredToken); leaked != c.twinLeaks {
				t.Fatalf("net/http's own text holds the token: %v, want %v: %q", leaked, c.twinLeaks, err)
			}
			printed := Unanswered(err, request, c.shown)
			if piece := redacttest.Piece(printed.Error(), unansweredToken); piece != "" || strings.Contains(printed.Error(), "moved") {
				t.Errorf("Unanswered = %q, which holds the token or the Location", printed)
			}
			name := c.shown
			if name == "" {
				name = URL(request.URL.String())
			}
			head := `Get "` + name + `": `
			if c.body {
				head = ""
			}
			switch {
			case !c.ownWords && printed.Error() != head+c.want:
				t.Errorf("Unanswered = %q, want %q", printed, head+c.want)
			case c.ownWords && (!strings.HasPrefix(printed.Error(), head) || !strings.HasSuffix(printed.Error(), c.want) ||
				!ownText(err, strings.TrimPrefix(printed.Error(), head))):
				t.Errorf("Unanswered = %q, want it to begin %q and end in net/http's own text of the failure, ending %q", printed, head, c.want)
			}
			if !errors.Is(printed, err) {
				t.Errorf("Unanswered = %#v, which no longer finds net/http's error", printed)
			}
			var failed *url.Error
			if !c.body && !errors.As(printed, &failed) {
				t.Errorf("Unanswered = %#v, which no longer finds net/http's *url.Error", printed)
			}
			switch c.name {
			case "refused-connection":
				if !errors.Is(printed, syscall.ECONNREFUSED) || !strings.HasSuffix(printed.Error(), failed.Err.Error()) {
					t.Errorf("Unanswered = %q, want net/http's own text of a refused connection, %q, found as ECONNREFUSED", printed, failed.Err)
				}
			case "deadline":
				var timeout net.Error
				if !errors.Is(printed, context.DeadlineExceeded) || !errors.As(printed, &timeout) || !timeout.Timeout() {
					t.Errorf("Unanswered = %#v, want the deadline found as context.DeadlineExceeded and a net.Error timeout", printed)
				}
			case "canceled":
				if !errors.Is(printed, context.Canceled) {
					t.Errorf("Unanswered = %#v, want the caller's own cancellation found as context.Canceled", printed)
				}
			}
		})
	}
	t.Run("not-a-url", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodGet, "http://api.github.example/moved%zz"+unansweredToken, nil)
		if err == nil {
			t.Fatal("the request was made")
		}
		if !strings.Contains(err.Error(), unansweredToken) {
			t.Fatalf("net/url's own text holds no token, so the row cannot see a leak: %q", err)
		}
		if printed := Unanswered(err, nil, "the installation's lookup"); printed.Error() != `parse "the installation's lookup": `+ClassNotAURL {
			t.Errorf("Unanswered = %q, want the call named and its class", printed)
		}
		if printed := Unanswered(err, nil, ""); printed.Error() != `parse "`+NotPrinted+`": `+ClassNotAURL {
			t.Errorf("Unanswered = %q, want the URL as URL prints it and the class", printed)
		}
	})
	if Unanswered(nil, nil, "") != nil {
		t.Error("Unanswered(nil) is not nil")
	}
}
