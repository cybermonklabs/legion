// Package specchange reads the change request of an issue: the requirement, check and component
// blocks the issue's tree holds, each mapped to the node of The Spec of the same id (D2 of the
// work-units plan). A block of an issue writes The Spec's relations in attributes of its own, apart
// from an issue's placement (`parent`, `serves`): a statement's parent is `under`, the statements it
// supports are `supports`, a check's statement is `statement`, and a component's parent is `under`.
// Each names the approved version its writer read in `base`, absent for a node the writer adds, and
// `removes` asks for the node's removal. FromBlocks maps the blocks through specgraph's one parser
// (specgraph.Parse), so a block reads as The Spec's files read the same node. Classify compares each
// node with the approved spec.
package specchange

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// The kinds of node a request holds, as specgraph names them.
const (
	KindStatement = "statement"
	KindCheck     = "check"
	KindComponent = "component"
	KindInterface = "interface"
)

// fileForm is, for each block type of an issue that holds a node of The Spec, the attribute of the
// issue's block each attribute of The Spec's file form is read from (specgraph's blockAttributes),
// in the order specgraph prints them.
var fileForm = map[string][][2]string{
	"requirement": {{"kind", "kind"}, {"parent", "under"}, {"serves", "supports"}, {"about", "about"}},
	"check": {{"serves", "statement"}, {"subject", "subject"}, {"procedure", "procedure"}, {"runner", "runner"},
		{"selector", "selector"}, {"job", "job"}, {"time_limit", "time_limit"}},
	"component": {{"name", "name"}, {"kind", "kind"}, {"parent", "under"}, {"joins", "joins"}},
}

// IsSpecBlock is whether a block of type typ holds a node of The Spec.
func IsSpecBlock(typ string) bool {
	_, held := fileForm[typ]
	return held
}

// Block is one requirement, check or component block of an issue as the store holds it at a cut:
// the block node's address and its text, the block as a document holding it alone renders it
// (BlockText), from its opening line to its closing one.
type Block struct {
	Address string
	Text    string
}

// Proposal is one node of The Spec a request holds: its id, its kind, the node as specgraph holds
// it (a specgraph.Statement, specgraph.Check or specgraph.Component), the approved version its
// writer read (0 for none), whether it asks for the node's removal, and the block it was read from.
type Proposal struct {
	ID      string
	Kind    string
	Node    any
	Base    int
	Removes bool
	Block   string
}

// Request is an issue's change request: each node it holds, in the order of its blocks, and each
// problem of a block that gives no node, or gives one specgraph's parser finds a fault in.
type Request struct {
	Issue     string
	Proposals []Proposal
	Problems  []specgraph.Problem
	// CheckStatements is, for each check block of the request by its address, the statement its
	// `statement` attribute names, "" for none: what OfTheSpec judges whether the block is a check
	// of The Spec by, read even where the block gives no node.
	CheckStatements map[string]string
}

// FromBlocks reads the request of the issue from its blocks, in their order. A block whose opening
// line no parser reads, or whose base is no whole number from 1, gives a problem and no node; a
// block of another type is passed over.
func FromBlocks(issue string, blocks []Block) Request {
	request := Request{Issue: issue, CheckStatements: map[string]string{}}
	for _, block := range blocks {
		first, _, _ := strings.Cut(strings.ReplaceAll(block.Text, "\r\n", "\n"), "\n")
		if name, attrs, ok := pmdoc.ParseTypedDirectiveOpen(first); ok && name == KindCheck {
			request.CheckStatements[block.Address] = strings.TrimSpace(attrText(attrs, "statement"))
		}
		proposal, problems, ok := fromBlock(block)
		request.Problems = append(request.Problems, problems...)
		if ok {
			request.Proposals = append(request.Proposals, proposal)
		}
	}
	return request
}

// OfTheSpec is request with the issue's own acceptance checks left out (R22, R25). A check block
// is a check of The Spec only when its statement names a statement the request proposes or the
// approved spec holds as a current statement; any other check block - one that names no
// statement, or names one neither holds - is the issue's own acceptance check, which the change
// request does not hold: no approval writes it or is refused for it, and it changes no approved
// check. Its node and every problem of its block leave the request; every other node and problem
// stays. It is the one predicate of this rule: the approval's write (specstore.Apply) and the read
// of a change request (api's readChangeRequest) both judge the request it answers.
func OfTheSpec(request Request, approved specgraph.Approved) Request {
	proposed := map[string]bool{}
	for _, proposal := range request.Proposals {
		if proposal.Kind == KindStatement {
			proposed[proposal.ID] = true
		}
	}
	acceptance := map[string]bool{}
	for address, statement := range request.CheckStatements {
		if statement == "" || !proposed[statement] && !approvedStatement(approved, statement) {
			acceptance[address] = true
		}
	}
	if len(acceptance) == 0 {
		return request
	}
	kept := Request{Issue: request.Issue, CheckStatements: request.CheckStatements}
	for _, proposal := range request.Proposals {
		if !acceptance[proposal.Block] {
			kept.Proposals = append(kept.Proposals, proposal)
		}
	}
	for _, problem := range request.Problems {
		if !acceptance[problem.Pos.File] {
			kept.Problems = append(kept.Problems, problem)
		}
	}
	return kept
}

// approvedStatement is whether approved holds id as a current statement: held, not removed, and
// a statement at its newest version.
func approvedStatement(approved specgraph.Approved, id string) bool {
	node, held := approved.Nodes[id]
	if !held || node.Removed != nil && *node.Removed || len(node.Versions) == 0 {
		return false
	}
	current, err := VersionNode(id, node.Versions[len(node.Versions)-1])
	if err != nil {
		return false
	}
	_, statement := current.(specgraph.Statement)
	return statement
}

// fromBlock maps one block to its node: its opening line's attributes, renamed to the file form,
// and its body, read by specgraph.Parse as one file named for the block.
func fromBlock(block Block) (Proposal, []specgraph.Problem, bool) {
	lines := strings.Split(strings.ReplaceAll(block.Text, "\r\n", "\n"), "\n")
	pos := specgraph.Position{File: block.Address, Line: 1}
	refuse := func(id, detail string) (Proposal, []specgraph.Problem, bool) {
		if id == "" {
			id = "-"
		}
		rule, _, _ := strings.Cut(detail, ":")
		return Proposal{}, []specgraph.Problem{{Pos: pos, Node: id, Rule: rule, Detail: detail}}, false
	}
	name, attrs, ok := pmdoc.ParseTypedDirectiveOpen(lines[0])
	if !ok {
		return refuse("", "bad-block-line: the line opens no block")
	}
	form, held := fileForm[name]
	if !held {
		return Proposal{}, nil, false
	}
	id := attrText(attrs, pmdoc.BlockIDAttr)
	proposal := Proposal{ID: id, Block: block.Address, Removes: attrText(attrs, "removes") == "true"}
	if base := attrText(attrs, "base"); base != "" {
		number, err := strconv.Atoi(base)
		if err != nil || number < 1 {
			return refuse(id, fmt.Sprintf("bad-base: base %q is no approved version, a whole number from 1", base))
		}
		proposal.Base = number
	}
	var file strings.Builder
	file.WriteString(":::" + name + "{#" + escape(id))
	for _, pair := range form {
		if value := attrText(attrs, pair[1]); value != "" {
			file.WriteString(" " + pair[0] + `="` + escape(value) + `"`)
		}
	}
	file.WriteString("}\n")
	body := lines[1:]
	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	if len(body) > 0 && strings.TrimRight(body[len(body)-1], " \t") == ":::" {
		body = body[:len(body)-1]
	}
	for _, line := range body {
		file.WriteString(line + "\n")
	}
	file.WriteString(":::\n")
	tree, problems := specgraph.Parse(map[string][]byte{block.Address: []byte(file.String())})
	nodes := 0
	for _, statement := range tree.Statements {
		proposal.Kind, proposal.Node = KindStatement, statement
		nodes++
	}
	for _, check := range tree.Checks {
		proposal.Kind, proposal.Node = KindCheck, check
		nodes++
	}
	for _, component := range tree.Components {
		proposal.Kind, proposal.Node = KindComponent, component
		if component.Interface {
			proposal.Kind = KindInterface
		}
		nodes++
	}
	if nodes != 1 {
		if len(problems) == 0 {
			problems = []specgraph.Problem{{Pos: pos, Node: idOrDash(id), Rule: "bad-block", Detail: "bad-block: the block reads as no node of The Spec"}}
		}
		return Proposal{}, problems, false
	}
	return proposal, problems, true
}

// attrText is the value of one attribute of an opening line as text: a string as written, a
// boolean as true or false, "" when the line does not write it.
func attrText(attrs pmdoc.Attrs, name string) string {
	switch value := attrs[name].(type) {
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

// escape writes the two characters of an attribute value that would end it or read as a character
// reference, as specgraph's printer writes them.
func escape(value string) string {
	return strings.NewReplacer("&", "&amp;", `"`, "&quot;").Replace(value)
}

// idOrDash is id, or "-" for a block that names none, as specgraph names a node of no id.
func idOrDash(id string) string {
	if id == "" {
		return "-"
	}
	return id
}

// The ways a proposal stands to the approved spec (Classify).
const (
	// Context is a node equal to the approved version its base names: no change.
	Context = "context"
	// Added is a node the approved spec does not hold, written with no base.
	Added = "added"
	// Changed is a node whose fields differ from the current approved version its base names.
	Changed = "changed"
	// Removed is a node whose block asks for its removal at the current approved version.
	Removed = "removed"
)

// Change is one proposal and how it stands to the approved spec.
type Change struct {
	Proposal
	What string
}

// Stale is one proposal written against a version that is not the current approved one: Base is
// the version the writer read (0 for none) and Current the approved spec's current version (0 where
// it holds none, or removed the node, which RemovedNode says).
type Stale struct {
	ID          string
	Base        int
	Current     int
	RemovedNode bool
}

// Classify compares each proposal of request with the approved spec: a node whose base is the
// approved spec's current version is context when it equals that version's text and changed or
// removed otherwise; a node with no base that the approved spec never held is added; any other is
// stale - a base that is not current, a base on a node the approved spec never held, no base on a
// node it holds, or any base on a node it removed. A removed node written again with no base is
// added here, and refused where the approved spec is judged (specgraph.Validate's retired-id). A
// removal of a node the approved spec never held is a problem of the request: it removes nothing.
func Classify(request Request, approved specgraph.Approved) (changes []Change, stale []Stale, problems []specgraph.Problem, err error) {
	for _, proposal := range request.Proposals {
		node, held := approved.Nodes[proposal.ID]
		removed := held && node.Removed != nil && *node.Removed
		current := 0
		if held && !removed {
			current = node.Versions[len(node.Versions)-1].Version
		}
		switch {
		case !held && proposal.Base == 0:
			if proposal.Removes {
				problems = append(problems, specgraph.Problem{Pos: specgraph.Position{File: proposal.Block, Line: 1}, Node: proposal.ID,
					Rule: "removes-nothing", Detail: "removes-nothing: the approved spec holds no node " + proposal.ID + " to remove"})
				continue
			}
			changes = append(changes, Change{Proposal: proposal, What: Added})
		case removed && proposal.Base == 0:
			changes = append(changes, Change{Proposal: proposal, What: Added})
		case !held || removed || proposal.Base != current:
			stale = append(stale, Stale{ID: proposal.ID, Base: proposal.Base, Current: current, RemovedNode: removed})
		default:
			text, err := VersionNode(proposal.ID, node.Versions[len(node.Versions)-1])
			if err != nil {
				return nil, nil, nil, err
			}
			switch {
			case proposal.Removes:
				changes = append(changes, Change{Proposal: proposal, What: Removed})
			case Digest(text) == Digest(proposal.Node):
				changes = append(changes, Change{Proposal: proposal, What: Context})
			default:
				changes = append(changes, Change{Proposal: proposal, What: Changed})
			}
		}
	}
	return changes, stale, problems, nil
}

// VersionNode is the node one approved version of id holds, read from its printed block. A problem
// of form that keeps the node (a claim of two sentences) is no fault here: an approval writes such
// a node as it stands (specstore's nodeLosses), so its version reads back as that node.
func VersionNode(id string, version specgraph.ApprovedVersion) (any, error) {
	tree, problems := specgraph.Parse(map[string][]byte{"approved": []byte(version.Block)})
	nodes := NodesOf(tree)
	if len(nodes) != 1 || slices.ContainsFunc(problems, specgraph.Problem.LosesNode) {
		return nil, fmt.Errorf("specchange: version %d of the approved node %s holds no one node: %v", version.Version, id, problems)
	}
	return nodes[0], nil
}

// NodesOf is every node of tree: its statements, checks and components, in that order.
func NodesOf(tree specgraph.Tree) []any {
	var nodes []any
	for _, statement := range tree.Statements {
		nodes = append(nodes, statement)
	}
	for _, check := range tree.Checks {
		nodes = append(nodes, check)
	}
	for _, component := range tree.Components {
		nodes = append(nodes, component)
	}
	return nodes
}

// Digest is the digest of a node's content apart from its position (specgraph's Digest), "" for a
// value that is no node.
func Digest(node any) string {
	switch n := node.(type) {
	case specgraph.Statement:
		return n.Digest()
	case specgraph.Check:
		return n.Digest()
	case specgraph.Component:
		return n.Digest()
	}
	return ""
}

// IsCheck is whether change writes a check.
func (c Change) IsCheck() bool { return c.Kind == KindCheck }

// Writes is whether the change writes the approved spec: an added, changed or removed node.
func (c Change) Writes() bool { return c.What != Context }

// SortStale orders stale proposals by id, as a refusal names them.
func SortStale(stale []Stale) {
	slices.SortFunc(stale, func(a, b Stale) int { return strings.Compare(a.ID, b.ID) })
}
