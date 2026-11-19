import type { ReactNode } from "react";

import type { IssueDetails } from "../../api/types";
import { pageHeaderControl, pageQuietText } from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { DetailsErrorLine } from "./details-error";
import { pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * Who holds the issue, since when, and the control that hands the claim back, in the claim's
 * row of Details (dispatch://INFRA-1196 R13), which carries the label "Claimed by"; `None`
 * while nobody holds it. A release that fails says so beside it, with a retry.
 */
export function IssueClaimControl({
  issue,
  writes,
}: {
  issue: IssueDetails;
  writes: Pick<IssueHeaderWrites, "releaseClaim">;
}): ReactNode {
  const { releaseClaim } = writes;
  if (issue.claim === null) {
    return (
      <span className={pageQuietText} {...pageChrome}>
        {CONTROL_LABELS.none}
      </span>
    );
  }
  return (
    <>
      <ClaimChip author claim={issue.claim} inRow liveness={issue} />
      <button
        aria-label={`Release the claim on ${issue.key}`}
        className={`${pageHeaderControl} disabled:cursor-not-allowed disabled:opacity-50`}
        disabled={releaseClaim.isPending}
        onClick={() => releaseClaim.mutate()}
        type="button"
        {...pageChrome}
      >
        {releaseClaim.isPending ? ANSWER_LINES.saving_release : CONTROL_LABELS.release_claim}
      </button>
      {releaseClaim.isError ? (
        <DetailsErrorLine
          message="Could not release this claim."
          onRetry={() => releaseClaim.mutate()}
          retrying={releaseClaim.isPending}
        />
      ) : null}
    </>
  );
}
