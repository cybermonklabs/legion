// The two JetStream reads and the one write a stage proof makes of a stream it does not own: whether
// the stream carries a subject, the durable consumers of a project, and the deletion of the run's
// own durable consumers at teardown. It reads what the stream holds and never publishes. Beside
// them, the two steps a proof of Envoy's fresh consumer naming mode (ENVOY_CONSUMER_NAMES=fresh)
// takes on its own throwaway server: the selection bucket made as the operator's create-bucket
// makes it, and a session's current consumer read from its selection record.
//
//   bun scripts/e2e/lib/nats-stream.ts last <nats-url> <stream> <subject-filter> <seconds>
//     prints {"subject","time","seq"} of the newest message on the filter (wildcards allowed), read
//     through one ordered consumer delivery that starts at the stream's last matching message; exits
//     1 when the stream holds none within <seconds>
//   bun scripts/e2e/lib/nats-stream.ts consumers <nats-url> <stream> <name-prefix>
//     prints one {"name","filters","pending"} line per durable consumer whose name starts with the
//     prefix
//   bun scripts/e2e/lib/nats-stream.ts delete <nats-url> <stream> <name>
//     deletes one durable consumer and prints "deleted"; one that does not exist is already gone:
//     it prints "absent" and exits 0 too, so a caller says which it was
//   bun scripts/e2e/lib/nats-stream.ts session-bucket <nats-url> <stream> <seconds>
//     waits up to <seconds> for <stream> to exist, then makes the session consumers' selection
//     bucket and its sentinel (createSessionBucket) and prints the sentinel's JSON
//   bun scripts/e2e/lib/nats-stream.ts current <nats-url> <session-id>
//     prints the consumer the session's selection record names now (readCurrent), or an empty
//     line when it names none
import {
  type ConsumerMessages,
  connect,
  DeliverPolicy,
  type NatsConnection,
  type NatsError,
  StorageType,
  type StoredMsg,
  type StreamInfo,
} from "nats";
import type { NatsSeed } from "../../../packages/envoy-client/src/envoy-settings";
import { messageFor } from "../../../packages/envoy-client/src/errors";
import { natsAuthOptions } from "../../../packages/envoy-client/src/nats-auth";
import {
  natsDialFailure,
  natsServersForLog,
} from "../../../packages/envoy-client/src/nats-connection";
import { operatorNatsSeed } from "./operator-nats-seed";

function refuse(message: string): never {
  process.stderr.write(`nats-stream: ${message}\n`);
  process.exit(2);
}

// The fresh naming mode's selection bucket of the session consumers, and the key of its sentinel
// (consumernames.SessionBucket and consumernames.SentinelKey, packages/envoy/consumernames/kv.go).
export const sessionBucket = "envoy_session_consumers";
export const sentinelKey = "envoy.bucket";

// The JetStream API's error codes these steps read: a stream that does not exist, and a stream
// message get that found no message.
const streamNotFound = 10059;
const noMessageFound = 10037;

function apiErrorCode(error: unknown): number | undefined {
  return (error as Partial<NatsError> | undefined)?.api_error?.err_code;
}

/**
 * The sentinel the session bucket is made with (consumernames.StoreState), keyed by its own key and
 * naming the session family.
 */
export interface SessionBucketState {
  key: typeof sentinelKey;
  family: "session";
  cutover: "done";
  boundary: number;
}

/**
 * Makes the session consumers' selection bucket as the operator's create-bucket makes it
 * (consumernames.CreateSessionBucket): it waits up to `seconds` for `stream`, the stream the
 * session consumers read, to exist, reads its first sequence, then creates the bucket with one
 * value a key, on file storage, at the stream's own replica count, and writes the sentinel
 * {"key":"envoy.bucket","family":"session","cutover":"done","boundary":B} with B = max(1, that
 * first sequence) as the key's first write. The listener reads only a sentinel that names its own
 * key and a family.
 * A bucket that exists already is refused, as create-bucket refuses it. Answers the sentinel.
 */
export async function createSessionBucket(
  nc: NatsConnection,
  stream: string,
  seconds: number
): Promise<SessionBucketState> {
  const jsm = await nc.jetstreamManager();
  const deadline = Date.now() + Math.ceil(seconds * 1000);
  let info: StreamInfo | undefined;
  while (info === undefined) {
    try {
      info = await jsm.streams.info(stream);
    } catch (error) {
      if (apiErrorCode(error) !== streamNotFound) throw error;
      if (Date.now() >= deadline)
        throw new Error(`the stream ${stream} did not exist within ${seconds} s`);
      await Bun.sleep(250);
    }
  }
  const state: SessionBucketState = {
    key: sentinelKey,
    family: "session",
    cutover: "done",
    boundary: Math.max(1, info.state.first_seq),
  };
  const exists = await jsm.streams.info(`KV_${sessionBucket}`).then(
    () => true,
    (error: unknown) => {
      if (apiErrorCode(error) === streamNotFound) return false;
      throw error;
    }
  );
  if (exists) throw new Error(`the bucket ${sessionBucket} exists already`);
  const kv = await nc.jetstream().views.kv(sessionBucket, {
    description: "Envoy session consumers' selection records: each session's current consumer name",
    history: 1,
    storage: StorageType.File,
    replicas: info.config.num_replicas,
  });
  await kv.create(sentinelKey, JSON.stringify(state));
  return state;
}

/**
 * The "current command": the consumer the fresh naming mode's selection record of `sessionId`
 * names now, the record's `current` (consumernames.Record) under the key `sessionId` in
 * sessionBucket, read as the listener reads it (the key's last message, from the bucket stream's
 * leader). "" when the record names none, as after the session's clean stop, which leaves the
 * record with no current, and when the bucket holds no record of the session at all. A key whose
 * last message is a delete or purge marker is thrown, as the listener refuses such a session, and
 * so are a value that is not the session's own record (its `key` is another) and a bucket that
 * does not exist.
 */
export async function readCurrent(nc: NatsConnection, sessionId: string): Promise<string> {
  const jsm = await nc.jetstreamManager();
  let message: StoredMsg;
  try {
    message = await jsm.streams.getMessage(`KV_${sessionBucket}`, {
      last_by_subj: `$KV.${sessionBucket}.${sessionId}`,
    });
  } catch (error) {
    if (apiErrorCode(error) === noMessageFound) return "";
    throw error;
  }
  const operation = message.header?.get("KV-Operation");
  if (operation === "DEL" || operation === "PURGE")
    throw new Error(
      `the selection record of ${sessionId} in ${sessionBucket} is gone: its last message is a ${operation} marker`
    );
  const record = message.json<{ key?: unknown; current?: unknown }>();
  if (record.key !== sessionId)
    throw new Error(
      `the key ${sessionId} of ${sessionBucket} holds no record of its own: its key is ${JSON.stringify(record.key)}`
    );
  const current = record.current ?? "";
  if (typeof current !== "string")
    throw new Error(
      `the selection record of ${sessionId} in ${sessionBucket} holds a current that is no name: ${JSON.stringify(current)}`
    );
  return current;
}

export interface LastMessage {
  subject: string;
  time: string;
  seq: number;
}

export interface LastRead {
  message: LastMessage | undefined;
  timedOut: boolean;
}

export async function readLast(
  nc: NatsConnection,
  stream: string,
  subjectFilter: string,
  seconds: number
): Promise<LastRead> {
  const timeoutMilliseconds = Math.ceil(seconds * 1000);
  let stopTimer = () => {};
  const deadline = new Promise<undefined>((resolve) => {
    const timer = setTimeout(resolve, timeoutMilliseconds);
    stopTimer = () => clearTimeout(timer);
  });
  let messages: ConsumerMessages | undefined;
  const nextMessage = nc
    .jetstream()
    .consumers.get(stream, {
      filterSubjects: [subjectFilter],
      deliver_policy: DeliverPolicy.Last,
    })
    .then(async (consumer) => {
      messages = await consumer.fetch({
        max_messages: 1,
        expires: Math.max(1000, timeoutMilliseconds),
      });
      for await (const message of messages) return message;
      return undefined;
    });
  try {
    const result = await Promise.race([nextMessage, deadline]);
    if (result === undefined) {
      if (messages) {
        await messages.close();
        await nextMessage.catch(() => undefined);
      } else {
        void nc.close();
        void nextMessage.catch(() => undefined);
      }
      return { message: undefined, timedOut: true };
    }
    return result
      ? {
          message: {
            subject: result.subject,
            time: new Date(result.info.timestampNanos / 1e6).toISOString(),
            seq: result.seq,
          },
          timedOut: false,
        }
      : { message: undefined, timedOut: false };
  } finally {
    stopTimer();
  }
}

async function main(): Promise<void> {
  const [command, url, stream, argument, extra] = process.argv.slice(2);
  // current takes one operand, the session id, in the place where the other commands take a stream.
  if (!command || !url || !stream || (command !== "current" && !argument)) {
    refuse(
      "usage: last|consumers|delete|session-bucket <nats-url> <stream> <subject-filter|name-prefix|name|seconds> [seconds], or current <nats-url> <session-id>"
    );
  }

  // Production NATS: the operator's nkey user, when the server requires one, as a command run from
  // the operator's shell resolves it (operator-nats-seed.ts).
  let seed: NatsSeed | undefined;
  try {
    seed = operatorNatsSeed(url, "<nats-url>", process.env, process.cwd());
  } catch (error) {
    refuse(messageFor(error));
  }
  // A failed connect is named, and nats.js's own error left uncaught would print the URL it was
  // given, a credential in it included (a malformed URL's `input`): the line names the server by
  // its argument and the reason as a log may (natsDialFailure), and the error goes no further.
  let nc: NatsConnection;
  try {
    nc = await connect({
      servers: url,
      timeout: 10_000,
      name: "legion-e2e-nats-stream",
      ...natsAuthOptions(seed === undefined ? { kind: "none" } : { kind: "seed", seed }),
    });
  } catch (error) {
    process.stderr.write(
      `nats-stream: ${natsDialFailure(natsServersForLog("<nats-url>", 1), error)}\n`
    );
    process.exit(1);
  }
  try {
    if (command === "last") {
      const seconds = Number(extra ?? "10");
      if (!Number.isFinite(seconds) || seconds <= 0)
        refuse(`seconds must be a positive number, not ${extra}`);
      const result = await readLast(nc, stream, argument, seconds);
      if (!result.message) {
        process.stderr.write(`nats-stream: stream ${stream} holds no message on ${argument}\n`);
        process.exitCode = 1;
        if (result.timedOut) {
          void nc.close();
          process.exit(1);
        }
      } else {
        process.stdout.write(`${JSON.stringify(result.message)}\n`);
      }
    } else if (command === "consumers") {
      const jsm = await nc.jetstreamManager();
      for await (const info of jsm.consumers.list(stream)) {
        if (!info.name.startsWith(argument)) continue;
        const filters =
          info.config.filter_subjects ??
          (info.config.filter_subject ? [info.config.filter_subject] : []);
        process.stdout.write(
          `${JSON.stringify({ name: info.name, filters, pending: info.num_pending })}\n`
        );
      }
    } else if (command === "delete") {
      const jsm = await nc.jetstreamManager();
      try {
        await jsm.consumers.delete(stream, argument);
        process.stdout.write("deleted\n");
      } catch (error) {
        if (!String(error).includes("consumer not found")) throw error;
        process.stdout.write("absent\n");
      }
    } else if (command === "session-bucket") {
      const seconds = Number(argument);
      if (!Number.isFinite(seconds) || seconds <= 0)
        refuse(`seconds must be a positive number, not ${argument}`);
      const state = await createSessionBucket(nc, stream, seconds);
      process.stdout.write(`${JSON.stringify(state)}\n`);
    } else if (command === "current") {
      process.stdout.write(`${await readCurrent(nc, stream)}\n`);
    } else {
      refuse(`unknown command ${command}`);
    }
  } finally {
    await nc.close();
  }
}

if (import.meta.main) await main();
