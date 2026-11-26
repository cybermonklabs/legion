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
  `components/one-definition.test.ts` refuses a page's own search box, `<table>`, panel, hand-written control mark and value import of the table library, but not a `<select>`.
  In the seven features it names in `ADDRESS_FEATURES`, it also refuses every read or write of the address that goes round `lib/page-address.ts`.

  - `components/ControlRow.tsx`: the one control bar a page's controls sit in, one line that never wraps and never grows.
    It takes each control in a slot of its own kind, as that control's props and never as an element: `search`, `switch`, `trigger` with `clear`, `chips`, `actions`, `hint`, `columns`, `display` and `main`.
    It draws each shared control itself, at the page's `size`, and reads its own width: below 640 px the chips leave the line, the actions move into **More**, and **Filter**, the display menu, **More** and the main action show their glyphs alone.

  - `components/SearchBox.tsx`: the search box of a list narrowed by typed words, with the key that focuses it as its `hint`.

  - `components/SegmentedControl.tsx`: the switch after the bar's search box, two or more parts of one list, one of them chosen (`aria-pressed`).

  - `components/PanelTrigger.tsx`: the button that opens a menu, a filter panel or a column filter; the bar draws it in its quiet look (`BarLook`, `components/bar-look.tsx`).

  - `components/ViewPanel.tsx`: the trigger of a page's own panel that narrows nothing, such as an action's panel or the (i) hint.

  - `components/BarAction.tsx`: an action of the bar, a quiet button, refused with its refusal shown where it carries one, and the page's one main action drawn filled.

  - `components/MoreMenu.tsx`: the bar's **More** (⋯), whose items are the actions it holds, check items among them.

  - `components/DisplayMenu.tsx`: the display choices at the bar's end, under the label the page gives it.

  - `components/ColumnFilter.tsx`: the filter on a table column's header.

  - `components/ColumnsMenu.tsx`: which of a table's columns are shown.

  - `components/SelectionBar.tsx`: the bar shown while rows are marked.

  - `components/TableShell.tsx`: a table with no search, sort or filter; `DataTable` is the one with them.

  - `components/DataTable.tsx`: the table core, over TanStack Table.

  - `components/Pager.tsx`: a table's pager.

  - `components/Chip.tsx`: the interactive chip the controls draw: a toggle, a removable value, or the control row's `+N more`, which opens a panel.

- `lib/page-address.ts`: the module that reads and writes a page's narrowing in the address, for the seven features in `ADDRESS_FEATURES`; `features/architecture/` still reads the router itself.

- `components/focus-place.ts`: a component's own hand-off of a control a press takes out of it (`useFocusPlace`), armed by the reader's press at the component's root (`armOnPress`); `components/row-marker.ts` says what it treats as a row.

- `components/held-press.ts`: the hold on a control that cannot be pressed now: one whose press started a write or a read that is still out, a step with no page to go to (`StepButton.tsx`, `Pager.tsx`), or a control with nothing to do, such as Rename with no new name (`features/delivery/SessionGroupsEditor.tsx`) and Move with no epic staged (`features/project/issues-table-page.tsx`).
  A hold through a write's answer (`useHeldThroughAnswer`) keeps the armed hand-off of the press that started the write live (`holdHandOff`); a control held with nothing to do, before its press or once what answers its press has ended, keeps no press live (`components/focus-place.ts`).

- One primary per screen, chrome after it; type 22px, 15px, `text-sm`, `text-xs`; spacing in 4px steps; colour means state.

- Show an agent only through `features/refs/AgentName.tsx`, or `useAgentText()` for a string; lint misses a namespace import (`import * as`), so review for it.

- Editor: never write a class into editor DOM (ProseMirror reads it as an edit); use `ProofEditorHandle` decorations.
  Fix editor gaps in `packages/proof-editor`.

- Bumping a package that a file in the root `patches/` patches means redoing its patch: Bun skips a stale patch silently.

- Each route of `app.tsx` is in `packages/dispatch/e2e/review-routes.ts`, reviewed or excluded with a reason.

- In a unit test, silence `console.error` only with `quietConsoleError()` (`__tests__/act-warnings.ts` says why): a row's own spy or replacement turns the act rule off.

## Layout (paths under `web/src/`)

1. A component that shows several elements in a row names its groups, in a fixed order, in its doc comment. Each element is in one group.
2. When a row is too narrow, a whole group moves to the next line. The entries of a group may wrap inside it. Text that does not fit truncates inside its own element, with the whole text as its title, or wraps inside it. It never pushes the next element out.
3. A list of chips that comes from data shows at most 10 entries while it is closed, then "+N", which shows the rest in place. `components/CappedGroup.tsx` does this and marks the list `data-layout-group`.
4. Nothing scrolls sideways, and nothing is cut off sideways without an ellipsis. A code block, a picture, a data table and a box marked `data-layout-scroll="board"` may scroll inside their own box.
5. An element is in a header or a row only when a reader acts on it there, or when it says that something is wrong now. Every other property is in the component's Details, as a label and a value. Nothing is removed.
6. Nothing appears, hides or moves because of a state the reader cannot see or set. The reader sets the width and opens each disclosure.

`bun run e2e:layout` checks points 3 and 4, and the cut text of point 2, at 360, 900, 1280, 1440 and 1920 px. A reviewer checks the other points.
