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

/** `gh <args>`, answered by the stand-in from the fixtures of `run`, with nothing on its stdin. */
function gh(run: string, ...args: string[]) {
  return ghWith(run, "", ...args);
}
/** `gh <args>` with STDIN on its stdin, as a `-F body=@-` call hands it a body. */
function ghWith(run: string, stdin: string, ...args: string[]) {
  const result = Bun.spawnSync(
    [process.execPath, path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
    {
      env: { PATH: process.env.PATH ?? "", SKILL_SCENARIO_RUN: run },
      stdin: new TextEncoder().encode(stdin),
    }
  );
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}
/** Each line of the run's call record, parsed. */
const recorded = (run: string) =>
  readFileSync(path.join(run, "calls.jsonl"), "utf8")
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line));

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

// A forward world's scorer reads a reply on a review thread and its resolution from this record
// (score.ts forwardOnly, W4): each must name the thread, by its node id over GraphQL and by its first
// comment's id over REST, with the reply's body as the call gave it.
test("a reply on a review thread, over GraphQL and over REST, and its resolution, are recorded with the thread", () => {
  const run = runWith([
    { match: "addPullRequestReviewThreadReply", stdout: { data: { addPullRequestReviewThreadReply: { comment: { id: "c1" } } } } },
    { match: "resolveReviewThread", stdout: { data: { resolveReviewThread: { thread: { isResolved: true } } } } },
    { match: "/replies", stdout: { id: 4300 } },
    { match: "^gh pr view", stdout: { number: 41 } },
  ]);
  const reply =
    "mutation($id:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$id,body:$body}){comment{id}}}";
  expect(gh(run, "api", "graphql", "-f", `query=${reply}`, "-F", "id=PRRT_1", "-f", "body=Reported as defect d-7.").code).toBe(0);
  const inline =
    'mutation{addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:"PRRT_2",body:"Declined: the \\"line\\" stays."}){comment{id}}}';
  expect(gh(run, "api", "graphql", "-f", `query=${inline}`).code).toBe(0);
  expect(
    ghWith(run, "Reported as defect d-8.", "api", "repos/acme/widgets/pulls/41/comments/4242/replies", "-F", "body=@-").code
  ).toBe(0);
  const resolve = "mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}";
  expect(gh(run, "api", "graphql", "-f", `query=${resolve}`, "-F", "id=PRRT_1").code).toBe(0);
  expect(gh(run, "pr", "view", "41").code).toBe(0);
  const lines = recorded(run);
  expect(lines.map((line) => ({ reply: line.reply ?? null, resolved: line.resolved ?? null }))).toEqual([
    { reply: { thread: "PRRT_1", body: "Reported as defect d-7." }, resolved: null },
    { reply: { thread: "PRRT_2", body: 'Declined: the "line" stays.' }, resolved: null },
    { reply: { thread: "4242", body: "Reported as defect d-8." }, resolved: null },
    { reply: null, resolved: "PRRT_1" },
    { reply: null, resolved: null },
  ]);
});

// GitHub also takes a thread's reply as a new review comment that names the thread's first comment
// in `in_reply_to`: the record names that comment as the thread.
test("a reply sent as a new review comment with in_reply_to is recorded with its thread", () => {
  const run = runWith([{ match: "pulls/41/comments", stdout: { id: 4301 } }]);
  expect(
    gh(run, "api", "repos/acme/widgets/pulls/41/comments", "-f", "body=Reported as defect d-7.", "-F", "in_reply_to=4242").code
  ).toBe(0);
  expect(gh(run, "api", "repos/acme/widgets/pulls/41/comments", "-f", "body=A new comment.").code).toBe(0);
  expect(recorded(run).map((line) => line.reply ?? null)).toEqual([
    { thread: "4242", body: "Reported as defect d-7." },
    null,
  ]);
});

// A forward world's pull request is at the head the label's test builds, which no fixture can know:
// a route names it `{{HEAD}}`, and the stand-in fills it from the run's ready.json.
test("a route that holds {{HEAD}} answers the head of ready.json, and fails while there is none", () => {
  const run = runWith([{ match: "^gh pr view", stdout: { number: 41, headRefOid: "{{HEAD}}" } }]);
  const missing = gh(run, "pr", "view", "41", "--json", "headRefOid");
  expect(missing.code).toBe(1);
  expect(missing.stderr).toContain(`${run}/ready.json holds no head`);
  const head = "c".repeat(40);
  writeFileSync(path.join(run, "ready.json"), JSON.stringify({ head, session: "ses_pane" }));
  expect(gh(run, "pr", "view", "41", "--json", "headRefOid")).toEqual({
    code: 0,
    stdout: `{"headRefOid":"${head}"}\n`,
    stderr: "",
  });
});
