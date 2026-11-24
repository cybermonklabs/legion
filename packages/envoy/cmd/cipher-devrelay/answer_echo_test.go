package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAListedCallersRefusalPrintsNoBearerItsAnswerEchoes: an answer in the broker's place refuses
// a listed caller's call with an envelope whose code and words echo the bearer the call carried.
// The *apiError prints neither under Error, %v or %+v: apiClient.do reads the envelope through the
// record of the request it sent (redact.Carried), while the status still prints. Captures
// agent-secrets-devrelay.answer-echo/<case>.
func TestAListedCallersRefusalPrintsNoBearerItsAnswerEchoes(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"words", `{"code":"UNAUTHORIZED","error":"the bearer {auth} is not on the list"}`},
		{"code", `{"code":"{auth}","error":"not on the list"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, strings.ReplaceAll(c.body, "{auth}", r.Header.Get("Authorization")))
			}))
			defer server.Close()
			client := &apiClient{base: cipheraddr.New(server.URL), http: &http.Client{}, token: redacttest.Token}
			_, err := client.do(context.Background(), http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": "ABCD-EFGH"})
			if err == nil {
				t.Fatal("the call answered no error")
			}
			capture := "agent-secrets-devrelay.answer-echo/" + c.name
			redacttest.RequireNone(t, capture, fmt.Sprintf("%s | %v | %+v", err.Error(), err, err))
			if !strings.Contains(err.Error(), "(HTTP 401)") {
				t.Errorf("the refusal is %q, want its status", err)
			}
		})
	}
}

// TestALookedUpStatePrintsAsTheAddressLetsItPrint: whatever answers at --broker accepts the lookup
// of the typed code and names a state that is not pending and repeats a credential. machine-approve
// refuses to decide it, and its line names the state as the address lets the answer's text print
// (cipheraddr.Address.WordsOf), as the call's refusals print theirs: beside an address that carries
// a credential in its user information or its fragment, cipheraddr.WordsNotPrinted, since whoever
// answers there can repeat that credential in any text of its answer, and a fragment, which no
// request sends, is a credential the request's record cannot redact; beside a clean address, the
// state as the rule reads it. Captures devrelay.machine-approve-state/<case>.
func TestALookedUpStatePrintsAsTheAddressLetsItPrint(t *testing.T) {
	token := tokenFile(t, "caller-token")
	for _, c := range []struct{ name, suffix, state, want string }{
		{"user-information", "", redacttest.Password, cipheraddr.WordsNotPrinted},
		{"fragment", "#access_token=" + redacttest.Token, redacttest.Token, cipheraddr.WordsNotPrinted},
		{"clean", "", redacttest.Code, redacttest.CodeShown},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"record_id": "record-1", "state": c.state})
			}))
			defer server.Close()
			broker := server.URL + c.suffix
			if c.name == "user-information" {
				broker = strings.Replace(server.URL, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1)
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"machine-approve", "--code", "KQ7M-X4PZ", "--broker", broker, "--account-token-file", token, "--login", testApprover}, &stdout, &stderr); code != 1 {
				t.Fatalf("--broker %s: exit %d, want 1\nstdout: %s\nstderr: %s", broker, code, stdout.String(), stderr.String())
			}
			out := stdout.String() + stderr.String()
			redacttest.RequireNone(t, "devrelay.machine-approve-state/"+c.name, out)
			if want := "names a machine login that is " + c.want + ", not pending"; !strings.Contains(out, want) {
				t.Errorf("machine-approve printed %q, want %q in it", out, want)
			}
		})
	}
}
