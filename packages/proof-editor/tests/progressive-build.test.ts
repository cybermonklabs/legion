// What `createProofEditor` does with a document of many blocks: it builds the editor a batch of
// blocks at a time, each batch its own task, shows the reader each batch as it lands, takes
// their edits in the blocks it has built, and has the collab binding adopt what the batches made
// (`src/progressive-build.ts`, dispatch://INFRA-341 R11). These rows read what that makes true of
// the editor a reader is handed: every block of the document is in its DOM, the root is shown
// with part of the document in it, the host is told which blocks each batch built, an edit the
// reader makes in a built block is in the Yjs document before the last batch, another person's
// change to a built block is in the editor in the task it arrives in, and the end of the build is
// three tasks - the last batch, the binding's adoption, and the host's swap - rather than the one
// task of 1,370 ms the three were on a phone at 4x CPU throttling. The last rows read how the
// thread goes back between batches, which decides whether anything else on the page runs while
// the build lasts.
import { describe, expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { Window as HappyWindow } from "happy-dom";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import { constructProofEditor, createProofEditor, type ProofEditorConstruction } from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
import { endOfTask } from "../src/progressive-build";
import { withDomWindow } from "./dom-window";
import { pastePlainText, withHTMLElementGlobal } from "./paste";

/** Blocks enough that the build takes several batches: the first takes eight and each one after
 *  it is sized by what the one before cost, growing fourfold at most. */
const BLOCKS = 160;

/** The words the paragraph at `index` of the generated document is made of. */
function paragraph(index: number): string {
  return `Paragraph ${index} of the ledger.`;
}

/** A document of `blocks` paragraphs, each with words of its own so a test can name one. */
function paragraphs(blocks: number): string {
  return `${Array.from({ length: blocks }, (_, index) => paragraph(index)).join("\n\n")}\n`;
}

/** A document of `BLOCKS` top-level blocks whose first is `opening` - a list, a quote, a table:
 *  a block whose own content is blocks, which the first batch builds, so another person's edit
 *  inside it lands in a block the batches have built. */
function openingWith(opening: string): string {
  const rest = Array.from({ length: BLOCKS - 1 }, (_, index) => paragraph(index + 1));
  return `${opening}\n\n${rest.join("\n\n")}\n`;
}

/** A document of `count` sections, each a heading of its own and a paragraph under it, which is
 *  `count * 2` top-level blocks. A heading in every batch is what brings Milkdown's
 *  `syncHeadingIdPlugin` into the build: it is a plugin view, so ProseMirror runs its `update`
 *  inside the `view.dispatch` each batch is placed with, and it dispatches an `id` for every
 *  heading the batch brought (`@milkdown/preset-commonmark`). */
function sections(count: number): string {
  return `${Array.from(
    { length: count },
    (_, index) => `## Section ${index}\n\n${paragraph(index)}`
  ).join("\n\n")}\n`;
}

/** The blocks the editor's root holds, at whatever depth. */
function blocksBuilt(root: HTMLElement): number {
  return root.querySelectorAll("[data-block-id]").length;
}

/** What one batch told the host: the ids it was about to build, how many blocks the editor held
 *  when it said so, and how many it held once they were in. */
interface Batch {
  readonly after: number;
  readonly before: number;
  readonly ids: readonly string[];
}

/** What one look at the build found: how many blocks the editor's root held, and whether the
 *  reader could see it. */
interface Look {
  readonly blocks: number;
  readonly shown: boolean;
}

/**
 * Reads how far the build has got, once per task, for as long as the build runs, and calls `act`
 * the first time it finds the editor holding part of the document and not all of it - which is a
 * batch that has landed while others have not, the only moment an edit can cross a build.
 *
 * The watcher is a real `setTimeout(0)` chain, not a fake clock: it waits for nothing, it reads
 * the editor once per task, and what it watches for is the build's own yield between batches,
 * which a fake clock would drive rather than observe.
 */
function watchTheBuild(
  root: HTMLElement,
  act: () => void
): { readonly looks: Look[]; stop: () => void } {
  const looks: Look[] = [];
  let acted = false;
  let watching = true;
  const look = () => {
    if (!watching) return;
    const blocks = blocksBuilt(root);
    looks.push({ blocks, shown: root.style.display !== "none" });
    if (!acted && blocks > 0 && blocks < BLOCKS) {
      acted = true;
      act();
    }
    setTimeout(look, 0);
  };
  setTimeout(look, 0);
  return {
    looks,
    stop: () => {
      watching = false;
    },
  };
}

/** A second client's copy of `ydoc`, for an edit made while the editor is being built. */
function peerOf(ydoc: Y.Doc): Y.Doc {
  const peer = new Y.Doc();
  Y.applyUpdate(peer, Y.encodeStateAsUpdate(ydoc));
  return peer;
}

/** Sends everything `peer` has that `ydoc` has not, as a collaborator's socket does. */
function sendToTheEditor(peer: Y.Doc, ydoc: Y.Doc): void {
  Y.applyUpdate(ydoc, Y.encodeStateAsUpdate(peer, Y.encodeStateVector(ydoc)));
}

/** A paragraph block, as a collaborator's client builds one. */
function paragraphBlock(id: string, text: string): Y.XmlElement {
  const block = new Y.XmlElement("paragraph");
  block.setAttribute("blockId", id);
  const content = new Y.XmlText();
  content.insert(0, text);
  block.insert(0, [content]);
  return block;
}

/** A block of a kind the schema has no node for, as a client of a newer schema writes one:
 *  `createNodeFromYElement` refuses it, and deletes it from the Yjs document as it refuses. */
function refusedBlock(id: string): Y.XmlElement {
  const block = new Y.XmlElement("quokka_block");
  block.setAttribute("blockId", id);
  const content = new Y.XmlText();
  content.insert(0, "Quokka.");
  block.insert(0, [content]);
  return block;
}

/** The `Y.XmlText` at `path` under `block`: what another person types into, inside the blocks a
 *  nested block is made of. */
function textAt(block: Y.XmlElement, path: readonly number[]): Y.XmlText {
  let at: Y.XmlElement | Y.XmlText | undefined = block;
  for (const step of path) {
    if (!(at instanceof Y.XmlElement)) throw new Error(`no element at ${path.join(".")}`);
    at = at.get(step) as Y.XmlElement | Y.XmlText | undefined;
  }
  if (!(at instanceof Y.XmlText)) throw new Error(`no text at ${path.join(".")}`);
  return at;
}

/** Where the editor's top-level block `index` starts. */
function startOf(doc: ProseMirrorNode, index: number): number {
  let pos = 0;
  for (let at = 0; at < index; at += 1) pos += doc.child(at).nodeSize;
  return pos;
}

/** Puts the caret `offset` characters into the editor's top-level block `index`. */
function caretIn(view: EditorView, index: number, offset: number): void {
  const at = startOf(view.state.doc, index) + 1 + offset;
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, at)));
}

/** The characters `node` holds, read off Yjs: a text's own inserts, and what everything under
 *  an element or a fragment holds. Stripping tags out of a `toString()` answers with text that
 *  is not the text, since the serialization spells a block's own `<script>` the way it spells
 *  the block, and a row would then pass on a document it never read. */
function textOf(node: Y.XmlFragment | Y.XmlText | Y.XmlHook): string {
  if (node instanceof Y.XmlText) {
    const delta = node.toDelta() as { insert?: unknown }[];
    return delta.map(({ insert }) => (typeof insert === "string" ? insert : "")).join("");
  }
  // A `Y.XmlElement` is a `Y.XmlFragment`, so this arm is both.
  return node instanceof Y.XmlFragment ? node.toArray().map(textOf).join("") : "";
}

/** The shape `node` holds, as Yjs holds it: each element's own name around the names of its
 *  children, with the text at the leaves. A block another person edits inside has to come out
 *  of the build the one block it went in as, and its text alone cannot say so - a list item
 *  split into three list items spells the same characters as the one item does. */
function shapeOf(node: Y.XmlFragment | Y.XmlText | Y.XmlHook): string {
  if (node instanceof Y.XmlText) return JSON.stringify(textOf(node));
  // A `Y.XmlElement` is a `Y.XmlFragment`, so this arm is first.
  if (node instanceof Y.XmlElement) {
    return `${node.nodeName}(${node.toArray().map(shapeOf).join(", ")})`;
  }
  return node instanceof Y.XmlFragment ? node.toArray().map(shapeOf).join(", ") : "";
}

/** The text of the Yjs document's top-level block `index`, which is what a reader's edit has to
 *  reach: read off the fragment itself, so nothing of the editor's own state is in the answer. */
function blockText(fragment: Y.XmlFragment, index: number): string {
  const block = fragment.get(index);
  return block === undefined ? "" : textOf(block);
}

/** The text of every top-level block of the Yjs document. */
function everyBlockText(fragment: Y.XmlFragment): string[] {
  return fragment.toArray().map((_, index) => blockText(fragment, index));
}

/** Every top-level block of the Yjs document, by the id it carries and the text it holds: what
 *  a build of a document nobody is editing has to leave exactly as it found it. */
function everyBlock(fragment: Y.XmlFragment): string[] {
  return fragment
    .toArray()
    .map(
      (block, index) =>
        `${block instanceof Y.XmlElement ? (block.getAttribute("blockId") ?? "-") : "-"}:` +
        blockText(fragment, index)
    );
}

/** What the build left behind, for a row to read. */
interface Built {
  readonly batches: Batch[];
  readonly blocks: number;
  readonly displayWhenHandled: string;
  readonly displayWhenRevealed: string;
  /** The Yjs document's own blocks as the build found them, and as it left them. */
  readonly documentAfter: string[];
  readonly documentBefore: string[];
  readonly looks: Look[];
  readonly markdown: string;
  readonly text: string;
  readonly writePath: string | undefined;
}

/** What a row is handed the first time it finds the build partway through. */
interface MidBuild {
  readonly fragment: Y.XmlFragment;
  readonly root: HTMLElement;
  readonly view: EditorView;
  readonly ydoc: Y.Doc;
}

/** The other two moments of a build a row acts in, each a task of the build's own. */
interface Hooks {
  /** Once the binding has adopted the batches' mapping and the handle is in hand: the reader's
   *  next keystroke, which is what carries whatever the build left in the editor into the Yjs
   *  document. */
  readonly afterwards?: (what: MidBuild) => void;
  /** In the task between the last batch and the binding's adoption - the build's own
   *  `await endOfTask()` before it adopts. A socket message lands there like any other. */
  readonly inTheGap?: (what: MidBuild) => void;
}

/**
 * Builds the editor over a Y.Doc holding `markdown`, with `act` run the first time the build is
 * found partway through and `hooks` run at the two moments after it, and answers what the build
 * left behind.
 */
async function build(
  markdown: string,
  act: (what: MidBuild) => void = () => {},
  hooks: Hooks = {}
): Promise<Built> {
  const { parseMarkdown } = await createHeadlessProof({});
  const ydoc = new Y.Doc();
  const fragment = ydoc.getXmlFragment("prosemirror");
  prosemirrorToYXmlFragment(parseMarkdown(markdown), fragment);
  const documentBefore = everyBlock(fragment);
  return withDomWindow(async (window) => {
    const root = window.document.body.appendChild(
      window.document.createElement("div")
    ) as unknown as HTMLElement;
    const batches: Batch[] = [];
    let view: EditorView | undefined;
    const watch = watchTheBuild(root, () => {
      if (view === undefined) throw new Error("the build never handed its view over");
      act({ fragment, root, view, ydoc });
    });
    let gap = hooks.inTheGap;
    const handle = await createProofEditor(root, {
      beforeBuild: (built) => {
        view = built;
      },
      heatMapMode: "hidden",
      onBatch: (ids) => {
        const before = blocksBuilt(root);
        return () => {
          batches.push({ after: blocksBuilt(root), before, ids });
          // The last batch: the editor holds every block the fragment has, so what the build
          // does next is the `await endOfTask()` it adopts after. A real 0 ms timer, as the
          // watcher above is and for the same reason: what this rides on is the order the
          // browsing context's own queue serves two tasks in - this one was set first, so it
          // runs in the gap, before the build's own continuation - which a fake clock would
          // drive rather than observe. It waits for nothing.
          const built = view;
          if (gap === undefined || built === undefined) return;
          if (built.state.doc.childCount !== fragment.length) return;
          const inTheGap = gap;
          gap = undefined;
          setTimeout(() => inTheGap({ fragment, root, view: built, ydoc }), 0);
        };
      },
      onMarkClick: () => {},
      onMarkHover: () => {},
      user: { color: "#0369a1", name: "alice" },
      ydoc,
    });
    watch.stop();
    const displayWhenHandled = root.style.display;
    const writePath = root.dataset.writePath;
    handle.reveal();
    const displayWhenRevealed = root.style.display;
    if (view !== undefined) hooks.afterwards?.({ fragment, root, view, ydoc });
    try {
      return {
        batches,
        blocks: blocksBuilt(root),
        displayWhenHandled,
        displayWhenRevealed,
        documentAfter: everyBlock(fragment),
        documentBefore,
        looks: watch.looks,
        markdown: handle.getMarkdown(),
        text: handle.view.dom.textContent ?? "",
        writePath,
      };
    } finally {
      const { view: built } = handle;
      handle.destroy();
      while (!built.isDestroyed) await Promise.resolve();
    }
  });
}

/** Every look that found the editor holding part of the document and not all of it. */
function partway(built: Built): Look[] {
  return built.looks.filter((look) => look.blocks > 0 && look.blocks < BLOCKS);
}

describe("a document built in batches", () => {
  test("ends with every one of its blocks in the DOM, having held part of it on the way", async () => {
    const built = await build(paragraphs(BLOCKS));

    expect(built.blocks).toBe(BLOCKS);
    // The build was in batches: at least one look found the editor holding some of the document
    // and not all of it, which a build done in one task never shows.
    expect(partway(built).length).toBeGreaterThan(0);
    expect(built.text).toContain(paragraph(0));
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  // The last batch, the binding's adoption and the host's swap were one task, and on a phone at
  // 4x CPU throttling that task was 1,370 ms - a freeze a reader sees. They are three tasks
  // now, so the look between them runs twice with the whole document built and the editor not
  // yet handed over: once while the adoption is still owed, and once while the swap is.
  test("ends in three tasks: the last batch, the binding's adoption and the host's swap", async () => {
    const built = await build(paragraphs(BLOCKS));

    expect(built.blocks).toBe(BLOCKS);
    expect(
      built.looks.filter((look) => look.blocks === BLOCKS).length,
      "the whole document was built, the binding adopted it and the handle was answered in one " +
        "task: nothing else could run between them, which is the freeze this build exists to end"
    ).toBeGreaterThanOrEqual(2);
  });

  // The reader waited eight to thirteen seconds for a document whose first batch was ready in
  // half of one. The root is hidden until that first batch and shown in the task it lands in, so
  // what a reader has from then on is the editor's own blocks, growing.
  test("shows the reader the editor with part of the document in it", async () => {
    const built = await build(paragraphs(BLOCKS));

    expect(
      partway(built).filter((look) => !look.shown),
      "the editor was built out of sight again: the reader waits for the last batch to see any " +
        "of it"
    ).toEqual([]);
    expect(built.displayWhenHandled).toBe("");
    expect(built.displayWhenRevealed).toBe("");
    // And the root says those blocks take edits that reach the Yjs document, which is what the
    // measurement reads the editor's first usable block by (`e2e/large-documents-page.ts`).
    expect(built.writePath).toBe("live");
  });

  test("hands the host each batch's blocks, before and after they are in the editor", async () => {
    const built = await build(paragraphs(BLOCKS));

    expect(built.batches.length).toBeGreaterThan(1);
    expect(built.batches[0]?.ids.length).toBe(8);
    // Every block of the document, once, in document order: what the host takes its own draw of
    // off the page as each batch lands.
    expect(built.batches.flatMap((batch) => batch.ids).length).toBe(BLOCKS);
    expect(new Set(built.batches.flatMap((batch) => batch.ids)).size).toBe(BLOCKS);
    // Each batch's blocks were in the editor between the call and what it answered, and nothing
    // of the batch was in it before.
    let standing = 0;
    for (const batch of built.batches) {
      expect({ after: batch.after, before: batch.before }).toEqual({
        after: standing + batch.ids.length,
        before: standing,
      });
      standing = batch.after;
    }
  });

  // Nobody edited this document, so the build has to leave it block for block as it found it.
  // It did not: Milkdown's `syncHeadingIdPlugin` dispatches an `id` for each heading a batch
  // brought, inside the very `view.dispatch` that batch is placed with, and the write path
  // reads that as a reader's edit and writes the editor's blocks through a window as many
  // blocks wide as the frontier says. The frontier used to move after that dispatch, so the
  // window was a batch short of the editor and `updateYFragment` wrote the difference into the
  // Yjs document - the batch's blocks a second time - and then, a batch later, with the
  // frontier counting that batch twice, deleted blocks the batches had still to reach. What
  // the reader was left with was a document missing blocks of its own, and blocks under ids
  // nothing had indexed (dispatch://INFRA-341, R11).
  test("leaves the Yjs document's blocks as it found them, under the ids it was given", async () => {
    const built = await build(sections(BLOCKS / 2));

    expect(built.documentBefore.length).toBe(BLOCKS);
    expect(built.documentAfter).toEqual(built.documentBefore);
    expect(built.blocks).toBe(BLOCKS);
    expect(partway(built).length).toBeGreaterThan(0);
  });
});

// The two phases a host whose document is still arriving needs: the editor is constructed with
// nothing of the document in it, and the binding builds the document into it
// (dispatch://INFRA-341, R11, `src/lib.ts`). Dispatch constructs as its socket is admitted and
// binds when the state frame has been applied, so what the construction must never do is put
// an empty document on the screen in front of the reader's own draw of the one they are
// reading.
describe("an editor constructed before its document arrives", () => {
  /** The editor built over a Y.Doc holding `markdown`, with nothing bound to it yet; its view
   *  comes from `beforeBuild`, which the construction calls over the empty document. */
  async function construct(
    window: HappyWindow,
    markdown: string
  ): Promise<{ construction: ProofEditorConstruction; root: HTMLElement; view: EditorView }> {
    const { parseMarkdown } = await createHeadlessProof({});
    const ydoc = new Y.Doc();
    prosemirrorToYXmlFragment(parseMarkdown(markdown), ydoc.getXmlFragment("prosemirror"));
    const root = window.document.body.appendChild(
      window.document.createElement("div")
    ) as unknown as HTMLElement;
    let view: EditorView | undefined;
    const construction = await constructProofEditor(root, {
      beforeBuild: (built) => {
        view = built;
      },
      heatMapMode: "hidden",
      onMarkClick: () => {},
      onMarkHover: () => {},
      user: { color: "#0369a1", name: "alice" },
      ydoc,
    });
    if (view === undefined) throw new Error("the construction never handed its view over");
    return { construction, root, view };
  }

  test("stands hidden and empty until it is bound, and holds the document after", async () => {
    await withDomWindow(async (window) => {
      const { construction, root } = await construct(window, paragraphs(BLOCKS));

      // Built and standing: the root is the editor's own, and nothing of the document is in
      // it, on the screen, or writable through it.
      expect(root.classList.contains("proof-editor")).toBe(true);
      expect(root.style.display).toBe("none");
      expect(blocksBuilt(root)).toBe(0);
      expect(root.dataset.writePath).toBeUndefined();

      const handle = await construction.bind();
      try {
        expect(blocksBuilt(root)).toBe(BLOCKS);
        expect(root.style.display).toBe("");
        expect(root.dataset.writePath).toBe("live");
      } finally {
        const { view } = handle;
        handle.destroy();
        while (!view.isDestroyed) await Promise.resolve();
      }
    });
  });

  // A read-only admission lands before the sync that binds, so the access it decides for is
  // one the editor has already been built with (`ProofDocument`'s `onAdmission`).
  test("takes a read-only decision made before it was bound", async () => {
    await withDomWindow(async (window) => {
      const { construction, root } = await construct(window, paragraphs(8));

      construction.setAccess("read");
      const handle = await construction.bind();
      try {
        expect(handle.view.editable).toBe(false);
        expect(blocksBuilt(root)).toBe(8);
      } finally {
        const { view } = handle;
        handle.destroy();
        while (!view.isDestroyed) await Promise.resolve();
      }
    });
  });

  // A construction the host never binds - a page left, a socket the server refused - is torn
  // down as it stands, and the root it was built in is the host's again, for the editor it
  // builds there next.
  test("gives the root back when it is destroyed unbound", async () => {
    await withDomWindow(async (window) => {
      const { construction, root, view } = await construct(window, paragraphs(8));

      construction.destroy();
      expect(root.style.display).toBe("");
      expect(blocksBuilt(root)).toBe(0);
      while (!view.isDestroyed) await Promise.resolve();
    });
  });

  // A host's effect that re-runs while a construction is still standing hands the same element
  // to a second construction, which finds it hidden: the first hid it, and nothing but this
  // module writes that root's `display`. An editor that remembered the value it found would
  // remember `none`, and its `reveal` would put the root back to hidden - which is how a
  // version deep link came to draw its quote into an editor the reader could not see, and no
  // host can prevent it, since tearing a construction down is itself a promise away
  // (dispatch://INFRA-341, R11).
  test("is shown when it was built into a root a construction before it had hidden", async () => {
    await withDomWindow(async (window) => {
      const first = await construct(window, paragraphs(8));
      // The second construction, in the same root, while the first still stands there hidden.
      // Its document holds no blocks - a past version's editor, which is drawn with
      // `setMarkdown` after the binding - so showing the root is `reveal`'s alone.
      const second = await constructProofEditor(first.root, {
        heatMapMode: "hidden",
        onMarkClick: () => {},
        onMarkHover: () => {},
        user: { color: "#0369a1", name: "alice" },
        ydoc: new Y.Doc(),
      });
      const handle = await second.bind();
      try {
        handle.reveal();
        expect(first.root.style.display).toBe("");
      } finally {
        const { view } = handle;
        handle.destroy();
        first.construction.destroy();
        while (!view.isDestroyed || !first.view.isDestroyed) await Promise.resolve();
      }
    });
  });
});

describe("a reader's edit in a block the batches have built", () => {
  test("is in the Yjs document before the last batch lands", async () => {
    let written = "";
    let untouched: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ fragment, view }) => {
      caretIn(view, 0, 0);
      view.dispatch(view.state.tr.insertText("Pangolin. "));
      written = blockText(fragment, 0);
      untouched = everyBlockText(fragment).slice(1);
    });

    expect(written).toBe(`Pangolin. ${paragraph(0)}`);
    // Every other block of the document is as it was: the window the edit is written through
    // reaches the built blocks and no further.
    expect(untouched).toEqual(
      Array.from({ length: BLOCKS - 1 }, (_, index) => paragraph(index + 1))
    );
    expect(built.markdown).toContain(`Pangolin. ${paragraph(0)}`);
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  test("splits a block at Enter and joins two at Backspace, in the Yjs document", async () => {
    let afterSplit: string[] = [];
    let afterJoin: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ fragment, view }) => {
      // Enter in the middle of the first block: two blocks where there was one.
      caretIn(view, 0, "Paragraph 0".length);
      view.dispatch(view.state.tr.split(view.state.selection.head));
      afterSplit = everyBlockText(fragment).slice(0, 3);
      // Backspace at the start of the second half joins it back onto the first.
      caretIn(view, 1, 0);
      view.dispatch(view.state.tr.join(startOf(view.state.doc, 1)));
      afterJoin = everyBlockText(fragment).slice(0, 2);
    });

    expect(afterSplit).toEqual(["Paragraph 0", " of the ledger.", paragraph(1)]);
    expect(afterJoin).toEqual([paragraph(0), paragraph(1)]);
    expect(built.markdown).toContain(paragraph(0));
    expect(built.markdown).toContain(paragraph(BLOCKS - 1));
  });

  test("writes a paste that spans several blocks to the Yjs document", async () => {
    let pasted: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ fragment, view }) => {
      // The reader selects from inside the first block to inside the third and pastes three
      // blocks over them, which is a delete and an insert across the built run at once.
      const from = startOf(view.state.doc, 0) + 1;
      const to = startOf(view.state.doc, 2) + 1 + "Paragraph 2".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
      withHTMLElementGlobal(() => pastePlainText(view, "Otter one\n\nOtter two\n\nOtter three"));
      pasted = everyBlockText(fragment).slice(0, 4);
    });

    expect(pasted).toEqual(["Otter one", "Otter two", "Otter three of the ledger.", paragraph(3)]);
    expect(built.markdown).toContain("Otter two");
    expect(built.markdown).toContain(paragraph(BLOCKS - 1));
  });
});

describe("another person's change while the batches run", () => {
  test("is in the editor in the task it arrives in, for the block it touched", async () => {
    let atOnce = "";
    const built = await build(paragraphs(BLOCKS), ({ root, ydoc }) => {
      const peer = peerOf(ydoc);
      const first = peer.getXmlFragment("prosemirror").get(0) as Y.XmlElement;
      (first.get(0) as Y.XmlText).insert(0, "Pangolin. ");
      sendToTheEditor(peer, ydoc);
      atOnce = root.textContent ?? "";
    });

    expect(atOnce).toContain(`Pangolin. ${paragraph(0)}`);
    expect(built.blocks).toBe(BLOCKS);
    expect(built.markdown).toContain(`Pangolin. ${paragraph(0)}`);
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  test("keeps the reader's cursor in the block they are writing in", async () => {
    let where = -1;
    let inside = "";
    const built = await build(paragraphs(BLOCKS), ({ view, ydoc }) => {
      caretIn(view, 1, "Paragraph 1".length);
      const peer = peerOf(ydoc);
      const second = peer.getXmlFragment("prosemirror").get(1) as Y.XmlElement;
      (second.get(0) as Y.XmlText).insert(0, "Pangolin. ");
      sendToTheEditor(peer, ydoc);
      where = view.state.selection.head - startOf(view.state.doc, 1) - 1;
      // The reader writes on without looking, and their word lands where their cursor was
      // rather than at the top of the block another person has just rewritten.
      view.dispatch(view.state.tr.insertText("!"));
      inside = blockText(ydoc.getXmlFragment("prosemirror"), 1);
    });

    // The caret stayed on the reader's own words, which another person's ten characters have
    // pushed along: it is where "Paragraph 1" ends, not where it ended before they wrote.
    expect(where).toBe("Pangolin. Paragraph 1".length);
    expect(inside).toBe("Pangolin. Paragraph 1! of the ledger.");
    expect(built.markdown).toContain("Pangolin. Paragraph 1! of the ledger.");
  });

  test("keeps a block they add inside the built run, at the index they added it", async () => {
    let around: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ root, ydoc }) => {
      const peer = peerOf(ydoc);
      peer
        .getXmlFragment("prosemirror")
        .insert(1, [paragraphBlock("written-while-building", "Pangolin.")]);
      sendToTheEditor(peer, ydoc);
      around = Array.from(
        root.querySelectorAll('[data-block-id="written-while-building"]'),
        (block) => block.textContent ?? ""
      );
    });

    expect(around).toEqual(["Pangolin."]);
    expect(built.blocks).toBe(BLOCKS + 1);
    expect(built.markdown).toContain("Pangolin.");
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  test("keeps a block whose words are tag-shaped, as the characters they are", async () => {
    let written = "";
    const built = await build(paragraphs(BLOCKS), ({ fragment, ydoc }) => {
      const peer = peerOf(ydoc);
      peer.getXmlFragment("prosemirror").insert(1, [paragraphBlock("tag-shaped", "<script>")]);
      sendToTheEditor(peer, ydoc);
      written = blockText(fragment, 1);
    });

    expect(written).toBe("<script>");
    expect(built.documentAfter[1]).toBe("tag-shaped:<script>");
    expect(built.blocks).toBe(BLOCKS + 1);
  });

  test("takes a block they remove from the built run out of the editor", async () => {
    let afterwards = "";
    const built = await build(paragraphs(BLOCKS), ({ root, ydoc }) => {
      const peer = peerOf(ydoc);
      peer.getXmlFragment("prosemirror").delete(1, 1);
      sendToTheEditor(peer, ydoc);
      afterwards = root.textContent ?? "";
    });

    expect(afterwards).not.toContain(paragraph(1));
    expect(built.blocks).toBe(BLOCKS - 1);
    expect(built.markdown).not.toContain(paragraph(1));
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });

  test("leaves the reader's own edit of a built block standing beside it", async () => {
    let both: string[] = [];
    const built = await build(paragraphs(BLOCKS), ({ fragment, view, ydoc }) => {
      const peer = peerOf(ydoc);
      const first = peer.getXmlFragment("prosemirror").get(0) as Y.XmlElement;
      (first.get(0) as Y.XmlText).insert(0, "Pangolin. ");
      peer
        .getXmlFragment("prosemirror")
        .insert(1, [paragraphBlock("written-while-building", "Otter.")]);
      peer.getXmlFragment("prosemirror").delete(3, 1);
      sendToTheEditor(peer, ydoc);
      caretIn(view, 2, 0);
      view.dispatch(view.state.tr.insertText("Beaver. "));
      both = everyBlockText(fragment).slice(0, 4);
    });

    expect(both).toEqual([
      `Pangolin. ${paragraph(0)}`,
      "Otter.",
      `Beaver. ${paragraph(1)}`,
      paragraph(3),
    ]);
    expect(built.markdown).toContain(`Beaver. ${paragraph(1)}`);
    expect(built.markdown).toContain(paragraph(BLOCKS - 1));
  });

  // The loop ends when the frontier reaches the fragment's length, and the adoption is a task
  // of its own after it. A block another person appends in that gap - a peer pressing Enter at
  // the end of the document - reaches `restructure` at the frontier, where the batch loop used
  // to be left to draw it and no batch ran again: the editor never held it, and the binding's
  // first write of the editor's document over the fragment deleted it from the Yjs document
  // (dispatch://INFRA-341, R11).
  test("keeps a block they append in the gap between the last batch and the adoption", async () => {
    let afterwards: string[] = [];
    const built = await build(paragraphs(BLOCKS), () => {}, {
      afterwards: ({ fragment, view }) => {
        // The reader's next keystroke, which is what carries the editor's own document into
        // the Yjs document through the binding that has just adopted it.
        caretIn(view, 0, 0);
        view.dispatch(view.state.tr.insertText("!"));
        afterwards = everyBlockText(fragment);
      },
      inTheGap: ({ ydoc }) => {
        const peer = peerOf(ydoc);
        peer
          .getXmlFragment("prosemirror")
          .insert(BLOCKS, [paragraphBlock("written-in-the-gap", "Pangolin.")]);
        sendToTheEditor(peer, ydoc);
      },
    });

    expect(afterwards.length).toBe(BLOCKS + 1);
    expect(afterwards[BLOCKS]).toBe("Pangolin.");
    expect(built.documentAfter[BLOCKS]).toBe("written-in-the-gap:Pangolin.");
    expect(built.blocks).toBe(BLOCKS + 1);
    expect(built.text).toContain("Pangolin.");
  });

  // A block the schema has no node for costs nothing but itself. `createNodeFromYElement`
  // deletes such a block from the Yjs document as it refuses it, inside a transaction of
  // y-prosemirror's own origin that never comes back to the observer here, so what the
  // conversion made - not what the batch or the insert held - is what the frontier moves by:
  // counting the refused block left the window one block wider than the editor, and the first
  // block the batches had not reached was read and then deleted from the Yjs document
  // (dispatch://INFRA-341, R11).
  test("loses no block of its own to one of theirs the schema refuses", async () => {
    let afterwards: string[] = [];
    const built = await build(
      paragraphs(BLOCKS),
      ({ ydoc }) => {
        const peer = peerOf(ydoc);
        peer.getXmlFragment("prosemirror").insert(1, [refusedBlock("refused-while-building")]);
        sendToTheEditor(peer, ydoc);
      },
      {
        afterwards: ({ fragment, view }) => {
          // The reader's next keystroke, which carries the editor's own document into the Yjs
          // document through the binding that has just adopted it.
          caretIn(view, 0, 0);
          view.dispatch(view.state.tr.insertText("!"));
          afterwards = everyBlockText(fragment);
        },
      }
    );

    expect(afterwards.length).toBe(BLOCKS);
    expect(afterwards[0]).toBe(`!${paragraph(0)}`);
    expect(afterwards.slice(1)).toEqual(
      Array.from({ length: BLOCKS - 1 }, (_, index) => paragraph(index + 1))
    );
    expect(built.blocks).toBe(BLOCKS);
    expect(built.text).toContain(paragraph(8));
    expect(built.text).not.toContain("Quokka.");
  });

  // The same refusal reached through `redraw`: their edit makes a block the batches have
  // already built one the schema cannot hold, so the whole block is refused and deleted from
  // the Yjs document. The editor used to keep the block that is no longer there, and the
  // frontier to count it, which took the window past the editor for every block after it.
  test("takes a built block out of the editor when their edit makes the schema refuse it", async () => {
    let afterwards: string[] = [];
    const built = await build(
      paragraphs(BLOCKS),
      ({ ydoc }) => {
        const peer = peerOf(ydoc);
        const first = peer.getXmlFragment("prosemirror").get(0) as Y.XmlElement;
        // A paragraph inside a paragraph: the block's content no longer fits its own node
        // type, so `createNodeFromYElement` refuses the block whole.
        first.insert(1, [paragraphBlock("nested-while-building", "Quokka.")]);
        sendToTheEditor(peer, ydoc);
      },
      {
        afterwards: ({ fragment, view }) => {
          caretIn(view, 0, 0);
          view.dispatch(view.state.tr.insertText("!"));
          afterwards = everyBlockText(fragment);
        },
      }
    );

    expect(afterwards.length).toBe(BLOCKS - 1);
    expect(afterwards[0]).toBe(`!${paragraph(1)}`);
    expect(afterwards.slice(1)).toEqual(
      Array.from({ length: BLOCKS - 2 }, (_, index) => paragraph(index + 2))
    );
    expect(built.blocks).toBe(BLOCKS - 1);
    expect(built.text).not.toContain(paragraph(0));
    expect(built.text).toContain(paragraph(BLOCKS - 1));
  });
});

// A block whose own content is blocks - a list item, a quote, a table cell, a typed block
// holding paragraphs - is where another person's edit used to split the document. The smallest
// range of the block that differs is read at the depth it differs at, and the drawn block was
// cut at those two positions with `Fragment.cut`, which keeps every ancestor of a cut that ends
// inside a child: `list_item(paragraph("X"))` put at a position inside the reader's own
// paragraph, which ProseMirror fits by splitting the paragraph and the item around it. One list
// item became three, all three under the one `blockId`, and the binding's first write after the
// adoption carried that into the Yjs document (dispatch://INFRA-341, R11).
describe("another person's edit inside a nested block", () => {
  /** The Yjs shape of the document's first block, after another person typed `X` two characters
   *  into the text at `path` inside it while the batches ran, and the reader typed a character
   *  of their own once the binding had adopted - the keystroke that carries whatever the build
   *  left in the editor into the Yjs document. */
  async function afterTypingInside(
    markdown: string,
    path: readonly number[]
  ): Promise<{ readonly built: Built; readonly shape: string }> {
    let shape = "";
    const built = await build(
      markdown,
      ({ ydoc }) => {
        const peer = peerOf(ydoc);
        textAt(peer.getXmlFragment("prosemirror").get(0) as Y.XmlElement, path).insert(2, "X");
        sendToTheEditor(peer, ydoc);
      },
      {
        afterwards: ({ fragment, view }) => {
          caretIn(view, BLOCKS - 1, 0);
          view.dispatch(view.state.tr.insertText("!"));
          shape = shapeOf(fragment.get(0) as Y.XmlElement);
        },
      }
    );
    return { built, shape };
  }

  test("leaves a list item the one list item it is", async () => {
    const { built, shape } = await afterTypingInside(
      openingWith("- item zero\n- item one"),
      [0, 0, 0]
    );

    expect(shape).toBe(
      'bullet_list(list_item(paragraph("itXem zero")), list_item(paragraph("item one")))'
    );
    expect(built.documentAfter.length).toBe(BLOCKS);
  });

  test("leaves a blockquote's paragraph the one paragraph it is", async () => {
    const { built, shape } = await afterTypingInside(openingWith("> quote zero"), [0, 0]);

    expect(shape).toBe('blockquote(paragraph("quXote zero"))');
    expect(built.documentAfter.length).toBe(BLOCKS);
  });

  test("leaves a table cell's paragraph the one paragraph it is", async () => {
    const { built, shape } = await afterTypingInside(
      openingWith("| head a | head b |\n| --- | --- |\n| cell zero | cell one |"),
      [1, 0, 0, 0]
    );

    expect(shape).toBe(
      "table(" +
        'table_header_row(table_header(paragraph("head a")), table_header(paragraph("head b"))), ' +
        'table_row(table_cell(paragraph("ceXll zero")), table_cell(paragraph("cell one")))' +
        ")"
    );
    expect(built.documentAfter.length).toBe(BLOCKS);
  });
});

// A host's node views have to be on the view before the build, not after it: `view.setProps`
// with a new `nodeViews` map re-runs ProseMirror's view update over the whole document, which
// destroys every node view the batches made and creates the host's in its place. For Dispatch's
// typed blocks over the 406,319-character document that was one task of 1,218 ms on a phone at
// 4x CPU throttling, longer than any batch of the build it followed (dispatch://INFRA-341, R11).
describe("the host's own node views", () => {
  test("are installed before the first batch, and built a batch at a time with it", async () => {
    const { parseMarkdown } = await createHeadlessProof({});
    const ydoc = new Y.Doc();
    prosemirrorToYXmlFragment(
      parseMarkdown(paragraphs(BLOCKS)),
      ydoc.getXmlFragment("prosemirror")
    );
    await withDomWindow(async (window) => {
      const root = window.document.body.appendChild(
        window.document.createElement("div")
      ) as unknown as HTMLElement;
      /** How many blocks the fragment had put in the editor when the host was called. */
      let whenCalled = -1;
      let created = 0;
      // How many of the host's node views existed at each look, one look per task, which is
      // what says they were created across the batches rather than in one task after them. A
      // real `setTimeout(0)` chain, as `watchTheBuild` is and for the same reason: what it
      // watches for is the build's own yield between batches, which a fake clock would drive
      // rather than observe.
      const seen: number[] = [];
      let watching = true;
      const look = () => {
        if (!watching) return;
        seen.push(created);
        setTimeout(look, 0);
      };
      setTimeout(look, 0);
      const handle = await createProofEditor(root, {
        beforeBuild: (view) => {
          whenCalled = blocksBuilt(root);
          view.setProps({
            nodeViews: {
              ...view.props.nodeViews,
              paragraph: () => {
                created += 1;
                const dom = window.document.createElement("p") as unknown as HTMLElement;
                return { contentDOM: dom, dom };
              },
            },
          });
        },
        heatMapMode: "hidden",
        onMarkClick: () => {},
        onMarkHover: () => {},
        user: { color: "#0369a1", name: "alice" },
        ydoc,
      });
      watching = false;
      const { view } = handle;
      try {
        // Called over the empty document Milkdown created, before a block of the fragment was
        // in it: a node view installed here is the one each batch builds with.
        expect(whenCalled).toBe(0);
        expect(view.state.doc.childCount).toBe(BLOCKS);
        expect(created).toBeGreaterThanOrEqual(BLOCKS);
        expect(
          seen.filter((count) => count > 0 && count < BLOCKS).length,
          "every one of the host's node views was created in one task: a host that installs " +
            "them after the build pays for the whole document a second time"
        ).toBeGreaterThan(0);
      } finally {
        handle.destroy();
        while (!view.isDestroyed) await Promise.resolve();
      }
    });
  });
});

/** Runs `act` with `scheduler` standing in for the browsing context's own, and puts back
 *  whatever was there. `undefined` runs it in a context that has none. */
async function withScheduler(scheduler: unknown, act: () => Promise<void>): Promise<void> {
  const had = Object.getOwnPropertyDescriptor(globalThis, "scheduler");
  if (scheduler === undefined) {
    Reflect.deleteProperty(globalThis, "scheduler");
  } else {
    Object.defineProperty(globalThis, "scheduler", {
      configurable: true,
      value: scheduler,
      writable: true,
    });
  }
  try {
    await act();
  } finally {
    Reflect.deleteProperty(globalThis, "scheduler");
    if (had !== undefined) Object.defineProperty(globalThis, "scheduler", had);
  }
}

/** A real 0 ms timer, not a fake clock: what these rows read is the order the browsing
 *  context's own queue serves two tasks in, which a fake clock would drive rather than
 *  observe. Neither waits for anything. */
function thePageSetsATimer(order: string[]): void {
  setTimeout(() => order.push("the page's own timer"), 0);
}

// How the thread goes back between batches is the whole of whether anything else on the page
// runs while a build lasts. `scheduler.yield()` stood here, and the browser serves its
// continuation ahead of every posted task: for as long as a build lasted - about 35 s over one
// screenful of the 406,319-character document and minutes over a whole drawn one - no socket
// message, no fetch callback and no timer ran. `scheduler.postTask()` at its default priority
// left the socket message served in 7 ms and a timer waiting 19,945 ms, because Chromium runs
// a web-scheduling task of that priority ahead of a timer (dispatch://INFRA-341, R11).
describe("the thread a batch hands back", () => {
  test("is handed back behind a task the page is already waiting on", async () => {
    const order: string[] = [];

    await withScheduler(undefined, async () => {
      thePageSetsATimer(order);
      await endOfTask();
      order.push("the build's next batch");
    });

    expect(order).toEqual(["the page's own timer", "the build's next batch"]);
  });

  test("is a timer even where the browsing context has a scheduler of its own", async () => {
    const order: string[] = [];
    const taken: string[] = [];
    const scheduler = {
      postTask: (work: () => void) => {
        taken.push("postTask");
        work();
        return Promise.resolve();
      },
      yield: () => {
        taken.push("yield");
        return Promise.resolve();
      },
    };

    await withScheduler(scheduler, async () => {
      thePageSetsATimer(order);
      await endOfTask();
      order.push("the build's next batch");
    });

    expect(taken).toEqual([]);
    expect(order).toEqual(["the page's own timer", "the build's next batch"]);
  });
});
