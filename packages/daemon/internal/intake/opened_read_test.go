package intake

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A read that finds a pull request open on an issue's branch carries the base the list answer
// names, as the webhook's opened would: the reads of the merge state follow the head on that base
// (daemon's followOnBase) before any of them has answered, and a record with no base would leave
// a head whose read fails unread.
func TestAnOpenedReadCarriesTheBaseTheListAnswerNames(t *testing.T) {
	t.Parallel()
	var pull githubrest.PullRequest
	pull.Number, pull.Head.SHA, pull.Base.Ref = 42, "head", "main"
	fact, _ := OpenedRead("LEGION", "sjawhar/legion", "legion/LEGION-208", pull, 1, nil)
	if fact.Base != "main" {
		t.Fatalf("the opened read carries base %q, want main", fact.Base)
	}
	reopened, _ := OpenedRead("LEGION", "sjawhar/legion", "legion/LEGION-208", pull, 1, &record.PullRequest{Number: 42, State: record.PullRequestClosed})
	if reopened.Base != "main" || !reopened.Reopened {
		t.Fatalf("the read of a pull request recorded closed carries base %q, reopened %t; want main, a reopen", reopened.Base, reopened.Reopened)
	}
}
