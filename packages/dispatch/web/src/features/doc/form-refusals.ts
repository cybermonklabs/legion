import { useCallback, useEffect, useState, useSyncExternalStore } from "react";

import type { ApiError } from "../../api/client";

/**
 * Dispatch's refusal of a person's Approve for the form of The Spec (`SPEC_FORM`), kept by the
 * approval request it refused, under one key for its document (`approvalRefusalKey`). The
 * document's header (`ApprovalChip`) and each card of the request (`AskCard`'s answer form) read
 * it there, so a refusal of a press in one place shows in each place that shows the request.
 *
 * A refusal belongs to that request, not to the card or the header that shows it, for the reason
 * an unsent answer belongs to its ask (`inbox/answer-drafts.ts`): a host can draw its card again
 * while the answer is out. The issue page's Needs you does that every time, because the answer
 * takes the ask's row out of the Inbox read and the refusal puts it back. The card drawn again
 * reads the refusal here, and with it picks Request changes and takes the focus that the press
 * owes. Each surface that shows the request shows its refusal, only one takes the focus
 * (`takeFormRefusalFocus`), and only one card puts the reason in the request's draft
 * (`takeFormRefusalReason`).
 *
 * Each refusal holds the version it judged, and a read for another version finds none, since the
 * findings are of the text that version holds. The next press of the same request, or an answer
 * that Dispatch records, lets the refusal go.
 */
interface FormRefusal {
  readonly error: ApiError;
  readonly press: FormRefusalPress;
  /** Whether the press it answers still owes the focus to a surface that shows it: true until
   *  one takes it (`takeFormRefusalFocus`). */
  focusOwed: boolean;
  /** Whether the reason it carries is still to go in the request's draft: true until a card
   *  puts it there (`takeFormRefusalReason`). */
  reasonOwed: boolean;
}

/** The key of the refusal of the approval request of the document `artifactId`, which its header
 *  and each card of the request share. */
export function approvalRefusalKey(artifactId: string): string {
  return `artifact:${artifactId}`;
}

/** One press of Approve: the surface where the person pressed it (`useRefusalSurface`), when,
 *  and the version it judged. */
export interface FormRefusalPress {
  readonly surface: number;
  readonly at: number;
  readonly version: number | undefined;
}

const refusals = new Map<string, FormRefusal>();
const listeners = new Map<string, Set<() => void>>();
/** The order of draws and presses: each surface and each press takes the next number. */
let moments = 0;
/** The surfaces that are in the page now. */
const drawn = new Set<number>();

/** This surface's number, in the order of draws and presses, for as long as it is in the page. */
export function useRefusalSurface(): number {
  const [surface] = useState(() => ++moments);
  useEffect(() => {
    drawn.add(surface);
    return () => {
      drawn.delete(surface);
    };
  }, [surface]);
  return surface;
}

/** A press of Approve on `surface`, for the version it judges. */
export function pressOn(surface: number, version: number | undefined): FormRefusalPress {
  return { at: ++moments, surface, version };
}

/** Keeps `error` as the refusal of `key` that answers `press`, or lets the refusal of `key` go. */
export function setFormRefusal(
  key: string,
  refusal: { readonly error: ApiError; readonly press: FormRefusalPress } | undefined
): void {
  if (refusal === undefined && !refusals.has(key)) return;
  if (refusal === undefined) refusals.delete(key);
  else refusals.set(key, { ...refusal, focusOwed: true, reasonOwed: true });
  for (const listener of listeners.get(key) ?? []) listener();
}

/** The refusal of `key` at `version`, or undefined. The component re-renders when it changes. */
export function useFormRefusal(key: string, version: number | undefined): ApiError | undefined {
  const subscribe = useCallback(
    (listener: () => void) => {
      const keyListeners = listeners.get(key) ?? new Set<() => void>();
      listeners.set(key, keyListeners);
      keyListeners.add(listener);
      return () => {
        keyListeners.delete(listener);
        if (keyListeners.size === 0) listeners.delete(key);
      };
    },
    [key]
  );
  const refusal = useSyncExternalStore(subscribe, () => refusals.get(key));
  return refusal === undefined || refusal.press.version !== version ? undefined : refusal.error;
}

/**
 * Whether `surface` takes the focus that the press of `key`'s refusal owes, and takes it once.
 * The surface where the person pressed takes it. When its host took that surface out of the page,
 * a surface drawn after the press stands in for it, while the focus is on no control or inside
 * `root`. Another surface that shows the refusal, such as a second card of the same request on
 * the page, takes no focus: the focus stays where the person pressed.
 */
export function takeFormRefusalFocus(key: string, surface: number, root: Element | null): boolean {
  const refusal = refusals.get(key);
  if (refusal === undefined || !refusal.focusOwed) return false;
  const { press } = refusal;
  const standsIn = !drawn.has(press.surface) && surface > press.at && focusIsFreeFor(root);
  if (surface !== press.surface && !standsIn) return false;
  refusal.focusOwed = false;
  return true;
}

/** Whether the reason of `key`'s refusal is still to go in the request's draft, and takes it once:
 *  the draft belongs to the request, so the first card that asks puts the reason there for each
 *  card, and a card drawn later leaves the text as the person changed it. */
export function takeFormRefusalReason(key: string): boolean {
  const refusal = refusals.get(key);
  if (refusal === undefined || !refusal.reasonOwed) return false;
  refusal.reasonOwed = false;
  return true;
}

/** Whether the focus is on no control, or on one inside `surface`: where a surface may give the
 *  focus that a refusal's press owes without taking it from where the reader moved it. */
export function focusIsFreeFor(surface: Element | null): boolean {
  const active = document.activeElement;
  return active === null || active === document.body || surface?.contains(active) === true;
}

/** Forgets every refusal, for the unit-test preload, since the store outlives any one render. */
export function resetFormRefusals(): void {
  refusals.clear();
}
