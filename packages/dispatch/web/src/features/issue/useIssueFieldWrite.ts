import { useRef, useState } from "react";
import { useCache, useWriteSelection, useWrites } from "../../api/cache/cache";

import { mergeIssue } from "../../api/cache/issue-cache";
import {
  defineWriteKind,
  liveWrite,
  ofKind,
  type Shown,
  type Write,
} from "../../api/cache/write-store";
import { issuePatchRequest } from "../../api/client";
import { inboxQuery, issueQuery, projectListsKey } from "../../api/queries";
import type { UpdateIssueInput } from "../../api/types";
import {
  pressKey,
  type StoppedPress,
  stoppedPress,
  WriteDiscardedError,
} from "../writes/keyed-write";
import { type Shape, shape } from "../writes/write-storage";

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

/** Whether `row` is a write of `issue`'s `field` that may still reach the server (`liveWrite`).
 *  A refused one never is - the engine shows its own refusals (`inline`), so the store lets it
 *  go the moment Dispatch refuses it. */
function stillInLine(row: Write, issue: string, field: string): boolean {
  const write = ofKind(row, issueFieldWrite);
  return (
    write !== undefined &&
    write.detail.issue === issue &&
    write.detail.field === field &&
    liveWrite(write)
  );
}

/**
 * The optimistic write engine behind every single-field issue write (priority, assignee,
 * repository). The caller supplies `patch`, the request body for a value, and `show`, which puts
 * the value in the issue detail, every project issue list and every inbox row that shows the
 * issue, through the store's own `Shown`.
 *
 * Every pick joins the store's line when it is pressed: one write of its own on the issue's line,
 * kept with the request it will send, so picks reach the server in press order whatever becomes
 * of the control - another page, an unmount, a reload - and none the person saw is lost. The
 * store composes the pick from the moment the write joins over each answer that lands on one
 * of those caches while the write is live (R7 of dispatch://INFRA-299), so an answer composed
 * before the pick arrives whole - every other row of it with it - and leaves the person's own
 * change on screen. A pick that leaves without landing - refused, discarded in Pending changes,
 * taken by a sign-out - stops being composed, as every write's change does: each cache it named
 * reads as what Dispatch last gave it with every other live write's change over it,
 * newer picks of the field among them, and is read again (R10), so a read that fails leaves the
 * screen on the value the server last confirmed rather than on a pick nothing will send. The
 * engine keeps no snapshot of its own. It merges the server's answer and reads the issue again
 * once a save lands with no newer write of the field left in the line, where the screen is
 * already the newer pick's. A pick that replaces one of this control's that has not gone out
 * takes it out of the line (`Writes.discard`), which refuses that press with
 * `WriteDiscardedError`: the engine made that write go, so the person sees no failure for it, as
 * they see none for a pick discarded in Pending changes.
 */
export function useIssueFieldWrite<Value>(
  issueKey: string,
  field: {
    /** The field's name in the store, which tells this engine's writes from those of another
     *  field of the same issue. */
    name: string;
    patch: (value: Value) => UpdateIssueInput;
    /** Shows the value on each cache that draws it, through the store's own `Shown`: the store
     *  composes this over every read of one of those caches while the write is live. */
    show: (value: Value, shown: Shown) => void;
    /** What the shell's indicator names one save of this field. */
    label: (value: Value) => string;
  }
): IssueFieldWrite<Value> {
  const cache = useCache();
  const writes = useWrites();
  const detailKey = issueQuery(issueKey).queryKey;
  const [refused, setRefused] = useState<Refused<Value> | undefined>(undefined);
  /** The write this control pressed last, while a pick of its own may still replace it. Another
   *  surface's pick, and one this page pressed before a reload, stand in the line on their own:
   *  nothing of theirs is this control's to take out. */
  const mine = useRef<number | undefined>(undefined);
  /** The last press the reader stopped, which a press of the same value makes again under its
   *  key, so Dispatch answers it with what it did rather than taking the write twice. */
  const stopped = useRef<StoppedPress<Value> | undefined>(undefined);
  /** How many presses of this control the store has settled but the engine has not reported yet.
   *  A write the store lets go - Dispatch refused it, and the engine shows that refusal itself
   *  (`inline`) - leaves `rows()` a commit before this engine says what became of it, and a
   *  surface that keeps its control while its write is live (the Inbox's `Assign to me`) would
   *  drop the control in that gap. */
  const [inHand, setInHand] = useState(0);
  const inLine = useWriteSelection(`issue-field:${issueKey}:${field.name}`, (rows) =>
    rows.some((row) => stillInLine(row, issueKey, field.name))
  );
  // Another write to the same issue (a status change from the header) may have settled in
  // between; reading the issue again makes it server-authoritative rather than trusting the
  // arrival order of the two responses.
  const reread = (): void => {
    void cache.invalidate({ queryKey: detailKey });
    void cache.invalidate({ queryKey: ["issues"] });
    void cache.invalidate({ queryKey: inboxKey });
  };
  const submit = (value: Value): void => {
    // Two picks of this control that have not gone out are one request, the newest: the one
    // waiting leaves the line and its press is refused `WriteDiscardedError`, which the engine
    // swallows. A write already out, or one another surface pressed, is nobody's to take out.
    if (mine.current !== undefined) writes.discard(mine.current);
    setRefused(undefined);
    setInHand((count) => count + 1);
    for (const queryKey of [detailKey, projectListsKey, inboxKey]) {
      // A read in flight would otherwise answer with the server's older value over the pick.
      void cache.cancel({ queryKey });
    }
    // A value here is a priority, a login or a repository, so the same value is the same press
    // made again, under the key Dispatch may already have taken that press under.
    const key = pressKey(stopped.current, value);
    stopped.current = undefined;
    const press = writes.run({
      detail: { field: field.name, issue: issueKey },
      inline: true,
      key,
      kind: issueFieldWrite,
      label: field.label(value),
      presentation: "indicator",
      request: issuePatchRequest(issueKey, field.patch(value)),
      // The pick is shown here, by the store, so it is composed over each answer that lands on
      // one of the caches it names until this write settles (R7), and stops being composed if
      // it leaves without landing (R10): this engine undoes nothing itself.
      shows: (shown) => field.show(value, shown),
      target: `issue:${issueKey}`,
    });
    // The row `run` has just joined, read at the press while the store holds it: its id tells
    // this press from another surface's, and where it joined the store's order says every write
    // of the field that joined after it is a newer pick.
    const row = writes.rows().find((candidate) => candidate.key === key);
    if (row === undefined) {
      throw new Error(`${field.label(value)}: the store holds no write under the key it just ran`);
    }
    const { id, joined } = row;
    mine.current = id;
    /** This press has settled here, so the control holds it no longer, and whether a newer write
     *  of the field is left: one that joined after this one and is still live. Whether the store
     *  still publishes this write as live when its press settles is the store's to say, so the
     *  answer reads only the writes that joined after it. */
    const settled = (): boolean => {
      setInHand((count) => count - 1);
      if (mine.current === id) {
        mine.current = undefined;
      }
      return writes
        .rows()
        .some((row) => row.joined > joined && stillInLine(row, issueKey, field.name));
    };
    press.then(
      (answer) => {
        if (settled()) {
          return;
        }
        if (answer !== null) {
          mergeIssue(cache, answer);
        }
        reread();
      },
      (error: unknown) => {
        const newer = settled();
        if (error instanceof WriteDiscardedError) {
          return;
        }
        // The pick is composed no longer: the screen is the value the server last confirmed, with
        // every live write's change over it - a newer pick of this field among them - read again.
        const refusal = error instanceof Error ? error : new Error(String(error));
        stopped.current = stoppedPress(key, value, refusal) ?? stopped.current;
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
