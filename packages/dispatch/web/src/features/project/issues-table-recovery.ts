import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError } from "../../api/client";
import { newSendKey } from "../../lib/send-key";
import { defineWriteKind, useWrites } from "../writes/write-store";

export const tableStorageKey = (project: string) => `dispatch.project-table.${project}`;
export interface TableAction {
  id: string;
  kind: "newEpic" | "startNext" | "merge";
  project: string;
  keys: readonly string[];
  source?: string;
  target?: string;
  writes: Record<string, string>;
}
export function readTableAction(project: string): TableAction | undefined {
  try {
    const action = JSON.parse(localStorage.getItem(tableStorageKey(project)) ?? "{}").action;
    if (
      action?.project === project &&
      typeof action.id === "string" &&
      ["newEpic", "startNext", "merge"].includes(action.kind) &&
      Array.isArray(action.keys) &&
      action.keys.every((key: unknown) => typeof key === "string") &&
      action.writes &&
      typeof action.writes === "object" &&
      !Array.isArray(action.writes) &&
      Object.values(action.writes).every((key) => typeof key === "string") &&
      (action.target === undefined || typeof action.target === "string") &&
      (action.kind !== "merge" ||
        (typeof action.source === "string" && typeof action.target === "string"))
    )
      return action;
  } catch {
    // A malformed preference is not an action to send.
  }
  return undefined;
}
function keepAction(project: string, action: TableAction | undefined): void {
  const saved = JSON.parse(localStorage.getItem(tableStorageKey(project)) ?? "{}");
  localStorage.setItem(tableStorageKey(project), JSON.stringify({ ...saved, action }));
}

// Keep the answer's issue key even when the table that started creation is no longer mounted.
export const tableEpicWrite = defineWriteKind<string>({
  name: "issues-table-epic",
  kept: (value): value is string => typeof value === "string",
  landed: (client, write, answer, awaited) => {
    const action = readTableAction(write.detail);
    if (
      action?.writes.create === write.key &&
      answer &&
      typeof answer === "object" &&
      "key" in answer &&
      typeof answer.key === "string"
    ) {
      keepAction(write.detail, { ...action, target: answer.key });
    }
    if (!awaited || answer === null) void client.invalidateQueries();
  },
});

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
      keepAction(project, next);
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
      } else if (failed.current.delete(event.write.id)) {
        reportRef.current(
          failed.current.size
            ? `Changes not saved: ${[...failed.current.values()].join(", ")}. Retry them in Pending changes.`
            : event.kind === "discarded"
              ? "The failed change was discarded."
              : current.current
                ? "The retried changes are saved. Finish the interrupted action to complete its remaining steps."
                : "All changes saved."
        );
      }
      if (event.kind !== "refused") owned.current.delete(event.write.id);
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
    begin: (record: Omit<TableAction, "id" | "project" | "writes">) => {
      const next = { ...record, id: newSendKey(), project, writes: {} };
      remember(next);
      return next;
    },
    key: (step: string, id?: string) => {
      const key = newSendKey();
      const record = current.current;
      if (id && record?.id === id)
        remember({ ...record, writes: { ...record.writes, [step]: key } });
      return key;
    },
    watch: (key: string, issue: string) => {
      const row = writes.rows().find((value) => value.key === key);
      if (row) owned.current.set(row.id, { ...owned.current.get(row.id), issue });
    },
    settle: async (record: TableAction) => {
      await Promise.all(
        writes
          .rows()
          .filter((row) => Object.values(record.writes).includes(row.key))
          .map((row) => {
            const { promise, resolve, reject } = Promise.withResolvers<void>();
            const unsubscribe = writes.onOutcome((event) => {
              if (event.write.id !== row.id) return;
              unsubscribe();
              if (event.kind === "landed") resolve();
              else
                reject(
                  event.kind === "refused"
                    ? event.refusal
                    : new Error(`${row.label} was discarded.`)
                );
            });
            if (row.status === "refused") writes.retry(row.id);
            return promise;
          })
      );
    },
    dismiss: () => remember(undefined),
  };
}
