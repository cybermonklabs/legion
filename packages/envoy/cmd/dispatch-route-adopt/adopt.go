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
var oldNames = regexp.MustCompile(`\b(writeClasses|idRoutes|routeScopes|readScopeRoutes|routeShapes|routeRow|withIDRoute|scopedRoutesNotMounted|shapedRoutesNotMounted|readRoutesNotMountedAsGET|routeIndexGolden)\b`)

// adoptReport is what one run did and what it leaves for the person.
type adoptReport struct {
	newSide, oldSide, base string

	written, updated, merged, removed []string
	fixtureRemoved                    bool

	// conflicted are route files the three-way merge left markers in.
	conflicted []string
	// decide are routes main and the branch each changed in a way no merge settles: one side
	// removed the route the other changed.
	decide []string
	// carry are the branch's edits to routes_table.go and routes_shapes.go outside the moved
	// entries, each a diff of that file's shared code from the merge base to the branch.
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
		{"wrote the route the branch added", r.written},
		{"wrote the branch's change to a route main left as the merge base had it", r.updated},
		{"merged the branch's change into main's changed route", r.merged},
		{"removed the route the branch removed", r.removed},
	} {
		for _, name := range list.names {
			fmt.Fprintf(w, "  %s: %s\n", list.what, path.Join(apiDir, name))
		}
	}
	fmt.Fprintf(w, "  took %s and %s from %s\n", path.Join(apiDir, tableFile), path.Join(apiDir, shapesFile), r.newSide)
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

// routeWrite is one file this step writes once the merge is made, with the index stages git
// records for it where its three-way merge is left in conflict.
type routeWrite struct {
	name    string
	content []byte
	// stages are the merge base's, HEAD's and MERGE_HEAD's own bytes, nil for a file the merge
	// settled. They are never the conflict-marked text the working tree holds, so `git checkout
	// --ours` and `--theirs` answer a side a person can use.
	stages *[3][]byte
}

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
	report := &adoptReport{}
	var oldRev, newRev, hereSide, thereSide string
	switch {
	case hereOld && thereOld:
		return nil, fmt.Errorf("the branch and %s both hold the old route table: neither side has the layout to adopt, and nothing is merged", into)
	case !hereOld && !thereOld:
		return nil, fmt.Errorf("the branch and %s both hold the route files already: there is nothing to adopt, and nothing is merged", into)
	case hereOld:
		report.oldSide, report.newSide = "the branch "+here[:12], into+" "+there[:12]
		oldRev, newRev = here, there
		hereSide, thereSide = report.oldSide, report.newSide
	default:
		report.oldSide, report.newSide = into+" "+there[:12], "the branch "+here[:12]
		oldRev, newRev = there, here
		hereSide, thereSide = report.newSide, report.oldSide
	}
	base, err := repo.line("merge-base", here, there)
	if err != nil {
		return nil, fmt.Errorf("the branch and %s share no merge base, so there is no old layout to split their routes from, and nothing is merged (%w)", into, err)
	}
	report.base = base
	if baseOld, err := hasOldLayout(gitTree{repo, base}); err != nil {
		return nil, fmt.Errorf("read the merge base's %s: %w", tableFile, err)
	} else if !baseOld {
		return nil, fmt.Errorf("the merge base %s holds the route files already, so the branch's table is no old layout this can split, and nothing is merged", base)
	}
	baseFiles, err := splitOldLayout(gitTree{repo, base})
	if err != nil {
		return nil, fmt.Errorf("split the merge base's routes: %w", err)
	}
	branchFiles, err := splitOldLayout(gitTree{repo, oldRev})
	if err != nil {
		return nil, fmt.Errorf("split %s's routes: %w", report.oldSide, err)
	}

	newSide := gitTree{repo, newRev}
	var writes []routeWrite
	var remove []string
	// The inputs and labels of every three-way merge are in git's order, HEAD first and
	// MERGE_HEAD last, as the index stages are: an editor's "current change" in a route file is
	// HEAD's, as it is in every file git merged itself.
	labels := [3]string{"HEAD (" + hereSide + ")", "merge base", into + " (" + thereSide + ")"}
	merge := func(name string, newContent, baseContent, oldContent []byte) (routeWrite, error) {
		ours, theirs := oldContent, newContent
		if oldRev != here {
			ours, theirs = newContent, oldContent
		}
		merged, conflict, err := repo.mergeFile(ours, baseContent, theirs, labels)
		if err != nil {
			return routeWrite{}, err
		}
		write := routeWrite{name: name, content: merged}
		if conflict {
			report.conflicted = append(report.conflicted, name)
			write.stages = &[3][]byte{baseContent, ours, theirs}
		} else {
			report.merged = append(report.merged, name)
		}
		return write, nil
	}
	names := slices.Sorted(maps.Keys(baseFiles))
	for name := range branchFiles {
		if _, inBase := baseFiles[name]; !inBase {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		baseContent, inBase := baseFiles[name]
		branchContent, inBranch := branchFiles[name]
		if inBase && inBranch && bytes.Equal(baseContent, branchContent) {
			continue
		}
		// Only the new side holds route files, so the merge leaves each one as the new side holds
		// it: reading it there is reading what the merge will leave, before it is made.
		current, inTree, err := readIfPresent(newSide, name)
		if err != nil {
			return nil, err
		}
		write := routeWrite{name: name, content: branchContent}
		switch {
		case !inBase: // the branch added it
			switch {
			case !inTree:
				report.written = append(report.written, name)
			case bytes.Equal(current, branchContent):
				continue // main added the same route, so the merge holds it already
			default:
				if write, err = merge(name, current, nil, branchContent); err != nil {
					return nil, err
				}
			}
		case !inBranch: // the branch removed it
			switch {
			case !inTree:
			case bytes.Equal(current, baseContent):
				report.removed = append(report.removed, name)
				remove = append(remove, name)
			default:
				report.decide = append(report.decide, fmt.Sprintf("the branch removes the route of %s, which main has changed since the merge base: delete it or keep main's", path.Join(apiDir, name)))
			}
			continue
		default: // the branch changed it
			switch {
			case !inTree:
				report.decide = append(report.decide, fmt.Sprintf("main removed the route of %s, which the branch changes: the branch's version is not written", path.Join(apiDir, name)))
				continue
			case bytes.Equal(current, baseContent):
				report.updated = append(report.updated, name)
			default:
				if write, err = merge(name, current, baseContent, branchContent); err != nil {
					return nil, err
				}
			}
		}
		writes = append(writes, write)
	}
	for _, name := range []string{tableFile, shapesFile} {
		content, err := newSide.read(name)
		if err != nil {
			return nil, err
		}
		writes = append(writes, routeWrite{name: name, content: content})
		carried, err := sharedCodeChange(repo, name, gitTree{repo, base}, gitTree{repo, oldRev})
		if err != nil {
			return nil, err
		}
		if carried != "" {
			report.carry = append(report.carry, carried)
		}
	}
	if _, err := (gitTree{repo, oldRev}).read(routesFixture); err == nil {
		if _, err := newSide.read(routesFixture); err != nil {
			remove = append(remove, routesFixture)
			report.fixtureRemoved = true
		}
	}

	if err := repo.startMerge(into, here, there); err != nil {
		return nil, err
	}
	if err := writeTheMerge(repo, report, writes, remove); err != nil {
		return nil, fmt.Errorf("%w\nthe merge is made and its route layer only partly written: return the tree to the branch with %s, and run this again", err, repo.leaveTheMerge(here, true, true))
	}
	return report, nil
}

// writeTheMerge writes and stages what adopt settled into the merge it made, and reads the merged
// tree for what is left.
func writeTheMerge(repo gitRepo, report *adoptReport, writes []routeWrite, remove []string) error {
	dir := filepath.Join(repo.root, apiDir)
	var stage []string
	for _, write := range writes {
		if err := os.WriteFile(filepath.Join(dir, write.name), write.content, 0o644); err != nil {
			return err
		}
		if write.stages == nil {
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
		if err := repo.recordConflict(write.name, write.stages[1], write.stages[0], write.stages[2]); err != nil {
			return err
		}
	}
	if err := repo.stage(stage, remove); err != nil {
		return err
	}
	var err error
	if report.unmerged, err = repo.unmerged(); err != nil {
		return err
	}
	report.stale, err = staleLines(dir)
	return err
}

// sharedCodeChange is the branch's change to one old-layout file outside the entries the route
// files took over - the rows of routes() and the side maps' entries - as a diff from the merge
// base, and empty where the branch changed nothing else of it.
func sharedCodeChange(repo gitRepo, name string, base, branch tree) (string, error) {
	baseContent, err := base.read(name)
	if err != nil {
		return "", err
	}
	branchContent, err := branch.read(name)
	if err != nil {
		return "", err
	}
	if bytes.Equal(baseContent, branchContent) {
		return "", nil
	}
	baseShared, err := sharedCode(name, baseContent)
	if err != nil {
		return "", fmt.Errorf("read the merge base's %s: %w", name, err)
	}
	branchShared, err := sharedCode(name, branchContent)
	if err != nil {
		return "", fmt.Errorf("read the branch's %s: %w", name, err)
	}
	if baseShared == branchShared {
		return "", nil
	}
	diff, err := repo.diffText(baseShared, branchShared)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("carry by hand the branch's edit to %s outside the moved entries (the moved entries are cut out, as …):\n%s",
		path.Join(apiDir, name), diff), nil
}

// sharedCode is an old-layout file with every moved entry cut out: the elements of the side maps
// and of readScopeRoutes, the rows of routes() and the test hooks' rows, each list left as {…}.
func sharedCode(name string, content []byte) (string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "", err
	}
	type cut struct{ from, to int }
	var cuts []cut
	offset := func(pos token.Pos) int { return fset.Position(pos).Offset }
	for _, mapName := range []string{writeClassesName, idRoutesName, routeScopesName, readScopeRoutesName, routeShapesName} {
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

// staleLines is every line of package api, route files and the two resolved files aside, that
// names a declaration the old layout had, as file:line: text.
func staleLines(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || isRouteFile(name) || name == tableFile || name == shapesFile {
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
