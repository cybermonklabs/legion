// packages/envoy-client/src/dispatch-commands/exit-codes.ts
//
// The exit codes of every `dispatch` command, as `skill://dispatch` states them. It imports
// nothing, so `dispatch-cli.ts` and each command it runs can take them without an import cycle.

/** Done: written or read. */
export const OK = 0;

/** A refusal: a call Dispatch itself turned down (a near copy, an AskGate block, a server
 *  refusal). */
export const REFUSED = 1;

/** A usage error: an argument the CLI refuses before any request, whether the argv named a
 *  command or not. */
export const USAGE = 2;
