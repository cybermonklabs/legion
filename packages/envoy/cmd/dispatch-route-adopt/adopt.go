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

// adopt makes the merge of into and resolves its route layer (see the package comment).
//
// Nothing is merged until both sides are read: a tree this cannot adopt is refused before the
// first write, so a person who runs it on the wrong branch is left where they were rather than
// mid-merge with a refusal.
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
	var oldRev, newRev string
	switch {
	case hereOld && thereOld:
		return nil, fmt.Errorf("the branch and %s both hold the old route table: neither side has the layout to adopt, and nothing is merged", into)
	case !hereOld && !thereOld:
		return nil, fmt.Errorf("the branch and %s both hold the route files already: there is nothing to adopt, and nothing is merged", into)
	case hereOld:
		report.oldSide, report.newSide = "the branch "+here[:12], into+" "+there[:12]
		oldRev, newRev = here, there
	default:
		report.oldSide, report.newSide = into+" "+there[:12], "the branch "+here[:12]
		oldRev, newRev = there, here
	}
	base, err := repo.line("merge-base", here, there)
	if err != nil {
		return nil, err
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
	// Everything this needs is read; now the merge.
	if err := repo.startMerge(into, there); err != nil {
		return nil, err
	}
	// No guard against a second run: startMerge refuses a tree that is mid-merge or holds a
	// change, so this always begins from a tree nobody has touched, and after it the tree is
	// mid-merge. A run over this run's own output cannot happen.
	dir := filepath.Join(repo.root, apiDir)
	var stage, remove []string
	labels := [3]string{"main (" + report.newSide + ")", "merge base", "branch (" + report.oldSide + ")"}
	names := slices.Sorted(maps.Keys(baseFiles))
	for name := range branchFiles {
		if _, inBase := baseFiles[name]; !inBase {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	// merge is one route's three-way merge, recorded in the report: merged where the two sides
	// settle, and in conflict where they do not, which the loop then writes into the index.
	merge := func(name string, ours, base, theirs []byte) ([]byte, error) {
		merged, conflict, err := repo.mergeFile(ours, base, theirs, labels)
		if err != nil {
			return nil, err
		}
		if conflict {
			report.conflicted = append(report.conflicted, name)
		} else {
			report.merged = append(report.merged, name)
		}
		return merged, nil
	}
	for _, name := range names {
		baseContent, inBase := baseFiles[name]
		branchContent, inBranch := branchFiles[name]
		if inBase && inBranch && bytes.Equal(baseContent, branchContent) {
			continue
		}
		file := filepath.Join(dir, name)
		current, err := os.ReadFile(file)
		inTree := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if hasConflictMarkers(current) {
			// An earlier run left this route's conflict for the person: merging it again would nest
			// one conflict inside the other.
			report.conflicted = append(report.conflicted, name)
			continue
		}
		switch {
		case !inBase: // the branch added it
			switch {
			case !inTree:
				report.written = append(report.written, name)
			case bytes.Equal(current, branchContent):
				// Written already: main added the same route, or an earlier run wrote it and stopped
				// before staging. Either way it is staged, so the merge commit holds it.
				stage = append(stage, name)
				continue
			default:
				branchContent, err = merge(name, current, nil, branchContent)
				if err != nil {
					return nil, err
				}
			}
		case !inBranch: // the branch removed it
			switch {
			case !inTree:
				continue
			case bytes.Equal(current, baseContent):
				report.removed = append(report.removed, name)
				remove = append(remove, name)
				continue
			default:
				report.decide = append(report.decide, fmt.Sprintf("the branch removes the route of %s, which main has changed since the merge base: delete it or keep main's", path.Join(apiDir, name)))
				continue
			}
		default: // the branch changed it
			switch {
			case !inTree:
				report.decide = append(report.decide, fmt.Sprintf("main removed the route of %s, which the branch changes: the branch's version is not written", path.Join(apiDir, name)))
				continue
			case bytes.Equal(current, baseContent):
				report.updated = append(report.updated, name)
			default:
				branchContent, err = merge(name, current, baseContent, branchContent)
				if err != nil {
					return nil, err
				}
			}
		}
		if err := os.WriteFile(file, branchContent, 0o644); err != nil {
			return nil, err
		}
		if !slices.Contains(report.conflicted, name) {
			stage = append(stage, name)
			continue
		}
		// A conflict this step cannot resolve is written into the index as one, so git refuses to
		// commit the path until the person resolves it and runs `git add`. Without this, the
		// version in the working tree sits at stage 0, which a plain `git commit` records.
		//
		// The stages are git's own, for a merge of `into` made while the branch is checked out:
		// stage 1 the merge base, stage 2 HEAD's side, which is the branch, and stage 3
		// MERGE_HEAD's, which is what the branch merges. So `git checkout --ours` answers the
		// branch's route and `--theirs` the other side's, as they do for every other path of this
		// merge. Each is the side's own bytes, never the conflict-marked text the working tree
		// holds: the branch's route is branchFiles[name], not branchContent, which merge replaced.
		ours, theirs := branchFiles[name], current
		if oldRev != here {
			// The branch holds the route files and `into` the old table: HEAD is the new side.
			ours, theirs = current, branchFiles[name]
		}
		if err := repo.recordConflict(name, ours, baseContent, theirs); err != nil {
			return nil, err
		}
	}

	for _, name := range []string{tableFile, shapesFile} {
		mainContent, err := gitTree{repo, newRev}.read(name)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), mainContent, 0o644); err != nil {
			return nil, err
		}
		stage = append(stage, name)
		carried, err := sharedCodeChange(repo, name, gitTree{repo, base}, gitTree{repo, oldRev})
		if err != nil {
			return nil, err
		}
		if carried != "" {
			report.carry = append(report.carry, carried)
		}
	}

	if _, err := (gitTree{repo, oldRev}).read(routesFixture); err == nil {
		if _, err := (gitTree{repo, newRev}).read(routesFixture); err != nil {
			remove = append(remove, routesFixture)
			report.fixtureRemoved = true
		}
	}
	if err := repo.stage(stage, remove); err != nil {
		return nil, err
	}

	if report.unmerged, err = repo.unmerged(); err != nil {
		return nil, err
	}
	if report.stale, err = staleLines(dir); err != nil {
		return nil, err
	}
	return report, nil
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
