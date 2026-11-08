/**
 * A fake Legion daemon that pushes change events to Dispatch's POST /api/v1/legion/events
 * (dispatch://INFRA-979 prototype). It replays a saved `GET /legion/v1/state` body: a boot
 * snapshot, then every few seconds it moves a few issues through phases, starts and ends turns,
 * changes a checks verdict and reorders the queue, sending each batch in one POST. A heartbeat
 * goes every 60 s, and a full snapshot whenever Dispatch's last_seq differs from its own.
 *
 *   bun run packages/dispatch/scripts/legion-fake-daemon.ts --dispatch http://127.0.0.1:<port> \
 *     --token-file <f> --project INFRA --replay <state.json> [--interval 3] [--drop-every N]
 *     [--heartbeat 60] [--session <uuid>]
 *
 * --drop-every N gives every Nth change event a seq but never sends it, so the gap repair is
 * visible: Dispatch's last_seq stays below the gap, and the next answer brings a snapshot.
 */
import { parseArgs } from "node:util";

const { values: args } = parseArgs({
  options: {
    dispatch: { type: "string" },
    "token-file": { type: "string" },
    project: { type: "string" },
    replay: { type: "string" },
    interval: { type: "string", default: "3" },
    "drop-every": { type: "string", default: "0" },
    heartbeat: { type: "string", default: "60" },
    // The shared token needs a session actor, as the daemon's own status writes carry.
    session: { type: "string", default: "019a1c09-0000-7000-8000-00000000da01" },
  },
});
for (const required of ["dispatch", "token-file", "project", "replay"] as const) {
  if (!args[required]) throw new Error(`--${required} is required`);
}
const dispatchUrl = args.dispatch!.replace(/\/$/, "");
const token = (await Bun.file(args["token-file"]!).text()).trim();
const project = args.project!;
const intervalMs = Number(args.interval) * 1000;
const heartbeatMs = Number(args.heartbeat) * 1000;
const dropEvery = Number(args["drop-every"]);

interface DaemonIssue {
  key: string;
  phase?: string;
  slot?: { index: number } | null;
  waitingOn?: unknown;
  architect?: { state?: string };
  workers?: Record<string, { claim?: { state?: string } }>;
  pullRequest?: { checksVerdict?: string };
}
interface DaemonState {
  issues: Record<string, DaemonIssue>;
  admission?: { waiting?: string[]; [key: string]: unknown };
  settings?: Record<string, unknown>;
  settingSources?: Record<string, unknown> | null;
}

interface PushEvent {
  seq: number;
  at: string;
  kind: string;
  issue?: string;
  value: unknown;
}

const state = (await Bun.file(args.replay!).json()) as DaemonState;
const generation = `fake-${Date.now()}`;
let seq = 0;
let dropped = 0;
/** Change events made so far, which --drop-every counts; a snapshot's events are never dropped. */
let changes = 0;
let pending: PushEvent[] = [];
let needSnapshot = true;

/** The phases the simulation walks an issue through, and the role in a turn in each. */
const PHASES = [
  "admitted",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
  "merging",
  "awaiting_merge",
  "done",
];
const ROLE: Record<string, string> = {
  planning: "planner",
  implementing: "implementer",
  testing: "tester",
  reviewing: "reviewer",
  accepting: "architect",
  retro: "implementer",
  merging: "merger",
  awaiting_merge: "merger",
};
const VERDICTS = ["green", "pending", "failing"];

function setClaim(issue: DaemonIssue, role: string, claim: string): void {
  if (role === "architect") issue.architect = { ...issue.architect, state: claim };
  else {
    issue.workers ??= {};
    issue.workers[role] = { ...issue.workers[role], claim: { state: claim } };
  }
}

/** A new event with the next seq, at now. A dropped one takes its seq and is never sent. */
function emit(kind: string, value: unknown, issue?: string): void {
  seq += 1;
  const event: PushEvent = { seq, at: new Date().toISOString(), kind, value };
  if (issue !== undefined) event.issue = issue;
  changes += 1;
  if (dropEvery > 0 && changes % dropEvery === 0) {
    dropped += 1;
    console.log(`  dropped seq ${seq} (${kind} ${issue ?? ""})`);
    return;
  }
  pending.push(event);
}

/** Every current value as events with fresh seqs: what a boot or a repair sends. */
function snapshotEvents(): PushEvent[] {
  const events: PushEvent[] = [];
  const add = (kind: string, value: unknown, issue?: string) => {
    seq += 1;
    const event: PushEvent = { seq, at: new Date().toISOString(), kind, value };
    if (issue !== undefined) event.issue = issue;
    events.push(event);
  };
  for (const issue of Object.values(state.issues)) {
    if (issue.phase) add("phase", issue.phase, issue.key);
    if (issue.slot) add("slot", issue.slot.index, issue.key);
    if (issue.waitingOn) add("waiting_on", issue.waitingOn, issue.key);
    if (issue.pullRequest?.checksVerdict)
      add("checks_verdict", issue.pullRequest.checksVerdict, issue.key);
    if (issue.architect?.state)
      add("claim_state", { role: "architect", state: issue.architect.state }, issue.key);
    for (const [role, worker] of Object.entries(issue.workers ?? {})) {
      if (worker.claim?.state) add("claim_state", { role, state: worker.claim.state }, issue.key);
    }
  }
  if (state.admission) add("waiting_line", state.admission);
  for (const [key, value] of Object.entries(state.settings ?? {})) {
    add("setting", { key, value, source: state.settingSources?.[key] ?? { source: "file" } });
  }
  return events;
}

/** One tick of simulated work. */
function simulate(): void {
  const moving = Object.values(state.issues).filter(
    (issue) => issue.phase !== undefined && issue.phase !== "done" && issue.phase !== "admitted"
  );
  for (let n = 0; n < 3 && moving.length > 0; n++) {
    const issue = moving.splice(Math.floor(Math.random() * moving.length), 1)[0]!;
    const from = issue.phase!;
    const to = PHASES[Math.min(PHASES.indexOf(from) + 1, PHASES.length - 1)]!;
    const ended = ROLE[from];
    const endedClaim =
      ended === "architect" ? issue.architect?.state : issue.workers?.[ended ?? ""]?.claim?.state;
    if (ended && endedClaim === "working") {
      setClaim(issue, ended, "idle");
      emit("claim_state", { role: ended, state: "idle" }, issue.key);
    }
    issue.phase = to;
    emit("phase", to, issue.key);
    const started = ROLE[to];
    if (started) {
      setClaim(issue, started, "working");
      emit("claim_state", { role: started, state: "working" }, issue.key);
    }
    if (to === "done" && issue.slot) {
      issue.slot = null;
      emit("slot", null, issue.key);
    }
    console.log(`  ${issue.key}: ${from} -> ${to}`);
  }
  const withPull = Object.values(state.issues).filter(
    (issue) => issue.pullRequest && issue.phase !== "done"
  );
  const pull = withPull[Math.floor(Math.random() * withPull.length)];
  if (pull?.pullRequest) {
    const verdict = VERDICTS[(VERDICTS.indexOf(pull.pullRequest.checksVerdict ?? "") + 1) % 3]!;
    pull.pullRequest.checksVerdict = verdict;
    emit("checks_verdict", verdict, pull.key);
    console.log(`  ${pull.key}: checks ${verdict}`);
  }
  const waiting = state.admission?.waiting;
  if (waiting && waiting.length > 1) {
    waiting.unshift(waiting.pop()!);
    emit("waiting_line", state.admission);
    console.log(`  queue: ${waiting[0]} moved to the front`);
  }
}

/** One POST of every pending event; a snapshot first when one is owed. */
async function deliver(heartbeat: boolean): Promise<void> {
  let events = pending;
  let snapshot = false;
  if (needSnapshot) {
    events = snapshotEvents();
    snapshot = true;
  }
  if (heartbeat) {
    events = [
      ...events,
      { seq: 0, at: new Date().toISOString(), kind: "heartbeat", value: { last_seq: seq } },
    ];
  }
  if (events.length === 0) return;
  let response: Response;
  try {
    response = await fetch(`${dispatchUrl}/api/v1/legion/events`, {
      method: "POST",
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
      body: JSON.stringify({
        project,
        generation,
        events,
        snapshot,
        actor: { kind: "session", id: args.session },
      }),
    });
  } catch (error) {
    console.log(`POST failed (${String(error)}); kept ${pending.length} events`);
    return;
  }
  const body = await response.text();
  if (!response.ok) {
    console.log(`POST ${response.status}: ${body}; a snapshot follows`);
    needSnapshot = true;
    pending = [];
    return;
  }
  const answer = JSON.parse(body) as { last_seq: number };
  pending = [];
  needSnapshot = answer.last_seq !== seq;
  console.log(
    `${new Date().toISOString()} sent ${events.length} events${snapshot ? " (snapshot)" : ""}${heartbeat ? " +heartbeat" : ""}: ` +
      `last_seq ours ${seq}, Dispatch ${answer.last_seq}${needSnapshot ? " -> snapshot next" : ""}` +
      (dropped > 0 ? `, ${dropped} dropped so far` : "")
  );
}

console.log(
  `fake Legion daemon for ${project}: ${Object.keys(state.issues).length} issues, generation ${generation}`
);
let lastHeartbeat = Date.now();
let busy = false;
await deliver(false);
setInterval(async () => {
  if (busy) return;
  busy = true;
  try {
    if (!needSnapshot) simulate();
    const heartbeat = Date.now() - lastHeartbeat >= heartbeatMs;
    if (heartbeat) lastHeartbeat = Date.now();
    await deliver(heartbeat);
  } finally {
    busy = false;
  }
}, intervalMs);
