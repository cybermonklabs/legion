import { useCallback, useEffect, useRef, useState } from "react";
import { useWrites } from "../../api/cache/cache";
import { defineWriteKind, type WriteOutcome } from "../../api/cache/write-store";
import { ApiError } from "../../api/client";
import { projectIssuesQueryKey } from "../../api/queries";
import { newSendKey } from "../../lib/send-key";
import { settledWithLimit } from "../../lib/settled";
import { WRITES_IN_FLIGHT } from "./issues-table-model";
import { readTableAction, type TableAction, writeTableAction } from "./issues-table-storage";

export type { TableAction } from "./issues-table-storage";

/** Dispatch's answer to an epic's create, as far as its kind reads it: the issue it made, read
 *  through the answer's `body`. */
interface CreatedEpic {
  readonly body: { readonly key: string };
}

// Keep the answer's issue key even when the table that started creation is no longer mounted.
export const tableEpicWrite = defineWriteKind<string, string, CreatedEpic>({
  name: "issues-table-epic",
  kept: (value): value is string => typeof value === "string",
  landed: (cache, write, answer, awaited) => {
    const action = readTableAction(write.detail);
    if (action?.writes.create === write.key && answer !== null) {
      writeTableAction(write.detail, { ...action, target: answer.body.key });
    }
    // What a created epic changes is this project's listings and the ancestry read that hangs
    // off them, not every query the tab holds.
    if (!awaited || answer === null) {
      void cache.invalidate({ queryKey: projectIssuesQueryKey(write.detail, []) });
      void cache.invalidate({ queryKey: ["issue-ancestors", write.detail] });
    }
  },
});

/** What the table says once the last of its refused writes leaves Pending changes, by what became
 *  of it; `ofAction` says whether it was a step of the interrupted action, which Finish
 *  completes. */
function outcomeReport(kind: Exclude<WriteOutcome["kind"], "refused">, ofAction: boolean): string {
  switch (kind) {
    case "landed":
      return ofAction
        ? "The retried changes are saved. Finish the interrupted action to complete its remaining steps."
        : "All changes saved.";
    case "discarded":
      return "The failed change was discarded.";
    case "taken":
      return "The failed change was not made.";
  }
}

export function useTableRecovery(project: string, report: (text: string) => void) {
  const writes = useWrites();
  const [action, setAction] = useState(() => readTableAction(project));
  const current = useRef(action);
  const alive = useRef(true);
  const reportRef = useRef(report);
  reportRef.current = report;
  const owned = useRef(new Map<number, { issue: string; step?: string; action?: string }>());
  const failed = useRef(new Map<number, string>());
  const remember = useCallback(
    (next: TableAction | undefined) => {
      writeTableAction(project, next);
      current.current = next;
      if (alive.current) setAction(next);
    },
    [project]
  );
  useEffect(() => {
    alive.current = true;
    current.current = readTableAction(project);
    setAction(current.current);
    const observeRows = () => {
      let record = current.current;
      if (!record) return;
      const steps = new Map(Object.entries(record.writes).map(([step, key]) => [key, step]));
      for (const row of writes.rows()) {
        const step: string | undefined =
          steps.get(row.key) ??
          (owned.current.get(row.id)?.action === record.id
            ? owned.current.get(row.id)?.step
            : undefined);
        if (!step) continue;
        owned.current.set(row.id, { issue: row.label, step, action: record.id });
        if (record.writes[step] !== row.key) {
          record = { ...record, writes: { ...record.writes, [step]: row.key } };
          remember(record);
        }
      }
    };
    const observe = writes.subscribe(observeRows);
    observeRows();
    const outcome = writes.onOutcome((event) => {
      if (!owned.current.has(event.write.id)) return;
      const record = readTableAction(project);
      if (record?.id === current.current?.id) {
        current.current = record;
        setAction(record);
      }
      if (event.kind === "refused") {
        if (!(event.refusal instanceof ApiError && event.refusal.code === "POSSIBLE_DUPLICATE"))
          failed.current.set(
            event.write.id,
            owned.current.get(event.write.id)?.issue ?? event.write.label
          );
        return;
      }
      if (failed.current.delete(event.write.id)) {
        // Only a write of the interrupted action's own says anything about finishing it: a
        // Move or a Rename retried beside one reports like any other change.
        const ofAction =
          current.current !== undefined &&
          owned.current.get(event.write.id)?.action === current.current.id;
        reportRef.current(
          failed.current.size
            ? `Changes not saved: ${[...failed.current.values()].join(", ")}. Retry them in Pending changes.`
            : outcomeReport(event.kind, ofAction)
        );
      }
      if (event.kind !== "landed") {
        // A write discarded or let go of no longer exists, so the record must not go on
        // claiming it: the step goes with it, which keeps `settle` from waiting on a row that is
        // gone and lets the resume work that step out from the server again.
        const owner = owned.current.get(event.write.id);
        const held = current.current;
        if (
          owner?.step &&
          held &&
          owner.action === held.id &&
          held.writes[owner.step] !== undefined
        ) {
          const { [owner.step]: _gone, ...rest } = held.writes;
          remember({ ...held, writes: rest });
        }
      }
      owned.current.delete(event.write.id);
    });
    return () => {
      alive.current = false;
      observe();
      outcome();
    };
  }, [project, writes, remember]);

  const active = (id: string) => alive.current && current.current?.id === id;
  return {
    action,
    current,
    active,
    begin: (record: Omit<TableAction, "id" | "project" | "writes" | "startedAt">) => {
      const next = { ...record, id: newSendKey(), project, startedAt: Date.now(), writes: {} };
      remember(next);
      return next;
    },
    /** The keys of the writes a step of `id`'s action is about to send, kept with the record so
     *  a resume can wait on them. One storage write and one render for the whole batch: a
     *  250-row action records its keys once, not once per issue. */
    record: (
      steps: Readonly<Record<string, string>>,
      id?: string,
      fields?: Partial<TableAction>
    ) => {
      const held = current.current;
      if (id && held?.id === id && (Object.keys(steps).length || fields))
        remember({ ...held, ...fields, writes: { ...held.writes, ...steps } });
    },
    watch: (key: string, issue: string) => {
      const row = writes.rows().find((value) => value.key === key);
      if (row) owned.current.set(row.id, { ...owned.current.get(row.id), issue });
    },
    settle: async (record: TableAction) => {
      const rows = writes.rows().filter((row) => Object.values(record.writes).includes(row.key));
      /** What becomes of `row`, told by the store for every way a write leaves it: a write it
       *  let go of with nothing made - a sign-out landing, a sign-out in another tab, another
       *  login taking over, a `DISPATCH_LOGIN_CHANGED` refusal (`taken`) - says it was not made,
       *  rather than leaving Finish waiting for an answer that never comes. Its id, not its key,
       *  names it: a Retry may move it to a new key. */
      const outcome = (row: (typeof rows)[number]) => {
        const { promise, resolve, reject } = Promise.withResolvers<void>();
        const stop = writes.onOutcome((event) => {
          if (event.write.id !== row.id) return;
          stop();
          switch (event.kind) {
            case "landed":
              resolve();
              return;
            case "refused":
              reject(event.refusal);
              return;
            case "discarded":
              reject(new Error(`${row.label} was discarded.`));
              return;
            case "taken":
              reject(new Error(`${row.label} was not made.`));
              return;
            default:
              event satisfies never;
          }
        });
        return promise;
      };
      // A write still in flight is only waited on; a refused one is sent again, and no more of
      // those run at once than a bulk action sends, so Finish after a 250-row refusal is the
      // same bounded stream the action itself was.
      const waiting = rows.filter((row) => row.status !== "refused").map(outcome);
      const retried = settledWithLimit(
        rows.filter((row) => row.status === "refused"),
        WRITES_IN_FLIGHT,
        async (row) => {
          // Between the read above and this row's turn the reader may have retried it in
          // Pending changes, and a write already on its way answers no second retry: asking
          // for one would leave Finish waiting for an outcome that never comes.
          const now = writes.rows().find((current) => current.id === row.id);
          if (now === undefined) return;
          if (now.status !== "refused") {
            await outcome(row);
            return;
          }
          const landed = outcome(row);
          writes.retry(row.id);
          await landed;
        }
      );
      const [, results] = await Promise.all([Promise.all(waiting), retried]);
      const refused = results.find((result) => result.status === "rejected");
      if (refused?.status === "rejected") throw refused.reason;
    },
    dismiss: () => remember(undefined),
  };
}
