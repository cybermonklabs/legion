import { type RefObject, useEffect } from "react";

/** What can hold the focus inside Details: the controls a reader reaches, and any element given a
 *  `tabindex`. */
const CONTROLS = "a[href], button, input, select, textarea, [tabindex]";

/** The `overflow` values that cut what an element draws down to its own box. */
const CLIPPING: Readonly<Record<string, true>> = {
  auto: true,
  clip: true,
  hidden: true,
  scroll: true,
};

/** The `overflow` values of a box that scrolls its content. */
const SCROLLING: Readonly<Record<string, true>> = {
  auto: true,
  scroll: true,
};

/** The `position` values that pin an element to the screen or to the box it scrolls in, so that
 *  it stands in sight, at no distance, wherever the reader has scrolled to: the drawer's resize
 *  handle, as tall as the screen, for one. */
const PINNED: Readonly<Record<string, true>> = {
  fixed: true,
  sticky: true,
};

/** What the keeper hears: the two attributes that take a control from the reader, and every
 *  element added or removed. */
const WATCHED: MutationObserverInit = {
  attributeFilter: ["disabled", "hidden"],
  attributes: true,
  childList: true,
  subtree: true,
};

/** The events by which the keeper hears the reader press the control that holds the focus
 *  (`watchPresses`): a click, and a key on it. A mouse press focuses a control as its button goes
 *  down, and a tap only once the pointer is up, so a press that gave the control the focus is
 *  heard by its click. */
const PRESSES = ["click", "keydown"] as const;

/** What a press watch hears of its control (`watchPresses`): the attributes by which a control
 *  says the act a press set off is still out - busy while a read it sent is out, taking no press
 *  or disabled while a write it sent is. */
const BUSY: MutationObserverInit = {
  attributeFilter: ["aria-busy", "aria-disabled", "disabled"],
};

/** The attribute each entry of the new issue page's history carries, its own id there, by which
 *  the keeper tells a redrawn control of one entry from a control of another
 *  (`ControlIdentity`). */
export const HISTORY_ENTRY_ATTRIBUTE = "data-history-entry";

/** What tells a control apart across a redraw, where React draws it anew as another element: the
 *  history entry it stands in, its own `id`, its tag, its role and its name, in one key. The lost
 *  control's identity is read from the lost control itself once it is lost, all but its entry,
 *  which is the one it stood in when it took the focus, since a control removed from inside its
 *  entry no longer finds it around it. So a control renamed since it took the focus, as a Pin
 *  that the reader's press or another session's change relabelled Unpin is, is still known as the
 *  same control when it is drawn anew. */
type ControlIdentity = string;

/** A stretch of the window from `top` down to `bottom`, in CSS pixels. */
interface Span {
  readonly bottom: number;
  readonly top: number;
}

/** A rectangle of the window, in CSS pixels. */
interface Bounds extends Span {
  readonly left: number;
  readonly right: number;
}

/** What the keeper reads of an element between a control and the box, once per search: the part
 *  of the window it cuts what it draws down to, where it cuts it (`CLIPPING`), and whether it is
 *  pinned (`PINNED`). */
interface Frame {
  readonly clip: Bounds | null;
  readonly pinned: boolean;
}

/** How the control that held the focus lost it. */
type Loss = "disabled" | "hidden" | "removed";

/**
 * Keeps a keyboard reader's focus inside Details while `active` holds, where the reader took the
 * control that held it away themselves: when that control is removed, turns disabled or is no
 * longer drawn while a press of the reader's on it stands (`watchPresses`), and the focus has
 * fallen to the page, the keeper gives it to another place in `box`. Chromium drops the focus of a
 * button that turns disabled, or is removed, to the body. Without the keeper, a reader whose Retry
 * goes with its line once the read it sent answers, or whose Load older goes away with the log's
 * last page, starts again from the top of the page.
 *
 * The keeper hears the reader's presses on the control that holds the focus (`PRESSES`), and it
 * forgets them once the focus moves to another control. A loss while no press stands - a live
 * update, another session's change that takes a row away, a read that answers on its own, a
 * control or a popover gone - leaves the focus on the page's body, where the page's keys work, and
 * the keeper moves nothing and scrolls nothing. A control drawn in the same change with the lost
 * one's identity (`ControlIdentity`), as when React draws an entry anew under a new key, is the
 * same control and no loss: it is given the focus back whether or not the reader pressed it, and
 * it keeps their press.
 *
 * Where a press stands, the focus goes, in this order: to that control drawn anew; else to the
 * control nearest to where the lost one stood, among those that stay enabled, drawn and in sight
 * and are not pinned (`PINNED`), looked for first inside the box that scrolls the lost one,
 * Details' body, and only where that holds none in the rest of the box; else to the box's
 * heading, its first `h2`.
 *
 * It is a backstop under each site's own hand-on, not a replacement for it. It hears a change
 * through one MutationObserver on the box, and through a `focusout` that names no next element,
 * which it reads a microtask later, and it moves the focus one task after that. A site hands the
 * focus on in an effect of the commit that changed the DOM. A layout effect runs before the
 * observer's callback. A passive effect runs after it when the change came from outside an event,
 * a save that lands, but in a task React queued as the commit ended, before the keeper queued its
 * own. So the site's choice already holds the focus when the keeper moves: the keeper moves only a
 * focus still on the page, or still on the disabled or hidden control itself, which Chromium moves
 * to the page a moment later. A focus the reader puts anywhere else, inside the box or outside it,
 * stays where they put it.
 *
 * It does nothing while `active` is false or the box is not drawn, and nothing while the focus is
 * outside the box: its observer runs only while a control inside the box holds the focus.
 */
export function useDetailsFocusKeeper(box: RefObject<HTMLElement | null>, active: boolean): void {
  useEffect(() => {
    const root = box.current;
    if (!active || root === null) return;
    return keepFocusIn(root);
  }, [active, box]);
}

/** The watch of the reader's presses on one control (`watchPresses`). */
export interface PressWatch<C extends Element = Element> {
  /** The control the watch hears: the one it was made on, or the one it follows since. */
  readonly control: C;
  /** Hears `event`, which is a click or a keydown: a press of the control where it was made on
   *  the control or inside it while the control holds the focus, and no press otherwise. */
  hear(event: Event): void;
  /** Whether a press of the reader's on the control stands, so that its loss now is theirs. */
  stands(): boolean;
  /** Moves the watch, and a press that stands, to `control`: the same control drawn anew. */
  follow(control: C): void;
  /** Ends the watch and forgets the press. */
  stop(): void;
}

/**
 * Hears the reader's pointer presses on `page`, and returns what ends the hearing. It is the
 * page's one reading of a pointer press (decision 404): the header's focus keeper, its watches of
 * a layer and its title row read presses through it (`issue-page/focus-keeper.ts`), and so does
 * each watch of a control's presses here (`watchPresses`), so a press is under way, and over, for
 * each of them on the same events. `begin` is told of each press as its pointer goes down; once
 * the press is over, `done` is told where the focus was as the press began and what `begin`
 * answered. A press is over at the first of these:
 * - Its pointerup, where it has moved the focus by then or is of a button other than the primary
 *   one. A mouse press moves the focus as its button goes down, so a press that has moved it by
 *   its pointerup has done its move then, and a press of any other button sends no click.
 * - Its click. A tap moves the focus only after its pointer is up, from the mousedown the browser
 *   sends it then, so a press of the primary button that has moved no focus by its pointerup is
 *   still under way until its click. That click is heard in the capture phase on the page, before
 *   the target's own handler: the header's Details control cancels its mousedown so the focus
 *   stays where it is, and closes Details from its click, which must not read as the press moving
 *   the focus.
 * - Its pointercancel. A press the browser cancels, as it does a touch it turns into a scroll and
 *   as Chromium does a press it turns into a drag, sends neither a pointerup nor a click.
 * - Its dragend. A press that becomes a drag sends no pointerup and no click, so the drag's end
 *   ends the press where no pointercancel did.
 * - Its contextmenu. Where the menu opens as the button goes down, it takes the pointerup itself.
 * - The blur of the page's window. A press under way as the reader leaves the window may send the
 *   page no release at all. The window keeps its focused element as it blurs, so a press that had
 *   moved no focus by then reads as no move. The blur is heard on the window outside the capture
 *   phase, and only the window's own: a capture listener there hears every element's blur.
 * A new pointerdown replaces a press still under way.
 */
export function hearPresses<T>(
  page: Document,
  begin: (event: PointerEvent) => T,
  done: (from: Element | null, start: T) => void
): () => void {
  /** The press under way: where the focus was as it began, whether it is of the primary button,
   *  and what `begin` answered; null while no press is under way. */
  let press: {
    readonly from: Element | null;
    readonly primary: boolean;
    readonly start: T;
  } | null = null;
  const notePress = (event: PointerEvent) => {
    // A touch and a pen press as the primary button does.
    press = { from: page.activeElement, primary: event.button === 0, start: begin(event) };
  };
  /** Ends the press under way, if any, and tells `done` of it. */
  const endPress = () => {
    if (press === null) return;
    const { from, start } = press;
    press = null;
    done(from, start);
  };
  const notePointerUp = () => {
    // No click follows a press of any button but the primary one, so such a press ends here
    // whether or not it moved the focus.
    if (press !== null && (!press.primary || page.activeElement !== press.from)) endPress();
  };
  const view = page.defaultView;
  page.addEventListener("pointerdown", notePress, true);
  page.addEventListener("pointerup", notePointerUp, true);
  page.addEventListener("pointercancel", endPress, true);
  page.addEventListener("dragend", endPress, true);
  page.addEventListener("contextmenu", endPress, true);
  page.addEventListener("click", endPress, true);
  view?.addEventListener("blur", endPress);
  return () => {
    page.removeEventListener("pointerdown", notePress, true);
    page.removeEventListener("pointerup", notePointerUp, true);
    page.removeEventListener("pointercancel", endPress, true);
    page.removeEventListener("dragend", endPress, true);
    page.removeEventListener("contextmenu", endPress, true);
    page.removeEventListener("click", endPress, true);
    view?.removeEventListener("blur", endPress);
  };
}

/**
 * Watches the reader's presses on `control`, the control that holds the focus, by the one rule
 * every hand-on of Details follows. A loss of the focus the reader caused - a loss of the control
 * while a press of theirs on it stands - is handed on inside its region. A loss they did not
 * cause leaves the focus on the page's body, and nothing scrolls.
 *
 * Only an act on the control is a press: a click, which is a pointer's click, or Enter or Space
 * on a button, or Enter on a link, which the browser turns into a click; or a key the control
 * handles as its act. A handler takes such a key, so its default is prevented once the page's
 * handlers have run. React runs them at the app's root, so the watch reads that only once the
 * key's dispatch is over. A key that acts on nothing - ArrowDown, PageDown and every other key the
 * reader reads by, which scroll Details - is no press. A pointer press on the control stands as
 * its act from its pointerdown, while it is under way by the page's one reading of a pointer press
 * (`hearPresses`), which the header's keeper reads too: the write the press starts may take the
 * control away before its click. One that ends with no click, as a touch the browser turns into a
 * scroll does, acted on nothing, and nothing of it stands once it is over.
 *
 * A press stands until it is answered while the control still stands, on the page and not hidden.
 * Where the control was busy as it was pressed, or turned busy since - `aria-busy` while a read it
 * sent is out, `aria-disabled` or `disabled` while a write is - the press is answered as soon as
 * the control is neither busy nor disabled again. Where the act made it busy in none of those
 * ways, the press is answered once the commit that answers the act has drawn, two frames after
 * the press. So a loss while the act is out - in the commit the press made, while the control is
 * busy, or as the act lands - is the reader's, and a loss after the press was answered is not: a
 * Retry whose read failed again stands answered, and a read that answers on its own later and
 * takes it away leaves the focus on the page's body. A press belongs to the control it was made
 * on, and the holder of the watch stops it, forgetting the press, once the focus moves to another
 * control.
 */
export function watchPresses<C extends Element>(control: C): PressWatch<C> {
  let watched = control;
  // A click heard, or a key a handler took, that no answer has met yet.
  let acted = false;
  // The last key heard, an act only where a handler took it. It is read once its dispatch is
  // over: by the next press, or by `framesDrawn`, and until then by `stands`.
  let key: Event | null = null;
  // Whether the control has been busy or disabled since the press, so that its standing idle
  // again answers the press.
  let tookBusy = false;
  // The animation frame the last press waits for.
  let frame: number | null = null;
  // Whether a pointer press on the control is under way (`hearPresses`), from its pointerdown to
  // its end. Its click, where it ends with one, is an act the watch hears (`hear`).
  let pointing = false;
  const stopPointing = hearPresses(
    control.ownerDocument,
    (event) => {
      // A new pointerdown replaces a press still under way, wherever it falls.
      pointing =
        document.activeElement === watched &&
        event.target instanceof Node &&
        watched.contains(event.target);
    },
    () => {
      pointing = false;
    }
  );
  const observer = new MutationObserver(() => {
    if (stands() && (tookBusy || busy(watched))) answerIfIdle();
  });
  observer.observe(control, BUSY);

  function stands(): boolean {
    return acted || key?.defaultPrevented === true || pointing;
  }

  function settleKey(): void {
    if (key?.defaultPrevented === true) acted = true;
    key = null;
  }

  function answer(): void {
    acted = false;
    key = null;
    tookBusy = false;
    if (frame !== null) window.cancelAnimationFrame(frame);
    frame = null;
  }

  // Answers the press where the control stands idle where the reader pressed it, on the page and
  // not hidden; a control still busy is noted, and its going idle answers the press (`observer`).
  function answerIfIdle(): void {
    if (busy(watched)) tookBusy = true;
    else if (watched.isConnected && watched.closest("[hidden]") === null) answer();
  }

  // Runs two frames after the last press, when the commit its act made - in the press's own event
  // or in a task that event queued - has drawn.
  function framesDrawn(): void {
    frame = null;
    settleKey();
    if (acted) answerIfIdle();
  }

  return {
    get control() {
      return watched;
    },
    follow(next) {
      watched = next;
      observer.disconnect();
      observer.observe(watched, BUSY);
    },
    hear(event) {
      const { target, type } = event;
      if (
        document.activeElement !== watched ||
        !(target instanceof Node) ||
        !watched.contains(target)
      ) {
        return;
      }
      settleKey();
      if (!acted) tookBusy = false;
      if (type === "click") acted = true;
      else key = event;
      if (busy(watched)) tookBusy = true;
      if (frame !== null) window.cancelAnimationFrame(frame);
      frame = window.requestAnimationFrame(() => {
        frame = window.requestAnimationFrame(framesDrawn);
      });
    },
    stands,
    stop() {
      answer();
      observer.disconnect();
      stopPointing();
    },
  };
}

/** Whether `control` says an act is still out on it (`BUSY`). */
function busy(control: Element): boolean {
  return (
    control.getAttribute("aria-busy") === "true" ||
    control.getAttribute("aria-disabled") === "true" ||
    control.matches(":disabled")
  );
}

/** Keeps the focus inside `root` (`useDetailsFocusKeeper`) until the returned function is called. */
function keepFocusIn(root: HTMLElement): () => void {
  // The control inside the box that holds the focus: the watch of the reader's presses on it,
  // which names it, and the history entry it stood in when it took the focus (`ControlIdentity`),
  // which a control removed from inside its entry no longer finds around it (`entryOf`). Its loss
  // while a press stands is the reader's, and the keeper hands the focus on.
  let held: { readonly entry: string | null; readonly press: PressWatch<HTMLElement> } | null =
    null;
  // The move the keeper makes a task after the held control was lost: every record heard since,
  // and the timer that makes it.
  let pending: { readonly records: MutationRecord[]; readonly timer: number } | null = null;
  const observer = new MutationObserver((records) => keep(records, false));
  const listening = new AbortController();

  function note(target: EventTarget | null): void {
    // The box itself holding the focus - the phone's sheet - leaves no control to keep.
    if (!(target instanceof HTMLElement) || target === root) {
      forget();
      return;
    }
    cancel();
    // The focus come back to the control that held it, as the window is focused again or as the
    // keeper gives the control drawn anew the focus (`move`), has not moved to another control,
    // so the reader's press on it still stands.
    if (held?.press.control !== target) {
      held?.press.stop();
      held = { entry: entryOf(target), press: watchPresses(target) };
    }
    observer.observe(root, WATCHED);
  }

  function hear(event: Event): void {
    held?.press.hear(event);
  }

  function forget(): void {
    cancel();
    held?.press.stop();
    held = null;
    observer.disconnect();
  }

  function cancel(): void {
    if (pending === null) return;
    window.clearTimeout(pending.timer);
    pending = null;
  }

  // `measure` reads whether the held control is still drawn, a layout read the observer makes only
  // when a `hidden` changed around it.
  function keep(records: readonly MutationRecord[], measure: boolean): void {
    const focus = document.activeElement;
    const onPage = focus === null || focus === document.body;
    if (!onPage && !root.contains(focus)) {
      forget();
      return;
    }
    if (held === null) return;
    const { control } = held.press;
    const loss = lossOf(
      control,
      root,
      records,
      measure ||
        records.some(
          (record) => record.attributeName === "hidden" && record.target.contains(control)
        )
    );
    if (loss === null) {
      // The control stands and the focus is on the page: the reader put it there.
      if (onPage) forget();
      return;
    }
    // Off the page, the focus is the keeper's to move only while it is still on the disabled or
    // hidden control itself; a focus a site or the reader put anywhere else stays.
    if (!onPage && (focus !== control || loss === "removed")) return;
    // A site's passive effect, where the change came from outside an event, runs after this, in a
    // task React queued as the commit ended: the keeper moves in a task of its own, queued behind
    // that one, with every record it heard until then.
    if (pending === null) {
      pending = { records: [...records], timer: window.setTimeout(move, 0) };
    } else {
      pending.records.push(...records);
    }
  }

  function move(): void {
    if (pending === null || held === null) return;
    const { records } = pending;
    pending = null;
    const { entry, press } = held;
    const { control } = press;
    // A focus a site's passive effect or the reader put somewhere since stays where they put it,
    // and so does one still on the lost control once it stands again.
    const focus = document.activeElement;
    if (
      focus !== null &&
      focus !== document.body &&
      (focus !== control || lossOf(control, root, [], true) === null)
    ) {
      return;
    }
    if (!drawn(root)) return;
    const removal = removalOf(control, records);
    const stood = placeOf(control, root, removal);
    const replacement = replacementOf(identityOf(control, entry), root, records, stood);
    const target =
      replacement ??
      (press.stands()
        ? (nearestTo(stood, root, scrollerOf(removal?.target ?? control.parentElement, root)) ??
          headingOf(root))
        : null);
    if (target === null) {
      // No press of the reader's that stands took the control away: the focus stays where the
      // browser put it, on the page's body, and the keeper lets the control go.
      forget();
      return;
    }
    if (target === replacement) {
      // The control drawn anew is the lost one: it takes the watch over, so a press the reader
      // made on the lost one stands on it, and the focus it is given comes back to the control
      // the keeper holds (`note`).
      press.follow(replacement);
    }
    target.focus({ preventScroll: true });
  }

  for (const type of PRESSES) {
    root.addEventListener(type, hear, { capture: true, signal: listening.signal });
  }

  root.addEventListener("focusin", (event) => note(event.target), { signal: listening.signal });
  root.addEventListener(
    "focusout",
    (event) => {
      const next = event.relatedTarget;
      if (next instanceof Node && root.contains(next)) return;
      if (next !== null) {
        forget();
        return;
      }
      // Read once the change that blurred it is done; the observer's records not yet delivered
      // are read with it, so a removed control's place is known here too.
      queueMicrotask(() => {
        if (held !== null) keep(observer.takeRecords(), true);
      });
    },
    { signal: listening.signal }
  );
  // A focus that came into the box before the keeper listened: the drawer's own Close, which the
  // commit that opens Details focuses.
  if (root.contains(document.activeElement)) note(document.activeElement);
  return () => {
    listening.abort();
    forget();
  };
}

function lossOf(
  control: HTMLElement,
  root: HTMLElement,
  records: readonly MutationRecord[],
  measure: boolean
): Loss | null {
  if (!root.contains(control)) return "removed";
  if (control.matches(":disabled")) return "disabled";
  if (measure && !drawn(control)) return "hidden";
  // Taken out and put back, as a keyed list drawn in a new order moves its elements: Chromium
  // drops the focus of a moved element as it does a removed one's.
  return removalOf(control, records) === undefined ? null : "removed";
}

function drawn(element: Element): boolean {
  return element.getClientRects().length > 0;
}

/** The last record that took `control`, or an element around it, out of the DOM. */
function removalOf(control: Node, records: readonly MutationRecord[]): MutationRecord | undefined {
  let removal: MutationRecord | undefined;
  for (const record of records) {
    for (const removed of record.removedNodes) {
      if (removed.contains(control)) {
        removal = record;
        break;
      }
    }
  }
  return removal;
}

/** The identity of `control` (`ControlIdentity`) in the history entry `entry`: its own `id`, tag,
 *  role and name as they stand now. */
function identityOf(control: HTMLElement, entry: string | null): ControlIdentity {
  return JSON.stringify([
    entry,
    control.id,
    control.tagName,
    control.getAttribute("role"),
    control.getAttribute("aria-label") ?? (control.textContent ?? "").trim(),
  ]);
}

/** The id of the history entry `control` stands in; null outside every entry. */
function entryOf(control: Element): string | null {
  return (
    control.closest(`[${HISTORY_ENTRY_ATTRIBUTE}]`)?.getAttribute(HISTORY_ENTRY_ATTRIBUTE) ?? null
  );
}

/** Where the lost control stood. A removed one's place is the gap its `removal` left between the
 *  drawn elements beside it; a disabled one is still drawn where it was; a hidden one stands where
 *  the outermost hidden element around it does among its siblings. */
function placeOf(
  control: HTMLElement,
  root: HTMLElement,
  removal: MutationRecord | undefined
): Span | null {
  if (removal !== undefined) {
    return between(removal.previousSibling, removal.nextSibling, removal.target, root);
  }
  if (!root.contains(control)) return null;
  if (drawn(control)) return control.getBoundingClientRect();
  let outer: Element = control;
  while (
    outer.parentElement !== null &&
    outer.parentElement !== root &&
    !drawn(outer.parentElement)
  ) {
    outer = outer.parentElement;
  }
  return between(outer.previousSibling, outer.nextSibling, outer.parentNode ?? root, root);
}

function between(
  before: Node | null,
  after: Node | null,
  parent: Node,
  root: HTMLElement
): Span | null {
  const above = drawnSibling(before, "previousSibling")?.getBoundingClientRect().bottom;
  const below = drawnSibling(after, "nextSibling")?.getBoundingClientRect().top;
  if (above !== undefined && below !== undefined) {
    return { bottom: Math.max(above, below), top: Math.min(above, below) };
  }
  const edge = above ?? below ?? drawnAround(parent, root)?.getBoundingClientRect().top;
  return edge === undefined ? null : { bottom: edge, top: edge };
}

function drawnSibling(start: Node | null, step: "nextSibling" | "previousSibling"): Element | null {
  for (let node = start; node !== null; node = node[step]) {
    if (node instanceof Element && node.isConnected && drawn(node)) return node;
  }
  return null;
}

/** The nearest drawn element at or around `start`, no further out than `root`. */
function drawnAround(start: Node, root: HTMLElement): Element | null {
  for (
    let node: Node | null = start;
    node !== null;
    node = node === root ? null : node.parentNode
  ) {
    if (node instanceof Element && node.isConnected && drawn(node)) return node;
  }
  return null;
}

/** How far `rect` stands above or below `stood`: none where the two overlap. */
function distance(rect: Span, stood: Span): number {
  return Math.max(0, stood.top - rect.bottom, rect.top - stood.bottom);
}

/** The control drawn in this change with the lost one's identity, nearest to where it stood. */
function replacementOf(
  identity: ControlIdentity,
  root: HTMLElement,
  records: readonly MutationRecord[],
  stood: Span | null
): HTMLElement | null {
  let replacement: HTMLElement | null = null;
  let nearest = Number.POSITIVE_INFINITY;
  for (const record of records) {
    for (const added of record.addedNodes) {
      if (!(added instanceof Element) || !root.contains(added)) continue;
      for (const control of [added, ...added.querySelectorAll(CONTROLS)]) {
        if (
          !(control instanceof HTMLElement) ||
          !control.matches(CONTROLS) ||
          control.matches(":disabled") ||
          !drawn(control) ||
          identityOf(control, entryOf(control)) !== identity
        ) {
          continue;
        }
        const away = stood === null ? 0 : distance(control.getBoundingClientRect(), stood);
        if (away < nearest) {
          nearest = away;
          replacement = control;
        }
      }
    }
  }
  return replacement;
}

/** The control in the reader's tab order, enabled, in sight and not pinned, nearest to where the
 *  lost one stood; the first in the box's order of those as near. It is looked for first inside
 *  `scroller`, the box that scrolled the lost one, and only where that holds none in the rest of
 *  the box, so the reader's next Tab goes on from their place in Details' body rather than from
 *  its head, from which it goes to the body's first control and scrolls the body back to its top. */
function nearestTo(
  stood: Span | null,
  root: HTMLElement,
  scroller: Element | null
): HTMLElement | null {
  if (stood === null) return null;
  const sight = sightOf(root);
  const frames = new Map<Element, Frame>();
  const nearestIn = (scope: Element, searched: Element | null): HTMLElement | null => {
    let nearest: HTMLElement | null = null;
    let best = Number.POSITIVE_INFINITY;
    for (const control of scope.querySelectorAll(CONTROLS)) {
      if (
        !(control instanceof HTMLElement) ||
        control.tabIndex < 0 ||
        control.matches(":disabled") ||
        !drawn(control) ||
        searched?.contains(control) === true
      ) {
        continue;
      }
      const rect = control.getBoundingClientRect();
      const away = distance(rect, stood);
      if (away < best && inSightUnpinned(control, rect, root, sight, frames)) {
        best = away;
        nearest = control;
      }
    }
    return nearest;
  };
  return (scroller === null ? null : nearestIn(scroller, null)) ?? nearestIn(root, scroller);
}

/** The nearest box at or around `start` that scrolls its content, inside the box and short of it:
 *  Details' body, for a control drawn in it. Null where none stands between `start` and the box. */
function scrollerOf(start: Node | null, root: HTMLElement): Element | null {
  if (start === null || !root.contains(start)) return null;
  for (
    let node = start instanceof Element ? start : start.parentElement;
    node !== null && node !== root;
    node = node.parentElement
  ) {
    const style = getComputedStyle(node);
    if (SCROLLING[style.overflowY] === true || SCROLLING[style.overflowX] === true) return node;
  }
  return null;
}

/** The part of the window the box covers. */
function sightOf(root: HTMLElement): Bounds {
  const rect = root.getBoundingClientRect();
  return {
    bottom: Math.min(rect.bottom, window.innerHeight),
    left: Math.max(rect.left, 0),
    right: Math.min(rect.right, window.innerWidth),
    top: Math.max(rect.top, 0),
  };
}

/** Whether `control` can be where the reader's place is: neither it nor any element between it
 *  and the box is pinned (`PINNED`), and some of `rect` shows - inside the box's part of the
 *  window, and inside every element between the control and the box that cuts what it draws: the
 *  box's scrolling body, for one, whose controls scrolled out of it still stand inside the box's
 *  own rect. */
function inSightUnpinned(
  control: HTMLElement,
  rect: Bounds,
  root: HTMLElement,
  sight: Bounds,
  frames: Map<Element, Frame>
): boolean {
  if (PINNED[getComputedStyle(control).position] === true) return false;
  let { bottom, left, right, top } = sight;
  for (
    let ancestor = control.parentElement;
    ancestor !== null && ancestor !== root;
    ancestor = ancestor.parentElement
  ) {
    let frame = frames.get(ancestor);
    if (frame === undefined) {
      const style = getComputedStyle(ancestor);
      frame = {
        clip:
          CLIPPING[style.overflowX] === true || CLIPPING[style.overflowY] === true
            ? ancestor.getBoundingClientRect()
            : null,
        pinned: PINNED[style.position] === true,
      };
      frames.set(ancestor, frame);
    }
    if (frame.pinned) return false;
    const { clip } = frame;
    if (clip !== null) {
      bottom = Math.min(bottom, clip.bottom);
      left = Math.max(left, clip.left);
      right = Math.min(right, clip.right);
      top = Math.max(top, clip.top);
    }
  }
  return rect.bottom > top && rect.top < bottom && rect.right > left && rect.left < right;
}

/** The box's heading, able to take the focus: given `tabindex="-1"` where it takes none. */
function headingOf(root: HTMLElement): HTMLElement | null {
  const heading = root.querySelector("h2");
  if (heading === null) return null;
  if (!heading.hasAttribute("tabindex")) heading.setAttribute("tabindex", "-1");
  return heading;
}
