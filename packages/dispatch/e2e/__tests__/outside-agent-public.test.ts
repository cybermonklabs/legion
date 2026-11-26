/** The public leg of the outside agent's acceptance (`e2e/acceptance/outside-agent-public.sh`)
 *  against stand-ins of what it drives: an `omp` whose plugin install leaves the plugin's
 *  `dispatch` command, a `dispatch login --device` that prints a code and keeps a login, an approve
 *  command, a `tmux` that names the Oh My Pi session's directory and runs nothing, and an
 *  `outside-agent-probe.ts` beside the leg that answers each command as a scenario file says. The
 *  stand-in edge finds the session but answers its send to the peer 503, so P4 reads UNKNOWN at
 *  once and P8 does not run; no reach check, revoke or browser command is given.
 *
 *  Each row reads the leg's own judgments PASS where the stand-in edge refuses as it should, and,
 *  in the same row, not PASS where the stand-in crosses the rule the judgment reads. */

import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { type BoundName, rowLimit, runBounded } from "./bounded-subprocess";
import {
  copyLeg,
  type LegRun,
  publicLeg,
  rowsOf,
  script,
  textToolPath,
  verdictsOf,
} from "./outside-agent-fixture";
import { closedPort } from "./outside-path-reach-fixtures";

// On tmpfs: the afterEach that removes a row's directories has Bun's 5 s for a hook, and on the
// shared disk one removal of a row's ~30 entries was measured at up to 17.5 s (median 2 ms, 30
// trials, load ~1500), against at most 16 ms on /dev/shm. A row here holds at most 5 runs of about
// 40 entries each, so 8 MiB leaves room for 8 runs sharing the host.
const scratch = temporaryDirectories("outside-agent-public", {
  root: memoryRoot({ need: 8 * 2 ** 20 }),
});

interface PublicScenario {
  /** The answers of the trades the T5 window makes, in turn, the last one repeating: true for an
   *  answered trade, false for a refusal, null for an answer the leg cannot read. */
  readonly trades: readonly (boolean | null)[];
  /** The code the relay socket the T5 window holds closes with; it stays open for the whole hold
   *  the leg asks for where none is given, or where no trade is refused. It closes once the leg
   *  has made its first trade after the first refused one (the first false of `trades`), which the
   *  leg makes only after it has read the socket open at that refusal. No time stands for that
   *  read: a close a fixed 15 s after the socket opened came before a loaded leg's refusal, which
   *  follows its 10 s between trades and the start of its probe, and the leg then read no socket
   *  open at the refusal (dispatch://INFRA-3904). */
  readonly holdCode?: number;
  /** The relay socket closes the moment it opens, with `holdCode`, so it is closed well before
   *  the leg's first refused trade: the leg reads no socket open at the refusal, and a close it
   *  reads then says nothing of the removal. */
  readonly holdClosesAtOpen?: boolean;
  /** The probe's answers by command, where they differ from an edge that refuses as it should. */
  readonly foreignSend?: Record<string, unknown>;
  readonly approve?: Record<string, unknown>;
  readonly livePublish?: Record<string, unknown>;
  /** The rows the workflow's browser (OUTSIDE_T5_COMMAND) writes, in order, each
   *  `[row, got, verdict]`; with none, OUTSIDE_T5_COMMAND is unset. */
  readonly browserRows?: readonly (readonly [string, string, string])[];
}

/** The stand-in probe: a Bun process the leg starts for each command it runs, on its own clock,
 *  which no fake timer of this file reaches; it answers from scenario.json beside it. Its `trade`
 *  counts the trades the leg made, in trades.count, and its `hold` reads that count every 25 ms, a
 *  gap no verdict reads, until the trade it closes at: a write of another process raises no event
 *  the hold could await. Without a close, the hold keeps its socket open for the window the leg
 *  asks for, as the real probe does. */
const standInProbe = `
const dir = import.meta.dir;
const scenario = JSON.parse(await Bun.file(dir + "/scenario.json").text());
const [command, ...args] = Bun.argv.slice(2);
const self = process.env.OUTSIDE_PROBE_SESSION ?? "";
const print = (value) => process.stdout.write(JSON.stringify(value) + "\\n");
const refusal = (code) => ({ status: 403, code, detail: "refused" });
const counter = dir + "/trades.count";
const tradesMade = async () => ((await Bun.file(counter).exists()) ? Number(await Bun.file(counter).text()) : 0);
const firstRefusal = scenario.trades.indexOf(false);
const closes = scenario.holdCode !== undefined && firstRefusal !== -1;
switch (command) {
  case "whoami":
    print({ status: 200, code: null, detail: null, kind: "agent", owner: "outside-e2e@example.com", project: "EXAMPLE", scopes: [] });
    break;
  case "interest":
    print(refusal(args[0].startsWith("notifications.github.") ? "repository_not_in_project" : "topic_not_in_project"));
    break;
  case "publish":
    print(args[0].startsWith("agentstream.") ? (scenario.livePublish ?? refusal("topic_not_published")) : refusal("topic_not_published"));
    break;
  case "github-event":
    print({ status: 200, code: null, detail: null, event: "notifications.github.example-owner.example-repo.push", sockets: 1 });
    break;
  case "sessions":
    print({ status: 200, code: null, detail: null, sessions: ["example-peer"], at_dir: args[0] === undefined ? null : "omp-session-1" });
    break;
  case "relay-sub":
    print(args[0] === "notifications.envoy.deliver." + self ? { granted: true } : { granted: false, code: "subject_not_granted" });
    break;
  case "send":
    if (args[0].endsWith("-foreign")) print(scenario.foreignSend ?? { status: 404, code: null, detail: "no such session" });
    else if (args[0] === "example-peer") print({ status: 503, code: null, detail: "the stand-in edge serves no peer" });
    else print({ status: 200, code: null, detail: null });
    break;
  case "agent-token":
    print(refusal("OAUTH_SCOPE_REQUIRED"));
    break;
  case "approve":
    print(scenario.approve ?? refusal("OAUTH_SCOPE_REQUIRED"));
    break;
  case "model":
    print({ port: 1 });
    break;
  case "trade": {
    const done = await tradesMade();
    await Bun.write(counter, String(done + 1));
    const traded = scenario.trades[Math.min(done, scenario.trades.length - 1)];
    if (traded === null) print({ unknown: "the stand-in broker did not answer" });
    else print(traded ? { traded: true, expires_at: Date.now() + 300000 } : { traded: false, status: 403, detail: "refused" });
    break;
  }
  case "hold": {
    print({ opened_at: Date.now() });
    if (scenario.holdClosesAtOpen) {
      print({ closed_at: Date.now(), code: scenario.holdCode ?? 4401 });
      break;
    }
    const end = Date.now() + Number(args[0]) * 1000;
    if (closes) {
      while ((await tradesMade()) <= firstRefusal + 1 && Date.now() < end) await Bun.sleep(25);
      if ((await tradesMade()) > firstRefusal + 1) {
        print({ closed_at: Date.now(), code: scenario.holdCode });
        break;
      }
    }
    await Bun.sleep(Math.max(0, end - Date.now()));
    break;
  }
  default:
    print({ unknown: "the stand-in probe has no " + command });
    process.exit(2);
}
`;

/** One run of a copy of the public leg against the stand-ins, as `scenario` says. */
async function publicStandInRun(
  scenario: PublicScenario,
  bound: BoundName = "publicRun"
): Promise<LegRun> {
  const leg = copyLeg(scratch, publicLeg);
  const legDir = leg.slice(0, leg.lastIndexOf("/"));
  await Bun.write(join(legDir, "outside-agent-probe.ts"), standInProbe);
  await Bun.write(join(legDir, "scenario.json"), JSON.stringify(scenario));
  const bin = textToolPath(scratch);
  const bun = hostTool("bun");
  if (bun !== null) script(join(bin, "bun"), `#!/bin/sh\nexec '${bun}' "$@"\n`);
  const dispatchCommand = `#!/usr/bin/env bash
echo 'Open https://dispatch.example/device and enter the code ABCD-EFGH'
mkdir -p "$HOME/.config/legion"
(umask 077 && echo '{}' >"$HOME/.config/legion/device-login.json")
`;
  script(
    join(bin, "omp"),
    `#!/usr/bin/env bash
[ "$1 $2" = "plugin install" ] || exit 2
plugin="$HOME/.omp/plugins/node_modules/@sjawhar/pi-envoy"
mkdir -p "$plugin/bin"
echo '{"version":"0.0.0"}' >"$plugin/package.json"
cat >"$plugin/bin/dispatch" <<'SH'
${dispatchCommand}SH
chmod 755 "$plugin/bin/dispatch"
`
  );
  // The stand-in tmux runs nothing: it keeps the directory a session was started in, and shows it
  // as the pane, as Oh My Pi's TUI names its directory.
  const state = scratch("state");
  script(
    join(bin, "tmux"),
    `#!/usr/bin/env bash
[ "$1" = -S ] && shift 2
command=$1
shift
case $command in
  new-session)
    while [ "$#" -gt 0 ]; do
      case $1 in -c) printf '%s\\n' "$2" >'${state}/tmux-dir'; shift 2 ;; *) shift ;; esac
    done
    ;;
  capture-pane) cat '${state}/tmux-dir' 2>/dev/null ;;
esac
exit 0
`
  );
  const approve = script(join(scratch("inputs"), "approve"), '#!/bin/sh\necho "APPROVED $1"\n');
  const browserRows = scenario.browserRows ?? [];
  const rowsFile = join(state, "browser-rows.tsv");
  await Bun.write(
    rowsFile,
    browserRows
      .map(([name, got, verdict]) => `${name}\tthe browser's ${name}\t${got}\t${verdict}\n`)
      .join("")
  );
  const browser = script(
    join(bin, "t5-browser"),
    `#!/bin/sh\ncat '${rowsFile}' >"$OUTSIDE_T5_TSV"\n`
  );
  const origin = `http://127.0.0.1:${await closedPort()}`;
  const { code, stdout, stderr } = await runBounded({
    argv: ["bash", leg],
    bound,
    cwd: scratch("cwd"),
    env: {
      PATH: bin,
      HOME: scratch("home"),
      ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
      OUTSIDE_ALLOW_HTTP: "1",
      OUTSIDE_DISPATCH_URL: origin,
      OUTSIDE_BROKER_URL: origin,
      OUTSIDE_LISTENER_URL: origin,
      OUTSIDE_PROJECT: "EXAMPLE",
      OUTSIDE_PERSON: "outside-e2e@example.com",
      OUTSIDE_PEER_SESSION: "example-peer",
      OUTSIDE_APPROVE_COMMAND: approve,
      OUTSIDE_PI_ENVOY_VERSION: "0.0.0",
      OUTSIDE_REPOSITORY: "example-owner/example-repo",
      ...(browserRows.length === 0 ? {} : { OUTSIDE_T5_COMMAND: browser }),
    },
    from: import.meta.url,
  });
  return { code, stdout, stderr };
}

describe.skipIf(skipWithout("bash", "bun"))(
  "the public leg's own judgments, against stand-ins",
  () => {
    test(
      "P6, P7 and P10 pass only on the edge's own refusals, and T5's agent part only when every later trade is refused and the socket open at the refusal closes 4401 or 4403",
      async () => {
        const [pass, crossing, early, dropped, unread, closedBefore] = await Promise.all([
          publicStandInRun({ trades: [true, false, false], holdCode: 4401 }),
          publicStandInRun({
            trades: [true, false, true],
            holdCode: 4401,
            foreignSend: { status: 200, code: null, detail: null },
            approve: { status: 403, code: "FORBIDDEN", detail: "refused" },
            livePublish: { status: 200, code: null, detail: null },
          }),
          publicStandInRun({ trades: [false] }),
          publicStandInRun({ trades: [true, false, false], holdCode: 1006 }),
          publicStandInRun({ trades: [true, false, null], holdCode: 4401 }),
          publicStandInRun({
            trades: [true, false, false],
            holdCode: 4401,
            holdClosesAtOpen: true,
          }),
        ]);
        expect({ verdicts: verdictsOf(pass.stdout), stdout: pass.stdout }).toMatchObject({
          verdicts: {
            P1: "PASS",
            P2: "PASS",
            P3: "PASS",
            P5: "PASS",
            P6: "PASS",
            P7: "PASS",
            P10: "PASS",
            "T5-agent": "PASS",
          },
        });
        const passed = rowsOf(pass.stdout);
        expect(passed.get("P6")?.got).toContain(
          "foreign_listed=no foreign_send=refused sub=refused"
        );
        expect(passed.get("T5-agent")?.got).toContain("closed 4401");
        expect(passed.get("T5-agent")?.got).toMatch(/last_answered_at=\d+ /);

        // The control of those PASSes: the edge accepts a send to the session outside the project,
        // refuses the approve for another reason than its scope, takes a publish of the session's
        // live view, and answers a trade after the first refusal.
        expect({ verdicts: verdictsOf(crossing.stdout), stdout: crossing.stdout }).toMatchObject({
          verdicts: { P6: "FAIL", P7: "FAIL", P10: "FAIL", "T5-agent": "FAIL" },
        });
        const crossed = rowsOf(crossing.stdout);
        expect(crossed.get("P6")?.got).toContain("foreign_send=accepted sub=refused");
        expect(crossed.get("P7")?.got).toContain(
          "approve answered 403 FORBIDDEN, not OAUTH_SCOPE_REQUIRED"
        );
        expect(crossed.get("P10")?.got).toContain(
          "publish agentstream.omp-session-1.frames answered 200"
        );
        expect(crossed.get("T5-agent")?.got).toContain("a trade was answered");
        expect(crossing.code).toBe(1);

        // A removal before any socket opened leaves the socket part unread, not failed; a close with
        // no edge code says nothing of the removal; and a later trade with no readable answer is
        // neither a refusal nor an answer, so "every later trade refused" is not read.
        expect(rowsOf(early.stdout).get("T5-agent")).toMatchObject({ verdict: "UNKNOWN" });
        expect(rowsOf(early.stdout).get("T5-agent")?.got).toContain("no relay socket was open");
        expect(rowsOf(dropped.stdout).get("T5-agent")).toMatchObject({ verdict: "UNKNOWN" });
        expect(rowsOf(dropped.stdout).get("T5-agent")?.got).toContain(
          "closed 1006, not the edge's own"
        );
        expect({
          agent: rowsOf(unread.stdout).get("T5-agent"),
          stdout: unread.stdout,
        }).toMatchObject({ agent: { verdict: "UNKNOWN" } });
        expect(rowsOf(unread.stdout).get("T5-agent")?.got).toContain("got no readable answer");
        // A socket that closed before the refusal, even with the edge's own 4401, was not the
        // socket open at the removal: the leg reads its close as nothing of the removal, and the
        // socket part as not read, where it would read PASS if it took a closed socket for open.
        expect({
          agent: rowsOf(closedBefore.stdout).get("T5-agent"),
          stdout: closedBefore.stdout,
        }).toMatchObject({ agent: { verdict: "UNKNOWN" } });
        expect(rowsOf(closedBefore.stdout).get("T5-agent")?.got).toContain(
          "no relay socket was open at the first refusal"
        );
      },
      // The six runs at once, each a bounded child under the public leg's own bound.
      rowLimit({
        atOnce: ["publicRun", "publicRun", "publicRun", "publicRun", "publicRun", "publicRun"],
      })
    );

    test(
      "each of the browser's T5 rows reads as its worst copy: a FAIL then a PASS of T5-cookie reads FAIL",
      async () => {
        const run = await publicStandInRun({
          trades: [true, false, false],
          holdCode: 4401,
          browserRows: [
            ["T5-cookie", "at=100 the cookie still answered", "FAIL"],
            ["T5-cookie", "at=130 the cookie got 401", "PASS"],
            ["T5-passkey", "at=140 the passkey sign-in was refused", "PASS"],
          ],
        });
        // T5-passkey, one PASS, is the control: one copy reads as it is.
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: { "T5-cookie": "FAIL", "T5-passkey": "PASS" },
        });
        expect(rowsOf(run.stdout).get("T5-cookie")?.got).toContain("the cookie still answered");
      },
      // The run, a bounded child under the public leg's own bound.
      rowLimit("publicRun")
    );

    // With no close time the real stand-in's hold sleeps for the T5 window (20 minutes). The
    // 30-second publicT5Diagnostic bound covers the measured public-leg start to this hold under
    // the row's parallel shape, so the forced hold names the T5 window and live processes.
    test(
      "a T5 relay hold that never closes leaves a named timeout diagnostic",
      async () => {
        const failure = await publicStandInRun({ trades: [true] }, "publicT5Diagnostic").then(
          () => "the public leg ended before its deliberate held relay bound",
          (error: unknown) => (error instanceof Error ? error.message : String(error))
        );
        expect(failure, "the held relay did not reach the T5 window").toContain(
          "outside-agent-public: T5: window open for up to 1200 s"
        );
        expect(failure, "the held relay named no live process").toMatch(
          /still running when its bound passed:\n {2}pid=\d+ ppid=\d+ name=/
        );
      },
      rowLimit("publicT5Diagnostic")
    );
  }
);
