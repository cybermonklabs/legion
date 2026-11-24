package leakguard

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// hopToken is the installation token the redirect rows' Source mints. It holds the gate's marker
// -RDX-, so the text a row captures shows it where a stand-in was sent it.
const hopToken = "ghs_k7v2p-not-a-secret-RDX-f3h8"

// hopSource is a Source reading checkRepository at api with hopToken, over client (nil is the
// daemon's own, http.DefaultClient).
func hopSource(api string, client *http.Client) *Source {
	return &Source{Repository: checkRepository, API: api, Client: client, Token: func(context.Context) (string, error) {
		return hopToken, nil
	}}
}

// TestTheTarballReadSendsNoRedirectTheToken: whatever answers the daemon's read of the check's
// repository at GitHub's API address answers it 302 to a stand-in on the same host name at another
// port, whose requests the stand-in keeps (redirectstest.Sent), the Location the stand-in's
// address and the read's own path (other-port). The token can reach the stand-in only in the
// Authorization net/http copies onto the redirect's request, which it drops only on the way to
// another host name, and a port is no part of one. The read follows the redirect, as it follows
// GitHub's to its download host, and sends the stand-in no Authorization; it is refused with the
// stand-in's 500. The twin shows a client that follows the 302 under net/http's own policy sends
// the stand-in the token, so the row can see one sent on. The control is GitHub's own redirect: a
// 302 to another host name whose Location carries the download's own signed query, which the read
// still follows to the tarball. Captures leakguard.redirect-authorization/other-port, what the
// stand-in was sent.
func TestTheTarballReadSendsNoRedirectTheToken(t *testing.T) {
	target, sent := redirectstest.Sent(t)
	// api is the stand-in at GitHub's API address: it answers the tarball read 302 to the stand-in
	// that keeps what it is sent, at the read's own path.
	api := githubtest.Server(t, githubtest.Spec{Token: hopToken, Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tarballRoute+theTag {
			githubtest.Unexpected(t, w, r)
			return
		}
		http.Redirect(w, r, "http://"+target+r.URL.RequestURI(), http.StatusFound)
	}})
	apiURL, err := url.Parse(api)
	if err != nil {
		t.Fatal(err)
	}
	if host, _, _ := net.SplitHostPort(target); host != apiURL.Hostname() || target == apiURL.Host {
		t.Fatalf("the stand-in is at %s and the API at %s: the row is a redirect to the same host name at another port", target, apiURL.Host)
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequestWithContext(bound(t), http.MethodGet, api+tarballRoute+theTag, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+hopToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("a client that follows the 302 failed: %v", err)
		}
		_ = response.Body.Close()
		if text := sent(); !strings.Contains(text, hopToken) {
			t.Fatalf("a client that follows the 302 under net/http's own policy sent the stand-in no token, so the row cannot see one sent on: %q", text)
		}
	})
	t.Run("other-port", func(t *testing.T) {
		_, err := hopSource(api, nil).Get(bound(t), theTag)
		text := sent()
		oauth2calltest.RequireNoPiece(t, "leakguard.redirect-authorization/other-port", text, hopToken)
		if !strings.HasPrefix(text, "GET "+tarballRoute+theTag+" ") {
			t.Errorf("the stand-in the Location names was sent %q, want the read's redirect followed to it", text)
		}
		if want := RefusalPrefix + "GitHub answered the read of " + checkRepository.String() + " at " + theTag + " with HTTP 500"; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Get = %v, want it refused with the stand-in's 500 (%q)", err, want)
		}
	})
	t.Run("control", func(t *testing.T) {
		const downloads = "codeload.leak-check.test"
		const signed = "token=AZ7QK3XWP2VDMB4LNEJ5"
		raw := leakguardtest.GitHubTarball(t, theCheck(answersItsHelp))
		var refused atomic.Value
		download := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != signed || r.Header.Get("Authorization") != "" {
				refused.Store(r.URL.RawQuery + " " + r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write(raw)
		}})
		github := githubtest.Server(t, githubtest.Spec{Token: hopToken, Handle: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tarballRoute+theTag {
				githubtest.Unexpected(t, w, r)
				return
			}
			http.Redirect(w, r, "http://"+downloads+"/example-org/deployment/legacy.tar.gz/refs/tags/"+theTag+"?"+signed, http.StatusFound)
		}})
		dialer := &net.Dialer{}
		transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == downloads+":80" {
				addr = download.Listener.Addr().String()
			}
			return dialer.DialContext(ctx, network, addr)
		}}
		t.Cleanup(transport.CloseIdleConnections)

		got, err := hopSource(github, &http.Client{Transport: transport}).Get(bound(t), theTag)
		if err != nil {
			t.Fatalf("the read through GitHub's redirect to its download host: %v (the download host refused %v)", err, refused.Load())
		}
		if string(got.Body) != string(served(t, raw)) {
			t.Fatalf("the read served %d bytes, want the download host's tarball packed", len(got.Body))
		}
	})
}

// TestTheTarballReadFollowsTenRedirectsAtMost: the read keeps net/http's own bound on the
// redirects it follows. An API address that answers every read with a redirect to itself is asked
// ten times, and the read is refused rather than followed until its bound.
func TestTheTarballReadFollowsTenRedirectsAtMost(t *testing.T) {
	var asked atomic.Int32
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Redirect(w, r, r.URL.RequestURI(), http.StatusFound)
	}})
	if _, err := hopSource(api, nil).Get(bound(t), theTag); err == nil {
		t.Fatal("the read redirected without end was served")
	}
	if n := asked.Load(); n != 10 {
		t.Fatalf("the API was asked %d times, want 10: net/http's own bound", n)
	}
}
