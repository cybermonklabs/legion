package specgraph

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// A read of a file that readFile kept gives what the first read gave, and what a caller does to
// the layout it got changes no later read. The row changes, in place, the list of a statement
// (About), the list of an interface (Joins) and the test part of a check, which the fixture's files
// hold, and places a new term into the layout, as print does. A later read is the read from before
// the changes, byte for byte as JSON. The control is a read handed out as it was kept, sharing its
// lists, its test parts and its documents with the cache: the later read then holds the changes.
func TestAReadOfAKeptFileIsTheReadBeforeAnyCallerChangedIt(t *testing.T) {
	t.Parallel()
	files := readFiles(t, "testdata/compare/approved-text/spec")
	first, problems := ReadLayout(files)
	if len(problems) != 0 {
		t.Fatalf("ReadLayout(approved-text): %+v, want no problem", problems)
	}
	second, _ := ReadLayout(files)
	before, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if after, err := json.Marshal(first); err != nil || string(after) != string(before) {
		t.Fatalf("two reads of the same files differ (%v):\n%s\n%s", err, after, before)
	}
	changed := map[string]bool{}
	for _, documents := range first.Files {
		for _, d := range documents {
			switch n := d.Node.(type) {
			case Statement:
				if len(n.About) > 0 {
					n.About[0], changed["about"] = "changed-by-a-caller", true
				}
			case Component:
				if len(n.Joins) > 0 {
					n.Joins[0], changed["joins"] = "changed-by-a-caller", true
				}
			case Check:
				if n.Test != nil {
					n.Test.Selector, changed["test"] = "example.com/acme/changed TestChangedByACaller", true
				}
			}
		}
	}
	if len(changed) != 3 {
		t.Fatalf("the fixture gave the row %v to change; want about, joins and test, or the row proves less than it says", changed)
	}
	term := Term{ID: "a-placed-term", Name: "placed term", Definition: "A placed term is a term that a caller placed."}
	if _, err := first.Place(term, notes.MaxFileSize); err != nil {
		t.Fatalf("Place(%s): %v", term.ID, err)
	}
	third, _ := ReadLayout(files)
	if after, err := json.Marshal(third); err != nil || string(after) != string(before) {
		t.Errorf("a read after a caller changed its layout is not the read from before (%v):\n got %s\nwant %s", err, after, before)
	}
}

// The reads that fileReads keeps are of at most its bound of bytes of files, a name's bytes
// counted with its content's. A read that would take the cache past the bound empties it first,
// and a file over the bound alone is not kept, so a process that reads new files without end
// holds the reads of at most the bound.
func TestTheKeptReadsOfFilesHoldAtMostTheirBound(t *testing.T) {
	t.Parallel()
	c := fileReadCache{bound: 10}
	expect := func(when string, want ...string) {
		t.Helper()
		var got []string
		for _, key := range []fileKey{{"a", "12345"}, {"b", "123"}, {"c", "1"}, {"d", "1234567890"}} {
			if _, ok := c.load(key); ok {
				got = append(got, key.name)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: the cache holds %v, want %v", when, got, want)
		}
	}
	c.store(fileKey{"a", "12345"}, fileRead{})
	c.store(fileKey{"b", "123"}, fileRead{})
	expect("after 6 and 4 bytes of a bound of 10", "a", "b")
	c.store(fileKey{"c", "1"}, fileRead{})
	expect("after 2 more bytes, past the bound", "c")
	c.store(fileKey{"d", "1234567890"}, fileRead{})
	expect("after a file of 11 bytes, over the bound alone", "c")
	if c.bytes != 2 {
		t.Errorf("the cache counts %d bytes, want 2: the bytes of c alone", c.bytes)
	}
}
