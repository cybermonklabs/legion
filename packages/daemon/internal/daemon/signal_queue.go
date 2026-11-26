package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The kinds of a pull request's timeline event that move it in the merge queue: GitHub's own
// names, as its REST timeline API spells them.
const (
	queueTimelineAdded   = "added_to_merge_queue"
	queueTimelineRemoved = "removed_from_merge_queue"
	queueTimelineMerged  = "merged"
)

// queueTimelineEventURL matches the url GitHub's REST timeline API gives each issue event,
// <API>/repos/<owner>/<name>/issues/events/<id>, and captures GitHub's own owner and repository
// name: the repository the event is of as GitHub names it now, whichever name the read was made
// under, which differs from the name the record keeps after a transfer.
var queueTimelineEventURL = regexp.MustCompile(`^https?://[^/?#]+(?:/[^?#]*)?/repos/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues/events/[0-9]+$`)

// readQueueTimeline reads pull request number of the repository repoName (as the record spells it)
// from GitHub's REST timeline API, every page (GET /repos/<owner>/<name>/issues/<n>/timeline), as
// the implement App, as readPullRequest reads a pull request, and answers its merge-queue events in
// GitHub's order (parseQueueTimeline).
func (w *workflowRuntime) readQueueTimeline(ctx context.Context, repoName string, number int) ([]signals.QueueEvent, error) {
	recorded, err := ghrepo.Parse("the pull request's repository", repoName)
	if err != nil {
		return nil, err
	}
	repository := w.repositories.serving(recorded)
	github, err := implementClient(ctx, w.tokens, w.githubAPI, repository)
	if err != nil {
		return nil, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
	}
	items, err := githubrest.GetPages[json.RawMessage](ctx, github, fmt.Sprintf("/issues/%d/timeline", number))
	if err != nil {
		return nil, fmt.Errorf("read the timeline of %s#%d: %w", repository, number, err)
	}
	return parseQueueTimeline(number, items)
}

// parseQueueTimeline is the merge-queue events of pull request number among items, the events of
// its timeline as GitHub serves them: each added_to_merge_queue, removed_from_merge_queue and
// merged event, in the order of the timeline, which is the order of events of one second. Each is
// kept with GitHub's own id and created_at and the repository its url names. An event of another
// kind is left out whatever it holds; a queue event with no id, no time or no repository is an
// error rather than an event with a time the daemon made up.
func parseQueueTimeline(number int, items []json.RawMessage) ([]signals.QueueEvent, error) {
	events := []signals.QueueEvent{}
	for _, raw := range items {
		var head struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("read an event of the timeline of #%d: %w", number, err)
		}
		if head.Event != queueTimelineAdded && head.Event != queueTimelineRemoved && head.Event != queueTimelineMerged {
			continue
		}
		var item struct {
			ID        int64     `json:"id"`
			URL       string    `json:"url"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("read the %s event of the timeline of #%d: %w", head.Event, number, err)
		}
		named := queueTimelineEventURL.FindStringSubmatch(item.URL)
		if item.ID <= 0 || item.CreatedAt.IsZero() || named == nil {
			return nil, fmt.Errorf("the %s event of the timeline of #%d (id %d, created %s, url %q) names no id, no time or no repository",
				head.Event, number, item.ID, item.CreatedAt, item.URL)
		}
		events = append(events, signals.QueueEvent{ID: item.ID, Repo: named[1] + "/" + named[2], PullRequest: number,
			At: item.CreatedAt.UTC(), Enters: head.Event == queueTimelineAdded})
	}
	return events, nil
}

// queueTimelineGone says whether err is GitHub answering that the pull request's timeline is not
// there to read (404 or 410): the pull request or its repository is gone, or the App no longer
// reaches it, and no later read answers otherwise.
func queueTimelineGone(err error) bool {
	var answer *githubrest.Answer
	return errors.As(err, &answer) && !answer.RateLimited &&
		(answer.Status == http.StatusNotFound || answer.Status == http.StatusGone)
}

// queueTimelineRead runs a queue timeline read (record.QueueTimelineRead): the daemon's read of a
// pull request's timeline after GitHub notified one of its moves in the merge queue, outside the
// fact lock, applied as intake.QueueTimeline under an event id that names the row, so a row run
// again after its finish was lost applies none twice. The events carry GitHub's own ids and times,
// and the fact is kept once however many reads, boots and rows see an event (admit's
// keepSignalFacts).
//
// A read GitHub refuses or does not answer is read again after the runtime's wait, or after
// GitHub's Retry-After when that is longer (readAgainAfter), for as long as it fails: a move
// dropped here leaves an exit unkept, and the pull request would read as the longest wait of the
// queue until the next boot. The one exception is GitHub answering that the timeline is not there
// (queueTimelineGone), which no read repairs and is logged. A read GitHub answers whose timeline
// holds fewer moves in the notified direction than the row needs (MinCount), since the timeline is
// behind the notification, is read again up to mergeStateLadderReads attempts in all, as the
// merge-queue read reads a state GitHub has not computed yet; the last attempt keeps what the
// timeline holds, which is every event GitHub shows and no event the daemon made up.
func (r *outbox) queueTimelineRead(ctx context.Context, row record.OutboxRow, payload record.QueueTimelineRead) error {
	if r.readQueueTimeline == nil || r.mergeStateWait == nil {
		return errors.New("queue timeline read executor has no read of a pull request's timeline from GitHub")
	}
	attempt := row.Attempts + 1
	bounded, cancel := context.WithTimeout(ctx, issueRead)
	events, failed := r.readQueueTimeline(bounded, payload.Repo, payload.Number)
	cancel()
	readAt := r.now().UTC()
	attributes := []any{"row", row.ID, "issue", row.Issue, "repo", payload.Repo, "pullRequest", payload.Number, "move", payload.Move, "attempt", attempt}
	if failed != nil {
		if queueTimelineGone(failed) {
			r.log.Error("outbox queue-timeline read: GitHub has no timeline of the pull request; the read is dropped", append(attributes, "error", failed)...)
			return nil
		}
		delay := r.readAgainAfter(failed)
		r.log.Warn("outbox queue-timeline read: the read of GitHub failed; it reads again", append(attributes, "wait", delay, "error", failed)...)
		return wait.Until(readAt.Add(delay), fmt.Sprintf("the queue timeline read of %s#%d reads GitHub again in %s, after attempt %d failed: %v",
			payload.Repo, payload.Number, delay, attempt, failed))
	}
	shown := 0
	for _, e := range events {
		if e.Enters == (payload.Move == "enqueued") {
			shown++
		}
	}
	if shown < payload.MinCount {
		if attempt < mergeStateLadderReads {
			delay := r.mergeStateWait()
			r.log.Info("outbox queue-timeline read: GitHub's timeline does not show the notified move yet; it reads again",
				append(attributes, "shown", shown, "want", payload.MinCount, "attempts", mergeStateLadderReads, "wait", delay)...)
			return wait.Until(readAt.Add(delay), fmt.Sprintf("the timeline of %s#%d shows %d of the %d %s moves it should, after attempt %d of %d",
				payload.Repo, payload.Number, shown, payload.MinCount, payload.Move, attempt, mergeStateLadderReads))
		}
		r.log.Warn("outbox queue-timeline read: GitHub's timeline still does not show the notified move; the events it shows are kept",
			append(attributes, "shown", shown, "want", payload.MinCount, "attempts", mergeStateLadderReads)...)
	}
	if len(events) == 0 {
		return nil
	}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("queue-timeline:%s:%d", row.Issue, row.ID),
		intake.QueueTimeline{Repo: payload.Repo, Number: payload.Number, Events: events}, r.handlers...); err != nil {
		return fmt.Errorf("apply the queue timeline of %s#%d: %w", payload.Repo, payload.Number, err)
	}
	return nil
}

// mergeQueueQuery is the page of a repository's merge queue on one target branch: its entries in
// the order of the queue, each with the pull request it holds. The snapshot only finds the pull
// requests whose timeline the boot reads; no queue event is made from it.
const mergeQueueQuery = `query($owner: String!, $name: String!, $branch: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    mergeQueue(branch: $branch) {
      entries(first: 100, after: $after) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          pullRequest {
            number
            headRefName
          }
        }
      }
    }
  }
}`

// queuedPull is a pull request of a repository's merge queue, as the live queue lists it.
type queuedPull struct {
	number int
	branch string
}

// queryMergeQueue pages GitHub's GraphQL mergeQueue(branch:) of repo, 100 entries a page, as the
// implement App, as queryChunk reads a batch of pull requests: each page is one query through the
// gate of the implement App's installation for repo's owner (queryGate.run: its rate-limit hold,
// its pace and one query at a time), its token minted in the query's own turn. It answers each
// pull request in the queue. A branch with no merge queue answers GitHub's null, which is a queue
// with no entry; a repository GitHub answers no object for, an error of GitHub's and a page that
// does not come are errors, never an empty queue.
func (w *workflowRuntime) queryMergeQueue(ctx context.Context, repo ghrepo.Repository, branch string) ([]queuedPull, error) {
	served := w.repositories.serving(repo)
	var queued []queuedPull
	var after any
	for {
		var page struct {
			Repository *struct {
				MergeQueue *struct {
					Entries struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							PullRequest *struct {
								Number      int    `json:"number"`
								HeadRefName string `json:"headRefName"`
							} `json:"pullRequest"`
						} `json:"nodes"`
					} `json:"entries"`
				} `json:"mergeQueue"`
			} `json:"repository"`
		}
		var minted error
		err := w.queryGate(implementScope(served)).run(ctx, func() error {
			minting, cancelMint := context.WithTimeout(ctx, issueRead)
			github, err := implementClient(minting, w.tokens, w.githubAPI, served)
			cancelMint()
			if err != nil {
				minted = err
				return err
			}
			query := githubrest.Client{Token: github.Token, API: githubrest.GraphQLEndpoint(w.githubAPI)}
			bounded, cancel := context.WithTimeout(ctx, issueRead)
			defer cancel()
			return query.GraphQL(bounded, mergeQueueQuery,
				map[string]any{"owner": served.Owner(), "name": served.Name(), "branch": branch, "after": after}, &page)
		})
		if minted != nil {
			return nil, fmt.Errorf("mint the implement App token for %s: %w", served.Owner(), minted)
		}
		if err != nil {
			return nil, fmt.Errorf("read the merge queue of %s@%s: %w", served, branch, err)
		}
		if page.Repository == nil {
			return nil, fmt.Errorf("read the merge queue of %s@%s: GitHub answered no repository", served, branch)
		}
		if page.Repository.MergeQueue == nil {
			return queued, nil
		}
		entries := page.Repository.MergeQueue.Entries
		for _, node := range entries.Nodes {
			if node.PullRequest != nil && node.PullRequest.Number > 0 {
				queued = append(queued, queuedPull{number: node.PullRequest.Number, branch: node.PullRequest.HeadRefName})
			}
		}
		if !entries.PageInfo.HasNextPage || entries.PageInfo.EndCursor == "" {
			return queued, nil
		}
		after = entries.PageInfo.EndCursor
	}
}

// queueBranch is a repository and a target branch whose merge queue serves the project's pull
// requests.
type queueBranch struct {
	repo   ghrepo.Repository
	branch string
}

// queueTargets is each repository and target branch whose merge queue the boot reads, and each
// pull request the store holds an open wait of. A queue serves the project's pull requests where
// one of them is open on its branch, or where a live issue of the project works in its repository,
// whose pull requests target that repository's default branch. Each repository is the name GitHub
// holds it under now (repositories.serving).
func (w *workflowRuntime) queueTargets(ctx context.Context) ([]queueBranch, []record.OpenQueuePull, error) {
	found := map[string]queueBranch{}
	add := func(repoName, branch string) {
		recorded, err := ghrepo.Parse("the repository of a pull request of the queue", repoName)
		if err != nil || branch == "" {
			return
		}
		served := w.repositories.serving(recorded)
		found[strings.ToLower(served.String())+"@"+branch] = queueBranch{repo: served, branch: branch}
	}
	var open []record.OpenQueuePull
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		pulls, err := w.records.OpenPullRequests(ctx, tx, w.dispatchProject)
		if err != nil {
			return err
		}
		for _, pr := range pulls {
			add(pr.Repo, pr.Base)
		}
		issues, err := w.records.LiveIssues(ctx, tx, w.dispatchProject)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			add(issue.GitHubRepo, issue.DefaultBranch)
		}
		open, err = w.records.OpenQueuePulls(ctx, tx, w.dispatchProject)
		return err
	}); err != nil {
		return nil, nil, err
	}
	targets := make([]queueBranch, 0, len(found))
	for _, key := range slices.Sorted(maps.Keys(found)) {
		targets = append(targets, found[key])
	}
	return targets, open, nil
}

// retryQueueBootRead reads GitHub's merge queue (readQueueOnce) until a read succeeds or ctx ends
// (retryBootRead), so the queue the boot could not read is not left unread until the next boot:
// the queue signal reads as not recorded, with the reason of the last failed read, until one
// succeeds (intake.QueueBootStatus).
func (w *workflowRuntime) retryQueueBootRead(ctx context.Context, bootID int64, first, most time.Duration) {
	retryBootRead(ctx, bootID, first, most, w.readQueueOnce)
}

// retryBootRead makes read, one boot read of a signal's source that says whether it succeeded and
// logs its own failure, until a read succeeds or ctx ends, waiting first, then twice as long up to
// most, between reads (retryUntilApplied). Each read is named by the boot and its attempt, so no
// read's facts are taken for another's as already applied.
func retryBootRead(ctx context.Context, bootID int64, first, most time.Duration, read func(ctx context.Context, id string) bool) {
	if ctx.Err() != nil {
		return
	}
	retryUntilApplied(ctx, first, most, func(ctx context.Context, attempt int) error {
		if !read(ctx, fmt.Sprintf("%d.%d", bootID, attempt)) {
			return errBootReadFailed
		}
		return nil
	}, nil)
}

// errBootReadFailed is a boot read that did not succeed, which the read has logged itself.
var errBootReadFailed = errors.New("the boot's read did not succeed")

// readQueueOnce is one read of GitHub's merge queue as it stands, which id names (the boot and its
// read, so no read's facts are taken for another's as already applied): the live queue of each
// repository and target branch that serves the project's pull requests (queueTargets), paged, finds
// the pull requests of the project that are queued (intake.ProjectBranchIssue); the timeline of each
// of those, and of each pull request the store holds an open wait of that the queue no longer
// shows, is read (readQueueTimeline) and applied as the same fact a webhook's row applies
// (intake.QueueTimeline). It says whether every read succeeded, and applies intake.QueueBootStatus
// either way: a failed read records its reason, which keeps the queue unrecorded, and is never
// taken for a queue with no entry, and a read that succeeded clears it.
func (w *workflowRuntime) readQueueOnce(ctx context.Context, id string) bool {
	status := func(readError string) {
		fact := intake.QueueBootStatus{ReadError: readError}
		if _, err := intake.ApplyFact(ctx, w.pool, "daemon", fmt.Sprintf("boot-queue-status:%s:%s", w.dispatchProject, id), fact, w.handlers...); err != nil && ctx.Err() == nil {
			w.log.Warn("legion daemon could not record the outcome of its read of the merge queue", "read", id, "readError", readError, "error", err)
		}
	}
	failed := func(why string, err error) bool {
		if ctx.Err() != nil {
			return false
		}
		w.log.Warn("legion daemon could not read the merge queue; the queue signal is not recorded until it can", "read", id, "why", why, "error", err)
		status(fmt.Sprintf("%s: %v", why, err))
		return false
	}
	targets, open, err := w.queueTargets(ctx)
	if err != nil {
		return failed("list the repositories of the project's pull requests", err)
	}
	toRead := map[string]record.OpenQueuePull{}
	add := func(repo string, number int) {
		toRead[fmt.Sprintf("%s#%d", strings.ToLower(repo), number)] = record.OpenQueuePull{Repo: repo, Number: number}
	}
	for _, pull := range open {
		add(pull.Repo, pull.Number)
	}
	for _, target := range targets {
		queued, err := w.queryMergeQueue(ctx, target.repo, target.branch)
		if err != nil {
			return failed(fmt.Sprintf("read the merge queue of %s@%s", target.repo, target.branch), err)
		}
		for _, pull := range queued {
			if _, ours := intake.ProjectBranchIssue(pull.branch, w.dispatchProject); ours {
				add(target.repo.String(), pull.number)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(toRead)) {
		pull := toRead[key]
		bounded, cancel := context.WithTimeout(ctx, issueRead)
		events, err := w.readQueueTimeline(bounded, pull.Repo, pull.Number)
		cancel()
		if err != nil {
			return failed(fmt.Sprintf("read the timeline of %s#%d", pull.Repo, pull.Number), err)
		}
		if len(events) == 0 {
			continue
		}
		fact := intake.QueueTimeline{Repo: pull.Repo, Number: pull.Number, Events: events}
		if _, err := intake.ApplyFact(ctx, w.pool, "daemon", fmt.Sprintf("boot-queue-timeline:%s:%s:%s", w.dispatchProject, id, key), fact, w.handlers...); err != nil {
			return failed(fmt.Sprintf("keep the timeline of %s#%d", pull.Repo, pull.Number), err)
		}
	}
	status("")
	return ctx.Err() == nil
}
