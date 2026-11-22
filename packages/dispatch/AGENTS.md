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

- One primary per screen, chrome after it; type 22px, 15px, `text-sm`, `text-xs`; spacing in 4px steps; colour means state.

- Show an agent only through `features/refs/AgentName.tsx`, or `useAgentText()` for a string; lint misses a namespace import (`import * as`), so review for it.

- Editor: never write a class into editor DOM (ProseMirror reads it as an edit); use `ProofEditorHandle` decorations.
  Fix editor gaps in `packages/proof-editor`.

- Bumping a package that a file in the root `patches/` patches means redoing its patch: Bun skips a stale patch silently.

- Each route of `app.tsx` is in `packages/dispatch/e2e/review-routes.ts`, reviewed or excluded with a reason.

- In a unit test, silence `console.error` only with `quietConsoleError()` (`__tests__/act-warnings.ts` says why): a row's own spy or replacement turns the act rule off.

## Layout (paths under `web/src/`)

One rule for every row of elements.
`packages/dispatch/e2e/layout-routes.ts` lists every route the check reads, with the states a reader opens on it and the groups it must mark; `e2e/layout-rule.stub.e2e.ts` reads them against a recording of a stress seed on every pull request, `e2e/layout.e2e.ts` reads on the real stack the few a recording cannot draw.

1. A component that shows several elements in a row names its groups, in a fixed order, in its doc comment.
   Each element belongs to one group.

2. A row too narrow for its groups gives way one whole group at a time: the group moves to the next line as a whole; the entries inside a group may wrap within it.
   Text that does not fit truncates inside its own element (`components/TruncatedText.tsx`, with the whole text as its title) or wraps inside it; it never pushes the next element out.

3. A list of chips that comes from data (labels, components, pull requests, checks, links, roles, chosen agents) shows at most 10 entries while it is closed, then "+N", which shows the rest in place (INFRA-1196's R13, dispatch://INFRA-1196/spec#b-r-properties).
   A paged list (an Inbox band, a table) and a list a reader moves through (tabs, a machine's windows) are not such lists: a paged list pages, and a navigation list wraps and shows every entry.
   Use `components/CappedGroup.tsx`.
   The group carries `data-layout-group="<name>"` (`data-layout-max` when its cap is below 10, `data-layout-open` while the reader has it open); its "+N" control carries `data-layout-more`.

4. Nothing scrolls sideways, and nothing is cut off at a side without an ellipsis, at any width from 360 to 1920 px (INFRA-1196's R18, dispatch://INFRA-1196/spec#b-r-reflow); the check samples 360, 900, 1280, 1440 and 1920 px, and reads sideways only.
   Exempt, when it is what overflows the box: a code block (`pre`, `code`), a picture (`img`, `picture`, `canvas`, `video`; not an inline `svg` icon), a data table (a `table`, `role=table` or `role=grid`, its key columns pinned at the left) and a board's lanes (the box marked `data-layout-scroll="board"`).
   No other `overflow-x-auto` row, and no `flex-nowrap` or `overflow-hidden` row that can outgrow its box.

5. An element earns its place in a header or a row only when a reader acts on it there, or it says something is wrong now (a failing check, a route nobody holds).
   Every other property goes in the component's Details, as a label and a value; nothing is removed, so every property and control stays reachable.

6. Nothing appears, hides or moves on state the reader cannot see or set.
   Width is the reader's to set; a disclosure the reader opens is the reader's.

- `packages/dispatch/e2e/layout-breaches.ts` lists the breaches main still holds, each with the INFRA-5200 slice that removes it; a fix deletes its entries, and a new breach is fixed, not listed.
