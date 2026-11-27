/**
 * What the reader does to the page, and whether their own scroll is still moving it.
 *
 * Two watches keep a reader's place while a document is drawn around them: the keepers
 * (`keepTheReadersPlace` and `keepWhatStandsUnder`, `readers-place.ts`), which answer a growth
 * above the reader in the task it is seen in, and the landing correction and the swap's
 * put-back (`refocusWhenSettled`, `refocus-when-settled.ts`), which put the reader back on a
 * target in the frame after the drawing around it moved. Both must let go of the reader the
 * moment the reader moves on, and only then: an input means the reader has moved on when it
 * moves their place or their focus, and not otherwise. This module is the one answer to that,
 * and it reads and writes no scroll.
 *
 * The answer is a table (`THE_READERS_INPUTS`) rather than a rule per watch, because a rule
 * written for one kind of input at a time missed the next kind each round: the page's own
 * letter shortcuts, which move the focus or switch a tab, read as letters typed; a tap, which
 * moves nothing, read as a drag. Every input that reaches the page is an entry, with one of
 * three effects, and both watches listen for exactly the events the table names and read
 * nothing else.
 */

import { appKeymap, isEditableTarget, type ShortcutEffect } from "../shell/keymap";

/** What an input does to the reader: moves their place or their focus, moves neither, or cannot
 *  be told at its start - a finger going down is a tap or the start of a drag - and is read at
 *  the first sign that it has begun to move them, which is an entry of its own. A key the page
 *  binds declares one of the first two where it is bound (`KeyBinding.effect`). */
export type WhatItDoes = ShortcutEffect | "unknown until it moves";

/** The events the page hears the reader's inputs by. */
type ReadersEvent = "keydown" | "pointerdown" | "scroll" | "touchmove" | "touchstart" | "wheel";

/** One input that reaches the page, and what it does to the reader. */
interface ReadersInput {
  /** The input, as a reader would name it. */
  readonly input: string;
  /** The event the page hears it by. */
  readonly heard: ReadersEvent;
  /** Whether `event`, heard as above, is this input. The first entry that answers yes is the
   *  one an event is read by, so an entry stands before the wider one it narrows. */
  readonly is: (event: Event) => boolean;
  readonly does: WhatItDoes;
}

/** Every entry of one event is this input. */
const always = (): boolean => true;

/** The key a keyboard reported, or `undefined` for a `keydown` that names none. */
function keyOf(event: Event): string | undefined {
  return event instanceof KeyboardEvent ? event.key : undefined;
}

/** Whether a scroll is the page's own: its box stands where the page's own place-keeping last
 *  left it - a keeper's correction, a placement, a put-back, or a landing correction's focus,
 *  which the Proof editor scrolls and the landing records after it - which only that record can
 *  answer (`thePageLeftItThere` and `thePageLeftIt`, `readers-place.ts`, which tells the table
 *  so when it loads). A held press whose box the page itself moves has not moved anybody. The
 *  browser's own scroll anchoring leaves no record: the landing correction reads it for itself
 *  before it asks the table (`scrolled()`, `refocus-when-settled.ts`), and the keepers, whose
 *  window it opens while a press is held, take the place afresh from where it left them. */
let thePagesOwnScroll: (scroll: Event) => boolean = () => false;

/** Tells the table how to know the page's own scroll from a press's. */
export function knowThePagesOwnScroll(test: (scroll: Event) => boolean): void {
  thePagesOwnScroll = test;
}

/** Whether a pointer is held down on the page: from its `pointerdown` to its `pointerup` or
 *  `pointercancel`, for as long as a watch is on the page to hear both (`followThePress`). A
 *  press is an input whose effect is unknown at its start, and a scroll while it is held is the
 *  first sign that it has moved the reader. */
let aPressIsHeld = false;
let watchesFollowingThePress = 0;

function pressed(): void {
  aPressIsHeld = true;
}

function letGo(): void {
  aPressIsHeld = false;
}

/** Follows the reader's presses for as long as the watch that calls it stands, and returns the
 *  dispose: what the table reads a scroll by (`THE_READERS_INPUTS`). `capture`, so a press is
 *  heard wherever on the page it lands; `passive`, because this moves nothing. */
export function followThePress(): () => void {
  watchesFollowingThePress += 1;
  if (watchesFollowingThePress === 1) {
    window.addEventListener("pointerdown", pressed, { capture: true, passive: true });
    window.addEventListener("pointerup", letGo, { capture: true, passive: true });
    window.addEventListener("pointercancel", letGo, { capture: true, passive: true });
  }
  let following = true;
  return () => {
    if (!following) {
      return;
    }
    following = false;
    watchesFollowingThePress -= 1;
    if (watchesFollowingThePress > 0) {
      return;
    }
    window.removeEventListener("pointerdown", pressed, true);
    window.removeEventListener("pointerup", letGo, true);
    window.removeEventListener("pointercancel", letGo, true);
    aPressIsHeld = false;
  };
}

/** The keys whose own default action scrolls the box the reader is in: Space, the arrows, Home
 *  and End. A field takes them for itself, and so does a control Space presses. */
const KEYS_THAT_SCROLL: Record<string, true> = {
  " ": true,
  ArrowDown: true,
  ArrowLeft: true,
  ArrowRight: true,
  ArrowUp: true,
  End: true,
  Home: true,
};

/** The `<input>` types Space presses or ticks rather than scrolling past: the button family, a
 *  checkbox and a radio. A `<button>` is the other control it presses. */
const INPUTS_SPACE_PRESSES: Record<string, true> = {
  button: true,
  checkbox: true,
  image: true,
  radio: true,
  reset: true,
  submit: true,
};

/**
 * Every input that reaches the page, and what it does to the reader. Read top to bottom: the
 * first entry an event is is the one it is read by (`whatItDoes`).
 *
 * The pointer and the finger. A press of a mouse, a pen or a finger cannot be told at its
 * start: a click on a margin button, a checkbox or empty space scrolls nothing, and a tap moves
 * nothing. A press that stood the keepers down left a growth in its frames on the reader: the
 * tester's row with a `touchstart` in place of the key read 1,250 where it expects 1,582. So a
 * press waits for its first movement. A finger's is its first `touchmove`, a drag; a mouse's or
 * a pen's is a scroll while it is held down, a drag on the scroll bar or a selection dragged past
 * the box's edge (`followThePress`), and not a scroll the page made itself while it was held
 * (`thePagesOwnScroll`). A landing correction with a document of its own asks this of a scroll
 * only once that scroll is none of its own readings - its correction, a keeper's write, the
 * browser's scroll anchoring (`scrolled()`, `refocus-when-settled.ts`). What a press does to
 * the focus is the landing correction's own stop - a focus moving out of the document, or a
 * selection changing inside it - and moves nobody's place for the keepers to give up. A wheel or
 * a trackpad's stroke asks to scroll; held with Control it zooms, and at the end of a page it
 * scrolls nothing, which the keepers learn when their window closes having seen no scroll
 * (`watchTheReadersScroll`).
 *
 * Whether a script's write cuts a press's drag was measured on this pod on a probe page, three
 * runs an arm, the offset read every frame: the drag alone; the drag with one pixel written one
 * frame after its first scroll, as a keeper would write it; and the drag under the keepers as
 * shipped, with one pixel growing above the reader in that frame. In Playwright's WebKit 27.2 a
 * mouse drag of the page's own scroll bar - drawn as a classic bar through `::-webkit-scrollbar`,
 * since headless WPE's overlay bar takes no press - ended at 4,997 px in 3 runs of 3 alone and in
 * 3 of 3 with the write, the thumb following the pointer past it. In Chromium 156 a touch drag
 * sent through `Input.dispatchTouchEvent` and held still before it lifted ended 285 px down in 3
 * runs of 3 alone and 286 px in 3 of 3 with the write, which it carried along; lifted while
 * moving, it flung on to 332 to 373 px in 6 runs of 8 alone and to 346 to 363 px in 7 of 8 with
 * the write. A write between the press and its first movement, which the keepers may still make,
 * cut neither: the drag ended at 4,997 px in WebKit and 286 px down in Chromium, 3 runs of 3 each.
 * Under the keepers as shipped, which answered the same growth with no input by a one-pixel write
 * each time, the keepers had stood down when the growth landed and wrote nothing in 12 runs of
 * 12, 6 in each engine, and the drags ran their length: to 4,978 to 4,998 px in WebKit, where the
 * growth with no keeper left them too, 285 px when held in Chromium, and into a fling in 3 swipes
 * of 3. Neither a scroll-bar drag in WebKit nor a touch drag in Chromium was cut by a one-pixel
 * write, so for those two inputs the stand-down at a press's first movement is measured to cost
 * nothing and to protect nothing. Why a press is unknown until it moves is the other half: a
 * press or a tap that moves nothing must not stand the keepers down (above), which is what the
 * table's entries for `pointerdown` and `touchstart` hold. WebKit's own touch drag could not be
 * driven here, since Playwright's WebKit sends no touch drag.
 *
 * Any other scroll is none of the reader's inputs, and no entry: the page's own place-keeping,
 * the browser's scroll anchoring, a find-in-page jump, an assistive tool. Each watch reads it for
 * itself as the place the reader now has - the keepers take the place afresh and correct
 * nothing for it, and the landing correction ends where it did not make it (`scrolled()`,
 * `refocus-when-settled.ts`).
 *
 * The keys. A key the page binds is read by its binding, which says what it does where it is
 * bound: `s`, `p` and `e` focus a control of the issue's header, which scrolls the page to it,
 * and a tab chord switches the tab, so a reader who pressed one has moved on; a digit that sets
 * the priority or the key that pins the issue writes without moving anybody. The first key of a
 * chord moves nothing, since the registry holds it for the key that completes the chord. A key
 * the page does not bind is read by its browser default. Tab takes the focus to the next thing
 * that holds one and the page to wherever that stands, and PageUp and PageDown page whatever box
 * they are pressed in: from an editable box PageDown moved the page a screenful in one frame in
 * both engines (689 px in WebKit, 752 px in Chromium). Space, an arrow, Home and End scroll the
 * box the reader is in, except where something takes them for itself: a field moves its caret or
 * steps its options (`isEditableTarget`, `shell/keymap.ts`: a text box, the editor, a select),
 * and from an editable box each moved the page by 0 px in both engines; Space presses a button
 * or ticks a checkbox or a radio, and scrolls nothing. A checkbox, a radio or a button is not a
 * field: from a checkbox or an `input[type=button]` the arrows, Home and End scroll the page in
 * both engines, and from a radio Home and End do (the review of round 11, on a probe page), where
 * the arrows step the group's checked option and the focus with it. Enter on a link follows it.
 * Every other key moves nothing: a modifier, a letter, a digit, a copy, Enter and Escape.
 *
 * A key that scrolls is a wheel's scroll under another name, and gets a wheel's stand-down.
 * Measured on this pod on a probe page, eighteen presses on the page in each engine: in
 * Playwright's WebKit 27.2 PageDown and Space ran 576 px, an arrow 80 px, End 26,419 px and Home
 * 4,000 px, each a scroll the engine animates over 5 to 13 frames, and one pixel written by a
 * script two frames in ended each where it stood - PageDown after 92 to 110 px, Space after 56
 * to 79, the arrow after 31 to 54, End after 1,890 to 7,183 and Home after 552 to 1,315.
 * Chromium 156 animates the same keys over 8 to 10 frames and carries the write along instead,
 * ending one pixel further: the stand-down is for the engine that needs it, and costs the other
 * nothing.
 *
 * A watch that took every key for the reader moving gave the page up to nobody: with Shift
 * pressed as a panel landed above a reader, the keepers stood down and the reader's words moved
 * the panel's 88 px in 3 runs of 6 (`e2e/large-documents-first-screen.e2e.ts`, scroll anchoring
 * off); pressed while the editor was built over the draw, it ended the put-back and left the
 * reader 924.6 px from their place in 5 runs of 8 (`e2e/large-documents-build.e2e.ts`). With the
 * key read, the panel row moved the reader's words 0.00 to 0.48 px in 18 runs of 18 - no key,
 * Shift every 30 ms and every 120 ms, a copy every 30 ms, Shift held, and a `keydown` of Shift
 * every 4 ms, the last two of which moved them 88 px in 5 runs of 6 before - and the build left a
 * reader who pressed Shift through it 0.1 to 0.7 px from their place in 6 runs of 6.
 */
export const THE_READERS_INPUTS: readonly ReadersInput[] = [
  {
    does: "unknown until it moves",
    heard: "pointerdown",
    input: "a press of a mouse, a pen or a finger, which may scroll nothing at all",
    is: always,
  },
  {
    does: "moves the reader",
    heard: "scroll",
    input: "a scroll while a press is held: a drag on the scroll bar, a selection dragged on",
    is: (event) => aPressIsHeld && !thePagesOwnScroll(event),
  },
  {
    does: "unknown until it moves",
    heard: "touchstart",
    input: "a finger going down, which is a tap or the start of a drag",
    is: always,
  },
  {
    does: "moves the reader",
    heard: "touchmove",
    input: "a finger moving: a drag, a swipe, a pinch",
    is: always,
  },
  {
    does: "moves the reader",
    heard: "wheel",
    input: "a wheel turned or a trackpad stroked, with Control held to zoom or not",
    is: always,
  },
  {
    does: "moves nothing",
    heard: "keydown",
    input: "a keydown no keyboard made, which a script dispatched with no key to name",
    is: (event) => keyOf(event) === undefined,
  },
  {
    does: "moves the reader",
    heard: "keydown",
    input: "a key the page binds to move the reader: a focus, a tab, a dialog, another page",
    is: (event) =>
      event instanceof KeyboardEvent && appKeymap.effectOf(event) === "moves the reader",
  },
  {
    does: "moves nothing",
    heard: "keydown",
    input: "a key the page binds to move nothing, and the first key of a chord",
    is: (event) => event instanceof KeyboardEvent && appKeymap.effectOf(event) === "moves nothing",
  },
  {
    does: "moves the reader",
    heard: "keydown",
    input: "Tab, which takes the focus to the next thing that holds one",
    is: (event) => keyOf(event) === "Tab",
  },
  {
    does: "moves the reader",
    heard: "keydown",
    input: "PageUp or PageDown, which page whatever box they are pressed in",
    is: (event) => keyOf(event) === "PageUp" || keyOf(event) === "PageDown",
  },
  {
    does: "moves the reader",
    heard: "keydown",
    input: "Enter on a link, which follows it",
    is: (event) =>
      keyOf(event) === "Enter" &&
      event.target instanceof HTMLAnchorElement &&
      event.target.href !== "",
  },
  {
    does: "moves nothing",
    heard: "keydown",
    input: "Space, an arrow, Home or End in a field, which moves its caret or steps its options",
    is: (event) => KEYS_THAT_SCROLL[keyOf(event) ?? ""] === true && isEditableTarget(event.target),
  },
  {
    does: "moves nothing",
    heard: "keydown",
    input: "Space on a button, a checkbox or a radio, which presses or ticks it",
    is: (event) =>
      keyOf(event) === " " &&
      (event.target instanceof HTMLButtonElement ||
        (event.target instanceof HTMLInputElement &&
          INPUTS_SPACE_PRESSES[event.target.type] === true)),
  },
  {
    does: "moves the reader",
    heard: "keydown",
    input: "Space, an arrow, Home or End anywhere else: the page, a link, a checkbox, a radio",
    is: (event) => KEYS_THAT_SCROLL[keyOf(event) ?? ""] === true,
  },
  {
    does: "moves nothing",
    heard: "keydown",
    input: "every other key: a modifier, a letter, a digit, a copy, Enter, Escape",
    is: always,
  },
];

/** The events both watches listen for: every one the table reads an input by, and no other. */
export const THE_READERS_EVENTS: readonly ReadersEvent[] = [
  ...new Set(THE_READERS_INPUTS.map((entry) => entry.heard)),
];

/** What `event` does to the reader, by the first entry of the table it is. An event the table
 *  names no entry for is none of the reader's inputs, and moves nothing. */
export function whatItDoes(event: Event): WhatItDoes {
  for (const entry of THE_READERS_INPUTS) {
    if (entry.heard === event.type && entry.is(event)) {
      return entry.does;
    }
  }
  return "moves nothing";
}

/** Whether `event` moves the reader: the one question both watches ask of an input. One whose
 *  effect is unknown until it moves does not, at its start; its first sign of moving is an entry
 *  of its own, which does. */
export function movesTheReader(event: Event): boolean {
  return whatItDoes(event) === "moves the reader";
}

/**
 * How many frames in a row with no scroll anywhere on the page end the reader's own scroll. An
 * engine that animates the scroll a wheel asks for - WebKit does, over five to twelve frames -
 * reports a scroll in the frame the wheel arrives in and in every frame after it until the
 * movement ends. Measured on this pod in Playwright's WebKit, 24 wheels of 300 and 2,000 px with
 * the main thread idle or busy for 12 or 40 ms of each frame: the first scroll came in the
 * wheel's own frame every time, and none of the 221 frames from a wheel's first scroll to its
 * last went without one. A key's scroll reads the same: of 18 presses in WebKit the first scroll
 * came in the key's own frame 16 times and in the next one twice, and none of the 173 frames
 * from a key's first scroll to its last went without one. One still frame is the movement over;
 * the second is room for a frame an engine skips.
 */
const STILL_FRAMES = 2;

/** The reader's own scroll while it may still be moving the page: from the input that began it
 *  until the page has gone `STILL_FRAMES` frames without a scroll. `moved` is whether this frame
 *  has seen an input or a scroll, and `scrolled` whether any scroll came since it began.
 *  `undefined` while the reader is not scrolling. */
let readersScroll: { frame: number; moved: boolean; scrolled: boolean; still: number } | undefined;

/** The keepers on the page, each told when the reader's scroll is over and whether it scrolled
 *  anything. The listeners that watch for that scroll stand while one of them does, and so does
 *  the following of the reader's press (`followThePress`), which they release with the last. */
const keepers = new Set<(scrolled: boolean) => void>();
let releaseThePress: (() => void) | undefined;

/** Whether the reader's own scroll may still be moving the page, which a keeper's write would
 *  end in the engine that animates it: the keepers write nothing while it is. */
export function theReaderIsScrolling(): boolean {
  return readersScroll !== undefined;
}

/** The reader has done something, or a box on the page has scrolled. An input that moves the
 *  reader begins their scroll, or keeps it moving; one that moves nothing, or cannot tell yet,
 *  leaves the keepers at work. A scroll during the reader's own is that scroll moving on, and a
 *  scroll while a press is held is the press's first movement (`THE_READERS_INPUTS`). */
function theReaderActs(event: Event): void {
  const scrolled = event.type === "scroll";
  if (readersScroll !== undefined) {
    if (scrolled || movesTheReader(event)) {
      readersScroll.moved = true;
      readersScroll.scrolled ||= scrolled;
    }
    return;
  }
  if (!movesTheReader(event)) {
    return;
  }
  readersScroll = {
    frame: window.requestAnimationFrame(aFrameOfTheirScroll),
    moved: true,
    scrolled,
    still: 0,
  };
}

/** A frame of the reader's own scroll: the scroll is over once `STILL_FRAMES` of them in a row
 *  have passed with no scroll, and every keeper is told whether it scrolled anything. */
function aFrameOfTheirScroll(): void {
  const scroll = readersScroll;
  if (scroll === undefined) {
    return;
  }
  scroll.still = scroll.moved ? 0 : scroll.still + 1;
  scroll.moved = false;
  if (scroll.still < STILL_FRAMES) {
    scroll.frame = window.requestAnimationFrame(aFrameOfTheirScroll);
    return;
  }
  readersScroll = undefined;
  for (const keeper of [...keepers]) {
    keeper(scroll.scrolled);
  }
}

/**
 * Watches for the reader's own scroll for as long as the keeper that calls it stands, and
 * returns the dispose. `over` is called each time that scroll ends, with whether it scrolled
 * anything: a scroll that did moved the reader, and the keeper takes their place afresh from
 * where it left them; one that scrolled nothing - a wheel at the end of the page, a zoom, Tab to
 * a control already on screen, PageDown at the end - moved nobody, and the keeper answers
 * whatever grew meanwhile against the place it held before the input, as if the input had never
 * come.
 *
 * `capture`, because a column's scroll does not bubble, and because a key is read as the
 * registry stands before its own listener takes the key and moves a chord on (`effectOf`);
 * `passive`, because this moves nothing and must not hold up the scroll it watches.
 */
export function watchTheReadersScroll(over: (scrolled: boolean) => void): () => void {
  // A function of its own, so a keeper that passes the same one twice is two keepers.
  const keeper = (scrolled: boolean) => over(scrolled);
  keepers.add(keeper);
  if (keepers.size === 1) {
    for (const type of THE_READERS_EVENTS) {
      window.addEventListener(type, theReaderActs, { capture: true, passive: true });
    }
    releaseThePress = followThePress();
  }
  return () => {
    if (!keepers.delete(keeper) || keepers.size > 0) {
      return;
    }
    for (const type of THE_READERS_EVENTS) {
      window.removeEventListener(type, theReaderActs, true);
    }
    releaseThePress?.();
    releaseThePress = undefined;
    if (readersScroll !== undefined) {
      window.cancelAnimationFrame(readersScroll.frame);
    }
    readersScroll = undefined;
  };
}
