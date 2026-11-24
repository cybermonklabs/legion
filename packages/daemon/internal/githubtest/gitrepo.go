package githubtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// GitRepo is a fake GitHub repository whose refs and commits are a real bare git repository's: a
// test reads what the code under test made on GitHub with git itself, and fetches it as a clone's
// origin. Its Handler answers the REST calls that read the repository and write its branches, a
// merge of one branch into another included, that compare two commits, and that open, list and
// read pull requests, as GitHub answers them, and leaves every other path to the test's own
// handler.
type GitRepo struct {
	t             *testing.T
	owner, name   string
	defaultBranch string
	dir           string
	// env is the environment of each git the fake runs.
	env []string
	// mu holds each of the fake's own operations whole, so two requests, or a request and a test's
	// Commit, never interleave their reads and writes of one ref or of pulls.
	mu sync.Mutex
	// pulls is each pull request the fake opened, the one numbered n at n-1.
	pulls []PullRequest
}

// GitRepository is owner/name on a fake GitHub: a bare repository under t.TempDir() whose HEAD names
// defaultBranch, which holds one commit. Its git runs under fixtureenv.Isolated, so it reads no
// configuration, repository or credential but the test's own.
func GitRepository(t *testing.T, owner, name, defaultBranch string) *GitRepo {
	t.Helper()
	root := t.TempDir()
	g := &GitRepo{
		t:             t,
		owner:         owner,
		name:          name,
		defaultBranch: defaultBranch,
		dir:           filepath.Join(root, name+".git"),
		env:           fixtureenv.Isolated(filepath.Join(root, "home")),
	}
	if out, err := g.command(root, "init", "--quiet", "--bare", "--initial-branch="+defaultBranch, g.dir).CombinedOutput(); err != nil {
		t.Fatalf("make the fake GitHub's repository %s/%s: %v: %s", owner, name, err, out)
	}
	g.Commit(t, defaultBranch, map[string]string{"README.md": "# " + name + "\n"}, "Initial commit")
	return g
}

// Dir is the bare repository, which a test clones, or fetches and pushes as its origin.
func (g *GitRepo) Dir() string {
	return g.dir
}

// PullRequest is a pull request the fake opened: its number, title and body, its base and head
// branches, its state, and the bearer token of the request that opened it, which a test reads to
// say which App opened it.
type PullRequest struct {
	Number                  int
	Title, Body, Base, Head string
	State                   string
	OpenedWith              string
}

// PullRequests is each pull request the fake opened, in the order it opened them.
func (g *GitRepo) PullRequests() []PullRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.pulls)
}

// Commit adds a commit on branch that writes each of files (path to content) over its parent's
// tree, and answers its id. The parent is branch's head, or the default branch's head where the
// repository has no branch of that name, which the commit then makes.
func (g *GitRepo) Commit(t *testing.T, branch string, files map[string]string, message string) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	old, err := g.head(branch)
	parent := old
	if err == nil && parent == "" {
		parent, err = g.head(g.defaultBranch)
	}
	if err != nil {
		t.Fatalf("read the head of %s: %v", branch, err)
	}
	// The tree is built in an index of the commit's own, since a bare repository has none.
	index := append(g.env[:len(g.env):len(g.env)], "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "index"))
	read := []string{"read-tree", "--empty"}
	if parent != "" {
		read = []string{"read-tree", parent}
	}
	if _, err := g.gitWith(index, "", read...); err != nil {
		t.Fatalf("read the parent's tree of a commit on %s: %v", branch, err)
	}
	for path, content := range files {
		blob, err := g.gitWith(index, content, "hash-object", "-w", "--stdin")
		if err == nil {
			_, err = g.gitWith(index, "", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
		}
		if err != nil {
			t.Fatalf("write %s for a commit on %s: %v", path, branch, err)
		}
	}
	tree, err := g.gitWith(index, "", "write-tree")
	if err != nil {
		t.Fatalf("write the tree of a commit on %s: %v", branch, err)
	}
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := g.git(args...)
	if err != nil {
		t.Fatalf("make a commit on %s: %v", branch, err)
	}
	if _, err := g.git("update-ref", "refs/heads/"+branch, commit, orZero(old)); err != nil {
		t.Fatalf("move %s to its new commit: %v", branch, err)
	}
	return commit
}

// Head is branch's head commit, or "" where the repository has no such branch.
func (g *GitRepo) Head(t *testing.T, branch string) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	head, err := g.head(branch)
	if err != nil {
		t.Fatalf("read the head of %s: %v", branch, err)
	}
	return head
}

// Handler answers r as GitHub's REST API answers it, where r is one of the calls the fake serves
// for its repository, and reports whether it did:
//
//   - GET /repos/{owner}/{name}: the repository's full_name and default_branch.
//   - GET /repos/{owner}/{name}/git/ref/heads/{branch}: the branch's head, or 404.
//   - POST /repos/{owner}/{name}/git/refs: the ref made at the commit it names (201), or 422
//     "Reference already exists" with the ref left where it was.
//   - POST /repos/{owner}/{name}/merges: 201 with the sha of a merge commit on base whose parents
//     are base's head and head's head, in that order; 204 with no body where base holds head
//     already; 409 "Merge conflict" with base left where it was; 404 for a base or head the
//     repository lacks.
//   - GET /repos/{owner}/{name}/compare/{base}...{head}, each a branch or a commit: the head's
//     commits base lacks (ahead_by, total_commits) and base's commits the head lacks (behind_by),
//     the status they make (ahead, behind, identical or diverged), the merge base and base's
//     commit, and the files the head changes since the merge base; 404 for a branch or commit the
//     repository lacks.
//   - POST /repos/{owner}/{name}/pulls: 201 with the pull request it opens, numbered from 1, on
//     head (a branch, or owner:branch) against base; 422 for a head that has an open pull request
//     ("A pull request already exists for owner:branch."), for a head with no commit base lacks
//     ("No commits between base and head") and for a branch the repository lacks.
//   - GET /repos/{owner}/{name}/pulls, filtered by head (owner:branch) and state (open where it
//     names none, or closed, or all), and GET /repos/{owner}/{name}/pulls/{number}: the pull
//     requests the fake opened, each naming its head branch's commit as it stands when it is read.
//
// Any other request is the caller's: Handler writes nothing for it and answers false, so a test's
// own handler goes on. A failure of the fake's own git fails the test and answers 500.
func (g *GitRepo) Handler(w http.ResponseWriter, r *http.Request) bool {
	prefix := "/repos/" + g.owner + "/" + g.name
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == prefix:
		g.answer(w, http.StatusOK, map[string]string{"full_name": g.owner + "/" + g.name, "default_branch": g.defaultBranch})
	case r.Method == http.MethodGet && strings.HasPrefix(path, prefix+"/git/ref/heads/"):
		g.readRef(w, strings.TrimPrefix(path, prefix+"/git/ref/heads/"))
	case r.Method == http.MethodPost && path == prefix+"/git/refs":
		g.createRef(w, r)
	case r.Method == http.MethodPost && path == prefix+"/merges":
		g.merge(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, prefix+"/compare/"):
		g.compare(w, strings.TrimPrefix(path, prefix+"/compare/"))
	case r.Method == http.MethodPost && path == prefix+"/pulls":
		g.openPull(w, r)
	case r.Method == http.MethodGet && path == prefix+"/pulls":
		g.listPulls(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, prefix+"/pulls/"):
		g.readPull(w, strings.TrimPrefix(path, prefix+"/pulls/"))
	default:
		return false
	}
	return true
}

// readRef answers the read of branch's ref.
func (g *GitRepo) readRef(w http.ResponseWriter, branch string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	head, err := g.head(branch)
	switch {
	case err != nil:
		g.failed(w, "read refs/heads/"+branch, err)
	case head == "":
		g.refuse(w, http.StatusNotFound, "Not Found")
	default:
		g.answer(w, http.StatusOK, refAnswer("refs/heads/"+branch, head))
	}
}

// createRef answers the create of the ref r's body names, at the commit it names.
func (g *GitRepo) createRef(w http.ResponseWriter, r *http.Request) {
	var create struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&create); err != nil || !strings.HasPrefix(create.Ref, "refs/") || create.SHA == "" {
		g.refuse(w, http.StatusUnprocessableEntity, "Invalid request.")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.git("rev-parse", "--verify", "--quiet", create.Ref); err == nil {
		g.refuse(w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	}
	if _, err := g.git("cat-file", "-e", create.SHA+"^{commit}"); err != nil {
		g.refuse(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	if _, err := g.git("update-ref", create.Ref, create.SHA, orZero("")); err != nil {
		g.failed(w, "create "+create.Ref, err)
		return
	}
	g.answer(w, http.StatusCreated, refAnswer(create.Ref, create.SHA))
}

// merge answers the merge of the head r's body names into its base, with its commit message.
func (g *GitRepo) merge(w http.ResponseWriter, r *http.Request) {
	var merge struct {
		Base          string `json:"base"`
		Head          string `json:"head"`
		CommitMessage string `json:"commit_message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&merge); err != nil || merge.Base == "" || merge.Head == "" {
		g.refuse(w, http.StatusUnprocessableEntity, "Invalid request.")
		return
	}
	message := merge.CommitMessage
	if message == "" {
		message = "Merge " + merge.Head + " into " + merge.Base
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// A push into Dir() takes no lock of the fake's, so base can move between its read and the
	// merge commit's move of it; the merge is then made again on base's new head, as GitHub makes
	// a merge on the head base has when it lands.
	for attempt := 1; ; attempt++ {
		base, err := g.head(merge.Base)
		if err != nil {
			g.failed(w, "read the merge's base "+merge.Base, err)
			return
		}
		if base == "" {
			g.refuse(w, http.StatusNotFound, "Base does not exist")
			return
		}
		head, err := g.head(merge.Head)
		if err != nil {
			g.failed(w, "read the merge's head "+merge.Head, err)
			return
		}
		if head == "" {
			g.refuse(w, http.StatusNotFound, "Head does not exist")
			return
		}
		switch holds, err := g.exitsZero("merge-base", "--is-ancestor", head, base); {
		case err != nil:
			g.failed(w, "read whether "+merge.Base+" holds "+merge.Head, err)
			return
		case holds:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		tree, err := g.git("merge-tree", "--write-tree", base, head)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			g.refuse(w, http.StatusConflict, "Merge conflict")
			return
		}
		if err != nil {
			g.failed(w, "merge "+merge.Head+" into "+merge.Base, err)
			return
		}
		commit, err := g.git("commit-tree", firstLine(tree), "-p", base, "-p", head, "-m", message)
		if err != nil {
			g.failed(w, "make the merge commit of "+merge.Head+" into "+merge.Base, err)
			return
		}
		if _, err := g.git("update-ref", "refs/heads/"+merge.Base, commit, base); err != nil {
			if now, read := g.head(merge.Base); read == nil && now != base && attempt < 5 {
				continue
			}
			g.failed(w, "move "+merge.Base+" to its merge commit", err)
			return
		}
		g.answer(w, http.StatusCreated, map[string]any{
			"sha":     commit,
			"parents": []map[string]string{{"sha": base}, {"sha": head}},
		})
		return
	}
}

// compare answers the compare of the two commits basehead names, base...head.
func (g *GitRepo) compare(w http.ResponseWriter, basehead string) {
	baseName, headName, ok := strings.Cut(basehead, "...")
	if !ok {
		g.refuse(w, http.StatusNotFound, "Not Found")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	base, err := g.commitOf(baseName)
	if err != nil {
		g.failed(w, "read the compare's base "+baseName, err)
		return
	}
	head, err := g.commitOf(headName)
	if err != nil {
		g.failed(w, "read the compare's head "+headName, err)
		return
	}
	if base == "" || head == "" {
		g.refuse(w, http.StatusNotFound, "Not Found")
		return
	}
	ahead, behind, mergeBase, err := g.distance(base, head)
	if err != nil {
		g.failed(w, "compare "+baseName+" with "+headName, err)
		return
	}
	files, err := g.changedFiles(mergeBase, head)
	if err != nil {
		g.failed(w, "list the files "+headName+" changes since "+baseName, err)
		return
	}
	status := "diverged"
	switch {
	case ahead == 0 && behind == 0:
		status = "identical"
	case behind == 0:
		status = "ahead"
	case ahead == 0:
		status = "behind"
	}
	g.answer(w, http.StatusOK, map[string]any{
		"status": status, "ahead_by": ahead, "behind_by": behind, "total_commits": ahead,
		"base_commit": map[string]string{"sha": base}, "merge_base_commit": map[string]string{"sha": mergeBase},
		"files": files,
	})
}

// distance is how many commits head has that base lacks, how many base has that head lacks, and
// their merge base.
func (g *GitRepo) distance(base, head string) (ahead, behind int, mergeBase string, err error) {
	if mergeBase, err = g.git("merge-base", base, head); err != nil {
		return 0, 0, "", err
	}
	for _, count := range []struct {
		into  *int
		since string
	}{{&ahead, base + ".." + head}, {&behind, head + ".." + base}} {
		out, err := g.git("rev-list", "--count", count.since)
		if err == nil {
			*count.into, err = strconv.Atoi(out)
		}
		if err != nil {
			return 0, 0, "", err
		}
	}
	return ahead, behind, mergeBase, nil
}

// changedFiles is each file head changes since from, as GitHub lists a comparison's files: its
// name, its name before a rename, and what the change did to it. It is empty, not nil, for none.
func (g *GitRepo) changedFiles(from, head string) ([]map[string]string, error) {
	out, err := g.git("diff", "--name-status", "-M", "-z", from, head)
	if err != nil {
		return nil, err
	}
	statuses := map[byte]string{'A': "added", 'D': "removed", 'M': "modified", 'R': "renamed", 'C': "copied", 'T': "changed"}
	files := []map[string]string{}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		kind := fields[i][0]
		file := map[string]string{"status": statuses[kind], "filename": fields[i+1]}
		if (kind == 'R' || kind == 'C') && i+2 < len(fields) {
			i++
			file["previous_filename"], file["filename"] = fields[i], fields[i+1]
		}
		files = append(files, file)
	}
	return files, nil
}

// openPull answers the open of the pull request r's body names.
func (g *GitRepo) openPull(w http.ResponseWriter, r *http.Request) {
	var open struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&open); err != nil || open.Head == "" || open.Base == "" {
		g.refuse(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	branch := open.Head
	if owner, named, forked := strings.Cut(open.Head, ":"); forked {
		if owner != g.owner {
			g.refuse(w, http.StatusUnprocessableEntity, "Validation Failed")
			return
		}
		branch = named
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	base, err := g.head(open.Base)
	if err != nil {
		g.failed(w, "read the pull request's base "+open.Base, err)
		return
	}
	head, err := g.head(branch)
	if err != nil {
		g.failed(w, "read the pull request's head "+branch, err)
		return
	}
	if base == "" || head == "" {
		g.refuse(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	if slices.ContainsFunc(g.pulls, func(p PullRequest) bool { return p.State == "open" && p.Head == branch }) {
		g.refuse(w, http.StatusUnprocessableEntity, fmt.Sprintf("Validation Failed: A pull request already exists for %s:%s.", g.owner, branch))
		return
	}
	ahead, _, _, err := g.distance(base, head)
	if err != nil {
		g.failed(w, "compare the pull request's base "+open.Base+" with "+branch, err)
		return
	}
	if ahead == 0 {
		g.refuse(w, http.StatusUnprocessableEntity, fmt.Sprintf("Validation Failed: No commits between %s and %s", open.Base, branch))
		return
	}
	pull := PullRequest{Number: len(g.pulls) + 1, Title: open.Title, Body: open.Body, Base: open.Base, Head: branch, State: "open",
		OpenedWith: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	g.pulls = append(g.pulls, pull)
	g.answer(w, http.StatusCreated, g.pullAnswer(pull, head))
}

// listPulls answers the list of the pull requests the fake opened that r's head and state name.
func (g *GitRepo) listPulls(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	listed := []map[string]any{}
	for _, pull := range g.pulls {
		if head := query.Get("head"); head != "" && head != g.owner+":"+pull.Head {
			continue
		}
		if state != "all" && state != pull.State {
			continue
		}
		sha, err := g.head(pull.Head)
		if err != nil {
			g.failed(w, "read the head of pull request "+strconv.Itoa(pull.Number), err)
			return
		}
		listed = append(listed, g.pullAnswer(pull, sha))
	}
	g.answer(w, http.StatusOK, listed)
}

// readPull answers the read of the pull request numbered number.
func (g *GitRepo) readPull(w http.ResponseWriter, number string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > len(g.pulls) {
		g.refuse(w, http.StatusNotFound, "Not Found")
		return
	}
	pull := g.pulls[n-1]
	sha, err := g.head(pull.Head)
	if err != nil {
		g.failed(w, "read the head of pull request "+number, err)
		return
	}
	g.answer(w, http.StatusOK, g.pullAnswer(pull, sha))
}

// pullAnswer is GitHub's body of pull, its head branch at sha.
func (g *GitRepo) pullAnswer(pull PullRequest, sha string) map[string]any {
	return map[string]any{
		"number": pull.Number, "state": pull.State, "title": pull.Title, "body": pull.Body,
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", g.owner, g.name, pull.Number),
		"base":     map[string]any{"ref": pull.Base},
		"head":     map[string]any{"ref": pull.Head, "sha": sha, "repo": map[string]string{"full_name": g.owner + "/" + g.name}},
		"merged":   false, "merged_at": nil,
	}
}

// commitOf is the commit name names, a branch first and else a commit id, or "" where the
// repository has neither.
func (g *GitRepo) commitOf(name string) (string, error) {
	commit, err := g.head(name)
	if err != nil || commit != "" {
		return commit, err
	}
	commit, err = g.git("rev-parse", "--verify", "--quiet", name+"^{commit}")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	return commit, err
}

// head is branch's head commit, or "" where the repository has no such branch.
func (g *GitRepo) head(branch string) (string, error) {
	head, err := g.git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	return head, err
}

// exitsZero runs git with args and answers whether it exited 0, where it exited 0 or 1.
func (g *GitRepo) exitsZero(args ...string) (bool, error) {
	_, err := g.git(args...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// git runs git with args in the repository and answers its standard output, trimmed. Its error is
// the *exec.ExitError of a run that exited other than 0, with what git printed on stderr.
func (g *GitRepo) git(args ...string) (string, error) {
	return g.gitWith(g.env, "", args...)
}

// gitWith is git under env, with stdin as its standard input.
func (g *GitRepo) gitWith(env []string, stdin string, args ...string) (string, error) {
	command := g.command(g.dir, args...)
	command.Env = env
	command.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// command is git with args in dir, under the fake's environment.
func (g *GitRepo) command(dir string, args ...string) *exec.Cmd {
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = g.env
	return command
}

// answer writes status and body, as JSON.
func (g *GitRepo) answer(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		g.failed(w, "encode an answer", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// refuse writes status with GitHub's body of a refusal, its message.
func (g *GitRepo) refuse(w http.ResponseWriter, status int, message string) {
	g.answer(w, status, map[string]string{"message": message})
}

// failed fails the test for a failure of the fake's own, what it was doing and err, and answers 500.
func (g *GitRepo) failed(w http.ResponseWriter, what string, err error) {
	g.t.Errorf("the fake GitHub could not %s: %v", what, err)
	w.WriteHeader(http.StatusInternalServerError)
}

// refAnswer is GitHub's body of a ref and the commit it names.
func refAnswer(ref, sha string) map[string]any {
	return map[string]any{"ref": ref, "object": map[string]string{"sha": sha, "type": "commit"}}
}

// orZero is old, the value a ref must hold for update-ref to move it, or git's id of no object
// where the ref must not exist yet.
func orZero(old string) string {
	if old == "" {
		return strings.Repeat("0", 40)
	}
	return old
}

// firstLine is the first line of out.
func firstLine(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	return line
}
