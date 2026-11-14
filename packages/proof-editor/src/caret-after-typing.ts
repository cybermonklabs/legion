import { Plugin, TextSelection } from "@milkdown/kit/prose/state";
import { $prose } from "@milkdown/kit/utils";

/** The text the reader has just typed, as `handleTextInput` was given it: where it goes and what
 *  it is, read once by the `appendTransaction` of that same keystroke. */
interface TypedText {
  from: number;
  text: string;
}

/**
 * Leaves the caret after the character the reader just typed, where the browser put their caret
 * before it.
 *
 * ProseMirror reads a keystroke back from the DOM: the browser inserts the character, its
 * MutationObserver reads the change, and the selection the editor takes is the one the browser
 * is left holding (`readDOMChange`, prosemirror-view). Chromium does not always leave that
 * caret after the character. The first character typed into an empty block is the case: an
 * empty text block is drawn with a placeholder `<br class="ProseMirror-trailingBreak">`, and
 * since the block holds no text yet, the caret there is a position on the block element rather
 * than in a text node. Typing usually replaces the placeholder with a text node and leaves the
 * caret in it; with no frame between the Enter that made the block and the character - a
 * reader typing straight on, on a machine with none to spare - Chromium instead inserts the
 * text node *before* the placeholder it keeps, and leaves the caret where it was: on the
 * block, at offset 0, in front of the character. Measured in Chromium 156 on this pod: the
 * keystroke that works reports `<p>s</p>` with the caret in the text node at offset 1, the one
 * that does not `<p>s<br class="ProseMirror-trailingBreak"></p>` with the caret on the `<p>` at
 * offset 0 (dispatch://INFRA-341).
 *
 * The editor then takes that caret as the reader's own, and every character after it goes in
 * ahead of the first: a word typed into a fresh block was stored as `urvives a read-only
 * admissions` rather than `survives a read-only admission`, and the editor wrote that to the
 * Yjs document, where the server kept it (`e2e/offline-edits.e2e.ts`'s read-only admission row
 * in Dispatch, CI run 42504071017).
 *
 * So a character the reader typed is held to what typing means: the caret ends after it. Only
 * their own typing is corrected - `handleTextInput` is the editor's own typing path and runs
 * for nothing else, neither another person's change arriving through the binding, nor the
 * build's own writes, nor an undo - and only where the keystroke landed exactly as it was
 * reported: the text stands where it was typed and the caret is in front of it. Anything else
 * is left alone, because a caret that did not end up in either of those two places was put
 * somewhere by a plugin that meant it (an input rule rewriting what was typed, say).
 *
 * This plugin is a workaround and is to be deleted once ProseMirror reads that keystroke back
 * correctly, or once Chromium stops leaving the caret there. Upstream has not fixed it yet: the
 * repository pins prosemirror-view 1.42.3, and the fourth row of
 * `packages/dispatch/e2e/build-typing.e2e.ts` is red three times of three with this plugin
 * unregistered on **1.42.9**, the newest release at the time of writing (2026-10-31), which the
 * run installed through a `prosemirror-view` override in the root manifest. Nothing between
 * 1.42.4 and 1.42.9 claims this fix either; the nearest entries are 1.42.9's two about the
 * selection around focus, which is why that version was the one tried. So when the pin moves,
 * unregister this plugin, run that row, and delete the plugin if the row stays green.
 */
export const caretAfterTypingPlugin = $prose(() => {
  let typed: TypedText | undefined;
  return new Plugin({
    appendTransaction: (_transactions, _oldState, newState) => {
      const input = typed;
      typed = undefined;
      if (input === undefined) return null;
      const after = input.from + input.text.length;
      const { selection } = newState;
      if (!(selection instanceof TextSelection) || !selection.empty) return null;
      if (selection.from !== input.from || after > newState.doc.content.size) return null;
      if (newState.doc.textBetween(input.from, after, "", "") !== input.text) return null;
      return newState.tr.setSelection(TextSelection.create(newState.doc, after));
    },
    props: {
      handleTextInput: (_view, from, _to, text) => {
        typed = { from, text };
        return false;
      },
    },
  });
});
