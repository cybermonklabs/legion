import type { Node as ProseMirrorNode } from "prosemirror-model";

import type { MarkPlacement } from "../margin/useMarginItems";

const recordMarkTypes = ["proofComment", "proofSuggestion", "dispatchAsk"] as const;

type SelectionBarKind = "comment" | "suggest" | "ask";

export function markPlacements(
  doc: ProseMirrorNode,
  offsets: ReadonlyMap<string, number>
): Map<string, MarkPlacement> {
  const placements = new Map<string, MarkPlacement>();
  doc.descendants((node, pos) => {
    if (!node.isText) {
      return true;
    }
    for (const mark of node.marks) {
      if (
        recordMarkTypes.includes(mark.type.name as (typeof recordMarkTypes)[number]) &&
        typeof mark.attrs.id === "string" &&
        !placements.has(mark.attrs.id)
      ) {
        const top = offsets.get(mark.attrs.id);
        if (top !== undefined) {
          placements.set(mark.attrs.id, { pos, top });
        }
      }
    }
    return true;
  });
  return placements;
}

/**
 * The ids of the pending insert suggestions whose marks lie on any text of `doc` between `from`
 * and `to`, each once, in document order: the editor's `proofSuggestion` marks of kind `insert`.
 * An insert's words are its comment's own, and the comment's delete takes them out of the
 * document, so words selected there are a deleted post's once that comment is deleted
 * (`ComposerAnchor.insertMarks`). An accept takes the insert's mark off its words, so every such
 * mark is a pending one. Empty where the range holds none: the words lie in no insert. A draft an
 * older build kept names no list at all, which says something else (`useQuoteInDocument`).
 */
export function insertMarksIn(doc: ProseMirrorNode, from: number, to: number): string[] {
  const ids: string[] = [];
  doc.nodesBetween(from, to, (node) => {
    if (!node.isText) {
      return true;
    }
    for (const mark of node.marks) {
      const id = mark.attrs.id;
      if (
        mark.type.name === "proofSuggestion" &&
        mark.attrs.kind === "insert" &&
        typeof id === "string" &&
        !ids.includes(id)
      ) {
        ids.push(id);
      }
    }
    return false;
  });
  return ids;
}

export function blockOffsets(root: HTMLElement): Map<string, number> {
  const offsets = new Map<string, number>();
  const rootTop = root.getBoundingClientRect().top;
  for (const block of root.querySelectorAll<HTMLElement>("[data-block-id]")) {
    const blockId = block.dataset.blockId;
    if (blockId !== undefined && !offsets.has(blockId)) {
      offsets.set(blockId, block.getBoundingClientRect().top - rootTop);
    }
  }
  return offsets;
}

export function blockPlacements(
  doc: ProseMirrorNode,
  offsets: ReadonlyMap<string, number>
): Map<string, MarkPlacement> {
  const placements = new Map<string, MarkPlacement>();
  doc.descendants((node, pos) => {
    const blockId = node.attrs.blockId;
    if (typeof blockId === "string" && !placements.has(blockId)) {
      const top = offsets.get(blockId);
      if (top !== undefined) {
        placements.set(blockId, { pos, top });
      }
    }
    return true;
  });
  return placements;
}

export function composerKindFor(kind: SelectionBarKind): "comment" | "suggestion" | "ask" {
  if (kind === "suggest") {
    return "suggestion";
  }
  return kind;
}

export function selectionBarKindFor(kind: "comment" | "suggestion" | "ask"): SelectionBarKind {
  return kind === "suggestion" ? "suggest" : kind;
}
