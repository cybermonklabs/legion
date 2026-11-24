// label-checkouts.sh's labels and hidden labels, on a repository of its own: one ablate label,
// ablate-x, whose patch deletes the one line of docs/rule.md. The labels are the patches' names. A
// `--hidden <label>` gives `<label>-hidden` a checkout of its own with <label>'s content; the
// control is the script with its hidden copy of ablate-x made from head's content, which step 4
// must catch.
//
//   bun test label-checkouts.test.ts

import { beforeAll, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { spawnWithin } from "@legion/test-fixtures/row-clock";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { fileRowLimit, LABEL_CHECKOUTS, VCS_COMMAND } from "./test-limit";

// Each row clones the fixture repository three or four times and runs `bun install` in each
// clone, so each test and hook gets the limit test-limit.ts computes from the bounds of what they
// wait for.
setDefaultTimeout(fileRowLimit(LABEL_CHECKOUTS, LABEL_CHECKOUTS));

const SCRIPT = path.join(import.meta.dir, "label-checkouts.sh");
const scratch = temporaryDirectory("label-checkouts", { real: true });
const repo = path.join(scratch, "repo");
let commit = "";

function git(cwd: string, ...args: string[]): string {
  const run = spawnWithin(
    { ...VCS_COMMAND, part: `git ${args.join(" ")}` },
    [
      "git",
      "-c",
      "user.name=Fixture",
      "-c",
      "user.email=fixture@example.invalid",
      "-c",
      "commit.gpgsign=false",
      ...args,
    ],
    { cwd, stdout: "pipe", stderr: "pipe" }
  );
  if (run.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${run.stderr.toString()}`);
  return run.stdout.toString().trim();
}

beforeAll(() => {
  mkdirSync(path.join(repo, "docs"), { recursive: true });
  writeFileSync(path.join(repo, "package.json"), '{"name":"fixture","private":true}\n');
  writeFileSync(path.join(repo, "docs/rule.md"), "The rule.\n");
  git(scratch, "init", "-q", "-b", "main", repo);
  git(repo, "add", "-A");
  git(repo, "commit", "-qm", "fixture");
  commit = git(repo, "rev-parse", "HEAD");
  writeFileSync(
    path.join(scratch, "ablate-x.patch"),
    [
      "diff --git a/docs/rule.md b/docs/rule.md",
      "--- a/docs/rule.md",
      "+++ b/docs/rule.md",
      "@@ -1 +0,0 @@",
      "-The rule.",
      "",
    ].join("\n")
  );
  writeFileSync(
    path.join(scratch, "ablate-z.patch"),
    [
      "diff --git a/docs/other.md b/docs/other.md",
      "--- a/docs/other.md",
      "+++ b/docs/other.md",
      "@@ -1 +0,0 @@",
      "-A line the commit does not hold.",
      "",
    ].join("\n")
  );
});

/** label-checkouts.sh (or `script`) on the fixture, its scratch directories under a TMPDIR of the
 * row's own, so each row reads only its own labels.tsv. */
function run(row: string, args: string[], script = SCRIPT) {
  const tmp = path.join(scratch, row);
  mkdirSync(tmp);
  const result = spawnWithin(LABEL_CHECKOUTS, ["bash", script, repo, commit, ...args], {
    env: { ...process.env, TMPDIR: tmp },
    stdout: "pipe",
    stderr: "pipe",
  });
  const out = result.stdout.toString();
  const err = result.stderr.toString();
  const tsv = /list (\S+)$/m.exec(out)?.[1];
  const rows = tsv === undefined ? [] : readFileSync(tsv, "utf8").trim().split("\n");
  const checkouts = new Map(rows.map((line) => line.split("\t") as [string, string]));
  const head = /head (\S+), labels/.exec(out)?.[1];
  return { code: result.exitCode, out, err, checkouts, head };
}

// Catches: a hidden label that shares its source's checkout (score.ts leaves unscored a run that
// reads inside another label's checkout), or whose checkout lacks its source's patch.
test("each hidden label gets a checkout of its own, with its source's content", () => {
  const { code, out, err, checkouts, head } = run("hidden", [
    "--hidden",
    "head",
    "--hidden",
    "ablate-x",
    path.join(scratch, "ablate-x.patch"),
  ]);
  expect({ code, err }).toEqual({ code: 0, err: "" });
  expect([...checkouts.keys()]).toEqual(["ablate-x", "head-hidden", "ablate-x-hidden"]);
  const dirs = [...checkouts.values()];
  expect(new Set([...dirs, head]).size).toBe(4);
  const rule = (dir: string | undefined) =>
    readFileSync(path.join(dir ?? "", "docs/rule.md"), "utf8");
  expect(rule(checkouts.get("ablate-x-hidden"))).toBe("");
  expect(rule(checkouts.get("ablate-x"))).toBe("");
  expect(rule(checkouts.get("head-hidden"))).toBe("The rule.\n");
  expect(out).toContain(
    "label-checkouts: head-hidden: the same commit, diff and status as head's checkout"
  );
  expect(out).toContain(
    "label-checkouts: ablate-x-hidden: the same commit, diff and status as ablate-x's checkout"
  );
});

// The control of step 5: the script with ablate-x-hidden made from head's content, a copy that
// would measure the head's words under the control's name.
test("a hidden label whose checkout holds other content than its source's is a FAULT", () => {
  const source = readFileSync(SCRIPT, "utf8");
  const line = `for source in "\${hidden[@]}"; do new_checkout "$source-hidden" "$source"; done`;
  expect(source).toContain(line);
  const mutant = path.join(scratch, "label-checkouts-mutant.sh");
  writeFileSync(mutant, source.replace(line, line.replace('"$source"; done', "head; done")));
  const { code, err } = run(
    "mutant",
    ["--hidden", "ablate-x", path.join(scratch, "ablate-x.patch")],
    mutant
  );
  expect(code).toBe(1);
  expect(err).toMatch(
    /ablate-x-hidden: FAULT, its checkout \S+ holds other content than ablate-x's/
  );
});

test("a --hidden that names no label of the patches, or one label twice, is refused", () => {
  const patch = path.join(scratch, "ablate-x.patch");
  const none = run("none", ["--hidden", "ablate-y", patch]);
  expect(none.code).toBe(2);
  expect(none.err).toContain("--hidden ablate-y names neither head nor a label of the patches");
  const twice = run("twice", ["--hidden", "ablate-x", "--hidden", "ablate-x", patch]);
  expect(twice.code).toBe(2);
  expect(twice.err).toContain("--hidden ablate-x is named twice");
});

test("with no --hidden, labels.tsv lists each patch's label alone", () => {
  const { code, checkouts } = run("plain", [path.join(scratch, "ablate-x.patch")]);
  expect(code).toBe(0);
  expect([...checkouts.keys()]).toEqual(["ablate-x"]);
});

// Catches: a label's checkout made from a patch that does not apply to the commit, which would
// measure the head's words under the label's name.
test("a patch that does not apply at the commit is refused", () => {
  const { code, err } = run("unapplied", [path.join(scratch, "ablate-z.patch")]);
  expect(code).toBe(2);
  expect(err).toContain("the patch of ablate-z does not apply at");
});
