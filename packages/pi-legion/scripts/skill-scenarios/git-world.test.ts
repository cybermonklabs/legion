import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { loadCase } from "./case-world";
import { buildGitWorld, recordWorkspace } from "./git-world";

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
 * its standard output trimmed. */
function inRun(run: string, cwd: string, ...argv: string[]): string {
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
  return result.stdout.toString().trim();
}

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
