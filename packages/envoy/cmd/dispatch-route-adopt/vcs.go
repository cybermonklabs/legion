package main

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// jjOutput runs jj in dir and answers its stdout, failing with its stderr.
func jjOutput(dir string, args ...string) (string, error) {
	command := exec.Command("jj", args...)
	command.Dir = dir
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("jj %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// jjLine runs jj in dir and answers its stdout's one line.
func jjLine(dir string, args ...string) (string, error) {
	output, err := jjOutput(dir, args...)
	return strings.TrimSpace(output), err
}

// mergeSides is the merge in progress as two commits and their names in the report.
type mergeSides struct {
	first, second         string
	firstName, secondName string
}

// sides finds the merge in progress: HEAD and MERGE_HEAD while `git merge` has stopped, or the
// two parents of jj's working-copy commit, which `jj new <branch> main@origin` makes.
func (g gitRepo) sides() (mergeSides, error) {
	if !g.jj {
		mergeHead, err := g.line("rev-parse", "-q", "--verify", "MERGE_HEAD")
		if err != nil || mergeHead == "" {
			return mergeSides{}, errors.New("no merge is in progress: run this while `git merge origin/main` has stopped")
		}
		head, err := g.line("rev-parse", "HEAD")
		if err != nil {
			return mergeSides{}, err
		}
		return mergeSides{head, mergeHead, "HEAD", "MERGE_HEAD"}, nil
	}
	output, err := jjOutput(g.root, "log", "--no-graph", "-r", "parents(@)", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return mergeSides{}, err
	}
	parents := strings.Fields(output)
	if len(parents) != 2 {
		return mergeSides{}, fmt.Errorf("jj's working-copy commit has %d parents, and a merge has two: run this with the merge as @, after `jj new <branch> main@origin`", len(parents))
	}
	return mergeSides{parents[0], parents[1], "@'s first parent " + parents[0][:12], "@'s second parent " + parents[1][:12]}, nil
}

// stage records files of package api written or removed: git adds them to its index; jj takes the
// working copy as it is when its next command runs, so nothing is done for it.
func (g gitRepo) stage(names, removed []string) error {
	if g.jj {
		for _, name := range removed {
			if err := os.Remove(filepath.Join(g.root, apiDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	if len(names) > 0 {
		if _, err := g.run(append([]string{"add", "--"}, prefixed(names)...)...); err != nil {
			return err
		}
	}
	// A route the branch removes is a file the incoming side added, so git's index holds it as a
	// staged addition, which a plain `git rm` refuses to drop. adopt removes only a file whose bytes
	// are the merge base's own, or the fixture the new layout no longer commits, so nobody's change
	// is lost: the index entry is forced out and the file deleted.
	if len(removed) > 0 {
		if _, err := g.run(append([]string{"rm", "-q", "-f", "--ignore-unmatch", "--"}, prefixed(removed)...)...); err != nil {
			return err
		}
	}
	return nil
}

// prefixed names each file of package api from the repository's root.
func prefixed(names []string) []string {
	paths := make([]string, len(names))
	for index, name := range names {
		paths[index] = path.Join(apiDir, name)
	}
	return paths
}

// routesNotAdopted names every route the branch adds, changes or removes that this step has not
// touched yet, which is what "the step has not run on this merge" means. The question is not
// whether a route's file holds the branch's text: a person may have resolved a conflict this step
// left, writing text that is neither side's, and a second run would take routes_table.go and
// routes_shapes.go from the new side again and throw away what they carried into them by hand.
//
// So a route counts only where the file still stands as the merge left it: the new side's text,
// or absent where the new side has none. Every other state is a run's own work or a person's --
// the branch's text this step wrote, the conflict markers it wrote, or a hand resolution -- and
// none of them is a route waiting to be written.
func routesNotAdopted(dir string, baseFiles, branchFiles map[string][]byte, newSide tree) ([]string, error) {
	// untouched reports whether name stands as the merge left it, and what the merge left.
	untouched := func(name string) (bool, error) {
		ours, err := newSide.read(name)
		onNewSide := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		current, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			return !onNewSide, nil
		case err != nil:
			return false, err
		}
		return onNewSide && bytes.Equal(current, ours), nil
	}
	var pending []string
	for _, name := range slices.Sorted(maps.Keys(baseFiles)) {
		if _, inBranch := branchFiles[name]; inBranch {
			continue
		}
		// The branch removed the route: the step removes its file, so it is pending while the
		// file still stands as the merge left it.
		stands, err := untouched(name)
		if err != nil {
			return nil, err
		}
		if stands {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				pending = append(pending, name)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(branchFiles)) {
		if baseContent, inBase := baseFiles[name]; inBase && bytes.Equal(baseContent, branchFiles[name]) {
			continue
		}
		stands, err := untouched(name)
		if err != nil {
			return nil, err
		}
		if stands {
			pending = append(pending, name)
		}
	}
	slices.Sort(pending)
	return slices.Compact(pending), nil
}

// recordConflict writes one path into git's index as a conflict: the merge base at stage 1, the
// new side's at 2 and the branch's at 3, which is what `git merge` leaves for a path it could not
// merge. git then refuses to commit until the person resolves the file and runs `git add`. jj
// takes the working copy as it is, markers and all, and keeps the file as a conflict of its own,
// so nothing is written there.
func (g gitRepo) recordConflict(name string, ours, base, theirs []byte) error {
	if g.jj {
		return nil
	}
	stages := []struct {
		stage   int
		content []byte
	}{{1, base}, {2, ours}, {3, theirs}}
	var index strings.Builder
	for _, entry := range stages {
		if entry.content == nil {
			continue
		}
		blob, err := g.hashObject(entry.content)
		if err != nil {
			return err
		}
		fmt.Fprintf(&index, "100644 %s %d\t%s\n", blob, entry.stage, path.Join(apiDir, name))
	}
	if _, err := g.run("rm", "-q", "-f", "--cached", "--ignore-unmatch", "--", path.Join(apiDir, name)); err != nil {
		return err
	}
	command := g.command("update-index", "--index-info")
	command.Stdin = strings.NewReader(index.String())
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("git update-index --index-info for %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// hashObject writes content into git's object store and answers its id.
func (g gitRepo) hashObject(content []byte) (string, error) {
	command := g.command("hash-object", "-w", "--stdin")
	command.Stdin = bytes.NewReader(content)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git hash-object: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// hasConflictMarkers reports whether a file holds a conflict git or jj wrote into it: a line that
// begins with seven '<' (git's `<<<<<<< ours`, jj's `<<<<<<< conflict 1 of 1`).
func hasConflictMarkers(content []byte) bool {
	for line := range strings.Lines(string(content)) {
		if strings.HasPrefix(line, "<<<<<<< ") || strings.TrimRight(line, "\n") == "<<<<<<<" {
			return true
		}
	}
	return false
}
