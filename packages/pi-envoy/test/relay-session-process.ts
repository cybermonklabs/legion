#!/usr/bin/env bun
/**
 * relay-session-process.ts — one Oh My Pi session whose inbound is the listener's relay socket
 * (`ENVOY_INBOUND=listener`), in an OS process of its own, for the rows of
 * `extensions/envoy-relay-socket.test.ts` (`runRelaySession`, `test/relay-session-report.ts`).
 *
 * Run it as `bun --preload <dir>/host-stub.ts relay-session-process.ts <session id> [wait for]`
 * with an environment that names the relay socket's listener and the broker it trades at, both
 * the row's stand-in (`@legion/envoy-client/test/relay-socket-stand-in`). It loads the real
 * `extensions/envoy.ts`, hands it the suite's stand-in host (`createStandInHost`), answers the
 * listener's HTTP routes in `globalThis.fetch` (`standInListenerFetch`: every registration
 * accepted, no role held) and hands the broker's token exchange to the real fetch; then it opens
 * the session `<session id>` and lets its first pass settle, and, given `<wait for>`, waits until
 * a notice or a line holds that text, for 20 s at most. The relay socket itself is Bun's own
 * WebSocket, to the stand-in. What it prints is its `RelaySessionReport`.
 */
import type { PiApi } from "@legion/pi-shared/pi-types";
import { logger } from "@oh-my-pi/pi-utils";
import envoyExtension from "../extensions/envoy";
import { RELAY_SESSION_REPORT, type RelaySessionReport } from "./relay-session-report";
import {
  createStandInHost,
  type EventHandler,
  processSessionContext,
  standInListenerFetch,
} from "./stand-in-host";

const [sessionID = "", waitFor] = process.argv.slice(2);
if (sessionID === "")
  throw new Error("relay-session-process.ts needs the id of the session it opens");

const lines: string[] = [];
logger.registerLogSink((entry) => {
  if (entry.message.startsWith("envoy:")) lines.push(entry.message);
});

const realFetch = globalThis.fetch;
globalThis.fetch = standInListenerFetch((pathname, _body, input, init) =>
  pathname === "/v1/oauth/token" ? realFetch(input, init) : undefined
);

const notices: string[] = [];
const { entries, handlers, pi } = createStandInHost();
const context = processSessionContext({
  sessionID,
  cwd: process.cwd(),
  entries,
  notify: (message) => void notices.push(message),
});

// The stand-in host is a subset of the real `PiApi`: the extension reads only what the suite's
// host supplies, and the cast is that same narrowing.
await envoyExtension(pi as unknown as PiApi);
const start: EventHandler | undefined = handlers.get("session_start");
if (start === undefined) throw new Error("the extension registered no session_start handler");
await start({}, context);

if (waitFor !== undefined) {
  const ends = Date.now() + 20_000;
  while (![...notices, ...lines].some((text) => text.includes(waitFor)) && Date.now() < ends) {
    await Bun.sleep(25);
  }
}

const report: RelaySessionReport = { notices, lines };
console.log(`${RELAY_SESSION_REPORT}${JSON.stringify(report)}`);
// The extension's own timers and its relay socket would keep the process up.
process.exit(0);
