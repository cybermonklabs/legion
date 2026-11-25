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

// routesNotAdopted names every route this step would still write or delete, which is what "the
// step has not run on this merge" means. A route counts on two conditions together, and each is
// needed:
//
//   - its file still stands as the merge left it: the new side's bytes, or absent where the new
//     side has none. Every other state is a run's own work (the branch's text it wrote, or the
//     conflict markers it wrote) or a person's (a conflict resolved by hand, writing text that is
//     neither side's). A second run over any of those would take routes_table.go and
//     routes_shapes.go from the new side again and throw away whatever was carried into them.
//   - the step would change that file. Three states it leaves alone would otherwise count for
//     ever: a route main removed and the branch changes, which the step reports for the person to
//     decide; a route the branch adds or changes exactly as main did, which is already what it
//     would write; and a route the branch removes that main has changed, another decide.
//
// The conditions mirror the loop of adopt, which is why they are written beside its three cases
// rather than as one predicate over them.
func routesNotAdopted(dir string, baseFiles, branchFiles map[string][]byte, newSide tree) ([]string, error) {
	var pending []string
	for _, name := range slices.Sorted(maps.Keys(baseFiles)) {
		if _, inBranch := branchFiles[name]; inBranch {
			continue
		}
		// The branch removed the route. The step deletes the file where main left it as the merge
		// base had it, and reports a decide where main changed it.
		ours, onNewSide, err := readIfPresent(newSide, name)
		if err != nil {
			return nil, err
		}
		current, inTree, err := readFileIfPresent(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		stands := onNewSide == inTree && (!inTree || bytes.Equal(current, ours))
		if stands && onNewSide && bytes.Equal(ours, baseFiles[name]) {
			pending = append(pending, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(branchFiles)) {
		branchContent := branchFiles[name]
		baseContent, inBase := baseFiles[name]
		if inBase && bytes.Equal(baseContent, branchContent) {
			continue
		}
		ours, onNewSide, err := readIfPresent(newSide, name)
		if err != nil {
			return nil, err
		}
		current, inTree, err := readFileIfPresent(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if stands := onNewSide == inTree && (!inTree || bytes.Equal(current, ours)); !stands {
			continue
		}
		switch {
		case !inBase:
			// The branch added the route: the step writes it, unless main added the same bytes.
			if !onNewSide || !bytes.Equal(branchContent, ours) {
				pending = append(pending, name)
			}
		case onNewSide && !bytes.Equal(branchContent, ours):
			// The branch changed the route and main still holds a version to write over. Where
			// main removed the route (no file on the new side), the step reports a decide.
			pending = append(pending, name)
		}
	}
	slices.Sort(pending)
	return slices.Compact(pending), nil
}

// readIfPresent is a file of one tree, and whether the tree holds it.
func readIfPresent(src tree, name string) ([]byte, bool, error) {
	content, err := src.read(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return content, true, nil
}

// readFileIfPresent is a file on disk, and whether it is there.
func readFileIfPresent(path string) ([]byte, bool, error) {
	content, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return content, true, nil
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

// sharedFilesInConflict reports whether routes_table.go or routes_shapes.go still stands in
// conflict, which is how a merge of the two layouts leaves them and what this step resolves by
// taking them from the new side. A file a person has resolved is not in conflict, and the step's
// own resolution is not either, so this answers false once either has happened.
func (g gitRepo) sharedFilesInConflict() (bool, error) {
	unmerged, err := g.unmerged()
	if err != nil {
		return false, err
	}
	for _, name := range []string{tableFile, shapesFile} {
		if slices.Contains(unmerged, path.Join(apiDir, name)) {
			return true, nil
		}
		content, _, err := readFileIfPresent(filepath.Join(g.root, apiDir, name))
		if err != nil {
			return false, err
		}
		if hasConflictMarkers(content) {
			return true, nil
		}
	}
	return false, nil
}
