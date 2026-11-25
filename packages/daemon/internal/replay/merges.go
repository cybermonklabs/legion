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
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
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
	// From and To bound the commits kept, by their committer's time: [From, To). The listing
	// reaches from mergesMargin before From to the branch's tip: GitHub's since, and no until.
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
// commit listed, through every listed commit (ParseMerges), and keeps the commits committed in
// [cfg.From, cfg.To). A commit's first parent can be committed after it, after cfg.To too, and the
// walk goes through such a parent; a listing with an until would leave it out, and the walk would
// end there. A walk that ends at a commit committed before cfg.From, in the margin, or at a root
// commit holds the span's history whole. A walk that ends at a first parent the listing lacks,
// while the last commit it walked was committed at or after cfg.From, reads that parent (GET
// /repos/<owner>/<name>/commits/<sha>): a parent committed before the listing's since is where a
// branch that was quiet before the span ends, and the walk ends there too; a parent committed at
// or after since is a commit the listing lacks, so the span's history before it would be left
// out, and the walk is refused, naming the last commit walked and that parent, each with its
// time, and nothing is written.
// Each commit keeps the pull request it merged (Merge.PR) where the first line of its message ends
// with GitHub's squash suffix ` (#<n>)`, and none where it does not, so the conflict draw can leave
// a stretch's own merge out of its count. It sends GET alone, each read GitHub refuses for now
// (429, a 5xx, no answer) tried again with a growing wait. It returns the branch's commits as
// written, oldest first.
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
	first := api.JoinPath("repos", owner, name, "commits")
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
		parentAt, err := reader.committed(ctx, api.JoinPath("repos", owner, name, "commits", lacking))
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
	merges := slices.DeleteFunc(walk, func(m Merge) bool { return m.At.Before(cfg.From) || !m.At.Before(cfg.To) })
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

// githubReader reads GitHub's REST API with a token: the pages of one listing, and a commit.
type githubReader struct {
	api    *url.URL
	token  string
	client *http.Client
	wait   func(ctx context.Context, d time.Duration) error
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
		var body []byte
		var link string
		err := retry(ctx, r.wait, retryableGitHub, func() (err error) {
			body, link, err = r.get(ctx, next)
			return err
		})
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
	var body []byte
	err := retry(ctx, r.wait, retryableGitHub, func() (err error) {
		body, _, err = r.get(ctx, commit)
		return err
	})
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
		return nil, "", &githubRefusal{status: response.StatusCode, body: string(bytes.TrimSpace(body[:min(len(body), 300)]))}
	}
	return body, response.Header.Get("Link"), nil
}

// githubRefusal is a page GitHub answered with a status other than 2xx, and the start of what it
// said.
type githubRefusal struct {
	status int
	body   string
}

func (e *githubRefusal) Error() string {
	return fmt.Sprintf("GitHub answered %d %s: %s", e.status, http.StatusText(e.status), e.body)
}

// githubUnanswered is a page GitHub gave no whole answer to.
type githubUnanswered struct{ err error }

func (e *githubUnanswered) Error() string { return "GitHub gave no answer: " + e.err.Error() }
func (e *githubUnanswered) Unwrap() error { return e.err }

// retryableGitHub says whether GitHub refused a read for now: it answered 429 or a 5xx, or gave no
// whole answer, the client's own timeout among them (net/http reports it as a deadline exceeded).
// A read the caller cancelled is no refusal of GitHub's, and is not tried again; a caller's
// deadline that passed ends the wait before the next attempt (retry).
func retryableGitHub(err error) bool {
	var refusal *githubRefusal
	if errors.As(err, &refusal) {
		return refusal.status == http.StatusTooManyRequests || refusal.status >= 500
	}
	var unanswered *githubUnanswered
	return errors.As(err, &unanswered) && !errors.Is(err, context.Canceled)
}
