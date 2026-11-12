import { type QueryKey, useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";

import { issuePatchRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { inboxQuery, projectListsKey } from "../../api/queries";
import type { InboxRow, IssueDetails, IssueSummary, UpdateIssueInput } from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { retriesUnderItsKey, WriteDiscardedError } from "../writes/keyed-write";
import { type Shape, shape } from "../writes/write-storage";
import {
  defineWriteKind,
  ofKind,
  useWriteSelection,
  useWrites,
  type Write,
} from "../writes/write-store";

export interface IssueFieldWrite<Value> {
  /** The server's reason after a refused save, until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  /** True while any write of this field of this issue stands in the store unsettled, whichever
   *  surface pressed it and whether or not the page that pressed it is still mounted, and while
   *  a press of this control's own is settled there but not reported here yet. */
  pending: boolean;
  /** Repeats the refused save, under its own key where Dispatch never decided it. Undefined
   *  where a newer write of the field was left in the line when the refusal came: a Retry joins
   *  at the back of that line and would store the older pick last, so the control reports the
   *  refusal alone. */
  retry: (() => void) | undefined;
  /** Saves `value`: one write of the page's store, on the issue's line, made as it is pressed.
   *  A pick that replaces one of this control's that has not gone out takes its place, so two
   *  picks nobody has sent are one request, the newest. */
  submit: (value: Value) => void;
}

interface Snapshot {
  details: IssueDetails | undefined;
  inbox: [QueryKey, InboxRow[] | undefined][];
  lists: [QueryKey, IssueSummary[] | undefined][];
}

/** A refused save as the control reports it: the server's reason, the pick to make again, and
 *  whether a Retry is offered at all. */
interface Refused<Value> {
  readonly message: string;
  readonly retriable: boolean;
  readonly value: Value;
}

/** One field write as the store holds it: the issue and the field it writes. The engine reads
 *  its own field's writes back out of `Writes.rows()` by this record (`ofKind`), so what it
 *  knows of them outlives the surface that pressed them and a reload. */
interface IssueFieldDetail {
  readonly field: string;
  readonly issue: string;
}

const keptIssueField = shape.record({
  field: shape.string,
  issue: shape.string,
}) satisfies Shape<IssueFieldDetail>;

/**
 * One save of one field of one issue (priority, assignee, repository), ordered on the issue's
 * line (`issue:<KEY>`) with every other write of that issue and made again from its request
 * after a reload, so a pick the person saw is sent even though the page that took it is gone.
 */
export const issueFieldWrite = defineWriteKind<IssueFieldDetail>({
  kept: keptIssueField,
  name: "issue-field",
});

export const inboxKey = inboxQuery().queryKey;

/** Whether `row` is a write of `issue`'s `field` that may still reach the server: waiting its
 *  turn, out, or late. A refused one never is - the engine shows its own refusals (`inline`), so
 *  the store lets it go the moment Dispatch refuses it. */
function stillInLine(row: Write, issue: string, field: string): boolean {
  const write = ofKind(row, issueFieldWrite);
  return (
    write !== undefined &&
    write.detail.issue === issue &&
    write.detail.field === field &&
    (write.status === "queued" || write.status === "sending" || write.status === "late")
  );
}

/**
 * The optimistic write engine behind every single-field issue write (priority, assignee,
 * repository). The caller supplies `patch`, the request body for a value, and `show`, which lands
 * the value in the issue detail, every project issue list and every inbox row that shows the
 * issue.
 *
 * Every pick joins the store's line when it is pressed: one write of its own on the issue's line,
 * kept with the request it will send, so picks reach the server in press order whatever becomes
 * of the control - another page, an unmount, a reload - and none the person saw is lost. The
 * engine snapshots the caches before showing a pick and restores that snapshot if the server
 * refuses it, merges the server's answer and reads the issue again once the save settles - each
 * of those only while no newer write of the field is left in the line, where the screen is
 * already the newer pick's. A pick that replaces one of this control's that has not gone out
 * takes it out of the line (`Writes.discard`), which refuses that press with
 * `WriteDiscardedError`: the engine made that write go, so the person sees no failure for it.
 */
export function useIssueFieldWrite<Value>(
  issueKey: string,
  field: {
    /** The field's name in the store, which tells this engine's writes from those of another
     *  field of the same issue. */
    name: string;
    patch: (value: Value) => UpdateIssueInput;
    show: (value: Value) => void;
    /** What the shell's indicator names one save of this field. */
    label: (value: Value) => string;
  }
): IssueFieldWrite<Value> {
  const queryClient = useQueryClient();
  const writes = useWrites();
  const detailKey = ["issue", issueKey] as const;
  const [refused, setRefused] = useState<Refused<Value> | undefined>(undefined);
  /** The write this control pressed last, while a pick of its own may still replace it. Another
   *  surface's pick, and one this page pressed before a reload, stand in the line on their own:
   *  nothing of theirs is this control's to take out. */
  const mine = useRef<number | undefined>(undefined);
  /** The last press the reader stopped, which a press of the same value makes again under its
   *  key, so Dispatch answers it with what it did rather than taking the write twice. */
  const stopped = useRef<{ readonly key: string; readonly value: Value } | undefined>(undefined);
  /** What a later press of this control rolls back to once an earlier one was refused: the
   *  caches as they stood before that refused press showed its value, since the server never
   *  took it. Without it a second refusal would put the first refused pick on screen and call it
   *  the value the server last confirmed. Cleared the moment a press settles with nothing newer
   *  of the field left in the line, where the chain is over. */
  const refusedBase = useRef<Snapshot | undefined>(undefined);
  /** How many presses of this control the store has settled but the engine has not reported yet.
   *  A write the store lets go - Dispatch refused it, and the engine shows that refusal itself
   *  (`inline`) - leaves `rows()` a commit before this engine says what became of it, and a
   *  surface that keeps its control while its write is live (the Inbox's `Assign to me`) would
   *  drop the control in that gap. */
  const [inHand, setInHand] = useState(0);
  const inLine = useWriteSelection(`issue-field:${issueKey}:${field.name}`, (rows) =>
    rows.some((row) => stillInLine(row, issueKey, field.name))
  );
  const takeSnapshot = (): Snapshot => ({
    details: queryClient.getQueryData<IssueDetails>(detailKey),
    inbox: queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey }),
    lists: queryClient.getQueriesData<IssueSummary[]>({ queryKey: projectListsKey }),
  });
  const restore = (snapshot: Snapshot): void => {
    if (snapshot.details !== undefined) {
      queryClient.setQueryData<IssueDetails>(detailKey, snapshot.details);
    }
    for (const [key, data] of snapshot.lists) {
      queryClient.setQueryData(key, data);
    }
    for (const [key, data] of snapshot.inbox) {
      queryClient.setQueryData(key, data);
    }
  };
  // Another write to the same issue (a status change from the header) may have settled in
  // between; reading the issue again makes it server-authoritative rather than trusting the
  // arrival order of the two responses or a pre-write snapshot.
  const reread = (): void => {
    void queryClient.invalidateQueries({ queryKey: detailKey });
    void queryClient.invalidateQueries({ queryKey: ["issues"] });
    void queryClient.invalidateQueries({ queryKey: inboxKey });
  };
  const submit = (value: Value): void => {
    // Two picks of this control that have not gone out are one request, the newest: the one
    // waiting leaves the line and its press is refused `WriteDiscardedError`, which the engine
    // swallows. A write already out, or one another surface pressed, is nobody's to take out.
    if (mine.current !== undefined) {
      writes.discard(mine.current);
    }
    setRefused(undefined);
    setInHand((count) => count + 1);
    for (const queryKey of [detailKey, projectListsKey, inboxKey]) {
      // A read in flight would otherwise answer with the server's older value over the pick.
      void queryClient.cancelQueries({ queryKey });
    }
    const snapshot = takeSnapshot();
    field.show(value);
    const again = stopped.current;
    stopped.current = undefined;
    // A value here is a priority, a login or a repository, so the same value is the same press
    // made again, under the key Dispatch may already have taken that press under.
    const key = again !== undefined && Object.is(again.value, value) ? again.key : newSendKey();
    const press = writes.run({
      detail: { field: field.name, issue: issueKey },
      inline: true,
      key,
      kind: issueFieldWrite,
      label: field.label(value),
      presentation: "indicator",
      request: issuePatchRequest(issueKey, field.patch(value)),
      target: `issue:${issueKey}`,
    });
    const id = writes.rows().find((row) => row.key === key)?.id;
    mine.current = id;
    /** This press has settled here, so the control holds it no longer, and whether a newer write
     *  of the field is left: the store has let this one go by now, and one write of a target
     *  goes out at a time, so every write of the field still in the line was pressed after it. */
    const settled = (): boolean => {
      setInHand((count) => count - 1);
      if (mine.current === id) {
        mine.current = undefined;
      }
      return writes.rows().some((row) => stillInLine(row, issueKey, field.name));
    };
    press.then(
      (answer) => {
        if (settled()) {
          return;
        }
        // Nothing of this field is left in the line, so the chain is over and the next press
        // starts from what is on screen now.
        refusedBase.current = undefined;
        if (answer !== null) {
          mergeIssue(queryClient, answer);
        }
        reread();
      },
      (error: unknown) => {
        const newer = settled();
        if (error instanceof WriteDiscardedError) {
          return;
        }
        const refusal = error instanceof Error ? error : new Error(String(error));
        if (retriesUnderItsKey(refusal)) {
          stopped.current = { key, value };
        }
        if (newer) {
          // A press after this one is still in the line and will roll back for both of them:
          // to here, before this refused value was shown, rather than to a value the server
          // refused.
          refusedBase.current ??= snapshot;
        } else {
          restore(refusedBase.current ?? snapshot);
          refusedBase.current = undefined;
          reread();
        }
        setRefused({ message: refusal.message, retriable: !newer, value });
      }
    );
  };
  return {
    error: refused?.message,
    failed: refused !== undefined,
    pending: inHand > 0 || inLine,
    retry: refused?.retriable === true ? () => submit(refused.value) : undefined,
    submit,
  };
}
