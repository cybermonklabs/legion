package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gitRemote is a repository a test builds with the git CLI in a directory of its own, which
// collect-git clones and fetches from as GitHub's remote: it lets a fetch name any commit it holds
// and honours a fetch's filter, as GitHub does, so a clone that asked to leave blobs out would lack
// them. Each commit is made a minute after the one before.
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
	return r.gitInput("", args...)
}

// gitInput runs git args in the repository as git does, with stdin on its standard input, and is
// what it printed.
func (r *gitRemote) gitInput(stdin string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Stdin = strings.NewReader(stdin)
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

// mergedAlone is git's verdict at base of head merged alone, as git merge-tree --write-tree
// --no-messages --name-only -z <base> <head> in the repository gives it: clean on exit 0, and a
// conflict in the paths it names after the merged tree, sorted, on exit 1.
func (r *gitRemote) mergedAlone(base, head string) GitVerdict {
	r.t.Helper()
	cmd := exec.Command("git", "merge-tree", "--write-tree", "--no-messages", "--name-only", "-z", base, head)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.Output()
	var exit *exec.ExitError
	conflict := errors.As(err, &exit) && exit.ExitCode() == 1
	if err != nil && !conflict {
		r.t.Fatalf("git merge-tree of %s and %s: %v", base, head, err)
	}
	paths := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")[1:]
	slices.Sort(paths)
	return GitVerdict{Base: base, Conflict: conflict, Paths: paths}
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
// merged each head with its bases in one merge-tree --stdin (batchMergeTree); git cloned and fetched
// with no filter, so the clone lacks no object its refs reach; and ReadWindow reads the window back
// with the file as typed data.
func TestCollectGitWritesEachHeadsVerdictsAndSendsGetAlone(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	dir := c.windowDir(t)
	calls := &gitCalls{}
	var finished []GitPullFinished
	clones := filepath.Join(t.TempDir(), "clones")
	config := c.config(dir, clones, calls)
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
	if trees := countCalls(gitRan, "diff-tree"); trees != 7 || countCalls(gitRan, "clone") != 1 || countCalls(gitRan, "fetch") == 0 {
		t.Errorf("git ran %d diff-trees, %d clones and %d fetches, want 7, 1 and some: %q", trees, countCalls(gitRan, "clone"),
			countCalls(gitRan, "fetch"), gitRan)
	}
	if got, want := callsOf(gitRan, "merge-tree"), slices.Repeat([][]string{batchMergeTree}, 3); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("git ran the merge-trees %q, want %q, one for each of p1, p1b and p2", got, want)
	}
	for _, call := range slices.Concat(callsOf(gitRan, "clone"), callsOf(gitRan, "fetch")) {
		if slices.ContainsFunc(call, func(arg string) bool { return strings.HasPrefix(arg, "--filter") }) {
			t.Errorf("git ran %q, which leaves objects out of the clone", call)
		}
	}
	if lacked := lackedObjects(t, filepath.Join(clones, "acme__widgets"), "--all"); len(lacked) != 0 {
		t.Errorf("the clone lacks the objects %q, which its refs reach", lacked)
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
// whose missing input names it: the check of the clone, the ask of the remote for #13's head ref,
// which it holds none of, the fetch of the head alone into a ref of its own, with no filter, and the
// check. The file it leaves is the one the first run wrote, byte for byte.
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
	list := []string{"ls-remote", "origin", "refs/pull/13/head"}
	fetch := []string{"fetch", "--quiet", "--no-tags", "origin", unreachableHead + ":refs/fetched/" + unreachableHead}
	check := []string{"cat-file", "--batch-check"}
	if got, want := calls.take(), [][]string{check, list, fetch, check}; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
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

// callsOf is each of calls that runs the git subcommand named, in order.
func callsOf(calls [][]string, subcommand string) [][]string {
	return slices.DeleteFunc(slices.Clone(calls), func(call []string) bool { return len(call) == 0 || call[0] != subcommand })
}

// batchMergeTree is the merge-tree that merges a head with each base commit a line of its stdin
// names.
var batchMergeTree = []string{"merge-tree", "--stdin", "--write-tree", "--no-messages", "--name-only", "-z"}

// clonePicture is what a test reads of a clone: each file under its objects directory, as its path
// there and its size, and each entry of the clone's own directory, each sorted.
type clonePicture struct{ objects, entries []string }

// pictureClone is the clone in dir as it stands.
func pictureClone(t *testing.T, dir string) clonePicture {
	t.Helper()
	var picture clonePicture
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Errorf("read the clone in %s: %v", dir, err)
		return picture
	}
	for _, entry := range entries {
		picture.entries = append(picture.entries, entry.Name())
	}
	objects := filepath.Join(dir, "objects")
	err = filepath.WalkDir(objects, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(objects, path)
		picture.objects = append(picture.objects, fmt.Sprintf("%s %d", filepath.ToSlash(name), info.Size()))
		return err
	})
	if err != nil {
		t.Errorf("read the objects of the clone in %s: %v", dir, err)
	}
	return picture
}

// pictureBeforeMerges is an onGit that tells calls of each git command, and pictures the clone in
// dir into before as collect-git's first merge-tree is about to run.
func pictureBeforeMerges(t *testing.T, dir string, calls *gitCalls, before *clonePicture) func([]string) {
	return func(args []string) {
		calls.add(args)
		if args[0] == "merge-tree" && before.entries == nil {
			*before = pictureClone(t, dir)
		}
	}
}

// checkMergesWroteNothing fails t where the clone in dir, once collect-git has run, is not as
// before pictured it before its merge-trees: where its objects directory gained or lost a file, or
// the clone holds another entry than it held then, such as a directory of the merges' own objects
// (gitMergeObjects), which collect-git removes before its first merge-tree and after its last.
func checkMergesWroteNothing(t *testing.T, dir string, before clonePicture) {
	t.Helper()
	if before.entries == nil {
		t.Fatal("collect-git ran no merge-tree, so the test pictured the clone before none")
	}
	after := pictureClone(t, dir)
	notIn := func(a, b []string) []string {
		return slices.DeleteFunc(slices.Clone(a), func(s string) bool { return slices.Contains(b, s) })
	}
	if gained, lost := notIn(after.objects, before.objects), notIn(before.objects, after.objects); len(gained) > 0 || len(lost) > 0 {
		t.Errorf("the merges changed the clone's objects directory: it gained %d files%s\nand lost %d%s", len(gained), describeLines(gained), len(lost),
			describeLines(lost))
	}
	want := slices.DeleteFunc(slices.Clone(before.entries), func(name string) bool { return name == gitMergeObjects })
	if !slices.Equal(after.entries, want) {
		t.Errorf("the clone holds after the run the entries %q, want %q: those it held before the first merge-tree, but no directory %s of the merges' objects",
			after.entries, want, gitMergeObjects)
	}
}

// lackedObjects is each object the revisions revs of the repository in dir reach that it does not
// hold, asked with lazy fetching off (git rev-list --objects --missing=print <revs>); a revision it
// does not hold fails t.
func lackedObjects(t *testing.T, dir string, revs ...string) []string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "rev-list", "--objects", "--missing=print"}, revs...)...)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-list --objects --missing=print %s in %s: %v: %s", strings.Join(revs, " "), dir, err, stderr.Bytes())
	}
	var lacked []string
	for line := range strings.Lines(string(out)) {
		if sha, missing := strings.CutPrefix(strings.TrimSpace(line), "?"); missing {
			lacked = append(lacked, sha)
		}
	}
	return lacked
}

// cloneRefs is each ref the repository in dir holds (git show-ref), its commit by its name.
func cloneRefs(t *testing.T, dir string) map[string]string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "show-ref").Output()
	if err != nil {
		t.Fatalf("git show-ref in %s: %v", dir, err)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok {
			refs[name] = sha
		}
	}
	return refs
}

// gitOnly is the case's config of the git phase alone into the window in dir, the clones in
// clones, git's calls told to calls.
func (c *gitCase) gitOnly(dir, clones string, calls *gitCalls) GitConfig {
	config := c.config(dir, clones, calls)
	config.Token, config.API, config.GitOnly = "", "", true
	return config
}

// fetchOnly and mergeOnly are the case's configs of the git phase's fetching alone and of its
// merging alone into the window in dir, the clones in clones, git's calls told to calls.
func (c *gitCase) fetchOnly(dir, clones string, calls *gitCalls) GitConfig {
	config := c.gitOnly(dir, clones, calls)
	config.GitOnly, config.FetchOnly = false, true
	return config
}

func (c *gitCase) mergeOnly(dir, clones string, calls *gitCalls) GitConfig {
	config := c.gitOnly(dir, clones, calls)
	config.GitOnly, config.MergeOnly = false, true
	return config
}

// notInClone is the detail of the missing input of a commit the clone does not hold, where the git
// phase's merging alone, which asks the remote for nothing, reads it.
const notInClone = "the clone does not hold the commit, and this run of the merges alone asks the remote for nothing"

// fetchingAlone is each git subcommand the git phase's fetching alone may run: the clone, the check
// of what the clone holds, the ask of the remote for the pull requests' head refs, and the fetch.
var fetchingAlone = []string{"clone", "cat-file", "ls-remote", "fetch"}

// remoteWords is each argument of a git command that names the remote or reaches it.
var remoteWords = []string{"origin", "clone", "fetch", "ls-remote"}

// checkNoRemote fails t where a git command of calls, which the git phase's merging alone ran,
// names the remote or reaches it (remoteWords).
func checkNoRemote(t *testing.T, calls [][]string) {
	t.Helper()
	for _, call := range calls {
		if slices.ContainsFunc(call, func(arg string) bool { return slices.Contains(remoteWords, arg) }) {
			t.Errorf("the merging alone ran git %q, which names the remote or reaches it", call)
		}
	}
}

// windowBytes is the bytes of the window's git file of acme/widgets and of its window.json, in dir,
// by name.
func windowBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	for _, name := range []string{"git/acme__widgets.jsonl", windowFile} {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	return files
}

// checkSameBytes fails t where got, what windowBytes read of the window what names, is not want,
// file by file.
func checkSameBytes(t *testing.T, what string, got, want map[string][]byte) {
	t.Helper()
	for name, data := range want {
		if !bytes.Equal(got[name], data) {
			t.Errorf("%s holds %s as\n%s\nwant\n%s", what, name, got[name], data)
		}
	}
}

// collect-git fetches each pull request's head ref before any commit of it: it asks the remote
// which pull requests whose heads the clone lacks it holds refs/pull/<n>/head of (git ls-remote),
// fetches each such ref into refs/pulls/<n> of the clone, and then fetches no commit that a ref
// brought. Here the remote holds #12's head ref at p1b, so p1 and p1b come with it, and only p2, of
// the force push, and #13's head, which the remote holds no ref and no commit of, are fetched as
// commits; #13's head ref, which the remote does not hold, is never fetched. The file is the one a
// run against a remote with no pull request's ref writes (gitCase). It fails where a commit the ref
// brings is fetched by itself, where a ref the remote does not hold is fetched, or where the clone
// keeps no ref of #12's head.
func TestCollectGitFetchesAPullRequestsHeadRefBeforeItsCommits(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	c.remote.git("update-ref", "refs/pull/12/head", c.p1b)
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	calls := &gitCalls{}
	if _, err := CollectGit(context.Background(), c.config(dir, clones, calls)); err != nil {
		t.Fatalf("CollectGit: %v", err)
	}
	fetches := callsOf(calls.take(), "fetch")
	pulled := false
	for _, call := range fetches {
		for _, arg := range call {
			switch {
			case arg == "+refs/pull/12/head:refs/pulls/12":
				pulled = true
			case strings.Contains(arg, c.p1) || strings.Contains(arg, c.p1b):
				t.Errorf("collect-git fetched %q, which names a commit #12's head ref brings", call)
			case strings.Contains(arg, "refs/pull/13/"):
				t.Errorf("collect-git fetched %q, which names #13's head ref, which the remote does not hold", call)
			}
		}
	}
	if !pulled {
		t.Errorf("collect-git fetched %q, want #12's head ref fetched as +refs/pull/12/head:refs/pulls/12", fetches)
	}
	if got := cloneRefs(t, filepath.Join(clones, "acme__widgets"))["refs/pulls/12"]; got != c.p1b {
		t.Errorf("the clone holds refs/pulls/12 at %q, want p1b %s", got, c.p1b)
	}
	if got, want := gitFileLines(t, dir, fetchFailed), c.wantLines(true); !slices.Equal(got, want) {
		t.Errorf("the git file holds%s\nwant%s", describeLines(got), describeLines(want))
	}
}

// collect-git fetches each commit it lacks into a ref of its own, refs/fetched/<sha>, so each later
// fetch tells the remote it holds the commit, and splits a batch whose fetch did not bring all its
// commits into two halves, each fetched again, down to a commit alone. Here the git file alone
// gives #13's heads: fifteen commits the remote holds, each off c1 on no branch, and
// unreachableHead; the remote holds no pull request's ref. The fetch of the batch of sixteen fails
// whole, as a fetch that names a commit the remote does not hold does; then at each of four
// halvings the half that holds unreachableHead fails and the other half is fetched: nine fetches,
// not the batch and then each of the sixteen alone. The clone keeps a ref of each of the fifteen,
// each is read, and unreachableHead is the head-unreachable missing input of its fetch alone.
func TestCollectGitKeepsARefOfEachCommitAndSplitsAFailingFetch(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	heads := []GitHead{{SHA: unreachableHead, At: c.window.From, From: GitHeadCommit}}
	var reachable []string
	for i := range 15 {
		c.remote.git("checkout", "--quiet", "--detach", c.c1)
		sha := c.remote.commit(fmt.Sprintf("head %d", i), map[string]string{"k.txt": fmt.Sprintf("k%d\n", i)})
		reachable = append(reachable, sha)
		heads = append(heads, GitHead{SHA: sha, At: c.window.From, From: GitHeadCommit})
	}
	c.remote.git("checkout", "--quiet", "main")
	slices.SortFunc(heads, compareGitHeads)
	c.window.Git = map[string]GitInput{"acme/widgets": {Pulls: []GitPull{{Number: 13, Base: "main", Heads: heads}}}}
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	calls := &gitCalls{}
	if _, err := CollectGit(context.Background(), c.gitOnly(dir, clones, calls)); err != nil {
		t.Fatalf("CollectGit --git-only: %v", err)
	}
	fetches := callsOf(calls.take(), "fetch")
	for _, call := range fetches {
		for _, arg := range call[slices.Index(call, "origin")+1:] {
			if sha, ref, _ := strings.Cut(arg, ":"); !commitSHA.MatchString(sha) || ref != "refs/fetched/"+sha {
				t.Errorf("collect-git fetched %q, whose %q is no <sha>:refs/fetched/<sha>", call, arg)
				break
			}
		}
	}
	if len(fetches) > 9 {
		t.Errorf("collect-git ran %d fetches of #13's sixteen heads, want at most 9, the batch and two at each halving: %q", len(fetches), fetches)
	}
	refs := cloneRefs(t, filepath.Join(clones, "acme__widgets"))
	for _, sha := range reachable {
		if got := refs["refs/fetched/"+sha]; got != sha {
			t.Errorf("the clone holds refs/fetched/%s at %q, want the commit", sha, got)
		}
	}
	if got, held := refs["refs/fetched/"+unreachableHead]; held {
		t.Errorf("the clone holds refs/fetched/%s at %s, a commit the remote does not hold", unreachableHead, got)
	}
	var missing []string
	for _, line := range gitFileLines(t, dir, fetchFailed) {
		if strings.HasPrefix(line, `{"kind":"missing"`) {
			missing = append(missing, line)
		}
	}
	want := []string{fmt.Sprintf(`{"kind":"missing","pr":13,"head":%q,"base":"","reason":"head-unreachable","detail":"%s%s"}`, unreachableHead,
		fetchFailed, gitDetailOfGit)}
	if !slices.Equal(missing, want) {
		t.Errorf("the git file holds the missing lines%s\nwant%s", describeLines(missing), describeLines(want))
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow of the window collect-git wrote: %v", err)
	}
	pull, held := read.Git["acme/widgets"].Pull(13)
	if !held || len(pull.Heads) != len(heads) {
		t.Fatalf("the git file holds #13 as %+v (%v), want its %d heads", pull, held, len(heads))
	}
	for _, h := range pull.Heads {
		if h.Read() == (h.SHA == unreachableHead) {
			t.Errorf("the git file holds #13's head %s read %v, want each head read but unreachableHead", h.SHA, h.Read())
		}
	}
}

// collect-git asks git once for what a head two pull requests share rests on. Here the git file
// alone gives #12 and #13 each the one head p1, neither merged in the window's inputs, so each reads
// p1 against m6, the merges file's last commit: git gives p1's merge base with m6, its paths since
// and, in one merge-tree --stdin, its verdict at each of the five base commits that change f.txt,
// once, and the second pull request takes them, so both write the same reading. It fails where git
// is asked again for what it gave the run.
func TestCollectGitReadsAHeadTwoPullRequestsShareOnce(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	head := []GitHead{{SHA: c.p1, At: c.window.From, From: GitHeadCommit}}
	c.window.Git = map[string]GitInput{"acme/widgets": {Pulls: []GitPull{{Number: 12, Base: "main", Heads: head}, {Number: 13, Base: "main", Heads: head}}}}
	dir := c.windowDir(t)
	calls := &gitCalls{}
	if _, err := CollectGit(context.Background(), c.gitOnly(dir, filepath.Join(t.TempDir(), "clones"), calls)); err != nil {
		t.Fatalf("CollectGit --git-only: %v", err)
	}
	ran := calls.take()
	if got, want := callsOf(ran, "merge-tree"), [][]string{batchMergeTree}; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("git ran the merge-trees %q, want the one %q of p1's bases", got, want)
	}
	if bases, diffs := countCalls(ran, "merge-base"), countCalls(ran, "diff"); bases != 1 || diffs != 1 {
		t.Errorf("git ran %d merge-bases and %d diffs, want one of each for p1: %q", bases, diffs, ran)
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow of the window collect-git wrote: %v", err)
	}
	input := read.Git["acme/widgets"]
	twelve, _ := input.Pull(12)
	thirteen, _ := input.Pull(13)
	if len(twelve.Heads) != 1 || len(thirteen.Heads) != 1 || len(input.Missing) != 0 {
		t.Fatalf("the git file holds #12 as %+v, #13 as %+v and the missing inputs %+v, want one head of each and none missing", twelve, thirteen,
			input.Missing)
	}
	a, errA := json.Marshal(twelve.Heads[0])
	b, errB := json.Marshal(thirteen.Heads[0])
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	if h := twelve.Heads[0]; !h.Read() || len(h.Verdicts) != 5 || !bytes.Equal(a, b) {
		t.Errorf("the git file holds p1 of #12 as %s and of #13 as %s, want the same reading, a verdict at each of m1, m3, m4, m5 and m6", a, b)
	}
}

// collect-git merges a head with each base commit it reads a verdict at in one git merge-tree
// --stdin (batchMergeTree), a line of its stdin for each pair, rather than in a merge-tree for each
// pair, and the merge writes nothing into the clone. Here the git file alone gives #13, which did
// not merge, the one head q, off c1, which changes f.txt's fifth line as p1 does and other.txt, so q
// reads against m6, and each of m1 to m6 changes one of its paths. git runs one merge-tree, and q's
// verdicts are those git gives each pair merged alone: clean at m1, a conflict in other.txt at m2,
// in f.txt and other.txt at m3 and m4, and in other.txt alone at m5 and m6, which change f.txt's
// fifth line as q does. The clone's objects directory holds after the run the files it held as the
// merge-tree was about to run, and the clone no directory of the merges' objects
// (checkMergesWroteNothing). It fails where git runs a merge-tree for each pair, where a verdict is
// not the one of its pair merged alone, or where the merge-tree wrote its trees and blobs into the
// clone or left its object directory there.
func TestCollectGitMergesAHeadWithItsBasesInOneMergeTree(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	c.remote.git("checkout", "--quiet", "--detach", c.c1)
	q := c.remote.commit("q", map[string]string{"f.txt": nineLines(map[int]string{5: "PR"}), "other.txt": "o3\n"})
	c.remote.git("checkout", "--quiet", "main")
	heads := []GitHead{{SHA: q, At: c.window.From, From: GitHeadCommit}}
	c.window.Git = map[string]GitInput{"acme/widgets": {Pulls: []GitPull{{Number: 13, Base: "main", Heads: heads}}}}
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	calls := &gitCalls{}
	var before clonePicture
	config := c.gitOnly(dir, clones, calls)
	config.onGit = pictureBeforeMerges(t, filepath.Join(clones, "acme__widgets"), calls, &before)
	if _, err := CollectGit(context.Background(), config); err != nil {
		t.Fatalf("CollectGit --git-only: %v", err)
	}
	if got, want := callsOf(calls.take(), "merge-tree"), [][]string{batchMergeTree}; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("git ran the merge-trees %q, want the one %q of q's six bases", got, want)
	}
	conflict := func(base string, paths ...string) GitVerdict {
		return GitVerdict{Base: base, Conflict: true, Paths: paths}
	}
	want := []GitVerdict{{Base: c.m1, Paths: []string{}}, conflict(c.m2, "other.txt"), conflict(c.m3, "f.txt", "other.txt"),
		conflict(c.m4, "f.txt", "other.txt"), conflict(c.m5, "other.txt"), conflict(c.m6, "other.txt")}
	alone := make([]GitVerdict, len(want))
	for i, verdict := range want {
		alone[i] = c.remote.mergedAlone(verdict.Base, q)
	}
	if !sameReading(alone, want) {
		t.Fatalf("git merges q with each base alone as %s, want %s", describeReading(alone), describeReading(want))
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow of the window collect-git wrote: %v", err)
	}
	input := read.Git["acme/widgets"]
	pull, held := input.Pull(13)
	if !held || len(pull.Heads) != 1 || len(input.Missing) != 0 {
		t.Fatalf("the git file holds #13 as %+v (%v) and the missing inputs %+v, want q and none missing", pull, held, input.Missing)
	}
	if h := pull.Heads[0]; h.Tip != c.m6 || h.MergeBase != c.c1 || !slices.Equal(h.Paths, []string{"f.txt", "other.txt"}) || !sameReading(h.Verdicts, alone) {
		t.Errorf("the git file holds q as %s, want it read against m6 from c1, its paths f.txt and other.txt, and the verdicts %s", describeReading(h),
			describeReading(alone))
	}
	checkMergesWroteNothing(t, filepath.Join(clones, "acme__widgets"), before)
}

// mergeFailed is the start of the detail of the missing input of a verdict git's merge-tree of the
// base and the head alone did not give, where git died.
const mergeFailed = "git merge-tree of the base and the head: git exits 128: "

// collect-git keeps each verdict its merge-tree --stdin gave whole before it broke off, and merges
// alone each pair it gave none of, as one merge-tree a pair did, up to the first git does not give.
// Here the clone already holds main and x, a commit on m6 whose f.txt is a blob the clone does not
// hold, and y, a commit on x whose tree is m6's; the merges file runs on from m6 to x and y, and the
// git file alone gives #13 the one head p1, which reads against y. Of p1's bases m1, m3, m4, m5, m6,
// x and y, the merge-tree --stdin gives m1 to m6 and dies at x, which git cannot read; collect-git
// keeps those five, merges x alone, which fails too, and writes the five, a git-error missing input
// at x whose detail is that of x's merge-tree alone, and no verdict at y, which rests on x. The
// clone also holds the directory of the merges' objects (gitMergeObjects) a stopped run left, an
// object in it; after the run the clone holds neither that directory nor an object the two
// merge-trees wrote (checkMergesWroteNothing). It fails where a verdict the batch gave is merged
// again, where y is merged, where the missing input is not x's, or where a merge-tree wrote into
// the clone or a directory of the merges' objects is left there.
func TestCollectGitMergesAloneWhatItsMergeTreeStdinDidNotGive(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	clones := filepath.Join(t.TempDir(), "clones")
	clone := filepath.Join(clones, "acme__widgets")
	c.remote.git("clone", "--quiet", "--no-checkout", "--bare", "--single-branch", "--no-tags", "--branch", "main", "file://"+c.remote.dir, clone)
	gone := c.remote.gitInput("a blob the clone does not hold\n", "hash-object", "--stdin")
	var entries []string
	for _, entry := range strings.Split(c.remote.git("-C", clone, "ls-tree", c.m6), "\n") {
		if strings.HasSuffix(entry, "\tf.txt") {
			entry = "100644 blob " + gone + "\tf.txt"
		}
		entries = append(entries, entry)
	}
	tree := c.remote.gitInput(strings.Join(entries, "\n")+"\n", "-C", clone, "mktree", "--missing")
	x := c.remote.git("-C", clone, "commit-tree", "-p", c.m6, "-m", "x", tree)
	y := c.remote.git("-C", clone, "commit-tree", "-p", x, "-m", "y", c.m6+"^{tree}")
	leftover := filepath.Join(clone, gitMergeObjects, "ab")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, strings.Repeat("c", 38)), []byte("left by a stopped run\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	c.window.Merges["acme/widgets"] = append(c.window.Merges["acme/widgets"],
		Merge{SHA: x, At: capturedTime(t, "2001-03-05T10:45:00Z"), Branch: "main"}, Merge{SHA: y, At: capturedTime(t, "2001-03-05T10:50:00Z"), Branch: "main"})
	heads := []GitHead{{SHA: c.p1, At: c.window.From, From: GitHeadCommit}}
	c.window.Git = map[string]GitInput{"acme/widgets": {Pulls: []GitPull{{Number: 13, Base: "main", Heads: heads}}}}
	dir := c.windowDir(t)
	calls := &gitCalls{}
	var before clonePicture
	config := c.gitOnly(dir, clones, calls)
	config.onGit = pictureBeforeMerges(t, clone, calls, &before)
	if _, err := CollectGit(context.Background(), config); err != nil {
		t.Fatalf("CollectGit --git-only: %v", err)
	}
	alone := []string{"merge-tree", "--write-tree", "--no-messages", "--name-only", "-z", x, c.p1}
	if got, want := callsOf(calls.take(), "merge-tree"), [][]string{batchMergeTree, alone}; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("git ran the merge-trees %q, want the one of p1's seven bases and x's alone %q", got, want)
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow of the window collect-git wrote: %v", err)
	}
	pull, held := read.Git["acme/widgets"].Pull(13)
	if !held || len(pull.Heads) != 1 {
		t.Fatalf("the git file holds #13 as %+v (%v), want its head p1", pull, held)
	}
	clean := func(base string) GitVerdict { return GitVerdict{Base: base, Paths: []string{}} }
	want := []GitVerdict{clean(c.m1), {Base: c.m3, Conflict: true, Paths: []string{"f.txt"}}, {Base: c.m4, Conflict: true, Paths: []string{"f.txt"}},
		clean(c.m5), clean(c.m6)}
	if h := pull.Heads[0]; h.Tip != y || h.MergeBase != c.c1 || !sameReading(h.Verdicts, want) {
		t.Errorf("the git file holds p1 as %s, want it read against y from c1 with the verdicts %s", describeReading(h), describeReading(want))
	}
	var missing []string
	for _, line := range gitFileLines(t, dir, mergeFailed) {
		if strings.HasPrefix(line, `{"kind":"missing"`) {
			missing = append(missing, line)
		}
	}
	wantMissing := []string{fmt.Sprintf(`{"kind":"missing","pr":13,"head":%q,"base":%q,"reason":"git-error","detail":"%s%s"}`, c.p1, x, mergeFailed,
		gitDetailOfGit)}
	if !slices.Equal(missing, wantMissing) {
		t.Errorf("the git file holds the missing lines%s\nwant%s", describeLines(missing), describeLines(wantMissing))
	}
	if data, err := os.ReadFile(filepath.Join(dir, "git", "acme__widgets.jsonl")); err != nil || !bytes.Contains(data, []byte(gone)) {
		t.Errorf("the git file (%v) names no blob %s, which x's merge-tree alone cannot read:\n%s", err, gone, data)
	}
	checkMergesWroteNothing(t, clone, before)
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

// The git phase's fetching alone (FetchOnly) reads no GitHub API, clones the repository and fetches
// every commit the merges of the window's heads read, as the git phase does, but reads no head,
// merges nothing and leaves the window's git file and window.json byte for byte as they were. Here
// the git file the GitHub phase wrote gives #12, merged as m5, the heads p1, p1b and p2, and #13 the
// head q, off c1; the remote holds every head, and #12's head ref at p1b. The fetching alone runs no
// git but the clone, the checks of what the clone holds, the ask for the head refs and the fetches;
// the clone then holds each head, each commit of main the merges file holds and c1, the base
// commit a read names, with every object they reach; and the run counts those eleven commits held
// and no head or base commit the remote did not give. The merging alone (MergeOnly) on that clone,
// its origin remote removed, runs no git command that names the remote, a clone, a fetch or an
// ls-remote, and writes the git file and window.json the git phase alone writes on a copy of the
// window, byte for byte. It fails where the fetching alone reads or merges a head or writes the
// window, where the clone lacks an object a merge reads, or where the merging alone reaches the
// remote or writes another file than the git phase alone.
func TestCollectGitsFetchingAloneThenMergingAloneWriteWhatTheGitPhaseWrites(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	c.remote.git("update-ref", "refs/pull/12/head", c.p1b)
	c.remote.git("checkout", "--quiet", "--detach", c.c1)
	q := c.remote.commit("q", map[string]string{"f.txt": nineLines(map[int]string{5: "PR"}), "other.txt": "o3\n"})
	c.remote.git("checkout", "--quiet", "main")
	at := func(clock string) time.Time { return capturedTime(t, "2001-03-05T"+clock+"Z") }
	c.window.Git = map[string]GitInput{"acme/widgets": {Pulls: []GitPull{
		{Number: 12, Merge: c.m5, Base: "main", Heads: []GitHead{{SHA: c.p1, At: at("07:30:00"), From: GitHeadCommit},
			{SHA: c.p1b, At: at("09:30:00"), From: GitHeadRead}, {SHA: c.p2, At: at("10:00:00"), From: GitHeadForcePush}}},
		{Number: 13, Base: "main", Heads: []GitHead{{SHA: q, At: at("09:15:00"), From: GitHeadCommit}}},
	}}}
	split, whole := c.windowDir(t), c.windowDir(t)
	clones := filepath.Join(t.TempDir(), "clones")
	clone := filepath.Join(clones, "acme__widgets")
	before := windowBytes(t, split)
	calls := &gitCalls{}
	fetched, err := CollectGit(context.Background(), c.fetchOnly(split, clones, calls))
	if err != nil {
		t.Fatalf("CollectGit --fetch-only: %v", err)
	}
	ran := calls.take()
	for _, call := range ran {
		if !slices.Contains(fetchingAlone, call[0]) {
			t.Errorf("the fetching alone ran git %q, want none but %q", call, fetchingAlone)
		}
	}
	if countCalls(ran, "clone") != 1 || countCalls(ran, "ls-remote") == 0 || countCalls(ran, "fetch") == 0 {
		t.Errorf("the fetching alone ran git %q, want a clone, an ask for the head refs and fetches", ran)
	}
	checkSameBytes(t, "the window after the fetching alone", windowBytes(t, split), before)
	commits := []string{c.p1, c.p1b, c.p2, q, c.m1, c.m2, c.m3, c.m4, c.m5, c.m6, c.c1}
	if lacked := lackedObjects(t, clone, commits...); len(lacked) != 0 {
		t.Errorf("the clone the fetching alone left lacks the objects %q, which a merge reads", lacked)
	}
	if fetched.Held != len(commits) || fetched.LackingHeads != 0 || fetched.LackingBases != 0 {
		t.Errorf("the fetching alone counts %d commits held and %d heads and %d base commits the remote did not give, want %d, 0 and 0",
			fetched.Held, fetched.LackingHeads, fetched.LackingBases, len(commits))
	}

	c.remote.git("-C", clone, "remote", "remove", "origin")
	if _, err := CollectGit(context.Background(), c.mergeOnly(split, clones, calls)); err != nil {
		t.Fatalf("CollectGit --merge-only: %v", err)
	}
	checkNoRemote(t, calls.take())
	if _, err := CollectGit(context.Background(), c.gitOnly(whole, filepath.Join(t.TempDir(), "clones"), &gitCalls{})); err != nil {
		t.Fatalf("CollectGit --git-only: %v", err)
	}
	checkSameBytes(t, "the window the fetching alone and then the merging alone wrote", windowBytes(t, split), windowBytes(t, whole))
	read, err := ReadWindow(whole)
	if err != nil {
		t.Fatalf("ReadWindow of the window collect-git wrote: %v", err)
	}
	input, heads := read.Git["acme/widgets"], 0
	for _, pull := range input.Pulls {
		for _, h := range pull.Heads {
			if h.Read() && len(h.Verdicts) > 0 {
				heads++
			}
		}
	}
	if heads != 4 || len(input.Missing) != 0 {
		t.Errorf("the git phase alone wrote %d heads read with verdicts and the missing inputs %+v, want the 4 heads and none missing", heads, input.Missing)
	}
}

// The git phase's merging alone (MergeOnly) opens no connection to the remote, though a head the
// clone lacks is among its heads. Here the GitHub phase alone writes the case's git file (gitCase),
// and the fetching alone fills the clone with each commit the merges read but #13's head, which
// the remote does not hold, and counts ten commits held and one head the remote did not give. Then
// the clone's origin names a directory that does not exist, and its upload-pack, which git starts
// as it reaches a remote of a path, for an ls-remote as for a fetch, is a script that writes a
// marker file. The merging alone ends without error, writes #12's heads with their verdicts and
// #13's head as a head-unreachable missing input whose detail says the clone does not hold the
// commit and the run asks the remote for nothing (notInClone), runs no git command that names the
// remote, a clone, a fetch or an ls-remote, and leaves no marker. The git phase alone in the same
// setting reaches for #13's head and leaves the marker, so a merging alone that fell back to a fetch
// would too. It fails where the merging alone reaches the remote, fails on the head the clone
// lacks, or writes the file otherwise.
func TestCollectGitMergingAloneOpensNoConnectionToTheRemote(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	clone := filepath.Join(clones, "acme__widgets")
	calls := &gitCalls{}
	config := c.config(dir, clones, calls)
	config.GitHubOnly = true
	if _, err := CollectGit(context.Background(), config); err != nil {
		t.Fatalf("CollectGit --github-only: %v", err)
	}
	fetched, err := CollectGit(context.Background(), c.fetchOnly(dir, clones, calls))
	if err != nil {
		t.Fatalf("CollectGit --fetch-only: %v", err)
	}
	if fetched.Held != 10 || fetched.LackingHeads != 1 || fetched.LackingBases != 0 {
		t.Errorf("the fetching alone counts %d commits held and %d heads and %d base commits the remote did not give, want 10, 1 and 0",
			fetched.Held, fetched.LackingHeads, fetched.LackingBases)
	}
	scratch := t.TempDir()
	marker, uploadPack := filepath.Join(scratch, "marker"), filepath.Join(scratch, "upload-pack")
	if err := os.WriteFile(uploadPack, fmt.Appendf(nil, "#!/bin/sh\necho \"$@\" >> '%s'\nexit 1\n", marker), 0o755); err != nil {
		t.Fatal(err)
	}
	c.remote.git("-C", clone, "remote", "set-url", "origin", "file://"+filepath.Join(scratch, "gone"))
	c.remote.git("-C", clone, "config", "remote.origin.uploadpack", uploadPack)
	calls.take()
	if _, err := CollectGit(context.Background(), c.mergeOnly(dir, clones, calls)); err != nil {
		t.Fatalf("CollectGit --merge-only with a head the clone lacks: %v", err)
	}
	checkNoRemote(t, calls.take())
	if data, err := os.ReadFile(marker); err == nil {
		t.Errorf("the merging alone reached the remote: git started its upload-pack for %q", data)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	want := c.wantLines(true)
	want[len(want)-1] = fmt.Sprintf(`{"kind":"missing","pr":13,"head":%q,"base":"","reason":"head-unreachable","detail":%q}`, unreachableHead, notInClone)
	if got := gitFileLines(t, dir, fetchFailed); !slices.Equal(got, want) {
		t.Errorf("the merging alone wrote%s\nwant%s", describeLines(got), describeLines(want))
	}

	if _, err := CollectGit(context.Background(), c.gitOnly(dir, clones, &gitCalls{})); err != nil {
		t.Fatalf("CollectGit --git-only: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the git phase alone, which reaches for #13's head, left no marker (%v), so the marker shows nothing of a reach", err)
	}
}

// The git phase's merging alone (MergeOnly) refuses a clones' directory that holds no clone of the
// repository, which only a clone from the remote could make, with an error that names the clone's
// directory and says to run the fetching alone first; it runs no git, makes no directory, and leaves
// the window's git file and window.json byte for byte as they were. Here the git file alone gives
// #13 the one head p1.
func TestCollectGitMergingAloneRefusesAClonesDirectoryWithNoClone(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	heads := []GitHead{{SHA: c.p1, At: c.window.From, From: GitHeadCommit}}
	c.window.Git = map[string]GitInput{"acme/widgets": {Pulls: []GitPull{{Number: 13, Base: "main", Heads: heads}}}}
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	clone := filepath.Join(clones, "acme__widgets")
	before := windowBytes(t, dir)
	calls := &gitCalls{}
	if _, err := CollectGit(context.Background(), c.mergeOnly(dir, clones, calls)); err == nil || !strings.Contains(err.Error(), clone) ||
		!strings.Contains(err.Error(), "run --fetch-only first") {
		t.Errorf("CollectGit --merge-only with no clone in %s = %v, want a refusal that names %s and says to run --fetch-only first", clones, err, clone)
	}
	if ran := calls.take(); len(ran) != 0 {
		t.Errorf("the refused merging alone ran git %q, want none", ran)
	}
	if _, err := os.Stat(clones); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused merging alone left the clones' directory %s (%v)", clones, err)
	}
	checkSameBytes(t, "the window after the refused merging alone", windowBytes(t, dir), before)
}

// collect-git's four runs of a part alone exclude each other: the GitHub phase alone, the git phase
// alone, the git phase's fetching alone and its merging alone. The fetching alone and the merging
// alone, as the git phase alone, read no GitHub API, so each refuses a GitHub token, GitHub's API, a
// reserve and a rate. Each refusal comes before any read of GitHub or git, with nothing written.
func TestCollectGitRefusesTheRunsThatExcludeEachOther(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	calls := &gitCalls{}
	runs := []struct {
		name string
		set  func(*GitConfig)
	}{
		{"the GitHub phase alone", func(config *GitConfig) { config.GitHubOnly = true }},
		{"the git phase alone", func(config *GitConfig) { config.GitOnly = true }},
		{"the git phase's fetching alone", func(config *GitConfig) { config.FetchOnly = true }},
		{"the git phase's merging alone", func(config *GitConfig) { config.MergeOnly = true }},
	}
	for i, a := range runs {
		for _, b := range runs[i+1:] {
			config := c.config(dir, clones, calls)
			a.set(&config)
			b.set(&config)
			want := a.name + " and " + b.name + " exclude each other"
			if _, err := CollectGit(context.Background(), config); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("CollectGit of %s and %s = %v, want the refusal %q", a.name, b.name, err, want)
			}
		}
	}
	for _, run := range runs[2:] {
		for _, share := range []struct {
			name string
			set  func(*GitConfig)
		}{
			{"a GitHub token", func(config *GitConfig) { config.Token = stubGitHubToken }},
			{"GitHub's API", func(config *GitConfig) { config.API = c.server.URL }},
			{"a reserve", func(config *GitConfig) { config.Reserve = 10 }},
			{"a rate", func(config *GitConfig) { config.Rate = 1 }},
		} {
			config := c.gitOnly(dir, clones, calls)
			config.GitOnly = false
			run.set(&config)
			share.set(&config)
			want := run.name + " reads no GitHub API, so it takes no GitHub token, API, reserve or rate"
			if _, err := CollectGit(context.Background(), config); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("CollectGit of %s with %s = %v, want the refusal %q", run.name, share.name, err, want)
			}
		}
	}
	if ran, sent := calls.take(), c.seen.all(); len(ran) != 0 || len(sent) != 0 {
		t.Errorf("the refused runs ran git %q and sent GitHub %v, want nothing", ran, sent)
	}
	if _, err := os.Stat(filepath.Join(dir, "git")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused run left the window's git directory (%v)", err)
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

// parallelPulls gives the case's window the git file the GitHub phase writes of five pull requests
// of several heads each, for the git phase alone: #12, merged as m5, with its heads p1, p1b and p2
// (gitCase); #13 with unreachableHead, which the remote does not hold, and three heads of its own;
// and #14, #15 and #16, which ACME-2's events attribute to it too, with three heads each. Each head
// of #13 to #16 is a commit of its own off c1 or m1 that changes f.txt's fifth line, which m3
// changes too, so its merges conflict from m3 on, or its seventh, which no commit of main changes,
// so they are clean; and the third head of each changes other.txt too, which m2 changes. It is the
// heads of each pull request by its number.
func (c *gitCase) parallelPulls(t *testing.T) map[int][]string {
	t.Helper()
	at := func(minute int) time.Time { return c.window.From.Add(time.Duration(minute) * time.Minute) }
	heads := map[int][]string{12: {c.p1, c.p1b, c.p2}}
	pulls := []GitPull{{Number: 12, Merge: c.m5, Base: "main", Heads: []GitHead{{SHA: c.p1, At: at(0), From: GitHeadCommit},
		{SHA: c.p1b, At: at(1), From: GitHeadRead}, {SHA: c.p2, At: at(2), From: GitHeadForcePush}}}}
	for number := 13; number <= 16; number++ {
		if number > 13 {
			c.window.Events["ACME-2"] = append(c.window.Events["ACME-2"], inputsAttribution(number-9, number-11, "ACME-2", "acme/widgets", number))
		}
		pull := GitPull{Number: number, Base: "main"}
		if number == 13 {
			pull.Heads = append(pull.Heads, GitHead{SHA: unreachableHead, At: at(0), From: GitHeadCommit})
		}
		for k := range 3 {
			base, first := c.c1, "1"
			if (number+k)%2 == 1 {
				base, first = c.m1, "A"
			}
			own := fmt.Sprintf("#%d-%d", number, k)
			files := map[string]string{"f.txt": nineLines(map[int]string{1: first, 5 + 2*(k%2): own})}
			if k == 2 {
				files["other.txt"] = own + "\n"
			}
			c.remote.git("checkout", "--quiet", "--detach", base)
			sha := c.remote.commit("head "+own, files)
			heads[number] = append(heads[number], sha)
			pull.Heads = append(pull.Heads, GitHead{SHA: sha, At: at(1 + k), From: GitHeadCommit})
		}
		pulls = append(pulls, pull)
	}
	c.remote.git("checkout", "--quiet", "main")
	c.window.Git = map[string]GitInput{"acme/widgets": {Pulls: pulls}}
	return heads
}

// mergeBarrier is an onGit seam that holds the first merge-tree collect-git runs until a second
// one starts, so that the two run at once, and that, where no second one starts within bound,
// fails t and lets the first go on, so a run of one git command at a time ends. Only a run that
// merges two heads at once passes it.
type mergeBarrier struct {
	t       *testing.T
	bound   time.Duration
	mu      sync.Mutex
	started int
	second  chan struct{}
}

func newMergeBarrier(t *testing.T, bound time.Duration) *mergeBarrier {
	return &mergeBarrier{t: t, bound: bound, second: make(chan struct{})}
}

func (b *mergeBarrier) onGit(args []string) {
	if args[0] != "merge-tree" {
		return
	}
	b.mu.Lock()
	b.started++
	started := b.started
	b.mu.Unlock()
	switch started {
	case 1:
		select {
		case <-b.second:
		case <-time.After(b.bound):
			b.t.Errorf("no second merge-tree started within %s of the first, which waited for one: the git phase ran one git command at a time", b.bound)
		}
	case 2:
		close(b.second)
	}
}

// collect-git's git phase with Parallel 4 runs its reads and merges of four heads at once, and
// writes what it writes with Parallel 1, byte for byte. Here the git file alone gives five pull
// requests of several heads each (parallelPulls), with clean and conflicting merges and a head the
// remote does not hold. The git phase alone with Parallel 4 writes the git file and window.json the
// git phase alone writes with Parallel 1, and tells Finished of the same pull requests with the same
// counts in the same order; and two of its merge-trees ran at once, since the first waits for the
// second to start (mergeBarrier). It fails where the run with Parallel 4 merges one head at a time,
// writes another file, or finishes the pull requests in another order or with other counts.
func TestCollectGitInParallelWritesWhatOneAtATimeWrites(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	c.parallelPulls(t)
	one, four := c.windowDir(t), c.windowDir(t)
	run := func(dir string, parallel int, onGit func([]string)) (GitCollected, []GitPullFinished) {
		t.Helper()
		var finished []GitPullFinished
		config := c.gitOnly(dir, filepath.Join(t.TempDir(), "clones"), &gitCalls{})
		config.Parallel, config.Finished = parallel, func(done GitPullFinished) { finished = append(finished, done) }
		if onGit != nil {
			config.onGit = onGit
		}
		collected, err := CollectGit(context.Background(), config)
		if err != nil {
			t.Fatalf("CollectGit --git-only with Parallel %d: %v", parallel, err)
		}
		return collected, finished
	}
	collected, alone := run(one, 1, nil)
	if collected.Conflicts == 0 || collected.Verdicts == collected.Conflicts || collected.Missing[GitHeadUnreachable] != 1 || len(alone) != 5 {
		t.Fatalf("the git phase alone with Parallel 1 counts %+v and finished %+v, want clean and conflicting verdicts, one head-unreachable and five pull requests",
			collected, alone)
	}
	barrier := newMergeBarrier(t, 30*time.Second)
	_, together := run(four, 4, barrier.onGit)
	checkSameBytes(t, "the window the git phase alone wrote with Parallel 4", windowBytes(t, four), windowBytes(t, one))
	if !slices.Equal(together, alone) {
		t.Errorf("the git phase alone with Parallel 4 finished%s\nwant, as with Parallel 1,%s", describeFinished(together), describeFinished(alone))
	}
}

// describeFinished is finished, a pull request to a line, for a failure's message.
func describeFinished(finished []GitPullFinished) string {
	lines := make([]string, len(finished))
	for i, done := range finished {
		lines[i] = fmt.Sprintf("%+v", done)
	}
	return describeLines(lines)
}

// A run of the git phase with Parallel 4 that stops once the file holds its first pull request
// leaves the file as it stood then, and a run again with Parallel 4 asks git for nothing that file
// holds and writes the file one whole run writes. Here (parallelPulls) the first run's context is
// cancelled as Finished is told of #12, the first pull request: the run ends with the context's
// error and leaves the file as it was then, #12's heads read and no later pull request's. The run
// again runs no diff-tree, since the file holds the commits, and no git command that names a head
// of #12 or its merge m5; and it writes, byte for byte, what one run with Parallel 1 writes. It
// fails where a stopped run writes a pull request before an earlier one is whole, or where a run
// again reads a head the file holds or writes another file.
func TestCollectGitInParallelGoesOnWhereAStoppedRunStopped(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	heads := c.parallelPulls(t)
	whole, stopped := c.windowDir(t), c.windowDir(t)
	if _, err := CollectGit(context.Background(), c.gitOnly(whole, filepath.Join(t.TempDir(), "clones"), &gitCalls{})); err != nil {
		t.Fatalf("CollectGit --git-only: %v", err)
	}
	clones := filepath.Join(t.TempDir(), "clones")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var held map[string][]byte
	config := c.gitOnly(stopped, clones, &gitCalls{})
	config.Parallel, config.Finished = 4, func(GitPullFinished) {
		if held == nil {
			held = windowBytes(t, stopped)
			cancel()
		}
	}
	if _, err := CollectGit(ctx, config); !errors.Is(err, context.Canceled) {
		t.Fatalf("CollectGit --git-only with Parallel 4, cancelled as #12 finished = %v, want the context's cancellation", err)
	}
	if held == nil {
		t.Fatal("the stopped run told Finished of no pull request")
	}
	checkSameBytes(t, "the window the stopped run left", windowBytes(t, stopped), held)
	read, err := ReadWindow(stopped)
	if err != nil {
		t.Fatalf("ReadWindow of the window the stopped run left: %v", err)
	}
	for _, pull := range read.Git["acme/widgets"].Pulls {
		for _, h := range pull.Heads {
			if h.Read() != (pull.Number == 12) {
				t.Errorf("the stopped run left #%d's head %s read %v, want #12's heads read and no other", pull.Number, h.SHA, h.Read())
			}
		}
	}
	calls := &gitCalls{}
	config = c.gitOnly(stopped, clones, calls)
	config.Parallel = 4
	if _, err := CollectGit(context.Background(), config); err != nil {
		t.Fatalf("CollectGit --git-only with Parallel 4 again: %v", err)
	}
	ran := calls.take()
	if trees := countCalls(ran, "diff-tree"); trees != 0 {
		t.Errorf("the run again ran %d diff-trees, want none: the file holds the commits", trees)
	}
	held12 := append(slices.Clone(heads[12]), c.m5)
	for _, call := range ran {
		if slices.ContainsFunc(call, func(arg string) bool {
			return slices.ContainsFunc(held12, func(sha string) bool { return strings.Contains(arg, sha) })
		}) {
			t.Errorf("the run again ran git %q, which names a head of #12 or its merge, which the file holds", call)
		}
	}
	checkSameBytes(t, "the window the run again wrote", windowBytes(t, stopped), windowBytes(t, whole))
}

// collect-git's merges with Parallel 4 leave nothing in the clone: no object a merge-tree wrote and
// no directory of the merges' objects (gitMergeObjects); and the directory of the merges' objects
// a stopped run left, with a read-only object in it, goes. Here the fetching alone fills the clone
// with what the merges of parallelPulls read, as the test pictures it (pictureClone); then the
// clone gains that left directory, and the merging alone with Parallel 4 merges each head. It fails
// where a merge-tree wrote into the clone's objects or a directory of the merges' objects is left.
func TestCollectGitInParallelLeavesNothingOfItsMergesInTheClone(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	c.parallelPulls(t)
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	clone := filepath.Join(clones, "acme__widgets")
	if _, err := CollectGit(context.Background(), c.fetchOnly(dir, clones, &gitCalls{})); err != nil {
		t.Fatalf("CollectGit --fetch-only: %v", err)
	}
	before := pictureClone(t, clone)
	leftover := filepath.Join(clone, gitMergeObjects, "3", "ab")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, strings.Repeat("c", 38)), []byte("left by a stopped run\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	calls := &gitCalls{}
	config := c.mergeOnly(dir, clones, calls)
	config.Parallel = 4
	if _, err := CollectGit(context.Background(), config); err != nil {
		t.Fatalf("CollectGit --merge-only with Parallel 4: %v", err)
	}
	if merges := countCalls(calls.take(), "merge-tree"); merges < 2 {
		t.Errorf("the merging alone ran %d merge-trees, want one for each head the clone holds", merges)
	}
	checkMergesWroteNothing(t, clone, before)
}

// A clones' directory whose path holds a ':', which git splits GIT_ALTERNATE_OBJECT_DIRECTORIES on,
// and a '"' gets each verdict, from a merge-tree --stdin and from a merge-tree alone: collect-git
// names the clone's objects to git as a C-quoted path where the path holds either. Here the case
// (gitCase) runs into clones in such a path twice: once as it stands, each of its three heads
// merged in a merge-tree --stdin, and once on a window of its own with each merge-tree --stdin
// killed as it starts, the guard reading its memory past the cap, so that each of the eight
// verdicts comes from a merge-tree alone. Each run writes the case's file. It fails where a merge
// in such a clone is a missing input.
func TestCollectGitMergesInAClonesPathThatHoldsAColon(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	clones := filepath.Join(t.TempDir(), `clo:nes "q"`)
	batched, alone := c.windowDir(t), c.windowDir(t)
	calls := &gitCalls{}
	if _, err := CollectGit(context.Background(), c.config(batched, clones, calls)); err != nil {
		t.Fatalf("CollectGit: %v", err)
	}
	if got, want := callsOf(calls.take(), "merge-tree"), slices.Repeat([][]string{batchMergeTree}, 3); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("git ran the merge-trees %q, want %q, one for each of p1, p1b and p2", got, want)
	}
	if got, want := gitFileLines(t, batched, fetchFailed), c.wantLines(true); !slices.Equal(got, want) {
		t.Errorf("the merges in --stdin in %s wrote%s\nwant%s", clones, describeLines(got), describeLines(want))
	}

	var batch atomic.Bool
	config := c.config(alone, clones, calls)
	config.onGit = func(args []string) {
		calls.add(args)
		batch.Store(slices.Equal(args, batchMergeTree))
	}
	config.rss = func(pgid int) int64 {
		if batch.Load() {
			return DefaultGitMemoryCap + 1
		}
		return groupRSS(pgid)
	}
	if _, err := CollectGit(context.Background(), config); err != nil {
		t.Fatalf("CollectGit with each merge-tree --stdin killed: %v", err)
	}
	merges := callsOf(calls.take(), "merge-tree")
	batches := len(slices.DeleteFunc(slices.Clone(merges), func(call []string) bool { return !slices.Equal(call, batchMergeTree) }))
	if batches != 3 || len(merges)-batches != 8 {
		t.Errorf("git ran the merge-trees %q, want three --stdin, each killed, and eight alone", merges)
	}
	if got, want := gitFileLines(t, alone, fetchFailed), c.wantLines(true); !slices.Equal(got, want) {
		t.Errorf("the merges alone in %s wrote%s\nwant%s", clones, describeLines(got), describeLines(want))
	}
}

// collect-git gives git the clone's objects as an entry of GIT_ALTERNATE_OBJECT_DIRECTORIES
// (gitAlternate): a plain path as it is, and one that holds a ':', which git splits the variable
// on, or a '"', in double quotes, as git reads a C-quoted path, a backslash and a double quote each
// after a backslash and a control character as three octal digits. It fails where a plain path is
// quoted or a path git would split or misread is not quoted so.
func TestCollectGitQuotesTheClonesObjectsWhereGitWouldSplitThem(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ dir, want string }{
		{"/work/clones/acme__widgets/objects", "/work/clones/acme__widgets/objects"},
		{`/work/back\slash/objects`, `/work/back\slash/objects`},
		{"/work/clo:nes/objects", `"/work/clo:nes/objects"`},
		{`/work/clo:nes "q"/objects`, `"/work/clo:nes \"q\"/objects"`},
		{"/work/a:b\\c\nd/objects", `"/work/a:b\\c\012d/objects"`},
	} {
		if got := gitAlternate(test.dir); got != test.want {
			t.Errorf("gitAlternate(%q) = %q, want %q", test.dir, got, test.want)
		}
	}
}

// collect-git refuses, before any read of GitHub or git and with nothing written, a parallel count
// below zero, and one above 1 for the GitHub phase alone and for the fetching alone, which run none
// of the git phase's reads and merges (GitConfig.Parallel).
func TestCollectGitRefusesAParallelCountWhereNoMergeRuns(t *testing.T) {
	t.Parallel()
	c := newGitCase(t)
	dir, clones := c.windowDir(t), filepath.Join(t.TempDir(), "clones")
	calls := &gitCalls{}
	below := c.gitOnly(dir, clones, calls)
	below.Parallel = -1
	github := c.config(dir, clones, calls)
	github.GitHubOnly, github.Parallel = true, 2
	fetch := c.fetchOnly(dir, clones, calls)
	fetch.Parallel = 2
	for _, test := range []struct {
		name   string
		config GitConfig
		want   string
	}{
		{"the git phase alone with a parallel count of -1", below, "the parallel count -1 is below zero"},
		{"the GitHub phase alone with a parallel count of 2", github,
			"the GitHub phase alone runs none of the git phase's reads and merges, so it takes no parallel count above 1"},
		{"the fetching alone with a parallel count of 2", fetch,
			"the git phase's fetching alone runs none of the git phase's reads and merges, so it takes no parallel count above 1"},
	} {
		if _, err := CollectGit(context.Background(), test.config); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("CollectGit of %s = %v, want the refusal %q", test.name, err, test.want)
		}
	}
	if ran, sent := calls.take(), c.seen.all(); len(ran) != 0 || len(sent) != 0 {
		t.Errorf("the refused runs ran git %q and sent GitHub %v, want nothing", ran, sent)
	}
	if _, err := os.Stat(filepath.Join(dir, "git")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused run left the window's git directory (%v)", err)
	}
}
