// Builds the views demo's fixtures (README.md beside this file) from production reads:
//
//   DISPATCH_URL=… DISPATCH_TOKEN_FILE=… bun e2e/views-demo/fixtures.ts --out <dir> \
//     --project <KEY> --reader <login> --daemon-url <daemon base URL>
//
// It writes into --out only, never into the repository: the reads under <out>/raw/, and beside
// them the files the demo's stub loads (web/src/demo/fixtures.ts) and manifest.json, which names
// for each fixture its source, when it was read, and which of its fields are real, derived or
// invented. A second run reads only what <out>/raw/ lacks; --refresh reads everything again.
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { IssueSummary, LegionBoardFacts, LegionSetting, Project } from "@legion/contracts";
import type { DemoManifest, ReaderFixture } from "../../web/src/demo/fixtures";
import {
  type Admission,
  type BoardPopulation,
  computeBoard,
  type ObservedIssue,
  PHASE_COLUMNS,
  PhaseAwaitingPerson,
  PhaseDone,
  parseWait,
  treeLookupOf,
  treeRoot,
} from "./board-rules";
import { closedAt, deriveFacts, hasPhaseWork, type IssueLog, readLog } from "./phase-facts";
import { ProductionReader, type ReadEvent } from "./production";

const { values: flags } = parseArgs({
  options: {
    out: { type: "string" },
    project: { type: "string" },
    reader: { type: "string" },
    "daemon-url": { type: "string" },
    "daemon-state": { type: "string" },
    refresh: { type: "boolean", default: false },
    "reads-only": { type: "boolean", default: false },
  },
});

const out = required(flags.out, "--out <dir>, outside the repository");
const project = required(flags.project, "--project <KEY>");
const login = required(flags.reader, "--reader <login>, the person the page is signed in as");
const reader = new ProductionReader(
  join(out, "raw"),
  process.env.DISPATCH_URL,
  process.env.DISPATCH_TOKEN_FILE,
  flags.refresh
);
await reader.open();

/** The part of the daemon's `GET /legion/v1/state` the fixtures read. */
interface DaemonState {
  issues: Record<string, DaemonIssue>;
  admission?: Admission;
  settings?: Record<string, unknown>;
  settingSources?: Record<string, unknown>;
}
interface DaemonIssue {
  key: string;
  phase: string;
  tree?: string | null;
  slot?: { index: number; admittedAt?: string } | null;
  waitingOn?: unknown;
  architect?: { state?: string } | null;
  workers?: Record<string, { claim?: { state?: string } }> | null;
  pullRequest?: { checksVerdict?: string } | null;
}

const state = (await reader.daemonState(flags["daemon-url"], flags["daemon-state"])) as DaemonState;
const open = await reader.dispatch<IssueSummary[]>(
  "issues-open.json",
  `/api/v1/issues?project=${project}&open=true`
);
const done = await reader.dispatch<IssueSummary[]>(
  "issues-done.json",
  `/api/v1/issues?project=${project}&status=done`
);
const projects = await reader.dispatch<Project[]>("projects.json", "/api/v1/projects");
const schema = await reader.dispatch("schema-blocks.json", "/api/v1/schema/blocks");
const agents = await reader.dispatch<unknown[]>("agents.json", "/api/v1/agents");

// The events of every done issue (its close time and its facts) and of every open issue the
// daemon reports (its facts and its time in phase).
const withEvents = [...done, ...open.filter((issue) => state.issues[issue.key] !== undefined)];
const events = new Map<string, ReadEvent[]>();
for (const [index, issue] of withEvents.entries()) {
  events.set(issue.key, await reader.events(issue.key, issue.last_seq));
  if ((index + 1) % 200 === 0) console.log(`events: ${index + 1}/${withEvents.length}`);
}
if (flags["reads-only"]) process.exit(0);

const times = reader.readTimes();
const builtAt = new Date().toISOString();
const daemonReadAt = times["daemon-state.json"];
const issuesReadAt = times["issues-open.json"];
const now = Date.parse(issuesReadAt);

// The listing as main serves it: every row carries `closed_at`. Production's build does not
// carry the column yet, so a done issue's close is the newest event that set it done.
let closedUnknown = 0;
const issues: IssueSummary[] = [
  ...open.map((issue) => ({ ...issue, closed_at: null })),
  ...done.map((issue) => {
    const at = closedAt(events.get(issue.key) ?? []);
    if (at === null) closedUnknown++;
    return { ...issue, closed_at: at };
  }),
];
const byKey = new Map(issues.map((issue) => [issue.key, issue]));
const logs = new Map<string, IssueLog>();
for (const [key, read] of events) logs.set(key, readLog(key, read));

// What the sampler's newest observations would hold, from the one read of the daemon's state.
// The sampler records when a phase moved; one read cannot, so a card's time in phase is the
// start of the phase the issue's events put it in, where that is the daemon's phase, else its
// newest status change, else its creation.
let phaseSinceFromTimeline = 0;
let phaseSinceFromStatus = 0;
const observed: Record<string, ObservedIssue> = {};
for (const [key, issue] of Object.entries(state.issues)) {
  const roles = new Set<string>();
  if (issue.architect?.state === "working") roles.add("architect");
  for (const [role, worker] of Object.entries(issue.workers ?? {})) {
    if (worker.claim?.state === "working") roles.add(role);
  }
  const slot = issue.slot ?? null;
  observed[key] = {
    phase: issue.phase,
    phase_since: issue.phase === PhaseDone ? null : phaseSince(key, issue),
    slot: slot === null ? null : slot.index,
    slot_since: slot?.admittedAt ?? null,
    tree: issue.tree ?? "",
    waiting_on: issue.waitingOn ?? null,
    checks_verdict: issue.pullRequest?.checksVerdict ?? "",
    roles: [...roles].sort(),
  };
}

function phaseSince(key: string, issue: DaemonIssue): string | null {
  const wait = parseWait(issue.waitingOn);
  if (issue.phase === PhaseAwaitingPerson && wait?.since) return wait.since;
  const log = logs.get(key);
  const dispatch = byKey.get(key);
  if (log !== undefined) {
    const firstSlot = issue.slot?.admittedAt ? Date.parse(issue.slot.admittedAt) : null;
    const last = deriveFacts(log, firstSlot, now).last_phase;
    if (last !== null && PHASE_COLUMNS[last.phase] === PHASE_COLUMNS[issue.phase]) {
      phaseSinceFromTimeline++;
      return last.since;
    }
    const status = log.statusMarks.at(-1);
    if (status !== undefined) {
      phaseSinceFromStatus++;
      return new Date(status.at).toISOString();
    }
  }
  return dispatch?.created_at ?? null;
}

const phaseWork = [...logs].filter(([, log]) => hasPhaseWork(log)).map(([key]) => key);
const population: BoardPopulation = {
  project,
  read: { at: daemonReadAt, ok: true, failure: null },
  admission: state.admission ?? null,
  settings: state.settings ?? null,
  setting_sources: state.settingSources ?? null,
  issues: observed,
  phase_work: phaseWork,
  facts: {},
  cursor_age_seconds: 20,
};

// The per-issue facts: one row for every issue with Legion phase work, derived from its events
// by the metrics' rule for an issue the sampler never observed (phase-facts.ts).
const firstBoard = computeBoard(population, issues, builtAt);
const cards = new Map(
  (firstBoard.columns ?? []).flatMap((column) =>
    (column.cards ?? []).map((card) => [card.key, card])
  )
);
const treeLookup = treeLookupOf(population, issues);
const facts: Record<string, LegionBoardFacts> = {};
for (const key of phaseWork) {
  const issue = byKey.get(key);
  const log = logs.get(key);
  if (issue === undefined || log === undefined) continue;
  const slot = state.issues[key]?.slot;
  const firstSlot = slot?.admittedAt ? Date.parse(slot.admittedAt) : null;
  const end = issue.closed_at === null ? now : Date.parse(issue.closed_at);
  const derived = deriveFacts(log, firstSlot, end);
  const root = treeRoot(key, treeLookup);
  facts[key] = {
    opened_at: issue.created_at,
    closed_at: issue.closed_at,
    hours_by_phase: derived.hours_by_phase,
    work_hours: derived.work_hours,
    review_rounds: derived.review_rounds,
    root_key: root,
    tree_depth: treeDepth(key, root),
    slot_hours:
      firstSlot === null ? 0 : Math.round(((end - firstSlot) / 3_600_000) * 10_000) / 10_000,
    waiting_reason: cards.get(key)?.waiting?.reason ?? "",
    work_first_at: derived.work_first_at,
    work_last_at: derived.work_last_at,
    // Invented: written as if at the read, the moment the page's clock stands at (clock.ts).
    updated_at: daemonReadAt,
  };
}
population.facts = facts;

/** treeDepth: the child_of links between the issue and its root, 0 where the parents do not
 *  reach it. */
function treeDepth(key: string, root: string): number {
  const climbed = new Set<string>();
  for (
    let depth = 0, at: string | null = key;
    at !== null && at !== "" && !climbed.has(at);
    depth++
  ) {
    if (at === root) return depth;
    climbed.add(at);
    at = byKey.get(at)?.parent ?? null;
  }
  return 0;
}

const board = computeBoard(population, issues, builtAt);
const legionSettings: LegionSetting[] = [
  {
    project,
    state_url: flags["daemon-url"]
      ? `${flags["daemon-url"].replace(/\/$/, "")}/legion/v1/state`
      : "",
    created_by: { kind: "user", id: login },
    updated_at: daemonReadAt,
  } as LegionSetting,
];
const readerFixture: ReaderFixture = { login, user_state: {}, legion_settings: legionSettings };

const doneWithin = (days: number) =>
  issues.filter(
    (issue) => issue.closed_at !== null && now - Date.parse(issue.closed_at) <= days * 86_400_000
  ).length;
const cardCount = cards.size;
const manifest: DemoManifest = {
  built_at: builtAt,
  project,
  reader: login,
  issues_read_at: issuesReadAt,
  daemon_read_at: daemonReadAt,
  derived_summary: ["time in phase", "time per phase", "work time", "rounds", "close times"],
  // What the pages show that no production read holds, in reader.json's and population.json's
  // words below; the facts' cursor age is invented too, and no page shows it.
  invented_summary: [
    `${project}'s Legion setting`,
    "the signed-in reader and its empty Inbox",
    "the facts' write times",
  ],
  fixtures: {
    "issues.json": {
      source: `GET /api/v1/issues?project=${project}&open=true (${open.length} rows) and GET /api/v1/issues?project=${project}&status=done (${done.length} rows), production Dispatch`,
      read_at: issuesReadAt,
      real: ["every field of every row the two reads answered"],
      derived: [
        `closed_at of a done issue: the newest of its events that set its status to done (production's build carries no closed_at). ${done.length - closedUnknown} of ${done.length} done issues have one; ${closedUnknown} have no such event and keep null, so no closed window lists them. ${doneWithin(28)} closed in the 28 days before the read, ${doneWithin(40)} in 40.`,
        "closed_at of an open issue: null",
      ],
      invented: [],
    },
    "population.json": {
      source: `the INFRA Legion daemon's GET /legion/v1/state (${Object.keys(state.issues).length} issues, admission cap ${state.admission?.cap ?? "none"}), read in the cluster; and the events of ${events.size} issues (GET /api/v1/issues/{key}/events), production Dispatch`,
      read_at: daemonReadAt,
      real: [
        "each issue's phase, slot, tree, waitingOn, pull request checks verdict and working claims (the roles), as the state read answered them",
        "the admission block (cap, active, waiting, trees, blocked) and the settings",
        "read.at: when the state was read; read.ok true",
      ],
      derived: [
        "slot_since: the daemon's slot.admittedAt, in place of the sampler's first observation of the slot",
        `phase_since (a card's time in phase, and the Waited of a queued card): one read cannot say when a phase began, which the sampler records. For a card waiting on a person it is waitingOn.since; else the start of the phase the issue's events draw (the metrics' fallback timeline, phase-facts.ts) where that phase falls in the daemon's column (${phaseSinceFromTimeline} issues); else the issue's newest status change (${phaseSinceFromStatus}); else its creation`,
        `phase_work: the ${phaseWork.length} issues whose events hold a role session's event or a handoff, of the ${events.size} whose events were read (every done issue and every open issue the daemon reports); an open issue the daemon does not report is taken to have none`,
        `facts (${Object.keys(facts).length} rows, one per issue with phase work): hours_by_phase, work_hours, review_rounds, work_first_at and work_last_at by the metrics package's rule for an issue the sampler never observed (timeline.go fallbackPhases, compute.go computeWorkTime and reviewRounds), from Dispatch events alone; the sampler's own observations would move them. Waiting on a person, Held and waiting on children have no hours here: their time is in 'other'. root_key and tree_depth by the board's tree walk over the daemon's tree and Dispatch's parents; waiting_reason is the card's; opened_at and closed_at are the issue's`,
        "slot_hours: from slot.admittedAt to the read (or the close) for an issue holding a slot; 0 for any other",
      ],
      invented: ["facts updated_at: the daemon's read time", "cursor_age_seconds: 20"],
    },
    "board.json": {
      source:
        "computed by e2e/views-demo/board-rules.ts (the board route's rules of #3578 at 02a7b3c67e, ported) from population.json and issues.json; the stub computes it again in the browser",
      read_at: daemonReadAt,
      real: [
        `the ${cardCount} cards, their columns, waiting places, slots, slot uses, waiting reasons, roles, CI verdicts, blockers and children, and the header's slots in use (${board.header.slots_in_use}), cap (${board.header.slot_cap}), trees (${board.header.trees}) and queue length (${board.header.queue_length})`,
      ],
      derived: ["phase_since, slot_since and the issues map, as population.json says"],
      invented: ["computed_at"],
    },
    "agents.json": {
      source: `GET /api/v1/agents (${agents.length} sessions), production Dispatch`,
      read_at: times["agents.json"],
      real: [
        "every field, as it stood at the read: a session's state and last activity do not move",
      ],
      derived: [],
      invented: [],
    },
    "schema.json": {
      source: "GET /api/v1/schema/blocks, production Dispatch",
      read_at: times["schema-blocks.json"],
      real: ["every field"],
      derived: [],
      invented: [],
    },
    "projects.json": {
      source: "GET /api/v1/projects, production Dispatch",
      read_at: times["projects.json"],
      real: ["every field"],
      derived: [],
      invented: [],
    },
    "reader.json": {
      source:
        "none: production's build has no GET /api/v1/settings/legion, and the reads used a session token, not the reader's own sign-in",
      read_at: null,
      real: [],
      derived: [],
      invented: [
        `the signed-in reader (${login})`,
        `the Legion setting naming ${project}, made by the reader at the daemon's read time, with the daemon's in-cluster state address`,
        "the reader's issue state: nothing pinned, nothing read",
        "answered by the stub with no fixture: the reader's Inbox, replies and agent state, all empty; no secrets broker (the credential list answers null); no architecture source for any project; no comments and no asks on any issue (an issue's own page is outside the demo)",
      ],
    },
  },
};

await writeFile(join(out, "issues.json"), JSON.stringify(issues));
await writeFile(join(out, "population.json"), JSON.stringify(population));
await writeFile(join(out, "board.json"), JSON.stringify(board));
await writeFile(join(out, "projects.json"), JSON.stringify(projects));
await writeFile(join(out, "schema.json"), JSON.stringify(schema));
await writeFile(join(out, "agents.json"), JSON.stringify(agents));
await writeFile(join(out, "reader.json"), JSON.stringify(readerFixture, null, 2));
await writeFile(join(out, "manifest.json"), JSON.stringify(manifest, null, 2));
console.log(
  `wrote ${issues.length} issues, ${cardCount} cards, ${Object.keys(facts).length} facts rows to ${out}`
);

function required(value: string | undefined, what: string): string {
  if (value === undefined || value === "") throw new Error(`name ${what}`);
  return value;
}
