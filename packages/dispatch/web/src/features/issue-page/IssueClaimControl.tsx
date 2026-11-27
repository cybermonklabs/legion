import { type ReactNode, useLayoutEffect, useRef, useState } from "react";

import { apiErrorMessage } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { useFocusHandOff } from "../../hooks/useFocusHandOff";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { pageHeaderControl } from "../../theme/classes";
import { pageQuietText } from "../../theme/shell-classes";
import { ClaimChip } from "../issue/ClaimChip";
import { stoppedNote, useRefusalOfSubject } from "../writes/useKeyedWrite";
import { DetailsErrorLine, useRetryHandOff, useSaidFailure } from "./details-error";
import { pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/** What the row says of a release that failed, before the reason Dispatch gave. */
export const RELEASE_FAILED = "Could not release this claim.";

/**
 * Who holds the issue, since when, and the control that hands the claim back, in the claim's
 * row of Details, which carries the label "Claimed by"; `None` while nobody holds it. A release
 * that fails says so beside it, with Dispatch's reason where Dispatch gave one, and a retry; one
 * the reader stopped says it may have been made, as the header says it.
 *
 * No control here is disabled while its release is out, since Chromium moves the focus of a
 * control that turns disabled to the page's body, which would lose a reader who pressed it by
 * keyboard: Release and Retry take no press while it is out (`aria-disabled`), and the failure's
 * line stays until the release Retry sent again is answered (`useSaidFailure`). A focus on a
 * control the reader's own press takes away goes on in the row (`useFocusHandOff`), and one a
 * change the reader did not make takes away stays on the page's body, as on every page of
 * Dispatch, so nothing in the row takes it. Retry hands its focus to Release where the reader's
 * press of Retry took the line away: its release landed while the claim it was pressed on is
 * still drawn. The claim's box hands its focus to the None the row reads then where the release
 * the reader pressed, by Release or by Retry, took the claim away: the claim is read gone while
 * that release is out or once it has landed. A read that finds the claim gone while the reader's
 * release is out - the release's own event, which can arrive before its answer, or a release
 * made elsewhere meanwhile - reached what the press asked for, so the row takes it for the
 * answer. A press whose release failed answers nothing, and nor does one whose claim another
 * session replaced before the claim went: the issue claimed again holds another claim, which
 * that press did not ask to release. So a release made in another session while no release of
 * the reader's is out, or a claim taken again there, hands nothing on; nor does None, which only
 * a claim made in another session takes away.
 *
 * A release's failure is about the claim it was pressed on, and the row says it only while that
 * claim is still the issue's: Dispatch releases whichever claim the issue holds, so once the claim
 * is released elsewhere and the issue claimed again, the line would say the new claim could not
 * be released, and its Retry would release it.
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
  // The issue's claim by its holder and since when: the issue claimed again, by the same holder
  // or another, holds another claim.
  const held =
    issue.claim === null
      ? null
      : JSON.stringify([issue.claim.actor.kind, issue.claim.actor.id, issue.claim.at]);
  // The claim the newest press of Release or Retry was made on; until one, the claim the row is
  // first drawn with.
  const [pressedOn, setPressedOn] = useState(held);
  // A failure of a release pressed on a claim the issue no longer holds is said by no line,
  // whatever the write's state, since `useSaidFailure` keeps the line while its Retry is out. The
  // write forgets such a failure once it stands, so a press on the new claim never draws it.
  const onHeld = pressedOn === held;
  useRefusalOfSubject(releaseClaim, () => onHeld);
  const said = useSaidFailure(releaseClaim);
  const failure = onHeld ? said : null;
  const releaseControl = useRef<HTMLButtonElement>(null);
  const none = useRef<HTMLSpanElement>(null);
  // The reader's press of Release or Retry, noted as it sends the release and forgotten as that
  // release fails, or as the claim's box leaves the page, with the focus or not
  // (`useFocusHandOff`'s `pressed`), so a later loss no press caused moves nothing.
  const released = useRef(false);
  // A claim drawn in place of the one the release was pressed on is another session's: the claim's
  // going after it is no answer of the reader's press.
  useLayoutEffect(() => {
    if (held !== null && held !== pressedOn) released.current = false;
  }, [held, pressedOn]);
  // The reader's press of Retry, which the line notes as it is pressed (`useRetryHandOff`) and
  // forgets as Retry leaves the page, with the focus or not, so a later loss moves nothing. The
  // line goes too when the issue is claimed again in another session, which no Retry did.
  const retry = useRetryHandOff(() => (onHeld ? releaseControl.current : null));
  const claimLeft = useFocusHandOff(() => none.current, released);
  // Release and Retry send the one release, and a press while it is out sends nothing: the guard
  // drops a press made before React has drawn the release out, and `isPending` one made after
  // this row was drawn afresh, with a guard that starts free.
  const release = () => {
    if (releaseClaim.isPending) return;
    setPressedOn(held);
    press.guard(() => {
      released.current = true;
      releaseClaim.mutate(issue.key, {
        onError: () => {
          released.current = false;
        },
        onSettled: () => press.release(),
      });
    });
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
          // A release the reader stopped may have been made, and Retry makes it again, so it is not
          // said as failed; any other is, in the row's words and then any reason Dispatch gave.
          message={
            stoppedNote(failure, "the release of this claim", "made") ??
            releaseFailedLine(apiErrorMessage(failure.error, ""))
          }
          onRetry={release}
          refused={releaseClaim.isPending}
          {...retry}
        />
      )}
    </div>
  );
}

/** The line of a failed release: the row's words, and `reason` after them where there is one. */
function releaseFailedLine(reason: string): string {
  return reason === "" ? RELEASE_FAILED : `${RELEASE_FAILED} ${reason}`;
}
