#!/usr/bin/env bun
// Stand-in for `gh` on a skill scenario's PATH. Each call is recorded to
// $SKILL_SCENARIO_RUN/calls.jsonl (argv, the contents of any file or stdin it hands GitHub a body
// through, its exit status, and the pull request body it kept, if any), then answered from the
// first route in $SKILL_SCENARIO_RUN/fixtures.json whose regex matches "gh <argv...>". Nothing is
// sent anywhere.
//
// A call no route matches is the rig's fault, never GitHub's answer: it prints
// `STAND-IN HAS NO ROUTE: gh <argv...>` to stderr, appends that line to
// $SKILL_SCENARIO_RUN/no-route.log, records the call with `route: null` and `no_route: true`, and
// exits 97, which no gh status means. rig.sh prints the log's lines after the run's own, and
// score.ts counts such a run as a rig error, so a route the world lacks can never read as GitHub's
// answer and pass or fail a run. A world answers each refusal GitHub gives (a 404, a 403) with a
// route of its own, as GitHub words it.
//
// As gh does: `--json a,b` picks fields of an object or of each object of a list, `--jq`/`-q` runs
// jq, `--slurp` wraps a `--paginate` answer in a list, and `-i`/`--include` prints the status line
// and headers first. A route with an HTTP `status` of 400 or more is GitHub's refusal: gh prints its
// body as GitHub sent it, unfiltered and with no newline, then the route's `stderr`, and exits with
// the route's `exit` (1). A route with an `exit` and no such status is a porcelain command's own
// answer, such as `gh pr checks` exiting 8 while a check runs: its output is filtered as any other.
//
// Writes a later read sees, as GitHub's next read would:
//   - a pull request body edit (`gh pr edit --body`, `--body-file`, or a PATCH through `gh api`):
//     every route answering with an object that has a `body`, and that is that pull request's or
//     names none, then answers with the new one; the call's record carries it as `body`;
//   - a review (a route with `keep: "review"`: a POST to `pulls/<n>/reviews`, `gh pr review`) and
//     an issue comment (`keep: "comment"`: a POST to `issues/<n>/comments`, `gh pr comment`): the
//     write is answered with the object GitHub returns, and appended to each route that lists that
//     pull request's reviews or comments (`list` and `pull`) and to the `reviews` or `comments` of
//     each `gh pr view` object of that pull request. A route with `lookup: "review"` reads one review
//     by its id from the pull request's list, and answers GitHub's 404 for an id it does not hold;
//   - a press that moves the world on (a route with `moves`, such as the re-run of a failed job):
//     the world's `state` becomes the route's `moves`, and from then on a route with `when`
//     answers only while the state is its `when` ("" before any press), as GitHub answers every
//     read of the run after a re-run with the attempt it started. A route with no `when` answers
//     in every state. The rig moves the state on between two turns of a run the same way, so a
//     later turn reads the attempt as it settled.
//   gh-standin.ts gh <args...>
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
import { z } from "zod";

/** rig.sh (and heads-routes.ts, for a heads scenario) writes each route as {match, stdout?,
 * stderr?, exit?, status?, keep?, list?, lookup?, pull?, when?, moves?}. `status` is the HTTP
 * status `-i` prints (200 for a route that exits 0). */
const Route = z.looseObject({
  match: z.string(),
  stdout: z.unknown().optional(),
  stderr: z.string().optional(),
  exit: z.number().optional(),
  status: z.number().optional(),
  keep: z.enum(["review", "comment"]).optional(),
  list: z.enum(["reviews", "comments"]).optional(),
  lookup: z.literal("review").optional(),
  pull: z.number().optional(),
  when: z.string().optional(),
  moves: z.string().optional(),
});
/** `current` is the pull request a call that names none is about (the workspace's branch); `login`
 * the account the run's token is; `kept` how many reviews and comments the run has written;
 * `state` the world's state, which a route's `when` names ("" when it names none). */
const Fixtures = z.looseObject({
  routes: z.array(Route),
  current: z.number().optional(),
  login: z.string().optional(),
  repo: z.string().optional(),
  kept: z.object({ reviews: z.number(), comments: z.number() }).optional(),
  state: z.string().optional(),
});

/** The status every route of a world can give gh's `-i`. */
const REASONS: Record<number, string> = {
  200: "OK",
  201: "Created",
  204: "No Content",
  400: "Bad Request",
  403: "Forbidden",
  404: "Not Found",
  422: "Unprocessable Entity",
};
/** The exit status of a call no route matches: no status gh gives means it. */
export const NO_ROUTE_EXIT = 97;
/** The line a call no route matches prints and logs, before the call. */
export const NO_ROUTE = "STAND-IN HAS NO ROUTE:";

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
  /(^|\s)(-X|--method)[\s=]?PATCH(\s|$)/.test(args.join(" ")) &&
  args.some((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+$/.test(arg));
let body: string | undefined;
/** The body this call left the pull request with, once the routes serve it. */
let kept: string | undefined;
/** The `-f`/`-F` fields the call sends, a file or stdin read in. */
const fields: Record<string, string> = {};
for (let index = 0; index < args.length; index += 1) {
  const arg = args[index] ?? "";
  const next = args[index + 1];
  if (!next) continue;
  if (prEdit && (arg === "-b" || arg === "--body")) body = next;
  else if (prEdit && (arg === "-F" || arg === "--body-file")) {
    files[next] = await readSource(next);
    body = files[next];
  } else if (arg === "--input" || arg === "--body-file") files[next] = await readSource(next);
  else if (arg === "-F" || arg === "--field" || arg === "-f" || arg === "--raw-field") {
    const [key = "", ...rest] = next.split("=");
    let value = rest.join("=");
    if ((arg === "-F" || arg === "--field") && value.startsWith("@")) {
      files[value.slice(1)] = await readSource(value.slice(1));
      value = files[value.slice(1)] ?? "";
    }
    fields[key] = value;
    if (prPatch && key === "body") body = value;
  }
}
const inputIndex = args.indexOf("--input");
const input = inputIndex >= 0 ? args[inputIndex + 1] : undefined;
/** The `--input` JSON object, or undefined when there is none or it is no object. */
let sent: Record<string, unknown> | undefined;
if (input !== undefined) {
  try {
    const parsed = z.record(z.string(), z.unknown()).safeParse(JSON.parse(files[input] ?? "null"));
    sent = parsed.success ? parsed.data : undefined;
  } catch {
    sent = undefined;
  }
}
if (prPatch && typeof sent?.body === "string") body = sent.body;
const joined = `${as} ${args.join(" ")}`;
/** The world's state as the call finds it: "" until a press moved it. */
const state = fixtures.state ?? "";
const route = routes.find(
  (candidate) =>
    (candidate.when === undefined || candidate.when === state) &&
    new RegExp(candidate.match, "s").test(joined)
);
/** Records the call with the status it exits with and the world's state it found, then exits. */
function exit(status: number, extra: Record<string, unknown> = {}): never {
  appendFileSync(
    `${run}/calls.jsonl`,
    `${JSON.stringify({ at, as, argv: args, files, stdin, route: route?.match ?? null, body: kept, exit: status, state, ...extra })}\n`
  );
  process.exit(status);
}
// `gh api --input <file>` sends the file's bytes as the request's JSON body, and GitHub refuses a
// body that is not JSON before any route acts on it: gh prints its answer and exits 1. Answering
// such a call as the route does told an agent its review had posted (in the third round, one wrote
// its review's JSON with an unescaped quote in it).
const inputBody = as === "gh" && args[0] === "api" && input ? files[input] : undefined;
if (inputBody !== undefined) {
  try {
    JSON.parse(inputBody);
  } catch {
    console.error("gh: Problems parsing JSON (HTTP 400)");
    exit(1);
  }
}
if (!route) {
  const line = `${NO_ROUTE} ${joined}`;
  console.error(line);
  appendFileSync(`${run}/no-route.log`, `${line}\n`);
  exit(NO_ROUTE_EXIT, { no_route: true });
}

/** The pull request number a call names: `pulls/<n>` or `issues/<n>` in a path, a `gh pr`
 * argument, or a pull request URL; else the world's current one. */
function pullOf(): number | undefined {
  for (const arg of args) {
    const inPath = /(?:^|\/)(?:pulls|issues)\/(\d+)(?:[/?]|$)/.exec(arg);
    if (inPath) return Number(inPath[1]);
  }
  if (args[0] === "pr") {
    const target = args.slice(2).find((arg) => /^(\d+|#\d+|https:\/\/\S+\/pull\/\d+)$/.test(arg));
    if (target !== undefined) return Number(/(\d+)$/.exec(target)?.[1]);
  }
  return fixtures.current;
}
const pull = route.pull ?? pullOf();

/** Each object a route answers with, its own or each member of a list. */
function* objects(): Generator<object> {
  for (const candidate of routes) {
    const out = candidate.stdout;
    for (const item of Array.isArray(out) ? out : [out])
      if (typeof item === "object" && item !== null && !Array.isArray(item)) yield item;
  }
}
/** A field of an object a route answers with, or undefined. */
const fieldOf = (object: object, key: string): unknown =>
  Object.getOwnPropertyDescriptor(object, key)?.value;
/** A string field of the pull request's own object (its `url`, its `headRefOid`), or undefined. */
function pullField(number: number | undefined, key: "url" | "headRefOid"): string | undefined {
  for (const object of objects()) {
    const value = fieldOf(object, key);
    if (
      fieldOf(object, "number") === number &&
      typeof value === "string" &&
      (key !== "url" || value.includes("/pull/"))
    )
      return value;
  }
  return undefined;
}
/** The lists `key` of each `gh pr view` object of the pull request. */
function prViews(
  number: number | undefined,
  key: "reviews" | "latestReviews" | "comments"
): unknown[][] {
  const lists: unknown[][] = [];
  for (const object of objects()) {
    const list = fieldOf(object, key);
    if (fieldOf(object, "number") === number && Array.isArray(list)) lists.push(list);
  }
  return lists;
}
/** Each route that lists the pull request's `list`: the list its answer is. */
function listsOf(list: "reviews" | "comments", number: number | undefined): unknown[][] {
  return routes.flatMap((candidate) =>
    candidate.list === list && candidate.pull === number && Array.isArray(candidate.stdout)
      ? [candidate.stdout]
      : []
  );
}
/** The body a `gh pr review` or `gh pr comment` sends: `-b`/`--body`, or `-F`/`--body-file`. */
function prBody(): string {
  const at = args.findIndex((arg) => ["-b", "--body", "-F", "--body-file"].includes(arg));
  const value = at < 0 ? "" : (args[at + 1] ?? "");
  return args[at] === "-F" || args[at] === "--body-file"
    ? (files[value] ?? readFileSync(value, "utf8"))
    : value;
}
/** A field the call sends, from its `--input` object or its `-f`/`-F` fields. */
function sentField(key: string): string | undefined {
  const value = sent?.[key];
  return typeof value === "string" ? value : fields[key];
}
const counts = fixtures.kept ?? { reviews: 0, comments: 0 };
const login = fixtures.login ?? "rig[bot]";
const url =
  pullField(pull, "url") ?? `https://github.com/${fixtures.repo ?? "example/widgets"}/pull/${pull}`;

let output: unknown = route.stdout ?? "";
if (body !== undefined) {
  const edited = pullOf();
  for (const object of objects())
    if (
      "body" in object &&
      (edited === undefined || !("number" in object) || object.number === edited)
    )
      object.body = body;
  kept = body;
}
if (route.keep === "review") {
  // GitHub's review: an event of APPROVE, REQUEST_CHANGES or COMMENT, or none (a pending review).
  const event =
    args[0] !== "pr"
      ? (sentField("event") ?? "").toUpperCase()
      : args.some((arg) => arg === "--approve" || arg === "-a")
        ? "APPROVE"
        : args.some((arg) => arg === "--request-changes" || arg === "-r")
          ? "REQUEST_CHANGES"
          : "COMMENT";
  const text = args[0] === "pr" ? prBody() : (sentField("body") ?? "");
  const states: Record<string, string> = {
    APPROVE: "APPROVED",
    REQUEST_CHANGES: "CHANGES_REQUESTED",
    COMMENT: "COMMENTED",
  };
  const state = states[event] ?? "PENDING";
  counts.reviews += 1;
  const id = 7001 + counts.reviews;
  const commit =
    (args[0] === "pr" ? undefined : sentField("commit_id")) ||
    (pullField(pull, "headRefOid") ?? "");
  const made = {
    id,
    node_id: `PRR_rig${id}`,
    user: { login, type: "Bot" },
    body: text,
    state,
    commit_id: commit,
    ...(state === "PENDING" ? {} : { submitted_at: at }),
    html_url: `${url}#pullrequestreview-${id}`,
    pull_request_url: url
      .replace("https://github.com/", "https://api.github.com/repos/")
      .replace("/pull/", "/pulls/"),
    author_association: "NONE",
  };
  for (const list of listsOf("reviews", pull)) list.push(made);
  for (const key of ["reviews", "latestReviews"] as const)
    for (const list of prViews(pull, key))
      list.push({ author: { login }, state, body: text, submittedAt: at, commit: { oid: commit } });
  output = args[0] === "pr" ? "" : made;
}
if (route.keep === "comment") {
  const text = args[0] === "pr" ? prBody() : (sentField("body") ?? "");
  counts.comments += 1;
  const id = 6000 + counts.comments;
  const made = {
    id,
    node_id: `IC_rig${id}`,
    user: { login, type: "Bot" },
    body: text,
    created_at: at,
    updated_at: at,
    html_url: `${url}#issuecomment-${id}`,
    author_association: "NONE",
  };
  for (const list of listsOf("comments", pull)) list.push(made);
  for (const list of prViews(pull, "comments"))
    list.push({ author: { login }, body: text, createdAt: at, url: made.html_url });
  output = args[0] === "pr" ? made.html_url : made;
}
if (body !== undefined || route.keep !== undefined || route.moves !== undefined)
  writeFileSync(
    `${run}/fixtures.json`,
    JSON.stringify({ ...fixtures, routes, kept: counts, state: route.moves ?? state })
  );

/** Prints the status line and headers `gh api -i` prints, then nothing more. */
function include(status: number) {
  if (as === "gh" && args[0] === "api" && args.some((arg) => arg === "-i" || arg === "--include"))
    process.stdout.write(
      `HTTP/2.0 ${status} ${REASONS[status] ?? ""}\r\nContent-Type: application/json; charset=utf-8\r\nX-Github-Api-Version-Selected: 2022-11-28\r\n\r\n`
    );
}
/** gh's answer to an HTTP error: the body as GitHub sent it, unfiltered, then the message, and
 * the route's exit status (gh exits 1 on an HTTP error). */
function refuse(status: number, answer: unknown, message: string, code: number): never {
  include(status);
  if (answer !== undefined && answer !== "")
    process.stdout.write(typeof answer === "string" ? answer : JSON.stringify(answer));
  process.stderr.write(message);
  exit(code);
}
if (route.lookup === "review") {
  const id = Number(/\/reviews\/(\d+)/.exec(args.join(" "))?.[1]);
  const found = listsOf("reviews", pull)
    .flat()
    .find(
      (review) =>
        typeof review === "object" && review !== null && "id" in review && review.id === id
    );
  if (found === undefined)
    refuse(
      404,
      {
        message: "Not Found",
        documentation_url:
          "https://docs.github.com/rest/pulls/reviews#get-a-review-for-a-pull-request",
        status: "404",
      },
      "gh: Not Found (HTTP 404)\n",
      1
    );
  output = found;
}
// A route with an HTTP status of 400 or more is GitHub refusing the call. A route with an exit
// status alone is a porcelain command's own answer (`gh pr checks` exits 8 while a check runs, and
// prints its table all the same), so its output goes through `--json`, `--jq` and `-i` below.
if ((route.status ?? 0) >= 400)
  refuse(route.status ?? 404, route.stdout, route.stderr ?? "", route.exit ?? 1);

// gh's `--json` picks the named fields of the answer, or of each object of a list: gh's own answer
// is the whole object, which the routes hold.
const jsonIndex = args.indexOf("--json");
if (jsonIndex >= 0 && typeof output === "object" && output !== null) {
  const wanted = (args[jsonIndex + 1] ?? "").split(",");
  const pick = (item: unknown) =>
    typeof item === "object" && item !== null && !Array.isArray(item)
      ? Object.fromEntries(Object.entries(item).filter(([field]) => wanted.includes(field)))
      : item;
  output = Array.isArray(output) ? output.map(pick) : pick(output);
}
let text = typeof output === "string" ? output : JSON.stringify(output);
if (args.includes("--slurp") && typeof output !== "string") text = `[${text}]`;
const jqIndex = args.findIndex((arg) => arg === "--jq" || arg === "-q");
const filter = jqIndex >= 0 ? args[jqIndex + 1] : undefined;
if (filter !== undefined) {
  const jq = Bun.spawnSync(["jq", "-r", filter], { stdin: new TextEncoder().encode(text) });
  text = jq.stdout.toString();
  if (jq.exitCode !== 0) {
    process.stderr.write(jq.stderr.toString());
    exit(1);
  }
}
include(route.status ?? 200);
if (text !== "") process.stdout.write(text.endsWith("\n") ? text : `${text}\n`);
if (route.stderr) process.stderr.write(route.stderr);
exit(route.exit ?? 0);
