/** How Define's citation names the block it cites (INFRA-3231's R7, as the architect ruled): by
 *  what the block carries - its own label where it is a typed block with one, the bold lead of a
 *  requirement's or a check's first paragraph up to its first full stop ("R3" of
 *  "**R3. Flip audit.**"), else the text of the heading it stands under, and "block" only where
 *  it has neither. Only Define's citation reads it; every other reference keeps `shortForm`. */

/** One block of a document as its text read with blocks gives it: where its markdown runs. */
export interface CitedBlock {
  readonly id: string;
  readonly type: string;
  readonly from: number;
  readonly to: number;
}

/** The typed blocks whose first paragraph opens with their own label. */
const LABELLED_TYPES: Readonly<Record<string, true>> = {
  check: true,
  plan_task: true,
  requirement: true,
};

/** The bold lead of a typed block's first paragraph up to its first full stop, or undefined
 *  where the paragraph opens with no bold. */
function ownLabel(markdown: string): string | undefined {
  const lead = /^\*\*(.+?)\*\*/m.exec(markdown.replace(/^:::[^\n]*\n/, ""))?.[1];
  if (lead === undefined) return undefined;
  const label = lead.split(/\.(\s|$)/)[0]?.trim() ?? "";
  return label === "" ? undefined : label;
}

/** The label of the block `blockId` of `markdown`, whose blocks are `blocks`. */
export function citationLabel(
  markdown: string,
  blocks: readonly CitedBlock[],
  blockId: string
): string {
  const ordered = [...blocks].sort((first, second) => first.from - second.from);
  const index = ordered.findIndex((block) => block.id === blockId);
  const cited = ordered[index];
  if (cited === undefined) return "block";
  if (LABELLED_TYPES[cited.type] === true) {
    const own = ownLabel(markdown.slice(cited.from, cited.to));
    if (own !== undefined) return own;
  }
  for (let at = index; at >= 0; at -= 1) {
    const block = ordered[at];
    if (block?.type !== "heading") continue;
    const text = markdown
      .slice(block.from, block.to)
      .replace(/^#+\s*/, "")
      .trim();
    if (text !== "") return text;
  }
  return "block";
}
