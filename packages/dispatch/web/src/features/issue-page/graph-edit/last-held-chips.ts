import { readTextReferences } from "../../refs/text-references";
import { wordsOf } from "../../refs/tree-fragment";
import type { IssueTreeReads } from "../issue-tree";
import { type Draft, lastHeldOf, type TreeContext } from "../tree-drafts";
import { lastHeldPath } from "./last-held";

/**
 * The tree's chips and the page's published map for a citation of a node the batched reference
 * read answers gone with the version that last held it (R9 of INFRA-5705's spec): the model's
 * side of `last-held.ts`, which the tree (`issue-tree.ts`' `buildIssueTree`) and the page's names
 * read (`useIssueTree.ts`) call, so both read a target's `last_held` by the one rule a chip
 * carries it by (`tree-drafts.ts`' `lastHeldOf`).
 */

/**
 * A chip for each node a draft's own words cite that the batched reference read answers gone
 * with the version that last held it (R9 of INFRA-5705): the chip `chipOf` makes from that
 * answer (`NodeChip.lastHeld`), which the row's `Chip` draws as the page's gone word linked to
 * that version (`NodeRow.tsx`), as the row's words draw the same reference in place
 * (`RowText.tsx`), so a comment citing a node deleted in Graph reads it on a chip as well. The
 * words are read as the page's names read reads the node the draft becomes (`issue-tree.ts`'
 * `nodeWords`, through `citedAddresses`), so each reference is the one the read answered under,
 * and nothing is read but that answer. A reference the read answers live, ambiguous, gone with
 * no version, or gone with one the page cannot open (`lastHeldPath`) earns no chip, and neither
 * does one that a chip of the draft already names, however often the words cite it: the draft's
 * own chips stand as they are, and each chip added goes after them. Until the read answers,
 * nothing is gone, and no draft gains one.
 */
export function addLastHeldChips(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  if (!(reads.targets ?? []).some((target) => lastHeldOf(target) !== undefined)) return;
  for (const draft of drafts.values()) {
    // The words `nodeWords` reads of the node `assemble` makes of the draft, its text and reading.
    const words = draft.tree === undefined ? draft.text : wordsOf(draft.tree);
    for (const reference of citedAddresses(words)) {
      const chip = context.chipOf(reference);
      if (chip.ambiguous === true || lastHeldPath(chip.lastHeld) === undefined) continue;
      if (draft.chips.some((held) => held.ref === reference)) continue;
      draft.chips = [...draft.chips, chip];
    }
  }
}

/** How many drafts' words the page keeps the cited addresses of (`citedAddresses`). The largest
 *  issue the page draws holds 2,106 blocks over all its documents, 86 comments and 11 messages
 *  (`row-references.ts`' `READINGS_KEPT`), and the tree is built from the outline read and from
 *  each view read: the cap keeps the words of an outline and a view of that issue whole, with
 *  room to spare. */
const CITED_KEPT = 8_192;

/** Each text's cited addresses, with the origin they were read against, the text read first
 *  first: the one dropped when a new text comes past `CITED_KEPT`. */
const citedByWords = new Map<
  string,
  { readonly addresses: readonly string[]; readonly origin: string }
>();

/** Every address `words` cite, as `row-references.ts`' `citedReferences` reads them for the
 *  page's names read, which this module cannot import: `row-references.ts` imports
 *  `issue-tree.ts`, which imports this one. A bare key and a question are left out: neither
 *  names a block, the one kind of target the read names a version of. Read once for each text and
 *  origin while it is kept (`CITED_KEPT`): the tree is built again with every names answer and
 *  read refresh, and reads every draft's words each time. */
function citedAddresses(words: string): readonly string[] {
  if (words === "") return [];
  const origin = window.location.origin;
  const kept = citedByWords.get(words);
  if (kept?.origin === origin) return kept.addresses;
  const cited = new Set<string>();
  for (const found of readTextReferences(words, { origin }).references) {
    if (found.form === "address" && found.reference !== undefined) cited.add(found.reference);
  }
  const addresses = [...cited];
  if (kept === undefined && citedByWords.size >= CITED_KEPT) {
    const oldest = citedByWords.keys().next();
    if (oldest.done !== true) citedByWords.delete(oldest.value);
  }
  citedByWords.set(words, { addresses, origin });
  return addresses;
}

/**
 * What the page publishes beside its names for every link beside the tree to draw
 * (`last-held-store.ts`' `usePublishLastHeldReferences`): each target of the batched reference
 * read (`reads.targets`) that is gone, by its address, with the version that last held it where
 * the read names one, read by the rule a chip naming the target carries it by (`lastHeldOf`,
 * `NodeChip.lastHeld`), so a chip and a reference in a node's words open one version (R9 of
 * INFRA-5705), and undefined where it names none. A short reference the read answered ambiguous
 * is left out: it names no one target, gone or not (`NodeChip.ambiguous`). No reference in it is
 * among `targetNaming`'s `readings`: a gone target is named by nothing.
 */
export function targetLastHeld(
  targets: IssueTreeReads["targets"]
): ReadonlyMap<string, string | undefined> {
  const lastHeld = new Map<string, string | undefined>();
  for (const target of targets ?? []) {
    if (target.gone && target.ambiguous !== true) lastHeld.set(target.ref, lastHeldOf(target));
  }
  return lastHeld;
}
