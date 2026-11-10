import { expect, test } from "bun:test";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import {
  createProofEditor,
  type EditorReferences,
  type ProofEditorHandle,
  type ReferenceCandidate,
  type ReferenceTarget,
} from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
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
  run: (editor: ProofEditorHandle) => void | Promise<void>
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
    });
    handle.setReferences(references({ "dispatch://CORE-1": CORE_1 }));
    try {
      await run(handle);
    } finally {
      // Whatever `run` left, the view's teardown is drained while this window is still open: its
      // plugins read the DOM as they go, and before this fix it ran in a microtask of its own,
      // after the window was closed, where its throw would stand in for whatever `run` reported.
      for (let drain = 0; drain < 100 && !handle.view.isDestroyed; drain += 1) {
        await Promise.resolve();
      }
    }
  });
}

test("destroy takes the view down before the ctx that draws it", async () => {
  await withTornDownEditor((handle) => {
    const { view } = handle;
    const title = view.dom.querySelector("code > a.dispatch-reference");
    expect(title?.textContent).toBe("CORE-1 Fix the thing");

    handle.destroy();

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

    handle.destroy();

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
