import type * as ProseMirrorState from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";

/**
 * Part R's writes of the issue's one room (INFRA-5705's R3, and R5's retype): the transactions
 * the page itself dispatches in a row's editor that break the row's hold on its block on purpose
 * (`node-caret.ts`' `holdEditsToBlock`). Each carries `GRAPH_EDIT_META`, which the hold admits and
 * no edit of the reader's carries, so `keepsToBlock` reads every other transaction as before.
 *
 * The module imports types alone, so naming the meta key costs `node-caret.ts` none of the
 * editor library, which sits behind its own `import()` (`doc/editor.ts`); the ProseMirror state
 * module the write needs is handed in by its caller, which loaded it with the hold
 * (`loadEditsHold`).
 */

/** The meta key of this part's own transactions, which the edit hold admits. */
export const GRAPH_EDIT_META = "dispatch-graph-edit";

/** The attribute a block of the editor's document holds its id in (`@legion/proof-editor`'s
 *  `BLOCK_ID_ATTR`), written out rather than imported, as `doc/block-id.ts` writes its DOM
 *  twin. */
const BLOCK_ID_PROPERTY = "blockId";

/**
 * Writes a new row's block (R3): one paragraph of `words` at the end of the document of the
 * editor `view` - the issue's own fragment, the tree artifact's editor - with the block id
 * `blockId` the page chose for it, so the tree read returns the node by that id, and the
 * selection at the end of its words, so the reader's next letter follows the first. One
 * transaction, tagged with `GRAPH_EDIT_META`. Answers whether it was dispatched: a schema with
 * no paragraph writes nothing.
 */
export function writeNewBlock(
  view: EditorView,
  state: typeof ProseMirrorState,
  blockId: string,
  words: string
): boolean {
  const { doc, schema } = view.state;
  const paragraph = schema.nodes.paragraph;
  if (paragraph === undefined) return false;
  const end = doc.content.size;
  const block = paragraph.create({ [BLOCK_ID_PROPERTY]: blockId }, schema.text(words));
  const transaction = view.state.tr.insert(end, block);
  transaction.setSelection(state.TextSelection.create(transaction.doc, end + 1 + words.length));
  view.dispatch(transaction.setMeta(GRAPH_EDIT_META, true));
  return true;
}
