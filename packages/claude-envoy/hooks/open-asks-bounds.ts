// The open-asks hook's own bounds, in a module of their own so a test can read them: importing
// session-hook.ts runs the hook. hooks.json gives that hook 20 s, and Claude Code kills it past
// that, losing the summary; tests/hooks-timeout.test.ts holds these bounds, with the `!` token
// command's (envoy-client's `commandTimeoutMs`), inside it.

/** The bound on the Dispatch read of the session's open asks. */
export const OPEN_ASKS_TIMEOUT_MS = 3_000

/** The bound on resolving jj's own user config chain for the overlay, inside the 20 s hooks.json
 *  gives this hook: 3 s of jj and then 3 s of the Dispatch read (`OPEN_ASKS_TIMEOUT_MS`) still
 *  leave the session its summary. Not `JJ_CONFIG_PATH_TIMEOUT_MS`, which is the Oh My Pi lane's
 *  bound inside the 30 s that harness gives its session-start handler. A jj that never answers
 *  costs the session the export and nothing else. */
export const JJ_BASE_CHAIN_TIMEOUT_MS = 3_000
