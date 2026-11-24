package session

import "sync/atomic"

// storedByWriterSeam is a test's per-session override of the leader read StoredByWriter makes
// (bus.KeyValue.NewestFromLeader). Its zero value, the production value, overrides nothing: every
// StoredByWriter call reads the session bucket's stream leader. The overrides pointer is nil until
// a test installs one through SessionRegistry.SetStoredByWriterSeamForTest; nothing else writes it.
// StoredByWriter is called only from a request goroutine, never from the registry's own watcher or
// reaper, but the pointer is atomic so an install and a concurrent read never race under -race.
type storedByWriterSeam struct {
	overrides atomic.Pointer[map[string]storedByWriterResult]
}

// storedByWriterResult is one session id's seamed answer: the (entry, written, error) triple
// StoredByWriter returns for that id without reading the bucket.
type storedByWriterResult struct {
	entry   SessionEntry
	written bool
	err     error
}

// lookup returns sessionID's installed override and whether one is installed.
func (s *storedByWriterSeam) lookup(sessionID string) (storedByWriterResult, bool) {
	installed := s.overrides.Load()
	if installed == nil {
		return storedByWriterResult{}, false
	}
	res, ok := (*installed)[sessionID]
	return res, ok
}

// set installs res for sessionID, or removes sessionID's override when res is nil, leaving every
// other session's override as it is. It swaps a fresh map in, so a lookup reads a map no set mutates.
func (s *storedByWriterSeam) set(sessionID string, res *storedByWriterResult) {
	for {
		current := s.overrides.Load()
		next := map[string]storedByWriterResult{}
		if current != nil {
			for key, value := range *current {
				next[key] = value
			}
		}
		if res == nil {
			delete(next, sessionID)
		} else {
			next[sessionID] = *res
		}
		if s.overrides.CompareAndSwap(current, &next) {
			return
		}
	}
}

// SetStoredByWriterSeamForTest makes StoredByWriter answer (entry, written, err) for sessionID
// instead of reading that key's newest message from the session bucket's stream leader
// (bus.KeyValue.NewestFromLeader). It gives a cmd/listener handler test the two leader-read cases it
// cannot force from outside the package:
//
//   - a leader read that does not answer: pass a non-nil err. StoredByWriter returns it, so the edge
//     route answers the bus-unavailable 503 it answers for any read that did not answer, and the
//     control that drops that error (sessionInProject, edgeScope.showsConfirmed) is a silent 200 the
//     test then catches.
//   - a leader record that differs from the cached entry: pass an entry whose projects differ.
//     edgeScope.showsConfirmed and sessionInProject re-judge on this record, never the cached one, so
//     the edge follows the record the leader holds.
//
// Every other session id still reads the real leader, so the session a request acts as is confirmed
// as it is in production. It exists in tests alone; no deployment installs a seam. The returned func
// removes this session's override and leaves every other as it is.
func (r *SessionRegistry) SetStoredByWriterSeamForTest(sessionID string, entry SessionEntry, written bool, err error) (restore func()) {
	r.leaderSeam.set(sessionID, &storedByWriterResult{entry: entry, written: written, err: err})
	return func() { r.leaderSeam.set(sessionID, nil) }
}
