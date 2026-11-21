import type { GoingPlanAnswer, GoingPlanRow, GoingPlanTick } from "../../api/types";
import { actionLabel } from "./action-names";
import { objectOf, textOf } from "./fields";
import type { PlanDraft } from "./going-store";

/**
 * What one plan's rows say to the person, and the person's answer to them: each row's change as
 * the item, the value now and the new value; a document change as its sentences and its diff;
 * the ticks, where unticking a row unticks every row that needs it and ticking one ticks every
 * row it needs; and the approval those make. Where each row is drawn is `plan-surfaces.ts`'.
 */

type Arguments = Readonly<Record<string, unknown>>;

/** A row's arguments as an object: what its tool runs with. */
export function argumentsOf(row: GoingPlanRow): Arguments {
  return objectOf(row.arguments);
}

/** The tools that make a new issue, whose row shows on the list of its parent. */
export function makesIssue(row: GoingPlanRow): boolean {
  return row.tool === "dispatch_issue";
}

/** How a row names its change wherever it is drawn: its action and its item, and for a new issue,
 *  which has no item before it is made, its title; so each row of a plan is named apart. */
export function rowLabel(row: GoingPlanRow): string {
  return actionLabel(
    row.tool,
    row.item === "" && makesIssue(row) ? textOf(argumentsOf(row).title) : row.item
  );
}

/** A value as one line of the row: a string as it is, anything else as its JSON. */
export function valueText(value: unknown): string {
  if (value === undefined || value === null) {
    return "none";
  }
  return typeof value === "string" ? value : JSON.stringify(value);
}

/** One field a row writes: its name, its value now and the value the row writes, the person's
 *  edit where they made one, and whether the person can edit it (a value of text). */
export interface RowChange {
  readonly field: string;
  readonly now: unknown;
  readonly next: unknown;
  readonly editable: boolean;
}

/** The fields a new issue's row names, in the order the row shows them. */
const NEW_ISSUE_FIELDS = ["title", "project", "parent", "priority", "labels"] as const;

/** What a row changes, field by field: the fields its item's read names, each with its value now
 *  and the new value; a new issue's title, project, parent, priority and labels, none of which has
 *  a value now. A row that names no field shows its item alone. */
export function rowChanges(row: GoingPlanRow, edits: Arguments | undefined): RowChange[] {
  const args = { ...argumentsOf(row), ...edits };
  const fields = makesIssue(row)
    ? NEW_ISSUE_FIELDS.filter((field) => args[field] !== undefined)
    : (row.fields ?? []);
  const now = objectOf(row.now);
  return fields.map((field) => ({
    editable: typeof args[field] === "string",
    field,
    next: args[field],
    now: makesIssue(row) ? undefined : (now[field] ?? (fields.length === 1 ? row.now : undefined)),
  }));
}

/** One piece of a document change: the text it takes out and the text it puts in. */
export interface ChangePiece {
  readonly removed: string;
  readonly added: string;
}

/** A change of a document's text, as the plan shows it first: two or three sentences that say what
 *  it changes, and the pieces of its diff, which the plan keeps closed under them. */
export interface DocumentChange {
  readonly sentences: string;
  readonly pieces: readonly ChangePiece[];
}

/** The document change a row makes, or undefined for a row of any other kind: a suggestion
 *  (`dispatch_suggest`, its quote and its replacement), an edit (`dispatch_doc_edit`, each
 *  operation's text out and in) and an upload over a document (`dispatch_artifact`, the content now
 *  and the content it writes). Its sentences are the ones the model wrote for it - the
 *  suggestion's rationale, the version's summary - or else the plan's own summary. */
export function documentChange(row: GoingPlanRow, summary: string): DocumentChange | undefined {
  const args = argumentsOf(row);
  const said = (value: unknown) =>
    typeof value === "string" && value.trim() !== "" ? value : summary;
  switch (row.tool) {
    case "dispatch_suggest":
      return {
        pieces: [{ added: textOf(args.replace_with), removed: textOf(args.quote) }],
        sentences: said(args.body),
      };
    case "dispatch_doc_edit": {
      const ops = Array.isArray(args.ops) ? args.ops : [];
      return {
        pieces: ops.map((op: unknown) => {
          const fields = objectOf(op);
          return { added: textOf(fields.with ?? fields.markdown), removed: textOf(fields.find) };
        }),
        sentences: said(args.summary),
      };
    }
    case "dispatch_artifact":
      return typeof args.content === "string"
        ? {
            pieces: [{ added: args.content, removed: textOf(objectOf(row.now).content) }],
            sentences: said(args.summary),
          }
        : undefined;
    default:
      return undefined;
  }
}

/** The rows that need `row`, and the rows that need those, by number. */
function rowsNeeding(rows: readonly GoingPlanRow[], row: number): Set<number> {
  const needing = new Set<number>();
  let grew = true;
  while (grew) {
    grew = false;
    for (const candidate of rows) {
      const needs = candidate.needs ?? [];
      if (!needing.has(candidate.row) && needs.some((need) => need === row || needing.has(need))) {
        needing.add(candidate.row);
        grew = true;
      }
    }
  }
  return needing;
}

/** The rows `row` needs, and the rows those need, by number. */
function rowsNeeded(rows: readonly GoingPlanRow[], row: number): Set<number> {
  const byNumber = new Map(rows.map((candidate) => [candidate.row, candidate]));
  const needed = new Set<number>();
  const visit = (at: number) => {
    for (const need of byNumber.get(at)?.needs ?? []) {
      if (!needed.has(need)) {
        needed.add(need);
        visit(need);
      }
    }
  };
  visit(row);
  return needed;
}

/** The draft with `row` ticked or unticked: unticking it unticks each row that needs it, and
 *  ticking it ticks each row it needs, so the draft never approves a row without what it needs. */
export function toggledDraft(
  draft: PlanDraft,
  rows: readonly GoingPlanRow[],
  row: number
): PlanDraft {
  const unticked = new Set(draft.unticked);
  if (unticked.has(row)) {
    unticked.delete(row);
    for (const need of rowsNeeded(rows, row)) unticked.delete(need);
  } else {
    unticked.add(row);
    for (const needing of rowsNeeding(rows, row)) unticked.add(needing);
  }
  return { ...draft, unticked };
}

/** The draft with the person's edit of `field` of `row`. */
export function editedDraft(
  draft: PlanDraft,
  row: number,
  field: string,
  value: string
): PlanDraft {
  const edits = new Map(draft.edits);
  edits.set(row, { ...edits.get(row), [field]: value });
  return { ...draft, edits };
}

/** Whether the row waits for the person: the rows a tick or an edit can change. */
export function rowWaits(row: GoingPlanRow): boolean {
  return row.state === "waiting";
}

/** The approval the draft makes of `plan` (`POST /api/v1/going/runs/{id}/plan/approve`): the
 *  version of the plan the person was shown, which Dispatch refuses the approval over once the
 *  plan has changed since, each waiting row's tick, and the values the person edited. */
export function approvalOf(
  plan: GoingPlanAnswer,
  draft: PlanDraft
): { plan: number; rows: GoingPlanTick[]; version: GoingPlanAnswer["version"] } {
  const rows: GoingPlanTick[] = (plan.rows ?? []).filter(rowWaits).map((row) => {
    const values = draft.edits.get(row.row);
    return {
      row: row.row,
      tick: !draft.unticked.has(row.row),
      ...(values === undefined ? {} : { values: { ...values } }),
    };
  });
  return { plan: plan.plan, rows, version: plan.version };
}
