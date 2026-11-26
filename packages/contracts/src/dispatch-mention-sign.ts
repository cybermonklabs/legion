/**
 * The sign a mention's words open with, and the words a mention of a party draws: the part of
 * the mention rule (`dispatch-mentions.ts`) that writes a mention rather than reads one. A module
 * of its own that imports nothing: the dashboard writes a kept draft's old mentions back as
 * links in the entry chunk every page parses first
 * (`packages/dispatch/web/src/features/refs/party-mention.ts`), and the readers of the rule stay
 * out of that chunk.
 */

/** The sign a mention's words open with. */
export const MENTION_SIGN = "@";

/** The words a mention of a party named `name` draws: the mention sign, then the name. */
export function mentionName(name: string): string {
  return `${MENTION_SIGN}${name}`;
}
