/**
 * What the table keeps in the browser about an action a reload interrupted, and the box both
 * halves of that storage read and write through. The reader's column layout is the other half,
 * and it lives in `issues-table-storage.ts`. Each name is imported from the module that defines
 * it: neither half re-exports the other's.
 *
 * Why the two halves are two modules. The table's write kind is registered from the kinds chunk
 * the write store's take-up fetches as the person signs in, so that a reload finds a kept table
 * write's kind (`features/writes/kinds.ts`), and that kind reads an interrupted action from here
 * (`issues-table-recovery.ts`). This half is all a reload's kind needs, so the kinds chunk loads
 * it alone: with the layout half in the same module, what every signed-in page downloads at
 * sign-in grows by 650 bytes (measured at `efdb650c`, dispatch://INFRA-1109, R13). Neither half
 * reaches the table model: the layout half takes the column list it validates against from its
 * caller (`StoredColumns`). Nothing of the table is drawn by a first paint: it is a page.
 *
 * One failure rule, for every field: a browser that refuses storage, a full quota or a corrupt
 * value is a table with no saved preferences, never an exception out of a click.
 */
export const tableStorageKey = (project: string) => `dispatch.project-table.${project}`;

/** The kinds of action a reload can interrupt. The union and the check a stored record passes
 *  are one list, so neither can gain a kind the other refuses. */
export const ACTION_KINDS = ["newEpic", "startNext", "merge"] as const;

export interface TableAction {
  id: string;
  kind: (typeof ACTION_KINDS)[number];
  project: string;
  keys: readonly string[];
  source?: string;
  target?: string;
  /** When the action was begun, as epoch milliseconds. A record older than `ACTION_LIFETIME_MS`
   *  is a tab somebody closed a day ago, not work this reader left half-done. */
  startedAt?: number;
  /** The merge reason exactly as it was sent, so a resume knows the sentence it is looking for
   *  even if the target has been renamed, or closed, since. */
  reasonBody?: string;
  /** The name a New epic was given, as the reader typed it: what Finish's question calls an epic
   *  whose create has not landed, which has no key yet. */
  title?: string;
  writes: Record<string, string>;
}

/** How long a stored action is still the reader's to finish: a day. Past that the banner goes
 *  and a new action may begin, because the writes it named have long since landed or been
 *  discarded in Pending changes. */
export const ACTION_LIFETIME_MS = 24 * 60 * 60 * 1000;

export function readStore(project: string): Record<string, unknown> {
  try {
    const stored: unknown = JSON.parse(localStorage.getItem(tableStorageKey(project)) ?? "{}");
    return stored !== null && typeof stored === "object" && !Array.isArray(stored)
      ? (stored as Record<string, unknown>)
      : {};
  } catch {
    return {};
  }
}

export function writeStore(project: string, fields: Record<string, unknown>): void {
  try {
    localStorage.setItem(
      tableStorageKey(project),
      JSON.stringify({ ...readStore(project), ...fields })
    );
  } catch {
    /* The URL still works when the browser refuses local preferences. */
  }
}

export function readTableAction(project: string): TableAction | undefined {
  const action = readStore(project).action as TableAction | undefined;
  if (
    action?.project === project &&
    typeof action.id === "string" &&
    (ACTION_KINDS as readonly string[]).includes(action.kind) &&
    Array.isArray(action.keys) &&
    action.keys.every((key: unknown) => typeof key === "string") &&
    action.writes &&
    typeof action.writes === "object" &&
    !Array.isArray(action.writes) &&
    Object.values(action.writes).every((key) => typeof key === "string") &&
    (action.target === undefined || typeof action.target === "string") &&
    (action.startedAt === undefined ||
      (typeof action.startedAt === "number" &&
        Date.now() - action.startedAt < ACTION_LIFETIME_MS)) &&
    (action.reasonBody === undefined || typeof action.reasonBody === "string") &&
    (action.title === undefined || typeof action.title === "string") &&
    (action.kind !== "merge" ||
      (typeof action.source === "string" && typeof action.target === "string"))
  )
    return action;
  return undefined;
}

export function writeTableAction(project: string, action: TableAction | undefined): void {
  writeStore(project, { action });
}
