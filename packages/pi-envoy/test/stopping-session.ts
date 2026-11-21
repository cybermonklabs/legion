#!/usr/bin/env bun
/**
 * stopping-session.ts — one Oh My Pi Envoy extension instance in an OS process of its own, for
 * `extensions/stopping-session.test.ts`, which stops it with SIGTERM as a Legion pane's shim
 * stops Oh My Pi.
 *
 * It loads the real `extensions/envoy.ts` against the stand-in host the package's suite is driven
 * against (`createStandInHost`). The listener is the test's own, over HTTP at `ENVOY_URL`, and the
 * NATS connection is one that carries nothing, since no listener call rides it.
 * `@oh-my-pi/pi-utils` is the module Oh My Pi hands its plugins as its own, and loading it
 * installs Oh My Pi's signal handlers (`postmortem`): the SIGTERM the test sends runs the exit
 * callbacks registered there, waits for them, and ends the process with status 143, as it ends
 * Oh My Pi.
 *
 * Run it as `bun --preload ../scripts/role-takeover/host-stub.ts stopping-session.ts`: the preload
 * supplies the host package the extension imports.
 *
 * The environment says what the process does:
 *  - `STOPPING_SESSION_ID`: the session the host has open.
 *  - `STOPPING_SESSION_MODE`: `rpc`, Oh My Pi's RPC mode, whose exit disposes no session, so a
 *    SIGTERM emits no `session_shutdown`; or `interactive`, whose exit disposes the session from
 *    an exit callback of its own (Oh My Pi's `session-teardown`), which emits `session_shutdown`
 *    beside every other exit callback.
 *  - `STOPPING_SESSION_ROLE`: a role the session claims with `envoy_role_set` once it started.
 *  - `STOPPING_SESSION_TRANSCRIPT`: the transcript the session carries, as the JSON array of its
 *    entries, which a relaunch of the same session reads.
 *
 * Once the session has started, and claimed its role, it writes one JSON line to stdout: its
 * transcript entries, and what the role tool answered. Then it waits for its signal.
 */
import type { PiApi } from "@legion/pi-shared/pi-types";
import { postmortem } from "@oh-my-pi/pi-utils";
import type { connect } from "nats";
import envoyExtension, { setNatsConnectForTests } from "../extensions/envoy";
import { type BranchEntry, createStandInHost, type EventHandler } from "./stand-in-host";

const {
  STOPPING_SESSION_ID = "",
  STOPPING_SESSION_MODE = "rpc",
  STOPPING_SESSION_ROLE = "",
  STOPPING_SESSION_TRANSCRIPT = "[]",
} = process.env;

/** A NATS connection that carries nothing: every subscription and the status feed wait until
 *  the connection is closed, drained or unsubscribed. */
function quietConnection() {
  const ended = Promise.withResolvers<void>();
  let closed = false;
  const close = async (): Promise<void> => {
    closed = true;
    ended.resolve();
  };
  const endsWith = (until: Promise<void>) => ({
    [Symbol.asyncIterator]: () => ({
      next: async (): Promise<IteratorResult<never>> => {
        await until;
        return { done: true, value: undefined };
      },
    }),
  });
  return {
    isClosed: () => closed,
    close,
    drain: close,
    status: () => endsWith(ended.promise),
    flush: async (): Promise<void> => undefined,
    publish: (): void => undefined,
    subscribe: () => {
      const unsubscribed = Promise.withResolvers<void>();
      return {
        unsubscribe: () => unsubscribed.resolve(),
        ...endsWith(Promise.race([unsubscribed.promise, ended.promise])),
      };
    },
  };
}
setNatsConnectForTests((async () => quietConnection()) as unknown as typeof connect);

const { entries, handlers, pi, tools } = createStandInHost();
entries.push(...(JSON.parse(STOPPING_SESSION_TRANSCRIPT) as BranchEntry[]));
const notices: string[] = [];

// One context, as the host has one session manager. The heartbeat is never armed: the test reads
// the listener calls of the start, the claim and the stop, and no tick of its own interleaves.
const context = {
  cwd: process.cwd(),
  hasUI: true,
  agent: { kind: "main" as const },
  sessionManager: {
    getSessionId: () => STOPPING_SESSION_ID,
    getSessionName: () => undefined,
    getBranch: (): readonly unknown[] => entries,
    getEntries: (): readonly unknown[] => entries,
    getSessionFile: () => undefined,
    ensureOnDisk: async (): Promise<void> => undefined,
  },
  isIdle: () => true,
  setInterval: (): void => undefined,
  setTimeout: (callback: () => void): void => {
    setTimeout(callback, 0);
  },
  ui: {
    notify: (message: string, level: string) => {
      notices.push(`${level}: ${message}`);
    },
    onTerminalInput: () => () => undefined,
    getEditorText: () => "",
  },
};

function handlerFor(event: string): EventHandler {
  const handler = handlers.get(event);
  if (handler === undefined) throw new Error(`the extension registered no ${event} handler`);
  return handler;
}

// The stand-in host is a subset of the real `PiApi`: the extension reads only what the suite's
// host supplies, and the cast is that same narrowing.
envoyExtension(pi as unknown as PiApi);
await handlerFor("session_start")({}, context);

let claim: string | undefined;
if (STOPPING_SESSION_ROLE !== "") {
  const roleTool = tools.find((tool) => tool.name === "envoy_role_set");
  if (roleTool === undefined) throw new Error("the extension registered no envoy_role_set");
  const result = await roleTool.execute(
    "stopping-session",
    { role: STOPPING_SESSION_ROLE },
    undefined,
    undefined,
    context
  );
  claim = result.content.map((part) => part.text).join("\n");
}

// Oh My Pi's interactive mode disposes its session from an exit callback of its own, and the
// dispose emits `session_shutdown`: the same callback list the SIGTERM runs, so both ends of the
// extension's stop are reached by the one signal.
if (STOPPING_SESSION_MODE === "interactive") {
  postmortem.register("session-teardown", async () => {
    await handlerFor("session_shutdown")({}, context);
  });
}

process.stdout.write(`${JSON.stringify({ ready: true, entries, claim, notices })}\n`);
// Nothing else keeps the process alive until its signal.
setInterval(() => undefined, 2 ** 30);
