package replay

import (
	"cmp"
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
// committer's time, the pull request it merged (none where 0), which its subject names by GitHub's
// squash suffix or, for a merge commit (mergeCommit), only as GitHub's merge commit subject does,
// with no suffix, its parents, the first parent first, and whether the listing lacks it whatever
// its time. mergedAt, mergedAs and into are GitHub's record of that pull request's merge (pull):
// its merged_at, its merge_commit_sha and its base.ref, where unset the committer's time, the
// commit's own sha and main, so that the commit is the pull request's merge into main, merged as
// it was committed. bare says GitHub's list of the pull requests of the commit leaves out
// merge_commit_sha, as that list's schema allows, broken that GitHub answers the read of the pull
// request with no pull request, and refused how many reads of the pull request by its number GitHub
// answers with its 422 of too many files changed (tooManyFilesChanged) before it serves one, every
// read where refused is below 0.
type stubCommit struct {
	sha         string
	at          time.Time
	pr          int
	mergeCommit bool
	parents     []string
	unlisted    bool
	mergedAt    time.Time
	mergedAs    string
	into        string
	bare        bool
	broken      bool
	refused     int
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

// merge is c as collect-merges writes it, on main: where GitHub records c's pull request merged as
// c into main (pull), at that merge's time, with the pull request, as GitHub's merge event;
// otherwise at its committer's time, with no pull request; and either way with its committer's
// time as Committed.
func (c stubCommit) merge() Merge {
	if c.pr != 0 && cmp.Or(c.mergedAs, c.sha) == c.sha && cmp.Or(c.into, "main") == "main" {
		return Merge{SHA: c.sha, At: cmp.Or(c.mergedAt, c.at), Committed: c.at, Branch: "main", PR: c.pr, Event: true}
	}
	return Merge{SHA: c.sha, At: c.at, Committed: c.at, Branch: "main"}
}

// committed is c as a window collected before collect-merges read merge events holds it: on main,
// at its committer's time, with the pull request its subject's squash suffix names, none for a
// merge commit.
func (c stubCommit) committed() Merge {
	if c.mergeCommit {
		return Merge{SHA: c.sha, At: c.at, Branch: "main"}
	}
	return Merge{SHA: c.sha, At: c.at, Branch: "main", PR: c.pr}
}

// listed is c as GitHub's commit listing and its read of one commit hold it, cut to what
// collect-merges reads.
func (c stubCommit) listed() map[string]any {
	message := "feat(widgets): a change\n\nThe change's body."
	switch {
	case c.pr != 0 && c.mergeCommit:
		message = fmt.Sprintf("Merge pull request #%d from acme/feature\n\nThe change's body.", c.pr)
	case c.pr != 0:
		message = fmt.Sprintf("feat(widgets): a change (#%d)\n\nThe change's body.", c.pr)
	}
	parents := []map[string]string{}
	for _, parent := range c.parents {
		parents = append(parents, map[string]string{"sha": parent})
	}
	return map[string]any{"sha": c.sha, "parents": parents,
		"commit": map[string]any{"message": message, "committer": map[string]any{"date": c.at.Format(time.RFC3339)}}}
}

// pull is the pull request c merged as GitHub's listings of pull requests hold it, cut to
// its merge: closed, and merged at mergedAt as the commit mergedAs into into (stubCommit). GET
// /pulls/<n> adds merged, which the listings leave out (githubrest.PullRequest).
func (c stubCommit) pull() map[string]any {
	merged := cmp.Or(c.mergedAt, c.at).Format(time.RFC3339)
	return map[string]any{"number": c.pr, "state": "closed", "merged_at": merged, "updated_at": merged,
		"merge_commit_sha": cmp.Or(c.mergedAs, c.sha), "base": map[string]any{"ref": cmp.Or(c.into, "main")}}
}

// githubListing serves acme/widgets's main from history, the branch's commits newest first.
// GitHub's commit listing of main is served as GitHub filters it: on one page, each commit whose
// committer's time is at or after the request's since and, where the request has an until, at or
// before it, but a commit marked unlisted. Each commit of history, unlisted or not, is served by
// its sha (/repos/acme/widgets/commits/<sha>). The pull request each commit merged is served as
// GitHub records its merge (stubCommit.pull): by its number (/repos/acme/widgets/pulls/<n>), with
// no pull request where the commit is broken, and GitHub's 422 of too many files changed for as
// many reads as the commit's refused says; and among the pull requests of the commit it was merged
// as (/repos/acme/widgets/commits/<sha>/pulls), with no merge_commit_sha where the commit is bare.
// A request for anything else, or without the token, is refused.
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
		if number, one := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"); one {
			for _, commit := range history {
				if commit.pr != 0 && strconv.Itoa(commit.pr) == number {
					reads := 0 // of this pull request, this one among them
					for _, request := range requests.all() {
						if request.Target == r.URL.RequestURI() {
							reads++
						}
					}
					if commit.refused < 0 || reads <= commit.refused {
						http.Error(w, tooManyFilesChanged.body, tooManyFilesChanged.status)
						return
					}
					if commit.broken {
						serve("no pull request")
						return
					}
					pull := commit.pull()
					pull["merged"] = true
					serve(pull)
					return
				}
			}
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if sha, one := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/commits/"); one {
			if sha, pulls := strings.CutSuffix(sha, "/pulls"); pulls {
				page := []map[string]any{}
				for _, commit := range history {
					if commit.pr != 0 && cmp.Or(commit.mergedAs, commit.sha) == sha {
						pull := commit.pull()
						if commit.bare {
							delete(pull, "merge_commit_sha")
						}
						page = append(page, pull)
					}
				}
				serve(page)
				return
			}
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
// githubListing: what it answered, the merges written and how many commits it left unread
// (MergesCollected), the window it wrote into, that window's files before it ran, each wait it
// made, recorded rather than slept, and the requests the stub served.
type walkRun struct {
	merges   []Merge
	unread   int
	err      error
	window   string
	before   map[string][]byte
	waits    []time.Duration
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
	collected, err := CollectMerges(context.Background(), MergesConfig{Repo: "acme/widgets", Branch: "main",
		From: walkFrom, To: walkTo, Token: stubGitHubToken, API: server.URL, Window: run.window,
		Wait: func(_ context.Context, d time.Duration) error {
			run.waits = append(run.waits, d)
			return nil
		}})
	run.merges, run.unread, run.err = collected.Merges, collected.Unread, err
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
// before since, and keeps the span's commits, since none of them is left out. Its first requests
// are the listing and that one read, and every request after them is a read of the merge of a
// commit walked (eventRead), each a GET: none of the parent's. It fails where collect-merges
// refuses the walk or does not read the parent, or reads it after the merges.
func TestCollectMergesEndsAQuietBranchsWalkAtAParentBeforeTheListing(t *testing.T) {
	t.Parallel()
	quiet := stubbed(t, 0x500, "2026-11-16T12:00:00Z", 500, 0x499)
	first := stubbed(t, 0x501, "2026-11-20T03:00:00Z", 501, 0x500)
	tip := stubbed(t, 0x502, "2026-11-20T05:00:00Z", 502, 0x501)
	run := collectWalk(t, tip, first, quiet)
	run.wantWritten(t, []Merge{first.merge(), tip.merge()})
	want := []stubRequest{
		{Method: http.MethodGet, Target: "/repos/acme/widgets/commits?per_page=100&sha=main&since=2026-11-19T00%3A00%3A00Z"},
		{Method: http.MethodGet, Target: "/repos/acme/widgets/commits/" + quiet.sha},
	}
	if len(run.requests) < len(want) || !slices.Equal(run.requests[:len(want)], want) {
		t.Fatalf("collect-merges sent %v, want the listing and a read of %s first: %v", run.requests, quiet.sha, want)
	}
	for _, request := range run.requests[len(want):] {
		if !eventRead(request, tip, first) {
			t.Errorf("collect-merges sent %s %s after the listing and the read of %s, want reads of the merges of %s and %s alone",
				request.Method, request.Target, quiet.sha, first.sha, tip.sha)
		}
	}
}

// eventRead says whether request is a GET of how GitHub records the merge of one of commits: of
// the pull request its subject names (/repos/acme/widgets/pulls/<n>), or of the pull requests
// GitHub lists for it (/repos/acme/widgets/commits/<sha>/pulls).
func eventRead(request stubRequest, commits ...stubCommit) bool {
	target, err := url.Parse(request.Target)
	if err != nil || request.Method != http.MethodGet {
		return false
	}
	return slices.ContainsFunc(commits, func(c stubCommit) bool {
		return target.Path == "/repos/acme/widgets/commits/"+c.sha+"/pulls" ||
			c.pr != 0 && target.Path == "/repos/acme/widgets/pulls/"+strconv.Itoa(c.pr)
	})
}

// collect-merges lists main from a day before the span's start, so the walk reaches below the
// span, and with no until, so a first parent committed after its child is listed: its first
// request is sha=main, since=2026-11-19T00:00:00Z and per_page=100, with no until, and every
// request after it reads the merge of a commit walked (eventRead), no other page of commits.
func TestCollectMergesListsFromADayBeforeTheSpanWithNoUntil(t *testing.T) {
	t.Parallel()
	below := stubbed(t, 0x400, "2026-11-19T12:00:00Z", 400, 0x399)
	tip := stubbed(t, 0x401, "2026-11-20T05:00:00Z", 401, 0x400)
	run := collectWalk(t, tip, below)
	if run.err != nil {
		t.Fatalf("CollectMerges: %v", run.err)
	}
	if len(run.requests) == 0 {
		t.Fatal("collect-merges sent no request, want the listing first")
	}
	for _, request := range run.requests[1:] {
		if !eventRead(request, tip, below) {
			t.Errorf("collect-merges sent %s %s after the listing, want reads of the merges of the commits walked alone", request.Method, request.Target)
		}
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

// A pull request's merge lands on main at GitHub's merge event, its merged_at, which can come well
// after its commit's committer's time. collect-merges places each first-parent commit of main at
// the merge event of the pull request GitHub records as merged as that commit (merge_commit_sha)
// into main (base.ref), with that pull request, every other commit at its committer's time with
// none, and keeps the commits so placed in [walkFrom, walkTo). Here #601, committed at 23:40 the
// day before, merged at 00:00, the span's start, and is kept at 00:00; #602, committed at 01:00,
// merged at 01:45 and is kept at 01:45; and #605, committed at 05:20, merged at 06:00, the span's
// end, and is not kept. #607 and #608 merged at 02:50 and 03:20 as merge commits, whose subjects
// name no squash suffix: each is found among the pull requests GitHub lists for its commit, #608
// there without its merge_commit_sha, and kept at its merge with its pull request. #603's subject
// names a pull request GitHub merged as another commit, and #604's one GitHub merged into
// release-1, each after the span: neither merge event is the commit's own, so each is kept at its
// committer's time, and with no pull request. A stretch's M counts the commits as the window holds
// them: ACME-1's, from 23:50 to 01:50, whose own pull request is #602, holds #601 and leaves its
// own merge out, M = 1; ACME-2's, from 04:30 to 06:00, whose own is #699, holds none, M = 0;
// ACME-3's, from 02:30 to 02:55, whose own is #607, leaves its own merge out, M = 0; and
// ACME-4's, from 01:50 to 02:30, whose own is #603, holds the commit whose subject names #603 and
// does not take it for its own merge, M = 1, its own merge unknown. At the commits' committer's
// times, with the pull requests their subjects' squash suffixes name, as a window collected before
// merge events holds them, they would be 0, 1, 1 and 0. Each commit kept also keeps its committer's
// time as committed, beside at, so #601's line reads "at":"2026-11-20T00:00:00Z" and
// "committed":"2026-11-19T23:40:00Z" in RFC 3339; the stretches' counts read at alone. It fails
// where collect-merges keeps a commit by its committer's time, takes a merge event from a pull
// request that did not merge that commit into main, keeps a pull request GitHub's record does not
// bear out, or writes a line without its committer's time.
func TestCollectMergesKeepsEachMergeAtGitHubsMergeEvent(t *testing.T) {
	t.Parallel()
	root := stubbed(t, 0x600, "2026-11-19T12:00:00Z", 600, 0x5ff)
	early := stubbed(t, 0x601, "2026-11-19T23:40:00Z", 601, 0x600)
	early.mergedAt = capturedTime(t, "2026-11-20T00:00:00Z")
	queued := stubbed(t, 0x602, "2026-11-20T01:00:00Z", 602, 0x601)
	queued.mergedAt = capturedTime(t, "2026-11-20T01:45:00Z")
	copied := stubbed(t, 0x603, "2026-11-20T02:00:00Z", 603, 0x602)
	copied.mergedAt, copied.mergedAs = capturedTime(t, "2026-11-20T06:30:00Z"), stubSHA(0x6f3)
	joined := stubbed(t, 0x607, "2026-11-20T02:40:00Z", 607, 0x603, 0x6e7)
	joined.mergeCommit, joined.mergedAt = true, capturedTime(t, "2026-11-20T02:50:00Z")
	joinedBare := stubbed(t, 0x608, "2026-11-20T03:00:00Z", 608, 0x607, 0x6e8)
	joinedBare.mergeCommit, joinedBare.bare, joinedBare.mergedAt = true, true, capturedTime(t, "2026-11-20T03:20:00Z")
	released := stubbed(t, 0x604, "2026-11-20T04:00:00Z", 604, 0x608)
	released.mergedAt, released.into = capturedTime(t, "2026-11-20T06:15:00Z"), "release-1"
	late := stubbed(t, 0x605, "2026-11-20T05:20:00Z", 605, 0x604)
	late.mergedAt = capturedTime(t, "2026-11-20T06:00:00Z")
	tip := stubbed(t, 0x606, "2026-11-20T07:00:00Z", 606, 0x605)
	tip.mergedAt = capturedTime(t, "2026-11-20T07:20:00Z")
	history := []stubCommit{tip, late, released, joinedBare, joined, copied, queued, early, root}
	run := collectWalk(t, history...)
	run.wantWritten(t, []Merge{
		{SHA: early.sha, At: early.mergedAt, Committed: early.at, Branch: "main", PR: 601, Event: true},
		{SHA: queued.sha, At: queued.mergedAt, Committed: queued.at, Branch: "main", PR: 602, Event: true},
		{SHA: copied.sha, At: copied.at, Committed: copied.at, Branch: "main"},
		{SHA: joined.sha, At: joined.mergedAt, Committed: joined.at, Branch: "main", PR: 607, Event: true},
		{SHA: joinedBare.sha, At: joinedBare.mergedAt, Committed: joinedBare.at, Branch: "main", PR: 608, Event: true},
		{SHA: released.sha, At: released.at, Committed: released.at, Branch: "main"},
	})
	raw, err := os.ReadFile(filepath.Join(run.window, "merges", "acme__widgets.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `"at":"2026-11-20T00:00:00Z","committed":"2026-11-19T23:40:00Z"`; !strings.Contains(string(raw), want) ||
		strings.Count(string(raw), `"committed":`) != 6 {
		t.Errorf("collect-merges wrote the lines\n%s\nwant each with its committer's time, #601's holding %s", raw, want)
	}
	read, err := ReadWindow(run.window)
	if err != nil {
		t.Fatalf("ReadWindow of the window collect-merges wrote into: %v", err)
	}
	branches := Trace{Branches: map[string]string{"acme/widgets": "main"}}
	placed := newBaseMerges(read, branches)
	var byCommitter []Merge
	for _, commit := range history {
		byCommitter = append(byCommitter, commit.committed())
	}
	committed := newBaseMerges(Window{Merges: map[string][]Merge{"acme/widgets": byCommitter}}, branches)
	for _, s := range []struct {
		issue      string
		own        int
		start, end string
		merges     int
		unknown    bool
	}{
		{"ACME-1", 602, "2026-11-19T23:50:00Z", "2026-11-20T01:50:00Z", 1, false},
		{"ACME-2", 699, "2026-11-20T04:30:00Z", "2026-11-20T06:00:00Z", 0, false},
		{"ACME-3", 607, "2026-11-20T02:30:00Z", "2026-11-20T02:55:00Z", 0, false},
		{"ACME-4", 603, "2026-11-20T01:50:00Z", "2026-11-20T02:30:00Z", 1, true},
	} {
		start, end := capturedTime(t, s.start), capturedTime(t, s.end)
		if count, _ := committed.between("acme/widgets", s.own, start, end); count == s.merges {
			t.Fatalf("%s's stretch from %s to %s counts M = %d at the commits' committer's times and their subjects' pull requests too, "+
				"so it tells no merge event from a committer's time", s.issue, s.start, s.end, count)
		}
		if count, unknown := placed.between("acme/widgets", s.own, start, end); count != s.merges || unknown != s.unknown {
			t.Errorf("%s's stretch from %s to %s counts M = %d over the window's merges (own merge unknown: %t), want %d (%t)",
				s.issue, s.start, s.end, count, unknown, s.merges, s.unknown)
		}
	}
}

// An answer of GitHub's that is no pull request is refused, as one that is no commit is: GitHub
// answers the read of #701, which main's commit of 03:00 names, with a string. collect-merges
// refuses, naming the commit and the pull request, and writes nothing into the window. It fails
// where the commit is kept at its committer's time as one GitHub records no merge of.
func TestCollectMergesRefusesAnAnswerThatIsNoPullRequest(t *testing.T) {
	t.Parallel()
	below := stubbed(t, 0x700, "2026-11-19T12:00:00Z", 700, 0x6ff)
	broken := stubbed(t, 0x701, "2026-11-20T03:00:00Z", 701, 0x700)
	broken.broken = true
	run := collectWalk(t, broken, below)
	if run.err == nil {
		t.Errorf("CollectMerges answered %s and no error, want a refusal", describeReading(run.merges))
	} else {
		for _, part := range []string{broken.sha, "#701", "no pull request"} {
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

// eventReads is the targets of the requests among requests that read how GitHub records the merge
// of c (eventRead), in order.
func eventReads(requests []stubRequest, c stubCommit) []string {
	var targets []string
	for _, request := range requests {
		if eventRead(request, c) {
			targets = append(targets, request.Target)
		}
	}
	return targets
}

// GitHub's answer to a read of a pull request computes the pull request's diff, and GitHub can
// refuse it for now with a 422 whose message says too many files changed (githubTooManyFiles): a
// collection of a real repository met it, and the same read answered 200 OK five minutes later. The
// read is tried again as a 5xx is: GitHub refuses the first read of #801, which main's commit of
// 03:00 names, and answers the read after one wait of a second, told through the reader's wait. The
// commit lands at #801's merge, 03:30, with its pull request, and no commit is left unread. It
// fails where the 422 fails the collection.
func TestCollectMergesReadsAPullRequestAgainAfterGitHubsTooManyFiles(t *testing.T) {
	t.Parallel()
	below := stubbed(t, 0x800, "2026-11-19T12:00:00Z", 800, 0x7ff)
	wide := stubbed(t, 0x801, "2026-11-20T03:00:00Z", 801, 0x800)
	wide.mergedAt, wide.refused = capturedTime(t, "2026-11-20T03:30:00Z"), 1
	run := collectWalk(t, wide, below)
	run.wantWritten(t, []Merge{{SHA: wide.sha, At: wide.mergedAt, Committed: wide.at, Branch: "main", PR: 801, Event: true}})
	if !slices.Equal(run.waits, []time.Duration{retryFirstWait}) || run.unread != 0 {
		t.Errorf("collect-merges waited %v and left %d commits unread, want one wait of %s and none", run.waits, run.unread, retryFirstWait)
	}
	if got, want := eventReads(run.requests, wide), []string{"/repos/acme/widgets/pulls/801", "/repos/acme/widgets/pulls/801"}; !slices.Equal(got, want) {
		t.Errorf("collect-merges read the merge of %s with %v, want %v", wide.sha, got, want)
	}
}

// A pull request GitHub still refuses to read whole after the reader's tries is matched by GitHub's
// list of the commit's pull requests, which computes no diff and gives the pull request's
// merge_commit_sha, merged_at and base.ref: every read of #801, which main's commit of 03:00 names,
// answers the 422 of too many files changed, retryAttempts reads, each after a wait but the first,
// and the commit's list holds #801, merged as that commit into main at 03:30. The commit lands there
// with #801, and is not counted unread. It fails where the refusal fails the collection, or where
// the commit keeps its committer's time.
func TestCollectMergesMatchesAPullRequestGitHubWillNotReadByTheCommitsList(t *testing.T) {
	t.Parallel()
	below := stubbed(t, 0x800, "2026-11-19T12:00:00Z", 800, 0x7ff)
	wide := stubbed(t, 0x801, "2026-11-20T03:00:00Z", 801, 0x800)
	wide.mergedAt, wide.refused = capturedTime(t, "2026-11-20T03:30:00Z"), -1
	run := collectWalk(t, wide, below)
	run.wantWritten(t, []Merge{{SHA: wide.sha, At: wide.mergedAt, Committed: wide.at, Branch: "main", PR: 801, Event: true}})
	want := append(slices.Repeat([]string{"/repos/acme/widgets/pulls/801"}, retryAttempts), "/repos/acme/widgets/commits/"+wide.sha+"/pulls?per_page=100")
	if got := eventReads(run.requests, wide); !slices.Equal(got, want) {
		t.Errorf("collect-merges read the merge of %s with %v, want %v", wide.sha, got, want)
	}
	if len(run.waits) != retryAttempts-1 || run.unread != 0 {
		t.Errorf("collect-merges waited %v and left %d commits unread, want %d waits and none", run.waits, run.unread, retryAttempts-1)
	}
}

// Where GitHub still refuses to read a pull request whole after the reader's tries, and its list of
// the commit's pull requests gives nothing to match it by, nothing GitHub answers says whether it
// merged as the commit. The collection goes on: it keeps the commit at its committer's time, with no
// merge event and no pull request, as one no pull request matches, and counts it unread, so that a
// run again later can place it. Every read of #801 answers the 422 of too many files changed, and
// the list of main's commit of 03:00 does not hold #801, which the commit names; holds #801 without
// its merge_commit_sha; or, for a merge commit, which names no pull request, holds #801 without its
// merge_commit_sha, so that #801 is read whole to match it. It fails where the refusal fails the
// collection, where the commit lands at a merge event, or where it is not counted.
func TestCollectMergesKeepsACommitWhosePullRequestDoesNotReadAtItsCommitterTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(c *stubCommit)
	}{
		{name: "the list does not hold it", set: func(c *stubCommit) { c.mergedAs = stubSHA(0x8f1) }},
		{name: "the list gives no merge_commit_sha of it", set: func(c *stubCommit) { c.bare = true }},
		{name: "a merge commit whose list gives no merge_commit_sha of it", set: func(c *stubCommit) { c.mergeCommit, c.bare = true, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			below := stubbed(t, 0x800, "2026-11-19T12:00:00Z", 800, 0x7ff)
			wide := stubbed(t, 0x801, "2026-11-20T03:00:00Z", 801, 0x800)
			wide.mergedAt, wide.refused = capturedTime(t, "2026-11-20T03:30:00Z"), -1
			tc.set(&wide)
			run := collectWalk(t, wide, below)
			run.wantWritten(t, []Merge{{SHA: wide.sha, At: wide.at, Committed: wide.at, Branch: "main"}})
			if run.unread != 1 {
				t.Errorf("collect-merges left %d commits unread, want 1", run.unread)
			}
			reads := eventReads(run.requests, wide)
			if got := slices.DeleteFunc(slices.Clone(reads), func(target string) bool { return target != "/repos/acme/widgets/pulls/801" }); len(got) != retryAttempts {
				t.Errorf("collect-merges read the merge of %s with %v, want %d reads of #801", wide.sha, reads, retryAttempts)
			}
		})
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

// GitHub's 422 answers of the tests of a read it refuses: its refusal of a pull request whose diff
// it cannot compute for now, as a collection of a real repository met it (githubTooManyFiles), and
// a validation failure, a 422 that no later read clears.
var (
	tooManyFilesChanged = stubAnswer{status: http.StatusUnprocessableEntity,
		body: `{"message":"The request could not be processed because too many files changed","documentation_url":"https://docs.github.com/rest/pulls/pulls#get-a-pull-request","status":"422"}`}
	validationFailed = `{"message":"Validation Failed","documentation_url":"https://docs.github.com/rest","status":"422"}`
)

// GitHub answers 422 for refusals that no later read clears, a validation failure among them. Of
// them, only its refusal of a pull request whose diff it cannot compute for now, whose message says
// too many files changed (githubTooManyFiles), is tried again, after a wait of a second, as a 5xx
// is. Any other 422 is the read's answer at once, with no wait and one request, naming GitHub's
// status and words. It fails where every 422 is tried again, or where none is.
func TestGitHubReaderTriesNo422AgainButTooManyFilesChanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		answer  stubAnswer
		refused bool
		waits   []time.Duration
	}{
		{name: "a validation failure", answer: stubAnswer{status: http.StatusUnprocessableEntity, body: validationFailed}, refused: true},
		{name: "too many files changed", answer: tooManyFilesChanged, waits: []time.Duration{retryFirstWait}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			read := readThroughLimits(t, tc.answer, emptyPage)
			switch {
			case tc.refused && (read.err == nil || !strings.Contains(read.err.Error(), "422 Unprocessable Entity") || !strings.Contains(read.err.Error(), "Validation Failed")):
				t.Errorf("the read answered %v, want GitHub's 422 and its message", read.err)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
