import { useCallback, useEffect, useState, useSyncExternalStore } from "react";

import type { ApiError } from "../../api/client";
import type { ArtifactApproval } from "../../api/types";

/**
 * Dispatch's refusal of a person's Approve for the form of The Spec (`SPEC_FORM`), kept by the
 * approval request it refused, under the request's own key (`requestRefusalKey`, its ask). The
 * header (`ApprovalChip`) of each document the request names and each card of the request
 * (`AskCard`'s answer form) read it there, so a refusal of a press in one place shows in each place
 * that shows the request, and in no place that shows another request. A header's Approve with no
 * request open is kept for its document alone (`approvalRefusalKey`).
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
 * Each refusal holds the versions it judged, one for each document the request names
 * (`requestVersions`), and a read for other versions finds none, since the findings are of the
 * texts those versions hold: a new version of any document of the request hides it. The next
 * press of the same request, or an answer that Dispatch records, lets the refusal go.
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

/** The key of the refusal of the approval request `askId`, which each card of the request and
 *  the header of each document it names share. */
export function requestRefusalKey(askId: string): string {
  return `ask:${askId}`;
}

/** The key the header of the document `artifactId` keeps a refusal under: its open request's
 *  (`requestRefusalKey`) while that request awaits an answer, or else the document's own. With no
 *  request open, `approval.ask_id` names the request of the latest review, which is answered, and
 *  a refusal kept under it would show on that request's completed card. */
export function approvalRefusalKey(
  approval: Pick<ArtifactApproval, "ask_id" | "state"> | undefined,
  artifactId: string
): string {
  return approval?.state === "awaiting" && approval.ask_id !== undefined
    ? requestRefusalKey(approval.ask_id)
    : `artifact:${artifactId}`;
}

/** A document an approval request names, at the version the request names. */
interface RequestDocument {
  readonly artifact_id: string;
  readonly version: number;
}

/** The versions a refusal judged, written once for comparison: each document of the request at
 *  its version, or `single` alone when the read lists no documents, as a request of one document
 *  reads; undefined when no version is known. */
export function requestVersions(
  documents: readonly RequestDocument[] | null | undefined,
  single: { readonly artifact_id: string; readonly version: number | undefined }
): string | undefined {
  if (documents !== null && documents !== undefined && documents.length > 0) {
    return documents
      .map(({ artifact_id, version }) => `${artifact_id}@${version}`)
      .sort()
      .join(",");
  }
  return single.version === undefined ? undefined : `${single.artifact_id}@${single.version}`;
}

/** One press of Approve: the surface where the person pressed it (`useRefusalSurface`), when,
 *  and the versions it judged (`requestVersions`). */
export interface FormRefusalPress {
  readonly surface: number;
  readonly at: number;
  readonly versions: string | undefined;
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

/** A press of Approve on `surface`, for the versions it judges (`requestVersions`). */
export function pressOn(surface: number, versions: string | undefined): FormRefusalPress {
  return { at: ++moments, surface, versions };
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

/** The refusal of `key` at `versions`, or undefined. The component re-renders when it changes. */
export function useFormRefusal(key: string, versions: string | undefined): ApiError | undefined {
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
  return refusal === undefined || refusal.press.versions !== versions ? undefined : refusal.error;
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
