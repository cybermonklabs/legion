import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { QueryClient } from "@tanstack/react-query";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";
import * as Y from "yjs";

import { onFakeClock, settle, tick } from "../../__tests__/fake-clock";
import type { ConnectionCallbacks } from "../doc/connection";
import { bindBlockEditor } from "./block-editor";
import { createBlockRoom } from "./block-room";
import { openEarlyCaret } from "./early-caret";
import { watchLoadEnd } from "./load-end";
import { KEY_REPORTS } from "./page-structure";
import { splitSync } from "./sync-parts";

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
  const words = row.appendChild(document.createElement("p"));
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
    buildEditor: async () => editor,
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
    words,
    editorDom,
    binding,
    drain,
    start,
    load,
    caret: () => caret,
    sync() {
      if (!callbacks) throw new Error("the page did not open a document socket");
      callbacks.onAdmission(false);
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
