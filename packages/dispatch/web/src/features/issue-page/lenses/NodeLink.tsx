import type { MouseEvent, ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { pageMutedText } from "../../../theme/classes";
import { isPlainClick } from "../../refs/picture-lightbox";
import { authorText, pageChrome } from "../page-structure";
import type { LensChip } from "./lens-drafts";
import { lensPlaceOf, rowPathIn } from "./lens-reads";

/**
 * One chip of a lens line: a lens, a check or a constraint, by its words; or a node a fact line
 * names (`LensLines.tsx`'s `FactWords`), by the words the line names it by. Where the tree read
 * gives the target's id, the words are the link to its row on this issue's page (`?node=`), drawn
 * as `NodeRow.tsx`'s `Chip` draws its link, in the view that draws the row (R7): in the open
 * outline where it draws it - Graph, which draws every row, and Conversation, but for a target
 * only Graph draws (`LensChip.graphOnly`), as an issue constraint is in Conversation, or one no
 * page read so far holds - and in Graph from anywhere else: from Conversation for such a target,
 * and from the Spec view. The issue's key is the address's own (`lensPlaceOf`), so the link works
 * in each view alike. A target with no id, or an address that names no issue, draws the words
 * alone. A chip's words are whole, and wrap inside the line they stand in, as `NodeRow.tsx`'s
 * `ParentLink` does.
 *
 * A press on a link to the open outline also focuses the target's row where the tree the chip
 * stands in draws it already (`focusDrawnRow`), at every press: the address puts the focus on its
 * node once, and no more after the reader's first act, so a second press on the same chip, once
 * the reader has moved on, would otherwise move nothing. A link to Graph from another view only
 * goes there, and Graph puts the focus on the row as its address names it.
 *
 * The link is out of the tab order, as every other control inside a row is, unless `tabStops`
 * holds: `f` steps to it and Enter follows it (`useTreeKeymap.ts`'s `rowStops`). The words are
 * one element right inside the chip, so the marks of a node's state and visibility can stand
 * after them. They are the author's (`authorText`), but where the chip marks them as the page's
 * own (`chip.chrome`: the page's word for a lens whose kind has no words, and for the kind of a
 * node no page read so far holds), as `NodeRow.tsx`'s `Chip` marks the page's word for a
 * document.
 */
export function NodeLink({ chip, tabStops }: { chip: LensChip; tabStops: boolean }): ReactNode {
  const { pathname, search } = useLocation();
  const id = chip.id;
  const place = id === undefined ? undefined : lensPlaceOf(pathname, search);
  const marks = chip.chrome === true ? pageChrome : authorText;
  const view =
    place?.outline === "conversation" && chip.graphOnly !== true ? "conversation" : "graph";
  return (
    <span className="min-w-0 [overflow-wrap:anywhere]" data-node-chip="">
      {id === undefined || place === undefined ? (
        <span data-chip-name="" {...marks}>
          {chip.words}
        </span>
      ) : (
        <Link
          className={`dispatch-inline-link font-medium underline decoration-page-bullet underline-offset-[3px] ${pageMutedText}`}
          data-chip-name=""
          onClick={view === place.outline ? (event) => focusDrawnRow(event, id) : undefined}
          tabIndex={tabStops ? undefined : -1}
          to={rowPathIn(view, place.issueKey, id)}
          {...marks}
        >
          {chip.words}
        </Link>
      )}
    </span>
  );
}

/**
 * Focuses, where it stands, the row the tree the chip stands in (`role="tree"`) draws for the
 * node whose id in the tree read is `id`, as `IssueTree.tsx`'s `reach` focuses a row it finds
 * drawn; the link then goes to the address as it always does. A row carries its node's read id
 * (`data-node-read-id`, `TreeNode.readId`), so the row is found by the id the chip names and no
 * second rule of tree ids is needed. A press that opens the link elsewhere - with Control, Meta,
 * Shift or Alt held, or with a button other than the first - leaves the focus where it is.
 */
function focusDrawnRow(event: MouseEvent<HTMLAnchorElement>, id: string): void {
  if (!isPlainClick(event.nativeEvent)) return;
  event.currentTarget
    .closest('[role="tree"]')
    ?.querySelector<HTMLElement>(`[data-node-row][data-node-read-id="${CSS.escape(id)}"]`)
    ?.focus();
}
