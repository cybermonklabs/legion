// The routes of GitHub's REST API a run's Dispatch server can ask for, and the sweep that finds
// where Dispatch's code builds each. The run's GitHub stand-in (github-standin.ts) answers the
// routes the world's flows were measured to ask for (GITHUB_ROUTES), each with the Dispatch code
// path that asks for it and the flow that did; every other place Dispatch's code builds a GitHub
// request is named in UNREACHED with why no world reaches it. github-standin.test.ts holds the
// two to the code: every site `requestSites` finds is a row of one of the tables, and every row
// names a site that is there, so a read Dispatch adds later fails that pin rather than being
// refused quietly in a batch, and a row whose code is gone fails it too.
//
// Measured on 2026-11-16 (INFRA-764) on rig.sh's own flows with the agents stubbed: the seeds of
// tester-proof, reviewer-text-findings (text-only, defect) and threads-closed (code-defect, and
// the case `correct` replaced, whose seeds accept and correct make as it made them), `dispatch
// results` at the pull request's head and one push below it, and the delivery reconcile forced
// (DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL=2s) over a head row aged past its stale window;
// ask-on-message, measure-before-ask, brainstorm and askgate (facts) asked GitHub nothing. Since
// main's #4681 every world's Dispatch also makes the GitHub rate reading's reads at its start,
// whatever the scenario (RATE_READING): the four boot runs of the batch at rig 3abb0726 were
// refused the first of them, its list of the App's installations, on 2026-11-28.
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";

/** One route the stand-in answers: its method and path template, the media type where GitHub
 * answers the path in more than one and the world asked for one, the credential GitHub takes for
 * it (the App's JWT or an installation token), the status GitHub answers it with when it succeeds
 * (measured on 2026-11-16 at 16:29Z: 200 on every read, 201 on the token's mint), the place in
 * Dispatch's code that builds it (`site`, a file under packages/envoy and its function), the path
 * through Dispatch that asks for it, and the flow it was measured in. */
export interface GithubRoute {
  readonly route: string;
  readonly accept?: string;
  readonly credential: "app" | "installation";
  readonly status: 200 | 201;
  readonly site: string;
  readonly asked: string;
  readonly measured: string;
}

/** The route of GITHUB_ROUTES that a request of `method` to `requestPath` (its query string
 * dropped) asking for the media type `accept` is, with the path's parameters (`templatePattern`'s
 * groups): its method, its path's template, and its media type where the route names one.
 * Undefined for any other request. The stand-in answers a request by it and health's rule 18 reads
 * each line of the stand-in's log by it, so the two match a request one way. */
export function githubRouteOf(
  method: string,
  requestPath: string,
  accept: string
): { route: GithubRoute; params: Record<string, string> } | undefined {
  const bare = requestPath.split("?")[0] ?? "";
  for (const route of GITHUB_ROUTES) {
    if (route.route.split(" ")[0] !== method) continue;
    if (route.accept !== undefined && route.accept !== accept) continue;
    const match = templatePattern(route.route).exec(bare);
    if (match !== null) return { route, params: match.groups ?? {} };
  }
  return undefined;
}

/** Dispatch's GitHub rate reading (packages/envoy/cmd/dispatch/ratereading.go, main's #4681): a
 * pass at the server's start and every ten minutes after (githubrate.Interval), wherever the
 * server holds the App's private key, as every world's does (run-server.sh), that lists the App's
 * installations and reads two counters of each. */
const RATE_READING =
  "Dispatch's GitHub rate reading (cmd/dispatch/ratereading.go, main's #4681), a pass at the server's start and every ten minutes";
/** Where the rate reading's reads were measured. */
const RATE_MEASURED =
  "every world's Dispatch at its start, whatever the scenario: run 96a307652764 (rig 3abb0726, head label 2ee75548, 2026-11-28T18:37:04Z) was refused the list 501, and github-standin.test.ts's world rows run the checkout's own Dispatch to the pass's end";

export const GITHUB_ROUTES: readonly GithubRoute[] = [
  {
    route: "GET /repos/{owner}/{repo}/installation",
    credential: "app",
    status: 200,
    site: "internal/dispatch/githubapp/client.go installation",
    asked:
      "githubapp.Client.RepositoryToken, which mints the installation token of every read of the repository below (delivery/branch.go repositoryRead)",
    measured:
      "a worker world's seeds, at the first result they record below the pull request's stored head (threads-closed's accept, correct and crossing); in every other worker world, at the first repository read its Dispatch makes (`dispatch results` one push below the head, or the reconcile's re-read of a stale head)",
  },
  {
    route: "POST /app/installations/{id}/access_tokens",
    credential: "app",
    status: 201,
    site: "internal/dispatch/githubapp/client.go token",
    asked:
      "githubapp.Client.RepositoryToken → RepositoryTokenWithStatuses, once a token's life (an hour); since #4299 the request is built in token, which Token calls too, as the rate reading does for each installation its list names (ratereading.go read)",
    measured:
      "a worker world's seeds, at the first result they record below the pull request's stored head (threads-closed's accept, correct and crossing); in every other worker world, at the first repository read its Dispatch makes (`dispatch results` one push below the head, or the reconcile's re-read of a stale head); and, since #4681, every world's Dispatch at its start, for the rate reading",
  },
  {
    route: "GET /repos/{owner}/{repo}/commits/{sha}",
    accept: "application/vnd.github.sha",
    credential: "installation",
    status: 200,
    site: "internal/dispatch/githubapp/commits.go HasCommit",
    asked:
      "api/results_linked.go refuseUnknownCommit → delivery.RepositoryHoldsCommit (commit_held.go): a result is refused at a commit the repository does not hold; one at an open or merged head Dispatch stores for the issue is answered from that head and asks nothing (HeadsAttributedTo, since #5416)",
    measured:
      "each result a worker world's seeds record below the pull request's stored head (implement-handoff and tester-result in threads-closed's accept, correct and crossing); measured asking nothing at a seeded result at that head (run 7ca197407eba, 2026-11-27)",
  },
  {
    route: "GET /repos/{owner}/{repo}",
    credential: "installation",
    status: 200,
    site: "internal/dispatch/githubapp/commits.go DefaultBranch",
    asked:
      "api/results_comparison.go defaultBranchOf → delivery.Branches.DefaultBranchOf (branch.go branchOf → repositoryDefaultBranch): whether Dispatch can read the repository, for a result at none of the compared commits (nothingReason, commitStands)",
    measured: "`dispatch results` of each worker world whose results stand one push below the head",
  },
  {
    route: "GET /repos/{owner}/{repo}/commits/{sha}/pulls",
    credential: "installation",
    status: 200,
    site: "internal/dispatch/delivery/github_prs.go fetchCommitPullPayloads",
    asked:
      "api/results_comparison.go learn → readThroughHead → delivery.ReadThroughCommitPulls (pull_request_heads.go): the pull requests of a result's commit that heads none Dispatch holds",
    measured: "`dispatch results` of each worker world whose results stand one push below the head",
  },
  {
    route: "GET /repos/{owner}/{repo}/pulls/{number}",
    credential: "installation",
    status: 200,
    site: "internal/dispatch/delivery/github_prs.go fetchPullRequestPayload",
    asked:
      "delivery/reconcile.go runOnce → reconcilePullRequestHeads → RefreshPullRequestHead (pull_request_heads.go): every pass, five minutes apart, re-reads an open head unread for five minutes (PullRequestHeadStaleAfter), so a run past ten minutes reaches it",
    measured:
      "a worker world's head row aged past the stale window, under DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL=2s",
  },
  {
    route: "GET /app/installations",
    credential: "app",
    status: 200,
    site: "internal/dispatch/githubapp/client.go listInstallations",
    asked: `${RATE_READING}: the pass lists the App's installations first, every page of 100 (ratereading.go pass → githubapp.Client.ReadInstallations)`,
    measured: RATE_MEASURED,
  },
  {
    route: "GET /app",
    credential: "app",
    status: 200,
    site: "internal/dispatch/githubapp/client.go AppIdentity",
    asked: `${RATE_READING}: the App's id and slug (ratereading.go app → githubapp.Client.AppIdentity), read only where the list named neither, as a refused or empty list does; a world's list names both`,
    measured:
      "run 96a307652764 (rig 3abb0726, 2026-11-28T18:37:04Z), once the stand-in had refused the list",
  },
  {
    route: "GET /installation/repositories",
    credential: "installation",
    status: 200,
    site: "cmd/dispatch/ratereading.go repository",
    asked: `${RATE_READING}: the first repository an installation lists, per_page=1, whose answer its core counter is read from, read once and remembered (ratereading.go read → core → repository)`,
    measured: RATE_MEASURED,
  },
  {
    route: "GET /repos/{owner}/{repo}/actions/workflows",
    credential: "installation",
    status: 200,
    site: "cmd/dispatch/ratereading.go core",
    asked: `${RATE_READING}: the core counter, read off the rate limit headers of the first repository's workflows, per_page=1 (ratereading.go core, githubrate.CoreBucket)`,
    measured: RATE_MEASURED,
  },
  {
    route: "GET /rate_limit",
    credential: "installation",
    status: 200,
    site: "cmd/dispatch/ratereading.go read",
    asked: `${RATE_READING}: the GraphQL counter, its answer's resources.graphql (ratereading.go read, githubrate.GraphQLBucket); the read costs no core call`,
    measured: RATE_MEASURED,
  },
];

/** A place Dispatch's code builds a GitHub request that no world reaches, and why. */
export interface UnreachedSite {
  readonly site: string;
  readonly route: string;
  readonly reason: string;
}

/** Why the delivery reconcile's steps after its head pass ask nothing: the world configures no
 * delivery, and the pass stops at that (reconcile.go runOnce), as every measured world's server
 * logged ("delivery is not configured"). */
const NO_DELIVERY =
  "the delivery reconcile's steps after its head pass and the delivery intake: no world configures delivery (runOnce stops at `delivery is not configured`, logged by every measured world's server) and no webhook reaches a world's server, whose intake is idle with NATS off";
/** Why the CI reconcile asks nothing: it needs NATS, which run-server.sh disables. */
const NO_CI_RECONCILE =
  "the CI reconcile (cireconcile), off in a world's server: it needs NATS, which run-server.sh disables (`CI reconcile off`, logged by every measured world's server)";
/** Why the webhook redelivery sweep asks nothing: the same. */
const NO_REDELIVERY =
  "the webhook redelivery sweep (redeliver), off in a world's server: it needs NATS (`webhook redelivery off`, logged by every measured world's server)";
/** Why the architecture importer and the source checks ask nothing. */
const NO_ARCHITECTURE =
  "an architecture source's import and check (architecture/importer.go, api/architecture_source.go), which a person's write of a source in the dashboard starts; no world writes one";
/** Why a spec check's result asks nothing: a CI job's results token is the one credential that
 * records it, and no world mints one. */
const NO_CI_RESULTS =
  "a spec check's result a CI job records (api/spec_results.go recordSpecCheckResult, main's #4184), which refuses every credential but a CI job's results token (`only a CI job's results token writes the result of a spec check`); no world mints one";

export const UNREACHED: readonly UnreachedSite[] = [
  {
    site: "internal/dispatch/githubapp/commits.go repoPath",
    route: "-",
    reason: "the path helper the repository reads build their path with; it asks nothing itself",
  },
  {
    site: "internal/dispatch/githubapp/client.go listInstallationRepositories",
    route: "GET /installation/repositories",
    reason: NO_DELIVERY,
  },
  {
    site: "internal/dispatch/githubapp/client.go GraphQL",
    route: "POST /graphql",
    reason: NO_CI_RECONCILE,
  },
  {
    site: "internal/dispatch/githubapp/client.go CheckSource",
    route: "GET /repos/{owner}/{repo}",
    reason: `${NO_ARCHITECTURE}, and the delivery settings' check (api/settings_delivery.go), which a person's write of delivery settings starts`,
  },
  {
    site: "internal/dispatch/githubapp/client.go Ref",
    route: "GET /repos/{owner}/{repo}/commits/{branch}",
    reason: NO_ARCHITECTURE,
  },
  {
    site: "internal/dispatch/githubapp/client.go Branch",
    route: "GET /repos/{owner}/{repo}/branches/{branch}",
    reason: NO_ARCHITECTURE,
  },
  {
    site: "internal/dispatch/githubapp/client.go Dir",
    route: "GET /repos/{owner}/{repo}/contents/{dir}?ref={sha}",
    reason: NO_ARCHITECTURE,
  },
  {
    site: "internal/dispatch/githubapp/client.go DirFiles",
    route: "GET /repos/{owner}/{repo}/git/blobs/{sha}",
    reason: NO_ARCHITECTURE,
  },
  {
    site: "internal/dispatch/githubapp/commits.go CommitOnBranch",
    route: "GET /repos/{owner}/{repo}/compare/{branch}...{sha}",
    reason:
      "an after_merge or default_branch check's walk of a branch (api/results_comparison.go onNamedBranch, delivery/branch.go CommitOnBranch); every world's check is before_merge (seed.ts tester-proof)",
  },
  {
    site: "internal/dispatch/githubapp/commits.go CommitCheckRuns",
    route: "GET /repos/{owner}/{repo}/commits/{sha}/check-runs",
    reason: `a check CI proves (api/results_ci.go, a check naming a ci_check), which no world's check is, and ${NO_CI_RECONCILE}`,
  },
  {
    site: "internal/dispatch/githubapp/commits.go CommitCheckSuites",
    route: "GET /repos/{owner}/{repo}/commits/{sha}/check-suites",
    reason: NO_CI_RECONCILE,
  },
  {
    site: "internal/dispatch/githubapp/commits.go CommitStatuses",
    route: "GET /repos/{owner}/{repo}/commits/{sha}/status",
    reason: `the commit statuses of a head the CI reconcile settles (cireconcile/heads.go, #4299), and ${NO_CI_RECONCILE}`,
  },
  {
    site: "internal/dispatch/githubapp/commits.go OpenPullRequests",
    route: "GET /repos/{owner}/{repo}/pulls?state=open",
    reason: NO_CI_RECONCILE,
  },
  {
    site: "internal/dispatch/githubapp/commits.go RepositoryIdentity",
    route: "GET /repos/{owner}/{repo}",
    reason: NO_CI_RECONCILE,
  },
  {
    site: "internal/dispatch/githubapp/deliveries.go Deliveries",
    route: "GET /app/hook/deliveries",
    reason: NO_REDELIVERY,
  },
  {
    site: "internal/dispatch/githubapp/deliveries.go Redeliver",
    route: "POST /app/hook/deliveries/{id}/attempts",
    reason: NO_REDELIVERY,
  },
  {
    site: "internal/dispatch/githubapp/check_run.go CheckRun",
    route: "GET /repos/{owner}/{repo}/check-runs/{id}",
    reason: NO_CI_RESULTS,
  },
  {
    site: "internal/dispatch/githubapp/file.go File",
    route: "GET /repos/{owner}/{repo}/contents/{path}?ref={sha}",
    reason: NO_CI_RESULTS,
  },
  {
    site: "internal/dispatch/delivery/github_prs.go FetchPullRequest",
    route: "GET /repos/{owner}/{repo}/pulls/{number}/commits?per_page=1",
    reason: NO_DELIVERY,
  },
  {
    site: "internal/dispatch/delivery/github_runs.go fetchWorkflowRunsPage",
    route: "GET /repos/{owner}/{repo}/actions/workflows/{workflow}/runs",
    reason: NO_DELIVERY,
  },
  {
    site: "internal/dispatch/delivery/github_runs.go FetchWorkflowRun",
    route: "GET /repos/{owner}/{repo}/actions/runs/{id}",
    reason: NO_DELIVERY,
  },
  {
    site: "internal/dispatch/delivery/github_runs.go ListWorkflowRunJobs",
    route: "GET /repos/{owner}/{repo}/actions/runs/{id}/jobs",
    reason: NO_DELIVERY,
  },
  {
    site: "internal/dispatch/delivery/intake.go fetchCommitMessagesWithToken",
    route: "GET /repos/{owner}/{repo}/pulls/{number}/commits",
    reason:
      "a pull request's commit messages, which the attribution reads only where its branch and title name no issue (delivery/pull_request_heads.go attributeHead) and the delivery intake reads; every world's branch is legion/<key>, measured attributed by its branch on the reconcile's re-read, and the intake is idle",
  },
];

/** A line of Go that builds a GitHub request path: a literal of one of GitHub's REST roots, the
 * App's own read (`"/app"`), the counters' read (`"/rate_limit"`), the GraphQL endpoint, or
 * githubapp's own path helper applied to a repository. */
const REQUEST =
  /"\/repos\/|repoPath\(owner, repo\)|"\/app\/|"\/app"|%s\/app\/|"\/installation\/|"\/rate_limit"|"\/graphql"/;

/** The directories of the envoy module that hold Dispatch's code: its packages, and the
 * program's own, where the rate reading builds its reads (ratereading.go). */
const DISPATCH_SOURCES = ["internal/dispatch", "cmd/dispatch"];

/** Every place under `envoyDir` (packages/envoy) whose Go code of Dispatch (DISPATCH_SOURCES)
 * builds a GitHub request (REQUEST), as `<file> <function>`, the file by its path in the module:
 * tests and githubapptest, a test server of its own, are not Dispatch's reads; a comment is no
 * code. */
export function requestSites(envoyDir: string): string[] {
  const sites = new Set<string>();
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        if (entry.name !== "githubapptest") walk(full);
        continue;
      }
      if (!entry.name.endsWith(".go") || entry.name.endsWith("_test.go")) continue;
      let within = "";
      for (const line of readFileSync(full, "utf8").split("\n")) {
        const declared = /^func (?:\([^)]*\) )?(\w+)/.exec(line);
        if (declared !== null) within = declared[1] ?? "";
        if (/^\s*\/\//.test(line) || !REQUEST.test(line)) continue;
        sites.add(`${path.relative(envoyDir, full)} ${within}`);
      }
    }
  };
  for (const dir of DISPATCH_SOURCES) walk(path.join(envoyDir, dir));
  return [...sites].sort();
}

/** `template`'s path as a regular expression over a request's path: `{owner}`, `{repo}` and the
 * other names are one segment each, `{sha}` 40 hexadecimal digits and `{id}` and `{number}`
 * digits, each a named group. */
export function templatePattern(template: string): RegExp {
  const [, routePath = ""] = template.split(" ");
  const source = routePath
    .split(/(\{\w+\})/)
    .map((part) => {
      const name = /^\{(\w+)\}$/.exec(part)?.[1];
      if (name === undefined) return part.replace(/[.*+?^$()|[\]\\]/g, "\\$&");
      if (name === "sha") return `(?<${name}>[0-9a-f]{40})`;
      if (name === "id" || name === "number") return `(?<${name}>\\d+)`;
      return `(?<${name}>[^/]+)`;
    })
    .join("");
  return new RegExp(`^${source}$`);
}
