# Quadratic controls for pmdoc's CPU-budget tests (dispatch://INFRA-1075)

Four deliberately super-linear changes, one per test, each a unified diff against
`packages/envoy/internal/dispatch/pmdoc/`. Apply **one at a time** (each declares its own
`quadraticDefectSink`, so two of them together will not compile), run that test, show it fails,
discard the copy.

```bash
cd packages/envoy/internal/dispatch/pmdoc
git apply /path/to/pmdoc-quadratic-<test>.patch
cd ../../../..            # packages/envoy
go test -count=1 -timeout 20m -v -run '<regexp>' ./internal/dispatch/pmdoc/
```

| Test | `-run` regexp | Patch | What it makes quadratic |
| --- | --- | --- | --- |
| `TestParseIsLinearInATableOfEscapedPipes` | `^TestParseIsLinearInATableOfEscapedPipes$` | `pmdoc-quadratic-TestParseIsLinearInATableOfEscapedPipes.patch` | `unescapeTablePipes`: every code span reads the whole document again, one byte in four - the shape of the check-each-code-span-against-every-escaped-pipe the goldmark transformer did before it. |
| `TestBareURLShapesParseAndRenderLinearly` | `^TestBareURLShapesParseAndRenderLinearly$` | `pmdoc-quadratic-TestBareURLShapesParseAndRenderLinearly.patch` | `literalAddress`: every place the tokenizer may start an address reads the rest of its line again, one byte in 64 - the shape of the read-the-domain-again-from-every-trigger `literalDomains` replaced. |
| `TestUnclosedRawHTMLShapesParseAndRenderLinearly` | `^TestUnclosedRawHTMLShapesParseAndRenderLinearly$` | `pmdoc-quadratic-TestUnclosedRawHTMLShapesParseAndRenderLinearly.patch` | `inlineRawHTMLParser.Parse`: every opener of a kind that ends at a closer of its own reads the block's source again, one byte in 64 - the shape of the walk-from-every-opener `unclosedRawHTML` replaced. |
| `TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText` | `^TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText$/^a_$` | `pmdoc-quadratic-TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText.patch` | `parseInlineMarks`: every piece of inline text reads the whole source again, one byte in 64 - the shape of a scan of the run from each of its characters. |

Each is cheap per byte on purpose, so the test fails on its ratio rather than sitting on
`perCallDeadline`.

## What each failure looked like on a two-CPU gVisor pod, 2026-11-08, at the sizes in the tree

```
linear_test.go:337: parsing a table of `\|` cells took 6.7s at 1 MiB, 550 times the 12ms it took at 16 KiB: the input grew 64 times, and a parse linear in it may grow 256 times at most
linear_test.go:371: reading and writing "_www.a" took 3.42s at 1 MiB, 996 times the 3ms it took at 16 KiB: the input grew 64 times, and a parse linear in it may grow 256 times at most
linear_test.go:416: reading and writing "p <!a" took 700ms at 1 MiB, 1171 times the 1ms it took at 16 KiB: the input grew 64 times, and a parse linear in it may grow 256 times at most
linear_test.go:63:  parsing "a_" took 1.09s at 1 MiB, 50 times the 22ms it took at 128 KiB: the input grew 8 times, and a parse linear in it may grow 24 times at most
```

The first three ran at the two sizes this change leaves (`ladderSizes`, 16 KiB and 1 MiB); the
fourth at the pair it already had (128 KiB and 1 MiB).
