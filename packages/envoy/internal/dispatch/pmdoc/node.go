package pmdoc

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
)

type Attrs map[string]any

type Mark struct {
	Type  string
	Attrs Attrs
}

type Node struct {
	Type     string
	Attrs    Attrs
	Text     string
	Marks    []Mark
	Children []*Node
}

var ErrSchema = errors.New("outside Proof schema")

// ErrBlockHTML is the refusal of HTML that markdown reads as a block: the Proof schema carries
// inline HTML only.
var ErrBlockHTML = fmt.Errorf("%w: block HTML is not accepted by Proof", ErrSchema)

// ErrUnknownElement marks a schema refusal (ErrSchema) of an element this build does not know: a
// node, mark or typed block its manifest does not name, an attribute a typed block does not
// declare, or a value an enum attribute does not list. Two builds serve one database during a
// release, so a newer build can have written it. A reader tells it from a known element in a
// broken shape, which is ErrSchema alone.
var ErrUnknownElement = errors.New("element this build does not know")

// unknownElementError is a refusal of an element this build does not know: err, which names it,
// is also ErrUnknownElement, and keeps its own message. elements names those elements as data, so
// a caller reads them without parsing the message (UnknownElements). It is used by pointer: a
// struct holding a slice cannot be compared, and a caller that compares two errors with == (as
// docs.askRefusalInstead's caller does) would panic on two values of it.
type unknownElementError struct {
	err      error
	elements []string
}

func (e *unknownElementError) Error() string { return e.err.Error() }

func (e *unknownElementError) Is(target error) bool { return target == ErrUnknownElement }

func (e *unknownElementError) Unwrap() error { return e.err }

// unknownElement marks err as a refusal of an element this build does not know
// (ErrUnknownElement), carrying names, the names of those elements, as data (UnknownElements).
func unknownElement(err error, names ...string) error {
	return &unknownElementError{err: err, elements: names}
}

// UnknownElements names the elements a refusal of an element this build does not know
// (ErrUnknownElement) is of: the type of the node, mark or typed block the manifest does not
// name, the attributes a typed block does not declare, or the value an enum attribute does not
// list. It is empty for every other error.
//
// It is there for a caller that records what a document holds without recording the refusal
// itself: an error's message carries whatever the chain that answered the request wrapped it in,
// the caller's own identity among it, and these names carry the document and nothing else.
func UnknownElements(err error) []string {
	var unknown *unknownElementError
	if errors.As(err, &unknown) {
		return unknown.elements
	}
	return nil
}

// sortMarks puts marks in one order whatever order they arrive in: by type, then, for two marks of
// one type (the record marks, which may overlap: anchorMarkTypes), by id, and by their attributes
// only when the ids tie. Every positional comparison of two mark sets (marksEqual, run joining)
// relies on it, and so does every reading of marks in writing order (visibleMarks). The sort is
// unstable, which no output can see: two marks markLess orders neither way are equal in type, in
// id and in the JSON of their canonical attributes, so each stands for the other wherever a mark
// is read.
func sortMarks(marks []Mark) {
	slices.SortFunc(marks, func(left, right Mark) int {
		switch {
		case markLess(left, right):
			return -1
		case markLess(right, left):
			return 1
		}
		return 0
	})
}

func markLess(left, right Mark) bool {
	if left.Type != right.Type {
		return left.Type < right.Type
	}
	leftID, _ := left.Attrs["id"].(string)
	rightID, _ := right.Attrs["id"].(string)
	if leftID != rightID {
		return leftID < rightID
	}
	leftJSON, _ := json.Marshal(canonicalAttrs(left.Attrs))
	rightJSON, _ := json.Marshal(canonicalAttrs(right.Attrs))
	return string(leftJSON) < string(rightJSON)
}

func marksEqual(left, right []Mark) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Type != right[i].Type || !attrsEqual(left[i].Attrs, right[i].Attrs) {
			return false
		}
	}
	return true
}

func sortNodeMarks(n *Node) {
	sortMarks(n.Marks)
	for _, child := range n.Children {
		sortNodeMarks(child)
	}
}

// yattrToMarkName is the mark type stored under the Y.Text attribute key name: a record mark's key
// carries a `--` and an 8-character digest after its type (markAttributeKey), which it drops, and
// any other key is the type itself. It runs for every attribute key of every Y.Text run read, so
// it checks the key's bytes rather than matching the pattern it stands for, `^(.*)--[a-zA-Z0-9+/=]{8}$`,
// whose `.` matches no newline.
func yattrToMarkName(name string) string {
	const suffix = len("--") + 8
	if len(name) < suffix || name[len(name)-suffix:len(name)-8] != "--" {
		return name
	}
	for index := len(name) - 8; index < len(name); index++ {
		if !isMarkKeyDigestByte(name[index]) {
			return name
		}
	}
	if prefix := name[:len(name)-suffix]; !strings.Contains(prefix, "\n") {
		return prefix
	}
	return name
}

// isMarkKeyDigestByte reports whether b may stand in a record mark key's digest: a standard base64
// character.
func isMarkKeyDigestByte(b byte) bool {
	return isASCIIAlphaNumeric(b) || b == '+' || b == '/' || b == '='
}

func (n *Node) Validate() error {
	return validateNode(n, 0)
}

// validateWithin is the validation of doc, the tree markdown parsed to, as the children a block of
// type parent holds after its own: each is held to the schema as Validate holds it, and each is a
// block or of a type parent admits after its content (AdmitsAfterContent), which stands nowhere
// at a document's level. parent "" is the document itself (Validate). Where among the block's
// children such a block may stand is the block's content rule, which only the whole block can be
// held to (InsertInto).
func validateWithin(doc *Node, parent string) error {
	if parent == "" {
		return doc.Validate()
	}
	for _, child := range doc.Children {
		if err := validateNode(child, 1); err != nil {
			return err
		}
		if isBlockNodeType(child.Type) || AdmitsAfterContent(parent, child.Type) {
			continue
		}
		if err := misplacedBlock([]*Node{child}); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s cannot hold %s", ErrSchema, blockName(parent), blockName(child.Type))
	}
	return nil
}

func validateNode(n *Node, depth int) error {
	if n == nil {
		return fmt.Errorf("%w: nil node", ErrSchema)
	}
	if err := treeDepthError(depth); err != nil {
		return err
	}
	manifest := active.Load()
	if !manifest.nodes[n.Type] {
		if _, typed := manifest.types[n.Type]; !typed {
			return unknownElement(fmt.Errorf("%w: node %q", ErrSchema, n.Type), n.Type)
		}
	}
	if n.Type != "text" && len(n.Marks) != 0 {
		return fmt.Errorf("%w: node %q cannot have marks", ErrSchema, n.Type)
	}
	for _, m := range n.Marks {
		if !manifest.marks[m.Type] {
			return unknownElement(fmt.Errorf("%w: mark %q", ErrSchema, m.Type), m.Type)
		}
		if err := attrNestingError("mark", m.Type, m.Attrs, maxMarkAttrNesting); err != nil {
			return err
		}
	}
	if err := attrNestingError("node", n.Type, n.Attrs, maxNodeAttrNesting); err != nil {
		return err
	}
	for _, child := range n.Children {
		if err := validateNode(child, depth+1); err != nil {
			return err
		}
	}

	switch n.Type {
	case "doc", "blockquote", "footnote_definition":
		if len(n.Children) == 0 || !childrenAreBlocks(n.Children) {
			if err := misplacedBlock(n.Children); err != nil {
				return err
			}
			return fmt.Errorf("%w: %s requires block children", ErrSchema, n.Type)
		}
	case "paragraph", "heading":
		if !childrenAreInline(n.Children) {
			return fmt.Errorf("%w: %s requires inline children", ErrSchema, n.Type)
		}
	case "bullet_list", "ordered_list":
		if len(n.Children) == 0 || !childrenAre(n.Children, "list_item") {
			return fmt.Errorf("%w: %s requires list items", ErrSchema, n.Type)
		}
	case "list_item":
		if len(n.Children) == 0 || n.Children[0].Type != "paragraph" || !childrenAreBlocks(n.Children[1:]) {
			if err := misplacedBlock(n.Children); err != nil {
				return err
			}
			return fmt.Errorf("%w: list item requires a paragraph followed by blocks", ErrSchema)
		}
	case "code_block":
		if !childrenAre(n.Children, "text") {
			return fmt.Errorf("%w: code block requires text children", ErrSchema)
		}
		for _, child := range n.Children {
			for _, mark := range child.Marks {
				if mark.Type != "proofAuthored" && mark.Type != "proofSuggestion" && mark.Type != "proofComment" && mark.Type != "proofFlagged" && mark.Type != "proofApproved" {
					return fmt.Errorf("%w: code block cannot contain mark %q", ErrSchema, mark.Type)
				}
			}
		}
	case "table":
		if len(n.Children) < 2 || n.Children[0].Type != "table_header_row" || !childrenAre(n.Children[1:], "table_row") {
			return fmt.Errorf("%w: table requires a header row followed by rows", ErrSchema)
		}
	case "table_header_row":
		if !childrenAre(n.Children, "table_header") {
			return fmt.Errorf("%w: table header row requires header cells", ErrSchema)
		}
	case "table_row":
		if !childrenAre(n.Children, "table_cell") {
			return fmt.Errorf("%w: table row requires cells", ErrSchema)
		}
	case "table_header", "table_cell":
		if !childrenAre(n.Children, "paragraph") || len(n.Children) != 1 {
			return fmt.Errorf("%w: %s requires one paragraph", ErrSchema, n.Type)
		}
	case "frontmatter":
		if !childrenAre(n.Children, "text") {
			return fmt.Errorf("%w: frontmatter requires text children", ErrSchema)
		}
	case "text", "hr", "hardbreak", "image", "html", "footnote_reference":
		if len(n.Children) != 0 {
			return fmt.Errorf("%w: %s cannot have children", ErrSchema, n.Type)
		}
	default:
		if typ, typed := typedBlock(n.Type); typed {
			return validateTypedBlock(n, typ)
		}
	}
	return nil
}

// undeclaredAttributes is the refusal for a typed block holding attributes its schema does not
// declare, naming every one of them in sorted order, or nil when it declares them all.
func undeclaredAttributes(typeName string, attrs Attrs, declared BlockAttributes) error {
	var names []string
	for name := range attrs {
		if !declared.Has(name) && name != BlockIDAttr {
			names = append(names, name)
		}
	}
	switch len(names) {
	case 0:
		return nil
	case 1:
		return unknownElement(fmt.Errorf("%w: typed block %q does not declare attribute %q", ErrSchema, typeName, names[0]), names[0])
	}
	sort.Strings(names)
	quoted := make([]string, len(names))
	for index, name := range names {
		quoted[index] = fmt.Sprintf("%q", name)
	}
	return unknownElement(fmt.Errorf("%w: typed block %q does not declare attributes %s", ErrSchema, typeName, strings.Join(quoted, ", ")), names...)
}

// validateTypedBlock holds n, a block of typ, to its schema: what it holds and its attributes. A
// block of a type the browser editor keeps in any order (keepsAnyOrder) is held to holding only
// blocks it may hold, in any order; every other block to its content rule (typedContentError).
func validateTypedBlock(n *Node, typ BlockTypeSchema) error {
	if keepsAnyOrder(n.Type) {
		held := len(n.Children) > 0
		for _, child := range n.Children {
			held = held && child != nil && (isBlockNodeType(child.Type) || AdmitsAfterContent(n.Type, child.Type))
		}
		if !held {
			return fmt.Errorf("%w: typed block %q requires block children", ErrSchema, n.Type)
		}
	} else if err := typedContentError(n, typ); err != nil {
		return err
	}
	if err := undeclaredAttributes(n.Type, n.Attrs, typ.Attributes); err != nil {
		return err
	}
	for name, value := range n.Attrs {
		if name == BlockIDAttr {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%w: typed block %q blockId must be a string", ErrSchema, n.Type)
			}
			continue
		}
		definition := typ.Attributes.Get(name)
		if nullReadsAbsent(typ, BlockAttribute{Name: name, BlockAttributeSchema: definition}, value) {
			continue
		}
		if err := validateAttributeValue(definition, value); err != nil {
			return fmt.Errorf("%w: typed block %q attribute %q: %w", ErrSchema, n.Type, name, err)
		}
	}
	// A block an earlier build wrote may lack an attribute added since: it reads with the
	// attribute's default (writtenDefault), so the attribute is not required here.
	return nil
}

// keepsAnyOrder reports whether the browser editor keeps a block of type name holding its
// children in any order (proof-editor's relaxAskContentCheck): an ask, and a block of a type that
// admits blocks after its own content (TypedContent: a decision, which readings close).
// Concurrent browser edits can leave one off its content rule - an ask with two bullet lists or a
// paragraph after its list, a decision or an ask with a paragraph landing after a reading another
// writer added at the same place - and a tree the browser cannot build is deleted from the shared
// document, its text with it, so the browser builds it and the server keeps it: settlement marks
// such an ask `invalid`. Markdown written in whole is held to the rule all the same, an ask by its
// own written rule (AskWrittenContentError) and every other such block by its content rule
// (writtenContentError).
func keepsAnyOrder(name string) bool {
	return name == "ask" || len(active.Load().admitted[name]) > 0
}

// typedContentError is the refusal of n, a block of typ, whose children break typ's content rule,
// or nil. The blocks of the types it admits after its content (TypedContent) are no part of the
// rule, which reads the rest as it reads a block that holds none; a block of such a type standing
// anywhere else is refused by name (misplacedBlock).
func typedContentError(n *Node, typ BlockTypeSchema) error {
	content, _ := TypedContent(n)
	switch typ.Content {
	case BlockContentParagraphs:
		if len(content) == 0 || !childrenAre(content, "paragraph") {
			if err := misplacedBlock(content); err != nil {
				return err
			}
			return fmt.Errorf("%w: typed block %q content %q requires one or more paragraphs", ErrSchema, n.Type, typ.Content)
		}
	case BlockContentBlocks:
		if len(content) == 0 || !childrenAreBlocks(content) {
			if err := misplacedBlock(content); err != nil {
				return err
			}
			return fmt.Errorf("%w: typed block %q content %q requires one or more blocks", ErrSchema, n.Type, typ.Content)
		}
	case BlockContentParagraphsOptionalBulletList:
		if askContentBreak(content) != "" {
			if err := misplacedBlock(content); err != nil {
				return err
			}
			return fmt.Errorf("%w: typed block %q content %q requires paragraphs followed by an optional bullet list", ErrSchema, n.Type, typ.Content)
		}
	default:
		return fmt.Errorf("%w: typed block %q has unsupported content rule %q", ErrSchema, n.Type, typ.Content)
	}
	return nil
}

// writtenContentError holds each block of doc, a tree of markdown a caller wrote in whole, whose
// type the browser editor keeps in any order (keepsAnyOrder) to its content rule
// (typedContentError), naming the first that breaks it in document order, or is nil: such
// markdown is written in one piece, so no concurrent edit left it off its rule. An ask is held to
// its own written rule instead (AskWrittenContentError), which every write of the document
// service runs and answers INVALID_ASK_BLOCK.
func writtenContentError(doc *Node) error {
	var found error
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if found != nil {
			return false
		}
		if node.Type != "ask" && keepsAnyOrder(node.Type) {
			typ, _ := typedBlock(node.Type)
			found = typedContentError(node, typ)
		}
		return found == nil
	})
	return found
}

// AskContentError reports the first ask block in node that breaks its content rule, paragraph+
// bullet_list? - one or more paragraphs, then at most one bullet list, last - or nil. The readings
// an ask admits after that content (TypedContent) are no part of it. Validate lets an ask hold
// other blocks, since concurrent browser edits can leave one there, the browser keeps it, and
// settlement marks the ask `invalid`; markdown written in whole is held to the rule, as the
// browser editor's parser holds it.
//
// An ask whose question a person's edit emptied keeps the rule, options and all: the document
// service keeps such a block and repairs it through its question (refuseEmptyQuestionEdit,
// docs/ask_block_text.go), and a refusal of this check must not name it. Markdown that writes a
// question nowhere is AskWrittenContentError's to refuse.
func AskContentError(node *Node) error {
	return askContentError(node, false)
}

// AskWrittenContentError is AskContentError over markdown written in whole - an upload, a
// replacement, a repair - where an ask written with its options and no question is refused too.
// Both parsers read such markdown holding the empty paragraph the Proof schema fills in ahead of
// the options (emptyParagraphFirst), so the content rule itself no longer catches what, before
// that paragraph was read, it refused as a bullet list before the question; the write is refused
// here instead, by what is wrong with it.
func AskWrittenContentError(node *Node) error {
	return askContentError(node, true)
}

func askContentError(node *Node, written bool) error {
	var found error
	walk(node, func(ask *Node, _ []int, _, _ int) bool {
		if found != nil {
			return false
		}
		if ask.Type != "ask" {
			return true
		}
		content, _ := TypedContent(ask)
		broken := askContentBreak(content)
		if broken == "" {
			if written && askWritesNoQuestion(content) {
				id, _ := ask.Attrs[BlockIDAttr].(string)
				found = fmt.Errorf("ask block %q holds its options and no question, which no one can answer, where its content rule, %s, reads the question as the paragraph before them; write it on the line under the block's opening fence, above its options", id, BlockContentParagraphsOptionalBulletList)
				return false
			}
			return true
		}
		id, _ := ask.Attrs[BlockIDAttr].(string)
		found = fmt.Errorf("ask block %q%s holds %s where its content rule, %s, allows one or more paragraphs and then at most one bullet list, last", id, askQuestionOpening(ask), broken, BlockContentParagraphsOptionalBulletList)
		return false
	})
	return found
}

// askQuestionOpening is `, asking "<its question's opening words>"`, or "" for an ask that opens
// with no question. A refusal names an ask by its block id, which the parser makes up when the
// markdown gives none, so the question is how its author knows which ask is meant.
func askQuestionOpening(ask *Node) string {
	if len(ask.Children) == 0 || ask.Children[0] == nil || ask.Children[0].Type != "paragraph" {
		return ""
	}
	question := openingWords(oneLineText(ask.Children[0]))
	if question == "" {
		return ""
	}
	return fmt.Sprintf(", asking %q,", question)
}

// openingWords is text's first 40 characters, cut short with an ellipsis where it runs on: how a
// refusal names a block whose author has only its text to find it by.
func openingWords(text string) string {
	const opening = 40
	runes := []rune(text)
	if len(runes) <= opening {
		return text
	}
	return strings.TrimRight(string(runes[:opening]), " ") + "…"
}

// askWritesNoQuestion reports whether these children are an ask's whose markdown wrote its
// options and no question: one or more paragraphs that hold nothing - the one the Proof schema
// fills in ahead of a list (emptyParagraphFirst), and any the author wrote out of inlines that
// write no bytes, a link with no text among them - and then that list. A question emptied by a
// person's edit leaves the same shape, which the document service keeps and repairs through its
// question (refuseEmptyQuestionEdit, docs/ask_block_text.go), so only markdown written in whole
// is held to this (AskWrittenContentError).
func askWritesNoQuestion(children []*Node) bool {
	last := len(children) - 1
	if last < 1 || children[last] == nil || children[last].Type != "bullet_list" {
		return false
	}
	for _, child := range children[:last] {
		if child == nil || child.Type != "paragraph" || len(child.Children) != 0 {
			return false
		}
	}
	return true
}

// askContentBreak names the first child that breaks the content rule paragraph+ bullet_list? -
// one or more paragraphs, then at most one bullet list, last - or is "" when the children keep it.
func askContentBreak(children []*Node) string {
	if len(children) == 0 {
		return "nothing"
	}
	for index, child := range children {
		switch {
		case child == nil:
			return "a missing block"
		case index == 0 && child.Type != "paragraph":
			return blockName(child.Type) + " before its question"
		case child.Type == "paragraph" && index > 0 && children[index-1].Type == "bullet_list":
			return "a paragraph after its options"
		case child.Type == "bullet_list" && index > 0 && children[index-1].Type == "bullet_list":
			return "a second bullet list"
		case child.Type != "paragraph" && child.Type != "bullet_list":
			return blockName(child.Type)
		}
	}
	return ""
}

// blockName is a block type as a reader names it, with its article: "a code block", "an
// ordered list", "a horizontal rule".
func blockName(nodeType string) string {
	name := blockNoun(nodeType)
	if strings.ContainsRune("aeiou", rune(name[0])) {
		return "an " + name
	}
	return "a " + name
}

func blockNoun(nodeType string) string {
	if nodeType == "hr" {
		return "horizontal rule"
	}
	return strings.ReplaceAll(nodeType, "_", " ")
}

// BlockNames names blocks in order as a reader names them, a run of one type counted: "a bullet
// list", "two paragraphs", "a horizontal rule and a heading".
func BlockNames(blocks []*Node) string {
	var names []string
	for start := 0; start < len(blocks); {
		end := start + 1
		for end < len(blocks) && blocks[end].Type == blocks[start].Type {
			end++
		}
		if count := end - start; count > 1 {
			names = append(names, fmt.Sprintf("%s %ss", countWord(count), blockNoun(blocks[start].Type)))
		} else {
			names = append(names, blockName(blocks[start].Type))
		}
		start = end
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func countWord(count int) string {
	if words := []string{"two", "three", "four", "five", "six", "seven", "eight", "nine"}; count-2 < len(words) {
		return words[count-2]
	}
	return fmt.Sprint(count)
}

func childrenAre(children []*Node, typeName string) bool {
	for _, child := range children {
		if child == nil || child.Type != typeName {
			return false
		}
	}
	return true
}

func childrenAreBlocks(children []*Node) bool {
	for _, child := range children {
		if child == nil || !isBlockNodeType(child.Type) {
			return false
		}
	}
	return true
}

func childrenAreInline(children []*Node) bool {
	for _, child := range children {
		if child == nil || !isInlineNodeType(child.Type) {
			return false
		}
	}
	return true
}

// blockNodeTypesJSON lists the block node types besides the typed blocks. The proof editor's
// schema drift test reads the same file, so the editor's `block` group and this set stay equal.
//
//go:embed schema/block-node-types.json
var blockNodeTypesJSON []byte

var blockNodeTypes = func() map[string]struct{} {
	var names []string
	if err := json.Unmarshal(blockNodeTypesJSON, &names); err != nil {
		panic(fmt.Sprintf("pmdoc: decode embedded block node types: %v", err))
	}
	if len(names) == 0 {
		panic("pmdoc: embedded block node types are empty")
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}()

// isBlockNodeType reports whether typeName is in the block group: a block node of the Proof
// schema, or a typed block that names no parents (BlockTypeSchema.Parents), since one that does
// stands only where a parent admits it (TypedContent).
func isBlockNodeType(typeName string) bool {
	if _, ok := blockNodeTypes[typeName]; ok {
		return true
	}
	manifest := active.Load()
	_, typed := manifest.types[typeName]
	return typed && !manifest.placed[typeName]
}

// TypedContent splits a typed block's children in two: the content its own content rule holds,
// and the trailing run of blocks of the types it admits after that content (the types whose
// parents name it, BlockTypeSchema.Parents), such as the readings a decision or an ask closes
// with. A block of such a type anywhere before the last block of another type is content, which
// the rule then refuses. A block of no type, or of one that admits none, holds content alone.
func TypedContent(node *Node) (content, admitted []*Node) {
	admits := active.Load().admitted[node.Type]
	end := len(node.Children)
	for end > 0 && node.Children[end-1] != nil && admits[node.Children[end-1].Type] {
		end--
	}
	return node.Children[:end], node.Children[end:]
}

// AdmitsAfterContent reports whether a block of type parent admits one of type child after its
// own content (TypedContent).
func AdmitsAfterContent(parent, child string) bool {
	return active.Load().admitted[parent][child]
}

// misplacedBlock is the refusal of the first of children whose type names parents, which stands
// where none of them admits it (TypedContent leaves such a block in a parent's content), or nil.
// The browser editor holds such a type in no node group, so its parser would drop the block
// there, text and all.
func misplacedBlock(children []*Node) error {
	manifest := active.Load()
	for _, child := range children {
		if child == nil || !manifest.placed[child.Type] {
			continue
		}
		parents := manifest.types[child.Type].Parents
		named := make([]string, len(parents))
		for index, parent := range parents {
			named[index] = blockName(parent)
		}
		id, _ := child.Attrs[BlockIDAttr].(string)
		return fmt.Errorf("%w: typed block %q %q stands only at the end of %s, after that block's own content", ErrSchema, child.Type, id, strings.Join(named, " or "))
	}
	return nil
}

func isInlineNodeType(typeName string) bool {
	switch typeName {
	case "text", "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}

// Equal compares document content and structure while ignoring block identity.
func (n *Node) Equal(o *Node) bool {
	return equalNode(n, o, func(_ string, a, b Attrs) bool { return attrsEqualButBlockID(a, b) })
}

// EqualWithBlockIDs compares document content, structure, and block identity.
func (n *Node) EqualWithBlockIDs(o *Node) bool {
	return equalNode(n, o, func(_ string, a, b Attrs) bool { return attrsEqual(a, b) })
}

// EqualOutsideServerState compares document content, structure and block identity as
// EqualWithBlockIDs does, but for the attributes of a typed block that the server keeps (the
// schema's server attributes): an ask's state, who answered it, when and with what, and whether it
// reads. No caller writes those, and none of them is the block's own text: a person's answer is
// held once, on the ask's node, and while two builds serve the server
// keeps a copy of it in the block for the build before (docs' rollWindowAskAttributes).
func (n *Node) EqualOutsideServerState(o *Node) bool {
	return equalNode(n, o, attrsEqualOutsideServerState)
}

func attrsEqualOutsideServerState(nodeType string, a, b Attrs) bool {
	typ, typed := typedBlock(nodeType)
	if !typed {
		return attrsEqual(a, b)
	}
	return attrsEqualBut(a, b, func(name string) bool { return typ.Attributes.Get(name).Server })
}

// equalNode compares n and o, their content and structure, holding each node's attributes to
// attrs, which is given the node's type.
func equalNode(n, o *Node, attrs func(nodeType string, a, b Attrs) bool) bool {
	if n == nil || o == nil {
		return n == o
	}
	if n.Type != o.Type || n.Text != o.Text || !attrs(n.Type, n.Attrs, o.Attrs) || len(n.Marks) != len(o.Marks) || len(n.Children) != len(o.Children) {
		return false
	}
	for i := range n.Marks {
		if n.Marks[i].Type != o.Marks[i].Type || !attrsEqual(n.Marks[i].Attrs, o.Marks[i].Attrs) {
			return false
		}
	}
	for i := range n.Children {
		if !equalNode(n.Children[i], o.Children[i], attrs) {
			return false
		}
	}
	return true
}

// attrsEqualButBlockID is attrsEqual over every attribute but the block id.
func attrsEqualButBlockID(a, b Attrs) bool {
	return attrsEqualBut(a, b, func(name string) bool { return name == BlockIDAttr })
}

// attrsEqual treats nil and empty as equal and compares values by JSON semantics.
func attrsEqual(a, b Attrs) bool {
	return attrsEqualBut(a, b, func(string) bool { return false })
}

// attrsEqualBut is attrsEqual over the attributes ignored does not name.
func attrsEqualBut(a, b Attrs, ignored func(name string) bool) bool {
	for key, av := range a {
		if ignored(key) {
			continue
		}
		bv, ok := b[key]
		if !ok {
			if av == nil {
				continue
			}
			return false
		}
		if !reflect.DeepEqual(normalizeJSON(av), normalizeJSON(bv)) {
			return false
		}
	}
	for key, bv := range b {
		if ignored(key) {
			continue
		}
		if _, ok := a[key]; !ok && bv != nil {
			return false
		}
	}
	return true
}

// normalizeJSON makes equivalent wire representations compare equal.
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float32:
		return float64(x)
	case []string:
		out := make([]any, len(x))
		for index := range x {
			out[index] = x[index]
		}
		return out
	case []any:
		out := make([]any, len(x))
		for index := range x {
			out[index] = normalizeJSON(x[index])
		}
		return out
	default:
		return x
	}
}

type jsonNode struct {
	Type    string      `json:"type"`
	Attrs   Attrs       `json:"attrs,omitempty"`
	Content []*jsonNode `json:"content,omitempty"`
	Text    string      `json:"text,omitempty"`
	Marks   []jsonMark  `json:"marks,omitempty"`
}

type jsonMark struct {
	Type  string `json:"type"`
	Attrs Attrs  `json:"attrs,omitempty"`
}

func (n *Node) toJSON() *jsonNode {
	j := &jsonNode{Type: n.Type, Attrs: n.Attrs, Text: n.Text}
	for _, m := range n.Marks {
		j.Marks = append(j.Marks, jsonMark{Type: m.Type, Attrs: m.Attrs})
	}
	for _, c := range n.Children {
		j.Content = append(j.Content, c.toJSON())
	}
	return j
}

func (n *Node) JSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(n.toJSON())
}
