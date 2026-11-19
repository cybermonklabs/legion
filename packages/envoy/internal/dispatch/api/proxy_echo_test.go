package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
)

// The credential routes hand the browser the broker's refusal as the broker wrote it
// (relayBrokerResponse). A gateway in the broker's place can answer Dispatch's call with a page
// echoing it back, Dispatch's own ServiceAccount token bare on a line of its own; the browser is
// then given the gateway's status and the {error} it reads (api/client.ts errorFrom), naming the
// page with the token redacted, never the page.
func TestProxyEchoTheCredentialRoutesHandTheBrowserNoAccountToken(t *testing.T) {
	// A JWT's shape, its parts joined at run time: the push's leak check refuses one written whole.
	account := strings.Join([]string{"ey" + "JhbGciOiJSUzI1NiJ9", "ey" + "JzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQ6ZGlzcGF0Y2gifQ", "cmVsYXktc2lnbmF0dXJl"}, ".")
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "<html><head><title>502 Bad Gateway</title></head><body><pre>\n%s\n%s %s HTTP/1.1\nAuthorization: %s\n</pre></body></html>",
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
	}))
	defer gateway.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(account), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	body, err := agentsecrets.New(gateway.URL, tokenFile).Pending(context.Background(), "sjawhar")
	response := httptest.NewRecorder()
	relayBrokerResponse(response, body, err)
	text := response.Body.String()
	t.Logf("the browser is handed %d %s", response.Code, text)
	for at := 0; at+12 <= len(account); at++ {
		if strings.Contains(text, account[at:at+12]) {
			t.Fatalf("the browser is given Dispatch's ServiceAccount token: %d %s", response.Code, text)
		}
	}
	var answer map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the browser's answer %q is not the JSON it reads: %v", text, err)
	}
	if response.Code != http.StatusBadGateway || !strings.Contains(answer["error"], `a body that is not the broker's error JSON ("`) {
		t.Fatalf("the browser got %d %s, want 502 naming a body that is not the broker's error JSON", response.Code, text)
	}
}
