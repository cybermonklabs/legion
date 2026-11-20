package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// cliWordsRows are the broker's words that quote a URL carrying a credential, each with what the
// rule for remote text prints of them.
var cliWordsRows = []struct{ name, words, shown string }{
	{"user information and a query value", redacttest.Words, redacttest.Shown},
	{"a fragment's value", redacttest.FragmentWords, redacttest.FragmentShown},
	{"words the rule cannot read", redacttest.Unreadable, redact.WordsNotReadable},
}

// refusingBroker answers every request with status and the broker's error envelope holding code,
// words and, where it is not empty, answerID.
func refusingBroker(t *testing.T, status int, code, words, answerID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envelope := map[string]string{"code": code, "error": words}
		if answerID != "" {
			envelope["answer_id"] = answerID
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// credentialedAddress is srv's address with the probe's user information in it.
func credentialedAddress(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1)
}

// statusStderr is `agent-secrets status req-1` against address: its stderr.
func statusStderr(t *testing.T, address string) string {
	t.Helper()
	t.Setenv("AGENT_SECRETS_URL", address)
	t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "req-1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("status exited %d, want 1\nstderr: %s", code, stderr.String())
	}
	return stderr.String()
}

// podLogin is `agent-secrets enroll --pod` against address: its stderr.
func podLogin(t *testing.T, address string) string {
	t.Helper()
	t.Setenv("AGENT_SECRETS_URL", address)
	t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("the pod login exited %d, want 1 (stderr %q)", code, stderr.String())
	}
	return stderr.String()
}

// TestAStatusRefusalPrintsTheBrokersWordsAsTheRuleReadsThem: `agent-secrets status` is refused by
// a clean broker with a code, words and an answer id, one of which quotes a URL carrying a
// credential. Its stderr and the apiError its client reads print each as the rule reads it, the
// error under every verb and in a value that holds it, and none of the credentials.
func TestAStatusRefusalPrintsTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	type answer struct{ code, words, answerID, want string }
	rows := map[string]answer{}
	for _, row := range cliWordsRows {
		rows["the words: "+row.name] = answer{"REQUEST_INVALID", row.words, "", row.shown + " (REQUEST_INVALID)"}
		rows["no code, the words: "+row.name] = answer{"", row.words, "", "HTTP 403: " + row.shown}
	}
	rows["the code"] = answer{redacttest.Code, "refused", "", "refused (" + redacttest.CodeShown + ")"}
	rows["the answer id"] = answer{"REQUEST_INVALID", "refused", redacttest.Code, "refused (REQUEST_INVALID) (answer " + redacttest.CodeShown + ")"}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			srv := refusingBroker(t, http.StatusForbidden, row.code, row.words, row.answerID)
			stderr := statusStderr(t, srv.URL)
			redacttest.RequireNone(t, "agent-secrets.status-stderr", stderr)
			if !strings.Contains(stderr, row.want) {
				t.Errorf("status's stderr %q does not carry %q", stderr, row.want)
			}
			_, err := newClient(brokeraddr.New(srv.URL)).do(context.Background(), http.MethodGet, srv.URL+"/v1/requests/req-1", nil, nil)
			var refused *apiError
			if !errors.As(err, &refused) || !refused.hasCode(row.code) {
				t.Fatalf("the client read %v, want the broker's refusal", err)
			}
			redacttest.RequireNoneFormatted(t, "agent-secrets.apiError", refused)
		})
	}
}

// TestARefusalBesideAnAddressThatIsNotCleanPrintsItsStatusAlone: whoever answers at an address
// that carries a credential refuses with words that repeat a credential. `agent-secrets status`
// and a pod login print the status and brokeraddr.WordsNotPrinted, and no word of the answer.
func TestARefusalBesideAnAddressThatIsNotCleanPrintsItsStatusAlone(t *testing.T) {
	srv := refusingBroker(t, http.StatusForbidden, "REQUEST_INVALID", redacttest.Words, "")
	address := credentialedAddress(srv)
	for capture, printed := range map[string]func(*testing.T) string{
		"agent-secrets.withheld-status": func(t *testing.T) string { return statusStderr(t, address) },
		"agent-secrets.withheld-pod":    func(t *testing.T) string { return podLogin(t, address) },
	} {
		t.Run(capture, func(t *testing.T) {
			stderr := printed(t)
			redacttest.RequireNone(t, capture, stderr)
			if want := "HTTP 403: " + brokeraddr.WordsNotPrinted; !strings.Contains(stderr, want) {
				t.Errorf("the stderr %q does not carry %q", stderr, want)
			}
		})
	}
}

// TestARenewRefusalPrintsTheBrokersWordsAsTheRuleReadsThem: the broker ends an enrollment with
// words that quote a URL carrying a credential. The renew loop's stderr line names the status,
// the code and the words as the rule reads them, and none of the credentials.
func TestARenewRefusalPrintsTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	for _, row := range cliWordsRows {
		t.Run(row.name, func(t *testing.T) {
			srv := refusingBroker(t, http.StatusUnauthorized, "ENROLLMENT_NOT_LIVE", row.words, "")
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("pod-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
			_, after := steppedWaits(1)
			var stderr bytes.Buffer
			if code := renewLoop(context.Background(), client, signer, enrollmentID, tokenFile, &stderr, after); code != exitRenewRefused {
				t.Fatalf("renew loop exit = %d, want %d\nstderr: %s", code, exitRenewRefused, stderr.String())
			}
			redacttest.RequireNone(t, "agent-secrets.renew-stderr", stderr.String())
			if want := "HTTP 401 ENROLLMENT_NOT_LIVE: " + row.shown; !strings.Contains(stderr.String(), want) {
				t.Errorf("renew's stderr %q does not carry %q", stderr.String(), want)
			}
		})
	}
}

// TestAPodRefusalPrintsTheBrokersWordsAsTheRuleReadsThem: the broker refuses a pod's login with a
// code or words that quote a URL carrying a credential. The pod login's stderr line names the
// status, the code and the words as the rule reads them, and none of the credentials.
func TestAPodRefusalPrintsTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	type answer struct{ code, words, want string }
	rows := map[string]answer{"the code": {redacttest.Code, "refused", "HTTP 403 " + redacttest.CodeShown + ": refused"}}
	for _, row := range cliWordsRows {
		rows["the words: "+row.name] = answer{"REQUEST_INVALID", row.words, "HTTP 403 REQUEST_INVALID: " + row.shown}
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			srv := refusingBroker(t, http.StatusForbidden, row.code, row.words, "")
			stderr := podLogin(t, srv.URL)
			redacttest.RequireNone(t, "agent-secrets.pod-stderr", stderr)
			if !strings.Contains(stderr, row.want) {
				t.Errorf("the pod login's stderr %q does not carry %q", stderr, row.want)
			}
		})
	}
}
