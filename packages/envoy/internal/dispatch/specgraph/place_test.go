package specgraph

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// placeLimit is the bound of one file in TestPlace.
const placeLimit = 4096

// The nodes of TestPlace: a top statement, a statement under it, two checks and a component and a
// term.
var (
	placeTop   = Statement{ID: "acme-keeps-each-order", Kind: KindBehavior, Text: "ACME keeps each order that a customer sends."}
	placeChild = Statement{ID: "an-order-survives-a-restart", Kind: KindBehavior, Parent: placeTop.ID, Text: "ACME keeps each order after a restart."}
	placeCheck = Check{
		ID: "a-restart-keeps-each-order", Serves: placeTop.ID, Subject: SubjectCode, Procedure: ProcedureAgent,
		Claim: "The order service reads each order after it starts again.", ProcedureText: "An agent reads the orders.", FailingExample: "A service with no store.",
	}
	placeComponent = Component{ID: "orders", Name: "Orders", Purpose: "It keeps each order."}
	placeTerm      = Term{ID: "order", Name: "order", Definition: "An order is a request of a customer."}
)

// placeRow is one row of the table of the cases of Place (its doc comment): a kind of node and
// whether the directory holds its statement or its parent. layout gives the directory before the
// write and the node, new or replaced (the replacement has text more), and the home of the node;
// want is the answer of each column: W writes at the home, M moves the block to the next file of
// the home's series, T, S and P are ErrTooLarge, ErrNoStatement and ErrNoParent.
type placeRow struct {
	name         string
	layout       func(t *testing.T, replaced bool) (l Layout, node Node, home string)
	newWant      [3]string // fits at home; does not fit, block fits alone; block alone over the bound
	replacedWant [3]string
}

func TestPlace(t *testing.T) {
	t.Parallel()
	other := func(t *testing.T) Layout {
		return Layout{Files: map[string][]Document{"other.yaml": {placeDocument(t, Term{ID: "other", Name: "other", Definition: "Another term."})}}}
	}
	files := func(t *testing.T, home string, nodes ...Node) Layout {
		var documents []Document
		for _, n := range nodes {
			documents = append(documents, placeDocument(t, n))
		}
		return Layout{Files: map[string][]Document{home: documents}}
	}
	childCheck := placeCheck
	childCheck.ID, childCheck.Serves = "a-crash-keeps-each-order", placeChild.ID
	secondCheck := placeCheck
	secondCheck.ID = "a-crash-keeps-each-order"
	rows := []placeRow{
		{
			name: "check, the directory holds its statement",
			layout: func(t *testing.T, replaced bool) (Layout, Node, string) {
				l := files(t, "acme-keeps-each-order.yaml", placeTop, placeCheck)
				if replaced {
					return l, more(placeCheck), "acme-keeps-each-order.yaml"
				}
				return l, secondCheck, "acme-keeps-each-order.yaml"
			},
			newWant: [3]string{"W", "M", "T"}, replacedWant: [3]string{"W", "M", "T"},
		},
		{
			name: "check, the directory does not hold its statement",
			layout: func(t *testing.T, replaced bool) (Layout, Node, string) {
				if replaced {
					return files(t, "orders.yaml", placeCheck), more(placeCheck), "orders.yaml"
				}
				return other(t), placeCheck, ""
			},
			newWant: [3]string{"S", "S", "S"}, replacedWant: [3]string{"W", "M", "T"},
		},
		{
			name: "statement, the directory holds its parent",
			layout: func(t *testing.T, replaced bool) (Layout, Node, string) {
				if replaced {
					return files(t, "acme-keeps-each-order.yaml", placeTop, placeCheck, placeChild, childCheck), more(placeChild), "acme-keeps-each-order.yaml"
				}
				return files(t, "acme-keeps-each-order.yaml", placeTop, placeCheck), placeChild, "acme-keeps-each-order.yaml"
			},
			newWant: [3]string{"W", "M", "T"}, replacedWant: [3]string{"W", "M", "T"},
		},
		{
			name: "statement, the directory does not hold its parent",
			layout: func(t *testing.T, replaced bool) (Layout, Node, string) {
				if replaced {
					return files(t, "orders.yaml", placeChild, childCheck), more(placeChild), "orders.yaml"
				}
				return other(t), placeChild, ""
			},
			newWant: [3]string{"P", "P", "P"}, replacedWant: [3]string{"W", "M", "T"},
		},
		{
			name: "statement with no parent",
			layout: func(t *testing.T, replaced bool) (Layout, Node, string) {
				if replaced {
					return files(t, "acme-keeps-each-order.yaml", placeTop, placeCheck), more(placeTop), "acme-keeps-each-order.yaml"
				}
				return files(t, "acme-keeps-each-order.yaml"), placeTop, "acme-keeps-each-order.yaml"
			},
			newWant: [3]string{"W", "M", "T"}, replacedWant: [3]string{"W", "M", "T"},
		},
		{
			name: "component or interface",
			layout: func(t *testing.T, replaced bool) (Layout, Node, string) {
				if replaced {
					return files(t, "components.yaml", placeComponent), more(placeComponent), "components.yaml"
				}
				return files(t, "components.yaml"), placeComponent, "components.yaml"
			},
			newWant: [3]string{"W", "M", "T"}, replacedWant: [3]string{"W", "M", "T"},
		},
		{
			name: "term",
			layout: func(t *testing.T, replaced bool) (Layout, Node, string) {
				if replaced {
					return files(t, "terms.yaml", placeTerm), more(placeTerm), "terms.yaml"
				}
				return files(t, "terms.yaml"), placeTerm, "terms.yaml"
			},
			newWant: [3]string{"W", "M", "T"}, replacedWant: [3]string{"W", "M", "T"},
		},
	}
	cells := 0
	for _, row := range rows {
		for _, replaced := range []bool{false, true} {
			wants := row.newWant
			if replaced {
				wants = row.replacedWant
			}
			for column, want := range wants {
				cells++
				age := map[bool]string{false: "new", true: "replaced"}[replaced]
				name := fmt.Sprintf("%s, %s, %s", row.name, age, [3]string{"fits", "does not fit, block fits alone", "block alone over the bound"}[column])
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					l, node, home := row.layout(t, replaced)
					if column == 2 {
						node = huge(node)
					}
					if home != "" {
						target := int64(1000)
						if column == 1 {
							target = placeLimit + 1 - writeDelta(t, l, node)
						}
						l.Files[home] = fillTo(t, l.Files[home], target, "filler")
					}
					expectPlace(t, l, node, home, want)
				})
			}
		}
	}
	if cells != 42 {
		t.Errorf("the table has %d cells, want 42", cells)
	}

	t.Run("a node that the directory holds two times", func(t *testing.T) {
		t.Parallel()
		l := Layout{Files: map[string][]Document{"a.yaml": {placeDocument(t, placeTerm)}, "b.yaml": {placeDocument(t, placeTerm)}}}
		before := cloneLayout(l)
		_, err := l.Place(more(placeTerm), placeLimit)
		if !errors.Is(err, ErrHeldTwice) || !strings.Contains(err.Error(), "a.yaml:1") || !strings.Contains(err.Error(), "b.yaml:1") {
			t.Errorf("Place = %v, want ErrHeldTwice naming a.yaml:1 and b.yaml:1", err)
		}
		sameLayout(t, l, before)
	})
	t.Run("a block that grows in terms-2.yaml while terms.yaml has room moves to terms.yaml", func(t *testing.T) {
		t.Parallel()
		l := Layout{Files: map[string][]Document{
			"terms.yaml":   fillTo(t, nil, 500, "filler"),
			"terms-2.yaml": {placeDocument(t, placeTerm)},
		}}
		l.Files["terms-2.yaml"] = fillTo(t, l.Files["terms-2.yaml"], placeLimit+1-writeDelta(t, l, more(placeTerm)), "padding")
		file, err := l.Place(more(placeTerm), placeLimit)
		if err != nil || file != "terms.yaml" {
			t.Fatalf("Place = %q, %v, want terms.yaml", file, err)
		}
		holdsEachNodeOnce(t, l)
	})
}

// expectPlace places node into l and holds the answer to want: W, the home; M, the file after the
// home in its series, each of the two within the bound; T, S or P, the refusal and l unchanged.
func expectPlace(t *testing.T, l Layout, node Node, home, want string) {
	t.Helper()
	before := cloneLayout(l)
	file, err := l.Place(node, placeLimit)
	refusals := map[string]error{"T": ErrTooLarge, "S": ErrNoStatement, "P": ErrNoParent}
	switch want {
	case "W", "M":
		wantFile := home
		if want == "M" {
			wantFile = strings.TrimSuffix(home, FileSuffix) + "-2" + FileSuffix
		}
		if err != nil || file != wantFile {
			t.Fatalf("Place = %q, %v, want %s (%s)", file, err, wantFile, want)
		}
		for _, name := range []string{home, wantFile} {
			if n := size(l.Files[name]); n > placeLimit {
				t.Errorf("%s is %d bytes, more than %d", name, n, placeLimit)
			}
		}
		holdsEachNodeOnce(t, l)
	default:
		if !errors.Is(err, refusals[want]) {
			t.Fatalf("Place = %q, %v, want %v", file, err, refusals[want])
		}
		sameLayout(t, l, before)
	}
}

// placeDocument is the document of node, as Print writes it.
func placeDocument(t *testing.T, node Node) Document {
	t.Helper()
	text, err := printNode(node)
	if err != nil {
		t.Fatal(err)
	}
	return Document{Node: node, Text: text}
}

// more is node with one more word in its text.
func more(node Node) Node {
	switch n := node.(type) {
	case Statement:
		n.Text += " now"
		return n
	case Check:
		n.ProcedureText += " now"
		return n
	case Component:
		n.Purpose += " now"
		return n
	case Term:
		n.Definition += " now"
		return n
	}
	return node
}

// huge is node with a text of more than placeLimit bytes, so its block is over the bound alone.
func huge(node Node) Node {
	text := strings.Repeat("order ", placeLimit/5)
	switch n := node.(type) {
	case Statement:
		n.Text = text
		return n
	case Check:
		n.ProcedureText = text
		return n
	case Component:
		n.Purpose = text
		return n
	case Term:
		n.Definition = text
		return n
	}
	return node
}

// writeDelta is how many bytes the write of node adds to its home in l: its document, less the
// document that it replaces.
func writeDelta(t *testing.T, l Layout, node Node) int64 {
	t.Helper()
	delta := int64(len(placeDocument(t, node).Text))
	for _, p := range l.placesOf(func(n Node) bool { return n.NodeID() == node.NodeID() }) {
		delta -= int64(len(l.Files[p.file][p.index].Text))
	}
	return delta
}

// fillTo is documents with term documents after them, so that they are exactly target bytes; the
// ids of those terms start with prefix.
func fillTo(t *testing.T, documents []Document, target int64, prefix string) []Document {
	t.Helper()
	documents = slices.Clone(documents)
	for i := 0; ; i++ {
		id := fmt.Sprintf("%s-%c%c", prefix, 'a'+i/26, 'a'+i%26)
		base := int64(len(placeDocument(t, Term{ID: id, Name: "filler", Definition: "x"}).Text)) - 1
		rest := target - size(documents)
		switch {
		case rest-base > 400:
			documents = append(documents, placeDocument(t, Term{ID: id, Name: "filler", Definition: strings.Repeat("x", 200)}))
		case rest-base >= 1:
			documents = append(documents, placeDocument(t, Term{ID: id, Name: "filler", Definition: strings.Repeat("x", int(rest-base))}))
			if n := size(documents); n != target {
				t.Fatalf("filled to %d bytes, want %d", n, target)
			}
			return documents
		default:
			t.Fatalf("the documents are %d bytes, too many to fill to %d", size(documents), target)
		}
	}
}

// holdsEachNodeOnce fails the test when a node of l stands in two places.
func holdsEachNodeOnce(t *testing.T, l Layout) {
	t.Helper()
	seen := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(l.Files)) {
		for _, d := range l.Files[name] {
			id := idOf(d.Node)
			if at, twice := seen[id]; twice {
				t.Errorf("node %q is in %s and in %s", id, at, name)
			}
			seen[id] = name
		}
	}
}

// cloneLayout is a copy of l that a change of l does not change.
func cloneLayout(l Layout) Layout {
	c := Layout{Files: make(map[string][]Document, len(l.Files))}
	for name, documents := range l.Files {
		c.Files[name] = slices.Clone(documents)
	}
	return c
}

// sameLayout fails the test when l is not before.
func sameLayout(t *testing.T, l, before Layout) {
	t.Helper()
	if !maps.EqualFunc(l.Files, before.Files, func(a, b []Document) bool {
		return slices.EqualFunc(a, b, func(x, y Document) bool { return x.Text == y.Text && idOf(x.Node) == idOf(y.Node) })
	}) {
		t.Errorf("the layout changed on a refusal:\n got %v\nwant %v", l.Bytes(), before.Bytes())
	}
}

// idOf is the id of n, "" for a document that gives no node.
func idOf(n Node) string {
	if n == nil {
		return ""
	}
	return n.NodeID()
}
