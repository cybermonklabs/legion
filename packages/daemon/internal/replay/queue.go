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
	w, err := ReadWindow(cfg.Window)
	if err != nil {
		return QueueCollected{}, fmt.Errorf("replay: collect-queue: %w", err)
	}
	numbers, noRepo, err := queuedPullRequests(w, cfg.Repo)
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
	if w.Queue == nil {
		w.Queue = make(map[string][]json.RawMessage)
	}
	w.Queue[cfg.Repo] = lines
	if w.Signals == nil {
		w.Signals = make(map[string][]Span)
	}
	w.Signals[SignalQueue] = []Span{{From: w.From.Add(-time.Duration(w.Before)), To: w.End()}}
	if err := w.Write(cfg.Window); err != nil {
		return QueueCollected{}, fmt.Errorf("replay: collect-queue: %w", err)
	}
	return QueueCollected{PullRequests: numbers, Lines: len(lines), NoRepo: noRepo}, nil
}

// queuedPullRequests is the pull requests of repo whose merge queue events w's queue file of repo
// holds (CollectQueue), ascending, and each pull request a line of w's log names for an issue whose
// repository w's events do not give and no attribution names, ascending. An attribution event of
// an issue of another project than w's is not read.
func queuedPullRequests(w Window, repo string) ([]int, []int, error) {
	numbers := make(map[int]bool)
	issueRepo := make(map[string]string)
	for _, key := range slices.Sorted(maps.Keys(w.Events)) {
		for _, raw := range w.Events[key] {
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				return nil, nil, fmt.Errorf("an event of %s does not read: %w", key, err)
			}
			switch head.Type {
			case attributionEvent:
				if project, _, _ := strings.Cut(key, "-"); project != w.Project {
					continue
				}
				event, err := ParseDispatchEvent(raw)
				if err != nil {
					return nil, nil, err
				}
				if event.Attribution.Repo == repo && event.Attribution.Number > 0 {
					numbers[event.Attribution.Number] = true
				}
			case "issue.created", "issue.updated", "issue.closed":
				if _, known := issueRepo[key]; !known {
					if _, named := readIssuePayload(raw).subject(); named != "" {
						issueRepo[key] = named
					}
				}
			}
		}
	}
	noRepo := make(map[int]bool)
	for _, raw := range w.Log {
		var line struct {
			Issue       string `json:"issue"`
			PullRequest *int   `json:"pullRequest"`
		}
		if json.Unmarshal(raw, &line) != nil || line.PullRequest == nil || *line.PullRequest <= 0 {
			continue
		}
		switch issueRepo[line.Issue] {
		case repo:
			numbers[*line.PullRequest] = true
		case "":
			noRepo[*line.PullRequest] = true
		}
	}
	for number := range numbers {
		delete(noRepo, number)
	}
	return slices.Sorted(maps.Keys(numbers)), slices.Sorted(maps.Keys(noRepo)), nil
}
