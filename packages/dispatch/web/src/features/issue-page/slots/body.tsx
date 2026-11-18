import { useQueries } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { api } from "../../../api/client";
import { primarySpec } from "../../../api/issue-cache";
import { issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import { refetchFailedOnFocus } from "../../refs/reference-target";
import { useAddressTarget } from "../address-target";
import { useOutlineEditing } from "../IssuePresetPanels";
import { IssueTree } from "../IssueTree";
import { issueAddress } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import { NoTooltips } from "../NoTooltips";
import { useOutlineRequest } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { specSummary } from "../spec-summary";
import { TreeReadFailed } from "../tree-read/view-read";
import { useIssueTree } from "../useIssueTree";

/** What each read the outline waits on takes: a copy of the tree's own rule for its reads,
 *  `useIssueTree.ts`'s `EVENT_FRESH`, which this file is to import once that file exports it.
 *  Events keep a read fresh and its age never does, so the slot mounted again on a press of a
 *  view more than 30 s after the load reads none of them again, and window focus fetches again
 *  only a read in error. */
const EVENT_FRESH = {
  refetchOnWindowFocus: refetchFailedOnFocus,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** Filled by INFRA-1196 part G, "A node's row", then part I, "The outline". */
export function BodySlot({ issue, route }: SlotProps): ReactNode {
  // The outline: the issue's one tree as the Conversation view leaves it, folded to the
  // frontier, in place of the rows drawn with every branch open before it. The spec's summary
  // callout is no row of it: the Spec view draws it as the issue's three sentences.
  const { hash, search } = useLocation();
  const specSlug = primarySpec(issue)?.slug;
  const address = issueAddress(route, search, hash, specSlug);
  const { blocksHeld, root } = useIssueTree(issue);
  // Whether every read the tree's nodes come from holds its answer: the issue's children come
  // with the issue, the documents' blocks are `blocksHeld`, and every other node comes from one
  // of these, read under the keys `useIssueTree` reads them by, so they cost no request of their
  // own. Until then a tree with no node is one whose nodes have not arrived, and the outline's
  // line that the issue holds nothing would be false on every cold load, so the outline draws
  // nothing in its place; so would `q` and `w` saying there is no question or decision to go to,
  // so they say nothing. A read that failed holds nothing either: the page cannot say what it
  // would have held.
  const key = issue.key;
  const nodesHeld = useQueries({
    combine: (answers) => answers.every((answer) => answer.data !== undefined),
    queries: [
      { ...issueCoverageQuery(key), ...EVENT_FRESH },
      { ...issueResultsQuery(key), ...EVENT_FRESH },
      { queryFn: () => api.listIssueAsks(key, "all"), queryKey: ["asks", key], ...EVENT_FRESH },
      { queryFn: () => api.listComments(key), queryKey: ["comments", key], ...EVENT_FRESH },
      {
        queryFn: () => api.listIssueMessages(key),
        queryKey: ["issue-messages", key],
        ...EVENT_FRESH,
      },
    ],
  });
  const readsHeld = nodesHeld && blocksHeld;
  // The address's own node is resolved against the whole tree and then kept by the view, so an
  // address that names a node the words filter would not have matched still draws it.
  const { named, reveal } = useAddressTarget(root, address);
  const memo = useMemo(() => specSummary(root, specSlug ?? "")?.id, [root, specSlug]);
  const view = useMemo(
    () => presetView(root, "conversation", address.filter, named, memo),
    [address.filter, memo, named, root]
  );
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  const request = useOutlineRequest();
  // The document a row of the outline is being edited in, and that document's one editor, which
  // the body draws for the row to take onto its own stage (`useOutlineEditing`).
  const { onEditing, panels } = useOutlineEditing(issue);
  const isClosed = issue.closed_at !== null;
  return (
    <NoTooltips>
      {/* The outline's measure, the prototype's `#pane`: at most 1000 px, in the middle of the
          column. On a phone 4 px from the left edge and 10 px from the right, as the
          prototype's `#reading` is there, so the bullets keep the picture's place. */}
      <div className="mx-auto flex w-full max-w-[1000px] min-w-0 flex-col max-[761px]:-mr-1 max-[761px]:-ml-2.5 max-[761px]:w-auto">
        {/* A tree read refused before it first answered leaves the tree the issue alone, which
            draws no row, so the outline says so over the tree, with a Retry. */}
        <TreeReadFailed issue={issue} />
        <IssueTree
          artifacts={artifacts}
          context={view.context}
          emptyLine={
            address.filter === "" ? EMPTY_STATE_LINES.conversation : EMPTY_STATE_LINES.no_match
          }
          filtering={view.filtering}
          focusReveal
          // The words filter's Escape comes before the outline is left.
          holdsEscape={address.filter !== ""}
          isClosed={isClosed}
          onEditing={onEditing}
          readsHeld={readsHeld}
          {...(request === undefined ? {} : { request })}
          reveal={reveal}
          root={view.root}
          rootRow={view.rootRow}
        />
      </div>
      {panels}
    </NoTooltips>
  );
}
