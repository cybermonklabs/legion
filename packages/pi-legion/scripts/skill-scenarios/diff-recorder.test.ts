import { expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { diffCall, type Program, recorderScript } from "./diff-recorder";

// diff-recorder.ts stands in front of the real `jj` and `git` of a case run: a call goes to the
// real program as it is, and a diff read that the program read is recorded in calls.jsonl.

const work = temporaryDirectories("diff-recorder");

/** A stand-in of the real program: it prints its argv, copies its stdin to stdout, writes to
 * stderr, ends by SIGTERM where an argument is `die`, answers its checkout root query with its
 * working directory, and exits with FAKE_EXIT. */
const FAKE = `#!/bin/sh
if [ "$1" = rev-parse ] || [ "$*" = "--ignore-working-copy root" ]; then pwd -P; exit 0; fi
for arg do [ "$arg" = die ] && kill -TERM $$; done
printf 'argv:'; printf ' [%s]' "$@"; printf '\\n'
cat
echo "to stderr" >&2
exit "\${FAKE_EXIT:-0}"
`;

/** A run directory whose workspace holds a.txt, the real program `program` (the fake) in
 * <run>/real, and its recorder in <run>/bin, as case-world.ts writes it. */
function run(program: Program) {
  const dir = realpathSync(work());
  const workspace = path.join(dir, "workspace");
  mkdirSync(workspace);
  writeFileSync(path.join(workspace, "a.txt"), "a\n");
  for (const sub of ["real", "bin"]) mkdirSync(path.join(dir, sub));
  const real = path.join(dir, "real", program);
  writeFileSync(real, FAKE);
  chmodSync(real, 0o755);
  writeFileSync(
    path.join(dir, "bin", program),
    recorderScript(dir, program, real, process.execPath)
  );
  chmodSync(path.join(dir, "bin", program), 0o755);
  const spawn = (file: string, args: string[], env: Record<string, string> = {}) => {
    const result = Bun.spawnSync([file, ...args], {
      cwd: workspace,
      env: { PATH: `${path.dirname(process.execPath)}:/usr/bin:/bin`, HOME: dir, ...env },
      stdin: Buffer.from("from stdin\n"),
      stdout: "pipe",
      stderr: "pipe",
    });
    return {
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
      exit: result.exitCode,
      signal: result.signalCode ?? null,
    };
  };
  const rows = () =>
    existsSync(path.join(dir, "calls.jsonl"))
      ? readFileSync(path.join(dir, "calls.jsonl"), "utf8")
          .trimEnd()
          .split("\n")
          .map((line) => JSON.parse(line))
      : [];
  return {
    dir,
    rows,
    direct: (args: string[], env?: Record<string, string>) => spawn(real, args, env),
    recorded: (args: string[], env?: Record<string, string>) =>
      spawn(path.join(dir, "bin", program), args, env),
  };
}

test("a diff read and any other call reach the real program with their argv, stdin, stdout, stderr and exit status as they are", () => {
  const git = run("git");
  for (const [args, env] of [
    [["diff", "--stat", "--", "a.txt"], {}],
    [["diff", "--", "a.txt"], { FAKE_EXIT: "3" }],
    [["log", "--format=%s %n", "it's"], {}],
    [["status"], { FAKE_EXIT: "1" }],
  ] as [string[], Record<string, string>][]) {
    const through = git.recorded(args, env);
    expect(through).toEqual(git.direct(args, env));
    expect(through.stdout).toContain("from stdin");
    expect(through.stderr).toBe("to stderr\n");
  }
  // Only the two diff reads are recorded, each with its exit, and the root and paths of the one
  // that exited 0.
  expect(git.rows()).toMatchObject([
    {
      as: "git",
      argv: ["diff", "--stat", "--", "a.txt"],
      exit: 0,
      command: "git diff",
      source: "$LEGION_WORKSPACE",
      paths: ["a.txt"],
    },
    { as: "git", argv: ["diff", "--", "a.txt"], exit: 3, source: null, paths: null },
  ]);
});

test("a program a signal ended ends the recorder by that signal, for a diff read and any other call", () => {
  const jj = run("jj");
  for (const args of [
    ["diff", "die"],
    ["log", "die"],
  ]) {
    const direct = jj.direct(args);
    expect(direct).toMatchObject({ exit: null, signal: "SIGTERM" });
    expect(jj.recorded(args)).toEqual(direct);
  }
  expect(jj.rows()).toMatchObject([{ argv: ["diff", "die"], exit: 143, source: null }]);
});

test("a diff read names its subcommand and the paths it names, and any other call reads no diff", () => {
  const cwd = realpathSync(work());
  mkdirSync(path.join(cwd, "sub"));
  writeFileSync(path.join(cwd, "sub", "b.go"), "");
  const git = (...argv: string[]) => diffCall("git", argv, cwd);
  const jj = (...argv: string[]) => diffCall("jj", argv, cwd);
  // git: globals before the subcommand, -C moving the directory paths are read from; a path
  // before `--` is one that exists, one after `--` is a path, and `:/` names one from the root.
  expect(git("-C", "sub", "--no-pager", "diff", "-S", "b.go", "HEAD", "b.go")).toEqual({
    command: "git diff",
    checkout: ["-C", "sub", "--no-pager"],
    paths: [{ root: false, path: path.join(cwd, "sub", "b.go") }],
  });
  expect(git("show", "HEAD~1", "--", "gone.ts", ":/docs/x.md")).toEqual({
    command: "git show",
    checkout: [],
    paths: [
      { root: false, path: path.join(cwd, "gone.ts") },
      { root: true, path: "docs/x.md" },
    ],
  });
  expect(git("show", "HEAD")?.paths).toEqual([]);
  // git show of a revision's file prints the file and reads no diff; beside a commit it still
  // shows that commit's diff.
  expect(git("show", "HEAD:README.md")).toBeUndefined();
  expect(git("show", "HEAD~1:sub/b.go", "main:docs/")).toBeUndefined();
  expect(git("show", "HEAD", "HEAD:README.md")?.command).toBe("git show");
  // jj: globals anywhere, -R the checkout, each positional a fileset of the working directory;
  // jj show's positional is a revision.
  expect(jj("diff", "-R", "ws", "-r", "@-", "sub", "root:docs", "glob:*.ts")).toEqual({
    command: "jj diff",
    checkout: ["-R", "ws"],
    paths: [
      { root: false, path: path.join(cwd, "sub") },
      { root: true, path: "docs" },
      { root: false, path: path.join(cwd, "*.ts") },
    ],
  });
  expect(jj("--no-pager", "interdiff", "--from", "a", "--to", "b")).toEqual({
    command: "jj interdiff",
    checkout: [],
    paths: [],
  });
  expect(jj("show", "@-")?.paths).toEqual([]);
  for (const call of [jj("log", "-r", "diff"), git("status"), git("difftool"), jj()])
    expect(call).toBeUndefined();
});

/** `argv` in `cwd` under `env`, which must exit 0. */
function must(argv: string[], cwd: string, env: Record<string, string>) {
  const result = Bun.spawnSync(argv, { cwd, env, stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) throw new Error(`${argv.join(" ")}: ${result.stderr.toString()}`);
}

// The real programs name the checkout root the read read: the run's workspace and a scratch copy
// under its TMPDIR, each by the name the role was given for it, with each path from that root.
test.skipIf(skipWithout("git", "jj"))(
  "a real git and jj read is recorded with the checkout it read, as the role names it, and the paths from its root",
  () => {
    const dir = realpathSync(work());
    const workspace = path.join(dir, "workspace");
    const copy = path.join(dir, "tmp", "stage-a");
    for (const each of [path.join(workspace, "sub"), path.join(copy, "sub")])
      mkdirSync(each, { recursive: true });
    writeFileSync(path.join(dir, "jj.toml"), '[user]\nname = "T"\nemail = "t@example.invalid"\n');
    const env = {
      PATH: process.env.PATH ?? "",
      HOME: dir,
      TMPDIR: path.join(dir, "tmp"),
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_NOSYSTEM: "1",
      JJ_CONFIG: path.join(dir, "jj.toml"),
    };
    must(["git", "init", "-q"], workspace, env);
    writeFileSync(path.join(workspace, "sub", "a.txt"), "one\n");
    must(["git", "add", "."], workspace, env);
    must(
      ["git", "-c", "user.name=T", "-c", "user.email=t@e", "commit", "-qm", "one"],
      workspace,
      env
    );
    writeFileSync(path.join(workspace, "sub", "a.txt"), "two\n");
    must(["jj", "git", "init"], copy, env);
    writeFileSync(path.join(copy, "sub", "b.txt"), "b\n");
    mkdirSync(path.join(dir, "bin"));
    for (const program of ["git", "jj"] as const) {
      const real = Bun.which(program) ?? "";
      writeFileSync(
        path.join(dir, "bin", program),
        recorderScript(dir, program, real, process.execPath)
      );
      chmodSync(path.join(dir, "bin", program), 0o755);
    }
    const through = (cwd: string, ...argv: string[]) =>
      Bun.spawnSync([path.join(dir, "bin", argv[0] ?? ""), ...argv.slice(1)], {
        cwd,
        env,
        stdout: "pipe",
      });
    expect(
      through(path.join(workspace, "sub"), "git", "diff", "--", "a.txt").stdout.toString()
    ).toContain("+two");
    expect(through(workspace, "git", "diff", "--exit-code").exitCode).toBe(1);
    expect(through(copy, "jj", "diff", "--summary", "sub").stdout.toString()).toContain(
      "sub/b.txt"
    );
    expect(through(copy, "jj", "log", "-r", "@", "--no-graph", "-T", "description").exitCode).toBe(
      0
    );
    const rows = readFileSync(path.join(dir, "calls.jsonl"), "utf8")
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(rows.map(({ command, exit, source, paths }) => [command, exit, source, paths])).toEqual([
      ["git diff", 0, "$LEGION_WORKSPACE", ["sub/a.txt"]],
      ["git diff", 1, null, null],
      ["jj diff", 0, "$TMPDIR/stage-a", ["sub"]],
    ]);
  },
  60_000
);
