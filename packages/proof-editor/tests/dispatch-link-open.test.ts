import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type {
  CreateProofEditorOptions,
  OpenLink,
  ProofEditorAccess,
  ReferenceCandidate,
} from "../src/lib";
import { serverBlockSchema } from "./block-schema-fixture";
import { withMarksEditor } from "./marks-editor";

/** A paragraph holding a link to another site and a `dispatch://` link. It opens at 0, so its
 *  text starts at 1: "the plan" is 6 to 14 and "the other issue" 19 to 34. */
const LINKS =
  "Read [the plan](https://example.com/a) and [the other issue](dispatch://CORE-2) today.";

/** The host's link opener, recording each target it is handed and whether for a new tab. */
function recordedOpener(): {
  readonly openLink: OpenLink;
  readonly opened: Array<[string, boolean]>;
} {
  const opened: Array<[string, boolean]> = [];
  return { openLink: (target, { newTab }) => void opened.push([target, newTab]), opened };
}

/** The link of the document whose words are `text`. */
function linkOf(view: EditorView, text: string): Element {
  const link = Array.from(view.dom.querySelectorAll("a")).find(
    (anchor) => anchor.textContent === text
  );
  if (link === undefined) throw new Error(`no link reads "${text}"`);
  return link;
}

/** Dispatches a mouse event of `type` on `target`, as the browser does: ProseMirror's own
 *  listener hands it to the editor's handlers. Only for a press the editor prevents, or one that
 *  is no click: happy-dom follows an unprevented click on a link by navigating its window. */
function dispatch(target: Element, type: string, init: MouseEventInit = {}): MouseEvent {
  const event = new window.MouseEvent(type, {
    bubbles: true,
    button: 0,
    cancelable: true,
    ...init,
  });
  target.dispatchEvent(event);
  return event;
}

/** Offers a mouse event of `type`, aimed at `target`, to the editor's DOM event handlers as
 *  ProseMirror's own listener does, with no default action to follow, and answers whether one
 *  handled it: for a click the editor leaves to the browser. */
function offer(
  view: EditorView,
  target: Element,
  type: "auxclick" | "click" | "mousedown" | "mouseup",
  init: MouseEventInit = {}
): { readonly event: MouseEvent; readonly handled: boolean } {
  const event = new window.MouseEvent(type, {
    bubbles: true,
    button: 0,
    cancelable: true,
    ...init,
  });
  Object.defineProperty(event, "target", { value: target });
  const handled = view.someProp("handleDOMEvents", (handlers) =>
    (handlers[type] as ((view: EditorView, event: Event) => boolean | undefined) | undefined)?.(
      view,
      event
    )
  );
  return { event, handled: handled === true };
}

function caretAt(view: EditorView, pos: number): void {
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, pos)));
}

/** Offers Alt+Enter (Option+Enter) to the editor's keydown handlers, as ProseMirror's own
 *  listener does before its keymaps, and answers whether one took it. Upstream's task-checkbox
 *  handler reads the page's `KeyboardEvent`, which a browser defines and the test window does
 *  not install, so it is installed for the call. */
function altEnter(view: EditorView): { readonly event: KeyboardEvent; readonly handled: boolean } {
  const event = new window.KeyboardEvent("keydown", {
    altKey: true,
    bubbles: true,
    cancelable: true,
    key: "Enter",
  });
  const previous = Object.getOwnPropertyDescriptor(globalThis, "KeyboardEvent");
  Object.defineProperty(globalThis, "KeyboardEvent", {
    configurable: true,
    value: window.KeyboardEvent,
    writable: true,
  });
  try {
    const handled = view.someProp("handleDOMEvents", (handlers) => handlers.keydown?.(view, event));
    return { event, handled: handled === true };
  } finally {
    if (previous === undefined) Reflect.deleteProperty(globalThis, "KeyboardEvent");
    else Object.defineProperty(globalThis, "KeyboardEvent", previous);
  }
}

test("a plain click opens a link in every access mode, and Ctrl, Cmd and a middle click open it in a new tab", async () => {
  for (const access of ["edit", "comment", "read"] as const satisfies ProofEditorAccess[]) {
    const { openLink, opened } = recordedOpener();
    await withMarksEditor(
      LINKS,
      ({ view }) => {
        const away = linkOf(view, "the plan");
        const issue = linkOf(view, "the other issue");
        const page = window.location.href;

        expect({ access, prevented: dispatch(away, "click").defaultPrevented }).toEqual({
          access,
          prevented: true,
        });
        dispatch(issue, "click");
        dispatch(issue, "click", { ctrlKey: true });
        dispatch(away, "click", { metaKey: true });
        expect(dispatch(issue, "auxclick", { button: 1 }).defaultPrevented).toBe(true);
        // A Shift or Alt click, and a right button's, are the browser's own.
        expect(offer(view, away, "click", { shiftKey: true }).handled).toBe(false);
        expect(offer(view, away, "click", { altKey: true }).handled).toBe(false);
        expect(offer(view, away, "auxclick", { button: 2 }).handled).toBe(false);

        expect({ access, opened }).toEqual({
          access,
          opened: [
            ["https://example.com/a", false],
            ["dispatch://CORE-2", false],
            ["dispatch://CORE-2", true],
            ["https://example.com/a", true],
            ["dispatch://CORE-2", true],
          ],
        });
        // The host opened each one; the browser followed none.
        expect(window.location.href).toBe(page);
      },
      { access, openLink }
    );
  }
});

test("a middle button's press and release on a link are prevented, so the browser pastes nothing", async () => {
  const { openLink } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ handle, view }) => {
      const stored = handle.getMarkdown();
      for (const text of ["the plan", "the other issue"]) {
        const link = linkOf(view, text);
        expect({
          text,
          press: dispatch(link, "mousedown", { button: 1 }).defaultPrevented,
        }).toEqual({ press: true, text });
        expect({
          text,
          release: dispatch(link, "mouseup", { button: 1 }).defaultPrevented,
        }).toEqual({ release: true, text });
      }
      // Away from a link a middle press is the editor's as before, and a primary one is always.
      const paragraph = view.dom.querySelector("p");
      if (paragraph === null) throw new Error("no paragraph");
      expect(offer(view, paragraph, "mouseup", { button: 1 }).handled).toBe(false);
      expect(offer(view, linkOf(view, "the plan"), "mousedown").handled).toBe(false);
      expect(handle.getMarkdown()).toBe(stored);
    },
    { openLink }
  );
});

test("a click on a link inside commented text opens the link and names no comment", async () => {
  const { openLink, opened } = recordedOpener();
  const clickedMarks: string[] = [];
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const { schema } = view.state;
      const comment = schema.marks.proofComment?.create({ by: "user:alice", id: "comment-1" });
      if (comment === undefined) throw new Error("no comment mark");
      view.dispatch(view.state.tr.addMark(1, view.state.doc.content.size - 1, comment));
      const away = linkOf(view, "the plan");
      const besideLink = Array.from(view.dom.querySelectorAll('span[data-proof="comment"]')).find(
        (span) => span.closest("a") === null && span.querySelector("a") === null
      );
      if (besideLink === undefined) throw new Error("no commented text outside a link");
      // The release ProseMirror reads as a click, which the margin hears through `handleClick`.
      const release = (target: Element) => {
        const event = new window.MouseEvent("mouseup", { bubbles: true, button: 0 });
        Object.defineProperty(event, "target", { value: target });
        view.someProp("handleClick", (handleClick) => handleClick(view, 8, event));
      };

      release(away);
      expect(clickedMarks).toEqual([]);
      expect(dispatch(away, "click").defaultPrevented).toBe(true);
      expect(opened).toEqual([["https://example.com/a", false]]);

      // The comment stays reachable from the commented words beside the link.
      release(besideLink);
      expect(clickedMarks).toEqual(["comment-1"]);
    },
    { onMarkClick: (markId) => void clickedMarks.push(markId), openLink }
  );
});

test("a click that ends a drag across a link opens nothing", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const away = linkOf(view, "the plan");
      const before = away.previousSibling ?? away.parentElement?.firstChild;
      const inside = away.firstChild;
      if (before === null || before === undefined || inside === null) throw new Error("no text");
      const range = view.dom.ownerDocument.createRange();
      range.setStart(before, 0);
      range.setEnd(inside, 3);
      const selection = view.dom.ownerDocument.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);

      expect(offer(view, away, "click").handled).toBe(false);
      expect(opened).toEqual([]);
      // A Ctrl click is never a drag's end: ProseMirror reads its press as selecting the node it
      // lands in, which leaves a selection across the link that the reader never made.
      expect(offer(view, away, "click", { ctrlKey: true }).handled).toBe(true);
      expect(opened).toEqual([["https://example.com/a", true]]);

      // Once the selection collapses, the next click opens it.
      selection?.collapse(inside, 1);
      expect(offer(view, away, "click").handled).toBe(true);
      expect(opened).toEqual([
        ["https://example.com/a", true],
        ["https://example.com/a", false],
      ]);
    },
    { openLink }
  );
});

test("an anchor inside a host island is the host's, and one a node view draws as its own is the editor's", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const paragraph = view.dom.querySelector("p");
      if (paragraph === null) throw new Error("no paragraph");
      // A slot a host draws into: an element the editor holds no text in, its anchors deeper
      // inside; beside it, a leaf's own root holding its link, as Dispatch's picture fallback is.
      const island = view.dom.ownerDocument.createElement("span");
      island.setAttribute("contenteditable", "false");
      island.innerHTML = '<span><a href="https://example.com/thread">a thread link</a></span>';
      const leaf = view.dom.ownerDocument.createElement("span");
      leaf.setAttribute("contenteditable", "false");
      leaf.innerHTML =
        '<a href="/issues/CORE-1/artifacts/notes-md" data-dispatch-href="dispatch://CORE-1/artifact/notes-md">CORE-1 notes-md</a>';
      paragraph.append(island, leaf);
      try {
        expect(offer(view, linkOf(view, "a thread link"), "click").handled).toBe(false);
        expect(offer(view, linkOf(view, "a thread link"), "auxclick", { button: 1 }).handled).toBe(
          false
        );
        expect(offer(view, linkOf(view, "CORE-1 notes-md"), "click").handled).toBe(true);
        expect(opened).toEqual([["dispatch://CORE-1/artifact/notes-md", false]]);
      } finally {
        island.remove();
        leaf.remove();
      }
    },
    { openLink }
  );
});

/** A plan whose task serves a requirement of another issue's spec. A paragraph comes first, so
 *  the editor's first caret has text to stand in. */
const PLAN_TASK =
  'The plan.\n\n:::plan_task{#t1 serves="dispatch://CORE-1/spec#b-r1"}\nShip the change.\n:::\n';

/** A typed block drawn as Dispatch's `typed-block-drawing.ts` draws one: a header the reader
 *  cannot edit, holding the block's first `serves` pointer as a link in a name/value pair, above
 *  the block's content. */
const drawTypedBlock: NonNullable<CreateProofEditorOptions["renderBlock"]> = (node) => {
  const [reference] = node.attrs.serves as string[];
  return [
    "section",
    { "data-proof-block-type": node.type.name },
    [
      "header",
      { contenteditable: "false", "data-proof-block-summary": "" },
      [
        "dl",
        {},
        ["dt", {}, "serves"],
        [
          "dd",
          {},
          [
            "a",
            { "data-dispatch-href": reference, href: "/issues/CORE-1/spec#b-r1" },
            "CORE-1 spec block",
          ],
        ],
      ],
    ],
    ["div", { "data-proof-block-content": "" }, 0],
  ];
};

// dispatch://INFRA-2160, R1: a plan's or a Verification document's `serves` and `needs` pointers
// sit two elements below a header that is `contenteditable="false"`, and a click on one opens it
// through the host as any link of the document does, with no new load of the host's page.
test("a pointer in a typed block's header opens its reference, and Ctrl and a middle click open it in a new tab", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    PLAN_TASK,
    ({ view }) => {
      const pointer = view.dom.querySelector(
        "header[contenteditable=false] > dl > dd > a[data-dispatch-href]"
      );
      if (pointer === null) throw new Error("the plan's task drew no pointer in its header");

      expect(offer(view, pointer, "click").handled).toBe(true);
      expect(dispatch(pointer, "click").defaultPrevented).toBe(true);
      dispatch(pointer, "click", { ctrlKey: true });
      expect(dispatch(pointer, "mousedown", { button: 1 }).defaultPrevented).toBe(true);
      expect(dispatch(pointer, "mouseup", { button: 1 }).defaultPrevented).toBe(true);
      expect(dispatch(pointer, "auxclick", { button: 1 }).defaultPrevented).toBe(true);
      expect(opened).toEqual([
        ["dispatch://CORE-1/spec#b-r1", false],
        ["dispatch://CORE-1/spec#b-r1", false],
        ["dispatch://CORE-1/spec#b-r1", true],
        ["dispatch://CORE-1/spec#b-r1", true],
      ]);

      // The same header drawn inside a host island - a typed block of a comment a host draws
      // into a thread's slot - is the host's.
      const content = view.dom.querySelector("[data-proof-block-content] p");
      if (content === null) throw new Error("the plan's task holds no paragraph");
      const island = view.dom.ownerDocument.createElement("span");
      island.setAttribute("contenteditable", "false");
      island.innerHTML =
        '<span><header contenteditable="false" data-proof-block-summary=""><dl><dd><a href="/issues/CORE-1/spec#b-r1" data-dispatch-href="dispatch://CORE-1/spec#b-r1">in a thread</a></dd></dl></header></span>';
      content.append(island);
      try {
        expect(offer(view, linkOf(view, "in a thread"), "click").handled).toBe(false);
        expect(opened).toHaveLength(4);
      } finally {
        island.remove();
      }
    },
    { blockSchema: serverBlockSchema, openLink, renderBlock: drawTypedBlock }
  );
});

test("a click on a drawn title opens its reference, and Ctrl and a middle click open it in a new tab", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    "See CORE-1 here.",
    ({ handle, view }) => {
      handle.setReferences({
        onKeys: () => {},
        scan: (text): ReferenceCandidate[] => {
          const start = text.indexOf("CORE-1");
          return start === -1 ? [] : [{ end: start + 6, key: "CORE-1", start }];
        },
        targets: new Map([
          [
            "CORE-1",
            { href: "dispatch://CORE-1", path: "/issues/CORE-1", title: "CORE-1 Fix the thing" },
          ],
        ]),
      });
      const title = view.dom.querySelector("a.dispatch-reference");
      if (title === null) throw new Error("no title drawn");

      expect(dispatch(title, "click").defaultPrevented).toBe(true);
      dispatch(title, "click", { ctrlKey: true });
      expect(dispatch(title, "mousedown", { button: 1 }).defaultPrevented).toBe(true);
      expect(dispatch(title, "mouseup", { button: 1 }).defaultPrevented).toBe(true);
      expect(dispatch(title, "auxclick", { button: 1 }).defaultPrevented).toBe(true);

      expect(opened).toEqual([
        ["dispatch://CORE-1", false],
        ["dispatch://CORE-1", true],
        ["dispatch://CORE-1", true],
      ]);
    },
    { openLink }
  );
});

test("Alt+Enter at a link is left to the host's key, which opens it and changes no text", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ handle, view }) => {
      const stored = handle.getMarkdown();
      // Inside the link, and at either end of it.
      for (const [pos, target] of [
        [8, "https://example.com/a"],
        [6, "https://example.com/a"],
        [14, "https://example.com/a"],
        [25, "dispatch://CORE-2"],
      ] as const) {
        caretAt(view, pos);
        expect({ pos, target: handle.linkAtCaret() }).toEqual({ pos, target });
      }

      caretAt(view, 8);
      // The host's keymap listens on the window: it hears the press unprevented, and takes it.
      const heard: boolean[] = [];
      const host = (event: Event) => {
        heard.push(event.defaultPrevented);
        event.preventDefault();
      };
      window.addEventListener("keydown", host);
      try {
        const press = altEnter(view);
        expect(press.handled).toBe(true);
        window.dispatchEvent(press.event);
        expect(heard).toEqual([false]);
      } finally {
        window.removeEventListener("keydown", host);
      }
      expect(handle.openLinkAtCaret()).toBe(true);
      expect(opened).toEqual([["https://example.com/a", false]]);

      // Where nothing after the editor takes the press, it is prevented once it has gone by, so
      // the browser writes no new line.
      const untaken = altEnter(view);
      expect(untaken.handled).toBe(true);
      window.dispatchEvent(untaken.event);
      expect(untaken.event.defaultPrevented).toBe(true);

      // Away from every link the press is ProseMirror's own, which writes nothing either.
      caretAt(view, 3);
      expect(handle.linkAtCaret()).toBeNull();
      expect(altEnter(view).handled).toBe(false);
      expect(handle.openLinkAtCaret()).toBe(false);

      expect(opened).toHaveLength(1);
      expect(handle.getMarkdown()).toBe(stored);
    },
    { openLink }
  );
});

test("Alt+Enter beside a bare key opens the reference its title stood for", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    "See CORE-1 here.",
    ({ handle, view }) => {
      handle.setReferences({
        onKeys: () => {},
        scan: (text): ReferenceCandidate[] => {
          const start = text.indexOf("CORE-1");
          return start === -1 ? [] : [{ end: start + 6, key: "CORE-1", start }];
        },
        targets: new Map([
          [
            "CORE-1",
            { href: "dispatch://CORE-1", path: "/issues/CORE-1", title: "CORE-1 Fix the thing" },
          ],
        ]),
      });
      const stored = handle.getMarkdown();
      // "See CORE-1 here." opens at 0, so CORE-1 is 5 to 11: the caret is right after it.
      caretAt(view, 11);
      expect(handle.linkAtCaret()).toBe("dispatch://CORE-1");
      expect(altEnter(view).handled).toBe(true);
      expect(handle.openLinkAtCaret()).toBe(true);
      expect(opened).toEqual([["dispatch://CORE-1", false]]);
      expect(handle.getMarkdown()).toBe(stored);
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, k-link-keys: an arrow key moves the page's caret at once, and the
// editor's state follows only once the page's `selectionchange` has run, or once ProseMirror's own
// handler of the next key reads the page's selection, after the handlers of `handleDOMEvents`. A
// busy page ran Alt+Enter before that event, the press read the caret where it was before the
// arrow, beside no title, and ProseMirror prevented it, so the host's key never opened the key.
test("Alt+Enter straight after an arrow key reads the caret where the page holds it", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    "See CORE-1 here.",
    ({ handle, view }) => {
      handle.setReferences({
        onKeys: () => {},
        scan: (text): ReferenceCandidate[] => {
          const start = text.indexOf("CORE-1");
          return start === -1 ? [] : [{ end: start + 6, key: "CORE-1", start }];
        },
        targets: new Map([
          [
            "CORE-1",
            { href: "dispatch://CORE-1", path: "/issues/CORE-1", title: "CORE-1 Fix the thing" },
          ],
        ]),
      });
      const stored = handle.getMarkdown();
      view.focus();
      // "See CORE-1 here." opens at 0, so the space before CORE-1 is 4 to 5: the caret is before
      // it, beside no title.
      caretAt(view, 4);
      expect(handle.linkAtCaret()).toBeNull();

      // The arrow's move, past the space onto the key's start. The page has not run its
      // `selectionchange` yet, so the editor's listener does not hear it.
      const before = view.dom.querySelector("p")?.firstChild;
      expect(before?.textContent).toBe("See ");
      if (before === null || before === undefined) throw new Error("the paragraph holds no text");
      const unheard = (event: Event) => event.stopImmediatePropagation();
      view.dom.ownerDocument.addEventListener("selectionchange", unheard, true);
      try {
        window.getSelection()?.collapse(before, 4);
        expect(view.state.selection.head).toBe(4);

        expect(altEnter(view).handled).toBe(true);
        expect(handle.linkAtCaret()).toBe("dispatch://CORE-1");
        expect(handle.openLinkAtCaret()).toBe(true);
      } finally {
        view.dom.ownerDocument.removeEventListener("selectionchange", unheard, true);
      }
      expect(opened).toEqual([["dispatch://CORE-1", false]]);
      expect(handle.getMarkdown()).toBe(stored);
    },
    { openLink }
  );
});

test("a view the reader cannot edit leaves Alt+Enter alone, and opens links on a click all the same", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      caretAt(view, 8);
      expect(altEnter(view).handled).toBe(false);
      expect(dispatch(linkOf(view, "the plan"), "click").defaultPrevented).toBe(true);
      expect(opened).toEqual([["https://example.com/a", false]]);
    },
    { access: "read", openLink }
  );
});

test("an editor with no link opener opens no link", async () => {
  await withMarksEditor(LINKS, ({ handle, view }) => {
    const away = linkOf(view, "the plan");
    const click = offer(view, away, "click");
    expect(click.handled).toBe(false);
    expect(click.event.defaultPrevented).toBe(false);
    expect(offer(view, away, "auxclick", { button: 1 }).handled).toBe(false);
    caretAt(view, 8);
    expect(altEnter(view).handled).toBe(false);
    expect(handle.openLinkAtCaret()).toBe(false);
  });
});

test("the editor's stylesheet holds no Open link card", async () => {
  const stylesheet = await Bun.file(new URL("../src/lib.css", import.meta.url)).text();

  expect(stylesheet).not.toContain("markdown-link-action-card");
  expect(stylesheet).not.toContain("markdown-link-hover-target");
});
