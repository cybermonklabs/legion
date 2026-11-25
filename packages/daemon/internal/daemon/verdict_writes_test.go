package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/verdict"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// verdictAppLogin is the one App's bot login in these rigs, verdictAppSlug the slug GitHub names
// that App by on each check run it made, and verdictAppNumber the App's id there, which the
// writer's configuration names as verdictAppID (github_apps.implement.app_id). verdictBotNumber is
// the id of the App's bot user, which each status it creates names as its creator's.
const (
	verdictAppLogin        = "legion-app[bot]"
	verdictAppSlug         = "legion-app"
	verdictAppNumber int64 = 1001
	verdictAppID           = "1001"
	verdictBotNumber int64 = 2001
)

// botLeases is tokens whose verdict lease carries the one App's bot identity, as appauth's leases
// do: its git identity, and its bot user's id (appauth.Lease.BotUserID), by which the writer tells
// the bot's commit statuses. A lease the tokens it wraps give an identity of their own keeps it.
type botLeases struct{ appauth.Tokens }

func (t botLeases) VerdictToken(ctx context.Context, owner string) (appauth.Lease, error) {
	lease, err := t.Tokens.VerdictToken(ctx, owner)
	if err == nil && lease.Identity == (appauth.GitIdentity{}) {
		lease.Identity = appauth.GitIdentity{Name: verdictAppLogin, Email: fmt.Sprintf("%d+%s@users.noreply.github.com", verdictBotNumber, verdictAppLogin)}
		lease.BotUserID = verdictBotNumber
	}
	return lease, err
}

// verdictLeaseAs is roleTokens whose verdict lease carries identity as the App bot's git identity
// and bot as its bot user's id, for a case that gives the lease's two parts apart.
type verdictLeaseAs struct {
	roleTokens
	identity appauth.GitIdentity
	bot      int64
}

func (t verdictLeaseAs) VerdictToken(ctx context.Context, owner string) (appauth.Lease, error) {
	lease, err := t.roleTokens.VerdictToken(ctx, owner)
	lease.Identity, lease.BotUserID = t.identity, t.bot
	return lease, err
}

// ciReviewer is the bot login acme/widgets' verdict entry names as its ci_reviewer.
const ciReviewer = "example-ci-review[bot]"

// widgetsEntry is acme/widgets as a verdict repository whose entry names its ci_reviewer.
var widgetsEntry = config.VerdictRepository{Repository: ghrepo.MustParse("acme/widgets"), CIReviewer: ciReviewer}

// verdictBearer is the Authorization a write of legion-verdict on acme/widgets carries: the
// daemon's own verdict token, as roleTokens mints it.
func verdictBearer() string { return "Bearer " + setToken(appauth.SetVerdict, "acme") }

// fakeRun is one check run GitHub holds at a head: its id, name, conclusion, the slug of the App
// that made it and that App's id (appID), and the title and summary of its output.
type fakeRun struct {
	ID         int64
	Head       string
	Name       string
	Conclusion string
	App        string
	AppID      int64
	Title      string
	Summary    string
}

// appID is the id of the App that made run: AppID where a case gives one, the one App's for a run
// under its slug, and 0, another App's, for every other.
func (run fakeRun) appID() int64 {
	switch {
	case run.AppID != 0:
		return run.AppID
	case run.App == verdictAppSlug:
		return verdictAppNumber
	}
	return 0
}

// fakePull is an open pull request of acme/widgets: its number, head branch, head commit and the
// branch it merges into, main where base is "".
type fakePull struct {
	number int
	ref    string
	head   string
	base   string
}

// baseRef is the branch pull merges into.
func (pull fakePull) baseRef() string {
	if pull.base == "" {
		return "main"
	}
	return pull.base
}

// fakeStatus is one commit status GitHub holds at a head, newest last, with its creator's login and
// that account's id (creatorID).
type fakeStatus struct {
	ID        int64
	Head      string
	Context   string
	State     string
	Creator   string
	CreatorID int64
}

// creatorID is the id of the account that created status: CreatorID where a case gives one, the
// App's bot's for a status under its login, and 0, another account's, for every other.
func (status fakeStatus) creatorID() int64 {
	switch {
	case status.CreatorID != 0:
		return status.CreatorID
	case status.Creator == verdictAppLogin:
		return verdictBotNumber
	}
	return 0
}

// fakeWrite is one write of a check run or a commit status the daemon made: the method, the path
// under the repository, the bearer and the body.
type fakeWrite struct {
	Method, Path, Bearer string
	Body                 map[string]any
}

// verdictGitHub is GitHub's REST API for acme/widgets as the writer of legion-verdict meets it: the
// check runs and the commit statuses of each head, created and updated by the daemon's writes; pull
// request 86's merge state, `blocked` while its head has no legion-verdict run concluding success
// and `clean` once it has, which is what a ruleset that requires the check answers; pull request
// 86's commits, one for each head it stood at (history), oldest first, each made by the
// implementer's session unless messages gives its message, as GitHub's compare of the base and a
// head lists them, and none at all where compareStatus is set, which GitHub's answer to the compare
// then is (502 where it fails the request, 404 or 422 where it cannot serve that compare); the
// branch pull request 86 merges into (base86, main where it is ""), whose tip holds the
// first parentCommits commits of history, so a compare with that branch lists only the commits after
// them while a compare with main lists them all; any other pull request named in pulls, whose reads it
// counts in pullReads; and the open pull requests at a commit (GET /commits/{sha}/pulls): pull
// request 86 at its head, and each of opened at its own. Each write and each read of pull request
// 86 is recorded in order. failCreates is how many of the next creates of a check run GitHub
// answers 502, as it does when it fails a request; staleReads is how many reads of pull request
// 86, made once its head has a success run, still answer `blocked`, as GitHub does while it has
// not yet counted a new run.
type verdictGitHub struct {
	mu            sync.Mutex
	nextID        int64
	head86        string
	base86        string
	parentCommits int
	history       []string
	messages      map[string]string
	compareStatus int
	runs          []fakeRun
	statuses      []fakeStatus
	writes        []fakeWrite
	events        []string
	pulls         map[int]string
	pullReads     map[int]int
	opened        []fakePull
	failCreates   int
	staleReads    int
}

func newVerdictGitHub() *verdictGitHub {
	return &verdictGitHub{nextID: 1000, head86: "code", history: []string{"code"}, messages: map[string]string{}, pulls: map[int]string{},
		pullReads: map[int]int{}}
}

// pullsAtCommit is the open pull requests at commit as GitHub lists them (GET
// /commits/{sha}/pulls, with state and head).
func pullsAtCommit(commit string, pulls []fakePull) []map[string]any {
	listed := []map[string]any{}
	for _, pull := range pulls {
		if pull.head == commit {
			listed = append(listed, map[string]any{"number": pull.number, "state": "open", "base": map[string]any{"ref": pull.baseRef()},
				"head": map[string]any{"ref": pull.ref, "sha": pull.head}})
		}
	}
	return listed
}

// implementerCommit is the message of a commit the implementer's session made: its two trailers,
// as `legion git` adds them.
func implementerCommit(sha string) string {
	return "work at " + sha + "\n\nOmp-Session: " + captureImplementerSession + "\nLegion-Role: implementer\n"
}

// commitsTo is pull request 86's commits up to head, oldest first: each head of history up to head,
// with its message, and whether history reaches head at all.
func (g *verdictGitHub) commitsTo(head string) ([]map[string]any, bool) {
	listed := []map[string]any{}
	for _, sha := range g.history {
		message, given := g.messages[sha]
		if !given {
			message = implementerCommit(sha)
		}
		listed = append(listed, map[string]any{"sha": sha, "commit": map[string]any{"message": message}})
		if sha == head {
			return listed, true
		}
	}
	return listed, false
}

// pullCommitsListed is the most commits GitHub lists of a pull request (GET /pulls/{number}/commits).
const pullCommitsListed = 250

// comparePage is one page of GitHub's compare of the base and head (GET /compare/BASE...HEAD): the
// commits of the page the query names, per_page to a page, and the Link to the next page where one
// is left. With no per_page GitHub pages nothing and lists the first 250 commits.
func comparePage(r *http.Request, commits []map[string]any) (page []map[string]any, next string) {
	size, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || size < 1 {
		return commits[:min(len(commits), pullCommitsListed)], ""
	}
	at, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || at < 1 {
		at = 1
	}
	from, to := min(len(commits), (at-1)*size), min(len(commits), at*size)
	if to < len(commits) {
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(at+1))
		next = "<http://" + r.Host + r.URL.Path + "?" + query.Encode() + `>; rel="next"`
	}
	return commits[from:to], next
}

// runOutput is the title and summary of the output a write of a check run sent.
func runOutput(sent map[string]any) (title, summary string) {
	shown, _ := sent["output"].(map[string]any)
	title, _ = shown["title"].(string)
	summary, _ = shown["summary"].(string)
	return title, summary
}

func (g *verdictGitHub) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		path, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets")
		if !ok {
			githubtest.Unexpected(t, w, r)
			return
		}
		parts := strings.Split(strings.Trim(path, "/"), "/")
		answer := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Errorf("encode the fake's answer: %v", err)
			}
		}
		body := func() map[string]any {
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Errorf("decode %s %s: %v", r.Method, r.URL.Path, err)
			}
			g.writes = append(g.writes, fakeWrite{Method: r.Method, Path: path, Bearer: r.Header.Get("Authorization"), Body: sent})
			return sent
		}
		switch {
		case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "pulls" && parts[1] == "86" && parts[2] == "commits":
			g.events = append(g.events, "listed the commits of pull request 86")
			listed, _ := g.commitsTo(g.head86)
			answer(listed[:min(len(listed), pullCommitsListed)])
		case r.Method == http.MethodGet && len(parts) >= 2 && parts[0] == "compare":
			// A branch name holds slashes, and GitHub takes BASE...HEAD as it is.
			spec := strings.Join(parts[1:], "/")
			base, head, ranged := strings.Cut(spec, "...")
			g.events = append(g.events, "compared "+spec)
			if r.Header.Get("Authorization") != "Bearer "+readToken("acme") {
				t.Errorf("the change %s was compared with %q, want the read token", spec, r.Header.Get("Authorization"))
			}
			commits, reached := g.commitsTo(head)
			base86 := fakePull{base: g.base86}.baseRef()
			switch {
			case g.compareStatus != 0:
				http.Error(w, fmt.Sprintf(`{"message":%q}`, http.StatusText(g.compareStatus)), g.compareStatus)
				return
			case !ranged || base != "main" && base != base86 || !reached:
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			if base != "main" {
				commits = commits[min(g.parentCommits, len(commits)):]
			}
			page, next := comparePage(r, commits)
			if next != "" {
				w.Header().Set("Link", next)
			}
			answer(map[string]any{"status": "ahead", "total_commits": len(commits), "commits": page})
		case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "pulls" && parts[1] == "86":
			state := "blocked"
			if slices.ContainsFunc(g.runs, func(run fakeRun) bool {
				return run.Head == g.head86 && run.Name == verdict.CheckName && run.App == verdictAppSlug && run.Conclusion == "success"
			}) {
				state = "clean"
				if g.staleReads > 0 {
					g.staleReads--
					state = "blocked"
				}
			}
			g.events = append(g.events, "read pull request 86: "+state)
			answer(map[string]any{"number": 86, "state": "open", "base": map[string]any{"ref": fakePull{base: g.base86}.baseRef()},
				"head": map[string]any{"ref": "legion/CAPTURE-1", "sha": g.head86}, "mergeable": true, "mergeable_state": state})
		case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "pulls":
			number, _ := strconv.Atoi(parts[1])
			pull, known := g.pulls[number]
			g.pullReads[number]++
			if !known {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(pull))
		case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "collaborators" && parts[2] == "permission":
			// a-writer has write access, a-reader read access; every bot is an App, which GitHub
			// may give write access and the daemon never reads as a person.
			login := parts[1]
			switch {
			case login == "a-writer":
				answer(map[string]any{"permission": "write", "user": map[string]any{"login": login, "type": "User"}})
			case login == "a-reader":
				answer(map[string]any{"permission": "read", "user": map[string]any{"login": login, "type": "User"}})
			case strings.HasSuffix(login, "[bot]"):
				answer(map[string]any{"permission": "write", "user": map[string]any{"login": login, "type": "Bot"}})
			default:
				http.NotFound(w, r)
			}
		case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "commits" && parts[2] == "pulls":
			if r.Header.Get("Authorization") != "Bearer "+readToken("acme") {
				t.Errorf("the pull requests at %s were listed with %q, want the read token", parts[1], r.Header.Get("Authorization"))
			}
			answer(pullsAtCommit(parts[1], append([]fakePull{{number: 86, ref: "legion/CAPTURE-1", head: g.head86, base: g.base86}}, g.opened...)))
		case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "commits" && parts[2] == "check-runs":
			if r.Header.Get("Authorization") != verdictBearer() {
				t.Errorf("the check runs of %s were listed with %q, want the verdict token", parts[1], r.Header.Get("Authorization"))
			}
			listed := []map[string]any{}
			for _, run := range g.runs {
				if run.Head == parts[1] && run.Name == r.URL.Query().Get("check_name") {
					listed = append(listed, map[string]any{"id": run.ID, "head_sha": run.Head, "name": run.Name, "status": "completed",
						"conclusion": run.Conclusion, "app": map[string]any{"id": run.appID(), "slug": run.App},
						"output": map[string]any{"title": run.Title, "summary": run.Summary}})
				}
			}
			answer(map[string]any{"total_count": len(listed), "check_runs": listed})
		case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "commits" && parts[2] == "statuses":
			listed := []map[string]any{}
			for i := len(g.statuses) - 1; i >= 0; i-- {
				if status := g.statuses[i]; status.Head == parts[1] {
					listed = append(listed, map[string]any{"id": status.ID, "context": status.Context, "state": status.State,
						"creator": map[string]any{"login": status.Creator, "id": status.creatorID()}})
				}
			}
			answer(listed)
		case r.Method == http.MethodPost && path == "/check-runs" && g.failCreates > 0:
			body()
			g.failCreates--
			g.events = append(g.events, "refused a create with 502")
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
		case r.Method == http.MethodPost && path == "/check-runs":
			sent := body()
			g.nextID++
			title, summary := runOutput(sent)
			run := fakeRun{ID: g.nextID, Head: fmt.Sprint(sent["head_sha"]), Name: fmt.Sprint(sent["name"]), Conclusion: fmt.Sprint(sent["conclusion"]), App: verdictAppSlug,
				Title: title, Summary: summary}
			g.runs = append(g.runs, run)
			g.events = append(g.events, fmt.Sprintf("created run %d on %s: %s", run.ID, run.Head, run.Conclusion))
			w.WriteHeader(http.StatusCreated)
			answer(map[string]any{"id": run.ID, "head_sha": run.Head, "conclusion": run.Conclusion})
		case r.Method == http.MethodPatch && len(parts) == 2 && parts[0] == "check-runs":
			sent := body()
			id, _ := strconv.ParseInt(parts[1], 10, 64)
			at := slices.IndexFunc(g.runs, func(run fakeRun) bool { return run.ID == id })
			if at < 0 {
				http.NotFound(w, r)
				return
			}
			g.runs[at].Conclusion = fmt.Sprint(sent["conclusion"])
			g.runs[at].Title, g.runs[at].Summary = runOutput(sent)
			g.events = append(g.events, fmt.Sprintf("updated run %d on %s: %s", id, g.runs[at].Head, g.runs[at].Conclusion))
			answer(map[string]any{"id": id, "conclusion": g.runs[at].Conclusion})
		case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == "statuses":
			sent := body()
			g.nextID++
			status := fakeStatus{ID: g.nextID, Head: parts[1], Context: fmt.Sprint(sent["context"]), State: fmt.Sprint(sent["state"]), Creator: verdictAppLogin}
			g.statuses = append(g.statuses, status)
			g.events = append(g.events, fmt.Sprintf("created status %d on %s: %s", status.ID, status.Head, status.State))
			w.WriteHeader(http.StatusCreated)
			answer(map[string]any{"id": status.ID, "state": status.State})
		default:
			githubtest.Unexpected(t, w, r)
		}
	}
}

// legionRuns is every legion-verdict check run GitHub holds at head.
func (g *verdictGitHub) legionRuns(head string) []fakeRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(g.runs), func(run fakeRun) bool { return run.Head != head || run.Name != verdict.CheckName })
}

// recorded is every write the daemon made, and every event, in order.
func (g *verdictGitHub) recorded() ([]fakeWrite, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.writes), slices.Clone(g.events)
}

// verdictRig is CAPTURE-1 on acme/widgets' pull request 86 at head code, the workflow runtime
// that writes legion-verdict on acme/widgets as the verdict repository entries say, its writer and
// its merge-state reader running, its log kept, the fake GitHub it writes on, and the head of each
// write the workflow asked for (workflow.Config.WriteVerdict), in order.
type verdictRig struct {
	pool   *store.Pool
	w      *workflowRuntime
	github *verdictGitHub
	logs   *syncBuffer
	n      int

	mu    sync.Mutex
	asked []string
}

// newVerdictRig builds a verdictRig over the store seed fills. wrap, where given, builds the fake's
// handler around the rig's verdictGitHub and its own handler, for a test that serves more of GitHub.
func newVerdictRig(t *testing.T, tokens appauth.Tokens, entries []config.VerdictRepository, seed func(*testing.T, *store.Pool),
	wrap ...func(*verdictGitHub, http.HandlerFunc) http.HandlerFunc) *verdictRig {
	t.Helper()
	return newVerdictRigOfApp(t, verdictAppID, tokens, entries, seed, wrap...)
}

// newVerdictRigOfApp is newVerdictRig whose writer's configuration names appID as the one App's id
// (github_apps.implement.app_id).
func newVerdictRigOfApp(t *testing.T, appID string, tokens appauth.Tokens, entries []config.VerdictRepository, seed func(*testing.T, *store.Pool),
	wrap ...func(*verdictGitHub, http.HandlerFunc) http.HandlerFunc) *verdictRig {
	t.Helper()
	rig := &verdictRig{pool: isolatedOutboxPool(t), github: newVerdictGitHub(), logs: &syncBuffer{}}
	seed(t, rig.pool)
	handle := rig.github.serve(t)
	for _, around := range wrap {
		handle = around(rig.github, handle)
	}
	api := githubtest.Server(t, githubtest.Spec{Handle: handle})
	log := slog.New(slog.NewJSONHandler(rig.logs, nil))
	w := &workflowRuntime{
		pool: rig.pool, records: projectRecords{Store: record.NewStore(), project: "CAPTURE"}, repositories: servedAcme(),
		tokens: botLeases{tokens}, dispatchProject: "CAPTURE", bootID: "test-boot", log: log, githubAPI: api, appLogin: verdictAppLogin,
		mergeStates: newWakeSet[string](), mergeStateRunning: map[string]*mergeStateRun{}, mergeStateRetry: readRigWait,
		verdicts: newVerdictWriter(entries, appID, log),
	}
	w.engine = workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories, AppLogin: verdictAppLogin,
		ReadMergeState: w.queueMergeState, WriteVerdict: func(issue, repo string, number int, head string) {
			rig.mu.Lock()
			rig.asked = append(rig.asked, head)
			rig.mu.Unlock()
			w.queueVerdictWrite(issue, repo, number, head)
		}}, log)
	w.handlers = []intake.Handler{w.engine, w.verdictFacts()}
	rig.w = w
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.readMergeStates(ctx)
	go w.writeVerdicts(ctx)
	return rig
}

// apply applies fact through the rig's handlers under an event id of its own.
func (rig *verdictRig) apply(t *testing.T, fact intake.Fact) {
	t.Helper()
	rig.n++
	applyFact(t, rig.pool, "test", fmt.Sprintf("verdict-%d", rig.n), fact, rig.w.handlers...)
}

// askedHeads is the head of each write the workflow asked for so far.
func (rig *verdictRig) askedHeads() []string {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return slices.Clone(rig.asked)
}

// review applies fact, a webhook's review, as intake delivers it: with its author's write access
// read from GitHub by the daemon's own reader (reviewerCanWrite, which the fake answers) for the
// reviews intake reads it for - a request for changes of a recorded pull request, and an approval
// or a request for changes of an outside one (intake's resolveReviewPermission, which
// TestADecidingReviewCarriesItsAuthorsWriteAccessBeforeItIsApplied and
// TestLegionVerdictAnOutsidePullRequestIsAFactOnAVerdictRepositoryAlone hold) - and with none
// for every other, as intake leaves it.
func (rig *verdictRig) review(t *testing.T, fact intake.Fact) {
	t.Helper()
	canWrite := func(review intake.PullRequestReview) bool {
		t.Helper()
		ok, err := rig.w.reviewerCanWrite(context.Background(), review)
		if err != nil {
			t.Fatalf("read %s's permission: %v", review.Author, err)
		}
		return ok
	}
	switch review := fact.(type) {
	case intake.PullRequestReview:
		if review.Decides() {
			review.AuthorCanWrite = canWrite(review)
		}
		fact = review
	case intake.OutsideReview:
		if review.Deciding() {
			review.AuthorCanWrite = canWrite(review.PullRequestReview)
		}
		fact = review
	default:
		t.Fatalf("%T is no review", fact)
	}
	rig.apply(t, fact)
}

// written waits until the writer holds no write queued or running.
func (rig *verdictRig) written(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the writer of legion-verdict to hold no write", func() bool { return rig.w.verdicts.idle() })
}

// idle is whether the writer holds no write queued or being made.
func (v *verdictWriter) idle() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.queue) == 0 && len(v.swept) == 0 && v.running == 0
}

// writeLines is every legion_verdict_write line the rig logged.
func (rig *verdictRig) writeLines(t *testing.T) []map[string]any {
	t.Helper()
	return loggedMessages(t, rig.logs.String(), "legion_verdict_write")
}

// verdictWrites is every write of legion-verdict the store records on acme/widgets at head.
func verdictWrites(t *testing.T, pool *store.Pool, head string) []store.VerdictWrite {
	t.Helper()
	var writes []store.VerdictWrite
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		writes, err = store.VerdictWritesTx(context.Background(), tx, ghrepo.MustParse("acme/widgets"), head)
		return err
	}); err != nil {
		t.Fatalf("read the writes of legion-verdict at %s: %v", head, err)
	}
	return writes
}

// verdictReviews is every review the store records of acme/widgets' pull request number.
func verdictReviews(t *testing.T, pool *store.Pool, number int) []store.VerdictReview {
	t.Helper()
	var reviews []store.VerdictReview
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reviews, err = store.VerdictReviewsTx(context.Background(), tx, ghrepo.MustParse("acme/widgets"), number)
		return err
	}); err != nil {
		t.Fatalf("read the reviews of pull request %d: %v", number, err)
	}
	return reviews
}

// The rows of CAPTURE-1's phases these rigs seed: the tester's completion at code, and a reviewer
// that has given no verdict yet.
var (
	testerPassAtCode = record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim", HandoffCommit: "code"}
	reviewerWithout  = record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim"}
)

// reviewingWithPass seeds CAPTURE-1 in reviewing on the default steps, with the tester's completion
// at code, no check state held and no merge state read for the head, so an approval waits on
// GitHub's answer (workflow's reviewRound).
func reviewingWithPass(t *testing.T, pool *store.Pool) {
	t.Helper()
	seedCapture(t, pool, phase.Reviewing, "needs_review", testerPassAtCode, reviewerWithout)
	clearCaptureMergeState(t, pool)
}

// The claims of CAPTURE-1's steps, each the agent its step's record names (the start writes the
// phase worker's claim, and for the owner's acceptance the architect of the issue's tree), which
// alone the daemon takes a review step's verdict from; and the session each records, which the
// Omp-Session test reads a commit's trailer against.
const (
	captureReviewer           claim.Token = "legion-capture-capture-1-reviewer"
	captureImplementer        claim.Token = "legion-capture-capture-1-implementer"
	captureArchitect          claim.Token = "legion-capture-capture-1-architect"
	captureTester             claim.Token = "legion-capture-capture-1-tester"
	captureImplementerSession             = "implementer-session"
	captureReviewerSession                = "reviewer-session"
	captureArchitectSession               = "architect-session"
	captureTesterSession                  = "tester-session"
)

// commitOf is the message of a commit whose Omp-Session line names session, as `legion git` writes
// it for the agent of that session.
func commitOf(session string) string {
	return "a change\n\nOmp-Session: " + session + "\n"
}

// The places of CAPTURE-1's default steps (planner implementer tester reviewer acceptance:owner)
// on its task default: the implementer's step, the tester's, the reviewer's and the owner's
// acceptance.
const (
	implementerStep = 2
	testerStep      = 3
	reviewerStep    = 4
	acceptanceStep  = 5
)

// seedStepStart records agent as the agent of the latest start of the step at place of CAPTURE-1's
// task, as the daemon's start writes it (workflow's startStep), and ends that run where ended: the
// step has passed.
func seedStepStart(t *testing.T, pool *store.Pool, task string, place int, agent claim.Token, ended bool) {
	t.Helper()
	at := record.StepPlace{Task: task, Kind: record.PlaceStep, Place: place}
	started := time.Date(2026, 11, 21, 5, place, 0, 0, time.UTC)
	inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if _, err := records.RaiseStepStarts(ctx, tx, "CAPTURE-1", at, func(int64) record.StepRecord {
			return record.StepRecord{Task: at.Task, Step: at.Place, Kind: at.Kind, Agent: string(agent), StartedAt: &started}
		}); err != nil || !ended {
			return err
		}
		_, _, err := records.EndStepStart(ctx, tx, "CAPTURE-1", at)
		return err
	})
}

// seedClaimSession records token, CAPTURE-1's claim of role, with session as the session it
// records, and holds that session for it in claim_sessions (internal/store's HoldSessions), as the
// claim's registration leaves them.
func seedClaimSession(t *testing.T, pool *store.Pool, token claim.Token, role claim.Role, session string) {
	t.Helper()
	inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, updated_at)
			values ($1, 'legion', 'CAPTURE-1', 'CAPTURE-1', $2, 1, $3, '', 'ready', 0, 0, 0, 0, now())`, string(token), string(role), session); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into claim_sessions (project, kind, name, claim) values ('legion', 'session', $1, $2)`, session, string(token))
		return err
	})
}

// dropClaimSessions leaves token's claim recording no session: claim_sessions holds no name for it,
// as an operator's release of its sessions leaves it, while the claim itself stands.
func dropClaimSessions(t *testing.T, pool *store.Pool, token claim.Token) {
	t.Helper()
	inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `delete from claim_sessions where claim = $1`, string(token))
		return err
	})
}

// checkSeq is the Dispatch sequence and serial of the next check state a rig holds, so each is
// newer than the one before it (record.Store.PutCheckState).
var checkSeq atomic.Int64

// bothChecks is the set of CAPTURE-1's checks at every head of these rigs.
var bothChecks = []string{"k1", "k2"}

// seedChecks keeps bothChecks as the set of CAPTURE-1's checks at head and each check's state there
// that states names, as Dispatch's check.state_changed events left them (workflow's checkState): a
// check states does not name holds no state.
func seedChecks(t *testing.T, pool *store.Pool, head string, states map[string]string) {
	t.Helper()
	inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if _, err := records.PutCheckSet(ctx, tx, "CAPTURE-1", "acme/widgets", head, bothChecks, checkSeq.Add(1)); err != nil {
			return err
		}
		for check, state := range states {
			if _, err := records.PutCheckState(ctx, tx, record.CheckState{Issue: "CAPTURE-1", Repo: "acme/widgets", Head: head, Check: check,
				State: state, Seq: checkSeq.Add(1)}); err != nil {
				return err
			}
		}
		return nil
	})
}

// bothYes is both checks reading yes.
var bothYes = map[string]string{"k1": verdict.StateYes, "k2": verdict.StateYes}

// checkState applies Dispatch's check.state_changed of CAPTURE-1's check at head, reading state, with
// bothChecks as the head's set, as intake decodes it.
func (rig *verdictRig) checkState(t *testing.T, head, check, state string) {
	t.Helper()
	seq := checkSeq.Add(1)
	rig.apply(t, intake.DispatchCheckState{Key: "CAPTURE-1", Mark: record.EventMark{Seq: seq}, Repo: "acme/widgets", Head: head,
		PullRequests: []int{86}, Checks: bothChecks, Check: check, CheckVersion: 1, State: state, Serial: seq})
}

// seedStepVerdict records agent's verdict word of the review step at place of CAPTURE-1's task at
// head, as the workflow recorded it (recordVerdict).
func seedStepVerdict(t *testing.T, pool *store.Pool, task string, place int, head string, word verdict.Word, agent claim.Token) {
	t.Helper()
	inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.RecordVerdictTx(ctx, tx, store.Verdict{Repository: widgets, Number: 86, Head: head, Issue: "CAPTURE-1", Task: task,
			Step: place, Verdict: word, Session: string(agent), RecordedAt: recordedAt})
		return err
	})
}

// seedAgents seeds the agents of CAPTURE-1's default steps and the session each claim records: the
// implementer at its step and the reviewer at its step, each run passed, and the owner's
// acceptance under the architect of the issue's tree, its run passed where accepted and standing
// open otherwise.
func seedAgents(t *testing.T, pool *store.Pool, accepted bool) {
	t.Helper()
	seedStepStart(t, pool, planwords.DefaultTask, implementerStep, captureImplementer, true)
	seedStepStart(t, pool, planwords.DefaultTask, reviewerStep, captureReviewer, true)
	seedStepStart(t, pool, planwords.DefaultTask, acceptanceStep, captureArchitect, accepted)
	seedClaimSession(t, pool, captureImplementer, claim.RoleImplementer, captureImplementerSession)
	seedClaimSession(t, pool, captureReviewer, claim.RoleReviewer, captureReviewerSession)
	seedClaimSession(t, pool, captureArchitect, claim.RoleArchitect, captureArchitectSession)
}

// acceptingAt seeds CAPTURE-1 in accepting on the default steps, the reviewer's approve of code
// recorded and the owner's acceptance not given yet, with the states of its checks at code that
// states names.
func acceptingAt(states map[string]string) func(*testing.T, *store.Pool) {
	return func(t *testing.T, pool *store.Pool) {
		t.Helper()
		seedCapture(t, pool, phase.Accepting, "needs_review", reviewerApprovedCode)
		seedAgents(t, pool, false)
		seedChecks(t, pool, "code", states)
		seedStepVerdict(t, pool, planwords.DefaultTask, reviewerStep, "code", verdict.ReviewerApprove, captureReviewer)
	}
}

// acceptingAtCode is CAPTURE-1 in accepting on the default steps, every step but the owner's
// acceptance passed at code: both checks yes there, and the reviewer's approve of code.
var acceptingAtCode = acceptingAt(bothYes)

// seedPlan records an owner's plan for CAPTURE-1 of one task, a, whose steps are roles, its task
// standing at the step at place.
func seedPlan(t *testing.T, pool *store.Pool, place int, roles ...string) {
	t.Helper()
	builtin := planwords.BuiltinRoles()
	inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		version := record.PlanVersion{Issue: "CAPTURE-1", Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: captureArchitect, GivenAt: recordedAt}
		task := record.PlanTask{Issue: "CAPTURE-1", Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.",
			State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: place}}
		var steps []record.PlanStep
		for i, role := range roles {
			steps = append(steps, record.PlanStep{Issue: "CAPTURE-1", Generation: 1, Task: "a", Position: i + 1, Role: role, Type: builtin[role].Type})
		}
		return record.NewStore().PutPlan(ctx, tx, version, []record.PlanTask{task}, steps)
	})
}

// reviewingOnPlan seeds CAPTURE-1 in reviewing on a plan of roles, a task whose reviewer's step is
// its last review step, at place: no merge state read for the head, so the reviewer's approve
// waits on GitHub's answer (workflow's reviewRound); the implementer's step, the first, passed
// under its claim; both claims' sessions; and both checks yes at code.
func reviewingOnPlan(place int, roles ...string) func(*testing.T, *store.Pool) {
	return func(t *testing.T, pool *store.Pool) {
		t.Helper()
		seedCapture(t, pool, phase.Reviewing, "needs_review", testerPassAtCode, reviewerWithout)
		clearCaptureMergeState(t, pool)
		seedPlan(t, pool, place, roles...)
		seedStepStart(t, pool, "a", 1, captureImplementer, true)
		seedClaimSession(t, pool, captureImplementer, claim.RoleImplementer, captureImplementerSession)
		seedClaimSession(t, pool, captureReviewer, claim.RoleReviewer, captureReviewerSession)
		seedChecks(t, pool, "code", bothYes)
	}
}

// reviewerLastAtCode is CAPTURE-1 in reviewing on the plan implementer tester reviewer, whose last
// step is the reviewer's, with every other step passed at code.
var reviewerLastAtCode = reviewingOnPlan(3, "implementer", "tester", "reviewer")

// reviewerVerdict is the reviewer's completion of reviewing with verdict at commit.
func reviewerVerdict(verdict, commit string, n int) intake.HandoffComplete {
	return intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: captureReviewer, Summary: "reviewed",
		Verdict: verdict, Commit: commit, HandoffRecord: fmt.Sprintf("review-%d", n), HandoffVersion: n}
}

// personsReview is a webhook's review of pull request 86 at code by a-writer, a person with write
// access, as GitHub delivers it: its write access is the daemon's to read (verdictRig.review).
func personsReview(id int64, state string) intake.PullRequestReview {
	return intake.PullRequestReview{Repo: "acme/widgets", Number: 86, ID: id, State: state, CommitID: "code", HeadSHA: "code",
		Author: "a-writer", SubmittedAt: time.Date(2026, 11, 21, 6, 0, int(id), 0, time.UTC)}
}

// signOff is the owner's acceptance of CAPTURE-1 by the architect of its tree, as its route reads
// it (api's signOff): the issue's generation, the run of accepting that stands, and the head the
// owner read, which is the head the record holds for its pull request when the owner signs off.
func (rig *verdictRig) signOff(t *testing.T) intake.SignOff {
	t.Helper()
	var entry record.StepEntry
	var head string
	inStore(t, rig.pool, func(ctx context.Context, tx pgx.Tx) (err error) {
		store := record.NewStore()
		if entry, err = store.OpenStep(ctx, tx, "CAPTURE-1"); err != nil {
			return err
		}
		pr, err := store.PullRequest(ctx, tx, "CAPTURE-1")
		if pr != nil {
			head = pr.HeadSHA
		}
		return err
	})
	return intake.SignOff{Issue: "CAPTURE-1", By: captureArchitect, Generation: 1, Entry: entry, Commit: head}
}

// soleRun fails the test unless legion-verdict has exactly one run on head, concluding conclusion,
// and answers it.
func (rig *verdictRig) soleRun(t *testing.T, head, conclusion string) fakeRun {
	t.Helper()
	runs := rig.github.legionRuns(head)
	if len(runs) != 1 || runs[0].Conclusion != conclusion {
		t.Fatalf("legion-verdict runs on %s = %+v, want one concluding %s", head, runs, conclusion)
	}
	return runs[0]
}

// approvedAndWritten applies the reviewer's approve at code to a rig reviewerLastAtCode seeded, the
// last step of its plan, and waits for the success it writes and for the round to settle as
// approved on GitHub's clean answer, which moves the issue on to its retro.
func (rig *verdictRig) approvedAndWritten(t *testing.T) fakeRun {
	t.Helper()
	rig.apply(t, reviewerVerdict("approve", "code", 1))
	testwait.Eventually(t, "the approve to write success on code and the round to settle as approved", func() bool {
		got, _ := capturePhase(t, rig.pool)
		return got == phase.Retro
	})
	rig.written(t)
	return rig.soleRun(t, "code", "success")
}

// acceptedAndWritten applies the owner's acceptance of code to a rig acceptingAtCode seeded, the
// last step of the default steps, and waits for the success it writes.
func (rig *verdictRig) acceptedAndWritten(t *testing.T) fakeRun {
	t.Helper()
	rig.apply(t, rig.signOff(t))
	rig.written(t)
	return rig.soleRun(t, "code", "success")
}

// successWrittenFirst fails the test unless run is the one write of rig's head code, a completed
// legion-verdict concluding success made with the verdict token, recorded in the store and in one
// legion_verdict_write line, and GitHub saw it before its first read of pull request 86's merge
// state, which answers clean.
func successWrittenFirst(t *testing.T, rig *verdictRig, run fakeRun) {
	t.Helper()
	writes, _ := rig.github.recorded()
	if len(writes) != 1 || writes[0].Method != http.MethodPost || writes[0].Bearer != verdictBearer() ||
		writes[0].Body["head_sha"] != "code" || writes[0].Body["conclusion"] != "success" || writes[0].Body["name"] != verdict.CheckName ||
		writes[0].Body["status"] != "completed" {
		t.Fatalf("writes = %+v, want one POST of a completed legion-verdict concluding success on code with the verdict token", writes)
	}
	var events []string
	testwait.Eventually(t, "GitHub to answer a read of pull request 86's merge state", func() bool {
		_, events = rig.github.recorded()
		return slices.ContainsFunc(events, func(e string) bool { return strings.HasPrefix(e, "read pull request 86") })
	})
	if first := slices.IndexFunc(events, func(e string) bool { return strings.HasPrefix(e, "read pull request 86") }); events[first] != "read pull request 86: clean" ||
		!slices.Contains(events[:first], fmt.Sprintf("created run %d on code: success", run.ID)) {
		t.Fatalf("GitHub saw %v, want the run written before the first read of the merge state, which answers clean", events)
	}
	if recorded := verdictWrites(t, rig.pool, "code"); len(recorded) != 1 || recorded[0].GitHubID != run.ID ||
		recorded[0].Kind != store.VerdictWriteCheckRun || recorded[0].Conclusion != store.VerdictConclusionSuccess || recorded[0].ForeignID != 0 {
		t.Fatalf("the store records %+v, want the one check run %d concluding success", recorded, run.ID)
	}
	lines := rig.writeLines(t)
	if len(lines) != 1 || lines[0]["head"] != "code" || lines[0]["conclusion"] != "success" || lines[0]["kind"] != "check_run" ||
		lines[0]["id"] != float64(run.ID) || lines[0]["foreign_id"] != float64(0) || lines[0]["repository"] != "acme/widgets" {
		t.Fatalf("legion_verdict_write lines = %v, want one naming the run, its head, its kind and its conclusion", lines)
	}
}

// C2: on a legion/KEY head H, with each check of the task reading yes at H, the review steps'
// approves at H write legion-verdict success on H with the verdict token, in the pass that
// records the last of them and before the merge-state read; GitHub then answers a clean merge
// state. The write is recorded in the store and in one legion_verdict_write line. With the
// default steps the owner's acceptance is the last of them, and the merge-state read is the one
// the writer asks for after a success. With a plan whose last review step is the reviewer's, the
// reviewer's approve is the last, its round waits on GitHub's answer, and the round settles.
func TestLegionVerdictApproveWritesSuccessBeforeTheMergeStateRead(t *testing.T) {
	t.Run("the default steps, the owner's acceptance last", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
		successWrittenFirst(t, rig, rig.acceptedAndWritten(t))
	})
	t.Run("a plan whose last review step is the reviewer's: the round settles", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewerLastAtCode)
		successWrittenFirst(t, rig, rig.approvedAndWritten(t))
	})
}

// GitHub can answer the first read after the success run is written with `blocked`, as it does
// while it has not yet counted a new run. That answer ends the climb, and no other fact asks
// again: Envoy leaves the one App's runs out of the CI record, so the write brings no checks fact.
// One more read of the issue after a write that produced success still settles the approve.
func TestLegionVerdictAStaleBlockedAfterTheWriteStillSettlesTheApprove(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewerLastAtCode)
	rig.github.mu.Lock()
	rig.github.staleReads = 1
	rig.github.mu.Unlock()
	rig.approvedAndWritten(t)
	_, events := rig.github.recorded()
	if first := slices.Index(events, "read pull request 86: blocked"); first < 0 || !slices.Contains(events[first:], "read pull request 86: clean") {
		t.Fatalf("GitHub saw %v, want a stale blocked after the write and then a clean read", events)
	}
}

// C2: a push of a new head writes nothing on that head: a check run belongs to one commit, and no
// verdict names the new one. The new head holds no verdict, so a write asked for there would write
// nothing either: what the test holds is that the push asks for none.
func TestLegionVerdictAPushWritesNothingOnTheNewHead(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
	rig.acceptedAndWritten(t)
	rig.apply(t, intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 86, Branch: "legion/CAPTURE-1", Before: "code", HeadSHA: "code-2"})
	rig.written(t)
	if runs := rig.github.legionRuns("code-2"); len(runs) != 0 {
		t.Fatalf("legion-verdict runs on the pushed head = %+v, want none", runs)
	}
	if asked := rig.askedHeads(); !slices.Equal(asked, []string{"code"}) {
		t.Fatalf("the workflow asked for writes on %v, want only the acceptance's on code", asked)
	}
	if writes, _ := rig.github.recorded(); len(writes) != 1 {
		t.Fatalf("writes = %+v, want only the acceptance's", writes)
	}
}

// C2: a reviewer's changes at H writes failure on H.
func TestLegionVerdictReviewerChangesWritesFailure(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	if run := rig.soleRun(t, "code", "failure"); run.Title != "legion-verdict: failure" {
		t.Fatalf("the run's title is %q, want legion-verdict: failure: a step failed, so no reason is named", run.Title)
	}
}

// C2: a check that reads no at H writes failure on H: Dispatch's check state at the head asks for
// the write (workflow's checkState), before any review step has a verdict there.
func TestLegionVerdictACheckNoAtHWritesFailure(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.checkState(t, "code", "k1", verdict.StateYes)
	rig.written(t)
	rig.nothingWrittenOn(t, "code")
	rig.checkState(t, "code", "k2", verdict.StateNo)
	rig.written(t)
	rig.soleRun(t, "code", "failure")
	if asked := rig.askedHeads(); !slices.Equal(asked, []string{"code", "code"}) {
		t.Fatalf("the workflow asked for writes on %v, want one on code for each check state there", asked)
	}
}

// A write GitHub fails is logged naming what repairs it, the writers' sweep at boot and every 10
// minutes, and leaves the head with no run: the writer makes no second attempt of its own.
func TestLegionVerdictAFailedWriteIsLoggedForTheWritersSweep(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.github.mu.Lock()
	rig.github.failCreates = 1
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 0 {
		t.Fatalf("legion-verdict runs on code = %+v, want none after GitHub failed the create", runs)
	}
	lines := loggedMessages(t, rig.logs.String(), "verdict: a write of legion-verdict failed; the writers' sweep at boot and every 10 minutes")
	if len(lines) != 1 || lines[0]["head"] != "code" || lines[0]["issue"] != "CAPTURE-1" {
		t.Fatalf("failed-write lines = %v, want one naming code and CAPTURE-1, and the sweep that repairs it", lines)
	}
}

// Review Focus 1: an approve at H1 after a push made H2 writes on H1 only, and H2 has no
// legion-verdict: no write is asked for on H2, which holds no verdict.
func TestLegionVerdictAtAnOldHeadWritesNothingOnTheNewHead(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewerLastAtCode)
	rig.apply(t, intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 86, Branch: "legion/CAPTURE-1", Before: "code", HeadSHA: "code-2"})
	rig.apply(t, reviewerVerdict("approve", "code", 1))
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].Conclusion != "success" {
		t.Fatalf("legion-verdict runs on the approved head = %+v, want one concluding success", runs)
	}
	if runs := rig.github.legionRuns("code-2"); len(runs) != 0 {
		t.Fatalf("legion-verdict runs on the new head = %+v, want none", runs)
	}
	if asked := rig.askedHeads(); !slices.Equal(asked, []string{"code"}) {
		t.Fatalf("the workflow asked for writes on %v, want only the approve's on code", asked)
	}
}

// Review Focus 2: a person's request for changes after the writer's success turns the writer's own
// run to failure: the same run, updated, and no second run.
func TestLegionVerdictPersonsLateRefusalTurnsSuccessToFailure(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
	run := rig.acceptedAndWritten(t)
	rig.review(t, personsReview(1, "changes_requested"))
	rig.written(t)
	runs := rig.github.legionRuns("code")
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Conclusion != "failure" {
		t.Fatalf("legion-verdict runs on code = %+v, want run %d alone, updated to failure", runs, run.ID)
	}
	writes, _ := rig.github.recorded()
	if last := writes[len(writes)-1]; last.Method != http.MethodPatch || last.Path != fmt.Sprintf("/check-runs/%d", run.ID) || last.Bearer != verdictBearer() {
		t.Fatalf("the refusal's write = %+v, want a PATCH of run %d with the verdict token", last, run.ID)
	}
}

// A success the writer wrote does not stand once the verdict at that head no longer does: when
// the writer decides the head again and the record holds no verdict there, it writes failure over
// its own run, so the head cannot merge on a success nothing backs (R4: a commit with no verdict
// cannot merge). Here Dispatch's state of a check at code turns from yes to unknown, which asks
// for the write on code (workflow's checkState).
func TestLegionVerdictASuccessWhoseVerdictNoLongerStandsTurnsToFailure(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
	run := rig.acceptedAndWritten(t)
	rig.checkState(t, "code", "k2", verdict.StateUnknown)
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].ID != run.ID || runs[0].Conclusion != "failure" {
		t.Fatalf("legion-verdict runs on code = %+v, want run %d turned to failure", runs, run.ID)
	}
	if recorded := verdictWrites(t, rig.pool, "code"); len(recorded) != 2 || recorded[1].Conclusion != store.VerdictConclusionFailure {
		t.Fatalf("the store records %+v, want the success and then the failure over it", recorded)
	}
}

// C2: a person's request for changes at H refuses a Legion head every step passed at, and a later
// approval of H by the same person lifts that refusal, so the steps' passes write success again.
func TestLegionVerdictPersonsApprovalLiftsTheirRefusal(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
	run := rig.acceptedAndWritten(t)
	rig.review(t, personsReview(1, "changes_requested"))
	rig.written(t)
	rig.review(t, personsReview(2, "approved"))
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].ID != run.ID || runs[0].Conclusion != "success" {
		t.Fatalf("legion-verdict runs on code = %+v, want run %d back at success", runs, run.ID)
	}
	if _, events := rig.github.recorded(); !slices.Contains(events, fmt.Sprintf("updated run %d on code: failure", run.ID)) {
		t.Fatalf("GitHub saw %v, want the refusal's failure before the success", events)
	}
}

// A person's approval delivered after their later request for changes lifts nothing: intake can
// apply a review late (a nak, a redelivery), so each author's last review is the one GitHub
// submitted last (record.ReviewOrder), not the one the daemon recorded last, and the run the
// refusal turned to failure stays at failure.
func TestLegionVerdictAnApprovalDeliveredLateLiftsNoLaterRefusal(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
	run := rig.acceptedAndWritten(t)
	rig.review(t, personsReview(2, "changes_requested"))
	rig.written(t)
	rig.review(t, personsReview(1, "approved"))
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].ID != run.ID || runs[0].Conclusion != "failure" {
		t.Fatalf("legion-verdict runs on code = %+v, want run %d left at failure", runs, run.ID)
	}
}

// C2: on a Legion head with no review step's approve, a person's approval at H writes nothing:
// only the review steps' verdicts approve a head a Legion issue owns.
func TestLegionVerdictPersonsApprovalWritesNothingOnALegionHead(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.review(t, personsReview(1, "approved"))
	rig.written(t)
	if writes, _ := rig.github.recorded(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
}

// The outside pull request 90 of acme/widgets, on a branch no issue owns, at head out-1.
var outsideOpened = intake.OutsidePullRequest{Repo: "acme/widgets", Number: 90, Branch: "feature/faster-builds", HeadSHA: "out-1"}

// outsideReview is a webhook's review of pull request 90 at out-1, as GitHub delivers it: its
// author's write access is the daemon's to read (verdictRig.review).
func outsideReview(id int64, author, state string) intake.OutsideReview {
	return intake.OutsideReview{PullRequestReview: intake.PullRequestReview{Repo: "acme/widgets", Number: 90, ID: id, State: state,
		CommitID: "out-1", HeadSHA: "out-1", Author: author, SubmittedAt: time.Date(2026, 11, 21, 6, 0, int(id), 0, time.UTC)}}
}

// nothingSeeded seeds no issue.
func nothingSeeded(*testing.T, *store.Pool) {}

// C2: on a head no Legion issue owns, of a verdict repository whose entry names a ci_reviewer, the
// CI reviewer's APPROVED at H writes success and its CHANGES_REQUESTED failure, and a person's
// CHANGES_REQUESTED after the CI reviewer's approval writes failure. A review by an account of type
// Bot that is not the CI reviewer writes nothing, and an approval by an account of type User with
// write access writes success.
func TestLegionVerdictOnAnOutsideHead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reviews []intake.OutsideReview
		want    []string
	}{
		{"the CI reviewer approves", []intake.OutsideReview{outsideReview(1, ciReviewer, "approved")}, []string{"success"}},
		{"the CI reviewer asks for changes", []intake.OutsideReview{outsideReview(1, ciReviewer, "changes_requested")}, []string{"failure"}},
		{"a person asks for changes after the CI reviewer's approval", []intake.OutsideReview{
			outsideReview(1, ciReviewer, "approved"), outsideReview(2, "a-writer", "changes_requested")}, []string{"success", "failure"}},
		{"a bot that is not the CI reviewer", []intake.OutsideReview{outsideReview(1, "another-bot[bot]", "approved")}, nil},
		{"a person approves", []intake.OutsideReview{outsideReview(1, "a-writer", "approved")}, []string{"success"}},
		{"an account with no write access approves", []intake.OutsideReview{outsideReview(1, "a-reader", "approved")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded)
			rig.apply(t, outsideOpened)
			var concluded []string
			for _, review := range tc.reviews {
				rig.review(t, review)
				rig.written(t)
				if runs := rig.github.legionRuns("out-1"); len(runs) == 1 {
					concluded = append(concluded, runs[0].Conclusion)
				} else if len(runs) > 1 {
					t.Fatalf("legion-verdict runs on out-1 = %+v, want one at most", runs)
				}
			}
			if !slices.Equal(slices.Compact(concluded), tc.want) {
				t.Fatalf("legion-verdict on out-1 concluded %v after each review, want %v", concluded, tc.want)
			}
		})
	}
}

// C2: a pull request on a branch that is no issue's branch is recorded with its head and its
// reviews.
func TestLegionVerdictRecordsAnOutsidePullRequestWithItsHeadAndReviews(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded)
	rig.apply(t, outsideOpened)
	rig.review(t, outsideReview(1, "a-writer", "approved"))
	rig.written(t)
	widgets := ghrepo.MustParse("acme/widgets")
	if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		pr, found, err := store.OutsidePullRequestTx(context.Background(), tx, widgets, 90)
		if err != nil {
			return err
		}
		if !found || pr.Head != "out-1" {
			t.Errorf("the outside pull request 90 reads %+v (found %v), want it at out-1", pr, found)
		}
		reviews, err := store.VerdictReviewsTx(context.Background(), tx, widgets, 90)
		if err != nil {
			return err
		}
		if len(reviews) != 1 || reviews[0].Head != "out-1" || reviews[0].Author != "a-writer" || !reviews[0].AuthorCanWrite ||
			reviews[0].State != "approved" || reviews[0].GitHubID != 1 {
			t.Errorf("the reviews of pull request 90 read %+v, want the person's approval at out-1", reviews)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// otherProjectsLine is what a daemon logs, once per pull request, of a legion/<KEY> head of
// another project on one of its verdict repositories (Review Focus 5, N7).
const otherProjectsLine = "verdict: the head is another project's issue branch, of whose steps this daemon holds none, so no verdict stands there"

// Review Focus 5, N7 and C2: a legion/<KEY> head of another project on a verdict repository holds
// no verdict here, since this daemon holds no step of KEY's issue, and one line once per pull
// request says why. So a review of that pull request, which no verdict reads, writes nothing and is
// not recorded, and with no run or status of the App on the head nothing is written there.
func TestLegionVerdictAnotherProjectsIssueHeadIsLoggedNotWritten(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded)
	rig.apply(t, intake.PullRequestOpened{Repo: "acme/widgets", Number: 91, Branch: "legion/OTHER-1", HeadSHA: "other-1", Base: "main"})
	rig.apply(t, intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 91, Branch: "legion/OTHER-1", Before: "other-1", HeadSHA: "other-2"})
	review := intake.OutsideReview{PullRequestReview: intake.PullRequestReview{Repo: "acme/widgets", Number: 91, ID: 5, State: "approved",
		CommitID: "other-2", HeadSHA: "other-2", Author: "a-writer"}}
	rig.review(t, review)
	rig.written(t)
	if writes, _ := rig.github.recorded(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none on another project's issue branch", writes)
	}
	if reviews := verdictReviews(t, rig.pool, 91); len(reviews) != 0 {
		t.Fatalf("the reviews recorded of pull request 91 = %+v, want none of another project's issue branch", reviews)
	}
	lines := loggedMessages(t, rig.logs.String(), otherProjectsLine)
	if len(lines) != 1 || lines[0]["repository"] != "acme/widgets" || lines[0]["pull_request"] != float64(91) || lines[0]["issue"] != "OTHER-1" {
		t.Fatalf("lines = %v, want one naming the pull request and the other project's issue", lines)
	}
}

// C2: a daemon writes on no repository its verdict_repositories does not name: a Legion head's
// approve there settles its round as before, and nothing is written.
func TestLegionVerdictWritesOnNoRepositoryItsEntriesDoNotName(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, nil, func(t *testing.T, pool *store.Pool) {
		seedCapture(t, pool, phase.Reviewing, "needs_review", testerPassAtCode, reviewerWithout)
	})
	rig.apply(t, reviewerVerdict("approve", "code", 1))
	rig.apply(t, outsideOpened)
	rig.review(t, outsideReview(1, "a-writer", "approved"))
	rig.written(t)
	if got, _ := capturePhase(t, rig.pool); got != phase.Accepting {
		t.Fatalf("the issue is in %s, want accepting on the clean merge state its record holds", got)
	}
	if writes, _ := rig.github.recorded(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none on a repository no entry names", writes)
	}
}

// tokenModeTokens mints as roleTokens does, and answers the verdict token as a daemon in token
// mode does (appauth.ErrNoVerdictToken).
type tokenModeTokens struct{ roleTokens }

func (tokenModeTokens) VerdictToken(context.Context, string) (appauth.Lease, error) {
	return appauth.Lease{}, appauth.ErrNoVerdictToken
}

// tokenModeLine is what the writer logs, once, when the App is in token mode.
const tokenModeLine = "verdict: legion-verdict is not written: the App is in token mode, which mints no verdict token"

// A daemon in token mode mints no verdict token (appauth.ErrNoVerdictToken), so it writes nothing
// and says why once, however many verdicts it holds.
func TestLegionVerdictTokenModeWritesNothingAndSaysWhyOnce(t *testing.T) {
	rig := newVerdictRig(t, tokenModeTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	rig.apply(t, outsideOpened)
	rig.review(t, outsideReview(1, ciReviewer, "approved"))
	rig.written(t)
	if writes, _ := rig.github.recorded(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none in token mode", writes)
	}
	if lines := loggedMessages(t, rig.logs.String(), tokenModeLine); len(lines) != 1 {
		t.Fatalf("token-mode lines = %v, want one", lines)
	}
}

// R19: a legion-verdict check run and a commit status of the one App at a head, which the daemon's
// record does not hold, get the daemon's own conclusion over them: the run updated to it, and a
// status of the daemon's own with the same state set over the status. Each write is in the record
// and in a legion_verdict_write line naming the foreign id. A run of another App is left alone.
func TestLegionVerdictWritesItsConclusionOverAForeignRunAndStatus(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.github.mu.Lock()
	rig.github.runs = append(rig.github.runs,
		fakeRun{ID: 500, Head: "code", Name: verdict.CheckName, Conclusion: "success", App: verdictAppSlug},
		fakeRun{ID: 501, Head: "code", Name: verdict.CheckName, Conclusion: "success", App: "github-actions"})
	rig.github.statuses = append(rig.github.statuses, fakeStatus{ID: 600, Head: "code", Context: verdict.CheckName, State: "success", Creator: verdictAppLogin})
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	runs := rig.github.legionRuns("code")
	if len(runs) != 2 || runs[0].ID != 500 || runs[0].Conclusion != "failure" || runs[1].ID != 501 || runs[1].Conclusion != "success" {
		t.Fatalf("legion-verdict runs on code = %+v, want the App's foreign run 500 at failure, another App's left alone, and no new run", runs)
	}
	rig.github.mu.Lock()
	statuses := slices.Clone(rig.github.statuses)
	rig.github.mu.Unlock()
	if len(statuses) != 2 || statuses[1].State != "failure" || statuses[1].Context != verdict.CheckName {
		t.Fatalf("statuses on code = %+v, want a newer legion-verdict status of the daemon's own concluding failure", statuses)
	}
	recorded := verdictWrites(t, rig.pool, "code")
	if len(recorded) != 2 || recorded[0].Kind != store.VerdictWriteCheckRun || recorded[0].GitHubID != 500 || recorded[0].ForeignID != 500 ||
		recorded[1].Kind != store.VerdictWriteStatus || recorded[1].GitHubID != statuses[1].ID || recorded[1].ForeignID != 600 {
		t.Fatalf("the store records %+v, want the run written over 500 and the status written over 600", recorded)
	}
	lines := rig.writeLines(t)
	if len(lines) != 2 || lines[0]["foreign_id"] != float64(500) || lines[1]["foreign_id"] != float64(600) || lines[1]["kind"] != "status" {
		t.Fatalf("legion_verdict_write lines = %v, want one per write, each naming its foreign id", lines)
	}
}

// B3 (R4, spec "The merge rule"): on a Legion head, the CI reviewer's request for changes at the
// head refuses it, as a person's does: the entry's ci_reviewer reaches the Legion head's decision.
// Here every step passed at code and wrote success; the CI reviewer's CHANGES_REQUESTED at code
// turns that run to failure, and its own later approval of code lifts its refusal, so the steps'
// passes write success again.
func TestLegionVerdictTheCIReviewersRefusalRefusesALegionHead(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
	run := rig.acceptedAndWritten(t)
	byCIReviewer := func(id int64, state string) intake.PullRequestReview {
		review := personsReview(id, state)
		review.Author = ciReviewer
		return review
	}
	rig.review(t, byCIReviewer(1, "changes_requested"))
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].ID != run.ID || runs[0].Conclusion != "failure" {
		t.Fatalf("legion-verdict runs on code after the CI reviewer's request for changes = %+v, want run %d turned to failure", runs, run.ID)
	}
	rig.review(t, byCIReviewer(2, "approved"))
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].ID != run.ID || runs[0].Conclusion != "success" {
		t.Fatalf("legion-verdict runs on code after the CI reviewer's approval = %+v, want run %d back at success", runs, run.ID)
	}
}

// N8 (R4: a refusal at a head beats every approval at that head): a check run belongs to a commit,
// so two open pull requests at one commit share one legion-verdict, and the writer decides that
// head over both. Here outside pull request 90 stands at code, the head of CAPTURE-1's pull request
// 86, whose every step passed there. A person's refusal of either one leaves code at failure, however
// often the other's verdict is decided after it: by a fact of the other, and by two sweeps that list
// both.
func TestLegionVerdictARefusalOfOnePullRequestAtAHeadBeatsAnotherOnesVerdict(t *testing.T) {
	atCode := func(id int64, state string) intake.OutsideReview {
		review := outsideReview(id, "a-writer", state)
		review.CommitID, review.HeadSHA = "code", "code"
		return review
	}
	for _, tc := range []struct {
		name    string
		refuse  func(*testing.T, *sweepRig)
		another func(*testing.T, *sweepRig)
	}{
		{"the outside pull request refused, then a fact of the Legion one",
			func(t *testing.T, rig *sweepRig) { rig.review(t, atCode(1, "changes_requested")) },
			func(t *testing.T, rig *sweepRig) { rig.checkState(t, "code", "k1", verdict.StateYes) }},
		{"the Legion pull request refused, then the outside one approved",
			func(t *testing.T, rig *sweepRig) { rig.review(t, personsReview(1, "changes_requested")) },
			func(t *testing.T, rig *sweepRig) { rig.review(t, atCode(2, "approved")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode,
				openCapture, sweptPull{number: 90, ref: "feature/faster-builds", head: "code"})
			rig.apply(t, intake.OutsidePullRequest{Repo: "acme/widgets", Number: 90, Branch: "feature/faster-builds", HeadSHA: "code"})
			run := rig.acceptedAndWritten(t)
			atFailure := func(after string) {
				t.Helper()
				if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].ID != run.ID || runs[0].Conclusion != "failure" {
					t.Fatalf("legion-verdict runs on code %s = %+v, want run %d at failure: a refusal at code beats every approval there", after, runs, run.ID)
				}
			}
			tc.refuse(t, rig)
			rig.written(t)
			atFailure("after the refusal")
			tc.another(t, rig)
			rig.written(t)
			atFailure("after the other pull request's fact")
			rig.swept(t)
			rig.swept(t)
			atFailure("after two sweeps")
		})
	}
}

// N9 (R19): the legion_verdict_write line names each run and status the daemon wrote on GitHub,
// even one its store could not record: the line is what a read of the App's runs at a head matches
// each run against (C56), so a write with no line would read as another writer's. The record's
// failure still fails the write, which is logged for the writers' sweep.
func TestLegionVerdictLogsAWriteTheStoreCouldNotRecord(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
		reviewingWithPass(t, pool)
		inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `create function refuse_verdict_write() returns trigger language plpgsql as
				$$ begin raise exception 'the store refuses the write'; end $$`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `create trigger refuse_verdict_write before insert on verdict_writes
				for each row execute function refuse_verdict_write()`)
			return err
		})
	})
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	run := rig.soleRun(t, "code", "failure")
	if recorded := verdictWrites(t, rig.pool, "code"); len(recorded) != 0 {
		t.Fatalf("the store records %+v, want nothing: it refuses every write", recorded)
	}
	lines := rig.writeLines(t)
	if len(lines) != 1 || lines[0]["id"] != float64(run.ID) || lines[0]["kind"] != "check_run" || lines[0]["conclusion"] != "failure" ||
		lines[0]["head"] != "code" {
		t.Fatalf("legion_verdict_write lines = %v, want one naming run %d at failure on code", lines, run.ID)
	}
	failed := loggedMessages(t, rig.logs.String(), verdictWriteFailed)
	if len(failed) != 1 || !strings.Contains(fmt.Sprint(failed[0]["error"]), "the store refuses the write") {
		t.Fatalf("failed-write lines = %v, want one naming the store's refusal", failed)
	}
}

// N9 (R19, C56): the writer tells the one App's runs by the App's id, which a rename of the App
// leaves as it is, while its slug changes (R16). A run whose slug is another but whose App id is
// the App's is the App's: the writer writes its conclusion over it rather than making a run beside
// it. A run of another App under the App's old slug is no run of the App, and is left alone.
func TestLegionVerdictTellsTheAppsRunsByItsID(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.github.mu.Lock()
	rig.github.runs = append(rig.github.runs,
		fakeRun{ID: 500, Head: "code", Name: verdict.CheckName, Conclusion: "success", App: "legion-renamed", AppID: verdictAppNumber},
		fakeRun{ID: 501, Head: "code", Name: verdict.CheckName, Conclusion: "success", App: verdictAppSlug, AppID: 4242})
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	runs := rig.github.legionRuns("code")
	if len(runs) != 2 || runs[0].ID != 500 || runs[0].Conclusion != "failure" || runs[1].ID != 501 || runs[1].Conclusion != "success" {
		t.Fatalf("legion-verdict runs on code = %+v, want the App's run 500 at failure, the other App's run 501 left alone, and no run beside them", runs)
	}
	if recorded := verdictWrites(t, rig.pool, "code"); len(recorded) != 1 || recorded[0].GitHubID != 500 || recorded[0].ForeignID != 500 {
		t.Fatalf("the store records %+v, want run 500 written over as another writer's", recorded)
	}
}

// R19: the writer tells the one App's runs by the id github_apps.implement.app_id names, and an id
// that is no number tells no run as the App's. The writer says so where it is built, once, as an
// ERROR that names the setting and the value it holds; a number says nothing. An id that is not set
// says nothing either: a daemon with no App id (token mode, or a boot whose configuration names no
// App) writes no legion-verdict as the App, and an ERROR at each boot would only be noise.
func TestLegionVerdictAnAppIDThatIsNoNumberIsLoggedWhereTheWriterIsBuilt(t *testing.T) {
	for _, tc := range []struct {
		appID  string
		id     int64
		errors int
	}{
		{appID: verdictAppID, id: verdictAppNumber},
		{appID: "legion-app", errors: 1},
		{appID: ""},
	} {
		t.Run(tc.appID, func(t *testing.T) {
			logs := &syncBuffer{}
			writer := newVerdictWriter([]config.VerdictRepository{widgetsEntry}, tc.appID, slog.New(slog.NewJSONHandler(logs, nil)))
			named := slices.DeleteFunc(loggedLines(t, logs.String(), "github_apps.implement.app_id"), func(line map[string]any) bool {
				return line["level"] != "ERROR" || line["app_id"] != tc.appID
			})
			if writer.appID != tc.id || len(named) != tc.errors {
				t.Fatalf("the writer of app_id %q holds id %d and logged %v; want id %d and %d ERROR lines naming the setting and its value",
					tc.appID, writer.appID, named, tc.id, tc.errors)
			}
		})
	}
}

// R19: an App id that is no number tells no run as the App's, the daemon's own among them, so a
// write would make a new run beside its own at each write. While the writer holds no App id it
// writes nothing, as in token mode: neither a fact's write nor the writers' sweep's, whose pass reads
// nothing of GitHub. The one ERROR where the writer is built says why.
func TestLegionVerdictAnAppIDThatIsNoNumberWritesNothing(t *testing.T) {
	rig := newSweepRigOfApp(t, "legion-app", roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass, openCapture)
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	rig.checkState(t, "code", "k1", verdict.StateNo)
	rig.written(t)
	_, before := rig.sweep.counts()
	rig.swept(t)
	rig.swept(t)
	if writes, _ := rig.github.recorded(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none while the App id is no number", writes)
	}
	if _, after := rig.sweep.counts(); after != before {
		t.Fatalf("the two sweeps made %d requests, want none: they could lead to no write", after-before)
	}
	named := slices.DeleteFunc(loggedLines(t, rig.logs.String(), "github_apps.implement.app_id"), func(line map[string]any) bool {
		return line["level"] != "ERROR"
	})
	if len(named) != 1 {
		t.Fatalf("the ERROR lines naming github_apps.implement.app_id are %v, want the one where the writer is built", named)
	}
}

// R19, C56: the writer tells the one App's bot's commit statuses by the bot user's id, which C56
// reads as each status's creator.id and which a rename of the App leaves as it is, while the bot's
// login changes (R16). Here the newest legion-verdict status is another account's that now holds
// the bot's old login, and the one before it is the App's bot's under its new login: the writer
// passes over the first and sets a status of its own over the bot's.
func TestLegionVerdictTellsTheBotsStatusesByItsID(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.github.mu.Lock()
	rig.github.statuses = append(rig.github.statuses,
		fakeStatus{ID: 600, Head: "code", Context: verdict.CheckName, State: "success", Creator: "legion-renamed[bot]", CreatorID: verdictBotNumber},
		fakeStatus{ID: 601, Head: "code", Context: verdict.CheckName, State: "success", Creator: verdictAppLogin, CreatorID: 4242})
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	rig.github.mu.Lock()
	statuses := slices.Clone(rig.github.statuses)
	rig.github.mu.Unlock()
	if len(statuses) != 3 || statuses[2].State != "failure" || statuses[2].creatorID() != verdictBotNumber {
		t.Fatalf("statuses on code = %+v, want a newer legion-verdict status of the daemon's own at failure", statuses)
	}
	if over := statusesOver(t, rig.pool, "code"); !slices.Equal(over, []int64{600}) {
		t.Fatalf("the daemon's statuses went over %v, want over the App's bot's status 600 alone, not the other account's 601", over)
	}
}

// statusesOver is the id of the status each legion-verdict status the store records the daemon
// set on head went over, 0 for one of its own, in the order it set them.
func statusesOver(t *testing.T, pool *store.Pool, head string) []int64 {
	t.Helper()
	var over []int64
	for _, written := range verdictWrites(t, pool, head) {
		if written.Kind == store.VerdictWriteStatus {
			over = append(over, written.ForeignID)
		}
	}
	return over
}

// R19, C56: the bot user's id the writer tells the bot's statuses by is the verdict lease's own
// (appauth.Lease.BotUserID), never a reading of the lease's git identity, so the form of the Git
// author email changes nothing. Here the email names no id at all, and the writer still sets a
// status of its own over the bot's.
func TestLegionVerdictTellsTheBotsStatusesWhateverTheLeasesEmailSays(t *testing.T) {
	tokens := verdictLeaseAs{identity: appauth.GitIdentity{Name: verdictAppLogin, Email: verdictAppLogin + "@example.invalid"}, bot: verdictBotNumber}
	rig := newVerdictRig(t, tokens, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.github.mu.Lock()
	rig.github.statuses = append(rig.github.statuses,
		fakeStatus{ID: 600, Head: "code", Context: verdict.CheckName, State: "success", Creator: verdictAppLogin})
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	if over := statusesOver(t, rig.pool, "code"); !slices.Equal(over, []int64{600}) {
		t.Fatalf("the daemon's statuses went over %v, want over the App's bot's status 600, which the lease's bot user id names", over)
	}
}

// R19: a verdict lease whose bot user's id is 0, unknown, tells no status as the bot's: not the
// bot's own, though the lease's email names the bot's id, and not one whose creator GitHub names
// with id 0. The writer still writes its check run, and sets no status.
func TestLegionVerdictTellsNoStatusAsTheBotsWhereTheLeaseNamesNoBotUserID(t *testing.T) {
	tokens := verdictLeaseAs{identity: appauth.GitIdentity{Name: verdictAppLogin, Email: fmt.Sprintf("%d+%s@users.noreply.github.com", verdictBotNumber, verdictAppLogin)}}
	rig := newVerdictRig(t, tokens, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
	rig.github.mu.Lock()
	rig.github.statuses = append(rig.github.statuses,
		fakeStatus{ID: 600, Head: "code", Context: verdict.CheckName, State: "success", Creator: verdictAppLogin},
		fakeStatus{ID: 601, Head: "code", Context: verdict.CheckName, State: "success", Creator: "a-writer"})
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("changes", "code", 1))
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].Conclusion != "failure" {
		t.Fatalf("legion-verdict runs on code = %+v, want the daemon's own run at failure", runs)
	}
	rig.github.mu.Lock()
	statuses := slices.Clone(rig.github.statuses)
	rig.github.mu.Unlock()
	if len(statuses) != 2 {
		t.Fatalf("statuses on code = %+v, want the two there before and none of the daemon's own", statuses)
	}
	if over := statusesOver(t, rig.pool, "code"); len(over) != 0 {
		t.Fatalf("the daemon's statuses went over %v, want none: the lease names no bot user id", over)
	}
}

// C2's fake answers a merge-state read that waits on the write: queueVerdictWrite holds the
// issue's reads until the write ends (verdictWriter.await), so a read asked for on the same commit
// as the write is made after it.
func TestLegionVerdictHoldsTheIssuesMergeStateReadUntilItsWriteEnds(t *testing.T) {
	writer := newVerdictWriter([]config.VerdictRepository{widgetsEntry}, verdictAppID, slog.New(slog.DiscardHandler))
	w := &workflowRuntime{verdicts: writer, log: slog.New(slog.DiscardHandler)}
	w.queueVerdictWrite("CAPTURE-1", "acme/widgets", 86, "code")
	read := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	go func() {
		ended, err := writer.await(ctx, "CAPTURE-1")
		if err == nil && !ended {
			err = errors.New("the bound passed")
		}
		read <- err
	}()
	select {
	case err := <-read:
		t.Fatalf("the read went on (%v) while the issue's write was pending", err)
	case <-time.After(50 * time.Millisecond):
	}
	if ended, err := writer.await(ctx, "CAPTURE-2"); err != nil || !ended {
		t.Fatalf("another issue's read waited: %v, %v", ended, err)
	}
	target, ok := writer.next()
	if !ok || target.head != "code" || target.number != 86 {
		t.Fatalf("the writer took %+v, %v; want the queued write of code", target, ok)
	}
	writer.done(target)
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("the read after the write ended: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the read still waits after the write ended")
	}
	if ended, err := writer.await(canceledContext(), "CAPTURE-1"); err != nil || !ended {
		t.Fatalf("a read of an issue with no write pending waited: %v, %v", ended, err)
	}
}

// Two asks for one write, made before the writer takes it, queue that write once: the one write
// answers both, so GitHub is read and written once, and the issue's merge-state reads wait on that
// write alone. The same holds for two of the writers' sweep's asks. Once the writer has taken the
// write, the same ask queues it again, since the head's verdict may change after the write read it.
func TestLegionVerdictTwoAsksForOneWriteQueueItOnce(t *testing.T) {
	writer := newVerdictWriter([]config.VerdictRepository{widgetsEntry}, verdictAppID, slog.New(slog.DiscardHandler))
	fact := verdictTarget{issue: "CAPTURE-1", repository: widgets, number: 86, head: "code"}
	swept := verdictTarget{repository: widgets, number: 90, head: "out-1", overForeign: true}
	writer.add(fact)
	writer.add(fact)
	writer.add(swept)
	writer.add(swept)
	target, ok := writer.next()
	if !ok || target != fact {
		t.Fatalf("the writer took %+v, %v; want the fact's write of code", target, ok)
	}
	writer.done(target)
	if ended, err := writer.await(canceledContext(), "CAPTURE-1"); err != nil || !ended {
		t.Fatalf("CAPTURE-1's reads wait (%v, %v) after its one write ended: two asks queued two writes", ended, err)
	}
	writer.add(fact)
	var taken []verdictTarget
	for target, ok := writer.next(); ok; target, ok = writer.next() {
		taken = append(taken, target)
		writer.done(target)
	}
	if want := []verdictTarget{fact, swept}; !slices.Equal(taken, want) {
		t.Fatalf("the writer then took %+v, want %+v: the fact's ask after the writer took its write, and the sweep's two asks as one", taken, want)
	}
}

// A merge-state read waits on its issue's write of legion-verdict for one bound at most (the
// writer's bound, one issueRead): a write held up by a slow GitHub or a long queue holds the
// issue's reads no longer than that, and the read then goes on and says so once.
func TestLegionVerdictAMergeStateReadWaitsOnItsWriteForOneBoundAtMost(t *testing.T) {
	writer := newVerdictWriter([]config.VerdictRepository{widgetsEntry}, verdictAppID, slog.New(slog.DiscardHandler))
	writer.bound = 50 * time.Millisecond
	logs := &syncBuffer{}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: projectRecords{Store: record.NewStore(), project: "CAPTURE"},
		verdicts: writer, log: slog.New(slog.NewJSONHandler(logs, nil))}
	// The write is queued and never taken, as a write that never ends.
	w.queueVerdictWrite("CAPTURE-1", "acme/widgets", 86, "code")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := w.readMergeState(ctx, "CAPTURE-1", ""); err != nil {
		t.Fatalf("the read behind a write that never ends: %v, want it to go on after the bound", err)
	}
	lines := loggedMessages(t, logs.String(), verdictReadWentOn)
	if len(lines) != 1 || lines[0]["issue"] != "CAPTURE-1" {
		t.Fatalf("%q lines = %v, want one naming CAPTURE-1", verdictReadWentOn, lines)
	}
}

// A write asked for on a repository no verdict entry names, under the name a record keeps or the
// one GitHub serves it under now, is queued for nothing: it would write nothing, and the issue's
// merge-state reads would wait on it. One the entries name under its serving name is queued.
func TestLegionVerdictQueuesNoWriteOnARepositoryItsEntriesDoNotName(t *testing.T) {
	served := newRepositories("CAPTURE", transferredCapture)
	served.fill(ghrepo.MustParse("cybermonklabs/widgets"))
	writer := newVerdictWriter([]config.VerdictRepository{{Repository: ghrepo.MustParse("cybermonklabs/widgets")}}, verdictAppID, slog.New(slog.DiscardHandler))
	w := &workflowRuntime{verdicts: writer, repositories: served, log: slog.New(slog.DiscardHandler)}
	w.queueVerdictWrite("CAPTURE-1", "acme/gadgets", 86, "code")
	if !writer.idle() {
		t.Fatal("a write on acme/gadgets, which no entry names, is queued; want none")
	}
	if ended, err := writer.await(canceledContext(), "CAPTURE-1"); err != nil || !ended {
		t.Fatalf("CAPTURE-1's read waits (%v, %v) on a write on a repository no entry names", ended, err)
	}
	w.queueVerdictWrite("CAPTURE-1", "acme/widgets", 86, "code")
	if target, ok := writer.next(); !ok || target.repository.String() != "acme/widgets" || target.head != "code" {
		t.Fatalf("the writer took %+v, %v; want the write on acme/widgets, which GitHub serves as the entry's cybermonklabs/widgets", target, ok)
	}
}

// A write asked for on a pull request of a verdict repository that no record holds - one opened
// before the daemon heard its repository, whose first event here is a review - reads that pull
// request from GitHub once (GET /pulls/{number}, decideVerdict). On a branch that is no issue's it
// is recorded as an outside pull request at the head GitHub shows and decided as one; on another
// project's issue branch no verdict stands, which is logged once, and with no run of the App there
// nothing is written; on this project's issue branch that no record holds it has no verdict here;
// and one GitHub shows closed merges nothing, so it is neither recorded nor written on, as a closed
// Legion pull request is not.
func TestLegionVerdictReadsAPullRequestNoRecordHoldsFromGitHub(t *testing.T) {
	pull := func(state, ref string) string {
		return fmt.Sprintf(`{"number":92,"state":%q,"base":{"ref":"main"},"head":{"ref":%q,"sha":"out-9"}}`, state, ref)
	}
	for _, tc := range []struct {
		name, pull string
		want       []string
		recorded   bool
		logged     bool
	}{
		{name: "a branch that is no issue's", pull: pull("open", "feature/faster-tests"), want: []string{"success"}, recorded: true},
		{name: "another project's issue branch", pull: pull("open", "legion/OTHER-2"), logged: true},
		{name: "this project's issue branch, which no record holds", pull: pull("open", "legion/CAPTURE-9")},
		{name: "a pull request GitHub shows closed", pull: pull("closed", "feature/faster-tests")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded)
			rig.github.mu.Lock()
			rig.github.pulls[92] = tc.pull
			rig.github.mu.Unlock()
			rig.review(t, intake.OutsideReview{PullRequestReview: intake.PullRequestReview{Repo: "acme/widgets", Number: 92, ID: 9, State: "approved",
				CommitID: "out-9", HeadSHA: "out-9", Author: "a-writer", SubmittedAt: time.Date(2026, 11, 21, 6, 0, 9, 0, time.UTC)}})
			rig.written(t)
			var concluded []string
			for _, run := range rig.github.legionRuns("out-9") {
				concluded = append(concluded, run.Conclusion)
			}
			if !slices.Equal(concluded, tc.want) {
				t.Fatalf("legion-verdict on out-9 concluded %v, want %v", concluded, tc.want)
			}
			var sighted store.OutsidePullRequest
			var found bool
			if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				sighted, found, err = store.OutsidePullRequestTx(context.Background(), tx, ghrepo.MustParse("acme/widgets"), 92)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if found != tc.recorded || found && sighted.Head != "out-9" {
				t.Fatalf("the outside pull request 92 reads %+v (found %v), want found %v at out-9", sighted, found, tc.recorded)
			}
			if lines := loggedMessages(t, rig.logs.String(), otherProjectsLine); tc.logged != (len(lines) == 1 && lines[0]["issue"] == "OTHER-2") || len(lines) > 1 {
				t.Fatalf("lines = %v, want one naming OTHER-2: %v", lines, tc.logged)
			}
		})
	}
}

// The output of a legion-verdict run that concludes failure because each check reads yes and every
// testing and review step passed and a commit of the change has no reader, or the daemon could not
// read the change's commits, as the merger reads it to tell the owner: the one title of both, and
// the opening of each summary. The summary of a commit with no reader then names each such commit,
// one to a line.
const (
	wantNoReaderTitle   = "legion-verdict: failure (a commit with no reader)"
	wantNoReaderOpening = "Each check reads yes and every testing and review step of the task's plan passed at this head, and no agent of the task read these commits of the change"
	wantUnreadOpening   = "Each check reads yes and every testing and review step of the task's plan passed at this head, but the daemon could not read every commit of the change"
)

// noReader fails the test unless the one legion-verdict run on head concludes failure with the
// title of a commit with no reader and a summary that opens with opening and names commits, in
// their order, one to a line ("- <sha>"), and no other.
func (rig *verdictRig) noReader(t *testing.T, head, opening string, commits ...string) {
	t.Helper()
	run := rig.soleRun(t, head, "failure")
	var named []string
	for _, line := range strings.Split(run.Summary, "\n") {
		if sha, ok := strings.CutPrefix(line, "- "); ok {
			named = append(named, sha)
		}
	}
	if run.Title != wantNoReaderTitle || !strings.HasPrefix(run.Summary, opening) || !slices.Equal(named, commits) {
		t.Fatalf("the run's output reads %q / %q, want %q, a summary opening %q and naming the commits %q: the merger's notice to the owner names each commit with no reader",
			run.Title, run.Summary, wantNoReaderTitle, opening, commits)
	}
}

// C2: the same holds on a task branch legion/KEY.slug: a pull request opened on legion/CAPTURE-1.d is
// CAPTURE-1's (claim.BranchIssue), and every step passed at its head writes success there.
func TestLegionVerdictOnATaskBranch(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
		acceptingAtCode(t, pool)
		if _, err := pool.Exec(context.Background(), `delete from pull_requests where issue = 'CAPTURE-1'`); err != nil {
			t.Fatalf("leave CAPTURE-1 with no pull request: %v", err)
		}
	})
	rig.apply(t, intake.PullRequestOpened{Repo: "acme/widgets", Number: 86, Branch: "legion/CAPTURE-1.d", Base: "main", HeadSHA: "code",
		UpdatedAt: captureHeadUpdated})
	if _, pr := capturePhase(t, rig.pool); pr == nil || pr.Branch != "legion/CAPTURE-1.d" {
		t.Fatalf("CAPTURE-1's pull request is %+v, want #86 on the task branch", pr)
	}
	rig.acceptedAndWritten(t)
}

// C2: before every testing and review step has passed at H, and with none failed there, nothing
// is written on H: the reviewer's approve with the owner's acceptance not given yet, and both review
// steps' approves with a check that holds no state at H.
func TestLegionVerdictWritesNothingBeforeEveryStepPassed(t *testing.T) {
	t.Run("the reviewer's approve, the owner's acceptance not given", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
			reviewingWithPass(t, pool)
			seedAgents(t, pool, false)
			seedChecks(t, pool, "code", bothYes)
		})
		rig.apply(t, reviewerVerdict("approve", "code", 1))
		rig.written(t)
		if asked := rig.askedHeads(); !slices.Equal(asked, []string{"code"}) {
			t.Fatalf("the workflow asked for writes on %v, want the approve's on code", asked)
		}
		rig.nothingWrittenOn(t, "code")
	})
	t.Run("both approves, a check with no state at H", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAt(map[string]string{"k1": verdict.StateYes}))
		rig.apply(t, rig.signOff(t))
		rig.written(t)
		if asked := rig.askedHeads(); !slices.Equal(asked, []string{"code"}) {
			t.Fatalf("the workflow asked for writes on %v, want the acceptance's on code", asked)
		}
		rig.nothingWrittenOn(t, "code")
		if _, events := rig.github.recorded(); readTheChange(events) {
			t.Fatalf("GitHub saw %v, want no read of the change's commits before every step passed", events)
		}
	})
}

// readTheChange is whether GitHub saw a read of pull request 86's commits among events: a compare
// of its base and a head, or a list of its commits.
func readTheChange(events []string) bool {
	return slices.ContainsFunc(events, func(event string) bool {
		return strings.HasPrefix(event, "compared ") || event == "listed the commits of pull request 86"
	})
}

// C2: the fake's compare of the base and H gives a change of 300 commits, in three pages of 100,
// each carrying the implementer's Omp-Session line; H is the 300th, code. With no line of the
// reviewer's session, the plan implementer reviewer writes success on H once the reviewer approves
// it; with the reviewer's line on commit 260, it writes failure there, and the summary names commit
// 260 alone. GitHub's list of a pull request's commits stops at 250, so only the compare's every
// page reads the whole change.
func TestLegionVerdictReadsEveryPageOfALongChange(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reviewer bool
	}{
		{"no line of the reviewer's session", false},
		{"the reviewer's line on commit 260", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingOnPlan(2, "implementer", "reviewer"))
			rig.github.mu.Lock()
			rig.github.history = nil
			for i := 1; i < 300; i++ {
				rig.github.history = append(rig.github.history, fmt.Sprintf("c%03d", i))
			}
			rig.github.history = append(rig.github.history, "code")
			if tc.reviewer {
				rig.github.messages["c260"] = implementerCommit("c260") + "Omp-Session: " + captureReviewerSession + "\n"
			}
			rig.github.mu.Unlock()
			rig.apply(t, reviewerVerdict("approve", "code", 1))
			rig.written(t)
			if _, events := rig.github.recorded(); !slices.Equal(compares(events), []string{"compared main...code", "compared main...code", "compared main...code"}) {
				t.Fatalf("GitHub saw %v, want three pages of the compare of main and code", events)
			}
			if !tc.reviewer {
				rig.soleRun(t, "code", "success")
				return
			}
			rig.noReader(t, "code", wantNoReaderOpening, "c260")
		})
	}
}

// compares is each compare among events, in order.
func compares(events []string) []string {
	var compared []string
	for _, event := range events {
		if strings.HasPrefix(event, "compared ") {
			compared = append(compared, event)
		}
	}
	return compared
}

// R4: the change is GitHub's compare of the head with the branch its pull request merges into, which
// the writer reads from the open pull request at the head as GitHub lists it. Here CAPTURE-1 is a
// child issue: its pull request 86 merges into its parent's branch, legion/CAPTURE-0, whose tip holds
// commit p1, and its record holds no base, as one recorded before the daemon kept the base. p1 names
// a session of the reviewer's claim. With the plan implementer reviewer and the reviewer's approve at
// code, the change is code alone, which the reviewer reads, so the head gets success. A compare with
// the issue's default branch would count the parent's p1 as the child's, which no agent but its
// maker read, and write failure.
func TestLegionVerdictComparesTheHeadWithTheBranchItsPullRequestMergesInto(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingOnPlan(2, "implementer", "reviewer"))
	rig.github.mu.Lock()
	rig.github.base86, rig.github.parentCommits = "legion/CAPTURE-0", 1
	rig.github.history = []string{"p1", "code"}
	rig.github.messages["p1"] = commitOf(captureReviewerSession)
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("approve", "code", 1))
	rig.written(t)
	if _, events := rig.github.recorded(); !slices.Equal(compares(events), []string{"compared legion/CAPTURE-0...code"}) {
		t.Fatalf("GitHub saw %v, want one compare of legion/CAPTURE-0, the branch pull request 86 merges into, and code", events)
	}
	rig.soleRun(t, "code", "success")
}

// C2: a plan of the steps implementer reviewer has no testing step. There, with each check reading
// yes at H, the reviewer's approve at H, from a claim no session of which a commit of the change
// names, writes success on H; a check that reads no at H writes failure there all the same, since
// each check must read yes whatever steps the plan names (R28).
func TestLegionVerdictAPlanWithNoTestingStep(t *testing.T) {
	t.Run("each check yes", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingOnPlan(2, "implementer", "reviewer"))
		rig.apply(t, reviewerVerdict("approve", "code", 1))
		rig.written(t)
		rig.soleRun(t, "code", "success")
	})
	t.Run("a check no", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
			reviewingOnPlan(2, "implementer", "reviewer")(t, pool)
			seedChecks(t, pool, "code", map[string]string{"k1": verdict.StateNo})
		})
		rig.apply(t, reviewerVerdict("approve", "code", 1))
		rig.written(t)
		rig.soleRun(t, "code", "failure")
	})
}

// C2: a plan whose one review step's claim names its session in the Omp-Session line of a commit
// writes failure on H once every step has passed there: no agent but that commit's maker approved
// it. The run's summary names that commit.
func TestLegionVerdictAReviewStepReadsNoCommitItsSessionMade(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingOnPlan(2, "implementer", "reviewer"))
	rig.github.mu.Lock()
	rig.github.history = []string{"code-0", "code"}
	rig.github.messages["code"] = "fix a typo\n\nOmp-Session: " + captureReviewerSession + "\nLegion-Role: reviewer\n"
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("approve", "code", 1))
	rig.written(t)
	rig.noReader(t, "code", wantNoReaderOpening, "code")
}

// C2, the default steps, two ways. Where the claims of the tester, the reviewer and the owner each
// record a session, a commit that names the reviewer's session and another that names the owner's
// each have a reader - the reviewer's commit the tester and the owner, the owner's the tester and
// the reviewer - so the head gets success; one independent review of the whole change, the rule
// before, has none here. Where the tester's and the owner's claims record no session, a commit that
// names the reviewer's session has no reader (R4's third departure), so the head gets failure once
// every step has passed there, and the run's summary names that commit.
func TestLegionVerdictEachCommitHasAReaderOnTheDefaultSteps(t *testing.T) {
	t.Run("each of the three claims records a session", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
			acceptingAtCode(t, pool)
			seedStepStart(t, pool, planwords.DefaultTask, testerStep, captureTester, true)
			seedClaimSession(t, pool, captureTester, claim.RoleTester, captureTesterSession)
		})
		rig.github.mu.Lock()
		rig.github.history = []string{"code-0", "code"}
		rig.github.messages["code-0"] = commitOf(captureReviewerSession)
		rig.github.messages["code"] = commitOf(captureArchitectSession)
		rig.github.mu.Unlock()
		rig.acceptedAndWritten(t)
	})
	t.Run("the tester's and the owner's claims record no session", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
			acceptingAtCode(t, pool)
			seedStepStart(t, pool, planwords.DefaultTask, testerStep, captureTester, true)
			dropClaimSessions(t, pool, captureArchitect)
		})
		rig.github.mu.Lock()
		rig.github.messages["code"] = commitOf(captureReviewerSession)
		rig.github.mu.Unlock()
		rig.apply(t, rig.signOff(t))
		rig.written(t)
		rig.noReader(t, "code", wantNoReaderOpening, "code")
	})
}

// C2: a commit with no Omp-Session line has no agent maker, as a person's commit has none, so each
// agent that reads at the head reads it: with the plan implementer reviewer the reviewer's approve
// writes success. The reviewer also reads a commit whose line names a session that no claim records,
// a maker outside the task's claims, so that case writes success too.
func TestLegionVerdictACommitWithNoAgentMakerOfTheTaskIsRead(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{"a commit with no Omp-Session line", "Merge main into legion/CAPTURE-1\n"},
		{"a commit naming a session no claim records", commitOf("a-session-of-no-claim")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingOnPlan(2, "implementer", "reviewer"))
			rig.github.mu.Lock()
			rig.github.messages["code"] = tc.message
			rig.github.mu.Unlock()
			rig.apply(t, reviewerVerdict("approve", "code", 1))
			rig.written(t)
			rig.soleRun(t, "code", "success")
		})
	}
}

// C2: where the reviewer's claim records no session, the daemon cannot tell whether a line names
// it, so its approve reads no commit: the plan implementer reviewer writes failure on H once every
// step has passed there, naming the change's one commit.
func TestLegionVerdictAReviewerWhoseClaimRecordsNoSessionReadsNoCommit(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
		seedCapture(t, pool, phase.Reviewing, "needs_review", testerPassAtCode, reviewerWithout)
		clearCaptureMergeState(t, pool)
		seedPlan(t, pool, 2, "implementer", "reviewer")
		seedStepStart(t, pool, "a", 1, captureImplementer, true)
		seedClaimSession(t, pool, captureImplementer, claim.RoleImplementer, captureImplementerSession)
		seedChecks(t, pool, "code", bothYes)
	})
	rig.apply(t, reviewerVerdict("approve", "code", 1))
	rig.written(t)
	rig.noReader(t, "code", wantNoReaderOpening, "code")
}

// C2: where the fake answers no list of the change's commits, the default steps write failure on H
// once every step has passed there: where the daemon cannot read every commit of the change, no
// commit has a reader, and it says why. So does a list that does not reach H, which is the change of
// another head.
func TestLegionVerdictUnreadCommitsHaveNoReader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		github func(*verdictGitHub)
	}{
		{"GitHub answers no list", func(g *verdictGitHub) { g.compareStatus = http.StatusBadGateway }},
		{"the list does not reach H", func(g *verdictGitHub) { g.history = []string{"code-0"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
			rig.github.mu.Lock()
			tc.github(rig.github)
			rig.github.mu.Unlock()
			rig.apply(t, rig.signOff(t))
			rig.written(t)
			rig.noReader(t, "code", wantUnreadOpening)
			if lines := loggedMessages(t, rig.logs.String(), changeUnreadLine); len(lines) != 1 || lines[0]["head"] != "code" {
				t.Fatalf("%q lines = %v, want one naming code", changeUnreadLine, lines)
			}
		})
	}
}

// changeUnreadLine is what the writer logs when it cannot read the change's commits.
const changeUnreadLine = "verdict: the change's commits could not be read, so no commit has a reader"

// unreadReviewersCommit seeds CAPTURE-1 in accepting on the default steps, every step but the
// owner's acceptance passed at code, where the tester's claim and the owner's record no session:
// the reviewer's own commit code then has no reader once the owner accepts code.
func unreadReviewersCommit(t *testing.T, pool *store.Pool) {
	t.Helper()
	acceptingAtCode(t, pool)
	seedStepStart(t, pool, planwords.DefaultTask, testerStep, captureTester, true)
	dropClaimSessions(t, pool, captureArchitect)
}

// R4 (the run's summary names each commit with no reader, and the merger reads its title): the run's
// title and summary follow the decision of the head, not its conclusion alone, so a new reason of a
// failure whose conclusion stays is written over the run. Here the reviewer's own commit code has
// no reader once the owner accepts it. First the failure stands for another reason: a person's
// request for changes before the owner's acceptance, which names no reason; or the change GitHub
// did not serve, refused (404) or with no answer (502), which a later read serves. Then the writer
// writes the run's title and summary again, naming code.
func TestLegionVerdictANewReasonOfAFailureIsWrittenOverItsRun(t *testing.T) {
	t.Run("a person's request for changes, then a commit with no reader", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, unreadReviewersCommit)
		rig.github.mu.Lock()
		rig.github.messages["code"] = commitOf(captureReviewerSession)
		rig.github.mu.Unlock()
		rig.review(t, personsReview(1, "changes_requested"))
		rig.written(t)
		if run := rig.soleRun(t, "code", "failure"); run.Title != "legion-verdict: failure" {
			t.Fatalf("the run's title is %q, want legion-verdict: failure: a person refused code before every step passed there", run.Title)
		}
		rig.apply(t, rig.signOff(t))
		rig.written(t)
		rig.noReader(t, "code", wantNoReaderOpening, "code")
	})
	for _, status := range []int{http.StatusNotFound, http.StatusBadGateway} {
		t.Run(fmt.Sprintf("a compare GitHub answered %d, then a commit with no reader", status), func(t *testing.T) {
			rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, unreadReviewersCommit)
			rig.github.mu.Lock()
			rig.github.messages["code"] = commitOf(captureReviewerSession)
			rig.github.compareStatus = status
			rig.github.mu.Unlock()
			rig.apply(t, rig.signOff(t))
			rig.written(t)
			rig.github.mu.Lock()
			rig.github.compareStatus = 0
			rig.github.mu.Unlock()
			rig.checkState(t, "code", "k1", verdict.StateYes)
			rig.written(t)
			rig.noReader(t, "code", wantNoReaderOpening, "code")
		})
	}
}

// Every Omp-Session line of a commit's message names a session the Omp-Session test reads, in any
// case of its key and wherever it stands, as a squash can carry it into the body of another: the
// two `legion git` writes, one an amend gave a second line, one a squash's body kept. A line with
// no session, or whose key only begins with Omp-Session, names none.
func TestLegionVerdictReadsEveryOmpSessionLine(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    []string
	}{
		{"work\n\nOmp-Session: s1\nLegion-Role: implementer\n", []string{"s1"}},
		{"seven\n\nOmp-Session: S1\nLegion-Role: tester\nOmp-Session: S2\r\n", []string{"S1", "S2"}},
		{"squashed\n\n* fix\n\nomp-session: s3\n\nmore words\n", []string{"s3"}},
		{"no trailer\n", nil},
		{"work\n\nOmp-Session:\nOmp-Sessions: s4\nOmp-Session: two words\n", nil},
	} {
		if got := ompSessions(tc.message); !slices.Equal(got, tc.want) {
			t.Errorf("ompSessions(%q) = %q, want %q", tc.message, got, tc.want)
		}
	}
}

// planWithNoReviewStep seeds CAPTURE-1 in testing on the plan implementer tester, the implementer's
// step passed under its claim and the tester's step started under its own, with no check state held
// at code; the tester's claim records its session where withSession is set.
func planWithNoReviewStep(withSession bool) func(*testing.T, *store.Pool) {
	return func(t *testing.T, pool *store.Pool) {
		t.Helper()
		seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: captureTester})
		seedPlan(t, pool, 2, "implementer", "tester")
		seedStepStart(t, pool, "a", 1, captureImplementer, true)
		seedStepStart(t, pool, "a", 2, captureTester, false)
		seedClaimSession(t, pool, captureImplementer, claim.RoleImplementer, captureImplementerSession)
		if withSession {
			seedClaimSession(t, pool, captureTester, claim.RoleTester, captureTesterSession)
		}
	}
}

// C2: a plan of the steps implementer tester has no review step. Its testing step passes once each
// check reads yes at H, and the daemon takes that pass as its agent's read (R4's fifth departure):
// with no commit naming a session of the tester's claim it writes success on H, where a commit
// names the tester's session it writes failure there, naming that commit, and where the tester's
// claim records no session it writes failure once each check reads yes. Nothing is written while a
// check holds no state at H.
func TestLegionVerdictAPlanWithNoReviewStep(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withSession bool
		message     string
		success     bool
	}{
		{"no commit names the tester's session", true, "", true},
		{"a commit names the tester's session", true, commitOf(captureTesterSession), false},
		{"the tester's claim records no session", false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, planWithNoReviewStep(tc.withSession))
			if tc.message != "" {
				rig.github.mu.Lock()
				rig.github.messages["code"] = tc.message
				rig.github.mu.Unlock()
			}
			rig.checkState(t, "code", "k1", verdict.StateYes)
			rig.written(t)
			rig.nothingWrittenOn(t, "code")
			rig.checkState(t, "code", "k2", verdict.StateYes)
			rig.written(t)
			if tc.success {
				rig.soleRun(t, "code", "success")
				return
			}
			rig.noReader(t, "code", wantNoReaderOpening, "code")
		})
	}
}

// C2: a plan with no testing step and no review step writes failure on H once each check reads yes
// there: no agent reads its commits. Nothing is written before.
func TestLegionVerdictAPlanWithNoTestingStepAndNoReviewStep(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
		seedCapture(t, pool, phase.Implementing, "in_progress")
		seedPlan(t, pool, 1, "implementer")
		seedStepStart(t, pool, "a", 1, captureImplementer, false)
		seedClaimSession(t, pool, captureImplementer, claim.RoleImplementer, captureImplementerSession)
	})
	rig.checkState(t, "code", "k1", verdict.StateYes)
	rig.written(t)
	rig.nothingWrittenOn(t, "code")
	rig.checkState(t, "code", "k2", verdict.StateYes)
	rig.written(t)
	rig.noReader(t, "code", wantNoReaderOpening, "code")
}

// R19's status half, over a status the daemon holds: once the daemon's own status stands at a head,
// a verdict that turns the head's conclusion sets a status of the daemon's own at the new state over
// it, since GitHub reads the newest status of each context, with no foreign id.
func TestLegionVerdictSetsItsOwnStatusAgainWhenTheConclusionTurns(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
	rig.github.mu.Lock()
	rig.github.statuses = append(rig.github.statuses, fakeStatus{ID: 600, Head: "code", Context: verdict.CheckName, State: "failure", Creator: verdictAppLogin})
	rig.github.mu.Unlock()
	rig.acceptedAndWritten(t)
	rig.review(t, personsReview(1, "changes_requested"))
	rig.written(t)
	rig.github.mu.Lock()
	statuses := slices.Clone(rig.github.statuses)
	rig.github.mu.Unlock()
	var states []string
	for _, status := range statuses {
		states = append(states, status.State)
	}
	if !slices.Equal(states, []string{"failure", "success", "failure"}) {
		t.Fatalf("the legion-verdict statuses on code, oldest first, are %v; want the foreign failure, the daemon's success over it, then the daemon's failure", states)
	}
	lines := rig.writeLines(t)
	if last := lines[len(lines)-1]; last["kind"] != "status" || last["conclusion"] != "failure" || last["foreign_id"] != float64(0) {
		t.Fatalf("the last legion_verdict_write line = %v, want the daemon's own status at failure, over no foreign one", last)
	}
}

// A review names the commit it read (commit_id), and that is the one head it is a verdict at, as
// the workflow's refusal holds it (workflow's review): a review that names none is recorded at no
// head and asks for no write, rather than at the head the pull request stood at when GitHub sent it.
func TestLegionVerdictAReviewNamingNoCommitIsNoVerdict(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded)
	rig.apply(t, outsideOpened)
	review := outsideReview(1, "a-writer", "approved")
	review.CommitID = ""
	rig.review(t, review)
	rig.written(t)
	if runs := rig.github.legionRuns("out-1"); len(runs) != 0 {
		t.Fatalf("legion-verdict runs on out-1 = %+v, want none from a review that names no commit", runs)
	}
	if reviews := verdictReviews(t, rig.pool, 90); len(reviews) != 0 {
		t.Fatalf("the reviews recorded of pull request 90 = %+v, want none", reviews)
	}
}

// canceledContext is a context already ended.
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// The repositories GitHub intake hears include every verdict repository, so the events of one the
// mapping does not name reach the writer.
func TestLegionVerdictRepositoriesAreHeard(t *testing.T) {
	pool := isolatedOutboxPool(t)
	w := &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "CAPTURE"}, repositories: servedAcme(),
		verdicts: newVerdictWriter([]config.VerdictRepository{widgetsEntry, {Repository: ghrepo.MustParse("acme/Verdict-Scratch")}}, verdictAppID, slog.New(slog.DiscardHandler))}
	heard, err := w.heardAtBoot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, repository := range heard {
		names = append(names, repository.String())
	}
	if want := []string{"acme/Verdict-Scratch", "acme/widgets"}; !slices.Equal(names, want) {
		t.Fatalf("heard = %v, want %v: the served repository and the verdict repository the mapping does not name", names, want)
	}
}
