#!/usr/bin/env bun
// Stand-in for `gh` on a skill scenario's PATH. Each call is recorded to
// $SKILL_SCENARIO_RUN/calls.jsonl (argv, the contents of any file or stdin it hands GitHub a body
// through, its exit status, and the pull request body it kept, if any), then answered from the
// first route in $SKILL_SCENARIO_RUN/fixtures.json whose regex matches "gh <argv...>". A call no
// route matches answers `gh: Not Found (HTTP 404)` and exits 1. Nothing is sent anywhere.
// `--json a,b` picks those fields of the answer, or of each object of a list answer (`gh pr list`),
// and `--jq`/`-q` runs jq, as gh does. A pull request body edit (`gh pr edit --body`,
// `--body-file`, or a PATCH through `gh api`) is kept: every route answering with an object that
// has a `body`, or with a list that holds such objects, then answers with the new one in each such
// object, as GitHub's next read would, and the call's record carries it as `body`. Only a pull
// request (an object with a `number`) takes the new body: its reviews' bodies are their own.
// A review submission (`gh api` POST to `…/pulls/<n>/reviews`, or `gh pr review`) is kept too:
// the POST answers with the review it created, and every route answering with that pull request
// or with its review list then holds the review, the pull request's `reviewDecision` following.
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
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
import { z } from "zod";

/** rig.sh's worker_fixture writes each route as {match, stdout?, pages?, stderr?, exit?}. */
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
});

const run = process.env.SKILL_SCENARIO_RUN;
if (!run) {
  console.error("gh-standin: SKILL_SCENARIO_RUN is unset");
  process.exit(2);
}
const [as = "gh", ...args] = Bun.argv.slice(2);
const at = new Date().toISOString();
const fixtures = Fixtures.parse(JSON.parse(readFileSync(`${run}/fixtures.json`, "utf8")));
const routes = fixtures.routes;

let stdin: string | undefined;
const readStdin = async () => {
  stdin ??= await new Response(Bun.stdin.stream()).text();
  return stdin;
};
const files: Record<string, string> = {};
const readSource = async (source: string) =>
  source === "-" ? readStdin() : readFileSync(source, "utf8");
// A pull request body edit: `gh pr edit` takes it from `-b/--body` or `-F/--body-file`, a
// `gh api -X PATCH repos/<o>/<r>/pulls/<n>` from a `body` field or an `--input` JSON object.
const prEdit = as === "gh" && args[0] === "pr" && args[1] === "edit";
const prPatch =
  as === "gh" &&
  args[0] === "api" &&
  /(^|\s)(-X|--method)\s?PATCH(\s|$)/.test(args.join(" ")) &&
  args.some((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+$/.test(arg));
let body: string | undefined;
/** The body this call left the pull request with, once the routes serve it. */
let kept: string | undefined;
/** The `-f`/`-F` fields of a `gh api` call, each file field read. */
const apiFields: Record<string, string> = {};
for (let index = 0; index < args.length; index += 1) {
  const arg = args[index] ?? "";
  const next = args[index + 1];
  if (!next) continue;
  if (prEdit && (arg === "-b" || arg === "--body")) body = next;
  else if (prEdit && (arg === "-F" || arg === "--body-file")) {
    files[next] = await readSource(next);
    body = files[next];
  } else if (arg === "--input" || arg === "--body-file") files[next] = await readSource(next);
  else if (args[0] !== "api" && arg === "-F") files[next] = await readSource(next);
  else if (arg === "-F" || arg === "--field" || arg === "-f" || arg === "--raw-field") {
    const [key = "", ...rest] = next.split("=");
    let value = rest.join("=");
    if ((arg === "-F" || arg === "--field") && value.startsWith("@")) {
      files[value.slice(1)] = await readSource(value.slice(1));
      value = files[value.slice(1)] ?? "";
    }
    apiFields[key] = value;
    if (prPatch && key === "body") body = value;
  }
}
const inputIndex = args.indexOf("--input");
const input = inputIndex >= 0 ? args[inputIndex + 1] : undefined;
if (prPatch && input !== undefined) {
  const parsed: unknown = JSON.parse(files[input] ?? "null");
  if (
    typeof parsed === "object" &&
    parsed !== null &&
    "body" in parsed &&
    typeof parsed.body === "string"
  ) {
    body = parsed.body;
  }
}
const joined = `${as} ${args.join(" ")}`;
const route = routes.find((candidate) => new RegExp(candidate.match, "s").test(joined));
/** Records the call with the status it exits with, then exits. */
function exit(status: number): never {
  appendFileSync(
    `${run}/calls.jsonl`,
    `${JSON.stringify({ at, as, argv: args, files, stdin, route: route?.match ?? null, body: kept, exit: status })}\n`
  );
  process.exit(status);
}
if (!route) {
  console.error(as === "gh" ? "gh: Not Found (HTTP 404)" : `unknown command: ${args.join(" ")}`);
  exit(1);
}
if (body !== undefined) {
  /** `value` with the new body, when it is a pull request (an object with a `number` and a body),
   * never one of its reviews or comments, whose bodies are their own. */
  const withBody = (value: unknown): unknown =>
    typeof value === "object" &&
    value !== null &&
    !Array.isArray(value) &&
    "body" in value &&
    "number" in value
      ? { ...value, body }
      : value;
  const edited = routes.map((candidate) => ({
    ...candidate,
    stdout: Array.isArray(candidate.stdout)
      ? candidate.stdout.map(withBody)
      : withBody(candidate.stdout),
  }));
  writeFileSync(`${run}/fixtures.json`, JSON.stringify({ ...fixtures, routes: edited }));
  kept = body;
}

/** The method a `gh api` call names (`-X`, `--method`, `-XPOST`, `--method=POST`), uppercase. */
function apiMethod(): string | undefined {
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index] ?? "";
    const hit = /^(?:-X|--method=?)(.*)$/.exec(arg);
    if (hit) return (hit[1] || args[index + 1] || "").toUpperCase();
  }
  return undefined;
}
/** The pull request object a route answers with: the case world's `gh pr view` answer. */
type Pull = {
  number: number;
  url?: string;
  headRefName?: string;
  headRefOid?: string;
  reviews: unknown[];
};
const pulls = routes
  .map((candidate) => candidate.stdout)
  .filter(
    (value): value is Pull =>
      typeof value === "object" &&
      value !== null &&
      !Array.isArray(value) &&
      "number" in value &&
      "reviews" in value &&
      Array.isArray(value.reviews)
  );
// A review submission: `gh api` POST to `repos/<o>/<r>/pulls/<n>/reviews` (its `event`,
// `commit_id` and `body` from fields or an `--input` object) or `gh pr review [<n>]` with
// `--approve`, `--request-changes` or `--comment`. GitHub answers the POST with the review it
// created, and its next read of the pull request holds it: every route answering with that pull
// request or with its review list then holds the new review too, its review decision follows, and
// the POST answers with the review. The review is submitted at the case clock (clock.txt, moved
// on by the time since the launch, clock-real.txt).
let review: Record<string, unknown> | undefined;
const reviewsPath = args.find((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+\/reviews$/.test(arg));
const apiReview =
  as === "gh" &&
  args[0] === "api" &&
  reviewsPath !== undefined &&
  (apiMethod() ?? (Object.keys(apiFields).length > 0 || input !== undefined ? "POST" : "GET")) ===
    "POST";
const prReview = as === "gh" && args[0] === "pr" && args[1] === "review";
if (apiReview || prReview) {
  let number: number | undefined;
  let event: string | undefined;
  let commit: string | undefined;
  let text = "";
  if (apiReview) {
    number = Number(/pulls\/(\d+)\/reviews$/.exec(reviewsPath ?? "")?.[1]);
    const given: Record<string, unknown> =
      input === undefined ? apiFields : { ...JSON.parse(files[input] ?? "{}"), ...apiFields };
    event = typeof given.event === "string" ? given.event : undefined;
    commit = typeof given.commit_id === "string" ? given.commit_id : undefined;
    text = typeof given.body === "string" ? given.body : "";
  } else {
    const selector = args[2] !== undefined && !args[2].startsWith("-") ? args[2] : undefined;
    number = (
      selector === undefined && pulls.length === 1
        ? pulls[0]
        : pulls.find((pull) => [String(pull.number), pull.url, pull.headRefName].includes(selector))
    )?.number;
    if (args.some((arg) => arg === "-a" || arg === "--approve")) event = "APPROVE";
    else if (args.some((arg) => arg === "-r" || arg === "--request-changes"))
      event = "REQUEST_CHANGES";
    else if (args.some((arg) => arg === "-c" || arg === "--comment")) event = "COMMENT";
    for (let index = 2; index < args.length; index += 1) {
      const flag = args[index];
      const value = args[index + 1];
      if (value === undefined) continue;
      if (flag === "-b" || flag === "--body") text = value;
      if (flag === "-F" || flag === "--body-file") text = files[value] ?? "";
    }
  }
  const pull = pulls.find((candidate) => candidate.number === number);
  if (pull !== undefined) {
    const states: Record<string, string> = {
      APPROVE: "APPROVED",
      REQUEST_CHANGES: "CHANGES_REQUESTED",
      COMMENT: "COMMENTED",
    };
    let when = at;
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
        when = new Date(clock + (Date.parse(at) - launched)).toISOString();
    } catch {
      // A world with no clock files keeps the call's own time.
    }
    const sha = commit ?? pull.headRefOid ?? "";
    const ids = pull.reviews.map((each) =>
      typeof each === "object" && each !== null && "id" in each ? Number(each.id) : 0
    );
    review = {
      id: Math.max(0, ...ids.filter(Number.isFinite)) + 1,
      author: { login: "legion-review[bot]" },
      user: { login: "legion-review[bot]" },
      body: text,
      state: (event && states[event]) ?? "PENDING",
      commit_id: sha,
      commit: { oid: sha },
      submitted_at: when,
      submittedAt: when,
    };
    const decision =
      [...pull.reviews, review]
        .map((each) =>
          typeof each === "object" && each !== null && "state" in each ? each.state : ""
        )
        .filter((state) => state === "APPROVED" || state === "CHANGES_REQUESTED")
        .at(-1) ?? "";
    // Every route answering with this pull request (`gh pr view`'s and `gh api …/pulls/<n>`'s) and
    // every route answering with its review list.
    const edited = routes.map((candidate) => {
      const value = candidate.stdout;
      if (pulls.includes(value as Pull) && (value as Pull).number === number)
        return {
          ...candidate,
          stdout: {
            ...(value as Pull),
            reviews: [...(value as Pull).reviews, review],
            reviewDecision: decision,
          },
        };
      if (Array.isArray(value) && candidate.match.includes(`/pulls/${number}/reviews`))
        return { ...candidate, stdout: [...value, review] };
      return candidate;
    });
    writeFileSync(`${run}/fixtures.json`, JSON.stringify({ ...fixtures, routes: edited }));
  }
}
const jqIndex = args.findIndex((arg) => arg === "--jq" || arg === "-q");
const filter = jqIndex >= 0 ? args[jqIndex + 1] : undefined;

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

/** Refuses the flags of the call as gh does: its sentence, then its usage, exit status 1. */
function refuseFlags(sentence: string): never {
  process.stderr.write(`${sentence}\n\n${GH_API_USAGE}`);
  return exit(1);
}

/** The value of the request variable `name` the call sets (`-f`, `-F`, `--field`, `--raw-field`). */
function variable(name: string): string | undefined {
  const set = args.findIndex(
    (arg, index) =>
      ["-f", "-F", "--field", "--raw-field"].includes(arg) &&
      (args[index + 1] ?? "").startsWith(`${name}=`)
  );
  return set < 0 ? undefined : (args[set + 1] ?? "").slice(name.length + 1);
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

/** `text` through `jq`'s filter, as gh runs it: a string printed raw, any other value compact with
 * its keys sorted, one result a line. A filter that fails ends the call, exit status 1. */
function runJq(text: string, expression: string): string {
  const jq = Bun.spawnSync(["jq", "-r", "-c", "-S", expression], {
    stdin: new TextEncoder().encode(text),
  });
  if (jq.exitCode !== 0) {
    process.stderr.write(jq.stderr.toString());
    exit(1);
  }
  return jq.stdout.toString();
}

if (route.pages !== undefined) {
  const paginate = args.includes("--paginate");
  const slurp = args.includes("--slurp");
  if (slurp && !paginate) refuseFlags("`--paginate` required when passing `--slurp`");
  if (slurp && (filter !== undefined || args.includes("--template") || args.includes("-t"))) {
    refuseFlags("the `--slurp` option is not supported with `--jq` or `--template`");
  }
  const wanted = paginate
    ? route.pages
    : [pageAfter(route.pages, variable("endCursor") ?? variable("after"))];
  let out: string;
  if (slurp) out = JSON.stringify(wanted);
  else if (filter !== undefined)
    out = wanted.map((page) => runJq(JSON.stringify(page), filter)).join("");
  else if (wanted.length > 1 && wanted.every(Array.isArray)) out = JSON.stringify(wanted.flat());
  else out = wanted.map((page) => JSON.stringify(page)).join("");
  process.stdout.write(out);
  if (route.stderr) process.stderr.write(route.stderr);
  exit(route.exit ?? 0);
}
let output: unknown = apiReview && review !== undefined ? review : (route.stdout ?? "");
const jsonIndex = args.indexOf("--json");
if (jsonIndex >= 0) {
  const fields = (args[jsonIndex + 1] ?? "").split(",");
  /** The fields `--json` names of `value`, when it is an object. */
  const pick = (value: unknown): unknown => {
    if (typeof value !== "object" || value === null || Array.isArray(value)) return value;
    const record: Record<string, unknown> = { ...value };
    return Object.fromEntries(
      fields.filter((field) => Object.hasOwn(record, field)).map((field) => [field, record[field]])
    );
  };
  output = Array.isArray(output) ? output.map(pick) : pick(output);
}
let text = typeof output === "string" ? output : JSON.stringify(output);
if (filter !== undefined) text = runJq(text, filter);
if (text !== "") process.stdout.write(text.endsWith("\n") ? text : `${text}\n`);
if (route.stderr) process.stderr.write(route.stderr);
exit(route.exit ?? 0);
