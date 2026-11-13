import { MAX_ISSUE_BLOCKERS } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";

import { ApiError, apiErrorMessage, issuePatchRequest } from "../../api/client";
import { landIssueWrite } from "../../api/issue-cache";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import type { BlockedByRefusalWords } from "./page-words";

/** A refusal of a blocked-by list the page has a sentence for (`BLOCKED_BY_REFUSALS`), with the
 *  issue it is about where the refusal names one. */
export interface BlockedByRefusal {
  readonly key?: string;
  readonly words: BlockedByRefusalWords;
}

export interface BlockedByWrite {
  /** Why the last save failed, until the next save starts or the control forgets it
   *  (`forgetRefusal`): one of the page's sentences for a refusal it knows, else what Dispatch
   *  said, or the page's line for a save that never reached it. */
  readonly error: BlockedByRefusal | string | undefined;
  /** Drops that refusal, so an editor opened again never draws the one before it. */
  readonly forgetRefusal: () => void;
  readonly pending: boolean;
  /** Saves the issues this one waits on. `onSaved` runs on a landing and on nothing else, so
   *  the editor stays open over a refusal with the reader's list still in it. */
  readonly save: (keys: readonly string[]) => void;
}

/**
 * Which of the page's sentences a refusal of the list `sent` is, by its `code`
 * (`packages/envoy/internal/dispatch/api/issue_parent.go`): `BLOCKED_BY_OUTSIDE_PROJECT` names
 * an issue of another project; `BLOCKED_BY_INPUT` an issue Dispatch has none of, the issue
 * itself, or - naming none - a list over `MAX_ISSUE_BLOCKERS`; and `DEPENDENCY_CYCLE` the issue
 * the list would make wait on itself, or - naming none - a chain longer than Dispatch follows.
 * The key is the one of this issue's and `sent` the server's message names, so the sentence
 * carries the server's detail as a value and never its words. Undefined for every other answer,
 * which the editor says as it always has.
 */
export function blockedByRefusal(
  error: unknown,
  issueKey: string,
  sent: readonly string[]
): BlockedByRefusal | undefined {
  if (!(error instanceof ApiError)) return undefined;
  const named = error.message.split(/[^A-Z0-9-]+/u);
  const key = [issueKey, ...sent].find((one) => named.includes(one));
  if (error.code === "DEPENDENCY_CYCLE") {
    return key === undefined ? { words: "too_deep" } : { key, words: "cycle" };
  }
  if (error.code === "BLOCKED_BY_OUTSIDE_PROJECT" && key !== undefined) {
    return { key, words: "outside_project" };
  }
  if (error.code !== "BLOCKED_BY_INPUT") return undefined;
  if (key !== undefined) return { key, words: key === issueKey ? "itself" : "not_found" };
  return sent.length > MAX_ISSUE_BLOCKERS ? { words: "too_many" } : undefined;
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
      ? (blockedByRefusal(write.error, issueKey, write.variables ?? []) ??
        apiErrorMessage(write.error, `Could not save what ${issueKey} waits on.`))
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
