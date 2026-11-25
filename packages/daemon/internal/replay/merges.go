package replay

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// DefaultGitHubAPI is GitHub's REST API, which collect-merges reads where its caller names no
// other (a GitHub Enterprise server's, or a test's).
const DefaultGitHubAPI = "https://api.github.com"

// mergesPageSize is how many commits a page of GitHub's listing holds, its largest.
const mergesPageSize = 100

// mergesPageLimit bounds the pages one branch is read in, so a listing that never ends is refused
// rather than read for ever: a hundred thousand commits in a window's span.
const mergesPageLimit = 1000

// mergesMargin is how far before a span's start collect-merges lists a branch's commits from. A
// commit's first parent can be committed a little before it, so a walk through the span's commits
// can pass through a commit of the margin with no read of its own; and a walk that reaches a
// commit before the span holds the span's history whole (CollectMerges).
const mergesMargin = 24 * time.Hour

// MergesConfig is one read of a base branch's first-parent history into a window (CollectMerges).
type MergesConfig struct {
	// Repo is the repository as owner/name, and Branch its base branch.
	Repo, Branch string
	// From and To bound the commits kept, by the time each landed on the branch as Merge.At gives
	// it: [From, To). The listing reaches from mergesMargin before From to the branch's tip, by
	// the commits' committer's times: GitHub's since, and no until.
	From, To time.Time
	// Token is the GitHub token the reads are made with, sent as the Authorization header alone
	// and never written anywhere.
	Token string
	// API is GitHub's REST API (DefaultGitHubAPI); every page is read from its scheme and host.
	API string
	// Window is the directory of the window the merges are written into, which collect wrote.
	Window string
	// Wait is how a read GitHub refused for now waits before it is tried again; nil waits on a
	// timer (backoff.Sleep).
	Wait func(ctx context.Context, d time.Duration) error
}

// CollectMerges reads cfg.Branch's first-parent history in [cfg.From, cfg.To) from GitHub and
// writes it into the window in cfg.Window as merges/<owner>__<name>.jsonl, in place of the commits
// of that branch the file held and beside those of the repository's other base branches, with
// window.json's sha256 of the file written again. It lists the branch's commits from a day before
// cfg.From to its tip (GET /repos/<owner>/<name>/commits?sha=<branch>&since=<from − mergesMargin>,
// with no until, every page by its Link header), walks first parents from the tip, the first
// commit listed, through every listed commit (ParseMerges), places each commit walked at the time
// it landed on the branch as far as GitHub records it (mergeEvent), and keeps the commits so
// placed in [cfg.From, cfg.To), in the order of those times and, at one time, in first-parent
// order. A commit's first parent can be committed after it, after cfg.To too, and the walk goes
// through such a parent; a listing with an until would leave it out, and the walk would end
// there. A walk that ends at a commit committed before cfg.From, in the margin, or at a root
// commit holds the span's history whole. A walk that ends at a first parent the listing lacks,
// while the last commit it walked was committed at or after cfg.From, reads that parent (GET
// /repos/<owner>/<name>/commits/<sha>): a parent committed before the listing's since is where a
// branch that was quiet before the span ends, and the walk ends there too; a parent committed at
// or after since is a commit the listing lacks, so the span's history before it would be left
// out, and the walk is refused, naming the last commit walked and that parent, each with its
// time, and nothing is written. Both checks read committer's times, as GitHub's since does, so
// a first-parent commit committed before since whose merge event falls in the span all the same,
// as one a merge queue held for longer than mergesMargin, is left out of the history written: the
// margin bounds what the walk reaches, and the commits kept are no census of the span's merge
// events.
// A commit walked lands at GitHub's merge event, the merged_at of the pull request GitHub records
// as merged as that commit (merge_commit_sha) into cfg.Branch (base.ref), and keeps that pull
// request (Merge.PR) with Merge.Event set, so the conflict draw can leave a stretch's own merge
// out of its count. A commit GitHub records no such merge of keeps its committer's time, an
// estimate and no merge event, with no pull request, whatever its subject names. Those reads
// follow the listing and the read of a lacking parent, and read nothing of a commit the walk did
// not reach. It sends GET alone: a read GitHub refuses for now (a 5xx, no answer) is tried again
// with a growing wait, and one GitHub's rate limit refuses is tried again after the wait GitHub
// names, each such wait told on stderr and bounded (githubReader.read); any other refusal but the
// 404 of a pull request a subject names, or an answer that is no commit or pull request, fails
// the read, and nothing is written. It returns the branch's commits as written, oldest first.
//
// It reads the window once, a line at a time (walkWindow), and refuses every window ReadWindow
// refuses, before any read of GitHub. Of the window it holds its window.json and the lines of
// cfg.Repo's merges file alone, never another input. It writes that file and window.json alone,
// each through a temporary file renamed into place (replaceInput); every other input of the window
// keeps its bytes.
func CollectMerges(ctx context.Context, cfg MergesConfig) ([]Merge, error) {
	owner, name, ok := splitRepo(cfg.Repo)
	switch {
	case !ok:
		return nil, fmt.Errorf("replay: collect-merges: %q is no owner/name of a repository", cfg.Repo)
	case cfg.Branch == "":
		return nil, errors.New("replay: collect-merges: no branch is named")
	case !cfg.From.Before(cfg.To):
		return nil, fmt.Errorf("replay: collect-merges: the span [%s, %s) is empty", cfg.From.Format(time.RFC3339), cfg.To.Format(time.RFC3339))
	case cfg.Token == "":
		return nil, errors.New("replay: collect-merges: no GitHub token is given")
	}
	api, err := url.Parse(strings.TrimRight(cmp.Or(cfg.API, DefaultGitHubAPI), "/"))
	if err != nil || (api.Scheme != "https" && api.Scheme != "http") || api.Host == "" {
		return nil, fmt.Errorf("replay: collect-merges: %q is no http or https URL of GitHub's API", cfg.API)
	}
	head, err := readWindowHead(cfg.Window)
	if err != nil {
		return nil, fmt.Errorf("replay: collect-merges: %w", err)
	}
	var held []Merge
	err = walkWindow(cfg.Window, head, func(input windowInput) func(json.RawMessage, any) {
		if input.kind != mergesDir || input.key != cfg.Repo {
			return nil
		}
		return func(_ json.RawMessage, merge any) { held = append(held, merge.(Merge)) }
	})
	if err != nil {
		return nil, fmt.Errorf("replay: collect-merges: %w", err)
	}
	reader := githubReader{api: api, token: cfg.Token, client: &http.Client{Timeout: time.Minute}, wait: cfg.Wait}
	if reader.wait == nil {
		reader.wait = backoff.Sleep
	}
	since := cfg.From.Add(-mergesMargin).UTC().Truncate(time.Second)
	query := url.Values{}
	query.Set("sha", cfg.Branch)
	query.Set("since", since.Format(time.RFC3339))
	query.Set("per_page", fmt.Sprint(mergesPageSize))
	repository := api.JoinPath("repos", owner, name)
	first := repository.JoinPath("commits")
	first.RawQuery = query.Encode()
	pages, err := reader.pages(ctx, first)
	if err != nil {
		return nil, fmt.Errorf("replay: collect-merges: the commits of %s %s: %w", cfg.Repo, cfg.Branch, err)
	}
	walk, lacking, err := ParseMerges(cfg.Branch, pages...)
	if err != nil {
		return nil, fmt.Errorf("replay: collect-merges: %w", err)
	}
	if lacking != "" && !walk[0].At.Before(cfg.From) {
		// The walk ends inside the span. A first parent committed before since is where a branch
		// quiet before the span ends; one committed later is a commit the listing lacks.
		parentAt, err := reader.committed(ctx, repository.JoinPath("commits", lacking))
		if err != nil {
			return nil, fmt.Errorf("replay: collect-merges: the commit %s of %s: %w", lacking, cfg.Repo, err)
		}
		if !parentAt.Before(since) {
			return nil, fmt.Errorf("replay: collect-merges: the first-parent history of %s %s is cut inside the span [%s, %s): "+
				"the walk of GitHub's listing since %s ends at %s, committed %s, and the listing lacks its first parent %s, committed %s; "+
				"nothing is written", cfg.Repo, cfg.Branch, cfg.From.UTC().Format(time.RFC3339), cfg.To.UTC().Format(time.RFC3339),
				since.Format(time.RFC3339), walk[0].SHA, walk[0].At.UTC().Format(time.RFC3339), lacking, parentAt.UTC().Format(time.RFC3339))
		}
	}
	for i, commit := range walk {
		if walk[i], err = reader.mergeEvent(ctx, repository, cfg.Branch, commit); err != nil {
			return nil, fmt.Errorf("replay: collect-merges: the merge of the commit %s of %s %s: %w", commit.SHA, cfg.Repo, cfg.Branch, err)
		}
	}
	merges := slices.DeleteFunc(walk, func(m Merge) bool { return m.At.Before(cfg.From) || !m.At.Before(cfg.To) })
	slices.SortStableFunc(merges, func(a, b Merge) int { return a.At.Compare(b.At) })
	kept := slices.DeleteFunc(held, func(m Merge) bool { return m.Branch == cfg.Branch })
	kept = append(kept, merges...)
	slices.SortStableFunc(kept, func(a, b Merge) int { return a.At.Compare(b.At) })
	lines, err := marshalLines(kept)
	if err != nil {
		return nil, fmt.Errorf("replay: collect-merges: %w", err)
	}
	if err := replaceInput(cfg.Window, head, path.Join(mergesDir, owner+"__"+name+".jsonl"), lines); err != nil {
		return nil, fmt.Errorf("replay: collect-merges: %w", err)
	}
	return merges, nil
}

// githubReader reads GitHub's REST API with a token: the pages of one listing, a commit and a pull
// request. It waits with wait, and tells each wait for GitHub's rate limit on stderr, os.Stderr
// where nil.
type githubReader struct {
	api    *url.URL
	token  string
	client *http.Client
	wait   func(ctx context.Context, d time.Duration) error
	stderr io.Writer
}

// nextPage is the next page's URL in a Link header GitHub writes on a listing's page.
var nextPage = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// pages reads first and every page its Link headers lead to, in order. A next page on another
// scheme or host than the API's is refused, so the token goes nowhere else.
func (r githubReader) pages(ctx context.Context, first *url.URL) ([][]byte, error) {
	var pages [][]byte
	for next := first; next != nil; {
		if len(pages) == mergesPageLimit {
			return nil, fmt.Errorf("the listing runs past %d pages", mergesPageLimit)
		}
		if next.Scheme != r.api.Scheme || next.Host != r.api.Host {
			return nil, fmt.Errorf("the next page is at %s://%s, not at the API's %s://%s", next.Scheme, next.Host, r.api.Scheme, r.api.Host)
		}
		body, link, err := r.read(ctx, next)
		if err != nil {
			return nil, err
		}
		pages = append(pages, body)
		next = nil
		if match := nextPage.FindStringSubmatch(link); match != nil {
			if next, err = url.Parse(match[1]); err != nil {
				return nil, fmt.Errorf("the next page %q is no URL: %w", match[1], err)
			}
		}
	}
	return pages, nil
}

// committed reads one commit (GET /repos/<owner>/<name>/commits/<sha>) and returns its
// committer's time. An answer that is not a commit with its committer's time is refused.
func (r githubReader) committed(ctx context.Context, commit *url.URL) (time.Time, error) {
	body, _, err := r.read(ctx, commit)
	if err != nil {
		return time.Time{}, err
	}
	var read struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &read); err != nil {
		return time.Time{}, fmt.Errorf("GitHub's answer is no commit: %w", err)
	}
	if read.Commit.Committer.Date.IsZero() {
		return time.Time{}, errors.New("GitHub's answer is a commit without its committer's time")
	}
	return read.Commit.Committer.Date, nil
}

// mergeEvent is merge, a first-parent commit of branch in repository (its API URL) as ParseMerges
// reads it, placed at GitHub's merge event of the pull request GitHub records as merged as that
// commit: the one whose merge_commit_sha is the commit's sha and whose base.ref is branch, with a
// merged_at, which is then At, its number PR, and Event set. The pull request the commit's squash
// suffix names (merge.PR) is a hint: it is read first (GET <repository>/pulls/<n>) and taken where
// it matches. Where it does not, where GitHub has no such pull request (404), or where the subject
// names none, as a merge commit's does, the pull requests GitHub associates with the commit are
// read (GET <repository>/commits/<sha>/pulls, every page) and the one of them that matches is
// taken; an entry that merged into branch but leaves out merge_commit_sha, as that listing's
// schema allows, is read whole (GET <repository>/pulls/<n>) to match it. Where none matches, or
// more than one does, GitHub gives the commit no merge event of its own, and it keeps its
// committer's time, an estimate, with no Event and no pull request: a suffix GitHub's record does
// not bear out names no stretch's own merge. An answer that is no list of pull requests, or no
// pull request, is refused.
func (r githubReader) mergeEvent(ctx context.Context, repository *url.URL, branch string, merge Merge) (Merge, error) {
	matches := func(pull githubrest.PullRequest) bool {
		return pull.MergeCommitSHA == merge.SHA && pull.Base.Ref == branch && pull.MergedAt != nil
	}
	landed := func(pull githubrest.PullRequest) Merge {
		return Merge{SHA: merge.SHA, At: *pull.MergedAt, Branch: merge.Branch, PR: pull.Number, Event: true}
	}
	if merge.PR != 0 {
		pull, err := r.pullRequest(ctx, repository, merge.PR)
		var refusal *githubRefusal
		switch {
		case err == nil && matches(pull):
			return landed(pull), nil
		case err != nil && (!errors.As(err, &refusal) || refusal.status != http.StatusNotFound):
			return Merge{}, fmt.Errorf("the pull request #%d its subject names: %w", merge.PR, err)
		}
	}
	listing := repository.JoinPath("commits", merge.SHA, "pulls")
	listing.RawQuery = url.Values{"per_page": {fmt.Sprint(mergesPageSize)}}.Encode()
	pages, err := r.pages(ctx, listing)
	if err != nil {
		return Merge{}, fmt.Errorf("its pull requests: %w", err)
	}
	var matched []githubrest.PullRequest
	for _, page := range pages {
		var pulls []githubrest.PullRequest
		if err := json.Unmarshal(page, &pulls); err != nil {
			return Merge{}, fmt.Errorf("GitHub's answer is no list of its pull requests: %w", err)
		}
		for _, pull := range pulls {
			switch number := pull.Number; {
			case number <= 0:
				return Merge{}, errors.New("GitHub lists a pull request of it without its number")
			case number == merge.PR:
				continue // read whole above, where it did not match
			case pull.MergeCommitSHA == "" && pull.Base.Ref == branch && pull.MergedAt != nil:
				if pull, err = r.pullRequest(ctx, repository, number); err != nil {
					return Merge{}, fmt.Errorf("the pull request #%d GitHub lists of it: %w", number, err)
				}
			}
			if matches(pull) {
				matched = append(matched, pull)
			}
		}
	}
	if len(matched) != 1 {
		return Merge{SHA: merge.SHA, At: merge.At, Branch: merge.Branch}, nil
	}
	return landed(matched[0]), nil
}

// pullRequest reads pull request number of repository (GET <repository>/pulls/<number>) as
// githubrest.PullRequest decodes it. An answer that is not that pull request is refused.
func (r githubReader) pullRequest(ctx context.Context, repository *url.URL, number int) (githubrest.PullRequest, error) {
	body, _, err := r.read(ctx, repository.JoinPath("pulls", strconv.Itoa(number)))
	if err != nil {
		return githubrest.PullRequest{}, err
	}
	var pull githubrest.PullRequest
	if err := json.Unmarshal(body, &pull); err != nil {
		return githubrest.PullRequest{}, fmt.Errorf("GitHub's answer is no pull request: %w", err)
	}
	if pull.Number != number {
		return githubrest.PullRequest{}, fmt.Errorf("GitHub's answer is not pull request #%d", number)
	}
	return pull, nil
}

// The bounds of the waits for GitHub's rate limit (githubReader.read). rateLimitLongestWait bounds
// one wait: an hour, the window of GitHub's primary limit, so that no reset is further ahead, and
// five minutes more for rateLimitMargin and a clock a little behind GitHub's. A time to try again
// past it is refused, naming that time, so the operator runs the collector again then rather than
// leave it quiet for longer. rateLimitWaits bounds the waits of one read, as GitHub asks ("throw an
// error after a specific number of retries"): a secondary limit that names no time is waited a
// minute, then two, four, eight and sixteen, 31 minutes in all, and a primary limit waited out to
// its reset leaves four waits for a budget that a client sharing it spends again at once.
// rateLimitMargin is how long after x-ratelimit-reset, a whole second, the read is tried again.
const (
	rateLimitLongestWait = 65 * time.Minute
	rateLimitWaits       = 5
	rateLimitMargin      = 5 * time.Second
)

// read reads one page as get does. A read GitHub refuses for now (retryableGitHub) is tried again
// with a growing wait (retry). A read GitHub's rate limit refuses is tried again after the wait
// GitHub names (rateLimitWait), each wait told on stderr with the time it ends, at most
// rateLimitWaits times: a wait past rateLimitLongestWait, or a refusal after the last wait, is the
// read's refusal, naming the time GitHub names to try again. The context's end ends a wait at once.
func (r githubReader) read(ctx context.Context, page *url.URL) ([]byte, string, error) {
	for waited := 0; ; waited++ {
		var body []byte
		var link string
		err := retry(ctx, r.wait, retryableGitHub, func() (err error) {
			body, link, err = r.get(ctx, page)
			return err
		})
		var refusal *githubRefusal
		if !errors.As(err, &refusal) || !refusal.limited {
			return body, link, err
		}
		now := time.Now()
		at, wait := refusal.rateLimitWait(now, waited)
		switch {
		case waited == rateLimitWaits:
			return nil, "", fmt.Errorf("GitHub's rate limit refuses the read again after %d waits, the most one read makes, and names %s to try again: %w",
				rateLimitWaits, at.UTC().Format(time.RFC3339), err)
		case wait > rateLimitLongestWait:
			return nil, "", fmt.Errorf("GitHub's rate limit refuses the read until %s, %s from now, past the %s one wait lasts at most: %w",
				at.UTC().Format(time.RFC3339), at.Sub(now).Round(time.Second), rateLimitLongestWait, err)
		}
		stderr := r.stderr
		if stderr == nil {
			stderr = os.Stderr
		}
		fmt.Fprintf(stderr, "replay: GitHub's rate limit refused GET %s (%v); waiting %s, until %s, wait %d of at most %d for this read\n",
			page.Redacted(), err, wait.Round(time.Second), now.Add(wait).UTC().Format(time.RFC3339), waited+1, rateLimitWaits)
		if err := r.wait(ctx, wait); err != nil {
			return nil, "", err
		}
	}
}

// get reads one page: its body and its Link header.
func (r githubReader) get(ctx context.Context, page *url.URL) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, page.String(), nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("Authorization", "Bearer "+r.token)
	response, err := r.client.Do(request)
	if err != nil {
		return nil, "", &githubUnanswered{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, "", &githubUnanswered{err: err}
	}
	if response.StatusCode/100 != 2 {
		header := response.Header
		return nil, "", &githubRefusal{status: response.StatusCode, body: string(bytes.TrimSpace(body[:min(len(body), 300)])),
			limited: githubrest.RateLimited(response, body), limit: githubrest.LimitHeaders{RetryAfter: header.Get("Retry-After"),
				Limit: header.Get("X-RateLimit-Limit"), Remaining: header.Get("X-RateLimit-Remaining"), Reset: header.Get("X-RateLimit-Reset"),
				Resource: header.Get("X-RateLimit-Resource")}}
	}
	return body, response.Header.Get("Link"), nil
}

// githubRefusal is a page GitHub answered with a status other than 2xx: the status, the start of
// what GitHub said, whether its rate limit refused the page, as the daemon reads every answer of
// GitHub's (githubrest.RateLimited: a 429, or a 403 with x-ratelimit-remaining 0, a retry-after or
// a message naming the secondary limit), and the rate-limit headers GitHub sent with it.
type githubRefusal struct {
	status  int
	body    string
	limited bool
	limit   githubrest.LimitHeaders
}

func (e *githubRefusal) Error() string {
	return fmt.Sprintf("GitHub answered %d %s: %s", e.status, http.StatusText(e.status), e.body)
}

// rateLimitWait is when GitHub's rate limit, which refused a read after waited waits for it, lets
// the read be tried again, and how long from now to wait for it, as GitHub's REST documentation
// says (https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api,
// "Exceeding the rate limit"): a retry-after of N seconds, N seconds from now, a day at most so
// that no header wraps the duration; else, with x-ratelimit-remaining 0, the x-ratelimit-reset
// epoch second, waited to rateLimitMargin past it; else a minute, doubled for each wait before,
// for a secondary limit that names no time or a reset already past on this host's clock. It reads
// the headers in githubrest.LimitHeaders.WaitAt's order but holds no wait to an hour, since a time
// past rateLimitLongestWait is the read's refusal (githubReader.read).
func (e *githubRefusal) rateLimitWait(now time.Time, waited int) (time.Time, time.Duration) {
	if seconds, err := strconv.ParseInt(e.limit.RetryAfter, 10, 64); err == nil && seconds > 0 {
		wait := time.Duration(min(seconds, int64(24*time.Hour/time.Second))) * time.Second
		return now.Add(wait), wait
	}
	if reset, err := strconv.ParseInt(e.limit.Reset, 10, 64); err == nil && e.limit.Remaining == "0" {
		if at := time.Unix(reset, 0); at.After(now) {
			return at, at.Add(rateLimitMargin).Sub(now)
		}
	}
	wait := time.Minute << waited
	return now.Add(wait), wait
}

// githubUnanswered is a page GitHub gave no whole answer to.
type githubUnanswered struct{ err error }

func (e *githubUnanswered) Error() string { return "GitHub gave no answer: " + e.err.Error() }
func (e *githubUnanswered) Unwrap() error { return e.err }

// retryableGitHub says whether GitHub refused a read for now: it answered a 5xx, or gave no whole
// answer, the client's own timeout among them (net/http reports it as a deadline exceeded). A
// refusal of GitHub's rate limit, a 429 among them, is tried again after the wait GitHub names
// instead (githubReader.read). A read the caller cancelled is no refusal of GitHub's, and is not
// tried again; a caller's deadline that passed ends the wait before the next attempt (retry).
func retryableGitHub(err error) bool {
	var refusal *githubRefusal
	if errors.As(err, &refusal) {
		return !refusal.limited && refusal.status >= 500
	}
	var unanswered *githubUnanswered
	return errors.As(err, &unanswered) && !errors.Is(err, context.Canceled)
}
