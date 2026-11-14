// The Legion board route's rules, ported from the Go of cybermonklabs/legion#3578 (head
// 02a7b3c67e) for the views demo (README.md beside this file): packages/envoy/internal/dispatch/
// legion/board_rules.go (Column, SlotUse, CardWaiting), waiting_line.go (WaitingLine), tree.go
// (TreeChain) and api/legion_board.go (legionBoardCompute). Each function keeps its Go name and
// its order of tests, so a card the demo draws is the card that route draws from the same state.
// One field is left out: a card's `checks_verdict`, which cybermonklabs/legion#2731's newest head
// removes. The fixtures build (fixtures.ts) and the demo's stub (web/src/demo/) both call
// computeBoard, the stub again whenever a simulated daemon read moves the state.
import type {
  LegionBoardCard,
  LegionBoardColumn,
  LegionBoardFacts,
  LegionBoardHeader,
  LegionBoardResponse,
} from "@legion/contracts";

export const ColumnQueue = "queue";
export const ColumnDesign = "design";
export const ColumnPlanning = "planning";
export const ColumnImplementing = "implementing";
export const ColumnTesting = "testing";
export const ColumnReviewing = "reviewing";
export const ColumnAccepting = "accepting";
export const ColumnMerging = "merging";
export const ColumnAwaiting = "awaiting_person";
export const ColumnHeld = "held";

type ColumnId = LegionBoardColumn["id"];

/** legionBoardColumnOrder: every column, left to right, with the label the view shows. */
export const BOARD_COLUMN_ORDER: readonly { id: ColumnId; label: string }[] = [
  { id: "queue", label: "Queue" },
  { id: "design", label: "Design" },
  { id: "planning", label: "Planning" },
  { id: "implementing", label: "Implementing" },
  { id: "testing", label: "Testing" },
  { id: "reviewing", label: "Reviewing" },
  { id: "accepting", label: "Accepting" },
  { id: "merging", label: "Merging" },
  { id: "awaiting_person", label: "Waiting on a person" },
  { id: "held", label: "Held" },
];

export const PhaseAdmitted = "admitted";
export const PhaseAwaitingPerson = "awaiting_person";
export const PhasePlanning = "planning";
export const PhaseImplementing = "implementing";
export const PhaseTesting = "testing";
export const PhaseReviewing = "reviewing";
export const PhaseAccepting = "accepting";
export const PhaseRetro = "retro";
export const PhaseMerging = "merging";
export const PhaseAwaitingMerge = "awaiting_merge";
export const PhaseDone = "done";
export const PhaseHeld = "held";

/** MergerPhases: the three phases the merger works. */
export const MERGER_PHASES = [PhaseRetro, PhaseMerging, PhaseAwaitingMerge];

/** PhaseColumns: the column each phase falls in; a phase it does not name is on no card. */
export const PHASE_COLUMNS: Readonly<Record<string, string>> = {
  [PhaseAdmitted]: ColumnDesign,
  [PhasePlanning]: ColumnPlanning,
  [PhaseImplementing]: ColumnImplementing,
  [PhaseTesting]: ColumnTesting,
  [PhaseReviewing]: ColumnReviewing,
  [PhaseAccepting]: ColumnAccepting,
  [PhaseAwaitingPerson]: ColumnAwaiting,
  [PhaseHeld]: ColumnHeld,
  [PhaseRetro]: ColumnMerging,
  [PhaseMerging]: ColumnMerging,
  [PhaseAwaitingMerge]: ColumnMerging,
};

/** WorkingColumns: the columns of the phases a worker works in. */
const WORKING_COLUMNS = new Set([
  ColumnPlanning,
  ColumnImplementing,
  ColumnTesting,
  ColumnReviewing,
  ColumnAccepting,
  ColumnMerging,
]);

/** Queued: what a reader of one issue knows of the daemon's waiting line. */
export enum Queued {
  Unread = 0,
  No = 1,
  Yes = 2,
}

/** Column: the board column an issue stands in, "" for one on no card. */
export function column(phase: string, slot: number | null, queued: Queued): string {
  const held = PHASE_COLUMNS[phase];
  const named = held !== undefined;
  switch (queued) {
    case Queued.Yes:
      if (!named || (slot === null && !WORKING_COLUMNS.has(held))) return ColumnQueue;
      break;
    case Queued.Unread:
      if (waitingForASlot(phase, slot)) return ColumnQueue;
      break;
  }
  return held ?? "";
}

function waitingForASlot(phase: string, slot: number | null): boolean {
  return phase === PhaseAdmitted && slot === null;
}

export const SlotUseWorking = "working";
export const SlotUseBetweenTurns = "between_turns";
export const SlotUseHeldChildren = "held_children";
export const SlotUseHeldPerson = "held_person";
export const SlotUseHeldIdle = "held_idle";

const HELD_FOR_CHILDREN = new Set([PhaseAccepting, PhaseAwaitingMerge, PhaseAwaitingPerson]);

/** ChildWork: the phases of a child that keep its root's held slot busy. */
export const CHILD_WORK = new Set([PhaseImplementing, PhaseTesting, PhaseReviewing]);

/** SlotUse: what the slot an issue holds of its own is used for, "" with no slot of its own. */
export function slotUse(
  phase: string,
  roles: readonly string[],
  slot: number | null,
  childWorks: boolean
): string {
  if (slot === null) return "";
  if (roles.length > 0) return SlotUseWorking;
  if (phase === PhaseAwaitingPerson) return SlotUseHeldPerson;
  if (!HELD_FOR_CHILDREN.has(phase)) return SlotUseBetweenTurns;
  if (childWorks) return SlotUseHeldChildren;
  return SlotUseHeldIdle;
}

export const WaitingReady = "ready";
export const WaitingSlot = "slot";
export const WaitingBlocked = "blocked";
export const WaitingPerson = "person";

/** Wait: the daemon's record of what a root waits on (its `waitingOn`). */
export interface Wait {
  kind: string;
  since: string | null;
  answeredAt: string | null;
}

/** ParseWait: the wait one `waitingOn` value records, or null for one of another shape or with
 *  no kind. */
export function parseWait(value: unknown): Wait | null {
  if (typeof value !== "object" || value === null) return null;
  const wait = value as Record<string, unknown>;
  if (typeof wait.kind !== "string" || wait.kind === "") return null;
  return {
    kind: wait.kind,
    since: typeof wait.since === "string" ? wait.since : null,
    answeredAt: typeof wait.answeredAt === "string" ? wait.answeredAt : null,
  };
}

interface WaitingInput {
  phase: string;
  column: string;
  waitingPlace: number | null;
  blockedBy: string[];
  phaseSince: string | null;
  waitingOn: Wait | null;
}

interface Waiting {
  reason: string;
  since: string | null;
  blockedBy: string[];
}

/** CardWaiting: why the issue waits, null for one that waits for nothing. */
export function cardWaiting(input: WaitingInput): Waiting | null {
  const placed = input.waitingPlace !== null || input.column === ColumnQueue;
  if (!placed && input.column !== ColumnAwaiting) return null;
  const onPerson = !placed || (input.waitingOn !== null && input.waitingOn.answeredAt === null);
  const waiting: Waiting = { reason: WaitingReady, since: null, blockedBy: input.blockedBy };
  if (placed && input.blockedBy.length > 0) {
    waiting.reason = WaitingBlocked;
  } else if (placed && input.column !== ColumnQueue && input.phase !== PhaseAwaitingPerson) {
    waiting.reason = WaitingSlot;
  } else if (onPerson) {
    waiting.reason = WaitingPerson;
  }
  waiting.since = input.phaseSince;
  if (onPerson && input.waitingOn !== null && input.waitingOn.since !== null) {
    waiting.since = input.waitingOn.since;
  }
  return waiting;
}

/** TreeNode: what the tree walk reads of one issue. */
export interface TreeNode {
  tree: string;
  parent: string;
  legion: boolean;
}

type TreeLookup = (key: string) => TreeNode | undefined;

/** TreeAbove: the one step every tree walk takes. */
function treeAbove(key: string, node: TreeNode): { above: string; fromParent: boolean } {
  if (node.tree !== "") {
    if (node.tree === key) return { above: "", fromParent: false };
    return { above: node.tree, fromParent: false };
  }
  if (node.parent === "") return { above: "", fromParent: false };
  return { above: node.parent, fromParent: true };
}

/** TreeChain: the issue, then every issue the walk climbs through, ending at the tree root. */
export function treeChain(key: string, lookup: TreeLookup): string[] {
  const chain = [key];
  const seen = new Set<string>();
  for (;;) {
    const current = chain[chain.length - 1];
    if (seen.has(current)) return chain;
    seen.add(current);
    const node = lookup(current);
    if (node === undefined) return chain;
    const { above, fromParent } = treeAbove(current, node);
    if (above === "") return chain;
    if (fromParent) {
      const up = lookup(above);
      if (up === undefined || !up.legion) return chain;
    }
    chain.push(above);
  }
}

/** TreeRoot: the last issue of the chain. */
export function treeRoot(key: string, lookup: TreeLookup): string {
  const chain = treeChain(key, lookup);
  return chain[chain.length - 1];
}

/** The daemon's admission block, in the members the board reads. */
export interface Admission {
  cap?: number | null;
  active?: string[] | null;
  waiting?: string[] | null;
  trees?: string[] | null;
  blocked?: { issue: string; blockedBy?: string[] | null }[] | null;
}

interface LineIssue {
  phase: string;
  slot: number | null;
}

/** WaitingLine: one project's waiting line. */
class WaitingLine {
  private readonly read: boolean;
  private readonly order: string[] = [];
  private readonly places = new Map<string, number>();
  private readonly blockers = new Map<string, string[]>();
  private readonly columns = new Map<string, string>();

  constructor(
    admission: Admission | null,
    private readonly issue: (key: string) => LineIssue | undefined,
    private readonly tree: TreeLookup
  ) {
    this.read = admission !== null;
    if (admission === null) return;
    for (const key of admission.waiting ?? []) {
      if (issue(key) === undefined) continue;
      if (this.places.has(key)) continue;
      this.order.push(key);
      this.places.set(key, this.places.size + 1);
    }
    for (const blocked of admission.blocked ?? []) {
      this.blockers.set(blocked.issue, blocked.blockedBy ?? []);
    }
  }

  private queued(key: string): Queued {
    if (!this.read) return Queued.Unread;
    if ((this.places.get(key) ?? 0) > 0 || (this.blockers.get(key)?.length ?? 0) > 0) {
      return Queued.Yes;
    }
    return Queued.No;
  }

  place(key: string): number | null {
    return this.places.get(key) ?? null;
  }

  blockedBy(key: string): string[] | undefined {
    return this.blockers.get(key);
  }

  column(key: string): string {
    const known = this.columns.get(key);
    if (known !== undefined) return known;
    this.columns.set(key, "");
    const issue = this.issue(key);
    if (issue === undefined) return "";
    if (this.unstarted(key, issue) && this.nearestCard(key) !== "") return "";
    const held = column(issue.phase, issue.slot, this.queued(key));
    this.columns.set(key, held);
    return held;
  }

  countedOn(key: string): string {
    const issue = this.issue(key);
    if (issue === undefined || !this.unstarted(key, issue)) return "";
    return this.nearestCard(key);
  }

  queue(keys: readonly string[]): string[] {
    const queue: string[] = [];
    for (const key of this.order) {
      if (this.column(key) === ColumnQueue) queue.push(key);
    }
    for (const key of keys) {
      if (this.places.has(key)) continue;
      if (this.column(key) === ColumnQueue) queue.push(key);
    }
    return queue;
  }

  private unstarted(key: string, issue: LineIssue): boolean {
    return waitingForASlot(issue.phase, issue.slot) && this.queued(key) !== Queued.Yes;
  }

  private nearestCard(key: string): string {
    for (const above of treeChain(key, this.tree).slice(1)) {
      if (this.column(above) !== "") return above;
    }
    return "";
  }
}

/** What the sampler's newest observations say of one issue the daemon reports (the board's
 *  legionBoardIssue, less what Dispatch's own issues table joins in). */
export interface ObservedIssue {
  phase: string;
  /** When the issue entered its phase: the sampler's first observation of it. */
  phase_since: string | null;
  slot: number | null;
  /** When the issue was admitted to its slot. */
  slot_since: string | null;
  /** The daemon's own `tree`, "" where it names none. */
  tree: string;
  /** The daemon's `waitingOn`, as it sent it. */
  waiting_on: unknown;
  /** The roles whose claim state is `working`, sorted. */
  roles: string[];
}

/** Everything one board answer reads (the board's legionBoardPopulation). */
export interface BoardPopulation {
  project: string;
  /** The newest read of the daemon, null when none was made. */
  read: { at: string; ok: boolean; failure: string | null } | null;
  admission: Admission | null;
  settings: Record<string, unknown> | null;
  setting_sources: Record<string, unknown> | null;
  /** Every issue the daemon reports, by key. */
  issues: Record<string, ObservedIssue>;
  /** The issues that carry Legion phase work (a role event or a handoff). */
  phase_work: string[];
  /** The stored per-issue facts, by key. */
  facts: Record<string, LegionBoardFacts>;
  cursor_age_seconds: number | null;
}

/** What the board joins from Dispatch's own issues table. */
export interface DispatchIssue {
  key: string;
  title: string;
  priority: number | null;
  parent: string | null;
  rank: string;
  created_at: string;
}

/** A card as the demo serves it: the route's card, less `checks_verdict`. */
export type DemoBoardCard = Omit<LegionBoardCard, "checks_verdict">;

/** A column of the demo's cards. */
export type DemoBoardColumn = Omit<LegionBoardColumn, "cards"> & { cards: DemoBoardCard[] };

/** The board route's body as the demo serves it, of the demo's cards. */
export type DemoBoardResponse = Omit<LegionBoardResponse, "columns"> & {
  columns: DemoBoardColumn[];
};

/** A card as the compute makes it: the wire card and the daemon's wait it was made from, which
 *  the waiting rule reads once every card is placed and which the body does not carry. */
type MadeCard = DemoBoardCard & { wait: Wait | null };

/** treeNode: what the tree walk reads of one issue — the daemon's `tree`, Dispatch's parent,
 *  and whether the issue is Legion's (the daemon reports it, or it carries phase work). */
export function treeLookupOf(
  population: BoardPopulation,
  dispatchIssues: readonly DispatchIssue[]
): TreeLookup {
  const held = new Map(dispatchIssues.map((issue) => [issue.key, issue]));
  const phaseWork = new Set(population.phase_work);
  const observed = population.issues;
  return (key) => {
    const reported = Object.hasOwn(observed, key);
    const dispatch = held.get(key);
    if (!reported && dispatch === undefined) return undefined;
    return {
      tree: reported ? observed[key].tree : "",
      parent: dispatch?.parent ?? "",
      legion: reported || phaseWork.has(key),
    };
  };
}

/** legionBoardCompute: the board route's body for one population and the project's issues. */
export function computeBoard(
  population: BoardPopulation,
  dispatchIssues: readonly DispatchIssue[],
  now: string
): DemoBoardResponse {
  const ranked = [...dispatchIssues].sort(rankOrder).map((issue) => issue.key);
  const held = new Map(dispatchIssues.map((issue) => [issue.key, issue]));
  const phaseWork = new Set(population.phase_work);
  const observed = population.issues;
  const treeNode = treeLookupOf(population, dispatchIssues);
  const lineIssue = (key: string): LineIssue | undefined =>
    Object.hasOwn(observed, key)
      ? { phase: observed[key].phase, slot: observed[key].slot }
      : undefined;

  const rankIndex = new Map(ranked.map((key, index) => [key, index]));
  const keys = Object.keys(observed).sort((a, b) => {
    const ra = rankIndex.get(a);
    const rb = rankIndex.get(b);
    if (ra !== undefined && rb !== undefined) return ra - rb;
    if (ra !== undefined) return -1;
    if (rb !== undefined) return 1;
    return a < b ? -1 : a > b ? 1 : 0;
  });

  const makeCard = (key: string, blockedBy: string[] | undefined, children: number) => {
    const issue = observed[key];
    const dispatch = held.get(key);
    const tree = treeRoot(key, treeNode);
    const wait = parseWait(issue.waiting_on);
    const root = observed[tree];
    const card: MadeCard = {
      key,
      title: dispatch?.title ?? "",
      priority: dispatch?.priority ?? null,
      phase: issue.phase,
      phase_since: issue.phase_since,
      slot: issue.slot,
      tree_slot: issue.slot === null && tree !== key && root?.slot != null ? root.slot : null,
      slot_since: issue.slot !== null ? issue.slot_since : null,
      waiting_place: null,
      roles: [...issue.roles],
      blocked_by: blockedBy ?? [],
      waiting_on:
        wait === null ? null : { kind: wait.kind, since: wait.since, answered_at: wait.answeredAt },
      slot_use: null,
      waiting: null,
      tree,
      children,
      wait,
    };
    return card;
  };

  const line = new WaitingLine(population.admission, lineIssue, treeNode);
  const cards = new Map<string, MadeCard[]>();
  const add = (columnId: string, card: MadeCard) => {
    const made = cards.get(columnId) ?? [];
    made.push(card);
    cards.set(columnId, made);
  };
  const queued = line.queue(keys);
  const placed = new Set(queued);
  const uncarded = new Map<string, number>();
  for (const key of keys) {
    if (placed.has(key)) continue;
    const columnId = line.column(key);
    if (columnId === "") {
      const above = line.countedOn(key);
      if (above !== "") uncarded.set(above, (uncarded.get(above) ?? 0) + 1);
      continue;
    }
    add(columnId, makeCard(key, line.blockedBy(key), 0));
  }
  for (const key of queued) {
    add(ColumnQueue, makeCard(key, line.blockedBy(key), uncarded.get(key) ?? 0));
  }
  for (const made of cards.values()) {
    for (const card of made) card.waiting_place = line.place(card.key);
  }
  for (const [columnId, made] of cards) {
    if (columnId === ColumnQueue) continue;
    for (const card of made) card.children = uncarded.get(card.key) ?? 0;
  }

  const childWorks = new Set<string>();
  for (const made of cards.values()) {
    for (const card of made) {
      if (card.tree !== card.key && CHILD_WORK.has(card.phase)) childWorks.add(card.tree);
    }
  }
  for (const [columnId, made] of cards) {
    for (const card of made) {
      const use = slotUse(card.phase, card.roles ?? [], card.slot, childWorks.has(card.key));
      card.slot_use = use === "" ? null : use;
      const waiting = cardWaiting({
        phase: card.phase,
        column: columnId,
        waitingPlace: card.waiting_place,
        blockedBy: card.blocked_by ?? [],
        phaseSince: card.phase_since,
        waitingOn: card.wait,
      });
      card.waiting =
        waiting === null
          ? null
          : { reason: waiting.reason, since: waiting.since, blocked_by: waiting.blockedBy };
    }
  }

  const columns: DemoBoardColumn[] = BOARD_COLUMN_ORDER.map(({ id, label }) => ({
    id,
    label,
    cards: (cards.get(id) ?? []).map(({ wait: _wait, ...card }) => card),
  }));

  const admission = population.admission;
  const header: LegionBoardHeader = {
    slots_in_use: 0,
    slot_cap: null,
    trees: 0,
    queue_length: cards.get(ColumnQueue)?.length ?? 0,
    read_at: null,
    read_ok: false,
    read_error: null,
    legion_issues: ranked.filter((key) => phaseWork.has(key)).length,
    facts_updated_at: null,
    facts_cursor_age_seconds: population.cursor_age_seconds,
  };
  if (admission !== null && (admission.active?.length ?? 0) > 0) {
    header.slots_in_use = admission.active?.length ?? 0;
  } else {
    header.slots_in_use = Object.values(observed).filter((issue) => issue.slot !== null).length;
  }
  if (admission !== null) {
    header.slot_cap = admission.cap ?? null;
    header.trees = admission.trees?.length ?? 0;
  }
  if (population.read !== null) {
    header.read_at = population.read.at;
    header.read_ok = population.read.ok;
    header.read_error = population.read.failure;
  }
  const issues: Record<string, LegionBoardFacts> = {};
  for (const [key, row] of Object.entries(population.facts)) {
    issues[key] = { ...row, hours_by_phase: row.hours_by_phase ?? {} };
    if (header.facts_updated_at === null || row.updated_at < header.facts_updated_at) {
      header.facts_updated_at = row.updated_at;
    }
  }
  return {
    project: population.project,
    header,
    columns,
    issues,
    settings: population.settings ?? {},
    setting_sources: population.setting_sources ?? {},
    computed_at: now,
  };
}

/** Dispatch's rank order: rank (byte order, the column's "C" collation), then the creation
 *  time, then the key. */
export function rankOrder(a: DispatchIssue, b: DispatchIssue): number {
  if (a.rank !== b.rank) return a.rank < b.rank ? -1 : 1;
  if (a.created_at !== b.created_at) return a.created_at < b.created_at ? -1 : 1;
  return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
}
