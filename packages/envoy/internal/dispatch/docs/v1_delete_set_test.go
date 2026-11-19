package docs

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// readV1DeleteSet is update's delete set as eachV1DeleteRange reads it, and whether it read.
func readV1DeleteSet(update []byte) (map[crdt.ClientID][]crdt.IDRange, bool) {
	read := crdt.NewIDSet()
	ok := eachV1DeleteRange(update, func(client crdt.ClientID, clock, length uint64) {
		read.Add(client, clock, length)
	})
	return rangesByClient(read), ok
}

// rangesByClient is set's ranges by client, merged as the set merges them.
func rangesByClient(set *crdt.IDSet) map[crdt.ClientID][]crdt.IDRange {
	ranges := make(map[crdt.ClientID][]crdt.IDRange)
	if set == nil {
		return ranges
	}
	for _, client := range set.Clients() {
		ranges[client] = set.Ranges(client)
	}
	return ranges
}

// The delete set the reader finds past an update's structs is the one ygo's own decoder finds
// there (crdt.ContentIDsFromUpdateV1, which decodes every struct into a document to reach it), for
// the frames a browser sends and the states a room answers with: each frame a docstest peer's
// transaction makes of an edit of each kind the editor writes, the crafted frames docstest builds
// (a link whose attribute nests values, a chain of blocks nested deep), whole states, and
// hand-written frames of every kind of content the decoder reads and the editor never writes -
// JSON content as JSON text and as the lib0 Any values an earlier Yjs build wrote, binary content,
// a type by its name, a subdocument, Any content nested deep. Each frame cut short by its last
// byte, which its delete set holds, reads as no delete set at all.
func TestTheDeleteSetReaderReadsWhatYgosDecoderReads(t *testing.T) {
	doc := seededDocumentCopy(t)
	fragment := doc.GetXmlFragment(fragmentName)
	text := func() *crdt.YXmlText { return paragraphTextsOf(fragment)[0] }
	edit := func(change func(*crdt.Transaction)) []byte {
		t.Helper()
		update := docstest.Transact(doc, change)
		if update == nil {
			t.Fatal("the edit made no update")
		}
		return update
	}
	frames := []struct {
		name   string
		update []byte
	}{
		{"typing", edit(func(txn *crdt.Transaction) { text().Insert(txn, 0, "typed ahead ", nil) })},
		{"a delete inside a run", edit(func(txn *crdt.Transaction) { text().Delete(txn, 2, 5) })},
		{"a replace", edit(func(txn *crdt.Transaction) {
			text().Delete(txn, 0, 3)
			text().Insert(txn, 0, "new", crdt.Attributes{"strong": true})
		})},
		{"a format", edit(func(txn *crdt.Transaction) { text().Format(txn, 1, 4, crdt.Attributes{"emphasis": true}) })},
		{"an embed", edit(func(txn *crdt.Transaction) { text().InsertEmbed(txn, 0, map[string]any{"src": "x.png"}, nil) })},
		{"an element with an attribute", edit(func(txn *crdt.Transaction) {
			heading := crdt.NewYXmlElement("heading")
			fragment.InsertElement(txn, 0, heading)
			heading.SetAttribute(txn, "level", "2")
		})},
		{"a block deleted whole", edit(func(txn *crdt.Transaction) { fragment.Delete(txn, 0, 1) })},
		{"a margin record", edit(func(txn *crdt.Transaction) {
			txn.GetMap(marksMapName).Set(txn, "c-1", map[string]any{"kind": "comment", "by": "ada", "at": []any{1.5, int64(7), nil, true}})
		})},
		{"a margin record deleted", edit(func(txn *crdt.Transaction) { txn.GetMap(marksMapName).Delete(txn, "c-1") })},
		{"a link whose attribute nests values", docstest.NestedLinkUpdate(t, doc, map[string]any{"deep": []any{map[string]any{"deeper": "x"}}})},
		{"a chain of blocks nested deep", edit(func(txn *crdt.Transaction) { docstest.WriteDeepChain(txn, fragment, 40, "deep") })},
		{"the whole state", crdt.EncodeStateAsUpdateV1(doc, nil)},
		{"JSON content as JSON text", newV1Frame(1).group(1, 7, 0).rooted(structJSONRef, marksMapName, "j").
			uint(3).str(`{"a":[1,2.5e3,"\"q\""]}`).str("undefined").str("null").uint(1).uint(7).uint(1).uint(0).uint(1).done()},
		{"JSON content as lib0 Any values", newV1Frame(1).group(1, 7, 0).rooted(structJSONRef, marksMapName, "j").
			uint(2).any(map[string]any{"a": "b"}).any([]any{"x", 1.25}).uint(1).uint(7).uint(1).uint(0).uint(1).done()},
		{"binary content", newV1Frame(1).group(1, 7, 0).rooted(3, marksMapName, "b").str("\x00\x01\xff").
			uint(2).uint(7).uint(1).uint(0).uint(1).uint(9).uint(1).uint(4).uint(2).done()},
		{"a type by its name", newV1Frame(1).group(1, 7, 0).rooted(structTypeRef, fragmentName, "").raw(3).str("paragraph").
			uint(1).uint(7).uint(2).uint(0).uint(1).uint(3).uint(4).done()},
		{"a subdocument", newV1Frame(1).group(1, 7, 0).rooted(structDocumentRef, marksMapName, "d").str("guid-1").
			any(map[string]any{"autoLoad": true}).uint(1).uint(7).uint(1).uint(0).uint(1).done()},
		{"Any content nested deep", newV1Frame(1).group(1, 7, 0).rooted(structAnyRef, marksMapName, "n").uint(1).
			any(nestedAny(60)).uint(1).uint(7).uint(1).uint(0).uint(1).done()},
		{"a GC run and a skip", newV1Frame(2).group(1, 8, 0).gc(10).group(1, 9, 3).skip(4).
			uint(2).uint(8).uint(1).uint(2).uint(5).uint(9).uint(1).uint(0).uint(3).done()},
	}
	for _, frame := range frames {
		t.Run(frame.name, func(t *testing.T) {
			ids, err := crdt.ContentIDsFromUpdateV1(frame.update)
			if err != nil {
				t.Fatalf("ygo's decoder does not read the frame: %v", err)
			}
			want := rangesByClient(ids.Deletes)
			got, ok := readV1DeleteSet(frame.update)
			if !ok {
				t.Fatalf("the reader did not read the %d-byte frame's delete set, which ygo reads as %v", len(frame.update), want)
			}
			if !maps.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("the reader read the delete set %v, where ygo's decoder reads %v", got, want)
			}
			if _, ok := readV1DeleteSet(frame.update[:len(frame.update)-1]); ok {
				t.Fatalf("the frame cut short by its last byte read as a whole delete set")
			}
			t.Logf("%d bytes, delete set %v", len(frame.update), got)
		})
	}
}

// nestedAny is a lib0 Any value depth arrays deep.
func nestedAny(depth int) any {
	var value any = strings.Repeat("v", 3)
	for range depth {
		value = []any{value}
	}
	return value
}

// The reader refuses what ygo's decoder refuses on the way to the delete set - a struct of a kind
// no Yjs build writes, a type reference past the last Yjs defines, an Any value nested past the
// depth ygo reads - rather than read the bytes after it as a delete set.
func TestTheDeleteSetReaderRefusesWhatYgosDecoderRefuses(t *testing.T) {
	for _, frame := range []struct {
		name   string
		update []byte
	}{
		{"a struct of kind 12", newV1Frame(1).group(1, 7, 0).rooted(12, marksMapName, "x").uint(0).uint(0).done()},
		{"a type reference past the last", newV1Frame(1).group(1, 7, 0).rooted(structTypeRef, fragmentName, "").raw(7).uint(0).done()},
		{"an Any value nested too deep", newV1Frame(1).group(1, 7, 0).rooted(structAnyRef, marksMapName, "n").uint(1).
			any(nestedAny(maxAnyDepth + 2)).uint(0).done()},
	} {
		t.Run(frame.name, func(t *testing.T) {
			if _, err := crdt.ContentIDsFromUpdateV1(frame.update); err == nil {
				t.Fatal("ygo's decoder reads the frame, so it is no frame the reader must refuse")
			}
			if _, ok := readV1DeleteSet(frame.update); ok {
				t.Fatal("the reader read a delete set past a struct ygo's decoder refuses")
			}
		})
	}
}
