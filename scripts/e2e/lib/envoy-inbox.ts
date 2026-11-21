// scripts/e2e/lib/envoy-inbox.ts: the inbox of a live proof's stand-in sessions, a core NATS
// subscriber of each one's relay subject. A session is delivered its messages from its own
// durable consumer, which the listener holding its registration relays as core frames on
// `notifications.envoy.deliver.<session>`, and from no plain subscription of the session's own
// ("Session consumers" in packages/envoy/AGENTS.md). So a proof that registers a stand-in with a
// consumer (`delivery.consumer`) reads here what the stand-in is delivered: the answers to its
// own sends, which the listener stores on its agent subject, and, for a stand-in holding a
// project's controller role, the notices on that project's controller lane, which its
// registration then stores and its consumer then carries.
//
//   bun scripts/e2e/lib/envoy-inbox.ts --nats <url> --record <file.jsonl> \
//     --watch <session>=<subject>[,<subject>] [--watch <session>=<subject>…]
//
// Each envelope that lands on a watched subject is one JSON line appended to --record: when it
// came, the session the subject was watched for, and its text as a session's deliverer renders it —
// `In Reply To: <event id>` when the envelope answers one, `Message:`, the payload (its summary
// when it carries no payload), a blank line and `Topic: <topic>`. An envelope whose dedupe key was
// already recorded on its topic is left out, so a proof counting the answers to one question counts
// deliveries, not the copies a redelivery makes.
//
// A relay frame carries a reply subject, and the inbox answers it `done` once the frame is
// recorded, or left out as a repeat or as no envelope: the listener acknowledges the message to
// the session's consumer on that word, and a frame left unanswered pauses the relay and comes
// again ("Relay over NATS" in packages/envoy/AGENTS.md). The listener's connection probe on the
// same subject (`Envoy-Probe: 1`, no body) is answered `done` before anything is read of it, and
// recorded as nothing, as a plugin answers it (RelayHeader.probe in @legion/envoy-client). A frame
// whose record could not be written is not answered, so its consumer offers it again.
//
// It prints `envoy-inbox listening` once every subscription is on the wire.
import { appendFileSync } from "node:fs";
import { connect } from "nats";

/** The fields of an Envoy envelope this inbox reads (internal/contracts.Envelope). */
export interface InboxEnvelope {
  topic?: string;
  dedupe_key?: string;
  in_reply_to?: string;
  payload?: string;
  payload_summary?: string;
}

/** An envelope's text as a session's deliverer renders it for the session it reaches. */
export function renderDelivery(envelope: InboxEnvelope): string {
  const inReplyTo = envelope.in_reply_to ? `In Reply To: ${envelope.in_reply_to}\n` : "";
  const message = envelope.payload || envelope.payload_summary || "";
  return `${inReplyTo}Message:\n${message}\n\nTopic: ${envelope.topic ?? ""}`;
}

/** The recorder the subscriptions share: it appends one line per envelope to `record`, and leaves
 * out an envelope whose topic and dedupe key it has already written together (the first write
 * wins). A dedupe key names one envelope on one topic, not one across the run: each of the run's
 * daemons keys its notices by its own outbox row, so the second daemon's keys repeat the first
 * one's on another controller lane, and each of those is a message of its own. It answers true
 * when it wrote the line. A frame that is not an envelope is named on stderr and recorded as
 * nothing: the subjects watched here carry envelopes only. */
export function makeRecorder(record: string): (session: string, frame: string) => boolean {
  const written = new Set<string>();
  return (session, frame) => {
    let envelope: InboxEnvelope;
    try {
      envelope = JSON.parse(frame) as InboxEnvelope;
    } catch {
      console.error(`envoy-inbox: ${session} was sent a frame that is not an envelope: ${frame}`);
      return false;
    }
    const key = envelope.dedupe_key ?? "";
    if (key !== "") {
      const pair = JSON.stringify([envelope.topic ?? "", key]);
      if (written.has(pair)) return false;
      written.add(pair);
    }
    const line = { at: new Date().toISOString(), session, text: renderDelivery(envelope) };
    appendFileSync(record, `${JSON.stringify(line)}\n`);
    return true;
  };
}

/** The header of the listener's connection probe, which is no frame (bus.ConnectionProbeHeader). */
export const PROBE_HEADER = "Envoy-Probe";

/** The one word the inbox answers a relay frame and a probe with: the frame is the session's, and
 * the listener acknowledges it to the session's consumer. */
export const RELAY_DONE = "done";

/** The parts of a NATS message the inbox reads. */
export interface InboxMessage {
  readonly data: Uint8Array;
  readonly reply?: string;
  readonly headers?: { get(name: string): string };
}

const decoder = new TextDecoder();

/** Takes one message of a watched subject for `session`: a probe is answered `done` and recorded
 * as nothing; any other message is given to `record`, and then answered `done` when it carries a
 * reply subject, whether `record` wrote it or left it out. A `record` that throws leaves the
 * message unanswered, and the throw goes to the caller. */
export function takeMessage(
  session: string,
  message: InboxMessage,
  record: (session: string, frame: string) => boolean,
  answer: (reply: string, word: string) => void
): void {
  const reply = message.reply ?? "";
  if (message.headers?.get(PROBE_HEADER) === "1") {
    if (reply !== "") answer(reply, RELAY_DONE);
    return;
  }
  record(session, decoder.decode(message.data));
  if (reply !== "") answer(reply, RELAY_DONE);
}

export interface InboxWatch {
  session: string;
  subjects: string[];
}

export interface InboxOptions {
  nats: string;
  record: string;
  watches: InboxWatch[];
}

/** Subscribes to every watched subject and records what lands on it until the process ends,
 * answering each relay frame and probe (takeMessage). It answers once every subscription has
 * reached the server, so a caller that waits for the readiness line misses nothing sent after it. */
export async function startInbox(options: InboxOptions) {
  const connection = await connect({ servers: options.nats });
  const record = makeRecorder(options.record);
  const answer = (reply: string, word: string) => connection.publish(reply, word);
  for (const watch of options.watches) {
    for (const subject of watch.subjects) {
      const subscription = connection.subscribe(subject);
      void (async () => {
        for await (const message of subscription) {
          try {
            takeMessage(watch.session, message, record, answer);
          } catch (error) {
            console.error(`envoy-inbox: ${watch.session}'s frame was not recorded: ${error}`);
          }
        }
      })();
    }
  }
  await connection.flush();
  return connection;
}

function parseArgs(argv: string[]): InboxOptions | string {
  const values: Record<string, string> = {};
  const watches: InboxWatch[] = [];
  for (let i = 0; i < argv.length; i += 2) {
    const name = argv[i];
    const value = argv[i + 1];
    if (!name?.startsWith("--") || value === undefined) return `bad argument ${name ?? ""}`;
    if (name === "--watch") {
      const at = value.indexOf("=");
      if (at <= 0) return `--watch wants <session>=<subject>[,<subject>], not ${value}`;
      const subjects = value
        .slice(at + 1)
        .split(",")
        .filter((subject) => subject !== "");
      if (subjects.length === 0) return `--watch names no subject for ${value.slice(0, at)}`;
      watches.push({ session: value.slice(0, at), subjects });
      continue;
    }
    values[name.slice(2)] = value;
  }
  const known: Record<string, true> = { nats: true, record: true };
  for (const name of Object.keys(values)) if (!known[name]) return `unknown argument --${name}`;
  if (!values.nats) return "--nats is required";
  if (!values.record) return "--record is required";
  if (watches.length === 0) return "--watch is required at least once";
  return { nats: values.nats, record: values.record, watches };
}

if (import.meta.main) {
  const options = parseArgs(process.argv.slice(2));
  if (typeof options === "string") {
    console.error(`envoy-inbox: ${options}`);
    console.error(
      "usage: bun scripts/e2e/lib/envoy-inbox.ts --nats <url> --record <file.jsonl> --watch <session>=<subject>[,<subject>] [--watch …]"
    );
    process.exit(2);
  }
  await startInbox(options);
  const watched = options.watches
    .map((watch) => `${watch.session}=${watch.subjects.join(",")}`)
    .join(" ");
  console.log(`envoy-inbox listening on ${options.nats} for ${watched}`);
}
