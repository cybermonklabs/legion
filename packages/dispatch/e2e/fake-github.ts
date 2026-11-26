import { harnessPorts } from "./harness-ports";

// Minimal GitHub App API for the architecture-source access check, the
// architecture importer, the delivery reconcile and the issue header's GitHub link:
// the installation lookup, the installation-token mint, the repository read that
// proves the minted token works, the exact branch read a source's save makes, the
// commit/tree/blob reads a sync walks, the commits read whose 422 says a repository
// holds no such commit, empty delivery listings, and a seeded pull request with its
// commits, its head's check runs and workflow runs. The GraphQL route answers the
// aliased pull request queries of the reconcile's facts reads from the seeded pull
// requests. It answers the merged search of the reconcile from the merged pull requests
// that a seed marks `searched`. It answers an empty search to every other query. The seed
// fields of the search and the completion are `searched`, `author`, `created_at`,
// `merged_at` and `labels`. A pull request with one of them also answers what the
// completion of a partial row reads. A repository seeded as rate_limited answers as GitHub
// does once its installation's budget is spent.
// Tests seed per-repository state through /__fixture/repos and read it back there,
// and read how many times the server asked for each commit's check runs and for each
// repository's pull requests, and each call it made, through /__fixture/requests; the
// server reaches this via DISPATCH_GITHUB_API_BASE.

const ARCHITECTURE_DIR = ".dispatch/architecture";

/** A pull request the GitHub link reads: its title, state and head commit, and the branch,
 *  base and body Dispatch's pull-request head tracking attributes it by. A seed that sets
 *  `searched`, `author`, `created_at`, `merged_at` or `labels` also gets the fields that the
 *  completion of a partial row reads (`completes`). A seed without them gets the same answers
 *  as before these fields. */
interface FakePull {
  readonly head_sha: string;
  readonly merged?: boolean;
  readonly state: string;
  readonly title: string;
  readonly body?: string;
  readonly head_ref?: string;
  /** The repository the head branch lives in, as GitHub's `head.repo.full_name`; the pull
   *  request's own repository when unset. Dispatch compares a check's results with a pull
   *  request's head only when the two are the same, so a fork's name here is how a fixture
   *  describes a pull request that attributes nobody. */
  readonly head_repo?: string;
  readonly base_ref?: string;
  readonly updated_at?: string;
  /** The commit GitHub made when the pull request merged, which a merged one answers. */
  readonly merge_commit_sha?: string;
  /** The message of each commit of the pull request, oldest first, as the commits read pages
   *  them and the GraphQL query answers them. One commit with an empty message when unset. */
  readonly commit_messages?: readonly string[];
  /** Where set, the commits read of this pull request answers this status in place of its
   *  commits, so a REST read stores the texts with the messages not read. A seed without it
   *  clears it. The GraphQL query still answers the messages. */
  readonly commits_status?: number;
  /** The merged search finds this pull request when the search names its repository. The pull
   *  request must also be merged and have a merge time (`merged_at` or `updated_at`). */
  readonly searched?: boolean;
  /** The author's login in the REST form, where a bot login ends in "[bot]". The default is
   *  "octocat". The search gives a bot as GraphQL does: the login without "[bot]", type Bot. */
  readonly author?: string;
  /** The time of creation. The default is 24 hours before `updated_at`. */
  readonly created_at?: string;
  /** The time of the merge of a merged pull request. The default is `updated_at`. */
  readonly merged_at?: string;
  /** The names of the labels. The default is no label. In the deploy repository, the population
   *  rule refuses a pull request without "task" or "non-task". */
  readonly labels?: readonly string[];
}

/** One check run of a commit, in the check suite `check_suite_id` names. GitHub always answers
 *  `completed_at`, `html_url` and `output`, so the fake does too: a run that leaves
 *  `completed_at` unset answers null, as a run not yet completed does, one that leaves
 *  `html_url` unset answers the run's page on github.com, and one that leaves `output` unset
 *  answers a null title and summary, as a GitHub Actions job does. */
interface FakeCheckRun {
  readonly app_id?: number;
  readonly check_suite_id: number;
  readonly completed_at?: string | null;
  readonly conclusion: string | null;
  readonly html_url?: string;
  readonly id: number;
  readonly name: string;
  readonly output?: { readonly title: string | null; readonly summary: string | null };
  readonly status: string;
}

/** What the check-runs read of one commit does in place of answering at once: answers `status`
 *  with GitHub's error body, or waits `hold_ms` milliseconds and then answers as it would have. */
type FakeCheckRunFault = { readonly status: number } | { readonly hold_ms: number };

/** One workflow run of a commit: its workflow's name, its triggering event and its check suite. */
interface FakeWorkflowRun {
  readonly check_suite_id: number;
  readonly event: string;
  readonly id: number;
  readonly name: string;
}

/** GitHub's page of a list: `per_page` (default 30, at most 100) items from page `page`. */
function pageOf<T>(items: readonly T[], url: URL): T[] {
  const perPage = Math.min(Number(url.searchParams.get("per_page") ?? 30), 100);
  const page = Number(url.searchParams.get("page") ?? 1);
  return items.slice((page - 1) * perPage, page * perPage);
}

interface FakeRepo {
  /** Contents permission the installation reports: "read", "write", "none". */
  readonly contents: string;
  /** Actions and Pull-requests permissions a delivery reconcile needs. */
  readonly actions?: string;
  readonly pull_requests?: string;
  readonly installation_id: number;
  /** Markdown files inside .dispatch/architecture/, keyed by file name. The
   *  branch commit is derived from their content, so a re-seed with different
   *  files moves the commit exactly like a push would. */
  readonly files?: Record<string, string>;
  /** The repository's branches, every one at the commit `files` derives; `["main"]` when
   *  unset, GitHub's default branch. The exact branch read answers 404 for any other name. */
  readonly branches?: readonly string[];
  /** Pull requests keyed by number; a number not here answers a server error, as before. */
  readonly pulls?: Record<string, FakePull>;
  /** Check runs keyed by commit sha, in any order. The read pages them as GitHub does
   *  (`pageOf`), so a commit of 101 runs answers 100 on page 1 and one on page 2. */
  readonly check_runs?: Record<string, readonly FakeCheckRun[]>;
  /** What the check-runs read of a commit does instead of answering at once, keyed by commit
   *  sha; a commit not here answers at once. */
  readonly check_run_faults?: Record<string, FakeCheckRunFault>;
  /** Workflow runs keyed by head sha. */
  readonly workflow_runs?: Record<string, readonly FakeWorkflowRun[]>;
  /** The repository's default branch; "main" when unset. */
  readonly default_branch?: string;
  /** The pull request numbers GitHub names for a commit, keyed by commit sha: what
   *  `GET /repos/{o}/{r}/commits/{sha}/pulls` answers, read from `pulls`. */
  readonly commit_pulls?: Record<string, readonly number[]>;
  /** The commits each branch holds, keyed by branch name: what the compare read answers
   *  `behind` (or `identical`, for the last of them) about, and `diverged` about any other. */
  readonly branch_commits?: Record<string, readonly string[]>;
  /** Commits the repository holds with nothing else attached to them: what the commits read
   *  answers 200 for beside the commits the fields above imply. A fixture that wants a commit
   *  refused simply leaves it out of every one of them. */
  readonly commits?: readonly string[];
  /** A repository Dispatch's App is not installed on: every read of it answers, but the
   *  installation lookup is GitHub's 404, so the server can reach no token for it. */
  readonly installation_missing?: boolean;
  /** The repository's installation has spent its rate limit: every REST call of each of its
   *  repositories answers 403 with `x-ratelimit-remaining: 0`, and every GraphQL query made with
   *  its token answers RATE_LIMITED. The installation lookup and the token mint are the App's own
   *  calls, and answer as before. */
  readonly rate_limited?: boolean;
}

/** Deterministic commit sha for a repository's current files. */
function commitFor(repo: FakeRepo): string {
  const hasher = new Bun.CryptoHasher("sha1");
  hasher.update(JSON.stringify(repo.files ?? {}));
  return hasher.digest("hex");
}

/** Every commit a repository holds, which the commits read answers 200 for: each commit the
 *  fixture attaches something to — the pull requests GitHub names for a commit, each pull
 *  request's head, the commits a branch holds, and the commits check runs and workflow runs
 *  hang off — the commit `files` derives, and the bare `commits` the fixture names. */
function heldCommits(repo: FakeRepo): Set<string> {
  return new Set([
    commitFor(repo),
    ...Object.keys(repo.commit_pulls ?? {}),
    ...Object.values(repo.pulls ?? {}).map((pull) => pull.head_sha),
    ...Object.values(repo.branch_commits ?? {}).flat(),
    ...Object.keys(repo.check_runs ?? {}),
    ...Object.keys(repo.workflow_runs ?? {}),
    ...(repo.commits ?? []),
  ]);
}

/** The permissions an installation reports for a repository, in both installation answers. */
function permissionsOf(repo: FakeRepo): Record<string, string | undefined> {
  return { actions: repo.actions, contents: repo.contents, pull_requests: repo.pull_requests };
}

/** The messages of a pull request's commits, oldest first: one empty message where the fixture
 *  names none, since every pull request holds at least one commit. */
function messagesOf(pull: FakePull): readonly string[] {
  return pull.commit_messages ?? [""];
}

/** The sha of the commit at `index` of a pull request's commits: the head for the last one, and a
 *  sha derived from the repository, the number and the index for each one before it. */
function commitShaOf(fullName: string, number: string, pull: FakePull, index: number): string {
  if (index === messagesOf(pull).length - 1) return pull.head_sha;
  const hasher = new Bun.CryptoHasher("sha1");
  hasher.update(`${fullName}#${number}/${index}`);
  return hasher.digest("hex");
}

/** Whether a seed sets a field of the merged search or the completion. Only such a pull request
 *  answers the fields of the completion, so that the answer to an older seed does not change. */
function completes(pull: FakePull): boolean {
  return (
    pull.searched !== undefined ||
    pull.author !== undefined ||
    pull.created_at !== undefined ||
    pull.merged_at !== undefined ||
    pull.labels !== undefined
  );
}

/** The time `hours` hours before `at`, in GitHub's form. It is undefined when `at` is unset or
 *  is not a time. */
function hoursBefore(at: string | undefined, hours: number): string | undefined {
  const time = at === undefined ? Number.NaN : Date.parse(at);
  if (Number.isNaN(time)) return undefined;
  return new Date(time - hours * 3_600_000).toISOString().replace(".000Z", "Z");
}

/** The login of the author, in the REST form. */
function authorOf(pull: FakePull): string {
  return pull.author ?? "octocat";
}

/** The time of creation, or undefined when the seed gives no time to make it from. */
function createdAtOf(pull: FakePull): string | undefined {
  return pull.created_at ?? hoursBefore(pull.updated_at, 24);
}

/** The time of the merge of a merged pull request. It is undefined for a pull request that is
 *  not merged. */
function mergedAtOf(pull: FakePull): string | undefined {
  return pull.merged === true ? (pull.merged_at ?? pull.updated_at) : undefined;
}

/** The author date and the committer date of each commit of a pull request that `completes`
 *  selects: 2 hours before `updated_at`. Without `updated_at`, a commit has no date, and the
 *  completion of the pull request fails. */
function commitDatesOf(pull: FakePull): Record<string, unknown> {
  const date = completes(pull) ? hoursBefore(pull.updated_at, 2) : undefined;
  return date === undefined ? {} : { author: { date }, committer: { date } };
}

/** The GraphQL `author` of a login in the REST form. GraphQL gives a bot its login without
 *  "[bot]" and the type Bot. */
function actorOf(login: string): { __typename: string; login: string } {
  const bot = login.match(/^(.+)\[bot\]$/)?.[1];
  return bot === undefined ? { __typename: "User", login } : { __typename: "Bot", login: bot };
}

/** The nodes that the merged search `q` finds, in the order of the `repo:` qualifiers of `q`,
 *  and by number in each repository. The search reads only the repositories of installation
 *  `installationId`. A node is a merged pull request that the seed marks `searched`. Its merge
 *  time must be in `merged:<from>..<to>`, and GitHub includes both ends. When `q` has `author:`
 *  qualifiers, the author must be one of them. */
function searchNodes(q: string, installationId: number | undefined): unknown[] {
  const names = new Set<string>();
  const authors: string[] = [];
  let from = Number.NEGATIVE_INFINITY;
  let to = Number.POSITIVE_INFINITY;
  for (const term of q.split(/\s+/)) {
    const at = term.indexOf(":");
    if (at === -1) continue;
    const qualifier = term.slice(0, at);
    const value = term.slice(at + 1);
    if (qualifier === "repo") names.add(value);
    if (qualifier === "author") authors.push(value);
    if (qualifier === "merged") {
      const [start = "", end = ""] = value.split("..");
      from = Date.parse(start);
      to = Date.parse(end);
    }
  }
  return [...names].flatMap((fullName) => {
    const repo = repos.get(fullName);
    if (
      repo === undefined ||
      repo.installation_missing === true ||
      repo.installation_id !== installationId
    ) {
      return [];
    }
    return Object.entries(repo.pulls ?? {}).flatMap(([number, pull]) => {
      const mergedAt = mergedAtOf(pull);
      if (pull.searched !== true || mergedAt === undefined) return [];
      const merged = Date.parse(mergedAt);
      if (!(merged >= from && merged <= to)) return [];
      if (authors.length > 0 && !authors.includes(authorOf(pull))) return [];
      return [
        {
          author: actorOf(authorOf(pull)),
          createdAt: createdAtOf(pull) ?? null,
          labels: { nodes: (pull.labels ?? []).map((name) => ({ name })) },
          mergedAt,
          number: Number(number),
          title: pull.title,
          url: `https://github.com/${fullName}/pull/${number}`,
        },
      ];
    });
  });
}

/** A pull request as GitHub's GraphQL API answers `pullRequest(number:)`, with every field the
 *  facts query of the reconcile reads, and the first `first` messages of its commits. */
function pullNode(fullName: string, number: number, pull: FakePull, first: number): unknown {
  const messages = messagesOf(pull);
  return {
    baseRefName: pull.base_ref ?? "main",
    body: pull.body ?? "",
    commits: {
      nodes: messages.slice(0, first).map((message) => ({ commit: { message } })),
      totalCount: messages.length,
    },
    headRefName: pull.head_ref ?? `branch-${number}`,
    headRefOid: pull.head_sha,
    headRepository: { nameWithOwner: pull.head_repo ?? fullName },
    mergeCommit: pull.merge_commit_sha === undefined ? null : { oid: pull.merge_commit_sha },
    merged: pull.merged ?? false,
    mergedAt: mergedAtOf(pull) ?? null,
    number,
    state: pull.merged === true ? "MERGED" : pull.state === "open" ? "OPEN" : "CLOSED",
    title: pull.title,
    updatedAt: pull.updated_at ?? null,
    url: `https://github.com/${fullName}/pull/${number}`,
  };
}

/** One `repository(owner:, name:)` field of a GraphQL query that asks for pull requests by
 *  number: the name its answer goes under (its alias, or `repository`), the repository, whether
 *  it asks for `pullRequests { totalCount }`, and each `pullRequest(number:)` field in it. */
interface AskedRepository {
  readonly alias: string;
  readonly counted: boolean;
  readonly fullName: string;
  readonly pulls: readonly { readonly alias: string; readonly number: number }[];
}

/** The arguments of one field, by name, from the text between its parentheses: each value is a
 *  variable (`$o0`), which `variables` gives, or a string or a number written in the query. */
function argumentsOf(text: string, variables: Record<string, unknown>): Record<string, unknown> {
  const values: Record<string, unknown> = {};
  for (const [, name = "", value = ""] of text.matchAll(
    /(\w+)\s*:\s*(\$\w+|"(?:[^"\\]|\\.)*"|-?\d+)/g
  )) {
    values[name] = value.startsWith("$") ? variables[value.slice(1)] : JSON.parse(value);
  }
  return values;
}

/** The text inside the braces that open at `open`, the index of a `{`. */
function selectionAt(query: string, open: number): string {
  let depth = 0;
  for (let at = open; at < query.length; at++) {
    if (query[at] === "{") depth++;
    if (query[at] === "}" && --depth === 0) return query.slice(open + 1, at);
  }
  return query.slice(open + 1);
}

/** Each repository field of `query` that holds a `pullRequest(number:)` field, in the order the
 *  query names them. A query with none, such as a search, answers an empty list. */
function askedRepositories(query: string, variables: Record<string, unknown>): AskedRepository[] {
  const asked: AskedRepository[] = [];
  for (const field of query.matchAll(/(?:(\w+)\s*:\s*)?\brepository\s*\(([^)]*)\)\s*\{/g)) {
    const selection = selectionAt(query, (field.index ?? 0) + field[0].length - 1);
    const pulls = [...selection.matchAll(/(?:(\w+)\s*:\s*)?\bpullRequest\s*\(([^)]*)\)/g)].map(
      (pull) => ({
        alias: pull[1] ?? "pullRequest",
        number: Number(argumentsOf(pull[2] ?? "", variables).number),
      })
    );
    if (pulls.length === 0) continue;
    const { name, owner } = argumentsOf(field[2] ?? "", variables);
    asked.push({
      alias: field[1] ?? "repository",
      counted: /\bpullRequests\s*\{/.test(selection),
      fullName: `${String(owner)}/${String(name)}`,
      pulls,
    });
  }
  return asked;
}

// "owner/repo" → its installation; a missing repository answers 404 (App not
// installed).
let repos = new Map<string, FakeRepo>();

// "owner/repo" → commit sha → how many check-runs reads (one a page) the fake has answered for
// that commit since the last seed, faulted ones included. GET /__fixture/requests answers it as
// `{check_runs: {"owner/repo": {"<sha>": <count>}}}`, and every PUT /__fixture/repos and every
// DELETE /__fixture/requests empties it.
let checkRunReads = new Map<string, Map<string, number>>();

/** One repository's reads of its pull requests since the counts were last emptied, by route:
 *  `GET …/pulls/{n}`, `GET …/pulls/{n}/commits` (one a page), `GET …/commits/{sha}/pulls`, and each
 *  `pullRequest(number:)` field of a GraphQL query. A refused read counts as well. */
interface PullReads {
  commit_pulls: number;
  graphql_pulls: number;
  pull: number;
  pull_commits: number;
}

// "owner/repo", as the request names it → its PullReads. GET /__fixture/requests answers it as
// `pulls`, and DELETE /__fixture/requests empties it with the check-runs counts. A seed keeps it,
// since a case seeds the fake's answers again between the reads it counts.
let pullReads = new Map<string, PullReads>();

function countPullRead(repo: string, route: keyof PullReads, reads = 1): void {
  const counted = pullReads.get(repo) ?? {
    commit_pulls: 0,
    graphql_pulls: 0,
    pull: 0,
    pull_commits: 0,
  };
  counted[route] += reads;
  pullReads.set(repo, counted);
}

/** The repository and the route of a REST read of pull requests at `pathname`, if it is one. */
function pullReadOf(pathname: string): [string, keyof PullReads] | undefined {
  const read = pathname.match(
    /^\/repos\/([^/]+)\/([^/]+)\/(?:pulls\/\d+(\/commits)?|commits\/[^/]+\/(pulls))$/
  );
  if (read === null) return undefined;
  const route =
    read[4] !== undefined ? "commit_pulls" : read[3] !== undefined ? "pull_commits" : "pull";
  return [`${read[1]}/${read[2]}`, route];
}

/** Whether the installation `id` has spent its rate limit: a repository of it is rate_limited. */
function installationLimited(id: number): boolean {
  return [...repos.values()].some(
    (repo) => repo.installation_id === id && repo.rate_limited === true
  );
}

/** The installation an installation token of this fake was minted for (`ghs_fake_<id>`), or
 *  undefined for any other credential. */
function tokenInstallation(request: Request): number | undefined {
  const token = request.headers.get("Authorization")?.match(/^Bearer ghs_fake_(\d+)$/);
  return token === null || token === undefined ? undefined : Number(token[1]);
}

/** The headers GitHub answers a call of a spent budget with: no requests remain, and the budget
 *  comes back in one minute, so a limit Dispatch records ends within the run. */
function spentBudgetHeaders(resource: string): Record<string, string> {
  return {
    "x-ratelimit-limit": "5000",
    "x-ratelimit-remaining": "0",
    "x-ratelimit-reset": String(Math.ceil(Date.now() / 1000) + 60),
    "x-ratelimit-resource": resource,
  };
}

function countCheckRunRead(repo: string, sha: string): void {
  const commits = checkRunReads.get(repo) ?? new Map<string, number>();
  commits.set(sha, (commits.get(sha) ?? 0) + 1);
  checkRunReads.set(repo, commits);
}

/** One call the server made, as the fake log keeps it: the method and the path with each
 *  repository, number and commit written as a name (`GET /repos/{owner}/{repo}/pulls/{n}`), the
 *  repository the path names, the installation a token is minted for, and, for a GraphQL query,
 *  what it asks for: each pull request it asks by number (`acme/tools#22`), with `(absence)` where
 *  it asks the count of the repository's pull requests too, or `search` for a query that asks for
 *  no pull request by number. */
interface Call {
  readonly route: string;
  readonly repo?: string;
  readonly installation?: number;
  readonly asks?: string;
}

/** The most calls the log keeps; later ones are counted in `callsDropped` alone. */
const callLogBound = 1_000;

// Each call the server made since the counts were last emptied, in the order the fake took them.
// GET /__fixture/requests answers it as `calls`, with `calls_dropped`, and DELETE
// /__fixture/requests empties both. A seed keeps them, as it keeps the counts of pull requests.
// The fixture routes and /healthz are no calls of the server's.
let calls: Call[] = [];
let callsDropped = 0;

function logCall(call: Call): void {
  if (calls.length < callLogBound) calls.push(call);
  else callsDropped++;
}

/** The call of `method` at `pathname`, its repository, numbers and commits written as names, and
 *  the installation of a token's mint. */
function callOf(method: string, pathname: string): Call {
  const named = pathname.match(/^\/repos\/([^/]+)\/([^/]+)(\/.*)?$/);
  const path = named === null ? pathname : `/repos/{owner}/{repo}${named[3] ?? ""}`;
  const route = `${method} ${path.replace(/\/[0-9a-f]{40}(?=\/|$)/g, "/{sha}").replace(/\/\d+(?=\/|$)/g, "/{n}")}`;
  if (named !== null) return { repo: `${named[1]}/${named[2]}`, route };
  const minted = pathname.match(/^\/app\/installations\/(\d+)\/access_tokens$/);
  return minted === null ? { route } : { installation: Number(minted[1]), route };
}

// FAKE_GITHUB_PORT, or with it unset port 0, so the OS chooses; the line at the end reports the
// port bound, which e2e/harness-start.ts reads.
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts().fakeGithub.port,
  async fetch(request, server) {
    const url = new URL(request.url);
    if (request.method === "PUT" && url.pathname === "/__fixture/repos") {
      const seeded = (await request.json()) as Record<string, FakeRepo>;
      repos = new Map(Object.entries(seeded));
      checkRunReads = new Map();
      return Response.json({ ok: true });
    }

    // The repositories as the last seed gave them, for a driver of another process that adds to
    // them: a seed replaces every repository.
    if (request.method === "GET" && url.pathname === "/__fixture/repos") {
      return Response.json(Object.fromEntries(repos));
    }

    if (request.method === "GET" && url.pathname === "/__fixture/requests") {
      return Response.json({
        calls,
        calls_dropped: callsDropped,
        check_runs: Object.fromEntries(
          [...checkRunReads].map(([repo, commits]) => [repo, Object.fromEntries(commits)])
        ),
        pulls: Object.fromEntries(pullReads),
      });
    }

    if (request.method === "DELETE" && url.pathname === "/__fixture/requests") {
      checkRunReads = new Map();
      pullReads = new Map();
      calls = [];
      callsDropped = 0;
      return Response.json({ ok: true });
    }

    if (url.pathname === "/healthz") return Response.json({ ok: true });
    // A GraphQL query is logged where its body is read, below, with what it asks for.
    if (!(request.method === "POST" && url.pathname === "/graphql")) {
      logCall(callOf(request.method, url.pathname));
    }

    const installation = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/installation$/);
    if (request.method === "GET" && installation !== null) {
      const repo = repos.get(`${installation[1]}/${installation[2]}`);
      // A repository nobody seeded, and one seeded as not installed on: both are GitHub's 404,
      // the answer the Go client reads as ErrNoInstallation.
      if (repo === undefined || repo.installation_missing === true) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({
        app_slug: "dispatch-e2e",
        id: repo.installation_id,
        permissions: permissionsOf(repo),
      });
    }

    if (
      request.method === "POST" &&
      /^\/app\/installations\/\d+\/access_tokens$/.test(url.pathname)
    ) {
      return Response.json(
        {
          expires_at: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
          token: `ghs_fake_${url.pathname.split("/")[3]}`,
        },
        { status: 201 }
      );
    }

    if (request.method === "GET" && url.pathname === "/app/installations") {
      const installations = new Map<number, FakeRepo>();
      for (const repo of repos.values()) installations.set(repo.installation_id, repo);
      return Response.json(
        [...installations.values()].map((repo) => ({
          account: { login: "acme" },
          app_slug: "dispatch-e2e",
          id: repo.installation_id,
          permissions: permissionsOf(repo),
        }))
      );
    }

    if (request.method === "GET" && url.pathname === "/installation/repositories") {
      const installationId = Number(
        request.headers.get("Authorization")?.replace("Bearer ghs_fake_", "")
      );
      return Response.json({
        repositories: [...repos]
          .filter(([, repo]) => repo.installation_id === installationId)
          .map(([fullName]) => ({ full_name: fullName })),
      });
    }

    // A read of pull requests counts before it is answered, whatever the answer is. A REST call of
    // a repository whose installation has spent its rate limit is GitHub's 403 with no requests
    // remaining, which the Go client reads as a rate limit (githubapp.RateLimit). The installation
    // lookup above is the App's own call, and answers as before.
    const pullRead = request.method === "GET" ? pullReadOf(url.pathname) : undefined;
    if (pullRead !== undefined) countPullRead(...pullRead);
    const repositoryCall = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)(?:\/|$)/);
    const calledRepo =
      repositoryCall === null ? undefined : repos.get(`${repositoryCall[1]}/${repositoryCall[2]}`);
    if (calledRepo !== undefined && installationLimited(calledRepo.installation_id)) {
      return Response.json(
        { message: `API rate limit exceeded for installation ID ${calledRepo.installation_id}.` },
        { headers: spentBudgetHeaders("core"), status: 403 }
      );
    }

    // GitHub's GraphQL API. A query of pull requests by number (the reconcile's facts query, and
    // its proof of absence) answers from the seeded pull requests. A search answers the nodes of
    // `searchNodes`, in pages of the size that its `first` argument gives. Every other query
    // answers an empty search. A repository the token's installation does not hold
    // answers null with NOT_FOUND on its own path, and a number the repository does not hold
    // answers null with NOT_FOUND on the path of its alias, as GitHub answers both. A query made
    // with the token of an installation that has spent its rate limit answers RATE_LIMITED and
    // no data.
    if (request.method === "POST" && url.pathname === "/graphql") {
      const { query = "", variables = {} } = (await request.json()) as {
        query?: string;
        variables?: Record<string, unknown>;
      };
      const asked = askedRepositories(query, variables);
      for (const repository of asked) {
        countPullRead(repository.fullName, "graphql_pulls", repository.pulls.length);
      }
      logCall({
        asks:
          asked.length === 0
            ? "search"
            : asked
                .flatMap((repository) =>
                  repository.pulls.map(
                    (pull) =>
                      `${repository.fullName}#${pull.number}${repository.counted ? " (absence)" : ""}`
                  )
                )
                .join(", "),
        route: "POST /graphql",
      });
      const installationId = tokenInstallation(request);
      if (installationId !== undefined && installationLimited(installationId)) {
        return Response.json(
          {
            data: null,
            errors: [
              {
                message: `API rate limit already exceeded for installation ID ${installationId}.`,
                type: "RATE_LIMITED",
              },
            ],
          },
          { headers: spentBudgetHeaders("graphql") }
        );
      }
      if (asked.length === 0) {
        const search = query.match(/\bsearch\s*\(([^)]*)\)/)?.[1] ?? "";
        const { after, first, query: q } = argumentsOf(search, variables);
        const found = typeof q === "string" ? searchNodes(q, installationId) : [];
        const offset = typeof after === "string" ? Number(after.replace(/^cursor:/, "")) : 0;
        const nodes = found.slice(offset, offset + (typeof first === "number" ? first : 100));
        return Response.json({
          data: {
            search: {
              issueCount: found.length,
              nodes,
              pageInfo: {
                endCursor: nodes.length === 0 ? "" : `cursor:${offset + nodes.length}`,
                hasNextPage: offset + nodes.length < found.length,
              },
            },
          },
        });
      }
      const first = Number(query.match(/\bcommits\s*\(\s*first\s*:\s*(\d+)/)?.[1] ?? 100);
      const data: Record<string, unknown> = {};
      const errors: { message: string; path: string[]; type: string }[] = [];
      for (const repository of asked) {
        const seeded = repos.get(repository.fullName);
        if (
          seeded === undefined ||
          seeded.installation_missing === true ||
          seeded.installation_id !== installationId
        ) {
          data[repository.alias] = null;
          errors.push({
            message: `Could not resolve to a Repository with the name '${repository.fullName}'.`,
            path: [repository.alias],
            type: "NOT_FOUND",
          });
          continue;
        }
        const answer: Record<string, unknown> = {};
        if (repository.counted) {
          answer.pullRequests = { totalCount: Object.keys(seeded.pulls ?? {}).length };
        }
        for (const pull of repository.pulls) {
          const found = seeded.pulls?.[String(pull.number)];
          answer[pull.alias] =
            found === undefined ? null : pullNode(repository.fullName, pull.number, found, first);
          if (found === undefined) {
            errors.push({
              message: `Could not resolve to a PullRequest with the number of ${pull.number}.`,
              path: [repository.alias, pull.alias],
              type: "NOT_FOUND",
            });
          }
        }
        data[repository.alias] = answer;
      }
      return Response.json(errors.length === 0 ? { data } : { data, errors });
    }

    const workflowRuns = url.pathname.match(
      /^\/repos\/([^/]+)\/([^/]+)\/actions\/workflows\/[^/]+\/runs$/
    );
    if (request.method === "GET" && workflowRuns !== null) {
      if (!repos.has(`${workflowRuns[1]}/${workflowRuns[2]}`)) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({ total_count: 0, workflow_runs: [] });
    }

    // A commit's workflow runs (`actions/runs?head_sha=`), which the GitHub link joins to the
    // check runs by check suite for each run's workflow name and event.
    const commitWorkflowRuns = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/actions\/runs$/);
    if (request.method === "GET" && commitWorkflowRuns !== null) {
      const repo = repos.get(`${commitWorkflowRuns[1]}/${commitWorkflowRuns[2]}`);
      if (repo === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      const headSHA = url.searchParams.get("head_sha") ?? "";
      const runs = repo.workflow_runs?.[headSHA] ?? [];
      return Response.json({
        total_count: runs.length,
        workflow_runs: pageOf(runs, url).map((run) => ({ ...run, head_sha: headSHA })),
      });
    }

    // A seeded pull request answers as GitHub does. A stored pull request's attribution read is
    // skipped on a server error, leaving the row as seeded, so an unseeded one answers 500: a 404
    // would mark it unfetchable and change the delivery data under the test. A pull request that
    // `completes` selects also answers the fields that the completion of a partial row reads. Its
    // line counts are always 3 added lines and 1 deleted line.
    const pull = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/pulls\/(\d+)$/);
    if (request.method === "GET" && pull !== null) {
      const seeded = repos.get(`${pull[1]}/${pull[2]}`)?.pulls?.[pull[3] ?? ""];
      if (seeded === undefined) {
        return Response.json({ message: "fixture has no pull request" }, { status: 500 });
      }
      return Response.json({
        ...(completes(seeded)
          ? {
              additions: 3,
              created_at: createdAtOf(seeded) ?? null,
              deletions: 1,
              labels: (seeded.labels ?? []).map((name) => ({ name })),
              merged_at: mergedAtOf(seeded) ?? null,
              user: { login: authorOf(seeded) },
            }
          : {}),
        base: { ref: seeded.base_ref ?? "main" },
        body: seeded.body ?? "",
        head: {
          ref: seeded.head_ref ?? `branch-${pull[3]}`,
          repo: { full_name: seeded.head_repo ?? `${pull[1]}/${pull[2]}` },
          sha: seeded.head_sha,
        },
        html_url: `https://github.com/${pull[1]}/${pull[2]}/pull/${pull[3]}`,
        merge_commit_sha: seeded.merge_commit_sha ?? null,
        merged: seeded.merged ?? false,
        number: Number(pull[3]),
        state: seeded.state,
        title: seeded.title,
        updated_at: seeded.updated_at ?? null,
      });
    }

    // A seeded pull request's commits, paged as GitHub pages them (`pageOf`): each one's sha and
    // message, oldest first, the last at the head. `commits_status` answers that status instead,
    // with GitHub's error body. An unseeded pull request answers 500, as its own read does. Each
    // commit of a pull request that `completes` selects also has the dates of `commitDatesOf`.
    const pullCommits = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/pulls\/(\d+)\/commits$/);
    if (request.method === "GET" && pullCommits !== null) {
      const fullName = `${pullCommits[1]}/${pullCommits[2]}`;
      const number = pullCommits[3] ?? "";
      const seeded = repos.get(fullName)?.pulls?.[number];
      if (seeded === undefined) {
        return Response.json({ message: "fixture has no pull request" }, { status: 500 });
      }
      if (seeded.commits_status !== undefined) {
        return Response.json({ message: "Server Error" }, { status: seeded.commits_status });
      }
      return Response.json(
        pageOf(
          messagesOf(seeded).map((message, index) => ({
            commit: { message, ...commitDatesOf(seeded) },
            sha: commitShaOf(fullName, number, seeded, index),
          })),
          url
        )
      );
    }

    const checkRuns = url.pathname.match(
      /^\/repos\/([^/]+)\/([^/]+)\/commits\/([^/]+)\/check-runs$/
    );
    if (request.method === "GET" && checkRuns !== null) {
      const fullName = `${checkRuns[1]}/${checkRuns[2]}`;
      const sha = checkRuns[3] ?? "";
      const repo = repos.get(fullName);
      if (repo === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      countCheckRunRead(fullName, sha);
      const fault = repo.check_run_faults?.[sha];
      if (fault !== undefined && "status" in fault) {
        return Response.json({ message: "Server Error" }, { status: fault.status });
      }
      if (fault !== undefined) {
        // Bun.serve closes a connection idle for 10 s, as long as Dispatch's own read deadline:
        // a held request lifts that bound, so Dispatch's deadline is what ends the wait.
        server.timeout(request, 0);
        await Bun.sleep(fault.hold_ms);
      }
      const runs = repo.check_runs?.[sha] ?? [];
      return Response.json({
        check_runs: pageOf(runs, url).map(
          ({ app_id, check_suite_id, completed_at, html_url, output, ...run }) => ({
            ...run,
            app: { id: app_id ?? 15368, slug: "github-actions" },
            check_suite: { id: check_suite_id },
            completed_at: completed_at ?? null,
            html_url: html_url ?? `https://github.com/${fullName}/runs/${run.id}`,
            output: output ?? { summary: null, title: null },
          })
        ),
        total_count: runs.length,
      });
    }

    // The pull requests GitHub names for a commit, which Dispatch reads through when a result
    // names a commit that is the head of no pull request it has an event for. It must match
    // before the bare `commits/{ref}` route below.
    const commitPulls = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/commits\/([^/]+)\/pulls$/);
    if (request.method === "GET" && commitPulls !== null) {
      const repo = repos.get(`${commitPulls[1]}/${commitPulls[2]}`);
      if (repo === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      const numbers = repo.commit_pulls?.[commitPulls[3] ?? ""] ?? [];
      return Response.json(
        numbers.flatMap((number) => {
          const seeded = repo.pulls?.[String(number)];
          if (seeded === undefined) return [];
          return [
            {
              base: { ref: seeded.base_ref ?? "main" },
              body: seeded.body ?? "",
              head: {
                ref: seeded.head_ref ?? `branch-${number}`,
                repo: { full_name: seeded.head_repo ?? `${commitPulls[1]}/${commitPulls[2]}` },
                sha: seeded.head_sha,
              },
              html_url: `https://github.com/${commitPulls[1]}/${commitPulls[2]}/pull/${number}`,
              merged: seeded.merged ?? false,
              number,
              state: seeded.state,
              title: seeded.title,
              updated_at: seeded.updated_at ?? "2026-01-01T00:00:00Z",
            },
          ];
        })
      );
    }

    // `compare/{base}...{head}`, of which Dispatch decodes only `status`: a branch that holds
    // the commit is `behind` it (`identical` when it is the branch's last commit), and one that
    // does not is `diverged`.
    const compare = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/compare\/(.+)$/);
    if (request.method === "GET" && compare !== null) {
      const repo = repos.get(`${compare[1]}/${compare[2]}`);
      const [base, head] = decodeURIComponent(compare[3] ?? "").split("...", 2);
      if (repo === undefined || base === undefined || head === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      const commits = repo.branch_commits?.[base] ?? [];
      const held = commits.indexOf(head);
      return Response.json({
        status: held === -1 ? "diverged" : held === commits.length - 1 ? "identical" : "behind",
      });
    }

    // The exact branch read a source's save makes (`git/ref/heads/{branch}`, a `/` in the name
    // percent-encoded by the Go client): a branch the repository was seeded with answers its ref
    // at the commit the commits read answers, and any other name 404, as GitHub does.
    const branchRef = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/git\/ref\/heads\/([^/]+)$/);
    if (request.method === "GET" && branchRef !== null) {
      const repo = repos.get(`${branchRef[1]}/${branchRef[2]}`);
      const branch = decodeURIComponent(branchRef[3] ?? "");
      if (repo === undefined || !(repo.branches ?? ["main"]).includes(branch)) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({
        object: { sha: commitFor(repo), type: "commit" },
        ref: `refs/heads/${branch}`,
      });
    }

    // As GitHub does: the bare sha for `Accept: application/vnd.github.sha` (what the Go
    // client's Ref asks for), the commit's JSON otherwise, and `422 No commit found for SHA`
    // for a commit the repository does not hold — the one answer HasCommit reads as absence.
    // Only a 40-hex ref can be refused: any other ref is a commit-ish, which a branch read asks
    // this route for, and answers the repository's head as it always has.
    const commit = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/commits\/([^/]+)$/);
    if (request.method === "GET" && commit !== null) {
      const repo = repos.get(`${commit[1]}/${commit[2]}`);
      if (repo === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      const ref = decodeURIComponent(commit[3] ?? "");
      const isSHA = /^[0-9a-f]{40}$/.test(ref);
      if (isSHA && !heldCommits(repo).has(ref)) {
        return Response.json({ message: `No commit found for SHA: ${ref}` }, { status: 422 });
      }
      const sha = isSHA ? ref : commitFor(repo);
      if (request.headers.get("Accept") === "application/vnd.github.sha") {
        return new Response(sha, { headers: { "Content-Type": "application/vnd.github.sha" } });
      }
      return Response.json({ sha });
    }

    // The architecture directory's subtree at a commit
    // (`git/trees/{sha}:.dispatch/architecture`, the ":" and "/" percent-encoded
    // by the Go client); a repository without the directory is GitHub's 404.
    const tree = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/git\/trees\/([^/]+)$/);
    if (request.method === "GET" && tree !== null) {
      const repo = repos.get(`${tree[1]}/${tree[2]}`);
      const dir = decodeURIComponent(tree[3] ?? "").split(":", 2)[1];
      if (repo === undefined || dir !== ARCHITECTURE_DIR) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      const files = repo.files ?? {};
      return Response.json({
        sha: `tree-${commitFor(repo)}`,
        truncated: false,
        tree: Object.keys(files).map((name) => ({
          path: name,
          mode: "100644",
          type: "blob",
          sha: `blob-${name}`,
          size: Buffer.byteLength(files[name] ?? ""),
        })),
      });
    }

    const blob = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/git\/blobs\/blob-(.+)$/);
    if (request.method === "GET" && blob !== null) {
      const content = repos.get(`${blob[1]}/${blob[2]}`)?.files?.[blob[3] ?? ""];
      if (content === undefined) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({
        content: Buffer.from(content).toString("base64"),
        encoding: "base64",
      });
    }

    const repository = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)$/);
    if (request.method === "GET" && repository !== null) {
      if (!request.headers.get("Authorization")?.startsWith("Bearer ghs_fake_")) {
        return Response.json({ message: "Requires authentication" }, { status: 401 });
      }
      if (!repos.has(`${repository[1]}/${repository[2]}`)) {
        return Response.json({ message: "Not Found" }, { status: 404 });
      }
      return Response.json({
        default_branch: repos.get(`${repository[1]}/${repository[2]}`)?.default_branch ?? "main",
        full_name: `${repository[1]}/${repository[2]}`,
      });
    }

    return new Response("not found", { status: 404 });
  },
});

console.log(`fake github listener on 127.0.0.1:${server.port}`);
