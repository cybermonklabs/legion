// The harness every test file under `extensions/` that calls `useExtensionHarness()` shares: the
// host modules the suites mock, the reset every row starts and ends with, and the helpers the
// suites call. The NATS stand-in the extension dials, the fetch that records every registration
// and the listener's answers are `./listener-stand-in`'s, which this module imports, and so
// evaluates, before it mocks anything. Bun evaluates an imported module once per process, so the
// files drive this one set of state, and `mock.module` replaces a module for the whole process.
// For the same reason this module registers no hook in its body: a hook here would take the scope
// of whichever test file imported it first. Each file calls `useExtensionHarness()` at its own
// module scope instead.
import { afterAll, afterEach, beforeEach, expect, jest, mock, setSystemTime } from "bun:test";
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
import { realTimeDeadline } from "@legion/test-fixtures/deadline";
import { captureEnvironment, restoreEnvironment } from "@legion/test-fixtures/environment";
import { settled, until } from "@legion/test-fixtures/wait";
import type { UserInputBuiltinCommand } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler";
import { logger, procmgr } from "@oh-my-pi/pi-utils";
import type { connect } from "nats";
import type { z } from "zod";
import type * as EnvoyEntry from "../extensions/envoy";
import { resetEnvoySessionsForTests } from "../src/envoy-session";
import {
  advanceRowGeneration,
  fromEndedRow,
  natsStandInConnect,
  originalFetch,
  resetNatsStandIn,
  responseWithRegistration,
  rowCheck,
  stillOn,
} from "./listener-stand-in";
import {
  createStandInHost,
  type EventHandler,
  type RegisteredTool,
  type SessionContext,
} from "./stand-in-host";

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

// The originals of what a row may stand in for a while, which `resetFileState` puts back.
export const originalAbortTimeout = AbortSignal.timeout;
export const originalGlobalSetTimeout = globalThis.setTimeout;
export const originalConsoleWarn = console.warn;

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
// The machine an agent box runs on, which `machineID` reports where ENVOY_MACHINE_ID is unset.
export const originalAgentboxHost = process.env.AGENTBOX_HOST;
// Both of the variables a place reading consults: a suite run inside a pane would otherwise have
// every registration carry the pane that started it (`@legion/envoy-client/tmux-place`).
const originalTmux = process.env.TMUX;
const originalTmuxPane = process.env.TMUX_PANE;
// The dispatch command's contract with the extension: the session id and host it exports, and the
// state directory both read, which each test points at a scratch directory of its own.
const originalDispatchSessionId = process.env.DISPATCH_SESSION_ID;
const originalDispatchHost = process.env.DISPATCH_HOST;
const originalDispatchStateDir = process.env.DISPATCH_STATE_DIR;
// The state root a row points elsewhere, to see where the session's record goes: put back as HOME
// is.
export const originalXdgStateHome = process.env.XDG_STATE_HOME;
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
const {
  resetNatsConnectForTests,
  resetSessionRoleLoggedForTests,
  setNatsConnectForTests: setNatsConnect,
} = (await import(natsStandInEntry)) as typeof EnvoyEntry;
restoreEntryLoad();

/**
 * Gives the NATS dial `dial` and its plan (`setNatsConnect`), as every row and hook of the files
 * that share this harness does through this one: a row that dials with a plan of redials of its
 * own, or with a dial of its own, sets it here, and `resetFileState` gives the stand-in its own
 * plan again before and after every row. A row body its guard abandoned in an ordinary await runs
 * on inside the row after it, whose dial `setUpRow` has set by then: its write is dropped
 * (`fromEndedRow`), so that row keeps its own.
 */
export function setNatsConnectForTests(...plan: Parameters<typeof setNatsConnect>): void {
  if (fromEndedRow()) return;
  setNatsConnect(...plan);
}

// Nothing here waits. The body removes and makes the row's scratch directories.
function setUpRow(): void {
  advanceRowGeneration();
  jest.useRealTimers();
  contextTimers.length = 0;
  resetFileState();
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
  // case that names no lifetime. Four of the tmux-place cases of `heartbeat-registration.test.ts`
  // name a 30 s one, on the test's clock, where it passes only as the case moves that clock
  // (`atPlaceLifetime`), and the other four run at this ceiling. None of them leaves its timer
  // running: each shuts its session down at its end, and `tearDownRow` shuts down one a case left
  // running, a case its guard abandoned included (`rowStops`).
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

/**
 * What stops each thing a row started that would run on past it: a log sink the row registered
 * (`rowLogSink`), so a row its guard abandoned collects nothing from the rows after it, and
 * something the row started that must end with it (a session's shutdown, a stall proxy's close:
 * `rowShutdown`). A session left running would act inside whatever row runs then. A placed
 * session's place cache reads tmux on a real `setInterval` at its lifetime, which would add a
 * reading to `tmuxPlaceState.calls`. A session whose background confirmation asks every few
 * milliseconds would add to the shared `natsState.flushes`. `tearDownRow` runs every stop left
 * here, a row its guard abandoned included, while that row's fetch still answers a session's
 * deregistration, and `resetFileState` runs the ones an `afterEach` bun skipped left
 * (`runRowStops`).
 */
export const rowStops = new Set<() => unknown>();

/**
 * Runs every stop left in `rowStops`, and answers once all have settled. A session's shutdown
 * stops its place cache before its first await, so the timer is stopped when this returns,
 * whether or not the caller waits for the rest (the deregistration and the drain).
 */
export function runRowStops(): Promise<unknown> {
  const left = [...rowStops];
  rowStops.clear();
  return Promise.all(left.map((stop) => stop()));
}

/**
 * Registers `sink` for the row running now, and answers what stops it. The harness's hooks stop
 * every sink a row registered (`rowStops`). A stop removes only its own sink, so a second run of
 * it does nothing. A body its guard abandoned that gets here only after its row ended throws, so
 * it registers no sink inside the row after it.
 */
export function rowLogSink(sink: logger.LogSink): () => void {
  stillOn(rowCheck(), "registering a log sink");
  const stop = logger.registerLogSink(sink);
  rowStops.add(stop);
  return stop;
}

/**
 * Registers `shutdown` in `rowStops`, the end of something the row running now started that must
 * end with it (a session's shutdown, a stall proxy's close), and answers what runs it. It runs
 * once: the first call starts it, and every later call answers that same run. The row's own
 * `finally` calls it. `tearDownRow` calls it too, a row its guard abandoned included, while that
 * row's fetch still answers a session's deregistration, and waits for it before it puts shared
 * state back. So a `finally` the abandoned body reaches late starts no shutdown inside the row
 * after its own, and what it ends is not left running, as a `finally` guarded by `ownRow()` alone
 * would leave it. A body its guard abandoned that gets here only after its row ended throws, so
 * it starts nothing more inside the row after it.
 */
export function rowShutdown(shutdown: () => unknown): () => Promise<unknown> {
  stillOn(rowCheck(), "registering the end of something the row started that must end with it");
  let running: Promise<unknown> | undefined;
  const stop = () => {
    running ??= Promise.resolve(shutdown());
    return running;
  };
  rowStops.add(stop);
  return stop;
}

/**
 * Registers the shutdown of the session `fixture`'s extension runs, as `rowShutdown` does, and
 * answers what runs it: the extension's `session_shutdown` handler, handed `context`, or what
 * `context()` answers when it runs, for a row that moves its session on after this call. An
 * extension that registered no such handler fails here, naming it, rather than leave a session
 * running past its row with nothing to say so.
 */
export function rowSessionShutdown(
  fixture: { readonly handlers: ReadonlyMap<string, EventHandler> },
  context: SessionContext | (() => SessionContext)
): () => Promise<unknown> {
  const shutdown = fixture.handlers.get("session_shutdown");
  if (shutdown === undefined) throw new Error("session_shutdown handler was not registered");
  return rowShutdown(async () => shutdown({}, typeof context === "function" ? context() : context));
}

/**
 * Puts back the state of this harness a row may have changed. `tearDownRow` runs it, and so does
 * `setUpRow`, because bun skips the hooks after one that fails, the `afterEach` that runs
 * `tearDownRow` among them, and the next row must not start from the failed row's state.
 */
export function resetFileState(): void {
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
  // What a row stands in for a while and puts back in its own `finally` (`rowCheck`): the
  // `AbortSignal.timeout`, `setTimeout` and `console.warn` it replaced, the system time it moved,
  // and the NATS dial it gave a plan of its own (the stand-in's own plan dials once). The
  // stand-in carries only the part of a connection the extension uses.
  AbortSignal.timeout = originalAbortTimeout;
  globalThis.setTimeout = originalGlobalSetTimeout;
  console.warn = originalConsoleWarn;
  setSystemTime();
  setNatsConnectForTests(natsStandInConnect as unknown as typeof connect);
  if (originalDispatchUrl === undefined) delete process.env.DISPATCH_URL;
  else process.env.DISPATCH_URL = originalDispatchUrl;
  if (originalDispatchToken === undefined) delete process.env.DISPATCH_TOKEN;
  else process.env.DISPATCH_TOKEN = originalDispatchToken;
  if (originalDispatchTokenFile === undefined) delete process.env.DISPATCH_TOKEN_FILE;
  else process.env.DISPATCH_TOKEN_FILE = originalDispatchTokenFile;
  if (originalMachineID === undefined) delete process.env.ENVOY_MACHINE_ID;
  else process.env.ENVOY_MACHINE_ID = originalMachineID;
  if (originalAgentboxHost === undefined) delete process.env.AGENTBOX_HOST;
  else process.env.AGENTBOX_HOST = originalAgentboxHost;
  if (originalHome === undefined) delete process.env.HOME;
  else process.env.HOME = originalHome;
  if (originalPath === undefined) delete process.env.PATH;
  else process.env.PATH = originalPath;
  if (originalTmux === undefined) delete process.env.TMUX;
  else process.env.TMUX = originalTmux;
  if (originalTmuxPane === undefined) delete process.env.TMUX_PANE;
  else process.env.TMUX_PANE = originalTmuxPane;
  // Only a row's own scratch directory: once put back, the name holds the caller's own.
  if (
    process.env.DISPATCH_STATE_DIR !== undefined &&
    process.env.DISPATCH_STATE_DIR !== originalDispatchStateDir
  ) {
    rmSync(process.env.DISPATCH_STATE_DIR, { recursive: true, force: true });
  }
  for (const [name, value] of [
    ["DISPATCH_SESSION_ID", originalDispatchSessionId],
    ["DISPATCH_HOST", originalDispatchHost],
    ["DISPATCH_STATE_DIR", originalDispatchStateDir],
    ["XDG_STATE_HOME", originalXdgStateHome],
  ] as const) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  for (const [name, value] of originalConfigVariables) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  if (configDirectory !== "") rmSync(configDirectory, { recursive: true, force: true });
  procmgr.__resetShellConfigCacheForTests();
  clipboardState.copiedSessionIDs.length = 0;
  clipboardState.error = undefined;
  // What a row left running that an `afterEach` bun skipped did not stop: a log sink stops at
  // once, and a placed session's place cache's timer is stopped before this returns
  // (`runRowStops`), while the rest of its shutdown goes on without it.
  void runRowStops();
  tmuxPlaceState.calls.length = 0;
  tmuxPlaceState.reading = refusedPlace;
  // Each is read when an extension instance is created, and a row aborted at Bun's own timeout
  // never runs its `finally`, so a row that sets one must not decide a later row's timings; the
  // place cache's lifetime is set again by `setUpRow`.
  delete process.env.ENVOY_CONTEXT_FRAME_INTERVAL_MS;
  delete process.env.ENVOY_RESUBSCRIBE_DELAY_MS;
  delete process.env.ENVOY_SUBSCRIBE_FLUSH_TIMEOUT_MS;
  delete process.env.ENVOY_SHELL_COMMAND_LIMIT_MS;
  delete process.env.ENVOY_TMUX_PLACE_CACHE_MS;
  delete process.env.ENVOY_HEARTBEAT_MS;
  delete process.env.ENVOY_PASS_DEADLINE_MS;
  delete process.env.ENVOY_SELF_CHECK_TIMEOUT_MS;
  // AskGate's mode, its review's limit and the project its decision records go to, and
  // slack_post's Web API, which a row sets for itself.
  delete process.env.OMP_ASKGATE;
  delete process.env.OMP_ASKGATE_TIMEOUT_MS;
  delete process.env.OMP_ASKGATE_RECORD_PROJECT;
  delete process.env.SLACK_POST_API_URL;
  resetNatsStandIn();
  hostBuiltins.length = 0;
  ledgerSeam.beforeFailureNotice = undefined;
}

// What can hang is a managed-timer callback a row scheduled that never settles, which the first
// `await` below waits for; the second, a session's shutdown (`rowStops`), ends at that shutdown's
// own 1 s deadline (`SHUTDOWN_DEADLINE_MS` in `extensions/envoy.ts`).
async function tearDownRow(): Promise<void> {
  advanceRowGeneration();
  // A row abandoned at its guard while it held the test's clock would otherwise leave fake timers
  // on for every row after it (`onTestClock` says what that costs), so the clock is put back first.
  jest.useRealTimers();
  // A managed-timer callback that rejects fails the test that scheduled it. The stops and the
  // reset below run whether or not one did, so a rejection leaves no row's state to the next.
  try {
    await Promise.all(contextTimers.splice(0));
  } finally {
    try {
      // What its row left running, at its guard included, is stopped while that row's fetch
      // still answers a session's deregistration (`rowStops`).
      await runRowStops();
    } finally {
      resetFileState();
    }
  }
}

/**
 * Registers the hooks every row that drives the extension through this harness needs: the
 * environment a row may leave set, the NATS stand-in, and the reset each row starts and ends
 * with. A test file calls it once, at its own module scope, before anything sets those names.
 */
export function useExtensionHarness(): void {
  // Names a row sets for itself and the code under test reads. They are Envoy's timings (the
  // resubscribe delay, the subscribe flush's bound, the place cache's lifetime, the heartbeat, the
  // pass deadline, the context frame's interval, a shell command's limit and the self-check's
  // bound), its identity (`machineID()` reports ENVOY_MACHINE_ID, else AGENTBOX_HOST), the session
  // role, AskGate's mode, limit and record project, and slack_post's Web API. This puts each back
  // to the caller's value after every row and after the file, so the next file in this bun process
  // reads what its caller set, never what a row here left. `setUpRow` gives each its starting value
  // again before every row, most of them in `resetFileState`, since bun skips this `afterEach`
  // once an `afterEach` of a `describe`, which it runs first, has failed.
  restoreEnvironment([
    "ENVOY_RESUBSCRIBE_DELAY_MS",
    "ENVOY_SUBSCRIBE_FLUSH_TIMEOUT_MS",
    "ENVOY_MACHINE_ID",
    "AGENTBOX_HOST",
    "ENVOY_TMUX_PLACE_CACHE_MS",
    "ENVOY_SESSION_ROLE",
    "ENVOY_SESSION_ROLE_SESSION",
    "ENVOY_HEARTBEAT_MS",
    "ENVOY_PASS_DEADLINE_MS",
    "ENVOY_CONTEXT_FRAME_INTERVAL_MS",
    "ENVOY_SHELL_COMMAND_LIMIT_MS",
    "ENVOY_SELF_CHECK_TIMEOUT_MS",
    "OMP_ASKGATE",
    "OMP_ASKGATE_TIMEOUT_MS",
    "OMP_ASKGATE_RECORD_PROJECT",
    "SLACK_POST_API_URL",
  ]);
  // The slot the setter writes is the process's, not the calling file's, so it is set in a hook
  // and cleared in one: a `bun test -t <pattern>` that selects no row of that file runs neither,
  // where a set at module scope would run and leave the next file of the same run dialling this
  // stand-in. `setUpRow` sets it before every row, with the stand-in's own plan
  // (`resetFileState`), so a row that gave it a plan of its own decides no later row's dial.
  // Every row builds its own instance in its own body.
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

/**
 * The probe of a wait (`until`, `@legion/test-fixtures/wait`) that reads `condition` while the
 * caller's row runs: a row waits for what the code under test does on real turns or real timers,
 * such as the resubscribe path's (`ENVOY_RESUBSCRIBE_DELAY_MS`, 10 ms in these tests), which a
 * busy machine runs later than any fixed sleep allows. The row is the one this call is made in
 * (`rowCheck`). Once it has ended, the harness's `tearDownRow` advances `rowGeneration` and the
 * probe throws (`stillOn`), which ends the wait at once, as every helper of this harness does
 * then, so an abandoned body goes no further than its wait. A wait refuses under
 * `jest.useFakeTimers()`, so a row on the test's clock (`onPassClock`) waits with its clock's own
 * `hold`, `run`, `wait` and `waitOn` instead.
 */
export function rowProbe<T>(condition: () => T): () => T {
  const ownRow = rowCheck();
  return () => {
    stillOn(ownRow, "waiting");
    return condition();
  };
}

/**
 * Drives a session's heartbeat by hand (`intervals[0]`) and counts the registrations the
 * listener stand-in accepts: the stand-in's fetch calls `accept()` for each one. Each wait
 * answers with the count the listener reached, and ends at its row's deadline, naming what it
 * waited for (`@legion/test-fixtures/wait`). Once its row has ended, each wait throws
 * (`stillOn`) before its next tick and in place of its answer: it ticks no dead extension into
 * the rows after it, and it answers no count once its row has ended.
 */
export function heartbeatTicks(intervals: readonly (() => void)[]) {
  let registrations = 0;
  /** Told of the next registration the listener accepts, while `registered` waits for one. */
  let told: (() => void) | undefined;
  return {
    /** The listener accepted a registration: the stand-in's fetch calls this for each one. */
    accept: (): void => {
      registrations += 1;
      told?.();
    },
    /** How many registrations the listener has accepted. */
    registrations: (): number => registrations,
    /**
     * Ticks once, and waits for the listener to accept the next registration. It is for a tick
     * no pass is running at: that tick starts a pass, and the pass registers. It never ticks
     * again. A tick while that pass runs, once a whole refresh interval has passed since the
     * last registration, meets a stall, and the heartbeat says so.
     */
    registered: async (): Promise<number> => {
      const ownRow = rowCheck();
      stillOn(ownRow, "waiting for a tick's registration");
      const accepted = Promise.withResolvers<void>();
      told = accepted.resolve;
      try {
        intervals[0]?.();
        await settled("the tick's registration", accepted.promise);
      } finally {
        told = undefined;
      }
      stillOn(ownRow, "waiting for a tick's registration");
      return registrations;
    },
    /**
     * Ticks until the listener has accepted one more registration, and answers the count. A
     * tick that lands inside the refresh interval posts nothing, so the wait's probe ticks
     * again, at the wait's own pace, until the count moves: a refresh is awaited where it
     * shows, at the listener.
     */
    refreshed: (): Promise<number> => {
      const ownRow = rowCheck();
      const seen = registrations;
      return until("a tick's refresh of the registration", () => {
        stillOn(ownRow, "ticking for a refresh");
        if (registrations !== seen) return registrations;
        intervals[0]?.();
        return undefined;
      });
    },
  };
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
 * Waits with the test's clock held until `done()` holds, running `turn` between reads, and ends
 * at the row's deadline, read in real time (`realTimeDeadline`), since the shared wait refuses to
 * start on the test's clock: one whose `done()` does not hold by then fails here, naming `what`
 * and that deadline, before the command's guard would end the row. `turn` is handed the moment
 * that deadline is reached, for a wait that parks on a promise rather than running turns.
 * `caller` names the wait to `realTimeDeadline`. The caller checks its own row (`stillOn`).
 */
export async function heldUntil(
  caller: string,
  what: string,
  done: () => boolean,
  turn: (deadlineReached: Promise<void>) => Promise<unknown>
): Promise<void> {
  const deadline = realTimeDeadline(`${caller} for ${what}`);
  while (!done()) {
    if (deadline.passed()) {
      throw new Error(
        `${what} did not come by the row's deadline, ${deadline.source}, with the test's clock held`
      );
    }
    await turn(deadline.reached);
  }
}

/**
 * Runs `body` with fake timers on (`jest.useFakeTimers`), and answers what it answered. Real
 * timers come back however `body` ends while its row runs (`rowCheck`). Once the row has ended,
 * at its guard included, `setUpRow` has put real timers back, and the row after it may hold a
 * fake clock of its own. So a call made after its row ended throws, saying so, before it touches
 * the timers, and a body that ends after its row leaves them as they are.
 */
export async function onFakeTimers<T>(body: () => Promise<T>): Promise<T> {
  const ownRow = rowCheck();
  stillOn(ownRow, "starting the test's clock");
  jest.useFakeTimers();
  try {
    return await body();
  } finally {
    if (ownRow()) jest.useRealTimers();
  }
}

/**
 * Settles `pending` on the test's clock (`onFakeTimers`), moving it `stepMs` at a time for at
 * most `steps` steps, and answers with what it settled to and how far the test's clock had moved
 * when it settled. The default, a second at a time for a minute, is for a write Dispatch keeps
 * failing, which is asked again under its key until the accept's 10 s bound: those pauses and
 * that bound pass at once rather than in real time. With `waiting`, the clock stands still until
 * it says the work is waiting on it: before its first request, the accept reads the session's
 * envoy.json, which is real I/O, while the accept's bound (an `AbortSignal.timeout`, on the
 * test's clock) already runs, and a step moved then would be one the code under test never
 * waited. That wait is on real turns, and it ends at the row's deadline, read in real time
 * (`realTimeDeadline`), since the shared wait refuses to start on the test's clock: work that has
 * not waited on the clock by then fails here, naming what the row waited for and that deadline,
 * with real timers restored, before the command's guard would end the row. It stops once its row
 * has ended too (`stillOn`).
 *
 * A pending that has not settled when the steps run out fails here, at once and with real timers
 * already restored. Left to the command's guard instead, Bun abandons the row with fake timers
 * still on, and every later row's `setTimeout` and `Bun.sleep` stop with them: measured on bun
 * 1.4.4, one such defect in the `session_shutdown` row failed six correct rows after it and cost
 * `envoy.test.ts` about 145 s more. Each move of the clock throws instead once the row has ended
 * (`stillOn`), as `onFakeTimers` does at the start.
 */
export function onTestClock<T>(
  pending: () => Promise<T>,
  {
    stepMs = 1_000,
    steps = 60,
    waiting,
  }: { stepMs?: number; steps?: number; waiting?: () => boolean } = {}
): Promise<{ value: T; elapsedMs: number }> {
  const ownRow = rowCheck();
  return onFakeTimers(async () => {
    let settled = false;
    let elapsedMs = 0;
    const startedAt = Date.now();
    const watched = pending().finally(() => {
      settled = true;
      // Read while the test's clock is still the clock.
      elapsedMs = Date.now() - startedAt;
    });
    watched.catch(() => undefined);
    if (waiting !== undefined) {
      const what = "the work to wait on the test's clock";
      await heldUntil(
        "onTestClock's wait",
        what,
        () => {
          if (settled || waiting()) return true;
          stillOn(ownRow, `waiting for ${what}`);
          return false;
        },
        realTurn
      );
    }
    for (let step = 0; step < steps && !settled; step++) {
      for (let flush = 0; flush < 50 && !settled; flush++) await Promise.resolve();
      if (!settled) {
        stillOn(ownRow, "moving the test's clock");
        jest.advanceTimersByTime(stepMs);
      }
    }
    for (let flush = 0; flush < 50 && !settled; flush++) await Promise.resolve();
    if (!settled) {
      throw new Error(`did not settle within ${steps * stepMs} ms of the test's clock`);
    }
    return { value: await watched, elapsedMs };
  });
}

/** What a row on `onPassClock` holds and moves the test's clock with. Once the row has ended, at
 *  its guard included, every hold, wait and move throws, naming what it was at, and `waitOn` does
 *  so once its promise settles (`onPassClock`). */
export interface PassClock {
  /** Lets the code's own work run on real turns (`settle`), which move no clock, until `done()`
   *  holds: for a row that relies on it. One that still does not hold once 10_000 turns have run
   *  fails here, naming `what`, the fact the row waited for. */
  readonly hold: (what: string, done: () => boolean) => Promise<void>;
  /** Lets `count` real turns run with the clock held, as `hold` does, for the reason `why`, and
   *  fails on nothing: a span, for a row that holds to see a thing not happen, or one that ends
   *  the span early once `done()` holds and reads afterwards, by a check of its own, whether it
   *  did. */
  readonly turns: (count: number, why: string, done?: () => boolean) => Promise<void>;
  /** Holds as `hold` does until `pending` settles, and answers what it settled to; one that has
   *  not settled when the turns run out fails here, naming `what`. */
  readonly run: <T>(what: string, pending: Promise<T> | T, turns?: number) => Promise<T>;
  /** Holds as `hold` does until `done()` holds, with no bound of turns: for work that reads a file
   *  or waits on another process, whose speed no count of turns measures. It ends at the row's
   *  deadline instead, read in real time (`realTimeDeadline`), since the shared wait refuses to
   *  start on the test's clock: one whose `done()` does not hold by then fails here, naming `what`
   *  and that deadline, before the command's guard would end the row. It ends with its row too,
   *  as every hold and move does: once the row has ended, the harness's hooks advance
   *  `rowGeneration`, and the wait throws, naming `what`, rather than spinning on beside the rows
   *  after it. */
  readonly wait: (what: string, done: () => boolean) => Promise<void>;
  /** Holds the clock until `pending` settles, then lets one `settle` of real turns run, and
   *  answers what `pending` settled to: for work that reads a file or waits on another process,
   *  such as a trade, which first reads the pod's token from disk (`projectedToken`'s `readFile`).
   *  It runs no turns while it waits, so it takes no core from that work. It ends at the row's
   *  deadline, as `wait` does: a `pending` that has not settled by then fails here, naming `what`
   *  and that deadline. Once its row has ended, a body parked here throws when `pending` settles,
   *  naming `what`, and stays parked if `pending` never settles, since the next mark of the
   *  row-deadline preload retires the ended row's deadline unreached. */
  readonly waitOn: <T>(what: string, pending: Promise<T> | T) => Promise<T>;
  /** Each `AbortSignal.timeout` the code armed since the clock was installed, in order, by its
   *  length: where a bound the row reads starts. */
  readonly armed: readonly number[];
  /** Moves the clock and lets what it ended run on real turns. */
  readonly advance: (ms: number) => Promise<void>;
  /** Moves the clock `ms` in steps of `stepMs` (10 by default), each step held on real turns as
   *  `advance` holds it: for a span the row spends on this process's own short timers (a
   *  listener call's retry, a pass's deadline), which then end in the order the code gives them,
   *  each with its work done before the next step. */
  readonly steps: (ms: number, stepMs?: number) => Promise<void>;
  /** Steps the clock as `steps` does until `pending` settles, and answers what it settled to:
   *  for a call whose work waits on such a timer. One that has not settled once `withinMs` of the
   *  test's clock has passed fails here, naming `what`. */
  readonly stepping: <T>(
    what: string,
    pending: Promise<T> | T,
    withinMs: number,
    stepMs?: number
  ) => Promise<T>;
  /** The test's clock. */
  readonly now: () => number;
}

/**
 * Runs `body` on the test's clock, for a row that reads how this process's own timers end its
 * work: a pass's deadline, a start's or a follow's bound and its trade's share, or a token's
 * renewal. On the test's clock they end at the times the code gives them, whatever the host's
 * speed, while the work between them (restores, dials, requests, reports, in-process here, or a
 * shell a row starts) runs on real turns, which move no clock. `AbortSignal.timeout`,
 * which bun 1.4.4's fake clock holds as it holds `setTimeout`, is watched for the bounds the code
 * arms, because a pass that reaches its `ready` makes no request a row could wait for. `Bun.sleep`
 * is held too, so a row on this clock waits with `hold`, `run`, `wait` or `waitOn`, never with
 * `until`. Real timers and `AbortSignal.timeout` come back however `body` ends inside its row, so
 * a hold that runs out fails there, naming what it waited for. Each hold, and each run unless it
 * names more, is bounded by 10_000 turns of `settle`, a hang guard: what can hang is work that
 * never finishes; the most the switch-follow row's pass took was 105 of them idle and 58 at the
 * 96 rung. A span (`turns`) runs the turns it names and fails on nothing. A wait for work that
 * reads a file or waits on another process (`wait`, `waitOn`) has no such bound, since no count of
 * turns measures that work: it ends at the row's deadline, read in real time
 * (`realTimeDeadline`), and fails there, naming what it waited for and that deadline, before the
 * command's guard would end the row. Once its row has ended, at its guard included, every hold,
 * wait and move of the clock throws, naming what it was waiting for and saying the row ended, and
 * so does this call: a row its guard abandoned is over, and `setUpRow` has put real timers and
 * `AbortSignal.timeout` back for the row after it, which may hold a fake clock of its own by
 * then. A body parked in `waitOn` throws so once its promise settles, and stays parked if the
 * promise never settles. So the abandoned body stops at its next wait rather than move that
 * clock, and its end leaves the timers and `AbortSignal.timeout` as that row has them.
 */
export async function onPassClock<T>(body: (clock: PassClock) => Promise<T>): Promise<T> {
  const ownRow = rowCheck();
  /** One `settle`, then a throw, saying what the caller was at, once the row has ended. Each
   *  caller checks the row itself before it calls this. */
  const settleFor = async (what: string): Promise<void> => {
    await settle();
    stillOn(ownRow, what);
  };
  const timeout = AbortSignal.timeout;
  const armed: number[] = [];
  const holdFor = async (what: string, done: () => boolean, turns: number): Promise<void> => {
    stillOn(ownRow, what);
    for (let turn = 0; turn < turns && !done(); turn += 1) await settleFor(what);
  };
  const watched = (ms: number) => {
    armed.push(ms);
    return timeout.call(AbortSignal, ms);
  };
  const advanceFor = async (what: string, ms: number): Promise<void> => {
    stillOn(ownRow, what);
    jest.advanceTimersByTime(ms);
    await settleFor(what);
  };
  const clock: PassClock = {
    hold: async (what, done) => {
      const turns = 10_000;
      await holdFor(`waiting for ${what}`, done, turns);
      if (!done()) {
        throw new Error(`${what} did not come in ${turns} turns with the test's clock held`);
      }
    },
    turns: (count, why, done = () => false) =>
      holdFor(`holding the test's clock for ${why}`, done, count),
    run: async (what, pending, turns = 10_000) => {
      let settled = false;
      const watched = Promise.resolve(pending).finally(() => {
        settled = true;
      });
      watched.catch(() => undefined);
      await holdFor(`waiting for ${what}`, () => settled, turns);
      if (!settled) throw new Error(`${what} did not settle with the test's clock held`);
      return await watched;
    },
    wait: async (what, done) => {
      const at = `waiting for ${what}`;
      stillOn(ownRow, at);
      await heldUntil("onPassClock's wait", what, done, () => settleFor(at));
    },
    waitOn: async (what, pending) => {
      const at = `waiting for ${what}`;
      const watched = Promise.resolve(pending);
      let settled = false;
      // Handled before the row's check, so a `pending` that rejects after that check threw, or
      // after the deadline below has passed, is not left unhandled.
      const settling = watched.then(
        () => {
          settled = true;
        },
        () => {
          settled = true;
        }
      );
      stillOn(ownRow, at);
      await heldUntil(
        "onPassClock's waitOn",
        what,
        () => settled,
        (deadlineReached) => Promise.race([settling, deadlineReached])
      );
      await settleFor(at);
      return await watched;
    },
    armed,
    advance: (ms) => advanceFor(`moving the test's clock ${ms} ms`, ms),
    steps: async (ms, stepMs = 10) => {
      const what = `stepping the test's clock ${ms} ms`;
      stillOn(ownRow, what);
      for (let moved = 0; moved < ms; moved += stepMs) await advanceFor(what, stepMs);
    },
    stepping: async (what, pending, withinMs, stepMs = 10) => {
      const at = `stepping the test's clock for ${what}`;
      let settled = false;
      const watched = Promise.resolve(pending).finally(() => {
        settled = true;
      });
      watched.catch(() => undefined);
      stillOn(ownRow, at);
      await settleFor(at);
      for (let moved = 0; !settled && moved < withinMs; moved += stepMs) {
        await advanceFor(at, stepMs);
      }
      if (!settled) {
        throw new Error(`${what} did not settle within ${withinMs} ms of the test's clock`);
      }
      return await watched;
    },
    now: () => Date.now(),
  };
  const ofBody = "waiting for the body on the test's clock";
  const value = await onFakeTimers(async () => {
    AbortSignal.timeout = watched;
    try {
      return await body(clock);
    } catch (error) {
      if (ownRow()) throw error;
      // The row ended before its body did, and its guard failed it: what the body met since goes
      // out as the cause of an error that says so, which bun reports between the rows.
      throw new Error(`stopped ${ofBody}: the row ended`, { cause: error });
    } finally {
      // Once the row has ended, `setUpRow` has put it back for the row after it.
      if (ownRow()) AbortSignal.timeout = timeout;
    }
  });
  stillOn(ownRow, ofBody);
  return value;
}

/**
 * Runs `body` with the Envoy timings it names, and restores whatever the process had while its
 * row still runs (`rowCheck`). The extension factory reads them when it runs (`envoySettings`),
 * so a test sets them before its own `envoyExtension(...)` call; every test of the files that
 * share this harness runs in one process, so none may leak.
 */
export async function withEnvoyTimings<T>(
  timings: {
    readonly heartbeatMs?: number;
    readonly passDeadlineMs?: number;
    readonly tmuxPlaceCacheMs?: number;
  },
  body: () => Promise<T>
): Promise<T> {
  const ownRow = rowCheck();
  // A body its guard abandoned that only now gets here sets nothing for the row after it.
  stillOn(ownRow, "setting the Envoy timings");
  const restore = captureEnvironment([
    "ENVOY_HEARTBEAT_MS",
    "ENVOY_PASS_DEADLINE_MS",
    "ENVOY_TMUX_PLACE_CACHE_MS",
  ]);
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
    if (ownRow()) restore();
  }
}

/**
 * The `ENVOY_PASS_DEADLINE_MS` of every row on `gatedRoleFixture`, which sets it, and of the rows
 * of the same kind that boot a session of their own: an hour, past the command's guard that each
 * of those rows runs under. Each pass and role move is abandoned at its deadline, a real
 * `setTimeout` of the extension (`createRegistrationState`, `src/registration-state.ts`), and at
 * the 90 s it is otherwise (the heartbeat's interval, which a pass's deadline is never shorter
 * than) a host slow enough could abandon a pass the row did not mean to. With an hour no deadline
 * ends a correct row's pass, however slow the host, and a pass that hangs ends at the command's
 * guard, which names the row.
 * This is not the test's clock (`onPassClock`) for most of those rows: they run on the real
 * clock and wait the listener's 250 ms retry (`LISTENER_CALL_RETRY_DELAY_MS`) and other short
 * real timers out, which a held clock would stop. The rows that boot the fixture inside
 * `onPassClock` (`clock.waitOn`) step that retry instead, 10 ms at a time (`stepping`, `steps`).
 * The long deadline takes the race away on both clocks: neither comes near an hour in a row.
 * `envoySettings` takes any finite number above zero and holds it under 2 147 483 647 ms, so the
 * hour stands. The harness's reset deletes the variable before and after every row
 * (`resetFileState`), so it reaches no other row.
 */
export const UNRACED_PASS_DEADLINE_MS = 3_600_000;

/**
 * Every `envoy: ENVOY_SESSION_ROLE …` line the extension logs from now on, which is the line a
 * process logs once when a session it decides for is not the one the launch named; `stop` ends
 * the collecting.
 */
export function sessionRoleLines(): { readonly lines: string[]; readonly stop: () => void } {
  const lines: string[] = [];
  const stop = rowLogSink((entry) => {
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
