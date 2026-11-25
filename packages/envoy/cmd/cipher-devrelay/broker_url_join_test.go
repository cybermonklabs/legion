package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerCallGoesToThePathJoinedOntoTheBrokerAddress: --broker can carry a path and a query.
// Each call machine-approve makes, the lookup of the typed code and the decision, goes to the
// address's path with the call's route joined onto it and the address's query kept:
// http://host/prefix?key=... asks /prefix/v1/machine-logins/lookup?key=... and
// /prefix/v1/credential-requests/<record>/approve?key=... (query), where the route appended to the
// address's text sat inside the query and the broker was asked /prefix with the query's value
// changed. An address with a path and no query asks where it did (no-query). A stand-in records
// the path and the query each call reaches it with.
func TestABrokerCallGoesToThePathJoinedOntoTheBrokerAddress(t *testing.T) {
	token := tokenFile(t, "caller-token")
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/prefix?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"no-query", "/prefix", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, *r.URL)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"record_id": "record-1", "state": "pending"})
			}))
			t.Cleanup(srv.Close)
			var stdout, stderr bytes.Buffer
			code := run([]string{"machine-approve", "--code", "KQ7M-X4PZ", "--broker", srv.URL + c.suffix, "--account-token-file", token, "--login", testApprover}, &stdout, &stderr)
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/prefix/v1/machine-logins/lookup", "/prefix/v1/credential-requests/record-1/approve"}
			if len(asked) != len(want) {
				t.Fatalf("--broker ending %q: the broker was asked %d calls (%v), want the lookup and the decision; exit %d, stderr %q", c.suffix, len(asked), asked, code, stderr.String())
			}
			for i, got := range asked {
				if got.Path != want[i] || got.RawQuery != c.query {
					t.Errorf("--broker ending %q: call %d reached path %q with query %q; want path %q with query %q", c.suffix, i, got.Path, got.RawQuery, want[i], c.query)
				}
			}
			if code != 0 {
				t.Errorf("--broker ending %q: exit %d, stderr %q; want 0", c.suffix, code, stderr.String())
			}
		})
	}
}

// TestABrokerAddressThatDoesNotParsePrintsNoneOfIt: --broker with redacttest's credentials in its
// user information and its query. One url.Parse refuses (an invalid escape in its path) fails the
// lookup by the setting and the class alone, before any request (unparsed); http.NewRequest's
// error for the address with the route appended quotes it whole (twin). One url.Parse takes,
// whose joined URL, as url.URL.String writes it, url.Parse refuses again (an IPv6 zone holding a
// byte that is not ASCII, which String escapes as %FD), prints through the Address's Error as the
// call's other failures do, the URL as redact.NotPrinted (reparsed); its twin shows
// http.NewRequest's error for the joined URL holds the credentials. Captures
// devrelay.broker-url/<case>.
func TestABrokerAddressThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	token := tokenFile(t, "caller-token")
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/broker%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/broker?" + param + "=" + redacttest.Token
	approve := func(t *testing.T, broker string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"machine-approve", "--code", "KQ7M-X4PZ", "--broker", broker, "--account-token-file", token, "--login", testApprover}, &stdout, &stderr); code != 1 {
			t.Fatalf("machine-approve exit %d, want 1; stderr %q", code, stderr.String())
		}
		return stdout.String() + stderr.String()
	}
	np := `"` + redact.NotPrinted + `": ` + redact.ClassNotAURL
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodPost, unparsed+"/v1/machine-logins/lookup", nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := approve(t, unparsed)
		redacttest.RequireNoPiece(t, "devrelay.broker-url/unparsed", said)
		if want := "lookup: --broker (or AGENT_SECRETS_URL) " + np + "\n"; !strings.HasSuffix(said, want) {
			t.Errorf("machine-approve printed %q; want it to end %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the address (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodPost, parsed.JoinPath("v1/machine-logins/lookup").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := approve(t, reparsed)
		redacttest.RequireNoPiece(t, "devrelay.broker-url/reparsed", said)
		if want := "lookup: parse " + np + "\n"; !strings.HasSuffix(said, want) {
			t.Errorf("machine-approve printed %q; want it to end %q", said, want)
		}
	})
}
