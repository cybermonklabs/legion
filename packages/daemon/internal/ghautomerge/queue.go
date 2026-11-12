// Package ghautomerge: this file holds the read of a branch's merge queue (QueueEntries), with
// the document it is read by and the entry it answers. It is a read and no part of Submit's
// path: a caller makes it before it decides a jump, so that the account of a jump can name the
// queue the jump asks to go ahead of while that queue still stands.
package ghautomerge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// queuePage is how many entries of a branch's merge queue one QueueEntries read asks GitHub for.
// One page is the whole of the read: the record keeps the queue as it stood before a jump
// (record.MergeJumpRecord), with GitHub's own totalCount beside it, so a reader of a longer queue
// is told how long it was rather than shown every entry. Fifty keeps a jump to one GitHub call,
// where a paginated read would spend another call, and another wait, on a mark that is already
// spent.
const queuePage = 50

// queueRead is the read of a branch's merge queue: GitHub's own order, oldest entry first, with
// totalCount, which is how long the queue is however much of it this one page carries. It is a
// read and nothing else — no mutation of any kind — so a caller may run it before it decides
// anything. A branch GitHub keeps a merge queue for answers that queue's entries as they stand,
// an empty entries object among them; a branch it keeps none for answers a null mergeQueue with
// no error of GraphQL's own at all, which QueueEntries answers ErrNoMergeQueue for.
var queueRead = fmt.Sprintf(`query MergeQueueEntries($owner: String!, $name: String!, $branch: String!) {
  repository(owner: $owner, name: $name) {
    mergeQueue(branch: $branch) {
      entries(first: %d) { totalCount nodes { position jump state enqueuedAt pullRequest { number } } }
    }
  }
}`, queuePage)

// ErrNoMergeQueue is what QueueEntries answers for a branch GitHub says has no merge queue: the
// read came back with a null mergeQueue, which is GitHub's answer for a repository with no merge
// queue at all, for a branch whose base keeps none, and for a name that is no branch of the
// repository (the empty one a pull request with no base ref answers, ghautomerge's
// pullRequest.baseName, among them).
//
// It is no failure of the read — nothing went wrong, and reading again answers the same — and it
// is no reading of a queue either, which is why it is an error and not an empty answer: a caller
// that keeps the account of the queue a jump was asked to go ahead of has no reading of that
// queue to keep (record.MergeJumpRecord.EntriesUnknown), and an empty answer would be recorded
// as a queue read and found empty. Callers tell it from a read GitHub refused with errors.Is.
var ErrNoMergeQueue = errors.New("GitHub answers that the branch has no merge queue")

// QueueEntry is one entry of a branch's merge queue as QueueEntries read it: Number is the pull
// request the entry is for, Position its place in the queue as GitHub answered it, Jump whether
// that entry itself jumped the queue, State GitHub's own state of it (QUEUED, AWAITING_CHECKS,
// MERGEABLE, UNMERGEABLE, LOCKED, in GitHub's own spelling, so a state this daemon does not know
// is still carried), and EnqueuedAt when GitHub says it joined the queue.
//
// It is what the record keeps of a queue a jump asked to go ahead of
// (record.MergeQueueEntryRecord), so it holds what tells a reader whose merge the jump was asked
// of and nothing else of the pull request: the number is enough to find the rest.
type QueueEntry struct {
	Number     int
	Position   int
	Jump       bool
	State      string
	EnqueuedAt time.Time
}

// QueueEntries is the merge queue of branch in repo as GitHub has it now, up to queuePage entries
// of it in GitHub's own order, and GitHub's totalCount for the whole queue, which is above the
// entries answered where the queue is longer than one page. It changes nothing: a caller reads it
// before it decides a jump, and reading it again decides nothing differently.
//
// A queue GitHub answered is answered here as it stands, the queue of no entries among them: an
// empty entries object is a queue read and found empty, never a queue nobody read. A branch
// GitHub answers no merge queue for answers ErrNoMergeQueue instead, wrapped with the branch and
// the repository, which is no failure of the read and no reading of a queue either: the caller
// that was about to jump it records no reading rather than an empty queue. Every other failure,
// a repository the token cannot see among them, is returned for the caller to retry.
func QueueEntries(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, branch string) ([]QueueEntry, int, error) {
	var answer struct {
		Repository struct {
			MergeQueue *struct {
				Entries struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						Position    int       `json:"position"`
						Jump        bool      `json:"jump"`
						State       string    `json:"state"`
						EnqueuedAt  time.Time `json:"enqueuedAt"`
						PullRequest *struct {
							Number int `json:"number"`
						} `json:"pullRequest"`
					} `json:"nodes"`
				} `json:"entries"`
			} `json:"mergeQueue"`
		} `json:"repository"`
	}
	variables := map[string]any{"owner": repo.Owner(), "name": repo.Name(), "branch": branch}
	if err := github.GraphQL(ctx, queueRead, variables, &answer); err != nil {
		return nil, 0, fmt.Errorf("read the merge queue of %q of %s: %w", branch, repo, err)
	}
	queue := answer.Repository.MergeQueue
	if queue == nil {
		return nil, 0, fmt.Errorf("read the merge queue of %q of %s: %w", branch, repo, ErrNoMergeQueue)
	}
	entries := make([]QueueEntry, 0, len(queue.Entries.Nodes))
	for _, node := range queue.Entries.Nodes {
		entry := QueueEntry{Position: node.Position, Jump: node.Jump, State: node.State, EnqueuedAt: node.EnqueuedAt}
		if node.PullRequest != nil {
			entry.Number = node.PullRequest.Number
		}
		entries = append(entries, entry)
	}
	return entries, queue.Entries.TotalCount, nil
}
