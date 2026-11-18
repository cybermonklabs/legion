package specgraph

import (
	"slices"
	"testing"
)

// The tree finds each node by its id, the checks that serve a statement and the statements under
// it, and answers false for an id it does not hold.
func TestTreeFindsNodes(t *testing.T) {
	t.Parallel()
	tree := Tree{
		Statements: []Statement{{ID: "child-one", Parent: "top"}, {ID: "child-two", Parent: "top"}, {ID: "top"}},
		Checks:     []Check{{ID: "check-one", Serves: "child-one"}, {ID: "check-two", Serves: "child-one"}, {ID: "check-three", Serves: "child-two"}},
		Components: []Component{{ID: "envoy", Name: "Envoy"}},
	}
	if s, ok := tree.StatementByID("child-two"); !ok || s.Parent != "top" {
		t.Errorf("StatementByID(child-two) = %+v, %t", s, ok)
	}
	if _, ok := tree.StatementByID("missing"); ok {
		t.Error("StatementByID(missing) found a statement")
	}
	if c, ok := tree.CheckByID("check-three"); !ok || c.Serves != "child-two" {
		t.Errorf("CheckByID(check-three) = %+v, %t", c, ok)
	}
	if _, ok := tree.CheckByID("missing"); ok {
		t.Error("CheckByID(missing) found a check")
	}
	if c, ok := tree.ComponentByID("envoy"); !ok || c.Name != "Envoy" {
		t.Errorf("ComponentByID(envoy) = %+v, %t", c, ok)
	}
	if _, ok := tree.ComponentByID("missing"); ok {
		t.Error("ComponentByID(missing) found a component")
	}
	if got := ids(tree.ChecksOf("child-one"), func(c Check) string { return c.ID }); !slices.Equal(got, []string{"check-one", "check-two"}) {
		t.Errorf("ChecksOf(child-one) = %q", got)
	}
	if got := tree.ChecksOf("top"); len(got) != 0 {
		t.Errorf("ChecksOf(top) = %+v, want none", got)
	}
	if got := ids(tree.Children("top"), func(s Statement) string { return s.ID }); !slices.Equal(got, []string{"child-one", "child-two"}) {
		t.Errorf("Children(top) = %q", got)
	}
	if got := tree.Children("child-one"); len(got) != 0 {
		t.Errorf("Children(child-one) = %+v, want none", got)
	}
}

// ids is the id of each node, in order.
func ids[T any](nodes []T, id func(T) string) []string {
	var all []string
	for _, node := range nodes {
		all = append(all, id(node))
	}
	return all
}
