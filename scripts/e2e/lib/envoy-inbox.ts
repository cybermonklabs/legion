// scripts/e2e/lib/envoy-inbox.ts: the inbox of a live proof's stand-in sessions, a core NATS
// subscriber. A proof that sends an Envoy message as its own session reads what comes back on that
// session's own subject, `notifications.agent.<session>`, and a proof that stands in for a Legion
// controller reads the notices published to `notifications.legion.<project token>.controller` —
// a private stream subject no registration may carry (bus.ReachesPrivateStreamSubject), and the
// listener delivers nothing to a serving port of its own (POST /v1/interests/subscribe refuses
// `port`, cmd/listener/api.go), so both are heard on the bus itself.
//
//   bun scripts/e2e/lib/envoy-inbox.ts --nats <url> --record <file.jsonl> \
//     --watch <session>=<subject>[,<subject>] [--watch <session>=<subject>…]
//
// Each envelope that lands on a watched subject is one JSON line appended to --record: when it
// came, the session the subject was watched for, and its text as a session's deliverer renders it —
// `In Reply To: <event id>` when the envelope answers one, `Message:`, the payload (its summary
// when it carries no payload), a blank line and `Topic: <topic>`. An envelope whose dedupe key was
// already recorded is left out, so a proof counting the answers to one question counts deliveries,
// not the copies a redelivery makes.
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
 * out an envelope whose dedupe key it has already written (the first write wins). It answers true
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
      if (written.has(key)) return false;
      written.add(key);
    }
    const line = { at: new Date().toISOString(), session, text: renderDelivery(envelope) };
    appendFileSync(record, `${JSON.stringify(line)}\n`);
    return true;
  };
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

/** Subscribes to every watched subject and records what lands on it until the process ends. It
 * answers once every subscription has reached the server, so a caller that waits for the readiness
 * line misses nothing sent after it. */
export async function startInbox(options: InboxOptions) {
  const connection = await connect({ servers: options.nats });
  const record = makeRecorder(options.record);
  const decoder = new TextDecoder();
  for (const watch of options.watches) {
    for (const subject of watch.subjects) {
      const subscription = connection.subscribe(subject);
      void (async () => {
        for await (const message of subscription) {
          record(watch.session, decoder.decode(message.data));
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
