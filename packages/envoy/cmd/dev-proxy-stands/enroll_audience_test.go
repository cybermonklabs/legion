package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestAnEnrolmentSignsTheBrokersOwnURL: --url carries a path and a query (query), or user
// information (user). The broker verifies a service login's request object for its own public URL
// exactly (record.VerifyRequestObject) and a launcher proof's htu against that URL followed by the
// path the proof was sent to, neither of which holds a query or user information. enroll-pod signs
// the request object for that URL (proof.Audience) and the proof over it (proof.TargetURI), so
// the stand-in broker, which checks both as the broker does, takes the login and the enrolment;
// where enroll-pod signed the --url's whole text and the joined URL with its query, it refused
// the login.
func TestAnEnrolmentSignsTheBrokersOwnURL(t *testing.T) {
	for _, c := range []struct{ name, user, suffix string }{
		{"query", "", "/prefix?key=k3y-7Qz4k"},
		{"user", "registrar:pw-5Rt8@", "/prefix"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var refused []string
			var public string
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/prefix/v1/launcher-credentials/token":
					var login struct {
						Request string `json:"request"`
					}
					if err := json.NewDecoder(r.Body).Decode(&login); err != nil {
						refused = append(refused, "the login's body: "+err.Error())
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if _, err := record.VerifyRequestObject(login.Request, public, time.Minute, time.Now()); err != nil {
						refused = append(refused, fmt.Sprintf("the request object for %s: %v", public, err))
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					fmt.Fprint(w, `{"credential_id":"credential-1"}`)
				case "/prefix/v1/enrollments":
					if htu, want := proofURL(t, r.Header.Get("Proof")), public+"/v1/enrollments"; htu != want {
						refused = append(refused, fmt.Sprintf("the launcher proof signs %q, want %q", htu, want))
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"enrollment_id":"enrollment-1","lease_expires_at":"2026-01-01T00:00:00Z"}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(broker.Close)
			public = broker.URL + "/prefix"
			given := strings.Replace(broker.URL, "://", "://"+c.user, 1) + c.suffix
			dir := t.TempDir()
			tokenFile := writeFile(t, dir, "registrar-token", "registrar-token\n")
			podTokenFile := writeFile(t, dir, "pod-token", "pod-token\n")
			var stdout, stderr strings.Builder
			code := run(context.Background(), []string{"enroll-pod", "--url", given, "--token-file", tokenFile, "--pod-token-file", podTokenFile,
				"--pod-uid", "pod-uid-1", "--thumbprint", "thumbprint-1"}, &stdout, &stderr)
			mu.Lock()
			defer mu.Unlock()
			if code != 0 || len(refused) > 0 || !strings.Contains(stdout.String(), `"enrollment_id": "enrollment-1"`) {
				t.Errorf("--url %q: exit %d, the broker refused %q, stdout %q, stderr %q; want the login and the enrolment taken", given, code, refused, stdout.String(), stderr.String())
			}
		})
	}
}
