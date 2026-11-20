package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// baseLine starts the first line of the list that changed prints and citations --changed reads,
// "base <object name>": the commit that the paths of the list changed since.
const baseLine = "base "

// objectName is whether s is a full git object name: 40 lower-case hex digits, or 64 in a
// repository of SHA-256. citations names commits so in its own files, since a short or a symbolic
// name can name another commit tomorrow.
func objectName(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}

// gitCommand is `git -C <repo> --no-replace-objects <args>`, run with no variable GIT_* of the
// process, as specgraph.GitHistory runs git: a replace ref would put another commit, and its tree,
// in the place of one, and a variable such as GIT_DIR or GIT_OBJECT_DIRECTORY would make git read
// another clone than repo (TestTheGitCallsOfTheBaseAreTheSameUnderEachSetting). It takes the
// environment through exec.Cmd.Environ, as GoCode does.
func gitCommand(repo string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", repo, "--no-replace-objects"}, args...)...)
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
	return cmd
}

// oneLine is what git wrote to its standard error, its white space runs as one space each, so a
// line of the command that names it stays one line.
func oneLine(stderr *bytes.Buffer) string {
	return strings.Join(strings.Fields(stderr.String()), " ")
}

// commitName is the full object name of the commit that rev names in the git clone at repo, and
// whether git could read one. --end-of-options makes git read a rev that starts with "-" as a
// revision, never as an option, as GitHistory does.
func commitName(repo, rev string) (string, bool) {
	name, err := gitCommand(repo, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}").Output()
	return strings.TrimSpace(string(name)), err == nil
}

// isCloneTop is whether repo is the top of its git clone, the one directory from which changed
// names each path and citations reads the ties and the code; when it is not, it has written the
// one line that names the top that git rev-parse --show-toplevel gives, to pass instead, or why
// git gives none. A directory below the top would make the two commands disagree, since git names
// each changed path from the top whatever directory the command names. The top named through a
// link is the top, since the two are one directory.
func isCloneTop(stderr io.Writer, repo string) bool {
	var gitErr bytes.Buffer
	show := gitCommand(repo, "rev-parse", "--show-toplevel")
	show.Stderr = &gitErr
	out, err := show.Output()
	if err != nil {
		say(stderr, "%s: git finds no work tree there: %s", repo, oneLine(&gitErr))
		return false
	}
	top := strings.TrimSuffix(string(out), "\n")
	repoInfo, repoErr := os.Stat(repo)
	topInfo, topErr := os.Stat(top)
	if repoErr != nil || topErr != nil || !os.SameFile(repoInfo, topInfo) {
		say(stderr, "%s: is not the top of its git clone; pass the top, --repo %s", repo, top)
		return false
	}
	return true
}

// readTiesFiles reads the files of --ties, each in the form of specgraph.ReadCommitTies, as one
// set of ties by commit. It refuses a commit that is no full object name, and a commit that two of
// the files hold, since the command could not say which file's ties are the commit's; of a file's
// commits it judges each in sorted order, so a refusal names the same commit on each run.
func readTiesFiles(paths []string) (specgraph.CommitTies, error) {
	all := specgraph.CommitTies{}
	from := map[string]string{}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot be read: %w", path, err)
		}
		ties, err := specgraph.ReadCommitTies(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, commit := range slices.Sorted(maps.Keys(ties)) {
			switch {
			case !objectName(commit):
				return nil, fmt.Errorf("%s: commit %q: not a full object name of 40 or 64 lower-case hex digits", path, commit)
			case from[commit] != "":
				return nil, fmt.Errorf("%s: commit %s: %s holds its ties too", path, commit, from[commit])
			}
			all[commit], from[commit] = ties[commit], path
		}
	}
	return all, nil
}

// fileTie is the tie of check at commit in the files of --ties, Known false when they hold none.
func fileTie(files specgraph.CommitTies, commit, check string) specgraph.Tie {
	checks, held := files[commit]
	if !held {
		return specgraph.Tie{Why: fmt.Sprintf("--ties holds no ties of commit %s", commit)}
	}
	tie, held := checks[check]
	if !held {
		return specgraph.Tie{Why: fmt.Sprintf("--ties holds no tie of the check at commit %s", commit)}
	}
	return tie
}

// derivedTies is the tie of each check that the .github/scripts/go-test-reads.sh of a repository
// derives (specgraph.GoTies), one check at a time on the first ask, each set of the script's
// arguments asked once. It memoizes across the per-check asks of specgraph.Citations, which GoTies
// cannot, since each ask is a new call of GoTies. specgraph.Citations asks for a check's tie only
// when its test did not run, so a run where each test ran runs no script.
type derivedTies struct {
	reads   specgraph.TestReads
	answers map[string]readsAnswer
}

// readsAnswer is one answer of go-test-reads.sh.
type readsAnswer struct {
	reads []string
	err   error
}

// newDerivedTies is the derived ties of the repository at root, whose go-test-reads.sh runs in
// root, where the root go.work takes the import paths of the repository's modules, with environ
// as its whole environment, nil for the process's own (specgraph.ScriptTestReads).
func newDerivedTies(root string, environ []string) *derivedTies {
	return &derivedTies{reads: specgraph.ScriptTestReads(root, environ), answers: map[string]readsAnswer{}}
}

// tie is the tie of c, Known false with the cause when it cannot be derived.
func (d *derivedTies) tie(c specgraph.Check) specgraph.Tie {
	return specgraph.GoTies([]specgraph.Check{c}, ".", nil, d.once)[c.ID]
}

// once is the script's answer for its arguments, asked the first time only.
func (d *derivedTies) once(workdir string, pkgs, env []string) ([]string, error) {
	key := strings.Join(append(append([]string{workdir}, pkgs...), env...), "\x00")
	answer, asked := d.answers[key]
	if !asked {
		answer.reads, answer.err = d.reads(workdir, pkgs, env)
		d.answers[key] = answer
	}
	return answer.reads, answer.err
}

// baseCheckout is the tie of each check at the base commit of --changed, derived as the ties at the
// head are, by the base's own .github/scripts/go-test-reads.sh, in a checkout of the base commit
// that the first ask makes and close removes. A file that the change deleted or moved out of a tie
// is in no tie at the head, so a result carries from the base only while no file of the base's
// tie changed either (specgraph.Citations), which asks only for a check whose test did not run and
// whose tie at the head no changed path reaches: a run where each test ran makes no checkout, and
// citations makes none for a base that is the head, whose tie is the one at the head.
//
// The script lists the repository's files with git ls-files (ci_filter_coverage.py's repo_files,
// which ci_job_reads.py's tables read too), and the checkout is no git work tree. So the script
// runs with git's view of the base (gitView): GIT_DIR the git directory of the clone, GIT_WORK_TREE
// the checkout's tree and GIT_INDEX_FILE the checkout's index of the base's tree, and no other
// variable GIT_* of the process. Its git ls-files then lists the base's files, and no file of the
// clone's own index or work tree (TestTheBaseTieReadsGitsListOfTheBase).
type baseCheckout struct {
	repo, commit string
	dir          string // the checkout's directory, once made
	err          error  // why the checkout could not be made
	ties         *derivedTies
}

// tie is the tie of c at the base commit, Known false with the cause when it cannot be derived.
func (b *baseCheckout) tie(c specgraph.Check) specgraph.Tie {
	if b.dir == "" && b.err == nil {
		if b.dir, b.err = checkout(b.repo, b.commit); b.err == nil {
			var environ []string
			if environ, b.err = gitView(b.repo, b.dir); b.err == nil {
				b.ties = newDerivedTies(filepath.Join(b.dir, "tree"), environ)
			}
		}
	}
	if b.err != nil {
		return specgraph.Tie{Why: b.err.Error()}
	}
	return b.ties.tie(c)
}

// gitView is the environment of the base's go-test-reads.sh in the checkout at dir: the process's
// own with no variable GIT_*, which would make git read another clone or another index, as in
// gitCommand, then GIT_DIR the git directory of the clone at repo, GIT_WORK_TREE <dir>/tree and
// GIT_INDEX_FILE <dir>/index, which checkout filled with the base's tree.
func gitView(repo, dir string) ([]string, error) {
	var stderr bytes.Buffer
	show := gitCommand(repo, "rev-parse", "--absolute-git-dir")
	show.Stderr = &stderr
	gitDir, err := show.Output()
	if err != nil {
		return nil, fmt.Errorf("no git directory of %s for the checkout of the base: %w: %s", repo, err, oneLine(&stderr))
	}
	environ := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
	return append(environ, "GIT_DIR="+strings.TrimSuffix(string(gitDir), "\n"), "GIT_WORK_TREE="+filepath.Join(dir, "tree"),
		"GIT_INDEX_FILE="+filepath.Join(dir, "index")), nil
}

// close removes the checkout of the base commit.
func (b *baseCheckout) close() {
	if b.dir != "" {
		os.RemoveAll(b.dir)
	}
}

// checkout writes the tree of commit of the clone at repo to <dir>/tree of a new directory, and
// its index to <dir>/index, and changes nothing of the clone. It writes the tree as git stores it,
// whatever the settings of the clone, its .gitattributes or the process say
// (TestTheGitCallsOfTheBaseAreTheSameUnderEachSetting), since go-test-reads.sh derives the tie at
// the base from those files: each path of the tree, from the top of the clone, whatever directory
// of it repo names and whatever sparse checkout; each blob's own bytes, through no filter,
// end-of-line conversion or ident, in a file that is executable for the mode 100755; a link as a
// link to its blob's text, whatever core.symlinks; and a gitlink as an empty directory, as git
// checks out a submodule that it did not clone. So it reads the tree with git ls-tree and the blobs
// with git cat-file, which give the objects as stored, and writes each file itself. The index is
// what git read-tree writes for commit, a full index whatever index.sparse says, so a git ls-files
// of it lists each path of the tree (gitView).
func checkout(repo, commit string) (string, error) {
	dir, err := os.MkdirTemp("", "spec-check-base-")
	if err != nil {
		return "", fmt.Errorf("no checkout of the base commit %s: %w", commit, err)
	}
	if err := writeTree(repo, commit, filepath.Join(dir, "tree")); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("no checkout of the base commit %s: %w", commit, err)
	}
	var stderr bytes.Buffer
	read := gitCommand(repo, "-c", "index.sparse=false", "read-tree", commit)
	read.Env = append(read.Env, "GIT_INDEX_FILE="+filepath.Join(dir, "index"))
	read.Stderr = &stderr
	if err := read.Run(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("no checkout of the base commit %s: git read-tree: %w: %s", commit, err, oneLine(&stderr))
	}
	return dir, nil
}

// treeEntry is one entry of `git ls-tree -r`: its mode, its object and its path from the top.
type treeEntry struct{ mode, object, path string }

// maxLinkTarget is the most bytes that a link's blob may hold, PATH_MAX of Linux.
const maxLinkTarget = 4096

// writeTree writes the tree of commit of the clone at repo under root, which it makes. It writes
// the files and the directories first, the parent of each link included, and the links last, and
// never replaces a path that it wrote: so a tree whose path runs through one of its links, which
// git writes no tree of, is an error, and nothing is written outside root. A path that git would
// not check out, with an empty part, ".", ".." or ".git", is an error too.
func writeTree(repo, commit, root string) error {
	var stderr bytes.Buffer
	list := gitCommand(repo, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", commit)
	list.Stderr = &stderr
	out, err := list.Output()
	if err != nil {
		return fmt.Errorf("git ls-tree: %w: %s", err, oneLine(&stderr))
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		return err
	}
	var blobs []treeEntry
	for record := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if record == "" {
			continue
		}
		meta, path, _ := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return fmt.Errorf("git ls-tree gave the entry %q, which is not <mode> <type> <object>\\t<path>", record)
		}
		entry := treeEntry{mode: fields[0], object: fields[2], path: path}
		if fault := treePathFault(path); fault != "" {
			return fmt.Errorf("the tree holds the path %q, %s", path, fault)
		}
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		switch {
		case entry.mode == "160000" && fields[1] == "commit":
			if err := os.Mkdir(full, 0o755); err != nil {
				return err
			}
		case (entry.mode == "100644" || entry.mode == "100755" || entry.mode == "120000") && fields[1] == "blob":
			blobs = append(blobs, entry)
		default:
			return fmt.Errorf("the tree holds %s, a %s of mode %s, which no checkout writes", path, fields[1], entry.mode)
		}
	}
	links, err := writeBlobs(repo, root, blobs)
	if err != nil {
		return err
	}
	for _, link := range links {
		if err := os.Symlink(link.target, filepath.Join(root, filepath.FromSlash(link.path))); err != nil {
			return err
		}
	}
	return nil
}

// treePathFault is why git would not check out the tree path p, or "": a part that is empty, ".",
// ".." or ".git" in any case of its letters.
func treePathFault(p string) string {
	for part := range strings.SplitSeq(p, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Sprintf("whose part %q git checks out nowhere", part)
		}
	}
	return ""
}

// treeLink is a link of the tree: its path and the text of its blob.
type treeLink struct{ path, target string }

// writeBlobs reads the blobs with one git cat-file --batch, in their order, writes each file under
// root, and answers each link, in the order of the tree, for the caller to make last.
func writeBlobs(repo, root string, blobs []treeEntry) (links []treeLink, err error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	var objects strings.Builder
	for _, blob := range blobs {
		objects.WriteString(blob.object + "\n")
	}
	var stderr bytes.Buffer
	cat := gitCommand(repo, "cat-file", "--batch")
	cat.Stdin = strings.NewReader(objects.String())
	cat.Stderr = &stderr
	stdout, err := cat.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cat.Start(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cat.Process.Kill()
			cat.Wait()
			return
		}
		if waitErr := cat.Wait(); waitErr != nil {
			err = fmt.Errorf("git cat-file --batch: %w: %s", waitErr, oneLine(&stderr))
		}
	}()
	r := bufio.NewReader(stdout)
	for _, blob := range blobs {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file --batch gave no object %s: %w", blob.object, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != blob.object || fields[1] != "blob" {
			return nil, fmt.Errorf("git cat-file --batch gave %q for the blob %s of %s", strings.TrimSpace(header), blob.object, blob.path)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("git cat-file --batch gave %q for the blob %s of %s", strings.TrimSpace(header), blob.object, blob.path)
		}
		full := filepath.Join(root, filepath.FromSlash(blob.path))
		if blob.mode == "120000" {
			if size > maxLinkTarget {
				return nil, fmt.Errorf("the link %s holds %d bytes, more than a link can name (%d)", blob.path, size, maxLinkTarget)
			}
			target := make([]byte, size)
			if _, err := io.ReadFull(r, target); err != nil {
				return nil, err
			}
			links = append(links, treeLink{path: blob.path, target: string(target)})
		} else if err := writeBlob(full, blob.mode == "100755", r, size); err != nil {
			return nil, err
		}
		if b, err := r.ReadByte(); err != nil || b != '\n' {
			return nil, fmt.Errorf("git cat-file --batch gave no line feed after the blob %s of %s", blob.object, blob.path)
		}
	}
	return links, nil
}

// writeBlob writes size bytes of r to a new file at path, executable when executable is true.
func writeBlob(path string, executable bool, r io.Reader, size int64) error {
	perm := os.FileMode(0o644)
	if executable {
		perm = 0o755
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
