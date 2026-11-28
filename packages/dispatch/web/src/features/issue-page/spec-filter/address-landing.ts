import { type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import { landOnRow } from "../IssueTree";
import { useFollowing } from "../slots/following";

/**
 * Puts the focus on the item of `tree` that draws node `id`, the node the address names, once
 * per following of the address (R19), as the outline lands on one (`IssueTree.tsx`'s
 * `landOnRow`): the focus comes in the next frame and brings the item on screen, and the hold
 * keeps it there, until the reader acts, while the page grows around it. It is the landing of the
 * Spec view and of the Verification view.
 *
 * A view gives `id` only once every read the page's tree is built from has settled
 * (`readsSettled`), for one reason the hold and the reader's acts do not cover: until then the
 * stage may still draw the outline in the view's place. The view tells the stage whether it draws
 * the node only once those reads have settled (`onAddressDrawn`, `useTellsAddressDrawn`), and a
 * read the view draws no line of its own for that stands refused then - the comments', the
 * messages' - makes the stage take the view off the page and draw the outline, which lands on the
 * node itself. A landing before that would move the focus and the page to an item the stage then
 * takes away, and the outline's landing would move them again.
 *
 * The item is not on the page until the reads that hold it have answered, so this looks again
 * each time the view draws other rows (`drawn`), and waits while it draws none; the address is
 * taken only once its item is found. The reader's first act since they followed the address ends
 * the landing (`acts`), as it ends the outline's: a reader who acted before the reads settled - a
 * key, a turn of the wheel, a press on another control, while the stage gave the view's tab stop
 * no focus, its landing undecided (`ViewStage`) - keeps the focus where they put it and the page
 * where they had it, and the landing does nothing to the page: no focus, no scroll, no hold. The
 * acts are watched from the following's start and not the landing's, since a read that answered
 * late would otherwise pull them back to the address's item. The landing is keyed on the
 * following and the node, as the outline's is: a link to the node the reader moved away from,
 * followed again, lands again, and the rows drawn again at the same following land nowhere again.
 *
 * Answers what holds the landing and then the item it landed on (`landOnRow`'s `held`), which a
 * hold the view starts of its own takes the place of, so one item is held at a time. The view
 * leaving the page ends it.
 */
export function useAddressLanding({
  acts,
  drawn,
  id,
  tree,
}: {
  /** The reader's acts since they followed the address: a controller their first act aborts
   *  (`StageSlot`'s `ReaderActsWatch`). None where the view is drawn without the stage, which
   *  then lands as where the reader made no act. */
  acts: RefObject<AbortController | undefined> | undefined;
  /** The ids of the rows the view draws, joined by spaces, and empty while it draws none: a
   *  string, which compares by value, so the landing looks again only when the rows change. */
  drawn: string;
  /** The node the address names, once the view may land on it; undefined until then. */
  id: string | undefined;
  tree: RefObject<HTMLElement | null>;
}): RefObject<AbortController | undefined> {
  const held = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => held.current?.abort(), []);
  const following = useFollowing();
  const taken = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (id === undefined || drawn === "") return;
    const landingKey = `${following}:${id}`;
    if (taken.current === landingKey) return;
    const item = treeItemOf(tree.current, id);
    if (item === null) return;
    taken.current = landingKey;
    // The landing ends as it starts: the reader has acted since the following, and the page
    // stays as they left it.
    if (acts?.current?.signal.aborted === true) return;
    landOnRow(item, held);
  }, [acts, drawn, following, id, tree]);
  return held;
}

/**
 * Whether the reader has acted since they followed the address (`following`), as read during a
 * render from the controller the stage hands the view (`acts`, `StageSlot`'s `ReaderActsWatch`).
 *
 * The stage starts each following's controller as that following's first commit is laid out, after
 * its render, which still reads the last following's controller; this hook notes its following as
 * each of its commits is laid out, after the stage. So a render of a following the view has not
 * committed in yet reads `acted: false`: the reader cannot have acted in it yet.
 */
export function useAddressActed(acts: RefObject<AbortController | undefined> | undefined): {
  readonly acted: boolean;
  readonly following: string;
} {
  const following = useFollowing();
  const committed = useRef<string | undefined>(undefined);
  useLayoutEffect(() => {
    committed.current = following;
  }, [following]);
  return {
    acted: committed.current === following && acts?.current?.signal.aborted === true,
    following,
  };
}

/** The item of `tree` that draws node `id` - a statement, a decision cell, a row of the
 *  Verification view - or null where none does. */
export function treeItemOf(tree: HTMLElement | null, id: string): HTMLElement | null {
  return (
    tree?.querySelector<HTMLElement>(`[data-node-id="${CSS.escape(id)}"][role="treeitem"]`) ?? null
  );
}
