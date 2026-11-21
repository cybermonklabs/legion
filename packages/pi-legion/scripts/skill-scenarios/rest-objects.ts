// The objects GitHub's REST API answers with that more than one route of the world carries, built
// one way for the gh stand-in (gh-standin.ts) and for the HTTP stand-in Dispatch reads
// (github-standin.ts): an account (`userJson`), an App (`appJson`, GITHUB_ACTIONS,
// GITHUB_ADVANCED_SECURITY), a repository
// (`repositoryJson`) and a pull request (`pullRequestJson`). Each is in the shape GitHub answers
// it in — every field GitHub's description requires (rest-required.json) and every field
// production's answer holds (production-shapes.json), and no other — with values read from the
// world: its accounts (<run>/accounts.json, rig.sh's fixture_first_commit), its repository's
// settings (fixture_write's `gh api repos/<repo>` answer), its pull request as `gh pr view` reads
// it, and the run's own remote. A route that names a user, a repository or the pull request
// names it by these, so an id, a URL, a SHA or a time one answer gives is the one every other
// answer gives: INFRA-764's read search found the world's repository short of 79 fields GitHub
// requires, its pull request answered in gh's own shape (a string `id`, a `mergeable` of
// `MERGEABLE`), and every user it named a login alone (2026-11-17).
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

type Json = Record<string, unknown>;
const API = "https://api.github.com";
const WEB = "https://github.com";

const isObject = (value: unknown): value is Json =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** What `git -C <run>/remote.git <args...>` prints, trimmed, or undefined when it fails: the run's
 * own remote, read at the call. `env` is set for a git that writes an object (`mergeCommit`). */
function remote(runDir: string, args: readonly string[], env?: Record<string, string>) {
  const read = Bun.spawnSync(["git", "-C", path.join(runDir, "remote.git"), ...args], {
    env: env === undefined ? process.env : { ...process.env, ...env },
  });
  return read.exitCode === 0 ? read.stdout.toString().trim() : undefined;
}

/** `seconds` since the epoch as GitHub writes a time: UTC, to the second. */
export function githubTime(seconds: number): string {
  return githubInstant(new Date(seconds * 1000).toISOString());
}
/** The ISO time `iso` (a call's own `at`, to the millisecond) as GitHub writes a time. */
export function githubInstant(iso: string): string {
  return iso.replace(/\.\d{3}Z$/, "Z");
}

/** One GitHub account the world holds (<run>/accounts.json): its login, its id, the address the
 * commits it makes carry, its type where its login does not say it (an organization), and, for
 * an App's bot, the App it is (its id and slug). */
const Account = z.object({
  login: z.string(),
  id: z.number(),
  email: z.string().optional(),
  type: z.enum(["User", "Bot", "Organization"]).optional(),
  app: z.object({ id: z.number(), slug: z.string() }).optional(),
});
export type Account = z.infer<typeof Account>;

/** The world's accounts, none for a run that wrote no accounts.json. */
export function accountsOf(runDir: string): Account[] {
  const file = path.join(runDir, "accounts.json");
  return existsSync(file) ? z.array(Account).parse(JSON.parse(readFileSync(file, "utf8"))) : [];
}

/** The App whose installation token a pane of `role` calls GitHub with, as production's daemon
 * decides it (packages/daemon/internal/appauth's `AppRoleFor`, which rig.sh's role_identity gives
 * each role's commits too): the implement App for the implementer and the merger, the review App
 * for every other role, and none for a call whose pane names no role. GitHub shows a pending
 * review to the account that wrote it alone, so this is whom the world shows one to. */
export function roleApp(role: string | undefined): "implement" | "review" | "" {
  if (role === undefined || role === "") return "";
  return role === "implementer" || role === "merger" ? "implement" : "review";
}

/** The node id GitHub gives an account of each type, before its id. */
const NODE_PREFIX = { User: "U_kgDO", Bot: "BOT_kgDO", Organization: "O_kgDO" } as const;

/** The account of `login` as GitHub's REST API answers a user (`simple-user`, with the
 * `user_view_type` production's answers carry): the world's account of that login, or, for a
 * login the world lists no account of (a bot that left a review in one world alone), an id read
 * off the login, so every route names that account by one id. Its type is the account's, else
 * `type` (a repository's owner, which the world's settings name an organization), else a bot's
 * for a login ending `[bot]`, whose page is its App's, else a user's. */
export function userJson(login: string, accounts: readonly Account[], type?: Account["type"]) {
  const account = accounts.find((one) => one.login === login);
  const id =
    account?.id ??
    300_000_000 + Number.parseInt(createHash("sha256").update(login).digest("hex").slice(0, 7), 16);
  const kind = account?.type ?? type ?? (login.endsWith("[bot]") ? "Bot" : "User");
  const api = `${API}/users/${encodeURIComponent(login)}`;
  return {
    login,
    id,
    node_id: `${NODE_PREFIX[kind]}${id}`,
    avatar_url:
      account?.app === undefined
        ? `https://avatars.githubusercontent.com/u/${id}?v=4`
        : `https://avatars.githubusercontent.com/in/${account.app.id}?v=4`,
    gravatar_id: "",
    url: api,
    html_url: kind === "Bot" ? `${WEB}/apps/${login.replace(/\[bot\]$/, "")}` : `${WEB}/${login}`,
    followers_url: `${api}/followers`,
    following_url: `${api}/following{/other_user}`,
    gists_url: `${api}/gists{/gist_id}`,
    starred_url: `${api}/starred{/owner}{/repo}`,
    subscriptions_url: `${api}/subscriptions`,
    organizations_url: `${api}/orgs`,
    repos_url: `${api}/repos`,
    events_url: `${api}/events{/privacy}`,
    received_events_url: `${api}/received_events`,
    type: kind,
    user_view_type: "public",
    site_admin: false,
  };
}

/** The account the commit address `email` belongs to, as GitHub links a commit to one, or null
 * where the world holds no account with that address, as GitHub answers a commit no account
 * made. */
export function committerJson(email: string, accounts: readonly Account[]): Json | null {
  const account = accounts.find((one) => one.email === email);
  return account === undefined ? null : userJson(account.login, accounts);
}

/** The time the world's Apps were registered and last changed, the world's own. */
const APP_TIME = "2026-01-01T00:00:00Z";
/** The permissions the world's role Apps hold on its repository, the ones production's role App
 * answers its comments with (production-shapes.json's issue comments' `performed_via_github_app`). */
const ROLE_APP_PERMISSIONS = {
  actions: "read",
  attestations: "read",
  checks: "read",
  contents: "write",
  issues: "write",
  metadata: "read",
  packages: "read",
  pull_requests: "write",
  repository_projects: "read",
  security_events: "read",
  statuses: "read",
  workflows: "write",
};

/** The App whose bot `login` is, as GitHub's REST API answers one (`integration`): the App the
 * world's account of that bot names, owned by `owner`, the organization that registered it; or
 * null for a login that is no App's bot the world holds, as GitHub answers a comment no App made
 * (`performed_via_github_app`). */
export function appJson(login: string, owner: string, accounts: readonly Account[]): Json | null {
  const app = accounts.find((one) => one.login === login)?.app;
  if (app === undefined) return null;
  return {
    id: app.id,
    client_id: `Iv23li${createHash("sha256").update(app.slug).digest("hex").slice(0, 14)}`,
    slug: app.slug,
    node_id: `A_kwDO${app.id}`,
    owner: userJson(owner, accounts),
    name: app.slug,
    description: "",
    external_url: `${WEB}/${owner}`,
    html_url: `${WEB}/apps/${app.slug}`,
    created_at: APP_TIME,
    updated_at: APP_TIME,
    permissions: ROLE_APP_PERMISSIONS,
    events: [],
  };
}

/** The App GitHub reports a workflow's check runs as made by, GitHub Actions, as its check-runs
 * route names it (`integration`): GitHub's own App, no name of this world's. */
export const GITHUB_ACTIONS: Json = {
  id: 15368,
  client_id: "Iv1.05c79e9ad1f6bdfa",
  slug: "github-actions",
  node_id: "MDM6QXBwMTUzNjg=",
  owner: userJson("github", [{ login: "github", id: 9919, type: "Organization" }]),
  name: "GitHub Actions",
  description: "Automate your workflow from idea to production",
  external_url: "https://help.github.com/en/actions",
  html_url: `${WEB}/apps/github-actions`,
  created_at: "2018-07-30T09:30:17Z",
  updated_at: "2024-04-10T20:33:16Z",
  permissions: Object.fromEntries(
    [
      "actions",
      "administration",
      "artifact_metadata",
      "attestations",
      "checks",
      "code_quality",
      "contents",
      "copilot_requests",
      "deployments",
      "discussions",
      "drives",
      "issues",
      "merge_queues",
      "metadata",
      "models",
      "packages",
      "pages",
      "pull_requests",
      "repository_hooks",
      "repository_projects",
      "security_events",
      "statuses",
      "vulnerability_alerts",
    ].map((permission) => [permission, permission === "metadata" ? "read" : "write"])
  ),
  events: [
    "check_run",
    "check_suite",
    "create",
    "delete",
    "deployment",
    "issue_comment",
    "issues",
    "label",
    "merge_group",
    "pull_request",
    "pull_request_review",
    "pull_request_review_comment",
    "push",
    "release",
    "repository_dispatch",
    "status",
    "workflow_dispatch",
    "workflow_run",
  ],
};

/** The App GitHub reports a code-scanning tool's check run as made by, GitHub Advanced Security,
 * as its check-runs route names it: GitHub's own App, no name of this world's. Measured on
 * 2026-11-18 from a Legion issue pod with the role App token its gh gives: the `CodeQL` check run
 * of an open cybermonklabs/legion pull request's head (`gh api
 * repos/<repo>/commits/<head>/check-runs?check_name=CodeQL`) named this App, with these
 * permissions, no events and no description. */
export const GITHUB_ADVANCED_SECURITY: Json = {
  id: 57789,
  client_id: "Iv1.4643383dd86f00de",
  slug: "github-advanced-security",
  node_id: "MDM6QXBwNTc3ODk=",
  owner: userJson("github", [{ login: "github", id: 9919, type: "Organization" }]),
  name: "GitHub Advanced Security",
  description: "",
  external_url: "https://github.com/features/security",
  html_url: `${WEB}/apps/github-advanced-security`,
  created_at: "2020-03-17T21:25:09Z",
  updated_at: "2026-09-14T08:13:48Z",
  permissions: {
    actions: "read",
    checks: "write",
    contents: "read",
    copilot_requests: "write",
    metadata: "read",
    pull_requests: "read",
  },
  events: [],
};

/** The App GitHub reports a world's check run `run` as made by: the one its `app` names by id
 * (rig.sh's concluded_checks), GitHub Actions where it names none. */
export function checkRunApp(run: { readonly app?: unknown }): Json {
  const id = isObject(run.app) ? run.app.id : undefined;
  return id === GITHUB_ADVANCED_SECURITY.id ? GITHUB_ADVANCED_SECURITY : GITHUB_ACTIONS;
}

/** The reactions of the comment whose API URL is `url`, none, as GitHub answers a comment's. */
export function reactionsJson(url: string): Json {
  return {
    url: `${url}/reactions`,
    total_count: 0,
    "+1": 0,
    "-1": 0,
    laugh: 0,
    hooray: 0,
    confused: 0,
    heart: 0,
    rocket: 0,
    eyes: 0,
  };
}

/** The license GitHub reads off a repository's `LICENSE` file at its default branch: the MIT
 * license, which the world's repository carries (rig.sh's fixture_open), or null for a file that
 * is none or no MIT license. */
function licenseOf(runDir: string, branch: string): Json | null {
  const text = remote(runDir, ["cat-file", "blob", `refs/heads/${branch}:LICENSE`]);
  if (text === undefined || !text.startsWith("MIT License")) return null;
  return {
    key: "mit",
    name: "MIT License",
    spdx_id: "MIT",
    url: `${API}/licenses/mit`,
    node_id: "MDc6TGljZW5zZTEz",
  };
}

/** How much of a repository GitHub answers: its minimal form (a commit status's `repository`),
 * the form a pull request's head and base carry, and the whole of it (`GET repos/<repo>`). */
export type RepositoryForm = "minimal" | "pull" | "full";

/** The world's repository as GitHub's REST API answers it in `form`: `held`, the world's own
 * settings (rig.sh's fixture_write: its id, name, owner, visibility, default branch, language,
 * merge settings and the role App's permissions on it), with what GitHub computes from the run's
 * remote — when it was created (its default branch's first commit), when it was last pushed (the
 * remote's last push, or its newest commit), its size and its license — and GitHub's defaults for
 * a repository whose settings nobody changed. `openPulls` is the number of its open pull
 * requests, which GitHub counts among its open issues. */
export function repositoryJson(
  held: Json,
  runDir: string,
  accounts: readonly Account[],
  form: RepositoryForm,
  openPulls = 1
): Json {
  const full = String(held.full_name ?? "");
  const owner = isObject(held.owner) ? String(held.owner.login ?? "") : (full.split("/")[0] ?? "");
  const ownerType = isObject(held.owner) ? (held.owner.type as Account["type"]) : undefined;
  const branch = String(held.default_branch ?? "");
  const api = `${API}/repos/${full}`;
  const html = `${WEB}/${full}`;
  const minimal = {
    id: held.id,
    node_id: held.node_id,
    name: held.name,
    full_name: full,
    private: held.private ?? false,
    owner: userJson(owner, accounts, ownerType),
    html_url: html,
    description: held.description ?? null,
    fork: held.fork ?? false,
    url: api,
    forks_url: `${api}/forks`,
    keys_url: `${api}/keys{/key_id}`,
    collaborators_url: `${api}/collaborators{/collaborator}`,
    teams_url: `${api}/teams`,
    hooks_url: `${api}/hooks`,
    issue_events_url: `${api}/issues/events{/number}`,
    events_url: `${api}/events`,
    assignees_url: `${api}/assignees{/user}`,
    branches_url: `${api}/branches{/branch}`,
    tags_url: `${api}/tags`,
    blobs_url: `${api}/git/blobs{/sha}`,
    git_tags_url: `${api}/git/tags{/sha}`,
    git_refs_url: `${api}/git/refs{/sha}`,
    trees_url: `${api}/git/trees{/sha}`,
    statuses_url: `${api}/statuses/{sha}`,
    languages_url: `${api}/languages`,
    stargazers_url: `${api}/stargazers`,
    contributors_url: `${api}/contributors`,
    subscribers_url: `${api}/subscribers`,
    subscription_url: `${api}/subscription`,
    commits_url: `${api}/commits{/sha}`,
    git_commits_url: `${api}/git/commits{/sha}`,
    comments_url: `${api}/comments{/number}`,
    issue_comment_url: `${api}/issues/comments{/number}`,
    contents_url: `${api}/contents/{+path}`,
    compare_url: `${api}/compare/{base}...{head}`,
    merges_url: `${api}/merges`,
    archive_url: `${api}/{archive_format}{/ref}`,
    downloads_url: `${api}/downloads`,
    issues_url: `${api}/issues{/number}`,
    pulls_url: `${api}/pulls{/number}`,
    milestones_url: `${api}/milestones{/number}`,
    notifications_url: `${api}/notifications{?since,all,participating}`,
    labels_url: `${api}/labels{/name}`,
    releases_url: `${api}/releases{/id}`,
    deployments_url: `${api}/deployments`,
  };
  if (form === "minimal") return minimal;
  const root = remote(runDir, ["rev-list", "--max-parents=0", `refs/heads/${branch}`])?.split(
    "\n"
  )[0];
  const created = Number(
    root === undefined ? "" : remote(runDir, ["show", "-s", "--format=%ct", root])
  );
  const newest = Number(
    remote(runDir, [
      "for-each-ref",
      "--sort=-committerdate",
      "--count=1",
      "--format=%(committerdate:unix)",
      "refs/heads",
    ])
  );
  const log = path.join(runDir, "pushes.log");
  const lastPush = existsSync(log)
    ? Date.parse(readFileSync(log, "utf8").trim().split("\n").at(-1)?.split(" ")[0] ?? "") / 1000
    : Number.NaN;
  const pushed = Math.floor(
    Math.max(Number.isNaN(lastPush) ? 0 : lastPush, Number.isNaN(newest) ? 0 : newest)
  );
  const counted = Object.fromEntries(
    (remote(runDir, ["count-objects", "-v"]) ?? "").split("\n").map((line) => line.split(": "))
  ) as Record<string, string | undefined>;
  const size = (Number(counted.size) || 0) + (Number(counted["size-pack"]) || 0);
  const pulled = {
    ...minimal,
    archived: held.archived ?? false,
    disabled: held.disabled ?? false,
    homepage: held.homepage ?? "",
    language: held.language ?? null,
    visibility: held.visibility ?? "public",
    is_template: false,
    topics: [],
    forks_count: 0,
    forks: 0,
    stargazers_count: 0,
    watchers_count: 0,
    watchers: 0,
    open_issues_count: openPulls,
    open_issues: openPulls,
    size,
    default_branch: branch,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    has_pages: false,
    has_downloads: true,
    has_discussions: false,
    has_pull_requests: true,
    pushed_at: githubTime(pushed),
    created_at: githubTime(Number.isNaN(created) ? pushed : created),
    updated_at: githubTime(pushed),
    allow_forking: true,
    web_commit_signoff_required: false,
    pull_request_creation_policy: "all",
    license: licenseOf(runDir, branch),
    git_url: `git://github.com/${full}.git`,
    ssh_url: `git@github.com:${full}.git`,
    clone_url: `${html}.git`,
    svn_url: html,
    mirror_url: null,
  };
  if (form === "pull") return pulled;
  const organization = minimal.owner.type === "Organization" ? minimal.owner : null;
  return {
    ...pulled,
    permissions: held.permissions ?? {
      admin: false,
      maintain: false,
      push: false,
      triage: false,
      pull: true,
    },
    temp_clone_token: "",
    allow_squash_merge: held.allow_squash_merge ?? true,
    allow_merge_commit: held.allow_merge_commit ?? true,
    allow_rebase_merge: held.allow_rebase_merge ?? true,
    allow_auto_merge: held.allow_auto_merge ?? false,
    delete_branch_on_merge: held.delete_branch_on_merge ?? false,
    allow_update_branch: held.allow_update_branch ?? false,
    use_squash_pr_title_as_default: held.use_squash_pr_title_as_default ?? false,
    squash_merge_commit_message: held.squash_merge_commit_message ?? "COMMIT_MESSAGES",
    squash_merge_commit_title: held.squash_merge_commit_title ?? "COMMIT_OR_PR_TITLE",
    merge_commit_message: held.merge_commit_message ?? "PR_TITLE",
    merge_commit_title: held.merge_commit_title ?? "MERGE_MESSAGE",
    custom_properties: {},
    ...(organization === null ? {} : { organization }),
    network_count: 0,
    subscribers_count: 0,
  };
}

/** GitHub's test merge of the pull request (`merge_commit_sha` of an open one): the commit that
 * merges `head` into `base`, which GitHub keeps while the two merge cleanly. The world writes it
 * into the run's remote as GitHub writes its own, unreferenced, at the head's own time, so every
 * read of one pair of heads names the one commit; two heads that conflict have none. */
function mergeCommit(runDir: string, base: string, head: string): string | null {
  const tree = remote(runDir, ["merge-tree", "--write-tree", "--no-messages", base, head]);
  const when = remote(runDir, ["show", "-s", "--format=%ct", head]);
  if (tree === undefined || when === undefined || !/^[0-9a-f]{40}$/.test(tree)) return null;
  const date = `@${when} +0000`;
  const made = remote(
    runDir,
    ["commit-tree", tree, "-p", base, "-p", head, "-m", `Merge ${head} into ${base}`],
    {
      GIT_AUTHOR_NAME: "GitHub",
      GIT_AUTHOR_EMAIL: "noreply@github.com",
      GIT_AUTHOR_DATE: date,
      GIT_COMMITTER_NAME: "GitHub",
      GIT_COMMITTER_EMAIL: "noreply@github.com",
      GIT_COMMITTER_DATE: date,
    }
  );
  return made ?? null;
}

/** What a pull request is read from: the world's repository, its number, its two branches and
 * their tips on the run's remote, the world's settings of its repository (`repositoryJson`'s
 * `held`) and accounts, and how many comments its conversation and its review threads hold as the
 * run leaves them. */
export interface PullFacts {
  readonly runDir: string;
  readonly repo: string;
  readonly pr: number;
  readonly branch: string;
  readonly base: string;
  readonly head: string;
  readonly baseSha: string;
  readonly repository: Json;
  readonly accounts: readonly Account[];
  readonly comments: number;
  readonly reviewComments: number;
}

/** The pull request as GitHub's REST API answers it: `view`, the pull request as `gh pr view`
 * reads it as it stands (its node id, URL, title, body, state, draft, mergeability and merge
 * state), at the tips `facts` names, in GitHub's shape: `simple` as GitHub lists one (the pull
 * requests of a commit), `full` as it answers one by its number, with its counts. The two
 * spellings say one thing: `mergeable` is gh's `MERGEABLE`, `mergeable_state` gh's
 * `mergeStateStatus` in GitHub's lower case, and the state gh's `OPEN`. It was opened when the
 * first commit of its branch was made, by that commit's author — the account its address is, or
 * the name it carries, which is a role App's bot (rig.sh's role_identity) — as the implementer
 * opens the pull request of the branch it pushed. GitHub's REST API names an empty body `null`,
 * where its GraphQL and gh read it as `""`: measured on 2026-11-18, cybermonklabs/legion #612
 * (the one pull request of its first 4000 with an empty body) read `body` `null` through `gh api
 * repos/<o>/<r>/pulls/612`, `""` through GraphQL and `gh pr view --json body`, and `--\n\n`
 * closing `gh pr view`; the world answered the REST `body` `""`. */
export function pullRequestJson(view: Json, facts: PullFacts, form: "simple" | "full"): Json {
  const { runDir, repo, pr, accounts } = facts;
  const api = `${API}/repos/${repo}`;
  const html = `${WEB}/${repo}/pull/${pr}`;
  const owner = repo.split("/")[0] ?? "";
  const repository = repositoryJson(facts.repository, runDir, accounts, "pull");
  const side = (ref: string, sha: string) => ({
    label: `${owner}:${ref}`,
    ref,
    sha,
    user: repository.owner,
    repo: repository,
  });
  const first = remote(runDir, [
    "log",
    "--reverse",
    "--format=%ct%x00%ae%x00%an",
    `${facts.baseSha}..${facts.head}`,
  ])?.split("\n")[0];
  const [opened = "", email = "", name = ""] = (first ?? "").split("\0");
  const author = accounts.find((one) => one.email === email)?.login ?? name;
  const updated = remote(runDir, ["show", "-s", "--format=%ct", facts.head]);
  const time = (seconds: string | undefined) =>
    seconds === undefined || seconds === "" ? null : githubTime(Number(seconds));
  const state = String(view.state ?? "OPEN").toLowerCase();
  const simple = {
    url: `${api}/pulls/${pr}`,
    id: 2_000_000_000 + pr,
    node_id: view.id ?? `PR_kwDOAAAAAc4AAA${pr}`,
    html_url: html,
    diff_url: `${html}.diff`,
    patch_url: `${html}.patch`,
    issue_url: `${api}/issues/${pr}`,
    commits_url: `${api}/pulls/${pr}/commits`,
    review_comments_url: `${api}/pulls/${pr}/comments`,
    review_comment_url: `${api}/pulls/comments{/number}`,
    comments_url: `${api}/issues/${pr}/comments`,
    statuses_url: `${api}/statuses/${facts.head}`,
    number: pr,
    state: state === "merged" ? "closed" : state,
    locked: false,
    title: view.title ?? "",
    user: userJson(author, accounts),
    body: view.body === "" || view.body === undefined ? null : view.body,
    labels: [],
    milestone: null,
    active_lock_reason: null,
    created_at: time(opened === "" ? updated : opened),
    updated_at: time(updated),
    closed_at: null,
    merged_at: null,
    merge_commit_sha: mergeCommit(runDir, facts.baseSha, facts.head),
    assignee: null,
    assignees: [],
    requested_reviewers: [],
    requested_teams: [],
    head: side(facts.branch, facts.head),
    base: side(facts.base, facts.baseSha),
    _links: {
      self: { href: `${api}/pulls/${pr}` },
      html: { href: html },
      issue: { href: `${api}/issues/${pr}` },
      comments: { href: `${api}/issues/${pr}/comments` },
      review_comments: { href: `${api}/pulls/${pr}/comments` },
      review_comment: { href: `${api}/pulls/comments{/number}` },
      commits: { href: `${api}/pulls/${pr}/commits` },
      statuses: { href: `${api}/statuses/${facts.head}` },
    },
    author_association: "NONE",
    auto_merge: null,
    draft: view.isDraft === true,
  };
  if (form === "simple") return simple;
  const range = `${facts.baseSha}...${facts.head}`;
  const stat = remote(runDir, ["diff", "--shortstat", range]) ?? "";
  const count = (word: string) => Number(new RegExp(`(\\d+) ${word}`).exec(stat)?.[1] ?? 0);
  const mergeable =
    view.mergeable === "MERGEABLE" ? true : view.mergeable === "CONFLICTING" ? false : null;
  return {
    ...simple,
    merged: false,
    mergeable,
    rebaseable: mergeable,
    mergeable_state: String(view.mergeStateStatus ?? "UNKNOWN").toLowerCase(),
    merged_by: null,
    comments: facts.comments,
    review_comments: facts.reviewComments,
    maintainer_can_modify: false,
    commits: Number(
      remote(runDir, ["rev-list", "--count", `${facts.baseSha}..${facts.head}`]) ?? 0
    ),
    additions: count("insertion"),
    deletions: count("deletion"),
    changed_files: count("file"),
  };
}
