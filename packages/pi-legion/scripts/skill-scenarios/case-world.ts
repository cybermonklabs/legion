#!/usr/bin/env bun
import {
  existsSync,
  mkdirSync,
  readFileSync,
  rmdirSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { isLegionRole } from "@legion/contracts";
import { workerPane } from "./daemon-pane";
import { buildGitWorld, defaultBranch, stageJob, translateCommits } from "./git-world";
import {
  HELDOUT_ROLES,
  heldoutGaps,
  legionAgents,
  legionPartition,
  parseHeldoutFile,
  parseSkillUseFile,
  skillUseGaps,
  World,
} from "./heldout-case";

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
 * the gaps of the whole file, which the batch refuses to start on. A held-out set's gaps are
 * heldoutGaps and a skill-use set's skillUseGaps, counted as `heldout-case.ts gaps` counts them,
 * against this checkout's agents and Legion skill partition. A development set holds a few cases
 * and no population, so it has none. */
export function batchGaps(file: string, set: string, id: string): string[] {
  const input = loadCaseFile(file, set);
  if (!input.cases.some((entry) => entry.id === id))
    throw new Error(`${file} has no ${set} case ${id}`);
  if (input.set === "skilluse") return skillUseGaps(legionPartition(), input.cases);
  if (input.set === "heldout") return heldoutGaps(HELDOUT_ROLES, legionAgents(), input.cases);
  return [];
}

interface Route {
  match: string;
  stdout?: unknown;
  stderr?: string;
  exit?: number;
  pages?: unknown[];
}

/** Compile the case's GitHub world to the recording stand-in's first-match route list. */
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
      id: `PR_${pr.number}`,
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
    routes.push({
      match: `^gh api .*graphql(?=.*reviewThreads)${pullRequests.length === 1 ? "" : `(?=.*(?:number:\\s*|number=)${n}\\b)`}`,
      pages,
    });
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
    {
      match: `^gh api .*repos/${repo}(?: |$)`,
      stdout: { default_branch: branch, full_name: repository },
    },
    { match: "^gh (api user|auth status)", stdout: { login: "rig[bot]" } }
  );
  const issuePr = pulls.find((pr) => pr.headRefName === `legion/${issue}`);
  if (issuePr) {
    routes.push({ match: "^gh pr view( |$)", stdout: issuePr });
    routes.push({ match: "^gh pr (edit|comment|review|ready)( |$)", stdout: issuePr.url });
  }
  return { routes };
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
  text(
    "fixtures.json",
    JSON.stringify(
      githubFixtures(launch.github, defaultBranch(launch.git), launch.dispatch.issue.key)
    )
  );
  for (const log of ["daemon.log", "calls.jsonl", "staging.jsonl", "deployments.jsonl"])
    writeFileSync(path.join(dir, log), "");
  const workspace = path.join(dir, "cwd");
  if (existsSync(workspace)) rmdirSync(workspace);
  for (const alias of ["cwd", "ws"]) symlinkSync("workspace", path.join(dir, alias));
  const task = translateCommits(scenario.task, commitMap);
  const clock = `The simulated clock at the start of this run is ${world.clock}. Use it for dates in this task.`;
  const staged = stage
    ? `\nThe two scratch copies are $TMPDIR/stage-a and $TMPDIR/stage-b. To stage a Job, run stage-job NAME [CHECKOUT] (default: current checkout). No Job runs.`
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
    })
  );
  if (stage)
    writeFileSync(
      path.join(dir, "bin", "stage-job"),
      `#!/bin/sh\nexec ${sh(process.execPath)} ${sh(import.meta.path)} stage ${sh(dir)} "$@"\n`,
      { mode: 0o755 }
    );
  if (launch.production !== null) {
    const probe = launch.production.probe;
    // A URL or the curl command in the case addresses only the frozen revision, not production.
    const url = /https?:\/\/[^\s"']+/.exec(probe)?.[0];
    if (!url) throw new Error(`production probe must name its HTTP URL: ${probe}`);
    writeFileSync(
      path.join(dir, "bin", "curl"),
      `#!/bin/sh\nfor arg do\n  if [ "$arg" = ${sh(url)} ]; then printf '%s\\n' ${sh(launch.production.revision)}; exit 0; fi\ndone\necho 'curl: no route in the case world' >&2\nexit 22\n`,
      { mode: 0o755 }
    );
  }
}

function sh(value: string): string {
  return `'${value.replaceAll("'", `'\\''`)}'`;
}

async function casePane(
  dir: string,
  checkout: string,
  port: string,
  profile: string,
  project: string,
  role: string,
  issue: string,
  tree: string
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
  // The pane names the issue's repository and its default branch as the daemon's record does: the
  // case world's, where the world has a GitHub repository.
  const world = World.parse(JSON.parse(readFileSync(path.join(dir, "launch-world.json"), "utf8")));
  const claim = {
    project,
    issue: role === "controller" ? "" : issue,
    role: role as Parameters<typeof workerPane>[2]["role"],
    ...(role === "controller" ? { controller: "headless" as const } : { tree }),
    ...(world.github === null
      ? {}
      : { repository: world.github.repository, defaultBranch: defaultBranch(world.git) }),
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
  } else if (mode === "stage" && a && b && args.length <= 3)
    console.log(JSON.stringify(stageJob(a, b, c)));
  else if (mode === "order" && a && b && c) console.log(armOrder(a, b, args.slice(2)).join("\n"));
  else if (mode === "pane" && args.length === 8 && a && b && c && d) {
    const [, , , , project, role, issue, tree] = args;
    if (!project || !role || !issue || !tree)
      throw new Error("pane needs project, claim role, issue and tree");
    await casePane(a, b, c, d, project, role, issue, tree);
  } else
    throw new Error(
      "usage: case-world.ts prepare <run dir> <case file> <set> <id> | check <case file> <set> <id> | stage <run dir> <job> [checkout] | order <seed> <pair> <labels...>"
    );
}
