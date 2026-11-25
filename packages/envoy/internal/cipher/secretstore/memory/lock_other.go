//go:build !unix

package memory

import "sync"

// fileLocks holds one lock per memory secrets file for this process. A system without flock
// serializes the writers of one process alone; the memory store serves local runs and tests,
// which write a file from one process.
var fileLocks sync.Map

// lockFile takes this process's lock of path and answers its release.
func lockFile(path string) (unlock func(), err error) {
	l, _ := fileLocks.LoadOrStore(path, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock, nil
}
