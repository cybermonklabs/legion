package refs

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The run of a pass's block values builds the facts of one batch of blocks at a time
// (blockValueBatch) and keeps that batch alone: read in order, each block's facts come in the
// order planBlockValues states them - its rank, its text, its placement, the retraction of the
// text parent it held, its text_parent, its text_rank and then its plan values - and the run never
// holds the facts of more blocks than one batch; read out of order, it answers the same fact for an
// index.
func TestABlockValueRunHoldsOneBatchOfFactsAndStatesEachBlocksInOrder(t *testing.T) {
	at := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	blocks := 3*blockValueBatch + 7
	run := &blockValueRun{
		document: IndexedVersion{RefKey: "RUN-1/spec", At: at, Writer: who},
		ids:      make([]string, blocks), standing: map[string]heldBlockValue{},
		states: make([]uint8, blocks), texts: make([]string, blocks), batch: -1,
	}
	for index := range blocks {
		id := fmt.Sprintf("b%05d", index)
		run.ids[index] = id
		run.states[index] = statesRank | statesText | statesPlacement | statesTextParent | statesTextRank
		run.texts[index] = id + " words\n"
		run.standing[id] = heldBlockValue{
			rank: fmt.Sprintf("r%05d", index), placement: PlacementText, textRank: fmt.Sprintf("t%05d", index), text: true,
			textParent: blockFarEnd{kind: graphstore.KindArtifact, id: "document"},
		}
	}
	// One block moved under another text parent and is a plan node whose size its line now writes
	// and whose skills it no longer does; one states no fact at all.
	moved := blockValueBatch + 3
	run.states[moved] |= statesTextParentRetracted
	run.extras = map[int]blockValueExtra{moved: {
		textParent: blockFarEnd{kind: BlockNodeKind, id: "RUN-1/spec#b00000"},
		plan:       []planFact{{name: AttributePlanSize, value: "90", asserted: true}, {name: AttributePlanSkills, value: `["go"]`}},
	}}
	silent := 2*blockValueBatch + 1
	run.states[silent] = 0
	run.count()

	if want := 5*blocks + 1 + 2 - 5; run.Len() != want {
		t.Fatalf("the run holds %d facts, want %d", run.Len(), want)
	}
	var got []string
	most := 0
	for index := range run.Len() {
		fact := run.At(index)
		got = append(got, fact.FromID+" "+fact.Kind+" "+fact.ToKind+":"+fact.ToID+fmt.Sprintf(" %t", fact.Asserted))
		most = max(most, len(run.built))
		if !fact.At.Equal(at) || fact.Who != who {
			t.Fatalf("fact %d is dated %v by %+v, want the pass's own time and writer", index, fact.At, fact.Who)
		}
	}
	if most > 5*blockValueBatch+3 {
		t.Errorf("the run held %d facts at once, want one batch's, %d blocks' at most", most, blockValueBatch)
	}
	var want []string
	for index := range blocks {
		if index == silent {
			continue
		}
		id := fmt.Sprintf("b%05d", index)
		address := "RUN-1/spec#" + id
		want = append(want,
			address+" rank value:"+fmt.Sprintf("r%05d", index)+" true",
			address+" text value:"+id+" words\n true",
			address+" placement value:text true",
		)
		if index == moved {
			want = append(want, address+" text_parent block:RUN-1/spec#b00000 false")
		}
		want = append(want,
			address+" text_parent artifact:document true",
			address+" text_rank value:"+fmt.Sprintf("t%05d", index)+" true",
		)
		if index == moved {
			want = append(want, address+" size value:90 true", address+" skills value:[\"go\"] false")
		}
	}
	if !slices.Equal(got, want) {
		for index := range min(len(got), len(want)) {
			if got[index] != want[index] {
				t.Fatalf("fact %d of %d is %q, want %q", index, len(got), got[index], want[index])
			}
		}
		t.Fatalf("the run built %d facts, want %d", len(got), len(want))
	}
	for _, index := range []int{run.Len() - 1, 0, 5*blockValueBatch + 2, 1} {
		if fact := run.At(index); fact.FromID+" "+fact.Kind+" "+fact.ToKind+":"+fact.ToID+fmt.Sprintf(" %t", fact.Asserted) != want[index] {
			t.Errorf("read out of order, fact %d is %+v, want %q", index, fact.Tuple, want[index])
		}
	}
}

// The first index of a document of more blocks than one batch of its values (blockValueBatch)
// appends each block's values in the order the pass states them, every block's in document order:
// the text_parent of each block among the relations, and its rank, text, placement and text_rank
// among the attributes, one block after another. A pass over the same version states none again.
func TestTheFirstIndexOfALargeDocumentAppendsItsValuesInTheirOrder(t *testing.T) {
	database := parentDatabase(t)
	document := newParentDocument(t, database, "BATCH", "batch-md")
	ids := make([]string, 2*blockValueBatch+5)
	for index := range ids {
		ids[index] = fmt.Sprintf("p%05d", index)
	}
	doc := lines(ids...)
	stated := indexed(t, document, 1, doc)

	var relations, attributes []string
	for _, tuple := range statedOutOf(stated, document, TupleTextParent, rankAttribute, textAttribute, placementAttribute, AttributeTextRank) {
		row := tuple.FromID[len(document.refKey)+1:] + " " + tuple.Kind
		if tuple.ToKind == graphstore.KindValue {
			attributes = append(attributes, row)
			continue
		}
		relations = append(relations, row)
	}
	var wantRelations, wantAttributes []string
	for _, id := range ids {
		wantRelations = append(wantRelations, id+" "+TupleTextParent)
		wantAttributes = append(wantAttributes, id+" "+rankAttribute, id+" "+textAttribute, id+" "+placementAttribute, id+" "+AttributeTextRank)
	}
	if !slices.Equal(relations, wantRelations) {
		t.Errorf("the pass appended %d text_parent tuples, want one of each of the %d blocks in document order", len(relations), len(ids))
	}
	if !slices.Equal(attributes, wantAttributes) {
		for index := range min(len(attributes), len(wantAttributes)) {
			if attributes[index] != wantAttributes[index] {
				t.Fatalf("value %d of %d the pass appended is %q, want %q", index, len(attributes), attributes[index], wantAttributes[index])
			}
		}
		t.Fatalf("the pass appended %d values, want %d", len(attributes), len(wantAttributes))
	}
	for _, id := range []string{ids[0], ids[blockValueBatch], ids[len(ids)-1]} {
		block := doc.blocks[slices.IndexFunc(doc.blocks, func(block model.ArtifactBlock) bool { return block.ID == id })]
		if text, _ := document.currentValue(t, id, textAttribute); text != doc.markdown[block.From:block.To] {
			t.Errorf("block %s's text value is %q, want its slice of the version %q", id, text, doc.markdown[block.From:block.To])
		}
	}

	again := indexed(t, document, 2, doc)
	if values := statedOutOf(again, document, TupleTextParent, rankAttribute, textAttribute, placementAttribute, AttributeTextRank); len(values) != 0 {
		t.Errorf("a pass over a version that moved nothing stated %d values, want none", len(values))
	}
}
