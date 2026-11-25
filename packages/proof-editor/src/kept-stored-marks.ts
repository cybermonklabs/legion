import type { Mark } from "@milkdown/kit/prose/model";
import { Plugin, PluginKey } from "@milkdown/kit/prose/state";
import { $prose } from "@milkdown/kit/utils";

/**
 * The marks a person's transaction stored for the text typed next, held while plugins append
 * transactions after it.
 *
 * A mark's input rule (`**text**`, `*text*`, `` `text` ``) closes the mark it adds by storing the
 * marks the cursor had before, so the text typed next is not under it. ProseMirror drops stored
 * marks whenever a transaction changes the document without storing marks of its own, and the
 * authored tracker appends one after every typed character, marking it as the person's. So the
 * marks the rule stored were gone by the next keystroke, and the bold mark, which reaches the
 * character typed at its end, took the rest of the line: typing
 * `**Status quo is not an excuse.** Reject historical mistakes.` stored the whole sentence bold,
 * and a person who then turned bold off after the space left a bold run ending in a space, which
 * the markdown can only write `&#32;**&#82;eject` (dispatch://INFRA-1387, INFRA-4345 R6).
 *
 * This plugin remembers the marks a person's own transaction stored, with where its cursor stood,
 * and stores them again after any transaction a plugin appends to it drops them, while the cursor
 * stands there still. A person's next transaction that stores no marks forgets them, as
 * ProseMirror would.
 */
interface Kept {
  marks: readonly Mark[];
  head: number;
}

const keptStoredMarksKey = new PluginKey<Kept | null>("keptStoredMarks");

export function createKeptStoredMarksPlugin(): Plugin<Kept | null> {
  return new Plugin<Kept | null>({
    key: keptStoredMarksKey,
    state: {
      init: () => null,
      apply(tr, kept) {
        // A transaction a plugin appended, rather than one a person or a peer made.
        if (tr.getMeta("appendedTransaction") !== undefined) {
          return kept === null ? null : { marks: kept.marks, head: tr.mapping.map(kept.head) };
        }
        if (tr.storedMarksSet && tr.storedMarks !== null) {
          return { marks: tr.storedMarks, head: tr.selection.head };
        }
        return null;
      },
    },
    appendTransaction(_transactions, _oldState, newState) {
      const kept = keptStoredMarksKey.getState(newState);
      if (kept === null || kept === undefined || newState.storedMarks !== null) return null;
      if (!newState.selection.empty || newState.selection.head !== kept.head) return null;
      return newState.tr.setStoredMarks(kept.marks).setMeta("addToHistory", false);
    },
  });
}

export const keptStoredMarksPlugin = $prose(() => createKeptStoredMarksPlugin());
