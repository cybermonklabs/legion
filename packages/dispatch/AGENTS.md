# Dispatch web app

The React app for Dispatch (`web/`) and its Playwright suite (`e2e/`).
The Go server's guide: `packages/envoy/cmd/dispatch/AGENTS.md`.
`README.md` has the commands, the end-to-end suite and the rules its rows follow, the stubbed layer, the review of each route, and the local instance;
`docs/testing.md` at the repository root has the rules a unit test follows.

## Code (paths under `web/src/`)

- API types come from `@legion/contracts`; `api/hand-written-shapes.md` lists the hand-written ones.

- Requests go through `api/client.ts`; every content write, through the store in `features/writes/`.

- Order server timestamps with `compareTimestamps` (`lib/timestamps.ts`), never as text or `Date.parse`.

- Query keys are literal arrays, reused for the same data; only `eventRefreshTargets` (`api/event-refresh.ts`) maps events to refreshes.

- Keys: `useKeymap` (`features/shell/keymap.ts`); list a new scope in `ShortcutHelp.tsx`'s `SCOPE_ORDER` and `SCOPE_LABELS`, or `?` drops it.

- Colour: import a composite from `theme/classes.ts`; a new pair is a new static composite there.

- Truncate inside a flex control with `components/TruncatedText.tsx`.

- Search, sort and filter controls: a page draws these.
  `components/one-definition.test.ts` refuses a page's own search box, `<table>`, panel and hand-written control mark, but not a `<select>`.

  - `components/ControlRow.tsx`: the one row a page's controls sit in.

  - `components/SearchBox.tsx`: the search box of a list narrowed by typed words.

  - `components/PanelTrigger.tsx`: the button that opens a menu, a filter panel or a column filter.

  - `components/DisplayMenu.tsx`: the display choices at the row's end.

  - `components/ColumnFilter.tsx`: the filter on a table column's header.

  - `components/ColumnsMenu.tsx`: which of a table's columns are shown.

  - `components/SelectionBar.tsx`: the bar shown while rows are marked.

  - `components/TableShell.tsx`: the markup of every table.

  - `components/DataTable.tsx`: the table core, over TanStack Table.

  - `components/Pager.tsx`: a table's pager.

  - `components/Chip.tsx`: a narrowing that is on.

- `lib/page-address.ts`: the one module that reads and writes a page's narrowing in the address.

- `components/focus-place.ts`: the shell's one focus rescue; `components/row-marker.ts` says what it treats as a row.

- One primary per screen, chrome after it; type 22px, 15px, `text-sm`, `text-xs`; spacing in 4px steps; colour means state.

- Show an agent only through `features/refs/AgentName.tsx`, or `useAgentText()` for a string; lint misses a namespace import (`import * as`), so review for it.

- Editor: never write a class into editor DOM (ProseMirror reads it as an edit); use `ProofEditorHandle` decorations.
  Fix editor gaps in `packages/proof-editor`.

- Bumping a package that a file in the root `patches/` patches means redoing its patch: Bun skips a stale patch silently.

- Each route of `app.tsx` is in `packages/dispatch/e2e/review-routes.ts`, reviewed or excluded with a reason.

- In a unit test, silence `console.error` only with `quietConsoleError()` (`__tests__/act-warnings.ts` says why): a row's own spy or replacement turns the act rule off.
