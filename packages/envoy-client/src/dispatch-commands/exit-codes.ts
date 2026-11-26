// packages/envoy-client/src/dispatch-commands/exit-codes.ts
//
// The exit codes of every `dispatch` command, as `skill://dispatch` states them. It imports
// nothing, so `dispatch-cli.ts` and each command it runs can take them without an import cycle.

/** Done: written or read. */
export const OK = 0;

/** A refusal: a call Dispatch itself turned down (a near copy, an AskGate block, a server
 *  refusal), or text the CLI refuses in Dispatch's own refusal once the flags are read, before it
 *  asks: bytes that are not UTF-8 in a file or stdin a flag read, and a Markdown document's text
 *  past the bytes one document holds or holding U+0000 or a byte that is not UTF-8, given inline,
 *  read from a file or stdin, or in a Markdown `--path` file. */
export const REFUSED = 1;

/** A usage error: any other argument the CLI refuses before any request, whether the argv named
 *  a command or not. */
export const USAGE = 2;
