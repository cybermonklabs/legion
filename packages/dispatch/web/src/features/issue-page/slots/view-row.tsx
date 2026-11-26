import { useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useLayoutEffect, useMemo, useRef } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { TREE_FILTERS, whoAmIQuery } from "../../../api/queries";
import { pageLineBorder, pagePaperBg } from "../../../theme/classes";
import { useMediaQuery } from "../../shell/useDialog";
import { CONTENTS_PANE_QUERY, drawsContents, OutlineSheet, PAGE_INSET } from "../contents-frame";
import { useDecisionsTree } from "../decisions-filter/decisions-entries";
import { IssuePresets } from "../IssuePresets";
import { HEADER_SCROLLS_QUERY } from "../identity-block";
import { issueAddress } from "../issue-address";
import type { TreeNode } from "../issue-tree";
import { useOutlineReads } from "../outline-reads";
import type { Preset } from "../page-words";
import { type Filter, filterSignals, type PresetSignal } from "../presets";
import type { SlotProps } from "../slot-props";
import { useIssueKeymap } from "../useIssueKeymap";

/**
 * The view row (R2, R11): Conversation with no count, the four filter controls with signals
 * from the issue's tree, Graph with no count, and Keys (`IssuePresets.tsx`). Below 1024 px, in
 * Conversation, each filter and Graph (R13), the Outline control stands beside the one control
 * (`contents-frame.tsx`'s `drawsContents`). The row binds the views' chords
 * (`useIssueKeymap.ts`).
 *
 * Signals wait for the tree's reads (`readsHeld`): a read still out or failed leaves nodes
 * missing from the counts. They also wait for the reader's login: the outline marks no
 * decision `waiting` until the login answers (`tree-read/tree-nodes.ts`' `draftDecision`), so
 * the Decisions flag would omit decisions that need review. Under a words filter (`?q=`) signals
 * wait for the names (`namesHeld`), since the names supply the words of plan tasks and callouts.
 * Without a words filter the counts do not depend on the names. A filter loaded cold with no
 * outline read draws its own count from its view answer alone (`viewRowSignals`).
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
  // Decisions' flag counts the decisions marked `waiting` for the reader.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // The tree the Decisions view is drawn over, which its count is read off: the one its own
  // view read builds once the page holds that read, and the outline's until then.
  const decisionsRoot = useDecisionsTree(issue, root, viewer);
  // What each control draws after its name (`viewRowSignals`).
  const signals = useMemo(
    () =>
      viewRowSignals({
        decisionsRoot,
        filter: address.filter,
        namesHeld,
        preset: address.preset,
        readsHeld,
        root,
        viewer,
      }),
    [address.filter, address.preset, decisionsRoot, namesHeld, readsHeld, root, viewer]
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
 * What each control of the view row draws after its name: a filter's count of its main kind and
 * what of it asks something of the reader, and nothing for Conversation or Graph. Each follows
 * the words filter (`filter`, `?q=`), and carries "+" while the read it is read from holds nodes
 * no page read yet holds (R15). None while the reads (`readsHeld`) or the reader's login
 * (`viewer`) do not hold, nor under a words filter while the names do not (`namesHeld`).
 * Decisions is counted off the tree its view is drawn over (`decisionsRoot`, `presets.ts`'
 * `filterSignals`): the one its own view read builds once the page holds that read, and `root`
 * until then, as where none is given.
 *
 * A filter loaded cold with no outline read - Spec below 1024 px, which sends none until the
 * Outline sheet opens (R13) - never holds the outline's reads, and its count is its own view
 * answer's nodes of its main kind (R4): once that answer is held (`TreeViews`), its control
 * draws that count alone, and every other control none.
 */
export function viewRowSignals({
  decisionsRoot,
  filter,
  namesHeld,
  preset,
  readsHeld,
  root,
  viewer,
}: {
  decisionsRoot?: TreeNode;
  filter: string;
  namesHeld: boolean;
  preset: Preset;
  readsHeld: boolean;
  root: TreeNode;
  viewer: string | undefined;
}): Readonly<Record<Filter, PresetSignal | undefined>> | undefined {
  if (!namesHeld && filter !== "") return undefined;
  if (readsHeld && viewer !== undefined) return filterSignals(root, filter, decisionsRoot);
  const cold = TREE_FILTERS.find((one) => one === preset);
  if (cold === undefined || root.views?.[cold] === undefined) return undefined;
  return {
    decisions: undefined,
    plan: undefined,
    spec: undefined,
    verification: undefined,
    [cold]: filterSignals(root, filter)[cold],
  };
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
