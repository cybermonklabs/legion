# Quadratic controls for pmdoc's CPU-budget tests (dispatch://INFRA-1075)

Four deliberately super-linear changes, one per test, each a unified diff against
`packages/envoy/internal/dispatch/pmdoc/`. Apply **one at a time** (each declares its own
`quadraticDefectSink`, so two of them together will not compile), run that test, show it fails,
reverse it.

```bash
cd <repository root>
git apply --directory=packages/envoy/internal/dispatch/pmdoc /path/to/pmdoc-quadratic-<test>.patch
cd packages/envoy
go test -count=1 -timeout 20m -v -run '<regexp>' ./internal/dispatch/pmdoc/
cd ..
git apply -R --directory=packages/envoy/internal/dispatch/pmdoc /path/to/pmdoc-quadratic-<test>.patch
```

| Test | `-run` regexp | Patch | What it makes quadratic |
| --- | --- | --- | --- |
| `TestParseIsLinearInATableOfEscapedPipes` | `^TestParseIsLinearInATableOfEscapedPipes$` | `pmdoc-quadratic-TestParseIsLinearInATableOfEscapedPipes.patch` | `unescapeTablePipes`: every code span reads the whole document again, one byte in four - the shape of the check-each-code-span-against-every-escaped-pipe the goldmark transformer did before it. |
| `TestBareURLShapesParseAndRenderLinearly` | `^TestBareURLShapesParseAndRenderLinearly$` | `pmdoc-quadratic-TestBareURLShapesParseAndRenderLinearly.patch` | `literalAddress`: every place the tokenizer may start an address reads the rest of its line again, one byte in 64 - the shape of the read-the-domain-again-from-every-trigger `literalDomains` replaced. |
| `TestUnclosedRawHTMLShapesParseAndRenderLinearly` | `^TestUnclosedRawHTMLShapesParseAndRenderLinearly$` | `pmdoc-quadratic-TestUnclosedRawHTMLShapesParseAndRenderLinearly.patch` | `inlineRawHTMLParser.Parse`: every opener of a kind that ends at a closer of its own reads the block's source again, one byte in 64 - the shape of the walk-from-every-opener `unclosedRawHTML` replaced. |
| `TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText` | `^TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText$/^a_$` | `pmdoc-quadratic-TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText.patch` | `parseInlineMarks`: every piece of inline text reads the whole source again, one byte in 64 - the shape of a scan of the run from each of its characters. |

Each is cheap per byte on purpose, so the test fails on its ratio rather than sitting on
`perCallDeadline`. The first three run at `ladderSizes`; the fourth at the pair its own test
declares (128 KiB and 1 MiB), which this change does not touch, and it stops at its first shape,
so give it the `/^a_$` subtest and it fails in minutes.

## What each control did at `ladderSizes` = {4 KiB, 256 KiB}

Measured on a two-CPU gVisor pod on 2026-11-08, one control at a time, each reversed afterwards
(`sha256sum -c` on the patched file against its pre-patch digest: OK for all four).

```
PASS  linear_test.go:337: parsing a table of `\|` cells: 3ms at 4 KiB, 500ms at 256 KiB, 193.6 times for an input 64 times larger (at most 256)
FAIL  linear_test.go:371: reading and writing "_www.a" took 260ms at 256 KiB, 370 times the 1ms it took at 4 KiB: the input grew 64 times, and a parse linear in it may grow 256 times at most
FAIL  linear_test.go:416: reading and writing "p <!a" took 60ms at 256 KiB, 595 times the 0s it took at 4 KiB: the input grew 64 times, and a parse linear in it may grow 256 times at most
FAIL  linear_test.go:63:  parsing "a_" took 1.1s at 1 MiB, 51 times the 22ms it took at 128 KiB: the input grew 8 times, and a parse linear in it may grow 24 times at most
```

Against dispatch://INFRA-985's rule - a control whose ratio is under 512, twice the 256 the
64-fold step allows, means the 4 KiB call is mostly fixed cost and that test keeps 16 KiB and
1 MiB - these pod figures say:

- `TestParseIsLinearInATableOfEscapedPipes`: **193.6**, under the 256 allowance itself. The
  control does not merely come in low, it stops failing: at these rungs the test would not catch
  the defect it exists for. Refused.
- `TestBareURLShapesParseAndRenderLinearly`: **370**. Fails, but under 512. Refused.
- `TestUnclosedRawHTMLShapesParseAndRenderLinearly`: **595**. Fails, over 512. Stands.
- `TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText`: **51** against its own allowance of
  24 (`linearSlack`*`growthStep`). Its sizes are unchanged, so the 512 rule does not bear on it;
  it is here so the bounds workflow can still run all four.

For comparison, the same four controls at the old `ladderSizes` = {16 KiB, 1 MiB}, on the same
pod on 2026-11-08: 550, 996, 1171 and 51.

These are the ratios of a two-core pod whose CPU clock steps 10 ms. CI runs four cores and a
clock that steps far finer, so the ratios the bounds workflow prints there are the ones the rule
is read against.
