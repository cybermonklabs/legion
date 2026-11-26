/** What `priorities-production.test.ts` and `priority-list-production.test.ts` share, the tests of
 *  the two looks at production whose common steps `e2e/acceptance/lib.sh` holds: the commits and
 *  the Dispatch every row names, the stand-in `gh` and the parts of the stand-ins of `curl` and
 *  `gcloud` both files write alike, an access log line, the `gcloud logging read` a look runs, the
 *  reads of what the stand-ins recorded, and the environment a look runs in. It starts no process;
 *  each test file runs its look itself, through `runBounded`, so `e2e/row-limits.ts` reads every
 *  row that waits for one. Each helper that writes takes a directory of the test file's own
 *  `temporaryDirectories`, whose hooks remove it. */

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { masked } from "./bounded-subprocess";

/** The directory each look runs from. */
export const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** MERGE_SHA, and the commit production's fleet serves in every row that names none of its own. */
export const mergeSha = "4f1c2b9e8d7a6c5b4a39281706f5e4d3c2b1a090";
export const fleetSha = "9a8b7c6d5e4f30211203f4e5d6c7b8a9f0e1d2c3";
/** DISPATCH_URL in every row: a host no row reaches. */
export const dispatchUrl = "https://dispatch.internal.example";
/** The arguments of `gh` for GitHub's compare of MERGE_SHA with `fleetSha`. */
export const fleetCompare = `api repos/cybermonklabs/legion/compare/${mergeSha}...${fleetSha} --jq .status`;
/** LOGGING_PROJECT in every row, a made-up project. */
const loggingProject = "example-logging-project";

/** The inputs both looks require. */
export const requiredInputs: Readonly<Record<string, string>> = {
  DISPATCH_URL: dispatchUrl,
  MERGE_SHA: mergeSha,
  LOGGING_PROJECT: loggingProject,
};

/** What one look printed: its exit code and output, and the lines of its stdout but the empty
 *  ones. */
export interface Printed {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
  readonly lines: readonly string[];
}

/** What `run`, one look's run, printed. */
export function printed(run: {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}): Printed {
  return {
    code: run.code,
    stdout: run.stdout,
    stderr: run.stderr,
    lines: run.stdout.split("\n").filter((line) => line !== ""),
  };
}

/** What a row's failure says of a look: its exit code and all it printed. */
export function said({ code, stdout, stderr }: Printed): string {
  return ` The script exited ${code}. Its stdout read:\n${stdout || "(nothing)"}\nIts stderr read:\n${masked(stderr) || "(nothing)"}`;
}

/** The environment of one look: the stand-ins of `bin` first on PATH, this process's HOME and
 *  TMPDIR, each of `inputs` but an undefined one, and `label` as DISPATCH_E2E_CASE, which the
 *  script never reads and which tells the cost lines apart. */
export function lookEnv(
  label: string,
  bin: string,
  inputs: Readonly<Record<string, string | undefined>>
): Record<string, string> {
  const env: Record<string, string> = {
    DISPATCH_E2E_CASE: label,
    HOME: process.env.HOME ?? "",
    PATH: `${bin}:${process.env.PATH ?? ""}`,
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
  };
  for (const [name, value] of Object.entries(inputs)) {
    if (value !== undefined) env[name] = value;
  }
  return env;
}

/** /healthz's answer: its `fleet_commit` is `fleetCommit`, `fleetSha` where that is undefined,
 *  and null where no one build serves. */
export function healthz(fleetCommit: string | null | undefined): string {
  const fleet = fleetCommit === undefined ? fleetSha : fleetCommit;
  return JSON.stringify({ ok: true, commit: fleet, fleet_commit: fleet });
}

/** The head of a stand-in `curl`: it reads its arguments into `out` (-o), `agent` (-A) and `url`,
 *  and skips the value of each other option a look passes with one. */
export const curlArguments: readonly string[] = [
  "#!/bin/sh",
  "out='' agent='' url=''",
  'while [ "$#" -gt 0 ]; do',
  "  case $1 in",
  "    -o) out=$2; shift ;;",
  "    -A) agent=$2; shift ;;",
  "    -w | -H | --max-time) shift ;;",
  "    -*) ;;",
  "    *) url=$1 ;;",
  "  esac",
  "  shift",
  "done",
];

/** A stand-in `gh` that records its arguments in `dir`/gh-calls, runs `before`, and answers
 *  GitHub's compare with `compare`, `ahead` where that is undefined, or fails as a 502 of GitHub's
 *  does where it is `unanswered`. */
export function gh(
  dir: string,
  compare: string | undefined,
  before: readonly string[] = []
): string {
  return [
    "#!/bin/sh",
    `printf '%s\\n' "$*" >>'${join(dir, "gh-calls")}'`,
    ...before,
    compare === "unanswered"
      ? "echo 'HTTP 502: Bad Gateway (https://api.github.com/repos/cybermonklabs/legion/compare)' >&2; exit 1"
      : `echo '${compare ?? "ahead"}'`,
    "",
  ].join("\n");
}

/** The line of a stand-in `gcloud`, a bash script, that records its arguments in
 *  `dir`/gcloud-calls: `call`, then each argument, each ended by a NUL, which `gcloudCalls` reads
 *  back. */
export function gcloudRecord(dir: string): string {
  return `{ printf 'call\\0'; printf '%s\\0' "$@"; } >>'${join(dir, "gcloud-calls")}'`;
}

/** Writes each stand-in of `scripts` into `dir`/bin, executable, and `log` into `dir`/log, a line
 *  each. Answers `dir`/bin, for the head of PATH. */
export function standInBin(
  dir: string,
  scripts: Readonly<Record<string, string>>,
  log: readonly string[] = []
): string {
  const bin = join(dir, "bin");
  mkdirSync(bin, { recursive: true });
  for (const [name, text] of Object.entries(scripts)) {
    writeFileSync(join(bin, name), text, { mode: 0o755 });
  }
  writeFileSync(join(dir, "log"), log.map((line) => `${line}\n`).join(""));
  return bin;
}

/** The text of a file of `dir`, empty where the file is missing. Any other failure to read it
 *  throws. */
function recordedText(dir: string, name: string): string {
  try {
    return readFileSync(join(dir, name), "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return "";
    throw error;
  }
}

/** The lines a file of `dir` holds, none where the file is missing. */
export function recorded(dir: string, name: string): string[] {
  return recordedText(dir, name).split("\n").slice(0, -1);
}

/** The arguments of each run of the stand-in `gcloud` of `dir`, as `gcloudRecord` wrote them. */
export function gcloudCalls(dir: string): string[][] {
  return recordedText(dir, "gcloud-calls")
    .split("call\0")
    .slice(1)
    .map((call) => call.split("\0").slice(0, -1));
}

/** The arguments of the `gcloud logging read` a look runs for the access log lines that each of
 *  `terms` matches as well, as `access_log` of `e2e/acceptance/lib.sh` builds its filter. */
export function accessLogRead(...terms: readonly string[]): string[] {
  return [
    "logging",
    "read",
    [
      'resource.type="k8s_container"',
      'resource.labels.namespace_name="dispatch"',
      'textPayload:"msg=\\"dispatch: request\\""',
      ...terms,
    ].join(" AND "),
    "--project",
    loggingProject,
    "--format=value(textPayload)",
  ];
}

/** One access log line as Dispatch's server writes it through slog's text handler (the example of
 *  "Access log" in `packages/envoy/cmd/dispatch/README.md`). */
export function requestLine(line: {
  readonly method: string;
  readonly route: string;
  readonly path: string;
  readonly callerKind: string;
  readonly caller: string;
  readonly agent: string;
  readonly status: number;
  readonly elapsed: number;
  readonly bytes: number;
}): string {
  return `time=2026-11-25T09:00:00.000Z level=INFO msg="dispatch: request" method=${line.method} route=${line.route} path=${line.path} caller_kind=${line.callerKind} caller=${line.caller} address=203.0.113.9 peer=10.8.0.12 user_agent=${line.agent} status=${line.status} elapsed_ms=${line.elapsed} bytes=${line.bytes}`;
}

/** `count` of `value`. */
export function times<T>(count: number, value: T): T[] {
  return Array.from({ length: count }, () => value);
}

/** 200 distinct times from 200 down to 1 ms: their p90, the 180th in ascending order, is 180. */
export const distinct = Array.from({ length: 200 }, (_, index) => 200 - index);
