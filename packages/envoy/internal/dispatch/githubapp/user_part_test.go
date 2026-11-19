package githubapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
)

// The API base (DISPATCH_GITHUB_API_BASE) may carry a user and a password. A request that cannot
// reach the base fails naming no part of it, with the failure's class, since net/http's transport
// error quotes the request's URL with the user kept and a token written as the whole user part
// whole; and an answer it refuses, a read's, an installation token mint's (Token) or a blob's
// (DirFiles), is named by the request's path, not by the URL the base begins, and so is the
// failure a read notes for /healthz (githubreads). The control is the same base with no user
// part, whose errors name the same path, and whose unreached request's error is net/http's own,
// the request's URL with it, since redact.URL prints that base whole. The user part's values are
// made for this test.
func TestAnAPIBaseWithAUserPartIsNamedWithNoneOfIt(t *testing.T) {
	const (
		user     = "made-up-user-3a8c"
		password = "not-a-secret-d04e72"
		token    = "not-a-secret-tok1f5"
	)
	_, key := githubapptest.Key(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"made-up failure"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	for _, userPart := range []string{user + ":" + password + "@", token + "@", ""} {
		texts := map[string]string{}
		client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.made-up", PEM: key}, "http://"+userPart+"127.0.0.1:1")
		if err != nil {
			t.Fatalf("user part %q: New: %v", userPart, err)
		}
		_, _, _, err = client.Read(context.Background(), "made-up-installation-token", "/repos/acme/widgets")
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("user part %q: Read err = %v, want the refused connection's class", userPart, err)
		}
		if userPart == "" && !strings.Contains(err.Error(), `Get "http://127.0.0.1:1/repos/acme/widgets"`) {
			t.Errorf("the control: Read err = %v, want net/http's own error naming the request's URL", err)
		}
		texts["a request that cannot reach the base"] = err.Error()
		answering, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.made-up", PEM: key}, "http://"+userPart+strings.TrimPrefix(failing.URL, "http://"))
		if err != nil {
			t.Fatalf("user part %q: New: %v", userPart, err)
		}
		_, err = answering.HasCommit(context.Background(), "made-up-installation-token", "acme", "widgets", "0123456789abcdef0123456789abcdef01234567")
		if err == nil || !strings.Contains(err.Error(), "/repos/acme/widgets/commits/") {
			t.Fatalf("user part %q: HasCommit err = %v, want the refusal of the answer naming the request's path", userPart, err)
		}
		texts["an answer the client refuses"] = err.Error()
		_, err = answering.Token(context.Background(), 7)
		if err == nil || !strings.Contains(err.Error(), "/app/installations/7/access_tokens") || !strings.Contains(err.Error(), "status 500") {
			t.Fatalf("user part %q: Token err = %v, want the refusal of the mint naming its path and status 500", userPart, err)
		}
		texts["a token mint the API refuses"] = err.Error()
		_, err = answering.DirFiles(context.Background(), "made-up-installation-token", "acme", "widgets", githubapp.Dir{Files: []githubapp.DirFile{{Name: "a.md", Path: "notes/a.md", SHA: "0123456789abcdef0123456789abcdef01234567", Size: 1}}})
		if err == nil || !strings.Contains(err.Error(), "/repos/acme/widgets/git/blobs/") || !strings.Contains(err.Error(), "status 500") {
			t.Fatalf("user part %q: DirFiles err = %v, want the refusal of the blob read naming its path and status 500", userPart, err)
		}
		texts["a blob read the API refuses"] = err.Error()
		noted, attempt := githubreads.Begin(context.Background())
		_, _ = answering.HasCommit(noted, "made-up-installation-token", "acme", "widgets", "0123456789abcdef0123456789abcdef01234567")
		if _, failed, cause := attempt.Outcome(); !failed || !strings.Contains(cause, "/repos/acme/widgets/commits/") {
			t.Fatalf("user part %q: the read's noted failure is %v, %q, want the 500 naming the request's path", userPart, failed, cause)
		} else {
			texts["the failure a GitHub read notes, which /healthz names"] = cause
		}
		for name, text := range texts {
			for _, part := range []string{user, password, token} {
				if strings.Contains(text, part) {
					t.Errorf("user part %q, %s: the error holds a part of the user part: %q", userPart, name, text)
				}
			}
		}
	}
}
