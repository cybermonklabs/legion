package pmdoc

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ErrBlockPointer is a write refused because a `block[]` attribute it introduces points at a
// target the attribute may not name (CheckBlockPointers). It is the caller's to fix, never a
// schema refusal (ErrSchema): the tree it names is inside the schema.
var ErrBlockPointer = errors.New("block pointer refused")

// BlockPointer is one reference a typed block's `block[]` attribute holds: the typed block's id
// and type, the attribute, the reference as written and as read, the attribute's targets (none:
// any block of a document), the item kinds it declares beside them (none: blocks of documents
// alone) and the scope its declaration limits it to (none: any block of the writing document's
// own issue or of one above it).
type BlockPointer struct {
	Block     string
	BlockType string
	Attribute string
	Ref       string
	Target    text.Ref
	Targets   []string
	Items     []BlockItemKind
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
			declared := typ.Attributes.Get(name)
			for _, ref := range refs {
				target, canonical := pointerTarget(ref, declared.Items)
				if !canonical {
					continue
				}
				pointers = append(pointers, BlockPointer{
					Block: id, BlockType: node.Type, Attribute: name, Ref: ref, Target: target,
					Targets: declared.Targets, Items: declared.Items, Scope: declared.Scope,
				})
			}
		}
		return true
	})
	return pointers
}

// BlockTarget is a target a pointer names, as the write naming it would leave it. A block of a
// document carries the types the block index holds under its node id, the document holding it
// (an artifact's reference key), and that document's issue, "" for a document a project holds. An
// ask, a comment or a message, or a block of one, carries its item's own issue, whatever issue
// the reference spells, and whether it is a conversation node at the write's time
// (refs.IsConversationNode). An issue carries its own key as its issue.
type BlockTarget struct {
	Types        []string
	Document     string
	Issue        string
	Conversation bool
}

// BlockWriting is where the write holding the pointers lands: the document it writes (an
// artifact's reference key), that document's issue, "" for a document a project holds, the issues
// above it, `child_of` upward (refs.IssueAncestors), and the issues below it, `child_of`
// downward (refs.IssueDescendants), which only a pointer at an issue reads. Creatable answers the
// creatable number of the processes serving the database: the highest since every live process
// reads and some process creates, which bounds what a page may write (docs' schemaFleet). It is
// called only for a pointer at an item of a kind its attribute's ItemVersions give a
// creatable_since above 1, at most once, and a nil Creatable answers 0.
type BlockWriting struct {
	Document    string
	Issue       string
	Ancestors   []string
	Descendants []string
	Creatable   func() (int, error)
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
// A pointer at a block of a document is refused when it names a block no document holds, a
// block of a type the attribute's targets do not allow (more than one type is refused when any is
// outside them), a block of an issue that is neither the writing document's own issue nor one
// above it, a block of another document when a project holds the writing document and no issue
// does, or a block of another document under an attribute the schema scopes to its own document
// (BlockAttributeScope). A pointer at an ask, a comment or a message, or at a block of one (an
// item its attribute's items declare), is refused when no row holds it under the issue the
// reference spells, when that issue is neither the writing document's own nor one above it, and
// when the item is no conversation node (refs.IsConversationNode), such as an agent's status
// comment or a message of the Legion daemon's session, where no walk of the issue's tree reaches
// it. A pointer at an issue is refused when no issue has the key, and unless the issue stands
// under the writing document's issue (refs.IssueDescendants). A pointer at an item of a kind its
// attribute gives a creatable_since above 1 (ItemVersions), such as an issue under a plan task's
// needs, is refused, as a write of an element this build may not create is refused
// (refuseUncreatable), while the processes serving the database cannot all read it
// (BlockWriting.Creatable below its creatable_since): a build before that version reads it as a
// value outside the schema, not as newer content it leaves alone. Only a write that comes this way
// is held to this: a browser's edit never does, its page being admitted writable only under the
// same bound (docs' schemaFleet.admits), and a pointer it leaves at a block no document holds stays
// the unresolved reference it is.
//
// resolve is called at most once, with the target of each introduced pointer whose attribute has
// targets or items, and answers where the write lands and the target each names by node id
// (text.TargetID), with none for an id no block, item, item block or issue has. Its error is
// returned as it is, and so is Creatable's.
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
		if len(pointer.Targets) == 0 && len(pointer.Items) == 0 {
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
	creatable := -1
	for _, pointer := range introduced {
		// at names the pointer every refusal below is about: the block holding it, the attribute,
		// and the reference as written. It is built only by a refusal: a plan whose dozens of
		// pointers are all legal formats nothing.
		at := func() string {
			return fmt.Sprintf("typed block %q %s attribute %q points at %s",
				pointer.BlockType, TypedBlockIDAttribute(pointer.Block), pointer.Attribute, pointer.Ref)
		}
		if refusal, err := notCreatableItem(pointer, writing, &creatable); err != nil || refusal != "" {
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: %s, %s", ErrBlockPointer, at(), refusal)
		}
		target, holds := found[text.TargetID(pointer.Target)]
		var err error
		switch {
		case pointer.Target.Kind == "issue":
			err = issueReach(writing, target, holds)
		case ItemKindOf(pointer.Target) != "":
			err = itemReach(writing, pointer, target, holds)
		default:
			err = documentReach(writing, pointer, target, holds)
		}
		if err != nil {
			return fmt.Errorf("%w: %s, %w", ErrBlockPointer, at(), err)
		}
	}
	return nil
}

// notCreatableItem is the refusal of pointer, an introduced one, when it names an item of a kind
// its attribute gives a creatable_since above the processes serving the database may create
// (BlockWriting.Creatable), in refuseUncreatable's words, and "" when it may stand. creatable is
// the number Creatable answered, read once for the whole write; -1 until it is read.
func notCreatableItem(pointer BlockPointer, writing BlockWriting, creatable *int) (string, error) {
	item, named := itemOf(pointer.Target)
	if !named {
		return "", nil
	}
	element := Element{Kind: ItemElement, Block: pointer.BlockType, Attribute: pointer.Attribute, Name: string(item)}
	versions, declared := element.Versions()
	if !declared || versions.CreatableSince <= 1 {
		return "", nil
	}
	if *creatable < 0 {
		*creatable = 0
		if writing.Creatable != nil {
			fleet, err := writing.Creatable()
			if err != nil {
				return "", err
			}
			*creatable = fleet
		}
	}
	if *creatable >= versions.CreatableSince {
		return "", nil
	}
	return notCreatableYet(element), nil
}

// documentReach refuses a pointer at a block of a document: one no document holds, one of a type
// the attribute's targets do not allow, one outside the writing issue and the issues above it,
// one outside a writing document a project holds, and one outside the writing document under an
// attribute scoped to it.
func documentReach(writing BlockWriting, pointer BlockPointer, target BlockTarget, holds bool) error {
	if !holds {
		return errors.New("which no document holds")
	}
	for _, typ := range target.Types {
		if len(pointer.Targets) > 0 && !slices.Contains(pointer.Targets, typ) {
			return fmt.Errorf("a %q block; it points only at %s", typ, strings.Join(pointer.Targets, ", "))
		}
	}
	switch {
	case writing.Issue == "":
		if target.Document != writing.Document {
			return fmt.Errorf("a block of %s; a document a project holds, which no issue does, points only at a block of itself", target.Document)
		}
	case target.Issue != writing.Issue && !slices.Contains(writing.Ancestors, target.Issue):
		return fmt.Errorf("a block of %s; it points only at a block of %s or of an issue above it", issueOf(target), writing.Issue)
	}
	if pointer.Scope == BlockAttributeScopeDocument && target.Document != writing.Document {
		return fmt.Errorf("a block of %s; it points only at a block of its own document, %s", target.Document, writing.Document)
	}
	return nil
}

// itemReach refuses a pointer at an ask, a comment or a message, or a block of one: one no row
// holds under the issue its reference spells, one of an issue that is neither the writing issue
// nor one above it (and every one where a project holds the writing document), and one that is no
// conversation node.
func itemReach(writing BlockWriting, pointer BlockPointer, target BlockTarget, holds bool) error {
	item, issue := ItemKindOf(pointer.Target), pointer.Target.IssueKey
	// The kind takes the article its noun does: an ask, a comment, a message.
	what := "a " + item
	if item == "ask" {
		what = "an ask"
	}
	missing := fmt.Sprintf("which is no %s of %s", item, issue)
	if pointer.Target.Kind == "block" {
		what = "a block of " + what
		missing = fmt.Sprintf("which no %s of %s holds", item, issue)
	}
	switch {
	case !holds || target.Issue != issue:
		return errors.New(missing)
	case writing.Issue == "":
		return fmt.Errorf("%s of %s; a document a project holds, which no issue does, points only at a block of itself", what, issue)
	case issue != writing.Issue && !slices.Contains(writing.Ancestors, issue):
		return fmt.Errorf("%s of %s; it points only at one of %s or of an issue above it", what, issue, writing.Issue)
	case !target.Conversation:
		return fmt.Errorf("%s that is no conversation node: an ask is one, and a comment or a message is one where a person wrote it or a person wrote in its thread, but never a message of the Legion daemon's session", what)
	}
	return nil
}

// issueReach refuses a pointer at an issue: one no issue has the key of, and one that does not
// stand under the writing document's issue, a project's document having no issue under it.
func issueReach(writing BlockWriting, target BlockTarget, holds bool) error {
	switch {
	case !holds:
		return errors.New("which is no issue")
	case writing.Issue == "":
		return errors.New("an issue; a document a project holds, which no issue does, points at no issue")
	case !slices.Contains(writing.Descendants, target.Issue):
		return fmt.Errorf("an issue that is not under %s; it points only at an issue under its own", writing.Issue)
	}
	return nil
}

// CheckIssueTies refuses tree, the tree a write leaves, when a tie it introduces over prior names
// something of another issue than the writing document's: a value of a `block[]` attribute that
// declares no targets and no items (a decision's or an ask's `shaped`), which reaches a block of
// any document of that issue and of no other, or the value of an `ask` attribute (a decision's
// `from_ask`), which names an ask of that issue alone. One declaring targets or items (a parent)
// reaches an issue above as well, which CheckBlockPointers holds it to. It names the first such
// tie in document order, quoting the attribute and the value. A tie is introduced as
// CheckBlockPointers reads a pointer introduced: prior holds no tie with the same typed block id,
// typed block type, attribute and value; a nil prior holds none. A typed block repeating the id
// of a typed block before it is passed over, as BlockPointers passes over its pointers.
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
// each value of a `block[]` attribute declaring no targets and no items, and each value of an
// `ask` attribute, in document order and each block's attributes in the order its type declares
// them. A typed block repeating the id of a typed block before it holds none, as BlockPointers
// lists none of its pointers.
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
			case attribute.Kind == BlockAttributeBlockList && len(attribute.Targets) == 0 && len(attribute.Items) == 0:
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
