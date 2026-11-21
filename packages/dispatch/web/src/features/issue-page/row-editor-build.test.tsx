import { expect, spyOn, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useCallback, useState } from "react";
import { MemoryRouter } from "react-router-dom";
import * as Y from "yjs";

import { type FakeDocumentRuntime, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { answerRequests } from "../../__tests__/network";
import {
  artifact,
  createQueryClient,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import { type ConstructEditor, constructEditor, type EditorConstruction } from "../doc/editor";
import { DocumentRuntime, type DocumentRuntimeValue } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { DocumentAdmissionProvider } from "./document-admission";
import { DocumentStages } from "./document-stage";
import { FocusedNodeProvider } from "./focused-node";
import { IssuePresetPanels } from "./IssuePresetPanels";
import { IssueTree } from "./IssueTree";
import type { TreeNode } from "./issue-tree";
import { KEY_REPORTS } from "./page-structure";
import type { OnEditing, RowEditorHandlers } from "./useNodeEditing";

/**
 * A row of the outline edited in the page's own editor of its document - `IssuePresetPanels`,
 * `StagedDocument`, `ProofDocument` and the real editor (`doc/editor.ts`) - while that editor is
 * still being built: its binding draws the document's blocks a batch a task, and the editor
 * takes the reader's edits from its first batch (`@legion/proof-editor`'s `bind`). The row holds
 * the editor's own edits to the node's block (`node-caret.ts`'s `holdEditsToBlock`), and it was
 * handed the editor only once the binding had answered: an edit the editor took outside that
 * block before then wrote another block of the shared document. The page hands the row the
 * editor as it is constructed (`ProofDocument`'s `onEditorConstructed`), and the binding keeps
 * the row's hold through its adoption.
 */

/** The statement's document as the room holds it: the words above the statement, the statement
 *  (`r1`), and the words below it. */
const BLOCKS: readonly (readonly [id: string, words: string])[] = [
  ["r0", "The words above the statement."],
  ["r1", "A statement of the spec."],
  ["r2", "The words below the statement."],
];

/** The issue, holding the statement of its spec (`artifact`, whose slug is `spec`). */
const STATEMENT: TreeNode = {
  alsoUnder: [],
  children: [],
  chips: [],
  facts: [],
  gaps: [],
  held: 0,
  id: "spec#b-r1",
  question: false,
  replaced: false,
  text: "A statement of the spec.",
  transcluded: false,
  type: "requirement",
  waiting: false,
};
const ISSUE: TreeNode = {
  ...STATEMENT,
  children: [STATEMENT],
  id: "CORE-1",
  text: "CORE-1",
  type: "issue",
};

/** The page's own `issue` scope, which the tree's keys are registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** The tree and, below it, the page's editor of the document a row is being edited in, as the
 *  outline's body draws them (`IssuePresetPanels.tsx`'s `useOutlineEditing`). */
function RowsAndEditor(): ReactNode {
  const [editing, setEditing] = useState<
    { readonly artifactId: string; readonly handlers?: RowEditorHandlers } | undefined
  >(undefined);
  const onEditing = useCallback<OnEditing>(
    (artifactId, handlers) =>
      setEditing(artifactId === undefined ? undefined : { artifactId, handlers }),
    []
  );
  return (
    <>
      <IssueTree
        artifacts={new Map([[artifact.slug, artifact]])}
        context={new Set()}
        emptyLine={undefined}
        filtering={false}
        focusReveal={false}
        holdsEscape={false}
        isClosed={false}
        onEditing={onEditing}
        readsHeld
        readsSettled
        reveal={undefined}
        root={ISSUE}
        rootRow
      />
      <IssuePresetPanels
        editedDocument={editing?.artifactId === artifact.id ? artifact : undefined}
        handlers={editing?.handlers}
        isClosed={false}
        issueKey="CORE-1"
        onToolbarSink={() => () => {}}
        onVersionChange={() => {}}
      />
    </>
  );
}

/** The words of block `id` of the document `doc` holds. */
function wordsOf(doc: Y.Doc, id: string): string | undefined {
  const block = doc
    .getXmlFragment("prosemirror")
    .toArray()
    .find((each) => each instanceof Y.XmlElement && each.getAttribute("blockId") === id);
  return block instanceof Y.XmlElement
    ? block
        .toArray()
        .map((each) => (each instanceof Y.XmlText ? each.toString() : ""))
        .join("")
    : undefined;
}

/** Puts an `X` at the start of block `id` of the editor drawn in `root`, as the browser does with
 *  a letter typed there: in the page, which the editor reads back as its own edit. */
function typeAtStart(root: HTMLElement, id: string): void {
  const words = root.querySelector(`[data-block-id="${id}"]`)?.firstChild;
  if (!(words instanceof Text)) throw new Error(`the editor draws no words of block ${id}`);
  words.data = `X${words.data}`;
}

/** Draws the tree and, below it, the page's editor of its document (`RowsAndEditor`) under the
 *  page's providers, with `construct` making that editor and each connection's document holding
 *  the statement's blocks, as the room's state frame does. Answers the drawn view, the runtime
 *  whose connections the case syncs, and what ends the case. */
function drawRowsAndEditor(construct: ConstructEditor): {
  readonly end: () => void;
  readonly fake: FakeDocumentRuntime;
  readonly view: RenderResult;
} {
  answerRequests(...documentSurfaceHandlers());
  const text = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: BLOCKS.map(([, words]) => words).join("\n\n"),
    token: "sha256:spec-text",
    version: null,
  });
  const fake = fakeDocumentRuntime();
  const runtime: DocumentRuntimeValue = {
    constructEditor: construct,
    loadTransport: async () => {
      const connect = await fake.runtime.loadTransport();
      return (artifactId, callbacks) => {
        const connection = connect(artifactId, callbacks);
        connection.doc.getXmlFragment("prosemirror").insert(
          0,
          BLOCKS.map(([id, words]) => {
            const block = new Y.XmlElement("paragraph");
            block.setAttribute("blockId", id);
            block.insert(0, [new Y.XmlText(words)]);
            return block;
          })
        );
        return connection;
      };
    },
  };
  const client = createQueryClient();
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={runtime}>
          <KeymapProvider>
            <MarginProvider>
              <FocusedNodeProvider>
                <DocumentAdmissionProvider>
                  <DocumentStages>
                    <IssueScope />
                    <RowsAndEditor />
                  </DocumentStages>
                </DocumentAdmissionProvider>
              </FocusedNodeProvider>
            </MarginProvider>
          </KeymapProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    end: () => {
      view.unmount();
      text.mockRestore();
    },
    fake,
    view,
  };
}

/** Enter on the statement's row, and the room's sync once the page has constructed the row's
 *  editor. Answers the row and the page's one connection to the room. */
async function editTheStatement(
  { fake, view }: { readonly fake: FakeDocumentRuntime; readonly view: RenderResult },
  user: UserEvent
): Promise<{
  readonly connection: FakeDocumentRuntime["connections"][number];
  readonly row: HTMLElement;
}> {
  const row = view.container.querySelector<HTMLElement>('[data-node-id="spec#b-r1"]');
  if (row === null) throw new Error("the statement's row is not drawn");
  await act(async () => row.focus());
  await user.keyboard("{Enter}");
  // The editor loads its library on its first use, which can take more than a second.
  await waitFor(() => expect(fake.connections).toHaveLength(1), { timeout: 10_000 });
  const [connection] = fake.connections;
  if (connection === undefined) throw new Error("the page opened no connection");
  await waitFor(() => expect(row.querySelector('[contenteditable="true"]')).not.toBeNull(), {
    timeout: 10_000,
  });
  await act(async () => fake.sync());
  return { connection, row };
}

// The reader presses into the row's editor while it is still being built and types: once as
// the batch that drew the statement lands, before the binding has adopted the blocks, and once
// the binding has answered, before the page hands the row the bound editor. Both letters go in
// outside the statement's block, and neither may reach the document.
test("an edit outside the node's block while the row's editor is being built reaches no document", async () => {
  const typed: string[] = [];
  // The real editor, typed into at the two moments above.
  const building: ConstructEditor = async (root, options) => {
    const construction = await constructEditor(root, {
      ...options,
      onBatch: (blocks) => {
        const landed = options.onBatch?.(blocks);
        return () => {
          landed?.();
          if (blocks.includes("r1")) {
            typeAtStart(root, "r0");
            typed.push("r0");
          }
        };
      },
    });
    return {
      ...construction,
      bind: async () => {
        const handle = await construction.bind();
        typeAtStart(root, "r2");
        typed.push("r2");
        return handle;
      },
    };
  };
  const user = userEvent.setup();
  const drawn = drawRowsAndEditor(building);
  try {
    const { connection, row } = await editTheStatement(drawn, user);
    await waitFor(() => expect(typed).toEqual(["r0", "r2"]), { timeout: 10_000 });
    const editable = row.querySelector<HTMLElement>('[contenteditable="true"]');
    // The row is handed the editor, and its caret goes into the statement.
    await waitFor(
      () => {
        const caret = window.getSelection()?.anchorNode?.parentElement;
        expect(
          editable !== null &&
            document.activeElement === editable &&
            caret?.closest("[data-block-id]")?.getAttribute("data-block-id") === "r1",
          "the row took no caret in the statement once it was handed its editor"
        ).toBe(true);
      },
      { timeout: 10_000 }
    );
    expect(
      { r0: wordsOf(connection.doc, "r0"), r2: wordsOf(connection.doc, "r2") },
      "a letter typed outside the statement while the row's editor was built reached the document"
    ).toEqual({ r0: "The words above the statement.", r2: "The words below the statement." });
  } finally {
    drawn.end();
  }
});

// The row's editor is constructed, and its binding then fails, so no editor is coming: the
// document says so (`ProofDocument`'s `onNoEditor`, from `reportLoadFailure`), and `StagedDocument`
// and `IssuePresetPanels` pass that word on to the row (`RowEditorHandlers`'s `none`). The row,
// still waiting for its editor with the focus on it, ends the editing with no caret placed and
// says its editor could not open.
test("a row whose editor's binding fails says its editor could not open", async () => {
  const built: EditorConstruction[] = [];
  // The real editor, whose binding rejects.
  const failing: ConstructEditor = async (root, options) => {
    const construction = await constructEditor(root, options);
    built.push(construction);
    return {
      ...construction,
      bind: async () => {
        throw new Error("the binding failed");
      },
    };
  };
  const user = userEvent.setup();
  const drawn = drawRowsAndEditor(failing);
  try {
    const { row } = await editTheStatement(drawn, user);
    await waitFor(
      () =>
        expect(
          drawn.view.container.querySelector("[data-row-report]")?.textContent,
          "the row whose editor's binding failed did not say its editor could not open"
        ).toBe(KEY_REPORTS.editor_closed),
      { timeout: 10_000 }
    );
    expect(document.activeElement, "the focus is not on the statement's row").toBe(row);
    expect(
      row
        .querySelector('[contenteditable="true"]')
        ?.contains(window.getSelection()?.anchorNode ?? null) === true,
      "a caret went into the editor whose binding failed"
    ).toBe(false);
  } finally {
    drawn.end();
    for (const construction of built) construction.destroy();
  }
});
