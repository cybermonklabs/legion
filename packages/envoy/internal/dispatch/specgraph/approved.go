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
// in approved.json a time of timeForm, and Document, the node's YAML document as Print writes it,
// which a change copies into the files.
type ApprovedVersion struct {
	Version  int       `json:"version" input:"nonzero"`
	At       time.Time `json:"at" input:"nonzero"`
	Document string    `json:"document" input:"nonzero"`
}

// ApprovedNode is one statement, check, component, interface or term of the approved spec, with
// each text that it held, oldest first. The newest version is its current text, unless the
// approved spec removed the node: a removed node has no current text, and its id is never used
// for another text (R1). PartlyChecked marks a statement whose checks decide only part of it, as
// the reviewer of its level judged (R10). A caller that builds an Approved and leaves Removed and
// PartlyChecked false gives a node that the approved spec keeps, and a statement whose checks
// decide all of it; approved.json sets both of each node, and ReadApproved refuses a node that
// leaves one out (approvedFileNode).
type ApprovedNode struct {
	ID            string            `json:"id" input:"nonzero"`
	Removed       bool              `json:"removed" input:"zero: a node that the approved spec keeps"`
	PartlyChecked bool              `json:"partly_checked" input:"zero: a statement whose checks decide all of it"`
	Versions      []ApprovedVersion `json:"versions" input:"nonzero"`
}

// Approved is the approved spec (R2): each node that it holds or held, by id.
type Approved struct {
	Nodes map[string]ApprovedNode `input:"zero: no node"`
}

// approvedFile is approved.json, the approved spec as a file holds it, and the one definition of
// that form (approvedForm): WriteApproved writes it, and ReadApproved reads it. "at" is an RFC 3339
// time as time.RFC3339Nano writes it, such as "2026-11-10T00:00:00Z" (timeForm). An approved spec
// that holds no node is {"nodes":[]}.
type approvedFile struct {
	Nodes []approvedFileNode `json:"nodes" input:"nonzero"`
}

// approvedFileNode is one node of approved.json, an ApprovedNode as the file holds it: Removed and
// PartlyChecked are nil where the file leaves them out, which ReadApproved refuses (unsetFault),
// since false is a value of each.
type approvedFileNode struct {
	ID            string            `json:"id" input:"nonzero"`
	Removed       *bool             `json:"removed" input:"present: false is a node that the approved spec keeps"`
	PartlyChecked *bool             `json:"partly_checked" input:"present: false is a statement whose checks decide all of it"`
	Versions      []ApprovedVersion `json:"versions" input:"nonzero"`
}

// node is the ApprovedNode that n holds, false for a member that n leaves out.
func (n approvedFileNode) node() ApprovedNode {
	return ApprovedNode{ID: n.ID, Removed: n.Removed != nil && *n.Removed, PartlyChecked: n.PartlyChecked != nil && *n.PartlyChecked, Versions: n.Versions}
}

// unsetFault is the refusal of n that leaves out removed or partly_checked, whose false is a value
// of each (its tag input, members.go), or nil.
func (n approvedFileNode) unsetFault() error {
	switch {
	case n.Removed == nil:
		return fmt.Errorf("specgraph: approved node %s: it sets no removed; a node that the approved spec keeps sets false", quoted(n.ID))
	case n.PartlyChecked == nil:
		return fmt.Errorf("specgraph: approved node %s: it sets no partly_checked; a statement whose checks decide all of it sets false", quoted(n.ID))
	}
	return nil
}

// timeForm is a time as each form text of this package names its type: an RFC 3339 time with an
// upper-case T, in UTC with an upper-case Z, as time.RFC3339Nano writes it, or at an offset such as
// +01:00, which the readers order by its instant. The readers refuse a lower-case t or z, which RFC
// 3339 allows. A time at the zero instant, 0001-01-01T00:00:00Z at any offset, is no time: the
// readers refuse it as a member that is not set ("it sets no at", "it sets no now", "it has no
// time").
const timeForm = `<RFC 3339 time with an upper-case T, and Z or an offset such as +01:00>`

// approvedForm is the form of approved.json, with the type of each value, as ReadApproved names it
// for a value of another type.
const approvedForm = `{"nodes":[{"id":"<id>","removed":<true or false>,"partly_checked":<true or false>,"versions":[{"version":<whole number from 1>,"at":"` + timeForm + `","document":"<document>"}, …]}, …]}`

// ReadApproved reads an approved spec in the form of approved.json (approvedFile). It refuses
// what decodeJSON refuses (a byte that is not UTF-8, a field it does not know or that is written
// in other letters, a field that one object gives two times, a value of another type than
// approvedForm needs, such as a time that is not RFC 3339, named by its place, no object or text
// after it, and a null at any place, such as null or "nodes": null), a file with no list of nodes
// ({}), and what approvedFault refuses, in an error that names the node, by its id as quoted
// prints a value of the input: a node with no id, an id on two nodes, a node that leaves out
// removed or partly_checked, since false is a value of each (approvedFileNode.unsetFault), a node
// with no version, version numbers that do not go up from 1, a version with no time or with one
// older than the one before it, a document that is not its node's one document as Print writes it or
// whose id is no id (versionText), a document that names at a relation no node of the approved spec,
// or, as the current text of a node that the approved spec keeps, a node of a kind that the
// relation may not name (relationFault), naming the version, and a loop of parent and serves
// relations among the current statements, as Validate's loop rule writes it. It reads at most
// fileBound bytes, and refuses a file that holds more.
func ReadApproved(r io.Reader) (Approved, error) {
	var file approvedFile
	if err := decodeJSONFrom(r, &file, approvedForm); err != nil {
		return Approved{}, fmt.Errorf("specgraph: approved spec: %w", err)
	}
	return file.approved()
}

// WriteApproved writes a to w as approved.json (approvedFile), the nodes in the order of their ids,
// and a line break after it, so ReadApproved reads back a and two writes of a give the same bytes.
// It writes removed and partly_checked of each node, false too. For an approved spec that
// ReadApproved would refuse, such as a node under another id than its own, it writes nothing and
// answers the fault.
func WriteApproved(w io.Writer, a Approved) error {
	nodes := make([]ApprovedNode, 0, len(a.Nodes))
	file := approvedFile{Nodes: make([]approvedFileNode, 0, len(a.Nodes))}
	for _, id := range slices.Sorted(maps.Keys(a.Nodes)) {
		n := a.Nodes[id]
		if err := n.keyFault(id); err != nil {
			return err
		}
		nodes = append(nodes, n)
		file.Nodes = append(file.Nodes, approvedFileNode{ID: n.ID, Removed: &n.Removed, PartlyChecked: &n.PartlyChecked, Versions: n.Versions})
	}
	if err := approvedFault(nodes, nil); err != nil {
		return err
	}
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("specgraph: approved spec: %w", err)
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// approved is the approved spec that f holds, or the first fault of f that ReadApproved refuses
// (approvedFault).
func (f approvedFile) approved() (Approved, error) {
	if f.Nodes == nil {
		return Approved{}, errors.New(`specgraph: approved spec: it holds no "nodes" list`)
	}
	nodes := make([]ApprovedNode, len(f.Nodes))
	for i, n := range f.Nodes {
		nodes[i] = n.node()
	}
	if err := approvedFault(nodes, f.Nodes); err != nil {
		return Approved{}, err
	}
	a := Approved{Nodes: make(map[string]ApprovedNode, len(nodes))}
	for _, n := range nodes {
		a.Nodes[n.ID] = n
	}
	for _, n := range f.Nodes {
		if err := n.unsetFault(); err != nil {
			return Approved{}, err
		}
	}
	return a, nil
}

// approvedFault is the first fault of the approved spec of nodes, in their order, that ReadApproved
// and the entry of the derivation (Input.check) refuse, or nil: the faults of one node first, of
// each node an id that a node before it has too, or a fault of its id or of its texts
// (ApprovedNode.texts), then of each node of read a member that the file leaves out
// (approvedFileNode.unsetFault), read being the nodes as ReadApproved read them, in the order of
// nodes, and nil for a caller that gives values (Input.check, WriteApproved); then a fault between
// two nodes, of each node a relation of one of its texts that names no node of the approved spec,
// kept or removed, or, of its current text where the approved spec keeps it, a node of a kind that
// the relation may not name (relationFault); and last a loop of parent and serves relations among
// the current statements (R5: "The parent and support relations taken together form no cycle, and
// Dispatch refuses a change that makes one"), the first that findLoops gives. The kind of a node is
// the kind of its newest text. The derivation reads the tree of each statement down its relations,
// so a statement of a loop would read as under no statement, and the statement above it as met
// without it.
func approvedFault(nodes []ApprovedNode, read []approvedFileNode) error {
	texts := make([][]anyNode, len(nodes))
	kinds := make(map[string]string, len(nodes))
	for i, n := range nodes {
		if _, twice := kinds[n.ID]; twice {
			return fmt.Errorf("specgraph: approved spec: node %s is in the approved spec two times", quoted(n.ID))
		}
		var err error
		if texts[i], err = n.texts(); err != nil {
			return err
		}
		kinds[n.ID] = kindOf(texts[i][len(texts[i])-1].node)
	}
	for _, n := range read {
		if err := n.unsetFault(); err != nil {
			return err
		}
	}
	var current []Statement
	for i, n := range nodes {
		for j, text := range texts[i] {
			if detail := relationFault(text.node, kinds, j == len(texts[i])-1 && !n.Removed); detail != "" {
				return fmt.Errorf("specgraph: approved node %s version %d: %s (line %d of its document)", quoted(n.ID), n.Versions[j].Version, detail, text.pos.Line)
			}
		}
		if s, isStatement := texts[i][len(texts[i])-1].node.(Statement); isStatement && !n.Removed {
			current = append(current, s)
		}
	}
	if loops := findLoops(statementEdges(current)); len(loops) > 0 {
		return fmt.Errorf("specgraph: approved spec: loop: %s", loopDetail(loops[0]))
	}
	return nil
}

// texts of n: no id, no version, version numbers that do not go up from 1, a version with no time
// or with one older than the one before it, and a document that versionText refuses.
func (n ApprovedNode) texts() ([]anyNode, error) {
	switch {
	case n.ID == "":
		return nil, errors.New("specgraph: approved spec: a node has no id")
	case len(n.Versions) == 0:
		return nil, fmt.Errorf("specgraph: approved node %s: it has no version", quoted(n.ID))
	}
	texts := make([]anyNode, len(n.Versions))
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
			if text, problems := versionText(n.ID, v); len(problems) != 0 {
				fault = fmt.Sprintf("%s (line %d of its document)", problems[0].Detail, problems[0].Pos.Line)
			} else {
				texts[i] = nodesOf(text)[0]
			}
		}
		if fault != "" {
			return nil, fmt.Errorf("specgraph: approved node %s version %d: %s", quoted(n.ID), v.Version, fault)
		}
	}
	return texts, nil
}

// keyFault is the refusal of n under the key id of an Approved, which is not its own id, or nil:
// Current reads a node by its key and would leave such a node out, so WriteApproved and the entry
// of the derivation (Input.check) refuse it.
func (n ApprovedNode) keyFault(id string) error {
	if n.ID != id {
		return fmt.Errorf("specgraph: approved spec: node %s is under the id %s", quoted(n.ID), quoted(id))
	}
	return nil
}

// Current is the tree of the current text of each node that the approved spec did not remove.
// Each node is at the position "approved <id> version <n>", line 1. A caller that builds an
// Approved itself, as the pilot does from the store, can give a document that ReadApproved would
// refuse: Current leaves that node out and gives each problem of its document (versionText), or the
// problem of its relation that names no node of the approved spec of the kind that it needs
// (relationFault). It leaves out a node with no version too, which approvedFault refuses.
func (a Approved) Current() (Tree, []Problem) {
	var tree Tree
	var problems []Problem
	kinds := make(map[string]string, len(a.Nodes)) // of each node, kept or removed, by its newest text
	var kept []anyNode
	for id, n := range a.Nodes {
		if len(n.Versions) == 0 {
			continue
		}
		text, refused := versionText(id, n.Versions[len(n.Versions)-1])
		if len(refused) == 0 {
			kinds[id] = kindOf(nodesOf(text)[0].node)
		}
		switch {
		case n.Removed:
		case len(refused) != 0:
			problems = append(problems, refused...)
		default:
			kept = append(kept, nodesOf(text)[0])
		}
	}
	for _, k := range kept {
		if detail := relationFault(k.node, kinds, true); detail != "" {
			problems = append(problems, idProblem(k.pos, k.id, detail))
			continue
		}
		switch n := k.node.(type) {
		case Statement:
			tree.Statements = append(tree.Statements, n)
		case Check:
			tree.Checks = append(tree.Checks, n)
		case Component:
			tree.Components = append(tree.Components, n)
		case Term:
			tree.Terms = append(tree.Terms, n)
		}
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
// position "approved <id> version <n>". problems is each reason it holds none: a problem of the
// reader in the document (bad-yaml, no-node, bad-time-limit, not-printed), or bad-approved-document
// (approvalKinds) for a document of no node or of two, of a node with another id, with a value at an
// id place that is not an id (notAnID), or not as Print writes its node. The schema's other
// problems are the rules' of the files, as Validate's are, and pass here: an approved text that
// breaks a rule reads as its node, so a check with no failing example reads as such (R9). A
// relation of the node is a fact of the whole approved spec, which relationFault reads.
func versionText(id string, v ApprovedVersion) (Tree, []Problem) {
	name := fmt.Sprintf("approved %s version %d", id, v.Version)
	tree, problems := Parse(map[string][]byte{name: []byte(v.Document)})
	// Parse holds a document with no problem that loses a node to the document that Print writes
	// (not-printed); a document with a problem of the schema that loses a node it does not hold to
	// it, so the check below does.
	printedFormUnchecked := slices.ContainsFunc(problems, func(p Problem) bool { return p.Effect == LosesNode })
	problems = slices.DeleteFunc(problems, func(p Problem) bool { return p.Rule == "schema" })
	if len(problems) != 0 {
		return Tree{}, problems
	}
	refuse := func(kind approvalKind, args ...any) (Tree, []Problem) {
		return Tree{}, []Problem{idProblem(Position{File: name, Line: 1}, id, fmt.Sprintf(approvalKinds[kind], args...))}
	}
	nodes := nodesOf(tree)
	switch {
	case len(nodes) != 1:
		return refuse(approvedNodeCount, len(nodes))
	case nodes[0].id != id:
		return refuse(approvedOtherNode, quoted(nodes[0].id), quoted(id))
	}
	if member, value, found := notAnID(nodes[0].node); found {
		return refuse(approvedNotAnID, member, quoted(value))
	}
	if printedFormUnchecked {
		if printed, err := nodes[0].node.Print(); err != nil || printed != v.Document {
			return refuse(approvedNotPrinted)
		}
	}
	return tree, nil
}

// notAnID is the first value of node, the one node of an approved document, at a place that names
// a node and not in the shape of an id, by its member: its id, its parent, and each node that it
// serves, is about or joins, the places that the schema gives the definition id (#/$defs/id, or an
// item of #/$defs/ids). The derivation reads a relation only by an id (isIDShape), so a relation of
// another value, a name of no letter and no digit among them, would read as no relation: a
// statement at the top of the tree in place of under its parent, which can make that parent met.
// The schema refuses such a value in the files; in an approved document its problems pass, so
// versionText refuses it here. A "" at a place that is not an entry of a list, such as an id, a
// parent or the statement that a check serves, is one that the node does not set, and each entry
// of a list is set.
// The problem is bad-approved-document: <member> "<value>" is no id, with the value as quoted
// prints it, and ReadApproved refuses the version with it (approvedFault).
func notAnID(node any) (member, value string, found bool) {
	type place struct {
		member, value string
		entry         bool
	}
	entries := func(member string, ids []string) []place {
		places := make([]place, len(ids))
		for i, id := range ids {
			places[i] = place{member, id, true}
		}
		return places
	}
	var places []place
	switch n := node.(type) {
	case Statement:
		places = slices.Concat([]place{{"id", n.ID, false}, {"parent", n.Parent, false}}, entries("serves", n.Serves), entries("about", n.About))
	case Check:
		places = []place{{"id", n.ID, false}, {"serves", n.Serves, false}}
	case Component:
		places = slices.Concat([]place{{"id", n.ID, false}, {"parent", n.Parent, false}}, entries("joins", n.Joins))
	case Term:
		places = []place{{"id", n.ID, false}}
	}
	for _, p := range places {
		if (p.entry || p.value != "") && !slug.MatchString(p.value) {
			return p.member, p.value, true
		}
	}
	return "", "", false
}

// relationFault is the refusal of the first relation of node, the one node of an approved document,
// that names no node of the approved spec, or, where current, a node of a kind that the relation
// may not name (relationRule), or "". kinds is the kind of each node of the approved spec, kept or
// removed (kindOf). current is whether node is the current text of a node that the approved spec
// keeps, the text that Current, the derivation and Validate read. The derivation reads a relation
// only to a node of its kind, so another would read as no relation: a statement at the top of the
// tree in place of under its parent, or a supporter that supports nothing, which can make a
// statement met. R22: "Dispatch refuses the approval of a change request that puts a statement
// under a parent that the approved spec does not hold", so the approved spec holds no such
// relation, and Validate refuses one in the files by the same kinds. An older text, or the text of
// a removed node, is judged only by the nodes that it names: the derivation reads none of its
// relations, and R15: "An interface is a component marked as an interface", so a component that
// such a text names as its parent can be an interface now. Dispatch keeps each text that the
// approved spec held (R2), so a node that a text named is in the approved spec still, and a
// relation of any text to no node is refused. A value that is no id names no node, since the id of
// each node is an id (versionText). A node that the approved spec removed is one that it holds, so
// a relation to it is taken: the derivation reads the current tree, which leaves the removed node
// out.
func relationFault(node any, kinds map[string]string, current bool) string {
	for _, r := range relationsOf(node) {
		kind, held := kinds[r.id]
		switch {
		case !held:
			return fmt.Sprintf("bad-approved-document: %s %s names no node of the approved spec", r.member, quoted(r.id))
		case current && !slices.Contains(r.kinds, kind):
			wanted := make([]string, len(r.kinds))
			for i, k := range r.kinds {
				wanted[i] = withArticle(k)
			}
			return fmt.Sprintf("bad-approved-document: %s %s names %s, not %s", r.member, quoted(r.id), withArticle(kind), strings.Join(wanted, " or "))
		}
	}
	return ""
}

// relation is one relation of a node: its member, such as parent, the id that it names, and the
// kinds of node that it may name (relationRule).
type relation struct {
	member, id string
	kinds      []string
}

// relationsOf is each relation that node, one node of a tree, sets: its parent, and each node that
// it serves, is about or joins. A check serves one statement (R6: its claim is "narrower than its
// statement"), so its serves is a relation also when its document leaves it out, and "" names no
// node. A parent left out is one that the node does not set, as a statement or a component at the
// top of the tree has none (R1: "Each statement has one parent, except at the top level"), and
// Parse drops an empty entry of a list.
func relationsOf(node any) []relation {
	var relations []relation
	add := func(rule relationRule, ids ...string) {
		for _, id := range ids {
			relations = append(relations, relation{rule.member, id, rule.kinds})
		}
	}
	switch n := node.(type) {
	case Statement:
		if n.Parent != "" {
			add(statementParent, n.Parent)
		}
		add(statementServes, n.Serves...)
		add(statementAbout, n.About...)
	case Check:
		add(checkServes, n.Serves)
	case Component:
		if n.Parent != "" {
			add(componentParent, n.Parent)
		}
		add(componentJoins, n.Joins...)
	}
	return relations
}

// kindOf is the kind of node, one node of a tree, as Validate names it: statement, check,
// component, interface or term.
func kindOf(node any) string {
	switch n := node.(type) {
	case Statement:
		return "statement"
	case Check:
		return "check"
	case Component:
		if n.Interface {
			return "interface"
		}
		return "component"
	}
	return "term"
}

// withArticle is kind with its article, such as "a check" or "an interface".
func withArticle(kind string) string {
	if strings.ContainsRune("aeiou", rune(kind[0])) {
		return "an " + kind
	}
	return "a " + kind
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
