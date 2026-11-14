#!/usr/bin/env bun
// Stand-in for `gh` on a skill scenario's PATH. Each call is recorded to
// $SKILL_SCENARIO_RUN/calls.jsonl (argv, the contents of any file or stdin it hands GitHub a body
// through, its exit status, the pull request body it kept, if any, and what became of any
// settlement it published), then answered from the first route in $SKILL_SCENARIO_RUN/fixtures.json
// whose regex matches "gh <argv...>". A call no route matches answers `gh: Not Found (HTTP 404)`
// and exits 1. Nothing is sent anywhere but the settlement below.
// `--json a,b` picks fields and `--jq`/`-q` runs jq, as gh does. A pull request body edit (`gh pr
// edit --body`, `--body-file`, or a PATCH through `gh api`) is kept: every route answering with an
// object that has a `body` then answers with the new one, as GitHub's next read would, and the
// call's record carries it as `body`.
// A route answers a sequence rather than the one answer, and stops matching once it has answered
// its share, so one head reads red before a re-run and green after it:
//   `replies` is a list of {stdout?, stderr?, exit?}, served in order: the first call matching the
//   route takes the first reply, the second the second, and a call past the end takes the last
//   again. A route with no `replies` answers its own {stdout, stderr, exit} to every call.
//   `follows` names another route by its `match` text, and the sequence is then reached by the
//   calls that route has answered rather than by this route's own: a read of the head that follows
//   the re-run's route answers its first reply however many times it is read, and its second from
//   the moment the re-run is pressed. The head's state changes when the role acts, never when it
//   reads, so a role that reads the head twice in one turn reads the same state twice. A `follows`
//   that names no route of the file reaches nothing, leaving the route at its first reply.
//   `spends` is how many matching calls the route answers. After that many it matches nothing, and
//   the call falls through to the next route whose pattern matches it, or to the 404.
// The stand-in is a fresh process per call, so the count of calls a route has answered outlives it
// in fixtures.json, as the route's `answered`, written back with the body keep's own
// read-modify-write. That write happens where this call changed what the file holds — a body edit,
// or an `answered` that is read back: a route carrying `spends` or `replies`, or one another
// route's `replies` follow — and never for the calls of a plain route. It is atomic: a sibling
// temporary file in the run directory, renamed over fixtures.json, so a second `gh` process of the
// same turn never parses a half-written file.
// `publish` is an envelope a route publishes once it has answered: the stand-in POSTs it as it
// stands to $ENVOY_URL/v1/messages/publish, with the bearer the file $ENVOY_TOKEN_FILE names, so a
// role that subscribes to the pull request's checks topic and ends its turn is woken by the
// settlement the fixture wrote. The fixture writes every field of that envelope (`topic`, `source`,
// `message`, its `payload`) and the stand-in fills in none of it. A publish that fails leaves the
// call's exit status to the route, since a non-zero one would read to the agent as a failed `gh`
// call; what became of it is the call's record `published`.
//   gh-standin.ts gh <args...>
import { appendFileSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { z } from "zod";

/** One answer: what a route serves a call, and what each entry of a route's `replies` serves. */
const Reply = z.object({
  stdout: z.unknown().optional(),
  stderr: z.string().optional(),
  exit: z.number().optional(),
});
/** rig.sh's worker_fixture writes each route as {match, stdout?, stderr?, exit?}, and a route that
 * answers a sequence, follows another route's calls, spends itself or publishes a settlement adds
 * `replies`, `follows`, `spends` and `publish`. `answered` is the stand-in's own: the count of
 * calls the route has answered. */
const Fixtures = z.looseObject({
  routes: z.array(
    Reply.extend({
      match: z.string(),
      replies: z.array(Reply).optional(),
      follows: z.string().optional(),
      spends: z.number().optional(),
      answered: z.number().optional(),
      publish: z.unknown().optional(),
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
/** The routes this call can match: a route that has answered its `spends` calls is spent, and the
 * call falls through to the next route whose pattern matches it. */
const routes = fixtures.routes.filter(
  (route) => route.spends === undefined || (route.answered ?? 0) < route.spends
);

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
/** What became of the settlement a route publishes, where it names one. */
type Published = { status: number; body: string } | { error: string };
let published: Published | undefined;
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
    `${JSON.stringify({ at, as, argv: args, files, stdin, route: route?.match ?? null, body: kept, published, exit: status })}\n`
  );
  process.exit(status);
}
/** `stdout` as GitHub's next read would answer it, once this call edited the pull request's body:
 * an answer that is an object with a `body` answers the new one. */
function withBody(stdout: unknown, edit: string): unknown {
  return typeof stdout === "object" && stdout !== null && "body" in stdout
    ? { ...stdout, body: edit }
    : stdout;
}
/** Publishes the envelope a route names, as the settlement GitHub's checks would raise: a POST of
 * what the fixture wrote, nothing of it filled in here, to $ENVOY_URL/v1/messages/publish with the
 * bearer the file $ENVOY_TOKEN_FILE names. A publish that fails is the stand-in's own trouble, not
 * the agent's: it is answered with what went wrong, for the call's record. The POST is bounded at
 * five seconds, as the rig's other probes are (rig.sh's pg_sql and nats_ready): a listener that
 * takes the request and never answers would otherwise hold the agent's own `gh` call until the
 * run's timeout, and the abort lands in the catch below as the call's `published.error`. */
async function publish(envelope: unknown): Promise<Published> {
  const url = process.env.ENVOY_URL;
  const tokenFile = process.env.ENVOY_TOKEN_FILE;
  if (!url || !tokenFile) return { error: "ENVOY_URL or ENVOY_TOKEN_FILE is unset" };
  try {
    const token = readFileSync(tokenFile, "utf8").trim();
    const answer = await fetch(`${url.replace(/\/+$/, "")}/v1/messages/publish`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
      body: JSON.stringify(envelope),
      signal: AbortSignal.timeout(5000),
    });
    return { status: answer.status, body: (await answer.text()).trim() };
  } catch (error) {
    return { error: error instanceof Error ? error.message : String(error) };
  }
}
if (!route) {
  console.error(as === "gh" ? "gh: Not Found (HTTP 404)" : `unknown command: ${args.join(" ")}`);
  exit(1);
}
/** The answer this call takes: the reply its route's sequence has reached — the first call takes
 * the first reply, a call past the end takes the last again — or the route's own answer. A route
 * that follows another reaches its sequence by that route's answered calls rather than its own, so
 * the state it answers turns on the call the role makes, not on the reads it makes. */
const replies = route.replies ?? [];
const followed =
  route.follows === undefined
    ? route
    : fixtures.routes.find((candidate) => candidate.match === route.follows);
const answer = replies[Math.min(followed?.answered ?? 0, replies.length - 1)] ?? route;
// The count of calls each route has answered outlives this process in fixtures.json, with the body
// where this call edited the pull request's: one read-modify-write of the file carries both. The
// file is written only where this call changed what it holds: a body edit, or an `answered` that
// is read back — this route carries `spends`, it carries `replies`, or another route's `replies`
// follow it by `match`. The calls of a plain route change nothing a later call reads.
const counted =
  route.spends !== undefined ||
  route.replies !== undefined ||
  fixtures.routes.some(
    (candidate) => candidate.replies !== undefined && candidate.follows === route.match
  );
if (body !== undefined || counted) {
  const edited = fixtures.routes.map((candidate) => ({
    ...candidate,
    answered: candidate === route ? (candidate.answered ?? 0) + 1 : candidate.answered,
    ...(body === undefined
      ? {}
      : {
          stdout: withBody(candidate.stdout, body),
          replies: candidate.replies?.map((reply) => ({
            ...reply,
            stdout: withBody(reply.stdout, body),
          })),
        }),
  }));
  // The rename is what makes the write atomic: the temporary file is a sibling, in the run
  // directory, so the rename is within one filesystem.
  const temporary = `${run}/fixtures.${process.pid}.json`;
  writeFileSync(temporary, JSON.stringify({ ...fixtures, routes: edited }));
  renameSync(temporary, `${run}/fixtures.json`);
}
if (body !== undefined) kept = body;
let output: unknown = answer.stdout ?? "";
const jsonIndex = args.indexOf("--json");
if (jsonIndex >= 0 && typeof output === "object" && output !== null && !Array.isArray(output)) {
  const record: Record<string, unknown> = { ...output };
  const fields = (args[jsonIndex + 1] ?? "").split(",");
  output = Object.fromEntries(
    fields.filter((field) => field in record).map((field) => [field, record[field]])
  );
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
if (answer.stderr) process.stderr.write(answer.stderr);
if (route.publish !== undefined) published = await publish(route.publish);
exit(answer.exit ?? 0);
