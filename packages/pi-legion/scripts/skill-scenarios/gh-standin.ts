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
// object, as GitHub's next read would, and the call's record carries it as `body`.
// A reply on a review thread, GraphQL's `addPullRequestReviewThreadReply`, REST's
// `repos/<o>/<r>/pulls/<n>/comments/<id>/replies`, or a new review comment whose `in_reply_to`
// names the thread's first comment, is recorded as `reply: {thread, body}`: the thread's node id,
// or for REST its first comment's id. A `resolveReviewThread` is recorded as
// `resolved: <thread node id>`. A route's answer that holds `{{HEAD}}` gets the head of
// $SKILL_SCENARIO_RUN/ready.json in its place, the head the label's test built (rig.sh
// run_forward); with no such head the call fails, naming the file.
//   gh-standin.ts gh <args...>
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
import { z } from "zod";

/** rig.sh's worker_fixture writes each route as {match, stdout?, stderr?, exit?}. */
const Fixtures = z.looseObject({
  routes: z.array(
    z.object({
      match: z.string(),
      stdout: z.unknown().optional(),
      stderr: z.string().optional(),
      exit: z.number().optional(),
    })
  ),
});
/** The part of the label's ready.json that a `{{HEAD}}` route reads: the head the test built. */
const Ready = z.looseObject({ head: z.string().regex(/^[0-9a-f]{40}$/) });

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
/** Every `-f`/`-F` field of the call, a file's or stdin's content in place of `@<file>`. */
const given: Record<string, string> = {};
/** The body this call left the pull request with, once the routes serve it. */
let kept: string | undefined;
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
    if (prPatch && key === "body") body = value;
    given[key] = value;
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
// A review thread's reply and resolution, as GraphQL names their fields: a literal string, or a
// `$variable` that a field of the call gives.
const graphql = as === "gh" && args[0] === "api" && args[1] === "graphql";
const query = given.query ?? "";
const named = (field: string): string | undefined => {
  const hit = new RegExp(`\\b${field}\\s*:\\s*(?:"((?:[^"\\\\]|\\\\.)*)"|\\$(\\w+))`).exec(query);
  if (hit === null) return undefined;
  return hit[1] !== undefined ? String(JSON.parse(`"${hit[1]}"`)) : given[hit[2] ?? ""];
};
let reply: { thread: string; body: string } | undefined;
let resolved: string | undefined;
if (graphql && /\baddPullRequestReviewThreadReply\b/.test(query))
  reply = { thread: named("pullRequestReviewThreadId") ?? "", body: named("body") ?? "" };
if (graphql && /\bresolveReviewThread\b/.test(query)) resolved = named("threadId") ?? "";
const replies = args
  .map((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+\/comments\/(\d+)\/replies$/.exec(arg)?.[1])
  .find((id) => id !== undefined);
if (as === "gh" && args[0] === "api" && replies !== undefined)
  reply = { thread: replies, body: given.body ?? "" };
// GitHub also takes a thread's reply as a new review comment whose `in_reply_to` names the thread's
// first comment, given as a field or in the --input body.
const newComment = args.some((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+\/comments$/.test(arg));
if (as === "gh" && args[0] === "api" && newComment) {
  const parsed: unknown = input === undefined ? given : JSON.parse(files[input] ?? "null");
  const sent = typeof parsed === "object" && parsed !== null ? (parsed as Record<string, unknown>) : {};
  const thread = sent.in_reply_to === undefined ? "" : String(sent.in_reply_to);
  if (thread !== "") reply = { thread, body: sent.body === undefined ? "" : String(sent.body) };
}
/** Records the call with the status it exits with, then exits. */
function exit(status: number): never {
  const record = {
    at,
    as,
    argv: args,
    files,
    stdin,
    route: route?.match ?? null,
    body: kept,
    ...(reply === undefined ? {} : { reply }),
    ...(resolved === undefined ? {} : { resolved }),
    exit: status,
  };
  appendFileSync(`${run}/calls.jsonl`, `${JSON.stringify(record)}\n`);
  process.exit(status);
}
if (!route) {
  console.error(as === "gh" ? "gh: Not Found (HTTP 404)" : `unknown command: ${args.join(" ")}`);
  exit(1);
}
if (body !== undefined) {
  /** `value` with the new body, when it is an object that has a body. */
  const withBody = (value: unknown): unknown =>
    typeof value === "object" && value !== null && !Array.isArray(value) && "body" in value
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
let output: unknown = route.stdout ?? "";
if (JSON.stringify(output).includes("{{HEAD}}")) {
  const ready = `${run}/ready.json`;
  let head: string | undefined;
  try {
    head = Ready.parse(JSON.parse(readFileSync(ready, "utf8"))).head;
  } catch {
    head = undefined;
  }
  if (head === undefined) {
    console.error(`gh-standin: the route ${route.match} names {{HEAD}}, and ${ready} holds no head`);
    exit(1);
  }
  output = JSON.parse(JSON.stringify(output).replaceAll("{{HEAD}}", head));
}
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
if (text !== "") process.stdout.write(text.endsWith("\n") ? text : `${text}\n`);
if (route.stderr) process.stderr.write(route.stderr);
exit(route.exit ?? 0);
