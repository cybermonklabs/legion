package api

import "sync"

// issueLocks is one mutex for each issue a request holds or waits on, made at its first use and
// dropped once no request holds it or waits on it, so the map keeps only the issues in use. The
// zero value is ready.
type issueLocks struct {
	mu    sync.Mutex
	locks map[string]*issueLock
}

// issueLock is one issue's mutex and the number of requests that hold it or wait on it.
type issueLock struct {
	sync.Mutex
	users int
}

// lock waits for issue's mutex and answers its release, which the caller must call once.
func (l *issueLocks) lock(issue string) (unlock func()) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*issueLock{}
	}
	held, ok := l.locks[issue]
	if !ok {
		held = &issueLock{}
		l.locks[issue] = held
	}
	held.users++
	l.mu.Unlock()
	held.Lock()
	return func() {
		held.Unlock()
		l.mu.Lock()
		if held.users--; held.users == 0 {
			delete(l.locks, issue)
		}
		l.mu.Unlock()
	}
}
