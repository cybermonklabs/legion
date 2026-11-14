// Package dispatchblocks reads the typed blocks of one version of a Dispatch document, for a
// caller outside this module: a reader that must know what a named version of a document held -
// which requirement blocks it marked, and which blocks each of its checks serves - where
// Dispatch's blocks route (GET /api/v1/artifacts/{id}/blocks) answers only the live document.
//
// The reading is the server's own. Read reads a version's markdown with the reader Dispatch shows
// a past version with (pmdoc.ReadForDisplay in pmdoc.DisplayVersion, the mode the version route
// reads one in), and takes each typed block's references with the reader Dispatch's pointer check
// takes them with (pmdoc.BlockPointers, which parses each with text.CanonicalBlockRef), so a
// caller reads a version exactly as Dispatch reads it rather than approximating it with a parser
// of its own. Those readers are internal packages, unreachable from another module; this package
// is the one public door onto them, which is why it exists at all.
package dispatchblocks

import "github.com/sjawhar/envoy/internal/dispatch/pmdoc"

// Block is one typed block of a document version: its id as the markdown writes it ("" for a
// block whose markdown names none), its type as the block schema names it (`requirement`,
// `check`, `plan_task`, …) and every reference its `block[]` attributes hold, a block's
// attributes by name and each attribute's references in the order it holds them. A block of no
// such attribute, or of one holding no reference, has no Refs.
type Block struct {
	ID   string
	Type string
	Refs []Ref
}

// Ref is one reference a typed block's `block[]` attribute holds, as Dispatch reads it: the
// attribute holding it (a check's `serves`), the reference as written, and the block it names.
// The block is a block of a document of an issue (Issue), or of a project's document a project
// holds and no issue does (Project, Issue empty). Primary says the document is the issue's
// primary document, which a reference writes `dispatch://<ISSUE>/spec`; every other document is
// named by its slug (`dispatch://<ISSUE or PROJECT>/artifact/<slug>`), Slug being empty for the
// primary document. Block is the block's id as decoded from the reference's `#b-` fragment, and
// Pin the document version a pinned reference (`@v<N>`) names, 0 for an unpinned one.
type Ref struct {
	Attribute string
	Text      string
	Issue     string
	Project   string
	Primary   bool
	Slug      string
	Block     string
	Pin       int
}

// Read answers the typed blocks of markdown, one version's text of a Dispatch document, in
// document order. It never refuses: a block the server's reader does not draw as a typed block
// reads as the literal text it is, and is no typed block here either. That is a directive of a
// name the block schema does not know, and a typed block the schema refuses, a `block[]`
// attribute holding anything but canonical block references (text.CanonicalBlockRef) among them,
// so every reference a listed block holds is one.
//
// A typed block repeating the id of a typed block before it is not listed, since the block index
// takes the first holder of an id for its node and lists no pointer of the repeat
// (pmdoc.BlockPointers): what a reader of the version finds under an id is the block listed
// under it here. A block whose markdown names no id repeats nothing and is always listed.
func Read(markdown string) []Block {
	var blocks []Block
	held := make(map[string]struct{})
	pmdoc.Walk(pmdoc.ReadForDisplay(markdown, pmdoc.DisplayVersion), func(node *pmdoc.Node) bool {
		if !pmdoc.IsTypedBlock(node.Type) {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if id != "" {
			if _, repeated := held[id]; repeated {
				return true
			}
			held[id] = struct{}{}
		}
		blocks = append(blocks, Block{ID: id, Type: node.Type, Refs: refsOf(node)})
		return true
	})
	return blocks
}

// refsOf is every reference node's own `block[]` attributes hold, read by pmdoc.BlockPointers
// over node alone: its attributes without its content, so a typed block nested inside it is read
// as a block of its own and lends it none of its references.
func refsOf(node *pmdoc.Node) []Ref {
	pointers := pmdoc.BlockPointers(&pmdoc.Node{Type: node.Type, Attrs: node.Attrs})
	if len(pointers) == 0 {
		return nil
	}
	refs := make([]Ref, len(pointers))
	for index, pointer := range pointers {
		target := pointer.Target
		primary := target.IssueKey != "" && target.ID == "spec"
		slug := target.ID
		if primary {
			slug = ""
		}
		refs[index] = Ref{
			Attribute: pointer.Attribute, Text: pointer.Ref, Issue: target.IssueKey, Project: target.Project,
			Primary: primary, Slug: slug, Block: target.Block, Pin: target.Pin,
		}
	}
	return refs
}
