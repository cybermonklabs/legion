import type { GoingPlanRow } from "../../api/types";
import { argumentsOf, makesIssue } from "./plan-rows";

/**
 * Where a planned change is drawn (R4, plan.md settlement 7): on the page when the page shows its
 * item, and otherwise in the plan card. A change of an issue's field, link or claim shows on a list
 * or a board that shows the issue and on the issue's own page; a new issue shows on the list of its
 * parent - the project's issue list or board for an issue with no parent, the parent's Children tab
 * for one with a parent, and any list that shows the parent - and a change of a document's text on
 * that document's page. These five pages draw what they show, and every other page leaves the
 * whole plan to the card.
 */

/** What one page shows, which decides the plan rows it draws. */
export type PlanSurface =
  /** The project's issue list or board: its project and the issues it shows. */
  | { readonly kind: "issues"; readonly project: string; readonly issues: readonly string[] }
  /** One issue's page: the issue itself. */
  | { readonly kind: "issue"; readonly issue: string }
  /** One issue's Children tab: the issue and its children. */
  | { readonly kind: "children"; readonly issue: string; readonly children: readonly string[] }
  /** One document's page: its id, its name and slug, and who owns it. */
  | {
      readonly kind: "document";
      readonly id: string;
      readonly names: readonly string[];
      readonly owner: string;
    };

/** The pages that draw a planned change of an item they show, each by the file that mounts the
 *  drawing (`PlannedChanges`) and the kind of surface it gives it. `plan-surfaces.test.ts` holds
 *  that each of them draws the rows of its items. */
export const DRAWING_SURFACES = [
  { file: "features/project/IssuesTable.tsx", kind: "issues", page: "the issue list" },
  { file: "features/project/IssueBoard.tsx", kind: "issues", page: "the board" },
  { file: "features/issue-page/IssuePage.tsx", kind: "issue", page: "the issue page" },
  { file: "features/issue/ChildrenTab.tsx", kind: "children", page: "the issue's Children tab" },
  { file: "features/document/DocumentPage.tsx", kind: "document", page: "a document page" },
] as const satisfies readonly {
  readonly file: string;
  readonly kind: PlanSurface["kind"];
  readonly page: string;
}[];

/** The classes of change a list, a board or an issue page draws on the item it shows: a field, a
 *  link, a claim, a new issue. A message, a document change and the rest of the classes are no
 *  value of the item the page draws. */
const ITEM_CLASSES: Readonly<Record<string, true>> = {
  claim: true,
  create_plain: true,
  field: true,
  link: true,
};

/** The classes of change a document's page draws: its text, and a suggestion in it. */
const DOCUMENT_CLASSES: Readonly<Record<string, true>> = {
  create_text: true,
  suggestion: true,
  text: true,
};

/** The keys a surface names, as an issue key or a document name is written, upper-cased once
 *  for each read of a plan's rows: the items it shows, and the issue or project that holds them. */
interface SurfaceKeys {
  readonly items: ReadonlySet<string>;
  readonly holder: ReadonlySet<string>;
}

function upperCased(keys: readonly string[]): ReadonlySet<string> {
  return new Set(keys.map((key) => key.toUpperCase()));
}

function keysOf(surface: PlanSurface): SurfaceKeys {
  switch (surface.kind) {
    case "issues":
      return { holder: upperCased([surface.project]), items: upperCased(surface.issues) };
    case "issue":
      return { holder: upperCased([surface.issue]), items: upperCased([surface.issue]) };
    case "children":
      return { holder: upperCased([surface.issue]), items: upperCased(surface.children) };
    case "document":
      return { holder: upperCased([surface.owner]), items: upperCased(surface.names) };
  }
}

/** Whether `value` is one of `keys`, whatever its case. */
function named(value: unknown, keys: ReadonlySet<string>): boolean {
  return typeof value === "string" && value !== "" && keys.has(value.toUpperCase());
}

/** Whether `surface`, whose keys are `keys`, draws `row`. */
function drawsRow(surface: PlanSurface, keys: SurfaceKeys, row: GoingPlanRow): boolean {
  const args = argumentsOf(row);
  switch (surface.kind) {
    case "issues":
      if (makesIssue(row)) {
        return args.parent === undefined || args.parent === ""
          ? named(args.project, keys.holder)
          : named(args.parent, keys.items);
      }
      return ITEM_CLASSES[row.class] === true && named(row.item || row.issue, keys.items);
    case "issue":
      return (
        !makesIssue(row) &&
        ITEM_CLASSES[row.class] === true &&
        named(row.item || row.issue, keys.items)
      );
    case "children":
      return makesIssue(row)
        ? named(args.parent, keys.holder)
        : ITEM_CLASSES[row.class] === true && named(row.item || row.issue, keys.items);
    case "document":
      return (
        DOCUMENT_CLASSES[row.class] === true &&
        (row.item === surface.id ||
          (named(args.artifact ?? args.name, keys.items) &&
            named(args.issue ?? args.project, keys.holder)))
      );
  }
}

/** The numbers of the rows of `rows` that `surface` draws. */
export function rowsOnSurface(surface: PlanSurface, rows: readonly GoingPlanRow[]): number[] {
  const keys = keysOf(surface);
  return rows.filter((row) => drawsRow(surface, keys, row)).map((row) => row.row);
}
