package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
)

// The span the tests of collect-merges's walk read acme/widgets's main over: the first six hours
// of 2026-11-20.
var (
	walkFrom = time.Date(2026, time.November, 20, 0, 0, 0, 0, time.UTC)
	walkTo   = time.Date(2026, time.November, 20, 6, 0, 0, 0, time.UTC)
)

// stubCommit is one commit of acme/widgets's main that githubListing serves: its sha, its
// committer's time, the pull request its subject's squash suffix names (none where 0), its
// parents, the first parent first, and whether the listing lacks it whatever its time.
type stubCommit struct {
	sha      string
	at       time.Time
	pr       int
	parents  []string
	unlisted bool
}

// stubSHA is the sha of the stub commit numbered n: n in hex, 40 digits long.
func stubSHA(n int) string { return fmt.Sprintf("%040x", n) }

// stubbed is the stub commit numbered n, committed at at (RFC 3339), the squash merge of pull
// request pr (none where 0), whose parents are the commits numbered parents, the first parent
// first.
func stubbed(t *testing.T, n int, at string, pr int, parents ...int) stubCommit {
	t.Helper()
	commit := stubCommit{sha: stubSHA(n), at: capturedTime(t, at), pr: pr}
	for _, parent := range parents {
		commit.parents = append(commit.parents, stubSHA(parent))
	}
	return commit
}

// merge is c as collect-merges writes it: on main, at its committer's time, with its pull request.
func (c stubCommit) merge() Merge {
	return Merge{SHA: c.sha, At: c.at, Branch: "main", PR: c.pr}
}

// listed is c as GitHub's commit listing and its read of one commit hold it, cut to what
// collect-merges reads.
func (c stubCommit) listed() map[string]any {
	message := "feat(widgets): a change\n\nThe change's body."
	if c.pr != 0 {
		message = fmt.Sprintf("feat(widgets): a change (#%d)\n\nThe change's body.", c.pr)
	}
	parents := []map[string]string{}
	for _, parent := range c.parents {
		parents = append(parents, map[string]string{"sha": parent})
	}
	return map[string]any{"sha": c.sha, "parents": parents,
		"commit": map[string]any{"message": message, "committer": map[string]any{"date": c.at.Format(time.RFC3339)}}}
}

// githubListing serves acme/widgets's main from history, the branch's commits newest first.
// GitHub's commit listing of main is served as GitHub filters it: on one page, each commit whose
// committer's time is at or after the request's since and, where the request has an until, at or
// before it, but a commit marked unlisted. Each commit of history, unlisted or not, is served by
// its sha (/repos/acme/widgets/commits/<sha>). A request for anything else, or without the token,
// is refused.
func githubListing(t *testing.T, history []stubCommit) (*httptest.Server, *stubRequests) {
	t.Helper()
	requests := &stubRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r)
		serve := func(answer any) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(answer); err != nil {
				t.Errorf("serve %s: %v", r.URL.RequestURI(), err)
			}
		}
		query := r.URL.Query()
		if r.Header.Get("Authorization") != "Bearer "+stubGitHubToken || r.Method != http.MethodGet {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if sha, one := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/commits/"); one {
			for _, commit := range history {
				if commit.sha == sha {
					serve(commit.listed())
					return
				}
			}
			http.Error(w, `{"message":"No commit found for SHA: `+sha+`"}`, http.StatusUnprocessableEntity)
			return
		}
		if r.URL.Path != "/repos/acme/widgets/commits" || query.Get("sha") != "main" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		bound := func(name string) *time.Time {
			if !query.Has(name) {
				return nil
			}
			at, err := time.Parse(time.RFC3339, query.Get(name))
			if err != nil {
				t.Errorf("the listing's %s is %q, no RFC 3339 time", name, query.Get(name))
			}
			return &at
		}
		since, until := bound("since"), bound("until")
		page := []map[string]any{}
		for _, commit := range history {
			if !commit.unlisted && (since == nil || !commit.at.Before(*since)) && (until == nil || !commit.at.After(*until)) {
				page = append(page, commit.listed())
			}
		}
		serve(page)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// walkRun is one collect-merges of acme/widgets's main over [walkFrom, walkTo) against
// githubListing: what it answered, the window it wrote into, that window's files before it ran,
// and the requests the stub served.
type walkRun struct {
	merges   []Merge
	err      error
	window   string
	before   map[string][]byte
	requests []stubRequest
}

// collectWalk runs collect-merges of acme/widgets's main over [walkFrom, walkTo) against
// githubListing serving history, newest first, into a window of its own that holds no merges.
func collectWalk(t *testing.T, history ...stubCommit) walkRun {
	t.Helper()
	run := walkRun{window: filepath.Join(t.TempDir(), "window")}
	if err := (Window{Project: "ACME", From: walkFrom, To: walkTo}).Write(run.window); err != nil {
		t.Fatal(err)
	}
	run.before = windowFiles(t, run.window)
	server, seen := githubListing(t, history)
	run.merges, run.err = CollectMerges(context.Background(), MergesConfig{Repo: "acme/widgets", Branch: "main",
		From: walkFrom, To: walkTo, Token: stubGitHubToken, API: server.URL, Window: run.window})
	run.requests = seen.all()
	return run
}

// wantWritten fails t unless run answered want, oldest first, and wrote it into its window as
// the merges of acme/widgets.
func (run walkRun) wantWritten(t *testing.T, want []Merge) {
	t.Helper()
	if run.err != nil {
		t.Fatalf("CollectMerges: %v", run.err)
	}
	if !sameReading(run.merges, want) {
		t.Errorf("CollectMerges answered\n  %s\nwant\n  %s", describeReading(run.merges), describeReading(want))
	}
	read, err := ReadWindow(run.window)
	if err != nil {
		t.Fatalf("ReadWindow of the window collect-merges wrote into: %v", err)
	}
	if !sameReading(read.Merges["acme/widgets"], want) {
		t.Errorf("the window's merges of acme/widgets are\n  %s\nwant\n  %s", describeReading(read.Merges["acme/widgets"]), describeReading(want))
	}
}

// windowFiles is every file under dir, by its slash-separated path under dir, with its bytes.
func windowFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(name)], err = os.ReadFile(path)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A first parent can be committed after its child, and the walk goes on through it. main's
// first-parent chain holds a commit of 04:00 whose first parent was committed at 08:00, after the
// span's end; below that parent are two commits inside the span and one of the day before, whose
// first parent the listing lacks, where the walk ends. Every first-parent commit in [walkFrom,
// walkTo) is kept, and the one of 08:00 is walked and not kept. It fails where the listing has an
// until, which leaves out the parent of 08:00, so that the walk stops at the commit of 04:00.
func TestCollectMergesWalksThroughAParentCommittedAfterTheSpan(t *testing.T) {
	before := stubbed(t, 0x100, "2026-11-19T23:00:00Z", 100, 0x99)
	first := stubbed(t, 0x101, "2026-11-20T01:00:00Z", 101, 0x100)
	second := stubbed(t, 0x102, "2026-11-20T02:00:00Z", 102, 0x101)
	late := stubbed(t, 0x103, "2026-11-20T08:00:00Z", 103, 0x102)
	child := stubbed(t, 0x104, "2026-11-20T04:00:00Z", 104, 0x103)
	tip := stubbed(t, 0x105, "2026-11-20T05:00:00Z", 105, 0x104)
	run := collectWalk(t, tip, child, late, second, first, before)
	run.wantWritten(t, []Merge{first.merge(), second.merge(), child.merge(), tip.merge()})
}

// The walk starts at the branch's tip, the first commit listed, whatever its time. main's tip was
// committed at 09:00, after the span, and its first parent at 07:00: a merge commit, whose first
// parent is a squash merge of 05:00 and whose second a commit of 05:30 on the merged branch, both
// above the branch's root, of 03:00. The two first-parent commits in the span are kept, the two
// after it are walked and not kept, and the walk ends at the root, inside the span, as a branch's
// history does. It fails where the listing has an until: the first commit listed is then the
// merged branch's of 05:30, and a walk from it keeps that commit in place of the squash merge.
func TestCollectMergesWalksFromATipCommittedAfterTheSpan(t *testing.T) {
	root := stubbed(t, 0x200, "2026-11-20T03:00:00Z", 0)
	squash := stubbed(t, 0x201, "2026-11-20T05:00:00Z", 201, 0x200)
	side := stubbed(t, 0x202, "2026-11-20T05:30:00Z", 0, 0x200)
	merged := stubbed(t, 0x203, "2026-11-20T07:00:00Z", 0, 0x201, 0x202)
	tip := stubbed(t, 0x204, "2026-11-20T09:00:00Z", 204, 0x203)
	run := collectWalk(t, tip, merged, side, squash, root)
	run.wantWritten(t, []Merge{root.merge(), squash.merge()})
}

// A walk that ends at a first parent the listing lacks, while the last commit it walked is inside
// the span, reads that parent. One committed at or after the listing's since is a commit the
// listing should hold, so the span's history before it would be left out: collect-merges refuses
// the walk, naming the repository, the branch, the last commit walked and that parent, each with
// its time, and writes nothing into the window. Here the listing lacks the first parent, of 02:00,
// of main's commit of 03:00. It fails where the short history is written.
func TestCollectMergesRefusesAWalkThatEndsInsideTheSpan(t *testing.T) {
	lacked := stubbed(t, 0x300, "2026-11-20T02:00:00Z", 300, 0x299)
	lacked.unlisted = true
	last := stubbed(t, 0x301, "2026-11-20T03:00:00Z", 301, 0x300)
	tip := stubbed(t, 0x302, "2026-11-20T05:00:00Z", 302, 0x301)
	run := collectWalk(t, tip, last, lacked)
	if run.err == nil {
		t.Errorf("CollectMerges answered %s and no error, want a refusal", describeReading(run.merges))
	} else {
		for _, part := range []string{"acme/widgets", "main", last.sha, "2026-11-20T03:00:00Z", lacked.sha, "2026-11-20T02:00:00Z"} {
			if !strings.Contains(run.err.Error(), part) {
				t.Errorf("CollectMerges refuses with %q, which does not name %s", run.err, part)
			}
		}
	}
	if after := windowFiles(t, run.window); !reflect.DeepEqual(after, run.before) {
		t.Errorf("the window holds %v after the refusal, want its files as they were before it: %v",
			slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(run.before)))
	}
}

// A branch can be quiet for longer than the margin before the span: main's first commit in the
// span, of 03:00, has a first parent committed on 2026-11-16, days before the listing's since. The
// walk ends at that parent, which the listing lacks; collect-merges reads it, finds it committed
// before since, and keeps the span's commits, since none of them is left out. Its requests are the
// listing and that one read, each a GET. It fails where collect-merges refuses the walk or does not
// read the parent.
func TestCollectMergesEndsAQuietBranchsWalkAtAParentBeforeTheListing(t *testing.T) {
	quiet := stubbed(t, 0x500, "2026-11-16T12:00:00Z", 500, 0x499)
	first := stubbed(t, 0x501, "2026-11-20T03:00:00Z", 501, 0x500)
	tip := stubbed(t, 0x502, "2026-11-20T05:00:00Z", 502, 0x501)
	run := collectWalk(t, tip, first, quiet)
	run.wantWritten(t, []Merge{first.merge(), tip.merge()})
	want := []stubRequest{
		{Method: http.MethodGet, Target: "/repos/acme/widgets/commits?per_page=100&sha=main&since=2026-11-19T00%3A00%3A00Z"},
		{Method: http.MethodGet, Target: "/repos/acme/widgets/commits/" + quiet.sha},
	}
	if !slices.Equal(run.requests, want) {
		t.Errorf("collect-merges sent %v, want the listing and a read of %s: %v", run.requests, quiet.sha, want)
	}
}

// collect-merges lists main from a day before the span's start, so the walk reaches below the
// span, and with no until, so a first parent committed after its child is listed: its one request
// is sha=main, since=2026-11-19T00:00:00Z and per_page=100, with no until.
func TestCollectMergesListsFromADayBeforeTheSpanWithNoUntil(t *testing.T) {
	below := stubbed(t, 0x400, "2026-11-19T12:00:00Z", 400, 0x399)
	tip := stubbed(t, 0x401, "2026-11-20T05:00:00Z", 401, 0x400)
	run := collectWalk(t, tip, below)
	if run.err != nil {
		t.Fatalf("CollectMerges: %v", run.err)
	}
	if len(run.requests) != 1 {
		t.Fatalf("collect-merges sent %v, want one request", run.requests)
	}
	target, err := url.Parse(run.requests[0].Target)
	if err != nil {
		t.Fatal(err)
	}
	if query := target.Query(); query.Get("sha") != "main" || query.Get("since") != "2026-11-19T00:00:00Z" ||
		query.Get("per_page") != "100" || query.Has("until") {
		t.Errorf("collect-merges listed %s, want sha=main, since=2026-11-19T00:00:00Z and per_page=100, with no until", run.requests[0].Target)
	}
}

// stubAnswer is one answer limitStub gives: its status, the headers it carries and its body.
type stubAnswer struct {
	status int
	header map[string]string
	body   string
}

// limitStub serves answers to the requests it is sent, one a request in order and the last again
// for every request after it, and the requests it served.
func limitStub(t *testing.T, answers ...stubAnswer) (*httptest.Server, *stubRequests) {
	t.Helper()
	requests := &stubRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r)
		answer := answers[min(len(requests.all()), len(answers))-1]
		for name, value := range answer.header {
			w.Header().Set(name, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		io.WriteString(w, answer.body)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// limitRead is one read of acme/widgets's commit listing through the reader both collect-merges
// and collect-queue read GitHub with, against limitStub: the pages it read, its error, each wait it
// made, recorded rather than slept, and the requests the stub served.
type limitRead struct {
	pages    [][]byte
	err      error
	waits    []time.Duration
	requests []stubRequest
}

// readThroughLimits reads acme/widgets's commit listing from limitStub serving answers. A wait
// past the twentieth ends the read, so a reader that waits without end fails rather than hangs.
func readThroughLimits(t *testing.T, answers ...stubAnswer) limitRead {
	t.Helper()
	server, seen := limitStub(t, answers...)
	api, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var read limitRead
	reader := githubReader{api: api, token: stubGitHubToken, client: server.Client(), wait: func(_ context.Context, d time.Duration) error {
		read.waits = append(read.waits, d)
		if len(read.waits) > 20 {
			return errors.New("the test ends a read that waits a twenty-first time")
		}
		return nil
	}}
	read.pages, read.err = reader.pages(context.Background(), api.JoinPath("repos", "acme", "widgets", "commits"))
	read.requests = seen.all()
	return read
}

// The answers GitHub gives the tests of its rate limit: a listing's empty page; the message of a
// spent primary limit, which GitHub answers 403 or 429 with x-ratelimit-remaining 0; the message
// of the secondary limit; and a permission refusal, a 403 that is no rate limit.
var (
	emptyPage      = stubAnswer{status: http.StatusOK, body: "[]"}
	primarySpent   = `{"message":"API rate limit exceeded for installation ID 1.","status":"403"}`
	secondaryLimit = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","status":"403"}`
	notPermitted   = `{"message":"Resource not accessible by integration","status":"403"}`
)

// spentUntil is GitHub's answer when the primary rate limit is spent until reset: a 403 with
// x-ratelimit-remaining 0 and x-ratelimit-reset at reset, in epoch seconds.
func spentUntil(reset time.Time) stubAnswer {
	return stubAnswer{status: http.StatusForbidden, body: primarySpent, header: map[string]string{
		"X-RateLimit-Limit": "15000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}}
}

// GitHub's primary rate limit answers 403 with x-ratelimit-remaining 0, and a read is not tried
// again before x-ratelimit-reset (GitHub's "Exceeding the rate limit"). A listing refused so with
// a reset 20 minutes ahead is read again after one wait, which ends after the reset and within
// ten seconds of it, and the second request is answered. It fails where the 403 is the answer at
// once, as a refusal that is no rate limit is, or where the wait ends before the reset.
func TestGitHubReaderWaitsOutAPrimaryRateLimitUntilItsReset(t *testing.T) {
	before := time.Now()
	reset := before.Add(20 * time.Minute).Truncate(time.Second)
	read := readThroughLimits(t, spentUntil(reset), emptyPage)
	after := time.Now()
	if read.err != nil {
		t.Fatalf("the read refused with %v, want it read again after the reset", read.err)
	}
	if len(read.waits) != 1 || before.Add(read.waits[0]).Before(reset) || after.Add(read.waits[0]).After(reset.Add(10*time.Second)) {
		t.Errorf("the read waited %v, want one wait that ends after the reset at %s and within ten seconds of it",
			read.waits, reset.UTC().Format(time.RFC3339))
	}
	if len(read.requests) != 2 || len(read.pages) != 1 {
		t.Errorf("the read sent %v and read %d pages, want two requests and the one page", read.requests, len(read.pages))
	}
}

// A rate limit's retry-after names the seconds to wait (GitHub's "Exceeding the rate limit"): a
// 429 asking for 7 seconds is waited 7 seconds, once, and the read is answered after it. It fails
// where the 429 is tried again after the first wait of a read GitHub refuses for now, a second.
func TestGitHubReaderWaitsTheSecondsARetryAfterNames(t *testing.T) {
	limited := stubAnswer{status: http.StatusTooManyRequests, body: secondaryLimit, header: map[string]string{"Retry-After": "7"}}
	read := readThroughLimits(t, limited, emptyPage)
	if read.err != nil {
		t.Fatalf("the read refused with %v, want it read again after the retry-after", read.err)
	}
	if !slices.Equal(read.waits, []time.Duration{7 * time.Second}) {
		t.Errorf("the read waited %v, want one wait of 7s, the retry-after", read.waits)
	}
	if len(read.requests) != 2 {
		t.Errorf("the read sent %v, want two requests", read.requests)
	}
}

// A 403 is a rate limit's only where GitHub says so. A permission refusal, whether it carries no
// rate-limit header or the primary limit's calls left, as every answer of GitHub's does, is the
// read's answer at once: no wait and no second request. Beside it, a 403 with the same calls left
// and neither retry-after nor a remaining of 0, whose message names the secondary limit, is waited
// a minute, the least GitHub asks, and read again. It fails where a permission refusal is waited
// out, or where the secondary limit's 403 is taken for a refusal.
func TestGitHubReaderRefusesAForbiddenThatIsNoRateLimitAtOnce(t *testing.T) {
	callsLeft := map[string]string{"X-RateLimit-Limit": "15000", "X-RateLimit-Remaining": "14671", "X-RateLimit-Reset": "1795000000"}
	for _, tc := range []struct {
		name    string
		answer  stubAnswer
		refused bool
		waits   []time.Duration
	}{
		{name: "a permission refusal with no rate-limit header", answer: stubAnswer{status: http.StatusForbidden, body: notPermitted}, refused: true},
		{name: "a permission refusal with calls left", answer: stubAnswer{status: http.StatusForbidden, body: notPermitted, header: callsLeft}, refused: true},
		{name: "the secondary limit named by its message alone", answer: stubAnswer{status: http.StatusForbidden, body: secondaryLimit, header: callsLeft},
			waits: []time.Duration{time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := readThroughLimits(t, tc.answer, emptyPage)
			switch {
			case tc.refused && (read.err == nil || !strings.Contains(read.err.Error(), "403") || !strings.Contains(read.err.Error(), "Resource not accessible")):
				t.Errorf("the read answered %v, want GitHub's 403 and its message", read.err)
			case !tc.refused && read.err != nil:
				t.Errorf("the read refused with %v, want it read again after its wait", read.err)
			}
			if !slices.Equal(read.waits, tc.waits) {
				t.Errorf("the read waited %v, want %v", read.waits, tc.waits)
			}
			if want := 1 + len(tc.waits); len(read.requests) != want {
				t.Errorf("the read sent %v, want %d requests", read.requests, want)
			}
		})
	}
}

// One wait is bounded: a time to try again further ahead than an hour and five minutes, the
// primary limit's window and a margin, is refused at once, with no wait, naming that time, so the
// operator knows when to run the collector again: a reset two hours ahead, and a retry-after of
// two hours. It fails where the read waits for it, or refuses without naming the time.
func TestGitHubReaderRefusesAWaitPastTheLongest(t *testing.T) {
	before := time.Now()
	reset := before.Add(2 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		answer stubAnswer
		names  func(after time.Time) []string
	}{
		{name: "a reset two hours ahead", answer: spentUntil(reset),
			names: func(time.Time) []string { return []string{reset.UTC().Format(time.RFC3339)} }},
		{name: "a retry-after of two hours", answer: stubAnswer{status: http.StatusTooManyRequests, body: secondaryLimit, header: map[string]string{"Retry-After": "7200"}},
			names: func(after time.Time) []string {
				var times []string
				for at := before.Add(2 * time.Hour).Truncate(time.Second); !at.After(after.Add(2 * time.Hour)); at = at.Add(time.Second) {
					times = append(times, at.UTC().Format(time.RFC3339))
				}
				return times
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := readThroughLimits(t, tc.answer, emptyPage)
			times := tc.names(time.Now())
			if read.err == nil || !slices.ContainsFunc(times, func(at string) bool { return strings.Contains(read.err.Error(), at) }) {
				t.Errorf("the read answered %v, want a refusal naming the time GitHub names, one of %v", read.err, times)
			}
			if len(read.waits) != 0 || len(read.requests) != 1 {
				t.Errorf("the read waited %v and sent %v, want no wait and one request", read.waits, read.requests)
			}
		})
	}
}

// The waits of one read are bounded (GitHub's "throw an error after a specific number of
// retries"): a listing GitHub's secondary limit refuses on every request, with a 403 that names the
// limit in its message alone, is waited a minute, then two, four, eight and sixteen, five waits
// growing as GitHub asks, and is refused at the sixth refusal, naming GitHub's message. It fails
// where the read is refused at once, waits a constant time, or waits on.
func TestGitHubReaderRefusesARateLimitPastItsLastWait(t *testing.T) {
	read := readThroughLimits(t, stubAnswer{status: http.StatusForbidden, body: secondaryLimit})
	if read.err == nil || !strings.Contains(read.err.Error(), "secondary rate limit") {
		t.Errorf("the read answered %v, want a refusal naming GitHub's message", read.err)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute}
	if !slices.Equal(read.waits, want) {
		t.Errorf("the read waited %v, want %v", read.waits, want)
	}
	if len(read.requests) != len(want)+1 {
		t.Errorf("the read sent %d requests, want %d, one after each wait", len(read.requests), len(want)+1)
	}
}

// Each wait for GitHub's rate limit is told on stderr as it begins, so an operator sees why the
// collector is quiet: one line naming the page, GitHub's answer, the wait, the time it ends, the
// reset and rateLimitMargin after it, and which of the read's waits it is. It fails where a wait
// is made untold.
func TestGitHubReaderTellsEachRateLimitWaitOnStderr(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	server, _ := limitStub(t, spentUntil(reset), emptyPage)
	api, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var told strings.Builder
	reader := githubReader{api: api, token: stubGitHubToken, client: server.Client(), wait: noWait, stderr: &told}
	if _, err := reader.pages(context.Background(), api.JoinPath("repos", "acme", "widgets", "commits")); err != nil {
		t.Fatalf("the read refused with %v, want it read again after the reset", err)
	}
	lines := strings.Split(strings.TrimSuffix(told.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr holds %q, want one line for the one wait", told.String())
	}
	for _, part := range []string{"GET " + server.URL + "/repos/acme/widgets/commits", "403 Forbidden", "API rate limit exceeded",
		"until " + reset.Add(rateLimitMargin).UTC().Format(time.RFC3339), "wait 1 of at most 5"} {
		if !strings.Contains(lines[0], part) {
			t.Errorf("stderr tells the wait as %q, which does not name %q", lines[0], part)
		}
	}
}

// The read's context ends a wait for GitHub's rate limit at once: a read whose context is
// cancelled as it begins to wait out a reset 20 minutes ahead on a timer (backoff.Sleep) ends with
// the cancellation, and sends no second request. It fails where the wait outlasts the context, or
// where the read is tried again after it.
func TestGitHubReaderEndsARateLimitWaitWithItsContext(t *testing.T) {
	server, seen := limitStub(t, spentUntil(time.Now().Add(20*time.Minute)), emptyPage)
	api, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := githubReader{api: api, token: stubGitHubToken, client: server.Client(), stderr: io.Discard,
		wait: func(ctx context.Context, d time.Duration) error {
			cancel()
			return backoff.Sleep(ctx, d)
		}}
	done := make(chan error, 1)
	go func() {
		_, err := reader.pages(ctx, api.JoinPath("repos", "acme", "widgets", "commits"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the read answered %v, want the cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read still waits ten seconds after its context was cancelled")
	}
	if requests := seen.all(); len(requests) != 1 {
		t.Errorf("the read sent %v, want the one request before the wait", requests)
	}
}
