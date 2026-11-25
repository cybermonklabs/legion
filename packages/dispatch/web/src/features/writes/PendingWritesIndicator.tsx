import {
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
} from "react";

import { apiErrorMessage, NETWORK_ERROR_MESSAGE } from "../../api/client";
import { ARM_ON_PRESS, causedByReader, inSightNow, outOfSight } from "../../components/focus-place";
import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import {
  borderDefault,
  calloutWarningBorder,
  card,
  dangerText,
  secondaryButtonCompact,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { focusOnDocument } from "../shell/roving";
import { conflictWords } from "./conflicts";
import { retryNote, SendInterruptedError, SendStoppedError } from "./keyed-write";
import { StopTrying } from "./StopTrying";
import { useWriteSelection, useWrites, type Write, writeDraft } from "./write-store";

/** Why `refusal` left a write unsaved, in the reader's words. One that ended without Dispatch's
 *  verdict - stopped, its key run out, or interrupted by a reload - may have landed, and what its
 *  Retry does is `retryNote`'s, by whether the write goes under its key (`keyed`, `Write.keyed`):
 *  a `PUT` or a `DELETE` sends none, so no Retry of one asks under a key. */
function refusalReason(keyed: boolean, refusal: Error): string {
  if (!(refusal instanceof SendStoppedError)) {
    return apiErrorMessage(refusal, NETWORK_ERROR_MESSAGE);
  }
  const retry = retryNote(keyed, refusal, "saved");
  return refusal instanceof SendInterruptedError
    ? `this page was closed before Dispatch confirmed it. ${retry}`
    : `Dispatch has not confirmed it. ${retry}`;
}

/** The reader's own text a write a conflict refused carried, drawn as they wrote it, with a way
 *  to copy it: no Retry sends it again, and the reader may want it to write again by hand. */
function DraftToCopy({ text }: { text: string }): ReactNode {
  const { copy, status } = useCopyFeedback();
  const id = useId();
  return (
    <span className="block basis-full space-y-1">
      <span className="block" id={id}>
        Your text:
      </span>
      <span
        aria-labelledby={id}
        className={`block max-h-32 overflow-y-auto whitespace-pre-wrap border-l-2 pl-2 ${borderDefault} ${textSecondaryOnSurface}`}
        role="note"
      >
        {text}
      </span>
      <button className={secondaryButtonCompact} onClick={() => copy(text)} type="button">
        {status === "copied"
          ? "Copied"
          : status === "idle"
            ? "Copy your text"
            : "Copy failed - select the text"}
      </button>
    </span>
  );
}

/** The writes the indicator shows: every one shown there that Dispatch has not yet made, but for
 *  a refusal a surface mounted now settles with the reader (`claimed`), which is that surface's. */
function shown(rows: readonly Write[]): readonly Write[] {
  return rows.filter(
    (row) =>
      row.presentation === "indicator" &&
      row.status !== "made" &&
      !(row.status === "refused" && row.claimed)
  );
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

/** The buttons of `row`, then the rest of the panel's (`box`, which holds `row`), each in sight
 *  now (`inSightNow`) and each measured once, as the hand-off reaches it: it stops at the first
 *  that takes the focus. */
function* buttonsInSight(
  row: Element | null | undefined,
  box: Element | null,
  clips: Map<Element, boolean>
): Generator<HTMLElement> {
  for (const control of row?.querySelectorAll<HTMLElement>("button") ?? []) {
    if (inSightNow(control, clips)) yield control;
  }
  for (const control of box?.querySelectorAll<HTMLElement>("button") ?? []) {
    if (row?.contains(control) !== true && inSightNow(control, clips)) yield control;
  }
}

/**
 * Hands on the focus a press of the reader's in the panel takes away. A press here can take the
 * control it was made on off the page - Stop trying once its write is refused, Retry or Discard
 * once their row goes, any of them as the panel goes once nothing is pending - and the browser
 * then leaves the focus on the page's body, which loses a keyboard reader's place. So the panel
 * arms the hand-off at its root for each press made in it (`armOnPress`), notes the row the focus
 * is in and where the focus came from as it entered the panel, and once the element that held the
 * focus in the panel has left the page while the focus is on the body, it reads who caused the
 * loss. A loss the reader caused (`causedByReader`), of a control they had not scrolled out of
 * their sight (`outOfSight`), moves the focus, with no scroll, to the first control the reader can
 * see now (`inSightNow`): while the panel is still drawn, one in that element's row (the Retry a
 * stopped write's row offers once Stop trying has gone), else one of the panel's, passing over a
 * row scrolled out of the panel's list; only once none of the panel's is in sight, the element the
 * focus came from, where it is in sight, else the page's main region (`app.tsx`'s
 * `main-content`), which the shell focuses on a page change too. A loss the reader did not cause -
 * a write that lands by itself, a refusal that comes in, with nothing armed - leaves the focus
 * where the browser puts it, on the body, and scrolls nothing: a control they did not choose, and
 * may not see, never takes it.
 * A focus the reader takes out of the panel, by a Tab or by a press that leaves the focus outside
 * it, is forgotten, so nothing moves a focus the reader put somewhere else; a press elsewhere that
 * leaves the focus in the panel, as on a control whose `mousedown` is prevented or on a scrollbar,
 * forgets nothing. A focus a pointer press puts in the panel is forgotten too, never noted:
 * Chromium focuses a pressed button and draws it no focus ring, so a hand-off would move that
 * focus, unseen, onto another row's control, and the next Space or Enter would press it - Discard,
 * first in a row waiting to send. A press lasts from its `pointerdown` until its `click` has been
 * dispatched, or it is cancelled: on a touch screen the browser moves the focus only once the
 * finger lifts, on the `mousedown` it sends after the `pointerup`, which can come well after the
 * press began; and a touch that scrolls the panel's list ends in a `pointercancel` and no `click`,
 * after which a press still under way would forget every focus that enters the panel, the
 * hand-off's own too. A key pressed in between ends it as well, so the focus the keyboard moves is
 * noted even after a press no `click` ended.
 * Returns the panel's ref.
 */
function usePanelFocusReturn(): RefObject<HTMLElement | null> {
  const panel = useRef<HTMLElement>(null);
  // The element that holds the focus in the panel, the write whose row it is in, and the element
  // that held the focus before it came in.
  const held = useRef<Element | null>(null);
  const heldRow = useRef<string | null>(null);
  const from = useRef<HTMLElement | null>(null);
  // Forgets all three at once: a focus the reader takes out of the panel, and the hand-off below
  // once it has read them.
  const forget = useCallback(() => {
    held.current = null;
    heldRow.current = null;
    from.current = null;
  }, []);
  useEffect(() => {
    const inPanel = (target: EventTarget | null): target is Element =>
      target instanceof Element && panel.current?.contains(target) === true;
    // Where the pointer press under way began - in the panel or elsewhere - from its
    // `pointerdown` until its `click` has been dispatched, until it is cancelled, or until a key
    // is pressed; null between presses.
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
        target.closest("[data-pending-write]")?.getAttribute("data-pending-write") ?? null;
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
    const endPress = () => {
      press = null;
    };
    document.addEventListener("focusin", focused, true);
    document.addEventListener("pointerdown", pressed, true);
    document.addEventListener("click", clicked, true);
    document.addEventListener("pointercancel", endPress, true);
    document.addEventListener("keydown", endPress, true);
    return () => {
      document.removeEventListener("focusin", focused, true);
      document.removeEventListener("pointerdown", pressed, true);
      document.removeEventListener("click", clicked, true);
      document.removeEventListener("pointercancel", endPress, true);
      document.removeEventListener("keydown", endPress, true);
    };
  }, [forget]);
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
    forget();
    if (!focusOnDocument() || outOfSight(left) || !causedByReader(left)) return;
    // One read of each box's style for the whole hand-off (`inSightNow`).
    const clips = new Map<Element, boolean>();
    if (focusFirst(buttonsInSight(row, box, clips))) {
      // The focus listener took that move for the focus entering the panel afresh; it came in
      // from where it did before.
      from.current = back;
      return;
    }
    focusFirst([
      back !== null && inSightNow(back, clips) ? back : null,
      document.getElementById("main-content"),
    ]);
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
 * failed; a late one with Stop trying; a refused one with why, Retry and Discard. A refusal only
 * the reader can settle (`isConflict`) says why in its own words and offers no Retry, since
 * Dispatch refuses its body the same way every time: the reader's own text the write carried is
 * drawn as written, to copy, beside Discard. One a page mounted now settles with the reader
 * (`Writes.claim`) is that page's, and stands here again once the page goes. A write still within
 * its deadline is only counted, since there is nothing to do about it yet.
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
      {...ARM_ON_PRESS}
      ref={panel}
    >
      {out.length === 0 ? null : (
        <p aria-live="polite" className={`font-medium ${textSecondaryOnSurface}`} role="status">
          {late ? `Dispatch has not confirmed ${count} — still trying` : `Saving ${count}…`}
        </p>
      )}
      {named.length === 0 ? null : (
        // A box that scrolls is a Tab stop of its own in Firefox, so it is a region with a name,
        // not a stop that reads as nothing: a row of an upload in progress or of a write held for
        // a sign-out holds no control, and a list of only those has no other stop to scroll it
        // from. Its name is not the panel's, "Pending changes", so the two regions read apart.
        <section
          aria-label="Each pending change"
          className={`max-h-48 overflow-y-auto ${out.length === 0 ? "" : "mt-2"}`}
        >
          <ul className="space-y-1">
            {named.map((row) => {
              const uploaded = uploadedPercent(row);
              const conflict = row.status === "refused" ? conflictWords(row.refusal) : undefined;
              const draft = conflict === undefined ? undefined : writeDraft(row);
              return (
                <li
                  className="flex min-h-8 flex-wrap items-center gap-x-2 gap-y-1"
                  data-pending-write={row.id}
                  key={row.id}
                >
                  {row.status === "refused" ? (
                    <>
                      <span className={`min-w-0 flex-1 ${dangerText}`}>
                        {conflict ?? "Not saved"}: {row.label}
                        {conflict === undefined
                          ? ` — ${refusalReason(row.keyed, row.refusal)}`
                          : null}
                      </span>
                      {draft === undefined ? null : <DraftToCopy text={draft} />}
                      {conflict === undefined ? (
                        <button
                          className={secondaryButtonCompact}
                          onClick={() => writes.retry(row.id)}
                          type="button"
                        >
                          Retry
                        </button>
                      ) : null}
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
        </section>
      )}
    </section>
  );
}
