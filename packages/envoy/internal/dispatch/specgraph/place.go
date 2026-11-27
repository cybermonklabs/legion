package specgraph

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Layout is a spec directory as print changes it: the documents of each file, in file order, and
// Uncounted, the reason for each file whose documents ReadLayout cannot say for certain (a file
// that yaml.v3 reads as UTF-16, a file whose reading stopped at a fault of the YAML, or a file
// whose lines the reader and yaml.v3 count in two ways). The texts of such a file, joined, are
// still the file, but one text can hold more than one document, so print writes nothing while the
// directory holds such a file.
type Layout struct {
	Files     map[string][]Document
	Uncounted map[string]string
}

// Document is one YAML document of a file: its node (nil for a document that gives none) and its
// text, as Print writes it for a node that Place wrote and as the file holds it for a document
// that ReadLayout read.
type Document struct {
	Node Node
	Text string
}

// The refusals of Place. Each *PlaceError wraps one, and names the node and the places or the node
// that the case needs.
var (
	ErrHeldTwice   = errors.New("held two times")
	ErrNoStatement = errors.New("no statement")
	ErrNoParent    = errors.New("no parent")
	ErrTooLarge    = errors.New("too large")
)

// PlaceError is one refusal of Place: Err is ErrHeldTwice, ErrNoStatement, ErrNoParent or
// ErrTooLarge; Node and Kind are the id and the kind of the node it refuses ("statement", "check",
// "component", "interface" or "term"). Needs is the statement or the parent that the directory does
// not hold, Places the first two places of a node held two times ("<file>:<line>"), and Size and
// Limit the bytes of a block that no file holds and the bound.
type PlaceError struct {
	Err         error
	Node, Kind  string
	Needs       string
	Places      []string
	Size, Limit int64
}

func (e *PlaceError) Unwrap() error { return e.Err }

func (e *PlaceError) Error() string {
	switch e.Err {
	case ErrHeldTwice:
		return fmt.Sprintf("node %q is in %s and in %s", Cut(e.Node), e.Places[0], e.Places[1])
	case ErrNoStatement:
		return fmt.Sprintf("check %q serves %q, which the directory does not hold: check-serves: a check serves exactly one statement of the files", Cut(e.Node), Cut(e.Needs))
	case ErrNoParent:
		return fmt.Sprintf("statement %q has the parent %q, which the directory does not hold: no-parent: the files hold no statement %q", Cut(e.Node), Cut(e.Needs), Cut(e.Needs))
	default:
		return fmt.Sprintf("%s %q: its block is %d bytes, more than one file holds (limit %d)", e.Kind, Cut(e.Node), e.Size, e.Limit)
	}
}

// Bytes is the content of each file of l: the texts of its documents, joined.
func (l Layout) Bytes() map[string][]byte {
	files := make(map[string][]byte, len(l.Files))
	for name, documents := range l.Files {
		b := make([]byte, 0, size(documents))
		for _, d := range documents {
			b = append(b, d.Text...)
		}
		files[name] = b
	}
	return files
}

// Place writes the document of node into l, with limit as the bound of one file (print passes
// notes.MaxFileSize), and answers the file that holds the node after the write. On an error, l is
// as it was.
//
// A node that l holds is at home where it stands, and its new document replaces the old one. A node
// that l holds two times has no home (ErrHeldTwice). A new node has its home by its kind: a check,
// the file of the statement that it serves, after that statement and the checks that follow it
// (ErrNoStatement when l holds no such statement); a statement with a parent, the end of its
// parent's file (ErrNoParent when l holds no such statement); a statement with no parent, the end
// of "<id>.yaml"; a component or an interface, the end of "components.yaml"; a term, the end of
// "terms.yaml".
//
// A statement and the checks that directly follow it in its file are one block, and each other
// node is a block of its own. When the home is larger than limit after the write, the node's block
// leaves the home for the end of the first file of the home's series that holds it within limit:
// "<stem>.yaml", "<stem>-2.yaml", "<stem>-3.yaml" and on, the home aside, where a file that l does
// not hold holds any block within limit. A block larger than limit by itself fits in no file
// (ErrTooLarge). So each file stays within limit: the home was within it before the write, and the
// block that leaves holds the new document.
func (l *Layout) Place(node Node, limit int64) (string, error) {
	id := node.NodeID()
	text, err := printNode(node)
	if err != nil {
		return "", err
	}
	refuse := func(kind error) *PlaceError {
		return &PlaceError{Err: kind, Node: id, Kind: node.KindName(), Limit: limit}
	}
	held := l.placesOf(func(n Node) bool { return n.NodeID() == id })
	if len(held) > 1 {
		e := refuse(ErrHeldTwice)
		for _, p := range held[:2] {
			e.Places = append(e.Places, p.file+":"+strconv.Itoa(l.line(p)))
		}
		return "", e
	}
	document := Document{Node: node, Text: text}
	var home string
	var documents []Document
	var at int
	if len(held) == 1 {
		home, at = held[0].file, held[0].index
		documents = slices.Clone(l.Files[home])
		documents[at] = document
	} else {
		var needs error
		home, at, needs = l.home(node)
		if needs != nil {
			e := refuse(needs)
			switch n := node.(type) {
			case Check:
				e.Needs = n.Serves
			case Statement:
				e.Needs = n.Parent
			}
			return "", e
		}
		documents = slices.Insert(slices.Clone(l.Files[home]), at, document)
	}
	if l.Files == nil {
		l.Files = map[string][]Document{}
	}
	if size(documents) <= limit {
		l.Files[home] = documents
		return home, nil
	}
	start, end := blockOf(documents, at)
	block := documents[start:end]
	if n := size(block); n > limit {
		e := refuse(ErrTooLarge)
		e.Size = n
		return "", e
	}
	target := l.nextInSeries(home, size(block), limit)
	l.Files[target] = slices.Concat(l.Files[target], block)
	l.Files[home] = slices.Concat(documents[:start], documents[end:])
	return target, nil
}

// place is where one document stands: its file and its index in the file.
type place struct {
	file  string
	index int
}

// placesOf is each place of l whose node matches, in file then index order.
func (l Layout) placesOf(match func(node Node) bool) []place {
	var places []place
	for _, name := range slices.Sorted(maps.Keys(l.Files)) {
		for i, d := range l.Files[name] {
			if d.Node != nil && match(d.Node) {
				places = append(places, place{name, i})
			}
		}
	}
	return places
}

// line is the line of the file of p at which its document starts, each line as yaml.v3 counts it.
func (l Layout) line(p place) int {
	line := 1
	for _, d := range l.Files[p.file][:p.index] {
		line += len(lineStarts([]byte(d.Text))) - 1
	}
	return line
}

// home is the file and the index where a node that l does not hold goes, or the refusal of a check
// or a statement whose statement or parent l does not hold.
func (l Layout) home(node Node) (string, int, error) {
	statement := func(id string) []place {
		return l.placesOf(func(n Node) bool { s, ok := n.(Statement); return ok && s.ID == id })
	}
	switch n := node.(type) {
	case Check:
		served := statement(n.Serves)
		if len(served) == 0 {
			return "", 0, ErrNoStatement
		}
		p := served[0]
		_, end := blockOf(l.Files[p.file], p.index)
		return p.file, end, nil
	case Statement:
		if n.Parent == "" {
			name := n.ID + FileSuffix
			return name, len(l.Files[name]), nil
		}
		parents := statement(n.Parent)
		if len(parents) == 0 {
			return "", 0, ErrNoParent
		}
		name := parents[0].file
		return name, len(l.Files[name]), nil
	case Component:
		return "components" + FileSuffix, len(l.Files["components"+FileSuffix]), nil
	default:
		return "terms" + FileSuffix, len(l.Files["terms"+FileSuffix]), nil
	}
}

// blockOf is the block of the document at of documents, as [start, end): a statement and the
// checks that directly follow it, or a node alone. A check that follows its statement is in that
// statement's block.
func blockOf(documents []Document, at int) (int, int) {
	serves := func(i int, statement string) bool {
		c, ok := documents[i].Node.(Check)
		return ok && c.Serves == statement
	}
	start := at
	switch n := documents[at].Node.(type) {
	case Statement:
	case Check:
		for start > 0 && serves(start-1, n.Serves) {
			start--
		}
		s, ok := documents[max(start-1, 0)].Node.(Statement)
		if start == 0 || !ok || s.ID != n.Serves {
			return at, at + 1
		}
		start--
	default:
		return at, at + 1
	}
	statement := documents[start].Node.NodeID()
	end := start + 1
	for end < len(documents) && serves(end, statement) {
		end++
	}
	return start, end
}

// nextInSeries is the first file of the series of home, home aside, that holds n more bytes within
// limit: "<stem>.yaml", then "<stem>-2.yaml", "<stem>-3.yaml" and on. The stem of "<stem>-<n>.yaml",
// for n of 2 or more, is <stem>.
func (l Layout) nextInSeries(home string, n, limit int64) string {
	stem := strings.TrimSuffix(home, FileSuffix)
	if i := strings.LastIndex(stem, "-"); i > 0 {
		if k, err := strconv.Atoi(stem[i+1:]); err == nil && k >= 2 && strconv.Itoa(k) == stem[i+1:] {
			stem = stem[:i]
		}
	}
	for k := 1; ; k++ {
		name := stem + FileSuffix
		if k > 1 {
			name = stem + "-" + strconv.Itoa(k) + FileSuffix
		}
		if name == home {
			continue
		}
		if documents, held := l.Files[name]; !held || size(documents)+n <= limit {
			return name
		}
	}
}

// size is the bytes of documents.
func size(documents []Document) int64 {
	var n int64
	for _, d := range documents {
		n += int64(len(d.Text))
	}
	return n
}
