import {
  appendFileSync,
  mkdirSync,
  mkdtempSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import type { World } from "./heldout-case";

export type CommitMap = Record<string, string>;

function run(cwd: string, env: NodeJS.ProcessEnv, args: string[], input?: string): string {
  const result = Bun.spawnSync(args, {
    cwd,
    env,
    stdin: input === undefined ? undefined : Buffer.from(input),
    stdout: "pipe",
    stderr: "pipe",
  });
  if (result.exitCode !== 0)
    throw new Error(`${args.join(" ")}: ${result.stderr.toString().trim()}`);
  return result.stdout.toString().trimEnd();
}

function gitEnv(): NodeJS.ProcessEnv {
  return {
    ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))),
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_AUTHOR_NAME: "Scenario Worker",
    GIT_AUTHOR_EMAIL: "worker@example.invalid",
    GIT_COMMITTER_NAME: "Scenario Worker",
    GIT_COMMITTER_EMAIL: "worker@example.invalid",
  };
}

export function defaultBranch(world: World["git"]): string {
  const branches = world?.remote.branches ?? [];
  return branches.find(({ name }) => name === "main")?.name ?? branches[0]?.name ?? "main";
}

/** Case ids can be fictitious. Preserve the topology and descriptions, giving each commit its
 * own small file because the case schema specifies no trees. Use a stable author and date so
 * paired arms start with identical commit ids and trees. */
export function buildGitWorld(dir: string, world: World, stage = false): CommitMap {
  const fixture = world.git;
  const remote = path.join(dir, "remote.git");
  const workspace = path.join(dir, "workspace");
  const temp = path.join(dir, "tmp");
  const config = path.join(dir, "jj.toml");
  mkdirSync(workspace);
  mkdirSync(temp, { recursive: true });
  writeFileSync(
    config,
    '[user]\nname = "Scenario Worker"\nemail = "worker@example.invalid"\n[fsmonitor]\nbackend = "none"\n'
  );
  const env = {
    ...gitEnv(),
    JJ_CONFIG: config,
    GIT_AUTHOR_DATE: world.clock,
    GIT_COMMITTER_DATE: world.clock,
  };
  const git = (...args: string[]) => run(workspace, env, ["git", ...args]);
  const plumbing = (input: string, ...args: string[]) =>
    run(workspace, env, ["git", ...args], input);
  const jj = (...args: string[]) => run(workspace, env, ["jj", ...args]);
  const branch = defaultBranch(fixture);
  const remoteName = fixture?.remote.name ?? "origin";
  run(dir, env, ["git", "init", "-q", "--bare", "--object-format=sha1", "-b", branch, remote]);
  git("init", "-q", "--object-format=sha1", "-b", branch);
  const commits = new Map(
    fixture?.commits.map((commit, index) => [commit.id, { ...commit, index }])
  );
  if (commits.size !== (fixture?.commits.length ?? 0)) throw new Error("duplicate git commit id");
  const building = new Set<string>();
  const built = new Map<string, { sha: string; files: Map<string, string> }>();
  const build = (id: string) => {
    const previous = built.get(id);
    if (previous) return previous;
    if (building.has(id)) throw new Error(`git commit cycle at ${id}`);
    const commit = commits.get(id);
    if (!commit) throw new Error(`git parent ${id} is not in the case`);
    building.add(id);
    const parents = commit.parents.map((parent): { sha: string; files: Map<string, string> } =>
      build(parent)
    );
    const files = new Map(parents.flatMap((parent) => [...parent.files]));
    files.set(
      `change-${commit.index + 1}.txt`,
      plumbing(`${commit.description}\n`, "hash-object", "-w", "--stdin")
    );
    const tree = plumbing(
      [...files].map(([name, blob]) => `100644 blob ${blob}\t${name}\n`).join(""),
      "mktree"
    );
    const sha = plumbing(
      `${commit.description}\n`,
      "commit-tree",
      tree,
      ...parents.flatMap((parent) => ["-p", parent.sha])
    );
    const result = { sha, files };
    built.set(id, result);
    building.delete(id);
    return result;
  };
  for (const id of commits.keys()) build(id);
  const mapping = Object.fromEntries([...built].map(([id, { sha }]) => [id, sha]));
  writeFileSync(path.join(dir, "commit-map.json"), `${JSON.stringify(mapping, null, 2)}\n`);
  git("remote", "add", "--", remoteName, remote);
  // Populate the bare repository before installing the recorder. Local-only objects never enter it.
  for (const { name, head } of fixture?.remote.branches ?? []) {
    git("check-ref-format", `refs/heads/${name}`);
    run(remote, env, [
      "git",
      "fetch",
      "-q",
      "--no-tags",
      workspace,
      `${mapping[head]}:refs/heads/${name}`,
    ]);
  }
  const standing = fixture && mapping[fixture.workspace.standingOn];
  if (standing) {
    git("switch", "--detach", standing);
    for (const [index, id] of (fixture?.workspace.localOnly ?? []).entries()) {
      git("update-ref", `refs/heads/local-only/${index + 1}`, mapping[id] ?? "");
    }
  }
  jj("git", "init", "--colocate");
  if (fixture?.remote.branches.length) jj("git", "fetch", "--remote", remoteName);
  if (standing) {
    // git init imported the detached HEAD and made its child the jj working copy.
    if (jj("log", "-r", "@-", "--no-graph", "-T", "commit_id") !== standing) {
      throw new Error(`jj workspace does not stand on ${standing}`);
    }
  }
  writeFileSync(path.join(dir, "pushes.log"), "");
  writeFileSync(
    path.join(remote, "hooks", "post-receive"),
    `#!/bin/sh
while read -r old new ref; do
  printf '%s %s %s %s\\n' "$(/bin/date -u +%FT%T.%NZ)" "$ref" "$old" "$new" >>"$(dirname "$0")/../../pushes.log"
done
`,
    { mode: 0o755 }
  );
  if (stage) {
    const baseHead = fixture?.remote.branches.find(({ name }) => name === branch)?.head;
    const base = baseHead === undefined ? standing : mapping[baseHead];
    if (!base) throw new Error("D1 needs a git base commit");
    for (const name of ["stage-a", "stage-b"]) {
      const copy = path.join(temp, name);
      run(temp, env, ["git", "clone", "-q", "--no-local", "--no-checkout", workspace, copy]);
      run(copy, env, ["git", "switch", "--detach", base]);
      run(copy, env, ["git", "remote", "set-url", "origin", remote]);
    }
  }
  return mapping;
}

/** Translate full ids and unambiguous abbreviated ids, including those embedded in prose. */
export function translateCommits<T>(value: T, mapping: CommitMap): T {
  const ids = Object.entries(mapping);
  const translate = (item: unknown): unknown => {
    if (typeof item === "string")
      return item.replace(/[0-9a-f]{7,}/g, (word, offset: number) => {
        if (/[0-9a-f]/.test(item[offset - 1] ?? "")) return word;
        const matches = ids.filter(([id]) => id.startsWith(word));
        return matches.length === 1 ? matches[0]?.[1].slice(0, word.length) : word;
      });
    if (Array.isArray(item)) return item.map(translate);
    if (item !== null && typeof item === "object") {
      return Object.fromEntries(
        Object.entries(item).map(([key, field]) => [key, translate(field)])
      );
    }
    return item;
  };
  return translate(value) as T;
}

/** stage-job records the current checked-out files (tracked and non-ignored untracked files),
 * not HEAD. Both the index and new objects go into a disposable repository, not the source. */
export function stageJob(dir: string, job: string, source = process.cwd()) {
  if (!job.trim()) throw new Error("stage-job needs a job name");
  const root = realpathSync(source);
  const env = gitEnv();
  const actualRoot = run(root, env, ["git", "rev-parse", "--show-toplevel"]);
  if (root !== actualRoot) throw new Error("stage-job takes a checkout root");
  const candidate = mkdtempSync(path.join(dir, "tmp", "stage-index-"));
  try {
    run(candidate, env, ["git", "init", "--bare", "-q", "--object-format=sha1"]);
    const snapshot = ["git", `--git-dir=${candidate}`, `--work-tree=${root}`];
    // Start from HEAD so that tracked files are still picked up if the current ignore rules changed.
    const objects = run(root, env, [
      "git",
      "rev-parse",
      "--path-format=absolute",
      "--git-path",
      "objects",
    ]);
    const snapshotEnv = { ...env, GIT_ALTERNATE_OBJECT_DIRECTORIES: objects };
    run(root, snapshotEnv, [
      ...snapshot,
      "read-tree",
      run(root, env, ["git", "rev-parse", "HEAD"]),
    ]);
    run(root, snapshotEnv, [...snapshot, "add", "-u", "--", "."]);
    const untracked = run(root, env, ["git", "ls-files", "-z", "--others", "--exclude-standard"]);
    // Feed only the source checkout's non-ignored files, independent of the scratch index's ignores.
    if (untracked)
      run(
        root,
        snapshotEnv,
        [
          ...snapshot,
          "--literal-pathspecs",
          "add",
          "-f",
          "--pathspec-from-file=-",
          "--pathspec-file-nul",
        ],
        untracked
      );
    const tree = run(root, snapshotEnv, [...snapshot, "write-tree"]);
    const staged = run(root, snapshotEnv, [...snapshot, "ls-tree", "-r", "-z", "--name-only", tree])
      .split("\0")
      .filter(Boolean);
    if (staged.length === 0) throw new Error("stage-job snapshot has no files");
    const record = { at: new Date().toISOString(), job, tree, files: staged };
    appendFileSync(path.join(dir, "staging.jsonl"), `${JSON.stringify(record)}\n`);
    return record;
  } finally {
    rmSync(candidate, { recursive: true });
  }
}
