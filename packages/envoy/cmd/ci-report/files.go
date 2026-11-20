package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// filesAt is the set of spec files in dir at commit, read through git from the commit's tree and
// never from the working tree, so a step before the last one that changes the spec files and
// commits nothing changes no check (R12: "the check nodes of the files at its commit"). It reads
// the tree as Dispatch's results route reads the files of a run's commit from GitHub's listing
// (githubapp.Client.Dir and DirFiles): an entry is a spec file when notes.IsNote keeps it, read
// with notes.GitKind from its type and mode, and each other entry is in Rejected; the set is
// checked with notes.CheckSet (notes.MaxFiles files, notes.MaxSetSize bytes in sum) and every
// spec file with notes.CheckSize, both from the listed sizes before any file is read, and both
// again from the files read, in notes.ReadSet's order. Its answers are notes.ReadSet's:
// notes.ErrNoDirectory for a path that the commit does not hold, a *notes.SizeRefusal for a set or
// a file over a bound, and an
// error for any other case, in the reporter's words for a path outside the checkout and for one
// that the commit holds as a file, a symbolic link or a submodule (treeAt). dir is absolute, and
// it is read from the working directory, the checkout.
func filesAt(dir, commit string) (notes.Set, error) {
	wd, err := os.Getwd()
	if err != nil {
		return notes.Set{}, fmt.Errorf("cannot be read: %w", err)
	}
	rel, err := filepath.Rel(wd, dir)
	if err != nil {
		return notes.Set{}, fmt.Errorf("cannot be read: %w", err)
	}
	// The path from the checkout's root: the working directory's own path (git's prefix), then
	// dir from the working directory. A path that leaves the root is not a path of the commit.
	prefix, err := runGit(nil, "rev-parse", "--show-prefix")
	if err != nil {
		return notes.Set{}, fmt.Errorf("cannot be read: %w", err)
	}
	at := path.Clean(strings.TrimSuffix(string(prefix), "\n") + filepath.ToSlash(rel))
	if at == ".." || strings.HasPrefix(at, "../") {
		return notes.Set{}, errors.New("is outside the checkout, so its files at the commit cannot be read")
	}
	tree, err := treeAt(commit, at)
	if err != nil {
		return notes.Set{}, err
	}
	// --full-tree: ls-tree otherwise lists only the entries under the working directory's own
	// path, which a tree of a directory below the checkout's root does not hold.
	listing, err := runGit(nil, "ls-tree", "--full-tree", "-z", "-l", tree)
	if err != nil {
		return notes.Set{}, fmt.Errorf("cannot be read: %w", err)
	}

	var set notes.Set
	var names, blobs []string
	var refusals []error
	var listed int64
	for _, entry := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		// <mode> SP <type> SP <object> SP <size, padded> TAB <name>, the size "-" for an entry
		// that is not a blob.
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return set, fmt.Errorf("cannot be read: git ls-tree printed the entry %q", entry)
		}
		path := filepath.Join(dir, name)
		if kind := notes.GitKind(fields[1], fields[0]); !notes.IsNote(name, kind) {
			set.Rejected = append(set.Rejected, notes.Rejected{Path: path, Kind: kind})
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return set, fmt.Errorf("cannot be read: git ls-tree printed the size %q of %s", fields[3], path)
		}
		if err := notes.CheckSize(path, size); err != nil {
			refusals = append(refusals, err)
		}
		names, blobs, listed = append(names, name), append(blobs, fields[2]), listed+size
	}
	// The set's bounds first, then each file's, as notes.ReadSet orders them, and both from the
	// listing, so a set over a bound is refused before any file is read.
	if err := notes.CheckSet(dir, len(names), listed); err != nil {
		return set, &notes.SizeRefusal{Held: len(names), Problems: []error{err}}
	}
	if len(refusals) > 0 {
		return set, &notes.SizeRefusal{Held: len(names), Problems: refusals}
	}

	contents, err := readBlobs(blobs)
	if err != nil {
		return set, fmt.Errorf("cannot be read: %w", err)
	}
	files := make(map[string][]byte, len(names))
	var read int64
	for i, name := range names {
		if err := notes.CheckSize(filepath.Join(dir, name), int64(len(contents[i]))); err != nil {
			refusals = append(refusals, err)
		}
		files[name], read = contents[i], read+int64(len(contents[i]))
	}
	if err := notes.CheckSet(dir, len(names), read); err != nil {
		return set, &notes.SizeRefusal{Held: len(names), Problems: []error{err}}
	}
	if len(refusals) > 0 {
		return set, &notes.SizeRefusal{Held: len(names), Problems: refusals}
	}
	set.Files = files
	return set, nil
}

// treeAt is the tree object of the directory at, a path from the checkout's root, at commit. It
// reads the path's own entry of the commit's tree (`git ls-tree`, its pathspec read literally), so
// a path that the commit holds as a file, a symbolic link or a submodule is refused with the
// entry's kind, as notes.GitKind names it, and never read; a path that the commit does not hold
// is notes.ErrNoDirectory.
func treeAt(commit, at string) (string, error) {
	if at == "." {
		tree, err := runGit(nil, "rev-parse", "--verify", commit+"^{tree}")
		if err != nil {
			return "", fmt.Errorf("cannot be read: %w", err)
		}
		return strings.TrimSpace(string(tree)), nil
	}
	entry, err := runGit(nil, "--literal-pathspecs", "ls-tree", "--full-tree", "-z", commit, "--", at)
	if err != nil {
		return "", fmt.Errorf("cannot be read: %w", err)
	}
	if len(entry) == 0 {
		return "", notes.ErrNoDirectory
	}
	// <mode> SP <type> SP <object> TAB <name> NUL, one entry: the path's own.
	meta, name, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
	fields := strings.Fields(meta)
	if !ok || len(fields) != 3 || name != at {
		return "", fmt.Errorf("cannot be read: git ls-tree printed the entry %q for %s", entry, at)
	}
	if kind := notes.GitKind(fields[1], fields[0]); kind != notes.KindDirectory {
		return "", fmt.Errorf("is a %s at %s, not a directory", kind, commit)
	}
	return fields[2], nil
}

// readBlobs is the content of each blob of shas, in order, read with one `git cat-file --batch`,
// each a part of the one buffer git wrote.
func readBlobs(shas []string) ([][]byte, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	out, err := runGit(strings.NewReader(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	contents := make([][]byte, 0, len(shas))
	for _, sha := range shas {
		// <object> SP <type> SP <size> LF <content> LF
		header, rest, ok := bytes.Cut(out, []byte("\n"))
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 || fields[0] != sha || fields[1] != "blob" {
			return nil, fmt.Errorf("git cat-file --batch answered %q for the blob %s", header, sha)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || size >= len(rest) || rest[size] != '\n' {
			return nil, fmt.Errorf("git cat-file --batch answered a blob %s of size %q and %d bytes", sha, fields[2], len(rest))
		}
		contents = append(contents, rest[:size:size])
		out = rest[size+1:]
	}
	return contents, nil
}

// runGit runs git with args in the working directory, with stdin on its standard input when it is
// not nil, and answers what it wrote on standard output, or an error that names the command and
// holds what git wrote on standard error.
func runGit(stdin io.Reader, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Stdin, cmd.Stderr = stdin, &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
