package specgraph

import (
	"slices"
	"testing"
)

// A kept read is the read of one name and one content: the same name with other bytes is read
// again and gives its own node, the same bytes under another name are read again and give their
// node at that name, and the same name and bytes a second time give the kept read itself, the
// documents that the first read gave, so the file is not read again. Each read the cache keeps
// counts fileReadEntryCharge and its name's and content's bytes against the bound, these files
// having no problem. The row reads through a cache of its own, so no other test of the package
// reads into it or empties it. The controls: a key of the name alone gives the second content the
// first content's node; a key of the bytes alone gives the other name's read the first name's
// position; a read that never looks the key up gives new documents on the second read of the same
// name and bytes; a read that counts nothing against the bound leaves the count at 0.
func TestAKeptReadIsTheReadOfItsNameAndItsBytes(t *testing.T) {
	t.Parallel()
	c := fileReadCache{bound: fileReadsBound}
	one := specFile(t, Term{ID: "first-term", Name: "first term", Definition: "A first term is the term of the first file."})
	two := specFile(t, Term{ID: "second-term", Name: "second term", Definition: "A second term is the term of the second file."})
	read := func(name string, data []byte) []Document {
		t.Helper()
		documents, problems, _ := c.read(name, data)
		if len(problems) != 0 || len(documents) != 1 || documents[0].Node == nil {
			t.Fatalf("read(%s): %d documents and the problems %+v; want one document with a node, and no problem", name, len(documents), problems)
		}
		return documents
	}
	kept := read("terms.yaml", one)
	if got := read("terms.yaml", two)[0].Node.NodeID(); got != "second-term" {
		t.Errorf("terms.yaml read again with other bytes gave the node %s; want second-term, the node of its new bytes", got)
	}
	if got := read("other.yaml", one)[0].Node.NodePos(); got.File != "other.yaml" {
		t.Errorf("the bytes of terms.yaml read under the name other.yaml gave a node at %+v; want a node of other.yaml", got)
	}
	if again := read("terms.yaml", one); &again[0] != &kept[0] {
		t.Error("terms.yaml read a second time with the same bytes gave new documents; want the documents its first read kept")
	}
	if len(c.reads) != 3 {
		t.Errorf("the cache holds %d reads; want 3, one for each name and content read", len(c.reads))
	}
	want := 3*fileReadEntryCharge + len("terms.yaml") + len(one) + len("terms.yaml") + len(two) + len("other.yaml") + len(one)
	if c.bytes != want {
		t.Errorf("the cache counts %d bytes against its bound for its three reads; want %d, fileReadEntryCharge and the name's and content's bytes of each", c.bytes, want)
	}
}

// A caller's write into the problems that ReadLayout gave it reaches no later read: each call's
// problems are a list of its own, and the kept read's problems are never handed out. The row reads
// a file with problems through a cache of its own, deletes some of the problems it got and
// rewrites the rest, as versionText's DeleteFunc and a SortProblems do, and reads the file again:
// the second read is the kept one (its documents are the first read's), and its problems are a
// fresh read's. The read counts fileReadProblemCharge for each problem against the bound too.
// The controls: a layout that hands out a file's kept problems when it reads one file gives the
// second read the first caller's writes; a read that counts nothing for its problems counts less.
func TestAWriteIntoTheProblemsOfAReadReachesNoLaterRead(t *testing.T) {
	t.Parallel()
	c := fileReadCache{bound: fileReadsBound}
	name, data := "problems.yaml", []byte("---\n1\n---\n2\n---\n3\n")
	_, fresh, _ := readFile(name, data)
	if len(fresh) < 2 {
		t.Fatalf("readFile(%q) gave the problems %+v; want two or more, or the row proves less than it says", data, fresh)
	}
	first, problems := readLayout(&c, map[string][]byte{name: data})
	for i := range problems {
		problems[i].Detail = "changed by a caller"
	}
	_ = slices.DeleteFunc(problems, func(p Problem) bool { return p.Pos.Line == 1 })
	second, again := readLayout(&c, map[string][]byte{name: data})
	if &second.Files[name][0] != &first.Files[name][0] {
		t.Fatal("the second read of the same file gave new documents; want the kept read, or the row proves less than it says")
	}
	if !slices.Equal(again, fresh) {
		t.Errorf("a read after a caller wrote into its problems gave\n%+v\nwant a fresh read's\n%+v", again, fresh)
	}
	if want := fileReadEntryCharge + len(name) + len(data) + fileReadProblemCharge*len(fresh); c.bytes != want {
		t.Errorf("the cache counts %d bytes against its bound for a read with %d problems; want %d", c.bytes, len(fresh), want)
	}
}

// The reads that a cache keeps are of at most its bound. A read that would take the cache past the
// bound empties it first, and a read over the bound alone is not kept, so a process that reads new
// files without end holds the reads of at most the bound. A second keep of a read the cache holds,
// as when two readers missed the same file, keeps the cache as it is, even at the bound. The
// controls: a cache that never empties holds a, b and c past its bound; one that keeps a read
// over the bound alone holds d; one that empties before it looks for the key holds a alone after
// a's second keep.
func TestTheKeptReadsOfFilesHoldAtMostTheirBound(t *testing.T) {
	t.Parallel()
	c := fileReadCache{bound: 10}
	expect := func(when string, want ...string) {
		t.Helper()
		var got []string
		for _, name := range []string{"a", "b", "c", "d"} {
			if _, kept := c.reads[fileKey{name: name}]; kept {
				got = append(got, name)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: the cache holds %v, want %v", when, got, want)
		}
	}
	c.keep(fileKey{name: "a"}, 6, fileRead{})
	c.keep(fileKey{name: "b"}, 4, fileRead{})
	expect("after 6 and 4 bytes of a bound of 10", "a", "b")
	c.keep(fileKey{name: "a"}, 6, fileRead{})
	expect("after a second keep of a, at the bound", "a", "b")
	c.keep(fileKey{name: "c"}, 2, fileRead{})
	expect("after 2 more bytes, past the bound", "c")
	c.keep(fileKey{name: "d"}, 11, fileRead{})
	expect("after a file of 11 bytes, over the bound alone", "c")
	if c.bytes != 2 {
		t.Errorf("the cache counts %d bytes, want 2: the bytes of c alone", c.bytes)
	}
}
