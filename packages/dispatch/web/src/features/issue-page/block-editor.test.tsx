import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import * as Y from "yjs";

import { onFakeClock, settle, tick } from "../../__tests__/fake-clock";
import { issueTreeKey } from "../../api/queries";
import type { Ask, Comment, DecisionEntry } from "../../api/types";
import type { ConnectionCallbacks } from "../doc/connection";
import type { EditorHandle } from "../doc/editor";
import { issueSpecAddress } from "../doc/spec-marks";
import { MarginProvider, useMargin } from "../margin/margin-context";
import { anchoredAsk, comment, issue, specArtifact } from "../margin/margin-fixture";
import { marginItemId, useMarginItems } from "../margin/useMarginItems";
import { bindBlockEditor, IssueBlockRoomContext, type KeptBind } from "./block-editor";
import { createBlockRoom } from "./block-room";
import {
  artifact,
  decision,
  entryOf,
  node,
  shaped,
  specRef,
} from "./decisions-filter/decisions-fixtures";
import { openEarlyCaret } from "./early-caret";
import { anchoredCount, KeptBlockReferences } from "./kept-editor-parts";
import { watchLoadEnd } from "./load-end";
import { KEY_REPORTS } from "./page-structure";
import { splitSync } from "./sync-parts";
import { commentNode, treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import { useNodeEditing } from "./useNodeEditing";

function capture(): Uint8Array {
  const stored: unknown = JSON.parse(
    readFileSync(new URL("./fixtures/sync-frames/multiple.json", import.meta.url), "utf8")
  );
  if (typeof stored !== "string") throw new Error("multiple.json must contain one base64 frame");
  const decoder = createDecoder(Buffer.from(stored, "base64"));
  readVarString(decoder);
  expect(readVarUint(decoder)).toBe(0);
  expect(readVarUint(decoder)).toBe(1);
  return readVarUint8Array(decoder);
}

const spec = { id: "spec", fragment: "prosemirror" };
const WORDS = "The first line.";

function withPage(run: (page: ReturnType<typeof setup>) => Promise<void>) {
  return onFakeClock(async () => {
    const page = setup();
    try {
      await run(page);
    } finally {
      page.destroy();
    }
  });
}

function setup() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { gcTime: Infinity, retry: false } },
  });
  const keys: unknown[] = [];
  const unsubscribe = queryClient
    .getQueryCache()
    .subscribe(({ query }) => keys.push(query.queryKey));
  const doc = new Y.Doc();
  const text = new Y.XmlText(WORDS);
  const block = new Y.XmlElement("paragraph");
  block.setAttribute("blockId", "r1");
  block.insert(0, [text]);
  doc.getXmlFragment(spec.fragment).insert(0, [block]);
  const host = document.body.appendChild(document.createElement("div"));
  const row = host.appendChild(document.createElement("div"));
  row.tabIndex = 0;
  row.setAttribute("data-node-row", "");
  row.setAttribute("data-row-key", "r1");
  row.setAttribute("data-editable", "");
  const words = row.appendChild(document.createElement("p"));
  words.setAttribute("data-node-words", "");
  words.textContent = WORDS;
  const hidden = host.appendChild(document.createElement("div"));
  const editorDom = hidden.appendChild(document.createElement("div"));
  editorDom.contentEditable = "true";
  const opened: string[] = [];
  const reports: string[] = [];
  const queue: (() => void)[] = [];
  let callbacks: ConnectionCallbacks | undefined;
  let caret = 0;
  const editor = {
    view: { dom: editorDom },
    destroy() {},
    rebindElement(element: Y.XmlElement) {
      editorDom.textContent = element.toString();
    },
    setCaret(at: number) {
      caret = at;
      editorDom.focus();
    },
  };
  const keptView = document.createElement("div");
  keptView.contentEditable = "true";
  const kept = {
    view: { dom: keptView, editable: true },
    destroy() {},
    rebindElement(element: Y.XmlElement) {
      keptView.textContent = element
        .toArray()
        .map((child) => child.toString())
        .join("");
    },
    setAccess(access: "edit" | "comment" | "read") {
      kept.view.editable = access === "edit";
      keptView.contentEditable = String(kept.view.editable);
    },
  };
  const room = createBlockRoom({
    spec,
    converted: true,
    queryClient,
    host,
    loadTransport: async () => (id: string, next: ConnectionCallbacks) => {
      opened.push(id);
      callbacks = next;
      return { doc, destroy() {} };
    },
    loadEditor: async () => {},
    loadStyles: async () => {},
    buildEditor: async (_, { root }) => {
      root.append(keptView);
      return kept as unknown as EditorHandle;
    },
    schedule: (run: () => void) => {
      queue.push(run);
    },
    mark: () => {},
    measure: () => {},
    report: (line: string) => reports.push(line),
  });
  const start = () => room.start();
  const origin = Date.now();
  const load = watchLoadEnd({
    queryClient,
    ready: () => true,
    subscribeReady: () => () => {},
    now: () => Date.now() - origin,
    observe: () => undefined,
    mark: () => {},
    start,
  });
  const binding = bindBlockEditor({ hidden, editor, report: (line: string) => reports.push(line) });
  const drain = async () => {
    await settle();
    for (let tasks = 0; queue.length > 0; tasks += 1) {
      if (tasks === 100) throw new Error("the room did not finish its steps");
      queue.shift()?.();
      await settle();
    }
  };
  return {
    doc,
    text,
    block,
    room,
    keys,
    opened,
    reports,
    row,
    host,
    words,
    editorDom,
    binding,
    drain,
    start,
    load,
    caret: () => caret,
    sync(readOnly = false) {
      if (!callbacks) throw new Error("the page did not open a document socket");
      callbacks.onAdmission(readOnly);
      callbacks.onSynced();
    },
    destroy() {
      load.destroy();
      room.destroy();
      binding.destroy();
      unsubscribe();
      queryClient.clear();
      doc.destroy();
      host.remove();
    },
  };
}

test("the page opens the room once, at the end of the load or at the first Enter, and reads no document", async () => {
  for (const early of [false, true]) {
    await withPage(async (page) => {
      expect(page.opened).toEqual([]);
      if (early) page.load.startNow();
      else await tick(1_500);
      await page.drain();
      expect(page.opened).toEqual([spec.id]);
      page.sync();
      await page.drain();
      page.room.start();
      await tick(1_500);
      await page.drain();
      expect(page.opened).toEqual([spec.id]);
      expect(page.keys).toEqual([]);
    });
  }
});

test("the first sync's parts applied in order give the same document as the whole captured update", () => {
  const update = capture();
  const whole = new Y.Doc();
  const split = new Y.Doc();
  try {
    const pieces = splitSync(update, 2);
    expect(pieces.length).toBeGreaterThan(1);
    Y.applyUpdate(whole, update);
    for (const part of pieces) Y.applyUpdate(split, part);
    expect(Y.encodeStateAsUpdate(split)).toEqual(Y.encodeStateAsUpdate(whole));
  } finally {
    whole.destroy();
    split.destroy();
  }
});

test("keys typed before the bind are applied in order, composition included", () =>
  withPage(async (page) => {
    const held = openEarlyCaret({
      row: page.row,
      words: page.words,
      offset: 0,
      editable: true,
      start: page.start,
      report: (line: string) => page.reports.push(line),
    });
    const typing = page.row.querySelector('[contenteditable="plaintext-only"]');
    expect(typing).toBe(document.activeElement);
    if (!held || !typing) throw new Error("the row did not draw a stand-in");
    function write(inserted: string, inputType: string) {
      if (!typing) throw new Error("stand-in disappeared before the bind");
      typing.textContent = inserted + WORDS;
      document.getSelection()?.collapse(typing.firstChild, inserted.length);
      typing.dispatchEvent(new InputEvent("input", { bubbles: true, inputType }));
    }
    try {
      write("abc", "insertText");
      typing.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true }));
      write("abcに", "insertCompositionText");
      write("abc日本", "insertCompositionText");
      typing.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: "日本" }));
      write("abc日本de", "insertText");
      expect(page.text.toString()).toBe(WORDS);
      await page.drain();
      page.sync();
      await page.drain();
      page.binding.bind({ row: page.row, words: page.words, element: page.block, at: 0 });
      await held.bind({
        replace(from: number, to: number, text: string) {
          page.doc.transact(() => {
            page.text.delete(from, to - from);
            page.text.insert(from, text);
          });
          page.editorDom.textContent = page.text.toString();
        },
        positionAt: (at: number) => at,
        keydown: (event: KeyboardEvent) => page.editorDom.dispatchEvent(event),
        select: (at: number) => editorSelection(page.editorDom, at),
        end: () => {},
      });
      expect(page.text.toString()).toBe(`abc日本de${WORDS}`);
      expect(document.getSelection()?.anchorOffset).toBe(7);
    } finally {
      held.destroy();
    }
  }));

for (const typed of [false, true]) {
  test(`a waiting Enter binds read-only ${typed ? "with" : "without"} held text`, () =>
    withPage(async (page) => {
      const room = { current: page.room };
      const container = { current: page.host };
      const artifacts = new Map([["spec", artifact(spec.id, "spec", true)]]);
      const wholeEditors: string[] = [];
      const hook = renderHook(
        () =>
          useNodeEditing({
            artifacts,
            container,
            onEditing: (id) => {
              if (id !== undefined) wholeEditors.push(id);
            },
            onReport: (line) => page.reports.push(line),
          }),
        {
          wrapper: ({ children }: { children: ReactNode }) => (
            <IssueBlockRoomContext.Provider value={room}>{children}</IssueBlockRoomContext.Provider>
          ),
        }
      );
      try {
        page.row.focus();
        act(() => hook.result.current.edit("r1", node({ id: "spec#b-r1", type: "requirement" })));
        const standIn = page.row.querySelector('[contenteditable="plaintext-only"]');
        expect(standIn).toBe(document.activeElement);
        if (standIn === null) throw new Error("Expected a stand-in before admission");
        if (typed) {
          standIn.textContent = `Unsaved ${WORDS}`;
          standIn.dispatchEvent(new InputEvent("input", { bubbles: true }));
        }
        await act(async () => {
          await page.drain();
          page.sync(true);
          await page.drain();
        });
        const box = page.row.querySelector("[data-editor-box]");
        if (box === null) throw new Error("The read-only admission did not bind the block");
        expect(
          box.querySelector('[data-read-only="true"] [contenteditable="false"]')
        ).not.toBeNull();
        expect(box.querySelector('[contenteditable="plaintext-only"]')).toBeNull();
        expect(page.text.toString()).toBe(WORDS);
        expect(box.textContent).toBe(WORDS);
        expect(page.reports).toEqual(typed ? [KEY_REPORTS.editor_text_not_saved] : []);
        expect(wholeEditors).toEqual([]);
        expect(page.keys).toEqual([]);
      } finally {
        hook.unmount();
      }
    }));
}

function editorSelection(editor: HTMLElement, at: number) {
  const contents = editor.firstChild;
  if (contents) document.getSelection()?.collapse(contents, at);
}

test("a deleted held block ends the editor and sets the status entry", () =>
  withPage(async (page) => {
    page.load.startNow();
    await page.drain();
    page.sync();
    await page.drain();
    page.binding.bind({ row: page.row, words: page.words, element: page.block, at: 0 });
    expect(page.row.contains(page.editorDom)).toBe(true);
    const peer = new Y.Doc();
    try {
      Y.applyUpdate(peer, Y.encodeStateAsUpdate(page.doc));
      peer.getXmlFragment(spec.fragment).delete(0, 1);
      Y.applyUpdate(page.doc, Y.encodeStateAsUpdate(peer, Y.encodeStateVector(page.doc)), peer);
      expect(page.row.contains(page.editorDom)).toBe(false);
      expect(page.reports).toEqual([KEY_REPORTS.editor_block_removed]);
    } finally {
      peer.destroy();
    }
  }));

/** A comment of the margin's fixture, anchored to the block `blockId` of the spec. */
function commentOn(id: string, blockId: string): Comment {
  if (comment.anchor === null) throw new Error("the fixture's comment has no document anchor");
  return { ...comment, anchor: { ...comment.anchor, block_id: blockId, mark_id: id }, id };
}

/** An ask of the margin's fixture, anchored to the block `blockId` of the document `artifactId`,
 *  as the asks read answers it. */
function askOn(
  id: string,
  blockId: string,
  state: Ask["state"],
  artifactId = specArtifact.id
): Ask {
  if (anchoredAsk.anchor === null) throw new Error("the fixture's ask has no document anchor");
  const anchor = { ...anchoredAsk.anchor, artifact_id: artifactId, block_id: blockId, mark_id: id };
  return { ...anchoredAsk, anchor, id, state };
}

/** An agent's decision of the spec, as the decisions read answers it, that shaped each block of
 *  `to` in the spec. */
function decisionShaping(block: string, to: readonly string[]): DecisionEntry {
  const document = issueSpecAddress(issue.key);
  return entryOf(
    decision(block, {
      links: to.map((id) => {
        const link = shaped(specRef(id));
        return { ...link, to: { ...link.to, block_id: id, document } };
      }),
    })
  );
}

/** The id of the margin's card of `entry` beside the spec's block `blockId` (`useMarginItems`). */
function decisionCard(entry: DecisionEntry, blockId: string): string {
  return `decision:${entry.node.id}:${issue.key}/spec#${blockId}`;
}

/** The bound block's references button over the tree reads `client` holds, and the margin's
 *  cards beside the spec by their ids, under the filter a press on the button sets. */
function ReferencesAndCards({ bind, client }: { bind: KeptBind; client: QueryClient }) {
  const { blockFilterGroup, blockFilterId, filterToBlocks } = useMargin();
  const { marginItems } = useMarginItems(
    { key: issue.key, kind: "issue" },
    "comments",
    specArtifact,
    new Map(),
    new Map(),
    blockFilterId,
    { blockFilterIds: blockFilterGroup?.ids, documentShown: true }
  );
  return (
    <>
      <KeptBlockReferences
        bind={bind}
        onReferencesPress={(_, blockId, blocks) => filterToBlocks(blockId, blocks)}
        reference={anchoredCount(client, issue.key, bind)}
      />
      <ul aria-label="Margin cards">
        {marginItems.map((item) => (
          <li key={marginItemId(item)}>{marginItemId(item)}</li>
        ))}
      </ul>
    </>
  );
}

/** Draws the references button of the spec's block `requirement`, bound with the block
 *  `paragraph` inside it, and the margin's cards, over reads that hold `comments`, `asks` and
 *  `decisions`: the tree read each comment's node and each ask's, with the block its anchor names
 *  (`block_anchor`). Answers the cards drawn, in order of their ids, and how to take it down. */
function drawReferences({
  asks = [],
  comments,
  decisions = [],
}: {
  readonly asks?: readonly Ask[];
  readonly comments: readonly Comment[];
  readonly decisions?: readonly DecisionEntry[];
}) {
  const doc = new Y.Doc();
  const element = new Y.XmlElement("requirement");
  const inner = new Y.XmlElement("paragraph");
  doc.getXmlFragment(spec.fragment).insert(0, [element]);
  element.setAttribute("blockId", "requirement");
  element.insert(0, [inner]);
  inner.setAttribute("blockId", "paragraph");
  const bind: KeptBind = {
    artifactId: specArtifact.id,
    box: document.createElement("div"),
    element,
  };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(
    issueTreeKey(issue.key),
    treeAnswer([
      ...comments.map((one) => commentNode(one)),
      ...asks.map((ask) => {
        if (ask.anchor === null || ask.anchor.block_id === null) throw new Error("no anchor");
        const { artifact_id, block_id } = ask.anchor;
        return treeNode(ask.id, "ask", issue.key, { block_anchor: { artifact_id, block_id } });
      }),
    ])
  );
  client.setQueryData(["comments", issue.key], comments);
  client.setQueryData(["asks", issue.key], asks);
  client.setQueryData(["decisions", issue.key], {
    decisions: [],
    document: null,
    entries: decisions,
    open_questions: [],
  });
  client.setQueryData(["inbox"], []);
  client.setQueryData(["user-state"], {});
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>
          <ReferencesAndCards bind={bind} client={client} />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    cards: () =>
      view
        .queryAllByRole("listitem")
        .map((card) => card.textContent ?? "")
        .sort(),
    close: () => {
      view.unmount();
      client.clear();
      doc.destroy();
    },
    view,
  };
}

// The bound block's references button counts the asks anchored to the block and to the blocks
// inside it beside its comments (R7, decision 567), as the whole document's editor counts them
// from the blocks read (`references.comments + references.asks`): from the tree read's ask nodes,
// each of which names the block its anchor stands in (`block_anchor`), so the bind reads nothing.
// An ask of another block, and one of a block of the same id in another document, is not counted.
// A press shows the cards it counted, the asks among them, and no other block's.
test("the bound block's references button counts the asks anchored to it and inside it beside its comments, and its press shows them", async () => {
  const drawn = drawReferences({
    asks: [
      askOn("ask on requirement", "requirement", "open"),
      askOn("ask on paragraph", "paragraph", "answered"),
      askOn("ask on another block", "other", "open"),
      askOn("ask on another document", "requirement", "open", "artifact-2"),
    ],
    comments: [
      commentOn("comment on requirement", "requirement"),
      commentOn("comment on paragraph", "paragraph"),
      commentOn("comment on another block", "other"),
    ],
  });
  try {
    const button = drawn.view.getByRole("button", { name: "4 references on block" });
    expect(button.getAttribute("data-block-references")).toBe("requirement");
    await userEvent.setup().click(button);
    await waitFor(() =>
      expect(drawn.cards()).toEqual([
        "ask on paragraph",
        "ask on requirement",
        "comment on paragraph",
        "comment on requirement",
      ])
    );
    expect(drawn.cards()).toHaveLength(Number(button.textContent));
  } finally {
    drawn.close();
  }
});

// A press on the bound block's references button filters the decisions beside the spec too, as
// the whole document's editor's press does for its one block (`useMarginItems`'s `decisions`): a
// decision shows where a block it shaped is the bound block or a block inside it, once for each
// such block, and a decision that shaped only another block is not shown. Before the press the
// margin draws every decision beside the block it shaped.
test("a press on the bound block's references button shows the decisions that shaped it or a block inside it, and none that shaped another block", async () => {
  const on = decisionShaping("d-on", ["requirement"]);
  const inside = decisionShaping("d-inside", ["paragraph"]);
  const both = decisionShaping("d-both", ["paragraph", "other"]);
  const elsewhere = decisionShaping("d-elsewhere", ["other"]);
  const drawn = drawReferences({
    comments: [commentOn("comment on requirement", "requirement")],
    decisions: [on, inside, both, elsewhere],
  });
  const shown = [
    "comment on requirement",
    decisionCard(on, "requirement"),
    decisionCard(inside, "paragraph"),
    decisionCard(both, "paragraph"),
  ];
  try {
    await waitFor(() =>
      expect(drawn.cards(), "the margin did not draw every decision before the press").toEqual(
        [...shown, decisionCard(both, "other"), decisionCard(elsewhere, "other")].sort()
      )
    );
    await userEvent
      .setup()
      .click(drawn.view.getByRole("button", { name: "1 references on block" }));
    await waitFor(() =>
      expect(drawn.cards(), "the press shows a decision that shaped another block").toEqual(
        [...shown].sort()
      )
    );
  } finally {
    drawn.close();
  }
});
