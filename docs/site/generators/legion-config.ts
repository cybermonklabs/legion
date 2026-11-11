#!/usr/bin/env bun
// Writes Legion's configuration reference, <content dir>/legion/reference/config.md, in two
// halves. The keys of `legion.yaml`: the loader's key table (packages/daemon/internal/config/
// keytable.go), read through `legion config-reference --json`, which prints it from the binary
// scripts/build-binaries.sh built and reads no file and no daemon. The example files: every
// example the repository tracks under deploy/kubernetes - the daemon's legion.yaml, the
// operator-side controller.yaml, the operator's model route - rendered whole, comments included,
// since the comments are the documentation. Only tracked files are read (`git ls-files`), so
// nothing an operator keeps untracked in a checkout reaches the page, and a deploy/kubernetes
// holding no example fails the build rather than writing half a page.
// Contract: scripts/generate.ts.
import { readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { inline, writePage } from "./lib/markdown.ts";

const REPO_ROOT = resolve(import.meta.dir, "../../..");
const EXAMPLES_DIR = "deploy/kubernetes";
/** The vocabulary a row's `required` is written in, the Dispatch settings table's
 *  (packages/envoy/cmd/dispatch/settings.go), which keytable.go's RequiredBy holds itself to. */
const REQUIRED = /^(yes|no|when .+)$/;

/** One nested key of a top-level key, as `legion config-reference --json` prints it: `path` is the
 *  mapping an operator writes it under, with `<KEY>`, `<APP>`, `<POD>` and `<NAME>` for a name the
 *  operator chooses and `[]` for a sequence entry, so `path` and `name` joined with a dot are the
 *  key as it is written. */
interface NestedKey {
  path: string;
  name: string;
  default: string;
  required: string;
  description: string;
}

interface ConfigKey {
  name: string;
  default: string;
  required: string;
  description: string;
  nested: NestedKey[];
}

/** The key as an operator writes it, for a message about a row. A row with no name is named by its
 *  own JSON, since there is nothing else to call it by. */
function written(row: Record<string, unknown>, group: string): string {
  if (typeof row.name !== "string" || row.name === "") return JSON.stringify(row);
  return group === "" ? row.name : `${group}.${row.name}`;
}

/** Refuses a row the page cannot render honestly, naming the key: a `required` outside the
 *  vocabulary, and a missing name, line of prose or default, each of which would reach the table
 *  as a cell saying nothing. The default alone may be empty, which is a key with nothing in its
 *  place. */
function checkRow(row: Record<string, unknown>, where: string): void {
  for (const field of ["name", "description"]) {
    if (typeof row[field] !== "string" || row[field] === "") {
      throw new Error(`key ${where} has no ${field}`);
    }
  }
  if (typeof row.default !== "string") {
    throw new Error(`key ${where} has a default that is not text`);
  }
  if (typeof row.required !== "string" || !REQUIRED.test(row.required)) {
    throw new Error(`key ${where} has required ${JSON.stringify(row.required)}`);
  }
}

/** Runs `legion config-reference --json` from PATH and returns the key table it prints, every row
 *  checked. scripts/generate.ts puts the legion scripts/build-binaries.sh built from this commit
 *  first on PATH; to run this generator alone, build the binaries into a directory and put that
 *  first on PATH:
 *  `docs/site/scripts/build-binaries.sh <dir> && PATH=<dir>:$PATH bun docs/site/generators/legion-config.ts <out>`. */
function readKeys(): ConfigKey[] {
  const binary = Bun.which("legion");
  if (binary === null) {
    throw new Error(
      "legion is not on PATH: run this generator through docs/site/scripts/generate.ts, which builds it (scripts/build-binaries.sh) and puts it there"
    );
  }
  const command = "legion config-reference --json";
  const run = Bun.spawnSync([binary, "config-reference", "--json"], {
    stdout: "pipe",
    stderr: "pipe",
  });
  if (run.exitCode !== 0) {
    throw new Error(`${command} exited ${run.exitCode}:\n${run.stderr.toString()}`);
  }
  const keys = JSON.parse(run.stdout.toString()) as unknown;
  if (!Array.isArray(keys) || keys.length === 0) {
    throw new Error(`${command} printed no keys`);
  }
  for (const key of keys as Record<string, unknown>[]) {
    checkRow(key, written(key, ""));
    if (!Array.isArray(key.nested)) {
      throw new Error(`key ${written(key, "")} has no list of nested keys`);
    }
    for (const nested of key.nested as Record<string, unknown>[]) {
      if (typeof nested.path !== "string" || nested.path === "") {
        throw new Error(
          `a nested key of ${written(key, "")} has no path: ${JSON.stringify(nested)}`
        );
      }
      checkRow(nested, written(nested, nested.path));
    }
  }
  return keys as ConfigKey[];
}

/** A code span of a key's name, safe in a table cell. */
function code(text: string): string {
  return inline(`\`${text}\``, true);
}

/** One row of the key table: what the key is called, what the daemon does without it, whether it
 *  is required, and the one line the key table carries for it. */
function row(name: string, key: ConfigKey | NestedKey): string {
  const required = inline(
    key.required.replace(/^./, (first) => first.toUpperCase()),
    true
  );
  return `| ${name} | ${key.default === "" ? "—" : inline(key.default, true)} | ${required} | ${inline(key.description, true)} |`;
}

function renderKeys(keys: ConfigKey[]): string {
  const nested = keys.reduce((count, key) => count + key.nested.length, 0);
  const lines = [
    "## Keys",
    "",
    `The daemon reads one file, \`legion.yaml\`, whose ${keys.length} top-level keys are below, each followed by the keys of the mappings it carries (${nested} in all). A nested key is written as the table names it: \`runtime.kubernetes.image\` is \`image\` under \`kubernetes\` under \`runtime\`, a \`<KEY>\`, \`<APP>\`, \`<POD>\` or \`<NAME>\` segment is a name you choose, and \`[]\` is an entry of a sequence. **Default** is what the daemon does when the key is absent, and an em dash is a key with nothing in its place.`,
    "",
    "| Key | Default | Required | What it does |",
    "| --- | --- | --- | --- |",
  ];
  for (const key of keys) {
    lines.push(row(`**${code(key.name)}**`, key));
    for (const nested of key.nested) {
      lines.push(row(code(`${nested.path}.${nested.name}`), nested));
    }
  }
  lines.push("");
  return lines.join("\n");
}

/** Every example the repository tracks under deploy/kubernetes. Git's pathspec `*` matches across
 *  `/`, so each pattern reaches every directory below. */
function exampleFiles(): string[] {
  const patterns = ["*.example", "*.yml", "*.yaml"].map((pattern) => `${EXAMPLES_DIR}/${pattern}`);
  const run = Bun.spawnSync(["git", "ls-files", "--", ...patterns], {
    cwd: REPO_ROOT,
    stdout: "pipe",
    stderr: "pipe",
  });
  if (run.exitCode !== 0) {
    throw new Error(`git ls-files exited ${run.exitCode}:\n${run.stderr.toString()}`);
  }
  const files = run.stdout.toString().split("\n").filter(Boolean);
  if (files.length === 0) {
    throw new Error(`${EXAMPLES_DIR} holds no tracked example file`);
  }
  return files;
}

/** The fence a file's contents go in: one backtick more than the file's longest run of backticks,
 *  and at least three, so no line of the file can close the block early. */
function fence(text: string): string {
  const longest = Math.max(0, ...[...text.matchAll(/`+/g)].map((match) => match[0].length));
  return "`".repeat(Math.max(3, longest + 1));
}

function renderExamples(files: string[]): string {
  const lines = [
    "## The example files",
    "",
    "Each file below is rendered whole from the repository when the site was built; its comments are",
    "its documentation. [Running Legion](/legion/legion/running-legion/) says which file goes where and in what",
    "order an operator sets them up.",
    "",
  ];
  for (const file of files) {
    const text = readFileSync(join(REPO_ROOT, file), "utf8");
    const ticks = fence(text);
    // A file whose last line has no newline still closes its fence on a line of its own.
    const contents = text === "" || text.endsWith("\n") ? text : `${text}\n`;
    lines.push(`## \`${file}\``, "", `${ticks}yaml`, `${contents}${ticks}`, "");
  }
  return lines.join("\n");
}

writePage({
  path: "legion/reference/config.md",
  title: "Configuration reference",
  description:
    "Every key of legion.yaml, nested keys included, and the annotated example files Legion ships for an operator, rendered from the repository when the site was built.",
  sources: ["packages/daemon/internal/config/keytable.go", EXAMPLES_DIR],
  body: () => `${renderKeys(readKeys())}\n${renderExamples(exampleFiles())}`,
});
