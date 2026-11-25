import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { Schema } from "@milkdown/kit/prose/model";
import { EditorState } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, type RefObject, useLayoutEffect, useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import type { Artifact } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { DocumentAdmissionProvider } from "./document-admission";
import { DocumentStage, DocumentStages } from "./document-stage";
import { FocusedNodeProvider } from "./focused-node";
import { IssueTree } from "./IssueTree";
import { FOOT_HEIGHT_PROPERTY, IDENTITY_HEIGHT_PROPERTY } from "./identity-block";
import type { TreeNode } from "./issue-tree";
import { EDITOR_BOX_ATTRIBUTE } from "./node-caret";
import { waitInAct } from "./outline-fixtures";
import { CONTROL_LABELS, type NodeType } from "./page-words";
import { EditorBox } from "./StagedDocument";
import { abortOnReaderAct, type OnEditing, type OnEditor } from "./useNodeEditing";

/**
 * R18 (decision 420 (1)): a press on the words of an outline row that takes a caret opens its
 * editor with the caret where the press landed, by the point the Spec view's statements pass
 * (`press-point.ts`, decisions 446 and 455), one case for each row of the plan's ST-8. A row that
 * takes no caret, a link, a control, a drag, a modifier and the row being edited keep what a
 * press does today; each such case presses the words plainly after, so a tree with no press of
 * its own fails it too.
 *
 * The tree is drawn alone, as `IssueTree.test.tsx` draws it, with a plain editable standing in
 * for the editor of the spec, which draws each statement's block; happy-dom has no
 * `caretPositionFromPoint`, so each press names the place the browser would read under it.
 */

const KEY = "CORE-1";

const specDocument: Artifact = {
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-spec",
  issue_key: KEY,
  kind: "doc",
  name: "Spec",
  primary: false,
  project: "CORE",
  ref_key: "artifact:artifact-spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

/** The first statement's words, which most cases press. */
const FIRST = "The page draws each row whole.";
/** Where in `FIRST` a press lands: 3 characters into "draws". */
const IN_DRAWS = "The page dra".length;
const SECOND = "A second statement stands under it.";

function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** Statement `block` of the spec, which takes a caret while the issue is open. */
function statement(block: string, text: string, rest: Partial<TreeNode> = {}): TreeNode {
  return node({ id: `spec#b-${block}`, statement: true, text, type: "requirement", ...rest });
}

/** A comment another person wrote, which is no block of a document and takes no caret. */
const theirComment = node({
  author: { id: "bob", kind: "user" },
  id: "comment-1",
  text: "Another person's words on the issue.",
  type: "comment",
});

function issue(children: readonly TreeNode[]): TreeNode {
  return node({ children: [...children], id: KEY, type: "issue" });
}

/** The tree draws three levels from 1024 px up and reads one node at a time below it
 *  (`ZOOMED_TREE_QUERY`); happy-dom answers every media query against its own window. */
function setViewport(width: number): void {
  window.matchMedia = ((query: string) => {
    const max = /max-width:\s*([\d.]+)px/.exec(query)?.[1];
    const min = /min-width:\s*([\d.]+)rem/.exec(query)?.[1];
    const matched =
      max !== undefined ? width <= Number(max) : min !== undefined && width >= Number(min) * 16;
    return {
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query.startsWith("not all and ") ? !matched : matched,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    } as MediaQueryList;
  }) as typeof window.matchMedia;
}

const originalMatchMedia = window.matchMedia;
const originalPoint = Object.getOwnPropertyDescriptor(document, "caretPositionFromPoint");
afterEach(() => {
  window.matchMedia = originalMatchMedia;
  if (originalPoint === undefined) Reflect.deleteProperty(document, "caretPositionFromPoint");
  else Object.defineProperty(document, "caretPositionFromPoint", originalPoint);
  window.getSelection()?.removeAllRanges();
});

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The page's own `issue` scope, which the tree's keys are registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** Where the page's address stands. */
function AddressProbe({ seen }: { seen: { path?: string } }): ReactNode {
  const location = useLocation();
  seen.path = `${location.pathname}${location.search}${location.hash}`;
  return null;
}

/** A ProseMirror view the row holds its editor's edits on (`holdEditsToBlock`): the row puts a
 *  caret in the editable standing in for the editor only once that hold is on. */
function standInView(): EditorView {
  return new EditorView(null, {
    state: EditorState.create({
      schema: new Schema({ nodes: { doc: { content: "text*" }, text: {} } }),
    }),
  });
}

/** The spec's one editor as the page draws it for the row being edited, on that row's stage, in
 *  the box it scrolls in: an editable drawing `blocks`, each by its block id, handed to the row
 *  as the page hands its editor over. `held.set` is how the tree's `onEditing` reaches it. */
function EditedDocument({
  blocks,
  held,
}: {
  blocks: ReactNode;
  held: { set?: OnEditing };
}): ReactNode {
  const [edited, setEdited] = useState<{ artifactId: string; onEditor?: OnEditor }>();
  held.set = (artifactId, onEditor) =>
    setEdited(artifactId === undefined ? undefined : { artifactId, onEditor });
  return edited === undefined ? null : (
    <DocumentStage artifactId={edited.artifactId}>
      <EditorBox>
        <HandedEditor onEditor={edited.onEditor}>
          <div contentEditable suppressContentEditableWarning>
            {blocks}
          </div>
        </HandedEditor>
      </EditorBox>
    </DocumentStage>
  );
}

function HandedEditor({
  children,
  onEditor,
}: {
  children: ReactNode;
  onEditor: OnEditor | undefined;
}): ReactNode {
  // A layout effect, as the page's editor hands itself over in the task it draws the document
  // (`ProofDocument.tsx`'s `onEditorHandle`) and the tree waits one task for it
  // (`useNodeEditing.ts`'s `handing`): a passive effect of a render a press's timer started can
  // run a task later.
  useLayoutEffect(() => {
    const view = standInView();
    onEditor?.({ view });
    return () => {
      view.destroy();
      onEditor?.(undefined);
    };
  }, [onEditor]);
  return children;
}

/** Each statement of `root` as the spec's editor draws it: a paragraph of its words, by its
 *  block id, or what `held` gives for its block. */
function specBlocks(root: TreeNode, held: Readonly<Record<string, ReactNode>> = {}): ReactNode {
  const statements: TreeNode[] = [];
  const walk = (one: TreeNode) => {
    if (one.id.startsWith("spec#b-")) statements.push(one);
    for (const child of one.children) walk(child);
  };
  walk(root);
  return statements.map((one) => {
    const block = one.id.slice("spec#b-".length);
    return (
      <p data-block-id={block} key={block}>
        {held[block] ?? one.text}
      </p>
    );
  });
}

/** The tree alone, under the page's keymap, as `IssueTree.test.tsx` draws it, with the spec's
 *  editor standing in (`EditedDocument`). `reveal` is the chain to the node an address names, and
 *  `addressActs` the reader's acts since they followed it, as the page's body hands them on. */
function renderTree(
  root: TreeNode,
  {
    addressActs,
    held,
    isClosed = false,
    reveal,
  }: {
    addressActs?: RefObject<AbortController | undefined>;
    held?: Readonly<Record<string, ReactNode>>;
    isClosed?: boolean;
    reveal?: readonly TreeNode[];
  } = {}
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const edited: (string | undefined)[] = [];
  const document_: { set?: OnEditing } = {};
  const seen: { path?: string } = {};
  const tree = { root };
  const draw = () => (
    <MemoryRouter>
      <AddressProbe seen={seen} />
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <FocusedNodeProvider>
              <DocumentAdmissionProvider>
                <DocumentStages>
                  <IssueScope />
                  <IssueTree
                    {...(addressActs === undefined ? {} : { addressActs })}
                    addressKey="following-1"
                    artifacts={new Map([[specDocument.slug, specDocument]])}
                    context={new Set()}
                    filtering={false}
                    focusReveal={reveal !== undefined}
                    holdsEscape={false}
                    isClosed={isClosed}
                    onEditing={(artifactId, onEditor) => {
                      edited.push(artifactId);
                      document_.set?.(artifactId, onEditor);
                    }}
                    reveal={reveal}
                    root={tree.root}
                    rootRow
                  />
                  <EditedDocument blocks={specBlocks(root, held)} held={document_} />
                </DocumentStages>
              </DocumentAdmissionProvider>
            </FocusedNodeProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw());
  return {
    address: () => seen.path,
    edited,
    /** Draws `next` in place of the tree, as a read answering with other nodes does. */
    reread: (next: TreeNode) => {
      tree.root = next;
      view.rerender(draw());
    },
    ...view,
  };
}

/** The row drawn for `nodeId`. */
function rowOf(container: HTMLElement, nodeId: string): HTMLElement {
  const row = container.querySelector<HTMLElement>(`[data-node-row][data-node-id="${nodeId}"]`);
  if (row === null) throw new Error(`the tree draws no row of ${nodeId}`);
  return row;
}

/** The node whose row holds the focus, or undefined. */
function focusedRow(container: HTMLElement): string | undefined {
  const active = document.activeElement;
  return [...container.querySelectorAll<HTMLElement>("[data-node-row]")]
    .find((row) => row.contains(active))
    ?.getAttribute("data-node-id") as string | undefined;
}

/** The text of a row's drawn words that reads `words`. */
function drawnText(row: HTMLElement, words: string): Text {
  const walker = document.createTreeWalker(
    row.querySelector("[data-node-words]") ?? row,
    NodeFilter.SHOW_TEXT
  );
  for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
    if (text instanceof Text && text.data === words) return text;
  }
  throw new Error(`the row draws no words "${words}"`);
}

/** The editable of the editor a row holds, or null where it holds none. */
function editorIn(row: HTMLElement): HTMLElement | null {
  return row.querySelector<HTMLElement>('[contenteditable="true"]');
}

/** Presses `text`, `offset` characters in, at (`x`, `y`): the browser reads that place under the
 *  pointer (`caretPositionFromPoint`), and the press goes down and comes up on the words. A `tap`
 *  is a touch's press, which sends the same mouse events after it. */
async function press(
  user: UserEvent,
  text: Text,
  offset: number,
  { tap = false, x = 200, y = 300 }: { tap?: boolean; x?: number; y?: number } = {}
): Promise<void> {
  Object.defineProperty(document, "caretPositionFromPoint", {
    configurable: true,
    value: () => ({ offset, offsetNode: text }),
  });
  const target = text.parentElement;
  if (target === null) throw new Error("the pressed words stand in no element");
  await user.pointer({ coords: { x, y }, keys: tap ? "[TouchA]" : "[MouseLeft]", target });
}

/** Waits until the row holds its editor with the focus in it and the caret collapsed `offset`
 *  characters into the editor's text that reads `words`, and answers that text. */
async function caretIn(row: HTMLElement, words: string, offset: number): Promise<Text> {
  let found: Text | undefined;
  await waitFor(
    () => {
      const editable = editorIn(row);
      expect(editable, "a press on the text opened no editor").not.toBeNull();
      expect(document.activeElement === editable, "the row's editor holds no focus").toBe(true);
      const selection = window.getSelection();
      const anchor = selection?.anchorNode;
      expect(anchor?.textContent, "the caret is in other words").toBe(words);
      expect(selection?.anchorOffset, "the caret is not where the press landed").toBe(offset);
      expect(selection?.isCollapsed).toBe(true);
      found = anchor as Text;
    },
    { timeout: 10_000 }
  );
  return found as Text;
}

/** Lets the press's end, its click's task and a frame pass. */
async function settle(): Promise<void> {
  await waitInAct(0);
  await waitInAct(20);
}

/** Fails where the tree draws any editor, or asked the page for one since `from`. */
function expectNoEditor(view: { container: HTMLElement; edited: readonly unknown[] }, from = 0) {
  expect(view.container.querySelector('[contenteditable="true"]'), "a press opened an editor").toBe(
    null
  );
  expect(view.edited.slice(from), "a press asked for the document's editor").toEqual([]);
}

/** A plain press in the middle of the first statement's words opens its editor there: what each
 *  case that keeps a press's old behaviour ends on, so a tree with no press of its own fails. */
async function pressOpensFirst(user: UserEvent, container: HTMLElement): Promise<void> {
  const row = rowOf(container, "spec#b-r1");
  await press(user, drawnText(row, FIRST), IN_DRAWS);
  await caretIn(row, FIRST, IN_DRAWS);
}

/** Lays the statement's row, its editor's box and its block out by hand, since happy-dom lays
 *  nothing out: the row's top 30 px down, under a band of 60 px at the top, so a row that placed
 *  itself as it took the focus would scroll the page; the box 100 px down; the statement's block
 *  500 px into the box, less the box's scroll. The page's scroll is watched and moves nothing. */
function layoutByHand(): { readonly restore: () => void; readonly scrolled: () => number } {
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    if (this.matches("[data-node-row]")) return new DOMRect(0, 30, 800, 400);
    const box = this.closest<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`);
    if (box === null) return new DOMRect();
    if (box === this) return new DOMRect(0, 100, 800, 600);
    if (this.dataset.blockId === "r1") return new DOMRect(0, 600 - box.scrollTop, 800, 24);
    return new DOMRect();
  });
  const by = spyOn(window, "scrollBy").mockImplementation(() => {});
  const to = spyOn(window, "scrollTo").mockImplementation(() => {});
  const published = document.documentElement.style;
  published.setProperty(IDENTITY_HEIGHT_PROPERTY, "60px");
  published.setProperty(FOOT_HEIGHT_PROPERTY, "100px");
  return {
    restore: () => {
      boxes.mockRestore();
      by.mockRestore();
      to.mockRestore();
      published.removeProperty(IDENTITY_HEIGHT_PROPERTY);
      published.removeProperty(FOOT_HEIGHT_PROPERTY);
    },
    scrolled: () => by.mock.calls.length + to.mock.calls.length,
  };
}

test("a press on the words of a row that takes a caret opens its editor with the caret where it landed, moves no page, keeps the word under the pointer, and a typed letter lands there", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(issue([theirComment, statement("r1", FIRST), statement("r2", SECOND)]));
  const layout = layoutByHand();
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r1");
    const scrolledBefore = layout.scrolled();
    await press(user, drawnText(row, FIRST), IN_DRAWS, { y: 300 });
    const caret = await caretIn(row, FIRST, IN_DRAWS);
    expect(view.edited.at(-1)).toBe(specDocument.id);
    await settle();
    expect(layout.scrolled() - scrolledBefore, "the press moved the page").toBe(0);
    // The editor's box is scrolled so the caret's line stands at the press's y.
    const block = row.querySelector<HTMLElement>('[data-block-id="r1"]');
    const box = block?.getBoundingClientRect();
    expect(
      box === undefined ? undefined : (box.top + box.bottom) / 2,
      "the word left the pointer"
    ).toBe(300);
    await user.keyboard("X");
    expect(caret.data, "the typed letter did not land where the press did").toBe(
      "The page draXws each row whole."
    );
  } finally {
    layout.restore();
    view.unmount();
  }
});

test("a press that dragged across a row's words to select them opens no editor, and the selection stays", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(issue([theirComment, statement("r1", FIRST)]));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r1");
    const text = drawnText(row, FIRST);
    const target = text.parentElement as HTMLElement;
    Object.defineProperty(document, "caretPositionFromPoint", {
      configurable: true,
      value: () => ({ offset: IN_DRAWS, offsetNode: text }),
    });
    const from = view.edited.length;
    await user.pointer([
      { keys: "[MouseLeft>]", offset: 4, target },
      { offset: 14, target },
      { keys: "[/MouseLeft]", target },
    ]);
    await settle();
    expectNoEditor(view, from);
    expect(window.getSelection()?.toString(), "the drag's selection is gone").toBe("page draws");
    expect(focusedRow(view.container)).toBe("spec#b-r1");

    await pressOpensFirst(user, view.container);
  } finally {
    view.unmount();
  }
});

test("a press on the words of the row being edited is its editor's own, and asks for no editor again", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(issue([theirComment, statement("r1", FIRST)]));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await pressOpensFirst(user, view.container);
    const row = rowOf(view.container, "spec#b-r1");
    const asked = view.edited.length;
    // The press lands in the editor's words, 9 characters in, where the editor puts its own
    // caret; the place the browser reads under the pointer is 2 characters in, which a press read
    // as the outline's would put the caret at.
    const editorText = (editorIn(row)?.querySelector('[data-block-id="r1"]')?.firstChild ??
      null) as Text | null;
    if (editorText === null) throw new Error("the row's editor draws no words");
    Object.defineProperty(document, "caretPositionFromPoint", {
      configurable: true,
      value: () => ({ offset: 2, offsetNode: editorText }),
    });
    await user.pointer({
      keys: "[MouseLeft]",
      offset: 9,
      target: editorText.parentElement as HTMLElement,
    });
    await settle();
    await settle();
    expect(window.getSelection()?.anchorNode).toBe(editorText);
    expect(window.getSelection()?.anchorOffset, "the press in the editor was taken from it").toBe(
      9
    );
    expect(view.edited.slice(asked), "the press in the editor asked for the editor again").toEqual(
      []
    );
    expect(document.activeElement === editorIn(row)).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a press on a row that takes no caret moves the focus to it and opens no editor: another person's comment, and a statement of a closed issue", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const root = issue([statement("r1", FIRST), theirComment]);
  const closed = renderTree(root, { isClosed: true });
  try {
    await waitFor(() => expect(focusedRow(closed.container)).toBe("spec#b-r1"));
    const comment = rowOf(closed.container, "comment-1");
    await press(user, drawnText(comment, "Another person's words on the issue."), 8);
    await settle();
    expect(focusedRow(closed.container), "the press did not move the focus").toBe("comment-1");
    expectNoEditor(closed);
    const row = rowOf(closed.container, "spec#b-r1");
    await press(user, drawnText(row, FIRST), IN_DRAWS);
    await settle();
    expect(focusedRow(closed.container), "the press did not move the focus").toBe("spec#b-r1");
    expectNoEditor(closed);
  } finally {
    closed.unmount();
  }

  const open = renderTree(root);
  try {
    await waitFor(() => expect(focusedRow(open.container)).toBe("spec#b-r1"));
    const comment = rowOf(open.container, "comment-1");
    await press(user, drawnText(comment, "Another person's words on the issue."), 8);
    await settle();
    expect(focusedRow(open.container), "the press did not move the focus").toBe("comment-1");
    expectNoEditor(open);
    await pressOpensFirst(user, open.container);
  } finally {
    open.unmount();
  }
});

test("a press on a link in a row's words follows the link and opens no editor", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const linked = "Read dispatch://CORE-2 first, then the rest.";
  const view = renderTree(issue([theirComment, statement("r1", FIRST), statement("r2", linked)]));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r2");
    const link = row.querySelector<HTMLAnchorElement>("[data-node-words] a[href]");
    if (link === null) throw new Error("the statement's words draw no link");
    await user.click(link);
    await settle();
    expect(view.address(), "the press did not follow the link").toBe("/issues/CORE-2");
    expectNoEditor(view);

    await pressOpensFirst(user, view.container);
  } finally {
    view.unmount();
  }
});

test("a press on a control in a row does the control's act and opens no editor: a chip and Copy reference", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
  const clipboard = navigator.clipboard;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const reference = `dispatch://${KEY}/spec#b-r1`;
  const view = renderTree(
    issue([
      theirComment,
      statement("r1", FIRST, {
        chips: [
          {
            answered: false,
            gone: false,
            name: "The second statement",
            ref: `dispatch://${KEY}/spec#b-r2`,
          },
        ],
        reference,
      }),
      statement("r2", SECOND),
    ])
  );
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r1");
    // The row holds the focus, so it draws its acts.
    await act(async () => row.focus());
    await settle();
    const copy = await waitFor(() => {
      const found = [...row.querySelectorAll("button")].find(
        (button) => button.textContent === CONTROL_LABELS.copy_reference
      );
      expect(found, "the focused row draws no Copy reference").toBeDefined();
      return found as HTMLButtonElement;
    });
    const from = view.edited.length;
    await user.click(copy);
    await settle();
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(reference));
    expectNoEditor(view, from);

    const chip = row.querySelector<HTMLAnchorElement>("[data-node-trail] a[href]");
    if (chip === null) throw new Error("the row's words draw no chip");
    const href = chip.getAttribute("href");
    await user.click(chip);
    await settle();
    expect(view.address(), "the press on the chip did not follow it").toBe(href ?? "");
    expectNoEditor(view, from);

    await pressOpensFirst(user, view.container);
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: clipboard });
    view.unmount();
  }
});

test("Enter on a row that takes a caret opens its editor at the block's start, before and after a press opened it at the press", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(issue([theirComment, statement("r1", FIRST)]));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r1");
    await act(async () => row.focus());
    await user.keyboard("{Enter}");
    await caretIn(row, FIRST, 0);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(editorIn(row)).toBeNull());

    await pressOpensFirst(user, view.container);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(editorIn(row)).toBeNull());
    expect(document.activeElement).toBe(row);
    await user.keyboard("{Enter}");
    await caretIn(row, FIRST, 0);
  } finally {
    view.unmount();
  }
});

test("a tap on the words of a row that takes a caret at 390 px opens its editor with the caret where it landed", async () => {
  setViewport(390);
  const user = userEvent.setup();
  const view = renderTree(issue([theirComment, statement("r1", FIRST)]));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r1");
    await press(user, drawnText(row, FIRST), IN_DRAWS, { tap: true });
    await caretIn(row, FIRST, IN_DRAWS);
    expect(view.edited.at(-1)).toBe(specDocument.id);
  } finally {
    view.unmount();
  }
});

test("a double click on a row's words opens its editor with the caret there on the first click, and the second is the editor's own", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(issue([theirComment, statement("r1", FIRST)]));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r1");
    await press(user, drawnText(row, FIRST), IN_DRAWS);
    const caret = await caretIn(row, FIRST, IN_DRAWS);
    const asked = view.edited.length;
    // The second click lands on the editor, which stands where the words were by then, and the
    // browser selects the word under it, "draws", as a double click does; its click counts 2.
    const target = caret.parentElement as HTMLElement;
    const at = { bubbles: true, button: 0, cancelable: true, clientX: 200, clientY: 300 };
    await act(async () => {
      target.dispatchEvent(new MouseEvent("mousedown", { ...at, detail: 2 }));
      window
        .getSelection()
        ?.setBaseAndExtent(caret, "The page ".length, caret, "The page draws".length);
      target.dispatchEvent(new MouseEvent("mouseup", { ...at, detail: 2 }));
      target.dispatchEvent(new MouseEvent("click", { ...at, detail: 2 }));
      target.dispatchEvent(new MouseEvent("dblclick", { ...at, detail: 2 }));
    });
    await settle();
    await settle();
    expect(window.getSelection()?.toString(), "the second click's word is not selected").toBe(
      "draws"
    );
    expect(view.edited.slice(asked), "the second click asked for the editor again").toEqual([]);
    expect(document.activeElement === editorIn(row)).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a press with Shift, Ctrl, Alt or Meta held moves the focus to the row and opens no editor", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(issue([theirComment, statement("r1", FIRST)]));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r1");
    for (const key of ["Shift", "Control", "Alt", "Meta"]) {
      await act(async () => rowOf(view.container, "comment-1").focus());
      await user.keyboard(`{${key}>}`);
      await press(user, drawnText(row, FIRST), IN_DRAWS);
      await user.keyboard(`{/${key}}`);
      await settle();
      expect(focusedRow(view.container), `a press with ${key} held left the focus`).toBe(
        "spec#b-r1"
      );
      expectNoEditor(view);
    }

    await pressOpensFirst(user, view.container);
  } finally {
    view.unmount();
  }
});

test("a press on a row while another row's editor is open closes that editor and opens the pressed row's with the caret at the press", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderTree(issue([theirComment, statement("r1", FIRST), statement("r2", SECOND)]));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await pressOpensFirst(user, view.container);
    const first = rowOf(view.container, "spec#b-r1");
    const second = rowOf(view.container, "spec#b-r2");
    await press(user, drawnText(second, SECOND), "A second st".length);
    await caretIn(second, SECOND, "A second st".length);
    expect(editorIn(first), "the first row's editor stayed open").toBeNull();
    expect(drawnText(first, FIRST).data, "the first row does not draw its words again").toBe(FIRST);
    expect(focusedRow(view.container)).toBe("spec#b-r2");
  } finally {
    view.unmount();
  }
});

test("a press on an atom in a row's words opens its editor with the caret just after the atom", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const after = " and then the rest.";
  const text = `Kept by <name>${after}`;
  const view = renderTree(
    issue([
      theirComment,
      statement("r1", FIRST),
      statement("r3", text, { tree: displayTree(text) }),
    ]),
    {
      held: {
        r3: (
          <>
            {"Kept by "}
            <span contentEditable={false} data-type="html">
              {"<name>"}
            </span>
            {after}
          </>
        ),
      },
    }
  );
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const row = rowOf(view.container, "spec#b-r3");
    const atom = await waitFor(() => {
      const found = row.querySelector<HTMLElement>('[data-node-words] [data-type="html"]');
      expect(found?.textContent, "the row draws no inline html").toBe("<name>");
      return found as HTMLElement;
    });
    await press(user, atom.firstChild as Text, 3);
    const caret = await caretIn(row, after, 0);
    expect(caret.previousSibling, "the caret is not just after the atom").toBe(
      row.querySelector('[data-block-id="r3"] [data-type="html"]')
    );
    await user.keyboard("X");
    expect(row.querySelector('[data-block-id="r3"]')?.textContent).toBe(
      "Kept by <name>X and then the rest."
    );
  } finally {
    view.unmount();
  }
});

test("a press while an address's landing is undecided is the reader's act: the editor opens, and the landing moves the focus no more", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const addressed = node({
    author: { id: "bob", kind: "user" },
    id: "comment-9",
    text: "The comment the address names, which a later read brings.",
    type: "comment",
  });
  const before = issue([theirComment, statement("r1", FIRST)]);
  const acts = abortOnReaderAct();
  const addressActs = { current: acts };
  const view = renderTree(before, { addressActs, reveal: [before, addressed] });
  try {
    await pressOpensFirst(user, view.container);
    const row = rowOf(view.container, "spec#b-r1");
    view.reread(issue([theirComment, statement("r1", FIRST), addressed]));
    rowOf(view.container, "comment-9");
    await settle();
    await settle();
    expect(focusedRow(view.container), "the landing took the focus from the editor").toBe(
      "spec#b-r1"
    );
    expect(document.activeElement === editorIn(row), "the editor lost the focus").toBe(true);
    expect(window.getSelection()?.anchorOffset).toBe(IN_DRAWS);
  } finally {
    acts.abort();
    view.unmount();
  }
});
