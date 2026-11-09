import { partyLink } from "./ref-picker";
import {
  buildDispatchReference,
  type DispatchReferenceRoute,
  type PartyRoute,
  sessionOfAddress,
} from "./routes";
import { readTextReferences } from "./text-references";

/**
 * Who a draft addresses, read out of its own words. A mention is a reference (INFRA-1702 R4):
 * the party is written into the text as a link whose words are its name and whose address names
 * it, and nothing at all is kept beside the text. So every surface that asks who a draft names -
 * the delivery chips under a box, the composer's delivery notices, whether a typed `/btw` is a
 * command, and the request Send makes - asks here, and this asks the one reader of what a text
 * cites (`readTextReferences`), the twin of the server's own. What the writer is told and what
 * the server will read can therefore never be two readings of the same draft.
 */

/** Whether a reference names a party: an agent's session, a role, or a person. */
export function isPartyRoute(route: DispatchReferenceRoute): route is PartyRoute {
  return route.kind === "agent-session" || route.kind === "role" || route.kind === "person";
}

/** The delivery address a party is reached at - `session:<id>` or `role:<token>` - or undefined
 *  for a person: a person reads the record and is never a delivering party (R5). */
export function deliveryTargetOf(route: PartyRoute): string | undefined {
  switch (route.kind) {
    case "agent-session":
      return `session:${route.session}`;
    case "role":
      return `role:${route.role}`;
    case "person":
      return undefined;
  }
}

/** The party a delivery address names, or undefined for an address that names none. */
export function partyRouteOf(target: string): PartyRoute | undefined {
  const session = sessionOfAddress(target);
  if (session !== undefined) return { kind: "agent-session", session };
  return target.startsWith("role:")
    ? { kind: "role", role: target.slice("role:".length) }
    : undefined;
}

/** The mention of the party at `target`, written as the link that names it: `title` for the
 *  words, or the address itself where the party has no name that fits on one line. Undefined
 *  for an address that names no party, which is then mentioned by nothing. */
export function partyMention(target: string, title: string): string | undefined {
  const route = partyRouteOf(target);
  if (route === undefined) return undefined;
  const named = title.trim();
  const words = named === "" || /[\r\n]/.test(named) ? target : named;
  return partyLink(words, buildDispatchReference(route));
}

/** What a draft's text says about who it addresses. */
export interface DraftParties {
  /** Every delivering party the text names, in the order it names them and each once: who the
   *  write is delivered to, and what the delivery notices are drawn from. */
  readonly targets: readonly string[];
  /** Whether the text names a party at all, a person among them. Nothing is delivered to a
   *  person, but naming one is still addressing somebody: it is what makes a typed `/btw` the
   *  command it looks like rather than the reader's own first word. */
  readonly named: boolean;
}

/** The parties `body` names. */
export function draftParties(body: string): DraftParties {
  const targets: string[] = [];
  const seen = new Set<string>();
  let named = false;
  for (const found of readTextReferences(body).references) {
    const route = found.route;
    if (route === undefined || !isPartyRoute(route)) continue;
    named = true;
    const target = deliveryTargetOf(route);
    if (target === undefined || seen.has(target)) continue;
    seen.add(target);
    targets.push(target);
  }
  return { named, targets };
}

/** `[start, end)` widened over the inline link that wraps it, where one does. A mention's
 *  address is only half of what a reader sees, so a caller taking one out of the text must take
 *  its words with it rather than leave `[Planner](` behind. */
function linkAround(body: string, start: number, end: number): { end: number; start: number } {
  if (body.charAt(end) !== ")" || body.slice(start - 2, start) !== "](") return { end, start };
  // The words run back to the `[` that opens them. A bracket among them is escaped
  // (`partyLink`), and a line break ends the search: no link's words hold one.
  for (let at = start - 3; at >= 0; at -= 1) {
    const character = body.charAt(at);
    if (character === "\n") break;
    if (character === "[" && body.charAt(at - 1) !== "\\") return { end: end + 1, start: at };
  }
  return { end, start };
}

/** Where `body` first mentions the party at `target`, taken whole - the link's words with its
 *  address - or undefined where it never does. */
export function partyMentionSpan(
  body: string,
  target: string
): { end: number; start: number } | undefined {
  for (const found of readTextReferences(body).references) {
    const route = found.route;
    if (route === undefined || !isPartyRoute(route) || deliveryTargetOf(route) !== target) {
      continue;
    }
    return linkAround(body, found.start, found.end);
  }
  return undefined;
}
