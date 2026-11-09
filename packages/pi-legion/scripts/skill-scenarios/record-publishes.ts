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
 * precedes the cell's execution. This records the fact instead: Envoy publishes each accepted
 * message on its topic's NATS subject, so a subscriber of `notifications.role.*` sees exactly what
 * reached a topic, when, and from whom, whatever tool the agent used.
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
 * Each line is `{"at","topic","message","sender","subject"}`: `at` is the time this saw the
 * message, the envelope's `issued_at` when it carries one, `topic` the envelope's own topic,
 * `message` its body (its payload, or its one-line summary when the payload is empty, which is
 * how the listener carries a message of one line), `sender` the session Envoy recorded and
 * `subject` the NATS subject
 * the copy arrived on. Lines are appended in arrival order and never rewritten, so a reader takes
 * the last line of a topic as the message that topic last carried.
 *
 * It prints `recording <out file>` once subscribed, which is what the rig waits for before it
 * boots an agent, and exits on SIGTERM with the file flushed.
 */
import { appendFileSync } from "node:fs";
import { connect } from "nats";

const [outFile, prefix] = Bun.argv.slice(2);
if (outFile === undefined || prefix === undefined) {
  console.error("usage: bun record-publishes.ts <out file> <topic prefix>");
  process.exit(2);
}
const url = process.env.SKILL_SCENARIOS_NATS_URL ?? process.env.ENVOY_NATS_URL;
if (url === undefined || url === "") {
  console.error("record-publishes.ts: no NATS url (SKILL_SCENARIOS_NATS_URL, ENVOY_NATS_URL)");
  process.exit(2);
}

const nats = await connect({ servers: url, name: `skill-scenarios-record-${prefix}` });
const subscription = nats.subscribe("notifications.>");
console.log(`recording ${outFile}`);

const stop = async () => {
  subscription.unsubscribe();
  await nats.drain().catch(() => undefined);
  process.exit(0);
};
process.on("SIGTERM", stop);
process.on("SIGINT", stop);

const ROLE_PREFIX = "notifications.role.";
/** The envelopes already written, by their own id: a forward repeated until its holder's receipt,
 * and a kept copy delivered later, are one message. */
const written = new Set<string>();

for await (const message of subscription) {
  let envelope: Record<string, unknown> = {};
  try {
    const parsed: unknown = JSON.parse(message.string());
    if (typeof parsed === "object" && parsed !== null) envelope = parsed as Record<string, unknown>;
  } catch {
    // A message that is not JSON carries no topic of this run, so it is none of this run's.
    continue;
  }
  const topic = typeof envelope.topic === "string" ? envelope.topic : message.subject;
  if (!topic.startsWith(ROLE_PREFIX) || !topic.slice(ROLE_PREFIX.length).startsWith(prefix)) {
    continue;
  }
  const id =
    typeof envelope.event_id === "string"
      ? envelope.event_id
      : typeof envelope.dedupe_key === "string"
        ? envelope.dedupe_key
        : `${topic}\u0000${message.string()}`;
  if (written.has(id)) continue;
  written.add(id);
  // The message as the listener carries it: a message of one line is its summary and its payload
  // is empty, a longer one is the payload whole (`messageEnvelope`,
  // `packages/envoy/cmd/listener/api.go`).
  const payload = typeof envelope.payload === "string" ? envelope.payload : "";
  const summary = typeof envelope.payload_summary === "string" ? envelope.payload_summary : "";
  const body = payload !== "" ? payload : summary;
  const issued = typeof envelope.issued_at === "number" ? envelope.issued_at : undefined;
  // Who sent it, as the envelope records it: the publishing session where the listener knows one
  // (`source_session`), else its source.
  const sentBy =
    typeof envelope.source_session === "string" && envelope.source_session !== ""
      ? envelope.source_session
      : typeof envelope.source === "string"
        ? envelope.source
        : "";
  appendFileSync(
    outFile,
    `${JSON.stringify({
      at: new Date(issued ?? Date.now()).toISOString(),
      topic,
      message: body,
      sender: sentBy,
      subject: message.subject,
    })}\n`
  );
}
