/** What `outside-agent-acceptance.test.ts` and `outside-agent-public.test.ts` share: where the
 *  legs are, the rows each prints, and the helpers that copy a leg, make a PATH of text tools and
 *  read a TSV. It starts no process; each test file runs its legs itself, through `runBounded`, so
 *  `e2e/row-limits.ts` reads every row that waits for one. Each helper that makes a directory takes
 *  the test file's own `scratch`, whose directories that file's hooks remove. */

import { chmodSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool } from "@legion/test-fixtures/host-tools";

export const acceptance = fileURLToPath(new URL("../acceptance/", import.meta.url));
export const publicLeg = join(acceptance, "outside-agent-public.sh");
export const tailnetLeg = join(acceptance, "outside-agent-tailnet.sh");
/** The support files the legs source from beside them: a leg copied elsewhere takes them along. */
export const supportFiles = [
  "outside-agent-lib.sh",
  "outside-agent-peer.sh",
  "outside-agent-sweep.sh",
];

/** The public leg's P rows, in order. */
export const publicRows = ["P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9", "P10"];

/** A directory maker of a test file's own: `temporaryDirectories(...)`'s answer. */
export type Scratch = (label: string) => string;

export interface LegRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

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

/** A loopback port that nothing listens on: one the kernel gave a listener this row then closed. */
export function closedPort(): number {
  const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  const { port } = listener;
  listener.stop(true);
  return port;
}

/** A directory holding a link to each text tool this host has, and to each of `more`, for PATH. */
export function textToolPath(scratch: Scratch, more: readonly string[] = []): string {
  const bin = join(scratch("bin"), "bin");
  mkdirSync(bin);
  for (const tool of [...textTools, ...more]) {
    const found = hostTool(tool);
    if (found !== null) symlinkSync(found, join(bin, tool));
  }
  return bin;
}

/** A copy of `leg` in a directory of its own, with the support files it sources beside it, each
 *  through `change`; nothing else it runs beside it (the probe, the sweep, the reach check) is
 *  there. */
export function copyLeg(
  scratch: Scratch,
  leg: string,
  change: (text: string) => string = (text) => text
): string {
  const dir = scratch("leg");
  for (const file of [leg, ...supportFiles.map((name) => join(acceptance, name))]) {
    const copy = join(dir, file.split("/").pop() ?? "leg.sh");
    writeFileSync(copy, change(readFileSync(file, "utf8")));
    chmodSync(copy, 0o755);
  }
  return join(dir, leg.split("/").pop() ?? "leg.sh");
}

/** An executable file at `path` holding `text`. */
export function script(path: string, text: string): string {
  writeFileSync(path, text);
  chmodSync(path, 0o755);
  return path;
}

/** Each row of a leg's TSV by name: its `got` and its verdict. */
export function rowsOf(
  stdout: string
): Map<string, { readonly got: string; readonly verdict: string }> {
  const rows = new Map<string, { got: string; verdict: string }>();
  for (const line of stdout.split("\n").slice(1)) {
    const [name = "", , got = "", verdict = ""] = line.split("\t");
    if (name !== "") rows.set(name, { got, verdict });
  }
  return rows;
}

/** Each row's verdict by name. */
export function verdictsOf(stdout: string): Record<string, string> {
  return Object.fromEntries([...rowsOf(stdout)].map(([name, row]) => [name, row.verdict]));
}
