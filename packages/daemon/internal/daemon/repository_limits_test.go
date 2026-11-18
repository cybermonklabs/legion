package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
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
// that repository's reads, from 1. Every other request is answered 404. It counts the reads of
// each named repository.
type limitedGitHub struct {
	url   string
	mu    sync.Mutex
	reads map[string]int
}

func newLimitedGitHub(t *testing.T, named map[string]string, refuse func(w http.ResponseWriter, repo string, read int) bool) *limitedGitHub {
	t.Helper()
	g := &limitedGitHub{reads: map[string]int{}}
	g.url = githubtest.Server(t, githubtest.Spec{Credential: aLease, Handle: func(w http.ResponseWriter, r *http.Request) {
		repo := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		answer, ok := named[repo]
		if r.Method != http.MethodGet || !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`)
			return
		}
		g.mu.Lock()
		g.reads[repo]++
		read := g.reads[repo]
		g.mu.Unlock()
		if refuse(w, repo, read) {
			return
		}
		name, branch, _ := strings.Cut(answer, "@")
		_, _ = io.WriteString(w, `{"full_name":"`+name+`","default_branch":"`+branch+`"}`)
	}})
	return g
}

// readsOf is how many reads of repo arrived.
func (g *limitedGitHub) readsOf(repo string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads[repo]
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
// of each read answered from the branch held, and each branch kept on the runtime's clock, the
// rig's fake), with each of recorded recorded before the boot reads the mapping, as an earlier boot
// of the project left them.
func limitedRig(t *testing.T, github *limitedGitHub, logs *syncBuffer, recorded ...record.Issue) func(*workflowRuntime) {
	t.Helper()
	return func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, w.timers().Now)
		w.defaultBranches.onHold = w.heldThroughLimit
		w.log = slog.New(slog.NewJSONHandler(logs, nil))
		if err := pgx.BeginFunc(context.Background(), w.pool, func(tx pgx.Tx) error {
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

// limitAhead is how far after a refused read the reset of a spent primary limit falls: half an
// hour, inside the hour githubrest caps such a wait at and far past the minute it gives a limit that
// names no wait, so the wait the daemon arms is the reset's however long the machine takes between
// GitHub's answer and the daemon's read of it. The daemon waits on a fake clock the test moves, so
// a wait that long costs the test nothing.
const limitAhead = 30 * time.Minute

// requireResetWait fails t unless wait, the wait the daemon names for repo's spent primary limit
// whose reset fell limitAhead after the refused read, is the wait to that reset: no longer than
// limitAhead, since GitHub's answer is read after it was sent, and longer than the minute a limit
// that names no wait gets (unnamedLimitWait).
func requireResetWait(t *testing.T, repo string, wait time.Duration) {
	t.Helper()
	if wait > limitAhead || wait <= unnamedLimitWait {
		t.Fatalf("the daemon waits %s for %s's limit, want the wait to its reset: at most %s, and more than the %s a limit naming no wait gets",
			wait, repo, limitAhead, unnamedLimitWait)
	}
}

// namedWait is the wait a line of the daemon's names for a rate-limited read (limitAttributes).
func namedWait(t *testing.T, line map[string]any) time.Duration {
	t.Helper()
	wait, err := time.ParseDuration(fmt.Sprint(line["wait"]))
	if err != nil {
		t.Fatalf("read the wait the line %v names: %v", line, err)
	}
	return wait
}

// awaitLine waits for the next line of the daemon's JSON log that holds text and names repo, off
// the log's stream, and returns it decoded.
func awaitLine(t *testing.T, logs *syncBuffer, text, repo string) map[string]any {
	t.Helper()
	var fields map[string]any
	testwait.Next(t, fmt.Sprintf("the daemon's line %q naming %s", text, repo), logs.Stream(), func(line string) bool {
		fields = nil
		return strings.Contains(line, text) && json.Unmarshal([]byte(line), &fields) == nil && fields["repo"] == repo
	})
	return fields
}

// fireLimitWait waits for the line the boot logs as it waits out repo's rate limit (waitOutLimits),
// holds that GitHub was read for repo only the once its limit refused while that wait stands, and
// moves clk past the wait. clk is the daemon's, made by clocktest.New, which stands still until the
// test moves it, so the daemon's present at the refusal is clk's present now, and the wait is armed
// for exactly the wait the line names, ending at the time the line names (waitUntil). It returns
// that wait.
func fireLimitWait(t *testing.T, clk *clocktest.Fake, logs *syncBuffer, github *limitedGitHub, repo string) time.Duration {
	t.Helper()
	line := awaitLine(t, logs, waitLine, repo)
	wait := namedWait(t, line)
	if until := clk.Now().Add(wait).UTC().Format(time.RFC3339); line["waitUntil"] != until {
		t.Fatalf("the boot's wait line for %s names %v as the time it waits until, want %s: the end of the %s it waits, counted from the refusal",
			repo, line["waitUntil"], until, wait)
	}
	// The boot logs the line and then waits on clk, which only the test moves, so no read of repo
	// after the refused one can have been made yet.
	if reads := github.readsOf(repo); reads != 1 {
		t.Fatalf("GitHub was read for %s %d times while the boot waits out its limit, want only the one it refused", repo, reads)
	}
	fireArmed(t, clk, "the boot's wait out of "+repo+"'s rate limit", wait)
	return wait
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
// since, and that read serves the new name. The hold and its read again run on the rig's fake
// clock, so "inside the limit" is before the test moves the clock to the hold's end, and "after
// the reset" is once it has.
func TestABootServesARecordedDefaultBranchThroughARateLimitAndReadsItAgainAfterTheReset(t *testing.T) {
	var reset atomic.Int64
	github := newLimitedGitHub(t, limitedBoth, spendsOnce("zeta/tools", limitAhead, &reset))
	logs := &syncBuffer{}
	w, _, _, _ := mappingRigWith(t, &installations{}, limitedRig(t, github, logs, doneOn("LEGION-1", "LEGION", "zeta/tools", "main")))
	clk := rigClock(w)

	stage := loggedLines(t, logs.String(), `"stage":"repositories"`)
	if len(stage) != 1 {
		t.Fatalf("boot stage lines for the repositories = %v, want one", stage)
	}
	if accepted, _ := stage[0]["accepted"].([]any); !slices.Contains(accepted, any("zeta/tools")) || !slices.Contains(accepted, any("acme/widgets")) {
		t.Fatalf("the boot serves %v, want acme/widgets and zeta/tools", stage[0]["accepted"])
	}
	held := loggedLines(t, logs.String(), heldLine)
	if len(held) != 1 || held[0]["repo"] != "zeta/tools" || held[0]["defaultBranch"] != "main" || held[0]["rateLimit"] != "15000" ||
		held[0]["rateLimitRemaining"] != "0" || held[0]["rateLimitReset"] != strconv.FormatInt(reset.Load(), 10) {
		t.Fatalf("the held repository's lines = %v, want one naming zeta/tools at the recorded main and GitHub's limit headers", held)
	}
	hold := namedWait(t, held[0])
	requireResetWait(t, "zeta/tools", hold)
	// The rig's boot read the mapping with its clock never moved, and the read again at the hold's
	// end stands armed on that clock, at the time the line names: the boot waited for nothing it
	// holds.
	readAgain := clk.Now().Add(hold)
	if armed := clk.Armed(); !slices.ContainsFunc(armed, readAgain.Equal) || held[0]["readAgainAt"] != readAgain.UTC().Format(time.RFC3339) {
		t.Fatalf("the fake clock holds %v armed once the boot read the mapping, and the held line reads zeta/tools again at %v; want the read again armed at the hold's end, %s",
			armed, held[0]["readAgainAt"], readAgain)
	}

	if got, err := w.defaultBranches.Named(context.Background(), ghrepo.MustParse("zeta/tools")); err != nil || got.DefaultBranch != "main" {
		t.Fatalf("a read of zeta/tools inside the limit = %+v, %v; want the held main", got, err)
	}
	heldAdmission(t, w, "zeta/tools")
	if reads := github.readsOf("zeta/tools"); reads != 1 {
		t.Fatalf("GitHub was read for zeta/tools %d times by the reads inside the limit, want only the one it refused", reads)
	}

	fireArmed(t, clk, "the read again of zeta/tools at its hold's end", hold)
	testwait.Eventually(t, "zeta/tools read from GitHub again after the limit's reset", func() bool {
		kept, ok := w.defaultBranches.Kept("zeta/tools")
		return ok && kept.DefaultBranch == "trunk"
	})
	if reads := github.readsOf("zeta/tools"); reads != 2 {
		t.Fatalf("GitHub was read for zeta/tools %d times, want the refused read and the one the hold's end asked for", reads)
	}
	if !served(w, "zeta/tools") {
		t.Fatal("zeta/tools is not served after its read again")
	}
}

// A boot whose read of a repository's default branch GitHub refuses for a spent rate limit, and
// that holds no default branch for it, waits until the reset GitHub names, saying so in one line
// that names the repository, the limit as GitHub's headers gave it and the time it waits until,
// then reads it once more and serves it. Before, the boot exited on the refusal, and its
// supervisor restarted it straight into the same limit. The wait runs on the rig's fake clock,
// which the test moves past it while the boot waits (fireLimitWait).
func TestABootWithNoDefaultBranchHeldWaitsOutARateLimitAndReadsOnceMore(t *testing.T) {
	var reset atomic.Int64
	github := newLimitedGitHub(t, limitedBoth, spendsOnce("zeta/tools", limitAhead, &reset))
	logs := &syncBuffer{}
	var wait time.Duration
	w, _, _, _, _ := mappingRigDriven(t, &installations{}, func(w *workflowRuntime) {
		// The boot passes its repository read only once its wait out of zeta/tools's limit has
		// ended, and only the test's move of the clock ends it: while it stands, no stage line.
		testwait.Eventually(t, "the boot's line that it waits out zeta/tools's rate limit", func() bool {
			return len(linesFor(loggedLines(t, logs.String(), waitLine), "zeta/tools")) > 0
		})
		if stage := loggedLines(t, logs.String(), `"stage":"repositories"`); len(stage) != 0 {
			t.Fatalf("boot stage lines for the repositories = %v while the boot waits out zeta/tools's limit, want none", stage)
		}
		wait = fireLimitWait(t, rigClock(w), logs, github, "zeta/tools")
	}, limitedRig(t, github, logs))
	requireResetWait(t, "zeta/tools", wait)

	if reads := github.readsOf("zeta/tools"); reads != 2 {
		t.Fatalf("GitHub was read for zeta/tools %d times, want the refused read and one more once the wait to its reset ended", reads)
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
		"repo": "zeta/tools", "rateLimit": "15000", "rateLimitRemaining": "0",
		"rateLimitReset": strconv.FormatInt(reset.Load(), 10), "rateLimitResource": "core", "budget": "core",
	} {
		if got := waits[0][key]; got != want {
			t.Errorf("the wait line's %s = %v, want %q", key, got, want)
		}
	}
	if stage := loggedLines(t, logs.String(), `"stage":"repositories"`); len(stage) != 1 {
		t.Fatalf("boot stage lines for the repositories = %v, want one, once the wait had ended", stage)
	}
}

// A repository GitHub still refuses for its rate limit once the boot has waited the limit out is
// left out for now, as one whose installation read GitHub did not answer is, rather than
// refusing the boot: the other repository is served, and the refresher reads the mapping again
// on installReadRetry until GitHub answers, then serves it, with no restart. The boot's wait and
// the refresher's are both the rig's fake clock's, which the test moves past each (fireLimitWait,
// recheck).
func TestABootLeavesOutARepositoryGitHubStillRateLimitsAndServesItOnceGitHubAnswers(t *testing.T) {
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
	w, _, _, _, _ := mappingRigDriven(t, &installations{}, func(w *workflowRuntime) {
		if wait := fireLimitWait(t, rigClock(w), logs, github, "zeta/tools"); wait != time.Second {
			t.Fatalf("the boot waits %s for zeta/tools's limit, want the second its Retry-After asks", wait)
		}
	}, limitedRig(t, github, logs))

	if held := w.repositories.lookup("zeta/tools"); held.refusal == "" || !held.unread {
		t.Fatalf("zeta/tools = refusal %q, unread %v; want it left out for now", held.refusal, held.unread)
	}
	if !served(w, "acme/widgets") {
		t.Fatal("acme/widgets is not served, want it served beside the repository left out")
	}
	if reads := github.readsOf("zeta/tools"); reads != 2 {
		t.Fatalf("GitHub was read for zeta/tools %d times, want the refused read and the boot's one more once the second Retry-After asked for had passed", reads)
	}
	if waits := loggedLines(t, logs.String(), waitLine); len(waits) != 1 || waits[0]["retryAfter"] != "1" || waits[0]["budget"] != "secondary" {
		t.Fatalf("wait lines = %v, want one, naming the Retry-After and the secondary budget", waits)
	}
	if left := loggedLines(t, logs.String(), "so the daemon boots leaving it out and reads it again"); len(left) != 1 || left[0]["repo"] != "zeta/tools" {
		t.Fatalf("left-out lines = %v, want one naming zeta/tools", left)
	}

	limiting.Store(false)
	recheck(t, w)
	testwait.Eventually(t, "zeta/tools served once GitHub answers", func() bool { return served(w, "zeta/tools") })
}

// A read after the boot has waited out a rate limit that GitHub does not answer at all — the
// connection closed with no answer — leaves the repository out for now and reads it again, as a
// limit there does, rather than refusing the boot: GitHub said nothing about the repository. The
// boot's wait is the rig's fake clock's, which the test moves past it (fireLimitWait).
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
	w, _, _, _, _ := mappingRigDriven(t, &installations{}, func(w *workflowRuntime) {
		if wait := fireLimitWait(t, rigClock(w), logs, github, "zeta/tools"); wait != time.Second {
			t.Fatalf("the boot waits %s for zeta/tools's limit, want the second its Retry-After asks", wait)
		}
	}, limitedRig(t, github, logs))

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
// as a refusal of the repository, as before, the answer exited the boot. The minute is the rig's
// fake clock's, which nothing moves here, so every read is inside it.
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
	if reads := github.readsOf("zeta/tools"); reads != 1 {
		t.Fatalf("GitHub was read for zeta/tools %d times, want the one refused read and none inside the limit", reads)
	}
	if !served(w, "zeta/tools") {
		t.Fatal("zeta/tools is not served, want it served at the recorded main")
	}
}

// A 404 for a repository's default branch is GitHub's answer about the repository, and refuses the
// boot at once naming it, as before; a 500 is GitHub's trouble, read again on the boot's retry and
// refusing the boot only once the retry's attempts are spent, as before. Neither is a rate limit,
// so the boot neither waits nor serves a branch it holds. The retry's waits are the runtime's fake
// clock's, which the test moves past each (fireBackoff).
func TestABootRefusesADefaultBranchRead404AndRetriesA500AsBefore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reads  int
		want   string
	}{
		{name: "a 404", status: http.StatusNotFound, reads: 1, want: `read the default branch of acme/widgets: GitHub answered GET /repos/acme/widgets with 404: {"message":"no such repository"}`},
		{name: "a 500", status: http.StatusInternalServerError, reads: appMintRetry.Attempts, want: fmt.Sprintf("within its retry budget (%d attempts)", appMintRetry.Attempts)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}, func(w http.ResponseWriter, _ string, _ int) bool {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":"no such repository"}`)
				return true
			})
			clk := clocktest.New(time.Now())
			w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(), clock: clk,
				defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, clk.Now)}
			w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
			named := make(chan error, 1)
			go func() { named <- w.nameMapping(context.Background()) }()
			fireBackoff(t, clk, "the wait before the repository read's next attempt", appMintRetry, tc.reads-1)
			if err := testwait.Receive(t, "nameMapping to return", named); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("nameMapping = %v, want the boot refused with %q", err, tc.want)
			}
			if got := github.readsOf("acme/widgets"); got != tc.reads {
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
			repos.set([]mappedRepository{{Repo: ghrepo.MustParse(tc.read)}}, nil)
			clk := clocktest.New(time.Now())
			w := &workflowRuntime{pool: pool, records: records, repositories: repos, log: quietLogger(), clock: clk,
				defaultBranches: newDefaultBranches(roleTokens{}, repos, "", defaultBranchTTL, clk.Now)}
			limited := limitedRead{mapped: mappedRepository{Repo: ghrepo.MustParse(tc.read)}, answer: &githubrest.Answer{Status: http.StatusForbidden},
				wait: time.Minute, until: clk.Now().Add(time.Minute)}
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
// then 2 s. Before, the first refusal refused the whole boot here. Each wait is armed on a fake
// clock for exactly that span, and no read is made again until the test moves the clock past it.
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
			github := newLimitedGitHub(t, limitedBoth, func(w http.ResponseWriter, repo string, read int) bool {
				if repo != "zeta/tools" || read > len(tc.gaps) {
					return false
				}
				tc.refuse(w)
				return true
			})
			logs := &syncBuffer{}
			clk := clocktest.New(time.Now())
			w := &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"}, repositories: newRepositories("LEGION", ghrepo.Renames{}),
				clock: clk, defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, clk.Now), log: slog.New(slog.NewJSONHandler(logs, nil))}
			client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-7", ResolvedRepo: "zeta/tools"}, {Key: "LEGION-9", ResolvedRepo: "acme/widgets"}}}
			state := t.TempDir()
			backfilled := make(chan error, 1)
			go func() {
				backfilled <- backfillRepositories(ctx, clk, w.pool, w.records, client, "LEGION", state, w.defaultBranches, ghrepo.Renames{}, w.log)
			}()
			for index, gap := range tc.gaps {
				what := fmt.Sprintf("the backfill's wait %d of %d", index+1, len(tc.gaps))
				testwait.Next(t, "the line of "+what, logs.Stream(), func(line string) bool { return strings.Contains(line, backfillWaitLine) })
				// The backfill logs the line and then waits on clk, which only the test moves.
				if reads := github.readsOf("zeta/tools"); reads != index+1 {
					t.Fatalf("GitHub was read for zeta/tools %d times while %s stands, want %d: no read again before its wait ends", reads, what, index+1)
				}
				fireArmed(t, clk, what, gap)
			}
			if err := testwait.Receive(t, "the backfill to return", backfilled); err != nil {
				t.Fatalf("backfill = %v, want the boot to wait and go on", err)
			}
			if unfilled := unfilledKeys(t, w); len(unfilled) != 0 {
				t.Fatalf("unfilled after the backfill = %v, want none", unfilled)
			}
			var issues []record.Issue
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
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
			if reads := github.readsOf("zeta/tools"); reads != len(tc.gaps)+1 {
				t.Fatalf("GitHub was read for zeta/tools %d times, want the %d refused and the one answered", reads, len(tc.gaps))
			}
			waits := loggedLines(t, logs.String(), backfillWaitLine)
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

// backfillWaitLine is the line the boot's backfill logs for each wait before it reads a record's
// repository again (backfillRead).
const backfillWaitLine = "so the boot waits and reads it again"

// The reads after the boot's waits run mappingReads at a time, as the first reads do, however many
// repositories share one reset: here more of them than that wait out one limit, already passed, and
// GitHub holds each read until the test lets the reads go. Once mappingReads are held, a span
// (testwait.Span) gives any read past them the time to arrive: whether one starts is decided by
// waitOutLimits' own semaphore, which no state the test can read shows; the reads are real HTTP,
// which testing/synctest does not run; and no later action is ordered after a read the semaphore
// holds back.
func TestTheBootsReadsAfterARateLimitRunMappingReadsAtATime(t *testing.T) {
	var inFlight, most atomic.Int64
	clk := clocktest.New(time.Now())
	named := map[string]string{}
	var unheld []limitedRead
	for index := range mappingReads + 4 {
		repo := "acme/r" + strconv.Itoa(index)
		named[repo] = repo + "@main"
		unheld = append(unheld, limitedRead{mapped: mappedRepository{Repo: ghrepo.MustParse(repo)}, answer: &githubrest.Answer{Status: http.StatusTooManyRequests},
			until: clk.Now()})
	}
	gate := make(chan struct{})
	github := newLimitedGitHub(t, named, func(http.ResponseWriter, string, int) bool {
		now := inFlight.Add(1)
		for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
		}
		<-gate
		inFlight.Add(-1)
		return false
	})
	// The reads are let go before the stand-in's server closes, however the test ends.
	var open sync.Once
	release := func() { open.Do(func() { close(gate) }) }
	t.Cleanup(release)
	w := &workflowRuntime{log: quietLogger(), clock: clk, defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, clk.Now)}
	type outcome struct {
		served []mappedRepository
		unread []refusedRepository
		err    error
	}
	waited := make(chan outcome, 1)
	go func() {
		served, unread, err := w.waitOutLimits(context.Background(), unheld)
		waited <- outcome{served, unread, err}
	}()
	testwait.Until(t, fmt.Sprintf("GitHub to hold %d reads at once", mappingReads), func() (int64, bool) {
		held := inFlight.Load()
		return held, held >= mappingReads
	})
	testwait.Span(t, 200*time.Millisecond, "a read past mappingReads starts only when waitOutLimits' semaphore lets it, which no state the test can read shows; the reads are real HTTP, which testing/synctest does not run; and no later action is ordered after a read the semaphore holds back")
	if got := most.Load(); got > mappingReads {
		t.Fatalf("GitHub held %d reads at once, want at most %d", got, mappingReads)
	}
	release()
	result := testwait.Receive(t, "waitOutLimits to return", waited)
	if result.err != nil || len(result.served) != len(unheld) || len(result.unread) != 0 {
		t.Fatalf("waitOutLimits = %d served, %d unread, %v; want all %d served", len(result.served), len(result.unread), result.err, len(unheld))
	}
}

// liveBootDispatch is a Dispatch for a daemon's boot whose project maps repos, the first its
// default, and lists listed, answering every other call every boot makes (servedBootCall). It is a
// dispatchBoard: a request without the daemon's bearer is another process's, answered 404, and
// fails nothing.
func liveBootDispatch(t *testing.T, project string, repos []string, listed []map[string]any) string {
	t.Helper()
	return dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
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
	})
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
// at once: GitHub refuses the boot's read of the repository's default branch with a 403 naming no
// call left until a reset half an hour away, and the daemon reports itself ready with its clock, a
// fake, never moved, serving the repository on the recorded branch, with the read again at the
// hold's end still armed.
func TestABootWithARecordedDefaultBranchComesUpAtOnceThroughARateLimit(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	recorded := cfg.Project + "-1"
	cfg.DispatchURL = liveBootDispatch(t, cfg.Project, []string{"acme/widgets"}, []map[string]any{listedAs(recorded, "done", "acme/widgets")})
	recordBeforeBoot(t, cfg, doneOn(recorded, cfg.Project, "acme/widgets", "main"))
	var reset atomic.Int64
	github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}, spendsOnce("acme/widgets", limitAhead, &reset))
	logs := &syncBuffer{}
	clk := clocktest.New(time.Now())
	if err := bootOnGitHub(t, cfg, &workflowTokenRecorder{}, github.url, clk, slog.New(slog.NewJSONHandler(logs, nil))).readyOrExit(t); err != nil {
		t.Fatalf("boot = %v, want it to come up on the recorded default branch", err)
	}
	held := linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets")
	if len(held) != 1 || held[0]["defaultBranch"] != "main" || held[0]["rateLimitReset"] != strconv.FormatInt(reset.Load(), 10) {
		t.Fatalf("held lines for acme/widgets = %v, want one naming the recorded main and GitHub's reset", held)
	}
	hold := namedWait(t, held[0])
	requireResetWait(t, "acme/widgets", hold)
	if readAgain := clk.Now().Add(hold); !slices.ContainsFunc(clk.Armed(), readAgain.Equal) {
		t.Fatalf("the daemon's clock holds %v armed once it is ready, want the read again of acme/widgets at its hold's end, %s: it waits for nothing it holds",
			clk.Armed(), readAgain)
	}
	if waits := loggedLines(t, logs.String(), waitLine); len(waits) != 0 {
		t.Fatalf("wait lines = %v, want none", waits)
	}
	if reads := github.readsOf("acme/widgets"); reads != 1 {
		t.Fatalf("GitHub was read for acme/widgets %d times, want only the one it refused", reads)
	}
}

// A held branch outlives its own reset for the rest of the boot. The daemon serves acme/widgets on
// its recorded branch, then waits out zeta/tools's longer limit, by which time the hold on
// acme/widgets has passed its reset; the boot's reconciliation then prepares an unrecorded todo on
// acme/widgets, which reads its default branch again, and GitHub's limit refuses that read too. The
// read is answered from the held branch and the daemon comes up, where a refusal there would end
// the boot. The daemon's clock is a fake the test moves past zeta/tools's wait (fireLimitWait),
// which takes it past acme/widgets's hold too.
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
			zetaReset.Store(time.Now().Add(limitAhead).Unix())
			spentLimit(w, zetaReset.Load())
			return true
		}
		return false
	})
	logs := &syncBuffer{}
	clk := clocktest.New(time.Now())
	boot := bootOnGitHub(t, cfg, &workflowTokenRecorder{}, github.url, clk, slog.New(slog.NewJSONHandler(logs, nil)))
	// The boot reads both repositories before it logs its wait out of zeta/tools's limit, and reads
	// acme/widgets again only once that wait, which only the test can end, has ended.
	testwait.Eventually(t, "the boot's line that it waits out zeta/tools's rate limit", func() bool {
		return len(linesFor(loggedLines(t, logs.String(), waitLine), "zeta/tools")) > 0
	})
	if widgets := github.readsOf("acme/widgets"); widgets != 1 {
		t.Fatalf("GitHub was read for acme/widgets %d times before zeta/tools's wait ended, want only the boot's refused read", widgets)
	}
	if wait := fireLimitWait(t, clk, logs, github, "zeta/tools"); wait <= time.Second {
		t.Fatalf("the boot waits %s for zeta/tools's limit, want longer than acme/widgets's hold of a second, which this test is about", wait)
	}
	if err := boot.readyOrExit(t); err != nil {
		t.Fatalf("boot = %v, want it to come up, the admission's read answered from the held branch", err)
	}
	if widgets := github.readsOf("acme/widgets"); widgets < 2 {
		t.Fatalf("GitHub was read for acme/widgets %d times, want the boot's refused read and the reconciliation's after zeta/tools's wait ended", widgets)
	}
	if held := linesFor(loggedLines(t, logs.String(), heldLine), "acme/widgets"); len(held) < 2 {
		t.Fatalf("held lines for acme/widgets = %v, want the boot's and the reconciliation's read", held)
	}
	if waits := linesFor(loggedLines(t, logs.String(), waitLine), "zeta/tools"); len(waits) != 1 || waits[0]["rateLimitReset"] != strconv.FormatInt(zetaReset.Load(), 10) {
		t.Fatalf("wait lines for zeta/tools = %v, want one naming GitHub's reset", waits)
	}
}

// The daemon itself boots through a spent rate limit on a repository it holds no branch for:
// GitHub refuses the boot's read of the project's repository with a 403 naming no call left until a
// reset half an hour away, and then answers. The boot waits the limit out, says so once, reads
// once more and comes up, where before it exited after its appauth stage with GitHub's refusal.
// The wait is the daemon's fake clock's, which the test moves past it (fireLimitWait). A boot that
// reads again before its wait has ended shows it whatever moment the machine runs that read: a read
// that reaches the stand-in before the move does so at the fake clock's reading before the wait's
// end, which the stand-in records; and a read still in flight at the move has its own bound
// (appMintAttempt, armed on the same clock as the read began) ended by the move, so it fails and
// the boot leaves the repository out (waitOutLimits' unread) rather than serving it. A read that
// begins only once the clock has moved is a read after the wait on the daemon's own clock.
func TestABootThroughASpentRateLimitComesUp(t *testing.T) {
	var reset atomic.Int64
	clk := clocktest.New(time.Now())
	var readsMu sync.Mutex
	var readAt []time.Time
	spends := spendsOnce("acme/widgets", limitAhead, &reset)
	github := newLimitedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}, func(w http.ResponseWriter, repo string, read int) bool {
		readsMu.Lock()
		readAt = append(readAt, clk.Now())
		readsMu.Unlock()
		return spends(w, repo, read)
	})
	logs := &syncBuffer{}
	boot := bootOnGitHub(t, workflowConfig(t, workflowNATS(t)), &workflowTokenRecorder{}, github.url, clk, slog.New(slog.NewJSONHandler(logs, nil)))
	requireResetWait(t, "acme/widgets", fireLimitWait(t, clk, logs, github, "acme/widgets"))
	// fireLimitWait moved the clock to the wait's end, and nothing else moves it.
	waitEnd := clk.Now()
	if err := boot.readyOrExit(t); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit and come up", err)
	}
	if reads := github.readsOf("acme/widgets"); reads != 2 {
		t.Fatalf("GitHub was read for acme/widgets %d times, want the refused read and one more once the wait to its reset ended", reads)
	}
	readsMu.Lock()
	again := readAt[1]
	readsMu.Unlock()
	if again.Before(waitEnd) {
		t.Fatalf("the boot read acme/widgets again at %s on its clock, before its wait ended at %s: it read inside GitHub's limit", again, waitEnd)
	}
	stage := loggedLines(t, testwait.Next(t, "the boot's stage line naming the repositories it serves", logs.Stream(), func(line string) bool {
		return strings.Contains(line, `"stage":"repositories"`)
	}), `"stage":"repositories"`)
	if accepted, _ := stage[0]["accepted"].([]any); !slices.Contains(accepted, any("acme/widgets")) {
		t.Fatalf("the boot serves %v and leaves out %v, want acme/widgets served on its read once the wait ended", stage[0]["accepted"], stage[0]["refused"])
	}
	if waits := loggedLines(t, logs.String(), waitLine); len(waits) != 1 || waits[0]["repo"] != "acme/widgets" || waits[0]["rateLimitReset"] != strconv.FormatInt(reset.Load(), 10) {
		t.Fatalf("wait lines = %v, want one naming acme/widgets and GitHub's reset", waits)
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
			if got := github.readsOf("zeta/tools"); got != 1 {
				t.Fatalf("GitHub was read for zeta/tools %d times, want once", got)
			}
		})
	}
}
