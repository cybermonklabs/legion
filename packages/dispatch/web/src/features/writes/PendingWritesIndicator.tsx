import type { ReactNode } from "react";
import { type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import { apiErrorMessage } from "../../api/client";
import {
  borderDefault,
  calloutWarningBorder,
  card,
  dangerText,
  secondaryButtonCompact,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { focusOnDocument } from "../shell/roving";
import { retryNote, SendInterruptedError, SendStoppedError } from "./keyed-write";
import { StopTrying } from "./StopTrying";
import { useWriteSelection, useWrites, type Write } from "./write-store";

/** Why `refusal` left a write unsaved, in the reader's words. One that ended without Dispatch's
 *  verdict - stopped, its key run out, or interrupted by a reload - may have landed, and what its
 *  Retry does is `retryNote`'s, by whether the write goes under its key (`keyed`, `Write.keyed`):
 *  a `PUT` or a `DELETE` sends none, so no Retry of one asks under a key. */
function refusalReason(keyed: boolean, refusal: Error): string {
  if (!(refusal instanceof SendStoppedError)) {
    return apiErrorMessage(refusal, "Dispatch could not be reached");
  }
  const retry = retryNote(keyed, refusal, "saved");
  return refusal instanceof SendInterruptedError
    ? `this page was closed before Dispatch confirmed it. ${retry}`
    : `Dispatch has not confirmed it. ${retry}`;
}

/** The writes the indicator shows: every one shown there that Dispatch has not yet made. */
function shown(rows: readonly Write[]): readonly Write[] {
  return rows.filter((row) => row.presentation === "indicator" && row.status !== "made");
}

/** Whether `row` waits its turn behind an earlier write to its target and was never sent. */
export function waitingToSend(row: Write): boolean {
  return row.status === "queued" && !row.sent;
}

/** Whether `row` waits for its person's sign-out in another tab of this browser: none of its
 *  attempts starts until that tab says how it ended (`Writes.holdFor`). A refused one waits for
 *  nothing. */
export function heldForSignOut(row: Write): boolean {
  return row.signingOutElsewhere && row.status !== "refused";
}

/** How much of `row`'s upload has gone up, while its body is still going up on its first try;
 *  `undefined` for any other write. Until an attempt of it has failed, it is uploading, not being
 *  tried again. */
export function uploadedPercent(row: Write): number | undefined {
  return (row.status === "sending" || row.status === "late") && !row.retried
    ? row.uploaded
    : undefined;
}

/** Focuses the first of `candidates` that takes the focus, without scrolling; says whether one
 *  did. */
function focusFirst(candidates: Iterable<HTMLElement | null>): boolean {
  for (const candidate of candidates) {
    candidate?.focus({ preventScroll: true });
    if (candidate !== null && document.activeElement === candidate) return true;
  }
  return false;
}

/**
 * Hands on the focus a press in the panel takes away. A press here can take the control it was
 * made on off the page - Stop trying once its write is refused, Retry or Discard once their row
 * goes, any of them as the panel goes once nothing is pending - and the browser then leaves the
 * focus on the page's body, which loses a keyboard reader's place. So the panel notes the row the
 * focus is in and where the focus came from as it entered the panel, and once the element that
 * held the focus in the panel has left the page while the focus is on the body, the focus goes to
 * the first control that takes it: while the panel is still drawn, one in that element's row (the
 * Retry a stopped write's row offers once Stop trying has gone), else the panel's first; only once
 * the panel is gone or holds none, the element the focus came from, else the page's main region
 * (`app.tsx`'s `main-content`), which the shell focuses on a page change too. A focus the reader
 * takes out of the panel, by a Tab or by a press that leaves the focus outside it, is forgotten,
 * so nothing moves a focus the reader put somewhere else; a press elsewhere that leaves the focus
 * in the panel, as on a control whose `mousedown` is prevented or on a scrollbar, forgets nothing.
 * A focus a pointer press puts in the panel is forgotten too, never noted: Chromium focuses a
 * pressed button and draws it no focus ring, so a hand-off would move that focus, unseen, onto
 * another row's control, and the next Space or Enter would press it - Discard, first in a row
 * waiting to send. A press lasts from its `pointerdown` until its `click` has been dispatched: on
 * a touch screen the browser moves the focus only once the finger lifts, on the `mousedown` it
 * sends after the `pointerup`, which can come well after the press began. A key pressed in
 * between ends it, so the focus the keyboard moves is noted even after a press no `click` ended.
 * Returns the panel's ref.
 */
function usePanelFocusReturn(): RefObject<HTMLElement | null> {
  const panel = useRef<HTMLElement>(null);
  // The element that holds the focus in the panel, the write whose row it is in, and the element
  // that held the focus before it came in.
  const held = useRef<Node | null>(null);
  const heldRow = useRef<string | null>(null);
  const from = useRef<HTMLElement | null>(null);
  useEffect(() => {
    const inPanel = (target: EventTarget | null): target is Node =>
      target instanceof Node && panel.current?.contains(target) === true;
    const forget = () => {
      held.current = null;
      heldRow.current = null;
      from.current = null;
    };
    // Where the pointer press under way began - in the panel or elsewhere - from its
    // `pointerdown` until its `click` has been dispatched, or until a key is pressed; null
    // between presses.
    let press: "panel" | "elsewhere" | null = null;
    const focused = (event: FocusEvent) => {
      const { relatedTarget, target } = event;
      if (press === "panel" || !inPanel(target)) {
        forget();
        return;
      }
      if (held.current === null) {
        from.current = relatedTarget instanceof HTMLElement ? relatedTarget : null;
      }
      held.current = target;
      heldRow.current =
        target instanceof Element
          ? (target.closest("[data-pending-write]")?.getAttribute("data-pending-write") ?? null)
          : null;
    };
    const pressed = (event: PointerEvent) => {
      press = inPanel(event.target) ? "panel" : "elsewhere";
    };
    // A press elsewhere forgets, not a blur: the control that holds the focus is blurred too as
    // the window loses the focus to another tab or app, which moves nothing of the reader's place
    // on the page. Whatever the press does to the focus is done once its `click` has been
    // dispatched, so where it left the focus is read after that, and the press ends there.
    const clicked = () => {
      const ended = press;
      setTimeout(() => {
        press = null;
        if (ended === "elsewhere" && !inPanel(document.activeElement)) forget();
      });
    };
    const keyed = () => {
      press = null;
    };
    document.addEventListener("focusin", focused, true);
    document.addEventListener("pointerdown", pressed, true);
    document.addEventListener("click", clicked, true);
    document.addEventListener("keydown", keyed, true);
    return () => {
      document.removeEventListener("focusin", focused, true);
      document.removeEventListener("pointerdown", pressed, true);
      document.removeEventListener("click", clicked, true);
      document.removeEventListener("keydown", keyed, true);
    };
  }, []);
  // React sends no blur for an element it takes off the page, and the browser moves the focus
  // with no event, so each commit looks: a layout effect, so the focus moves in the commit that
  // takes the element away.
  useLayoutEffect(() => {
    const left = held.current;
    if (left === null || left.isConnected) return;
    const box = panel.current;
    const id = heldRow.current;
    const rowSelector = id === null ? null : `[data-pending-write="${CSS.escape(id)}"]`;
    const row = rowSelector === null ? null : box?.querySelector(rowSelector);
    const back = from.current;
    held.current = null;
    heldRow.current = null;
    from.current = null;
    if (!focusOnDocument()) return;
    const controls = [
      ...(row?.querySelectorAll<HTMLElement>("button") ?? []),
      ...(box?.querySelectorAll<HTMLElement>("button") ?? []),
    ];
    if (focusFirst(controls)) {
      // The focus listener took that move for the focus entering the panel afresh; it came in
      // from where it did before.
      from.current = back;
      return;
    }
    focusFirst([back, document.getElementById("main-content")]);
  });
  return panel;
}

/**
 * The shell's one account of the writes no composer shows (`write-store.ts`'s `indicator`
 * writes): chrome, out of the page's flow in the corner the connection pill shares, and gone while
 * nothing is pending. Its status line counts the writes that went out - `Saving 2 changes…`, then,
 * once one is past its deadline and is being tried again, `Dispatch has not confirmed 2 changes —
 * still trying` - and it names each write the reader can act on or should see: one held for the
 * person's sign-out in another tab, `Waiting for a sign-out in another tab`; one waiting its turn
 * behind another that was never sent, `Waiting to send` with Discard; an upload whose body is
 * still going up, `Uploading… N%`, which is never "still trying" until an attempt of it has
 * failed; a late one with Stop trying; a refused one with why, Retry and Discard. A write still
 * within its deadline is only counted, since there is nothing to do about it yet.
 */
export function PendingWritesIndicator(): ReactNode {
  const writes = useWrites();
  const rows = useWriteSelection("indicator", shown);
  // Before the early return, so it outlives the panel going while nothing is pending.
  const panel = usePanelFocusReturn();
  if (rows.length === 0) return null;
  const out = rows.filter(
    (row) => row.status !== "refused" && !waitingToSend(row) && !heldForSignOut(row)
  );
  const late = out.some((row) => row.status === "late" && uploadedPercent(row) === undefined);
  const named = rows.filter(
    (row) =>
      row.status === "late" ||
      row.status === "refused" ||
      heldForSignOut(row) ||
      waitingToSend(row) ||
      uploadedPercent(row) !== undefined
  );
  const count = out.length === 1 ? "1 change" : `${out.length} changes`;
  const actionable = rows.some((row) => row.status === "late" || row.status === "refused");
  return (
    <section
      aria-label="Pending changes"
      className={`w-full max-w-md rounded-xl border px-3 py-2 text-xs shadow-lg ${card} ${
        actionable ? calloutWarningBorder : borderDefault
      }`}
      data-testid="pending-writes"
      ref={panel}
    >
      {out.length === 0 ? null : (
        <p aria-live="polite" className={`font-medium ${textSecondaryOnSurface}`} role="status">
          {late ? `Dispatch has not confirmed ${count} — still trying` : `Saving ${count}…`}
        </p>
      )}
      {named.length === 0 ? null : (
        <ul className={`max-h-48 space-y-1 overflow-y-auto ${out.length === 0 ? "" : "mt-2"}`}>
          {named.map((row) => {
            const uploaded = uploadedPercent(row);
            return (
              <li
                className="flex min-h-8 flex-wrap items-center gap-x-2 gap-y-1"
                data-pending-write={row.id}
                key={row.id}
              >
                {row.status === "refused" ? (
                  <>
                    <span className={`min-w-0 flex-1 ${dangerText}`}>
                      Not saved: {row.label} — {refusalReason(row.keyed, row.refusal)}
                    </span>
                    <button
                      className={secondaryButtonCompact}
                      onClick={() => writes.retry(row.id)}
                      type="button"
                    >
                      Retry
                    </button>
                    <button
                      className={secondaryButtonCompact}
                      onClick={() => writes.discard(row.id)}
                      type="button"
                    >
                      Discard
                    </button>
                  </>
                ) : heldForSignOut(row) ? (
                  <>
                    <span className={`min-w-0 flex-1 ${textSecondaryOnSurface}`}>
                      {row.label} — Waiting for a sign-out in another tab
                    </span>
                    {waitingToSend(row) ? (
                      <button
                        className={secondaryButtonCompact}
                        onClick={() => writes.discard(row.id)}
                        type="button"
                      >
                        Discard
                      </button>
                    ) : null}
                  </>
                ) : waitingToSend(row) ? (
                  <>
                    <span className={`min-w-0 flex-1 ${textSecondaryOnSurface}`}>
                      {row.label} — Waiting to send
                    </span>
                    <button
                      className={secondaryButtonCompact}
                      onClick={() => writes.discard(row.id)}
                      type="button"
                    >
                      Discard
                    </button>
                  </>
                ) : (
                  <>
                    <span className={`min-w-0 flex-1 ${textSecondaryOnSurface}`}>
                      {uploaded === undefined
                        ? row.label
                        : `${row.label} — Uploading… ${uploaded}%`}
                    </span>
                    {row.status === "late" ? (
                      <StopTrying onStop={() => writes.stop(row.id)} stopping={row.stopping} />
                    ) : null}
                  </>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}
