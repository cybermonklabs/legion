import { useQueryClient } from "@tanstack/react-query";

import { apiErrorMessage, issuePatchRequest } from "../../api/client";
import { landIssueWrite } from "../../api/issue-cache";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { useKeyedWrite } from "../writes/useKeyedWrite";

export interface BlockedByWrite {
  /** Why Dispatch refused the last save, in its own words, until the next save starts or the
   *  control forgets it (`forgetRefusal`). */
  readonly error: string | undefined;
  /** Drops that refusal, so an editor opened again never draws the one before it. */
  readonly forgetRefusal: () => void;
  readonly pending: boolean;
  /** Saves the issues this one waits on. `onSaved` runs on a landing and on nothing else, so
   *  the editor stays open over a refusal with the reader's list still in it. */
  readonly save: (keys: readonly string[]) => void;
}

/**
 * The one way the panel writes the issues an issue waits on (`blocked_by`).
 *
 * A refused list is refused for a reason the reader can mend - "CORE-9 not found", "CORE-1
 * cannot be blocked by itself", a blocker outside the project - so the write says what Dispatch
 * refused and leaves the editor open with the typed list in it; there is no Retry, because
 * sending the same refused list again would be refused again
 * (`packages/dispatch/AGENTS.md`: a field's refusal belongs at its control).
 */
export function useIssueBlockedBy(issueKey: string, onSaved: () => void): BlockedByWrite {
  const queryClient = useQueryClient();
  const guard = useSubmitGuard();
  // One write of the issue's line (`issue:<key>`), as every other field write is, so an older
  // one still being tried never lands after a newer one.
  const write = useKeyedWrite({
    inline: true,
    label: (keys: readonly string[]) =>
      keys.length === 0 ? `${issueKey} waits on nothing` : `${issueKey} waits on ${keys.join(" ")}`,
    target: () => `issue:${issueKey}`,
    request: (keys: readonly string[]) => issuePatchRequest(issueKey, { blocked_by: [...keys] }),
    onSettled: () => guard.release(),
    onSuccess: (next) => {
      // The one landing every write of an issue's fields takes, the identity block's included.
      landIssueWrite(queryClient, issueKey, next);
      onSaved();
    },
  });
  return {
    error: write.isError
      ? apiErrorMessage(write.error, `Could not save what ${issueKey} waits on.`)
      : undefined,
    forgetRefusal: () => {
      if (write.isError) write.reset();
    },
    pending: write.isPending,
    save: (keys) => {
      guard.guard(() => write.mutate(keys));
    },
  };
}
