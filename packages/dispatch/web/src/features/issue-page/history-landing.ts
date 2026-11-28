import { useQuery } from "@tanstack/react-query";
import { type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import { whoAmIQuery } from "../../api/queries";
import { EVENT_FRESH } from "../refs/reference-target";
import { ENTRY_CONTROLS } from "./details-leaves";
import { type HistoryDay, type HistoryTurn, threadOf } from "./history-model";
import { useMarginDrawer } from "./margin-slot";
import { type HeldLanding, LANDING_WAIT_MS, type LandingSight, newOwnNode } from "./outline-writer";

/** The turn `id` as the history drawn under `root` draws it, where it has a box: undefined where
 *  it is not drawn yet, and where Details is still closed around the list, which gives it none.
 *  The address's landing finds it (`item`) and so does a write's (`useHistoryLanding`). */
export function drawnTurn(root: ParentNode | null, id: string): HTMLElement | undefined {
  const turn = root?.querySelector<HTMLElement>(`[data-history-item="${CSS.escape(id)}"]`);
  return turn == null || turn.getClientRects().length === 0 ? undefined : turn;
}

/**
 * Lands a write from the line at the foot that answered a turn of a thread the history drawn in
 * `list` holds, which is the history's to land (`landing`, `HeldLanding.in`), as every other
 * write's is the outline's: the reader is taken to their new turn in that thread - or, for a
 * `self` landing, back to the turn they answered - focused, and wholly in Details' view by the
 * least scroll of Details' box, never of the page, as a hand-on brings what it focuses there
 * (`handTo`), the turn first (`bringIntoBox`) and then its first control, which takes the focus.
 * The send has landed when the line asks, but the read that draws the new turn is still out, so
 * the turns the thread holds are noted first, and the reader's turn that is new is gone to once a
 * read has brought it and the list has drawn it, by the one rule the outline goes to their new
 * row by (`newOwnNode`). A landing the writer already holds as the history mounts was asked for
 * while it was not drawn, with Details shut, and the reader has been elsewhere since, unless the
 * writer opened Details for it, on a phone, where a turn's Reply shut it
 * (`HeldLanding.opensDetails`): the history is drawn from the commit that holds that landing,
 * down to the thread the landing answers (`IssueHistory`'s `landingPlace`), and lands it as one
 * asked for while it is drawn. That thread can stand on a page of the log not read yet: the log's
 * pages can have left the cache while Details was shut, and the log read anew holds its first page
 * alone, which leaves out a thread older than every event on it (`HistoryInput.loadedFrom`). The
 * landing then waits while the history reads on for it (`readsOnForThread`, as `IssueHistory`'s
 * `seeksThread` reads on), and no longer: once a read of the log has failed, whose Retry line the
 * reader has, or the log has ended without that thread, it has no turn to go to. Nor has one
 * asked for while Details is shut around the list, nor one whose thread this history no longer
 * holds, as an agents' post a person wrote in, which the outline draws from then on. Each of
 * these ends with the focus where it is, as the outline ends a landing under a node it draws no
 * row of, but for one whose reply takes that node out of this history
 * (`HeldLanding.leavesHistory`), and so does one whose new turn no read has drawn once the read's
 * wait is out (`LANDING_WAIT_MS`), and one still waiting as the history goes.
 */
export function useHistoryLanding({
  bringIntoBox,
  days,
  endLanding,
  handTo,
  landing,
  list,
  readsOnForThread,
}: {
  readonly bringIntoBox: (element: HTMLElement) => void;
  readonly days: readonly HistoryDay[];
  /** Ends the writer's landing (`OutlineWriter.landed`); undefined where no writer is drawn. */
  readonly endLanding: (() => void) | undefined;
  readonly handTo: (element: HTMLElement) => void;
  /** The writer's landing where it is the history's (`HeldLanding.in`). */
  readonly landing: HeldLanding | undefined;
  readonly list: RefObject<HTMLElement | null>;
  /** Whether the history still reads the log on for the thread of `landing`, one the writer
   *  opened Details for, which `days` does not hold yet (`IssueHistory`'s `seeksThread`): while
   *  an older page of the log is left to read and no read of the log has failed. False for every
   *  other landing. */
  readonly readsOnForThread: boolean;
}): void {
  const drawer = useMarginDrawer();
  const landingAtMount = useRef(landing);
  const endsAtMount = landing === landingAtMount.current && landing?.opensDetails !== true;
  const landingFrom = useRef<LandingSight | undefined>(undefined);
  // The reader, whose own new turn a landing goes to, as the outline reads them: read only while
  // a landing waits, and answered by the shell's own read of them.
  const signedIn = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH, enabled: landing !== undefined });
  const reader = signedIn.data?.login;
  useLayoutEffect(() => {
    if (landing === undefined || endLanding === undefined) return;
    if (endsAtMount || drawer?.open === false) {
      endLanding();
      return;
    }
    const answered = landing.under?.id;
    const thread = answered === undefined ? undefined : threadOf(days, answered);
    if (thread === undefined) {
      if (!readsOnForThread) endLanding();
      return;
    }
    const turns = [thread.root, ...thread.replies];
    let to: HistoryTurn | undefined;
    if (landing.to === "self") {
      to = turns.find((turn) => turn.id === answered);
    } else {
      const look = newOwnNode(landing, landingFrom.current, turns, reader);
      if ("sight" in look) {
        landingFrom.current = look.sight;
        return;
      }
      to = look.made;
    }
    const landedOn = to === undefined ? undefined : drawnTurn(list.current, to.id);
    if (landedOn === undefined) return;
    endLanding();
    bringIntoBox(landedOn);
    const control = landedOn.querySelector<HTMLElement>(ENTRY_CONTROLS);
    if (control !== null) handTo(control);
  });
  // A landing waits for the read that draws the reader's new turn no longer than
  // `LANDING_WAIT_MS`, and no longer than the history it waits in: one still waiting as the
  // history goes - Details shut before a read drew that turn, as a reader on a phone shuts the
  // sheet the writer opened for it - ends then. Only this history can end it, as the outline
  // takes none of its landings, and held on, it would be taken up by the next history to mount at
  // any later open of Details: one the writer opened Details for would draw that history down to
  // its thread and take a new turn of the reader's there. A landing that ends, or a newer one in
  // its place, runs this effect again, and the cleanup of the run before ends nothing while the
  // history is still on the page. Once the history has gone, that cleanup ends the landing the
  // writer holds a microtask later, whichever it is (`endLanding` is the writer's `landed`, which
  // names no landing to end): that is the one this run waited on, since no path today sets a new
  // landing in the task that takes the history off the page - on a phone `land` opens Details and
  // never shuts it (`outline-writer.tsx`), and a turn's Reply shuts Details and sets no landing
  // (`history-entries.tsx`). React runs a mount's effects a second time under StrictMode, their
  // cleanups between, within the same task, so whether the history went is read a microtask on
  // (`onPage`).
  const onPage = useRef(false);
  useEffect(() => {
    onPage.current = true;
    return () => {
      onPage.current = false;
    };
  }, []);
  useEffect(() => {
    if (landing === undefined || endsAtMount || endLanding === undefined) return;
    const late = window.setTimeout(endLanding, LANDING_WAIT_MS);
    return () => {
      window.clearTimeout(late);
      queueMicrotask(() => {
        if (!onPage.current) endLanding();
      });
    };
  }, [endLanding, endsAtMount, landing]);
}
