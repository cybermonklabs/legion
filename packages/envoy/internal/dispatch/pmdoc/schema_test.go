package pmdoc

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// A block has one parent, so the plan_task block's parent attribute holds at most one reference,
// wherever it is read: markdown parsed for a write, a stored version's markdown, and a tree's
// value. The values and the refusal are the shared table's rows of parent and of needs
// (sharedPointerRows), so the rule is held to the words @legion/contracts' parentProblem gives.
// The manifest here is this one at the version after it, which creates parent, so each refusal is
// the limit's own and not the release order's. needs, another block[] attribute of the same block,
// holds two, so the limit is parent's alone. A manifest whose parent is not a block[] attribute is
// refused: parent places a block, and only a block[] attribute points at one.
func TestAParentHoldsAtMostOneReference(t *testing.T) {
	var one, two, needsTwo sharedPointerRow
	for _, row := range sharedPointerRows(t) {
		switch {
		case row.Attribute == ParentAttribute && row.OK && row.Value != "":
			one = row
		case row.Attribute == ParentAttribute && !row.OK:
			two = row
		case row.Attribute == "needs" && strings.Contains(row.Value, " "):
			needsTwo = row
		}
	}
	if one.Value == "" || two.Problem == "" || needsTwo.Value == "" {
		t.Fatalf("the shared table holds no row of one parent (%+v), of two parents with a problem (%+v), or of a needs of two (%+v)", one, two, needsTwo)
	}
	restore, err := UseManifestForTest(manifestAheadForTest(t, func(*BlockSchema, int) {}))
	if err != nil {
		t.Fatalf("install the manifest of the version after this one: %v", err)
	}
	t.Cleanup(restore)

	tree, err := ParseForWrite(":::plan_task{#t2 parent=\""+one.Value+"\"}\nBody.\n:::\n", nil)
	if err != nil {
		t.Fatalf("a plan task under one block = %v, want it written", err)
	}
	twice := ":::plan_task{#t2 parent=\"" + two.Value + "\"}\nBody.\n:::\n"
	if _, err := ParseForWrite(twice, nil); err == nil || !strings.Contains(err.Error(), two.Problem) {
		t.Errorf("a write of a plan task under two blocks = %v, want it refused: %s", err, two.Problem)
	}
	if _, err := Parse(twice); err == nil || !strings.Contains(err.Error(), two.Problem) {
		t.Errorf("a stored plan task under two blocks = %v, want it refused: %s", err, two.Problem)
	}
	// A tree holds the value as the array Yjs gives back.
	items := []any{}
	for _, item := range strings.Split(two.Value, " ") {
		items = append(items, item)
	}
	tree.Children[0].Attrs["parent"] = items
	if err := tree.Validate(); err == nil || !strings.Contains(err.Error(), two.Problem) {
		t.Errorf("a tree's plan task under two blocks = %v, want it refused: %s", err, two.Problem)
	}
	if _, err := ParseForWrite(":::plan_task{#t2 needs=\""+needsTwo.Value+"\"}\nBody.\n:::\n", nil); err != nil {
		t.Errorf("a plan task that needs two blocks = %v, want it written", err)
	}

	for _, kind := range []BlockAttributeKind{BlockAttributeString, BlockAttributeStringList} {
		manifest := manifestAheadForTest(t, func(value *BlockSchema, _ int) {
			for index := range value.Types {
				if value.Types[index].Name != "plan_task" {
					continue
				}
				value.Types[index].Attributes = append(slices.DeleteFunc(value.Types[index].Attributes, func(attribute BlockAttribute) bool {
					return attribute.Name == "parent"
				}), BlockAttribute{Name: "parent", BlockAttributeSchema: BlockAttributeSchema{Kind: kind}})
			}
		})
		restore, err := UseManifestForTest(manifest)
		if err == nil {
			restore()
			t.Errorf("a manifest whose parent is a %s attribute was installed, want it refused", kind)
			continue
		}
		if !strings.Contains(err.Error(), `attribute "parent" places a block in its tree, so it must be a block[] attribute`) {
			t.Errorf("a manifest whose parent is a %s attribute = %v, want it refused as no block[] attribute", kind, err)
		}
	}
}

// manifestAheadForTest is storeManifestAhead encoded: the embedded manifest at the version after
// it, with change applied, as UseManifestForTest takes it.
func manifestAheadForTest(t *testing.T, change func(value *BlockSchema, ahead int)) []byte {
	t.Helper()
	encoded, err := json.Marshal(storeManifestAhead(t, change))
	if err != nil {
		t.Fatalf("encode the manifest of the version after this one: %v", err)
	}
	return encoded
}
