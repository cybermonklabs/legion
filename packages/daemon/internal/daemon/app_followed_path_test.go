package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// TestTheStartsRateLimitRefusalNamesNoPathARedirectWrote: whatever answers in GitHub's place
// answers the start's App logins read (GET /app, which carries the App's JWT) with a 302 on
// GitHub's own origin to /app/<that JWT> (followed), which answers GitHub's rate limit (429), or
// answers GET /app with the 429 itself (control). Once the start's waits reach their end, its
// refusal names GitHub's answer by the call the daemon sent, GET /app, and holds no run of the
// JWT; it printed the Location the appauth Manager's client followed, the JWT whole. The twin
// shows a following client's response names that Location in its request, so the row can see a
// leak. Captures daemon.followed-path/<case>.
func TestTheStartsRateLimitRefusalNamesNoPathARedirectWrote(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for _, c := range []struct {
		name     string
		redirect bool
	}{
		{"twin", true},
		{"control", false},
		{"followed", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var jwt string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/app" {
					sent := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					mu.Lock()
					jwt = sent
					mu.Unlock()
					if c.redirect {
						w.Header().Set("Location", "/app/"+sent)
						w.WriteHeader(http.StatusFound)
						return
					}
				}
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusTooManyRequests)
			}))
			defer server.Close()
			apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
				appauth.Options{BaseURL: server.URL, HTTPClient: server.Client()})
			defer apps.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, loginErr := apps.AppLogin(ctx, appauth.Implement)
			if loginErr == nil {
				t.Fatal("the App logins read answered no error")
			}
			mu.Lock()
			signed := jwt
			mu.Unlock()
			if !strings.HasPrefix(signed, "eyJ") || strings.Count(signed, ".") != 2 {
				t.Fatalf("the read carried no JWT the stand-in could echo: %q", signed)
			}

			if c.name == "twin" {
				// The same read as a following client makes it, with the JWT the Manager signed.
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/app", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+signed)
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusTooManyRequests || !strings.Contains(response.Request.URL.Path, signed) {
					t.Fatalf("the followed answer = %d to %s; want the 429 to a Location that holds the JWT, so the row can see a leak",
						response.StatusCode, response.Request.URL.Path)
				}
				return
			}

			clock := &setClock{now: time.Unix(1_800_000_000, 0)}
			calls := productionAppCalls()
			calls.rateLimits, calls.clock = rateLimitPolicy{Max: time.Hour, Bound: time.Second, NoTime: time.Minute}, clock
			waits := newStartWaits(calls)
			const call = "GitHub App logins read"
			if _, limited := limitOutcome(loginErr, waits, call); !limited {
				t.Fatalf("the read's error is not GitHub's rate limit to the start (bootLimit): %v", loginErr)
			}
			clock.now = clock.now.Add(time.Minute)
			outcome, limited := limitOutcome(loginErr, waits, call)
			if !limited || outcome.Refusal == nil {
				t.Fatalf("the start's waits did not end in a refusal: limited=%v outcome=%+v", limited, outcome)
			}
			refusal := outcome.Refusal.Error()
			oauth2calltest.RequireNoPiece(t, "daemon.followed-path/"+c.name, refusal, signed)
			if want := "GitHub answered GET /app with 429"; !strings.Contains(refusal, want) {
				t.Errorf("the start's refusal is %q, want it to name %q, the read the daemon sent", refusal, want)
			}
		})
	}
}
