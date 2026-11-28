import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { loadCase } from "./case-world";
import { buildGitWorld, namedSource, recordWorkspace } from "./git-world";
import type { World } from "./heldout-case";

const work = temporaryDirectory("git-world");
const example = path.join(import.meta.dir, "testdata", "heldout-case-example.json");
const withoutTools = skipWithout("git", "jj");

/** `bun git-world.ts args...`, as lane D's replay runs it. */
function command(...args: string[]) {
  const result = Bun.spawnSync(
    [process.execPath, path.join(import.meta.dir, "git-world.ts"), ...args],
    { env: { PATH: process.env.PATH ?? "", HOME: work, TMPDIR: work } }
  );
  return { code: result.exitCode, stderr: result.stderr.toString() };
}

/** `argv` in `cwd` under the run's jj configuration (`<run>/jj.toml`, as the rig gives the pane),
 * its standard output as it printed it. */
function runIn(run: string, cwd: string, ...argv: string[]): string {
  const result = Bun.spawnSync(argv, {
    cwd,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: work,
      JJ_CONFIG: path.join(run, "jj.toml"),
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_NOSYSTEM: "1",
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  if (result.exitCode !== 0) throw new Error(`${argv.join(" ")}: ${result.stderr.toString()}`);
  return result.stdout.toString();
}

/** runIn's standard output trimmed. */
function inRun(run: string, cwd: string, ...argv: string[]): string {
  return runIn(run, cwd, ...argv).trim();
}

/** Each tree and file of `commit`'s tree in `run`'s workspace, at any depth, by its path: a tree's
 * mode, and a file's mode and text. */
function treeOf(run: string, commit: string): Record<string, string> {
  const git = (...argv: string[]) => runIn(run, path.join(run, "workspace"), "git", ...argv);
  const entries = git("ls-tree", "-r", "-t", "--format=%(objectmode) %(objecttype) %(path)", commit)
    .split("\n")
    .filter((entry) => entry !== "");
  return Object.fromEntries(
    entries.map((entry) => {
      const [mode = "", type, ...words] = entry.split(" ");
      const file = words.join(" ");
      return [
        file,
        type === "blob" ? `${mode} ${git("cat-file", "blob", `${commit}:${file}`)}` : mode,
      ];
    })
  );
}

type WorldCommit = NonNullable<World["git"]>["commits"][number];

/** P05's world with `commits` for its Git: the remote's main at the first, and the workspace on the
 * last, which no remote branch holds. */
function worldOf(...commits: WorldCommit[]): World {
  const { world } = loadCase(example, "heldout", "P05");
  return {
    ...world,
    git: {
      commits,
      remote: { name: "origin", branches: [{ name: "main", head: commits[0]?.id ?? "" }] },
      workspace: { standingOn: commits.at(-1)?.id ?? "", localOnly: [] },
    },
  };
}

/** The commit ids the builder gives the example's P05 and P10 worlds, which name no files. */
const NO_FILES_COMMIT_IDS: Record<string, Record<string, string>> = {
  P05: {
    "1f3a5c7e9b2d4f6a8c0e1b3d5f7a9c2e4b6d8f0a": "3d669591dcafe9170992a26efe256208e7d658bf",
    "7c2e9a4b6d8f0a1c3e5b7d9f2a4c6e8b0d1f3a5c": "92e496e0dbed653a0675d987ce34492368592cbc",
  },
  P10: {
    "3b5d7f9a1c2e4a6c8e0b2d4f6a8c1e3b5d7f9a0c": "7c0a24192e73d9b20f40351da1a32f98eb9ae32b",
    "9e1c3a5b7d9f0b2d4f6a8c0e3b5d7f9a1c2e4b6d": "335610bafab88255c68273ac9d3e2155714f8bd0",
    "5d7f9b1d3f5a7c9e0a2c4e6b8d1f3a5c7e9b0d2f": "fcb0836afd8c35f6d161e2747d281b1d823da3e1",
  },
};

/** P05's head commit's files, as a world names them. */
const HEAD_FILES = [
  { path: "go.mod", text: "module example.invalid/acme\n\ngo 1.22\n" },
  { path: "cmd/acme/main.go", text: "package main\n\nfunc main() {}\n" },
  { path: "internal/deliver/deliver.go", text: "package deliver\n" },
  { path: "internal/deliver/testdata/empty", text: "" },
];

/** HEAD_FILES as treeOf reads them in a commit's tree. */
const HEAD_TREE = {
  "go.mod": "100644 module example.invalid/acme\n\ngo 1.22\n",
  cmd: "040000",
  "cmd/acme": "040000",
  "cmd/acme/main.go": "100644 package main\n\nfunc main() {}\n",
  internal: "040000",
  "internal/deliver": "040000",
  "internal/deliver/deliver.go": "100644 package deliver\n",
  "internal/deliver/testdata": "040000",
  "internal/deliver/testdata/empty": "100644 ",
};

// P05's head is off main, so with no files it holds CHANGES.md and the module P05's checks run in.
// Once it names files it holds those alone, in nested trees, with no CHANGES.md and no module, and
// its base, which names none, is the commit it was.
test.skipIf(withoutTools)(
  "a commit that names files holds those files alone",
  () => {
    const { world } = loadCase(example, "heldout", "P05");
    const [base, head] = world.git?.commits ?? [];
    if (!base || !head) throw new Error("P05 lost a commit");
    const named = worldOf(base, { ...head, files: HEAD_FILES });
    const run = path.join(work, "files-named");
    mkdirSync(run);
    const map = buildGitWorld(run, named);
    expect(treeOf(run, map[head.id] ?? "")).toEqual(HEAD_TREE);
    expect(treeOf(run, map[base.id] ?? "")).toEqual({
      "CHANGES.md": "100644 # Changes\n\n- main: the base the issue branch starts from\n",
    });
    expect(map[base.id]).toBe(NO_FILES_COMMIT_IDS.P05?.[base.id] ?? "");
  },
  60_000
);

// A commit's own file replaces its parent's at one path, the parent's other files stay, and a
// commit above that names none of its own holds its parent's tree.
test.skipIf(withoutTools)(
  "a commit above it holds its parent's files with its own written over them",
  () => {
    const { world } = loadCase(example, "heldout", "P05");
    const [base, head] = world.git?.commits ?? [];
    if (!base || !head) throw new Error("P05 lost a commit");
    const fix = {
      id: "3".repeat(40),
      parents: [head.id],
      description: "fix(deliver): retry",
      files: [
        { path: "cmd/acme/main.go", text: "package main\n\nfunc main() { run() }\n" },
        { path: "internal/deliver/deliver_test.go", text: "package deliver\n" },
      ],
    };
    const above = { id: "4".repeat(40), parents: [fix.id], description: "docs: nothing" };
    const run = path.join(work, "files-above");
    mkdirSync(run);
    const map = buildGitWorld(run, worldOf(base, { ...head, files: HEAD_FILES }, fix, above));
    const fixed = {
      ...HEAD_TREE,
      "cmd/acme/main.go": "100644 package main\n\nfunc main() { run() }\n",
      "internal/deliver/deliver_test.go": "100644 package deliver\n",
    };
    expect(treeOf(run, map[fix.id] ?? "")).toEqual(fixed);
    expect(treeOf(run, map[above.id] ?? "")).toEqual(fixed);
  },
  60_000
);

// A merge stands above both its parents and takes its first parent's files: a file only its second
// parent holds is not in it, and where both hold a path the first parent's text is. A merge whose
// second parent alone names files holds its own alone, and not the module its first parent holds.
test.skipIf(withoutTools)(
  "a merge commit has its first parent's files, and its second parent's are not taken",
  () => {
    const commit = (n: number, parents: number[], files?: Record<string, string>) => ({
      id: String(n).repeat(40),
      parents: parents.map((parent) => String(parent).repeat(40)),
      description: `commit ${n}`,
      ...(files && {
        files: Object.entries(files).map(([file, text]) => ({ path: file, text })),
      }),
    });
    const both = path.join(work, "files-merged");
    mkdirSync(both);
    const map = buildGitWorld(
      both,
      worldOf(
        commit(1, [], { "go.mod": "module a\n", "x.go": "a\n" }),
        commit(2, [1], { "x.go": "b\n", "b.go": "b\n" }),
        commit(3, [1], { "x.go": "c\n", "c.go": "c\n" }),
        commit(4, [2, 3])
      )
    );
    expect(treeOf(both, map["4".repeat(40)] ?? "")).toEqual({
      "b.go": "100644 b\n",
      "go.mod": "100644 module a\n",
      "x.go": "100644 b\n",
    });

    const second = path.join(work, "files-merged-second");
    mkdirSync(second);
    const secondMap = buildGitWorld(
      second,
      worldOf(
        commit(1, []),
        commit(2, [1]),
        commit(3, [1], { "c.go": "c\n" }),
        commit(4, [2, 3], { "m.go": "m\n" })
      )
    );
    expect(Object.keys(treeOf(second, secondMap["2".repeat(40)] ?? ""))).toContain("go.mod");
    expect(treeOf(second, secondMap["4".repeat(40)] ?? "")).toEqual({ "m.go": "100644 m\n" });
  },
  60_000
);

test.skipIf(withoutTools)(
  "a world that names no files builds the trees it builds today",
  () => {
    for (const id of ["P05", "P10"]) {
      const run = path.join(work, `no-files-${id}`);
      mkdirSync(run);
      expect(buildGitWorld(run, loadCase(example, "heldout", id).world)).toEqual(
        NO_FILES_COMMIT_IDS[id] ?? {}
      );
    }
  },
  60_000
);

// Lane D's replay starts a run from the Git the command builds from the run's world.json: the
// commits a run's start builds from that world (buildGitWorld), by the same ids, with the
// local-only commit the workspace stands on absent from the remote.
test.skipIf(withoutTools)(
  "the command builds a case world's Git with the commit ids a run's start gives it",
  () => {
    const { world } = loadCase(example, "heldout", "P05");
    const worldFile = path.join(work, "world.json");
    writeFileSync(worldFile, JSON.stringify(world));
    const started = path.join(work, "started");
    const replayed = path.join(work, "replayed");
    mkdirSync(started);
    mkdirSync(replayed);
    const map = buildGitWorld(started, world);
    expect(command("build", replayed, worldFile)).toEqual({ code: 0, stderr: "" });
    const built = JSON.parse(readFileSync(path.join(replayed, "commit-map.json"), "utf8"));
    expect(built).toEqual(map);
    const standing = map[world.git?.workspace.standingOn ?? ""] ?? "";
    const onRemote = Bun.spawnSync([
      "git",
      "--git-dir",
      path.join(replayed, "remote.git"),
      "cat-file",
      "-e",
      standing,
    ]);
    expect(onRemote.exitCode).not.toBe(0);
    expect(command("build", replayed).code).toBe(2);
  },
  60_000
);

// The record names, for each jj operation of the workspace, the commit `@-` stood on then: the
// case's standing commit at the start, then the commit the role made with `jj commit`, which it
// never pushed; the pack holds that commit, and recording changes nothing of the workspace.
test.skipIf(withoutTools)(
  "the record names each operation's @- and packs a commit the role made and never pushed",
  () => {
    const { world } = loadCase(example, "heldout", "P05");
    const run = path.join(work, "recorded");
    mkdirSync(run);
    const map = buildGitWorld(run, world);
    const workspace = path.join(run, "workspace");
    const standing = map[world.git?.workspace.standingOn ?? ""] ?? "";
    writeFileSync(path.join(workspace, "NOTES.md"), "experiment and variant\n");
    inRun(run, workspace, "jj", "commit", "-m", "docs: note experiment and variant");
    const made = inRun(run, workspace, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id");
    expect(made).not.toBe(standing);
    const ids = ["jj", "op", "log", "--no-graph", "-T", 'id ++ "\\n"'];
    const before = inRun(run, workspace, ...ids);
    const operations = recordWorkspace(workspace, run);
    expect(inRun(run, workspace, ...ids)).toBe(before);
    const lines = readFileSync(path.join(run, "workspace-ops.jsonl"), "utf8")
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(lines).toEqual(operations);
    const stood = operations.map(({ standingOn }) => standingOn.join(","));
    expect(stood.at(-1)).toBe(made);
    expect(stood.lastIndexOf(standing)).toBeLessThan(stood.indexOf(made));
    for (const { start, end } of operations)
      expect(Date.parse(end)).toBeGreaterThanOrEqual(Date.parse(start));
    const bare = path.join(work, "unpacked.git");
    inRun(run, work, "git", "init", "-q", "--bare", bare);
    const unpacked = Bun.spawnSync(["git", "--git-dir", bare, "unpack-objects", "-q"], {
      stdin: readFileSync(path.join(run, "workspace.pack")),
    });
    expect(unpacked.exitCode).toBe(0);
    expect(inRun(run, work, "git", "--git-dir", bare, "rev-parse", `${made}^`)).toBe(standing);
    expect(command("record", workspace).code).toBe(2);
  },
  60_000
);

// A staged Job's source reaches the reader with the run's own directories written by the names the
// role was given for them, so two Jobs staged from separate scratch copies read as separate and no
// run-specific path is in the record.
test("a staged source is written by the name the role was given for the run's directory that holds it", () => {
  const run = path.join(work, "named-run");
  const stageA = path.join(run, "tmp", "stage-a");
  mkdirSync(stageA, { recursive: true });
  mkdirSync(path.join(run, "workspace", "packages"), { recursive: true });
  symlinkSync("workspace", path.join(run, "ws"));
  const elsewhere = path.join(work, "named-elsewhere");
  mkdirSync(elsewhere);

  // The test process's own TMPDIR names no directory here: each call gives the variables it means.
  const named = (root: string) => namedSource(root, run, {});
  expect(named(stageA)).toBe("$TMPDIR/stage-a");
  expect(named(path.join(run, "tmp"))).toBe("$TMPDIR");
  expect(named(path.join(run, "workspace"))).toBe("$LEGION_WORKSPACE");
  expect(named(path.join(run, "workspace", "packages"))).toBe("$LEGION_WORKSPACE/packages");
  // A symlink the role was given (`ws`) is the directory it points to.
  expect(named(path.join(run, "ws"))).toBe("$LEGION_WORKSPACE");
  // A root already named, and a root no run directory holds, are written as they are.
  expect(named("$TMPDIR/stage-b")).toBe("$TMPDIR/stage-b");
  expect(named(elsewhere)).toBe(elsewhere);
  // A directory only shares a name's prefix with the run's: it is not inside it.
  mkdirSync(path.join(run, "tmp-other"));
  expect(named(path.join(run, "tmp-other"))).toBe(path.join(run, "tmp-other"));
  // A path nothing has made yet (the report a Job would write, a staging directory the role names)
  // is named by the run's directory that would hold it, through a symlink the role was given too,
  // and a path no run directory holds is written as its absolute path, a relative one from the
  // working directory.
  expect(named(path.join(run, "ws", "out", "report.json"))).toBe(
    "$LEGION_WORKSPACE/out/report.json"
  );
  expect(named(path.join(stageA, "bundle.tar"))).toBe("$TMPDIR/stage-a/bundle.tar");
  expect(named(path.join(run, "tmp", "labels-queue", "snapshot"))).toBe(
    "$TMPDIR/labels-queue/snapshot"
  );
  expect(named("relative/report.json")).toBe(path.resolve("relative/report.json"));
  expect(named(path.join(elsewhere, "gone", "x.json"))).toBe(
    path.join(elsewhere, "gone", "x.json")
  );

  // The role's own variables name the directories too, and the deepest one that holds the root is
  // the name it is written by.
  const env = { TMPDIR: path.join(run, "tmp"), LEGION_WORKSPACE: path.join(run, "tmp", "stage-a") };
  expect(namedSource(stageA, undefined, env)).toBe("$LEGION_WORKSPACE");
  expect(namedSource(path.join(run, "tmp"), undefined, env)).toBe("$TMPDIR");
  expect(namedSource(stageA, undefined, {})).toBe(stageA);
});
