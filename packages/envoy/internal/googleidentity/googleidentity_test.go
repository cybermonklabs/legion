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

// TestProbeSaysNoMetadataServerWithinItsTimeout: where nothing answers - a port no one listens
// on, or a listener that takes the connection and never answers, as a laptop's network may - the
// probe answers ErrNoMetadataServer, the absence a caller skips, within 2 s.
func TestProbeSaysNoMetadataServerWithinItsTimeout(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	closed.Close()

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

	for name, host := range map[string]string{"a closed port": closedAddr, "a listener that never answers": silent.Addr().String()} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(MetadataHostVar, host)
			started := time.Now()
			email, err := Probe(context.Background())
			took := time.Since(started)
			if !errors.Is(err, ErrNoMetadataServer) || took > 2*time.Second {
				t.Fatalf("Probe = %q, %v after %s; want ErrNoMetadataServer within 2s", email, err, took)
			}
		})
	}
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
