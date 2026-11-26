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

- Colour: import a composite from `theme/classes.ts`, or `theme/shell-classes.ts` for strings the entry chunk draws; a new pair is a new static composite there.

- Truncate inside a flex control with `components/TruncatedText.tsx`.

- One primary per screen, chrome after it; type 22px, 15px, `text-sm`, `text-xs`; spacing in 4px steps; colour means state.

- Show an agent only through `features/refs/AgentName.tsx`, or `useAgentText()` for a string; lint misses a namespace import (`import * as`), so review for it.
  The rail, which the entry chunk carries, imports that module's session half alone (`features/refs/SessionName.tsx`, which says why).

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

## Browser CI

The pull-request lane runs the `@pr` rows on two legs: `real` starts the stack, and `stub`
replays recordings against the downloaded dashboard without a stack. The counting-embedder
rows run separately after `real` on pull requests and after the wall-clock rows on `full`.
See [the browser CI notes](../../.github/ci-notes/dispatch-browser.md) for commands and budgets.
