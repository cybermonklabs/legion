package pmdoc

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

//go:embed schema/blocks.json
var blockSchemaJSON []byte

// The schema's shape - BlockSchema, BlockTypeSchema, BlockAttributeSchema, BlockContentRule and
// the attribute kinds - is declared in packages/contracts/schemas/block-schema.schema.json and
// generated into schema_generated.go, which blocks.json above is read into.

// installedSchema is the whole of what this package knows about the block schema: the value
// itself, the lookups built from it, and the bytes an editor is served. It is built once, off to
// the side, and published by storing one pointer, so a reinstall never writes a map another
// goroutine is reading (the store-driven reinstall, pmdoc/schemanodes). Nothing here is written
// after the pointer is stored.
type installedSchema struct {
	schema    BlockSchema
	types     map[string]BlockTypeSchema
	linkKinds []string
	// pointerAttributes maps each typed block declaring a `block[]` attribute to those
	// attributes' names, sorted.
	pointerAttributes map[string][]string
	// json is the value as GET /api/v1/schema/blocks serves it, encoded at install so a reader
	// takes the whole schema in one load rather than encoding it again per request.
	json []byte
}

// installed holds the one installed schema. Every reader loads it once and takes no lock.
var installed atomic.Pointer[installedSchema]

// builtSchemaVersion is the highest schema version this binary was built for: the version of the
// embedded file. A store schema above it names types this build cannot address, so it is not
// installed and the process reports itself not ready (NotReadyReason).
var builtSchemaVersion int

// schemaAhead holds the store schema version that is above builtSchemaVersion, or 0 while the
// installed value is this build's own. It is what makes the process not ready.
var schemaAhead atomic.Int64

func init() {
	var value BlockSchema
	if err := json.Unmarshal(blockSchemaJSON, &value); err != nil {
		panic(fmt.Sprintf("pmdoc: decode embedded block schema: %v", err))
	}
	builtSchemaVersion = value.Version
	if err := installSchema(value); err != nil {
		panic(fmt.Sprintf("pmdoc: install embedded block schema: %v", err))
	}
}

// installSchema makes value the one schema every reader of this package uses. It applies the two
// checks init applies to the embedded file - normalizeSchemaDefaults, then validateBlockSchema -
// and installs nothing when either refuses: the caller keeps the schema it has.
func installSchema(value BlockSchema) error {
	if err := normalizeSchemaDefaults(&value); err != nil {
		return fmt.Errorf("normalize block schema: %w", err)
	}
	if err := validateBlockSchema(value); err != nil {
		return fmt.Errorf("invalid block schema: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode block schema: %w", err)
	}
	next := &installedSchema{
		schema:            value,
		types:             make(map[string]BlockTypeSchema, len(value.Types)),
		pointerAttributes: map[string][]string{},
		json:              encoded,
	}
	kinds := map[string]struct{}{}
	for _, typ := range value.Types {
		next.types[typ.Name] = typ
		var names []string
		for _, attr := range typ.Attributes {
			if attr.Kind == BlockAttributeBlockList {
				kinds[attr.Name] = struct{}{}
				names = append(names, attr.Name)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			next.pointerAttributes[typ.Name] = names
		}
	}
	next.linkKinds = slices.Sorted(maps.Keys(kinds))
	installed.Store(next)
	return nil
}

// Install makes value, read from the node store, the installed schema. A value whose version is
// above the one this binary was built for installs nothing and makes the process not ready
// (NotReadyReason): the build cannot address the types that version declares, and the deployment
// replaces the process with one that can. A value that fails either check of installSchema
// installs nothing either, and leaves the process ready on the schema it has.
func Install(value BlockSchema) error {
	if value.Version > builtSchemaVersion {
		schemaAhead.Store(int64(value.Version))
		return fmt.Errorf("block schema version %d is above the highest version this build was made for (%d)",
			value.Version, builtSchemaVersion)
	}
	if err := installSchema(value); err != nil {
		return err
	}
	schemaAhead.Store(0)
	return nil
}

// BuiltSchemaVersion is the highest schema version this binary handles: the version of the file
// it embeds.
func BuiltSchemaVersion() int {
	return builtSchemaVersion
}

// NotReadyReason is why this process cannot serve, or "" while it can: the one reason is a store
// schema above the version this build handles. GET /healthz answers 503 with it, and the switch
// that closes the open document sockets reads the same reason.
func NotReadyReason() string {
	ahead := schemaAhead.Load()
	if ahead == 0 {
		return ""
	}
	return fmt.Sprintf("the store's block schema version %d is above the highest version this build was made for (%d)",
		ahead, builtSchemaVersion)
}

// LinkKinds is the edge kinds the schema's `block[]` attributes make, sorted: each such
// attribute's name, once however many types declare it. None is a structural edge kind
// (model.GraphEdgeKinds).
func LinkKinds() []string {
	return slices.Clone(installed.Load().linkKinds)
}

// HasLinkKinds reports whether the schema declares any `block[]` attribute (LinkKinds is not
// empty).
func HasLinkKinds() bool {
	return len(installed.Load().linkKinds) > 0
}

// SchemaJSON returns a copy of the installed block schema as JSON: what the store's nodes built,
// or the embedded file where nothing has been installed over it.
func SchemaJSON() []byte {
	return append([]byte(nil), installed.Load().json...)
}

// SchemaVersion returns the current schema version clients present during document admission.
func SchemaVersion() int {
	return installed.Load().schema.Version
}

// IsTypedBlock reports whether name is a typed block the schema declares.
func IsTypedBlock(name string) bool {
	_, ok := typedBlock(name)
	return ok
}

func typedBlock(name string) (BlockTypeSchema, bool) {
	typ, ok := installed.Load().types[name]
	return typ, ok
}

func typedBlockNames() []string {
	types := installed.Load().schema.Types
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
	// A target names a type of the whole schema, so it is checked once every name is known.
	for _, typ := range value.Types {
		for _, attr := range typ.Attributes {
			for _, target := range attr.Targets {
				if _, typed := seenTypes[target]; !typed && !builtInBlockNode(target) {
					return fmt.Errorf("type %q attribute %q targets %q, which is neither a type the schema declares nor a built-in block the link graph holds", typ.Name, attr.Name, target)
				}
			}
		}
	}
	return nil
}

// builtInBlockNode reports whether name is a built-in block type whose blocks are nodes of the
// link graph, which a `block[]` attribute's targets may name: a type the renderer records a block
// offset for (renderer.block: any node but the document and inline nodes), so one
// GET /api/v1/artifacts/{id}/blocks lists, less the types the renderer writes without recording
// one - a list item and a table's rows and cells - which no block list holds.
func builtInBlockNode(name string) bool {
	switch name {
	case "doc", "list_item", "table_header_row", "table_row", "table_header", "table_cell":
		return false
	}
	return nodeTypes[name] && !isInlineNodeType(name)
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

func validateAttributeSchema(typeName, name string, attr BlockAttributeSchema) error {
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeBool, BlockAttributeEnum, BlockAttributeStringList,
		BlockAttributeBlockList, BlockAttributeActor, BlockAttributeTimestamp:
	default:
		return fmt.Errorf("type %q attribute %q has unknown kind %q", typeName, name, attr.Kind)
	}
	if attr.Kind == BlockAttributeBlockList {
		if !linkKindPattern.MatchString(name) {
			return fmt.Errorf("type %q attribute %q is a block[] attribute, whose name is the kind of the links it makes and must match %s", typeName, name, linkKindPattern)
		}
		if slices.Contains(model.GraphEdgeKinds, name) {
			return fmt.Errorf("type %q attribute %q is a block[] attribute named after a structural edge kind (%s)", typeName, name, strings.Join(model.GraphEdgeKinds, ", "))
		}
	} else if attr.Targets != nil {
		return fmt.Errorf("type %q attribute %q has targets, which only a block[] attribute may carry", typeName, name)
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
		if err := validateAttributeValue(attr, attr.Default); err != nil {
			return fmt.Errorf("type %q attribute %q default: %w", typeName, name, err)
		}
		if items, _ := stringSlice(attr.Default); attr.Kind == BlockAttributeBlockList && len(items) > 0 {
			return fmt.Errorf("type %q attribute %q default must be absent or [], since a block[] default would point every new block at the same blocks", typeName, name)
		}
	}
	return nil
}

func validateAttributeValue(attr BlockAttributeSchema, value any) error {
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeActor:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("must be a string, got %T", value)
		}
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
		return fmt.Errorf("must be one of %s, got %q", strings.Join(attr.Choices, ", "), text)
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
		return validateBlockPointerValues(items)
	}
	return nil
}

// ParseBlockPointerValue reads a `block[]` attribute as markdown writes it: the empty string for
// no pointers, else canonical block references (text.CanonicalBlockRef) joined by exactly one
// space, with none leading or trailing and none repeated. Anything else is refused, never
// normalised, so the server and the editor never disagree on a value. Its twin is
// parseBlockPointerValue in @legion/contracts, whose blockPointerValueProblem gives its refusal's
// reason in this function's wording, quoting a value as JSON.stringify does where this uses %q;
// both are tested against `testdata/block-pointer-values.json`.
func ParseBlockPointerValue(raw string) ([]string, error) {
	if raw == "" {
		return []string{}, nil
	}
	if strings.HasPrefix(raw, " ") || strings.HasSuffix(raw, " ") || strings.Contains(raw, "  ") {
		return nil, fmt.Errorf("%q must be block references separated by exactly one space, with none leading or trailing", raw)
	}
	values := strings.Split(raw, " ")
	if err := validateBlockPointerValues(values); err != nil {
		return nil, err
	}
	return values, nil
}

// validateBlockPointerValues refuses a list of block pointers holding a part of an ask or a block
// of a comment or a message (text.Ref.Item), which a pointer never names: a `block[]` attribute
// points at blocks of documents, whose types its targets limit, and an item's block has no type.
// It refuses one that is otherwise not a canonical block reference (text.CanonicalBlockRef), and
// one named twice. Its twin is blockPointersProblem in @legion/contracts, which follows its
// wording, quoting a value as JSON.stringify does.
func validateBlockPointerValues(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if ref, ok := text.ParseRef(value, ""); ok && ref.Kind == "block" && ref.Item != "" {
			// The kind takes the article its noun does: an ask, a comment, a message.
			article := "a"
			if ref.Item == "ask" {
				article = "an"
			}
			return fmt.Errorf("%q is a block of %s %s: a block[] attribute points only at a block of a document", value, article, ref.Item)
		}
		if _, ok := text.CanonicalBlockRef(value); !ok {
			return fmt.Errorf("%q is not a block reference as Dispatch writes one: dispatch://<ISSUE>/spec#b-<id>, or dispatch://<ISSUE or PROJECT>/artifact/<slug>#b-<id>, the id percent-encoded in upper-case hex except ASCII letters, digits and -, optionally pinned @v<N>", value)
		}
		if _, repeated := seen[value]; repeated {
			return fmt.Errorf("names %q twice", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func parseTypedAttributeValue(attr BlockAttributeSchema, raw any) (any, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("must be encoded as a string, got %T", raw)
	}
	switch attr.Kind {
	case BlockAttributeString, BlockAttributeEnum, BlockAttributeActor, BlockAttributeTimestamp:
		if err := validateAttributeValue(attr, text); err != nil {
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
		if err := validateAttributeValue(attr, value); err != nil {
			return nil, err
		}
		return value, nil
	case BlockAttributeBlockList:
		return ParseBlockPointerValue(text)
	default:
		return nil, fmt.Errorf("has unknown kind %q", attr.Kind)
	}
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

func defaultAttributes(typ BlockTypeSchema) Attrs {
	attrs := make(Attrs, len(typ.Attributes))
	for _, attr := range typ.Attributes {
		if attr.Default != nil {
			attrs[attr.Name] = cloneSchemaValue(attr.Default)
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

// typedAttributeValues is a typed block's declared attributes, their written values and the kind
// its type declares each under, in declaration order, leaving out one the block does not hold and
// one the markdown never carries.
func typedAttributeValues(n *Node, typ BlockTypeSchema) ([]model.BlockAttributeValue, error) {
	values := make([]model.BlockAttributeValue, 0, len(typ.Attributes))
	for _, attribute := range typ.Attributes {
		name, definition := attribute.Name, attribute.BlockAttributeSchema
		if name == "invalid" && definition.Server {
			continue
		}
		value, present := n.Attrs[name]
		if !present || (value == nil && definition.Default == nil) {
			if definition.Default == nil {
				continue
			}
			return nil, fmt.Errorf("%w: typed block %q is missing attribute %q", ErrSchema, n.Type, name)
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
	case BlockAttributeString, BlockAttributeEnum, BlockAttributeActor, BlockAttributeTimestamp:
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
