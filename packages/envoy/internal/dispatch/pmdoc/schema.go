package pmdoc

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

//go:embed schema/blocks.json
var blockSchemaJSON []byte

// The schema's shape - BlockSchema, BlockSchemaElement, BlockTypeSchema, BlockAttributeSchema,
// BlockElementVersions, BlockContentRule and the attribute kinds - is declared in
// packages/contracts/schemas/block-schema.schema.json and generated into schema_generated.go,
// which blocks.json above is read into (manifest.go, compileManifest). The manifest names the
// nodes and marks of the Proof schema beside the typed blocks Dispatch adds to it, and every
// element of it - a node, a mark, a typed block, a typed block's attribute, an enum attribute's
// choice - may say from which schema version a build reads it (since) and from which a build may
// create it (creatable_since); both are 1 when it says neither. This build reads every element
// the installed manifest declares and creates those whose creatable_since is at most Version
// (SchemaVersion). The installed manifest is the store's once the server has read it (Install):
// the typed blocks the store's supertag nodes declare, beside this build's own nodes and marks.
// blocks.json is what those nodes were seeded from, and what is installed until then.

// LinkKinds is the edge kinds the manifest's `block[]` attributes make, sorted: each such
// attribute's name, once however many types declare it. None is a structural edge kind
// (model.GraphEdgeKinds) but needs (model.EdgeKindNeeds), the one relation an issue and a plan
// node both wait by (dispatch://INFRA-1253 R10).
func LinkKinds() []string {
	return slices.Clone(active.Load().linkKinds)
}

// HasLinkKinds reports whether the manifest declares any `block[]` attribute (LinkKinds is not
// empty).
func HasLinkKinds() bool {
	return len(active.Load().linkKinds) > 0
}

// SchemaJSON returns a copy of the installed block schema as JSON: what the store's nodes built,
// or the embedded file where nothing has been installed over it.
func SchemaJSON() []byte {
	return append([]byte(nil), active.Load().json...)
}

// IsTypedBlock reports whether name is a typed block the schema declares.
func IsTypedBlock(name string) bool {
	_, ok := typedBlock(name)
	return ok
}

// BlockTypeAttributes is the attributes the installed schema declares for the typed block name,
// in declaration order, and false for a type it does not declare. The block index reads a
// decision's from it to tell a tie its caller writes from one the server derives (refs'
// DecisionFacts): the schema's `server` flag is what says whose a value is.
func BlockTypeAttributes(name string) (BlockAttributes, bool) {
	typ, ok := typedBlock(name)
	return typ.Attributes, ok
}

func typedBlock(name string) (BlockTypeSchema, bool) {
	typ, ok := active.Load().types[name]
	return typ, ok
}

func typedBlockNames() []string {
	types := active.Load().schema.Types
	names := make([]string, 0, len(types))
	for _, typ := range types {
		names = append(names, typ.Name)
	}
	sort.Strings(names)
	return names
}

func validateBlockSchema(value BlockSchema) error {
	if value.Version <= 0 {
		return fmt.Errorf("version must be positive")
	}
	if len(value.Types) == 0 {
		return fmt.Errorf("must declare at least one type")
	}
	seenTypes := make(map[string]struct{}, len(value.Types))
	for _, typ := range value.Types {
		if !directiveName(typ.Name) {
			return fmt.Errorf("type %q is not a directive name", typ.Name)
		}
		if _, duplicate := seenTypes[typ.Name]; duplicate {
			return fmt.Errorf("type %q is declared twice", typ.Name)
		}
		seenTypes[typ.Name] = struct{}{}
		if !validBlockContentRule(typ.Content) {
			return fmt.Errorf("type %q has unsupported content %q", typ.Name, typ.Content)
		}
		if typ.Render != "host" {
			return fmt.Errorf("type %q has unsupported render %q", typ.Name, typ.Render)
		}
		seenAttributes := make(map[string]struct{}, len(typ.Attributes))
		for _, attr := range typ.Attributes {
			name := attr.Name
			if !directiveName(name) {
				return fmt.Errorf("type %q attribute %q is invalid", typ.Name, name)
			}
			if _, duplicate := seenAttributes[name]; duplicate {
				return fmt.Errorf("type %q attribute %q is declared twice", typ.Name, name)
			}
			seenAttributes[name] = struct{}{}
			// A typed block's markdown reads `id="…"` as its block id (TypedBlockIDAttribute), and
			// its tree holds that id as blockId, so neither can name an attribute.
			if name == "id" || name == BlockIDAttr {
				return fmt.Errorf("type %q attribute %q names the block id, which no attribute may", typ.Name, name)
			}
			if err := validateAttributeSchema(typ.Name, name, attr.BlockAttributeSchema); err != nil {
				return err
			}
		}
	}
	// A target names a type of the whole schema, so it is checked once every name is known, and so
	// is a parent.
	for _, typ := range value.Types {
		for _, attr := range typ.Attributes {
			for _, target := range attr.Targets {
				if _, typed := seenTypes[target]; !typed && !builtInBlockNode(value, target) {
					return fmt.Errorf("type %q attribute %q targets %q, which is neither a type the schema declares nor a built-in block the link graph holds", typ.Name, attr.Name, target)
				}
			}
		}
		if err := validateParents(typ, seenTypes); err != nil {
			return err
		}
	}
	return nil
}

// validateParents refuses a type's parents (BlockTypeSchema.Parents) that no block could stand
// in: a name the schema declares no type of, the type itself, and a name given twice.
func validateParents(typ BlockTypeSchema, declared map[string]struct{}) error {
	seen := make(map[string]struct{}, len(typ.Parents))
	for _, parent := range typ.Parents {
		if _, typed := declared[parent]; !typed {
			return fmt.Errorf("type %q names the parent %q, which is no type the schema declares", typ.Name, parent)
		}
		if parent == typ.Name {
			return fmt.Errorf("type %q names itself as its parent", typ.Name)
		}
		if _, repeated := seen[parent]; repeated {
			return fmt.Errorf("type %q names the parent %q twice", typ.Name, parent)
		}
		seen[parent] = struct{}{}
	}
	return nil
}

// builtInBlockNode reports whether name is a built-in block type of value, the manifest being
// validated, whose blocks are nodes of the link graph, which a `block[]` attribute's targets may
// name: a type the renderer records a block offset for (renderer.block: any node but the document
// and inline nodes), so one GET /api/v1/artifacts/{id}/blocks lists, less the types the renderer
// writes without recording one - a list item and a table's rows and cells - which no block list
// holds. It reads value's own nodes rather than the installed manifest's, since the manifest
// being validated is not installed yet (compileManifest).
func builtInBlockNode(value BlockSchema, name string) bool {
	switch name {
	case "doc", "list_item", "table_header_row", "table_row", "table_header", "table_cell":
		return false
	}
	if isInlineNodeType(name) {
		return false
	}
	return slices.ContainsFunc(value.Nodes, func(node BlockSchemaElement) bool { return node.Name == name })
}

func validBlockContentRule(rule BlockContentRule) bool {
	switch rule {
	case BlockContentParagraphs, BlockContentBlocks, BlockContentParagraphsOptionalBulletList:
		return true
	default:
		return false
	}
}

func normalizeSchemaDefaults(value *BlockSchema) error {
	for typeIndex := range value.Types {
		attributes := value.Types[typeIndex].Attributes
		for attrIndex := range attributes {
			attr := &attributes[attrIndex]
			if (attr.Kind != BlockAttributeStringList && attr.Kind != BlockAttributeBlockList) || attr.Default == nil {
				continue
			}
			items, ok := attr.Default.([]any)
			if !ok {
				continue
			}
			normalized := make([]string, len(items))
			for index, item := range items {
				text, ok := item.(string)
				if !ok {
					return fmt.Errorf("type %q attribute %q default contains %T", value.Types[typeIndex].Name, attr.Name, item)
				}
				normalized[index] = text
			}
			attr.Default = normalized
		}
	}
	return nil
}

// linkKindPattern is a `block[]` attribute's name, which is the kind of the edges it makes: what
// every structural edge kind matches, and nothing a `?kind=` comma list would split.
var linkKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ParentAttribute is the name of the attribute that places a typed block in its issue's tree
// where its rendering cannot (dispatch://INFRA-1253/spec R2 and R3): a `block[]` attribute
// holding at most one reference, because a node has one parent, which the block index turns into
// the block's `part_of` tuple. It is the one attribute whose `items` name the conversation's kinds
// (ask, comment, message).
const ParentAttribute = "parent"

func validateAttributeSchema(typeName, name string, attr BlockAttributeSchema) error {
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeBool, BlockAttributeEnum, BlockAttributeStringList,
		BlockAttributeBlockList, BlockAttributeActor, BlockAttributeTimestamp, BlockAttributeAsk, BlockAttributeDays:
	default:
		return fmt.Errorf("type %q attribute %q has unknown kind %q", typeName, name, attr.Kind)
	}
	if attr.Kind == BlockAttributeBlockList {
		if !linkKindPattern.MatchString(name) {
			return fmt.Errorf("type %q attribute %q is a block[] attribute, whose name is the kind of the links it makes and must match %s", typeName, name, linkKindPattern)
		}
		if name != model.EdgeKindNeeds && slices.Contains(model.GraphEdgeKinds, name) {
			return fmt.Errorf("type %q attribute %q is a block[] attribute named after a structural edge kind (%s; %s alone is both)",
				typeName, name, strings.Join(model.GraphEdgeKinds, ", "), model.EdgeKindNeeds)
		}
	} else if attr.Targets != nil {
		return fmt.Errorf("type %q attribute %q has targets, which only a block[] attribute may carry", typeName, name)
	}
	if attr.Kind != BlockAttributeBlockList && attr.Items != nil {
		return fmt.Errorf("type %q attribute %q has items, which only a block[] attribute may carry", typeName, name)
	}
	if name == ParentAttribute && attr.Kind != BlockAttributeBlockList {
		return fmt.Errorf("type %q attribute %q places a block in its tree, so it must be a block[] attribute", typeName, name)
	}
	for index, item := range attr.Items {
		switch item {
		case BlockItemAsk, BlockItemComment, BlockItemMessage, BlockItemIssue:
		default:
			return fmt.Errorf("type %q attribute %q names the item kind %q; items are ask, comment, message and issue", typeName, name, item)
		}
		if slices.Index(attr.Items, item) != index {
			return fmt.Errorf("type %q attribute %q names the item kind %q twice", typeName, name, item)
		}
	}
	if attr.Kind == BlockAttributeEnum {
		if len(attr.Choices) == 0 {
			return fmt.Errorf("type %q attribute %q enum has no choices", typeName, name)
		}
		seen := make(map[string]struct{}, len(attr.Choices))
		for _, choice := range attr.Choices {
			if choice == "" {
				return fmt.Errorf("type %q attribute %q enum has an empty choice", typeName, name)
			}
			if _, duplicate := seen[choice]; duplicate {
				return fmt.Errorf("type %q attribute %q enum repeats %q", typeName, name, choice)
			}
			seen[choice] = struct{}{}
		}
	}
	if attr.Default != nil {
		if err := validateAttributeValue(name, attr, attr.Default); err != nil {
			return fmt.Errorf("type %q attribute %q default: %w", typeName, name, err)
		}
		if items, _ := stringSlice(attr.Default); attr.Kind == BlockAttributeBlockList && len(items) > 0 {
			return fmt.Errorf("type %q attribute %q default must be absent or [], since a block[] default would point every new block at the same blocks", typeName, name)
		}
	}
	return nil
}

// validateAttributeValue refuses value, a tree's value of the attribute name declared as attr,
// unless it is one of attr's kind. ParentAttribute holds at most one reference.
func validateAttributeValue(name string, attr BlockAttributeSchema, value any) error {
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeActor:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("must be a string, got %T", value)
		}
	case BlockAttributeDays:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be a duration in days, got %T", value)
		}
		return validateDays(text)
	case BlockAttributeTimestamp:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be an RFC 3339 timestamp string, got %T", value)
		}
		if _, err := time.Parse(time.RFC3339, text); err != nil {
			return fmt.Errorf("must be an RFC 3339 timestamp: %w", err)
		}
	case BlockAttributeBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("must be a bool, got %T", value)
		}
	case BlockAttributeEnum:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be one of %s, got %T", strings.Join(attr.Choices, ", "), value)
		}
		for _, choice := range attr.Choices {
			if text == choice {
				return nil
			}
		}
		return unknownElement(fmt.Errorf("must be one of %s, got %q", strings.Join(attr.Choices, ", "), text), text)
	case BlockAttributeStringList:
		switch items := value.(type) {
		case []string:
			for _, item := range items {
				if item == "" {
					return fmt.Errorf("must not contain an empty string")
				}
			}
		case []any:
			for _, item := range items {
				text, ok := item.(string)
				if !ok || text == "" {
					return fmt.Errorf("must be a string array")
				}
			}
		default:
			return fmt.Errorf("must be a string array, got %T", value)
		}
	case BlockAttributeBlockList:
		items, ok := stringSlice(value)
		if !ok {
			return fmt.Errorf("must be an array of block references, got %T", value)
		}
		if err := validateBlockPointerValues(items, attr.Items); err != nil {
			return err
		}
		return parentCount(name, items)
	case BlockAttributeAsk:
		reference, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be an ask reference, got %T", value)
		}
		return validateAskReference(reference)
	}
	return nil
}

// validateAskReference refuses the value of an `ask` attribute unless it is one ask of an issue,
// written whole as every writer writes an ask's address (`dispatch://<ISSUE>/ask/<id>`), with
// nothing before or after it. A part of an ask (`dispatch://<ISSUE>/ask/<id>#b-<block>`) is
// refused with a reason of its own: it names a block of the ask's text, not the ask a decision
// settles. Whether the ask is one of the block's own issue only the store can say, so that is no
// rule of the schema's.
func validateAskReference(value string) error {
	ref, ok := text.ParseRef(value, "")
	if ok && ref.Kind == "block" && ref.Item == "ask" {
		return fmt.Errorf("%q is a part of an ask: an ask attribute names the ask itself, dispatch://<ISSUE>/ask/<id>", value)
	}
	if !ok || ref.Kind != "ask" || ref.IssueKey == "" || "dispatch://"+ref.IssueKey+"/ask/"+ref.ID != value {
		return fmt.Errorf("%q is not an ask of an issue as Dispatch writes one: dispatch://<ISSUE>/ask/<id>", value)
	}
	return nil
}

// daysPattern is a duration in days as markdown writes one: a decimal with no sign and no
// exponent, no leading zero but the one before a point, and at most two digits after its point.
var daysPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]{1,2})?$`)

// zeroDays is every spelling daysPattern admits whose value is zero, which no duration is.
var zeroDays = regexp.MustCompile(`^0(?:\.0{1,2})?$`)

// validateDays refuses the value of a `days` attribute unless it is a positive number of days
// written as daysPattern reads one: `0.25`, `1` and `2.5` are durations; `0`, `-1`, `1.234`,
// `01`, `.5`, `1.` and `1e2` are not. The value is kept as its author wrote it, so it reads back
// character for character. Its twin is daysValueProblem in @legion/proof-editor.
func validateDays(value string) error {
	if !daysPattern.MatchString(value) || zeroDays.MatchString(value) {
		return fmt.Errorf("%q is not a duration in days: a positive number with at most two digits after its point, such as 0.25, 1 or 2.5", value)
	}
	return nil
}

// ParseBlockPointerValue reads a `block[]` attribute as markdown writes it: the empty string for
// no pointers, else references joined by exactly one space, with none leading or trailing and
// none repeated, each one a canonical block of a document (text.CanonicalBlockRef) or one of the
// item kinds items declares (validateBlockPointer). Anything else is refused, never normalised,
// so the server and the editor never disagree on a value. Its twin is parseBlockPointerValue in
// @legion/contracts, whose blockPointerValueProblem gives its refusal's reason in this
// function's wording, quoting a value as JSON.stringify does where this uses %q; both are tested
// against `testdata/block-pointer-values.json`.
func ParseBlockPointerValue(raw string, items []BlockItemKind) ([]string, error) {
	if raw == "" {
		return []string{}, nil
	}
	if strings.HasPrefix(raw, " ") || strings.HasSuffix(raw, " ") || strings.Contains(raw, "  ") {
		return nil, fmt.Errorf("%q must be block references separated by exactly one space, with none leading or trailing", raw)
	}
	values := strings.Split(raw, " ")
	if err := validateBlockPointerValues(values, items); err != nil {
		return nil, err
	}
	return values, nil
}

// validateBlockPointerValues refuses a list of block pointers holding one validateBlockPointer
// refuses, and one named twice. Its twin is blockPointersProblem in @legion/contracts, which
// follows its wording, quoting a value as JSON.stringify does.
func validateBlockPointerValues(values []string, items []BlockItemKind) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateBlockPointer(value, items); err != nil {
			return err
		}
		if _, repeated := seen[value]; repeated {
			return fmt.Errorf("names %q twice", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

// validateBlockPointer refuses one reference a `block[]` attribute declaring items holds unless
// it is a block of a document written as text.CanonicalBlockRef reads one, or an item the
// attribute declares: an issue's ask, comment or message, or a block of one, written as
// text.CanonicalItemBlockRef reads it, or an issue written exactly `dispatch://<ISSUE>`. A block
// of an ask, a comment or a message (text.Ref.Item) is refused with a reason of its own, which
// names the kind with the article it takes: of a kind the attribute does not declare, or written
// otherwise than FormatItemBlockRef writes it; so is an ask, a comment or a message of a kind the
// attribute declares, written otherwise. Whether a target is a block, an item or an issue the
// write may name is the store's to say (CheckBlockPointers).
func validateBlockPointer(value string, items []BlockItemKind) error {
	if _, ok := pointerTarget(value, items); ok {
		return nil
	}
	ref, read := text.ParseRef(value, "")
	item := ref.Item
	if ref.Kind != "block" {
		item = ref.Kind
	}
	// The kind takes the article its noun does: an ask, a comment, a message.
	article := "a"
	if item == "ask" {
		article = "an"
	}
	switch {
	case read && ref.Kind == "block" && ref.Item != "" && !slices.Contains(items, BlockItemKind(ref.Item)):
		return fmt.Errorf("%q is a block of %s %s: a block[] attribute points only at a block of a document%s", value, article, ref.Item, declaredItems(items))
	case read && ref.Kind == "block" && ref.Item != "":
		return fmt.Errorf("%q is not a block of %s %s as Dispatch writes one: %s, the item's whole lower-case uuid, the block id percent-encoded in upper-case hex except ASCII letters, digits and -, and no pin", value, article, ref.Item, itemForm(BlockItemKind(ref.Item)))
	case read && (item == "ask" || item == "comment" || item == "message") && slices.Contains(items, BlockItemKind(item)):
		return fmt.Errorf("%q is not %s %s as Dispatch writes one: %s, the item's whole lower-case uuid, and no pin", value, article, item, itemForm(BlockItemKind(item)))
	}
	return fmt.Errorf("%q is not a block reference as Dispatch writes one: dispatch://<ISSUE>/spec#b-<id>, or dispatch://<ISSUE or PROJECT>/artifact/<slug>#b-<id>, the id percent-encoded in upper-case hex except ASCII letters, digits and -, optionally pinned @v<N>%s", value, declaredItems(items))
}

// pointerTarget is what value names when it is a reference a `block[]` attribute declaring items
// holds (validateBlockPointer): a block of a document, an item of a kind items names or a block
// of one, or, where items names issue, an issue. ok is false for anything else.
func pointerTarget(value string, items []BlockItemKind) (text.Ref, bool) {
	if ref, ok := text.CanonicalBlockRef(value); ok {
		return ref, true
	}
	if len(items) == 0 {
		return text.Ref{}, false
	}
	if ref, ok := text.CanonicalItemBlockRef(value); ok && slices.Contains(items, BlockItemKind(ItemKindOf(ref))) {
		return ref, true
	}
	if slices.Contains(items, BlockItemIssue) {
		if ref, ok := text.ParseRef(value, ""); ok && ref.Kind == "issue" && value == "dispatch://"+ref.IssueKey {
			return ref, true
		}
	}
	return text.Ref{}, false
}

// ItemKindOf is the kind of the ask, comment or message ref names, itself or a block of it: ask,
// comment or message, and "" for a reference to anything else.
func ItemKindOf(ref text.Ref) string {
	switch {
	case ref.Kind == "block":
		return ref.Item
	case ref.Kind == "ask" || ref.Kind == "comment" || ref.Kind == "message":
		return ref.Kind
	}
	return ""
}

// itemOf is the item kind of items a reference pointerTarget read names: ask, comment or
// message for one of those or a block of one, issue for an issue, and false for a block of a
// document.
func itemOf(ref text.Ref) (BlockItemKind, bool) {
	if ref.Kind == "issue" {
		return BlockItemIssue, true
	}
	kind := ItemKindOf(ref)
	return BlockItemKind(kind), kind != ""
}

// pointerItem is the item kind of items that value, one reference a `block[]` attribute holds,
// names (itemOf), and false for a block of a document and for a value the attribute does not
// hold. A canonical block of a document names its document by `/spec#` or `/artifact/`, which no
// canonical item reference holds, so such a pointer is passed over unread: a walk of a document's
// elements (eachElement) meets every pointer of a plan.
func pointerItem(value string, items []BlockItemKind) (BlockItemKind, bool) {
	if strings.Contains(value, "/spec#") || strings.Contains(value, "/artifact/") {
		return "", false
	}
	ref, held := pointerTarget(value, items)
	if !held {
		return "", false
	}
	return itemOf(ref)
}

// itemForm is how a reference to an item of kind item is written where a `block[]` attribute
// declares it: an issue's ask, comment or message, or a block of one, and an issue.
func itemForm(item BlockItemKind) string {
	if item == BlockItemIssue {
		return "dispatch://<ISSUE>"
	}
	return "dispatch://<ISSUE>/" + string(item) + "/<id>[#b-<block id>]"
}

// declaredItems is what a refusal adds for an attribute declaring items: the form of each, as
// ", or an item it declares: dispatch://<ISSUE>/comment/<id>#b-<block id>, dispatch://<ISSUE>".
// An attribute declaring none adds nothing.
func declaredItems(items []BlockItemKind) string {
	if len(items) == 0 {
		return ""
	}
	forms := make([]string, len(items))
	for index, item := range items {
		forms[index] = itemForm(item)
	}
	return ", or an item it declares: " + strings.Join(forms, ", ")
}

func parseTypedAttributeValue(name string, attr BlockAttributeSchema, raw any) (any, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("must be encoded as a string, got %T", raw)
	}
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeEnum, BlockAttributeActor, BlockAttributeTimestamp, BlockAttributeAsk, BlockAttributeDays:
		if err := validateAttributeValue(name, attr, text); err != nil {
			return nil, err
		}
		return text, nil
	case BlockAttributeBool:
		value, err := strconv.ParseBool(text)
		if err != nil {
			return nil, fmt.Errorf("must be true or false")
		}
		return value, nil
	case BlockAttributeStringList:
		var value []string
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			legacy, ok := stringListWrittenWithGoEscapes(text)
			if !ok {
				return nil, fmt.Errorf("must be a JSON string array: %w", err)
			}
			value = legacy
		}
		if err := validateAttributeValue(name, attr, value); err != nil {
			return nil, err
		}
		return value, nil
	case BlockAttributeBlockList:
		values, err := ParseBlockPointerValue(text, attr.Items)
		if err != nil {
			return nil, err
		}
		if err := parentCount(name, values); err != nil {
			return nil, err
		}
		return values, nil
	default:
		return nil, fmt.Errorf("has unknown kind %q", attr.Kind)
	}
}

// parentCount refuses values, the references the attribute name holds, when name is
// ParentAttribute and they are more than one: a block has one parent. Its twin is parentProblem in
// @legion/proof-editor.
func parentCount(name string, values []string) error {
	if name == ParentAttribute && len(values) > 1 {
		return fmt.Errorf("holds %d references; a block has one parent, so it holds at most one", len(values))
	}
	return nil
}

// stringListWrittenWithGoEscapes reads a `string[]` attribute a version stored before this reader
// read an attribute as the browser editor reads one: the writer quoted the value with Go's own
// quoting then, so a list holding a quote was written with a backslash before the quote's
// character reference, and the backslash is the value's own text to both parsers now, which no
// JSON array holds. Such a value is read by taking Go's escapes out once, as that writer put them
// in, so the document still opens and its next write stores the list as the browser editor reads
// it. Nothing else is read this way: a value that is no JSON array after that is refused.
func stringListWrittenWithGoEscapes(text string) ([]string, bool) {
	if !strings.Contains(text, `\`) {
		return nil, false
	}
	unquoted, err := strconv.Unquote(`"` + strings.ReplaceAll(text, `"`, `\"`) + `"`)
	if err != nil {
		return nil, false
	}
	var value []string
	if err := json.Unmarshal([]byte(unquoted), &value); err != nil {
		return nil, false
	}
	return value, true
}

// defaultAttributes is the attributes a block of typ holds before it names any of its own: each
// attribute's written default (writtenDefault).
func defaultAttributes(typ BlockTypeSchema) Attrs {
	attrs := make(Attrs, len(typ.Attributes))
	for _, attr := range typ.Attributes {
		if value, ok := writtenDefault(typ, attr); ok {
			attrs[attr.Name] = value
		}
	}
	return attrs
}

// writtenDefault is the value of typ's attribute attr, for a block that does not hold it: its
// default, and false when it has none or when this build may not create the attribute yet. A
// block an earlier build wrote, before the attribute existed, reads with that default (readElement,
// renderTypedAttributes), as the browser editor reads it. An attribute this build cannot create is
// left out, so that no write of this build adds it. A server attribute has no written default
// either: the server writes it into a block or nobody does, and a block it is not written into -
// an ask's `state` once the fleet keeps an answer on the ask's node alone (docs'
// rollWindowAskAttributes) - is read, taken at an agent's boundary (StripServerOwnedAttrs) and
// rendered without it.
func writtenDefault(typ BlockTypeSchema, attr BlockAttribute) (any, bool) {
	if !hasWrittenDefault(typ, attr) {
		return nil, false
	}
	return cloneSchemaValue(attr.Default), true
}

// hasWrittenDefault reports whether typ's attribute attr has a written default (writtenDefault).
func hasWrittenDefault(typ BlockTypeSchema, attr BlockAttribute) bool {
	return attr.Default != nil && !attr.Server && creatable(Element{Kind: AttributeElement, Block: typ.Name, Name: attr.Name})
}

// nullReadsAbsent reports whether value, what a block of typ holds for its attribute attr, is a
// null that reads as the block not holding attr: a null where attr has no written default
// (writtenDefault). The browser editor gives such an attribute no default either - a server
// attribute, which the server writes into a block or nobody does, and one it may not create yet -
// so its node holds it as undefined, which y-prosemirror writes into the live document whenever it
// makes the node, as a paste does, and a read of the live document reads as null (readElement).
// So an ask block a browser pasted holds `state` null, although the attribute's default is
// `open`. A null where attr has a written default is no value of it, and is refused.
func nullReadsAbsent(typ BlockTypeSchema, attr BlockAttribute, value any) bool {
	return value == nil && !hasWrittenDefault(typ, attr)
}

// withWrittenDefaults is attrs, a typed block's attributes as a stored tree holds them, with the
// written default of each attribute of typ that it does not hold. It adds to attrs in place.
func withWrittenDefaults(typ BlockTypeSchema, attrs Attrs) Attrs {
	for _, attr := range typ.Attributes {
		if _, present := attrs[attr.Name]; present {
			continue
		}
		if value, ok := writtenDefault(typ, attr); ok {
			if attrs == nil {
				attrs = make(Attrs, len(typ.Attributes))
			}
			attrs[attr.Name] = value
		}
	}
	return attrs
}

// TypedBlockAttributes is each attribute node carries, in the order its type declares them
// (schema/blocks.json), which is the order its opening line writes them: the values a reader of
// that line reads back, with no escape and no quotes. A block of no type the schema declares
// carries none, and so does one whose attributes the renderer would refuse.
func TypedBlockAttributes(node *Node) []model.BlockAttributeValue {
	typ, ok := typedBlock(node.Type)
	if !ok {
		return nil
	}
	values, err := typedAttributeValues(node, typ)
	if err != nil {
		return nil
	}
	return values
}

// InMarkdown reports whether a typed block's markdown carries the attribute: its `markdown` field,
// true where the manifest names none. One it is false for is never rendered (typedAttributeValues),
// and markdown that names it is read as if it did not (typedDirectiveRefusal), since its value
// lives on the block's node, where the server writes it.
func (a BlockAttributeSchema) InMarkdown() bool {
	return a.Markdown == nil || *a.Markdown
}

// typedAttributeValues is a typed block's declared attributes, their written values and the kind
// its type declares each under, in declaration order, leaving out one the block does not hold and
// one the markdown never carries (InMarkdown).
func typedAttributeValues(n *Node, typ BlockTypeSchema) ([]model.BlockAttributeValue, error) {
	values := make([]model.BlockAttributeValue, 0, len(typ.Attributes))
	for _, attribute := range typ.Attributes {
		name, definition := attribute.Name, attribute.BlockAttributeSchema
		if !definition.InMarkdown() {
			continue
		}
		value, present := n.Attrs[name]
		if !present {
			value, present = writtenDefault(typ, attribute)
		}
		if !present || nullReadsAbsent(typ, attribute, value) {
			continue
		}
		encoded, err := encodeTypedAttribute(n.Type, name, definition, value)
		if err != nil {
			return nil, err
		}
		values = append(values, model.BlockAttributeValue{
			Name: name, Value: encoded, Kind: string(definition.Kind),
		})
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

// renderTypedAttributes writes a typed block's directive attributes in the order its type
// declares them (schema/blocks.json), which is the order every reader of the markdown sees. It
// answers the values it wrote beside the line, so the one caller that wants them both - a
// version write, which lists every typed block's attributes beside the markdown it renders -
// encodes each value once rather than walking the tree again for them.
func renderTypedAttributes(n *Node, typ BlockTypeSchema) (string, []model.BlockAttributeValue, error) {
	id, ok := n.Attrs[BlockIDAttr].(string)
	if !ok || id == "" {
		return "", nil, fmt.Errorf("%w: typed block %q is missing blockId", ErrSchema, n.Type)
	}
	values, err := typedAttributeValues(n, typ)
	if err != nil {
		return "", nil, err
	}
	parts := make([]string, 0, len(values)+1)
	parts = append(parts, TypedBlockIDAttribute(id))
	for _, value := range values {
		parts = append(parts, value.Name+`="`+escapeDirectiveAttribute(value.Value)+`"`)
	}
	return strings.Join(parts, " "), values, nil
}

// encodeTypedAttribute is the text one attribute value is written as, before escaping and
// quoting; it refuses a value whose kind is not the one the type declares.
func encodeTypedAttribute(blockType, name string, definition BlockAttributeSchema, value any) (string, error) {
	switch definition.Kind {
	case BlockAttributeString, BlockAttributeEnum, BlockAttributeActor, BlockAttributeTimestamp, BlockAttributeAsk, BlockAttributeDays:
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%w: typed block %q attribute %q must be a string", ErrSchema, blockType, name)
		}
		return text, nil
	case BlockAttributeBool:
		flag, ok := value.(bool)
		if !ok {
			return "", fmt.Errorf("%w: typed block %q attribute %q must be a bool", ErrSchema, blockType, name)
		}
		return strconv.FormatBool(flag), nil
	case BlockAttributeStringList:
		items, ok := stringSlice(value)
		if !ok {
			return "", fmt.Errorf("%w: typed block %q attribute %q must be a string array", ErrSchema, blockType, name)
		}
		jsonValue, err := json.Marshal(items)
		if err != nil {
			return "", fmt.Errorf("%w: encode typed block %q attribute %q: %v", ErrSchema, blockType, name, err)
		}
		return string(jsonValue), nil
	case BlockAttributeBlockList:
		// Joined by one space, as ParseBlockPointerValue reads them. The joined value still
		// passes through escapeDirectiveAttribute and strconv.Quote in the renderer, which leave
		// a canonical reference unchanged: it holds none of the characters they escape.
		items, ok := stringSlice(value)
		if !ok {
			return "", fmt.Errorf("%w: typed block %q attribute %q must be an array of block references", ErrSchema, blockType, name)
		}
		return strings.Join(items, " "), nil
	default:
		return "", fmt.Errorf("%w: typed block %q attribute %q has unknown kind %q", ErrSchema, blockType, name, definition.Kind)
	}
}

// escapeDirectiveAttribute writes a value's characters as the browser editor's parser reads them
// back: that parser ends a quoted value at the first quote and decodes character references alone
// (micromark-extension-directive), so the characters that would end the value or decode are
// written as references and every other character, a backslash among them, is written as it is. A
// line ending is written as a reference too, since the attributes stand on the block's one
// opening line.
func escapeDirectiveAttribute(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "\"", "&#x22;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, "\n", "&#xA;")
	return strings.ReplaceAll(value, "\r", "&#xD;")
}

func stringSlice(value any) ([]string, bool) {
	switch items := value.(type) {
	case []string:
		return items, true
	case []any:
		out := make([]string, len(items))
		for index, item := range items {
			var ok bool
			out[index], ok = item.(string)
			if !ok {
				return nil, false
			}
		}
		return out, true
	default:
		return nil, false
	}
}

func cloneSchemaValue(value any) any {
	switch current := value.(type) {
	case []string:
		return append([]string(nil), current...)
	default:
		return current
	}
}
