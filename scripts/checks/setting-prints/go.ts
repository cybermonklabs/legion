/**
 * The Go half of the search (`scripts/checks/setting-prints/go/`): running it over every package
 * of the envoy Go module, and reading the records it prints.
 */
import { readdirSync } from "node:fs";
import path from "node:path";
import type { Finding } from "./model";
import { root } from "./program";

const strings = (value: unknown): value is string[] =>
  Array.isArray(value) && value.every((each) => typeof each === "string");

/** A finding as the Go half prints it, read as what it must be, or undefined. */
function goFinding(f: unknown): Finding | undefined {
  if (
    typeof f === "object" &&
    f !== null &&
    "file" in f &&
    typeof f.file === "string" &&
    "line" in f &&
    typeof f.line === "number" &&
    "text" in f &&
    typeof f.text === "string" &&
    "hows" in f &&
    strings(f.hows) &&
    "expressions" in f &&
    strings(f.expressions) &&
    "sources" in f &&
    strings(f.sources) &&
    "ends" in f &&
    strings(f.ends)
  ) {
    const { file, text, hows, expressions, sources, ends } = f;
    return { file, line: f.line, text, hows, expressions, sources, ends };
  }
  return undefined;
}

/** One line of the Go half's output, read as what it must be, or an error naming the line. */
function goRecord(line: string): {
  finding?: Finding;
  quiet?: Finding;
  summary?: string;
  blind?: string[];
} {
  const record: unknown = JSON.parse(line);
  if (typeof record === "object" && record !== null) {
    if ("summary" in record && typeof record.summary === "string")
      return { summary: record.summary };
    if ("blind" in record && strings(record.blind)) return { blind: record.blind };
    const finding = "finding" in record ? goFinding(record.finding) : undefined;
    if (finding !== undefined) return { finding };
    const quiet = "quiet" in record ? goFinding(record.quiet) : undefined;
    if (quiet !== undefined) return { quiet };
  }
  throw new Error(`the Go half printed a line this program cannot read: ${line.slice(0, 200)}`);
}

/** The Go half's files, relative to the root: `go run` takes them by name. */
const goDirectory = "scripts/checks/setting-prints/go";

/** The lines the Go half finds, those of settings that reach no URL, and what its planted shapes
 *  gave, with each package of the module it could not load. It reads every package of the module:
 *  a setting one package reads can be printed by another's line. */
export function goFindings(): {
  findings: Finding[];
  quiet: Finding[];
  summary: string;
  blind: string[];
} {
  const sources = readdirSync(path.join(root, goDirectory))
    .filter((file) => file.endsWith(".go"))
    .sort()
    .map((file) => `${goDirectory}/${file}`);
  const run = Bun.spawnSync(["go", "run", ...sources, "packages/envoy"], {
    cwd: root,
    stderr: "pipe",
    stdout: "pipe",
  });
  const findings: Finding[] = [];
  const quiet: Finding[] = [];
  let summary = "";
  const blind: string[] = [];
  for (const line of run.stdout.toString().split("\n")) {
    if (line === "") continue;
    const record = goRecord(line);
    if (record.finding !== undefined) findings.push(record.finding);
    if (record.quiet !== undefined) quiet.push(record.quiet);
    if (record.summary !== undefined) summary = record.summary;
    if (record.blind !== undefined) blind.push(...record.blind);
  }
  if (run.exitCode !== 0 || summary === "") {
    blind.push(
      `the Go half exited ${run.exitCode}: ${run.stderr.toString().trim().slice(0, 2000)}`
    );
  }
  return { findings, quiet, summary, blind };
}
