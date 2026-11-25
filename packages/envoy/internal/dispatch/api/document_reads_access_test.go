package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// fakeGitHubAppConfig is a GitHub App credential with a key of its own, which a test server whose
// GitHub is a fake (githubtest.Server) signs its App requests with.
func fakeGitHubAppConfig(t *testing.T) *auth.AppConfig {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App's key: %v", err)
	}
	return &auth.AppConfig{ClientID: "Iv1.test", PEM: string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))}
}
