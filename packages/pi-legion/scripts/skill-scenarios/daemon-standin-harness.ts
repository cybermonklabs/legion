// What daemon-standin.test.ts and daemon-standin-plan.test.ts share: starting daemon-standin.ts as
// the rig runs it, on a port of its own, and reading what it answered and logged; its claim and
// grant; and a planted label checkout. It holds no state: each test file keeps its own directories.
import { expect } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { daemonContract } from "@legion/contracts/daemon-contract";
import {
  LegionGrantResponse,
  LegionRegisterResponse,
  type LegionRegistration,
} from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import type { Subprocess } from "bun";
import pkg from "../../package.json";
import { StandinLogLine } from "./scored-runs-contracts";

const STANDIN = path.join(import.meta.dir, "daemon-standin.ts");
/** The contract this checkout's plugin speaks, as the stand-in computes it without `--label`. */
export const OWN_CONTRACT = daemonContract(pkg, import.meta.dir);

/** A Legion project token: lower-case letters and digits (`isLegionProjectToken`). */
export const PROJECT = "skillsstandin";
/** The repository and default branch the stand-in's state puts every issue on: its seventh and
 *  eighth words. */
export const REPOSITORY = "example/widgets";
export const DEFAULT_BRANCH = "trunk";
export const REPO = [REPOSITORY, DEFAULT_BRANCH] as const;

export interface Standin {
  readonly url: string;
  readonly log: string;
  readonly bootToken: string;
  readonly process: Subprocess;
}

/** Waits for the stand-in's `listening on` line and answers its URL, or throws with its stderr
 *  when it exits first. */
async function listening(child: Subprocess<"ignore", "pipe", "pipe">): Promise<string> {
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  let text = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) {
      const code = await child.exited;
      const stderr = await new Response(child.stderr).text();
      throw new Error(`the stand-in exited ${code} before it listened: ${stderr}`);
    }
    text += decoder.decode(value, { stream: true });
    const match = /^listening on (http:\/\/127\.0\.0\.1:\d+)$/m.exec(text);
    if (match?.[1] !== undefined) {
      reader.releaseLock();
      return match[1];
    }
  }
}

/** Starts daemon-standin.ts on a port the kernel picks, with a boot token file of its own, and
 *  runs `body` with it; the process is stopped when `body` ends. */
export async function withStandin(
  dir: string,
  args: readonly string[],
  body: (standin: Standin) => Promise<void>
): Promise<void> {
  const log = path.join(dir, "daemon.log");
  const bootFile = path.join(dir, "boot");
  const bootToken = fixtureSecret("boot");
  writeFileSync(bootFile, `${bootToken}\n`, { mode: 0o600 });
  const child = Bun.spawn([process.execPath, STANDIN, "0", log, bootFile, ...args], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  try {
    const url = await listening(child);
    await body({ url, log, bootToken, process: child });
  } finally {
    child.kill();
    await child.exited;
  }
}

/** The stand-in's exit and stderr for a command line it must refuse before it listens. */
export function refused(dir: string, args: readonly string[]): { code: number; stderr: string } {
  const bootFile = path.join(dir, "boot");
  writeFileSync(bootFile, `${fixtureSecret("boot")}\n`, { mode: 0o600 });
  const result = Bun.spawnSync(
    [process.execPath, STANDIN, "0", path.join(dir, "daemon.log"), bootFile, ...args],
    { stdin: "ignore", stdout: "pipe", stderr: "pipe", timeout: 50_000 }
  );
  return { code: result.exitCode ?? -1, stderr: result.stderr.toString() };
}

export function post(standin: Standin, route: string, body: unknown): Promise<Response> {
  return fetch(`${standin.url}${route}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

/** Every line of the stand-in's log, each held to C3's shape. */
export function logLines(standin: Standin): StandinLogLine[] {
  return readFileSync(standin.log, "utf8")
    .trim()
    .split("\n")
    .map((line) => StandinLogLine.parse(JSON.parse(line)));
}

/** The one log line of `route`, which the row made one request to. */
export function lineOf(standin: Standin, route: string): StandinLogLine {
  const [line, ...others] = logLines(standin).filter((each) => each.path === route);
  expect(others).toHaveLength(0);
  if (line === undefined) throw new Error(`the stand-in's log holds no line of ${route}`);
  return line;
}

/** The claim the stand-in registers for the boot token, as the plugin's boot registers it. */
export async function register(standin: Standin): Promise<LegionRegistration> {
  const response = await post(standin, "/legion/v1/claims/register", {
    bootToken: standin.bootToken,
  });
  expect(response.status).toBe(200);
  return LegionRegisterResponse.parse(await response.json());
}

export const SESSION = "rig-session-1";

/** A grant of the claim, as the `legion` tool mints one before each daemon call. */
export async function grant(standin: Standin, claim: LegionRegistration): Promise<string> {
  const response = await post(standin, "/legion/v1/grants", {
    sessionId: SESSION,
    secret: claim.secret,
    tree: claim.tree,
    issue: claim.issue,
  });
  expect(response.status).toBe(200);
  return LegionGrantResponse.parse(await response.json()).grantId;
}

/** A label checkout with the parts the stand-in reads: its plugin's `package.json`, which states
 *  its contract as a number, or, given `released` and `entries`, states none and leaves it to the
 *  checkout's frozen list and its count of entry fixtures (one file beside them is no entry); and a
 *  stub of the entry at the path the rig calls it by. The stub takes exactly
 *  the entry's interface for `plan-check` (exit 2 for a usage error), keeps the request it was
 *  given as `request-seen.json` beside itself, then: with `planted-no-answer` beside it, prints
 *  its PASS line and exits 0 with no answer file; with `planted-answer.json`, answers that; and
 *  otherwise fails as a run whose test did not pass: lines on stderr, the last of them
 *  `planted-failure.txt`'s, and exit 1. */
export function plantLabel(
  dir: string,
  contract: number | { readonly released: readonly number[]; readonly entries: number }
): { label: string; entryDir: string } {
  const label = path.join(dir, "label");
  const plugin = path.join(label, "packages", "pi-legion");
  const entryDir = path.join(plugin, "scripts", "skill-scenarios");
  mkdirSync(entryDir, { recursive: true });
  if (typeof contract === "number") {
    writeFileSync(
      path.join(plugin, "package.json"),
      JSON.stringify({ name: "@sjawhar/pi-legion", legion: { daemonApiVersion: contract } })
    );
  } else {
    writeFileSync(
      path.join(plugin, "package.json"),
      JSON.stringify({ name: "@sjawhar/pi-legion" })
    );
    const fixtures = path.join(label, "packages", "contracts", "fixtures", "daemon-api");
    mkdirSync(path.join(fixtures, "contracts"), { recursive: true });
    writeFileSync(
      path.join(fixtures, "released-contracts.json"),
      JSON.stringify(contract.released)
    );
    for (let entry = 0; entry < contract.entries; entry++) {
      writeFileSync(
        path.join(fixtures, "contracts", `2026112500000${entry}_entry-${entry}.json`),
        "{}"
      );
    }
    writeFileSync(path.join(fixtures, "contracts", "README.md"), "no entry\n");
  }
  writeFileSync(
    path.join(entryDir, "label-texts.sh"),
    [
      "#!/usr/bin/env bash",
      "set -euo pipefail",
      'if [ "$#" -ne 3 ] || [ "$1" != plan-check ]; then',
      '  echo "usage: label-texts.sh plan-check <request> <answer>" >&2',
      "  exit 2",
      "fi",
      'here=$(cd "$(dirname "$0")" && pwd)',
      'cp "$2" "$here/request-seen.json"',
      'if [ -f "$here/planted-no-answer" ]; then',
      '  echo "--- PASS: TestRigPlanCheck"',
      "  exit 0",
      "fi",
      'if [ -f "$here/planted-answer.json" ]; then',
      '  cp "$here/planted-answer.json" "$3"',
      '  echo "--- PASS: TestRigPlanCheck"',
      "  exit 0",
      "fi",
      'echo "=== RUN   TestRigPlanCheck" >&2',
      'echo "--- FAIL: TestRigPlanCheck" >&2',
      'cat "$here/planted-failure.txt" >&2',
      "exit 1",
      "",
    ].join("\n"),
    { mode: 0o755 }
  );
  return { label, entryDir };
}
