import { mentionName } from "@legion/contracts/dispatch-mention-sign";

import type { PartyRoute } from "./routes";

/**
 * A party's delivery address and its mention, read and written without the grammar of
 * references (`routes.ts`): the session or role a `session:` or `role:` address names, the
 * address a party is reached at, the reference that names it, and the link that mentions it.
 * The entry chunk rewrites a kept draft's old mentions as these links as it reads the draft back
 * (`held-sends.ts`'s `restoredDraft`), so this module imports nothing of the grammar: what the
 * entry needs of a party's reference is here, and the grammar reads a party by the same rules.
 */

/** A role's token, the charset a `role:<token>` delivery route is written in (the server's
 *  `model.ParseRoute`). */
export const roleTokenOnlyPattern = /^[a-z0-9-]+$/;
/** A session id a `session:<id>` delivery route is written with, the server's `model.ParseRoute`
 *  again. It is narrower than the ids a reference may name (`SESSION_ID_PATTERN`, which takes
 *  `.` and `:` among the rest), so a body can write an agent address no route reaches. */
const sessionRouteIDPattern = /^[A-Za-z0-9_-]+$/;

/** The session id a `session:<id>` delivery address names, or undefined for any other address
 *  (a `role:` address). */
export function sessionOfAddress(address: string): string | undefined {
  return address.startsWith("session:") ? address.slice("session:".length) : undefined;
}

/** The party a delivery address names, written out kind by kind: `session:<id>` an agent's
 *  session and `role:<token>` a role, as the server's `model.ParseRoute` reads them
 *  (`internal/dispatch/model/model.go`), the grammar it holds every mention target to
 *  (`api.checkMentionRoute`). Undefined for an address of any other form, which no route
 *  reaches. A body can name a party outside this grammar - an agent reference takes the wider
 *  `SESSION_ID_PATTERN` - and such a party is reached by nothing, so the composer says so
 *  rather than listing it as one who is told (`party-references.ts`). A kind added to this
 *  grammar is named here or it is no party, rather than falling to whichever arm is last. */
export function partyOfAddress(address: string): PartyRoute | undefined {
  const session = sessionOfAddress(address);
  if (session !== undefined) {
    return sessionRouteIDPattern.test(session) ? { kind: "agent-session", session } : undefined;
  }
  if (address.startsWith("role:")) {
    const role = address.slice("role:".length);
    return roleTokenOnlyPattern.test(role) ? { kind: "role", role } : undefined;
  }
  return undefined;
}

/** The delivery address a party is reached at, or undefined for one no address reaches: a
 *  person, who reads the record and is never a delivering party (R5), and an agent whose
 *  session id is outside the grammar above. */
export function deliveryAddressOf(route: PartyRoute): string | undefined {
  switch (route.kind) {
    case "agent-session":
      return sessionRouteIDPattern.test(route.session) ? `session:${route.session}` : undefined;
    case "role":
      return roleTokenOnlyPattern.test(route.role) ? `role:${route.role}` : undefined;
    case "person":
      return undefined;
  }
}

/** The `dispatch://` reference that names `route`, the party arms of `buildDispatchReference`
 *  (`routes.ts`). A party is written as it is read, its name needing no encoding: a session id
 *  carries none of the characters a reference ends at, a role token is `[a-z0-9-]` and a login
 *  is a lowercased email. */
export function partyReference(route: PartyRoute): string {
  switch (route.kind) {
    case "agent-session":
      return `dispatch://agent/${route.session}`;
    case "role":
      return `dispatch://role/${route.role}`;
    case "person":
      return `dispatch://person/${route.login}`;
  }
}

/** What Markdown reads as markup in a link's words: the brackets that would end them, the
 *  backslash that escapes, the delimiters of emphasis, strong, code and strikethrough, which over
 *  a part of the words would make them their author's words rather than a mention
 *  (`@legion/contracts`'s `isMentionWords`), and the `<` and `&` that open inline HTML and a
 *  character reference: `Fix <Suspense> fallback` written as it is reads `<Suspense>` as a tag,
 *  which holds none of the words, and `&amp;` reads as `&`. */
const LINK_WORDS_MARKUP = /[[\]\\*_`~<&]/gu;

/** A party's name as a link's words, each character Markdown would read as markup written
 *  escaped: a session title is whatever its holder typed - `Planner [2]`, `fix *the* cache`,
 *  `Fix <Suspense> fallback` - and every reader of the link takes the escaped words as exactly
 *  those characters. */
function linkWords(label: string): string {
  return label.replace(LINK_WORDS_MARKUP, (character) => `\\${character}`);
}

/** The mention of a party: a link whose words are the mention sign and its name and whose
 *  address names it (R4), so the stored text reads as the person, session or role it mentions
 *  and still names it after an edit of the words. A link whose words start with the sign and
 *  whose address names a party reads as the sign and that party's current name wherever it is
 *  shown (`@legion/contracts`'s `mentionName`); a link whose words its author wrote another way
 *  keeps those words (R8). The name is written as literal words (`linkWords`), so a name that
 *  holds a formatting character is still a mention's plain words and reads as the party's
 *  current name after a rename. The one writer of that shape, read by the reference picker's
 *  pick (`ref-picker.ts`'s `insertionText`), by the live composer's (`ReferenceTriggers`) and
 *  by the seed a channel owes its own party and a kept draft's rewrite (`partyMention`). */
export function partyLink(name: string, reference: string): string {
  return `[${mentionName(linkWords(name))}](${reference})`;
}

/** The mention of the party at `target`, written as the link that names it: the mention sign
 *  and `title` for the words, or the sign and the address itself where the party has no name
 *  that fits on one line. Undefined for an address that names no party, which is then mentioned
 *  by nothing. */
export function partyMention(target: string, title: string): string | undefined {
  const route = partyOfAddress(target);
  if (route === undefined) return undefined;
  const named = title.trim();
  const words = named === "" || /[\r\n]/.test(named) ? target : named;
  return partyLink(words, partyReference(route));
}
