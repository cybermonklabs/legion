/**
 * record-publishes.ts <out file> <topic prefix> — every message Envoy accepted on a role topic of
 * one run, as the servers saw it, one JSON line each.
 *
 * The scenarios of this directory score who told whom (score.ts's `publishedTo` and the rig's
 * `reviewer_wake`), and until 2026-11-09 both worked that out by reading the agent's own code:
 * the arguments of an `envoy_publish` tool call, the JSON of a write to `xd://envoy_publish`, and
 * then an evaluator over an `eval` cell's source. A reader of intent is wrong in both directions
 * — it passed a run on a message the agent wrote and never sent, and failed one whose message was
 * sent in a shape it could not read — and it timed a cell by the call's own timestamp, which
 * precedes the cell's execution. This records the fact instead: a subscriber of the bus sees
 * exactly what reached a topic, when, and from whom, whatever tool the agent used.
 *
 * What it subscribes to is `notifications.>` rather than the role subjects themselves, because a
 * role message with a live holder never travels on its role subject: the publish route resolves
 * the holder and forwards the envelope to that session's own agent subject
 * (`contracts.AgentSubject`, `packages/envoy/cmd/listener/confirmed.go`), repeating the forward
 * until the holder's receipt, and one with no live holder is stored on the role's kept subject
 * (`bus.KeptRoleSubject`, `role_keep.go`). Every one of those copies keeps the envelope's own
 * `topic`, which stays the role topic, so the topic is read from the envelope and the subject is
 * only where it was seen. A message is recorded once: each repeat of a forward, and the kept copy
 * a drain later delivers, carries the envelope's own id, and a copy of an id already written is
 * passed over.
 *
 * The file is a record of the recorder's own run as much as of the run's messages, because a
 * reader must be able to tell a run that published nothing from one this never saw. It opens with
 * `{"at","recording"}`, written once the subscription is registered with the server, and ends
 * with `{"at","closed"}`, written when the rig stops it. A file without both is a rig error
 * (score.ts's `unscored`), so a recorder that died mid-run — its connection lost, its process
 * killed — scores no run at all rather than scoring every message it missed as a message never
 * sent. Between them each message is `{"at","topic","message","sender","subject"}`: `at` is the
 * envelope's `issued_at`, `topic` its own topic, `message` its body (its payload, or its one-line
 * summary when the payload is empty, which is how the listener carries a message of one line),
 * `sender` the session Envoy recorded and `subject` the NATS subject the copy arrived on. An
 * envelope that is not one of this run's role messages is passed over, and one that reaches a
 * role topic of this run that this cannot read, or cannot attribute to a session, is never an act
 * an agent did not do: it is logged and written as
 * `{"at","unreadable","subject"}`, which makes every run of the record a rig error.
 */
import { appendFileSync } from "node:fs";
import { connect } from "nats";
import { z } from "zod";

const [outFile, prefix] = Bun.argv.slice(2);
if (outFile === undefined || prefix === undefined) {
  console.error("usage: bun record-publishes.ts <out file> <topic prefix>");
  process.exit(2);
}
const url = process.env.SKILL_SCENARIOS_NATS_URL;
if (url === undefined || url === "") {
  console.error("record-publishes.ts: no NATS url (SKILL_SCENARIOS_NATS_URL)");
  process.exit(2);
}

/** What the listener puts on the bus, as far as this reads it: the fields every envelope of a
 * message carries (`contracts.Envelope`, `messageEnvelope` in
 * `packages/envoy/cmd/listener/api.go`, where `payload_summary` is written for every message and
 * `payload` only for one of more than one line). A frame that is not a message — a receipt, a
 * cursor, a Dispatch event — fails this parse and is passed over; one on a role topic of this run
 * that fails it is written into the record as `unreadable`, which makes the run a rig error
 * (score.ts's `unscored`), because a message this cannot read is this reader's limit and never an
 * act an agent did not do. `source_session` is read alone and never `source`: the two are
 * different kinds — `source` is `agent` for every agent's publish — so a message with no session
 * would otherwise carry a sender that belongs to no half. Such a publish is one Envoy accepts
 * (`SourceSession` carries no `omitempty`, and `packages/envoy/AGENTS.md` says a message without
 * it acts as no session and is not refused), so it is no defect of the listener: it is a message
 * this rig cannot attribute, and every message a scenario scores must be attributable, which is
 * why it is written as `unreadable` rather than scored. */
const Envelope = z.looseObject({
  event_id: z.string().min(1),
  topic: z.string().min(1),
  issued_at: z.number(),
  payload_summary: z.string(),
  payload: z.string().optional(),
  source_session: z.string().min(1),
});

const line = (value: Record<string, unknown>) =>
  appendFileSync(outFile, `${JSON.stringify(value)}\n`);
const now = () => new Date().toISOString();

// No reconnect: core NATS loses whatever was published while a connection is down, so a drop
// must end this process rather than leave it recording a record with a hole in it. The loop then
// ends with no closing line and every run of the record is a rig error, which is the one answer
// that is true — what was sent in the gap is unknown.
const nats = await connect({
  servers: url,
  name: `skill-scenarios-record-${prefix}`,
  reconnect: false,
});
const subscription = nats.subscribe("notifications.>");
// The flush is what makes the next line true: a subscription is registered with the server when
// the server has answered, and without this the rig could boot an agent whose first message this
// has not yet asked for.
await nats.flush();
line({ at: now(), recording: prefix });
console.log(`recording ${outFile}`);

/** The rig's stop, which is the only end this file is written for: the closing line says the
 * record is whole. The rig sends TERM and waits (rig.sh's stop_run), so this runs. */
let closing = false;
const stop = () => {
  if (closing) return;
  closing = true;
  line({ at: now(), closed: "stopped" });
  subscription.unsubscribe();
  nats.close().finally(() => process.exit(0));
};
process.on("SIGTERM", stop);
process.on("SIGINT", stop);

const ROLE_PREFIX = "notifications.role.";
/** The envelopes already written, by their own id: a forward repeated until its holder's receipt,
 * and a kept copy delivered later, are one message. */
const written = new Set<string>();

for await (const message of subscription) {
  let parsed: unknown;
  try {
    parsed = JSON.parse(message.string());
  } catch {
    continue;
  }
  // The topic is read before the parse, so a frame that is none of this run's business is passed
  // over in silence and one of this run's that cannot be read is reported.
  const topic =
    typeof parsed === "object" && parsed !== null && "topic" in parsed ? parsed.topic : undefined;
  if (typeof topic !== "string") continue;
  if (!topic.startsWith(ROLE_PREFIX) || !topic.slice(ROLE_PREFIX.length).startsWith(prefix)) {
    continue;
  }
  const envelope = Envelope.safeParse(parsed);
  if (!envelope.success) {
    console.error(
      `record-publishes.ts: ${message.subject} carries a ${topic} message this cannot read: ${envelope.error.message}`
    );
    line({ at: now(), unreadable: topic, subject: message.subject });
    continue;
  }
  const { event_id, issued_at, payload, payload_summary, source_session } = envelope.data;
  if (written.has(event_id)) continue;
  written.add(event_id);
  line({
    at: new Date(issued_at).toISOString(),
    topic,
    // A message of one line is its summary and its payload is empty; a longer one is the payload
    // whole (`messageEnvelope`, `packages/envoy/cmd/listener/api.go`).
    message: payload !== undefined && payload !== "" ? payload : payload_summary,
    sender: source_session,
    subject: message.subject,
  });
}

// The loop ended. A stop ended it on purpose and `stop` has written the closing line and is
// closing the connection, so this exits with it; anything else — the connection dropped, the
// server closed it — leaves no closing line, so the record is unfinished and every run that
// reads it is a rig error rather than a run that published nothing.
if (!closing) {
  const failure = await nats.closed();
  console.error(
    `record-publishes.ts: the subscription ended before the run did${failure === undefined ? "" : `: ${failure}`}`
  );
  process.exit(1);
}
