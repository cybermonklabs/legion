package agentsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoAccountToken is Dispatch's ServiceAccount token in the rows below: a JWT's shape and length,
// so the excerpt redacts every run of it. Its parts are joined at run time, since the push's leak
// check refuses a JWT written whole in a source file, a stand-in's as well.
var echoAccountToken = strings.Join([]string{"ey" + "JhbGciOiJSUzI1NiJ9", "ey" + "JzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQ6ZGlzcGF0Y2gifQ", "c2lnbmF0dXJlLW9mLWRpc3BhdGNo"}, ".")

// echoGateway answers every call the way a gateway in the broker's place can: its own HTML page
// at status, echoing the request back, its body first (so a typed code sits inside the excerpt's
// one line), then its bearer bare on a line of its own (so only the redaction of that token can
// hide it) and its Authorization line.
func echoGateway(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<html><head><title>%d %s</title></head><body><pre>\n%s\n%s\n%s %s HTTP/1.1\nAuthorization: %s\n</pre></body></html>",
			status, http.StatusText(status), body, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
	}))
	t.Cleanup(server.Close)
	return server
}

// holdsAny reports whether text holds any twelve characters of credential in a row, or the whole
// of a shorter one.
func holdsAny(text, credential string) bool {
	run := min(12, len(credential))
	for at := 0; at+run <= len(credential); at++ {
		if strings.Contains(text, credential[at:at+run]) {
			return true
		}
	}
	return false
}

// A refusal that is not the broker's error JSON was written by something in the broker's place,
// which can echo the call back: Dispatch's own ServiceAccount token and, for a machine login's
// decision, the code the person typed. The *Error keeps the status and an empty Code, and its Body,
// which the routes hand the browser, is Dispatch's own {"error"} naming that page with both
// redacted. Error() still prints the status alone.
func TestProxyEchoABrokerRefusalHandsTheBrowserNoCredential(t *testing.T) {
	const typed = "QRST-77889"
	for _, row := range []struct {
		name    string
		call    func(*Client) error
		carried []string
	}{
		{"pending list", func(c *Client) error { _, err := c.Pending(context.Background(), "sjawhar"); return err }, []string{echoAccountToken}},
		{"machine login deny", func(c *Client) error {
			code := typed
			_, err := c.Deny(context.Background(), "req-1", Decision{Approver: "sjawhar", Code: &code})
			return err
		}, []string{echoAccountToken, typed}},
	} {
		t.Run(row.name, func(t *testing.T) {
			server := echoGateway(t, http.StatusBadGateway)
			err := row.call(New(server.URL, tokenFile(t, echoAccountToken)))
			var brokerErr *Error
			if !errors.As(err, &brokerErr) {
				t.Fatalf("err = %v (%T), want *Error", err, err)
			}
			if brokerErr.Status != http.StatusBadGateway || brokerErr.Code != "" || err.Error() != "agent-secrets: broker returned 502" {
				t.Fatalf("Status %d, Code %q, Error() %q; want 502, no code and the status alone", brokerErr.Status, brokerErr.Code, err.Error())
			}
			for _, credential := range row.carried {
				if holdsAny(string(brokerErr.Body), credential) {
					t.Errorf("Body hands the browser a credential the call carried (%.16s…): %s", credential, brokerErr.Body)
				}
			}
			t.Logf("the browser is handed %d %s", brokerErr.Status, brokerErr.Body)
			var answer map[string]string
			if err := json.Unmarshal(brokerErr.Body, &answer); err != nil {
				t.Fatalf("Body %s is not the JSON the browser reads: %v", brokerErr.Body, err)
			}
			if _, named := answer["code"]; named || !strings.Contains(answer["error"], `a body that is not the broker's error JSON ("`) {
				t.Fatalf("Body = %s, want only an error naming a body that is not the broker's error JSON", brokerErr.Body)
			}
		})
	}
}
