// dispatch-proxy.ts as the rig runs it: a real process between a client and a stub of the run's
// Dispatch, serving a planted recording of the plans read. The recording has the shape of the
// body the plan ratings proof keeps (`plan_ratings_live_test.go`'s capture): the project key and
// each issue key stand in `project`, `issues[i].issue` and `issues[i].plan.issue`.
import { expect, setDefaultTimeout, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { loopbackHost } from "@legion/test-fixtures/loopback-host";
import { readStandinBody } from "@legion/test-fixtures/standin-body";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server, Subprocess } from "bun";
import { z } from "zod";
import { DispatchProxyLine, type PlansAnswerSource } from "./scored-runs-contracts";

setDefaultTimeout(60_000);

const PROXY = path.join(import.meta.dir, "dispatch-proxy.ts");
const freshDir = temporaryDirectories("dispatch-proxy");

/** The run's project, and the project and keys the planted recording was captured under. */
const RUN_PROJECT = "LWEVAL";
const CAPTURED = "SRPLANT7";
const KEY_MAP: Record<string, string> = {
  [CAPTURED]: RUN_PROJECT,
  [`${CAPTURED}-1`]: `${RUN_PROJECT}-41`,
  [`${CAPTURED}-3`]: `${RUN_PROJECT}-43`,
  [`${CAPTURED}-2`]: `${RUN_PROJECT}-42`,
};

/** JSON as Go's encoder writes a body: `<` and `&` escaped, and a newline at the end. */
function goJson(value: unknown): string {
  return `${JSON.stringify(value).replaceAll("<", "\\u003c").replaceAll("&", "\\u0026")}\n`;
}

/** A plans answer of the captured shape, under `project` with its issues mapped by `key`. A
 *  reason names `<project>-12`, a key the map does not hold that starts with one it holds, which
 *  must stay as it is. */
function plansAnswer(project: string, key: (n: number) => string, planIds: readonly string[]) {
  return {
    project,
    issues: [1, 3, 2].map((n, i) => ({
      issue: key(n),
      title: `Planted issue ${n}`,
      closed_at: "2026-11-24T12:03:46.643708Z",
      plan: {
        id: planIds[i],
        issue: key(n),
        generation: 1,
        source: "default",
        artifact_id: null,
        version: null,
        tasks: [
          {
            id: "default",
            steps: ["planner", "implementer", "tester", "reviewer", "acceptance:owner"],
          },
        ],
      },
      ratings: [
        {
          rating: 3,
          reason: `The plan fit; ${CAPTURED}-12 stays, in <1 day & with no new task.`,
          step_id: planIds[i],
          ended_at: "2026-11-24T12:03:46.233434374Z",
        },
      ],
      started_at: "2026-11-24T12:03:44.68222Z",
      ended_at: "2026-11-24T12:03:46.63200078Z",
      duration_seconds: 1,
      failed_steps: n === 1 ? 1 : 0,
    })),
  };
}

interface Recording {
  readonly answerFile: string;
  /** `plans-answer.source`, beside the answer, where the proxy reads it. */
  readonly sourceFile: string;
  /** The body the proxy must serve: the recording with every key mapped. */
  readonly served: string;
}

/** Plants the recorded answer and its `plans-answer.source`, whose sha256 is `sha256` when given
 *  and the answer's own otherwise. */
function plantRecording(dir: string, sha256?: string): Recording {
  const planIds = [randomUUID(), randomUUID(), randomUUID()];
  const raw = goJson(plansAnswer(CAPTURED, (n) => `${CAPTURED}-${n}`, planIds));
  const answerFile = path.join(dir, "plans-answer.json");
  writeFileSync(answerFile, raw);
  const source: PlansAnswerSource = {
    commit: "0".repeat(40),
    sha256: sha256 ?? createHash("sha256").update(raw).digest("hex"),
    keyMap: KEY_MAP,
  };
  const sourceFile = path.join(dir, "plans-answer.source");
  writeFileSync(sourceFile, JSON.stringify(source));
  const served = goJson(plansAnswer(RUN_PROJECT, (n) => `${RUN_PROJECT}-4${n}`, planIds));
  return { answerFile, sourceFile, served };
}

interface Seen {
  readonly method: string;
  readonly path: string;
  readonly query: string;
  readonly authorization: string | null;
  readonly body: string;
}

/** What the stub upstream answers `/api/v1/compressed` with: a body gzipped by the server, as a
 *  server sends one to a client that accepts it. */
const COMPRESSED_BODY = Bun.gzipSync(
  new TextEncoder().encode(JSON.stringify({ from: "upstream", compressed: true }))
);

/** A stub of the run's Dispatch that records each request whole and answers it by its path. */
function stubUpstream(): {
  readonly server: Server<undefined>;
  readonly url: string;
  readonly seen: Seen[];
} {
  const seen: Seen[] = [];
  const server = Bun.serve({
    hostname: loopbackHost(),
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      const read = await readStandinBody(request);
      if (read.refusal !== undefined) return read.refusal;
      seen.push({
        method: request.method,
        path: url.pathname,
        query: url.search.replace(/^\?/, ""),
        authorization: request.headers.get("authorization"),
        body: request.method === "POST" ? JSON.stringify(read.body) : "",
      });
      if (url.pathname === "/api/v1/issues") {
        return Response.json(
          { issues: [{ key: `${RUN_PROJECT}-1` }], from: "upstream" },
          { headers: { "X-Upstream": "issues" } }
        );
      }
      if (url.pathname === "/api/v1/compressed") {
        return new Response(COMPRESSED_BODY, {
          headers: { "Content-Encoding": "gzip", "Content-Type": "application/json" },
        });
      }
      if (request.method === "POST" && url.pathname.endsWith("/comments")) {
        return Response.json({ received: read.body }, { status: 201 });
      }
      if (/^\/api\/v1\/projects\/[^/]+\/plans$/.test(url.pathname)) {
        return Response.json({ from: "upstream", path: url.pathname });
      }
      return Response.json(
        { code: "NOT_FOUND", error: `no route for ${request.method} ${url.pathname}` },
        { status: 404 }
      );
    },
  });
  return { server, url: server.url.origin, seen };
}

/** Waits for the proxy's `listening on` line and answers its URL, or throws with its stderr when
 *  it exits first. */
async function listening(child: Subprocess<"ignore", "pipe", "pipe">): Promise<string> {
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  let text = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) {
      const code = await child.exited;
      const stderr = await new Response(child.stderr).text();
      throw new Error(`the proxy exited ${code} before it listened: ${stderr}`);
    }
    text += decoder.decode(value, { stream: true });
    const match = /^listening on (http:\/\/127\.0\.0\.1:\d+)$/m.exec(text);
    if (match?.[1] !== undefined) {
      reader.releaseLock();
      return match[1];
    }
  }
}

/** Starts the proxy in front of `upstream` with the planted recording, and runs `body` with its
 *  URL and its log; the process is stopped when `body` ends. */
async function withProxy(
  dir: string,
  upstream: string,
  recording: Recording,
  body: (url: string, log: string) => Promise<void>
): Promise<void> {
  const log = path.join(dir, "dispatch-proxy.jsonl");
  const child = Bun.spawn(
    [process.execPath, PROXY, "0", upstream, log, "--plans", recording.answerFile, RUN_PROJECT],
    { stdin: "ignore", stdout: "pipe", stderr: "pipe" }
  );
  try {
    await body(await listening(child), log);
  } finally {
    child.kill();
    await child.exited;
  }
}

/** Every line of the proxy's log, each held to C1's shape, without its time. */
function logged(log: string) {
  return readFileSync(log, "utf8")
    .trim()
    .split("\n")
    .map((line) => {
      const { at: _, ...rest } = DispatchProxyLine.parse(JSON.parse(line));
      return rest;
    });
}

// Catches: a proxy that forwards the plans read to the run's Dispatch, serves it for one query
// alone, leaves a captured key in the body, or replaces a key inside a longer key
// (`SRPLANT7-12`), and one that changes the recording's other bytes.
test("the proxy serves the recorded plans answer for the run's project, whatever its query, with the keys mapped", async () => {
  const dir = freshDir();
  const recording = plantRecording(dir);
  const upstream = stubUpstream();
  try {
    await withProxy(dir, upstream.url, recording, async (url, log) => {
      const queries = ["limit=20", "since=2026-11-01T00:00:00Z&limit=3", ""];
      for (const query of queries) {
        const route = `/api/v1/projects/${RUN_PROJECT}/plans${query === "" ? "" : `?${query}`}`;
        const response = await fetch(`${url}${route}`, {
          headers: { Authorization: `Bearer ${fixtureSecret("dsp")}` },
        });
        expect(response.status).toBe(200);
        expect(await response.text()).toBe(recording.served);
      }
      expect(recording.served).not.toContain(`"${CAPTURED}`);
      expect(recording.served).toContain(`${CAPTURED}-12 stays`);
      expect(upstream.seen).toEqual([]);
      expect(logged(log)).toEqual(
        queries.map((query) => ({
          method: "GET",
          path: `/api/v1/projects/${RUN_PROJECT}/plans`,
          query,
          status: 200,
        }))
      );
    });
  } finally {
    upstream.server.stop(true);
  }
});

// Catches: a proxy that serves the recording on another path, `/api/v1/issues` among them, or for
// another project, that changes a request on its way (its method, path, query, body or bearer),
// that changes the answer on its way back, or that leaves a request out of its log.
test("every other request passes through and is logged", async () => {
  const dir = freshDir();
  const recording = plantRecording(dir);
  const upstream = stubUpstream();
  const bearer = `Bearer ${fixtureSecret("dsp")}`;
  try {
    await withProxy(dir, upstream.url, recording, async (url, log) => {
      const issues = await fetch(`${url}/api/v1/issues?project=${RUN_PROJECT}&status=open`, {
        headers: { Authorization: bearer },
      });
      expect(issues.status).toBe(200);
      expect(issues.headers.get("X-Upstream")).toBe("issues");
      expect(await issues.json()).toEqual({
        issues: [{ key: `${RUN_PROJECT}-1` }],
        from: "upstream",
      });

      const comment = { body: `planted ${randomUUID()}` };
      const posted = await fetch(`${url}/api/v1/issues/${RUN_PROJECT}-1/comments`, {
        method: "POST",
        headers: { Authorization: bearer, "Content-Type": "application/json" },
        body: JSON.stringify(comment),
      });
      expect(posted.status).toBe(201);
      expect(await posted.json()).toEqual({ received: comment });

      const other = await fetch(`${url}/api/v1/projects/OTHER/plans?limit=2`, {
        headers: { Authorization: bearer },
      });
      expect(other.status).toBe(200);
      expect(await other.json()).toEqual({
        from: "upstream",
        path: "/api/v1/projects/OTHER/plans",
      });

      const missing = await fetch(`${url}/api/v1/nothing`, { headers: { Authorization: bearer } });
      expect(missing.status).toBe(404);

      expect(upstream.seen).toEqual([
        {
          method: "GET",
          path: "/api/v1/issues",
          query: `project=${RUN_PROJECT}&status=open`,
          authorization: bearer,
          body: "",
        },
        {
          method: "POST",
          path: `/api/v1/issues/${RUN_PROJECT}-1/comments`,
          query: "",
          authorization: bearer,
          body: JSON.stringify(comment),
        },
        {
          method: "GET",
          path: "/api/v1/projects/OTHER/plans",
          query: "limit=2",
          authorization: bearer,
          body: "",
        },
        { method: "GET", path: "/api/v1/nothing", query: "", authorization: bearer, body: "" },
      ]);
      expect(logged(log)).toEqual([
        {
          method: "GET",
          path: "/api/v1/issues",
          query: `project=${RUN_PROJECT}&status=open`,
          status: 200,
        },
        {
          method: "POST",
          path: `/api/v1/issues/${RUN_PROJECT}-1/comments`,
          query: "",
          status: 201,
        },
        { method: "GET", path: "/api/v1/projects/OTHER/plans", query: "limit=2", status: 200 },
        { method: "GET", path: "/api/v1/nothing", query: "", status: 404 },
      ]);
    });
  } finally {
    upstream.server.stop(true);
  }
});

/** The exit, stdout and stderr of a proxy started with `recording`, which it must refuse before it
 *  listens. Its upstream is never reached. */
function startRefused(recording: Recording) {
  const result = Bun.spawnSync(
    [
      process.execPath,
      PROXY,
      "0",
      "http://127.0.0.1:9",
      path.join(freshDir(), "dispatch-proxy.jsonl"),
      "--plans",
      recording.answerFile,
      RUN_PROJECT,
    ],
    { stdin: "ignore", stdout: "pipe", stderr: "pipe", timeout: 50_000 }
  );
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

// Catches: a proxy that serves a recording its source does not vouch for, or one whose key map
// takes the recording's project to another project than the run's.
test("a recording whose sha256 is not its source's, or whose project the key map does not take to the run's, stops the proxy before it listens", () => {
  const recorded = "f".repeat(64);
  const changed = plantRecording(freshDir(), recorded);
  const actual = createHash("sha256").update(readFileSync(changed.answerFile)).digest("hex");
  const sums = startRefused(changed);
  expect(sums.code).toBe(2);
  expect(sums.stdout).not.toContain("listening on");
  expect(sums.stderr).toContain(recorded);
  expect(sums.stderr).toContain(actual);

  const elsewhere = plantRecording(freshDir());
  writeFileSync(
    elsewhere.sourceFile,
    JSON.stringify({
      commit: "0".repeat(40),
      sha256: createHash("sha256").update(readFileSync(elsewhere.answerFile)).digest("hex"),
      keyMap: { ...KEY_MAP, [CAPTURED]: "OTHER" },
    })
  );
  const project = startRefused(elsewhere);
  expect(project.code).toBe(2);
  expect(project.stderr).toContain(CAPTURED);
  expect(project.stderr).toContain("OTHER");
  expect(project.stderr).toContain(RUN_PROJECT);
});

// Catches: a proxy that starts on a recording that is no JSON, whose plans read it would answer
// with a body the CLI cannot read, or that refuses it for another reason than what it is.
test("a recording that is not JSON stops the proxy before it listens, naming it", () => {
  const recording = plantRecording(freshDir());
  const raw = "plans answer begin\nnot a body\n";
  writeFileSync(recording.answerFile, raw);
  writeFileSync(
    recording.sourceFile,
    JSON.stringify({
      commit: "0".repeat(40),
      sha256: createHash("sha256").update(raw).digest("hex"),
      keyMap: KEY_MAP,
    })
  );
  const refused = startRefused(recording);
  expect(refused.code).toBe(2);
  expect(refused.stdout).not.toContain("listening on");
  expect(refused.stderr).toContain(recording.answerFile);
  expect(refused.stderr).toContain("is no JSON");
});

// Catches: a proxy that decompresses an answer and hands it on still labelled compressed, or
// hands it on with another body than the server sent.
test("a compressed answer passes through unchanged", async () => {
  const dir = freshDir();
  const upstream = stubUpstream();
  try {
    await withProxy(dir, upstream.url, plantRecording(dir), async (url, log) => {
      const raw = await fetch(`${url}/api/v1/compressed`, {
        headers: { "Accept-Encoding": "gzip" },
        decompress: false,
      });
      expect(raw.status).toBe(200);
      expect(raw.headers.get("Content-Encoding")).toBe("gzip");
      expect(new Uint8Array(await raw.arrayBuffer())).toEqual(COMPRESSED_BODY);
      // A client that decodes it reads the server's own body.
      const decoded = await fetch(`${url}/api/v1/compressed`, {
        headers: { "Accept-Encoding": "gzip" },
      });
      expect(await decoded.json()).toEqual({ from: "upstream", compressed: true });
      expect(logged(log)).toEqual([
        { method: "GET", path: "/api/v1/compressed", query: "", status: 200 },
        { method: "GET", path: "/api/v1/compressed", query: "", status: 200 },
      ]);
    });
  } finally {
    upstream.server.stop(true);
  }
});

/** The proxy's own refusal of a request it could not hand on. */
const ProxyRefusal = z.object({ error: z.string() });

// Catches: a proxy that dies, hangs or answers 200 when the run's Dispatch is down, or leaves the
// failed request out of its log; the recording it serves needs no upstream.
test("an upstream that is down is answered 502 and logged, and the plans read is still served", async () => {
  const dir = freshDir();
  const recording = plantRecording(dir);
  // A port that was just closed: its server stops before the proxy starts.
  const gone = Bun.serve({ hostname: loopbackHost(), port: 0, fetch: () => new Response("") });
  const upstream = gone.url.origin;
  gone.stop(true);
  await withProxy(dir, upstream, recording, async (url, log) => {
    const issues = await fetch(`${url}/api/v1/issues?project=${RUN_PROJECT}`);
    expect(issues.status).toBe(502);
    expect(ProxyRefusal.parse(await issues.json()).error).toContain(upstream);
    const plans = await fetch(`${url}/api/v1/projects/${RUN_PROJECT}/plans`);
    expect(plans.status).toBe(200);
    expect(await plans.text()).toBe(recording.served);
    expect(logged(log)).toEqual([
      { method: "GET", path: "/api/v1/issues", query: `project=${RUN_PROJECT}`, status: 502 },
      { method: "GET", path: `/api/v1/projects/${RUN_PROJECT}/plans`, query: "", status: 200 },
    ]);
  });
});
