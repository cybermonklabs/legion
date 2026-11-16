/**
 * The code of the refusal `error` carries, or undefined for an error carrying none.
 *
 * A refusal Dispatch decided (`DispatchServiceError`, and the gateway answer that stands in for
 * one) carries the code of the first rule the call broke beside its message, and `fromDispatch`
 * is what tells such an error from a Node errno error, which carries a `code` of its own (ENOENT,
 * EPERM, …) that says nothing about what Dispatch decided. The check is structural rather than an
 * `instanceof`, because those classes live in `dispatch-http.ts`, which reads this module's
 * neighbours.
 */
function dispatchCode(error: Error): string | undefined {
  if (!("fromDispatch" in error) || !("code" in error)) return undefined;
  const code = error.code;
  return typeof code === "string" && code !== "" ? code : undefined;
}

/**
 * Coerce an unknown thrown value into a human-readable message.
 *
 * A refusal Dispatch decided reads `<CODE>: <message>`, because `skill://dispatch` has every agent
 * branch on an error's code rather than on its words, and the code was in no line the command
 * printed: a reader had to run the same call through curl to see it (reported by the tree of
 * dispatch://INFRA-1523). A message that already opens with its own code is left as it is, and
 * every other thrown value prints as it always did.
 */
export function messageFor(error: unknown): string {
  if (!(error instanceof Error)) return String(error);
  const code = dispatchCode(error);
  return code === undefined || error.message.startsWith(code)
    ? error.message
    : `${code}: ${error.message}`;
}

/** True when `error` is a Node errno error carrying `code` (ENOENT, EPERM, …). */
export function hasErrnoCode(error: unknown, code: string): boolean {
  return error instanceof Error && "code" in error && error.code === code;
}
