// Command dispatch-route-adopt carries a branch made before Dispatch's routes moved into a file
// each across main's change, which leaves a merge of main into such a branch with routes_table.go
// and routes_shapes.go in conflict.
//
// Before the change every route of internal/dispatch/api was one row of routes() in
// routes_table.go, and what else was known of it stood in four maps beside the table, keyed
// "<method> <pattern>" (writeClasses, idRoutes, routeScopes, readScopeRoutes), and in a fifth in
// routes_shapes.go (routeShapes). Now each route is one file of its own, route_<path>+<method>.go
// (routeFileName says how a pattern is written), whose one declaration states every one of those
// facts, so two branches that add different routes touch no file in common.
//
// Usage, from packages/envoy of a checkout that holds this command, with the branch to adopt on a
// clean tree:
//
//	go run ./cmd/dispatch-route-adopt [-C <worktree>] [<revision>]
//
// It makes the merge itself: do not merge first. <revision> is what to merge: main as the remote
// has it by default, which jj names main@origin and git origin/main. -C names the worktree to
// adopt, which a branch made before the move needs, since that branch does not hold this command
// until the merge brings it in: run it from a checkout of main (or of the branch that adds it)
// with -C naming the branch's worktree.
//
// It works in three steps, in this order:
//
//  1. It refuses a tree that is mid-merge or holds a change, naming the commands that return it
//     and what they cost: `git merge --abort`; under jj, where the merge is the working-copy
//     commit, `jj abandon @` and then `jj edit` (or `jj new`) of the merge's first parent, since
//     `jj abandon @` alone leaves a new working copy on the same two parents.
//  2. It reads both sides, the merge base and both splits, and settles every file it will write
//     and every three-way merge, so a branch it cannot adopt is refused with the tree as it was.
//  3. It makes the merge (`git merge --no-commit --no-ff`; `jj new <branch side> <revision>`),
//     checks that git left MERGE_HEAD at the revision it read, and writes. A failure after the
//     merge is made names the commands that return the tree to the branch.
//
// A checkout jj answers for (`jj root`) is resolved with jj: it makes the merge commit from a fresh
// empty `@` (`jj new <branch>` first), commits are read from jj's git store (`jj git root`),
// nothing is staged, and the conflicts left are the ones `jj file list` reports.
//
// One side of the merge holds the old layout and the other the new one, in either order: a branch
// made before the move merging main, or the move's own branch merging a main that still had the
// old table (how the move itself was carried across main). The tool splits the old layout at the
// merge base and at the old-layout side into one file per route, with the splitter that wrote
// main's route files, and compares the two splits route by route:
//
//   - a route the branch added is written;
//   - a route the branch changed is merged into the file the new side holds (git merge-file, HEAD's
//     side first and MERGE_HEAD's last), and a file the merge leaves in conflict carries git's
//     markers, is written into git's index in conflict (stage 1 the merge base, 2 HEAD's side, 3
//     MERGE_HEAD's, each that side's own bytes) and is named among what is left;
//   - a route the branch removed is deleted, where the new side's file is still the one the
//     merge base held, and is named among what is left where main has changed it since.
//
// routes_table.go and routes_shapes.go are then taken whole from the new side. An edit the
// branch made to either outside the moved entries - a type, a helper, a comment, routes() itself -
// is printed as a diff for the person to carry by hand, since nothing can place it mechanically.
// testdata/routes.txt, which the new layout no longer commits, is removed. Every file it writes or
// removes is staged; it changes no other file. It ends by listing what is left: route files merged
// with conflicts, edits to carry by hand, the other paths git still holds in conflict, and every
// line of package api's other files that still names one of the old maps.
//
// It exits 0 when nothing is left, 2 when it resolved what it could and something is left for the
// person, and 1 when it could not run.
//
//	go run ./cmd/dispatch-route-adopt split -rev <commit> -out <dir>
//
// writes the route files the old layout at <commit> splits into, and nothing else, into <dir>. It
// is how main's route files were made: splitting main's old table again writes each route's
// declaration as the committed file holds it, which is what lets the adopt step above find a
// route the branch did not touch unchanged on main's side. The comments differ: a comment the old
// maps wrote above a group of entries came out on the group's first route alone, where it said
// nothing, and those were rewritten by hand once.
//
// The tool is for the branches open when the routes moved into files. Once each of them has
// merged or closed, delete it: the file-name rule it writes names with is
// internal/dispatch/routefile's, which package api's own test holds every route file to
// (TestEachRouteIsDeclaredInTheFileItsNameNames), so nothing of the rule is lost with it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command, with its exit status.
func run(args []string, stdout, stderr io.Writer) int {
	working, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "dispatch-route-adopt:", err)
		return 1
	}
	repo, err := findRepo(working)
	if err != nil {
		fmt.Fprintln(stderr, "dispatch-route-adopt:", err)
		return 1
	}
	if len(args) > 0 && args[0] == "split" {
		return runSplit(repo, args[1:], stdout, stderr)
	}
	// The step runs before the merge, so the branch's own tree does not hold this command yet:
	// it is run from a checkout that has it (main, once this has landed) with -C naming the
	// branch's worktree.
	into := ""
	for len(args) > 0 {
		switch {
		case args[0] == "-C" && len(args) > 1:
			repo, err = findRepo(args[1])
			if err != nil {
				fmt.Fprintln(stderr, "dispatch-route-adopt:", err)
				return 1
			}
			args = args[2:]
		case !strings.HasPrefix(args[0], "-"):
			into, args = args[0], args[1:]
		default:
			fmt.Fprintf(stderr, "dispatch-route-adopt: unknown argument %q: run it with none to merge main (origin/main; main@origin under jj), with `-C <worktree>` for another checkout, with one revision to merge that, or as `split -rev <commit> -out <dir>`\n", args[0])
			return 1
		}
	}
	if into == "" {
		into = repo.defaultRevision()
	}
	report, err := adopt(repo, into)
	if err != nil {
		fmt.Fprintln(stderr, "dispatch-route-adopt:", err)
		return 1
	}
	report.print(stdout)
	if report.left() {
		return 2
	}
	return 0
}

// runSplit writes the route files of the old layout at one commit into a directory.
func runSplit(repo gitRepo, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("split", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rev := flags.String("rev", "", "the commit whose old layout to split")
	out := flags.String("out", "", "the directory to write the route files into")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if *rev == "" || *out == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "dispatch-route-adopt split: -rev and -out are both required, and nothing else")
		return 1
	}
	files, err := splitOldLayout(gitTree{repo: repo, rev: *rev})
	if err == nil {
		err = writeFiles(*out, files)
	}
	if err != nil {
		fmt.Fprintln(stderr, "dispatch-route-adopt split:", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %d route files into %s\n", len(files), *out)
	return 0
}

// writeFiles writes files into dir, in name order.
func writeFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}
