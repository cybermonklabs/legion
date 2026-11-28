package record

import "testing"

// OwnsUnder is the one rule of which issues an architect moves into or out of the workflow: the
// API's rerun_child takes it, and so does the engine's exception for the owning architect's status
// on a child (workflow's sessionStatusWrite). The root's architect owns every issue of its tree. A
// sub-architect owns only the issues under its own issue through their recorded parents, and not
// its own issue, which an architect above it moves. A chain that leaves the tree, or loops back on
// itself, ends at the root (Ancestors), so only the root's architect owns an issue on such a chain.
func TestOwnsUnderIsTheRuleOfTheArchitectsChildOperations(t *testing.T) {
	const (
		root            = "LEGION-1"
		child           = "LEGION-2"
		grandchild      = "LEGION-3" // under child
		greatGrandchild = "LEGION-4" // under grandchild
		sibling         = "LEGION-5"
		nephew          = "LEGION-6" // under sibling
		strayed         = "LEGION-7" // its parent is no issue of the tree
		looped          = "LEGION-8" // its parent's parent is itself
		loop            = "LEGION-9"
	)
	member := func(key, parent string) Issue {
		issue := Issue{Key: key, Tree: root}
		if parent != "" {
			issue.Parent = new(parent)
		}
		return issue
	}
	members := []Issue{
		member(root, ""), member(child, root), member(grandchild, child), member(greatGrandchild, grandchild),
		member(sibling, root), member(nephew, sibling), member(strayed, "OTHER-1"), member(looped, loop), member(loop, looped),
	}
	byKey := byKey(members)
	for _, tc := range []struct {
		name      string
		architect string
		target    string
		owns      bool
	}{
		{name: "the root's architect on a child", architect: root, target: child, owns: true},
		{name: "the root's architect on a grandchild", architect: root, target: grandchild, owns: true},
		{name: "the root's architect on an issue whose parent left the tree", architect: root, target: strayed, owns: true},
		{name: "the root's architect on an issue in a cycle", architect: root, target: looped, owns: true},
		{name: "a sub-architect on its own issue", architect: child, target: child, owns: false},
		{name: "a sub-architect on an issue under it", architect: child, target: grandchild, owns: true},
		{name: "a sub-architect on a grandchild under it", architect: child, target: greatGrandchild, owns: true},
		{name: "a sub-architect on its sibling", architect: child, target: sibling, owns: false},
		{name: "a sub-architect on an issue under its sibling", architect: child, target: nephew, owns: false},
		{name: "a sub-architect on the root", architect: child, target: root, owns: false},
		{name: "a sub-architect on an issue whose parent left the tree", architect: child, target: strayed, owns: false},
		{name: "a sub-architect on an issue in a cycle", architect: child, target: looped, owns: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OwnsUnder(members, root, tc.architect, byKey[tc.target]); got != tc.owns {
				t.Fatalf("OwnsUnder(the architect of %s, %s) = %t, want %t", tc.architect, tc.target, got, tc.owns)
			}
		})
	}
}
