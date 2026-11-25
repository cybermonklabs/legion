import { type ReactNode, useLayoutEffect, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec, roleDocument } from "../../../api/issue-cache";
import { standsFailed } from "../../refs/reference-target";
import { useAddressTarget } from "../address-target";
import { useOutlineEditing } from "../IssuePresetPanels";
import { issueAddress } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import { NoTooltips } from "../NoTooltips";
import { blocksKey, useOutlineReads } from "../outline-reads";
import { useOutlineRequest } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { type PlanFailedRead, PlanFilterBody, type PlanReads } from "../plan-filter/PlanFilterBody";
import { presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { useTreeReadRunning } from "../tree-read/tree-read";
import { NextViewPage } from "../tree-read/view-read";
import { useTellsAddressDrawn } from "./refused-reads";
import type { ViewSlotProps } from "./stage";

/**
 * The Plan view: the plan's tasks as a chart of time, with one control in the view's first line
 * that draws them as ordered steps instead (`plan-filter/PlanFilterBody.tsx`). The steps are the
 * outline's own rows, so their props are built as `slots/body.tsx` builds the outline's, from the
 * Plan view of the issue's one tree (`presetView`), and a row of them is edited in its
 * document's one editor as a row of the outline is (`useOutlineEditing`). Two things differ from
 * the outline's: the address's node is the view's only where it is a task, and the view is
 * handed the whole plan beside what the words filter left of it, for the order.
 */
export function ViewPlanSlot({
  issue,
  onAddressDrawn,
  route,
}: SlotProps & Partial<Pick<ViewSlotProps, "onAddressDrawn">>): ReactNode {
  const { hash, search } = useLocation();
  const spec = primarySpec(issue);
  const address = issueAddress(route, search, hash, spec?.slug);
  // The tree, the line of each read it is built from that stands refused, and whether every one
  // of those reads has settled (`useOutlineReads`), each under the key and with the freshness the
  // tree reads it by, so they cost no request of their own.
  const { blocksOf, readsSettled, refused: refusedLines, root } = useOutlineReads(issue);
  // The view waits on the reads of the two documents it draws from, and on no other: the plan's
  // blocks, which are its tasks, and the spec's, which are the statements a task serves.
  //
  // Every read the view watches - these two, and the plan's blocks again for the order they are
  // written in (`PlanFilterBody`) - is watched under the key `useIssueTree` fills, by an observer
  // that `useIssueTree` mounts (`useOutlineReads`' `blocksOf`): all three are the one tree read,
  // kept fresh by its own rule (`TREE_FRESH`). So the view sends no request of its own and sees
  // whatever the tree's observer fetches; `refetch` still fetches, which is what Retry asks. A
  // document the issue does not name needs no read.
  //
  // A read has answered where it holds data, whatever its status, and has failed where it stands
  // failed (`refs/reference-target.ts`'s `standsFailed`, the one rule, which says why neither
  // is read from the status). The failed read draws one line, which stays while the read runs
  // again or waits for the network, its Retry busy while it runs (`retrying`), and says each
  // failure again (`failure`). The view draws its tasks, its switch and its empty line once both
  // reads hold data, so it never says the issue has no plan tasks before they are in; until then
  // its tree holds nothing, and the read that failed is one line over the tree, with a Retry that
  // asks that read alone again (`PlanFilterBody`), one line for both documents. The view
  // stays mounted through all of it, as the outline does (`slots/body.tsx`), so no read takes
  // from the reader the press of "Steps", the focused row and its mark, a step being edited, or
  // the address's step having taken the focus, which it takes once.
  const planId = roleDocument(issue, "plan")?.id;
  const planBlocks = blocksOf("plan");
  const specId = spec?.id;
  const specBlocks = blocksOf("spec");
  // Whether the tree read runs, from an observer of its own (`useTreeReadRunning`).
  const running = useTreeReadRunning(issue.key);
  const answered =
    (planId === undefined || planBlocks.data !== undefined) &&
    (specId === undefined || specBlocks.data !== undefined);
  // Both are the one tree read, so its refusal is one line with one Retry: the statements' where
  // the issue names a spec, else the plan's.
  const watched = [
    { id: specId === undefined ? planId : undefined, query: planBlocks, read: "plan" },
    { id: specId, query: specBlocks, read: "statements" },
  ] as const;
  const failed = watched.flatMap(({ id, query, read }): PlanFailedRead[] =>
    id !== undefined && standsFailed(query)
      ? [
          {
            failure: query.errorUpdateCount,
            read,
            retry: query.refetch,
            retrying: running,
          },
        ]
      : []
  );
  // A read is still out where it holds no answer and either runs - its first read, or a refused
  // read running again - or has not failed yet. A refused read that waits for its Retry, or for
  // the network (paused), is not out.
  const out = watched.some(
    ({ id, query }) =>
      id !== undefined && query.data === undefined && (running || !standsFailed(query))
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
  // Whether the view draws a row of the node the address names, which the stage is told by the
  // rule for refused reads (`useTellsAddressDrawn`). The view draws a line of its own for the
  // plan's blocks and the spec's (`watched`). It draws a row for a task alone - a step of the
  // chart or of the steps, or the row of the cycle that holds it - and none for a statement, a
  // comment or any other node, so where the view may tell, it says it draws the node where the
  // address names a task it keeps (`reveal`). It says so before the browser paints, so where the
  // reads were held as the view was drawn, as on Back to its address, no chart is painted in the
  // outline's place. Drawn outside the stage, the slot is handed no call and tells no one.
  const own = watched.flatMap(({ id }) => (id === undefined ? [] : [blocksKey(id)]));
  const tells = useTellsAddressDrawn(refusedLines, readsSettled, own, onAddressDrawn);
  useLayoutEffect(() => {
    if (tells) onAddressDrawn?.(view.root.children.some(({ id }) => named.has(id)));
  }, [named, onAddressDrawn, tells, view.root]);
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
          focusReveal
          // The words filter's Escape comes before the view is left.
          holdsEscape={address.filter !== ""}
          isClosed={isClosed}
          onEditing={onEditing}
          plan={plan}
          planBlocks={planBlocks.data}
          reads={reads}
          {...(request === undefined ? {} : { request })}
          reveal={reveal}
          root={view.root}
          rootRow={view.rootRow}
        />
        {/* While the view read answers a next page, the control that reads it (R15). */}
        <NextViewPage filter="plan" issueKey={issue.key} />
      </div>
      {panels}
    </NoTooltips>
  );
}
