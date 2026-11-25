package pmdoc

import "sync/atomic"

// parseWork, renderWork and updateWork count what ParseWork, RenderWork and UpdateWork answer.
var parseWork, renderWork, updateWork atomic.Uint64

// The tests hold this package's work linear in its input by counting it with Go's coverage
// counters, which count every statement of this package, goldmark and ygo that a call runs
// (counted_work_test.go). Three stages of that work are counted here as well, each because the
// coverage counters cannot tell it apart: ParseWork and RenderWork count bytes the standard
// library's searches read, which the counters do not instrument, and UpdateWork counts the items
// a write reaches apart from ygo's own walks, which the counters count as ygo's. A test reads each
// before and after a call and judges it beside the counted work, never in its place: each stage is
// counted at the operation the code makes now, so a change back to a quadratic shape, such as
// inserting a fragment's children one index at a time, would walk past a count left where it was,
// and only the coverage counters, which count ygo's walks wherever this package makes them
// (TestUpdateIsLinearInTheBlockCount), would fail it.

// ParseWork is how many bytes this process's inline raw HTML walks have read since it started:
// each byte of each line a walk reads from where it stands on it, looking for its closer with
// bytes.Index or a regexp (walkedLines).
func ParseWork() uint64 { return parseWork.Load() }

// RenderWork is how many characters this process's renders' searches for a closer have read since
// it started: each character a forward scan of a text reads with strings.IndexByte to find its
// next `]` or `>` (forwardScan.indexFrom).
func RenderWork() uint64 { return renderWork.Load() }

// UpdateWork is how many fragment items this process's writes of a tree into a document have
// reached since it started: for each fragment operation Update makes, the items ygo walks over to
// the index it is given and the items it deletes or inserts there, each inserted child counted
// once, before the loop that builds them (insertPChildren), and the items each list of a
// fragment's children holds. linear_test.go reads it for the shapes whose walks the coverage
// counters cannot tell apart from the items Update itself reaches.
func UpdateWork() uint64 { return updateWork.Load() }
