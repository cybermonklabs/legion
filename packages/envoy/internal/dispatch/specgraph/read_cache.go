package specgraph

import (
	"crypto/sha256"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// fileReads keeps readFile's read of each file that ReadLayout reads, so a process reads the
// same file once. A process reads one file many times: Approved.Current reads the newest document
// of each node on each call (versionText), print reads its directory before and after its plan,
// and a test of a rule reads a scenario again for each change it makes to one of its files.
// readFile is a function of a file's name and bytes alone: it reads no clock, no environment and
// no state that a caller sets. So the read kept for a name and content is the read of them.
//
// A kept read is shared, never copied, as the values of bytelru are: a caller of ReadLayout or
// Parse must not write into the documents, nodes or problems it gets. None does: Layout.Place
// copies a file's documents before it changes them, and Parse gives lists of its own.
var fileReads = fileReadCache{bound: fileReadsBound}

// fileReadsBound is the bytes of files whose reads fileReads keeps: 32 times notes.MaxFileSize,
// the bound of one spec file. It counts file bytes, not memory: a kept read holds its documents'
// texts, which are a copy of the file, and the nodes decoded from them. Measured on three
// fillings (a 1.5 KB file under 2,000 names, and 31 files of about 1 MB, of small terms and of
// long ones), the heap held 2.21, 2.57 and 2.03 bytes for each counted byte, so about 70 to 90 MB
// at the bound. Past the bound the cache starts again from empty, so a process that reads new
// files without end, as a test of random trees does, holds the reads of at most this many bytes
// of files.
const fileReadsBound = 32 * notes.MaxFileSize

// fileReadCache keeps the read of each file, up to bound bytes of files.
type fileReadCache struct {
	mu    sync.Mutex
	bound int
	bytes int
	reads map[fileKey]fileRead
}

// fileKey is one file: its name, which each position of its read names, and the SHA-256 of its
// bytes. The digest stands for the bytes, so a kept read holds no second copy of its file.
type fileKey struct {
	name string
	sum  [sha256.Size]byte
}

// fileRead is what readFile gave for one file.
type fileRead struct {
	documents []Document
	problems  []Problem
	uncounted string
}

// read is readFile's read of the file name with the content data: the kept read when the cache
// holds one for that name and content, else a new read, which it keeps.
func (c *fileReadCache) read(name string, data []byte) ([]Document, []Problem, string) {
	key := fileKey{name: name, sum: sha256.Sum256(data)}
	c.mu.Lock()
	kept, found := c.reads[key]
	c.mu.Unlock()
	if found {
		return kept.documents, kept.problems, kept.uncounted
	}
	documents, problems, uncounted := readFile(name, data)
	c.keep(key, len(name)+len(data), fileRead{documents: documents, problems: problems, uncounted: uncounted})
	return documents, problems, uncounted
}

// keep keeps read under key, counted as size bytes of files. A file over the bound alone is not
// kept; one that would take the cache past its bound empties it first. When two readers missed
// the same key, the first to keep its read wins.
func (c *fileReadCache) keep(key fileKey, size int, read fileRead) {
	if size > c.bound {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads == nil || c.bytes+size > c.bound {
		c.reads, c.bytes = map[fileKey]fileRead{}, 0
	}
	if _, found := c.reads[key]; !found {
		c.reads[key] = read
		c.bytes += size
	}
}
