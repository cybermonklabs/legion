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
// reads, every field GitHub's description requires (rest-required.json) among them. An answer to
// an installation token carries the installation's core counter in GitHub's rate limit headers,
// every such request but GET /rate_limit spending one call of it (`spend`), as production's every
// answer does, and a list is paged as GitHub pages it (`paged`): Dispatch's rate reading reads
// both (github-routes.ts's RATE_READING). A request for a repository or an installation the world
// does not hold is answered as GitHub answers it, 404. Any other request is refused loudly:
// answered 501 and logged with `refused`, so a read Dispatch adds later surfaces as a refusal,
// which health.ts's `github` rule makes the run unhealthy for, and never as an answer GitHub
// would not give.
//
// Its output is the run's <run>/logs/github.log (start_process), which the world does not show:
// one JSON object a line, {"at","listening":"127.0.0.1:<port>"} first, then one per request,
// {"at","method","path","accept","status","route"} for a route of the table (`path` with its
// query string, `route` the table's template) and {"at","method","path","accept","status":501,
// "refused"} for any other. A line that is not JSON is this program's own failure.
import { createHash, randomBytes } from "node:crypto";
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
import {
  API,
  accountsOf,
  counterLimit,
  INSTALLATION_COUNTERS,
  pullRequestJson,
  pusherOf,
  repositoryJson,
  userJson,
  WEB,
} from "./rest-objects";
import { CALLS } from "./run-records";

/** The world one stand-in serves, the tokens it minted, and its installation's core counter: when
 * the counter's hour began, in seconds, and the calls it counted since (`spend`). */
interface Served {
  readonly runDir: string;
  readonly repo: string;
  readonly defaultBranch: string;
  readonly minted: Map<string, Date>;
  readonly core: { start: number; used: number };
}

/** The installation of the run's App on the world's repository, and the App it installs, with the
 * App's client id, which its installation names too. */
const INSTALLATION_ID = 51_204_113;
const APP_ID = 2_101_400;
const CLIENT_ID = `Iv23li${INSTALLATION_ID}`;
/** When the App was installed, the world's own time. */
const INSTALLED = "2026-01-01T00:00:00Z";
/** The permissions GitHub grants that installation, the ones Dispatch's App asks for: actions
 * among them, which the delivery's reads of workflow runs (delivery/github_runs.go) and the rate
 * reading's of the workflows (ratereading.go core) need, GitHub answering `actions=read` as the
 * permission of a workflows read (`X-Accepted-Github-Permissions`, `gh api -i
 * repos/cybermonklabs/legion/actions/workflows?per_page=1`, `Date: Sat, 28 Nov 2026 19:08:01 GMT`). */
const PERMISSIONS = {
  actions: "read",
  checks: "read",
  contents: "read",
  issues: "read",
  metadata: "read",
  pull_requests: "read",
  statuses: "read",
};
/** The webhook events the App subscribes to. */
const EVENTS = ["check_run", "check_suite", "pull_request", "push", "status"];
/** The one read of an installation token that spends no call of its counter: "Calling this
 * endpoint does not count against your REST API rate limit" (GitHub's rate-limit page, as
 * githubrate.go's rateLimitPaths quotes it); production answered it with the core counter's
 * headers unchanged and `Cache-Control: no-cache` (`gh api -i rate_limit`, 19:07:28 GMT). */
const FREE_READ = "GET /rate_limit";
/** A counter's hour, and its minute, in seconds. */
const RESETS = { hour: 3600, minute: 60 };
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
  const state = mergeStateOf(at, threads, mergeStates, head, calls, () =>
    pusherOf(served.runDir, head)
  );
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

/** The App's slug, which its installation and its own answer name alike. */
const appSlug = (served: Served) => `${served.repo.split("/")[1] ?? ""}-dispatch`;

/** The installation of the run's App on the world's organization, as GitHub answers it to the
 * App's JWT by its repository (GET /repos/{owner}/{repo}/installation) and in the App's list of
 * its installations (GET /app/installations) alike. */
function installationJson(served: Served): Record<string, unknown> {
  const owner = served.repo.split("/")[0] ?? "";
  const account = userJson(owner, accountsOf(served.runDir), "Organization");
  return {
    id: INSTALLATION_ID,
    client_id: CLIENT_ID,
    account,
    repository_selection: "selected",
    access_tokens_url: `${API}/app/installations/${INSTALLATION_ID}/access_tokens`,
    repositories_url: `${API}/installation/repositories`,
    html_url: `${WEB}/organizations/${owner}/settings/installations/${INSTALLATION_ID}`,
    app_id: APP_ID,
    app_slug: appSlug(served),
    target_id: account.id,
    target_type: "Organization",
    permissions: PERMISSIONS,
    events: EVENTS,
    created_at: INSTALLED,
    updated_at: INSTALLED,
    single_file_name: null,
    has_multiple_single_files: false,
    single_file_paths: [],
    suspended_by: null,
    suspended_at: null,
  };
}

/** The App, as GitHub answers GET /app to its JWT (`integration`), in the form rest-objects.ts's
 * `appJson` gives a role App: the world's organization registered it, its slug, client id,
 * permissions and events are the ones its installation names, and that one installation is its
 * count. No pod holds an App's JWT, so no production read of it is pinned. */
function dispatchApp(served: Served): Record<string, unknown> {
  const owner = served.repo.split("/")[0] ?? "";
  const slug = appSlug(served);
  return {
    id: APP_ID,
    client_id: CLIENT_ID,
    slug,
    node_id: `A_kwDO${APP_ID}`,
    owner: userJson(owner, accountsOf(served.runDir), "Organization"),
    name: slug,
    description: "",
    external_url: `${WEB}/${owner}`,
    html_url: `${WEB}/apps/${slug}`,
    created_at: INSTALLED,
    updated_at: INSTALLED,
    permissions: PERMISSIONS,
    events: EVENTS,
    installations_count: 1,
  };
}

/** The page of `list` a request asks for, as GitHub pages a list read: `per_page` items (30 unless
 * the query string names another, at most 100) of page `page` (1 unless it names another), and
 * GitHub's `Link` to the list's other pages, each `url` with the request's own query string and
 * `page` set: the previous, the next, the last and the first, each where it is another page, and
 * a page past the last naming the last as both its previous and the last. A list of one page links
 * none. Measured on 2026-11-28 with a role App's installation token: of three repositories,
 * `installation/repositories?per_page=1` linked page 2 `next` and page 3 `last` (`Date: Sat, 28
 * Nov 2026 19:07:43 GMT`), and `?per_page=1&page=9` listed none and linked page 3 `prev` and
 * `last` and page 1 `first` (19:08:23 GMT); a repository's workflows were linked by the
 * repository's id (`repositories/1146927990/actions/workflows?per_page=1&page=2`, 19:08:01 GMT),
 * as the gh stand-in's lists are (gh-standin.ts's `linkOf`). */
function paged<T>(
  request: Request,
  list: readonly T[],
  url: string
): { items: T[]; link?: string } {
  const query = new URL(request.url).searchParams;
  const perPage = Math.min(100, Math.max(1, Number(query.get("per_page") ?? 30) || 30));
  const page = Math.max(1, Number(query.get("page") ?? 1) || 1);
  const last = Math.max(1, Math.ceil(list.length / perPage));
  const to = (target: number, rel: string) => {
    const linked = new URLSearchParams(query);
    linked.set("page", String(target));
    return `<${url}?${linked}>; rel="${rel}"`;
  };
  const links = [
    ...(page > 1 ? [to(Math.min(page - 1, last), "prev")] : []),
    ...(page < last ? [to(page + 1, "next")] : []),
    ...(page !== last ? [to(last, "last")] : []),
    ...(page > 1 ? [to(1, "first")] : []),
  ];
  return {
    items: list.slice((page - 1) * perPage, page * perPage),
    ...(links.length === 0 ? {} : { link: links.join(", ") }),
  };
}

/** A list's answer: `body`, with the `Link` its paging names, where it names one (`paged`). */
function listed(body: unknown, link: string | undefined): Response {
  return Response.json(body, link === undefined ? {} : { headers: { Link: link } });
}

/** The world's repository as an installation's list of its repositories names it: the form a pull
 * request carries (rest-objects.ts's `repositoryJson`), with the token's permissions on it, every
 * one false, and its custom properties, none, as production listed a repository of a role App's
 * installation (`gh api 'installation/repositories?per_page=1'`, 2026-11-28 at 19:07Z). */
function listedRepository(served: Served): Record<string, unknown> {
  return {
    ...repositoryJson(heldRepository(served), served.runDir, accountsOf(served.runDir), "pull"),
    permissions: { admin: false, maintain: false, pull: false, push: false, triage: false },
    custom_properties: {},
  };
}

/** The workflows GitHub lists for the world's repository (`workflow`): each workflow file of its
 * default branch's .github/workflows, as GitHub registers one, named by its `name:` and by its
 * path where it names none, active, created when the branch first held it and updated when it last
 * changed there, each time to GitHub's milliseconds as production wrote a workflow's
 * (`2026-11-16T17:02:36.000Z`, `gh api repos/cybermonklabs/legion/actions/workflows?per_page=1`,
 * 19:08:01 GMT), with an id and a node id of the world's own; none where the run's remote holds no
 * such branch yet. */
function workflowsOf(served: Served): Record<string, unknown>[] {
  const branch = String(heldRepository(served).default_branch ?? served.defaultBranch);
  const dir = ".github/workflows";
  const files = git(served, "ls-tree", "--name-only", `refs/heads/${branch}:${dir}`) ?? "";
  const time = (seconds: string | undefined) => new Date(Number(seconds) * 1000).toISOString();
  return files
    .split("\n")
    .filter((file) => /\.ya?ml$/.test(file))
    .map((file) => {
      const at = `${dir}/${file}`;
      const text = git(served, "cat-file", "blob", `refs/heads/${branch}:${at}`) ?? "";
      const name = /^name:[ \t]*["']?(.*?)["']?[ \t]*$/m.exec(text)?.[1] || at;
      const changed = (git(served, "log", "--format=%ct", `refs/heads/${branch}`, "--", at) ?? "")
        .split("\n")
        .filter((line) => line !== "");
      const id =
        400_000_000 +
        Number.parseInt(
          createHash("sha256").update(`${served.repo}\0${at}`).digest("hex").slice(0, 7),
          16
        );
      return {
        id,
        node_id: `W_kwDO${id}`,
        name,
        path: at,
        state: "active",
        created_at: time(changed.at(-1)),
        updated_at: time(changed[0]),
        url: `${API}/repos/${served.repo}/actions/workflows/${id}`,
        html_url: `${WEB}/${served.repo}/blob/${branch}/${at}`,
        badge_url: `${WEB}/${served.repo}/workflows/${encodeURIComponent(name)}/badge.svg`,
      };
    });
}

/** The installation's core counter at `now`, in seconds: the calls it counted in the hour it is
 * in, and when that hour ends; where no hour runs, none, and an hour from now, as production
 * answered every hourly counter no call had spent (rest-objects.ts's INSTALLATION_COUNTERS). */
function coreAt(served: Served, now: number): { used: number; reset: number } {
  const { start, used } = served.core;
  return start > 0 && now < start + RESETS.hour
    ? { used, reset: start + RESETS.hour }
    : { used: 0, reset: now + RESETS.hour };
}

/** Spends one call of the installation's core counter at `now`, its hour beginning where none
 * runs. */
function spend(served: Served, now: number): void {
  const { used } = coreAt(served, now);
  if (used === 0) served.core.start = now;
  served.core.used = used + 1;
}

/** `response` with the installation's core counter at `now` in GitHub's rate limit headers, as
 * production named it in every answer to an installation token (`X-Ratelimit-Limit`,
 * `-Remaining`, `-Reset`, `-Resource: core` and `-Used`, in `gh api -i` of the installation's
 * repositories, a repository's workflows and the counters, 2026-11-28 from 19:07:28Z). */
function withCore(served: Served, response: Response, now: number): Response {
  const limit = counterLimit("core");
  const { used, reset } = coreAt(served, now);
  const headers = new Headers(response.headers);
  headers.set("X-RateLimit-Limit", String(limit));
  headers.set("X-RateLimit-Remaining", String(limit - used));
  headers.set("X-RateLimit-Reset", String(reset));
  headers.set("X-RateLimit-Resource", "core");
  headers.set("X-RateLimit-Used", String(used));
  return new Response(response.body, { status: response.status, headers });
}

/** How each route of GITHUB_ROUTES is answered, by its template. A route's `{owner}/{repo}` is the
 * world's repository by then: `answer` refuses any other one first. */
const ANSWERS: Readonly<
  Record<string, (served: Served, params: Record<string, string>, request: Request) => Response>
> = {
  "GET /repos/{owner}/{repo}/installation": (served) => Response.json(installationJson(served)),
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
  // The App's installations, the world's one, a page of them as the request asks.
  "GET /app/installations": (served, _, request) => {
    const { items, link } = paged(request, [installationJson(served)], `${API}/app/installations`);
    return listed(items, link);
  },
  "GET /app": (served) => Response.json(dispatchApp(served)),
  // The installation's repositories, the world's one, in production's order of the fields.
  "GET /installation/repositories": (served, _, request) => {
    const { items, link } = paged(
      request,
      [listedRepository(served)],
      `${API}/installation/repositories`
    );
    return listed({ total_count: 1, repository_selection: "selected", repositories: items }, link);
  },
  "GET /repos/{owner}/{repo}/actions/workflows": (served, _, request) => {
    const workflows = workflowsOf(served);
    const id = String(heldRepository(served).id ?? "");
    const { items, link } = paged(
      request,
      workflows,
      `${API}/repositories/${id}/actions/workflows`
    );
    return listed({ total_count: workflows.length, workflows: items }, link);
  },
  // Every counter of the installation, each in production's order of its fields, and the core
  // one again as `rate`, as production answered (rest-objects.ts's INSTALLATION_COUNTERS).
  "GET /rate_limit": (served) => {
    const now = Math.floor(Date.now() / 1000);
    const resources = Object.fromEntries(
      INSTALLATION_COUNTERS.map(([name, limit, every]) => {
        const { used, reset } =
          name === "core" ? coreAt(served, now) : { used: 0, reset: now + RESETS[every] };
        return [name, { limit, used, remaining: limit - used, reset }];
      })
    );
    return Response.json(
      { resources, rate: resources.core },
      { headers: { "Cache-Control": "no-cache" } }
    );
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

/** `request`'s answer from `served`, and the log line it makes. A request with no credential its
 * route takes is GitHub's 401; every other to an installation token spends a call of the
 * installation's core counter but GET /rate_limit (FREE_READ), and carries the counter in its
 * headers (`withCore`). */
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
    const logged = (response: Response) => ({
      response,
      line: { ...asked, status: response.status, route: route.route },
    });
    if (!credited(served, route, request)) return logged(githubError(401, "Bad credentials"));
    const now = Math.floor(Date.now() / 1000);
    const counted = route.credential === "installation";
    if (counted && route.route !== FREE_READ) spend(served, now);
    const response =
      params.owner !== undefined && `${params.owner}/${params.repo}` !== served.repo
        ? githubError(404, "Not Found")
        : answering(served, params, request);
    return logged(counted ? withCore(served, response, now) : response);
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
  const served: Served = {
    runDir,
    repo,
    defaultBranch,
    minted: new Map(),
    core: { start: 0, used: 0 },
  };
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
