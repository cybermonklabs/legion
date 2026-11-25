/** The read a page's own script makes for the outside driver (`pageRead` of `outside-edge.ts`):
 *  one function, which the driver hands Playwright's `evaluate` and which runs in the page from its
 *  text, so it reads nothing from outside its own body. `outside-edge.test.ts` runs it in Bun
 *  against a listener that never answers. */

/** What a page's own JSON read got: its answer, its status alone where its body did not come
 *  (`cut`, why), or why no answer came. */
export type PageAnswer =
  | { readonly status: number; readonly body: unknown; readonly cut?: string }
  | { readonly unanswered: string };

/** Reads `at` with the page's origin and cookie, and waits for it until `until`, a `Date.now()`
 *  reading the driver took as it asked: the read's deadline is the driver's own clock, not the
 *  moment the page started the script. A page whose main thread was busy when the driver asked
 *  starts the script late, and with a deadline counted from its start the read would end past the
 *  driver's own wait for the script (`PAGE_SCRIPT_MS`), which then names the script, not the read;
 *  counted from the ask, it ends as the driver's 30 s do, and the driver's 5 s more are the
 *  answer's way back alone. A read that got no answer by then, or none at all, is `unanswered`,
 *  with why; one whose status came and whose body did not is that status, the far end's answer,
 *  with `cut` saying why the body did not come. */
export async function readInPage({
  at,
  until,
}: {
  readonly at: string;
  readonly until: number;
}): Promise<PageAnswer> {
  let status: number | undefined;
  try {
    const response = await fetch(at, {
      headers: { Accept: "application/json" },
      signal: AbortSignal.timeout(Math.max(0, until - Date.now())),
    });
    status = response.status;
    const text = await response.text();
    let body: unknown = text;
    try {
      body = JSON.parse(text);
    } catch {
      // Not JSON: the text itself is what a refusal says.
    }
    return { status, body };
  } catch (error) {
    const why = error instanceof Error ? `${error.name}: ${error.message}` : String(error);
    return status === undefined ? { unanswered: why } : { status, body: undefined, cut: why };
  }
}
