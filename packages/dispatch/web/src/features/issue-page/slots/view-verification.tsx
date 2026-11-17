import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { artifactBlocksQuery, issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type { CheckBlock } from "../../../api/types";
import { refetchFailedOnFocus } from "../../refs/reference-target";
import { useAddressTarget } from "../address-target";
import { issueAddress } from "../issue-address";
import { NoTooltips } from "../NoTooltips";
import { presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { useIssueTree } from "../useIssueTree";
import {
  type FailedRead,
  VerificationFilterBody,
  type VerificationRead,
  type VerificationReads,
} from "../verification-filter/VerificationFilterBody";

/** Filled by INFRA-1196 part V, "The Verification view". */
export function ViewVerificationSlot({ issue, route }: SlotProps): ReactNode {
  // The Verification view: the issue's one tree as the Verification view leaves it, each statement
  // with the checks that serve it and each check beside its newest result.
  const { hash, search } = useLocation();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const { root } = useIssueTree(issue);
  const { checks, reads } = useVerificationReads(issue.key);
  // The address's own node is resolved against the whole tree and then kept by the view, so an
  // address that names a node the view holds no row for still draws it, as the outline resolves
  // it (`useAddressTarget`).
  const { named } = useAddressTarget(root, address);
  const view = useMemo(
    () => presetView(root, "verification", address.filter, named),
    [address.filter, named, root]
  );
  // The view with no words query, where a query is typed: what the issue holds whatever the query
  // left of it. A query is typed where it is not empty once trimmed, as `presetView` reads it, so
  // a query of spaces alone draws as no query does; with none typed the body reads the view
  // itself, and nothing is built a second time.
  const typed = address.filter.trim() !== "";
  const whole = useMemo(
    () => (typed ? presetView(root, "verification", "", named).root : undefined),
    [named, root, typed]
  );
  return (
    <NoTooltips>
      {/* The stage's column, which the body draws its own measure in. */}
      <div className="flex w-full min-w-0 flex-col">
        <VerificationFilterBody
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
  reads: VerificationReads;
} {
  // The coverage read, and the issue's checks with the results recorded against them, under the
  // keys `useIssueTree.ts` already reads them with, so the view sends no request of its own. The
  // results read holds a result's commit, its verdict and its withdrawal, which the tree does
  // not carry (`verification-filter/statement-state.ts`). Staleness is per observer: with the
  // app's 30 s (`main.tsx`), pressing the view more than 30 s after the page read them read both
  // again. Each holds what it read until an event refreshes its key (`staleTime: Infinity`), and
  // the window's focus asks again only a read in error (`refetchFailedOnFocus`), as the page's
  // reference reads do. These are this slot's observers alone: `useIssueTree`'s own observers of
  // the same keys still ask again after the app's 30 s until they are given the same options, so
  // in the page a view pressed more than 30 s after the page read them can still read them
  // again, through those observers.
  const coverage = useQuery({
    ...issueCoverageQuery(issueKey),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: refetchFailedOnFocus,
  });
  const results = useQuery({
    ...issueResultsQuery(issueKey),
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: refetchFailedOnFocus,
  });
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
  // and the reader's presses with it. A read has failed where it holds no data and was refused,
  // while it is in error and while it runs again after that (`failedWithNothing`): it was
  // refused before it first answered. The view draws its rows only once all three have answered,
  // so it never says that an issue has no checks before they are in; where a read failed it says
  // which, each with a Retry that asks that read alone again, rather than draw an empty view that
  // gives the reader nothing to act on.
  const answered =
    coverage.data !== undefined &&
    results.data !== undefined &&
    (documentId === undefined || blocks.data !== undefined);
  // Each read that failed with no answer held, by its name, with the press that asks that read
  // alone again and whether it runs again; the body says them in an order of its own.
  const failed: Partial<Record<VerificationRead, FailedRead>> = {};
  if (failedWithNothing(coverage)) failed.coverage = failedRead(coverage);
  if (failedWithNothing(results)) failed.results = failedRead(results);
  if (documentId !== undefined && failedWithNothing(blocks)) failed.blocks = failedRead(blocks);
  const reads: VerificationReads =
    Object.keys(failed).length > 0
      ? { failed, state: "failed" }
      : answered
        ? { state: "answered" }
        : { state: "waiting" };
  return { checks, reads };
}

/**
 * Whether `read` failed with no answer held: a read asked again that fails keeps the answer it
 * had, which the view still draws from, so only a read that left nothing to draw says it failed,
 * as the Spec view reads its own.
 *
 * It says so while it runs again too, on a Retry or with no press (the window's focus, or an
 * event that names its key), until it answers. TanStack Query puts a read that holds no answer
 * back to `pending` as it runs again, so read by its error alone the line, and the Retry the
 * reader's focus is on, would leave the page for as long as the read runs and come back if it is
 * refused again; the line stays instead, and leaves only when the read answers. The read keeps
 * its count of refusals (`errorUpdateCount`) as it runs again, which tells it from a read on its
 * first load, out with nothing refused yet, which the view draws as waiting.
 */
function failedWithNothing(read: {
  readonly data: unknown;
  readonly errorUpdateCount: number;
  readonly isError: boolean;
  readonly isFetching: boolean;
}): boolean {
  return (
    read.data === undefined && (read.isError || (read.isFetching && read.errorUpdateCount > 0))
  );
}

/** What the line of a read that failed is given: the press that asks that read alone again, and
 *  whether it runs again (`isFetching`), while which the line's Retry asks nothing more. */
function failedRead(read: {
  readonly isFetching: boolean;
  readonly refetch: () => Promise<unknown>;
}): FailedRead {
  return { retry: () => void read.refetch(), retrying: read.isFetching };
}
