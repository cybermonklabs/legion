import { expect, test } from "bun:test";
import * as Y from "yjs";

import { bindBlockEditor } from "./block-editor";
import { KEY_REPORTS } from "./page-structure";

function paragraph(id: string, text: string) {
  const block = new Y.XmlElement("paragraph");
  block.setAttribute("blockId", id);
  block.insert(0, [new Y.XmlText(text)]);
  return block;
}

function setup() {
  const room = new Y.Doc();
  const peer = new Y.Doc();
  const spec = room.getXmlFragment("spec");
  const verification = room.getXmlFragment("verification");
  spec.insert(0, [paragraph("a", "First line."), paragraph("b", "Second line.")]);
  verification.insert(0, [paragraph("c", "Verification line.")]);
  Y.applyUpdate(peer, Y.encodeStateAsUpdate(room));
  room.on("update", (update, origin) => {
    if (origin !== peer) Y.applyUpdate(peer, update, room);
  });
  peer.on("update", (update, origin) => {
    if (origin !== room) Y.applyUpdate(room, update, peer);
  });

  const root = document.body.appendChild(document.createElement("div"));
  const hidden = root.appendChild(document.createElement("div"));
  hidden.inert = true;
  const dom = hidden.appendChild(document.createElement("div"));
  dom.contentEditable = "true";
  const rows = ["a", "b", "c"].map((id) => {
    const row = root.appendChild(document.createElement("div"));
    row.tabIndex = 0;
    const words = row.appendChild(document.createElement("p"));
    words.textContent = `${id} words`;
    return { id, row, words };
  });
  const bound: string[] = [];
  const reports: string[] = [];
  const cursorEvents: string[] = [];
  const comments: string[] = [];
  const writes: string[] = [];
  const disposers: (() => void)[] = [];
  let caret = -1;
  let current: Y.XmlElement | undefined;
  let undo: Y.UndoManager | undefined;
  const ownEdit = {};
  const editor = {
    view: { dom },
    rebindElement(element: Y.XmlElement) {
      current = element;
      undo?.destroy();
      undo = new Y.UndoManager(element, { trackedOrigins: new Set([ownEdit]) });
      bound.push(element.getAttribute("blockId") ?? "missing id");
      const draw = () => {
        if (current === element) dom.textContent = element.toString();
      };
      element.observeDeep(draw);
      disposers.push(() => element.unobserveDeep(draw));
      draw();
    },
    setCaret(at: number) {
      caret = at;
      dom.focus();
    },
    commentOnSelection() {
      comments.push(current?.getAttribute("blockId") ?? "missing id");
      return true;
    },
    undo() {
      undo?.undo();
    },
    remoteCursor(id: string) {
      cursorEvents.push(id);
    },
  };
  const controller = bindBlockEditor({
    hidden,
    editor,
    report: (line: string) => reports.push(line),
    request: (url: string) => writes.push(url),
  });
  const find = (doc: Y.Doc, id: string) => {
    const fragment = doc.getXmlFragment(id === "c" ? "verification" : "spec");
    const block = fragment
      .toArray()
      .find((element) => element instanceof Y.XmlElement && element.getAttribute("blockId") === id);
    if (!(block instanceof Y.XmlElement)) throw new Error(`No block ${id}`);
    return block;
  };
  function type(doc: Y.Doc, id: string, text: string) {
    const held = find(doc, id).get(0);
    if (!(held instanceof Y.XmlText)) throw new Error(`Block ${id} has no text`);
    doc.transact(() => held.insert(0, text), ownEdit);
  }
  function bind(id: string, at = 0) {
    const row = rows.find((entry) => entry.id === id);
    if (!row) throw new Error(`No row ${id}`);
    controller.bind({ ...row, element: find(room, id), at });
    return row;
  }
  function remove(id: string, merge = false) {
    const fragment = peer.getXmlFragment("spec");
    const at = fragment.toArray().indexOf(find(peer, id));
    peer.transact(() => {
      if (merge) type(peer, "b", "Merged first line. ");
      fragment.delete(at, 1);
    }, "peer");
  }
  return {
    room,
    peer,
    dom,
    hidden,
    editor,
    controller,
    bound,
    reports,
    cursorEvents,
    comments,
    writes,
    rows,
    caret: () => caret,
    find,
    type,
    bind,
    remove,
    cleanup() {
      controller.destroy();
      undo?.destroy();
      for (const dispose of disposers) dispose();
      room.destroy();
      peer.destroy();
      root.remove();
    },
  };
}

function row(name: string, run: (f: ReturnType<typeof setup>) => void) {
  test(name, () => {
    const f = setup();
    try {
      run(f);
    } finally {
      f.cleanup();
    }
  });
}

row("Enter draws the kept editor once in the row and starts the caret at zero", (f) => {
  const a = f.bind("a");
  expect(a.row.querySelector("[data-editor-box]")?.contains(f.dom)).toBe(true);
  expect(f.caret()).toBe(0);
  expect(f.bound).toEqual(["a"]);
  expect(f.writes).toEqual([]);
});

row("a click moves the same editor to the pressed offset", (f) => {
  f.bind("a", 4);
  expect(f.caret()).toBe(4);
  expect(f.bound).toEqual(["a"]);
});

row("binding another row restores the first row's words without rebuilding the editor", (f) => {
  const a = f.bind("a");
  const b = f.bind("b");
  expect(a.words.isConnected).toBe(true);
  expect(a.row.contains(f.dom)).toBe(false);
  expect(b.row.contains(f.dom)).toBe(true);
  expect(f.bound).toEqual(["a", "b"]);
});

row("Escape restores the row's words and parks the editor without destroying the room", (f) => {
  const a = f.bind("a");
  f.controller.keydown(new KeyboardEvent("keydown", { key: "Escape", cancelable: true }));
  expect(f.hidden.contains(f.dom)).toBe(true);
  expect(a.words.isConnected).toBe(true);
  expect(document.activeElement).toBe(a.row);
  expect(f.room.getXmlFragment("spec").length).toBe(2);
});

for (const merge of [false, true]) {
  row(`a ${merge ? "merged" : "deleted"} block closes the editor and names removal`, (f) => {
    f.bind("a");
    f.remove("a", merge);
    expect(f.reports).toEqual([KEY_REPORTS.editor_block_removed]);
    expect(f.hidden.contains(f.dom)).toBe(true);
    expect(f.controller.keydown(new KeyboardEvent("keydown", { key: "x" }))).toBe(false);
    expect(f.find(f.room, "b").toString()).not.toContain("xSecond");
  });
}

row("deleting a block with an unacknowledged local edit reports that it was not saved", (f) => {
  f.bind("a");
  f.controller.noteUnacknowledgedEdit("not kept");
  f.remove("a");
  expect(f.reports).toEqual([KEY_REPORTS.editor_text_not_saved]);
});

row("a schema reconnect leaves the same block writable without a reload report", (f) => {
  f.bind("a");
  const held = f.find(f.room, "a");
  f.controller.admissionChanged(false);
  f.type(f.room, "a", "after reconnect ");
  expect(f.find(f.room, "a")).toBe(held);
  expect(f.find(f.peer, "a").toString()).toContain("after reconnect");
  expect(f.reports).toEqual([]);
});

row(
  "the block editor keeps each behaviour of the base and ends on each removal of its block",
  (f) => {
    f.bind("a");
    f.type(f.room, "a", "one ");
    f.bind("b");
    f.type(f.room, "b", "two ");
    f.controller.keydown(new KeyboardEvent("keydown", { key: "z", ctrlKey: true }));
    expect(f.find(f.peer, "b").toString()).not.toContain("two ");
    expect(f.find(f.peer, "a").toString()).toContain("one ");
    f.controller.keydown(new KeyboardEvent("keydown", { key: "m", ctrlKey: true, altKey: true }));
    expect(f.comments).toEqual(["b"]);
    f.controller.receiveCursor("bob");
    expect(f.cursorEvents).toEqual(["bob"]);
    f.bind("a");
    f.remove("a", true);
    expect(f.reports).toEqual([KEY_REPORTS.editor_block_removed]);
  }
);

row("a collaborator's edit and cursor reach the block now bound", (f) => {
  f.bind("a");
  f.type(f.peer, "a", "from Bob ");
  f.controller.receiveCursor("bob");
  expect(f.dom.textContent).toContain("from Bob");
  expect(f.cursorEvents).toEqual(["bob"]);
});

row("typing changes only the block's content, not its attributes, sibling or marks", (f) => {
  const attrs = f.find(f.room, "a").getAttributes();
  const sibling = f.find(f.room, "b").toString();
  f.room.getMap("marks").set("one", { quoted: "First line." });
  f.bind("a");
  f.type(f.room, "a", "new ");
  expect(f.find(f.peer, "a").getAttributes()).toEqual(attrs);
  expect(f.find(f.peer, "b").toString()).toBe(sibling);
  expect(f.peer.getMap("marks").toJSON()).toEqual({ one: { quoted: "First line." } });
});

row("ArrowDown from the last line after a hard break stays inside the bound block", (f) => {
  const element = f.find(f.room, "a");
  element.insert(1, [new Y.XmlElement("hardbreak"), new Y.XmlText("Last line.")]);
  f.bind("a", "First line.\n".length);
  const arrow = new KeyboardEvent("keydown", { key: "ArrowDown", cancelable: true });
  expect(f.controller.keydown(arrow)).toBe(true);
  expect(f.dom).toBe(document.activeElement);
});

row("the converted room also edits the Verification fragment on the kept editor", (f) => {
  f.bind("a");
  const before = f.room.getXmlFragment("spec").toString();
  f.bind("c");
  f.type(f.room, "c", "revised check ");
  expect(f.peer.getXmlFragment("verification").toString()).toContain("revised check");
  expect(f.peer.getXmlFragment("spec").toString()).toBe(before);
  expect(f.bound).toEqual(["a", "c"]);
});
