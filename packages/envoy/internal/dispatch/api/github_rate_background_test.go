package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// The background GitHub client (Deps.BackgroundGitHub), which the architecture importer and the
// delivery intakes and reconcile call through, counts its calls on the process's one Counter, as
// the API's client does: GitHub budgets the installation, and the rate reading reports what the
// process spent of it whichever client spent it (githubapp.WithRateCounter).
func TestTheBackgroundGitHubClientCountsItsCallsOnTheProcesssRateCounter(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/77/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_background","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/legion/arch", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Resource", githubrate.Core)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		fmt.Fprint(w, `{}`)
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	counter := githubrate.NewCounter()
	_, _, deps := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))},
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP}),
		githubRate:    counter,
	})
	if deps.BackgroundGitHub == nil || deps.BackgroundGitHub == deps.GitHub {
		t.Fatal("the server built no background GitHub client apart from the API's")
	}
	ctx := githubrate.WithCaller(context.Background(), "importer")
	token, err := deps.BackgroundGitHub.Token(ctx, 77)
	if err != nil {
		t.Fatalf("mint installation 77's token on the background client: %v", err)
	}
	if _, status, _, err := deps.BackgroundGitHub.Read(ctx, token, "/repos/legion/arch"); err != nil || status != http.StatusOK {
		t.Fatalf("read on the background client: status %d, %v", status, err)
	}
	if core, _ := counter.Own("77"); fmt.Sprint(core) != "map[importer:1]" {
		t.Errorf("the process's Counter holds core %v for installation 77 after one background read, want map[importer:1]", core)
	}
}
