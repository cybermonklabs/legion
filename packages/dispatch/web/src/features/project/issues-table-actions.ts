import { useState } from "react";
import { useCache, useWrites } from "../../api/cache/cache";
import { mergeIssue } from "../../api/cache/issue-cache";
import { plainWrite } from "../../api/cache/write-store";
import {
  ApiError,
  api,
  issueCreateRequest,
  issuePatchRequest,
  type WriteRequest,
} from "../../api/client";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { settledWithLimit } from "../../lib/settled";
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
}
/** An action waiting on the reader's yes, with everything the dialog says and does. */
interface Confirmation {
  title: string;
  body: string;
  confirmLabel: string;
  run: () => void;
}
/** The epic whose name the reader is typing: a new one for the selected issues, or a rename. */
export type EpicNaming =
  | { kind: "new"; keys: readonly string[] }
  | { kind: "rename"; key: string; title: string };

/** The one sentence a merge records on the source issue, written and recognised in one place.
 *  A resume compares the whole sentence against the source's own messages, because the server
 *  is what knows whether it was posted: a recorded key only says a write was made. */
const mergeReason = (into: string, title: string) =>
  `Merged into ${into} (“${title}”) from the issues table.`;

/** What an interrupted action of each kind says it left behind, and how it is finished. The
 *  record's kind union is the only list of kinds: a new one fails to compile until both are
 *  written for it. */
interface ResumableAction {
  describe: (record: TableAction) => string;
  resume: (record: TableAction, current: readonly IssueSummary[]) => Promise<void>;
}

export function useIssueTableActions({
  project,
  issues,
  items,
  clearSelection,
}: {
  project: string;
  issues: readonly IssueSummary[];
  items: readonly Item[];
  clearSelection: () => void;
}) {
  const cache = useCache();
  const writes = useWrites();
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<Notice>();
  const [moved, setMoved] = useState<ReadonlySet<string>>(new Set());
  const [duplicateEpic, setDuplicateEpic] = useState<DuplicateEpic>();
  const [unansweredEpic, setUnansweredEpic] = useState<string>();
  const [naming, setNaming] = useState<EpicNaming>();
  const [confirming, setConfirming] = useState<Confirmation>();
  const recovery = useTableRecovery(project, (text) =>
    setNotice({ kind: text.startsWith("Changes not saved") ? "error" : "success", text })
  );

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
    const results = await settledWithLimit(
      sends,
      WRITES_IN_FLIGHT,
      async ({ key, input, write }) => {
        const answer = await request(
          issuePatchRequest(key, input),
          `Update ${key} from the issues table`,
          key,
          write
        );
        if (answer !== null) mergeIssue(cache, answer);
        setMoved((current) => new Set([...current, key]));
        return key;
      }
    );
    const failed = results.flatMap((result, index) =>
      result.status === "rejected"
        ? [{ key: operations[index]?.key ?? "Unknown issue", error: result.reason }]
        : []
    );
    if (failed.length)
      throw new Error(
        `${operations.length - failed.length} of ${operations.length} changes saved. Changes not saved: ${failed.map(({ key }) => key).join(", ")}. ${String(failed[0]?.error)} Retry each failed change in Pending changes.`
      );
    return operations.map(({ key }) => key);
  };
  const perform = async (
    said: string,
    action: () => Promise<readonly string[] | undefined>,
    duplicate?: (error: ApiError) => void
  ) => {
    setBusy(true);
    setNotice(undefined);
    setMoved(new Set());
    try {
      const changed = await action();
      if (changed !== undefined) {
        setNotice({ kind: "success", text: said });
        setMoved(new Set(changed));
        clearSelection();
      }
    } catch (error) {
      if (error instanceof ApiError && error.code === "POSSIBLE_DUPLICATE" && duplicate)
        duplicate(error);
      else
        setNotice({ kind: "error", text: error instanceof Error ? error.message : String(error) });
    } finally {
      setBusy(false);
      void cache.invalidate({ queryKey: ["issues"] });
      void cache.invalidate({ queryKey: ["issue"] });
      void cache.invalidate({ queryKey: ["inbox"] });
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
  const moveTo = (keys: readonly string[], epic: string | null) => {
    const open = openSelected(keys);
    const skipped = skippedClosed(keys.length - open.length);
    if (!open.length) {
      setNotice({ kind: "error", text: `Only open issues can change epic.${skipped}` });
      return;
    }
    const said =
      (epic === null
        ? `Took ${named(open)} out of ${open.length === 1 ? "its epic" : "their epics"}.`
        : `Moved ${named(open)} to the epic “${titleOf(epic)}”.`) + skipped;
    void perform(said, () => apply(open.map((key) => ({ key, input: { parent: epic } }))));
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
      async () => {
        const ran = await runStartNext(
          record,
          items,
          (remaining, error) =>
            `Did not move up: ${remaining.join(", ")}. ${error instanceof Error ? error.message : String(error)}`
        );
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
  const newEpic = async (keys: readonly string[], chosenTitle?: string, force = false) => {
    if (unansweredEpic || !canBegin()) return;
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
    setDuplicateEpic(undefined);
    const record = recovery.begin({ kind: "newEpic", keys: open });
    const key = newSendKey();
    recovery.record({ create: key }, record.id);
    /** The id of the create this press makes, once `run` has joined it. */
    let epicWrite: number | undefined;
    await perform(
      `Made the epic “${title}” from ${named(open)}.${skipped}`,
      async () => {
        const creation = writes.run({
          kind: tableEpicWrite,
          detail: project,
          key,
          label: `Create epic “${title}”`,
          presentation: "indicator",
          request: issueCreateRequest({
            project,
            title,
            labels: ["epic"],
            ...(force ? { force: true } : {}),
          }),
          target: `project:${project}`,
        });
        recovery.watch(key, `Create epic “${title}”`);
        // The write's id, read as `run` has just left it in the rows: a refusal below takes this
        // write out by it, whatever the rows hold by the time that refusal is handled.
        epicWrite = writes.idOf(key);
        const epic = await creation;
        if (!recovery.active(record.id)) return undefined;
        if (epic === null) {
          setUnansweredEpic(title);
          return undefined;
        }
        mergeIssue(cache, epic);
        const changed = await apply(
          open.map((key) => ({ key, input: { parent: epic.key } })),
          record.id
        );
        if (!recovery.active(record.id)) return undefined;
        recovery.dismiss();
        return changed;
      },
      (error) => {
        // Dispatch's "possible duplicate" refusal of the create, which the reader answers in the
        // dialog it opens rather than in Pending changes.
        if (epicWrite !== undefined) writes.discard(epicWrite);
        recovery.dismiss();
        setDuplicateEpic({ keys, title, message: error.message });
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
      void perform(`Renamed the epic to “${title}”.`, () => apply([{ key, input: { title } }]));
  };
  const closeMerge = async (record: TableAction, resumed: boolean) => {
    const from = record.source;
    const into = record.target;
    if (!from || !into || !recovery.active(record.id)) return;
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
  /** What a merge is about to do, in the page, before any write: a merge closes an issue, so
   *  it is the one table action that asks whatever the input device. */
  const askMerge = (from: string, into: string) => {
    if (!canBegin()) return;
    const moving = mergeMembers(issues, from).length;
    setConfirming({
      title: "Merge this epic?",
      body: `${moving} open ${moving === 1 ? "issue moves" : "issues move"} from ${from} “${titleOf(from)}” to ${into} “${titleOf(into)}”, and ${from} closes as done.`,
      confirmLabel: "Merge",
      run: () => void mergeEpic(from, into),
    });
  };
  const mergeEpic = (from: string, into: string) => {
    if (!canBegin()) return;
    return perform(`Merged “${titleOf(from)}” into “${titleOf(into)}”.`, async () => {
      const descendants = await api.listIssues({ project });
      const members = mergeMembers(descendants, from);
      const record = recovery.begin({ kind: "merge", keys: members, source: from, target: into });
      try {
        await apply(
          members.map((key) => ({ key, input: { parent: into } })),
          record.id
        );
      } catch (error) {
        throw new Error(
          `The source epic ${from} remains open. ${error instanceof Error ? error.message : String(error)}`
        );
      }
      await closeMerge(record, false);
      if (!recovery.active(record.id)) return undefined;
      recovery.dismiss();
      return members;
    });
  };

  const kinds: Record<TableAction["kind"], ResumableAction> = {
    newEpic: {
      describe: (record) =>
        `New epic${record.target ? ` ${record.target}` : ""}: ${record.keys.join(", ")} are not yet adopted. Finish checks which open issues still need to move.`,
      resume: async (record, current) => {
        // A creation still in Pending changes can be retried; one the reader discarded is gone
        // with its step, so there is no epic and nothing to adopt — Finish says which it is.
        if (!record.target)
          throw new Error(
            record.writes.create === undefined
              ? "The epic was never created: its creation was discarded. Nothing has been adopted. Dismiss this action and make the epic again."
              : "The created epic's key is still unknown. Retry its creation in Pending changes before finishing adoption."
          );
        const target = current.find((issue) => issue.key === record.target);
        if (!target || target.status === "done")
          throw new Error(
            `The created epic ${record.target} is unavailable or closed. The selected issues have not been adopted.`
          );
        await apply(
          current
            .filter(
              (issue) =>
                record.keys.includes(issue.key) &&
                issue.status !== "done" &&
                issue.parent !== record.target
            )
            .map((issue) => ({ key: issue.key, input: { parent: record.target } })),
          record.id
        );
      },
    },
    startNext: {
      describe: (record) =>
        `Start next: ${record.keys.join(", ")} may still need to move up, in that order.`,
      resume: async (record, current) => {
        await runStartNext(
          record,
          buildItems(current, () => "")
        );
      },
    },
    merge: {
      describe: (record) =>
        `Merge ${record.source} into ${record.target}: remaining open issues, the merge reason and closing the source still need to be checked.`,
      resume: async (record, current) => {
        if (current.find((issue) => issue.key === record.source)?.status === "done") return;
        const members = mergeMembers(current, record.source ?? "");
        await apply(
          members.map((key) => ({ key, input: { parent: record.target } })),
          record.id
        );
        if (!recovery.active(record.id)) return;
        const remaining = mergeMembers(await api.listIssues({ project }), record.source ?? "");
        if (remaining.length)
          throw new Error(
            `The source epic ${record.source} remains open: ${remaining.join(", ")} still need to move.`
          );
        await closeMerge(record, true);
      },
    },
  };
  const finish = () => {
    const interrupted = recovery.current.current;
    if (!interrupted) return;
    void perform("Finished the interrupted action.", async () => {
      await recovery.settle(interrupted);
      if (!recovery.active(interrupted.id)) return undefined;
      const record = recovery.current.current;
      if (!record) return undefined;
      const current = await api.listIssues({ project });
      if (!recovery.active(record.id)) return undefined;
      await kinds[record.kind].resume(record, current);
      if (!recovery.active(record.id)) return undefined;
      recovery.dismiss();
      setUnansweredEpic(undefined);
      return record.keys;
    });
  };
  const interrupted = busy ? undefined : recovery.action;
  return {
    busy,
    notice,
    moved,
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
    cancelConfirming: () => setConfirming(undefined),
    confirmed: () => {
      const waiting = confirming;
      setConfirming(undefined);
      waiting?.run();
    },
    finish,
    dismiss: recovery.dismiss,
    moveTo,
    startNext,
    newEpic,
    renameEpic,
    mergeEpic: askMerge,
  };
}
