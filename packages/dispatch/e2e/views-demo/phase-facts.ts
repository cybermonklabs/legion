// One issue's Legion facts for the views demo (README.md beside this file), derived from the
// issue's Dispatch events by the metrics package's own rule for an issue the sampler never
// observed (packages/envoy/internal/dispatch/legion/metrics at 02a7b3c67e: timeline.go's
// fallbackPhases and fallbackMarks, compute.go's computeWorkTime, reviewRounds and treeDepth,
// breakdown.go's issueParts). Production's facts come from the sampler's phase observations,
// which no route serves; this fallback reads only the event log, so its hours are near the
// stored ones and not equal to them. The parts that need the observations (Waiting on a person,
// Held, and the hours a root waits on its children) are not drawn: their hours fall to `other`.
import type { ReadEvent } from "./production";

const PHASE_DESIGN = "design";
/** phaseOrder: the workflow's order of the phases the fallback names. */
const PHASE_ORDER = [
  PHASE_DESIGN,
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
];
/** rolePhase: the phase each role's first event starts. */
const ROLE_PHASE: Record<string, string> = {
  architect: PHASE_DESIGN,
  planner: "planning",
  implementer: "implementing",
  tester: "testing",
  reviewer: "reviewing",
  merger: "retro",
};
/** handoffStarts: the phase each handoff word starts. */
const HANDOFF_STARTS: Record<string, string> = {
  plan: "implementing",
  implement: "testing",
  test: "reviewing",
  review: "accepting",
};
const SENT_BACK_STATUSES = ["testing", "needs_review", "retro"];
const PARKED_STATUSES = ["backlog", "icebox"];
/** breakdownParts: the board's columns and the phases whose hours each holds. */
const BREAKDOWN_PARTS: [string, string[]][] = [
  ["design", [PHASE_DESIGN]],
  ["planning", ["planning"]],
  ["implementing", ["implementing"]],
  ["testing", ["testing"]],
  ["reviewing", ["reviewing"]],
  ["accepting", ["accepting"]],
  ["merging", ["retro", "merging", "awaiting_merge"]],
];
const ROLES = new Set(Object.keys(ROLE_PHASE));

enum Source {
  Role = 0,
  Handoff = 1,
  Status = 2,
}

interface Mark {
  at: number;
  source: Source;
  phase: string;
}

interface Span {
  from: number;
  to: number;
}

/** What the event log says of one issue: its roles' first events, its handoffs, its statuses. */
export interface IssueLog {
  firstRoleEvent: Map<string, number>;
  handoffs: { at: number; phase: string }[];
  statusMarks: { at: number; value: string }[];
}

/** The role of a session titled `Legion <role> · <KEY>` that works `key` (legion.WorkerTitle). */
function workerRole(title: string | null, key: string): string | undefined {
  const match = /^Legion (\S+) · (\S+)$/.exec((title ?? "").trim());
  if (match === null || match[2] !== key || !ROLES.has(match[1])) return undefined;
  return match[1];
}

export function readLog(key: string, events: readonly ReadEvent[]): IssueLog {
  const log: IssueLog = { firstRoleEvent: new Map(), handoffs: [], statusMarks: [] };
  for (const event of events) {
    const at = Date.parse(event.created_at);
    const role = event.actor_kind === "session" ? workerRole(event.session_title, key) : undefined;
    if (role !== undefined && !((log.firstRoleEvent.get(role) ?? Infinity) <= at)) {
      log.firstRoleEvent.set(role, at);
    }
    if (event.handoff_phase !== null) log.handoffs.push({ at, phase: event.handoff_phase });
    const status = statusSet(event);
    if (status !== null) log.statusMarks.push({ at, value: status });
  }
  return log;
}

/** hasPhaseWork: a role event or a handoff. */
export function hasPhaseWork(log: IssueLog): boolean {
  return log.firstRoleEvent.size > 0 || log.handoffs.length > 0;
}

/** The status an event sets, or null for one that sets none: `issue.closed` sets done, and an
 *  `issue.created` or an `issue.updated` whose `changed` names the status sets its own. */
function statusSet(event: ReadEvent): string | null {
  if (event.type === "issue.closed") return "done";
  if (event.type === "issue.created" || (event.changed ?? []).includes("status")) {
    return event.status;
  }
  return null;
}

/** The time the issue's status last became done, from its events: its close. */
export function closedAt(events: readonly ReadEvent[]): string | null {
  for (let index = events.length - 1; index >= 0; index--) {
    if (statusSet(events[index]) === "done") return events[index].created_at;
  }
  return null;
}

function statusPhase(previous: string, status: string): string | undefined {
  switch (status) {
    case "testing":
      return "testing";
    case "needs_review":
      return "reviewing";
    case "retro":
      return "retro";
    case "in_progress":
      return SENT_BACK_STATUSES.includes(previous) ? "implementing" : undefined;
  }
  return undefined;
}

function fallbackMarks(log: IssueLog, firstSlot: number | null): Mark[] {
  const marks: Mark[] = [];
  for (const [role, at] of log.firstRoleEvent) {
    if (role !== "architect") marks.push({ at, source: Source.Role, phase: ROLE_PHASE[role] });
  }
  let architect = log.firstRoleEvent.get("architect");
  if (architect !== undefined) {
    if (firstSlot !== null && firstSlot < architect) architect = firstSlot;
    marks.push({ at: architect, source: Source.Role, phase: PHASE_DESIGN });
  }
  for (const handoff of log.handoffs) {
    const phase = HANDOFF_STARTS[handoff.phase];
    if (phase !== undefined) marks.push({ at: handoff.at, source: Source.Handoff, phase });
  }
  let previous = "";
  for (const mark of log.statusMarks) {
    if (mark.value === previous) continue;
    const phase = statusPhase(previous, mark.value);
    if (phase !== undefined) marks.push({ at: mark.at, source: Source.Status, phase });
    previous = mark.value;
  }
  return marks.sort(
    (a, b) =>
      a.at - b.at ||
      a.source - b.source ||
      PHASE_ORDER.indexOf(a.phase) - PHASE_ORDER.indexOf(b.phase)
  );
}

function phaseSteps(marks: readonly Mark[]): Mark[] {
  const steps: Mark[] = [];
  let reached = -1;
  for (const mark of marks) {
    const rank = PHASE_ORDER.indexOf(mark.phase);
    if (mark.source === Source.Role && rank <= reached) continue;
    reached = Math.max(reached, rank);
    const last = steps.length - 1;
    if (last >= 0 && steps[last].phase === mark.phase) continue;
    if (last >= 0 && steps[last].at === mark.at) {
      steps[last] = { ...steps[last], phase: mark.phase };
      if (last > 0 && steps[last - 1].phase === mark.phase) steps.pop();
      continue;
    }
    steps.push({ ...mark });
  }
  return steps;
}

function firstWorkerEvent(log: IssueLog): number | null {
  const times = [...log.firstRoleEvent.values(), ...log.handoffs.map((handoff) => handoff.at)];
  return times.length === 0 ? null : Math.min(...times);
}

/** The phase timeline the event log draws, each phase running to the next one's start. */
export function fallbackPhases(
  log: IssueLog,
  firstSlot: number | null,
  end: number
): { phase: string; span: Span }[] {
  const steps = phaseSteps(fallbackMarks(log, firstSlot));
  if (steps.length === 0) return [];
  const first = firstWorkerEvent(log);
  if (first !== null && first < steps[0].at) steps[0] = { ...steps[0], at: first };
  const timeline: { phase: string; span: Span }[] = [];
  for (const [index, step] of steps.entries()) {
    const span = { from: step.at, to: index + 1 < steps.length ? steps[index + 1].at : end };
    if (span.to - span.from === 0) continue;
    timeline.push({ phase: step.phase, span });
  }
  return timeline;
}

function normalize(spans: readonly Span[]): Span[] {
  const sorted = spans.filter((span) => span.to > span.from).sort((a, b) => a.from - b.from);
  const out: Span[] = [];
  for (const span of sorted) {
    const last = out.at(-1);
    if (last !== undefined && span.from <= last.to) last.to = Math.max(last.to, span.to);
    else out.push({ ...span });
  }
  return out;
}

function intersect(a: readonly Span[], b: readonly Span[]): Span[] {
  const out: Span[] = [];
  for (const x of normalize(a)) {
    for (const y of normalize(b)) {
      const from = Math.max(x.from, y.from);
      const to = Math.min(x.to, y.to);
      if (to > from) out.push({ from, to });
    }
  }
  return normalize(out);
}

function subtract(a: readonly Span[], b: readonly Span[]): Span[] {
  let rest = normalize(a);
  for (const cut of normalize(b)) {
    rest = rest.flatMap((span) => {
      if (cut.to <= span.from || cut.from >= span.to) return [span];
      const left = { from: span.from, to: cut.from };
      const right = { from: cut.to, to: span.to };
      return [left, right].filter((part) => part.to > part.from);
    });
  }
  return rest;
}

function total(spans: readonly Span[]): number {
  return spans.reduce((sum, span) => sum + (span.to - span.from), 0);
}

/** hours: a duration in hours with four decimals, as the metrics round it. */
function hours(ms: number): number {
  return Math.round((ms / 3_600_000) * 10_000) / 10_000;
}

/** The facts of one issue the fallback can derive: its phase hours, its work time, its review
 *  rounds and the ends of its measured work. */
export interface DerivedFacts {
  hours_by_phase: Record<string, number>;
  work_hours: number;
  review_rounds: number;
  work_first_at: string | null;
  work_last_at: string | null;
  /** The phase the timeline ends in and when it began, for a card's time in phase. */
  last_phase: { phase: string; since: string } | null;
}

export function deriveFacts(log: IssueLog, firstSlot: number | null, end: number): DerivedFacts {
  const timeline = fallbackPhases(log, firstSlot, end);
  const start = firstWorkerEvent(log);
  let worked: Span[] = [];
  if (start !== null) {
    const life = [{ from: start, to: end }];
    const parked = log.statusMarks.flatMap((mark, index) => {
      if (!PARKED_STATUSES.includes(mark.value)) return [];
      const to = index + 1 < log.statusMarks.length ? log.statusMarks[index + 1].at : end;
      return [{ from: mark.at, to }];
    });
    worked = subtract(life, intersect(parked, life));
  }
  let rest = worked;
  const spent: Record<string, number> = {};
  for (const [id, phases] of BREAKDOWN_PARTS) {
    const claim = timeline.filter((step) => phases.includes(step.phase)).map((step) => step.span);
    const taken = intersect(rest, claim);
    spent[id] = total(taken);
    rest = subtract(rest, taken);
  }
  spent.other = total(rest);
  const hoursByPhase: Record<string, number> = {};
  for (const [id, ms] of Object.entries(spent)) if (hours(ms) > 0) hoursByPhase[id] = hours(ms);
  let rounds = 0;
  for (const [index, mark] of log.statusMarks.entries()) {
    if (
      mark.value === "needs_review" &&
      (index === 0 || log.statusMarks[index - 1].value !== mark.value)
    ) {
      rounds++;
    }
  }
  const bars = normalize(worked);
  const last = timeline.at(-1);
  return {
    hours_by_phase: hoursByPhase,
    work_hours: hours(total(worked)),
    review_rounds: rounds,
    work_first_at: bars.length === 0 ? null : new Date(bars[0].from).toISOString(),
    work_last_at: bars.length === 0 ? null : new Date(bars[bars.length - 1].to).toISOString(),
    last_phase:
      last === undefined
        ? null
        : { phase: last.phase, since: new Date(last.span.from).toISOString() },
  };
}
