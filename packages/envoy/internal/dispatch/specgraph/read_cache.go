package specgraph

import (
	"crypto/sha256"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// fileReads keeps readFile's read of each file that ReadLayout reads, so that a file is read once
// while its read is kept. A process reads one file many times: Approved.Current reads the newest
// document of each node on each call (versionText), print reads its directory before and after
// its plan, and a test of a rule reads a scenario again for each change it makes to one of its
// files. readFile is a function of a file's name and bytes alone: it reads no clock, no
// environment and no state that a caller sets. So the read kept for a name and content is the read
// of them. A file is read again after the cache empties at its bound, and two readers that miss
// the same file at once both read it.
//
// What a caller of ReadLayout or Parse may write into, stated here alone:
//   - The documents of a layout and the node of each are the kept read, shared and never copied,
//     as the values of bytelru are, and so are each node's lists (a statement's Serves and About,
//     an interface's Joins) and a check's Test. A caller must not write into them. Layout.Place
//     copies a file's documents before it changes them.
//   - The problems, the maps of a layout and the lists of a Tree that Parse gives are each call's
//     own. ReadLayout appends each file's problems to a list that it makes, and that copy is what
//     keeps a caller's write into its problems, such as versionText's DeleteFunc or a
//     SortProblems, out of the kept read: the kept problems are never handed out themselves.
var fileReads = fileReadCache{bound: fileReadsBound}

// fileReadsBound is what fileReads keeps at most, as fileReadCharge counts it: 32 times
// notes.MaxFileSize, the bound of one spec file. Past it the cache starts again from empty, so a
// process that reads new files without end, as a test of random trees does, holds no more.
const fileReadsBound = 32 * notes.MaxFileSize

// fileReadEntryCharge and fileReadProblemCharge are what fileReadCharge adds to a file's bytes for
// each kept read and for each of its problems, so that the bound holds the heap a read keeps
// whatever the shape of its files. A kept read holds an entry of the map, its documents' texts,
// which are a copy of the file, the nodes decoded from them, and its problems. Measured on nine
// shapes, with the heap after a collection of a cache filled with 8 MiB of files or more: an empty
// file kept 319 bytes, one line that is no node 320, a document of one short term 345 to 459, and
// 1 MB of documents that are no node, one problem each, about 172 bytes a document. With these
// charges the heap held 0.44 to 2.93 bytes for each byte counted, the most for 1 MB of minimal
// terms, so at most about 100 MB at the bound. Without them, counting file bytes alone, an empty
// file held 22.8 bytes for each byte counted and 1 MB of documents that are no node 28.8.
const fileReadEntryCharge, fileReadProblemCharge = 512, 192

// fileReadCache keeps the read of each file, up to bound as fileReadCharge counts it.
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
	c.keep(key, fileReadCharge(name, data, problems), fileRead{documents: documents, problems: problems, uncounted: uncounted})
	return documents, problems, uncounted
}

// fileReadCharge is what a kept read of the file name with the content data counts against the
// bound: fileReadEntryCharge, the bytes of the name and the content, and fileReadProblemCharge
// for each problem of the read.
func fileReadCharge(name string, data []byte, problems []Problem) int {
	return fileReadEntryCharge + len(name) + len(data) + fileReadProblemCharge*len(problems)
}

// keep keeps read under key, counted as size. A read the cache already holds is kept as it is
// held, so when two readers missed the same key, the first to keep its read wins. A read over the
// bound alone is not kept; one that would take the cache past its bound empties it first.
func (c *fileReadCache) keep(key fileKey, size int, read fileRead) {
	if size > c.bound {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, found := c.reads[key]; found {
		return
	}
	if c.reads == nil || c.bytes+size > c.bound {
		c.reads, c.bytes = map[fileKey]fileRead{}, 0
	}
	c.reads[key] = read
	c.bytes += size
}
