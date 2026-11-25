#!/usr/bin/env bun
// GitHub's REST API as a run's Dispatch server reads it. rig.sh's start_dispatch starts this, one
// per run, before the run's Dispatch, and hands its address to the server as
// DISPATCH_GITHUB_API_BASE (run-server.sh's FAKE_GITHUB_PORT). Production's Dispatch reads GitHub
// through its App to say where a result's commit stands, and a world whose GitHub was port 1,
// where nothing listens, told every reviewer `A pass stands: not known`, Dispatch "cannot read the
// repository", where production answers `no`, that commit being none of the compared commits
// (INFRA-764, measured 2026-11-16 on a result one push below the pull request's head).
//
//   bun github-standin.ts <run dir> <owner/repo> <default branch>
//
// It answers the routes the world's flows were measured to ask for (github-routes.ts's
// GITHUB_ROUTES, each with the Dispatch code that asks for it), each from the run's own remote
// (<run>/remote.git) and the world's own data (<run>/world.json, its accounts, the gh answers
// rig.sh's fixture_write writes, <run>/fixtures.json, and the record of the gh calls the run
// made), read at each request: the world is built after the server starts, and every push of the
// run moves it. Each answer is GitHub's whole object, built as the gh stand-in builds it
// (rest-objects.ts), so the repository and the pull request Dispatch reads are the ones an agent
// reads, every field GitHub's description requires (rest-required.json) among them. A request for
// a repository or an
// installation the world does not hold is answered as GitHub answers it, 404. Any other request is
// refused loudly: answered 501 and logged with `refused`, so a read Dispatch adds later surfaces as
// a refusal, which health.ts's `github` rule makes the run unhealthy for, and never as an answer
// GitHub would not give.
//
// Its output is the run's <run>/logs/github.log (start_process), which the world does not show:
// one JSON object a line, {"at","listening":"127.0.0.1:<port>"} first, then one per request,
// {"at","method","path","accept","status","route"} for a route of the table (`path` with its
// query string, `route` the table's template) and {"at","method","path","accept","status":501,
// "refused"} for any other. A line that is not JSON is this program's own failure.
import { randomBytes } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { type Route, readWorld, routeFor } from "./gh-call";
import {
  commentsOf,
  conversationOf,
  mergeableOf,
  mergeStateOf,
  recordedCalls,
  type ThreadRoute,
  threadState,
} from "./gh-threads";
import { GITHUB_ROUTES, type GithubRoute, githubRouteOf } from "./github-routes";
import { accountsOf, pullRequestJson, pusherOf, repositoryJson, userJson } from "./rest-objects";
import { CALLS } from "./run-records";

/** The world one stand-in serves, and the tokens it minted. */
interface Served {
  readonly runDir: string;
  readonly repo: string;
  readonly defaultBranch: string;
  readonly minted: Map<string, Date>;
}

/** The installation of the run's App on the world's repository, and the App it installs. */
const INSTALLATION_ID = 51_204_113;
const APP_ID = 2_101_400;
/** When the App was installed, the world's own time. */
const INSTALLED = "2026-01-01T00:00:00Z";
/** The permissions GitHub grants that installation, the ones Dispatch's App asks for. */
const PERMISSIONS = {
  checks: "read",
  contents: "read",
  issues: "read",
  metadata: "read",
  pull_requests: "read",
  statuses: "read",
};
/** Why a request no route of the table answers is refused. */
export const UNROUTED = "no route of the world answers it";

/** What `git -C <run>/remote.git <args...>` prints, trimmed, or undefined when it fails. */
function git(served: Served, ...args: string[]): string | undefined {
  const read = Bun.spawnSync(["git", "-C", path.join(served.runDir, "remote.git"), ...args]);
  return read.exitCode === 0 ? read.stdout.toString().trim() : undefined;
}

/** The world's gh answers and the merge states it names for a head (rig.sh's fixture_write), or
 * none before fixture_write wrote them. */
function fixturesOf(served: Served): { routes: ThreadRoute[]; mergeStates: unknown } {
  const file = path.join(served.runDir, "fixtures.json");
  if (!existsSync(file)) return { routes: [], mergeStates: {} };
  const read: { routes?: Route[]; mergeStates?: unknown } = JSON.parse(readFileSync(file, "utf8"));
  return { routes: read.routes ?? [], mergeStates: read.mergeStates ?? {} };
}

/** GitHub's error body, as its REST API writes one. */
function githubError(status: number, message: string): Response {
  return Response.json(
    { message, documentation_url: "https://docs.github.com/rest", status: String(status) },
    { status }
  );
}

/** The world's repository settings, as the gh stand-in answers `gh api repos/<repo>` from them
 * (rig.sh's fixture_write), and before the world is built the settings of a repository on the
 * default branch the run was started with. */
function heldRepository(served: Served): Record<string, unknown> {
  const held = routeFor(fixturesOf(served).routes, ["api", `repos/${served.repo}`])?.stdout;
  if (typeof held === "object" && held !== null && !Array.isArray(held))
    return held as Record<string, unknown>;
  const [owner = "", name = ""] = served.repo.split("/");
  return {
    id: 1,
    node_id: "R_kgDOAAAAAQ",
    name,
    full_name: served.repo,
    owner: { login: owner, type: "Organization" },
    default_branch: served.defaultBranch,
  };
}

/** The pull request the world's remote holds, as GitHub's REST API answers it in `form` (rest-
 * objects.ts's `pullRequestJson`): its head the tip of the issue branch at the call and its base
 * the tip of the branch it is open against, read from the run's remote; its merge state and
 * mergeability the ones its base's rules leave it in with the run's reviews, checks and threads,
 * and its comment counts the run's, as the gh stand-in answers them (gh-threads.ts's
 * `mergeStateOf`, `mergeableOf`, `threadState`, `conversationOf`); the rest the world's
 * (fixtures.json's `gh pr view`). Undefined before the world is built. */
function pullRequest(served: Served, form: "simple" | "full"): Record<string, unknown> | undefined {
  const read = existsSync(path.join(served.runDir, "world.json"))
    ? readWorld(served.runDir)
    : undefined;
  if (read === undefined || !("world" in read)) return undefined;
  const world = read.world;
  const { routes, mergeStates } = fixturesOf(served);
  const held = routeFor(routes, ["pr", "view", String(world.pr)])?.stdout;
  const head = git(served, "rev-parse", `refs/heads/${world.branch}`);
  const base = git(served, "rev-parse", `refs/heads/${world.base}`);
  if (head === undefined || base === undefined) return undefined;
  const record = path.join(served.runDir, CALLS);
  const calls = existsSync(record) ? recordedCalls(record) : [];
  const at = { routes, world };
  const threads = threadState(at, calls);
  const view = typeof held === "object" && held !== null ? (held as Record<string, unknown>) : {};
  const comments =
    (Array.isArray(view.comments) ? view.comments.length : 0) + conversationOf(world, calls).length;
  const state = mergeStateOf(at, threads, mergeStates, head, calls, pusherOf(served.runDir, head));
  return pullRequestJson(
    { ...view, mergeStateStatus: state, mergeable: mergeableOf(state) },
    {
      runDir: served.runDir,
      repo: served.repo,
      pr: world.pr,
      branch: world.branch,
      base: world.base,
      head,
      baseSha: base,
      repository: heldRepository(served),
      accounts: accountsOf(served.runDir),
      comments,
      reviewComments: threads.list
        .flatMap(commentsOf)
        .filter((comment) => comment.state !== "PENDING").length,
    },
    form
  );
}

/** The head and base commits of a pull request object `pullRequest` built. */
const sides = (pull: Record<string, unknown>) => ({
  head: (pull.head as { sha: string }).sha,
  base: (pull.base as { sha: string }).sha,
});

/** How each route of GITHUB_ROUTES is answered, by its template. A route's `{owner}/{repo}` is the
 * world's repository by then: `answer` refuses any other one first. */
const ANSWERS: Readonly<
  Record<string, (served: Served, params: Record<string, string>) => Response>
> = {
  "GET /repos/{owner}/{repo}/installation": (served) => {
    const [owner = "", name = ""] = served.repo.split("/");
    const account = userJson(owner, accountsOf(served.runDir), "Organization");
    return Response.json({
      id: INSTALLATION_ID,
      client_id: `Iv23li${INSTALLATION_ID}`,
      account,
      repository_selection: "selected",
      access_tokens_url: `https://api.github.com/app/installations/${INSTALLATION_ID}/access_tokens`,
      repositories_url: "https://api.github.com/installation/repositories",
      html_url: `https://github.com/organizations/${owner}/settings/installations/${INSTALLATION_ID}`,
      app_id: APP_ID,
      app_slug: `${name}-dispatch`,
      target_id: account.id,
      target_type: "Organization",
      permissions: PERMISSIONS,
      events: ["check_run", "check_suite", "pull_request", "push", "status"],
      created_at: INSTALLED,
      updated_at: INSTALLED,
      single_file_name: null,
      has_multiple_single_files: false,
      single_file_paths: [],
      suspended_by: null,
      suspended_at: null,
    });
  },
  "POST /app/installations/{id}/access_tokens": (served, { id }) => {
    if (Number(id) !== INSTALLATION_ID) return githubError(404, "Not Found");
    const token = `ghs_${randomBytes(18).toString("hex")}`;
    const expires = new Date(Date.now() + 60 * 60 * 1000);
    served.minted.set(token, expires);
    return Response.json(
      {
        token,
        expires_at: expires.toISOString(),
        permissions: PERMISSIONS,
        repository_selection: "selected",
      },
      { status: 201 }
    );
  },
  // `Accept: application/vnd.github.sha`: the commit's SHA alone, or GitHub's 422 for a commit the
  // repository does not hold, whose message Dispatch reads as that proof (commits.go HasCommit).
  "GET /repos/{owner}/{repo}/commits/{sha}": (served, { sha = "" }) => {
    const held = git(served, "rev-parse", "--verify", "--quiet", `${sha}^{commit}`);
    if (held === undefined) return githubError(422, `No commit found for SHA: ${sha}`);
    return new Response(held, {
      headers: { "Content-Type": "application/vnd.github.sha; charset=utf-8" },
    });
  },
  "GET /repos/{owner}/{repo}": (served) =>
    Response.json(
      repositoryJson(heldRepository(served), served.runDir, accountsOf(served.runDir), "full")
    ),
  // The pull requests GitHub associates with a commit: for one on the issue branch alone, the open
  // pull request whose commits it is among; for one the default branch already holds, the merged
  // pull request that brought it there, which no world has; for one the remote does not hold,
  // GitHub's 422.
  "GET /repos/{owner}/{repo}/commits/{sha}/pulls": (served, { sha = "" }) => {
    if (git(served, "cat-file", "-e", `${sha}^{commit}`) === undefined)
      return githubError(422, `No commit found for SHA: ${sha}`);
    const pull = pullRequest(served, "simple");
    if (pull === undefined) return Response.json([]);
    const { head, base } = sides(pull);
    const onBranch = git(served, "merge-base", "--is-ancestor", sha, head) !== undefined;
    const onBase = git(served, "merge-base", "--is-ancestor", sha, base) !== undefined;
    return Response.json(onBranch && !onBase ? [pull] : []);
  },
  // One pull request, as GitHub answers it by its number: the listing's object with the counts
  // and the mergeability GitHub adds to a single one.
  "GET /repos/{owner}/{repo}/pulls/{number}": (served, { number }) => {
    const pull = pullRequest(served, "full");
    if (pull === undefined || pull.number !== Number(number)) return githubError(404, "Not Found");
    return Response.json(pull);
  },
};

/** The bearer a request carries, or undefined. */
const bearer = (request: Request) =>
  /^Bearer (\S+)$/.exec(request.headers.get("authorization") ?? "")?.[1];

/** Whether `request` carries the credential `route` takes: an App's JWT, three base64url parts,
 * or an installation token this stand-in minted and has not let lapse. */
function credited(served: Served, route: GithubRoute, request: Request): boolean {
  const given = bearer(request) ?? "";
  if (route.credential === "app") return /^[\w-]+\.[\w-]+\.[\w-]+$/.test(given);
  const expires = served.minted.get(given);
  return expires !== undefined && expires.getTime() > Date.now();
}

// Every route of GITHUB_ROUTES has an answer and every answer a route: either one missing is this
// program's own error.
for (const { route } of GITHUB_ROUTES)
  if (ANSWERS[route] === undefined) throw new Error(`github-standin.ts answers no ${route}`);
for (const template of Object.keys(ANSWERS))
  if (!GITHUB_ROUTES.some((route) => route.route === template))
    throw new Error(`github-standin.ts answers ${template}, which GITHUB_ROUTES does not hold`);

/** `request`'s answer from `served`, and the log line it makes. */
function answer(served: Served, request: Request): { response: Response; line: object } {
  const url = new URL(request.url);
  const accept = request.headers.get("accept") ?? "";
  const asked = {
    at: new Date().toISOString(),
    method: request.method,
    path: `${url.pathname}${url.search}`,
    accept,
  };
  const found = githubRouteOf(request.method, url.pathname, accept);
  const answering = found === undefined ? undefined : ANSWERS[found.route.route];
  if (found !== undefined && answering !== undefined) {
    const { route, params } = found;
    const response = !credited(served, route, request)
      ? githubError(401, "Bad credentials")
      : params.owner !== undefined && `${params.owner}/${params.repo}` !== served.repo
        ? githubError(404, "Not Found")
        : answering(served, params);
    return { response, line: { ...asked, status: response.status, route: route.route } };
  }
  return {
    response: githubError(501, `${request.method} ${url.pathname}: ${UNROUTED}`),
    line: { ...asked, status: 501, refused: UNROUTED },
  };
}

if (import.meta.main) {
  const [runDir = "", repo = "", defaultBranch = ""] = Bun.argv.slice(2);
  if (runDir === "" || !/^[^/\s]+\/[^/\s]+$/.test(repo) || defaultBranch === "") {
    console.error("usage: github-standin.ts <run dir> <owner/repo> <default branch>");
    process.exit(2);
  }
  const served: Served = { runDir, repo, defaultBranch, minted: new Map() };
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const { response, line } = answer(served, request);
      console.log(JSON.stringify(line));
      return response;
    },
  });
  console.log(
    JSON.stringify({ at: new Date().toISOString(), listening: `127.0.0.1:${server.port}` })
  );
}
