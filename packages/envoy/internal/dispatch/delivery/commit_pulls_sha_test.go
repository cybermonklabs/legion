package delivery

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestACommitsPullRequestsAreNotReadForAValueThatIsNoSHA: the pull requests of a commit are read
// (fetchCommitPullPayloads) for a value that is no full sha but quotes a credential's URL. The
// read is refused before any request, as the App client's reads of a commit refuse such a value,
// and its error names the value only by a phrase that names the field. Captures
// delivery.commit-pulls/not-a-sha.
func TestACommitsPullRequestsAreNotReadForAValueThatIsNoSHA(t *testing.T) {
	var reads atomic.Int64
	handle := func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.EscapedPath(); {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/installation"):
			mustEncode(t, w, map[string]any{
				"id": 1, "app_slug": "delivery-test",
				"permissions": map[string]string{"contents": "read", "pull_requests": "read"},
			})
		case r.Method == http.MethodPost && path == "/app/installations/1/access_tokens":
			w.WriteHeader(http.StatusCreated)
			mustEncode(t, w, map[string]any{"token": "ghs_installation_token_for_the_pulls", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case strings.HasPrefix(path, "/repos/acme/widgets/commits/"):
			reads.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			mustEncode(t, w, map[string]any{"message": "Server Error"})
		default:
			githubtest.Unexpected(t, w, r)
		}
	}
	client := newAppClient(t, githubtest.Spec{Handle: handle})
	token, err := client.RepositoryToken(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("the installation token of acme/widgets: %v", err)
	}
	_, err = fetchCommitPullPayloads(t.Context(), client, token, "acme", "widgets", redacttest.Words)
	if err == nil {
		t.Fatal("the read of a value that is no sha answered no error")
	}
	redacttest.RequireNone(t, "delivery.commit-pulls/not-a-sha", err.Error())
	if n := reads.Load(); n != 0 {
		t.Errorf("the read asked GitHub %d times about a value that is no sha; want none", n)
	}
}
