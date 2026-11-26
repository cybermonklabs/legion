/**
 * The committed regression rig for the three session states (dispatch://INFRA-392): a session
 * whose box stalls it is `unresponsive`, not dead, and keeps its record, its role claim and its
 * messages until the gone horizon.
 *
 * It runs no agent, so it runs where CI runs: no model route and no agent process. The stalled
 * session is a child Bun process of this file (`runSessionChild`) carrying this package's real
 * registration and heartbeat — `createEnvoyClient(...).subscribe`, the call every plugin's
 * heartbeat repeats (`registerSession`, packages/pi-envoy/extensions/envoy.ts), with the real
 * role claim and the real session consumer beside it: the child subscribes its relay subject and
 * reads what the stream stored for it through `createConsumerDelivery`, the one path a message
 * reaches a session by — against the real listener and real natsd test/envoy-stack.ts builds
 * from this checkout. The stall is `SIGSTOP`, which stops exactly what a loaded box stops: the
 * heartbeat, with nothing closed and nothing deregistered.
 * `SIGCONT` resumes it and `SIGKILL` ends it with no deregistration, as a killed pane leaves it.
 *
 * Run it from packages/envoy-client with `LEGION_E2E_ENVOY_STACK=1 bun run test:stall:session`.
 * That script is the command of the envoy-session-stall job's step, which sets
 * LEGION_E2E_ENVOY_STACK too: its `--timeout` is the one guard of the file's hooks and rows, and
 * it loads the row-deadline preload (`@legion/test-fixtures/row-deadline`) their waits end by.
 */

import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, test } from "bun:test";
import { EnvelopeSchema, legacyAgentSubject } from "@legion/contracts";
import { settled, span, until } from "@legion/test-fixtures/wait";
import { connect, Events, type KV, type NatsConnection, StringCodec } from "nats";
import { z } from "zod";
import { createConsumerDelivery } from "../src/consumer-delivery";
import { createDeliveryLedger } from "../src/delivery-ledger";
import { createEnvoyClient, type EnvoyClient, type SessionState } from "../src/transport";
import { delivered } from "./delivered";
import {
  type EnvoyStack,
  envoyStackEnabled,
  readInRow,
  startStoppableEnvoyStack,
  throwIfExited,
  throwIfRowEnded,
  useEnvoyStackDirectory,
} from "./envoy-stack";

// Destructured: claude-envoy's typecheck, which compiles this directory, forbids `process.env.X`.
const { ENVOY_SESSION_STALL_CHILD } = process.env;

/**
 * The windows the listener runs with here, the deployment's shape in seconds instead of hours so
 * that crossing each one costs the test its real time and no more. The ratio is the deployed one:
 * the child re-registers five times inside the live window, as a pane's 90 s heartbeat does
 * inside the five-minute window (`session.LiveWindow`, packages/envoy/internal/session/state.go).
 */
const HEARTBEAT_MS = 2_000;
const LIVE_WINDOW_MS = 10_000;
const GONE_HORIZON_MS = 60_000;
/**
 * The cadence of both of the listener's session loops, each read from a setting of its own:
 * ENVOY_SESSION_REAP_INTERVAL for the session reaper, which deletes the record of a session past
 * the horizon, and ENVOY_ROLE_CLAIM_REAP_INTERVAL for the role claim reaper, which releases that
 * session's role claim and no earlier one's (`StartRoleClaimReaper`,
 * packages/envoy/cmd/listener/main.go). The deployment's role-claim period is five minutes, and
 * nothing else releases a claim, so this rig gives the listener one of its own and the last
 * property below waits out a pass of it rather than those five minutes.
 */
const REAP_INTERVAL_MS = 2_000;
/**
 * The session bucket's maximum age, far above the gone horizon. It is the setting that makes the
 * horizon mean anything: the bucket drops a record this long after the last registration whatever
 * the horizon says, so an age below the horizon would take a stalled session's record away before
 * it could be read as unresponsive — which reads as `gone`, the very bug this rig exists for.
 */
const SESSION_MAX_AGE_MS = 30 * 60_000;

const ROLE_CLAIM_BUCKET = "envoy_roles";
const OBSERVER_SESSION = "ses_session_stall_observer";
const PLUGIN = { name: "@legion/envoy-client", version: "session-stall-rig" };

/** A Go duration for the listener's environment: milliseconds are a Go duration unit. */
const goDuration = (ms: number): string => `${ms}ms`;

const StateSchema = z.enum(["live", "unresponsive", "gone"]);

const SessionRowSchema = z.object({
  session_id: z.string(),
  state: StateSchema,
  last_seen: z.number().int(),
});

/** The 404 of a session that is not live: the kept `no live session <id>` wording, with the
 *  state and the last registration the refusal was decided on beside it. */
const NoLiveSessionSchema = z.object({
  error: z.string(),
  state: StateSchema,
  last_seen: z.number().int().optional(),
});

const RoleHolderSchema = z.object({
  role: z.string(),
  holder: z.string(),
  last_seen: z.number().int(),
  holder_state: StateSchema.optional(),
});

const RoleRefusalSchema = z.object({
  error: z.string(),
  reason: z.string(),
  holder: z.string().optional(),
  holder_state: StateSchema.optional(),
  last_seen: z.number().int().optional(),
  claim_released: z.boolean().optional(),
});

/** One role claim as the `envoy_roles` bucket holds it. `claimed_at` is readable nowhere else:
 *  no route answers it, which is why this rig reads the bucket to prove a stall moved neither it
 *  nor the holder. */
const RoleClaimSchema = z.object({
  holder_session_id: z.string(),
  claimed_at: z.number().int(),
  previous_session_id: z.string().optional(),
});

const ChildConfigSchema = z.object({
  envoyUrl: z.string(),
  natsUrl: z.string(),
  sessionID: z.string(),
  role: z.string(),
  directory: z.string(),
  title: z.string(),
  heartbeatMs: z.number().int().positive(),
});

const ChildEventSchema = z.discriminatedUnion("event", [
  z.object({ event: z.literal("registered") }),
  z.object({ event: z.literal("role"), claimed: z.boolean() }),
  z.object({ event: z.literal("message"), text: z.string(), seq: z.number().int() }),
  z.object({ event: z.literal("gap"), message: z.string() }),
  z.object({ event: z.literal("failed"), what: z.string(), error: z.string() }),
]);

type ChildConfig = z.infer<typeof ChildConfigSchema>;
type ChildEvent = z.infer<typeof ChildEventSchema>;
type RoleClaim = z.infer<typeof RoleClaimSchema>;

/**
 * The stalled session itself, run as a child process of this file.
 *
 * Everything it does on the wire is this package's own: `createEnvoyClient(...).subscribe` is the
 * registration, repeated on an interval, which is what a plugin's heartbeat is (pi-envoy's
 * `registerSession` sends this very body: port 0, its own agent subject followed,
 * self-subscribed, the plugin named, and the delivery request its relay holds); `setRole` is the
 * real role claim; `createConsumerDelivery` on the relay subject is the real read of what the
 * stream stored for it, the one path a message reaches a session by, including what was stored
 * while it was away. Nothing here fakes a listener call.
 *
 * It says each accepted registration and each message its consumer handed it on stdout, one JSON
 * object per line, and never returns: the parent freezes, continues and kills it.
 */
async function runSessionChild(config: ChildConfig): Promise<never> {
  const say = (event: ChildEvent): void => {
    process.stdout.write(`${JSON.stringify(event)}\n`);
  };
  const client = createEnvoyClient({ baseUrl: config.envoyUrl, fetch });
  const subject = legacyAgentSubject(config.sessionID);
  const nats = await connect({ servers: [config.natsUrl], name: config.sessionID });
  const codec = StringCodec();
  // The ledger a host keeps of what it has already shown. Nothing is persisted: this child lives
  // once, so what it was shown is what it has said on stdout.
  const ledger = createDeliveryLedger({
    host: {
      sessionID: () => config.sessionID,
      followedSubjects: () => [subject],
      showFailureNotice: async () => undefined,
      failureNoticeFailed: (_notice, error) =>
        say({ event: "failed", what: "a failure notice", error: String(error) }),
      log: (message, error) => say({ event: "failed", what: message, error: String(error) }),
      write: () => undefined,
    },
  });
  const delivery = createConsumerDelivery({
    sessionID: config.sessionID,
    ledger,
    host: {
      deliver: (frame) => {
        say({
          event: "message",
          text: EnvelopeSchema.parse(JSON.parse(frame.data)).payload_summary,
          seq: frame.streamSeq,
        });
        frame.done();
      },
      gap: (notice) => say({ event: "gap", message: notice.message }),
      log: (message, error) => say({ event: "failed", what: message, error: String(error) }),
    },
    publish: (reply, body) => nats.publish(reply, codec.encode(body)),
    persist: () => true,
  });
  const relay = nats.subscribe(delivery.subject);
  void (async () => {
    for await (const message of relay) delivery.receive(message);
  })();
  // nats.js resubscribes on its own, but the listener must restart this session's relay at its
  // ack floor: each reconnect begins an epoch, which the next registration carries. A freeze
  // long enough for the server to drop the connection ends this way.
  void (async () => {
    for await (const status of nats.status()) {
      if (status.type === Events.Reconnect) delivery.beginEpoch();
    }
  })();
  const register = async (): Promise<void> => {
    const answer = await client.subscribe({
      sessionID: config.sessionID,
      directory: config.directory,
      topics: [subject],
      port: 0,
      title: config.title,
      driving: false,
      selfSubscribed: true,
      plugin: PLUGIN,
      delivery: delivery.registration(),
    });
    delivery.answered(answer.delivery);
    say({ event: "registered" });
  };

  await register();
  const claim = await client.setRole({ sessionID: config.sessionID, role: config.role });
  say({ event: "role", claimed: claim.claimed });
  setInterval(() => {
    void (async () => {
      try {
        await register();
      } catch (error) {
        // A pass the freeze cut in half fails once on resume; the next tick registers again,
        // exactly as a pane's heartbeat does after its box comes back.
        say({ event: "failed", what: "a heartbeat pass", error: String(error) });
      }
    })();
  }, config.heartbeatMs);
  return new Promise<never>(() => undefined);
}

if (ENVOY_SESSION_STALL_CHILD !== undefined) {
  // A child runs the session above until it is killed and never reaches the suite below.
  await runSessionChild(ChildConfigSchema.parse(JSON.parse(ENVOY_SESSION_STALL_CHILD)));
}

// Below the child branch, and not above it: a child runs under `bun run`, where the hooks
// `useEnvoyStackDirectory` registers are not the test runner's and throw. The directory is the
// stack's scratch directory; reserved at this file's module scope, it is removed once the
// `describe` below has stopped the stack, whether or not the suite reached `stop` (a SIGKILL'd
// child, a failed start).
const stackDirectory = useEnvoyStackDirectory();

/** A session process the rig can stall, as the parent holds it. */
interface StalledSession {
  readonly sessionID: string;
  readonly role: string;
  /** The `payload_summary` of every message it has read back, in arrival order. */
  messages(): readonly string[];
  /** Freezes it as a loaded box does: no heartbeat, no reads, no socket closed, no
   *  deregistration. */
  freeze(): void;
  /** Continues it. */
  thaw(): void;
  /** Kills it outright, so nothing deregisters it: what a pane whose process is killed leaves. */
  kill(): Promise<void>;
  /** Whatever it has written to stderr, for an error that names why it stopped registering. */
  stderr(): string;
}

/** Starts the child, and answers it at once with the wait for the listener's acceptance of its
 *  registration and its claim, so the caller holds the child before that wait ends. The wait
 *  fails by name, killing the child, once natsd or the listener of `stack` has exited, once the
 *  child has, or once the listener has refused the claim: a refused claim is an answer, not a
 *  thing to wait out. It has no deadline of its own: how long a child `bun run` takes to register
 *  and claim is the host's speed, and the wait, `until`'s, fails at its row's deadline. */
function startStalledSession(
  config: ChildConfig,
  stack: EnvoyStack
): { readonly session: StalledSession; readonly started: Promise<void> } {
  const child = Bun.spawn(["bun", "run", import.meta.path], {
    env: { ...process.env, ENVOY_SESSION_STALL_CHILD: JSON.stringify(config) },
    stdout: "pipe",
    stderr: "pipe",
  });
  let registered = false;
  // Undefined until the child says what the listener answered its claim.
  let claimed: boolean | undefined;
  const messages: string[] = [];
  let errors = "";
  const read = async (
    stream: ReadableStream<Uint8Array>,
    onLine: (line: string) => void
  ): Promise<void> => {
    const decoder = new TextDecoder();
    let rest = "";
    for await (const chunk of stream) {
      const lines = (rest + decoder.decode(chunk, { stream: true })).split("\n");
      rest = lines.pop() ?? "";
      for (const line of lines) onLine(line);
    }
  };
  void read(child.stdout, (line) => {
    if (line.trim() === "") return;
    const event = ChildEventSchema.parse(JSON.parse(line));
    if (event.event === "registered") registered = true;
    if (event.event === "role") claimed = event.claimed;
    if (event.event === "message") messages.push(event.text);
    // A gap says the stream no longer holds something this session was away for, which is the
    // one explanation of a message that never arrives: it is kept beside the failures.
    if (event.event === "gap") errors += `a gap notice: ${event.message}\n`;
    if (event.event === "failed") errors += `${event.what}: ${event.error}\n`;
  }).catch((error: unknown) => {
    errors += `reading the session's output: ${String(error)}\n`;
  });
  void read(child.stderr, (line) => {
    errors += `${line}\n`;
  }).catch(() => undefined);
  const session: StalledSession = {
    sessionID: config.sessionID,
    role: config.role,
    messages: () => messages,
    freeze: () => child.kill("SIGSTOP"),
    // A child that has exited is not continued: the last property kills it, and the describe's
    // `afterEach` thaws whatever a row left.
    thaw: () => {
      if (child.exitCode === null && child.signalCode === null) child.kill("SIGCONT");
    },
    kill: async () => {
      // Idempotent: the last property kills it, and the suite's teardown asks again.
      if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
      await child.exited;
    },
    stderr: () => errors,
  };
  const started = until(
    `${config.sessionID}'s registration and its claim of ${config.role}`,
    async () => {
      if (registered && claimed === true) return true;
      if (claimed === false) {
        await session.kill();
        throw new Error(
          `the listener refused the stalled session's claim of ${config.role}: ${errors}`
        );
      }
      const exited = stack.exited();
      if (exited !== undefined) {
        await session.kill();
        throw new Error(`waiting for the stalled session's registration and claim: ${exited}`);
      }
      if (child.exitCode !== null || child.signalCode !== null) {
        await session.kill();
        const code = child.exitCode ?? child.signalCode;
        throw new Error(
          `the stalled session exited (${code}) before it claimed ${config.role}: ${errors}`
        );
      }
      return false;
    }
  ).then(() => undefined);
  return { session, started };
}

/** What `GET /v1/registry/<id>?include=unresponsive` says about one session. */
interface Sighting {
  readonly state: SessionState;
  /** Its last registration (Unix ms), 0 when the listener remembers none. */
  readonly lastSeen: number;
}

/** Reads `sessionID`'s `Sighting`. The read ends once `ended`, its row's signal, has aborted. */
async function sighting(
  envoyUrl: string,
  sessionID: string,
  ended: AbortSignal
): Promise<Sighting> {
  const path = `/v1/registry/${encodeURIComponent(sessionID)}?include=unresponsive`;
  const response = await fetch(`${envoyUrl}${path}`, { signal: ended });
  const body: unknown = await response.json();
  if (response.ok) {
    const row = SessionRowSchema.parse(body);
    return { state: row.state, lastSeen: row.last_seen };
  }
  if (response.status !== 404) {
    throw new Error(`GET ${path} answered ${response.status}: ${JSON.stringify(body)}`);
  }
  const refusal = NoLiveSessionSchema.parse(body);
  return { state: refusal.state, lastSeen: refusal.last_seen ?? 0 };
}

/**
 * One read of a wait that is `doing` something in the row whose signal is `ended`. It first
 * throws once that row has ended, then once natsd or the listener of `stack` has exited
 * (`throwIfExited`). Once the row has ended it also throws in place of the read's answer, so a
 * wait of a row that ended stops there and its row goes no further. A read that fails once natsd
 * or the listener has exited throws naming that exit, with the read's error as its cause: with
 * the listener gone, a fetch fails on a refused connection, which names neither the exit nor its
 * log tail.
 */
async function rowRead<T>(
  stack: EnvoyStack,
  ended: AbortSignal,
  doing: string,
  read: () => T | Promise<T>
): Promise<T> {
  throwIfRowEnded(ended, doing);
  throwIfExited(stack, doing);
  return await readInRow(ended, doing, () => stack.exited(), read);
}

/** Waits for a fact read in the row, stopping on row end or stack exit. */
function untilRow<T>(
  what: string,
  stack: EnvoyStack,
  ended: AbortSignal,
  read: () => T | Promise<T>
) {
  return until(what, () => rowRead(stack, ended, `waiting for ${what}`, read));
}

/** Returns the session's sighting once its state matches `want`. */
function waitForState(
  stack: EnvoyStack,
  ended: AbortSignal,
  sessionID: string,
  want: SessionState,
  what = `${sessionID} to read ${want}`
): Promise<Sighting> {
  return untilRow(what, stack, ended, async () => {
    const seen = await sighting(stack.envoyUrl, sessionID, ended);
    return seen.state === want ? seen : undefined;
  });
}

type RoleAnswer =
  | { readonly status: number; readonly holder: z.infer<typeof RoleHolderSchema> }
  | { readonly status: number; readonly refusal: z.infer<typeof RoleRefusalSchema> };

/** `GET /v1/roles/<role>`: its holder, or the 404 naming why the listener routes nothing there.
 *  The read ends once `ended`, its row's signal, has aborted. */
async function roleAnswer(envoyUrl: string, role: string, ended: AbortSignal): Promise<RoleAnswer> {
  const response = await fetch(`${envoyUrl}/v1/roles/${encodeURIComponent(role)}`, {
    signal: ended,
  });
  const body: unknown = await response.json();
  return response.ok
    ? { status: response.status, holder: RoleHolderSchema.parse(body) }
    : { status: response.status, refusal: RoleRefusalSchema.parse(body) };
}

/** The claim the role bucket holds, or undefined once something released it. */
async function roleClaim(claims: KV, role: string): Promise<RoleClaim | undefined> {
  const entry = await claims.get(role);
  if (entry === null || entry.operation !== "PUT") return undefined;
  return RoleClaimSchema.parse(JSON.parse(entry.string()));
}

describe.skipIf(!envoyStackEnabled)("a session its box stalls", () => {
  let stack: EnvoyStack | undefined;
  let stopStack: (() => Promise<void>) | undefined;
  let nats: NatsConnection | undefined;
  let claims: KV | undefined;
  let child: StalledSession | undefined;
  let client: EnvoyClient | undefined;
  /** This process's clock just before the child's spawn: every registration of the child, and so
   *  every stamp the listener gives one, comes after it. */
  let spawnedAt = 0;

  // The signal of the row running. Every wait and listener read of a row takes it, each freeze
  // or thaw that follows an await checks it first, and the `afterEach` below aborts it once the
  // row has ended, at its guard included. A row that ended then stops at its next wait, listener
  // read, freeze or thaw, so it freezes or thaws nothing inside a later row, on the `child` all
  // three rows share.
  let rowEnd = new AbortController();

  /** The started rig, so each test reads one checked set of handles, with its row's signal.
   *  `child` is held from its spawn, so the rig reads as started only once `claims`, which the
   *  `beforeAll` sets after the child's start, is set too. */
  const rig = () => {
    if (
      stack === undefined ||
      claims === undefined ||
      child === undefined ||
      client === undefined
    ) {
      throw new Error("the session-stall rig did not start");
    }
    return { stack, claims, child, client, ended: rowEnd.signal };
  };
  /** The rig once the child reads live: a row that a failure or its guard cut off between a
   *  freeze and its thaw leaves the child continued by the `afterEach` below, but not yet
   *  registered again, so each row starts from a live session whatever the row before it did. */
  const liveRig = async () => {
    const started = rig();
    const { sessionID } = started.child;
    await waitForState(started.stack, started.ended, sessionID, "live");
    return started;
  };

  beforeAll(async () => {
    const start = startStoppableEnvoyStack({
      work: stackDirectory,
      listenerEnv: {
        ENVOY_SESSION_LIVE_WINDOW: goDuration(LIVE_WINDOW_MS),
        ENVOY_SESSION_GONE_HORIZON: goDuration(GONE_HORIZON_MS),
        ENVOY_SESSION_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
        ENVOY_ROLE_CLAIM_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
        ENVOY_SESSION_MAX_AGE: goDuration(SESSION_MAX_AGE_MS),
      },
    });
    stopStack = start.stop;
    // What can hang is the stack's start, which builds natsd and the listener with `go build`
    // and waits for each to serve: the start the live describes of pi-envoy and claude-envoy
    // make. Then the stalled session's start and the NATS connection after it.
    const started = await settled("the Envoy stack's start", start.started);
    stack = started;
    client = createEnvoyClient({ baseUrl: started.envoyUrl, fetch });
    spawnedAt = Date.now();
    const childStart = startStalledSession(
      {
        envoyUrl: started.envoyUrl,
        natsUrl: started.natsUrl,
        sessionID: `ses_stalled_${process.pid}`,
        role: "session-stall-reviewer",
        directory: "/test/session-stall",
        title: "Stalled session",
        heartbeatMs: HEARTBEAT_MS,
      },
      started
    );
    // Held before the wait, so the `afterAll` kills a child whose start failed or whose wait gave
    // up.
    child = childStart.session;
    await childStart.started;
    nats = await connect({ servers: [started.natsUrl], name: "session-stall-observer" });
    claims = await nats.jetstream().views.kv(ROLE_CLAIM_BUCKET, { bindOnly: true });
  });

  afterAll(async () => {
    // What can hang is the wait for the child's exit, the observer's NATS close, or the stop of
    // natsd or the listener.
    if (child !== undefined) await settled("the stalled session's exit", child.kill());
    if (nats !== undefined) await settled("the observer's NATS close", nats.close());
    if (stopStack !== undefined) await settled("the Envoy stack's stop", stopStack());
  });

  beforeEach(() => {
    rowEnd = new AbortController();
  });
  // It aborts the row's signal and continues the child, and waits on nothing. A row that ended
  // between its freeze and its thaw leaves the child running again for the row after it.
  afterEach(() => {
    rowEnd.abort(new Error("the row ended"));
    child?.thaw();
  });

  // Properties 1, 2 and 3. A listener that read a missed heartbeat as death answers this session
  // nothing at all: no state to read, no last_seen to date the stall by, and an `include` that
  // can list nothing, because the record is gone the moment the window passes.
  test("a stopped session reads unresponsive with its last registration, is listed only when the caller asks for it, and SIGCONT returns it to live", async () => {
    const { stack, child, client, ended } = await liveRig();
    // The listener's own stamp of the last registration it held before the freeze. A
    // registration already on its way when the freeze comes can still land after it, so the
    // stalled record holds this stamp or a later one, and nothing the child sends after the freeze.
    const beforeFreeze = (await sighting(stack.envoyUrl, child.sessionID, ended)).lastSeen;
    throwIfRowEnded(ended, "freezing the session");
    child.freeze();

    const stalled = await waitForState(stack, ended, child.sessionID, "unresponsive");

    // (1) The state is its record's age, and `last_seen` is a registration it made before it
    // froze. Each assertion below holds however slow the host: a later read moves `Date.now()`
    // on, and a stamp never moves back. The stamp read before the freeze is in this process's
    // milliseconds: no earlier than the child's spawn.
    expect(beforeFreeze).toBeGreaterThanOrEqual(spawnedAt);
    // The stalled record's `last_seen` is at least the stamp read before the freeze: the freeze
    // took no registration back, and one already on its way when it came may have moved it on.
    expect(stalled.lastSeen).toBeGreaterThanOrEqual(beforeFreeze);
    // It is past the live window when the listener reads the session as stalled, so it is not
    // the moment the listener noticed.
    expect(Date.now() - stalled.lastSeen).toBeGreaterThan(LIVE_WINDOW_MS);

    // (2) The default listing is unchanged for every caller that only ever wanted live
    // sessions; the stalled row is there for a caller that asks, with the same last_seen.
    const byDefault = await client.listSessions();
    expect(byDefault.map((row) => row.session_id)).not.toContain(child.sessionID);
    const asked = await client.listSessions({ include: "unresponsive" });
    const row = asked.find((listed) => listed.session_id === child.sessionID);
    expect(row?.state).toBe("unresponsive");
    expect(row?.last_seen).toBe(stalled.lastSeen);

    // The stall moves nothing: its last registration is still the one it froze on.
    expect((await sighting(stack.envoyUrl, child.sessionID, ended)).lastSeen).toBe(
      stalled.lastSeen
    );

    // (3) The box comes back, the heartbeat resumes, and the same session is live again. Once the
    // row has ended the body stops here, rather than thaw the session inside a later row.
    throwIfRowEnded(ended, "thawing the session");
    child.thaw();
    const resumed = await waitForState(
      stack,
      ended,
      child.sessionID,
      "live",
      `${child.sessionID} to read live again`
    );
    expect(resumed.lastSeen).toBeGreaterThan(stalled.lastSeen);
    expect((await client.listSessions()).map((listed) => listed.session_id)).toContain(
      child.sessionID
    );
    // What can hang is a wait for a state that never comes.
  });

  // Properties 4 and 5. A listener that read the stall as death releases this claim on the first
  // lookup and refuses the send with "no live session", so the role goes to whoever asks next and
  // the message is never stored for the session that was only stalled.
  test("a stalled session keeps its role claim through any number of lookups, and a send to it is stored and arrives when it answers again", async () => {
    const { stack, claims, child, client, ended } = await liveRig();
    const live = `stall-live-${crypto.randomUUID()}`;
    const sentLive = delivered(
      await client.send({
        source: "agent",
        sourceSessionID: OBSERVER_SESSION,
        targetSessionID: child.sessionID,
        message: live,
      })
    );
    expect(sentLive.recipientState).toBe("live");
    const reachedLive = `${live} to reach the live session`;
    await untilRow(reachedLive, stack, ended, () => child.messages().includes(live));
    const claimed = await roleClaim(claims, child.role);
    expect(claimed?.holder_session_id).toBe(child.sessionID);

    throwIfRowEnded(ended, "freezing the session");
    child.freeze();
    const stalled = await waitForState(stack, ended, child.sessionID, "unresponsive");

    // (4) Every lookup answers the 404 it always did, now saying whose claim it is, that the
    // holder is stalled rather than gone, and that it released nothing. Five of them, because
    // the release this replaces happened inside the lookup: one used to be enough to lose it.
    for (let lookup = 0; lookup < 5; lookup += 1) {
      const answer = await roleAnswer(stack.envoyUrl, child.role, ended);
      expect(answer.status).toBe(404);
      expect("refusal" in answer ? answer.refusal : undefined).toMatchObject({
        reason: "holder_lapsed",
        holder: child.sessionID,
        holder_state: "unresponsive",
        last_seen: stalled.lastSeen,
        claim_released: false,
      });
    }
    // The claim record itself, holder and claimed_at, is byte for byte the one it made.
    expect(await roleClaim(claims, child.role)).toEqual(claimed);

    // (5) A send to it is taken and stored, naming the state it was taken for, and reaches the
    // session when it answers again — which is what a stalled pane must find waiting for it.
    const missed = `stall-missed-${crypto.randomUUID()}`;
    const sentStalled = delivered(
      await client.send({
        source: "agent",
        sourceSessionID: OBSERVER_SESSION,
        targetSessionID: child.sessionID,
        message: missed,
      })
    );
    expect(sentStalled.recipientState).toBe("unresponsive");
    expect(child.messages()).not.toContain(missed);

    throwIfRowEnded(ended, "thawing the session");
    child.thaw();
    const reachedMissed = `${missed}, sent while the session was stalled, to reach it`;
    await untilRow(reachedMissed, stack, ended, () => child.messages().includes(missed));
    await waitForState(
      stack,
      ended,
      child.sessionID,
      "live",
      `${child.sessionID} to read live again`
    );
    const back = await roleAnswer(stack.envoyUrl, child.role, ended);
    expect("holder" in back ? back.holder.holder : undefined).toBe(child.sessionID);
    expect(await roleClaim(claims, child.role)).toEqual(claimed);
    // What can hang is a wait for a state or a message that never comes.
  });

  // Property 6. The horizon is the whole point: a listener that read the missed heartbeats as
  // death would release this claim within the live window — within seconds of a stall a loaded
  // box recovers from — instead of holding it for the hour a session has to come back in.
  test("a killed session stays unresponsive until the gone horizon, then reads gone, and only then does the reaper release its role claim", async () => {
    const { stack, claims, child, ended } = await liveRig();
    const claimed = await roleClaim(claims, child.role);
    expect(claimed?.holder_session_id).toBe(child.sessionID);

    await settled("the killed session's exit", child.kill());

    const stalled = await waitForState(stack, ended, child.sessionID, "unresponsive");
    // Past the live window it is unresponsive, not gone, and its claim stands: nothing can yet
    // tell this from a box that will answer again in a minute.
    const whileStalled = await roleAnswer(stack.envoyUrl, child.role, ended);
    expect("refusal" in whileStalled ? whileStalled.refusal : undefined).toMatchObject({
      holder: child.sessionID,
      holder_state: "unresponsive",
      claim_released: false,
    });
    expect(await roleClaim(claims, child.role)).toEqual(claimed);

    // The role claim reaper runs on its own REAP_INTERVAL_MS pass, as the session reaper does, and
    // passes over an unresponsive holder every time: for the whole horizon the claim stays the
    // one the child made. Every read here is taken a pass short of the horizon
    // (stalled.lastSeen + GONE_HORIZON_MS), which is the earliest moment any pass may release it.
    // A read every pass proves an absence, a release that does not come, so each pause between
    // two reads is a span.
    const reading = "reading the claim to the horizon";
    while (Date.now() < stalled.lastSeen + GONE_HORIZON_MS - REAP_INTERVAL_MS) {
      expect(await rowRead(stack, ended, reading, () => roleClaim(claims, child.role))).toEqual(
        claimed
      );
      await span(
        REAP_INTERVAL_MS,
        "the claim must stand through each reaper pass up to the horizon, and each pass runs on " +
          "the listener's own clock, which no state the row holds and no later action reaches"
      );
    }

    await waitForState(stack, ended, child.sessionID, "gone");
    // It reached the horizon by its record's age, not by anything the rig did to it.
    expect(Date.now() - stalled.lastSeen).toBeGreaterThan(GONE_HORIZON_MS);

    // Only the role claim reaper releases the claim, and only now that the holder is gone. It
    // runs on the listener's own reap interval, so from here its pass can fall between any two
    // reads and no read can hold that moment still. What a lookup does is read on each side of
    // the pass instead, and on each side it is nothing: while the claim stands a lookup answers
    // holder_lapsed for a gone holder, says it released nothing and leaves the record byte for
    // byte the one the child made, and once the reaper has taken it a lookup answers unclaimed.
    // No answer ever reports a claim a lookup released.
    const released = "the role claim reaper to release the gone holder's claim";
    await untilRow(released, stack, ended, async () => {
      const answer = await roleAnswer(stack.envoyUrl, child.role, ended);
      const refusal = "refusal" in answer ? answer.refusal : undefined;
      expect(answer.status).toBe(404);
      expect(refusal?.claim_released ?? false).toBe(false);
      if (refusal?.reason === "unclaimed") return true;
      expect(refusal).toMatchObject({
        reason: "holder_lapsed",
        holder: child.sessionID,
        holder_state: "gone",
        claim_released: false,
      });
      // The record itself is the claim the child made until the reaper's pass takes it away:
      // a lookup never writes a third thing into the role bucket.
      expect([claimed, undefined]).toContainEqual(await roleClaim(claims, child.role));
      return false;
    });
    expect(await roleClaim(claims, child.role)).toBeUndefined();
    // What can hang is a wait for a state or a release that never comes.
  });
});
