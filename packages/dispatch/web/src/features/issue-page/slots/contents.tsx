import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { useContentsChosen } from "../contents-chosen";
import { producingMessages } from "../contents-entries";
import { ContentsList } from "../IssueContents";
import { briefCounts, IssueThreeThings } from "../IssueThreeThings";
import { issueAddress } from "../issue-address";
import { NoTooltips } from "../NoTooltips";
import { useIssueNames } from "../one-line";
import { useOutlineReads } from "../outline-reads";
import { useAskOutline, useFocusBranch } from "../outline-request";
import { isFilter, presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { specSummary } from "../spec-summary";

/**
 * The contents slot, which the Spec view's work fills: what the page's Contents pane holds from
 * 1024 px up, and its Outline sheet below that (`contents-frame.tsx`), with no view on (R1, R2).
 * From the top: the issue's three sentences under "In short" and the two rows under them
 * (`IssueThreeThings`), then the Contents list of each question and each thread the reader began
 * (`ContentsList`). Under a view it draws nothing: the Spec view draws the three sentences as its
 * own first group, and no other view draws them.
 *
 * The list reads the tree the outline draws (`useIssueTree`), as the Conversation view leaves it
 * with the words filter's `?q=`. An entry chosen sends the outline to its node through the
 * outline's own request (`useAskOutline`), then closes the Outline sheet the list is drawn in
 * (`useContentsChosen`), which does nothing in the pane. Focus branch is the outline's own
 * (`useFocusBranch`), so the pane and the sheet share it.
 */
export function ContentsSlot({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const specSlug = primarySpec(issue)?.slug ?? "";
  const address = issueAddress(route, search, hash, specSlug);
  const { namesHeld, readsHeld, root } = useOutlineReads(issue);
  // The sentences are drawn from the summary node's reading (`TreeNode.tree`), which comes with
  // the batched names read, and that read waits until no read of the tree is still out - the
  // plan's blocks, the comments, the messages and the reader's login among them
  // (`useIssueTree.ts`) - so until then "In short" draws its heading and the labels of its two
  // rows, with nothing under them. The rows count over the same tree and the same words filter
  // as the view row does, once it may be counted (`briefCounts`).
  const memo = useMemo(() => specSummary(root, specSlug), [root, specSlug]);
  const counts = useMemo(
    () => briefCounts({ namesHeld, readsHeld, root }, address.filter),
    [address.filter, namesHeld, readsHeld, root]
  );
  const view = useMemo(
    () => presetView(root, "conversation", address.filter, undefined, memo?.id),
    [address.filter, memo, root]
  );
  // Whether a message produced a node is read over the whole tree: the words filter's view drops
  // the node a matching message produced (`isContentsEntry`).
  const producing = useMemo(() => producingMessages(root), [root]);
  // How a reference the sentences and the entries cite is named, read once over the whole tree
  // (`referenceStandIns`).
  const { nameOf } = useIssueNames(issue, root);
  const ask = useAskOutline();
  const [focusBranch, setFocusBranch] = useFocusBranch();
  const chosen = useContentsChosen();
  if (isFilter(address.preset)) return null;
  return (
    <NoTooltips>
      <IssueThreeThings
        counts={counts}
        issueKey={issue.key}
        look="contents"
        nameOf={nameOf}
        summary={memo}
      />
      <ContentsList
        focusBranch={focusBranch}
        nameOf={nameOf}
        onFocusBranch={(current) => {
          setFocusBranch(!focusBranch);
          ask(current, !focusBranch);
        }}
        onGo={(id) => {
          ask(id, focusBranch);
          chosen();
        }}
        producing={producing}
        root={view.root}
      />
    </NoTooltips>
  );
}
