//go:build !unix

package memory

import "errors"

// errNoFileLock refuses the memory store's writer on a system without flock. Each agent-secrets
// run is a process of its own, so a lock held within one process would let two runs read the same
// file and lose each other's write. agent-secrets ships for Linux and macOS alone, which have it.
var errNoFileLock = errors.New("the memory store's writer needs a file lock between processes (flock), which this system does not have")

// fileLockMissing is errNoFileLock: OpenWriter refuses before any write.
func fileLockMissing() error { return errNoFileLock }

// lockFile takes no lock here and answers errNoFileLock.
func lockFile(string) (unlock func(), err error) { return nil, errNoFileLock }
