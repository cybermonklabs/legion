#!/usr/bin/env bun
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  rmdirSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { isLegionRole } from "@legion/contracts";
import { workerPane } from "./daemon-pane";
import { recorderScript } from "./diff-recorder";
import { buildGitWorld, defaultBranch, stageJob, translateCommits } from "./git-world";
import { caseFileGaps, parseHeldoutFile, parseSkillUseFile, type World } from "./heldout-case";

/** The case file `file` in the shape of `set`; throws when the file holds another set. */
function loadCaseFile(file: string, set: string) {
  const text = readFileSync(file, "utf8");
  const input = set === "skilluse" ? parseSkillUseFile(text) : parseHeldoutFile(text);
  if (input.set !== set) throw new Error(`${file} holds ${input.set}, not ${set}`);
  return input;
}

export function loadCase(file: string, set: string, id: string) {
  const found = loadCaseFile(file, set).cases.find((entry) => entry.id === id);
  if (!found) throw new Error(`${file} has no ${set} case ${id}`);
  return found;
}

/** What a batch of case `id` checks before its first run: the case parses and is in `file`, and
 * the gaps of the whole file, which the batch refuses to start on, as `heldout-case.ts gaps`
 * counts them (caseFileGaps): none for a development set. */
export function batchGaps(file: string, set: string, id: string): string[] {
  const input = loadCaseFile(file, set);
  if (!input.cases.some((entry) => entry.id === id))
    throw new Error(`${file} has no ${set} case ${id}`);
  return caseFileGaps(input);
}

interface Route {
  match: string;
  stdout?: unknown;
  stderr?: string;
  exit?: number;
  pages?: unknown[];
}

/** Compile the case's GitHub world to the recording stand-in's first-match route list. Each pull
 * request is answered as GitHub answers it: `gh pr view` and the REST read with its node id
 * (`nodeId`, else `PR_<number>`), its merge state and its auto-merge request where the world
 * states them (GraphQL's `mergeStateStatus` and `autoMergeRequest`, REST's `mergeable_state` and
 * `auto_merge`), GraphQL's read of it (`repository.pullRequest`), `gh pr diff` with its `diff`,
 * and a merge (`gh pr merge`, `PUT …/pulls/<n>/merge`) with its `mergeResult`. The repository's
 * read (`GET repos/<repository>`) answers the world's `settings` over the stand-in's own members,
 * and `GET repos/<repository>/rules/branches/<branch>` each branch's `branchRules`, `[]` for a
 * branch they do not name, as GitHub answers a branch with no rules. A read of a fact no part
 * holds matches no route, and the stand-in answers it 404 as GitHub does. */
export function githubFixtures(github: World["github"], branch: string, issue: string) {
  const routes: Route[] = [];
  if (github === null) return { routes };
  const { pullRequests, mergeQueue, repository } = github;
  const pulls = pullRequests.map((pr) => {
    const url = `https://github.com/${repository}/pull/${pr.number}`;
    const reviews = pr.reviews.map((review) => ({
      id: review.id,
      author: { login: review.author },
      user: { login: review.author },
      body: review.body,
      state: review.state,
      commit_id: review.commit,
      commit: { oid: review.commit },
      submitted_at: review.at,
      submittedAt: review.at,
    }));
    const checks = pr.checks.filter(({ commit }) => commit === pr.head.sha);
    const rollup = checks.map((check) => ({
      name: check.name,
      status: check.status.toUpperCase(),
      conclusion: check.conclusion?.toUpperCase() ?? "",
      __typename: "CheckRun",
    }));
    const approval = reviews
      .filter(({ state }) => ["APPROVED", "CHANGES_REQUESTED"].includes(state))
      .at(-1)?.state;
    return {
      number: pr.number,
      url,
      html_url: url,
      id: pr.nodeId ?? `PR_${pr.number}`,
      node_id: pr.nodeId ?? `PR_${pr.number}`,
      title: pr.title,
      body: pr.body,
      state: pr.state.toUpperCase(),
      draft: pr.draft,
      isDraft: pr.draft,
      merged: pr.state === "merged",
      head: pr.head,
      base: { ref: pr.base },
      headRefName: pr.head.ref,
      headRefOid: pr.head.sha,
      baseRefName: pr.base,
      mergeCommit: pr.mergeCommit && { oid: pr.mergeCommit },
      merge_commit_sha: pr.mergeCommit,
      reviews,
      reviewDecision: approval ?? "",
      statusCheckRollup: rollup,
      mergeable: "MERGEABLE",
      ...(pr.mergeStateStatus === undefined
        ? {}
        : {
            mergeStateStatus: pr.mergeStateStatus,
            mergeable_state: pr.mergeStateStatus.toLowerCase(),
          }),
      ...(pr.autoMergeRequest === undefined
        ? {}
        : { autoMergeRequest: pr.autoMergeRequest, auto_merge: pr.autoMergeRequest }),
    };
  });
  for (const entry of mergeQueue) {
    const op = RegExp.escape(entry.request);
    routes.push({
      match: `^gh .*${op}`,
      stdout: entry.detail || { state: entry.answer },
      exit: entry.answer === "refused" ? 1 : 0,
    });
  }
  for (const pr of pullRequests) {
    const view = pulls.find(({ number }) => number === pr.number);
    const n = pr.number;
    const rest = `repos/${RegExp.escape(repository)}/pulls/${n}`;
    const selector = `(?:${n}|${RegExp.escape(pr.head.ref)}|${RegExp.escape(view?.url ?? "")})`;
    routes.push(
      { match: `^gh pr view(?=.* ${selector}(?: |$))`, stdout: view },
      {
        match: `^gh pr checks(?=.* ${selector}(?: |$))`,
        stdout: view?.statusCheckRollup
          .map(
            (c) =>
              `${c.name}\t${c.conclusion === "SUCCESS" ? "pass" : c.conclusion === "" ? "pending" : "fail"}`
          )
          .join("\n"),
      },
      ...(pr.diff === undefined
        ? []
        : [{ match: `^gh pr diff(?=.* ${selector}(?: |$))`, stdout: pr.diff }]),
      ...(pr.mergeResult === undefined
        ? []
        : [
            { match: `^gh pr merge(?=.* ${selector}(?: |$))`, stdout: pr.mergeResult },
            {
              match: `^gh api (?=.*${rest}/merge(?: |$))(?=.*(?:-X|--method)[ =]PUT(?: |$))`,
              stdout: pr.mergeResult,
            },
          ]),
      { match: `^gh api .*${rest}/reviews(?: |$)`, stdout: view?.reviews },
      { match: `^gh api .*${rest}/comments(?: |$)`, stdout: [] },
      {
        match: `^gh api .*${rest}(?: |$)`,
        stdout: { ...view, state: pr.state === "open" ? "open" : "closed" },
      },
      {
        match: `^gh pr (edit|comment|review|ready|merge)(?=.* ${selector}(?: |$))`,
        stdout: view?.url,
      }
    );
    const pages = (pr.reviewThreads.length ? pr.reviewThreads : [[]]).map((page, i, all) => ({
      data: {
        repository: {
          pullRequest: {
            reviewThreads: {
              pageInfo: {
                hasNextPage: i + 1 < all.length,
                endCursor: i + 1 < all.length ? `thread-page-${i + 1}` : null,
              },
              nodes: page.map((thread) => ({
                id: thread.id,
                path: thread.path,
                line: thread.line,
                isResolved: thread.resolved,
                isOutdated: thread.outdated,
                comments: {
                  nodes: thread.comments.map((c) => ({
                    body: c.body,
                    createdAt: c.at,
                    author: { login: c.author },
                  })),
                },
              })),
            },
          },
        },
      },
    }));
    const numbered = pullRequests.length === 1 ? "" : `(?=.*(?:number:\\s*|number=)${n}\\b)`;
    routes.push(
      { match: `^gh api .*graphql(?=.*reviewThreads)${numbered}`, pages },
      // GraphQL's read of the pull request (`repository.pullRequest`): no mutation, which the
      // merge queue's routes and gh-standin's change rules answer.
      {
        match: `^gh api .*graphql(?!.*\\bmutation\\b)(?=.*\\bpullRequest\\s*\\()${numbered}`,
        stdout: {
          data: {
            repository: {
              pullRequest: view && {
                id: view.id,
                number: view.number,
                url: view.url,
                title: view.title,
                body: view.body,
                state: view.state,
                isDraft: view.isDraft,
                merged: view.merged,
                headRefName: view.headRefName,
                headRefOid: view.headRefOid,
                baseRefName: view.baseRefName,
                mergeCommit: view.mergeCommit,
                mergeable: view.mergeable,
                reviewDecision: view.reviewDecision,
                ...(pr.mergeStateStatus === undefined
                  ? {}
                  : { mergeStateStatus: pr.mergeStateStatus }),
                ...(pr.autoMergeRequest === undefined
                  ? {}
                  : { autoMergeRequest: pr.autoMergeRequest }),
              },
            },
          },
        },
      }
    );
  }
  for (const sha of new Set(pullRequests.flatMap((pr) => pr.checks.map(({ commit }) => commit)))) {
    const runs = pullRequests.flatMap((pr) => pr.checks.filter(({ commit }) => commit === sha));
    routes.push({
      match: `^gh api .*commits/${sha}/check-runs`,
      stdout: { total_count: runs.length, check_runs: runs },
    });
  }
  const state = "(?:--state[ =]|-s )";
  const head = "(?:--head[ =]|-H )";
  for (const filter of ["open", "closed", "merged", "all"]) {
    const selected = pulls.filter((pr) => filter === "all" || pr.state.toLowerCase() === filter);
    const stateMatch =
      filter === "open"
        ? `(?!.* ${state}(?:closed|merged|all)(?: |$))`
        : `(?=.* ${state}${filter}(?: |$))`;
    for (const ref of new Set(selected.map((pr) => pr.headRefName))) {
      routes.push({
        match: `^gh pr list${stateMatch}(?=.* ${head}${RegExp.escape(ref)}(?: |$))`,
        stdout: selected.filter((pr) => pr.headRefName === ref),
      });
    }
    routes.push({ match: `^gh pr list${stateMatch}(?!.* ${head})`, stdout: selected });
  }
  const newNumber = Math.max(0, ...pullRequests.map(({ number }) => number)) + 1;
  const url = `https://github.com/${repository}/pull/${newNumber}`;
  const repo = RegExp.escape(repository);
  // Creating a fixture PR returns its URL. The daemon verifies existence from the recorded call.
  routes.push(
    { match: "^gh pr list", stdout: [] },
    { match: "^gh pr create", stdout: url },
    {
      match: `^gh api (?=.*repos/${repo}/pulls(?: |$))(?=.*(?:-X|--method)[ =]POST(?: |$))`,
      stdout: { html_url: url, number: newNumber },
    },
    {
      match: `^gh api .*repos/${repo}/pulls(?: |$)`,
      stdout: pulls.filter((p) => p.state === "OPEN"),
    },
    // The repository's read: the world's settings over the stand-in's own members.
    {
      match: `^gh api .*repos/${repo}(?: |$)`,
      stdout: { default_branch: branch, full_name: repository, ...github.settings },
    },
    // A branch's rules, `[]` for a branch the world names none of, once the world states them.
    ...(github.branchRules ?? []).map((rules) => ({
      match: `^gh api .*repos/${repo}/rules/branches/${RegExp.escape(rules.branch)}(?:[ ?]|$)`,
      stdout: rules.rules,
    })),
    ...(github.branchRules === undefined
      ? []
      : [{ match: `^gh api .*repos/${repo}/rules/branches/[^ ?]+(?:[ ?]|$)`, stdout: [] }]),
    { match: "^gh (api user|auth status)", stdout: { login: "rig[bot]" } }
  );
  const issuePr = pulls.find((pr) => pr.headRefName === `legion/${issue}`);
  if (issuePr) {
    const world = pullRequests.find((pr) => pr.number === issuePr.number);
    routes.push({ match: "^gh pr view( |$)", stdout: issuePr });
    if (world?.diff !== undefined) routes.push({ match: "^gh pr diff( |$)", stdout: world.diff });
    if (world?.mergeResult !== undefined)
      routes.push({ match: "^gh pr merge( |$)", stdout: world.mergeResult });
    routes.push({ match: "^gh pr (edit|comment|review|ready)( |$)", stdout: issuePr.url });
  }
  return { routes };
}

/** The rig's own repository: daemon-pane.go's rigRepository and rig.sh's worker_repo. */
const RIG_REPOSITORY = "example/widgets";

/** The repository a case's issue is on and that repository's default branch, the one pair both
 * the pane and the daemon stand-in are given (launch.json): a real daemon tells a launch the
 * repository and default branch of its record of the issue (specs.workerSpawnSpec, from
 * workflowRuntime.launchRepository), the very fields its read of that record answers (`repo`,
 * `defaultBranch`), and refuses a launch on an issue that has no repository. The repository is
 * the world's GitHub repository, or the rig's own where the world has no GitHub; the branch is the
 * default branch of the world's remote (git-world.ts), its HEAD, which a world with no GitHub has
 * as well. */
export function caseRepository(world: World): { repository: string; defaultBranch: string } {
  return {
    repository: world.github?.repository ?? RIG_REPOSITORY,
    defaultBranch: defaultBranch(world.git),
  };
}

/** Shuffle label pairs independently of launch time. Hashing seed + pair + label makes ordering
 * repeatable even if the shell has a different PRNG. Callers run the printed rows serially. */
export function armOrder(seed: string, pair: string, labels: string[]): string[] {
  return labels
    .map((label) => ({
      label,
      order: new Bun.CryptoHasher("sha256")
        .update(JSON.stringify([seed, pair, label]))
        .digest("hex"),
    }))
    .sort((a, b) => a.order.localeCompare(b.order))
    .map(({ label }) => label);
}

export function prepareCase(dir: string, file: string, set: string, id: string): void {
  const scenario = loadCase(file, set, id);
  const { world } = scenario;
  const agent = "agent" in scenario ? scenario.agent : undefined;
  const stage = scenario.kind === "D1";
  mkdirSync(dir, { recursive: true });
  mkdirSync(path.join(dir, "bin"), { recursive: true });
  mkdirSync(path.join(dir, "sessions"), { recursive: true });
  const commitMap = buildGitWorld(dir, world, stage);
  const launch = translateCommits(world, commitMap);
  const text = (name: string, value: string) => writeFileSync(path.join(dir, name), `${value}\n`);
  text("world.json", JSON.stringify(world));
  text("launch-world.json", JSON.stringify(launch));
  text("case.txt", id);
  text("clock.txt", world.clock);
  const where = caseRepository(launch);
  text(
    "fixtures.json",
    JSON.stringify(githubFixtures(launch.github, where.defaultBranch, launch.dispatch.issue.key))
  );
  for (const log of ["daemon.log", "calls.jsonl", "staging.jsonl", "deployments.jsonl"])
    writeFileSync(path.join(dir, log), "");
  const workspace = path.join(dir, "cwd");
  if (existsSync(workspace)) rmdirSync(workspace);
  for (const alias of ["cwd", "ws"]) symlinkSync("workspace", path.join(dir, alias));
  const task = translateCommits(scenario.task, commitMap);
  const clock = `The simulated clock at the start of this run is ${world.clock}. Use it for dates in this task.`;
  const staged = stage
    ? `\nThe two scratch copies are $TMPDIR/stage-a and $TMPDIR/stage-b. To stage a Job, run stage-job NAME [CHECKOUT] [--bundle <path>] [--report <path>] (CHECKOUT defaults to the current checkout; --bundle names the Job's input bundle and --report the path the Job writes its report to). No Job runs.`
    : "";
  const production = launch.production ? `\nProduction probe: ${launch.production.probe}` : "";
  text(
    "prompt.txt",
    agent === undefined
      ? `${task}\n\n${clock}${staged}${production}`
      : `Make exactly one foreground task(agent=${JSON.stringify(agent)}) call. Give the agent the brief below and the location of its starting world (${path.join(dir, "launch-world.json")}). Return its report.\n\n${task}\n\n${clock}`
  );
  const role = scenario.role ?? null;
  text(
    "launch.json",
    JSON.stringify({
      role,
      agent: agent ?? null,
      plain: agent !== undefined || ("session" in scenario && scenario.session === "plain-envoy"),
      tools: agent === undefined ? scenario.tools : ["task"],
      stage,
      ...where,
    })
  );
  if (stage)
    writeFileSync(
      path.join(dir, "bin", "stage-job"),
      `#!/bin/sh\nexec ${sh(process.execPath)} ${sh(import.meta.path)} stage ${sh(dir)} "$@"\n`,
      { mode: 0o755 }
    );
  // Each `jj` and `git` the role runs reaches the real program through diff-recorder.ts, which
  // records a diff read; any other call goes to the real program at once. A world with a workspace
  // and no real program to record in front of would record no diff read, which a grade would read
  // as none made, so it fails the setup.
  const bin = path.resolve(dir, "bin");
  const searched = (process.env.PATH ?? "")
    .split(path.delimiter)
    .filter((entry) => entry !== "" && path.resolve(entry) !== bin)
    .join(path.delimiter);
  for (const program of ["jj", "git"] as const) {
    const real = Bun.which(program, { PATH: searched });
    if (real === null) {
      if (world.git !== null)
        throw new Error(
          `no ${program} on PATH: the case world has a workspace, and a run's diff reads are recorded in front of the real ${program}`
        );
      continue;
    }
    writeFileSync(path.join(bin, program), recorderScript(dir, program, real, process.execPath), {
      mode: 0o755,
    });
  }
  if (launch.production !== null) {
    const probe = launch.production.probe;
    // A URL or the curl command in the case addresses only the frozen revision, not production.
    const url = /https?:\/\/[^\s"']+/.exec(probe)?.[0];
    if (!url) throw new Error(`production probe must name its HTTP URL: ${probe}`);
    // A call that names the probe's address is answered by this script's `probe` mode, which
    // records the probe before it answers; a call that is no probe records nothing.
    const answer = [
      process.execPath,
      import.meta.path,
      "probe",
      dir,
      url,
      launch.production.revision,
    ];
    writeFileSync(
      path.join(bin, "curl"),
      [
        "#!/bin/sh",
        "for arg do",
        `  if [ "$arg" = ${sh(url)} ]; then exec ${answer.map(sh).join(" ")} "$@"; fi`,
        "done",
        "echo 'curl: no route in the case world' >&2",
        "exit 22",
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
  }
}

function sh(value: string): string {
  return `'${value.replaceAll("'", `'\\''`)}'`;
}

/** The pane of the case's claim on `repository` and its default branch `branch`, the pair the
 * daemon stand-in was given (launch.json's, caseRepository). */
async function casePane(
  dir: string,
  checkout: string,
  port: string,
  profile: string,
  project: string,
  role: string,
  issue: string,
  tree: string,
  repository: string,
  branch: string
): Promise<void> {
  if (role !== "controller" && !isLegionRole(role)) throw new Error(`not a claim role: ${role}`);
  const base = Object.fromEntries(
    readFileSync(path.join(dir, "pane.env"), "utf8")
      .trimEnd()
      .split("\n")
      .map((line) => {
        const equals = line.indexOf("=");
        if (equals <= 0) throw new Error("pane.env must have KEY=value lines");
        return [line.slice(0, equals), line.slice(equals + 1)];
      })
  );
  const claim = {
    project,
    issue: role === "controller" ? "" : issue,
    role: role as Parameters<typeof workerPane>[2]["role"],
    ...(role === "controller" ? { controller: "headless" as const } : { tree }),
    repository,
    defaultBranch: branch,
  };
  const pane = await workerPane({ rig: dir, port: Number(port), profile }, base, claim, {
    daemonModule: path.join(checkout, "packages", "daemon"),
    ghPath: path.join(dir, "bin", "gh"),
    systemPrompt: true,
  });
  if (pane.systemPromptArgument === undefined)
    throw new Error("the daemon composed no system prompt");
  writeFileSync(path.join(dir, "system-args"), pane.systemPromptArgument);
  writeFileSync(
    path.join(dir, "worker.env"),
    Object.entries(pane.env)
      .map(([key, value]) => {
        if (value.includes("\n")) throw new Error(`${key} is not one pane.env line`);
        return `${key}=${value}\n`;
      })
      .join(""),
    { mode: 0o600 }
  );
}

if (import.meta.main) {
  const [mode, ...args] = Bun.argv.slice(2);
  const [a, b, c, d] = args;
  if (mode === "prepare" && a && b && c && d && args.length === 4) prepareCase(a, b, c, d);
  else if (mode === "check" && a && b && c && args.length === 3) {
    // Each gap on stderr, as `heldout-case.ts gaps` writes one, then the refusal.
    const gaps = batchGaps(a, b, c);
    for (const gap of gaps) console.error(`${a}: ${gap}`);
    if (gaps.length > 0) {
      console.error(`${a}: the ${b} batch does not start while the file has a gap`);
      process.exit(1);
    }
  } else if (mode === "stage" && a) {
    // stage-job NAME [CHECKOUT] [--bundle <path>] [--report <path>], after the run directory.
    const { values, positionals } = parseArgs({
      args: args.slice(1),
      options: { bundle: { type: "string" }, report: { type: "string" } },
      allowPositionals: true,
    });
    const [job, checkout, ...extra] = positionals;
    if (job === undefined || extra.length > 0)
      throw new Error(
        "usage: stage-job NAME [CHECKOUT] [--bundle <path>] [--report <path>]: one name, and one checkout at most"
      );
    console.log(JSON.stringify(stageJob(a, job, checkout, values)));
  } else if (mode === "probe" && a && b && c) {
    // The `curl` stand-in's answer to a probe: one line of calls.jsonl, as rig.sh's other
    // stand-ins write theirs, written before the revision is printed, so a probe answered is a
    // probe recorded, and one whose line cannot be written fails with no answer.
    appendFileSync(
      path.join(a, "calls.jsonl"),
      `${JSON.stringify({ at: new Date().toISOString(), as: "curl", argv: args.slice(3), exit: 0, target: b, revision: c })}\n`
    );
    console.log(c);
  } else if (mode === "order" && a && b && c) console.log(armOrder(a, b, args.slice(2)).join("\n"));
  else if (mode === "pane" && args.length === 10 && a && b && c && d) {
    const [, , , , project, role, issue, tree, repository, branch] = args;
    if (!project || !role || !issue || !tree || !repository || !branch)
      throw new Error("pane needs project, claim role, issue, tree, repository and default branch");
    await casePane(a, b, c, d, project, role, issue, tree, repository, branch);
  } else
    throw new Error(
      "usage: case-world.ts prepare <run dir> <case file> <set> <id> | check <case file> <set> <id> | stage <run dir> <job> [checkout] [--bundle <path>] [--report <path>] | probe <run dir> <address> <revision> [curl argv...] | order <seed> <pair> <labels...> | pane <run dir> <checkout> <port> <profile> <project> <claim role> <issue> <tree> <repository> <default branch>"
    );
}
