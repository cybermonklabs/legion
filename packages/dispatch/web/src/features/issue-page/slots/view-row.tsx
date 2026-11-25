import { useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useLayoutEffect, useMemo, useRef } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { whoAmIQuery } from "../../../api/queries";
import { pageLineBorder, pagePaperBg } from "../../../theme/classes";
import { useMediaQuery } from "../../shell/useDialog";
import { contentsEntries } from "../contents-entries";
import { CONTENTS_PANE_QUERY, drawsContents, OutlineSheet, PAGE_INSET } from "../contents-frame";
import { IssuePresets } from "../IssuePresets";
import { HEADER_SCROLLS_QUERY } from "../identity-block";
import { issueAddress } from "../issue-address";
import { useOutlineReads } from "../outline-reads";
import { filterSignals, presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { useIssueKeymap } from "../useIssueKeymap";

/**
 * The view row (R2, R11): the view controls and Keys (`IssuePresets.tsx`), each control's signal
 * read off the issue's one tree, and below 1024 px, in Conversation, each filter and Graph (R13),
 * the Outline control beside the one control (`contents-frame.tsx`'s `drawsContents`). It binds
 * the views' chords (`useIssueKeymap.ts`). The signals are drawn only once every read the tree
 * is built from holds its answer (`useOutlineReads`'s `readsHeld`) and the reader's login has
 * answered, which Conversation's count is read for: until then the tree lacks the nodes of a
 * read still out or failed, or Conversation counts no thread as the reader's, and a count read
 * then - a `0`, or a number short of the real one - would read as the real count, so the
 * controls draw none. Under a words filter (`?q=`) they wait on the names too (`namesHeld`): a
 * plan task's or a callout's words come from the names alone, so a count read before they are
 * held finds none of those nodes. With no filter on a count reads no words, and does not wait
 * on the names.
 *
 * The row's rule runs the column's whole width, as the prototype's `#tabs` does, and its
 * controls keep the page's inset. At 760 px and below it stays at the top of the screen as the
 * page scrolls; above, it scrolls away with the header. While it stays it stands over the page
 * and under the navigation's backdrop (`z-20`), so an open navigation dims it as it dims the
 * page, and its height is published (`VIEW_ROW_HEIGHT_PROPERTY`).
 */
export function ViewRowSlot(props: SlotProps): ReactNode {
  const { issue, route } = props;
  const { hash, search } = useLocation();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const { namesHeld, readsHeld, root } = useOutlineReads(issue);
  // Who the reader is, which Conversation's count is read for: a thread is counted where the
  // reader began it.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // What each control draws after its name: a view's count of its main kind and what of it asks
  // something of the reader, and for Conversation how many entries the Contents list holds, as
  // the prototype's signal counts its branches. Each follows `?q=`, and carries "+" while the read
  // it is read from holds nodes no page read yet holds (R15). None while the reads or the
  // reader's login do not hold, nor under `?q=` while the names do not (above).
  const signals = useMemo(
    () =>
      readsHeld && viewer !== undefined && (namesHeld || address.filter === "")
        ? {
            ...filterSignals(root, address.filter),
            conversation: {
              count: contentsEntries(presetView(root, "conversation", address.filter).root, viewer),
              ...(root.moreToRead === true ? { more: true as const } : {}),
            },
          }
        : undefined,
    [address.filter, namesHeld, readsHeld, root, viewer]
  );
  const paned = useMediaQuery(CONTENTS_PANE_QUERY);
  const row = useRef<HTMLDivElement | null>(null);
  useViewRowHeight(row);
  useIssueKeymap(issue.key);
  return (
    <div
      className={`border-b ${PAGE_INSET} ${pageLineBorder} ${pagePaperBg} max-[761px]:sticky max-[761px]:top-0 max-[761px]:z-[19]`}
      ref={row}
    >
      <IssuePresets
        active={address.preset}
        issueKey={issue.key}
        outline={
          paned || !drawsContents(address.preset) ? undefined : <OutlineSheet slot={props} />
        }
        signals={signals}
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
