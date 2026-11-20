import { type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import { focusOnDocument } from "../shell/roving";
import { DETAILS_ATTRIBUTE } from "./margin-slot";

/** What a region lost the focus to, as each fallback of `useFocusKeeper` reads it. */
export interface FocusLoss {
  /** The region's root. */
  readonly root: HTMLElement;
  /** The element that held the focus, now removed, disabled, hidden or inert. */
  readonly lost: HTMLElement;
  /** The elements the same change added inside the region, in the order they were added. */
  readonly added: readonly HTMLElement[];
  /**
   * Where the lost element stood at the change: its parent while it is still in the document;
   * for a removal, the element the change took it (or an ancestor of it) out of, while that
   * element is still in the document; else null. For a control lost while the focus was in a
   * layer outside the region, whose removal the keeper does not hear, that element is the
   * nearest one around the control as it stood when the focus left it that is still in the
   * document. A fallback that wants the control next to the lost one starts from here.
   */
  readonly place: HTMLElement | null;
}

/** One place the focus may go once it is lost, or nothing when this fallback has none to give. */
export type FocusFallback = (loss: FocusLoss) => HTMLElement | null | undefined;

/** What takes an element, and everything inside it, out of the focus's reach. */
const OUT_OF_REACH = "[hidden], [inert]";

/** Whether `element`, or an element around it below `within`, is hidden or inert: the nearest
 *  such element refuses unless it is `within` or above it. With no `within`, or one that does
 *  not hold `element`, every element around it is read. */
function outOfReach(element: Element, within: Element | null | undefined): boolean {
  const shut = element.closest(OUT_OF_REACH);
  if (shut === null) return false;
  return within?.contains(element) !== true || !shut.contains(within);
}

/**
 * Whether the element can take the focus as it stands: it is on the page, not disabled, and
 * neither it nor an ancestor is hidden or inert. It reads no layout, so a test's DOM, which has
 * none, answers as a browser does.
 *
 * With `within`, only the element and the elements around it below `within` are read for
 * `hidden` and `inert`: `within` and what is above it are not, since a caller names a bound
 * that may be shut on purpose (`usableControl`, `usable-control.ts`).
 */
export function canHoldFocus(
  element: Element | null | undefined,
  within?: Element | null
): element is HTMLElement {
  return (
    element instanceof HTMLElement &&
    element.isConnected &&
    !element.matches(":disabled") &&
    !outOfReach(element, within)
  );
}

/** The attributes whose change can change what `canHoldFocus` answers: `LOSSES` watches them,
 *  and so does a watch of `usableControl`'s answer (`usable-control.ts`), among its own. */
export const FOCUS_ATTRIBUTES = ["disabled", "hidden", "inert"] as const;

/** The changes that can take the focus away: a removal, and a disable, a hide or an inert. */
const LOSSES: MutationObserverInit = {
  attributeFilter: [...FOCUS_ATTRIBUTES],
  attributes: true,
  childList: true,
  subtree: true,
};

/** A layer a reader opens over the page and closes again: a dialog, or Details' drawer, which
 *  stands beside the page as an `aside` rather than as a dialog. No other `aside` is a layer:
 *  Details holds one of its own inside its drawer, and the Contents pane is one that stays. */
const LAYER = `[role="dialog"], aside[${DETAILS_ATTRIBUTE}]`;

/** The outermost layer around `element`, or null where it stands in none. A dialog inside
 *  Details is a part of Details, which the reader is still in when that dialog closes. */
function outermostLayer(element: Element): Element | null {
  let outermost: Element | null = null;
  let layer = element.closest(LAYER);
  while (layer !== null) {
    outermost = layer;
    layer = layer.parentElement?.closest(LAYER) ?? null;
  }
  return outermost;
}

/** The changes that close a layer, observed on the layer and on each of its ancestors but not on
 *  what they hold: the removal of the layer or of an ancestor is a change of its parent's
 *  children, and a hide or an inert of either is a change of that element's own attributes. */
const LAYER_CLOSINGS: MutationObserverInit = {
  attributeFilter: ["hidden", "inert"],
  attributes: true,
  childList: true,
};

/** The elements `records` added, in the order they were added. */
function addedBy(records: readonly MutationRecord[]): HTMLElement[] {
  const added: HTMLElement[] = [];
  for (const record of records) {
    for (const node of record.addedNodes) {
      if (node instanceof HTMLElement) added.push(node);
    }
  }
  return added;
}

/** Where `lost` stood at the change `records` heard, as `FocusLoss.place` says. Of two removals
 *  that took it out, the later one is read, since that is where it stood last. */
function placeOf(lost: HTMLElement, records: readonly MutationRecord[]): HTMLElement | null {
  if (lost.isConnected) return lost.parentElement;
  let place: HTMLElement | null = null;
  for (const record of records) {
    const { target } = record;
    if (!(target instanceof HTMLElement) || !target.isConnected) continue;
    for (const removed of record.removedNodes) {
      if (removed.contains(lost)) place = target;
    }
  }
  return place;
}

/** The first answer of `fallbacks` to `loss` that can hold the focus, or null when none can. */
function firstHolder(fallbacks: readonly FocusFallback[], loss: FocusLoss): HTMLElement | null {
  for (const fallback of fallbacks) {
    const next = fallback(loss);
    if (canHoldFocus(next)) return next;
  }
  return null;
}

/**
 * Hears the reader's pointer presses on `page`, and returns what ends the hearing. It is the one
 * reader of presses on the page: the keeper's watches and the header's title row read presses
 * through it, so a press is over for each of them on the same events. `begin` is told of each
 * press as its pointer goes down; once the press is over, `done` is told where the focus was as
 * the press began and what `begin` answered. A press is over at the first of these:
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
 * Watches `layer`, a layer outside `region` that the focus left the region for from `opener`,
 * and returns what ends the watch (decision 237). A write may take the opener away while the
 * layer is open, as the header's labels' count goes once every label fits; the layer, as it
 * closes, then has nothing to give the focus back to and leaves it on the body. So the watch
 * hears the changes that may close the layer, and one task after one it reads the layer. Once
 * the layer is closed - out of the page, or under a hidden or inert element - the watch ends,
 * and it asks `fallbacks` about the opener and focuses the first answer that can hold the focus,
 * as for a loss inside the region, when all of these hold:
 * - the focus is on the body or nowhere, or still on a control of the closed layer, which the
 *   browser is about to drop to the body;
 * - the opener can no longer hold the focus: it is removed, disabled, hidden or inert;
 * - no press of the reader's put the focus on the body. A press outside the layer that moved the
 *   focus off an element still in the page onto the page, with no element focused since, is the
 *   reader putting it there. A press that moved no focus is no move of the reader's: the header's
 *   Details control keeps the focus where it is and closes Details. Nor is a fall to the body
 *   from an element that went as the press began: a popover that closes on a pointerdown, as an
 *   `AnchoredPanel` does, takes the control focused in it along and drops the focus to the body
 *   before the press's mousedown. Nor is a press that moved the focus to an element, as one in a
 *   popover the layer opened: a later fall to the body is not its doing. A press is read once its
 *   own move is done (`hearPresses`), and its click is read before the target's own handler, so a
 *   click that closes the layer is read before it does.
 *
 * The loss names no added elements, since nothing watched the region while the layer was open,
 * and its place is read from where the opener stood when the watch began (`FocusLoss.place`).
 *
 * A loss inside a layer still open is the layer's own. The watch also ends when the reader goes
 * on to the page: a focus on an element of the page's own top-level container (the app's root)
 * that stands in no layer leaves nothing of the layer's to drop, and a later fall of that focus to
 * the body, as a control there goes, is no doing of the layer's. A focus in another layer keeps
 * the watch going, since that layer gives the focus back as it closes: a dialog the app draws in
 * its root, as the search palette is, gives it back to the control of the layer that held it as
 * the dialog opened; a popover a control of the layer opens, as Details' Edit labels does, is
 * drawn on the page's body outside that container, and the focus comes back into the layer from
 * it or falls with it as the layer closes.
 */
function watchLayer(
  region: HTMLElement,
  opener: HTMLElement,
  layer: Element,
  fallbacks: RefObject<readonly FocusFallback[]>
): () => void {
  const page = region.ownerDocument;
  // The elements around the opener, from its parent up to the region, are read now, while it is
  // in the page: a write that takes it away may take the element around it too, as an approval
  // chip's two actions go with the box that keeps them together, and an element out of the page
  // leads up to nothing.
  const around: HTMLElement[] = [];
  for (let element = opener.parentElement; element !== null; element = element.parentElement) {
    around.push(element);
    if (element === region) break;
  }
  // The page's own top-level container, the app's root, which holds the region, the rest of the
  // page and the dialogs the app draws, but not a popover drawn straight on the page's body.
  const app = region.closest("body > *");
  /** Whether the reader's last press that moved the focus put it on the page from outside the
   *  layer, off an element still in the page, and no element has taken the focus since: then that
   *  press, not the layer, put the focus on the body. */
  let pressedOntoPage = false;
  /** The timer of the read of the layer, which waits one task after a change of the layer or of
   *  an ancestor, or undefined while no read waits. */
  let read: number | undefined;

  const readPress = (from: Element | null, outside: boolean) => {
    // A press that left the focus where it was is no move of the reader's, and leaves the mark as
    // it was. A fall to the body is the press's own only while the element the focus left is
    // still in the page: a popover that closes as the press begins takes its focused control
    // along, which drops the focus to the body before the press could move it.
    if (page.activeElement === from) return;
    pressedOntoPage = outside && focusOnDocument() && from?.isConnected === true;
  };
  const noteFocus = (event: FocusEvent) => {
    const { target } = event;
    if (
      target instanceof Element &&
      app?.contains(target) === true &&
      outermostLayer(target) === null
    ) {
      // The reader went on to the page, so the layer holds no focus of theirs to drop as it
      // closes, and a later fall of this focus to the body is none of the layer's doing. A focus
      // in a layer, this one or another, is not on the page, so the watch goes on: the layer
      // gives the focus back as it closes.
      stop();
      return;
    }
    // An element took the focus, so a later fall of the focus to the body is not a press's doing.
    pressedOntoPage = false;
  };
  const observer = new MutationObserver(() => {
    // The read waits one task, as the region's move does: the layer's own code gives the focus
    // back as it closes, and the browser drops a focus the closing took away to the body.
    read ??= window.setTimeout(readLayer, 0);
  });
  const stop = () => {
    window.clearTimeout(read);
    read = undefined;
    observer.disconnect();
    page.removeEventListener("focusin", noteFocus, true);
    stopPresses();
  };
  const readLayer = () => {
    read = undefined;
    // A layer still open keeps the watch going: a loss inside it is the layer's own.
    if (layer.isConnected && layer.closest(OUT_OF_REACH) === null) return;
    stop();
    const active = page.activeElement;
    // A focus still on a control of the closed layer is as good as nowhere: the browser drops it
    // to the body in a later task.
    const dropped = focusOnDocument() || layer.contains(active);
    // An opener that can still hold the focus is the layer's to give it back to, or the reader
    // moved the focus off it.
    if (!dropped || pressedOntoPage || canHoldFocus(opener)) return;
    const place = around.find((element) => element.isConnected) ?? null;
    const loss: FocusLoss = { added: [], lost: opener, place, root: region };
    firstHolder(fallbacks.current, loss)?.focus();
  };

  // Each change that closes the layer is a record on the layer or on one of its ancestors, so no
  // other change of the page wakes the observer.
  for (let node: Node | null = layer; node !== null; node = node.parentNode) {
    observer.observe(node, LAYER_CLOSINGS);
  }
  page.addEventListener("focusin", noteFocus, true);
  // Each press notes as it begins whether it fell outside the layer, which `readPress` reads.
  const stopPresses = hearPresses(
    page,
    (event) => !(event.target instanceof Node && layer.contains(event.target)),
    readPress
  );
  return stop;
}

/**
 * Keeps a reader's place in a region when a change takes away the control that holds the focus
 * (decision 216). It remembers which element inside `root` holds the focus. When that element is
 * removed, disabled, hidden or made inert while it holds the focus, the browser drops the focus to
 * the page's body; the keeper then asks `fallbacks` in order and focuses the first answer that
 * can hold the focus, so a keyboard reader goes on from where they were rather than from the top
 * of the page. A fallback reads the loss: the lost element, where it stood and what the same
 * change added, so it can find the control drawn in the lost one's place.
 *
 * The keeper waits one task before it moves the focus, and moves it only if the focus is still on
 * the body or on the lost element then: a site that hands the focus on itself, even from a passive
 * effect that runs after the change, keeps its own choice.
 *
 * When the keeper moved the focus off a control because that control was disabled, and the
 * control is enabled again while the focus still rests where the keeper put it, the keeper gives
 * the focus back to it: a Retry that is disabled while its read runs again and enabled when that
 * read fails keeps the reader on the Retry. Any act of the reader in between ends that claim - a
 * key or a pointer press anywhere on the page, or a focus on any other element - and so do the
 * focus leaving the region and the control being removed.
 *
 * Apart from that give-back, it never moves the focus when nothing was lost. A focus the reader
 * moved elsewhere, a focus the reader put on the body (a press on the page, a `blur()`), and a
 * focus a handler already handed to another control all stay where they are. When no fallback can
 * hold the focus it stays on the body. A region nobody has focused costs nothing: the keeper
 * watches the region only while the focus is inside it.
 *
 * When the focus leaves the region for a layer outside it - the outermost dialog or Details'
 * drawer (`aside[data-details]`) around the element that took the focus, so a dialog inside
 * Details does not narrow the watch to that dialog - the keeper stops watching the region as
 * above, and watches that layer for the control the focus left (`watchLayer`): a write may take
 * that control away while the layer is open, and the layer then has nothing to give the focus
 * back to as it closes. That watch ends in four ways: when the focus comes back into the region,
 * whose own watch keeps the focus from there, even where it comes back into a dialog the region
 * draws, which is a layer of its own; when it goes on to an element of the page that stands in no
 * layer, from which a later fall to the body is no loss of the control's; once the layer has
 * closed, whether the keeper placed the focus, the layer's own code gave it back to the control,
 * or the reader moved it; and on unmount. A focus in another layer - a dialog the app draws in its
 * root, or a popover the layer opened on the page's body - keeps it going, since that layer gives
 * the focus back as it closes.
 *
 * `fallbacks` may be a new array on every render; the keeper always asks the latest one.
 */
export function useFocusKeeper(
  root: RefObject<HTMLElement | null>,
  fallbacks: readonly FocusFallback[]
): void {
  // Read through a ref, because subscribing again for each render's new array would forget which
  // element held the focus.
  const latestFallbacks = useRef(fallbacks);
  useLayoutEffect(() => {
    latestFallbacks.current = fallbacks;
  });

  useEffect(() => {
    const region = root.current;
    if (region === null) return;
    const page = region.ownerDocument;
    /** The element inside the region that holds the focus, or null while the focus is outside. */
    let held: HTMLElement | null = null;
    /** The observer's records since the last check: what a change added, and where it took an
     *  element out. */
    let heard: MutationRecord[] = [];
    /** Whether a press of the reader's somewhere other than `held` may have moved the focus off
     *  it. Such a press sets the mark as it begins and keeps it until it is done (`hearPresses`):
     *  a mouse press on the page blurs the control to the body as its button goes down, and the
     *  handler that blur runs may remove the control before the check below runs, all before the
     *  press's pointerup. A press that is done keeps the mark when it moved the focus and clears
     *  it when it moved none: a control that cancels its own mousedown, as the header's Details
     *  control does, leaves the focus on `held`, and a write that then takes `held` away is a loss
     *  the keeper owes the reader. A press on `held` clears it as it begins, and so do a key,
     *  which acts from where the focus is, and a focus on an element of the region (`hold`). */
    let pointerMoved = false;
    /** Ends the hearing of the reader's presses (`hearPresses`), which runs while the keeper holds
     *  an element, or is undefined while it holds none. */
    let stopPresses: (() => void) | undefined;
    /** The timer of the keeper's move, which waits one task after the check that found the loss,
     *  or undefined while no move waits. */
    let waiting: number | undefined;
    /** The keeper's last move of the focus off a control that turned disabled: that control, and
     *  where the keeper put the focus. Null once the control has the focus back, the reader has
     *  acted or the focus has moved on. */
    let given: { readonly from: HTMLElement; readonly to: HTMLElement } | null = null;
    /** Ends the watch of the layer the focus last left the region for (`watchLayer`), or is
     *  undefined while no watch runs, save that a watch that ends by itself, as its layer closes
     *  or the reader goes on to the page, leaves it set: ending a watch that has ended does
     *  nothing. */
    let stopLayerWatch: (() => void) | undefined;

    const notePress = (event: PointerEvent) => {
      // A press is the reader acting, which ends a claim to give a control its focus back.
      given = null;
      // A press on the control that holds the focus leaves the focus there and focuses nothing
      // anew, and the write it starts, even before its click, may take the control away, which is
      // a loss the keeper owes the reader. Any other press is marked until it is done.
      const onHeld = held !== null && event.target instanceof Node && held.contains(event.target);
      pointerMoved = !onHeld;
    };
    const readPress = (from: Element | null) => {
      // A press that moved the focus keeps the mark; one that left it where it was is no move of
      // the reader's.
      if (page.activeElement === from) pointerMoved = false;
    };
    const noteKey = () => {
      given = null;
      pointerMoved = false;
    };
    const hear = (records: readonly MutationRecord[]) => {
      for (const record of records) heard.push(record);
    };
    const observer = new MutationObserver((records) => {
      hear(records);
      check();
    });
    const hold = (element: HTMLElement) => {
      if (held === null) {
        observer.observe(region, LOSSES);
        stopPresses = hearPresses(page, notePress, readPress);
        page.addEventListener("keydown", noteKey, true);
      }
      held = element;
      pointerMoved = false;
    };
    const forget = () => {
      held = null;
      heard = [];
      pointerMoved = false;
      window.clearTimeout(waiting);
      waiting = undefined;
      given = null;
      observer.disconnect();
      stopPresses?.();
      stopPresses = undefined;
      page.removeEventListener("keydown", noteKey, true);
    };
    /** Whether the focus has moved from `lost` to an element outside the region: the reader has
     *  left the region, and the keeper stops watching it. Where that element stands in a layer
     *  outside the region, the keeper watches that layer for `lost` until it closes. A focus on
     *  the body, still on `lost`, or on another element of the region has not left. */
    const leftRegion = (lost: HTMLElement): boolean => {
      const active = page.activeElement;
      if (focusOnDocument() || active === null || active === lost || region.contains(active)) {
        return false;
      }
      forget();
      const layer = outermostLayer(active);
      if (layer !== null && !layer.contains(region)) {
        stopLayerWatch = watchLayer(region, lost, layer, latestFallbacks);
      }
      return true;
    };
    /** Whether the focus `lost` held is lost: it is on the body or still on `lost`, no press of
     *  the reader's put it there, and `lost` can no longer hold it. Every other answer is final,
     *  and with the focus on the body the keeper then stops watching the region. A focus outside
     *  the region is read before this, as the reader leaving it (`leftRegion`). */
    const isLost = (lost: HTMLElement): boolean => {
      const onBody = focusOnDocument();
      if (!onBody && page.activeElement !== lost) {
        // The focus moved to another element of the region, whose focusin has already made it
        // the one held: a handler that handed the focus on is obeyed.
        return false;
      }
      if (onBody && pointerMoved) {
        // The reader's own press put the focus on the page.
        forget();
        return false;
      }
      if (canHoldFocus(lost) && region.contains(lost)) {
        // Nothing was lost. A focus on the body is where the reader put it.
        if (onBody) forget();
        return false;
      }
      return true;
    };
    /** Gives the focus back to the control the keeper moved it off, once that control can hold
     *  it again and the focus still rests where the keeper put it. */
    const giveBack = () => {
      if (given === null) return;
      const { from, to } = given;
      if (!region.contains(from)) {
        // The control is gone, so there is nothing to give the focus back to.
        given = null;
        return;
      }
      if (!canHoldFocus(from) || page.activeElement !== to) return;
      given = null;
      from.focus();
    };
    const check = () => {
      if (held === null) return;
      hear(observer.takeRecords());
      giveBack();
      // A give-back moved the focus, and its focusin made the control given it the one held.
      const lost = held;
      if (leftRegion(lost)) return;
      if (!isLost(lost)) {
        heard = [];
        return;
      }
      // The move waits one task: a site's own hand-on may run in a passive effect, which for a
      // change from outside an event runs after this check, and the site's choice wins.
      waiting ??= window.setTimeout(move, 0);
    };
    const move = () => {
      waiting = undefined;
      // The focus may have moved since the check, so the loss is read again, of whatever the
      // region holds now, and the focus may have left the region since.
      const lost = held;
      if (lost === null || leftRegion(lost)) return;
      hear(observer.takeRecords());
      const change = heard;
      heard = [];
      if (!isLost(lost)) return;
      const loss: FocusLoss = {
        added: addedBy(change),
        lost,
        place: placeOf(lost, change),
        root: region,
      };
      // Only a control that is merely disabled gets the focus back: enabled again it is the same
      // control in the same place, where a removed, hidden or inert one is out of the reader's way.
      const disabled =
        lost.isConnected && lost.matches(":disabled") && lost.closest(OUT_OF_REACH) === null;
      const next = firstHolder(latestFallbacks.current, loss);
      if (next === null) {
        forget();
        return;
      }
      // One move is kept at a time: this one replaces whatever an older move kept.
      given = disabled ? { from: lost, to: next } : null;
      next.focus();
      if (!region.contains(page.activeElement)) forget();
    };
    const noteFocus = (event: FocusEvent) => {
      if (!(event.target instanceof HTMLElement)) return;
      // The reader is back in the region, so the layer they left it for, if any, holds no focus
      // of theirs to drop as it closes, and the region's own watch keeps the focus from here. The
      // layer's watch goes on through a focus that stands in a layer, as one in a dialog the
      // region draws does, so this is what ends it then.
      stopLayerWatch?.();
      stopLayerWatch = undefined;
      // A focus anywhere but where the keeper put it moves the focus on, past any give-back.
      if (given !== null && event.target !== given.to) given = null;
      hold(event.target);
    };
    const noteLeave = (event: FocusEvent) => {
      const next = event.relatedTarget;
      if (next instanceof Node && region.contains(next)) return;
      // Chromium blurs a disabled control by itself, so this focusout can come before or after
      // the observer hears the change; the check waits a microtask for the change's records.
      queueMicrotask(check);
    };

    // A control that took the focus as it mounted did so before this effect could hear it.
    const active = page.activeElement;
    if (active instanceof HTMLElement && region.contains(active)) hold(active);
    region.addEventListener("focusin", noteFocus);
    region.addEventListener("focusout", noteLeave);
    return () => {
      region.removeEventListener("focusin", noteFocus);
      region.removeEventListener("focusout", noteLeave);
      forget();
      stopLayerWatch?.();
    };
  }, [root]);
}
