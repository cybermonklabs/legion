// A Legion daemon's state lane, for the browser rows that need Dispatch's Legion intake (k25 and
// k28 of dispatch://INFRA-2918/artifact/plan-md, T42): it publishes state messages on the subject a
// daemon publishes them on (`legionStateSubject`), into the stream Dispatch's intake reads its
// durable from, each one as the daemon writes it.
//
// The bus is the harness's: DISPATCH_E2E_NATS_URLS, which e2e/run-server.sh hands the server as its
// NATS_URLS, which turns its publisher and its Legion intake on (e2e/fake-envoy.ts reads the same
// variable). A run that names none has no intake, and `requireNats` fails the row by that name.
//
// Each message is the daemon's shape, read from packages/contracts/fixtures/daemon-api/
// state-events.json and the intake's decoder (packages/envoy/internal/dispatch/legion/
// state_message.go): an Envoy envelope whose payload is one transaction's state message (project,
// run, stateId, firstSeq, seq, at, events). Every message here holds one `admission` event, whose
// block is the stub daemon's (`admissionBlock`, e2e/legion-daemon-stub.ts) without `p0Bound`, as
// the daemon's event carries it, and both the envelope and the message are checked against the
// contract's readers before they are sent. A batch of entries goes in messages of at most
// ENTRIES_PER_MESSAGE entries each.
//
// The intake applies one project's messages one at a time, and a message that arrives while 16 of
// the project's wait is judged beside the project's runner and applies nothing (`stateMaxWaiting`,
// legion/crowded.go): the next one is then a gap that only a whole read covers. So the feed keeps at
// most IN_FLIGHT messages ahead of the sequence the intake has applied, read through the caller's
// `applied` (the project's `legion_event_cursors.last_seq`).
import { randomBytes } from "node:crypto";

import { EnvelopeSchema, legionProjectToken, legionStateSubject } from "@legion/contracts";
import { legionStateMessage } from "@legion/contracts/legion-api";
import { connect, StringCodec } from "nats";

import {
  type AdmissionRecords,
  admissionBlock,
  type CapChange,
  type SignalChange,
} from "./legion-daemon-stub";
import { expect } from "./test";

/** The stream that carries every notification, the state lanes among them (`bus.Stream`), which the
 *  harness's Dispatch server creates at its start (`bus.ConnectOwningStream`). */
const STREAM = "ENVOY_NOTIFICATIONS";

/** The durable Dispatch's Legion intake reads the state lanes from
 *  (`bus.DispatchLegionStateDurable`), which a take-over creates once the server holds it. */
const INTAKE_DURABLE = "legion-state-events";

/** The most entries one message carries. */
export const ENTRIES_PER_MESSAGE = 500;

/** The most messages the feed keeps ahead of what the intake has applied: under the 16 one
 *  project's runner keeps waiting (`stateMaxWaiting`). */
const IN_FLIGHT = 8;

/** How long the feed waits for the intake to take its durable, or to apply a message. A message of
 *  ENTRIES_PER_MESSAGE readings is one transaction of the intake's, and the bound is many of them. */
const INTAKE_WAIT_MS = 180_000;

/** The servers DISPATCH_E2E_NATS_URLS names, failing the row by that name where it names none. */
export function requireNats(): string[] {
  const servers = (process.env.DISPATCH_E2E_NATS_URLS ?? "")
    .split(",")
    .map((server) => server.trim())
    .filter((server) => server !== "");
  if (servers.length === 0) {
    throw new Error(
      "DISPATCH_E2E_NATS_URLS names no NATS server: this row feeds Dispatch's Legion intake, which runs only where e2e/run-server.sh hands the server a NATS (a nats-server with JetStream, NATS=1 in the browser Job)"
    );
  }
  return servers;
}

/** The daemon run and store the feed publishes as, and the sequence a whole read of the stub daemon
 *  started the project's state events from (`daemon.stateSeq`). */
export interface FeedStart {
  readonly project: string;
  readonly run: string;
  readonly stateId: string;
  readonly seq: number;
  /** The sequence the intake has applied for the project, null before its first. */
  readonly applied: () => Promise<number | null>;
}

/** A state lane being fed. */
export interface StateFeed {
  /** The subject it publishes on. */
  readonly subject: string;
  /** The sequence of the newest event it published (the start's, before the first). */
  seq(): number;
  /** Publishes one message of one admission event reporting `records`, committed at `at`: the next
   *  sequence, or `seq` where given (a sequence past the next is a gap, which calls for a whole
   *  read). Answers the message's sequence. */
  admission(at: string, records: AdmissionRecords, seq?: number): Promise<number>;
  /** Publishes `capChanges` and `signalChanges`, in the order given, in messages of at most
   *  ENTRIES_PER_MESSAGE entries, each of one admission event reporting `cap` and committed at its
   *  newest entry's time, each held until the intake is at most IN_FLIGHT messages behind it.
   *  Answers how many messages it published. */
  entries(
    cap: number | null,
    capChanges: readonly CapChange[],
    signalChanges: readonly SignalChange[]
  ): Promise<number>;
  /** Waits until the intake has applied every message published. */
  settled(): Promise<void>;
  close(): Promise<void>;
}

/** 32 hex digits, an id as the daemon's outbox mints one. */
function eventId(): string {
  return randomBytes(16).toString("hex");
}

/** When an entry was made. */
function entryTime(entry: CapChange | SignalChange): number {
  return Date.parse("setAt" in entry ? entry.setAt : entry.at);
}

/** Opens a feed of `start.project`'s state lane: connects to the harness's NATS, holds that the
 *  stream Dispatch's server created carries the lane, and waits for the intake's durable on it. */
export async function openStateFeed(start: FeedStart): Promise<StateFeed> {
  const servers = requireNats();
  const subject = legionStateSubject(legionProjectToken(start.project, "project"));
  const connection = await connect({ name: "e2e-legion-state-feed", servers });
  const manager = await connection.jetstreamManager();
  const carrying = await manager.streams.find(subject).catch(() => "");
  if (carrying !== STREAM) {
    await connection.close();
    throw new Error(
      `no stream ${STREAM} carries ${subject} on ${servers.join(",")} (found "${carrying}"): Dispatch's server creates it at its start, so the server named in DISPATCH_E2E_NATS_URLS is not this run's`
    );
  }
  await expect
    .poll(
      () =>
        manager.consumers.info(STREAM, INTAKE_DURABLE).then(
          () => true,
          () => false
        ),
      {
        message: `the Legion intake's durable ${INTAKE_DURABLE} on ${STREAM}`,
        timeout: INTAKE_WAIT_MS,
      }
    )
    .toBe(true);
  const stream = connection.jetstream();
  const codec = StringCodec();
  let last = start.seq;

  const waitApplied = async (seq: number, what: string) => {
    await expect
      .poll(async () => (await start.applied()) ?? 0, {
        message: `${start.project}'s intake applies its state events to ${what}`,
        timeout: INTAKE_WAIT_MS,
      })
      .toBeGreaterThanOrEqual(seq);
  };

  const publish = async (at: string, records: AdmissionRecords, seq: number): Promise<number> => {
    const { p0Bound: _judged, ...block } = admissionBlock(records);
    const message = {
      at,
      events: [{ admission: block, kind: "admission", seq }],
      firstSeq: seq,
      project: start.project,
      run: start.run,
      seq,
      stateId: start.stateId,
    };
    const read = legionStateMessage.safeParse(message);
    if (!read.success)
      throw new Error(`a state message the contract refuses: ${read.error.message}`);
    const envelope = {
      dedupe_key: `legion-outbox:${seq}`,
      event_id: eventId(),
      issued_at: Date.now(),
      payload: JSON.stringify(message),
      payload_summary: `${start.project} state ${seq}-${seq}`,
      source: "agent",
      source_event_id: eventId(),
      topic: subject,
      trace_id: eventId(),
    };
    const sealed = EnvelopeSchema.safeParse(envelope);
    if (!sealed.success)
      throw new Error(`an envelope the contract refuses: ${sealed.error.message}`);
    const ack = await stream.publish(subject, codec.encode(JSON.stringify(envelope)), {
      msgID: envelope.event_id,
    });
    expect(ack.stream, `the stream that stored ${subject}`).toBe(STREAM);
    last = Math.max(last, seq);
    return seq;
  };

  return {
    admission: (at, records, seq) => publish(at, records, seq ?? last + 1),
    close: () => connection.drain(),
    entries: async (cap, capChanges, signalChanges) => {
      const all: (CapChange | SignalChange)[] = [...capChanges, ...signalChanges];
      let messages = 0;
      for (let from = 0; from < all.length; from += ENTRIES_PER_MESSAGE) {
        const batch = all.slice(from, from + ENTRIES_PER_MESSAGE);
        const next = last + 1;
        await waitApplied(next - IN_FLIGHT, `sequence ${next - IN_FLIGHT}`);
        const newestFirst = (a: CapChange | SignalChange, b: CapChange | SignalChange) =>
          entryTime(b) - entryTime(a) || b.id - a.id;
        await publish(
          new Date(Math.max(...batch.map(entryTime))).toISOString(),
          {
            cap,
            capChanges: batch
              .filter((entry): entry is CapChange => "setAt" in entry)
              .sort(newestFirst),
            signalChanges: batch
              .filter((entry): entry is SignalChange => !("setAt" in entry))
              .sort(newestFirst),
          },
          next
        );
        messages += 1;
      }
      return messages;
    },
    seq: () => last,
    settled: () => waitApplied(last, `sequence ${last}`),
    subject,
  };
}
