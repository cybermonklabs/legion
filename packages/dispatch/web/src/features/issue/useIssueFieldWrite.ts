import { useRef, useState } from "react";

import { newSendKey } from "../../lib/send-key";
import { retriesUnderItsKey, WriteDiscardedError } from "../writes/keyed-write";
import { ofKind, useWrites, type Writes } from "../writes/write-store";
import {
  type IssueField,
  type IssueFieldPick,
  type IssueFieldValues,
  issueFieldWrite,
  newerWriteOf,
  pickLabel,
  pickRequest,
  sameField,
} from "./issue-field-writes";

export interface IssueFieldWrite<Value> {
  /** The server's reason after a refused save, until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  /** True while a pick made here is in the store, not yet landed, refused or taken out. */
  pending: boolean;
  /** Makes the refused pick again, under its own key where Dispatch never decided it
   *  (`retriesUnderItsKey`); a no-op while nothing is refused. */
  retry: () => void;
  /** Picks `value`: one write of the page's store on the issue's line, which takes out every
   *  other pick of the field still waiting there unsent, so the newest is the one saved. */
  submit: (value: Value) => void;
}

/** A refused pick as the picker reports it: the server's reason, and the value to pick again. */
interface Refused<Value> {
  readonly message: string;
  readonly value: Value;
}

/** Every pick in `writes` of `field` of `issueKey`. */
function picksOf(writes: Writes, of: Pick<IssueFieldPick, "field" | "issueKey">) {
  return writes.rows().flatMap((row) => {
    const pick = ofKind(row, issueFieldWrite);
    return pick !== undefined && sameField(pick.detail, of) ? [pick] : [];
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
 * of issues (a project's board and table, the Priorities view's tables) and each Inbox row, keeps
 * the field shown as its newest live write - whichever control made it - over every answer while
 * the pick is live, and when it ends without landing puts back the newest live write of the
 * field, or else the value Dispatch last answered for it and reads the issue again
 * (`issue-field-writes.ts`).
 *
 * The picker shows its own refusal (`inline`), so the shell's indicator shows the write only
 * while it is out. A refused pick with no newer live write of its field - a pick, a board move to
 * another lane - shows the server's reason with Retry; one with a newer live write shows nothing,
 * since the screen shows that newer write and a Retry would join behind it and be stored last. A
 * pick of the value the reader stopped is that press made again under its key, so Dispatch
 * answers it with what it did.
 */
export function useIssueFieldWrite<F extends IssueField>(
  issueKey: string,
  field: F
): IssueFieldWrite<IssueFieldValues[F]> {
  type Value = IssueFieldValues[F];
  const writes = useWrites();
  const [out, setOut] = useState(0);
  const [refused, setRefused] = useState<Refused<Value> | undefined>(undefined);
  /** The last pick here the reader stopped, which a pick of the same value makes again. */
  const stopped = useRef<{ readonly key: string; readonly value: Value } | undefined>(undefined);
  /** Joins `pick` to its line under `key`, takes out the picks of its field it replaces, and
   *  reports what becomes of it. */
  const join = (pick: IssueFieldPick, key: string, value: Value): void => {
    const press = writes.run({
      detail: pick,
      inline: true,
      key,
      kind: issueFieldWrite,
      label: pickLabel(pick),
      presentation: "indicator",
      request: pickRequest(pick),
      target: `issue:${issueKey}`,
    });
    // `run` joins the write to its line before it answers, so the store holds it now.
    const joined = picksOf(writes, pick).find((row) => row.key === key);
    if (joined === undefined) {
      throw new Error(`${pickLabel(pick)}: the store holds no write under the key it just ran`);
    }
    for (const other of picksOf(writes, pick)) {
      if (other.id !== joined.id && other.status === "queued" && !other.sent) {
        writes.discard(other.id);
      }
    }
    press.then(
      () => setOut((count) => count - 1),
      (error: unknown) => {
        setOut((count) => count - 1);
        // This engine took it out for a newer pick: no refusal of Dispatch's.
        if (error instanceof WriteDiscardedError) return;
        const refusal = error instanceof Error ? error : new Error(String(error));
        if (retriesUnderItsKey(refusal)) stopped.current = { key, value };
        // A newer live write of the field is left in the line, and the screen shows it.
        if (newerWriteOf(writes, joined)) return;
        setRefused({ message: refusal.message, value });
      }
    );
  };
  const submit = (value: Value): void => {
    const again = stopped.current;
    stopped.current = undefined;
    const key = again !== undefined && Object.is(again.value, value) ? again.key : newSendKey();
    // `value` is the value of `field`, which the type checker cannot pair across the union of
    // fields: the one pick the two make together.
    const pick = { field, issueKey, value } as IssueFieldPick;
    setRefused(undefined);
    setOut((count) => count + 1);
    // The pick joins its line in a microtask of the press - before anything else can happen, a
    // reload included - so the picker renders the press as pending before the pick shows: a
    // surface that keeps its control while its write is live (the Inbox's Assign to me, whose row
    // the pick moves out of the Unassigned filter) has said so by then.
    queueMicrotask(() => join(pick, key, value));
  };
  return {
    error: refused?.message,
    failed: refused !== undefined,
    pending: out > 0,
    retry: () => {
      if (refused !== undefined) submit(refused.value);
    },
    submit,
  };
}
