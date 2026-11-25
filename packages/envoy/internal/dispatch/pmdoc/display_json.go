package pmdoc

import (
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
)

// DisplayCacheBytes is how much memory the display trees one process keeps may hold. Each tree is
// charged what keeping it costs the heap - its whole array, its key and the cache's entry for it
// (displayCacheEntrySize) - so a full cache holds about this much and no more. The markdown the
// trees were read from is not counted. 64 MiB holds the stored text a busy dashboard reads over
// and over - an issue's comment list re-read on every event, an inbox's asks - and is a bound a
// server can carry beside its pools, not a target to fill.
const DisplayCacheBytes = 64 << 20

// DisplayTreeLongestKept is the longest tree the display cache keeps at all: a tree whose array
// is this long is charged the whole of DisplayCacheBytes (displayCacheEntrySize), and a longer
// one is never kept.
const DisplayTreeLongestKept = DisplayCacheBytes - int(unsafe.Sizeof(displayCacheKey{})) - displayCacheEntryBytes

// TextSum names one stored text: its SHA-256, which is what the display cache keys a reading of
// that text by (displayCacheKey) and what the stored trees are keyed by too (texttrees). A
// caller that holds the sum asks this package for a reading without the text and without hashing
// it a second time (DisplayJSONOf, HeldDisplayTree, PutStoredDisplayTree).
func TextSum(text string) [32]byte { return sha256.Sum256([]byte(text)) }

// DisplayJSON is the ProseMirror tree markdown is shown as (ReadForDisplay), encoded as the JSON
// a response carries beside the stored text, or nil for text with nothing in it. The read never
// refuses, so neither does this; an encoding that fails - which only an attribute no JSON holds
// could cause - is logged and answered with nil, since a reader shown no tree falls back to the
// markdown beside it.
//
// The answer is shared, never copied: a caller marshals it into a response and must not write to
// it.
func DisplayJSON(markdown string, mode DisplayMode) json.RawMessage {
	if markdown == "" {
		return nil
	}
	return DisplayJSONOf(TextSum(markdown), markdown, mode)
}

// DisplayJSONOf is DisplayJSON for a caller that already holds the text's sum: a read that
// hashed each of its texts once to ask the table for their stored trees (api's display tree
// reader) reads the misses through this, so the text is never hashed a second time.
//
// The sum must be TextSum of markdown. It is the key this reading is both looked up and kept
// under, so a sum of another text would answer that text's tree.
func DisplayJSONOf(sum [32]byte, markdown string, mode DisplayMode) json.RawMessage {
	if markdown == "" {
		return nil
	}
	key := displayCacheKey{sum: sum, mode: mode}
	if tree, held := displayCache.Get(key); held {
		return tree
	}
	displayTreeBuilds.Add(1)
	tree := encodeDisplayTree(markdown, mode)
	if tree == nil {
		return nil
	}
	keepDisplayTree(key, tree)
	return tree
}

// DisplayTreeToStore is the tree DisplayJSON answers, for a caller that is storing it rather
// than serving it (texttrees). It answers the cache's bytes where the cache holds that reading
// already, and reads the markdown where it does not; it never puts what it read into the cache.
//
// Reading the cache and filling it are not the same thing. A write is no reader - the cache
// holds the trees readers were served, and filling it with the tree of every text written would
// make one writer's long document the eviction of the bodies a hundred readers are re-reading,
// which is what TestARoomFedOnlyByAgentWritesKeepsMemoryWithinTheBound measures - but taking a
// tree the cache already holds evicts nothing and holds nothing new.
//
// It counts no build either (DisplayTreeBuilds), on a hit or on a miss, since no response was
// read to make it.
func DisplayTreeToStore(markdown string, mode DisplayMode) json.RawMessage {
	if markdown == "" {
		return nil
	}
	if tree, held := displayCache.Get(displayCacheKey{sum: TextSum(markdown), mode: mode}); held {
		return tree
	}
	return encodeDisplayTree(markdown, mode)
}

// encodeDisplayTree reads markdown for display in mode and encodes that reading, or answers nil
// where the encoding failed - which only an attribute no JSON holds could cause, and which is
// logged and costs a reader the markdown beside the tree and never the answer.
func encodeDisplayTree(markdown string, mode DisplayMode) json.RawMessage {
	tree, err := json.Marshal(ReadForDisplay(markdown, mode).toDisplayJSON())
	if err != nil {
		slog.Warn("dispatch: display tree encoding failed", "error", err)
		return nil
	}
	return tree
}

// HeldDisplayTree is the display tree this process already holds of the text whose SHA-256 is
// sum, read in mode, and whether it holds one at all. It is the key DisplayJSONOf looks that
// reading up under (displayCacheKey), which is the key the stored trees are keyed by too, so a
// caller holding the sum asks without the text and without hashing it again.
//
// A caller asks this to leave a text out of a read it would otherwise make - the query of the
// stored trees a request's display tree reader runs - so the answer is about this process alone
// and says nothing about the table. It marks the entry used, as the shaping that follows does.
func HeldDisplayTree(sum [32]byte, mode DisplayMode) (json.RawMessage, bool) {
	return displayCache.Get(displayCacheKey{sum: sum, mode: mode})
}

// PutStoredDisplayTree puts a tree this process did not read - the text_trees row of the text
// whose sum is sum, read in mode and read back by a reader that is about to answer it
// (texttrees) - into the display cache, so the DisplayJSON that follows answers these bytes and
// reads no markdown.
//
// The cache is the first of two levels: the trees this process has already served stand in front
// of the table, so a hot list - an issue's comments re-read on every event of it - costs neither
// a query nor a read, and only a text no reader of this process has seen yet costs the query.
//
// It counts no build (DisplayTreeBuilds): whichever process stored the row did the reading, and
// this one copied the bytes. That is what lets a test assert that a comment list read after a
// restart built nothing.
//
// The caller passes the sum it read the row under (TextSum), the same key it asked the table
// with, so a read of a list hashes each of its texts once rather than once a level. An empty
// text has no tree and no row, so no caller holds a sum to put for one, and a row holding no
// bytes is no tree either and is ignored: nothing a caller passes can shadow a reading with
// nothing.
//
// The tree is kept as it is handed over and charged its array's capacity (displayCacheEntrySize),
// so the caller hands over an array of its own grown by append, as texttrees.Read does.
func PutStoredDisplayTree(sum [32]byte, mode DisplayMode, tree json.RawMessage) {
	if len(tree) == 0 {
		return
	}
	keepDisplayTree(displayCacheKey{sum: sum, mode: mode}, tree)
}

// displayTreeBuilds counts what DisplayTreeBuilds answers.
var displayTreeBuilds atomic.Uint64

// DisplayTreeBuilds is how many display trees of a stored text this process has read since it
// started: one for every DisplayJSON the cache could not answer, which is every call that reads
// the markdown. A caller served a tree from elsewhere - a stored tree, once there is a table of
// them - calls DisplayJSON at all only on a miss, so a test that reads a comment list after a
// restart holds this number where it stood, which is the proof that the trees the response
// carried were stored rather than read.
//
// A read whose encoding then fails counts all the same: the number is the reading work a
// response cost, and that work was done whether or not a tree came out of it.
//
// A snippet is no build (DisplaySnippetJSON). Its tree is cut fresh for one query, stored by
// nobody and keyed by no version, so counting it would move a number whose only question is
// whether a stored text had to be read again.
func DisplayTreeBuilds() uint64 { return displayTreeBuilds.Load() }

// DisplayHighlightMark is the mark DisplaySnippetJSON puts over what a search matched. It is a
// mark of the display tree alone: no document stores it, no write accepts it, and it is not in
// markTypes, which is the schema of what Dispatch keeps.
const DisplayHighlightMark = "searchHighlight"

// DisplaySnippetJSON is the display tree of a search hit's snippet: markdown, the snippet already
// read out of its source and balanced where it is a cut (SearchSnippet), read for display as it
// stands, with DisplayHighlightMark over every range the sentinels start and stop named. It does
// not balance the markdown again: SearchSnippet decided where the snippet is a cut and where it is
// code, which no balancing of the snippet alone can see. The sentinels ride through the read
// rather than being taken out before it, because they are private-use characters, which the
// parser reads as ordinary word characters and leaves in the text nodes they fall in: a tree walk
// then has their exact places, where an offset into the markdown would have had to survive both
// the snippet's reading and the parse. The same reading leaves the ones the query matched in an
// address or a caption in the attribute they fall in, and the walk takes those out
// (highlightSnippet).
//
// The snippet is read fresh rather than from the cache: every query cuts its own headline, so a
// cached snippet would be a tree nothing asks for again, taking the room of a body many readers
// do share.
func DisplaySnippetJSON(markdown, start, stop string) json.RawMessage {
	if markdown == "" {
		return nil
	}
	tree := ReadForDisplay(markdown, DisplayLatest)
	highlightSnippet(tree, start, stop, false)
	encoded, err := json.Marshal(tree.toDisplayJSON())
	if err != nil {
		slog.Warn("dispatch: search snippet tree encoding failed", "error", err)
		return nil
	}
	return encoded
}

// highlightSnippet rewrites node's text children into the runs the sentinels cut them into, the
// runs between a start and a stop carrying DisplayHighlightMark, and answers whether the walk is
// inside a highlight when it leaves. The state carries across nodes in document order, since a
// headline may start a highlight in one text node and stop it in the next.
//
// Only text carries a highlight, so the walk takes the sentinels out of every attribute of every
// node it passes and of every mark on it (withoutSentinels), and those leave the state where it
// was. ts_headline marks the words of an address as it marks any other words: a document stores
// the autolink `<dispatch://CORE-1>` as `[dispatch://CORE-1](dispatch://CORE-1)` (text's
// referenceEnd), and a search for CORE answers `<b>CORE</b>-1](dispatch://<b>CORE</b>-1)`, which
// SearchSnippet widens back to the whole link (measured on PostgreSQL 18 with headlineOptions).
// Left in the href, the sentinels make an address the browser cannot read as a
// reference, so it takes the link away and shows the raw address; left in an image's src, the
// picture names no file. An attribute holds no run a mark can sit on, so a sentinel there has
// nothing to mark: a caption drawn from an image's alt is drawn plain. ts_headline marks one word
// at a time, so a start and its stop fall in the same attribute, and toggling the state on them
// would only have marked the words after the link, or left the ones the query matched plain.
func highlightSnippet(node *Node, start, stop string, inside bool) bool {
	if node == nil {
		return inside
	}
	node.Attrs, _ = withoutSentinels(node.Attrs, start, stop)
	node.Marks = marksWithoutSentinels(node.Marks, start, stop)
	if node.Type == "text" {
		return inside
	}
	var rewritten []*Node
	for _, child := range node.Children {
		if child == nil {
			continue
		}
		if child.Type != "text" {
			inside = highlightSnippet(child, start, stop, inside)
			rewritten = append(rewritten, child)
			continue
		}
		child.Attrs, _ = withoutSentinels(child.Attrs, start, stop)
		child.Marks = marksWithoutSentinels(child.Marks, start, stop)
		rewritten, inside = appendHighlightRuns(rewritten, child, start, stop, inside)
	}
	node.Children = rewritten
	return inside
}

// withoutSentinels is attrs with start and stop taken out of every string value and every string
// of a list value, which are the shapes of every attribute the schema carries that text can
// reach: a link's href and title, an image's src, alt and title, a code block's language, an
// inline HTML node's value, a footnote's label, and a typed block's string and list attributes.
// It answers whether it took anything out. The answer is a map of its own where it did and attrs
// itself where it did not, since one link's attributes are shared by every text node the link
// spans, so attrs is never written to.
func withoutSentinels(attrs Attrs, start, stop string) (Attrs, bool) {
	var cleaned Attrs
	for name, value := range attrs {
		var stripped any
		switch typed := value.(type) {
		case string:
			if !holdsSentinel(typed, start, stop) {
				continue
			}
			stripped = stripSentinels(typed, start, stop)
		case []string:
			if !slices.ContainsFunc(typed, func(item string) bool { return holdsSentinel(item, start, stop) }) {
				continue
			}
			list := make([]string, len(typed))
			for index, item := range typed {
				list[index] = stripSentinels(item, start, stop)
			}
			stripped = list
		default:
			continue
		}
		if cleaned == nil {
			cleaned = maps.Clone(attrs)
		}
		cleaned[name] = stripped
	}
	if cleaned == nil {
		return attrs, false
	}
	return cleaned, true
}

// marksWithoutSentinels is marks with withoutSentinels applied to each mark's attributes, in a
// slice of its own where any mark changed and marks itself where none did: the runs a text node
// is cut into share the node's marks, so the slice is never written to.
func marksWithoutSentinels(marks []Mark, start, stop string) []Mark {
	var cleaned []Mark
	for index, mark := range marks {
		attrs, changed := withoutSentinels(mark.Attrs, start, stop)
		if !changed {
			continue
		}
		if cleaned == nil {
			cleaned = slices.Clone(marks)
		}
		cleaned[index].Attrs = attrs
	}
	if cleaned == nil {
		return marks
	}
	return cleaned
}

// holdsSentinel answers whether value holds start or stop.
func holdsSentinel(value, start, stop string) bool {
	return strings.Contains(value, start) || strings.Contains(value, stop)
}

// stripSentinels is value with every start and every stop taken out.
func stripSentinels(value, start, stop string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, start, ""), stop, "")
}

// appendHighlightRuns appends the runs text is cut into at its sentinels, each carrying the text
// node's own marks and, while the walk is inside a highlight, DisplayHighlightMark. The sentinels
// themselves are dropped, and a run with nothing in it is never appended, so a node whose whole
// text is one sentinel leaves nothing behind.
func appendHighlightRuns(into []*Node, text *Node, start, stop string, inside bool) ([]*Node, bool) {
	if !strings.Contains(text.Text, start) && !strings.Contains(text.Text, stop) {
		if inside {
			text.Marks = highlighted(text.Marks)
		}
		return append(into, text), inside
	}
	rest := text.Text
	for rest != "" {
		next, sentinel := len(rest), ""
		if at := strings.Index(rest, start); at >= 0 {
			next, sentinel = at, start
		}
		if at := strings.Index(rest, stop); at >= 0 && at < next {
			next, sentinel = at, stop
		}
		if next > 0 {
			run := &Node{Type: "text", Attrs: text.Attrs, Text: rest[:next], Marks: text.Marks}
			if inside {
				run.Marks = highlighted(text.Marks)
			}
			into = append(into, run)
		}
		if sentinel == "" {
			break
		}
		inside = sentinel == start
		rest = rest[next+len(sentinel):]
	}
	return into, inside
}

// highlighted is marks with DisplayHighlightMark beside them, in a slice of its own: the runs a
// text node is cut into share the node's marks, so none of them may be appended to in place.
func highlighted(marks []Mark) []Mark {
	out := make([]Mark, len(marks), len(marks)+1)
	copy(out, marks)
	return append(out, Mark{Type: DisplayHighlightMark})
}

// displayCacheKey finds one reading of one text: the SHA-256 of the markdown and the mode it was
// read in, since the two modes read the same text into different trees.
type displayCacheKey struct {
	sum  [sha256.Size]byte
	mode DisplayMode
}

// displayCache is the process's one display-tree cache, held for the process's life: a tree is
// the reading of the text its key hashes, so nothing in it ever goes stale, and an entry leaves
// only to make room for another (DisplayCacheBytes). Every tree is put through keepDisplayTree,
// which charges it what it holds, so the bound is on the heap the cache holds, which is what a
// reader of DisplayCacheBytes is promised.
var displayCache = bytelru.New[displayCacheKey, json.RawMessage](DisplayCacheBytes)

// displayCacheEntryBytes is what one entry costs beyond its tree's array and its key's own bytes:
// bytelru's entry (which holds the key again, the tree's slice header and the charge), its list
// element, and its slot in the cache's map with that slot's share of the map's empty slots.
//
// A copy of bytelru on go1.27.3 linux/amd64 was fed SHA-256 keys and trees within 200 bytes of one
// length, each length from 1,000 to 8,000 bytes in steps of 200, until it had taken 60 times what
// it holds. Each reading measured the live heap of an entry beyond its tree, its key included.
// While bytelru never made its map afresh, the map kept the slots of evicted keys and split its
// tables for them: an entry held about 200 to 250 bytes when the cache was first full and up to
// 440 bytes after 60 turnovers, so at this charge 19 of the 36 lengths ended past the bound, by up
// to 3.77 MB (trees of 1,000 bytes); after 210 turnovers an entry held up to 525 bytes. Now that
// bytelru makes its map afresh each time it has evicted half as many entries as it holds, the most
// an entry held at any reading was 287 bytes (trees of 5,200 bytes), every run ended 0.89 MB or
// more under the bound, the cache's heap moved by at most 0.06 MB over its last 20 turnovers, and
// runs of 200 turnovers held at most 284 bytes an entry. The charge, this and the key's 40 bytes,
// is 360 bytes, 73 above the worst of those.
const displayCacheEntryBytes = 320

// keepDisplayTree puts tree into the display cache under key, charged what keeping it costs
// (displayCacheEntrySize). Both ways a tree enters the cache, a tree this process read
// (DisplayJSONOf) and a tree it read back from the table (PutStoredDisplayTree), go through this.
func keepDisplayTree(key displayCacheKey, tree json.RawMessage) {
	displayCache.PutSize(key, tree, displayCacheEntrySize(tree))
}

// displayCacheEntrySize is what keeping tree costs this process: the tree's whole array, not
// only the part its JSON fills, the key's own bytes, and the entry itself
// (displayCacheEntryBytes). Both ways in hand over an array grown by append, whose capacity is
// the whole allocation the runtime rounded it up to and which shares no larger buffer: a tree
// this process read is json.Marshal's, and a tree read back from the table is the copy
// texttrees.Read appends from the row's bytes. An array made exactly a tree's length would hide
// that rounding, 46 bytes a tree on average for trees of comment length.
func displayCacheEntrySize(tree json.RawMessage) int64 {
	return int64(cap(tree)) + int64(unsafe.Sizeof(displayCacheKey{})) + displayCacheEntryBytes
}
