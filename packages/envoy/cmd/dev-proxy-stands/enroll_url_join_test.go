package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEnrolmentGoesToTheRoutesJoinedOntoTheBrokerURL: --url can carry a path and a query.
// enroll-pod's two calls go to the URL's path with their routes joined onto it and the URL's query
// kept: http://host/prefix?key=... asks /prefix/v1/launcher-credentials/token?key=... and
// /prefix/v1/enrollments?key=... (query), where the route appended to the URL's text sat inside
// the query and the broker was asked /prefix with the query's value changed. A URL with a path and
// no query asks where it did (no-query). In both, the launcher proof the enrolment carries signs
// TargetURI of the URL the enrolment asked (its htu): its scheme, host and path, without the query,
// as the broker compares it. A stand-in records the path and the query each call reaches it with,
// answering the calls in order.
func TestAnEnrolmentGoesToTheRoutesJoinedOntoTheBrokerURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/prefix?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"no-query", "/prefix", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			var targets, proofs []string
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, *r.URL)
				targets = append(targets, "http://"+r.Host+r.URL.EscapedPath())
				proofs = append(proofs, r.Header.Get("Proof"))
				w.Header().Set("Content-Type", "application/json")
				if len(asked) == 1 {
					fmt.Fprint(w, `{"credential_id":"credential-1"}`)
					return
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"enrollment_id":"enrollment-1","lease_expires_at":"2026-01-01T00:00:00Z"}`)
			}))
			t.Cleanup(broker.Close)
			dir := t.TempDir()
			tokenFile := writeFile(t, dir, "registrar-token", "registrar-token\n")
			podTokenFile := writeFile(t, dir, "pod-token", "pod-token\n")
			var stdout, stderr strings.Builder
			code := run(context.Background(), []string{"enroll-pod", "--url", broker.URL + c.suffix, "--token-file", tokenFile, "--pod-token-file", podTokenFile,
				"--pod-uid", "pod-uid-1", "--thumbprint", "thumbprint-1"}, &stdout, &stderr)
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/prefix/v1/launcher-credentials/token", "/prefix/v1/enrollments"}
			if len(asked) != len(want) {
				t.Fatalf("--url ending %q: the broker was asked %d calls (%v), want the service login and the enrolment; exit %d, stderr %q", c.suffix, len(asked), asked, code, stderr.String())
			}
			for i, got := range asked {
				if got.Path != want[i] || got.RawQuery != c.query {
					t.Errorf("--url ending %q: call %d reached path %q with query %q; want path %q with query %q", c.suffix, i, got.Path, got.RawQuery, want[i], c.query)
				}
			}
			if htu := proofURL(t, proofs[1]); htu != targets[1] {
				t.Errorf("--url ending %q: the launcher proof signs %q; want %q, the enrolment's URL without its query", c.suffix, htu, targets[1])
			}
			if code != 0 || !strings.Contains(stdout.String(), `"enrollment_id": "enrollment-1"`) {
				t.Errorf("--url ending %q: exit %d, stdout %q, stderr %q; want 0 and the enrolment's report", c.suffix, code, stdout.String(), stderr.String())
			}
		})
	}
}

// proofURL is the htu a launcher proof signs, read from its payload without checking its signature.
func proofURL(t *testing.T, compact string) string {
	t.Helper()
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatalf("the Proof header %q is not a compact JWS", compact)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("the proof's payload: %v", err)
	}
	var claims struct {
		URL string `json:"htu"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("the proof's payload: %v", err)
	}
	return claims.URL
}

// TestAnEnrolmentURLThatDoesNotParsePrintsNoneOfIt: --url with redacttest's credentials in its user
// information and its query. One url.Parse refuses (an invalid escape in its path) fails enroll-pod
// by --url and the class alone, before any call (unparsed); http.NewRequest's error for the URL
// with the service login's route appended quotes it whole (twin). One url.Parse takes, whose
// joined URL, as url.URL.String writes it, url.Parse refuses again (an IPv6 zone holding a byte
// that is not ASCII, which String escapes as %FD), fails the service login by its method, the
// URL as redact.URL prints it and the class (reparsed); its twin shows
// http.NewRequest's error for the joined URL holds the credentials. Captures
// dev-proxy-stands.enroll-url/<case>.
func TestAnEnrolmentURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/broker%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/broker?" + param + "=" + redacttest.Token
	dir := t.TempDir()
	tokenFile := writeFile(t, dir, "registrar-token", "registrar-token\n")
	podTokenFile := writeFile(t, dir, "pod-token", "pod-token\n")
	enroll := func(t *testing.T, broker string) string {
		t.Helper()
		var stdout, stderr strings.Builder
		if code := run(context.Background(), []string{"enroll-pod", "--url", broker, "--token-file", tokenFile, "--pod-token-file", podTokenFile,
			"--pod-uid", "pod-uid-1", "--thumbprint", "thumbprint-1"}, &stdout, &stderr); code != 1 {
			t.Fatalf("enroll-pod exited %d, want 1; stderr %q", code, stderr.String())
		}
		return stdout.String() + stderr.String()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodPost, unparsed+"/v1/launcher-credentials/token", nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := enroll(t, unparsed)
		redacttest.RequireNoPiece(t, "dev-proxy-stands.enroll-url/unparsed", said)
		if want := "dev-airlock-stands enroll-pod: --url \"" + redact.NotPrinted + "\": " + redact.ClassNotAURL + "\n"; said != want {
			t.Errorf("enroll-pod printed %q; want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused --url (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodPost, parsed.JoinPath("v1/launcher-credentials/token").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := enroll(t, reparsed)
		redacttest.RequireNoPiece(t, "dev-proxy-stands.enroll-url/reparsed", said)
		if want := "dev-airlock-stands enroll-pod: the service login: POST " + redact.NotPrinted + ": " + redact.ClassNotAURL + "\n"; said != want {
			t.Errorf("enroll-pod printed %q; want %q", said, want)
		}
	})
}
