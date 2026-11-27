// The Git of a scenario's case world (buildGitWorld), the jj operation record of a run's workspace
// (recordWorkspace), and the staging recorder of D1 (stageJob). Lane D's replay starts each run
// from the Git `build` makes of the run's world.json, with the commit ids, parents and trees the run
// started from, and moves its workspace as the run's `record` says the run's workspace moved:
//   bun git-world.ts build <dir> <world file>   the case world's Git in <dir>, with commit-map.json
//   bun git-world.ts record <workspace> <dir>   workspace-ops.jsonl and workspace.pack in <dir>
import {
  appendFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { World } from "./heldout-case";

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

/** `command`'s words, as a shell splits a plain command line: blanks between words, and single
 * and double quotes around a word's parts. */
function shellWords(command: string): string[] {
  const words: string[] = [];
  let word: string | undefined;
  let quote: string | undefined;
  for (const char of command) {
    if (quote !== undefined) {
      if (char === quote) quote = undefined;
      else word += char;
    } else if (char === "'" || char === '"') {
      quote = char;
      word ??= "";
    } else if (/\s/.test(char)) {
      if (word !== undefined) words.push(word);
      word = undefined;
    } else word = (word ?? "") + char;
  }
  if (word !== undefined) words.push(word);
  return words;
}

/** A test name `go test -run <pattern>` selects: the pattern's first alternative of its top
 * level (before any `/`), its letters, digits and `_`, with `Test` in front where it has none;
 * undefined where that name does not match the pattern. */
function goTestName(pattern: string | undefined): string | undefined {
  if (pattern === undefined || pattern === "") return "TestCheck";
  const top = pattern.split("/")[0] ?? "";
  const first = (
    top
      .replace(/^\^|\$$/g, "")
      .replace(/[()]/g, "")
      .split("|")[0] ?? ""
  ).replace(/[^A-Za-z0-9_]/g, "");
  const name = first.startsWith("Test") ? first : `Test${first}`;
  try {
    return new RegExp(top).test(name) ? name : undefined;
  } catch {
    return undefined;
  }
}

/** The minimal Go module a world's `go test` checks run green in: for each `check` block of the
 * world's documents whose `run` is a `go test` command, go.mod at the root and, in each package
 * directory the command names, a source file and one test, empty and so passing, whose name its
 * `-run` pattern selects. A check of any other command shape, or one whose pattern names no test
 * goTestName can write, gets no code. Paths to file text. */
export function goCheckModule(world: World): Map<string, string> {
  const tests = new Map<string, Set<string>>();
  for (const { body } of world.dispatch.documents) {
    for (const [, attributes = ""] of body.matchAll(/:::check\{([^}]*)\}/g)) {
      const run = /(?:^|\s)run="([^"]*)"/.exec(attributes)?.[1];
      const words = shellWords(run ?? "");
      if (words[0] !== "go" || words[1] !== "test") continue;
      let pattern: string | undefined;
      const packages: string[] = [];
      for (let i = 2; i < words.length; i += 1) {
        const word = words[i] ?? "";
        if (word === "-run") pattern = words[++i];
        else if (word.startsWith("-run=")) pattern = word.slice("-run=".length);
        else if (word === "." || word.startsWith("./")) packages.push(word);
      }
      const name = goTestName(pattern);
      if (name === undefined) continue;
      for (const pkg of packages.length === 0 ? ["."] : packages) {
        const dir = pkg
          .replace(/\/\.\.\.$/, "")
          .replace(/^\.\/?/, "")
          .replace(/\/+$/, "");
        tests.set(dir, (tests.get(dir) ?? new Set()).add(name));
      }
    }
  }
  const files = new Map<string, string>();
  if (tests.size === 0) return files;
  files.set("go.mod", "module example.invalid/caseworld\n\ngo 1.22\n");
  for (const [dir, names] of tests) {
    const pkg = (dir.split("/").at(-1) || "caseworld").replace(/[^A-Za-z0-9_]/g, "_").toLowerCase();
    const at = (file: string) => (dir === "" ? file : `${dir}/${file}`);
    files.set(
      at("doc.go"),
      `// Package ${pkg} stands in for the code this world's checks read.\npackage ${pkg}\n`
    );
    files.set(
      at(`${pkg}_check_test.go`),
      `package ${pkg}\n\nimport "testing"\n${[...names].map((name) => `\nfunc ${name}(t *testing.T) {}\n`).join("")}`
    );
  }
  return files;
}

/** Case ids can be fictitious and the schema holds no trees. Preserve the topology and the
 * descriptions, and give each commit a tree: CHANGES.md, one line for the commit and each commit
 * under it, and, at every commit that is off the default branch's first-parent line or above one
 * that is, goCheckModule's module, so a `go test` check runs green at the role's head as a real
 * tree's would. Use a stable author and date so paired arms start with identical commit ids and
 * trees. No bookmark names a local-only commit: the workspace stands on it. A local
 * `legion/<issue>` bookmark tracks the remote's branch only where the remote holds it. */
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
  const commits = new Map(fixture?.commits.map((commit) => [commit.id, commit]));
  if (commits.size !== (fixture?.commits.length ?? 0)) throw new Error("duplicate git commit id");
  // The default branch's first-parent line: the base no generated code is in.
  const base = new Set<string>();
  for (
    let id = fixture?.remote.branches.find(({ name }) => name === branch)?.head;
    id !== undefined && !base.has(id);
    id = commits.get(id)?.parents[0]
  )
    base.add(id);
  const module = goCheckModule(world);
  const blob = (text: string) => plumbing(text, "hash-object", "-w", "--stdin");
  const moduleBlobs = new Map([...module].map(([file, text]) => [file, blob(text)]));
  const index = path.join(temp, "case-world.index");
  const writeTree = (files: Map<string, string>) => {
    const indexEnv = { ...env, GIT_INDEX_FILE: index };
    rmSync(index, { force: true });
    run(
      workspace,
      indexEnv,
      ["git", "update-index", "--add", "--index-info"],
      [...files].map(([file, sha]) => `100644 ${sha}\t${file}\n`).join("")
    );
    return run(workspace, indexEnv, ["git", "write-tree"]);
  };
  interface Built {
    sha: string;
    changes: string[];
    module: boolean;
  }
  const building = new Set<string>();
  const built = new Map<string, Built>();
  const build = (id: string): Built => {
    const previous = built.get(id);
    if (previous) return previous;
    if (building.has(id)) throw new Error(`git commit cycle at ${id}`);
    const commit = commits.get(id);
    if (!commit) throw new Error(`git parent ${id} is not in the case`);
    building.add(id);
    const parents = commit.parents.map(build);
    const changes = [
      ...new Set([...parents.flatMap((parent) => parent.changes), `- ${commit.description}`]),
    ];
    // Off the default branch's first-parent line (any commit but a root, where the remote names
    // no default branch), or above a commit that is.
    const offBase = base.size > 0 ? !base.has(id) : parents.length > 0;
    const carries = offBase || parents.some((parent) => parent.module);
    const files = new Map([["CHANGES.md", blob(`# Changes\n\n${changes.join("\n")}\n`)]]);
    if (carries) for (const [file, sha] of moduleBlobs) files.set(file, sha);
    const sha = plumbing(
      `${commit.description}\n`,
      "commit-tree",
      writeTree(files),
      ...parents.flatMap((parent) => ["-p", parent.sha])
    );
    const result = { sha, changes, module: carries };
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
    for (const id of fixture?.workspace.localOnly ?? []) {
      const reachable = Bun.spawnSync(
        ["git", "merge-base", "--is-ancestor", mapping[id] ?? "", standing],
        {
          cwd: workspace,
          env,
        }
      );
      if (reachable.exitCode !== 0)
        throw new Error(
          `local-only commit ${id} is not under the commit the workspace stands on, so no working copy keeps it`
        );
    }
    git("switch", "--detach", standing);
  }
  jj("git", "init", "--colocate");
  if (fixture?.remote.branches.length) jj("git", "fetch", "--remote", remoteName);
  const own = `legion/${world.dispatch.issue.key}`;
  if (fixture?.remote.branches.some(({ name }) => name === own))
    jj("bookmark", "track", own, "--remote", remoteName);
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

/** The directory variables the role is given for the run's own directories, each with its
 * subdirectory inside the run directory, in the order a tie between two that name one directory is
 * broken. */
const RUN_DIRECTORIES = [
  ["LEGION_WORKSPACE", "workspace"],
  ["TMPDIR", "tmp"],
  ["HOME", "home"],
] as const;

/** `root` with the run's own directory that holds it written by the name the role was given for
 * it: the deepest directory one of RUN_DIRECTORIES names in `env` or under `runDir`
 * (`$TMPDIR/stage-a`, `$LEGION_WORKSPACE`), each compared by its real path, as `root` is one. A
 * root already named that way, or one none of them holds, is written as it is. */
export function namedSource(
  root: string,
  runDir?: string,
  env: NodeJS.ProcessEnv = process.env
): string {
  if (root.startsWith("$")) return root;
  const real = (dir: string): string | undefined => {
    try {
      return realpathSync(dir);
    } catch {
      return undefined;
    }
  };
  const target = real(root) ?? root;
  let named: { name: string; dir: string } | undefined;
  for (const [name, sub] of RUN_DIRECTORIES) {
    const candidates = [env[name], runDir === undefined ? undefined : path.join(runDir, sub)];
    for (const value of candidates) {
      if (!value) continue;
      const dir = real(value);
      if (dir === undefined) continue;
      const holds =
        target === dir || target.startsWith(dir.endsWith(path.sep) ? dir : dir + path.sep);
      if (holds && dir.length > (named?.dir.length ?? -1)) named = { name, dir };
    }
  }
  if (named === undefined) return root;
  const rest = target.slice(named.dir.length).replace(/^\/*/, "");
  return rest === "" ? `$${named.name}` : `$${named.name}/${rest}`;
}

/** stage-job records the current checked-out files (tracked and non-ignored untracked files),
 * not HEAD. Both the index and new objects go into a disposable repository, not the source. The
 * record names the job and the checkout root it was taken from, written by the name the role was
 * given for the run's directory that holds it (namedSource). */
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
    const record = {
      at: new Date().toISOString(),
      job,
      tree,
      files: staged,
      source: namedSource(root, dir),
    };
    appendFileSync(path.join(dir, "staging.jsonl"), `${JSON.stringify(record)}\n`);
    return record;
  } finally {
    rmSync(candidate, { recursive: true });
  }
}

/** One operation of a run workspace's jj record, as workspace-ops.jsonl holds it. */
export interface WorkspaceOperation {
  id: string;
  start: string;
  end: string;
  description: string;
  standingOn: string[];
}

/** jj's template of one operation but the root: its id, when it began and ended (RFC 3339, to the
 * millisecond jj keeps), and its description's first line, separated by tabs. */
const OPERATION_LINE =
  'if(self.root(), "", id ++ "\\t" ++ self.time().start().format("%Y-%m-%dT%H:%M:%S%.3f%:z") ++ "\\t" ++ self.time().end().format("%Y-%m-%dT%H:%M:%S%.3f%:z") ++ "\\t" ++ description.first_line() ++ "\\n")';

/** The record of what a run's workspace stood on, which lane D's replay reads to rebuild the commit
 * the workspace stood on (`@-`) at each legion call: one line of `<out>/workspace-ops.jsonl` for
 * each jj operation of `workspace` but the root, oldest first, with its id, when it began and
 * ended, its description's first line and the commits `@-` names in its view; and
 * `<out>/workspace.pack`, the objects of each of those commits, so the replay holds a commit the
 * role made and never pushed. jj reads each operation's view as it was and leaves the working copy
 * alone, and git only reads: the workspace is not changed. */
export function recordWorkspace(workspace: string, out: string): WorkspaceOperation[] {
  const temp = mkdtempSync(path.join(out, "workspace-record-"));
  try {
    const config = path.join(temp, "jj.toml");
    writeFileSync(
      config,
      '[user]\nname = "Scenario Worker"\nemail = "worker@example.invalid"\n[fsmonitor]\nbackend = "none"\n'
    );
    const env = { ...gitEnv(), JJ_CONFIG: config };
    const jj = (...args: string[]) => run(workspace, env, ["jj", "--ignore-working-copy", ...args]);
    const operations = jj("op", "log", "--no-graph", "-T", OPERATION_LINE)
      .split("\n")
      .filter((line) => line !== "")
      .reverse()
      .map((line): WorkspaceOperation => {
        const [id = "", start = "", end = "", ...description] = line.split("\t");
        const standingOn = jj(
          "--at-op",
          id,
          "log",
          "-r",
          "@-",
          "--no-graph",
          "-T",
          'commit_id ++ "\\n"'
        )
          .split("\n")
          .filter((commit) => commit !== "");
        return { id, start, end, description: description.join("\t"), standingOn };
      });
    writeFileSync(
      path.join(out, "workspace-ops.jsonl"),
      operations.map((operation) => `${JSON.stringify(operation)}\n`).join("")
    );
    // jj's root commit is no Git object.
    const commits = [...new Set(operations.flatMap(({ standingOn }) => standingOn))].filter(
      (commit) => !/^0+$/.test(commit)
    );
    if (commits.length > 0) {
      const objects = run(workspace, env, ["git", "rev-list", "--objects", ...commits]);
      const pack = Bun.spawnSync(["git", "pack-objects", "--stdout", "-q"], {
        cwd: workspace,
        env,
        stdin: Buffer.from(`${objects}\n`),
        stdout: "pipe",
        stderr: "pipe",
      });
      if (pack.exitCode !== 0)
        throw new Error(`git pack-objects: ${pack.stderr.toString().trim()}`);
      writeFileSync(path.join(out, "workspace.pack"), pack.stdout);
    }
    return operations;
  } finally {
    rmSync(temp, { recursive: true });
  }
}

if (import.meta.main) {
  const [mode, first, second, ...rest] = Bun.argv.slice(2);
  if (mode === "build" && first !== undefined && second !== undefined && rest.length === 0)
    buildGitWorld(first, World.parse(JSON.parse(readFileSync(second, "utf8"))));
  else if (mode === "record" && first !== undefined && second !== undefined && rest.length === 0)
    recordWorkspace(first, second);
  else {
    console.error("usage: git-world.ts build <dir> <world file> | record <workspace> <dir>");
    process.exit(2);
  }
}
