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
   * element is still in the document; else null. A fallback that wants the control next to the
   * lost one starts from here.
   */
  readonly place: HTMLElement | null;
}

/** One place the focus may go once it is lost, or nothing when this fallback has none to give. */
export type FocusFallback = (loss: FocusLoss) => HTMLElement | null | undefined;

/** What takes an element, and everything inside it, out of the focus's reach. */
const OUT_OF_REACH = "[hidden], [inert]";

/**
 * Whether the element can take the focus as it stands: it is on the page, not disabled, and
 * neither it nor an ancestor is hidden or inert. It reads no layout, so a test's DOM, which has
 * none, answers as a browser does.
 */
export function canHoldFocus(element: Element | null | undefined): element is HTMLElement {
  return (
    element instanceof HTMLElement &&
    element.isConnected &&
    !element.matches(":disabled") &&
    element.closest(OUT_OF_REACH) === null
  );
}

/** The changes that can take the focus away: a removal, and a disable, a hide or an inert. */
const LOSSES: MutationObserverInit = {
  attributeFilter: ["disabled", "hidden", "inert"],
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
    if (record.type !== "childList" || !(target instanceof HTMLElement) || !target.isConnected) {
      continue;
    }
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
 * above, but remembers the control the focus left and that layer (decision 237). A write may
 * take that control away while the layer is open, as the header's labels' count goes once every
 * label fits; the layer, as it closes, then has nothing to give the focus back to and leaves it
 * on the body. So while the keeper remembers them it watches the layer and its ancestors, and
 * one task after a change of them it asks `fallbacks` about that control, with no added elements
 * and no place, and focuses the first answer that can hold the focus, as for a loss inside the
 * region. It does so only when all of these hold:
 * - the layer is closed: out of the page, or under a hidden or inert element;
 * - the focus is on the body or nowhere, or still on a control of the closed layer, which the
 *   browser is about to drop to the body;
 * - the control can no longer hold the focus: it is removed, disabled, hidden or inert;
 * - the reader's last pointer press since the focus was last inside the layer, if any, fell
 *   inside it: a press outside it is the reader putting the focus on the page, where a press on
 *   the layer's own Close is how a reader closes it.
 *
 * A loss inside a layer still open is never the region's to act on. The keeper forgets the
 * control and the layer when the focus comes back into the region, when the focus goes to an
 * element outside both, once the layer has closed - whether the keeper placed the focus, the
 * layer's own code gave it back to the control, or the reader moved it - and on unmount.
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
    /** Whether the reader has pressed the pointer somewhere other than `held` since it took the
     *  focus, and pressed no key since. Such a press is the reader moving the focus: a press on
     *  the page blurs the control to the body, and the handler that blur runs may remove it before
     *  the check below runs. A press that moves no focus on purpose (a control that prevents its
     *  mousedown) leaves the focus on `held` with no focusin to clear the mark, so a key clears
     *  it: a key acts from where the focus is. */
    let pointerMoved = false;
    /** The timer of the keeper's move, which waits one task after the check that found the loss,
     *  or undefined while no move waits. */
    let waiting: number | undefined;
    /** The keeper's last move of the focus off a control that turned disabled: that control, and
     *  where the keeper put the focus. Null once the control has the focus back, the reader has
     *  acted or the focus has moved on. */
    let given: { readonly from: HTMLElement; readonly to: HTMLElement } | null = null;
    /** The control that held the focus when it left the region for a layer outside it, and that
     *  layer, while the keeper waits for the layer to close; null while it waits for none. */
    let left: { readonly layer: Element; readonly opener: HTMLElement } | null = null;
    /** Whether the reader's last pointer press since the focus was last inside the layer fell
     *  outside it: then that press, not the layer, put the focus where it is. */
    let pressedOutside = false;
    /** The timer of the read of the layer, which waits one task after a change of the layer or of
     *  an ancestor, or undefined while no read waits. */
    let layerRead: number | undefined;

    const notePress = (event: PointerEvent) => {
      // A press is the reader acting, which ends a claim to give a control its focus back.
      given = null;
      // A press on the control that holds the focus leaves the focus there and focuses nothing
      // anew, so no focusin would clear the mark: the write that press starts may then take the
      // control away, which is a loss the keeper owes the reader.
      if (held !== null && event.target instanceof Node && held.contains(event.target)) return;
      pointerMoved = true;
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
        page.addEventListener("pointerdown", notePress, true);
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
      page.removeEventListener("pointerdown", notePress, true);
      page.removeEventListener("keydown", noteKey, true);
    };
    /** Whether the focus `lost` held is lost: it is on the body or still on `lost`, no press of
     *  the reader's put it there, and `lost` can no longer hold it. Every other answer is final,
     *  and where the focus has left the region the keeper stops watching it. */
    const isLost = (lost: HTMLElement): boolean => {
      const active = page.activeElement;
      const onBody = focusOnDocument();
      if (!onBody && active !== lost) {
        // The focus moved to another element. Inside the region the focusin has already made it
        // the one held, and a handler that handed the focus on is obeyed; outside it, the reader
        // has left the region, and where they went into a layer the keeper waits for it to close.
        if (!region.contains(active)) {
          forget();
          const layer = active === null ? null : outermostLayer(active);
          if (layer !== null && !layer.contains(region)) watchLayer(lost, layer);
        }
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
      // region holds now.
      const lost = held;
      if (lost === null) return;
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
    const layerObserver = new MutationObserver(() => {
      // The read waits one task, as the move above does: the layer's own code gives the focus
      // back as it closes, and the browser drops a focus the closing took away to the body.
      layerRead ??= window.setTimeout(readLayer, 0);
    });
    const notePagePress = (event: PointerEvent) => {
      if (left === null) return;
      pressedOutside = !(event.target instanceof Node && left.layer.contains(event.target));
    };
    const notePageFocus = (event: FocusEvent) => {
      if (left === null) return;
      if (event.target instanceof Node && left.layer.contains(event.target)) {
        pressedOutside = false;
        return;
      }
      // The focus came back into the region, which the keeper watches again from its own focusin,
      // or the reader took it somewhere else.
      forgetLayer();
    };
    const watchLayer = (opener: HTMLElement, layer: Element) => {
      if (left === null) {
        page.addEventListener("focusin", notePageFocus, true);
        page.addEventListener("pointerdown", notePagePress, true);
      }
      // Each change that closes the layer is a record on the layer or on one of its ancestors, so
      // no other change of the page wakes the observer.
      for (let node: Node | null = layer; node !== null; node = node.parentNode) {
        layerObserver.observe(node, LAYER_CLOSINGS);
      }
      left = { layer, opener };
      pressedOutside = false;
    };
    const forgetLayer = () => {
      left = null;
      pressedOutside = false;
      window.clearTimeout(layerRead);
      layerRead = undefined;
      layerObserver.disconnect();
      page.removeEventListener("focusin", notePageFocus, true);
      page.removeEventListener("pointerdown", notePagePress, true);
    };
    /** Once the layer has closed, forgets it, and places the focus it dropped by the fallbacks
     *  when the control it was opened from can no longer hold the focus. */
    const readLayer = () => {
      layerRead = undefined;
      if (left === null) return;
      const { layer, opener } = left;
      // A loss inside a layer still open is the layer's own.
      if (layer.isConnected && layer.closest(OUT_OF_REACH) === null) return;
      const active = page.activeElement;
      // A focus still on a control of the closed layer is as good as nowhere: the browser drops
      // it to the body in a later task.
      const dropped = focusOnDocument() || (active !== null && layer.contains(active));
      const readerMoved = pressedOutside;
      forgetLayer();
      // A control that can still hold the focus is the layer's to give it back to, or the reader
      // moved the focus off it; a press outside the layer put the focus on the page.
      if (!dropped || readerMoved || canHoldFocus(opener)) return;
      const loss: FocusLoss = { added: [], lost: opener, place: null, root: region };
      firstHolder(latestFallbacks.current, loss)?.focus();
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
      forgetLayer();
    };
  }, [root]);
}
