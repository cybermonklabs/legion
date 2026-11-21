/** What the two legs of the outside agent's acceptance print when a row's target is absent:
 *  `e2e/acceptance/outside-agent-public.sh` (rows P1-P9, the leg a runner on the public internet
 *  runs) and `e2e/acceptance/outside-agent-tailnet.sh` (the pins, the scratch project's issue,
 *  rows T1-T5, the public leg's rows, the public leg's own last word, the sweep and the gate, the
 *  leg a developer box runs). Each leg prints a TSV on stdout, a header `row want got verdict` and
 *  one line per row, and a row it could not run reads UNKNOWN with a `got` that names what it
 *  could not read. Every row UNKNOWN exits 2.
 *
 *  Each leg runs twice here. Once with no input at all, so every row names the input it lacks. Once
 *  with every input set and nothing behind any of them: each origin is a loopback port that
 *  nothing listens on, and PATH holds only the shell tools the legs read text with, no `omp`,
 *  `tmux`, `bun`, `gh`, `git`, `crane` or `agent-secrets`, so every row reaches its first read of
 *  a target and finds nothing.
 *
 *  The control is the same check run on a copy of each leg in which every `UNKNOWN` reads `PASS`
 *  and every `exit 2` reads `exit 0`: a leg that defaults to PASS. The check must fail on that
 *  copy, and the row says what it found, so a check that cannot fail fails here.
 *
 *  The blocks after those run the legs against stand-ins of what they drive, and read the rows
 *  each leg judges by its own controls PASS where those controls answer and not PASS where they do
 *  not; and the background shapes the legs start their children in, stopped by the shared
 *  `reap`, beside the shapes that `reap` cannot stop.
 */

import { describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";

const acceptance = fileURLToPath(new URL("../acceptance/", import.meta.url));
const publicLeg = join(acceptance, "outside-agent-public.sh");
const tailnetLeg = join(acceptance, "outside-agent-tailnet.sh");
/** The support files the legs source from beside them: a leg copied elsewhere takes them along. */
const supportFiles = ["outside-agent-lib.sh", "outside-agent-peer.sh", "outside-agent-sweep.sh"];

/** The rows each leg prints, in order, after its header: the public leg's P1-P9, its revocations,
 *  its three T5 rows and its verdict; the tailnet leg's own rows, the public leg's P1-P9 as it read
 *  them, the public leg's own last word, the sweep and the gate. */
const publicRows = ["P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9"];
const publicLegRows = [
  ...publicRows,
  "revocations",
  "T5-agent",
  "T5-cookie",
  "T5-passkey",
  "verdict",
];
const tailnetRows = [
  "pins",
  "issue",
  "T1",
  "T2",
  "T3",
  "T4",
  "T5",
  ...publicRows,
  "public",
  "sweep",
  "gate",
];

/** The shell tools the legs read text with. PATH holds these alone in the absent-target runs, so
 *  no tool the legs drive (`omp`, `tmux`, `bun`, `gh`, `git`, `crane`, `agent-secrets`,
 *  `agent-secrets-helper`) is there. */
const textTools = [
  "bash",
  "curl",
  "sleep",
  "date",
  "mktemp",
  "rm",
  "mkdir",
  "cat",
  "ls",
  "od",
  "tr",
  "chmod",
  "wc",
  "env",
  "kill",
];

const withoutTools = skipWithout("bash", "curl");
// On tmpfs: the afterEach that removes a row's directories has Bun's 5 s for a hook, and on the
// shared disk one removal of a row's ~30 entries was measured at up to 17.5 s (median 2 ms, 30
// trials, load ~1500), against at most 16 ms on /dev/shm. The suite holds at most 233 entries at
// once (measured with the legs' own scratch under the same root), under 1 MiB at one 4 KiB page a
// file, so 8 MiB leaves room for 8 runs sharing the host.
const scratch = temporaryDirectories("outside-agent-acceptance", {
  root: memoryRoot({ need: 8 * 2 ** 20 }),
});

/** A loopback port that nothing listens on: one the kernel gave a listener this row then closed. */
function closedPort(): number {
  const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  const { port } = listener;
  listener.stop(true);
  return port;
}

/** A directory holding a link to each of `textTools` this host has, for PATH. */
function textToolPath(): string {
  const bin = join(scratch("bin"), "bin");
  mkdirSync(bin);
  for (const tool of textTools) {
    const found = hostTool(tool);
    if (found !== null) symlinkSync(found, join(bin, tool));
  }
  return bin;
}

/** A copy of `leg` in a directory of its own, with the support files it sources beside it, each
 *  through `change`; nothing else it runs beside it (the probe, the sweep, the reach check) is
 *  there. */
function copyLeg(leg: string, change: (text: string) => string = (text) => text): string {
  const dir = scratch("leg");
  for (const file of [leg, ...supportFiles.map((name) => join(acceptance, name))]) {
    const copy = join(dir, file.split("/").pop() ?? "leg.sh");
    writeFileSync(copy, change(readFileSync(file, "utf8")));
    chmodSync(copy, 0o755);
  }
  return join(dir, leg.split("/").pop() ?? "leg.sh");
}

/** A copy of `leg` that defaults to PASS: every `UNKNOWN` reads `PASS` and every `exit 2` reads
 *  `exit 0`, in the leg and in what it sources. */
function passingCopy(leg: string): string {
  return copyLeg(leg, (text) => text.replaceAll("UNKNOWN", "PASS").replaceAll("exit 2", "exit 0"));
}

interface LegRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

async function runLeg(
  leg: string,
  args: readonly string[],
  env: Record<string, string>
): Promise<LegRun> {
  const { code, stdout, stderr } = await runBounded({
    argv: ["bash", leg, ...args],
    bound: "short",
    cwd: scratch("cwd"),
    env,
    from: import.meta.url,
  });
  return { code, stdout, stderr };
}

/** What is wrong with `run` as a leg's answer when every row's target is absent: each problem in
 *  words, none when every one of `rows` reads UNKNOWN in order under the header, names what it
 *  could not read, and the run exits 2. An empty stdout is a problem for every row, never a pass. */
function unknownProblems(run: LegRun, rows: readonly string[]): string[] {
  const problems: string[] = [];
  if (run.code !== 2) problems.push(`exit ${run.code}, want 2`);
  const lines = run.stdout.split("\n").filter((line) => line !== "");
  if (lines[0] !== "row\twant\tgot\tverdict") {
    problems.push(`the first line is ${JSON.stringify(lines[0] ?? "")}, want the header`);
  }
  const body = lines.slice(1);
  rows.forEach((row, index) => {
    const fields = (body[index] ?? "").split("\t");
    if (fields[0] !== row) {
      problems.push(`line ${index + 2} is ${JSON.stringify(body[index] ?? "")}, want row ${row}`);
      return;
    }
    if (fields.length !== 4) problems.push(`row ${row} has ${fields.length} fields, want 4`);
    if ((fields[1] ?? "").trim() === "") problems.push(`row ${row} names no want`);
    if ((fields[2] ?? "").trim() === "")
      problems.push(`row ${row} names nothing it could not read`);
    if (fields[3] !== "UNKNOWN") problems.push(`row ${row} reads ${fields[3]}, want UNKNOWN`);
  });
  if (body.length !== rows.length) {
    problems.push(`${body.length} rows, want ${rows.length}`);
  }
  return problems;
}

function noInput(): Record<string, string> {
  return {
    PATH: process.env.PATH ?? "",
    HOME: scratch("home"),
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
  };
}

function publicInputs(): Record<string, string> {
  const origin = `http://127.0.0.1:${closedPort()}`;
  const inputs = scratch("inputs");
  const targets = join(inputs, "targets.txt");
  writeFileSync(targets, `hub 127.0.0.1 ${closedPort()}\n`);
  const approve = join(inputs, "approve");
  writeFileSync(approve, '#!/bin/sh\necho "UNKNOWN: $1 no browser"\nexit 2\n');
  chmodSync(approve, 0o755);
  return {
    PATH: textToolPath(),
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
    OUTSIDE_PATH_TARGETS: targets,
    OUTSIDE_REPOSITORY: "example-owner/example-repo",
    OUTSIDE_CANARY: "EXAMPLE_CANARY",
    OUTSIDE_AGENT_PUBLIC_TSV: join(inputs, "public.tsv"),
  };
}

function tailnetInputs(): Record<string, string> {
  const origin = `http://127.0.0.1:${closedPort()}`;
  const inputs = scratch("inputs");
  const pins = join(inputs, "pins.txt");
  writeFileSync(
    pins,
    [
      `broker ${join(inputs, "absent-broker.yaml")} example:brokerImage`,
      `dispatch ${join(inputs, "absent-dispatch.yaml")} example:dispatchImage`,
      `edge-listener ${join(inputs, "absent-listener.yaml")} example:listenerImage`,
      "",
    ].join("\n")
  );
  return {
    PATH: textToolPath(),
    HOME: scratch("home"),
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
    OUTSIDE_ALLOW_HTTP: "1",
    OUTSIDE_PINS: pins,
    OUTSIDE_ENROLL_REFUSAL_COMMIT: "0123456789abcdef0123456789abcdef01234567",
    OUTSIDE_DISPATCH_COMMITS: "89abcdef0123456789abcdef0123456789abcdef",
    OUTSIDE_TAILNET_BROKER_URL: origin,
    OUTSIDE_LISTENER_URL: origin,
    OUTSIDE_PERSON: "outside-e2e@example.com",
    OUTSIDE_CANARY: "EXAMPLE_CANARY",
    OUTSIDE_REPOSITORY: "example-owner/example-repo",
    OUTSIDE_WORKFLOW_REPO: "example-owner/example-deploy",
    OUTSIDE_WORKFLOW_INPUTS: "edge_host=dispatch.edge.example",
    OUTSIDE_TAILNET_TSV: join(inputs, "tailnet.tsv"),
  };
}

describe.skipIf(withoutTools)("the outside agent's acceptance legs", () => {
  test(
    "the public leg reads UNKNOWN for every row and exits 2 when it is given no input",
    async () => {
      const run = await runLeg(publicLeg, [], noInput());
      expect(unknownProblems(run, publicLegRows)).toEqual([]);
      for (const name of [
        "OUTSIDE_DISPATCH_URL",
        "OUTSIDE_PI_ENVOY_VERSION",
        "OUTSIDE_PATH_TARGETS",
      ]) {
        expect(run.stdout).toContain(name);
      }
    },
    // The leg, a bounded child under the short bound.
    rowLimit("short")
  );

  test(
    "the public leg reads UNKNOWN for every row and exits 2 when nothing answers behind its inputs",
    async () => {
      const env = publicInputs();
      const run = await runLeg(publicLeg, [], env);
      expect(unknownProblems(run, publicLegRows)).toEqual([]);
      expect(run.stdout).toContain("omp");
      // The TSV it writes for the workflow's artifact is the one it prints.
      expect(readFileSync(env.OUTSIDE_AGENT_PUBLIC_TSV as string, "utf8")).toBe(run.stdout);
    },
    // The leg, a bounded child under the short bound; its P9 runs outside-path-reach.sh inside it.
    rowLimit("short")
  );

  test(
    "the tailnet leg reads UNKNOWN for every row and exits 2 when it is given no input",
    async () => {
      const run = await runLeg(tailnetLeg, [], noInput());
      expect(unknownProblems(run, tailnetRows)).toEqual([]);
      for (const name of [
        "the project key",
        "OUTSIDE_PINS",
        "OUTSIDE_ENROLL_REFUSAL_COMMIT",
        "OUTSIDE_DISPATCH_COMMITS",
      ]) {
        expect(run.stdout).toContain(name);
      }
    },
    // The leg, a bounded child under the short bound.
    rowLimit("short")
  );

  test(
    "the tailnet leg reads UNKNOWN for every row and exits 2 when nothing answers behind its inputs",
    async () => {
      const env = tailnetInputs();
      const run = await runLeg(tailnetLeg, ["EXAMPLE"], env);
      expect(unknownProblems(run, tailnetRows)).toEqual([]);
      expect(readFileSync(env.OUTSIDE_TAILNET_TSV as string, "utf8")).toBe(run.stdout);
    },
    // The leg, a bounded child under the short bound.
    rowLimit("short")
  );

  test(
    "the check fails on a copy of either leg that defaults to PASS",
    async () => {
      const runs = [
        await runLeg(passingCopy(publicLeg), [], noInput()),
        await runLeg(passingCopy(publicLeg), [], publicInputs()),
        await runLeg(passingCopy(tailnetLeg), [], noInput()),
        await runLeg(passingCopy(tailnetLeg), ["EXAMPLE"], tailnetInputs()),
      ];
      const rowsOf = [publicLegRows, publicLegRows, tailnetRows, tailnetRows];
      runs.forEach((run, index) => {
        const problems = unknownProblems(run, rowsOf[index] as string[]);
        expect(problems).toContain("exit 0, want 2");
        expect(problems.some((problem) => problem.endsWith("reads PASS, want UNKNOWN"))).toBe(true);
      });
    },
    // The four copies, one after another, each a bounded child under the short bound.
    rowLimit("short", "short", "short", "short")
  );
});

/** An executable file at `path` holding `text`. */
function script(path: string, text: string): string {
  writeFileSync(path, text);
  chmodSync(path, 0o755);
  return path;
}

/* The shapes a leg starts its background children in, and the shared `reap` that stops them at the
 * leg's end. Each shape's leaf is a `sleep` of a number no other process runs, so a leaf left
 * running after `reap` is found by that number. The shapes the legs use are a simple command, a
 * function whose last word is `exec`, and such a function behind a prefix assignment: `$!` names
 * the leaf itself, and `reap` stops it. The two shapes the legs no longer use are the control: a
 * subshell that does more after its command, and a function that runs its command without `exec`,
 * whose leaf `reap` cannot reach. A row per shape. */
describe.skipIf(skipWithout("bash", "sleep", "pgrep"))(
  "the shapes a leg starts its children in",
  () => {
    test(
      "reap stops every shape the legs use, and not the two they no longer use",
      async () => {
        const base = `9${Math.floor(Math.random() * 1e6)
          .toString()
          .padStart(6, "0")}`;
        const sleep = hostTool("sleep") as string;
        const shapes = script(
          join(scratch("shapes"), "shapes.sh"),
          `#!/usr/bin/env bash
set -u
. '${join(acceptance, "outside-agent-lib.sh")}'
background=''
env -i '${sleep}' ${base}1 &
background="$background $!"
leaf() { exec '${sleep}' "$1"; }
leaf ${base}2 &
background="$background $!"
probe_as=x leaf ${base}3 &
background="$background $!"
( '${sleep}' ${base}4; echo done >/dev/null ) &
background="$background $!"
noexec() { '${sleep}' "$1"; }
noexec ${base}5 &
background="$background $!"
sleep 1
reap
sleep 1
for n in 1 2 3 4 5; do
  pids=$(pgrep -f "^${sleep} ${base}$n\\$")
  if [ -n "$pids" ]; then
    echo "$n survived"
    # shellcheck disable=SC2086 # each pid a word
    kill $pids
  else
    echo "$n stopped"
  fi
done
`
        );
        const { code, stdout } = await runBounded({
          argv: ["bash", shapes],
          bound: "short",
          cwd: scratch("cwd"),
          env: { PATH: process.env.PATH ?? "" },
          from: import.meta.url,
        });
        expect({ code, stdout }).toEqual({
          code: 0,
          stdout: ["1 stopped", "2 stopped", "3 stopped", "4 survived", "5 survived", ""].join(
            "\n"
          ),
        });
      },
      // The shape script, a bounded child under the short bound.
      rowLimit("short")
    );
  }
);

/* The public leg against stand-ins of what it drives: an `omp` whose plugin install leaves the
 * plugin's `dispatch` command, a `dispatch login --device` that prints a code and keeps a login,
 * an approve command, and an `outside-agent-probe.ts` beside the leg that answers each command as
 * a scenario file says. No `tmux` is on PATH, so P4 and P8 read UNKNOWN in every run here, and no
 * reach check or revoke or browser command is given. */

interface PublicScenario {
  /** The answers of the trades the T5 window makes, in turn; the last one repeats. */
  readonly trades: readonly boolean[];
  /** When the relay socket the T5 window holds closes after it opens, and with which code; it
   *  stays open where none is given. */
  readonly holdCloseMs?: number;
  readonly holdCode?: number;
  /** The probe's answers by command, where they differ from an edge that refuses as it should. */
  readonly foreignSend?: Record<string, unknown>;
  readonly approve?: Record<string, unknown>;
}

/** The stand-in probe: answers each command the public leg runs, from scenario.json beside it. */
const standInProbe = `
const dir = import.meta.dir;
const scenario = JSON.parse(await Bun.file(dir + "/scenario.json").text());
const [command, ...args] = Bun.argv.slice(2);
const print = (value) => process.stdout.write(JSON.stringify(value) + "\\n");
const refusal = (code) => ({ status: 403, code, detail: "refused" });
switch (command) {
  case "whoami":
    print({ status: 200, code: null, detail: null, kind: "agent", owner: "outside-e2e@example.com", project: "EXAMPLE", scopes: [] });
    break;
  case "interest":
    print(refusal(args[0].startsWith("notifications.github.") ? "repository_not_in_project" : "topic_not_in_project"));
    break;
  case "publish":
    print(refusal("topic_not_published"));
    break;
  case "github-event":
    print({ status: 200, code: null, detail: null, event: "notifications.github.example-owner.example-repo.push", sockets: 1 });
    break;
  case "sessions":
    print({ status: 200, code: null, detail: null, sessions: ["example-peer"], at_dir: null });
    break;
  case "relay-sub":
    print({ granted: false, code: "subject_not_granted" });
    break;
  case "send":
    print(args[0].endsWith("-foreign") ? (scenario.foreignSend ?? { status: 404, code: null, detail: "no such session" }) : { status: 200, code: null, detail: null });
    break;
  case "agent-token":
    print(refusal("OAUTH_SCOPE_REQUIRED"));
    break;
  case "approve":
    print(scenario.approve ?? refusal("OAUTH_SCOPE_REQUIRED"));
    break;
  case "trade": {
    const counter = dir + "/trades.count";
    const done = (await Bun.file(counter).exists()) ? Number(await Bun.file(counter).text()) : 0;
    await Bun.write(counter, String(done + 1));
    const traded = scenario.trades[Math.min(done, scenario.trades.length - 1)];
    print(traded ? { traded: true, expires_at: Date.now() + 300000 } : { traded: false, status: 403, detail: "refused" });
    break;
  }
  case "hold":
    print({ opened_at: Date.now() });
    if (scenario.holdCloseMs === undefined) await Bun.sleep(Number(args[0]) * 1000);
    else {
      await Bun.sleep(scenario.holdCloseMs);
      print({ closed_at: Date.now(), code: scenario.holdCode ?? 4401 });
    }
    break;
  default:
    print({ unknown: "the stand-in probe has no " + command });
    process.exit(2);
}
`;

/** One run of a copy of the public leg against the stand-ins, as `scenario` says. */
async function publicStandInRun(scenario: PublicScenario): Promise<LegRun> {
  const leg = copyLeg(publicLeg);
  const legDir = leg.slice(0, leg.lastIndexOf("/"));
  writeFileSync(join(legDir, "outside-agent-probe.ts"), standInProbe);
  writeFileSync(join(legDir, "scenario.json"), JSON.stringify(scenario));
  const bin = textToolPath();
  const bun = hostTool("bun");
  if (bun !== null) symlinkSync(bun, join(bin, "bun"));
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
  const inputs = scratch("inputs");
  const approve = script(join(inputs, "approve"), '#!/bin/sh\necho "APPROVED $1"\n');
  const origin = `http://127.0.0.1:${closedPort()}`;
  return runLeg(leg, [], {
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
  });
}

describe.skipIf(skipWithout("bash", "bun"))(
  "the public leg's own judgments, against stand-ins",
  () => {
    test(
      "P6 and P7 pass only on the edge's own refusals, and T5's agent part only when every later trade is refused and the socket open at the refusal closes 4401 or 4403",
      async () => {
        const [pass, crossing, early, dropped] = await Promise.all([
          publicStandInRun({ trades: [true, false, false], holdCloseMs: 15_000, holdCode: 4401 }),
          publicStandInRun({
            trades: [true, false, true],
            holdCloseMs: 15_000,
            holdCode: 4401,
            foreignSend: { status: 200, code: null, detail: null },
            approve: { status: 403, code: "FORBIDDEN", detail: "refused" },
          }),
          publicStandInRun({ trades: [false] }),
          publicStandInRun({ trades: [true, false, false], holdCloseMs: 15_000, holdCode: 1006 }),
        ]);
        expect({ verdicts: verdictsOf(pass.stdout), stdout: pass.stdout }).toMatchObject({
          verdicts: {
            P1: "PASS",
            P2: "PASS",
            P3: "PASS",
            P5: "PASS",
            P6: "PASS",
            P7: "PASS",
            "T5-agent": "PASS",
          },
        });
        expect(rowsOf(pass.stdout).get("P6")?.got).toContain(
          "foreign_listed=no foreign_send=refused sub=refused"
        );
        expect(rowsOf(pass.stdout).get("T5-agent")?.got).toContain("closed 4401");

        // The control of those PASSes: the edge accepts a send to the session outside the project,
        // refuses the approve for another reason than its scope, and answers a trade after the
        // first refusal.
        expect({ verdicts: verdictsOf(crossing.stdout), stdout: crossing.stdout }).toMatchObject({
          verdicts: { P6: "FAIL", P7: "FAIL", "T5-agent": "FAIL" },
        });
        const crossed = rowsOf(crossing.stdout);
        expect(crossed.get("P6")?.got).toContain("foreign_send=accepted sub=refused");
        expect(crossed.get("P7")?.got).toContain(
          "approve answered 403 FORBIDDEN, not OAUTH_SCOPE_REQUIRED"
        );
        expect(crossed.get("T5-agent")?.got).toContain("a trade was answered");
        expect(crossing.code).toBe(1);

        // A removal before any socket opened leaves the socket part unread, not failed; and a close
        // with no edge code says nothing of the removal.
        expect(rowsOf(early.stdout).get("T5-agent")).toMatchObject({ verdict: "UNKNOWN" });
        expect(rowsOf(early.stdout).get("T5-agent")?.got).toContain("no relay socket was open");
        expect(rowsOf(dropped.stdout).get("T5-agent")).toMatchObject({ verdict: "UNKNOWN" });
        expect(rowsOf(dropped.stdout).get("T5-agent")?.got).toContain(
          "closed 1006, not the edge's own"
        );
      },
      // The four runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short"] })
    );
  }
);

/* The tailnet leg against stand-ins of what it drives, so the rows it judges by its own controls
 * can be read PASS, FAIL and UNKNOWN: a git checkout whose commits the pins name; an
 * `agent-secrets` and an `agent-secrets-helper` that act as a broker holding the enrollment refusal
 * would; a Dispatch that answers the full member's browser session, as the private origin does; a
 * read sweep beside the leg; and a public leg that approves the scratch login and writes the TSV
 * and artifacts the workflow would. No listener is named, so the peer does not start, and P4, P5,
 * P6 and T5 read UNKNOWN in every run here. */

/** The broker's refusal of a restricted member's enrollment, as the scratch helper prints it. */
const memberRefusal = "403 RESTRICTED_MEMBER_ENROLLS_NOTHING: a restricted member enrolls nothing";
const fullLogin = "full@example.com";
const proxyRepository = "example-owner/private-repo";

interface StandIns {
  /** Whether the stand-in Dispatch approves the full member's machine login and serves the GitHub
   *  proxy's read of the sweep's repository, or refuses both. */
  readonly controls: "answered" | "refused";
  /** What the scratch helper's canary session, and the canary's request, are refused with. */
  readonly canaryRefusal: string;
  /** Whether Dispatch's build holds every commit OUTSIDE_DISPATCH_COMMITS names. */
  readonly dispatchCommits: "held" | "lacking";
  /** Whether OUTSIDE_FULL_COOKIE_FILE is given, and the proxy read the fills leave out, if any. */
  readonly cookie?: "given" | "absent";
  readonly fillsLeaveOut?: string;
  /** Whether the public leg approves the scratch login; a login nobody approves ends `expired`. */
  readonly approve?: boolean;
  /** The verdict the public leg's revoke row of the scratch login reads (default PASS). */
  readonly revokeVerdict?: string;
  /** The public leg's own last word: its verdict row (none for "absent") and its exit. */
  readonly publicVerdict?: "PASS" | "FAIL" | "absent";
  readonly publicExit?: number;
  /** P6's row as the public leg writes it, where it differs from a PASS. */
  readonly p6?: string;
}

interface StandInRun extends LegRun {
  /** Each request the stand-in Dispatch answered, `METHOD path`. */
  readonly seen: readonly string[];
  readonly state: string;
  readonly repo: Checkout;
}

interface Checkout {
  readonly dir: string;
  /** A commit every image's commit holds. */
  readonly held: string;
  /** The commit every image is built from, a child of `held`. */
  readonly built: string;
  /** Another child of `held`, which `built` does not hold. */
  readonly apart: string;
}

function git(dir: string, ...args: string[]): string {
  const ran = Bun.spawnSync(["git", "-C", dir, "-c", "init.defaultBranch=main", ...args], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: dir,
      GIT_CONFIG_NOSYSTEM: "1",
      GIT_AUTHOR_NAME: "example",
      GIT_AUTHOR_EMAIL: "example@example.com",
      GIT_COMMITTER_NAME: "example",
      GIT_COMMITTER_EMAIL: "example@example.com",
    },
  });
  if (!ran.success) throw new Error(`git ${args.join(" ")}: ${ran.stderr.toString()}`);
  return ran.stdout.toString().trim();
}

function checkout(): Checkout {
  const dir = scratch("checkout");
  git(dir, "init", "-q", "--template=");
  git(dir, "commit", "-q", "--allow-empty", "-m", "held");
  const held = git(dir, "rev-parse", "HEAD");
  git(dir, "commit", "-q", "--allow-empty", "-m", "built");
  const built = git(dir, "rev-parse", "HEAD");
  const apart = git(dir, "commit-tree", `${held}^{tree}`, "-p", held, "-m", "apart");
  return { dir, held, built, apart };
}

/** A PATH of the text tools, the few more the stand-in run reads with, and the stand-ins of
 *  `agent-secrets` and `agent-secrets-helper`, which keep their state in `state`. */
function standInPath(state: string): string {
  const bin = textToolPath();
  for (const tool of ["git", "base64", "sh", "printenv", "cp"]) {
    const found = hostTool(tool);
    if (found !== null) symlinkSync(found, join(bin, tool));
  }
  // The helper's one duty here is its socket, which the leg waits for; Bun makes it.
  script(
    join(bin, "agent-secrets-helper"),
    `#!/usr/bin/env bash\nexec '${process.execPath}' -e 'Bun.listen({ unix: process.env.AGENT_SECRETS_HELPER_SOCK, socket: { data() {} } });'\n`
  );
  // A session of the box's own (no operator file) is enrolled and granted the canary; a scratch
  // helper's (an operator file) is refused, the first register for the member kind and the
  // canary's session and request with what canary-refusal holds. A machine login completes once
  // the stand-in Dispatch or the public leg marks its code approved, and where the state holds
  // login-expires it ends as the real one ends a code nobody approved.
  script(
    join(bin, "agent-secrets"),
    `#!/usr/bin/env bash
state='${state}'
operator=''
[ -z "\${AGENT_SECRETS_OPERATOR_FILE:-}" ] || operator=$(cat "$AGENT_SECRETS_OPERATOR_FILE")
case $operator in ${fullLogin}) code=FULL-0001 ;; *) code=TEST-0001 ;; esac
case $1 in
  machine)
    if [ "$2" = login ]; then
      echo "machine login code: $code"
      if [ -f "$state/login-expires" ] && [ "$code" = TEST-0001 ]; then
        sleep 1
        echo 'agent-secrets machine login: expired' >&2
        exit 1
      fi
      while [ ! -f "$state/approved-$code" ] && [ -e "$AGENT_SECRETS_HELPER_SOCK" ]; do sleep 0.1; done
      [ -f "$state/approved-$code" ] || exit 1
      printf '{"credential_id":"cred-%s"}\\n' "$(printf '%s' "$code" | tr 'A-Z' 'a-z')" >"$AGENT_SECRETS_MACHINE_FILE"
    elif [ "\${3:-}" = --json ]; then
      printf '[{"credential_id":"cred-%s","approved_by":"%s"}]\\n' "$(printf '%s' "$code" | tr 'A-Z' 'a-z')" "$operator"
    elif [ -f "$state/revoked" ]; then
      echo 'agent-secrets: LAUNCHER_INVALID' >&2
      exit 1
    fi
    ;;
  register)
    session=$3
    while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
    shift
    [ -n "$operator" ] || exec "$@"
    case $session in
      *-t2b-*) echo "agent-secrets: register $session: $(cat "$state/canary-refusal")" >&2 ;;
      *) echo "agent-secrets: register $session: ${memberRefusal}" >&2 ;;
    esac
    "$@"
    exit 1
    ;;
  whoami) [ -z "$operator" ] || exit 1 ;;
  request) [ -z "$operator" ] || { cat "$state/canary-refusal" >&2; exit 1; } ;;
  *)
    [ -z "$operator" ] || exit 1
    name=$1
    shift 2
    export "$name=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    exec "$@"
    ;;
esac
`
  );
  return bin;
}

interface StandInDispatch {
  readonly origin: string;
  /** Each request it answered, `METHOD path`. */
  readonly seen: readonly string[];
  stop(): void;
}

/** A Dispatch that answers the full member's browser session (cookie `session=full`, with the
 *  origin's own Origin) on the routes the leg's controls read, and 401 to anything else. */
function standInDispatch(state: string, controls: StandIns["controls"]): StandInDispatch {
  const seen: string[] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      const path = url.pathname;
      seen.push(`${request.method} ${path}`);
      if (
        request.headers.get("cookie") !== "session=full" ||
        request.headers.get("origin") !== url.origin
      ) {
        return Response.json({ code: "UNAUTHENTICATED" }, { status: 401 });
      }
      const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
      const code = typeof body.code === "string" ? body.code : "";
      const route = `${request.method} ${path}`;
      if (route === "GET /api/v1/whoami") {
        return Response.json({ kind: "user", login: fullLogin, scopes: ["person", "approve"] });
      }
      if (route === "POST /api/v1/me/agent-tokens") {
        return Response.json(
          { id: "token-1", name: body.name, token: "dsp_plaintext" },
          { status: 201 }
        );
      }
      if (route === "DELETE /api/v1/me/agent-tokens/token-1")
        return new Response(null, { status: 204 });
      if (route === "POST /api/v1/credential-requests/machine-lookup") {
        return Response.json({ record_id: `record-${code}` });
      }
      if (route === `POST /api/v1/credential-requests/record-${code}/approve`) {
        if (controls === "refused") return Response.json({ code: "FORBIDDEN" }, { status: 403 });
        writeFileSync(join(state, `approved-${code}`), "");
        return Response.json({ credential_id: `cred-${code.toLowerCase()}` });
      }
      if (route.startsWith("POST /api/v1/machine-logins/cred-")) return Response.json({});
      if (route === `GET /api/github/rest/repos/${proxyRepository}/actions/runs`) {
        return controls === "answered"
          ? Response.json({ total_count: 0, workflow_runs: [] })
          : Response.json({ code: "GITHUB_TOKEN_UNAVAILABLE" }, { status: 503 });
      }
      return Response.json({ code: "NOT_FOUND" }, { status: 404 });
    },
  });
  return { origin: server.url.origin, seen, stop: () => server.stop(true) };
}

/** A copy of the tailnet leg ready to run against the stand-ins, with a stand-in read sweep beside
 *  it and a public leg that approves the scratch login and writes what the workflow's would. Every
 *  synchronous step (the checkout's git, the files, the links) is here, before any leg starts: the
 *  stand-in Dispatch answers on this process's event loop, which a `spawnSync` would hold while a
 *  leg running beside it waits for an answer. */
interface PreparedStandIn {
  readonly leg: string;
  readonly env: Record<string, string>;
  readonly dispatch: StandInDispatch;
  readonly state: string;
  readonly repo: Checkout;
}

function prepareStandIn(standIns: StandIns): PreparedStandIn {
  const state = scratch("state");
  writeFileSync(join(state, "canary-refusal"), `${standIns.canaryRefusal}\n`);
  if (standIns.approve === false) writeFileSync(join(state, "login-expires"), "");
  // The fills the sweep's fills mode writes: the repository, and a `can_fail` line for each proxy
  // read, but only with the full member's cookie, as restricted-sweep.sh does.
  const canFail = [
    "GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}",
    "GET /api/github/rest/repos/{owner}/{repo}/issues/{number}",
    "GET /api/github/rest/repos/{owner}/{repo}/commits/{sha}/check-runs",
    "GET /api/github/rest/repos/{owner}/{repo}/actions/runs?head_sha={sha}",
    "GET /api/v1/github/app-installation/{owner}/{repo}",
  ].filter((read) => read !== standIns.fillsLeaveOut);
  writeFileSync(
    join(state, "fills"),
    [
      "restricted-sweep-fills\t1",
      `proxy_repo\t${proxyRepository}`,
      ...canFail.map((read) => `can_fail\t${read}`),
      "",
    ].join("\n")
  );
  // The public leg's TSV: P1-P9 PASS but where a row is given, its revocations and its verdict.
  const tab = "\t";
  const publicTsv = [
    ["row", "want", "got", "verdict"],
    ...publicRows.map((name) =>
      name === "P6" && standIns.p6 !== undefined
        ? [name, `the row ${name}`, standIns.p6, standIns.p6.split(" ")[0] as string]
        : [name, `the row ${name}`, `what ${name} read`, "PASS"]
    ),
    ["revocations", "every login revoked", "the revoke command exited 0", "PASS"],
    ...(standIns.publicVerdict === "absent"
      ? []
      : [["verdict", "every row PASS", "as read", standIns.publicVerdict ?? "PASS"]]),
  ]
    .map((fields) => fields.join(tab))
    .join("\n");
  writeFileSync(join(state, "public.tsv"), `${publicTsv}\n`);
  const repo = checkout();
  const inputs = scratch("inputs");
  const leg = copyLeg(tailnetLeg);
  const legDir = leg.slice(0, leg.lastIndexOf("/"));
  // The writer is stopped by the leg when the public leg ends: on a TERM it leaves a mark and
  // ends. A writer the leg failed to stop is killed later by the bounded child's own stop, with
  // a SIGKILL, which leaves no mark.
  script(
    join(legDir, "restricted-sweep.sh"),
    `#!/usr/bin/env bash
case $SWEEP_MODE in
  fills)
    [ -n "\${SWEEP_FULL_COOKIE:-}" ] || { echo 'UNKNOWN: SWEEP_FULL_COOKIE is required'; exit 2; }
    : >'${state}/fills-read-the-cookie'
    cat '${state}/fills' >"$SWEEP_FILLS_FILE"
    ;;
  writer)
    echo 'write 2026-01-01T00:00:05Z delivered'
    sleep 600 &
    child=$!
    trap 'kill "$child"; : >"${state}/writer-stopped"; exit 0' TERM
    wait "$child"
    ;;
esac
`
  );
  const approve =
    standIns.approve === false ? "" : `: >'${state}/approved-'"$OUTSIDE_TAILNET_USER_CODE"`;
  const publicLegCommand = script(
    join(inputs, "public.sh"),
    `#!/usr/bin/env bash
${approve}
out=$OUTSIDE_PUBLIC_RESULTS
cp '${state}/public.tsv' "$out/outside-agent-public.tsv"
printf 'revoke-cred-%s\\trevoked\\tstatus 200\\t%s\\n' "$(printf '%s' "$OUTSIDE_TAILNET_USER_CODE" | tr 'A-Z' 'a-z')" '${standIns.revokeVerdict ?? "PASS"}' >"$out/outside-edge-revocations.tsv"
: >'${state}/revoked'
printf 'the workflow note\\nlive window 2026-01-01T00:00:00Z 2026-01-01T00:00:09Z\\nPASS: every read refused\\n' >"$out/restricted-sweep.log"
echo 0 >"$out/restricted-sweep.exit"
exit ${standIns.publicExit ?? 0}
`
  );
  const revision = script(join(inputs, "revision.sh"), `#!/bin/sh\necho ${repo.built}\n`);
  const pins: string[] = [];
  for (const name of ["broker", "dispatch", "edge-listener"]) {
    const file = join(inputs, `${name}.yaml`);
    writeFileSync(file, `example:image: registry.example/${name}@sha256:0\n`);
    pins.push(`${name} ${file} example:image`);
  }
  writeFileSync(join(inputs, "pins.txt"), `${pins.join("\n")}\n`);
  writeFileSync(join(inputs, "cookie"), "session=full\n");
  writeFileSync(join(inputs, "token"), "example-token\n");
  const dispatch = standInDispatch(state, standIns.controls);
  const env = {
    PATH: standInPath(state),
    HOME: scratch("home"),
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
    OUTSIDE_ALLOW_HTTP: "1",
    OUTSIDE_PINS: join(inputs, "pins.txt"),
    OUTSIDE_IMAGE_REVISION_COMMAND: `sh ${revision}`,
    OUTSIDE_LEGION_CHECKOUT: repo.dir,
    OUTSIDE_ENROLL_REFUSAL_COMMIT: repo.held,
    OUTSIDE_DISPATCH_COMMITS:
      standIns.dispatchCommits === "held" ? repo.held : `${repo.held} ${repo.apart}`,
    OUTSIDE_TAILNET_BROKER_URL: `http://127.0.0.1:${closedPort()}`,
    OUTSIDE_TAILNET_DISPATCH_URL: dispatch.origin,
    ...(standIns.cookie === "absent" ? {} : { OUTSIDE_FULL_COOKIE_FILE: join(inputs, "cookie") }),
    OUTSIDE_PERSON: "outside-e2e@example.com",
    OUTSIDE_CANARY: "EXAMPLE_CANARY",
    OUTSIDE_REPOSITORY: "example-owner/example-repo",
    OUTSIDE_SWEEP_PROJECT: "PRIV",
    OUTSIDE_SWEEP_FULL_TOKEN_FILE: join(inputs, "token"),
    OUTSIDE_SWEEP_CONVERSATION_FILE: "example-session/example-slug",
    OUTSIDE_SWEEP_WRITE_ISSUE: "PRIV-1",
    OUTSIDE_PUBLIC_COMMAND: `bash ${publicLegCommand}`,
  };
  return { leg, env, dispatch, state, repo };
}

/** The prepared leg's one run; its stand-in Dispatch stops when it ends. */
async function runStandIn(prepared: PreparedStandIn): Promise<StandInRun> {
  try {
    const run = await runLeg(prepared.leg, ["EXAMPLE"], prepared.env);
    return {
      ...run,
      seen: [...prepared.dispatch.seen],
      state: prepared.state,
      repo: prepared.repo,
    };
  } finally {
    prepared.dispatch.stop();
  }
}

/** Runs each prepared leg at once; every run answers, in the order given. */
async function runStandIns(...standIns: StandIns[]): Promise<StandInRun[]> {
  return Promise.all(standIns.map(prepareStandIn).map(runStandIn));
}

/** Each row of a leg's TSV by name: its `got` and its verdict. */
function rowsOf(stdout: string): Map<string, { readonly got: string; readonly verdict: string }> {
  const rows = new Map<string, { got: string; verdict: string }>();
  for (const line of stdout.split("\n").slice(1)) {
    const [name = "", , got = "", verdict = ""] = line.split("\t");
    if (name !== "") rows.set(name, { got, verdict });
  }
  return rows;
}

/** Each row's verdict by name. */
function verdictsOf(stdout: string): Record<string, string> {
  return Object.fromEntries([...rowsOf(stdout)].map(([name, row]) => [name, row.verdict]));
}

/** The number of UNKNOWN rows the gate counted, from its `got`. */
function unknownsCounted(stdout: string): number {
  const counted = /(\d+) UNKNOWN/.exec(rowsOf(stdout).get("gate")?.got ?? "");
  return counted === null ? -1 : Number(counted[1]);
}

const passingControls: StandIns = {
  controls: "answered",
  canaryRefusal: memberRefusal,
  dispatchCommits: "held",
};

describe.skipIf(skipWithout("bash", "curl", "git", "base64"))(
  "the tailnet leg's own controls, against stand-ins",
  () => {
    test(
      "P7 and the sweep pass only with the full member's browser answered, and T2 only with the member kind's refusal",
      async () => {
        const [answered, refused] = await runStandIns(passingControls, {
          controls: "refused",
          canaryRefusal: "403 REQUESTER_NOT_LISTED: requester not listed",
          dispatchCommits: "held",
        });
        if (answered === undefined || refused === undefined) throw new Error("two runs, want two");
        // The verdicts beside the whole TSV, so a mismatch prints every row the leg wrote.
        expect({ verdicts: verdictsOf(answered.stdout), stdout: answered.stdout }).toMatchObject({
          verdicts: {
            pins: "PASS",
            T1: "PASS",
            T2: "PASS",
            T3: "PASS",
            T4: "PASS",
            P7: "PASS",
            public: "PASS",
            sweep: "PASS",
          },
        });
        const pass = rowsOf(answered.stdout);
        expect(pass.get("P7")?.got).toContain(
          "the positive control: the full member's browser minted an agent token, 201 (revoked: 204), and approved a machine login, 200 (revoked: 200)"
        );
        expect(pass.get("sweep")?.got).toContain(
          `the full member's browser read ${proxyRepository}'s workflow runs through the proxy: 200`
        );
        // Each thing the control made is revoked again, the sweep's fills read the cookie, and the
        // leg stopped the sweep's writer itself.
        expect(answered.seen).toContain("DELETE /api/v1/me/agent-tokens/token-1");
        expect(answered.seen).toContain("POST /api/v1/machine-logins/cred-full-0001/revoke");
        expect(existsSync(join(answered.state, "fills-read-the-cookie"))).toBe(true);
        expect(existsSync(join(answered.state, "writer-stopped"))).toBe(true);
        // P4, P5, P6 and T5 have no peer, so the gate is held: P6's PASS without the session
        // outside the project to stand behind it reads UNKNOWN.
        expect(pass.get("P6")).toMatchObject({ verdict: "UNKNOWN" });
        expect(answered.code).toBe(2);

        // The control of those PASSes: the same run with the controls refused, and the canary's
        // session refused for a reason other than the member kind.
        expect({ verdicts: verdictsOf(refused.stdout), stdout: refused.stdout }).toMatchObject({
          verdicts: { T1: "PASS", T2: "FAIL", P7: "UNKNOWN", sweep: "UNKNOWN" },
        });
        const held = rowsOf(refused.stdout);
        expect(held.get("T2")?.got).toContain("the canary got no value for another reason");
        expect(held.get("P7")?.got).toContain("no positive control");
        expect(held.get("sweep")?.got).toContain("its GitHub proxy rows have no positive control");
        expect(refused.code).toBe(1);
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "the pins row fails, and no leg starts, when Dispatch's build lacks a commit the check needs",
      async () => {
        const [run] = await runStandIns({ ...passingControls, dispatchCommits: "lacking" });
        if (run === undefined) throw new Error("one run, want one");
        const rows = rowsOf(run.stdout);
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: { pins: "FAIL" },
        });
        expect(rows.get("pins")?.got).toContain(
          `Dispatch's build ${run.repo.built} does not hold ${run.repo.apart}`
        );
        expect(rows.get("T1")?.got).toContain("not run: a pin lacks a change the check needs");
        expect(run.seen).toEqual([]);
        expect(run.code).toBe(1);
      },
      // The leg, a bounded child under the short bound.
      rowLimit("short")
    );

    test(
      "the sweep reads UNKNOWN without the full member's cookie, and with fills that lack a proxy read's line",
      async () => {
        const [noCookie, gap] = await runStandIns(
          { ...passingControls, cookie: "absent" },
          {
            ...passingControls,
            fillsLeaveOut: "GET /api/v1/github/app-installation/{owner}/{repo}",
          }
        );
        if (noCookie === undefined || gap === undefined) throw new Error("two runs, want two");
        // Without the cookie the sweep does not run, and P7 has no positive control either.
        expect({ verdicts: verdictsOf(noCookie.stdout), stdout: noCookie.stdout }).toMatchObject({
          verdicts: { sweep: "UNKNOWN", P7: "UNKNOWN" },
        });
        expect(rowsOf(noCookie.stdout).get("sweep")?.got).toContain(
          "OUTSIDE_FULL_COOKIE_FILE is unset"
        );
        expect(existsSync(join(noCookie.state, "fills-read-the-cookie"))).toBe(false);
        // Fills that lack a proxy read's full-member answer hold the sweep, and only the sweep.
        expect({ verdicts: verdictsOf(gap.stdout), stdout: gap.stdout }).toMatchObject({
          verdicts: { sweep: "UNKNOWN", P7: "PASS" },
        });
        expect(rowsOf(gap.stdout).get("sweep")?.got).toContain(
          "the sweep's fills hold no full member's answer that can fail for GET /api/v1/github/app-installation/{owner}/{repo}"
        );
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "T1 reads a login nobody approved UNKNOWN, and T4 counts a revoke row with no verdict as UNKNOWN",
      async () => {
        const [control, expired, noVerdict] = await runStandIns(
          passingControls,
          { ...passingControls, approve: false },
          { ...passingControls, revokeVerdict: "SKIPPED" }
        );
        if (control === undefined || expired === undefined || noVerdict === undefined) {
          throw new Error("three runs, want three");
        }
        expect(rowsOf(control.stdout).get("T4")).toMatchObject({ verdict: "PASS" });
        expect({ verdicts: verdictsOf(expired.stdout), stdout: expired.stdout }).toMatchObject({
          verdicts: { T1: "UNKNOWN" },
        });
        expect(rowsOf(expired.stdout).get("T1")?.got).toContain(
          "was never approved: the login expired"
        );
        expect({ verdicts: verdictsOf(noVerdict.stdout), stdout: noVerdict.stdout }).toMatchObject({
          verdicts: { T4: "UNKNOWN" },
        });
        expect(rowsOf(noVerdict.stdout).get("T4")?.got).toContain(
          "reads SKIPPED, which is no verdict"
        );
        // The same run but for that row: the gate counts it, so it can never exit 0 on it.
        expect(unknownsCounted(noVerdict.stdout)).toBe(unknownsCounted(control.stdout) + 1);
      },
      // The three runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short"] })
    );

    test(
      "the public row holds the gate to the public leg's own verdict and end, and P6's sub crossing stands",
      async () => {
        const [failed, silent] = await runStandIns(
          {
            ...passingControls,
            publicVerdict: "FAIL",
            publicExit: 1,
            p6: "FAIL a relay sub to example-peer's subject was granted; peer_listed=yes foreign_listed=no foreign_send=refused sub=granted",
          },
          { ...passingControls, publicVerdict: "absent" }
        );
        if (failed === undefined || silent === undefined) throw new Error("two runs, want two");
        expect({ verdicts: verdictsOf(failed.stdout), stdout: failed.stdout }).toMatchObject({
          verdicts: { public: "FAIL", P6: "FAIL" },
        });
        expect(rowsOf(failed.stdout).get("public")?.got).toContain("verdict row FAIL");
        expect(failed.code).toBe(1);
        expect({ verdicts: verdictsOf(silent.stdout), stdout: silent.stdout }).toMatchObject({
          verdicts: { public: "UNKNOWN" },
        });
        expect(rowsOf(silent.stdout).get("public")?.got).toContain("verdict row absent");
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );
  }
);
