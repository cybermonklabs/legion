import { afterAll, expect, test } from "bun:test";
import { chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// dispatch-recorder.ts stands in front of the real `dispatch` and records, for each invocation, the
// requests that invocation sent Dispatch and the CLI's own reading of its argv. The CLI here is a
// stand-in of the CLI's wire behaviour alone (it sends the requests a command sends); the recorder
// reads argv with the real CLI's parser.

const work = temporaryDirectories("dispatch-recorder");
const RECORDER = path.join(import.meta.dir, "dispatch-recorder.ts");

/** What each `dispatch` command of the stand-in CLI sends: `read` one GET, `comment` a GET and a
 * POST, a dry run nothing; any other command exits with status 3 and prints to both streams. */
const CLI = `#!${process.execPath}
const [command, ...rest] = process.argv.slice(2);
const base = process.env.DISPATCH_URL ?? "";
const issue = rest[rest.indexOf("--issue") + 1];
const send = async (method, path) => {
  const answered = await fetch(base + path, {
    method,
    headers: { authorization: "Bearer stand-in" },
    body: method === "GET" ? undefined : '{"body":"hi"}',
  });
  return answered.text();
};
if (rest.includes("--dry-run")) console.log("dry run");
else if (command === "read") {
  console.log(await send("GET", "/api/v1/issues/" + issue));
} else if (command === "comment") {
  await send("GET", "/api/v1/issues/" + issue);
  console.log(await send("POST", "/api/v1/issues/" + issue + "/comments"));
} else {
  console.log("to stdout");
  console.error("to stderr");
  process.exit(3);
}
`;

/** The Dispatch the CLI reaches: what it was sent, and an answer that names the request. */
const received: { method: string; path: string; authorization: string | null; body: string }[] = [];
/** While set, Dispatch answers no request until `size` have arrived, so that many invocations are
 * all in flight at once, whatever the machine's speed. */
let barrier: { size: number; arrived: (() => void)[] } | undefined;
const dispatch = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    if (barrier !== undefined) {
      const { promise, resolve } = Promise.withResolvers<void>();
      barrier.arrived.push(resolve);
      if (barrier.arrived.length === barrier.size) for (const release of barrier.arrived) release();
      await promise;
    }
    received.push({
      method: request.method,
      path: url.pathname,
      authorization: request.headers.get("authorization"),
      body: await request.text(),
    });
    return new Response(`answered ${request.method} ${url.pathname}`, { status: 201 });
  },
});
afterAll(() => dispatch.stop(true));

/** A run directory with the recorder's shim and the real CLI's directory, and `run(args)`, which
 * runs the shim as the pane does, with the recorder's directory first on PATH. */
function pane(env: Record<string, string | undefined> = {}) {
  const dir = work();
  const own = path.join(dir, "state", "bin");
  const real = path.join(dir, "plugin", "bin");
  mkdirSync(own, { recursive: true });
  mkdirSync(real, { recursive: true });
  writeFileSync(
    path.join(own, "dispatch"),
    `#!/bin/sh\nexec ${process.execPath} ${RECORDER} ${dir} ${own} "$@"\n`
  );
  writeFileSync(path.join(real, "dispatch"), CLI);
  chmodSync(path.join(own, "dispatch"), 0o755);
  chmodSync(path.join(real, "dispatch"), 0o755);
  const run = async (...args: string[]) => {
    const child = Bun.spawn([path.join(own, "dispatch"), ...args], {
      env: {
        PATH: `${own}:${real}:${path.dirname(process.execPath)}:/usr/bin:/bin`,
        HOME: dir,
        DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
        ...env,
      },
      stdout: "pipe",
      stderr: "pipe",
    });
    const [stdout, stderr, exit] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    return { stdout, stderr, exit };
  };
  const rows = () =>
    readFileSync(path.join(dir, "calls.jsonl"), "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line));
  return { run, rows };
}

test("an invocation runs the real CLI as it is and records the requests it sent and the CLI's reading of it", async () => {
  const { run, rows } = pane();
  const before = received.length;
  const read = await run("read", "--issue", "ACME-12");
  expect(read).toEqual({
    stdout: "answered GET /api/v1/issues/ACME-12\n",
    stderr: "",
    exit: 0,
  });
  // Dispatch was sent the request as it came, its credential included.
  expect(received.slice(before)).toEqual([
    { method: "GET", path: "/api/v1/issues/ACME-12", authorization: "Bearer stand-in", body: "" },
  ]);
  const write = await run("comment", "--issue", "ACME-12", "--body", "A note.");
  expect(write.exit).toBe(0);
  expect(received.slice(before).map(({ method, path }) => `${method} ${path}`)).toEqual([
    "GET /api/v1/issues/ACME-12",
    "GET /api/v1/issues/ACME-12",
    "POST /api/v1/issues/ACME-12/comments",
  ]);
  expect(received.at(-1)?.body).toBe('{"body":"hi"}');
  const [first, second] = rows();
  expect(first).toMatchObject({
    as: "dispatch",
    argv: ["read", "--issue", "ACME-12"],
    exit: 0,
    requests: [{ method: "GET", path: "/api/v1/issues/ACME-12" }],
    call: { command: "read", flags: { issue: "ACME-12" }, dryRun: false },
  });
  expect(Date.parse(first.done)).toBeGreaterThanOrEqual(Date.parse(first.at));
  expect(second).toMatchObject({
    requests: [
      { method: "GET", path: "/api/v1/issues/ACME-12" },
      { method: "POST", path: "/api/v1/issues/ACME-12/comments" },
    ],
    call: { command: "comment", flags: { issue: "ACME-12", body: "A note." }, dryRun: false },
  });
});

test("the CLI's exit status and both its streams pass through, a command it reads as none has no reading, and a dry run sends nothing", async () => {
  const { run, rows } = pane();
  expect(await run("explode")).toEqual({ stdout: "to stdout\n", stderr: "to stderr\n", exit: 3 });
  expect((await run("read", "--issue", "ACME-12", "--dry-run")).stdout).toBe("dry run\n");
  const [refused, dry] = rows();
  expect(refused).toMatchObject({ exit: 3, requests: [], call: null });
  expect(dry).toMatchObject({
    exit: 0,
    requests: [],
    call: { command: "read", dryRun: true },
  });
});

// Two invocations at once each keep the requests that are theirs: a window of time around each
// would hold both, and a proxy of the invocation's own holds one.
test("invocations that run at once each record only their own requests", async () => {
  const { run, rows } = pane();
  const issues = ["ACME-1", "ACME-2", "ACME-3"];
  barrier = { size: issues.length, arrived: [] };
  await Promise.all(issues.map((issue) => run("read", "--issue", issue)));
  barrier = undefined;
  const recorded = rows().map((row) => [row.call.flags.issue, row.requests]);
  expect(recorded.toSorted()).toEqual(
    issues.map((issue) => [issue, [{ method: "GET", path: `/api/v1/issues/${issue}` }]])
  );
});

test("with no Dispatch address the CLI runs unproxied and its requests are unknown", async () => {
  const { run, rows } = pane({ DISPATCH_URL: undefined });
  const ran = await run("explode");
  expect(ran.exit).toBe(3);
  expect(rows()).toMatchObject([{ exit: 3, requests: null }]);
});
