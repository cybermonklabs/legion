package pmdoc

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ErrBlockPointer is a write refused because a `block[]` attribute it introduces points at a block
// of a type the attribute's targets do not allow (CheckBlockPointers). It is the caller's to fix,
// never a schema refusal (ErrSchema): the tree it names is inside the schema.
var ErrBlockPointer = errors.New("block pointer refused")

// BlockPointer is one reference a typed block's `block[]` attribute holds: the typed block's id
// and type, the attribute, the canonical block reference as written and as read, the attribute's
// targets (none: any block) and the scope its declaration limits it to (none: any block of the
// writing document's own issue or of one above it).
type BlockPointer struct {
	Block     string
	BlockType string
	Attribute string
	Ref       string
	Target    text.Ref
	Targets   []string
	Scope     BlockAttributeScope
}

// BlockPointers lists, in document order, each reference each typed block's `block[]` attributes
// hold, a block's attributes by name. A schema declaring no `block[]` attribute lists none
// without walking tree. A tree inside the schema holds only canonical
// references (ParseBlockPointerValue); any other value is skipped.
//
// A typed block repeating the id of a typed block before it in document order lists none: the
// block index takes the first holder of an id for its node (refs.PlanBlocks). Only typed blocks
// count here, because only their ids are written in markdown, and so are the same in the tree a
// version write indexes and in the markdown the reconciliation reads back: the rendering writes a
// typed block's id so that it reads back as the tree holds it (TypedBlockIDAttribute), each U+0000
// aside, which it writes U+FFFD (withNulsWritten), so each block is listed under its id as
// written. A paragraph's id is not written in markdown, and a list item or table cell has no
// offset, so it is never the index's node. An earlier holder that is not typed but has an offset
// (a paragraph) is caught by the index itself, which writes a pointer only from a node of the
// pointer's own block type (refs.PlanBlocks).
func BlockPointers(tree *Node) []BlockPointer {
	manifest := active.Load()
	if len(manifest.linkKinds) == 0 {
		return nil
	}
	var pointers []BlockPointer
	held := make(map[string]struct{})
	Walk(tree, func(node *Node) bool {
		typ, typed := manifest.types[node.Type]
		if !typed {
			return true
		}
		id, _ := node.Attrs[BlockIDAttr].(string)
		id = nulAsReplacement(id)
		if id != "" {
			if _, repeated := held[id]; repeated {
				return true
			}
			held[id] = struct{}{}
		}
		for _, name := range manifest.pointerAttributes[node.Type] {
			refs, _ := stringSlice(node.Attrs[name])
			for _, ref := range refs {
				target, canonical := text.CanonicalBlockRef(ref)
				if !canonical {
					continue
				}
				declared := typ.Attributes.Get(name)
				pointers = append(pointers, BlockPointer{
					Block: id, BlockType: node.Type, Attribute: name, Ref: ref, Target: target,
					Targets: declared.Targets, Scope: declared.Scope,
				})
			}
		}
		return true
	})
	return pointers
}

// BlockTarget is a block a pointer names, as the write naming it would leave it: the types the
// block index holds under its node id, the document holding it (an artifact's reference key),
// and that document's issue, "" for a document a project holds.
type BlockTarget struct {
	Types    []string
	Document string
	Issue    string
}

// BlockWriting is where the write holding the pointers lands: the document it writes (an
// artifact's reference key), that document's issue, "" for a document a project holds, and the
// issues above it, `child_of` upward (refs.IssueAncestors).
type BlockWriting struct {
	Document  string
	Issue     string
	Ancestors []string
}

// CheckBlockPointers refuses tree, the tree a write leaves, when a pointer it introduces names a
// block the attribute may not name, naming the first such pointer in document order. A pointer is
// introduced when prior, the tree before the write, holds no pointer with the same typed block id,
// typed block type, attribute and reference: a new pointer, a changed value, or a block retyped
// into a type declaring the attribute, whatever the type before declared under that name. A nil
// prior (a new document, or one outside the schema) holds none. A pointer the write leaves
// unchanged is never refused, so a target retyped elsewhere blocks no unrelated edit. Its pointers
// are BlockPointers', so a typed block repeating the id of a typed block before it is never
// checked. The block index writes no edge for it while it repeats the id, but settlement's
// EnsureBlockIDs then gives it a fresh id, and its unchecked pointer becomes an edge, which the
// readers hide while its target has a type the attribute does not allow (block_ref_listed). A
// typed block repeating the id of an earlier paragraph is checked, though the index writes no edge
// for it either.
//
// A pointer is refused when it names a block no document holds, a block of a type the attribute's
// targets do not allow (more than one type is refused when any is outside them), a block of an
// issue that is neither the writing document's own issue nor one above it, a block of another
// document when a project holds the writing document and no issue does, or a block of another
// document under an attribute the schema scopes to its own document (BlockAttributeScope).
// Only a write that comes this way is held to this: a browser's edit never does, and a pointer it
// leaves at a block no document holds stays the unresolved reference it is.
//
// resolve is called at most once, with the target of each introduced pointer whose attribute has
// targets, and answers where the write lands and the block each target names by node id
// (text.TargetID), with none for an id no block has. Its error is returned as it is.
func CheckBlockPointers(tree, prior *Node, resolve func(targets []text.Ref) (BlockWriting, map[string]BlockTarget, error)) error {
	pointers := BlockPointers(tree)
	if len(pointers) == 0 {
		return nil
	}
	held := make(map[[4]string]struct{})
	for _, pointer := range BlockPointers(prior) {
		held[[4]string{pointer.Block, pointer.BlockType, pointer.Attribute, pointer.Ref}] = struct{}{}
	}
	var introduced []BlockPointer
	var targets []text.Ref
	for _, pointer := range pointers {
		if len(pointer.Targets) == 0 {
			continue
		}
		if _, unchanged := held[[4]string{pointer.Block, pointer.BlockType, pointer.Attribute, pointer.Ref}]; unchanged {
			continue
		}
		introduced = append(introduced, pointer)
		targets = append(targets, pointer.Target)
	}
	if len(introduced) == 0 {
		return nil
	}
	writing, found, err := resolve(targets)
	if err != nil {
		return err
	}
	for _, pointer := range introduced {
		// at names the pointer every refusal below is about: the block holding it, the attribute,
		// and the reference as written. It is built only by a refusal: a plan whose dozens of
		// pointers are all legal formats nothing.
		at := func() string {
			return fmt.Sprintf("typed block %q %s attribute %q points at %s",
				pointer.BlockType, TypedBlockIDAttribute(pointer.Block), pointer.Attribute, pointer.Ref)
		}
		target, holds := found[text.TargetID(pointer.Target)]
		if !holds {
			return fmt.Errorf("%w: %s, which no document holds", ErrBlockPointer, at())
		}
		for _, typ := range target.Types {
			if !slices.Contains(pointer.Targets, typ) {
				return fmt.Errorf("%w: %s, a %q block; it points only at %s",
					ErrBlockPointer, at(), typ, strings.Join(pointer.Targets, ", "))
			}
		}
		switch {
		case writing.Issue == "":
			if target.Document != writing.Document {
				return fmt.Errorf("%w: %s, a block of %s; a document a project holds, which no issue does, points only at a block of itself",
					ErrBlockPointer, at(), target.Document)
			}
		case target.Issue != writing.Issue && !slices.Contains(writing.Ancestors, target.Issue):
			return fmt.Errorf("%w: %s, a block of %s; it points only at a block of %s or of an issue above it",
				ErrBlockPointer, at(), issueOf(target), writing.Issue)
		}
		if pointer.Scope == BlockAttributeScopeDocument && target.Document != writing.Document {
			return fmt.Errorf("%w: %s, a block of %s; it points only at a block of its own document, %s",
				ErrBlockPointer, at(), target.Document, writing.Document)
		}
	}
	return nil
}

// CheckIssueTies refuses tree, the tree a write leaves, when a tie it introduces over prior names
// something of another issue than the writing document's: a value of a `block[]` attribute that
// declares no targets (a decision's or an ask's `shaped`), which reaches a block of any document
// of that issue and of no other, or the value of an `ask` attribute (a decision's `from_ask`),
// which names an ask of that issue alone. It names the first such tie in document order, quoting
// the attribute and the value. A tie is introduced as CheckBlockPointers reads a pointer
// introduced: prior holds no tie with the same typed block id, typed block type, attribute and
// value; a nil prior holds none. A typed block repeating the id of a typed block before it is
// passed over, as BlockPointers passes over its pointers.
//
// The issue a value names is the one its reference spells, the issue key or, for a document a
// project holds, the project, which is the editor's own reading (proof-editor's
// otherIssueRefusal), so a write the editor builds is one this accepts. issue answers the writing
// document's issue and is called only once tree introduces a tie; a document no issue holds ("")
// is held to neither rule, as the editor holds it to none.
func CheckIssueTies(tree, prior *Node, issue func() (string, error)) error {
	ties := issueTies(tree)
	if len(ties) == 0 {
		return nil
	}
	held := make(map[issueTie]struct{})
	for _, tie := range issueTies(prior) {
		held[tie] = struct{}{}
	}
	writing := ""
	asked := false
	for _, tie := range ties {
		if _, unchanged := held[tie]; unchanged {
			continue
		}
		if !asked {
			var err error
			if writing, err = issue(); err != nil {
				return err
			}
			asked = true
		}
		if writing == "" {
			return nil
		}
		named := ReferencedIssue(tie.value)
		if named == "" || named == writing {
			continue
		}
		at := fmt.Sprintf("typed block %q %s attribute %q", tie.blockType, TypedBlockIDAttribute(tie.block), tie.attribute)
		if tie.ask {
			return fmt.Errorf("%w: %s names %s, an ask of %s; it names only an ask of its own issue, %s",
				ErrBlockPointer, at, tie.value, named, writing)
		}
		return fmt.Errorf("%w: %s points at %s, a block of %s; it points only at a block of its own issue, %s",
			ErrBlockPointer, at, tie.value, named, writing)
	}
	return nil
}

// ReferencedIssue is the issue a `dispatch://` reference names, as a tie's value spells it: its
// issue key, or the project of a document a project holds, and "" for a value that is no such
// reference. CheckIssueTies holds a write to it, and the block index states no tie whose value
// names another issue than its document's (refs.DecisionFacts).
func ReferencedIssue(value string) string {
	ref, ok := text.ParseRef(value, "")
	if !ok {
		return ""
	}
	if ref.IssueKey != "" {
		return ref.IssueKey
	}
	return ref.Project
}

// issueTie is one value CheckIssueTies holds to the writing document's issue: the typed block
// holding it, the attribute, the value as written, and whether the attribute is of kind `ask`.
type issueTie struct {
	block, blockType, attribute, value string
	ask                                bool
}

// issueTies is every value of tree's typed blocks that ties the block to something of an issue:
// each value of a `block[]` attribute declaring no targets, and each value of an `ask` attribute,
// in document order and each block's attributes in the order its type declares them. A typed
// block repeating the id of a typed block before it holds none, as BlockPointers lists none of
// its pointers.
func issueTies(tree *Node) []issueTie {
	manifest := active.Load()
	var ties []issueTie
	held := make(map[string]struct{})
	Walk(tree, func(node *Node) bool {
		typ, typed := manifest.types[node.Type]
		if !typed {
			return true
		}
		id, _ := node.Attrs[BlockIDAttr].(string)
		id = nulAsReplacement(id)
		if id != "" {
			if _, repeated := held[id]; repeated {
				return true
			}
			held[id] = struct{}{}
		}
		for _, attribute := range typ.Attributes {
			switch {
			case attribute.Kind == BlockAttributeBlockList && len(attribute.Targets) == 0:
				values, _ := stringSlice(node.Attrs[attribute.Name])
				for _, value := range values {
					ties = append(ties, issueTie{block: id, blockType: node.Type, attribute: attribute.Name, value: value})
				}
			case attribute.Kind == BlockAttributeAsk:
				if value, _ := node.Attrs[attribute.Name].(string); value != "" {
					ties = append(ties, issueTie{block: id, blockType: node.Type, attribute: attribute.Name, value: value, ask: true})
				}
			}
		}
		return true
	})
	return ties
}

// issueOf names where a target's document sits, for a refusal: its issue, or the document itself
// when a project holds it and no issue does.
func issueOf(target BlockTarget) string {
	if target.Issue == "" {
		return target.Document + ", which a project holds and no issue does"
	}
	return target.Issue
}
