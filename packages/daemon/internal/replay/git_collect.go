package replay

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The phases of collect-git, as GitPullFinished names them.
const (
	GitPhaseGitHub = "github"
	GitPhaseGit    = "git"
)

// gitFetchBatch is how many commits, or pull requests' head refs, one fetch asks the remote for at
// most, gitListBatch how many head refs one git ls-remote asks it for, and gitMergeBatch how many
// base commits one git merge-tree --stdin merges a head with.
const (
	gitFetchBatch = 40
	gitListBatch  = 200
	gitMergeBatch = 200
)

// gitMergeObjects is the directory, in the clone beside its objects, that each merge-tree writes
// its merged trees and blobs into, and that collect-git removes as the merge-tree ends
// (gitClone.merge): a name git gives nothing of its own in a repository's directory.
const gitMergeObjects = "merge-objects"

// GitConfig is one collection of git's reading of a repository's pull requests into a window
// (CollectGit).
type GitConfig struct {
	// Repo is the repository as owner/name, and Branch its base branch, whose first-parent commits
	// the window's merges file of Repo holds.
	Repo, Branch string
	// Token is the GitHub token the GitHub phase reads with, sent as the Authorization header alone
	// and never written anywhere, and API GitHub's REST API (DefaultGitHubAPI), every page read from
	// its scheme and host. The git phase alone (GitOnly) takes neither.
	Token, API string
	// Window is the directory of the window collect wrote, which holds the merges file of Repo.
	Window string
	// Clones is the directory the repository is cloned into, as <owner>__<name>, and never one
	// inside the window; "" for clones/ in the window's parent directory.
	Clones string
	// MemoryCap is the memory guard's cap on git's processes, in bytes (gitGuard); 0 for
	// DefaultGitMemoryCap.
	MemoryCap int64
	// GitHubOnly runs the GitHub phase alone and GitOnly the git phase alone; with neither set,
	// CollectGit runs both, the GitHub phase first.
	GitHubOnly, GitOnly bool
	// Wait is how a read GitHub refused for now waits before it is tried again; nil waits on a
	// timer (backoff.Sleep).
	Wait func(ctx context.Context, d time.Duration) error
	// Reserve and Rate are the GitHub phase's share of the GitHub quota the daemon and the other
	// roles share (githubReader): the requests it leaves them, and the most requests a second it
	// sends; 0 turns either off. The git phase alone takes neither.
	Reserve int
	Rate    float64
	// Finished, where set, is told of each pull request a phase finished or kept, as it does.
	Finished func(GitPullFinished)

	// The tests' seams: remote is the URL the repository is cloned from,
	// https://github.com/<owner>/<name>.git where ""; onGit is told the arguments of each git
	// command (its subcommand on) before it runs; and rss reads a process group's resident memory
	// for the guard, groupRSS where nil.
	remote string
	onGit  func(args []string)
	rss    func(pgid int) int64
}

// GitPullFinished is a pull request one phase of collect-git finished: the phase, its number,
// whether the phase kept what the window's git file held of it rather than reading it again, and
// what the file now holds of it: its heads, their verdicts, the conflicts among those, and its
// missing inputs.
type GitPullFinished struct {
	Phase                               string
	PR                                  int
	Kept                                bool
	Heads, Verdicts, Conflicts, Missing int
}

// GitCollected is what a collect-git wrote: the pull requests, heads, verdicts and conflicts its
// file holds, its missing inputs by reason, the pull requests a line of the log names for an issue
// whose repository the window does not give (whose heads it reads from no repository), the reads
// of a merge state of its pull requests the log holds, the most memory git's processes held as the
// guard read it, and how long it took.
type GitCollected struct {
	PullRequests, Heads, Verdicts, Conflicts int
	Missing                                  map[string]int
	NoRepo                                   []int
	Reads                                    int
	PeakMemory                               int64
	Took                                     time.Duration
}

// CollectGit reads, for each pull request of cfg.Repo of the window's issues, its heads from
// GitHub and git's reading of each head against cfg.Branch, and writes them into the window in
// cfg.Window as git/<owner>__<name>.jsonl (GitInput), with window.json's sha256 of the file written
// again. The pull requests are the ones collect-queue reads (queueReading.pullRequests).
//
// The GitHub phase reads, with GET alone through the reader collect-queue and collect-merges read
// with (githubReader: a 5xx or no answer tried again, a rate limit waited out, cfg.Reserve
// requests of the shared quota left to the daemon and the other roles, at most cfg.Rate requests a
// second), each pull request (GET /repos/<owner>/<name>/pulls/<n>) and its whole timeline (GET
// /repos/<owner>/<name>/issues/<n>/timeline): its merge, merge_commit_sha where merged is true,
// base.ref is cfg.Branch and the merges file holds that commit; and its heads, each committed
// event's commit at its committer's time, which only estimates when the commit was pushed, and
// each head_ref_force_pushed event's new tip (commit_id) at the event's time. It writes the heads
// whose time falls in the window's inputs, from its start less Before to its end with the run-out;
// the head that stood at that start, the latest at or before it, a force push before a commit of
// the same second and a later event before an earlier one of the same second; each head the
// timeline gives that a read of the window's log names (MergeStateRead: its head and its recorded
// head); and, from "read" at the read's time, each such head the timeline does not give. Each
// distinct head is written once, at its earliest time. A pull request GitHub answers 404 Not Found
// for is a pull-not-found missing input, one GitHub still refuses to read after the reader's tries
// with its 422 of a pull request whose diff it cannot compute for now (tooManyFiles) a pull-unread
// one, whose timeline is not read, and a timeline page or event that does not read, a force push
// that names no commit among them, a timeline-unread one; any other refusal fails the run.
//
// The git phase reads no GitHub API. It clones the repository once, where cfg.Clones holds no
// clone of it, as git clone --no-checkout --bare --single-branch of cfg.Branch alone, every blob
// with it, from https://github.com/<owner>/<name>.git with the credential the environment's git
// credential helper gives, which it reads, prints and stores none of. It fetches each commit it
// needs that the clone lacks (GIT_NO_LAZY_FETCH=1 git cat-file --batch-check), its blobs with it:
// first the head ref of each pull request whose heads the clone lacks, where the remote holds one,
// then each commit still lacking, each into a ref of the clone's so a later fetch sends nothing
// twice (ensure). Each command that reads the clone runs with lazy fetching off, so none fetches.
// It writes, for each commit of cfg.Branch the merges file holds and each base commit a read names,
// the paths it changes, and for each head its tip, merge base, paths and verdicts (GitHead), each
// read once in the run however many pull requests hold the head, its verdicts from one git
// merge-tree --stdin of up to gitMergeBatch base commits (readVerdicts). A merge-tree writes its
// merged trees and blobs into a directory of its own that the run removes, so the clone does not
// grow with the merges (gitClone.merge). Every git command runs alone under the memory guard
// (gitGuard, cfg.MemoryCap) and its time, a merge-tree --stdin's that of one merge, counted from
// the last record it wrote; a command git fails, or the guard kills, is a missing input, and the
// run goes on.
//
// A run on a window whose git file of cfg.Repo exists asks for nothing the file holds. The GitHub
// phase keeps each pull request the file holds a pull line of and no missing input of GitHub's.
// The git phase keeps each commit's paths; a head's merge base where its tip is the same, its paths
// where its merge base is, and each verdict of the same head at the same base commit; and a head's
// whole reading, verdicts and all, where its tip is the same, no missing input names the head, and
// the commits the file holds are those the window names now. The git phase alone takes the pull
// and head lines from that file. So a run the pod's release killed, or one phase run in one place
// and the other in another, gives the file one run gives.
//
// It reads the window once, a line at a time (walkWindow), and refuses every window ReadWindow
// refuses, and a window that holds no merges file of cfg.Repo or no commit of cfg.Branch in it,
// before any read of GitHub or git. It writes the git file and window.json alone, each through a
// temporary file renamed into place (replaceInput), once the commits are read and again as each
// pull request is finished; every other input of the window keeps its bytes.
func CollectGit(ctx context.Context, cfg GitConfig) (GitCollected, error) {
	began := time.Now()
	refuse := func(format string, args ...any) (GitCollected, error) {
		return GitCollected{}, fmt.Errorf("replay: collect-git: "+format, args...)
	}
	owner, name, ok := splitRepo(cfg.Repo)
	switch {
	case !ok:
		return refuse("%q is no owner/name of a repository", cfg.Repo)
	case cfg.Branch == "":
		return refuse("no branch is named")
	case cfg.GitHubOnly && cfg.GitOnly:
		return refuse("the GitHub phase alone and the git phase alone exclude each other")
	case cfg.GitOnly && (cfg.Token != "" || cfg.API != "" || cfg.Reserve != 0 || cfg.Rate != 0):
		return refuse("the git phase alone reads no GitHub API, so it takes no GitHub token, API, reserve or rate")
	case !cfg.GitOnly && cfg.Token == "":
		return refuse("no GitHub token is given")
	case cfg.MemoryCap < 0:
		return refuse("the memory cap of %d bytes is below zero", cfg.MemoryCap)
	}
	if err := CheckGitHubShare(cfg.Reserve, cfg.Rate); err != nil {
		return refuse("%w", err)
	}
	var api *url.URL
	if !cfg.GitOnly {
		parsed, err := url.Parse(strings.TrimRight(cmp.Or(cfg.API, DefaultGitHubAPI), "/"))
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return refuse("%q is no http or https URL of GitHub's API", cfg.API)
		}
		api = parsed
	}
	clones := cfg.Clones
	if clones == "" {
		window, err := filepath.Abs(cfg.Window)
		if err != nil {
			return refuse("%w", err)
		}
		clones = filepath.Join(filepath.Dir(window), "clones")
	}
	if within(resolvedPath(cfg.Window), resolvedPath(clones)) {
		return refuse("the clones' directory %s is inside the window in %s", clones, cfg.Window)
	}
	head, err := readWindowHead(cfg.Window)
	if err != nil {
		return refuse("%w", err)
	}
	mergesName := path.Join(mergesDir, owner+"__"+name+".jsonl")
	if _, held := head.Files[mergesName]; !held {
		return refuse("the window in %s holds no merges file of %s (%s); run collect-merges first", cfg.Window, cfg.Repo, mergesName)
	}

	read := newQueueReading(head.Project, cfg.Repo)
	var merges []Merge
	var reads []MergeStateRead
	var old *gitReading
	var readErr error
	err = walkWindow(cfg.Window, head, func(input windowInput) func(json.RawMessage, any) {
		switch {
		case input.kind == logFile:
			return func(line json.RawMessage, value any) {
				read.logLine(line, value)
				switch r, err := mergeStateReadOf(line); {
				case err != nil && readErr == nil:
					readErr = err
				case r != nil && r.Repo == cfg.Repo:
					reads = append(reads, *r)
				}
			}
		case input.kind == mergesDir && input.key == cfg.Repo:
			return func(_ json.RawMessage, value any) {
				if merge := value.(Merge); merge.Branch == cfg.Branch {
					merges = append(merges, merge)
				}
			}
		case input.kind == gitDir && input.key == cfg.Repo:
			old = &gitReading{name: input.name}
			return func(_ json.RawMessage, value any) { old.add(value.(gitLineRead)) }
		}
		return read.visit(input)
	})
	if err == nil {
		err = readErr
	}
	if err != nil {
		return refuse("%w", err)
	}
	numbers, noRepo, err := read.pullRequests()
	if err != nil {
		return refuse("%w", err)
	}
	if len(merges) == 0 {
		return refuse("the merges file of %s holds no commit of %s", cfg.Repo, cfg.Branch)
	}
	var held GitInput
	if old != nil {
		if held, err = old.input(); err != nil {
			return refuse("read the window in %s: %w", cfg.Window, err)
		}
	} else if cfg.GitOnly {
		return refuse("the window in %s holds no git file of %s, whose pull and head lines the git phase reads; run the GitHub phase first",
			cfg.Window, cfg.Repo)
	}

	c := newGitCollector(cfg, owner, name, head, numbers, merges, reads, held, old != nil)
	if !cfg.GitOnly {
		reader := newGitHubReader(api, cfg.Token, cfg.Wait, cfg.Reserve, cfg.Rate)
		if err := c.github(ctx, reader, api.JoinPath("repos", owner, name)); err != nil {
			return GitCollected{}, err
		}
	}
	if !cfg.GitHubOnly {
		if err := c.git(ctx, clones); err != nil {
			return GitCollected{}, err
		}
	}
	if err := c.write(); err != nil {
		return GitCollected{}, err
	}
	collected := c.collected()
	collected.NoRepo, collected.PeakMemory, collected.Took = noRepo, c.guard.peak, time.Since(began)
	return collected, nil
}

// mergeStateReadOf is line, a line of a window's log, as the read of a merge state it is
// (MergeStateRead); nil for a line of another kind.
func mergeStateReadOf(line json.RawMessage) (*MergeStateRead, error) {
	var head struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(line, &head) != nil || head.Msg != mergeStateReadLine {
		return nil, nil
	}
	parsed, err := ParseLogLine(line)
	if err != nil {
		return nil, err
	}
	read := parsed.(MergeStateRead)
	return &read, nil
}

// headRef is a head by its pull request, and commitPair a pair of commits a reading of git's is
// held under: a head and its tip, a merge base and a head, or a head and a base commit.
type (
	headRef struct {
		pr  int
		sha string
	}
	commitPair [2]string
)

// gitCollector is one run of collect-git: what it read of the window, the input as it stands (cur),
// and what the window's git file held before the run (old), by what each reading of git's rests on.
type gitCollector struct {
	cfg         GitConfig
	owner, name string
	head        Window
	file        string
	numbers     []int
	// merges is the merges file's commits of the branch, in its order, mergeSet them by sha and
	// readBases each base commit a read names that the merges file does not hold, in the log's
	// order.
	merges    []Merge
	mergeSet  map[string]bool
	readBases []string
	reads     map[int][]MergeStateRead
	readCount int
	guard     *gitGuard

	cur, old  GitInput
	holdsFile bool
	// The readings the old file holds and those the run read, by what each rests on, and each head a
	// missing input of git's names.
	mergeBases map[commitPair]string
	headPaths  map[commitPair][]string
	verdicts   map[commitPair]GitVerdict
	oldMissing map[headRef]bool
}

// newGitCollector is the run of cfg over what the walk of its window read: the pull requests
// numbers, the merges of the branch, the reads of the repository's merge states, and the git file
// held, where holdsFile.
func newGitCollector(cfg GitConfig, owner, name string, head Window, numbers []int, merges []Merge, reads []MergeStateRead, held GitInput,
	holdsFile bool) *gitCollector {
	c := &gitCollector{cfg: cfg, owner: owner, name: name, head: head, file: path.Join(gitDir, owner+"__"+name+".jsonl"), numbers: numbers,
		merges: merges, mergeSet: make(map[string]bool), reads: make(map[int][]MergeStateRead), old: held, holdsFile: holdsFile,
		guard: &gitGuard{limit: cmp.Or(cfg.MemoryCap, DefaultGitMemoryCap), rss: cfg.rss}, mergeBases: make(map[commitPair]string),
		headPaths: make(map[commitPair][]string), verdicts: make(map[commitPair]GitVerdict), oldMissing: make(map[headRef]bool)}
	for _, merge := range merges {
		c.mergeSet[merge.SHA] = true
	}
	seen := make(map[string]bool)
	for _, r := range reads {
		if _, found := slices.BinarySearch(numbers, r.PullRequest); !found {
			continue
		}
		c.reads[r.PullRequest] = append(c.reads[r.PullRequest], r)
		c.readCount++
		if commitSHA.MatchString(r.BaseSHA) && !c.mergeSet[r.BaseSHA] && !seen[r.BaseSHA] {
			seen[r.BaseSHA] = true
			c.readBases = append(c.readBases, r.BaseSHA)
		}
	}
	for _, pull := range held.Pulls {
		if _, found := slices.BinarySearch(numbers, pull.Number); found {
			// The git phase writes its readings into the heads it holds, never into the file's.
			pull.Heads = slices.Clone(pull.Heads)
			c.cur.Pulls = append(c.cur.Pulls, pull)
		}
		for _, h := range pull.Heads {
			if !h.Read() {
				continue
			}
			c.mergeBases[commitPair{h.SHA, h.Tip}] = h.MergeBase
			c.headPaths[commitPair{h.MergeBase, h.SHA}] = h.Paths
			for _, verdict := range h.Verdicts {
				c.verdicts[commitPair{h.SHA, verdict.Base}] = verdict
			}
		}
	}
	c.cur.Commits = held.Commits
	for _, missing := range held.Missing {
		if _, found := slices.BinarySearch(numbers, missing.PR); found || missing.PR == 0 {
			c.cur.Missing = append(c.cur.Missing, missing)
		}
		if !githubReason(missing.Reason) && missing.Head != "" {
			c.oldMissing[headRef{missing.PR, missing.Head}] = true
		}
	}
	return c
}

// github is the GitHub phase: each pull request read from GitHub, through reader, from repository
// (its API URL), but each one the file holds (githubHeld), and the file written as each is
// finished.
func (c *gitCollector) github(ctx context.Context, reader githubReader, repository *url.URL) error {
	for _, number := range c.numbers {
		if c.githubHeld(number) {
			c.finished(GitPhaseGitHub, number, true)
			continue
		}
		pull, missing, err := c.readPull(ctx, reader, repository, number)
		if err != nil {
			return fmt.Errorf("replay: collect-git: %s#%d: %w", c.cfg.Repo, number, err)
		}
		c.dropMissing(func(m GitMissing) bool { return m.PR == number })
		c.cur.Missing = append(c.cur.Missing, missing...)
		c.cur.Pulls = slices.DeleteFunc(c.cur.Pulls, func(p GitPull) bool { return p.Number == number })
		if pull != nil {
			c.cur.Pulls = append(c.cur.Pulls, *pull)
			slices.SortFunc(c.cur.Pulls, func(a, b GitPull) int { return cmp.Compare(a.Number, b.Number) })
		}
		if err := c.write(); err != nil {
			return err
		}
		c.finished(GitPhaseGitHub, number, false)
	}
	return nil
}

// githubHeld says whether the git file holds the pull request number as the GitHub phase writes
// it: a pull line of it, and no missing input of GitHub's.
func (c *gitCollector) githubHeld(number int) bool {
	_, held := c.cur.Pull(number)
	return held && !slices.ContainsFunc(c.cur.Missing, func(m GitMissing) bool { return m.PR == number && githubReason(m.Reason) })
}

// readPull reads the pull request number of repository from GitHub: the pull request, whose merge
// it writes where it merged into the branch as a commit the merges file holds, and its timeline,
// whose heads it writes (selectHeads); with the missing inputs of what did not read. A pull
// request or a timeline GitHub answers 404 Not Found for is none, and a missing input, and so is a
// pull request GitHub still refuses to read after the reader's tries with its 422 of too many
// files changed (tooManyFiles), whose timeline is then not read; any other refusal is the read's
// failure.
func (c *gitCollector) readPull(ctx context.Context, reader githubReader, repository *url.URL, number int) (*GitPull, []GitMissing, error) {
	unread := func(reason, what string, err error) (*GitPull, []GitMissing, error) {
		return nil, []GitMissing{{PR: number, Reason: reason, Detail: fmt.Sprintf("%s of %s#%d: %v", what, c.cfg.Repo, number, err)}}, nil
	}
	var refusal *githubRefusal
	detail, err := reader.pullRequest(ctx, repository, number)
	switch {
	case errors.As(err, &refusal) && refusal.status == http.StatusNotFound:
		return unread(GitPullNotFound, "the pull request", err)
	case tooManyFiles(err):
		return unread(GitPullUnread, "the pull request", err)
	case err != nil:
		return nil, nil, fmt.Errorf("the pull request: %w", err)
	}
	if detail.Base.Ref == "" {
		return nil, nil, fmt.Errorf("the pull request names no base branch")
	}
	pull := GitPull{Number: number, Base: detail.Base.Ref}
	if detail.Merged && detail.Base.Ref == c.cfg.Branch && c.mergeSet[detail.MergeCommitSHA] {
		pull.Merge = detail.MergeCommitSHA
	}
	first := repository.JoinPath("issues", strconv.Itoa(number), "timeline")
	first.RawQuery = url.Values{"per_page": {"100"}}.Encode()
	pages, err := reader.pages(ctx, first)
	if errors.As(err, &refusal) && refusal.status == http.StatusNotFound {
		return unread(GitPullNotFound, "the timeline", err)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("the timeline: %w", err)
	}
	timeline, changes, missing := parseGitTimeline(number, pages)
	pull.BaseChanges = changes
	pull.Heads = c.selectHeads(number, timeline)
	return &pull, missing, nil
}

// timelineHead is a head a pull request's timeline gives: its commit, its time and where that
// comes from (GitHeadCommit, GitHeadForcePush), and the event's place in the timeline.
type timelineHead struct {
	sha   string
	at    time.Time
	from  string
	order int
}

// parseGitTimeline reads pages, the pages of the timeline of the pull request number as GitHub
// serves them, for its heads (timelineHead): each committed event's commit at its committer's time,
// and each head_ref_force_pushed event's new tip, its commit_id, at its created_at. A page that is
// no list of events, a committed event that gives no commit or no committer's time, and a force
// push that gives no time or names no commit, whose tip is never guessed, are each a
// timeline-unread missing input.
func parseGitTimeline(number int, pages [][]byte) ([]timelineHead, []time.Time, []GitMissing) {
	var heads []timelineHead
	changes := []time.Time{}
	var missing []GitMissing
	unread := func(format string, args ...any) {
		missing = append(missing, GitMissing{PR: number, Reason: GitTimelineUnread, Detail: fmt.Sprintf(format, args...)})
	}
	for i, page := range pages {
		var items []json.RawMessage
		if err := json.Unmarshal(page, &items); err != nil {
			unread("page %d of the timeline is no list of events: %v", i+1, err)
			continue
		}
		for _, item := range items {
			var event struct {
				Event string `json:"event"`
			}
			if err := json.Unmarshal(item, &event); err != nil {
				unread("an item of page %d of the timeline is no event: %v", i+1, err)
				continue
			}
			switch event.Event {
			case "committed":
				var committed struct {
					SHA       string `json:"sha"`
					Committer *struct {
						Date *time.Time `json:"date"`
					} `json:"committer"`
				}
				if err := json.Unmarshal(item, &committed); err != nil || !commitSHA.MatchString(committed.SHA) || committed.Committer == nil ||
					committed.Committer.Date == nil {
					unread("a committed event of the timeline gives no commit or no committer's time: %.200s", item)
					continue
				}
				heads = append(heads, timelineHead{sha: committed.SHA, at: committed.Committer.Date.UTC(), from: GitHeadCommit, order: len(heads)})
			case "base_ref_changed":
				var changed struct {
					At time.Time `json:"created_at"`
				}
				if err := json.Unmarshal(item, &changed); err != nil || changed.At.IsZero() {
					unread("a base_ref_changed event of the timeline gives no time: %.200s", item)
				} else {
					changes = append(changes, changed.At.UTC())
				}
			case "head_ref_force_pushed":
				var pushed struct {
					CommitID  *string    `json:"commit_id"`
					CreatedAt *time.Time `json:"created_at"`
				}
				switch err := json.Unmarshal(item, &pushed); {
				case err != nil || pushed.CreatedAt == nil:
					unread("a head_ref_force_pushed event of the timeline gives no time: %.200s", item)
				case pushed.CommitID == nil || !commitSHA.MatchString(*pushed.CommitID):
					unread("the force push at %s names no commit, so its new tip is not known", pushed.CreatedAt.UTC().Format(time.RFC3339))
				default:
					heads = append(heads, timelineHead{sha: *pushed.CommitID, at: pushed.CreatedAt.UTC(), from: GitHeadForcePush, order: len(heads)})
				}
			}
		}
	}
	slices.SortFunc(changes, time.Time.Compare)
	return heads, changes, missing
}

// selectHeads is the heads of the pull request number the git file writes, of those its timeline
// gives (CollectGit): those whose time falls in the window's inputs, the one standing at their
// start (laterHead), those a read names, and, from the reads, each head a read names that the
// timeline does not give, at the earliest read's time (MergeStateRead.ReadAt, else its line's
// time). Each is written once, at its earliest time, a force push before a commit of that time;
// by At, then SHA.
func (c *gitCollector) selectHeads(number int, timeline []timelineHead) []GitHead {
	reach, end := c.head.From.Add(-time.Duration(c.head.Before)), c.head.End()
	chosen, given := make(map[string]bool), make(map[string]bool)
	var standing *timelineHead
	for i := range timeline {
		h := &timeline[i]
		given[h.sha] = true
		if !h.at.Before(reach) && h.at.Before(end) {
			chosen[h.sha] = true
		}
		if !h.at.After(reach) && (standing == nil || laterHead(*h, *standing)) {
			standing = h
		}
	}
	if standing != nil {
		chosen[standing.sha] = true
	}
	read := make(map[string]time.Time)
	for _, r := range c.reads[number] {
		at := r.ReadAt
		if at.IsZero() {
			at = r.Time
		}
		for _, sha := range []string{r.HeadSHA, r.RecordedHead} {
			switch was, seen := read[sha]; {
			case !commitSHA.MatchString(sha):
			case given[sha]:
				chosen[sha] = true
			case !seen || at.Before(was):
				read[sha] = at
			}
		}
	}
	earliest := make(map[string]timelineHead)
	for _, h := range timeline {
		was, seen := earliest[h.sha]
		if chosen[h.sha] && (!seen || h.at.Before(was.at) || (h.at.Equal(was.at) && h.from == GitHeadForcePush)) {
			earliest[h.sha] = h
		}
	}
	heads := make([]GitHead, 0, len(earliest)+len(read))
	for sha, h := range earliest {
		heads = append(heads, GitHead{SHA: sha, At: h.at.UTC(), From: h.from})
	}
	for sha, at := range read {
		heads = append(heads, GitHead{SHA: sha, At: at.UTC(), From: GitHeadRead})
	}
	slices.SortFunc(heads, compareGitHeads)
	return heads
}

// laterHead says whether the timeline's head a stood after b, as the feasibility reading chose the
// head that stood at a moment: by time, then a force push after a commit of the same second, then
// the later event of the timeline.
func laterHead(a, b timelineHead) bool {
	switch {
	case !a.at.Equal(b.at):
		return a.at.After(b.at)
	case (a.from == GitHeadForcePush) != (b.from == GitHeadForcePush):
		return a.from == GitHeadForcePush
	}
	return a.order > b.order
}

// git is the git phase: the paths of each commit the file holds, then each pull request's heads
// read against their tip (readHead), the file written once the commits are read and again as each
// pull request is finished. It opens the clone, and asks git anything, only where the file does not
// hold all it writes.
func (c *gitCollector) git(ctx context.Context, clones string) error {
	needed := make([]string, 0, len(c.merges)+len(c.readBases))
	for _, merge := range c.merges {
		needed = append(needed, merge.SHA)
	}
	needed = append(needed, c.readBases...)
	heldPaths := make(map[string][]string, len(c.old.Commits))
	oldCommits := make(map[string]bool)
	for _, commit := range c.old.Commits {
		heldPaths[commit.SHA] = commit.Paths
		oldCommits[commit.SHA] = true
	}
	for _, missing := range c.old.Missing {
		if missing.PR == 0 {
			oldCommits[missing.Base] = true
		}
	}
	newCommits := make(map[string]bool, len(needed))
	for _, sha := range needed {
		newCommits[sha] = true
	}
	sameCommits := c.holdsFile && maps.Equal(oldCommits, newCommits)

	// Each pull request's tip where the file holds it: the merges file's last commit for one that
	// did not merge in the inputs, and the tip its heads were read against for one whose merge
	// is the one read then.
	last := c.merges[len(c.merges)-1].SHA
	tips := make(map[int]string)
	for _, pull := range c.cur.Pulls {
		if pull.Merge == "" {
			tips[pull.Number] = last
			continue
		}
		if was, held := c.old.Pull(pull.Number); held && was.Merge == pull.Merge {
			for _, h := range was.Heads {
				if h.Read() {
					tips[pull.Number] = h.Tip
					break
				}
			}
		}
	}
	whole := func(pr int, sha string) (GitHead, bool) {
		tip, known := tips[pr]
		was, held := c.old.Pull(pr)
		if !known || !held || !sameCommits || c.oldMissing[headRef{pr, sha}] {
			return GitHead{}, false
		}
		i := slices.IndexFunc(was.Heads, func(h GitHead) bool { return h.SHA == sha })
		if i < 0 || !was.Heads[i].Read() || was.Heads[i].Tip != tip {
			return GitHead{}, false
		}
		return was.Heads[i], true
	}

	var objects []string
	for _, sha := range needed {
		if _, held := heldPaths[sha]; !held {
			objects = append(objects, sha)
		}
	}
	heads := make(map[int][]string)
	for _, pull := range c.cur.Pulls {
		if _, known := tips[pull.Number]; !known {
			objects = append(objects, pull.Merge)
		}
		for _, h := range pull.Heads {
			if _, held := whole(pull.Number, h.SHA); !held {
				objects = append(objects, h.SHA)
				heads[pull.Number] = append(heads[pull.Number], h.SHA)
			}
		}
	}
	slices.Sort(objects)
	objects = slices.Compact(objects)
	var clone *gitClone
	unreachable := make(map[string]gitFailure)
	if len(objects) > 0 {
		var err error
		if clone, err = c.openClone(ctx, clones); err != nil {
			return err
		}
		if unreachable, err = clone.ensure(ctx, objects, heads); err != nil {
			return fmt.Errorf("replay: collect-git: %w", err)
		}
	}

	commits := make([]GitCommit, 0, len(needed))
	commitPaths := make(map[string][]string, len(needed))
	var missing []GitMissing
	for _, sha := range needed {
		paths, held := heldPaths[sha]
		if !held {
			if failure, lacking := unreachable[sha]; lacking {
				missing = append(missing, c.missingOf(failure, 0, "", sha, GitBaseUnreachable))
				continue
			}
			run := clone.git(ctx, gitOtherTime, nil, true, "diff-tree", "-r", "--no-commit-id", "--name-only", "-z", sha+"^1", sha)
			if paths, held = gitPaths(run); !held {
				missing = append(missing, c.missingOf(gitFailure{run, gitOtherTime, "git diff-tree of the commit and its first parent"}, 0, "", sha, GitError))
				continue
			}
		}
		commits = append(commits, GitCommit{SHA: sha, Paths: paths})
		commitPaths[sha] = paths
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.cur.Commits = commits
	c.dropMissing(func(m GitMissing) bool { return m.PR == 0 })
	c.cur.Missing = append(c.cur.Missing, missing...)
	if err := c.write(); err != nil {
		return err
	}

	for i := range c.cur.Pulls {
		pull := &c.cur.Pulls[i]
		number := pull.Number
		missing = nil
		tip, known := tips[number]
		if !known {
			var failure gitFailure
			if failure, known = unreachable[pull.Merge]; known {
				for _, h := range pull.Heads {
					missing = append(missing, c.missingOf(failure, number, h.SHA, pull.Merge, GitBaseUnreachable))
				}
				known = false
			} else {
				run := clone.git(ctx, gitOtherTime, nil, true, "rev-parse", "--verify", pull.Merge+"^1")
				tip = strings.TrimSpace(string(run.stdout))
				if known = run.ok() && commitSHA.MatchString(tip); !known {
					for _, h := range pull.Heads {
						missing = append(missing, c.missingOf(gitFailure{run, gitOtherTime, "git rev-parse of the merge's first parent"}, number, h.SHA,
							pull.Merge, GitError))
					}
				}
			}
		}
		for j := range pull.Heads {
			h := &pull.Heads[j]
			h.Tip, h.MergeBase, h.Paths, h.Verdicts = "", "", nil, nil
			if !known {
				continue
			}
			if was, held := whole(number, h.SHA); held {
				h.Tip, h.MergeBase, h.Paths, h.Verdicts = was.Tip, was.MergeBase, was.Paths, was.Verdicts
				continue
			}
			if failure, lacking := unreachable[h.SHA]; lacking {
				missing = append(missing, c.missingOf(failure, number, h.SHA, "", GitHeadUnreachable))
				continue
			}
			missing = append(missing, c.readHead(ctx, clone, number, h, tip, commitPaths)...)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		c.dropMissing(func(m GitMissing) bool { return m.PR == number && !githubReason(m.Reason) })
		c.cur.Missing = append(c.cur.Missing, missing...)
		if err := c.write(); err != nil {
			return err
		}
		c.finished(GitPhaseGit, number, false)
	}
	return nil
}

// readHead reads h, a head of the pull request pr, against tip with git: its merge base, the paths
// it changes since, and its verdict at each commit of the merges file on its tip's first-parent
// chain after its merge base whose paths (commitPaths) share one of its own, each the file holds or
// the run read already rather than read again, and each it reads kept for the run's other heads
// (readVerdicts). A merge base, paths or chain git does not give leave h unread; a verdict git does
// not give ends its verdicts. It is the missing inputs of what git did not give.
func (c *gitCollector) readHead(ctx context.Context, clone *gitClone, pr int, h *GitHead, tip string, commitPaths map[string][]string) []GitMissing {
	failed := func(run guardedRun, timeout time.Duration, base, what string) []GitMissing {
		return []GitMissing{c.missingOf(gitFailure{run, timeout, what}, pr, h.SHA, base, GitError)}
	}
	mergeBase, held := c.mergeBases[commitPair{h.SHA, tip}]
	if !held {
		run := clone.git(ctx, gitOtherTime, nil, true, "merge-base", h.SHA, tip)
		if mergeBase = strings.TrimSpace(string(run.stdout)); !run.ok() || !commitSHA.MatchString(mergeBase) {
			return failed(run, gitOtherTime, tip, "git merge-base of the head and its tip")
		}
		c.mergeBases[commitPair{h.SHA, tip}] = mergeBase
	}
	paths, held := c.headPaths[commitPair{mergeBase, h.SHA}]
	if !held {
		run := clone.git(ctx, gitOtherTime, nil, true, "diff", "--no-renames", "--name-only", "-z", mergeBase, h.SHA)
		if paths, held = gitPaths(run); !held {
			return failed(run, gitOtherTime, mergeBase, "git diff of the head since its merge base")
		}
		c.headPaths[commitPair{mergeBase, h.SHA}] = paths
	}
	run := clone.git(ctx, gitOtherTime, nil, true, "rev-list", "--first-parent", "--reverse", tip, "^"+h.SHA)
	chain := strings.Fields(string(run.stdout))
	if !run.ok() || slices.ContainsFunc(chain, func(sha string) bool { return !commitSHA.MatchString(sha) }) {
		return failed(run, gitOtherTime, tip, "git rev-list of the tip's first parents since the head")
	}
	// The base commits the head has a verdict at, in the chain's order, and those of them whose
	// verdict neither the file nor the run holds.
	var bases, unread []string
	for _, base := range chain {
		touched, known := commitPaths[base]
		if !c.mergeSet[base] || !known || !sharePath(touched, paths) {
			continue
		}
		bases = append(bases, base)
		if _, held := c.verdicts[commitPair{h.SHA, base}]; !held {
			unread = append(unread, base)
		}
	}
	missing := c.readVerdicts(ctx, clone, pr, h.SHA, unread)
	verdicts := []GitVerdict{}
	for _, base := range bases {
		verdict, held := c.verdicts[commitPair{h.SHA, base}]
		if !held {
			break
		}
		verdicts = append(verdicts, verdict)
	}
	h.Tip, h.MergeBase, h.Paths, h.Verdicts = tip, mergeBase, paths, verdicts
	return missing
}

// readVerdicts reads git's verdict of merging head with each base commit of bases, in order, into
// the run's verdicts: up to gitMergeBatch of them at a time from one git merge-tree --stdin, a line
// of its stdin for each (gitVerdicts). From the first base that command gives no verdict at, as
// where git cannot merge it or the guard kills git, it merges each alone (gitVerdict), up to the
// first git does not give, whose missing input it is. Each merge-tree leaves nothing in the clone
// (gitClone.merge).
func (c *gitCollector) readVerdicts(ctx context.Context, clone *gitClone, pr int, head string, bases []string) []GitMissing {
	for len(bases) > 0 {
		batch := bases[:min(gitMergeBatch, len(bases))]
		var stdin []byte
		for _, base := range batch {
			stdin = fmt.Appendf(stdin, "%s %s\n", base, head)
		}
		run := clone.merge(ctx, stdin, "--stdin", "--write-tree", "--no-messages", "--name-only", "-z")
		given := gitVerdicts(run.stdout, batch)
		for _, verdict := range given {
			c.verdicts[commitPair{head, verdict.Base}] = verdict
		}
		bases = bases[len(given):]
		if len(given) < len(batch) {
			break
		}
	}
	for _, base := range bases {
		run := clone.merge(ctx, nil, "--write-tree", "--no-messages", "--name-only", "-z", base, head)
		verdict, given := gitVerdict(run, base)
		if !given {
			return []GitMissing{c.missingOf(gitFailure{run, gitMergeTime, "git merge-tree of the base and the head"}, pr, head, base, GitError)}
		}
		c.verdicts[commitPair{head, base}] = verdict
	}
	return nil
}

// sharePath says whether the sorted lists a and b hold a path in common.
func sharePath(a, b []string) bool {
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			return true
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return false
}

// gitPaths is the paths a git command run with -z --name-only names, sorted, each once, and whether
// it gave them: whether it exited 0 by itself.
func gitPaths(run guardedRun) ([]string, bool) {
	if !run.ok() {
		return nil, false
	}
	paths := slices.DeleteFunc(strings.Split(string(run.stdout), "\x00"), func(p string) bool { return p == "" })
	slices.Sort(paths)
	return append([]string{}, slices.Compact(paths)...), true
}

// gitVerdict is git's verdict at base that run, a merge-tree --write-tree --name-only -z, gives,
// and whether it gives one: exit 0 with the merged tree is clean, exit 1 with the merged tree a
// conflict in the paths it names after it. Any other exit, or an output that begins with no tree,
// gives none: git also exits 1 where it cannot merge at all.
func gitVerdict(run guardedRun, base string) (GitVerdict, bool) {
	if run.err != nil || run.killed != "" || (run.code != 0 && run.code != 1) {
		return GitVerdict{}, false
	}
	parts := strings.Split(string(run.stdout), "\x00")
	if !commitSHA.MatchString(parts[0]) {
		return GitVerdict{}, false
	}
	paths := slices.DeleteFunc(parts[1:], func(p string) bool { return p == "" })
	if run.code == 0 && len(paths) > 0 {
		return GitVerdict{}, false
	}
	slices.Sort(paths)
	return GitVerdict{Base: base, Conflict: run.code == 1, Paths: append([]string{}, slices.Compact(paths)...)}, true
}

// gitVerdicts is git's verdict at each base commit of bases, in order, that stdout, what a
// merge-tree --stdin --write-tree --no-messages --name-only -z of a line of each with the head
// wrote, gives whole, however the command ended: a record of each merge, its status, 1 clean or 0 a
// conflict, its merged tree and the paths of its conflict, each ended by a NUL, and the record by
// one more. The first record that is not whole or not so, as where git stopped at a merge it could
// not make or the guard killed it, ends them.
func gitVerdicts(stdout []byte, bases []string) []GitVerdict {
	// Each field ended by a NUL; what follows the last NUL is no whole field.
	fields := strings.Split(string(stdout), "\x00")
	fields = fields[:len(fields)-1]
	var verdicts []GitVerdict
	for _, base := range bases {
		end := slices.Index(fields, "")
		if end < 2 || (fields[0] != "0" && fields[0] != "1") || !commitSHA.MatchString(fields[1]) || (fields[0] == "1" && end > 2) {
			break
		}
		paths := append([]string{}, fields[2:end]...)
		slices.Sort(paths)
		verdicts = append(verdicts, GitVerdict{Base: base, Conflict: fields[0] == "0", Paths: slices.Compact(paths)})
		fields = fields[end+1:]
	}
	return verdicts
}

// gitFailure is a git command that did not give what it was run for: the run, its time, and what it
// was run for.
type gitFailure struct {
	run     guardedRun
	timeout time.Duration
	what    string
}

// missingOf is the missing input failure leaves of the pull request pr, the head and the base
// commit: memory-cap or timeout where the guard killed the command, else reason, with what failed.
func (c *gitCollector) missingOf(failure gitFailure, pr int, head, base, reason string) GitMissing {
	run := failure.run
	detail := failure.what
	switch {
	case run.killed == GitMemoryCap:
		reason, detail = GitMemoryCap, fmt.Sprintf("%s: the guard killed git past the cap of %d bytes", detail, c.guard.limit)
	case run.killed == GitTimeout:
		reason, detail = GitTimeout, fmt.Sprintf("%s: the guard killed git after %s", detail, failure.timeout)
	case run.err != nil:
		detail = fmt.Sprintf("%s: %v", detail, run.err)
	default:
		detail = fmt.Sprintf("%s: git exits %d", detail, run.code)
		if said := stderrSummary(run.stderr); said != "" {
			detail += ": " + said
		}
	}
	return GitMissing{PR: pr, Head: head, Base: base, Reason: reason, Detail: detail}
}

// stderrSummary is what a missing input's detail says of git's own words: each line of text that
// holds more than white space, trimmed, once, in sorted order, joined with "; " and cut to 300
// bytes. It is no last line, because two processes write one stderr: for a commit the remote does
// not hold, `git fetch` and the `git upload-pack` it runs each write a `fatal:` line to it, in
// whichever order they happen to run, and two collections of the same inputs must write the same
// bytes.
func stderrSummary(text []byte) string {
	var lines []string
	for line := range bytes.Lines(text) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			lines = append(lines, string(line))
		}
	}
	slices.Sort(lines)
	summary := strings.Join(slices.Compact(lines), "; ")
	if len(summary) > 300 {
		summary = summary[:300]
	}
	return summary
}

// dropMissing takes each missing input drop says of out of the input as it stands.
func (c *gitCollector) dropMissing(drop func(GitMissing) bool) {
	c.cur.Missing = slices.DeleteFunc(c.cur.Missing, drop)
}

// write writes the input as it stands into the window, its missing inputs in the format's order,
// each once.
func (c *gitCollector) write() error {
	slices.SortFunc(c.cur.Missing, compareGitMissing)
	c.cur.Missing = slices.CompactFunc(c.cur.Missing, func(a, b GitMissing) bool { return compareGitMissing(a, b) == 0 })
	lines, err := c.cur.lines()
	if err == nil {
		err = replaceInput(c.cfg.Window, c.head, c.file, lines)
	}
	if err != nil {
		return fmt.Errorf("replay: collect-git: %w", err)
	}
	return nil
}

// finished tells cfg.Finished of the pull request number phase finished, or kept as the file held
// it.
func (c *gitCollector) finished(phase string, number int, kept bool) {
	if c.cfg.Finished == nil {
		return
	}
	done := GitPullFinished{Phase: phase, PR: number, Kept: kept}
	if pull, held := c.cur.Pull(number); held {
		done.Heads = len(pull.Heads)
		for _, h := range pull.Heads {
			done.Verdicts += len(h.Verdicts)
			for _, verdict := range h.Verdicts {
				if verdict.Conflict {
					done.Conflicts++
				}
			}
		}
	}
	for _, missing := range c.cur.Missing {
		if missing.PR == number {
			done.Missing++
		}
	}
	c.cfg.Finished(done)
}

// collected is what the input as it stands holds (GitCollected), but the run's own counts.
func (c *gitCollector) collected() GitCollected {
	out := GitCollected{PullRequests: len(c.cur.Pulls), Missing: make(map[string]int), Reads: c.readCount}
	for _, pull := range c.cur.Pulls {
		out.Heads += len(pull.Heads)
		for _, h := range pull.Heads {
			out.Verdicts += len(h.Verdicts)
			for _, verdict := range h.Verdicts {
				if verdict.Conflict {
					out.Conflicts++
				}
			}
		}
	}
	for _, missing := range c.cur.Missing {
		out.Missing[missing.Reason]++
	}
	return out
}

// gitClone is the bare clone of a repository the git phase reads, in dir, an absolute path, each
// command run under guard and told to onGit first.
type gitClone struct {
	dir   string
	guard *gitGuard
	onGit func(args []string)
}

// openClone is the clone of the repository in clones, which it clones first where clones holds
// none: git clone --no-checkout --bare --single-branch of the branch alone, every blob with it,
// into a temporary directory renamed into place once the clone is whole, so a clone the guard or
// the pod's release killed is never taken for one. A clone that fails fails the run.
func (c *gitCollector) openClone(ctx context.Context, clones string) (*gitClone, error) {
	// The merges name the clone's object directories to git by their paths (gitClone.merge), so its
	// directory is absolute.
	dir, err := filepath.Abs(filepath.Join(clones, c.owner+"__"+c.name))
	if err != nil {
		return nil, fmt.Errorf("replay: collect-git: %w", err)
	}
	clone := &gitClone{dir: dir, guard: c.guard, onGit: c.cfg.onGit}
	switch _, err := os.Stat(dir); {
	case err == nil:
		return clone, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("replay: collect-git: %w", err)
	}
	if err := os.MkdirAll(clones, 0o755); err != nil {
		return nil, fmt.Errorf("replay: collect-git: %w", err)
	}
	temp, err := os.MkdirTemp(clones, c.owner+"__"+c.name+".clone-")
	if err != nil {
		return nil, fmt.Errorf("replay: collect-git: %w", err)
	}
	remote := cmp.Or(c.cfg.remote, "https://github.com/"+c.owner+"/"+c.name+".git")
	run := clone.run(ctx, "", gitCloneTime, nil, false, "clone", "--quiet", "--no-checkout", "--bare", "--single-branch", "--no-tags", "--branch",
		c.cfg.Branch, remote, temp)
	if !run.ok() {
		os.RemoveAll(temp)
		failure := c.missingOf(gitFailure{run, gitCloneTime, "git clone of " + c.cfg.Branch}, 0, "", "", GitError)
		return nil, fmt.Errorf("replay: collect-git: %s (%s)", failure.Detail, failure.Reason)
	}
	if err := os.Rename(temp, dir); err != nil {
		os.RemoveAll(temp)
		return nil, fmt.Errorf("replay: collect-git: %w", err)
	}
	return clone, nil
}

// git runs git args in the clone (git -C <dir>), under the guard and its time timeout, with stdin
// where it is not nil, and lazy fetching off where noLazy.
func (r *gitClone) git(ctx context.Context, timeout time.Duration, stdin []byte, noLazy bool, args ...string) guardedRun {
	return r.run(ctx, r.dir, timeout, stdin, noLazy, args...)
}

// run runs git args in dir ("" for none), as command makes them, under the guard and its time
// timeout of the whole run.
func (r *gitClone) run(ctx context.Context, dir string, timeout time.Duration, stdin []byte, noLazy bool, args ...string) guardedRun {
	return r.guard.run(ctx, r.command(dir, stdin, noLazy, args...), timeout, false)
}

// command is git args in dir ("" for none), told to onGit first, as git describes: never asking for
// a credential on a terminal, never collecting garbage or running maintenance on its own, and in
// the C locale; with stdin where it is not nil, and lazy fetching off where noLazy.
func (r *gitClone) command(dir string, stdin []byte, noLazy bool, args ...string) *exec.Cmd {
	if r.onGit != nil {
		r.onGit(slices.Clone(args))
	}
	full := []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	if dir != "" {
		full = append(full, "-C", dir)
	}
	cmd := exec.Command("git", append(full, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if noLazy {
		cmd.Env = append(cmd.Env, "GIT_NO_LAZY_FETCH=1")
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd
}

// merge runs git merge-tree args in the clone, with stdin where it is not nil and lazy fetching
// off, and leaves nothing of it in the clone: GIT_OBJECT_DIRECTORY names the directory of the
// clone's gitMergeObjects, which git writes the merged trees and blobs into, and
// GIT_ALTERNATE_OBJECT_DIRECTORIES the clone's objects, which it reads the commits from. That
// directory is made empty before the command, whatever a stopped run left there, and removed once
// the command ends, however it ended; an error of either is the run's (guardedRun.err), as one of
// starting git is. No other command gets the two variables. The guard's time is gitMergeTime, that
// of one merge: a merge-tree --stdin, whose stdin is not nil, writes the record of each merge as
// it makes it, so its time is counted from the last output (gitGuard.run's sinceOutput).
func (r *gitClone) merge(ctx context.Context, stdin []byte, args ...string) guardedRun {
	objects := filepath.Join(r.dir, gitMergeObjects)
	if err := os.RemoveAll(objects); err != nil {
		return guardedRun{code: -1, err: err}
	}
	if err := os.Mkdir(objects, 0o755); err != nil {
		return guardedRun{code: -1, err: err}
	}
	cmd := r.command(r.dir, stdin, true, append([]string{"merge-tree"}, args...)...)
	cmd.Env = append(cmd.Env, "GIT_OBJECT_DIRECTORY="+objects, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(r.dir, "objects"))
	run := r.guard.run(ctx, cmd, gitMergeTime, stdin != nil)
	if err := os.RemoveAll(objects); err != nil && run.err == nil {
		run.err = err
	}
	return run
}

// ensure has the clone hold each commit of shas, fetching each it lacks (present): first the head
// ref of each pull request of heads, each pull request's heads by its number, that holds a head
// the clone lacks (fetchPulls); then each commit still lacking, in batches of up to gitFetchBatch
// (fetchCommits). Each fetch writes a ref of what it fetched, so each later fetch tells the remote
// the clone holds it, and the remote sends no commit or tree twice. It is each commit the clone
// still lacks, with the fetch of it alone that failed. A check of what the clone holds that fails
// fails the run.
func (r *gitClone) ensure(ctx context.Context, shas []string, heads map[int][]string) (map[string]gitFailure, error) {
	held, err := r.present(ctx, shas)
	if err != nil {
		return nil, err
	}
	lacking := slices.DeleteFunc(slices.Clone(shas), func(sha string) bool { return held[sha] })
	var numbers []int
	for number, its := range heads {
		if slices.ContainsFunc(its, func(sha string) bool { return !held[sha] }) {
			numbers = append(numbers, number)
		}
	}
	slices.Sort(numbers)
	if r.fetchPulls(ctx, numbers) {
		if held, err = r.present(ctx, lacking); err != nil {
			return nil, err
		}
		lacking = slices.DeleteFunc(lacking, func(sha string) bool { return held[sha] })
	}
	failures := make(map[string]gitFailure)
	for start := 0; start < len(lacking); start += gitFetchBatch {
		if err := r.fetchCommits(ctx, lacking[start:min(start+gitFetchBatch, len(lacking))], failures); err != nil {
			return nil, err
		}
	}
	return failures, nil
}

// fetchPulls fetches the head ref of each pull request of numbers the remote holds one of,
// refs/pull/<n>/head, into refs/pulls/<n>, moved wherever it stood, in batches of up to
// gitFetchBatch, and says whether it fetched any. It asks the remote first which of those refs it
// holds (git ls-remote, gitListBatch at a time), so a pull request whose ref the remote does not
// hold, as on a remote that holds no pull request's ref, fails no fetch. A list or a fetch that
// fails leaves the commits to fetchCommits.
func (r *gitClone) fetchPulls(ctx context.Context, numbers []int) bool {
	ref := func(number int) string { return "refs/pull/" + strconv.Itoa(number) + "/head" }
	var held []int
	for start := 0; start < len(numbers); start += gitListBatch {
		batch := numbers[start:min(start+gitListBatch, len(numbers))]
		args := []string{"ls-remote", "origin"}
		for _, number := range batch {
			args = append(args, ref(number))
		}
		run := r.git(ctx, gitOtherTime, nil, false, args...)
		if !run.ok() {
			continue
		}
		listed := make(map[string]bool)
		for line := range strings.Lines(string(run.stdout)) {
			if sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && commitSHA.MatchString(sha) {
				listed[name] = true
			}
		}
		for _, number := range batch {
			if listed[ref(number)] {
				held = append(held, number)
			}
		}
	}
	for start := 0; start < len(held); start += gitFetchBatch {
		var refspecs []string
		for _, number := range held[start:min(start+gitFetchBatch, len(held))] {
			refspecs = append(refspecs, "+"+ref(number)+":refs/pulls/"+strconv.Itoa(number))
		}
		r.fetch(ctx, gitBranchTime, refspecs)
	}
	return len(held) > 0
}

// fetchCommits fetches each commit of batch into a ref of its own, refs/fetched/<sha>, and, where
// the clone still lacks some of them, the two halves of those it lacks in turn, down to a commit
// alone, since a fetch that names one commit the remote does not hold fetches nothing. Each commit
// the clone still lacks once fetched alone is a failure of failures, with that fetch.
func (r *gitClone) fetchCommits(ctx context.Context, batch []string, failures map[string]gitFailure) error {
	timeout := gitBranchTime
	if len(batch) == 1 {
		timeout = gitFetchTime
	}
	refspecs := make([]string, len(batch))
	for i, sha := range batch {
		refspecs[i] = sha + ":refs/fetched/" + sha
	}
	run := r.fetch(ctx, timeout, refspecs)
	held, err := r.present(ctx, batch)
	if err != nil {
		return err
	}
	lacking := slices.DeleteFunc(slices.Clone(batch), func(sha string) bool { return held[sha] })
	if len(lacking) > 0 && len(batch) == 1 {
		failures[batch[0]] = gitFailure{run, gitFetchTime, "git fetch of the commit alone"}
		return nil
	}
	for _, half := range [][]string{lacking[:len(lacking)/2], lacking[len(lacking)/2:]} {
		if len(half) == 0 {
			continue
		}
		if err := r.fetchCommits(ctx, half, failures); err != nil {
			return err
		}
	}
	return nil
}

// fetch runs git fetch of refspecs from the remote, with no tags, the blobs of what it fetches with
// it, under the guard and its time timeout.
func (r *gitClone) fetch(ctx context.Context, timeout time.Duration, refspecs []string) guardedRun {
	return r.git(ctx, timeout, nil, false, append([]string{"fetch", "--quiet", "--no-tags", "origin"}, refspecs...)...)
}

// present is each commit of shas the clone holds, asked with lazy fetching off, so asking fetches
// nothing (GIT_NO_LAZY_FETCH=1 git cat-file --batch-check).
func (r *gitClone) present(ctx context.Context, shas []string) (map[string]bool, error) {
	run := r.git(ctx, gitOtherTime, []byte(strings.Join(shas, "\n")+"\n"), true, "cat-file", "--batch-check")
	lines := strings.Split(strings.TrimSuffix(string(run.stdout), "\n"), "\n")
	if !run.ok() || len(lines) != len(shas) {
		return nil, fmt.Errorf("git cat-file --batch-check of %d commits in %s: exit %d, %s, %d lines: %s", len(shas), r.dir, run.code,
			cmp.Or(run.killed, "not killed"), len(lines), stderrSummary(run.stderr))
	}
	held := make(map[string]bool, len(shas))
	for i, line := range lines {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == shas[i] && fields[1] == "commit" {
			held[shas[i]] = true
		}
	}
	return held, nil
}

// ok says whether the command exited 0 by itself.
func (run guardedRun) ok() bool {
	return run.err == nil && run.killed == "" && run.code == 0
}
