import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import {
  ApiError,
  api,
  apiErrorMessage,
  issuePatchRequest,
  statusChangeRequest,
  type WriteRequest,
} from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { advancesIssuesParams } from "../../api/queries";
import { invalidateWaitingForPaused } from "../../api/query-client";
import { useReadBound } from "../../api/read-bound";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { holdHandOff } from "../../components/focus-place";
import { issueProject } from "../../lib/issue-key";
import { newSendKey } from "../../lib/send-key";
import { settledWithLimit } from "../../lib/settled";
import {
  cardHasOpenChild,
  dryRunStatusChange,
  isParentStatusConflict,
  optionInput,
} from "../issue/parent-status/useParentStatusMove";
import { retriesUnderItsKey, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { plainWrite, useWrites } from "../writes/write-store";
import { IN_WORK_STATUSES, statusText } from "./board-model";
import { type IssueListing, listingName } from "./issues-table-data";
import {
  buildItems,
  type Item,
  mergeMembers,
  queuedInOrder,
  startNextWrites,
  WRITES_IN_FLIGHT,
} from "./issues-table-model";
import { type TableAction, tableEpicWrite, useTableRecovery } from "./issues-table-recovery";

interface Patch {
  key: string;
  input: UpdateIssueInput;
}
/** The write key each step of an action was given, by the issue the step writes. */
type Keys = Readonly<Record<string, string>>;
interface Notice {
  kind: "success" | "error";
  text: string;
}
interface DuplicateEpic {
  keys: readonly string[];
  title: string;
  message: string;
  /** The reader said yes to the new epic's move into work before its first create. */
  intoWork?: true;
}
/** An action waiting on the reader's yes, with everything the dialog says and does. */
interface Confirmation {
  title: string;
  body: string;
  /** Each change the action makes first, which the dialog lists under its body. */
  changes?: readonly string[];
  confirmLabel: string;
  run: () => void;
  /** Told of a Cancel, an Escape or a press on the backdrop, beside the dialog closing: what a
   *  Finish that waits on the answer hears. */
  cancel?: () => void;
}
/** What a question says, before the page knows what its yes and its Cancel do. */
type Question = Omit<Confirmation, "cancel" | "run">;
/** What an action that moves issues under an epic is, as the question it asks says it: what the
 *  action does (`lead`), said before the question's own sentences, and its name. */
interface TargetAction {
  readonly lead?: string;
  readonly noun: "merge" | "move";
}
/** The epic whose name the reader is typing: a new one for the selected issues, or a rename. */
export type EpicNaming =
  | { kind: "new"; keys: readonly string[] }
  | { kind: "rename"; key: string; title: string };

/** What the list says when its read failed, and what an action's notice says before why the list
 *  read after it has not settled. */
export const ISSUES_NOT_LOADED = "Could not load issues.";

/** The one sentence a merge records on the source issue, written and recognised in one place.
 *  A resume compares the whole sentence against the source's own messages, because the server
 *  is what knows whether it was posted: a recorded key only says a write was made. */
const mergeReason = (into: string, title: string) =>
  `Merged into ${into} (“${title}”) from the issues table.`;

/** The status change that takes an epic into work with each parent above it that is not in work
 *  (`include_parent`): what an action sends before it moves issues under an epic the rule needs in
 *  work (D12). */
const INTO_WORK = optionInput("in_progress", "include_parent");

/** Whether moving `moving` under `target` needs `target` in work first: an issue in work under an
 *  epic that is not in work, or any open issue under a done epic. A new epic, which is no issue
 *  yet, is `undefined`: it is not in work. */
function targetMustMove(
  target: Pick<IssueSummary, "status"> | undefined,
  moving: readonly Pick<IssueSummary, "status">[]
): boolean {
  if (target?.status === "done") return moving.length > 0;
  return (
    (target === undefined || !IN_WORK_STATUSES.has(target.status)) &&
    moving.some((issue) => IN_WORK_STATUSES.has(issue.status))
  );
}

/** The steps of an interrupted action that Finish decides afresh rather than sending again: the
 *  move of its epic into work, which the guard makes or finds it need not (`intoWorkOnFinish`),
 *  and a merge's close of its source, which `closeMerge` sends only once no issue is open under
 *  the source (`refuseHeldClose`). Every other step is a write Finish sends again once the kind's
 *  guard, where it has one, lets it through (`resendsStep`). */
function afreshSteps(record: TableAction): readonly string[] {
  return record.kind === "merge" && record.source !== undefined
    ? ["intoWork", record.source]
    : ["intoWork"];
}

/** Whether Finish sends the refused write of a step of `record` again: every step but those it
 *  decides afresh (`afreshSteps`). */
function resendsStep(record: TableAction): (step: string) => boolean {
  const afresh = afreshSteps(record);
  return (step) => !afresh.includes(step);
}

/** Whether the listing `current` holds the merge's source `source` as done. */
function sourceClosed(current: readonly IssueSummary[], source: string): boolean {
  return current.find((issue) => issue.key === source)?.status === "done";
}

/** What an interrupted action of each kind says it left behind, how Finish guards it, and how it
 *  is finished. The record's kind union is the only list of kinds: a new one fails to compile
 *  until each is written for it. */
interface ResumableAction {
  describe: (record: TableAction) => string;
  /**
   * The one guard of every parent write and every move into work that Finish and `resume` make
   * (D12: the table asks whatever the setting). From the listing `current`, and the dry run of the
   * epic's move into work where the listing shows the epic out of work, it decides whether the
   * moves the action still makes need its epic in work. Where they do, it asks the action's own
   * question, and on a yes makes that move into work. It refuses an epic the kind refuses, and
   * answers false for a Cancel, or for the table going away while it asks, with nothing written
   * and the action left to finish. Answering true, it has dropped the refused writes of the steps
   * Finish decides afresh (`afreshSteps`) from Pending changes.
   *
   * Its callers, each before the writes it guards:
   * - `finish`, before it sends any refused write of the action again (`recovery.settle`);
   * - merge's `resume`, before it moves the issues left under the source and closes the source;
   * - New epic's `resume`, before it adopts the issues.
   * Start next's rank writes change no parent and no status, so it has no guard (`null`): Finish
   * reads no listing for it and sends its refused writes again unasked. No other path of this file
   * finishes an action, and the table has no undo: the first runs (`moveTo`, `askMerge`,
   * `newEpic`) ask before their own first write.
   */
  guard: ((record: TableAction, current: readonly IssueSummary[]) => Promise<boolean>) | null;
  /** Finishes the action, and answers whether it did: false where the reader cancelled the
   *  question its guard asks, which wrote nothing, so the action stays to be finished. */
  resume: (record: TableAction, current: readonly IssueSummary[]) => Promise<boolean>;
}

export function useIssueTableActions({
  listing,
  issues,
  items,
  clearSelection,
}: {
  listing: IssueListing;
  issues: readonly IssueSummary[];
  items: readonly Item[];
  clearSelection: () => void;
}) {
  const client = useQueryClient();
  const writes = useWrites();
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<Notice>();
  // Why the list read after an action has not settled, once its hold ran out (`useReadBound`).
  const [unsettled, holdThrough] = useReadBound();
  const [moved, setMoved] = useState<ReadonlySet<string>>(new Set());
  // The issues the action in hand is taking out of the view, which `leaving` names: each issue its
  // writes are for, from the reader's press that began it (`perform`, and `askTarget` while its dry
  // run is out), and an issue the action finds only as it runs - a merge's issues as its own read
  // finds them - from its write's send at the latest (`apply`). An issue whose write is refused or
  // stopped stays where it is, so its name ends at once (`apply`, and `finish` for the writes it
  // waits on or sends again).
  const [taking, setTaking] = useState<ReadonlySet<string>>(new Set());
  /** Ends the name of `key` at once (`taking`): a write for it was refused or stopped. */
  const release = (key: string) =>
    setTaking((current) => {
      if (!current.has(key)) return current;
      const kept = new Set(current);
      kept.delete(key);
      return kept;
    });
  const [duplicateEpic, setDuplicateEpic] = useState<DuplicateEpic>();
  const [unansweredEpic, setUnansweredEpic] = useState<string>();
  const [naming, setNaming] = useState<EpicNaming>();
  const [confirming, setConfirming] = useState<Confirmation>();
  /** Answers the question a Finish waits on (`askOnFinish`) while it is open. */
  const answering = useRef<((yes: boolean) => void) | undefined>(undefined);
  // The table going away ends the question a Finish waits on, as a Cancel does: the Finish ends,
  // with nothing more written, rather than wait on a dialog nobody can see.
  useEffect(() => () => answering.current?.(false), []);
  // The project a write of this table may name, which a scoped table has none of: the epic
  // actions are withheld rather than sent against a listing that belongs to no project.
  const project = "project" in listing ? listing.project : undefined;
  // What an interrupted action is recorded under, which is the set it was started on: one
  // priority's work table resumes its own action and not the table of the priority beside it.
  const recoveryKey = listingName(listing);
  const recovery = useTableRecovery(recoveryKey, (text) =>
    setNotice({ kind: text.startsWith("Changes not saved") ? "error" : "success", text })
  );
  /** Every issue of this table's listing, read afresh: what an action checks itself against. */
  const listAll = () =>
    "project" in listing
      ? api.listIssues({ project: listing.project })
      : api.listIssues(advancesIssuesParams(listing));

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
    // Every write is watched, so a refusal names its issue; only a write of a recorded action
    // belongs to it, so a refused Move or Rename retries with the ordinary wording.
    recovery.watch(key, issue);
    return answer;
  };
  const apply = async (operations: readonly Patch[], id?: string, minted?: Keys) => {
    const sends = operations.map((operation) => ({
      ...operation,
      write: minted?.[operation.key] ?? newSendKey(),
    }));
    // A caller that minted the keys has already recorded them: a step-at-a-time action records
    // its whole batch once rather than re-serialising the store on every step.
    if (minted === undefined)
      recovery.record(Object.fromEntries(sends.map(({ key, write }) => [key, write])), id);
    // Each issue is named from its write's send at the latest (`taking`): a merge finds its issues
    // only once its own read of the listing has answered.
    setTaking((current) =>
      operations.every(({ key }) => current.has(key))
        ? current
        : new Set([...current, ...operations.map(({ key }) => key)])
    );
    const results = await settledWithLimit(
      sends,
      WRITES_IN_FLIGHT,
      async ({ key, input, write }) => {
        const answer = await request(
          issuePatchRequest(key, input),
          `Update ${key} from the issues table`,
          key,
          write
        ).catch((error: unknown) => {
          // Refused or stopped, the write leaves its issue where it is, so the name ends now.
          release(key);
          throw error;
        });
        if (answer !== null) mergeIssue(client, answer);
        setMoved((current) => new Set([...current, key]));
        return key;
      }
    );
    const failed = results.flatMap((result, index) =>
      result.status === "rejected"
        ? [{ key: operations[index]?.key ?? "Unknown issue", error: result.reason as unknown }]
        : []
    );
    if (failed.length) {
      // A change the reader stopped in Pending changes may have been saved, so it is said apart
      // from a refused one, in the words of a stopped write; the error's own are a composer's.
      const refused: { error: unknown; key: string }[] = [];
      const stopped: { error: SendStoppedError; key: string }[] = [];
      for (const { error, key } of failed) {
        if (error instanceof SendStoppedError) stopped.push({ error, key });
        else refused.push({ error, key });
      }
      const refusedKeys = refused.map(({ key }) => key).join(", ");
      // The refusal in its own words: `String` of an error leads with its class's name.
      const why = refused[0]?.error;
      const unsaved =
        refused[0] === undefined
          ? ""
          : ` Changes not saved: ${refusedKeys}. ${why instanceof Error ? why.message : String(why)}`;
      // A change whose key Dispatch still holds is asked again under it, and one whose key ran out
      // (`SendExpiredError`) is a new write, so each group is said with its own Retry's promise.
      const notes = [
        stopped.filter(({ error }) => retriesUnderItsKey(error)),
        stopped.filter(({ error }) => !retriesUnderItsKey(error)),
      ].flatMap((group) => {
        const first = group[0];
        if (first === undefined) return [];
        const keys = group.map(({ key }) => key).join(", ");
        const what = group.length === 1 ? `the change to ${keys}` : `the change to each of ${keys}`;
        // The batch runs through `writes.run` with no hook, so no `KeyedWriteResult` is in hand
        // to read the method from; each issues-table change is this `PATCH`.
        return [` ${stoppedWriteNote(what, "saved", { method: "PATCH", stopped: first.error })}`];
      });
      const unconfirmed = notes.join("");
      const saved = `${operations.length - failed.length} of ${operations.length} changes saved.`;
      throw new Error(
        `${saved}${unsaved}${unconfirmed} Retry each failed change in Pending changes.`,
        { cause: (stopped[0] ?? refused[0])?.error }
      );
    }
    return operations.map(({ key }) => key);
  };
  /** Runs `action`, which the reader's press began and whose writes are for the issues `sends`
   *  as the table lists them: those are named as leaving from the press (`taking`). */
  const perform = async (
    said: string,
    sends: readonly string[],
    action: () => Promise<readonly string[] | undefined>,
    duplicate?: (error: ApiError) => void
  ) => {
    // The action's whole run keeps the armed hand-off of the reader's press that began it live
    // (`holdHandOff`), until its controls are let go. An action of several writes runs on after
    // its first writes are answered: Start next writes one step at a time, New epic moves the
    // issues after the create, and a batch sends `WRITES_IN_FLIGHT` writes at a time. Without
    // this hold the press could end before the action did, and the bar's going would read as no
    // doing of the reader's.
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
      if (error instanceof ApiError && error.code === "POSSIBLE_DUPLICATE" && duplicate)
        duplicate(error);
      else
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
  const titleOf = (key: string) => issues.find((issue) => issue.key === key)?.title ?? key;
  const named = (keys: readonly string[]) =>
    keys.length === 1 ? `${keys[0]} “${titleOf(keys[0] ?? "")}”` : `${keys.length} issues`;
  const openSelected = (keys: readonly string[]) =>
    keys.filter((key) => issues.some((issue) => issue.key === key && issue.status !== "done"));
  const skippedClosed = (count: number) =>
    count ? ` Skipped ${count} closed ${count === 1 ? "issue" : "issues"}.` : "";
  const canBegin = () => {
    if (!recovery.current.current) return true;
    setNotice({
      kind: "error",
      text: "Finish or Dismiss the interrupted action before starting another one.",
    });
    return false;
  };
  /** The table's rows of `keys`, as it last read them. */
  const listed = (keys: readonly string[]) => issues.filter((issue) => keys.includes(issue.key));
  /** Which of `rows` are in work, as the question says it: `CORE-1 “…” is in work`. */
  const inWorkOf = (rows: readonly IssueSummary[]) => {
    const working = rows
      .filter((issue) => IN_WORK_STATUSES.has(issue.status))
      .map(({ key }) => key);
    return `${named(working)} ${working.length === 1 ? "is" : "are"} in work`;
  };
  /** Sends the status change the reader said yes to: `epic` into work, with each parent above it
   *  that is not (`INTO_WORK`), before the moves it lets in. Its key is a step of the action `id`,
   *  so Finish waits on it, and where it was refused or stopped makes it afresh (`afreshSteps`). */
  const moveIntoWork = async (epic: string, id?: string) => {
    const write = newSendKey();
    recovery.record({ intoWork: write }, id);
    try {
      const answer = await request(
        statusChangeRequest(epic, INTO_WORK),
        `Move ${epic} into work from the issues table`,
        epic,
        write
      );
      if (answer !== null) mergeIssue(client, answer.issue);
    } catch (error) {
      // A change the reader stopped in Pending changes may have been made, which its words say in
      // place of the error's own, a composer's. Finish makes a step of an action again: it checks
      // the epic again and asks before it moves it. A Retry in Pending changes would send the move
      // past those checks.
      const what = `the move of ${epic} into work`;
      const why =
        error instanceof SendStoppedError
          ? id === undefined
            ? `${stoppedWriteNote(what, "made", { method: "POST", stopped: error })} Retry it in Pending changes.`
            : stoppedWriteNote(what, "made", {
                anew: `Finish the interrupted action: it checks ${epic} again and asks before it moves it.`,
              })
          : error instanceof Error
            ? error.message
            : String(error);
      throw new Error(`No issue moved to ${epic}. ${why}`, { cause: error });
    }
  };
  /**
   * The question `askTarget` asks before the moves of `rows` under the epic `target` (D12), from
   * the dry run of the epic's move into work (`INTO_WORK`): undefined where that finds the rule
   * needs no move. The dry run reads the epic as Dispatch holds it now, which the listing may be
   * behind. The question lists each change the dry run names; `action.lead` is said before its
   * own sentences.
   */
  const targetQuestion = async (
    target: string,
    rows: readonly IssueSummary[],
    action: TargetAction
  ): Promise<Question | undefined> => {
    const answer = await dryRunStatusChange(client, target, INTO_WORK);
    const changed = answer.changed ?? [];
    if (changed.length === 0 || !targetMustMove(answer.issue, rows)) return undefined;
    const { key, title } = answer.issue;
    const done = answer.issue.status === "done";
    const why = done
      ? `The epic ${key} “${title}” is done. A done epic holds no open issue.`
      : `${inWorkOf(rows)}, and the epic ${key} “${title}” is not. An epic stays in work while an issue under it is in work.`;
    return {
      title: done ? `Reopen ${key} too?` : `Move ${key} into work too?`,
      body: [action.lead, why, `Before the ${action.noun}, Dispatch makes these changes:`]
        .filter((sentence) => sentence !== undefined)
        .join(" "),
      // A dry run's entries carry no issue: the epic is the answer's own, and a parent above it
      // is named as the listing holds it, or by its key alone.
      changes: changed.map((entry) => {
        const known =
          entry.key === key ? title : issues.find((issue) => issue.key === entry.key)?.title;
        const issue = known === undefined ? entry.key : `${entry.key} “${known}”`;
        return `${issue} moves to ${statusText(entry.status)}`;
      }),
      confirmLabel: done ? "Reopen it too" : "Move into work too",
    };
  };
  /** What the table says where the dry run of `target`'s move into work was not answered. */
  const checkFailed = (target: string, error: unknown) =>
    `Nothing moved: Dispatch could not check ${target}. ${apiErrorMessage(error, "The page could not reach Dispatch.")}`;
  /**
   * Starts an action that moves the issues `moving` under the epic `target` (D12). Where Dispatch's
   * rule needs the epic in work first, the page sends the dry run of its move into work and asks
   * one question (`targetQuestion`): a yes is `go(true)`, which makes that status change before
   * the action's own writes, and a Cancel writes nothing. Where the rule needs nothing, as the
   * dry run finds the epic too, it is `go(false)` at once.
   */
  const askTarget = (
    target: string,
    moving: readonly string[],
    action: TargetAction,
    go: (intoWork: boolean) => void
  ) => {
    const rows = listed(moving);
    const held = issues.find((issue) => issue.key === target);
    // An epic the listing no longer holds may have closed since it was picked, which only the dry
    // run can tell: the table's open listing leaves a closed issue out.
    if (held === undefined ? rows.length === 0 : !targetMustMove(held, rows)) {
      go(false);
      return;
    }
    setBusy(true);
    setNotice(undefined);
    setTaking(new Set(moving));
    targetQuestion(target, rows, action).then(
      (question) => {
        setBusy(false);
        if (question === undefined) go(false);
        else setConfirming({ ...question, run: () => go(true) });
      },
      (error: unknown) => {
        setBusy(false);
        setNotice({ kind: "error", text: checkFailed(target, error) });
      }
    );
  };
  /** Puts `question` to the reader while a Finish of the action `id` holds the table busy, and
   *  answers their yes: false for a Cancel, or for the table going away while it asks. */
  const askOnFinish = async (id: string, question: Question): Promise<boolean> => {
    if (!recovery.active(id)) return false;
    const answer = Promise.withResolvers<boolean>();
    answering.current = answer.resolve;
    setConfirming({
      ...question,
      cancel: () => answer.resolve(false),
      run: () => answer.resolve(true),
    });
    const yes = await answer.promise;
    answering.current = undefined;
    return yes;
  };
  /** Whether a Finish of `record` goes on past its guard's question `question`, where there is
   *  one (`askOnFinish`): false for a Cancel, for the table going away while it asks, or for the
   *  action no longer in hand, with nothing written. Going on, it first drops the refused writes
   *  of the steps Finish decides afresh (`afreshSteps`) from Pending changes. */
  const goOnFinish = async (
    record: TableAction,
    question: Question | undefined
  ): Promise<boolean> => {
    if (question !== undefined && !(await askOnFinish(record.id, question))) return false;
    if (!recovery.active(record.id)) return false;
    recovery.discard(record.id, afreshSteps(record));
    return true;
  };
  /**
   * The guard's part for an epic that exists (`ResumableAction.guard`): whether a Finish of
   * `record` goes on to move `rows` under the epic `target`, which the listing holds as `epic`.
   * Where the listing shows the rule needs nothing, it goes on at once, with no dry run. Else it
   * sends the dry run of the epic's move into work, and where that finds a change, asks
   * `askTarget`'s question (`goOnFinish`): a Cancel, or the table going away while it asks, is
   * false, with nothing written. Going on, it first drops the refused writes of the steps Finish
   * decides afresh (`afreshSteps`), then on a yes makes the epic's move into work
   * (`moveIntoWork`) as a step of the action. Called by merge's guard and New epic's.
   */
  const intoWorkOnFinish = async (
    record: TableAction,
    target: string,
    epic: Pick<IssueSummary, "status"> | undefined,
    rows: readonly IssueSummary[],
    action: TargetAction
  ): Promise<boolean> => {
    let question: Question | undefined;
    if (targetMustMove(epic, rows)) {
      try {
        question = await targetQuestion(target, rows, action);
      } catch (error) {
        throw new Error(checkFailed(target, error), { cause: error });
      }
    }
    if (!(await goOnFinish(record, question))) return false;
    if (question !== undefined) await moveIntoWork(target, record.id);
    return true;
  };
  /** The question New epic asks before its create where an issue it moves is in work (D12), from
   *  the listing: the epic is no issue yet, so no dry run can name its changes. `name` is the
   *  epic as the question calls it. Its first run asks it, and its guard asks it again before a
   *  refused create goes again. */
  const newEpicQuestion = (name: string, rows: readonly IssueSummary[]): Question => ({
    title: `Move ${name} into work too?`,
    body: `${inWorkOf(rows)}, and a new epic is not. An epic stays in work while an issue under it is in work. Dispatch makes the epic and then makes this change:`,
    changes: [`${name} moves to ${statusText("in_progress")}`],
    confirmLabel: "Move into work too",
  });
  const moveTo = (keys: readonly string[], epic: string | null) => {
    const open = openSelected(keys);
    const skipped = skippedClosed(keys.length - open.length);
    if (!open.length) {
      // Refused with no write, the action is answered at once, as an action that writes is at the
      // end of `perform`: a hold on what answers the reader's press, begun and ended with the
      // refusal (`holdHandOff`). So Move, which its own press leaves held with no epic staged,
      // keeps that press live no longer, since a hold that only says there is nothing to do keeps
      // no press live, and a live update that then empties the bar is no doing of the reader's.
      const answered = holdHandOff();
      setNotice({ kind: "error", text: `Only open issues can change epic.${skipped}` });
      answered();
      return;
    }
    const said =
      (epic === null
        ? `Took ${named(open)} out of ${open.length === 1 ? "its epic" : "their epics"}.`
        : `Moved ${named(open)} to the epic “${titleOf(epic)}”.`) + skipped;
    const move = (intoWork: boolean) =>
      void perform(said, open, async () => {
        if (intoWork && epic !== null) await moveIntoWork(epic);
        return apply(open.map((key) => ({ key, input: { parent: epic } })));
      });
    if (epic === null) move(false);
    else askTarget(epic, open, { noun: "move" }, move);
  };
  /** The rank writes of a start-next action, sent in order, stopping the moment the reader
   *  dismisses it. One loop and one guard serve the first attempt and the resume; `failure`
   *  is how the attempt names what did not move. The answer says whether the action is still
   *  the one in hand when the last write landed. */
  const runStartNext = async (
    record: TableAction,
    current: readonly Item[],
    failure: (remaining: readonly string[], error: unknown) => string = (_remaining, error) =>
      error instanceof Error ? error.message : String(error)
  ) => {
    const operations = startNextWrites(current, record.keys);
    // Every step's key, minted and stored in one write before the first send: 250 issues move
    // up under one storage write rather than 250 read-modify-write cycles of the whole store.
    const minted: Keys = Object.fromEntries(operations.map(({ key }) => [key, newSendKey()]));
    recovery.record(minted, record.id);
    for (const [index, operation] of operations.entries()) {
      if (!recovery.active(record.id)) return false;
      try {
        await apply([operation], record.id, minted);
      } catch (error) {
        throw new Error(
          failure(
            operations.slice(index).map(({ key }) => key),
            error
          )
        );
      }
    }
    return recovery.active(record.id);
  };
  /** Sends a start-next action for queued issues already checked by `startNext`. */
  const runQueued = (queued: readonly string[]) => {
    if (!queued.length) {
      setNotice({
        kind: "error",
        text: "Only issues waiting in Legion's queue can start next.",
      });
      return;
    }
    if (!startNextWrites(items, queued).length) {
      setNotice({
        kind: "success",
        text: `${named(queued)} already ${queued.length === 1 ? "starts" : "start"} next.`,
      });
      return;
    }
    const record = recovery.begin({ kind: "startNext", keys: queued });
    void perform(
      `${named(queued)} now ${queued.length === 1 ? "starts" : "start"} next.`,
      queued,
      async () => {
        const ran = await runStartNext(record, items, (remaining, error) => {
          // One step goes at a time, so the first of `remaining` is the one `apply` wrote. One
          // the reader stopped may have moved up, which `apply`'s own words say, so it is not
          // listed as one that did not.
          const stopped = error instanceof Error && error.cause instanceof SendStoppedError;
          const unmoved = stopped ? remaining.slice(1) : remaining;
          const why = error instanceof Error ? error.message : String(error);
          return unmoved.length === 0 ? why : `Did not move up: ${unmoved.join(", ")}. ${why}`;
        });
        if (!ran) return undefined;
        recovery.dismiss();
        return queued;
      }
    );
  };
  /** A start-next the reader has not seen the whole of: `Select all N matching` picked rows
   *  off the page, and each one is a write, so the count is put to them first. */
  const startNext = (keys: readonly string[], confirmFirst = false) => {
    if (!canBegin()) return;
    const queued = queuedInOrder(items, keys);
    if (confirmFirst && queued.length) {
      setConfirming({
        title: "Start these next?",
        body: `${queued.length} issues move to the head of Legion's queue, one write each.`,
        confirmLabel: "Start next",
        run: () => runQueued(queued),
      });
      return;
    }
    runQueued(queued);
  };
  /** `title` is the name the reader gave, `force` makes the epic past a near copy Dispatch named,
   *  and `intoWork` is the reader's yes to the new epic's move into work, which the action asks
   *  for before its create where an issue it moves is in work (D12). */
  const newEpic = async (
    keys: readonly string[],
    {
      force = false,
      intoWork,
      title: chosenTitle,
    }: { readonly force?: boolean; readonly intoWork?: true; readonly title?: string } = {}
  ) => {
    if (project === undefined || unansweredEpic || !canBegin()) return;
    const open = openSelected(keys);
    const skipped = skippedClosed(keys.length - open.length);
    if (!open.length) {
      setNotice({
        kind: "error",
        text: `Select at least one open issue before creating an epic.${skipped}`,
      });
      return;
    }
    if (chosenTitle === undefined) {
      setNaming({ kind: "new", keys: open });
      return;
    }
    const title = chosenTitle.trim();
    if (!title) return;
    setNaming(undefined);
    // The duplicate's notice goes in the commit that marks the List busy (`perform`), so Create
    // anyway's own press never disables it under the focus: its going hands the focus on among
    // the notices (`IssuesTable`).
    setDuplicateEpic(undefined);
    const rows = listed(open);
    if (intoWork === undefined && targetMustMove(undefined, rows)) {
      setConfirming({
        ...newEpicQuestion(`“${title}”`, rows),
        run: () => void newEpic(keys, { force, intoWork: true, title }),
      });
      return;
    }
    // The title goes with the record: a Finish whose create was refused asks this run's question
    // again, before the epic has a key.
    const record = recovery.begin({ kind: "newEpic", keys: open, title });
    const key = newSendKey();
    recovery.record({ create: key }, record.id);
    await perform(
      `Made the epic “${title}” from ${named(open)}.${skipped}`,
      open,
      async () => {
        const creation = writes.run({
          kind: tableEpicWrite,
          detail: recoveryKey,
          key,
          label: `Create epic “${title}”`,
          presentation: "indicator",
          method: "POST",
          write: (bounds) =>
            api.createIssue(
              { project, title, labels: ["epic"], ...(force ? { force: true } : {}) },
              bounds
            ),
        });
        recovery.watch(key, `Create epic “${title}”`);
        // A create the reader stopped in Pending changes may have made the epic, which its words
        // say in place of the error's own, a composer's; its Retry there makes that very create
        // again under its key.
        const epic = await creation.catch((error: unknown) => {
          throw error instanceof SendStoppedError
            ? new Error(
                // The epic goes through `writes.run` with no hook, so no `KeyedWriteResult` is in
                // hand to read the method from; creating an epic is this `POST`.
                `${stoppedWriteNote(`the epic “${title}”`, "made", { method: "POST", stopped: error })} Retry it in Pending changes.`,
                { cause: error }
              )
            : error;
        });
        if (!recovery.active(record.id)) return undefined;
        if (epic === null) {
          setUnansweredEpic(title);
          return undefined;
        }
        mergeIssue(client, epic);
        if (intoWork) await moveIntoWork(epic.key, record.id);
        if (!recovery.active(record.id)) return undefined;
        const changed = await apply(
          open.map((key) => ({ key, input: { parent: epic.key } })),
          record.id
        );
        if (!recovery.active(record.id)) return undefined;
        recovery.dismiss();
        return changed;
      },
      (error) => {
        const refused = writes.rows().find((row) => row.key === key);
        if (refused) writes.discard(refused.id);
        recovery.dismiss();
        setDuplicateEpic({ keys, title, message: error.message, intoWork });
      }
    );
  };
  const renameEpic = (key: string, chosenTitle?: string) => {
    if (chosenTitle === undefined) {
      setNaming({ kind: "rename", key, title: titleOf(key) });
      return;
    }
    const title = chosenTitle.trim();
    setNaming(undefined);
    if (title && title !== titleOf(key))
      void perform(`Renamed the epic to “${title}”.`, [key], () =>
        apply([{ key, input: { title } }])
      );
  };
  /**
   * Refuses the close of a merge's source `source` while an issue the merge does not move is open
   * under it: the table reads its own project alone, and a child can be in another project.
   * `current` is the listing read just before the close. Its row of the source counts the source's
   * children in every project (`progress.children`), so only where that row shows one not done
   * does the page send the dry run of the source's close. Dispatch answers that dry run as the
   * rule is on, whatever its setting, and its `409 PARENT_STATUS_CONFLICT` names each issue that
   * holds the close: the refusal names each one with its project. A dry run that is not answered
   * refuses the close too. Either refusal comes before the merge's reason, with nothing written.
   * Its one caller is `closeMerge`, before its first write.
   */
  const refuseHeldClose = async (source: string, current: readonly IssueSummary[]) => {
    const row = current.find((issue) => issue.key === source);
    if (row === undefined || !cardHasOpenChild(row)) return;
    try {
      await dryRunStatusChange(client, source, { status: "done" });
    } catch (error) {
      if (!isParentStatusConflict(error))
        throw new Error(
          `The source epic ${source} remains open: Dispatch could not check the issues under it. ${apiErrorMessage(error, "The page could not reach Dispatch.")}`,
          { cause: error }
        );
      const held = (error.issues ?? []).map(
        ({ key, title }) => `${key} “${title}” (project ${issueProject(key)})`
      );
      throw new Error(
        `The source epic ${source} remains open: ${held.join(", ")} still ${held.length === 1 ? "needs" : "need"} to move.`,
        { cause: error }
      );
    }
  };
  /**
   * Closes the merge's source as the last step of the merge `record`, and writes nothing once the
   * action is no longer in hand. First it refuses a close that an open issue under the source
   * holds (`refuseHeldClose`), from the listing `current` that its caller read just before. Then
   * it posts the merge reason on the source, except on a resume (`resumed`) that finds the reason
   * already in the source's messages, and closes the source as done. Its callers are the merge's
   * first run (`mergeEpic`) and merge's `resume`.
   */
  const closeMerge = async (
    record: TableAction,
    resumed: boolean,
    current: readonly IssueSummary[]
  ) => {
    const from = record.source;
    const into = record.target;
    if (!from || !into || !recovery.active(record.id)) return;
    await refuseHeldClose(from, current);
    // Whether the reason was posted is the server's answer on a resume: a recorded key says
    // only that a write was made, and a write discarded in Pending changes leaves that key
    // behind. `settle` has already waited on any write of this action still in flight, so a
    // reason that landed is in the source's messages, compared as the whole sentence.
    const posted = recovery.current.current?.reasonBody;
    const prefix = `Merged into ${into} (`;
    const alreadySaid =
      resumed &&
      (await api.listIssueMessages(from)).some(({ message }) =>
        // The sentence the merge actually sent, kept with the record: the target may have been
        // renamed or closed since, which would make a freshly built sentence miss and post a
        // second one. A record from before this was kept falls back to the title-free opening.
        posted === undefined ? message.body.startsWith(prefix) : message.body === posted
      );
    if (!recovery.active(record.id)) return;
    if (!alreadySaid) {
      const reason = newSendKey();
      const body = mergeReason(into, titleOf(into));
      recovery.record({ reason }, record.id, { reasonBody: body });
      await request(
        {
          method: "POST",
          path: `/api/v1/issues/${encodeURIComponent(from)}/messages`,
          body: { body },
        },
        `Record the merge reason on ${from}`,
        from,
        reason
      );
    }
    if (recovery.active(record.id))
      await apply([{ key: from, input: { status: "done" } }], record.id);
  };
  /** What a merge of `from` into `into` does, moving `moving` open issues, said before any
   *  question it asks. */
  const mergeLead = (from: string, into: string, moving: number) =>
    `${moving} open ${moving === 1 ? "issue moves" : "issues move"} from ${from} “${titleOf(from)}” to ${into} “${titleOf(into)}”, and ${from} closes as done.`;
  /** What a merge is about to do, in the page, before any write: a merge closes an issue, so
   *  it is the one table action that asks whatever the input device. Where the target must go
   *  into work first, that question is the one the merge asks. */
  const askMerge = (from: string, into: string) => {
    if (!canBegin()) return;
    const members = mergeMembers(issues, from);
    const lead = mergeLead(from, into, members.length);
    askTarget(into, members, { lead, noun: "merge" }, (intoWork) => {
      if (intoWork) void mergeEpic(from, into, true);
      else
        setConfirming({
          title: "Merge this epic?",
          body: lead,
          confirmLabel: "Merge",
          run: () => void mergeEpic(from, into),
        });
    });
  };
  const mergeEpic = (from: string, into: string, intoWork = false) => {
    if (!canBegin()) return;
    // The issues under the source as the table lists them, and the source, which closes: the run
    // reads the listing afresh and names any other issue it moves as it sends its write (`apply`).
    const sends = [...mergeMembers(issues, from), from];
    return perform(`Merged “${titleOf(from)}” into “${titleOf(into)}”.`, sends, async () => {
      const descendants = await listAll();
      const members = mergeMembers(descendants, from);
      const record = recovery.begin({ kind: "merge", keys: members, source: from, target: into });
      try {
        if (intoWork) await moveIntoWork(into, record.id);
        await apply(
          members.map((key) => ({ key, input: { parent: into } })),
          record.id
        );
      } catch (error) {
        throw new Error(
          `The source epic ${from} remains open. ${error instanceof Error ? error.message : String(error)}`
        );
      }
      if (!recovery.active(record.id)) return undefined;
      await closeMerge(record, false, await listAll());
      if (!recovery.active(record.id)) return undefined;
      recovery.dismiss();
      return members;
    });
  };
  /** The issues a New epic still adopts: each of its open issues not yet under its epic. */
  const adoptionsOf = (record: TableAction, current: readonly IssueSummary[]) =>
    current.filter(
      (issue) =>
        record.keys.includes(issue.key) && issue.status !== "done" && issue.parent !== record.target
    );
  /**
   * New epic's guard (`ResumableAction.guard`). An epic the page knows the key of is refused where
   * the listing holds it gone or closed, and otherwise goes through `intoWorkOnFinish`. One whose
   * create has not landed has no key, so no dry run: where its refused create still stands in
   * Pending changes, the guard asks the first run's question (`newEpicQuestion`) from the listing
   * before the create goes again, and on a yes sends the create (`recovery.settle`), then moves the
   * new epic into work. A create the reader discarded, or one whose key never reached the page, is
   * refused.
   */
  const newEpicGuard = async (record: TableAction, current: readonly IssueSummary[]) => {
    const adopting = adoptionsOf(record, current);
    const unknownKey =
      "The created epic's key is still unknown. Retry its creation in Pending changes before finishing adoption.";
    if (!record.target) {
      if (record.writes.create === undefined)
        throw new Error(
          "The epic was never created: its creation was discarded. Nothing has been adopted. Dismiss this action and make the epic again."
        );
      if (!recovery.refused(record.id).includes("create")) throw new Error(unknownKey);
      const name = record.title === undefined ? "the new epic" : `“${record.title}”`;
      const question = targetMustMove(undefined, adopting)
        ? newEpicQuestion(name, adopting)
        : undefined;
      if (!(await goOnFinish(record, question))) return false;
      // With no issue in work, the create goes again with the action's other refused writes.
      if (question === undefined) return true;
      await recovery.settle(record, (step) => step === "create");
      if (!recovery.active(record.id)) return false;
      const made = recovery.current.current?.target;
      if (made === undefined) throw new Error(unknownKey);
      await moveIntoWork(made, record.id);
      return true;
    }
    const epic = current.find((issue) => issue.key === record.target);
    if (!epic || epic.status === "done")
      throw new Error(
        `The created epic ${record.target} is unavailable or closed. The selected issues have not been adopted.`
      );
    return intoWorkOnFinish(record, epic.key, epic, adopting, { noun: "move" });
  };
  /**
   * Merge's guard (`ResumableAction.guard`). The moves a merge still makes are the refused moves
   * Finish sends again and, while the source is open, the move of each issue left under it
   * (`mergeMembers`). A target done or gone while one of those needs it in work is refused, as
   * New epic's guard refuses a closed epic, rather than reopened unasked; one out of work goes
   * through `intoWorkOnFinish`, with the merge's own question.
   */
  const mergeGuard = async (record: TableAction, current: readonly IssueSummary[]) => {
    const source = record.source ?? "";
    const into = record.target ?? "";
    const left = sourceClosed(current, source) ? [] : mergeMembers(current, source);
    const resends = resendsStep(record);
    const resent = recovery.refused(record.id).filter((step) => step !== "reason" && resends(step));
    const wanted = new Set([...left, ...resent]);
    const moving = current.filter((issue) => issue.status !== "done" && wanted.has(issue.key));
    const target = current.find((issue) => issue.key === into);
    if (targetMustMove(target, moving) && (target === undefined || target.status === "done"))
      throw new Error(
        `The target epic ${into} is unavailable or closed. The source epic ${source} remains open, and its issues have not moved to it.`
      );
    return intoWorkOnFinish(record, into, target, moving, {
      lead: mergeLead(source, into, moving.length),
      noun: "merge",
    });
  };

  const kinds: Record<TableAction["kind"], ResumableAction> = {
    newEpic: {
      describe: (record) =>
        `New epic${record.target ? ` ${record.target}` : ""}: ${record.keys.join(", ")} are not yet adopted. Finish checks which open issues still need to move.`,
      guard: newEpicGuard,
      resume: async (record, current) => {
        if (!(await newEpicGuard(record, current))) return false;
        // Finish reads the record once the create has gone again, so by here the guard has
        // refused an epic with no key, and `record.target` names the epic.
        await apply(
          adoptionsOf(record, current).map((issue) => ({
            key: issue.key,
            input: { parent: record.target },
          })),
          record.id
        );
        return true;
      },
    },
    startNext: {
      describe: (record) =>
        `Start next: ${record.keys.join(", ")} may still need to move up, in that order.`,
      // Its rank writes change no parent and no status, so it has no guard: Finish sends them
      // again unasked.
      guard: null,
      resume: async (record, current) => {
        await runStartNext(
          record,
          buildItems(current, () => "")
        );
        return true;
      },
    },
    merge: {
      describe: (record) =>
        `Merge ${record.source} into ${record.target}: remaining open issues, the merge reason and closing the source still need to be checked.`,
      guard: mergeGuard,
      resume: async (record, current) => {
        const source = record.source ?? "";
        // A source closed since leaves no move to make and no guard to run, so the refused writes
        // of the steps Finish decides afresh are dropped here, as the guard drops them where it
        // answers true: a Retry of the move into work would need no merge.
        if (sourceClosed(current, source)) {
          recovery.discard(record.id, afreshSteps(record));
          return true;
        }
        if (!(await mergeGuard(record, current))) return false;
        const members = mergeMembers(current, source);
        await apply(
          members.map((key) => ({ key, input: { parent: record.target } })),
          record.id
        );
        if (!recovery.active(record.id)) return true;
        const latest = await listAll();
        const remaining = mergeMembers(latest, source);
        if (remaining.length)
          throw new Error(
            `The source epic ${source} remains open: ${remaining.join(", ")} still need to move.`
          );
        await closeMerge(record, true, latest);
        return true;
      },
    },
  };
  /**
   * Finishes the interrupted action. It waits on each write of the action still in flight and
   * sends none again, then runs the kind's guard, where it has one, before the first write it
   * sends: before the refused writes it sends again, where there are any, so that a Cancel writes
   * nothing at all, and in `resume`, before resume's own writes, on the listing read after those
   * sent again. A step the guard decides afresh (`afreshSteps`) is never sent again.
   */
  const finish = () => {
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
    void perform("Finished the interrupted action.", interrupted.keys, async () => {
      await recovery.settle(interrupted, () => false);
      // The record while it is still the one in hand, as the writes waited on left it: a create
      // that landed names its epic.
      const held = () => (recovery.active(interrupted.id) ? recovery.current.current : undefined);
      const record = held();
      if (!record) return undefined;
      const kind = kinds[record.kind];
      const resend = resendsStep(record);
      // Where no refused write goes again, the first write Finish sends is resume's own, which
      // resume's guard stands before. A Cancel of the question a guard asks leaves the action to
      // be finished, and writes nothing. A kind with no guard reads no listing here.
      if (kind.guard !== null && recovery.refused(record.id).some(resend)) {
        const current = await listAll();
        if (!held() || !(await kind.guard(record, current))) return undefined;
      }
      const guarded = held();
      if (!guarded) return undefined;
      await recovery.settle(guarded, resend);
      const settled = held();
      if (!settled) return undefined;
      const current = await listAll();
      if (!held() || !(await kind.resume(settled, current))) return undefined;
      if (!held()) return undefined;
      recovery.dismiss();
      setUnansweredEpic(undefined);
      return record.keys;
    }).finally(unwatch);
  };
  const interrupted = busy ? undefined : recovery.action;
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
    // Whether the press of this action is taking row `key` out of the view: an issue the action's
    // writes are for (`taking`), from the reader's press, not from its write's answer, so a row is
    // named while its write is still out, as the bar's hand-off and the List's anchor read it. The
    // name ends at once where that write is refused or stopped, and for every row once the action
    // is over (`busy`) and the list read after it has settled (`unsettled`): the commit that lets
    // the action go draws that read at the latest, and a read that outlived the hold is drawn in
    // the commit that ends `unsettled`, so by then each row the press took away has left the
    // list and each row still listed stays. The List's anchor asks the render before a commit as
    // well (`ViewportAnchor`'s `leaving`), so the rows are named in that commit. A row a later live
    // update takes away is not named. With no action out, `leaving` is undefined and names no row.
    leaving: busy || unsettled !== undefined ? (key: string) => taking.has(key) : undefined,
    duplicateEpic,
    setDuplicateEpic,
    unansweredEpic,
    dismissUnansweredEpic: () => {
      setUnansweredEpic(undefined);
      recovery.dismiss();
    },
    interrupted,
    interruptedDescription: interrupted && kinds[interrupted.kind].describe(interrupted),
    naming,
    cancelNaming: () => setNaming(undefined),
    confirming,
    // A Cancel tells a Finish that waits on the answer (`askOnFinish`).
    cancelConfirming: () => {
      const waiting = confirming;
      setConfirming(undefined);
      waiting?.cancel?.();
    },
    confirmed: () => {
      const waiting = confirming;
      setConfirming(undefined);
      waiting?.run();
    },
    finish,
    dismiss: recovery.dismiss,
    moveTo,
    startNext,
    renameEpic,
    // An epic belongs to a project, so a table that stands on none offers neither action: the
    // controls draw nothing where they are given nothing.
    newEpic: project === undefined ? undefined : newEpic,
    mergeEpic: project === undefined ? undefined : askMerge,
  };
}
