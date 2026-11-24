package delivery

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// TestACommitsPullRequestsReadRefusesAShortSHAAsTheAppClientDoes: a commit's pull requests are
// read only for a full sha. A short one, which GitHub would answer at the same path, is refused
// before any request with githubapp.ErrNotASHA, the refusal a caller tells apart with errors.Is
// whichever read of a commit made it.
func TestACommitsPullRequestsReadRefusesAShortSHAAsTheAppClientDoes(t *testing.T) {
	_, err := fetchCommitPullPayloads(t.Context(), nil, "", "acme", "widgets", "abc1234")
	if !errors.Is(err, githubapp.ErrNotASHA) {
		t.Fatalf("the read of a short sha answered %v; want githubapp.ErrNotASHA", err)
	}
}
