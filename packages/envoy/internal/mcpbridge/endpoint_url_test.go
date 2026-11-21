package mcpbridge

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// startAgainst starts an HTTPServer against an MCP server whose SSE endpoint event names
// endpoint(host), host being the server's own address, and whose message route refuses every POST
// with 400, and answers Start's error.
func startAgainst(t *testing.T, endpoint func(host string) string) (string, error) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint(r.Host))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "refused", http.StatusBadRequest)
	})
	server := httptest.NewServer(mux)
	defer func() {
		server.CloseClientConnections()
		server.Close()
	}()
	s := NewHTTPServer(ServerConfig{Name: "endpoint", Transport: "http", URL: server.URL}, func(string) {})
	err := s.Start()
	s.closeSSE()
	return server.Listener.Addr().String(), err
}

// TestAFailedPostNamesTheEndpointAsTheRuleReadsIt: the MCP server's SSE endpoint event names the URL
// the bridge posts to, with a credential in its user information and in its query, and the POST
// fails: the server refuses it (refused), or nothing answers at that address (unreachable). Start's
// error names the endpoint as the rule reads it (redact.Field), with no user information and no
// query value, and the failure by its status and the server's words or by its class, and none of
// the credentials. An endpoint that carries none prints as the server wrote it (control). Captures
// mcpbridge.endpoint-url/refused and mcpbridge.endpoint-url/unreachable.
func TestAFailedPostNamesTheEndpointAsTheRuleReadsIt(t *testing.T) {
	const param = "access" + "_token"
	credentialed := func(host string) string {
		return "http://" + redacttest.User + ":" + redacttest.Password + "@" + host + "/message?" + param + "=" + redacttest.Token
	}
	t.Run("refused", func(t *testing.T) {
		host, err := startAgainst(t, credentialed)
		if err == nil {
			t.Fatal("Start succeeded against a server that refuses every POST")
		}
		redacttest.RequireNone(t, "mcpbridge.endpoint-url/refused", err.Error())
		if want := "POST to http://" + host + "/message?" + param + "=: status 400: refused"; !strings.Contains(err.Error(), want) {
			t.Errorf("Start = %q; want it to name the endpoint as the rule reads it and the refusal: %q", err, want)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		_, err := startAgainst(t, func(string) string { return credentialed("127.0.0.1:1") })
		if err == nil {
			t.Fatal("Start succeeded against an endpoint nothing answers at")
		}
		redacttest.RequireNone(t, "mcpbridge.endpoint-url/unreachable", err.Error())
		if want := "POST to http://127.0.0.1:1/message?" + param + "=: " + redact.ClassRefused; !strings.Contains(err.Error(), want) {
			t.Errorf("Start = %q; want it to name the endpoint as the rule reads it and the failure's class: %q", err, want)
		}
	})
	t.Run("control", func(t *testing.T) {
		host, err := startAgainst(t, func(host string) string { return "http://" + host + "/message" })
		if want := "POST to http://" + host + "/message: status 400: refused"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Start = %v; want an endpoint with no credential named as the server wrote it: %q", err, want)
		}
	})
}
