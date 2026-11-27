import { type ReactNode, useRef } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { useFootInset } from "../../../hooks/useFootInset";
import { type BoxPlace, IssueComposer, useComposerShown } from "../IssueComposer";
import { IssueReadMark } from "../IssueReadMark";
import { issueAddress } from "../issue-address";
import { useOutlineReads } from "../outline-reads";
import type { SlotProps } from "../slot-props";

/**
 * The foot of the new issue page: the end of the outline, which reads the issue once the
 * reader has seen it (`IssueReadMark`, R9), and the box to write in, held at the foot of the
 * screen (`IssueComposer`, R1). The column the root draws the stage and this slot in is at least
 * the screen's height (`IssueOutlinePage.tsx`), so the box's wrapper stands at the screen's foot
 * however short the outline is, and stays there while a long one scrolls under it.
 *
 * The wrapper is the outline's measure, at most 1,000 px in the middle of the column, and 10 px
 * from each side of the screen at 760 px and below, where the column's own inset is 14 px. It
 * draws no padding: the 8 px under the box and the fade above it are the box's own section
 * (`pageComposer.foot`), so nothing of the box shows while it is hidden. Its layer is over the
 * outline's rows and under the view row and Details. It is an element of the foot's page layer
 * (`useFootInset`), so the shell's floats - the update banner, the status corner - keep clear
 * of it while it has a box on the screen, riding above it.
 *
 * The mark and the wrapper are the column's last items, at `order: 1`, so the spacer that
 * `styles.css` draws at `order: 0` in a column holding end room lands above both. While a float
 * lies over the outline, the column takes that room for it above the box, not under it
 * (`FOOT_LAYERS`, "Why the room goes above an element sticky to the document's foot"): the box
 * stays at the foot, the floats above it stay where they are, and at the end of the scroll the
 * outline's last row ends 1rem above the highest of them.
 *
 * The mark stands in the flow after the stage and before the box, 12 px under the outline's last
 * row (the tree's own `mb-3`, `IssueTree.tsx`) and under that room while the column holds it, and
 * is moved down by the box's height less those 12 px (`--issue-composer-height`,
 * `FOOT_HEIGHT_PROPERTY` of `identity-block.ts`), which moves its box and not the layout. So it
 * stands where the last row's bottom edge would stand if it were the box's height and the room
 * lower: the dwell reads where it is moved to, so it reaches the screen exactly once the last row
 * is clear of the box, and 1rem clear of the floats above it while they show (`End` leaves that
 * row 8 px above the box), and on an outline shorter than the screen at once. It is drawn while
 * the address names Conversation and no document, the one place the conversation's outline ends:
 * a view, Dashboard among them (dispatch://INFRA-5702/spec#b-r-new-page), draws no outline.
 */
export function FootSlot({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const place: BoxPlace = {
    documentOpen: address.document !== undefined,
    viewOn: address.preset !== "conversation",
  };
  // The issue's one tree, read as the outline reads it (`useOutlineReads`, as the view row does),
  // so it costs no request: the box reads off it which entries and statements a draft it keeps
  // can go back to. Until every read the tree is built from holds its answer and its names are
  // held, the tree may lack a row that is there, so the box is handed none and draws no draft as
  // gone meanwhile.
  const { namesHeld, readsHeld, root } = useOutlineReads(issue);
  const shown = useComposerShown(place);
  const wrapper = useRef<HTMLDivElement>(null);
  useFootInset(wrapper, shown, "page");
  return (
    <>
      {place.viewOn || place.documentOpen ? null : (
        <div className="relative order-1 top-[calc(var(--issue-composer-height,0px)-0.75rem)]">
          <IssueReadMark issueKey={issue.key} />
        </div>
      )}
      <div
        className="sticky bottom-0 z-[15] order-1 mx-auto mt-auto w-full max-w-[1000px] min-w-0 max-[761px]:-mx-1 max-[761px]:w-auto"
        ref={wrapper}
      >
        <IssueComposer
          documentOpen={place.documentOpen}
          isClosed={issue.closed_at !== null}
          issueKey={issue.key}
          tree={readsHeld && namesHeld ? root : undefined}
          viewOn={place.viewOn}
        />
      </div>
    </>
  );
}
