/**
 * The two waits the `open-asks` hook spends in series, in a module of its own so a test can
 * assert them: `session-hook.ts` runs its mode off `process.argv` at import, so importing that
 * file to read a constant would run a hook.
 *
 * Both are bounded by something outside this package: Claude Code kills the hook at the
 * `timeout` `hooks/hooks.json` declares for it (20 s for `open-asks`), and a hook it kills
 * writes no env file, so the session loses `DISPATCH_HOST`, `ENVOY_CONFIG_FILE` and its open-ask
 * summary. The two bounds below run one after the other on the slow path, so their sum is what
 * has to stay inside that timeout; `tests/open-asks-hook.test.ts` asserts it against the number
 * `hooks.json` itself carries.
 */

/** The bound on the Dispatch read that prints the session's open asks. A Dispatch that does not
 *  answer inside it costs the session the summary and nothing else. */
export const OPEN_ASKS_TIMEOUT_MS = 3_000

/** The bound on resolving jj's own user config chain for the overlay: 3 s of jj and then 3 s of
 *  the Dispatch read (`OPEN_ASKS_TIMEOUT_MS`) still leave the session its summary. Not
 *  `JJ_CONFIG_PATH_TIMEOUT_MS`, which is the Oh My Pi lane's bound inside the 30 s that harness
 *  gives its session-start handler. A jj that never answers costs the session the export and
 *  nothing else. */
export const JJ_BASE_CHAIN_TIMEOUT_MS = 3_000
