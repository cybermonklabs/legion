package specgraph

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// ApprovedVersion is one text that the approved spec held for a node (R2: "Dispatch keeps each
// text that the approved spec held"): its version number, the time of the approval that wrote it,
// in approved.json an RFC 3339 time as time.RFC3339Nano writes it, with an upper-case T and Z, and
// Block, the node's block as Print writes it, which a change copies into the files.
type ApprovedVersion struct {
	Version int       `json:"version"`
	At      time.Time `json:"at"`
	Block   string    `json:"block"`
}

// ApprovedNode is one statement, check, component, interface or term of the approved spec, with
// each text that it held, oldest first. The newest version is its current text, unless the
// approved spec removed the node: a removed node has no current text, and its id is never used
// for another text (R1). PartlyChecked marks a statement whose checks decide only part of it, as
// the reviewer of its level judged (R10).
type ApprovedNode struct {
	ID            string            `json:"id"`
	Removed       bool              `json:"removed"`
	PartlyChecked bool              `json:"partly_checked"`
	Versions      []ApprovedVersion `json:"versions"`
}

// Approved is the approved spec (R2): each node that it holds or held, by id.
type Approved struct {
	Nodes map[string]ApprovedNode
}

// ApprovedFile is approved.json, the approved spec as a file holds it, and the one definition of
// that form (approvedForm): WriteApproved writes it, and ReadApproved reads it. "at" is an RFC 3339
// time as time.RFC3339Nano writes it, with an upper-case T and Z, such as "2026-11-10T00:00:00Z":
// the reader refuses a lower-case t or z, which RFC 3339 allows, and the writer writes upper case.
// An approved spec that holds no node is {"nodes":[]}.
type ApprovedFile struct {
	Nodes []ApprovedNode `json:"nodes"`
}

// timeForm is a time as each form text of this package names its type: an RFC 3339 time as
// time.RFC3339Nano writes it, which the readers take only with an upper-case T and Z.
const timeForm = `<RFC 3339 time with an upper-case T and Z>`

// approvedForm is the form of approved.json, with the type of each value, as ReadApproved names it
// for a value of another type.
const approvedForm = `{"nodes":[{"id":"<id>","removed":<true or false>,"partly_checked":<true or false>,"versions":[{"version":<whole number from 1>,"at":"` + timeForm + `","block":"<block>"}, …]}, …]}`

// ReadApproved reads an approved spec in the form of approved.json (ApprovedFile). It refuses
// what decodeJSON refuses (a byte that is not UTF-8, a field it does not know or that is written
// in other letters, a field that one object gives two times, a value of another type than
// approvedForm needs, such as a time that is not RFC 3339, named by its place, no object or text
// after it), a file with no list of nodes (null, {} or "nodes": null), a node with no id, an id on
// two nodes, a node with no version, version numbers that do not go up from 1, a version with no
// time or with one older than the one before it, and a block that is not its node's one block as
// Print writes it. The error names the node, by its id as quoted prints a value of the input, and
// the version. It reads the whole file, with no bound on its size.
func ReadApproved(r io.Reader) (Approved, error) {
	var file ApprovedFile
	if err := decodeJSONFrom(r, &file, approvedForm); err != nil {
		return Approved{}, fmt.Errorf("specgraph: approved spec: %w", err)
	}
	return file.approved()
}

// WriteApproved writes a to w as approved.json (ApprovedFile), the nodes in the order of their ids,
// and a line break after it, so ReadApproved reads back a and two writes of a give the same bytes.
// For an approved spec that ReadApproved would refuse, such as a node under another id than its
// own, it writes nothing and answers the fault.
func WriteApproved(w io.Writer, a Approved) error {
	file := ApprovedFile{Nodes: make([]ApprovedNode, 0, len(a.Nodes))}
	for _, id := range slices.Sorted(maps.Keys(a.Nodes)) {
		n := a.Nodes[id]
		if n.ID != id {
			return fmt.Errorf("specgraph: approved spec: node %s is under the id %s", quoted(n.ID), quoted(id))
		}
		file.Nodes = append(file.Nodes, n)
	}
	if _, err := file.approved(); err != nil {
		return err
	}
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("specgraph: approved spec: %w", err)
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// approved is the approved spec that f holds, or the first fault of f that ReadApproved refuses.
func (f ApprovedFile) approved() (Approved, error) {
	if f.Nodes == nil {
		return Approved{}, errors.New(`specgraph: approved spec: it holds no "nodes" list`)
	}
	a := Approved{Nodes: make(map[string]ApprovedNode, len(f.Nodes))}
	for _, n := range f.Nodes {
		if n.ID == "" {
			return Approved{}, errors.New("specgraph: approved spec: a node has no id")
		}
		if _, twice := a.Nodes[n.ID]; twice {
			return Approved{}, fmt.Errorf("specgraph: approved spec: node %s is in the approved spec two times", quoted(n.ID))
		}
		if err := n.check(); err != nil {
			return Approved{}, err
		}
		a.Nodes[n.ID] = n
	}
	return a, nil
}

// check is the first fault of n that ReadApproved refuses, or nil.
func (n ApprovedNode) check() error {
	if len(n.Versions) == 0 {
		return fmt.Errorf("specgraph: approved node %s: it has no version", quoted(n.ID))
	}
	for i, v := range n.Versions {
		fault := ""
		switch {
		case v.Version < 1 || i > 0 && v.Version <= n.Versions[i-1].Version:
			fault = "the versions are 1 and more, oldest first"
		case v.At.IsZero():
			fault = "it has no time"
		case i > 0 && v.At.Before(n.Versions[i-1].At):
			fault = fmt.Sprintf("it is older than version %d", n.Versions[i-1].Version)
		default:
			if _, problems := versionText(n.ID, v); len(problems) != 0 {
				fault = fmt.Sprintf("%s (line %d of its block)", problems[0].Detail, problems[0].Pos.Line)
			}
		}
		if fault != "" {
			return fmt.Errorf("specgraph: approved node %s version %d: %s", quoted(n.ID), v.Version, fault)
		}
	}
	return nil
}

// Current is the tree of the current text of each node that the approved spec did not remove.
// Each node is at the position "approved <id> version <n>", line 1. A caller that builds an
// Approved itself, as the pilot does from the store, can give a block that ReadApproved would
// refuse: Current leaves that node out and gives each problem of its block.
func (a Approved) Current() (Tree, []Problem) {
	var tree Tree
	var problems []Problem
	for id, n := range a.Nodes {
		if n.Removed || len(n.Versions) == 0 {
			continue
		}
		text, refused := versionText(id, n.Versions[len(n.Versions)-1])
		problems = append(problems, refused...)
		tree.Statements = append(tree.Statements, text.Statements...)
		tree.Checks = append(tree.Checks, text.Checks...)
		tree.Components = append(tree.Components, text.Components...)
		tree.Terms = append(tree.Terms, text.Terms...)
	}
	slices.SortFunc(tree.Statements, func(x, y Statement) int { return cmp.Compare(x.ID, y.ID) })
	slices.SortFunc(tree.Checks, func(x, y Check) int { return cmp.Compare(x.ID, y.ID) })
	slices.SortFunc(tree.Components, func(x, y Component) int { return cmp.Compare(x.ID, y.ID) })
	slices.SortFunc(tree.Terms, func(x, y Term) int { return cmp.Compare(x.ID, y.ID) })
	SortProblems(problems)
	return tree, problems
}

// Since is the time of the approval that wrote the current text of the node id, and the zero time
// when the approved spec does not hold id or removed it.
func (a Approved) Since(id string) time.Time {
	n, held := a.Nodes[id]
	if !held || n.Removed || len(n.Versions) == 0 {
		return time.Time{}
	}
	return n.Versions[len(n.Versions)-1].At
}

// versionText is the tree of the one node that version v of the approved node id holds, at the
// position "approved <id> version <n>". problems is each reason it holds none: a line that Parse
// refuses, or bad-approved-block for a block of no node or of two, of a node with another id, or
// not as Print writes its node.
func versionText(id string, v ApprovedVersion) (Tree, []Problem) {
	name := fmt.Sprintf("approved %s version %d", id, v.Version)
	tree, problems := Parse(map[string][]byte{name: []byte(v.Block)})
	if len(problems) != 0 {
		return Tree{}, problems
	}
	refuse := func(detail string) (Tree, []Problem) {
		return Tree{}, []Problem{idProblem(Position{File: name, Line: 1}, id, "bad-approved-block: "+detail)}
	}
	nodes := nodesOf(tree)
	switch {
	case len(nodes) != 1:
		return refuse(fmt.Sprintf("the block holds %d nodes, not one", len(nodes)))
	case nodes[0].id != id:
		return refuse(fmt.Sprintf("the block holds node %s, not %s", quoted(nodes[0].id), quoted(id)))
	}
	if printed, err := nodes[0].node.Print(); err != nil || printed != v.Block {
		return refuse("the block is not as Print writes it")
	}
	return tree, nil
}

// idProblem is the problem detail of the node id at pos: its rule is the word before the
// detail's first colon, and a node with no id is "-".
func idProblem(pos Position, id, detail string) Problem {
	if id == "" {
		id = "-"
	}
	rule, _, _ := strings.Cut(detail, ":")
	return Problem{Pos: pos, Node: id, Rule: rule, Detail: detail}
}

// anyNode is one node of a tree, of any of the four kinds: its id, its position, and the node.
type anyNode struct {
	id   string
	pos  Position
	node interface {
		Print() (string, error)
		Digest() string
	}
}

// nodesOf is every node of tree: its statements, checks, components, then terms, each in the
// tree's order.
func nodesOf(tree Tree) []anyNode {
	all := make([]anyNode, 0, len(tree.Statements)+len(tree.Checks)+len(tree.Components)+len(tree.Terms))
	for _, s := range tree.Statements {
		all = append(all, anyNode{s.ID, s.Pos, s})
	}
	for _, c := range tree.Checks {
		all = append(all, anyNode{c.ID, c.Pos, c})
	}
	for _, c := range tree.Components {
		all = append(all, anyNode{c.ID, c.Pos, c})
	}
	for _, t := range tree.Terms {
		all = append(all, anyNode{t.ID, t.Pos, t})
	}
	return all
}

// SortProblems sorts problems by file, line, node, rule and detail: the order of every list of
// problems that this package gives, so a caller that joins two lists sorts them the same way.
func SortProblems(problems []Problem) {
	slices.SortFunc(problems, func(a, b Problem) int {
		return cmp.Or(cmp.Compare(a.Pos.File, b.Pos.File), cmp.Compare(a.Pos.Line, b.Pos.Line), cmp.Compare(a.Node, b.Node),
			cmp.Compare(a.Rule, b.Rule), cmp.Compare(a.Detail, b.Detail))
	})
}
