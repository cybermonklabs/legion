// The frames a session's relay subject carries and how a session reads them: a relay frame, one
// stored message of the session's durable consumer, and a task frame, the claim's task value the
// listener's task relay publishes there. Consumer delivery (`./consumer-delivery`) takes both.

/** The headers of a relay frame, and of the listener's connection probe. */
export const RelayHeader = {
  /** The subject the stream stored the message on. */
  subject: "Envoy-Subject",
  /** The stream sequence, decimal. */
  streamSeq: "Envoy-Stream-Seq",
  /** The stream's `envoy_stream_id`. */
  streamID: "Envoy-Stream-Id",
  /** JetStream's delivery count, decimal; 1 is the first delivery. */
  delivered: "Envoy-Delivered",
  /** When the stream stored the message: RFC 3339 with nanoseconds, UTC. */
  storedAt: "Envoy-Stored-At",
  /** `true` only on the first frame after the relay restarted below the stream's first sequence. */
  gap: "Envoy-Gap",
  /**
   * `1` on the listener's connection probe, which is no frame: a core request on this session's
   * relay subject with this header alone and no body, sent when someone sends to the session. Any
   * answer, or none within the listener's second, reads connected; NATS no-responders reads
   * stopped. It never carries a frame header, so it can never decode as a frame and move the
   * resume point (`receive` answers it before the decode).
   */
  probe: "Envoy-Probe",
} as const;

/**
 * The headers of a task frame: the core message the listener publishes on a session's relay
 * subject for each put and delete of the Legion task bucket's key of the claim whose role that
 * session holds (the listener's task relay; the names are `legiontasks.Header*` in
 * `packages/envoy/legiontasks`). A frame carrying `claim` is a task frame, whatever else it
 * carries: it is no stored message of the session's consumer, so it has none of the relay
 * headers above.
 */
export const TaskHeader = {
  /** The bucket's key the frame relays: the claim's token. */
  claim: "Envoy-Task-Claim",
  /** The bucket's epoch the revision belongs to: its stream's creation time, compared only. */
  epoch: "Envoy-Task-Epoch",
  /** The key's revision in the bucket, decimal. */
  revision: "Envoy-Task-Revision",
  /** What happened to the key at that revision: `put` or `delete`. */
  op: "Envoy-Task-Op",
} as const;

/**
 * A claim's task value as a task frame relays it: the claim, the bucket's epoch and the key's
 * revision in it, and what happened to the key there. A `put` names the number of the claim's
 * current task (`task`), the one thing the bucket holds for a claim; a `delete` names none, the
 * claim having no current task.
 */
export type RelayTaskValue =
  | {
      readonly claim: string;
      readonly epoch: string;
      readonly revision: number;
      readonly op: "put";
      readonly task: number;
    }
  | {
      readonly claim: string;
      readonly epoch: string;
      readonly revision: number;
      readonly op: "delete";
    };

type DecodedTaskFrame = { readonly value: RelayTaskValue } | { readonly error: string };

// A put's data, exactly as the daemon writes it (`legiontasks.Value.Encode`) and its decode reads
// it: one object holding the one member `task`, a positive integer with no sign, fraction or
// exponent, and JSON's insignificant whitespace around each token.
const TASK_VALUE =
  /^[ \t\r\n]*\{[ \t\r\n]*"task"[ \t\r\n]*:[ \t\r\n]*([1-9][0-9]*)[ \t\r\n]*\}[ \t\r\n]*$/;

/** What `decodeRelayFrame` reads headers through; a NATS message's `headers` is one. */
export interface RelayHeaders {
  get(name: string): string | null | undefined;
}

/** A message received on the relay subject, as any NATS client hands it over. */
export interface RelayMessage {
  readonly data: Uint8Array | string;
  readonly headers?: RelayHeaders | undefined;
  readonly reply?: string | undefined;
}

/** A decoded relay frame. */
export interface RelayFrame {
  /** The subject the stream stored the message on. */
  readonly subject: string;
  readonly streamSeq: number;
  readonly streamID: string;
  /** JetStream's delivery count; 1 is the first delivery. */
  readonly delivered: number;
  /** The stored time as the header carries it (RFC 3339, UTC). */
  readonly storedAt: string;
  /** The stored time in milliseconds since the epoch. */
  readonly storedAtMs: number;
  /** The relay restarted below the stream's first sequence just before this frame. */
  readonly gap: boolean;
  /** The stored message's bytes as text: the Envoy envelope JSON. */
  readonly data: string;
  /** The listener's inbox for this frame's replies. */
  readonly reply: string;
}

type DecodedRelayFrame = { readonly frame: RelayFrame } | { readonly error: string };

const decoder = new TextDecoder();
const DECIMAL = /^[0-9]+$/;
// RFC 3339 with up to nine fractional digits. `Date.parse` reads at most milliseconds portably,
// so the fraction is cut to three digits first.
const RFC3339 = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/;

function positiveInteger(text: string | null | undefined): number | undefined {
  if (text === null || text === undefined || !DECIMAL.test(text)) return undefined;
  const value = Number(text);
  return Number.isSafeInteger(value) && value > 0 ? value : undefined;
}

function parseStoredAt(text: string): number | undefined {
  const match = RFC3339.exec(text);
  if (match === null) return undefined;
  const fraction = (match[2] ?? "").padEnd(3, "0").slice(0, 3);
  const ms = Date.parse(`${match[1]}.${fraction}${match[3]}`);
  return Number.isNaN(ms) ? undefined : ms;
}

/** Reads a relay frame's headers; any header missing or malformed, or no reply, is an error. */
export function decodeRelayFrame(message: RelayMessage): DecodedRelayFrame {
  const reply = message.reply ?? "";
  if (reply === "") return { error: "no reply subject" };
  const headers = message.headers;
  if (headers === undefined) return { error: "no headers" };
  const subject = headers.get(RelayHeader.subject) ?? "";
  if (subject === "") return { error: `no ${RelayHeader.subject}` };
  const streamSeq = positiveInteger(headers.get(RelayHeader.streamSeq));
  if (streamSeq === undefined) return { error: `bad ${RelayHeader.streamSeq}` };
  const streamID = headers.get(RelayHeader.streamID) ?? "";
  if (streamID === "") return { error: `no ${RelayHeader.streamID}` };
  const delivered = positiveInteger(headers.get(RelayHeader.delivered));
  if (delivered === undefined) return { error: `bad ${RelayHeader.delivered}` };
  const storedAt = headers.get(RelayHeader.storedAt) ?? "";
  const storedAtMs = parseStoredAt(storedAt);
  if (storedAtMs === undefined) return { error: `bad ${RelayHeader.storedAt}` };
  return {
    frame: {
      subject,
      streamSeq,
      streamID,
      delivered,
      storedAt,
      storedAtMs,
      gap: headers.get(RelayHeader.gap) === "true",
      data: typeof message.data === "string" ? message.data : decoder.decode(message.data),
      reply,
    },
  };
}

/** Reads a task frame; any header missing or malformed, or a put whose data is no task value, is
 * an error. A delete's data is not read: the bucket holds nothing at a delete. */
export function decodeTaskFrame(message: RelayMessage): DecodedTaskFrame {
  const headers = message.headers;
  const claim = headers?.get(TaskHeader.claim) ?? "";
  if (headers === undefined || claim === "") return { error: `no ${TaskHeader.claim}` };
  const epoch = headers.get(TaskHeader.epoch) ?? "";
  if (epoch === "") return { error: `no ${TaskHeader.epoch}` };
  const revision = positiveInteger(headers.get(TaskHeader.revision));
  if (revision === undefined) return { error: `bad ${TaskHeader.revision}` };
  const op = headers.get(TaskHeader.op);
  if (op === "delete") return { value: { claim, epoch, revision, op } };
  if (op !== "put") return { error: `bad ${TaskHeader.op}` };
  const data = typeof message.data === "string" ? message.data : decoder.decode(message.data);
  const task = positiveInteger(TASK_VALUE.exec(data)?.[1]);
  if (task === undefined) return { error: "a put whose data is no task value" };
  return { value: { claim, epoch, revision, op, task } };
}
