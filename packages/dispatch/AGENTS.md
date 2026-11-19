# Dispatch web app

The React app for Dispatch (`web/`) and its Playwright suite (`e2e/`).
The Go server's guide: `packages/envoy/cmd/dispatch/AGENTS.md`.
`README.md` covers the e2e harness and local instance.

## Commands (run from this package)

- `bun run dev`, `bun run build:web`, `bun run typecheck`, `bun run lint`, `bun run test`, `bun run dispatch:local` (a seeded, signed-in instance).
- One unit test file: `bun run test:web web/src/path/to/file.test.tsx`; a bare `bun test <file>` loads no DOM.
- End-to-end: a file, a wall-clock file, all wall-clock rows.

  ```bash
  bun run e2e -- e2e/claim.e2e.ts --project=chromium
  bun run e2e -- --project=chromium-alone e2e/heavy-bodies.e2e.ts --workers=1
  bun run e2e:clock
  ```

## Code (paths under `web/src/`)

- API types come from `@legion/contracts`; `api/hand-written-shapes.md` lists the hand-written ones.
- Requests go through `api/client.ts`; every content write, through the store in `features/writes/`.
- Order server timestamps with `compareTimestamps` (`lib/timestamps.ts`), never as text or `Date.parse`.
- Query keys are literal arrays, reused for the same data; only `eventRefreshTargets` (`api/event-refresh.ts`) maps events to refreshes.
- Keys: `useKeymap` (`features/shell/keymap.ts`); list a new scope in `ShortcutHelp.tsx`'s `SCOPE_ORDER` and `SCOPE_LABELS`, or `?` drops it.
- Colour: import a composite from `theme/classes.ts`; a new pair is a new static composite there.
- Truncate inside a flex control with `components/TruncatedText.tsx`.
- One primary per screen, chrome after it; type 22px, 15px, `text-sm`, `text-xs`; spacing in 4px steps; colour means state.
- Editor: never write a class into editor DOM (ProseMirror reads it as an edit); use `ProofEditorHandle` decorations. Fix editor gaps in `packages/proof-editor`.
- Root `patches/` patch `@milkdown/plugin-clipboard`, `@milkdown/preset-gfm` and `prosemirror-tables` at one version each; Bun skips a stale patch silently.

## How an agent is shown

- Only `features/refs/AgentName.tsx`, or `useAgentText()` for a string. Lint misses `import * as`; review for it.

## Unit tests

- An unanswered request fails the test (`web/src/__tests__/network.ts`): spy the `api` method, or `answerRequests(http.get(…))`.
- A React `act` warning fails the test: wrap updates in `act`, a promise in `await act(async …)`, and under fake timers give a held answer there after the press. Silence logs only with `quietConsoleError()`.
- Use `user-event` (`userOnFakeClock()` under fake timers). A `fireEvent` call needs a `biome-ignore` on the import and a reason comment above it.

## End-to-end tests

- `bun run e2e` builds the contracts and app, then a stack per worker. Bare `bunx playwright` needs `bun run build:contracts` first; `bun run e2e:install` gets browsers.
- One worker can use `DATABASE_URL` (a throwaway: every row empties it); more need `DISPATCH_E2E_PG_BIN` or `DISPATCH_E2E_PG_IMAGE`.
- Write a project as `--project=<name>` and never `--project <name>`, which reads the next file as a project.
- In an issue pod, run only the files you changed, and wall-clock rows in `chromium-alone`: the pod has no WebKit libraries.
- Real-stack specs take `test` from `e2e/test.ts`: it resets the database and signs in `alice`. Address the harness as `127.0.0.1:<port>`.
- If every row of a file fails at the reset's lock step, a session holds the tables: stop it.
- A row binds `page`, `browser` or `context`; other claims go in `e2e/__tests__/` or a Go test.
- A row takes under 2 s or carries `// SLOW(>=2s): <why>`; check a one-worker report with `bun e2e/tools/check-slow-rows.ts <report.json> --dir e2e`.
- Never branch on a one-instant read (`isVisible`, `count`): use `settledState` (`e2e/settled.ts`).
- Two rules keep an assertion honest: it fails with the change reverted, and what decides its scope (seeding, a selector) is asserted and seen failing once.
- Subprocesses: `runBounded` and `rowLimit(...)` (`e2e/__tests__/bounded-subprocess.ts`). Row files: `testInfo.outputPath()`.
- A markdown shape is a file in `packages/envoy/internal/dispatch/pmdoc/testdata/corpus/` plus `bun run gen` in `pmdoc/gen`, never a string in a spec.
- `*.multiprocess.e2e.ts` runs only in `bun run e2e:multiprocess -- <file>`.
- A real-stack row takes `@pr` (the pull-request lane) only with a `// @pr: <why>` line.
- Projects pick rows by title tag (`@webkit`, `@firefox`, `@phone`), so a lost tag fails nothing: check `bunx playwright test --config e2e/playwright.config.ts --project=<name> --list`.

## Phone acceptance

- `iphone` runs `@phone` rows (a phone width is under 768 px); it does not replace README's "Phone check".
- Tag `@phone` only where the phone can change what the row asserts: R1 layout, R2 touch (`e2e/touch.ts`), R3 a phone-only surface, R4 a phone-only row, R5 a request count `iphone` got wrong.

## The stubbed layer

- `bun run e2e:stub` runs `*.stub.e2e.ts` rows, declared with `stubbed(seed)`, against answers `bun run e2e:stub:record` recorded.
- A stubbed row asserts what the browser does with a fixed answer, and skips only in its body. Server decisions, own-write reads, server frames and server clocks stay real-stack.
- A new spec file needs a line in `e2e/stub/real-stack-reasons.ts`, then `bun e2e/tools/row-map.ts --write`.

## Accessibility and visual review

- Each route of `web/src/app.tsx` is in `e2e/review-routes.ts`, reviewed or excluded with a reason.
- Fix axe findings in `web/src`; `e2e/accessibility-exclusions.ts` is only for a fix that changes another row's claim.
- Baselines come only from the pinned container (`e2e/visual-run.sh --update-snapshots`). From a pod:
  1. Read the full-lane run that failed.
  2. `gh run download <run id> --repo <owner>/<repo> --name dispatch-browser-visual-<attempt>`.
  3. Each failing row's `<route>-<scheme>-actual.png` is under `packages/dispatch/e2e/test-results/`.
  4. Compare it with `e2e/__screenshots__/<project>/<route>-<scheme>.png`, then copy it over.
