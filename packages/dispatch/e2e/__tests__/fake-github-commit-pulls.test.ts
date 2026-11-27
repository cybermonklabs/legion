import { expect, test } from "bun:test";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { fileURLToPath } from "node:url";

import { withoutHarnessPort } from "../harness-ports";

// The fake GitHub (e2e/fake-github.ts) answers the pull requests of a commit as GitHub does, in
// pages: `GET /repos/{o}/{r}/commits/{sha}/pulls` lists them oldest first, `per_page` at a time
// (default 30, at most 100), from page `page`. Dispatch reads every page of that list, 32 at a time
// (delivery's fetchCommitPullPayloads): a fake that answered every pull request on every page read
// a commit of more than 32 as a list that never ends, so a test of the read past its first page
// could not pass against the fake, and a test below that bound could not tell a read of one page
// from a read of all of them. A commit of forty pull requests is past it.
//
// Each test starts a fake of its own on a port the OS chooses, as fake-envoy.test.ts does, and
// reads the port from the fake's listening line.
const fakeGitHubModule = fileURLToPath(new URL("../fake-github.ts", import.meta.url));

async function startFakeGitHub(): Promise<{ origin: string; stop: () => Promise<void> }> {
  const fake = spawn("bun", [fakeGitHubModule], {
    env: withoutHarnessPort("fakeGithub"),
    stdio: ["ignore", "pipe", "inherit"],
  });
  const exited = once(fake, "exit");
  const stop = async () => {
    fake.kill();
    await exited;
  };
  try {
    const ready = Promise.withResolvers<number>();
    let output = "";
    fake.stdout.setEncoding("utf8");
    fake.stdout.on("data", (chunk: string) => {
      output += chunk;
      const listening = /fake github listener on 127\.0\.0\.1:(\d+)\n/.exec(output);
      if (listening !== null) ready.resolve(Number(listening[1]));
    });
    fake.once("exit", (code) => {
      ready.reject(new Error(`the fake GitHub exited ${code} before it listened: ${output}`));
    });
    fake.once("error", ready.reject);
    const port = await ready.promise;
    return { origin: `http://127.0.0.1:${port}`, stop };
  } catch (error) {
    await stop();
    throw error;
  }
}

const repository = "acme/paged";
const commit = "c".repeat(40);
const numbers = Array.from({ length: 40 }, (_, index) => 101 + index);

/** A repository whose one commit GitHub names forty open pull requests for, oldest first. */
function seed(origin: string): Promise<Response> {
  return Bun.fetch(`${origin}/__fixture/repos`, {
    body: JSON.stringify({
      [repository]: {
        commit_pulls: { [commit]: numbers },
        contents: "read",
        installation_id: 4209,
        pull_requests: "read",
        pulls: Object.fromEntries(
          numbers.map((number) => [
            String(number),
            { head_sha: commit, state: "open", title: `work ${number}` },
          ])
        ),
      },
    }),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
}

/** The numbers of the pull requests one REST page of the commit's list names. */
async function restPage(origin: string, query: string): Promise<number[]> {
  const response = await Bun.fetch(`${origin}/repos/${repository}/commits/${commit}/pulls${query}`);
  expect(response.status).toBe(200);
  return ((await response.json()) as { number: number }[]).map((pull) => pull.number);
}

test("the REST list of a commit's pull requests is answered in GitHub's pages", async () => {
  const fake = await startFakeGitHub();
  try {
    expect((await seed(fake.origin)).status).toBeLessThan(300);
    // Dispatch's own pages: 32, then the 8 after them, then none.
    expect(await restPage(fake.origin, "?per_page=32&page=1")).toEqual(numbers.slice(0, 32));
    expect(await restPage(fake.origin, "?per_page=32&page=2")).toEqual(numbers.slice(32));
    expect(await restPage(fake.origin, "?per_page=32&page=3")).toEqual([]);
    // A page halved past the response cap starts where the page it halves started.
    expect(await restPage(fake.origin, "?per_page=16&page=3")).toEqual(numbers.slice(32, 40));
    // GitHub's default page is 30, and its largest 100.
    expect(await restPage(fake.origin, "")).toEqual(numbers.slice(0, 30));
    expect(await restPage(fake.origin, "?per_page=1000")).toEqual(numbers);
  } finally {
    await fake.stop();
  }
});
