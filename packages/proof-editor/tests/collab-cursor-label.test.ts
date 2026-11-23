import { expect, test } from "bun:test";
import { EditorState } from "@milkdown/kit/prose/state";
import { type DecorationSet, EditorView } from "@milkdown/kit/prose/view";
import type { Window } from "happy-dom";
import {
  absolutePositionToRelativePosition,
  prosemirrorToYXmlFragment,
  yCursorPluginKey,
  ySyncPlugin,
  ySyncPluginKey,
} from "y-prosemirror";
import { Awareness, applyAwarenessUpdate, encodeAwarenessUpdate } from "y-protocols/awareness";
import * as Y from "yjs";
import { collabCursorPlugin } from "../src/collab-cursor-plugin";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withDomWindow } from "./dom-window";

const INWARD = "proof-collab-cursor__label--inward";

/** The width of the box the editor scrolls in, until a step changes it, and of every caret's
 *  label, in px. */
const BOX_WIDTH = 350;
const LABEL_WIDTH = 60;

function rect(left: number, width: number): DOMRect {
  return {
    bottom: 20,
    height: 20,
    left,
    right: left + width,
    top: 0,
    width,
    x: left,
    y: 0,
  } as DOMRect;
}

/**
 * Runs `run` with every caret the editor draws laid out as a browser would, which happy-dom does
 * not: the caret's 2 px bar stands where `barAt` says for its peer's name, and its label,
 * `LABEL_WIDTH` wide, hangs from the bar's middle whichever way its class says. Every other
 * element keeps happy-dom's own answer.
 */
async function withCaretLayout(
  window: Window,
  barAt: Record<string, number>,
  run: () => Promise<void>
): Promise<void> {
  const prototype = window.HTMLElement.prototype;
  const own = Object.getOwnPropertyDescriptor(prototype, "getBoundingClientRect");
  const measure = prototype.getBoundingClientRect;
  const caretBar = (caret: Element) => {
    const name = caret.querySelector(".proof-collab-cursor__label")?.textContent ?? "";
    const left = barAt[name];
    if (left === undefined) throw new Error(`no layout for the caret of ${name}`);
    return left;
  };
  Object.defineProperty(prototype, "getBoundingClientRect", {
    configurable: true,
    value(this: Element) {
      if (this.classList.contains("proof-collab-cursor")) return rect(caretBar(this), 2);
      if (this.classList.contains("proof-collab-cursor__label") && this.parentElement !== null) {
        const middle = caretBar(this.parentElement) + 1;
        const inward = this.classList.contains(INWARD);
        return rect(inward ? middle - LABEL_WIDTH : middle, LABEL_WIDTH);
      }
      return measure.call(this);
    },
  });
  try {
    await run();
  } finally {
    if (own === undefined) Reflect.deleteProperty(prototype, "getBoundingClientRect");
    else Object.defineProperty(prototype, "getBoundingClientRect", own);
  }
}

function inward(view: EditorView, name: string): boolean {
  const label = Array.from(
    view.dom.querySelectorAll<HTMLElement>(".proof-collab-cursor__label")
  ).find((element) => element.textContent === name);
  if (label === undefined) throw new Error(`the editor draws no caret of ${name}`);
  return label.classList.contains(INWARD);
}

// Upstream's label hangs from a collaborator's caret toward the line's end. With the caret near a
// line's end it passed the end of the box the editor scrolls in, and the box scrolled sideways
// while the caret stood there. Here bob's caret stands 20 px from the end of a 350 px box and
// carol's 100 px from its start, where the label fits either way; then the two swap, and each
// label is decided again. In a box of 80 px, a label with room on neither side keeps upstream's
// way, and one with room behind its caret hangs back. And a label is decided again when its box
// resizes, with no caret moved: bob's, hanging back 20 px from a 350 px box's end, hangs toward
// the line's end once the box is 700 px wide.
test("a caret label that would pass the end of its box hangs back from the caret where it fits, and is decided again when its box resizes", async () => {
  const { parseMarkdown, schema } = await createHeadlessProof();
  const ydoc = new Y.Doc();
  const fragment = ydoc.getXmlFragment("prosemirror");
  prosemirrorToYXmlFragment(
    parseMarkdown("A plain statement whose line runs to the end of its box.\n"),
    fragment
  );
  const awareness = new Awareness(ydoc);
  const bob = new Awareness(new Y.Doc());
  const carol = new Awareness(new Y.Doc());
  bob.setLocalStateField("user", { color: "#b45309", name: "bob" });
  carol.setLocalStateField("user", { color: "#0369a1", name: "carol" });
  const barAt: Record<string, number> = { bob: BOX_WIDTH - 20, carol: 100 };
  let boxWidth = BOX_WIDTH;
  // Each `ResizeObserver` the editor makes, kept as a call of its callback with itself, which
  // the resize step makes as the browser would once a box it watches resized: happy-dom lays
  // nothing out, so no box of it ever resizes.
  const resizes: (() => void)[] = [];
  const KeptResizeObserver = class {
    constructor(callback: ResizeObserverCallback) {
      resizes.push(() => callback([], this));
    }
    disconnect(): void {}
    observe(): void {}
    unobserve(): void {}
  };
  try {
    await withDomWindow(async (window) => {
      // happy-dom gives no element overflow's initial value, and a box is found by its overflow.
      const sheet = window.document.createElement("style");
      sheet.textContent = "* { overflow-x: visible; }";
      window.document.head.appendChild(sheet);
      const box = window.document.createElement("div");
      box.style.overflowX = "auto";
      window.document.body.appendChild(box);
      Object.defineProperties(box, {
        clientLeft: { value: 0 },
        clientWidth: { get: () => boxWidth },
        getBoundingClientRect: { value: () => rect(0, boxWidth) },
        scrollLeft: { value: 0 },
      });
      // Made before the view, which makes its observer as its plugins' views start.
      Object.defineProperty(window, "ResizeObserver", {
        configurable: true,
        value: KeptResizeObserver,
      });
      await withCaretLayout(window, barAt, async () => {
        const view = new EditorView(box as unknown as HTMLElement, {
          state: EditorState.create({
            plugins: [ySyncPlugin(fragment), collabCursorPlugin(awareness)],
            schema,
          }),
        });
        // y-prosemirror draws the shared document, and each awareness change, in a task it
        // queues (`setTimeout(0)`). Timeouts of one delay run in the order they were set, so a
        // 0 ms timeout set now runs after every task queued so far. No fake clock can stand in:
        // the task is y-prosemirror's own.
        const queuedSoFar = () => {
          const { promise, resolve } = Promise.withResolvers<void>();
          setTimeout(resolve, 0);
          return promise;
        };
        // Each pass yields to every task queued so far, rather than waiting out a duration. Each
        // pass was a `Bun.sleep(0)`, and about once in a hundred runs a hundred of those ended
        // before y-prosemirror's task had run.
        const yieldUntil = async (drawn: () => boolean, what: string) => {
          for (let task = 0; !drawn(); task++) {
            if (task > 100) throw new Error(`y-prosemirror never drew ${what}`);
            await queuedSoFar();
          }
        };
        // The next animation frame, which the labels are decided in.
        const nextFrame = async () => {
          const { promise, resolve } = Promise.withResolvers<void>();
          window.requestAnimationFrame(() => resolve());
          await promise;
        };
        // Puts each peer's caret at its position and waits until the editor draws it there,
        // then for the animation frame after that draw.
        const putCarets = async (carets: [Awareness, number][]) => {
          const { mapping } = ySyncPluginKey.getState(view.state).binding;
          for (const [peer, at] of carets) {
            const position = absolutePositionToRelativePosition(at, fragment, mapping);
            peer.setLocalStateField("cursor", { anchor: position, head: position });
            applyAwarenessUpdate(awareness, encodeAwarenessUpdate(peer, [peer.clientID]), "peer");
          }
          await yieldUntil(() => {
            const drawn: DecorationSet | undefined = yCursorPluginKey.getState(view.state);
            return carets.every(
              ([peer, at]) =>
                drawn?.find(at, at, (spec) => spec.key === String(peer.clientID)).length === 1
            );
          }, "the carets where they were put");
          await nextFrame();
        };
        try {
          await yieldUntil(
            () => ySyncPluginKey.getState(view.state).binding.mapping.size > 0,
            "the shared document"
          );
          const textEnd = view.state.doc.content.size - 1;

          await putCarets([
            [bob, textEnd],
            [carol, 10],
          ]);
          expect(
            inward(view, "bob"),
            "bob's label, 20 px from the box's end, still hangs toward the line's end"
          ).toBe(true);
          expect(
            inward(view, "carol"),
            "carol's label, with room ahead of it, hangs back from her caret"
          ).toBe(false);

          barAt.bob = 100;
          barAt.carol = BOX_WIDTH - 20;
          await putCarets([
            [bob, 10],
            [carol, textEnd],
          ]);
          expect(
            inward(view, "bob"),
            "bob's label still hangs back after his caret moved away from the box's end"
          ).toBe(false);
          expect(
            inward(view, "carol"),
            "carol's label, moved 20 px from the box's end, still hangs toward the line's end"
          ).toBe(true);

          // Room on neither side: carol's caret 29 px from the start of an 80 px box, where her
          // label would pass its end hanging toward the line's end and its start hanging back,
          // so it keeps upstream's way, though it hung back before; bob's 59 px from its start,
          // where hanging back fits.
          boxWidth = 80;
          barAt.bob = 59;
          barAt.carol = 29;
          await putCarets([
            [bob, 20],
            [carol, 30],
          ]);
          expect(
            inward(view, "carol"),
            "carol's label, with room on neither side of her caret, hangs back past the box's start"
          ).toBe(false);
          expect(
            inward(view, "bob"),
            "bob's label, with room behind his caret in the 80 px box, still hangs toward its end"
          ).toBe(true);

          // The box resizes with no caret moved: bob's label, which hung back 20 px from the
          // end of a 350 px box, has room ahead of it once the box is 700 px wide.
          boxWidth = BOX_WIDTH;
          barAt.bob = BOX_WIDTH - 20;
          await putCarets([[bob, textEnd]]);
          expect(
            inward(view, "bob"),
            "bob's label, 20 px from the 350 px box's end, still hangs toward the line's end"
          ).toBe(true);
          boxWidth = 2 * BOX_WIDTH;
          for (const resize of resizes) resize();
          await nextFrame();
          expect(
            inward(view, "bob"),
            "bob's label still hangs back after the box widened to 700 px around his caret"
          ).toBe(false);
        } finally {
          // y-prosemirror hands each awareness change it hears to the view in a 0 ms timeout it
          // queues (`setMeta`), and runs it on a view even once that view is destroyed. In CI one
          // such task ran after the view was gone (`this.docView` null in prosemirror-view's
          // `updateStateInner`), and one after this window was gone (`document is not defined`).
          // So the case yields to every task queued so far first while the view still lives,
          // then once more after the view is gone, while this window is still the document.
          await queuedSoFar();
          view.destroy();
          await queuedSoFar();
        }
      });
    });
  } finally {
    awareness.destroy();
    bob.destroy();
    carol.destroy();
  }
});
