import { notifyManager, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import type { WriteMethod } from "../../api/client";
import { newSendKey } from "../../lib/send-key";
import {
  retriesUnderItsKey,
  SendStoppedError,
  stoppedWriteNote,
  WriteDiscardedError,
} from "../writes/keyed-write";
import { waitingToSend } from "../writes/PendingWritesIndicator";
import { ofKind, useWrites, type Write, type Writes } from "../writes/write-store";
import {
  fieldWritesOf,
  type IssueField,
  type IssueFieldPick,
  type IssueFieldValues,
  issueFieldWrite,
  newerWriteOf,
  pickLabel,
  pickRequest,
  sameField,
  waitingMoveOf,
} from "./issue-field-writes";

export interface IssueFieldWrite<Value> {
  /** The server's reason after a refused save, or what a save the reader stopped may have done
   *  and what pressing the picker does now (`stopped`), until the next pick here; a refusal
   *  reported with no Retry also ends once a newer write of the field lands or the field is
   *  picked again in another picker. */
  error: string | undefined;
  /** True after a save failed, until the next pick here; a refusal reported with no Retry also
   *  ends once a newer write of the field lands or the field is picked again in another picker. */
  failed: boolean;
  /** True while a pick made here is in the store, not yet landed, refused or taken out. */
  pending: boolean;
  /** Makes the refused pick again - its issue and its value, from the pick itself - under its own
   *  key where Dispatch never decided it (`retriesUnderItsKey`). Undefined while nothing is
   *  refused, and for a refused pick with a newer live write of its field, which the screen shows
   *  and Dispatch stores last. */
  retry: (() => void) | undefined;
  /** The field as its picker draws it. From a pick here it is that pick, until TanStack Query has
   *  told the observers of the issue's caches of it or the pick is refused, taken out or
   *  discarded; else it is `host`, the value the picker's host reads from those caches. A pick
   *  made while a board move of its field still waits its turn on its board is never drawn: the
   *  move is newer than it (`fieldWritesOf`), and `host` draws the move's lane. Nor is a pick of
   *  another issue than the one the picker is drawn for: a pick whose host draws the picker for
   *  another issue before it is told, and a Retry of a pick made before the host did (`retry`). */
  shown: Value;
  /** What a save the reader stopped may have done and what pressing the picker does now, while
   *  `error` reports it; undefined after any other end. */
  stopped: string | undefined;
  /** Picks `value`: one write of the page's store on the issue's line, which takes out every
   *  other pick of the field still waiting there unsent, so the newest is the one saved. */
  submit: (value: Value) => void;
}

/** A refused pick as the picker reports it: what ended it, the method of its request, the value
 *  to pick again, the write it was - whose record is the pick, its issue and its value - and
 *  whether Retry may pick it - not while a newer live write of its field is left, whose landing
 *  then ends the report. */
interface Refused<Value> {
  readonly method: WriteMethod;
  readonly refusal: Error;
  readonly retryable: boolean;
  readonly value: Value;
  readonly write: Write<IssueFieldPick>;
}

/** A pick made here, as its picker draws it, with the issue it is of. Each is its own object, so
 *  the end of one pick ends its own hold and never that of a newer pick here. */
interface Held<Value> {
  readonly issueKey: string;
  readonly value: Value;
}

/** The field's name in the sentence a pick the reader stopped says (`stoppedWriteNote`). */
const FIELD_WORDS: Readonly<Record<IssueField, string>> = {
  assignee: "assignee",
  priority: "priority",
  repo: "repository",
};

/** Every pick in `writes` of `field` of `issueKey`. */
function picksOf(writes: Writes, of: Pick<IssueFieldPick, "field" | "issueKey">) {
  return writes.rows().flatMap((row) => {
    const pick = ofKind(row, issueFieldWrite);
    return pick !== undefined && sameField(pick.detail, of) ? [pick] : [];
  });
}

/** What the picker says of a pick the reader stopped: Dispatch has not confirmed the field of the
 *  pick's own issue, and what pressing the picker does now. With Retry offered, that makes the
 *  very pick again (`retryNote`, by the request's method); with a newer live write of the field
 *  left, no Retry is offered, and the newer write the screen shows goes after it. */
function stoppedPickNote(refused: Refused<unknown>, stopped: SendStoppedError): string {
  const { field, issueKey } = refused.write.detail;
  const what = `the ${FIELD_WORDS[field]} of ${issueKey}`;
  return refused.retryable
    ? stoppedWriteNote(what, "saved", { method: refused.method, stopped })
    : stoppedWriteNote(what, "saved", {
        anew: "The newer change shown goes to Dispatch after it, so that one is stored last.",
      });
}

/**
 * The write engine behind each picker of one field of an issue: its priority, its assignee or
 * its repository (`useIssuePriority`, `useIssueAssignee`, `useIssueRepo`). Each `submit` is one
 * write of the page's store of the kind `issueFieldWrite`, joined to the issue's line
 * (`issue:<KEY>`) within the press, behind every other write of that issue, so the picks go out
 * in the order they were made, are kept across a reload, and are sent although the picker is
 * gone. A pick joins and then takes out every other pick of the same field still waiting in the
 * line unsent, from this picker or another of the same field (an Inbox row and its reading
 * pane): two picks nobody has sent are one request, the newest, and the pick taken out shows no
 * error and puts nothing back. The write, not this hook, shows the pick on the issue, each list
 * of issues (a project's board and table, the Priorities view's tables), each Inbox row and each
 * ask of the one read behind the Inbox's opt-in, keeps the field shown as its newest live write -
 * whichever control made it - over every answer while the pick is live, and when it ends without
 * landing puts back the newest live write of the field, or else the value Dispatch last answered
 * for it and reads the issue again (`issue-field-writes.ts`).
 *
 * The picker shows its own refusal (`inline`), so the shell's indicator shows the write only
 * while it is out. A refused pick with no newer live write of its field - a pick, a board move to
 * another lane, out or still waiting its turn on its board (`newerWriteOf`) - shows the server's
 * reason with Retry; one with a newer live write shows the reason with no Retry, since the screen
 * shows that newer write and a Retry would join behind it and be stored last. That report ends
 * once a newer write of the field lands or the reader picks the field again, in this picker or
 * any other, and offers Retry once every newer write of the field has ended without landing -
 * refused or discarded - since the refused pick then has no newer live write of its field. A pick
 * the reader stopped is reported in the words of a write that may have landed (`stoppedPickNote`),
 * never the stop's own, which are a composer's. A pick of the value the reader stopped, on the
 * issue it was of, is that press made again under its key, so Dispatch answers it with what it
 * did.
 *
 * Every byte a pick sends rides in it: its issue, its field and its value (`IssueFieldPick`),
 * which its request, its line and its label are made from, and Retry makes the refused pick
 * itself again, so a picker the host has drawn for another issue since still retries the issue
 * the refused pick was of, and says so.
 *
 * The picker draws `shown`. A pick reaches the issue's caches in a microtask of its press
 * (`join`), and the picker's host hears of it only when TanStack Query tells the caches'
 * observers, in its notify manager's next flush, a timer later, before which a browser can paint:
 * a picker that drew `host` from the press would draw the value its pick replaces in that frame.
 * So the press holds the pick, and the hold ends in the flush after the one that tells the host,
 * whatever the caches show by then - the pick, or a newer write of the field over it - since the
 * host passes that from then on. A pick refused, taken out or discarded ends its hold at once,
 * and the picker draws what the write put back. A newer pick here holds the picker in its place,
 * from its press. A pick made while a board move of its field still waits its turn on its board
 * holds nothing: the move is newer than it, and the caches and the picker keep showing its lane.
 * A hold is drawn only while the picker is drawn for the issue its pick is of, since the host
 * passes the field of the issue it draws: a host that draws the picker for another issue before
 * the flush, and a Retry of a pick made before the host drew it for another issue, draw `host`.
 */
export function useIssueFieldWrite<F extends IssueField>(
  issueKey: string,
  field: F,
  host: IssueFieldValues[F]
): IssueFieldWrite<IssueFieldValues[F]> {
  type Value = IssueFieldValues[F];
  const client = useQueryClient();
  const writes = useWrites();
  const [out, setOut] = useState(0);
  const [refused, setRefused] = useState<Refused<Value> | undefined>(undefined);
  const [held, setHeld] = useState<Held<Value> | undefined>(undefined);
  /** The report shown, for the listener of the issue's line below, which outlives a render. */
  const reported = useRef<Refused<Value> | undefined>(undefined);
  /** Notes the writes of the field while a refusal reported with no Retry stands (the listener
   *  below); undefined while no picker listens. */
  const noting = useRef<(() => void) | undefined>(undefined);
  const report = (next: Refused<Value> | undefined): void => {
    reported.current = next;
    setRefused(next);
    noting.current?.();
  };
  /** The last pick here the reader stopped, which a pick of the same value of the same issue
   *  makes again. */
  const stopped = useRef<{ readonly key: string; readonly pick: IssueFieldPick } | undefined>(
    undefined
  );
  /** Joins `pick` to its line under `key`, takes out the picks of its field it replaces, and
   *  reports what becomes of it, and of `hold`, which draws it. */
  const join = (pick: IssueFieldPick, key: string, hold: Held<Value>): void => {
    /** Ends `hold`, unless a newer pick here holds the picker. */
    const release = () => setHeld((current) => (current === hold ? undefined : current));
    const request = pickRequest(pick);
    const press = writes.run({
      detail: pick,
      inline: true,
      key,
      kind: issueFieldWrite,
      label: pickLabel(pick),
      presentation: "indicator",
      request,
      target: `issue:${pick.issueKey}`,
    });
    // `run` wrote the pick to the caches, and TanStack Query has queued the flush that tells the
    // host; the release queued now runs in the flush after it.
    notifyManager.schedule(release);
    // `run` joins the write to its line before it answers, so the store holds it now.
    const picks = picksOf(writes, pick);
    const joined = picks.find((row) => row.key === key);
    if (joined === undefined) {
      throw new Error(`${pickLabel(pick)}: the store holds no write under the key it just ran`);
    }
    for (const other of picks) {
      if (other.id !== joined.id && waitingToSend(other)) writes.discard(other.id);
    }
    press.then(
      () => setOut((count) => count - 1),
      (error: unknown) => {
        setOut((count) => count - 1);
        release();
        // Taken out - by a newer pick here, or by the reader's Discard in the Pending changes
        // list: no refusal of Dispatch's.
        if (error instanceof WriteDiscardedError) return;
        const refusal = error instanceof Error ? error : new Error(String(error));
        if (retriesUnderItsKey(refusal)) stopped.current = { key, pick };
        // Where a newer live write of the field is left in the line, the screen shows it, and the
        // refusal is reported with no Retry to join behind it.
        report({
          method: request.method,
          refusal,
          retryable: !newerWriteOf(client, joined),
          value: hold.value,
          write: joined,
        });
      }
    );
  };
  /** Makes `pick`, under the key of the pick the reader stopped where it is that pick again. */
  const make = (pick: IssueFieldPick, value: Value): void => {
    const again = stopped.current;
    stopped.current = undefined;
    const key =
      again !== undefined && sameField(again.pick, pick) && Object.is(again.pick.value, pick.value)
        ? again.key
        : newSendKey();
    const hold: Held<Value> = { issueKey: pick.issueKey, value };
    // A board move of the field still waiting its turn is newer than this pick (`waitingMoveOf`):
    // the picker draws `host`, which draws the move's lane, as the caches do.
    setHeld(waitingMoveOf(client, pick) ? undefined : hold);
    report(undefined);
    setOut((count) => count + 1);
    // The pick joins its line in a microtask of the press - before anything else can happen, a
    // reload included - so the picker renders the press as pending before the pick shows: a
    // surface that keeps its control while its write is live (the Inbox's Assign to me, whose row
    // the pick moves out of the Unassigned filter) has said so by then.
    queueMicrotask(() => join(pick, key, hold));
  };
  const submit = (value: Value): void => {
    // `value` is the value of `field`, which the type checker cannot pair across the union of
    // fields: the one pick the two make together.
    make({ field, issueKey, value } as IssueFieldPick, value);
  };
  // A refusal reported with no Retry stands for the newer writes of its field the screen shows,
  // until one of them lands or the person picks the field again, here or in any other picker of
  // it (a pick here ends it at its press, `submit`). At each outcome of one: landed, Dispatch
  // holds a newer value and the report has nothing left to say; refused or discarded with none
  // left live, the refused pick is one with no newer live write of its field, which Retry may
  // pick again. While such a report stands, each write of the field is noted, with its place in
  // the person's order, while the store holds it, since an outcome names a write the store has
  // let go of, whose request it no longer tells: those the store holds as the report is made
  // (`standing`), and each after; a pick the store holds that is none of those is the person
  // picking again, and ends the report. A board move still waiting its turn is noted once it
  // joins the line, before its outcome. A write's place is noted afresh at each change of the
  // store: a Retry from the Pending changes list puts a refused write at the back of the line,
  // and it is from there that its landing is newer than the refused pick or not. The listener
  // lives with the picker, so no outcome falls between a refusal and it.
  useEffect(() => {
    const of = { field, issueKey };
    const ofField = new Map<number, number>();
    /** The writes of the field the store held as the report was made; undefined while none
     *  stands. */
    let standing: Set<number> | undefined;
    const end = (next: Refused<Value> | undefined): void => {
      reported.current = next;
      setRefused(next);
      ofField.clear();
      standing = undefined;
    };
    const note = () => {
      if (reported.current?.retryable !== false) {
        ofField.clear();
        standing = undefined;
        return;
      }
      const ofIt = fieldWritesOf(client, of);
      if (standing === undefined) {
        standing = new Set(ofIt.flatMap(({ id }) => (id === undefined ? [] : [id])));
      } else if (ofIt.some(({ id, ofPick }) => ofPick && id !== undefined && !standing?.has(id))) {
        end(undefined);
        return;
      }
      for (const { id, rank } of ofIt) {
        if (id !== undefined) ofField.set(id, rank);
      }
    };
    noting.current = note;
    note();
    const stopNoting = writes.subscribe(note);
    const stopHearing = writes.onOutcome((outcome) => {
      const current = reported.current;
      const rank = ofField.get(outcome.write.id);
      ofField.delete(outcome.write.id);
      if (
        rank === undefined ||
        current === undefined ||
        current.retryable ||
        rank <= current.write.joined
      ) {
        return;
      }
      if (outcome.kind !== "landed" && newerWriteOf(client, current.write)) return;
      end(outcome.kind === "landed" ? undefined : { ...current, retryable: true });
    });
    return () => {
      noting.current = undefined;
      stopNoting();
      stopHearing();
    };
  }, [client, field, issueKey, writes]);
  const stoppedText =
    refused !== undefined && refused.refusal instanceof SendStoppedError
      ? stoppedPickNote(refused, refused.refusal)
      : undefined;
  return {
    error: stoppedText ?? refused?.refusal.message,
    failed: refused !== undefined,
    pending: out > 0,
    retry:
      refused?.retryable === true ? () => make(refused.write.detail, refused.value) : undefined,
    shown: held !== undefined && held.issueKey === issueKey ? held.value : host,
    stopped: stoppedText,
    submit,
  };
}
