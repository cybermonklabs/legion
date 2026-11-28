package pmdoc

import "sync/atomic"

// parseWork holds the process total that ParseWork reads.
var parseWork atomic.Uint64

// renderWork holds the process total that RenderWork reads.
var renderWork atomic.Uint64

// updateWork holds the process total that UpdateWork reads.
var updateWork atomic.Uint64

// scanWork holds the process total that ScanWork reads.
var scanWork atomic.Uint64

// ParseWork returns the bytes read by the process's inline raw HTML walks. It counts the bytes of
// each line a walk reads while looking for a closer with bytes.Index or a regexp. Coverage counts
// the calls, not the bytes those standard-library searches read.
func ParseWork() uint64 { return parseWork.Load() }

// RenderWork returns the bytes read by the process's forward scans for a closing bracket or angle
// bracket in rendered text. It counts through the closer, or to the end if there is none. Coverage
// counts the calls to strings.IndexByte, not the bytes it reads.
func RenderWork() uint64 { return renderWork.Load() }

// UpdateWork returns the items the process's writes reach in the fragment. It counts the children
// listed, the items walked over to an edit's index, and the children or text removed or inserted.
// A text index or deletion length uses Yjs's UTF-16 units, and a text insertion counts the bytes
// passed to Yjs. Coverage counts ygo's statements but does not separate these items from its work.
func UpdateWork() uint64 { return updateWork.Load() }

// ScanWork returns the bytes that scans read apart from ParseWork and RenderWork.
// The tab scan in readSource counts through the first tab or to the end of the source.
// tildeParser counts the run of tildes read by Goldmark's delimiter scan, even if the run is too long
// to become a delimiter. Coverage does not count the bytes of either scan.
func ScanWork() uint64 { return scanWork.Load() }

// indexByteWork counts the bytes an IndexByte search reaches, including a match if it finds one.
func indexByteWork(length, found int) uint64 {
	if found < 0 {
		return uint64(length)
	}
	return uint64(found + 1)
}
