// flight.go holds the shared sync the callers of one architecture source join: what a flight
// hands every caller that joined it, the newest ended flight of each source while a call of it
// runs, the flight itself — the contexts it runs on and its errSuperseded retry chain — and each
// attempt the flight makes (syncOnce), with the record of its failure on the source row (failed).
// The Importer type, Sync and Close stay in importer.go: they are the type's own surface, beside
// the reads of a source. The count Close waits on is in sync_count.go, and the row locks and
// projection writes an attempt calls are in writes.go.

package architecture

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// syncFlight is one shared sync of one source: what the flight hands every
// caller that joined it, with the flight's error. Every write to it happens
// in the flight's goroutine before the result is sent, and before the flight
// records itself as its source's newest ended flight (endedFlights), and every
// read after that receive or that record's read, so the channel and the
// record's mutex order them: the flight itself needs no mutex, and -race is
// quiet.
type syncFlight struct {
	// startedAt is when the flight began, its very first act, so that even a
	// flight which does nothing answers a time no earlier than its callers'
	// and so ends their loop.
	startedAt time.Time
	// headAt is when the flight issued its branch-head lookup — the moment the
	// lookup went out, never the moment it answered, because the head may have
	// moved in between. A retry round stamps it again.
	headAt time.Time
	source model.ArchitectureSource
	// err is the error the flight ended with, which every caller it answers
	// takes beside its source.
	err error
}

// servedAt is how fresh the flight's answer is: when it asked GitHub for the
// head, or, for a flight that failed before asking, when it began.
func (f *syncFlight) servedAt() time.Time {
	if f.headAt.IsZero() {
		return f.startedAt
	}
	return f.headAt
}

// syncKey is the key Sync shares flights on, in syncs and in the record of
// ended flights: the source, by its project and its repository both.
func syncKey(project, repo string) string { return project + "\x00" + repo }

// endedFlights holds, for each source a call of Sync is running for, the
// newest flight of that source to have ended, its error with it, by the key
// Sync shares flights on. One flight of a key runs at a time (singleflight),
// and each records itself here before singleflight drops its key and sends its
// answer, so while a call runs, the newest flight of its source is either the
// one in flight or the one recorded here: a caller that looks for one finds
// it, however late it looks.
//
// A key's entry lives only while a call of that key runs: the first call to
// enter makes it, and the last to leave drops it, with what it recorded. So
// the record never holds more entries than there are calls running, whatever
// error a flight ended with and whatever key a caller names, a key that names
// no source among them. No caller loses by the drop: a flight is fresh only
// for a call made before its head lookup, and a call holds its key's entry
// open from before it stamps its call until it returns (Sync). A flight that
// ends once every call of its key has left is recorded nowhere. Its zero value
// is ready.
type endedFlights struct {
	mu    sync.Mutex
	byKey map[string]*keyFlights
}

// keyFlights is one key's entry: how many calls of it are running, and its
// newest ended flight, nil until one ends while a call runs.
type keyFlights struct {
	calls int
	ended *syncFlight
}

// enter counts one more running call of key, making its entry if it has none.
func (e *endedFlights) enter(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byKey == nil {
		e.byKey = map[string]*keyFlights{}
	}
	entry := e.byKey[key]
	if entry == nil {
		entry = &keyFlights{}
		e.byKey[key] = entry
	}
	entry.calls++
}

// leave counts one call of key fewer, and drops key's entry once none runs.
func (e *endedFlights) leave(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := e.byKey[key]
	entry.calls--
	if entry.calls == 0 {
		delete(e.byKey, key)
	}
}

// fresh is key's newest ended flight when its head lookup came no earlier
// than calledAt, and nil when there is none or it is older.
func (e *endedFlights) fresh(key string, calledAt time.Time) *syncFlight {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := e.byKey[key]
	if entry == nil || entry.ended == nil || entry.ended.servedAt().Before(calledAt) {
		return nil
	}
	return entry.ended
}

// record stores flight, which has ended, as key's newest ended flight while a
// call of key runs, and drops it when none does.
func (e *endedFlights) record(key string, flight *syncFlight) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if entry := e.byKey[key]; entry != nil {
		entry.ended = flight
	}
}

// flight is the shared sync singleflight runs for one source: one set of
// GitHub calls and one errSuperseded retry chain, however many callers joined
// it. ctx is whichever caller started it, and only its values are kept.
func (i *Importer) flight(ctx context.Context, project, repo string) (*syncFlight, error) {
	flight := &syncFlight{startedAt: time.Now()}
	if !i.running.begin() {
		return flight, i.owner.Err()
	}
	defer i.running.done()
	// The starter is one of several callers and may leave before the others,
	// so its cancellation is dropped: one hang-up fails nobody else's sync.
	detached := context.WithoutCancel(ctx)
	// A sync opens its own transactions on the shared pool, so it is marked like an API
	// request: a read it makes while one is open must come from that transaction, never from a
	// second connection (store.ErrNestedAcquire). The mark has to be the flight's own, because
	// WithoutCancel keeps the starter's values: sharing an HTTP request's mark would make
	// SyncProject's next Sync a nested acquire, and WithTransactionTracking, a no-op when a
	// mark is already present, would hand that shared one back.
	marked, err := store.ForConcurrentRead(detached)
	if err != nil {
		return flight, err
	}
	base, abandon := context.WithCancel(marked)
	defer abandon()
	// Nothing but the process stopping ends a flight early.
	stop := context.AfterFunc(i.owner, abandon)
	defer stop()
	// The deadline is the flight's own, counted from when it began rather than
	// from when its first caller arrived.
	syncCtx, cancel := context.WithTimeout(base, syncTimeout)
	defer cancel()
	// Each try runs in a held scope (githubreads.Hold) of the attempt syncCtx carries when a
	// leased import started the flight, since syncCtx keeps the starter's values: a try another
	// write superseded is asked again, so only the last try that reached GitHub counts. syncOnce
	// notes on try alone: base, its parent, is only where it records a failure on the source's
	// row once try has ended.
	//
	// standing is the scope of the newest try that reached GitHub (githubreads.Held.Reached),
	// unsettled: a later try that reaches GitHub drops it, since only that try's outcome replaces
	// its notes, and the flight's end keeps it. A try that reached nothing holds nothing, and is
	// dropped at once: a retry can end before it asks GitHub anything, at its load on the
	// flight's spent deadline or on a database error, and a flight that ended so with the
	// superseded try's failure dropped would record a GitHub read that failed as one that
	// succeeded.
	var standing *githubreads.Held
	for {
		try, held := githubreads.Hold(syncCtx)
		source, err := i.syncOnce(try, base, project, repo, flight)
		if held.Reached() {
			standing.Drop()
			standing = held
		} else {
			held.Drop()
		}
		if errors.Is(err, errSuperseded) {
			continue
		}
		standing.Keep()
		flight.source = source
		return flight, err
	}
}

// syncOnce is one attempt of Sync, from the load of the source row to its write. It stamps the
// flight's head lookup, which is how fresh the answer every caller of the flight reads is.
func (i *Importer) syncOnce(ctx, parent context.Context, project, repo string, flight *syncFlight) (model.ArchitectureSource, error) {
	source, err := i.loadSourceByRepo(ctx, project, repo)
	if err != nil {
		return model.ArchitectureSource{}, err
	}
	owner, name, token, importErr := i.resolve(ctx, &source)
	if importErr != nil {
		// Each of the four steps of the sync that read GitHub -- resolve here, then Ref, Dir and
		// DirFiles -- gives up this source's read when it fails, which the running import's
		// attempt records (githubreads), whatever failed inside it. The client notes only the
		// answers no caller takes as data; a repository the App is not installed on, a missing
		// Contents permission, a branch GitHub does not have and a 4xx GitHub answers a read with
		// are answers it leaves to its caller, so the sync notes them here. ctx carries the values
		// of the caller that started the flight, so a sync a route or a command started notes
		// nothing. The content of the files (a note over notes.MaxFileSize, notes.Parse, the
		// projection) is no GitHub read: last_error records it, and the attempt does not.
		githubreads.NoteFailure(ctx, importErr)
		return i.failed(ctx, parent, source, importErr)
	}
	flight.headAt = time.Now()
	commit, importErr := i.github.Ref(ctx, token, owner, name, source.Branch)
	if importErr != nil {
		githubreads.NoteFailure(ctx, importErr)
		return i.failed(ctx, parent, source, importErr)
	}
	// Unchanged head of a healthy source: the projection already is this
	// commit. Refresh the freshness stamp only.
	if source.LastError == nil && source.LastCommit != nil && *source.LastCommit == commit {
		return i.touch(ctx, source, commit)
	}
	listing, importErr := i.github.Dir(ctx, token, owner, name, commit, SourceDir)
	if importErr != nil {
		githubreads.NoteFailure(ctx, importErr)
		return i.failed(ctx, parent, source, importErr)
	}
	// The head moved but the architecture directory's tree object is
	// identical: record the new commit without re-fetching or re-projecting.
	if source.LastError == nil && source.LastTreeSha != nil && *source.LastTreeSha == listing.SHA {
		return i.touch(ctx, source, commit)
	}
	files, importErr := i.github.DirFiles(ctx, token, owner, name, listing)
	if importErr != nil {
		// The client refuses a note over notes.MaxFileSize at two points, both
		// notes.ErrFileTooLarge (notes.CheckSize): by its listed size before any blob read, and by
		// a blob's decoded size after the read GitHub answered. Either is the source's content
		// and no failure of GitHub's, so the attempt does not note it.
		if !errors.Is(importErr, notes.ErrFileTooLarge) {
			githubreads.NoteFailure(ctx, importErr)
		}
		return i.failed(ctx, parent, source, importErr)
	}
	parsed, importErr := notes.Parse(files)
	if importErr != nil {
		return i.failed(ctx, parent, source, importErr)
	}
	updated, importErr := i.project(ctx, source, commit, listing.SHA, files, parsed)
	// A projection PostgreSQL refused, or one whose ids another source of the project holds, is
	// recorded like any other import failure. A source that moved or was deleted while the sync
	// ran has nothing to record it on, and one another sync recorded first is retried.
	if importErr != nil && !errors.Is(importErr, errSourceMoved) && !errors.Is(importErr, ErrNoSource) &&
		!errors.Is(importErr, errSuperseded) {
		return i.failed(ctx, parent, source, importErr)
	}
	return updated, importErr
}

// failed records importErr on the source row and hands both back: the Sync
// contract for every import failure. The sync's own deadline is always
// recorded, on parent — the flight's own context, which carries no sync
// deadline of its own and which the process stopping is the only thing to end
// — so that the timeout lands on the row like any other failure while Close
// can still cut a record write short, as it must: cmd/dispatch runs Close
// inside the wait the shutdown takes before the pool closes. Which caller is
// still listening does not come into it: a flight has no single caller, and
// nobody being there was never a reason to lose the fact that GitHub timed
// out.
func (i *Importer) failed(ctx, parent context.Context, source loadedSource, importErr error) (model.ArchitectureSource, error) {
	recordCtx := ctx
	if ctx.Err() != nil {
		recordCtx = parent
	}
	updated, err := i.recordFailure(recordCtx, source, importErr)
	if err != nil {
		return model.ArchitectureSource{}, err
	}
	return updated, importErr
}
