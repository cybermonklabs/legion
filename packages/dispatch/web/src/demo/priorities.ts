// The views demo's priorities (packages/dispatch/e2e/views-demo/README.md): the invented
// priorities of priorities.json and the `advances` relations real issues state on them, answered
// as main's routes answer them (internal/dispatch/api/priority_queries.go and priorities.go, and
// issue_queries.go's `advances` filter and columns). Every count is read from the issues as the
// stub holds them at the moment of the request, so a write that moves an issue moves the counts.
import type {
  Actor,
  IssueAdvance,
  IssueSummary,
  PatchPriorityInput,
  Priorities,
  Priority,
  PriorityAct,
  PriorityIssue,
  PriorityRow,
} from "@legion/contracts";
import type { Moved, PrioritiesFixture } from "./fixtures";
import { goTime, ISSUE_STATUSES, rankAt } from "./wire";

/** model.IssueStatusesInFlight and model.IssueStatusesParked. */
const IN_FLIGHT = ["in_progress", "testing", "needs_review", "retro"];
const PARKED = ["icebox", "backlog"];
/** priorityFaultRows: how many rows each fault list carries beside its whole count. */
const FAULT_ROWS = 50;
/** priorityClosedWindow: how far back "closed recently" reaches. */
const CLOSED_WINDOW_MS = 7 * 86_400_000;
/** refs.IssueDepthCap: how far the inheritance walk climbs. */
const DEPTH_CAP = 32;
/** priorityNoneWritten: the sentence the answer carries when no priority is live. */
const NONE_WRITTEN =
  "No priority is live now, so the list is empty. Write one, then say which work advances it.";

/** What the tab keeps of the priorities across a reload: the nodes as the writes left them,
 *  each write's act, and each move into backlog or icebox a status write made. */
export interface PrioritiesKept {
  nodes: Priority[];
  writes: { priority: string; act: PriorityAct }[];
  moved: Record<string, Moved | null>;
}

/** A result the stub answers as a route's: its status and its body. */
type Answer = { status: number; body: unknown };

export class DemoPriorities {
  private readonly nodes = new Map<string, Priority>();
  /** The live priority ids each issue's own row states, and the seq of its newest relation. */
  private readonly own = new Map<string, string[]>();
  private readonly revision = new Map<string, number>();
  private readonly fixtureActs = new Map<string, PriorityAct[]>();
  private readonly writes: { priority: string; act: PriorityAct }[] = [];
  private readonly moved = new Map<string, Moved | null>();
  /** The moves a status write made, which the tab keeps. */
  private readonly statusMoves = new Map<string, Moved | null>();

  constructor(
    fixture: PrioritiesFixture,
    private readonly issues: ReadonlyMap<string, IssueSummary>,
    private readonly reader: Actor,
    kept: PrioritiesKept | undefined
  ) {
    for (const node of fixture.priorities) this.nodes.set(node.id, node);
    for (const [index, relation] of fixture.relations.entries()) {
      this.own.set(relation.issue, [...(this.own.get(relation.issue) ?? []), relation.priority]);
      this.revision.set(relation.issue, index + 1);
      this.actsOf(this.fixtureActs, relation.priority).push({
        field: "advances",
        value: "",
        issue: relation.issue,
        asserted: true,
        actor: relation.actor,
        at: relation.at,
      });
    }
    for (const node of fixture.priorities) {
      for (const field of ["state", "title", "owner", "description"] as const) {
        const value = field === "state" ? node.state : node[field];
        if (value === "") continue;
        this.actsOf(this.fixtureActs, node.id).push({
          field,
          value: field === "description" ? "" : value,
          issue: "",
          asserted: true,
          actor: node.made_by,
          at: node.made_at,
        });
      }
    }
    for (const [key, move] of Object.entries(fixture.moved)) this.moved.set(key, move);
    if (kept === undefined) return;
    for (const node of kept.nodes) this.nodes.set(node.id, node);
    this.writes.push(...kept.writes);
    for (const [key, move] of Object.entries(kept.moved)) {
      this.moved.set(key, move);
      this.statusMoves.set(key, move);
    }
  }

  /** What the tab keeps of the priorities: everything a write changed. */
  kept(): PrioritiesKept {
    const seen = new Set<string>();
    return {
      nodes: this.writes.flatMap(({ priority }) => {
        const node = this.nodes.get(priority);
        if (node === undefined || seen.has(priority)) return [];
        seen.add(priority);
        return [node];
      }),
      writes: this.writes,
      moved: Object.fromEntries(this.statusMoves),
    };
  }

  /** A status write's effect on the move cells: an issue moved into backlog or icebox was moved
   *  now by the reader, and one moved out of them has no move to show. */
  statusWritten(key: string, status: string, at: string): void {
    const move = PARKED.includes(status) ? { at, by: this.reader } : null;
    this.moved.set(key, move);
    this.statusMoves.set(key, move);
  }

  /** The live priorities in their one order: by rank in byte order, then by id. */
  private live(): Priority[] {
    return [...this.nodes.values()]
      .filter((node) => node.state === "live")
      .sort((a, b) => byteOrder(a.rank, b.rank) || byteOrder(a.id, b.id));
  }

  /** The priorities an issue advances (advancesScan): its own, then those it inherits from the
   *  issues above it, in the byte order of the id, an id held both ways listed once as its own. */
  advancesOf(key: string): IssueAdvance[] {
    const live = new Set(this.live().map((node) => node.id));
    const own = new Set((this.own.get(key) ?? []).filter((id) => live.has(id)));
    const inherited = new Set<string>();
    const climbed = new Set([key]);
    let at = this.issues.get(key)?.parent ?? null;
    for (let depth = 0; at !== null && !climbed.has(at) && depth < DEPTH_CAP; depth++) {
      climbed.add(at);
      for (const id of this.own.get(at) ?? []) if (live.has(id) && !own.has(id)) inherited.add(id);
      at = this.issues.get(at)?.parent ?? null;
    }
    return [
      ...[...own].map((id) => ({ id, inherited: false })),
      ...[...inherited].map((id) => ({ id, inherited: true })),
    ].sort((a, b) => byteOrder(a.id, b.id));
  }

  /** The two cells every issue read answers for its relations. */
  columnsOf(key: string): { advances: IssueAdvance[]; advances_revision: number } {
    return { advances: this.advancesOf(key), advances_revision: this.revision.get(key) ?? 0 };
  }

  /** The listing's `advances` filter (parseAdvancesFilter): each value a priority id, `none` or
   *  `any`, the values a union; undefined where the request names none, and an error where a
   *  value is blank. */
  advancesFilter(values: string[]): ((key: string) => boolean) | undefined | Answer {
    if (values.length === 0) return undefined;
    const ids = new Set<string>();
    let none = false;
    let some = false;
    for (const raw of values) {
      const value = raw.trim();
      if (value === "none") none = true;
      else if (value === "any") some = true;
      else if (value === "") {
        return refusal(
          400,
          "INVALID_ADVANCES",
          `advances filter values must be a priority id, none or any, got "${raw}"`
        );
      } else ids.add(value);
    }
    return (key) => {
      const entries = this.advancesOf(key);
      return (
        (none && entries.length === 0) ||
        (some && entries.length > 0) ||
        entries.some((entry) => ids.has(entry.id))
      );
    };
  }

  /** The move cells `?moved=true` answers on a row standing in backlog or icebox. */
  movedOf(issue: IssueSummary): { moved_at?: string; moved_by?: Actor } {
    const move = PARKED.includes(issue.status) ? this.moved.get(issue.key) : undefined;
    if (move === undefined || move === null) return {};
    return { moved_at: move.at, ...(move.by === null ? {} : { moved_by: move.by }) };
  }

  /** `GET /api/v1/priorities` (readPriorities): the live priorities in their order with the
   *  work under each, and the two faults, narrowed to the projects the request names; with
   *  `summary=true` the two fault counts alone. */
  list(q: URLSearchParams): Priorities {
    const projects = q
      .getAll("project")
      .map((value) => value.trim())
      .filter((value) => value !== "");
    const summary = q.get("summary") === "true";
    const live = this.live();
    const now = Date.now();
    const rows = new Map<string, PriorityRow>();
    if (!summary) {
      for (const node of live) {
        rows.set(node.id, {
          ...node,
          work: {
            by_status: Object.fromEntries(ISSUE_STATUSES.map((status) => [status, 0])),
            claimed: 0,
            open_asks: 0,
            closed_last_7_days: 0,
            newest_event_at: null,
          },
        });
      }
    }
    const unplaced: IssueSummary[] = [];
    const parked: { issue: IssueSummary; ids: string[] }[] = [];
    for (const issue of this.issues.values()) {
      if (projects.length > 0 && !projects.includes(projectOf(issue.key))) continue;
      const ids = this.advancesOf(issue.key).map((entry) => entry.id);
      const open = isOpen(issue);
      if (open && IN_FLIGHT.includes(issue.status) && ids.length === 0) unplaced.push(issue);
      if (open && PARKED.includes(issue.status) && ids.length > 0) parked.push({ issue, ids });
      for (const id of ids) {
        const work = rows.get(id)?.work;
        if (work === undefined) continue;
        if (open) {
          work.by_status = {
            ...work.by_status,
            [issue.status]: (work.by_status?.[issue.status] ?? 0) + 1,
          };
          if (issue.claim !== null) work.claimed++;
          work.open_asks += issue.open_asks;
        }
        if (issue.closed_at !== null && Date.parse(issue.closed_at) >= now - CLOSED_WINDOW_MS)
          work.closed_last_7_days++;
        if (work.newest_event_at === null || timeOrder(issue.updated_at, work.newest_event_at) > 0)
          work.newest_event_at = issue.updated_at;
      }
    }
    const body: Priorities = {
      priorities: [...rows.values()],
      work_under_no_priority: { count: unplaced.length, by_project: {}, issues: [] },
      priority_work_parked: { count: parked.length, by_priority: {}, issues: [] },
    };
    if (live.length === 0) body.empty = NONE_WRITTEN;
    if (summary) return body;
    body.work_under_no_priority.by_project = countBy(unplaced.map((issue) => projectOf(issue.key)));
    body.work_under_no_priority.issues = unplaced
      .sort((a, b) => timeOrder(b.updated_at, a.updated_at) || byteOrder(a.key, b.key))
      .slice(0, FAULT_ROWS)
      .map((issue) => this.faultRow(issue, false));
    body.priority_work_parked.by_priority = countBy(parked.flatMap(({ ids }) => ids));
    body.priority_work_parked.issues = parked
      .map(({ issue }) => this.faultRow(issue, true))
      .sort((a, b) => laterFirst(a.moved_at, b.moved_at) || byteOrder(a.key, b.key))
      .slice(0, FAULT_ROWS);
    return body;
  }

  private faultRow(issue: IssueSummary, parked: boolean): PriorityIssue {
    const move = parked ? this.moved.get(issue.key) : undefined;
    return {
      key: issue.key,
      title: issue.title,
      project: projectOf(issue.key),
      status: issue.status,
      assignee: issue.assignee,
      claim: issue.claim,
      changed_at: issue.updated_at,
      advances: parked ? this.advancesOf(issue.key) : [],
      moved_at: move?.at ?? null,
      moved_by: move?.by ?? null,
    };
  }

  /** `GET /api/v1/priorities/{id}/history`: every act of the priority, newest first. */
  history(id: string): Answer {
    if (!this.nodes.has(id)) return refusal(404, "PRIORITY_NOT_FOUND", `no priority ${id}`);
    const acts = [
      ...(this.fixtureActs.get(id) ?? []),
      ...this.writes.filter((write) => write.priority === id).map((write) => write.act),
    ];
    return { status: 200, body: acts.sort((a, b) => timeOrder(b.at, a.at)) };
  }

  /** `PATCH /api/v1/priorities/{id}`: its title, description, owner or place in the order. */
  patch(id: string, input: PatchPriorityInput): Answer {
    const held = this.nodes.get(id);
    if (held === undefined || held.state !== "live")
      return refusal(404, "PRIORITY_NOT_FOUND", `no priority ${id}`);
    const at = goTime(new Date());
    const next: Priority = { ...held };
    for (const field of ["title", "description", "owner"] as const) {
      const value = input[field];
      if (value === null || value === undefined || value === held[field]) continue;
      next[field] = value;
      this.write(id, field, field === "description" ? "" : value, at);
    }
    if (input.rank !== null && input.rank !== undefined) {
      const others = this.live().filter((node) => node.id !== id);
      const rankOf = (other: string | null | undefined) =>
        other ? (this.nodes.get(other)?.rank ?? "") : "";
      next.rank = rankAt(
        others.map((node) => node.rank),
        rankOf(input.rank.after),
        rankOf(input.rank.before)
      );
      this.write(id, "rank", next.rank, at);
    }
    this.nodes.set(id, next);
    return { status: 200, body: next };
  }

  /** `POST /api/v1/priorities`: a new priority, which takes the last place of the order; its
   *  id defaults to its title as a slug. */
  create(input: { title?: string; description?: string; owner?: string; id?: string }): Answer {
    const title = (input.title ?? "").trim();
    if (title === "") return refusal(400, "PRIORITY_INPUT", "a priority needs a title");
    const id = (input.id ?? "").trim() || slug(title);
    if (this.nodes.has(id) || id === "none" || id === "any")
      return refusal(409, "PRIORITY_EXISTS", `a priority ${id} is written already`);
    const at = goTime(new Date());
    const node: Priority = {
      id,
      title,
      description: input.description ?? "",
      owner: input.owner ?? "",
      rank: rankAt(
        this.live().map((one) => one.rank),
        this.live().at(-1)?.rank ?? "",
        ""
      ),
      state: "live",
      made_at: at,
      made_by: this.reader,
    };
    this.nodes.set(id, node);
    for (const field of ["state", "title", "owner", "description"] as const) {
      const value = field === "state" ? node.state : node[field];
      if (value !== "") this.write(id, field, field === "description" ? "" : value, at);
    }
    return { status: 201, body: node };
  }

  private write(priority: string, field: string, value: string, at: string): void {
    this.writes.push({
      priority,
      act: { field, value, issue: "", asserted: true, actor: this.reader, at },
    });
  }

  private actsOf(acts: Map<string, PriorityAct[]>, id: string): PriorityAct[] {
    const held = acts.get(id);
    if (held !== undefined) return held;
    const fresh: PriorityAct[] = [];
    acts.set(id, fresh);
    return fresh;
  }
}

/** Whether the issue is open: the stub's done rows of a project whose close times were not
 *  read keep a null closed_at, and are closed all the same. */
function isOpen(issue: IssueSummary): boolean {
  return issue.closed_at === null && issue.status !== "done";
}

function projectOf(key: string): string {
  return key.slice(0, key.lastIndexOf("-"));
}

function countBy(values: string[]): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const value of values) counts[value] = (counts[value] ?? 0) + 1;
  return counts;
}

/** Go's strings.Compare, the byte order the server sorts ids, keys and ranks in. */
function byteOrder(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** comparePriorityTimes: the later move first, a row with no move after every row with one. */
function laterFirst(a: string | null, b: string | null): number {
  if (a === b) return 0;
  if (a === null) return 1;
  if (b === null) return -1;
  return timeOrder(b, a);
}

/** Two times in their order, whatever fraction of a second each is written with. */
function timeOrder(a: string, b: string): number {
  return Date.parse(a) - Date.parse(b);
}

/** A title as the id a create derives from it: lower case, words joined by `-`. */
function slug(title: string): string {
  return title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

function refusal(status: number, code: string, error: string): Answer {
  return { status, body: { code, error } };
}
