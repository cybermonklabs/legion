package specgraph

import (
	"path/filepath"
	"reflect"
	"testing"
)

// A statement and its checks that move to another file change nothing: the list reads the nodes,
// not the files that hold them (v-tree).
func TestAMoveToAnotherFileChangesNothing(t *testing.T) {
	t.Parallel()
	base, head := changesPair(t, "move")
	before, _ := base.StatementByID("a-claim-survives-a-restart")
	after, _ := head.StatementByID("a-claim-survives-a-restart")
	if before.Pos.File == after.Pos.File {
		t.Fatalf("the move fixture keeps the statement in %s: it moves nothing", before.Pos.File)
	}
	if got := Changes(base, head); len(got) != 0 {
		t.Errorf("Changes(move) = %+v, want none", got)
	}
}

// One word more in the text of a statement lists that statement, and only it (v-tree).
func TestOneWordChangesTheText(t *testing.T) {
	t.Parallel()
	base, head := changesPair(t, "one-word")
	want := []Change{{ID: "a-claim-survives-a-restart", What: []string{"text"}}}
	if got := Changes(base, head); !reflect.DeepEqual(got, want) {
		t.Errorf("Changes(one-word) = %+v, want %+v", got, want)
	}
}

// A change to the claim of a check is a change of the checks of the statement that it serves.
func TestACheckChangeIsAChangeOfItsStatement(t *testing.T) {
	t.Parallel()
	base, head := changesPair(t, "checks")
	want := []Change{{ID: "a-claim-survives-a-restart", What: []string{"checks"}}}
	if got := Changes(base, head); !reflect.DeepEqual(got, want) {
		t.Errorf("Changes(checks) = %+v, want %+v", got, want)
	}
}

// A new parent, and a new value of kind, serves or about, are changes of the statement. A
// statement whose child changed is not changed itself, and the parts of one change come in the
// order text, parent, relations.
func TestAParentAndARelationChange(t *testing.T) {
	t.Parallel()
	base, head := changesPair(t, "relations")
	want := []Change{
		{ID: "a-claim-survives-a-restart", What: []string{"relations"}},
		{ID: "a-lost-claim-is-reported", What: []string{"text", "parent"}},
	}
	if got := Changes(base, head); !reflect.DeepEqual(got, want) {
		t.Errorf("Changes(relations) = %+v, want %+v", got, want)
	}
}

// A statement that only the head holds is added, one that only the base holds is removed, and each
// relation of kind, serves and about counts.
func TestAddedAndRemoved(t *testing.T) {
	t.Parallel()
	kept := Statement{ID: "kept-statement", Text: "Legion keeps each claim.", Kind: KindBehavior, About: []string{"legion"}}
	check := Check{ID: "a-check", Serves: "kept-statement", Claim: "The daemon keeps each claim."}
	for _, row := range []struct {
		name       string
		base, head Tree
		want       []Change
	}{
		{
			name: "one statement each",
			base: Tree{Statements: []Statement{{ID: "gone-statement", Text: "Legion drops each claim."}, kept}, Checks: []Check{check}},
			head: Tree{Statements: []Statement{kept, {ID: "new-statement", Text: "Legion reports each claim."}}, Checks: []Check{check}},
			want: []Change{{ID: "gone-statement", What: []string{"removed"}}, {ID: "new-statement", What: []string{"added"}}},
		},
		{
			name: "kind",
			base: Tree{Statements: []Statement{kept}},
			head: Tree{Statements: []Statement{with(kept, func(s *Statement) { s.Kind = KindProperty })}},
			want: []Change{{ID: "kept-statement", What: []string{"relations"}}},
		},
		{
			name: "serves",
			base: Tree{Statements: []Statement{kept}},
			head: Tree{Statements: []Statement{with(kept, func(s *Statement) { s.Serves = []string{"other-statement"} })}},
			want: []Change{{ID: "kept-statement", What: []string{"relations"}}},
		},
		{
			name: "about",
			base: Tree{Statements: []Statement{kept}},
			head: Tree{Statements: []Statement{with(kept, func(s *Statement) { s.About = []string{"dispatch", "legion"} })}},
			want: []Change{{ID: "kept-statement", What: []string{"relations"}}},
		},
		{
			name: "a removed check",
			base: Tree{Statements: []Statement{kept}, Checks: []Check{check}},
			head: Tree{Statements: []Statement{kept}},
			want: []Change{{ID: "kept-statement", What: []string{"checks"}}},
		},
		{
			name: "a check that moves to another statement changes both",
			base: Tree{Statements: []Statement{{ID: "another-statement", Text: "Legion reports each claim."}, kept}, Checks: []Check{check}},
			head: Tree{
				Statements: []Statement{{ID: "another-statement", Text: "Legion reports each claim."}, kept},
				Checks:     []Check{with(check, func(c *Check) { c.Serves = "another-statement" })},
			},
			want: []Change{{ID: "another-statement", What: []string{"checks"}}, {ID: "kept-statement", What: []string{"checks"}}},
		},
		{
			name: "every part at once",
			base: Tree{Statements: []Statement{kept}, Checks: []Check{check}},
			head: Tree{Statements: []Statement{with(kept, func(s *Statement) {
				s.Text, s.Parent, s.Kind = "Legion keeps each open claim.", "top-statement", KindProperty
			})}},
			want: []Change{{ID: "kept-statement", What: []string{"text", "parent", "relations", "checks"}}},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := Changes(row.base, row.head); !reflect.DeepEqual(got, row.want) {
				t.Errorf("Changes = %+v, want %+v", got, row.want)
			}
		})
	}
}

// changesPair parses the base and the head of testdata/changes/<name>, and fails the test when
// either is not a valid tree, so that spec-check changes can read the same pair.
func changesPair(t *testing.T, name string) (base, head Tree) {
	t.Helper()
	base = parseDir(t, filepath.Join("testdata/changes", name, "base"))
	head = parseDir(t, filepath.Join("testdata/changes", name, "head"))
	for side, tree := range map[string]Tree{"base": base, "head": head} {
		if problems := Validate(tree, Options{}); len(problems) != 0 {
			t.Fatalf("testdata/changes/%s/%s: %+v", name, side, problems)
		}
	}
	return base, head
}

// with is node after edit, which changes a copy.
func with[T any](node T, edit func(*T)) T {
	edit(&node)
	return node
}
