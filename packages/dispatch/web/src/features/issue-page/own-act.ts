import { replaceEqualDeep } from "@tanstack/react-query";
import { createContext, useContext } from "react";

import type { DisplayTree } from "../../api/types";
import { firstWordedBlock, wordsOf } from "../refs/tree-fragment";
import { nodeWords, type TreeNode } from "./issue-tree";
import { pathTo } from "./tree-fold";

/**
 * How a write a row makes tells the tree that draws the row that the reader acted on the row's
 * node: Keep calls it as its write lands, before the page draws the write's answer, with the
 * node's own `dispatch://` address (`TreeNode.reference`, which is the decision's `ref`), and
 * Graph's node menu calls it as a delete is taken, with the node's read id (`TreeNode.readId`,
 * `graph-edit/row-edits.tsx`), since a block of the issue's own tree has no address. The tree
 * notes that node, and a read that then drops its row is the reader's own doing, so the focus
 * goes on to the nearest row rather than to the page's body (`IssueTree.tsx`'s `ownAct`). Outside
 * a tree nothing hears it. It is a file of its own: the tree imports the row's writes
 * (`DecisionActions.tsx`), so it cannot live in the tree's file, and the tree, not any one row,
 * is what gives it. An edit in a row's editor is noted by the tree itself, with the words the
 * edit left (`editWords`, `drawnWords`).
 */
export const NoteOwnAct = createContext<(named: string) => void>(() => {});

/** What a row's write calls as it lands, with the address or the read id of the node it acted
 *  on. */
export function useNoteOwnAct(): (named: string) => void {
  return useContext(NoteOwnAct);
}

/**
 * The note of the node the reader last acted on themselves, by its id, with the tree drawn as it
 * was noted (`root`): a read that drops that node's row is theirs (`IssueTree.tsx`'s `ownAct`). A
 * Keep of a decision notes the node as that tree draws it (`node`). An edit in a row's editor notes
 * the words it left in the row's block (`editWords`), and the row it was made in, not yet `ended`.
 * Each tree drawn after it decides whether the note lasts (`noteAfterDraw`).
 */
export type OwnAct =
  | {
      readonly id: string;
      readonly kind: "keep";
      readonly node: TreeNode;
      readonly root: TreeNode;
    }
  | {
      readonly ended: boolean;
      readonly id: string;
      readonly kind: "edit";
      readonly root: TreeNode;
      readonly row: string;
      readonly words: string;
    };

/**
 * The note of the reader's own act once the tree drawn from `root` is drawn, `editingRow` the row
 * being edited then (`useNodeEditing.ts`'s `editing.row`): the note held to `root`, the note marked
 * `ended`, the note as it was, or none where it ends. The tree reads the note for a drop of its
 * node's row before it asks this (`IssueTree.tsx`'s `ownAct`), so the tree that ends a note can
 * still count such a drop as the reader's. A note is held to the last tree that kept it, so a
 * render of that same tree compares nothing, and a tree that draws the node nowhere ends it.
 *
 * A Keep's note lasts while each tree drawn draws its node as it was noted (`sameOwnFields`), so
 * another read that lands first - a comment, a read mark - leaves it for the Keep's own read. The
 * node is compared by its own fields, its children and their count left out, so a reply someone
 * else adds under it changes nothing.
 *
 * An edit's note follows decision 409. The note ends at the first tree that draws the node with
 * the words the edit left, or at the first tree drawn after the editing of its row ends, whichever
 * comes first. The row is how the tree tells that end (`editing.row`), and `ended` says it has
 * been seen. A tree's words are read by the rule the edit's own are read by (`drawnWords`). While
 * the reader edits the row, a tree that draws the node with other words keeps the note: a save
 * lands between two of the reader's keys, so the read that draws an earlier edit comes before the
 * one that drops the row. The tree that draws the edit's words is the edit's own read, so a drop in
 * a later tree - a version a collaborator wrote, another session's change - is not the reader's.
 * The edit's words may never be drawn: a collaborator's change to the block, saved before the read
 * of the reader's words, is drawn merged with them. So the editing's end bounds the note, or it
 * would last until the row left the tree and count any later drop of the row as the reader's. The
 * render that finds the editing of the row over marks the note `ended`, held to the tree drawn
 * then, and the next tree drawn ends it. That tree still reads the note for a drop, as the read of
 * the reader's last edit may come just after they leave the row. That errs in one narrow case,
 * which decision 409 accepts: a collaborator's change that drops the row in that one tree counts as
 * the reader's, and the focus moves on to the nearest row rather than to the page's body, which
 * loses the reader nothing.
 */
export function noteAfterDraw(
  noted: OwnAct | undefined,
  root: TreeNode,
  editingRow: string | undefined
): OwnAct | undefined {
  if (noted === undefined) return undefined;
  let held = noted;
  if (noted.root !== root) {
    if (noted.kind === "edit" && noted.ended) return undefined;
    const now = pathTo(root, noted.id)?.at(-1);
    if (now === undefined) return undefined;
    const lasts =
      noted.kind === "keep" ? sameOwnFields(noted.node, now) : drawnWords(now) !== noted.words;
    if (!lasts) return undefined;
    held = { ...noted, root };
  }
  return held.kind === "edit" && !held.ended && held.row !== editingRow
    ? { ...held, ended: true }
    : held;
}

/** Whether `after` draws the node `before` drew as it was, by its own fields: what it holds under
 *  it - its children, and their count (`held`) - is left out. Each read builds the whole tree
 *  anew, so the fields are compared by value, as TanStack Query compares two answers of a read
 *  (`replaceEqualDeep` gives back its first value where the second holds the same values). The
 *  tree keeps its note of a Keep while each tree it draws draws the noted node so
 *  (`noteAfterDraw`). */
function sameOwnFields(before: TreeNode, after: TreeNode): boolean {
  const own = { ...before, children: [], held: 0 };
  return replaceEqualDeep(own, { ...after, children: [], held: 0 }) === own;
}

/**
 * The words of a block as an edit leaves them and as a read draws them, by one rule: the block's
 * text, each text run joined to the one before it and a space wherever anything else stands
 * between two (`refs/tree-fragment.ts`'s `wordsOf`), with each run of white space made one space
 * and none left at either end. The block is the JSON of a ProseMirror node of the documents' one
 * schema: the editor's own block as `Node.toJSON` writes it, which the editor's hold reports an
 * edit with (`node-caret.ts`'s `holdEditsToBlock`), and the server's reading of the markdown the
 * block is stored as (`DisplayTree`, `drawnWords`). Both hold the words of a link and the address
 * of a `dispatch://` reference as they are stored: the editor draws a reference's title over the
 * address it holds (`@legion/proof-editor`'s `reference-decorations.ts`), and no drawing is in
 * either. No mark splits a word, as two marks may split a run on one side alone: the editor holds
 * the reader's own letters under an authorship mark (`proofAuthored`) the stored markdown does
 * not write, and an address typed out bare under no mark, which the server's reading links.
 */
export function editWords(block: DisplayTree): string {
  return flatWords(wordsOf(block));
}

/**
 * The words a tree draws `node` with, by the rule an edit's are read by (`editWords`), for the
 * block an edit of the node's row holds (`useNodeEditing.ts`): the node's words as a reader reads
 * them (`issue-tree.ts`'s `nodeWords`), the server's reading of them (`TreeNode.tree`), which for
 * a block of a document is the reading of the block's whole stored text. A node whose read sent
 * no reading - this page asks every read for one (`api/client.ts`'s `Dispatch-Trees`) - is read
 * from its words as that read holds them (`TreeNode.text`). A decision's reading is the reading
 * of its section, whose first block is the decision's own heading, the block its row edits, so a
 * decision whose read sent one is drawn with the words of the first block of it that holds any
 * (`refs/tree-fragment.ts`'s `firstWordedBlock`), the block its one line is read from
 * (`lineReading`). The issue's words join its key to its title, which no edit leaves, but the
 * outline draws no row of the issue, so no edit notes it.
 */
function drawnWords(node: TreeNode): string {
  const block =
    node.type === "decision" && node.tree !== undefined ? firstWordedBlock(node.tree) : undefined;
  return block === undefined ? flatWords(nodeWords(node)) : editWords(block);
}

/** `words` with each run of white space made one space and none left at either end. */
function flatWords(words: string): string {
  return words.trim().replace(/\s+/g, " ");
}
