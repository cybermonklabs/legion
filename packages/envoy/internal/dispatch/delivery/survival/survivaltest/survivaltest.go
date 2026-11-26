// Package survivaltest builds a git repository with the real git for the tests of the code
// survival computation and of the run that feeds it, and reads back what those tests replay: each
// first-parent commit's patch as `git log --first-parent -p -U0 -M --format=%x01%H` writes it,
// with its parents and committer time, a commit's diff against its first parent in the shape
// GitHub's diff media type serves, and the tree a replay must reproduce.
//
// Every commit carries a committer and author date the test gives, so a history's days are the
// test's own. Git runs in the repository's directory, under an environment of its own: a home in
// the test's temporary directory, no system or global configuration (GIT_CONFIG_NOSYSTEM=1,
// GIT_CONFIG_GLOBAL=/dev/null) and no variable of the process's but PATH, so no GIT_DIR, git
// configuration or credential a pod hands its processes reaches it (docs/testing.md, "A test that
// starts jj or git inherits no pod's environment").
package survivaltest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Repo is one repository a test builds, on the branch main from its first commit.
type Repo struct {
	t   testing.TB
	dir string
	env []string
}

// Commit is one first-parent commit of a branch: its SHA, its parents (the first parent first,
// none for the root), its committer time, and its patch against its first parent (against the
// empty tree for the root) as git log writes it with -U0 and -M.
type Commit struct {
	SHA         string
	Parents     []string
	CommittedAt time.Time
	Patch       []byte
}

// Entry is one entry of a tree, as `git ls-tree -r` lists it: a blob, a symbolic link (a blob of
// mode 120000) or a submodule (an object of type commit, mode 160000).
type Entry struct {
	Mode   string
	Type   string
	Object string
	Path   string
}

// New initialises an empty repository on the branch main in a directory of the test's own.
func New(t testing.TB) *Repo {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "repo")
	for _, made := range []string{home, dir} {
		if err := os.MkdirAll(made, 0o755); err != nil {
			t.Fatalf("survivaltest: make %s: %v", made, err)
		}
	}
	repo := &Repo{t: t, dir: dir, env: []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=survivaltest",
		"GIT_AUTHOR_EMAIL=survivaltest@example.invalid",
		"GIT_COMMITTER_NAME=survivaltest",
		"GIT_COMMITTER_EMAIL=survivaltest@example.invalid",
		"LC_ALL=C",
		"TZ=UTC",
	}}
	repo.Git("init", "-q", "-b", "main")
	return repo
}

// Dir is the repository's working tree.
func (r *Repo) Dir() string { return r.dir }

// Git runs git with args in the repository and answers its standard output; it fails the test
// when git fails.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	return string(r.run(nil, args...))
}

// run runs git with args under the repository's environment and extra.
func (r *Repo) run(extra []string, args ...string) []byte {
	r.t.Helper()
	command := exec.Command("git", args...)
	command.Dir = r.dir
	command.Env = append(append([]string{"PATH=" + os.Getenv("PATH")}, r.env...), extra...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		r.t.Fatalf("survivaltest: git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return out
}

// Write writes text to path, a path of the working tree, making its directories.
func (r *Repo) Write(path, text string) {
	r.t.Helper()
	r.WriteBytes(path, []byte(text))
}

// WriteBytes writes data to path, as Write does; a test writes a binary file with it.
func (r *Repo) WriteBytes(path string, data []byte) {
	r.t.Helper()
	full := r.path(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("survivaltest: make the directory of %s: %v", path, err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		r.t.Fatalf("survivaltest: write %s: %v", path, err)
	}
}

// Symlink makes path a symbolic link to target, which git records as a blob of mode 120000
// holding target with no newline.
func (r *Repo) Symlink(path, target string) {
	r.t.Helper()
	full := r.path(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("survivaltest: make the directory of %s: %v", path, err)
	}
	if err := os.Symlink(target, full); err != nil {
		r.t.Fatalf("survivaltest: link %s to %s: %v", path, target, err)
	}
}

// Rename moves the file from to to with git mv, which a commit records as a rename when the
// contents stay alike.
func (r *Repo) Rename(from, to string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.path(to)), 0o755); err != nil {
		r.t.Fatalf("survivaltest: make the directory of %s: %v", to, err)
	}
	r.Git("mv", from, to)
}

// Delete removes path from the working tree.
func (r *Repo) Delete(path string) {
	r.t.Helper()
	if err := os.Remove(r.path(path)); err != nil {
		r.t.Fatalf("survivaltest: delete %s: %v", path, err)
	}
}

// Submodule stages a submodule entry (mode 160000) at path that names commit, with an empty
// directory in its place, which `git add -A` leaves as it is.
func (r *Repo) Submodule(path, commit string) {
	r.t.Helper()
	if err := os.MkdirAll(r.path(path), 0o755); err != nil {
		r.t.Fatalf("survivaltest: make %s: %v", path, err)
	}
	r.Git("update-index", "--add", "--cacheinfo", "160000,"+commit+","+path)
}

// Commit stages every change of the working tree and commits it on the current branch with
// message, committed and authored at at; it answers the commit's SHA. A commit with no change is
// allowed, and its patch is empty.
func (r *Repo) Commit(message string, at time.Time) string {
	r.t.Helper()
	r.Git("add", "-A")
	r.run(dates(at), "commit", "-q", "--allow-empty", "--no-verify", "-m", message)
	return r.Head()
}

// Branch makes the branch name at the current commit and checks it out.
func (r *Repo) Branch(name string) {
	r.t.Helper()
	r.Git("checkout", "-q", "-b", name)
}

// Checkout checks out the branch name.
func (r *Repo) Checkout(name string) {
	r.t.Helper()
	r.Git("checkout", "-q", name)
}

// Merge merges branch into the current branch with a merge commit (--no-ff) whose message is
// message, committed and authored at at; it answers the merge commit's SHA.
func (r *Repo) Merge(branch, message string, at time.Time) string {
	r.t.Helper()
	r.run(dates(at), "merge", "-q", "--no-ff", "--no-verify", "-m", message, branch)
	return r.Head()
}

// Head is the SHA of the current commit.
func (r *Repo) Head() string {
	r.t.Helper()
	return strings.TrimSpace(r.Git("rev-parse", "HEAD"))
}

// FirstParents is the first-parent chain that ends at tip, oldest first, each commit with its
// patch as `git log --first-parent -p -U0 -M --format=%x01%H` writes it (with --reverse for the
// chain's order; a merge's patch is against its first parent, the diff --first-parent implies).
func (r *Repo) FirstParents(tip string) []Commit {
	r.t.Helper()
	var chain []Commit
	for _, line := range strings.Split(strings.TrimSpace(r.Git("log", "--reverse", "--first-parent",
		"--format=%H %ct %P", tip)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			r.t.Fatalf("survivaltest: git log line %q has no SHA and time", line)
		}
		seconds, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			r.t.Fatalf("survivaltest: git log line %q: %v", line, err)
		}
		chain = append(chain, Commit{SHA: fields[0], Parents: fields[2:],
			CommittedAt: time.Unix(seconds, 0).UTC()})
	}
	patches := bytes.Split(r.run(nil, "log", "--reverse", "--first-parent", "-p", "-U0", "-M",
		"--no-color", "--format=%x01%H", tip), []byte{1})
	if len(patches) != len(chain)+1 || len(bytes.TrimSpace(patches[0])) != 0 {
		r.t.Fatalf("survivaltest: git log -p wrote %d commits where the chain holds %d",
			len(patches)-1, len(chain))
	}
	for index, patch := range patches[1:] {
		sha, body, _ := bytes.Cut(patch, []byte{'\n'})
		if string(sha) != chain[index].SHA {
			r.t.Fatalf("survivaltest: git log -p wrote %s where the chain holds %s", sha,
				chain[index].SHA)
		}
		chain[index].Patch = body
	}
	return chain
}

// Diff is the diff of commit against its first parent, or against the empty tree for a root, in
// the shape GitHub's diff media type serves: three lines of context and renames found (-M).
func (r *Repo) Diff(commit string) []byte {
	r.t.Helper()
	parents := strings.Fields(r.Git("log", "-1", "--format=%P", commit))
	base := strings.TrimSpace(r.Git("hash-object", "-t", "tree", os.DevNull))
	if len(parents) > 0 {
		base = parents[0]
	}
	return r.run(nil, "diff", "--no-color", "-M", base, commit)
}

// Tree is every entry of commit's tree, recursively, in git's order.
func (r *Repo) Tree(commit string) []Entry {
	r.t.Helper()
	var entries []Entry
	for _, record := range strings.Split(r.Git("ls-tree", "-r", "-z", commit), "\x00") {
		if record == "" {
			continue
		}
		meta, path, found := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !found || len(fields) != 3 {
			r.t.Fatalf("survivaltest: git ls-tree wrote %q", record)
		}
		entries = append(entries, Entry{Mode: fields[0], Type: fields[1], Object: fields[2], Path: path})
	}
	return entries
}

// Blob is the contents of the blob object.
func (r *Repo) Blob(object string) []byte {
	r.t.Helper()
	return r.run(nil, "cat-file", "blob", object)
}

// path is the working-tree path of a repository-relative path.
func (r *Repo) path(path string) string { return filepath.Join(r.dir, filepath.FromSlash(path)) }

// dates is the environment that dates a commit at at, for both its committer and its author.
func dates(at time.Time) []string {
	stamp := at.UTC().Format(time.RFC3339)
	return []string{"GIT_COMMITTER_DATE=" + stamp, "GIT_AUTHOR_DATE=" + stamp}
}
