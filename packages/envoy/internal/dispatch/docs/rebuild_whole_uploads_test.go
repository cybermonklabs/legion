package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// wholeUploadParagraphs and wholeUploadParagraphBytes are the shape of the document the decode
// cap's refusal was measured on (dispatch://INFRA-179): about 3,000 paragraphs and 860 KB of
// markdown, rewritten whole by an agent's uploads until one was refused at 1,047,004 structs.
const (
	wholeUploadParagraphs     = 3_000
	wholeUploadParagraphBytes = 280
)

// wholeUploadFiller is the text every paragraph carries after its number and its revision.
var wholeUploadFiller = strings.Repeat("words of the paragraph ", wholeUploadParagraphBytes/23+1)

// wholeUploadParagraph is paragraph's text in the document's upload number round: its number and
// the round, each of a fixed width, so every upload changes the same few characters of every
// paragraph and none moves the rest.
func wholeUploadParagraph(paragraph, round int) string {
	head := fmt.Sprintf("Paragraph %04d, revision %04d: ", paragraph, round)
	return head + wholeUploadFiller[:wholeUploadParagraphBytes-len(head)]
}

// wholeUploadText is the markdown of the document's upload number round.
func wholeUploadText(round int) string {
	var markdown strings.Builder
	for paragraph := range wholeUploadParagraphs {
		markdown.WriteString(wholeUploadParagraph(paragraph, round))
		markdown.WriteString("\n\n")
	}
	return markdown.String()
}

// wholeUploadTree is the tree an upload of wholeUploadText(round) writes over the document the
// upload before it left: every paragraph's text, and a block id of its own for each paragraph,
// since every paragraph changed (pmdoc.ParseForWrite carries an id only to a block the upload
// leaves unchanged). It is built rather than parsed, which is what makes the fixture cheap: the
// parse with its read-back is three quarters of an upload's cost here.
// TestTheWholeUploadFixtureWritesWhatAnUploadWrites holds the two to the same tree and the same
// structs.
func wholeUploadTree(round int) *pmdoc.Node {
	doc := &pmdoc.Node{Type: "doc", Children: make([]*pmdoc.Node, 0, wholeUploadParagraphs)}
	for paragraph := range wholeUploadParagraphs {
		doc.Children = append(doc.Children, &pmdoc.Node{
			Type:     "paragraph",
			Attrs:    pmdoc.Attrs{pmdoc.BlockIDAttr: fmt.Sprintf("upload-%d-%d", round, paragraph)},
			Children: []*pmdoc.Node{{Type: "text", Text: wholeUploadParagraph(paragraph, round)}},
		})
	}
	return doc
}

// wholeUploadedState is the stored state of the document after rounds uploads.
type wholeUploadedState struct {
	state  []byte
	rounds int
}

// wholeUploads holds the two states wholeUploadedStates builds, once a test process.
var wholeUploads struct {
	sync.Mutex
	band, far *wholeUploadedState
}

// wholeUploadFarHeadroom is how far below the decode cap the far state stands: more than twice
// what the near-cap measure counts for this document (wholeTextWriteAdds, 24,000 structs: 12,000
// a fresh encode holds and two for each of its 3,000 texts and its 3,000 runs).
const wholeUploadFarHeadroom = 60_000

// wholeUploadedStates is the document an agent rewrites whole, upload after upload, each upload
// changing every paragraph's revision and so leaving the paragraph's last revision and its last
// block id deleted behind it: the history the decode cap counts grows by two structs a paragraph
// an upload, while the text stays 860 KB. band is its state once the next upload would carry it
// past the cap, with more room left than a write's smallest replacement needs, so a small write
// still lands while the agent's upload is refused; far is its state once it first stood within
// wholeUploadFarHeadroom of the cap, more than one upload short of the band.
//
// The uploads are written into one document as an upload writes them (pmdoc.Update of
// wholeUploadTree), each a transaction of its own, as each upload is. Built here, about 175
// uploads take about 15 s on one processor, so the states are built rather than stored, once a
// test process.
func wholeUploadedStates(t *testing.T) (band, far wholeUploadedState) {
	t.Helper()
	wholeUploads.Lock()
	defer wholeUploads.Unlock()
	if wholeUploads.band != nil {
		return *wholeUploads.band, *wholeUploads.far
	}
	began := time.Now()
	doc := newDocumentCopy(crdt.WithClientID(0x5eed))
	fragment := doc.GetXmlFragment(fragmentName)
	doc.GetMap(marksMapName)
	var structs, fewest uint64
	for round := 0; ; round++ {
		// The uploads after the first two each delete what the one before wrote and split
		// nothing, so the fewest structs one of them adds is what the next one adds at least.
		if round > 2 && structs+fewest > maxUpdateItems {
			if structs+smallestReplacementAdds > maxUpdateItems {
				t.Fatalf("the document stands at %d structs after %d uploads, within a smallest replacement of the %d cap: the band this fixture is for is the one below that", structs, round, maxUpdateItems)
			}
			wholeUploads.band = &wholeUploadedState{state: crdt.EncodeStateAsUpdateV1(doc, nil), rounds: round}
			break
		}
		if wholeUploads.far == nil && structs+wholeUploadFarHeadroom > maxUpdateItems {
			wholeUploads.far = &wholeUploadedState{state: crdt.EncodeStateAsUpdateV1(doc, nil), rounds: round}
		}
		tree := wholeUploadTree(round)
		if err := doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
			t.Fatal(err)
		}
		added := structCeiling(doc) - structs
		structs += added
		if round >= 2 && (fewest == 0 || added < fewest) {
			fewest = added
		}
	}
	t.Logf("%d whole uploads of %d paragraphs left %d structs in %s (the far state: %d uploads)",
		wholeUploads.band.rounds, wholeUploadParagraphs, structs, time.Since(began), wholeUploads.far.rounds)
	return *wholeUploads.band, *wholeUploads.far
}

// The fixture's uploads are written as built trees rather than parsed from markdown
// (wholeUploadTree), so this holds the two to the same write: for each of the first three uploads,
// the upload's own parse of its markdown over the document before it (pmdoc.ParseForWrite, as
// ReplaceText parses it) makes the built tree but for its block ids, and writing the two into the
// same document adds the same structs.
func TestTheWholeUploadFixtureWritesWhatAnUploadWrites(t *testing.T) {
	built := newDocumentCopy(crdt.WithClientID(0x5eed))
	parsed := newDocumentCopy(crdt.WithClientID(0x5eed))
	var live *pmdoc.Node
	for round := range 3 {
		tree := wholeUploadTree(round)
		upload, err := pmdoc.ParseForWrite(wholeUploadText(round), live)
		if err != nil {
			t.Fatal(err)
		}
		upload = asUploaded(upload)
		if len(upload.Children) != len(tree.Children) {
			t.Fatalf("upload %d parses to %d blocks, want the %d the fixture builds", round, len(upload.Children), len(tree.Children))
		}
		for index, block := range upload.Children {
			if live != nil && block.Attrs[pmdoc.BlockIDAttr] == live.Children[index].Attrs[pmdoc.BlockIDAttr] {
				t.Fatalf("upload %d carries block %d's id %v, want a fresh one for the changed block", round, index, block.Attrs[pmdoc.BlockIDAttr])
			}
		}
		if !upload.Equal(tree) {
			t.Fatalf("upload %d parses to another tree than the fixture builds", round)
		}
		for _, doc := range []*crdt.Doc{built, parsed} {
			fragment := doc.GetXmlFragment(fragmentName)
			write := tree
			if doc == parsed {
				write = upload
			}
			if err := doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, write) }); err != nil {
				t.Fatal(err)
			}
		}
		if built, parsed := countRoomStructs(built), countRoomStructs(parsed); built != parsed {
			t.Fatalf("after upload %d the built trees leave %d structs and the parsed ones %d", round, built, parsed)
		}
		live = upload
	}
}

// atTheElementBound is the most repeats of unit that markdown holds within the upload's bounds
// (pmdoc.MaxDocumentElements, pmdoc.MaxDocumentBytes), behind head and before tail, parsed.
func atTheElementBound(t *testing.T, head, unit, tail string) *pmdoc.Node {
	t.Helper()
	fits := func(repeats int) bool {
		size := pmdoc.MeasureDocument(head + strings.Repeat(unit, repeats) + tail)
		return size.Counted && size.Elements <= pmdoc.MaxDocumentElements && size.Bytes <= pmdoc.MaxDocumentBytes
	}
	low, high := 0, pmdoc.MaxDocumentElements
	for low < high {
		if middle := (low + high + 1) / 2; fits(middle) {
			low = middle
		} else {
			high = middle - 1
		}
	}
	tree, err := pmdoc.Parse(head + strings.Repeat(unit, low) + tail)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// anchoredRuns is tree with a comment's anchor mark of its own on every text run, as a document's
// anchors are written onto its text (MarkSpec's mark).
func anchoredRuns(tree *pmdoc.Node, author model.Actor) *pmdoc.Node {
	anchored := 0
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "text" {
			node.Marks = append(node.Marks, MarkSpec{Kind: MarkComment, ID: fmt.Sprintf("c%d", anchored), By: author}.pmMark())
			anchored++
		}
		return true
	})
	return tree
}

// wholeTextWriteCeiling is above what the near-cap measure counts for the heaviest documents an
// upload can write - each at the upload's element bound, built of the block or the inline syntax
// that writes the most structs an element - and for two of them carrying an anchor mark on every
// run, which no markdown writes and a document's anchors put on its text. A document further from
// the cap than the ceiling is far without its tree being read (nearTheDecodeCap), so a document
// whose measure passed it would be answered far wrongly.
//
// The floors a document's read counts in place of the measure (NearDecodeCap) never pass it on
// any of these documents, nor on the whole-upload fixture's: the tree walk (wholeTextWriteFloor)
// and the top-level blocks a room's replica records. A floor that passed the measure would let the
// page offer a rebuild the route refuses.
func TestNoDocumentAnUploadCanWriteCountsPastTheWholeTextWriteCeiling(t *testing.T) {
	author := model.Actor{Kind: "user", ID: "alice"}
	for _, row := range []struct {
		name string
		tree func(t *testing.T) *pmdoc.Node
	}{
		{"headings", func(t *testing.T) *pmdoc.Node { return atTheElementBound(t, "", "# a\n", "") }},
		{"paragraphs", func(t *testing.T) *pmdoc.Node { return atTheElementBound(t, "", "a\n\n", "") }},
		{"runs of marked text", func(t *testing.T) *pmdoc.Node { return atTheElementBound(t, "", "***a*** ", "\n") }},
		{"runs of linked text", func(t *testing.T) *pmdoc.Node { return atTheElementBound(t, "", "[a](b) ", "\n") }},
		{"list items", func(t *testing.T) *pmdoc.Node { return atTheElementBound(t, "", "- a\n", "") }},
		{"table cells", func(t *testing.T) *pmdoc.Node {
			return atTheElementBound(t, "| a | b | c | d |\n| - | - | - | - |\n", "| a | b | c | d |\n", "")
		}},
		{"task items", func(t *testing.T) *pmdoc.Node { return atTheElementBound(t, "", "- [ ] a\n", "") }},
		{"paragraphs of anchored runs", func(t *testing.T) *pmdoc.Node {
			return anchoredRuns(atTheElementBound(t, "", "a\n\n", ""), author)
		}},
		{"anchored runs of marked text", func(t *testing.T) *pmdoc.Node {
			return anchoredRuns(atTheElementBound(t, "", "***a*** ", "\n"), author)
		}},
		{"the whole-upload fixture's text", func(t *testing.T) *pmdoc.Node { return wholeUploadTree(0) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			tree := row.tree(t)
			adds, err := wholeTextWriteAdds(tree)
			if err != nil {
				t.Fatal(err)
			}
			floor := wholeTextWriteFloor(tree)
			t.Logf("a whole-text write of %d top-level blocks counts %d structs (ceiling %d); the read's floors count %d and %d", len(tree.Children), adds, wholeTextWriteCeiling, floor, len(tree.Children))
			if adds >= wholeTextWriteCeiling {
				t.Fatalf("a whole-text write counts %d structs, want under the %d ceiling", adds, wholeTextWriteCeiling)
			}
			if floor > adds || uint64(len(tree.Children)) > floor {
				t.Fatalf("the read's floors count %d and %d structs, over the %d the measure counts", floor, len(tree.Children), adds)
			}
		})
	}
}

// wholeUploadAgent is the agent whose uploads the fixture's are.
var wholeUploadAgent = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

// A document an agent rewrote whole until the decode cap refused its next upload loads, and a
// small write still lands on it, but the upload it must still take does not: before nearACap
// counted what such an upload adds, it answered this document far from the cap - its struct total
// and a write's smallest replacement, three more, are within the cap - and the rebuild route
// refused it, "this document loads; nothing to rebuild" (dispatch://INFRA-179, measured at
// 1,047,004 structs). Its rebuild now takes it, from its own text: the rebuilt document reads the
// same, holds no more structs than a fresh document of its text and its list of retired clients,
// and takes the upload that was refused.
func TestADocumentWholeUploadsCarriedPastTheCapIsNearAndItsRebuildTakesTheUpload(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	band, _ := wholeUploadedStates(t)
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, band.state)
	ctx := context.Background()
	before, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	structs := countRoomStructs(decodedHistory(t, band.state))
	upload := wholeUploadText(band.rounds)
	if _, err := joinedReplaceText(service, artifactID, upload, wholeUploadAgent); !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), "items ygo decodes") {
		t.Fatalf("the agent's next upload to the %d-struct document was answered %v, want the decode cap's refusal", structs, err)
	}
	closeResidentRoom(t, service, artifactID)

	report, _, err := rebuildDocument(t, service, artifactID, nil)
	if err != nil {
		t.Fatalf("rebuild a %d-struct document its next upload was refused at: %v", structs, err)
	}
	loaded, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := countRoomStructs(decodedHistory(t, loaded.Update))
	t.Logf("%d structs in a %d-byte state rebuilt to %d in a %d-byte state (%s)", structs, len(band.state), rebuilt, len(loaded.Update), report.ValidationError)
	after, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("the rebuilt document reads %d bytes, want the %d it read before", len(after), len(before))
	}
	fresh, retired := freshStructs(t, loaded.Update), uint64(len(historyClients(decodedHistory(t, band.state))))
	if rebuilt > fresh+retired {
		t.Fatalf("the rebuilt document holds %d structs, want no more than the %d of a fresh document of its text and its %d retired clients", rebuilt, fresh, retired)
	}
	if _, err := joinedReplaceText(service, artifactID, upload, wholeUploadAgent); err != nil {
		t.Fatalf("the refused upload after the rebuild: %v", err)
	}
}

// The same document more than an upload short of the band is far from the cap: its next upload
// lands, so a write can still change it, and its rebuild is refused with or without supplied
// markdown, as every history that loads far from both caps is.
func TestADocumentWholeUploadsLeaveFarFromTheCapIsRefusedItsRebuild(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	_, far := wholeUploadedStates(t)
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, far.state)
	structs := countRoomStructs(decodedHistory(t, far.state))
	markdown := wholeUploadText(far.rounds)
	for _, supplied := range []*string{nil, &markdown} {
		if _, _, err := rebuildDocument(t, service, artifactID, supplied); !errors.Is(err, ErrDocumentLoads) {
			t.Fatalf("rebuild a %d-struct document with markdown %t: %v, want ErrDocumentLoads", structs, supplied != nil, err)
		}
	}
	if _, err := joinedReplaceText(service, artifactID, markdown, wholeUploadAgent); err != nil {
		t.Fatalf("the next upload to the %d-struct document: %v", structs, err)
	}
}

// The decode cap's refusal of a write names the rebuild exactly where the rebuild takes the
// document: the agent's upload to the document in the band is refused naming it and the route
// with the document's id, and a write that would carry the same document's far state past the cap
// - thousands of headings appended, more than a whole-text write of it adds - is refused telling
// its writer to shorten the change and naming no rebuild, which the route would refuse.
func TestTheDecodeCapsRefusalNamesTheRebuildOnlyWhereTheRebuildTakesTheDocument(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	band, far := wholeUploadedStates(t)
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, band.state)
	_, err := joinedReplaceText(service, artifactID, wholeUploadText(band.rounds), wholeUploadAgent)
	if !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), "items ygo decodes") {
		t.Fatalf("the agent's next upload was answered %v, want the decode cap's refusal", err)
	}
	if route := "POST /api/v1/artifacts/" + artifactID + "/rebuild"; !strings.Contains(err.Error(), rebuildRemedy) || !strings.Contains(err.Error(), route) {
		t.Fatalf("the refusal of the agent's upload reads %q, want it to name the rebuild (%q) and its route (%q)", err, rebuildRemedy, route)
	}

	service, artifactID = newTestService(t)
	storeHistory(t, service, artifactID, far.state)
	before, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatal(err)
	}
	grown := before + strings.Repeat("# h\n\n", headingsWithin(before))
	_, err = joinedReplaceText(service, artifactID, grown, wholeUploadAgent)
	if !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), "items ygo decodes") {
		t.Fatalf("a write appending headings to the far document was answered %v, want the decode cap's refusal", err)
	}
	if strings.Contains(err.Error(), "rebuild") || !strings.Contains(err.Error(), "shorten the change") {
		t.Fatalf("the refusal of a write into a document the rebuild refuses reads %q, want it to say to shorten the change and name no rebuild", err)
	}
}

// headingsWithin is the most one-letter headings markdown can take after it within the upload's
// element bound.
func headingsWithin(markdown string) int {
	low, high := 0, pmdoc.MaxDocumentElements
	for low < high {
		middle := (low + high + 1) / 2
		if size := pmdoc.MeasureDocument(markdown + strings.Repeat("# h\n\n", middle)); size.Counted && size.Elements <= pmdoc.MaxDocumentElements && size.Bytes <= pmdoc.MaxDocumentBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

// A document in the band reads as near the decode cap with no write refused, so the page can offer
// the rebuild to a person whose own typing still lands: from its stored state, which the cold read
// decodes and judges once, and from its room once one is resident. The far state and an ordinary
// document read as not near either way, and every near answer is one the rebuild route takes:
// the same test (nearTheDecodeCap), and here the route's own check of the same history.
func TestADocumentNearTheDecodeCapReadsAsNearWithNoWriteRefused(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	band, far := wholeUploadedStates(t)
	ctx := context.Background()
	for _, row := range []struct {
		name  string
		state func(t *testing.T, service *Service, artifactID string)
		near  bool
	}{
		{"a document whose next whole upload the cap refuses", func(t *testing.T, service *Service, artifactID string) {
			storeHistory(t, service, artifactID, band.state)
		}, true},
		{"the same document more than an upload short of that", func(t *testing.T, service *Service, artifactID string) {
			storeHistory(t, service, artifactID, far.state)
		}, false},
		{"an ordinary document", func(t *testing.T, service *Service, artifactID string) {
			seedServiceText(t, service, artifactID, "# Ordinary\n\nA document far from every cap.\n")
		}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			row.state(t, service, artifactID)
			closeResidentRoom(t, service, artifactID)
			why, near, err := service.NearDecodeCap(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("from the stored state: near=%t %q", near, why)
			if near != row.near || (why != "") != row.near {
				t.Fatalf("the stored state reads near=%t (%q), want near=%t", near, why, row.near)
			}
			if err := loadRoomOf(service, artifactID); err != nil {
				t.Fatal(err)
			}
			if service.srv.GetDoc(artifactID) == nil {
				t.Fatal("the room did not load")
			}
			live := service.srv.GetDoc(artifactID)
			resident, residentNear, err := service.NearDecodeCap(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("from the resident room: near=%t %q", residentNear, resident)
			if residentNear != row.near {
				t.Fatalf("the resident room reads near=%t (%q), want near=%t", residentNear, resident, row.near)
			}
			// The route judges the resident room, and the stored history once the room is gone
			// (RebuildDocument, rebuildSource): both answer as the read did.
			if _, routeNear, err := nearACap(ctx, live, service.copyOfRoom(live), int64(service.roomStateBytes(live))); err != nil || routeNear != row.near {
				t.Fatalf("the rebuild route reads the resident room near=%t (%v), the read near=%t", routeNear, err, row.near)
			}
			loaded, err := service.persistence.Load(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			if _, routeNear, err := nearACap(ctx, decodedHistory(t, loaded.Update), copyWeight(int64(len(loaded.Update)), 0), int64(len(loaded.Update))); err != nil || routeNear != row.near {
				t.Fatalf("the rebuild route reads the stored history near=%t (%v), the read near=%t", routeNear, err, row.near)
			}
		})
	}
}

// The near-decode-cap test hook leaves a document genuinely near the decode cap, for the reason
// the agent's uploads met: its struct total leaves a write's smallest replacement room, so a
// keystroke still fits, and the rebuild route's own check answers it near for a write of its
// whole text; a person's write adding a paragraph is refused naming the rebuild, the page's read
// answers near with no write refused, and the route rebuilds it to a small document that reads
// the same and takes the refused write.
func TestTheNearDecodeCapHookLeavesADocumentTheRebuildTakes(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	ctx := context.Background()
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# Near the cap\n\nThe first paragraph.\n\nThe second paragraph.\n\nThe third paragraph.\n")
	before, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := service.NearDecodeCapForTest(ctx, artifactID); err != nil {
		t.Fatal(err)
	}
	t.Logf("the hook took %s", time.Since(began))
	loaded, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	history := decodedHistory(t, loaded.Update)
	if structs := structCeiling(history); structs+smallestReplacementAdds > maxUpdateItems {
		t.Fatalf("the hooked history holds %d structs, past a write's smallest replacement: a keystroke would be refused too", structs)
	}
	why, near, err := nearACap(ctx, history, copyWeight(int64(len(loaded.Update)), 0), int64(len(loaded.Update)))
	if err != nil || !near || !strings.Contains(why, "a write of its whole text") {
		t.Fatalf("the rebuild route reads the hooked history near=%t (%q, %v), want near for a write of its whole text", near, why, err)
	}
	t.Logf("the route reads it near: %s", why)
	if after, err := service.Text(ctx, artifactID); err != nil || after != before {
		t.Fatalf("the hooked document reads %q (%v), want the text it read before", after, err)
	}
	if read, readNear, err := service.NearDecodeCap(ctx, artifactID); err != nil || !readNear {
		t.Fatalf("the page's read of the hooked document answers near=%t (%q, %v), want near", readNear, read, err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	grown := before + "\nA paragraph the person adds.\n"
	if _, err := joinedReplaceText(service, artifactID, grown, alice); !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), rebuildRemedy) {
		t.Fatalf("a person's write adding a paragraph was answered %v, want the decode cap's refusal naming the rebuild", err)
	}
	closeResidentRoom(t, service, artifactID)
	report, _, err := rebuildDocument(t, service, artifactID, nil)
	if err != nil {
		t.Fatalf("rebuild the hooked document: %v", err)
	}
	rebuilt, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if structs := countRoomStructs(decodedHistory(t, rebuilt.Update)); structs > 100 {
		t.Fatalf("the rebuilt document holds %d structs (%s), want a fresh document's handful", structs, report.ValidationError)
	}
	if after, err := service.Text(ctx, artifactID); err != nil || after != before {
		t.Fatalf("the rebuilt document reads %q (%v), want the text it held", after, err)
	}
	if _, err := joinedReplaceText(service, artifactID, grown, alice); err != nil {
		t.Fatalf("the refused write after the rebuild: %v", err)
	}
}

// A rebuild judges a document once. Where this server holds the document's room and the room
// stands near a cap, the room decides, and the rebuild closes it and runs; the decoded history is
// judged again only where no room decided. Before, the route judged the room near, closed it, and
// then judged the decoded history on its own: a room that a decode reads otherwise - a decode
// merges the runs of typing the room holds a struct a keystroke, and the room's tracked state runs
// at or above its encode between re-bases - was closed and answered DOCUMENT_LOADS, the
// offer-then-refuse the read's floors exist to prevent. Here the room is tracked within a browser's
// allowance of the state cap while its stored history is a few hundred bytes, and the first
// rebuild takes it.
func TestARebuildOfANearResidentRoomRunsThoughItsDecodedHistoryReadsFar(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	const markdown = "# Resident\n\nA room the hook refuses an ordinary edit in.\n"
	seedServiceText(t, service, artifactID, markdown)
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatal(err)
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the room did not load")
	}
	service.roomWeighingOf(live).applies().loaded(live, maxRoomStateBytes-1<<10)
	if why, near, err := nearACap(ctx, live, service.copyOfRoom(live), int64(service.roomStateBytes(live))); err != nil || !near {
		t.Fatalf("the resident room reads near=%t (%q, %v), want near the state cap", near, why, err)
	}
	loaded, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if why, near, err := nearACap(ctx, decodedHistory(t, loaded.Update), copyWeight(int64(len(loaded.Update)), 0), int64(len(loaded.Update))); err != nil || near {
		t.Fatalf("the decoded history reads near=%t (%q, %v), want far", near, why, err)
	}
	report, _, err := rebuildDocument(t, service, artifactID, nil)
	if err != nil {
		t.Fatalf("the first rebuild of a resident room near a cap: %v", err)
	}
	t.Logf("rebuilt: %s", report.ValidationError)
	if text, err := service.Text(ctx, artifactID); err != nil || text != markdown {
		t.Fatalf("the rebuilt document reads %q (%v), want %q", text, err, markdown)
	}
}
