#!/usr/bin/env bun
// The class search: every class of defect INFRA-764's review rounds found in the rig, each run as
// a named check over the checkout this file lies in, with the rule each check judges a hit by
// (the architect's ruling of 2026-11-18T08:40Z: a committed, rerunnable command beside the
// tester's read search, covering writes, reads and readers, whose output at the push commit goes
// in the pull request's body).
//
//   rig.sh class-search [<checkout>]
//
// prints, for each check, its class, the reading it makes, its rule, the count of its hits and
// each hit as `<file>:<line>` (or the world, route or read it names) with its text cut, and the
// verdict the rule gives it: `instance` (of the class) or `true` (a true statement, with why).
// Then the declared residuals (class-search.json's `residuals`, each with its ruling), apart from
// the findings, and each residual no hit is an instance of any more (stale); then the hand search
// of f853f11b (class-search.json's `searches`), each of its 46 entries mapped to the check that
// runs it, or to the row that holds it, or named as no mechanical search with the reason. Its
// report opens with the free space of the checkout's file system and of $TMPDIR's, read at the
// search's start and at its end (`diskReadings`). It exits 0 when no check has an instance
// outside the residuals and no residual is stale, 1 when one has or one is, 2 when a check could
// not run, and 3 when its report or its error line could not be written whole, naming which on
// the other stream (`emit`).
//
// What it does not check is in packages/pi-envoy/scripts/README.md ("The class search").
import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statfsSync,
  writeFileSync,
  writeSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SyncSubprocess } from "bun";
import { z } from "zod";
import { labelGhSelectors, RIG_GH_TOKEN } from "./daemon-pane";
import { endpointPath, methodOf, type Route, readWorld, routeFor, type World } from "./gh-call";
import { routeOf, WORLD_ROUTES, worldRouteOf } from "./gh-routes";
import { RUN_SELECTORS } from "./gh-selectors";
import { callWrites, GH_COMMANDS } from "./gh-writes";
import { GITHUB_ROUTES, githubRouteOf } from "./github-routes";
import { PRINTS_TOKEN } from "./health";
import { leakCheckEnv } from "./leak-check-env";
import { argvOf, type PinnedRead, pinnedReads, RECORD_READ, shapeDiff } from "./production-shapes";
import { casesOf, SCENARIOS, WORLD_PROJECT, workerWorld } from "./run-names";

/** The rig's directory, under a checkout's root. */
export const RIG = "packages/pi-legion/scripts/skill-scenarios";
/** The checkout this file lies in, which the search reads. */
const CHECKOUT = path.resolve(import.meta.dir, "../../../..");
/** The data file: the residuals and the map of the hand search. */
export const DATA = path.join(import.meta.dir, "class-search.json");

/** What a check found: where (`<file>:<line>`, or the world, route or read it names), the text
 * there, and the verdict its rule gives it, with why. */
export interface Hit {
  readonly at: string;
  readonly text: string;
  readonly verdict: "instance" | "true";
  readonly why: string;
}

/** A checkout's files as the search reads them: the tree under `root`, with `planted` standing
 * in for the files it names (a row plants an instance of a class so). */
export class Tree {
  constructor(
    readonly root: string,
    readonly planted: ReadonlyMap<string, string> = new Map()
  ) {}
  text(file: string): string {
    return this.planted.get(file) ?? readFileSync(path.join(this.root, file), "utf8");
  }
  lines(file: string): string[] {
    return this.text(file).split("\n");
  }
  /** The files under `dir` whose path `keep` takes, every directory below it when `deep`, in
   * byte order; dependencies, builds and version control left out. */
  files(dir: string, keep: (file: string) => boolean, deep = false): string[] {
    const found = new Set<string>();
    const walk = (below: string) => {
      const full = path.join(this.root, below);
      if (!existsSync(full)) return;
      for (const entry of readdirSync(full, { withFileTypes: true })) {
        const file = path.join(below, entry.name);
        if (entry.isDirectory()) {
          if (deep && !["node_modules", ".git", "dist", ".jj"].includes(entry.name)) walk(file);
        } else if (entry.isFile() && keep(file)) found.add(file);
      }
    };
    walk(dir);
    for (const file of this.planted.keys())
      if (file.startsWith(`${dir}/`) && keep(file)) found.add(file);
    return [...found].sort();
  }
}

/** The search's own files, which hold the pattern of every class as text, and the rows' planted
 * instances of each: no grep of the search reads them. */
const OWN = /(?:^|\/)class-search(?:\.test)?\.ts$/;
/** The rig's programs: every file of its directory that is no test, the search's own left out. */
const programs = (tree: Tree, ext = /\.ts$/) =>
  tree.files(RIG, (file) => ext.test(file) && !file.endsWith(".test.ts") && !OWN.test(file));
/** The rig's tests, the search's own left out. */
const tests = (tree: Tree) =>
  tree.files(RIG, (file) => file.endsWith(".test.ts") && !OWN.test(file));
/** `file` under the rig's directory. */
const rig = (file: string) => `${RIG}/${file}`;

/** Whether `line` of `file` is a comment and no code: a TypeScript or Go `//`, `/*` or ` *`
 * line, or a shell `#` line. */
function comment(file: string, line: string): boolean {
  const trimmed = line.trim();
  if (/\.(ts|go|js)$/.test(file)) return /^(\/\/|\/\*|\*)/.test(trimmed);
  if (/\.sh$/.test(file) || path.basename(file) === "rig.sh") return trimmed.startsWith("#");
  return false;
}

/** `text` cut to `width` characters, with `…` where it was cut. */
export function cut(text: string, width = 110): string {
  const one = text.trim().replace(/\s+/g, " ");
  return one.length > width ? `${one.slice(0, width - 1)}…` : one;
}

/** Every line of `files` that `pattern` matches, as `<file>:<line>` and its text; comment lines
 * too only where `comments` says so. */
function grep(
  tree: Tree,
  files: readonly string[],
  pattern: RegExp,
  comments = false
): { file: string; line: number; text: string }[] {
  return files.flatMap((file) =>
    tree
      .lines(file)
      .flatMap((text, index) =>
        pattern.test(text) && (comments || !comment(file, text))
          ? [{ file, line: index + 1, text }]
          : []
      )
  );
}

/** A top-level definition of a TypeScript file and the lines it spans (1-based, inclusive):
 * a function, a `const` holding an arrow function, or the `if (import.meta.main)` block, named
 * `(main)`, which runs only when the file is the program run. */
interface Span {
  readonly name: string;
  readonly start: number;
  readonly end: number;
}
const SPAN_START =
  /^(?:export )?(?:async )?function\*? (\w+)\s*[<(]|^(?:export )?const (\w+)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:\(|\w+\s*=>|<)|^if \(import\.meta\.main\) \{/;
/** The top-level definitions of `text` (a file as biome writes it, a top-level statement at column
 * 0 and everything inside it indented), each from its first line to the line at column 0 that
 * closes it (`}`, `};`, `)`, `});`, but no `): T {` that goes on), or, where none closes it, the
 * last line before the next top-level statement. */
function spans(text: string): Span[] {
  const lines = text.split("\n");
  const found: Span[] = [];
  for (let index = 0; index < lines.length; index++) {
    const start = SPAN_START.exec(lines[index] ?? "");
    if (start === null) continue;
    const name = start[1] ?? start[2] ?? "(main)";
    let end = index;
    for (let next = index + 1; next < lines.length; next++) {
      const line = lines[next] ?? "";
      if (/^[})\]]/.test(line) && !/(?:[{([]|=>)\s*$/.test(line)) {
        end = next;
        break;
      }
      if (/^[^\s})\]]/.test(line)) break;
      if (line.trim() !== "") end = next;
    }
    found.push({ name, start: index + 1, end: end + 1 });
    index = end;
  }
  return found;
}
/** The span of `spans` that holds `line`, undefined for a line of the file's top level. */
const spanAt = (all: readonly Span[], line: number) =>
  all.find((one) => one.start <= line && line <= one.end);
/** The text of `span` in `lines`. */
const spanText = (lines: readonly string[], span: Span) =>
  lines.slice(span.start - 1, span.end).join("\n");

// ---------------------------------------------------------------------------------------------
// The worlds rig.sh builds, and the gh stand-in that answers them.

/** A world rig.sh's worker_fixture built, as the gh stand-in reads it: its name (the scenario
 * and case it is built for), its directory, its routes and its world.json. */
export interface BuiltWorld {
  readonly name: string;
  readonly dir: string;
  readonly routes: (Route & { readonly graph?: unknown; readonly answer?: string })[];
  readonly world: World;
}

/** Every function definition of the shell `script` at the start of a line, one-line ones too,
 * each read to the `}` alone that closes it with the lines of each here-document it holds
 * skipped (test-programs.ts's `scriptDefinition` reads one so). */
function scriptDefinitions(script: string): string[] {
  const lines = script.split("\n");
  const found: string[] = [];
  for (let index = 0; index < lines.length; index++) {
    const start = /^([A-Za-z_][A-Za-z0-9_]*)\(\) \{(.*)$/.exec(lines[index] ?? "");
    if (start === null) continue;
    if ((start[2] ?? "").trim() !== "" && /\}\s*$/.test(start[2] ?? "")) {
      found.push(lines[index] ?? "");
      continue;
    }
    let delimiter: string | undefined;
    let end = index + 1;
    for (; end < lines.length; end++) {
      const line = lines[end] ?? "";
      if (delimiter !== undefined) {
        if (line.trim() === delimiter) delimiter = undefined;
      } else if (line === "}") break;
      else delimiter = /(?<!<)<<(?!<)-?\s*(['"]?)(\w+)\1/.exec(line)?.[2];
    }
    found.push(lines.slice(index, end + 1).join("\n"));
    index = end;
  }
  return found;
}

/** The shell a script of the tree's rig.sh starts with to run the rig's own functions: `set -euo
 * pipefail`, every function of the script defined as the script writes it, and its world's values
 * (`worker_repo`, `worker_base`, `world_*`, `text_finding_*`) as it assigns them. */
export function rigShell(tree: Tree): string[] {
  const script = tree.text(rig("rig.sh"));
  const values = script.match(
    /^(?:worker_repo|worker_base|world_[a-z_]+|text_finding_[a-z_]+)=.*$/gm
  );
  return ["set -euo pipefail", ...scriptDefinitions(script), ...(values ?? [])];
}

/** Linux's limit on one argument of a process (MAX_ARG_STRLEN, 32 pages): a longer one fails the
 * spawn with E2BIG. */
export const ARGUMENT_LIMIT = 131_072;
let rigScripts = 0;
/** Runs `lines`, a script of the rig's own shell (`rigShell`), with `args` as its positional
 * parameters and `env` as its whole environment, from a file written under `dir` and removed
 * after: never as `bash -c`'s one argument, since rig.sh's functions run past ARGUMENT_LIMIT
 * (about 300 KB), which a runner's kernel refuses with E2BIG (CI run 44545489442) where a
 * gVisor pod's let it through. Not on standard input either, which a command of the script that
 * reads its standard input would read the rest of the script from. */
export function runRigShell(
  dir: string,
  lines: readonly string[],
  args: readonly string[],
  env: Record<string, string>
): SyncSubprocess<"pipe", "pipe"> {
  const file = path.join(dir, `rig-shell-${process.pid}-${++rigScripts}.sh`);
  writeFileSync(file, `${lines.join("\n")}\n`);
  try {
    return Bun.spawnSync(rigShellArgv(file, args), { env, stdout: "pipe", stderr: "pipe" });
  } finally {
    rmSync(file, { force: true });
  }
}
/** The argument list `runRigShell` runs the script `file` with. */
export function rigShellArgv(file: string, args: readonly string[]): string[] {
  return ["bash", file, ...args];
}

/** The worlds rig.sh builds, each built under `into` by rig.sh's own worker_fixture, for every
 * case of every scenario run-names.ts's SCENARIOS gives a worker world (its one case, "", for a
 * scenario of none), in the rig's own shell (`rigShell`), with fixture_workspace alone left out,
 * since it clones the remote into the issue workspace with jj and writes neither fixtures.json nor
 * world.json. Each world is given the checkout's gh
 * selectors (gh-selectors.ts's RUN_SELECTORS), as worker_boot gives them, without which the gh
 * stand-in answers no call. Each build runs under the caller's PATH, a HOME of its own and the
 * caller's PUBLIC_LEAK_CHECK, as a pane carries it (leak-check-env.ts): its pushes to the world's
 * remote.git meet the guard of the `git` on PATH, which on a pod finds the shared check through
 * that variable. */
export function buildWorlds(tree: Tree, into: string): BuiltWorld[] {
  const shell = rigShell(tree);
  const selectors = `${JSON.stringify(labelGhSelectors(tree.root))}\n`;
  const built: BuiltWorld[] = [];
  for (const [scenario, { cases }] of Object.entries(SCENARIOS)) {
    if (!workerWorld(scenario)) continue;
    for (const one of cases.length === 0 ? [""] : casesOf(scenario)) {
      const name = one === "" ? scenario : `${scenario}-${one}`;
      const dir = path.join(into, name);
      mkdirSync(dir, { recursive: true });
      const made = runRigShell(
        into,
        [...shell, "fixture_workspace() { :; }", "R=$1 worker_key=$2", 'worker_fixture 1 100 "$3"'],
        [dir, `${WORLD_PROJECT}-1`, one],
        { PATH: process.env.PATH ?? "", HOME: into, ...leakCheckEnv() }
      );
      if (made.exitCode !== 0)
        throw new Error(`rig.sh's worker_fixture of ${name}: ${made.stderr.toString().trim()}`);
      writeFileSync(path.join(dir, RUN_SELECTORS), selectors);
      built.push(worldOfDir(name, dir));
    }
  }
  return built;
}

/** The world under `dir` as the gh stand-in reads it: its fixtures.json, and its world.json read
 * by gh-call.ts's `readWorld`, the one reader of it. */
export function worldOfDir(name: string, dir: string): BuiltWorld {
  const { routes } = JSON.parse(readFileSync(path.join(dir, "fixtures.json"), "utf8"));
  const read = readWorld(dir);
  if ("why" in read) throw new Error(`${dir}/world.json is no world: ${read.why}`);
  return { name, dir, routes, world: read.world };
}

/** What the gh stand-in of the tree answered a call, and its record of it (calls.jsonl's last
 * line: the route, the handler and the exit). */
interface Answered {
  readonly code: number;
  readonly out: string;
  readonly err: string;
  readonly handler: string;
}
/** Runs the tree's gh stand-in as `gh <argv...>` in the world under `dir`, from a pane whose gh
 * configuration is `config` (a `hosts.yml`, as the daemon renders a role's claim) and which
 * carries the caller's PUBLIC_LEAK_CHECK, as a pane does (leak-check-env.ts). */
async function gh(
  tree: Tree,
  dir: string,
  config: string,
  argv: readonly string[]
): Promise<Answered> {
  const child = Bun.spawn(
    [process.execPath, path.join(tree.root, rig("gh-standin.ts")), "gh", ...argv],
    {
      env: {
        PATH: process.env.PATH ?? "",
        HOME: dir,
        GH_DATA_DIR: dir,
        GH_CONFIG_DIR: config,
        ...leakCheckEnv(),
      },
      stdout: "pipe",
      stderr: "pipe",
      stdin: "ignore",
    }
  );
  const [out, err, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  const record = readFileSync(path.join(dir, "calls.jsonl"), "utf8").trim().split("\n").at(-1);
  const handler = record === undefined ? "" : String(JSON.parse(record).handler ?? "");
  return { code, out, err, handler };
}

/** `fn` over `items`, `limit` at a time, in their order. */
async function limited<T, R>(items: readonly T[], limit: number, fn: (item: T) => Promise<R>) {
  const results: R[] = new Array(items.length);
  let next = 0;
  await Promise.all(
    Array.from({ length: Math.min(limit, items.length) }, async () => {
      while (next < items.length) {
        const at = next++;
        results[at] = await fn(items[at] as T);
      }
    })
  );
  return results;
}

/** The worlds a search reads, built once, with a pane's gh configuration and a scratch of the
 * search's own, which `done` removes. */
export class Worlds {
  private built: BuiltWorld[] | undefined;
  readonly scratch: string;
  readonly config: string;
  private copies = 0;
  constructor(
    readonly tree: Tree,
    given?: BuiltWorld[]
  ) {
    this.scratch = mkdtempSync(path.join(os.tmpdir(), "class-search-"));
    this.config = path.join(this.scratch, "pane-gh");
    mkdirSync(this.config);
    // The token every pane's gh configuration holds (daemon-pane.ts's RIG_GH_TOKEN), which the
    // world's gh names and never reads.
    writeFileSync(
      path.join(this.config, "hosts.yml"),
      `github.com:\n    oauth_token: ${RIG_GH_TOKEN}\n    user: x-access-token\n    git_protocol: https\n`
    );
    this.built = given;
  }
  all(): BuiltWorld[] {
    this.built ??= buildWorlds(this.tree, path.join(this.scratch, "worlds"));
    return this.built;
  }
  /** A copy of `world`'s directory, for one call to write into. */
  copy(world: BuiltWorld): string {
    const dir = path.join(this.scratch, `copy-${++this.copies}`);
    cpSync(world.dir, dir, { recursive: true });
    return dir;
  }
  /** `gh <argv...>` in a fresh copy of `world`, and that copy. */
  async call(world: BuiltWorld, argv: readonly string[]) {
    const dir = this.copy(world);
    return { dir, answered: await gh(this.tree, dir, this.config, argv) };
  }
  /** `gh <argv...>` in the world directory `dir` as it stands. */
  in(dir: string, argv: readonly string[]) {
    return gh(this.tree, dir, this.config, argv);
  }
  done(): void {
    rmSync(this.scratch, { recursive: true, force: true });
  }
}

/** The reads a world's effect is seen by: the pull request as gh views it, its reviews, its review
 * comments, its conversation and its review threads, each as the stand-in answers them. */
function observers(world: BuiltWorld): string[][] {
  const [owner, name] = world.world.repo.split("/");
  const pulls = `repos/${world.world.repo}/pulls/${world.world.pr}`;
  return [
    ["pr", "view", String(world.world.pr), "--json", "body,comments,reviews,reviewDecision,state"],
    ["api", `${pulls}/reviews`],
    ["api", `${pulls}/comments`],
    ["api", `repos/${world.world.repo}/issues/${world.world.pr}/comments`],
    [
      "api",
      "graphql",
      "-f",
      `query=query{repository(owner:"${owner}",name:"${name}"){pullRequest(number:${world.world.pr}){reviewThreads(first:100){nodes{id isResolved comments(first:100){nodes{databaseId body}}}}}}}`,
    ],
  ];
}
/** What each observer reads in the world directory `dir`. */
async function observed(worlds: Worlds, world: BuiltWorld, dir: string): Promise<string[]> {
  const read: string[] = [];
  for (const argv of observers(world)) {
    const got = await worlds.in(dir, argv);
    read.push(`${got.code}\n${got.out}`);
  }
  return read;
}
/** The world's reads before any call (each observer in a pristine copy), once a world; a read of
 * the world that changes what a later read answers would leave no write judged, so it fails the
 * search. */
const baselines = new WeakMap<BuiltWorld, Promise<string[]>>();
function baseline(worlds: Worlds, world: BuiltWorld): Promise<string[]> {
  const known = baselines.get(world);
  if (known !== undefined) return known;
  const made = (async () => {
    const dir = worlds.copy(world);
    const before = await observed(worlds, world, dir);
    const again = await observed(worlds, world, dir);
    if (again.join("\0") !== before.join("\0"))
      throw new Error(`${world.name}: a read of the world changed what a read answers`);
    return before;
  })();
  baselines.set(world, made);
  return made;
}

/** The text a call of the world sends: a marker no world holds. */
const SENT = "class-search-probe";

/** The verdict on one write sent to a world: refused (exit ≠ 0, with the handler that refused
 * it), or answered with a success, which is modelled when a read of the world answers otherwise
 * after it (the effect every later read replays from the record) and an instance of the class
 * when no read changes. */
async function judgedWrite(
  worlds: Worlds,
  world: BuiltWorld,
  argv: readonly string[],
  at: string,
  text: string
): Promise<Hit> {
  const shown = `gh ${argv.join(" ")} (${text})`;
  const { dir, answered } = await worlds.call(world, argv);
  if (answered.code !== 0)
    return {
      at,
      text: shown,
      verdict: "true",
      why: `refused: exit ${answered.code}, handler ${answered.handler}`,
    };
  const before = await baseline(worlds, world);
  const after = await observed(worlds, world, dir);
  const changed = observers(world).filter((_, index) => before[index] !== after[index]);
  return changed.length > 0
    ? {
        at,
        text: shown,
        verdict: "true",
        why: `modelled: handler ${answered.handler}, and \`gh ${cut(changed[0]?.join(" ") ?? "", 50)}\` reads its effect`,
      }
    : {
        at,
        text: shown,
        verdict: "instance",
        why: `a success (exit 0, handler ${answered.handler}) that changes nothing a read of the world answers`,
      };
}

/** One shortest text the regex `source` matches, for each choice of its alternatives, with each
 * `[0-9]+` read as each of `ids`: the forms rig.sh writes a route's `match` and `endpoint` in
 * (literals, `\x`, `.*`, `[…]`, `(…|…)`, `?`, `*`, `+`). */
export function instancesOf(source: string, ids: readonly string[]): string[] {
  const choices = (at: number, end: string): [string[], number] => {
    let texts = [""];
    let index = at;
    const append = (options: string[]) => {
      texts = texts.flatMap((text) => options.map((option) => text + option));
    };
    while (index < source.length && !end.includes(source[index] ?? "")) {
      const char = source[index] ?? "";
      let options: string[];
      if (char === "\\") {
        options = [source[index + 1] ?? ""];
        index += 2;
      } else if (char === "(") {
        const alternatives: string[] = [];
        index++;
        if (source.startsWith("?:", index)) index += 2;
        for (;;) {
          const [inner, after] = choices(index, "|)");
          alternatives.push(...inner);
          index = after;
          if (source[index] === ")") break;
          index++;
        }
        index++;
        options = alternatives;
      } else if (char === "[") {
        const close = source.indexOf("]", index + 1);
        const set = source.slice(index + 1, close);
        index = close + 1;
        options =
          set === "0-9"
            ? [...ids]
            : set.startsWith("^")
              ? ["x"]
              : [set.replace(/\\/g, "")[0] ?? ""];
      } else if (char === ".") {
        options = ["x"];
        index++;
      } else if (char === "^" || char === "$") {
        options = [""];
        index++;
      } else {
        options = [char];
        index++;
      }
      const quantifier = source[index];
      if (quantifier === "?" || quantifier === "*") {
        options = [""];
        index++;
      } else if (quantifier === "+") index++;
      append(options);
    }
    return [texts, index];
  };
  return [...new Set(choices(0, "")[0])];
}

/** The ids a world holds, which a route's `[0-9]+` is read as: its reviews' and its review
 * comments' (fixture_write's REVIEWS and THREADS), and 1, which it holds none of. */
function idsOf(world: BuiltWorld): string[] {
  const text = JSON.stringify(world.routes);
  const ids = [...text.matchAll(/"(?:id|databaseId)":(\d{4})\b/g)].map(([, id]) => id ?? "");
  return [...new Set([...ids, "1"])];
}

/** The methods HTTP defines as safe (RFC 9110, 9.2.1), which change nothing of the server's: a
 * route takes a write where it names another, or names none and so answers every verb (gh-call.ts's
 * `routeFor`). */
const SAFE = ["GET", "HEAD", "OPTIONS", "TRACE"];

/** The writes that reach `route` of `world`: each write verb it takes, sent to each path its
 * `endpoint` (or its `match`) reads as, with a body alone and with a body and the fields a review
 * or an inline comment takes; each mutation its graph holds, and one it does not; and, for a gh
 * command of its own, the command, where gh v2.98.0's source reads it as a write
 * (gh-writes.ts's `callWrites`), with a body. Undefined for a route no write reaches, with why. */
function writesTo(
  world: BuiltWorld,
  route: BuiltWorld["routes"][number]
): string[][] | { none: string } {
  const { repo, pr, head } = world.world;
  const body = ["-f", `body=${SENT}`];
  const fields = [...body, "-f", `commit_id=${head}`, "-f", "path=greet.ts", "-F", "line=1"];
  const verbs = (route.methods ?? ["POST", "PUT", "PATCH", "DELETE"]).filter(
    (verb) => !SAFE.includes(verb.toUpperCase())
  );
  if (route.endpoint === "graphql") {
    const held = Object.keys(
      (route.graph as { mutation?: Record<string, unknown> } | undefined)?.mutation ?? {}
    );
    const thread = /"id":"(PRRT_\w+)"/.exec(JSON.stringify(route.graph))?.[1] ?? "PRRT_none";
    return [...held, "addComment"].map((field) => [
      "api",
      "graphql",
      "-f",
      `query=mutation{${field}(input:{threadId:"${thread}",pullRequestReviewThreadId:"${thread}",subjectId:"${thread}",body:"${SENT}"}){clientMutationId}}`,
    ]);
  }
  const words = instancesOf(route.match, idsOf(world)).map((text) =>
    text.trim().split(/\s+/).slice(1)
  );
  if (route.endpoint !== undefined || words.some((argv) => argv[0] === "api")) {
    const paths =
      route.endpoint === undefined
        ? words.map(
            (argv) =>
              argv.find((word, at) => at > 0 && word.includes("/")) ?? `repos/${repo}/${SENT}`
          )
        : instancesOf(route.endpoint, idsOf(world));
    return [...new Set(paths)].flatMap((target) =>
      verbs.flatMap((verb) => [
        ["api", "--method", verb, target, ...body],
        ["api", "--method", verb, target, ...fields, "-f", "event=COMMENT"],
      ])
    );
  }
  const commands = words.filter((argv) => callWrites({ argv }));
  if (commands.length === 0)
    return {
      none: `no write reaches it: \`gh ${words.map((argv) => argv.join(" ")).join("`, `gh ")}\` writes nothing as gh v2.98.0's source reads it (gh-writes.json)`,
    };
  return [...new Set(commands.map((argv) => JSON.stringify(argv)))].map((argv) => [
    ...(JSON.parse(argv) as string[]),
    String(pr),
    "--body",
    SENT,
  ]);
}

// ---------------------------------------------------------------------------------------------
// The checks.

/** A named check: its class, the reading it makes, the rule its verdict on a hit is given by,
 * and its run over the tree (and the worlds the tree's rig builds). */
export interface Check {
  readonly name: string;
  readonly class: string;
  readonly reading: string;
  readonly rule: string;
  readonly run: (tree: Tree, worlds: Worlds) => Hit[] | Promise<Hit[]>;
}

/** A check that greps `files` for `pattern` and judges each code line it matches by `verdict`. */
function grepCheck(spec: {
  name: string;
  class: string;
  files: (tree: Tree) => string[];
  pattern: RegExp;
  reading: string;
  rule: string;
  comments?: boolean;
  verdict: (
    hit: { file: string; line: number; text: string },
    tree: Tree
  ) => Omit<Hit, "at" | "text">;
}): Check {
  return {
    name: spec.name,
    class: spec.class,
    reading: spec.reading,
    rule: spec.rule,
    run: (tree) =>
      grep(tree, spec.files(tree), spec.pattern, spec.comments).map((hit) => ({
        at: `${hit.file}:${hit.line}`,
        text: hit.text,
        ...spec.verdict(hit, tree),
      })),
  };
}
const instance = (why: string) => ({ verdict: "instance" as const, why });
const holds = (why: string) => ({ verdict: "true" as const, why });

/** The daemon's role prompts, which no check of the search reads (dispatch://INFRA-764 R4). */
const PROMPT_TREE = /^packages\/daemon\/internal\/prompts\/(?:roles|go)\//;
/** Every text file of the trees a citation can sit in. */
const TEXT_FILE = /\.(ts|tsx|js|go|sh|md|mdx|yml|yaml|json|txt|toml)$/;

// The writes.

/** The route of a world fixture_write and the world builders write, as one line. */
const routeLine = (route: BuiltWorld["routes"][number]) =>
  `${route.methods?.join(",") ?? "*"} ${route.match}${route.endpoint === undefined ? "" : ` (endpoint ${route.endpoint})`}`;

const writeRoutes: Check = {
  name: "writes: every write route of every world is modelled or refused",
  class: "A world route that answers a write with a success it does not model.",
  reading:
    "each world rig.sh's worker_fixture builds, its fixtures.json read whole: every route that takes a write verb or names none, sent each write that reaches it, in a copy of the world, through the gh stand-in",
  rule: "a write refused (exit ≠ 0) is a true statement; a success is modelled where a read of the world (gh pr view, the reviews, the review comments, the conversation, the review threads) answers otherwise after it; a success no read sees is an instance. A route met in several worlds is sent its writes in the first.",
  run: async (_, worlds) => {
    const seen = new Map<string, string>();
    const jobs: (() => Promise<Hit>)[] = [];
    const hits: Hit[] = [];
    for (const world of worlds.all())
      world.routes.forEach((route, index) => {
        if (route.methods?.every((verb) => SAFE.includes(verb.toUpperCase())) === true) return;
        const key = JSON.stringify(route);
        const at = `${world.name}: fixtures.json routes[${index}]`;
        if (seen.has(key)) return;
        seen.set(key, world.name);
        const writes = writesTo(world, route);
        if ("none" in writes) hits.push({ at, text: routeLine(route), ...holds(writes.none) });
        else
          for (const argv of writes)
            jobs.push(() => judgedWrite(worlds, world, argv, at, `route ${routeLine(route)}`));
      });
    return [...hits, ...(await limited(jobs, 2, (job) => job()))];
  },
};

/** The world that holds the most routes, where a call no route of any world serves is sent. */
const richest = (worlds: Worlds) =>
  worlds.all().reduce((one, other) => (other.routes.length > one.routes.length ? other : one));

const ghWriteCommands: Check = {
  name: "writes: every gh write command the world serves is modelled",
  class: "A world route that answers a write with a success it does not model.",
  reading:
    "gh-writes.json (gh v2.98.0's commands read out of its source by gh-writes.go): each command that writes and runs, called with the world's pull request and a body, in each world a route of serves it (gh-call.ts's `routeFor`, the stand-in's own matcher), else in the world of the most routes",
  rule: "as the write routes': refused is a true statement, a success a read of the world sees is modelled, a success no read sees is an instance",
  run: async (_, worlds) => {
    const jobs: (() => Promise<Hit>)[] = [];
    for (const [command, { writes, runs, byRequest }] of Object.entries(GH_COMMANDS.commands)) {
      if (!writes || !runs || byRequest === true) continue;
      const argv = (world: BuiltWorld) => [
        ...command.split(" "),
        String(world.world.pr),
        "--body",
        SENT,
      ];
      const serving = worlds
        .all()
        .filter((world) => routeFor(world.routes, argv(world)) !== undefined);
      for (const world of serving.length > 0 ? serving : [richest(worlds)])
        jobs.push(() =>
          judgedWrite(
            worlds,
            world,
            argv(world),
            `gh-writes.json: ${command}`,
            serving.length > 0 ? `a route of ${world.name} serves it` : "no world's route serves it"
          )
        );
    }
    return limited(jobs, 2, (job) => job());
  },
};

const noCatchAll: Check = {
  name: "writes: no route of any world is a catch-all",
  class: "A world route that answers a write with a success it does not model (a catch-all).",
  reading:
    "each world's fixtures.json, matched by gh-call.ts's `routeFor` (the stand-in's own matcher) against a call of every verb to a path of the world's repository the world holds nothing at, and a gh command and a gh pr command gh has not got",
  rule: "any route that answers such a call is an instance: it answers what the world holds nothing of",
  run: (_, worlds) =>
    worlds.all().flatMap((world) => {
      const nowhere = `repos/${world.world.repo}/${SENT}/1`;
      const calls = [
        ["api", nowhere],
        ...["POST", "PUT", "PATCH", "DELETE"].map((verb) => [
          "api",
          "--method",
          verb,
          nowhere,
          "-f",
          `body=${SENT}`,
        ]),
        [SENT],
        ["pr", SENT, String(world.world.pr)],
      ];
      return calls.flatMap((argv) => {
        const route = routeFor(world.routes, argv);
        return route === undefined
          ? []
          : [
              {
                at: `${world.name}: fixtures.json routes[${world.routes.indexOf(route)}]`,
                text: `${routeLine(route)} ← gh ${argv.join(" ")}`,
                ...instance("a route answers a call to what the world holds nothing of"),
              },
            ];
      });
    }),
};

/** appauth's permission sets, one an App: what each App's token is minted with. */
const PERMISSIONS = "packages/daemon/internal/appauth/permissions.go";
/** The world role whose pane acts as each App of appauth's sets (identity.go's `AppRoleFor`: the
 * implementer and the merger are the implement App's, every other role the review App's). */
const APP_ROLES: Readonly<Record<string, string>> = {
  Implement: "implementer",
  Review: "reviewer",
};
/** The workflow file a push of the check changes, the one the round worlds carry. */
const WORKFLOW = ".github/workflows/greet.yml";

/** Each App of appauth's writePermissions as the map names it (`Implement`, `Review`), and whether
 * its set holds `workflows: write`, read off the map's lines. */
export function workflowGrants(tree: Tree): { app: string; workflows: boolean }[] {
  const lines = tree.lines(PERMISSIONS);
  const start = lines.findIndex((line) => line.startsWith("var writePermissions = map[AppRole]"));
  const grants: { app: string; workflows: boolean }[] = [];
  for (const line of start < 0 ? [] : lines.slice(start + 1)) {
    const entry = /^\t(\w+):\s+\{(.*)\},$/.exec(line);
    if (entry === null) break;
    grants.push({ app: entry[1] ?? "", workflows: /"workflows": "write"/.test(entry[2] ?? "") });
  }
  if (grants.length === 0) throw new Error(`${PERMISSIONS} holds no writePermissions entry`);
  return grants;
}

/** The push ROLE's App makes of a commit adding a line to WORKFLOW over the pull request's branch
 * of a copy of `world`, through the issue workspace's remote helper rig.sh writes for ROLE
 * (world_remote, then world_remote_app, as worker_boot writes it before ROLE's agent starts), from
 * a clone of the world's remote whose origin names that helper (`remote.origin.vcs`): its exit
 * and git's stderr. */
function appPush(tree: Tree, worlds: Worlds, world: BuiltWorld, role: string) {
  const dir = worlds.copy(world);
  const env = { PATH: process.env.PATH ?? "", HOME: worlds.scratch, ...leakCheckEnv() };
  const made = runRigShell(
    worlds.scratch,
    [
      ...rigShell(tree),
      'R=$1 work=$1/push-work && world_remote && world_remote_app "$2"',
      'git clone -q -b "$3" "$R/remote.git" "$work"',
      'git -C "$work" config remote.origin.vcs "$world_remote_vcs"',
      'mkdir -p "$(dirname "$work/$4")" && printf "# %s\\n" "$5" >>"$work/$4" && git -C "$work" add -A',
      'git -C "$work" -c user.name=probe -c user.email=probe@example.invalid commit -qm "$5"',
    ],
    [dir, role, world.world.branch, WORKFLOW, SENT],
    env
  );
  if (made.exitCode !== 0)
    throw new Error(`${world.name}: the ${role}'s push could not be made: ${made.stderr}`);
  const pushed = Bun.spawnSync(
    [
      "git",
      "-C",
      path.join(dir, "push-work"),
      "push",
      "origin",
      `HEAD:refs/heads/${world.world.branch}`,
    ],
    { env: { ...env, PATH: `${path.join(dir, "bin")}:${env.PATH}` } }
  );
  return { code: pushed.exitCode, err: pushed.stderr.toString() };
}

const appPushes: Check = {
  name: "writes: each App's workflow push is answered as GitHub answers that App",
  class: "A push the world's remote answers otherwise than GitHub answers the App that makes it.",
  reading: `appauth's writePermissions (${PERMISSIONS}): whether each App's set holds \`workflows: write\`; then, in a copy of each world rig.sh's worker_fixture builds, a \`git push\` of a commit adding a line to ${WORKFLOW} over the pull request's branch, through the remote helper rig.sh writes for the role that App's token serves (world_remote_app; identity.go's AppRoleFor)`,
  rule: "the push of an App holding `workflows: write` taken, and the push of an App without it refused with GitHub's ` ! [remote rejected] <src> -> <branch> (refusing to allow a GitHub App to create or update workflow `<path>` without `workflows` permission)`, are true statements; any other answer is an instance",
  run: (tree, worlds) =>
    worlds.all().flatMap((world) =>
      workflowGrants(tree).map(({ app, workflows }) => {
        const role = APP_ROLES[app];
        if (role === undefined)
          throw new Error(
            `${PERMISSIONS} names the App ${app}, which no role of the world acts as`
          );
        const { code, err } = appPush(tree, worlds, world, role);
        const refusal = ` ! [remote rejected] HEAD -> ${world.world.branch} (refusing to allow a GitHub App to create or update workflow \`${WORKFLOW}\` without \`workflows\` permission)`;
        const refused = err.split("\n").some((line) => line.trimEnd() === refusal);
        const at = `${world.name}: the ${app} App's push of ${WORKFLOW}`;
        const text = `git push as the ${role} (${app} App): exit ${code}${refused ? ", GitHub's refusal" : ""}`;
        if (workflows)
          return code === 0
            ? {
                at,
                text,
                ...holds("taken, as GitHub takes the push of an App holding `workflows: write`"),
              }
            : {
                at,
                text,
                ...instance(`refused, where the App holds \`workflows: write\`: ${cut(err)}`),
              };
        if (code !== 0 && refused)
          return { at, text, ...holds("refused with GitHub's line: the App holds no `workflows`") };
        return {
          at,
          text,
          ...instance(
            code === 0
              ? "taken, where GitHub refuses an App without `workflows` a push that changes a workflow"
              : `refused in other words than GitHub's: ${cut(err)}`
          ),
        };
      })
    ),
};

// The reads.

/** A read a stand-in of the world serves: its route, the stand-in, and the pinned reads that ask
 * production the same. */
export interface ServedRead {
  readonly route: string;
  readonly by: string;
}
/** Every GET the world serves: each read of the gh world's table (gh-routes.ts's WORLD_ROUTES: a
 * REST GET, a GraphQL query, a gh command that reads), each GET of the GitHub stand-in Dispatch
 * reads (github-routes.ts's GITHUB_ROUTES), and each GET the daemon stand-in answers, read off
 * daemon-standin.ts's own `request.method === "GET" && path === …` tests. */
export function servedReads(tree: Tree): ServedRead[] {
  const gh = WORLD_ROUTES.filter(({ route }) => {
    if (route.startsWith("GET ") || route.startsWith("GRAPHQL query ")) return true;
    if (!route.startsWith("GH ")) return false;
    return routeOf(route.split(" ").slice(1))?.kind === "read";
  }).map(({ route }) => ({ route, by: "gh stand-in (gh-routes.ts's WORLD_ROUTES)" }));
  const http = GITHUB_ROUTES.filter(({ route }) => route.startsWith("GET ")).map(({ route }) => ({
    route,
    by: "GitHub stand-in (github-routes.ts's GITHUB_ROUTES)",
  }));
  const daemon = [
    ...tree
      .text(rig("daemon-standin.ts"))
      .matchAll(/request\.method === "GET" && path === "([^"]+)"/g),
  ].map(([, route]) => ({ route: `GET ${route}`, by: "daemon stand-in (daemon-standin.ts)" }));
  return [...gh, ...http, ...daemon];
}
/** The world's values a pinned read's placeholders are filled with when its command is read for
 * the route it asks (production-shapes.ts's `argvOf`). */
const PLACES: Record<string, string> = {
  owner: "fernbrook",
  repo: "widgets",
  number: "1001",
  head: "0123456789abcdef0123456789abcdef01234567",
  base: "main",
  review: "5001",
  comment: "7001",
  path: "greet.ts",
  id: "1",
  branch: "legion/WID-1",
  app: "widgets-implementer",
};
/** The routes each pinned read asks production for: a gh read's route of the gh world's table
 * (gh-routes.ts's `routeOf`, `worldRouteOf`), a `gh api` read's route of the GitHub stand-in's
 * (github-routes.ts's `githubRouteOf`, its media type from `-H Accept:`), and the issue record
 * (production-shapes.ts's RECORD_READ), which `legion read_record` reads from the daemon's
 * `GET /legion/v1/state`. */
export function pinnedRoutes(pins: readonly PinnedRead[]): Map<string, string[]> {
  const held = new Map<string, string[]>();
  const add = (route: string, name: string) => held.set(route, [...(held.get(route) ?? []), name]);
  for (const read of pins) {
    if (read.name === RECORD_READ) {
      add("GET /legion/v1/state", read.name);
      continue;
    }
    if (read.command[0] !== "gh") continue;
    const argv = argvOf(read, PLACES);
    const key = routeOf(argv)?.key;
    const row = key === undefined ? undefined : worldRouteOf(key);
    if (row !== undefined) add(row.route, read.name);
    if (argv[0] === "api") {
      const header = argv.indexOf("-H");
      const accept =
        /^Accept:\s*(.+)$/.exec(argv[header + 1] ?? "")?.[1] ?? "application/vnd.github+json";
      const found = githubRouteOf(methodOf(argv), `/${endpointPath(argv)}`, accept);
      if (found !== undefined) add(found.route.route, read.name);
    }
  }
  return held;
}
/** The README's pin section: from its heading to the end of the rig's own section, the "sections
 * below" its claim names ("A read with no pin, and an answer this section and the sections below
 * do not name, is not claimed to answer as production does"), with where it starts. */
export function pinSection(readme: string): { text: string; first: number } {
  const lines = readme.split("\n");
  const start = lines.findIndex((line) =>
    line.startsWith("### Which of the world's answers are pinned")
  );
  if (start < 0)
    throw new Error('the README has no section "Which of the world\'s answers are pinned"');
  const end = lines.findIndex((line, at) => at > start && /^## /.test(line));
  return { text: lines.slice(start, end < 0 ? undefined : end).join("\n"), first: start + 1 };
}
/** A route written with every placeholder as `*` (`{owner}`, `<o>`, `<number>`), which is how the
 * README and the tables are matched one against the other. */
const templated = (text: string) => text.replace(/\{[^}]*\}|<[^>]*>/g, "*");

export function readsHeld(
  served: readonly ServedRead[],
  held: ReadonlyMap<string, readonly string[]>,
  section: { text: string; first: number }
): Hit[] {
  return served.map(({ route, by }) => {
    const pins = held.get(route) ?? [];
    if (pins.length > 0)
      return {
        at: route,
        text: by,
        ...holds(
          `held by ${pins.length} pinned read(s), first \`${cut(pins[0] ?? "", 60)}\`; every pinned read is compared by a row (gh-standin.test.ts's "every read the pin holds is compared by a row")`
        ),
      };
    const lines = section.text.split("\n");
    const named = lines.findIndex((line) => templated(line).includes(templated(route)));
    return named >= 0
      ? {
          at: route,
          text: by,
          ...holds(`no pin; the README's pin section names it (README:${section.first + named})`),
        }
      : {
          at: route,
          text: by,
          ...instance("no pin holds it, and the README's pin section does not name it"),
        };
  });
}

const reads: Check = {
  name: "reads: every GET the world serves is held by a pin a row compares",
  class: "A world read with no pin.",
  reading:
    "the GETs the world serves (gh-routes.ts's WORLD_ROUTES reads, github-routes.ts's GITHUB_ROUTES GETs, daemon-standin.ts's GETs) against the routes production-shapes.json's pinned reads ask, each read's command filled with the world's values; and, for a GET no pin holds, the README's pin section (README.md's \"Which of the world's answers are pinned\" to the end of the rig's section)",
  rule: "a GET some pinned read asks is held, since gh-standin.test.ts's coverage row fails on any pinned read no row compares; a GET no pin holds is a true statement where the pin section names its route (placeholders read alike), and an instance where it does not",
  run: (tree) =>
    readsHeld(
      servedReads(tree),
      pinnedRoutes(pinnedReads()),
      pinSection(tree.text("packages/pi-envoy/scripts/README.md"))
    ),
};

/** The values a pinned read is made with in the world of the most routes after the run's writes:
 * an approved world, whose create route answers the review 5002, holding a thread whose first
 * comment is the one the reply below names (PLACES' `comment`), which accept's approval, leaving
 * no thread, does not hold. */
async function differences(worlds: Worlds): Promise<Hit[]> {
  const world =
    worlds
      .all()
      .find(
        (one) =>
          one.routes.some(
            (route) =>
              route.match.includes("/reviews([ ?]|$)") &&
              route.methods?.includes("POST") &&
              JSON.stringify(route.stdout ?? "").includes("5002")
          ) &&
          one.routes.some((route) =>
            JSON.stringify(route.graph ?? "").includes(`"databaseId":${PLACES.comment}`)
          )
      ) ?? richest(worlds);
  const dir = worlds.copy(world);
  const { repo, pr, head, base, branch } = world.world;
  const pulls = `repos/${repo}/pulls/${pr}`;
  // The run's writes, so that no list a read names is empty: a comment on the conversation, a
  // review with an inline comment, and a reply in a thread (gh-standin.test.ts's pinned world).
  for (const argv of [
    ["pr", "comment", String(pr), "--body", SENT],
    [
      "api",
      "-X",
      "POST",
      `${pulls}/reviews`,
      "-f",
      `commit_id=${head}`,
      "-f",
      "event=COMMENT",
      "-f",
      `body=${SENT}`,
    ],
    ["api", "-X", "POST", `${pulls}/comments/${PLACES.comment}/replies`, "-f", `body=${SENT}`],
  ]) {
    const done = await worlds.in(dir, argv);
    if (done.code !== 0)
      throw new Error(`${world.name}: gh ${argv.join(" ")}: exit ${done.code}: ${done.err}`);
  }
  const [owner = "", name = ""] = repo.split("/");
  const values = { ...PLACES, owner, repo: name, number: String(pr), head, base, branch };
  const pins = pinnedReads().filter((read) => read.command[0] === "gh");
  const answered = await limited(pins, 2, async (read) => ({
    read,
    got: await worlds.in(dir, argvOf(read, values)),
  }));
  const hits: Hit[] = [];
  const missing = new Map<string, string[]>();
  for (const { read, got } of answered) {
    const status = /HTTP (\d{3})/.exec(read.stderr)?.[1];
    if (read.exit !== 0 && status !== undefined) {
      const gives = got.code !== 0 && got.err.includes(`HTTP ${status}`);
      hits.push({
        at: `production-shapes.json: ${read.name}`,
        text: `production: exit ${read.exit}, ${cut(read.stderr, 60)}`,
        ...(gives
          ? holds(`the world refuses it with HTTP ${status} too (exit ${got.code})`)
          : instance(
              `the world answers it exit ${got.code}${got.code === 0 ? "" : `, ${cut(got.err, 40)}`}, and no world gives production's HTTP ${status}`
            )),
      });
    }
    if (read.exit !== 0 || got.code !== 0) continue;
    let answer: unknown = got.out;
    try {
      answer = JSON.parse(got.out);
    } catch {}
    for (const field of shapeDiff(answer, read).missing.filter((one) => one.startsWith(".")))
      missing.set(field, [...(missing.get(field) ?? []), read.name]);
  }
  for (const [field, names] of missing)
    hits.push({
      at: `production-shapes.json: ${field}`,
      text: `in ${names.length} pinned read(s), first \`${cut(names[0] ?? "", 50)}\``,
      ...instance(
        `production's answer holds ${field}, and the world's answer to the same command does not`
      ),
    });
  return hits;
}

const productionAnswers: Check = {
  name: "reads: production's pinned answers the world does not give",
  class: "An answer of the world an agent reads that production would not give.",
  reading:
    "every gh read production-shapes.json pins, made with the world's values in the world of the most routes after a comment, a review and a reply of the run's (gh-standin.test.ts's pinned world), through the gh stand-in",
  rule: "a read production refused with an HTTP status is a true statement where the world refuses it with that status, and an instance where it does not; a JSON path production's answer holds that the world's answer to the same command lacks (production-shapes.ts's `shapeDiff`, both exiting 0) is an instance",
  run: (_, worlds) => differences(worlds),
};

const remoteHelperKey = grepCheck({
  name: "reads: a remote helper key on the world's clone",
  class: "An answer of the world an agent reads that production would not give.",
  files: () => [rig("rig.sh")],
  pattern: /config(?: --[\w-]+)* remote\.[^ ]+\.vcs\b/,
  reading: "grep rig.sh for a `git config remote.<name>.vcs` it sets",
  rule: "every hit is an instance: the key has git run a remote helper of the world's own (`git-remote-<vcs>`), where a production clone of `https://github.com/…` runs git's own https transport and holds no such key",
  verdict: () => instance("the world's clone carries a remote helper key no production clone has"),
});

const printedCredential: Check = {
  name: "reads: a credential production prints that the world holds none of",
  class: "An answer of the world an agent reads that production would not give.",
  reading:
    "each `gh auth` command of gh-writes.json, alone and with `-t` and `--show-token`, that health.ts's rule 21 reads as printing the token (PRINTS_TOKEN), called in the world of the most routes",
  rule: "every such call is an instance: production answers it with the pane's token, and no stand-in reads a token, so the world cannot",
  run: async (_, worlds) => {
    // A command that prints the token alone is tried alone; a flag is tried on one that does not.
    const calls = Object.keys(GH_COMMANDS.commands)
      .filter((command) => command.startsWith("auth "))
      .flatMap((command) =>
        PRINTS_TOKEN.test(`gh ${command}`)
          ? [[command]]
          : [
              [command, "-t"],
              [command, "--show-token"],
            ]
      )
      .map((words) => words.join(" ").split(" "))
      .filter((argv) => PRINTS_TOKEN.test(`gh ${argv.join(" ")}`));
    const world = richest(worlds);
    return limited(calls, 2, async (argv) => {
      const { answered } = await worlds.call(world, argv);
      return {
        at: `gh ${argv.join(" ")}`,
        text: `the world: exit ${answered.code}, handler ${answered.handler}`,
        ...instance("production prints the pane's token; the world holds none to print"),
      };
    });
  },
};

/** The row of rig.test.ts that holds every file rig.sh builds into a run: it composes each
 * scenario's run for both labels as cmd_run composes it, up to its first agent
 * (SKILL_SCENARIOS_COMPOSE_ONLY), and reads every file of it for the guard's words. */
export const WHOLE_WORLD_ROW =
  "no file rig.sh writes, copies or builds into a run before its agent starts names a word of the measurement";

/** The rig's records a run's agent can read that name the rig: each file rig.sh copies into the
 * top of a run's directory (`cp <file> "$R/<name>"`), which the run's namespace shows (world_paths
 * prints `rw $R` and hides only what it prints `hide` for), whose source a writer fills with a
 * line naming `$here`, the rig's own directory. A writer is a line that redirects into the
 * source (the same path, or a call of the same function where the source is one's output), read
 * as the function it calls, or as the `{ … }` group it closes. And each file rig.sh builds into a
 * run (`--outfile "$R/<path>"`), a bundle of a module of the rig's and every module it imports,
 * whose words no reading of rig.sh's text can see: the gh bundle took the route table's evidence
 * at 9083afb5, which named a scenario, and the guard refused every reviewer of INFRA-764's round
 * 30. Such a file is held by rig.test.ts's WHOLE_WORLD_ROW, which builds it as the rig does and
 * reads it, and is an instance where that row is not there. */
export function rigRecords(tree: Tree): Hit[] {
  const file = rig("rig.sh");
  const lines = tree.lines(file);
  const script = lines.join("\n");
  const wholeWorld = tree.text(rig("rig.test.ts")).includes(`"${WHOLE_WORLD_ROW}"`);
  const hidden = [...script.matchAll(/printf 'hide %s\\n' "\$R\/([\w.-]+)"/g)].map(
    ([, name]) => name
  );
  const definitions = new Map(
    scriptDefinitions(script).map((text) => [/^(\w+)\(\)/.exec(text)?.[1] ?? "", text])
  );
  /** What the writer at line `at` writes: the group its `}` closes, or the function it calls. */
  const written = (at: number): string => {
    const line = lines[at] ?? "";
    if (/^\s*\}/.test(line)) {
      const indent = /^\s*/.exec(line)?.[0] ?? "";
      let open = at - 1;
      while (open > 0 && !new RegExp(`^${indent}\\{\\s*$`).test(lines[open] ?? "")) open--;
      return lines.slice(open, at + 1).join("\n");
    }
    return definitions.get(/^\s*(\w+)\b/.exec(line)?.[1] ?? "") ?? line;
  };
  const hits: Hit[] = [];
  lines.forEach((text, index) => {
    if (comment(file, text)) return;
    const built = /--outfile "\$R\/([^"]+)"/.exec(text);
    if (built !== null) {
      hits.push({
        at: `${file}:${index + 1}`,
        text,
        ...(wholeWorld
          ? holds(
              `<run>/${built[1]}, built into every run, is held by rig.test.ts's "${WHOLE_WORLD_ROW}", which builds it as the rig does and reads it for the guard's words`
            )
          : instance(
              `<run>/${built[1]} is built into every run, which the agent can read, and no row of rig.test.ts ("${WHOLE_WORLD_ROW}") builds and reads it`
            )),
      });
      return;
    }
    const copied = /\bcp (?:-\w+ )*(.+?) "\$R\/([\w.-]+)"\s*$/.exec(text);
    if (copied === null || hidden.includes(copied[2] ?? "")) return;
    const source = (copied[1] ?? "").replace(/^"|"$/g, "");
    if (source.startsWith("$R/")) return;
    const called = /^\$\((\w+) /.exec(source)?.[1];
    const into = called === undefined ? `>"${source}"` : `>"$(${called} `;
    const naming = lines.flatMap((line, at) =>
      at !== index &&
      !comment(file, line) &&
      line.includes(into) &&
      /(?:printf|echo)\b[^\n]*\$here\b/.test(written(at))
        ? [`${file}:${at + 1}`]
        : []
    );
    hits.push({
      at: `${file}:${index + 1}`,
      text,
      ...(naming.length > 0
        ? instance(
            `<run>/${copied[2]}, which the agent can read, is filled at ${naming.join(", ")} with lines naming the rig's own directory ($here)`
          )
        : holds(`<run>/${copied[2]}: no writer of ${source} names the rig's directory`)),
    });
  });
  return hits;
}
const runRecords: Check = {
  name: "an agent's world: a record of the rig's that names the rig",
  class: "Anything an agent can read naming the test.",
  reading:
    'rig.sh: each `cp <file> "$R/<name>"` into the top of a run\'s directory, which world_paths shows (`rw $R`) and does not hide, and the functions that write <file>; and each `--outfile "$R/<path>"`, a file it builds into every run',
  rule: `a record whose writer prints \`$here\`, the rig's own directory (which holds \`skill-scenarios\`), is an instance; one whose writers print no such path is a true statement; a built file is a true statement where rig.test.ts holds "${WHOLE_WORLD_ROW}", which builds and reads it, and an instance where it does not`,
  run: rigRecords,
};

/** Each `mount --bind <source> <target>` of a line, each word as the line writes it: a word in
 * double quotes, a command substitution in it included (`"$(machine "$p")"`), or a bare word. */
const BIND =
  /\bmount --bind ("(?:[^"$]|\$\([^()]*\)|\$(?!\())*"|\S+) ("(?:[^"$]|\$\([^()]*\)|\$(?!\())*"|\S+)/g;
/** private_world's holder, `h=<dir>`, the directory of /dev/shm it binds each shown path into. */
const HOLDER = /^h=(\/dev\/shm\/[\w.-]+)$/m;
const worldContent = grepCheck({
  name: "an agent's world: a path the namespace fills with the rig's own content",
  class:
    "A world path whose content the rig declares by a premise about the image, not by the host's own file.",
  files: () => [rig("rig.sh")],
  pattern: /\bmount --bind /,
  reading:
    "grep rig.sh for each `mount --bind <source> <target>` a run's namespace or its probe makes (private_world, namespace_probe), and read private_world's holder, its `h=` assignment",
  rule: "a bind onto the holder (a target `$h/...`, where private_world assigns `h` a directory under /dev/shm, which the agent's world drops, or a target under /dev/shm), a bind of the holder's copy of a path back onto that path (a source `$h/$<n>`), and the run's own <run>/tmp bound onto /tmp (the README's world section) are true statements; a bind of any other source is an instance: the agent reads there what the rig made, where production's pane reads the host's own file, as it read an empty file bound over the system git configuration because the worker image had none, until INFRA's image carried one",
  verdict: ({ text }, tree) => {
    const word = (written: string) => written.replace(/^"|"$/g, "");
    const holder = HOLDER.exec(tree.text(rig("rig.sh")))?.[1];
    for (const [, source = "", target = ""] of text.matchAll(BIND)) {
      if (word(target).startsWith("/dev/shm/")) continue;
      if (
        holder !== undefined &&
        (word(target).startsWith("$h/") || /^\$h\/\$\w+$/.test(word(source)))
      )
        continue;
      if (word(source) === "$R/tmp" && word(target) === "/tmp") continue;
      return instance(
        `binds ${source} over ${target}: content the rig made, at a path of the agent's world`
      );
    }
    return holds(
      `each bind holds a path in the holder (${holder ?? "/dev/shm"}), binds the holder's copy back at its own path, or binds the run's own /tmp`
    );
  },
});

// The readers.

/** The readers, and every module of the rig's directory they import, transitively. */
function readerModules(tree: Tree): string[] {
  const seen = new Set<string>();
  const pending = ["health.ts", "score.ts", "judge.ts"].map(rig);
  for (let file = pending.pop(); file !== undefined; file = pending.pop()) {
    if (seen.has(file)) continue;
    seen.add(file);
    for (const [, name] of tree.text(file).matchAll(/from "\.\/([\w.-]+)"/g)) {
      const target = rig(name?.endsWith(".ts") ? name : `${name}.ts`);
      if (existsSync(path.join(tree.root, target)) || tree.planted.has(target))
        pending.push(target);
    }
  }
  return [...seen].sort();
}
/** What of each module a reader can run: the readers whole, and of every module they import its
 * top level (but its `main` block) and each definition a reached part names, by its own name or
 * by the name it is imported under, transitively. */
export function readerReach(tree: Tree): Map<string, { spans: Span[]; reached: Set<string> }> {
  const files = readerModules(tree);
  const reach = new Map<string, { spans: Span[]; reached: Set<string> }>();
  for (const file of files) reach.set(file, { spans: spans(tree.text(file)), reached: new Set() });
  const readers = ["health.ts", "score.ts", "judge.ts"].map(rig);
  const pending: [string, string][] = [];
  for (const file of files) {
    pending.push([file, "(top)"]);
    if (readers.includes(file))
      for (const span of reach.get(file)?.spans ?? []) pending.push([file, span.name]);
  }
  const imports = new Map(
    files.map((file) => [
      file,
      [
        ...tree.text(file).matchAll(/import\s*(?:type\s*)?\{([^}]*)\}\s*from "\.\/([\w.-]+)"/g),
      ].flatMap(([, names, from]) =>
        (names ?? "").split(",").flatMap((one) => {
          const [own = "", local = own] = one
            .replace(/\btype\b/, "")
            .trim()
            .split(/\s+as\s+/);
          return own === ""
            ? []
            : [{ own, local, from: rig(from?.endsWith(".ts") ? from : `${from}.ts`) }];
        })
      ),
    ])
  );
  for (let next = pending.pop(); next !== undefined; next = pending.pop()) {
    const [file, name] = next;
    const entry = reach.get(file);
    if (entry === undefined || entry.reached.has(name)) continue;
    entry.reached.add(name);
    const lines = tree.lines(file).map((line) => (comment(file, line) ? "" : line));
    // An import names what a module takes, and runs none of it: a name it takes is reached where
    // a reached part of the module uses it.
    const text =
      name === "(top)"
        ? lines
            .filter((_, at) => spanAt(entry.spans, at + 1) === undefined)
            .join("\n")
            .replace(/^import\b[\s\S]*?\bfrom "[^"]+";$/gm, "")
        : entry.spans
            .filter((span) => span.name === name)
            .map((span) => spanText(lines, span))
            .join("\n");
    const words = new Set(text.match(/\b[A-Za-z_$][\w$]*\b/g) ?? []);
    for (const span of entry.spans)
      if (span.name !== "(main)" && span.name !== name && words.has(span.name))
        pending.push([file, span.name]);
    for (const { own, local, from } of imports.get(file) ?? [])
      if (words.has(local)) pending.push([from, own]);
  }
  return reach;
}

/** The inputs a reader may not judge a run by (the architect's ruling of 2026-11-18: a reader
 * judges a run from its archive and its recorded provenance alone; README.md's "What a reader
 * reads"), each by the code that reads it. */
const READER_INPUTS: readonly [string, RegExp][] = [
  ["TMPDIR", /process\.env(?:\.TMPDIR\b|\[["'`]TMPDIR["'`]\])|\b(?:os\.)?tmpdir\(\)/],
  ["HOME", /process\.env(?:\.HOME\b|\[["'`]HOME["'`]\])|\b(?:os\.)?homedir\(\)/],
  ["the working directory", /process\.cwd\(\)|process\.env\.PWD\b/],
  ["LEGION_WORKSPACE", /\bLEGION_WORKSPACE\b/],
  ["RIG_ROOT", /\bRIG_ROOT\b/],
  ["the work directory", /\bSKILL_SCENARIOS_WORK\b/],
  ["rig.sh world", /rig\.sh["'`]?\s*\)?\s*,\s*["'`]world["'`]|rig\.sh world\b/],
  [
    "a profile or an install",
    /\b(?:existsSync|readdirSync|readFileSync|statSync|lstatSync)\([^)]*\b(?:profiles|installs)\b/,
  ],
  ["the reader's own checkout", /\bimport\.meta\.dir\b/],
];

export function readerEnvironment(tree: Tree): Hit[] {
  const reach = readerReach(tree);
  const hits: Hit[] = [];
  for (const [file, { spans: all, reached }] of reach) {
    const lines = tree.lines(file);
    const text = lines.join("\n");
    // A top-level name of a place under the reader's own checkout (`const X = path.join(
    // import.meta.dir, …)`, over as many lines as it takes): each use of it is read as a read of
    // the checkout, and the lines that define it as no read.
    const defined = [
      ...text.matchAll(
        /^(?:export )?const (\w+) = (?:path\.(?:join|resolve)\(\s*)?import\.meta\.dir\b/gm
      ),
    ].map((found) => {
      const first = text.slice(0, found.index).split("\n").length;
      return { name: found[1] ?? "", first, last: first + (found[0].split("\n").length - 1) };
    });
    const inputs: [string, RegExp][] = [
      ...READER_INPUTS,
      ...defined.map(({ name }): [string, RegExp] => [
        "the reader's own checkout",
        new RegExp(`\\b${name}\\b`),
      ]),
    ];
    const runsProgram = new Set<string>();
    for (const span of all)
      if (/\b(?:Bun\.spawn|spawn\w*|execFile\w*)\(/.test(spanText(lines, span)))
        runsProgram.add(span.name);
    for (let grew = true; grew; ) {
      grew = false;
      for (const span of all)
        if (
          !runsProgram.has(span.name) &&
          [...runsProgram].some((name) => new RegExp(`\\b${name}\\(`).test(spanText(lines, span)))
        ) {
          runsProgram.add(span.name);
          grew = true;
        }
    }
    const hashes = (span: Span | undefined) =>
      span !== undefined && /createHash\(|CryptoHasher|\.digest\(/.test(spanText(lines, span));
    const callers = (span: Span) =>
      all.filter(
        (caller) =>
          caller !== span && new RegExp(`\\b${span.name}\\(`).test(spanText(lines, caller))
      );
    lines.forEach((line, index) => {
      if (comment(file, line)) return;
      const span = spanAt(all, index + 1);
      const name = span?.name ?? "(top)";
      if (!reached.has(name)) return;
      const input = inputs.find(([, pattern]) => pattern.test(line))?.[0];
      if (input === undefined) return;
      const place = defined.find(({ first, last }) => first <= index + 1 && index + 1 <= last);
      // The name a read of a profile or an install reads (`existsSync(profiles)`), which a path
      // under the run's own directory may have been given above.
      const read = /\b(?:existsSync|readdirSync|readFileSync|statSync|lstatSync)\(\s*(\w+)/.exec(
        line
      )?.[1];
      const underRun =
        /\b(?:run\.dir|runDir)\b/.test(line) ||
        (span !== undefined &&
          read !== undefined &&
          new RegExp(`const ${read} = path\\.join\\((?:run\\.dir|runDir)\\b`).test(
            spanText(lines, span)
          ));
      let verdict: Omit<Hit, "at" | "text">;
      if (place !== undefined)
        verdict = holds(`the place ${place.name} names, which each use of it is judged at`);
      else if (/mkdtemp(?:Sync)?\(\s*path\.join\(\s*(?:os\.)?tmpdir\(\)/.test(line))
        verdict = holds("a scratch directory of the reader's own, which holds no input");
      else if (
        input === "the reader's own checkout" &&
        (/["'`][\w.-]+\.(?:go|ts|sh|json)["'`]/.test(line) || runsProgram.has(name))
      )
        verdict = holds(
          "where a program of the rig's own is, or is run from, which the reader runs"
        );
      else if (underRun) verdict = holds("a path under the run's own directory, its archive");
      else if (
        hashes(span) ||
        (span !== undefined &&
          callers(span).some((caller) => reached.has(caller.name)) &&
          callers(span).every(hashes))
      )
        verdict = holds("a cache keyed by the bytes of what it reads, which decides no verdict");
      else verdict = instance(`a reader judges a run by ${input}, which the run did not record`);
      hits.push({ at: `${file}:${index + 1}`, text: line, ...verdict });
    });
  }
  return hits;
}

const readersArchive: Check = {
  name: "readers: a reader reads the run's archive and its recorded provenance alone",
  class: "A reader that judges a run by what the pod holds, not by what the run recorded.",
  reading:
    "health.ts, score.ts, judge.ts and every module of the rig's directory they import, the parts of each a reader can run (its top level, and each definition a reached part names): each code line that reads TMPDIR, HOME, the working directory, LEGION_WORKSPACE, RIG_ROOT, the work directory, `rig.sh world`, a profile or an install, or the reader's own checkout (`import.meta.dir`, and each top-level place built on it)",
  rule: "a true statement where the line names a place each use of which is judged, makes a scratch directory of its own, locates a program of the rig's own it runs, reads under the run's own directory, or sits in a cache keyed by the bytes it reads (its definition, or every caller of it, computes a digest); every other is an instance. rig.test.ts's \"one archived run reads the same under another TMPDIR, working directory and work directory\" holds health to the same, run",
  run: readerEnvironment,
};

/** The patterns a reader tests a world id with: a GitHub node id, a commit's hex, a REST id's
 * path, a ref, or a write's name. */
const WORLD_ID =
  /PRR[TC]?_|PR_kw|\[0-9a-f\]|\[a-f0-9\]|\\\/(?:pulls|reviews|comments|replies)\b|\/(?:pulls|reviews|comments)\\\/|refs\\\/heads|addPullRequestReviewThreadReply|resolveReviewThread|\\\/replies/;
/** Of those, the ones that name what the world did with a call: a node id or a write's name. */
const WORLD_ACT = /PRR[TC]?_|PR_kw|addPullRequestReviewThreadReply|resolveReviewThread|replies/;
/** A call's own text: what a call sent, which an agent wrote. */
const CALL_TEXT = /\bcallText\(|\bargv\b|\bstdin\b|\.files\b/;

export function readerTexts(tree: Tree): Hit[] {
  const hits: Hit[] = [];
  for (const file of ["score.ts", "health.ts"].map(rig)) {
    const lines = tree.lines(file);
    const text = lines.join("\n");
    const all = spans(text);
    const patterns = new Map(
      [
        ...text.matchAll(
          /^(?:export )?const ([A-Z][A-Z0-9_]+)\s*=\s*(\/.+\/[gimsuy]*|new RegExp\([\s\S]*?\);)/gm
        ),
      ].map(([, name, source]) => [name ?? "", source ?? ""])
    );
    lines.forEach((line, index) => {
      if (comment(file, line)) return;
      const sites = [
        ...line.matchAll(
          /(\/(?:[^/\\\n]|\\.)+\/[gimsuy]*|\b[A-Z][A-Z0-9_]+)\.(?:test|exec)\(([^)]*)\)/g
        ),
      ].map(([, pattern, subject]) => ({ pattern: pattern ?? "", subject: subject ?? "" }));
      for (const [, subject, pattern] of line.matchAll(
        /([\w.$\][?]+(?:\([^)]*\))?)\.(?:match|matchAll|search)\((\/(?:[^/\\\n]|\\.)+\/[gimsuy]*|[A-Z][A-Z0-9_]+)/g
      ))
        sites.push({ pattern: pattern ?? "", subject: subject ?? "" });
      for (const [, subject, needle] of line.matchAll(
        /([\w.$\][?]+(?:\([^)]*\))?)\.includes\(([^)]*)\)/g
      ))
        if (
          /\.(?:id|sha|oid|head|headRefOid|commit_id)\b|^(?:sha|tip|oid|commitId|threadId|nodeId)$/.test(
            (needle ?? "").trim()
          )
        )
          sites.push({ pattern: `includes(${needle})`, subject: subject ?? "" });
      for (const [, subject] of line.matchAll(/(?<!function )\bnamesCommit\(([^,]+),/g))
        sites.push({
          pattern: "namesCommit (a commit's hex, score.ts's `namesCommit`)",
          subject: subject ?? "",
        });
      const span = spanAt(all, index + 1);
      for (const site of sites) {
        const source = patterns.get(site.pattern) ?? site.pattern;
        const id =
          site.pattern.startsWith("includes(") ||
          site.pattern.startsWith("namesCommit") ||
          WORLD_ID.test(source);
        if (!id) continue;
        const at = `${file}:${index + 1}`;
        if (span?.name === "namesCommit") {
          hits.push({
            at,
            text: line,
            ...holds("the helper's own test; each call of it is listed where it is made"),
          });
          continue;
        }
        // Where the subject comes from: a callback over a list (`argv.some((arg) => …)`), or an
        // assignment of it in the same definition.
        const word = /^[\w$]+/.exec(site.subject.trim())?.[0] ?? "";
        const over = new RegExp(
          `([\\w.$]+(?:\\([^)]*\\))?)\\.(?:some|every|find|findLast|filter|map|flatMap)\\(\\(?${word}\\)?\\s*=>`
        ).exec(line)?.[1];
        const assigned =
          span === undefined || word === ""
            ? undefined
            : new RegExp(`(?:const|let) ${word}\\s*=\\s*([^;\\n]+)`).exec(
                spanText(lines, span)
              )?.[1];
        const from = [site.subject, over ?? "", assigned ?? ""].join(" ");
        const call = CALL_TEXT.test(from);
        hits.push({
          at,
          text: line,
          ...(call && WORLD_ACT.test(source)
            ? instance(
                "decides what the world did with a call from the call's own text, which the replay records (gh-threads.ts's threadCall)"
              )
            : call
              ? holds("which route the call asked for, whose record is the call's own argv")
              : holds(
                  `reads ${cut(site.subject, 40)}, no call's text: a record of its own (a body, a message, a log, a query)`
                )),
        });
      }
    });
  }
  return hits;
}

const readersText: Check = {
  name: "readers: no reader decides a world fact from an agent's text",
  class:
    "A reader that decides a world fact from an agent's text instead of the world's own record.",
  reading:
    "score.ts and health.ts: every `.includes(`, `.test(`, `.exec(`, `.match(`, `.matchAll(`, `.search(` and `namesCommit(` whose pattern (a literal, or a top-level const of the file) names a world id — a node id (`PRRT_`, `PRRC_`, `PRR_`, `PR_kw`), a commit's hex, a REST id's path, a ref, a write's name — or whose needle is an id, a sha, a head or a tip",
  rule: "an instance where the text tested is a call's own (`callText(`, its argv, stdin or files, directly or as the list a callback reads or the value a name was given) and the pattern names what the world did with it (a node id or a write's name), which the replay records; a true statement where the call's text is tested for the route it asked (a REST path), or where the text is no call's",
  run: readerTexts,
};

// The stand-ins' declarations of what they do not model.

/** A stand-in's declaration that it does not model something, its words split across a
 * comment's lines read as one. */
const NOT_MODELLED = /\bnot\s+modell?ed\b/gi;
/** The declarations of `file` that something is not modelled: each comment block of it (its
 * consecutive comment lines, `comment`) read as one text, its comment markers cut, and each match
 * of NOT_MODELLED in it, at the line it starts on, with the sentence that holds it and the
 * sentence after it where that one gives why no scenario reaches the behaviour (`Unreachable:`),
 * as the declaration writes them. A sentence ends at a full stop before white space or at the
 * block's end. */
export function notModelledIn(tree: Tree, file: string): { line: number; text: string }[] {
  const lines = tree.lines(file);
  const found: { line: number; text: string }[] = [];
  for (let start = 0; start < lines.length; ) {
    if (!comment(file, lines[start] ?? "")) {
      start += 1;
      continue;
    }
    let end = start;
    while (end < lines.length && comment(file, lines[end] ?? "")) end += 1;
    const offsets: number[] = [];
    let text = "";
    for (let at = start; at < end; at++) {
      offsets.push(text.length);
      text += `${(lines[at] ?? "").replace(/^\s*(?:\/\*\*?|\*\/|\*|\/\/|#)?\s?/, "")} `;
    }
    for (const match of text.matchAll(NOT_MODELLED)) {
      const at = match.index;
      let opened = 0;
      for (const stop of text.slice(0, at).matchAll(/\.\s/g)) opened = stop.index + 2;
      const stops = [...text.slice(at).matchAll(/\.\s/g)].map((stop) => at + stop.index + 1);
      const first = stops[0] ?? text.length;
      const next = text.slice(first).trimStart();
      const closed = next.startsWith("Unreachable:") ? (stops[1] ?? text.length) : first;
      let line = start;
      while (line + 1 < end && (offsets[line + 1 - start] ?? Infinity) <= at) line += 1;
      found.push({ line: line + 1, text: text.slice(opened, closed).replace(/\s+/g, " ").trim() });
    }
    start = end;
  }
  return found;
}

const notModelled: Check = {
  name: "stand-ins: a behaviour a stand-in declares not modelled",
  class:
    "A stand-in that answers otherwise than production where it declares a behaviour not modelled, with no ruling that a scenario cannot reach it or that it is a residual.",
  reading:
    "every program of the rig (each stand-in it runs among them: daemon-standin.ts, standin-record.ts, gh-standin.ts, github-standin.ts, daemon-pane.ts and daemon-pane.go, and every other non-test .ts and .go of its directory but the search's own) for `not modelled` in a comment, its words across the comment's lines read as one, each with its sentence and the `Unreachable:` sentence after it",
  rule: "every declaration is an instance: class-search.json's residuals cover each one ruled a residual, and its `unreachable` entries each one no scenario's world reaches, each by the text the declaration writes; a declaration neither covers is a finding, and an entry no declaration matches is stale",
  run: (tree) =>
    programs(tree, /\.(ts|go)$/).flatMap((file) =>
      notModelledIn(tree, file).map(({ line, text }) => ({
        at: `${file}:${line}`,
        text,
        ...instance("declares a behaviour of production not modelled"),
      }))
    ),
};

// The tests' bound.

/** A call that runs a process: test-programs.ts's spawnChecked, Bun's spawn, spawnSync and shell
 * (`Bun.$`), and node's spawnSync and execSync. */
const RUNS_PROCESS = /\b(?:spawnChecked|spawnSync|execSync)\s*\(|\bBun\.spawn\s*\(|\bBun\.\$/;
/** A call at a file's top level (biome writes a top-level statement at column 0) that bounds every
 * row of the file, and its argument: bun:test's setDefaultTimeout, or scripts/e2e/lib/row-waits.ts's
 * rowWaits, assigned to a name, which calls setDefaultTimeout with its bound. */
const DEFAULT_TIMEOUT = /^(?:(setDefaultTimeout)|(?:const|let) \w+ = (rowWaits))\(([^)]*)\)/;
/** The least bound in milliseconds a test file that runs processes sets its rows: six times bun's
 * 5 s default. */
const LEAST_BOUND = 30_000;

/** Each test file of the rig, judged by the bound its top level sets its rows: at its first code
 * line that runs a process (a row's, or a helper's of the file, which its rows call), or alone
 * where it runs none. */
export function defaultBounds(tree: Tree): Hit[] {
  return tests(tree).map((file) => {
    const lines = tree.lines(file);
    const first = lines.findIndex(
      (text) =>
        !comment(file, text) &&
        // Each string's text cut, so a call written inside one (a row's sample of code) is no call.
        RUNS_PROCESS.test(text.replace(/(["'`])(?:\\.|(?!\1)[^\\])*\1/g, "$1$1"))
    );
    if (first === -1) return { at: file, text: "", ...holds("runs no process") };
    const bounds = lines.flatMap((text, index) => {
      const call = DEFAULT_TIMEOUT.exec(text);
      return call === null
        ? []
        : [
            {
              line: index + 1,
              name: call[1] ?? call[2] ?? "",
              argument: (call[3] ?? "").trim(),
            },
          ];
    });
    const held = bounds.find(
      ({ argument }) =>
        /^\d+(?:_\d+)*$/.test(argument) && Number(argument.replaceAll("_", "")) >= LEAST_BOUND
    );
    return {
      at: `${file}:${first + 1}`,
      text: lines[first] ?? "",
      ...(held !== undefined
        ? holds(`${held.name}(${held.argument}) at line ${held.line} bounds its rows`)
        : instance(
            bounds.length === 0
              ? "runs a process under bun's 5 s default: its top level calls no setDefaultTimeout or rowWaits"
              : `runs a process under a bound below 30 000 ms: ${bounds.map(({ name, argument, line }) => `${name}(${argument}) at line ${line}`).join(", ")}`
          )),
    };
  });
}

const defaultBound: Check = {
  name: "hand search 11: a test that runs processes under bun's default bound",
  class: "A test file whose rows run processes under bun's 5 s default bound.",
  reading:
    "every test file of the rig for a code line that calls `spawnChecked`, `Bun.spawn`, `Bun.spawnSync`, `spawnSync`, `execSync` or `Bun.$` outside a string (a row's, or a helper's of the file, which its rows call), and for a `setDefaultTimeout(<n>)`, or a `rowWaits(<n>)` assigned to a name (scripts/e2e/lib/row-waits.ts, which calls setDefaultTimeout with it), at its top level",
  rule: "a file that runs a process is an instance unless its top level calls `setDefaultTimeout(<n>)` or assigns `rowWaits(<n>)` with `n` a number literal (`_` separators allowed) of at least 30 000, six times bun's 5 s default; a file that runs no process is a true statement",
  run: defaultBounds,
};

// The hand search's greps, each with its rule.

const greps: Check[] = [
  grepCheck({
    name: "hand search 1: a branch on a label's name",
    class: "A stand-in that answers one label's contract only.",
    files: programs,
    pattern:
      /\b\w*label\w*\s*[!=]==?\s*["'`](?:base|head)["'`]|["'`](?:base|head)["'`]\s*[!=]==?\s*\w*label|LABEL_DAEMONS/i,
    reading:
      'grep the rig\'s programs for a comparison of a label with "base" or "head", and for LABEL_DAEMONS',
    rule: "every hit is an instance: a program that branches on a label's name answers that label's contract and no other's",
    verdict: () => instance("branches on a label's name"),
  }),
  grepCheck({
    name: "hand search 4 and 6: one fact defined in two places",
    class: "One fact read in two places.",
    files: (tree) => programs(tree),
    pattern:
      /^(?:export )?(?:function|const|type|interface) (createdState|createdHead|reviewOf|readWorld|worldOf|World)\b/,
    reading:
      "grep the rig's programs for a definition of createdState, createdHead, reviewOf, readWorld, worldOf and World",
    rule: "a name's definitions in the first of the rig's programs (in byte order) that defines it are its one definition, a true statement; a definition of it in any other file is an instance",
    verdict: ({ file, text }, tree) => {
      const name = /(?:function|const|type|interface) (\w+)/.exec(text)?.[1] ?? "";
      const first = programs(tree).find((one) =>
        new RegExp(`^(?:export )?(?:function|const|type|interface) ${name}\\b`, "m").test(
          tree.text(one)
        )
      );
      return first === file
        ? holds(`${name}'s one definition`)
        : instance(`${name} is defined in ${first} too`);
    },
  }),
  grepCheck({
    name: "hand search 6: a reader of world.json of its own",
    class: "One fact named in two places.",
    files: programs,
    pattern: /readFileSync\([^)]*world\.json/,
    reading: "grep the rig's programs for a readFileSync of world.json",
    rule: "gh-call.ts's readWorld is the one reader (a true statement); a read anywhere else is an instance",
    verdict: ({ file }) =>
      file === rig("gh-call.ts")
        ? holds("readWorld, the one reader")
        : instance("reads world.json past readWorld"),
  }),
  grepCheck({
    name: "hand search 5: a citation of a test by its line",
    class: "A citation that moves when the lines it names move.",
    files: (tree) =>
      [".github", "packages", "docs", "scripts"].flatMap((dir) =>
        tree.files(
          dir,
          (file) => TEXT_FILE.test(file) && !OWN.test(file) && !PROMPT_TREE.test(file),
          true
        )
      ),
    pattern:
      /\b(?:rig|score|health|daemon-standin|gh-standin|github-standin|daemon-pane)\.test\.ts:[0-9]/,
    comments: true,
    reading:
      "grep .github, packages (the daemon's role prompts left out), docs and scripts for `<suite>.test.ts:<line>`",
    rule: "every hit is an instance: the line it names moves",
    verdict: () => instance("cites a test by its line"),
  }),
  grepCheck({
    name: "hand search 7: a verb read by a reader of its own",
    class:
      "A route or reader that decides a call's verb by a regex of its own rather than by the shared reader.",
    files: programs,
    pattern: /["'`](?:-X|--method)["'`=]/,
    reading: "grep the rig's programs for a quoted `-X` or `--method`",
    rule: "a true statement in gh-call.ts (methodOf, the shared reader) and on a line that reads curl's or wget's options, which methodOf does not model; an instance anywhere else",
    verdict: ({ file, text }) =>
      file === rig("gh-call.ts")
        ? holds("the shared reader (methodOf)")
        : /curl|wget|CURL|WGET/.test(text)
          ? holds("curl's or wget's options, which gh's methodOf does not model")
          : instance("reads a gh call's verb past methodOf"),
  }),
  grepCheck({
    name: "hand search 9: a reference to a deleted symbol",
    class: "A reference to a symbol or refusal another change deleted.",
    files: (tree) =>
      ["packages", "scripts"].flatMap((dir) =>
        tree.files(
          dir,
          (file) => TEXT_FILE.test(file) && !OWN.test(file) && !PROMPT_TREE.test(file),
          true
        )
      ),
    pattern:
      /READY_UNREVIEWED_CHANGE|unreviewedChange|readyNote|READY_HEAD_MOVED|READY_NOT_APPROVED|LABEL_DAEMONS/,
    comments: true,
    reading:
      "grep packages (the daemon's role prompts left out) and scripts for the READY refusals main deleted (a4c0ff99) and LABEL_DAEMONS",
    rule: "a true statement in a CHANGELOG.md (a dated release line) and in prompts_test.go (the retired-phrase census); an instance anywhere else",
    verdict: ({ file }) =>
      file.endsWith("CHANGELOG.md")
        ? holds("a dated release line")
        : file.endsWith("prompts_test.go")
          ? holds("the retired-phrase census")
          : instance("names a deleted symbol"),
  }),
  grepCheck({
    name: "hand search 11: a bound a slow host fails",
    class: "A pin whose pass depends on how fast the host is.",
    files: tests,
    pattern: /(?:Date|performance)\.now\(\)\s*-/,
    reading: "grep the rig's tests for an elapsed time (`Date.now() -`, `performance.now() -`)",
    rule: "an instance where the elapsed time is held under a bound (`expect(<elapsed>).toBeLessThan…`, `<elapsed> <`), which a slow host fails; a true statement where it is the bound itself (`toBeLessThan…(<elapsed>)`), which a slow host only raises",
    verdict: ({ file, line }, tree) => {
      const statement = tree
        .lines(file)
        .slice(Math.max(0, line - 3), line + 1)
        .join(" ");
      if (/toBeLessThan(?:OrEqual)?\(\s*\(?\s*(?:Date|performance)\.now\(\)\s*-/.test(statement))
        return holds("the elapsed time is the bound, which a slow host only raises");
      return /(?:Date|performance)\.now\(\)\s*-[^;]*?(?:\)\s*\.toBeLessThan|<=?\s*[\d(])/.test(
        statement
      )
        ? instance("holds an elapsed time under a bound")
        : holds("orders or names, and bounds nothing");
    },
  }),
  grepCheck({
    name: "hand search 15 and 31: a reader of another program's source as text",
    class: "A reader that parses another program's source in one shape only.",
    files: programs,
    pattern: /\b[A-Z_]*SOURCE\b\.includes\(|readFileSync\([^)]*\.go["'`]\)/,
    reading:
      "grep the rig's programs for a Go source's text tested with `.includes(` or read whole",
    rule: "a true statement where the code throws within the next three lines, naming what it did not find; an instance where a marker it does not find is read as a daemon that does not do it",
    verdict: ({ file, line }, tree) =>
      /\bthrow\b/.test(
        tree
          .lines(file)
          .slice(line - 1, line + 3)
          .join("\n")
      )
        ? holds("refuses a shape it does not know, naming it")
        : instance("reads a missing marker as a daemon that does not do it"),
  }),
  grepCheck({
    name: "hand search 30: a reader of the boot record of its own",
    class: "A record of an earlier step standing in for a missing later one.",
    files: programs,
    pattern: /path\.join\([^)]*\b(?:BOOTED|"booted")/,
    reading:
      "grep the rig's programs for a path to the boot record (completion-answers.ts's BOOTED)",
    rule: "health.ts's bootDecisions is the one parser (a true statement); a read anywhere else is an instance",
    verdict: ({ file, line }, tree) =>
      file === rig("health.ts") && spanAt(spans(tree.text(file)), line)?.name === "bootDecisions"
        ? holds("bootDecisions, the one parser")
        : instance("reads the boot record past bootDecisions"),
  }),
  grepCheck({
    name: "hand search 20 and 35: a hand-kept list of the rig's programs",
    class: "A hand-kept list a new entry must join.",
    files: () => [rig("rig.sh"), rig("rig.test.ts"), rig("rig-isolation.test.ts")],
    pattern: /\b[\w-]+\.(?:ts|go)\b.*\b[\w-]+\.(?:ts|go)\b/,
    reading:
      "grep rig.sh, rig.test.ts and rig-isolation.test.ts for a code line naming two files ending .ts or .go",
    rule: "an instance where the line lists two or more of the rig's own programs (its directory's files that are no test, read off the directory) as one list: the items of one array (each its own string) or the words of one shell list (`for … in`, `=( … )`); a true statement where it lists fewer (a world's own files, a test's, one program's path, a text that names two)",
    verdict: ({ text }, tree) => {
      const own = new Set(programs(tree, /\.(ts|go)$/).map((file) => path.basename(file)));
      const lists = [
        ...[...text.matchAll(/\[([^\]]*)\]/g)].map(([, items]) =>
          [...(items ?? "").matchAll(/["'`]([\w-]+\.(?:ts|go))["'`]/g)].map(
            ([, name]) => name ?? ""
          )
        ),
        ...[...text.matchAll(/\bfor \w+ in ([^;]*)|=\(([^)]*)\)/g)].map(([, words, array]) =>
          (words ?? array ?? "").split(/\s+/).map((word) => word.replace(/^["']|["']$/g, ""))
        ),
      ];
      const listed = lists
        .map((names) => names.filter((name) => own.has(name)))
        .find((names) => names.length >= 2);
      return listed !== undefined
        ? instance(`lists ${listed.join(", ")} by hand`)
        : holds("lists no two of the rig's programs");
    },
  }),
  grepCheck({
    name: "hand search 37: a reader of the system prompt's argument as its text",
    class: "A reader of the system prompt that reads the argument rather than the file.",
    files: (tree) => [...programs(tree), rig("rig.sh")],
    pattern: /system-args/,
    reading: "grep the rig's programs and rig.sh for `system-args`",
    rule: "an instance where a line outside run-names.ts (systemPromptOf, the one reader) reads the file (readFileSync, Bun.file, `cat`, `<`); a true statement for its writer, for rig.sh passing the argument on, and for a place's name",
    verdict: ({ file, text }) =>
      file !== rig("run-names.ts") &&
      /readFileSync|Bun\.file|\bcat\b/.test(text) &&
      !/\$\(<"\$R\/system-args"\)/.test(text)
        ? instance("reads the system prompt's argument past systemPromptOf")
        : holds("the one reader, a writer, the argument passed on, or a place's name"),
  }),
  grepCheck({
    name: "hand search 40: an identity a commit sets that the environment beats",
    class: "An identity a commit sets in a way the environment can beat.",
    files: (tree) => [
      ...programs(tree),
      rig("rig.sh"),
      ...tree.files("scripts/e2e/lib", (file) => TEXT_FILE.test(file), true),
    ],
    pattern: /-c user\.(?:name|email)/,
    reading:
      "grep the rig's programs, rig.sh and scripts/e2e/lib for `-c user.name` and `-c user.email`",
    rule: "every hit is an instance: GIT_AUTHOR_* and GIT_COMMITTER_* of the environment beat `-c user.*` (git-commit(1)), and the rig sets its identity through them",
    verdict: () => instance("sets a commit's identity the environment beats"),
  }),
  grepCheck({
    name: "hand search 41: a database a shared server keeps",
    class: "A database a shared server keeps.",
    files: tests,
    pattern: /CREATE DATABASE/,
    reading: "grep the rig's tests for CREATE DATABASE",
    rule: "a line that tests a text for the statement (`.includes(`, `.test(`, `.exec(`, `.match(`, a regex) reads a record of one and makes none, a true statement; any other line sends it: a true statement where the same file drops what it makes (DROP DATABASE), an instance where it does not",
    verdict: ({ file, text }, tree) =>
      /\.(?:includes|test|exec|match|matchAll)\(|\/[^/\n]*CREATE DATABASE[^/\n]*\//.test(text)
        ? holds("reads a record of the statement, and makes no database")
        : /DROP DATABASE/.test(tree.text(file))
          ? holds("the file drops what it makes")
          : instance("makes a database it does not drop"),
  }),
];

/** Every check, in the order the search runs and prints them. */
export const CHECKS: readonly Check[] = [
  writeRoutes,
  ghWriteCommands,
  noCatchAll,
  appPushes,
  reads,
  productionAnswers,
  remoteHelperKey,
  printedCredential,
  runRecords,
  worldContent,
  readersArchive,
  readersText,
  notModelled,
  defaultBound,
  ...greps,
];

// ---------------------------------------------------------------------------------------------
// The data: the residuals, the declarations no scenario reaches, and the hand search's map.

const Residual = z.strictObject({
  name: z.string(),
  check: z.string(),
  at: z.string(),
  ruled: z.string(),
  words: z.string(),
  source: z.string(),
});
export type Residual = z.infer<typeof Residual>;
/** A declaration of a behaviour not modelled that no scenario's world reaches: the check that
 * finds it, the pattern of the hit it covers (which holds the `Unreachable:` sentence the
 * declaration writes beside it), and why, shown by what the worlds serve. */
const Unreachable = z.strictObject({
  name: z.string(),
  check: z.string(),
  at: z.string(),
  reason: z.string(),
});
export type Unreachable = z.infer<typeof Unreachable>;
const Search = z.strictObject({
  entry: z.number(),
  finding: z.string(),
  check: z.array(z.string()).optional(),
  held: z.string().optional(),
  notMechanical: z.string().optional(),
});
const Data = z.strictObject({
  residuals: z.array(Residual),
  unreachable: z.array(Unreachable),
  searches: z.array(Search),
});
export type Data = z.infer<typeof Data>;
/** The data file, checked: each residual and each unreachable entry names a check, and each entry
 * of the hand search names a check, a row that holds it, or why no mechanical search can, and
 * nothing else. */
export function readData(text: string): Data {
  const data = Data.parse(JSON.parse(text));
  const names = new Set(CHECKS.map(({ name }) => name));
  for (const residual of data.residuals)
    if (!names.has(residual.check))
      throw new Error(`residual ${residual.name} names no check: ${residual.check}`);
  for (const entry of data.unreachable)
    if (!names.has(entry.check))
      throw new Error(`unreachable entry ${entry.name} names no check: ${entry.check}`);
  for (const search of data.searches) {
    const given = [search.check ?? search.held, search.notMechanical].filter(
      (one) => one !== undefined
    );
    if (given.length !== 1)
      throw new Error(
        `hand search entry ${search.entry} names ${given.length === 0 ? "neither" : "both"} a check or a row that holds it, and why no mechanical search can`
      );
    for (const check of search.check ?? [])
      if (!names.has(check))
        throw new Error(`hand search entry ${search.entry} names no check: ${check}`);
  }
  return data;
}

/** What the search found: each check's hits, each instance as a finding, as the residual that
 * covers it or as the unreachable entry that does, and each residual and each unreachable entry no
 * instance is of any more. */
export interface Result {
  readonly checks: { check: Check; hits: Hit[] }[];
  readonly findings: { check: Check; hit: Hit }[];
  readonly residuals: { residual: Residual; hits: Hit[] }[];
  readonly stale: Residual[];
  readonly unreachable: { entry: Unreachable; hits: Hit[] }[];
  readonly staleUnreachable: Unreachable[];
}
export function judged(
  checks: { check: Check; hits: Hit[] }[],
  residuals: readonly Residual[],
  unreachable: readonly Unreachable[] = []
): Result {
  const covered = residuals.map((residual) => ({ residual, hits: [] as Hit[] }));
  const declared = unreachable.map((entry) => ({ entry, hits: [] as Hit[] }));
  const covers = (one: { check: string; at: string }, check: Check, hit: Hit) =>
    one.check === check.name && new RegExp(one.at).test(`${hit.at} ${hit.text}`);
  const findings: Result["findings"] = [];
  for (const { check, hits } of checks)
    for (const hit of hits) {
      if (hit.verdict !== "instance") continue;
      const by =
        covered.find(({ residual }) => covers(residual, check, hit)) ??
        declared.find(({ entry }) => covers(entry, check, hit));
      if (by === undefined) findings.push({ check, hit });
      else by.hits.push(hit);
    }
  return {
    checks,
    findings,
    residuals: covered.filter(({ hits }) => hits.length > 0),
    stale: covered.filter(({ hits }) => hits.length === 0).map(({ residual }) => residual),
    unreachable: declared.filter(({ hits }) => hits.length > 0),
    staleUnreachable: declared.filter(({ hits }) => hits.length === 0).map(({ entry }) => entry),
  };
}

/** Bytes in a mebibyte. */
const MIB = 2 ** 20;
/** The free space of the file system under each directory of `dirs`, each `[name, dir]`, as
 * statfs reads it at the moment of the call: what an unprivileged writer may still take of it and
 * its size, in MiB, or why it could not be read. A run of 549 s at 7812df81 exited 2 with both
 * streams empty (2026-11-21), which fits a write that failed, as a full disk fails one; these
 * readings say whether the disk was full. */
export function diskReadings(dirs: readonly (readonly [string, string])[]): string {
  return dirs
    .map(([name, dir]) => {
      try {
        const { bavail, blocks, bsize } = statfsSync(dir);
        const free = Math.floor((bavail * bsize) / MIB);
        return `${name} ${dir}: ${free} MiB free of ${Math.floor((blocks * bsize) / MIB)} MiB`;
      } catch (error) {
        return `${name} ${dir}: unread (${error instanceof Error ? error.message : String(error)})`;
      }
    })
    .join("; ");
}

/** The search's printout, opening with the readings of the disks at the search's start and at its
 * end (`diskReadings`). */
export function report(
  result: Result,
  data: Data,
  head: string,
  disk: { readonly start: string; readonly end: string }
): string {
  const out: string[] = [
    `class search of ${head}`,
    `disk at the start: ${disk.start}`,
    `disk at the end: ${disk.end}`,
    "",
  ];
  for (const { check, hits } of result.checks) {
    out.push(
      `== ${check.name}`,
      `class: ${check.class}`,
      `reading: ${check.reading}`,
      `rule: ${check.rule}`
    );
    out.push(
      `hits: ${hits.length} (${hits.filter((hit) => hit.verdict === "instance").length} instances)`
    );
    for (const hit of hits)
      out.push(
        `  ${hit.at}  ${cut(hit.text)}  — ${hit.verdict === "instance" ? "INSTANCE" : "true"}: ${hit.why}`
      );
    out.push("");
  }
  out.push(`== findings outside the residuals: ${result.findings.length}`);
  for (const { check, hit } of result.findings)
    out.push(`  ${check.name}: ${hit.at}  ${cut(hit.text)}`);
  out.push("", `== residuals found: ${result.residuals.length} of ${data.residuals.length}`);
  for (const { residual, hits } of result.residuals) {
    out.push(
      `  ${residual.name} (${residual.check}), ruled ${residual.ruled}: ${residual.words} (${residual.source})`
    );
    for (const hit of hits) out.push(`    ${hit.at}  ${cut(hit.text, 90)}`);
  }
  out.push("", `== stale residuals (declared, and no instance found): ${result.stale.length}`);
  for (const residual of result.stale) out.push(`  ${residual.name} (${residual.check})`);
  out.push(
    "",
    `== declared unreachable found: ${result.unreachable.length} of ${data.unreachable.length}`
  );
  for (const { entry, hits } of result.unreachable) {
    out.push(`  ${entry.name} (${entry.check}): ${entry.reason}`);
    for (const hit of hits) out.push(`    ${hit.at}  ${cut(hit.text, 90)}`);
  }
  out.push(
    "",
    `== stale unreachable entries (declared, and no declaration found): ${result.staleUnreachable.length}`
  );
  for (const entry of result.staleUnreachable) out.push(`  ${entry.name} (${entry.check})`);
  out.push("", `== the hand search of f853f11b: ${data.searches.length} entries`);
  for (const search of data.searches) {
    const mapped = [
      ...(search.check === undefined ? [] : [`check: ${search.check.join("; ")}`]),
      ...(search.held === undefined ? [] : [`held: ${search.held}`]),
      ...(search.notMechanical === undefined ? [] : [`not mechanical: ${search.notMechanical}`]),
    ];
    out.push(`  ${search.entry}. ${cut(search.finding, 80)} → ${mapped.join("; ")}`);
  }
  out.push(
    "",
    `result: ${result.findings.length} findings outside the residuals, ${result.residuals.length} residuals found, ${result.unreachable.length} declared unreachable found, ${result.stale.length + result.staleUnreachable.length} stale`
  );
  return `${out.join("\n")}\n`;
}

/** Runs every check of `checks` over `tree`, each check that throws named and the search
 * stopped. */
export async function search(tree: Tree, worlds: Worlds, checks = CHECKS) {
  const done: { check: Check; hits: Hit[] }[] = [];
  for (const check of checks) {
    try {
      done.push({ check, hits: await check.run(tree, worlds) });
    } catch (error) {
      throw new Error(`${check.name}: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
  return done;
}

/** The exit status of a search whose report or error line could not be written whole, apart from
 * 1 (findings) and 2 (a check could not run): the output a caller holds is then not the search's. */
export const WRITE_FAILED = 3;

/** Writes all of `text` to file descriptor `fd` with synchronous writes, each checked, or answers
 * why it could not: the error of the write that failed and how much of `text` went before it. A
 * descriptor that answers EAGAIN, a pipe whose reader has not drained it, is written again. */
export function writeWhole(fd: number, text: string): string | undefined {
  const bytes = Buffer.from(text);
  let written = 0;
  while (written < bytes.length) {
    try {
      written += writeSync(fd, bytes, written, bytes.length - written);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "EAGAIN") {
        Bun.sleepSync(1);
        continue;
      }
      return `${error instanceof Error ? error.message : String(error)}, after ${written} of ${bytes.length} bytes`;
    }
  }
  return undefined;
}

/** Writes the search's `report` to the descriptor `out` and its `error` line to `err`, each whole
 * (`writeWhole`), and answers the exit status: `status` where every write went through, else
 * WRITE_FAILED, with a line naming each write that failed and the search's own status written to
 * `err`, or to `out` where `err` takes nothing. The search wrote its report once at its end with
 * `process.stdout.write` and its error with `console.error`, neither checked, so a run whose
 * writes failed exited 2 with nothing on either stream and nothing said why. */
export function emit(
  output: { readonly report?: string; readonly error?: string },
  status: number,
  fds: { readonly out: number; readonly err: number }
): number {
  const failed: string[] = [];
  const write = (what: string, stream: string, fd: number, text: string | undefined) => {
    const why = text === undefined ? undefined : writeWhole(fd, text);
    if (why !== undefined)
      failed.push(`the ${what}'s write to ${stream} (fd ${fd}) failed: ${why}`);
  };
  write("report", "stdout", fds.out, output.report);
  write("error", "stderr", fds.err, output.error);
  if (failed.length === 0) return status;
  const line = `class-search: ${failed.join("; ")}; the search's own status was ${status}\n`;
  if (writeWhole(fds.err, line) !== undefined) writeWhole(fds.out, line);
  return WRITE_FAILED;
}

if (import.meta.main) {
  const fds = { out: 1, err: 2 };
  if (process.argv.length > 2)
    process.exit(emit({ error: "usage: rig.sh class-search [<checkout>]\n" }, 2, fds));
  const disks = [
    ["the checkout", CHECKOUT],
    ["$TMPDIR", os.tmpdir()],
  ] as const;
  const start = diskReadings(disks);
  const tree = new Tree(CHECKOUT);
  const worlds = new Worlds(tree);
  let output: { report?: string; error?: string };
  let status: number;
  try {
    const data = readData(readFileSync(DATA, "utf8"));
    const head = Bun.spawnSync(["git", "-C", CHECKOUT, "rev-parse", "HEAD"])
      .stdout.toString()
      .trim();
    const result = judged(await search(tree, worlds), data.residuals, data.unreachable);
    const named = head === "" ? CHECKOUT : `${CHECKOUT} at ${head}`;
    output = { report: report(result, data, named, { start, end: diskReadings(disks) }) };
    status =
      result.findings.length === 0 &&
      result.stale.length === 0 &&
      result.staleUnreachable.length === 0
        ? 0
        : 1;
  } catch (error) {
    output = {
      error: [
        `class-search: ${error instanceof Error ? error.message : String(error)}`,
        `class-search: disk at the start: ${start}`,
        `class-search: disk at the end: ${diskReadings(disks)}`,
        "",
      ].join("\n"),
    };
    status = 2;
  } finally {
    worlds.done();
  }
  process.exitCode = emit(output, status, fds);
}
