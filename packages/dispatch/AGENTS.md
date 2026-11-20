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

- Show an agent only through `features/refs/AgentName.tsx`, or `useAgentText()` for a string.

- Editor: never write a class into editor DOM (ProseMirror reads it as an edit); use `ProofEditorHandle` decorations.
  Fix editor gaps in `packages/proof-editor`.

- Bumping a package that a file in the root `patches/` patches means redoing its patch: Bun skips a stale patch silently.

- Each route of `app.tsx` is in `e2e/review-routes.ts`, reviewed or excluded with a reason.
