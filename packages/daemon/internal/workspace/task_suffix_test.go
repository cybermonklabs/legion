package workspace

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// besideWorkingCopies says, for each suffix constant of this package (a constant named ...Suffix),
// whether the package puts it on the directory of a working copy, whose siblings are the working
// copies of the other tasks of its issue.
var besideWorkingCopies = map[string]bool{
	// Remove renames a working copy aside to its directory and this suffix (removingPath).
	"removingSuffix": true,
	// The temporary directory a feed repository is filled in, beside the feed, where no working
	// copy is (fetch.go).
	"cloneSuffix": false,
}

// The working copy of each task of an issue is a directory beside the issue's own, named for the
// task's branch without legion/ (`<key>` or `<key>.<part>`), and this package renames a working copy
// it removes to a sibling, its directory and a suffix. A task whose branch were another task's
// branch and that suffix would have its working copy at the other's renamed path, and the removal
// of the other would delete the task's work. A task with several needs joins them on a branch of
// its own beside its branch, its branch and claim.JoinBranchSuffix, and a task whose branch were
// that join branch would work on the other's join. So claim.BranchTask takes no such branch, from
// the issue's own branch, a slug's first give or a later give. Each suffix constant is read from
// this package's source, so a new one is either put to this rule or named in besideWorkingCopies
// as one that lands beside no working copy, and the join suffix is read from claim, its one
// source.
func TestNoTaskBranchIsAnotherWithASuffixOfAWorkingCopy(t *testing.T) {
	suffixes := suffixConstants(t)
	if len(suffixes) == 0 {
		t.Fatal("read no suffix constant from this package's source: the read is blind, not clean")
	}
	checked := map[string]string{"claim.JoinBranchSuffix": claim.JoinBranchSuffix}
	for name, suffix := range suffixes {
		beside, known := besideWorkingCopies[name]
		if !known {
			t.Errorf("%s (%q) is a suffix of this package that besideWorkingCopies does not name: say whether it lands beside a working copy", name, suffix)
			continue
		}
		if beside {
			checked[name] = suffix
		}
	}
	for name, suffix := range checked {
		if part := strings.TrimPrefix(suffix, "."); claim.IsTaskSlug(part) {
			t.Errorf("claim.IsTaskSlug takes %q, which %s puts beside a task's branch or working copy", part, name)
		}
		for _, branch := range []string{claim.IssueBranch("ACME-1"), claim.TaskBranch("ACME-1", "d"), claim.TaskBranchOfGive("ACME-1", "d", 2)} {
			if _, part, ok := claim.BranchTask(branch + suffix); ok {
				t.Errorf("claim.BranchTask takes %q (task part %q), which is %s with the suffix %s", branch+suffix, part, branch, name)
			}
		}
	}
}

// suffixConstants is each string constant of this package, outside its tests, whose name ends in
// Suffix, by name.
func suffixConstants(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list this package's source: %v", err)
	}
	suffixes := map[string]string{}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if !strings.HasSuffix(name.Name, "Suffix") || i >= len(spec.Values) {
					continue
				}
				literal, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s in %s is no string literal: read its value by hand", name.Name, file)
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote %s in %s: %v", name.Name, file, err)
				}
				suffixes[name.Name] = value
			}
			return true
		})
	}
	return suffixes
}
