package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// limitedGitHub is a GitHub REST API whose GET /repos/<owner>/<name> answers each repository named
// names as "<GitHub's spelling>@<default branch>", as newNamedGitHub's does, unless refuse answers
// the read itself: refuse is called with the repository, lowercased, and the read's number among
// that repository's reads, from 1. Every other request is answered 404. It keeps when each read of
// each named repository arrived.
type limitedGitHub struct {
	url   string
	mu    sync.Mutex
	reads map[string][]time.Time
}

func newLimitedGitHub(t *testing.T, named map[string]string, refuse func(w http.ResponseWriter, repo string, read int) bool) *limitedGitHub {
	t.Helper()
	g := &limitedGitHub{reads: map[string][]time.Time{}}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		repo := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		answer, ok := named[repo]
		if r.Method != http.MethodGet || !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`)
			return
		}
		g.mu.Lock()
		g.reads[repo] = append(g.reads[repo], time.Now())
		read := len(g.reads[repo])
		g.mu.Unlock()
		if refuse(w, repo, read) {
			return
		}
		name, branch, _ := strings.Cut(answer, "@")
		_, _ = io.WriteString(w, `{"full_name":"`+name+`","default_branch":"`+branch+`"}`)
	}})
	return g
}

// readsOf is when each read of repo arrived, in order.
func (g *limitedGitHub) readsOf(repo string) []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.reads[repo])
}

// spentLimit answers a read with GitHub's 403 for a spent primary rate limit: none of the
// installation's 15000 calls on the core budget left until reset, an epoch second.
func spentLimit(w http.ResponseWriter, reset int64) {
	w.Header().Set("X-RateLimit-Limit", "15000")
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
	w.Header().Set("X-RateLimit-Resource", "core")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 7.","status":"403"}`)
}

// spendsOnce is a refuse for newLimitedGitHub that answers the first read of repo with spentLimit,
// its reset ahead of that read, which it stores in reset, and lets every later read be answered.
func spendsOnce(repo string, ahead time.Duration, reset *atomic.Int64) func(w http.ResponseWriter, repo string, read int) bool {
	limited := repo
	return func(w http.ResponseWriter, repo string, read int) bool {
		if repo != limited || read > 1 {
			return false
		}
		reset.Store(time.Now().Add(ahead).Unix())
		spentLimit(w, reset.Load())
		return true
	}
}

// secondaryLimit answers a read with GitHub's 429 for its secondary rate limit, asking for a
// second's wait.
func secondaryLimit(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
}

// limitedRig is an option of repositoryRigWith that has the rig's runtime read each repository's
// default branch from github and log to logs, as JSON, as the boot wires it (heldThroughLimit told
// of each read answered from the branch held).
func limitedRig(t *testing.T, github *limitedGitHub, logs *syncBuffer) func(*workflowRuntime) {
	t.Helper()
	return func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
		w.defaultBranches.onHold = w.heldThroughLimit
		w.log = slog.New(slog.NewJSONHandler(logs, nil))
	}
}

// doneOn is a done issue of project, recorded on repo at the default branch branch.
func doneOn(key, project, repo, branch string) record.Issue {
	return record.Issue{Key: key, Tree: key, Project: project, Title: key, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done",
		Rank: "V", Repo: repo, DefaultBranch: branch, GitHubRepo: repo}
}

// limitedBoth is the rig's two repositories as GitHub answers them once it answers: zeta/tools's
// default branch is trunk, where the anchor on it holds main.
var limitedBoth = map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@trunk"}

// loggedAt is when the JSON log line line was written.
func loggedAt(t *testing.T, line map[string]any) time.Time {
	t.Helper()
	stamp, _ := line["time"].(string)
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("read the time of the log line %v: %v", line, err)
	}
	return at
}

// heldLine is the line a repository served on a default branch the daemon holds in place of a
// rate-limited read logs (heldThroughLimit).
const heldLine = "so the daemon serves it on the default branch it holds"

// preparedLine is the line the refresher's first job logs once it has prepared the repositories
// the records name (prepareAtStart).
const preparedLine = "legion workflow prepared the repositories its records name"

// linesFor is each of lines that names repo.
func linesFor(lines []map[string]any, repo string) []map[string]any {
	var named []map[string]any
	for _, line := range lines {
		if line["repo"] == repo {
			named = append(named, line)
		}
	}
	return named
}

// heldAdmission prepares an unrecorded todo of the rig's project handed to Legion on repo, as the
// Dispatch consumer prepares one (prepareUnrecorded), and requires it prepared with no error: what
// admission reads of the repository's default branch.
func heldAdmission(t *testing.T, w *workflowRuntime, repo string) {
	t.Helper()
	if _, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "LEGION-2", Status: "todo", HandedOver: true, ResolvedRepo: repo}, false); err != nil {
		t.Fatalf("prepare an admission on %s: %v, want it prepared on the branch held", repo, err)
	}
}

// The refresher's first job, whose read of a repository's default branch GitHub refuses for a
// spent rate limit, serves the repository at once on the default branch a record of the project
// holds for it, and the boot waited for none of it: the limit says nothing about the repository,
// and the record's branch is the one GitHub named when the issue was recorded. A read of the branch
// and an admission's preparation inside the limit are answered from the held branch and ask GitHub
// nothing, and the first read after the reset, the refresher's fourth job's, is the read the refused
// one owed: here GitHub has renamed the default branch since, and that read serves the new name.
func TestABootServesARecordedDefaultBranchThroughARateLimitAndReadsItAgainAfterTheReset(t *testing.T) {
	var reset atomic.Int64
	github := newLimitedGitHub(t, limitedBoth, spendsOnce("zeta/tools", 6*time.Second, &reset))
	logs := &syncBuffer{}
	w, _, _, _ := repositoryRigWith(t, &installations{}, limitedRig(t, github, logs))
	resetAt := time.Unix(reset.Load(), 0)

	testwait.Eventually(t, "the first job's line", func() bool { return len(loggedLines(t, logs.String(), preparedLine)) == 1 })
	prepared := loggedLines(t, logs.String(), preparedLine)[0]
	if at := loggedAt(t, prepared); !at.Before(resetAt) {
		t.Fatalf("the first job prepared the records' repositories at %s, want before the limit's reset at %s: it waits for nothing a record holds", at, resetAt)
	}
	if repos, _ := prepared["repositories"].([]any); !slices.Contains(repos, any("zeta/tools")) || !slices.Contains(repos, any("acme/widgets")) {
		t.Fatalf("the first job prepared %v, want acme/widgets and zeta/tools", prepared["repositories"])
	}
	if !recordsOn(w, "zeta/tools") || !recordsOn(w, "acme/widgets") {
		t.Fatalf("recorded on zeta/tools %v, acme/widgets %v; want both", recordsOn(w, "zeta/tools"), recordsOn(w, "acme/widgets"))
	}
	held := loggedLines(t, logs.String(), heldLine)
	if len(held) != 1 || held[0]["repo"] != "zeta/tools" || held[0]["defaultBranch"] != "main" || held[0]["rateLimit"] != "15000" ||
		held[0]["rateLimitRemaining"] != "0" || held[0]["rateLimitReset"] != strconv.FormatInt(reset.Load(), 10) {
		t.Fatalf("the held repository's lines = %v, want one naming zeta/tools at the recorded main and GitHub's limit headers", held)
	}

	if got, err := w.defaultBranches.Named(context.Background(), ghrepo.MustParse("zeta/tools")); err != nil || got.DefaultBranch != "main" {
		t.Fatalf("a read of zeta/tools inside the limit = %+v, %v; want the held main", got, err)
	}
	heldAdmission(t, w, "zeta/tools")
	if !time.Now().Before(resetAt) {
		t.Fatalf("the reads inside the limit ran past its reset at %s, so they prove nothing of it", resetAt)
	}
	if reads := github.readsOf("zeta/tools"); len(reads) != 1 {
		t.Fatalf("GitHub was read for zeta/tools %d times by the reads inside the limit, want only the one it refused", len(reads))
	}

	testwait.Eventually(t, "zeta/tools read from GitHub again after the limit's reset", func() bool {
		kept, ok := w.defaultBranches.Kept("zeta/tools")
		return ok && kept.DefaultBranch == "trunk"
	})
	if reads := github.readsOf("zeta/tools"); len(reads) != 2 || reads[1].Before(resetAt) || reads[1].After(resetAt.Add(10*time.Second)) {
		t.Fatalf("GitHub was read for zeta/tools at %v, want the refused read and one more soon after the reset at %s", reads, resetAt)
	}
	if !recordsOn(w, "zeta/tools") {
		t.Fatal("zeta/tools is not recorded on after its read again")
	}
}

// A preparation whose read of a repository's default branch GitHub refuses for a spent rate limit,
// where no record holds a branch for the repository — the launch of an issue no record names —
// keeps the limit as the repository's read error and refuses that launch alone for now, naming the
// limit, rather than holding a branch nothing recorded (holdLimited answering none held). Nothing
// is held, so the next launch reads GitHub once more and works the repository on its answer.
func TestALaunchWithNoDefaultBranchHeldKeepsARateLimitAsItsReadErrorAndReadsOnceMore(t *testing.T) {
	ctx := context.Background()
	var reset atomic.Int64
	github := newLimitedGitHub(t, limitedBoth, spendsOnce("zeta/tools", 2*time.Second, &reset))
	client := &repositoryDispatch{issues: map[string]dispatch.Issue{"LEGION-5": {Key: "LEGION-5", Status: "todo", ResolvedRepo: "zeta/tools"}}}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: projectRecords{Store: record.NewStore(), project: "LEGION"}, dispatch: client,
		dispatchProject: "LEGION", installs: &installations{}, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(),
		setRepositories: hearsNothing}
	w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
	w.defaultBranches.onHold = w.heldThroughLimit

	_, _, err := w.launchRepository(ctx, "LEGION-5")
	if err == nil || !strings.Contains(err.Error(), "the name and default branch of zeta/tools could not be read") || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("the launch inside the limit = %v, want it refused for now, naming the limit", err)
	}
	if entry := w.repositories.entryOf("zeta/tools"); !strings.Contains(entry.readError, "the name and default branch of zeta/tools could not be read") {
		t.Fatalf("zeta/tools' entry = %+v, want the limit kept as its read error", entry)
	}
	if _, kept := w.defaultBranches.Kept("zeta/tools"); kept {
		t.Fatal("a default branch is held for zeta/tools, want none: no record names it")
	}
	repo, branch, err := w.launchRepository(ctx, "LEGION-5")
	if err != nil || repo.String() != "zeta/tools" || branch != "trunk" {
		t.Fatalf("the next launch = %s@%s, %v; want GitHub's zeta/tools@trunk", repo, branch, err)
	}
	if reads := github.readsOf("zeta/tools"); len(reads) != 2 {
		t.Fatalf("GitHub was read for zeta/tools at %v, want the refused read and one more by the next launch", reads)
	}
}

// A repository GitHub still refuses for its secondary rate limit after the first wait is served on
// the branch a record holds throughout, rather than left out: the refresher's fourth job reads it
// again at each wait GitHub asks for, and each refusal holds the branch again and says so, naming
// the Retry-After and the secondary budget. The other repository is served throughout, and once
// GitHub answers, the read takes GitHub's branch, with no restart.
func TestARepositoryGitHubStillRateLimitsIsServedOnItsHeldBranchUntilGitHubAnswers(t *testing.T) {
	var limiting atomic.Bool
	limiting.Store(true)
	github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, _ int) bool {
		if repo != "zeta/tools" || !limiting.Load() {
			return false
		}
		secondaryLimit(w)
		return true
	})
	logs := &syncBuffer{}
	w, _, _, _ := repositoryRigWith(t, &installations{}, limitedRig(t, github, logs))
	if !recordsOn(w, "zeta/tools") || !recordsOn(w, "acme/widgets") {
		t.Fatalf("recorded on zeta/tools %v, acme/widgets %v; want both", recordsOn(w, "zeta/tools"), recordsOn(w, "acme/widgets"))
	}
	testwait.Eventually(t, "zeta/tools read again after the wait GitHub asked for and held again", func() bool {
		return len(linesFor(loggedLines(t, logs.String(), heldLine), "zeta/tools")) >= 2
	})
	for _, line := range linesFor(loggedLines(t, logs.String(), heldLine), "zeta/tools") {
		if line["retryAfter"] != "1" || line["budget"] != "secondary" || line["defaultBranch"] != "main" {
			t.Fatalf("held line = %v, want it naming the Retry-After, the secondary budget and the recorded main", line)
		}
	}
	if reads := github.readsOf("zeta/tools"); len(reads) < 2 || reads[1].Sub(reads[0]) < time.Second {
		t.Fatalf("GitHub was read for zeta/tools at %v, want the read again a second after the refused one, as Retry-After asked", reads)
	}
	if !recordsOn(w, "zeta/tools") || !recordsOn(w, "acme/widgets") {
		t.Fatal("a repository is no longer recorded on while GitHub's limit refuses zeta/tools, want both served throughout")
	}
	limiting.Store(false)
	testwait.Eventually(t, "zeta/tools read from GitHub once its limit lets it", func() bool {
		kept, ok := w.defaultBranches.Kept("zeta/tools")
		return ok && kept.DefaultBranch == "trunk"
	})
}

// A read after a rate limit that GitHub does not answer at all — the connection closed with no
// answer — refuses nothing: the repository stays recorded on at the branch its record holds, the
// failure is logged naming it, and the next read asks GitHub again. GitHub said nothing about the
// repository.
func TestAReadAfterARateLimitThatGetsNoAnswerLeavesTheRepositoryServed(t *testing.T) {
	github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, read int) bool {
		switch {
		case repo != "zeta/tools":
			return false
		case read == 1:
			secondaryLimit(w)
			return true
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("take the connection to close it unanswered: %v", err)
			return true
		}
		_ = conn.Close()
		return true
	})
	logs := &syncBuffer{}
	w, _, _, _ := repositoryRigWith(t, &installations{}, limitedRig(t, github, logs))
	failed := "the read of a default branch held through GitHub's rate limit failed after the limit"
	testwait.Eventually(t, "the read after the limit failing, logged", func() bool {
		return len(linesFor(loggedLines(t, logs.String(), failed), "zeta/tools")) != 0
	})
	if !recordsOn(w, "zeta/tools") || !recordsOn(w, "acme/widgets") {
		t.Fatalf("recorded on zeta/tools %v, acme/widgets %v; want both: the unanswered read refuses nothing", recordsOn(w, "zeta/tools"), recordsOn(w, "acme/widgets"))
	}
}

// GitHub's 403 naming its rate limit in the body alone, with thousands of calls left in its
// headers, is the limit to the read of a default branch: the refresher's first job serves the
// repository on the default branch a record holds, a read and an admission inside the minute
// GitHub's documentation waits such a limit ask GitHub nothing, and the line names the headers as
// GitHub sent them. Read as a refusal of the repository, the answer left it unrecorded on.
func TestABootReadsARateLimitNamedOnlyByItsMessageAsARateLimit(t *testing.T) {
	github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, read int) bool {
		if repo != "zeta/tools" || read > 1 {
			return false
		}
		w.Header().Set("X-RateLimit-Limit", "15000")
		w.Header().Set("X-RateLimit-Remaining", "14671")
		w.Header().Set("X-RateLimit-Resource", "core")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 7. If you reach out to GitHub Support for help, please include the request ID 0000:0000:0000000:0000000:00000000.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api","status":"403"}`)
		return true
	})
	logs := &syncBuffer{}
	w, _, _, _ := repositoryRigWith(t, &installations{}, limitedRig(t, github, logs))

	held := loggedLines(t, logs.String(), heldLine)
	if len(held) != 1 || held[0]["repo"] != "zeta/tools" || held[0]["defaultBranch"] != "main" || held[0]["rateLimitRemaining"] != "14671" || held[0]["wait"] != "1m0s" {
		t.Fatalf("the held repository's lines = %v, want one naming zeta/tools at the recorded main, GitHub's 14671 calls left and a minute's wait", held)
	}
	if got, err := w.defaultBranches.Named(context.Background(), ghrepo.MustParse("zeta/tools")); err != nil || got.DefaultBranch != "main" {
		t.Fatalf("a read of zeta/tools inside the limit = %+v, %v; want the held main", got, err)
	}
	heldAdmission(t, w, "zeta/tools")
	if reads := github.readsOf("zeta/tools"); len(reads) != 1 {
		t.Fatalf("GitHub was read for zeta/tools %d times, want the one refused read and none inside the limit", len(reads))
	}
	if !recordsOn(w, "zeta/tools") {
		t.Fatal("zeta/tools is not recorded on, want it served at the recorded main")
	}
}

// A 404 for a repository's default branch is GitHub's answer about the repository, and a 500 is
// GitHub's trouble; neither is a rate limit. The refresher's first job reads each once and keeps it
// as the repository's read error, naming GitHub's answer, which legion state shows and the
// refresher's second job reads again; it holds no branch for it, says no hold, and refuses no boot:
// the other repository is recorded on.
func TestADefaultBranchRead404Or500IsKeptAsTheRepositorysReadError(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, _ int) bool {
				if repo != "zeta/tools" {
					return false
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"message":"no such repository"}`)
				return true
			})
			logs := &syncBuffer{}
			w, _, _, _ := repositoryRigWith(t, &installations{}, limitedRig(t, github, logs))
			want := fmt.Sprintf("GitHub answered GET /repos/zeta/tools with %d", status)
			if entry := w.repositories.entryOf("zeta/tools"); !strings.Contains(entry.readError, "the name and default branch of zeta/tools could not be read") || !strings.Contains(entry.readError, want) {
				t.Fatalf("zeta/tools' read error = %q, want GitHub's answer, %q", entry.readError, want)
			}
			if recordsOn(w, "zeta/tools") || !recordsOn(w, "acme/widgets") {
				t.Fatalf("recorded on zeta/tools %v, acme/widgets %v; want acme/widgets alone", recordsOn(w, "zeta/tools"), recordsOn(w, "acme/widgets"))
			}
			if got := len(github.readsOf("zeta/tools")); got != 1 {
				t.Fatalf("GitHub was read for zeta/tools %d times, want once", got)
			}
			if held := loggedLines(t, logs.String(), heldLine); len(held) != 0 {
				t.Fatalf("held lines = %v, want none: neither answer is a rate limit", held)
			}
		})
	}
}

// A record holds a branch only for the repository it names itself, in its own spelling, GitHub's:
// a limited read of the name a record's repository moved to under a configured rename is not held
// from that record, since neither the record nor the configured pair, which holds the pair's
// spelling as configured, holds GitHub's spelling of it, and intake's subjects are case-sensitive.
// It is kept as a read that did not answer, as any repository with nothing held is.
func TestARecordedDefaultBranchIsHeldOnlyForTheRepositoryTheRecordNames(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := projectRecords{Store: record.NewStore(), project: "LEGION"}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, doneOn("LEGION-1", "LEGION", "Old/Widgets", "main"))
	}); err != nil {
		t.Fatal(err)
	}
	renames := ghrepo.MustRenames(ghrepo.MustRename("old/widgets", "New/Widgets"))
	for _, tc := range []struct {
		name string
		read string
		held string
	}{
		{name: "the name the record keeps, in another case", read: "old/widgets", held: "Old/Widgets"},
		{name: "the name a configured rename moved it to, as configured", read: "New/Widgets"},
		{name: "the name a configured rename moved it to, in another case", read: "new/widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repos := newRepositories("LEGION", renames)
			w := &workflowRuntime{pool: pool, records: records, repositories: repos, log: quietLogger(),
				defaultBranches: newDefaultBranches(roleTokens{}, repos, "", defaultBranchTTL, time.Now)}
			limited := limitedRead{repo: ghrepo.MustParse(tc.read), answer: &githubrest.Answer{Status: http.StatusForbidden},
				wait: time.Minute, until: time.Now().Add(time.Minute)}
			held, ok, err := w.holdLimited(limited, w.onceRecordedBranches(ctx))
			if err != nil {
				t.Fatal(err)
			}
			if tc.held == "" {
				if _, kept := w.defaultBranches.Kept(tc.read); ok || kept {
					t.Fatalf("held %v (%v), kept %v; want %s unheld and nothing kept", held, ok, kept, tc.read)
				}
				return
			}
			if !ok || held.Name.String() != tc.held {
				t.Fatalf("held %v (%v); want %s served as %s", held, ok, tc.read, tc.held)
			}
			if kept, ok := w.defaultBranches.Kept(tc.read); !ok || kept.Name.String() != tc.held || kept.DefaultBranch != "main" {
				t.Fatalf("the branch held for %s = %+v, %v; want %s at main", tc.read, kept, ok, tc.held)
			}
		})
	}
}

// The boot's backfill of records from before the record carried its repository leaves a record
// whose read GitHub's rate limit refuses, or GitHub does not answer, as it is, saying so in one line
// that names the issue, the repository and the cause, and the boot goes on (INFRA-4401 D11): it
// waits on GitHub for no record. Every other record is filled. The refresher's second job then
// tries the record again (backfillLater) and fills it once GitHub answers, and GitHub intake hears
// the filled record's repository as it hears a new record's (hear), by one change of the set its
// decoder keeps; the read of the live records is asked for, so the record joins the live set and
// the cache set.
func TestABootsBackfillLeavesARecordGitHubDoesNotAnswerForAndTheRefresherFillsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(w http.ResponseWriter)
	}{
		{name: "a rate limit asking for a second", refuse: secondaryLimit},
		{name: "a read GitHub does not answer", refuse: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"message":"Server Error"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				for _, key := range []string{"LEGION-7", "LEGION-9"} {
					if _, err := tx.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
						values ($1, $1, 'LEGION', $1, 'planning', 1, 'in_progress', 'A', 1)`, key); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var answering atomic.Bool
			github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, _ int) bool {
				if repo != "zeta/tools" || answering.Load() {
					return false
				}
				tc.refuse(w)
				return true
			})
			logs := &syncBuffer{}
			var published [][]ghrepo.Repository
			w := &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"}, repositories: newRepositories("LEGION", ghrepo.Renames{}),
				dispatch:        &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-7", ResolvedRepo: "zeta/tools"}, {Key: "LEGION-9", ResolvedRepo: "acme/widgets"}}},
				dispatchProject: "LEGION", stateDir: t.TempDir(), liveWake: make(chan struct{}, 1), log: slog.New(slog.NewJSONHandler(logs, nil)),
				defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now),
				setRepositories: func(_ context.Context, repos []ghrepo.Repository) error {
					published = append(published, repos)
					return nil
				}}
			filled, left, err := backfillRepositories(ctx, w.pool, w.records, w.dispatch, "LEGION", w.stateDir, w.defaultBranches, ghrepo.Renames{}, w.log)
			if err != nil || !slices.Equal(repositoryNames(filled), []string{"acme/widgets"}) || !left {
				t.Fatalf("backfill = filled %v, left %v, %v; want LEGION-9 filled on acme/widgets, LEGION-7 left and the boot going on", filled, left, err)
			}
			if unfilled := unfilledKeys(t, w); !slices.Equal(unfilled, []string{"LEGION-7"}) {
				t.Fatalf("unfilled after the boot's backfill = %v, want LEGION-7 alone", unfilled)
			}
			if got := len(github.readsOf("zeta/tools")); got != 1 {
				t.Fatalf("the boot's backfill read zeta/tools %d times, want once: it waits on GitHub for no record", got)
			}
			leaves := loggedLines(t, logs.String(), "a record is left as it is, without its repository columns")
			if len(leaves) != 1 || leaves[0]["issue"] != "LEGION-7" || leaves[0]["repo"] != "zeta/tools" || leaves[0]["cause"] == nil {
				t.Fatalf("leave lines = %v, want one naming LEGION-7, zeta/tools and the cause", leaves)
			}
			if _, err := w.heardAtBoot(ctx); err != nil {
				t.Fatalf("the boot's subscription after the backfill = %v, want what the records name heard", err)
			}

			answering.Store(true)
			w.backfillPending.Store(left)
			w.backfillLater(ctx)
			if unfilled := unfilledKeys(t, w); len(unfilled) != 0 {
				t.Fatalf("unfilled after the refresher's backfill = %v, want none", unfilled)
			}
			if w.backfillPending.Load() {
				t.Fatal("the backfill is still pending after the refresher filled the record, want nothing left")
			}
			if heard := repositoryNames(*w.heard.Load()); !slices.Equal(heard, []string{"acme/widgets", "zeta/tools"}) || len(published) != 1 {
				t.Fatalf("GitHub intake hears %v after %d publications, want zeta/tools added to the boot's acme/widgets by one", heard, len(published))
			}
			if len(w.liveWake) != 1 {
				t.Fatal("the read of the live records was not asked for, want it: the filled record joins the live set and the cache set")
			}
		})
	}
}

// The refresher's first job reads the repositories the records name repositoryReads at a time:
// here more records than that each name a repository of their own, and GitHub, holding each read of
// a default branch a moment, never has more than repositoryReads in flight, and every repository is
// recorded on.
func TestTheFirstJobReadsTheRecordsRepositoriesRepositoryReadsAtATime(t *testing.T) {
	ctx := context.Background()
	var inFlight, most atomic.Int64
	named := map[string]string{}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: projectRecords{Store: record.NewStore(), project: "LEGION"}, dispatch: &repositoryDispatch{},
		dispatchProject: "LEGION", installs: &installations{}, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
	var repos []string
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		for index := range repositoryReads + 4 {
			repo := "acme/r" + strconv.Itoa(index)
			named[repo], repos = repo+"@main", append(repos, repo)
			if err := w.records.PutIssue(ctx, tx, doneOn(fmt.Sprintf("LEGION-%d", index+1), "LEGION", repo, "main")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	github := newLimitedGitHub(t, named, func(http.ResponseWriter, string, int) bool {
		now := inFlight.Add(1)
		for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return false
	})
	w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
	w.prepareAtStart(ctx)
	for _, repo := range repos {
		if !recordsOn(w, repo) {
			t.Fatalf("%s is not recorded on after the first job, want every repository the records name", repo)
		}
	}
	if got := most.Load(); got > repositoryReads {
		t.Fatalf("GitHub held %d reads at once, want at most %d", got, repositoryReads)
	}
}

// liveBootDispatch is a Dispatch for a daemon's boot whose project lists listed, answering every
// other call every boot makes (servedBootCall).
func liveBootDispatch(t *testing.T, project string, listed []map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch {
		case servedBootCall(t, w, r, project):
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			body = listed
		default:
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write %s: %v", r.URL.Path, err)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// listedAs is an unlabelled issue as Dispatch's listing shows it, in status on repo.
func listedAs(key, status, repo string) map[string]any {
	return map[string]any{"key": key, "title": key, "status": status, "labels": []string{}, "resolved_repo": repo}
}

// recordBeforeBoot records issues in cfg's database, migrated first, as an earlier boot of the
// project left them.
func recordBeforeBoot(t *testing.T, cfg config.Config, issues ...record.Issue) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		for _, issue := range issues {
			if err := record.NewStore().PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the project's issues before the boot: %v", err)
	}
}

// The daemon itself, with a record of an issue on its repository, boots through a spent rate limit
// at once: GitHub refuses the refresher's first read of the repository's default branch with a 403
// naming no call left until a reset half a minute away, and the daemon answers /healthz long
// before it, serving the repository on the recorded branch once its first job has run.
func TestABootWithARecordedDefaultBranchComesUpAtOnceThroughARateLimit(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	recorded := cfg.Project + "-1"
	cfg.DispatchURL = liveBootDispatch(t, cfg.Project, []map[string]any{listedAs(recorded, "done", "acme/widgets")})
	recordBeforeBoot(t, cfg, doneOn(recorded, cfg.Project, "acme/widgets", "main"))
	var reset atomic.Int64
	github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}, spendsOnce("acme/widgets", 30*time.Second, &reset))
	logs := &syncBuffer{}
	if err := bootsOrExitsOn(t, cfg, &workflowTokenRecorder{}, github.url, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to come up on the recorded default branch", err)
	}
	testwait.Eventually(t, "the first job holding acme/widgets's recorded branch", func() bool {
		return len(linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets")) != 0
	})
	resetAt := time.Unix(reset.Load(), 0)
	if !time.Now().Before(resetAt) {
		t.Fatalf("the daemon served acme/widgets on its held branch at or after the limit's reset at %s, want before it: it waits for nothing it holds", resetAt)
	}
	if held := linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets"); len(held) != 1 || held[0]["defaultBranch"] != "main" {
		t.Fatalf("held lines for acme/widgets = %v, want one naming the recorded main", held)
	}
	if reads := github.readsOf("acme/widgets"); len(reads) != 1 {
		t.Fatalf("GitHub was read for acme/widgets %d times, want only the one it refused", len(reads))
	}
}

// A held branch outlives its own reset: the refresher's first job holds acme/widgets's recorded
// branch through a limit that asks for a second's wait, and after that reset a read of the default
// branch that GitHub's limit refuses again — an admission's, or the refresher's own — is answered
// from the branch held, saying so again, where a refusal would leave the read with nothing.
func TestAHeldDefaultBranchAnswersAReadAfterTheHoldsReset(t *testing.T) {
	github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, _ int) bool {
		if repo != "acme/widgets" {
			return false
		}
		// Every read of acme/widgets is refused, each asking for a second's wait: relative, so
		// the hold always ends a second after its read, never at an epoch second already past.
		secondaryLimit(w)
		return true
	})
	logs := &syncBuffer{}
	w, _, _, _ := repositoryRigWith(t, &installations{}, limitedRig(t, github, logs))
	reads := github.readsOf("acme/widgets")
	if len(reads) == 0 || !recordsOn(w, "acme/widgets") {
		t.Fatalf("GitHub was read for acme/widgets at %v, recorded on %v; want the first job's read, held", reads, recordsOn(w, "acme/widgets"))
	}
	first := reads[0]
	testwait.Eventually(t, "acme/widgets refused again after its hold's reset and held again", func() bool {
		return len(linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets")) >= 2
	})
	if got, err := w.defaultBranches.Named(context.Background(), ghrepo.MustParse("acme/widgets")); err != nil || got.DefaultBranch != "main" {
		t.Fatalf("a read of acme/widgets after its hold's reset = %+v, %v; want the held main", got, err)
	}
	heldAdmission(t, w, "acme/widgets")
	if reads := github.readsOf("acme/widgets"); len(reads) < 2 || reads[1].Before(first.Add(time.Second)) {
		t.Fatalf("GitHub was read for acme/widgets at %v, want a read again after the first hold's reset", reads)
	}
	if held := linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets"); len(held) < 2 {
		t.Fatalf("held lines for acme/widgets = %v, want the first job's and each read refused after the reset", held)
	}
}

// The daemon itself boots through a spent rate limit on the repository its record names: GitHub
// refuses the refresher's first read of it with a 403 naming no call left until a reset three
// seconds away, and then answers. The daemon comes up at once, holds the record's branch and reads
// the repository once more soon after the reset, where before it exited after its appauth stage
// with GitHub's refusal.
func TestABootThroughASpentRateLimitComesUp(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	recorded := cfg.Project + "-1"
	cfg.DispatchURL = liveBootDispatch(t, cfg.Project, []map[string]any{listedAs(recorded, "done", "acme/widgets")})
	recordBeforeBoot(t, cfg, doneOn(recorded, cfg.Project, "acme/widgets", "main"))
	var reset atomic.Int64
	github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}, spendsOnce("acme/widgets", 3*time.Second, &reset))
	logs := &syncBuffer{}
	if err := bootsOrExitsOn(t, cfg, &workflowTokenRecorder{}, github.url, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to come up through GitHub's rate limit", err)
	}
	testwait.Eventually(t, "acme/widgets read once more after the limit's reset", func() bool { return len(github.readsOf("acme/widgets")) >= 2 })
	resetAt := time.Unix(reset.Load(), 0)
	if reads := github.readsOf("acme/widgets"); reads[1].Before(resetAt) || reads[1].After(resetAt.Add(5*time.Second)) {
		t.Fatalf("GitHub was read for acme/widgets at %v, want the refused read and one more soon after the reset at %s", reads, resetAt)
	}
	if held := linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets"); len(held) != 1 {
		t.Fatalf("held lines for acme/widgets = %v, want one", held)
	}
}

// An event's preparation whose read of a repository's default branch GitHub refuses for its rate
// limit, the daemon holding no read of the repository, is delivered again after the wait GitHub
// asked for (an intake.RetryLater), not a second later into the same limit, each delivery reading
// GitHub again: a 429's Retry-After, and the minute a 403 naming the limit in its body alone waits.
// The limit is kept in the one write of the repository's entry the preparation makes (prepareName),
// as the refresher's and a launch's are, so the entry names it as its read error until a read
// answers; Dispatch is asked nothing of its webhook App, since GitHub named no repository.
func TestARateLimitedDefaultBranchReadDeliversTheEventAgainAfterGitHubsWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(w http.ResponseWriter)
		want   time.Duration
	}{
		{name: "a 429 asking for 17 s", want: 17 * time.Second, refuse: func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "17")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
		}},
		{name: "a 403 naming the limit in its body alone", want: time.Minute, refuse: func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "14671")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 7."}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, _ string, _ int) bool {
				tc.refuse(w)
				return true
			})
			w := &workflowRuntime{installs: &installations{}, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(),
				defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
			_, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "LEGION-2", Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools"}, false)
			var later *intake.RetryLater
			if !errors.As(err, &later) || later.After != tc.want {
				t.Fatalf("prepare = %v, want an intake.RetryLater after %s", err, tc.want)
			}
			if got := len(github.readsOf("zeta/tools")); got != 1 {
				t.Fatalf("GitHub was read for zeta/tools %d times, want once", got)
			}
			if entry := w.repositories.entryOf("zeta/tools"); !entry.served() || !strings.Contains(entry.readError, "the name and default branch of zeta/tools could not be read") {
				t.Fatalf("zeta/tools' entry = %+v, want both Apps installed and the limit as its read error", entry)
			}
		})
	}
}

// A read of a default branch after the start keeps the wait GitHub names, whatever the start's
// maximum (c-boot-mint-rate-limit case 10, the maximum shortened to 5 s): an event's preparation
// whose read a 429 refuses, asking for 17 s, is delivered again after 17 s, not 5 s.
func TestAnEventsDefaultBranchReadAfterTheStartStillWaitsTheRetryAfterGitHubNames(t *testing.T) {
	withStartRateLimits(t, func(policy *rateLimitPolicy) { policy.Max = 5 * time.Second })
	github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, _ string, _ int) bool {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
		return true
	})
	w := &workflowRuntime{installs: &installations{}, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(),
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	_, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "LEGION-2", Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools"}, false)
	var later *intake.RetryLater
	if !errors.As(err, &later) || later.After != 17*time.Second {
		t.Fatalf("prepare = %v, want an intake.RetryLater after GitHub's 17s", err)
	}
}
