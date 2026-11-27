// Which `gh` calls write to GitHub, read from production's side: gh's own commands at the tag the
// worker image pins (`ARG GH_TOOL=gh@2.98.0` in packages/daemon/docker/worker.Dockerfile), each
// with whether it writes, read out of gh's source by gh-writes.go and never written by hand
// (gh-writes.json); for `gh api`, the request the call names: the method gh sends, and for
// GraphQL the operation the document itself defines. Rule 20 of `rig.sh health` (health.ts) reads
// a recorded call by this (`callWrites`).
// It once read it by the world's own router (gh-routes.ts's `routeOf`), whose `kind` called every
// `gh pr`, `gh run` and `gh auth` command a write unless the router listed it as a read, and no
// command of another group a write at all: a write the router had no route for was no write to
// health, so rule 20 passed it, and the check rested on the thing it checks (the architect's
// ruling of 2026-11-18T15:05Z). `routeOf` says which handler answered a call; it never says
// whether the call wrote.
//
//   curl -fsSL -o gh.tgz https://codeload.github.com/cli/cli/tar.gz/refs/tags/v<version>
//   bun gh-writes.ts gh.tgz
//
// reads the commit the archive was made from (`git get-tar-commit-id`), unpacks it into a scratch
// directory, runs gh-writes.go over it (outside every module, as daemon-pane.ts runs
// gh-selectors.go), and writes gh-writes.json with the tag and that commit, formatted as the
// repository's biome does. The archive is gh's public source, read with no credential: the Legion
// gh wrapper refuses the owner `cli`, and a directory of the source is no file
// raw.githubusercontent.com serves. A bump of the pin fails gh-writes.test.ts until the file is
// derived again from the new tag's source.
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { z } from "zod";
import { endpointPath, methodOf } from "./gh-call";
import { graphqlSent, type Recorded } from "./gh-threads";
import extract from "./gh-writes.json";

/** The extract this directory keeps, and the program that derives it. */
export const GH_WRITES = path.join(import.meta.dir, "gh-writes.json");
const PROGRAM = path.join(import.meta.dir, "gh-writes.go");
/** The worker image's Dockerfile, whose `ARG GH_TOOL=gh@<version>` pins the gh every pane runs. */
export const WORKER_DOCKERFILE = path.join(
  import.meta.dir,
  "..",
  "..",
  "..",
  "daemon",
  "docker",
  "worker.Dockerfile"
);

/** One command of gh's, as gh-writes.go prints it: the file of its `cobra.Command`, its aliases,
 * whether gh hides it, whether it runs on its own (one that does not prints its help), whether a
 * call of it is read by the request it names (`byRequest`, `gh api`), and whether it writes, with
 * the first write gh-writes.go's reading of it found. */
const Command = z.strictObject({
  file: z.string(),
  aliases: z.array(z.string()).optional(),
  hidden: z.boolean().optional(),
  runs: z.boolean(),
  byRequest: z.boolean().optional(),
  writes: z.boolean(),
  write: z.string().optional(),
});
const Extract = z.strictObject({
  made_by: z.string(),
  source: z.literal("cli/cli"),
  tag: z.string().regex(/^v\d+\.\d+\.\d+$/),
  commit: z.string().regex(/^[0-9a-f]{40}$/),
  commands: z.record(z.string(), Command),
});

/** The extract, as gh-writes.json holds it. */
export const GH_COMMANDS = Extract.parse(extract);

/** The tag of gh the worker image pins, read off its Dockerfile (`ARG GH_TOOL=gh@2.98.0` is
 * `v2.98.0`, the tag cli/cli releases that version under). */
export function pinnedTag(dockerfile = WORKER_DOCKERFILE): string {
  const pin = /^ARG GH_TOOL=gh@(\d+\.\d+\.\d+)$/m.exec(readFileSync(dockerfile, "utf8"));
  if (pin === null) throw new Error(`${dockerfile} pins no gh (\`ARG GH_TOOL=gh@<version>\`)`);
  return `v${pin[1]}`;
}

/** The commands under each command, by every word that names one (its name and each alias); the
 * root's are under "". */
const CHILDREN = new Map<string, Map<string, string>>();
for (const [command, { aliases }] of Object.entries(GH_COMMANDS.commands)) {
  const words = command.split(" ");
  const parent = words.slice(0, -1).join(" ");
  const named = CHILDREN.get(parent) ?? new Map<string, string>();
  for (const word of [words.at(-1) ?? "", ...(aliases ?? [])]) named.set(word, command);
  CHILDREN.set(parent, named);
}

/** The methods HTTP defines as safe (RFC 9110, 9.2.1), which change nothing of the server's. */
const SAFE_METHODS = ["GET", "HEAD", "OPTIONS", "TRACE"];

/** Whether the request a `gh api <argv...>` call names writes: a GraphQL call (to the endpoint
 * `graphql`) whose document runs a mutation (`graphqlWrites`), and any other a call gh sends with
 * a method other than a safe one: GET unless the call gives a field or `--input`, which make it a
 * POST, or names one with `-X`/`--method` (gh-call.ts's `methodOf`, gh's own reading at
 * pkg/cmd/api/api.go:289 and :325 of v2.98.0). So a GET never reads as a write. */
function requestWrites(call: Recorded): boolean {
  if (endpointPath(call.argv) === "graphql") {
    const { query, operationName } = graphqlSent(call);
    return graphqlWrites(query, operationName);
  }
  return !SAFE_METHODS.includes(methodOf(call.argv));
}

/** Whether the recorded call `gh <argv...>` writes to GitHub, read from production's side and
 * never from the world's router. Its command is found as cobra finds it: each word that is no
 * option names the command under the one before it, by its name or an alias, until a command that
 * adds none. An option before them (`gh pr -R o/r merge 1`) is passed over, and with it the word
 * after it when the option may take that word as its value, which only the option's own
 * definition says: either reading that reaches a write makes the call a write. A command of gh's
 * own writes as gh-writes.json reads it, but one read by its request (`byRequest`, `gh api`),
 * which writes as that request does (`requestWrites`). A word no command under a group names is
 * the group's own argument where the group runs on its own, and an unknown command where it does
 * not, as is a call that names none: neither runs anything of gh's, nor does an alias or an
 * extension of a configuration's. */
export function callWrites(call: Recorded): boolean {
  const { argv } = call;
  const reads = (at: string, from: number): boolean => {
    const command = at === "" ? undefined : GH_COMMANDS.commands[at];
    if (command?.byRequest === true)
      return requestWrites({ ...call, argv: ["api", ...argv.slice(from)] });
    const own = command?.writes ?? false;
    const under = CHILDREN.get(at);
    if (under === undefined) return own;
    for (let index = from; index < argv.length; index++) {
      const word = argv[index] ?? "";
      // cobra's own reading: `--` ends the options and with them the command's words.
      if (word === "--") return own;
      if (word.startsWith("-")) {
        const mayTakeNext = !word.includes("=") && (word.startsWith("--") || word.length === 2);
        if (mayTakeNext && reads(at, index + 2)) return true;
        continue;
      }
      const child = under.get(word);
      return child === undefined ? own : reads(child, index + 1);
    }
    return own;
  };
  return reads("", 0);
}

/** Each operation a GraphQL document defines, read by the document's own grammar (the GraphQL
 * specification's ExecutableDocument, not by any parser of the world's): its type, a selection
 * set alone being a query, and its name where it has one. Read token by token: white space, commas
 * and comments are passed over, and a string is one token, so a `mutation` in a string, an
 * argument or a selection set, at any depth of brackets, is no operation. A fragment is no
 * operation. */
export function graphqlOperations(text: string): { type: string; name?: string }[] {
  const operations: { type: string; name?: string }[] = [];
  let depth = 0;
  let opened: { type: string; name?: string } | undefined;
  const tokens = graphqlTokens(text);
  for (const [index, token] of tokens.entries()) {
    if (depth === 0) {
      if (token === "{") {
        operations.push(opened ?? { type: "query" });
        opened = undefined;
      } else if (opened === undefined && /^(query|mutation|subscription|fragment)$/.test(token)) {
        const name = tokens[index + 1] ?? "";
        opened = { type: token, ...(/^[_A-Za-z]/.test(name) ? { name } : {}) };
      }
    }
    if (token === "{" || token === "(" || token === "[") depth++;
    else if (token === "}" || token === ")" || token === "]") depth = Math.max(0, depth - 1);
  }
  return operations.filter((operation) => operation.type !== "fragment");
}

/** The tokens of a GraphQL text: names, punctuators one character each but `...`, and each
 * string, a block string (`"""…"""`) among them, as `""`. */
function graphqlTokens(text: string): string[] {
  const tokens: string[] = [];
  let at = 0;
  while (at < text.length) {
    const char = text[at] ?? "";
    if (char === "#") {
      while (at < text.length && text[at] !== "\n" && text[at] !== "\r") at++;
    } else if (/[\s,\uFEFF]/.test(char)) at++;
    else if (text.startsWith('"""', at)) {
      at += 3;
      while (at < text.length && !text.startsWith('"""', at))
        at += text.startsWith('\\"""', at) ? 4 : 1;
      at += 3;
      tokens.push('""');
    } else if (char === '"') {
      at++;
      while (at < text.length && text[at] !== '"' && text[at] !== "\n")
        at += text[at] === "\\" ? 2 : 1;
      at++;
      tokens.push('""');
    } else {
      const name = /^[_A-Za-z][_0-9A-Za-z]*/.exec(text.slice(at))?.[0];
      const token = name ?? (text.startsWith("...", at) ? "..." : char);
      tokens.push(token);
      at += token.length;
    }
  }
  return tokens;
}

/** Whether GitHub runs a mutation for a GraphQL document sent with `operationName`: the
 * operation of that name, or the document's one operation where the call names none, as the
 * specification's GetOperation chooses it. A document of several operations sent with no name,
 * or with a name none of them has, is refused before anything of it runs. */
export function graphqlWrites(text: string, operationName: string | undefined): boolean {
  const operations = graphqlOperations(text);
  const chosen =
    operationName === undefined || operationName === ""
      ? operations.length === 1
        ? operations[0]
        : undefined
      : operations.find((operation) => operation.name === operationName);
  return chosen?.type === "mutation";
}

/** Runs `command` in this directory, returning its stdout; exits naming it, with its stderr, when
 * it fails. */
function run(command: string[], env: Record<string, string> = {}): string {
  const result = Bun.spawnSync(command, { cwd: import.meta.dir, env: { ...process.env, ...env } });
  if (result.exitCode !== 0) {
    console.error(`${command.join(" ")} failed:\n${result.stderr.toString()}`);
    process.exit(1);
  }
  return result.stdout.toString();
}

if (import.meta.main) {
  const [given, ...rest] = Bun.argv.slice(2);
  if (given === undefined || rest.length > 0) {
    console.error("usage: bun gh-writes.ts <cli/cli source archive of the pinned tag>");
    process.exit(2);
  }
  const archive = path.resolve(given);
  const tag = pinnedTag();
  const commit = run(["sh", "-c", 'gunzip -c "$0" | git get-tar-commit-id', archive]).trim();
  const scratch = mkdtempSync(path.join(tmpdir(), "gh-writes-"));
  try {
    run(["tar", "-xzf", archive, "-C", scratch]);
    const source = path.join(scratch, `cli-${tag.slice(1)}`);
    if (!existsSync(source)) {
      console.error(`${archive} holds no cli-${tag.slice(1)}: it is no source of gh ${tag}`);
      process.exit(1);
    }
    const commands: unknown = JSON.parse(run(["go", "run", PROGRAM, source], { GOWORK: "off" }));
    const derived = Extract.parse({
      made_by:
        "gh-writes.ts (bun gh-writes.ts <archive>), from gh's source at the tag the worker image pins, read by gh-writes.go",
      source: "cli/cli",
      tag,
      commit,
      commands,
    });
    writeFileSync(GH_WRITES, `${JSON.stringify(derived, null, 2)}\n`);
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
  const formatted = Bun.spawnSync(["bunx", "biome", "format", "--write", GH_WRITES], {
    stdout: "inherit",
    stderr: "inherit",
  });
  process.exit(formatted.exitCode);
}
