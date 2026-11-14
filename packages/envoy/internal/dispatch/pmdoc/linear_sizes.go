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
// time each of their shapes at: 16 KiB and the mebibyte a document may be, which is the size each
// of those shapes is said to stay linear up to. The input grows 64 times between them, so
// growthAllowance lets a linear read grow 256 times where a quadratic one grows 4,096 - a wider
// margin than either step of the 16 KiB, 256 KiB and 1 MiB ladder the three used to run.
//
// These rungs are the largest single piece of the envoy-go CI step, which is bound by the cores it
// runs on and has to come under 480 s, so all three were tried a quarter of the size
// (dispatch://INFRA-1075). dispatch://INFRA-985's architect allowed that on the conditions that
// the margin not move, that every shape's quadratic control still fail, and that the smaller rung
// stay far enough above a fixed per-call cost for the ratio to be the shape's own: a control whose
// measured ratio is under twice the 256 times allowed says the smaller call is mostly that fixed
// cost, and the test it guards keeps the pair it had. ALL THREE FAILED THAT RULE, so the size
// stands, and the numbers are worth keeping because they are the reason:
//
//   - the escaped-pipe table's control, at 4 KiB and 256 KiB, read 193 times its own smaller call
//     and so PASSED the test it is meant to fail, where at these rungs it fails at 1,331 (CI run
//     42505385314);
//   - the bare-URL shapes' control failed at 370 there and at 888 here (the same run);
//   - the unclosed raw-HTML shapes' control, which a pod read at 595, failed at only 383 on CI at
//     4 KiB and 256 KiB, under the 512 the rule asks for - and the healthy run of those shapes
//     read 0 ms or 1 ms a call at 4 KiB on the runner itself, which is the fixed cost the rule
//     exists to catch.
//
// Every one of those ratios is CI's, because a gVisor pod cannot read them: a window lasts at least
// windowSteps steps of the clock this process's CPU time is reported in, the call being repeated
// until it does (cputest.Measure), so where that step is 10 ms a window is half a second of CPU
// whatever the rung, and the runs one window holds allocate against timedWindow's
// windowMemoryLimit rather than the rung doing so - measured on a two-CPU gVisor pod on
// 2026-11-08, one bare-URL shape's window allocated 2.7 GiB at 4 KiB, 2.8 at 256 KiB, 2.7 at
// 16 KiB and 2.4 at 1 MiB against that limit's 3 GiB, which is the same window four times over
// rather than four rungs.
var ladderSizes = []int{16 << 10, 1 << 20}

// alternatingBlocks are the documents TestUpdateIsLinearInTheBlockCount replaces every other block
// of: 16,384 blocks is about the most paragraphs the 65,536 elements one upload may make allow.
var alternatingBlocks = []int{4_096, 16_384}
