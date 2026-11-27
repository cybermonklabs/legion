import { expect, jest, spyOn, test } from "bun:test";
import { onlineManager } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: an armed press is live only until a frame and a task after it, and user-event waits a task after every press it makes, so the click the armed cases make and the commit after it must come in the same task, which user-event has no form for; nor has it one for a key marked as an input method's, at keyCode 229.
import { act, fireEvent, render, renderHook } from "@testing-library/react";
import { Component, createElement, Fragment, type ReactNode, useRef, useState } from "react";
import { createPortal } from "react-dom";

import { disarm } from "../__tests__/armed-press";
import { realClock, tick } from "../__tests__/fake-clock";
import { frameAndTasks, nextFrame } from "../__tests__/frames";
import { NETWORK_ERROR_MESSAGE, REQUEST_DEADLINE_MS } from "../api/client";
import { useReadBound } from "../api/read-bound";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { PANEL_ATTRIBUTE, useKeymap } from "../features/shell/keymap";
import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { CONTROL } from "./control-names";
import {
  ARM_ON_PRESS,
  actionWriteBegins,
  armOnPress,
  causedByReader,
  focusedOutOfSight,
  holdHandOff,
  landNearest,
  pressArmed,
  pressIsArmedOwn,
  pressOfWrite,
  repeatTookFocusFrom,
  useFocusPlace,
} from "./focus-place";
import { useHeldThroughAnswer } from "./held-press";

// The page's focus hand-offs, read in happy-dom with the page built by hand: a component's own
// hand-off (`useFocusPlace`), in a line of buttons React draws (`HandOffLine`, `ArmedLine`) or a
// list (`RecipientsList`, `ArmedRows`), armed by the reader's press at its root (`armOnPress`).
// A focused element taken away fires a `focusout` in Chromium while it is still in the page, and
// none at all in Firefox (measured, INFRA-3135 round 7) nor in happy-dom: `lose` fires it as
// Chromium does before the element goes, and a bare `remove()` is the Firefox case. A `blur()`
// fires its own.

/** The page's main content holding one button per label, in the document. */
function mainOf(...labels: string[]): HTMLElement {
  const main = document.createElement("main");
  main.id = "main-content";
  for (const label of labels) buttonIn(main, label);
  document.body.append(main);
  return main;
}

/** Fires the `focusout` Chromium fires as `element`, which holds the focus, is taken away, and
 *  then takes it away with `leave`. */
function lose(element: HTMLElement, leave: () => void): void {
  element.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
  leave();
}

/** Runs `body` with every `scrollIntoView` watched, and expects none of them to have run. */
async function scrollingNothing(body: () => Promise<void>): Promise<void> {
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await body();
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
  }
}

/** The reader's Enter on `element`: its `keydown`, which arms a component's hand-off at its root
 *  (`armOnPress`). */
function enterPress(element: Element): void {
  element.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Enter" }));
}

/** A button labelled `label`, put at the end of `parent`. */
function buttonIn(parent: Element, label: string): HTMLButtonElement {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = label;
  parent.append(button);
  return button;
}

/** Lays `element` out `top` px down the window and 40 px tall: where a scroll left it, which
 *  happy-dom, laying nothing out, does not say. */
function placeAt(element: Element, top: number): void {
  element.getBoundingClientRect = () => new DOMRect(0, top, 200, 40);
}

// (bd) A live update withdraws the question whose answer form holds the focus and draws a Reply
// form where that card stood, its text box first: a loss the reader did not cause, with nothing
// armed. Nothing scrolls, and the page's keys answer after it, as they did before the page had any
// hand-off: `j` pressed where the loss left the focus runs the page's binding. By Chromium's
// `focusout` and by none.
test("an uncaused loss whose place now holds a form scrolls nothing, and the page's keys answer after it", async () => {
  for (const withFocusout of [true, false]) {
    await scrollingNothing(async () => {
      const walked: string[] = [];
      const next = { id: "next", keys: "j", label: "Next row", run: () => walked.push("j") };
      const keys = renderHook(() => useKeymap("global", [next]), { wrapper: KeymapProvider });
      const main = mainOf("Pin");
      try {
        const asked = document.createElement("article");
        const answer = document.createElement("form");
        const choice = document.createElement("input");
        choice.type = "radio";
        choice.name = "choice";
        answer.append(choice);
        buttonIn(answer, "Send answer");
        asked.append(answer);
        main.append(asked);
        const withdrawn = document.createElement("article");
        const reply = document.createElement("form");
        const box = document.createElement("textarea");
        box.setAttribute("aria-label", "Reply");
        reply.append(box);
        buttonIn(reply, "Send");
        withdrawn.append(reply);
        choice.focus();
        // The update takes the asked card away and draws the withdrawn one where it stood.
        const replace = () => {
          asked.remove();
          main.append(withdrawn);
        };
        if (withFocusout) lose(choice, replace);
        else replace();
        await frameAndTasks();
        const landed = document.activeElement as HTMLElement;
        landed.dispatchEvent(
          new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: "j" })
        );
        expect(walked).toEqual(["j"]);
      } finally {
        keys.unmount();
        main.remove();
      }
    });
  }
});

/** A control of `kind`: a `<select>`, else an `<input>` of that type. */
function controlOf(kind: string): HTMLElement {
  if (kind === "select") return document.createElement(kind);
  const input = document.createElement("input");
  input.type = kind;
  return input;
}

/** The name of the element holding the focus: its `aria-label`, else its text. */
function focusedName(): string | null {
  const focused = document.activeElement;
  return focused === null ? null : nameOf(focused);
}

/** A line of buttons, one per label, that hands its own focus off (`useFocusPlace`) and arms
 *  that hand-off at its own presses (`armOnPress`), as a component does at its root: each button
 *  is counted in the line, and the line falls back to what `fallback` names. Drawn again with
 *  fewer labels, it takes the others away, as a press that takes its own control away does. */
function HandOffLine(props: {
  fallback: () => HTMLElement | null;
  labels: readonly string[];
}): ReactNode {
  const line = useRef<HTMLDivElement>(null);
  const noteFocus = useFocusPlace(
    (element) => (line.current?.contains(element) ? line.current : null),
    props.fallback
  );
  return createElement(
    "div",
    { ...ARM_ON_PRESS, onFocus: noteFocus, ref: line },
    props.labels.map((label) => createElement("button", { key: label, type: "button" }, label))
  );
}

/** One move a hand-off asks of the page: a focus, or a scroll into view, of the element it names
 *  (`nameOf`), with the options it asks with. */
interface Move {
  readonly move: "focus" | "scroll";
  readonly of: string | null;
  readonly options: unknown;
}

/** The name a case reads an element by: its `aria-label`, else its text. */
function nameOf(element: Element): string | null {
  return element.getAttribute("aria-label") ?? element.textContent;
}

/** Spies on every focus and every scroll into view the page is asked for, in order, until
 *  `restore`. A focus is made as asked; a scroll is only noted, as happy-dom scrolls nothing. */
function recordMoves(): { moves: Move[]; restore: () => void } {
  const moves: Move[] = [];
  const realFocus = HTMLElement.prototype.focus;
  const focus = spyOn(HTMLElement.prototype, "focus").mockImplementation(function (
    this: HTMLElement,
    options?: FocusOptions
  ) {
    moves.push({ move: "focus", of: nameOf(this), options });
    realFocus.call(this, options);
  });
  const scroll = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element,
    options?: boolean | ScrollIntoViewOptions
  ) {
    moves.push({ move: "scroll", of: nameOf(this), options });
  });
  return {
    moves,
    restore: () => {
      focus.mockRestore();
      scroll.mockRestore();
    },
  };
}

/** A press in a `HandOffLine` that takes its own button away. */
interface HandOffCase {
  /** The control the line falls back to, else none. */
  readonly fallback: HTMLElement | null;
  /** The line's buttons, by label, in order. */
  readonly labels: readonly string[];
  /** How far down the window each button named here is laid out, in px (`placeAt`). A button not
   *  named keeps happy-dom's own box, which reads as in sight. */
  readonly laidOut: Readonly<Record<string, number>>;
  /** The element the line is drawn at the end of. */
  readonly parent: Element;
  /** The button the press takes away. */
  readonly pressed: string;
}

/** Draws `handed`'s line, lays its buttons out, focuses the pressed one and clicks it, and takes
 *  it away, as the press's own commit does. Answers where the focus landed - the page, or the
 *  element by its name (`nameOf`) - and every move the hand-off asked of the page, in order
 *  (`recordMoves`). The click arms the line's hand-off (`armOnPress`), so the loss reads as the
 *  reader's (`causedByReader`). */
function handOff(handed: HandOffCase): { landed: string | null; moves: Move[] } {
  const host = document.createElement("div");
  handed.parent.append(host);
  const fallback = () => handed.fallback;
  const view = render(createElement(HandOffLine, { fallback, labels: handed.labels }), {
    container: host,
  });
  try {
    for (const button of host.querySelectorAll("button")) {
      const top = handed.laidOut[button.textContent ?? ""];
      if (top !== undefined) placeAt(button, top);
    }
    const pressed = [...host.querySelectorAll("button")].find(
      (button) => button.textContent === handed.pressed
    );
    pressed?.focus();
    // The click and the commit after it come in one task, before the armed press ends; user-event
    // waits a task after every press.
    if (pressed !== undefined) fireEvent.click(pressed);
    const recorded = recordMoves();
    try {
      const left = handed.labels.filter((label) => label !== handed.pressed);
      view.rerender(createElement(HandOffLine, { fallback, labels: left }));
    } finally {
      recorded.restore();
    }
    return { landed: landing(), moves: recorded.moves };
  } finally {
    disarm();
    view.unmount();
    host.remove();
  }
}

/** A `<select>` named Window at the end of `parent`, laid out `top` px down the window: the
 *  control a line falls back to, as the Delivery page's header falls back to its Window list's
 *  trigger. */
function windowSelectIn(parent: Element, top: number): HTMLElement {
  const select = controlOf("select");
  select.setAttribute("aria-label", "Window");
  parent.append(select);
  placeAt(select, top);
  return select;
}

/** Runs `body` with the page's main content in the document, named Main content, and takes it
 *  away after. */
function withMain<T>(body: (main: HTMLElement) => T): T {
  const main = mainOf();
  main.setAttribute("aria-label", "Main content");
  try {
    return body(main);
  } finally {
    main.remove();
  }
}

/** The scroll `land` asks for: to the nearest edge, the least distance that shows the control
 *  whole, which moves nothing for a control wholly in sight. */
const NEAREST_EDGE = { block: "nearest", inline: "nearest" };

// (cd) A component's own hand-off (`useFocusPlace`) passes over every control out of the reader's
// sight: the control now at the lost one's place, under the window's bottom edge, and the one
// before the lost place, above the window's top edge. The nearest control in sight before them
// takes the focus. Where none of the line is in sight, the line's fallback, in sight, takes it.
test("a component's hand-off passes over its controls out of sight to the nearest in sight, else to its fallback", () => {
  // Where each hand-off landed, and where it belongs.
  const landed: Record<string, string | null> = {};
  const belongs: Record<string, string> = {};
  for (const oneShown of [true, false]) {
    const named = oneShown ? "One in sight" : "none of the line in sight";
    landed[named] = withMain(
      (main) =>
        handOff({
          fallback: windowSelectIn(main, 100),
          labels: ["One", "Two", "Three", "Four"],
          laidOut: { Four: 2_000, One: oneShown ? 300 : -500, Two: -100 },
          parent: main,
          pressed: "Three",
        }).landed
    );
    belongs[named] = oneShown ? "One" : "Window";
  }
  expect(landed).toEqual(belongs);
});

// (ce) Where nothing a component's hand-off can land on is in the reader's sight - the line's own
// control before the lost one and the line's fallback, each scrolled out above the window, as the
// Delivery page's Window list's trigger is for a reader scrolled below it - the focus goes, with
// no scroll, to the own stop of the region the line is in: the dialog or panel holding it, else the
// main content around it, else the page's main content for a line drawn outside it. With no main
// content in the page, nothing moves, and the focus stays on the page.
test("a component's hand-off with nothing in sight lands on its region's own stop, scrolling nothing", () => {
  // Where each hand-off landed and what it asked of the page, and where it belongs.
  const landed: Record<string, object> = {};
  const belongs: Record<string, object> = {};
  const ownStop = (name: string) => ({
    landed: name,
    moves: [{ move: "focus", of: name, options: { preventScroll: true } }],
  });
  /** The line's case, drawn at the end of `parent`, falling back to a select in `page`. */
  const lineIn = (parent: Element, page: Element) =>
    handOff({
      fallback: windowSelectIn(page, -100),
      labels: ["Before", "Clear"],
      laidOut: { Before: -200 },
      parent,
      pressed: "Clear",
    });
  landed["in a panel"] = withMain((main) => {
    const panel = document.createElement("div");
    panel.setAttribute("role", "dialog");
    panel.setAttribute("aria-label", "Filter");
    document.body.append(panel);
    try {
      return lineIn(panel, main);
    } finally {
      panel.remove();
    }
  });
  belongs["in a panel"] = ownStop("Filter");
  landed["in the main content"] = withMain((main) => lineIn(main, main));
  belongs["in the main content"] = ownStop("Main content");
  landed["outside the main content"] = withMain((main) => {
    const portal = document.createElement("div");
    document.body.append(portal);
    try {
      return lineIn(portal, main);
    } finally {
      portal.remove();
    }
  });
  belongs["outside the main content"] = ownStop("Main content");
  const page = document.createElement("div");
  document.body.append(page);
  try {
    landed["in a page with no main content"] = lineIn(page, page);
  } finally {
    page.remove();
  }
  belongs["in a page with no main content"] = { landed: "the page", moves: [] };
  expect(landed).toEqual(belongs);
});

// (cf) A control any part of which is in the reader's sight is one a component's hand-off lands
// on (`land`): focused with no scroll of the engine's own, then scrolled to its nearest edge, the
// least distance that shows it whole. The control before the lost one, cut by the window's top
// edge or by its bottom edge, takes the focus and is scrolled by that edge alone. The focus asks
// for `preventScroll`, so the engine's own rule for a focused control - Chromium's and Firefox's
// centring - moves nothing, and every engine shows the landing the same way.
test("a component's hand-off lands on a control partly in sight and scrolls it to its nearest edge", () => {
  // What each hand-off asked of the page, and what it belongs to ask.
  const moved: Record<string, Move[]> = {};
  const belongs: Record<string, Move[]> = {};
  for (const [edge, top] of [
    ["top", -20],
    ["bottom", window.innerHeight - 20],
  ] as const) {
    const named = `cut by the window's ${edge} edge`;
    moved[named] = withMain(
      (main) =>
        handOff({
          fallback: windowSelectIn(main, 100),
          labels: ["One", "Two"],
          laidOut: { One: top },
          parent: main,
          pressed: "Two",
        }).moves
    );
    belongs[named] = [
      { move: "focus", of: "One", options: { preventScroll: true } },
      { move: "scroll", of: "One", options: NEAREST_EDGE },
    ];
  }
  expect(moved).toEqual(belongs);
});

// (cg) A control wholly in the reader's sight takes the focus where a component's hand-off has
// always put it: the control before the lost one, else, where the line holds no other control, the
// line's fallback, as the Delivery page's Window list's trigger takes it from clear brush window.
// It asks for no scroll at all: the focus is taken with `preventScroll`, and a control wholly in
// sight needs no scroll to its nearest edge.
test("a component's hand-off lands on a control wholly in sight where it always did, and scrolls nothing", () => {
  // Where each hand-off landed and what it asked of the page, and where it belongs.
  const landed: Record<string, object> = {};
  const belongs: Record<string, object> = {};
  for (const [named, labels, target] of [
    ["the control before", ["One", "Two"], "One"],
    ["the fallback", ["Clear"], "Window"],
  ] as const) {
    landed[named] = withMain((main) =>
      handOff({
        fallback: windowSelectIn(main, 100),
        labels,
        laidOut: { One: 300 },
        parent: main,
        pressed: labels[labels.length - 1] ?? "",
      })
    );
    belongs[named] = {
      landed: target,
      moves: [{ move: "focus", of: target, options: { preventScroll: true } }],
    };
  }
  expect(landed).toEqual(belongs);
});

/** What the Inbox list's `ViewportAnchor` does to the page in a commit that changes nothing above
 *  the row it holds: it reads where the page stands before the commit (`getSnapshotBeforeUpdate`)
 *  and puts the page back there from `componentDidUpdate`, in the layout phase, after the layout
 *  effects of every component drawn before it, a component's hand-off among them. `page.y` is how
 *  far down the page the window stands, as the case lays it out. */
class KeepsPlace extends Component<{ readonly page: { y: number } }, object, number> {
  getSnapshotBeforeUpdate(): number {
    return this.props.page.y;
  }

  componentDidUpdate(
    _props: Readonly<{ readonly page: { y: number } }>,
    _state: Readonly<object>,
    snapshot?: number
  ): void {
    if (snapshot !== undefined) this.props.page.y = snapshot;
  }

  render(): ReactNode {
    return null;
  }
}

// (ch) A component's hand-off lands in a layout effect, and a layout effect after it in the same
// commit can scroll the page back under the landing: the Inbox list's `ViewportAnchor` puts the
// row it holds back where the commit found it, from its `componentDidUpdate`. At 390 px, Enter on
// Mark all read scrolled out of sight left Blocked on you, its landing, with 4 of its 44 px in
// view in Chromium and Firefox. A landing partly in sight - 4 of its 44 px below the window's top
// edge - is scrolled to its nearest edge once that commit is done, and stands whole in the window;
// a landing wholly in sight scrolls nothing, then or after.
test("a landing a later layout effect of its commit scrolls back partly out of sight is scrolled to its nearest edge once the commit is done, and one wholly in sight scrolls nothing", async () => {
  const landed: Record<string, object> = {};
  for (const [named, top] of [
    ["partly in sight", -40],
    ["wholly in sight", 100],
  ] as const) {
    const page = { y: 1_000 };
    // A scroll into view moves the window by the least that shows the element whole, as
    // `{ block: "nearest" }` does.
    const scrolls = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
      this: Element
    ) {
      const box = this.getBoundingClientRect();
      if (box.top < 0) page.y += box.top;
      else if (box.bottom > window.innerHeight) page.y += box.bottom - window.innerHeight;
    });
    try {
      landed[named] = await inMain([], async (main) => {
        const host = document.createElement("div");
        main.append(host);
        const draw = (labels: readonly string[]) =>
          createElement(
            Fragment,
            null,
            createElement(HandOffLine, { fallback: () => null, labels }),
            createElement(KeepsPlace, { page })
          );
        const view = render(draw(["One", "Two"]), { container: host });
        try {
          const [one, two] = [...host.querySelectorAll("button")];
          if (one === undefined || two === undefined) throw new Error("the line drew no buttons");
          // One stands `top` px down the window while it is scrolled to 1,000 px, and 44 px tall.
          one.getBoundingClientRect = () => new DOMRect(0, 1_000 + top - page.y, 200, 44);
          clickPress(two);
          view.rerender(draw(["One"]));
          // The commit is done.
          await Promise.resolve();
          return {
            landed: landing(),
            scrolled: scrolls.mock.calls.length > 0,
            top: one.getBoundingClientRect().top,
          };
        } finally {
          view.unmount();
          host.remove();
        }
      });
    } finally {
      scrolls.mockRestore();
    }
  }
  expect(landed).toEqual({
    "partly in sight": { landed: "One", scrolled: true, top: 0 },
    "wholly in sight": { landed: "One", scrolled: false, top: 100 },
  });
});

// A component's own hand-off lands on no field that takes typed text, its fallback included: the
// reader never chose it, and their next key would be typed into it. A line whose only control a
// press takes away falls back to a search box, as a selection header's Clear selection with no
// row matching and its select-all checkbox disabled has nothing else before it. The hand-off
// passes over that fallback, and, with no control of the line left, walks back itself over the
// main content (`walkBackInRegion`), to the Filter button before the line, which takes no text.
// With Chromium's `focusout` and without.
test("a component's hand-off passes over a fallback that takes typed text, and walks back to the control before the line", async () => {
  const landed: Record<string, string | null> = {};
  const belongs: Record<string, string> = {};
  for (const withFocusout of [true, false]) {
    const named = withFocusout ? "with a focusout" : "without a focusout";
    const main = mainOf();
    const search = controlOf("search");
    search.setAttribute("aria-label", "Search agents");
    main.append(search);
    buttonIn(main, "Filter");
    const host = document.createElement("div");
    main.append(host);
    const fallback = () => search;
    const view = render(createElement(HandOffLine, { fallback, labels: ["Clear selection"] }), {
      container: host,
    });
    try {
      const clear = host.querySelector("button") as HTMLButtonElement;
      clear.focus();
      enterPress(clear);
      const goes = () => view.rerender(createElement(HandOffLine, { fallback, labels: [] }));
      if (withFocusout) lose(clear, goes);
      else goes();
      await frameAndTasks();
      landed[named] = focusedName();
    } finally {
      view.unmount();
      main.remove();
    }
    belongs[named] = "Filter";
  }
  expect(landed).toEqual(belongs);
});

/** An Inbox card's recipients, drawn by React inside a list that hands its own focus off
 *  (`useFocusPlace`), the list its container, and arms that hand-off at its own presses
 *  (`armOnPress`), as `HandOffLine` does: the card's follower chip (the name link Replier, then
 *  Unfollow) and its subscriber chip (Watcher, then Unsubscribe), then the next question's Select
 *  checkbox. Drawn again without `subscribers`, it takes the subscribers' list away with the name
 *  link and the Unsubscribe. */
function RecipientsList(props: { readonly subscribers: boolean }): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  const noteFocus = useFocusPlace(
    (element) => (list.current?.contains(element) ? list.current : null),
    () => null
  );
  // A list of one chip: the session's name link, then its remove button.
  const chipList = (name: string, label: string) =>
    createElement(
      "ul",
      { key: name },
      createElement(
        "li",
        null,
        createElement("a", { href: `/agents/${name}` }, name),
        createElement("button", { type: "button" }, label)
      )
    );
  return createElement(
    "ul",
    { ...ARM_ON_PRESS, onFocus: noteFocus, ref: list },
    createElement(
      "li",
      null,
      createElement(
        "article",
        null,
        chipList("Replier", "Unfollow"),
        props.subscribers ? chipList("Watcher", "Unsubscribe") : null
      )
    ),
    createElement(
      "li",
      null,
      createElement("input", { "aria-label": "Select CORE-3", type: "checkbox" })
    )
  );
}

// (db) A component's own hand-off counts the place of a control taken away off the spot it went
// from in its container (`useFocusPlace`'s `placeLeft`). The reader's Enter on the card's
// Unsubscribe arms the list's hand-off, and the write's answer takes the subscribers' list away
// with the name link before the Unsubscribe. Counted while the Unsubscribe still stood there, the
// place takes in the name link that went with it, and the walk back would start at the next
// question's Select checkbox; counted off the spot, the focus lands on the card's Unfollow. With
// Chromium's `focusout` and without: the hand-off answers in the commit that takes the list away.
test("a component's hand-off for a control taken away with a control before it lands on the card's control before the place", async () => {
  const landed: Record<string, string | null> = {};
  for (const withFocusout of [true, false]) {
    const main = mainOf();
    const view = render(createElement(RecipientsList, { subscribers: true }), {
      container: main,
    });
    try {
      const unsubscribe = view.getByRole("button", { name: "Unsubscribe" });
      unsubscribe.focus();
      enterPress(unsubscribe);
      // The write lands, and the commit takes the subscribers' list away.
      const answer = () => view.rerender(createElement(RecipientsList, { subscribers: false }));
      if (withFocusout) lose(unsubscribe, answer);
      else answer();
      await frameAndTasks();
      landed[withFocusout ? "with Chromium's focusout" : "without"] = focusedName();
    } finally {
      disarm();
      view.unmount();
      main.remove();
    }
  }
  expect(landed).toEqual({ "with Chromium's focusout": "Unfollow", without: "Unfollow" });
});

// The armed hand-off. A component's own hand-off (`useFocusPlace`, `SelectionBar`) answers a loss
// as the reader's only while a press that armed it is still live (`armHandOff`, armed at a
// component's root by `armOnPress`), and leaves every other loss as the page did before it had any
// hand-off: the focus on the body, and nothing scrolled. Each case is one row of the state table
// of INFRA-3135's t31: the state the press left, the event, and where the focus goes.

/** What an `ArmedLine` draws: one button per label, `held` ones held while their write is out
 *  (`aria-disabled`, as `heldPress` holds one), `disabled` ones disabled, after a disabled
 *  checkbox where `box` asks for one - the Agents page's select-all while no row matches - and
 *  the line itself only while `shown` (by default). With `list`, a list one of its buttons opened
 *  is drawn as well, outside the line in a portal on the page's body, holding the text box named
 *  Search, as the shared list draws its own (`MultiSelect`). `fallback` names the control the line
 *  falls back to (`useFocusPlace`), else none. */
interface ArmedLineProps {
  readonly box?: boolean;
  readonly disabled?: readonly string[];
  readonly fallback?: () => HTMLElement | null;
  readonly held?: readonly string[];
  readonly labels: readonly string[];
  readonly list?: boolean;
  readonly shown?: boolean;
}

/** A component that hands its own focus off (`useFocusPlace`) and arms that hand-off at its own
 *  presses (`armOnPress`), as every component with a hand-off does at its root: a line of
 *  buttons, the container they are counted in, inside a root that stays when the line goes. */
function ArmedLine(props: ArmedLineProps): ReactNode {
  const line = useRef<HTMLDivElement>(null);
  const noteFocus = useFocusPlace(
    (element) => (line.current?.contains(element) ? line.current : null),
    () => props.fallback?.() ?? null
  );
  const { box = false, disabled = [], held = [], labels, list = false, shown = true } = props;
  const buttons = labels.map((label) =>
    createElement(
      "button",
      {
        "aria-disabled": held.includes(label) || undefined,
        disabled: disabled.includes(label),
        key: label,
        type: "button",
      },
      label
    )
  );
  return createElement(
    "div",
    { ...ARM_ON_PRESS, onFocus: noteFocus },
    shown
      ? createElement(
          "div",
          { ref: line },
          box
            ? createElement("input", {
                "aria-label": "Select all",
                disabled: true,
                type: "checkbox",
              })
            : null,
          buttons
        )
      : null,
    list
      ? createPortal(
          createElement("input", { "aria-label": "Search", type: "text" }),
          document.body
        )
      : null
  );
}

/** An `ArmedLine` drawn at the end of `parent`: its button named `label`, a redraw with new
 *  props - the commit a press, a write's answer or an update makes - and its removal. */
function armedLineIn(
  parent: Element,
  props: ArmedLineProps
): {
  button: (label: string) => HTMLButtonElement;
  redraw: (next: ArmedLineProps) => void;
  remove: () => void;
} {
  const host = document.createElement("div");
  parent.append(host);
  const view = render(createElement(ArmedLine, props), { container: host });
  return {
    button: (label) => {
      const found = [...host.querySelectorAll("button")].find(
        (button) => button.textContent === label
      );
      if (found === undefined) throw new Error(`no button ${label} is drawn`);
      return found;
    },
    redraw: (next) => view.rerender(createElement(ArmedLine, next)),
    remove: () => {
      view.unmount();
      host.remove();
    },
  };
}

/** The reader's click on `control`, through the event path a hand-off's root arms at
 *  (`armOnPress`): the focus a pointer's press gives a button in Chromium and Firefox, then the
 *  click itself. */
function clickPress(control: HTMLElement): void {
  control.focus();
  fireEvent.click(control);
}

/** Runs `body` with the page's main content in the document, holding one button per label, and
 *  takes it away after, with any press still armed (`disarm`). */
async function inMain<T>(
  labels: readonly string[],
  body: (main: HTMLElement) => Promise<T>
): Promise<T> {
  const main = mainOf(...labels);
  try {
    return await body(main);
  } finally {
    disarm();
    main.remove();
  }
}

/** Where the focus is: the page, or the element by its name (`nameOf`). */
function landing(): string | null {
  const focused = document.activeElement;
  return focused === null || focused === document.body ? "the page" : nameOf(focused);
}

/** The labels every line below starts with. */
const LINE = ["One", "Two", "Three"] as const;

// (de) Armed by a control's own press, the commit of that press takes the focused control away:
// the focus goes to the control now at its place, which is in sight, and nothing scrolls.
test("an armed press whose commit takes its control away hands the focus to the control now at its place, scrolling nothing", async () => {
  const moved = await inMain([], async (main) => {
    const line = armedLineIn(main, { labels: LINE });
    try {
      clickPress(line.button("Two"));
      const recorded = recordMoves();
      try {
        line.redraw({ labels: ["One", "Three"] });
      } finally {
        recorded.restore();
      }
      return { landed: landing(), moves: recorded.moves };
    } finally {
      line.remove();
    }
  });
  expect(moved).toEqual({
    landed: "Three",
    moves: [{ move: "focus", of: "Three", options: { preventScroll: true } }],
  });
});

// (df) Armed, and the press's commit takes its whole container away - Clear filters with the
// chips line, Clear selection with the selection it empties - or leaves it no control the focus
// can reach - the Agents page's header, whose select-all is disabled while no row matches. The
// component's hand-off walks back itself over the region its container was in, the main content,
// from the place the control left: past the search box, which takes typed text, to the nearest
// control before it in sight, the Filter button.
test("an armed press whose commit leaves its container nothing walks back over the main content to the nearest control in sight", async () => {
  const landed: Record<string, string | null> = {};
  for (const way of ["the line goes", "the line keeps only a disabled box"] as const) {
    landed[way] = await inMain(["Filter"], async (main) => {
      const search = controlOf("search");
      search.setAttribute("aria-label", "Search agents");
      main.append(search);
      const kept = way !== "the line goes";
      const line = armedLineIn(main, { box: kept, labels: ["Clear selection"] });
      try {
        clickPress(line.button("Clear selection"));
        line.redraw(kept ? { box: true, labels: [] } : { labels: [], shown: false });
        return landing();
      } finally {
        line.remove();
      }
    });
  }
  expect(landed).toEqual({
    "the line goes": "Filter",
    "the line keeps only a disabled box": "Filter",
  });
});

// (dg) Armed, and the press's write held by its hold: the control held while the write is out
// (`heldPress`), or a hold on what answers it (`holdHandOff`). The write lands several frames
// later and its answer takes the control away: the focus goes to the control now at its place.
test("an armed press whose write is held hands the focus on when the write lands and takes its control away", async () => {
  const landed: Record<string, string | null> = {};
  for (const way of ["its control held", "a hold on what answers it"] as const) {
    landed[way] = await inMain([], async (main) => {
      const line = armedLineIn(main, { labels: LINE });
      try {
        clickPress(line.button("Two"));
        let answered: () => void = () => undefined;
        if (way === "its control held") line.redraw({ held: ["Two"], labels: LINE });
        else answered = holdHandOff();
        for (const _ of [0, 1, 2]) await frameAndTasks();
        line.redraw({ labels: ["One", "Three"] });
        answered();
        return landing();
      } finally {
        line.remove();
      }
    });
  }
  expect(landed).toEqual({ "a hold on what answers it": "Three", "its control held": "Three" });
});

// (dh) Armed, offline, and the press's write held by its hold: the write landed, and its control
// is held through the re-read after it (`useReadBound`), which a paused fetch never answers. The
// hold ends at the request deadline with the network's reason, and the commit that lets the
// control go takes it away: the focus goes to the control now at its place, in that same render.
test("an armed press held offline hands the focus on in the render its hold's end at the request deadline takes its control away", async () => {
  jest.useFakeTimers();
  const online = onlineManager.isOnline();
  const bound = renderHook(() => useReadBound());
  try {
    act(() => onlineManager.setOnline(false));
    const landed = await inMain([], async (main) => {
      const line = armedLineIn(main, { labels: LINE });
      try {
        clickPress(line.button("Two"));
        const press = pressOfWrite();
        line.redraw({ held: ["Two"], labels: LINE });
        const held = bound.result.current[1](new Promise<never>(() => undefined), press);
        await act(async () => {
          jest.advanceTimersByTime(REQUEST_DEADLINE_MS);
          await held;
        });
        line.redraw({ labels: ["One", "Three"] });
        return { landed: landing(), reason: bound.result.current[0] };
      } finally {
        line.remove();
      }
    });
    expect(landed).toEqual({ landed: "Three", reason: NETWORK_ERROR_MESSAGE });
  } finally {
    act(() => onlineManager.setOnline(online));
    bound.unmount();
    realClock();
  }
});

// (di) Armed, and the reader presses Tab inside the region the press was made in while its write
// is out: a panel's Rename, held, and the reader's Tab to the control after it in the panel. The
// press stays armed, so the write's answer, which lets Rename go and takes that control away, is
// still the press's doing: the focus goes to the control now at its place.
test("a Tab inside the region of an armed press whose write is out keeps it armed, and the loss hands the focus on", async () => {
  const panel = document.createElement("div");
  panel.setAttribute("aria-label", "Session groups");
  panel.setAttribute("role", "dialog");
  document.body.append(panel);
  const labels = ["One", "Two", "Three", "Four"];
  const line = armedLineIn(panel, { labels });
  try {
    const two = line.button("Two");
    const three = line.button("Three");
    clickPress(two);
    line.redraw({ held: ["Two"], labels });
    // The reader's Tab, and the engine's move of the focus to Three.
    two.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Tab" }));
    three.focus();
    three.dispatchEvent(new KeyboardEvent("keyup", { bubbles: true, key: "Tab" }));
    for (const _ of [0, 1, 2]) await frameAndTasks();
    line.redraw({ labels: ["One", "Two", "Four"] });
    expect(landing()).toBe("Four");
  } finally {
    line.remove();
    disarm();
    panel.remove();
  }
});

/** A list that hands its own focus off and arms the hand-off at its own presses, as `ArmedLine`
 *  does: one row per name, each an `<li>` holding its Answer button and, after it, a panel the
 *  row opened in its own place (`role="dialog"`, a region of its own), whose Later `held` holds
 *  while its write is out. */
function ArmedRows(props: {
  readonly held: boolean;
  readonly names: readonly string[];
}): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  const noteFocus = useFocusPlace(
    (element) => (list.current?.contains(element) ? list.current : null),
    () => null
  );
  return createElement(
    "ul",
    { ...ARM_ON_PRESS, onFocus: noteFocus, ref: list },
    props.names.map((name) =>
      createElement(
        "li",
        { key: name },
        createElement("button", { type: "button" }, `Answer ${name}`),
        createElement(
          "div",
          { "aria-label": `Snooze ${name}`, role: "dialog" },
          createElement(
            "button",
            { "aria-disabled": props.held || undefined, type: "button" },
            `Later ${name}`
          )
        )
      )
    )
  );
}

// (dj) Armed, and the reader acts again before the loss: another key, a pointer press, or a Tab
// out of the region the press was made in. The reader presses Later in a panel opened in the
// first row, whose write holds it, and the focus moves to the row's Answer, outside that panel.
// Then the write lands and the row goes with the Answer. That loss is the press's only while the
// press is armed - the row held the pressed Later - and then the walk back lands on Before, the
// control before the row. Each act of the reader's disarms it, and the loss is answered as main
// does: the focus stays on the body, and nothing scrolls.
test("another key, a pointer press or a Tab out of the press's region disarms it, and the loss leaves the focus on the page", async () => {
  const acts: Readonly<Record<string, (later: HTMLElement, answer: HTMLElement) => void>> = {
    "a Tab out of the panel": (later, answer) => {
      later.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Tab" }));
      answer.focus();
      answer.dispatchEvent(new KeyboardEvent("keyup", { bubbles: true, key: "Tab" }));
    },
    "another key": (_later, answer) => {
      answer.focus();
      answer.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "x" }));
      answer.dispatchEvent(new KeyboardEvent("keyup", { bubbles: true, key: "x" }));
    },
    "a pointer press on the page": (_later, answer) => {
      answer.focus();
      document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    },
    "nothing after the press": (_later, answer) => answer.focus(),
  };
  const landed: Record<string, string | null> = {};
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    for (const [named, actAgain] of Object.entries(acts)) {
      landed[named] = await inMain(["Before"], async (main) => {
        const host = document.createElement("div");
        main.append(host);
        const names = ["One", "Two"];
        const view = render(createElement(ArmedRows, { held: false, names }), { container: host });
        try {
          const later = view.getByRole("button", { name: "Later One" });
          clickPress(later);
          view.rerender(createElement(ArmedRows, { held: true, names }));
          actAgain(later, view.getByRole("button", { name: "Answer One" }));
          await frameAndTasks();
          view.rerender(createElement(ArmedRows, { held: false, names: ["Two"] }));
          return landing();
        } finally {
          view.unmount();
          host.remove();
        }
      });
    }
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
  }
  expect(landed).toEqual({
    "a Tab out of the panel": "the page",
    "a pointer press on the page": "the page",
    "another key": "the page",
    "nothing after the press": "Before",
  });
});

// (dk) Armed by a press that starts no write and holds nothing, a frame and a task pass after its
// commit with no loss: the press is disarmed, and an update that takes the control away after
// that is no doing of the reader's. The focus stays on the body, and nothing scrolls.
test("an armed press that holds nothing is disarmed a frame and a task on, and a later loss leaves the focus on the page", async () => {
  await scrollingNothing(async () => {
    await inMain([], async (main) => {
      const line = armedLineIn(main, { labels: LINE });
      try {
        clickPress(line.button("Two"));
        await frameAndTasks();
        line.redraw({ labels: ["One", "Three"] });
        expect(document.activeElement).toBe(document.body);
      } finally {
        line.remove();
      }
    });
  });
});

// (dl) Armed by a press that starts a write, whose control is held through the re-read after the
// write lands (`useReadBound`): a loss inside that read is still the press's, and the focus goes
// to the control now at its place; once the read settles, the hold ends, and a frame and a task
// pass with no loss, the press is disarmed, and a loss after that leaves the focus on the page.
test("an armed press held through its write's re-read causes a loss inside that read, and none a frame and a task after the hold ends", async () => {
  const landed: Record<string, string | null> = {};
  const bound = renderHook(() => useReadBound());
  try {
    for (const when of ["inside the read", "after the read's hold ended"] as const) {
      landed[when] = await inMain([], async (main) => {
        const line = armedLineIn(main, { labels: LINE });
        const reread = Promise.withResolvers<void>();
        try {
          clickPress(line.button("Two"));
          const press = pressOfWrite();
          line.redraw({ held: ["Two"], labels: LINE });
          await frameAndTasks();
          // The write lands, and its `onSuccess` holds the press it began under through the
          // list's read.
          const held = bound.result.current[1](reread.promise, press);
          for (const _ of [0, 1, 2]) await frameAndTasks();
          if (when === "after the read's hold ended") {
            reread.resolve();
            await held;
            line.redraw({ labels: LINE });
            await frameAndTasks();
          }
          line.redraw({ labels: ["One", "Three"] });
          reread.resolve();
          await held;
          return landing();
        } finally {
          line.remove();
        }
      });
    }
  } finally {
    bound.unmount();
  }
  expect(landed).toEqual({ "after the read's hold ended": "the page", "inside the read": "Three" });
});

// (dm) Armed, and the press's write is refused: the press disabled its own control while the
// write was out, which dropped the focus to the page, and the refusal enables it again. Nothing
// pulls the focus back to it, and a frame and a task later the press is disarmed: a loss after
// that leaves the focus on the page.
test("an armed press whose write is refused is disarmed, and the focus is not pulled back to its control", async () => {
  await scrollingNothing(async () => {
    await inMain([], async (main) => {
      const line = armedLineIn(main, { labels: LINE });
      try {
        const two = line.button("Two");
        clickPress(two);
        // The press disables its own control, and the engine drops the focus off it then;
        // happy-dom blurs no disabled button, so the focus is dropped first.
        two.blur();
        line.redraw({ disabled: ["Two"], labels: LINE });
        for (const _ of [0, 1]) await frameAndTasks();
        line.redraw({ labels: LINE });
        await frameAndTasks();
        expect(document.activeElement).toBe(document.body);
        // Focused with no press, as the page's own keys put it there, and taken away by an update.
        two.focus();
        line.redraw({ labels: ["One", "Three"] });
        expect(document.activeElement).toBe(document.body);
      } finally {
        line.remove();
      }
    });
  });
});

// (dn) Armed, and a hold on what answers the press that never ends: the press stays armed until
// the write deadline and then the request deadline have passed from it, the longest a write that
// lands within its deadline and then its re-read can take, and no longer. A loss a second short
// of that bound is the press's, and the focus goes to the control now at its place; a refetch
// once the bound has passed, a frame and a task on, is no doing of the reader's.
test("an armed press whose hold never ends is disarmed at its bound, and a later refetch leaves the focus on the page", async () => {
  jest.useFakeTimers();
  const landed: Record<string, string | null> = {};
  try {
    const waits = {
      "a second short of the bound": SEND_DEADLINE_MS + REQUEST_DEADLINE_MS - 1_000,
      "once the bound has passed": SEND_DEADLINE_MS + REQUEST_DEADLINE_MS,
    };
    for (const [named, wait] of Object.entries(waits)) {
      landed[named] = await inMain([], async (main) => {
        const line = armedLineIn(main, { labels: LINE });
        try {
          clickPress(line.button("Two"));
          const answered = holdHandOff();
          jest.advanceTimersByTime(wait);
          // A press its bound ended ends a frame and a task on.
          await nextFrame();
          jest.advanceTimersByTime(0);
          line.redraw({ labels: ["One", "Three"] });
          answered();
          return landing();
        } finally {
          line.remove();
        }
      });
    }
  } finally {
    realClock();
  }
  expect(landed).toEqual({
    "a second short of the bound": "Three",
    "once the bound has passed": "the page",
  });
});

// An action of several writes, as the List's Start next, holds its press through its whole run
// (`holdHandOff`), and each write it begins counts the press's bound from there
// (`actionWriteBegins`). A write begun 50 s after the press keeps the press live past the bound
// counted from the press, until the bound counted from that write, and no longer. A write begun
// once the bound has passed counts nothing: the press ends a frame and a task after the bound,
// as one whose action begins no write does.
test("each write an armed press's action begins counts its bound from there, and one begun past the bound counts nothing", async () => {
  const bound = SEND_DEADLINE_MS + REQUEST_DEADLINE_MS;
  // How long after the press the write begins, and how long after that the update comes.
  const writes: Readonly<Record<string, readonly [number, number]>> = {
    "a write begun 50 s on, a second short of the bound from it": [50_000, bound - 1_000],
    "a write begun 50 s on, once the bound from it has passed": [50_000, bound],
    "a write begun once the bound from the press has passed": [bound, 0],
  };
  jest.useFakeTimers();
  const landed: Record<string, string | null> = {};
  try {
    for (const [named, [begun, after]] of Object.entries(writes)) {
      landed[named] = await inMain([], async (main) => {
        const line = armedLineIn(main, { labels: LINE });
        try {
          clickPress(line.button("Two"));
          const press = pressOfWrite();
          const answered = holdHandOff(press);
          jest.advanceTimersByTime(begun);
          actionWriteBegins(press);
          jest.advanceTimersByTime(after);
          // A press its bound ended ends a frame and a task on.
          await nextFrame();
          jest.advanceTimersByTime(0);
          line.redraw({ labels: ["One", "Three"] });
          answered();
          return landing();
        } finally {
          line.remove();
        }
      });
    }
  } finally {
    realClock();
  }
  expect(landed).toEqual({
    "a write begun 50 s on, a second short of the bound from it": "Three",
    "a write begun 50 s on, once the bound from it has passed": "the page",
    "a write begun once the bound from the press has passed": "the page",
  });
});

// A hold over a write takes the press the write began under (`pressOfWrite`), however late the
// hold begins. Here that press ended a frame and a task after its arm, with nothing holding it,
// and the reader has pressed again since: a hold of the ended press keeps nothing live, neither
// that press nor the later one, which ends a frame and a task after its own arm as a press that
// holds nothing does. The update that then takes the later press's control away leaves the focus
// on the page.
test("a hold of a press that has ended keeps nothing live, neither that press nor a later one", async () => {
  await scrollingNothing(async () => {
    const seen = await inMain([], async (main) => {
      const line = armedLineIn(main, { labels: LINE });
      try {
        clickPress(line.button("One"));
        const ended = pressOfWrite();
        await frameAndTasks();
        const armedAfterOne = pressArmed();
        clickPress(line.button("Two"));
        const answered = holdHandOff(ended);
        await frameAndTasks();
        const armedAfterTwo = pressArmed();
        line.redraw({ labels: ["One", "Three"] });
        answered();
        return { armedAfterOne, armedAfterTwo, landed: landing() };
      } finally {
        line.remove();
      }
    });
    expect(seen).toEqual({ armedAfterOne: false, armedAfterTwo: false, landed: "the page" });
  });
});

/** The writes `TwoWrites` makes, each answered when the case says: A's, the re-read A's landing
 *  holds through, and B's. */
interface TwoWritesAnswers {
  readonly readA: () => Promise<void>;
  readonly writeA: () => Promise<void>;
  readonly writeB: () => Promise<void>;
}

/** Two controls in one root that arms at a press (`ARM_ON_PRESS`), as the Inbox's reading column
 *  draws its card's: A, whose landing holds through the re-read after it (`useReadBound`), as the
 *  card's Unfollow does, and B, held through its answer (`useHeldThroughAnswer`), as the column
 *  head's Snooze is. Each is held while its write is out, and A reads the press its write began
 *  under at its own press (`pressOfWrite`), as a keyed write does in its `onMutate`. */
function TwoWrites({ writes }: { readonly writes: TwoWritesAnswers }): ReactNode {
  const [, holdThrough] = useReadBound();
  const [pendingA, setPendingA] = useState(false);
  const [pendingB, setPendingB] = useState(false);
  const heldA = useHeldThroughAnswer(pendingA);
  const heldB = useHeldThroughAnswer(pendingB);
  const pressA = () => {
    const press = pressOfWrite();
    setPendingA(true);
    void writes
      .writeA()
      .then(() => holdThrough(writes.readA(), press))
      .then(() => setPendingA(false));
  };
  const pressB = () => {
    setPendingB(true);
    void writes.writeB().then(() => setPendingB(false));
  };
  return createElement(
    "div",
    ARM_ON_PRESS,
    createElement(
      "button",
      { "aria-disabled": heldA || undefined, onClick: pressA, type: "button" },
      "A"
    ),
    createElement(
      "button",
      { "aria-disabled": heldB || undefined, onClick: pressB, type: "button" },
      "B"
    )
  );
}

// Two writes in one root, each from a press of its own: the reader presses A, whose landing holds
// through the re-read after it (`useReadBound`), and then B while A's write is out, which ends A's
// press. A's write lands while B's is out, and its re-read does not settle. That hold is of the
// press A's write began under, which has ended, so it keeps nothing live: B's press ends a frame
// and a task after B's own write lands, as B's alone does, and a loss of B after that is no doing
// of the reader's. A hold that took the press armed as A's answer came, B's, would keep B's press
// live until the re-read's bound, 30 s on.
test("a later press is not kept live by an earlier write's re-read: it ends a frame and a task after its own write lands", async () => {
  const a = Promise.withResolvers<void>();
  const readA = Promise.withResolvers<void>();
  const b = Promise.withResolvers<void>();
  const view = render(
    createElement(TwoWrites, {
      writes: { readA: () => readA.promise, writeA: () => a.promise, writeB: () => b.promise },
    })
  );
  const seen: Record<string, boolean> = {};
  jest.useFakeTimers();
  try {
    const buttonB = view.getByRole("button", { name: "B" });
    act(() => view.getByRole("button", { name: "A" }).click());
    await tick(0);
    act(() => buttonB.click());
    await tick(0);
    seen["B pressed"] = causedByReader(buttonB);
    await act(async () => a.resolve());
    await tick(0);
    seen["A's write landed, B's out"] = causedByReader(buttonB);
    await act(async () => b.resolve());
    await tick(0);
    await tick(0);
    seen["a frame and a task after B's write landed"] = causedByReader(buttonB);
    await act(async () => readA.resolve());
    await tick(0);
  } finally {
    disarm();
    realClock();
    view.unmount();
  }
  expect(seen).toEqual({
    "a frame and a task after B's write landed": false,
    "A's write landed, B's out": true,
    "B pressed": true,
  });
});

// (do) Nothing armed: a refetch, a live update or another device takes the focused control away
// - a control of a component that hands its focus off, or the row holding it. That is as main
// does: the focus stays on the body, in no row, and nothing scrolls. The selection bar emptied
// the same way is `SelectionBar.test.tsx`'s.
test("with nothing armed, a control taken away by an update leaves the focus on the page, in no row, scrolling nothing", async () => {
  const landed: Record<string, string | null> = {};
  await scrollingNothing(async () => {
    landed["a control of a line"] = await inMain([], async (main) => {
      const line = armedLineIn(main, { labels: LINE });
      try {
        line.button("Two").focus();
        line.redraw({ labels: ["One", "Three"] });
        return landing();
      } finally {
        line.remove();
      }
    });
    landed["a row of a list"] = await inMain(["Before"], async (main) => {
      const host = document.createElement("div");
      main.append(host);
      const view = render(createElement(ArmedRows, { held: false, names: ["One", "Two"] }), {
        container: host,
      });
      try {
        view.getByRole("button", { name: "Answer One" }).focus();
        view.rerender(createElement(ArmedRows, { held: false, names: ["Two"] }));
        return landing();
      } finally {
        view.unmount();
        host.remove();
      }
    });
  });
  expect(landed).toEqual({ "a control of a line": "the page", "a row of a list": "the page" });
});

// (dp) Nothing armed, offline past the request deadline: a control held through a re-read the
// network never answers (`useReadBound`), focused with no press. Its hold ends as its own rule
// says, at the request deadline with the network's reason, and lets the control go, which keeps
// the focus. The update that then takes it away is no press's doing: the focus stays on the
// page, and nothing scrolls.
test("with nothing armed, a hold that ends offline at the request deadline keeps its own rule, and the later loss leaves the focus on the page", async () => {
  jest.useFakeTimers();
  const online = onlineManager.isOnline();
  const bound = renderHook(() => useReadBound());
  try {
    act(() => onlineManager.setOnline(false));
    await scrollingNothing(async () => {
      await inMain([], async (main) => {
        const line = armedLineIn(main, { labels: LINE });
        try {
          const two = line.button("Two");
          two.focus();
          line.redraw({ held: ["Two"], labels: LINE });
          const held = bound.result.current[1](new Promise<never>(() => undefined), pressOfWrite());
          await act(async () => {
            jest.advanceTimersByTime(REQUEST_DEADLINE_MS);
            await held;
          });
          line.redraw({ labels: LINE });
          expect(bound.result.current[0]).toBe(NETWORK_ERROR_MESSAGE);
          expect(two).toHaveFocus();
          line.redraw({ labels: ["One", "Three"] });
          expect(document.activeElement).toBe(document.body);
        } finally {
          line.remove();
        }
      });
    });
  } finally {
    act(() => onlineManager.setOnline(online));
    bound.unmount();
    realClock();
  }
});

// (dq) A keyboard reader arms what a pointer reader arms (`armOnPress`): at a component's root,
// Enter arms on any element - a list's text box picks its active row at Enter's `keydown`
// (`MultiSelect`) - and Space on an element that takes no typed text, where it presses. Space in
// a text box types, Escape is the reader leaving a control, and Tab or any other key moves or
// acts on nothing of the component's, so none of them arms.
test("Enter, and Space on a control that takes no typed text, arm a component's hand-off at its root, and no other key does", async () => {
  const armed: Record<string, boolean> = {};
  await inMain([], async (main) => {
    const host = document.createElement("div");
    main.append(host);
    const view = render(
      createElement(
        "div",
        { onKeyDownCapture: armOnPress },
        createElement("button", { type: "button" }, "Save"),
        createElement("input", { "aria-label": "Name", type: "text" })
      ),
      { container: host }
    );
    try {
      const save = view.getByRole("button", { name: "Save" });
      const name = view.getByRole("textbox", { name: "Name" });
      const keys = [
        ["Enter on a button", save, "Enter"],
        ["Space on a button", save, " "],
        ["Enter in a text box", name, "Enter"],
        ["Space in a text box", name, " "],
        ["Escape on a button", save, "Escape"],
        ["Tab on a button", save, "Tab"],
        ["x on a button", save, "x"],
      ] as const;
      for (const [named, target, key] of keys) {
        disarm();
        target.focus();
        // The arm is read in the task the key is pressed in, which user-event's keyboard waits past.
        fireEvent.keyDown(target, { key });
        armed[named] = causedByReader(target);
      }
    } finally {
      view.unmount();
      host.remove();
    }
  });
  expect(armed).toEqual({
    "Enter in a text box": true,
    "Enter on a button": true,
    "Escape on a button": false,
    "Space in a text box": false,
    "Space on a button": true,
    "Tab on a button": false,
    "x on a button": false,
  });
});

// (dr) Armed by a press whose action holds what answers it (`holdHandOff`), and whose control
// stays held once that action is answered only because there is nothing left to do: the project
// List's Move, whose own press leaves it no epic to move to (`issues-table-page.tsx`). A hold that
// says only that keeps no press live: once the action's hold ends and a frame and a task pass,
// the press is disarmed, and a re-render that then takes the focused control away - a live update
// or a refetch - is no doing of the reader's. The focus stays on the page, and nothing scrolls.
test("an armed press whose action is answered while its control stays held with nothing to do is disarmed, and a later loss leaves the focus on the page", async () => {
  await scrollingNothing(async () => {
    await inMain([], async (main) => {
      const line = armedLineIn(main, { labels: LINE });
      try {
        clickPress(line.button("Two"));
        // The action's run holds what answers it, and the press holds its control.
        const answered = holdHandOff();
        line.redraw({ held: ["Two"], labels: LINE });
        await frameAndTasks();
        // The action is answered; the control stays held, with nothing left to do.
        answered();
        await frameAndTasks();
        line.redraw({ held: ["Two"], labels: ["One", "Three"] });
        expect(landing()).toBe("the page");
      } finally {
        line.remove();
      }
    });
  });
});

/** The reader's pointer press on `element` that moves no focus, as WebKit's press on a button
 *  leaves the focus where it was: its `pointerdown`, then its `click`. */
function pointerPress(element: HTMLElement): void {
  element.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
  fireEvent.click(element);
}

// (ds) Armed by a pick in a list a control of the component opened - Enter in the list's text
// box, where the shared list picks its active row (`MultiSelect`) - whose write holds that
// control while it is out: the Inbox selection bar's Snooze selected asks. The pick puts the
// focus on the control and takes the list, and the text box the press was armed on, away. A
// second press on the held control, by Enter or by a click, takes nothing but its hold: the pick
// stays the cause, though the control it was armed on has left the page, and the write's answer
// that takes the control away hands the focus to the control now at its place, as it does with
// no second press. A press on Pinned, held since before the pick, is the reader's next act: it
// arms anew, nothing keeps that press live, and the loss leaves the focus on the page.
test("a second press on the control a pick's write holds leaves the pick the cause though its list's text box has gone, and a press on a control held before the pick arms anew", async () => {
  const labels = ["Before", "Snooze", "After", "Pinned"];
  const acts: Readonly<Record<string, (snooze: HTMLElement, pinned: HTMLElement) => void>> = {
    "a click on Pinned, held before the pick": (_snooze, pinned) => pointerPress(pinned),
    "a click on the held Snooze": (snooze) => pointerPress(snooze),
    "a second Enter on the held Snooze": (snooze) => enterPress(snooze),
    "nothing after the pick": () => undefined,
  };
  const landed: Record<string, string | null> = {};
  for (const [named, actAgain] of Object.entries(acts)) {
    const bar = document.createElement("div");
    bar.dataset.control = CONTROL.selectionBar;
    document.body.append(bar);
    const line = armedLineIn(bar, { held: ["Pinned"], labels, list: true });
    try {
      const search = document.querySelector<HTMLElement>('input[aria-label="Search"]');
      if (search === null) throw new Error("the list drew no text box");
      search.focus();
      // The pick: Enter in the list's text box arms the hand-off, the list puts the focus on its
      // trigger and goes, and the write the pick sends holds the trigger and what answers it.
      enterPress(search);
      const snooze = line.button("Snooze");
      snooze.focus();
      line.redraw({ held: ["Pinned", "Snooze"], labels });
      const answered = holdHandOff();
      actAgain(snooze, line.button("Pinned"));
      for (const _ of [0, 1, 2]) await frameAndTasks();
      // The write lands, and its answer takes the trigger away.
      line.redraw({ held: ["Pinned"], labels: ["Before", "After", "Pinned"] });
      answered();
      landed[named] = landing();
    } finally {
      line.remove();
      disarm();
      bar.remove();
    }
  }
  expect(landed).toEqual({
    "a click on Pinned, held before the pick": "the page",
    "a click on the held Snooze": "After",
    "a second Enter on the held Snooze": "After",
    "nothing after the pick": "After",
  });
});

/** A component that hands its own focus off and counts every focus inside it in its line of
 *  buttons, one per label, a portal's focus too, as a panel counts every focus in its content
 *  (`PanelTrigger`). With `list`, a list one of its buttons opened is drawn as well, outside the
 *  line in a portal on the page's body, as a panel of its own holding one button per label: the
 *  shared list's panel (`MultiSelect`), which `AnchoredPanel` marks with `PANEL_ATTRIBUTE`. */
function LineWithList(props: {
  readonly labels: readonly string[];
  readonly list?: readonly string[];
}): ReactNode {
  const line = useRef<HTMLDivElement>(null);
  const noteFocus = useFocusPlace(
    () => line.current,
    () => null
  );
  const button = (label: string) => createElement("button", { key: label, type: "button" }, label);
  return createElement(
    "div",
    { ...ARM_ON_PRESS, onFocus: noteFocus },
    createElement("div", { ref: line }, props.labels.map(button)),
    props.list === undefined
      ? null
      : createPortal(
          createElement("div", { [PANEL_ATTRIBUTE]: "" }, props.list.map(button)),
          document.body
        )
  );
}

/** The button named `label` anywhere in the document. */
function buttonNamed(label: string): HTMLButtonElement {
  const found = [...document.querySelectorAll("button")].find(
    (button) => button.textContent === label
  );
  if (found === undefined) throw new Error(`no button ${label} is drawn`);
  return found;
}

// (dt) Armed by a press on an option of a list a control of the component opened, which the page
// draws as a panel of its own outside the component: the Inbox Filter panel's Project list, on the
// option of a project the press unpicks. The focus on the option is counted at the place of the
// control that opened the list. Where the press's commit takes the option away and the list stays,
// the loss is the list's to hand on (`MultiSelect` gives it to its search box), and the
// component's hand-off moves nothing. Where the list goes with the option, as where the control
// that opened it goes, the hand-off lands on the control now at that control's place.
test("a loss inside a list a control opened outside the component is the list's while the list stays, and the component's once the list goes with it", async () => {
  const landed: Record<string, string | null> = {};
  for (const way of ["the list stays", "the list goes with the option"] as const) {
    landed[way] = await inMain([], async (main) => {
      const host = document.createElement("div");
      main.append(host);
      const view = render(createElement(LineWithList, { labels: LINE, list: ["Alpha", "Beta"] }), {
        container: host,
      });
      try {
        buttonNamed("Two").focus();
        clickPress(buttonNamed("Alpha"));
        view.rerender(
          createElement(LineWithList, {
            labels: LINE,
            list: way === "the list stays" ? ["Beta"] : undefined,
          })
        );
        return landing();
      } finally {
        view.unmount();
        host.remove();
      }
    });
  }
  expect(landed).toEqual({
    "the list goes with the option": "Two",
    "the list stays": "the page",
  });
});

/** The project List's section (`IssuesTable`): a root that arms the hand-off at its own presses
 *  (`armOnPress`), around two components that hand their own focus off and arm it at their own
 *  roots (`ArmedLine`): the selection bar's line of `bar` buttons, and the table's body, whose
 *  one row draws Start next, `held` while the bar's write is out (`perform`). */
function ArmedSection(props: {
  readonly bar: readonly string[];
  readonly held: boolean;
}): ReactNode {
  return createElement(
    "section",
    ARM_ON_PRESS,
    createElement(ArmedLine, { labels: props.bar }),
    createElement(ArmedLine, { held: props.held ? ["Start next"] : [], labels: ["Start next"] })
  );
}

// (dt) Armed by a press in one component whose write holds a control of another inside the same
// root: the project List's selection bar's Take out of epic, whose run holds every row's Start
// next, in the table's body, a root no press had reached by the arm. A double click on the held
// Start next - in WebKit, which leaves the focus on the bar's button - takes nothing but its hold,
// though its first click is the first press the body's root hears: the bar's press stays the
// cause, and the write's answer that takes Take out of epic away hands the focus to the control
// now at its place, as it does with no click.
test("a double click on a control the press's write holds, in a root inside the press's own that first hears a press after the arm, leaves the press the cause", async () => {
  const bar = ["Before", "Take out of epic", "After"];
  const acts: Readonly<Record<string, (start: HTMLElement) => void>> = {
    "a double click on the held Start next": (start) => {
      pointerPress(start);
      pointerPress(start);
    },
    "nothing after the press": () => undefined,
  };
  const landed: Record<string, string | null> = {};
  for (const [named, actAgain] of Object.entries(acts)) {
    landed[named] = await inMain([], async (main) => {
      const host = document.createElement("div");
      main.append(host);
      const view = render(createElement(ArmedSection, { bar, held: false }), { container: host });
      try {
        clickPress(view.getByRole("button", { name: "Take out of epic" }));
        // The run the press starts holds every row's Start next, and what answers it.
        view.rerender(createElement(ArmedSection, { bar, held: true }));
        const answered = holdHandOff();
        actAgain(view.getByRole("button", { name: "Start next" }));
        for (const _ of [0, 1, 2]) await frameAndTasks();
        // The writes land, and their answer takes Take out of epic away with the selection.
        view.rerender(createElement(ArmedSection, { bar: ["Before", "After"], held: false }));
        answered();
        return landing();
      } finally {
        view.unmount();
        host.remove();
      }
    });
  }
  expect(landed).toEqual({
    "a double click on the held Start next": "After",
    "nothing after the press": "After",
  });
});

/** The reader's pointer press on `element` that drops the focus onto the body, as WebKit's press
 *  on a button can where nothing around the button takes the focus: its `pointerdown`, the focus
 *  taken off the element that holds it, then its `click`. */
function droppingPress(element: HTMLElement): void {
  element.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
  if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  fireEvent.click(element);
}

// INFRA-3135, round 29 (the architect's ruling of 2026-11-26 06:18Z, rule (3)): what the bar's
// hand-off reads to tell the reader's next act from a press the armed press counts as its own
// (`pressIsArmedOwn`), the test the armed press itself ends on: a press on its own control, or on a
// control its write holds - the held Start next of Take out of epic's run - is its own; a press on
// a control nothing holds is not, and nothing is its own once the press has ended.
test("the armed press counts a press on its own control or on a control its write holds as its own, and no other", async () => {
  const read: Record<string, boolean> = {};
  await inMain(["Filter"], async (main) => {
    const host = document.createElement("div");
    main.append(host);
    const bar = ["Take out of epic"];
    const view = render(createElement(ArmedSection, { bar, held: false }), { container: host });
    try {
      const startNext = () => view.getByRole("button", { name: "Start next" });
      const takeOut = () => view.getByRole("button", { name: "Take out of epic" });
      const filter = main.querySelector<HTMLElement>(":scope > button");
      if (filter === null) throw new Error("the page draws no Filter");
      disarm();
      read["with nothing armed"] = pressIsArmedOwn(takeOut());
      clickPress(takeOut());
      view.rerender(createElement(ArmedSection, { bar, held: true }));
      const answered = holdHandOff();
      read["its own control"] = pressIsArmedOwn(takeOut());
      read["the Start next its write holds"] = pressIsArmedOwn(startNext());
      read["the Filter nothing holds"] = pressIsArmedOwn(filter);
      read["no element"] = pressIsArmedOwn(null);
      answered();
      disarm();
      read["once the press has ended"] = pressIsArmedOwn(startNext());
    } finally {
      view.unmount();
      host.remove();
    }
  });
  expect(read).toEqual({
    "with nothing armed": false,
    "its own control": true,
    "the Start next its write holds": true,
    "the Filter nothing holds": false,
    "no element": false,
    "once the press has ended": false,
  });
});

// What the selection bar reads of the armed press as it goes after the reader's press on a
// control outside it that the press holds (`SelectionBar`): whether that press, or a move of the
// focus onto such a control, took the focus from inside the bar (`repeatTookFocusFrom`), and
// whether the reader had scrolled the element the press last noted the focus on out of their
// sight (`focusedOutOfSight`), an element that may have left the page by the time the bar asks.
// Armed by the project List's Take out of epic, whose run holds the row's Start next. A press on
// that Start next takes the focus from the bar when it drops the focus onto the body, as WebKit's
// can, and when it puts the focus on the Start next. A second press keeps that, and a move of the
// focus from the bar onto the held Start next with no press counts the same. A move of the focus
// onto an unheld control forgets it, and so does the press's end. A press made after the focus
// left the bar for the body takes nothing from the bar. The Start next the reader clicks is in
// their sight until they scroll the page past it.
test("the armed press says whether a press on a control its write holds took the focus from a place, and whether the reader scrolled the element it last noted the focus on out of sight", async () => {
  const read: Record<string, boolean> = {};
  await inMain(["Filter"], async (main) => {
    const host = document.createElement("div");
    main.append(host);
    const bar = ["Take out of epic"];
    const view = render(createElement(ArmedSection, { bar, held: false }), { container: host });
    try {
      const startNext = () => view.getByRole("button", { name: "Start next" });
      const takeOut = () => view.getByRole("button", { name: "Take out of epic" });
      const filter = main.querySelector<HTMLElement>(":scope > button");
      // The bar's line, holding Take out of epic, and the table body's, holding Start next.
      const barLine = takeOut().parentElement;
      const startLine = startNext().parentElement;
      if (filter === null || barLine === null || startLine === null) {
        throw new Error("the page draws no Filter, or a button stands in no line");
      }
      // No press of an earlier case is still armed.
      disarm();
      read["with nothing armed"] = repeatTookFocusFrom(barLine);
      read["out of sight, with nothing armed"] = focusedOutOfSight();
      clickPress(takeOut());
      // The run the press starts holds Start next, and what answers it.
      view.rerender(createElement(ArmedSection, { bar, held: true }));
      const answered = holdHandOff();
      read["the bar's press alone"] = repeatTookFocusFrom(barLine);
      droppingPress(startNext());
      read["a press on the held Start next that focused nothing"] = repeatTookFocusFrom(barLine);
      read["the same, from the Start next's own line"] = repeatTookFocusFrom(startLine);
      // A second click, which focuses the held Start next, as Chromium's does.
      startNext().dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      clickPress(startNext());
      read["a second press, which focused the held Start next"] = repeatTookFocusFrom(barLine);
      read["out of sight, the focused Start next in sight"] = focusedOutOfSight();
      // The page scrolled past the row: its Start next stands above the window's top edge, which
      // the press reads at its next frame.
      placeAt(startNext(), -400);
      await frameAndTasks();
      read["out of sight, the focused Start next scrolled above the window"] = focusedOutOfSight();
      // A script's focus on the Filter, which nothing holds.
      filter.focus();
      read["the focus then moved onto the unheld Filter"] = repeatTookFocusFrom(barLine);
      // The focus back on Take out of epic, then off it onto the body, before a press on the held
      // Start next.
      takeOut().focus();
      takeOut().blur();
      droppingPress(startNext());
      read["a press after the focus left the bar for the body"] = repeatTookFocusFrom(barLine);
      // A script's focus on Take out of epic, then on the held Start next, with no press.
      takeOut().focus();
      startNext().focus();
      read["the focus moved from the bar onto the Start next"] = repeatTookFocusFrom(barLine);
      answered();
      disarm();
      read["once the press has ended"] = repeatTookFocusFrom(barLine);
      read["out of sight, once the press has ended"] = focusedOutOfSight();
    } finally {
      view.unmount();
      host.remove();
    }
  });
  expect(read).toEqual({
    "with nothing armed": false,
    "out of sight, with nothing armed": false,
    "the bar's press alone": false,
    "a press on the held Start next that focused nothing": true,
    "the same, from the Start next's own line": false,
    "a second press, which focused the held Start next": true,
    "out of sight, the focused Start next in sight": false,
    "out of sight, the focused Start next scrolled above the window": true,
    "the focus then moved onto the unheld Filter": false,
    "a press after the focus left the bar for the body": false,
    "the focus moved from the bar onto the Start next": true,
    "once the press has ended": false,
    "out of sight, once the press has ended": false,
  });
});

// (du) Armed, and the press's commit takes away the line with its control - its container gone,
// as a slot of the search bar's control row goes with its only control - inside a dialog, while
// the line's fallback stands out of the reader's sight. The focus goes, with no scroll, to the own
// stop of the region the control stood in as it took the focus: the dialog. A container that has
// left the page has no dialog or main content around it to say its region, and the page's main
// content behind the dialog is not where the reader was.
test("an armed press whose commit takes away its container in a dialog, with the fallback out of sight, lands on the dialog's own stop, not the page's main content", async () => {
  withMain(() => {
    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    dialog.setAttribute("aria-label", "Search bar");
    document.body.append(dialog);
    try {
      // The fallback stands 500 px above the window's top, out of the reader's sight.
      const trigger = windowSelectIn(dialog, -500);
      const fallback = () => trigger;
      const line = armedLineIn(dialog, { fallback, labels: ["Mark all read"] });
      try {
        clickPress(line.button("Mark all read"));
        const recorded = recordMoves();
        try {
          line.redraw({ fallback, labels: [], shown: false });
        } finally {
          recorded.restore();
        }
        expect({ landed: landing(), moves: recorded.moves }).toEqual({
          landed: "Search bar",
          moves: [{ move: "focus", of: "Search bar", options: { preventScroll: true } }],
        });
      } finally {
        line.remove();
      }
    } finally {
      disarm();
      dialog.remove();
    }
  });
});

/** The two Enters a text field can be sent: the reader's own, and the one that ends an input
 *  method's composition, which Safari sends after the composition's end, unmarked - `isComposing`
 *  false - with keyCode 229 (`inputMethodKey`). */
const ENTERS = {
  "a plain Enter": { key: "Enter" },
  "an input method's Enter": { isComposing: false, key: "Enter", keyCode: 229 },
} as const;

// (dv) Armed, and an input method's Enter comes in a text field while the press is live. It
// commits what the reader was composing and acts on nothing of the page's, so it is no act of the
// reader's (`isPress`): the press stays live, and its commit that takes the control away hands the
// focus to the control now at its place. A plain Enter in the same field is the reader's next act:
// it ends the press, and the loss leaves the focus on the page.
test("an input method's Enter in a text field, at keyCode 229, leaves an armed press live, where a plain Enter ends it", async () => {
  const landed: Record<string, string | null> = {};
  for (const [named, enter] of Object.entries(ENTERS)) {
    landed[named] = await inMain([], async (main) => {
      const field = controlOf("text");
      field.setAttribute("aria-label", "Title");
      main.append(field);
      const line = armedLineIn(main, { labels: LINE });
      try {
        clickPress(line.button("Two"));
        // user-event has no form for this key: it cannot mark a key as an input method's.
        fireEvent.keyDown(field, enter);
        line.redraw({ labels: ["One", "Three"] });
        return landing();
      } finally {
        line.remove();
      }
    });
  }
  expect(landed).toEqual({ "a plain Enter": "the page", "an input method's Enter": "Three" });
});

// (dw) An input method's Enter in a text box at a component's root arms nothing (`armOnPress`):
// it commits what the reader was composing into the box and presses nothing. A plain Enter there
// arms the hand-off, as a list's text box picks its active row at it (`MultiSelect`).
test("an input method's Enter at keyCode 229 arms a component's hand-off at its root nowhere, where a plain Enter arms it", async () => {
  const armed: Record<string, boolean> = {};
  await inMain([], async (main) => {
    const host = document.createElement("div");
    main.append(host);
    const view = render(
      createElement(
        "div",
        { onKeyDownCapture: armOnPress },
        createElement("input", { "aria-label": "Name", type: "text" })
      ),
      { container: host }
    );
    try {
      const name = view.getByRole("textbox", { name: "Name" });
      for (const [named, enter] of Object.entries(ENTERS)) {
        disarm();
        name.focus();
        // user-event has no form for this key: it cannot mark a key as an input method's.
        fireEvent.keyDown(name, enter);
        armed[named] = causedByReader(name);
      }
    } finally {
      view.unmount();
      host.remove();
    }
  });
  expect(armed).toEqual({ "a plain Enter": true, "an input method's Enter": false });
});

/** Where a list's box stands down the window, in px, as the project List draws one: it clips what
 *  overflows it, and its sticky head, 40 px tall, stays at its top. */
const LIST_BOX = { bottom: 500, headBottom: 140, top: 100 } as const;

/** Where a landing below a list's sticky head (`landNearest`) leaves a checkbox laid out 13 px
 *  tall at `top` px down the window, in a list's box (`LIST_BOX`) whose own scroll padding is
 *  `padding`, set as its inline style. happy-dom lays nothing out and scrolls nothing, so the
 *  box's own scroll moves the checkbox by hand. Answers how far the checkbox then stands below the
 *  box's top and above its bottom, and every scroll asked of the box and of the window, once the
 *  landing has looked again after its commit (`land`). */
async function landedInBox(
  top: number,
  padding: Readonly<Record<string, string>>
): Promise<{ above: number; below: number; box: unknown[]; window: unknown[] }> {
  const box = document.createElement("div");
  // happy-dom does not expand the `overflow` shorthand, so the case sets the two values the
  // browser computes for the List's `overflow-auto`.
  box.style.overflowX = "auto";
  box.style.overflowY = "auto";
  for (const [property, value] of Object.entries(padding)) box.style.setProperty(property, value);
  box.getBoundingClientRect = () =>
    new DOMRect(0, LIST_BOX.top, 400, LIST_BOX.bottom - LIST_BOX.top);
  const head = document.createElement("div");
  head.getBoundingClientRect = () =>
    new DOMRect(0, LIST_BOX.top, 400, LIST_BOX.headBottom - LIST_BOX.top);
  const mark = document.createElement("input");
  mark.type = "checkbox";
  mark.setAttribute("aria-label", "Select CORE-7");
  let markTop = top;
  mark.getBoundingClientRect = () => new DOMRect(10, markTop, 13, 13);
  box.append(head, mark);
  document.body.append(box);
  const boxScroll = spyOn(box, "scrollBy").mockImplementation((by?: ScrollToOptions | number) => {
    if (typeof by === "object") markTop -= by.top ?? 0;
  });
  const windowScroll = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    expect(landNearest(mark, head)).toBe(true);
    // The landing looks again in a microtask, once the commit it landed in is done (`land`).
    await Promise.resolve();
    const landed = mark.getBoundingClientRect();
    return {
      above: LIST_BOX.bottom - landed.bottom,
      below: landed.top - LIST_BOX.top,
      box: [...boxScroll.mock.calls],
      window: [...windowScroll.mock.calls],
    };
  } finally {
    windowScroll.mockRestore();
    boxScroll.mockRestore();
    box.remove();
  }
}

// A landing that scrolls a list's box brings its checkbox below the larger of the sticky head's
// bottom and the box's own `scroll-padding-top`, where the browser's own focus scroll in that box
// brings it too. The project List's box keeps 64 px at its top (`DataTable`), under a head about
// 50 px tall, and in WebKit a landing flush under the head let the engine scroll the box again by
// itself. A checkbox under the 40 px head lands 64 px below the box's top, not 40, by the least
// scroll that brings it there, and the window does not scroll.
test("a landing that scrolls a box brings its control below the box's own scroll-padding-top, not only below the sticky head", async () => {
  expect(await landedInBox(120, { "scroll-padding-top": "64px" })).toEqual({
    above: 323,
    below: 64,
    box: [[{ top: -44 }]],
    window: [],
  });
});

// The box's bottom follows the same rule: a checkbox the box's bottom edge cuts lands 64 px above
// it where the box keeps a `scroll-padding-bottom` of 64 px, given as a length or as a percentage
// of the box's scrollport, 16% of its 400 px.
test("a landing that scrolls a box brings its control above the box's own scroll-padding-bottom, as a length or a percentage", async () => {
  const landed: Record<string, object> = {};
  for (const padding of ["64px", "16%"]) {
    landed[padding] = await landedInBox(495, { "scroll-padding-bottom": padding });
  }
  const atPadding = { above: 64, below: 323, box: [[{ top: 72 }]], window: [] };
  expect(landed).toEqual({ "16%": atPadding, "64px": atPadding });
});

// A box that keeps no scroll padding lands its checkbox as before: flush below the sticky head
// from above, and flush on the box's bottom edge from below, by the least scroll.
test("a landing in a box with no scroll padding brings its control flush below the sticky head or on the box's bottom edge", async () => {
  expect({
    "from above": await landedInBox(120, {}),
    "from below": await landedInBox(495, {}),
  }).toEqual({
    "from above": { above: 347, below: 40, box: [[{ top: -20 }]], window: [] },
    "from below": { above: 0, below: 387, box: [[{ top: 8 }]], window: [] },
  });
});

// The padding says only where a scroll brings a control, not what is in sight: a checkbox that
// already stands whole in sight inside the padded strip at the box's top, between the head's
// bottom and the padding's edge, or inside the one at its bottom, is not moved.
test("a landing on a control already whole in sight inside a box's scroll padding scrolls nothing", async () => {
  const padded = { "scroll-padding-bottom": "64px", "scroll-padding-top": "64px" };
  expect({
    "in the bottom strip": await landedInBox(450, padded),
    "in the top strip": await landedInBox(150, padded),
  }).toEqual({
    "in the bottom strip": { above: 37, below: 350, box: [], window: [] },
    "in the top strip": { above: 337, below: 50, box: [], window: [] },
  });
});

// Empty space inside a selection bar root whose held press's write is out: clicking the bar's
// `tabindex="-1"` line focuses that line and acts on no control, so it must neither end the held
// press nor replace it with an unheld one; when the bar then goes with the rows, the focus on the
// line is still the press's doing (`causedByReader`). Clicking another unheld control in the bar,
// or empty space outside the bar, still ends the held press as before.
test("a pointer press on empty space inside a held selection bar keeps the press live, while another control or space outside it ends it", async () => {
  const view = render(
    createElement(
      "section",
      ARM_ON_PRESS,
      createElement("div", { "data-testid": "outside-blank" }, "Blank outside bar"),
      createElement(
        "div",
        {
          ...ARM_ON_PRESS,
          "data-control": CONTROL.selectionBar,
          "data-testid": "bar",
          role: "toolbar",
          tabIndex: -1,
        },
        createElement(
          "div",
          { "data-testid": "bar-line", tabIndex: -1 },
          createElement("span", null, "2 selected"),
          createElement("button", { type: "button" }, "Take out of epic"),
          createElement("button", { type: "button" }, "Clear selection")
        )
      )
    )
  );
  let release: () => void = () => undefined;
  try {
    const bar = view.getByTestId("bar");
    const line = view.getByTestId("bar-line");
    const takeOut = view.getByRole("button", { name: "Take out of epic" });
    const clear = view.getByRole("button", { name: "Clear selection" });
    const outside = view.getByTestId("outside-blank");
    const pressHeld = () => {
      release();
      disarm();
      act(() => {
        takeOut.focus();
        fireEvent.click(takeOut);
      });
      release = holdHandOff();
    };
    const pointerClick = (target: HTMLElement) => {
      act(() => {
        target.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
        if (target.hasAttribute("tabindex") || target.tagName === "BUTTON") target.focus();
        target.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
        fireEvent.click(target);
      });
    };

    // Empty space inside the bar's line: keeps the held press live past frames and tasks, notes the
    // line inside the bar, and does not count as the armed control itself (`pressIsArmedOwn`).
    pressHeld();
    pointerClick(line);
    expect(document.activeElement === line).toBe(true);
    expect(pressIsArmedOwn(line)).toBe(false);
    for (const _ of [0, 1]) await frameAndTasks();
    expect(pressArmed()).toBe(true);
    expect(causedByReader(line)).toBe(true);
    // Removing the bar with focus on the line still reports the loss as caused by the reader.
    bar.remove();
    expect(causedByReader(line)).toBe(true);

    // Re-attach bar and verify that another unheld control inside it ends the held press (replaced
    // by an unheld press that expires after frames and tasks).
    view.container.firstElementChild?.append(bar);
    pressHeld();
    pointerClick(clear);
    for (const _ of [0, 1]) await frameAndTasks();
    expect(pressArmed()).toBe(false);

    // Empty space outside the bar (inside the outer section) also ends the held press.
    pressHeld();
    pointerClick(outside);
    for (const _ of [0, 1]) await frameAndTasks();
    expect(pressArmed()).toBe(false);
  } finally {
    release();
    disarm();
    view.unmount();
  }
});
