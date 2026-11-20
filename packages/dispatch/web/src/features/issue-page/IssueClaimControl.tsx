import { type ReactNode, useRef } from "react";

import type { IssueDetails } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { pageHeaderControl, pageQuietText } from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { DetailsErrorLine, useFocusHandOff, useSaidFailure } from "./details-error";
import { pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * Who holds the issue, since when, and the control that hands the claim back, in the claim's
 * row of Details, which carries the label "Claimed by"; `None` while nobody holds it. A release
 * that fails says so beside it, with a retry.
 *
 * No control here is disabled while its release is out, since Chromium moves the focus of a
 * control that turns disabled to the page's body, which would lose a reader who pressed it by
 * keyboard: Release and Retry take no press while it is out (`aria-disabled`), and the failure's
 * line stays until the release Retry sent again is answered (`useSaidFailure`). A focus on a
 * control that goes as the release lands goes on in the row (`useFocusHandOff`): from Retry to
 * Release while the claim is still drawn, and from any of them, once it is not, to the None the
 * row reads then, the one place left in the row and the answer to the reader's press.
 */
export function IssueClaimControl({
  issue,
  writes,
}: {
  issue: IssueDetails;
  writes: Pick<IssueHeaderWrites, "releaseClaim">;
}): ReactNode {
  const { releaseClaim } = writes;
  const press = useSubmitGuard();
  const failure = useSaidFailure(releaseClaim);
  const releaseControl = useRef<HTMLButtonElement>(null);
  const none = useRef<HTMLSpanElement>(null);
  const retryLeft = useFocusHandOff(() => releaseControl.current);
  const claimLeft = useFocusHandOff(() => none.current);
  // Release and Retry send the one release, and a press while it is out sends nothing: the guard
  // drops a press made before React has drawn the release out, and `isPending` one made after
  // this row was drawn again - Details' section closed and opened - with a guard that starts free.
  const release = () => {
    if (releaseClaim.isPending) return;
    press.guard(() => releaseClaim.mutate(undefined, { onSettled: () => press.release() }));
  };
  if (issue.claim === null) {
    return (
      <span className={`outline-none ${pageQuietText}`} ref={none} tabIndex={-1} {...pageChrome}>
        {CONTROL_LABELS.none}
      </span>
    );
  }
  return (
    <div className="contents" ref={claimLeft}>
      <ClaimChip author claim={issue.claim} inRow liveness={issue} />
      <button
        aria-disabled={releaseClaim.isPending || undefined}
        aria-label={`Release the claim on ${issue.key}`}
        className={`${pageHeaderControl} refused:cursor-not-allowed refused:opacity-50`}
        onClick={release}
        ref={releaseControl}
        type="button"
        {...pageChrome}
      >
        {releaseClaim.isPending ? ANSWER_LINES.saving_release : CONTROL_LABELS.release_claim}
      </button>
      {failure === null ? null : (
        <DetailsErrorLine
          failure={failure.count}
          message="Could not release this claim."
          onRetry={release}
          refused={releaseClaim.isPending}
          retryRef={retryLeft}
        />
      )}
    </div>
  );
}
