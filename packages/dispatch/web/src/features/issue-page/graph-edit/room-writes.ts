import type { createTypedBlockCommands } from "@legion/proof-editor";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type * as ProseMirrorState from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";

import type { BlockSchema } from "../../../api/types";
import { RETYPE_KINDS, type RetypeAttributes, type RetypeKind } from "./placement";

/**
 * Part R's writes of the issue's one room (INFRA-5705's R3, and R5's retype): the transactions
 * the page itself dispatches in a row's editor that break the row's hold on its block on purpose
 * (`node-caret.ts`' `holdEditsToBlock`). Each carries `GRAPH_EDIT_META`, which the hold admits and
 * no edit of the reader's carries, so `keepsToBlock` reads every other transaction as before.
 *
 * The module imports no value of the editor library, so naming the meta key costs `node-caret.ts`
 * none of it: the library sits behind its own `import()` (`doc/editor.ts`), and the ProseMirror
 * state module and the typed-block commands a write needs are handed in by its caller, which
 * loaded them with the hold (`loadEditsHold`) and the editor.
 */

/** The meta key of this part's own transactions, which the edit hold admits. */
export const GRAPH_EDIT_META = "dispatch-graph-edit";

/** The attribute a block of the editor's document holds its id in (`@legion/proof-editor`'s
 *  `BLOCK_ID_ATTR`), written out rather than imported, as `doc/block-id.ts` writes its DOM
 *  twin. */
const BLOCK_ID_PROPERTY = "blockId";

/** The typed block of the schema each kind Retype offers is (R5): a statement is the schema's
 *  `requirement`, each other kind the type of its own name. */
const TYPE_OF_KIND: Readonly<Record<RetypeKind, string>> = {
  check: "check",
  criterion: "criterion",
  plan_task: "plan_task",
  statement: "requirement",
};

/**
 * The kinds of `RETYPE_KINDS`, in its order, whose typed block an editor of `schema` may make
 * (R5, decision 359): those the schema declares with a `creatable_since` of at most its version,
 * as the editor's own commands ask (`@legion/proof-editor`'s `creatable`). The schema the page
 * gives the editor of a child that is a view of its ancestor's spec makes no `requirement`
 * (`doc/schema.ts`' `viewBlockSchema`), so no statement there.
 */
export function retypeKinds(schema: BlockSchema): readonly RetypeKind[] {
  return RETYPE_KINDS.filter((kind) => {
    const type = schema.types.find((each) => each.name === TYPE_OF_KIND[kind]);
    return type !== undefined && (type.creatable_since ?? 1) <= schema.version;
  });
}

/** Where the block `blockId` of `doc` starts, and its size, or undefined where `doc` holds none. */
function blockIn(
  doc: ProseMirrorNode,
  blockId: string
): { readonly from: number; readonly size: number } | undefined {
  let found: { readonly from: number; readonly size: number } | undefined;
  doc.descendants((node, from) => {
    if (found !== undefined) return false;
    if (node.attrs[BLOCK_ID_PROPERTY] !== blockId) return !node.isTextblock;
    found = { from, size: node.nodeSize };
    return false;
  });
  return found;
}

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

/**
 * Gives the block `blockId` of the document of the editor `view` the kind `kind` (R5), with
 * `attributes` - the one `dispatch://` address of the node it stands under, as the typed block's
 * `parent` or `serves` - through the editor's own typed-block retype (`retypeBlock` of the
 * commands `create` makes, `createTypedBlockCommands` of `@legion/proof-editor`, decision 359)
 * under `schema`, the schema the page gave that editor: the block keeps its id, and its paragraph
 * becomes the typed block's body. One transaction, tagged with `GRAPH_EDIT_META`, which the
 * editor's handle (its own `retypeBlock`) never sets, with the selection at the end of the
 * block's words, so what the editor appends to it - an id for the body's paragraph - keeps to the
 * block the row holds. Answers whether the editor took it: a block the document does not hold, a
 * kind the schema does not make, and an attribute the commands refuse write nothing.
 */
export function writeRetype(
  view: EditorView,
  state: typeof ProseMirrorState,
  create: typeof createTypedBlockCommands,
  schema: BlockSchema,
  blockId: string,
  kind: RetypeKind,
  attributes: RetypeAttributes
): boolean {
  if (view.isDestroyed) return false;
  const before = view.state.doc;
  const commands = create({
    blockSchema: schema,
    dispatch: (transaction) => {
      const block = blockIn(transaction.doc, blockId);
      if (block !== undefined) {
        const end = transaction.doc.resolve(block.from + block.size - 1);
        transaction.setSelection(state.Selection.near(end, -1));
      }
      view.dispatch(transaction.setMeta(GRAPH_EDIT_META, true));
    },
    getState: () => view.state,
  });
  const pointers: Readonly<Record<string, readonly string[]>> =
    "parent" in attributes ? { parent: [attributes.parent] } : { serves: [attributes.serves] };
  try {
    commands.retypeBlock(blockId, TYPE_OF_KIND[kind], pointers);
  } catch {
    // The commands refuse a kind the schema does not make and a pointer they do not take, and
    // dispatch nothing for it.
    return false;
  }
  return view.state.doc !== before;
}
