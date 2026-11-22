# Dispatch web app

- Run one unit test file with `bun run test:web <file>` from `packages/dispatch`: a bare `bun test <file>` loads no DOM.

- Order two server timestamps with `compareTimestamps`, never as text or by `Date.parse`.
  Go writes them with no trailing fractional zeros and with microseconds, so the text does not sort and `Date.parse` drops digits.

- A query key is a literal array, and a read of data that some reader already caches uses that reader's key.
  An event refreshes a cached read only through `eventRefreshTargets`, which matches keys by prefix, so a read under a key of its own goes stale.

- Below 1280 px every link outside prose and every button is an inline-flex box, where `truncate` draws no ellipsis: put the text in `TruncatedText`.

- Every screen has one primary, the thing it exists for; chrome (identity, provenance, rare actions) goes after it, never above it.
  Colour means state, never decoration.

- Show an agent only through `AgentName`, or `useAgentText()` for a string.
  Biome refuses a named import of the contracts naming functions but not a namespace import (`import * as`), so never reach them through one.

- In a unit test, silence `console.error` only with `quietConsoleError()`.
  A row's own spy that swallows calls, or a replacement, drops React's act messages before the act rule reads them, and the row passes.

Where this file and the code disagree, the code is right: follow the code and correct this file.
