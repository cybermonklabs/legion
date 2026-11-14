// Builds the views demo's fixtures (README.md beside this file) from production reads:
//
//   DISPATCH_URL=… DISPATCH_TOKEN_FILE=… bun e2e/views-demo/fixtures.ts --out <dir> \
//     --project <KEY> --reader <login> --daemon-url <daemon base URL> \
//     [--no-daemon-project <KEY>]… [--priorities <file>]
//
// --project is the project with a Legion daemon. Each --no-daemon-project adds a real project's
// open and done listings, with no Legion setting, so the views of a project with no daemon show.
// --priorities names a file, kept outside the repository, of invented priorities and the real
// issues each one is advanced by (README.md, "The priorities"): production's build has none.
//
// It writes into --out only, never into the repository: the reads under <out>/raw/, and beside
// them the files the demo's stub loads (web/src/demo/fixtures.ts) and manifest.json, which names
// for each fixture its source, when it was read, and which of its fields are real, derived or
// invented. A second run reads only what <out>/raw/ lacks; --refresh reads everything again.
import { readFile, writeFile } from "node:fs/promises";
import { basename, join } from "node:path";
import { parseArgs } from "node:util";
import type {
  Actor,
  IssueSummary,
  LegionBoardFacts,
  LegionSetting,
  Priority,
  Project,
} from "@legion/contracts";
import type {
  DemoManifest,
  Moved,
  PrioritiesFixture,
  ReaderFixture,
} from "../../web/src/demo/fixtures";
import { rankBetween } from "../../web/src/demo/wire";
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
    "no-daemon-project": { type: "string", multiple: true, default: [] },
    "reads-only": { type: "boolean", default: false },
    priorities: { type: "string" },
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
// Each project with no daemon: its two listings alone. Its done rows keep a null closed_at, since
// a close time is read from the issue's events, which are not read for it.
const noDaemon = flags["no-daemon-project"].filter((key) => key !== project);
const noDaemonListings: { key: string; open: IssueSummary[]; done: IssueSummary[] }[] = [];
for (const key of noDaemon) {
  noDaemonListings.push({
    key,
    open: await reader.dispatch<IssueSummary[]>(
      `issues-open-${key}.json`,
      `/api/v1/issues?project=${key}&open=true`
    ),
    done: await reader.dispatch<IssueSummary[]>(
      `issues-done-${key}.json`,
      `/api/v1/issues?project=${key}&status=done`
    ),
  });
}
const noDaemonIssues: IssueSummary[] = noDaemonListings.flatMap((listing) => [
  ...listing.open.map((issue) => ({ ...issue, closed_at: null })),
  ...listing.done.map((issue) => ({ ...issue, closed_at: null })),
]);

// The events of every done issue (its close time and its facts) and of every open issue the
// daemon reports (its facts and its time in phase).
const withEvents = [...done, ...open.filter((issue) => state.issues[issue.key] !== undefined)];
const events = new Map<string, ReadEvent[]>();
for (const [index, issue] of withEvents.entries()) {
  events.set(issue.key, await reader.events(issue.key, issue.last_seq));
  if ((index + 1) % 200 === 0) console.log(`events: ${index + 1}/${withEvents.length}`);
}

/** The invented priorities (--priorities): each one's title, owner and description, and the
 *  real issues that state an `advances` relation on it, in its order. */
interface PriorityInput {
  made_at: string;
  priorities: {
    id?: string;
    title: string;
    owner: string;
    description: string;
    issues: string[];
  }[];
}
const priorityInput: PriorityInput =
  flags.priorities === undefined
    ? { made_at: "", priorities: [] }
    : JSON.parse(await readFile(flags.priorities, "utf8"));
const listed = new Map(
  [...open, ...done, ...noDaemonListings.flatMap((one) => [...one.open, ...one.done])].map(
    (issue) => [issue.key, issue]
  )
);
const stating = new Set(priorityInput.priorities.flatMap((priority) => priority.issues));
for (const key of stating) {
  if (!listed.has(key)) throw new Error(`--priorities names ${key}, which no listing holds`);
}
/** Whether the issue or one above it states a relation: the work under some priority. */
function underAPriority(key: string): boolean {
  const climbed = new Set<string>();
  for (let at: string | null = key; at !== null && !climbed.has(at); ) {
    if (stating.has(at)) return true;
    climbed.add(at);
    at = listed.get(at)?.parent ?? null;
  }
  return false;
}
const PARKED = ["backlog", "icebox"];
// The events of the priority work standing in backlog or icebox that the reads above did not
// take, for when each was moved there. Kept apart from `events`, which the facts are read from,
// so the priorities move no other fixture.
const parkedEvents = new Map<string, ReadEvent[]>();
for (const issue of listed.values()) {
  if (!PARKED.includes(issue.status) || events.has(issue.key) || !underAPriority(issue.key))
    continue;
  parkedEvents.set(issue.key, await reader.events(issue.key, issue.last_seq));
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

// The priorities: every node and relation invented from --priorities, each relation stated by the
// reader a minute apart after its priority was written; and each move into backlog or icebox
// that an issue's events show, as the parked fault reads it (parkedMoveLateral): the issue's
// newest `issue.updated` event that set one of the two statuses.
const priorityActor: Actor = { kind: "user", id: login };
const minutesAfter = (at: string, minutes: number) =>
  new Date(Date.parse(at) + minutes * 60_000).toISOString();
let previousRank = "";
const priorityNodes: Priority[] = priorityInput.priorities.map((input, index) => {
  previousRank = rankBetween(previousRank, "");
  return {
    id:
      input.id ??
      input.title
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-|-$/g, ""),
    title: input.title,
    description: input.description,
    owner: input.owner,
    rank: previousRank,
    state: "live",
    made_at: minutesAfter(priorityInput.made_at, index * 15),
    made_by: priorityActor,
  };
});
const relations = priorityInput.priorities.flatMap((input, index) =>
  input.issues.map((issue, at) => ({
    issue,
    priority: priorityNodes[index].id,
    at: minutesAfter(priorityNodes[index].made_at, at + 1),
    actor: priorityActor,
  }))
);
const moved: Record<string, Moved> = {};
let parkedUnderNoMove = 0;
for (const issue of listed.values()) {
  if (!PARKED.includes(issue.status)) continue;
  const read = events.get(issue.key) ?? parkedEvents.get(issue.key);
  const move = read
    ?.filter(
      (event) =>
        event.type === "issue.updated" &&
        (event.changed ?? []).includes("status") &&
        PARKED.includes(event.status ?? "")
    )
    .at(-1);
  if (move !== undefined) {
    moved[issue.key] = {
      at: move.created_at,
      by: move.actor_kind === "" ? null : { kind: move.actor_kind, id: move.actor_id },
    };
  } else if (underAPriority(issue.key)) parkedUnderNoMove++;
}
const prioritiesFixture: PrioritiesFixture = { priorities: priorityNodes, relations, moved };
const parkedUnder = [...listed.values()].filter(
  (issue) => PARKED.includes(issue.status) && underAPriority(issue.key)
).length;

const doneWithin = (days: number) =>
  issues.filter(
    (issue) => issue.closed_at !== null && now - Date.parse(issue.closed_at) <= days * 86_400_000
  ).length;
const cardCount = cards.size;
const manifest: DemoManifest = {
  built_at: builtAt,
  project,
  no_daemon_projects: noDaemon.map((key) => ({ key, read_at: times[`issues-open-${key}.json`] })),
  reader: login,
  issues_read_at: issuesReadAt,
  daemon_read_at: daemonReadAt,
  derived_summary: ["time in phase", "time per phase", "work time", "rounds", "close times"],
  // What the pages show that no production read holds, in reader.json's, population.json's and
  // priorities.json's words below; the facts' cursor age is invented too, and no page shows it.
  invented_summary: [
    `${project}'s Legion setting`,
    "the signed-in reader and its empty Inbox",
    "the facts' write times",
    ...(priorityNodes.length > 0 ? ["the priorities and their links"] : []),
  ],
  invented_phone: [
    "Legion setting",
    "reader's Inbox",
    "facts' write times",
    ...(priorityNodes.length > 0 ? ["priorities and their links"] : []),
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
    // The projects with no Legion daemon share issues.json, each with its own record here.
    ...Object.fromEntries(
      noDaemonListings.map((listing) => [
        `issues.json: ${listing.key}`,
        {
          source: `GET /api/v1/issues?project=${listing.key}&open=true (${listing.open.length} rows) and GET /api/v1/issues?project=${listing.key}&status=done (${listing.done.length} rows), production Dispatch`,
          read_at: times[`issues-open-${listing.key}.json`],
          real: [
            "every field of every row the two reads answered",
            sameAsAt(listing, issuesReadAt),
          ],
          derived: [
            `closed_at: null on every row, open and done: the issues' events, which a close time is read from, were not read for ${listing.key}, so no closed window lists its ${listing.done.length} done issues`,
          ],
          invented: [
            `${listing.key} has no Legion setting in the demo (reader.json names ${project} alone), so its views are a project's with no daemon`,
          ],
        },
      ])
    ),
    "population.json": {
      source: `the INFRA Legion daemon's GET /legion/v1/state (${Object.keys(state.issues).length} issues, admission cap ${state.admission?.cap ?? "none"}), read in the cluster; and the events of ${events.size} issues (GET /api/v1/issues/{key}/events), production Dispatch`,
      read_at: daemonReadAt,
      real: [
        "each issue's phase, slot, tree, waitingOn and working claims (the roles), as the state read answered them; not its pull request's checks verdict, which the board of #2731's newest head no longer carries",
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
        `the ${cardCount} cards, their columns, waiting places, slots, slot uses, waiting reasons, roles, blockers and children, and the header's slots in use (${board.header.slots_in_use}), cap (${board.header.slot_cap}), trees (${board.header.trees}) and queue length (${board.header.queue_length})`,
      ],
      derived: ["phase_since, slot_since and the issues map, as population.json says"],
      invented: [
        "computed_at",
        "with the page's demo=read-failed flag on (web/src/demo/install.ts): the newest read's failure and its error text, the cards staying those of the read before it",
      ],
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
    "priorities.json": {
      source:
        priorityNodes.length === 0
          ? "none: production's build has no priorities, and no --priorities file was named, so the pages say no priority is live"
          : `invented by the demo from ${basename(flags.priorities ?? "")} (production's build has no priorities); the moves into backlog or icebox from the issues' events (GET /api/v1/issues/{key}/events), production Dispatch`,
      read_at: null,
      real: [
        `the ${stating.size} issues the relations name and every issue below them: real issues of issues.json, as they stood at the read`,
      ],
      derived: [
        `moved: when ${Object.keys(moved).length} issues standing in backlog or icebox were moved there and by whom, the newest of their events that set that status, as the server's parked fault reads it (${parkedEvents.size} of them read for this, read times in raw/read-times.json); ${parkedUnderNoMove} of the ${parkedUnder} under a priority have no such event and show no move`,
        "every count under a priority (in flight, waiting, in backlog or icebox, closed in 7 days, claimed, open asks) and both faults: computed by the stub (web/src/demo/priorities.ts) from issues.json as GET /api/v1/priorities counts them, so a write moves them",
        "a priority's newest event: the newest updated_at of its work, which a comment alone would not move",
      ],
      invented: [
        `every priority (${priorityNodes.length}): its id, title, owner, description, order, state, made_at and made_by (the reader)`,
        `every advances relation (${relations.length}): which issue states it, when (a minute apart after its priority) and by whom (the reader); the issues below one inherit it`,
        "each priority's history: the writes of its state, title, owner and description at made_at, and each relation at its time",
      ],
    },
  },
};

await writeFile(join(out, "issues.json"), JSON.stringify([...issues, ...noDaemonIssues]));
await writeFile(join(out, "population.json"), JSON.stringify(population));
await writeFile(join(out, "board.json"), JSON.stringify(board));
await writeFile(join(out, "projects.json"), JSON.stringify(projects));
await writeFile(join(out, "schema.json"), JSON.stringify(schema));
await writeFile(join(out, "agents.json"), JSON.stringify(agents));
await writeFile(join(out, "reader.json"), JSON.stringify(readerFixture, null, 2));
await writeFile(join(out, "priorities.json"), JSON.stringify(prioritiesFixture, null, 2));
await writeFile(join(out, "manifest.json"), JSON.stringify(manifest, null, 2));
console.log(
  `wrote ${issues.length} ${project} issues and ${noDaemonIssues.length} of ${noDaemon.join(", ") || "no other project"}, ${cardCount} cards, ${Object.keys(facts).length} facts rows, ${priorityNodes.length} priorities with ${relations.length} relations to ${out}`
);

/** Whether a project's listing, read later than `at`, still holds the rows it held at `at`, the
 *  moment the page's clock stands at: true where no row was written after it. */
function sameAsAt(listing: { open: IssueSummary[]; done: IssueSummary[] }, at: string): string {
  const newest = [...listing.open, ...listing.done].reduce(
    (latest, issue) => (issue.updated_at > latest ? issue.updated_at : latest),
    ""
  );
  return newest !== "" && Date.parse(newest) <= Date.parse(at)
    ? `no row was written after ${at} (the newest updated_at is ${newest}), so the rows are the ones the page's clock time would read`
    : `rows were written after ${at} (the newest updated_at is ${newest}): they show what came after the page's clock time`;
}

function required(value: string | undefined, what: string): string {
  if (value === undefined || value === "") throw new Error(`name ${what}`);
  return value;
}
