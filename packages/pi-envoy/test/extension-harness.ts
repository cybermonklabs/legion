// The harness `extensions/envoy.test.ts` and `extensions/consumer-delivery.test.ts` share: the NATS
// stand-in the extension dials, the fetch that records every registration, the host modules both
// suites mock, the reset every row starts and ends with, and the helpers both suites call. Bun
// evaluates an imported module once per process, so both files drive this one set of state, and
// `mock.module` replaces a module for the whole process. For the same reason this module registers
// no hook in its body: a hook here would take the scope of whichever test file imported it first.
// Each file calls `useExtensionHarness()` at its own module scope instead.
import { afterAll, afterEach, beforeAll, beforeEach, mock } from "bun:test";
import { AsyncLocalStorage } from "node:async_hooks";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BUS_UNANSWERED_PREFIX } from "@legion/contracts";
import { forgetBrokerClients } from "@legion/envoy-client/broker-credential";
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery";
import type {
  DeliveryLedgerHost,
  DeliveryLedgerOptions,
} from "@legion/envoy-client/delivery-ledger";
import type { ExecFn } from "@legion/envoy-client/dispatch-cwd";
import { forgetKeyedLayerMemory } from "@legion/envoy-client/dispatch-http";
import { subjectsOverlap } from "@legion/envoy-client/stream-topics";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import type { TmuxPlaceReading } from "@legion/envoy-client/tmux-place";
import { resetEnvoyPluginInterfaceForTests } from "@legion/pi-shared/interface";
import { hostAgentRegistryMock, testAgentRoster } from "@legion/pi-shared/test/host-registry";
import { captureEnvironment, restoreEnvironment } from "@legion/test-fixtures/environment";
import type { UserInputBuiltinCommand } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler";
import { procmgr } from "@oh-my-pi/pi-utils";
import { ErrorCode, NatsError, connect as realConnect } from "nats";
import type { z } from "zod";
import type * as EnvoyEntry from "../extensions/envoy";
import { resetEnvoySessionsForTests } from "../src/envoy-session";
import { createStandInHost, type SessionContext } from "./stand-in-host";

/** A message's headers, as nats.js's `MsgHdrs` reads them: a missing one reads `""`. */
type MessageHeaders = { readonly get: (name: string) => string };

type Subscription = {
  readonly unsubscribe: () => void;
  readonly [Symbol.asyncIterator]: () => AsyncIterator<{
    readonly subject: string;
    readonly data: Uint8Array;
    readonly reply: string;
    readonly headers?: MessageHeaders;
  }>;
};

export type SubscriptionControls = {
  /** Hands the subscription a message; `headers`, when given, are the message's NATS headers. */
  readonly push: (data: string, reply?: string, headers?: Readonly<Record<string, string>>) => void;
  readonly end: () => void;
  readonly fail: (error: Error) => void;
  readonly active: () => boolean;
};

/**
 * Every subscription this process really holds, by subject: after a registration that is its
 * relay subject and its agent stream's control subject, and nothing else — a notification topic
 * reaches the session only through the relay of its own consumer.
 */
const openSubscriptions = new Map<string, SubscriptionControls>();

/** The stream sequence the next relayed frame carries. */
let relaySeq = 0;

/**
 * The topics the listener was last told this session follows, taken from every registration this
 * process makes. The session's consumer is filtered on exactly them, so they are the subjects a
 * relay frame can arrive on, and they are what "the session follows this topic" means now that
 * the plugin holds no subscription of its own on any of them.
 */
const registeredTopics = new Set<string>();

/** Whether a registered topic reaches `subject`, under NATS wildcard rules. */
function relayCarries(subject: string): boolean {
  for (const topic of registeredTopics) {
    if (subjectsOverlap(topic, subject)) return true;
  }
  return false;
}

/**
 * Records every registration whatever `fetch` a row installs, so the followed topics are known
 * centrally. A row's own fetch is kept behind this one; reading `globalThis.fetch` answers this
 * wrapper, so a row that wraps the fetch it found takes it from `rowFetch()`, never from
 * `globalThis.fetch`, which would call the row's own wrapper forever. That mistake fails at once,
 * by name: a call nested past `MOST_NESTED_FETCHES` of this wrapper's own is refused. Made once,
 * it grew one row's process to 118 GiB before anything stopped it.
 */
let installedFetch: typeof globalThis.fetch = globalThis.fetch;
/** The real fetch, as this module found it: what a row restores and what the live stack uses. */
const nativeFetch: typeof globalThis.fetch = installedFetch;
/** How deep the calls of `recordingFetch` are nested in the current call's async context. */
const fetchDepth = new AsyncLocalStorage<number>();
/** A row's fetch may hand a request on through `globalThis.fetch` this many times at most. */
const MOST_NESTED_FETCHES = 8;
const recordingFetch = (async (input, init) => {
  const depth = (fetchDepth.getStore() ?? 0) + 1;
  if (depth > MOST_NESTED_FETCHES) {
    throw new Error(
      `a row's fetch handed its request on through globalThis.fetch ${MOST_NESTED_FETCHES} times: globalThis.fetch is this harness's recording wrapper, which calls the row's fetch again, so take the fetch to hand on to from rowFetch()`
    );
  }
  try {
    if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as {
        readonly topics?: readonly string[];
      };
      registeredTopics.clear();
      for (const topic of body.topics ?? []) registeredTopics.add(topic);
    }
  } catch {
    // A request that is not a registration, or a body that is not JSON, teaches nothing.
  }
  return fetchDepth.run(depth, () => installedFetch(input, init));
}) as typeof globalThis.fetch;
/** The fetch a row installed, for a row that answers some requests itself and hands on the rest. */
export const rowFetch = (): typeof globalThis.fetch => installedFetch;
Object.defineProperty(globalThis, "fetch", {
  configurable: true,
  get: () => recordingFetch,
  set: (next: typeof globalThis.fetch) => {
    if (next !== recordingFetch) installedFetch = next;
  },
});

/**
 * Hands the session a frame published on `subject`, as the listener's relay of the session's
 * consumer hands it over: the frame travels the relay subject with the headers that name the
 * subject it was published on. Undefined while no relay subject is subscribed, as the controls
 * of an unsubscribed subject were.
 *
 * An agent subject is one session's own, so its own relay carries it, open or closed: a row
 * running two sessions holds two relays, and a session the host moved leaves the relay of every
 * session it was before in the map. Any other subject takes the live relay, and the last one
 * closed when none is live: what a subject answers is the subscription the session holds now,
 * never one it held before.
 */
function relayControls(subject: string): SubscriptionControls | undefined {
  if (!relayCarries(subject)) return undefined;
  const AGENT_PREFIX = "notifications.agent.";
  let live: SubscriptionControls | undefined;
  let closed: SubscriptionControls | undefined;
  const own = subject.startsWith(AGENT_PREFIX)
    ? openSubscriptions.get(deliverSubject(subject.slice(AGENT_PREFIX.length)))
    : undefined;
  for (const [on, controls] of openSubscriptions) {
    if (!on.startsWith("notifications.envoy.deliver.")) continue;
    if (controls.active()) live ??= controls;
    else closed = controls;
  }
  const relay = own ?? live ?? closed;
  if (relay === undefined) return undefined;
  return {
    push: (data, reply, headers) => {
      relaySeq += 1;
      relay.push(data, reply === undefined || reply === "" ? `_INBOX.relay.${relaySeq}` : reply, {
        [RelayHeader.subject]: subject,
        [RelayHeader.streamSeq]: String(relaySeq),
        [RelayHeader.streamID]: TEST_STREAM_ID,
        [RelayHeader.delivered]: "1",
        [RelayHeader.storedAt]: new Date().toISOString(),
        ...headers,
      });
    },
    end: relay.end,
    fail: relay.fail,
    active: relay.active,
  };
}

export const natsState = {
  subscriptions: new Map<string, Subscription>(),
  controls: {
    /** The subject's own subscription, else the relay that carries it (`relayControls`). */
    get: (subject: string): SubscriptionControls | undefined =>
      openSubscriptions.get(subject) ?? relayControls(subject),
    /** Whether the session takes `subject`: its own subscription, or a topic its consumer
     *  relays (`relayCarries`). */
    has: (subject: string): boolean => openSubscriptions.has(subject) || relayCarries(subject),
    set: (subject: string, controls: SubscriptionControls): void => {
      openSubscriptions.set(subject, controls);
    },
    clear: (): void => openSubscriptions.clear(),
    keys: (): IterableIterator<string> => openSubscriptions.keys(),
    /**
     * Every subject this process holds a live NATS subscription on, sorted: after a registration
     * that is the session's relay subject and its agent-stream control subject, and nothing else.
     */
    open: (): string[] =>
      [...openSubscriptions]
        .filter(([, controls]) => controls.active())
        .map(([subject]) => subject)
        .sort(),
  },
  controlsByTopic: new Map<string, SubscriptionControls[]>(),
  published: [] as { readonly subject: string; readonly data: Uint8Array | undefined }[],
  onPublish: undefined as ((subject: string) => void) | undefined,
  failPublishes: 0,
  failSubscribes: 0,
  connectedNames: [] as string[],
  /** What each connect was given: its servers, and whether it carried an nkey authenticator. */
  connects: [] as { readonly servers: readonly string[]; readonly authenticated: boolean }[],
  /** The token each CONNECT presented (`auth_token`), where its authenticator answers one: a
   *  connect's and each reconnect's, in order. */
  presented: [] as string[],
  /** Each `subscribe` and `reconnect`, in order, for a row that holds the two to an order. */
  timeline: [] as string[],
  /** Each connection `close()` was called on, by the name its dial carried: a dial nothing holds
   *  is never drained, and with `maxReconnectAttempts: -1` it outlives every session. */
  closedNames: [] as string[],
  /** Every connection the stand-in made, in order, so a row can close one for good, as nats.js
   *  closes a connection on an authorization error it does not ignore, or on `close()`. */
  connections: [] as { readonly close: () => Promise<void> }[],
  failConnects: 0,
  /** The error a failed connect throws, as nats.js throws it: its refusal by default, a
   *  resolver's (`getaddrinfo`, its text naming the host) for a row that sees what a notice
   *  carries of it. */
  failError: (): unknown => NatsError.errorForCode(ErrorCode.ConnectionRefused),
  /** What `closed()` of a connection settles with once it is closed, as nats.js settles it with
   *  the close's own error (protocol.js:906-913); none by default. */
  closeError: undefined as Error | undefined,
  /** Every connection reports itself closed, as one NATS dropped does. */
  connectionsClosed: false,
  /** Hands each connect, once recorded, to nats.js itself, for a row that watches what it dials. */
  realConnect: false,
  /** A connect a test parks on, so a pass can be abandoned at its deadline while its connection
   *  is still being made and what its late continuation writes can be watched. */
  heldConnect: undefined as Promise<void> | undefined,
  drainHangs: false,
  drainStarted: false,
  /** Each mocked connection's `status()` readers: a push hands every one of them the status, as
   *  nats.js reports a reconnect to every reader of the connection's status iterator. */
  statusReaders: [] as ((status: { readonly type: string; readonly data: unknown }) => void)[],
  /** Every subject the connection had been asked to subscribe to when a `flush()` was called:
   *  the round trip the server answers, which is what makes a subscription live. */
  flushes: [] as string[][],
  /** A `flush()` a test parks on, so what the extension does before the server confirms a
   *  subscription can be watched. */
  heldFlush: undefined as Promise<void> | undefined,
  /** The subject whose round trip rejects: the next `flush()` taken once that subject's SUB has
   *  been written fails and clears this, as nats.js rejects every pending flush when the
   *  connection drops (`nats@2.29.3` `protocol.js`, `resetOutbound`). Named rather than counted,
   *  because a pass takes one round trip per connection and only one case's subject carries it. */
  failFlushOnSubject: undefined as string | undefined,
  /** Whether that rejection also closes its connection: a rejection on a connection that is not
   *  closed leaves the open to go on, and only a connection closed for good makes it give up. */
  failFlushClosesConnection: false,
};

/** Reports `{ type }` to every reader of a mocked connection's `status()`. */
export function pushNatsStatus(type: string): void {
  for (const read of natsState.statusReaders) read({ type, data: "" });
}

const clipboardState = {
  copiedSessionIDs: [] as string[],
  error: undefined as Error | undefined,
};

// The dial `extensions/envoy.ts` opens NATS with, handed to it below through
// `setNatsConnectForTests`. The `nats` module itself stays as it is: a row that sets
// `natsState.realConnect` gets nats.js's own connect from here, and every file in this `bun test`
// process — `card-delivery-omp.test.ts` among them — reads the real module. A row that hands the
// entry a plan of redials of its own hands it this stand-in again with that plan.
export const natsStandInConnect = async (options: {
  readonly name: string;
  readonly servers: readonly string[];
  readonly authenticator?: unknown;
}) => {
  const { name, servers, authenticator } = options;
  if (natsState.failConnects > 0) {
    natsState.failConnects -= 1;
    throw natsState.failError();
  }
  natsState.connectedNames.push(name);
  natsState.connects.push({ servers, authenticated: authenticator !== undefined });
  // A CONNECT asks the authenticator for its credential over the server's nonce, as nats.js does
  // at every connect and reconnect.
  const connectWith = (nonce: string) => {
    if (typeof authenticator !== "function") return;
    const credential = (authenticator as (nonce: string) => unknown)(nonce);
    if (typeof credential === "object" && credential !== null && "auth_token" in credential) {
      natsState.presented.push(String(credential.auth_token));
    }
  };
  connectWith("stand-in-nonce");
  // Parked only where a test asks for it: every other connect answers at once, as before.
  if (natsState.heldConnect !== undefined) await natsState.heldConnect;
  if (natsState.realConnect) {
    return realConnect(options as unknown as Parameters<typeof realConnect>[0]);
  }
  let closed = false;
  // nats.js's `closed()`: settles once the connection is closed for good, with the close's error.
  const ended = Promise.withResolvers<Error | undefined>();
  const made = {
    isClosed: () => closed || natsState.connectionsClosed,
    closed: () => ended.promise,
    close: async () => {
      closed = true;
      natsState.closedNames.push(name);
      ended.resolve(natsState.closeError);
    },
    drain: async () => {
      natsState.drainStarted = true;
      if (natsState.drainHangs) await Promise.withResolvers<never>().promise;
    },
    // nats.js's client-initiated reconnect: the connection drops and comes back with a CONNECT of
    // its own, which every reader of its status hears of once it is back.
    reconnect: async () => {
      natsState.timeline.push("reconnect");
      connectWith("stand-in-nonce-again");
      setTimeout(() => pushNatsStatus("reconnect"), 0);
    },
    // An async iterable of the connection's status events, which a test feeds through
    // `pushNatsStatus`; it ends once the connection is closed.
    status: () => {
      const queued: { readonly type: string; readonly data: unknown }[] = [];
      let wake: (() => void) | undefined;
      const reader = (status: { readonly type: string; readonly data: unknown }) => {
        queued.push(status);
        wake?.();
        wake = undefined;
      };
      natsState.statusReaders.push(reader);
      return {
        [Symbol.asyncIterator]: () => ({
          next: async (): Promise<
            IteratorResult<{ readonly type: string; readonly data: unknown }>
          > => {
            for (;;) {
              if (closed) return { done: true, value: undefined };
              const status = queued.shift();
              if (status !== undefined) return { done: false, value: status };
              const pending = Promise.withResolvers<void>();
              wake = pending.resolve;
              await pending.promise;
            }
          },
        }),
      };
    },
    // nats.js's round trip: the server has every SUB written before it when this resolves, and
    // every pending one of them rejects when the connection drops. A rejection is transient
    // unless the connection is closed with it, which is the one case the extension gives up on.
    flush: async () => {
      natsState.flushes.push([...natsState.subscriptions.keys()]);
      if (natsState.heldFlush !== undefined) await natsState.heldFlush;
      const failing = natsState.failFlushOnSubject;
      if (failing !== undefined && natsState.controls.has(failing)) {
        natsState.failFlushOnSubject = undefined;
        if (natsState.failFlushClosesConnection) closed = true;
        throw new Error(`FLUSH_FAILED ${failing}`);
      }
    },
    publish: (subject: string, data?: Uint8Array) => {
      if (natsState.failPublishes > 0) {
        natsState.failPublishes -= 1;
        throw new Error("PUBLISH_FAILED");
      }
      natsState.published.push({ subject, data });
      natsState.onPublish?.(subject);
    },
    subscribe: (topic: string) => {
      natsState.timeline.push(`subscribe ${topic}`);
      if (natsState.failSubscribes > 0) {
        natsState.failSubscribes -= 1;
        throw new Error("SUBSCRIBE_FAILED");
      }
      let active = true;
      const queue: {
        readonly subject: string;
        readonly data: Uint8Array;
        readonly reply: string;
        readonly headers?: MessageHeaders;
      }[] = [];
      let wake: (() => void) | undefined;
      let ended = false;
      let failure: Error | undefined;
      const notify = () => {
        wake?.();
        wake = undefined;
      };
      const controls = {
        push: (data: string, reply = "", headers?: Readonly<Record<string, string>>) => {
          queue.push({
            subject: topic,
            data: new TextEncoder().encode(data),
            reply,
            ...(headers === undefined
              ? {}
              : { headers: { get: (name: string) => headers[name] ?? "" } }),
          });
          notify();
        },
        end: () => {
          ended = true;
          notify();
        },
        fail: (error: Error) => {
          failure = error;
          notify();
        },
        active: () => active,
      };
      natsState.controls.set(topic, controls);
      const controlsForTopic = natsState.controlsByTopic.get(topic);
      if (controlsForTopic === undefined) natsState.controlsByTopic.set(topic, [controls]);
      else controlsForTopic.push(controls);
      const subscription: Subscription = {
        unsubscribe: () => {
          active = false;
          // nats.js ends the async iterator on unsubscribe(), which is what
          // makes a deliberate close indistinguishable from a dropped
          // connection inside the pump. A mock that leaves the iterator
          // parked cannot observe that, and lets resubscribe bugs ship green.
          notify();
        },
        [Symbol.asyncIterator]: () => ({
          next: async (): Promise<
            IteratorResult<{
              readonly subject: string;
              readonly data: Uint8Array;
              readonly reply: string;
              readonly headers?: MessageHeaders;
            }>
          > => {
            for (;;) {
              if (failure) throw failure;
              if (!active || ended) return { done: true, value: undefined };
              const item = queue.shift();
              if (item) return { done: false, value: item };
              const pending = Promise.withResolvers<void>();
              wake = pending.resolve;
              await pending.promise;
            }
          },
        }),
      };
      natsState.subscriptions.set(topic, subscription);
      return subscription;
    },
  };
  natsState.connections.push(made);
  return made;
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

export const originalFetch = nativeFetch;

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

// Hands the stand-in above to every instance of the Envoy entry either suite loads: the setter
// keeps it on `globalThis`, since each `import("./envoy.ts?<row>")` is a module of its own. The
// import cannot be static, as the entry reads `@oh-my-pi/pi-coding-agent` and so must load below
// the stand-ins for it; it carries a query of its own so no row's instance is this one; and it
// sits below the `original*` captures because loading the entry sets `PATH`, `DISPATCH_HOST` and
// `ENVOY_CONFIG_FILE`, as every row's own load does, which `afterEach` puts back to what was
// captured. Loading it here changes those three names whether or not a row of either suite runs,
// so the capture below puts them back as soon as the entry is loaded; every row's own load changes
// them again.
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
  // What a test's Dispatch answers taught the process about its keyed layer is per process, so a
  // later suite in the same run (envoy-client's) would read it for the same origin.
  forgetKeyedLayerMemory();
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
  natsState.connectedNames.length = 0;
  natsState.connects.length = 0;
  natsState.presented.length = 0;
  natsState.timeline.length = 0;
  natsState.closedNames.length = 0;
  natsState.connections.length = 0;
  // A test aborted at Bun's own timeout never runs its `finally`: a connect left parked here
  // would park every later test's connect for the rest of the process.
  natsState.heldConnect = undefined;
  natsState.published.length = 0;
  natsState.onPublish = undefined;
  natsState.failPublishes = 0;
  natsState.subscriptions.clear();
  natsState.controls.clear();
  registeredTopics.clear();
  relaySeq = 0;
  natsState.controlsByTopic.clear();
  natsState.failConnects = 0;
  natsState.failError = () => NatsError.errorForCode(ErrorCode.ConnectionRefused);
  natsState.closeError = undefined;
  natsState.connectionsClosed = false;
  natsState.failSubscribes = 0;
  natsState.realConnect = false;
  natsState.drainHangs = false;
  natsState.drainStarted = false;
  natsState.statusReaders.length = 0;
  // As for `heldConnect`: a parked flush left behind would park every later test's subscribe.
  natsState.heldFlush = undefined;
  natsState.flushes.length = 0;
  natsState.failFlushOnSubject = undefined;
  natsState.failFlushClosesConnection = false;
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
  beforeAll(() => setNatsConnectForTests(natsStandInConnect as unknown as typeof realConnect));
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

/** The stream id every registration answer and relay frame of these suites names. */
export const TEST_STREAM_ID = "stream-test";

/** A server's answer, typed as Dispatch and the listener type theirs: a client reads a body as
 *  JSON only where the answer says it is JSON (`DispatchClient`'s `#response`). */
export function response(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
    status: 200,
  });
}

/** The listener's bus-outage body under `status`: with 503 it is the answer `isBusUnanswered`
 *  reads as a bus outage, and with any other status it is not. */
export function busOutageAnswer(status: number): Response {
  return Response.json(
    { error: `${BUS_UNANSWERED_PREFIX}: bus: NATS is reconnecting` },
    { status }
  );
}

export function responseWithRegistration(
  input: string | URL | Request,
  init: RequestInit | undefined,
  fallback: unknown
): Response {
  const path = new URL(input.toString()).pathname;
  if (path !== "/v1/interests/subscribe") return response(fallback);
  const body = JSON.parse(init?.body?.toString() ?? "{}") as {
    readonly session_id: string;
    readonly dir: string;
    readonly topics: readonly string[];
  };
  return response({
    session_id: body.session_id,
    machine_id: "test",
    dir: body.dir,
    topics: body.topics,
    delivery: consumerDelivery(body.session_id, { streamID: TEST_STREAM_ID }),
  });
}

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
export async function until(
  condition: () => boolean,
  what: string,
  timeoutMs = 2_000
): Promise<void> {
  const deadline = performance.now() + timeoutMs;
  while (!condition()) {
    if (performance.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await settle();
  }
}
