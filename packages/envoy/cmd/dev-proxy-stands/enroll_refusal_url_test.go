package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEnrolmentRefusalPrintsNoCredentialItsURLCarries: the broker refuses enroll-pod's service
// login, and the refusal names the call by its method and its URL. --url can carry a credential in
// its user information (user-information) or its query (query), and the call's URL holds it, so
// the refusal names that URL as redact.NotPrinted, never as written; a clean --url prints the URL
// as the call asked it (clean). The twin shows each URL the call asks holds the credentials.
// Captures dev-proxy-stands.enroll-refusal-url/<case>.
func TestAnEnrolmentRefusalPrintsNoCredentialItsURLCarries(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "refused")
	}))
	t.Cleanup(broker.Close)
	param := "access" + "_token"
	withUser := strings.Replace(broker.URL, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1)
	withQuery := broker.URL + "/broker?" + param + "=" + redacttest.Token
	dir := t.TempDir()
	tokenFile := writeFile(t, dir, "registrar-token", "registrar-token\n")
	podTokenFile := writeFile(t, dir, "pod-token", "pod-token\n")
	enroll := func(t *testing.T, at string) string {
		t.Helper()
		var stdout, stderr strings.Builder
		if code := run(context.Background(), []string{"enroll-pod", "--url", at, "--token-file", tokenFile, "--pod-token-file", podTokenFile,
			"--pod-uid", "pod-uid-1", "--thumbprint", "thumbprint-1"}, &stdout, &stderr); code != 1 {
			t.Fatalf("enroll-pod exited %d, want 1, the service login refused; stderr %q", code, stderr.String())
		}
		return stdout.String() + stderr.String()
	}
	t.Run("twin", func(t *testing.T) {
		if held := redacttest.Held(withUser + withQuery); len(held) != len(redacttest.Secrets) {
			t.Errorf("the URLs the call asks hold %q; want every credential, so the row can see them", held)
		}
	})
	for _, c := range []struct{ name, at string }{{"user-information", withUser}, {"query", withQuery}} {
		t.Run(c.name, func(t *testing.T) {
			said := enroll(t, c.at)
			redacttest.RequireNoPiece(t, "dev-proxy-stands.enroll-refusal-url/"+c.name, said)
			if want := "the service login: POST " + redact.NotPrinted + " answered 403: refused\n"; !strings.HasSuffix(said, want) {
				t.Errorf("enroll-pod printed %q; want it to end %q", said, want)
			}
		})
	}
	t.Run("clean", func(t *testing.T) {
		if said, want := enroll(t, broker.URL), "the service login: POST "+broker.URL+"/v1/launcher-credentials/token answered 403: refused\n"; !strings.HasSuffix(said, want) {
			t.Errorf("enroll-pod printed %q; want it to end %q", said, want)
		}
	})
}
