import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { ApiError, api, issuePatchRequest, type WriteRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { plainWrite, useWrites } from "../writes/write-store";
import {
  buildItems,
  type Item,
  mergeMembers,
  queuedInOrder,
  startNextWrites,
} from "./issues-table-model";
import { type TableAction, tableEpicWrite, useTableRecovery } from "./issues-table-recovery";

interface Patch {
  key: string;
  input: UpdateIssueInput;
}
interface Notice {
  kind: "success" | "error";
  text: string;
}
interface DuplicateEpic {
  keys: readonly string[];
  title: string;
  message: string;
}
/** The epic whose name the reader is typing: a new one for the selected issues, or a rename. */
export type EpicNaming =
  | { kind: "new"; keys: readonly string[] }
  | { kind: "rename"; key: string; title: string };

/** How many writes of one bulk action are in flight at once. A selection is as large as the
 *  project, and "Select all N matching" is two clicks away, so the table sends them in a
 *  bounded stream rather than all at once; each write is still independent in the store. */
const WRITES_IN_FLIGHT = 8;

/** `Promise.allSettled` over `items`, at most `limit` of them running at a time, answers in the
 *  order the items came in. */
async function settledWithLimit<T, R>(
  items: readonly T[],
  limit: number,
  run: (item: T) => Promise<R>
): Promise<PromiseSettledResult<R>[]> {
  const results: PromiseSettledResult<R>[] = Array.from({ length: items.length });
  let next = 0;
  await Promise.all(
    Array.from({ length: Math.min(limit, items.length) }, async () => {
      for (let index = next++; index < items.length; index = next++) {
        const item = items[index] as T;
        try {
          results[index] = { status: "fulfilled", value: await run(item) };
        } catch (reason) {
          results[index] = { status: "rejected", reason };
        }
      }
    })
  );
  return results;
}

const mergeReason = (into: string, title: string) =>
  `Merged into ${into} (“${title}”) from the issues table.`;

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
  const client = useQueryClient();
  const writes = useWrites();
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<Notice>();
  const [moved, setMoved] = useState<ReadonlySet<string>>(new Set());
  const [duplicateEpic, setDuplicateEpic] = useState<DuplicateEpic>();
  const [unansweredEpic, setUnansweredEpic] = useState<string>();
  const [naming, setNaming] = useState<EpicNaming>();
  const recovery = useTableRecovery(project, (text) =>
    setNotice({ kind: text.startsWith("Changes not saved") ? "error" : "success", text })
  );

  const request = <T>(
    value: WriteRequest<T>,
    label: string,
    issue: string,
    id?: string,
    step = issue
  ) => {
    const key = recovery.key(step, id);
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
  const apply = async (operations: readonly Patch[], id?: string) => {
    const results = await settledWithLimit(operations, WRITES_IN_FLIGHT, async ({ key, input }) => {
      const answer = await request(
        issuePatchRequest(key, input),
        `Update ${key} from the issues table`,
        key,
        id
      );
      if (answer !== null) mergeIssue(client, answer);
      setMoved((current) => new Set([...current, key]));
      return key;
    });
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
      void client.invalidateQueries({ queryKey: ["issues"] });
      void client.invalidateQueries({ queryKey: ["issue"] });
      void client.invalidateQueries({ queryKey: ["inbox"] });
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
  const startNext = (keys: readonly string[]) => {
    if (!canBegin()) return;
    const queued = queuedInOrder(items, keys);
    if (!queued.length) {
      setNotice({
        kind: "error",
        text: "Only issues waiting in Legion's queue can start next.",
      });
      return;
    }
    const operations = startNextWrites(items, queued);
    if (!operations.length) {
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
        for (const [index, operation] of operations.entries()) {
          if (!recovery.active(record.id)) return undefined;
          try {
            await apply([operation], record.id);
          } catch (error) {
            throw new Error(
              `Did not move up: ${operations
                .slice(index)
                .map(({ key }) => key)
                .join(", ")}. ${error instanceof Error ? error.message : String(error)}`
            );
          }
        }
        if (!recovery.active(record.id)) return undefined;
        recovery.dismiss();
        return queued;
      }
    );
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
    const key = recovery.key("create", record.id);
    await perform(
      `Made the epic “${title}” from ${named(open)}.${skipped}`,
      async () => {
        const creation = writes.run({
          kind: tableEpicWrite,
          detail: project,
          key,
          label: `Create epic “${title}”`,
          presentation: "indicator",
          write: (bounds) =>
            api.createIssue(
              { project, title, labels: ["epic"], ...(force ? { force: true } : {}) },
              bounds
            ),
        });
        recovery.watch(key, `Create epic “${title}”`);
        const epic = await creation;
        if (!recovery.active(record.id)) return undefined;
        if (epic === null) {
          setUnansweredEpic(title);
          return undefined;
        }
        mergeIssue(client, epic);
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
    // The reason write's own key says whether it was sent: a sentence anyone may comment does
    // not. The text check is the second answer, for a record that lost its key, and it compares
    // the whole sentence the writer builds.
    const saved = recovery.current.current;
    const alreadySaid =
      resumed &&
      (saved?.writes.reason !== undefined ||
        (await api.listIssueMessages(from)).some(
          ({ message }) => message.body === mergeReason(into, titleOf(into))
        ));
    if (!recovery.active(record.id)) return;
    if (!alreadySaid)
      await request(
        {
          method: "POST",
          path: `/api/v1/issues/${encodeURIComponent(from)}/messages`,
          body: { body: mergeReason(into, titleOf(into)) },
        },
        `Record the merge reason on ${from}`,
        from,
        record.id,
        "reason"
      );
    if (recovery.active(record.id))
      await apply([{ key: from, input: { status: "done" } }], record.id);
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
        if (!record.target)
          throw new Error(
            "The created epic's key is still unknown. Retry its creation in Pending changes before finishing adoption."
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
        for (const operation of startNextWrites(
          buildItems(current, () => ""),
          record.keys
        )) {
          if (!recovery.active(record.id)) return;
          await apply([operation], record.id);
        }
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
    setUnansweredEpic: (_value: undefined) => {
      setUnansweredEpic(undefined);
      recovery.dismiss();
    },
    interrupted,
    interruptedDescription: interrupted && kinds[interrupted.kind].describe(interrupted),
    naming,
    cancelNaming: () => setNaming(undefined),
    finish,
    dismiss: recovery.dismiss,
    moveTo,
    startNext,
    newEpic,
    renameEpic,
    mergeEpic,
  };
}
