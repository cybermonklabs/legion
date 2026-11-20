// packages/envoy/internal/googleidentity/googleidentity_test.go
//
// The metadata reader against a stub metadata server: what it asks for, what it answers, how it
// refuses, and that no proxy variable can take the token.
package googleidentity

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testport"
)

// stubMetadata stands up a metadata server at a host GCE_METADATA_HOST names, answering handler.
// It returns the requests it was asked, so a test reads back the path and the query the reader
// sent.
func stubMetadata(t *testing.T, handler http.HandlerFunc) *[]*http.Request {
	t.Helper()
	var asked []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv(MetadataHostVar, strings.TrimPrefix(server.URL, "http://"))
	return &asked
}

// TestTokenAsksTheMetadataServerForAFullFormatIdentityToken: the reader asks the default service
// account's identity path for the audience it was given, with format=full and the header Google
// requires, and answers the token with no whitespace around it.
func TestTokenAsksTheMetadataServerForAFullFormatIdentityToken(t *testing.T) {
	asked := stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("header.payload.signature\n"))
	})

	token, err := Token(context.Background(), "example-audience")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "header.payload.signature" {
		t.Fatalf("Token = %q, want the metadata server's answer with its trailing newline trimmed", token)
	}
	if len(*asked) != 1 {
		t.Fatalf("the metadata server was asked %d times, want once", len(*asked))
	}
	request := (*asked)[0]
	if request.URL.Path != identityPath {
		t.Errorf("the reader asked %q, want %q: the identity token of the instance's default service account", request.URL.Path, identityPath)
	}
	if got := request.URL.Query().Get("audience"); got != "example-audience" {
		t.Errorf("audience = %q, want the one the caller named", got)
	}
	if got := request.URL.Query().Get("format"); got != "full" {
		t.Errorf("format = %q, want full: without it Google mints a token carrying no google.compute_engine claims", got)
	}
	if got := request.Header.Get("Metadata-Flavor"); got != "Google" {
		t.Errorf("Metadata-Flavor = %q, want Google: the metadata server refuses a request without it", got)
	}
}

// TestTokenIgnoresEveryProxyVariable: the reader's transport has no proxy at all, so a machine
// whose environment names one hands that proxy nothing. The proxy here answers every request
// with a token of its own, so a reader that honoured it would pass the first check below and
// fail the count.
func TestTokenIgnoresEveryProxyVariable(t *testing.T) {
	var proxied atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.Write([]byte("the.proxys.token"))
	}))
	t.Cleanup(proxy.Close)
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(name, proxy.URL)
	}
	stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("the.machines.token"))
	})

	token, err := Token(context.Background(), "example-audience")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "the.machines.token" {
		t.Fatalf("Token = %q, want the metadata server's own answer: a proxy may never see a machine's identity token", token)
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("the proxy was asked %d times, want none: the metadata server's address is link-local and its token acts as the machine", n)
	}
}

// TestTokenRefusesWhatItCannotRead names each refusal, and none of them repeats a token: the
// server that answers a status, and the one that answers nothing at all.
func TestTokenRefusesWhatItCannotRead(t *testing.T) {
	for _, refusal := range []struct {
		name    string
		handler http.HandlerFunc
		says    string
	}{
		{
			name:    "a status that is not a success",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
			says:    "answered HTTP 403",
		},
		{
			name:    "an empty body",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.Write(nil) },
			says:    "answered an empty identity token",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			stubMetadata(t, refusal.handler)
			token, err := Token(context.Background(), "example-audience")
			if err == nil || !strings.Contains(err.Error(), refusal.says) {
				t.Fatalf("Token = %q, %v; want a refusal carrying %q", token, err, refusal.says)
			}
		})
	}
}

// TestTokenNamesTheHostItCouldNotReach: a metadata server that answers nothing is a fault of the
// machine's own link, and the refusal names the variable an operator sets to point the reader
// somewhere else.
func TestTokenNamesTheHostItCouldNotReach(t *testing.T) {
	t.Setenv(MetadataHostVar, "127.0.0.1:1")

	_, err := Token(context.Background(), "example-audience")
	if err == nil || !strings.Contains(err.Error(), MetadataHostVar) {
		t.Fatalf("Token's error = %v, want one naming %s", err, MetadataHostVar)
	}
}

// TestProbeAnswersTheEmailOfAGoogleMetadataServer: the probe asks the default service account's
// email path with the header Google requires, and answers the email a server that says it is
// Google's metadata server names.
func TestProbeAnswersTheEmailOfAGoogleMetadataServer(t *testing.T) {
	asked := stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		w.Write([]byte("machine@example.iam.gserviceaccount.com\n"))
	})

	email, err := Probe(context.Background())
	if err != nil || email != "machine@example.iam.gserviceaccount.com" {
		t.Fatalf("Probe = %q, %v; want the metadata server's email", email, err)
	}
	if len(*asked) != 1 || (*asked)[0].URL.Path != emailPath || (*asked)[0].Header.Get("Metadata-Flavor") != "Google" {
		t.Fatalf("the metadata server was asked %d times; want once, at %s, with Metadata-Flavor: Google", len(*asked), emailPath)
	}
}

// TestProbeSaysNoMetadataServerWithinItsTimeout: where nothing answers, the probe answers
// ErrNoMetadataServer, the absence a caller skips, and its error says why. A port no one listens
// on refuses the connection, and the error names the refusal. A listener that takes the connection
// and never answers, as a laptop's network may, holds the probe until the probe's own deadline
// ends it, and the error names that deadline. That deadline is ProbeTimeout from the probe's start:
// the row reads it off the request the probe sends (deadlineRecorder), and requires it no earlier
// than ProbeTimeout after the call began and no later than ProbeTimeout after the request left.
// Neither bound reads how long anything took, so a loaded machine does not move them, and a probe
// whose timeout grew fails them. Only the test binary's deadline bounds the time taken, which ends
// a probe that never returns.
func TestProbeSaysNoMetadataServerWithinItsTimeout(t *testing.T) {
	t.Run("a closed port", func(t *testing.T) {
		// A port the test holds and nothing listens on, so no other process can take it and
		// answer (testport.Refused).
		t.Setenv(MetadataHostVar, testport.Refused(t))
		email, err := Probe(context.Background())
		if !errors.Is(err, ErrNoMetadataServer) || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("Probe = %q, %v; want ErrNoMetadataServer naming the refused connection", email, err)
		}
	})
	t.Run("a listener that never answers", func(t *testing.T) {
		silent, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { silent.Close() })
		var held []net.Conn
		go func() {
			for {
				conn, err := silent.Accept()
				if err != nil {
					return
				}
				held = append(held, conn) // never answered, closed with the listener's test
			}
		}()
		t.Setenv(MetadataHostVar, silent.Addr().String())
		original := client.Transport
		recorder := &deadlineRecorder{next: original}
		client.Transport = recorder
		t.Cleanup(func() { client.Transport = original })
		before := time.Now()
		email, err := Probe(context.Background())
		took := time.Since(before)
		// The error prints the failure's class, "timed out", and not the library's text, which
		// quotes the request URL; the deadline it names is still the one errors.Is finds.
		if !errors.Is(err, ErrNoMetadataServer) || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") || took < ProbeTimeout {
			t.Fatalf("Probe = %q, %v after %s; want ErrNoMetadataServer ended by the probe's own deadline, %s or later", email, err, took, ProbeTimeout)
		}
		if recorder.calls != 1 || !recorder.hasDeadline {
			t.Fatalf("the probe sent %d requests, under a deadline: %v; want one, under the probe's own deadline", recorder.calls, recorder.hasDeadline)
		}
		if fromCall, fromRequest := recorder.deadline.Sub(before), recorder.deadline.Sub(recorder.entered); fromCall < ProbeTimeout || fromRequest > ProbeTimeout {
			t.Fatalf("the probe asked under a deadline %s after the call began and %s after the request left; want at least and at most %s", fromCall, fromRequest, ProbeTimeout)
		}
	})
}

// deadlineRecorder is an http.RoundTripper that records when each request left and the deadline its
// context carried, then sends it on next. http.Client calls it on the goroutine that called Do, so
// a test reads what it recorded once that call has returned.
type deadlineRecorder struct {
	next        http.RoundTripper
	calls       int
	entered     time.Time
	deadline    time.Time
	hasDeadline bool
}

func (r *deadlineRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	r.entered = time.Now()
	r.deadline, r.hasDeadline = req.Context().Deadline()
	return r.next.RoundTrip(req)
}

// TestProbeRefusesAServerWithoutTheGoogleFlavor: an answer that does not carry the
// Metadata-Flavor: Google header is not Google's metadata server, so the probe answers
// ErrNoMetadataServer, saying so, rather than an email it cannot vouch for.
func TestProbeRefusesAServerWithoutTheGoogleFlavor(t *testing.T) {
	stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("someone@example.com"))
	})

	email, err := Probe(context.Background())
	if !errors.Is(err, ErrNoMetadataServer) || !strings.Contains(err.Error(), "not a Google metadata server") {
		t.Fatalf("Probe = %q, %v; want ErrNoMetadataServer saying the server is not Google's", email, err)
	}
}

// TestAccessTokenSourceReadsTheMetadataToken: the token source asks the default service
// account's token path with the header Google requires, and answers the token the metadata
// server handed out, expiring when that answer's expires_in says.
func TestAccessTokenSourceReadsTheMetadataToken(t *testing.T) {
	asked := stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"metadata-not-a-secret","expires_in":600,"token_type":"Bearer"}`))
	})

	started := time.Now()
	token, err := AccessTokenSource().Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token.AccessToken != "metadata-not-a-secret" || token.TokenType != "Bearer" {
		t.Fatalf("token %q of type %q, want the metadata server's metadata-not-a-secret, Bearer", token.AccessToken, token.TokenType)
	}
	if expiry := token.Expiry.Sub(started); expiry < 590*time.Second || expiry > 610*time.Second {
		t.Fatalf("the token expires %s from the request, want the 600s its answer gave", expiry)
	}
	if len(*asked) != 1 || (*asked)[0].URL.Path != tokenPath || (*asked)[0].Header.Get("Metadata-Flavor") != "Google" {
		t.Fatalf("the metadata server was asked %d times; want once, at %s, with Metadata-Flavor: Google", len(*asked), tokenPath)
	}
}

// TestAccessTokenSourceRefusesAServerWithoutTheGoogleFlavor: a token answer that does not carry
// the Metadata-Flavor: Google header is not Google's metadata server, as a resolver that sends
// every name to one web server would bring it, so the token source answers ErrNoMetadataServer,
// saying so, and never a token it cannot vouch for.
func TestAccessTokenSourceRefusesAServerWithoutTheGoogleFlavor(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				w.Write([]byte(`{"access_token":"elsewhere-not-a-secret","expires_in":600,"token_type":"Bearer"}`))
			})
			token, err := AccessTokenSource().Token()
			if !errors.Is(err, ErrNoMetadataServer) || !strings.Contains(err.Error(), "not a Google metadata server") {
				t.Fatalf("Token = %v, %v; want ErrNoMetadataServer saying the server is not Google's", token, err)
			}
		})
	}
}

// TestAccessTokenSourceNamesAGoogleFaultAsAFault: a Google metadata server that answers the token
// read with an error is present and broken, so its error is not ErrNoMetadataServer.
func TestAccessTokenSourceNamesAGoogleFaultAsAFault(t *testing.T) {
	stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	token, err := AccessTokenSource().Token()
	if err == nil || errors.Is(err, ErrNoMetadataServer) || !strings.Contains(err.Error(), "answered HTTP 503") {
		t.Fatalf("Token = %v, %v; want a fault naming HTTP 503 that is not ErrNoMetadataServer", token, err)
	}
}

// TestAccessTokenSourceSaysNoMetadataServerWhenNoConnectionComesWithinTheProbeTimeout: a host no
// connection reaches - here 192.0.2.1, an address no network routes (RFC 5737), as a laptop's
// resolver or captive network can leave the metadata server's name - is no metadata server within
// ProbeTimeout plus a margin, not after the token read's whole 10 s.
func TestAccessTokenSourceSaysNoMetadataServerWhenNoConnectionComesWithinTheProbeTimeout(t *testing.T) {
	t.Setenv(MetadataHostVar, "192.0.2.1")
	started := time.Now()
	token, err := AccessTokenSource().Token()
	took := time.Since(started)
	if !errors.Is(err, ErrNoMetadataServer) || took > ProbeTimeout+2*time.Second {
		t.Fatalf("Token = %v, %v after %s; want ErrNoMetadataServer within %s", token, err, took, ProbeTimeout+2*time.Second)
	}
}
