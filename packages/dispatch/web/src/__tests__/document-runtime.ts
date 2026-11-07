import type { BlockMark, EditorReferences } from "@legion/proof-editor";
import { Awareness } from "y-protocols/awareness";
import * as Y from "yjs";

import { installTypedBlocks } from "../features/doc/ask-block";
import type {
  ConnectDocument,
  ConnectionCallbacks,
  ConnectionState,
  DocumentConnection,
} from "../features/doc/connection";
import {
  type ConstructEditor,
  type EditorHandle,
  type EditorOptions,
  editorAttributes,
  type StoredMark,
} from "../features/doc/editor";
import type { PendingState } from "../features/doc/pending-sync";
import type { DocumentRuntimeValue } from "../features/doc/runtime";

export interface FakeEditor {
  /** Each set of block marks the host handed the editor (`setBlockMarks`), in order. */
  blockMarks: BlockMark[][];
  /** How many times the host asked to comment on the selection. */
  commented: number;
  destroyed: boolean;
  focused: string[];
  markdown: string | undefined;
  focusedBlocks: string[];
  /** The node views the host had installed when the fragment was first drawn into the root -
   *  the real editor's first batch (`@legion/proof-editor`'s `beforeBuild`). A host that
   *  installs them afterwards leaves this empty, and the real editor then re-creates every
   *  node view of the built document in one task (dispatch://INFRA-341, R11). */
  nodeViewsAtBuild: readonly string[];
  options: EditorOptions;
  readOnly: boolean;
  remoteMarks: Record<string, StoredMark>[];
  root: HTMLElement;
  /** What the host last handed the editor to draw its references with (`setReferences`), or
   *  undefined before it has handed it anything; the real plugin reads a block's text through
   *  `scan` and reports what it found through `onKeys`, which a test does in its place. */
  references: EditorReferences | null | undefined;
  /** Whether the host has shown the editor's root (`reveal`), which the real editor keeps
   *  hidden from before the first batch of its build until the host swaps it in. */
  revealed: boolean;
  /** Whether the fake view's selection is empty (a caret); a test sets it to select text. */
  selectionEmpty: boolean;
  /** The innermost block with an id that holds the selection, as `blockIdAtSelection` answers;
   *  a test sets it to copy a block link. */
  selectedBlock: string | null;
  /** The fake view's props bag, where `setProps` accumulates node views the host installs. */
  viewProps: Record<string, unknown>;
}

interface FakeConnection extends DocumentConnection {
  destroyed: boolean;
}

export interface FakeDocumentRuntime {
  admit(readOnly: boolean): void;
  connections: FakeConnection[];
  /** Every editor the host has built, in order, bound or not: the real one is constructed
   *  while its socket syncs and bound when the state frame has been applied
   *  (`features/doc/editor.ts`). */
  constructions: FakeEditor[];
  /** Those of them the host has bound to a document, in order. */
  editors: FakeEditor[];
  pending(state: PendingState): void;
  /** The server refuses the current socket because the stored document is outside the Proof
   * schema; a destroyed provider hears nothing more, so earlier connections are not told. */
  refuse(): void;
  runtime: DocumentRuntimeValue;
  status(state: ConnectionState): void;
  sync(): void;
  text: string;
}

const blockSchema = {
  types: [
    {
      attributes: {
        kind: { choices: ["note", "warning"], default: "note", kind: "enum" as const },
        title: { default: "", kind: "string" as const },
      },
      content: "paragraph+" as const,
      name: "callout",
      render: "host" as const,
    },
  ],
  version: 1,
};

export function fakeDocumentRuntime(seed: { text?: string } = {}): FakeDocumentRuntime {
  const callbacks: ConnectionCallbacks[] = [];
  const connections: FakeConnection[] = [];
  const constructions: FakeEditor[] = [];
  const editors: FakeEditor[] = [];
  const connect: ConnectDocument = (_artifactId, nextCallbacks) => {
    const doc = new Y.Doc();
    if (seed.text !== undefined) {
      const paragraph = new Y.XmlElement("paragraph");
      paragraph.insert(0, [new Y.XmlText(seed.text)]);
      doc.getXmlFragment("prosemirror").insert(0, [paragraph]);
    }
    const connection: FakeConnection = {
      awareness: new Awareness(doc),
      destroyed: false,
      destroy() {
        connection.destroyed = true;
      },
      doc,
    };
    callbacks.push(nextCallbacks);
    connections.push(connection);
    return connection;
  };
  const constructEditor: ConstructEditor = async (root, options) => {
    // The real editor's root is hidden from the moment it is constructed until the first batch
    // of its build, and the host swaps it in after the last (`@legion/proof-editor`'s
    // `constructProofEditor` and `reveal`), so the fake's is too: a host that never revealed
    // its editor would otherwise be the only one with nothing to show for it. It is an inline
    // `display` set and removed again, as the real one is, rather than a value read off the
    // root and written back, which a second construction in the same element would read as
    // `none`.
    root.style.setProperty("display", "none");
    const editor: FakeEditor = {
      blockMarks: [],
      commented: 0,
      destroyed: false,
      focused: [],
      focusedBlocks: [],
      markdown: undefined,
      nodeViewsAtBuild: [],
      options,
      readOnly: options.readOnly ?? false,
      remoteMarks: [],
      references: undefined,
      root,
      revealed: false,
      selectionEmpty: true,
      selectedBlock: null,
      viewProps: { nodeViews: {} },
    };
    const handle = {
      applyRemoteMarks(metadata: Record<string, StoredMark>) {
        editor.remoteMarks.push(metadata);
      },
      blockIdAtSelection() {
        return editor.selectedBlock;
      },
      commentOnSelection() {
        editor.commented += 1;
        return true;
      },
      destroy() {
        editor.destroyed = true;
        root.style.removeProperty("display");
      },
      focusBlock(blockId: string) {
        editor.focusedBlocks.push(blockId);
      },
      focusMark(markId: string) {
        editor.focused.push(markId);
      },
      getMarkdown: () => editor.markdown ?? root.textContent ?? "",
      markOffsets() {
        return new Map<string, number>();
      },
      removeMark() {},
      remarkMark() {
        return { refused: "gone" as const };
      },
      retypeMark() {
        return { refused: "missing" as const };
      },
      reveal() {
        editor.revealed = true;
        root.style.removeProperty("display");
      },
      setActiveBlocks() {},
      setBlockMarks(marks: readonly BlockMark[]) {
        editor.blockMarks.push([...marks]);
      },
      setActiveMarks() {},
      setMarkdown(markdown: string) {
        editor.markdown = markdown;
        root.textContent = markdown;
      },
      setReferences(references: EditorReferences | null) {
        editor.references = references;
      },
      setReadOnly(readOnly: boolean) {
        editor.readOnly = readOnly;
      },
      view: {
        dispatch() {},
        dom: root,
        get editable() {
          return !editor.readOnly;
        },
        hasFocus: () => root.ownerDocument.activeElement === root,
        props: editor.viewProps,
        setProps(next: Record<string, unknown>) {
          Object.assign(editor.viewProps, next);
        },
        state: {
          doc: { descendants() {} },
          selection: {
            get empty() {
              return editor.selectionEmpty;
            },
          },
        },
      },
    } as unknown as EditorHandle;
    // The real `constructEditor` installs Dispatch's node views once the editor library has
    // loaded, and does it over the still-empty document, before the first batch of the build
    // (`features/doc/editor.ts`; `@legion/proof-editor`'s `beforeBuild`). The fake stands in
    // for that call and for the host's own `beforeBuild` after it, so what is on the view when
    // it draws the fragment is what the build would have drawn with.
    installTypedBlocks(
      handle.view,
      options.blockSchema,
      options.onAskBlockHosts,
      options.watchPicture,
      options.anchoredBlock
    );
    options.beforeBuild?.(handle.view);
    editor.nodeViewsAtBuild = Object.keys(
      (editor.viewProps.nodeViews ?? {}) as Record<string, unknown>
    );
    constructions.push(editor);
    return {
      // The document is drawn into the root here, and the editor wears its name from here, as
      // the real one does: nothing of either is true of a construction that was never bound
      // (`features/doc/editor.ts`).
      bind: async () => {
        for (const [name, value] of Object.entries(editorAttributes)) {
          root.setAttribute(name, value);
        }
        root.textContent = options.ydoc.getXmlFragment("prosemirror").toString();
        editors.push(editor);
        return handle;
      },
      destroy: () => handle.destroy(),
      setReadOnly: (readOnly: boolean) => handle.setReadOnly(readOnly),
    };
  };

  return {
    admit(readOnly) {
      for (const callback of callbacks) {
        callback.onAdmission(readOnly);
      }
    },
    connections,
    constructions,
    editors,
    pending(state) {
      for (const callback of callbacks) {
        callback.onPending(state);
      }
    },
    refuse() {
      callbacks.at(-1)?.onOutsideSchema();
    },
    runtime: {
      blockSchema,
      constructEditor,
      loadTransport: async () => connect,
    },
    status(state) {
      for (const callback of callbacks) {
        callback.onStatus(state);
      }
    },
    sync() {
      for (const callback of callbacks) {
        callback.onSynced();
      }
    },
    text: seed.text ?? "",
  };
}

/**
 * `inner`, with `change` given each handle as it is bound: the shape a row takes when it reads
 * or replaces what an editor answers, since the editor is built before its document arrives
 * and only the binding has a handle (`features/doc/editor.ts`).
 */
export function changingEachHandle(
  inner: ConstructEditor,
  change: (handle: EditorHandle) => void
): ConstructEditor {
  return async (root, options) => {
    const construction = await inner(root, options);
    return {
      ...construction,
      bind: async () => {
        const handle = await construction.bind();
        change(handle);
        return handle;
      },
    };
  };
}
