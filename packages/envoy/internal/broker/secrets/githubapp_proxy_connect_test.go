package secrets_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestATokenRequestWhoseProxyRefusedItsTunnelIsSentAgain: a broker that reaches GitHub through an
// HTTPS proxy asks the proxy for a tunnel (CONNECT) before it sends anything to GitHub, and a
// proxy that refuses that tunnel answers with a status the transport returns as an error of no
// type of its own. GitHub received nothing, so the check of a narrowing sends its token request
// again, as a release does, rather than read the refusal as a request GitHub may have minted
// for: a transport the minter owns, as its own default one is, reports the connection it got to
// the request's trace before it writes a byte, and this one never got one. This test's transport
// is one it made itself and registers no protocol on, so it counts as owned. Each run mints
// exactly one token, at the second tunnel.
func TestATokenRequestWhoseProxyRefusedItsTunnelIsSentAgain(t *testing.T) {
	key, err := testAppKey()
	if own, callers := secrets.OwnClientTracesConnectionsForTest(); !own || callers {
		t.Fatalf("the minters' own client is traced: %t, a caller's own *http.Transport is traced: %t; want true and false", own, callers)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []bool{false, true} {
		t.Run(fmt.Sprintf("a check: %t", check), func(t *testing.T) {
			var minted, tunnels, refused atomic.Int32
			github := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_, _ = io.WriteString(w, `[{"id":2,"account":{"login":"ada"}}]`)
				case http.MethodPost:
					minted.Add(1)
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprintf(w, `{"token":"ghs_proxy_connect_test","expires_at":%q,"repository_selection":"selected","repositories":[{"name":"lean"}]}`,
						time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
				case http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer github.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					http.Error(w, "not a CONNECT", http.StatusMethodNotAllowed)
					return
				}
				// The first tunnel carries the installations listing; the second, the token
				// request's, is refused before anything reaches GitHub.
				if tunnels.Add(1) == 2 {
					refused.Add(1)
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				upstream, err := net.Dial("tcp", r.Host)
				if err != nil {
					http.Error(w, "no route to the stand-in", http.StatusBadGateway)
					return
				}
				defer upstream.Close()
				client, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer client.Close()
				if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					return
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(upstream, buffered); done <- struct{}{} }()
				go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
				<-done
			}))
			defer proxy.Close()
			proxyURL, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			transport := github.Client().Transport.(*http.Transport).Clone()
			transport.Proxy = http.ProxyURL(proxyURL)
			transport.DisableKeepAlives = true
			defer transport.CloseIdleConnections()
			secrets.TraceTransportForTest(transport)
			retry, _, _ := testRetry(atTheBottom)
			app := secrets.GitHubApp{Base: github.URL, HTTP: &http.Client{Transport: transport}, Retry: retry}

			result, err := app.Mint(t.Context(), secrets.MintRequest{
				Value: githubAppValue(t, "123456", key), GitHubOwners: []string{"ada"}, Check: check,
				GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"lean"}},
			})
			if refused.Load() != 1 {
				t.Fatalf("the proxy refused %d tunnels, want the token request's one: the row reads nothing", refused.Load())
			}
			if err != nil || minted.Load() != 1 {
				t.Fatalf("the run answered %v (class %s) and GitHub minted %d tokens; want the token request sent again through a new tunnel, and one token",
					err, secrets.MintErrorClass(err), minted.Load())
			}
			if !check {
				app.GiveBack(t.Context(), "APP_KEY", result.Value)
			}
		})
	}
}
