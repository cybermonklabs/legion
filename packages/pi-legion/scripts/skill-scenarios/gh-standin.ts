#!/usr/bin/env bun
// Stand-in for `gh` on a skill scenario's PATH. Each call is answered from the first route in
// $SKILL_SCENARIO_RUN/fixtures.json whose regex matches "gh <argv...>", and recorded to
// $SKILL_SCENARIO_RUN/calls.jsonl. A call no route matches answers `gh: Not Found (HTTP 404)` and
// exits 1. Nothing is sent anywhere.
//
// The call's arguments are read once, as gh 2.98.0 reads them (readGhArgs, from each command's
// flags as its help prints them: testdata/f9-gh-help.json), and that one reading decides all the
// stand-in does with the call. Its record holds the argv, the contents of each file or stdin it
// hands GitHub a body through (`files`, `stdin`), the route, its exit status, when it began (`at`)
// and ended (`done`), the request it makes (`request`, ghRequest), the body it left a pull request
// with (`body`), and, where the stand-in took a call that changes a pull request or a review, that
// change (`pull`). The daemon stand-in, outcome.ts and lane D's replay take what a call did from
// `pull` alone. A call the stand-in refused records no change and changes no answer.
//
// The changes (`pull.action`, each with the pull request it changed, `number`): `open` (`gh pr
// create`, or a `gh api` POST to `repos/<o>/<r>/pulls`: the number, branch, base and head commit it
// gave the new one); `edit` (`gh pr edit`, or a PATCH of `…/pulls/<n>` that names no state: the
// pull request in `numbers`, the body it has after the edit, and the title the edit set); `review`
// (`gh pr review`, or a POST to `…/pulls/<n>/reviews`: the review it added); `review-comment` (a
// POST to `…/pulls/<n>/comments` or a reply under it); `comment` (`gh pr comment`, or a POST to
// `repos/<o>/<r>/issues/<n>/comments` of a pull request); `merge` (`gh pr merge`, or a PUT of
// `…/pulls/<n>/merge`: the method, and whether it waits for the requirements, `auto`) and
// `disable-auto`; `close` and `reopen` (`gh pr close`, `gh pr reopen`, or a PATCH naming the
// state); `ready` and `draft` (`gh pr ready`, `--undo`); `enqueue` and `dequeue` (the merge queue's
// GraphQL mutations); and, by their own names, every other `gh pr` command that is no read, any
// other GraphQL mutation (`mutation`, its `name`) and any other write under a pull request
// (`request`, its method and path). A `gh pr` command that changes a pull request names it as gh
// does: a number, a URL or a branch, or none for the one open pull request of the issue's branch
// (`legion/<issue>`, the branch the run's workspace stands for). One that names a pull request the
// routes hold none of, or names none where that branch has no single open one, fails as gh fails.
//
// `--json a,b` picks those fields of the answer, or of each object of a list answer (`gh pr list`),
// and `--jq`/`-q` runs jq, as gh does. A body edit is kept on that pull request alone: every route
// answering with it, wherever in the answer it stands, then answers with the new body, as GitHub's
// next read would; its reviews' and comments' bodies are their own. A review is kept on the one pull
// request the call names: the POST answers with the review it created, and every route answering
// with that pull request or with its review list then holds the review, the pull request's
// `reviewDecision` following. `gh pr review` takes exactly one of `--approve`, `--request-changes`
// and `--comment`, the last two with a body, or is refused with gh's words. A pull request opened is
// kept as GitHub keeps it: from the next call on, every read of it (by number, branch or URL, its
// reviews, its comments, `gh pr list` of its branch or of every open pull request, the REST list)
// and every write to it answers with it, and its head is its branch's head on the run's remote,
// moving with each push. Its number is one above every number the routes hold, as lane D numbers
// it; it opens the issue's branch when no head is named. GitHub's refusals hold: a head the remote
// does not hold, or a branch that has an open pull request into the same base, is refused with gh's
// own words, exit status 1. Help (`--help`, `-h`, `gh help`) and `gh --version` (`gh version`) reach
// no GitHub: the real gh answers them, the first executable `gh` on PATH outside the run's own bin
// directory that is no script (the daemon's rule for the real gh: this stand-in's wrapper and
// Legion's `legion gh` wrapper are scripts). They are recorded to gh-local.jsonl, not calls.jsonl,
// so none of them is a GitHub call: `gh pr create --help` opens no pull request.
// A route that holds `pages` (each page the whole JSON answer as GitHub sends it: a GraphQL answer
// with its `pageInfo {hasNextPage endCursor}`, or a list) answers a paged read as gh does. With
// `--paginate`: every page, as one JSON text per page with nothing between them (plain), each
// page through `--jq`'s filter (`--jq`), or one array of the pages (`--slurp`); `--slurp` with
// `--jq` or `--template`, or without `--paginate`, is refused as gh refuses it (its sentence, its
// usage, exit status 1); pages that are all lists are one list (plain). Without `--paginate`: the
// page after the one whose `endCursor` is the call's `endCursor` (or `after`) variable, the first
// page where it names none. `--jq` prints a string raw and any other value compact with its keys
// sorted, as gh's does (testdata/f7-*, captured from gh 2.98.0).
//   gh-standin.ts gh <args...>
import {
  accessSync,
  appendFileSync,
  constants,
  existsSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { z } from "zod";

/** Each command whose flags the stand-in reads, with its flags as gh 2.98.0 prints them in `gh
 * <command> --help` (testdata/f9-gh-help.json): `<letter> <name>`, or `<name>` for a flag with no
 * one-letter form, and `=` after the name of a flag that takes a value. */
export const GH_FLAGS: Readonly<Record<string, readonly string[]>> = {
  api: [
    "allow-escape-sequences",
    "cache=",
    "F field=",
    "H header=",
    "hostname=",
    "i include",
    "input=",
    "q jq=",
    "X method=",
    "paginate",
    "p preview=",
    "f raw-field=",
    "silent",
    "slurp",
    "t template=",
    "verbose",
    "help",
  ],
  "pr create": [
    "a assignee=",
    "B base=",
    "b body=",
    "F body-file=",
    "d draft",
    "dry-run",
    "e editor",
    "f fill",
    "fill-first",
    "fill-verbose",
    "H head=",
    "l label=",
    "m milestone=",
    "no-maintainer-edit",
    "p project=",
    "recover=",
    "r reviewer=",
    "T template=",
    "t title=",
    "w web",
    "help",
    "R repo=",
  ],
  "pr edit": [
    "add-assignee=",
    "add-label=",
    "add-project=",
    "add-reviewer=",
    "B base=",
    "b body=",
    "F body-file=",
    "m milestone=",
    "remove-assignee=",
    "remove-label=",
    "remove-milestone",
    "remove-project=",
    "remove-reviewer=",
    "t title=",
    "help",
    "R repo=",
  ],
  "pr review": [
    "a approve",
    "b body=",
    "F body-file=",
    "c comment",
    "r request-changes",
    "help",
    "R repo=",
  ],
  "pr merge": [
    "admin",
    "A author-email=",
    "auto",
    "b body=",
    "F body-file=",
    "d delete-branch",
    "disable-auto",
    "match-head-commit=",
    "m merge",
    "r rebase",
    "s squash",
    "t subject=",
    "help",
    "R repo=",
  ],
  "pr close": ["c comment=", "d delete-branch", "help", "R repo="],
  "pr reopen": ["c comment=", "help", "R repo="],
  "pr ready": ["undo", "help", "R repo="],
  "pr comment": [
    "b body=",
    "F body-file=",
    "create-if-none",
    "delete-last",
    "edit-last",
    "e editor",
    "w web",
    "yes",
    "help",
    "R repo=",
  ],
  "pr view": ["c comments", "q jq=", "json=", "t template=", "w web", "help", "R repo="],
  "pr list": [
    "app=",
    "a assignee=",
    "A author=",
    "B base=",
    "d draft",
    "H head=",
    "q jq=",
    "json=",
    "l label=",
    "L limit=",
    "S search=",
    "s state=",
    "t template=",
    "w web",
    "help",
    "R repo=",
  ],
  "pr checks": [
    "fail-fast",
    "i interval=",
    "q jq=",
    "json=",
    "required",
    "t template=",
    "watch",
    "w web",
    "help",
    "R repo=",
  ],
  "pr diff": [
    "allow-escape-sequences",
    "color=",
    "e exclude=",
    "name-only",
    "patch",
    "w web",
    "help",
    "R repo=",
  ],
  "run view": [
    "a attempt=",
    "exit-status",
    "j job=",
    "q jq=",
    "json=",
    "log",
    "log-failed",
    "t template=",
    "v verbose",
    "w web",
    "help",
    "R repo=",
  ],
  "run list": [
    "a all",
    "b branch=",
    "c commit=",
    "created=",
    "e event=",
    "q jq=",
    "json=",
    "L limit=",
    "s status=",
    "t template=",
    "u user=",
    "w workflow=",
    "help",
    "R repo=",
  ],
};

/** The one-letter flags of a command GH_FLAGS does not hold, by the name the record gives them. */
const LETTERS: Readonly<Record<string, string>> = {
  b: "body",
  B: "base",
  F: "body-file",
  H: "head",
  R: "repo",
  q: "jq",
  t: "title",
};

/** The commands of one word: any other first word is a group, and its second word the command. */
const SINGLE = ["api", "browse", "status"];

/** A gh call's arguments as gh reads them. */
export interface GhArgs {
  /** `api`, `pr view`, `run list`: the first word, and the second where the first is a group. */
  readonly command: string;
  /** The arguments that are no flag and no flag's value, in order. */
  readonly positional: readonly string[];
  /** Each flag the call set, by its name, with the value it took each time, in order: `true` for a
   * flag that takes none. */
  readonly flags: ReadonlyMap<string, readonly (string | true)[]>;
}

/** `argv` read as gh 2.98.0 reads it: `--name value`, `--name=value`, `-x value`, `-xvalue`,
 * `-x=value`, one-letter flags that take no value joined (`-cb text` is `-c` and `-b text`), flags
 * between the other arguments, and `--` ending the flags. A flag that takes no value names `false`
 * as `--name=false` to stay unset. A command GH_FLAGS does not hold is read without its flags: a
 * flag takes the next argument where that is no flag, and a one-letter flag is named by LETTERS or
 * as `-x`. */
export function readGhArgs(argv: readonly string[]): GhArgs {
  const words =
    SINGLE.includes(argv[0] ?? "") || argv[1] === undefined || argv[1].startsWith("-") ? 1 : 2;
  const command = argv.slice(0, words).join(" ");
  const known = GH_FLAGS[command];
  const takes = new Map<string, boolean>();
  const letters = new Map<string, string>();
  for (const spec of known ?? []) {
    const [first = "", second] = spec.split(" ");
    const name = (second ?? first).replace(/=$/, "");
    takes.set(name, (second ?? first).endsWith("="));
    if (second !== undefined) letters.set(first, name);
  }
  const flags = new Map<string, (string | true)[]>();
  const set = (name: string, value: string | true) =>
    flags.set(name, [...(flags.get(name) ?? []), value]);
  const positional: string[] = [];
  for (let index = words; index < argv.length; index += 1) {
    const arg = argv[index] ?? "";
    const next = argv[index + 1];
    /** Whether flag `name`, given no value of its own, takes the next argument. */
    const valued = (name: string) =>
      takes.get(name) ?? (known === undefined && next !== undefined && !next.startsWith("-"));
    if (arg === "--") {
      positional.push(...argv.slice(index + 1));
      break;
    }
    if (arg.startsWith("--")) {
      const equals = arg.indexOf("=");
      const name = arg.slice(2, equals < 0 ? undefined : equals);
      if (equals >= 0 && takes.get(name) === false) {
        if (arg.slice(equals + 1) !== "false") set(name, true);
      } else if (equals >= 0) set(name, arg.slice(equals + 1));
      else if (!valued(name)) set(name, true);
      else if (next !== undefined) {
        set(name, next);
        index += 1;
      }
      continue;
    }
    if (!arg.startsWith("-") || arg === "-") {
      positional.push(arg);
      continue;
    }
    for (let at = 1; at < arg.length; at += 1) {
      const letter = arg[at] ?? "";
      const name =
        letters.get(letter) ?? (known === undefined ? LETTERS[letter] : undefined) ?? `-${letter}`;
      const rest = arg.slice(at + 1);
      if (takes.get(name) ?? (known === undefined && arg.length === 2 && valued(name))) {
        if (rest !== "") set(name, rest.startsWith("=") ? rest.slice(1) : rest);
        else if (next !== undefined) {
          set(name, next);
          index += 1;
        }
        break;
      }
      if (rest.startsWith("=")) {
        if (rest.slice(1) !== "false") set(name, true);
        break;
      }
      set(name, true);
    }
  }
  return { command, positional, flags };
}

/** The value the call last gave flag `name`, which is the one gh keeps. */
function last(args: GhArgs, name: string): string | true | undefined {
  return args.flags.get(name)?.at(-1);
}

/** The text the call last gave flag `name`. */
function given(args: GhArgs, name: string): string | undefined {
  const value = last(args, name);
  return typeof value === "string" ? value : undefined;
}

/** The pull request a `gh pr` selector names by number, or by its URL. */
export function pullNumber(selector: string | undefined): number | undefined {
  const hit = /^(?:https?:\/\/[^/]+\/[^/]+\/[^/]+\/pull\/)?([1-9]\d*)$/.exec(selector ?? "");
  return hit ? Number(hit[1]) : undefined;
}

/** The files and the stdin (`-`) a call hands GitHub a body through: a `gh api` call's `--input`
 * and each `-F key=@<file>` field, and every other command's `--body-file`. */
export function ghSources(args: GhArgs): string[] {
  if (args.command !== "api") return (args.flags.get("body-file") ?? []).map(String);
  return [
    ...(args.flags.get("input") ?? []).map(String),
    ...(args.flags.get("field") ?? [])
      .map((field) => String(field).split("=").slice(1).join("="))
      .filter((value) => value.startsWith("@"))
      .map((value) => value.slice(1)),
  ];
}

/** The request a gh call makes, as outcome.ts records it: a `gh api` call's method and path, or a
 * gh subcommand; and its fields. */
export interface GhRequest {
  readonly command?: string;
  readonly method?: string;
  readonly path?: string;
  readonly fields: Record<string, unknown>;
}

/** The request `args` make, each file or stdin it names read through `read`. A `gh api` call's
 * fields are its `--input` object's members, then each `-f` field, then each `-F` field (`true`,
 * `false`, `null` and an integer typed, `@<file>` read), as gh sends them, and its other flags by
 * name; it is a POST where it sends fields and names no method, as gh makes it. Any other command's
 * fields are its flags by name, the text of its body file as `body`, and the pull request its
 * selector names as `number`. Neither holds the output's flags (`--json`, `--jq`) or a header. */
export function ghRequest(args: GhArgs, read: (source: string) => string | undefined): GhRequest {
  const fields: Record<string, unknown> = {};
  if (args.command !== "api") {
    for (const [name, values] of args.flags) {
      if (["json", "jq", "header"].includes(name)) continue;
      if (name === "body-file") {
        const body = read(String(values.at(-1)));
        if (body !== undefined) fields.body = body;
      } else fields[name] = values.at(-1);
    }
    const number = pullNumber(args.positional[0]);
    if (number !== undefined) fields.number = number;
    return { command: `gh ${args.command}`, fields };
  }
  const input = given(args, "input");
  if (input !== undefined) {
    const parsed: unknown = JSON.parse(read(input) ?? "null");
    if (typeof parsed === "object" && parsed !== null && !Array.isArray(parsed))
      Object.assign(fields, parsed);
  }
  for (const name of ["raw-field", "field"]) {
    for (const field of args.flags.get(name) ?? []) {
      const [key = "", ...parts] = String(field).split("=");
      const value = parts.join("=");
      if (name === "raw-field") fields[key] = value;
      else if (value.startsWith("@")) fields[key] = read(value.slice(1));
      else if (["true", "false", "null"].includes(value)) fields[key] = JSON.parse(value);
      else if (/^[+-]?\d+$/.test(value)) fields[key] = Number(value);
      else fields[key] = value;
    }
  }
  for (const [name, values] of args.flags)
    if (!["method", "raw-field", "field", "input", "jq", "header"].includes(name))
      fields[name] = values.at(-1);
  const sends = input !== undefined || args.flags.has("raw-field") || args.flags.has("field");
  return {
    method: (given(args, "method") ?? (sends ? "POST" : "GET")).toUpperCase(),
    path: (args.positional[0] ?? "").replace(/^\//, ""),
    fields,
  };
}

/** What gh prints after the sentence that refuses the flags of a `gh api` call: its usage, as gh
 * 2.98.0 prints it (testdata/f7-paginate-slurp-jq.stderr), whose last line holds two spaces. */
const GH_API_USAGE = [
  "Usage:  gh api <endpoint> [flags]",
  "",
  "Flags:",
  "      --allow-escape-sequences   Allow printing terminal escape sequences",
  '      --cache duration           Cache the response, e.g. "3600s", "60m", "1h"',
  '  -F, --field key=value          Add a typed parameter in key=value format (use "@<path>" or "@-" to read value from file or stdin)',
  "  -H, --header key:value         Add a HTTP request header in key:value format",
  '      --hostname string          The GitHub hostname for the request (default "github.com")',
  "  -i, --include                  Include HTTP response status line and headers in the output",
  '      --input file               The file to use as body for the HTTP request (use "-" to read from standard input)',
  "  -q, --jq string                Query to select values from the response using jq syntax",
  '  -X, --method string            The HTTP method for the request (default "GET")',
  "      --paginate                 Make additional HTTP requests to fetch all pages of results",
  "  -p, --preview strings          Opt into GitHub API previews (names should omit '-preview')",
  "  -f, --raw-field key=value      Add a string parameter in key=value format",
  "      --silent                   Do not print the response body",
  '      --slurp                    Use with "--paginate" to return an array of all pages of either JSON arrays or objects',
  '  -t, --template string          Format JSON output using a Go template; see "gh help formatting"',
  "      --verbose                  Include full HTTP request and response in the output",
  "  ",
  "",
].join("\n");

/** The usage gh 2.98.0 prints after the sentence that refuses the flags of a `gh pr review` call
 * (testdata/f9-gh-requests.json). */
const GH_PR_REVIEW_USAGE = [
  "Usage:  gh pr review [<number> | <url> | <branch>] [flags]",
  "",
  "Flags:",
  "  -a, --approve           Approve pull request",
  "  -b, --body string       Specify the body of a review",
  '  -F, --body-file file    Read body text from file (use "-" to read from standard input)',
  "  -c, --comment           Comment on a pull request",
  "  -r, --request-changes   Request changes on a pull request",
  "  ",
  "",
].join("\n");

/** The `gh pr` commands that read and change nothing on GitHub. */
const PR_READS = ["pr view", "pr list", "pr checks", "pr diff", "pr status", "pr checkout"];

/** rig.sh's worker_fixture writes each route as {match, stdout?, pages?, stderr?, exit?}; the
 * stand-in adds `created`, the pull requests the run opened, each with its branch and the head it
 * last answered with. */
const Fixtures = z.looseObject({
  routes: z.array(
    z.object({
      match: z.string(),
      stdout: z.unknown().optional(),
      pages: z.array(z.unknown()).min(1).optional(),
      stderr: z.string().optional(),
      exit: z.number().optional(),
    })
  ),
  created: z.array(z.object({ number: z.number(), head: z.string(), sha: z.string() })).optional(),
});
type Route = z.infer<typeof Fixtures>["routes"][number];

/** An object a route answers with that is a pull request: one with a number and the head branch,
 * the head or the reviews of a pull request. */
type PullObject = Record<string, unknown> & { number: number };
function isPull(value: unknown): value is PullObject {
  return (
    typeof value === "object" &&
    value !== null &&
    !Array.isArray(value) &&
    typeof Reflect.get(value, "number") === "number" &&
    ("headRefName" in value || "head" in value || "reviews" in value)
  );
}

/** `value` with `change` made to each object in it, at any depth. */
function everywhere(value: unknown, change: (object: Record<string, unknown>) => unknown): unknown {
  if (Array.isArray(value)) return value.map((each) => everywhere(each, change));
  if (typeof value !== "object" || value === null) return value;
  const inner = Object.fromEntries(
    Object.entries(value).map(([key, member]) => [key, everywhere(member, change)])
  );
  return change(inner);
}

/** The real gh: the first executable `gh` on PATH outside the run's own bin directory that is no
 * script, as the daemon resolves it (daemon-pane.go realGH). */
async function realGh(runDir: string): Promise<string | undefined> {
  const own = path.resolve(runDir, "bin");
  for (const dir of (process.env.PATH ?? "").split(path.delimiter)) {
    if (dir === "" || path.resolve(dir) === own) continue;
    const candidate = path.join(dir, "gh");
    try {
      accessSync(candidate, constants.X_OK);
    } catch {
      continue;
    }
    if ((await Bun.file(candidate).slice(0, 2).text()) === "#!") continue;
    return candidate;
  }
  return undefined;
}

async function main(run: string, as: string, args: string[]): Promise<never> {
  const at = new Date().toISOString();
  if (
    as === "gh" &&
    (args.some((arg) => arg === "--help" || arg === "-h") ||
      ["help", "version", "--version"].includes(args[0] ?? ""))
  ) {
    const real = await realGh(run);
    let status = 1;
    if (real === undefined) console.error("gh-standin: no real gh on PATH answers help or version");
    else {
      const answered = Bun.spawnSync([real, ...args], {
        env: { ...process.env, GH_NO_UPDATE_NOTIFIER: "1", GH_PROMPT_DISABLED: "1" },
        stdin: "ignore",
        stdout: "pipe",
        stderr: "pipe",
      });
      process.stdout.write(answered.stdout);
      process.stderr.write(answered.stderr);
      status = answered.exitCode ?? 1;
    }
    appendFileSync(
      `${run}/gh-local.jsonl`,
      `${JSON.stringify({ at, argv: args, gh: real ?? null, exit: status })}\n`
    );
    process.exit(status);
  }

  const fixtures = Fixtures.parse(JSON.parse(readFileSync(`${run}/fixtures.json`, "utf8")));
  let routes = fixtures.routes;
  const call = readGhArgs(args);
  let stdin: string | undefined;
  const files: Record<string, string> = {};
  if (as === "gh") {
    for (const source of ghSources(call)) {
      if (source === "-") {
        stdin ??= await new Response(Bun.stdin.stream()).text();
        files[source] = stdin;
      } else files[source] = readFileSync(source, "utf8");
    }
  }
  const request = as === "gh" ? ghRequest(call, (source) => files[source]) : undefined;
  const fields = request?.fields ?? {};

  /** The run's remote, which the role pushes to (git-world.ts buildGitWorld). */
  const remote = path.join(run, "remote.git");
  /** The commit branch `ref` points at on the run's remote, if it holds the branch. */
  const remoteHead = (ref: string): string | undefined => {
    const result = Bun.spawnSync(
      ["git", "--git-dir", remote, "rev-parse", "--verify", "-q", `refs/heads/${ref}^{commit}`],
      { stdout: "pipe", stderr: "ignore" }
    );
    return result.exitCode === 0 ? result.stdout.toString().trim() : undefined;
  };
  // A pull request the run opened has its branch's head as its own, as GitHub's does: its head
  // follows each push to the branch, in every answer that holds it, a list's included.
  const created = fixtures.created ?? [];
  for (const pull of created) {
    const sha = remoteHead(pull.head);
    if (sha === undefined || sha === pull.sha) continue;
    const moved = (value: unknown): unknown => {
      if (Array.isArray(value)) return value.map(moved);
      if (typeof value !== "object" || value === null) return value;
      if (Reflect.get(value, "number") !== pull.number || !("headRefOid" in value)) return value;
      return { ...value, headRefOid: sha, head: { ref: pull.head, sha } };
    };
    routes = routes.map((candidate) => ({ ...candidate, stdout: moved(candidate.stdout) }));
    pull.sha = sha;
    writeFileSync(`${run}/fixtures.json`, JSON.stringify({ ...fixtures, routes, created }));
  }

  const joined = `${as} ${args.join(" ")}`;
  const routeAt = routes.findIndex((candidate) => new RegExp(candidate.match, "s").test(joined));
  const route: Route | undefined = routes[routeAt];
  /** The fixtures the call leaves, kept once it exits 0. */
  let next: z.infer<typeof Fixtures> | undefined;
  /** The change the call made to a pull request or a review, recorded once it exits 0. */
  let change: Record<string, unknown> | undefined;
  /** The body the call left a pull request with. */
  let kept: string | undefined;
  /** Records the call with the status it exits with and the time it ends, then exits. A call that
   * exits 0 keeps what it changed. */
  const exit = (status: number): never => {
    if (status === 0 && next !== undefined)
      writeFileSync(`${run}/fixtures.json`, JSON.stringify(next));
    const done = new Date().toISOString();
    const taken = status === 0;
    appendFileSync(
      `${run}/calls.jsonl`,
      `${JSON.stringify({ at, as, argv: args, files, stdin, route: route?.match ?? null, request, body: taken ? kept : undefined, exit: status, done, pull: taken ? change : undefined })}\n`
    );
    process.exit(status);
  };
  /** Refuses the call with gh's sentence, and its usage where gh prints one, exit status 1. */
  const refuse = (sentence: string, usage?: string): never => {
    process.stderr.write(usage === undefined ? `${sentence}\n` : `${sentence}\n\n${usage}`);
    return exit(1);
  };

  /** Every pull request the routes answer with, by number: the members of its first answer. */
  const pulls = new Map<number, PullObject>();
  for (const candidate of routes)
    everywhere([candidate.stdout, candidate.pages], (object) => {
      if (isPull(object) && !pulls.has(object.number)) pulls.set(object.number, object);
      return object;
    });
  const isOpen = (pull: PullObject) => String(pull.state ?? "open").toLowerCase() === "open";
  const branchOf = (pull: PullObject) => {
    const head = pull.head;
    if (typeof pull.headRefName === "string") return pull.headRefName;
    return typeof head === "object" && head !== null ? Reflect.get(head, "ref") : undefined;
  };
  let issueBranch: string | undefined;
  try {
    const key = JSON.parse(readFileSync(`${run}/world.json`, "utf8"))?.dispatch?.issue?.key;
    if (typeof key === "string") issueBranch = `legion/${key}`;
  } catch {
    // A run with no case world names no issue branch: gh stands on no branch.
  }
  /** The pull request a `gh pr` command acts on, as gh finds it: the one its selector names by
   * number, URL or branch, or the one open pull request of the issue's branch. */
  const target = (): number => {
    const selector = call.positional[0];
    const numbered = pullNumber(selector);
    if (numbered !== undefined) {
      if (!pulls.has(numbered))
        refuse(
          `GraphQL: Could not resolve to a PullRequest with the number of ${numbered}. (repository.pullRequest)`
        );
      return numbered;
    }
    const branch = selector ?? issueBranch;
    if (branch === undefined)
      return refuse("could not determine current branch: failed to run git: not on any branch");
    const open = [...pulls.values()].filter((pull) => branchOf(pull) === branch && isOpen(pull));
    const found = open.length === 1 ? open[0] : undefined;
    if (found === undefined) return refuse(`no pull requests found for branch "${branch}"`);
    return found.number;
  };
  /** The case clock (clock.txt), moved on by the time since the launch (clock-real.txt): when the
   * stand-in's GitHub says a review was submitted. */
  const caseNow = (): string => {
    try {
      // Date.parse reads milliseconds; clock-real.txt holds nanoseconds.
      const instant = (file: string) =>
        Date.parse(
          readFileSync(`${run}/${file}`, "utf8")
            .trim()
            .replace(/(\.\d{3})\d+/, "$1")
        );
      const clock = instant("clock.txt");
      const launched = instant("clock-real.txt");
      if (Number.isFinite(clock) && Number.isFinite(launched))
        return new Date(clock + (Date.parse(at) - launched)).toISOString();
    } catch {
      // A world with no clock files keeps the call's own time.
    }
    return at;
  };

  const pr = call.command.startsWith("pr ") && as === "gh";
  const api = call.command === "api" && as === "gh";
  const method = request?.method ?? "";
  const apiPath = request?.path ?? "";
  /** A REST path under a repository's pull requests or issues: the kind, the number, what follows. */
  const rest = /^repos\/[^/]+\/[^/]+\/(pulls|issues)(?:\/(\d+))?(\/[^?]*)?(?:\?.*)?$/.exec(apiPath);
  const restNumber = rest?.[2] === undefined ? undefined : Number(rest[2]);
  const pullPath = rest?.[1] === "pulls" && restNumber !== undefined;
  const text = (value: unknown) => (typeof value === "string" ? value : undefined);

  // What gh reads before it asks GitHub anything: a review's flags, in gh's order, then the pull
  // request a `gh pr` command that changes one acts on. Then the route answers for GitHub.
  let event: string | undefined;
  if (pr && call.command === "pr review") {
    const blank = (text(fields.body) ?? "") === "";
    let found = 0;
    if (last(call, "approve") === true) {
      found += 1;
      event = "APPROVE";
    }
    if (last(call, "request-changes") === true) {
      found += 1;
      event = "REQUEST_CHANGES";
      if (blank) refuse("body cannot be blank for request-changes review", GH_PR_REVIEW_USAGE);
    }
    if (last(call, "comment") === true) {
      found += 1;
      event = "COMMENT";
      if (blank) refuse("body cannot be blank for comment review", GH_PR_REVIEW_USAGE);
    }
    if (found === 0)
      refuse(
        "--approve, --request-changes, or --comment required when not running interactively",
        GH_PR_REVIEW_USAGE
      );
    if (found > 1)
      refuse("need exactly one of --approve, --request-changes, or --comment", GH_PR_REVIEW_USAGE);
  }
  const acted = pr && ![...PR_READS, "pr create"].includes(call.command) ? target() : undefined;
  if (route === undefined) {
    console.error(as === "gh" ? "gh: Not Found (HTTP 404)" : `unknown command: ${args.join(" ")}`);
    return exit(1);
  }

  // A body or title edit: `gh pr edit` (`-b`, `-F`, `-t`) or a PATCH of `…/pulls/<n>` naming no
  // state. It reaches that pull request alone, in each route answering with it.
  const editing =
    pr && call.command === "pr edit"
      ? {
          body: text(fields.body),
          title: text(fields.title),
          base: text(fields.base),
        }
      : api && method === "PATCH" && pullPath && rest?.[3] === undefined && !fields.state
        ? { body: text(fields.body), title: text(fields.title), base: text(fields.base) }
        : undefined;
  let answerRoute = route;
  if (editing !== undefined) {
    const number = acted ?? restNumber ?? 0;
    const edited = routes.map((candidate) => ({
      ...candidate,
      stdout: everywhere(candidate.stdout, (object) =>
        object.number === number && "body" in object
          ? {
              ...object,
              ...(editing.body === undefined ? {} : { body: editing.body }),
              ...(editing.title === undefined || !("title" in object)
                ? {}
                : { title: editing.title }),
            }
          : object
      ),
    }));
    next = { ...fixtures, routes: edited };
    answerRoute = edited[routeAt] ?? route;
    const before = pulls.get(number);
    const body = editing.body ?? text(before?.body) ?? "";
    if (editing.body !== undefined) kept = editing.body;
    change = {
      action: "edit",
      numbers: [number],
      body,
      ...(editing.title === undefined ? {} : { title: editing.title }),
      ...(editing.base === undefined ? {} : { base: editing.base }),
    };
  }

  // A review: `gh pr review [<selector>]` with one of `--approve`, `--request-changes` and
  // `--comment`, or a `gh api` POST to `…/pulls/<n>/reviews` (its `event`, `commit_id` and `body`).
  let review: Record<string, unknown> | undefined;
  const apiReview = api && method === "POST" && pullPath && rest?.[3] === "/reviews";
  if (apiReview || (pr && call.command === "pr review")) {
    const number = acted ?? restNumber ?? 0;
    const body = text(fields.body) ?? "";
    if (apiReview) {
      event = text(fields.event);
      if (!pulls.has(number)) {
        console.error("gh: Not Found (HTTP 404)");
        return exit(1);
      }
    }
    const states: Record<string, string> = {
      APPROVE: "APPROVED",
      REQUEST_CHANGES: "CHANGES_REQUESTED",
      COMMENT: "COMMENTED",
    };
    const pull = pulls.get(number);
    const reviews = Array.isArray(pull?.reviews) ? pull.reviews : [];
    const when = caseNow();
    const sha = text(fields.commit_id) ?? text(pull?.headRefOid) ?? "";
    const ids = reviews.map((each) =>
      typeof each === "object" && each !== null && "id" in each ? Number(each.id) : 0
    );
    review = {
      id: Math.max(0, ...ids.filter(Number.isFinite)) + 1,
      author: { login: "legion-review[bot]" },
      user: { login: "legion-review[bot]" },
      body,
      state: (event && states[event]) ?? "PENDING",
      commit_id: sha,
      commit: { oid: sha },
      submitted_at: when,
      submittedAt: when,
    };
    const added = review;
    const decision =
      [...reviews, added]
        .map((each) =>
          typeof each === "object" && each !== null && "state" in each ? each.state : ""
        )
        .filter((state) => state === "APPROVED" || state === "CHANGES_REQUESTED")
        .at(-1) ?? "";
    // Every answer holding this pull request (`gh pr view`'s, `gh api …/pulls/<n>`'s, a list's)
    // and every route answering with its review list.
    const reviewed = routes.map((candidate) => {
      if (Array.isArray(candidate.stdout) && candidate.match.includes(`/pulls/${number}/reviews`))
        return { ...candidate, stdout: [...candidate.stdout, added] };
      return {
        ...candidate,
        stdout: everywhere(candidate.stdout, (object) =>
          isPull(object) && object.number === number && Array.isArray(object.reviews)
            ? { ...object, reviews: [...object.reviews, added], reviewDecision: decision }
            : object
        ),
      };
    });
    next = { ...fixtures, routes: reviewed };
    change = {
      action: "review",
      number,
      state: review.state,
      commit: sha,
      body,
      submittedAt: when,
    };
  }

  // A pull request opened: `gh pr create` (`--head`, `--base`, `--title`, `--body`, `--body-file`,
  // `--draft`) or a `gh api` POST to `repos/<o>/<r>/pulls` (those fields from `-f`/`-F` or an
  // `--input` object). It is kept where the routes name the repository (the case world's
  // repository route) and the run has a remote: its own routes go first, the open lists gain it,
  // and a branch's plain `gh pr view` answers with it where no route did. The POST answers with it
  // whole.
  let opened: Record<string, unknown> | undefined;
  const apiCreate =
    api && method === "POST" && rest?.[1] === "pulls" && rest[2] === undefined && !rest[3];
  const prCreate = pr && call.command === "pr create";
  const repository = routes
    .map((candidate) => candidate.stdout)
    .find(
      (value): value is { full_name: string; default_branch: string } =>
        typeof value === "object" &&
        value !== null &&
        typeof Reflect.get(value, "full_name") === "string" &&
        typeof Reflect.get(value, "default_branch") === "string"
    );
  const head = text(fields.head)?.replace(/^[^:]*:/, "") ?? issueBranch;
  if ((apiCreate || prCreate) && repository !== undefined && existsSync(remote) && head) {
    const base = text(fields.base) ?? repository.default_branch;
    const prBody = text(fields.body) ?? "";
    const title = text(fields.title) ?? "";
    const draft = fields.draft === true || fields.draft === "true";
    /** Every pull request a route answers with, by its `gh pr view` shape. */
    const views = routes
      .map((candidate) => candidate.stdout)
      .filter(
        (
          value
        ): value is {
          number: number;
          url: string;
          headRefName: string;
          baseRefName: string;
          state: string;
        } =>
          typeof value === "object" &&
          value !== null &&
          typeof Reflect.get(value, "number") === "number" &&
          typeof Reflect.get(value, "headRefName") === "string"
      );
    const sha = remoteHead(head);
    const open = views.find(
      (view) => view.headRefName === head && view.baseRefName === base && view.state === "OPEN"
    );
    const owner = repository.full_name.split("/")[0] ?? "";
    // gh's refusal: `gh pr create`'s sentence, or the REST answer of a 422 and gh api's line.
    const refuseCreate = (sentence: string, error: Record<string, string>): never => {
      if (apiCreate) {
        process.stdout.write(
          `${JSON.stringify({ message: "Validation Failed", errors: [{ resource: "PullRequest", ...error }], documentation_url: "https://docs.github.com/rest/pulls/pulls#create-a-pull-request", status: "422" })}\n`
        );
        process.stderr.write("gh: Validation Failed (HTTP 422)\n");
      } else process.stderr.write(sentence);
      return exit(1);
    };
    if (sha === undefined)
      refuseCreate(
        `pull request create failed: GraphQL: Head sha can't be blank, Base sha can't be blank, No commits between ${base} and ${head}, Head ref must be a branch (createPullRequest)\n`,
        { field: "head", code: "invalid" }
      );
    if (open !== undefined)
      refuseCreate(
        `a pull request for branch "${head}" into branch "${base}" already exists:\n${open.url}\n`,
        {
          code: "custom",
          message: `A pull request already exists for ${owner}:${head}.`,
        }
      );
    const number = Math.max(0, ...views.map((view) => view.number)) + 1;
    const url = `https://github.com/${repository.full_name}/pull/${number}`;
    const view = {
      number,
      url,
      html_url: url,
      id: `PR_${number}`,
      title,
      body: prBody,
      state: "OPEN",
      draft,
      isDraft: draft,
      merged: false,
      head: { ref: head, sha },
      base: { ref: base },
      headRefName: head,
      headRefOid: sha,
      baseRefName: base,
      mergeCommit: null,
      merge_commit_sha: null,
      reviews: [],
      reviewDecision: "",
      statusCheckRollup: [],
      mergeable: "MERGEABLE",
    };
    opened = { ...view, state: "open" };
    // Its own routes, as case-world.ts githubFixtures writes a world pull request's.
    const own = `repos/${RegExp.escape(repository.full_name)}/pulls/${number}`;
    const selector = `(?:${number}|${RegExp.escape(head)}|${RegExp.escape(url)})`;
    const state = "(?:--state[ =]|-s )";
    const branch = `(?:--head[ =]|-H )${RegExp.escape(head)}(?: |$)`;
    const ownRoutes = [
      { match: `^gh pr view(?=.* ${selector}(?: |$))`, stdout: view },
      { match: `^gh pr checks(?=.* ${selector}(?: |$))`, stdout: "" },
      { match: `^gh api .*${own}/reviews(?: |$)`, stdout: [] },
      { match: `^gh api .*${own}/comments(?: |$)`, stdout: [] },
      { match: `^gh api .*${own}(?: |$)`, stdout: opened },
      { match: `^gh pr (edit|comment|review|ready|merge)(?=.* ${selector}(?: |$))`, stdout: url },
      {
        match: `^gh pr list(?!.* ${state}(?:closed|merged|all)(?: |$))(?=.* ${branch})`,
        stdout: [view],
      },
      { match: `^gh pr list(?=.* ${state}all(?: |$))(?=.* ${branch})`, stdout: [view] },
    ];
    // The lists of every open pull request (`gh pr list`, `--state all`, the REST list) gain it,
    // and the next creation's route answers with the next number.
    const answering = (command: string) =>
      routes.find((candidate) => new RegExp(candidate.match, "s").test(command));
    const lists = [
      answering("gh pr list"),
      answering("gh pr list --state all"),
      answering(`gh api repos/${repository.full_name}/pulls`),
    ];
    const nextUrl = `https://github.com/${repository.full_name}/pull/${number + 1}`;
    const creating = answering("gh pr create");
    const posting = answering(`gh api -X POST repos/${repository.full_name}/pulls`);
    const updated = routes.map((candidate) => {
      if (candidate === creating) return { ...candidate, stdout: nextUrl };
      if (candidate === posting)
        return { ...candidate, stdout: { html_url: nextUrl, number: number + 1 } };
      if (lists.includes(candidate) && Array.isArray(candidate.stdout))
        return { ...candidate, stdout: [...candidate.stdout, view] };
      return candidate;
    });
    const plain =
      head === issueBranch && answering("gh pr view") === undefined
        ? [
            { match: "^gh pr view( |$)", stdout: view },
            { match: "^gh pr (edit|comment|review|ready)( |$)", stdout: url },
          ]
        : [];
    next = {
      ...fixtures,
      routes: [...ownRoutes, ...updated, ...plain],
      created: [...created, { number, head, sha: sha ?? "" }],
    };
    change = { action: "open", number, head, base, sha, title, body: prBody, draft };
  }

  // Every other call that changes a pull request: the other `gh pr` commands that are no read,
  // the merge queue's and every other GraphQL mutation, and the REST writes under a pull request.
  if (change === undefined && acted !== undefined) {
    const number = acted;
    const verb = call.command.slice("pr ".length);
    const comment = text(fields.comment);
    if (verb === "merge" && last(call, "disable-auto") === true)
      change = { action: "disable-auto", number };
    else if (verb === "merge")
      change = {
        action: "merge",
        number,
        method: ["merge", "squash", "rebase"].find((name) => last(call, name) === true) ?? null,
        auto: last(call, "auto") === true,
      };
    else if (verb === "ready")
      change = { action: last(call, "undo") === true ? "draft" : "ready", number };
    else if (verb === "comment")
      change = { action: "comment", number, body: text(fields.body) ?? "" };
    else change = { action: verb, number, ...(comment === undefined ? {} : { comment }) };
  }
  const query = text(fields.query);
  if (change === undefined && api && apiPath === "graphql" && /^\s*mutation\b/.test(query ?? "")) {
    const name = /\{\s*(\w+)\s*\(/.exec(query ?? "")?.[1] ?? "";
    // The pull request's node id: the input's `pullRequestId` or `id`, a literal or a variable.
    const bound = /\b(?:pullRequestId|id)\s*:\s*(?:\$(\w+)|"([^"]*)")/.exec(query ?? "");
    const id = bound?.[2] ?? text(fields[bound?.[1] ?? ""]);
    const number = [...pulls.values()].find((pull) => id !== undefined && pull.id === id)?.number;
    const actions: Record<string, string> = {
      enqueuePullRequest: "enqueue",
      dequeuePullRequest: "dequeue",
    };
    change = {
      action: actions[name] ?? "mutation",
      ...(actions[name] === undefined ? { name } : {}),
      ...(number === undefined ? {} : { number }),
      ...(id === undefined ? {} : { id }),
    };
  }
  if (change === undefined && api && method !== "GET" && rest !== null) {
    const number = restNumber;
    const pullOf = number !== undefined && pulls.has(number);
    if (pullPath && rest[3] === undefined && method === "PATCH" && fields.state)
      change = { action: fields.state === "closed" ? "close" : "reopen", number };
    else if (pullPath && rest[3] === "/merge" && method === "PUT")
      change = { action: "merge", number, method: text(fields.merge_method) ?? null, auto: false };
    else if (pullPath && /^\/comments(?:\/\d+\/replies)?$/.test(rest[3] ?? ""))
      change = { action: "review-comment", number, body: text(fields.body) ?? "" };
    else if (rest[1] === "issues" && rest[3] === "/comments" && pullOf)
      change = { action: "comment", number, body: text(fields.body) ?? "" };
    else if (rest[1] === "pulls" && (rest[2] !== undefined || rest[3] !== undefined))
      change = {
        action: "request",
        ...(number === undefined ? {} : { number }),
        method,
        path: apiPath,
      };
  }

  const jq = given(call, "jq");

  /** Refuses the flags of the call as gh does: its sentence, then its usage, exit status 1. */
  const refuseFlags = (sentence: string): never => refuse(sentence, GH_API_USAGE);

  /** The value of the request variable `name` the call sets (`-f`, `-F`, `--field`, `--raw-field`). */
  const variable = (name: string): string | undefined =>
    [...(call.flags.get("raw-field") ?? []), ...(call.flags.get("field") ?? [])]
      .map(String)
      .find((field) => field.startsWith(`${name}=`))
      ?.slice(name.length + 1);

  /** `text` through `jq`'s filter, as gh runs it: a string printed raw, any other value compact with
   * its keys sorted, one result a line. A filter that fails ends the call, exit status 1. */
  const runJq = (input: string, expression: string): string => {
    const filtered = Bun.spawnSync(["jq", "-r", "-c", "-S", expression], {
      stdin: new TextEncoder().encode(input),
    });
    if (filtered.exitCode !== 0) {
      process.stderr.write(filtered.stderr.toString());
      exit(1);
    }
    return filtered.stdout.toString();
  };

  if (route.pages !== undefined) {
    const paginate = call.flags.has("paginate");
    const slurp = call.flags.has("slurp");
    if (slurp && !paginate) refuseFlags("`--paginate` required when passing `--slurp`");
    if (slurp && (jq !== undefined || call.flags.has("template")))
      refuseFlags("the `--slurp` option is not supported with `--jq` or `--template`");
    const wanted = paginate
      ? route.pages
      : [pageAfter(route.pages, variable("endCursor") ?? variable("after"))];
    let out: string;
    if (slurp) out = JSON.stringify(wanted);
    else if (jq !== undefined) out = wanted.map((page) => runJq(JSON.stringify(page), jq)).join("");
    else if (wanted.length > 1 && wanted.every(Array.isArray)) out = JSON.stringify(wanted.flat());
    else out = wanted.map((page) => JSON.stringify(page)).join("");
    process.stdout.write(out);
    if (route.stderr) process.stderr.write(route.stderr);
    return exit(route.exit ?? 0);
  }
  let output: unknown =
    apiReview && review !== undefined
      ? review
      : apiCreate && opened !== undefined
        ? opened
        : (answerRoute.stdout ?? "");
  const json = given(call, "json");
  if (json !== undefined) {
    const names = json.split(",");
    /** The fields `--json` names of `value`, when it is an object. */
    const pick = (value: unknown): unknown => {
      if (typeof value !== "object" || value === null || Array.isArray(value)) return value;
      const record: Record<string, unknown> = { ...value };
      return Object.fromEntries(
        names.filter((name) => Object.hasOwn(record, name)).map((name) => [name, record[name]])
      );
    };
    output = Array.isArray(output) ? output.map(pick) : pick(output);
  }
  let answer = typeof output === "string" ? output : JSON.stringify(output);
  if (jq !== undefined) answer = runJq(answer, jq);
  if (answer !== "") process.stdout.write(answer.endsWith("\n") ? answer : `${answer}\n`);
  if (route.stderr) process.stderr.write(route.stderr);
  return exit(route.exit ?? 0);
}

/** The `pageInfo.endCursor` of a page: that of the first `pageInfo` member at any depth. */
function endCursorOf(value: unknown): string | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const info = Reflect.get(value, "pageInfo");
  if (typeof info === "object" && info !== null) {
    const cursor = Reflect.get(info, "endCursor");
    return typeof cursor === "string" ? cursor : undefined;
  }
  for (const member of Object.values(value)) {
    const cursor = endCursorOf(member);
    if (cursor !== undefined) return cursor;
  }
  return undefined;
}

/** `page` with every `nodes` list empty and no next page: what GitHub answers a read that asks for
 * the page after the last. */
function emptied(page: unknown): unknown {
  if (Array.isArray(page)) return page.map(emptied);
  if (typeof page !== "object" || page === null) return page;
  return Object.fromEntries(
    Object.entries(page).map(([key, member]) => [
      key,
      key === "nodes" ? [] : key === "hasNextPage" ? false : emptied(member),
    ])
  );
}

/** The page a read that asks for the one after `cursor` is answered with: the first page where it
 * names no page's end. */
function pageAfter(pages: unknown[], cursor: string | undefined): unknown {
  const index = pages.findIndex((page) => cursor !== undefined && endCursorOf(page) === cursor);
  if (index < 0) return pages[0];
  return index + 1 < pages.length ? pages[index + 1] : emptied(pages[index]);
}

if (import.meta.main) {
  const run = process.env.SKILL_SCENARIO_RUN;
  if (!run) {
    console.error("gh-standin: SKILL_SCENARIO_RUN is unset");
    process.exit(2);
  }
  const [as = "gh", ...args] = Bun.argv.slice(2);
  await main(run, as, args);
}
