export const BUILT_IN_VIEW_IDS = [
  "open",
  "triage",
  "waiting",
  "in-flight",
  "slots",
  "finished",
  "mine",
] as const;

export type BuiltInViewID = (typeof BUILT_IN_VIEW_IDS)[number];

export interface ExpectedIssue {
  readonly key: string;
  readonly status: string;
  readonly closed_at?: string | null;
  readonly created_by?: { readonly id?: string | null } | null;
  readonly labels?: readonly string[];
}

export interface ExpectedCard {
  readonly key: string;
  readonly slot_use: string | null;
  readonly waiting: object | null;
}

export interface ExpectedBoard {
  readonly columns: readonly {
    readonly id: string;
    readonly cards: readonly ExpectedCard[];
  }[];
  readonly issues?: Readonly<Record<string, unknown>>;
}

export interface ExpectedViewCountsInput {
  readonly openIssues: readonly ExpectedIssue[];
  readonly doneIssues: readonly ExpectedIssue[];
  readonly board: ExpectedBoard;
  readonly hasLegion: boolean;
  readonly login: string;
  readonly nowMs: number;
}

const DAY_MS = 24 * 60 * 60 * 1000;
export const IN_FLIGHT_STAGES: Readonly<Record<string, true>> = {
  "status:in_progress": true,
  "phase:design": true,
  "phase:planning": true,
  "phase:implementing": true,
  "phase:testing": true,
  "status:needs_review": true,
  "phase:reviewing": true,
  "phase:accepting": true,
  "phase:merging": true,
  "phase:awaiting_person": true,
  "phase:held": true,
  "phase:waiting_on_children": true,
  "phase:other": true,
  "status:retro": true,
};

function cardColumns(board: ExpectedBoard): Map<string, string> {
  const result = new Map<string, string>();
  for (const column of board.columns) {
    for (const card of column.cards) result.set(card.key, column.id);
  }
  return result;
}

export function expectedStageOf(issue: ExpectedIssue, phase: string | undefined): string {
  if (phase !== undefined) return `phase:${phase}`;
  if (issue.labels?.some((label) => label.toLowerCase() === "legion")) {
    if (issue.status === "in_progress") return "phase:implementing";
    if (issue.status === "needs_review") return "phase:reviewing";
    if (issue.status === "todo") return "phase:queue";
  }
  return issue.status === "testing" ? "phase:testing" : `status:${issue.status}`;
}

function closedInLast28Days(issue: ExpectedIssue, nowMs: number): boolean {
  if (issue.status !== "done" || issue.closed_at === undefined || issue.closed_at === null)
    return false;
  const closedMs = Date.parse(issue.closed_at);
  return !Number.isNaN(closedMs) && closedMs >= nowMs - 28 * DAY_MS && closedMs <= nowMs;
}

/** Counts every built-in view from the API responses alone, rather than from the page's view model. */
export function expectedViewCounts(input: ExpectedViewCountsInput): Record<BuiltInViewID, number> {
  const cards = cardColumns(input.board);
  let triage = 0;
  let inFlight = 0;
  let mine = 0;
  for (const issue of input.openIssues) {
    if (issue.status === "triage") triage += 1;
    if (IN_FLIGHT_STAGES[expectedStageOf(issue, cards.get(issue.key))] === true) inFlight += 1;
    if (issue.created_by?.id === input.login) mine += 1;
  }

  const openKeys = new Set(input.openIssues.map((issue) => issue.key));
  let waiting = 0;
  let slots = 0;
  for (const column of input.board.columns) {
    for (const card of column.cards) {
      if (!openKeys.has(card.key)) continue;
      if (card.waiting !== null) waiting += 1;
      if (card.slot_use !== null) slots += 1;
    }
  }

  let finished = 0;
  const facts = input.board.issues;
  for (const issue of input.doneIssues) {
    if (
      closedInLast28Days(issue, input.nowMs) &&
      (!input.hasLegion || facts?.[issue.key] !== undefined)
    )
      finished += 1;
  }

  return {
    open: input.openIssues.length,
    triage,
    waiting,
    "in-flight": inFlight,
    slots,
    finished,
    mine,
  };
}

export function expectedStageCount(input: ExpectedViewCountsInput, stage: string): number {
  const cards = cardColumns(input.board);
  let count = 0;
  for (const issue of input.openIssues) {
    if (expectedStageOf(issue, cards.get(issue.key)) === stage) count += 1;
  }
  return count;
}
