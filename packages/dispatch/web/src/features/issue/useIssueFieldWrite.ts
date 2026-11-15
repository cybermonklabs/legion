import { type QueryKey, useQueryClient } from "@tanstack/react-query";
import { useRef } from "react";

import { issuePatchRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { inboxQuery } from "../../api/queries";
import type { InboxRow, IssueDetails, IssueSummary, UpdateIssueInput } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";

export interface IssueFieldWrite<Value> {
  /** The server's reason after a refused save, or what a save the reader stopped may have done
   *  and what Retry does with it (`stopped`), until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the failed save; a no-op while one is in flight. */
  retry: () => void;
  /** What a save the reader stopped may have done and what Retry does with it, until the next
   *  attempt; undefined after any other end. */
  stopped: string | undefined;
  /** Saves `value`. A pick made while a save is in flight shows at once and is saved when that
   *  save settles; the newest such pick of an issue is the one saved for it. */
  submit: (value: Value) => void;
}

/** The caches a save shows its pick in, as they stood before the pick was shown: the detail of
 *  the issue the save is of (`issueKey`), and every project issue list and inbox query, which
 *  hold every issue. */
interface Snapshot {
  details: IssueDetails | undefined;
  inbox: [QueryKey, InboxRow[] | undefined][];
  issueKey: string;
  lists: [QueryKey, IssueSummary[] | undefined][];
}

export const projectListsKey = ["issues", "project"] as const;
export const inboxKey = inboxQuery().queryKey;

/** One save of a field: the issue it is of and the value it sets. `retry()` and `again()` run the
 *  newest render's `request` with the press's own values (`useKeyedWrite`), so every byte a Retry
 *  sends rides in the press: the save carries its issue, and its request, its Retry and what the
 *  page shows of it are of that issue, whichever issue the hook is rendered for by then. */
interface FieldSave<Value> {
  readonly issueKey: string;
  readonly value: Value;
}

/** A pick made during a save, waiting its turn, with the caches as they stood before it was
 *  shown: the base its save rolls back to, so its failure never lands on the pick that failed. */
interface QueuedSave<Value> {
  save: FieldSave<Value>;
  snapshot: Snapshot;
}

/**
 * The optimistic write engine behind every single-field issue write (priority, assignee). The
 * caller supplies `patch`, the request body for a value, and `show`, which lands the value in
 * the issue detail, every project issue list and every inbox row that shows the issue. The engine
 * snapshots those caches before showing a pick, rolls them back on failure, merges the server's
 * answer on success, and refetches once the save settles. Requests go one at a time, so the
 * server sees picks in the order they were made: a pick during a save is queued behind it - the
 * newest pick of an issue in place of one of that issue still waiting, a pick of another issue
 * in a place of its own - and the in-flight answer is not merged over a newer pick of its issue
 * the reader can already see.
 */
export function useIssueFieldWrite<Value>(
  issueKey: string,
  field: {
    patch: (value: Value) => UpdateIssueInput;
    /** Lands `value` in every cache that shows `issueKey`, the issue the save is of. */
    show: (value: Value, issueKey: string) => void;
    /** What the shell's indicator names one save of this field of `issueKey`. */
    label: (value: Value, issueKey: string) => string;
    /** What the field of `issueKey` is in the sentence a save the reader stopped says
     *  (`stoppedWriteNote`): `the priority of KEY`. */
    what: (issueKey: string) => string;
  }
): IssueFieldWrite<Value> {
  const queryClient = useQueryClient();
  const guard = useSubmitGuard();
  // The picks made during a save, oldest first, one per issue.
  const queued = useRef<QueuedSave<Value>[]>([]);
  const chainedSnapshot = useRef<Snapshot | null>(null);
  const takeSnapshot = (key: string): Snapshot => ({
    details: queryClient.getQueryData<IssueDetails>(["issue", key]),
    inbox: queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey }),
    issueKey: key,
    lists: queryClient.getQueriesData<IssueSummary[]>({ queryKey: projectListsKey }),
  });
  const waitingFor = (key: string): QueuedSave<Value> | undefined =>
    queued.current.find((waiting) => waiting.save.issueKey === key);
  // The control says why its save was refused and offers Retry where the reader just picked, so
  // the shell's indicator shows the save only while it is out. Every write to the issue's fields
  // goes in the issue's line (`issue:<key>`), so one still being tried after its surface is gone
  // never lands after a newer one.
  const mutation = useKeyedWrite({
    inline: true,
    label: (save: FieldSave<Value>) => field.label(save.value, save.issueKey),
    target: (save) => `issue:${save.issueKey}`,
    request: (save: FieldSave<Value>) => issuePatchRequest(save.issueKey, field.patch(save.value)),
    onMutate: async (save): Promise<Snapshot> => {
      await Promise.all(
        [["issue", save.issueKey], projectListsKey, inboxKey].map((queryKey) =>
          queryClient.cancelQueries({ queryKey })
        )
      );
      const snapshot = chainedSnapshot.current ?? takeSnapshot(save.issueKey);
      chainedSnapshot.current = null;
      field.show(save.value, save.issueKey);
      return snapshot;
    },
    onError: (_error, _save, snapshot) => {
      if (snapshot === undefined) {
        return;
      }
      if (snapshot.details !== undefined) {
        queryClient.setQueryData<IssueDetails>(["issue", snapshot.issueKey], snapshot.details);
      }
      for (const [key, data] of snapshot.lists) {
        queryClient.setQueryData(key, data);
      }
      for (const [key, data] of snapshot.inbox) {
        queryClient.setQueryData(key, data);
      }
      // The lists and the inbox are back at this save's base for every issue, and the detail for
      // this save's issue alone. A waiting pick's base is now as much of this one as that reached:
      // all of it for a pick of this issue, the lists and the inbox for a pick of another.
      for (const waiting of queued.current) {
        waiting.snapshot =
          waiting.save.issueKey === snapshot.issueKey
            ? snapshot
            : { ...waiting.snapshot, inbox: snapshot.inbox, lists: snapshot.lists };
      }
    },
    onSuccess: (next, save) => {
      // Saved with no answer to give: `onSettled` reads the issue again.
      if (next !== null && waitingFor(save.issueKey) === undefined) {
        mergeIssue(queryClient, next);
      }
    },
    onSettled: (_next, _error, save) => {
      guard.release();
      // Another write to the same issue (a status change from the header) may have settled in
      // between; refetching the detail makes it server-authoritative rather than trusting the
      // arrival order of the two responses or a pre-write snapshot. Not while a pick of the
      // issue still waits, which is what the reader sees until its own save settles.
      if (waitingFor(save.issueKey) === undefined) {
        void queryClient.invalidateQueries({ queryKey: ["issue", save.issueKey] });
      }
      const [next, ...rest] = queued.current;
      if (next !== undefined) {
        queued.current = rest;
        chainedSnapshot.current = next.snapshot;
        guard.guard(() => mutation.mutate(next.save));
        return;
      }
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  // A save the reader stopped may have been made, and Retry makes that very press again
  // (`retryLast`). The error's own words are a composer's, of a message, so the control says
  // this sentence instead, which names the issue that press was of.
  const stopped = stoppedNote(
    mutation,
    field.what(mutation.variables?.issueKey ?? issueKey),
    "saved"
  );
  return {
    error: stopped ?? mutation.error?.message,
    failed: mutation.isError,
    pending: mutation.isPending,
    retry: () => {
      guard.retryLast(mutation);
    },
    stopped,
    submit: (value) => {
      const save = { issueKey, value };
      if (guard.guard(() => mutation.mutate(save))) {
        return;
      }
      // The pick waits behind the save in flight: in place of a pick of its issue already
      // waiting, on that pick's base, or else in a place of its own, on its own issue's base.
      const waiting = waitingFor(issueKey);
      if (waiting === undefined) {
        queued.current = [...queued.current, { save, snapshot: takeSnapshot(issueKey) }];
      } else {
        waiting.save = save;
      }
      field.show(value, issueKey);
    },
  };
}
