import { afterAll, beforeAll, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// Check k7's Slack poster (slack.ts) prints the status and Slack's answer on one line, which
// airlock-check.sh reads for Slack's refusal of the check's fake token. Something in Slack's place
// - a proxy, a captive portal - can answer with a page of its own that echoes the request back,
// the bearer with it: the line names such a page with that bearer redacted, and Slack's own JSON
// as Slack wrote it. The script calls slack.com itself, so each run here is a process whose
// preload sends its one fetch to the stand-in below instead.

const token = fixtureSecret("slack");
const work = temporaryDirectory("airlock-slack-test");
const preload = join(work, "stand-in-slack.ts");

let answer: (request: Request) => Response = () => new Response("unset", { status: 500 });
const standIn = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: (request) => answer(request) });
afterAll(() => standIn.stop(true));

beforeAll(() => {
  writeFileSync(
    preload,
    `const real = globalThis.fetch;
globalThis.fetch = ((input: string | URL, init?: RequestInit) =>
  real(new URL(new URL(String(input)).pathname, process.env.STAND_IN_SLACK), init)) as typeof fetch;
`
  );
});

/** What one run of slack.ts printed, as airlock-check.sh reads it: its last line. */
async function printed(): Promise<string> {
  const child = Bun.spawn(["bun", "--preload", preload, join(import.meta.dir, "slack.ts")], {
    env: {
      ...process.env,
      SLACK_BOT_TOKEN: token,
      STAND_IN_SLACK: `http://127.0.0.1:${standIn.port}`,
    },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  expect(stderr).toBe("");
  expect(code).toBe(0);
  return stdout.trimEnd().split("\n").at(-1) ?? "";
}

test("a page in Slack's place that echoes the bearer is named in one line, the bearer redacted", async () => {
  answer = (request) => {
    const authorization = request.headers.get("authorization") ?? "";
    return new Response(
      `<html><body>\n${authorization.replace(/^Bearer /, "")}\n<pre>POST /api/auth.test\n` +
        `authorization: ${authorization}</pre></body></html>\n`,
      { headers: { "Content-Type": "text/html" }, status: 403 }
    );
  };
  const line = await printed();
  expect(line).toStartWith("403 a body that is not Slack's JSON (");
  expect(line).not.toContain(token);
});

test("Slack's own refusal prints as Slack wrote it", async () => {
  answer = () => Response.json({ ok: false, error: "invalid_auth" });
  expect(await printed()).toBe('200 {"ok":false,"error":"invalid_auth"}');
});
