import { expect, jest, test } from "bun:test";
import { existsSync, lstatSync, mkdirSync, symlinkSync, utimesSync, writeFileSync } from "node:fs";
import path from "node:path";
import { captureEnvironment } from "@legion/test-fixtures/environment";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { snapshotTag } from "./arms";
import {
  answerCall,
  type CheckoutCopy,
  colocateJj,
  grepScan,
  READ_ONLY_TOOLS,
  readOnlyCall,
  removeOutsideLinks,
  resetSandboxSpawnForTests,
  sandboxedBash,
  setSandboxSpawnForTests,
} from "./checkout";
import { detachedCommit, gitIn, writeTree } from "./testdata/fixtures";

const scratch = temporaryDirectory("skill-cuts-checkout", { real: true });

/** The made-up session's checkout root, as a written case names it. */
const ROOT = "/work/acme/proj";
/** A made-up secret in a file outside every copy, which no answer may show. */
const SECRET = fixtureSecret("outside");

/** A copy of a made-up checkout under `name`: docs/guide.md, a link inside it to that file, and
 *  links that leave it (to a file and a directory holding SECRET outside the copy) or resolve
 *  nowhere. */
function linkedCopy(name: string): string {
  const outside = path.join(scratch, `${name}-outside`);
  mkdirSync(path.join(outside, "dir"), { recursive: true });
  writeFileSync(path.join(outside, "secret.txt"), `${SECRET}\n`);
  writeFileSync(path.join(outside, "dir", "secret.txt"), `${SECRET}\n`);
  const dir = path.join(scratch, name);
  mkdirSync(path.join(dir, "docs"), { recursive: true });
  writeFileSync(path.join(dir, "docs", "guide.md"), "# Guide\n\nRun the app.\n");
  symlinkSync("docs/guide.md", path.join(dir, "inside.md"));
  symlinkSync(path.join(outside, "secret.txt"), path.join(dir, "leak.txt"));
  symlinkSync(path.join(outside, "dir"), path.join(dir, "docs", "up"));
  symlinkSync("missing.txt", path.join(dir, "gone"));
  return dir;
}

/** The guide of the made-up checkout. */
const GUIDE = "# Guide\n\nRun the app.\n";

/** A git checkout of a made-up repository under `name`, as cli.ts's shared checkout is: docs/
 *  guide.md, src/app.py and the files `more` (each path to its text) in one commit, checked out
 *  with a detached HEAD. */
function gitCopy(name: string, more: Record<string, string> = {}): string {
  const dir = path.join(scratch, name);
  writeTree(dir, { "docs/guide.md": GUIDE, "src/app.py": "def main():\n    return 0\n", ...more });
  detachedCommit(dir, "Add the guide");
  return dir;
}

/** The text of a `bash` answer of `copy` to `command` (run from `cwd`, when given), its wall time
 *  left out; the answer when it is none. */
async function bashText(
  copy: CheckoutCopy,
  command: string,
  cwd?: string
): Promise<string | { why: string }> {
  const answer = await answerCall(
    copy,
    "bash",
    { command, ...(cwd === undefined ? {} : { cwd }) },
    "hashline"
  );
  return "why" in answer ? answer : answer.text.replace(/\n\nWall time: \d+\.\d\d seconds/, "");
}

// Catches: a model-written read-only call of a written case's run that reads a file outside the
// copy of its checkout (a key file of the pod) through a symbolic link of the repository, or
// through a word the confinement check does not resolve, so its text reaches the model and the run
// records.
test("removeOutsideLinks removes each link that resolves outside the copy or nowhere and keeps one inside; no read, grep, glob or bash answer shows a file outside the copy", async () => {
  const dir = linkedCopy("copy");
  expect(removeOutsideLinks(dir)).toEqual(["docs/up", "gone", "leak.txt"]);
  expect(lstatSync(path.join(dir, "inside.md")).isSymbolicLink()).toBe(true);
  for (const removed of ["leak.txt", "docs/up", "gone"]) {
    expect({ removed, there: existsSync(path.join(dir, removed)) }).toEqual({
      removed,
      there: false,
    });
  }
  const copy = { dir, root: ROOT, cwd: ROOT, git: false };
  const answers = await Promise.all([
    answerCall(copy, "read", { path: "leak.txt" }, "hashline"),
    answerCall(copy, "grep", { pattern: SECRET.slice(0, 12), path: "." }, "hashline"),
    answerCall(copy, "bash", { command: "cat leak.txt" }, "hashline"),
    answerCall(copy, "bash", { command: `grep -R ${SECRET.slice(0, 12)} .` }, "hashline"),
    answerCall(copy, "bash", { command: "find -L . -type f | xargs cat" }, "hashline"),
    answerCall(copy, "bash", { command: "cat inside.md" }, "hashline"),
  ]);
  expect(answers.map((each) => ("why" in each ? `why: ${each.why}` : each.text))).toEqual([
    "Path 'leak.txt' not found",
    "No matches found",
    "cat: leak.txt: No such file or directory\n\n\nCommand exited with code 1",
    "(no output)\n\nCommand exited with code 1",
    "why: its command is not read-only",
    "# Guide\n\nRun the app.\n",
  ]);
  expect(JSON.stringify(answers)).not.toContain(SECRET);

  // A copy whose links were left in place: a read, grep or glob through one is not answered.
  const kept = { dir: linkedCopy("kept"), root: ROOT, cwd: ROOT, git: false };
  for (const [name, input] of [
    ["read", { path: "leak.txt" }],
    ["read", { path: "docs/up/secret.txt" }],
    ["grep", { pattern: "x", path: "leak.txt" }],
    ["grep", { pattern: "x", path: "docs/up" }],
    ["glob", { path: "docs/up/*.txt" }],
  ] as const) {
    const answer = await answerCall(kept, name, input, "hashline");
    expect({ name, input, answer }).toEqual({
      name,
      input,
      answer: { why: expect.stringContaining("through a symbolic link, outside the checkout") },
    });
  }

  // A bash word that may lead outside the copy, however it gets there, is not answered (a brace
  // expansion the read-only rule refuses itself).
  const deeper = { ...copy, cwd: `${ROOT}/docs` };
  for (const [command, why] of [
    ["cat $HOME/x", "$HOME/x expands"],
    ["cat {/etc/hostname,x}", "its command is not read-only"],
    ["cat ~/x", "~/x is under ~"],
    ["cat .git", ".git names .git"],
    ["cat /etc/hostname", "/etc/hostname is outside the session's checkout"],
    ["grep -f/etc/hostname guide.md", "-f/etc/hostname is outside the session's checkout"],
    ["cd .. && cat ../x", "../x is outside the session's checkout"],
  ] as const) {
    const answer = await answerCall(deeper, "bash", { command }, "hashline");
    expect({ command, answer }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a read of a code file the pin may summarize, or a ranged read, left unanswered (a run
// then judged on that read; thermonuclear's runs on a range past a file's end were dropped as
// reads-past-last-round), a range answered past the lines the pin shows, or a read with another
// selector answered in a form the runner does not rebuild.
test("a read of a code file of 100 lines or more is answered unsummarized and marked so, one past 300 lines with its first 300 and the count of the rest; a ranged read with the lines of its range, a line before and three after, and the pin's count of the lines left; a range past the file's end with the pin's notice; any other selector is not answered", async () => {
  const dir = path.join(scratch, "ranges");
  mkdirSync(path.join(dir, "src"), { recursive: true });
  mkdirSync(path.join(dir, "docs"), { recursive: true });
  const big = Array.from({ length: 120 }, (_, n) => `x${n + 1} = ${n + 1}\n`).join("");
  writeFileSync(path.join(dir, "src", "big.py"), big);
  writeFileSync(path.join(dir, "docs", "guide.md"), "# Guide\n\nRun the app.\n");
  const copy = { dir, root: ROOT, cwd: ROOT, git: false, answers: new Map() };
  const lines = big.replace(/\n$/, "").split("\n");
  const rows = (from: number, to: number) =>
    lines
      .slice(from - 1, to)
      .map((line, index) => `${from + index}:${line}`)
      .join("\n");
  const header = `[src/big.py#${snapshotTag(big)}]`;
  const read = (target: string) => answerCall(copy, "read", { path: target }, "hashline");
  expect(await read("src/big.py")).toEqual({
    text: `${header}\n${rows(1, 120)}`,
    isError: false,
    wholeText: true,
  });
  // A file past 300 lines, paged as the pin pages it unsummarized.
  const long = Array.from({ length: 350 }, (_, n) => `y${n + 1} = ${n + 1}\n`).join("");
  writeFileSync(path.join(dir, "src", "long.py"), long);
  const longRows = long
    .split("\n")
    .slice(0, 300)
    .map((line, index) => `${index + 1}:${line}`)
    .join("\n");
  expect(await read("src/long.py")).toEqual({
    text: `[src/long.py#${snapshotTag(long)}]\n${longRows}\n\n[50 more lines in file. Use :301 to continue]`,
    isError: false,
    wholeText: true,
  });
  for (const [target, text] of [
    [
      "src/big.py:10-12",
      `${header}\n${rows(9, 15)}\n\n[105 more lines in file. Use :16 to continue]`,
    ],
    ["src/big.py:5+2", `${header}\n${rows(4, 9)}\n\n[111 more lines in file. Use :10 to continue]`],
    ["src/big.py:1..2", `${header}\n${rows(1, 5)}\n\n[115 more lines in file. Use :6 to continue]`],
    [`${ROOT}/src/big.py:119-120`, `[src/big.py#${snapshotTag(big)}]\n${rows(118, 120)}`],
    // From line 2 on: no trailing context, and the rest of the file.
    [
      "docs/guide.md:2",
      `[docs/guide.md#${snapshotTag("# Guide\n\nRun the app.\n")}]\n1:# Guide\n2:\n3:Run the app.`,
    ],
    // A range that starts past the file's end: the pin's notice alone, no hashline line.
    [
      "src/big.py:200-210",
      "Line 200 is beyond end of file (120 lines total). Use :1 to read from the start, or :120 to read the last line.",
    ],
  ] as const) {
    expect({ target, answer: await read(target) }).toEqual({
      target,
      answer: { text, isError: false },
    });
  }
  for (const target of ["src/big.py:raw", "src/big.py:1-2,5-6", "src/big.py:-5"]) {
    expect({ target, answer: await read(target) }).toEqual({
      target,
      answer: { why: expect.stringContaining(target) },
    });
  }
  // A read, grep or glob answer is kept on the copy and given again; a bash answer is not kept.
  await answerCall(copy, "bash", { command: "cat docs/guide.md" }, "hashline");
  expect([...copy.answers.keys()].every((key) => key.startsWith("read\0hashline\0"))).toBe(true);
  expect(copy.answers.size).toBe(11);
});

// Catches: an inspection step counted as an act, or an act (a write, a commit, a command run
// through a substitution) taken for a read-only call: the report's inspection floors and the
// replay's checkout rounds both read this one rule.
test("readOnlyCall takes the read-only tools, and a bash call whose every command, joined by pipes, &&, || or ;, is one of the read-only programs with no redirection to a file", () => {
  expect([...READ_ONLY_TOOLS]).toEqual(["read", "grep", "glob", "find", "ls"]);
  for (const tool of READ_ONLY_TOOLS) {
    expect({ tool, readOnly: readOnlyCall(tool, { path: "src" }) }).toEqual({
      tool,
      readOnly: true,
    });
  }
  for (const command of [
    "cd src && cat app.py",
    "cat README.md | head -20",
    "head -n 5 a.txt; tail -n 5 a.txt",
    "ls -la docs",
    "find . -name '*.py' -type f",
    "grep -rn 'TODO' src || grep -rn FIXME src",
    "rg -n 'def main' src",
    "wc -l src/app.py",
    "sed -n '1,20p' src/app.py",
    "nl -ba src/app.py | sed -n 1,5p",
    "stat src/app.py",
    "file docs/logo.png",
    "tree -L 2 src",
    "git log --oneline -5",
    "git show HEAD:src/app.py",
    "git diff --stat",
    "git status --short",
    "git grep -n main",
    "git ls-files | wc -l",
    "git blame src/app.py",
    "git -C src status",
    "git --no-pager log -1",
    "ls missing 2>/dev/null; ls",
    "cat src/app.py 2>&1 | head",
    'grep -n "a > b" src/app.py',
    "cat < src/app.py",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
  for (const command of [
    "find . -name '*.pyc' -exec rm {} \\;",
    "find . -type d -execdir ls \\;",
    "find . -name '*.tmp' -delete",
    "find . -fprint found.txt",
    "cat src/app.py > copy.py",
    "grep -n main src/app.py >> notes.txt",
    "ls &> listing.txt",
    'grep "$(whoami)" src/app.py',
    "git push",
    "git commit -m wip",
    "git -c core.pager=less log",
    "rm -rf build",
    "sed -i 's/a/b/' src/app.py",
    "sed 's/a/b/' src/app.py",
    "python3 -c 'print(1)'",
    "cat src/app.py & cat README.md",
    "cat <(ls)",
    "/bin/cat src/app.py",
    "",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
  // Any other tool, and a bash call with no command, is no read-only call.
  for (const [name, input] of [
    ["write", { path: "a.py", content: "" }],
    ["edit", { path: "a.py" }],
    ["todo", { op: "view" }],
    ["task", { prompt: "look" }],
    ["bash", {}],
    ["bash", { command: 3 }],
  ] as const) {
    expect({ name, readOnly: readOnlyCall(name, input) }).toEqual({ name, readOnly: false });
  }
});

/** The dev box's start of a session's bash call, with made-up variables of its own. */
const UNSET = "unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM ACME_TOOLS_PIN ACME_TOOLS_IN";

// Catches: a written case's look at the work judged as its act because the read-only rule missed a
// form the dev box's sessions use (the defect: every judged step of test-driven-development's
// written cases was a bash call that starts with `unset`, prints `pwd`, or asks jj or git where it
// is), or an act (a branch made, a remote added, a jj change, a program the environment chooses, an
// option smuggled in a variable, a file written from a subshell) taken for a read-only call.
test("readOnlyCall takes pwd, true, echo, printf, env with no command, unset, export and assignments, git rev-parse, worktree list, remote and branch with no name, jj's read commands, and a subshell, group or substitution of read-only commands; it refuses each form that acts", () => {
  for (const command of [
    "pwd",
    "true",
    "echo done",
    "printf '%s\\n' a b",
    "printf -v line '%s' a",
    "env",
    "env -0",
    "env -i -u ACME_TOOLS_PIN",
    UNSET,
    "unset -v ACME_TOOLS_PIN",
    "export LC_ALL=C",
    "export ACME_MODE",
    "LIMIT=5",
    "FOO=bar cat src/app.py",
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a shell command's own `${dir}`
    'dir=src && ls $dir && cat "${dir}/app.py"',
    "cat $HOME/.profile",
    "echo $PATH $PWD",
    "git rev-parse --show-toplevel",
    "git rev-parse --abbrev-ref HEAD",
    "git worktree list",
    "git worktree list --porcelain",
    "git remote",
    "git remote -v",
    "git remote show origin",
    "git remote get-url origin",
    "git branch",
    "git branch -a",
    "git branch -r",
    "git branch -v",
    "git branch -vv",
    "git branch --list",
    "git branch --show-current",
    "jj status",
    "jj st",
    "jj log -r @ --no-graph",
    "jj show",
    "jj diff --git",
    "jj root",
    "jj file show src/app.py",
    "jj bookmark list",
    "jj workspace list",
    "jj --no-pager -R . st",
    "(cd src && ls)",
    "(jj st || git status) | cat",
    "{ ls; pwd; } 2>/dev/null",
    'cd "$(git rev-parse --show-toplevel)" && ls',
    "cat $(ls src)",
    "cat `ls src`",
    "(jj root 2>/dev/null; git rev-parse --show-toplevel 2>/dev/null; true)",
    `cd ${ROOT} && ${UNSET} && pwd && (jj st || git status) && git worktree list`,
    `${UNSET}; cd ${ROOT} && pwd && ls -a && (jj workspace list 2>/dev/null; git worktree list 2>/dev/null; jj st 2>/dev/null | head -50)`,
    `cd ${ROOT} && ${UNSET} && pwd && ls -a && (jj root 2>/dev/null; git rev-parse --show-toplevel 2>/dev/null; git status --short 2>/dev/null)`,
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
  for (const command of [
    "git branch new-name",
    "git branch -d old",
    "git branch -m old new",
    "git branch --set-upstream-to=origin/main",
    "git remote add x y",
    "git remote set-url origin https://example.test/x.git",
    "git remote remove origin",
    "git worktree add ../x",
    "git worktree remove ../x",
    "jj new",
    "jj describe -m wip",
    "jj git fetch",
    "jj bookmark set main",
    "jj workspace add ../x",
    "jj diff --tool rm",
    "jj --config ui.pager=less log",
    "export X=$(rm -rf /)",
    "echo $(git push)",
    "(cat src/app.py > copy.py)",
    "(ls; cat src/app.py >> notes.txt) && pwd",
    "{ ls; } > listing.txt",
    "cd $(mktemp -d)",
    "env -u GIT_DIR git status",
    "env rm -rf build",
    "env -S 'rm -rf build'",
    "unset PATH",
    "PATH=. ls",
    "export PATH=.",
    "HOME=. git log",
    "GIT_EXTERNAL_DIFF=rm git diff",
    "export GIT_DIR=../other",
    "JJ_CONFIG=jj.toml jj diff",
    "RIPGREP_CONFIG_PATH=rg.conf rg main",
    "LD_PRELOAD=./x.so ls",
    "X=--pre=sh && rg $X main",
    "X='1e rm -rf .' && sed -n \"$X\" src/app.py",
    "cat $FOO",
    "echo $?",
    "echo $((1 + 2))",
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a shell command's own `${X:-x}`
    "echo ${X:-x}",
    "printf -v 'a[$(id)]' x",
    "unset 'a[$(id)]'",
    "a[$(id)]=1",
    "( )",
    "{ ls }",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
});

// Catches: an answer that names the copy's own path (a scratch path in a body moved pass rates
// before), `pwd` and git's and jj's answers of where the session is given at the runner's paths,
// a jj command of a session that ran jj answered with an error that session never saw, or a word
// whose expansion may lead outside the copy answered.
test("a copy made a colocated jj repository answers jj st and jj root, and pwd, git rev-parse --show-toplevel, git worktree list and errors, at the session's paths; no answer holds the copy's path; an expansion the runner cannot place inside the checkout is not answered", async () => {
  const dir = gitCopy("colocated");
  expect(await colocateJj(dir)).toBe(true);
  const copy = { dir, root: ROOT, cwd: ROOT, git: true, jj: true as const };
  const given: unknown[] = [];
  const text = async (command: string, cwd?: string) => {
    const answer = await bashText(copy, command, cwd);
    given.push(answer);
    return answer;
  };
  expect(await text("pwd")).toBe(`${ROOT}\n`);
  expect(await text("pwd", "docs")).toBe(`${ROOT}/docs\n`);
  expect(await text("cd docs && pwd && echo $PWD")).toBe(`${ROOT}/docs\n${ROOT}/docs\n`);
  expect(await text("git rev-parse --show-toplevel", "docs")).toBe(`${ROOT}\n`);
  expect(await text("git worktree list")).toMatch(
    new RegExp(`^${ROOT} +[0-9a-f]{7,} \\(detached HEAD\\)\\n$`)
  );
  expect(await text("jj root", "src")).toBe(`${ROOT}\n`);
  const status = await text("jj st");
  expect(status).toEqual(expect.stringContaining("The working copy has no changes."));
  expect(status).toEqual(expect.stringContaining("Add the guide"));
  // A jj answer reads the snapshot the colocation took and takes no lock of the working copy (the
  // copy is read-only to it), so answers given at once are the same answer.
  expect(
    new Set(await Promise.all(Array.from({ length: 6 }, () => bashText(copy, "jj st"))))
  ).toEqual(new Set([status]));
  expect(await text("jj workspace list")).toEqual(expect.stringMatching(/^default: /));
  expect(await text(`cd "$(git rev-parse --show-toplevel)" && pwd`, "docs")).toBe(`${ROOT}\n`);
  // biome-ignore lint/suspicious/noTemplateCurlyInString: a shell command's own `${dir}`
  expect(await text("dir=docs && cat $dir/guide.md && cat ${dir}/guide.md")).toBe(GUIDE + GUIDE);
  expect(await text("git branch --show-current")).toBe("(no output, exit code 0)");
  const shape = await text(
    `cd ${ROOT} && ${UNSET} && pwd && (jj st || git status) && git worktree list`
  );
  expect(shape).toEqual(expect.stringMatching(new RegExp(`^${ROOT}\\nThe working copy has no`)));
  expect(shape).toEqual(expect.stringMatching(new RegExp(`\\n${ROOT} +[0-9a-f]{7,} \\(detached`)));
  expect(await text(`cd ${ROOT}/nowhere`)).toBe(
    `bash: line 1: cd: ${ROOT}/nowhere: No such file or directory\n\n\nCommand exited with code 1`
  );
  expect(await text(`cat ${ROOT}/src/missing.py`)).toBe(
    `cat: ${ROOT}/src/missing.py: No such file or directory\n\n\nCommand exited with code 1`
  );
  expect(await text("env")).toEqual(expect.stringContaining("PATH="));
  given.push(
    await answerCall(copy, "read", { path: `${ROOT}/docs/guide.md` }, "hashline"),
    await answerCall(copy, "grep", { pattern: "Guide", path: ROOT }, "hashline"),
    await answerCall(copy, "glob", { path: `${ROOT}/**/*.md` }, "hashline"),
    await answerCall(copy, "read", { path: "docs/missing.md" }, "hashline")
  );
  expect(given.every((each) => typeof each === "string" || !("why" in Object(each)))).toBe(true);
  expect(JSON.stringify(given)).not.toContain(dir);
  expect(JSON.stringify(given)).not.toContain(scratch);

  // A word whose expansion the runner cannot place inside the checkout is not answered.
  for (const [command, why] of [
    ["cat $HOME/.profile", "$HOME/.profile expands"],
    ["echo $HOME", "$HOME"],
    ["dir=/etc && cat $dir/hostname", "is outside the session's checkout"],
    ['cd "$(git rev-parse --git-dir)" && ls', "expands"],
    ["ls missing || dir=docs; cat $dir/guide.md", "$dir/guide.md expands"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a jj command of a session that ran no jj answered as in a jj workspace, or one answered
// where the copy could not be made a jj repository (its answer then an error the session never
// saw) or holds no history.
test("a copy no session made a jj repository answers jj as jj does outside one; where its colocation failed, or the copy is GitHub's tarball, a jj command is not answered", async () => {
  const dir = gitCopy("plain");
  const plain = { dir, root: ROOT, cwd: ROOT, git: true };
  const answer = await answerCall(plain, "bash", { command: "jj st" }, "hashline");
  expect(answer).toMatchObject({ isError: true });
  expect("text" in answer ? answer.text : answer.why).toStartWith(
    'Error: There is no jj repo in "."'
  );
  const failed = { ...plain, jj: { why: "jj git init --colocate failed: no room" } };
  expect(await bashText(failed, "(jj root || git rev-parse --show-toplevel)")).toEqual({
    why: expect.stringContaining("jj git init --colocate failed: no room"),
  });
  expect(await bashText(failed, "git rev-parse --show-toplevel")).toBe(`${ROOT}\n`);
  expect(await bashText({ ...plain, git: false }, "jj root")).toEqual({
    why: expect.stringContaining("GitHub's tarball"),
  });
}, 60_000);

// Catches: a glob of a directory or of a file with no glob character, or of a `;` list of paths,
// left unanswered (the defect: thermonuclear's written cases dropped as reads-past-last-round on
// such calls, which the pin answers), answered in a form the pin does not give, a missing path
// answered as no match where the pin says it is not found, or an answer that shows jj's own files
// of a colocated copy, which the pin's walk leaves out.
test("a glob of a directory with no glob character lists it, one of a file answers the file, and a ;-separated list answers each of its paths, a missing one noted; a missing path is not answered; jj's .jj directory is in no answer", async () => {
  const copy = filesCopy("globs", {
    "docs/guide.md": GUIDE,
    "docs/api/ref.md": "# Ref\n",
    "src/app.py": "def main():\n    return 0\n",
    ".jj/repo/store.md": "# Store\n",
  });
  const glob = (target: string) => answerCall(copy, "glob", { path: target }, "hashline");
  for (const [target, text] of [
    ["docs", "# docs/\nguide.md\n## api/\nref.md"],
    [`${ROOT}/docs/api`, "# docs/api/\nref.md"],
    ["src/app.py", "# src/\napp.py"],
    [`${ROOT}/src/app.py`, "# src/\napp.py"],
    ["docs/*.md;src/*.py", "# docs/\nguide.md\n# src/\napp.py"],
    ["docs/*.md; missing/*.py", "# docs/\nguide.md\n\nSkipped missing paths: missing/*.py"],
    ["**/*.md", "# docs/\nguide.md\n## api/\nref.md"],
  ] as const) {
    expect({ target, answer: await glob(target) }).toEqual({
      target,
      answer: { text, isError: false },
    });
  }
  for (const [target, why] of [
    ["missing", "not found"],
    ["missing/*.md", "not found"],
    ["a/*.md;b/*.md", "not found"],
    ["src/app.py,docs/guide.md", "is a list"],
    ["docs/*.md;/etc/*.conf", "outside the session's checkout"],
  ] as const) {
    expect({ target, answer: await glob(target) }).toEqual({
      target,
      answer: { why: expect.stringContaining(why) },
    });
  }
  expect(await answerCall(copy, "grep", { pattern: "Store", path: "." }, "hashline")).toEqual({
    text: "No matches found",
    isError: false,
  });
}, 60_000);

// The security review of the runner at dd14a587 found read-only calls that act or read outside the
// copy; each is a row below, made from the review's own command.

// Catches: a read-only call that sets a variable the shell or a program reads (`printf -vNAME`, the
// name attached to `-v`), runs a substitution through bash's old arithmetic `$[…]`, writes or reads
// a file through sed's commands, flags or options, runs a program or writes or reads a file
// through an option of git, jj, tree, file or rg given in a cluster, attached or abbreviated, or
// unsets the answers' own git or jj settings.
test("readOnlyCall refuses printf -vNAME, $[…], sed's w, W, e, r and R commands and s flags, its in-place option in every spelling and -f, the options of git, jj, tree, file and rg that run a program or write or read a file, clustered, attached or abbreviated, and an unset of the answers' git or jj settings; it still takes the read-only forms beside them", () => {
  for (const command of [
    // printf -v with the name attached.
    "printf -vPATH .",
    "printf -vGIT_CONFIG_GLOBAL vendor.gitconfig; git status",
    "printf -vIFS / && cat src/app.py",
    // $[…], which runs the substitution a variable it reads holds.
    "x='a[$(id)]'; echo $[x]",
    "echo $[1 + 2]",
    // sed: commands and flags that run a program or write or read a file, -i, -f.
    "sed -n 'r /etc/hostname' src/app.py",
    "sed -n '1R /etc/hostname' src/app.py",
    "sed -n '$!W copy.txt' src/app.py",
    "sed -n 'p;w copy.txt' src/app.py",
    "sed -n 's/a/b/w copy.txt' src/app.py",
    "sed -n 's/main/id/e' src/app.py",
    "sed -n 's/a/b/gpw copy.txt' src/app.py",
    "sed -n '1e id' src/app.py",
    "sed -n '/x/{p;r /etc/hostname\n}' src/app.py",
    "sed -n -e 1p -e 'w copy.txt' src/app.py",
    "sed --quiet --expression='$r /etc/hostname' src/app.py",
    "sed -n --e '1r /etc/hostname' src/app.py",
    "sed -ni p src/app.py",
    "sed -n -i.bak p src/app.py",
    "sed -n --in-place p src/app.py",
    "sed -n --in p src/app.py",
    "sed -n --in-pl=.bak p src/app.py",
    "sed -n -f script.sed src/app.py",
    "sed -nf script.sed src/app.py",
    "sed -n --fi=script.sed src/app.py",
    // git and jj options that run a program or write or read a file.
    "git grep -Oid main",
    "git grep -nOid main",
    "git grep -nO main",
    "git grep --open-files-in-pager=id main",
    "git grep --open=id main",
    "git grep --op main",
    "git log --out=copy.txt",
    "git diff --ext",
    "git diff --no-ind src docs",
    "git log --show-signature -1",
    "git log --show-sig -1",
    "git log --format=%G? -1",
    "git show --pretty=format:%GS",
    "jj diff --too rm",
    "jj log --config-f=jj.toml",
    "jj --conf ui.pager=less log",
    // tree, file and rg in clusters, attached or abbreviated.
    "tree -ao out.txt",
    "tree -Lo 2 out.txt",
    "tree -R -H . -L 1",
    "file -bC",
    "file --comp",
    "file -m magic src/app.py",
    "file -bmmagic src/app.py",
    "file --magic=magic src/app.py",
    "file -f names.txt",
    "file -z src/app.py",
    "file --uncompress src/app.py",
    "file -p src/app.py",
    "rg --pre sh main",
    "rg --pr=sh main",
    "rg --pre-glob '*' --pre sh main",
    "rg -z main",
    "rg -nz main",
    "rg --search-zip main",
    "rg --hostname-bin=id main",
    // The answers' own git and jj settings.
    "unset GIT_CONFIG_COUNT",
    "unset GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0",
    "unset JJ_CONFIG",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
  for (const command of [
    "printf -v line %s a",
    "printf -- -v",
    "sed -n '/def main/,/^$/p' src/app.py",
    "sed -n '/error/p' src/app.py",
    "sed -n '/^w/p;s/x/y/p' src/app.py",
    "sed -ne 5p src/app.py",
    "sed -n -e 1p -e '$p' src/app.py",
    "sed --quiet 10q src/app.py",
    "sed -nE '/a|b/{p;q}' src/app.py",
    "sed -n '$=' src/app.py",
    "sed -n '1~2p' src/app.py",
    "sed -n '/x/a\\\nnote' src/app.py",
    "git grep -e -O main",
    "git log -SOAuth",
    "git grep --or -e a -e b",
    "git diff --no-ext-diff",
    "git diff --output-indicator-new=+",
    "git log --format=%s -1",
    "jj diff --to @",
    "jj log --color=never -r @",
    "tree -L 2 -a src",
    "file -b src/app.py",
    "rg -tpy main",
    "rg -e -z src",
    "env -0 -u ACME_TOOLS_PIN",
    "env -",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
});

// Catches: a path through a symbolic link of the checkout that resolves inside it, so that
// removeOutsideLinks keeps it, and then `..`: the kernel walks the `..` from the link's target, so
// the path read as written stays in the checkout while the walk leaves the copy (the review's
// `l/../x` and `l/../../..`); or the same through a glob the link matches, which the runner reads
// as written (the second review's `a/l*/../../x`).
test("a bash path through a link that stays inside the copy, then .., is not answered where the kernel's walk of it leaves the copy, nor one whose glob comes before .. or may match . or .., nor a cd to a glob; a path through the same links that stays inside is answered", async () => {
  const copy = filesCopy("walk", { "docs/guide.md": GUIDE });
  writeTree(path.join(scratch, "walk-outside"), { "secret.txt": `${SECRET}\n` });
  mkdirSync(path.join(copy.dir, "docs", "a", "b"), { recursive: true });
  symlinkSync(".", path.join(copy.dir, "self"));
  symlinkSync("../../..", path.join(copy.dir, "docs", "a", "b", "up"));
  expect(removeOutsideLinks(copy.dir)).toEqual([]);
  for (const [command, cwd] of [
    ["cat self/../walk-outside/secret.txt", undefined],
    ["cat docs/a/b/up/../walk-outside/secret.txt", undefined],
    [`cat ${ROOT}/self/../walk-outside/secret.txt`, undefined],
    ["ls self/..", undefined],
    ["grep -r x self/../walk-outside", undefined],
    ["cat ../walk-outside/secret.txt", "self"],
  ] as const) {
    const answer = await bashText(copy, command, cwd);
    expect({ command, answer }).toEqual({
      command,
      answer: { why: expect.stringContaining("through a symbolic link of the checkout") },
    });
  }
  for (const command of [
    "cat sel*/../walk-outside/secret.txt",
    "cat [s]elf/../walk-outside/secret.txt",
    "cat docs/a/b/u?/../walk-outside/secret.txt",
    "cd sel* && cat ../walk-outside/secret.txt",
    "ls -d .*",
  ]) {
    const answer = await bashText(copy, command);
    expect({ command, answer }).toEqual({
      command,
      answer: { why: expect.stringContaining("holds a glob") },
    });
  }
  expect(await bashText(copy, "cat self/docs/guide.md docs/a/b/up/docs/guide.md docs/*.md")).toBe(
    GUIDE + GUIDE + GUIDE
  );
}, 60_000);

// Catches: jj, run in a copy that is no jj repository, finding a jj repository in a directory above
// the copy (the issue's workspace) and answering from it.
test("a jj command in a copy that is no jj repository, inside a directory that is one, answers as jj does outside any repository", async () => {
  const above = path.join(scratch, "jj-above");
  mkdirSync(above, { recursive: true });
  jjIn(above, "git", "init");
  const copy = { dir: gitCopy("jj-above/copy"), root: ROOT, cwd: ROOT, git: true };
  for (const command of ["jj root", "jj st"]) {
    const answer = await answerCall(copy, "bash", { command }, "hashline");
    expect({ command, answer }).toEqual({
      command,
      answer: {
        text: expect.stringMatching(/^Error: There is no jj repo in "\."\n/),
        isError: true,
      },
    });
    expect(JSON.stringify(answer)).not.toContain(above);
  }
}, 60_000);

// Catches: a git command of a copy fetched with no blobs (`--filter=blob:none`, as dd14a587's shared
// checkout was) fetching a blob it lacks from the commit's remote as it answers (`git show` of an
// older commit), which reaches the network with the pod's credential.
test("a git command in a copy that lacks an older commit's blob fetches nothing: the answer is git's error, and the copy still lacks the blob", async () => {
  const work = path.join(scratch, "lazy-work");
  writeTree(work, { "docs/guide.md": "# Guide\n\nRun the old app.\n" });
  gitIn(work, "init", "--quiet");
  gitIn(work, "add", ".");
  gitIn(work, "commit", "--quiet", "-m", "Write the first guide");
  const older = gitIn(work, "rev-parse", "HEAD:docs/guide.md");
  writeTree(work, { "docs/guide.md": GUIDE });
  gitIn(work, "commit", "--quiet", "-am", "Add the guide");
  const commit = gitIn(work, "rev-parse", "HEAD");
  const server = path.join(scratch, "lazy-server.git");
  gitIn(scratch, "clone", "--quiet", "--bare", work, server);
  gitIn(server, "config", "uploadpack.allowFilter", "true");
  gitIn(server, "config", "uploadpack.allowAnySHA1InWant", "true");
  const dir = path.join(scratch, "lazy");
  mkdirSync(dir, { recursive: true });
  gitIn(dir, "init", "--quiet");
  gitIn(dir, "remote", "add", "origin", server);
  gitIn(dir, "fetch", "--quiet", "--depth=50", "--filter=blob:none", "origin", commit);
  gitIn(dir, "checkout", "--quiet", "--detach", commit);
  const holdsOlder = () =>
    Bun.spawnSync(["git", "cat-file", "-e", older], {
      cwd: dir,
      env: { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: dir, GIT_NO_LAZY_FETCH: "1" },
    }).exitCode === 0;
  expect(holdsOlder()).toBe(false);
  const copy = { dir, root: ROOT, cwd: ROOT, git: true };
  const answer = await answerCall(
    copy,
    "bash",
    { command: "git show HEAD~1:docs/guide.md" },
    "hashline"
  );
  expect(answer).toMatchObject({ isError: true });
  expect(JSON.stringify(answer)).not.toContain("old app");
  expect(holdsOlder()).toBe(false);
}, 60_000);

// Catches: git in an answer's sandbox hashing every file of the copy again, so `git status` in a
// large checkout runs past the 30 s limit (the defect: 155 runs of test-driven-development's
// written cases got no answer to a bash call holding `git status`, in a checkout of 115,965 files):
// in the sandbox's user namespace each file shows uid and gid 0 where the index recorded the
// runner's, and the copy's ctime may not be the index's, so git's default stat check calls every
// file changed and compares its content, and with GIT_OPTIONAL_LOCKS=0 it never writes the
// refreshed index, so every call does it again. The probe of "not hashed again": a file whose text
// changed with its size and mtime kept is shown clean only when git trusts its stat data.
test("git in the sandbox trusts the stat data the runner's user wrote to the index, its ctime aside: a file whose text changed with its size and mtime kept is not hashed again, so git status shows it clean; one whose size changed shows as modified", async () => {
  const dir = gitCopy("stat-kept");
  const past = new Date("2020-01-01T00:00:00Z");
  const keepMtime = (file: string) => utimesSync(path.join(dir, file), past, past);
  keepMtime("docs/guide.md");
  keepMtime("src/app.py");
  // The index takes the files' stat data, written long after their mtime, so no entry of it is
  // racily clean (which git would compare by content whatever its stat check).
  gitIn(dir, "update-index", "--refresh");
  // Past the next whole second, so each changed file's ctime differs from the index's in seconds,
  // as a copy's may (so the row fails with git's default check even where the runner is root and
  // no owner differs): the kernel sets a file's ctime from its clock, and no call sets it back.
  await Bun.sleep(1_100);
  writeFileSync(path.join(dir, "docs/guide.md"), GUIDE.replace("app", "api"));
  keepMtime("docs/guide.md");
  writeFileSync(path.join(dir, "src/app.py"), "def main():\n    return 10\n");
  keepMtime("src/app.py");
  const copy = { dir, root: ROOT, cwd: ROOT, git: true };
  expect(await sandboxedBash(copy, "git status --short")).toMatchObject({
    output: " M src/app.py\n",
    exitCode: 0,
  });
}, 60_000);

// The negative control of verification v18's c-cases: calls the read-only rule counts as read-only,
// run through the path every answered bash call runs through (`sandboxedBash`), as though the
// rule's confinement had let each through, reach none of the pod's credentials and run nothing the
// copy's tracked configuration names; and a call the rule refuses runs nothing.
test("the negative control: cat of a key file, ls of the runner's home and of /opt, env, and git in a copy whose tracked bare repository's config sets core.fsmonitor, each read-only by the rule, show no key, an empty home and /opt, no credential and run no fsmonitor; the copy is read-only and has no network; curl is refused and never run", async () => {
  const keyDir = path.join(scratch, "gateway-key");
  writeTree(keyDir, { key: `${fixtureSecret("gwkey")}\n` });
  const credentials = path.join(scratch, "control-credentials");
  // Made-up credentials of the runner's own environment, which no answer may show.
  const planted: Record<string, string> = {
    GH_TOKEN: fixtureSecret("gh"),
    GITHUB_TOKEN: fixtureSecret("github"),
    DISPATCH_TOKEN_FILE: path.join(credentials, "dispatch", "token"),
    ENVOY_TOKEN_FILE: path.join(credentials, "envoy", "token"),
    AGENT_SECRETS_KEY_DIR: path.join(credentials, "keys"),
    ACME_GATEWAY_KEY_DIR: keyDir,
  };
  const restore = captureEnvironment(Object.keys(planted));
  Object.assign(process.env, planted);
  try {
    const marker = path.join(scratch, "control-fsmonitor-ran");
    const dir = gitCopy("control", {
      "vendor/tool.git/HEAD": "ref: refs/heads/main\n",
      "vendor/tool.git/config": `[core]\n\trepositoryformatversion = 0\n\tbare = false\n\tworktree = ..\n\tfsmonitor = "touch ${marker}; false"\n`,
      "vendor/tool.git/objects/keep": "",
      "vendor/tool.git/refs/keep": "",
    });
    const copy = { dir, root: ROOT, cwd: ROOT, git: true };
    const keyFile = path.join(keyDir, "key");
    const home = process.env.HOME ?? "/root";
    const calls = [
      `cat ${keyFile}`,
      `ls -A ${home}`,
      "ls -A /opt",
      "env",
      "git -C vendor/tool.git status",
    ];
    for (const command of calls) {
      expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
        command,
        readOnly: true,
      });
    }
    const ran = async (command: string) => {
      const answer = await sandboxedBash(copy, command);
      return "why" in answer ? answer : { output: answer.output, exitCode: answer.exitCode };
    };
    expect(await ran(`cat ${keyFile}`)).toEqual({
      output: `cat: ${keyFile}: No such file or directory\n`,
      exitCode: 1,
    });
    expect(await ran(`ls -A ${home}`)).toEqual({ output: "", exitCode: 0 });
    // The pod's installed tools, and the files of them any user may write, are hidden too.
    expect(await ran("ls -A /opt")).toEqual({ output: "", exitCode: 0 });
    // The pod's own model gateway key, where this machine has one: the answer is cat's error alone,
    // so it holds no byte of the key, which this test never reads.
    const gatewayKey = "/var/run/model-gateway-key/key";
    if (existsSync(gatewayKey)) {
      expect(await ran(`cat ${gatewayKey}`)).toEqual({
        output: `cat: ${gatewayKey}: No such file or directory\n`,
        exitCode: 1,
      });
    }
    // env and git as a run answers them, the rule's confinement passing both.
    const env = await answerCall(copy, "bash", { command: "env" }, "hashline");
    const shown = "text" in env ? env.text : env.why;
    expect(shown).toContain("PATH=");
    for (const [name, value] of Object.entries(planted)) {
      expect({ name, shown: shown.includes(name) || shown.includes(value) }).toEqual({
        name,
        shown: false,
      });
    }
    expect(
      await answerCall(copy, "bash", { command: "git -C vendor/tool.git status" }, "hashline")
    ).toMatchObject({ isError: true });
    expect(existsSync(marker)).toBe(false);
    // The copy is read-only to a command, which has no network: a server on this machine's
    // loopback hears nothing.
    let heard = 0;
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch: () => {
        heard += 1;
        return new Response("heard");
      },
    });
    try {
      const reached = await ran(`touch planted.txt; echo hi > /dev/tcp/127.0.0.1/${server.port}`);
      expect(reached).toEqual({
        output: expect.stringMatching(
          /Read-only file system[\s\S]*(Connection refused|Network is unreachable)/
        ),
        exitCode: 1,
      });
    } finally {
      server.stop(true);
    }
    expect({ heard, planted: existsSync(path.join(dir, "planted.txt")) }).toEqual({
      heard: 0,
      planted: false,
    });
    // A call the rule refuses runs nothing.
    const curl = "curl -s https://gateway.example.test/v1/messages";
    expect(readOnlyCall("bash", { command: curl })).toBe(false);
    expect(await answerCall(copy, "bash", { command: curl }, "hashline")).toEqual({
      why: "its command is not read-only",
    });
  } finally {
    restore();
  }
}, 60_000);

// The rows below are the shapes of the read-only calls a run of written cases answered in no form,
// so that each such run was made again and its cases dropped as reads-past-last-round: a grep path
// joined by `;` with `!` exclusions, a line past the 512 bytes grep shows, more matches in a file
// or more files than grep keeps, a read of a directory, a bash line past 768 bytes, an output the
// minimizer may rewrite, a quoted word that holds `$`, `?` or `*`, and a relative path after a `cd`.

/** A copy of a made-up checkout under `name` that holds `files` (each path to its text). */
function filesCopy(name: string, files: Record<string, string>): CheckoutCopy {
  const dir = path.join(scratch, name);
  writeTree(dir, files);
  return { dir, root: ROOT, cwd: ROOT, git: false };
}

/** The made-up checkout's files of the grep list and listing rows: a package with tests, a test
 *  directory, a generated directory of 14 files, an app, and an installed dependency. */
const LISTED: Record<string, string> = {
  "apps/web/e.ts": "export const kind = 'web';\n",
  "packages/ed/package.json": '{ "name": "ed" }\n',
  "packages/ed/src/a.ts": "const one = 1;\nconst kind = 'a';\n",
  "packages/ed/src/a.test.ts": "it('kind');\n",
  "packages/ed/src/tests/c.ts": "// kind\n",
  "packages/ed/src/.eslintrc": "{}\n",
  "packages/ed/src/.DS_Store": "",
  "packages/ed/node_modules/dep/index.js": "const kind = 'dep';\n",
  ...Object.fromEntries(
    Array.from({ length: 14 }, (_, n) => [
      `packages/ed/src/gen/g${String(n + 1).padStart(2, "0")}.ts`,
      "export {};\n",
    ])
  ),
};

/** The text of the `grep` answer of `copy` to `input`, in a session whose rows are numbered; the
 *  answer when it is none. */
async function grepText(
  copy: CheckoutCopy,
  input: Record<string, unknown>
): Promise<string | { why: string }> {
  const answer = await answerCall(copy, "grep", input, "numbered");
  return "why" in answer ? answer : answer.text;
}

// Catches: a grep path that is a list joined by `;`, with `!` exclusions or of files, left
// unanswered (34 of the dropped runs' calls), or answered with another order of its paths, its
// exclusions not applied, or without the pin's note of a missing path.
test("a grep path joined by ; is searched path by path in its order, a missing one noted after the matches; its ! parts leave out what they match below each path, an anchored one only inside it; node_modules is not entered", async () => {
  const copy = filesCopy("grep-lists", LISTED);
  const packages = [
    "# packages/ed/src/",
    "## a.test.ts",
    "*1|it('kind');",
    "## a.ts",
    " 1|const one = 1;",
    "*2|const kind = 'a';",
    "",
    "## tests/",
    "### c.ts",
    "*1|// kind",
  ];
  const apps = ["# apps/web/", "## e.ts", "*1|export const kind = 'web';"];
  for (const [given, lines] of [
    ["packages;apps", [...packages, "", ...apps]],
    ["packages/proj-not;apps", [...apps, "", "Skipped missing paths: packages/proj-not"]],
    [".;!**/node_modules/**", [...apps, "", ...packages]],
    [`${ROOT}/packages;!**/node_modules/**`, packages],
    [
      "packages/ed/src;!**/*.test.ts;!**/tests/**",
      packages.slice(0, 1).concat(packages.slice(3, 6)),
    ],
    ["packages;!packages/ed/src/tests", packages.slice(0, 6)],
    ["apps;!packages/ed", apps],
    [
      "packages/ed/src/a.ts;apps/web/e.ts;packages/ed/src/missing.ts",
      [
        "# packages/ed/src/",
        "## a.ts",
        ...packages.slice(4, 6),
        "",
        ...apps,
        "",
        "Skipped missing paths: packages/ed/src/missing.ts",
      ],
    ],
    ["packages/ed/src/a.ts; gone", [...packages.slice(4, 6), "", "Skipped missing paths: gone"]],
  ] as const) {
    expect({ given, answer: await grepText(copy, { pattern: "kind", path: given }) }).toEqual({
      given,
      answer: lines.join("\n"),
    });
  }
  // In a hashline session each file's header holds its snapshot tag.
  const hashline = await answerCall(
    copy,
    "grep",
    { pattern: "kind", path: "packages/ed/src/a.ts;apps/web/e.ts" },
    "hashline"
  );
  expect(hashline).toEqual({
    text: [
      "# packages/ed/src/",
      `## a.ts#${snapshotTag(LISTED["packages/ed/src/a.ts"] ?? "")}`,
      " 1:const one = 1;",
      "*2:const kind = 'a';",
      "",
      "# apps/web/",
      `## e.ts#${snapshotTag(LISTED["apps/web/e.ts"] ?? "")}`,
      "*1:export const kind = 'web';",
    ].join("\n"),
    isError: false,
  });
  expect(await grepText(copy, { pattern: "absent", path: "packages;gone" })).toBe(
    "No matches found\nSkipped missing paths: gone"
  );
  for (const [given, why] of [
    ["missing;gone", "are not in the checkout"],
    ["packages;!**/[ab].ts", "a glob of a form the runner does not read"],
    ["packages;/etc", "outside the session's checkout"],
    ["packages,apps", "a list of commas or blanks"],
    ["packages;packages", "names one path twice"],
  ] as const) {
    expect({ given, answer: await grepText(copy, { pattern: "kind", path: given }) }).toEqual({
      given,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a grep whose shown line passes the 512 bytes grep shows left unanswered (16 of the
// dropped runs' calls), or answered with the line whole, cut elsewhere than the pin's window
// around its first match, without its column, or without the pin's notice of cut lines.
test("a grep line past 512 bytes is cut as grep's native search cuts it: a context line to 509 bytes and ..., a match line around its first match with its column, trailing whitespace taken off, and the notice of cut lines after the matches", async () => {
  const copy = filesCopy("grep-wide", {
    "packages/fixtures/long.json": [
      "{",
      `  "note": "${"x".repeat(600)}kind${"y".repeat(100)}",`,
      `  "other": "${"z".repeat(600)}"`,
      `  "kind": "${"w".repeat(600)}"`,
      "}",
      "",
    ].join("\n"),
    "packages/fixtures/mid.txt": `${"é".repeat(300)}\n${"a".repeat(1000)}kind${"b".repeat(1000)}   \n`,
  });
  expect(await grepText(copy, { pattern: "kind", path: "packages" })).toBe(
    [
      "# packages/fixtures/",
      "## long.json",
      " 1|{",
      `*2|...${"x".repeat(403)}kind${"y".repeat(100)}", [col 612]`,
      ` 3|  "other": "${"z".repeat(497)}...`,
      `*4|  "kind": "${"w".repeat(498)}... [col 4]`,
      " 5|}",
      "## mid.txt",
      ` 1|${"é".repeat(254)}...`,
      `*2|...${"a".repeat(251)}kind${"b".repeat(251)}... [col 1001]`,
      "",
      "[Some lines truncated to 512 bytes]",
    ].join("\n")
  );
  // A cut context line alone brings no notice: the pin notes only cut match lines.
  expect(await grepText(copy, { pattern: "^\\}", path: "packages/fixtures/long.json" })).toBe(
    [` 4|  "kind": "${"w".repeat(498)}...`, "*5|}"].join("\n")
  );
}, 60_000);

// Catches: a grep with more matches in a file than grep keeps (20 of several files, 200 of one),
// or matches in more files than one page shows (20), left unanswered (11 of the dropped runs'
// calls), answered whole, or without the pin's notices and its next page by `skip`.
test("a grep keeps 20 matches a file of several and 200 of one, with grep's notice, and 20 files a page, with its notice of the next page, which skip answers; a total past the native search's 2000 matches is shown as at least", async () => {
  const many = Array.from({ length: 25 }, (_, n) => `const kind${n + 1} = ${n + 1};\n`).join("");
  const files: Record<string, string> = {
    "big/many.ts": many,
    "solo/huge.ts": Array.from({ length: 205 }, (_, n) => `kind ${n + 1}\n`).join(""),
  };
  for (let n = 1; n <= 22; n++) files[`big/pages/f${String(n).padStart(2, "0")}.ts`] = "kind\n";
  for (let n = 1; n <= 100; n++) files[`cap/f${String(n).padStart(3, "0")}.ts`] = many;
  const copy = filesCopy("grep-caps", files);
  const page = (from: number, to: number) =>
    Array.from({ length: to - from + 1 }, (_, n) => [
      `### f${String(from + n).padStart(2, "0")}.ts`,
      "*1|kind",
    ]).flat();
  expect(await grepText(copy, { pattern: "kind", path: "big" })).toBe(
    [
      "# big/",
      "## many.ts",
      ...Array.from({ length: 20 }, (_, n) => `*${n + 1}|const kind${n + 1} = ${n + 1};`),
      "",
      "## pages/",
      ...page(1, 19),
      "",
      "Showing files 1-20 of 23. Use skip=20 for the next page, or narrow paths/pattern.",
      "",
      "More than 20 matches in big/many.ts; grep keeps the first 20 per file when searching several. Search one file alone for up to 200.",
    ].join("\n")
  );
  expect(await grepText(copy, { pattern: "kind", path: "big", skip: 20 })).toBe(
    ["# big/pages/", ...page(20, 22).map((line) => line.replace(/^###/, "##"))].join("\n")
  );
  expect(await grepText(copy, { pattern: "kind", path: "big", skip: 30 })).toBe(
    "No more results (23 files total; skip=30 is past the end)"
  );
  expect(await grepText(copy, { pattern: "kind", path: "solo/huge.ts" })).toBe(
    [
      ...Array.from({ length: 200 }, (_, n) => `*${n + 1}|kind ${n + 1}`),
      "",
      "More than 200 matches; grep keeps the first 200 in a file. Add a line range (`path:N-M`) or narrow the pattern for the rest.",
    ].join("\n")
  );
  // 95 files of 21 fetched matches and 5 of the 96th fill the native search's 2000.
  expect(await grepText(copy, { pattern: "kind", path: "cap" })).toEqual(
    expect.stringMatching(
      /\n\nShowing files 1-20 of 96\+\. Use skip=20 for the next page, or narrow paths\/pattern\.\n\nMore than 20 matches in cap\/f001\.ts, cap\/f002\.ts, cap\/f003\.ts, cap\/f004\.ts, cap\/f005\.ts and 15 more files; grep keeps the first 20 per file when searching several\. Search one file alone for up to 200\.$/
    )
  );
}, 60_000);

// Catches: a read of a directory left unanswered, or answered with entries the pin leaves out
// (`.DS_Store`), past its depth, or past its cap of a directory under it; or a listing answered
// that would show the copy's own `.git`.
test("a read of a directory is answered with the pin's listing: entries two levels down by name, a directory under it capped at 12 with the count of the rest, .DS_Store left out; an empty one says so; a listing that shows .git, or a ranged read of a directory, is not answered", async () => {
  const copy = filesCopy("listing", LISTED);
  mkdirSync(path.join(copy.dir, "empty"), { recursive: true });
  const listing = [
    ".",
    "  - .eslintrc",
    "  - a.test.ts",
    "  - a.ts",
    "  - gen/",
    ...Array.from({ length: 11 }, (_, n) => `    - g${String(n + 1).padStart(2, "0")}.ts`),
    "    - … 2 more",
    "    - g14.ts",
    "  - tests/",
    "    - c.ts",
  ].join("\n");
  const read = (target: string) => answerCall(copy, "read", { path: target }, "hashline");
  for (const target of ["packages/ed/src", "packages/ed/src/", `${ROOT}/packages/ed/src`]) {
    expect({ target, answer: await read(target) }).toEqual({
      target,
      answer: { text: listing, isError: false },
    });
  }
  expect(await read("packages")).toEqual({
    text: [".", "  - ed/", "    - node_modules/", "    - package.json", "    - src/"].join("\n"),
    isError: false,
  });
  expect(await read("empty")).toEqual({ text: "(empty directory)", isError: false });
  mkdirSync(path.join(copy.dir, ".git"), { recursive: true });
  for (const [target, why] of [
    [".", "shows .git"],
    ["packages/ed/src:1-3", "a ranged read of a directory"],
  ] as const) {
    expect({ target, answer: await read(target) }).toEqual({
      target,
      answer: { why: expect.stringContaining(why) },
    });
  }
});

// Catches: a bash output line past 768 bytes but not 768 characters answered whole where the pin
// cuts it; an output the minimizer may rewrite answered as it is (a chain whose `cat` is not piped
// though another part is, a jj command's), or one it keeps as it is (in a subshell, or piped) not
// answered; and either refusal not saying that the pin's notice names an artifact the copy cannot
// know.
test("a bash line past 768 bytes, and an output of 1000 characters or more of an unpiped command the minimizer reads in a chain of && and ;, are not answered, each naming the artifact the pin's notice names; a subshell's or a piped command's is answered", async () => {
  const big = Array.from(
    { length: 60 },
    (_, n) => `export const line${String(n + 1).padStart(2, "0")} = "${"v".repeat(20)}";\n`
  ).join("");
  const copy = filesCopy("bash-wide", {
    "wide.txt": `${"é".repeat(400)}\n`,
    "pkg.json": '{ "name": "acme" }\n',
    "big.ts": big,
  });
  for (const [command, why] of [
    ["cat wide.txt | head -5", "passes the 768 bytes Oh My Pi shows"],
    [`cd ${ROOT} && cat pkg.json && sed -n 1,60p big.ts`, "minimizer may rewrite"],
    ["cat big.ts; ls | head", "minimizer may rewrite"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: {
        why: expect.stringMatching(
          new RegExp(`${why}.*the artifact .*whose id the copy cannot know`)
        ),
      },
    });
  }
  expect(await bashText(copy, "(cat big.ts)")).toBe(big);
  expect(await bashText(copy, "cat big.ts | head -80")).toBe(big);
  const colocated = {
    dir: gitCopy("minimized-jj", { "big.ts": big }),
    root: ROOT,
    cwd: ROOT,
    git: true,
  };
  expect(await colocateJj(colocated.dir)).toBe(true);
  const jj = { ...colocated, jj: true as const };
  expect(await bashText(jj, "jj file show big.ts")).toEqual({
    why: expect.stringContaining("minimizer may rewrite"),
  });
  expect(await bashText(jj, "jj file show big.ts | head -80")).toBe(big);
}, 60_000);

// Catches: a quoted word that holds `$`, `?`, `*`, `{…,…}` or `~` (a pattern) refused as though the
// shell expanded it, or an unquoted one that the shell does expand answered.
test("a quoted word is a literal: a single-quoted one, or a double-quoted one whose $ is escaped, is answered whatever it holds; an unquoted brace expansion or ~, or a $ that expands, is still refused", async () => {
  const copy = {
    dir: gitCopy("quoted", { "biome.json": "{}\n", "docs/notes.md": "Use ~/bin and x{2}\n" }),
    root: ROOT,
    cwd: ROOT,
    git: true,
  };
  for (const [command, text] of [
    ["git ls-files | grep -E '(^|/)biome\\.jsonc?$'", "biome.json\n"],
    [
      String.raw`git grep -n -I -E "keymap\(|\\\$useKeymap|Run" -- ':!**/node_modules/**' ':!**/*.py'`,
      "docs/guide.md:3:Run the app.\n",
    ],
    ["grep -nE 'x{2}|~/bin' docs/notes.md", "1:Use ~/bin and x{2}\n"],
    [
      String.raw`grep -n "x\$\|bin{1,2}" docs/notes.md`,
      "(no output)\n\nCommand exited with code 1",
    ],
    ["grep -c '~/bin' docs/notes.md", "1\n"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
  for (const [command, why] of [
    ["cat docs/{guide,notes}.md", "its command is not read-only"],
    ["cat ~/notes.md", "is under ~"],
    ['grep -n "$HOME" docs/notes.md', "expands $HOME"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
}, 60_000);

// Catches: a relative path after a `cd` to a directory of the checkout refused because it leaves
// the root from the directory the session started in, which the command is no longer in; or one
// answered after a `cd` that may fail, or may not run.
test("a cd to a literal directory the copy holds, that surely runs, moves every later command there, so a relative path is read from there alone; a cd that may fail or not run keeps both directories", async () => {
  const copy = filesCopy("cd-moves", LISTED);
  for (const [command, text] of [
    [
      `cd ${ROOT}/packages/ed/src && wc -l a.ts; cat ../package.json | head -60`,
      '2 a.ts\n{ "name": "ed" }\n',
    ],
    [
      `cd ${ROOT}/packages/ed && cat package.json; ls ../../node_modules/prosemirror-state 2>&1 | head -3`,
      `{ "name": "ed" }\nls: cannot access '../../node_modules/prosemirror-state': No such file or directory\n`,
    ],
    ["cd packages/ed/src || pwd; cat ../package.json", '{ "name": "ed" }\n'],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
  for (const command of [
    "cd packages/ed/missing && cat ../package.json",
    "cd packages/ed/package.json && cat ../package.json",
    "pwd && cd packages/ed/src && cat ../package.json",
    'cd "$PWD/packages/ed/src" && cat ../package.json',
    "cd packages/ed/src packages && cat ../package.json",
  ]) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: `../package.json is outside the session's checkout ${ROOT}` },
    });
  }
}, 60_000);

// The second security review of the read-only answers (at b7a917a9) found calls that run a
// program, read outside the copy or show the runner's values through an expansion the rule and
// the confinement read as written; each row below is made from the review's own call or checkout.

// Catches: a brace expansion whose `}` or `,` is quoted or escaped, or one in printf's words, that
// the rule did not see, so the call ran a command it names (sed's `e`), read a file outside the
// copy or set PATH; or a glob word that expands to an option a file of the checkout names
// (`--open-files-in-pager=sh`, `-vPATH`), taken for a read-only call.
test("readOnlyCall refuses a brace expansion in any word, its } or , quoted or escaped or not, and a glob word where the rule needs a word's one value or in printf, env, export and an assignment; it still takes braces with no comma or quoted, and a glob of a program that only opens files", () => {
  for (const command of [
    "sed -n -e p {x\\},-e1e\\ id} README",
    "cat {x\\},/etc/hostname}",
    "cat {x'}',/etc/hostname}",
    'cat {x"}",/etc/hostname}',
    "git grep {-ePWNED\\},--open-files-in-pager=sh}",
    "rg PWNED-42 {.\\},--pre=sh} .",
    "printf {-v,PATH} bin; cat README",
    "printf {-vPATH,bin}; cat README",
    "export z={a,b}",
    "cat < {x,y}",
    "echo $(cat {x\\},/etc/hostname})",
    "git grep -e PWNED *",
    "printf *; cat x",
    "sed -n p *",
    "rg x *",
    "tree *",
    "file *",
    "git log -- src/*.py",
    "find . -name *.py",
    "env X=*",
    "export X=a*",
    "X=*.py",
    "cat < src/*.py",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
  for (const command of [
    "git show HEAD@{1}:src/app.py",
    "git log --format='%h {%s}' -1",
    "grep -nE 'x{1,2}' src/app.py",
    "cat 'a{b,c}' \\{d,e\\}",
    "cat src/*.py",
    "grep -n main src/*.py",
    "ls -d */",
    "wc -l src/*.py",
    "head -n 3 docs/*.md",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
});

// Catches: `$(git rev-parse --show-toplevel)` taken for the root where git dies at a directory the
// checkout lays out as a bare repository, so the word is an absolute path outside the copy; an
// unquoted `~` of a word of echo (or any command) showing a home directory of the runner's; a jj
// command answered from a `.jj` the checkout tracks below its root, with jj's error or that
// repository's store; or the sandbox's jj verifying a signature with gpg or ssh-keygen.
test("$(git rev-parse --show-toplevel) is the root only where git meets no repository the checkout tracks below it; a word of any command with an unquoted ~ is not answered, nor a jj command whose repository may be a .jj the checkout tracks below its root; the sandbox's jj names no signing program", async () => {
  const dir = gitCopy("nested-repositories", {
    "vendor/tool/HEAD": "ref: refs/heads/main\n",
    "vendor/tool/objects/keep": "",
    "vendor/tool/refs/keep": "",
    "sub/.jj/junk": "x\n",
    "sub/f.txt": "y\n",
  });
  expect(await colocateJj(dir)).toBe(true);
  const copy = { dir, root: ROOT, cwd: ROOT, git: true, jj: true as const };
  for (const [command, text] of [
    ["cat $(git rev-parse --show-toplevel)/docs/guide.md", GUIDE],
    ['cd docs && cat "$(git rev-parse --show-toplevel)/docs/guide.md"', GUIDE],
    ["jj root", `${ROOT}\n`],
    ["cd src && jj root", `${ROOT}\n`],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
  // The copy's own repository answers (its snapshot leaves out the directory that holds a `.jj`).
  expect(await bashText(copy, "jj -R . st")).toEqual(
    expect.stringContaining("Parent commit (@-): ")
  );
  const nested = "a jj repository the checkout holds below its root";
  for (const [command, why] of [
    [
      "cd vendor/tool && cat $(git rev-parse --show-toplevel)/etc/hostname",
      "expands to a value the runner cannot place",
    ],
    ["echo ~legion ~root", "~legion is under ~"],
    ["echo a=~", "a=~ is under ~"],
    ["X=~/bin; echo $X", "~/bin is under ~"],
    ["cd sub && jj st", nested],
    ["jj -R sub log", nested],
    ["jj -Rsub log", nested],
    ["jj --repository=sub log", nested],
    ["cd sub && cat $(jj root)/f.txt", nested],
    ["cat /proc/self/mountinfo", "is outside the session's checkout"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining(why) },
    });
  }
  // No signature a template asks about is verified by a program: no backend's program is there.
  expect(
    await sandboxedBash(
      copy,
      "jj config get signing.backend; jj config get signing.backends.gpg.program; jj config get signing.backends.gpgsm.program; jj config get signing.backends.ssh.program"
    )
  ).toMatchObject({
    output: "none\n/mnt/no-signing-program\n/mnt/no-signing-program\n/mnt/no-signing-program\n",
    exitCode: 0,
  });
}, 60_000);

// Catches: a call of a program the read-only rule takes that the sandbox's PATH does not hold (this
// pod's has no rg) answered with bash's "command not found", which the session never saw.
test("a command one of whose programs is on no directory of the sandbox's PATH is not run, and not answered; one whose programs are there is answered", async () => {
  const copy = filesCopy("lacking", { "docs/guide.md": GUIDE });
  expect(await sandboxedBash(copy, "acme-tool docs", ROOT, ["acme-tool"])).toEqual({
    why: expect.stringContaining("the sandbox has no acme-tool on its PATH"),
  });
  // A program of the read-only list this machine lacks, where it lacks one.
  const lacking = ["rg", "file", "tree"].find(
    (each) => !["/usr/local/bin", "/usr/bin", "/bin"].some((at) => existsSync(path.join(at, each)))
  );
  if (lacking !== undefined) {
    expect(await bashText(copy, `${lacking} docs`)).toEqual({
      why: expect.stringContaining(`the sandbox has no ${lacking} on its PATH`),
    });
  }
  expect(await bashText(copy, "cat docs/guide.md | wc -l")).toBe("3\n");
}, 60_000);

/** Expects the `tool` call of `copy` with `input` not answered, past the 30 s limit, and its answer
 *  to come within the limit while the runner's own thread runs on. Real time, deliberately: the
 *  limit under test is the platform clock's 30 s, and a tick every 100 ms that keeps firing shows
 *  that the runner's own thread is free while the worker matches. */
async function expectPastLimit(
  copy: CheckoutCopy,
  tool: "grep" | "glob",
  input: Record<string, unknown>
): Promise<void> {
  let ticks = 0;
  const tick = setInterval(() => {
    ticks++;
  }, 100);
  const started = performance.now();
  const answer = await answerCall(copy, tool, input, "hashline");
  const tookMs = performance.now() - started;
  clearInterval(tick);
  expect(answer).toEqual({ why: "it ran past the 30 s limit" });
  expect(tookMs).toBeLessThan(32_000);
  expect(ticks).toBeGreaterThan(200);
}

// Catches: a grep whose regular expression backtracks (`(a+)+b` over a long run of one character)
// run in the runner's own thread with no time limit, which stops every run of the process.
test("a grep whose regular expression backtracks past the 30 s limit is not answered, and its answer comes within the limit while the runner's own thread runs on", async () => {
  // 20,000 lines, over each of which JavaScript's engine backtracks for a few milliseconds (about
  // 6 ms a line of 20 a's on this pod, under the 100 ms one test may take), so far past the limit.
  const copy = filesCopy("grep-backtracks", { "line.txt": `${"a".repeat(20)}\n`.repeat(20_000) });
  await expectPastLimit(copy, "grep", { pattern: "(a+)+b", path: "line.txt" });
}, 60_000);

// Catches: an option that reads a list of paths from a file (wc's `--files0-from`, find's
// `-files0-from`, tree's `--fromfile`), whose paths no word names, taken for a read-only call; or
// the value of an option that reads one file, attached to its letter (`grep -f..`, `git ls-files
// -X..`), not read as a path by the confinement, so it may name a file outside the copy.
test("readOnlyCall refuses wc --files0-from in every spelling, find -files0-from and tree --fromfile; an option's file value, attached to its letter, separate or after =, is read as a path, and one outside the checkout is not answered", async () => {
  for (const command of [
    "wc --files0-from=names",
    "wc --files0-from names",
    "wc -l --files0=names",
    "wc --f=names",
    "wc --files0-from=-",
    "wc -*",
    "find . -files0-from names",
    "find -files0-from names -type f",
    "tree --fromfile names",
    "tree --fromtabfile names",
    "tree --from names",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: false,
    });
  }
  for (const command of [
    "wc -l src/app.py",
    "wc --total=never -l src/app.py",
    "wc --lines src/*.py",
    "wc *",
    "find . -newer src/app.py",
    "tree -L 2 --noreport src",
  ]) {
    expect({ command, readOnly: readOnlyCall("bash", { command }) }).toEqual({
      command,
      readOnly: true,
    });
  }
  const copy = {
    dir: gitCopy("file-options", { "docs/patterns.txt": "Run\n" }),
    root: ROOT,
    cwd: ROOT,
    git: true,
  };
  for (const command of [
    "grep -f.. docs/guide.md",
    "grep -nf.. docs/guide.md",
    "grep --file=/etc/hostname docs/guide.md",
    "grep -r --exclude-from=../x Run .",
    "rg -f.. Run docs",
    "rg --ignore-file=/etc/hostname Run docs",
    "git grep -f.. main",
    "git ls-files -X..",
    "git ls-files --exclude-from=/etc/hostname",
    "git blame --contents=/etc/hostname src/app.py",
    "git blame --contents ../x src/app.py",
  ]) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining("is outside the session's checkout") },
    });
  }
  expect(await bashText(copy, "grep -nf docs/patterns.txt docs/guide.md")).toBe("3:Run the app.\n");
  expect(await bashText(copy, "grep -n --file=docs/patterns.txt docs/guide.md")).toBe(
    "3:Run the app.\n"
  );
}, 60_000);

// Catches: a glob word of a program the rule answers with globs (cat, wc, grep, ls, …) that bash
// expands, from the directory the shell is in, into an option a file of the checkout names (`wc *`
// with a file `--files0-from=.list`, which then counts the files `.list` names; `cat *` with a
// file `-n`).
test("a glob word that bash expands, from the shell's directory in the copy, to a word starting with - is not answered; a glob whose matches start with no - is", async () => {
  const copy = filesCopy("glob-options", {
    "opts/--files0-from=.list": "",
    "opts/.list": "a.txt\0",
    "opts/-n": "",
    "opts/a.txt": "a\n",
    "plain/a.txt": "a\n",
    "plain/b.txt": "b\n",
  });
  for (const [command, cwd] of [
    ["cd opts && wc *", undefined],
    ["cd opts && cat *", undefined],
    ["wc -l *", `${ROOT}/opts`],
    ["grep -n a [-a]*", `${ROOT}/opts`],
    ["cat -*", undefined],
  ] as const) {
    expect({ command, answer: await bashText(copy, command, cwd) }).toEqual({
      command,
      answer: { why: expect.stringContaining("which the program reads as an option") },
    });
  }
  for (const [command, text] of [
    ["cd plain && wc -l *", "1 a.txt\n1 b.txt\n2 total\n"],
    ["cat plain/*", "a\nb\n"],
    ["wc -c opts/*", "0 opts/--files0-from=.list\n0 opts/-n\n2 opts/a.txt\n2 total\n"],
    ["cd opts && cat ./*", "a\n"],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
}, 60_000);

// The third security review of the read-only answers (its round 2, at f0445b6f) found calls that
// read outside the copy, answer from a store the session never read, or stop the runner's own
// thread, through the forms 53a8e283 answers and the second review's fixes; each row below is made
// from the review's own call and checkout.

/** The review's made-up checkout of its cd rows under `name`, with the files `more` (each path to
 *  its text): the directories `a/b/c/d`, `e/f` and `e/g`, each with a file `keep` naming it, the
 *  guide, and the link `a/b/c/d/l` to `e/f`, which stays inside the copy, so removeOutsideLinks
 *  keeps it. */
function linkedDirsCopy(name: string, more: Record<string, string> = {}): CheckoutCopy {
  const copy = filesCopy(name, {
    "a/b/c/d/keep": "d\n",
    "e/f/keep": "f\n",
    "e/g/keep": "g\n",
    "docs/guide.md": GUIDE,
    ...more,
  });
  symlinkSync("../../../../e/f", path.join(copy.dir, "a/b/c/d/l"));
  expect(removeOutsideLinks(copy.dir)).toEqual([]);
  return copy;
}

/** Runs `jj args` in `cwd` as a made-up user, with no configuration of this machine's (its home a
 *  directory of the file's own); a failure fails the test. */
function jjIn(cwd: string, ...args: string[]): void {
  const home = path.join(scratch, "jj-home");
  mkdirSync(home, { recursive: true });
  const ran = Bun.spawnSync([Bun.which("jj") ?? "jj", ...args], {
    cwd,
    env: {
      PATH: process.env.PATH ?? "/usr/bin:/bin",
      HOME: home,
      JJ_USER: "Acme",
      JJ_EMAIL: "dev@acme.test",
    },
  });
  expect({ args, code: ran.exitCode, err: ran.stderr.toString() }).toMatchObject({ code: 0 });
}

// Catches: a `cd` that bash refuses (an option word, a second operand, a part before `..` that the
// copy lacks) taken as done, so the later commands were checked from a directory the shell never
// entered, and `cat ../../etc/hostname`, run from the root, read the machine's host name.
test("a cd that bash refuses, for an option word, a second operand or a part before .. that is no directory of the copy, may leave the shell where it was, so a later path that leaves the root from there is not answered; a cd of one word to a directory the copy holds, part by part, still moves the shell", async () => {
  const copy = linkedDirsCopy("cd-refused");
  for (const command of [
    "cd -x a/b; cat ../../etc/hostname",
    "cd --bogus a/b; cat ../../etc/hostname",
    "cd a/b -x; cat ../../etc/hostname",
    "cd x/../a/b; cat ../../etc/hostname",
  ]) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: `../../etc/hostname is outside the session's checkout ${ROOT}` },
    });
  }
  for (const [command, text] of [
    [
      "cd a/b; cat ../../etc/hostname",
      "cat: ../../etc/hostname: No such file or directory\n\n\nCommand exited with code 1",
    ],
    ["cd a/b/../../docs; cat ../docs/guide.md", GUIDE],
  ] as const) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({ command, answer: text });
  }
}, 60_000);

// Catches: a working directory, or a cd, through a link of the checkout that stays inside it: the
// kernel walks a later `..` from the real directory behind the link (the sandbox enters the working
// directory as it is real, `cd -P` goes there, and bash's own cd does where its walk of the path
// finds no directory), where the walk follows `..` from the path as written, so `cat
// ../../../etc/hostname` read the machine's host name; or a glob expanded there, which the walk
// read in another directory, so `wc` took `--files0-from=list` from a file's name.
test("a working directory or a cd through a symbolic link of the checkout is not answered where a later word holds .., nor where a path or a glob leaves the copy or makes an option from the directory the kernel's walk ends at; a call with no .. is answered from the real directory", async () => {
  const outside = path.join(scratch, "outside-cd-linked.txt");
  writeFileSync(outside, `${SECRET}\n`);
  const copy = linkedDirsCopy("cd-linked", {
    "e/--files0-from=list": "",
    "e/list": `${outside}\0`,
    "e/g/--files0-from=list": "",
    "e/g/list": `${outside}\0`,
  });
  const deep = { ...copy, cwd: `${ROOT}/a/b/c/d` };
  const linked = { ...copy, cwd: `${ROOT}/a/b/c/d/l` };
  const through = "through a symbolic link of the checkout";
  const leaves = "is outside the session's checkout";
  const option = "which the program reads as an option";
  const answers: unknown[] = [];
  for (const [at, command, cwd, why] of [
    [deep, "cd l/../g; cat ../../../../etc/hostname", undefined, leaves],
    [deep, "cd -P l/..; cat ../../../etc/hostname", undefined, leaves],
    [copy, "cd ..; cat ../../../etc/hostname", "a/b/c/d/l", through],
    [linked, "cd ..; cat ../../../etc/hostname", undefined, through],
    [deep, "cd l; cd ../g; wc [[:punct:]]*", undefined, through],
    [deep, "cd l/../g; wc [[:punct:]]*", undefined, "which the copy does not hold"],
    [deep, "cd -P l/..; wc [[:punct:]]*", undefined, option],
  ] as const) {
    const answer = await bashText(at, command, cwd);
    answers.push(answer);
    expect({ command, cwd, answer }).toEqual({
      command,
      cwd,
      answer: { why: expect.stringContaining(why) },
    });
  }
  for (const [at, command, cwd, text] of [
    [deep, "cd l && cat keep", undefined, "f\n"],
    [deep, "cd l/../g && cat keep", undefined, "g\n"],
    [copy, "cat keep", "a/b/c/d/l", "f\n"],
    [linked, "cat keep", undefined, "f\n"],
  ] as const) {
    const answer = await bashText(at, command, cwd);
    answers.push(answer);
    expect({ command, cwd, answer }).toEqual({ command, cwd, answer: text });
  }
  expect(JSON.stringify(answers)).not.toContain(outside);
}, 60_000);

// Catches: a glob whose bracket expression the glob-to-option check misread (`[[:punct:]]*`,
// `[]-]*`), so `wc` took `--files0-from=list` from a file's name and counted the files outside the
// copy that `list` names; the same check's matcher backtracking in the runner's own thread for
// minutes (`*a*a*a*a*a*a*a*a*b` beside a file `-aaa…`); or a glob part starting with `.`, which
// bash expands to `.git` (`.gi*/config`), read as written.
test("a glob word is not answered, at once, where the directory it expands in holds a name starting with -, whatever its bracket expressions; nor one whose part starts with ., which bash may expand to .git; a glob that expands below a named directory is answered", async () => {
  const outside = path.join(scratch, "outside-glob-class.txt");
  writeFileSync(outside, `${SECRET}\n`);
  const copy = filesCopy("glob-class", { "--files0-from=list": "", list: `${outside}\0` });
  const answers: unknown[] = [];
  for (const command of [
    "wc [[:punct:]]*",
    "wc []-]*",
    "wc -c [[:punct:]]*",
    "wc *",
    "wc [!a-z]*",
  ]) {
    const answer = await bashText(copy, command);
    answers.push(answer);
    expect({ command, answer }).toEqual({
      command,
      answer: { why: expect.stringContaining("which the program reads as an option") },
    });
  }
  const listBytes = Buffer.byteLength(`${outside}\0`);
  const width = String(listBytes).length;
  expect(await bashText(copy, "wc -c ./*")).toBe(
    `${"0".padStart(width)} ./--files0-from=list\n${listBytes} ./list\n${listBytes} total\n`
  );
  expect(JSON.stringify(answers)).not.toContain(outside);

  const dashed = filesCopy("glob-dashed", { [`-${"a".repeat(60)}`]: "" });
  const started = performance.now();
  expect(await bashText(dashed, `cat ${"*a".repeat(8)}*b`)).toEqual({
    why: expect.stringContaining("which the program reads as an option"),
  });
  expect(performance.now() - started).toBeLessThan(1_000);

  const git = { dir: gitCopy("glob-dotted"), root: ROOT, cwd: ROOT, git: true };
  for (const command of ["cat .gi*/config", "cat .g?t/HEAD"]) {
    expect({ command, answer: await bashText(git, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining("may match a name starting with .") },
    });
  }
  expect(await bashText(git, "cat docs/*.md")).toBe(GUIDE);
}, 60_000);

// Catches: the rule's brace-expansion test, a regular expression run on each word in the runner's
// own thread, taking time cubic in the word's length (`cat {,{,{,…`: 4 KB took about a second, so
// 64 KB would take about an hour), which stops every run of the process meanwhile.
test("readOnlyCall reads a 64 KB command of brace openings with no close in under 1 s, and finds the brace expansion where one closes", () => {
  const open = `cat ${"{,".repeat(32_766)}`;
  expect(open.length).toBe(65_536);
  for (const [command, readOnly] of [
    [open, true],
    [`${open}}`, false],
  ] as const) {
    const started = performance.now();
    expect(readOnlyCall("bash", { command })).toBe(readOnly);
    expect(performance.now() - started).toBeLessThan(1_000);
  }
});

// Catches: a jj command answered from a jj store that the checkout tracks at its root, in a copy
// whose context session ran no jj, so no colocation of the runner's made it: the session saw
// `There is no jj repo`, and such a store may name a git repository outside the copy (the nested
// `.jj` check stopped below the root).
test("no jj command is answered in a copy whose root holds a .jj that the runner's colocation did not make", async () => {
  const dir = gitCopy("tracked-jj");
  jjIn(dir, "git", "init", "--colocate");
  jjIn(dir, "describe", "-m", "From the tracked store");
  gitIn(dir, "add", "-f", ".jj");
  gitIn(dir, "commit", "--quiet", "-m", "Track .jj");
  gitIn(dir, "checkout", "--quiet", "--detach", gitIn(dir, "rev-parse", "HEAD"));
  const copy = { dir, root: ROOT, cwd: ROOT, git: true };
  for (const command of [
    "jj root",
    "jj log --no-graph -r 'all()' -T 'description ++ \"\\n\"'",
    "cd docs && jj st",
    "cat $(jj root)/docs/guide.md",
  ]) {
    expect({ command, answer: await bashText(copy, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining("a .jj at its root") },
    });
  }
}, 60_000);

// Catches: a grep answered `No matches found` where the pin's search matches: JavaScriptCore stops
// a backtracking match after a count of its steps (`(a+)+b` over 40 a's, then `xab`: about 0.7 s)
// and gives no match, where Rust's regex finds the `ab` at the line's end, so the run was judged
// on a wrong answer.
test("a grep one test of whose regular expression takes past 100 ms is not answered, since JavaScript's engine may have stopped it early with no match; a quick pattern over the same line is answered", async () => {
  const copy = filesCopy("grep-early-stop", { "one.txt": `${"a".repeat(40)}xab\n` });
  expect(await grepText(copy, { pattern: "(a+)+b", path: "one.txt" })).toEqual({
    why: expect.stringContaining("took past 100 ms"),
  });
  expect(await grepText(copy, { pattern: "a+b", path: "one.txt" })).toBe(`*1|${"a".repeat(40)}xab`);
}, 60_000);

// Catches: a grep match line past 512 bytes whose regular expression was tested again to place
// grep's window around the match, so that a second test past 100 ms (one the first test of that
// line did not take) turned the match the first test found into a call not answered.
test("a grep match line past 512 bytes is placed from the test that found it: a regular expression slow on a second test of the line still answers it", () => {
  const line = `${"x".repeat(600)}kind${"y".repeat(100)}`;
  const tested: string[] = [];
  class SlowAgain extends RegExp {
    override exec(text: string): RegExpExecArray | null {
      // Real time, deliberately: a second test of a line takes past the 100 ms one test may
      // take, as the runner measures it on the platform clock (`timedExec`); a line tested once,
      // as grep's should be, waits for nothing.
      if (tested.includes(text)) Bun.sleepSync(150);
      tested.push(text);
      return super.exec(text);
    }
  }
  expect(grepScan("long.txt", Buffer.from(`${line}\n`), new SlowAgain("kind", "u"), 20)).toEqual([
    {
      file: "long.txt",
      n: 1,
      text: `...${"x".repeat(405)}kind${"y".repeat(100)} [col 601]`,
      cut: true,
      before: [],
      after: [],
    },
  ]);
  expect(tested).toEqual([line]);
});

// Catches: two runs of one stage that make the same grep call at once each working its answer
// out in a worker of its own (each walking and reading the checkout again, in the one process
// whose memory the behaviour command stops at), as the copy kept only an answer that had come.
test("two identical grep calls made at once share one answer, worked out once", async () => {
  const answers = new Map();
  const keep = answers.set.bind(answers);
  const workedOut: string[] = [];
  answers.set = (key, value) => {
    workedOut.push(key);
    return keep(key, value);
  };
  const copy = { ...filesCopy("grep-shared", { "docs/guide.md": GUIDE }), answers };
  const input = { pattern: "Run", path: "docs" };
  expect(
    await Promise.all([
      answerCall(copy, "grep", input, "numbered"),
      answerCall(copy, "grep", input, "numbered"),
    ])
  ).toEqual([
    { text: "# docs/\n## guide.md\n 2|\n*3|Run the app.", isError: false },
    { text: "# docs/\n## guide.md\n 2|\n*3|Run the app.", isError: false },
  ]);
  expect(workedOut).toEqual([`grep\0numbered\0${JSON.stringify(input)}`]);
}, 60_000);

// Catches: a grep pattern that starts with an inline flag group (`(?i)technique[- ]targeted`),
// which the pin's Rust regex takes, left unanswered as JavaScript's RegExp throws on it, so the
// recorded runs exhausted their attempts on the call.
test("grep inline flag: a leading (?i) is taken off and makes the search case-insensitive, answered as the pin answers it", async () => {
  const copy = { dir: gitCopy("grep-inline-flag"), root: ROOT, cwd: ROOT, git: true };
  const grep = (pattern: string) =>
    answerCall(copy, "grep", { pattern, path: "docs/guide.md" }, "hashline");
  expect(await grep("(?i)run THE app")).toEqual({
    text: `[docs/guide.md#${snapshotTag(GUIDE)}]\n 2:\n*3:Run the app.`,
    isError: false,
  });
  expect(await grep("run THE app")).toEqual({ text: "No matches found", isError: false });
}, 60_000);

// Catches: a leading flag group's letters read in place of `case` rather than beside it (a
// `(?i)` search made case-sensitive by `case: true`), or its `m` and `s` not read.
test("grep inline flag: a leading (?i) keeps the search case-insensitive where case is true, and (?m) and (?s) are read beside it", async () => {
  const copy = { dir: gitCopy("grep-inline-flag-case"), root: ROOT, cwd: ROOT, git: true };
  const found = { text: "# docs/\n## guide.md\n 2|\n*3|Run the app.", isError: false };
  for (const [pattern, answer] of [
    ["(?i)RUN the APP", found],
    ["(?ism)^RUN.the APP\\.$", found],
    ["(?ms)^Run.the app\\.$", found],
    ["(?ms)^RUN.the app\\.$", { text: "No matches found", isError: false }],
  ] as const) {
    const given = { pattern, path: "docs", case: true };
    expect({ pattern, answer: await answerCall(copy, "grep", given, "numbered") }).toEqual({
      pattern,
      answer,
    });
  }
}, 60_000);

// Catches: a flag group the runner does not read (one elsewhere in the pattern, scoped, negated,
// repeated, a second one, or of another flag) answered in a form the pin may not give.
test("grep inline flag: a flag group elsewhere in the pattern, scoped, negated, repeated or of another flag is not answered", async () => {
  const copy = { dir: gitCopy("grep-inline-flag-other"), root: ROOT, cwd: ROOT, git: true };
  for (const pattern of ["a(?i)b", "(?i:run)", "(?-i)run", "(?x)run", "(?ii)run", "(?i)(?s)run"]) {
    const answer = await answerCall(copy, "grep", { pattern, path: "docs/guide.md" }, "hashline");
    expect({ pattern, answer }).toEqual({
      pattern,
      answer: { why: expect.stringContaining("flag group") },
    });
  }
}, 60_000);

// Catches: a grep that tests each file's whole text before its lines (the lines of a file with no
// match neither split nor tested, about 5 s of a 2 GB checkout's search on this pod) losing a
// match a test of a line finds: one past the first line, one anchored by `^` and `$` (which a
// whole-text test without the `m` flag finds only at the text's start and end), one at a word
// boundary by a newline, one of a case-insensitive search; or answering a whole-text match that
// only a newline or a `\r` before one makes, which no line holds.
test("grep speed: a file's whole text is tested before its lines and no match is lost, one on a later line, anchored by ^ and $, at a word boundary or of case false; a match only across a line's end is none", async () => {
  const copy = filesCopy("grep-speed", {
    "docs/crlf.md": "alpha\r\nbeta\r\n",
    "docs/later.md": "one\ntwo\nthree\nfour\nfive\nsix target\nseven\n",
    "docs/none.md": "nothing to find\n",
  });
  const found = " 5|five\n*6|six target\n 7|seven";
  for (const [input, answer] of [
    [{ pattern: "target", path: "docs/later.md" }, found],
    [{ pattern: "^six target$", path: "docs/later.md" }, found],
    [{ pattern: "\\bsix target\\b", path: "docs/later.md" }, found],
    [{ pattern: "SIX TARGET", path: "docs/later.md", case: false }, found],
    [{ pattern: "target", path: "docs" }, `# docs/\n## later.md\n${found}`],
    [{ pattern: "five\\s+six", path: "docs/later.md" }, "No matches found"],
    [{ pattern: "(?s)five.six", path: "docs" }, "No matches found"],
    [{ pattern: "alpha$", path: "docs/crlf.md" }, "No matches found"],
  ] as const) {
    expect({ input, answer: await grepText(copy, input) }).toEqual({ input, answer });
  }
}, 60_000);

// Catches: a grep that splits each file into lines and tests each line where a test of the
// file's whole text finds no match, which means no line holds one: with the reads, a search of a
// 2 GB checkout so ran past the 30 s limit.
test("grep speed: the lines of a text whose whole holds no match are not tested; those of one whose whole matches are each tested once", () => {
  const tested: string[] = [];
  class Recorded extends RegExp {
    override exec(text: string): RegExpExecArray | null {
      tested.push(text);
      return super.exec(text);
    }
  }
  const regex = new Recorded("x", "u");
  expect(grepScan("none.txt", Buffer.from("a\nb\nc\n"), regex, 20)).toEqual([]);
  expect(tested).toEqual([]);
  expect(grepScan("some.txt", Buffer.from("a\nx\nc\n"), regex, 20)).toEqual([
    {
      file: "some.txt",
      n: 2,
      text: "x",
      cut: false,
      before: [{ n: 1, text: "a" }],
      after: [{ n: 3, text: "c" }],
    },
  ]);
  expect(tested).toEqual(["a", "x", "c"]);
});

// Catches: a grep that reads each file's size from the descriptor it reads the file through (a
// stat of each path, a second lookup of it, took 12.6 s of a 2 GB checkout's search on this pod)
// losing the order the native search reads files in: a file past 4 MiB after every other, over
// its first 4 MiB; or answering such a file named alone, which the pin streams.
test("grep speed: a file past 4 MiB is searched over its first 4 MiB after every other file of its directory, and one named alone is not answered", async () => {
  const copy = filesCopy("grep-speed-sizes", {
    "big/a.txt": `kind 1\n${"filler\n".repeat(600_000)}kind 2\n`,
    "big/b.txt": "kind 3\n",
  });
  expect(await grepText(copy, { pattern: "kind", path: "big" })).toBe(
    [
      "# big/",
      "## b.txt",
      "*1|kind 3",
      "## a.txt",
      "*1|kind 1",
      " 2|filler",
      " 3|filler",
      " 4|filler",
    ].join("\n")
  );
  expect(await grepText(copy, { pattern: "kind", path: "big/a.txt" })).toEqual({
    why: expect.stringContaining("passes the 4194304 bytes grep reads into memory"),
  });
}, 60_000);

// Catches: a glob's matching (Bun.Glob's, of every path under its base) run in the runner's own
// thread with no time limit: a long pattern against each path of a large checkout takes seconds to
// minutes, which stops every run of the process meanwhile.
test("a glob whose matching runs past the 30 s limit is not answered, and its answer comes within the limit while the runner's own thread runs on", async () => {
  // 10,000 paths, each matched against a 57 KB pattern (about 16 ms a path on this pod), so far
  // past the limit.
  const copy = filesCopy(
    "glob-slow",
    Object.fromEntries(
      Array.from({ length: 10_000 }, (_, n) => [`big/${"a".repeat(60)}${n}`, ""] as const)
    )
  );
  await expectPastLimit(copy, "glob", { path: `**/${"{a*,*a,*}".repeat(6_400)}` });
}, 60_000);

// Catches: `pwd` from a working directory reached through a link of the checkout answered with the
// real directory's path (`…/e/f`), where the session's showed the path as written (`…/a/l`): the
// sandbox enters the working directory as it is real, so bash's `pwd` and `$PWD`, the environment
// that `env` and `export` print, and git and jj, which start from it, give the real path.
test("where the shell may be in a directory reached through a symbolic link, no answer that prints the working directory's own path is given: pwd, $PWD, env, export, git rev-parse --show-prefix, --show-cdup and --show-toplevel, jj root; from a directory with no link they are answered", async () => {
  const dir = gitCopy("pwd-linked", { "e/f/keep": "f\n" });
  mkdirSync(path.join(dir, "a"));
  symlinkSync("../e/f", path.join(dir, "a", "l"));
  expect(removeOutsideLinks(dir)).toEqual([]);
  expect(await colocateJj(dir)).toBe(true);
  const linked = { dir, root: ROOT, cwd: `${ROOT}/a/l`, git: true, jj: true as const };
  for (const command of [
    "pwd",
    "echo $PWD",
    "cat $(pwd)/keep",
    "env",
    "export -p",
    "git rev-parse --show-prefix",
    "git rev-parse --show-cdup",
    "git rev-parse --show-toplevel",
    "jj root",
  ]) {
    expect({ command, answer: await bashText(linked, command) }).toEqual({
      command,
      answer: { why: expect.stringContaining("may print the working directory's path") },
    });
  }
  expect(await bashText(linked, "cat keep")).toBe("f\n");
  const real = { ...linked, cwd: `${ROOT}/e/f` };
  for (const [command, text] of [
    ["pwd", `${ROOT}/e/f\n`],
    ["echo $PWD", `${ROOT}/e/f\n`],
    ["git rev-parse --show-prefix", "e/f/\n"],
    ["jj root", `${ROOT}\n`],
  ] as const) {
    expect({ command, answer: await bashText(real, command) }).toEqual({ command, answer: text });
  }
}, 60_000);

/** One macrotask turn, on a `setImmediate` that Bun's fake clock leaves real: it comes only once
 *  every promise and stream read the row set going has run as far as it goes. */
function nextTurn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** A stream of bytes the row writes to: `write` adds text, `close` ends it, once, and not after the
 *  runner cancelled it. */
function textStream() {
  const encoder = new TextEncoder();
  let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
  let open = true;
  const readable = new ReadableStream<Uint8Array>({
    start(started) {
      controller = started;
    },
    cancel() {
      open = false;
    },
  });
  return {
    readable,
    write(text: string) {
      if (open) controller?.enqueue(encoder.encode(text));
    },
    close() {
      if (!open) return;
      open = false;
      controller?.close();
    },
  };
}

/** What a row does to a stand-in for the child `unshare` starts for a sandbox. */
interface FakeSandbox {
  /** Says on standard error what the sandbox's setup says once the sandbox is made. */
  ready(): void;
  /** Prints `text` as the command's output. */
  print(text: string): void;
  /** Ends the child as a command that exits with `code`. */
  exit(code: number): void;
  /** The signal the runner killed the child with, once it has. */
  killedWith(): string | undefined;
}

/** A stand-in child named `name`, and what it does to it: its output and standard error are
 *  streams the row writes to, and it ends when the row exits it or the runner kills it, which
 *  `events` is told of (`exit <name>`, `kill <name>`). */
function fakeSandbox(name: string, events: string[]) {
  const output = textStream();
  const error = textStream();
  const exited = Promise.withResolvers<number>();
  let ended = false;
  let killedWith: string | undefined;
  const end = (how: string, code: number) => {
    if (ended) return;
    ended = true;
    events.push(`${how} ${name}`);
    output.close();
    error.close();
    exited.resolve(code);
  };
  const sandbox: FakeSandbox = {
    ready: () => error.write("ready\n"),
    print: (text) => output.write(text),
    exit: (code) => end("exit", code),
    killedWith: () => killedWith,
  };
  const child = {
    stdout: output.readable,
    stderr: error.readable,
    exited: exited.promise,
    kill(signal: "SIGKILL") {
      killedWith = signal;
      end("kill", 137);
    },
  };
  return { sandbox, child };
}

/** What a row of `onFakeSandboxes` holds. */
interface SandboxRig {
  /** What happened, in order: `spawn <n>`, then `exit <n>` or `kill <n>`, `n` from 1. */
  readonly events: string[];
  /** The `n`th stand-in child (from 1), once the runner has started it. */
  started(n: number): Promise<FakeSandbox>;
  /** `call`, which the row's end waits for, whatever the row did. */
  track<T>(call: Promise<T>): Promise<T>;
}

/** Runs `row` with a stand-in child (`fakeSandbox`) for each of the `count` sandboxes the runner
 *  starts, on Bun's fake clock, and the real spawn and clock put back after. A row that stops
 *  early leaves a command waiting for its place, so each call it tracked is let finish first: the
 *  children end at once and the queue empties, with no real sandbox started. */
async function onFakeSandboxes(
  count: number,
  row: (rig: SandboxRig) => Promise<void>
): Promise<void> {
  const events: string[] = [];
  const made = Array.from({ length: count }, () => Promise.withResolvers<FakeSandbox>());
  const children: FakeSandbox[] = [];
  const calls: Promise<unknown>[] = [];
  let over = false;
  setSandboxSpawnForTests(() => {
    const slot = made[children.length];
    if (slot === undefined) {
      throw new Error(`a sandbox past the ${count} the row expects was started`);
    }
    const name = String(children.length + 1);
    const { sandbox, child } = fakeSandbox(name, events);
    events.push(`spawn ${name}`);
    children.push(sandbox);
    slot.resolve(sandbox);
    if (over) sandbox.exit(1);
    return child;
  });
  jest.useFakeTimers();
  try {
    await row({
      events,
      started(n) {
        const slot = made[n - 1];
        if (slot === undefined) throw new Error(`the row has no stand-in child ${n}`);
        return slot.promise;
      },
      track(call) {
        calls.push(call);
        return call;
      },
    });
  } finally {
    over = true;
    for (const sandbox of children) sandbox.exit(1);
    await Promise.allSettled(calls);
    jest.useRealTimers();
    resetSandboxSpawnForTests();
  }
}

// Catches: a runner process's sandboxed commands started all at once (eight `git status` calls of
// one stage on a two-CPU pod each ran to the 30 s limit; two at once took 10.9 s each), not one
// after another; or a waiting command's 30 s counting from when it asked, or from its sandbox's
// spawn, so a command that waited, or whose sandbox was slow to make, is ended before it has run
// its 30 s. The children here are stand-ins and the clock is Bun's fake one: no sandbox is made
// and no row waits on a real clock.
test("sandbox queue: a second sandboxed command spawns no child while the first holds the place, and has its own 30 s from its ready marker, after its wait and its setup", async () => {
  const copy = filesCopy("sandbox-queue", { "docs/guide.md": GUIDE });
  await onFakeSandboxes(2, async ({ events, started, track }) => {
    const first = track(sandboxedBash(copy, "git status"));
    const second = track(sandboxedBash(copy, "git status --short"));
    const one = await started(1);
    await nextTurn();
    expect(events).toEqual(["spawn 1"]);

    // The first's setup takes 20 s and its command 29 s, each inside its 30 s. The second has then
    // waited 49 s: had its wait counted against its own 30 s, it would be ended before it spawns.
    jest.advanceTimersByTime(20_000);
    one.ready();
    await nextTurn();
    jest.advanceTimersByTime(29_000);
    expect(one.killedWith()).toBeUndefined();
    one.print("first\n");
    one.exit(0);
    expect(await first).toMatchObject({ output: "first\n", exitCode: 0 });

    const two = await started(2);
    expect(events).toEqual(["spawn 1", "exit 1", "spawn 2"]);
    // Its setup takes 25 s, then its command 1 ms short of 30 s: alive, and it answers.
    jest.advanceTimersByTime(25_000);
    two.ready();
    await nextTurn();
    jest.advanceTimersByTime(29_999);
    expect(two.killedWith()).toBeUndefined();
    two.print("second\n");
    two.exit(0);
    expect(await second).toMatchObject({ output: "second\n", exitCode: 0 });
    expect(events).toEqual(["spawn 1", "exit 1", "spawn 2", "exit 2"]);
  });
});

// Catches: a command past its 30 s, or a sandbox never made, left running so the queue never moves
// on; or the command's 30 s counted from the spawn, or the setup's from the wait, so a command is
// ended early or a sandbox that is never made is never ended; or such a call given an answer,
// where replay.ts's checkout round takes `why` as no verdict, `unanswered-read`, and makes the
// run again.
test("sandbox queue: a command 30 s past its ready marker, and a sandbox not made 30 s after its spawn, are ended and not answered, and the next command then starts", async () => {
  const copy = filesCopy("sandbox-queue-limits", { "docs/guide.md": GUIDE });
  await onFakeSandboxes(2, async ({ events, started, track }) => {
    const first = track(answerCall(copy, "bash", { command: "cat docs/guide.md" }, "hashline"));
    const second = track(
      answerCall(copy, "bash", { command: "head -n 1 docs/guide.md" }, "hashline")
    );
    const one = await started(1);
    // Its setup takes 10 s, so its command has until 30 s after the marker, not after the spawn.
    jest.advanceTimersByTime(10_000);
    one.ready();
    await nextTurn();
    jest.advanceTimersByTime(29_999);
    expect(one.killedWith()).toBeUndefined();
    jest.advanceTimersByTime(1);
    expect(await first).toEqual({ why: "it ran past the 30 s limit" });
    expect(one.killedWith()).toBe("SIGKILL");

    // The second waited 40 s; its limit counts from its spawn.
    const two = await started(2);
    expect(events).toEqual(["spawn 1", "kill 1", "spawn 2"]);
    jest.advanceTimersByTime(29_999);
    expect(two.killedWith()).toBeUndefined();
    jest.advanceTimersByTime(1);
    expect(await second).toEqual({ why: "it ran past the 30 s limit" });
    expect(two.killedWith()).toBe("SIGKILL");
    expect(events).toEqual(["spawn 1", "kill 1", "spawn 2", "kill 2"]);
  });
});
