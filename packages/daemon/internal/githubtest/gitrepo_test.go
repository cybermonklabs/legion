package githubtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// serveRepo is a fake GitHub that answers what g answers and fails the test on any other request.
func serveRepo(t *testing.T, g *GitRepo) string {
	t.Helper()
	return Server(t, Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if !g.Handler(w, r) {
			Unexpected(t, w, r)
		}
	}})
}

// send makes one request of the fake and answers its status and body.
func send(t *testing.T, method, url string, body any) (int, string) {
	t.Helper()
	return sendAs(t, "", method, url, body)
}

// sendAs is send with token as the request's bearer, none for "".
func sendAs(t *testing.T, token, method, url string, body any) (int, string) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %v: %v", body, err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, payload)
	if err != nil {
		t.Fatalf("make %s %s: %v", method, url, err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, url, err)
	}
	return response.StatusCode, string(answer)
}

// gitIn runs git in the bare repository dir and answers its output, trimmed.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = fixtureenv.Isolated(t.TempDir())
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The fake answers the reads and the create of a ref as GitHub's REST does: the repository names
// its default branch, a branch it has names its head and one it lacks is 404, a create makes the
// ref at the commit it names, and a create of a ref it has is 422 "Reference already exists" with
// the ref left where it was. A path it does not serve is the caller's.
func TestTheFakeReadsAndCreatesRefsAsGitHubDoes(t *testing.T) {
	g := GitRepository(t, "acme", "widgets", "main")
	api := serveRepo(t, g)
	main := g.Head(t, "main")
	if main == "" || main != gitIn(t, g.Dir(), "rev-parse", "refs/heads/main") {
		t.Fatalf("Head(main) = %q, want the one commit the repository starts with", main)
	}
	if status, body := send(t, http.MethodGet, api+"/repos/acme/widgets", nil); status != http.StatusOK || !strings.Contains(body, `"default_branch":"main"`) || !strings.Contains(body, `"full_name":"acme/widgets"`) {
		t.Fatalf("GET the repository = %d %s", status, body)
	}
	if status, body := send(t, http.MethodGet, api+"/repos/acme/widgets/git/ref/heads/main", nil); status != http.StatusOK || !strings.Contains(body, main) {
		t.Fatalf("GET refs/heads/main = %d %s, want its head %s", status, body, main)
	}
	if status, _ := send(t, http.MethodGet, api+"/repos/acme/widgets/git/ref/heads/legion/ACME-1", nil); status != http.StatusNotFound {
		t.Fatalf("GET a branch the repository lacks = %d, want 404", status)
	}
	if status, body := send(t, http.MethodPost, api+"/repos/acme/widgets/git/refs", map[string]string{"ref": "refs/heads/legion/ACME-1", "sha": main}); status != http.StatusCreated {
		t.Fatalf("create legion/ACME-1 = %d %s, want 201", status, body)
	}
	moved := g.Commit(t, "legion/ACME-1", map[string]string{"b.txt": "b\n"}, "b")
	if status, body := send(t, http.MethodPost, api+"/repos/acme/widgets/git/refs", map[string]string{"ref": "refs/heads/legion/ACME-1", "sha": main}); status != http.StatusUnprocessableEntity || !strings.Contains(body, "Reference already exists") {
		t.Fatalf("create of a ref the repository has = %d %s, want 422 Reference already exists", status, body)
	}
	if head := g.Head(t, "legion/ACME-1"); head != moved {
		t.Fatalf("legion/ACME-1 after the second create stands at %s, want %s, where it was", head, moved)
	}
	recorder := httptest.NewRecorder()
	if g.Handler(recorder, httptest.NewRequest(http.MethodPost, "/repos/acme/widgets/issues/1/comments", nil)) {
		t.Fatal("the fake answered POST /repos/acme/widgets/issues/1/comments, a path it does not serve")
	}
	if recorder.Body.Len() != 0 || recorder.Code != http.StatusOK {
		t.Fatalf("the fake wrote %d %q for a path it does not serve, want nothing written", recorder.Code, recorder.Body.String())
	}
}

// A merge of one branch into another is GitHub's: a merge commit on the base whose parents are the
// base's head and the head's head, in that order, answered with 201 and its sha; a head the base
// already holds is 204 and moves nothing.
func TestTheFakeMergesTwoBranchesAsGitHubDoes(t *testing.T) {
	g := GitRepository(t, "acme", "widgets", "main")
	api := serveRepo(t, g)
	a := g.Commit(t, "a", map[string]string{"a.txt": "a\n"}, "a")
	c := g.Commit(t, "c", map[string]string{"c.txt": "c\n"}, "c")
	status, body := send(t, http.MethodPost, api+"/repos/acme/widgets/merges", map[string]string{"base": "a", "head": "c", "commit_message": "ACME-1: join the work of task c"})
	var merged struct {
		SHA string `json:"sha"`
	}
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &merged) != nil || merged.SHA == "" {
		t.Fatalf("merge c into a = %d %s, want 201 with the merge commit's sha", status, body)
	}
	if parents := gitIn(t, g.Dir(), "rev-list", "--parents", "-n", "1", merged.SHA); parents != merged.SHA+" "+a+" "+c {
		t.Fatalf("the merge commit and its parents = %q, want %s %s %s", parents, merged.SHA, a, c)
	}
	if head := g.Head(t, "a"); head != merged.SHA {
		t.Fatalf("a stands at %s after the merge, want the merge commit %s", head, merged.SHA)
	}
	if message := gitIn(t, g.Dir(), "log", "-1", "--format=%s", merged.SHA); message != "ACME-1: join the work of task c" {
		t.Fatalf("the merge commit's message = %q", message)
	}
	if status, body := send(t, http.MethodPost, api+"/repos/acme/widgets/merges", map[string]string{"base": "a", "head": "c", "commit_message": "again"}); status != http.StatusNoContent || body != "" {
		t.Fatalf("merge of a head the base holds = %d %q, want 204 and no body", status, body)
	}
	if head := g.Head(t, "a"); head != merged.SHA {
		t.Fatalf("a stands at %s after a merge of nothing, want %s", head, merged.SHA)
	}
	if status, _ := send(t, http.MethodPost, api+"/repos/acme/widgets/merges", map[string]string{"base": "a", "head": "missing", "commit_message": "x"}); status != http.StatusNotFound {
		t.Fatalf("merge of a head the repository lacks = %d, want 404", status)
	}
}

// Two branches that change the same line of one file conflict: GitHub answers 409 "Merge conflict"
// and the base stays where it was.
func TestTheFakeAnswersAConflictWith409(t *testing.T) {
	g := GitRepository(t, "acme", "widgets", "main")
	api := serveRepo(t, g)
	a := g.Commit(t, "a", map[string]string{"x.txt": "from a\n"}, "a")
	g.Commit(t, "c", map[string]string{"x.txt": "from c\n"}, "c")
	status, body := send(t, http.MethodPost, api+"/repos/acme/widgets/merges", map[string]string{"base": "a", "head": "c", "commit_message": "join"})
	if status != http.StatusConflict || !strings.Contains(body, "Merge conflict") {
		t.Fatalf("merge of a conflicting head = %d %s, want 409 Merge conflict", status, body)
	}
	if head := g.Head(t, "a"); head != a {
		t.Fatalf("a stands at %s after the conflict, want %s, where it was", head, a)
	}
}

// comparison is the part of GitHub's compare of two commits a test reads.
type comparison struct {
	Status          string `json:"status"`
	AheadBy         int    `json:"ahead_by"`
	BehindBy        int    `json:"behind_by"`
	TotalCommits    int    `json:"total_commits"`
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	Files *[]struct {
		Filename string `json:"filename"`
	} `json:"files"`
}

// compare is the fake's compare of base with head, each escaped as a path segment as the daemon's
// client escapes it (githubrest.Client.Compare), and its status.
func compare(t *testing.T, api, base, head string) (int, comparison) {
	t.Helper()
	status, body := send(t, http.MethodGet, api+"/repos/acme/widgets/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head)+"?per_page=1", nil)
	var read comparison
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &read); err != nil {
			t.Fatalf("decode the compare of %s with %s: %v: %s", base, head, err, body)
		}
	}
	return status, read
}

// The fake compares two commits as GitHub does, each named by its branch or its sha: the head's
// commits the base lacks and the base's commits the head lacks, the status they make (ahead,
// behind, identical or diverged), the merge base, and the files the head changes since the merge
// base. A branch the repository lacks is 404.
func TestTheFakeComparesTwoBranchesAsGitHubDoes(t *testing.T) {
	g := GitRepository(t, "acme", "widgets", "main")
	api := serveRepo(t, g)
	main := g.Head(t, "main")
	g.Commit(t, "legion/ACME-1", map[string]string{"a.txt": "a\n"}, "a")
	a := g.Commit(t, "legion/ACME-1", map[string]string{"b.txt": "b\n"}, "b")
	g.Commit(t, "legion/ACME-1.c", map[string]string{"c.txt": "c\n"}, "c")

	for _, tc := range []struct {
		base, head    string
		status        string
		ahead, behind int
		files         []string
	}{
		{base: "main", head: "legion/ACME-1", status: "ahead", ahead: 2, files: []string{"a.txt", "b.txt"}},
		{base: "legion/ACME-1", head: "main", status: "behind", behind: 2, files: []string{}},
		{base: "legion/ACME-1", head: a, status: "identical", files: []string{}},
		{base: "legion/ACME-1", head: "legion/ACME-1.c", status: "diverged", ahead: 1, behind: 2, files: []string{"c.txt"}},
	} {
		status, got := compare(t, api, tc.base, tc.head)
		if status != http.StatusOK || got.Status != tc.status || got.AheadBy != tc.ahead || got.BehindBy != tc.behind || got.TotalCommits != tc.ahead {
			t.Errorf("compare %s...%s = %d %+v, want %s, ahead by %d, behind by %d", tc.base, tc.head, status, got, tc.status, tc.ahead, tc.behind)
			continue
		}
		if got.MergeBaseCommit.SHA == "" {
			t.Errorf("compare %s...%s names no merge base", tc.base, tc.head)
		}
		var files []string
		if got.Files != nil {
			files = []string{}
			for _, file := range *got.Files {
				files = append(files, file.Filename)
			}
		}
		if !slices.Equal(files, tc.files) {
			t.Errorf("compare %s...%s lists files %v, want %v", tc.base, tc.head, files, tc.files)
		}
	}
	if _, got := compare(t, api, "legion/ACME-1", "legion/ACME-1.c"); got.MergeBaseCommit.SHA != main {
		t.Errorf("the merge base of legion/ACME-1 and legion/ACME-1.c = %s, want main's head %s", got.MergeBaseCommit.SHA, main)
	}
	if status, _ := compare(t, api, "main", "legion/ACME-1.gone"); status != http.StatusNotFound {
		t.Errorf("compare with a branch the repository lacks = %d, want 404", status)
	}
}

// pullRequest is the part of GitHub's pull request a test reads.
type pullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Base   struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// The fake opens a pull request as GitHub does: 201 with its number, state, branches and head
// commit, and 422 for a head that already has an open one, for a head with no commit its base
// lacks, or for a branch the repository lacks. It lists the pull requests of a head (head=
// owner:branch) and a state, reads one by its number, and keeps, for the test, the bearer each was
// opened with. A list or read names the head branch's commit as it stands when it is asked.
func TestTheFakeOpensAndListsPullRequestsAsGitHubDoes(t *testing.T) {
	g := GitRepository(t, "acme", "widgets", "main")
	api := serveRepo(t, g)
	g.Commit(t, "legion/ACME-1.d", map[string]string{"d.txt": "d\n"}, "d")
	gitIn(t, g.Dir(), "update-ref", "refs/heads/legion/ACME-1.e", g.Head(t, "main"))
	pulls := api + "/repos/acme/widgets/pulls"
	open := map[string]string{"title": "feat: d", "head": "legion/ACME-1.d", "base": "main", "body": "The work of d."}

	status, body := sendAs(t, "implement-app-token", http.MethodPost, pulls, open)
	var opened pullRequest
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &opened) != nil || opened.Number != 1 || opened.State != "open" ||
		opened.Base.Ref != "main" || opened.Head.Ref != "legion/ACME-1.d" || opened.Head.SHA != g.Head(t, "legion/ACME-1.d") ||
		opened.Head.Repo.FullName != "acme/widgets" || opened.Title != "feat: d" || opened.Body != "The work of d." {
		t.Fatalf("open a pull request of legion/ACME-1.d = %d %s, want 201 with number 1, open, on main", status, body)
	}
	for _, refused := range []struct {
		why  string
		head string
		want string
	}{
		{"a head that has an open pull request", "acme:legion/ACME-1.d", "A pull request already exists for acme:legion/ACME-1.d."},
		{"a head with no commit its base lacks", "legion/ACME-1.e", "No commits between main and legion/ACME-1.e"},
		{"a head the repository lacks", "legion/ACME-1.gone", "Validation Failed"},
	} {
		again := map[string]string{"title": "again", "head": refused.head, "base": "main"}
		if status, body := sendAs(t, "implement-app-token", http.MethodPost, pulls, again); status != http.StatusUnprocessableEntity || !strings.Contains(body, refused.want) {
			t.Errorf("open a pull request of %s = %d %s, want 422 %q", refused.why, status, body, refused.want)
		}
	}

	moved := g.Commit(t, "legion/ACME-1.d", map[string]string{"d.txt": "d, again\n"}, "d again")
	var listed []pullRequest
	status, body = send(t, http.MethodGet, pulls+"?"+url.Values{"head": {"acme:legion/ACME-1.d"}, "state": {"open"}}.Encode(), nil)
	if status != http.StatusOK || json.Unmarshal([]byte(body), &listed) != nil || len(listed) != 1 || listed[0].Number != 1 || listed[0].Head.SHA != moved {
		t.Fatalf("list the open pull requests of legion/ACME-1.d = %d %s, want number 1 at its head %s", status, body, moved)
	}
	for _, query := range []url.Values{{"head": {"acme:legion/ACME-1.e"}}, {"head": {"acme:legion/ACME-1.d"}, "state": {"closed"}}} {
		if status, body := send(t, http.MethodGet, pulls+"?"+query.Encode(), nil); status != http.StatusOK || strings.TrimSpace(body) != "[]" {
			t.Errorf("list the pull requests of %v = %d %s, want none", query, status, body)
		}
	}
	var read pullRequest
	if status, body := send(t, http.MethodGet, pulls+"/1", nil); status != http.StatusOK || json.Unmarshal([]byte(body), &read) != nil || read.Number != 1 || read.Head.SHA != moved {
		t.Fatalf("read pull request 1 = %d %s, want it at its head %s", status, body, moved)
	}
	if status, _ := send(t, http.MethodGet, pulls+"/2", nil); status != http.StatusNotFound {
		t.Errorf("read pull request 2, which nobody opened = %d, want 404", status)
	}
	if got := g.PullRequests(); len(got) != 1 || got[0].Number != 1 || got[0].Head != "legion/ACME-1.d" || got[0].Base != "main" || got[0].OpenedWith != "implement-app-token" {
		t.Fatalf("the fake's pull requests = %+v, want number 1 of legion/ACME-1.d on main, opened with implement-app-token", got)
	}
}
