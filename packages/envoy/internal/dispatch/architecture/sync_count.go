// sync_count.go holds the count of the importer's syncs that run: each flight, and each call of
// Sync from its start until the flight it joined has answered (syncCount), which Close waits on and
// a test reads through Syncing.

package architecture

import "sync"

// syncCount counts the flights running and the calls of Sync waiting on one,
// each call until the flight it joined has answered whether or not its caller
// stayed (Syncing), and waits for none to be left. It has the shape of
// backgroundWork in api/server.go, and for its reason: a sync.WaitGroup is not
// it, because a flight may start while a wait is already waiting, and Add
// after Wait began is what a WaitGroup forbids. begin refuses once Close has
// begun, so a flight whose goroutine starts after Close took its decision does
// nothing rather than write into a closing pool, and so does a Sync called
// then.
type syncCount struct {
	mu      sync.Mutex
	closing bool
	running int
	// idle is closed when running last fell to zero, and replaced when a
	// flight or a call starts again.
	idle chan struct{}
}

func (c *syncCount) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return false
	}
	if c.running == 0 {
		c.idle = make(chan struct{})
	}
	c.running++
	return true
}

func (c *syncCount) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running--
	if c.running == 0 {
		close(c.idle)
	}
}

// stop refuses every later flight and waits for the running ones to end.
func (c *syncCount) stop() {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	for {
		c.mu.Lock()
		running, idle := c.running, c.idle
		c.mu.Unlock()
		if running == 0 {
			return
		}
		<-idle
	}
}

// Syncing answers whether a sync of the importer runs: a flight, or a call of
// Sync from its start until the flight it joined has answered, the call of a
// caller that left first included, since its flight goes on writing the
// source's rows. Nothing in production calls it: a test that reads the whole
// database before and after a request (the API tests' databaseFingerprint)
// waits for it to answer false.
func (i *Importer) Syncing() bool {
	if i == nil {
		return false
	}
	i.running.mu.Lock()
	defer i.running.mu.Unlock()
	return i.running.running > 0
}
