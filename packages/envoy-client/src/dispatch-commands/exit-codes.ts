// packages/envoy-client/src/dispatch-commands/exit-codes.ts
//
// The exit codes of every `dispatch` command: written or read, a call that did not do what it
// asks, and a usage error. It imports nothing, so `dispatch-cli.ts` and each command it runs can
// take them without an import cycle.

/** Done: written or read. */
export const OK = 0;

/** A call that did not do what it asks: a refusal or failure of Dispatch, or a refusal of AskGate
 *  (a near copy, an AskGate block, a server refusal, a 5xx that Dispatch answers:
 *  DispatchServiceError). It is also a Dispatch the command cannot reach
 *  (DispatchUnreachableError), a trade at the secrets broker that fails and leaves the call no
 *  token to present, and Dispatch keeping no answer for a call that writes nothing
 *  (DispatchAnswerNotKeptError), or text the CLI refuses in Dispatch's own refusal once the flags
 *  are read, before it asks: bytes that are not UTF-8 in a file or stdin a flag read, and a
 *  Markdown document's text past the bytes one document holds or holding U+0000 or a byte that is
 *  not UTF-8, given inline, read from a file or stdin, or in a Markdown `--path` file. */
export const REFUSED = 1;

/** A usage error: any other argument the CLI refuses before any request, whether the argv named
 *  a command or not, and a session or a configuration the command cannot use: a refusal of
 *  hostSession (DISPATCH_HOST or the session id unset), of hostedDispatch (Dispatch turned off, an
 *  envoy.json it cannot read) or of unhostedDispatch (`dispatch api` where no host runs it, with
 *  no Dispatch or no credential set), and a corrupted state.json. */
export const USAGE = 2;
