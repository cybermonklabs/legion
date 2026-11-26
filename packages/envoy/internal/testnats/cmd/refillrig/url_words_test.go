package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// credentialed is address, a stand-in's http URL, with redacttest's user and password in its user
// information and its token in its query.
func credentialed(address string) string {
	return strings.Replace(address, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1) + "/?access" + "_token=" + redacttest.Token
}

// stdoutOf is what run writes to the process's standard output.
func stdoutOf(t *testing.T, run func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	saved := os.Stdout
	os.Stdout = w
	func() {
		defer func() { os.Stdout = saved }()
		run()
	}()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestARegistrationPrintsNoCredentialItsListenerURLCarries: register's -url can carry a credential
// in its user information and its query. The line a whole registration prints names the listener
// as the rule prints -url (registered), and the error of a POST that got no answer names the call
// as the rule prints it beside -url, with the failure's class (post-error). The twin shows the URL
// as written holds the credentials. Captures refillrig.register-url/<case>.
func TestARegistrationPrintsNoCredentialItsListenerURLCarries(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(listener.Close)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	t.Run("twin", func(t *testing.T) {
		for _, raw := range []string{credentialed(listener.URL), credentialed(dead.URL)} {
			if held := redacttest.Held(raw); len(held) != len(redacttest.Secrets) {
				t.Errorf("the URL %q holds %q; want every credential, so the rows can see one printed", raw, held)
			}
		}
	})
	t.Run("registered", func(t *testing.T) {
		var err error
		said := stdoutOf(t, func() {
			err = runRegister([]string{"-url", credentialed(listener.URL), "-sets", "words=1", "-concurrency", "1"})
		})
		if err != nil {
			t.Fatalf("register = %v; want the listener's 200 taken", err)
		}
		redacttest.RequireNoPiece(t, "refillrig.register-url/registered", said, redacttest.User, redacttest.Password, redacttest.Token)
		if want := "registered 1 sessions through " + redact.NotPrinted + "/v1/interests/subscribe in "; !strings.HasPrefix(said, want) {
			t.Errorf("register printed %q; want it to begin %q", said, want)
		}
	})
	t.Run("post-error", func(t *testing.T) {
		err := runRegister([]string{"-url", credentialed(dead.URL), "-sets", "words=1", "-concurrency", "1"})
		if err == nil {
			t.Fatal("register succeeded against a listener that is gone")
		}
		redacttest.RequireNoPiece(t, "refillrig.register-url/post-error", err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		if want := `Post "` + redact.NotPrinted + `": ` + redact.ClassRefused; err.Error() != want {
			t.Errorf("register = %q; want %q, the call as the rule prints it and the failure's class", err, want)
		}
	})
}

// TestASeedThatCannotConnectPrintsNoCredentialItsNATSURLCarries: seed's -nats can carry a
// credential in its user information and its query, and nats.go's error for a URL it cannot parse
// quotes the URL whole. seed's error names the flag and the failure's class alone (connect). A
// failure that names no part of the URL keeps nats.go's own words (no-servers). The twin shows the
// URL as written holds the credentials. Captures refillrig.seed-url/connect.
func TestASeedThatCannotConnectPrintsNoCredentialItsNATSURLCarries(t *testing.T) {
	raw := "nats://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:not-a-port/?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if held := redacttest.Held(raw); len(held) != len(redacttest.Secrets) {
			t.Errorf("the URL %q holds %q; want every credential, so the row can see one printed", raw, held)
		}
	})
	t.Run("connect", func(t *testing.T) {
		err := runSeed([]string{"-nats", raw, "-sets", ""})
		if err == nil {
			t.Fatal("seed connected through a URL that does not parse")
		}
		redacttest.RequireNoPiece(t, "refillrig.seed-url/connect", err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		if want := "connect to the NATS server -nats names: " + redact.ClassNotAURL; err.Error() != want {
			t.Errorf("seed = %q; want %q, the flag and the failure's class", err, want)
		}
	})
	t.Run("no-servers", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		gone := "nats://" + redacttest.User + ":" + redacttest.Password + "@" + listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		err = runSeed([]string{"-nats", gone, "-sets", ""})
		if want := "connect to the NATS server -nats names: " + nats.ErrNoServers.Error(); err == nil || err.Error() != want {
			t.Errorf("seed against a server that is gone = %v; want %q, nats.go's own words", err, want)
		}
	})
}

// TestAPollThatGetsNoAnswerWritesNoCredentialItsListenerURLCarries: poll's -url can carry a
// credential in its user information and its query, and a request that got no answer is written
// to -out, which a judge prints (answer). net/http's error of a GET to a listener that is gone
// names the URL with its user name and its query (get-error), and http.NewRequest's error of a URL
// that does not parse quotes it whole (unparsed); each is written as the rule prints it beside
// -url. The twin shows each URL as written holds the credentials. Captures
// refillrig.poll-url/<case>.
func TestAPollThatGetsNoAnswerWritesNoCredentialItsListenerURLCarries(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	gone := credentialed(dead.URL)
	unparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/base%zz?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if held := redacttest.Held(gone); len(held) != len(redacttest.Secrets) {
			t.Errorf("the URL %q holds %q; want every credential, so the get-error row can see one printed", gone, held)
		}
		if _, err := http.NewRequest(http.MethodGet, unparsed+listPaths["sessions"], nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the unparsed row can see them", err)
		}
	})
	for _, c := range []struct{ name, url, want string }{
		{"get-error", gone, `Get "` + redact.NotPrinted + `": ` + redact.ClassRefused},
		{"unparsed", unparsed, `parse "` + redact.NotPrinted + `": ` + redact.ClassNotAURL},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := observe(context.Background(), &http.Client{Timeout: 5 * time.Second}, c.url, "sessions", nil)
			said := answer(o)
			redacttest.RequireNoPiece(t, "refillrig.poll-url/"+c.name, said, redacttest.User, redacttest.Password, redacttest.Token)
			if want := "no answer (" + c.want + ")"; said != want {
				t.Errorf("the judge prints the poll as %q; want %q, the call as the rule prints it and the failure's class", said, want)
			}
		})
	}
}
