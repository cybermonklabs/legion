package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAGitHubCheckRunsRefusalReachesTheDetailAsTheRuleReadsIt: GitHub, here a stub server,
// refuses the read of a head's check runs with a body whose `message` quotes a URL carrying a
// credential. The CI results read it through the GitHub App client (CommitCheckRuns), and the
// detail a check shows (ciUnreadDetail) names the status and GitHub's message as the rule for
// remote text reads it, and no credential. The test calls only what the tree before this change
// has too, so the check's control (16) runs it on that tree, where it fails on the captured
// detail. Captures api.ci-detail.
func TestAGitHubCheckRunsRefusalReachesTheDetailAsTheRuleReadsIt(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	appKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": redacttest.Words, "documentation_url": "https://docs.github.com/rest"})
	}))
	t.Cleanup(github.Close)
	client, err := githubapp.New(&auth.AppConfig{ID: 1, PEM: string(appKey)}, github.URL)
	if err != nil || client == nil {
		t.Fatalf("githubapp.New = %v, %v", client, err)
	}
	_, err = client.CommitCheckRuns(context.Background(), "installation-token", "acme", "widgets", "abc123", 1, "")
	if err == nil {
		t.Fatal("CommitCheckRuns read GitHub's refusal as a page of check runs")
	}
	detail := ciUnreadDetail(delivery.NewestHead{Repo: "acme/widgets", HeadSHA: "abc123"}, fmt.Errorf("read check runs: %w", err))
	redacttest.RequireNone(t, "api.ci-detail", detail)
	want := "GitHub could not be read for the check runs of acme/widgets at abc123: read check runs: GET " + github.URL +
		"/repos/acme/widgets/commits/abc123/check-runs?filter=all&per_page=100&page=1: status 422: " + redact.Field(redacttest.Words) + "."
	if detail != want {
		t.Errorf("the detail is %q, want %q", detail, want)
	}
}
