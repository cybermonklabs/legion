package pmdoc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wire encoding of a display tree leaves out only what the reader puts back. Every document
// of both corpora is read in both modes, encoded as a response carries it, decoded, and filled
// as the browser's schema fills a node it is handed no attribute for (ProseMirror's
// NodeType.create, which documentFromJSON calls): the result must be the tree the read made,
// attribute for attribute.
func TestTheDisplayEncodingLeavesOutOnlyWhatTheSchemaFillsBack(t *testing.T) {
	for _, path := range displayEncodingCorpus(t) {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		for _, mode := range []struct {
			name string
			mode DisplayMode
		}{{"latest", DisplayLatest}, {"version", DisplayVersion}} {
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				want := ReadForDisplay(string(markdown), mode.mode)
				encoded, err := json.Marshal(want.toDisplayJSON())
				if err != nil {
					t.Fatalf("encode the display tree: %v", err)
				}
				got, err := FromJSON(encoded)
				if err != nil {
					t.Fatalf("decode the display tree: %v", err)
				}
				fillSchemaDefaults(got)
				if !got.EqualWithBlockIDs(want) {
					wanted, _ := want.JSON()
					t.Errorf("the encoding filled back is\n\t%s\nwant the display read's\n\t%s", encoded, wanted)
				}
			})
		}
	}
}

// The encoding really does leave those attributes out: a plain bullet list carries no minted
// block id, no list marker the schema already knows and no spread the schema already knows,
// and keeps the one attribute whose value is not its default (a tight item's `spread`).
func TestTheDisplayEncodingWritesNoDefaultAttribute(t *testing.T) {
	encoded, err := json.Marshal(ReadForDisplay("# Title\n\n- one\n- two\n", DisplayLatest).toDisplayJSON())
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{`"blockId"`, `"label"`, `"listType"`, `"checked"`, `"id":""`, `"level":1`} {
		if strings.Contains(string(encoded), gone) {
			t.Errorf("the encoding still writes %s:\n\t%s", gone, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"spread":false`) {
		t.Errorf("the encoding dropped a tight list item's spread, which the schema defaults to true:\n\t%s", encoded)
	}
}

// A typed block is written whole, every attribute standing at its schema's default included: its
// schema can be installed over while a tree stored under it is still served (display_encode.go),
// so none of it is left for the reader's schema to fill back.
func TestTheDisplayEncodingWritesATypedBlockWhole(t *testing.T) {
	encoded, err := json.Marshal(ReadForDisplay(":::callout{kind=\"note\"}\nBody.\n:::\n", DisplayLatest).toDisplayJSON())
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{`"kind":"note"`, `"title":""`} {
		if !strings.Contains(string(encoded), kept) {
			t.Errorf("the encoding left %s out of a callout, which the callout schema defaults:\n\t%s", kept, encoded)
		}
	}
}

// displayEncodingCorpus is every document both corpora hold: the writer's and the display
// read's own.
func displayEncodingCorpus(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, corpus := range []string{"corpus", "corpus-display"} {
		found, err := filepath.Glob(filepath.Join("testdata", corpus, "*.md"))
		if err != nil || len(found) == 0 {
			t.Fatalf("testdata/%s: %d documents, %v", corpus, len(found), err)
		}
		paths = append(paths, found...)
	}
	return paths
}

// fillSchemaDefaults gives every node and mark of tree the attributes the encoding left out, as
// the browser's schema gives them to a node built with none: the declared default of each
// attribute the object does not name. It fills from the tables the encoding leaves out by, so the
// two read one list; a typed block is in none of them, being written whole.
func fillSchemaDefaults(tree *Node) {
	Walk(tree, func(node *Node) bool {
		node.Attrs = filledAttrs(node.Attrs, displayNodeDefaults[node.Type])
		for index := range node.Marks {
			node.Marks[index].Attrs = filledAttrs(node.Marks[index].Attrs, displayMarkDefaults[node.Marks[index].Type])
		}
		return true
	})
}

// filledAttrs is attrs with every default it does not name written into it.
func filledAttrs(attrs, defaults Attrs) Attrs {
	for name, value := range defaults {
		if _, named := attrs[name]; named {
			continue
		}
		if attrs == nil {
			attrs = make(Attrs, len(defaults))
		}
		attrs[name] = value
	}
	return attrs
}
