package specgraph

import "slices"

// approvalKind is one kind of problem of a comparison with the approved spec, an index of
// approvalKinds: a rule of Compare, or bad-approved-document, which versionText gives for a
// document of the approved spec and relationFault for a relation of one.
type approvalKind int

// The kinds of problem of a comparison with the approved spec. numApprovalKinds counts them, so
// each new kind is a place of approvalKinds, which ProblemKinds lists, and the table test of the
// lines that print a problem (cmd/spec-check) fails until it has a row.
const (
	unapprovedID approvalKind = iota
	unapprovedText
	removedID
	approvedNodeCount
	approvedOtherNode
	approvedNotAnID
	approvedNotPrinted
	approvedNoNode
	approvedOtherKind
	numApprovalKinds
)

// approvalKinds is the format of the Detail of each kind of problem of a comparison with the
// approved spec, which begins with its rule: Compare's rules, and bad-approved-document for a
// document of no node or of two, of a node with another id, with a value at an id place that is
// not an id, or not as Print writes its node, and for a relation that names no node of the
// approved spec or a node of a kind that it may not name.
var approvalKinds = [numApprovalKinds]string{
	unapprovedID:       "unapproved-id: the approved spec never held this id",
	unapprovedText:     "unapproved-text: the approved spec never held this text",
	removedID:          "removed-id: the approved spec removed this id and never held this text for it",
	approvedNodeCount:  "bad-approved-document: the document holds %d nodes, not one",
	approvedOtherNode:  "bad-approved-document: the document is of node %s, not %s",
	approvedNotAnID:    "bad-approved-document: %s %s is no id",
	approvedNotPrinted: "bad-approved-document: the document is not as Print writes it",
	approvedNoNode:     "bad-approved-document: %s %s names no node of the approved spec",
	approvedOtherKind:  "bad-approved-document: %s %s names %s, not %s",
}

// Holding is which approved text of a node the files hold: Present when they hold one, Version its
// number, and Current when it is the node's current text.
type Holding struct {
	Present bool
	Version int
	Current bool
}

// holdsCurrent is whether the files hold the node's current approved text: the one reading of it
// that the check judge (not_built) and the statement judge (notBuilt) share.
func (h Holding) holdsCurrent() bool {
	return h.Present && h.Current
}

// Comparison is what Compare finds: Problems, each node of the files in a text that the approved
// spec does not let them hold; Holds, for each node of the approved spec that it did not remove,
// the approved text that the files hold; and Removed, the id of each node that the files hold in
// a text that the approved spec held and that it then removed, sorted and once each. The
// comparison passes exactly when Problems is empty. Removed and each Holds entry that is not
// current are differences all the same (R28: "a statement is approved and not built, or the
// files hold one that is no longer approved"), which the work then removes.
type Comparison struct {
	Problems []Problem
	Holds    map[string]Holding
	Removed  []string
}

// Compare compares the spec files at one commit with the approved spec (R2). A node's text is its
// Digest, and an approved version's is the Digest of its document's node, so a move to another file or
// another order changes no text. Each problem names the node with its file and line, sorted as
// Validate sorts them:
//
//   - unapproved-id: the approved spec never held a node with this id;
//   - removed-id: the approved spec removed this id, and the files hold it in a text that the
//     node never held, which uses the id again (R1);
//   - unapproved-text: the approved spec never held this text for this node.
//
// The files may hold any text that the node held in the approved spec: an earlier text until the
// change that builds the current one lands, and a text of a removed node until the change that
// removes it lands (R2: the comparison fails on "a text that the approved spec never held";
// R22: the person approves a removal "before the work on it starts"). A removed node has no entry
// in Holds; its id is in Removed. When the files hold one id two times, Holds takes the first of
// them that holds an approved text; Validate refuses the doubled id.
func Compare(files Tree, a Approved) Comparison {
	c := Comparison{Holds: map[string]Holding{}}
	texts := make(map[string]map[string]int, len(a.Nodes)) // id -> digest -> its newest version
	for id, n := range a.Nodes {
		if !n.Removed {
			c.Holds[id] = Holding{}
		}
		texts[id] = map[string]int{}
		for _, v := range n.Versions {
			if text, problems := versionText(id, v); len(problems) == 0 {
				digest := Nodes(text)[0].Digest()
				texts[id][digest] = max(texts[id][digest], v.Version)
			}
		}
	}
	for _, f := range Nodes(files) {
		id := f.NodeID()
		approved, held := a.Nodes[id]
		version, approvedText := texts[id][f.Digest()]
		detail := ""
		switch {
		case !held:
			detail = approvalKinds[unapprovedID]
		case !approvedText && approved.Removed:
			detail = approvalKinds[removedID]
		case !approvedText:
			detail = approvalKinds[unapprovedText]
		case approved.Removed:
			// The files still hold a text of a removed node; it holds no current text.
			if !slices.Contains(c.Removed, id) {
				c.Removed = append(c.Removed, id)
			}
		case !c.Holds[id].Present:
			c.Holds[id] = Holding{Present: true, Version: version, Current: version == approved.Versions[len(approved.Versions)-1].Version}
		}
		if detail != "" {
			c.Problems = append(c.Problems, idProblem(f.NodePos(), id, detail))
		}
	}
	SortProblems(c.Problems)
	slices.Sort(c.Removed)
	return c
}
