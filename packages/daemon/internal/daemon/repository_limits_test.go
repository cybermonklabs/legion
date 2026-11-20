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

// limitedRig is an option of mappingRigWith that has the rig's runtime read each repository's
// default branch from github and log to logs, as JSON, as the boot wires it (heldThroughLimit told
// of each read answered from the branch held), with each of recorded recorded before the boot reads
// the mapping, as an earlier boot of the project left them.
func limitedRig(t *testing.T, github *limitedGitHub, logs *syncBuffer, recorded ...record.Issue) func(*workflowRuntime) {
	t.Helper()
	return func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)
		w.defaultBranches.onHold = w.heldThroughLimit
		w.log = slog.New(slog.NewJSONHandler(logs, nil))
		if err := store.Transact(context.Background(), w.pool, func(ctx context.Context, tx pgx.Tx) error {
			for _, issue := range recorded {
				if err := w.records.PutIssue(context.Background(), tx, issue); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("record the project's issues: %v", err)
		}
	}
}

// doneOn is a done issue of project, recorded on repo at the default branch branch.
func doneOn(key, project, repo, branch string) record.Issue {
	return record.Issue{Key: key, Tree: key, Project: project, Title: key, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done",
		Rank: "V", Repo: repo, DefaultBranch: branch, GitHubRepo: repo}
}

// limitedBoth is the rig's two repositories as GitHub answers them once it answers: zeta/tools's
// default branch is trunk.
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

// waitLine is the line the boot logs for a rate-limited repository it holds no branch for
// (waitOutLimits).
const waitLine = "so the boot waits as long as GitHub asks"

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

// heldAdmission prepares an unrecorded todo of the rig's project on repo, as the Dispatch consumer
// and the boot's listing prepare one (prepareUnrecorded), and requires it prepared with no error:
// what the boot's reconciliation and admission read of the repository's default branch.
func heldAdmission(t *testing.T, w *workflowRuntime, repo string) {
	t.Helper()
	if _, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "LEGION-2", Status: "todo", ResolvedRepo: repo}, false, 0, false); err != nil {
		t.Fatalf("prepare an admission on %s: %v, want it prepared on the branch held", repo, err)
	}
}

// A boot whose read of a repository's default branch GitHub refuses for a spent rate limit serves
// the repository at once on the default branch a record of the project holds for it, where it
// exited before: the limit says nothing about the repository, and the record's branch is the one
// GitHub named when the issue was recorded. A read of the branch and an admission's preparation
// inside the limit are answered from the held branch and ask GitHub nothing, and the first read
// after the reset is the read the refused one owed: here GitHub has renamed the default branch
// since, and that read serves the new name.
func TestABootServesARecordedDefaultBranchThroughARateLimitAndReadsItAgainAfterTheReset(t *testing.T) {
	var reset atomic.Int64
	github := newLimitedGitHub(t, limitedBoth, spendsOnce("zeta/tools", 6*time.Second, &reset))
	logs := &syncBuffer{}
	w, _, _, _ := mappingRigWith(t, &installations{}, limitedRig(t, github, logs, doneOn("LEGION-1", "LEGION", "zeta/tools", "main")))
	resetAt := time.Unix(reset.Load(), 0)

	stage := loggedLines(t, logs.String(), `"stage":"repositories"`)
	if len(stage) != 1 {
		t.Fatalf("boot stage lines for the repositories = %v, want one", stage)
	}
	if at := loggedAt(t, stage[0]); !at.Before(resetAt) {
		t.Fatalf("the boot passed its repository read at %s, want before the limit's reset at %s: it waits for nothing it holds", at, resetAt)
	}
	if accepted, _ := stage[0]["accepted"].([]any); !slices.Contains(accepted, any("zeta/tools")) || !slices.Contains(accepted, any("acme/widgets")) {
		t.Fatalf("the boot serves %v, want acme/widgets and zeta/tools", stage[0]["accepted"])
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
	if !served(w, "zeta/tools") {
		t.Fatal("zeta/tools is not served after its read again")
	}
}

// A boot whose read of a repository's default branch GitHub refuses for a spent rate limit, and
// that holds no default branch for it, waits until the reset GitHub names, saying so in one line
// that names the repository, the limit as GitHub's headers gave it and the time it waits until,
// then reads it once more and serves it. Before, the boot exited on the refusal, and its
// supervisor restarted it straight into the same limit.
func TestABootWithNoDefaultBranchHeldWaitsOutARateLimitAndReadsOnceMore(t *testing.T) {
	var reset atomic.Int64
	github := newLimitedGitHub(t, limitedBoth, spendsOnce("zeta/tools", 3*time.Second, &reset))
	logs := &syncBuffer{}
	w, _, _, _ := mappingRigWith(t, &installations{}, limitedRig(t, github, logs))
	resetAt := time.Unix(reset.Load(), 0)

	if reads := github.readsOf("zeta/tools"); len(reads) != 2 || reads[1].Before(resetAt) || reads[1].After(resetAt.Add(5*time.Second)) {
		t.Fatalf("GitHub was read for zeta/tools at %v, want the refused read and one more soon after the reset at %s", reads, resetAt)
	}
	if !served(w, "zeta/tools") || !served(w, "acme/widgets") {
		t.Fatalf("served zeta/tools %v, acme/widgets %v; want both", served(w, "zeta/tools"), served(w, "acme/widgets"))
	}
	if kept, ok := w.defaultBranches.Kept("zeta/tools"); !ok || kept.DefaultBranch != "trunk" {
		t.Fatalf("zeta/tools's default branch = %+v, %v; want GitHub's trunk", kept, ok)
	}
	waits := loggedLines(t, logs.String(), waitLine)
	if len(waits) != 1 {
		t.Fatalf("wait lines = %v, want one", waits)
	}
	for key, want := range map[string]string{
		"repo": "zeta/tools", "waitUntil": resetAt.UTC().Format(time.RFC3339), "rateLimit": "15000", "rateLimitRemaining": "0",
		"rateLimitReset": strconv.FormatInt(reset.Load(), 10), "rateLimitResource": "core", "budget": "core",
	} {
		if got := waits[0][key]; got != want {
			t.Errorf("the wait line's %s = %v, want %q", key, got, want)
		}
	}
	if stage := loggedLines(t, logs.String(), `"stage":"repositories"`); len(stage) != 1 || loggedAt(t, stage[0]).Before(resetAt) {
		t.Fatalf("boot stage lines for the repositories = %v, want one, after the reset at %s", stage, resetAt)
	}
}

// A repository GitHub still refuses for its rate limit once the boot has waited the limit out is
// left out for now, as one whose installation read GitHub did not answer is, rather than
// refusing the boot: the other repository is served, and the refresher reads the mapping again
// on installReadRetry until GitHub answers, then serves it, with no restart.
func TestABootLeavesOutARepositoryGitHubStillRateLimitsAndServesItOnceGitHubAnswers(t *testing.T) {
	quickInstallReads(t)
	var limiting atomic.Bool
	limiting.Store(true)
	github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, _ int) bool {
		if repo != "zeta/tools" || !limiting.Load() {
			return false
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
		return true
	})
	logs := &syncBuffer{}
	w, _, _, _ := mappingRigWith(t, &installations{}, limitedRig(t, github, logs))

	if held := w.repositories.lookup("zeta/tools"); held.refusal == "" || !held.unread {
		t.Fatalf("zeta/tools = refusal %q, unread %v; want it left out for now", held.refusal, held.unread)
	}
	if !served(w, "acme/widgets") {
		t.Fatal("acme/widgets is not served, want it served beside the repository left out")
	}
	if reads := github.readsOf("zeta/tools"); len(reads) < 2 || reads[1].Sub(reads[0]) < time.Second || reads[1].Sub(reads[0]) > 6*time.Second {
		t.Fatalf("GitHub was read for zeta/tools at %v, want the refused read and the boot's one more a second later, as Retry-After asked", reads)
	}
	if waits := loggedLines(t, logs.String(), waitLine); len(waits) != 1 || waits[0]["retryAfter"] != "1" || waits[0]["budget"] != "secondary" {
		t.Fatalf("wait lines = %v, want one, naming the Retry-After and the secondary budget", waits)
	}
	if left := loggedLines(t, logs.String(), "so the daemon boots leaving it out and reads it again"); len(left) != 1 || left[0]["repo"] != "zeta/tools" {
		t.Fatalf("left-out lines = %v, want one naming zeta/tools", left)
	}

	limiting.Store(false)
	testwait.Eventually(t, "zeta/tools served once GitHub answers", func() bool { return served(w, "zeta/tools") })
}

// A read after the boot has waited out a rate limit that GitHub does not answer at all — the
// connection closed with no answer — leaves the repository out for now and reads it again, as a
// limit there does, rather than refusing the boot: GitHub said nothing about the repository.
func TestABootLeavesOutARepositoryWhoseReadAfterARateLimitGetsNoAnswer(t *testing.T) {
	github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, read int) bool {
		switch {
		case repo != "zeta/tools":
			return false
		case read == 1:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
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
	w, _, _, _ := mappingRigWith(t, &installations{}, limitedRig(t, github, logs))

	if held := w.repositories.lookup("zeta/tools"); held.refusal == "" || !held.unread {
		t.Fatalf("zeta/tools = refusal %q, unread %v; want it left out for now", held.refusal, held.unread)
	}
	if !served(w, "acme/widgets") {
		t.Fatal("acme/widgets is not served, want it served beside the repository left out")
	}
	if left := loggedLines(t, logs.String(), "so the daemon boots leaving it out and reads it again"); len(left) != 1 || left[0]["repo"] != "zeta/tools" {
		t.Fatalf("left-out lines = %v, want one naming zeta/tools", left)
	}
}

// GitHub's 403 naming its rate limit in the body alone, with thousands of calls left in its
// headers, is the limit to the read of a default branch: the boot serves the repository on the
// default branch a record holds, a read and an admission inside the minute GitHub's documentation
// waits such a limit ask GitHub nothing, and the line names the headers as GitHub sent them. Read
// as a refusal of the repository, as before, the answer exited the boot.
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
	w, _, _, _ := mappingRigWith(t, &installations{}, limitedRig(t, github, logs, doneOn("LEGION-1", "LEGION", "zeta/tools", "main")))

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
	if !served(w, "zeta/tools") {
		t.Fatal("zeta/tools is not served, want it served at the recorded main")
	}
}

// A 404 for a repository's default branch is GitHub's answer about the repository, and refuses the
// boot at once naming it, as before; a 500 is GitHub's trouble, read again on the boot's retry and
// refusing the boot only once the retry's attempts are spent, as before. Neither is a rate limit,
// so the boot neither waits nor serves a branch it holds.
func TestABootRefusesADefaultBranchRead404AndRetriesA500AsBefore(t *testing.T) {
	quickAppMints(t)
	for _, tc := range []struct {
		name   string
		status int
		reads  int
		want   string
	}{
		{name: "a 404", status: http.StatusNotFound, reads: 1, want: `read the default branch of acme/widgets: GitHub answered GET /repos/acme/widgets with 404: {"message":"no such repository"}`},
		{name: "a 500", status: http.StatusInternalServerError, reads: appMintRetry.Attempts, want: "within its retry budget (3 attempts)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}, func(w http.ResponseWriter, _ string, _ int) bool {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":"no such repository"}`)
				return true
			})
			w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(),
				defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
			w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
			if err := w.nameMapping(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("nameMapping = %v, want the boot refused with %q", err, tc.want)
			}
			if got := len(github.readsOf("acme/widgets")); got != tc.reads {
				t.Fatalf("GitHub was read for acme/widgets %d times, want %d", got, tc.reads)
			}
		})
	}
}

// A record holds a branch only for the repository it names itself, in its own spelling, GitHub's:
// a limited read of the name a record's repository moved to under a configured rename is not held
// from that record, since neither the record nor the mapping, which can hold the pair's spelling as
// configured, holds GitHub's spelling of it, and intake's subjects are case-sensitive. It is waited
// out or left out as any repository with nothing held is.
func TestARecordedDefaultBranchIsHeldOnlyForTheRepositoryTheRecordNames(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := projectRecords{Store: record.NewStore(), project: "LEGION"}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
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
			repos.set([]mappedRepository{{Repo: ghrepo.MustParse(tc.read)}}, nil)
			w := &workflowRuntime{pool: pool, records: records, repositories: repos, log: quietLogger(),
				defaultBranches: newDefaultBranches(roleTokens{}, repos, "", defaultBranchTTL, time.Now)}
			limited := limitedRead{mapped: mappedRepository{Repo: ghrepo.MustParse(tc.read)}, answer: &githubrest.Answer{Status: http.StatusForbidden},
				wait: time.Minute, until: time.Now().Add(time.Minute)}
			held, unheld, err := w.holdLimited(ctx, []limitedRead{limited})
			if err != nil {
				t.Fatal(err)
			}
			if tc.held == "" {
				if _, kept := w.defaultBranches.Kept(tc.read); len(held) != 0 || len(unheld) != 1 || kept {
					t.Fatalf("held %v, unheld %d, kept %v; want %s unheld and nothing kept", held, len(unheld), kept, tc.read)
				}
				return
			}
			if len(held) != 1 || held[0].Repo.String() != tc.held || len(unheld) != 0 {
				t.Fatalf("held %v, unheld %d; want %s served as %s", held, len(unheld), tc.read, tc.held)
			}
			if kept, ok := w.defaultBranches.Kept(tc.read); !ok || kept.Name.String() != tc.held || kept.DefaultBranch != "main" {
				t.Fatalf("the branch held for %s = %+v, %v; want %s at main", tc.read, kept, ok, tc.held)
			}
		})
	}
}

// The boot's backfill of records from before the record carried its repository waits out GitHub's
// rate limit, and a read GitHub does not answer, and reads again until GitHub answers: the boot
// waits rather than exits, and no record is left without its repository or GitHub's spelling of it,
// which every later reader of a record takes as given. Each wait is one line naming the issue, the
// repository and the time it waits until: a limit's as GitHub asks, an unanswered read's 1 s and
// then 2 s. Before, the first refusal refused the whole boot here.
func TestABootsBackfillWaitsOutARateLimitAndFillsTheRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(w http.ResponseWriter)
		gaps   []time.Duration
	}{
		{name: "a rate limit asking for a second", gaps: []time.Duration{time.Second, time.Second}, refuse: func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
		}},
		{name: "a read GitHub does not answer", gaps: []time.Duration{time.Second, 2 * time.Second}, refuse: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"message":"Server Error"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
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
			github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, read int) bool {
				if repo != "zeta/tools" || read > len(tc.gaps) {
					return false
				}
				tc.refuse(w)
				return true
			})
			logs := &syncBuffer{}
			w := &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"}, repositories: newRepositories("LEGION", ghrepo.Renames{}),
				defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now), log: slog.New(slog.NewJSONHandler(logs, nil))}
			client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-7", ResolvedRepo: "zeta/tools"}, {Key: "LEGION-9", ResolvedRepo: "acme/widgets"}}}
			if err := backfillRepositories(ctx, w.pool, w.records, client, "LEGION", t.TempDir(), w.defaultBranches, ghrepo.Renames{}, w.log); err != nil {
				t.Fatalf("backfill = %v, want the boot to wait and go on", err)
			}
			if unfilled := unfilledKeys(t, w); len(unfilled) != 0 {
				t.Fatalf("unfilled after the backfill = %v, want none", unfilled)
			}
			var issues []record.Issue
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				issues, err = w.records.Issues(ctx, tx)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, issue := range issues {
				if issue.Repo == "" || issue.GitHubRepo == "" || issue.DefaultBranch == "" {
					t.Errorf("%s after the backfill = repo %q, GitHub's spelling %q, default branch %q; want all three", issue.Key, issue.Repo, issue.GitHubRepo, issue.DefaultBranch)
				}
			}
			reads := github.readsOf("zeta/tools")
			if len(reads) != len(tc.gaps)+1 {
				t.Fatalf("GitHub was read for zeta/tools %d times, want the %d refused and the one answered", len(reads), len(tc.gaps))
			}
			for index, gap := range tc.gaps {
				if got := reads[index+1].Sub(reads[index]); got < gap || got > gap+5*time.Second {
					t.Errorf("read %d came %s after the one before, want %s", index+2, got, gap)
				}
			}
			waits := loggedLines(t, logs.String(), "so the boot waits and reads it again")
			if len(waits) != len(tc.gaps) {
				t.Fatalf("wait lines = %v, want %d", waits, len(tc.gaps))
			}
			for _, line := range waits {
				if line["issue"] != "LEGION-7" || line["repo"] != "zeta/tools" || line["waitUntil"] == nil {
					t.Errorf("wait line = %v, want one naming LEGION-7, zeta/tools and the time it waits until", line)
				}
			}
			if _, err := w.heardAtBoot(ctx); err != nil {
				t.Fatalf("the boot's subscription after the backfill = %v, want what the records name heard", err)
			}
		})
	}
}

// The reads after the boot's waits run mappingReads at a time, as the first reads do, however many
// repositories share one reset: here more of them than that wait out one limit, and GitHub, holding
// each read a moment, never has more than mappingReads in flight.
func TestTheBootsReadsAfterARateLimitRunMappingReadsAtATime(t *testing.T) {
	var inFlight, most atomic.Int64
	named := map[string]string{}
	var unheld []limitedRead
	for index := range mappingReads + 4 {
		repo := "acme/r" + strconv.Itoa(index)
		named[repo] = repo + "@main"
		unheld = append(unheld, limitedRead{mapped: mappedRepository{Repo: ghrepo.MustParse(repo)}, answer: &githubrest.Answer{Status: http.StatusTooManyRequests},
			until: time.Now()})
	}
	github := newLimitedGitHub(t, named, func(http.ResponseWriter, string, int) bool {
		now := inFlight.Add(1)
		for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return false
	})
	w := &workflowRuntime{log: quietLogger(), defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	served, unread, err := w.waitOutLimits(context.Background(), unheld)
	if err != nil || len(served) != len(unheld) || len(unread) != 0 {
		t.Fatalf("waitOutLimits = %d served, %d unread, %v; want all %d served", len(served), len(unread), err, len(unheld))
	}
	if got := most.Load(); got > mappingReads {
		t.Fatalf("GitHub held %d reads at once, want at most %d", got, mappingReads)
	}
}

// Several repositories whose default-branch reads GitHub's rate limit refused together each wait
// GitHub's own wait, never doubled, and the total each wait line names counts the time the waits
// overlap once: three unheld reads, each asked by GitHub to wait 2 s from an answer just now, wait
// out through waitOutLimits on a runtime holding the start's waits at the production policy. Each
// of the three lines names a waitUntil 2 s after the answers, not 2, 4 and 8 s, and a total of about
// 2 s, not 6 s, and all three repositories are read once more about 2 s after the answers.
func TestTheBootsDefaultBranchReadsRefusedTogetherEachWaitGitHubsOwnWait(t *testing.T) {
	const wait = 2 * time.Second
	named := map[string]string{}
	var unheld []limitedRead
	answered := time.Now()
	for _, repo := range []string{"acme/one", "acme/two", "acme/three"} {
		named[repo] = repo + "@main"
		unheld = append(unheld, limitedRead{mapped: mappedRepository{Repo: ghrepo.MustParse(repo)},
			answer: &githubrest.Answer{Status: http.StatusTooManyRequests, RateLimited: true, RetryAfter: wait, Limit: githubrest.LimitHeaders{RetryAfter: "2"}},
			err:    errors.New("GitHub answered GET /repos/" + repo + " with 429"), wait: wait, until: answered.Add(wait)})
	}
	github := newLimitedGitHub(t, named, func(http.ResponseWriter, string, int) bool { return false })
	logs := &syncBuffer{}
	w := &workflowRuntime{log: slog.New(slog.NewJSONHandler(logs, nil)), defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now),
		startWaits: newStartWaits(startRateLimits, time.Now)}

	served, unread, err := w.waitOutLimits(context.Background(), unheld)

	if err != nil || len(served) != len(unheld) || len(unread) != 0 {
		t.Fatalf("waitOutLimits = %d served, %d unread, %v; want all %d served", len(served), len(unread), err, len(unheld))
	}
	waits := loggedLines(t, logs.String(), waitLine)
	if len(waits) != len(unheld) {
		t.Fatalf("wait lines = %v, want one for each repository", waits)
	}
	for _, line := range waits {
		if want := answered.Add(wait).UTC().Format(time.RFC3339); line["waitUntil"] != want {
			t.Errorf("%v's waitUntil = %v, want GitHub's own wait from its answer, %s", line["repo"], line["waitUntil"], want)
		}
		if waited := loggedDuration(t, line, "waited"); waited < wait-500*time.Millisecond || waited > wait {
			t.Errorf("%v's waited = %s, want about %s: the waits overlap, and their time counts once", line["repo"], waited, wait)
		}
	}
	for repo := range named {
		if reads := github.readsOf(repo); len(reads) != 1 || reads[0].Before(answered.Add(wait)) || reads[0].After(answered.Add(wait+3*time.Second)) {
			t.Errorf("GitHub was read for %s at %v, want once, soon after %s", repo, reads, answered.Add(wait))
		}
	}
}

// Each wait the start decides for GitHub's rate limit, one row for each row of the plan's table
// "One rate-limit refusal of a start's call", on the production policy and a clock of the test's
// own, so no row waits: an App call's wait (startWaits.wait) is GitHub's time with at most the
// maximum, or, where GitHub names none, a minute doubling for each further such refusal of that
// call, and ends at the start's end, after which the call ends the start; a default-branch read's
// (startWaits.readUntil) is #4043's wait from GitHub's answer with at most the maximum, never
// doubled, and none once it is over or at the end; the total waited counts overlapping waits once;
// and the backfill's wait has the maximum and no end.
func TestStartWaitsDecidesEachWait(t *testing.T) {
	if want := (rateLimitPolicy{Max: time.Hour, Bound: 70 * time.Minute, NoTime: time.Minute}); startRateLimits != want {
		t.Fatalf("the start's rate-limit policy = %+v, want R4's %+v", startRateLimits, want)
	}
	start := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	retryAfter := func(seconds string) *githubrest.Answer {
		return &githubrest.Answer{Status: http.StatusTooManyRequests, RateLimited: true, Limit: githubrest.LimitHeaders{RetryAfter: seconds}}
	}
	spent := func(reset time.Duration) *githubrest.Answer {
		return &githubrest.Answer{Status: http.StatusForbidden, RateLimited: true,
			Limit: githubrest.LimitHeaders{Remaining: "0", Reset: strconv.FormatInt(start.Add(reset).Unix(), 10)}}
	}
	noHeader := &githubrest.Answer{Status: http.StatusTooManyRequests, RateLimited: true}
	bodyAlone := &githubrest.Answer{Status: http.StatusForbidden, Body: `{"message":"API rate limit exceeded for installation ID 7."}`,
		Limit: githubrest.LimitHeaders{Remaining: "14671"}}
	const mint, logins = "GitHub App tokens mint", "GitHub App logins read"
	// step is one refusal: of an App call, or of a default-branch read where call is empty.
	type step struct {
		at     time.Duration // the clock, from the start
		call   string
		answer *githubrest.Answer
		wait   time.Duration // GitHub's wait: answerLimit's for an App call, #4043's for a read
		ago    time.Duration // how long before at GitHub answered a read
		until  time.Duration // the wait's end, from the start: at itself for no wait
		waited time.Duration
		ended  bool
	}
	for _, row := range []struct {
		name  string
		steps []step
	}{
		{name: "an App call's Retry-After at most the maximum", steps: []step{
			{call: mint, answer: retryAfter("30"), wait: 30 * time.Second, until: 30 * time.Second, waited: 30 * time.Second},
		}},
		{name: "an App call's Retry-After above the maximum", steps: []step{
			{call: mint, answer: retryAfter("7200"), wait: 2 * time.Hour, until: time.Hour, waited: time.Hour},
		}},
		{name: "an App call's spent limit with its reset ahead", steps: []step{
			{call: mint, answer: spent(20 * time.Minute), wait: 20 * time.Minute, until: 20 * time.Minute, waited: 20 * time.Minute},
		}},
		{name: "an App call that names no time, the n-th such refusal of the call", steps: []step{
			{call: mint, answer: noHeader, wait: time.Minute, until: time.Minute, waited: time.Minute},
			{at: time.Minute, call: mint, answer: spent(-time.Minute), wait: time.Minute, until: 3 * time.Minute, waited: 3 * time.Minute},
			{at: 3 * time.Minute, call: mint, answer: bodyAlone, wait: time.Minute, until: 7 * time.Minute, waited: 7 * time.Minute},
		}},
		{name: "the first refusal that names no time of another call", steps: []step{
			{call: logins, answer: noHeader, wait: time.Minute, until: time.Minute, waited: time.Minute},
			{at: time.Minute, call: logins, answer: noHeader, wait: time.Minute, until: 3 * time.Minute, waited: 3 * time.Minute},
			{at: 3 * time.Minute, call: mint, answer: noHeader, wait: time.Minute, until: 4 * time.Minute, waited: 4 * time.Minute},
		}},
		{name: "an App call whose wait passes the end", steps: []step{
			{call: mint, answer: retryAfter("1800"), wait: 30 * time.Minute, until: 30 * time.Minute, waited: 30 * time.Minute},
			{at: 50 * time.Minute, call: mint, answer: retryAfter("3600"), wait: time.Hour, until: 70 * time.Minute, waited: 50 * time.Minute},
		}},
		{name: "an App call at or past the end", steps: []step{
			{call: mint, answer: retryAfter("30"), wait: 30 * time.Second, until: 30 * time.Second, waited: 30 * time.Second},
			{at: 70 * time.Minute, call: logins, answer: retryAfter("30"), wait: 30 * time.Second, waited: 30 * time.Second, ended: true},
			{at: 75 * time.Minute, call: mint, answer: noHeader, wait: time.Minute, waited: 30 * time.Second, ended: true},
		}},
		{name: "a default-branch read's wait, counted from GitHub's answer", steps: []step{
			{answer: retryAfter("30"), wait: 30 * time.Second, ago: 5 * time.Second, until: 25 * time.Second, waited: 25 * time.Second},
		}},
		{name: "a default-branch read's wait above the maximum", steps: []step{
			{answer: retryAfter("7200"), wait: 2 * time.Hour, until: time.Hour, waited: time.Hour},
		}},
		{name: "a default-branch read's wait past the end", steps: []step{
			{call: mint, answer: retryAfter("60"), wait: time.Minute, until: time.Minute, waited: time.Minute},
			{at: 65 * time.Minute, answer: retryAfter("1800"), wait: 30 * time.Minute, until: 70 * time.Minute, waited: 6 * time.Minute},
		}},
		{name: "default-branch reads refused together, each its own wait, never doubled", steps: []step{
			{answer: noHeader, wait: time.Minute, until: time.Minute, waited: time.Minute},
			{answer: noHeader, wait: time.Minute, until: time.Minute, waited: time.Minute},
			{answer: noHeader, wait: time.Minute, until: time.Minute, waited: time.Minute},
		}},
		{name: "a default-branch read whose wait is already over", steps: []step{
			{answer: retryAfter("120"), wait: 2 * time.Minute, ago: 3 * time.Minute},
		}},
		{name: "a default-branch read at or past the end", steps: []step{
			{call: mint, answer: retryAfter("30"), wait: 30 * time.Second, until: 30 * time.Second, waited: 30 * time.Second},
			{at: 70 * time.Minute, answer: retryAfter("60"), wait: time.Minute, until: 70 * time.Minute, waited: 30 * time.Second},
		}},
		{name: "overlapping waits count once", steps: []step{
			{call: mint, answer: retryAfter("60"), wait: time.Minute, until: time.Minute, waited: time.Minute},
			{at: 30 * time.Second, answer: retryAfter("60"), wait: time.Minute, until: 90 * time.Second, waited: 90 * time.Second},
			{at: 40 * time.Second, answer: retryAfter("10"), wait: 10 * time.Second, until: 50 * time.Second, waited: 90 * time.Second},
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			now := start
			waits := newStartWaits(startRateLimits, func() time.Time { return now })
			for index, step := range row.steps {
				now = start.Add(step.at)
				var until time.Time
				var waited time.Duration
				var ended bool
				if step.call != "" {
					until, waited, ended = waits.wait(step.call, step.answer, step.wait)
				} else {
					until, waited = waits.readUntil(limitedRead{answer: step.answer, wait: step.wait, until: now.Add(step.wait - step.ago)})
				}
				if ended != step.ended || waited != step.waited || !step.ended && !until.Equal(start.Add(step.until)) {
					t.Fatalf("step %d: until %s, waited %s, ended %v; want until %s, waited %s, ended %v",
						index+1, until.Sub(start), waited, ended, step.until, step.waited, step.ended)
				}
			}
		})
	}
	t.Run("the backfill, which has no end: GitHub's wait with at most the maximum", func(t *testing.T) {
		for wait, want := range map[time.Duration]time.Duration{17 * time.Second: 17 * time.Second, 2 * time.Hour: time.Hour} {
			if got := backfillWait(wait); got != want {
				t.Errorf("the backfill's wait for GitHub's %s = %s, want %s", wait, got, want)
			}
		}
	})
}

// liveBootDispatch is a Dispatch for a daemon's boot whose project maps repos, the first its
// default, and lists listed, answering every other call every boot makes (servedBootCall).
func liveBootDispatch(t *testing.T, project string, repos []string, listed []map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings/repo-projects":
			mapping := make([]map[string]any, 0, len(repos))
			for index, repo := range repos {
				mapping = append(mapping, map[string]any{"repo": repo, "project": project, "default": index == 0})
			}
			body = mapping
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
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
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
// at once: GitHub refuses the boot's read of the repository's default branch with a 403 naming no
// call left until a reset half a minute away, and the daemon answers /healthz long before it,
// serving the repository on the recorded branch.
func TestABootWithARecordedDefaultBranchComesUpAtOnceThroughARateLimit(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	recorded := cfg.Project + "-1"
	cfg.DispatchURL = liveBootDispatch(t, cfg.Project, []string{"acme/widgets"}, []map[string]any{listedAs(recorded, "done", "acme/widgets")})
	recordBeforeBoot(t, cfg, doneOn(recorded, cfg.Project, "acme/widgets", "main"))
	var reset atomic.Int64
	github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}, spendsOnce("acme/widgets", 30*time.Second, &reset))
	logs := &syncBuffer{}
	if err := bootsOrExitsOn(t, cfg, &workflowTokenRecorder{}, github.url, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to come up on the recorded default branch", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if !time.Now().Before(resetAt) {
		t.Fatalf("the daemon answered /healthz at or after the limit's reset at %s, want before it: it waits for nothing it holds", resetAt)
	}
	if held := linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets"); len(held) != 1 || held[0]["defaultBranch"] != "main" {
		t.Fatalf("held lines for acme/widgets = %v, want one naming the recorded main", held)
	}
	if waits := loggedLines(t, logs.String(), waitLine); len(waits) != 0 {
		t.Fatalf("wait lines = %v, want none", waits)
	}
	if reads := github.readsOf("acme/widgets"); len(reads) != 1 {
		t.Fatalf("GitHub was read for acme/widgets %d times, want only the one it refused", len(reads))
	}
}

// A held branch outlives its own reset for the rest of the boot. The daemon serves acme/widgets on
// its recorded branch, then waits out zeta/tools's longer limit, by which time the hold on
// acme/widgets has passed its reset; the boot's reconciliation then prepares an unrecorded todo on
// acme/widgets, which reads its default branch again, and GitHub's limit refuses that read too. The
// read is answered from the held branch and the daemon comes up, where a refusal there would end
// the boot.
func TestABootServesAHeldDefaultBranchToAnAdmissionReadAfterTheHoldsReset(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	recorded, todo := cfg.Project+"-1", cfg.Project+"-2"
	cfg.DispatchURL = liveBootDispatch(t, cfg.Project, []string{"acme/widgets", "zeta/tools"}, []map[string]any{
		listedAs(recorded, "done", "acme/widgets"), listedAs(todo, "todo", "acme/widgets")})
	recordBeforeBoot(t, cfg, doneOn(recorded, cfg.Project, "acme/widgets", "main"))
	var zetaReset atomic.Int64
	github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"}, func(w http.ResponseWriter, repo string, read int) bool {
		switch {
		case repo == "acme/widgets":
			// Every read of acme/widgets is refused, each asking for a second's wait: relative, so
			// the hold always ends a second after its read, never at an epoch second already past.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
			return true
		case read == 1:
			zetaReset.Store(time.Now().Add(3 * time.Second).Unix())
			spentLimit(w, zetaReset.Load())
			return true
		}
		return false
	})
	logs := &syncBuffer{}
	if err := bootsOrExitsOn(t, cfg, &workflowTokenRecorder{}, github.url, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to come up, the admission's read answered from the held branch", err)
	}
	zetaAt := time.Unix(zetaReset.Load(), 0)
	widgets := github.readsOf("acme/widgets")
	if len(widgets) < 2 || widgets[1].Before(zetaAt) {
		t.Fatalf("GitHub was read for acme/widgets at %v, want the boot's refused read and the reconciliation's after zeta/tools's reset at %s", widgets, zetaAt)
	}
	if held := linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets"); len(held) < 2 {
		t.Fatalf("held lines for acme/widgets = %v, want the boot's and the reconciliation's read", held)
	}
	if waits := linesFor(loggedLines(t, logs.String(), waitLine), "zeta/tools"); len(waits) != 1 {
		t.Fatalf("wait lines for zeta/tools = %v, want one", waits)
	}
}

// The daemon itself boots through a spent rate limit on a repository it holds no branch for:
// GitHub refuses the boot's read of the project's repository with a 403 naming no call left until a
// reset three seconds away, and then answers. The boot waits the limit out, says so once, reads
// once more and comes up, where before it exited after its appauth stage with GitHub's refusal.
func TestABootThroughASpentRateLimitComesUp(t *testing.T) {
	var reset atomic.Int64
	github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}, spendsOnce("acme/widgets", 3*time.Second, &reset))
	logs := &syncBuffer{}
	if err := bootsOrExitsOn(t, workflowConfig(t, workflowNATS(t)), &workflowTokenRecorder{}, github.url, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit and come up", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if reads := github.readsOf("acme/widgets"); len(reads) < 2 || reads[1].Before(resetAt) || reads[1].After(resetAt.Add(5*time.Second)) {
		t.Fatalf("GitHub was read for acme/widgets at %v, want the refused read and one more soon after the reset at %s", reads, resetAt)
	}
	if waits := loggedLines(t, logs.String(), waitLine); len(waits) != 1 || waits[0]["repo"] != "acme/widgets" {
		t.Fatalf("wait lines = %v, want one naming acme/widgets", waits)
	}
}

// An event's preparation whose read of a repository's default branch GitHub refuses for its rate
// limit, the daemon holding no read of the repository, is delivered again after the wait GitHub
// asked for (an intake.RetryLater), not a second later into the same limit, each delivery reading
// GitHub again: a 429's Retry-After, and the minute a 403 naming the limit in its body alone waits.
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
			w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(),
				defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
			w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("zeta/tools"), Default: true}}, nil)
			_, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "LEGION-2", Status: "todo", ResolvedRepo: "zeta/tools"}, false, 0, false)
			var later *intake.RetryLater
			if !errors.As(err, &later) || later.After != tc.want {
				t.Fatalf("prepare = %v, want an intake.RetryLater after %s", err, tc.want)
			}
			if got := len(github.readsOf("zeta/tools")); got != 1 {
				t.Fatalf("GitHub was read for zeta/tools %d times, want once", got)
			}
		})
	}
}

// retriesIn30 answers a read with GitHub's 429 for its secondary limit, asking for 30 s.
func retriesIn30(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
}

// The start's wait for a default-branch read GitHub's rate limit refused gets the maximum and the
// start's end (c-boot-mint-rate-limit case 7), then reads once more, as #4043's does. Every read of
// zeta/tools, which no record holds a branch for, is a 429 asking for 30 s. With the maximum at 1 s,
// the wait line's waitUntil is a second ahead and the read once more comes a second after the
// first; with the bound at 1 s and the maximum at 10 s, the wait ends at the bound. Either way the
// read once more is refused again, zeta/tools is left out for now, and acme/widgets is served.
func TestABootsDefaultBranchReadWaitsAtMostTheMaximumAndEndsItsWaitAtTheBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy func(*rateLimitPolicy)
	}{
		{name: "the maximum", policy: func(policy *rateLimitPolicy) { policy.Max = time.Second }},
		{name: "the bound", policy: func(policy *rateLimitPolicy) { policy.Bound, policy.Max = time.Second, 10*time.Second }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStartRateLimits(t, tc.policy)
			github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, _ int) bool {
				if repo != "zeta/tools" {
					return false
				}
				retriesIn30(w)
				return true
			})
			logs := &syncBuffer{}
			w, _, _, _ := mappingRigWith(t, &installations{}, limitedRig(t, github, logs), func(w *workflowRuntime) {
				w.startWaits = newStartWaits(startRateLimits, time.Now)
			})

			waits := loggedLines(t, logs.String(), waitLine)
			if len(waits) != 1 || waits[0]["repo"] != "zeta/tools" {
				t.Fatalf("wait lines = %v, want one naming zeta/tools", waits)
			}
			until, err := time.Parse(time.RFC3339, fmt.Sprint(waits[0]["waitUntil"]))
			if ahead := until.Sub(loggedAt(t, waits[0])); err != nil || ahead > time.Second {
				t.Fatalf("the wait line's waitUntil = %v, %s after the line (%v); want at most a second ahead, not GitHub's 30 s", waits[0]["waitUntil"], ahead, err)
			}
			if waited := loggedDuration(t, waits[0], "waited"); waited <= 0 || waited > time.Second {
				t.Errorf("the wait line's waited = %s, want the second it waits", waited)
			}
			if reads := github.readsOf("zeta/tools"); len(reads) < 2 || reads[1].Sub(reads[0]) < 900*time.Millisecond || reads[1].Sub(reads[0]) > 5*time.Second {
				t.Fatalf("GitHub was read for zeta/tools at %v, want the refused read and one more about a second after it", reads)
			}
			if held := w.repositories.lookup("zeta/tools"); held.refusal == "" || !held.unread {
				t.Fatalf("zeta/tools = refusal %q, unread %v; want it left out for now", held.refusal, held.unread)
			}
			if !served(w, "acme/widgets") {
				t.Fatal("acme/widgets is not served, want it served beside the repository left out")
			}
		})
	}
}

// The start's backfill of a record without repository details waits at most the maximum for each
// rate limit and goes on past the start's end until GitHub answers (c-boot-mint-rate-limit case 8,
// the maximum shortened to 500 ms and the bound to 1 s): GitHub refuses the read of the record's
// repository four times with a 429 asking for 30 s, then answers. Each read comes 500 ms after the
// one before, the reads go on past the 1 s bound, and the record is filled.
func TestABootsBackfillWaitsAtMostTheMaximumAndPastTheBoundUntilGitHubAnswers(t *testing.T) {
	withStartRateLimits(t, func(policy *rateLimitPolicy) { policy.Max, policy.Bound = 500*time.Millisecond, time.Second })
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-7', 'LEGION-7', 'LEGION', 'LEGION-7', 'planning', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	const refusals = 4
	github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, read int) bool {
		if repo != "zeta/tools" || read > refusals {
			return false
		}
		retriesIn30(w)
		return true
	})
	logs := &syncBuffer{}
	w := &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"}, repositories: newRepositories("LEGION", ghrepo.Renames{}),
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now), log: slog.New(slog.NewJSONHandler(logs, nil))}
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-7", ResolvedRepo: "zeta/tools"}}}
	// GitHub's whole Retry-After four times over would take two minutes; the bound ends a backfill
	// that waits it well before.
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := backfillRepositories(bounded, w.pool, w.records, client, "LEGION", t.TempDir(), w.defaultBranches, ghrepo.Renames{}, w.log)
	waits := loggedLines(t, logs.String(), "so the boot waits and reads it again")
	if err != nil {
		t.Fatalf("backfill = %v, want the record filled once GitHub answers; its wait lines: %v", err, waits)
	}
	if unfilled := unfilledKeys(t, w); len(unfilled) != 0 {
		t.Fatalf("unfilled after the backfill = %v, want none", unfilled)
	}
	if len(waits) != refusals {
		t.Fatalf("wait lines = %v, want %d", waits, refusals)
	}
	reads := github.readsOf("zeta/tools")
	if len(reads) != refusals+1 {
		t.Fatalf("GitHub was read for zeta/tools %d times, want the %d refused and the one answered", len(reads), refusals)
	}
	for index := 1; index < len(reads); index++ {
		if gap := reads[index].Sub(reads[index-1]); gap < 500*time.Millisecond || gap > 3*time.Second {
			t.Errorf("read %d came %s after the one before, want the 500ms maximum", index+1, gap)
		}
	}
	if span := reads[len(reads)-1].Sub(reads[0]); span <= time.Second {
		t.Errorf("the reads spanned %s, want past the 1s bound", span)
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
	w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(),
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("zeta/tools"), Default: true}}, nil)
	_, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "LEGION-2", Status: "todo", ResolvedRepo: "zeta/tools"}, false, 0, false)
	var later *intake.RetryLater
	if !errors.As(err, &later) || later.After != 17*time.Second {
		t.Fatalf("prepare = %v, want an intake.RetryLater after GitHub's 17s", err)
	}
}
