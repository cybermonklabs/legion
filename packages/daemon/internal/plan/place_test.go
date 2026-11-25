package plan

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// top, partPlace and elementPlace are the three places a task block stands at (Places).
var top = Place{Kind: PlaceTop}

func partPlace(task string) Place    { return Place{Kind: PlacePart, Task: task} }
func elementPlace(task string) Place { return Place{Kind: PlaceElement, Task: task} }

// under is a task block id that stands directly under the block parent, "" for none, with the
// sub-owner owner, "" for none. Its `parent` names parent in the plan's document, which FromBlocks
// reads its Parent from, and its Parent is set for a test of Places alone.
func under(id, parent, owner string) Block {
	attributes := map[string]string{"steps": "implementer"}
	if owner != "" {
		attributes = map[string]string{"owner": owner}
	}
	return withParent(Block{ID: id, Type: TaskBlockType, Attributes: attributes, Text: "What block " + id + " says."}, parent)
}

// withParent is block standing directly under the block parent of the plan's document: its
// `parent` names that block, as a writer of the document writes it, and its Parent is parent. A
// block with no attributes is given a map of its own. parent "" leaves block under no block.
func withParent(block Block, parent string) Block {
	if parent == "" {
		return block
	}
	attributes := map[string]string{"parent": planDocument + "#b-" + parent}
	for name, value := range block.Attributes {
		if name != "parent" {
			attributes[name] = value
		}
	}
	block.Attributes, block.Parent = attributes, parent
	return block
}

// Each task block stands at one of three places, by the block it stands directly under. Under no
// block, under a typed block that is no task block, and under an id no block holds, it is a task
// at the top. Under a task with a sub-owner it is a part of that task, and under a task with no
// sub-owner an element of it. Under an element it is an element of that element's task. A part of
// a part with a sub-owner is a part of it, so the depth is not bounded, and a parent written later
// in the document places its block all the same. Task blocks that stand under each other in a
// cycle have no place, and Places errs, naming them.
//
// The faults are a part read as an element, or an element as a part, which Legion would carry out
// as work. The control is a Places that answers a part for every parent that is a task: the rows
// of an element fail.
func TestPlacesGivesEachTaskBlockItsPlace(t *testing.T) {
	requirement := Block{ID: "r1", Type: "requirement", Text: "What the plan serves."}
	role := Block{ID: "prober", Type: RoleBlockType, Text: "You probe."}
	callout := Block{ID: "note", Type: "callout", Text: "A note."}
	for _, tc := range []struct {
		name   string
		blocks []Block
		want   map[string]Place
		err    string
	}{
		{name: "under no block", blocks: []Block{under("a", "", "")}, want: map[string]Place{"a": top}},
		{name: "under a task with a sub-owner", blocks: []Block{under("f", "", "architect"), under("f1", "f", "")},
			want: map[string]Place{"f": top, "f1": partPlace("f")}},
		{name: "under a task with no sub-owner", blocks: []Block{under("a", "", ""), under("a1", "a", "")},
			want: map[string]Place{"a": top, "a1": elementPlace("a")}},
		{name: "under an element", blocks: []Block{under("a", "", ""), under("a1", "a", ""), under("a1x", "a1", "architect"), under("a1y", "a1x", "")},
			want: map[string]Place{"a": top, "a1": elementPlace("a"), "a1x": elementPlace("a"), "a1y": elementPlace("a")}},
		{name: "a part of a part whose task has a sub-owner", blocks: []Block{under("f", "", "architect"), under("f1", "f", "architect"), under("f1a", "f1", "")},
			want: map[string]Place{"f": top, "f1": partPlace("f"), "f1a": partPlace("f1")}},
		{name: "under a part with no sub-owner", blocks: []Block{under("f", "", "architect"), under("f1", "f", ""), under("f1a", "f1", "")},
			want: map[string]Place{"f": top, "f1": partPlace("f"), "f1a": elementPlace("f1")}},
		{name: "under a requirement, a role block and a callout", blocks: []Block{requirement, role, callout, under("a", "r1", ""), under("b", "prober", ""), under("c", "note", "")},
			want: map[string]Place{"a": top, "b": top, "c": top}},
		{name: "under an id no block holds", blocks: []Block{under("a", "gone", "")}, want: map[string]Place{"a": top}},
		{name: "a parent written later in the document", blocks: []Block{under("f1", "f", ""), under("f", "", "architect")},
			want: map[string]Place{"f": top, "f1": partPlace("f")}},
		{name: "two task blocks under each other", blocks: []Block{under("a", "", ""), under("c", "b", "architect"), under("b", "c", "architect")},
			err: "task blocks c, b stand under each other, so none of them has a place in the plan"},
		{name: "a task block under itself", blocks: []Block{under("a", "a", "")},
			err: "task block a stands under itself, so it has no place in the plan"},
		{name: "a chain into a cycle", blocks: []Block{under("x", "a", ""), under("a", "b", "architect"), under("b", "a", "architect")},
			err: "task blocks a, b stand under each other, so none of them has a place in the plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Places(tc.blocks)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("Places = %+v, %v, want the error %q", got, err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Places =\n%+v, %v\nwant\n%+v", got, err, tc.want)
			}
		})
	}
}

// ParentOf reads a task block's `parent` before Dispatch serves the tree, by the rows of the reader.
// No `parent` names no block. A reference to a block of the plan's document names that block's id,
// as a need's is read, a pin left out and the id percent-decoded, whether the block is a task block,
// another typed block, or an id the version holds no block of: Places and Validate judge what the
// id is. A reference to a block of another document of the issue, of another issue, or of an ask,
// a comment or a message names no block of the plan, whatever its id. The primary document's
// `/spec` form names the primary document alone, and a document's `/artifact/<slug>` form that
// document alone. A value that is not one `dispatch://` reference is an error, and so is a
// reference to this document whose id does not decode.
//
// The fault is a block of another document read as the block of this document with its id. The
// control is a ParentOf that reads the id after `#b-` from any reference: the rows of another
// document, of another issue and of the other form fail.
func TestParentOfReadsAParentBeforeTheTree(t *testing.T) {
	const plan, primary = "dispatch://ACME-1/artifact/plan-md", "dispatch://ACME-1/spec"
	for _, tc := range []struct {
		name, value, document, want, err string
	}{
		{name: "no parent", value: "", document: plan},
		{name: "a block of the plan's document", value: plan + "#b-f", document: plan, want: "f"},
		{name: "a pinned block of the plan's document", value: plan + "#b-f@v3", document: plan, want: "f"},
		{name: "an id written with an escape", value: plan + "#b-a%2Eb", document: plan, want: "a.b"},
		{name: "a heading's id from the live document", value: plan + "#b-h-why", document: plan, want: "h-why"},
		{name: "the primary document's form, where the plan is the primary document", value: primary + "#b-f", document: primary, want: "f"},
		{name: "the primary document's form, where the plan is not", value: primary + "#b-f", document: plan},
		{name: "the artifact form, where the plan is the primary document", value: plan + "#b-f", document: primary},
		{name: "a block of another document of the issue", value: "dispatch://ACME-1/artifact/notes-md#b-f", document: plan},
		{name: "a block of another issue", value: "dispatch://ACME-2/artifact/plan-md#b-f", document: plan},
		{name: "an ask", value: "dispatch://ACME-1/ask/0f9e8d7c-6b5a-4c3d-8e2f-1a0b9c8d7e6f", document: plan},
		{name: "a block of a comment", value: "dispatch://ACME-1/comment/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d#b-f", document: plan},
		{name: "a block of a message", value: "dispatch://ACME-1/message/2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e#b-f", document: plan},
		{name: "two references", value: plan + "#b-f " + plan + "#b-g", document: plan,
			err: `its parent "` + plan + `#b-f ` + plan + `#b-g" is not one dispatch:// reference`},
		{name: "a bare id", value: "f", document: plan, err: `its parent "f" is not one dispatch:// reference`},
		{name: "an id that does not decode", value: plan + "#b-f%zz", document: plan,
			err: `its parent "` + plan + `#b-f%zz" names a block of this document by an id that does not decode`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParentOf(tc.value, tc.document)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("ParentOf(%q) = %q, %v, want the error %q", tc.value, got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParentOf(%q) = %q, %v, want %q", tc.value, got, err, tc.want)
			}
		})
	}
}

// FromBlocks reads each task block where Places puts it, and Validate refuses what an element or a
// task cannot be. A part is a task of the plan with its task in PartOf, and an element is no task,
// kept in Elements with the block it stands under and what it names of `steps`, `owner` and
// `merges`. Validate refuses an element that names any of the three, and a task that needs an
// element, naming both blocks. An element that names none of them is taken, no reason names it, its
// own `needs` included, and its text is no task's. A `parent` outside the plan's document, a
// requirement of the spec or a block of another document or issue, puts its block at the top
// (ParentOf). A task block whose Parent names an id no block of the version holds, a heading's, a
// deleted block's or one that never existed, is a task at the top with that id in MissingParent,
// which Validate refuses, naming the block, the id and the version. Task blocks under each other
// are a plan FromBlocks cannot read.
//
// The faults are a part read from `part_of`, which no block writes now, an element read as a task,
// which Legion would carry out, and a block under an id the version lacks taken as a task at the
// top with nothing to refuse. The controls: a read that takes `attributes["part_of"]` as PartOf
// fails the rows of a part (f1 stands at the top); a FromBlocks with Places taken out, so every
// task block is a task, fails the row of an element with serves and needs; and a FromBlocks that
// leaves MissingParent empty fails the rows of a missing parent.
func TestFromBlocksPlacesEachTaskBlockByItsParent(t *testing.T) {
	owned := under("f", "", "architect")
	merging := func(block Block) Block {
		return withParent(Block{ID: block.ID, Type: block.Type, Attributes: map[string]string{"owner": "architect", "merges": "false"}, Text: block.Text}, block.Parent)
	}
	// outside is a task block id whose `parent` writes reference, with its Parent read as
	// register_plan reads it (ParentOf), against the plan's document.
	outside := func(id, reference string) Block {
		block := under(id, "", "")
		block.Attributes["parent"] = reference
		parent, err := ParentOf(reference, "dispatch://ACME-1/artifact/plan-md")
		if err != nil {
			t.Fatalf("ParentOf(%q): %v", reference, err)
		}
		block.Parent = parent
		return block
	}
	// elementReason is Validate's reason for id, an element directly under a, which names names.
	elementReason := func(id, names string) string {
		return "task block " + id + " is an element of task a: it stands under a, which names no sub-owner. " +
			"Legion does not carry out an element, so an element names no steps, owner or merges, and " + id + " names " + names + "."
	}
	// missingReason is Validate's reason for id, whose Parent no block of version holds.
	missingReason := func(id, parent string, version int) string {
		return fmt.Sprintf("task block %s names %s as its parent, which no typed block of version %d of the plan holds. "+
			"Name a typed block of this version in parent, or leave parent out.", id, parent, version)
	}
	// element is t1, an element of a with `serves` and `needs` alone, and withElement the plan of
	// the two.
	element := withParent(Block{ID: "t1", Type: TaskBlockType, Text: "Wire the parser to the tokens.",
		Attributes: map[string]string{"serves": "dispatch://ACME-1/spec#b-r1", "needs": needWord("a")}}, "a")
	withElement := []Block{under("a", "", ""), element}
	atTheTop := Block{ID: element.ID, Type: element.Type, Text: element.Text,
		Attributes: map[string]string{"serves": "dispatch://ACME-1/spec#b-r1", "needs": needWord("a")}}
	gone := Block{ID: "gone", Type: "requirement", Text: "A requirement the plan dropped."}
	needsAnElement := Block{ID: "b", Type: TaskBlockType, Attributes: map[string]string{"steps": "implementer", "needs": needWord("t1")}, Text: "What block b says."}
	for _, tc := range []struct {
		name   string
		blocks []Block
		// version is the version FromBlocks reads, 1 where the row names none.
		version int
		// placed is the PartOf of each task of the plan by its id, elements its elements,
		// missing the MissingParent of each task that has one, and refused Validate's answer.
		placed   map[string]string
		elements []Element
		missing  map[string]string
		refused  []Refusal
		// check holds what else the row says of the plan FromBlocks read.
		check func(t *testing.T, p Plan)
		err   string
	}{
		{name: "no parent", blocks: []Block{under("a", "", "")}, placed: map[string]string{"a": ""}},
		{name: "a part", blocks: []Block{owned, under("f1", "f", "")}, placed: map[string]string{"f": "", "f1": "f"}},
		{name: "a part of a part", blocks: []Block{owned, under("f1", "f", "architect"), under("f1a", "f1", "")},
			placed: map[string]string{"f": "", "f1": "f", "f1a": "f1"}},
		{name: "an element with serves and needs", blocks: withElement,
			placed: map[string]string{"a": ""}, elements: []Element{{ID: "t1", Of: "a", Under: "a"}},
			check: func(t *testing.T, p Plan) {
				for _, refusal := range append(Supported(p), Validate(p, builtin)...) {
					if refusal.Task == "t1" || strings.Contains(refusal.Reason, "t1") {
						t.Errorf("the reason %+v names t1, an element that names nothing only a task carries out", refusal)
					}
				}
				at := slices.IndexFunc(p.Tasks, func(task Task) bool { return task.ID == "a" })
				if at < 0 {
					t.Fatalf("the tasks of the plan = %+v, want a among them", p.Tasks)
				}
				for _, word := range strings.Fields(element.Text) {
					if slices.Contains(strings.Fields(p.Tasks[at].Text), word) {
						t.Errorf("the text of a = %q, which holds %q of the text of its element t1", p.Tasks[at].Text, word)
					}
				}
			}},
		{name: "an element moved to the top", blocks: []Block{under("a", "", ""), atTheTop},
			placed: map[string]string{"a": "", "t1": ""},
			check: func(t *testing.T, p Plan) {
				if got := ChangedTasks(planOf(t, 1, withElement...), p); !reflect.DeepEqual(got, []string{"t1"}) {
					t.Errorf("ChangedTasks(t1 under a, t1 at the top) = %q, want [t1]: t1 is a new task", got)
				}
			}},
		{name: "an element with steps", blocks: []Block{under("a", "", ""), under("a1", "a", "")},
			placed: map[string]string{"a": ""}, elements: []Element{{ID: "a1", Of: "a", Under: "a", Names: []string{"steps"}}},
			refused: []Refusal{{Task: "a1", Reason: elementReason("a1", "steps")}}},
		{name: "an element with a sub-owner", blocks: []Block{under("a", "", ""), under("a1", "a", "architect")},
			placed: map[string]string{"a": ""}, elements: []Element{{ID: "a1", Of: "a", Under: "a", Names: []string{"owner"}}},
			refused: []Refusal{{Task: "a1", Reason: elementReason("a1", "owner")}}},
		{name: "an element that names merges", blocks: []Block{under("a", "", ""), withParent(Block{ID: "a1", Type: TaskBlockType, Attributes: map[string]string{"merges": "false"}}, "a")},
			placed: map[string]string{"a": ""}, elements: []Element{{ID: "a1", Of: "a", Under: "a", Names: []string{"merges"}}},
			refused: []Refusal{{Task: "a1", Reason: elementReason("a1", "merges")}}},
		{name: "an element under an element", blocks: []Block{under("a", "", ""), withParent(Block{ID: "a1", Type: TaskBlockType}, "a"), merging(under("a1x", "a1", ""))},
			placed: map[string]string{"a": ""}, elements: []Element{{ID: "a1", Of: "a", Under: "a"}, {ID: "a1x", Of: "a", Under: "a1", Names: []string{"owner", "merges"}}},
			refused: []Refusal{{Task: "a1x", Reason: "task block a1x is an element of task a: it stands under a1, an element of a. " +
				"Legion does not carry out an element, so an element names no steps, owner or merges, and a1x names owner and merges."}}},
		{name: "a task that needs an element", blocks: []Block{under("a", "", ""), withParent(Block{ID: "t1", Type: TaskBlockType}, "a"), needsAnElement},
			placed: map[string]string{"a": "", "b": ""}, elements: []Element{{ID: "t1", Of: "a", Under: "a"}},
			refused: []Refusal{{Task: "b", Reason: "task b needs " + needWord("t1") + ", which is an element of task a. " +
				"Legion does not carry out an element, so a task cannot need one."}}},
		// Each need holds the id of the element t1, one into another issue and one into another
		// document of the plan's issue: neither names a block of the plan's document, so neither
		// names the element, and both stay needs.
		{name: "a need of another issue's block and of another document's with an element's id",
			blocks: []Block{under("a", "", ""), withParent(Block{ID: "t1", Type: TaskBlockType}, "a"), {ID: "b", Type: TaskBlockType,
				Attributes: map[string]string{"steps": "implementer", "needs": "dispatch://ACME-2/artifact/plan-md#b-t1 dispatch://ACME-1/spec#b-t1"}}},
			placed: map[string]string{"a": "", "b": ""}, elements: []Element{{ID: "t1", Of: "a", Under: "a"}},
			check: func(t *testing.T, p Plan) {
				want := []string{"dispatch://ACME-2/artifact/plan-md#b-t1", "dispatch://ACME-1/spec#b-t1"}
				at := slices.IndexFunc(p.Tasks, func(task Task) bool { return task.ID == "b" })
				if at < 0 || p.Tasks[at].ElementNeeds != nil || !reflect.DeepEqual(p.Tasks[at].Needs, want) {
					t.Errorf("the tasks = %+v, want b with the needs %q and no need of an element", p.Tasks, want)
				}
			}},
		{name: "a requirement of the version", blocks: []Block{{ID: "r1", Type: "requirement"}, under("a", "r1", "")}, placed: map[string]string{"a": ""}},
		// Each reference holds the id of the task a, so a reader that took the id of any
		// reference would put each block under a.
		{name: "a requirement of the spec, and a task block of another document and of another issue",
			blocks: []Block{under("a", "", ""), outside("r", "dispatch://ACME-1/spec#b-a"),
				outside("d", "dispatch://ACME-1/artifact/notes-md#b-a"), outside("i", "dispatch://ACME-2/artifact/plan-md#b-a")},
			placed: map[string]string{"a": "", "r": "", "d": "", "i": ""}},
		{name: "a heading's id from the live document", blocks: []Block{owned, under("f1", "h-why-the-plan", "")},
			placed: map[string]string{"f": "", "f1": ""}, missing: map[string]string{"f1": "h-why-the-plan"},
			refused: []Refusal{{Task: "f1", Reason: missingReason("f1", "h-why-the-plan", 1)}}},
		{name: "an id that never existed", blocks: []Block{owned, under("f1", "gone", "")},
			placed: map[string]string{"f": "", "f1": ""}, missing: map[string]string{"f1": "gone"},
			refused: []Refusal{{Task: "f1", Reason: missingReason("f1", "gone", 1)}}},
		{name: "a block the version before held", blocks: []Block{owned, under("f1", "gone", "")}, version: 2,
			placed: map[string]string{"f": "", "f1": ""}, missing: map[string]string{"f1": "gone"},
			refused: []Refusal{{Task: "f1", Reason: missingReason("f1", "gone", 2)}},
			check: func(t *testing.T, _ Plan) {
				before := planOf(t, 1, owned, gone, under("f1", "gone", ""))
				if r := Validate(before, builtin); len(r) != 0 || before.Tasks[1].MissingParent != "" {
					t.Errorf("version 1, which holds gone, = %+v with the refusals %+v, want f1 taken at the top", before.Tasks, r)
				}
			}},
		{name: "task blocks under each other", blocks: []Block{under("a", "b", "architect"), under("b", "a", "architect")},
			err: "task blocks a, b stand under each other, so none of them has a place in the plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := FromBlocks("plan-md", planDocument, max(tc.version, 1), tc.blocks)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("FromBlocks = %+v, %v, want the error %q", p, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromBlocks: %v", err)
			}
			placed, missing := map[string]string{}, map[string]string{}
			for _, task := range p.Tasks {
				placed[task.ID] = task.PartOf
				if task.MissingParent != "" {
					missing[task.ID] = task.MissingParent
				}
			}
			if !reflect.DeepEqual(placed, tc.placed) {
				t.Errorf("the tasks and their PartOf = %v, want %v", placed, tc.placed)
			}
			if !reflect.DeepEqual(p.Elements, tc.elements) {
				t.Errorf("Elements = %+v, want %+v", p.Elements, tc.elements)
			}
			if tc.missing == nil {
				tc.missing = map[string]string{}
			}
			if !reflect.DeepEqual(missing, tc.missing) {
				t.Errorf("MissingParent of each task = %v, want %v", missing, tc.missing)
			}
			if got := Validate(p, builtin); !reflect.DeepEqual(got, tc.refused) {
				t.Errorf("Validate =\n%+v\nwant\n%+v", got, tc.refused)
			}
			if tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

// An element is no task of the plan: the checks never see it, and a part names its task in PartOf.
// The plan holds f, its part f1 and a with its element a1, and the element's needs and text are no
// task's. The fault is an element read as a task, which Legion would carry out and which a version
// could not change without changing a task. The control is a FromBlocks that keeps each element
// among the tasks: the tasks of the plan and ChangedTasks each fail.
func TestAnElementIsNoTaskOfThePlan(t *testing.T) {
	element := func(text string) Block {
		return withParent(Block{ID: "a1", Type: TaskBlockType, Attributes: map[string]string{"needs": needWord("f")}, Text: text}, "a")
	}
	owned := under("f", "", "architect")
	held := planOf(t, 1, owned, under("f1", "f", ""), under("a", "", ""), element("How a starts."))
	ids := make([]string, len(held.Tasks))
	for i, task := range held.Tasks {
		ids[i] = task.ID
	}
	if want := []string{"f", "f1", "a"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("the tasks of the plan = %q, want %q: the element a1 is none", ids, want)
	}
	if got := held.Tasks[1].PartOf; got != "f" {
		t.Fatalf("PartOf(f1) = %q, want f", got)
	}
	if r := Validate(held, builtin); len(r) != 0 {
		t.Fatalf("Validate = %+v, want none: the element names nothing only a task carries out", r)
	}
	next := planOf(t, 2, owned, under("f1", "f", ""), under("a", "", ""), element("How a starts, said again."))
	if got := ChangedTasks(held, next); len(got) != 0 {
		t.Fatalf("ChangedTasks(the element's text changed) = %q, want none", got)
	}
}
