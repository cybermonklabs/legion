import { createContext, type ReactNode, useContext, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { focusVisibleRing, pageLineBorder, pageQuietText } from "../../../theme/classes";
import { useContentsChosen } from "../contents-chosen";
import { ContentsList } from "../IssueContents";
import { IssueThreeThings } from "../IssueThreeThings";
import { issueAddress } from "../issue-address";
import { NoTooltips } from "../NoTooltips";
import { useIssueNames } from "../one-line";
import { useAskOutline, useFocusBranch } from "../outline-request";
import { pageChrome } from "../page-structure";
import { CONTROL_LABELS } from "../page-words";
import { presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { specSummary } from "../spec-summary";
import { useOlderComments } from "../tree-read/tree-read";
import { useIssueTree } from "../useIssueTree";

/**
 * The contents slot, which the Spec view's work fills: what the page's Contents pane holds from
 * 1024 px up, and its Outline sheet below that (`contents-frame.tsx`), with no view on (R1, R2).
 * R13 adds each filter and Graph: the pane and the sheet stand beside them too, and draw this
 * slot.
 * From the top: the issue's three sentences under "In short" (`IssueThreeThings`), then the
 * Contents list of each question and each thread the reader began (`ContentsList`). Under a view
 * it draws the same list as beside Conversation, and the three sentences in the pane alone. Below
 * 1024 px the Outline sheet leaves them out (`ContentsSentences`), since above its first row each
 * view draws them.
 *
 * The list reads the tree the outline draws (`useIssueTree`), as the Conversation view leaves it
 * with the words filter's `?q=`. An entry chosen sends the outline to its node through the
 * outline's own request (`useAskOutline`), then closes the Outline sheet the list is drawn in
 * (`useContentsChosen`), which does nothing in the pane. Focus branch is the outline's own
 * (`useFocusBranch`), so the pane and the sheet share it.
 *
 * That tree is the outline read the page holds: Conversation's beside Conversation and each
 * filter, Graph's in Graph, so the list adds no read of its own. It holds the entries of the
 * comment pages that read has loaded, and while older comments remain, a control at its foot
 * reads their next page (`OlderEntries`).
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
  const sentences = useContext(ContentsSentences);
  // The older comments of the outline read the list reads (R15): the control at the list's foot
  // reads the same page as the control above the outline's first row, so each page is read once
  // whichever of them is pressed (`useOlderComments`).
  const older = useOlderComments(issue);
  return (
    <NoTooltips>
      {sentences ? (
        <IssueThreeThings look="contents" nameOf={nameOf} specHeld={specHeld} summary={memo} />
      ) : null}
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
      {older.remain ? (
        // A refused page leaves the answer as it was, and the control stays for another press
        // (`readOlderComments`).
        <OlderEntries onPress={() => older.read().catch(() => undefined)} />
      ) : null}
    </NoTooltips>
  );
}

/**
 * Whether the Contents slot draws the issue's three sentences above its list: so where none is
 * given, as in the Contents pane, and not in the Outline sheet, which provides false, since below
 * 1024 px the view draws them above its first row (R13). Declared here and not by the frame that
 * draws the sheet (`contents-frame.tsx`), because that frame draws this slot.
 */
export const ContentsSentences = createContext(true);

/**
 * The control at the foot of the Contents list while the outline read answers older comments than
 * it holds (R15): a press reads the next page of them, and the list draws their entries where the
 * read's order puts them. The press replaces what the control stood for, so it takes no focus:
 * the focus stays where it was, as with the outline's own controls that draw more in place
 * (`TreeRows.tsx`' `PageControl`).
 */
function OlderEntries({ onPress }: { onPress: () => void }): ReactNode {
  return (
    <div className={`shrink-0 border-t px-3.5 py-2 text-xs leading-5 ${pageLineBorder}`}>
      <button
        className={`dispatch-inline-link font-medium underline decoration-page-hair underline-offset-[3px] ${pageQuietText} ${focusVisibleRing}`}
        data-contents-older=""
        onClick={onPress}
        onMouseDown={(event) => event.preventDefault()}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.contents_older}
      </button>
    </div>
  );
}
