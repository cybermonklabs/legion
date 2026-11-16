import { expect, test } from "bun:test";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import {
  type CreateProofEditorOptions,
  createProofEditor,
  type EditorReferences,
  type ProofEditorHandle,
  type ReferenceCandidate,
  type ReferenceTarget,
} from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
import { serverBlockSchema } from "./block-schema-fixture";
import { withDomWindow } from "./dom-window";

/**
 * Tearing an editor down leaves nothing that can draw it (INFRA-2409).
 *
 * `Editor.destroy()` runs every plugin's cleanup before its own, and a `$ctx` plugin's cleanup
 * removes its slice, so the commonmark presets' mark attributes - `inlineCodeAttr` among them -
 * are gone while the ProseMirror view is still live and still mounted. Every mark's `toDOM` reads
 * its slice as it draws, so anything that redraws the view in that window throws
 * `Context "<slice>" not found, do you forget to inject it?` out of whichever call drew it: in
 * Dispatch that is a React effect, so the page's error boundary shows the throw. A host writes
 * its highlights and its reference titles from effects of its own, and React runs a child's
 * cleanup after the parent's, so such a write lands exactly in that window.
 *
 * `ProofEditorHandle.destroy` therefore destroys the view before the editor, and every
 * host-facing write answers a destroyed view by doing nothing.
 *
 * These tests own their teardown rather than borrowing `withMarksEditor`'s, which destroys the
 * handle itself: what the second call to `destroy` does is one of the things under test.
 */

/** A stand-in for the host's reader: a `dispatch://` address or an issue key. */
function scan(text: string): ReferenceCandidate[] {
  return Array.from(text.matchAll(/dispatch:\/\/[^\s<`]+|\b[A-Z]+-\d+\b/g), (match) => ({
    end: match.index + match[0].length,
    key: match[0],
    start: match.index,
  }));
}

const CORE_1: ReferenceTarget = { href: "dispatch://CORE-1", title: "CORE-1 Fix the thing" };

function references(targets: Record<string, ReferenceTarget | null>): EditorReferences {
  return { onKeys: () => {}, scan, targets: new Map(Object.entries(targets)) };
}

/** A spec as Dispatch stores one: the address is a link whose text is its own address, written
 *  as code, so the title the editor draws over it is drawn inside the `inlineCode` mark. */
const BODY = "Linked: [`dispatch://CORE-1`](dispatch://CORE-1) and `--window` after it.\n";

/** The editor `createProofEditor` builds over `BODY`, with the host's reference titles drawn,
 *  configured as Dispatch's margin configures it, as the package's other editor tests are
 *  (`./marks-editor.ts`). `run` is handed the handle and tears it down itself. */
async function withTornDownEditor(
  run: (editor: ProofEditorHandle) => void | Promise<void>,
  options: Partial<CreateProofEditorOptions> = {}
): Promise<void> {
  const { parseMarkdown } = await createHeadlessProof();
  const ydoc = new Y.Doc();
  prosemirrorToYXmlFragment(parseMarkdown(BODY), ydoc.getXmlFragment("prosemirror"));
  await withDomWindow(async (window) => {
    const root = window.document.body.appendChild(window.document.createElement("div"));
    const handle = await createProofEditor(root as unknown as HTMLElement, {
      heatMapMode: "hidden",
      onMarkClick: () => {},
      onMarkHover: () => {},
      user: { color: "#0369a1", name: "alice" },
      ydoc,
      ...options,
    });
    handle.setReferences(references({ "dispatch://CORE-1": CORE_1 }));
    try {
      await run(handle);
    } finally {
      // The editor's own teardown runs after the view's, and `destroy` is what answers for it.
      // It is awaited while this window is still open, because its plugins read the DOM as they
      // go: left to settle after `withDomWindow` closed the window, its throw would stand in for
      // whatever `run` reported. A `run` that never destroyed the handle is torn down here.
      await handle.destroy();
    }
  });
}

test("destroy takes the view down before the ctx that draws it", async () => {
  await withTornDownEditor((handle) => {
    const { view } = handle;
    const title = view.dom.querySelector("code > a.dispatch-reference");
    expect(title?.textContent).toBe("CORE-1 Fix the thing");

    void handle.destroy();

    // The slices every mark draws itself from are gone the moment the editor is destroyed, so a
    // view that outlived it would throw `Context "inlineCodeAttr" not found` out of its next
    // redraw - which is the crash this guards against.
    const inlineCode = view.state.schema.marks.inlineCode;
    expect(() => inlineCode.spec.toDOM?.(inlineCode.create(), true)).toThrow(
      'Context "inlineCodeAttr" not found, do you forget to inject it?'
    );
    expect(view.isDestroyed).toBe(true);
  });
});

test("a host write that lands after destroy draws nothing and throws nothing", async () => {
  await withTornDownEditor((handle) => {
    const blockId = handle.view.dom.querySelector("p")?.getAttribute("data-block-id") ?? "";
    expect(blockId).not.toBe("");

    void handle.destroy();

    expect(() => handle.setReferences(null)).not.toThrow();
    expect(() => handle.setActiveMarks(["mark-1"])).not.toThrow();
    expect(() => handle.setActiveBlocks([blockId])).not.toThrow();
    expect(() => handle.setBlockMarks([{ blockId, className: "dispatch-unread" }])).not.toThrow();
    expect(() =>
      handle.setChangeDecorations({
        text: [{ blockIndex: 0, className: "dispatch-change", range: { from: 0, to: 6 } }],
        widgets: [],
      })
    ).not.toThrow();
    // A host whose own teardown runs twice asks the editor twice.
    expect(() => handle.destroy()).not.toThrow();
  });
});

test("every other write on the handle does nothing after destroy, and says so", async () => {
  await withTornDownEditor(
    (handle) => {
      const markId = handle.view.dom.querySelector<HTMLElement>("[data-id]")?.dataset.id;
      const blockId = handle.view.dom.querySelector("p")?.getAttribute("data-block-id") ?? "";
      expect(handle.blockMenuItems().length).toBeGreaterThan(0);
      const staleMenuItem = handle.blockMenuItems()[0];

      void handle.destroy();

      expect(() => handle.setAccess("read")).not.toThrow();
      expect(() => handle.setMarkdown("Something else entirely.\n")).not.toThrow();
      expect(() => handle.applyRemoteMarks({})).not.toThrow();
      expect(() => handle.removeMark(markId ?? "no-such-mark")).not.toThrow();
      expect(handle.retypeMark(markId ?? "no-such-mark", "comment")).toEqual({
        refused: "missing",
      });
      expect(handle.remarkMark(markId ?? "no-such-mark", "comment", "Linked", null)).toEqual({
        refused: "gone",
      });
      expect(handle.commentOnSelection()).toBe(false);
      expect(handle.insertTypedBlock("ask")).toBe(false);
      expect(handle.retypeBlock(blockId, "ask")).toBe(false);
      expect(handle.setBlockAttributes(blockId, {})).toBe(false);
      expect(handle.blockMenuItems()).toEqual([]);
      // A menu item the host read before the teardown and ran after it.
      expect(() => staleMenuItem?.run()).not.toThrow();
    },
    { blockSchema: serverBlockSchema }
  );
});
