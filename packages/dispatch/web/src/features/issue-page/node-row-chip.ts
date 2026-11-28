import type { DispatchReferenceRoute } from "../refs/routes";
import { referenceTargetKind } from "../refs/target-id";
import { NODE_TYPE_NAMES, PRESET_NAMES } from "./page-words";

/**
 * The words a chip names its target by, on the node's own line (the architect's ruling 5): the
 * target's short name where the model
 * holds one, and until then the target's first words, cut at a word within `CHIP_LIMIT`
 * characters, the ellipsis included. No address, slug or document name is drawn in its place
 * (R22): a target with no words of its own answers `""`, and its chip draws nothing to name it by.
 * The words it cuts are the plain-text projection's (`refs/reference-names.tsx`), so the cut is
 * the page's and the words are the one reader's.
 *
 * Pure, so a filter's own row (`decisions-filter/`, `spec-filter/`) and the browser suite name a
 * target the same way the outline's row does.
 */

/** How many characters a chip's words may hold, the ellipsis of a cut included. */
export const CHIP_LIMIT = 32;

/** The marks a cut never ends on before its ellipsis. */
const TRAILING_MARKS = /[\s,;:.!?·–—-]+$/u;

export function chipWords(name: string): string {
  const whole = name.replace(/\s+/gu, " ").trim();
  if (whole.length <= CHIP_LIMIT) return whole;
  // The ellipsis takes the last place. The character after the room says whether a word ends
  // exactly at its edge, which is a cut at a word as much as a space inside it is.
  const room = CHIP_LIMIT - 1;
  const window = whole.slice(0, room + 1);
  const space = window.lastIndexOf(" ");
  const kept = space <= 0 ? whole.slice(0, room) : whole.slice(0, space);
  const words = kept.replace(TRAILING_MARKS, "");
  return `${words === "" ? kept : words}…`;
}

/**
 * The page's word for the whole document `route` names, or undefined where it names no whole
 * document (part G's R4, ruling 5): a document's own name is its file name, which no chip draws
 * (decision 39), so a chip names a document by the page's word for one, and the spec of `issue`,
 * the issue the row belongs to, by the Spec's name. This is the rule the page's store names a
 * document by (`tree-drafts.ts`'s `documentWord`), as far as an address and the row's issue say
 * it: another issue's spec is a document as any other is, since another issue's document plays
 * no part of this one, and which of the issue's own documents plays the plan's, Verification's
 * or the decisions' part is the issue read's answer alone, so each is a document here. A tie's
 * chip and one in place in a node's words draw this where the page gave them no word of its own
 * yet (`refs/reference-names.tsx`'s `ReferenceNameReading.chipWord`), so another issue's spec
 * reads one word before the page's read answers and after. The page publishes a document's name
 * as a link to it reads it, which every plain line draws (`tree-drafts.ts`'s `treeContext`).
 */
export function documentWord(
  route: DispatchReferenceRoute | undefined,
  issue: string | undefined
): string | undefined {
  if (route === undefined || referenceTargetKind(route) === "block") return undefined;
  if (route.kind === "spec") {
    return route.key === issue ? PRESET_NAMES.spec : NODE_TYPE_NAMES.document;
  }
  return route.kind === "artifact" || route.kind === "document" || route.kind === "agent-artifact"
    ? NODE_TYPE_NAMES.document
    : undefined;
}
