import { type RefObject, useEffect, useRef } from "react";
import { useNavigationType } from "react-router-dom";

import { scrollElementToTop } from "../doc/scroll-block";
import { HISTORY_ENTRY_ATTRIBUTE } from "../margin/details-focus-keeper";
import type { ViewportAnchor } from "../shell/ViewportAnchor";
import type { HistoryInput } from "./history-model";
import { scrollingAncestor } from "./scroll-box";

/** The event the history's heading raises, bubbling, once a following of `/activity`
 *  (`useFollowing`) has brought it to the top of Details' view. That scroll is the landing that
 *  following asked for, not a move that takes the reader from their place: the hold of the place
 *  `t v` lands the focus at keeps the focused history where this scroll leaves it
 *  (`details-more.tsx`). */
export const HISTORY_HEADING_LANDED_EVENT = "history-heading-landed";

/** A turn of a thread the history holds that an address names (`/issues/<key>/comments/<id>` or
 *  `/messages/<id>`), with the following of that address it was named at (`useFollowing`). */
export interface AddressedTurn {
  readonly following: string;
  readonly id: string;
}

/** What a page address names in the history (`DetailsHistory`), with the following of that
 *  address it was named at (`useFollowing`): a handoff, by its id, at
 *  `/issues/<key>/handoffs/<id>`, or the newest run, at `/issues/<key>/steps`
 *  (`HistoryInput.addressed`). */
export type AddressedHistory = NonNullable<HistoryInput["addressed"]> & {
  readonly following: string;
};

/** The turn `id` as the history drawn under `root` draws it, where it has a box: undefined where
 *  it is not drawn yet, and where Details is still closed around the list, which gives it none.
 *  The address's landing finds it (`item`) and so does a write's (`landing`). */
export function drawnTurn(root: ParentNode | null, id: string): HTMLElement | undefined {
  const turn = root?.querySelector<HTMLElement>(`[data-history-item="${CSS.escape(id)}"]`);
  return turn == null || turn.getClientRects().length === 0 ? undefined : turn;
}

/**
 * Brings `target` - an entry of the history, a turn of a thread, or the part of an entry that an
 * address names - into Details' view, and has the history's anchor keep it there. A target taller
 * than Details' view lands at the top of that view, so its head is read first; a shorter one in
 * the middle of it. The anchor keeps the target's own place, inside `entry`, the row of the history
 * it stands in (`keepRow`'s `element`), or the row's own place where the target is the row. A
 * change of height in the same frame, whether the scroll's event is read before it or after it,
 * then moves Details by as much rather than moving the target, until the reader's next act names
 * their place. That holds for a change inside the entry too, such as a turn above a reply whose
 * words finish drawing after the scroll, which leaves the entry's top where it was and moves the
 * reply. The place the reader's hand was on before the landing is often wholly outside the view
 * after it, and is no place they can see (`rowToKeep`).
 */
function landEntry(
  anchor: ViewportAnchor | null,
  target: HTMLElement,
  entry: HTMLElement | null
): void {
  const box = scrollingAncestor(target);
  const tall = box !== null && target.getBoundingClientRect().height > box.clientHeight;
  target.scrollIntoView({ block: tall ? "start" : "center" });
  anchor?.keepRow(entry, target === entry ? undefined : target);
}

/** Brings the heading to the top of the box Details scrolls in, and says it has landed
 *  (`HISTORY_HEADING_LANDED_EVENT`). Only the box Details scrolls in moves, never the page under
 *  it; the reader's own scroll after that is theirs. */
function landHeading(element: HTMLElement): void {
  const box = scrollingAncestor(element);
  if (box === null) return;
  scrollElementToTop(element, box);
  element.dispatchEvent(new CustomEvent(HISTORY_HEADING_LANDED_EVENT, { bubbles: true }));
}

/** The heading, where it has a box and the log under it is drawn, else null: Details still
 *  closed around it, or the panel not yet placed in the drawer, gives it no box, and before the
 *  log is drawn Details has nothing to scroll the heading up by. */
function drawnHeading(heading: HTMLElement | null, listDrawn: boolean): HTMLElement | null {
  return heading !== null && heading.getClientRects().length > 0 && listDrawn ? heading : null;
}

/** What one landing holds of the followings of its address (`useFollowing`): it lands once for
 *  each. */
interface Landing {
  /** Notes that the following has landed, and says whether the reader came back to it: a Back or
   *  a Forward to an address (`POP`) once a following of it has landed, which brings nothing to
   *  the landings of the heading and of the entry an address names. A reader who comes back to
   *  the history this list drew there before - Details left open beside a view, or at a turn's
   *  address - finds Details where they left it, and so does one who types a fragment at the
   *  address. */
  readonly note: () => boolean;
  /** Whether nothing is left to land: no address is followed, or its following has landed. */
  readonly settled: () => boolean;
}

/** The landing of the address at `following`, or none where no address is followed. */
function useLanding(following: string | undefined): Landing {
  const navigation = useNavigationType();
  const landed = useRef<string | undefined>(undefined);
  return {
    note: () => {
      const comingBack = navigation === "POP" && landed.current !== undefined;
      landed.current = following;
      return comingBack;
    },
    settled: () => following === undefined || landed.current === following,
  };
}

/**
 * How the history brings what a page address names into Details' view, once for each following
 * of that address (`useFollowing`), as the list first draws it there: the same address followed
 * again, a link to it pressed in the history among them, brings it into view again however far
 * the reader has read since. Returns whether every landing the history's address asks for has
 * landed, which the bodies' loader waits for.
 *
 * The turn an address names (`item`) lands once the list draws it. A turn with no box yet - not
 * drawn yet, or Details still closed around the list, which the page opens a commit after this
 * one mounts - is not landed on, so the render that draws it lands it (`landEntry`).
 *
 * `/activity` opens Details at the history: its heading (`activityFollowing`) lands on the first
 * render it has a box and the log under it is drawn. Each entry the page makes at the address is
 * a following of its own - `t v` pressed there, a link to it followed there - so each brings the
 * heading back to the top of Details' view, however far down Details the reader has read. A
 * following the browser steps to itself (`POP`) once this list has landed a heading brings
 * nothing (`Landing.note`). Once the heading has landed it says so
 * (`HISTORY_HEADING_LANDED_EVENT`).
 *
 * A handoff's page address lands on the entry that holds that handoff (`handoffId`), and `/steps`
 * on the newest run (`addressed`, `addressedEntry`), on the first render that draws that entry:
 * a handoff by its own line and fields inside a run's entry, and kept there by the history's
 * anchor as the bodies of the entries around it are read and drawn. An address whose handoff
 * the answered list does not hold, or whose list did not read, and `/steps` where no run is
 * read, land on the heading, as `/activity` does. Back or Forward to an address once this list
 * has landed one brings nothing, as at `/activity`.
 */
export function useHistoryLandings({
  activityFollowing,
  addressed,
  addressedEntry,
  anchor,
  handoffId,
  heading,
  item,
  listDrawn,
}: {
  /** The following of `/activity` the reader is on, or undefined at any other address. */
  activityFollowing: string | undefined;
  /** The handoff or the newest run the page address names. */
  addressed: AddressedHistory | undefined;
  /** The id of the entry `addressed` names, where the list holds one. */
  addressedEntry: string | undefined;
  /** The history's anchor, which keeps the reader's place in Details. */
  anchor: RefObject<ViewportAnchor | null>;
  /** The id of the handoff `addressed` names, where it names one. */
  handoffId: string | undefined;
  heading: RefObject<HTMLHeadingElement | null>;
  /** The turn the address names. */
  item: AddressedTurn | undefined;
  /** Whether the list under the heading is drawn. */
  listDrawn: boolean;
}): () => boolean {
  const turnLanding = useLanding(item?.following);
  const headingLanding = useLanding(activityFollowing);
  const entryLanding = useLanding(addressed?.following);
  useEffect(() => {
    if (item === undefined || turnLanding.settled()) return;
    const turn = drawnTurn(document, item.id);
    if (turn === undefined) return;
    // A Back or Forward to the address lands the turn again, as any following does.
    turnLanding.note();
    landEntry(anchor.current, turn, turn.closest<HTMLElement>(`[${HISTORY_ENTRY_ATTRIBUTE}]`));
  });
  useEffect(() => {
    const element = drawnHeading(heading.current, listDrawn);
    if (element === null || headingLanding.settled()) return;
    if (!headingLanding.note()) landHeading(element);
  });
  useEffect(() => {
    const element = drawnHeading(heading.current, listDrawn);
    if (element === null || addressed === undefined || entryLanding.settled()) return;
    const entry =
      addressedEntry === undefined
        ? null
        : (element.parentElement?.querySelector<HTMLElement>(
            `[${HISTORY_ENTRY_ATTRIBUTE}="${CSS.escape(addressedEntry)}"]`
          ) ?? null);
    if (addressedEntry !== undefined && (entry === null || entry.getClientRects().length === 0)) {
      return;
    }
    if (entryLanding.note()) return;
    if (entry === null) {
      landHeading(element);
      return;
    }
    const line =
      handoffId === undefined
        ? undefined
        : entry.querySelector(`[data-handoff-line="${CSS.escape(handoffId)}"]`)?.parentElement;
    landEntry(anchor.current, line ?? entry, entry);
  });
  return () => turnLanding.settled() && headingLanding.settled() && entryLanding.settled();
}
