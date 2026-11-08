package pmdoc

// parseFullTarget and renderFullTarget are
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText's full-size targets.
const (
	// parseFullTarget is a mebibyte: the write-path limit (MaxDocumentBytes) this loop's parse
	// reads back, since a caller's write of this much is refused for the elements it makes.
	parseFullTarget = 1 << 20
	// renderFullTarget is smaller: large enough for growthStep's quadratic margin, small enough
	// that even the slowest shape (`<a`) stays fast - rendering is not bound by MaxDocumentBytes.
	renderFullTarget = 512 << 10
	// updateSeedTarget is TestUpdateIsLinearInTheBlockCount's seed of one-word paragraphs: the
	// mebibyte a document may hold, which a rebuild from a stored rendering writes whole.
	updateSeedTarget = 1 << 20
	// updateShapeTarget is its other shapes', each 21,845 blocks or fewer, about what the 65,536
	// elements one upload may make allow of them.
	updateShapeTarget = 128 << 10
)

// ladderSizes is the two sizes TestParseIsLinearInATableOfEscapedPipes,
// TestBareURLShapesParseAndRenderLinearly and TestUnclosedRawHTMLShapesParseAndRenderLinearly
// time each of their shapes at: 16 KiB, and the mebibyte a document may be, which is the size
// each of those shapes is said to stay linear up to. The input grows 64 times between them, so
// growthAllowance lets a linear read grow 256 times where a quadratic one grows 4,096 - a wider
// margin than either step of the 16 KiB, 256 KiB and 1 MiB ladder the three used to run.
//
// That middle size cost a third of the three tests' timed windows and told the pair nothing. A
// window lasts at least windowSteps steps of the clock this process's CPU time is reported in,
// the call being repeated until it does (cputest.Measure), so a size whose own call is shorter
// than one window costs a window whatever that size is; only a size whose call is longer than a
// window costs what the size is. Measured on a two-CPU gVisor pod on 2026-11-08, these shapes
// cost 20-750 ms a call at 1 MiB and 5-170 ms at 256 KiB, against a window of 500 ms there, so
// the middle size never paid for itself and the two sizes that say something were kept.
var ladderSizes = []int{16 << 10, 1 << 20}

// alternatingBlocks are the documents TestUpdateIsLinearInTheBlockCount replaces every other block
// of: 16,384 blocks is about the most paragraphs the 65,536 elements one upload may make allow.
var alternatingBlocks = []int{4_096, 16_384}
