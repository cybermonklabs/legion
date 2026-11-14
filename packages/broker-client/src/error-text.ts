// packages/broker-client/src/error-text.ts
//
// The words of an error this package wraps in a refusal of its own, so every module names a
// failure the same way.

/** What `error` says: its message where it is an `Error`, else its string form. */
export function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
