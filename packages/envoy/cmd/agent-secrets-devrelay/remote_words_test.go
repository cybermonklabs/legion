package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerRefusalPrintsItsWordsAsTheRuleReadsThem: a clean broker refuses the lookup with a
// code or words that quote a URL carrying a credential. machine-approve prints the code and the
// words as the rule for remote text reads them, with the status, and none of the credentials; the
// apiError prints none of them under any verb or in a value that holds it.
func TestABrokerRefusalPrintsItsWordsAsTheRuleReadsThem(t *testing.T) {
	token := tokenFile(t, "caller-token")
	type answer struct{ code, words, want string }
	rows := map[string]answer{
		"the code": {redacttest.Code, "not listed", redacttest.CodeShown + ": not listed (HTTP 403)"},
		"the words: user information and a query value": {"CALLER_NOT_LISTED", redacttest.Words, "CALLER_NOT_LISTED: " + redacttest.Shown + " (HTTP 403)"},
		"the words: a fragment's value":                 {"CALLER_NOT_LISTED", redacttest.FragmentWords, "CALLER_NOT_LISTED: " + redacttest.FragmentShown + " (HTTP 403)"},
		"the words: words the rule cannot read":         {"CALLER_NOT_LISTED", redacttest.Unreadable, "CALLER_NOT_LISTED: " + redact.WordsNotReadable + " (HTTP 403)"},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": row.code, "error": row.words})
			}))
			t.Cleanup(srv.Close)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"machine-approve", "--code", "KQ7M-X4PZ", "--broker", srv.URL, "--account-token-file", token, "--login", testApprover}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1\nstderr: %s", code, stderr.String())
			}
			out := stdout.String() + stderr.String()
			redacttest.RequireNone(t, "devrelay.machine-approve", out)
			if !strings.Contains(out, row.want) {
				t.Errorf("machine-approve printed %q, want %q in it", out, row.want)
			}
			redacttest.RequireNoneFormatted(t, "devrelay.apiError", &apiError{Status: http.StatusForbidden, base: brokeraddr.New(srv.URL), code: redact.NewRemoteText(row.code), reason: redact.NewRemoteText(row.words)})
		})
	}
}
