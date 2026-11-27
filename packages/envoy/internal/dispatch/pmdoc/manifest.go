package pmdoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
)

// elementVersions is what the manifest says of one element: from which schema version a build
// reads it (Since) and from which a build may create it (CreatableSince), 1 where the manifest
// says neither (BlockElementVersions, generated from the contract). Two builds serve one database
// during a release, so an element whose writer would serve beside a build that cannot read it is
// added in two steps: one release reads it and does not create it, and a later one creates it,
// once no live build lacks it.
func elementVersions(since, creatableSince int) BlockElementVersions {
	return BlockElementVersions{Since: since, CreatableSince: creatableSince}
}

// ElementKind is what an Element names.
type ElementKind string

const (
	NodeElement       ElementKind = "node"
	MarkElement       ElementKind = "mark"
	TypedBlockElement ElementKind = "typed block"
	AttributeElement  ElementKind = "attribute"
	ChoiceElement     ElementKind = "value"
	// ItemElement is a kind of item a `block[]` attribute's items admit
	// (BlockAttributeSchema.Items), whose versions are the attribute's ItemVersions.
	ItemElement ElementKind = "item"
)

// Element names one element of the manifest. Name is its own name. Block is the typed block of an
// attribute, of a choice or of an item kind, and Attribute is the enum attribute of a choice or the
// `block[]` attribute of an item kind.
type Element struct {
	Kind      ElementKind
	Block     string
	Attribute string
	Name      string
}

// String names e as a refusal names it: `typed block "ask"`, `attribute "urgency" of typed block
// "ask"`, `value "low" of attribute "urgency" of typed block "ask"`, `item "issue" of attribute
// "needs" of typed block "plan_task"`.
func (e Element) String() string {
	switch e.Kind {
	case AttributeElement:
		return fmt.Sprintf("%s %q of typed block %q", e.Kind, e.Name, e.Block)
	case ChoiceElement, ItemElement:
		return fmt.Sprintf("%s %q of attribute %q of typed block %q", e.Kind, e.Name, e.Attribute, e.Block)
	default:
		return fmt.Sprintf("%s %q", e.Kind, e.Name)
	}
}

// Versions is e's since and creatable_since in the installed manifest, each 1 where the manifest
// says none, and false when the manifest does not declare e.
func (e Element) Versions() (BlockElementVersions, bool) {
	versions, declared := active.Load().versions[e]
	return versions, declared
}

// compiledManifest is a manifest decoded, checked and indexed. It is built whole, off to the
// side, and published by storing one pointer (install), so nothing in it is written after it is
// stored.
type compiledManifest struct {
	schema      BlockSchema
	types       map[string]BlockTypeSchema
	nodes       map[string]bool
	marks       map[string]bool
	versions    map[Element]BlockElementVersions
	uncreatable map[Element]bool
	creatable   int
	// typedBlockNames is the name of every typed block the manifest declares, sorted
	// (TypedBlockNames).
	typedBlockNames []string
	// linkKinds is the edge kinds the manifest's `block[]` attributes make, sorted (LinkKinds),
	// and pointerAttributes maps each typed block declaring such an attribute to those
	// attributes' names, sorted.
	linkKinds         []string
	pointerAttributes map[string][]string
	// serverAttributes maps each typed block declaring a server attribute to those attributes'
	// names, in declaration order (ServerAttributes), and markdownServerAttributes maps it to the
	// ones among them its markdown carries (BlockAttributeSchema.InMarkdown,
	// ServerAttributesInMarkdown).
	serverAttributes         map[string][]string
	markdownServerAttributes map[string][]string
	// admitted maps each typed block a type's parents name to the types it admits after its own
	// content (BlockTypeSchema.Parents), and placed holds each type that names parents: such a
	// type is in no node group (isBlockNodeType), so it stands only where a parent admits it.
	admitted map[string]map[string]bool
	placed   map[string]bool
	// maxSince is the highest since among the declared elements: no document's DocumentSince
	// is higher.
	maxSince int
	// json is the manifest as GET /api/v1/schema/blocks serves it (SchemaJSON), held as compiled
	// so a reader takes the whole manifest in one load rather than encoding it again per request.
	json []byte
}

// active holds the installed manifest, the one this process reads and writes by: the embedded
// one, the store's once the server has read it (Install), or another one a test installs
// (UseManifestForTest). A reinstall stores one pointer, so it never writes a map another
// goroutine is reading (the store-driven reinstall, pmdoc/schemanodes). Every reader loads it
// once and takes no lock.
var active atomic.Pointer[compiledManifest]

// embedded is this binary's own manifest, the one blocks.json declares. Its version is the
// highest this build handles (BuiltSchemaVersion), and its nodes and marks are the ones the
// store's manifest is installed with (Install).
var embedded *compiledManifest

func init() {
	compiled, err := compileManifest(blockSchemaJSON)
	if err != nil {
		panic(fmt.Sprintf("pmdoc: embedded block schema: %v", err))
	}
	embedded = compiled
	compiled.install()
}

// install makes m the installed manifest.
func (m *compiledManifest) install() {
	active.Store(m)
}

// Install makes value, read from the node store, the installed manifest. The store holds the
// typed blocks alone: the nodes and marks of the Proof schema are this build's own, the ones its
// parser and renderer know, so value is installed with the embedded manifest's. A value whose
// version is above the embedded one is a store a newer build migrated: it is installed at this
// build's own version, with the elements whose since is at or below that version and nothing
// else (asOfVersion), and the process keeps serving, as every build does while two builds serve
// one database: a document holding an element it does not know answers DOC_NEWER_CONTENT, and a
// write never adds an element it may not create. A value compileManifest refuses installs
// nothing, and the process keeps the manifest it has.
func Install(value BlockSchema) error {
	if built := embedded.schema.Version; value.Version > built {
		value = asOfVersion(value, built)
	}
	value.Nodes, value.Marks = embeddedElements()
	// The value is compiled from its encoding, which is also what SchemaJSON serves, so the
	// manifest compileManifest normalizes in place shares no slice with value's caller.
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode block schema: %w", err)
	}
	compiled, err := compileManifest(encoded)
	if err != nil {
		return fmt.Errorf("block schema: %w", err)
	}
	compiled.install()
	return nil
}

// asOfVersion is value as a build of version reads it: at that version, holding each typed
// block, attribute and enum choice whose since is at or below it, and no target of a `block[]`
// attribute that names a typed block it leaves out. A since the store leaves out is 1, as
// normalizeVersions reads it. An element it keeps keeps its creatable_since, so one this build
// reads but may not create yet stays one it does not create. It writes nothing of value's own:
// every slice and map it changes is a new one.
//
// Two of the store's own values cannot stand once the elements they name are gone, and each is
// answered here rather than left to the validation that would refuse the whole manifest and
// fail the boot with it (Install, cmd/dispatch's main). An enum default the store moved to a
// choice added later reads as this build's own default for that attribute
// (enumDefaultThisBuildReads). A `block[]` attribute every target of which names a typed block
// this build leaves out goes altogether: an empty target set means "any block"
// (CheckBlockPointers), so keeping it would let an older build accept at any block the pointers
// a newer one holds to a type it does not know. An enum attribute every choice of which is newer
// than this build goes the same way: an enum with no choice is one validateAttributeSchema
// refuses, which would fail the boot, and no value of it is one this build could read. So does a
// type every parent of which this build leaves out (BlockTypeSchema.Parents): a type naming no
// parent stands wherever a block does, which a newer build never let it.
func asOfVersion(value BlockSchema, version int) BlockSchema {
	reads := func(since int) bool { return max(since, 1) <= version }
	dropped := make(map[string]bool)
	types := make([]BlockTypeSchema, 0, len(value.Types))
	for _, typ := range value.Types {
		if !reads(typ.Since) {
			dropped[typ.Name] = true
			continue
		}
		types = append(types, typ)
	}
	// A type left with no parent drops out, which may leave another type naming it with none.
	isDropped := func(name string) bool { return dropped[name] }
	for changed := true; changed; {
		changed = false
		for index := range types {
			typ := &types[index]
			if !slices.ContainsFunc(typ.Parents, isDropped) {
				continue
			}
			typ.Parents = slices.DeleteFunc(slices.Clone(typ.Parents), isDropped)
			if len(typ.Parents) == 0 {
				dropped[typ.Name], changed = true, true
			}
		}
		types = slices.DeleteFunc(types, func(typ BlockTypeSchema) bool { return dropped[typ.Name] })
	}
	for index := range types {
		typ := &types[index]
		attributes := make(BlockAttributes, 0, len(typ.Attributes))
		for _, attribute := range typ.Attributes {
			attr := attribute.BlockAttributeSchema
			if !reads(attr.Since) {
				continue
			}
			if len(attr.Choices) > 0 {
				choices := make([]string, 0, len(attr.Choices))
				var versions map[string]BlockElementVersions
				for _, choice := range attr.Choices {
					held, given := attr.ChoiceVersions[choice]
					if !reads(held.Since) {
						continue
					}
					choices = append(choices, choice)
					if given {
						if versions == nil {
							versions = make(map[string]BlockElementVersions, len(attr.ChoiceVersions))
						}
						versions[choice] = held
					}
				}
				attr.Choices, attr.ChoiceVersions = choices, versions
				if len(choices) == 0 {
					continue
				}
			}
			if attr.Kind == BlockAttributeEnum {
				attr.Default = enumDefaultThisBuildReads(typ.Name, attribute.Name, attr)
			}
			// An item kind read from a later version goes as a choice does; the attribute keeps the
			// blocks of documents it points at whatever items it is left with.
			if len(attr.ItemVersions) > 0 {
				attr.Items = slices.DeleteFunc(slices.Clone(attr.Items), func(item BlockItemKind) bool {
					return !reads(attr.ItemVersions[string(item)].Since)
				})
				versions := make(map[string]BlockElementVersions, len(attr.ItemVersions))
				for _, item := range attr.Items {
					if held, given := attr.ItemVersions[string(item)]; given {
						versions[string(item)] = held
					}
				}
				attr.ItemVersions = versions
				if len(attr.Items) == 0 {
					attr.Items, attr.ItemVersions = nil, nil
				}
			}
			if len(attr.Targets) > 0 {
				attr.Targets = slices.DeleteFunc(slices.Clone(attr.Targets), func(target string) bool { return dropped[target] })
				if len(attr.Targets) == 0 {
					continue
				}
			}
			attributes = append(attributes, BlockAttribute{Name: attribute.Name, BlockAttributeSchema: attr})
		}
		typ.Attributes = attributes
	}
	value.Types = types
	value.Version = version
	return value
}

// enumDefaultThisBuildReads is the default of the enum attribute named attribute of typed block
// typeName, as a build reading only the choices attr still holds takes it: the store's own
// default while that is one of them, and this build's own default for that attribute otherwise,
// which is a choice this build was compiled against. An attribute this build does not declare,
// and one whose own default is no choice the store still holds, keeps none: an attribute with
// no default is left out of a block that does not hold it (writtenDefault), which is what a
// build that cannot name a value has to do.
func enumDefaultThisBuildReads(typeName, attribute string, attr BlockAttributeSchema) any {
	held, named := attr.Default.(string)
	if !named || slices.Contains(attr.Choices, held) {
		return attr.Default
	}
	own, declared := embedded.types[typeName].Attributes.Lookup(attribute)
	if !declared {
		return nil
	}
	if text, names := own.Default.(string); names && slices.Contains(attr.Choices, text) {
		return own.Default
	}
	return nil
}

// embeddedElements is the nodes and marks the embedded manifest declares, as blocks.json writes
// them: compileManifest writes 1 for every version an element leaves out, which the served
// manifest leaves out as the file does.
func embeddedElements() (nodes, marks []BlockSchemaElement) {
	var declared BlockSchema
	if err := json.Unmarshal(embedded.json, &declared); err != nil {
		panic(fmt.Sprintf("pmdoc: decode embedded block schema: %v", err))
	}
	return declared.Nodes, declared.Marks
}

// BuiltSchemaVersion is the highest schema version this binary handles: the version of the file
// it embeds, and the version it installs a newer store at (Install).
func BuiltSchemaVersion() int {
	return embedded.schema.Version
}

// compileManifest compiles raw, a manifest in blocks.json's format, which it also keeps as the
// bytes SchemaJSON serves.
func compileManifest(raw []byte) (*compiledManifest, error) {
	var value BlockSchema
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := normalizeSchemaDefaults(&value); err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}
	normalizeVersions(&value)
	if err := validateBlockSchema(value); err != nil {
		return nil, fmt.Errorf("invalid: %w", err)
	}
	compiled := &compiledManifest{
		schema:                   value,
		types:                    make(map[string]BlockTypeSchema, len(value.Types)),
		typedBlockNames:          make([]string, 0, len(value.Types)),
		nodes:                    make(map[string]bool, len(value.Nodes)),
		marks:                    make(map[string]bool, len(value.Marks)),
		versions:                 make(map[Element]BlockElementVersions),
		uncreatable:              make(map[Element]bool),
		pointerAttributes:        map[string][]string{},
		serverAttributes:         map[string][]string{},
		markdownServerAttributes: map[string][]string{},
		admitted:                 map[string]map[string]bool{},
		placed:                   map[string]bool{},
		json:                     raw,
	}
	linkKinds := map[string]struct{}{}
	declare := func(element Element, versions BlockElementVersions) error {
		if _, duplicate := compiled.versions[element]; duplicate {
			return fmt.Errorf("invalid: %s is declared twice", element)
		}
		if err := validateElementVersions(element, versions, value.Version); err != nil {
			return fmt.Errorf("invalid: %w", err)
		}
		compiled.versions[element] = versions
		compiled.maxSince = max(compiled.maxSince, versions.Since)
		if versions.CreatableSince > value.Version {
			compiled.uncreatable[element] = true
		} else {
			compiled.creatable = max(compiled.creatable, versions.Since)
		}
		return nil
	}
	for _, node := range value.Nodes {
		if node.Name == "" {
			return nil, fmt.Errorf("invalid: a node has no name")
		}
		if err := declare(Element{Kind: NodeElement, Name: node.Name}, elementVersions(node.Since, node.CreatableSince)); err != nil {
			return nil, err
		}
		compiled.nodes[node.Name] = true
	}
	for _, mark := range value.Marks {
		if mark.Name == "" {
			return nil, fmt.Errorf("invalid: a mark has no name")
		}
		if err := declare(Element{Kind: MarkElement, Name: mark.Name}, elementVersions(mark.Since, mark.CreatableSince)); err != nil {
			return nil, err
		}
		compiled.marks[mark.Name] = true
	}
	for _, typ := range value.Types {
		if compiled.nodes[typ.Name] {
			return nil, fmt.Errorf("invalid: type %q is also a node", typ.Name)
		}
		if err := declare(Element{Kind: TypedBlockElement, Name: typ.Name}, elementVersions(typ.Since, typ.CreatableSince)); err != nil {
			return nil, err
		}
		compiled.types[typ.Name] = typ
		compiled.typedBlockNames = append(compiled.typedBlockNames, typ.Name)
		for _, parent := range typ.Parents {
			if compiled.admitted[parent] == nil {
				compiled.admitted[parent] = map[string]bool{}
			}
			compiled.admitted[parent][typ.Name] = true
			compiled.placed[typ.Name] = true
		}
		var pointers, server, markdownServer []string
		for _, attribute := range typ.Attributes {
			name, attr := attribute.Name, attribute.BlockAttributeSchema
			if attr.Kind == BlockAttributeBlockList {
				linkKinds[name] = struct{}{}
				pointers = append(pointers, name)
			}
			if attr.Server {
				server = append(server, name)
				if attr.InMarkdown() {
					markdownServer = append(markdownServer, name)
				}
			}
			if err := declare(Element{Kind: AttributeElement, Block: typ.Name, Name: name}, elementVersions(attr.Since, attr.CreatableSince)); err != nil {
				return nil, err
			}
			for choice := range attr.ChoiceVersions {
				if !slices.Contains(attr.Choices, choice) {
					return nil, fmt.Errorf("invalid: type %q attribute %q gives versions to %q, which is not one of its choices", typ.Name, name, choice)
				}
			}
			for _, choice := range attr.Choices {
				if err := declare(Element{Kind: ChoiceElement, Block: typ.Name, Attribute: name, Name: choice}, attr.ChoiceVersions[choice]); err != nil {
					return nil, err
				}
			}
			for item := range attr.ItemVersions {
				if !slices.Contains(attr.Items, BlockItemKind(item)) {
					return nil, fmt.Errorf("invalid: type %q attribute %q gives versions to the item %q, which is not one of its items", typ.Name, name, item)
				}
			}
			for _, item := range attr.Items {
				if err := declare(Element{Kind: ItemElement, Block: typ.Name, Attribute: name, Name: string(item)}, attr.ItemVersions[string(item)]); err != nil {
					return nil, err
				}
			}
			// A write gives a new block an attribute's default (defaultAttributes), so a default
			// value a build may not create would be created with every block that takes it.
			if fallback, ok := attr.Default.(string); ok && attr.Kind == BlockAttributeEnum && attr.ChoiceVersions[fallback].CreatableSince > attr.CreatableSince {
				return nil, fmt.Errorf("invalid: type %q attribute %q default %q is creatable later than the attribute", typ.Name, name, fallback)
			}
		}
		if len(pointers) > 0 {
			slices.Sort(pointers)
			compiled.pointerAttributes[typ.Name] = pointers
		}
		if len(server) > 0 {
			compiled.serverAttributes[typ.Name] = server
		}
		if len(markdownServer) > 0 {
			compiled.markdownServerAttributes[typ.Name] = markdownServer
		}
	}
	compiled.linkKinds = slices.Sorted(maps.Keys(linkKinds))
	slices.Sort(compiled.typedBlockNames)
	if len(compiled.nodes) == 0 {
		return nil, fmt.Errorf("invalid: must declare its nodes")
	}
	if len(compiled.marks) == 0 {
		return nil, fmt.Errorf("invalid: must declare its marks")
	}
	return compiled, nil
}

// normalizeVersions writes 1 for every since and creatable_since the manifest leaves out.
func normalizeVersions(value *BlockSchema) {
	one := func(since, creatableSince *int) {
		if *since == 0 {
			*since = 1
		}
		if *creatableSince == 0 {
			*creatableSince = 1
		}
	}
	for index := range value.Nodes {
		one(&value.Nodes[index].Since, &value.Nodes[index].CreatableSince)
	}
	for index := range value.Marks {
		one(&value.Marks[index].Since, &value.Marks[index].CreatableSince)
	}
	for index := range value.Types {
		typ := &value.Types[index]
		one(&typ.Since, &typ.CreatableSince)
		for attrIndex := range typ.Attributes {
			attr := &typ.Attributes[attrIndex]
			one(&attr.Since, &attr.CreatableSince)
			if len(attr.Items) > 0 {
				items := make(map[string]BlockElementVersions, len(attr.Items))
				for _, item := range attr.Items {
					versions := attr.ItemVersions[string(item)]
					one(&versions.Since, &versions.CreatableSince)
					items[string(item)] = versions
				}
				for item, versions := range attr.ItemVersions {
					if _, listed := items[item]; !listed {
						items[item] = versions
					}
				}
				attr.ItemVersions = items
			}
			if len(attr.Choices) == 0 {
				continue
			}
			choices := make(map[string]BlockElementVersions, len(attr.Choices))
			for _, choice := range attr.Choices {
				versions := attr.ChoiceVersions[choice]
				one(&versions.Since, &versions.CreatableSince)
				choices[choice] = versions
			}
			for choice, versions := range attr.ChoiceVersions {
				if _, listed := choices[choice]; !listed {
					choices[choice] = versions
				}
			}
			attr.ChoiceVersions = choices
		}
	}
}

// validateElementVersions refuses numbers no build can act on: an element read before version 1,
// read only from a version after the manifest's own, or created before it is read.
func validateElementVersions(element Element, versions BlockElementVersions, version int) error {
	switch {
	case versions.Since < 1 || versions.CreatableSince < 1:
		return fmt.Errorf("%s has since %d and creatable_since %d; both must be 1 or more", element, versions.Since, versions.CreatableSince)
	case versions.Since > version:
		return fmt.Errorf("%s has since %d, after the manifest's version %d", element, versions.Since, version)
	case versions.CreatableSince < versions.Since:
		return fmt.Errorf("%s has creatable_since %d, before its since %d", element, versions.CreatableSince, versions.Since)
	}
	return nil
}

// SchemaVersion returns V, the installed manifest's schema version: the version a client presents
// during document admission. This build reads every element whose since is V or less, and
// creates every element whose creatable_since is V or less.
func SchemaVersion() int {
	return active.Load().schema.Version
}

// SchemaCreatable returns R, the highest since among the elements this build may create: a build
// whose SchemaVersion is R or more reads everything this build writes.
func SchemaCreatable() int {
	return active.Load().creatable
}

// creatable reports whether this build may create element: false only for an element its manifest
// declares with a creatable_since above SchemaVersion. It reports true for an element the manifest
// does not declare; its one caller, writtenDefault, asks it only of a declared attribute.
func creatable(element Element) bool {
	return !active.Load().uncreatable[element]
}

// DocumentSince returns D, the highest since among the declared elements doc holds: a client
// whose schema version is D or more can represent all of doc. An element this build's manifest
// does not declare counts for nothing, and a nil doc is 0. The walk stops at the first element
// whose since is the manifest's highest (maxSince), since no later one can raise D.
func DocumentSince(doc *Node) int {
	if doc == nil {
		return 0
	}
	manifest := active.Load()
	highest := 0
	manifest.eachElement([]*Node{doc}, func(element Element) bool {
		if versions, declared := manifest.versions[element]; declared {
			highest = max(highest, versions.Since)
		}
		return highest < manifest.maxSince
	})
	return highest
}

// eachElement calls yield with each element of m that nodes hold, in document order, until yield
// returns false: each node, mark and typed block, each attribute a typed block holds a value for,
// each enum attribute's value, and the kind of each item a `block[]` attribute's pointers name. It
// reports whether yield never returned false.
func (m *compiledManifest) eachElement(nodes []*Node, yield func(Element) bool) bool {
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if typ, typed := m.types[node.Type]; typed {
			if !yield(Element{Kind: TypedBlockElement, Name: node.Type}) {
				return false
			}
			for name, value := range node.Attrs {
				if name == BlockIDAttr || value == nil {
					continue
				}
				if !yield(Element{Kind: AttributeElement, Block: node.Type, Name: name}) {
					return false
				}
				declared := typ.Attributes.Get(name)
				if choice, ok := value.(string); ok && declared.Kind == BlockAttributeEnum {
					if !yield(Element{Kind: ChoiceElement, Block: node.Type, Attribute: name, Name: choice}) {
						return false
					}
				}
				if declared.Kind != BlockAttributeBlockList || len(declared.Items) == 0 {
					continue
				}
				pointers, _ := stringSlice(value)
				for _, pointer := range pointers {
					if item, named := pointerItem(pointer, declared.Items); named &&
						!yield(Element{Kind: ItemElement, Block: node.Type, Attribute: name, Name: string(item)}) {
						return false
					}
				}
			}
		} else if !yield(Element{Kind: NodeElement, Name: node.Type}) {
			return false
		}
		for _, mark := range node.Marks {
			if !yield(Element{Kind: MarkElement, Name: mark.Type}) {
				return false
			}
		}
		if !m.eachElement(node.Children, yield) {
			return false
		}
	}
	return true
}

// unknownBeside is err, Validate's refusal of doc, a tree read from a document. Where err refuses a
// known element's shape and doc also holds an element this build does not know, it is the refusal
// of the first such element in document order instead (ErrUnknownElement): a repair of the shape
// replaces the document whole, and would delete what a newer build wrote.
func unknownBeside(doc *Node, err error) error {
	if errors.Is(err, ErrUnknownElement) {
		return err
	}
	manifest := active.Load()
	var unknown Element
	if manifest.eachElement([]*Node{doc}, func(element Element) bool {
		_, declared := manifest.versions[element]
		unknown = element
		return declared
	}) {
		return err
	}
	return unknownElement(fmt.Errorf("%w: %s, in a tree also refused as %v", ErrSchema, unknown, err), unknown.Name)
}

// refuseUncreatable is the refusal of a write that makes nodes holding an element this build reads
// but may not create yet (creatable_since above SchemaVersion), naming the first one in document
// order, or nil. The validation of every tree a write produces runs it: markdown parsed for a
// write, Update, and the transforms that add content (Splice, InsertTableRows, RetypeBlock,
// SetBlockAttributes, SetBlockBody). A transform that only deletes or moves blocks adds no
// element, and reading a stored tree or stored markdown never refuses one. held is the document
// the write starts from, nil for a write that starts from nothing: an element held already holds
// is not created by the write, which keeps what a newer build wrote and edits beside it. held is
// called only when nodes hold such an element.
func refuseUncreatable(nodes []*Node, held func() *Node) error {
	manifest := active.Load()
	if len(manifest.uncreatable) == 0 {
		return nil
	}
	var made []Element
	seen := make(map[Element]bool)
	manifest.eachElement(nodes, func(element Element) bool {
		if manifest.uncreatable[element] && !seen[element] {
			seen[element] = true
			made = append(made, element)
		}
		return true
	})
	if len(made) == 0 {
		return nil
	}
	present := make(map[Element]bool, len(made))
	if held != nil {
		if source := held(); source != nil {
			manifest.eachElement([]*Node{source}, func(element Element) bool {
				if seen[element] {
					present[element] = true
				}
				return true
			})
		}
	}
	for _, element := range made {
		if !present[element] {
			return fmt.Errorf("%w: %s", ErrSchema, notCreatableYet(element))
		}
	}
	return nil
}

// notCreatableYet is the refusal of a write that creates element before it may: refuseUncreatable's,
// and CheckBlockPointers' for a pointer at an item of a kind not every process serving the database
// reads yet.
func notCreatableYet(element Element) string {
	return fmt.Sprintf("this Dispatch build reads %s but cannot write it yet. Remove it from your markdown, or write it again after the next release", element)
}
