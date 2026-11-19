# @legion/proof-editor

Dispatch's document editor: the Milkdown/ProseMirror entry point (`src/lib.ts`), the DOM-free parse and serialize engine (`src/lib-headless.ts`), the typed-block grammar and block ids.
Its consumers are `packages/dispatch` and the pmdoc fixture generator.
There is no build step: `exports` serves `src/*.ts`, which Bun runs and Vite compiles.

## Commands

- From this directory: `bun run test` (one file: `bun test tests/upstream-pin.test.ts`), `bun run typecheck`, which also runs `tests/upstream-boundary.ts`, and `bun run lint`.

## Copied code

- Most of `src/` is copied from the `sjawhar/proof-sdk` fork at commit 24a5fc94915cda5704897c497af6312c140db40d, which stays the audit source when the pin below moves.
  `src/directive-factories.ts` comes from micromark-extension-directive 4.0.0 and `src/html-flow.ts` from micromark-core-commonmark 2.0.3.
  The copies and the generated `upstream/` keep their MIT license (`LICENSE`); legion's own files, `scripts/` and `tests/` are Apache-2.0.

- Besides those two, a file under `src/` is a copy when the fork's commit 24a5fc94 holds it at the same path; the rest are legion's own.
  A new file of legion's own joins the names the first override of the root `biome.json` excludes, so that Biome checks it.

- Keep an edit to a copied file as small as the change needs; put new behaviour in a module of legion's own that the copy imports.
  To audit a copy, diff it against `jj --ignore-working-copy -R <proof-sdk> file show -r 24a5fc94 root:src/<file>`.

- Biome's formatter, linter and `assist` are off over the copies so that this diff stays readable.
  An editor that organizes imports on save still reorders them.

## Raising the upstream pin

The fork's modules that `src/` imports at run time come from the tarball `proof-sdk-<commit>.tgz` beside `package.json`, installed as `proof-sdk-upstream`.
To move that pin:

1. Run `bun run build-upstream-tarball <full 40-character sha>`, which needs GNU tar.

2. Delete the old tarball (`tests/one-upstream-tarball.test.ts` fails while two are present).

3. Set `package.json`'s `proof-sdk-upstream` to `file:./proof-sdk-<new commit>.tgz`.

4. Run `bun install`, which rewrites `bun.lock`.

5. Run `bun run upstream-declarations`, which rewrites `upstream/`.

6. Run `bun run test`: the pinned commit carries fixes upstream lacks, and `tests/upstream-pin.test.ts` fails when a pin loses one.

A reviewer rebuilds the tarball with `bun run build-upstream-tarball <commit> "$TMPDIR/rebuild.tgz"` and compares it with the committed one using `cmp`.
Every other path that names the tarball is the glob `packages/proof-editor/proof-sdk-*.tgz`, so a new name needs no edit there.

## The upstream boundary

- `src/` imports the fork's modules as `proof-sdk-upstream/src/…`.
  The upstream package declares no entry point, so each resolver is told the path:
  Bun through `paths` in `tsconfig.json`, Vite through `resolve.alias` in `packages/dispatch/web/vite.config.ts`, and tsc through `paths` in `tsconfig.check.json` and `packages/dispatch/tsconfig.json`.

- No TypeScript program may open those sources, which do not type-check.
  tsc reads `upstream/` instead: declarations that `scripts/upstream-declarations.ts` generates from the pinned sources, committed, and never edited by hand.

- Vite's `dedupe: ["prosemirror-model"]` keeps one copy of ProseMirror's model; two copies make rendering fail.

## Parser patches and bounds

- Four of the root `package.json`'s `patchedDependencies` make the markdown parsers linear: `micromark@4.0.2`, `micromark-core-commonmark@2.0.3`, `micromark-extension-gfm-footnote@2.1.0` and `mdast-util-from-markdown@2.0.3`.
  Each patch changes both `lib/` and `dev/lib/`, because Vite resolves the `development` export condition.

- The patches are keyed by version, so bumping a patched package means redoing its patch.
  A new micromark must also keep the tokenizer's `_closeFlow` and `resolveAll`, which `src/directive-container.ts` relies on.

- `tests/heavy-bodies.test.ts` and `tests/nested-directive.test.ts` hold two of the patches, reading CPU time through `tests/cpu-time.ts`.
  The `micromark` patch holds the lazy shapes (`a quote then lines carrying no marker`, `a list item then lines carrying no marker`), and the `micromark-core-commonmark` patch the bracket shapes (`a run of 15,000 brackets and as many closers`, `a typed fence whose label opens a run of brackets`).
  No test holds the other two patches, and a quadratic parse of a shape no row holds is caught by nothing.

- `maxBlockNesting` in `src/block-nesting.ts` equals `maxNesting` in `packages/envoy/internal/dispatch/pmdoc/nesting.go`; change both together.

- `tests/going-answer-no-link.test.ts` reads the golden `packages/envoy/going/testdata/filtered-answers.json`.
  After a change to that Go filter, rewrite it in `packages/envoy` with `go test ./going -run TestTheFilteredAnswersAreWhatTheFilterEmits -update`.
