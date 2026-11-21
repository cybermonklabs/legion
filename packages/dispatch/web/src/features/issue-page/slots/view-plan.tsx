import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec, roleDocument } from "../../../api/issue-cache";
import { artifactBlocksQuery } from "../../../api/queries";
import { useAddressTarget } from "../address-target";
import { useOutlineEditing } from "../IssuePresetPanels";
import { issueAddress } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import { NoTooltips } from "../NoTooltips";
import { useOutlineRequest } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { type PlanFailedRead, PlanFilterBody, type PlanReads } from "../plan-filter/PlanFilterBody";
import { presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { standsFailed, useIssueTree } from "../useIssueTree";

/**
 * The Plan view: the plan's tasks as a chart of time, with one control in the view's first line
 * that draws them as ordered steps instead (`plan-filter/PlanFilterBody.tsx`). The steps are the
 * outline's own rows, so their props are built as `slots/body.tsx` builds the outline's, from the
 * Plan view of the issue's one tree (`presetView`), and a row of them is edited in its
 * document's one editor as a row of the outline is (`useOutlineEditing`). Two things differ from
 * the outline's: the address's node is the view's only where it is a task, and the view is
 * handed the whole plan beside what the words filter left of it, for the order.
 */
export function ViewPlanSlot({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const spec = primarySpec(issue);
  const address = issueAddress(route, search, hash, spec?.slug);
  const { root } = useIssueTree(issue);
  // The view waits on the reads of the two documents it draws from, and on no other: the plan's
  // blocks, which are its tasks, and the spec's, which are the statements a task serves. It is
  // these two reads and not `useIssueTree`'s `blocksHeld`, which counts the Verification and
  // decisions documents too: one of those made while the reader is on Plan opens a read that
  // holds nothing yet, and one whose first read is refused holds nothing at all, and neither
  // changes anything in Plan; and a failed line needs the failed read's own error and Retry.
  //
  // Every read the view watches - these two, and the plan's blocks again for the order they are
  // written in (`PlanFilterBody`) - is watched under the key `useIssueTree` fills, by an observer
  // that is never enabled: staleness is an observer's own, so an enabled one mounted past the
  // app's stale time would fetch again. So the view sends no request of its own and sees
  // whatever the tree's observer fetches; `refetch` still fetches, which is what Retry asks. A
  // document the issue does not name needs no read.
  //
  // A read has answered where it holds data, whatever its status, and has failed where it stands
  // failed (`useIssueTree.ts`'s `standsFailed`, the outline's own rule, which says why neither
  // is read from the status). Each failed read draws one line, which stays while the read runs
  // again or waits for the network, its Retry busy while it runs (`retrying`), and says each
  // failure again (`failure`). The view draws its tasks, its switch and its empty line once both
  // reads hold data, so it never says the issue has no plan tasks before they are in; until then
  // its tree holds nothing, and each read that failed is one line over the tree, in the order
  // plan, statements, with a Retry that asks that read alone again (`PlanFilterBody`). The view
  // stays mounted through all of it, as the outline does (`slots/body.tsx`), so no read takes
  // from the reader the press of "Steps", the focused row and its mark, a step being edited, or
  // the address's step having taken the focus, which it takes once.
  const planId = roleDocument(issue, "plan")?.id;
  const planBlocks = useQuery({ ...artifactBlocksQuery(planId ?? ""), enabled: false });
  const specId = spec?.id;
  const specBlocks = useQuery({ ...artifactBlocksQuery(specId ?? ""), enabled: false });
  const answered =
    (planId === undefined || planBlocks.data !== undefined) &&
    (specId === undefined || specBlocks.data !== undefined);
  const watched = [
    { id: planId, query: planBlocks, read: "plan" },
    { id: specId, query: specBlocks, read: "statements" },
  ] as const;
  const failed = watched.flatMap(({ id, query, read }): PlanFailedRead[] =>
    id !== undefined && standsFailed(query)
      ? [
          {
            failure: query.errorUpdateCount,
            read,
            retry: query.refetch,
            retrying: query.isFetching,
          },
        ]
      : []
  );
  // A read is still out where it holds no answer and either runs - its first read, or a refused
  // read running again - or has not failed yet. A refused read that waits for its Retry, or for
  // the network (paused), is not out.
  const out = watched.some(
    ({ id, query }) =>
      id !== undefined && query.data === undefined && (query.isFetching || !standsFailed(query))
  );
  const reads: PlanReads =
    failed.length > 0
      ? { failed, out, state: "failed" }
      : answered
        ? { state: "answered" }
        : { state: "waiting" };
  // The address's own node, resolved against the whole tree as the outline resolves it
  // (`useAddressTarget`, `slots/body.tsx`): a step it names takes the focus.
  const found = useAddressTarget(root, address).reveal;
  // Plan draws no node but a task, so the address's node is kept in the view, and given the
  // focus, only where it is a task. `presetView` keeps a named node of any kind (`presets.ts`'s
  // `filterView`), as `AddressedNode.named` names it: handed every node, `/plan?comment=<id>`
  // would draw the comment as a bar and `/plan#b-<statement>` the statement as a bar with its
  // tasks under it, out of the chart. Handed the node only where it is a task, both draw every
  // task and no other node, as `/plan` does.
  const reveal = useMemo(() => (found?.at(-1)?.type === "plan_task" ? found : undefined), [found]);
  const named = useMemo(() => {
    const node = reveal?.at(-1);
    return new Set(node === undefined ? [] : [node.id]);
  }, [reveal]);
  // The whole plan's tasks, with no words filter on: the order and each bar's start are worked
  // out from every task of the plan, and the words filter chooses which of its places the view
  // draws. With no words filter on it is the view's own.
  const whole = useMemo(() => presetView(root, "plan", "", named), [named, root]);
  const view = useMemo(
    () => (address.filter === "" ? whole : presetView(root, "plan", address.filter, named)),
    [address.filter, named, root, whole]
  );
  const plan = whole.root.children;
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  const request = useOutlineRequest();
  // The document a step is being edited in, and that document's one editor, which the view draws
  // for the row to take onto its own stage: `IssuePresetPanels.tsx`'s `useOutlineEditing`, as the
  // outline's body reads it (`slots/body.tsx`).
  const { onEditing, panels } = useOutlineEditing(issue);
  const isClosed = issue.closed_at !== null;
  return (
    <NoTooltips>
      {/* The view's box, the prototype's `.plan`: at most 1300 px, in the middle of the column,
          and at 760 px and below the column less 10 px at each side. That is `max-[761px]:`,
          since Tailwind writes `max-[N]:` as `(width < N)` and 760 px itself is a phone. */}
      <div className="mx-auto flex w-full max-w-[1300px] min-w-0 flex-col max-[761px]:max-w-[calc(100%-20px)]">
        <PlanFilterBody
          artifacts={artifacts}
          context={view.context}
          emptyLine={address.filter === "" ? EMPTY_STATE_LINES.plan : EMPTY_STATE_LINES.no_match}
          filtering={view.filtering}
          focusReveal
          // The words filter's Escape comes before the view is left.
          holdsEscape={address.filter !== ""}
          isClosed={isClosed}
          onEditing={onEditing}
          plan={plan}
          reads={reads}
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
