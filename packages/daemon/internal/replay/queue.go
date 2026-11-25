package replay

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
)

// QueueConfig is one read of a repository's merge queue events into a window (CollectQueue).
type QueueConfig struct {
	// Repo is the repository as owner/name.
	Repo string
	// Token is the GitHub token the reads are made with, sent as the Authorization header alone
	// and never written anywhere.
	Token string
	// API is GitHub's REST API (DefaultGitHubAPI); every page is read from its scheme and host.
	API string
	// Window is the directory of the window the queue is written into, which collect wrote.
	Window string
	// Wait is how a read GitHub refused for now waits before it is tried again; nil waits on a
	// timer (backoff.Sleep).
	Wait func(ctx context.Context, d time.Duration) error
}

// QueueCollected is what a collect-queue read and wrote.
type QueueCollected struct {
	// PullRequests is each pull request of the repository whose timeline it read, ascending.
	PullRequests []int
	// Lines is how many merge queue events the queue file holds.
	Lines int
	// NoRepo is each pull request a line of the window's log names whose issue's repository the
	// window's events do not give, ascending, and that no attribution names either: its
	// repository is not known, so its timeline is read from no repository.
	NoRepo []int
}

// CollectQueue reads the merge queue events of cfg.Repo's pull requests of the window's project
// from GitHub and writes them into the window in cfg.Window as queue/<owner>__<name>.jsonl, in
// place of the file the window held of that repository, with window.json's sha256 of the file
// written again and a record of the queue signal over the window's reach (from its start less
// Before to its end with the run-out). The pull requests are those of cfg.Repo that an
// issue.pull_request_attribution_changed event of an issue of the window's project moves, to or
// from that issue, and those a line of the window's log names (pullRequest) for an issue whose
// repository, as its events first give it (the trace's IssueTrace.Repo), is cfg.Repo. It reads
// each one's timeline (GET
// /repos/<owner>/<name>/issues/<n>/timeline, every page by its Link header) and keeps the events of
// the kinds added_to_merge_queue, removed_from_merge_queue and merged, exactly as GitHub serves
// them, with the pull request's number (ParseQueueTimeline), the pull requests in ascending order.
// It sends GET alone, each read GitHub refuses for now (429, a 5xx, no answer) tried again with a
// growing wait, and writes nothing where a read fails.
//
// It reads the window once, a line at a time (walkWindow), and refuses every window ReadWindow
// refuses, before any read of GitHub. Of the window it holds its window.json and what names the
// pull requests alone (queueReading), never a whole input. It writes the queue file and
// window.json alone, each through a temporary file renamed into place (replaceInput); every other
// input of the window keeps its bytes.
func CollectQueue(ctx context.Context, cfg QueueConfig) (QueueCollected, error) {
	owner, name, ok := splitRepo(cfg.Repo)
	switch {
	case !ok:
		return QueueCollected{}, fmt.Errorf("replay: collect-queue: %q is no owner/name of a repository", cfg.Repo)
	case cfg.Token == "":
		return QueueCollected{}, errors.New("replay: collect-queue: no GitHub token is given")
	}
	api, err := url.Parse(strings.TrimRight(cmp.Or(cfg.API, DefaultGitHubAPI), "/"))
	if err != nil || (api.Scheme != "https" && api.Scheme != "http") || api.Host == "" {
		return QueueCollected{}, fmt.Errorf("replay: collect-queue: %q is no http or https URL of GitHub's API", cfg.API)
	}
	head, err := readWindowHead(cfg.Window)
	if err != nil {
		return QueueCollected{}, fmt.Errorf("replay: collect-queue: %w", err)
	}
	read := newQueueReading(head.Project, cfg.Repo)
	if err := walkWindow(cfg.Window, head, read.visit); err != nil {
		return QueueCollected{}, fmt.Errorf("replay: collect-queue: %w", err)
	}
	numbers, noRepo, err := read.pullRequests()
	if err != nil {
		return QueueCollected{}, fmt.Errorf("replay: collect-queue: %w", err)
	}
	reader := githubReader{api: api, token: cfg.Token, client: &http.Client{Timeout: time.Minute}, wait: cfg.Wait}
	if reader.wait == nil {
		reader.wait = backoff.Sleep
	}
	lines := []json.RawMessage{}
	for _, number := range numbers {
		first := api.JoinPath("repos", owner, name, "issues", strconv.Itoa(number), "timeline")
		first.RawQuery = url.Values{"per_page": {"100"}}.Encode()
		pages, err := reader.pages(ctx, first)
		if err != nil {
			return QueueCollected{}, fmt.Errorf("replay: collect-queue: the timeline of %s#%d: %w", cfg.Repo, number, err)
		}
		queued, err := ParseQueueTimeline(number, pages...)
		if err != nil {
			return QueueCollected{}, fmt.Errorf("replay: collect-queue: %s#%d: %w", cfg.Repo, number, err)
		}
		lines = append(lines, queued...)
	}
	if head.Signals == nil {
		head.Signals = make(map[string][]Span)
	}
	head.Signals[SignalQueue] = []Span{{From: head.From.Add(-time.Duration(head.Before)), To: head.End()}}
	if err := replaceInput(cfg.Window, head, path.Join(queueDir, owner+"__"+name+".jsonl"), lines); err != nil {
		return QueueCollected{}, fmt.Errorf("replay: collect-queue: %w", err)
	}
	return QueueCollected{PullRequests: numbers, Lines: len(lines), NoRepo: noRepo}, nil
}

// queueReading is what collect-queue keeps of a window's inputs as it walks them (visit), to read
// the pull requests of repo from (pullRequests): of each line of the log that names a pull request
// (pullRequest), that pull request and the issue the line names; each issue's repository, as the
// first of its issue.created, issue.updated and issue.closed events that names one gives it (the
// trace's IssueTrace.Repo); and each pull request of repo an attribution event of an issue of
// project moves. An attribution event of an issue of another project is not read.
type queueReading struct {
	project, repo string
	named         map[namedPullRequest]bool
	issueRepo     map[string]string
	attributed    map[int]bool
	// err is the first event, in the walk's order, that does not read.
	err error
}

// namedPullRequest is a pull request a line of a window's log names, with the issue it names.
type namedPullRequest struct {
	issue  string
	number int
}

// newQueueReading is the queueReading of the window of project, for repo, before its walk.
func newQueueReading(project, repo string) *queueReading {
	return &queueReading{project: project, repo: repo, named: make(map[namedPullRequest]bool), issueRepo: make(map[string]string),
		attributed: make(map[int]bool)}
}

// visit is the walk's visitor (windowVisitor): it reads the lines of the log and of the events,
// and no line of the other inputs.
func (q *queueReading) visit(input windowInput) func(json.RawMessage, any) {
	switch input.kind {
	case logFile:
		return q.logLine
	case eventsDir:
		project, _, _ := strings.Cut(input.key, "-")
		own := project == q.project
		return func(raw json.RawMessage, _ any) { q.event(input.key, own, raw) }
	}
	return nil
}

// logLine keeps the pull request raw, a line of the log, names, with the issue it names.
func (q *queueReading) logLine(raw json.RawMessage, _ any) {
	var line struct {
		Issue       string `json:"issue"`
		PullRequest *int   `json:"pullRequest"`
	}
	if json.Unmarshal(raw, &line) != nil || line.PullRequest == nil || *line.PullRequest <= 0 {
		return
	}
	q.named[namedPullRequest{issue: line.Issue, number: *line.PullRequest}] = true
}

// event keeps what raw, an event of the issue key, gives: the issue's repository, or the pull
// request of repo an attribution moves where key is an issue of the window's project (own).
func (q *queueReading) event(key string, own bool, raw json.RawMessage) {
	if q.err != nil {
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		q.err = fmt.Errorf("an event of %s does not read: %w", key, err)
		return
	}
	switch head.Type {
	case attributionEvent:
		if !own {
			return
		}
		event, err := ParseDispatchEvent(raw)
		if err != nil {
			q.err = err
			return
		}
		if event.Attribution.Repo == q.repo && event.Attribution.Number > 0 {
			q.attributed[event.Attribution.Number] = true
		}
	case "issue.created", "issue.updated", "issue.closed":
		if _, known := q.issueRepo[key]; !known {
			if _, named := readIssuePayload(raw).subject(); named != "" {
				q.issueRepo[key] = named
			}
		}
	}
}

// pullRequests is the pull requests of repo whose merge queue events the window's queue file of
// repo holds (CollectQueue), ascending, and each pull request a line of the log names for an issue
// whose repository the events do not give and no attribution names, ascending; or the first event
// that did not read.
func (q *queueReading) pullRequests() ([]int, []int, error) {
	if q.err != nil {
		return nil, nil, q.err
	}
	numbers := maps.Clone(q.attributed)
	noRepo := make(map[int]bool)
	for named := range q.named {
		switch q.issueRepo[named.issue] {
		case q.repo:
			numbers[named.number] = true
		case "":
			noRepo[named.number] = true
		}
	}
	for number := range numbers {
		delete(noRepo, number)
	}
	return slices.Sorted(maps.Keys(numbers)), slices.Sorted(maps.Keys(noRepo)), nil
}
