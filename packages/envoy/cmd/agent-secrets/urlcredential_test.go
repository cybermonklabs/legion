// packages/envoy/cmd/agent-secrets/urlcredential_test.go

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
)

// TestSelfPrintsNoPartOfTheBrokerURLCredential: `agent-secrets self` on a machine that proves an
// identity of its own trades it at the broker before anything else, and with the broker
// unreachable prints that trade's failure on stderr. The line names the failure, and no part of a
// user name, password, query or fragment AGENT_SECRETS_URL carries. The values are made up.
func TestSelfPrintsNoPartOfTheBrokerURLCredential(t *testing.T) {
	for form, address := range map[string]string{
		"a user name and password": "http://demouser7q:demopass9z-not-a-secret@127.0.0.1:1",
		"a query":                  "http://127.0.0.1:1/?token=demoquery4k",
		"a fragment":               "http://127.0.0.1:1#demofrag8w",
	} {
		t.Run(form, func(t *testing.T) {
			tmp := t.TempDir()
			projected := filepath.Join(tmp, "projected-token")
			if err := os.WriteFile(projected, []byte("projected.service.account.token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", tmp)
			t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
			t.Setenv("AGENT_SECRETS_SIGNIN_DIR", filepath.Join(tmp, "signins"))
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
			t.Setenv("AGENT_SECRETS_KEY_DIR", filepath.Join(tmp, "keys"))
			t.Setenv("AGENT_SECRETS_ENROLL_WAIT", "0s")
			t.Setenv("AGENT_SECRETS_URL", address)
			t.Setenv("AGENT_SECRETS_MACHINE_IDENTITY", "kubernetes")
			t.Setenv("AGENT_SECRETS_MACHINE_TOKEN_FILE", projected)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"self"}, &stdout, &stderr); code == 0 {
				t.Fatalf("self against a broker that never answers exited 0\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "connection refused") {
				t.Errorf("stderr does not name the failure:\n%s", stderr.String())
			}
			for _, marker := range []string{"demouser7q", "demopass9z-not-a-secret", "demoquery4k", "demofrag8w"} {
				if strings.Contains(stdout.String()+stderr.String(), marker) {
					t.Errorf("the output holds %q\nstdout: %s\nstderr: %s", marker, stdout.String(), stderr.String())
				}
			}
		})
	}
}

// TestTheCLIsBrokerClientFollowsNoRedirect: every call this command makes to the broker carries a
// Proof header of this session's own, so a 3xx answered for the broker - by the broker, or by
// anyone who can answer for it - must reach no other host. The client answers the 3xx to its
// caller instead, and the target receives nothing.
func TestTheCLIsBrokerClientFollowsNoRedirect(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var targetCalls atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer target.Close()
			var brokerCalls atomic.Int64
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				brokerCalls.Add(1)
				http.Redirect(w, r, target.URL+"/v1/enrollments/self", status)
			}))
			defer broker.Close()

			c := newClient(brokeraddr.New(broker.URL))
			_, _, _ = c.doStatus(context.Background(), http.MethodGet, broker.URL+"/v1/enrollments/self", nil, map[string]string{"Proof": "the-session-proof"})
			if got := targetCalls.Load(); got != 0 {
				t.Errorf("the redirect's target received %d requests, want none", got)
			}
			if got := brokerCalls.Load(); got != 1 {
				t.Errorf("the broker received %d requests, want 1", got)
			}
		})
	}
}

// TestABrokerRefusalPrintsNoCredentialItsWordsRepeat: the broker, or whoever answers at its
// address, refuses a call in its error envelope with words and an answer id that repeat the
// credential the configured address carries. Beside that address the refusal keeps its status
// and its code, and no line on stderr and no text of the error holds any part of the credential.
// Beside a clean address the broker's words and answer id are what a person acts on, and print
// whole. Either way the client still reads the words it tells refusals apart by.
func TestABrokerRefusalPrintsNoCredentialItsWordsRepeat(t *testing.T) {
	const answerID = "demopass9z-not-a-secret"
	var asked atomic.Int64
	var words string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/requests/req-1" {
			asked.Add(1)
		}
		body, _ := json.Marshal(map[string]string{"code": "REQUEST_INVALID", "error": words, "answer_id": answerID})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(answerIDHeader, answerID)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	credentialed := strings.Replace(srv.URL, "://", "://demouser7q:demopass9z-not-a-secret@", 1)
	words = "the request object names " + credentialed + ", which is not this broker"
	// status is `agent-secrets status req-1` against address: its stderr, and the refusal its
	// client reads.
	status := func(t *testing.T, address string) (string, *apiError) {
		t.Helper()
		t.Setenv("AGENT_SECRETS_URL", address)
		t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
		t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
		before := asked.Load()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"status", "req-1"}, &stdout, &stderr); code != 1 {
			t.Fatalf("status against a broker that refuses it exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
		}
		if asked.Load() == before {
			t.Fatalf("status never reached the broker\nstderr: %s", stderr.String())
		}
		_, err := newClient(brokeraddr.New(address)).do(context.Background(), http.MethodGet, address+"/v1/requests/req-1", nil, nil)
		var refusal *apiError
		if !errors.As(err, &refusal) {
			t.Fatalf("the refusal read as %v, want an *apiError", err)
		}
		if refusal.Status != http.StatusForbidden || refusal.Code != "REQUEST_INVALID" {
			t.Errorf("the refusal is %d %s, want the status and the code the broker answered, 403 REQUEST_INVALID", refusal.Status, refusal.Code)
		}
		if !refusal.says("which is not this broker") {
			t.Errorf("the refusal does not read the broker's words it is told apart by")
		}
		return stderr.String(), refusal
	}

	t.Run("an address carrying a credential", func(t *testing.T) {
		stderr, refusal := status(t, credentialed)
		printed := stderr + "\n" + refusal.Error() + "\n" + fmt.Sprintf("%+v %#v", refusal, *refusal)
		for _, marker := range []string{"demouser7q", "demopass9z-not-a-secret"} {
			if strings.Contains(printed, marker) {
				t.Errorf("the refusal holds %q:\n%s", marker, printed)
			}
		}
		if !strings.Contains(stderr, "REQUEST_INVALID") {
			t.Errorf("stderr does not name the broker's code:\n%s", stderr)
		}
	})

	t.Run("a clean address", func(t *testing.T) {
		stderr, refusal := status(t, srv.URL)
		if refusal.Message != words || refusal.AnswerID != answerID {
			t.Errorf("the refusal is %+v, want the broker's words %q and answer id %q whole", refusal, words, answerID)
		}
		if !strings.Contains(stderr, words+" (REQUEST_INVALID) (answer "+answerID+")") {
			t.Errorf("stderr does not carry the broker's words, code and answer id:\n%s", stderr)
		}
	})
}
