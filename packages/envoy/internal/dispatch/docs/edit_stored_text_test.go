package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/logtest"
)

// misreadBefore is a paragraph of an emphasised `(` and then a `y` both emphasised and strong,
// which reads back as written. misreadReplace writes a strong `x` between the two: the smallest
// shape found by reducing, its letters written as x and its digits as 0, the paragraph of a real
// document an edit stored reading back otherwise. The writer of that edit wrote the run it leaves
// as misreadWritten, where the run of three asterisks after the `(` cannot close the emphasis, so
// the text read back otherwise, as another run that writer could not write either, and an upload
// of it was refused too. The writer now spells it misreadSpelled, its `x` a reference, so that run
// both closes and opens (pmdoc's finalResorts).
const misreadBefore = "*(**y***\n"

var misreadReplace = model.EditOp{Op: "replace", Find: "y", With: "**x*y***"}

const misreadSpelled, misreadWritten = "*(***&#120;*y***", "*(***x*y***"

// storedTextEditor is the session the tests below edit as.
var storedTextEditor = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

// judgeAsWrittenBefore has the stored-text check judge every text holding misreadSpelled with
// misreadWritten in its place (judgedTexts): the text the earlier writer stored for that run, which
// reads back otherwise. It fails the test where no text the check judged held misreadSpelled, since
// the check was then handed nothing that reads back otherwise.
func judgeAsWrittenBefore(t *testing.T) {
	t.Helper()
	var rewritten atomic.Int64
	rewrite := func(markdown string) string {
		if strings.Contains(markdown, misreadSpelled) {
			rewritten.Add(1)
		}
		return strings.ReplaceAll(markdown, misreadSpelled, misreadWritten)
	}
	judgedTexts.Store(&rewrite)
	t.Cleanup(func() {
		judgedTexts.Store(nil)
		if rewritten.Load() == 0 {
			t.Errorf("no text the stored-text check judged held %q, so it judged no text that reads back otherwise", misreadSpelled)
		}
	})
}

// The edit misreadReplace makes, which an earlier writer stored with a text an upload of it is
// refused for, is stored with a text an upload of it is stored with: the writer spells the run it
// leaves so it reads back.
func TestAnEditOfARunTheWriterOnceMisspelledIsStoredAsAnUploadOfItIs(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, misreadBefore)
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{misreadReplace}, storedTextEditor, nil); err != nil {
		t.Fatalf("the edit was refused: %v, want it stored", err)
	}
	stored, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document the edit stored: %v", err)
	}
	if _, err := pmdoc.ParseForWrite(stored, nil); err != nil {
		t.Fatalf("the edit was stored as %q, which an upload of it is refused for: %v", stored, err)
	}
	if !strings.Contains(stored, misreadSpelled) {
		t.Errorf("the edit was stored as %q, want the run spelled %q", stored, misreadSpelled)
	}
}

// The text an edit batch stores is judged as an upload of that text is: a replace's own checks
// read back only the shape of the block it changed, so an edit could store a text an upload of it
// is refused for. An edit whose text would read back otherwise is refused with an upload's words,
// on the operation that wrote there, and leaves the document as it was. The writer spells the run
// this edit leaves so it reads back, so the check is handed the text the earlier writer stored for
// it (judgeAsWrittenBefore).
func TestAnEditWhoseTextWouldReadBackOtherwiseIsRefusedAsAnUploadOfItIs(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, misreadBefore)
	before, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document before the edit: %v", err)
	}
	if _, err := pmdoc.ParseForWrite(before, nil); err != nil {
		t.Fatalf("the document before the edit, %q, is refused as an upload: %v", before, err)
	}
	judgeAsWrittenBefore(t)

	_, err = joinedApplyOps(service, artifactID, []model.EditOp{misreadReplace}, storedTextEditor, nil)
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" || !strings.HasPrefix(err.Error(), "operation 0: ") ||
		!strings.Contains(err.Error(), "reads back otherwise") {
		t.Fatalf("the edit was refused with %v, want operation 0's with refused as reading back otherwise", err)
	}
	after, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document after the refused edit: %v", err)
	}
	if after != before {
		t.Fatalf("the refused edit left the document %q, want it as it was, %q", after, before)
	}
}

// A document whose text already reads back otherwise in one block - a browser edit or an earlier
// writer can leave one - still takes an edit elsewhere: the difference the text held before the
// edit, in the same block and the same characters, is not the edit's. It is stored all the same,
// so it is logged, by block id and the kind of difference alone, since a log line carries no
// document text. A browser writes the second block into the tree misreadReplace writes, and the
// check is handed that block as the earlier writer stored it (judgeAsWrittenBefore), in the text
// before the edit and in the text the edit stores alike.
func TestAnEditBesideABlockThatAlreadyReadsBackOtherwiseIsStoredAndLogsTheBlock(t *testing.T) {
	logs := &logtest.Recorder{}
	logtest.SetDefault(t, logs)
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Plain words.\n\n"+misreadBefore)
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		paragraph := tree.Children[1]
		if len(paragraph.Children) != 2 || pmdoc.TextContent(paragraph) != "(y" {
			t.Fatalf("the second block holds %d runs, want the two misreadBefore writes", len(paragraph.Children))
		}
		var strong pmdoc.Mark
		for _, mark := range paragraph.Children[1].Marks {
			if mark.Type == "strong" {
				strong = mark
			}
		}
		x := &pmdoc.Node{Type: "text", Text: "x", Marks: []pmdoc.Mark{strong}}
		paragraph.Children = []*pmdoc.Node{paragraph.Children[0], x, paragraph.Children[1]}
		return tree
	})
	tree := liveTree(t, service, artifactID)
	misread := blockID(tree.Children[1])
	stored, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document the browser left: %v", err)
	}
	if !strings.Contains(stored, misreadSpelled) {
		t.Fatalf("the document the browser left, %q, does not hold %q: the edit below would prove nothing", stored, misreadSpelled)
	}
	if verdict, err := pmdoc.RefuseMisreadStored(nil, "", tree, strings.ReplaceAll(stored, misreadSpelled, misreadWritten)); err != nil || !errors.Is(verdict.Refusal, pmdoc.ErrMisreadRendering) {
		t.Fatalf("the document the browser left, as the earlier writer stored it, does not read back otherwise (%v, %v): the edit below would prove nothing", verdict.Refusal, err)
	}
	judgeAsWrittenBefore(t)

	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "Plain words.", With: "Plain words, edited."}}, storedTextEditor, nil); err != nil {
		t.Fatalf("the edit of the first block was refused: %v, want it stored", err)
	}
	var excused []slog.Record
	for _, record := range logs.Records() {
		if record.Message == "dispatch: a write stores a block that reads back otherwise, as the text it replaces already did" {
			excused = append(excused, record)
		}
	}
	if len(excused) != 1 {
		t.Fatalf("the edit logged %d blocks it stored reading back otherwise, want the one the browser left:\n%s", len(excused), recordList(logs.Records()))
	}
	attributes := map[string]string{}
	excused[0].Attrs(func(attribute slog.Attr) bool {
		attributes[attribute.Key] = attribute.Value.String()
		return true
	})
	if attributes["room"] != artifactID || attributes["block"] != misread || attributes["difference"] != "paragraph text" || len(attributes) != 3 {
		t.Fatalf("the edit logged %v, want room %s, block %s and the difference paragraph text, and nothing else", attributes, artifactID, misread)
	}
}

// An edit settles the spread of only the lists and list items it changed (pmdoc.SettleDerived): an
// item beside the text it writes keeps the spread a browser edit gave it, which its markdown does
// not carry, since that spread is not the edit's, and the edit is stored all the same.
func TestAnEditLeavesTheSpreadOfAnItemBesideTheTextItWrites(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "- Body.\n- two\n")
	sibling := func(tree *pmdoc.Node) *pmdoc.Node {
		var found *pmdoc.Node
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if node.Type == "list_item" {
				found = node
			}
			return true
		})
		return found
	}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		sibling(tree).Attrs["spread"] = true
		return tree
	})
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "Body.", With: "Changed."}}, storedTextEditor, nil); err != nil {
		t.Fatalf("the edit of the first item was refused: %v, want it stored", err)
	}
	if spread := sibling(liveTree(t, service, artifactID)).Attrs["spread"]; spread != true {
		t.Fatalf("the item beside the edited text holds spread %v after the edit, want the true it held", spread)
	}
}

// recordList names every logged record by its message and attributes, for a failure to read.
func recordList(records []slog.Record) string {
	lines := make([]string, 0, len(records))
	for _, record := range records {
		line := "\t" + record.Message
		record.Attrs(func(attribute slog.Attr) bool {
			line += " " + attribute.String()
			return true
		})
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// countBlockReadBacks counts every read-back of a document-level block the package makes from here
// to the end of the test (blockReadBacksCounted), as countRenders records its renders. One test
// counts at a time, and the count is taken away in t.Cleanup.
func countBlockReadBacks(t *testing.T) *atomic.Int64 {
	t.Helper()
	count := &atomic.Int64{}
	if !blockReadBacksCounted.CompareAndSwap(nil, count) {
		t.Fatal("another test is counting the block read-backs already")
	}
	t.Cleanup(func() { blockReadBacksCounted.Store(nil) })
	return count
}

// A batch of replaces in one large block reads that block back once for each replace and once for
// the block it started from, since each replace starts from the very block the one before it left
// and the batch shares one read-back of each, and it renders the document once: the read-back it
// answers with is cut from the rendering it stores. A conditional batch runs its operations a
// second time to resolve the blocks its precondition must name, and judges nothing in that run, so
// it costs the same. Read-backs held for one operation each would read the block twice for every
// replace, and twice more in the conditional run, and a read-back rendered apart from the text the
// edit stores would render the document twice.
func TestReplacesInOneBlockReadItBackOnceEachAndRenderTheDocumentOnce(t *testing.T) {
	const replaces = 4
	words := make([]string, 2000)
	for index := range words {
		words[index] = fmt.Sprintf("w%04d", index)
	}
	large := strings.Join(words, " ") + "."
	ops := make([]model.EditOp, replaces)
	for index := range ops {
		word := words[100*(index+1)]
		ops[index] = model.EditOp{Op: "replace", Find: word, With: "v" + word[1:]}
	}
	for _, conditional := range []bool{false, true} {
		t.Run(fmt.Sprintf("conditional=%v", conditional), func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "Intro.\n\n"+large+"\n")
			settleCurrentGeneration(t, service, artifactID)
			// The edit before the measured one leaves the room's replica holding the state it
			// wrote, so the measured edit meets a warm room, as an edit of a document someone
			// reads does, and the rendering of the document before it is the replica's.
			if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "Intro.", With: "Intro, warmed."}}, storedTextEditor, nil); err != nil {
				t.Fatalf("warm the room with a first edit: %v", err)
			}
			var precondition *model.EditPrecondition
			if conditional {
				block := liveTree(t, service, artifactID).Children[1]
				token, err := nodeToken(block)
				if err != nil {
					t.Fatal(err)
				}
				precondition = &model.EditPrecondition{Blocks: []model.EditBlockPrecondition{{ID: blockID(block), Token: token}}}
			}

			readBacks := countBlockReadBacks(t)
			renders := countRenders(t)
			outcome, err := joinedApplyOps(service, artifactID, ops, storedTextEditor, precondition)
			rendered := renders()
			if err != nil {
				t.Fatalf("apply %d replaces in one block: %v", replaces, err)
			}
			if !outcome.Changed {
				t.Fatal("the replaces changed nothing")
			}
			if got := readBacks.Load(); got > replaces+1 {
				t.Errorf("%d replaces in one block read it back %d times, want at most %d", replaces, got, replaces+1)
			}
			stored, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read the edited document: %v", err)
			}
			if len(rendered) != 1 || rendersOf(rendered, stored) != 1 {
				t.Errorf("the edit rendered %d documents, %d of them the text it stored, want that one alone", len(rendered), rendersOf(rendered, stored))
			}
		})
	}
}
