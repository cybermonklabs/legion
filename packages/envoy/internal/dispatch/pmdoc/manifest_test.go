package pmdoc

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A typed block that an earlier build wrote before one of its attributes existed does not hold
// that attribute. It reads with the attribute's default, as the browser editor reads it, and the
// rendering writes that default. A tree built without the attribute validates and renders the same.
func TestATypedBlockMissingADefaultedAttributeReadsAsItsDefault(t *testing.T) {
	t.Parallel()
	const want = ":::callout{#callout-1 kind=\"note\" title=\"\"}\nBody.\n:::\n"
	tree, err := Parse(":::callout{#callout-1 kind=\"warning\" title=\"Read this\"}\nBody.\n:::\n")
	if err != nil {
		t.Fatalf("parse callout: %v", err)
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	if err := doc.TransactE(func(txn *crdt.Transaction) error {
		return Update(txn, fragment, tree)
	}); err != nil {
		t.Fatalf("write callout to Yjs: %v", err)
	}
	if err := doc.TransactE(func(txn *crdt.Transaction) error {
		callout := fragment.Children()[0].(*crdt.YXmlElement)
		callout.DeleteAttribute(txn, "kind")
		callout.DeleteAttribute(txn, "title")
		return nil
	}); err != nil {
		t.Fatalf("remove the callout's attributes: %v", err)
	}

	read, err := Read(fragment)
	if err != nil {
		t.Fatalf("read a callout without kind and title: %v", err)
	}
	if got := read.Children[0].Attrs; got["kind"] != "note" || got["title"] != "" {
		t.Fatalf("callout attributes = %#v, want kind note and an empty title", got)
	}
	if rendered, err := Render(read); err != nil || rendered != want {
		t.Fatalf("render the read callout = %q, %v; want %q", rendered, err, want)
	}

	built := &Node{Type: "doc", Children: []*Node{{
		Type:     "callout",
		Attrs:    Attrs{BlockIDAttr: "callout-1"},
		Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "Body."}}}},
	}}}
	if err := built.Validate(); err != nil {
		t.Fatalf("validate a callout without kind and title: %v", err)
	}
	if rendered, err := Render(built); err != nil || rendered != want {
		t.Fatalf("render a callout without kind and title = %q, %v; want %q", rendered, err, want)
	}
}

// The manifest names every node and mark of the Proof schema, and this package's node and mark
// sets are the manifest's.
func TestTheManifestNamesEveryNodeAndMarkThisPackageKnows(t *testing.T) {
	t.Parallel()
	wantNodes := []string{
		"blockquote", "bullet_list", "code_block", "doc", "footnote_definition", "footnote_reference",
		"frontmatter", "hardbreak", "heading", "hr", "html", "image", "list_item", "ordered_list",
		"paragraph", "table", "table_cell", "table_header", "table_header_row", "table_row", "text",
	}
	wantMarks := []string{
		"dispatchAsk", "emphasis", "inlineCode", "link", "proofApproved", "proofAuthored",
		"proofComment", "proofFlagged", "proofSuggestion", "strike_through", "strong",
	}
	var manifest struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
		Marks []struct {
			Name string `json:"name"`
		} `json:"marks"`
	}
	if err := json.Unmarshal(SchemaJSON(), &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	var nodes, marks []string
	for _, node := range manifest.Nodes {
		nodes = append(nodes, node.Name)
	}
	for _, mark := range manifest.Marks {
		marks = append(marks, mark.Name)
	}
	slices.Sort(nodes)
	slices.Sort(marks)
	if !slices.Equal(nodes, wantNodes) {
		t.Errorf("manifest nodes = %v, want %v", nodes, wantNodes)
	}
	if !slices.Equal(marks, wantMarks) {
		t.Errorf("manifest marks = %v, want %v", marks, wantMarks)
	}
	if got := slices.Sorted(maps.Keys(active.Load().nodes)); !slices.Equal(got, wantNodes) {
		t.Errorf("node types = %v, want %v", got, wantNodes)
	}
	if got := slices.Sorted(maps.Keys(active.Load().marks)); !slices.Equal(got, wantMarks) {
		t.Errorf("mark types = %v, want %v", got, wantMarks)
	}
}

// A newer build creates an element this build reads but may not create yet. This build reads a
// document holding it, from the live tree or from stored markdown, and edits around it. No write
// of this build creates one: markdown written in whole or in part, a retype, or a tree written
// over a document that does not hold the element. Each refusal tells the writer what to do. A tree
// written over an empty document as content a stored version already holds (a rebuild's seed)
// creates nothing, so it is written.
func TestAnElementThisBuildReadsButCannotCreateIsReadAndNeverCreated(t *testing.T) {
	const markdown = ":::future{#future-1}\nBody.\n:::\n"
	const instruction = "Remove it from your markdown, or write it again after the next release"
	refusesFuture := func(t *testing.T, what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), `typed block "future"`) || !strings.Contains(err.Error(), instruction) {
			t.Fatalf("%s: error = %v, want ErrSchema naming typed block \"future\" and saying %q", what, err, instruction)
		}
	}

	shipped, shippedCreatable := SchemaVersion(), SchemaCreatable()
	restoreCreatable, err := UseStagedBlockForTest("future", true)
	if err != nil {
		t.Fatalf("stage the future block as creatable: %v", err)
	}
	t.Cleanup(restoreCreatable)
	if got, want := SchemaCreatable(), shipped+1; got != want {
		t.Fatalf("SchemaCreatable() where future is creatable = %d, want %d", got, want)
	}
	written, err := ParseForWrite(markdown, nil)
	if err != nil {
		t.Fatalf("a build that creates future writes it: %v", err)
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	if err := doc.TransactE(func(txn *crdt.Transaction) error {
		return Update(txn, fragment, written)
	}); err != nil {
		t.Fatalf("a build that creates future stores it: %v", err)
	}

	restoreUncreatable, err := UseStagedBlockForTest("future", false)
	if err != nil {
		t.Fatalf("stage the future block as not creatable: %v", err)
	}
	t.Cleanup(restoreUncreatable)
	if got, want := SchemaVersion(), shipped+1; got != want {
		t.Fatalf("SchemaVersion() = %d, want %d", got, want)
	}
	if got := SchemaCreatable(); got != shippedCreatable {
		t.Fatalf("SchemaCreatable() where future is not creatable = %d, want the shipped manifest's %d", got, shippedCreatable)
	}
	versions, declared := Element{Kind: TypedBlockElement, Name: "future"}.Versions()
	if !declared || versions != (BlockElementVersions{Since: shipped + 1, CreatableSince: shipped + 2}) {
		t.Fatalf("future versions = %+v, %t; want since %d, creatable_since %d", versions, declared, shipped+1, shipped+2)
	}
	read, err := Read(fragment)
	if err != nil {
		t.Fatalf("read a stored tree holding future: %v", err)
	}
	if rendered, err := Render(read); err != nil || rendered != markdown {
		t.Fatalf("render a stored tree holding future = %q, %v; want %q", rendered, err, markdown)
	}
	if _, err := Parse(markdown); err != nil {
		t.Fatalf("read stored markdown holding future: %v", err)
	}
	if _, err := ParseForWrite(markdown+"\nMore.\n", read); err != nil {
		t.Fatalf("rewrite a document that holds future already: %v", err)
	}

	_, err = ParseForWrite(markdown, nil)
	refusesFuture(t, "write markdown holding future", err)
	_, err = ParseFragment(markdown, false, NewWriteBudget())
	refusesFuture(t, "write a fragment holding future", err)
	plain, err := Parse("Question?\n")
	if err != nil {
		t.Fatalf("parse a paragraph: %v", err)
	}
	_, err = RetypeBlock(plain, plain.Children[0].Attrs[BlockIDAttr].(string), "future", nil)
	refusesFuture(t, "retype a paragraph as future", err)
	empty := crdt.New()
	emptyFragment := empty.GetXmlFragment("prosemirror")
	err = empty.TransactE(func(txn *crdt.Transaction) error {
		return Update(txn, emptyFragment, read)
	})
	refusesFuture(t, "write a tree holding future over a document without it", err)
	reseeded := crdt.New()
	reseededFragment := reseeded.GetXmlFragment("prosemirror")
	if err := reseeded.TransactE(func(txn *crdt.Transaction) error {
		return UpdateHolding(txn, reseededFragment, read, read)
	}); err != nil {
		t.Fatalf("seed an empty document with a stored tree holding future: %v", err)
	}
	if seeded, err := Read(reseededFragment); err != nil {
		t.Fatalf("read the seeded document: %v", err)
	} else if rendered, err := Render(seeded); err != nil || rendered != markdown {
		t.Fatalf("the seeded document renders %q (%v), want the stored %q", rendered, err, markdown)
	}
}

// DocumentSince is the highest since among the declared elements a document holds: the schema
// version a client needs to represent all of it. A document of version 1 elements is 1, one that
// also holds an element the staged manifest reads from its own version is that version, and no
// document is 0. An element the manifest does not declare has no since, and counts for nothing.
func TestDocumentSinceIsTheHighestSinceTheDocumentHolds(t *testing.T) {
	restore, err := UseStagedBlockForTest("future", true)
	if err != nil {
		t.Fatalf("stage the future block as creatable: %v", err)
	}
	t.Cleanup(restore)
	plain, err := Parse("Question?\n")
	if err != nil {
		t.Fatalf("parse a paragraph: %v", err)
	}
	future, err := Parse("Question?\n\n:::future{#future-1}\nBody.\n:::\n")
	if err != nil {
		t.Fatalf("parse a document holding future: %v", err)
	}
	unknown := &Node{Type: "doc", Children: []*Node{{Type: "paragraph"}, {Type: "unknown_node"}}}
	for _, c := range []struct {
		name string
		doc  *Node
		want int
	}{
		{name: "no document", doc: nil, want: 0},
		{name: "a paragraph", doc: plain, want: 1},
		{name: "a paragraph and a future block", doc: future, want: SchemaVersion()},
		{name: "a paragraph and an undeclared node", doc: unknown, want: 1},
	} {
		if got := DocumentSince(c.doc); got != c.want {
			t.Errorf("DocumentSince(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

// manifestAsOf is the embedded manifest as the build of an earlier release read it: its version
// lowered to version, and every element it reads only from a later one dropped. An element whose
// since is at most version stays as it is, creatable_since above version included: that is an
// element the earlier build already read and did not create.
func manifestAsOf(t *testing.T, version int) []byte {
	t.Helper()
	var value BlockSchema
	if err := json.Unmarshal(SchemaJSON(), &value); err != nil {
		t.Fatalf("decode the embedded manifest: %v", err)
	}
	later := func(since int) bool { return since > version }
	value.Version = version
	value.Nodes = slices.DeleteFunc(value.Nodes, func(node BlockSchemaElement) bool { return later(node.Since) })
	value.Marks = slices.DeleteFunc(value.Marks, func(mark BlockSchemaElement) bool { return later(mark.Since) })
	value.Types = slices.DeleteFunc(value.Types, func(typ BlockTypeSchema) bool { return later(typ.Since) })
	for index := range value.Types {
		typ := &value.Types[index]
		typ.Attributes = slices.DeleteFunc(typ.Attributes, func(attr BlockAttribute) bool { return later(attr.Since) })
		for attrIndex := range typ.Attributes {
			attr := &typ.Attributes[attrIndex]
			attr.Choices = slices.DeleteFunc(slices.Clone(attr.Choices), func(choice string) bool {
				return later(attr.ChoiceVersions[choice].Since)
			})
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the manifest as of version %d: %v", version, err)
	}
	return encoded
}

// The typed blocks manifest version 3 added are staged by their numbers: a build at version 2,
// whose manifest is this one as of that version, does not declare them. That is a build of a
// release before the first one that shipped them: the three blocks ship from legion-envoy-v29.1.0
// at manifest version 3, so the releases from it on declare all three, and legion-envoy-v29.0.0,
// at manifest version 2, is the shape this models. Such a build creates none of them - a write
// naming one is refused - and a stored document holding one reads as an element it does not know
// (ErrUnknownElement), which the document service answers 409 DOC_NEWER_CONTENT for rather than
// repairing or rebuilding the document. The name says "this version" from when version 3 was the
// manifest's; the Verification document's check names the row, so it keeps its name, and the
// elements versions 4, 5 and 11 add each have a row of their own
// (TestTheAttributeVersionFourAddsIsUnknownToABuildAtVersionThree,
// TestTheAttributeVersionFiveAddsIsUnknownToABuildAtVersionFour,
// TestTheAttributeMergesIsUnknownToABuildOneVersionBack).
func TestTheTypedBlocksThisVersionAddsAreUnknownToABuildAtTheVersionBefore(t *testing.T) {
	const adds = 3
	added := []string{"requirement", "plan_task", "check"}
	for _, name := range added {
		versions, declared := Element{Kind: TypedBlockElement, Name: name}.Versions()
		if !declared || versions.Since != adds {
			t.Fatalf("typed block %q = %+v, %t; want since %d, the version that adds it", name, versions, declared, adds)
		}
	}
	markdown := func(name string) string {
		return ":::" + name + "{#b-" + name + "}\nBody.\n:::\n"
	}
	stored, err := Parse(markdown("requirement") + "\n" + markdown("plan_task") + "\n" + markdown("check"))
	if err != nil {
		t.Fatalf("this build reads a document holding the blocks it adds: %v", err)
	}
	for _, name := range added {
		if _, err := ParseForWrite(markdown(name), nil); err != nil {
			t.Fatalf("this build writes a %s block: %v", name, err)
		}
	}

	before := adds - 1
	restore, err := UseManifestForTest(manifestAsOf(t, before))
	if err != nil {
		t.Fatalf("install the manifest as of version %d: %v", before, err)
	}
	t.Cleanup(restore)
	if got := SchemaVersion(); got != before {
		t.Fatalf("SchemaVersion() of a build at the version before = %d, want %d", got, before)
	}
	for _, name := range added {
		if IsTypedBlock(name) {
			t.Errorf("a build at version %d declares typed block %q, which version %d adds", before, name, adds)
		}
		_, err := ParseForWrite(markdown(name), nil)
		if !errors.Is(err, ErrUnknownElement) || !strings.Contains(err.Error(), name) {
			t.Errorf("a build at version %d writing a %s block = %v, want ErrUnknownElement naming it", before, name, err)
		}
	}
	if err := stored.Validate(); !errors.Is(err, ErrUnknownElement) {
		t.Errorf("a build at version %d reading a stored document holding them = %v, want ErrUnknownElement", before, err)
	}
}

// The check block's ci_check attribute, which main added with manifest version 4 (the name of the
// GitHub check run that proves the check), is staged by its own numbers in the same way: this
// build reads and writes it, and a build at version 3 - legion-envoy-v30.11.0 is one - declares
// the check block but not the attribute, so it creates none and reads a stored check carrying one
// as an element it does not know (ErrUnknownElement, answered 409 DOC_NEWER_CONTENT).
func TestTheAttributeVersionFourAddsIsUnknownToABuildAtVersionThree(t *testing.T) {
	const adds = 4
	attribute := Element{Kind: AttributeElement, Block: "check", Name: "ci_check"}
	versions, declared := attribute.Versions()
	if !declared || versions.Since != adds || versions.CreatableSince != adds {
		t.Fatalf("attribute ci_check of check = %+v, %t; want since and creatable_since %d", versions, declared, adds)
	}
	if SchemaVersion() < adds {
		t.Fatalf("SchemaVersion() = %d, below the version %d that adds ci_check", SchemaVersion(), adds)
	}
	const markdown = ":::check{#b-c1 when=\"before_merge\" negative_control=\"n\" ci_check=\"build\"}\nBody.\n:::\n"
	stored, err := Parse(markdown)
	if err != nil {
		t.Fatalf("this build reads a check carrying ci_check: %v", err)
	}
	if _, err := ParseForWrite(markdown, nil); err != nil {
		t.Fatalf("this build writes a check carrying ci_check: %v", err)
	}

	before := adds - 1
	restore, err := UseManifestForTest(manifestAsOf(t, before))
	if err != nil {
		t.Fatalf("install the manifest as of version %d: %v", before, err)
	}
	t.Cleanup(restore)
	if !IsTypedBlock("check") {
		t.Fatalf("a build at version %d does not declare the check block, which version 3 adds", before)
	}
	if _, err := ParseForWrite(markdown, nil); !errors.Is(err, ErrUnknownElement) || !strings.Contains(err.Error(), "ci_check") {
		t.Errorf("a build at version %d writing a check carrying ci_check = %v, want ErrUnknownElement naming it", before, err)
	}
	if err := stored.Validate(); !errors.Is(err, ErrUnknownElement) {
		t.Errorf("a build at version %d reading a stored check carrying ci_check = %v, want ErrUnknownElement", before, err)
	}
}

// The plan_task block's steps attribute, which manifest version 5 adds (the steps an owner's plan
// task runs, in order), is staged by its own numbers in the same way: this build reads and writes
// it, and a build at version 4 - the release before it - declares the plan_task block but not the
// attribute, so it reads and writes a plan task without one, creates none carrying it and reads a
// stored plan task carrying one as an element it does not know (ErrUnknownElement, answered 409
// DOC_NEWER_CONTENT).
func TestTheAttributeVersionFiveAddsIsUnknownToABuildAtVersionFour(t *testing.T) {
	const adds = 5
	attribute := Element{Kind: AttributeElement, Block: "plan_task", Name: "steps"}
	versions, declared := attribute.Versions()
	if !declared || versions.Since != adds || versions.CreatableSince != adds {
		t.Fatalf("attribute steps of plan_task = %+v, %t; want since and creatable_since %d", versions, declared, adds)
	}
	if SchemaVersion() < adds {
		t.Fatalf("SchemaVersion() = %d, below the version %d that adds steps", SchemaVersion(), adds)
	}
	const markdown = ":::plan_task{#b-t1 steps=\"implementer reviewer\"}\nBody.\n:::\n"
	const withoutSteps = ":::plan_task{#b-t1}\nBody.\n:::\n"
	stored, err := Parse(markdown)
	if err != nil {
		t.Fatalf("this build reads a plan task carrying steps: %v", err)
	}
	if _, err := ParseForWrite(markdown, nil); err != nil {
		t.Fatalf("this build writes a plan task carrying steps: %v", err)
	}

	before := adds - 1
	restore, err := UseManifestForTest(manifestAsOf(t, before))
	if err != nil {
		t.Fatalf("install the manifest as of version %d: %v", before, err)
	}
	t.Cleanup(restore)
	if !IsTypedBlock("plan_task") {
		t.Fatalf("a build at version %d does not declare the plan_task block, which version 3 adds", before)
	}
	if _, err := Parse(withoutSteps); err != nil {
		t.Errorf("a build at version %d reading a plan task without steps = %v, want no error", before, err)
	}
	if _, err := ParseForWrite(withoutSteps, nil); err != nil {
		t.Errorf("a build at version %d writing a plan task without steps = %v, want no error", before, err)
	}
	if _, err := ParseForWrite(markdown, nil); !errors.Is(err, ErrUnknownElement) || !strings.Contains(err.Error(), "steps") {
		t.Errorf("a build at version %d writing a plan task carrying steps = %v, want ErrUnknownElement naming it", before, err)
	}
	if err := stored.Validate(); !errors.Is(err, ErrUnknownElement) {
		t.Errorf("a build at version %d reading a stored plan task carrying steps = %v, want ErrUnknownElement", before, err)
	}
}

// The plan_task block's merges attribute, which manifest version 11 adds (whether the work of a
// plan task merges by itself, "true" or "false"), is staged by its own numbers in the same way:
// this build reads and writes it, and a build one version back declares the plan_task block and
// its steps but not merges, so it creates none carrying it and reads a stored plan task carrying
// one as an element it does not know (ErrUnknownElement, answered 409 DOC_NEWER_CONTENT). Versions
// 6 to 10 are other changes' numbers, so the build one version back is a build at version 10.
func TestTheAttributeMergesIsUnknownToABuildOneVersionBack(t *testing.T) {
	const adds = 11
	attribute := Element{Kind: AttributeElement, Block: "plan_task", Name: "merges"}
	versions, declared := attribute.Versions()
	if !declared || versions != (BlockElementVersions{Since: adds, CreatableSince: adds}) {
		t.Fatalf("attribute merges of plan_task = %+v, %t; want since and creatable_since %d", versions, declared, adds)
	}
	if SchemaVersion() < adds {
		t.Fatalf("SchemaVersion() = %d, below the version %d that adds merges", SchemaVersion(), adds)
	}
	const markdown = ":::plan_task{#a merges=\"true\" steps=\"implementer\"}\nDo it.\n:::\n"
	const withoutMerges = ":::plan_task{#a steps=\"implementer\"}\nDo it.\n:::\n"
	stored, err := Parse(markdown)
	if err != nil {
		t.Fatalf("this build reads a plan task carrying merges: %v", err)
	}
	for _, value := range []string{"true", "false"} {
		written := ":::plan_task{#a merges=\"" + value + "\" steps=\"implementer\"}\nDo it.\n:::\n"
		if _, err := ParseForWrite(written, nil); err != nil {
			t.Fatalf("this build writes a plan task carrying merges=%q: %v", value, err)
		}
	}
	const maybe = ":::plan_task{#a merges=\"maybe\" steps=\"implementer\"}\nDo it.\n:::\n"
	if _, err := ParseForWrite(maybe, nil); err == nil || !strings.Contains(err.Error(), "must be one of true, false") {
		t.Errorf("this build writing a plan task carrying merges=\"maybe\" = %v, want a refusal naming the choices true and false", err)
	}

	before := adds - 1
	restore, err := UseManifestForTest(manifestAsOf(t, before))
	if err != nil {
		t.Fatalf("install the manifest as of version %d: %v", before, err)
	}
	t.Cleanup(restore)
	if !IsTypedBlock("plan_task") {
		t.Fatalf("a build at version %d does not declare the plan_task block, which version 3 adds", before)
	}
	if _, err := ParseForWrite(withoutMerges, nil); err != nil {
		t.Errorf("a build at version %d writing a plan task without merges = %v, want no error", before, err)
	}
	if _, err := ParseForWrite(markdown, nil); !errors.Is(err, ErrUnknownElement) || !strings.Contains(err.Error(), "merges") {
		t.Errorf("a build at version %d writing a plan task carrying merges = %v, want ErrUnknownElement naming it", before, err)
	}
	if _, err := Parse(markdown); !errors.Is(err, ErrUnknownElement) || !strings.Contains(err.Error(), "merges") {
		t.Errorf("a build at version %d parsing a plan task carrying merges = %v, want ErrUnknownElement naming it", before, err)
	}
	if err := stored.Validate(); !errors.Is(err, ErrUnknownElement) {
		t.Errorf("a build at version %d reading a stored plan task carrying merges = %v, want ErrUnknownElement", before, err)
	}
}

// storeManifestAhead is the manifest a store one version ahead of this build holds: the
// embedded one with its version raised by one and change applied to it. The build installs it
// through Install, which reads it as asOfVersion leaves it, so each case here is a rollback -
// the store migrated by a newer release, the pod carrying the one before it.
func storeManifestAhead(t *testing.T, change func(value *BlockSchema, ahead int)) BlockSchema {
	t.Helper()
	var value BlockSchema
	if err := json.Unmarshal(SchemaJSON(), &value); err != nil {
		t.Fatalf("decode the embedded manifest: %v", err)
	}
	ahead := BuiltSchemaVersion() + 1
	value.Version = ahead
	change(&value, ahead)
	return value
}

// changeManifestAttribute applies change to the schema of the attribute named attribute of
// typed block block, and fails the test where value declares no such attribute.
func changeManifestAttribute(t *testing.T, value *BlockSchema, block, attribute string, change func(*BlockAttributeSchema)) {
	t.Helper()
	for typeIndex := range value.Types {
		if value.Types[typeIndex].Name != block {
			continue
		}
		for attrIndex := range value.Types[typeIndex].Attributes {
			if value.Types[typeIndex].Attributes[attrIndex].Name == attribute {
				change(&value.Types[typeIndex].Attributes[attrIndex].BlockAttributeSchema)
				return
			}
		}
	}
	t.Fatalf("the manifest declares no attribute %q of typed block %q", attribute, block)
}

// installStoreManifest installs value as the server installs what it read from the store, and
// puts this build's own manifest back when the test ends. The manifest is the whole process's
// (active), so a test that calls this runs alone (runsAlone).
func installStoreManifest(t *testing.T, value BlockSchema) error {
	t.Helper()
	runsAlone(t)
	previous := active.Load()
	t.Cleanup(previous.install)
	return Install(value)
}

// runsAlone makes the testing package refuse t as a parallel test, now and later: t.Setenv fails
// a test that has called t.Parallel, or whose parent has, and t.Parallel fails a test that has
// called t.Setenv. Every helper that swaps the process-wide manifest or block schema calls it, so
// a test that swaps either cannot run beside the tests that read it, and cannot restore it out of
// order with another swapper (pmdoctest.UseBlockSchema keeps the same rule). Nothing reads the
// variable.
func runsAlone(t testing.TB) {
	t.Helper()
	t.Setenv("PMDOC_TEST_SWAPS_THE_BLOCK_SCHEMA", "1")
}

// A store whose enum default a newer release moved to a choice it added installs on the build
// before it, at that build's own default for the attribute. The default is a value of the
// attribute like any other, so a default naming a choice this build does not read is one
// validateBlockSchema refuses; refusing it would fail Install, and a failed install at boot
// exits the process (cmd/dispatch's main), which is a crash loop on rollback.
func TestAnEnumDefaultANewerStoreMovedInstallsAsThisBuildsOwn(t *testing.T) {
	const block, attribute, added = "callout", "kind", "banner"
	own := embedded.types[block].Attributes.Get(attribute).Default
	if own == nil {
		t.Fatalf("this build declares no default for attribute %q of typed block %q, so this proves nothing", attribute, block)
	}
	value := storeManifestAhead(t, func(value *BlockSchema, ahead int) {
		changeManifestAttribute(t, value, block, attribute, func(attr *BlockAttributeSchema) {
			attr.Choices = append(slices.Clone(attr.Choices), added)
			attr.ChoiceVersions = maps.Clone(attr.ChoiceVersions)
			if attr.ChoiceVersions == nil {
				attr.ChoiceVersions = map[string]BlockElementVersions{}
			}
			attr.ChoiceVersions[added] = BlockElementVersions{Since: ahead, CreatableSince: ahead}
			attr.Default = added
		})
	})

	if err := installStoreManifest(t, value); err != nil {
		t.Fatalf("install a store that moved the %s default of %s to the choice %q it adds: %v", attribute, block, added, err)
	}
	if got := SchemaVersion(); got != BuiltSchemaVersion() {
		t.Fatalf("SchemaVersion() = %d, want this build's own %d", got, BuiltSchemaVersion())
	}
	installed := active.Load().types[block].Attributes.Get(attribute)
	if slices.Contains(installed.Choices, added) {
		t.Errorf("the installed choices of %s are %v, want the choice %q the newer release adds left out", attribute, installed.Choices, added)
	}
	if installed.Default != own {
		t.Errorf("the installed default of %s is %v, want this build's own %v", attribute, installed.Default, own)
	}
	// What the default decides: a block whose markdown writes no value reads it, so a build
	// that installed a default it cannot name would answer one its own readers refuse.
	read, err := Parse(":::" + block + "{#b-c1}\nBody.\n:::\n")
	if err != nil {
		t.Fatalf("parse a %s that writes no %s: %v", block, attribute, err)
	}
	if got := read.Children[0].Attrs[attribute]; got != own {
		t.Errorf("a %s that writes no %s reads %v, want this build's own default %v", block, attribute, got, own)
	}
}

// A `block[]` attribute this build reads, every target of which names a typed block only a
// newer release declares, is left out of the installed manifest altogether. An empty target set
// means "any block" to CheckBlockPointers, which skips every pointer of such an attribute, so
// keeping it would let this build accept at any block the pointers the newer release holds to a
// type this one cannot even name.
func TestABlockAttributeWhoseTargetsANewerStoreAllAddsIsNotInstalled(t *testing.T) {
	const block, attribute, target = "check", "cites", "fact"
	built := BuiltSchemaVersion()
	value := storeManifestAhead(t, func(value *BlockSchema, ahead int) {
		value.Types = append(slices.Clone(value.Types), BlockTypeSchema{
			Name: target, Content: BlockContentParagraphs, Render: "host",
			Since: ahead, CreatableSince: ahead,
		})
		for index := range value.Types {
			if value.Types[index].Name != block {
				continue
			}
			value.Types[index].Attributes = append(slices.Clone(value.Types[index].Attributes), BlockAttribute{
				Name: attribute,
				BlockAttributeSchema: BlockAttributeSchema{
					Kind: BlockAttributeBlockList, Targets: []string{target},
					Since: built, CreatableSince: built,
				},
			})
		}
	})

	if err := installStoreManifest(t, value); err != nil {
		t.Fatalf("install a store whose %s attribute of %s points only at the %s block it adds: %v", attribute, block, target, err)
	}
	if IsTypedBlock(target) {
		t.Fatalf("this build declares typed block %q, which only the newer release does, so this proves nothing", target)
	}
	if installed, declared := active.Load().types[block].Attributes.Lookup(attribute); declared {
		t.Errorf("the installed %s block declares %s with targets %v: an attribute whose every target is gone points at any block, and must be dropped instead",
			block, attribute, installed.Targets)
	}
	if kinds := LinkKinds(); slices.Contains(kinds, attribute) {
		t.Errorf("the installed link kinds are %v, want %q left out with the attribute that makes it", kinds, attribute)
	}
	// The tree a newer release stored, built here rather than parsed: markdown holding an
	// attribute no installed manifest declares is refused before a tree exists.
	stored := &Node{Type: "doc", Children: []*Node{{
		Type:     block,
		Attrs:    Attrs{BlockIDAttr: "b-c1", attribute: "dispatch://CORE-1/spec#b-x"},
		Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "Body."}}}},
	}}}
	if pointers := BlockPointers(stored); len(pointers) != 0 {
		t.Errorf("a %s carrying %s holds the pointers %+v, want none: a pointer with no target is one CheckBlockPointers never checks",
			block, attribute, pointers)
	}
	if err := stored.Validate(); !errors.Is(err, ErrUnknownElement) || !strings.Contains(err.Error(), attribute) {
		t.Errorf("reading a stored %s carrying %s = %v, want ErrUnknownElement naming it", block, attribute, err)
	}
}

// An enum attribute this build reads, every choice of which only a newer release declares, is
// left out of the installed manifest altogether, as the block[] attribute above is. Its choices
// filtered to the ones this build reads are none, and an enum with no choice is one
// validateAttributeSchema refuses: Install would fail, and a failed install at boot exits the
// process (cmd/dispatch's main), which is a crash loop on rollback.
func TestAnEnumAttributeWhoseChoicesANewerStoreAllAddsIsNotInstalled(t *testing.T) {
	const block, attribute, choice = "check", "tone", "urgent"
	if _, declared := embedded.types[block].Attributes.Lookup(attribute); declared {
		t.Fatalf("this build declares attribute %q of typed block %q, which only the newer release does, so this proves nothing", attribute, block)
	}
	built := BuiltSchemaVersion()
	value := storeManifestAhead(t, func(value *BlockSchema, ahead int) {
		for index := range value.Types {
			if value.Types[index].Name != block {
				continue
			}
			value.Types[index].Attributes = append(slices.Clone(value.Types[index].Attributes), BlockAttribute{
				Name: attribute,
				BlockAttributeSchema: BlockAttributeSchema{
					Kind: BlockAttributeEnum, Choices: []string{choice},
					ChoiceVersions: map[string]BlockElementVersions{choice: {Since: ahead, CreatableSince: ahead}},
					Since:          built, CreatableSince: built,
				},
			})
		}
	})

	if err := installStoreManifest(t, value); err != nil {
		t.Fatalf("install a store whose %s attribute of %s holds only the choice %q it adds: %v", attribute, block, choice, err)
	}
	if installed, declared := active.Load().types[block].Attributes.Lookup(attribute); declared {
		t.Errorf("the installed %s block declares %s with the choices %v: an enum whose every choice is gone can hold no value this build reads, and must be dropped instead",
			block, attribute, installed.Choices)
	}
	stored := &Node{Type: "doc", Children: []*Node{{
		Type:     block,
		Attrs:    Attrs{BlockIDAttr: "b-c1", attribute: choice},
		Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "Body."}}}},
	}}}
	if err := stored.Validate(); !errors.Is(err, ErrUnknownElement) || !strings.Contains(err.Error(), attribute) {
		t.Errorf("reading a stored %s carrying %s = %v, want ErrUnknownElement naming it", block, attribute, err)
	}
}
