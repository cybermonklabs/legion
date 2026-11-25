import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { useContentsChosen } from "../contents-chosen";
import { ContentsList } from "../IssueContents";
import { IssueThreeThings } from "../IssueThreeThings";
import { issueAddress } from "../issue-address";
import { NoTooltips } from "../NoTooltips";
import { useIssueNames } from "../one-line";
import { useAskOutline, useFocusBranch } from "../outline-request";
import { isFilter, presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { specSummary } from "../spec-summary";
import { useIssueTree } from "../useIssueTree";

/**
 * The contents slot, which the Spec view's work fills: what the page's Contents pane holds from
 * 1024 px up, and its Outline sheet below that (`contents-frame.tsx`), with no view on (R1, R2).
 * From the top: the issue's three sentences under "In short" (`IssueThreeThings`), then the
 * Contents list of each question and each thread the reader began (`ContentsList`). Under a view
 * it draws nothing: the Spec view draws the three sentences as its own first group, and no other
 * view draws them.
 *
 * The list reads the tree the outline draws (`useIssueTree`), as the Conversation view leaves it
 * with the words filter's `?q=`. An entry chosen sends the outline to its node through the
 * outline's own request (`useAskOutline`), then closes the Outline sheet the list is drawn in
 * (`useContentsChosen`), which does nothing in the pane. Focus branch is the outline's own
 * (`useFocusBranch`), so the pane and the sheet share it.
 */
export function ContentsSlot({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const spec = primarySpec(issue);
  const specSlug = spec?.slug ?? "";
  const address = issueAddress(route, search, hash, specSlug);
  const { blocksOf, root } = useIssueTree(issue);
  // The head says the spec holds no summary only once the spec's blocks are held, which is when
  // the tree holds its summary node where it has one. The sentences are drawn from that node's
  // reading (`TreeNode.tree`), which the tree read carries with the spec's blocks (`blocksOf`).
  const specBlocks = blocksOf("spec");
  const specHeld = spec !== undefined && specBlocks.data !== undefined;
  const memo = useMemo(() => specSummary(root, specSlug), [root, specSlug]);
  const view = useMemo(
    () => presetView(root, "conversation", address.filter, undefined, memo?.id),
    [address.filter, memo, root]
  );
  // How a reference the sentences and the entries cite is named, read once over the whole tree
  // (`referenceStandIns`).
  const { nameOf } = useIssueNames(issue, root);
  const ask = useAskOutline();
  const [focusBranch, setFocusBranch] = useFocusBranch();
  const chosen = useContentsChosen();
  if (isFilter(address.preset)) return null;
  return (
    <NoTooltips>
      <IssueThreeThings look="contents" nameOf={nameOf} specHeld={specHeld} summary={memo} />
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
        root={view.root}
      />
    </NoTooltips>
  );
}
