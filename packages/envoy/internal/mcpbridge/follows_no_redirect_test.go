package mcpbridge

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAPostFollowsNoRedirectOffItsEndpointsOrigin: the MCP server's SSE endpoint event names the
// URL the bridge posts each message to, with the session id in its query (session), or with a
// password in its user information, which net/http sends as the POST's Basic Authorization
// (basic). Whatever answers at the endpoint answers the POST with a 307 to a stand-in on another
// port of the same host, which keeps what it is sent (redirectstest.Sent), its Location quoting
// the credential the POST carried. A client that follows the 307 sends the stand-in the message
// again, the Location's query, and the endpoint, query included, as its Referer. The stand-in is
// sent nothing, and Start fails on the 307 by its status, naming nothing of the Location. The
// twin shows a client that follows the 307 sends the stand-in the session id, so the rows can see
// one sent on. Captures mcpbridge.redirect-followed/<case>, what the stand-in was sent.
func TestAPostFollowsNoRedirectOffItsEndpointsOrigin(t *testing.T) {
	const session = "sess-not-a-secret-token-7Qz4k"
	target, sent := redirectstest.Sent(t)
	serve := func(endpoint func(host string) string, credential func(r *http.Request) string) string {
		mux := http.NewServeMux()
		mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint(r.Host))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, redirectstest.To(target, credential(r)), http.StatusTemporaryRedirect)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(func() {
			server.CloseClientConnections()
			server.Close()
		})
		return server.URL
	}
	sessioned := serve(func(host string) string {
		return "http://" + host + "/message?sessionId=" + session
	}, func(r *http.Request) string { return r.URL.Query().Get("sessionId") })
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, sessioned+"/message?sessionId="+session, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		redirectstest.RequireSentOn(t, request, session, sent)
	})
	basic := serve(func(host string) string {
		return "http://" + redacttest.User + ":" + redacttest.Password + "@" + host + "/message"
	}, func(r *http.Request) string { _, password, _ := r.BasicAuth(); return password })
	for _, c := range []struct {
		name, server, credential string
	}{
		{"session", sessioned, session},
		{"basic", basic, redacttest.Password},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := NewHTTPServer(ServerConfig{Name: "redirect", Transport: "http", URL: c.server}, func(string) {})
			err := s.Start()
			if err == nil {
				s.Stop()
			}
			text := sent()
			redacttest.RequireNoPiece(t, "mcpbridge.redirect-followed/"+c.name, text, c.credential)
			if text != "" {
				t.Errorf("the stand-in the Location names was sent: %q", text)
			}
			if err == nil || !strings.Contains(err.Error(), ": status 307: ") {
				t.Fatalf("Start = %v; want it refused on the endpoint's 307", err)
			}
			if strings.Contains(err.Error(), "moved") || redacttest.Piece(err.Error(), c.credential) != "" {
				t.Errorf("Start's error names the Location or the credential: %q", err)
			}
		})
	}
}
