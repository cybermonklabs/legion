// packages/broker-client/src/error-text.ts
//
// The words of an error this package wraps in a refusal of its own, so every module names a
// failure the same way.

/** What `error` says: its message where it is an `Error`, else its string form. */
export function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** A read error's code as the runtime sets it (`ENOENT`, `EACCES`): one word, never its text,
 *  which repeats the path it was handed. */
const READ_ERROR_CODE = /^[A-Z][A-Z0-9_]{0,31}$/;

/** Why a file read failed, as a text that must not name the file's path: its code, or "an error"
 *  where it has none. A path a setting names can be a secret pasted into that setting. */
export function readErrorCode(error: unknown): string {
  const { code } = (error ?? {}) as { code?: unknown };
  return typeof code === "string" && READ_ERROR_CODE.test(code) ? code : "an error";
}
