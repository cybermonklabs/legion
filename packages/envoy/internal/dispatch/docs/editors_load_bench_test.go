package docs

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// BenchmarkFiveEditorsTyping measures what a room of a mebibyte takes from five editors typing at
// once: each types one character every editorKeystrokeEvery into a text of its own, handed to the
// peer update hook as its browser's V1 update and applied by the room's own apply, with the room's
// update observer draining each commit into the replica inside that apply, as a room's does
// (observeRoomUpdates runs from the room's OnUpdate). From this change the room's apply check
// renders each update under the document's lock (renderCheck) and the observer takes that
// rendering; before it, the observer rendered after the commit. Either way every keystroke pays
// one render, and what this reads is where it lands: applies that wait for the document's lock
// behind another editor's render.
//
// Two documents: one long bullet list, whose every keystroke touches the one top-level block and
// so re-renders the whole list, the shape whose whole render costs the most; and many blocks,
// sections of headings, paragraphs, lists and code, where a keystroke re-renders its own block.
// Each run types for editorDriveFor and reports the applies completed a second, and each apply's
// time - from the hook's call to its return, which is what the editor's own read loop waits - at
// p50, p99 and the most.
func BenchmarkFiveEditorsTyping(b *testing.B) {
	for _, shape := range []struct {
		name     string
		markdown func() string
	}{
		{"one-long-list", oneLongListMarkdown},
		{"many-blocks", manyBlocksMarkdown},
	} {
		b.Run(shape.name, func(b *testing.B) {
			live := newDocumentCopy()
			tree, err := pmdoc.Parse(shape.markdown())
			if err != nil {
				b.Fatal(err)
			}
			update, err := encodeDocumentTree(tree)
			if err != nil {
				b.Fatal(err)
			}
			if err := crdt.ApplyUpdateV1(live, update, nil); err != nil {
				b.Fatal(err)
			}
			service := calibratedService()
			service.budget.limit = roomBudgetBytes
			replica := observedReplica(b, service, live, liveRendering(b, live))
			service.chargeLoadedRoom("bench", replica, int64(len(crdt.EncodeStateAsUpdateV1(live, nil))))
			live.OnUpdate(func([]byte, any) { replica.observe("bench") })
			if size, measured := replica.size(); !measured || size < pmdoc.MaxDocumentBytes-64<<10 {
				b.Fatalf("the room published %d bytes, want about a mebibyte", size)
			}
			for b.Loop() {
				applied, took := typeFiveEditors(b, service, live)
				slices.Sort(took)
				rank := func(percentile int) float64 {
					return float64(took[min(len(took)*percentile/100, len(took)-1)]) / float64(time.Millisecond)
				}
				b.ReportMetric(float64(applied)/editorDriveFor.Seconds(), "applies/s")
				b.ReportMetric(rank(50), "p50-ms")
				b.ReportMetric(rank(99), "p99-ms")
				b.ReportMetric(rank(100), "max-ms")
			}
			b.Logf("%s: render paths %+v", shape.name, readRenderPaths(service))
		})
	}
}

// editorKeystrokeEvery is how often each of the five editors types: five characters a second
// each, twenty-five a second into the room, a steady typist's rate.
const editorKeystrokeEvery = 200 * time.Millisecond

// editorDriveFor is how long one run of BenchmarkFiveEditorsTyping types for.
const editorDriveFor = 20 * time.Second

// typeFiveEditors runs the five editors against live for editorDriveFor and returns how many of
// their updates the room applied and how long each took.
func typeFiveEditors(b *testing.B, service *Service, live *crdt.Doc) (applied int, took []time.Duration) {
	var mu sync.Mutex
	var editors sync.WaitGroup
	deadline := time.Now().Add(editorDriveFor)
	for editor := range 5 {
		browser, err := snapshotDocument(live)
		if err != nil {
			b.Fatal(err)
		}
		texts := paragraphTexts(browser)
		if len(texts) < 5 {
			texts = allTexts(browser.GetXmlFragment(fragmentName))
		}
		into := texts[editor*len(texts)/5]
		var sent []byte
		browser.OnUpdate(func(update []byte, _ any) { sent = update })
		origin := new(int)
		editors.Go(func() {
			for tick := time.Now(); tick.Before(deadline); tick = tick.Add(editorKeystrokeEvery) {
				time.Sleep(time.Until(tick))
				browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 0, "y", nil) })
				info := websocket.PeerUpdateInfo{Room: "bench", Doc: live, Update: sent, Origin: origin}
				start := time.Now()
				err := service.weighPeerUpdate(info, func() error { return crdt.ApplyUpdateV1(live, info.Update, origin) })
				elapsed := time.Since(start)
				mu.Lock()
				took = append(took, elapsed)
				if err == nil {
					applied++
				}
				mu.Unlock()
				if err != nil {
					b.Errorf("the room refused editor %d's keystroke: %v", editor, err)
					return
				}
			}
		})
	}
	editors.Wait()
	return applied, took
}

// oneLongListMarkdown is a mebibyte of one bullet list, each item a sentence of words.
func oneLongListMarkdown() string {
	words := strings.TrimSpace(strings.Repeat("word ", 60))
	var list strings.Builder
	for item := 0; list.Len() < pmdoc.MaxDocumentBytes-8<<10; item++ {
		fmt.Fprintf(&list, "- Item %d %s\n", item, words)
	}
	return list.String()
}

// manyBlocksMarkdown is a mebibyte of sections, each a heading, a paragraph with marks, a list
// and a code block. Each paragraph runs on for 600 words, so the mebibyte stays inside the
// elements a document may hold (pmdoc.MaxDocumentElements), as benchmarkLiveDocument's does.
func manyBlocksMarkdown() string {
	words := strings.TrimSpace(strings.Repeat("word ", 600))
	var blocks strings.Builder
	for section := 0; blocks.Len() < pmdoc.MaxDocumentBytes-8<<10; section++ {
		fmt.Fprintf(&blocks, "## Section %d\n\nParagraph %d holds **bold text**, `inline code` and *emphasis*. %s\n\n- the first item\n- the second item\n\n```go\nfunc example() int {\n\treturn 1\n}\n```\n\n", section, section, words)
	}
	return blocks.String()
}
