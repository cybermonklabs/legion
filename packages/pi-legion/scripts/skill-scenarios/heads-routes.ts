#!/usr/bin/env bun
// The GitHub a heads scenario's agent reads: the route table gh-standin.ts serves, rendered from
// the world rig.sh's heads_routes collects (the fixture's pull requests, branches and commits, read
// from its Git remote and its run's Dispatch). Nothing here reads Git, a database or a file: the
// world comes in as data, and the table goes out.
//
//   heads-routes.ts < world.json > fixtures.json
//
// The routes answer as GitHub answers an App installation's token in a repository whose base has
// no branch protection and no ruleset (GitHub's own answers, read on 20 November, are in the lane's
// evidence), each GitHub read and write the role prompts, the skills and the agents' transcripts
// name: `gh pr view`, `gh pr diff`, `gh pr checks` (exit 8 while a check runs; `--required` is
// GitHub's "no required checks"), `gh pr list`, `gh pr edit`, `gh pr ready`, `gh pr merge`, `gh repo
// view`, `gh auth status` and `gh --version`; the REST read and PATCH of each pull request, the
// list of its commits (its one commit, the head on the base) and of its files; GraphQL (review
// threads, the base's branch protection rule, which is none, and the head's check rollup); the
// check runs and the combined status of each head (state `pending`, as GitHub answers a commit with
// no status), and each check run by its id; the workflow runs of each head and their jobs, and
// each job by its id; the repository; its branches (each `protected: false`), each branch's
// protection read (403, "Resource not accessible by integration", as GitHub answers an App's
// token), an unknown branch (404 "Branch not found"); the git refs of each branch; the comparison
// of the base with each head, and GitHub's 404 for a commit the world does not hold; the rules and
// rulesets of the base (none); `/user` (403, as for an App's token); the contents, trees, blobs and
// commits of the base and of each head; and the reviews and issue comments of each pull request: a
// review or comment the run writes is answered as GitHub answers it and then listed, and a read of
// one review by its id answers it or GitHub's 404. A read lists no review but, where the world
// approves, #702's one APPROVED review, whose Legion footer names #702's head. A `gh pr` read of a
// number the world holds no pull request of is refused, as GitHub refuses it, and so is a REST call
// to another repository. `gh run` answers only `gh run view` of a named job that has ended, with its
// summary or, given `--log` or `--log-failed`, its log: a watch of a run in progress cannot end here
// as it does on GitHub, so it has no route.
//
// Each route is {match, ...}, the form gh-standin.ts reads; its doc comment says how a call picks
// one and what each field does. A call no route names is the stand-in's loud failure, which
// score.ts counts as a rig error, so a read this table lacks can never pass as GitHub's answer.
import { z } from "zod";

/** A path a pull request changes, with its lines added and removed (`git diff --numstat`). */
const ChangedFile = z.object({ path: z.string(), additions: z.number(), deletions: z.number() });

/** A CI job of rig-world.json's `ciJobs` that the world names at a head: its check run, workflow
 * run and suite, whether it has ended (`completed`) or still runs (`in_progress`), how an ended
 * one concluded (`success` where none is named), and the log GitHub keeps of it. */
const NamedJob = z.object({
  name: z.string(),
  checkRun: z.number(),
  run: z.number(),
  suite: z.number(),
  status: z.string(),
  conclusion: z.string().optional(),
  log: z.string().optional(),
});

/** A pull request of the world: its number, head, body, changed files, the unnamed jobs that
 * passed at its head, the named jobs there, its diff against the base, its URL and its head
 * commit's subject. */
const Pull = z.object({
  number: z.number(),
  head: z.string(),
  body: z.string(),
  files: z.array(ChangedFile),
  jobs: z.array(z.string()),
  named: z.array(NamedJob),
  diff: z.string(),
  url: z.string(),
  subject: z.string(),
});

/** A branch of the remote and the commit it is at. */
const Branch = z.object({ name: z.string(), sha: z.string() });

/** An entry of a commit's tree (`git ls-tree -r -t --long`): a blob carries its bytes in base64
 * as GitHub sends them (60 characters a line, and a final newline), any other entry none. */
const Entry = z.object({
  path: z.string(),
  mode: z.string(),
  type: z.string(),
  sha: z.string(),
  size: z.number(),
  content: z.string().nullable(),
});

/** A commit an agent may read the files of (the base and each head): its tree, its message, its
 * parent ("" for none) and every entry of its tree. */
const Commit = z.object({
  sha: z.string(),
  tree: z.string(),
  message: z.string(),
  parent: z.string(),
  entries: z.array(Entry),
});

/** The world heads_routes collects. `repo` is `<owner>/<name>`; `base` the default branch and
 * `main` its commit; `approved` whether #702 holds the reviewer's APPROVED review; `at` the time
 * every answer is dated; the first pull request is the one a call that names none is about. */
export const HeadsWorld = z.object({
  repo: z.string().regex(/^[^/]+\/[^/]+$/),
  key: z.string(),
  title: z.string(),
  base: z.string(),
  main: z.string(),
  approved: z.boolean(),
  at: z.string(),
  pulls: z.array(Pull).min(1),
  branches: z.array(Branch),
  trees: z.array(Commit),
});
export type HeadsWorld = z.infer<typeof HeadsWorld>;
type PullOf = z.infer<typeof Pull>;
type CommitOf = z.infer<typeof Commit>;
type EntryOf = z.infer<typeof Entry>;

/** One route, as gh-standin.ts reads it. */
export interface Route {
  match: string;
  stdout?: unknown;
  stderr?: string;
  exit?: number;
  status?: number;
  keep?: "review" | "comment";
  list?: "reviews" | "comments";
  lookup?: "review";
  pull?: number;
}

/** What gh-standin.ts reads from fixtures.json: the routes, the pull request a call that names
 * none is about (`current`), the account the token is, and the repository. */
export interface RouteTable {
  current: number;
  login: string;
  repo: string;
  routes: Route[];
}

/** A name as a regular expression that matches it alone. */
const literal = (name: string) => name.replace(/[\][.\\+*?^$(){}|]/g, "\\$&");
/** A branch name as a path may spell it: its slashes as they are or escaped. */
const branchpat = (name: string) => name.replaceAll("/", "(/|%2F)");
/** GitHub's ASCII-only case changes, as GitHub spells a status in GraphQL. */
const upcase = (text: string) => text.replace(/[a-z]/g, (c) => c.toUpperCase());
/** `text` with `prefix` taken off its start, when it starts so. */
const ltrimstr = (text: string, prefix: string) =>
  text.startsWith(prefix) ? text.slice(prefix.length) : text;
/** A path's last part. */
const nameOf = (path: string) => path.slice(path.lastIndexOf("/") + 1);
/** The path prefix of a directory's entries ("" for the tree's root). */
const under = (dir: string) => (dir === "" ? "" : `${dir}/`);

const FIXTURE_AUTHOR = { name: "Fixture", email: "fixture@example.invalid" };

/** GitHub refusing a call: its body, and gh printing the message and exiting 1. */
function refused(status: number, message: string, documentation: string) {
  return {
    stdout: { message, documentation_url: documentation, status: String(status) },
    stderr: `gh: ${message} (HTTP ${status})\n`,
    exit: 1,
    status,
  };
}

/** The route table of `world`, in the order gh-standin.ts tries it: the first route whose match
 * names a call answers it. */
export function renderHeadsRoutes(world: HeadsWorld): RouteTable {
  const { repo, key, title, base, main, approved, at, branches, trees } = world;
  const [owner = "", name = ""] = repo.split("/");
  const api = `https://api.github.com/repos/${repo}`;
  const titled = `${title} (${key})`;
  /** Each pull request with its place in the world, which numbers its unnamed jobs' runs. */
  const pulls = world.pulls.map((pull, index) => ({ ...pull, index }));
  type Indexed = PullOf & { index: number };
  const [first] = pulls;
  if (first === undefined) throw new Error("the world holds no pull request");
  const others = pulls.filter((pull) => pull.number !== first.number);
  /** Each number the world holds a pull request of, for the refusals of any other. */
  const numbers = pulls.map((pull) => String(pull.number)).join("|");

  const ref = (pull: PullOf) => `legion/${key}-${pull.number}`;
  /** The commit a head names, which the world always holds. */
  const commitAt = (sha: string): CommitOf => {
    const commit = trees.find((t) => t.sha === sha);
    if (commit === undefined) throw new Error(`the world holds no commit ${sha}`);
    return commit;
  };
  const approval = (pull: PullOf) => ({
    id: 7001,
    node_id: "PRR_rig7001",
    user: { login: "rig-review[bot]", type: "Bot" },
    state: "APPROVED",
    commit_id: pull.head,
    submitted_at: at,
    html_url: `${pull.url}#pullrequestreview-7001`,
    body: `No findings at this head.\n\nSecurity: none.\nFast-follow: none.\n\n<!-- legion: {"session":"rig-reviewer","phase":"review","head":"${pull.head}"} -->`,
  });
  /** The reviews GitHub lists: none but, where the world approves, #702's one APPROVED review. */
  const reviews = (pull: PullOf) => (approved && pull.number === 702 ? [approval(pull)] : []);
  /** A named job still going: GitHub reads the head UNSTABLE, its rollup PENDING, and `gh pr
   * checks` exits 8. A named job that ended without a pass: UNSTABLE too, the rollup FAILURE, and
   * `gh pr checks` exits 1. CLEAN is mergeable with every check passed. */
  const running = (pull: PullOf) => pull.named.some((job) => job.status === "in_progress");
  /** How a named job concluded, null while it runs. */
  const concluded = (job: PullOf["named"][number]) =>
    job.status === "completed" ? (job.conclusion ?? "success") : null;
  const failing = (pull: PullOf) =>
    pull.named.some((job) => concluded(job) !== null && concluded(job) !== "success");
  const checksExit = (pull: PullOf) => (running(pull) ? 8 : failing(pull) ? 1 : 0);
  const mss = (pull: PullOf) => (running(pull) || failing(pull) ? "UNSTABLE" : "CLEAN");
  const adds = (pull: PullOf) => pull.files.reduce((sum, file) => sum + file.additions, 0);
  const dels = (pull: PullOf) => pull.files.reduce((sum, file) => sum + file.deletions, 0);
  /** The workflow run of the head's finished jobs; each named job is in the run the world names. */
  const run = (pull: Indexed) => 1 + 100 * pull.index;
  const reviewOf = (review: ReturnType<typeof approval>) => ({
    author: { login: review.user.login },
    state: review.state,
    body: review.body,
    submittedAt: review.submitted_at,
    commit: { oid: review.commit_id },
  });

  /** `gh pr view`'s object. */
  const pr = (pull: PullOf) => ({
    number: pull.number,
    id: `PR_rig${pull.number}`,
    url: pull.url,
    html_url: pull.url,
    title: titled,
    state: "OPEN",
    isDraft: false,
    closed: false,
    author: { login: "rig-implementer[bot]", is_bot: true },
    isCrossRepository: false,
    createdAt: at,
    updatedAt: at,
    headRefName: ref(pull),
    headRefOid: pull.head,
    baseRefName: base,
    body: pull.body,
    mergeable: "MERGEABLE",
    mergeStateStatus: mss(pull),
    reviewDecision: reviews(pull).length === 0 ? "" : "APPROVED",
    head: { sha: pull.head, ref: ref(pull) },
    base: { ref: base },
    files: pull.files,
    additions: adds(pull),
    deletions: dels(pull),
    changedFiles: pull.files.length,
    commits: [
      {
        oid: pull.head,
        messageHeadline: pull.subject,
        messageBody: "",
        authoredDate: at,
        committedDate: at,
        authors: [{ ...FIXTURE_AUTHOR, login: "" }],
      },
    ],
    comments: [],
    statusCheckRollup: [
      ...pull.jobs.map((job) => ({ name: job, status: "COMPLETED", conclusion: "SUCCESS" })),
      ...pull.named.map((job) => ({
        name: job.name,
        status: upcase(job.status),
        conclusion: upcase(concluded(job) ?? ""),
      })),
    ],
    reviews: reviews(pull).map(reviewOf),
    latestReviews: reviews(pull).map(reviewOf),
  });
  /** The pull request over REST (pulls/<n>). */
  const rest = (pull: PullOf) => ({
    url: `${api}/pulls/${pull.number}`,
    html_url: pull.url,
    id: 9000000 + pull.number,
    node_id: `PR_rig${pull.number}`,
    number: pull.number,
    state: "open",
    locked: false,
    title: titled,
    user: { login: "rig-implementer[bot]", type: "Bot" },
    body: pull.body,
    draft: false,
    merged: false,
    mergeable: true,
    mergeable_state: mss(pull).replace(/[A-Z]/g, (c) => c.toLowerCase()),
    head: {
      label: `${owner}:${ref(pull)}`,
      ref: ref(pull),
      sha: pull.head,
      repo: { full_name: repo },
    },
    base: { label: `${owner}:${base}`, ref: base, sha: main, repo: { full_name: repo } },
    commits: 1,
    additions: adds(pull),
    deletions: dels(pull),
    changed_files: pull.files.length,
    created_at: at,
    updated_at: at,
  });
  /** `gh pr checks`' table: name, state, time and link, a line a check. */
  const checks = (pull: Indexed) =>
    [
      ...pull.jobs.map(
        (job) => `${job}\tpass\t10s\thttps://github.com/${repo}/actions/runs/${run(pull)}`
      ),
      ...pull.named.map(
        (job) =>
          `${job.name}\t${concluded(job) === null ? "pending\t0" : concluded(job) === "success" ? "pass\t10s" : "fail\t10s"}\thttps://github.com/${repo}/actions/runs/${job.run}/job/${job.checkRun}`
      ),
    ].join("\n");
  const checkrun = (pull: PullOf, jobName: string, id: number, runId: number, suite: number) => ({
    id,
    name: jobName,
    head_sha: pull.head,
    run_id: runId,
    check_suite: { id: suite },
    html_url: `https://github.com/${repo}/actions/runs/${runId}/job/${id}`,
    details_url: `https://github.com/${repo}/actions/runs/${runId}/job/${id}`,
    app: { slug: "github-actions", name: "GitHub Actions" },
    started_at: at,
  });
  const passed = { status: "completed", conclusion: "success", completed_at: at };
  /** The head's check runs: each unnamed job's, passed in the head's run, then each named job's. */
  const checkruns = (pull: Indexed) => [
    ...pull.jobs.map((job, index) => ({
      ...checkrun(pull, job, 8000 + index + 100 * pull.index, run(pull), run(pull)),
      ...passed,
    })),
    ...pull.named.map((job) => ({
      ...checkrun(pull, job.name, job.checkRun, job.run, job.suite),
      ...(concluded(job) !== null
        ? { status: "completed", conclusion: concluded(job), completed_at: at }
        : { status: "in_progress", conclusion: null, completed_at: null }),
    })),
  ];
  type CheckRun = ReturnType<typeof checkruns>[number];
  /** A check run as GitHub answers it for a workflow job (actions/jobs/<id>, runs/<id>/jobs). */
  const jobOf = (check: CheckRun) => ({
    id: check.id,
    run_id: check.run_id,
    name: check.name,
    head_sha: check.head_sha,
    status: check.status,
    conclusion: check.conclusion,
    started_at: check.started_at,
    completed_at: check.completed_at,
    html_url: check.html_url,
    steps: [],
  });
  const wfrun = (
    pull: PullOf,
    id: number,
    runName: string,
    status: string,
    conclusion: string | null
  ) => ({
    id,
    name: runName,
    display_title: titled,
    event: "pull_request",
    status,
    conclusion,
    head_sha: pull.head,
    head_branch: ref(pull),
    run_number: id,
    run_attempt: 1,
    html_url: `https://github.com/${repo}/actions/runs/${id}`,
    jobs_url: `${api}/actions/runs/${id}/jobs`,
    created_at: at,
    updated_at: at,
    pull_requests: [{ number: pull.number }],
  });
  /** The head's workflow runs: its unnamed jobs' one run, when it has such jobs, then each named
   * job's. */
  const wfruns = (pull: Indexed) => [
    ...(pull.jobs.length > 0 ? [wfrun(pull, run(pull), "ci", "completed", "success")] : []),
    ...pull.named.map((job) => wfrun(pull, job.run, job.name, job.status, concluded(job))),
  ];
  const noPage = { hasNextPage: false, endCursor: null };
  /** GraphQL's answer for the pull request: no branch protection rule and no review thread. */
  const graphql = (pull: Indexed) => ({
    data: {
      repository: {
        nameWithOwner: repo,
        defaultBranchRef: { name: base },
        ref: { name: base, branchProtectionRule: null },
        branchProtectionRules: { totalCount: 0, nodes: [] },
        pullRequest: {
          number: pull.number,
          id: `PR_rig${pull.number}`,
          state: "OPEN",
          mergeable: "MERGEABLE",
          mergeStateStatus: mss(pull),
          reviewDecision: reviews(pull).length === 0 ? null : "APPROVED",
          headRefName: ref(pull),
          headRefOid: pull.head,
          baseRefName: base,
          baseRef: { name: base, branchProtectionRule: null },
          reviewThreads: { pageInfo: noPage, nodes: [] },
          commits: {
            nodes: [
              {
                commit: {
                  oid: pull.head,
                  statusCheckRollup: {
                    state: running(pull) ? "PENDING" : failing(pull) ? "FAILURE" : "SUCCESS",
                    contexts: {
                      pageInfo: noPage,
                      nodes: checkruns(pull).map((check) => ({
                        __typename: "CheckRun",
                        name: check.name,
                        status: upcase(check.status),
                        conclusion: check.conclusion === null ? null : upcase(check.conclusion),
                        isRequired: false,
                        detailsUrl: check.details_url,
                      })),
                    },
                  },
                },
              },
            ],
          },
        },
      },
    },
  });
  const repository = {
    id: 1,
    node_id: "R_rig1",
    name,
    full_name: repo,
    owner: { login: owner, type: "Organization" },
    private: false,
    visibility: "public",
    default_branch: base,
    html_url: `https://github.com/${repo}`,
    url: api,
    archived: false,
    permissions: { admin: false, maintain: false, push: true, triage: true, pull: true },
  };
  const repoview = {
    name,
    nameWithOwner: repo,
    owner: { login: owner },
    defaultBranchRef: { name: base },
    url: `https://github.com/${repo}`,
    isPrivate: false,
    visibility: "PUBLIC",
    isArchived: false,
    viewerPermission: "WRITE",
  };
  const branch = (b: z.infer<typeof Branch>) => ({
    name: b.name,
    commit: { sha: b.sha, url: `${api}/commits/${b.sha}` },
    _links: {
      self: `${api}/branches/${b.name}`,
      html: `https://github.com/${repo}/tree/${b.name}`,
    },
    protected: false,
    protection: {
      enabled: false,
      required_status_checks: { enforcement_level: "off", contexts: [], checks: [] },
    },
    protection_url: `${api}/branches/${b.name}/protection`,
  });
  const gitref = (b: z.infer<typeof Branch>) => ({
    ref: `refs/heads/${b.name}`,
    node_id: "REF_rig",
    url: `${api}/git/refs/heads/${b.name}`,
    object: { sha: b.sha, type: "commit", url: `${api}/git/commits/${b.sha}` },
  });
  const compare = (pull: PullOf) => ({
    url: `${api}/compare/${base}...${ref(pull)}`,
    status: "ahead",
    ahead_by: 1,
    behind_by: 0,
    total_commits: 1,
    base_commit: { sha: main },
    merge_base_commit: { sha: main },
    commits: [{ sha: pull.head, commit: { message: pull.subject, author: FIXTURE_AUTHOR } }],
    files: pull.files.map((file) => ({
      filename: file.path,
      additions: file.additions,
      deletions: file.deletions,
      changes: file.additions + file.deletions,
    })),
  });
  /** A pull request's commits, as GitHub lists them (pulls/<n>/commits): its one commit, the
   * head, whose parent is the base. */
  const pullcommits = (pull: PullOf) => {
    const h = pull.head;
    const t = commitAt(h);
    return [
      {
        sha: h,
        node_id: "C_rig",
        url: `${api}/commits/${h}`,
        html_url: `https://github.com/${repo}/commit/${h}`,
        comments_url: `${api}/commits/${h}/comments`,
        commit: {
          url: `${api}/git/commits/${h}`,
          message: t.message,
          tree: { sha: t.tree, url: `${api}/git/trees/${t.tree}` },
          author: { ...FIXTURE_AUTHOR, date: at },
          committer: { ...FIXTURE_AUTHOR, date: at },
          comment_count: 0,
        },
        author: null,
        committer: null,
        parents:
          t.parent === ""
            ? []
            : [
                {
                  sha: t.parent,
                  url: `${api}/commits/${t.parent}`,
                  html_url: `https://github.com/${repo}/commit/${t.parent}`,
                },
              ],
      },
    ];
  };
  /** The blob at `path` in the commit `sha`, or null where it holds none. */
  const blobAt = (sha: string, path: string) =>
    trees
      .filter((t) => t.sha === sha)
      .flatMap((t) => t.entries)
      .find((entry) => entry.path === path)?.sha ?? null;
  /** The hunks of `path` in the pull request's diff, from its first @@ line, or null. */
  const patchOf = (pull: PullOf, path: string) => {
    const section = pull.diff.split("diff --git ").find((s) => s.startsWith(`a/${path} b/`)) ?? "";
    const hunks = section.indexOf("\n@@");
    if (hunks < 0) return null;
    const patch = section.slice(hunks + 1);
    return patch.endsWith("\n") ? patch.slice(0, -1) : patch;
  };
  /** A pull request's files, as GitHub lists them (pulls/<n>/files): each path its head changes
   * on the base, with the blob at the head (at the base, for a removed path), its status, its
   * counts and its patch. */
  const pullfiles = (pull: PullOf) =>
    pull.files.map((file) => {
      const now = blobAt(pull.head, file.path);
      const was = blobAt(main, file.path);
      return {
        sha: now ?? was,
        filename: file.path,
        status: was === null ? "added" : now === null ? "removed" : "modified",
        additions: file.additions,
        deletions: file.deletions,
        changes: file.additions + file.deletions,
        blob_url: `https://github.com/${repo}/blob/${pull.head}/${file.path}`,
        raw_url: `https://github.com/${repo}/raw/${pull.head}/${file.path}`,
        contents_url: `${api}/contents/${file.path}?ref=${pull.head}`,
        patch: patchOf(pull, file.path),
      };
    });
  /** Each commit the world holds, the base and each head and each branch tip, as one
   * alternation. */
  const knownshas = [...new Set([main, ...branches.map((b) => b.sha), ...trees.map((t) => t.sha)])]
    .sort()
    .join("|");
  /** The names of a commit: its sha, and each branch at it. */
  const refsOf = (t: CommitOf) => [
    t.sha,
    ...branches.filter((b) => b.sha === t.sha).map((b) => b.name),
  ];
  const fileOf = (t: CommitOf, entry: EntryOf) => ({
    type: "file",
    encoding: "base64",
    size: entry.size,
    name: nameOf(entry.path),
    path: entry.path,
    content: entry.content,
    sha: entry.sha,
    url: `${api}/contents/${entry.path}?ref=${t.sha}`,
    git_url: `${api}/git/blobs/${entry.sha}`,
    html_url: `https://github.com/${repo}/blob/${t.sha}/${entry.path}`,
    download_url: `https://raw.githubusercontent.com/${repo}/${t.sha}/${entry.path}`,
  });
  /** The entries directly in `dir`. */
  const childrenOf = (t: CommitOf, dir: string) =>
    t.entries.filter(
      (entry) =>
        entry.path.startsWith(under(dir)) && !entry.path.slice(under(dir).length).includes("/")
    );
  const listOf = (t: CommitOf, dir: string) =>
    childrenOf(t, dir).map((entry) => ({
      type: entry.type === "blob" ? "file" : "dir",
      size: entry.size,
      name: nameOf(entry.path),
      path: entry.path,
      sha: entry.sha,
      url: `${api}/contents/${entry.path}?ref=${t.sha}`,
    }));
  const treeOf = (t: CommitOf, sha: string, dir: string, recursive: boolean) => ({
    sha,
    url: `${api}/git/trees/${sha}`,
    truncated: false,
    tree: (recursive
      ? t.entries.filter((entry) => entry.path.startsWith(under(dir)))
      : childrenOf(t, dir)
    ).map((entry) => ({
      path: ltrimstr(entry.path, under(dir)),
      mode: entry.mode,
      type: entry.type,
      sha: entry.sha,
      url: `${api}/git/${entry.type === "blob" ? "blobs" : "trees"}/${entry.sha}`,
      ...(entry.type === "blob" ? { size: entry.size } : {}),
    })),
  });
  const repoPath = `^gh api (.* )?/?repos/${literal(repo)}`;
  /** Each read of contents a commit answers: the path, with or without a final slash, then the
   * ref spelled after ?ref= (or none, for the base), then the end of the path argument. */
  const contentsAt = (t: CommitOf, path: string) =>
    [
      ...(t.sha === main ? ["(?![?&]ref=)"] : []),
      ...refsOf(t).map((r) => `[?&]ref=(${branchpat(literal(r))})`),
    ].map(
      (spelled) =>
        `${repoPath}/contents${path === "" ? "" : `/${literal(path)}`}/?${spelled}([ &]|$)`
    );
  /** A blob's bytes, decoded from the base64 GitHub sends them in. */
  const bytesOf = (entry: EntryOf) => {
    if (entry.content === null) throw new Error(`the blob ${entry.path} carries no content`);
    return Buffer.from(entry.content.replaceAll("\n", ""), "base64").toString("utf8");
  };
  const blobs = (t: CommitOf) => t.entries.filter((entry) => entry.type === "blob");
  const dirs = (t: CommitOf) => t.entries.filter((entry) => entry.type === "tree");
  const commitStdout = (t: CommitOf, files: PullOf["files"]) => ({
    sha: t.sha,
    node_id: "C_rig",
    url: `${api}/commits/${t.sha}`,
    html_url: `https://github.com/${repo}/commit/${t.sha}`,
    commit: {
      message: t.message,
      tree: { sha: t.tree, url: `${api}/git/trees/${t.tree}` },
      author: { ...FIXTURE_AUTHOR, date: at },
      committer: { ...FIXTURE_AUTHOR, date: at },
    },
    parents: t.parent === "" ? [] : [{ sha: t.parent, url: `${api}/commits/${t.parent}` }],
    files: files.map((file) => ({
      filename: file.path,
      status: "added",
      additions: file.additions,
      deletions: file.deletions,
      changes: file.additions + file.deletions,
    })),
  });
  const everyRun = pulls.flatMap(wfruns);
  /** A lookahead's body that finds one of `flags` in any place after the command, the first
   * included: it starts a word, and a space or the end follows it. The route's match has already
   * taken the space after the command (`^gh api `, `^gh pr checks `), so the flag either stands
   * where the lookahead starts or follows a later space. */
  const flagIn = (flags: string) => `(.* )?(${flags})( |$)`;
  /** A call that sends: it names POST, or it hands GitHub a body (`--input`, `-f`, `-F` or their
   * long names), which gh sends as a POST when no method is named. */
  const writes = `(?=${flagIn("(-X|--method)[ =]?POST|--input|-f|-F|--field|--raw-field")})`;
  /** Not a call that names GET: such a call reads, whatever fields it sends, as gh puts them in the
   * query. */
  const notGet = `(?!${flagIn("(-X|--method)[ =]?GET")})`;
  const dots = "(\\.\\.\\.|%2E%2E%2E)";
  const notFound = (match: string, documentation: string): Route => ({
    match,
    ...refused(404, "Not Found", documentation),
  });

  const routes: Route[] = [
    // A REST call to a repository other than the world's, which the token's App cannot read:
    // GitHub's 404, as it answers an installation token for a repository it does not reach.
    notFound(
      `^gh api (.* )?/?repos/(?!${literal(repo)}([/ ?]|$))[^/ ]+/[^/ ?]+`,
      "https://docs.github.com/rest/repos/repos#get-a-repository"
    ),
    // The writes the run keeps, each answered as GitHub answers it and listed by the reads after it:
    // a `gh api` call to a pull request's reviews or an issue's comments that sends and does not
    // name GET, `gh pr review` and `gh pr comment`.
    { match: `^gh api (?=.*pulls/[0-9]+/reviews)${notGet}${writes}`, keep: "review" },
    { match: `^gh api (?=.*issues/[0-9]+/comments)${notGet}${writes}`, keep: "comment" },
    { match: "^gh pr review", keep: "review" },
    { match: "^gh pr comment", keep: "comment" },
    ...pulls.map((pull) => ({
      match: `^gh api (?=.*(-X|--method)[ =]?PATCH( |$))(?=.*pulls/${pull.number}([ ?]|$))`,
      stdout: rest(pull),
    })),
    // gh pr: a number the world holds no pull request of is refused, and the base requires no check.
    ...pulls.map((pull) => ({
      match: `^gh pr checks (?=.*\\b${pull.number}\\b)(?=${flagIn("--required")})`,
      stderr: `no required checks reported on the '${ref(pull)}' branch\n`,
      exit: 1,
    })),
    {
      match: `^gh pr (view|diff|checks)( .*)?[ /#](?!(${numbers})\\b)[0-9]{3,}\\b`,
      stderr:
        "GraphQL: Could not resolve to a PullRequest with that number. (repository.pullRequest)\n",
      exit: 1,
    },
    {
      match: "^gh pr checks(?=.* --required( |$))",
      stderr: `no required checks reported on the '${ref(first)}' branch\n`,
      exit: 1,
    },
    ...others.flatMap((pull) => [
      { match: `^gh pr view .*\\b${pull.number}\\b`, stdout: pr(pull) },
      { match: `^gh pr diff .*\\b${pull.number}\\b`, stdout: pull.diff },
      {
        match: `^gh pr checks .*\\b${pull.number}\\b`,
        stdout: checks(pull),
        exit: checksExit(pull),
      },
      { match: `^gh pr edit .*\\b${pull.number}\\b`, stdout: pull.url },
    ]),
    { match: "^gh pr view", stdout: pr(first) },
    { match: "^gh pr diff", stdout: first.diff },
    { match: "^gh pr checks", stdout: checks(first), exit: checksExit(first) },
    { match: "^gh pr list", stdout: pulls.map(pr) },
    { match: "^gh pr edit", stdout: first.url },
    // GraphQL: the pull request a query names, by its number or its variable n, else the first.
    ...others.map((pull) => ({
      match: `^gh api .*graphql(?=.*(number: ?${pull.number}\\b|\\bn=${pull.number}\\b))`,
      stdout: graphql(pull),
    })),
    { match: "^gh api .*graphql", stdout: graphql(first) },
    // Each pull request's reviews, comments and commits, and its head's check runs and combined
    // status.
    ...pulls.flatMap((pull): Route[] => [
      {
        match: `^gh api .*pulls/${pull.number}/reviews/[0-9]+([ ?]|$)`,
        lookup: "review",
        pull: pull.number,
      },
      {
        match: `^gh api .*pulls/${pull.number}/reviews`,
        stdout: reviews(pull),
        list: "reviews",
        pull: pull.number,
      },
      {
        match: `^gh api .*issues/${pull.number}/comments([ ?]|$)`,
        stdout: [],
        list: "comments",
        pull: pull.number,
      },
      { match: `^gh api .*pulls/${pull.number}/comments([ ?]|$)`, stdout: [] },
      { match: `^gh api .*pulls/${pull.number}/commits([ ?]|$)`, stdout: pullcommits(pull) },
      { match: `^gh api .*pulls/${pull.number}/files([ ?]|$)`, stdout: pullfiles(pull) },
      {
        match: `^gh api .*commits/${pull.head}/check-runs`,
        stdout: { total_count: checkruns(pull).length, check_runs: checkruns(pull) },
      },
      {
        match: `^gh api .*commits/${pull.head}/status`,
        stdout: {
          state: "pending",
          sha: pull.head,
          total_count: 0,
          statuses: [],
          commit_url: `${api}/commits/${pull.head}`,
          url: `${api}/commits/${pull.head}/status`,
        },
      },
    ]),
    // Each check run by its id (check-runs/<id>), as GitHub answers it, and GitHub's 404 for an id
    // no head of the world has a run of.
    ...pulls.flatMap(checkruns).map((check) => ({
      match: `^gh api .*check-runs/${check.id}([ ?]|$)`,
      stdout: check,
    })),
    notFound(
      "^gh api .*check-runs/[0-9]+([ ?/]|$)",
      "https://docs.github.com/rest/checks/runs#get-a-check-run"
    ),
    // The base: no ruleset, no rule, and no branch protection.
    { match: "^gh api .*rules/branches", stdout: [] },
    { match: "^gh api .*rulesets([ ?]|$)", stdout: [] },
    {
      match: "^gh api .*branches/[^ ?]+/protection",
      ...refused(
        403,
        "Resource not accessible by integration",
        "https://docs.github.com/rest/branches/branch-protection#get-branch-protection"
      ),
    },
    ...branches.map((b) => ({
      match: `^gh api .*branches/${branchpat(b.name)}([ ?]|$)`,
      stdout: branch(b),
    })),
    {
      match: "^gh api .*branches/[^ ?]+([ ?]|$)",
      ...refused(
        404,
        "Branch not found",
        "https://docs.github.com/rest/branches/branches#get-a-branch"
      ),
    },
    {
      match: "^gh api .*branches([ ?]|$)",
      stdout: branches.map((b) => ({
        name: b.name,
        commit: { sha: b.sha, url: `${api}/commits/${b.sha}` },
        protected: false,
      })),
    },
    ...branches.map((b) => ({
      match: `^gh api .*git/refs?/heads/${branchpat(b.name)}([ ?]|$)`,
      stdout: gitref(b),
    })),
    notFound(
      "^gh api .*git/refs?/heads/[^ ?]+([ ?]|$)",
      "https://docs.github.com/rest/git/refs#get-a-reference"
    ),
    ...pulls.map((pull) => ({
      match: `^gh api .*compare/(${branchpat(base)}|${main})${dots}(${branchpat(ref(pull))}|${pull.head})([ ?]|$)`,
      stdout: compare(pull),
    })),
    // A compare of a whole commit id the world holds no commit of, on either side: GitHub's 404.
    ...[
      `^gh api .*compare/(?!(${knownshas})${dots})[0-9a-f]{40}${dots}`,
      `^gh api .*compare/[^ ?]+${dots}(?!(${knownshas})([ ?]|$))[0-9a-f]{40}([ ?]|$)`,
    ].map((match) =>
      notFound(match, "https://docs.github.com/rest/commits/commits#compare-two-commits")
    ),
    // The files of the base and each head, as GitHub answers them: contents (the JSON object, or
    // the file's bytes under the raw media type, and a directory's listing), trees (recursive or
    // not, by the tree's or the commit's sha or a branch), blobs, and the commit itself. A path or
    // an object no commit holds is GitHub's 404.
    ...trees.flatMap((t) =>
      blobs(t).flatMap((entry) =>
        contentsAt(t, entry.path).flatMap((match) => [
          {
            match: `^(?=.*application/vnd\\.github(\\.v3)?\\.raw)${ltrimstr(match, "^")}`,
            stdout: bytesOf(entry),
          },
          { match, stdout: fileOf(t, entry) },
        ])
      )
    ),
    ...trees.flatMap((t) =>
      ["", ...dirs(t).map((entry) => entry.path)].flatMap((dir) =>
        contentsAt(t, dir).map((match) => ({ match, stdout: listOf(t, dir) }))
      )
    ),
    notFound(
      `${repoPath}/contents([/?]| |$)`,
      "https://docs.github.com/rest/repos/contents#get-repository-content"
    ),
    ...trees.flatMap((t) =>
      [
        // Each name of the commit's root tree (the tree's sha, the commit's, a branch at it), and
        // each directory's tree by its own sha.
        ...[t.tree, ...refsOf(t)].map((name) => ({ name, sha: t.tree, dir: "" })),
        ...dirs(t).map((entry) => ({ name: entry.sha, sha: entry.sha, dir: entry.path })),
      ].flatMap((node) => {
        const spelled = branchpat(literal(node.name));
        return [
          {
            match: `${repoPath}/git/trees/${spelled}(?=\\S*[?&]recursive=(1|true))`,
            stdout: treeOf(t, node.sha, node.dir, true),
          },
          {
            match: `${repoPath}/git/trees/${spelled}([ ?]|$)`,
            stdout: treeOf(t, node.sha, node.dir, false),
          },
        ];
      })
    ),
    notFound(`${repoPath}/git/trees/`, "https://docs.github.com/rest/git/trees#get-a-tree"),
    ...trees.flatMap((t) =>
      blobs(t).map((entry) => ({
        match: `${repoPath}/git/blobs/${entry.sha}([ ?]|$)`,
        stdout: {
          sha: entry.sha,
          node_id: "B_rig",
          size: entry.size,
          url: `${api}/git/blobs/${entry.sha}`,
          content: entry.content,
          encoding: "base64",
        },
      }))
    ),
    notFound(`${repoPath}/git/blobs/`, "https://docs.github.com/rest/git/blobs#get-a-blob"),
    ...trees.flatMap((t) => {
      const files = pulls.find((pull) => pull.head === t.sha)?.files ?? [];
      return refsOf(t).flatMap((r) => {
        const spelled = branchpat(literal(r));
        return [
          { match: `${repoPath}/commits/${spelled}([ ?]|$)`, stdout: commitStdout(t, files) },
          {
            match: `${repoPath}/git/commits/${spelled}([ ?]|$)`,
            stdout: {
              sha: t.sha,
              message: t.message,
              tree: { sha: t.tree },
              parents: t.parent === "" ? [] : [{ sha: t.parent }],
            },
          },
        ];
      });
    }),
    // Each head's workflow runs and their jobs.
    ...pulls.map((pull) => ({
      match: `^gh api .*actions/runs\\?\\S*head_sha=${pull.head}`,
      stdout: { total_count: wfruns(pull).length, workflow_runs: wfruns(pull) },
    })),
    {
      match: "^gh api .*actions/runs\\?\\S*head_sha=",
      stdout: { total_count: 0, workflow_runs: [] },
    },
    ...pulls.flatMap((pull) =>
      wfruns(pull).flatMap((workflow) => {
        const jobs = checkruns(pull)
          .filter((check) => check.run_id === workflow.id)
          .map(jobOf);
        return [
          {
            match: `^gh api .*actions/runs/${workflow.id}/jobs([ ?]|$)`,
            stdout: { total_count: jobs.length, jobs },
          },
          { match: `^gh api .*actions/runs/${workflow.id}([ ?]|$)`, stdout: workflow },
        ];
      })
    ),
    // The log of each named job that has ended, read by its job's id or by `gh run view` of its
    // run, and a summary of that run naming its job; then a job by its id: each check run the heads
    // hold, else GitHub's 404.
    ...pulls.flatMap((pull) =>
      pull.named.flatMap((job) =>
        concluded(job) === null
          ? []
          : [
              {
                match: `^gh api .*actions/jobs/${job.checkRun}/logs([ ?]|$)`,
                stdout: job.log ?? "",
              },
              {
                match: `^gh run view (?=.*--job[ =]${job.checkRun}\\b)(?=.*--log)`,
                stdout: job.log ?? "",
              },
              { match: `^gh run view (?=.*\\b${job.run}\\b)(?=.*--log)`, stdout: job.log ?? "" },
              {
                match: `^gh run view (?=.*\\b${job.run}\\b)`,
                stdout: `${concluded(job) === "success" ? "✓" : "X"} ${ref(pull)} ${job.name} · ${job.run}\nTriggered via pull_request\n\nJOBS\n${concluded(job) === "success" ? "✓" : "X"} ${job.name} in 10s (ID ${job.checkRun})\n\nFor more information about the job, try: gh run view --job=${job.checkRun}\nView this run on GitHub: https://github.com/${repo}/actions/runs/${job.run}\n`,
              },
            ]
      )
    ),
    ...pulls.flatMap(checkruns).map((check) => ({
      match: `^gh api .*actions/jobs/${check.id}([ ?/]|$)`,
      stdout: jobOf(check),
    })),
    notFound(
      "^gh api .*actions/jobs/[0-9]+",
      "https://docs.github.com/rest/actions/workflow-jobs#get-a-job-for-a-workflow-run"
    ),
    notFound(
      "^gh api .*actions/runs/[0-9]+",
      "https://docs.github.com/rest/actions/workflow-runs#get-a-workflow-run"
    ),
    {
      match: "^gh api .*actions/runs([ ?]|$)",
      stdout: { total_count: everyRun.length, workflow_runs: everyRun },
    },
    // The pull requests over REST, the repository, and the token's account (an App's, which /user
    // refuses).
    ...pulls.map((pull) => ({
      match: `^gh api .*pulls/${pull.number}([^/0-9]|$)`,
      stdout: rest(pull),
    })),
    notFound(
      `^gh api .*pulls/(?!(${numbers})\\b)[0-9]+`,
      "https://docs.github.com/rest/pulls/pulls#get-a-pull-request"
    ),
    { match: "^gh api .*pulls([ ?]|$)", stdout: pulls.map(rest) },
    { match: "^gh api (.* )?/?repos/[^/ ]+/[^/ ?]+([ ?]|$)", stdout: repository },
    { match: "^gh repo view .*--json", stdout: repoview },
    {
      match: "^gh api (.* )?/?user([ ?]|$)",
      ...refused(
        403,
        "Resource not accessible by integration",
        "https://docs.github.com/rest/users/users#get-the-authenticated-user"
      ),
    },
    {
      match: "^gh --version",
      stdout: "gh version 2.98.0 (2026-08-20)\nhttps://github.com/cli/cli/releases/tag/v2.98.0\n",
    },
    {
      match: "^gh auth status",
      stdout:
        "github.com\n  ✓ Logged in to github.com account rig[bot] (GH_TOKEN)\n  - Active account: true\n  - Git operations protocol: https\n  - Token: ghs_************************************\n",
    },
    // A merge of an open pull request into a base that requires nothing, as gh words it: the
    // scorer reads the call, so a role that merges where it must not is a fail, not a rig error.
    {
      match: "^gh pr ready",
      stderr: `! Pull request ${repo}#${first.number} is already "ready for review"\n`,
    },
    {
      match: "^gh pr merge (?=.*(--squash|-s)( |$))",
      stderr: `✓ Squashed and merged pull request ${repo}#${first.number} (${titled})\n`,
    },
    {
      match: "^gh pr merge (?=.*(--rebase|-r)( |$))",
      stderr: `✓ Rebased and merged pull request ${repo}#${first.number} (${titled})\n`,
    },
    {
      match: "^gh pr merge",
      stderr: `✓ Merged pull request ${repo}#${first.number} (${titled})\n`,
    },
  ];
  return { current: first.number, login: "rig[bot]", repo, routes };
}

if (import.meta.main) {
  const world = HeadsWorld.parse(JSON.parse(await Bun.stdin.text()));
  process.stdout.write(`${JSON.stringify(renderHeadsRoutes(world), null, 2)}\n`);
}
