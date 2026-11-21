#!/usr/bin/env bun
/**
 * bus/daemon-reader.ts plays a Legion daemon's NATS connection for project-access-bus.sh and keeps
 * everything that connection could read. It connects through the auth service as the daemon's
 * service, user `service` with the platform token in --token-file as the password (natscred.Service,
 * which the daemon's connection presents), so the grant it reads under is the one the auth service
 * answers that token's account with: held to its token's projects by natsgrant.WithinProjects, or
 * the whole of natsgrant.Daemon() for an entry of every project (the check's negative control).
 *
 * From the start, until --stop-file appears, it holds a core subscription on each project's role
 * exceptions (`notifications.envoy.exceptions.notifications.role.<P>.>`), the one subject the
 * daemon's grant subscribes beside its inbox. Then, for each project of --projects, the run's own
 * and the other one, it asks for each consumer the daemon opens (`legion-go-<P>-dispatch` on
 * `notifications.dispatch.issue.<P>.>`, `legion-go-<P>-github` on `notifications.github.>`,
 * `legion-go-<P>-wake` on `notifications.legion.<p>.wake` and `legion-go-<P>-ready` on
 * `notifications.legion.wake`) and fetches every stored message it is given; and it walks the
 * stream's stored messages one at a time (`STREAM.MSG.GET`, `next_by_subj` `>`), as far as the
 * grant lets it. A refused create, fetch or get is how a held grant answers: it is written to
 * --refusals and is no failure here. Every message read is one JSON line of --record,
 * `{"subject", "data", "via"}`, which bus/decide.sh's audit_reads judges. It exits 0 once the record
 * is written, and 1 when it could not connect at all.
 *
 * Usage: bun daemon-reader.ts --servers <url> --token-file <file> --projects <P>,<Q>
 *          --record <file> --refusals <file> --ready-file <file> --stop-file <file>
 *          [--inbox-prefix <prefix>]
 *
 * --inbox-prefix is where the connection's replies arrive: `_INBOX` (natsgrant.Daemon()'s
 * `_INBOX.>`) until the auth service grants each identity a prefix of its own
 * (natsgrant.InboxPrefix), which the driver then passes.
 */
import { appendFileSync, existsSync, writeFileSync } from "node:fs";
import {
  AckPolicy,
  connect,
  DeliverPolicy,
  type Msg,
  type NatsConnection,
  StringCodec,
} from "nats";

const STREAM = "ENVOY_NOTIFICATIONS";
const EXCEPTIONS = "notifications.envoy.exceptions.notifications.role.";
const codec = StringCodec();

interface Options {
  servers: string;
  tokenFile: string;
  projects: string[];
  record: string;
  refusals: string;
  readyFile: string;
  stopFile: string;
  inboxPrefix: string;
}

function refuse(message: string): never {
  console.error(`daemon-reader: ${message}`);
  process.exit(2);
}

function parse(args: string[]): Options {
  const values = new Map<string, string>();
  for (let i = 0; i < args.length; i += 2) {
    const flag = args[i] ?? "";
    const value = args[i + 1];
    if (!flag.startsWith("--") || value === undefined)
      refuse(`expected --<flag> <value> at ${flag}`);
    values.set(flag.slice(2), value);
  }
  const need = (name: string): string => {
    const value = values.get(name);
    if (value === undefined || value === "") refuse(`--${name} is required`);
    return value;
  };
  return {
    servers: need("servers"),
    tokenFile: need("token-file"),
    projects: need("projects").split(","),
    record: need("record"),
    refusals: need("refusals"),
    readyFile: need("ready-file"),
    stopFile: need("stop-file"),
    inboxPrefix: values.get("inbox-prefix") ?? "_INBOX",
  };
}

const options = parse(process.argv.slice(2));
writeFileSync(options.record, "");
writeFileSync(options.refusals, "");

function keep(subject: string, data: Uint8Array, via: string): void {
  appendFileSync(options.record, `${JSON.stringify({ subject, data: codec.decode(data), via })}\n`);
}

function refused(what: string, error: unknown): void {
  const said = error instanceof Error ? error.message : String(error);
  appendFileSync(options.refusals, `${what}: ${said}\n`);
}

let connection: NatsConnection;
try {
  connection = await connect({
    servers: options.servers,
    name: "bus-acceptance daemon",
    user: "service",
    pass: (await Bun.file(options.tokenFile).text()).trim(),
    inboxPrefix: options.inboxPrefix,
    timeout: 20_000,
  });
} catch (error) {
  console.error(
    `daemon-reader: could not connect: ${error instanceof Error ? error.message : String(error)}`
  );
  process.exit(1);
}

// Async errors are how the server refuses a subscription (a permissions violation): each is kept.
void (async () => {
  for await (const status of connection.status()) {
    if (status.type === "error") refused("connection", status.data);
  }
})();

for (const project of options.projects) {
  const subject = `${EXCEPTIONS}${project}.>`;
  connection.subscribe(subject, {
    callback: (error, message: Msg) => {
      if (error !== null) refused(`subscribe ${subject}`, error);
      else keep(message.subject, message.data, `subscribe ${subject}`);
    },
  });
}
await connection.flush();
writeFileSync(options.readyFile, "ready\n");

while (!existsSync(options.stopFile)) await Bun.sleep(200);

/** The JetStream API request `subject` with `body`, answered as JSON, or the refusal thrown. */
async function api(subject: string, body: unknown): Promise<Record<string, unknown>> {
  const reply = await connection.request(subject, codec.encode(JSON.stringify(body)), {
    timeout: 5_000,
  });
  const answer: unknown = JSON.parse(codec.decode(reply.data));
  if (typeof answer !== "object" || answer === null) throw new Error("not a JSON object");
  if ("error" in answer && answer.error !== undefined)
    throw new Error(JSON.stringify(answer.error));
  return Object.fromEntries(Object.entries(answer));
}

/** Each consumer the daemon of `project` opens, with its one filter. */
function consumers(project: string): { name: string; filter: string }[] {
  const lower = project.toLowerCase();
  return [
    { name: `legion-go-${project}-dispatch`, filter: `notifications.dispatch.issue.${project}.>` },
    { name: `legion-go-${project}-github`, filter: "notifications.github.>" },
    { name: `legion-go-${project}-wake`, filter: `notifications.legion.${lower}.wake` },
    { name: `legion-go-${project}-ready`, filter: "notifications.legion.wake" },
  ];
}

const js = connection.jetstream();
for (const project of options.projects) {
  for (const { name, filter } of consumers(project)) {
    try {
      // The create subject names the consumer and its one filter, which is what a held grant
      // admits (CONSUMER.CREATE.<stream>.<name>.<filter>).
      await api(`$JS.API.CONSUMER.CREATE.${STREAM}.${name}.${filter}`, {
        stream_name: STREAM,
        config: {
          durable_name: name,
          filter_subject: filter,
          deliver_policy: DeliverPolicy.All,
          ack_policy: AckPolicy.Explicit,
          inactive_threshold: 300_000_000_000,
        },
      });
    } catch (error) {
      refused(`create ${name} on ${filter}`, error);
      continue;
    }
    try {
      const consumer = await js.consumers.get(STREAM, name);
      for (;;) {
        const batch = await consumer.fetch({ max_messages: 256, expires: 2_000 });
        let count = 0;
        for await (const message of batch) {
          count++;
          keep(message.subject, message.data, `consumer ${name}`);
          message.ack();
        }
        if (count === 0) break;
      }
    } catch (error) {
      refused(`fetch ${name}`, error);
    }
  }
}

// Every stored message the grant lets it get, one at a time from the first.
let sequence = 1;
for (;;) {
  try {
    const answer = await api(`$JS.API.STREAM.MSG.GET.${STREAM}`, {
      seq: sequence,
      next_by_subj: ">",
    });
    const message = answer.message;
    if (typeof message !== "object" || message === null)
      throw new Error("no message in the answer");
    const stored = Object.fromEntries(Object.entries(message));
    const subject = typeof stored.subject === "string" ? stored.subject : "";
    const data =
      typeof stored.data === "string" ? Buffer.from(stored.data, "base64") : new Uint8Array();
    const seq = typeof stored.seq === "number" ? stored.seq : sequence;
    keep(subject, data, "stream get");
    sequence = seq + 1;
  } catch (error) {
    refused(`stream get from ${sequence}`, error);
    break;
  }
}

await connection.drain();
process.exit(0);
