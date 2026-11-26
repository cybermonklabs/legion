import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { flushSync } from "react-dom";
import { ApiError, api, issuePatchRequest, type WriteRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { advancesIssuesParams } from "../../api/queries";
import { invalidateWaitingForPaused } from "../../api/query-client";
import { useReadBound } from "../../api/read-bound";
import type { UpdateIssueInput } from "../../api/types";
import { holdHandOff } from "../../components/focus-place";
import { newSendKey } from "../../lib/send-key";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { plainWrite, useWrites } from "../writes/write-store";
import { type IssueListing, listingName } from "./issues-table-data";
import { buildItems, type Item, queuedInOrder, startNextWrites } from "./issues-table-model";
import { type TableAction, useTableRecovery } from "./issues-table-recovery";

/** One step of an action: the issue it writes, its change, and the write key it was given. */
interface Patch {
  key: string;
  input: UpdateIssueInput;
  write: string;
}
interface Notice {
  kind: "success" | "error";
  text: string;
}
/** An action waiting on the reader's yes, with everything the dialog says and does. */
interface Confirmation {
  title: string;
  body: string;
  confirmLabel: string;
  run: () => void;
}

/** What the list says when its read failed, and what an action's notice says before why the list
 *  read after it has not settled. */
export const ISSUES_NOT_LOADED = "Could not load issues.";

/** What a bulk action is pressed over, read at the press: the rows the table lists, and the way
 *  to clear the table's selection once the action has moved what it named. A table drawn again
 *  since the press (another view of the Issues tab) has a selection of its own, which the action
 *  leaves as it is. */
export interface ActionRows {
  readonly items: readonly Item[];
  readonly clearSelection: () => void;
}

/** A table's bulk actions and what they say. */
export interface IssueTableActions {
  /** An action is out: its controls are held until the list read again has drawn its change. */
  readonly busy: boolean;
  readonly notice: Notice | undefined;
  /** The issues the last action moved, which their rows mark. */
  readonly moved: ReadonlySet<string>;
  /** Whether the press of this action is taking the row of the issue `key` out of the view: an
   *  issue the action's writes are for, from the reader's press, not from its write's answer, so
   *  a row is named while its write is still out, as the bar's hand-off and the List's anchor
   *  read it. The name ends at once where that write is refused or stopped, and for every row
   *  once the action is over (`busy`) and the list read after it has settled: the commit that
   *  lets the action go draws that read at the latest, and a read that outlived the hold is
   *  drawn in the commit that ends `unsettled`, so by then each row the press took away has left
   *  the list and each row still listed stays. The List's anchor asks the render before a
   *  commit as well (`ViewportAnchor`'s `leaving`), so the rows are named in that commit. A row
   *  a later live update takes away is not named. Undefined with no action out: no row is named. */
  readonly leaving: ((key: string) => boolean) | undefined;
  /** The record of an action a tab left behind, which this tab offers to Finish or Dismiss. */
  readonly interrupted: TableAction | undefined;
  readonly interruptedDescription: string | undefined;
  /** The action waiting on the reader's yes. */
  readonly confirming: Confirmation | undefined;
  readonly cancelConfirming: () => void;
  readonly confirmed: () => void;
  readonly finish: (clearSelection: () => void) => void;
  readonly dismiss: () => void;
  readonly startNext: (keys: readonly string[], rows: ActionRows, confirmFirst?: boolean) => void;
}

/**
 * The bulk actions of a table of `listing` and the record that lets a reload finish one. The
 * caller runs this above the views its table draws, so an action runs on across a change of view
 * and stops only when the caller goes: the reader leaves the page or its Issues tab, or closes
 * the browser tab.
 */
export function useIssueTableActions(listing: IssueListing): IssueTableActions {
  const client = useQueryClient();
  const writes = useWrites();
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<Notice>();
  // Why the list read after an action has not settled, once its hold ran out (`useReadBound`).
  const [unsettled, holdThrough] = useReadBound();
  const [moved, setMoved] = useState<ReadonlySet<string>>(new Set());
  // The issues the action in hand is taking out of the view, which `leaving` names: each issue
  // its writes are for, from the reader's press that began it (`perform`). An issue whose write
  // is refused or stopped stays where it is, so its name ends at once (`apply`, and `finish` for
  // the writes it waits on or sends again).
  const [taking, setTaking] = useState<ReadonlySet<string>>(new Set());
  /** Ends the name of `key` at once (`taking`): a write for it was refused or stopped. */
  const release = (key: string) =>
    setTaking((current) => {
      if (!current.has(key)) return current;
      const kept = new Set(current);
      kept.delete(key);
      return kept;
    });
  const [confirming, setConfirming] = useState<Confirmation>();
  // What an interrupted action is recorded under, which is the set it was started on: one
  // priority's work table resumes its own action and not the table of the priority beside it.
  const recoveryKey = listingName(listing);
  const recovery = useTableRecovery(recoveryKey, (text) =>
    setNotice({ kind: text.startsWith("Changes not saved") ? "error" : "success", text })
  );
  /** The open issues of this table's listing, read afresh: what Finish resumes Start next
   *  against. Start next moves only Todo issues, so no closed issue takes part in its writes. */
  const listOpen = () =>
    "project" in listing
      ? api.listIssues({ project: listing.project, open: true })
      : api.listIssues(advancesIssuesParams(listing, true));

  const request = <T>(value: WriteRequest<T>, label: string, issue: string, key: string) => {
    const answer = writes.run({
      kind: plainWrite,
      detail: null,
      key,
      label,
      presentation: "indicator",
      request: value,
      target: `issue:${issue}`,
    });
    // Every write is watched, so a refusal names its issue.
    recovery.watch(key, issue);
    return answer;
  };
  /** Sends one step's patch under the write key the step was given: the action recorded every
   *  key in one storage write before the first send, rather than one write per issue. A refusal
   *  throws the text that names the change not saved, and ends the issue's name as leaving
   *  (`release`): the write leaves its issue where it is. A change the reader stopped in Pending
   *  changes may have been saved, so it is said apart from a refused one, in the words of a
   *  stopped write; the error's own are a composer's. */
  const apply = async ({ key, input, write }: Patch) => {
    try {
      const answer = await request(
        issuePatchRequest(key, input),
        `Update ${key} from the issues table`,
        key,
        write
      );
      if (answer !== null) mergeIssue(client, answer);
      setMoved((current) => new Set([...current, key]));
    } catch (error) {
      release(key);
      // The step runs through `writes.run` with no hook, so no `KeyedWriteResult` is in hand to
      // read the method from; each issues-table change is this `PATCH`.
      const unsaved =
        error instanceof SendStoppedError
          ? stoppedWriteNote(`the change to ${key}`, "saved", { method: "PATCH", stopped: error })
          : `Changes not saved: ${key}. ${error instanceof Error ? error.message : String(error)}`;
      throw new Error(
        `0 of 1 changes saved. ${unsaved} Retry each failed change in Pending changes.`,
        {
          cause: error,
        }
      );
    }
  };
  /** Runs `action`, which the reader's press began, for the issues `sends` as the table lists
   *  them: those are named as leaving from the press (`taking`). It says `said` once it answers
   *  what it changed, and then clears the selection it was pressed over (`clearSelection`). */
  const perform = async (
    said: string,
    sends: readonly string[],
    action: () => Promise<readonly string[] | undefined>,
    clearSelection: () => void
  ) => {
    // The action's whole run keeps the armed hand-off of the reader's press that began it live
    // (`holdHandOff`), until its controls are let go. Start next writes one step at a time and
    // runs on after its first writes are answered. Without this hold the press could end before
    // the action did, and the bar's going would read as no doing of the reader's.
    const endHold = holdHandOff();
    setBusy(true);
    setNotice(undefined);
    setMoved(new Set());
    setTaking(new Set(sends));
    // Whether the action may have changed what the list draws: it answered what it changed, or a
    // write landed while it ran, or ended without Dispatch's verdict (the reader stopped it) and
    // so may have landed. A write Dispatch refused (`ApiError`) changed nothing.
    let mayHaveChanged = false;
    // Whether the action answered what it changed. Only such an action clears the selection.
    let answered = false;
    const unwatch = writes.onOutcome((outcome) => {
      if (
        outcome.kind === "landed" ||
        (outcome.kind === "refused" && !(outcome.refusal instanceof ApiError))
      )
        mayHaveChanged = true;
    });
    try {
      const changed = await action();
      if (changed !== undefined) {
        mayHaveChanged = true;
        answered = true;
        setNotice({ kind: "success", text: said });
        setMoved(new Set(changed));
      }
    } catch (error) {
      setNotice({ kind: "error", text: error instanceof Error ? error.message : String(error) });
    } finally {
      unwatch();
      // An action that may have changed the list keeps its controls held (`busy`) through the read
      // of the list that draws what it changed - a row's Start next gone once its issue starts
      // next. The hold above lasts until then, so that going is still the press's doing
      // (`causedByReader`). A batch that saved some of its changes before a refusal changed the
      // list too. The read settles either way, and a failed one lets them go. A read that has not
      // settled by the request deadline (`REQUEST_DEADLINE_MS`) - one paused while the browser is
      // offline - lets them go then, and the notice says why the list is not read yet
      // (`useReadBound`). An action that changed nothing - every write refused, or none made -
      // lets them go at once, while the list is still read again behind them.
      // An action that answered what it changed clears the selection in the same batch that lets
      // its controls go. So the selection bar, held, stays until that read settles or the
      // deadline passes, and its hand-off reads the view the action left. A marked row that left
      // the view is not in it, and the focus goes to the row now at its place
      // (`useLastMarkedPlace`). Where the row marked last is still listed, the focus goes to its
      // checkbox. An action that failed or changed nothing keeps the selection.
      // Read again waiting on a fetch the network has paused, since the hold below has a bound
      // of its own (`invalidateWaitingForPaused`, `useReadBound`).
      const listed = invalidateWaitingForPaused(client, ["issues"]);
      void client.invalidateQueries({ queryKey: ["issue"] });
      void client.invalidateQueries({ queryKey: ["inbox"] });
      if (mayHaveChanged) await holdThrough(listed);
      // The bar's going and its hand-off are one render. This line runs outside any event of the
      // reader's - the turn a write's answer takes, or the read's own bound - so React would
      // commit it at normal priority and run the bar's hand-off, a passive effect
      // (`components/SelectionBar.tsx`), in a later task: a frame would paint with the focus on
      // the page's body. The press ends a frame and a task from here (`endHold`,
      // `afterComponents`), which can come before that hand-off and leave it reading a press
      // already answered, so it would move nothing. `flushSync` commits the going here and runs
      // the bar's effects with it, so the focus is on the marked row's checkbox in the render
      // the bar goes, before the press ends.
      flushSync(() => {
        if (answered) clearSelection();
        setBusy(false);
      });
      endHold();
    }
  };
  const named = (keys: readonly string[], items: readonly Item[]) => {
    if (keys.length !== 1) return `${keys.length} issues`;
    const key = keys[0] ?? "";
    return `${key} “${items.find((item) => item.issue.key === key)?.issue.title ?? key}”`;
  };
  /** Whether a bulk action may begin. Where it may not, the notice says why: another tab's move
   *  or this tab's own interrupted action is still in the store (`useTableRecovery`). */
  const canBegin = () => {
    const refused = recovery.refusal();
    if (refused === undefined) return true;
    setNotice({ kind: "error", text: refused });
    return false;
  };
  /** The rank writes of a start-next action, sent in order, stopping the moment the action is
   *  no longer the one in hand: the part of the page that runs it went (the reader left its page
   *  or its Issues tab, or closed the browser tab), or, in a browser with no lock manager,
   *  another tab dismissed it. A change of view of the Issues tab does not stop it. One loop and
   *  one guard serve the first attempt and the resume; `failure` is how the attempt names what
   *  did not move. The answer says whether the action is still the one in hand when the last
   *  write landed. */
  const runStartNext = async (
    record: TableAction,
    current: readonly Item[],
    failure: (remaining: readonly string[], error: unknown) => string = (_remaining, error) =>
      error instanceof Error ? error.message : String(error)
  ) => {
    // Every step's key, minted and stored in one write before the first send: 250 issues move
    // up under one storage write rather than 250 read-modify-write cycles of the whole store.
    const steps = startNextWrites(current, record.keys).map((operation) => ({
      ...operation,
      write: newSendKey(),
    }));
    recovery.record(Object.fromEntries(steps.map(({ key, write }) => [key, write])), record.id);
    for (const [index, step] of steps.entries()) {
      if (!recovery.active(record.id)) return false;
      try {
        await apply(step);
      } catch (error) {
        throw new Error(
          failure(
            steps.slice(index).map(({ key }) => key),
            error
          )
        );
      }
    }
    return recovery.active(record.id);
  };
  /** Sends a start-next action for queued issues already checked by `startNext`. The controls are
   *  held (`perform`) from the press, before the begin asks for the listing's lock, so the focus
   *  moves as the press's doing when the action changes the rows. */
  const runQueued = (queued: readonly string[], { items, clearSelection }: ActionRows) => {
    if (!startNextWrites(items, queued).length) {
      setNotice({
        kind: "success",
        text: `${named(queued, items)} already ${queued.length === 1 ? "starts" : "start"} next.`,
      });
      return;
    }
    void perform(
      `${named(queued, items)} now ${queued.length === 1 ? "starts" : "start"} next.`,
      queued,
      async () => {
        // Refused here too: another tab can begin a move while the confirm is open.
        const begun = await recovery.begin({ kind: "startNext", keys: queued }, async (record) => {
          const ran = await runStartNext(record, items, (remaining, error) => {
            // One step goes at a time, so the first of `remaining` is the one `apply` wrote. One
            // the reader stopped may have moved up, which `apply`'s own words say, so it is not
            // listed as one that did not.
            const stopped = error instanceof Error && error.cause instanceof SendStoppedError;
            const unmoved = stopped ? remaining.slice(1) : remaining;
            const why = error instanceof Error ? error.message : String(error);
            return unmoved.length === 0 ? why : `Did not move up: ${unmoved.join(", ")}. ${why}`;
          });
          // Ended while this tab still holds the listing's lock, so no tab finds it left behind.
          if (ran) recovery.end(record.id);
          return ran;
        });
        if (begun.kind === "refused") throw new Error(begun.text);
        return begun.moved ? queued : undefined;
      },
      clearSelection
    );
  };
  /**
   * Starts `keys` next, of `rows`, what the table lists at the press. Start next keeps its own
   * set, the Todo issues labelled `legion` in rank order, and each issue of that set among `keys`
   * moves ahead of the others (`startNextWrites`). A selection with none of the set is refused
   * with a text that names the set. The reader is asked first where some selected issues are
   * outside the set, with how many stay where they are, and where the selection holds rows off
   * the page (`confirmFirst`: `Select all N matching`), since each issue is one write.
   */
  const startNext = (keys: readonly string[], rows: ActionRows, confirmFirst = false) => {
    if (!canBegin()) return;
    const queued = queuedInOrder(rows.items, keys);
    if (!queued.length) {
      setNotice({ kind: "error", text: "Start next moves only Todo issues labelled legion." });
      return;
    }
    const outside = keys.length - queued.length;
    if (confirmFirst || outside > 0) {
      const moves = `${queued.length} ${queued.length === 1 ? "issue moves" : "issues move"} ahead of the other Todo issues labelled legion.`;
      const stays =
        outside === 0
          ? ""
          : outside === 1
            ? " 1 selected issue is not a Todo issue labelled legion. It stays where it is."
            : ` ${outside} selected issues are not Todo issues labelled legion. They stay where they are.`;
      setConfirming({
        title: "Start these next?",
        body: `${moves} Dispatch sends one change for each issue.${stays}`,
        confirmLabel: "Start next",
        run: () => runQueued(queued, rows),
      });
      return;
    }
    runQueued(queued, rows);
  };

  /** Finishes the Start next left behind, while this tab holds its lock (`recovery.resume`): no
   *  other tab runs that move meanwhile, and none offers to end it. Once it has, the selection
   *  of the table it was pressed on is cleared (`clearSelection`). */
  const finish = (clearSelection: () => void) => {
    const interrupted = recovery.current.current;
    if (!interrupted) return;
    // A write of the interrupted action that Finish waits on or sends again (`recovery.settle`)
    // and that is refused or stopped again leaves its issue where it is, so that issue's name ends
    // at once, as `apply` ends the name of a write it sends: those writes are Pending changes' own
    // and never pass through `apply`. Each is known by its row there, the step it is for read off
    // the key the record holds: a write sent again keeps its row, and can be sent under a new key.
    const steps = new Map(Object.entries(interrupted.writes).map(([step, key]) => [key, step]));
    const stepOfRow = new Map(
      writes.rows().flatMap((row) => {
        const step = steps.get(row.key);
        return step === undefined ? [] : [[row.id, step] as const];
      })
    );
    const unwatch = writes.onOutcome((outcome) => {
      const step = outcome.kind === "refused" ? stepOfRow.get(outcome.write.id) : undefined;
      if (step !== undefined) release(step);
    });
    void perform(
      "Finished the interrupted action.",
      interrupted.keys,
      () =>
        recovery.resume(interrupted.id, async () => {
          await recovery.settle(interrupted);
          if (!recovery.active(interrupted.id)) return undefined;
          const record = recovery.current.current;
          if (!record) return undefined;
          const current = await listOpen();
          if (!recovery.active(record.id)) return undefined;
          await runStartNext(
            record,
            buildItems(current, () => "")
          );
          if (!recovery.active(record.id)) return undefined;
          recovery.end(record.id);
          return record.keys;
        }),
      clearSelection
    ).finally(unwatch);
  };
  // Offered only where no tab runs its move: a move another tab still runs is that tab's.
  const interrupted = busy ? undefined : recovery.leftBehind;
  return {
    busy,
    // A list read that has not settled is said after what the action did, in the words the List
    // says for a list read that failed.
    notice:
      unsettled === undefined
        ? notice
        : {
            kind: "error" as const,
            text: `${notice === undefined ? "" : `${notice.text} `}${ISSUES_NOT_LOADED} ${unsettled}.`,
          },
    moved,
    leaving: busy || unsettled !== undefined ? (key: string) => taking.has(key) : undefined,
    interrupted,
    interruptedDescription:
      interrupted &&
      `Start next: ${interrupted.keys.join(", ")} may still need to move up, in that order.`,
    confirming,
    cancelConfirming: () => setConfirming(undefined),
    confirmed: () => {
      const waiting = confirming;
      setConfirming(undefined);
      waiting?.run();
    },
    finish,
    dismiss: recovery.dismiss,
    startNext,
  };
}
