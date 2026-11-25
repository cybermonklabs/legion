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

// sidesOf is the two commits a merge of into would join: this checkout's own commit, and the one
// into names. It reads and merges nothing, so a caller can refuse a tree before any write.
func (g gitRepo) sidesOf(into string) (here, there string, err error) {
	if g.jj {
		if here, err = jjLine(g.root, "log", "--no-graph", "-r", "@", "-T", "commit_id"); err != nil {
			return "", "", err
		}
		if there, err = jjLine(g.root, "log", "--no-graph", "-r", into, "-T", "commit_id"); err != nil {
			return "", "", fmt.Errorf("%s names no commit jj knows: %w", into, err)
		}
		return here, there, nil
	}
	if here, err = g.line("rev-parse", "HEAD"); err != nil {
		return "", "", err
	}
	if there, err = g.line("rev-parse", "--verify", into+"^{commit}"); err != nil {
		return "", "", fmt.Errorf("%s names no commit: %w", into, err)
	}
	return here, there, nil
}

// startMerge makes the merge this step works in. The step writes route files, takes
// routes_table.go and routes_shapes.go whole from the new side, and writes conflicts into the
// index, so it must own the merge: a run over its own output would take those two files again and
// erase whatever had been carried into them. Afterwards the tree is mid-merge, so another run is
// refused by refuseATreeThisDidNotStart, with no file content read anywhere.
//
// Four guards that read content to answer "has this run already?" each missed a state a person or
// the step itself can leave. This asks nothing about content.
func (g gitRepo) startMerge(into, there string) error {
	if g.jj {
		head, err := jjLine(g.root, "log", "--no-graph", "-r", "@", "-T", "commit_id")
		if err != nil {
			return err
		}
		if _, err := jjOutput(g.root, "new", head, there, "-m", "merge "+into); err != nil {
			return err
		}
		return nil
	}
	// --no-commit, because the person finishes the merge once they have resolved what is left; a
	// merge that settles by itself still stops here, so the route layer is translated before
	// anything is recorded.
	_, mergeErr := g.run("merge", "--no-commit", "--no-ff", there)
	// A merge that stopped on conflicts is the state this step is for, and git records
	// MERGE_HEAD for it. A merge that failed for any other reason records none, and writing into
	// that tree would leave changes nobody asked for, so the failure is answered here.
	mergeHead, err := g.line("rev-parse", "-q", "--verify", "MERGE_HEAD")
	if err != nil || mergeHead == "" {
		if mergeErr != nil {
			return fmt.Errorf("git merge %s failed and left no merge in progress, so nothing was adopted: %w", into, mergeErr)
		}
		return fmt.Errorf("git merge %s left no MERGE_HEAD, so this step has no merge to resolve and wrote nothing", into)
	}
	if mergeHead != there {
		return fmt.Errorf("git merge %s left MERGE_HEAD at %s, and this step read %s: nothing is adopted into a merge it did not make", into, mergeHead, there)
	}
	return nil
}

// refuseATreeThisDidNotStart refuses a tree that is mid-merge or holds a change, naming what the
// person must do and what it costs them, since the step's safety rests on starting from a tree
// nobody has touched.
func (g gitRepo) refuseATreeThisDidNotStart() error {
	const lost = "any resolution you have already made in that merge is lost, this step's route files included; everything committed is untouched"
	if g.jj {
		parents, err := jjOutput(g.root, "log", "--no-graph", "-r", "parents(@)", "-T", `commit_id ++ "\n"`)
		if err != nil {
			return err
		}
		if len(strings.Fields(parents)) > 1 {
			return fmt.Errorf("@ is a merge already, and this step makes the merge itself: abandon it with `jj abandon @` and run this again (%s)", lost)
		}
		// jj has no uncommitted state: every change is in @ itself, so "nobody has touched it" is
		// @ being empty. `jj diff -r @` would answer the commit's own diff against its parent,
		// which is the branch's work when @ is a branch commit, so it is no reading of this.
		empty, err := jjLine(g.root, "log", "--no-graph", "-r", "@", "-T", "empty")
		if err != nil {
			return err
		}
		if empty != "true" {
			changed, err := jjOutput(g.root, "diff", "--summary", "-r", "@")
			if err != nil {
				return err
			}
			return fmt.Errorf("@ holds changes of its own, and this step begins from a commit nobody has touched: describe them and run `jj new` to sit on a fresh commit, then run this again:\n%s", strings.TrimRight(changed, "\n"))
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(g.gitDirOf(), "MERGE_HEAD")); err == nil {
		return fmt.Errorf("a merge is in progress, and this step makes the merge itself: end it with `git merge --abort` and run this again (%s)", lost)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changed, err := g.run("status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(changed)) != "" {
		return fmt.Errorf("the tree holds changes, and this step begins from a tree nobody has touched: commit or stash them, then run this again:\n%s", strings.TrimRight(string(changed), "\n"))
	}
	return nil
}

// gitDirOf is the git directory of this checkout: jj's store where jj answers for it, else git's
// own, which `git rev-parse --git-dir` names.
func (g gitRepo) gitDirOf() string {
	if g.gitDir != "" {
		return g.gitDir
	}
	dir, err := g.line("rev-parse", "--git-dir")
	if err != nil || dir == "" {
		return filepath.Join(g.root, ".git")
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(g.root, dir)
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

// recordConflict writes one path into git's index as a conflict, in git's own stage order: the
// merge base at stage 1, HEAD's side at 2 and MERGE_HEAD's at 3, which is what `git merge` leaves
// for a path it could not merge, and what `git checkout --ours` and `--theirs` read. git then refuses to commit until the person resolves the file and runs `git add`. jj
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
