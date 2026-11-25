// An Oh My Pi session whose inbound is the listener's relay socket (`ENVOY_INBOUND=listener`): the
// real extension, in a process of its own (`test/relay-session-process.ts`), opens the socket for
// its session with that session's broker token, takes its relay subject on it, and opens no live
// view, since the socket carries no agent stream and refuses that subject
// (`subject_not_granted`); and a session whose own relay subject the listener refuses is told why.
// The socket and the broker are one stand-in (`@legion/envoy-client/test/relay-socket-stand-in`),
// which this process runs and reads.
import { expect, test } from "bun:test";
import { readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  standInDeliverSubject,
  startRelaySocketStandIn,
} from "@legion/envoy-client/test/relay-socket-stand-in";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { timeLeft } from "@legion/test-fixtures/wait";
import { runRelaySession } from "../test/relay-session-report";

const tempDir = temporaryDirectories("pi-envoy-relay-socket");
const SESSION = "ses_relay_socket";

// What can hang in each row is the session process, which loads the real extension and opens its
// relay socket. It is killed at the row's deadline (`timeLeft()`, `@legion/test-fixtures/wait`),
// inside the command's guard, so a run that never reports fails the row naming that process.
// The refusal row's process waits for its text through the shared wait, at a deadline the row
// hands it (`LEGION_PROOF_DEADLINE_MS`, since it is no `bun test`) `REPORT_MS` before that kill,
// so the wait's own failure reaches the row's error before the kill would end the process.
const REPORT_MS = 1_000;
// Each session's pass deadline: an hour, past the command's guard, as envoy.test.ts's
// `UNRACED_PASS_DEADLINE_MS` is. With none named the extension's pass deadline is 90 s, and the
// relay socket's dial is bounded under it (`startTradeBoundMs`, src/start-bound.ts: 80 s), so on a
// slow host either would end the session's start before the row's deadline and decide the row in
// the guard's place.
const PASS_DEADLINE_MS = "3600000";

test("a relay socket session takes its relay subject on the socket and no agent stream", async () => {
  const server = startRelaySocketStandIn();
  try {
    const home = tempDir();
    const tokenFile = join(home, "token");
    writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
    // Whole milliseconds: `Bun.spawn` takes an integer `timeout`.
    const timeoutMs = Math.floor(timeLeft());
    const report = await runRelaySession({
      session: SESSION,
      cwd: import.meta.dir,
      // Only what the session reads: no NATS server, no Dispatch, no envoy.json of the developer's.
      env: {
        PATH: process.env.PATH,
        TMPDIR: process.env.TMPDIR,
        HOME: home,
        ENVOY_CONFIG_FILE: join(home, "envoy.json"),
        ENVOY_URL: server.url,
        ENVOY_INBOUND: "listener",
        AGENT_SECRETS_URL: server.url,
        AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
        AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
        ENVOY_PASS_DEADLINE_MS: PASS_DEADLINE_MS,
      },
      timeoutMs,
    });
    const opened = server.upgrades.filter((upgrade) => upgrade.status === 101);
    if (opened.length === 0) {
      throw new Error(
        `the session opened no relay socket; upgrades ${JSON.stringify(server.upgrades)}, lines ${JSON.stringify(report.lines)}`
      );
    }
    expect(opened.map((upgrade) => upgrade.session)).toEqual([SESSION]);
    expect(server.minted.map((token) => token.session)).toEqual([SESSION]);
    if (server.refusedSubs.length > 0) {
      throw new Error(
        `the stand-in refused these subscribes: ${server.refusedSubs.map((sub) => sub.subject).join(", ")}`
      );
    }
    expect(server.subs).toEqual([{ session: SESSION, subject: standInDeliverSubject(SESSION) }]);
    expect(report.notices.join("\n")).toContain(
      "Envoy's inbound messages come through the listener's relay socket (ENVOY_INBOUND is listener), which carries no live view: the Agents page shows nothing of this session's turns"
    );
    // The fixture's hook (`temporaryDirectories`' `afterEach`) removes HOME after the row, under
    // the command's guard (package.json). The session process must leave no transpiler cache there
    // (`runReportingProcess`): that cache held about 360 files, which the hook would otherwise
    // remove.
    const left = readdirSync(home, { recursive: true });
    expect(left.length).toBeLessThan(20);
  } finally {
    server.stop();
  }
});

// The wire lets the listener answer any `sub` `subject_not_granted`. #4658 answers so only a
// subject other than the session's own, and closes the socket 1013 when NATS refuses a subscribe;
// a listener that refused the session's own subject would leave a dead subscription unless it
// ends with that error, as a nats.js one the server refused does. So the session is told why it
// receives nothing, rather than told it holds no relay subject with no reason.
test("a relay subject the listener refuses is told naming why", async () => {
  const server = startRelaySocketStandIn();
  server.refuseOwnSubject = true;
  try {
    const home = tempDir();
    const tokenFile = join(home, "token");
    writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
    const refusal = `the listener's relay socket refused the subject ${standInDeliverSubject(SESSION)} (subject_not_granted)`;
    const timeoutMs = Math.floor(timeLeft());
    const report = await runRelaySession({
      session: SESSION,
      cwd: import.meta.dir,
      env: {
        PATH: process.env.PATH,
        TMPDIR: process.env.TMPDIR,
        HOME: home,
        ENVOY_CONFIG_FILE: join(home, "envoy.json"),
        ENVOY_URL: server.url,
        ENVOY_INBOUND: "listener",
        AGENT_SECRETS_URL: server.url,
        AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
        AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
        ENVOY_PASS_DEADLINE_MS: PASS_DEADLINE_MS,
        LEGION_PROOF_DEADLINE_MS: String(Date.now() + timeoutMs - REPORT_MS),
      },
      timeoutMs,
      waitFor: "receives nothing",
    });
    expect(server.refusedSubs[0]).toEqual({
      session: SESSION,
      subject: standInDeliverSubject(SESSION),
      code: "subject_not_granted",
    });
    const told = report.lines.filter((line) => line.includes("receives nothing"));
    if (told.length === 0) {
      throw new Error(`the session was never told it receives nothing: ${JSON.stringify(report)}`);
    }
    expect(told[0]).toContain(refusal);
  } finally {
    server.stop();
  }
});
