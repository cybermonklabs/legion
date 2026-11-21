import { expect, spyOn, test } from "bun:test";
import type { EditorView } from "@milkdown/kit/prose/view";
import { EDIT_REDRAW_DELAY_MS } from "../src/mermaid-code-block-view";
import { type MermaidDrawer, type MermaidDrawing, mermaidSvgElement } from "../src/mermaid-svg";
import { withMarksEditor } from "./marks-editor";

// The view's redraw timer is the happy-dom window's own `setTimeout`, which Bun's fake timers do
// not drive, so the cases that edit hold the redraw themselves (`withHeldRedraws`): a case finds
// the redraw an edit armed with `EDIT_REDRAW_DELAY_MS`, reads that an earlier one was cleared, and
// fires it when it chooses, rather than waiting real time to see a request come or not come.

const FIRST = "graph TD\n  A --> B";
const SECOND = "graph TD\n  C --> D";
const TWO_DIAGRAMS = `Intro\n\n\`\`\`mermaid\n${FIRST}\n\`\`\`\n\n\`\`\`mermaid\n${SECOND}\n\`\`\`\n`;
const ONE_DIAGRAM = `\`\`\`mermaid\n${FIRST}\n\`\`\`\n`;

interface DrawRequest {
  readonly source: string;
  cancels: number;
  /** Delivers an answer, unless the request was cancelled: a drawer never calls `draw` after its
   *  cancel, which is what the view relies on to drop an answer to an earlier source. */
  readonly answer: (drawing: MermaidDrawing) => void;
}

/** A drawer that records each request and answers only when the case calls `answer`, or at once
 *  with `answerAtOnce`'s drawing when it returns one; like the host's, it never draws a request
 *  after its cancel. */
function fakeDrawer(answerAtOnce?: (source: string) => MermaidDrawing | undefined): {
  drawMermaid: MermaidDrawer;
  requests: DrawRequest[];
} {
  const requests: DrawRequest[] = [];
  const drawMermaid: MermaidDrawer = (source, draw) => {
    const request: DrawRequest = {
      answer: (drawing) => {
        if (request.cancels === 0) draw(drawing);
      },
      cancels: 0,
      source,
    };
    requests.push(request);
    const drawing = answerAtOnce?.(source);
    if (drawing !== undefined) draw(drawing);
    return () => {
      request.cancels++;
    };
  };
  return { drawMermaid, requests };
}

/** A sanitized diagram of `document` whose one `<rect>` carries `id`; no renderer runs. */
function diagram(document: Document, id: string): MermaidDrawing {
  return {
    svg: mermaidSvgElement(
      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect id="${id}" width="4" height="4"/></svg>`,
      document
    ),
  };
}

interface BlockParts {
  readonly frame: HTMLElement;
  readonly pre: HTMLElement;
  readonly code: HTMLElement;
  readonly preview: HTMLElement | null;
  readonly status: HTMLElement | null;
}

function blocks(view: EditorView): BlockParts[] {
  return Array.from(view.dom.querySelectorAll<HTMLElement>("div.proof-code-block"), (frame) => {
    const pre = frame.querySelector<HTMLElement>("pre.proof-code-block-pre");
    const code = pre?.querySelector<HTMLElement>(":scope > code");
    if (!pre || !code) throw new Error("a code block has no pre > code");
    return {
      code,
      frame,
      pre,
      preview: frame.querySelector<HTMLElement>(":scope > div.proof-mermaid-preview"),
      status: frame.querySelector<HTMLElement>(":scope > div.proof-mermaid-status"),
    };
  });
}

/** The id of the `<rect>` the preview's shadow root draws, or null when it draws nothing. */
function shownDiagram(block: BlockParts): string | null {
  return block.preview?.shadowRoot?.querySelector("svg rect")?.id ?? null;
}

/** Appends `text` to the `index`th code block's source, as typing at its end does. */
function typeInto(view: EditorView, index: number, text: string): void {
  const ends: number[] = [];
  view.state.doc.descendants((node, pos) => {
    if (node.type.name === "code_block") ends.push(pos + 1 + node.content.size);
  });
  const end = ends[index];
  if (end === undefined) throw new Error(`the document has no code block ${index}`);
  view.dispatch(view.state.tr.insertText(text, end));
}

/** Switches the language of the document's last `mermaid` code block. The position is read once,
 *  at this call: a block that has left mermaid is not found again, and switching it back needs
 *  the position it had. */
function languageSetter(view: EditorView): (language: string) => void {
  let pos = -1;
  view.state.doc.descendants((node, at) => {
    if (node.attrs.language === "mermaid") pos = at;
  });
  return (language) => view.dispatch(view.state.tr.setNodeAttribute(pos, "language", language));
}

/** A redraw the view armed on the held `window.setTimeout`. */
interface HeldRedraw {
  /** Runs the redraw, as the window would once its delay had passed. */
  readonly fire: () => void;
  /** Whether the view cleared it. */
  cleared: boolean;
  /** Whether the case fired it. */
  fired: boolean;
}

/**
 * Runs `run` with the view's redraws held: a timer armed with `EDIT_REDRAW_DELAY_MS` on the
 * window is recorded and fires only when the case fires it, and the view's clearing of one marks
 * it cleared, so a case reads from the timers themselves that nothing more can come. Every other
 * timer, such as ProseMirror's own 20 ms flush of the DOM changes it observed, is the window's
 * own and runs as it would. A redraw armed with any other delay is not held, so the case finds no
 * redraw to fire and says so.
 */
async function withHeldRedraws(
  run: (redraws: HeldRedraw[]) => void | Promise<void>
): Promise<void> {
  const redraws: HeldRedraw[] = [];
  const windowSetTimeout = window.setTimeout.bind(window);
  const windowClearTimeout = window.clearTimeout.bind(window);
  const setTimeout = spyOn(window, "setTimeout").mockImplementation(((
    callback: () => void,
    delay?: number,
    ...rest: unknown[]
  ) => {
    if (delay !== EDIT_REDRAW_DELAY_MS) return windowSetTimeout(callback, delay, ...rest);
    const redraw: HeldRedraw = {
      cleared: false,
      fire: () => {
        redraw.fired = true;
        callback();
      },
      fired: false,
    };
    redraws.push(redraw);
    // A held redraw's id is minus its place in `redraws`, from -1, which no id of the window's
    // own is.
    return -redraws.length;
  }) as typeof window.setTimeout);
  const clearTimeout = spyOn(window, "clearTimeout").mockImplementation(((id?: number) => {
    if (typeof id !== "number" || id >= 0) return windowClearTimeout(id);
    const redraw = redraws[-id - 1];
    if (redraw !== undefined) redraw.cleared = true;
  }) as typeof window.clearTimeout);
  try {
    await run(redraws);
  } finally {
    setTimeout.mockRestore();
    clearTimeout.mockRestore();
  }
}

/** The redraws armed and neither cleared nor fired. */
function pending(redraws: readonly HeldRedraw[]): HeldRedraw[] {
  return redraws.filter((redraw) => !redraw.cleared && !redraw.fired);
}

/** The one pending redraw, which an edit armed with `EDIT_REDRAW_DELAY_MS`. */
function pendingRedraw(redraws: readonly HeldRedraw[]): HeldRedraw {
  const [redraw, ...others] = pending(redraws);
  if (redraw === undefined || others.length > 0) {
    throw new Error(
      `the view holds ${pending(redraws).length} pending redraws armed with ` +
        `EDIT_REDRAW_DELAY_MS (${EDIT_REDRAW_DELAY_MS} ms), not one`
    );
  }
  return redraw;
}

function expectSourceShown(block: BlockParts): void {
  const details = block.pre.parentElement;
  expect(details?.matches("details.proof-mermaid-source")).toBe(true);
  expect((details as HTMLDetailsElement).open).toBe(true);
  expect(details?.firstElementChild?.matches("summary.proof-mermaid-source-summary")).toBe(true);
  expect(block.frame.classList.contains("proof-mermaid-block")).toBe(true);
  expect(block.preview?.hidden).toBe(true);
  expect(block.status?.hidden).toBe(true);
}

test("at open each mermaid block asks once and shows its source; a late answer draws without moving the pre", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    TWO_DIAGRAMS,
    async ({ view }) => {
      expect(requests.map((request) => request.source)).toEqual([FIRST, SECOND]);
      const [first, second] = blocks(view);
      if (!first || !second) throw new Error("the document draws two code blocks");
      expectSourceShown(first);
      expectSourceShown(second);
      expect(first.code.textContent).toBe(FIRST);

      const details = first.pre.parentElement;
      const moved: Node[] = [];
      const Observer = view.dom.ownerDocument.defaultView?.MutationObserver;
      if (!Observer) throw new Error("the editor's document has a window");
      const observer = new Observer((records) => {
        for (const record of records) {
          for (const node of [...record.addedNodes, ...record.removedNodes]) {
            if (node === first.pre || node === first.code || node === details) moved.push(node);
          }
        }
      });
      observer.observe(first.frame, { childList: true, subtree: true });
      requests[0]?.answer(diagram(view.dom.ownerDocument, "first"));
      await Promise.resolve();
      observer.disconnect();

      const [after] = blocks(view);
      expect(shownDiagram(first)).toBe("first");
      expect(first.preview?.hidden).toBe(false);
      expect(first.status?.hidden).toBe(true);
      expect(after?.pre).toBe(first.pre);
      expect(after?.code).toBe(first.code);
      expect(first.pre.parentElement).toBe(details);
      expect(moved).toEqual([]);
      expect(requests).toHaveLength(2);
    },
    { drawMermaid }
  );

  const atOnce = fakeDrawer(() => diagram(document, "cached"));
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      expect(atOnce.requests).toHaveLength(1);
      expect(shownDiagram(block)).toBe("cached");
      expect(block.preview?.hidden).toBe(false);
      expect(block.pre.parentElement?.matches("details.proof-mermaid-source[open]")).toBe(true);
    },
    { drawMermaid: atOnce.drawMermaid }
  );
});

test("edits ask nothing until the pause after the last one, then ask once for the current source", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) =>
      withHeldRedraws((redraws) => {
        const document = view.dom.ownerDocument;
        requests[0]?.answer(diagram(document, "before"));
        const [block] = blocks(view);
        if (!block) throw new Error("the document draws a code block");

        typeInto(view, 0, "\n  B --> C");
        const afterFirstEdit = pendingRedraw(redraws);
        typeInto(view, 0, "\n  C --> D");
        // The last edit puts the redraw off again: the first edit's redraw is cleared and the one
        // left counts the whole delay from the last edit, and neither has asked. A redraw counted
        // from the first edit would leave the first one armed, one with no delay would have asked
        // at once, and one with another delay is not held, so `pendingRedraw` finds none.
        expect(afterFirstEdit.cleared).toBe(true);
        const afterLastEdit = pendingRedraw(redraws);
        expect(requests).toHaveLength(1);

        afterLastEdit.fire();
        expect(requests).toHaveLength(2);
        expect(requests[1]?.source).toBe(`${FIRST}\n  B --> C\n  C --> D`);
        expect(requests[0]?.cancels).toBe(1);
        expect(shownDiagram(block)).toBe("before");

        // No redraw is left to ask again.
        expect(pending(redraws)).toEqual([]);
        requests[1]?.answer(diagram(document, "after"));
        expect(shownDiagram(block)).toBe("after");
        expect(requests[1]?.cancels).toBe(0);
      }),
    { drawMermaid }
  );
});

test("destroying the editor cancels the request and the redraw timer", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ handle, view }) =>
      withHeldRedraws(async (redraws) => {
        const [block] = blocks(view);
        if (!block) throw new Error("the document draws a code block");
        typeInto(view, 0, "\n  B --> C");
        const redraw = pendingRedraw(redraws);

        // `destroy` is idempotent and answers with the editor's own teardown, so the harness may
        // call it again when this case ends.
        await handle.destroy();
        expect(view.isDestroyed).toBe(true);
        expect(requests).toHaveLength(1);
        expect(requests[0]?.cancels).toBe(1);

        // The redraw is cleared and none is left, so nothing can ask or draw later.
        expect(redraw.cleared).toBe(true);
        expect(pending(redraws)).toEqual([]);
        expect(block.preview?.shadowRoot ?? null).toBeNull();
        expect(block.preview?.hidden).toBe(true);
      }),
    { drawMermaid }
  );
});

test("with no drawer a mermaid block asks nothing and shows its source", async () => {
  await withMarksEditor(ONE_DIAGRAM, ({ view }) =>
    withHeldRedraws((redraws) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      expectSourceShown(block);
      typeInto(view, 0, "\n  B --> C");
      // The pause after the edit ends, and with no drawer there is nothing to ask.
      pendingRedraw(redraws).fire();
      const [after] = blocks(view);
      expect(after?.pre).toBe(block.pre);
      expectSourceShown(block);
      expect(block.preview?.shadowRoot ?? null).toBeNull();
      expect(block.code.textContent).toBe(`${FIRST}\n  B --> C`);
    })
  );
});

test("a code block in another language keeps upstream's plain layout and asks nothing", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    `\`\`\`js\nconst x = 1;\n\`\`\`\n\n${ONE_DIAGRAM}`,
    ({ view }) => {
      const [plain, mermaid] = blocks(view);
      if (!plain || !mermaid) throw new Error("the document draws two code blocks");
      expect(requests.map((request) => request.source)).toEqual([FIRST]);
      expect(Array.from(plain.frame.children)).toEqual([plain.pre]);
      expect(plain.pre.dataset.language).toBe("js");
      expect(plain.pre.firstElementChild).toBe(plain.code);
      expect(plain.frame.classList.contains("proof-mermaid-block")).toBe(false);

      // Switching the mermaid block to another language cancels its request and puts the pre
      // back alone in the frame; switching back asks again.
      const setLanguage = languageSetter(view);
      setLanguage("ts");
      expect(requests[0]?.cancels).toBe(1);
      expect(Array.from(mermaid.frame.children)).toEqual([mermaid.pre]);
      expect(mermaid.frame.classList.contains("proof-mermaid-block")).toBe(false);
      setLanguage("mermaid");
      expect(requests.map((request) => request.source)).toEqual([FIRST, FIRST]);
      expectSourceShown(mermaid);
    },
    { drawMermaid }
  );
});

// The case above switches a block that never drew, so what `showPlainLayout` clears is never
// observed there. It shows on the way back: `showMermaidLayout` puts the preview and the status
// into the frame again and the block asks for the current source, and until that answer arrives
// whatever the block held before its trip through another language is on the reader's page.
// These two cases take that window, one for each half of what the clearing empties.
test("a mermaid block that drew, changed language and changed back shows no drawing until its new answer", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      const setLanguage = languageSetter(view);
      requests[0]?.answer(diagram(view.dom.ownerDocument, "drawn"));
      expect(shownDiagram(block)).toBe("drawn");

      setLanguage("js");
      setLanguage("mermaid");

      // The block asked again and nothing has answered, so only its source shows: the preview and
      // the status are back in the frame, empty and hidden.
      expect(requests.map((request) => request.source)).toEqual([FIRST, FIRST]);
      expect(shownDiagram(block)).toBeNull();
      expectSourceShown(block);
    },
    { drawMermaid }
  );
});

test("a mermaid block the renderer refused, changed language and changed back shows no refusal until its new answer", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      const setLanguage = languageSetter(view);
      const error = "Unable to render Mermaid diagram: Dangling Mermaid edge";
      requests[0]?.answer({ error });
      expect(block.status?.textContent).toBe(error);
      expect(block.status?.hidden).toBe(false);

      setLanguage("js");
      setLanguage("mermaid");

      expect(requests.map((request) => request.source)).toEqual([FIRST, FIRST]);
      expect(block.status?.textContent).toBe("");
      expectSourceShown(block);
    },
    { drawMermaid }
  );
});

test("a refusal shows its text in the status line and hides the preview", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) =>
      withHeldRedraws((redraws) => {
        const [block] = blocks(view);
        if (!block) throw new Error("the document draws a code block");
        requests[0]?.answer(diagram(view.dom.ownerDocument, "drawn"));
        expect(shownDiagram(block)).toBe("drawn");

        typeInto(view, 0, " -->");
        pendingRedraw(redraws).fire();
        expect(requests).toHaveLength(2);
        const error = "Unable to render Mermaid diagram: Dangling Mermaid edge";
        requests[1]?.answer({ error });

        expect(block.status?.hidden).toBe(false);
        expect(block.status?.textContent).toBe(error);
        expect(block.preview?.hidden).toBe(true);
        expect(block.preview?.shadowRoot?.childNodes).toHaveLength(0);
        expect(block.pre.parentElement?.matches("details.proof-mermaid-source[open]")).toBe(true);
      }),
    { drawMermaid }
  );
});

test("an edit answered unavailable (no renderer can draw it) clears the earlier drawing and shows the source", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) =>
      withHeldRedraws((redraws) => {
        const [block] = blocks(view);
        if (!block) throw new Error("the document draws a code block");
        requests[0]?.answer(diagram(view.dom.ownerDocument, "drawn"));
        expect(shownDiagram(block)).toBe("drawn");

        typeInto(view, 0, "\n  B --> C");
        pendingRedraw(redraws).fire();
        expect(requests).toHaveLength(2);
        requests[1]?.answer({ unavailable: true });

        expect(block.preview?.hidden).toBe(true);
        expect(block.preview?.shadowRoot?.childNodes).toHaveLength(0);
        expect(block.status?.hidden).toBe(true);
        expect(block.status?.textContent).toBe("");
        expectSourceShown(block);
        expect(block.code.textContent).toBe(`${FIRST}\n  B --> C`);
      }),
    { drawMermaid }
  );
});
