package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAnRDSTokenMintPrintsNoEchoOfTheWebIdentityToken: the broker's credentials come from a web
// identity token, which the SDK trades at STS for a session (AssumeRoleWithWebIdentity, the token in
// its form) when a mint needs one. Whatever answers at STS's address answers that trade with a
// redirect whose Location quotes the token and does not parse, which net/http's error quotes
// whatever the client's redirect policy, since Go parses a Location before CheckRedirect is asked.
// The mint's error, which the database pool's sign-in wraps and the broker prints, holds no run of
// the token and no word of the Location, and names the trade that failed. The provider is the
// SDK's own, as the default credential chain builds it for AWS_WEB_IDENTITY_TOKEN_FILE. The twin
// shows the provider's own error holds the echo. Captures cipherd.rds-mint-echo.
func TestAnRDSTokenMintPrintsNoEchoOfTheWebIdentityToken(t *testing.T) {
	const token = "jt5bq-not-a-secret-RDX-8e3c"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Location", redirectstest.Unparsed(r.PostForm.Get("WebIdentityToken")))
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := func() aws.CredentialsProvider {
		client := sts.NewFromConfig(aws.Config{Region: "us-east-1", BaseEndpoint: aws.String(server.URL)})
		return aws.NewCredentialsCache(stscreds.NewWebIdentityRoleProvider(client, "arn:aws:iam::123456789012:role/broker", stscreds.IdentityTokenFile(tokenFile)))
	}
	t.Run("twin", func(t *testing.T) {
		_, err := provider().Retrieve(context.Background())
		if err == nil || !strings.Contains(err.Error(), token) {
			t.Fatalf("the provider's own error holds no echo of the web identity token, so the row cannot see a leak: %v", err)
		}
	})
	t.Run("mint", func(t *testing.T) {
		mint, err := rdsAuthTokens(aws.Config{Region: "us-east-1", Credentials: provider()})
		if err != nil {
			t.Fatalf("rdsAuthTokens: %v", err)
		}
		_, err = mint(context.Background(), "example-cluster.cluster-abcdefghijkl.us-east-1.rds.amazonaws.com:5432", "agent_secrets_broker")
		if err == nil {
			t.Fatal("the mint answered no error")
		}
		redacttest.RequireNoPiece(t, "cipherd.rds-mint-echo", err.Error(), token)
		if strings.Contains(err.Error(), "moved") {
			t.Errorf("the mint's error names the Location: %q", err)
		}
		if !strings.Contains(err.Error(), "AssumeRoleWithWebIdentity") {
			t.Errorf("the mint's error names no trade: %q", err)
		}
	})
}
