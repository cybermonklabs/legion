/**
 * The fake Legion daemon's state and outbox (dispatch://INFRA-979 prototype), apart from its
 * network loop (legion-fake-daemon.ts) so a test drives it.
 *
 * The simulation, which runs only when asked (--simulate), holds a steady state: each tick moves
 * a few of the issues the replay shows running one step through the workflow's running phases, a
 * reviewer or a red check sometimes sends one back to implementing, and an issue in
 * awaiting_merge starts over in planning, standing for the next root its slot takes, rather than
 * going to done. No issue leaves the running phases and none loses its slot. A root that enters a
 * phase holding its slot for its children (accepting, awaiting merge) gets no turn there, so the
 * held and idle slots stay those of a real daemon.
 */

export interface DaemonIssue {
  key: string;
  phase?: string;
  slot?: { index: number; admittedAt?: string } | null;
  waitingOn?: { since?: string; answeredAt?: string } | null;
  architect?: { state?: string };
  workers?: Record<string, { claim?: { state?: string } }>;
  pullRequest?: { checksVerdict?: string };
}

export interface DaemonState {
  issues: Record<string, DaemonIssue>;
  admission?: { waiting?: string[]; [key: string]: unknown };
  settings?: Record<string, unknown>;
  settingSources?: Record<string, unknown> | null;
}

export interface PushEvent {
  seq: number;
  at: string;
  kind: string;
  issue?: string;
  value: unknown;
  /** A phase whose `at` is not when the issue entered it: no record holds that time. */
  at_unknown?: boolean;
}

/** The Dispatch status each daemon phase writes, for reading an entry time off the status
 *  history; a phase no status names (awaiting a person, held) has none. */
const PHASE_STATUS: Readonly<Record<string, string>> = {
  admitted: "todo",
  planning: "in_progress",
  implementing: "in_progress",
  testing: "testing",
  reviewing: "needs_review",
  accepting: "needs_review",
  retro: "retro",
  merging: "retro",
  awaiting_merge: "retro",
  done: "done",
};

/** The handoff that ends the phase before, for the two phases that share a status with the one
 *  before them: implementing starts at the plan's handoff, accepting at the review's. */
const PHASE_HANDOFF: Readonly<Record<string, string>> = {
  implementing: "plan",
  accepting: "review",
};

/** One record of the history an entry time is read from: a status change or a handoff. */
export interface HistoryRow {
  kind: "status" | "handoff";
  key: string;
  at: string;
  /** The status written, or the handoff's phase word. */
  value: string;
  /** Who wrote it: the session or person id. */
  actor: string | null;
}

/** The status changes and handoffs the replayed daemon could have seen, by issue, in time order:
 *  none later than the replay, and none the fake daemon wrote itself, so a restart of the fake
 *  reads exactly what its first boot read. */
export function historyMarks(
  rows: readonly HistoryRow[],
  { replayAt, self }: { replayAt: string; self: string }
): {
  statuses: Map<string, { at: string; status: string }[]>;
  handoffs: Map<string, { at: string; phase: string }[]>;
} {
  const statuses = new Map<string, { at: string; status: string }[]>();
  const handoffs = new Map<string, { at: string; phase: string }[]>();
  const cutoff = Date.parse(replayAt);
  const kept = rows
    .filter((row) => row.actor !== self && Date.parse(row.at) <= cutoff)
    .sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
  for (const row of kept) {
    if (row.kind === "status") {
      const marks = statuses.get(row.key) ?? [];
      marks.push({ at: row.at, status: row.value });
      statuses.set(row.key, marks);
    } else {
      const marks = handoffs.get(row.key) ?? [];
      marks.push({ at: row.at, phase: row.value });
      handoffs.set(row.key, marks);
    }
  }
  return { statuses, handoffs };
}

/** Each issue's entry time into the phase it holds, from Dispatch's own records: its last status
 *  change into the status that phase writes, or for implementing and accepting the last handoff
 *  after it that starts them. An issue no record places in its phase has none. */
export function phaseEntryTimes(
  phases: ReadonlyMap<string, string>,
  statuses: ReadonlyMap<string, readonly { at: string; status: string }[]>,
  handoffs: ReadonlyMap<string, readonly { at: string; phase: string }[]>
): Map<string, string> {
  const entries = new Map<string, string>();
  for (const [key, phase] of phases) {
    const status = PHASE_STATUS[phase];
    if (status === undefined) continue;
    const marks = statuses.get(key) ?? [];
    let entered: string | undefined;
    marks.forEach((mark, index) => {
      if (mark.status === status && marks[index - 1]?.status !== status) entered = mark.at;
    });
    if (entered === undefined) continue;
    const word = PHASE_HANDOFF[phase];
    if (word !== undefined) {
      const after = (handoffs.get(key) ?? []).filter(
        (handoff) => handoff.phase === word && handoff.at >= (entered ?? "")
      );
      const last = after.at(-1);
      if (last !== undefined) entered = last.at;
    }
    entries.set(key, entered);
  }
  return entries;
}

/** The running phases in workflow order, and the role in a turn in each. */
export const RUNNING = [
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
  "merging",
  "awaiting_merge",
] as const;
export const ROLE: Readonly<Record<string, string>> = {
  planning: "planner",
  implementing: "implementer",
  testing: "tester",
  reviewing: "reviewer",
  accepting: "architect",
  retro: "implementer",
  merging: "merger",
  awaiting_merge: "merger",
};
/** The daemon's checks verdicts (classify.HeadVerdict): green, red, or none while pending. */
const VERDICTS = ["green", "red", ""];
/** The phases in which a root with children holds its slot for them alone. */
const HELD_FOR_CHILDREN = new Set(["accepting", "awaiting_merge", "awaiting_person"]);

/** The phase an issue moves to from `from`: the next running phase, planning out of
 *  awaiting_merge (its slot's next root), and implementing out of reviewing when `sendBack`. */
export function nextPhase(from: string, sendBack: boolean): string {
  if (from === "awaiting_merge") return "planning";
  if (from === "reviewing" && sendBack) return "implementing";
  const at = RUNNING.indexOf(from as (typeof RUNNING)[number]);
  return RUNNING[at + 1] ?? "implementing";
}

export class FakeDaemonModel {
  readonly state: DaemonState;
  readonly sources: Record<string, unknown>;
  readonly bootAt: string;
  seq = 0;
  dropped = 0;
  /** Change events made so far, which `dropEvery` counts; a snapshot's events are never dropped. */
  changes = 0;
  /** Every change event made, oldest first, up to `keep`: the outbox a resend reads. */
  readonly kept: PushEvent[] = [];
  /** The change events not yet sent, a dropped one aside. */
  unsent: PushEvent[] = [];
  /** When each value last changed, by `kind|issue|role or setting`: a snapshot's `at`. */
  readonly changedAt = new Map<string, string>();
  private readonly dropEvery: number;
  private readonly keep: number;
  private readonly now: () => Date;
  private readonly random: () => number;
  readonly log: (line: string) => void;
  /** The issues the replay shows running: the only ones the simulation moves. */
  private readonly running: readonly DaemonIssue[];
  /** The issues with a child, from each issue's parent. */
  private readonly withChildren: ReadonlySet<string>;

  constructor(
    state: DaemonState,
    options: {
      dropEvery?: number;
      keep?: number;
      now?: () => Date;
      random?: () => number;
      log?: (line: string) => void;
      /** Each issue's entry time into the phase the replay gives it (phaseEntryTimes). */
      entryTimes?: ReadonlyMap<string, string>;
      /** Each issue's parent, by key: a parent is a root that can hold its slot for children. */
      parents?: ReadonlyMap<string, string>;
    } = {}
  ) {
    this.state = state;
    this.sources = { ...(state.settingSources ?? {}) };
    this.dropEvery = options.dropEvery ?? 0;
    this.keep = options.keep ?? 100_000;
    this.now = options.now ?? (() => new Date());
    this.random = options.random ?? Math.random;
    this.log = options.log ?? (() => {});
    this.bootAt = this.now().toISOString();
    this.running = Object.values(state.issues).filter((issue) =>
      RUNNING.includes(issue.phase as (typeof RUNNING)[number])
    );
    this.withChildren = new Set(options.parents?.values() ?? []);
    for (const [key, at] of options.entryTimes ?? []) this.changedAt.set(`phase|${key}|`, at);
    for (const issue of Object.values(state.issues)) {
      if (issue.slot?.admittedAt) this.changedAt.set(`slot|${issue.key}|`, issue.slot.admittedAt);
      const waited = issue.waitingOn?.answeredAt ?? issue.waitingOn?.since;
      if (waited) this.changedAt.set(`waiting_on|${issue.key}|`, waited);
    }
  }

  /** The issue's claim state for `role`. */
  claimOf(issue: DaemonIssue, role: string): string | undefined {
    return role === "architect" ? issue.architect?.state : issue.workers?.[role]?.claim?.state;
  }

  private setClaim(issue: DaemonIssue, role: string, claim: string): void {
    if (role === "architect") issue.architect = { ...issue.architect, state: claim };
    else {
      issue.workers ??= {};
      issue.workers[role] = { ...issue.workers[role], claim: { state: claim } };
    }
  }

  /** A change event with the next seq, at now, kept in the outbox. A dropped one is never sent
   *  the first time; a resend carries it. */
  emit(kind: string, value: unknown, issue?: string, sub = ""): void {
    this.seq += 1;
    const at = this.now().toISOString();
    this.changedAt.set(`${kind}|${issue ?? ""}|${sub}`, at);
    const event: PushEvent = { seq: this.seq, at, kind, value };
    if (issue !== undefined) event.issue = issue;
    this.kept.push(event);
    if (this.kept.length > this.keep) this.kept.shift();
    this.changes += 1;
    if (this.dropEvery > 0 && this.changes % this.dropEvery === 0) {
      this.dropped += 1;
      this.log(`  dropped seq ${this.seq} (${kind} ${issue ?? ""}) at ${at}`);
      return;
    }
    this.unsent.push(event);
  }

  /** Every current value as events with fresh seqs, each at the time its value changed: what a
   *  boot, or a gap older than the outbox, sends. */
  snapshotEvents(): PushEvent[] {
    const events: PushEvent[] = [];
    const add = (kind: string, value: unknown, issue?: string, sub = "") => {
      this.seq += 1;
      const known = this.changedAt.get(`${kind}|${issue ?? ""}|${sub}`);
      const event: PushEvent = { seq: this.seq, at: known ?? this.bootAt, kind, value };
      if (kind === "phase" && known === undefined) event.at_unknown = true;
      if (issue !== undefined) event.issue = issue;
      events.push(event);
    };
    for (const issue of Object.values(this.state.issues)) {
      if (issue.phase) add("phase", issue.phase, issue.key);
      if (issue.slot) add("slot", issue.slot.index, issue.key);
      if (issue.waitingOn) add("waiting_on", issue.waitingOn, issue.key);
      if (issue.pullRequest?.checksVerdict)
        add("checks_verdict", issue.pullRequest.checksVerdict, issue.key);
      if (issue.architect?.state)
        add(
          "claim_state",
          { role: "architect", state: issue.architect.state },
          issue.key,
          "architect"
        );
      for (const [role, worker] of Object.entries(issue.workers ?? {})) {
        if (worker.claim?.state)
          add("claim_state", { role, state: worker.claim.state }, issue.key, role);
      }
    }
    if (this.state.admission) add("waiting_line", this.state.admission);
    for (const [key, value] of Object.entries(this.state.settings ?? {})) {
      add(
        "setting",
        { key, value, source: this.sources[key] ?? { source: "file", at: this.bootAt } },
        undefined,
        key
      );
    }
    return events;
  }

  /** One tick of simulated work: `moves` running issues one step each, a checks verdict, and the
   *  waiting line turned by one. */
  simulate(moves = 2): void {
    const moving = [...this.running];
    for (let n = 0; n < moves && moving.length > 0; n++) {
      const issue = moving.splice(Math.floor(this.random() * moving.length), 1)[0];
      if (issue === undefined) break;
      const from = issue.phase ?? "";
      const to = nextPhase(from, this.random() < 0.3);
      const ended = ROLE[from];
      if (ended && this.claimOf(issue, ended) === "working") {
        this.setClaim(issue, ended, "idle");
        this.emit("claim_state", { role: ended, state: "idle" }, issue.key, ended);
      }
      issue.phase = to;
      this.emit("phase", to, issue.key);
      // A root held for its children has no turn of its own there.
      const started =
        HELD_FOR_CHILDREN.has(to) && this.withChildren.has(issue.key) ? undefined : ROLE[to];
      if (started) {
        this.setClaim(issue, started, "working");
        this.emit("claim_state", { role: started, state: "working" }, issue.key, started);
      }
      this.log(`  ${issue.key}: ${from} -> ${to}`);
    }
    const withPull = Object.values(this.state.issues).filter(
      (issue) => issue.pullRequest && issue.phase !== "done"
    );
    const pull = withPull[Math.floor(this.random() * withPull.length)];
    if (pull?.pullRequest) {
      const verdict = VERDICTS[(VERDICTS.indexOf(pull.pullRequest.checksVerdict ?? "") + 1) % 3];
      pull.pullRequest.checksVerdict = verdict;
      this.emit("checks_verdict", verdict, pull.key);
      this.log(`  ${pull.key}: checks ${verdict}`);
    }
    const waiting = this.state.admission?.waiting;
    if (waiting && waiting.length > 1) {
      const last = waiting.pop();
      if (last !== undefined) waiting.unshift(last);
      this.emit("waiting_line", this.state.admission);
      this.log(`  queue: ${waiting[0]} moved to the front`);
    }
  }

  /** A settings change through the daemon's settings route: applied, sourced and pushed. */
  applySetting(
    path: string,
    value: unknown,
    by: string | null
  ): { applied?: string; refused?: string } {
    const settings = this.state.settings ?? {};
    const [top, ...rest] = path.split(".");
    if (!top || !(top in settings)) return { refused: "no such setting" };
    if (rest.length === 0) settings[top] = value;
    else {
      let node = settings[top] as Record<string, unknown>;
      for (const part of rest.slice(0, -1)) {
        node[part] ??= {};
        node = node[part] as Record<string, unknown>;
      }
      node[rest.at(-1) as string] = value;
    }
    const source = { source: "route", by, at: this.now().toISOString() };
    this.sources[top] = source;
    this.emit("setting", { key: top, value: settings[top], source }, undefined, top);
    return { applied: "at-once" };
  }
}
