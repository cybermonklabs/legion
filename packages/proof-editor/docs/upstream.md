# The upstream proof-sdk modules

The modules of the `sjawhar/proof-sdk` fork that `src/` imports at run time, as `proof-sdk-upstream/src/…`, come from the tarball `proof-sdk-<commit>.tgz` beside `package.json`, installed as `proof-sdk-upstream`.
This page says how that pin moves, how each tool resolves those modules, and which dependency patches keep the markdown parsers linear, with the one shape they do not: emphasis nested inside emphasis.

## Raising the pin

1. Run `bun run build-upstream-tarball <full 40-character sha>`, which needs GNU tar.

2. Delete the old tarball: `tests/one-upstream-tarball.test.ts` fails while two are present.

3. Set `package.json`'s `proof-sdk-upstream` to `file:./proof-sdk-<new commit>.tgz`.

4. Run `bun install`, which rewrites `bun.lock`.

5. Run `bun run upstream-declarations`, which rewrites `upstream/`.

6. Run `bun run test`: the pinned commit carries fixes upstream lacks, and `tests/upstream-pin.test.ts` fails when a pin loses one.

A reviewer rebuilds the tarball with `bun run build-upstream-tarball <commit> "$TMPDIR/rebuild.tgz"` and compares it with the committed one using `cmp`.
Every other path that names the tarball is the glob `packages/proof-editor/proof-sdk-*.tgz`, so a new name needs no edit there.
Moving the pin leaves the copies' audit source where it is (`docs/copied-files.md`).

## Resolving the modules

The upstream package declares no entry point, so each resolver is told the path: Bun through `paths` in `tsconfig.json`, Vite through `resolve.alias` in `packages/dispatch/web/vite.config.ts`, and tsc through `paths` in `tsconfig.check.json` and `packages/dispatch/tsconfig.json`.
No TypeScript program may open the fork's sources, which do not type-check.
tsc reads `upstream/` instead: declarations that `scripts/upstream-declarations.ts` generates from the pinned sources, committed, and never edited by hand.

## Parser patches

Four of the root `package.json`'s `patchedDependencies` make the markdown parsers linear: `micromark@4.0.2`, `micromark-core-commonmark@2.0.3`, `micromark-extension-gfm-footnote@2.1.0` and `mdast-util-from-markdown@2.0.3`.
One shape stays quadratic, as upstream's does: emphasis nested inside emphasis costs the square of its depth, because micromark resolves what lies between an opener and its closer again each time an enclosing run closes (emphasis nested 800 deep, 4,801 characters: 26,899,192 attention steps).
Dispatch's server bounds that depth: it refuses a body that nests inline marks more than 100 deep (`maxInlineNesting`, `packages/envoy/internal/dispatch/pmdoc/nesting.go`), where this parse takes 422,392 attention steps.
A fifth, the `proof-sdk` tarball's, makes the proof marks' transform (`remarkProofMarks`) linear, its legacy-formatting step (`convertHtmlFormatting`) included.
A sixth, `remark-inline-links@7.0.0`'s, removes each parent's definitions in one pass after its walk, where a splice for each shifted every later sibling.
Each of the four changes both `lib/` and `dev/lib/`, because Vite resolves the `development` export condition.
`tests/heavy-bodies.test.ts` and `tests/nested-directive.test.ts` hold the patches by the steps each parse takes, which `tests/parse-work.ts` counts.
Its "The dependency patches" says what each patch makes linear and what it counts, and how to put a patch's scan back to see a row catch it.
The `micromark` patch holds the lazy shapes (`a quote then lines carrying no marker`, `a list item then lines carrying no marker`), and the `micromark-core-commonmark` patch the bracket shapes (`a run of 15,000 brackets and as many closers`, `a typed fence whose label opens a run of brackets`) and, with the `remark-inline-links` patch, `800 definitions and 800 references to them`.
The `proof-sdk` patch holds the proof-span shapes, `a proof span holding 1,000 emphasis tags that nothing closes` among them.
No test holds the `micromark-extension-gfm-footnote` patch, since neither file has a footnote shape, and a quadratic parse of a shape no row holds is caught by nothing.
