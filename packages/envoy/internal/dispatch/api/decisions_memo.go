package api

import (
	"container/list"
	"sync"
)

// documentCutsBytes bounds what the cuts this process keeps hold (documentCuts): 32 MiB. A cut's
// texts and headings are slices of its markdown, so an entry holds the whole markdown however few
// decisions it cuts, and the bound is on bytes rather than on entries: it holds the cuts of more
// than 600 markdowns of 25 to 43 KB each, at the overhead of 40 decisions apiece
// (documentCutOverheadBytes). It is half the bound the display trees (pmdoc.DisplayCacheBytes) and
// the list answers (listAnswerCacheBytes) of the same process are held to: a bound a server
// carries beside its pools, not a target to fill.
const documentCutsBytes = 32 << 20

// documentCutOverheadBytes is what an entry costs beside its markdown, charged once for the entry
// and once for each decision it cuts: the list's element, the map's slot, the key and the entry
// itself, and for each decision its key, its cut, the 64 bytes of its sum and its slot in the
// cut's map. It is a round number above what those come to on a 64-bit build.
const documentCutOverheadBytes = 256

// documentCuts is the cut of each markdown of a decisions document a view has read: each stored
// version (scanStoredDecisions), and the document as it stood when a view last cut it
// (readDecisions), each by its key (documentCutKey), the parser's refusal included, so a markdown
// is parsed at most once while its cut stays here, however many views read it. The bulk of an
// entry is the bytes of its markdown (documentCutsBytes), so the memo holds the most recently used
// cuts within that bound and drops the least recently used first. An entry is served only for the
// markdown it was cut from (get), so a cut never goes stale.
var documentCuts = newDocumentCutMemo(documentCutsBytes)

// documentCutKey names the markdown of one document a cut was made from: the document as it
// stands where live is set, and otherwise its stored version of that number. The document as it
// stands is no stored version, so its key names none: its number is 0 and is never read.
type documentCutKey struct {
	artifactID string
	live       bool
	number     int
}

// documentCutEntry is one kept cut with the key that finds it, so eviction can drop the map's
// entry from the list's own element, and what it was charged, so eviction gives back exactly that.
type documentCutEntry struct {
	key  documentCutKey
	cut  documentCut
	size int64
}

// documentCutMemo is the cuts kept by their key, the least recently used first out, charged at
// most the limit it was made with. Every method takes its lock, so one memo serves every view of a
// process. A kept cut is shared by every view that reads it, and none writes to it. It is
// bytelru's list and map over a value of their own: bytelru keeps byte slices alone, and a cut is
// a map of slices into its markdown.
type documentCutMemo struct {
	limit int64

	mu    sync.Mutex
	held  int64
	items map[documentCutKey]*list.Element
	order *list.List // of *documentCutEntry, the most recently used at the front
}

func newDocumentCutMemo(limit int64) *documentCutMemo {
	return &documentCutMemo{limit: limit, items: map[documentCutKey]*list.Element{}, order: list.New()}
}

// cut is key's markdown cut into its decisions: the cut kept for key when it was cut from that
// markdown, else decisionCuts' parse of it, which is then kept. The parse runs outside the lock,
// so two views that miss one markdown at once both parse it, and the second cut kept replaces the
// first.
func (m *documentCutMemo) cut(key documentCutKey, markdown string) documentCut {
	if cut, held := m.get(key, markdown); held {
		return cut
	}
	cut := documentCut{markdown: markdown}
	cut.cuts, cut.read = decisionCuts(markdown)
	m.put(key, cut)
	return cut
}

// get is the cut kept for key when it was cut from markdown, marked the most recently used.
func (m *documentCutMemo) get(key documentCutKey, markdown string) (documentCut, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	element, held := m.items[key]
	if !held {
		return documentCut{}, false
	}
	entry := element.Value.(*documentCutEntry)
	if entry.cut.markdown != markdown {
		return documentCut{}, false
	}
	m.order.MoveToFront(element)
	return entry.cut, true
}

// put keeps cut under key in place of any cut kept there, and drops the least recently used
// entries until what is charged fits the limit. An entry charged more than the whole limit is
// never kept, so one enormous markdown cannot empty the memo for every other.
func (m *documentCutMemo) put(key documentCutKey, cut documentCut) {
	size := int64(len(cut.markdown)) + int64(1+len(cut.cuts))*documentCutOverheadBytes
	if size > m.limit {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if element, held := m.items[key]; held {
		m.remove(element)
	}
	m.items[key] = m.order.PushFront(&documentCutEntry{key: key, cut: cut, size: size})
	m.held += size
	for m.held > m.limit {
		m.remove(m.order.Back())
	}
}

// remove drops element's entry, and what it was charged. Its caller holds the lock.
func (m *documentCutMemo) remove(element *list.Element) {
	entry := m.order.Remove(element).(*documentCutEntry)
	delete(m.items, entry.key)
	m.held -= entry.size
}
