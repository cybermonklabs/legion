import { beforeEach, expect, test } from "bun:test";
import { bearerCredential } from "../bearer";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

// A spec that binds its routes (`DispatchToolSpec.routes`) is served by one generic command: the
// request it sends is built from the binding alone, and what it prints is the route's answer.

beforeEach(forgetKeyedLayerMemory);

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};

const exec: ExecFn = async (file, args) => {
  if (file === "jj" && args.join(" ") === "git remote list") {
    return { stdout: "origin https://github.com/owner/repo.git\n" };
  }
  throw new Error(`unexpected command: ${file} ${args.join(" ")}`);
};

interface Sent {
  readonly method: string;
  readonly pathname: string;
  readonly search: URLSearchParams;
  readonly body: unknown;
}

/** A fetch that records each request and answers `answer` as JSON. */
function recording(answer: unknown): { readonly sent: Sent[]; readonly fetchImpl: typeof fetch } {
  const sent: Sent[] = [];
  const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input));
    sent.push({
      method: init?.method ?? "GET",
      pathname: url.pathname,
      search: url.searchParams,
      body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
    });
    return new Response(JSON.stringify(answer), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;
  return { sent, fetchImpl };
}

function run(tool: string, args: Record<string, unknown>, fetchImpl: typeof fetch) {
  return executeDispatchTool({
    tool,
    args,
    cwd: "/workspace",
    host: "omp",
    config,
    env: { LEGION_ISSUE: "DSP-9" },
    sessionId: "session-one",
    exec,
    fetchImpl,
  });
}

test("a bound write fills its path, sends its body arguments and its actor, and prints the answer", async () => {
  const answer = { id: "ship-it", title: "Ship it to everyone", rank: 2 };
  const { sent, fetchImpl } = recording(answer);
  const result = await run(
    "dispatch_edit_priority",
    { priority: "ship-it", title: "Ship it to everyone", rank: { after: "docs" } },
    fetchImpl
  );
  expect(sent).toHaveLength(1);
  expect(sent[0]?.method).toBe("PATCH");
  expect(sent[0]?.pathname).toBe("/api/v1/priorities/ship-it");
  expect(sent[0]?.body).toEqual({
    title: "Ship it to everyone",
    rank: { after: "docs" },
    actor: expect.objectContaining({ kind: "session", id: "session-one" }),
  });
  expect(result.text).toBe(JSON.stringify(answer));
  expect(result.details).toMatchObject({ route: "PATCH /api/v1/priorities/{id}" });
});

test("a bound read sends its query arguments, a list as one parameter per item, and no body", async () => {
  const answer = { related: [] };
  const { sent, fetchImpl } = recording(answer);
  const result = await run(
    "dispatch_find_related",
    {
      project: "DSP",
      title: "Renamed issues",
      exclude_issues: ["DSP-1", "DSP-2"],
      keywords_only: true,
    },
    fetchImpl
  );
  expect(sent).toHaveLength(1);
  expect(sent[0]?.method).toBe("GET");
  expect(sent[0]?.pathname).toBe("/api/v1/projects/DSP/related");
  expect(sent[0]?.search.get("title")).toBe("Renamed issues");
  expect(sent[0]?.search.getAll("exclude_issues")).toEqual(["DSP-1", "DSP-2"]);
  expect(sent[0]?.search.get("keywords_only")).toBe("true");
  expect(sent[0]?.search.has("project")).toBe(false);
  expect(sent[0]?.body).toBeUndefined();
  expect(result.text).toBe(JSON.stringify(answer));
});

test("a bound spec with several routes takes the first its arguments fill", async () => {
  const { sent, fetchImpl } = recording({ version: 4 });
  await run(
    "dispatch_name_version",
    { project: "DSP", document: "notes", summary: "Agreed" },
    fetchImpl
  );
  await run(
    "dispatch_name_version",
    { document: "01234567-0000-4000-8000-000000000001", summary: "Agreed" },
    fetchImpl
  );
  expect(sent.map((request) => `${request.method} ${request.pathname}`)).toEqual([
    "POST /api/v1/projects/DSP/artifacts/notes/versions",
    "POST /api/v1/artifacts/01234567-0000-4000-8000-000000000001/versions",
  ]);
});
