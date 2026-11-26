import { createContext, type ReactNode, useContext, useMemo, useRef } from "react";

import type { DisplayTree } from "../../api/types";
import { firstWordedBlock } from "../refs/tree-fragment";
import type { TreeNode } from "./issue-tree";
import { nodesInOrder } from "./tree-keys";

/**
 * The line a block chip holds its place with until the page's names read has named its target
 * (R3, the architect's choice 1 on the check row that grew by a line at `da175e7a`): each block of
 * this issue the tree holds, by its address, read from the tree's own reading of the block's words
 * (`TreeNode.tree`) as the names store reads a block's line from the names read's reading of it
 * (`refs/reference-names.tsx`'s `chipLineReading`): its first block that holds words
 * (`firstWordedBlock`). A row's tie names a block only once that read answers (`NodeRow.tsx`'s
 * `Chip`), and drew nothing before, so a trail grew by every chip's words as the names came, ran
 * past the words' last line and took a line of its own. With this line a chip is drawn from the
 * first drawing with the tree's name of its block, invisible, and shown once the names read has
 * named it.
 *
 * The tree's owner holds it for the page (`issue-tree-owner.tsx`), so a slot drawn with no owner
 * above it, as an existing case draws one, holds none and draws its chips as before. It holds none
 * either while the names read stands refused, which names no chip: a chip drawn invisible then
 * would stay so with the load ended, where the page draws nothing.
 */
const HeldChipLines = createContext<ReadonlyMap<string, DisplayTree> | undefined>(undefined);

/** No block held: the one empty map, for a tree that holds none. */
const NO_LINES: ReadonlyMap<string, DisplayTree> = new Map();

/** Each reading's first block that holds words, made once for each reading: a read made again
 *  keeps an unchanged node's reading as the same object (`tree-read/tree-nodes.ts`' `readingOf`),
 *  so its line is the same object too, and the map of lines is the one before. */
const linesOf = new WeakMap<DisplayTree, DisplayTree | null>();

/** The line of `reading` a chip is named by (`HeldChipLines`), or undefined where it holds no
 *  words. */
function lineOf(reading: DisplayTree): DisplayTree | undefined {
  let line = linesOf.get(reading);
  if (line === undefined) {
    line = firstWordedBlock(reading) ?? null;
    linesOf.set(reading, line);
  }
  return line ?? undefined;
}

/** Whether two maps of lines hold the same line for every address. */
function sameLines(
  one: ReadonlyMap<string, DisplayTree>,
  other: ReadonlyMap<string, DisplayTree>
): boolean {
  if (one.size !== other.size) return false;
  for (const [reference, line] of one) if (other.get(reference) !== line) return false;
  return true;
}

/** Holds the line of each block `root` holds for every chip under it (`HeldChipLines`), while the
 *  names read stands unrefused (`namesFailed`). One map for as long as the lines stay the same,
 *  so a read that changes no block's words draws no chip again. */
export function HeldChipLinesProvider({
  children,
  namesFailed,
  root,
}: {
  readonly children: ReactNode;
  readonly namesFailed: boolean;
  readonly root: TreeNode;
}): ReactNode {
  const built = useMemo(() => {
    const lines = new Map<string, DisplayTree>();
    for (const node of nodesInOrder(root)) {
      if (node.reference === undefined || node.tree === undefined || lines.has(node.reference)) {
        continue;
      }
      const line = lineOf(node.tree);
      if (line !== undefined) lines.set(node.reference, line);
    }
    return lines.size === 0 ? NO_LINES : lines;
  }, [root]);
  const held = useRef(built);
  if (!sameLines(held.current, built)) held.current = built;
  return (
    <HeldChipLines.Provider value={namesFailed ? undefined : held.current}>
      {children}
    </HeldChipLines.Provider>
  );
}

/** The line the tree holds for the block at `reference` (`HeldChipLines`), or undefined where no
 *  owner holds lines, the names read stands refused, or the tree holds no words of that block. */
export function useHeldChipLine(reference: string): DisplayTree | undefined {
  return useContext(HeldChipLines)?.get(reference);
}
