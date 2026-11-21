/**
 * The open-asks hook's two waits, which it runs one after the other: jj's own user config chain
 * for the pane's attribution overlay, then the Dispatch read of the session's open asks. Claude
 * Code kills the hook at the `timeout` hooks.json gives `open-asks`, and the session then loses its
 * exports and its summary, so the two must end inside it: tests/open-asks-hook.test.ts holds their
 * sum below that timeout. A module of its own so that the test can import them: session-hook.ts
 * runs the hook when it is loaded.
 */

/** The bound on resolving jj's own user config chain for the overlay. Not
 *  `JJ_CONFIG_PATH_TIMEOUT_MS`, which is the Oh My Pi lane's bound inside the 30 s that harness
 *  gives its session-start handler. A jj that never answers costs the session the export and
 *  nothing else. */
export const JJ_BASE_CHAIN_TIMEOUT_MS = 3_000

/** The bound on the Dispatch read of the session's open asks. */
export const OPEN_ASKS_TIMEOUT_MS = 3_000
