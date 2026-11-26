import { type ReactNode, type RefObject, useLayoutEffect, useMemo, useRef } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { pageLineBorder, pagePaperBg } from "../../../theme/classes";
import { useMediaQuery } from "../../shell/useDialog";
import { CONTENTS_PANE_QUERY, OutlineSheet, PAGE_INSET } from "../contents-frame";
import { useOpenedDocument } from "../document-address";
import { IssuePresets } from "../IssuePresets";
import { HEADER_SCROLLS_QUERY } from "../identity-block";
import { issueAddress } from "../issue-address";
import { useOutlineReads } from "../outline-reads";
import { filterSignals, isFilter } from "../presets";
import type { SlotProps } from "../slot-props";
import { useIssueKeymap } from "../useIssueKeymap";
import { useWhoIsReading } from "../useIssueTree";
import { useViewRowTools } from "../view-row-tools";

/**
 * The view row (R2, R11): Dashboard and Conversation with no count, the four filter controls
 * with signals from the issue's tree, Documents with no count, and Keys (`IssuePresets.tsx`).
 * Below 1024 px, with no view on, the Outline control stands beside the one control
 * (`contents-frame.tsx`). The row binds the views' chords (`useIssueKeymap.ts`).
 *
 * Signals wait for the tree's reads (`readsHeld`): a read still out or failed leaves nodes
 * missing from the counts. They also wait for the reader's login: `tree-drafts.ts` marks no
 * decision `waiting` until the login answers, so the Decisions flag would omit decisions that
 * need review. Under a words filter (`?q=`) signals wait for the names (`namesHeld`), since
 * the names supply the words of plan tasks and callouts. Without a words filter the counts
 * do not depend on the names.
 *
 * The row's rule runs the column's whole width, as the prototype's `#tabs` does, and its
 * controls keep the page's inset. At 760 px and below it stays at the top of the screen as the
 * page scrolls; above, it scrolls away with the header. While it stays it stands over the page
 * and under the navigation's backdrop (`z-20`), so an open navigation dims it as it dims the
 * page, and its height is published (`VIEW_ROW_HEIGHT_PROPERTY`).
 *
 * Part N adds the row's two small controls, the version control and the filter by words, before
 * Keys (`view-row-tools.tsx`), and below 1024 px the Outline sheet that holds them, with a view on
 * too, in place of D's Outline control. Documents draws no outline and so no sheet (root R14),
 * and neither do Dashboard (dispatch://INFRA-5702/spec#b-r-new-page) and a document drawn as the
 * stage.
 */
export function ViewRowSlot(props: SlotProps): ReactNode {
  const { issue, route } = props;
  const { hash, search } = useLocation();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const { namesHeld, readsHeld, root } = useOutlineReads(issue);
  // Decisions' flag counts the decisions marked `waiting` for the reader.
  const viewer = useWhoIsReading().data?.login;
  // The four filter signals follow `?q=` and stay hidden until their reads are ready.
  const signals = useMemo(
    () =>
      readsHeld && viewer !== undefined && (namesHeld || address.filter === "")
        ? filterSignals(root, address.filter)
        : undefined,
    [address.filter, namesHeld, readsHeld, root, viewer]
  );
  const paned = useMediaQuery(CONTENTS_PANE_QUERY);
  const row = useRef<HTMLDivElement | null>(null);
  useViewRowHeight(row);
  useIssueKeymap(issue.key);
  const tools = useViewRowTools(props);
  const opened = useOpenedDocument(issue, route);
  // Below 1024 px, where root R14 opens the Contents list as the Outline sheet, the sheet holds
  // the row's two small controls, with a view on too (part N's R4). Documents draws no outline,
  // so no sheet: its version control stands at the row's end. Nor does a document drawn as the
  // stage, whichever way it was opened: no Contents list is drawn while it is (the plan's preset
  // table), and its small controls stand at the row's end too.
  const on = tools.active ?? address.preset;
  const drawsOutline = !paned && opened === undefined && (on === "conversation" || isFilter(on));
  return (
    <div
      className={`border-b ${PAGE_INSET} ${pageLineBorder} ${pagePaperBg} max-[761px]:sticky max-[761px]:top-0 max-[761px]:z-[19]`}
      ref={row}
    >
      <IssuePresets
        active={address.preset}
        issueKey={issue.key}
        sheet={
          drawsOutline ? (inSheet) => <OutlineSheet slot={props} tools={inSheet} /> : undefined
        }
        signals={signals}
        // Part N's: its two small controls, and Documents as the control that is on while a
        // document its list opened is drawn (`view-row-tools.tsx`).
        {...tools}
      />
    </div>
  );
}

/**
 * How tall the view row is while it stays at the top of the screen, at 760 px and below, and
 * `0px` while it scrolls away with the header: a custom property on the document element beside
 * `--issue-identity-height` (`identity-block.ts`). `styles.css` sets the new page's
 * `scroll-padding-top` from the two, so what a scroll brings into view (`scrollIntoView`) stands
 * below everything that stays, and gives the row's own controls a top scroll margin of minus this
 * height, so a focus that enters the row scrolls no page (decision 113 of the parts). Unset while
 * no view row is drawn.
 */
const VIEW_ROW_HEIGHT_PROPERTY = "--issue-view-row-height";

/**
 * Publishes `VIEW_ROW_HEIGHT_PROPERTY` for `ref`, the view row's own box, as `useIdentityBlock`
 * publishes the band's height: in a layout effect, so the first frame already carries it, then
 * again whenever the row's size changes and whenever the width crosses `HEADER_SCROLLS_QUERY`,
 * where the row starts or stops staying.
 */
function useViewRowHeight(ref: RefObject<HTMLElement | null>): void {
  useLayoutEffect(() => {
    const row = ref.current;
    if (row === null) return;
    const narrow = window.matchMedia(HEADER_SCROLLS_QUERY);
    const publish = () => {
      const stays = getComputedStyle(row).position === "sticky";
      document.documentElement.style.setProperty(
        VIEW_ROW_HEIGHT_PROPERTY,
        `${stays ? Math.round(row.getBoundingClientRect().height) : 0}px`
      );
    };
    const sizes = new ResizeObserver(publish);
    sizes.observe(row);
    narrow.addEventListener("change", publish);
    publish();
    return () => {
      sizes.disconnect();
      narrow.removeEventListener("change", publish);
      document.documentElement.style.removeProperty(VIEW_ROW_HEIGHT_PROPERTY);
    };
  }, [ref]);
}
