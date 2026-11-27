// An Oh My Pi session whose inbound is the listener's relay socket (`ENVOY_INBOUND=listener`): the
// real extension, in a process of its own (`test/relay-session-process.ts`), opens the socket for
// its session with that session's broker token, takes its relay subject on it, and opens no live
// view, since the socket carries no agent stream and refuses that subject
// (`subject_not_granted`); a session whose own relay subject the listener refuses is told why; and
// a claim's task frame on the socket, and each reopen of it, reach the Legion entry's taker as
// they do over NATS. The socket and the broker are one stand-in
// (`@legion/envoy-client/test/relay-socket-stand-in`), which this process runs and reads.
import { expect, test } from "bun:test";
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { TaskHeader } from "@legion/envoy-client/relay-frame";
import {
  standInDeliverSubject,
  startRelaySocketStandIn,
} from "@legion/envoy-client/test/relay-socket-stand-in";
import type { EnvoyTaskSignal } from "@legion/pi-shared/interface";
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
    // The fixture's hook removes HOME after the row, within Bun's 5 s bound on a hook: the session
    // process must leave no transpiler cache there (`runReportingProcess`), which held about 360
    // files and outlasted that bound on a loaded box.
    const left = readdirSync(home, { recursive: true });
    expect(left.length).toBeLessThan(20);
  } finally {
    server.stop();
  }
}, 60_000);

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
      },
      timeoutMs: 45_000,
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
}, 60_000);

// A claim's task value rides the session's relay subject as a task frame, which the socket carries
// as it carries every message on that subject (a `msg` whose headers are the frame's): the frame
// reaches the Legion entry's taker as a value and is answered nothing, since it is no message of
// the session's consumer. A reopen of the socket takes the subject again on the new socket and
// begins a new epoch, whose first registration answer tells the taker to read the value again, as
// a NATS reconnect does: a task frame relayed while the socket was closed is lost.
test("a task frame on the relay socket reaches the Legion entry's taker unanswered, and a reopen of the socket tells it a new epoch", async () => {
  const server = startRelaySocketStandIn();
  try {
    const home = tempDir();
    const tokenFile = join(home, "token");
    writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
    const signalFile = join(home, "task-signals.jsonl");
    const reporting = runRelaySession({
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
      },
      timeoutMs: 45_000,
      taskSignals: { file: signalFile, count: 3 },
    });
    // The process's failure ends the waits below rather than leaving them to their bound.
    let failed: unknown;
    reporting.catch((error: unknown) => {
      failed = error;
    });
    // The signals the session's taker has taken so far, read from the file the process appends
    // each to as it comes, once there are `count` of them. The taker runs in the session's own
    // process, so the file is polled: no event of it reaches this one.
    const signalsTaken = async (count: number, what: string): Promise<EnvoyTaskSignal[]> => {
      const ends = Date.now() + 20_000;
      for (;;) {
        const taken: EnvoyTaskSignal[] = existsSync(signalFile)
          ? readFileSync(signalFile, "utf8")
              .split("\n")
              .filter((line) => line !== "")
              .map((line) => JSON.parse(line))
          : [];
        if (taken.length >= count) return taken;
        if (failed !== undefined) throw failed;
        if (Date.now() > ends) {
          throw new Error(
            `${what} never reached the taker; it took ${JSON.stringify(taken)}, the stand-in granted ${JSON.stringify(server.subs)} and read ${JSON.stringify(server.receipts)}`
          );
        }
        await Bun.sleep(25);
      }
    };
    const epoch: EnvoyTaskSignal = { kind: "epoch", session: SESSION };
    expect(await signalsTaken(1, "the first epoch")).toEqual([epoch]);

    const claim = "legion-legion-legion-208-implementer";
    const bucketEpoch = "2026-11-13T16:21:44.123456789Z";
    const reply = server.deliver(SESSION, {
      headers: {
        [TaskHeader.claim]: [claim],
        [TaskHeader.epoch]: [bucketEpoch],
        [TaskHeader.revision]: ["12"],
        [TaskHeader.op]: ["put"],
      },
      data: JSON.stringify({ task: 4 }),
    });
    if (reply === undefined) {
      throw new Error("the stand-in holds no relay subscription of the session");
    }
    const value: EnvoyTaskSignal = {
      kind: "value",
      session: SESSION,
      value: { claim, epoch: bucketEpoch, revision: 12, op: "put", task: 4 },
    };
    expect(await signalsTaken(2, "the task frame")).toEqual([epoch, value]);

    // The listener is stopping: the socket opens again at once and takes the subject again. The
    // session sends that sub before its new epoch, which its registration's answer can tell before
    // the stand-in has read the sub, so the test waits for the stand-in to read it.
    server.close(1012, "listener_stopping");
    const report = await reporting;
    expect(report.signals).toEqual([epoch, value, epoch]);
    const subsEnd = Date.now() + 10_000;
    while (server.subs.length < 2 && Date.now() < subsEnd) await Bun.sleep(25);
    expect(server.upgrades.filter((upgrade) => upgrade.status === 101)).toHaveLength(2);
    expect(server.subs).toEqual([
      { session: SESSION, subject: standInDeliverSubject(SESSION) },
      { session: SESSION, subject: standInDeliverSubject(SESSION) },
    ]);
    // The process has exited, so it sends nothing more: the task frame's reply was never answered.
    expect(server.receipts.filter((receipt) => receipt.reply === reply)).toEqual([]);
    expect(server.refusedPublishes).toEqual([]);
  } finally {
    server.stop();
  }
}, 60_000);
