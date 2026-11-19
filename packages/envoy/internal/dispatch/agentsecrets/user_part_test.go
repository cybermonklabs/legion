package agentsecrets_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
)

// The broker URL (DISPATCH_AGENT_SECRETS_URL) may carry a user and a password, and net/http's
// transport error quotes the request's URL with the user kept and a token written as the whole
// user part whole. A relay whose broker cannot be reached fails naming no part of it, with the
// failure's class. The control is the same URL with no user part, whose error is net/http's own,
// the request's URL and its query with it, since redact.URL prints that URL whole. The user
// part's values are made for this test.
func TestABrokerURLWithAUserPartIsNamedWithNoneOfIt(t *testing.T) {
	const (
		user     = "made-up-user-1d7f"
		password = "not-a-secret-5a90c3"
		token    = "not-a-secret-tok2e8"
	)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("made-up-account-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, userPart := range []string{user + ":" + password + "@", token + "@", ""} {
		client := agentsecrets.New("http://"+userPart+"127.0.0.1:1", file)
		_, err := client.Pending(context.Background(), "made-up-approver@example.com")
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("user part %q: err = %v, want the refused connection's class", userPart, err)
		}
		if userPart == "" && !strings.Contains(err.Error(), `Get "http://127.0.0.1:1/v1/pending?approver=`) {
			t.Errorf("the control: err = %v, want net/http's own error naming the request's URL", err)
		}
		for _, part := range []string{user, password, token} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("user part %q: the error holds a part of the user part: %q", userPart, err)
			}
		}
	}
}
