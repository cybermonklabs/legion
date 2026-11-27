// The harness every test file under `extensions/` that calls `useExtensionHarness()` shares: the
// host modules the suites mock, the reset every row starts and ends with, and the helpers the
// suites call. The NATS stand-in the extension dials, the fetch that records every registration
// and the listener's answers are `./listener-stand-in`'s, which this module imports, and so
// evaluates, before it mocks anything. Bun evaluates an imported module once per process, so the
// files drive this one set of state, and `mock.module` replaces a module for the whole process.
// For the same reason this module registers no hook in its body: a hook here would take the scope
// of whichever test file imported it first. Each file calls `useExtensionHarness()` at its own
// module scope instead.
import { afterAll, afterEach, beforeAll, beforeEach, expect, jest, mock } from "bun:test";
import { createHash } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { forgetBrokerClients } from "@legion/envoy-client/broker-credential";
import type {
  DeliveryLedgerHost,
  DeliveryLedgerOptions,
} from "@legion/envoy-client/delivery-ledger";
import type { ExecFn } from "@legion/envoy-client/dispatch-cwd";
import type { TmuxPlaceReading } from "@legion/envoy-client/tmux-place";
import { resetEnvoyPluginInterfaceForTests } from "@legion/pi-shared/interface";
import { hostAgentRegistryMock, testAgentRoster } from "@legion/pi-shared/test/host-registry";
import { captureEnvironment, restoreEnvironment } from "@legion/test-fixtures/environment";
import type { UserInputBuiltinCommand } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler";
import { logger, procmgr } from "@oh-my-pi/pi-utils";
import type { connect } from "nats";
import type { z } from "zod";
import type * as EnvoyEntry from "../extensions/envoy";
import { resetEnvoySessionsForTests } from "../src/envoy-session";
import {
  natsStandInConnect,
  originalFetch,
  resetNatsStandIn,
  responseWithRegistration,
  startRow,
} from "./listener-stand-in";
import { createStandInHost, type RegisteredTool, type SessionContext } from "./stand-in-host";

const clipboardState = {
  copiedSessionIDs: [] as string[],
  error: undefined as Error | undefined,
};

mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async (text: string) => {
    if (clipboardState.error !== undefined) throw clipboardState.error;
    clipboardState.copiedSessionIDs.push(text);
  },
  ...hostAgentRegistryMock,
}));
// The host's built-in slash commands, which our fork lists beside `sendUserInput`
// (`listUserInputBuiltinCommands`): a test fills this list, and `afterEach` empties it. No package
// in this workspace provides the module, so without this stand-in the extension lists no
// built-ins, as on a host without the method.
export const hostBuiltins: UserInputBuiltinCommand[] = [];
mock.module("@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler", () => ({
  listUserInputBuiltinCommands: () => hostBuiltins,
}));
// What the extension reads when it asks where this session sits in tmux, and what it asked with.
// `@legion/envoy-client/tmux-place` is mocked for the whole process the way `nats` is: the real
// reader shells out to tmux, so a suite run inside a pane would report the pane that started it.
// The default is the reading a session outside tmux gets; `afterEach` puts it back.
const refusedPlace = (): TmuxPlaceReading => ({ kind: "refused" });
export const tmuxPlaceState = {
  /** Every reading the extension asked for, with the environment it passed. */
  calls: [] as { readonly env: Record<string, string | undefined> }[],
  /** What the next reading comes to. A row that throws here drives a reader that fails. */
  reading: refusedPlace as (
    env: Record<string, string | undefined>,
    exec: ExecFn
  ) => TmuxPlaceReading | Promise<TmuxPlaceReading>,
};
// Copied out before the mock replaces the module: the row that drives the real reader over an
// exec that records every call (`@legion/envoy-client/tmux-place` runs none outside tmux).
export const { readTmuxPlace: realReadTmuxPlace } = await import("@legion/envoy-client/tmux-place");
mock.module("@legion/envoy-client/tmux-place", () => ({
  readTmuxPlace: async (env: Record<string, string | undefined>, exec: ExecFn) => {
    tmuxPlaceState.calls.push({ env });
    return tmuxPlaceState.reading(env, exec);
  },
}));
// The delivery ledger, which calls the host with the session the frame a failure notice names
// was relayed to (`DeliveryLedgerHost.showFailureNotice`). Everything passes straight through;
// `beforeFailureNotice` is the one seam: the ledger binds the give-up to that session before it
// calls, so the session the notice names and the conversation the host has open can part company
// only inside the call, and a row that drives that moves the host from this hook. The running
// process has no such wait: pi-envoy hands the card in the same run as the ledger's call, and
// what holds a notice to its session there is the hand itself, `UnwrittenFrames.sendCard` asking
// `hostIsOn` of the session the notice names. The hook makes the host already moved when that
// hand runs, which is the case `sendCard` decides. The module is read here,
// and not imported at the top, because the mock below replaces it: this is the real one it
// passes through to, copied out as the tmux reader above is.
const realDeliveryLedger = await import("@legion/envoy-client/delivery-ledger");
// The function itself, taken now: the namespace above is live, so once the mock below replaces
// the module, `realDeliveryLedger.createDeliveryLedger` names the mock, and a mock that called
// through it would call itself without end (measured: one row of `envoy.test.ts` grew past 2.5 GB
// in 9 s, against 422 MB without the seam).
const realCreateDeliveryLedger = realDeliveryLedger.createDeliveryLedger;
export const ledgerSeam: { beforeFailureNotice?: (addressed: string) => Promise<void> } = {};
mock.module("@legion/envoy-client/delivery-ledger", () => ({
  ...realDeliveryLedger,
  createDeliveryLedger: (options: DeliveryLedgerOptions) => {
    const host: DeliveryLedgerHost = {
      ...options.host,
      showFailureNotice: async (addressed, notice) => {
        await ledgerSeam.beforeFailureNotice?.(addressed);
        const outcome = await options.host.showFailureNotice(addressed, notice);
        return outcome === "queued" ? "queued" : "shown";
      },
    };
    return realCreateDeliveryLedger({ ...options, host });
  },
}));

// The directory this process was started in. A row that chdirs elsewhere is put back in
// `afterEach`, which a row aborted at Bun's own timeout reaches where its own `finally` does not.
const originalStartCwd = process.cwd();

const originalNatsUrl = process.env.ENVOY_NATS_URL;
const originalEnvoyUrl = process.env.ENVOY_URL;
const originalHome = process.env.HOME;
const originalPath = process.env.PATH;
const originalDispatchUrl = process.env.DISPATCH_URL;
const originalDispatchToken = process.env.DISPATCH_TOKEN;
// The Legion daemon exports DISPATCH_TOKEN_FILE into every pane it spawns; a suite run from one
// must not see it, or the extension resolves a real Dispatch config and registers the tools.
const originalDispatchTokenFile = process.env.DISPATCH_TOKEN_FILE;
// The logical machine name a Legion pod exports, which `machineID` reports over the kernel
// hostname: a suite run from one must not see it, or every row that reads a machine id reads the
// pod's instead of this process's own.
const originalMachineID = process.env.ENVOY_MACHINE_ID;
// Both of the variables a place reading consults: a suite run inside a pane would otherwise have
// every registration carry the pane that started it (`@legion/envoy-client/tmux-place`).
const originalTmux = process.env.TMUX;
const originalTmuxPane = process.env.TMUX_PANE;
// The dispatch command's contract with the extension: the session id and host it exports, and the
// state directory both read, which each test points at a scratch directory of its own.
const originalDispatchSessionId = process.env.DISPATCH_SESSION_ID;
const originalDispatchHost = process.env.DISPATCH_HOST;
const originalDispatchStateDir = process.env.DISPATCH_STATE_DIR;
// What picks the envoy.json the extension reads, and the credentials a Legion pane carries beside
// it: each test starts with none of them but a file that holds no section (`beforeEach`).
const configVariables = [
  "ENVOY_CONFIG_FILE",
  "OMP_PROFILE",
  "PI_CODING_AGENT_DIR",
  "XDG_CONFIG_HOME",
  "ENVOY_TOKEN",
  "ENVOY_TOKEN_FILE",
  "NATS_NKEY_SEED",
  "NATS_NKEY_SEED_FILE",
  // A box's broker route and the NATS credential choice: a row that presents a broker token sets
  // them, and every other row runs with none, whatever the box running the suite holds.
  "NATS_AUTH",
  "AGENT_SECRETS_URL",
  "AGENT_SECRETS_MACHINE_IDENTITY",
  "AGENT_SECRETS_MACHINE_TOKEN_FILE",
] as const;
const originalConfigVariables = configVariables.map((name) => [name, process.env[name]] as const);
/** The envoy.json every test starts with: no Dispatch or Envoy section, and AskGate off. */
export const ASKGATE_OFF = { askgate: { enabled: false } };
export let configDirectory = "";

// Hands the NATS stand-in (`natsStandInConnect`) to every instance of the Envoy entry either
// suite loads: the setter keeps it on `globalThis`, since each `import("./envoy.ts?<row>")` is a
// module of its own. The import cannot be static, as the entry reads `@oh-my-pi/pi-coding-agent`
// and so must load below the stand-ins for it; it carries a query of its own so no row's instance
// is this one; and it sits below the `original*` captures because loading the entry sets `PATH`,
// `DISPATCH_HOST` and `ENVOY_CONFIG_FILE`, as every row's own load does, which `afterEach` puts
// back to what was captured. Loading it here changes those three names whether or not a row of
// either suite runs, so the capture below puts them back as soon as the entry is loaded; every
// row's own load changes them again.
const restoreEntryLoad = captureEnvironment(["PATH", "DISPATCH_HOST", "ENVOY_CONFIG_FILE"]);
// TypeScript resolves no specifier that carries a query, so the import names it through a value
// and the instance is typed as the entry it loads.
const natsStandInEntry = "../extensions/envoy.ts?nats-stand-in";
const { resetNatsConnectForTests, resetSessionRoleLoggedForTests, setNatsConnectForTests } =
  (await import(natsStandInEntry)) as typeof EnvoyEntry;
restoreEntryLoad();

// A row that dials with a plan of redials of its own, or with a dial of its own, sets it here,
// and sets the stand-in back with the default plan when it ends.
export { setNatsConnectForTests };

function setUpRow(): void {
  startRow();
  // `bun test` runs every file in one process: a Legion suite's bootstrapped-session record on
  // the process-wide interface would otherwise make every transcript here look like a
  // subagent's, the top-level session one test publishes would be the reply address the next
  // test's subagent instance reports, and a stale claim instance would capture a later claim.
  resetEnvoyPluginInterfaceForTests();
  resetEnvoySessionsForTests();
  testAgentRoster().splice(0);
  process.env.ENVOY_NATS_URL = "nats://nats-under-test:4222";
  // A test that never stubs fetch must not register its `ses_*` fixture on the real listener
  // (the client defaults to http://127.0.0.1:9020, which on a devbox is production): point the
  // URL at an unroutable host and answer every request with the registration echo by default.
  process.env.ENVOY_URL = "http://envoy.test";
  globalThis.fetch = (async (input, init) =>
    responseWithRegistration(input, init, [])) as typeof fetch;
  delete process.env.DISPATCH_URL;
  delete process.env.DISPATCH_TOKEN;
  delete process.env.DISPATCH_TOKEN_FILE;
  delete process.env.ENVOY_MACHINE_ID;
  // The role a session pod's launch names and the session it names it for, which every pass of
  // an instance created under them claims for that session: a suite run inside a session pod must
  // not claim that pod's role for a fixture. The line a process logs once when a session it
  // decides for is another is unsaid again, so a row that reads it reads its own.
  delete process.env.ENVOY_SESSION_ROLE;
  delete process.env.ENVOY_SESSION_ROLE_SESSION;
  resetSessionRoleLoggedForTests();
  process.env.HOME = "/nonexistent-home-for-envoy-tests";
  delete process.env.TMUX;
  delete process.env.TMUX_PANE;
  // The place cache refreshes itself on a timer at its lifetime, and a case here boots sessions
  // it never shuts down: at the 30 s default each of them would read tmux again half a minute
  // later, inside whatever case is running by then. The ceiling keeps that from happening for a
  // case that names no lifetime. The tmux-place cases of `envoy.test.ts` name a 30 s one
  // (`withEnvoyTimings`), so each of them still leaves a 30 s timer running; no later case counts
  // the readings such a timer makes.
  process.env.ENVOY_TMUX_PLACE_CACHE_MS = "2147483647";
  process.env.DISPATCH_STATE_DIR = mkdtempSync(join(tmpdir(), "envoy-dispatch-state-"));
  delete process.env.DISPATCH_SESSION_ID;
  // The extension picks its envoy.json when its module loads, and each test loads it fresh: a file
  // that holds no Dispatch or Envoy section, so Dispatch and Envoy come from the variables the test
  // sets and never from the caller's own envoy.json or Oh My Pi profile. It turns AskGate off: the
  // gate loads Oh My Pi modules this workspace does not install, and its handlers would take the
  // fixture's one slot per event (`createPi`). The tests in "AskGate" turn it back on.
  for (const name of configVariables) delete process.env[name];
  configDirectory = mkdtempSync(join(tmpdir(), "envoy-config-"));
  process.env.ENVOY_CONFIG_FILE = join(configDirectory, "envoy.json");
  writeFileSync(process.env.ENVOY_CONFIG_FILE, `${JSON.stringify(ASKGATE_OFF)}\n`);
}

async function tearDownRow(): Promise<void> {
  // A managed-timer callback that rejects fails the test that scheduled it.
  await Promise.all(contextTimers.splice(0));
  process.chdir(originalStartCwd);
  if (originalNatsUrl === undefined) delete process.env.ENVOY_NATS_URL;
  else process.env.ENVOY_NATS_URL = originalNatsUrl;
  if (originalEnvoyUrl === undefined) delete process.env.ENVOY_URL;
  else process.env.ENVOY_URL = originalEnvoyUrl;
  globalThis.fetch = originalFetch;
  // A broker client's retry loop trades on for the process's life, so each row's clients are
  // closed with it: one whose trades were failing would go on trading, and logging, into the
  // rows after it.
  forgetBrokerClients();
  if (originalDispatchUrl === undefined) delete process.env.DISPATCH_URL;
  else process.env.DISPATCH_URL = originalDispatchUrl;
  if (originalDispatchToken === undefined) delete process.env.DISPATCH_TOKEN;
  else process.env.DISPATCH_TOKEN = originalDispatchToken;
  if (originalDispatchTokenFile === undefined) delete process.env.DISPATCH_TOKEN_FILE;
  else process.env.DISPATCH_TOKEN_FILE = originalDispatchTokenFile;
  if (originalMachineID === undefined) delete process.env.ENVOY_MACHINE_ID;
  else process.env.ENVOY_MACHINE_ID = originalMachineID;
  if (originalHome === undefined) delete process.env.HOME;
  else process.env.HOME = originalHome;
  if (originalPath === undefined) delete process.env.PATH;
  else process.env.PATH = originalPath;
  if (originalTmux === undefined) delete process.env.TMUX;
  else process.env.TMUX = originalTmux;
  if (originalTmuxPane === undefined) delete process.env.TMUX_PANE;
  else process.env.TMUX_PANE = originalTmuxPane;
  if (process.env.DISPATCH_STATE_DIR !== undefined) {
    rmSync(process.env.DISPATCH_STATE_DIR, { recursive: true, force: true });
  }
  for (const [name, value] of [
    ["DISPATCH_SESSION_ID", originalDispatchSessionId],
    ["DISPATCH_HOST", originalDispatchHost],
    ["DISPATCH_STATE_DIR", originalDispatchStateDir],
  ] as const) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  for (const [name, value] of originalConfigVariables) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  rmSync(configDirectory, { recursive: true, force: true });
  procmgr.__resetShellConfigCacheForTests();
  clipboardState.copiedSessionIDs.length = 0;
  clipboardState.error = undefined;
  tmuxPlaceState.calls.length = 0;
  tmuxPlaceState.reading = refusedPlace;
  // Each is read when an extension instance is created, and a row aborted at Bun's own timeout
  // never runs its `finally`, so a row that sets one must not decide a later row's timings; the
  // place cache's lifetime is set again by the hook above.
  delete process.env.ENVOY_CONTEXT_FRAME_INTERVAL_MS;
  delete process.env.ENVOY_RESUBSCRIBE_DELAY_MS;
  delete process.env.ENVOY_SUBSCRIBE_FLUSH_TIMEOUT_MS;
  delete process.env.ENVOY_SHELL_COMMAND_LIMIT_MS;
  delete process.env.ENVOY_TMUX_PLACE_CACHE_MS;
  resetNatsStandIn();
  hostBuiltins.length = 0;
  ledgerSeam.beforeFailureNotice = undefined;
}

/**
 * Registers the hooks every row that drives the extension through this harness needs: the
 * environment a row may leave set, the NATS stand-in, and the reset each row starts and ends
 * with. A test file calls it once, at its own module scope, before anything sets those names.
 */
export function useExtensionHarness(): void {
  // Both are read when an extension instance is created, and ENVOY_MACHINE_ID is what
  // `machineID()` reports: a row aborted at Bun's own timeout never runs its `finally`, so a row
  // that sets one must not decide a later row's timings or identity — and the next file in this
  // bun process must read what its caller set, never what a row here left.
  restoreEnvironment([
    "ENVOY_RESUBSCRIBE_DELAY_MS",
    "ENVOY_SUBSCRIBE_FLUSH_TIMEOUT_MS",
    "ENVOY_MACHINE_ID",
    "AGENTBOX_HOST",
    "ENVOY_TMUX_PLACE_CACHE_MS",
    "ENVOY_SESSION_ROLE",
    "ENVOY_SESSION_ROLE_SESSION",
  ]);
  // The slot the setter writes is the process's, not the calling file's, so the set and the reset
  // are one pair of hooks: a `bun test -t <pattern>` that selects no row of that file runs
  // neither, where a set at module scope would run and leave the next file of the same run
  // dialling this stand-in. No pane starts before this `beforeAll`: every row builds its own
  // instance in its own body.
  // The stand-in carries only the part of a connection the extension uses.
  beforeAll(() => setNatsConnectForTests(natsStandInConnect as unknown as typeof connect));
  afterAll(() => resetNatsConnectForTests());
  beforeEach(setUpRow);
  afterEach(tearDownRow);
}

export function createPi(
  options: { readonly clipboardError?: Error; readonly zod?: typeof z } = {}
) {
  clipboardState.error = options.clipboardError;
  // The stand-in host `scripts/role-takeover/omp-client.ts` drives the extension with too, plus
  // the clipboard this module mocks.
  return {
    ...createStandInHost({ zod: options.zod }),
    copiedSessionIDs: clipboardState.copiedSessionIDs,
  };
}

/** Every callback the default `sessionContext()` timer ran, awaited after each test. */
export const contextTimers: Promise<void>[] = [];

/** A managed timer scheduled further out than this is left pending by the fake below, as the
 *  host's own clock leaves it in a test that lasts milliseconds: the heartbeat's extra call
 *  after a failed registration is 20–40 s out, and a test that wants it supplies a `setTimeout`
 *  of its own and runs the callback itself. */
export const TIMER_HORIZON_MS = 1_000;

// The session-manager surface `isSubagentSession` reads. No transcript path reads as a
// top-level session, so every fixture here registers unless a test supplies a transcript that
// sits inside a parent's directory.
export const topLevelSession = {
  getSessionFile: (): string | undefined => undefined,
  ensureOnDisk: async (): Promise<void> => undefined,
  getEntries: (): readonly unknown[] => [],
};

export function sessionContext(
  sessionID = "ses_omp",
  hasUI = true
): SessionContext & { readonly hasUI: boolean } {
  return {
    cwd: "/tmp/envoy-omp-test",
    hasUI,
    sessionManager: { ...topLevelSession, getSessionId: () => sessionID },
    setInterval: () => undefined,
    setTimeout: (callback, delayMs) => {
      if (delayMs > TIMER_HORIZON_MS) return;
      contextTimers.push(Promise.resolve().then(callback));
    },
    ui: {
      notify: () => undefined,
      onTerminalInput: () => () => undefined,
      getEditorText: () => "",
    },
  };
}

// The role arbiter forwards an envelope under its original key behind the forward mark, and a
// publish the daemon makes carries a key the listener minted, 32 hex digits: `label` stands for it.
export function forwardedRoleEnvelope(role: string, summary: string, label: string) {
  const minted = createHash("sha256").update(label).digest("hex").slice(0, 32);
  return JSON.stringify({
    event_id: `evt-${label}`,
    source: "envoy",
    source_event_id: `source-${label}`,
    topic: `notifications.role.${role}`,
    dedupe_key: `envoy.role.forward.publish.${minted}`,
    issued_at: 1,
    payload_summary: summary,
    trace_id: `trace-${label}`,
  });
}

/** A Dispatch delivery frame addressed to ses_delivery, on issue CORE-1. `bodyTruncated` says
 *  the event carries the body's first part alone, so the session reads the message whole before
 *  it renders it (`wholeCarriedBody`). */
export function targetedDispatchEnvelope(
  mode: "aside" | "btw" | "steer",
  dedupeKey: string,
  frame: { readonly bodyTruncated?: boolean } = {}
): string {
  return JSON.stringify({
    dedupe_key: dedupeKey,
    event_id: `dispatch-${dedupeKey}`,
    issued_at: 1,
    payload: JSON.stringify({
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: "CORE-1",
        payload: {
          author: { id: "alice", kind: "user" },
          body: `Delivery ${mode} ${dedupeKey}`,
          ...(frame.bodyTruncated === true ? { body_truncated: true } : {}),
          created_at: "2026-09-12T00:00:00Z",
          deliveries: [],
          id: "11111111-1111-4111-8111-111111111111",
          in_reply_to: null,
          issue_key: "CORE-1",
          target: "session:ses_delivery",
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode },
    }),
    payload_summary: `Delivery ${mode} ${dedupeKey}`,
    source: "dispatch",
    source_event_id: "1",
    topic: "notifications.agent.ses_delivery",
    trace_id: dedupeKey,
  });
}

/** The client instance id one extension instance mints, read by its shape alone: a test that
 *  needs the id itself reads it off a request the extension made. */
export const ANY_CLIENT_INSTANCE = expect.stringMatching(/^[0-9a-f]{32}$/);

// Lets every promise and I/O callback queued so far run: the mocked NATS and listener paths settle
// within a few turns, since nothing in them waits on a clock.
export async function settle(): Promise<void> {
  for (let turn = 0; turn < 25; turn += 1) {
    const { promise, resolve } = Promise.withResolvers<void>();
    setImmediate(resolve);
    await promise;
  }
}

// Polls `condition` between event-loop turns. Only the resubscribe path waits on a real timer
// (`ENVOY_RESUBSCRIBE_DELAY_MS`, 10 ms in these tests), which no fake clock reaches, and a busy
// machine runs it later than any fixed sleep allows; the deadline is measured on the monotonic
// clock, which `setSystemTime` leaves alone. A caller waiting on a real child process rather
// than on this process's own work passes a longer bound: starting a shell and killing its group
// is the operating system's work, and two seconds of it is a wait a loaded machine can lose.
// A row whose wait can still be unsettled when the row ends (bun fails a row at its timeout and
// goes on to the next while the row's own code waits on a turn of `settle`) passes `ended`, which
// its teardown aborts: once it is aborted the wait reads the condition no more and returns, so it
// neither polls on after the row nor throws into a later one.
export async function until(
  condition: () => boolean,
  what: string,
  timeoutMs = 2_000,
  ended?: AbortSignal
): Promise<void> {
  const deadline = performance.now() + timeoutMs;
  for (;;) {
    if (ended?.aborted === true) return;
    if (condition()) return;
    if (performance.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await settle();
  }
}

/** nats.js's own default server, the one it dials when a connect is handed none. */
export const NATS_DEFAULT_PORT = 4222;

/**
 * A listener on `127.0.0.1:<port>` that counts the connections made to it and closes each, or
 * null when something already listens there. `Bun.listen` binds synchronously and throws
 * `EADDRINUSE`, so a `describe` body can read the answer and hand it to `test.skipIf`, and the
 * listener it returns holds the port from that moment: no second bind, so no race with whatever
 * else on the machine might take the port between the question and the row that asks it.
 */
export function listenCountingDials(
  port: number
): { readonly dialed: () => number; readonly stop: () => void } | null {
  let dialed = 0;
  try {
    const listener = Bun.listen({
      hostname: "127.0.0.1",
      port,
      socket: {
        open(socket) {
          dialed += 1;
          socket.end();
        },
        data() {},
      },
    });
    return { dialed: () => dialed, stop: () => listener.stop(true) };
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "EADDRINUSE") return null;
    throw error;
  }
}

/** One turn of the real event loop, from before any test fakes the timers: a file read the
 *  settings make lands in such a turn, which no microtask flush reaches. */
const realSetImmediate = globalThis.setImmediate;
const realTurn = () => new Promise<void>((resolve) => realSetImmediate(() => resolve()));

/**
 * Settles `pending` on the test's clock (`jest.useFakeTimers`, on for the call), moving it a
 * second at a time: a write Dispatch keeps failing is asked again under its key until the
 * accept's 10 s bound, and those pauses and that bound pass at once rather than in real time.
 * The clock stands still until `waiting` says the work is waiting on it: before its first
 * request, the accept reads the session's envoy.json, which is real I/O, while the accept's bound
 * (an `AbortSignal.timeout`, on the test's clock) already runs, and a second moved then would be
 * one the code under test never waited.
 */
export async function onTestClock<T>(
  pending: () => Promise<T>,
  waiting: () => boolean
): Promise<T> {
  jest.useFakeTimers();
  try {
    let settled = false;
    const watched = pending().finally(() => {
      settled = true;
    });
    while (!settled && !waiting()) await realTurn();
    for (let second = 0; second < 60 && !settled; second++) {
      for (let flush = 0; flush < 50 && !settled; flush++) await Promise.resolve();
      jest.advanceTimersByTime(1_000);
    }
    return await watched;
  } finally {
    jest.useRealTimers();
  }
}

/**
 * Runs `body` with the Envoy timings it names, and restores whatever the process had. The
 * extension factory reads them when it runs (`envoySettings`), so a test sets them before its
 * own `envoyExtension(...)` call; every test of the files that share this harness runs in one
 * process, so none may leak.
 */
export async function withEnvoyTimings<T>(
  timings: {
    readonly heartbeatMs?: number;
    readonly passDeadlineMs?: number;
    readonly tmuxPlaceCacheMs?: number;
  },
  body: () => Promise<T>
): Promise<T> {
  const before = [
    ["ENVOY_HEARTBEAT_MS", process.env.ENVOY_HEARTBEAT_MS],
    ["ENVOY_PASS_DEADLINE_MS", process.env.ENVOY_PASS_DEADLINE_MS],
    ["ENVOY_TMUX_PLACE_CACHE_MS", process.env.ENVOY_TMUX_PLACE_CACHE_MS],
  ] as const;
  if (timings.heartbeatMs !== undefined) {
    process.env.ENVOY_HEARTBEAT_MS = String(timings.heartbeatMs);
  }
  if (timings.passDeadlineMs !== undefined) {
    process.env.ENVOY_PASS_DEADLINE_MS = String(timings.passDeadlineMs);
  }
  if (timings.tmuxPlaceCacheMs !== undefined) {
    process.env.ENVOY_TMUX_PLACE_CACHE_MS = String(timings.tmuxPlaceCacheMs);
  }
  try {
    return await body();
  } finally {
    for (const [name, value] of before) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  }
}

/**
 * Every `envoy: ENVOY_SESSION_ROLE …` line the extension logs from now on, which is the line a
 * process logs once when a session it decides for is not the one the launch named; `stop` ends
 * the collecting.
 */
export function sessionRoleLines(): { readonly lines: string[]; readonly stop: () => void } {
  const lines: string[] = [];
  const stop = logger.registerLogSink((entry) => {
    if (entry.level === "warn" && entry.message.startsWith("envoy: ENVOY_SESSION_ROLE ")) {
      lines.push(entry.message);
    }
  });
  return { lines, stop };
}

/**
 * Every subject the session follows now, as `envoy_whoami` reports them (`followed` in
 * `extensions/envoy.ts`). The session holds no NATS subscription on a topic it follows — its
 * consumer relays them all — and a role-bound notice subject is never registered with the
 * listener either, so what the session follows is the one place such a subject shows.
 */
export async function followedTopics(
  tools: readonly RegisteredTool[],
  context: SessionContext
): Promise<readonly string[]> {
  const whoami = tools.find((tool) => tool.name === "envoy_whoami");
  if (whoami === undefined) throw new Error("envoy_whoami was not registered");
  const { details } = await whoami.execute("follows", {}, undefined, undefined, context);
  const topics = details.topics;
  if (!Array.isArray(topics)) throw new Error("envoy_whoami reported no topics");
  return topics.filter((topic): topic is string => typeof topic === "string");
}
