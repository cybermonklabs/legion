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
 *  listeners hand it to the editor's handlers. A browser's mouse event also carries `x` and `y`,
 *  the same numbers as `clientX` and `clientY`, which prosemirror-view's `updateAllowDefault`
 *  reads the place of its press from; happy-dom's carries neither, so they are added. Only for a
 *  press, a move, a release or a middle click: happy-dom follows an unprevented click on a link
 *  by navigating its window, so a click goes through `click`. */
function dispatch(target: EventTarget, type: string, init: MouseEventInit = {}): MouseEvent {
  const event = new window.MouseEvent(type, {
    bubbles: true,
    button: 0,
    cancelable: true,
    ...init,
  });
  Object.defineProperties(event, { x: { value: event.clientX }, y: { value: event.clientY } });
  target.dispatchEvent(event);
  return event;
}

/** Offers a mouse event of `type`, aimed at `target`, to the editor's DOM event handlers as
 *  ProseMirror's own listener does, with no default action to follow, and answers whether one
 *  handled it: for an event the editor leaves to the browser or to the host. */
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

/** Where the pointer is, as `clientX` and `clientY`, in CSS pixels from the window's corner:
 *  happy-dom lays nothing out, so a row names the place itself. */
type Place = readonly [number, number];

/** Lays the page out for a press on `target`, as far as ProseMirror reads it. happy-dom finds no
 *  document position at any point (`posAtCoords` answers null), and prosemirror-view's
 *  `handlers.mousedown` starts no press of its own where it finds none. The view answers instead
 *  with the position at the start of `target`'s content and the node holding it, as a browser
 *  answers for a press on the element's first words. */
function layOutPressOn(view: EditorView, target: Element): void {
  const pos = view.posAtDOM(target, 0);
  const $pos = view.state.doc.resolve(pos);
  view.posAtCoords = () => ({ inside: $pos.depth > 0 ? $pos.before() : -1, pos });
}

/** A press of the primary button on `target` at `place`, with `keys` held, as a browser sends it:
 *  the editor's own handlers hear it, then ProseMirror's, which starts its press there
 *  (`layOutPressOn`) unless the press was prevented or taken first. `detail` is the browser's
 *  count of the clicks in a row: 2 for the second press of a double click. Answers the press. */
function press(
  view: EditorView,
  target: Element,
  [clientX, clientY]: Place,
  keys: MouseEventInit = {},
  detail = 1
): MouseEvent {
  layOutPressOn(view, target);
  return dispatch(target, "mousedown", { ...keys, clientX, clientY, detail });
}

/** The pointer moved to `place` with the primary button held, heard on the page, where
 *  ProseMirror hears the pointer of its press (the view's root), on `over` when named, else on the
 *  page itself. */
function moveTo(view: EditorView, [clientX, clientY]: Place, over?: Element): void {
  dispatch(over ?? view.dom.ownerDocument, "mousemove", { buttons: 1, clientX, clientY });
}

/** The release of the primary button on `target` at `place`, with `keys` held, `detail` counting
 *  the clicks in a row as `press` does. Answers the release, whose default ProseMirror prevents
 *  when `handleClick` took it as a click. */
function release(
  target: Element,
  [clientX, clientY]: Place,
  keys: MouseEventInit = {},
  detail = 1
): MouseEvent {
  return dispatch(target, "mouseup", { ...keys, clientX, clientY, detail });
}

/** Sends `target` a click, `init` naming its place, its keys and its `detail` - 1 for a pointer's,
 *  0 for one no pointer makes (Enter on a focused link, assistive technology, a script) - and
 *  answers whether the editor prevented its default, so the browser would follow no `href`. The
 *  click's last listener reads that, then prevents the default itself: happy-dom follows an
 *  unprevented click on a link by navigating its window. That listener is the page's, and for a
 *  target out of the page, which no listener of the page hears, the target's own, added after
 *  every listener the editor added to it. */
function click(target: Element, init: MouseEventInit): boolean {
  const last: EventTarget = target.isConnected ? target.ownerDocument : target;
  let prevented = false;
  const read = (event: Event) => {
    prevented = event.defaultPrevented;
    event.preventDefault();
  };
  last.addEventListener("click", read);
  try {
    dispatch(target, "click", init);
  } finally {
    last.removeEventListener("click", read);
  }
  return prevented;
}

/** A click no pointer makes - Enter on a focused link, assistive technology, a script - on
 *  `target`, with `keys` held: `detail` 0, at the window's corner. Answers whether the editor
 *  prevented its default. */
function keyboardClick(target: Element, keys: MouseEventInit = {}): boolean {
  return click(target, { ...keys, clientX: 0, clientY: 0, detail: 0 });
}

/** The nearest element holding both `pressed` and `released`, which a browser sends the click of
 *  a press on the one and a release on the other to. */
function commonAncestor(pressed: Element, released: Element): Element {
  for (let node: Element | null = pressed; node !== null; node = node.parentElement) {
    if (node.contains(released)) return node;
  }
  throw new Error("no element holds both the press and the release");
}

/** One pointer click: where the press goes down, the places the pointer moves through with the
 *  button held, the element the release lands on (the press's own unless named), the keys held
 *  throughout, and the browser's count of the clicks in a row (`press`), 1 unless named. */
interface PointerClick {
  readonly at: Place;
  readonly through?: readonly Place[];
  readonly releaseOn?: Element;
  readonly keys?: MouseEventInit;
  readonly detail?: number;
}

/** A pointer click on `target` as a browser sends it: the press (`press`), each move, the release
 *  where the pointer last was, then the click, sent to the nearest element holding both the press
 *  and the release (`commonAncestor`). Answers whether the editor prevented the click's default.
 *  ProseMirror reads a press within 10 px and 500 ms of its last one as a double click, which
 *  starts no press of its own (prosemirror-view's `handlers.mousedown`, `isNear`), so the plain
 *  presses of a row go down 20 px or more apart unless the row makes a double click. */
function pointerClick(
  view: EditorView,
  target: Element,
  { at, through = [], releaseOn = target, keys = {}, detail = 1 }: PointerClick
): boolean {
  press(view, target, at, keys, detail);
  for (const place of through) moveTo(view, place);
  const [clientX, clientY] = through.at(-1) ?? at;
  release(releaseOn, [clientX, clientY], keys, detail);
  return click(commonAncestor(target, releaseOn), { ...keys, clientX, clientY, detail });
}

/** A middle click on `target` as a browser sends it - the press, the release, then `auxclick` -
 *  answering whether each one's default was prevented. */
function middleClick(target: Element): {
  readonly auxclick: boolean;
  readonly press: boolean;
  readonly release: boolean;
} {
  const point = { button: 1, clientX: 0, clientY: 0, detail: 1 };
  return {
    press: dispatch(target, "mousedown", point).defaultPrevented,
    release: dispatch(target, "mouseup", point).defaultPrevented,
    auxclick: dispatch(target, "auxclick", point).defaultPrevented,
  };
}

// dispatch://INFRA-2160, R1: a plain pointer click on a link opens it once, through ProseMirror's
// own press and `handleClick`, in every access mode; Ctrl and Cmd open it once in a new tab
// through the click, whose press the editor takes; a middle click opens it once in a new tab
// through `auxclick`. Every click on the link is prevented, so the browser follows no `href`.
test("a plain pointer click opens a link once in every access mode, and Ctrl, Cmd and a middle click open it once in a new tab", async () => {
  for (const access of ["edit", "comment", "read"] as const satisfies ProofEditorAccess[]) {
    const { openLink, opened } = recordedOpener();
    await withMarksEditor(
      LINKS,
      ({ view }) => {
        const away = linkOf(view, "the plan");
        const issue = linkOf(view, "the other issue");

        const prevented = [
          pointerClick(view, away, { at: [20, 10] }),
          pointerClick(view, issue, { at: [60, 10] }),
          pointerClick(view, issue, { at: [100, 10], keys: { ctrlKey: true } }),
          pointerClick(view, away, { at: [140, 10], keys: { metaKey: true } }),
        ];
        const middle = middleClick(issue);
        // A right button's click is the browser's own.
        const right = offer(view, away, "auxclick", { button: 2 }).handled;

        expect({ access, middle, opened, prevented, right }).toEqual({
          access,
          middle: { auxclick: true, press: true, release: true },
          opened: [
            ["https://example.com/a", false],
            ["dispatch://CORE-2", false],
            ["dispatch://CORE-2", true],
            ["https://example.com/a", true],
            ["dispatch://CORE-2", true],
          ],
          prevented: [true, true, true, true],
          right: false,
        });
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
      // Away from a link a middle press is the editor's as before, and a primary one with no
      // modifier is always.
      const paragraph = view.dom.querySelector("p");
      if (paragraph === null) throw new Error("no paragraph");
      expect(offer(view, paragraph, "mouseup", { button: 1 }).handled).toBe(false);
      expect(offer(view, linkOf(view, "the plan"), "mousedown").handled).toBe(false);
      expect(handle.getMarkdown()).toBe(stored);
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: the margin's `handleClick` runs before the link's and stands down on
// `opensLinkOn`, so a pointer click on a link inside commented text opens the link and names no
// comment, and a click on the commented words beside it still names the comment.
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

      const onLink = pointerClick(view, away, { at: [20, 10] });
      const marksOnLink = [...clickedMarks];
      // The comment stays reachable from the commented words beside the link, whose click is no
      // link's and is left to the browser.
      const beside = pointerClick(view, besideLink, { at: [80, 10] });

      expect({ beside, clickedMarks, marksOnLink, onLink, opened }).toEqual({
        beside: false,
        clickedMarks: ["comment-1"],
        marksOnLink: [],
        onLink: true,
        opened: [["https://example.com/a", false]],
      });
    },
    { onMarkClick: (markId) => void clickedMarks.push(markId), openLink }
  );
});

/** `node`, which a row expects to be a text node of the paragraph. */
function textOf(node: Node | null): Node {
  if (node === null || node.nodeType !== 3) throw new Error("no text node there");
  return node;
}

/** Selects the page's text from `start` in `from` to `end` in `to`, as a drag over it leaves
 *  it, or the reader's selection the selection bar's Comment starts from. */
function selectText(view: EditorView, from: Node, start: number, to: Node, end: number): Selection {
  const selection = view.dom.ownerDocument.getSelection();
  if (selection === null) throw new Error("the page has no selection");
  const range = view.dom.ownerDocument.createRange();
  range.setStart(from, start);
  range.setEnd(to, end);
  selection.removeAllRanges();
  selection.addRange(range);
  return selection;
}

/** Puts the editable editor in a state with `make`, then makes one plain pointer click on "the
 *  other issue" at 60 px (`pointerClick`), and answers whether the editor prevented that click and
 *  every target the host was handed to open, `make`'s own included. */
async function pointerClickAfter(
  make: (view: EditorView, issue: Element) => void
): Promise<{ readonly opened: Array<[string, boolean]>; readonly prevented: boolean }> {
  const { openLink, opened } = recordedOpener();
  let prevented = false;
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");
      make(view, issue);
      prevented = pointerClick(view, issue, { at: [60, 10] });
    },
    { openLink }
  );
  return { opened, prevented };
}

// dispatch://INFRA-2160, R1: a plain pointer click opens a link once, whatever the page held
// selected before the press: ProseMirror reads the press and its release, not the selection.
test("a plain pointer click on a link opens it once when a selection holds the link", async () => {
  const clicked = await pointerClickAfter((view, issue) => {
    // "and the other issue today", as the reader selects a sentence to comment on it.
    const selection = selectText(
      view,
      textOf(issue.previousSibling),
      1,
      textOf(issue.nextSibling),
      6
    );
    expect(selection.toString()).toBe("and the other issue today");
  });

  expect(clicked).toEqual({ opened: [["dispatch://CORE-2", false]], prevented: true });
});

test("a plain pointer click on a link opens it once when a selection elsewhere in the document stands", async () => {
  const clicked = await pointerClickAfter((view, issue) => {
    // "today", after the link.
    const after = textOf(issue.nextSibling);
    const selection = selectText(view, after, 1, after, 6);
    expect(selection.toString()).toBe("today");
  });

  expect(clicked).toEqual({ opened: [["dispatch://CORE-2", false]], prevented: true });
});

test("a plain pointer click on a link opens it once when the caret is inside the link", async () => {
  const clicked = await pointerClickAfter((view, issue) => {
    const selection = view.dom.ownerDocument.getSelection();
    if (selection === null) throw new Error("the page has no selection");
    selection.collapse(textOf(issue.firstChild), 4);
    expect(selection.isCollapsed).toBe(true);
  });

  expect(clicked).toEqual({ opened: [["dispatch://CORE-2", false]], prevented: true });
});

for (const [key, keys] of [
  ["Ctrl", { ctrlKey: true }],
  ["Cmd", { metaKey: true }],
] as const) {
  test(`a plain pointer click on a link opens it once after a ${key} click on the link`, async () => {
    const clicked = await pointerClickAfter((view, issue) => {
      pointerClick(view, issue, { at: [20, 10], keys });
    });

    expect(clicked).toEqual({
      opened: [
        ["dispatch://CORE-2", true],
        ["dispatch://CORE-2", false],
      ],
      prevented: true,
    });
  });
}

test("a plain pointer click on a link opens it once after a middle click on the link", async () => {
  const clicked = await pointerClickAfter((_view, issue) => {
    middleClick(issue);
  });

  expect(clicked).toEqual({
    opened: [
      ["dispatch://CORE-2", true],
      ["dispatch://CORE-2", false],
    ],
    prevented: true,
  });
});

// dispatch://INFRA-2160, R1: ProseMirror tells a click from a drag (prosemirror-view's
// `updateAllowDefault`): a press and its release with the pointer never more than 4 px from the
// press along either axis is a click, which it hands to `handleClick`.
test("a press, a move of 4 px or less and the release on a link open it once", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      const moved = pointerClick(view, issue, { at: [60, 10], through: [[63, 10]] });
      // A hand that trembles 4 px along each axis between the press and the release still clicks.
      const trembled = pointerClick(view, issue, { at: [100, 10], through: [[104, 14]] });

      expect({ opened, prevented: { moved, trembled } }).toEqual({
        opened: [
          ["dispatch://CORE-2", false],
          ["dispatch://CORE-2", false],
        ],
        prevented: { moved: true, trembled: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a drag on a link selects its words and opens nothing. The pointer
// more than 4 px from the press along either axis at any moment before the release makes a drag,
// whose release ProseMirror hands to no `handleClick`; the click after it is prevented, so the
// browser follows no `href` either.
test("a drag of more than 4 px that starts and ends on a link opens nothing, and nor does one that starts off the link", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");
      const away = linkOf(view, "the plan");
      const words = textOf(issue.firstChild);
      const paragraph = view.dom.querySelector("p");
      if (paragraph === null || away.parentElement !== paragraph) {
        throw new Error("the link is not drawn straight inside its paragraph");
      }

      // The drag over "other": the press, the pointer 5 px on, one past the threshold, the words
      // it went over selected, and the release and the click there.
      press(view, issue, [60, 10]);
      moveTo(view, [65, 10]);
      selectText(view, words, 4, words, 9);
      release(issue, [65, 10]);
      const dragged = click(issue, { clientX: 65, clientY: 10, detail: 1 });

      // A drag along the link and back: the release is 1 px from the press, and the pointer went
      // 20 px away between them.
      view.dom.ownerDocument.getSelection()?.collapse(words, 4);
      const returned = pointerClick(view, issue, {
        at: [100, 10],
        through: [
          [120, 10],
          [101, 10],
        ],
      });

      // A drag from "Read" onto "the plan": the browser sends the click to the paragraph, which
      // holds both the press and the release, and is no link.
      const fromBefore = pointerClick(view, paragraph, {
        at: [140, 10],
        releaseOn: away,
        through: [[170, 10]],
      });

      expect({ opened, prevented: { dragged, fromBefore, returned } }).toEqual({
        opened: [],
        prevented: { dragged: true, fromBefore: false, returned: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: ProseMirror hears the pointer of its press on the page the editor is
// in, until the release: a drag that leaves the editor and comes back to the press is a drag.
test("a drag that leaves the editor and comes back to the press opens nothing, and the same press and release with no move between them opens the link once", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");
      const page = view.dom.ownerDocument.body;
      expect(view.dom.contains(page)).toBe(false);

      // The press, the pointer out of the editor 200 px away, back over the link at the press,
      // and the release and the click there.
      press(view, issue, [60, 10]);
      moveTo(view, [260, 10], page);
      moveTo(view, [60, 10], issue);
      release(issue, [60, 10]);
      const left = click(issue, { clientX: 60, clientY: 10, detail: 1 });

      // The same press and release, the pointer staying where it went down.
      const stayed = pointerClick(view, issue, { at: [100, 10] });

      // A drag released outside the editor: ProseMirror's press ends there with no click, and the
      // browser sends the click to the page's body, which holds the press and the release.
      const releasedOutside = pointerClick(view, issue, {
        at: [140, 10],
        releaseOn: page,
        through: [[340, 10]],
      });

      expect({ opened, prevented: { left, releasedOutside, stayed } }).toEqual({
        opened: [["dispatch://CORE-2", false]],
        prevented: { left: true, releasedOutside: false, stayed: true },
      });
    },
    { openLink }
  );
});

/** Reconfigures the editor's plugins, as the collab cursor's install
 *  (`installCollabCursorsWhenReady`) and Dispatch's hold on a block's edits (`holdEditsToBlock`)
 *  do while a press can be under way: ProseMirror destroys every plugin view and makes them anew,
 *  and its own press goes on, until the view's destroy (prosemirror-view's `destroyInput`). */
function reconfigure(view: EditorView): void {
  view.updateState(view.state.reconfigure({ plugins: [...view.state.plugins] }));
}

// dispatch://INFRA-2160, R1: ProseMirror's press outlives a reconfigure of the editor's plugins,
// so a click across one opens the link once and a drag across one opens nothing.
test("a press held across a reconfigure of the editor's plugins opens the link once on a click, and nothing on a drag", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      // The press, the plugins reconfigured, and the release and the click in place.
      press(view, issue, [60, 10]);
      reconfigure(view);
      release(issue, [60, 10]);
      const clicked = click(issue, { clientX: 60, clientY: 10, detail: 1 });

      // The same with the pointer 5 px on before the release.
      press(view, issue, [100, 10]);
      reconfigure(view);
      moveTo(view, [105, 10]);
      release(issue, [105, 10]);
      const dragged = click(issue, { clientX: 105, clientY: 10, detail: 1 });

      expect({ connected: issue.isConnected, opened, prevented: { clicked, dragged } }).toEqual({
        // The reconfigures drew the link anew neither time.
        connected: true,
        opened: [["dispatch://CORE-2", false]],
        prevented: { clicked: true, dragged: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a click no pointer makes (`detail` 0, or an empty `pointerType`)
// opens the link it lands on, whatever press came before it, a press no click ended included:
// Chromium sends no click for a press held across a reconfigure of the editor's plugins, and no
// browser sends one once the pressed element is gone or the browser has turned the press into a
// drag of its own.
test("Enter's click on a link opens it once after a drag on the link that no click ended, and once in a new tab with Ctrl", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      // A press, a drag of 10 px and the release on the link, and no click.
      press(view, issue, [60, 10]);
      moveTo(view, [70, 10]);
      release(issue, [70, 10]);
      const afterDrag = keyboardClick(issue);

      // A press the browser turns into a drag of its own, with neither a release nor a click: the
      // pointer moves on with no button held, which ends ProseMirror's press too.
      press(view, issue, [100, 10]);
      moveTo(view, [110, 10]);
      dispatch(view.dom.ownerDocument, "mousemove", { clientX: 100, clientY: 10 });
      const afterNativeDrag = keyboardClick(issue);

      const newTab = keyboardClick(issue, { ctrlKey: true });

      expect({ opened, prevented: { afterDrag, afterNativeDrag, newTab } }).toEqual({
        opened: [
          ["dispatch://CORE-2", false],
          ["dispatch://CORE-2", false],
          ["dispatch://CORE-2", true],
        ],
        prevented: { afterDrag: true, afterNativeDrag: true, newTab: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a click is a `PointerEvent`, and an empty `pointerType` is what says
// no pointer made it. Firefox sends Enter's click on a focused link with `detail` 1 and an empty
// `pointerType`, and Chromium with `detail` 0 (builds Job `infra-2160-probe-click-new-6gxl8`,
// 2026-11-23); a mouse's click names its pointer in both, and opens nothing here with no press
// before it, as the end of a drag would not.
test("a click whose pointerType is empty opens a link once whatever its detail, and a mouse's click with no press before it opens nothing", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");
      const send = (init: PointerEventInit): boolean => {
        const page = issue.ownerDocument;
        let prevented = false;
        const read = (event: Event) => {
          prevented = event.defaultPrevented;
          event.preventDefault();
        };
        page.addEventListener("click", read);
        try {
          issue.dispatchEvent(
            new window.PointerEvent("click", {
              bubbles: true,
              button: 0,
              cancelable: true,
              ...init,
            })
          );
        } finally {
          page.removeEventListener("click", read);
        }
        return prevented;
      };
      const firefoxEnter = send({ detail: 1, pointerType: "" });
      const chromiumEnter = send({ detail: 0, pointerType: "" });
      const mouse = send({ clientX: 60, clientY: 10, detail: 1, pointerType: "mouse" });
      expect({ opened, prevented: { chromiumEnter, firefoxEnter, mouse } }).toEqual({
        opened: [
          ["dispatch://CORE-2", false],
          ["dispatch://CORE-2", false],
        ],
        prevented: { chromiumEnter: true, firefoxEnter: true, mouse: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: ProseMirror reads a primary press with Ctrl (Cmd on a Mac) as
// selecting the node it lands in, here the paragraph around the link, in place of the reader's
// caret or selection. The editor takes that press, so ProseMirror never sees it, and the click
// opens the link once in a new tab and changes nothing in the document's selection.
test("a Ctrl or Cmd click on a link opens it once in a new tab and leaves the selection as it was, and a plain click after it opens it once", async () => {
  for (const keys of [{ ctrlKey: true }, { metaKey: true }] satisfies MouseEventInit[]) {
    const { openLink, opened } = recordedOpener();
    await withMarksEditor(
      LINKS,
      ({ view }) => {
        const issue = linkOf(view, "the other issue");
        const selection = () => ({
          collapsed: view.dom.ownerDocument.getSelection()?.isCollapsed,
          editor: view.state.selection.toJSON(),
        });
        view.focus();
        caretAt(view, 3);
        const before = selection();

        // The modified click as a browser sends it, its press in the link's words, inside the
        // paragraph that opens at 0 (`layOutPressOn`), where ProseMirror would select that
        // paragraph.
        const modifiedPress = press(view, issue, [60, 10], keys);
        release(issue, [60, 10], keys);
        const modified = click(issue, { ...keys, clientX: 60, clientY: 10, detail: 1 });
        const afterModified = selection();
        // The plain click after it, whose press ProseMirror hears as before.
        const plain = pointerClick(view, issue, { at: [100, 10] });
        // With Shift or Alt held too the press is ProseMirror's.
        const shiftOrAltTaken = [{ shiftKey: true }, { altKey: true }].map(
          (more) => offer(view, issue, "mousedown", { ...keys, ...more }).handled
        );

        expect({
          keys,
          opened,
          prevented: { modified, modifiedPress: modifiedPress.defaultPrevented, plain },
          selection: afterModified,
          shiftOrAltTaken,
        }).toEqual({
          keys,
          opened: [
            ["dispatch://CORE-2", true],
            ["dispatch://CORE-2", false],
          ],
          prevented: { modified: true, modifiedPress: true, plain: true },
          selection: before,
          shiftOrAltTaken: [false, false],
        });
      },
      { openLink }
    );
  }
});

// dispatch://INFRA-2160, R1: a Shift or an Alt click on a link opens nothing, and its default is
// prevented, so the browser opens no window and downloads nothing either.
test("a Shift or an Alt click on a link opens nothing and is prevented", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      const prevented = {
        alt: pointerClick(view, issue, { at: [60, 10], keys: { altKey: true } }),
        altKeyboard: keyboardClick(issue, { altKey: true }),
        shift: pointerClick(view, issue, { at: [20, 10], keys: { shiftKey: true } }),
        shiftKeyboard: keyboardClick(issue, { shiftKey: true }),
      };

      expect({ opened, prevented }).toEqual({
        opened: [],
        prevented: { alt: true, altKeyboard: true, shift: true, shiftKeyboard: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: ProseMirror reads a press within 500 ms and 10 px of its last one,
// with the same button, as a double or a triple click (prosemirror-view's `handlers.mousedown`,
// `isNear`): it starts no press of its own for it, so no `handleClick` hears its release, and it
// calls `handleDoubleClick` or `handleTripleClick` at the press instead. A double click on a link
// opens it once, on its first release, and its second click opens nothing.
test("a double click on a link opens it once, on the first release, and its second click is prevented", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      press(view, issue, [60, 10]);
      const firstRelease = release(issue, [60, 10]).defaultPrevented;
      const openedOnRelease = [...opened];
      const first = click(issue, { clientX: 60, clientY: 10, detail: 1 });
      // The second click 2 px on, the browser counting it the second in a row.
      const second = pointerClick(view, issue, { at: [62, 10], detail: 2 });

      expect({ opened, openedOnRelease, prevented: { first, firstRelease, second } }).toEqual({
        opened: [["dispatch://CORE-2", false]],
        openedOnRelease: [["dispatch://CORE-2", false]],
        prevented: { first: true, firstRelease: true, second: true },
      });
    },
    { openLink }
  );
});

test("a triple click on a link opens it once, and its second and third clicks are prevented", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      // Three clicks 1 px apart, the browser counting them 1, 2 and 3.
      const prevented = [1, 2, 3].map((detail) =>
        pointerClick(view, issue, { at: [60 + detail, 10], detail })
      );

      expect({ opened, prevented }).toEqual({
        opened: [["dispatch://CORE-2", false]],
        prevented: [true, true, true],
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: Chromium sends no click for a press held across a reconfigure of the
// editor's plugins. `handleClick` opens the link on that press's release, and a press within
// 500 ms and 10 px after it is a double click to ProseMirror, whose click opens nothing: the link
// opens once.
test("a press and a release on a link that no click followed, then a click on it within half a second, open it once", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      press(view, issue, [60, 10]);
      reconfigure(view);
      const released = release(issue, [60, 10]).defaultPrevented;
      // No click; the second press 2 px on, the browser counting it the second in a row.
      const second = pointerClick(view, issue, { at: [62, 10], detail: 2 });

      expect({ opened, prevented: { released, second } }).toEqual({
        opened: [["dispatch://CORE-2", false]],
        prevented: { released: true, second: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a tap sends its press and its release, and its click only later.
// `handleClick` opens the link at the release, the host moves the page, the editor leaves it, and
// the click lands on the anchor out of the page, where no listener of the editor hears it: the
// tester's probe D10b/D10c, 2026-11-23, on a phone (Chromium with touch). That click is prevented
// all the same, so the browser follows no `href`, and it opens nothing more.
test("a tap's click that lands on a link out of the page after its release opened the link is prevented, and opens nothing more", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      press(view, issue, [60, 10]);
      release(issue, [60, 10]);
      // The host moved the page at the release, and the editor left it.
      issue.remove();
      const clicked = click(issue, { clientX: 60, clientY: 10, detail: 1 });

      expect({ connected: issue.isConnected, opened, prevented: clicked }).toEqual({
        connected: false,
        opened: [["dispatch://CORE-2", false]],
        prevented: true,
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a release `handleClick` opened a link on, with no click after it
// (Chromium sends none for a press held across a reconfigure), leaves the link a listener for its
// next click that prevents nothing the editor hears and stops nothing: Enter's click on the link
// later opens it.
test("a press and a release on a link that no click followed, then Enter's click on it, open it twice", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      press(view, issue, [60, 10]);
      release(issue, [60, 10]);
      // No click; Enter's click on the link, still in the page.
      const keyboard = keyboardClick(issue);

      expect({ connected: issue.isConnected, opened, prevented: keyboard }).toEqual({
        connected: true,
        opened: [
          ["dispatch://CORE-2", false],
          ["dispatch://CORE-2", false],
        ],
        prevented: true,
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a row of clicks ends at a press ProseMirror reads as a single click,
// so a link a click of an earlier row opened keeps no later double click from opening a link.
test("a click beside a link and, within half a second, a click on the link open it once after an earlier click opened another link", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const away = linkOf(view, "the plan");
      const issue = linkOf(view, "the other issue");
      const paragraph = view.dom.querySelector("p");
      if (paragraph === null || issue.parentElement !== paragraph) {
        throw new Error("the link is not drawn straight inside its paragraph");
      }

      const earlier = pointerClick(view, away, { at: [20, 10] });
      // 35 px from the earlier press, a single click to ProseMirror, which opens nothing.
      const beside = pointerClick(view, paragraph, { at: [55, 10] });
      const onLink = pointerClick(view, issue, { at: [60, 10] });

      expect({ opened, prevented: { beside, earlier, onLink } }).toEqual({
        opened: [
          ["https://example.com/a", false],
          ["dispatch://CORE-2", false],
        ],
        prevented: { beside: false, earlier: true, onLink: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a click on a link within 500 ms and 10 px of a click that opened
// nothing is a double click to ProseMirror, and opens the link once all the same.
test("a click on the text beside a link and, within half a second, a click on the link open it once", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");
      const paragraph = view.dom.querySelector("p");
      if (paragraph === null || issue.parentElement !== paragraph) {
        throw new Error("the link is not drawn straight inside its paragraph");
      }

      // A click on " and ", 5 px left of the link: it lands on no link, and is the browser's.
      const beside = pointerClick(view, paragraph, { at: [55, 10] });
      const onLink = pointerClick(view, issue, { at: [60, 10] });

      expect({ opened, prevented: { beside, onLink } }).toEqual({
        opened: [["dispatch://CORE-2", false]],
        prevented: { beside: false, onLink: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a Shift press is never a click to ProseMirror, and a press within
// 500 ms and 10 px of it is a double click to it: a plain click on a link straight after a Shift
// click on it opens the link once.
test("a plain click on a link within half a second of a Shift click on it opens it once", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      const shifted = pointerClick(view, issue, { at: [60, 10], keys: { shiftKey: true } });
      const plain = pointerClick(view, issue, { at: [60, 10] });

      expect({ opened, prevented: { plain, shifted } }).toEqual({
        opened: [["dispatch://CORE-2", false]],
        prevented: { plain: true, shifted: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: the editor takes a Ctrl or Cmd press on a link, so ProseMirror never
// sees it and no `handleClick` hears its release. With Ctrl let go before the button the click
// carries no Ctrl, and opens the link once in the same tab, as the platform follows such a click.
test("a Ctrl press on a link, Ctrl let go before the release, opens the link once in the same tab", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");

      const ctrlPress = press(view, issue, [60, 10], { ctrlKey: true }).defaultPrevented;
      release(issue, [60, 10]);
      const clicked = click(issue, { clientX: 60, clientY: 10, detail: 1 });

      expect({ opened, prevented: { clicked, ctrlPress } }).toEqual({
        opened: [["dispatch://CORE-2", false]],
        prevented: { clicked: true, ctrlPress: true },
      });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a press beside a link and its release on the link, within 4 px, is a
// click to ProseMirror, which hands the release to `handleClick`. The browser sends that click to
// the nearest element holding both the press and the release, which is no link, so the platform
// opens nothing, and nor does the editor.
test("a press beside a link and its release on the link within 4 px open nothing", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      const issue = linkOf(view, "the other issue");
      const paragraph = view.dom.querySelector("p");
      if (paragraph === null || issue.parentElement !== paragraph) {
        throw new Error("the link is not drawn straight inside its paragraph");
      }

      // The press on " and ", 2 px left of the link, and the release on the link 2 px on.
      const prevented = pointerClick(view, paragraph, {
        at: [58, 10],
        releaseOn: issue,
        through: [[60, 10]],
      });

      expect({ opened, prevented }).toEqual({ opened: [], prevented: false });
    },
    { openLink }
  );
});

// dispatch://INFRA-2160, R1: a link whose target the sanitizer emptied, a scheme it refuses, is
// the editor's all the same: its click is prevented and opens nothing.
test("a link whose target the sanitizer emptied opens nothing, and its click is prevented", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    "Run [this script](javascript:void) now.",
    ({ view }) => {
      const link = linkOf(view, "this script");
      expect(link.getAttribute("href")).toBe("");

      const prevented = {
        keyboard: keyboardClick(link),
        pointer: pointerClick(view, link, { at: [20, 10] }),
      };

      expect({ opened, prevented }).toEqual({
        opened: [],
        prevented: { keyboard: true, pointer: true },
      });
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
        expect(offer(view, linkOf(view, "a thread link"), "click", { detail: 1 }).handled).toBe(
          false
        );
        expect(offer(view, linkOf(view, "a thread link"), "auxclick", { button: 1 }).handled).toBe(
          false
        );
        // A pointer's click on the leaf's link, whose press never reaches ProseMirror.
        expect(offer(view, linkOf(view, "CORE-1 notes-md"), "click", { detail: 1 }).handled).toBe(
          true
        );
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
 *  the block's content. Like Dispatch's node view of it (`typedBlockView`), the drawing has no
 *  `stopEvent`, so a press on the pointer reaches ProseMirror. */
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
// sit two elements below a header that is `contenteditable="false"`, in a block with no
// `stopEvent`, so a press on one reaches ProseMirror, which opens a pointer click on it through
// `handleClick` as it does a link mark's; Enter's click, Ctrl and a middle click open it as they
// open any link.
test("a pointer in a typed block's header opens its reference once on a pointer click and once on Enter's, and Ctrl and a middle click open it once in a new tab", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    PLAN_TASK,
    ({ view }) => {
      const pointer = view.dom.querySelector(
        "header[contenteditable=false] > dl > dd > a[data-dispatch-href]"
      );
      if (pointer === null) throw new Error("the plan's task drew no pointer in its header");

      // A pointer's click: ProseMirror reads its press and release as a click, and `handleClick`
      // takes the release, whose default ProseMirror then prevents.
      press(view, pointer, [20, 10]);
      const released = release(pointer, [20, 10]).defaultPrevented;
      const clicked = click(pointer, { clientX: 20, clientY: 10, detail: 1 });
      const entered = keyboardClick(pointer);
      const ctrlPress = press(view, pointer, [60, 10], { ctrlKey: true }).defaultPrevented;
      release(pointer, [60, 10], { ctrlKey: true });
      const ctrl = click(pointer, { clientX: 60, clientY: 10, ctrlKey: true, detail: 1 });
      const middle = middleClick(pointer);

      expect({
        middle,
        opened,
        prevented: { clicked, ctrl, ctrlPress, entered, released },
      }).toEqual({
        middle: { auxclick: true, press: true, release: true },
        opened: [
          ["dispatch://CORE-1/spec#b-r1", false],
          ["dispatch://CORE-1/spec#b-r1", false],
          ["dispatch://CORE-1/spec#b-r1", true],
          ["dispatch://CORE-1/spec#b-r1", true],
        ],
        prevented: { clicked: true, ctrl: true, ctrlPress: true, entered: true, released: true },
      });

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
        expect(offer(view, linkOf(view, "in a thread"), "click", { detail: 1 }).handled).toBe(
          false
        );
        expect(opened).toHaveLength(4);
      } finally {
        island.remove();
      }
    },
    { blockSchema: serverBlockSchema, openLink, renderBlock: drawTypedBlock }
  );
});

// dispatch://INFRA-2160, R1: a title prevents its own press, and its widget stops every event, so
// its press never reaches ProseMirror and the click opens it, on a pointer's click and on Enter's
// alike. A drag across the title is a click by the platform's rule (its press and its release land
// on the title, so the browser sends a click), and opens it too: measured in Chromium and Firefox
// in builds Job infra-2160-title-drag-t5nlk at b1ede4f7.
test("a drawn title opens its reference once on a pointer click, once on a drag across it and once on Enter's click, and Ctrl and a middle click open it once in a new tab", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    "See CORE-1 here.",
    ({ handle, view }) => {
      handle.setReferences({
        mentions: new Map(),
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

      const titlePress = press(view, title, [20, 10]).defaultPrevented;
      release(title, [20, 10]);
      const clicked = click(title, { clientX: 20, clientY: 10, detail: 1 });
      const entered = keyboardClick(title);
      const ctrl = pointerClick(view, title, { at: [60, 10], keys: { ctrlKey: true } });
      const dragged = pointerClick(view, title, { at: [100, 10], through: [[130, 10]] });
      const middle = middleClick(title);

      expect({
        middle,
        opened,
        prevented: { clicked, ctrl, dragged, entered, titlePress },
      }).toEqual({
        middle: { auxclick: true, press: true, release: true },
        opened: [
          ["dispatch://CORE-1", false],
          ["dispatch://CORE-1", false],
          ["dispatch://CORE-1", true],
          ["dispatch://CORE-1", false],
          ["dispatch://CORE-1", true],
        ],
        prevented: { clicked: true, ctrl: true, dragged: true, entered: true, titlePress: true },
      });
    },
    { openLink }
  );
});

/** Draws the document's pictures as Dispatch's `picture-view.ts` draws one the browser cannot
 *  draw: a leaf's root (which ProseMirror makes `contenteditable="false"`) holding a link to the
 *  picture's artifact, whose middle press is prevented, and a `stopEvent` that keeps every event
 *  on the link but a `dragstart` from the editor's own handlers. Dispatch's link also puts the
 *  caret after the picture on a primary press, which decides nothing here. */
function drawPictureFallback(view: EditorView): void {
  view.setProps({
    nodeViews: {
      ...view.props.nodeViews,
      image: () => {
        const document = view.dom.ownerDocument;
        const dom = document.createElement("span");
        const link = document.createElement("a");
        link.setAttribute("href", "/issues/CORE-1/artifacts/shot-png");
        link.setAttribute("data-dispatch-href", "dispatch://CORE-1/artifact/shot-png");
        link.textContent = "CORE-1 shot-png";
        link.addEventListener("mousedown", (event) => {
          if (event.button === 1) event.preventDefault();
        });
        dom.append(link);
        return {
          dom,
          stopEvent: (event: Event) =>
            event.type !== "dragstart" &&
            event.target instanceof window.Node &&
            link.contains(event.target as Node),
        };
      },
    },
  });
}

// dispatch://INFRA-2160, R1: the picture fallback's node view stops every press on its link, so
// the press never reaches ProseMirror and the click opens it, on a pointer's click and on
// Enter's alike.
test("the picture fallback's link opens once on a pointer click and once on Enter's, and Ctrl and a middle click open it once in a new tab", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    "See ![shot](dispatch://CORE-1/artifact/shot-png) here.",
    ({ view }) => {
      drawPictureFallback(view);
      const link = linkOf(view, "CORE-1 shot-png");
      expect(link.parentElement?.getAttribute("contenteditable")).toBe("false");

      const prevented = {
        clicked: pointerClick(view, link, { at: [20, 10] }),
        ctrl: pointerClick(view, link, { at: [60, 10], keys: { ctrlKey: true } }),
        entered: keyboardClick(link),
      };
      const middle = middleClick(link);

      expect({ middle, opened, prevented }).toEqual({
        middle: { auxclick: true, press: true, release: true },
        opened: [
          ["dispatch://CORE-1/artifact/shot-png", false],
          ["dispatch://CORE-1/artifact/shot-png", true],
          ["dispatch://CORE-1/artifact/shot-png", false],
          ["dispatch://CORE-1/artifact/shot-png", true],
        ],
        prevented: { clicked: true, ctrl: true, entered: true },
      });
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
        mentions: new Map(),
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
        mentions: new Map(),
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

// dispatch://INFRA-2160, R1: ProseMirror starts its press in a view the reader cannot edit too
// (prosemirror-view's `initInput` holds back only its `editHandlers` there, and `mousedown` is
// not one), so a plain pointer click there opens a link once through `handleClick`.
test("a view the reader cannot edit leaves Alt+Enter alone, and opens a link once on a plain pointer click all the same", async () => {
  const { openLink, opened } = recordedOpener();
  await withMarksEditor(
    LINKS,
    ({ view }) => {
      expect(view.editable).toBe(false);
      caretAt(view, 8);
      expect(altEnter(view).handled).toBe(false);

      const away = linkOf(view, "the plan");
      press(view, away, [20, 10]);
      const released = release(away, [20, 10]).defaultPrevented;
      const clicked = click(away, { clientX: 20, clientY: 10, detail: 1 });

      expect({ opened, prevented: { clicked, released } }).toEqual({
        opened: [["https://example.com/a", false]],
        prevented: { clicked: true, released: true },
      });
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
