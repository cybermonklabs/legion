import { type IssueRoute, parseDispatchReference } from "./routes";

/**
 * The address and the words of a time the command bar's agent writes in its words
 * (`dispatch-time.tsx`): the instant its `dispatch-time:` address names, and the issue its words
 * name where they are that issue's key. A Markdown body's drawing (`markdown-serializer.ts`,
 * `MarkdownBody`) and the page's plain words (`TimedWords`) read a time link by these alone, so
 * the two draw one link alike. They draw nothing themselves, so the Markdown drawing reads them
 * without the chip `TimedWords` draws.
 */

/** The scheme of a time's address. */
const DISPATCH_TIME = "dispatch-time:";

/** The attribute a Markdown body's drawing of a time link carries (`markdown-serializer.ts`), by
 *  which the body finds the time links it drew: one whose words are an issue's key draws as that
 *  issue's chip in an answer of the command bar (`MarkdownBody`, `timeLinkIssue`). */
export const TIME_LINK_ATTRIBUTE = "data-dispatch-time";

/** An instant as RFC 3339 writes it, which is how the agent is told to write one: a date, a time
 *  to the minute or finer, and its zone. */
const INSTANT = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})$/u;

/** Whether `href` is a time's address, whether or not it names an instant. */
export function isTimeAddress(href: unknown): href is string {
  return typeof href === "string" && href.trim().toLowerCase().startsWith(DISPATCH_TIME);
}

/** The instant a time's address names, as it writes it; undefined for an address that names
 *  none, such as `dispatch-time:last week`. */
export function instantOf(href: string): string | undefined {
  const written = href.trim().slice(DISPATCH_TIME.length);
  return INSTANT.test(written) && !Number.isNaN(Date.parse(written)) ? written : undefined;
}

/**
 * The issue a time link's words name, where they are an issue's key and nothing else: the command
 * bar's agent wrote when DEMO-17 last moved as `[DEMO-17](dispatch-time:2026-10-10T09:00:00Z)`,
 * and the words name the item, so the page draws the item's chip, the time on its hover, rather
 * than lose the item to a time (`TimedWords`, `MarkdownBody`). A reference of the key and nothing
 * more names that issue, and one of a key with anything after it names something else or nothing.
 * The words are the page's text, so they are percent-encoded before they make a route: a key
 * (letters, digits and a hyphen) encodes as itself, and words with a slash, a hash or any other
 * character encode to a key no issue has, so they stay a time.
 */
export function timeLinkIssue(words: string): Extract<IssueRoute, { kind: "issue" }> | undefined {
  const route = parseDispatchReference(`dispatch://${encodeURIComponent(words.trim())}`);
  return route?.kind === "issue" ? route : undefined;
}
