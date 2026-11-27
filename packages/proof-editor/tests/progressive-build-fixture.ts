// What the rows of `progressive-build.test.ts` and `progressive-build-host.test.ts` share: the
// document they build, and what they read the editor and each batch by.

/** Blocks enough that the build takes several batches: the first takes eight and each one after
 *  it is sized by what the one before cost, growing fourfold at most. */
export const BLOCKS = 160;

/** The words the paragraph at `index` of the generated document is made of. */
export function paragraph(index: number): string {
  return `Paragraph ${index} of the ledger.`;
}

/** A document of `blocks` paragraphs, each with words of its own so a test can name one. */
export function paragraphs(blocks: number): string {
  return `${Array.from({ length: blocks }, (_, index) => paragraph(index)).join("\n\n")}\n`;
}

/** The blocks the editor's root holds, at whatever depth. */
export function blocksBuilt(root: HTMLElement): number {
  return root.querySelectorAll("[data-block-id]").length;
}

/** What one batch told the host: the ids it was about to build, how many blocks the editor held
 *  when it said so, and how many it held once they were in. */
export interface Batch {
  readonly after: number;
  readonly before: number;
  readonly ids: readonly string[];
}
