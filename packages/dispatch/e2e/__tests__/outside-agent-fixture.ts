/** What `outside-agent-acceptance.test.ts` and `outside-agent-public.test.ts` share: where the
 *  legs are, the rows each prints, and the helpers that copy a leg, make a PATH of text tools and
 *  read a TSV. It starts no leg and no stand-in; each test file runs its legs itself, through
 *  `runBounded`, so `e2e/row-limits.ts` reads every row that waits for one. The one process it runs
 *  is `blankFreeScratch`'s probe of whether a script runs under a base. Each helper that makes a
 *  directory takes a maker of the test file's own, whose directories that file's hooks remove. */

import { chmodSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool } from "@legion/test-fixtures/host-tools";
import {
  baseWithout,
  cleanAncestor,
  moduleRoot,
  temporaryDirectories,
} from "@legion/test-fixtures/temporary-directories";

export const acceptance = fileURLToPath(new URL("../acceptance/", import.meta.url));
export const publicLeg = join(acceptance, "outside-agent-public.sh");
export const tailnetLeg = join(acceptance, "outside-agent-tailnet.sh");
export const codexLeg = join(acceptance, "outside-agent-codex.sh");
/** The support files the legs source from beside them: a leg copied elsewhere takes them along. */
export const supportFiles = [
  "outside-agent-lib.sh",
  "outside-agent-peer.sh",
  "outside-agent-sweep.sh",
  "outside-agent-pins.sh",
  "outside-agent-grants.sh",
];

/** The Codex leg's rows, in order, which the public leg prints after P10. */
export const codexRows = ["C0", "C1", "C2", "C3", "C4", "C5", "C6", "C7"];
/** The public leg's P rows and its Codex leg's C rows, in order. */
export const publicRows = [
  "P1",
  "P2",
  "P3",
  "P4",
  "P5",
  "P6",
  "P7",
  "P8",
  "P9",
  "P10",
  ...codexRows,
];

/** A directory maker of a test file's own: `temporaryDirectories(...)`'s answer. */
export type Scratch = (label: string) => string;

declare const holdsNoBlank: unique symbol;
declare const holdsNoQuestionMark: unique symbol;
/** A `Scratch` whose directories' paths hold no blank, for every path a leg reads from text it
 *  splits on blanks by design: each OUTSIDE_*_COMMAND, and each line of a pins file. */
export type BlankFreeScratch = Scratch & { readonly [holdsNoBlank]: true };
/** A `Scratch` whose directories' paths hold no `?`, for a copy of a leg that has Bun load the
 *  probe beside it: Bun loads no module from a path that holds one. */
export type ModuleScratch = Scratch & { readonly [holdsNoQuestionMark]: true };

/** Why a script in `directory` does not run, or undefined: a `noexec` filesystem takes a
 *  directory and runs nothing in it. The script is gone again when this returns. */
function runsScripts(directory: string): string | undefined {
  const probe = script(join(directory, "runs"), "#!/bin/sh\nexit 0\n");
  try {
    const ran = Bun.spawnSync([probe]);
    return ran.success ? undefined : `a script there exits ${ran.exitCode}`;
  } finally {
    rmSync(probe, { force: true });
  }
}

/** The maker of a test file's directories whose paths hold no blank, and whose scripts run:
 *  under `root` where its path holds none, else its closest ancestor that holds none, else /tmp.
 *  Call it where `temporaryDirectories` is called, at the test file's module scope. */
export function blankFreeScratch(prefix: string, root: string): BlankFreeScratch {
  return temporaryDirectories(prefix, {
    root: baseWithout(/\s/g, [root, ...cleanAncestor(root, /\s/g), "/tmp"], runsScripts),
  }) as BlankFreeScratch;
}

/** The maker of a test file's directories whose paths hold no `?`, under `root` where its path
 *  holds none, else its closest ancestor that holds none, else /tmp. Call it where
 *  `temporaryDirectories` is called, at the test file's module scope. */
export function moduleScratch(prefix: string, root: string): ModuleScratch {
  return temporaryDirectories(prefix, { root: moduleRoot(root) }) as ModuleScratch;
}

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
