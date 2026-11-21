import { type ReactNode, useRef } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { FOOT_HELD_ATTRIBUTE, useFootInset } from "../../../hooks/useFootInset";
import { type BoxPlace, IssueComposer, useComposerShown } from "../IssueComposer";
import { IssueReadMark } from "../IssueReadMark";
import { issueAddress } from "../issue-address";
import { isFilter } from "../presets";
import type { SlotProps } from "../slot-props";

/**
 * The foot of the new issue page: the end of the outline, which reads the issue once the
 * reader has seen it (`IssueReadMark`, R9), and the one place to write, held at the foot of the
 * screen (`IssueComposer`, R1). The column the root draws the stage and this slot in is at least
 * the screen's height (`IssueOutlinePage.tsx`), so the box's wrapper stands at the screen's foot
 * however short the outline is, and stays there while a long one scrolls under it.
 *
 * The wrapper is the outline's measure, at most 1,000 px in the middle of the column, and 10 px
 * from each side of the screen at 760 px and below, where the column's own inset is 14 px. It
 * draws no padding: the 8 px under the box and the fade above it are the box's own section
 * (`pageComposer.foot`), so nothing of the box shows while it is hidden. Its layer is over the
 * outline's rows and under the view row and Details. It says it is held at the foot
 * (`FOOT_HELD_ATTRIBUTE`), so the reach it publishes while it stands there lifts the shell's
 * floating status above it (`useFootInset`).
 *
 * The mark stands in the flow after the stage and before the box, 12 px under the outline's last
 * row (the tree's own `mb-3`, `IssueTree.tsx`), and is moved down by the box's height less those
 * 12 px (`--issue-composer-height`, `FOOT_HEIGHT_PROPERTY` of `identity-block.ts`), which moves
 * its box and not the layout. So it stands where the last row's bottom edge would stand if it
 * were the box's height lower: the dwell reads where it is moved to, so it reaches the screen
 * exactly once the last row is clear of the box (`End` leaves that row 8 px above the box), and on
 * an outline shorter than the screen at once. It is drawn while the address names no view and no
 * document, whose end is not the conversation's.
 */
export function FootSlot({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const place: BoxPlace = {
    documentOpen: address.document !== undefined,
    viewOn: isFilter(address.preset),
  };
  const shown = useComposerShown(place);
  const wrapper = useRef<HTMLDivElement>(null);
  useFootInset(wrapper, shown);
  return (
    <>
      {place.viewOn || place.documentOpen ? null : (
        <div className="relative top-[calc(var(--issue-composer-height,0px)-0.75rem)]">
          <IssueReadMark issueKey={issue.key} />
        </div>
      )}
      <div
        className="sticky bottom-0 z-[15] mx-auto mt-auto w-full max-w-[1000px] min-w-0 max-[761px]:-mx-1 max-[761px]:w-auto"
        ref={wrapper}
        {...{ [FOOT_HELD_ATTRIBUTE]: "" }}
      >
        <IssueComposer
          documentOpen={place.documentOpen}
          isClosed={issue.closed_at !== null}
          issueKey={issue.key}
          viewOn={place.viewOn}
        />
      </div>
    </>
  );
}
