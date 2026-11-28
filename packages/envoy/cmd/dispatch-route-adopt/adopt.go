package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// routesFixture is the route index the old layout committed, which the new one does not.
const routesFixture = "testdata/routes.txt"

// oldNames are the declarations the new layout removed: a line of package api that still names
// one was written against the old layout.
var oldNames = regexp.MustCompile(`\b(writeClasses|idRoutes|routeScopes|readScopeRoutes|approveScopeRoutes|readClasses|routeShapes|routeRow|withIDRoute|scopedRoutesNotMounted|shapedRoutesNotMounted|readRoutesNotMountedAsGET|routesNotMounted|routeIndexGolden)\b`)

// adoptReport is what one run did and what it leaves for the person.
type adoptReport struct {
	newSide, oldSide, base string
	// oldName and newName name the side that added, changed or removed a route in the report's
	// lines: "the branch" and the revision merged, either of which can be the side that holds the
	// old layout. oldSide and newSide are oldName and newName with their commits.
	oldName, newName string

	written, updated, merged, removed []string
	fixtureRemoved                    bool
	// taken are the old layout's shared files written as the new side holds them: routes_table.go
	// and routes_shapes.go always, and read_classes.go where the new side holds one.
	taken []string
	// removedShared are the old layout's shared files this removed because the new side removed
	// them: read_classes.go, where the merge base holds one and the new side none.
	removedShared []string

	// conflicted are route files the three-way merge left markers in.
	conflicted []string
	// decide are routes the two sides each changed in a way no merge settles: one side removed
	// the route the other changed.
	decide []string
	// carry are the old-layout side's edits to oldLayoutFiles outside the moved entries, each a
	// diff of that file's shared code from the merge base, or from nothing where the merge base
	// holds no such file, to that side.
	carry []string
	// unmerged are the paths git still holds in conflict once the run is done.
	unmerged []string
	// stale are the lines of package api that still name a declaration the old layout had.
	stale []string
}

// left reports whether anything is left for the person.
func (r *adoptReport) left() bool {
	return len(r.conflicted)+len(r.decide)+len(r.carry)+len(r.unmerged)+len(r.stale) > 0
}

func (r *adoptReport) print(w io.Writer) {
	fmt.Fprintf(w, "dispatch-route-adopt: the old layout is %s's, the new one %s's, the merge base %s\n", r.oldSide, r.newSide, r.base)
	for _, list := range []struct {
		what  string
		names []string
	}{
		{"wrote the route " + r.oldName + " added", r.written},
		{"wrote " + r.oldName + "'s change to a route " + r.newName + " left as the merge base had it", r.updated},
		{"merged " + r.oldName + "'s change into " + r.newName + "'s changed route", r.merged},
		{"removed the route " + r.oldName + " removed", r.removed},
	} {
		for _, name := range list.names {
			fmt.Fprintf(w, "  %s: %s\n", list.what, path.Join(apiDir, name))
		}
	}
	fmt.Fprintf(w, "  took %s from %s\n", spokenList(prefixed(r.taken)), r.newSide)
	for _, name := range prefixed(r.removedShared) {
		fmt.Fprintf(w, "  removed %s, which %s does not hold\n", name, r.newSide)
	}
	if r.fixtureRemoved {
		fmt.Fprintf(w, "  removed %s, which the new layout does not commit\n", path.Join(apiDir, routesFixture))
	}
	if !r.left() {
		fmt.Fprintln(w, "dispatch-route-adopt: nothing is left")
		return
	}
	fmt.Fprintln(w, "dispatch-route-adopt: left for you:")
	for _, name := range r.conflicted {
		fmt.Fprintf(w, "  resolve the conflict markers in %s\n", path.Join(apiDir, name))
	}
	for _, line := range r.decide {
		fmt.Fprintf(w, "  %s\n", line)
	}
	for _, diff := range r.carry {
		fmt.Fprintf(w, "  %s\n", strings.ReplaceAll(strings.TrimRight(diff, "\n"), "\n", "\n    "))
	}
	for _, name := range r.unmerged {
		fmt.Fprintf(w, "  still in conflict: %s\n", name)
	}
	for _, line := range r.stale {
		fmt.Fprintf(w, "  names a declaration the old layout had: %s\n", line)
	}
}

// spokenList is names as a sentence lists them: "a", "a and b", "a, b and c".
func spokenList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// routeWrite is one file this step writes once the merge is made, with the index stages git
// records for it where its three-way merge is left in conflict.
type routeWrite struct {
	name    string
	content []byte
	// sides are the merge base's, HEAD's and MERGE_HEAD's own bytes, nil for a file the merge
	// settled. They are never the conflict-marked text the working tree holds, so `git checkout
	// --ours` and `--theirs` answer a side a person can use.
	sides *conflictSides
}

// mergeSide is one side of the merge adopt makes: how the report names it, "the branch" or the
// revision merged, and its commit.
type mergeSide struct{ name, rev string }

// described is the side as the report and the conflict labels name it: its name and its commit.
func (s mergeSide) described() string { return s.name + " " + s.rev[:12] }

// adopt makes the merge of into and resolves its route layer (see the package comment).
//
// It reads, then merges, then writes. The tree is refused or accepted, and both sides, the merge
// base, both splits and every file this writes are read and settled, before the merge begins, so
// a branch this cannot adopt is refused with the tree as it was. Only the writes, the staging and
// the report's reading of the merged tree come after the merge, and a failure among them names
// how to return the tree to the branch side.
func adopt(repo gitRepo, into string) (*adoptReport, error) {
	if err := repo.refuseATreeThisDidNotStart(); err != nil {
		return nil, err
	}
	here, there, err := repo.sidesOf(into)
	if err != nil {
		return nil, err
	}
	hereOld, err := hasOldLayout(gitTree{repo, here})
	if err != nil {
		return nil, fmt.Errorf("read the branch's %s: %w", tableFile, err)
	}
	thereOld, err := hasOldLayout(gitTree{repo, there})
	if err != nil {
		return nil, fmt.Errorf("read %s's %s: %w", into, tableFile, err)
	}
	// The merge's two sides in git's order, HEAD first and MERGE_HEAD last, which is the order of
	// the index stages and of every three-way merge's inputs and labels. oldAt is the one that
	// holds the old layout, decided here alone: the report's names, the revisions split and taken
	// from, and the merge's order of ours and theirs all read from it.
	sides := [2]mergeSide{{"the branch", here}, {into, there}}
	var oldAt int
	switch {
	case hereOld && thereOld:
		return nil, fmt.Errorf("the branch and %s both hold the old route table: neither side has the layout to adopt, and nothing is merged", into)
	case !hereOld && !thereOld:
		return nil, fmt.Errorf("the branch and %s both hold the route files already: there is nothing to adopt, and nothing is merged", into)
	case hereOld:
		oldAt = 0
	default:
		oldAt = 1
	}
	report := &adoptReport{
		oldName: sides[oldAt].name, newName: sides[1-oldAt].name,
		oldSide: sides[oldAt].described(), newSide: sides[1-oldAt].described(),
	}
	oldRev, newRev := sides[oldAt].rev, sides[1-oldAt].rev
	base, err := repo.line("merge-base", here, there)
	if err != nil {
		return nil, fmt.Errorf("the branch and %s share no merge base, so there is no old layout to split their routes from, and nothing is merged (%w)", into, err)
	}
	report.base = base
	if baseOld, err := hasOldLayout(gitTree{repo, base}); err != nil {
		return nil, fmt.Errorf("read the merge base's %s: %w", tableFile, err)
	} else if !baseOld {
		return nil, fmt.Errorf("the merge base %s holds the route files already, so %s's table is no old layout this can split, and nothing is merged", base, report.oldName)
	}
	baseFiles, err := splitOldLayout(gitTree{repo, base})
	if err != nil {
		return nil, fmt.Errorf("split the merge base's routes: %w", err)
	}
	oldFiles, err := splitOldLayout(gitTree{repo, oldRev})
	if err != nil {
		return nil, fmt.Errorf("split %s's routes: %w", report.oldSide, err)
	}

	newSide := gitTree{repo, newRev}
	var writes []routeWrite
	var remove []string
	// Every three-way merge's inputs and labels are in the order of sides, so an editor's
	// "current change" in a route file is HEAD's, as it is in every file git merged itself.
	labels := [3]string{"HEAD (" + sides[0].described() + ")", "merge base", into + " (" + sides[1].described() + ")"}
	merge := func(name string, newContent, baseContent, oldContent []byte) (routeWrite, error) {
		var contents [2][]byte
		contents[oldAt], contents[1-oldAt] = oldContent, newContent
		ours, theirs := contents[0], contents[1]
		merged, conflict, err := repo.mergeFile(ours, baseContent, theirs, labels)
		if err != nil {
			return routeWrite{}, err
		}
		write := routeWrite{name: name, content: merged}
		if conflict {
			report.conflicted = append(report.conflicted, name)
			write.sides = &conflictSides{base: baseContent, ours: ours, theirs: theirs}
		} else {
			report.merged = append(report.merged, name)
		}
		return write, nil
	}
	names := slices.Sorted(maps.Keys(baseFiles))
	for name := range oldFiles {
		if _, inBase := baseFiles[name]; !inBase {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		baseContent, inBase := baseFiles[name]
		oldContent, inOld := oldFiles[name]
		if inBase && inOld && bytes.Equal(baseContent, oldContent) {
			continue
		}
		// Only the new side holds route files, so the merge leaves each one as the new side holds
		// it: reading it there is reading what the merge will leave, before it is made.
		current, inTree, err := readIfPresent(newSide, name)
		if err != nil {
			return nil, err
		}
		write := routeWrite{name: name, content: oldContent}
		switch {
		case !inBase: // the old-layout side added it
			switch {
			case !inTree:
				report.written = append(report.written, name)
			case bytes.Equal(current, oldContent):
				continue // the new side added the same route, so the merge holds it already
			default:
				if write, err = merge(name, current, nil, oldContent); err != nil {
					return nil, err
				}
			}
		case !inOld: // the old-layout side removed it
			switch {
			case !inTree:
			case bytes.Equal(current, baseContent):
				report.removed = append(report.removed, name)
				remove = append(remove, name)
			default:
				report.decide = append(report.decide, fmt.Sprintf("%s removes the route of %s, which %s has changed since the merge base: delete it or keep %s's", report.oldName, path.Join(apiDir, name), report.newName, report.newName))
			}
			continue
		default: // the old-layout side changed it
			switch {
			case !inTree:
				report.decide = append(report.decide, fmt.Sprintf("%s removed the route of %s, which %s changes: %s's version is not written", report.newName, path.Join(apiDir, name), report.oldName, report.oldName))
				continue
			case bytes.Equal(current, baseContent):
				report.updated = append(report.updated, name)
			default:
				if write, err = merge(name, current, baseContent, oldContent); err != nil {
					return nil, err
				}
			}
		}
		writes = append(writes, write)
	}
	// The old layout's shared files are taken whole from the new side, whose text of each is the
	// new layout's, and the old-layout side's edit to each outside the moved entries is printed
	// for the person to carry by hand, since nothing can place it mechanically.
	baseTree, oldTree := gitTree{repo, base}, gitTree{repo, oldRev}
	for _, name := range oldLayoutFiles {
		baseContent, inBase, err := readIfPresent(baseTree, name)
		if err != nil {
			return nil, err
		}
		content, err := newSide.read(name)
		switch {
		case err == nil:
			writes = append(writes, routeWrite{name: name, content: content})
			report.taken = append(report.taken, name)
		case mayBeMissing(name) && errors.Is(err, os.ErrNotExist):
			// read_classes.go is #4557's, and a new side made before #4557 holds none, so nothing
			// is written for it. Where the merge base holds one, the new side removed it, and so
			// does this: its readClasses entries are the route files' now and its other edits are
			// carried below, and git and jj otherwise leave an old-layout side's change to it in
			// conflict with the removal. Where the merge base holds none, the old-layout side added
			// it with #4557 and the merge adds it cleanly, with the types that side's Read fields
			// name. It is left as that side wrote it, so nothing of it is carried, and since it is
			// not taken, the stale-line check names its readClasses for the person.
			if !inBase {
				continue
			}
			remove = append(remove, name)
			report.removedShared = append(report.removedShared, name)
		default:
			return nil, err
		}
		carried, err := sharedCodeChange(repo, name, baseContent, inBase, oldTree, report.oldName)
		if err != nil {
			return nil, err
		}
		if carried != "" {
			report.carry = append(report.carry, carried)
		}
	}
	if _, err := oldTree.read(routesFixture); err == nil {
		if _, err := newSide.read(routesFixture); err != nil {
			remove = append(remove, routesFixture)
			report.fixtureRemoved = true
		}
	}
	// The files this writes that the branch's commit does not hold: a failure after the merge is
	// made leaves them where git does not track them, so the way back names each.
	var newToHere []string
	for _, write := range writes {
		if _, held, err := readIfPresent(gitTree{repo, here}, write.name); err != nil {
			return nil, err
		} else if !held {
			newToHere = append(newToHere, write.name)
		}
	}

	if err := repo.startMerge(into, here, there); err != nil {
		return nil, err
	}
	if err := writeTheMerge(repo, writes, remove); err != nil {
		return nil, fmt.Errorf("%w\nthe merge is made and its route layer only partly written: return the tree to the branch with %s, and run this again", err, repo.leaveTheMerge(here, newToHere))
	}
	// The route layer is written and staged whole. What is left only reads the merged tree for
	// the report, so a failure here leaves a complete merge, and its error names no way back.
	if report.unmerged, err = repo.unmerged(); err != nil {
		return nil, err
	}
	if report.stale, err = staleLines(filepath.Join(repo.root, apiDir), report.taken); err != nil {
		return nil, err
	}
	return report, nil
}

// writeTheMerge writes and stages what adopt settled into the merge it made.
func writeTheMerge(repo gitRepo, writes []routeWrite, remove []string) error {
	dir := filepath.Join(repo.root, apiDir)
	var stage []string
	for _, write := range writes {
		if err := os.WriteFile(filepath.Join(dir, write.name), write.content, 0o644); err != nil {
			return err
		}
		if write.sides == nil {
			stage = append(stage, write.name)
			continue
		}
		// A conflict this step cannot resolve is written into the index as one, so git refuses to
		// commit the path until the person resolves it and runs `git add`. Without this, the
		// version in the working tree sits at stage 0, which a plain `git commit` records.
		//
		// The stages are git's own: 1 the merge base, 2 HEAD's side and 3 MERGE_HEAD's. For a
		// branch made before the move HEAD is the branch, so `git checkout --ours` answers the
		// branch's route and `--theirs` main's, as they do for every other path of the merge.
		if err := repo.recordConflict(write.name, *write.sides); err != nil {
			return err
		}
	}
	return repo.stage(stage, remove)
}

// sharedCodeChange is the old-layout side's change to one old-layout file outside the entries the
// route files took over - the rows of routes() and the side maps' entries - as a diff from the
// merge base's text of it, base, and empty where that side changed nothing else of it. inBase
// reports whether the merge base holds the file: where it holds none, the diff is from nothing,
// so the whole of a file that side added is carried, its moved entries cut out. side is how the
// report names that side: the branch, or the revision merged.
//
// It is empty where that side holds no such file. Only read_classes.go can be missing, since the
// split read the table and the shapes at both, and a side without it never had it or removed
// it: either way, that side holds no code of it to carry.
func sharedCodeChange(repo gitRepo, name string, base []byte, inBase bool, old tree, side string) (string, error) {
	oldContent, inOld, err := readIfPresent(old, name)
	if err != nil {
		return "", err
	}
	if !inOld || inBase && bytes.Equal(base, oldContent) {
		return "", nil
	}
	var baseShared string // nothing, where the merge base holds no such file
	if inBase {
		if baseShared, err = sharedCode(name, base); err != nil {
			return "", fmt.Errorf("read the merge base's %s: %w", name, err)
		}
	}
	oldShared, err := sharedCode(name, oldContent)
	if err != nil {
		return "", fmt.Errorf("read %s's %s: %w", side, name, err)
	}
	if baseShared == oldShared {
		return "", nil
	}
	diff, err := repo.diffText(baseShared, oldShared)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("carry by hand %s's edit to %s outside the moved entries (the moved entries are cut out, as …):\n%s",
		side, path.Join(apiDir, name), diff), nil
}

// sharedCode is an old-layout file with every moved entry cut out: the elements of the side maps,
// readClasses among them, and of the two key lists, readScopeRoutes and approveScopeRoutes, the
// rows of routes() and the test hooks' rows, each list left as {…}.
func sharedCode(name string, content []byte) (string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "", err
	}
	type cut struct{ from, to int }
	var cuts []cut
	offset := func(pos token.Pos) int { return fset.Position(pos).Offset }
	for _, mapName := range []string{writeClassesName, idRoutesName, routeScopesName, readScopeRoutesName, approveRoutesName, routeShapesName, readClassesName} {
		if literal := packageLiteral(file, mapName); literal != nil {
			cuts = append(cuts, cut{offset(literal.Lbrace) + 1, offset(literal.Rbrace)})
		}
	}
	if body := routesBody(file); body != nil {
		ast.Inspect(body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CompositeLit:
				if isRowSlice(n.Type) {
					cuts = append(cuts, cut{offset(n.Lbrace) + 1, offset(n.Rbrace)})
					return false
				}
			case *ast.IfStmt:
				if string(content[offset(n.Cond.Pos()):offset(n.Cond.End())]) != hooksCondition {
					return true
				}
				// The test hooks' rows, `rows = append(rows, routeRow{...}, ...)`: each run of routeRow
				// operands is cut, and nothing else, so another statement or another append the branch
				// writes in the hooks' block is still compared.
				ast.Inspect(n.Body, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					first := -1
					for index, arg := range call.Args {
						if literal, isLiteral := arg.(*ast.CompositeLit); isLiteral {
							if ident, isIdent := literal.Type.(*ast.Ident); isIdent && ident.Name == "routeRow" {
								if first < 0 {
									first = index
								}
								if index == len(call.Args)-1 {
									cuts = append(cuts, cut{offset(call.Args[first].Pos()), offset(arg.End())})
								}
								continue
							}
						}
						if first >= 0 {
							cuts = append(cuts, cut{offset(call.Args[first].Pos()), offset(call.Args[index-1].End())})
							first = -1
						}
					}
					return true
				})
				return false
			}
			return true
		})
	}
	slices.SortFunc(cuts, func(a, b cut) int { return a.from - b.from })
	var out strings.Builder
	at := 0
	for _, c := range cuts {
		if c.from < at {
			continue
		}
		out.Write(content[at:c.from])
		out.WriteString("…")
		at = c.to
	}
	out.Write(content[at:])
	return out.String(), nil
}

// diffText is a unified diff of two texts, its hunks alone.
func (g gitRepo) diffText(from, to string) (string, error) {
	dir, err := os.MkdirTemp("", "dispatch-route-adopt-diff-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	fromPath, toPath := filepath.Join(dir, "merge-base"), filepath.Join(dir, "branch")
	if err := os.WriteFile(fromPath, []byte(from), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(toPath, []byte(to), 0o644); err != nil {
		return "", err
	}
	command := g.command("diff", "--no-index", "--no-color", "--exit-code", "-U3", fromPath, toPath)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return "", nil
	case !errors.As(err, &exit) || exit.ExitCode() != 1:
		return "", fmt.Errorf("git diff --no-index: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	text := stdout.String()
	if at := strings.Index(text, "\n@@"); at >= 0 {
		text = text[at+1:]
	}
	return text, nil
}

// staleLines is every line of package api that names a declaration the old layout had, as
// file:line: text, aside from the route files and the files taken whole from the new side, whose
// text is the new layout's.
func staleLines(dir string, taken []string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || isRouteFile(name) || slices.Contains(taken, name) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		number := 0
		for line := range strings.Lines(string(content)) {
			number++
			if oldNames.MatchString(line) {
				lines = append(lines, fmt.Sprintf("%s:%d: %s", path.Join(apiDir, name), number, strings.TrimSpace(line)))
			}
		}
	}
	return lines, nil
}
