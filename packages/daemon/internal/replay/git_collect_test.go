package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitRemote is a repository a test builds with the git CLI in a directory of its own, which
// collect-git clones and fetches from as GitHub's remote: it lets a fetch name any commit it holds
// and a clone leave blobs out, as GitHub does. Each commit is made a minute after the one before.
type gitRemote struct {
	t       *testing.T
	dir     string
	minutes int
}

func newGitRemote(t *testing.T) *gitRemote {
	t.Helper()
	r := &gitRemote{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch=main")
	r.git("config", "uploadpack.allowFilter", "true")
	r.git("config", "uploadpack.allowAnySHA1InWant", "true")
	return r
}

// git runs git args in the repository, with no configuration but the repository's own, and is
// what it printed.
func (r *gitRemote) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	stamp := time.Date(2001, time.March, 5, 6, 0, 0, 0, time.UTC).Add(time.Duration(r.minutes) * time.Minute).Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=person-1",
		"GIT_AUTHOR_EMAIL=person-1@example.invalid", "GIT_COMMITTER_NAME=person-1", "GIT_COMMITTER_EMAIL=person-1@example.invalid",
		"GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files, by name, on the branch checked out and commits them as message, and is the
// commit.
func (r *gitRemote) commit(message string, files map[string]string) string {
	r.t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "--all")
	r.minutes++
	r.git("commit", "--quiet", "-m", message)
	return r.git("rev-parse", "HEAD")
}

// land commits files on a branch of their own off main and merges it into main as a merge commit,
// main its first parent, and is that merge.
func (r *gitRemote) land(name string, files map[string]string) string {
	r.t.Helper()
	r.git("checkout", "--quiet", "-b", "side-"+name, "main")
	r.commit(name+" on its own branch", files)
	r.git("checkout", "--quiet", "main")
	r.minutes++
	r.git("merge", "--quiet", "--no-ff", "-m", name, "side-"+name)
	return r.git("rev-parse", "HEAD")
}

// nineLines is a file of the lines 1 to 9, each line the changes name replaced.
func nineLines(changes map[int]string) string {
	var b strings.Builder
	for line := 1; line <= 9; line++ {
		text, changed := changes[line]
		if !changed {
			text = strconv.Itoa(line)
		}
		b.WriteString(text + "\n")
	}
	return b.String()
}

// unreachableHead is a head GitHub's timeline names that the remote holds no commit of.
const unreachableHead = "1234567890abcdef1234567890abcdef12345678"

// gitCase is the repository, the window and GitHub of the collect-git tests. acme/widgets's main
// runs c0, c1, then the merges m1 to m6, each a merge commit of a branch of its own but m5, the
// merge of the pull request #12. Each but m2 changes f.txt, a file of nine lines: m1 its first line,
// which no head changes, m3 its fifth, as #12's heads do, m4 its last, m5 #12's own change, and m6
// its second; m2 changes other.txt alone. #12 was opened off c1 at p0 and p1, which change the
// fifth line, then p1b, which adds h.txt; and force-pushed to p2, off m1, with p1's change. #13's
// one head is unreachableHead.
//
// The window, of ACME over [09:00, 11:00) of 2001-03-05 with an hour before it and a run-out of
// half an hour, attributes #12 to ACME-1 and #13 to ACME-2, both issues of acme/widgets, and its
// merges file holds m1 to m6 of main, each at its merge, and c0 of the branch release. Its log holds
// two reads of #12's merge state: one at 09:30 of the head p1b, which no timeline event gives, the
// record standing at p1, on the base commit c1, which the merges file does not hold; and one at
// 10:05 of p2 on m3.
//
// GitHub's timeline of #12, over two pages, gives p0 committed at 07:00, before the reach, p1
// committed at 07:30, the head standing at the reach's start, a label, and the force push of p2 at
// 10:00 and the merge; GitHub records #12 merged into main as m5. #13's timeline gives its head
// committed at 09:15, and it is open.
type gitCase struct {
	remote                                      *gitRemote
	c0, c1, p1, p1b, p2, m1, m2, m3, m4, m5, m6 string
	window                                      Window
	server                                      *httptest.Server
	seen                                        *stubRequests
}

func newGitCase(t *testing.T) *gitCase {
	t.Helper()
	r := newGitRemote(t)
	c := &gitCase{remote: r}
	c.c0 = r.commit("base", map[string]string{"f.txt": nineLines(nil), "other.txt": "o\n", "g.txt": "g\n"})
	c.c1 = r.commit("before the window", map[string]string{"g.txt": "g2\n"})
	r.git("checkout", "--quiet", "-b", "pr")
	r.commit("p0", map[string]string{"f.txt": nineLines(map[int]string{5: "P0"})})
	c.p1 = r.commit("p1", map[string]string{"f.txt": nineLines(map[int]string{5: "PR"})})
	c.p1b = r.commit("p1b", map[string]string{"h.txt": "h\n"})
	p0 := r.git("rev-parse", "HEAD~2")
	r.git("checkout", "--quiet", "main")
	c.m1 = r.land("m1", map[string]string{"f.txt": nineLines(map[int]string{1: "A"})})
	r.git("checkout", "--quiet", "-b", "force", c.m1)
	c.p2 = r.commit("p2", map[string]string{"f.txt": nineLines(map[int]string{1: "A", 5: "PR"})})
	r.git("checkout", "--quiet", "main")
	c.m2 = r.land("m2", map[string]string{"other.txt": "o2\n"})
	c.m3 = r.land("m3", map[string]string{"f.txt": nineLines(map[int]string{1: "A", 5: "MAIN"})})
	c.m4 = r.land("m4", map[string]string{"f.txt": nineLines(map[int]string{1: "A", 5: "MAIN", 9: "NINE"})})
	c.m5 = r.commit("the change of #12 (#12)", map[string]string{"f.txt": nineLines(map[int]string{1: "A", 5: "PR", 9: "NINE"})})
	c.m6 = r.land("m6", map[string]string{"f.txt": nineLines(map[int]string{1: "A", 2: "TWO", 5: "PR", 9: "NINE"})})
	r.git("branch", "--quiet", "-D", "pr", "force", "side-m1", "side-m2", "side-m3", "side-m4", "side-m6")

	at := func(clock string) time.Time { return capturedTime(t, "2001-03-05T"+clock+"Z") }
	read := func(when, head, recorded, baseSha string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"time":"2001-03-05T%s:01Z","level":"INFO","msg":%q,"issue":"ACME-1","repo":"acme/widgets",`+
			`"pullRequest":12,"head":%q,"recordedHead":%q,"base":"main","baseSha":%q,"mergeable":"CONFLICTING","mergeState":"dirty",`+
			`"readAt":"2001-03-05T%s:00Z"}`, when, mergeStateReadLine, head, recorded, baseSha, when))
	}
	c.window = Window{Project: "ACME", From: at("09:00:00"), To: at("11:00:00"), Before: Duration(time.Hour), After: Duration(30 * time.Minute),
		Caps: []CapAt{}, Parts: []Part{}, Lines: []string{mergeStateReadLine},
		Log: []json.RawMessage{read("09:30", c.p1b, c.p1, c.c1), read("10:05", c.p2, c.p2, c.m3)},
		Events: map[string][]json.RawMessage{
			"ACME-1": {inputsEvent(1, 1, "ACME-1", "issue.created", "2001-03-05T08:05:00Z",
				`{"key":"ACME-1","status":"todo","title":"a change","repo":"acme/widgets","changed":["status"]}`),
				inputsAttribution(3, 2, "ACME-1", "acme/widgets", 12)},
			"ACME-2": {inputsEvent(2, 1, "ACME-2", "issue.created", "2001-03-05T08:06:00Z",
				`{"key":"ACME-2","status":"todo","title":"another change","repo":"acme/widgets","changed":["status"]}`),
				inputsAttribution(4, 2, "ACME-2", "acme/widgets", 13)},
		},
		Merges: map[string][]Merge{"acme/widgets": {
			{SHA: c.c0, At: at("08:05:00"), Branch: "release"},
			{SHA: c.m1, At: at("08:10:00"), Branch: "main"},
			{SHA: c.m2, At: at("08:20:00"), Branch: "main"},
			{SHA: c.m3, At: at("08:30:00"), Branch: "main"},
			{SHA: c.m4, At: at("08:40:00"), Branch: "main"},
			{SHA: c.m5, At: at("10:20:00"), Branch: "main", PR: 12, Event: true},
			{SHA: c.m6, At: at("10:40:00"), Branch: "main"},
		}},
	}
	committed := func(sha, clock string) string {
		return fmt.Sprintf(`{"sha":%q,"node_id":"C_1","author":{"name":"person-1","email":"person-1@example.invalid","date":"2001-03-05T%sZ"},`+
			`"committer":{"name":"person-1","email":"person-1@example.invalid","date":"2001-03-05T%sZ"},"message":"a commit","event":"committed"}`,
			sha, clock, clock)
	}
	timelines := map[int][][]byte{
		12: {
			[]byte("[" + committed(p0, "07:00:00") + "," + committed(c.p1, "07:30:00") +
				`,{"id":7,"event":"labeled","actor":{"login":"person-1"},"created_at":"2001-03-05T08:00:00Z","label":{"name":"ready"}}]`),
			[]byte(fmt.Sprintf(`[{"id":8,"event":"head_ref_force_pushed","actor":{"login":"person-1"},"commit_id":%q,"created_at":"2001-03-05T10:00:00Z"},`+
				`{"id":10,"event":"base_ref_changed","created_at":"2001-03-05T10:03:00Z"},`+
				`{"id":9,"event":"merged","actor":{"login":"person-1"},"commit_id":%q,"created_at":"2001-03-05T10:20:00Z"}]`, c.p2, c.m5)),
		},
		13: {[]byte("[" + committed(unreachableHead, "09:15:00") + "]")},
	}
	pulls := map[int]string{
		12: fmt.Sprintf(`{"number":12,"state":"closed","merged":true,"merged_at":"2001-03-05T10:20:00Z","merge_commit_sha":%q,"base":{"ref":"main"}}`, c.m5),
		13: fmt.Sprintf(`{"number":13,"state":"open","merged":false,"merged_at":null,"merge_commit_sha":%q,"base":{"ref":"main"}}`, strings.Repeat("e", 40)),
	}
	c.server, c.seen = pullsStub(t, pulls, timelines)
	return c
}

// pullsStub serves GitHub's record of each pull request of acme/widgets that pulls holds (GET
// /repos/acme/widgets/pulls/<n>) and its timeline from timelines (GET
// /repos/acme/widgets/issues/<n>/timeline?per_page=100), each page after the first through the
// Link header of the one before, and records every request. It refuses a request without the
// token, of another method or route, or of a pull request it holds none of.
func pullsStub(t *testing.T, pulls map[int]string, timelines map[int][][]byte) (*httptest.Server, *stubRequests) {
	t.Helper()
	requests := &stubRequests{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r)
		query := r.URL.Query()
		pull, isPull := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/pulls/")
		timeline, isTimeline := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/issues/")
		timeline, isTimeline = strings.CutSuffix(timeline, "/timeline")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Header.Get("Authorization") != "Bearer "+stubGitHubToken:
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		case r.Method == http.MethodGet && isPull && pulls[atoi(pull)] != "":
			fmt.Fprint(w, pulls[atoi(pull)])
		case r.Method == http.MethodGet && isTimeline && timelines[atoi(timeline)] != nil && query.Get("per_page") == "100":
			pages := timelines[atoi(timeline)]
			page := max(atoi(query.Get("page")), 1)
			if page < len(pages) {
				next := *r.URL
				query.Set("page", strconv.Itoa(page+1))
				next.RawQuery = query.Encode()
				w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, server.URL, next.RequestURI()))
			}
			w.Write(pages[page-1])
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// atoi is s as a whole number, 0 for none.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// windowDir writes the case's window into a directory of the test's own.
func (c *gitCase) windowDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "window")
	if err := c.window.Write(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// gitCalls records the arguments of each git command collect-git runs.
type gitCalls struct {
	mu    sync.Mutex
	calls [][]string
}

func (g *gitCalls) add(args []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, args)
}

// take is every call recorded since the last take, in order.
func (g *gitCalls) take() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	calls := g.calls
	g.calls = nil
	return calls
}

// count is how many of calls run the git subcommand named.
func countCalls(calls [][]string, subcommand string) int {
	n := 0
	for _, call := range calls {
		if len(call) > 0 && call[0] == subcommand {
			n++
		}
	}
	return n
}

// config is collect-git of acme/widgets's main into the window in dir, against the case's GitHub
// stub and remote, the clones in clones, git's calls told to calls.
func (c *gitCase) config(dir, clones string, calls *gitCalls) GitConfig {
	return GitConfig{Repo: "acme/widgets", Branch: "main", Token: stubGitHubToken, API: c.server.URL, Window: dir, Clones: clones, Wait: noWait,
		remote: "file://" + c.remote.dir, onGit: calls.add}
}

// gitDetailOfGit is the detail a missing line of the test's reading holds where git's own words
// stood, which change with git's release.
const gitDetailOfGit = "<git's words>"

// gitFileLines is the lines of the window's git file of acme/widgets, the detail of each missing
// input of a fetch that failed cut to where git's own words begin, which the test reads to start
// with prefix, and those words replaced with gitDetailOfGit.
func gitFileLines(t *testing.T, dir, prefix string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "git", "acme__widgets.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if i := strings.Index(line, `"detail":"`+prefix); i >= 0 {
			if !strings.HasSuffix(line, `"}`) || len(line)-2 <= i+len(`"detail":"`+prefix) {
				t.Errorf("the missing line %s holds no words of git's after %q", line, prefix)
			}
			line = line[:i] + `"detail":"` + prefix + gitDetailOfGit + `"}`
		}
		lines = append(lines, line)
	}
	return lines
}

// fetchFailed is the start of the detail of the missing input of a head git's fetch did not give.
const fetchFailed = "git fetch of the commit alone: git exits 128: "

// wantLines is the case's git file as one run of both phases writes it, or, with gitPhase false, as
// the GitHub phase alone writes it.
func (c *gitCase) wantLines(gitPhase bool) []string {
	var lines []string
	if gitPhase {
		for _, commit := range []struct{ sha, path string }{{c.m1, "f.txt"}, {c.m2, "other.txt"}, {c.m3, "f.txt"}, {c.m4, "f.txt"}, {c.m5, "f.txt"},
			{c.m6, "f.txt"}, {c.c1, "g.txt"}} {
			lines = append(lines, fmt.Sprintf(`{"kind":"commit","sha":%q,"paths":[%q]}`, commit.sha, commit.path))
		}
	}
	verdict := func(base string, conflict bool) string {
		if conflict {
			return fmt.Sprintf(`{"base":%q,"conflict":true,"paths":["f.txt"]}`, base)
		}
		return fmt.Sprintf(`{"base":%q,"conflict":false,"paths":[]}`, base)
	}
	head := func(pr int, sha, at, from, read string) string {
		line := fmt.Sprintf(`{"kind":"head","pr":%d,"sha":%q,"at":"2001-03-05T%sZ","from":%q`, pr, sha, at, from)
		if gitPhase && read != "" {
			line += "," + read
		}
		return line + "}"
	}
	fromC1 := fmt.Sprintf(`"tip":%q,"mergeBase":%q,"paths":%%s,"verdicts":[%s,%s,%s]`, c.m4, c.c1, verdict(c.m1, false), verdict(c.m3, true), verdict(c.m4, true))
	lines = append(lines,
		fmt.Sprintf(`{"kind":"pull","pr":12,"merge":%q,"base":"main","baseChanges":["2001-03-05T10:03:00Z"],"heads":3}`, c.m5),
		head(12, c.p1, "07:30:00", "commit", fmt.Sprintf(fromC1, `["f.txt"]`)),
		head(12, c.p1b, "09:30:00", "read", fmt.Sprintf(fromC1, `["f.txt","h.txt"]`)),
		head(12, c.p2, "10:00:00", "force-push", fmt.Sprintf(`"tip":%q,"mergeBase":%q,"paths":["f.txt"],"verdicts":[%s,%s]`, c.m4, c.m1,
			verdict(c.m3, true), verdict(c.m4, true))),
		`{"kind":"pull","pr":13,"merge":"","base":"main","baseChanges":[],"heads":1}`,
		head(13, unreachableHead, "09:15:00", "commit", ""))
	if gitPhase {
		lines = append(lines, fmt.Sprintf(`{"kind":"missing","pr":13,"head":%q,"base":"","reason":"head-unreachable","detail":"%s%s"}`,
			unreachableHead, fetchFailed, gitDetailOfGit))
	}
	return lines
}

// describeLines is lines, one to a line, for a failure's message.
func describeLines(lines []string) string {
	return "\n  " + strings.Join(lines, "\n  ")
}

// collect-git writes, for the pull requests of acme/widgets of the window's issues, the commit,
// pull, head and missing lines of the format exactly (gitCase): the paths of each commit of main
// the merges file holds, in its order, then of c1, the base commit a read names; #12 merged as m5,
// with its heads p1, standing at the reach's start, p1b from the read that names it, and p2 from
// its force push, but not p0, superseded before the reach. Each head's tip is m4, m5's first parent;
// p1 and p1b, off c1, are clean at m1 and conflict at m3, which changes the line they change, and
// again at m4, which alone would merge cleanly but holds m3 (the cumulative rule); p2, off m1,
// conflicts at m3 and m4. m2 touches none of their paths and has no verdict, and no verdict runs to
// m5 or m6, the pull request's own merge and the commit after it. #13, open, runs to m6, and its
// one head, which the remote does not hold, is a head-unreachable missing input. Every request the
// GitHub stub saw is a GET, each pull request read once and #12's timeline over its two pages; git
// merged each verdict once; and ReadWindow reads the window back with the file as typed data.
func TestCollectGitWritesEachHeadsVerdictsAndSendsGetAlone(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	dir := c.windowDir(t)
	calls := &gitCalls{}
	var finished []GitPullFinished
	config := c.config(dir, filepath.Join(t.TempDir(), "clones"), calls)
	config.Finished = func(done GitPullFinished) { finished = append(finished, done) }
	collected, err := CollectGit(context.Background(), config)
	if err != nil {
		t.Fatalf("CollectGit: %v", err)
	}
	if got, want := gitFileLines(t, dir, fetchFailed), c.wantLines(true); !slices.Equal(got, want) {
		t.Errorf("the git file holds%s\nwant%s", describeLines(got), describeLines(want))
	}
	if collected.PullRequests != 2 || collected.Heads != 4 || collected.Verdicts != 8 || collected.Conflicts != 6 || collected.Reads != 2 ||
		len(collected.Missing) != 1 || collected.Missing[GitHeadUnreachable] != 1 || collected.PeakMemory <= 0 {
		t.Errorf("collect-git counts %+v, want 2 pull requests, 4 heads, 8 verdicts, 6 conflicts, 2 reads, one head-unreachable and a peak of git's memory",
			collected)
	}
	wantFinished := []GitPullFinished{{Phase: GitPhaseGitHub, PR: 12, Heads: 3}, {Phase: GitPhaseGitHub, PR: 13, Heads: 1},
		{Phase: GitPhaseGit, PR: 12, Heads: 3, Verdicts: 8, Conflicts: 6}, {Phase: GitPhaseGit, PR: 13, Heads: 1, Missing: 1}}
	if !slices.Equal(finished, wantFinished) {
		t.Errorf("collect-git finished %+v, want %+v", finished, wantFinished)
	}
	wantRequests := []stubRequest{{http.MethodGet, "/repos/acme/widgets/pulls/12"}, {http.MethodGet, "/repos/acme/widgets/issues/12/timeline?per_page=100"},
		{http.MethodGet, "/repos/acme/widgets/issues/12/timeline?page=2&per_page=100"}, {http.MethodGet, "/repos/acme/widgets/pulls/13"},
		{http.MethodGet, "/repos/acme/widgets/issues/13/timeline?per_page=100"}}
	if got := c.seen.all(); !slices.Equal(got, wantRequests) {
		t.Errorf("the GitHub stub saw %v, want %v", got, wantRequests)
	}
	for _, request := range c.seen.all() {
		if request.Method != http.MethodGet {
			t.Errorf("collect-git sent %s %s; it sends GET alone", request.Method, request.Target)
		}
	}
	gitRan := calls.take()
	if merges, trees := countCalls(gitRan, "merge-tree"), countCalls(gitRan, "diff-tree"); merges != 8 || trees != 7 || countCalls(gitRan, "clone") != 1 {
		t.Errorf("git ran %d merge-trees, %d diff-trees and %d clones, want 8, 7 and 1: %q", merges, trees, countCalls(gitRan, "clone"), gitRan)
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow of the window collect-git wrote: %v", err)
	}
	input := read.Git["acme/widgets"]
	pull, held := input.Pull(12)
	if len(read.Git) != 1 || !held || len(pull.Heads) != 3 || pull.Heads[1].From != GitHeadRead || len(input.Commits) != 7 || len(input.Missing) != 1 {
		t.Errorf("ReadWindow reads the git input as %+v, want acme/widgets's 7 commits, #12 with its 3 heads, p1b from a read, and 1 missing input", read.Git)
	}
	if commit, held := input.Commit(c.c1); !held || !slices.Equal(commit.Paths, []string{"g.txt"}) {
		t.Errorf("the git input holds c1 as %+v (%v), want its path g.txt", commit, held)
	}
}

// A pull request GitHub still refuses to read after the reader's tries, with its 422 of too many
// files changed (githubTooManyFiles), is a pull-unread missing input, as one GitHub has no record of
// is a pull-not-found one, and the collection goes on to the next pull request. Every read of #12
// answers so, retryAttempts reads, and #12's timeline is not read; the git file holds no pull or
// head line of #12 but its missing line, which the git phase keeps, beside #13 as one run writes it
// (gitCase), and the run counts it. It fails where the refusal fails the collection, where #12's
// timeline is read, or where the missing line is dropped.
func TestCollectGitWritesAPullRequestGitHubWillNotReadAsPullUnread(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	stub, seen := c.server.Config.Handler, &stubRequests{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.add(r)
		if r.URL.Path != "/repos/acme/widgets/pulls/12" {
			stub.ServeHTTP(w, r)
			return
		}
		http.Error(w, tooManyFilesChanged.body, tooManyFilesChanged.status)
	}))
	t.Cleanup(c.server.Close)
	dir := c.windowDir(t)
	collected, err := CollectGit(context.Background(), c.config(dir, filepath.Join(t.TempDir(), "clones"), &gitCalls{}))
	if err != nil {
		t.Fatalf("CollectGit: %v", err)
	}
	detail, err := json.Marshal("the pull request of acme/widgets#12: GitHub answered 422 Unprocessable Entity: " + tooManyFilesChanged.body)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.DeleteFunc(c.wantLines(true), func(line string) bool { return strings.Contains(line, `"pr":12,`) })
	// #12's missing line sorts before #13's, the file's last.
	want = slices.Insert(want, len(want)-1, fmt.Sprintf(`{"kind":"missing","pr":12,"head":"","base":"","reason":"pull-unread","detail":%s}`, detail))
	if got := gitFileLines(t, dir, fetchFailed); !slices.Equal(got, want) {
		t.Errorf("the git file holds%s\nwant%s", describeLines(got), describeLines(want))
	}
	if collected.Missing[GitPullUnread] != 1 || collected.Missing[GitHeadUnreachable] != 1 || len(collected.Missing) != 2 {
		t.Errorf("collect-git counts the missing inputs %v, want one pull-unread and one head-unreachable", collected.Missing)
	}
	wantRequests := append(slices.Repeat([]stubRequest{{http.MethodGet, "/repos/acme/widgets/pulls/12"}}, retryAttempts),
		stubRequest{http.MethodGet, "/repos/acme/widgets/pulls/13"}, stubRequest{http.MethodGet, "/repos/acme/widgets/issues/13/timeline?per_page=100"})
	if got := seen.all(); !slices.Equal(got, wantRequests) {
		t.Errorf("the GitHub stub saw %v, want %v", got, wantRequests)
	}
}

// A run on a window whose git file collect-git wrote asks GitHub and git for nothing the file
// holds: run again with the same clones, it sends GitHub no request, since the file holds both
// pull requests with no missing input of GitHub's, and runs no git but what reaches for #13's head,
// whose missing input names it: the check of the clone, the fetch of its batch, the check, the fetch
// of it alone and the check. The file it leaves is the one the first run wrote, byte for byte.
func TestCollectGitAgainAsksForNothingTheFileHolds(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	calls := &gitCalls{}
	if _, err := CollectGit(context.Background(), c.config(dir, clones, calls)); err != nil {
		t.Fatalf("CollectGit: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "git", "acme__widgets.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	calls.take()
	requests := len(c.seen.all())
	if _, err := CollectGit(context.Background(), c.config(dir, clones, calls)); err != nil {
		t.Fatalf("CollectGit again: %v", err)
	}
	fetch := []string{"fetch", "--quiet", "--no-tags", "--filter=blob:none", "origin", unreachableHead}
	check := []string{"cat-file", "--batch-check"}
	if got, want := calls.take(), [][]string{check, fetch, check, fetch, check}; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("collect-git run again ran git %q, want only the reach for #13's head %q", got, want)
	}
	if got := c.seen.all()[requests:]; len(got) != 0 {
		t.Errorf("collect-git run again sent GitHub %v, want nothing", got)
	}
	again, err := os.ReadFile(filepath.Join(dir, "git", "acme__widgets.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Errorf("collect-git run again wrote\n%s\nwant the first run's\n%s", again, first)
	}
}

// The GitHub phase alone writes each pull request's pull line and its heads with their commit,
// time and source alone, and no commit line, sends GET alone and runs no git; the git phase alone
// refuses a GitHub token, then, from that file, sends GitHub nothing and writes the file and
// window.json one run of both phases writes, byte for byte. It fails where a phase reads what the
// other does, or where the two in turn write another file than one run.
func TestCollectGitsTwoPhasesInTurnWriteWhatOneRunWrites(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	phased, whole := c.windowDir(t), c.windowDir(t)
	calls := &gitCalls{}
	config := c.config(phased, filepath.Join(t.TempDir(), "clones"), calls)
	config.GitHubOnly = true
	if _, err := CollectGit(context.Background(), config); err != nil {
		t.Fatalf("CollectGit --github-only: %v", err)
	}
	if got, want := gitFileLines(t, phased, fetchFailed), c.wantLines(false); !slices.Equal(got, want) {
		t.Errorf("the GitHub phase alone wrote%s\nwant%s", describeLines(got), describeLines(want))
	}
	if ran := calls.take(); len(ran) != 0 {
		t.Errorf("the GitHub phase alone ran git %q, want none", ran)
	}
	requests := c.seen.all()
	for _, request := range requests {
		if request.Method != http.MethodGet {
			t.Errorf("the GitHub phase sent %s %s; it sends GET alone", request.Method, request.Target)
		}
	}

	config.GitHubOnly, config.GitOnly = false, true
	if _, err := CollectGit(context.Background(), config); err == nil || !strings.Contains(err.Error(), "takes no GitHub token") {
		t.Errorf("CollectGit --git-only with a GitHub token = %v, want a refusal", err)
	}
	config.Token, config.API = "", ""
	if _, err := CollectGit(context.Background(), config); err != nil {
		t.Fatalf("CollectGit --git-only: %v", err)
	}
	if got := c.seen.all()[len(requests):]; len(got) != 0 {
		t.Errorf("the git phase alone sent GitHub %v, want nothing", got)
	}
	if _, err := CollectGit(context.Background(), c.config(whole, filepath.Join(t.TempDir(), "clones"), &gitCalls{})); err != nil {
		t.Fatalf("CollectGit: %v", err)
	}
	for _, name := range []string{"git/acme__widgets.jsonl", windowFile} {
		got, err := os.ReadFile(filepath.Join(phased, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(whole, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("the two phases in turn wrote %s as\n%s\nwant, as one run wrote it,\n%s", name, got, want)
		}
	}
}

// git fetch and the git upload-pack it runs write one stderr, so for a commit the remote does not
// hold the two `fatal:` lines come in the order the two processes ran in, which differs from one
// collection to the next, and two collections of one input must write the same bytes. What a
// missing input's detail says of git's words, stderrSummary, is each line that holds more than
// white space, trimmed, once, in sorted order, joined with "; " and cut to 300 bytes: the same text
// for the two lines in either order. It fails under the last line of stderr, which is whichever
// line the processes wrote last.
func TestTheStderrSummaryDoesNotDependOnTheOrderOfItsLines(t *testing.T) {
	t.Parallel()
	fetch := "fatal: remote error: upload-pack: not our ref " + unreachableHead
	uploadPack := "fatal: git upload-pack: not our ref " + unreachableHead
	many := make([]string, 40)
	for i := range many {
		many[i] = fmt.Sprintf("line-%02d", i)
	}
	for name, test := range map[string]struct{ stderr, want string }{
		"fetch's line first":       {fetch + "\n" + uploadPack + "\n", uploadPack + "; " + fetch},
		"upload-pack's line first": {uploadPack + "\n" + fetch + "\n", uploadPack + "; " + fetch},
		"nothing":                  {"", ""},
		"white space alone":        {"  \n\t\r\n", ""},
		"trimmed and once":         {"  b \r\n\na\nb\n  ", "a; b"},
		"one long line":            {strings.Repeat("x", 400) + "\n", strings.Repeat("x", 300)},
		"many lines":               {strings.Join(many, "\n"), strings.Join(many, "; ")[:300]},
	} {
		if got := stderrSummary([]byte(test.stderr)); got != test.want {
			t.Errorf("the summary of stderr with %s is %q, want %q", name, got, test.want)
		}
	}
}

// collect-git refuses, before any read of GitHub or git and with nothing written, a window that
// holds no merges file of the repository, and a clones' directory inside the window.
func TestCollectGitRefusesAWindowWithNoMergesAndClonesInside(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	c.window.Merges = map[string][]Merge{"acme/other": {{SHA: c.m1, At: c.window.From, Branch: "main"}}}
	dir := c.windowDir(t)
	calls := &gitCalls{}
	if _, err := CollectGit(context.Background(), c.config(dir, filepath.Join(t.TempDir(), "clones"), calls)); err == nil ||
		!strings.Contains(err.Error(), "holds no merges file of acme/widgets") {
		t.Errorf("CollectGit of a window with no merges file of acme/widgets = %v, want a refusal naming it", err)
	}
	if _, err := CollectGit(context.Background(), c.config(dir, filepath.Join(dir, "clones"), calls)); err == nil ||
		!strings.Contains(err.Error(), "inside the window") {
		t.Errorf("CollectGit with the clones inside the window = %v, want a refusal", err)
	}
	if ran, sent := calls.take(), c.seen.all(); len(ran) != 0 || len(sent) != 0 {
		t.Errorf("the refused runs ran git %q and sent GitHub %v, want nothing", ran, sent)
	}
	if _, err := os.Stat(filepath.Join(dir, "git")); !os.IsNotExist(err) {
		t.Errorf("a refused run left the window's git directory (%v)", err)
	}
}
