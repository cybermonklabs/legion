// The world a heads scenario's GitHub holds, as rig.sh's heads_routes collects it, and what GitHub
// answers from it: each pull request, check run, workflow run, branch, commit and file as gh, REST
// and GraphQL give it. heads-routes.ts renders the route table that serves these answers.
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

/** A name as a regular expression that matches it alone. */
export const literal = (name: string) => name.replace(/[\][.\\+*?^$(){}|]/g, "\\$&");
/** A branch name as a path may spell it: its slashes as they are or escaped. */
export const branchpat = (name: string) => name.replaceAll("/", "(/|%2F)");
/** GitHub's ASCII-only case changes, as GitHub spells a status in GraphQL. */
const upcase = (text: string) => text.replace(/[a-z]/g, (c) => c.toUpperCase());
/** `text` with `prefix` taken off its start, when it starts so. */
export const ltrimstr = (text: string, prefix: string) =>
  text.startsWith(prefix) ? text.slice(prefix.length) : text;
/** A path's last part. */
const nameOf = (path: string) => path.slice(path.lastIndexOf("/") + 1);
/** The path prefix of a directory's entries ("" for the tree's root). */
const under = (dir: string) => (dir === "" ? "" : `${dir}/`);

const FIXTURE_AUTHOR = { name: "Fixture", email: "fixture@example.invalid" };

/** Every answer GitHub gives from `world`, and the names of the world the routes match. */
export function headsAnswers(world: HeadsWorld) {
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
  return {
    repo,
    base,
    main,
    branches,
    trees,
    api,
    titled,
    pulls,
    first,
    others,
    numbers,
    ref,
    reviews,
    concluded,
    checksExit,
    pr,
    rest,
    checks,
    checkruns,
    jobOf,
    wfrun,
    wfruns,
    graphql,
    repository,
    repoview,
    branch,
    gitref,
    compare,
    pullcommits,
    pullfiles,
    knownshas,
    refsOf,
    fileOf,
    listOf,
    treeOf,
    repoPath,
    contentsAt,
    bytesOf,
    blobs,
    dirs,
    commitStdout,
    everyRun,
  };
}
