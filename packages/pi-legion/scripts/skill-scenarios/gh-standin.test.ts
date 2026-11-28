import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// Every row runs gh-standin.ts as a scenario's agent does, a process of its own, against a run
// directory holding the row's own fixtures.json.
const work = temporaryDirectory("gh-standin");
let runs = 0;

/** A new run directory whose fixtures.json holds `routes`. */
function runWith(routes: object[]): string {
  const run = path.join(work, `run-${++runs}`);
  mkdirSync(run);
  writeFileSync(path.join(run, "fixtures.json"), JSON.stringify({ routes }));
  return run;
}

/** `gh <args>`, answered by the stand-in from the fixtures of `run`. */
function gh(run: string, ...args: string[]) {
  const result = Bun.spawnSync(
    [process.execPath, path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
    { env: { PATH: process.env.PATH ?? "", SKILL_SCENARIO_RUN: run } }
  );
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

// `--json` picks the fields it names of each object of a list answer, as `gh pr list --json` does,
// and of an object answer, and `--jq` reads the picked list.
test("--json picks the named fields of each object of a list answer and of an object answer", () => {
  const run = runWith([
    {
      match: "^gh pr list",
      stdout: [
        { number: 7, url: "https://example.invalid/7", body: "seven" },
        { number: 8, url: "https://example.invalid/8", body: "eight" },
      ],
    },
    {
      match: "^gh pr view",
      stdout: { number: 7, url: "https://example.invalid/7", body: "seven" },
    },
  ]);
  expect(gh(run, "pr", "list", "--json", "number,url")).toEqual({
    code: 0,
    stdout:
      '[{"number":7,"url":"https://example.invalid/7"},{"number":8,"url":"https://example.invalid/8"}]\n',
    stderr: "",
  });
  expect(gh(run, "pr", "list", "--json", "number", "--jq", ".[].number").stdout).toBe("7\n8\n");
  expect(gh(run, "pr", "view", "7", "--json", "url").stdout).toBe(
    '{"url":"https://example.invalid/7"}\n'
  );
});

// A pull request body edit reaches every object that has a body, in a list answer as in an object
// answer, so the next `gh pr list --json body` reads the new body, as GitHub's would; a list whose
// objects have no body is answered as it was.
test("a body edit gives each object of a list answer that has a body the new body", () => {
  const run = runWith([
    { match: "^gh pr edit", stdout: "https://example.invalid/7" },
    { match: "^gh pr list", stdout: [{ number: 7, body: "old" }] },
    { match: "^gh pr view", stdout: { number: 7, body: "old" } },
    { match: "^gh api .*comments", stdout: [{ id: 1 }] },
  ]);
  expect(gh(run, "pr", "edit", "7", "--body", "new").code).toBe(0);
  expect(gh(run, "pr", "list", "--json", "number,body").stdout).toBe(
    '[{"number":7,"body":"new"}]\n'
  );
  expect(gh(run, "pr", "view", "7", "--json", "body").stdout).toBe('{"body":"new"}\n');
  expect(gh(run, "api", "repos/example/widgets/issues/7/comments").stdout).toBe('[{"id":1}]\n');
  const [edit] = readFileSync(path.join(run, "calls.jsonl"), "utf8").trimEnd().split("\n");
  expect(JSON.parse(edit ?? "{}")).toMatchObject({
    argv: ["pr", "edit", "7", "--body", "new"],
    body: "new",
    exit: 0,
  });
});
