import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { artifactBlocksQuery, issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type { CheckBlock } from "../../../api/types";
import { EVENT_FRESH, standsFailed } from "../../refs/reference-target";
import { useAddressTarget } from "../address-target";
import { issueAddress } from "../issue-address";
import { NoTooltips } from "../NoTooltips";
import { blocksKey, useOutlineReads } from "../outline-reads";
import { presetView } from "../presets";
import {
  type FailedRead,
  queryTyped,
  VerificationFilterBody,
  type VerificationRead,
  type VerificationReads,
} from "../verification-filter/VerificationFilterBody";
import { useTellsAddressDrawn } from "./refused-reads";
import type { ViewSlotProps } from "./stage";

/** Filled by INFRA-1196 part V, "The Verification view". */
export function ViewVerificationSlot({ issue, onAddressDrawn, route }: ViewSlotProps): ReactNode {
  // The Verification view: the issue's one tree as the Verification view leaves it, each statement
  // with the checks that serve it and each check beside its newest result.
  const { hash, search } = useLocation();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  // The tree, the line of each read it is built from that stands refused, and whether every one
  // of those reads has settled (`useOutlineReads`), each under the key and with the freshness the
  // tree reads it by, so they cost no request of their own.
  const { failed, readsSettled, root } = useOutlineReads(issue);
  const { checks, documentId, reads } = useVerificationReads(issue.key);
  // The address's own node is resolved against the whole tree and then kept by the view, so an
  // address that names a node the view holds no row for still draws it, as the outline resolves
  // it (`useAddressTarget`).
  const { named } = useAddressTarget(root, address);
  const view = useMemo(
    () => presetView(root, "verification", address.filter, named),
    [address.filter, named, root]
  );
  // The view with no words query, where a query is typed (`queryTyped`, the body's one rule): what
  // the issue holds whatever the query left of it. A query of spaces alone draws as no query does;
  // with none typed the body reads the view itself, and nothing is built a second time.
  const typed = queryTyped(address.filter);
  const whole = useMemo(
    () => (typed ? presetView(root, "verification", "", named).root : undefined),
    [named, root, typed]
  );
  // Whether the view draws a row of the node the address names, which the stage is told by the
  // rule for refused reads (`useTellsAddressDrawn`). The view draws a line of its own for the
  // coverage read, the results read and, where the results read holds a check, its document's
  // blocks (`useVerificationReads`'s `documentId`). Where the view may tell, the body says whether
  // it draws the node, with the reader's chip applied (`addressed`, one value while the node and
  // the call stay the same): a statement, a check under a statement it draws, a check that serves
  // none or a row of its own, but under a chip only that state's statements and their checks, and
  // under a statement no node that is no check, such as a plan task that serves it. It tells it
  // before the browser paints, so where the reads were held as the view was drawn, as on Back to
  // its address, the table is never painted in the outline's place.
  const tells = useTellsAddressDrawn(
    failed,
    readsSettled,
    ["coverage", "results", ...(documentId === undefined ? [] : [blocksKey(documentId)])],
    onAddressDrawn
  );
  const addressed = useMemo(() => ({ named, tell: onAddressDrawn }), [named, onAddressDrawn]);
  return (
    <NoTooltips>
      {/* The stage's column, which the body draws its own measure in. */}
      <div className="flex w-full min-w-0 flex-col">
        <VerificationFilterBody
          {...(tells ? { addressed } : {})}
          checks={checks}
          context={view.context}
          reads={reads}
          view={view.root}
          whole={whole}
        />
      </div>
    </NoTooltips>
  );
}

/**
 * The reads the Verification view is drawn from, as its body takes them (the `checks` and
 * `reads` of `VerificationFilterBody`): the coverage read, the results read with the issue's
 * checks, and the blocks read of the Verification document, and how the three stand. They are
 * the slot's own observers, apart from `useIssueTree`'s of the same keys, so what they fetch is
 * read by their own options alone.
 */
export function useVerificationReads(issueKey: string): {
  checks: readonly CheckBlock[];
  /** The document whose blocks are read: none where the results read holds no check. */
  documentId: string | undefined;
  reads: VerificationReads;
} {
  // The coverage read, and the issue's checks with the results recorded against them, under the
  // keys `useIssueTree.ts` already reads them with, so the view sends no request of its own. The
  // results read holds a result's commit, its verdict and its withdrawal, which the tree does
  // not carry (`verification-filter/statement-state.ts`). Both take the tree's `EVENT_FRESH`
  // (held until an event refreshes their key; the window's focus asks again only a read in
  // error), as `useIssueTree`'s own observers of the same keys do, so no observer reads them
  // again by age and the view's reads cannot drift from the tree's.
  const coverage = useQuery({ ...issueCoverageQuery(issueKey), ...EVENT_FRESH });
  const results = useQuery({ ...issueResultsQuery(issueKey), ...EVENT_FRESH });
  const checks = useMemo(() => results.data?.checks ?? [], [results.data]);
  // The blocks read of the Verification document too: a check learns the statements it serves
  // from its block's `serves` pointer, which only that read carries (`tree-drafts.ts`). Drawn
  // before it answers, every check would stand at the table's foot as one that serves no
  // statement, every statement would read Unknown, and the line that counts a statement's checks
  // elsewhere would call the issue's own checks "elsewhere". The results read answers the checks
  // of one document, the issue's Verification document (`api/results.go`'s
  // `verificationDocument`), and names it in a field of its own (`verification_artifact_id`),
  // which is read where the read holds a check. It must agree with the document whose blocks
  // `useIssueTree` reads, which that hook finds from the issue read (`roleDocument`); no other
  // document's blocks place a check. The slot watches that read under the same key with an
  // observer that is never enabled, so it sends no request of its own and sees whatever
  // `useIssueTree`'s observer fetches; its `refetch` still fetches, which is what Retry asks.
  // Where the results read holds no check no blocks read is needed, and the observer of no
  // document never fetches. It is this one read and not `useIssueTree`'s `blocksHeld`, which
  // counts all four of the tree's documents: a refused plan or decisions read must not hold this
  // view empty, and the failed line needs this read's own error and Retry.
  const documentId = checks.length === 0 ? undefined : results.data?.verification_artifact_id;
  const blocks = useQuery({ ...artifactBlocksQuery(documentId ?? ""), enabled: false });
  // Each read has answered where it holds data, and not by its status: a background refetch
  // that fails sets TanStack Query's status to error and keeps the data it last answered, and
  // the query cache keeps that data when the stage mounts this slot again as the reader comes
  // back to the view. A view drawn by `isSuccess` would empty itself on one failed refetch, and
  // draw nothing on its return for as long as the refetch kept failing, and take its tab stop
  // and the reader's presses with it. A read has failed where it holds no data and a fetch of it
  // was refused, whether it is in error, runs again, or waits to run again while the browser is
  // offline (`standsFailed`, the page's one rule, which the outline uses for the same reads): it
  // was refused before it first answered. The view draws its rows only once all three have
  // answered, so it never says that an issue has no checks before they are in; where a read
  // failed it says which, each with a Retry that asks that read alone again, rather than draw an
  // empty view that gives the reader nothing to act on.
  const answered =
    coverage.data !== undefined &&
    results.data !== undefined &&
    (documentId === undefined || blocks.data !== undefined);
  // Each read that failed with no answer held, by its name, with the press that asks that read
  // alone again and whether it runs again; the body says them in an order of its own.
  const failed: Partial<Record<VerificationRead, FailedRead>> = {};
  if (standsFailed(coverage)) failed.coverage = failedRead(coverage);
  if (standsFailed(results)) failed.results = failedRead(results);
  if (documentId !== undefined && standsFailed(blocks)) failed.blocks = failedRead(blocks);
  const reads: VerificationReads =
    Object.keys(failed).length > 0
      ? { failed, state: "failed" }
      : answered
        ? { state: "answered" }
        : { state: "waiting" };
  return { checks, documentId, reads };
}

/** What the line of a read that failed is given: which refusal it says, the read's own count of
 *  its refusals (`errorUpdateCount`), as the outline gives it (`outline-reads.ts`); the press
 *  that asks that read alone again; and whether it runs again (`isFetching`), while which the
 *  line's Retry asks nothing more. */
function failedRead(read: {
  readonly errorUpdateCount: number;
  readonly isFetching: boolean;
  readonly refetch: () => Promise<unknown>;
}): FailedRead {
  return {
    failure: read.errorUpdateCount,
    retry: () => void read.refetch(),
    retrying: read.isFetching,
  };
}
