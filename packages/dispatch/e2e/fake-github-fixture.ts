// The fake GitHub's fixture control (fake-github.ts serves the GitHub API from it): the seed a test
// gives through PUT /__fixture/repos and reads back there, the counts of the server's reads and its
// log of calls that /__fixture/requests answers and DELETE empties, and the outage that PUT
// /__fixture/outage starts and DELETE ends. fixtureResponse answers those routes. The state is
// exported as live bindings: only this module writes it.

/** A pull request the GitHub link reads: its title, state and head commit, and the branch,
 *  base and body Dispatch's pull-request head tracking attributes it by. A seed that sets
 *  `searched`, `author`, `created_at`, `merged_at` or `labels` also gets the fields that the
 *  completion of a partial row reads (`completes`). A seed without them gets the same answers
 *  as before these fields. */
export interface FakePull {
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

/** One repository the fake answers for, as a seed gives it. */
export interface FakeRepo {
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
   *  (fake-github.ts's `pageOf`), so a commit of 101 runs answers 100 on page 1 and one on page 2. */
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
  /** Pull requests whose read GitHub refuses, by number, with the status it answers: the read of
   *  the pull request and the read of its commits answer that status with GitHub's words for it,
   *  404 `Not Found` for a number the repository does not have and 403 `Resource not accessible by
   *  integration` for a read the App's token may not make. A number not here answers as before. */
  readonly refused_pulls?: Record<string, number>;
}

// "owner/repo" → its installation; a missing repository answers 404 (App not
// installed).
export let repos = new Map<string, FakeRepo>();

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

export function countPullRead(repo: string, route: keyof PullReads, reads = 1): void {
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
export function pullReadOf(pathname: string): [string, keyof PullReads] | undefined {
  const read = pathname.match(
    /^\/repos\/([^/]+)\/([^/]+)\/(?:pulls\/\d+(\/commits)?|commits\/[^/]+\/(pulls))$/
  );
  if (read === null) return undefined;
  const route =
    read[4] !== undefined ? "commit_pulls" : read[3] !== undefined ? "pull_commits" : "pull";
  return [`${read[1]}/${read[2]}`, route];
}

export function countCheckRunRead(repo: string, sha: string): void {
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

export function logCall(call: Call): void {
  if (calls.length < callLogBound) calls.push(call);
  else callsDropped++;
}

/** The call of `method` at `pathname`, its repository, numbers and commits written as names, and
 *  the installation of a token's mint. */
export function callOf(method: string, pathname: string): Call {
  const named = pathname.match(/^\/repos\/([^/]+)\/([^/]+)(\/.*)?$/);
  const path = named === null ? pathname : `/repos/{owner}/{repo}${named[3] ?? ""}`;
  const route = `${method} ${path.replace(/\/[0-9a-f]{40}(?=\/|$)/g, "/{sha}").replace(/\/\d+(?=\/|$)/g, "/{n}")}`;
  if (named !== null) return { repo: `${named[1]}/${named[2]}`, route };
  const minted = pathname.match(/^\/app\/installations\/(\d+)\/access_tokens$/);
  return minted === null ? { route } : { installation: Number(minted[1]), route };
}

// The status GitHub answers every call with while it is down, or undefined while it answers.
// PUT /__fixture/outage {status} takes GitHub down, and DELETE /__fixture/outage brings it back: in
// between, every call of the server's, the App's own installation lookup and token mint included,
// is logged and answered that status with GitHub's error body, and nothing else is read or counted.
// The fixture routes and /healthz still answer, so a test sets what GitHub holds during an outage.
export let outage: number | undefined;

/** The answer to a fixture route of `request`, or undefined where it names none. */
export async function fixtureResponse(request: Request, url: URL): Promise<Response | undefined> {
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

  if (request.method === "PUT" && url.pathname === "/__fixture/outage") {
    const { status } = (await request.json()) as { status?: unknown };
    if (typeof status !== "number" || status < 400 || status > 599) {
      return Response.json(
        { message: "an outage needs a status from 400 to 599" },
        { status: 400 }
      );
    }
    outage = status;
    return Response.json({ outage });
  }

  if (request.method === "DELETE" && url.pathname === "/__fixture/outage") {
    outage = undefined;
    return Response.json({ outage: null });
  }
  return undefined;
}
