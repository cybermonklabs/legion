// Package docwork names each entry point of document work - a document's parse, its render, its
// read out of and its write into a Yjs fragment, the walks that find its anchors and the extraction
// of the references a text cites - so the xid window rigs see where that work runs. INFRA-636
// promises that a write holds its transaction id only for its database statements: document work
// done after a transaction's first write and before its end holds that id, and every reader's
// snapshot horizon with it, for as long as the Go code takes.
//
// Each entry point calls Enter with the work it does. A rig installs an observer (Observe), which
// storetest.XidTracer reads as it reads a collaborator call: work entered on a goroutine whose
// transaction has written and not yet ended fails the test, naming the work and the stack that did
// it. The entry points sit in pmdoc (parse, render, read, encode, the anchor walks) and in text
// (reference extraction), below every package that does such work, docs, refs and api among them,
// so each of their rigs installs the one observer. No observer is installed in production, where
// Enter is one atomic load.
package docwork

import (
	"slices"
	"sync"
	"sync/atomic"
)

// The work each entry point names.
const (
	// Parse is a read of markdown into a document tree (pmdoc's readContext and readInline).
	Parse = "parse"
	// Render is a write of a document tree as markdown (pmdoc's renderDocument, Layout.Splice).
	Render = "render"
	// Read is a read of a Yjs fragment into a document tree (pmdoc's readDocument).
	Read = "read"
	// Encode is a write of a document tree into a Yjs fragment (pmdoc's writeDocument).
	Encode = "encode"
	// AnchorWalk is a walk of a document tree for an anchor's mark or quote (pmdoc's FindMark,
	// FindMarks, FindQuote and QuoteFinder).
	AnchorWalk = "anchor walk"
	// References is an extraction of the references a text cites (text's Mentions and ExtractAt).
	References = "reference extraction"
)

var (
	// observers is the list Enter calls, nil while none is installed; installing replaces it with a
	// new slice under mu, so Enter reads it without a lock.
	observers atomic.Pointer[[]*func(work string)]
	mu        sync.Mutex
)

// Enter reports that the caller starts document work of the kind work names to every installed
// observer, on the caller's goroutine. With none installed it does nothing else.
func Enter(work string) {
	list := observers.Load()
	if list == nil {
		return
	}
	for _, observe := range *list {
		(*observe)(work)
	}
}

// Observe installs observe, which Enter calls on the goroutine that enters the work, until the
// returned stop runs. Several observers may be installed at once - each rig's tracer installs its
// own - and each sees every entry.
func Observe(observe func(work string)) (stop func()) {
	entry := &observe
	mu.Lock()
	defer mu.Unlock()
	var next []*func(string)
	if list := observers.Load(); list != nil {
		next = slices.Clone(*list)
	}
	next = append(next, entry)
	observers.Store(&next)
	return sync.OnceFunc(func() {
		mu.Lock()
		defer mu.Unlock()
		list := observers.Load()
		if list == nil {
			return
		}
		rest := slices.DeleteFunc(slices.Clone(*list), func(installed *func(string)) bool { return installed == entry })
		if len(rest) == 0 {
			observers.Store(nil)
			return
		}
		observers.Store(&rest)
	})
}
