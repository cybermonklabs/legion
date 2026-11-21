#!/usr/bin/env bun
/**
 * session-pod-process.ts — one session of a session pod in an OS process of its own, for the row
 * of `extensions/envoy.test.ts` that starts processes from a pod's session
 * (`ENVOY_SESSION_ROLE_SESSION`).
 *
 * Run it as `bun --preload <dir>/host-stub.ts session-pod-process.ts <session id> [spawn]`: the
 * preload (`scripts/role-takeover/host-stub.ts`) supplies the host package the extension imports.
 * It loads the real `extensions/envoy.ts` with the environment the process started with, hands it
 * the suite's stand-in host (`createStandInHost`), a NATS connection that takes every subscription
 * and delivers nothing, and a listener in `globalThis.fetch` that accepts every registration and
 * every role claim, then opens the session `<session id>` and lets its first pass settle. What it
 * prints is its `Report` (`test/session-pod-report.ts`): that session's role claims and the
 * extension's `envoy: ENVOY_SESSION_ROLE …` lines.
 *
 * With `spawn`, it then starts this script twice more, as an agent starts an `omp` inside the pod,
 * each for a session of its own: once with a `Bun.spawn` that names no `env`, which hands the child
 * the environment this process started with, and once with `env: process.env`, the live
 * environment Oh My Pi's shell is built from. Their reports are part of its own.
 */
import { roleTopic } from "@legion/contracts";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import type { PiApi } from "@legion/pi-shared/pi-types";
import { logger } from "@oh-my-pi/pi-utils";
import envoyExtension, { setNatsConnectForTests } from "../extensions/envoy";
import { REPORT_MARKER, type Report, runSessionPod } from "./session-pod-report";
import { createStandInHost, type EventHandler } from "./stand-in-host";

const [sessionID = "", mode] = process.argv.slice(2);
if (sessionID === "")
  throw new Error("session-pod-process.ts needs the id of the session it opens");

const lines: string[] = [];
logger.registerLogSink((entry) => {
  if (entry.message.startsWith("envoy: ENVOY_SESSION_ROLE ")) lines.push(entry.message);
});

/**
 * A NATS connection that takes every subscription and delivers nothing: what the pass needs to
 * open the session's relay subject, which a registration asks for, and nothing more. Each
 * iterator ends when its subscription or the connection does, as nats.js's own do.
 */
function quietConnection() {
  let closed = false;
  // Settles at the close, as nats.js's `closed()` does.
  const shut = Promise.withResolvers<void>();
  const waiting: (() => void)[] = [];
  const wake = () => {
    if (closed) shut.resolve();
    for (const resolve of waiting.splice(0)) resolve();
  };
  const silentUntil = (done: () => boolean) => ({
    [Symbol.asyncIterator]: () => ({
      next: async (): Promise<IteratorResult<never>> => {
        while (!done()) {
          const { promise, resolve } = Promise.withResolvers<void>();
          waiting.push(resolve);
          await promise;
        }
        return { done: true, value: undefined };
      },
    }),
  });
  return {
    isClosed: () => closed,
    closed: () => shut.promise,
    close: async () => {
      closed = true;
      wake();
    },
    drain: async () => {
      closed = true;
      wake();
    },
    status: () => silentUntil(() => closed),
    flush: async () => undefined,
    publish: () => undefined,
    subscribe: () => {
      let active = true;
      return {
        unsubscribe: () => {
          active = false;
          wake();
        },
        ...silentUntil(() => !active || closed),
      };
    },
  };
}

setNatsConnectForTests((async () => quietConnection()) as unknown as Parameters<
  typeof setNatsConnectForTests
>[0]);

const claims: { role: string; session_id: string }[] = [];
globalThis.fetch = (async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
  const { pathname } = new URL(input instanceof Request ? input.url : input.toString());
  const body = typeof init?.body === "string" ? JSON.parse(init.body) : {};
  if (pathname === "/v1/interests/subscribe") {
    return Response.json({
      session_id: body.session_id,
      machine_id: "test",
      dir: body.dir,
      topics: body.topics,
      delivery: consumerDelivery(body.session_id),
    });
  }
  if (pathname === "/v1/roles/set") {
    claims.push({ role: body.role, session_id: body.session_id });
    return Response.json({
      session_id: body.session_id,
      machine_id: "test",
      dir: "/tmp/session-pod-process",
      topics: [roleTopic(body.role)],
    });
  }
  if (pathname.startsWith("/v1/roles/")) {
    return Response.json({ error: "no holder" }, { status: 404 });
  }
  return Response.json({});
}) as typeof fetch;

const { entries, handlers, pi } = createStandInHost();
const context = {
  cwd: "/tmp/session-pod-process",
  hasUI: true,
  agent: { kind: "main" as const },
  sessionManager: {
    getSessionId: () => sessionID,
    getSessionName: () => undefined,
    getBranch: (): readonly unknown[] => entries,
    getEntries: (): readonly unknown[] => entries,
    getSessionFile: () => undefined,
    ensureOnDisk: async (): Promise<void> => undefined,
  },
  isIdle: () => true,
  setInterval: () => undefined,
  setTimeout: () => undefined,
  ui: {
    notify: () => undefined,
    onTerminalInput: () => () => undefined,
    getEditorText: () => "",
  },
};

// The stand-in host is a subset of the real `PiApi`: the extension reads only what the suite's
// host supplies, and the cast is that same narrowing.
await envoyExtension(pi as unknown as PiApi);
const start: EventHandler | undefined = handlers.get("session_start");
if (start === undefined) throw new Error("the extension registered no session_start handler");
await start({}, context);

/**
 * This script for `session`, started the way `spawn` names, and what it reported. A child that
 * hangs is killed at 20 s, before the test's own bound on this process, so none is left running
 * and its stderr comes back in the error.
 */
async function child(
  spawn: "no env" | "process.env",
  session: string
): Promise<Report & { readonly spawn: string }> {
  const report = await runSessionPod({
    session,
    timeoutMs: 20_000,
    ...(spawn === "process.env" ? { env: process.env } : {}),
  });
  return { spawn, ...report };
}

// The two children share nothing, so they start together; the report keeps their order.
const children =
  mode === "spawn"
    ? await Promise.all([
        child("no env", `${sessionID}_no_env`),
        child("process.env", `${sessionID}_process_env`),
      ])
    : undefined;
const report: Report = {
  session: sessionID,
  claims,
  lines,
  ...(children === undefined ? {} : { children }),
};
console.log(`${REPORT_MARKER}${JSON.stringify(report)}`);
// The extension's own timers would keep the process up; its report is all it is for.
process.exit(0);
