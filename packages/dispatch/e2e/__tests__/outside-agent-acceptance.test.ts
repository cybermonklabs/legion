/** What the two legs of the outside agent's acceptance print when a row's target is absent:
 *  `e2e/acceptance/outside-agent-public.sh` (rows P1-P9, the leg a runner on the public internet
 *  runs) and `e2e/acceptance/outside-agent-tailnet.sh` (the pins, the scratch project's issue,
 *  rows T1-T4, the public leg's rows and the gate, the leg a developer box runs). Each leg prints
 *  a TSV on stdout, a header `row want got verdict` and one line per row, and a row it could not
 *  run reads UNKNOWN with a `got` that names what it could not read. Every row UNKNOWN exits 2.
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
 */

import { describe, expect, test } from "bun:test";
import { chmodSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";

const acceptance = fileURLToPath(new URL("../acceptance/", import.meta.url));
const publicLeg = join(acceptance, "outside-agent-public.sh");
const tailnetLeg = join(acceptance, "outside-agent-tailnet.sh");

/** The rows each leg prints, in order, after its header: the public leg's P1-P9 and its verdict;
 *  the tailnet leg's own rows, the public leg's P1-P9 as it read them, and the gate. */
const publicRows = ["P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9"];
const publicLegRows = [...publicRows, "verdict"];
const tailnetRows = ["pins", "issue", "T1", "T2", "T3", "T4", "P10", ...publicRows, "gate"];

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
const scratch = temporaryDirectories("outside-agent-acceptance");

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

/** A copy of `leg` that defaults to PASS: every `UNKNOWN` reads `PASS` and every `exit 2` reads
 *  `exit 0`. It sits beside nothing, so the files the leg runs beside it are absent too. */
function passingCopy(leg: string): string {
  const copy = join(scratch("mutant"), leg.split("/").pop() ?? "leg.sh");
  writeFileSync(
    copy,
    readFileSync(leg, "utf8").replaceAll("UNKNOWN", "PASS").replaceAll("exit 2", "exit 0")
  );
  chmodSync(copy, 0o755);
  return copy;
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
    OUTSIDE_TAILNET_BROKER_URL: origin,
    OUTSIDE_LISTENER_URL: origin,
    OUTSIDE_PERSON: "outside-e2e@example.com",
    OUTSIDE_CANARY: "EXAMPLE_CANARY",
    OUTSIDE_REPOSITORY: "example-owner/example-repo",
    OUTSIDE_WORKFLOW_REPO: "example-owner/example-deploy",
    OUTSIDE_WORKFLOW_INPUTS: "invite_url=https://dispatch.example/invite/example",
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
      for (const name of ["the project key", "OUTSIDE_PINS", "OUTSIDE_ENROLL_REFUSAL_COMMIT"]) {
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
