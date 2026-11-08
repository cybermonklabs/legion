/**
 * A fake Legion daemon that pushes change events to Dispatch's POST /api/v1/legion/events
 * (dispatch://INFRA-979 prototype). It replays a saved `GET /legion/v1/state` body: a boot
 * snapshot, then every few seconds it moves a few issues through phases, starts and ends turns,
 * changes a checks verdict and reorders the queue, sending each batch in one POST, and a heartbeat
 * every 60 s.
 *
 * It keeps every change event it made, as the daemon's outbox does. When Dispatch answers a
 * last_seq below its own, it resends its kept events from last_seq+1. It sends a full snapshot
 * only at boot, or when last_seq is older than what it kept. Every event, a snapshot's included,
 * carries the time its value changed.
 *
 * With --listen, it also answers the daemon's settings route (PUT /legion/v1/operator/settings),
 * which Dispatch relays a Settings-page change to: it applies the change and pushes the setting.
 *
 *   bun run packages/dispatch/scripts/legion-fake-daemon.ts --dispatch http://127.0.0.1:<port> \
 *     --token-file <f> --project INFRA --replay <state.json> [--interval 3] [--drop-every N] \
 *     [--keep N] [--heartbeat 60] [--listen <port>] [--session <uuid>]
 *
 * --drop-every N gives every Nth change event a seq but never sends it the first time, so the
 * gap repair is visible. --keep N keeps only the last N change events, so a gap older than them
 * takes a snapshot.
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
    keep: { type: "string", default: "100000" },
    heartbeat: { type: "string", default: "60" },
    listen: { type: "string" },
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
const keepLimit = Number(args.keep);

interface DaemonIssue {
  key: string;
  phase?: string;
  slot?: { index: number; admittedAt?: string } | null;
  waitingOn?: { since?: string; answeredAt?: string } | null;
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
const sources: Record<string, unknown> = { ...(state.settingSources ?? {}) };
const generation = `fake-${Date.now()}`;
const bootAt = new Date().toISOString();
let seq = 0;
let dropped = 0;
/** Change events made so far, which --drop-every counts; a snapshot's events are never dropped. */
let changes = 0;
/** Every change event made, oldest first, up to --keep: the outbox a resend reads. */
const kept: PushEvent[] = [];
/** The change events not yet sent, a dropped one aside. */
let unsent: PushEvent[] = [];
/** Where the next delivery resends kept events from, when Dispatch answered a gap. */
let resendFrom: number | null = null;
let needSnapshot = true;
/** When each value last changed, by `kind|issue|role or setting`: a snapshot's `at`. A value the
 *  replay carries no time for changed at boot. */
const changedAt = new Map<string, string>();

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

for (const issue of Object.values(state.issues)) {
  if (issue.slot?.admittedAt) changedAt.set(`slot|${issue.key}|`, issue.slot.admittedAt);
  const waited = issue.waitingOn?.answeredAt ?? issue.waitingOn?.since;
  if (waited) changedAt.set(`waiting_on|${issue.key}|`, waited);
}

function setClaim(issue: DaemonIssue, role: string, claim: string): void {
  if (role === "architect") issue.architect = { ...issue.architect, state: claim };
  else {
    issue.workers ??= {};
    issue.workers[role] = { ...issue.workers[role], claim: { state: claim } };
  }
}

/** A change event with the next seq, at now, kept in the outbox. A dropped one is never sent the
 *  first time; a resend carries it. */
function emit(kind: string, value: unknown, issue?: string, sub = ""): void {
  seq += 1;
  const at = new Date().toISOString();
  changedAt.set(`${kind}|${issue ?? ""}|${sub}`, at);
  const event: PushEvent = { seq, at, kind, value };
  if (issue !== undefined) event.issue = issue;
  kept.push(event);
  if (kept.length > keepLimit) kept.shift();
  changes += 1;
  if (dropEvery > 0 && changes % dropEvery === 0) {
    dropped += 1;
    console.log(`  dropped seq ${seq} (${kind} ${issue ?? ""}) at ${at}`);
    return;
  }
  unsent.push(event);
}

/** Every current value as events with fresh seqs, each at the time its value changed: what a
 *  boot, or a gap older than the outbox, sends. */
function snapshotEvents(): PushEvent[] {
  const events: PushEvent[] = [];
  const add = (kind: string, value: unknown, issue?: string, sub = "") => {
    seq += 1;
    const at = changedAt.get(`${kind}|${issue ?? ""}|${sub}`) ?? bootAt;
    const event: PushEvent = { seq, at, kind, value };
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
  if (state.admission) add("waiting_line", state.admission);
  for (const [key, value] of Object.entries(state.settings ?? {})) {
    add(
      "setting",
      { key, value, source: sources[key] ?? { source: "file", at: bootAt } },
      undefined,
      key
    );
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
      emit("claim_state", { role: ended, state: "idle" }, issue.key, ended);
    }
    issue.phase = to;
    emit("phase", to, issue.key);
    const started = ROLE[to];
    if (started) {
      setClaim(issue, started, "working");
      emit("claim_state", { role: started, state: "working" }, issue.key, started);
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

/** One POST: a snapshot when one is owed, else the kept events from a gap, else the unsent ones. */
async function deliver(heartbeat: boolean): Promise<void> {
  let events: PushEvent[];
  let label = "";
  const snapshot = needSnapshot;
  if (snapshot) {
    events = snapshotEvents();
    label = " (snapshot)";
  } else if (resendFrom !== null) {
    const from = resendFrom;
    events = kept.filter((event) => event.seq >= from);
    label = ` (resent from seq ${from})`;
  } else {
    events = unsent;
  }
  unsent = [];
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
    // The outbox still holds every event: the next delivery resends from the first one here.
    const first = events.find((event) => event.kind !== "heartbeat");
    if (!snapshot && first !== undefined) resendFrom = Math.min(resendFrom ?? first.seq, first.seq);
    console.log(`POST failed (${String(error)}); resending from seq ${resendFrom ?? "-"}`);
    return;
  }
  const body = await response.text();
  if (!response.ok) {
    console.log(`POST ${response.status}: ${body}; a snapshot follows`);
    needSnapshot = true;
    return;
  }
  const answer = JSON.parse(body) as { last_seq: number };
  needSnapshot = false;
  resendFrom = null;
  let next = "";
  if (answer.last_seq < seq) {
    const oldest = kept[0]?.seq;
    if (oldest !== undefined && answer.last_seq + 1 >= oldest) {
      resendFrom = answer.last_seq + 1;
      next = ` -> resend from seq ${resendFrom}`;
    } else {
      needSnapshot = true;
      next = ` -> gap older than the ${kept.length} kept events (oldest seq ${oldest ?? "-"}): snapshot`;
    }
  }
  console.log(
    `${new Date().toISOString()} sent ${events.length} events${label}${heartbeat ? " +heartbeat" : ""}: ` +
      `last_seq ours ${seq}, Dispatch ${answer.last_seq}${next}` +
      (dropped > 0 ? `, ${dropped} dropped so far` : "")
  );
}

/** The daemon's settings route: apply each change, record the route as its source, and push it. */
async function answerSettings(request: Request): Promise<Response> {
  const url = new URL(request.url);
  if (request.method !== "PUT" || url.pathname !== "/legion/v1/operator/settings") {
    return new Response("{}", { status: 404 });
  }
  const change = (await request.json()) as { changes?: Record<string, unknown>; by?: string };
  const verdicts: Record<string, { applied?: string; refused?: string }> = {};
  for (const [path, value] of Object.entries(change.changes ?? {})) {
    const [top, ...rest] = path.split(".");
    if (!top || !(top in (state.settings ?? {}))) {
      verdicts[path] = { refused: "no such setting" };
      continue;
    }
    if (rest.length === 0) state.settings![top] = value;
    else {
      let node = state.settings![top] as Record<string, unknown>;
      for (const part of rest.slice(0, -1)) {
        node[part] ??= {};
        node = node[part] as Record<string, unknown>;
      }
      node[rest.at(-1)!] = value;
    }
    const source = { source: "route", by: change.by ?? null, at: new Date().toISOString() };
    sources[top] = source;
    emit("setting", { key: top, value: state.settings![top], source }, undefined, top);
    verdicts[path] = { applied: "at-once" };
    console.log(`  setting ${path} = ${JSON.stringify(value)} by ${change.by ?? "?"}`);
  }
  return Response.json({ settings: verdicts });
}

if (args.listen) {
  Bun.serve({ port: Number(args.listen), hostname: "127.0.0.1", fetch: answerSettings });
  console.log(`settings route on http://127.0.0.1:${args.listen}`);
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
    if (!needSnapshot && resendFrom === null) simulate();
    const heartbeat = Date.now() - lastHeartbeat >= heartbeatMs;
    if (heartbeat) lastHeartbeat = Date.now();
    await deliver(heartbeat);
  } finally {
    busy = false;
  }
}, intervalMs);
