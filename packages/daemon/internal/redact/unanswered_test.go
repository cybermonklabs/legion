package redact

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// unansweredToken is the bearer the requests below carry, which each stand-in echoes.
const unansweredToken = "hj4tn-not-a-secret-RDX-31d9"

// TestUnansweredNamesTheCallAndTheFailuresClassAndNoneOfNetHTTPsWords: each row makes a request
// that gets no answer, through a client that follows no redirect (refusing) or one that follows
// them (following), and reads Unanswered of net/http's error. A failure whose text net/http writes
// from the answer - a Location that does not parse, a status line that does not parse, the address
// a followed redirect named - prints as its class and holds no run of the token net/http's own
// text quotes (each row's twin: that text holds it); a transport failure on the request's own host
// prints net/http's text of it, as it did; the call is named shown, or the URL it asked through the
// URL rule; and errors.Is and errors.As find what they found before.
func TestUnansweredNamesTheCallAndTheFailuresClassAndNoneOfNetHTTPsWords(t *testing.T) {
	refusing := redirects.Refused(&http.Client{})
	following := &http.Client{}
	dead := redirectstest.Dead(t)
	echo := func(location func(credential string) string) string {
		return redirectstest.Echo(t, "Authorization", location)
	}
	// malformed answers each connection with a status line that quotes the token and is no
	// HTTP status line, which net/http quotes in its error.
	malformed := func() string {
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
				_, _ = bufio.NewReader(conn).ReadString('\n')
				_, _ = conn.Write([]byte("XTTP/1.1 " + unansweredToken + "\r\n\r\n"))
				_ = conn.Close()
			}
		}()
		return "http://" + listener.Addr().String()
	}
	for _, c := range []struct {
		name, base, shown, want string
		client                  *http.Client
		twinLeaks               bool
	}{
		{"location-unparsed", echo(redirectstest.Unparsed), "/pulls/7", classLocationUnparsed, refusing, true},
		{"status-line-unparsed", malformed(), "", classRequestFailed, refusing, true},
		{"followed-elsewhere", echo(func(credential string) string {
			return redirectstest.To(dead, strings.TrimPrefix(credential, "Bearer "))
		}), "", classRedirected, following, true},
		{"refused-connection", "http://" + dead, "", "connect: connection refused", refusing, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, c.base+"/repos/acme/widgets?per_page=100", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+unansweredToken)
			response, err := c.client.Do(request)
			if err == nil {
				_ = response.Body.Close()
				t.Fatal("the request got an answer")
			}
			if leaked := strings.Contains(err.Error(), unansweredToken); leaked != c.twinLeaks {
				t.Fatalf("net/http's own text holds the token: %v, want %v: %q", leaked, c.twinLeaks, err)
			}
			printed := Unanswered(err, request, c.shown)
			if strings.Contains(printed.Error(), unansweredToken[:8]) || strings.Contains(printed.Error(), "moved") {
				t.Errorf("Unanswered = %q, which holds the token or the Location", printed)
			}
			name := c.shown
			if name == "" {
				name = URLCredentials(request.URL.String())
			}
			if wantLine := `Get "` + name + `": `; !strings.HasPrefix(printed.Error(), wantLine) || !strings.Contains(printed.Error(), c.want) {
				t.Errorf("Unanswered = %q, want it to begin %q and hold %q", printed, wantLine, c.want)
			}
			var failed *url.Error
			if !errors.Is(printed, err) || !errors.As(printed, &failed) {
				t.Errorf("Unanswered = %#v, which no longer finds net/http's error", printed)
			}
			if c.name == "refused-connection" && (!errors.Is(printed, syscall.ECONNREFUSED) || !strings.HasSuffix(printed.Error(), failed.Err.Error())) {
				t.Errorf("Unanswered = %q, want net/http's own text of a refused connection, %q, found as ECONNREFUSED", printed, failed.Err)
			}
		})
	}
	t.Run("not-a-url", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodGet, "http://cache.example/moved%zz", nil)
		if err == nil {
			t.Fatal("the request was made")
		}
		if printed := Unanswered(err, nil, "the cache's health"); printed.Error() != `parse "the cache's health": `+classNotAURL || strings.Contains(printed.Error(), "%zz") {
			t.Errorf("Unanswered = %q, want the call named and its class", printed)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+dead+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = refusing.Do(request)
		if printed := Unanswered(err, request, ""); !errors.Is(printed, context.Canceled) || !strings.HasSuffix(printed.Error(), ": context canceled") {
			t.Errorf("Unanswered = %q, want the caller's own cancellation named as it is", printed)
		}
	})
	if Unanswered(nil, nil, "") != nil {
		t.Error("Unanswered(nil) is not nil")
	}
}
