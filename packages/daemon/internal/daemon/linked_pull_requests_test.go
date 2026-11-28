package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The runtime-level tests of the daemon's work on the pull requests an issue links in Dispatch
// (dispatch://INFRA-5910 D2, D6): a hand-built runtime (linkedRig) over a Postgres of its own, a
// fake GitHub of two repositories, acme/widgets and acme/deploy (linkedGitHub), and a fake
// Dispatch that holds each issue's external links and answers its webhook App check
// (linkDetails). The fake GitHub records every request it was asked, so a test reads which
// repository a read of one pull request was made in. The whole-daemon tests of D3, D4 and D10 are
// in linked_pull_requests_live_test.go.

// linkedDeploy is the second repository of the tests that link a pull request of another
// repository than the issue's own.
const linkedDeploy = "acme/deploy"

// linkedPull is one pull request of linkedGitHub: its repository, number, head branch, base, head
// commit and state; merged says GitHub merged it; conflicting that it conflicts with its base.
type linkedPull struct {
	repo, branch, base, head, state string
	number                          int
	merged, conflicting             bool
}

// linkedGitHub is GitHub's REST and GraphQL APIs over the pull requests it holds, found by
// repository, compared without case, and number.
type linkedGitHub struct {
	t     *testing.T
	url   string
	mu    sync.Mutex
	pulls []linkedPull
	seen  []string
}

func newLinkedGitHub(t *testing.T, pulls ...linkedPull) *linkedGitHub {
	t.Helper()
	g := &linkedGitHub{t: t, pulls: pulls}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.seen = append(g.seen, r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		g.serve(w, r)
	}})
	return g
}

// find is the pull request number of repo, nil for none.
func (g *linkedGitHub) find(repo string, number int) *linkedPull {
	g.mu.Lock()
	defer g.mu.Unlock()
	for at := range g.pulls {
		if strings.EqualFold(g.pulls[at].repo, repo) && g.pulls[at].number == number {
			return &g.pulls[at]
		}
	}
	return nil
}

// asked is each request GitHub was asked, "<method> <path>", in order.
func (g *linkedGitHub) asked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.seen)
}

// askedAbout is the requests GitHub was asked whose path holds part.
func (g *linkedGitHub) askedAbout(part string) []string {
	var about []string
	for _, ask := range g.asked() {
		if strings.Contains(ask, part) {
			about = append(about, ask)
		}
	}
	return about
}

func (g *linkedGitHub) pullAnswer(p linkedPull) map[string]any {
	mergeable, state := true, "clean"
	if p.conflicting {
		mergeable, state = false, "dirty"
	}
	answer := map[string]any{"number": p.number, "state": p.state, "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", p.repo, p.number),
		"body": "", "base": map[string]any{"ref": p.base, "repo": map[string]any{"full_name": p.repo}},
		"head":       map[string]any{"ref": p.branch, "sha": p.head, "repo": map[string]any{"full_name": p.repo}},
		"updated_at": "2026-11-27T00:00:00Z", "mergeable": mergeable, "mergeable_state": state, "merged": p.merged,
		"requested_reviewers": []any{}, "requested_teams": []any{}}
	if p.merged {
		answer["merge_commit_sha"] = "merge-" + p.head
		answer["mergeable_state"] = "unknown"
	}
	return answer
}

func (g *linkedGitHub) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
		var request batchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			g.t.Errorf("decode the GraphQL request: %v", err)
			return
		}
		var numbers []int
		var nodes []map[string]any
		for _, match := range askedAlias.FindAllStringSubmatch(request.Query, -1) {
			var number int
			_, _ = fmt.Sscanf(match[2], "%d", &number)
			numbers = append(numbers, number)
			if pull := g.find(request.Variables.Owner+"/"+request.Variables.Name, number); pull != nil {
				node := batchNode(number, pull.head, "MERGEABLE", "CLEAN")
				node["headRefName"], node["baseRefName"] = pull.branch, pull.base
				nodes = append(nodes, node)
			}
		}
		g.mu.Lock()
		g.seen = append(g.seen, fmt.Sprintf("graphql %s/%s %v", request.Variables.Owner, request.Variables.Name, numbers))
		g.mu.Unlock()
		_, _ = w.Write([]byte(batchAnswer(g.t, numbers, nodes...)))
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/repos/") || len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	repo, rest := parts[0]+"/"+parts[1], strings.Join(parts[2:], "/")
	switch {
	case rest == "":
		reply(map[string]any{"full_name": repo, "default_branch": "main"})
	case rest == "pulls":
		var listed []map[string]any
		head := strings.TrimPrefix(r.URL.Query().Get("head"), parts[0]+":")
		g.mu.Lock()
		for _, pull := range g.pulls {
			if strings.EqualFold(pull.repo, repo) && pull.branch == head && pull.state == "open" {
				listed = append(listed, g.pullAnswer(pull))
			}
		}
		g.mu.Unlock()
		reply(listed)
	case strings.HasPrefix(rest, "pulls/"):
		var number int
		var tail string
		_, _ = fmt.Sscanf(strings.TrimPrefix(rest, "pulls/"), "%d", &number)
		if _, after, found := strings.Cut(strings.TrimPrefix(rest, "pulls/"), "/"); found {
			tail = after
		}
		pull := g.find(repo, number)
		switch {
		case pull == nil:
			http.NotFound(w, r)
		case tail == "reviews":
			reply([]any{})
		case strings.HasPrefix(tail, "reviews/"):
			reply(map[string]any{"body": "the body GitHub holds"})
		case tail == "":
			g.mu.Lock()
			defer g.mu.Unlock()
			reply(g.pullAnswer(*pull))
		default:
			http.NotFound(w, r)
		}
	case strings.HasPrefix(rest, "collaborators/"):
		reply(map[string]any{"permission": "write", "user": map[string]any{"login": strings.Split(rest, "/")[1]}})
	default:
		http.NotFound(w, r)
	}
}

// linkDetails is Dispatch's detail of each issue it holds, with its external links, and its
// answer that its webhook App is installed on every repository.
type linkDetails struct {
	url   string
	mu    sync.Mutex
	links map[string][]string
}

func newLinkDetails(t *testing.T) *linkDetails {
	t.Helper()
	d := &linkDetails{links: map[string][]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/github/app-installation/"):
			_ = json.NewEncoder(w).Encode(map[string]bool{"installed": true})
		case strings.HasPrefix(r.URL.Path, "/api/v1/issues/"):
			key := strings.TrimPrefix(r.URL.Path, "/api/v1/issues/")
			links := []map[string]string{}
			for _, url := range d.links[key] {
				links = append(links, map[string]string{"url": url})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "project": "CAPTURE", "title": "root", "status": "in_progress", "rank": "U",
				"labels": []string{"legion"}, "last_seq": 7, "resolved_repo": "acme/widgets", "blocked_by": []string{}, "claim": nil, "external_links": links})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	d.url = server.URL
	return d
}

// linkedRig is the runtime of the tests, over pool, with the engine that applies every fact it
// reads, the repositories GitHub intake is told to hear (heard), and its logs.
type linkedRig struct {
	w        *workflowRuntime
	pool     *store.Pool
	github   *linkedGitHub
	dispatch *linkDetails
	logs     *syncBuffer
	mu       sync.Mutex
	heard    [][]ghrepo.Repository
}

func newLinkedRig(t *testing.T, pool *store.Pool, github *linkedGitHub) *linkedRig {
	t.Helper()
	rig := &linkedRig{pool: pool, github: github, dispatch: newLinkDetails(t), logs: &syncBuffer{}}
	log := slog.New(slog.NewJSONHandler(rig.logs, nil))
	repos := newRepositories("CAPTURE", ghrepo.Renames{})
	w := &workflowRuntime{
		pool: pool, records: projectRecords{Store: record.NewStore(), project: "CAPTURE"},
		tokens: roleTokens{}, apps: &workflowTokenRecorder{}, installs: &workflowTokenRecorder{},
		dispatch:        dispatch.New(rig.dispatch.url, dispatch.BearerTransport("test", nil)),
		repositories:    repos,
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log, githubAPI: github.url,
		mergeStates: newMergeStateSet(), mergeStateRunning: map[pullRead]*mergeStateRun{}, baseWake: newWakeSet[string](),
		etags: githubrest.NewETags(etagBytes),
	}
	w.defaultBranches = newDefaultBranches(w.tokens, repos, github.url, time.Hour, time.Now)
	w.setRepositories = func(_ context.Context, heard []ghrepo.Repository) error {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.heard = append(rig.heard, slices.Clone(heard))
		return nil
	}
	w.engine = workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories,
		ReadMergeState: w.queueMergeStateOf, ReadBaseMergeStates: w.queueBaseMergeStates,
		LinksTaken: w.linksTaken, HearRepository: w.hearRecorded}, log)
	w.handlers = []intake.Handler{w.engine}
	rig.w = w
	return rig
}

// told is the last set of repositories GitHub intake was told to hear.
func (r *linkedRig) told() []ghrepo.Repository {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.heard) == 0 {
		return nil
	}
	return r.heard[len(r.heard)-1]
}

// outbox is the outbox's executors over the rig's runtime, with the reads of GitHub the runtime
// hands it (attach).
func (r *linkedRig) outbox() *outbox {
	return &outbox{log: slog.New(slog.NewJSONHandler(r.logs, nil)), pool: r.pool, records: r.w.records, dispatchProject: "CAPTURE", tokens: roleTokens{},
		githubAPI: r.github.url, repositories: r.w.repositories, handlers: r.w.handlers, now: time.Now,
		readPullRequest: r.w.readPullRequest, applyMergeability: r.w.applyMergeability, mergeStateWait: r.w.mergeStateWait,
		queueMergeStateFollow: r.w.queueMergeStateFollow, readLinkedPullRequest: r.w.readLinkedPullRequest, linkHead: r.w.setLinkHead}
}

// linkedRow is the record's row of pull request number of repo, nil for none.
func linkedRow(t *testing.T, pool *store.Pool, repo string, number int) *record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pr, err = record.NewStore().PullRequestByNumber(ctx, tx, repo, number)
		return err
	}); err != nil {
		t.Fatalf("read %s#%d: %v", repo, number, err)
	}
	return pr
}

// linkedIssue is the record of key.
func linkedIssue(t *testing.T, pool *store.Pool, key string) record.Issue {
	t.Helper()
	var issue *record.Issue
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(ctx, tx, key)
		return err
	}); err != nil || issue == nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return *issue
}

// seedLinks writes the links of CAPTURE-1 to the pull requests named (repository and number), and a
// row of each that holds a branch, as the workflow records a linked pull request (record.Store).
func seedLinks(t *testing.T, pool *store.Pool, issue string, links []record.PullRequestLink, rows ...record.PullRequest) {
	t.Helper()
	records := record.NewStore()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.ReplacePullRequestLinks(ctx, tx, issue, links); err != nil {
			return err
		}
		for _, row := range rows {
			if err := records.PutPullRequest(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the links of %s: %v", issue, err)
	}
}

// linkedPullRow is a row of pull request number of repo, open, that issue links.
func linkedPullRow(issue, repo string, number int, branch, head string) record.PullRequest {
	return record.PullRequest{State: record.PullRequestOpen, Issue: issue, Repo: repo, Number: number, Branch: branch, Base: "main", HeadSHA: head, Failing: []string{}}
}

// Each read of an issue's pull requests the daemon makes is made in the repository of the pull
// request it reads, whichever repository the issue's own is (dispatch://INFRA-5910 D6, "the
// daemon's reads in the pull request's own repository"). CAPTURE-1, in merging, has its own pull
// request acme/widgets#86 and links acme/deploy#86, a pull request of the same number in the second
// repository, so a read that names the number in the other repository answers the wrong pull
// request. One row each for the merge state (readPullRequest, the ladder, the batch with its head
// rollup), the review reads (readReviews, reviewBody, readPermission), the merge-queue read and its
// message, the conflict's second read and its base's rules, the link read, the close of a task
// pull request (in the issue's repository alone), the repository a live link makes the daemon hear
// in the same pass, and a link to a repository whose Legion App is not installed (refused as
// unserved, one notice, the close held). Every row reads what GitHub was asked, and fails on a
// request that names acme/widgets for the linked pull request.
func TestTheDaemonReadsEachPullRequestInItsOwnRepository(t *testing.T) {
	t.Parallel()
	seed := func(t *testing.T, github *linkedGitHub) *linkedRig {
		t.Helper()
		pool := isolatedOutboxPool(t)
		seedCapture(t, pool, phase.Merging, "needs_review")
		seedLinks(t, pool, "CAPTURE-1", []record.PullRequestLink{{Issue: "CAPTURE-1", Repo: linkedDeploy, Number: 86}},
			linkedPullRow("CAPTURE-1", linkedDeploy, 86, "fix/deploy", "linked-head"))
		return newLinkedRig(t, pool, github)
	}
	pulls := func() []linkedPull {
		return []linkedPull{{repo: "acme/widgets", number: 86, branch: "legion/CAPTURE-1", base: "main", head: "own-head", state: "open"},
			{repo: linkedDeploy, number: 86, branch: "fix/deploy", base: "main", head: "linked-head", state: "open"}}
	}
	notInWidgets := func(t *testing.T, github *linkedGitHub, what string) {
		t.Helper()
		if asked := github.askedAbout("/repos/acme/widgets/"); len(asked) != 0 {
			t.Errorf("%s asked GitHub %q about acme/widgets, want it to read the linked pull request in acme/deploy alone", what, asked)
		}
		if len(github.askedAbout("deploy")) == 0 {
			t.Errorf("%s asked GitHub nothing about acme/deploy", what)
		}
	}

	t.Run("the merge state: readPullRequest and the ladder", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		linked := linkedRow(t, rig.pool, linkedDeploy, 86)
		if _, pull, err := rig.w.readPullRequest(context.Background(), *linked); err != nil || pull.Head.SHA != "linked-head" {
			t.Fatalf("readPullRequest of the linked pull request = %q, %v; want acme/deploy#86 at linked-head", pull.Head.SHA, err)
		}
		if state, _, _, err := rig.w.readMergeState(context.Background(), pullReadOf(linkedDeploy, 86)); err != nil || state != record.MergeStateClean {
			t.Fatalf("the ladder's read of the linked pull request = %q, %v; want clean", state, err)
		}
		notInWidgets(t, github, "the merge state's reads")
		if own := linkedRow(t, rig.pool, "acme/widgets", 86); !own.StateReadAt.IsZero() {
			t.Errorf("the own pull request acme/widgets#86 was read at %v, want only the linked one read", own.StateReadAt)
		}
		if got := linkedRow(t, rig.pool, linkedDeploy, 86); got.StateReadAt.IsZero() {
			t.Errorf("the linked pull request's record holds no moment of its read: the ladder recorded GitHub's answer on another row")
		}
	})

	t.Run("the batch and its head rollup", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go rig.w.readMergeStates(ctx)
		rig.w.queueBaseRead(ghrepo.MustParse(linkedDeploy), "main", map[pullRead]string{pullReadOf(linkedDeploy, 86): ""}, true)
		testwait.Eventually(t, "the batch's query of acme/deploy", func() bool { return len(github.askedAbout("graphql acme/deploy")) > 0 })
		if asked := github.askedAbout("graphql acme/deploy"); !slices.Contains(asked, "graphql acme/deploy [86]") {
			t.Errorf("the batch asked %q, want the query of acme/deploy's pull request 86", asked)
		}
		if asked := github.askedAbout("graphql acme/widgets"); len(asked) != 0 {
			t.Errorf("the batch of acme/deploy asked %q of acme/widgets", asked)
		}
		testwait.Eventually(t, "the linked pull request's merge state recorded", func() bool {
			return !linkedRow(t, rig.pool, linkedDeploy, 86).StateReadAt.IsZero()
		})
	})

	t.Run("the review reads: readReviews, readPermission and reviewBody", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		ctx := context.Background()
		linked := linkedRow(t, rig.pool, linkedDeploy, 86)
		client, _, err := rig.w.readPullRequest(ctx, *linked)
		if err != nil {
			t.Fatalf("read the linked pull request: %v", err)
		}
		rig.w.readReviews(ctx, ctx, client, *linked)
		if can, err := rig.w.readPermission(ctx, ghrepo.MustParse(linkedDeploy), "a-person"); err != nil || !can {
			t.Fatalf("readPermission = %t, %v; want write access", can, err)
		}
		rig.w.reviewAppLogin = "review[bot]"
		body, err := rig.w.reviewBody(ctx, intake.PullRequestReview{Repo: linkedDeploy, Number: 86, ID: 7, Author: "review[bot]", Body: "cut"})
		if err != nil || body != "the body GitHub holds" {
			t.Fatalf("reviewBody = %q, %v; want the body GitHub holds for review 7 of acme/deploy#86", body, err)
		}
		for _, want := range []string{"GET /repos/acme/deploy/pulls/86/reviews", "GET /repos/acme/deploy/collaborators/a-person/permission", "GET /repos/acme/deploy/pulls/86/reviews/7"} {
			if !slices.Contains(github.asked(), want) {
				t.Errorf("GitHub was asked %q, want %q among them", github.asked(), want)
			}
		}
		notInWidgets(t, github, "the review reads")
	})

	t.Run("the merge-queue read and its message", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		row := record.OutboxRow{ID: 1, Issue: "CAPTURE-1"}
		if err := rig.outbox().mergeQueueRead(context.Background(), row, record.MergeQueueRead{Repo: linkedDeploy, Number: 86, Head: "linked-head"}); err != nil {
			t.Fatalf("the merge-queue read of the linked pull request: %v", err)
		}
		notInWidgets(t, github, "the merge-queue read")
		var payload []byte
		if err := rig.pool.QueryRow(context.Background(), `select payload from outbox where issue = 'CAPTURE-1' and kind = $1`, string(record.OutboxKindMergeQueueRemoval)).Scan(&payload); err != nil {
			t.Fatalf("the read queued no merge-queue removal: %v", err)
		}
		var removal record.MergeQueueRemoval
		if err := json.Unmarshal(payload, &removal); err != nil || removal.Repo != linkedDeploy || removal.Number != 86 {
			t.Fatalf("the removal = %+v, %v; want it to name acme/deploy#86", removal, err)
		}
		if message := removal.Message("CAPTURE-1", removal.RepoOf("acme/widgets")); !strings.Contains(message, "acme/deploy#86") {
			t.Errorf("the merger's message %q does not name the linked pull request as acme/deploy#86", message)
		}
	})

	t.Run("the conflict's second read and its base's rules", func(t *testing.T) {
		t.Parallel()
		conflicting := pulls()
		conflicting[1].conflicting = true
		github := newLinkedGitHub(t, conflicting...)
		rig := seed(t, github)
		first := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
		linked := linkedPullRow("CAPTURE-1", linkedDeploy, 86, "fix/deploy", "linked-head")
		linked.ConflictRead = &record.ConflictRead{Head: "linked-head", Base: "main", At: first}
		if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error { return record.NewStore().PutPullRequest(ctx, tx, linked) }); err != nil {
			t.Fatalf("record the first read: %v", err)
		}
		row := record.OutboxRow{ID: 2, Issue: "CAPTURE-1"}
		if err := rig.outbox().conflictConfirm(context.Background(), row, record.ConflictConfirm{Repo: linkedDeploy, Number: 86, Head: "linked-head", FirstReadAt: first}); err != nil {
			t.Fatalf("the second read of the linked pull request: %v", err)
		}
		if rules := github.askedAbout("/repos/acme/deploy/rules/branches/main"); len(rules) == 0 {
			t.Errorf("GitHub was asked %q, want the rules of acme/deploy's main among them", github.asked())
		}
		notInWidgets(t, github, "the second read")
	})

	t.Run("the link read", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		read, err := rig.w.readLinkedPullRequest(context.Background(), "CAPTURE-1", linkedDeploy, 86)
		if err != nil || read.fact.State != record.PullRequestOpen || read.fact.HeadSHA != "linked-head" || read.fact.HeadRepo != linkedDeploy {
			t.Fatalf("the link read = %+v, %v; want acme/deploy#86 open at linked-head", read.fact, err)
		}
		notInWidgets(t, github, "the link read")
	})

	t.Run("a close of a task pull request, in the issue's repository alone", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		closing := newClosingGitHub(map[int]string{86: "open"})
		o := rig.outbox()
		o.githubAPI = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			if !closing.handle(w, r) {
				github.serve(w, r)
			}
		}})
		row := record.OutboxRow{ID: 3, Issue: "CAPTURE-1"}
		if err := o.closeTaskPullRequests(context.Background(), row, record.TaskPullRequestsClose{Generation: 1, Repo: "acme/widgets", Numbers: []int{86}, Comment: "Merged in #2."}); err != nil {
			t.Fatalf("the close of the task pull request: %v", err)
		}
		if state, _ := closing.pull(86); state != "closed" {
			t.Errorf("acme/widgets#86 is %s after the close of the issue's pull request 86, want closed", state)
		}
		if pull := github.find(linkedDeploy, 86); pull.state != "open" {
			t.Errorf("acme/deploy#86 is %s, want the linked pull request left open", pull.state)
		}
		if asked := github.askedAbout("deploy"); len(asked) != 0 {
			t.Errorf("the close asked GitHub %q about acme/deploy", asked)
		}
	})

	t.Run("a live link makes the daemon hear its repository in the same pass", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		issue := linkedIssue(t, rig.pool, "CAPTURE-1")
		if _, err := rig.w.heardAtBoot(context.Background()); err != nil {
			t.Fatalf("the boot's heard set: %v", err)
		}
		fact := intake.DispatchIssue{Key: "CAPTURE-1", Seq: 50, Type: "issue.updated", Status: issue.Status, Title: issue.Title, Rank: issue.Rank,
			Links: []intake.PullRequestLink{{Repo: linkedDeploy, Number: 86}, {Repo: "acme/gadgets", Number: 5}}, LinksKnown: true}
		if _, err := intake.ApplyFact(context.Background(), rig.pool, "dispatch", "link-event-"+randomSuffix(t), fact, rig.w.handlers...); err != nil {
			t.Fatalf("apply the event that adds a link: %v", err)
		}
		if !slices.ContainsFunc(rig.told(), func(repo ghrepo.Repository) bool { return strings.EqualFold(repo.String(), "acme/gadgets") }) {
			t.Errorf("GitHub intake was told to hear %v after the link of acme/gadgets#5 committed, want acme/gadgets among them", rig.told())
		}
		var reads int
		if err := rig.pool.QueryRow(context.Background(), `select count(*) from outbox where issue = 'CAPTURE-1' and kind = $1 and payload->>'repo' = 'acme/gadgets'`,
			string(record.OutboxKindPullRequestLinkRead)).Scan(&reads); err != nil || reads != 1 {
			t.Errorf("the outbox holds %d reads of the link to acme/gadgets#5 (%v), want the one the link owes", reads, err)
		}
	})

	t.Run("a link to a repository whose Legion App is not installed", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		rig.w.installs = &workflowTokenRecorder{missing: map[string]bool{"review acme/gadgets": true}}
		seedLinks(t, rig.pool, "CAPTURE-1", []record.PullRequestLink{{Issue: "CAPTURE-1", Repo: linkedDeploy, Number: 86}, {Issue: "CAPTURE-1", Repo: "acme/gadgets", Number: 5}})
		o := rig.outbox()
		for pass, id := range []int64{4, 5} {
			if err := o.pullRequestLinkRead(context.Background(), record.OutboxRow{ID: id, Issue: "CAPTURE-1"}, record.PullRequestLinkRead{Repo: "acme/gadgets", Number: 5}); err != nil {
				t.Fatalf("pass %d of the read of the link to acme/gadgets#5: %v", pass+1, err)
			}
		}
		var link *record.PullRequestLink
		if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			link, err = record.NewStore().PullRequestLinkOf(ctx, tx, "acme/gadgets", 5)
			return err
		}); err != nil || link == nil || link.Refused != record.LinkRefusedUnserved {
			t.Fatalf("the link to acme/gadgets#5 = %+v, %v; want it refused as unserved", link, err)
		}
		if told := len(noticeReasons(t, rig.pool, "pr-holds-close")); told != 1 {
			t.Errorf("the architect was sent %d notices that the link holds the close, want one over two passes", told)
		}
		merge := intake.PullRequestMerged{Repo: "acme/widgets", Number: 86, MergeSHA: "merge-own", HeadSHA: "code"}
		if _, err := intake.ApplyFact(context.Background(), rig.pool, "github", "merged-own-"+randomSuffix(t), merge, rig.w.handlers...); err != nil {
			t.Fatalf("apply the merge of the own pull request: %v", err)
		}
		if got := linkedIssue(t, rig.pool, "CAPTURE-1"); got.Phase == phase.Done {
			t.Errorf("CAPTURE-1 is %s after its own pull request merged, want the close held while a link names a repository Legion does not serve", got.Phase)
		}
	})

	t.Run("a repository only a link names is heard as GitHub spells it", func(t *testing.T) {
		t.Parallel()
		github := newLinkedGitHub(t, pulls()...)
		rig := seed(t, github)
		seedLinks(t, rig.pool, "CAPTURE-1", []record.PullRequestLink{{Issue: "CAPTURE-1", Repo: linkedDeploy, Number: 86}, {Issue: "CAPTURE-1", Repo: "acme-ops/tools", Number: 9}, {Issue: "CAPTURE-1", Repo: "acme-ops/plain", Number: 3}},
			linkedPullRow("CAPTURE-1", "Acme-Ops/Tools", 9, "fix/tools", "tools-head"))
		var heard []ghrepo.Repository
		if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			heard, err = rig.w.heardRepositories(ctx, tx)
			return err
		}); err != nil {
			t.Fatalf("heardRepositories: %v", err)
		}
		names := make([]string, len(heard))
		for at, repo := range heard {
			names[at] = repo.String()
		}
		for _, want := range []string{"Acme-Ops/Tools", "acme-ops/plain", linkedDeploy, "acme/widgets"} {
			if !slices.Contains(names, want) {
				t.Errorf("the repositories heard are %v, want %s among them, spelled so: NATS matches the subject of an event, which carries GitHub's spelling, case-sensitively", names, want)
			}
		}
		if slices.Contains(names, "acme-ops/tools") {
			t.Errorf("the repositories heard are %v, want the lowercase spelling of Acme-Ops/Tools, which no event's subject carries, left out", names)
		}
	})
}
