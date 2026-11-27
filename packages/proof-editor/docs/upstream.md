# The upstream proof-sdk modules

The modules of the `sjawhar/proof-sdk` fork that `src/` imports at run time, as `proof-sdk-upstream/src/…`, come from the tarball `proof-sdk-<commit>.tgz` beside `package.json`, installed as `proof-sdk-upstream`.
This page says how that pin moves, how each tool resolves those modules, and which dependency patches keep the markdown parsers linear.

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
Each patch changes both `lib/` and `dev/lib/`, because Vite resolves the `development` export condition.
`tests/heavy-bodies.test.ts` and `tests/nested-directive.test.ts` hold two of the patches, reading CPU time through `tests/cpu-time.ts`.
The `micromark` patch holds the lazy shapes (`a quote then lines carrying no marker`, `a list item then lines carrying no marker`), and the `micromark-core-commonmark` patch the bracket shapes (`a run of 15,000 brackets and as many closers`, `a typed fence whose label opens a run of brackets`).
No test holds the other two patches, and a quadratic parse of a shape no row holds is caught by nothing.

## Single-block collaboration

The root `patchedDependencies` pins a patch to `y-prosemirror@1.3.7`.
When an editor binds to a paragraph or heading's live `Y.XmlElement`, `Y.XmlText` can be a direct child of the bound root. Upstream treated every direct child as an element and tried to call `toArray()` on the text.
The patch uses the existing text-to-ProseMirror conversion for direct text children during the initial bind, remote updates, snapshot rendering and return from a snapshot. It records the text nodes in the binding's mapping for cursors.
The ESM export uses `src/plugins/sync-plugin.js`; the CommonJS export uses `dist/y-prosemirror.cjs`. Both receive the same change.
`src/tests/element-option.test.ts` exercises a live paragraph and heading at creation and rebind, including edits in both directions and the caret after a peer inserts text.
