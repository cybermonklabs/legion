/**
 * A fake Legion daemon that pushes change events to Dispatch's POST /api/v1/legion/events
 * (dispatch://INFRA-979 prototype). It replays a saved `GET /legion/v1/state` body as it is: a
 * boot snapshot, then a heartbeat every 60 s, and nothing moves on its own, so the page reads the
 * replayed daemon's slots, turns and phases. `--simulate <moves per minute>` (default 0) shows
 * live updates on demand: it moves issues the replay shows running one step through the running
 * phases (never to done), starts and ends their turns (never one on a root held for its
 * children), changes a checks verdict and turns the queue, each batch in one POST. The state and
 * the simulation are legion-fake-daemon-model.ts.
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
 *     --token-file <f> --project INFRA --replay <state.json> [--interval 3] [--simulate 0] \
 *     [--drop-every N] \
 *     [--keep N] [--heartbeat 60] [--listen <port>] [--session <uuid>] [--history-dsn <dsn>]
 *
 * --history-dsn reads each issue's phase entry time from a local Dispatch's imported history (its
 * last status change into the phase's status, or the handoff that starts the phase), and each
 * issue's parent, which tells --simulate a root that is held for its children; a phase no record
 * places is sent with `at_unknown`, which the page shows as no time in phase. It reads only the
 * records the replayed daemon could have seen: none later than the replay (--replay-at, by default
 * the replay file's modification time) and none of the fake daemon's own session, so a restart
 * reads the same entry times as the first boot.
 *
 * --drop-every N gives every Nth change event a seq but never sends it the first time, so the
 * gap repair is visible. --keep N keeps only the last N change events, so a gap older than them
 * takes a snapshot.
 */
import { statSync } from "node:fs";
import { parseArgs } from "node:util";

import { SQL } from "bun";

import {
  type DaemonState,
  FakeDaemonModel,
  type HistoryRow,
  historyMarks,
  type PushEvent,
  phaseEntryTimes,
} from "./legion-fake-daemon-model";

const { values: args } = parseArgs({
  options: {
    dispatch: { type: "string" },
    "token-file": { type: "string" },
    project: { type: "string" },
    replay: { type: "string" },
    interval: { type: "string", default: "3" },
    // Simulated moves a minute: 0, the default, keeps the replayed state as it is.
    simulate: { type: "string", default: "0" },
    "drop-every": { type: "string", default: "0" },
    keep: { type: "string", default: "100000" },
    heartbeat: { type: "string", default: "60" },
    listen: { type: "string" },
    // A local Dispatch's database holding the imported history: each issue's phase entry time
    // is read from its status changes and handoffs there (phaseEntryTimes).
    "history-dsn": { type: "string" },
    // When the replay was read: no record later than this is an entry time.
    "replay-at": { type: "string" },
    // The shared token needs a session actor, as the daemon's own status writes carry.
    session: { type: "string", default: "019a1c09-0000-7000-8000-00000000da01" },
  },
});
const required = (name: "dispatch" | "token-file" | "project" | "replay"): string => {
  const value = args[name];
  if (!value) throw new Error(`--${name} is required`);
  return value;
};
const dispatchUrl = required("dispatch").replace(/\/$/, "");
const token = (await Bun.file(required("token-file")).text()).trim();
const project = required("project");
const replayPath = required("replay");
const intervalMs = Number(args.interval) * 1000;
const heartbeatMs = Number(args.heartbeat) * 1000;
const dropEvery = Number(args["drop-every"]);
const keepLimit = Number(args.keep);
const movesPerMinute = Number(args.simulate);

const replay = (await Bun.file(replayPath).json()) as DaemonState;
const replayAt = args["replay-at"] ?? statSync(replayPath).mtime.toISOString();
const session = args.session as string;

/** Each issue's phase entry time, and each issue's parent, from the imported history where
 *  --history-dsn names it. */
async function fromHistory(
  dsn: string
): Promise<{ entryTimes: Map<string, string>; parents: Map<string, string> }> {
  const db = new SQL(dsn);
  const parents = new Map<string, string>();
  for (const row of (await db.unsafe(
    `select key, parent_key from issues where project_key = $1 and parent_key is not null`,
    [project]
  )) as { key: string; parent_key: string }[]) {
    parents.set(row.key, row.parent_key);
  }
  const keys = Object.keys(replay.issues);
  const rows = (kind: "status" | "handoff", sql: string) =>
    db
      .unsafe(sql, [keys])
      .then((found: { key: string; at: Date; value: string; actor: string | null }[]) =>
        found.map((row): HistoryRow => ({ ...row, kind, at: row.at.toISOString() }))
      );
  const { statuses, handoffs } = historyMarks(
    [
      ...(await rows(
        "status",
        `select issue_key as key, created_at as at, payload->>'status' as value, actor->>'id' as actor
         from events where issue_key = any($1) and payload->>'status' is not null
         order by created_at, id`
      )),
      ...(await rows(
        "handoff",
        `select issue_key as key, created_at as at, phase as value, author->>'id' as actor
         from handoffs where issue_key = any($1) order by created_at`
      )),
    ],
    { replayAt, self: session }
  );
  await db.close();
  const phases = new Map(
    Object.values(replay.issues).flatMap((issue) =>
      issue.phase === undefined ? [] : [[issue.key, issue.phase] as const]
    )
  );
  return { entryTimes: phaseEntryTimes(phases, statuses, handoffs), parents };
}

const { entryTimes, parents } = args["history-dsn"]
  ? await fromHistory(args["history-dsn"])
  : { entryTimes: new Map<string, string>(), parents: new Map<string, string>() };
console.log(
  `from the history: phase entry times of ${entryTimes.size} issues, parents of ${parents.size}`
);
const model = new FakeDaemonModel(replay, {
  entryTimes,
  parents,
  dropEvery,
  keep: keepLimit,
  log: (line) => console.log(line),
});
const state = model.state;
const generation = `fake-${Date.now()}`;
/** Where the next delivery resends kept events from, when Dispatch answered a gap. */
let resendFrom: number | null = null;
let needSnapshot = true;

/** One POST: a snapshot when one is owed, else the kept events from a gap, else the unsent ones. */
async function deliver(heartbeat: boolean): Promise<void> {
  let events: PushEvent[];
  let label = "";
  const snapshot = needSnapshot;
  if (snapshot) {
    events = model.snapshotEvents();
    label = " (snapshot)";
  } else if (resendFrom !== null) {
    const from = resendFrom;
    events = model.kept.filter((event) => event.seq >= from);
    label = ` (resent from seq ${from})`;
  } else {
    events = model.unsent;
  }
  model.unsent = [];
  if (heartbeat) {
    events = [
      ...events,
      { seq: 0, at: new Date().toISOString(), kind: "heartbeat", value: { last_seq: model.seq } },
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
  if (answer.last_seq < model.seq) {
    const oldest = model.kept[0]?.seq;
    if (oldest !== undefined && answer.last_seq + 1 >= oldest) {
      resendFrom = answer.last_seq + 1;
      next = ` -> resend from seq ${resendFrom}`;
    } else {
      needSnapshot = true;
      next = ` -> gap older than the ${model.kept.length} kept events (oldest seq ${oldest ?? "-"}): snapshot`;
    }
  }
  console.log(
    `${new Date().toISOString()} sent ${events.length} events${label}${heartbeat ? " +heartbeat" : ""}: ` +
      `last_seq ours ${model.seq}, Dispatch ${answer.last_seq}${next}` +
      (model.dropped > 0 ? `, ${model.dropped} dropped so far` : "")
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
    verdicts[path] = model.applySetting(path, value, change.by ?? null);
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
console.log(
  movesPerMinute > 0
    ? `simulating ${movesPerMinute} moves a minute`
    : "replaying the state as it is: no simulated moves (--simulate <moves per minute>)"
);
let lastHeartbeat = Date.now();
let busy = false;
/** The fraction of a move owed: --simulate's rate spread over the ticks. */
let owed = 0;
await deliver(false);
setInterval(async () => {
  if (busy) return;
  busy = true;
  try {
    if (movesPerMinute > 0 && !needSnapshot && resendFrom === null) {
      owed += (movesPerMinute * intervalMs) / 60_000;
      const moves = Math.floor(owed);
      owed -= moves;
      if (moves > 0) model.simulate(moves);
    }
    const heartbeat = Date.now() - lastHeartbeat >= heartbeatMs;
    if (heartbeat) lastHeartbeat = Date.now();
    await deliver(heartbeat);
  } finally {
    busy = false;
  }
}, intervalMs);
