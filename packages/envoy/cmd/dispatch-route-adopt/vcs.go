package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
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

// defaultRevision is what the step merges when it is given none: main as the remote has it,
// which jj names main@origin and git origin/main.
func (g gitRepo) defaultRevision() string {
	if g.jj {
		return "main@origin"
	}
	return "origin/main"
}

// startMerge makes the merge this step works in, of here (the commit sidesOf read as this
// checkout's own) and there. The step writes route files, takes routes_table.go and
// routes_shapes.go whole from the new side, and writes conflicts into the index, so it must own
// the merge. Afterwards the tree is mid-merge, and refuseATreeThisDidNotStart refuses another
// run for that reason alone.
func (g gitRepo) startMerge(into, here, there string) error {
	if g.jj {
		// git answers a revision the branch holds already with "Already up to date" and no
		// merge (below); jj would make a merge commit with a redundant parent and the step would
		// write into it. So jj's half asks first, of the git store jj keeps.
		ancestor := g.command("merge-base", "--is-ancestor", there, here).Run()
		var exit *exec.ExitError
		switch {
		case ancestor == nil:
			return fmt.Errorf("%s is an ancestor of the branch (the branch holds %s already), so there is no merge to make: nothing is merged and nothing is written", into, into)
		case !errors.As(ancestor, &exit) || exit.ExitCode() != 1:
			return fmt.Errorf("git merge-base --is-ancestor %s %s: %w, so nothing is merged", there, here, ancestor)
		}
		// here, not @ read again: the merge must join the commit whose layout the step read.
		if _, err := jjOutput(g.root, "new", here, there, "-m", "merge "+into); err != nil {
			return fmt.Errorf("jj new %s %s failed, so nothing is merged: %w", here, there, err)
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
	mergeHead, err := g.mergeHead()
	if err != nil {
		return fmt.Errorf("%w\ngit merge %s ran, and whether it left a merge is unread: if `git status` says a merge is in progress, end it with %s", err, into, g.gitAbort())
	}
	if mergeHead == "" {
		if mergeErr != nil {
			return fmt.Errorf("git merge %s failed and left no merge in progress, so nothing is merged and nothing is written: %w", into, mergeErr)
		}
		return fmt.Errorf("git merge %s left no MERGE_HEAD (the branch holds %s already, or git made no merge of it), so nothing is merged and nothing is written", into, into)
	}
	if mergeHead != there {
		return fmt.Errorf("git merge %s left MERGE_HEAD at %s, and this step read %s: nothing is adopted into a merge it did not make; end it with %s", into, mergeHead, there, g.gitAbort())
	}
	return nil
}

// mergeHead is the commit git's merge in progress merges, and "" where no merge is in progress.
func (g gitRepo) mergeHead() (string, error) {
	output, err := g.command("rev-parse", "-q", "--verify", "MERGE_HEAD").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(output) == 0 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("git rev-parse -q --verify MERGE_HEAD: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// shellQuoted is s as one word of a POSIX shell, so a printed command can be pasted whatever
// the path holds.
func shellQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitAbort is the command that ends a git merge in progress, as text to print: `git merge
// --abort` returns the index and every tracked file to HEAD, and refuses, rather than throws away,
// a file with a change not yet added, which a merge a person made may hold.
func (g gitRepo) gitAbort() string {
	return fmt.Sprintf("`git -C %s merge --abort`", shellQuoted(g.root))
}

// jjLeave is the commands that end jj's merge, the working-copy commit, and return to its
// first parent, branchSide, as text to print. `jj abandon @` alone puts a new working copy on the
// merge's two parents, which is a merge again, so the second command goes back to the branch
// side: `jj edit` where it is empty, as the step's own merge leaves it, since the step began on
// it, and `jj new` on it where it holds work. jj drops the empty working copy the abandon left.
func (g gitRepo) jjLeave(branchSide string, branchSideEmpty bool) string {
	back := "new"
	if branchSideEmpty {
		back = "edit"
	}
	return fmt.Sprintf("`jj -R %s abandon @`, then `jj -R %s %s %s`", shellQuoted(g.root), shellQuoted(g.root), back, branchSide)
}

// leaveTheMerge is how to return the tree after a failure once this step's own merge is made,
// as text to print. Under jj the merge's first parent is here, the empty commit the step began on.
//
// Under git it is not `git merge --abort`: a failure among the writes can leave a file the merge
// added with the step's bytes not yet added, which `git merge --abort` refuses ("not uptodate.
// Cannot merge"). The step began from a tree git listed no change in, so `git reset --hard HEAD`
// throws away only the merge and the step's own writes, and ends the merge. It leaves a file
// git never tracked, so a `git clean -x` of exactly the files the step wrote that HEAD does not
// hold, written, follows: -x, because a local exclude can hide one of them from git.
func (g gitRepo) leaveTheMerge(here string, written []string) string {
	if g.jj {
		return g.jjLeave(here, true)
	}
	text := fmt.Sprintf("`git -C %s reset -q --hard HEAD`", shellQuoted(g.root))
	if len(written) == 0 {
		return text
	}
	paths := make([]string, len(written))
	for index, name := range written {
		paths[index] = shellQuoted(path.Join(apiDir, name))
	}
	return text + fmt.Sprintf(", then `git -C %s clean -q -f -x -- %s`", shellQuoted(g.root), strings.Join(paths, " "))
}

// refuseATreeThisDidNotStart refuses a tree that is mid-merge or holds a change, naming what the
// person must do and what it costs them, since the step's safety rests on starting from a tree
// nobody has touched.
func (g gitRepo) refuseATreeThisDidNotStart() error {
	const lost = "any resolution you have already made in that merge is lost, this step's route files included"
	if g.jj {
		parents, err := jjOutput(g.root, "log", "--no-graph", "-r", "@", "-T",
			`parents.map(|c| c.commit_id() ++ " " ++ if(c.empty(), "empty", "changed")).join("\n") ++ "\n"`)
		if err != nil {
			return err
		}
		if lines := strings.Split(strings.TrimSpace(parents), "\n"); len(lines) > 1 {
			first, emptiness, _ := strings.Cut(lines[0], " ")
			return fmt.Errorf("@ is a merge already, and this step makes the merge itself: return to its first parent, the branch side, with %s, and run this again (%s; every other commit is untouched)", g.jjLeave(first, emptiness == "empty"), lost)
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
	if mergeHead, err := g.mergeHead(); err != nil {
		return err
	} else if mergeHead != "" {
		return fmt.Errorf("a merge is in progress, and this step makes the merge itself: end it with %s and run this again (%s; everything committed is untouched)", g.gitAbort(), lost)
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

// conflictSides are one path's three sides of a merge, each that side's own bytes: the merge
// base's, HEAD's (ours) and MERGE_HEAD's (theirs). nil is a side that has no such file.
type conflictSides struct {
	base, ours, theirs []byte
}

// recordConflict writes one path into git's index as a conflict, in git's own stage order: the
// merge base at stage 1, HEAD's side at 2 and MERGE_HEAD's at 3, which is what `git merge` leaves
// for a path it could not merge, and what `git checkout --ours` and `--theirs` read. git then
// refuses to commit until the person resolves the file and runs `git add`. jj takes the working
// copy as it is, markers and all, and keeps the file as a conflict of its own, so nothing is
// written there.
func (g gitRepo) recordConflict(name string, sides conflictSides) error {
	if g.jj {
		return nil
	}
	stages := []struct {
		stage   int
		content []byte
	}{{1, sides.base}, {2, sides.ours}, {3, sides.theirs}}
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
