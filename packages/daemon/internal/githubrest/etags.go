package githubrest

import (
	"container/list"
	"io"
	"sync"

	"github.com/sjawhar/envoy/oauth2call"
)

// ETags is what a client's conditional reads keep (Client.ETags): for each URL a GET answered 200
// with an ETag, under the client's Scope, that ETag, the answer as it was read
// (oauth2call.ReadAnswer, with the record of the request it answered) and the next page its Link
// header named, so the next GET of the same URL in the same scope asks GitHub with If-None-Match
// and reads a 304 as that answer. A client of another scope keeps answers of its own. GitHub
// counts a 304 against no rate limit ("Use conditional requests if appropriate", REST API best
// practices), so an unchanged pull request, rules list or branch costs a read nothing.
//
// It holds at most limit bytes of bodies and drops the URL read least recently to stay under it;
// a body larger than limit is not kept at all. A dropped or unkept URL is read whole the next time,
// which is what a read without ETags does. A 200 with no ETag drops what was kept for its URL. It is
// safe for concurrent use: the daemon's reads run on many goroutines.
type ETags struct {
	mu      sync.Mutex
	limit   int
	size    int
	entries map[string]*list.Element
	order   *list.List // front: read most recently; each element an *etagEntry
}

// etagEntry is one URL's kept answer, size the bytes of its body.
type etagEntry struct {
	url    string
	etag   string
	answer oauth2call.Answer
	size   int
	next   string
}

// NewETags is an empty ETags holding at most limit bytes of answer bodies.
func NewETags(limit int) *ETags {
	return &ETags{limit: limit, entries: map[string]*list.Element{}, order: list.New()}
}

// lookup is the answer kept for url, and whether one is; a found entry becomes the one read most
// recently.
func (e *ETags) lookup(url string) (etagEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	element, ok := e.entries[url]
	if !ok {
		return etagEntry{}, false
	}
	e.order.MoveToFront(element)
	return *element.Value.(*etagEntry), true
}

// keep records GitHub's 200 for url: its ETag, the answer read and its next page. An empty etag,
// or a body over the limit, keeps nothing and drops what was kept for url.
func (e *ETags) keep(url, etag string, answer oauth2call.Answer, next string) {
	// The body's size is counted by streaming its bytes to io.Discard: a count, never a print.
	size, _ := answer.StreamTo(io.Discard)
	e.mu.Lock()
	defer e.mu.Unlock()
	if element, ok := e.entries[url]; ok {
		e.remove(element)
	}
	if etag == "" || size > int64(e.limit) {
		return
	}
	entry := &etagEntry{url: url, etag: etag, answer: answer, size: int(size), next: next}
	e.entries[url] = e.order.PushFront(entry)
	e.size += entry.size
	for e.size > e.limit {
		e.remove(e.order.Back())
	}
}

// remove drops element, under e.mu.
func (e *ETags) remove(element *list.Element) {
	entry := element.Value.(*etagEntry)
	e.order.Remove(element)
	delete(e.entries, entry.url)
	e.size -= entry.size
}
