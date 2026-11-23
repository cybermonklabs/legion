# @legion/proof-editor

Dispatch's document editor: the Milkdown/ProseMirror entry point (`src/lib.ts`), the DOM-free parse and serialize engine (`src/lib-headless.ts`), the typed-block grammar and block ids.
Its consumers are `packages/dispatch` and the pmdoc fixture generator.
There is no build step: `exports` serves `src/*.ts`, which Bun runs and Vite compiles.
`docs/copied-files.md` says which files are copies and how they may change;
`docs/upstream.md` says how the fork's modules are pinned and resolved, and which dependency patches keep the markdown parsers linear.

## Rules

- Most of `src/` is copied from the `sjawhar/proof-sdk` fork:
  edit a copied file only in the kinds `docs/copied-files.md` lists, and as little as the change needs;
  put new behaviour in a module of legion's own that the copy imports.

- A new file of legion's own joins the names the first override of the root `biome.json` excludes, so that Biome checks it.

- `src/` imports the fork's modules as `proof-sdk-upstream/src/…`, and no TypeScript program may open those sources:
  tsc reads the generated `upstream/` declarations, which are never edited by hand.

- Bumping a package that one of the root `package.json`'s `patchedDependencies` patches means redoing its patch;
  a new micromark must also keep the tokenizer's `_closeFlow` and `resolveAll`, which `src/directive-container.ts` relies on.

- `maxBlockNesting` in `src/block-nesting.ts` equals `maxNesting` in `packages/envoy/internal/dispatch/pmdoc/nesting.go`; change both together.
