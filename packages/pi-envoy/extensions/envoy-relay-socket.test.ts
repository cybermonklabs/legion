// An Oh My Pi session whose inbound is the listener's relay socket (`ENVOY_INBOUND=listener`): the
// real extension, in a process of its own (`test/relay-session-process.ts`), opens the socket for
// its session with that session's broker token, takes its relay subject on it, and opens no live
// view, since the socket carries no agent stream and refuses that subject
// (`subject_not_granted`). The socket and the broker are one stand-in
// (`@legion/envoy-client/test/relay-socket-stand-in`), which this process runs and reads.
import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  standInDeliverSubject,
  startRelaySocketStandIn,
} from "@legion/envoy-client/test/relay-socket-stand-in";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runRelaySession } from "../test/relay-session-report";

const tempDir = temporaryDirectories("pi-envoy-relay-socket");
const SESSION = "ses_relay_socket";

test("a relay socket session takes its relay subject on the socket and no agent stream", async () => {
  const server = startRelaySocketStandIn();
  try {
    const home = tempDir();
    const tokenFile = join(home, "token");
    writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
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
      },
      timeoutMs: 30_000,
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
  } finally {
    server.stop();
  }
}, 60_000);
