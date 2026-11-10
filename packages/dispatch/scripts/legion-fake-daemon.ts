/**
 * A fake Legion daemon that pushes change events to Dispatch's POST /api/v1/legion/events
 * (dispatch://INFRA-979 prototype). It replays a saved `GET /legion/v1/state` body: a boot
 * snapshot, then every few seconds it moves a few running issues one step through the running
 * phases (never to done, so the slots, phases and roles hold steady), starts and ends turns,
 * changes a checks verdict and reorders the queue, sending each batch in one POST, and a heartbeat
 * every 60 s. The state and the simulation are legion-fake-daemon-model.ts.
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

import { type DaemonState, FakeDaemonModel, type PushEvent } from "./legion-fake-daemon-model";

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

const model = new FakeDaemonModel((await Bun.file(replayPath).json()) as DaemonState, {
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
let lastHeartbeat = Date.now();
let busy = false;
await deliver(false);
setInterval(async () => {
  if (busy) return;
  busy = true;
  try {
    if (!needSnapshot && resendFrom === null) model.simulate();
    const heartbeat = Date.now() - lastHeartbeat >= heartbeatMs;
    if (heartbeat) lastHeartbeat = Date.now();
    await deliver(heartbeat);
  } finally {
    busy = false;
  }
}, intervalMs);
