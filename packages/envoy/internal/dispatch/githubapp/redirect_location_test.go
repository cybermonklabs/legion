package githubapp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// redirectToken is the installation token the Branch rows carry, which each stand-in echoes.
const redirectToken = "ghs_mq4zt-not-a-secret-RDX-8c1f"

// echoed is the credential a stand-in saw in the Authorization of the request it answered, bare
// of its scheme: the App's JWT for a mint, which the test cannot know before the client signs it.
type echoed struct {
	mu   sync.Mutex
	bare string
}

// of records the credential authorization carries and answers it.
func (e *echoed) of(authorization string) string {
	bare := strings.TrimPrefix(authorization, "Bearer ")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bare = bare
	return bare
}

func (e *echoed) get() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bare
}

// rawStandIn serves, until t ends, a stand-in that reads each request on a connection of its own
// and writes answer of it on the connection itself, then closes it: an answer net/http's own
// server never writes. It answers the stand-in's base URL.
func rawStandIn(t *testing.T, answer func(request *http.Request) string) string {
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

// TestAGitHubCallThatGetsNoAnswerPrintsNoCredentialItCarried: whatever answers in GitHub's place
// answers a token mint, which carries the App's JWT, and a branch read, which carries an
// installation token, with words that quote that credential. A redirect whose Location does not
// parse (unparsed) and a status line that does not parse (status-line) are net/http's error
// whatever the client's redirect policy, since Go parses a Location before it asks CheckRedirect
// and quotes a status line it cannot read; a redirect that parses names an address on another
// host, one nothing listens at (unanswered), whose transport failure net/http names by the
// Location, or a stand-in that answers and counts what it is asked (followed). The call's error,
// which its callers log and GET /healthz serves as a read's cause, holds no run of the credential
// and no word of the Location, and the stand-in on another host is asked nothing: the client
// follows a redirect only on the API origin (redirects.SameOrigin), so another is GitHub's answer
// by its status. Each case's twin shows net/http's own error, or a client that follows every
// redirect, carries the echo, so a row can see a leak. Captures githubapp.no-answer/<call>-<case>.
func TestAGitHubCallThatGetsNoAnswerPrintsNoCredentialItCarried(t *testing.T) {
	target, asked := redirectstest.Target(t)
	dead := redirectstest.Dead(t)
	_, pemText := testKeyPEM(t)
	type standIn func(t *testing.T, seen *echoed) string
	echo := func(location func(bare string) string) standIn {
		return func(t *testing.T, seen *echoed) string {
			return redirectstest.Echo(t, "Authorization", func(credential string) string {
				return location(seen.of(credential))
			})
		}
	}
	cases := []struct {
		name  string
		serve standIn
	}{
		{"unparsed", echo(redirectstest.Unparsed)},
		{"status-line", func(t *testing.T, seen *echoed) string {
			return rawStandIn(t, func(r *http.Request) string {
				return "XTTP/1.1 " + seen.of(r.Header.Get("Authorization")) + "\r\n\r\n"
			})
		}},
		{"unanswered", echo(func(bare string) string { return redirectstest.To(dead, bare) })},
		{"followed", echo(func(bare string) string { return redirectstest.To(target, bare) })},
	}
	for _, c := range cases {
		t.Run("twin-"+c.name, func(t *testing.T) {
			var seen echoed
			request, err := http.NewRequest(http.MethodGet, c.serve(t, &seen)+"/repos/acme/web/git/ref/heads/main", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+redirectToken)
			response, err := http.DefaultClient.Do(request)
			if c.name == "followed" {
				if err != nil {
					t.Fatalf("a client that follows every redirect got no answer from the target: %v", err)
				}
				_ = response.Body.Close()
				if n := asked(); n != 1 {
					t.Fatalf("a client that follows every redirect asked the target %d times, want 1: the row cannot see one", n)
				}
				return
			}
			if err == nil {
				_ = response.Body.Close()
				t.Fatalf("net/http answered %d with no error: the row cannot see one", response.StatusCode)
			}
			if !strings.Contains(err.Error(), redirectToken) {
				t.Fatalf("net/http's own error holds no echo of the token, so the row cannot see a leak: %q", err)
			}
		})
	}
	calls := []struct {
		name string
		call func(client *Client) error
	}{
		{"mint", func(client *Client) error {
			_, err := client.Token(context.Background(), 7)
			return err
		}},
		{"branch", func(client *Client) error {
			_, err := client.Branch(context.Background(), redirectToken, "acme", "web", "main")
			return err
		}},
	}
	for _, call := range calls {
		for _, c := range cases {
			t.Run(call.name+"-"+c.name, func(t *testing.T) {
				var seen echoed
				client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, c.serve(t, &seen))
				if err != nil {
					t.Fatalf("new client: %v", err)
				}
				err = call.call(client)
				if err == nil {
					t.Fatal("the call answered no error")
				}
				sent := seen.get()
				if sent == "" {
					t.Fatal("the stand-in saw no credential: the row cannot see an echo of one")
				}
				redacttest.RequireNoPiece(t, "githubapp.no-answer/"+call.name+"-"+c.name, err.Error(), sent)
				if strings.Contains(err.Error(), "moved") {
					t.Errorf("the call's error names the Location: %q", err)
				}
				if n := asked(); n != 0 {
					t.Errorf("the redirect's target on another host was asked %d times, want none", n)
				}
			})
		}
	}
}

// TestARenamedRepositorysRedirectIsStillFollowed: GitHub answers a read of a renamed repository's
// path with a 301 to /repositories/<id> on the API origin, which the client still follows with
// the installation token, and reads.
func TestARenamedRepositorysRedirectIsStillFollowed(t *testing.T) {
	stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, renamed := strings.CutPrefix(r.URL.Path, "/repos/acme/old/"); renamed {
			http.Redirect(w, r, "/repositories/7/"+rest, http.StatusMovedPermanently)
			return
		}
		if r.URL.Path != "/repositories/7/git/ref/heads/main" || r.Header.Get("Authorization") != "Bearer "+redirectToken {
			t.Errorf("the followed read is %s with %q, want /repositories/7/git/ref/heads/main with the installation token", r.URL.Path, r.Header.Get("Authorization"))
		}
		fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":%q,"type":"commit"}}`, mainCommit)
	}))
	t.Cleanup(stand.Close)
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, stand.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if sha, err := client.Branch(context.Background(), redirectToken, "acme", "old", "main"); err != nil || sha != mainCommit {
		t.Fatalf("Branch of a renamed repository = %q, %v; want the 301 on the API origin followed and %s read", sha, err, mainCommit)
	}
}
