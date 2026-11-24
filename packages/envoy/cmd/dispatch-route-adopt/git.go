package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// apiDir is package api, from the repository's root.
const apiDir = "packages/envoy/internal/dispatch/api"

// gitRepo is one checkout: a git one, or a jj one, colocated or not. Every read of a commit goes
// through git, which in a jj checkout is jj's own store (`jj git root`), since jj keeps each of
// its commits as a git commit there; what differs is how the merge in progress is found, how its
// conflicts are listed and whether a written file is staged (vcs.go).
type gitRepo struct {
	root string
	// gitDir is the git store jj keeps, set in a jj checkout and empty in a git one, where git
	// finds its own from root.
	gitDir string
	// jj is a checkout jj answers for (`jj root`), which reads no git index: jj takes every file
	// as the working copy holds it when its next command runs.
	jj bool
}

// command is git with args, run at the checkout's root and in jj's store where there is one.
func (g gitRepo) command(args ...string) *exec.Cmd {
	if g.gitDir != "" {
		args = append([]string{"--git-dir", g.gitDir}, args...)
	}
	command := exec.Command("git", args...)
	command.Dir = g.root
	return command
}

// run runs git with args and answers its stdout, failing with its stderr.
func (g gitRepo) run(args ...string) ([]byte, error) {
	command := g.command(args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// line runs git and answers its stdout's one line.
func (g gitRepo) line(args ...string) (string, error) {
	output, err := g.run(args...)
	return strings.TrimSpace(string(output)), err
}

// findRepo is the checkout that holds dir: the jj workspace when jj answers for dir, else the git
// checkout. jj is asked first, since a colocated jj workspace is a git checkout too, whose git
// HEAD is no merge in progress.
func findRepo(dir string) (gitRepo, error) {
	if root, err := jjLine(dir, "root"); err == nil && root != "" {
		gitDir, err := jjLine(root, "git", "root")
		if err != nil {
			return gitRepo{}, fmt.Errorf("%s is a jj workspace whose git store jj does not name: %w", root, err)
		}
		return gitRepo{root: root, gitDir: gitDir, jj: true}, nil
	}
	command := exec.Command("git", "rev-parse", "--show-toplevel")
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		return gitRepo{}, fmt.Errorf("%s is in no git checkout or jj workspace: %w", dir, err)
	}
	return gitRepo{root: strings.TrimSpace(string(output))}, nil
}

// gitTree is package api at one commit.
type gitTree struct {
	repo gitRepo
	rev  string
}

// read is one file of package api at this commit. A file the commit does not hold reads as
// os.ErrNotExist, as a directory's missing file does, so a caller tells "this revision has no
// such file" from "git could not be asked".
func (t gitTree) read(name string) ([]byte, error) {
	content, err := t.repo.run("show", t.rev+":"+path.Join(apiDir, name))
	if err != nil && strings.Contains(err.Error(), "does not exist in") || err != nil && strings.Contains(err.Error(), "exists on disk, but not in") {
		return nil, fmt.Errorf("%s at %s: %w", name, t.rev, os.ErrNotExist)
	}
	return content, err
}

func (t gitTree) filesNaming(word string) ([]string, error) {
	command := t.repo.command("grep", "-l", "-w", "-F", "-e", word, t.rev, "--", apiDir+"/")
	output, err := command.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(output) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("git grep %s at %s: %w", word, t.rev, err)
	}
	var names []string
	for line := range strings.Lines(string(output)) {
		_, file, _ := strings.Cut(strings.TrimSpace(line), ":")
		if path.Dir(file) == apiDir && strings.HasSuffix(file, ".go") {
			names = append(names, path.Base(file))
		}
	}
	return names, nil
}

// dirTree is package api as one directory holds it.
type dirTree string

func (d dirTree) read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(d), name))
}

func (d dirTree) filesNaming(word string) ([]string, error) {
	entries, err := os.ReadDir(string(d))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(string(d), entry.Name()))
		if err != nil {
			return nil, err
		}
		if bytes.Contains(content, []byte(word)) {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// mergeFile is git merge-file's three-way merge of one file: the merged bytes, and whether they
// hold a conflict.
func (g gitRepo) mergeFile(ours, base, theirs []byte, labels [3]string) ([]byte, bool, error) {
	dir, err := os.MkdirTemp("", "dispatch-route-adopt-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	paths := make([]string, 3)
	for index, content := range [][]byte{ours, base, theirs} {
		paths[index] = filepath.Join(dir, fmt.Sprint(index))
		if err := os.WriteFile(paths[index], content, 0o644); err != nil {
			return nil, false, err
		}
	}
	command := exec.Command("git", "merge-file", "-p",
		"-L", labels[0], "-L", labels[1], "-L", labels[2], paths[0], paths[1], paths[2])
	command.Dir = g.root
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), false, nil
	case errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() < 127:
		return stdout.Bytes(), true, nil
	}
	return nil, false, fmt.Errorf("git merge-file: %w: %s", err, strings.TrimSpace(stderr.String()))
}

// unmerged is every path still in conflict, sorted: what git holds unmerged in its index, or what
// jj's working-copy commit holds as a conflict.
func (g gitRepo) unmerged() ([]string, error) {
	var output []byte
	var err error
	if g.jj {
		var text string
		text, err = jjOutput(g.root, "file", "list", "-r", "@", "-T", `if(conflict, path ++ "\n")`)
		output = []byte(text)
	} else {
		output, err = g.run("diff", "--name-only", "--diff-filter=U")
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for line := range strings.Lines(string(output)) {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}
