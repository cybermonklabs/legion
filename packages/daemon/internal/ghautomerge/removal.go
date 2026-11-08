package ghautomerge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// checkPage is one page of a merge queue commit's checks as removals and moreChecks read it: whether
// GitHub lists more after it, and each check run, with its workflow run, its Actions job id and
// commit status. A check run's databaseId is the id of the Actions job that ran it, which is what
// its log is read by (githubrest.Client.JobLog): measured on run 42397923554 of
// cybermonklabs/legion, whose failing dispatch check run answers databaseId 128479266994, the id
// that job has in the jobs API. detailsUrl is no substitute, since its shape is not one shape: the
// same run answers https://github.com/cybermonklabs/legion/runs/128483804195 for one check run and
// https://github.com/cybermonklabs/legion/actions/runs/42397923554/job/128483822023 for another.
const checkPage = `pageInfo { hasNextPage endCursor }
                  nodes {
                    __typename
                    ... on CheckRun { name status conclusion detailsUrl databaseId checkSuite { workflowRun { url } } }
                    ... on StatusContext { context state targetUrl }
                  }`

// removals is the read of what took a pull request's arm or merge-queue entry away: the GraphQL
// viewer, the account the token acts as, and the pull request's last arms and removals, oldest
// first, each queue removal with the commit the queue built for it and the first page of that
// commit's checks (moreChecks reads the rest).
const removals = `query PullRequestRemovals($owner: String!, $name: String!, $number: Int!) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      timelineItems(last: 20, itemTypes: [ADDED_TO_MERGE_QUEUE_EVENT, AUTO_MERGE_ENABLED_EVENT, REMOVED_FROM_MERGE_QUEUE_EVENT, AUTO_MERGE_DISABLED_EVENT]) {
        nodes {
          __typename
          ... on AddedToMergeQueueEvent { createdAt }
          ... on AutoMergeEnabledEvent { createdAt }
          ... on AutoMergeDisabledEvent { id createdAt reason reasonCode actor { login } }
          ... on RemovedFromMergeQueueEvent {
            id createdAt reason actor { login }
            beforeCommit {
              oid
              statusCheckRollup {
                contexts(first: 100) {
                  ` + checkPage + `
                }
              }
            }
          }
        }
      }
    }
  }
}`

// moreChecks is the read of a merge queue commit's checks after the page that ended at $after: a
// merge group can carry more than the 100 a page holds.
const moreChecks = `query MergeGroupContexts($owner: String!, $name: String!, $oid: GitObjectID!, $after: String!) {
  repository(owner: $owner, name: $name) {
    object(oid: $oid) {
      ... on Commit {
        oid
        statusCheckRollup {
          contexts(first: 100, after: $after) {
            ` + checkPage + `
          }
        }
      }
    }
  }
}`

// removal is what took pull request number of repo's arm or merge-queue entry away at READY's
// head, after Submit saw one there, as GitHub's timeline records it: the removals since the last
// arm or entry. The merge queue's removal for the merge itself is a wait for GitHub to show the
// merge (Merging), whatever else the timeline shows, and reads no check. A queue removal whose
// reason leaves it to the checks on the commit the queue built (timelineItem.judgedByChecks) is
// judged on every page of them (mergeGroupCommit.checks): one for failed_checks, or for an
// unexplained reason with a run that failed there (failingRuns), is MergeFailed. Two removals
// restart the submission, and removal answers nil when they are all there is, for Submit to submit
// again (timelineItem.restarts): the daemon's own disarm landing late, and auto-merge a close turned
// off, which the pull request's reopen at READY's head undid. Any other is Removed, the latest of
// them: GitHub's own (Removed.ByGitHub) when it names the account the token acts as, whose own
// disarm leaves no other reason; a queue removal for merge_conflict is that same removal carried as
// a ConflictRemoval instead, with mergeState, the merge state the read that found nothing armed
// answered for the pull request, since what the daemon does with it is decided by that state and
// not by the removal alone. A timeline that shows no removal since the last arm is behind the
// read of the pull request (Disappeared).
//
// github is the one client removal calls, GitHub's GraphQL route: no log of a failing job is
// read here. Each failing run a removal names carries the Actions job it ran as instead
// (FailingRun.JobID), for the caller to read once the removal is decided and only where it tells
// of it (ReadFailingTests, the daemon's readyMerge), since a read of a 54 MB log cannot finish
// inside a merge row's own budget and a judgment of a removal must wait on none.
func removal(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, head, mergeState string) error {
	var answer struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Repository struct {
			PullRequest *struct {
				TimelineItems struct {
					Nodes []timelineItem `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := github.GraphQL(ctx, removals, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "number": number}, &answer); err != nil {
		return fmt.Errorf("read what took pull request #%d of %s out of the merge queue or off auto-merge: %w", number, repo, err)
	}
	if answer.Repository.PullRequest == nil {
		return fmt.Errorf("GitHub answered no timeline for pull request #%d of %s", number, repo)
	}
	items := answer.Repository.PullRequest.TimelineItems.Nodes
	since := 0
	for i, item := range items {
		if item.Type == "AddedToMergeQueueEvent" || item.Type == "AutoMergeEnabledEvent" {
			since = i + 1
		}
	}
	removed := items[since:]
	for _, item := range removed {
		if item.Type == "RemovedFromMergeQueueEvent" && item.Reason == "merged" {
			return &Merging{Number: number, Repo: repo, Head: head}
		}
	}
	self := strings.TrimSuffix(answer.Viewer.Login, "[bot]")
	var failed, other *timelineItem
	var runs []FailingRun
	for i := range removed {
		item := &removed[i]
		if item.judgedByChecks() {
			checks, err := item.BeforeCommit.checks(ctx, github, repo)
			if err != nil {
				return err
			}
			if failing := failingRuns(checks); item.Reason == "failed_checks" || len(failing) > 0 {
				failed, runs = item, failing
				continue
			}
		}
		if !item.restarts(self) {
			other = item
		}
	}
	switch {
	case failed != nil:
		commit := ""
		if failed.BeforeCommit != nil {
			commit = failed.BeforeCommit.Oid
		}
		return &MergeFailed{Number: number, Head: head, Reason: failed.Reason, Commit: commit,
			Runs: runs, Event: failed.ID, At: failed.CreatedAt}
	case other != nil:
		// The removal GitHub explains by itself names the runs that failed on the page of the
		// commit's checks the timeline's own answer carried, the first hundred of them: a page
		// further would cost a request for a removal nothing here judges, where this page is in
		// hand. A judged removal that reaches here failed no run on any page, so its page names
		// none either.
		removed := &Removed{Number: number, Head: head, Actor: other.actor(), Reason: other.removalReason(), Detail: other.detail(),
			Queue: other.Type == "RemovedFromMergeQueueEvent", ByGitHub: self != "" && other.actor() == self, At: other.CreatedAt,
			Runs: failingRuns(other.BeforeCommit.page())}
		if removed.Queue && other.Reason == "merge_conflict" {
			return &ConflictRemoval{Removal: *removed, MergeState: mergeState}
		}
		return removed
	case since == len(items):
		return &Disappeared{Number: number, Repo: repo, Head: head}
	default:
		return nil
	}
}

// judgedByChecks says whether a removal is a queue removal whose reason leaves it to the checks on
// the commit the queue built: failed_checks, whose failing runs the issue goes back to implementing
// on, and a reason GitHub gives no other meaning, a failed merge only with a run that failed there.
// The merge itself (merged), a person's dequeue (manual) and a conflict (merge_conflict) mean what
// they say, so no page of their checks is read past the one the timeline's answer carried: a
// person's dequeue cancels the merge group's runs in flight, which is no failed merge. What that
// one page names as failed is still told (removal), since a removal says what failed whether or
// not the failure is why GitHub made it.
func (t *timelineItem) judgedByChecks() bool {
	if t.Type != "RemovedFromMergeQueueEvent" {
		return false
	}
	switch t.Reason {
	case "merged", "manual", "merge_conflict":
		return false
	default:
		return true
	}
}

// restarts says whether a removal restarts the submission rather than ending it: auto-merge a close
// turned off (reason code closed), which the reopen at READY's head undid, and the daemon's own
// disarm, made by the account the token acts as (self, its login without [bot], as the timeline
// names a bot) with the reason a disarm leaves: auto-merge manually_disabled, or a manual dequeue.
// GitHub names the App that enabled auto-merge as the actor of its own disables (a merge method
// the repository blocks, a base switch), so the actor alone does not make a disable the daemon's.
func (t *timelineItem) restarts(self string) bool {
	if t.Type == "AutoMergeDisabledEvent" && t.ReasonCode == "closed" {
		return true
	}
	if self == "" || t.actor() != self {
		return false
	}
	return t.Type == "AutoMergeDisabledEvent" && t.ReasonCode == "manually_disabled" ||
		t.Type == "RemovedFromMergeQueueEvent" && t.Reason == "manual"
}

// timelineItem is one arm or removal on a pull request's timeline as removals reads it.
type timelineItem struct {
	Type       string    `json:"__typename"`
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"createdAt"`
	Reason     string    `json:"reason"`
	ReasonCode string    `json:"reasonCode"`
	Actor      *struct {
		Login string `json:"login"`
	} `json:"actor"`
	BeforeCommit *mergeGroupCommit `json:"beforeCommit"`
}

// actor is the login of who made the item, "" when GitHub names no one.
func (t *timelineItem) actor() string {
	if t.Actor == nil {
		return ""
	}
	return t.Actor.Login
}

// removalReason is GitHub's reason for a removal: a queue removal's reason, or the reason code of
// an auto-merge turned off.
func (t *timelineItem) removalReason() string {
	if t.Type == "AutoMergeDisabledEvent" {
		return t.ReasonCode
	}
	return t.Reason
}

// detail is GitHub's sentence for an auto-merge turned off, and none for a queue removal, whose
// reason is all GitHub says of it.
func (t *timelineItem) detail() string {
	if t.Type == "AutoMergeDisabledEvent" {
		return t.Reason
	}
	return ""
}

// failingRuns is each of checks, every check on the commit the merge queue built for a queue
// removal (mergeGroupCommit.checks), that failed there, named with how it ended, the run it
// failed in and the Actions job it ran as (checkContext.run): a check run that ended in a result
// Legion's rule for a check run counts as failed (classify.RunResult: failure, timed out, failed
// to start, action required, stale), and a commit status in failure or error. A cancelled run is
// no failure here: a person's dequeue cancels every run in flight. A commit status is no Actions
// job and has no log of its own, so it names none.
//
// No log is read here: judging a removal is this package's own work and costs GitHub nothing
// beyond the pull request, and what each named job's log says is read afterwards by the caller
// that tells of the removal (ReadFailingTests).
func failingRuns(checks []checkContext) []FailingRun {
	var runs []FailingRun
	for _, check := range checks {
		switch check.Type {
		case "CheckRun":
			result := classify.RunResult(strings.ToLower(check.Status), strings.ToLower(check.Conclusion))
			if result == classify.Pending || result == classify.Success || result == classify.Cancelled {
				continue
			}
			link, job := check.run()
			runs = append(runs, FailingRun{Name: check.Name, Result: result, URL: link, JobID: job})
		case "StatusContext":
			if check.State == "FAILURE" || check.State == "ERROR" {
				runs = append(runs, FailingRun{Name: check.Context, Result: strings.ToLower(check.State), URL: check.TargetURL})
			}
		}
	}
	return runs
}

// ReadFailingTests fills in, for each failing run the removal err names, what the log of the
// Actions job it ran as says failed (jobFailures): the tests the end of that log states as
// failed with the first line stating one, or which way the daemon could not read them. A run
// GitHub named no Actions job for has no log at all, and is left as it stands.
//
// err is the removal: a *MergeFailed, or a *Removed, which is what a ConflictRemoval's caller
// passes its own Removal as where it tells of one — a *ConflictRemoval answers errors.As no
// *Removed on purpose, since whether its runs are ever told is the daemon's decision
// (conflictRemoval) and not this package's. Anything else is left alone.
//
// This is the one read of GitHub this package makes outside a submission, and it is made after
// the removal is decided and only where the row is about to tell of it — the fact a failed merge
// sends the issue back with, the notice a removal ends the row with, the message a further
// submission is posted with (the daemon's readyMerge) — so a run that only waits reads no log,
// and a decision already told is never read for twice. logs is the installation's repository
// REST base (githubrest.RepositoryAPI), the one route a job's log lives on, and ctx the context
// the reads run under, which its caller bounds as the whole of one removal's reads rather than
// leaving each read to its own bound (the daemon's removalLogReads): it is deliberately not the
// context the submission's own calls ran under, since no read of a 54 MB log finishes inside a
// merge row's 30 seconds. No failure of a read ever fails the removal: each is told in its own
// run's line and nowhere else.
//
// The logs are read at once, jobLogReads at a time, each into the run it belongs to, so the runs
// read in the order they came whatever order the reads finish in.
func ReadFailingTests(ctx context.Context, logs githubrest.Client, err error) {
	var runs []FailingRun
	var failed *MergeFailed
	var removed *Removed
	switch {
	case errors.As(err, &failed):
		runs = failed.Runs
	case errors.As(err, &removed):
		runs = removed.Runs
	default:
		return
	}
	var group errgroup.Group
	group.SetLimit(jobLogReads)
	for i := range runs {
		run := &runs[i]
		if run.JobID == 0 {
			continue
		}
		group.Go(func() error {
			run.Log = jobFailures(ctx, logs, run.JobID)
			return nil
		})
	}
	_ = group.Wait() // no log read ever fails a removal: each failure of one is told in its own line
}

// jobLogReads is how many of a removal's failing jobs have their logs read at once. One read is
// bounded at the log client's own time bound (githubrest.Client.JobLogTimeBound), a minute in
// production, and the whole of a removal's reads by the one deadline its caller gives them
// (ReadFailingTests), since a 54 MB log cannot be read inside a merge row's 30 seconds (the
// daemon's readyMerge). Read one after another, three failing jobs — an ordinary matrix shape —
// would hold the row three minutes; read at once they cost about one read instead, and the limit
// is what holds a merge group with dozens of failing jobs to eight reads of GitHub open at a
// time, and so to eight rings of githubrest.JobLogTail, rather than one per job.
const jobLogReads = 8

// jobFailures is what the log of Actions job id says failed, written as the end of that run's own
// line: the tests the log names as failed and the first line naming one (failingTests), or, when
// the daemon could not read them, which of the five ways it could not — the end of the log states
// no test name, the body passed the byte bound, the read passed the time bound, the fetch failed,
// or GitHub refused the read for want of the Actions grant, a deployment state and no defect. The
// run link the line already carries is what is left to read in every one of those. No failure of
// this read ever fails the removal it belongs to: each is told in the line and nowhere else.
func jobFailures(ctx context.Context, logs githubrest.Client, id int64) string {
	unread := func(why string) string { return ": the daemon could not read its failing tests, " + why }
	tail, err := logs.JobLog(ctx, id)
	switch {
	case errors.Is(err, githubrest.ErrJobLogForbidden):
		return unread("since GitHub refused the log: this deployment's App was never granted GitHub's optional Actions read")
	case errors.Is(err, githubrest.ErrJobLogTooLong):
		return unread(fmt.Sprintf("since its log passed the %d MiB this read takes", githubrest.JobLogBound>>20))
	case errors.Is(err, githubrest.ErrJobLogSlow):
		return unread(fmt.Sprintf("since the read passed its %s bound", logs.JobLogTimeBound()))
	case err != nil:
		return unread(fmt.Sprintf("since the fetch of its log failed: %s", fetchFailure(err)))
	}
	names, first := failingTests(tail)
	if len(names) == 0 {
		return unread("since the end of its log states no test name")
	}
	return fmt.Sprintf(": its log names %s as failing, its first failure line %q", strings.Join(names, ", "), first)
}

// fetchFailure is why a fetch of a job's log failed, with no URL in what it says: GitHub answers
// that read with a redirect to blob storage, and the URL it redirects to is pre-signed, so the
// *url.Error Go's client returns for a GET that failed on the way carries that whole URL, its
// sig= token included. Every line this package writes is rendered into Dispatch messages, the
// architect's notices and an implementer's prompt, which no signature belongs in: what is left
// is the request's own operation and what failed under it, which say what went wrong without
// saying where the token is. An error of any other shape says no URL of its own and is kept.
func fetchFailure(err error) string {
	var route *url.Error
	if errors.As(err, &route) {
		return fmt.Sprintf("%s: %v", route.Op, route.Err)
	}
	return err.Error()
}

// goFailure is what Go's test binary prints before the name of every test that failed;
// browserSeparator is what a browser runner puts between the parts of a failing row, and
// browserProject what such a row opens the project with.
const (
	goFailure        = "--- FAIL: "
	browserSeparator = " › "
	browserProject   = "["
)

// browserCount is the line that opens a browser runner's summary block at the end of its log, the
// count of the tests that failed ("  1 failed").
var browserCount = regexp.MustCompile(`^\s*\d+ failed\s*$`)

// logStamp is the prefix GitHub's job-log route writes before every line it carries: the UTC
// instant the line was printed and one space, as in "2026-11-07T18:04:24.8502586Z   1 failed",
// which stand before whatever the step printed, that line's own indentation included.
var logStamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z `)

// failingTests is what tail, the end of a failing job's log, states failed: each test it names as
// failed, and the first line naming one, verbatim. Every line is read with GitHub's timestamp
// prefix taken off it first (logStamp), since the job-log route the tail is read from writes one
// before every line; a line that carries none is read as it stands, so a log off that route and
// one off a runner's own output read the same. Two shapes are read, the two the measured cases
// carry, both printed at the end of a log, which is the part this read holds:
//
//   - Go's test binary, which prints "--- FAIL: <name> (<time>)" for each test that failed, its
//     subtests among them. The name is the word after the marker, and the first failure line that
//     whole line.
//   - a browser runner's summary block, a count of the failures ("  1 failed") followed by one
//     indented row for each, "[project] › file:line:col › title", until a row of another count
//     ends it ("  37 skipped"). The name is the row without the project it opens with, which the
//     first failure line, the whole row, keeps. The last such block in the tail is the one read,
//     since a runner prints its summary once, at the end.
//
// The "✘ <n> [project] › …" rows a browser runner prints as it goes are deliberately left out:
// they stand in the middle of the stream, which a read of the end never reaches. A tail stating
// neither shape names no test, which the caller reports as a log it could not read (jobFailures).
func failingTests(tail string) ([]string, string) {
	lines := strings.Split(tail, "\n")
	for i, line := range lines {
		if stamp := logStamp.FindStringIndex(line); stamp != nil {
			lines[i] = line[stamp[1]:]
		}
	}
	var names []string
	first := ""
	for _, line := range lines {
		at := strings.Index(line, goFailure)
		if at < 0 {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimSpace(line[at+len(goFailure):]), " ")
		if name == "" {
			continue
		}
		if first == "" {
			first = strings.TrimSpace(line)
		}
		names = append(names, name)
	}
	if len(names) > 0 {
		return names, first
	}
	block := -1
	for i, line := range lines {
		if browserCount.MatchString(line) {
			block = i
		}
	}
	if block < 0 {
		return nil, ""
	}
	for _, line := range lines[block+1:] {
		row := strings.TrimSpace(line)
		if !strings.HasPrefix(line, " ") || !strings.Contains(row, browserSeparator) {
			break
		}
		if first == "" {
			first = row
		}
		name := row
		if test, found := strings.CutPrefix(row, browserProject); found {
			if _, after, cut := strings.Cut(test, browserSeparator); cut {
				name = after
			}
		}
		names = append(names, name)
	}
	return names, first
}

// mergeGroupCommit is the commit the merge queue built for a pull request it took out, and how its
// checks ended.
type mergeGroupCommit struct {
	Oid               string `json:"oid"`
	StatusCheckRollup *struct {
		Contexts checkContexts `json:"contexts"`
	} `json:"statusCheckRollup"`
}

// checkContexts is the checks GitHub listed on a merge queue commit, and whether it lists more.
type checkContexts struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []checkContext `json:"nodes"`
}

// page is the checks GitHub listed on the commit in the answer that carried it, the first hundred
// of them: what a removal whose reason GitHub explains by itself names its failing runs from,
// where a page further would cost a request. A nil commit, or one with no checks, has none.
func (c *mergeGroupCommit) page() []checkContext {
	if c == nil || c.StatusCheckRollup == nil {
		return nil
	}
	return c.StatusCheckRollup.Contexts.Nodes
}

// checks is every check GitHub lists on the commit: the first page, as the timeline's read carried
// it, then each later page, read one by one (moreChecks), so a judgment of them misses no failure
// past the first. A nil commit, or one with no checks, has none.
func (c *mergeGroupCommit) checks(ctx context.Context, github githubrest.Client, repo ghrepo.Repository) ([]checkContext, error) {
	if c == nil || c.StatusCheckRollup == nil {
		return nil, nil
	}
	checks, page := c.page(), c.StatusCheckRollup.Contexts.PageInfo
	for page.HasNextPage {
		var answer struct {
			Repository struct {
				Object *mergeGroupCommit `json:"object"`
			} `json:"repository"`
		}
		if err := github.GraphQL(ctx, moreChecks, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "oid": c.Oid, "after": page.EndCursor}, &answer); err != nil {
			return nil, fmt.Errorf("read the checks of merge queue commit %s after %s: %w", c.Oid, page.EndCursor, err)
		}
		next := answer.Repository.Object
		if next == nil || next.StatusCheckRollup == nil {
			return nil, fmt.Errorf("GitHub answered no further checks of merge queue commit %s after %s", c.Oid, page.EndCursor)
		}
		checks = append(checks, next.StatusCheckRollup.Contexts.Nodes...)
		page = next.StatusCheckRollup.Contexts.PageInfo
	}
	return checks, nil
}

// checkContext is one check run or commit status on a merge queue's commit. JobID is GitHub's
// databaseId for a check run, which is the Actions job it ran as where an Actions workflow run
// is what made it (checkContext.run), and zero for a check run GitHub gives none and for a
// commit status, neither of which has an Actions log to read.
type checkContext struct {
	Type       string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	JobID      int64  `json:"databaseId"`
	CheckSuite *struct {
		WorkflowRun *struct {
			URL string `json:"url"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"targetUrl"`
}

// run is the run a failing check names, and the Actions job whose log says what failed in it: a
// check run GitHub names a workflow run for is a job of that run, read by the databaseId GitHub
// answers for it, and the run's own URL is the link. A check run with no workflow run is a
// third-party App's, which is no Actions job however GitHub numbers it — its log route answers
// 404 — so it is named by its details URL and read for nothing. A workflow run GitHub gives no
// URL leaves the check's own details URL as the link, the job still being its job.
func (c *checkContext) run() (string, int64) {
	if c.CheckSuite == nil || c.CheckSuite.WorkflowRun == nil {
		return c.DetailsURL, 0
	}
	if c.CheckSuite.WorkflowRun.URL == "" {
		return c.DetailsURL, c.JobID
	}
	return c.CheckSuite.WorkflowRun.URL, c.JobID
}
