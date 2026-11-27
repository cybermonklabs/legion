#!/usr/bin/env bun
/**
 * omp-client.ts — one Oh My Pi Envoy extension instance in an OS process of its own, for
 * `scripts/smoke-role-takeover.sh` and `scripts/e2e/nats-callout-sessions.sh`.
 *
 * It loads the real `extensions/envoy.ts` from this checkout and hands it the stand-in host the
 * package's own suite is driven against (`createStandInHost` in `test/stand-in-host.ts`, which
 * `extensions/envoy.test.ts` wraps as `createPi`): one session manager whose id and branch the
 * driver moves, `pi.appendEntry` writing into that branch, and
 * `context.setInterval` keeping the heartbeat callback rather than arming a timer on it, so a
 * pass runs when the smoke asks for one (`{"op":"heartbeat"}`) and no tick of its own interleaves
 * with what the smoke drives. Nothing else is faked: the extension dials the driver's natsd, and
 * every listener call goes over HTTP to the listener `envoy.url` names in the `ENVOY_CONFIG_FILE`
 * the driver writes (`smoke-role-takeover.sh`'s recording proxy, the stack's own listener in
 * `nats-callout-sessions.sh`). One process is one client instance, which is what the listener's
 * rule turns on (R5, R6 of the role takeover's spec).
 *
 * Its input ending is the host going away: the extension's `session_shutdown` runs (it deregisters
 * the session and drains NATS), bounded at 10 s, and the process exits, writing its notices.
 *
 * Run it as `bun --preload <dir>/host-stub.ts <dir>/omp-client.ts`: the preload supplies the host
 * package the extension imports.
 *
 * Protocol: one JSON object per line on stdin, one answer per line on stdout, matched by `id`.
 *
 *   {"id":"1","op":"start","session":"ses_a","roles":["reviewer"]}
 *   {"id":"2","op":"tool","name":"envoy_role_set","args":{"role":"reviewer"}}
 *   {"id":"3","op":"switch","session":"ses_b","reason":"fork"}
 *   {"id":"4","op":"heartbeat"}
 *   {"id":"5","op":"deliveries"}
 *   {"id":"6","op":"entries"}
 *   {"id":"7","op":"start","session":"ses_a","branch":[...]}
 *
 * `roles` replaces the session's branch with one `envoy-role-claim` entry per element (a string
 * is a claim, null a release), which is what a resumed or forked transcript carries; omitted, the
 * branch carries on as it is. `branch` on `start` replaces it with these entries as they are,
 * which is what a resumed transcript holds in full: what a killed process's `entries` answered,
 * its follow stamps among them, so the extension restores what that process had followed.
 * `heartbeat` runs the pass the extension's own interval would have
 * run: it re-registers the session, which is how a holder says it still answers for the role
 * claims it made, and it re-asserts the role that session holds. It answers as soon as the pass
 * is under way, as a timer tick does; the smoke reads the pass's own calls at the proxy.
 * `deliveries` answers every message the extension handed the host, in order: each card it showed
 * (`pi.sendMessage`) and each user turn it started (`pi.sendUserMessage`), the two ways an Envoy
 * delivery reaches the session. `entries` answers the session's branch as the extension wrote it,
 * each `envoy-role-claim` record among them: a claim this process landed names its own client
 * instance id, which a caller compares with the one the listener's role lookup names.
 */
import { appendFileSync } from "node:fs";
import type { PiApi } from "@legion/pi-shared/pi-types";
import envoyExtension from "../../extensions/envoy";
import { type BranchEntry, createStandInHost, type EventHandler } from "../../test/stand-in-host";

const ROLE_CLAIM_ENTRY = "envoy-role-claim";

interface Command {
  readonly id?: string;
  readonly op?: string;
  readonly session?: string;
  readonly reason?: string;
  readonly roles?: readonly (string | null)[];
  readonly branch?: readonly unknown[];
  readonly name?: string;
  readonly args?: Record<string, unknown>;
}

const { ROLE_TAKEOVER_CWD, ROLE_TAKEOVER_CLIENT_LOG, ENVOY_HEARTBEAT_MS } = process.env;
const cwd = ROLE_TAKEOVER_CWD ?? process.cwd();
// The interval the extension arms its heartbeat on (`settings.heartbeatMs`), so the callback kept
// below is that one and not another timer the extension arms on a period of its own.
const heartbeatMs = Number(ENVOY_HEARTBEAT_MS);

let sessionID = "";
/** The extension's heartbeat callback, kept by `context.setInterval` and run by `heartbeat`. */
let heartbeatPass: (() => void) | undefined;
// One host, shared with the package's own suite: its `entries` are this session's branch, which
// the extension's `pi.appendEntry` writes into.
const { entries, handlers, messages, pi, tools, userMessages } = createStandInHost();
const notices: string[] = [];

const sessionManager = {
  getSessionId: () => sessionID,
  getSessionName: () => undefined,
  getBranch: (): readonly unknown[] => entries,
  getEntries: (): readonly unknown[] => entries,
  getSessionFile: () => undefined,
  ensureOnDisk: async (): Promise<void> => undefined,
};

// One context, as the host has one session manager: every handler reads the id and branch as
// they stand when it runs, which is what makes a move mid-pass observable at all.
const context = {
  cwd,
  hasUI: true,
  agent: { kind: "main" as const },
  sessionManager,
  isIdle: () => true,
  // The real host says whether a message waits in its queue (`PiApi` context's
  // `hasPendingMessages`); with neither, the extension never counts the host idle and holds every
  // transcript notice (an outage, a refused topic, a gap) for good, so this client could see none.
  hasPendingMessages: () => false,
  // The extension arms its heartbeat here. The smoke drives every pass itself, so the callback is
  // kept for `{"op":"heartbeat"}` rather than armed on a timer; a timer on any other period is
  // dropped, as every timer here was.
  setInterval: (callback: () => void, intervalMs: number) => {
    if (intervalMs === heartbeatMs) heartbeatPass = callback;
  },
  setTimeout: (callback: () => void) => {
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

// A host writes each card it is handed and then tells the extension so (`message_end`), which is
// what settles the delivery and answers the listener `done`, so the session's consumer acks it; the
// suite's host records the card alone, so this one writes it too, as the package's tests do.
const recordCard = pi.sendMessage;
// The host's own record of the API, through which the wrapper below replaces sendMessage.
const host: { sendMessage: typeof recordCard } = pi;
host.sendMessage = (message, options) => {
  recordCard(message, options);
  queueMicrotask(() => {
    void handlers.get("message_end")?.(
      { message: { role: "custom", ...message, timestamp: Date.now() } },
      context
    );
  });
};

// The stand-in host is a subset of the real `PiApi`: the extension reads only what the suite's
// host supplies, and the cast is that same narrowing.
await envoyExtension(pi as unknown as PiApi);

/** Replaces the branch with the transcript a resumed, forked or fresh session carries. */
function setBranch(roles: readonly (string | null)[] | undefined): void {
  if (roles === undefined) return;
  entries.length = 0;
  for (const role of roles) {
    entries.push({ type: "custom", customType: ROLE_CLAIM_ENTRY, data: { role } });
  }
}

function handlerFor(event: string): EventHandler {
  const handler = handlers.get(event);
  if (handler === undefined) throw new Error(`the extension registered no ${event} handler`);
  return handler;
}

async function run(command: Command): Promise<Record<string, unknown>> {
  switch (command.op) {
    case "start": {
      sessionID = command.session ?? "";
      if (command.branch !== undefined) {
        entries.length = 0;
        // As the killed process's `entries` answered them: the host's own entry shape.
        entries.push(...(command.branch as readonly BranchEntry[]));
      } else {
        setBranch(command.roles ?? []);
      }
      await handlerFor("session_start")({}, context);
      return { session: sessionID };
    }
    case "switch": {
      await handlerFor("session_before_switch")({ reason: command.reason }, context);
      sessionID = command.session ?? sessionID;
      setBranch(command.roles);
      await handlerFor("session_switch")({ reason: command.reason }, context);
      return { session: sessionID };
    }
    case "tree": {
      sessionID = command.session ?? sessionID;
      setBranch(command.roles);
      await handlerFor("session_tree")({}, context);
      return { session: sessionID };
    }
    case "tool": {
      const tool = tools.find((candidate) => candidate.name === command.name);
      if (tool === undefined) throw new Error(`no tool named ${command.name}`);
      return {
        result: await tool.execute("smoke", command.args ?? {}, undefined, undefined, context),
      };
    }
    case "shutdown":
      await handlerFor("session_shutdown")({}, context);
      return { session: sessionID };
    case "heartbeat": {
      // One tick of the interval the extension armed: the pass runs behind it, as it does behind
      // a timer tick, and what it sent is read at the smoke's proxy.
      if (heartbeatPass === undefined) throw new Error("the extension armed no heartbeat yet");
      heartbeatPass();
      return { session: sessionID };
    }
    case "deliveries":
      return {
        deliveries: [...messages],
        turns: userMessages.map((turn) => turn.content),
      };
    case "entries":
      return { entries: [...entries] };
    default:
      throw new Error(`unknown op ${command.op}`);
  }
}

function answer(line: Record<string, unknown>): void {
  process.stdout.write(`${JSON.stringify(line)}\n`);
}

if (ROLE_TAKEOVER_CLIENT_LOG !== undefined) {
  // Every notice the pane would show, for a failed run's evidence.
  process.on("exit", () => {
    appendFileSync(ROLE_TAKEOVER_CLIENT_LOG, `${notices.join("\n")}\n`);
  });
}

answer({ ready: true, pid: process.pid });
for await (const line of console) {
  const text = line.trim();
  if (text === "") continue;
  const command = JSON.parse(text) as Command;
  const id = command.id ?? "";
  try {
    answer({ id, ok: true, ...(await run(command)) });
  } catch (error) {
    answer({ id, ok: false, error: error instanceof Error ? error.message : String(error) });
  }
}
await Promise.race([handlerFor("session_shutdown")({}, context), Bun.sleep(10_000)]).catch(
  () => undefined
);
process.exit(0);
