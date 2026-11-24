package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestADaemonRefusalIsReadFromItsWordsAsTheyCame: the daemon refuses the controller's calls with
// refusals whose sentence names a URL right before the JSON quote that ends it, which the rule
// cannot read as clean, so it prints the whole body as not readable. What each refusal is, is
// decided on the body as it came (oauth2call.Answer.Decode): the credential refresh's code
// CONTROLLER_LAUNCHED ends the refresh (controllerCredentialSuperseded), and the secret's 400
// naming the unknown field pluginContract is a daemon older than the contract. What each error
// prints is the rule's reading, which names no URL.
func TestADaemonRefusalIsReadFromItsWordsAsTheyCame(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/legion/v1/controller/github-credential":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"CONTROLLER_LAUNCHED","error":"the daemon launches its controller; see https://daemon.example/docs#controller"}`))
		case "/legion/v1/controller/secret":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"json: unknown field \"pluginContract\"; see https://daemon.example/docs#contracts"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	_, _, err := fetchControllerCredential(context.Background(), daemon.URL, "a-secret", "")
	if !controllerCredentialSuperseded(err) {
		t.Errorf("the credential refresh refused CONTROLLER_LAUNCHED beside a URL = %v, want the refresh ended", err)
	}
	if err != nil && strings.Contains(err.Error(), "docs#controller") {
		t.Errorf("the refusal prints %q, want the daemon's words as the rule reads them", err)
	}
	_, _, err = fetchControllerSecret(context.Background(), daemon.URL, "an-operator-token", 12)
	if err == nil || !strings.Contains(err.Error(), "older than 12") {
		t.Errorf("the secret refused the unknown field pluginContract beside a URL = %v, want a daemon older than the contract", err)
	}
	if err != nil && strings.Contains(err.Error(), "docs#contracts") {
		t.Errorf("the refusal prints %q, want the daemon's words as the rule reads them", err)
	}
}
