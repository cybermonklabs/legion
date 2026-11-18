import { shellWords } from "./shell-words";

/** The first line of what the CLI prints when a write may have landed unanswered. */
export const MAY_HAVE_LANDED = "dispatch: this write may have landed";
/** A lower-case UUID, the key the CLI names. */
const NAMED_KEY = /idempotency key ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/;

/** The key and the command the CLI's last lines name for sending the write again: the line
 *  starts with MAY_HAVE_LANDED and ends with the command, which may run over more than one line
 *  where a quoted value holds a line break. Throws, showing `stdout`, when no line says so. */
export function resendOf(stdout: string): { readonly key: string; readonly words: string[] } {
  const at = stdout.indexOf(MAY_HAVE_LANDED);
  if (at < 0) throw new Error(`no line says the write may have landed:\n${stdout}`);
  const tail = stdout.slice(at).replace(/\n$/, "");
  const key = NAMED_KEY.exec(tail)?.[1];
  if (key === undefined) throw new Error(`the line names no key:\n${tail}`);
  return { key, words: shellWords(tail.slice(tail.indexOf(": dispatch ") + 2)) };
}
