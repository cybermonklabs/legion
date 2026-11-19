import type { ReactNode } from "react";

import type { IssueDetails } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  pageQuietText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { ClaimChip } from "./ClaimChip";
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
        className={`min-h-11 shrink-0 rounded-lg px-2 py-1 text-sm font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
        disabled={releaseClaim.isPending}
        onClick={() => releaseClaim.mutate()}
        type="button"
        {...pageChrome}
      >
        {releaseClaim.isPending ? ANSWER_LINES.saving_release : CONTROL_LABELS.release_claim}
      </button>
      {releaseClaim.isError ? (
        <QueryError
          message="Could not release this claim."
          onRetry={() => releaseClaim.mutate()}
          retrying={releaseClaim.isPending}
        />
      ) : null}
    </>
  );
}
