// legion_phase_work.go holds the issues that carry Legion phase work, the input the board's tree
// walk needs beside the daemon's own reports (dispatch://INFRA-1077, the spec's "The top issue of
// a tree"): an issue is a Legion issue when the daemon reports it or when it carries Legion phase
// work — a handoff, or an event of a session titled `Legion <role> · <that issue's key>`.
//
// The one read of that fact is legion.PhaseWorkKeys, which the metrics make too
// (legion/metrics/population.go's readPhaseWork), so both readers answer the rule from the same
// rows and no second rule states it here. A session's title is its newest, so phase work is not
// only gained: a session renamed to another issue takes the work of the issue it wrote on with
// it, and an event that carried no title of its own is read through the session's newest title
// all the same. Only that one read can say which issues carry it.
//
// The board is recomputed on every event burst of every open page, so this keeps the read's
// answer rather than making it again each time — but only while something reads the feed that
// marks it stale, which is one run of the live hub (a generation) and nothing else. That
// generation's intake marks the set stale from the feed, never edits it
// (delivery_live_hub_intake.go's applyEventLocked), and the set holds the generation's own done
// channel as its watcher, taken by the fill beside that feed's cursor (reseed, seedEvents). The
// last page to close stops the generation at once (deliveryHub.dropLocked), which closes that
// channel: with no feed read, no event can mark the set stale, so a set nothing watches is
// current for nothing and every board read makes the one read again (ensure) until the next
// generation seeds and watches it. Trusting what the last page left behind is how a board read
// between pages placed an issue in the tree it stood in when that page closed.
//
// Only an event that can move what the read answers marks it stale, since the read is a scan of
// every project's whole event log and sessions write events all the time: a recorded handoff,
// and an event of a session the rule's title half can reach (observe). That is why the read
// answers the titles of the sessions it found working an issue beside the keys
// (legion.PhaseWork): a session no read and no event since has found to be a Legion role cannot
// take phase work from the issue it writes on, and it is the commonest writer of all.
//
// The set holds the issues' bare keys, of every project: a board walks only the chain of its own
// issues, and nothing here is scoped to one.
package api

import (
	"context"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// legionPhaseWork is every issue key that carries Legion phase work. Its zero value is a usable
// empty set no read has filled, which is what a server a test builds directly holds.
type legionPhaseWork struct {
	// reading is held across the read alone, so two board reads at start-up make one read and
	// not two. mu guards the fields and is never held across a query; the live hub's intake
	// takes it while it holds the hub's own lock, so the order is the hub's lock and then this
	// one, and nothing here takes the hub's.
	reading sync.Mutex
	mu      sync.Mutex
	// dirtied counts the events that have marked the set stale: a read answers the rows as they
	// stood before it began, so one marked stale while it ran is stale still. Whether a read
	// ever answered the set is no field of its own: a read is what sets the watcher (reseed,
	// watch), so a set with an open feed is a set a read has filled.
	dirtied uint64
	// read is the value of dirtied the newest read started at.
	read uint64
	keys map[string]struct{}
	// roleSessions is the per-session index the narrow invalidation stands on: the newest title
	// of every session the newest read, or an event since, found to be a Legion role working an
	// issue (legion.PhaseWork.RoleSessions). A session it does not name had no such title then,
	// so no issue holds phase work by it and none can lose any when it writes again (observe).
	roleSessions map[string]string
	// watching is the feed that marks the set stale: the done channel of the hub generation
	// whose intake reads the event log, as reseed took it. nil is a process whose hub has run
	// no generation, and a closed channel one whose generation has stopped; under either the
	// set answers no board read (isCurrent), since nothing would mark it stale.
	watching <-chan struct{}
}

// has is whether the issue carries Legion phase work. A nil set answers false: a population built
// without one is a population of the daemon's reports alone.
func (w *legionPhaseWork) has(key string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, held := w.keys[key]
	return held
}

// invalidate marks the set stale: an event the one read may answer differently for was written,
// so the next board read makes that read again. A recorded handoff is one such event whatever
// wrote it; an event of a session is one only where observe says so.
func (w *legionPhaseWork) invalidate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirtied++
}

// observe takes one event a session wrote — the session's id, that session's newest title as
// the event carries it, empty where the event carried none, and the issue the event was
// written on — and marks the set stale only where the one read can answer differently for it.
// An empty title is read as the session's known one, since an event carrying no title takes
// none away. The three cases it marks:
//
//   - the session's newest title is a Legion role's and the set's own read did not know it as
//     one, so every issue the session has written on may carry phase work now;
//   - the title moved from the one the read knew — to another issue's, or to a title of no
//     Legion role at all — so the issues it held under the old title may hold none now;
//   - the title is the one the read knew, it names this very issue, and the set does not hold
//     that issue yet, which is the session's first event on the issue its title names.
//
// Anything else cannot move the read's answer: an ordinary session's event, and a Legion role's
// event on an issue the set already holds or that its title does not name, which is every event
// of a running phase worker after its first. Which titles are a role's at all is the one
// statement of that rule (legion.WorkerTitle), never a second one stated here.
func (w *legionPhaseWork) observe(session, title, issue string) {
	if session == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	known, wasRole := w.roleSessions[session]
	if title == "" {
		// An event carrying no title takes none away: the name the read uses is the session's
		// newest, so an event of a session already known as a role is read under that title.
		// Read as a title of no role instead, such an event would say the session had stopped
		// being a role, and the third case below - the role's first event on the issue its
		// title names - would never mark the set for an event written without one.
		title = known
	}
	parsed, isRole := legion.WorkerTitle(title)
	if isRole && wasRole && known == title {
		if _, held := w.keys[issue]; held || !parsed.Works(issue) {
			return
		}
		w.dirtied++
		return
	}
	if !isRole && !wasRole {
		return
	}
	if isRole {
		if w.roleSessions == nil {
			w.roleSessions = map[string]string{}
		}
		w.roleSessions[session] = title
	} else {
		delete(w.roleSessions, session)
	}
	w.dirtied++
}

// ensure fills the set where no read has, where an event has marked it stale since the last
// one, and wherever no feed is watching it: between the live hub's generations nothing reads
// the event log, so nothing can mark the set stale and a board read answers from a read of its
// own. A read that fails leaves it as it was, so the next board read reads again.
func (w *legionPhaseWork) ensure(ctx context.Context, pool *store.Pool) error {
	if w.isCurrent() {
		return nil
	}
	w.reading.Lock()
	defer w.reading.Unlock()
	if w.isCurrent() {
		return nil
	}
	return w.fill(ctx, pool)
}

// reseed fills the set again whatever it holds and makes feed its watcher from then on: the
// intake of the generation feed belongs to marks the set stale from the event log above the
// cursor it starts on, so the fill and the watch belong beside that cursor (seedEvents), at the
// generation's start and after a feed closed. Nothing fills the set between generations: there
// ensure's own read answers each board read.
func (w *legionPhaseWork) reseed(ctx context.Context, pool *store.Pool, feed <-chan struct{}) error {
	w.reading.Lock()
	defer w.reading.Unlock()
	if err := w.fill(ctx, pool); err != nil {
		return err
	}
	w.watch(feed)
	return nil
}

// watch makes feed the set's watcher, the feed whose events mark it stale. A feed already
// closed watches nothing, so a generation that stopped while its reseed ran leaves the set
// unwatched rather than watched by a feed nobody reads.
func (w *legionPhaseWork) watch(feed <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if feedOpen(feed) {
		w.watching = feed
	}
}

// unwatch takes the watcher off, for a feed that closed under a generation still running: until
// the subscription that replaces it has seeded and watched the set (seedEvents), no event
// reaches it and each board read answers from a read of its own.
//
// It takes the feed whose intake is unwatching, and clears the watch only while that feed is
// the one watching: a stopping generation's intake can take its closed feed in the same pass as
// its own done channel, and by then the generation after it may already have seeded and
// watched, whose watch is not this one's to drop.
func (w *legionPhaseWork) unwatch(feed <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.watching == feed {
		w.watching = nil
	}
}

// feedOpen is whether feed still carries the events that mark the set stale: a watcher no
// generation set (nil) and one whose generation has stopped (closed) carry none.
func feedOpen(feed <-chan struct{}) bool {
	if feed == nil {
		return false
	}
	select {
	case <-feed:
		return false
	default:
		return true
	}
}

// isCurrent is whether the set can answer a board read: a feed is watching it, which is also
// what says a read has filled it, and nothing has marked it stale since that read. A set
// nothing watches is current for nothing however lately it was filled, since the events that
// would move it reach no reader while no page is open.
func (w *legionPhaseWork) isCurrent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.read == w.dirtied && feedOpen(w.watching)
}

// fill reads the whole set and takes it as it stands, the index of the sessions the read found
// working an issue with it. It replaces what the set held: an issue whose only phase work was a
// session since renamed carries none now, and only this read says so. An event that marked the
// set stale while the read ran leaves it stale (read below dirtied), so the index this takes is
// read again with the set it belongs to; until then it costs that session one mark too many and
// never one too few.
func (w *legionPhaseWork) fill(ctx context.Context, pool *store.Pool) error {
	w.mu.Lock()
	at := w.dirtied
	w.mu.Unlock()
	work, err := legion.PhaseWorkKeys(ctx, pool)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keys, w.roleSessions, w.read = work.Keys, work.RoleSessions, at
	return nil
}
