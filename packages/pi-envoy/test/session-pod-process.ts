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
 * prints, as one line after `session-pod-report `, is that session's role claims and the
 * extension's `envoy: ENVOY_SESSION_ROLE …` lines.
 *
 * With `spawn`, it then starts this script twice more, as an agent starts an `omp` inside the pod,
 * each for a session of its own: once with a `Bun.spawn` that names no `env`, which hands the child
 * the environment this process started with, and once with `env: process.env`, the live
 * environment Oh My Pi's shell is built from. Their reports are part of its own.
 */
import { join } from "node:path";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import type { PiApi } from "@legion/pi-shared/pi-types";
import { logger } from "@oh-my-pi/pi-utils";
import envoyExtension, { setNatsConnectForTests } from "../extensions/envoy";
import { createStandInHost, type EventHandler } from "./stand-in-host";

const REPORT_MARKER = "session-pod-report ";

interface Report {
  readonly session: string;
  readonly claims: readonly { readonly role: string; readonly session_id: string }[];
  readonly lines: readonly string[];
  readonly children?: readonly (Report & { readonly spawn: string })[];
}

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
  const waiting: (() => void)[] = [];
  const wake = () => {
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
      topics: [`notifications.role.${body.role}`],
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
envoyExtension(pi as unknown as PiApi);
const start: EventHandler | undefined = handlers.get("session_start");
if (start === undefined) throw new Error("the extension registered no session_start handler");
await start({}, context);

/** This script for the session `child`, started the way `spawn` names, and what it reported. */
async function child(spawn: string, child: string): Promise<Report & { readonly spawn: string }> {
  const argv = [
    process.execPath,
    "--preload",
    join(import.meta.dir, "..", "scripts", "role-takeover", "host-stub.ts"),
    join(import.meta.dir, "session-pod-process.ts"),
    child,
  ];
  const started =
    spawn === "no env"
      ? Bun.spawn(argv, { stdout: "pipe", stderr: "pipe" })
      : Bun.spawn(argv, { env: process.env, stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(started.stdout).text(),
    new Response(started.stderr).text(),
    started.exited,
  ]);
  const line = stdout.split("\n").find((entry) => entry.startsWith(REPORT_MARKER));
  if (exitCode !== 0 || line === undefined) {
    throw new Error(
      `the ${spawn} child for ${child} reported nothing (exit ${exitCode}): ${stderr}`
    );
  }
  return { spawn, ...(JSON.parse(line.slice(REPORT_MARKER.length)) as Report) };
}

const report: Report = {
  session: sessionID,
  claims,
  lines,
  ...(mode === "spawn"
    ? {
        children: [
          await child("no env", `${sessionID}_no_env`),
          await child("process.env", `${sessionID}_process_env`),
        ],
      }
    : {}),
};
console.log(`${REPORT_MARKER}${JSON.stringify(report)}`);
// The extension's own timers would keep the process up; its report is all it is for.
process.exit(0);
