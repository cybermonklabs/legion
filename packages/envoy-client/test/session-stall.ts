/**
 * The committed regression rig for the three session states (dispatch://INFRA-392): a session
 * whose box stalls it is `unresponsive`, not dead, and keeps its record, its role claim and its
 * messages until the gone horizon.
 *
 * Its first two rigs run no agent and need no model route. The stalled session is a child Bun
 * process of this file (`runSessionChild`) carrying this package's real
 * registration and heartbeat — `createEnvoyClient(...).subscribe`, the call every plugin's
 * heartbeat repeats (`registerSession`, packages/pi-envoy/extensions/envoy.ts), with the real
 * role claim and the real session consumer beside it: the child subscribes its relay subject and
 * reads what the stream stored for it through `createConsumerDelivery`, the one path a message
 * reaches a session by — against the real listener and real natsd test/envoy-stack.ts builds
 * from this checkout. The stall is `SIGSTOP`, which stops exactly what a loaded box stops: the
 * heartbeat, with nothing closed and nothing deregistered.
 * `SIGCONT` resumes it and `SIGKILL` ends it with no deregistration, as a killed pane leaves it.
 * The first rig holds the states themselves; the second, what a stalled session keeps across a
 * second listener and its restart, an outage of the bus, and a Dispatch server that reads the
 * holder of a routed event's role before it hands the event over. The third runs the pinned Oh
 * My Pi and Claude Code themselves against a model stand-in, for what a host does with the
 * session it leaves.
 *
 * Run it from packages/envoy-client with `bun test ./test/session-stall.ts` and
 * LEGION_E2E_ENVOY_STACK=1, with LEGION_E2E_DISPATCH_STACK=1 and DISPATCH_TEST_DATABASE_URL for
 * the second rig and LEGION_TEST_OMP and LEGION_TEST_CLAUDE for the third (the
 * envoy-session-stall job sets all of them). The kernel must let a test stop its child with
 * SIGSTOP without hanging up the test's own process group, which gVisor does not.
 */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdir, writeFile } from "node:fs/promises";
import * as path from "node:path";
import { EnvelopeSchema, legacyAgentSubject } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { requireOrSkip } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { connect, Events, type KV, type NatsConnection, StringCodec } from "nats";
import { z } from "zod";
// Relative rather than by package name: @legion/pi-shared depends on this package, so this package
// cannot depend on it, and the harness is test code that only this rig and the plugins' own tests
// run (scripts/e2e/lib/messages-standin.ts imports it the same way).
import {
  type Block,
  type Cleanup,
  messageStream,
  ompRoot,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "../../pi-shared/test/omp-harness";
import { createConsumerDelivery } from "../src/consumer-delivery";
import { createDeliveryLedger } from "../src/delivery-ledger";
import { createEnvoyClient, type EnvoyClient, type SessionState } from "../src/transport";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
  useDispatchStackDirectory,
} from "./dispatch-stack";
import {
  type EnvoyStack,
  envoyStackEnabled,
  startEnvoyStack,
  useEnvoyStackDirectory,
} from "./envoy-stack";

// Destructured: claude-envoy's typecheck, which compiles this directory, forbids `process.env.X`.
// LEGION_TEST_OMP and LEGION_TEST_CLAUDE name the pinned Oh My Pi and Claude Code the third rig
// runs (install-omp and install-claude-code set them in the envoy-session-stall job).
const {
  ENVOY_SESSION_STALL_CHILD,
  LEGION_TEST_OMP,
  LEGION_TEST_CLAUDE,
  PATH: SHELL_PATH = "",
} = process.env;

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

/** The gap allowed between a registration the child reports and the `last_seen` the listener
 *  stamped for it: one request's latency, plus room for a registration the freeze cut off
 *  between the listener's write and the child's own line. */
const REGISTRATION_SLACK_MS = 2_000;

const POLL_MS = 200;
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
  z.object({ event: z.literal("registered"), at: z.number().int() }),
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
    say({ event: "registered", at: Date.now() });
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
// The second rig's: an Envoy stack of its own and a Dispatch server on it (the describe below the
// first one).
const keptStackDirectory = useEnvoyStackDirectory();
const keptDispatchDirectory = useDispatchStackDirectory();
// The third rig's: an Envoy stack of its own, and the real hosts' homes, profiles and the
// claude-envoy plugin built from this checkout.
const hostsStackDirectory = useEnvoyStackDirectory();
const hostsDirectory = temporaryDirectory("session-stall-hosts");

/** A session process the rig can stall, as the parent holds it. */
interface StalledSession {
  readonly sessionID: string;
  readonly role: string;
  /** When the child's last accepted registration landed, by its own clock. */
  registeredAt(): number;
  /** The `payload_summary` of every message it has read back, in arrival order. */
  messages(): readonly string[];
  /** Freezes it as a loaded box does: no heartbeat, no reads, no socket closed, no
   *  deregistration. Answers the moment of the freeze. */
  freeze(): number;
  /** Continues it. */
  thaw(): void;
  /** Kills it outright, so nothing deregisters it: what a pane whose process is killed leaves. */
  kill(): Promise<void>;
  /** Whatever it has written to stderr, for an error that names why it stopped registering. */
  stderr(): string;
}

/** Starts the child and resolves once the listener has accepted its registration and its claim. */
async function startStalledSession(config: ChildConfig): Promise<StalledSession> {
  const child = Bun.spawn(["bun", "run", import.meta.path], {
    env: { ...process.env, ENVOY_SESSION_STALL_CHILD: JSON.stringify(config) },
    stdout: "pipe",
    stderr: "pipe",
  });
  let registeredAt = 0;
  let claimed = false;
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
    if (event.event === "registered") registeredAt = event.at;
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
    registeredAt: () => registeredAt,
    messages: () => messages,
    freeze: () => {
      child.kill("SIGSTOP");
      return Date.now();
    },
    thaw: () => child.kill("SIGCONT"),
    kill: async () => {
      // Idempotent: the last property kills it, and the suite's teardown asks again.
      if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
      await child.exited;
    },
    stderr: () => errors,
  };
  const deadline = Date.now() + 30_000;
  while (registeredAt === 0 || !claimed) {
    if (child.exitCode !== null || Date.now() > deadline) {
      await session.kill();
      throw new Error(`the stalled session did not register and claim ${config.role}: ${errors}`);
    }
    await Bun.sleep(POLL_MS);
  }
  return session;
}

/** What `GET /v1/registry/<id>?include=unresponsive` says about one session. */
interface Sighting {
  readonly state: SessionState;
  /** Its last registration (Unix ms), 0 when the listener remembers none. */
  readonly lastSeen: number;
}

async function sighting(envoyUrl: string, sessionID: string): Promise<Sighting> {
  const route = `/v1/registry/${encodeURIComponent(sessionID)}?include=unresponsive`;
  const response = await fetch(`${envoyUrl}${route}`);
  const body: unknown = await response.json();
  if (response.ok) {
    const row = SessionRowSchema.parse(body);
    return { state: row.state, lastSeen: row.last_seen };
  }
  if (response.status !== 404) {
    throw new Error(`GET ${route} answered ${response.status}: ${JSON.stringify(body)}`);
  }
  const refusal = NoLiveSessionSchema.parse(body);
  return { state: refusal.state, lastSeen: refusal.last_seen ?? 0 };
}

/** Polls the session until it reads `want`, throwing with what it did read when `deadline` passes. */
async function awaitState(
  envoyUrl: string,
  sessionID: string,
  want: SessionState,
  deadline: number
): Promise<Sighting> {
  let seen = await sighting(envoyUrl, sessionID);
  while (seen.state !== want) {
    if (Date.now() > deadline) {
      throw new Error(`${sessionID} still reads ${seen.state}, not ${want}, at the deadline`);
    }
    await Bun.sleep(POLL_MS);
    seen = await sighting(envoyUrl, sessionID);
  }
  return seen;
}

/** Polls `condition` until it holds, throwing `what` when `deadline` passes. */
async function until(
  what: string,
  deadline: number,
  condition: () => boolean | Promise<boolean>
): Promise<void> {
  while (!(await condition())) {
    if (Date.now() > deadline) throw new Error(`${what} did not happen before the deadline`);
    await Bun.sleep(POLL_MS);
  }
}

type RoleAnswer =
  | { readonly status: number; readonly holder: z.infer<typeof RoleHolderSchema> }
  | { readonly status: number; readonly refusal: z.infer<typeof RoleRefusalSchema> };

/** `GET /v1/roles/<role>`: its holder, or the 404 naming why the listener routes nothing there.
 *  Any other answer names neither - the 503 of a bus the listener could not read the claim or its
 *  holder's session through (writeBusUnavailable, packages/envoy/cmd/listener/api.go), or the 500
 *  of an unreadable claim - and is thrown with its status and body. */
async function roleAnswer(envoyUrl: string, role: string): Promise<RoleAnswer> {
  const route = `/v1/roles/${encodeURIComponent(role)}`;
  const response = await fetch(`${envoyUrl}${route}`);
  const body: unknown = await response.json();
  if (response.ok) return { status: response.status, holder: RoleHolderSchema.parse(body) };
  if (response.status !== 404) {
    throw new Error(`GET ${route} answered ${response.status}: ${JSON.stringify(body)}`);
  }
  return { status: response.status, refusal: RoleRefusalSchema.parse(body) };
}

/** A role lookup that waits out the listener's 503 once the bus is back, as awaitStateThrough
 *  waits out the session read: the lookup reads the claim from the bus itself, within one
 *  request's budget, and is refused until that read is served again. */
async function roleAnswerThrough(
  envoyUrl: string,
  role: string,
  deadline: number
): Promise<RoleAnswer> {
  for (;;) {
    try {
      return await roleAnswer(envoyUrl, role);
    } catch (error) {
      if (Date.now() > deadline) throw error;
    }
    await Bun.sleep(POLL_MS);
  }
}

/** The claim the role bucket holds, or undefined once something released it. */
async function roleClaim(claims: KV, role: string): Promise<RoleClaim | undefined> {
  const entry = await claims.get(role);
  if (entry === null || entry.operation !== "PUT") return undefined;
  return RoleClaimSchema.parse(JSON.parse(entry.string()));
}

describe.skipIf(!envoyStackEnabled)("a session its box stalls", () => {
  let stack: EnvoyStack | undefined;
  let nats: NatsConnection | undefined;
  let claims: KV | undefined;
  let child: StalledSession | undefined;
  let client: EnvoyClient | undefined;

  /** The started rig, so each test reads one checked set of handles. */
  const rig = () => {
    if (
      stack === undefined ||
      claims === undefined ||
      child === undefined ||
      client === undefined
    ) {
      throw new Error("the session-stall rig did not start");
    }
    return { envoyUrl: stack.envoyUrl, claims, child, client };
  };

  beforeAll(async () => {
    const started = await startEnvoyStack({
      work: stackDirectory,
      listenerEnv: {
        ENVOY_SESSION_LIVE_WINDOW: goDuration(LIVE_WINDOW_MS),
        ENVOY_SESSION_GONE_HORIZON: goDuration(GONE_HORIZON_MS),
        ENVOY_SESSION_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
        ENVOY_ROLE_CLAIM_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
        ENVOY_SESSION_MAX_AGE: goDuration(SESSION_MAX_AGE_MS),
      },
    });
    stack = started;
    client = createEnvoyClient({ baseUrl: started.envoyUrl, fetch });
    child = await startStalledSession({
      envoyUrl: started.envoyUrl,
      natsUrl: started.natsUrl,
      sessionID: `ses_stalled_${process.pid}`,
      role: "session-stall-reviewer",
      directory: "/test/session-stall",
      title: "Stalled session",
      heartbeatMs: HEARTBEAT_MS,
    });
    nats = await connect({ servers: [started.natsUrl], name: "session-stall-observer" });
    claims = await nats.jetstream().views.kv(ROLE_CLAIM_BUCKET, { bindOnly: true });
  }, 180_000);

  afterAll(async () => {
    await child?.kill();
    await nats?.close();
    await stack?.stop();
  }, 60_000);

  // Properties 1, 2 and 3. A listener that read a missed heartbeat as death answers this session
  // nothing at all: no state to read, no last_seen to date the stall by, and an `include` that
  // can list nothing, because the record is gone the moment the window passes.
  test("a stopped session reads unresponsive with its last registration, is listed only when the caller asks for it, and SIGCONT returns it to live", async () => {
    const { envoyUrl, child, client } = rig();
    const frozenAt = child.freeze();
    // Let a registration the freeze may have cut between the listener's write and the child's
    // own line land before either is read.
    await Bun.sleep(HEARTBEAT_MS);
    const registered = child.registeredAt();

    const stalled = await awaitState(
      envoyUrl,
      child.sessionID,
      "unresponsive",
      frozenAt + LIVE_WINDOW_MS + 20_000
    );

    // (1) The state is its record's age, and `last_seen` is the registration it made before it
    // froze — not the moment the listener noticed, which is a whole live window later.
    expect(Math.abs(stalled.lastSeen - registered)).toBeLessThan(REGISTRATION_SLACK_MS);
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
    expect((await sighting(envoyUrl, child.sessionID)).lastSeen).toBe(stalled.lastSeen);

    // (3) The box comes back, the heartbeat resumes, and the same session is live again.
    child.thaw();
    const resumed = await awaitState(
      envoyUrl,
      child.sessionID,
      "live",
      Date.now() + HEARTBEAT_MS * 3 + 20_000
    );
    expect(resumed.lastSeen).toBeGreaterThan(stalled.lastSeen);
    expect((await client.listSessions()).map((listed) => listed.session_id)).toContain(
      child.sessionID
    );
  }, 180_000);

  // Properties 4 and 5. A listener that read the stall as death releases this claim on the first
  // lookup and refuses the send with "no live session", so the role goes to whoever asks next and
  // the message is never stored for the session that was only stalled.
  test("a stalled session keeps its role claim through any number of lookups, and a send to it is stored and arrives when it answers again", async () => {
    const { envoyUrl, claims, child, client } = rig();
    const live = `stall-live-${crypto.randomUUID()}`;
    const sentLive = await client.send({
      source: "agent",
      sourceSessionID: OBSERVER_SESSION,
      targetSessionID: child.sessionID,
      message: live,
    });
    expect(sentLive.recipientState).toBe("live");
    await until(`${live} to reach the live session`, Date.now() + 30_000, () =>
      child.messages().includes(live)
    );
    const claimed = await roleClaim(claims, child.role);
    expect(claimed?.holder_session_id).toBe(child.sessionID);

    const frozenAt = child.freeze();
    const stalled = await awaitState(
      envoyUrl,
      child.sessionID,
      "unresponsive",
      frozenAt + LIVE_WINDOW_MS + 20_000
    );

    // (4) Every lookup answers the 404 it always did, now saying whose claim it is, that the
    // holder is stalled rather than gone, and that it released nothing. Five of them, because
    // the release this replaces happened inside the lookup: one used to be enough to lose it.
    for (let lookup = 0; lookup < 5; lookup += 1) {
      const answer = await roleAnswer(envoyUrl, child.role);
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
    const sentStalled = await client.send({
      source: "agent",
      sourceSessionID: OBSERVER_SESSION,
      targetSessionID: child.sessionID,
      message: missed,
    });
    expect(sentStalled.recipientState).toBe("unresponsive");
    expect(child.messages()).not.toContain(missed);

    child.thaw();
    await until(
      `${missed}, sent while the session was stalled, to reach it`,
      Date.now() + 60_000,
      () => child.messages().includes(missed)
    );
    await awaitState(envoyUrl, child.sessionID, "live", Date.now() + HEARTBEAT_MS * 3 + 20_000);
    const back = await roleAnswer(envoyUrl, child.role);
    expect("holder" in back ? back.holder.holder : undefined).toBe(child.sessionID);
    expect(await roleClaim(claims, child.role)).toEqual(claimed);
  }, 240_000);

  // Property 6. The horizon is the whole point: a listener that read the missed heartbeats as
  // death would release this claim within the live window — within seconds of a stall a loaded
  // box recovers from — instead of holding it for the hour a session has to come back in.
  test("a killed session stays unresponsive until the gone horizon, then reads gone, and only then does the reaper release its role claim", async () => {
    const { envoyUrl, claims, child } = rig();
    const claimed = await roleClaim(claims, child.role);
    expect(claimed?.holder_session_id).toBe(child.sessionID);

    const killedAt = Date.now();
    await child.kill();

    const stalled = await awaitState(
      envoyUrl,
      child.sessionID,
      "unresponsive",
      killedAt + LIVE_WINDOW_MS + 20_000
    );
    // Past the live window it is unresponsive, not gone, and its claim stands: nothing can yet
    // tell this from a box that will answer again in a minute.
    const whileStalled = await roleAnswer(envoyUrl, child.role);
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
    while (Date.now() < stalled.lastSeen + GONE_HORIZON_MS - REAP_INTERVAL_MS) {
      expect(await roleClaim(claims, child.role)).toEqual(claimed);
      await Bun.sleep(REAP_INTERVAL_MS);
    }

    await awaitState(
      envoyUrl,
      child.sessionID,
      "gone",
      stalled.lastSeen + GONE_HORIZON_MS + REAP_INTERVAL_MS + 30_000
    );
    // It reached the horizon by its record's age, not by anything the rig did to it.
    expect(Date.now() - stalled.lastSeen).toBeGreaterThan(GONE_HORIZON_MS);

    // Only the role claim reaper releases the claim, and only now that the holder is gone. It
    // runs on the listener's own reap interval, so from here its pass can fall between any two
    // reads and no read can hold that moment still. What a lookup does is read on each side of
    // the pass instead, and on each side it is nothing: while the claim stands a lookup answers
    // holder_lapsed for a gone holder, says it released nothing and leaves the record byte for
    // byte the one the child made, and once the reaper has taken it a lookup answers unclaimed.
    // No answer ever reports a claim a lookup released.
    await until(
      "the role claim reaper to release the gone holder's claim",
      Date.now() + REAP_INTERVAL_MS + 30_000,
      async () => {
        const answer = await roleAnswer(envoyUrl, child.role);
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
      }
    );
    expect(await roleClaim(claims, child.role)).toBeUndefined();
  }, 240_000);
});

/**
 * The second rig: what a stalled session keeps across the rest of the stack — a second listener
 * and that listener's restart, an outage of the bus itself, and Dispatch, which reads the session's
 * state from the listener before it hands an event routed to the session's role. Its listener's
 * gone horizon is three minutes rather than one, because the outage below stalls two sessions for
 * longer than the first rig's horizon.
 */
const KEPT_GONE_HORIZON_MS = 180_000;
/** How long the bus stays down under two stalled sessions. */
const OUTAGE_MS = 20_000;
/** How long a count waits after the copy it waited for, so that a second copy would be counted. */
const SETTLE_MS = 10_000;
/** How long Dispatch is given to push an event at a stalled holder, which it must not do. */
const HOLD_MS = 20_000;
/** How long the held event may take to reach its holder once it answers again: the outbox's own
 *  back-off (internal/dispatch/retry), which caps one wait at five minutes, and a margin. */
const HELD_FORWARD_MS = 360_000;
/** The person each Dispatch write is made as, under the stack's header identity. */
const PERSON = "alice";
/** The prefix of the dedupe key every role forward of Dispatch's outbox carries. */
const ROLE_FORWARD_KEY = "envoy.role.forward.";

const IssueSchema = z.object({ key: z.string() });
const IssueClaimSchema = z.object({
  claim: z.object({ actor: z.object({ id: z.string() }) }).nullable(),
  holder_state: StateSchema.optional(),
});

/** A sighting that waits out an answer the listener cannot give yet: after the bus comes back the
 *  listener reads its bucket again, and answers 503 until it stands behind its copy. */
async function awaitStateThrough(
  envoyUrl: string,
  sessionID: string,
  want: SessionState,
  deadline: number
): Promise<Sighting> {
  let last = "no answer";
  for (;;) {
    try {
      const seen = await sighting(envoyUrl, sessionID);
      if (seen.state === want) return seen;
      last = seen.state;
    } catch (error) {
      last = String(error);
    }
    if (Date.now() > deadline) {
      throw new Error(`${sessionID} still reads ${last}, not ${want}, at the deadline`);
    }
    await Bun.sleep(POLL_MS);
  }
}

describe.skipIf(!envoyStackEnabled || !dispatchStackEnabled)(
  "what a stalled session keeps across a second listener, a bus outage and Dispatch",
  () => {
    let stack: EnvoyStack | undefined;
    let dispatch: DispatchStack | undefined;
    let nats: NatsConnection | undefined;
    let claims: KV | undefined;
    const project = `STALL${String(process.pid).slice(-4)}`;

    const rig = () => {
      if (stack === undefined || dispatch === undefined || claims === undefined) {
        throw new Error("the kept-claims rig did not start");
      }
      return { stack, dispatch, claims };
    };

    /** A write to Dispatch as the person, or as a machine over the stack's agent bearer. */
    const write = async (
      as: "person" | "machine",
      method: string,
      route: string,
      body?: unknown
    ): Promise<unknown> => {
      const { dispatch } = rig();
      const response = await fetch(`${dispatch.url}${route}`, {
        method,
        headers: {
          "Content-Type": "application/json",
          ...(as === "person"
            ? { "X-Dispatch-User": PERSON }
            : { Authorization: `Bearer ${dispatch.token}` }),
        },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      const text = await response.text();
      if (!response.ok) throw new Error(`${method} ${route}: ${response.status} ${text}`);
      return JSON.parse(text) as unknown;
    };

    const newIssue = async (title: string): Promise<string> =>
      IssueSchema.parse(
        await write("person", "POST", "/api/v1/issues", {
          project,
          title,
          spec: `# ${title}\n\nThe session-stall rig's own issue.\n`,
          force: true,
        })
      ).key;

    /** A stalled-session child on this rig, under a session id and a role of the test's own. The
     *  role spells the name's `_` as `-`: Dispatch takes a role route only when the role is
     *  `[a-z0-9-]` (roleRoutePattern, packages/envoy/internal/dispatch/model/model.go), and the
     *  row that routes an issue to the holder's role would be refused 400 ROUTE_INVALID. */
    const session = (name: string): Promise<StalledSession> => {
      const { stack } = rig();
      return startStalledSession({
        envoyUrl: stack.envoyUrl,
        natsUrl: stack.natsUrl,
        sessionID: `ses_${name}_${process.pid}`,
        role: `session-stall-${name.replaceAll("_", "-")}`,
        directory: `/test/session-stall/${name}`,
        title: `Session ${name}`,
        heartbeatMs: HEARTBEAT_MS,
      });
    };

    beforeAll(async () => {
      const started = await startEnvoyStack({
        work: keptStackDirectory,
        listenerEnv: {
          ENVOY_SESSION_LIVE_WINDOW: goDuration(LIVE_WINDOW_MS),
          ENVOY_SESSION_GONE_HORIZON: goDuration(KEPT_GONE_HORIZON_MS),
          ENVOY_SESSION_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
          ENVOY_ROLE_CLAIM_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
          ENVOY_SESSION_MAX_AGE: goDuration(SESSION_MAX_AGE_MS),
        },
      });
      stack = started;
      // Dispatch reads a role route's holder from the session bucket by windows of its own, so it
      // is given the listener's: with its 5-minute default it would read a stalled holder live and
      // hand it the event the row below holds.
      dispatch = await startDispatchStack({
        work: keptDispatchDirectory,
        envoy: { envoyUrl: started.envoyUrl, natsUrl: started.natsUrl },
        sessionWindows: {
          liveWindow: goDuration(LIVE_WINDOW_MS),
          goneHorizon: goDuration(KEPT_GONE_HORIZON_MS),
        },
      });
      // The observer outlives the outage below: it reconnects for as long as the bus is down.
      nats = await connect({
        servers: [started.natsUrl],
        name: "session-stall-kept-observer",
        maxReconnectAttempts: -1,
        reconnectTimeWait: 500,
      });
      claims = await nats.jetstream().views.kv(ROLE_CLAIM_BUCKET, { bindOnly: true });
      await write("person", "POST", "/api/v1/projects", { key: project, name: "Session stall" });
    }, 600_000);

    afterAll(async () => {
      await nats?.close();
      await dispatch?.stop();
      await stack?.stop();
    }, 120_000);

    // Two listeners judge one session by the one record they both read, so a stalled session is
    // never live on one and unresponsive on the other; and a listener that has only just started,
    // and read the bucket afresh, says what the one that watched all along says.
    test("two listeners read a stalled session the same, and so does one restarted while it is stalled", async () => {
      const { stack } = rig();
      const child = await session("two_listeners");
      let second = await stack.startListener();
      try {
        const frozenAt = child.freeze();
        const onFirst = await awaitState(
          stack.envoyUrl,
          child.sessionID,
          "unresponsive",
          frozenAt + LIVE_WINDOW_MS + 20_000
        );
        const onSecond = await awaitState(
          second.url,
          child.sessionID,
          "unresponsive",
          Date.now() + 20_000
        );
        expect(onSecond.lastSeen).toBe(onFirst.lastSeen);

        await second.stop();
        second = await stack.startListener();
        const restarted = await awaitStateThrough(
          second.url,
          child.sessionID,
          "unresponsive",
          Date.now() + 30_000
        );
        expect(restarted.lastSeen).toBe(onFirst.lastSeen);

        child.thaw();
        const liveFirst = await awaitState(
          stack.envoyUrl,
          child.sessionID,
          "live",
          Date.now() + HEARTBEAT_MS * 3 + 20_000
        );
        const liveSecond = await awaitState(
          second.url,
          child.sessionID,
          "live",
          Date.now() + 20_000
        );
        expect(liveFirst.lastSeen).toBeGreaterThan(onFirst.lastSeen);
        expect(liveSecond.lastSeen).toBeGreaterThan(onFirst.lastSeen);
      } finally {
        await second.stop();
        await child.kill();
      }
    }, 180_000);

    // Dispatch reads an event's role holder from the listener before it hands the event over. A
    // live holder is handed it once, which is this test's control; a stalled holder's event is held
    // rather than pushed at a session that cannot read it, the claim and its claimed_at stand, and
    // once the holder answers again the event is handed over exactly once.
    test(
      "Dispatch holds an event routed to a stalled holder's role and forwards it once when the holder answers",
      async () => {
        const { stack, claims } = rig();
        const holder = await session("routed_holder");
        const watcher = await connect({ servers: [stack.natsUrl], name: "session-stall-forwards" });
        const codec = StringCodec();
        const forwards: string[] = [];
        // The holder's own agent subject, where a role forward lands; this subscriber answers none
        // of them, since the stream's acknowledgement is the forward's answer.
        const subscription = watcher.subscribe(legacyAgentSubject(holder.sessionID));
        void (async () => {
          for await (const message of subscription) forwards.push(codec.decode(message.data));
        })();
        await watcher.flush();
        const forwarded = (text: string): number =>
          forwards.filter((one) => one.includes(text) && one.includes(ROLE_FORWARD_KEY)).length;
        try {
          const issue = await newIssue("A role route reaches its holder");
          await write("machine", "PATCH", `/api/v1/issues/${issue}`, {
            route: `role:${holder.role}`,
            actor: { id: holder.sessionID, kind: "session" },
          });
          const comments = `/api/v1/issues/${issue}/comments`;
          const marker = `stall-routed-${crypto.randomUUID()}`;

          await write("person", "POST", comments, { body: `live ${marker}` });
          await until(
            "the live holder's role forward",
            Date.now() + 60_000,
            () => forwarded(`live ${marker}`) >= 1
          );
          await Bun.sleep(SETTLE_MS);
          expect(forwarded(`live ${marker}`)).toBe(1);

          const claimed = await roleClaim(claims, holder.role);
          expect(claimed?.holder_session_id).toBe(holder.sessionID);
          const frozenAt = holder.freeze();
          await awaitState(
            stack.envoyUrl,
            holder.sessionID,
            "unresponsive",
            frozenAt + LIVE_WINDOW_MS + 20_000
          );
          await write("person", "POST", comments, { body: `stalled ${marker}` });
          await Bun.sleep(HOLD_MS);
          expect(forwarded(`stalled ${marker}`)).toBe(0);
          expect(await roleClaim(claims, holder.role)).toEqual(claimed);
          const lookup = await roleAnswer(stack.envoyUrl, holder.role);
          expect("refusal" in lookup ? lookup.refusal : undefined).toMatchObject({
            reason: "holder_lapsed",
            holder: holder.sessionID,
            holder_state: "unresponsive",
            claim_released: false,
          });

          holder.thaw();
          await awaitState(
            stack.envoyUrl,
            holder.sessionID,
            "live",
            Date.now() + HEARTBEAT_MS * 3 + 20_000
          );
          await until(
            "the held event's role forward",
            Date.now() + HELD_FORWARD_MS,
            () => forwarded(`stalled ${marker}`) >= 1
          );
          await Bun.sleep(SETTLE_MS);
          expect(forwarded(`stalled ${marker}`)).toBe(1);
          expect(await roleClaim(claims, holder.role)).toEqual(claimed);
        } finally {
          subscription.unsubscribe();
          await watcher.close();
          await holder.kill();
        }
      },
      HELD_FORWARD_MS + 180_000
    );

    // The failure this rig exists for is a box under load, where the bus suffers too. With the bus
    // down around two stalled sessions, neither loses its claim: one holds a role and the other an
    // issue in Dispatch, and a claim is released only for a holder past the gone horizon.
    test("a bus outage under two stalled sessions takes neither the role claim of one nor the issue claim of the other", async () => {
      const { stack, claims } = rig();
      const roleHolder = await session("outage_role");
      const issueHolder = await session("outage_issue");
      try {
        const issue = await newIssue("A bus outage keeps the claims");
        await write("machine", "POST", `/api/v1/issues/${issue}/claim`, {
          actor: { kind: "session", id: issueHolder.sessionID },
        });
        const claimed = await roleClaim(claims, roleHolder.role);
        expect(claimed?.holder_session_id).toBe(roleHolder.sessionID);

        const frozenAt = Math.min(roleHolder.freeze(), issueHolder.freeze());
        for (const child of [roleHolder, issueHolder]) {
          await awaitState(
            stack.envoyUrl,
            child.sessionID,
            "unresponsive",
            frozenAt + LIVE_WINDOW_MS + 20_000
          );
        }

        await stack.restartNats(OUTAGE_MS);

        for (const child of [roleHolder, issueHolder]) {
          await awaitStateThrough(
            stack.envoyUrl,
            child.sessionID,
            "unresponsive",
            Date.now() + 60_000
          );
        }
        const lookup = await roleAnswerThrough(
          stack.envoyUrl,
          roleHolder.role,
          Date.now() + 60_000
        );
        expect("refusal" in lookup ? lookup.refusal : undefined).toMatchObject({
          holder: roleHolder.sessionID,
          holder_state: "unresponsive",
          claim_released: false,
        });
        expect(await roleClaim(claims, roleHolder.role)).toEqual(claimed);
        const read = IssueClaimSchema.parse(
          await write("person", "GET", `/api/v1/issues/${issue}`)
        );
        expect(read.claim?.actor.id).toBe(issueHolder.sessionID);
        expect(read.holder_state).toBe("unresponsive");
      } finally {
        await roleHolder.kill();
        await issueHolder.kill();
      }
    }, 240_000);
  }
);

/**
 * The third rig: the two real hosts' own sessions, on a listener with the first rig's short
 * windows. What a host does with the id it leaves when it starts another session is the host's and
 * its plugin's, and no stand-in can say it: Oh My Pi's `new_session` and Claude Code's `/clear`
 * each leave the id they replace gone at once rather than unresponsive until the horizon, and a
 * `claude --resume` of a session gets back the topics it had subscribed. The model is a stand-in
 * that answers both hosts; the listener and natsd are built from this checkout.
 */
const hostsSkipped =
  !envoyStackEnabled ||
  requireOrSkip(
    "LEGION_TEST_OMP and LEGION_TEST_CLAUDE, naming the pinned Oh My Pi and Claude Code",
    LEGION_TEST_OMP !== undefined && LEGION_TEST_CLAUDE !== undefined,
    "envoy-session-stall"
  );
/** How soon an id a host left must read gone: well inside the gone horizon, which is what "at
 *  once" is set against. */
const LEFT_GONE_MS = 20_000;
/** The bound on one host turn, and on a session's first registration. */
const HOST_TURN_MS = 120_000;
const PI_ENVOY_EXTENSION = path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts");
const CLAUDE_ENVOY_BUILD = path.resolve(import.meta.dir, "../../claude-envoy/scripts/build.ts");

const RegistryTopicsSchema = z.object({ topics: z.array(z.string()) });
const SessionListSchema = z.array(
  z.object({ session_id: z.string(), dir: z.string(), topics: z.array(z.string()).nullish() })
);

/** The live rows `GET /v1/sessions` lists for one project directory. */
async function sessionsOf(
  envoyUrl: string,
  directory: string
): Promise<z.infer<typeof SessionListSchema>> {
  const response = await fetch(`${envoyUrl}/v1/sessions`);
  if (!response.ok) throw new Error(`GET /v1/sessions answered ${response.status}`);
  return SessionListSchema.parse(await response.json()).filter((row) => row.dir === directory);
}

/** Whether the session's live registry row carries `topic`. */
async function holdsTopic(envoyUrl: string, sessionID: string, topic: string): Promise<boolean> {
  const response = await fetch(`${envoyUrl}/v1/registry/${encodeURIComponent(sessionID)}`);
  if (!response.ok) return false;
  return RegistryTopicsSchema.parse(await response.json()).topics.includes(topic);
}

/** The text of a Messages request's last user message, its tool results left out. */
function lastUserText(body: Record<string, unknown>): string {
  const messages = z
    .array(z.object({ role: z.string(), content: z.unknown() }))
    .catch([])
    .parse(body.messages);
  const last = messages.filter((message) => message.role === "user").at(-1);
  if (last === undefined) return "";
  if (typeof last.content === "string") return last.content;
  return z
    .array(z.object({ type: z.string(), text: z.string().optional() }))
    .catch([])
    .parse(last.content)
    .map((block) => (block.type === "text" ? (block.text ?? "") : ""))
    .join("\n");
}

/** Whether the request's newest message carries a tool result, which is the turn after a call. */
function answersACall(body: Record<string, unknown>): boolean {
  const messages = z
    .array(z.object({ content: z.unknown() }))
    .catch([])
    .parse(body.messages);
  return JSON.stringify(messages.at(-1)?.content ?? "").includes('"tool_result"');
}

/** The name under which the request offers a tool whose own name is `name`: Claude Code offers an
 *  MCP server's tool as `mcp__<server>__<name>`. */
function offeredTool(body: Record<string, unknown>, name: string): string | undefined {
  return z
    .array(z.object({ name: z.string() }))
    .catch([])
    .parse(body.tools)
    .map((tool) => tool.name)
    .find((offered) => offered === name || offered.endsWith(`__${name}`));
}

describe.skipIf(hostsSkipped)(
  "the real hosts' own sessions on a listener with short windows",
  () => {
    let stack: EnvoyStack | undefined;
    const cleanup: Cleanup = [];
    let modelBase = "";
    /** One line per Messages request the stand-in answered, which a failure below quotes. */
    const exchanges: string[] = [];
    const topic = `notifications.team.session-stall-${process.pid}.all`;

    const rig = () => {
      if (stack === undefined) throw new Error("the real-hosts rig did not start");
      return stack;
    };

    beforeAll(async () => {
      stack = await startEnvoyStack({
        work: hostsStackDirectory,
        listenerEnv: {
          ENVOY_SESSION_LIVE_WINDOW: goDuration(LIVE_WINDOW_MS),
          ENVOY_SESSION_GONE_HORIZON: goDuration(GONE_HORIZON_MS),
          ENVOY_SESSION_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
          ENVOY_ROLE_CLAIM_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
          ENVOY_SESSION_MAX_AGE: goDuration(SESSION_MAX_AGE_MS),
        },
      });
      // One model for both hosts: a turn that asks for envoy_subscribe gets the call, once, and every
      // other turn a word. Oh My Pi reaches it under /anthropic (writeStandinProfile), Claude Code at
      // the root (ANTHROPIC_BASE_URL); any other read is answered empty.
      let served = 0;
      modelBase = serveStandin(cleanup, (url, body) => {
        if (!url.pathname.endsWith("/v1/messages")) return Response.json({ data: [] });
        served += 1;
        const subscribe = offeredTool(body, "envoy_subscribe");
        const asked = lastUserText(body);
        const blocks: readonly Block[] =
          subscribe !== undefined && asked.includes("envoy_subscribe") && !answersACall(body)
            ? [{ type: "tool_use", name: subscribe, input: { topics: [topic] } }]
            : [{ type: "text", text: "Done." }];
        exchanges.push(
          `${served}: offered ${subscribe ?? "no envoy_subscribe"} among ${
            z.array(z.unknown()).catch([]).parse(body.tools).length
          } tools; asked ${JSON.stringify(asked.slice(-120))}; answered ${blocks
            .map((block) => block.type)
            .join(", ")}`
        );
        if (body.stream !== true) {
          return Response.json({
            id: `msg_${served}`,
            type: "message",
            role: "assistant",
            model: body.model,
            content: blocks,
            stop_reason: blocks.some((block) => block.type === "tool_use")
              ? "tool_use"
              : "end_turn",
            stop_sequence: null,
            usage: { input_tokens: 5, output_tokens: 1 },
          });
        }
        return new Response(messageStream(blocks, `msg_${served}`), {
          headers: { "content-type": "text/event-stream" },
        });
      }).base;
    }, 300_000);

    afterAll(async () => {
      for (const step of cleanup.splice(0).reverse()) await step();
      await stack?.stop();
    }, 120_000);

    test("Oh My Pi's new_session leaves the id it replaces gone at once, and the new one live", async () => {
      const stack = rig();
      const binary = LEGION_TEST_OMP ?? "";
      const { home, workspace, sessions } = await ompRoot(binary, "session-stall-omp-", cleanup);
      await writeStandinProfile(home, modelBase);
      const answers = new Map<string, Record<string, unknown>>();
      const rpc = spawnRpc(
        binary,
        {
          extensions: [PI_ENVOY_EXTENSION],
          home,
          workspace,
          sessions,
          env: {
            ENVOY_URL: stack.envoyUrl,
            ENVOY_NATS_URL: stack.natsUrl,
            ENVOY_HEARTBEAT_MS: String(HEARTBEAT_MS),
            ENVOY_MACHINE_ID: "session-stall-hosts",
          },
          onFrame: (frame) => {
            const answer = z
              .object({ type: z.literal("response"), id: z.string() })
              .passthrough()
              .safeParse(frame);
            if (answer.success) answers.set(answer.data.id, answer.data);
          },
        },
        cleanup
      );
      let asked = 0;
      const ask = async (type: string): Promise<Record<string, unknown>> => {
        asked += 1;
        const id = `session-stall-${asked}`;
        rpc.send({ type, id });
        await until(`Oh My Pi to answer ${type}`, Date.now() + HOST_TURN_MS, () => answers.has(id));
        return answers.get(id) ?? {};
      };
      const sessionOf = async (): Promise<string> =>
        z.object({ data: z.object({ sessionId: z.string() }) }).parse(await ask("get_state")).data
          .sessionId;

      const before = await sessionOf();
      await awaitState(stack.envoyUrl, before, "live", Date.now() + HOST_TURN_MS);
      await ask("new_session");
      await awaitState(stack.envoyUrl, before, "gone", Date.now() + LEFT_GONE_MS);
      const after = await sessionOf();
      expect(after).not.toBe(before);
      await awaitState(stack.envoyUrl, after, "live", Date.now() + HOST_TURN_MS);
      await rpc.end();
    }, 300_000);

    test("Claude Code's /clear leaves the id it replaces gone at once, registers the id it mints under its own direct subject alone, and claude --resume gets back the session's topics", async () => {
      const stack = rig();
      const binary = LEGION_TEST_CLAUDE ?? "";
      const [home, config, workspace] = ["home", "claude-config", "workspace"].map((name) =>
        path.join(hostsDirectory, name)
      );
      for (const directory of [home, config, workspace])
        await mkdir(directory, { recursive: true });
      // The plugin the session loads, built from this checkout outside it, since a pull request's
      // tree carries no built dist/ or legion/.
      const plugin = path.join(hostsDirectory, "claude-envoy");
      const build = Bun.spawn(["bun", CLAUDE_ENVOY_BUILD, "--out", plugin], {
        stdout: "pipe",
        stderr: "pipe",
      });
      if ((await build.exited) !== 0) {
        throw new Error(`claude-envoy did not build: ${await new Response(build.stderr).text()}`);
      }
      // The channel server is told the plugin data directory Claude Code hands this plugin's hooks
      // (`<config>/plugins/data/<plugin>-inline` for a plugin loaded by --plugin-dir): the hook writes
      // there the id `/clear` mints, and the server adopts it from there.
      const mcpConfig = path.join(hostsDirectory, "claude-mcp.json");
      await writeFile(
        mcpConfig,
        JSON.stringify({
          mcpServers: {
            legion: {
              command: process.execPath,
              args: [path.join(plugin, "dist", "envoy-channel.js")],
              env: {
                CLAUDE_PLUGIN_DATA: path.join(config, "plugins", "data", "claude-envoy-inline"),
              },
            },
          },
        })
      );
      const session = crypto.randomUUID();

      /** One Claude Code process in stream-json both ways, as a Legion worker's bridge runs it. */
      const start = (flag: "--session-id" | "--resume") => {
        const child = Bun.spawn(
          [
            binary,
            "-p",
            "--input-format",
            "stream-json",
            "--output-format",
            "stream-json",
            "--verbose",
            "--permission-mode",
            "bypassPermissions",
            "--strict-mcp-config",
            "--mcp-config",
            mcpConfig,
            "--plugin-dir",
            plugin,
            "--plugin-dir",
            path.join(plugin, "legion"),
            flag,
            session,
          ],
          {
            cwd: workspace,
            env: {
              PATH: SHELL_PATH,
              HOME: home,
              CLAUDE_CONFIG_DIR: config,
              CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
              // Every MCP tool offered with the turn rather than behind a tool search, so the
              // stand-in can call envoy_subscribe by the name the turn offers it under.
              ENABLE_TOOL_SEARCH: "false",
              ANTHROPIC_BASE_URL: modelBase,
              ANTHROPIC_API_KEY: fixtureSecret("standin"),
              ENVOY_URL: stack.envoyUrl,
              ENVOY_NATS_URL: stack.natsUrl,
              ENVOY_HEARTBEAT_MS: String(HEARTBEAT_MS),
              ENVOY_MACHINE_ID: "session-stall-hosts",
            },
            stdin: "pipe",
            stdout: "pipe",
            stderr: "pipe",
            // A session of its own, so Claude Code leads a process group that holds what it
            // starts (its stdio MCP servers) and stop can end that group whole. This row stops no
            // member, so the session's orphaned group sends it no SIGHUP
            // (scripts/e2e/lib/own-group.ts says when that matters).
            detached: true,
          }
        );
        let results = 0;
        let output = "";
        void (async () => {
          const decoder = new TextDecoder();
          // A line can reach this read split across two chunks, so a line is counted once its
          // newline has come: what follows a chunk's last newline waits for the chunks after it,
          // and is a whole line of its own when the output ends.
          let partial = "";
          const result = '"type":"result"';
          for await (const chunk of child.stdout) {
            const text = decoder.decode(chunk, { stream: true });
            output += text;
            const lines = (partial + text).split("\n");
            partial = lines.pop() ?? "";
            results += lines.filter((line) => line.includes(result)).length;
          }
          if (partial.includes(result)) results += 1;
        })();
        const stderr = new Response(child.stderr).text();
        // groupLives is whether any process of Claude Code's group still runs: kill(2) with signal 0
        // to the group's id answers ESRCH once its last member has gone.
        const groupLives = () => {
          try {
            process.kill(-child.pid, 0);
            return true;
          } catch (error) {
            if ((error as NodeJS.ErrnoException).code === "ESRCH") return false;
            throw error;
          }
        };
        const endGroup = () => {
          if (groupLives()) process.kill(-child.pid, "SIGKILL");
        };
        // stop ends the input, which ends Claude Code's run and closes its MCP servers' stdin, and
        // waits up to 10 s for the whole group to go before it kills what is left of it. The
        // SIGKILL of Claude Code alone would leave its MCP servers running with no end of input,
        // beside the `--resume` of the same session that follows (cybermonklabs/legion#4357's
        // case), so the fallback ends the group, and nothing of this process runs once stop returns.
        const stop = async () => {
          child.stdin.end();
          const timer = setTimeout(endGroup, 10_000);
          await child.exited;
          const deadline = Date.now() + 10_000;
          while (groupLives() && Date.now() < deadline) await Bun.sleep(POLL_MS);
          clearTimeout(timer);
          endGroup();
        };
        cleanup.push(stop);
        return {
          /** Sends one user turn and waits for that turn's result line. */
          turn: async (text: string) => {
            const before = results;
            child.stdin.write(
              `${JSON.stringify({ type: "user", message: { role: "user", content: [{ type: "text", text }] } })}\n`
            );
            child.stdin.flush();
            const deadline = Date.now() + HOST_TURN_MS;
            while (results <= before) {
              if (child.exitCode !== null || Date.now() > deadline) {
                throw new Error(
                  `Claude Code ended no turn for ${JSON.stringify(text)}:\n${output}\n${await stderr}`
                );
              }
              await Bun.sleep(POLL_MS);
            }
          },
          stop,
        };
      };

      const first = start("--session-id");
      await first.turn(`Call the envoy_subscribe tool for the topic ${topic}.`);
      await awaitState(stack.envoyUrl, session, "live", Date.now() + HOST_TURN_MS);
      await until("the session to hold its subscribed topic", Date.now() + HOST_TURN_MS, () =>
        holdsTopic(stack.envoyUrl, session, topic)
      ).catch((error: unknown) => {
        throw new Error(`${String(error)}\n${exchanges.join("\n")}`);
      });
      await first.turn("/clear");
      await awaitState(stack.envoyUrl, session, "gone", Date.now() + LEFT_GONE_MS);
      // The id /clear minted registers from the same project directory, within the 10 s the
      // channel server's poll of its handoff file allows, and its row lists its own direct subject
      // and never the one it replaced - at once, and again two heartbeats later, since a later
      // registration must not bring the old subject back.
      let minted = "";
      await until("the id /clear minted to register", Date.now() + 10_000, async () => {
        const rows = await sessionsOf(stack.envoyUrl, workspace);
        minted = rows.find((row) => row.session_id !== session)?.session_id ?? "";
        return minted !== "";
      });
      for (const wait of [0, 2 * HEARTBEAT_MS + 1_000]) {
        await Bun.sleep(wait);
        const rows = await sessionsOf(stack.envoyUrl, workspace);
        expect(rows.map((row) => row.session_id)).toEqual([minted]);
        expect(rows[0]?.topics ?? []).toContain(`notifications.agent.${minted}`);
        expect(rows[0]?.topics ?? []).not.toContain(`notifications.agent.${session}`);
      }
      await first.stop();

      const resumed = start("--resume");
      await resumed.turn("Reply with one word.");
      await until("the resumed session to get its topic back", Date.now() + HOST_TURN_MS, () =>
        holdsTopic(stack.envoyUrl, session, topic)
      );
      await resumed.stop();
    }, 420_000);
  }
);
