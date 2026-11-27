package specgraph

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/programfile"
)

// CommitName is the full object name of the commit that rev names in the clone at Dir, and
// whether git could read one. command's --no-replace-objects keeps a replace ref from naming
// another commit in its place, and --end-of-options makes git read a rev that starts with "-" as a
// revision, never as an option, as Changed does.
func (g GitHistory) CommitName(rev string) (string, bool) {
	if g.dirFault() != nil {
		return "", false
	}
	name, err := g.git("", "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	return strings.TrimSpace(string(name)), err == nil
}

// Top is the top of the work tree of the clone at Dir, as git rev-parse --show-toplevel names it,
// or an error that holds git's reason (gitReason), as for a directory where git finds no work
// tree, such as the clone's .git.
func (g GitHistory) Top() (string, error) {
	if err := g.dirFault(); err != nil {
		return "", err
	}
	out, err := g.git("", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errors.New(gitReason(err))
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

// Checkout is one commit of a clone written out by GitHistory.Checkout: Dir holds the commit's
// tree in Dir/tree (Tree) and its index in Dir/index, and Environ is git's view of the tree, the
// whole environment in which git reads Tree as a work tree of the clone at that commit.
type Checkout struct {
	Dir     string
	Environ []string
}

// Tree is the directory of the checkout's tree.
func (c Checkout) Tree() string {
	return filepath.Join(c.Dir, "tree")
}

// Remove removes the checkout's directory.
func (c Checkout) Remove() error {
	return os.RemoveAll(c.Dir)
}

// Checkout writes the tree of commit of the clone at Dir to <dir>/tree of a new directory, and its
// index to <dir>/index, and changes nothing of the clone. It writes the tree as git stores it,
// whatever the settings of the clone, its .gitattributes or the process say
// (TestTheGitCallsOfACheckoutAreTheSameUnderEachSetting), since a reader of the checkout, such as
// spec-check's go-test-reads.sh, derives what the commit holds from those files: each path of the
// tree, from the top of the clone, whatever directory of it Dir names and whatever sparse
// checkout; each blob's own bytes, through no filter, end-of-line conversion or ident, in a file
// that is executable for the mode 100755; a link as a link to its blob's text, whatever
// core.symlinks; and a gitlink as an empty directory, as git checks out a submodule that it did
// not clone. So it reads the tree with git ls-tree and the blobs with git cat-file, which give the
// objects as stored, and writes each file itself. The index is what git read-tree writes for
// commit, a full index whatever index.sparse says, so a git ls-files of it lists each path of the
// tree.
//
// The checkout is no git work tree, so Environ gives git its view of one: the environment of this
// package's git command (command), which is the process's own with no variable GIT_*, since such a
// variable would make git read another clone or another index; then GIT_DIR the git directory of
// the clone, GIT_WORK_TREE the checkout's tree and GIT_INDEX_FILE its index. A git ls-files run
// with Environ lists the commit's files, and no file of the clone's own index or work tree.
func (g GitHistory) Checkout(commit string) (Checkout, error) {
	if err := g.dirFault(); err != nil {
		return Checkout{}, err
	}
	fault := func(err error) (Checkout, error) {
		return Checkout{}, fmt.Errorf("specgraph: no checkout of commit %s of the clone at %s: %w", quoted(commit), quoted(g.Dir), err)
	}
	gitDir, err := g.git("", "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fault(fmt.Errorf("git rev-parse --absolute-git-dir: %s", gitReason(err)))
	}
	dir, err := os.MkdirTemp("", "specgraph-checkout-")
	if err != nil {
		return fault(err)
	}
	c := Checkout{Dir: dir, Environ: append(g.command().Env, "GIT_DIR="+strings.TrimSuffix(string(gitDir), "\n"),
		"GIT_WORK_TREE="+filepath.Join(dir, "tree"), "GIT_INDEX_FILE="+filepath.Join(dir, "index"))}
	if err := g.writeTree(commit, c.Tree()); err != nil {
		c.Remove()
		return fault(err)
	}
	read := g.command("-c", "index.sparse=false", "read-tree", "--end-of-options", commit)
	read.Env = append(read.Env, "GIT_INDEX_FILE="+filepath.Join(dir, "index"))
	if _, err := read.Output(); err != nil {
		c.Remove()
		return fault(fmt.Errorf("git read-tree: %s", gitReason(err)))
	}
	return c, nil
}

// treeEntry is one entry of `git ls-tree -r`: its mode, its object and its path from the top.
type treeEntry struct{ mode, object, path string }

// maxLinkTarget is the most bytes that a link's blob may hold, PATH_MAX of Linux.
const maxLinkTarget = 4096

// writeTree writes the tree of commit of the clone at Dir under root, which it makes. It writes
// the files and the directories first, the parent of each link included, and the links last, and
// never replaces a path that it wrote: so a tree whose path runs through one of its links, which
// git writes no tree of, is an error, and nothing is written outside root. A path that git would
// not check out (checkoutFault) is an error too.
func (g GitHistory) writeTree(commit, root string) error {
	out, err := g.git("", "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", commit)
	if err != nil {
		return fmt.Errorf("git ls-tree: %s", gitReason(err))
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
		if fault := checkoutFault(path); fault != "" {
			return fmt.Errorf("the tree holds the path %s, %s", quoted(path), fault)
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
	links, err := g.writeBlobs(root, blobs)
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

// checkoutFault is why git would not check out the tree path p, or "": p is no repository path
// (isRepoPath), or a part of it is ".git" in any case of its letters, the name of the git
// directory, which git checks out nowhere.
func checkoutFault(p string) string {
	if !isRepoPath(p) {
		return `which is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`
	}
	for part := range strings.SplitSeq(p, "/") {
		if strings.EqualFold(part, ".git") {
			return fmt.Sprintf("whose part %s names the git directory, which git checks out nowhere", quoted(part))
		}
	}
	return ""
}

// treeLink is a link of the tree: its path and the text of its blob.
type treeLink struct{ path, target string }

// writeBlobs reads the blobs with one git cat-file --batch, in their order, writes each file under
// root, and answers each link, in the order of the tree, for the caller to make last.
func (g GitHistory) writeBlobs(root string, blobs []treeEntry) (links []treeLink, err error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	var objects strings.Builder
	for _, blob := range blobs {
		objects.WriteString(blob.object + "\n")
	}
	var stderr strings.Builder
	cat := g.command("cat-file", "--batch")
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
			// Output fills the ExitError's Stderr, which gitReason reads; a command run with its
			// own Stderr leaves it empty.
			if exit := (*exec.ExitError)(nil); errors.As(waitErr, &exit) {
				exit.Stderr = []byte(stderr.String())
			}
			err = fmt.Errorf("git cat-file --batch: %s", gitReason(waitErr))
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
	if !executable {
		// No program runs this file, so no exec of it can meet ETXTBSY: it streams from the pipe,
		// outside the fork lock, and costs no more memory than the copy's buffer.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(f, r, size); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	// An executable blob may run, and another goroutine of this process forks to start commands,
	// so a plain os.OpenFile here could be held open for writing in a child when the file runs
	// (ETXTBSY): programfile.Write writes it under the fork lock. The blob comes from a git
	// cat-file --batch subprocess pipe, so it is read into memory first, and the pipe read does not
	// happen while the lock is held.
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return programfile.Write(path, os.O_EXCL, 0o755, bytes.NewReader(data))
}
