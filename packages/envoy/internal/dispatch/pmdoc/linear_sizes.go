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

// ladderSizes is the two sizes TestParseIsLinearInATableOfEscapedPipes and
// TestBareURLShapesParseAndRenderLinearly time each of their shapes at: 16 KiB and the mebibyte a
// document may be, which is the size each of those shapes is said to stay linear up to. The input
// grows 64 times between them, so growthAllowance lets a linear read grow 256 times where a
// quadratic one grows 4,096 - a wider margin than either step of the 16 KiB, 256 KiB and 1 MiB
// ladder these two used to run.
//
// Those rungs were the largest piece of the envoy-go CI step, which is bound by the four cores it
// runs on and has to come under 480 s, so both were tried a quarter of the size
// (dispatch://INFRA-1075). dispatch://INFRA-985's architect allowed that on the conditions that
// the margin not move, that every shape's quadratic control still fail, and that the smaller rung
// stay far enough above a fixed per-call cost for the ratio to be the shape's own: a control whose
// measured ratio is under twice the 256 times allowed says the smaller call is mostly that fixed
// cost, and the test it guards keeps the pair it had. Two of the three failed that rule when it
// was measured, so they keep these sizes: at 4 KiB and 256 KiB the escaped-pipe table's quadratic
// control READ 193 TIMES ITS OWN SMALLER CALL AND SO PASSED THE TEST IT IS MEANT TO FAIL, and the
// bare-URL shapes' control failed at 370, under the 512 the rule asks for. Only the unclosed
// raw-HTML shapes cleared it, at 595, and they read smallLadderSizes below.
var ladderSizes = []int{16 << 10, 1 << 20}

// smallLadderSizes is ladderSizes divided by four, read by
// TestUnclosedRawHTMLShapesParseAndRenderLinearly alone: the one ladder whose quadratic control
// still fails far enough past the allowance at these rungs (595 times its own 4 KiB call against
// the 256 allowed) for the ratio to be its shapes' own rather than a fixed per-call cost's. Its
// six shapes then cost a quarter of what they did, which is the point (dispatch://INFRA-1075).
//
// The ratios behind both pairs are read on CI, not on a gVisor pod. A window lasts at least
// windowSteps steps of the clock this process's CPU time is reported in, the call being repeated
// until it does (cputest.Measure), so where that step is 10 ms a window is half a second of CPU
// whatever the rung: a rung stops costing what it is, and the runs one window holds allocate
// against timedWindow's windowMemoryLimit rather than the rung doing so - measured on a two-CPU
// gVisor pod on 2026-11-08, one bare-URL shape's window allocated 2.7 GiB at 4 KiB, 2.8 at
// 256 KiB, 2.7 at 16 KiB and 2.4 at 1 MiB against that limit's 3 GiB, which is the same window
// four times over rather than four rungs. A Linux kernel steps that clock far finer (cputest),
// where one call fills a window at any of these sizes and a rung costs its own call again.
var smallLadderSizes = []int{4 << 10, 256 << 10}

// alternatingBlocks are the documents TestUpdateIsLinearInTheBlockCount replaces every other block
// of: 16,384 blocks is about the most paragraphs the 65,536 elements one upload may make allow.
var alternatingBlocks = []int{4_096, 16_384}
