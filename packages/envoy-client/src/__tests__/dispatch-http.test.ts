import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import {
  type ArchitectureSource,
  type IssueSummary,
  type ItemBlocks,
  NO_ROUTE_ROLLING_RELEASE_MESSAGE,
  type Sleep,
  sleep,
} from "@legion/contracts";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { bearerCredential, type RequestCredential } from "../bearer";
import {
  DISPATCH_WRITE_RETRY,
  DispatchClient,
  DispatchGatewayError,
  DispatchNoRouteError,
  type DispatchWriteRetry,
  DispatchWriteUnsettledError,
  forgetKeyedLayerMemory,
  type SearchOptions,
} from "../dispatch-http";
import { fakeSearchResponse } from "./fake-search-response";

interface RecordedRequest {
  readonly url: string;
  readonly init: RequestInit;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function fakeFetch(responses: readonly Response[]) {
  const requests: RecordedRequest[] = [];
  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    requests.push({ url: String(input), init: init ?? {} });
    const response = responses[requests.length - 1];
    if (!response) throw new Error("unexpected request");
    return response;
  };
  return { fetchImpl: fetchImpl as typeof fetch, requests };
}

function requestBody(request: RecordedRequest): unknown {
  return JSON.parse(request.init.body as string);
}

/** A sleep that returns at once, for a test that does not time the waits. */
const noWait = async (_ms: number): Promise<void> => {};

/** Four no-route answers, one for each attempt of a request, so its fallback runs next. */
function noRoutes(answer: Response): Response[] {
  return Array.from({ length: 4 }, () => answer.clone());
}

const actor = {
  kind: "session" as const,
  id: "session-1",
  origin: { host: "omp" as const, cwd: "/workspace", session_title: "Implement" },
};

describe("DispatchClient", () => {
  // A summary carrying only what these tests read; the rest of the shape is left out.
  const issue = (key: string) => ({ key, title: key, status: "todo" }) as unknown as IssueSummary;

  test("asks for a page of the issue listing with the filters and the updated_since boundary", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ issues: [], total: 0, limit: 5, offset: 10 }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await client.listIssuePage(
      { project: "DSP", updated_since: "2026-09-10T12:00:00Z" },
      { limit: 5, offset: 10 }
    );

    expect(requests).toHaveLength(1);
    expect(requests[0]?.url).toContain(
      "/api/v1/issues?project=DSP&updated_since=2026-09-10T12%3A00%3A00Z&limit=5&offset=10"
    );
  });

  test("reads a paging Dispatch's page as served", async () => {
    const served = { issues: [issue("DSP-4")], total: 4, limit: 3, offset: 3 };
    const { fetchImpl } = fakeFetch([jsonResponse(served)]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    // The page a paging Dispatch cut is the page: nothing is sliced out of it again.
    await expect(
      client.listIssuePage({ project: "DSP" }, { limit: 3, offset: 3 })
    ).resolves.toEqual(served);
  });

  // A Dispatch that ignores limit and offset answers the whole listing as an array. That is a
  // server older than the paging listing or a regression of it, so the client refuses it rather
  // than read every issue to cut the page itself. The refusal names the Dispatch and the whole
  // request, since a repository can point the tool at a Dispatch of its own.
  test("refuses a bare array answered to a paged request, naming the request and both causes", async () => {
    const { fetchImpl } = fakeFetch([
      jsonResponse(["DSP-1", "DSP-2", "DSP-3", "DSP-4"].map(issue)),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(
      client.listIssuePage({ project: "DSP", status: "todo" }, { limit: 2, offset: 1 })
    ).rejects.toThrow(
      "GET http://dispatch.test/api/v1/issues?project=DSP&status=todo&limit=2&offset=1 asked for " +
        "a page ({issues, total, limit, offset}) and got a bare array of 4 entries, the unpaged " +
        "listing, which means that Dispatch is older than sjawhar/legion#1612 or that change has " +
        "regressed. Retrying will not help: the same request gets the same answer until that " +
        "Dispatch is upgraded or fixed."
    );
  });

  // A JSON answer that is not a page is Dispatch's own and comes back the same on a retry; text in
  // its place is most likely a proxy or gateway page, which a retry can get past, and the read
  // path refuses it before any listing reads it (`#response`). The agent acts on the advice, so
  // each kind gets its own.
  test("refuses an issue listing answer that is not a page, naming what arrived and whether a retry can help", async () => {
    const { fetchImpl } = fakeFetch([
      jsonResponse({ issues: "DSP-1", total: 1 }),
      jsonResponse(null),
      new Response("<html>bad gateway</html>", { status: 200 }),
      new Response("{truncat", { status: 200, headers: { "Content-Type": "application/json" } }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );
    const refusal = () =>
      client.listIssuePage({ project: "DSP" }, { limit: 50, offset: 0 }).then(
        () => "",
        (error: Error) => error.message
      );
    const sameAgain =
      ". Retrying will not help: the same request gets the same answer until that Dispatch is " +
      "upgraded or fixed.";
    const maySucceed =
      "which looks like a proxy or gateway page rather than Dispatch's own answer, so a retry may " +
      "succeed.";

    expect(await refusal()).toEndWith(
      `and got an object without an issues array or a numeric limit or a numeric offset${sameAgain}`
    );
    expect(await refusal()).toEndWith(`and got null${sameAgain}`);
    const html = await refusal();
    expect(html).toStartWith(
      'GET http://dispatch.test/api/v1/issues?project=DSP&limit=50&offset=0 answered 200 with a body that is not JSON ("bad gateway")'
    );
    const truncated = await refusal();
    expect(truncated).toStartWith(
      'GET http://dispatch.test/api/v1/issues?project=DSP&limit=50&offset=0 answered 200 with a body that is not JSON ("{truncat")'
    );
    for (const text of [html, truncated]) {
      expect(text).toEndWith(maySucceed);
      expect(text).not.toContain("Retrying will not help");
    }
  });

  // The executor prints `showing A-B of N` from the served limit and offset, so a page missing
  // either must be refused rather than read as `showing NaN-NaN of N`.
  test("refuses a page that does not name its limit and offset", async () => {
    const { fetchImpl } = fakeFetch([
      jsonResponse({ issues: [issue("DSP-2")], total: 5 }),
      jsonResponse({ issues: [issue("DSP-2")], total: 5, limit: 1 }),
      jsonResponse({ issues: [issue("DSP-2")], total: 5, limit: "1", offset: 1 }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    for (const lacking of [
      "a numeric limit or a numeric offset",
      "a numeric offset",
      "a numeric limit",
    ]) {
      await expect(
        client.listIssuePage({ project: "DSP" }, { limit: 1, offset: 1 })
      ).rejects.toThrow(`and got an object without ${lacking}. Retrying`);
    }
  });

  // `--all` exists to read the exact set, so a page answered to the unpaged request is refused by
  // name: read as the listing, its first rows would pass for every issue that matched.
  test("refuses a page answered to the unpaged request, naming the request and the cause", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ issues: [issue("DSP-1")], total: 251, limit: 50, offset: 0 }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(client.listAllIssues({ project: "DSP", status: "needs_review" })).rejects.toThrow(
      "GET http://dispatch.test/api/v1/issues?project=DSP&status=needs_review asked for the " +
        "unpaged listing and got a page, so this Dispatch pages a request that carries no limit " +
        "and no offset."
    );
    expect(requests).toHaveLength(1);
  });

  test("search encodes q, project, limit, and offset and returns the response body", async () => {
    const body = fakeSearchResponse(
      [
        {
          kind: "document" as const,
          owner: { key: "LEGION-2", kind: "issue" as const, title: "Astrolabe", status: "triage" },
          artifact: { slug: "spec", name: "spec.md" },
          id: "artifact-2",
          snippet: "<mark>astrolabe</mark>",
          rank: 1,
          href: "/issues/LEGION-2/spec?q=astrolabe",
        },
      ],
      { total: 1, reachable: 1, limit: 5, offset: 10, took_ms: 12 }
    );
    const { fetchImpl, requests } = fakeFetch([jsonResponse(body)]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(
      client.search("astrolabe sextant", { project: "LEGION", limit: 5, offset: 10 })
    ).resolves.toEqual(body);

    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]?.url).pathname + new URL(requests[0]?.url).search).toBe(
      "/api/v1/search?q=astrolabe+sextant&project=LEGION&limit=5&offset=10"
    );
    expect(requests[0]?.init).toMatchObject({
      method: "GET",
      headers: { Authorization: "Bearer secret", Accept: "application/json" },
    });
  });

  test("a search of q alone stays on /api/v1/search", async () => {
    const { fetchImpl, requests } = fakeFetch([jsonResponse(fakeSearchResponse([]))]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await client.search("astrolabe");

    expect(new URL(requests[0]?.url ?? "").pathname).toBe("/api/v1/search");
  });

  // A member `everyBuildReadsSearch` does not keep on `/api/v1/search` is one a Dispatch
  // released before the results route drops, answering unnarrowed rows under the filters a
  // person chose; that build answers the results path `404 no route` instead, which is a
  // refusal. `SearchOptions` carries no such member today, so the test puts one on the request
  // through a cast: the rule reads the request's own keys, so the day the command grows a
  // filter it is routed without anyone remembering to.
  test("a search carrying a member everyBuildReadsSearch drops asks for /api/v1/search/results", async () => {
    const { fetchImpl, requests } = fakeFetch([jsonResponse(fakeSearchResponse([]))]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await client.search("astrolabe", {
      project: "LEGION",
      status: "in_progress",
    } as SearchOptions);

    const url = new URL(requests[0]?.url ?? "");
    expect(url.pathname + url.search).toBe(
      "/api/v1/search/results?q=astrolabe&project=LEGION&status=in_progress"
    );
  });

  // `keywords_only` is the one flag among the five, and the path is picked from the members the
  // request carries. A boolean the URL dropped would ask for the results route and arrive
  // without the flag, so the server would answer by meaning the search asked it to skip: the
  // harm this route exists to stop, caused by the client rather than by an older build.
  test("a boolean member reaches the URL, so the flag the path was picked for is sent", async () => {
    const { fetchImpl, requests } = fakeFetch([jsonResponse(fakeSearchResponse([]))]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await client.search("astrolabe", { keywords_only: true } as SearchOptions);

    const url = new URL(requests[0]?.url ?? "");
    expect(url.pathname + url.search).toBe("/api/v1/search/results?q=astrolabe&keywords_only=true");
  });

  test("lists this session's open asks with an optional server baseline", async () => {
    const body = {
      session_id: "session-1",
      as_of: "2026-09-13T00:00:00Z",
      opened_since: false,
      count: 0,
      waiting_on_human: 0,
      waiting_on_agent: 0,
      asks: [],
    };
    const { fetchImpl, requests } = fakeFetch([jsonResponse(body)]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(client.openAsks("session-1", "2026-09-12T00:00:00Z")).resolves.toEqual(body);

    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]?.url).pathname + new URL(requests[0]?.url).search).toBe(
      "/api/v1/asks/open?author_session=session-1&since=2026-09-12T00%3A00%3A00Z"
    );
    expect(requests[0]?.init).toMatchObject({
      method: "GET",
      headers: { Authorization: "Bearer secret", Accept: "application/json" },
    });
  });

  test("lists a project's open asks across issues and documents", async () => {
    const body = {
      session_id: "",
      as_of: "2026-09-13T00:00:00Z",
      opened_since: false,
      count: 0,
      waiting_on_human: 0,
      waiting_on_agent: 0,
      asks: [],
    };
    const { fetchImpl, requests } = fakeFetch([jsonResponse(body)]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(client.openAsksForProject("CORE")).resolves.toEqual(body);

    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]?.url).pathname + new URL(requests[0]?.url).search).toBe(
      "/api/v1/asks/open?project=CORE"
    );
    expect(requests[0]?.init).toMatchObject({
      method: "GET",
      headers: { Authorization: "Bearer secret", Accept: "application/json" },
    });
  });

  test("reads a session's answers with since and max_chars, and the caller's signal aborts it", async () => {
    const body = { session_id: "session-1", since: "2026-10-24T00:00:00Z", total: 0, items: [] };
    const { fetchImpl, requests } = fakeFetch([jsonResponse(body), jsonResponse(body)]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );
    const controller = new AbortController();

    await expect(
      client.sessionAnswers("session-1", {
        since: "2026-10-24T00:00:00Z",
        maxChars: 8000,
        signal: controller.signal,
      })
    ).resolves.toEqual(body);
    await expect(client.sessionAnswers("session-1")).resolves.toEqual(body);

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/agents/session-1/answers?since=2026-10-24T00%3A00%3A00Z&max_chars=8000",
      "/api/v1/agents/session-1/answers",
    ]);
    const signal = requests[0]?.init.signal;
    expect(signal?.aborted).toBe(false);
    controller.abort();
    expect(signal?.aborted).toBe(true);
    expect(requests[1]?.init.signal?.aborted).toBe(false);
  });

  test("maps project document and reference routes to authenticated API requests", async () => {
    const { fetchImpl, requests } = fakeFetch(
      Array.from({ length: 14 }, () => jsonResponse({ ok: true }))
    );
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await client.listProjectArtifacts("CORE", true);
    await client.projectArtifact("CORE", { name: "runbook.md", content: "# Runbook", actor });
    await client.getProjectArtifact("CORE", "runbook-md");
    await client.getArtifactAsks("artifact-1");
    await client.artifactAsk("artifact-1", {
      question: "The runbook is ready for readers. How should we publish it?",
      actor,
    });
    await client.getArtifactComments("artifact-1");
    await client.artifactComment("artifact-1", { body: "Looks good.", actor });
    await client.artifactSuggest("artifact-1", {
      anchor: { artifact: "artifact-1", quote: "draft" },
      replace_with: "final",
      actor,
    });
    await client.getArtifactEvents("artifact-1");
    await client.getIssueReferences("CORE-1");
    await client.getArtifactReferences("artifact-1");
    await client.getReferences({ to: "dispatch://CORE-1/ask/ask-1" });
    await client.getReferences({
      from: "dispatch://CORE/artifact/runbook-md",
      kind: ["mentions", "attached_to"],
      since: 918,
    });
    await client.getReferences({ to: "dispatch://CORE-1", kind: [] });

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/projects/CORE/artifacts?unlinked=true",
      "/api/v1/projects/CORE/artifacts",
      "/api/v1/projects/CORE/artifacts/runbook-md",
      "/api/v1/artifacts/artifact-1/asks",
      "/api/v1/artifacts/artifact-1/asks",
      "/api/v1/artifacts/artifact-1/comments",
      "/api/v1/artifacts/artifact-1/comments",
      "/api/v1/artifacts/artifact-1/comments",
      "/api/v1/artifacts/artifact-1/events?after=0&limit=200",
      "/api/v1/issues/CORE-1/references?versions=latest",
      "/api/v1/artifacts/artifact-1/references",
      "/api/v1/references?to=dispatch%3A%2F%2FCORE-1%2Fask%2Fask-1",
      "/api/v1/references?from=dispatch%3A%2F%2FCORE%2Fartifact%2Frunbook-md&kind=mentions%2Cattached_to&since=918",
      "/api/v1/references?to=dispatch%3A%2F%2FCORE-1",
    ]);
    expect(requestBody(requests[1] as RecordedRequest)).toEqual({
      name: "runbook.md",
      content: "# Runbook",
      actor,
    });
    expect(requestBody(requests[7] as RecordedRequest)).toEqual({
      anchor: { artifact: "artifact-1", quote: "draft" },
      suggestion: { replace_with: "final" },
      actor,
    });
  });
  test("maps dispatch operations to authenticated JSON and multipart API requests", async () => {
    const { fetchImpl, requests } = fakeFetch([
      ...Array.from({ length: 15 }, () => jsonResponse({ ok: true })),
      jsonResponse({ last_seq: 4 }),
      jsonResponse([]),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test/",
      bearerCredential("secret"),
      fetchImpl
    );

    await client.issue({ project: "DSP", title: "Issue", spec: "# Spec", actor });
    await client.getIssue("DSP-1");
    await client.getIssueEvents("DSP-1", 4, 10);
    await client.ask("DSP-1", { question: "Choose", actor });
    await client.comment("DSP-1", { body: "Review", actor });
    await client.suggest("DSP-1", {
      body: "Use this",
      anchor: { artifact: "artifact-1", quote: "before" },
      replace_with: "after",
      actor,
    });
    await client.message("DSP-1", { body: "Update", actor });
    await client.artifact("DSP-1", {
      name: "spec.md",
      file: new Blob(["# Spec"], { type: "text/markdown" }),
      summary: "Initial spec",
      actor,
    });
    await client.getArtifact("artifact-1");
    await client.docRead("artifact-1");
    await client.docRead("artifact-1", 2);
    await client.docEdit("artifact-1", { ops: [], actor });
    await client.nameArtifactVersion("artifact-1", { summary: "Named", actor });
    await client.getAsk("ask-1");
    await client.getComments("DSP-1");
    await client.read("DSP-1");

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/issues",
      "/api/v1/issues/DSP-1",
      "/api/v1/issues/DSP-1/events?after=4&limit=10",
      "/api/v1/issues/DSP-1/asks",
      "/api/v1/issues/DSP-1/comments",
      "/api/v1/issues/DSP-1/comments",
      "/api/v1/issues/DSP-1/messages",
      "/api/v1/issues/DSP-1/artifacts",
      "/api/v1/artifacts/artifact-1",
      "/api/v1/artifacts/artifact-1/text",
      "/api/v1/artifacts/artifact-1/versions/2",
      "/api/v1/artifacts/artifact-1/edits",
      "/api/v1/artifacts/artifact-1/versions",
      "/api/v1/asks/ask-1",
      "/api/v1/issues/DSP-1/comments",
      "/api/v1/issues/DSP-1",
      "/api/v1/issues/DSP-1/events?after=0&limit=10",
    ]);
    expect(requests.map((request) => request.init.method)).toEqual([
      "POST",
      "GET",
      "GET",
      "POST",
      "POST",
      "POST",
      "POST",
      "POST",
      "GET",
      "GET",
      "GET",
      "POST",
      "POST",
      "GET",
      "GET",
      "GET",
      "GET",
    ]);

    for (const request of requests) {
      const headers = request.init.headers as Record<string, string>;
      expect(headers.Authorization).toBe("Bearer secret");
      expect(headers.Accept).toBe("application/json");
      // Every write carries a key of its own; no read carries one.
      if (request.init.method === "GET") expect(headers["Idempotency-Key"]).toBeUndefined();
      else expect(headers["Idempotency-Key"]).toBeString();
    }
    const writeKeys = requests
      .filter((request) => request.init.method !== "GET")
      .map((request) => (request.init.headers as Record<string, string>)["Idempotency-Key"]);
    expect(new Set(writeKeys).size).toBe(writeKeys.length);
    expect(requestBody(requests[0] as RecordedRequest)).toEqual({
      project: "DSP",
      title: "Issue",
      spec: "# Spec",
      actor,
    });
    expect(requestBody(requests[3] as RecordedRequest)).toEqual({ question: "Choose", actor });
    expect(requestBody(requests[4] as RecordedRequest)).toEqual({ body: "Review", actor });
    expect(requestBody(requests[5] as RecordedRequest)).toEqual({
      body: "Use this",
      anchor: { artifact: "artifact-1", quote: "before" },
      suggestion: { replace_with: "after" },
      actor,
    });
    expect(requestBody(requests[6] as RecordedRequest)).toEqual({ body: "Update", actor });
    expect(requestBody(requests[11] as RecordedRequest)).toEqual({ ops: [], actor });
    expect(requestBody(requests[12] as RecordedRequest)).toEqual({ summary: "Named", actor });

    // The form is sent encoded, once, so a retry sends the bytes and boundary its first attempt did.
    const upload = requests[7] as RecordedRequest;
    const artifactBody = await new Response(upload.init.body, {
      headers: {
        "Content-Type": (upload.init.headers as Record<string, string>)["Content-Type"] as string,
      },
    }).formData();
    expect(artifactBody.get("name")).toBe("spec.md");
    expect(artifactBody.get("primary")).toBeNull();
    expect(artifactBody.get("summary")).toBe("Initial spec");
    expect(artifactBody.get("actor")).toBe(JSON.stringify(actor));
    expect((artifactBody.get("file") as Blob).type).toBe("text/markdown");
  });

  test("maps a structured conflict response into DispatchServiceError candidates", async () => {
    const candidates = [{ from: 12, to: 18, context: "duplicated text" }];
    const { fetchImpl } = fakeFetch([
      jsonResponse({ error: "quote is ambiguous", code: "TARGET_AMBIGUOUS", candidates }, 409),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(
      client.docEdit("artifact-1", { ops: [{ op: "delete", find: "same" }], actor })
    ).rejects.toEqual(
      expect.objectContaining({
        name: "DispatchServiceError",
        code: "TARGET_AMBIGUOUS",
        status: 409,
        message: "quote is ambiguous",
        candidates,
      })
    );
  });

  test("preserves a document precondition conflict's current tokens", async () => {
    const current = {
      document: "sha256:current-document",
      blocks: [{ id: "block-1", token: "sha256:current-block" }],
    };
    const mismatches = [
      {
        scope: "block",
        block_id: "block-1",
        expected: "sha256:stale-block",
        current: "sha256:current-block",
      },
    ];
    const { fetchImpl } = fakeFetch([
      jsonResponse(
        {
          error: 'document edit precondition failed: block "block-1" changed',
          code: "PRECONDITION_FAILED",
          current,
          mismatches,
        },
        409
      ),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(
      client.docEdit("artifact-1", {
        ops: [{ op: "delete", find: "same" }],
        precondition: { blocks: [{ id: "block-1", token: "sha256:stale-block" }] },
        actor,
      })
    ).rejects.toEqual(
      expect.objectContaining({
        name: "DispatchServiceError",
        code: "PRECONDITION_FAILED",
        status: 409,
        current,
        mismatches,
      })
    );
  });

  // Dispatch writes every answer as JSON, so a non-2xx body that is not its `{code, error}` came
  // from something in front of it: a gateway's HTML page, an empty body, JSON of another shape. The
  // agent reads only the error's message, so it names the request and the status, quotes the body
  // only as a one-line plain-text excerpt (scripts, styles and tags dropped), and says whether a
  // retry can help: on a read, a gateway's 5xx, 408 or 429 can clear, while any other status
  // answers the same request the same way. The bearer travels in a header, so the URL never
  // carries it. The error says it is a gateway's answer, so a caller with advice of its own can
  // use `answer` without the client's.
  test("names the request, status and a plain-text excerpt, advising a read's retry only for a status that can clear", async () => {
    const nginx =
      "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n<center><h1>502 Bad Gateway</h1>" +
      "</center>\r\n<hr><center>nginx/1.25.3</center>\r\n</body>\r\n</html>\r\n";
    const cloudflare =
      "<html><head><style>body{margin:0}</style><script>var x=1;</script></head><body>" +
      "<h1>Error 522</h1><p>Connection timed out</p></body></html>";
    const { fetchImpl } = fakeFetch([
      new Response(nginx, {
        status: 502,
        statusText: "Bad Gateway",
        headers: { "Content-Type": "text/html" },
      }),
      new Response(cloudflare, { status: 522, headers: { "Content-Type": "text/html" } }),
      new Response(null, { status: 503, statusText: "Service Unavailable" }),
      new Response("x".repeat(500), { status: 429, headers: { "Content-Type": "text/plain" } }),
      new Response(JSON.stringify({ message: "upstream timed out" }), {
        status: 504,
        statusText: "Gateway Timeout",
        headers: { "Content-Type": "application/json" },
      }),
      new Response("<html><body><h1>404 Not Found</h1></body></html>", {
        status: 404,
        statusText: "Not Found",
        headers: { "Content-Type": "text/html" },
      }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );
    const refusal = () =>
      client.getIssueEvents("DSP-1", 4, 10).then(
        () => {
          throw new Error("expected a refusal");
        },
        (error: Error) => error
      );
    const request =
      "GET http://dispatch.test/api/v1/issues/DSP-1/events?after=4&limit=10 answered ";
    const gateway = ", which looks like a proxy or gateway page rather than Dispatch's own answer";
    const maySucceed = `${gateway}, so a retry may succeed.`;
    const sameAgain =
      `${gateway}, so a retry gets the same answer until the Dispatch URL, or whatever answers ` +
      "in its place, is fixed.";

    const badGateway = await refusal();
    const answer =
      `${request}502 Bad Gateway with a body that is not Dispatch's error JSON ` +
      `("502 Bad Gateway 502 Bad Gateway nginx/1.25.3")${gateway}`;
    expect(badGateway).toBeInstanceOf(DispatchGatewayError);
    expect(badGateway).toEqual(
      expect.objectContaining({
        name: "DispatchServiceError",
        code: "HTTP_502",
        status: 502,
        answer,
        transient: true,
        message: `${answer}, so a retry may succeed.`,
      })
    );
    expect((await refusal()).message).toBe(
      `${request}522 with a body that is not Dispatch's error JSON ("Error 522 Connection timed out")${maySucceed}`
    );
    expect((await refusal()).message).toBe(
      `${request}503 Service Unavailable with an empty body${maySucceed}`
    );
    expect((await refusal()).message).toBe(
      `${request}429 with a body that is not Dispatch's error JSON ("${"x".repeat(120)}…")${maySucceed}`
    );
    expect((await refusal()).message).toBe(
      `${request}504 Gateway Timeout with a body that is not Dispatch's error JSON ` +
        `(${JSON.stringify('{"message":"upstream timed out"}')})${maySucceed}`
    );
    const notFound = await refusal();
    expect(notFound.message).toBe(
      `${request}404 Not Found with a body that is not Dispatch's error JSON ("404 Not Found")${sameAgain}`
    );
    expect(notFound).toEqual(expect.objectContaining({ code: "HTTP_404", transient: false }));
  });

  // F5 (the review of 81d860a8f7): a read behind a redirect is told where the redirect pointed,
  // by origin alone, which is what tells a Dispatch URL to correct. The redirect is not followed,
  // and the Location's user info, path and query are never printed.
  test("a read a redirect answers names the origin the redirect points at, and does not follow it", async () => {
    const { fetchImpl, requests } = fakeFetch([
      new Response("<html><body>Moved</body></html>", {
        status: 302,
        statusText: "Found",
        headers: {
          "Content-Type": "text/html",
          Location: "https://user:secret@elsewhere.test:8443/login?token=abc",
        },
      }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    const error = await client.getIssueEvents("DSP-1", 4, 10).then(
      () => {
        throw new Error("expected a refusal");
      },
      (caught: Error) => caught
    );

    expect(requests).toHaveLength(1);
    expect(requests[0]?.init.redirect).toBe("manual");
    expect(error).toBeInstanceOf(DispatchGatewayError);
    expect(error.message).toStartWith(
      "GET http://dispatch.test/api/v1/issues/DSP-1/events?after=4&limit=10 answered 302 Found " +
        "(a redirect to https://elsewhere.test:8443, which this client does not follow) with a body " +
        `that is not Dispatch's error JSON ("Moved")`
    );
    expect(error.message).not.toContain("user:");
    expect(error.message).not.toContain("token=abc");
    expect(error.message).not.toContain("/login");
  });

  // Red team 7 and 4 (the review of 4dad905e68): a read's 2xx that is not JSON is a
  // DispatchGatewayError, with its status, advice that a retry may succeed, and `transient` saying
  // so too: a portal or a proxy in the way, which a later request can get past, and which never
  // reached Dispatch. A JSON answer is read whatever the case of its type, and a `+json` type is
  // JSON; a bodiless 204 is no answer to refuse.
  test("a read's 2xx that is not JSON is a gateway error a retry may clear; any JSON type and a 204 are read", async () => {
    const { fetchImpl } = fakeFetch([
      new Response("<html><title>Sign in to the network</title></html>", {
        status: 200,
        headers: { "Content-Type": "text/html" },
      }),
      new Response(JSON.stringify([{ id: 5 }]), {
        status: 200,
        headers: { "Content-Type": "Application/JSON; charset=utf-8" },
      }),
      new Response(JSON.stringify([{ id: 6 }]), {
        status: 200,
        headers: { "Content-Type": "application/vnd.dispatch+json" },
      }),
      new Response(null, { status: 204 }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    const error = await client.getIssueEvents("DSP-1", 4, 10).then(
      () => {
        throw new Error("expected a refusal");
      },
      (caught: unknown) => caught
    );
    expect(error).toBeInstanceOf(DispatchGatewayError);
    const gateway = error as DispatchGatewayError;
    expect(gateway.status).toBe(200);
    expect(gateway.advice).toBe("a retry may succeed");
    expect(gateway.transient).toBe(true);
    expect(gateway.mayHaveReachedDispatch).toBe(false);
    expect(gateway.message).toContain(
      'answered 200 with a body that is not JSON ("Sign in to the network")'
    );

    const read = async (): Promise<unknown> => await client.getIssueEvents("DSP-1", 4, 10);
    expect(await read()).toEqual([{ id: 5 }]);
    expect(await read()).toEqual([{ id: 6 }]);
    expect(await read()).toBe("");
  });

  // A gateway's 5xx can come back after it forwarded a write and Dispatch applied it, so advising
  // a retry there can post a message twice: the write is told to check what took effect. A 408 or
  // 429 is the gateway's own timeout or rate limit, sent before it forwards the request, so the
  // write never reached Dispatch and a retry may succeed. Any other status still gets the same
  // advice as a read, since the request never got past the gateway either. Each status is one
  // write's last attempt here; how the attempts before it are made is dispatch-keyed-writes.test.ts.
  test("tells a write a gateway answered whether it may have reached Dispatch", async () => {
    const page = (status: number, statusText: string) =>
      new Response(`<html><body><h1>${status} ${statusText}</h1></body></html>`, {
        status,
        statusText,
        headers: { "Content-Type": "text/html" },
      });
    const { fetchImpl } = fakeFetch([
      page(502, "Bad Gateway"),
      page(408, "Request Timeout"),
      page(429, "Too Many Requests"),
      page(404, "Not Found"),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { writeRetry: { ...DISPATCH_WRITE_RETRY, pausesMs: [], withinMs: 0 } }
    );
    const refusal = () =>
      client.message("DSP-1", { body: "Shipped.", actor }).then(
        () => {
          throw new Error("expected a refusal");
        },
        (error: Error) => error
      );
    const request = "POST http://dispatch.test/api/v1/issues/DSP-1/messages answered ";
    const gateway = ", which looks like a proxy or gateway page rather than Dispatch's own answer";
    const page404 = `${request}404 Not Found with a body that is not Dispatch's error JSON ("404 Not Found")`;

    // A 502 can come after Dispatch committed the write, so with no ask left the write ends
    // unsettled (the transition table, write-outcome.ts), the gateway's page as its cause.
    const badGateway = await refusal();
    expect(badGateway).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(badGateway.message).toBe(
      `${request}502 Bad Gateway with a body that is not Dispatch's error JSON ("502 Bad Gateway")` +
        `${gateway} (its only attempt), and an attempt of the write may have reached Dispatch, so ` +
        "it may or may not have taken effect. Dispatch finishes an attempt it received whatever this " +
        "command does, so read the write back before you send it again: a new command sends it under a new key."
    );
    expect((badGateway as DispatchWriteUnsettledError).last).toEqual(
      expect.objectContaining({ mayHaveReachedDispatch: true })
    );
    for (const [status, statusText] of [
      [408, "Request Timeout"],
      [429, "Too Many Requests"],
    ] as const) {
      const refused = await refusal();
      expect(refused.message).toBe(
        `${request}${status} ${statusText} with a body that is not Dispatch's error JSON ` +
          `("${status} ${statusText}")${gateway}, so the write did not reach Dispatch, and a ` +
          "retry may succeed."
      );
      expect(refused).toEqual(
        expect.objectContaining({ transient: true, mayHaveReachedDispatch: false })
      );
    }
    expect((await refusal()).message).toBe(
      `${page404}${gateway}, so a retry gets the same answer until the Dispatch URL, or whatever ` +
        "answers in its place, is fixed."
    );
  });

  // The excerpt and the reason phrase are text a gateway chose, and a misconfigured one can echo
  // the request back, headers included. Two kinds of credential are redacted before either is
  // quoted: the client's own bearer wherever it appears (trimmed, as fetch sends it), and the
  // value after `Authorization:` or `Bearer`. The reason phrase is cut to one line like the body.
  test("redacts the bearer and an Authorization or Bearer value from the excerpt and the reason phrase", async () => {
    const token = fixtureSecret("dsp");
    const echo =
      `<html><body><h1>401 Unauthorized</h1><pre>token=${token}\r\n` +
      "Authorization: Basic dXNlcjpwYXNz\r\nX-Forwarded: Bearer other.jwt-value</pre></body></html>";
    const { fetchImpl } = fakeFetch([
      new Response(echo, {
        status: 401,
        statusText: "Unauthorized",
        headers: { "Content-Type": "text/html" },
      }),
      new Response(JSON.stringify({ headers: { authorization: `Bearer ${token}` } }), {
        status: 502,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(null, { status: 502, statusText: `Denied for ${token} ${"x".repeat(200)}` }),
      new Response(`<p>token=${token}</p>`, { status: 401, statusText: "Unauthorized" }),
    ]);
    const client = new DispatchClient("http://dispatch.test", bearerCredential(token), fetchImpl);
    const refusal = (from: DispatchClient) =>
      from.getIssue("DSP-1").then(
        () => "",
        (error: Error) => error.message
      );
    const request = "GET http://dispatch.test/api/v1/issues/DSP-1 answered ";
    const gateway = ", which looks like a proxy or gateway page rather than Dispatch's own answer";

    const unauthorized = await refusal(client);
    expect(unauthorized).toBe(
      `${request}401 Unauthorized with a body that is not Dispatch's error JSON ("401 Unauthorized ` +
        'token=[redacted] Authorization: Basic [redacted] X-Forwarded: Bearer [redacted]")' +
        `${gateway}, so a retry gets the same answer until the Dispatch URL, or whatever answers ` +
        "in its place, is fixed."
    );
    const echoedJson = await refusal(client);
    expect(echoedJson).toContain(
      JSON.stringify('{"headers":{"authorization":"Bearer [redacted]"}}')
    );
    const reasonPhrase = await refusal(client);
    expect(reasonPhrase).toBe(
      `${request}502 Denied for [redacted] ${"x".repeat(98)}… with an empty body${gateway}, so a ` +
        "retry may succeed."
    );
    const untrimmed = new DispatchClient(
      "http://dispatch.test",
      bearerCredential(` ${token}\n`),
      fetchImpl
    );
    const echoedTrimmed = await refusal(untrimmed);
    expect(echoedTrimmed).toContain('("token=[redacted]")');
    for (const message of [unauthorized, echoedJson, reasonPhrase, echoedTrimmed]) {
      expect(message).not.toContain(token);
      expect(message).not.toContain("dXNlcjpwYXNz");
      expect(message).not.toContain("other.jwt-value");
    }
  });

  // M4 (the review of 99b1e438db): a DPoP token after `Authorization: DPoP` or `DPoP` is redacted
  // as a bearer's is, and so is the token the request was sent with when the credential names
  // another by the time the answer comes (a renewal between the two): the excerpt redacts every
  // token this client presented, not only the one `secret()` names now.
  test("redacts a DPoP value and the token a request was sent with, after the credential renewed", async () => {
    const sent = fixtureSecret("sent");
    const renewed = fixtureSecret("renewed");
    let presentations = 0;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        presentations += 1;
        return { Authorization: `DPoP ${sent}`, DPoP: "proof" };
      },
      // The renewal landed while the request was out: the credential names the next token.
      secret: () => (presentations === 0 ? sent : renewed),
    };
    const echo =
      `<html><body><h1>502 Bad Gateway</h1><pre>Authorization: DPoP ${sent}\r\n` +
      `X-Echo: DPoP ${sent.slice(2)}\r\nX-Raw: ${sent}</pre></body></html>`;
    const { fetchImpl } = fakeFetch([
      new Response(echo, {
        status: 502,
        statusText: "Bad Gateway",
        headers: { "Content-Type": "text/html" },
      }),
    ]);
    const client = new DispatchClient("http://dispatch.test", credential, fetchImpl, {
      retryNoRoute: false,
    });

    const message = await client.getIssue("DSP-1").then(
      () => "",
      (error: Error) => error.message
    );

    expect(message).toContain(
      '("502 Bad Gateway Authorization: DPoP [redacted] X-Echo: DPoP [redacted] X-Raw: [redacted]")'
    );
    expect(message).not.toContain(sent.slice(-12));
  });

  // A Dispatch URL may carry a user and a password: the config accepts such a URL, and the
  // requests go to it as configured. Every message that names the request or the Dispatch names
  // no part of the URL, only its fixed phrase, since the URL parser can split such a URL otherwise
  // than its writer meant: a gateway's answer, a refused listing, a write whose answer was lost, a
  // review route the build lacks, and a write whose attempt heard nothing.
  test("names a Dispatch URL with user info by its fixed phrase in every refusal that names it", async () => {
    const user = "user-7f2a";
    const secret = "pw-4c7e19";
    const configured = `https://${user}:${secret}@host.example`;
    const { fetchImpl, requests } = fakeFetch([
      new Response("<html><body><h1>502 Bad Gateway</h1></body></html>", {
        status: 502,
        statusText: "Bad Gateway",
        headers: { "Content-Type": "text/html" },
      }),
      jsonResponse([]),
      jsonResponse({ issues: [], total: 0, limit: 2, offset: 0 }),
      jsonResponse({ code: "KEY_COMMITTED", error: "the write was made" }, 409),
      jsonResponse(
        { code: "NOT_FOUND", error: "no route for POST /api/v1/artifacts/a-1/versions/2/reviews" },
        404
      ),
    ]);
    const client = new DispatchClient(configured, bearerCredential("bearer-1"), fetchImpl, {
      writeRetry: { ...DISPATCH_WRITE_RETRY, pausesMs: [], withinMs: 0 },
      retryNoRoute: false,
    });
    // An attempt that hears nothing until the client cuts it off for silence.
    const silent = (async (_input: RequestInfo | URL, init?: RequestInit) => {
      const { promise, reject } = Promise.withResolvers<Response>();
      init?.signal?.addEventListener("abort", () => reject(init.signal?.reason), { once: true });
      return promise;
    }) as typeof fetch;
    const unanswered = new DispatchClient(configured, bearerCredential("bearer-1"), silent, {
      writeRetry: {
        ...DISPATCH_WRITE_RETRY,
        pausesMs: [],
        withinMs: 0,
        inFlightWithinMs: 0,
        silenceMs: 20,
      },
    });
    const refusal = (call: Promise<unknown>) =>
      call.then(
        () => {
          throw new Error("expected a refusal");
        },
        (error: Error) => error.message
      );

    const messages = [
      await refusal(client.getIssue("DSP-1")),
      await refusal(client.listIssuePage({}, { limit: 2, offset: 0 })),
      await refusal(client.listAllIssues({})),
      await refusal(client.message("DSP-1", { body: "Shipped.", actor })),
      await refusal(client.reviewArtifact("a-1", 2, { state: "approved" })),
      await refusal(unanswered.message("DSP-1", { body: "Shipped.", actor })),
    ];

    expect(requests.map((request) => request.url)).toEqual([
      `${configured}/api/v1/issues/DSP-1`,
      `${configured}/api/v1/issues?limit=2&offset=0`,
      `${configured}/api/v1/issues`,
      `${configured}/api/v1/issues/DSP-1/messages`,
      `${configured}/api/v1/artifacts/a-1/versions/2/reviews`,
    ]);
    const shown = `the configured Dispatch URL ${URL_NOT_PRINTED}`;
    expect(messages[0]).toStartWith(`GET ${shown} answered 502 Bad Gateway with a body`);
    expect(messages[1]).toStartWith(`GET ${shown} asked for a page`);
    expect(messages[2]).toBe(
      `GET ${shown} asked for the unpaged listing and got a page, so ` +
        "this Dispatch pages a request that carries no limit and no offset."
    );
    expect(messages[3]).toStartWith(`POST ${shown} was made, but its answer was lost`);
    expect(messages[4]).toStartWith(
      `Dispatch at ${shown} answered no route for ` +
        "POST /api/v1/artifacts/a-1/versions/2/reviews each time the review was sent"
    );
    expect(messages[4]).toContain("GET /healthz on that server names the commit");
    expect(messages[5]).toBe(
      `POST ${shown} made no progress for 0.02 s, so it ` +
        "was cut off (its only attempt), and an attempt of the write may have reached Dispatch, " +
        "so it may or may not have taken effect. Dispatch finishes an attempt it received " +
        "whatever this command does, so read the write back before you send it again: a new " +
        "command sends it under a new key."
    );
    for (const message of messages) {
      expect(message).not.toContain(user);
      expect(message).not.toContain(secret);
    }
  });

  // Whatever answers at the Dispatch URL (a repository's own `.envoy/envoy.json` can name it)
  // controls the body, and the excerpt is built synchronously on the host's event loop, before the
  // 120-character cut. Each pass over the body must stay linear and the scan bounded, or a page of
  // unclosed tags freezes the session: a quadratic pass takes seconds on tens of kilobytes. The
  // row's own 5 s limit is that bound, and nothing here reads a clock. It sits between the row and
  // the defect it catches, so an idle machine catches it as well as a busy one: measured in a
  // 2-CPU pod, dropping tags one at a time over the whole body costs this row 33.0 to 36.1 s with
  // no load at all and 153 to 225 s under 16 busy loops (8 x nproc), where the row itself costs
  // 0.03 s idle and 0.60 s at most under those loops. The limit is a bound on work, not a hang
  // guard, and it stays on the row rather than taking the suite's 30 s default, which the
  // defect's idle cost would pass.
  test("builds the excerpt of a megabyte of unclosed tags in bounded time", async () => {
    const megabyte = 1 << 20;
    // The last three hold the client's bearer, "secret", everywhere, or almost everywhere.
    for (const unit of [
      "<",
      "<script",
      "<script x",
      "<a <b",
      "</script>",
      "<script></script ",
      "secret",
      "<script>secret",
      "secre",
    ]) {
      const hostile = unit.repeat(Math.ceil(megabyte / unit.length));
      const { fetchImpl } = fakeFetch([
        new Response(hostile, { status: 502, headers: { "Content-Type": "text/html" } }),
      ]);
      const client = new DispatchClient(
        "http://dispatch.test",
        bearerCredential("secret"),
        fetchImpl
      );
      const message = await client.getIssue("DSP-1").then(
        () => "",
        (error: Error) => error.message
      );

      expect(message).toStartWith(
        "GET http://dispatch.test/api/v1/issues/DSP-1 answered 502 with "
      );
    }
  }, 5_000);

  // The excerpt reads at most the first 64 KiB of the body, so what it costs has a bound however
  // large the body is. Redacting the bearer shortens the text, so it runs on that slice: redacting
  // the whole body first pulls text from past the limit into the excerpt, and a slice cut through
  // a bearer would leave its first characters to be pulled in the same way. A bearer that starts
  // inside the limit and runs past it is redacted whole.
  test("scans at most the first 64 KiB of a body, however often it holds the bearer", async () => {
    const limit = 64 * 1024;
    const bearer = `dsp_${"Zq9x".repeat(10)}`;
    const styled = `<style>${bearer.repeat(Math.floor((limit - 80) / bearer.length))}</style>`;
    const shortOfLimit = limit - styled.length;
    // Visible words end at the limit, and a bearer starts one character past it.
    const pastLimit = `${styled}${"p".repeat(shortOfLimit)} ${bearer} after the limit`;
    // A bearer starts five characters short of the limit and ends past it.
    const acrossLimit = `${styled}${"q".repeat(shortOfLimit - 5)}${bearer} after the limit`;
    const { fetchImpl } = fakeFetch(
      [pastLimit, acrossLimit].map(
        (body) => new Response(body, { status: 502, headers: { "Content-Type": "text/html" } })
      )
    );
    const client = new DispatchClient("http://dispatch.test", bearerCredential(bearer), fetchImpl);
    const refusal = () =>
      client.getIssue("DSP-1").then(
        () => "",
        (error: Error) => error.message
      );

    const past = await refusal();
    expect(past).toContain(`("${"p".repeat(shortOfLimit)}")`);
    const across = await refusal();
    expect(across).toContain(`("${"q".repeat(shortOfLimit - 5)}[redacted]")`);
    for (const message of [past, across]) {
      expect(message).not.toContain("after the limit");
      expect(message).not.toContain(bearer.slice(0, 8));
    }
  });

  // A gateway can echo the header cut short, split by markup, or twice with the copies
  // overlapping (a bearer that ends in its own first characters), and an exact match finds none
  // of those. Every run of 8 or more of the bearer's characters within what the excerpt reads is
  // redacted, one that starts inside the scan limit and runs past it included, so no piece of the
  // bearer that long reaches the message. Each body holds such a piece, so the search can see one.
  // Dispatch's tokens are `dsp_` and 43 base64url characters; this one ends in its first two.
  const pieceBearer = "dsp_Q7vK2mXr9LtW4nBz8YcH1jPe5sAf3gUo6iNq0wEbTds";
  /** Every run of 8 of the bearer's characters `text` holds; any longer piece holds one. */
  const bearerPieces = (text: string) =>
    Array.from({ length: pieceBearer.length - 7 }, (_, at) => pieceBearer.slice(at, at + 8)).filter(
      (piece) => text.includes(piece)
    );
  test.each([
    [
      "cut short by its last character",
      `<p>echo=${pieceBearer.slice(0, -1)} end</p>`,
      "echo=[redacted] end",
    ],
    [
      "cut short by its last 10 characters",
      `<p>echo=${pieceBearer.slice(0, -10)} end</p>`,
      "echo=[redacted] end",
    ],
    [
      "followed by a copy that starts inside its last characters",
      `<p>echo=${pieceBearer}${pieceBearer.slice(2)} end</p>`,
      "echo=[redacted] end",
    ],
    [
      "split by a tag",
      `<p>echo=${pieceBearer.slice(0, 20)}<wbr>${pieceBearer.slice(20)} end</p>`,
      "echo=[redacted] [redacted] end",
    ],
    [
      // A 64 KiB scan limit; the copy starts 20 characters inside it and ends 17 past it.
      "cut short, across the scan limit",
      `<style>${"s".repeat(64 * 1024 - 75)}</style>${"q".repeat(40)}${pieceBearer.slice(0, -10)} after the limit`,
      `${"q".repeat(40)}[redacted]`,
    ],
  ] as const)("redacts a piece of the bearer %s", async (_copy, body, quoted) => {
    const { fetchImpl } = fakeFetch([
      new Response(body, { status: 502, headers: { "Content-Type": "text/html" } }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential(pieceBearer),
      fetchImpl
    );

    const message = await client.getIssue("DSP-1").then(
      () => "",
      (error: Error) => error.message
    );

    expect(bearerPieces(body)).not.toEqual([]);
    expect(bearerPieces(message)).toEqual([]);
    expect(message).toContain(`(${JSON.stringify(quoted)})`);
    expect(message).not.toContain("after the limit");
  });

  // Nothing checks a bearer's length or its characters on load, and the excerpt is built on the
  // host's event loop. A bearer of one character repeated is thousands of pieces that are all the
  // same, and searching the 64 KiB slice once for each of them marks every position again per
  // piece. Each distinct piece is searched once, and the row's own 2 s limit is what says so
  // without reading a clock. It sits between the row and the defect it catches, so an idle machine
  // catches it as well as a busy one: measured in a 2-CPU pod, a search per piece costs this row
  // 8.82 s with no load at all and 75 to 151 s under 16 busy loops (8 x nproc), where the row
  // itself costs 0.01 s idle and 0.19 s at most under those loops. The limit is a bound on work,
  // not a hang guard, and it stays on the row rather than taking the suite's 30 s default, which
  // the defect's idle cost would pass.
  test("redacts a bearer of one character repeated from a 64 KiB body in bounded time", async () => {
    const { fetchImpl } = fakeFetch([
      new Response("x".repeat(64 * 1024), {
        status: 502,
        headers: { "Content-Type": "text/html" },
      }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("x".repeat(2048)),
      fetchImpl
    );

    const message = await client.getIssue("DSP-1").then(
      () => "",
      (error: Error) => error.message
    );

    expect(message).toContain('("[redacted]")');
    expect(message).not.toContain("xxxxxxxx");
  }, 2_000);

  // A body of nothing but markup is not empty, and an agent told it was would look for a gateway
  // that sent nothing. The answer gives the body's size in bytes and says none of it reads as
  // text, or none of what the excerpt reads when the body runs past that.
  test("says a body of markup alone has no readable text, with its size, rather than that it is empty", async () => {
    const { fetchImpl } = fakeFetch([
      new Response("<p>\u00a0</p>", { status: 502, headers: { "Content-Type": "text/html" } }),
      new Response("<script>".repeat(1 << 17), {
        status: 502,
        headers: { "Content-Type": "text/html" },
      }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );
    const refusal = () =>
      client.getIssue("DSP-1").then(
        () => "",
        (error: Error) => error.message
      );
    const request = "GET http://dispatch.test/api/v1/issues/DSP-1 answered 502 with ";
    const gateway =
      ", which looks like a proxy or gateway page rather than Dispatch's own answer, so a retry " +
      "may succeed.";

    expect(await refusal()).toBe(`${request}a body of 9 bytes and no readable text${gateway}`);
    expect(await refusal()).toBe(
      `${request}a body of 1,048,576 bytes and no readable text in its first 64 KiB${gateway}`
    );
  });

  // Dispatch's own refusal is JSON with a string `error`; `code` names the check when the server
  // sets one (`writeError`) and is `HTTP_<status>` otherwise. Both render as they always did: the
  // server's text, nothing about requests or retries.
  test("renders Dispatch's own JSON error as its text, with or without a code", async () => {
    const { fetchImpl } = fakeFetch([
      jsonResponse({ code: "ISSUE_CLOSED", error: "issue is closed" }, 409),
      jsonResponse({ error: "no route for GET /api/v1/issues/DSP-1" }, 404),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(client.getIssue("DSP-1")).rejects.toEqual(
      expect.objectContaining({ code: "ISSUE_CLOSED", status: 409, message: "issue is closed" })
    );
    await expect(client.getIssue("DSP-1")).rejects.toEqual(
      expect.objectContaining({
        code: "HTTP_404",
        status: 404,
        message: "no route for GET /api/v1/issues/DSP-1",
      })
    );
  });

  test("resolves an external issue reference once before native-key routes", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ key: "DSP-42" }),
      jsonResponse({ key: "DSP-42", last_seq: 3 }),
      jsonResponse([]),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await client.read("owner/repo#42");

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
      "/api/v1/issues/DSP-42",
      "/api/v1/issues/DSP-42/events?after=0&limit=10",
    ]);
  });

  // A full page the read goes past must end past its cursor, or the next page would repeat it; a
  // page that ends the read is taken as served, as the single-page read always took it.
  test("read refuses a full page that does not move past its cursor only when it must read on", async () => {
    const page = (from: number, to: number) =>
      jsonResponse(Array.from({ length: to - from + 1 }, (_, index) => ({ seq: from + index })));
    const stuck = fakeFetch([
      jsonResponse({ key: "DSP-1", last_seq: 450 }),
      page(1, 200),
      page(1, 200),
    ]);
    await expect(
      new DispatchClient("http://dispatch.test", bearerCredential("secret"), stuck.fetchImpl).read(
        "DSP-1",
        {
          events: "all",
        }
      )
    ).rejects.toThrow("GET /api/v1/issues/DSP-1/events?after=200 answered no event after seq 200");
    expect(stuck.requests.map((request) => new URL(request.url).search)).toEqual([
      "",
      "?after=0&limit=200",
      "?after=200&limit=200",
    ]);

    const last = fakeFetch([jsonResponse({ key: "DSP-1", last_seq: 50 }), page(1, 10)]);
    const read = await new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      last.fetchImpl
    ).read("DSP-1");
    expect(read.events.map((event) => event.seq)).toEqual(
      Array.from({ length: 10 }, (_, index) => index + 1)
    );
    expect(last.requests.map((request) => new URL(request.url).search)).toEqual([
      "",
      "?after=40&limit=10",
    ]);
  });
  test("maps comment reply-chain reads to the API", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({
        comment: {
          id: "comment-1",
          issue_key: "DSP-1",
          author: actor,
          body: "Root comment",
          anchor: null,
          reply_to: null,
          resolved: false,
          suggestion: null,
          created_at: "2026-09-09T00:00:00Z",
        },
        replies: [
          {
            id: "comment-2",
            issue_key: "DSP-1",
            author: actor,
            body: "Reply",
            anchor: null,
            reply_to: "comment-1",
            resolved: false,
            suggestion: null,
            created_at: "2026-09-09T00:01:00Z",
          },
        ],
      }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await client.getComment("comment-1");
    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual(["/api/v1/comments/comment-1"]);
  });

  test("maps a comment's and a message's blocks reads to the API", async () => {
    const blocks: ItemBlocks = {
      item: {
        kind: "comment",
        id: "comment-1",
        made_by: null,
        made_at: null,
        changed_by: null,
        changed_at: null,
        parent: null,
        parent_version: null,
      },
      revision: 1,
      indexed: true,
      blocks: [{ id: "b1", type: "paragraph", text: "Alpha.", from: 0, to: 6, embedding: "none" }],
    };
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse(blocks),
      jsonResponse({ ...blocks, item: { kind: "message", id: "message-1" } }),
      jsonResponse({ ...blocks, item: { kind: "message", id: "message-2" } }),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    expect(await client.commentBlocks("comment-1")).toEqual(blocks);
    await client.messageBlocks("message-1");
    await client.messageBlocks("message-2", "s1");
    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/comments/comment-1/blocks",
      "/api/v1/messages/message-1/blocks",
      "/api/v1/messages/message-2/blocks?session=s1",
    ]);
  });
});

test("maps an ask resolution to the authenticated API", async () => {
  const { fetchImpl, requests } = fakeFetch([
    jsonResponse({
      id: "ask-1",
      issue_key: "DSP-1",
      state: "resolved",
      resolution: {
        actor,
        at: "2026-09-10T00:00:00Z",
        kind: "retracted",
        reason: "A newer question supersedes this one.",
      },
    }),
  ]);
  const client = new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl);
  const resolveAsk = Reflect.get(client, "resolveAsk");

  expect(typeof resolveAsk).toBe("function");
  if (typeof resolveAsk !== "function") throw new Error("DispatchClient.resolveAsk is missing");
  await resolveAsk.call(client, "ask-1", {
    actor,
    kind: "retracted",
    reason: "A newer question supersedes this one.",
  });

  expect(requests.map((request) => [request.init.method, new URL(request.url).pathname])).toEqual([
    ["POST", "/api/v1/asks/ask-1/resolve"],
  ]);
  expect(requestBody(requests[0] as RecordedRequest)).toEqual({
    actor,
    kind: "retracted",
    reason: "A newer question supersedes this one.",
  });
});

test("createProject posts the key, name and actor and answers the created project", async () => {
  const project = {
    key: "CORE",
    name: "Core",
    open_asks: 0,
    default_assignee: "alice@example.com",
    created_at: "2026-10-11T00:00:00Z",
    gated_steps: [],
  };
  const { fetchImpl, requests } = fakeFetch([jsonResponse(project, 201)]);
  const client = new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl);

  expect(await client.createProject({ key: "CORE", name: "Core", actor })).toEqual(project);
  expect(requests.map((request) => [request.init.method, new URL(request.url).pathname])).toEqual([
    ["POST", "/api/v1/projects"],
  ]);
  expect(requestBody(requests[0] as RecordedRequest)).toEqual({ key: "CORE", name: "Core", actor });
});

test("createProject surfaces PROJECT_EXISTS as the refusal's code", async () => {
  const { fetchImpl } = fakeFetch([
    jsonResponse({ code: "PROJECT_EXISTS", error: "a project with this key already exists" }, 409),
  ]);
  const client = new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl);

  await expect(client.createProject({ key: "CORE", name: "Core", actor })).rejects.toMatchObject({
    code: "PROJECT_EXISTS",
    status: 409,
    fromDispatch: true,
  });
});

test("maps handoff writes and reads to the handoff routes, the body sent as its own text", async () => {
  const summary = {
    id: "0b7c2c5e-8f6d-4b8a-9a43-5d2e8c1f0a11",
    issue_key: "DSP-1",
    phase: "implement",
    version: 2,
    author: actor,
    created_at: "2026-10-26T00:00:00Z",
  };
  // Spacing and key order a re-serialization would lose.
  const body = '{ "schemaVersion": 1,\n  "b": 2, "a": 1 }\n';
  const { fetchImpl, requests } = fakeFetch([
    jsonResponse(summary, 201),
    jsonResponse([summary]),
    jsonResponse({ ...summary, body }),
    jsonResponse({ ...summary, body }),
  ]);
  const client = new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl);

  expect(await client.writeHandoff("DSP-1", "implement", { actor, body })).toEqual(summary);
  expect(await client.listHandoffs("DSP-1")).toEqual([summary]);
  expect((await client.latestHandoff("DSP-1", "implement")).body).toBe(body);
  expect((await client.handoff(summary.id)).body).toBe(body);
  expect(requests.map((request) => [request.init.method, new URL(request.url).pathname])).toEqual([
    ["POST", "/api/v1/issues/DSP-1/handoffs/implement"],
    ["GET", "/api/v1/issues/DSP-1/handoffs"],
    ["GET", "/api/v1/issues/DSP-1/handoffs/implement/latest"],
    ["GET", `/api/v1/handoffs/${summary.id}`],
  ]);
  expect(requestBody(requests[0] as RecordedRequest)).toEqual({ actor, body });
});

test("syncArchitectureSource refuses a repository with an empty owner or name without a request", async () => {
  const { fetchImpl, requests } = fakeFetch([]);
  const client = new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl);

  for (const repo of ["acme/", "/infra"]) {
    await expect(client.syncArchitectureSource("CORE", repo)).rejects.toThrow(
      `repository must be owner/name, got ${JSON.stringify(repo)}`
    );
  }
  expect(requests).toEqual([]);
});

// A server older than the list route answers it with no route at each of the four attempts; the
// client then reads the singular route that server does serve, so a rollout that mixes versions
// still reads the project's model.
describe("listArchitectureSources against a server older than the list route", () => {
  const noRoute = jsonResponse(
    {
      code: "NOT_FOUND",
      error: "no route for GET /api/v1/projects/CORE/architecture-sources",
      hint: "GET /api/v1 lists every route",
    },
    404
  );
  const source: ArchitectureSource = {
    project: "CORE",
    repo: "legion/arch",
    branch: "main",
    enabled: true,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-09-23T00:00:00Z",
    last_sync_at: null,
    last_commit: null,
    last_error: null,
    components: 2,
  };

  test.each([
    ["200 null as no source", jsonResponse(null), []],
    [
      "a pre-#1522 404 SOURCE_NOT_FOUND as no source",
      jsonResponse({ code: "SOURCE_NOT_FOUND", error: "no architecture source configured" }, 404),
      [],
    ],
    ["a row as that one source", jsonResponse(source), [source]],
  ])("reads the singular route's %s", async (_answer, singular, want) => {
    const { fetchImpl, requests } = fakeFetch([...noRoutes(noRoute), singular]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { sleep: noWait }
    );

    expect(await client.listArchitectureSources("CORE")).toEqual(want);
    expect(requests.map((request) => new URL(request.url).pathname)).toEqual([
      ...Array.from({ length: 4 }, () => "/api/v1/projects/CORE/architecture-sources"),
      "/api/v1/projects/CORE/architecture-source",
    ]);
  });

  test("throws any other answer to the singular read", async () => {
    const { fetchImpl } = fakeFetch([
      ...noRoutes(noRoute),
      jsonResponse({ code: "NOT_FOUND", error: "not found" }, 404),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { sleep: noWait }
    );

    await expect(client.listArchitectureSources("CORE")).rejects.toMatchObject({
      code: "NOT_FOUND",
      status: 404,
      message: "not found",
    });
  });

  test("throws a current server's 404 for a project it does not know without the singular read", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ code: "NOT_FOUND", error: "not found" }, 404),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(client.listArchitectureSources("CORE")).rejects.toMatchObject({
      code: "NOT_FOUND",
      status: 404,
      message: "not found",
    });
    expect(requests).toHaveLength(1);
  });
});

// A server older than the every-source sync route answers it with no route at each of the four
// attempts; the client then asks that server's singular sync, so a rollout that mixes versions
// still syncs the project's one source.
describe("syncArchitectureSources against a server older than the every-source sync route", () => {
  const noRoute = jsonResponse(
    {
      code: "NOT_FOUND",
      error: "no route for POST /api/v1/projects/CORE/architecture-sources/sync",
      hint: "GET /api/v1 lists every route",
    },
    404
  );
  const source: ArchitectureSource = {
    project: "CORE",
    repo: "legion/arch",
    branch: "main",
    enabled: true,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-09-23T00:00:00Z",
    last_sync_at: "2026-09-24T00:00:00Z",
    last_commit: "abc123",
    last_error: null,
    components: 2,
  };

  test("returns the singular sync's row as that one source", async () => {
    const { fetchImpl, requests } = fakeFetch([...noRoutes(noRoute), jsonResponse(source)]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { sleep: noWait }
    );

    expect(await client.syncArchitectureSources("CORE")).toEqual([source]);
    expect(
      requests.map((request) => `${request.init.method} ${new URL(request.url).pathname}`)
    ).toEqual([
      ...Array.from({ length: 4 }, () => "POST /api/v1/projects/CORE/architecture-sources/sync"),
      "POST /api/v1/projects/CORE/architecture-source/sync",
    ]);
  });

  test("throws the singular sync's 404 SOURCE_NOT_FOUND for a project with no source", async () => {
    const { fetchImpl } = fakeFetch([
      ...noRoutes(noRoute),
      jsonResponse({ code: "SOURCE_NOT_FOUND", error: "no architecture source configured" }, 404),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { sleep: noWait }
    );

    await expect(client.syncArchitectureSources("CORE")).rejects.toMatchObject({
      code: "SOURCE_NOT_FOUND",
      status: 404,
      message: "no architecture source configured",
    });
  });

  test("throws a current server's 404 for a project it does not know without the singular sync", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ code: "NOT_FOUND", error: "not found" }, 404),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(client.syncArchitectureSources("CORE")).rejects.toMatchObject({
      code: "NOT_FOUND",
      status: 404,
      message: "not found",
    });
    expect(requests).toHaveLength(1);
  });
});

// Dispatch answers 429 OVERLOADED or EDIT_QUEUE_FULL before a request runs, and 503 DATABASE_BUSY
// or REQUEST_TIMEOUT when a bound of its own ended a call before the database answered it or a read
// passed its deadline, each with Retry-After. A read waits that out (plus jitter) and asks again, at
// most twice and never past the client's deadline; a PUT or a DELETE waits out the two 429s alone.
// A write (POST, PATCH) is asked again by its own loop under its one key (`#write`, whose table,
// write-outcome.ts, reads a 429 as `try-later` and a 5xx as `server-error`), on its own pacing or
// the longer Retry-After.
describe("DispatchClient waits out Retry-After", () => {
  // What a Dispatch runs is remembered per process and origin, and these rows drive one origin as
  // Dispatches of their own.
  beforeEach(forgetKeyedLayerMemory);
  afterEach(forgetKeyedLayerMemory);

  // The client waits out `Retry-After`, and paces a write's asking, on the write retry's own clock
  // and timer, so here a wait moves `at` instead of sleeping. The schedule is the write's default
  // (1, 2, 4 and 8 s, none starting past 30 s), which a read's wait-out does not read.
  function fakeClock() {
    const state = { at: 0, waits: [] as number[] };
    const writeRetry: DispatchWriteRetry = {
      ...DISPATCH_WRITE_RETRY,
      now: () => state.at,
      pause: async (ms) => {
        state.waits.push(ms);
        state.at += ms;
      },
    };
    return { state, writeRetry };
  }

  function retryClient(
    fetchImpl: typeof fetch,
    writeRetry: DispatchWriteRetry,
    deadlineMs?: number
  ): DispatchClient {
    return new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl, {
      writeRetry,
      deadlineMs,
    });
  }

  /** Dispatch's refusal `code`, with `retryAfter` as its Retry-After and `key` carried back as its
   *  keyed layer carries a write's key, where given. */
  function refusal(status: number, code: string, retryAfter?: string, key?: string): Response {
    return new Response(JSON.stringify({ error: "busy", code }), {
      status,
      headers: {
        "Content-Type": "application/json",
        ...(retryAfter === undefined ? {} : { "Retry-After": retryAfter }),
        ...(key === undefined ? {} : { "Idempotency-Key": key }),
      },
    });
  }

  const posted = { id: "message-1", issue_key: "DSP-1" };

  /** The 201 Dispatch's keyed layer answers a write with, its key carried back. */
  function created(key: string): Response {
    return new Response(JSON.stringify(posted), {
      status: 201,
      headers: { "Content-Type": "application/json", "Idempotency-Key": key },
    });
  }

  /** A connection reset once the request went out: the attempt may have reached Dispatch. */
  function reset(): Response {
    throw Object.assign(new TypeError("The socket connection was closed"), {
      code: "ECONNRESET",
    });
  }

  /** Each request's Idempotency-Key, undefined for one that carries none. */
  const keysOf = (requests: readonly RecordedRequest[]) =>
    requests.map(
      (request) => (request.init.headers as Record<string, string | undefined>)["Idempotency-Key"]
    );

  /**
   * A Dispatch whose every answer to the write is made from the key the request carries (an
   * answer that throws is a failed fetch), and whose `/healthz` says `runsKeyedLayer` of its keyed
   * layer. `requests` holds the write's sends alone.
   */
  function keyedDispatch(answers: readonly ((key: string) => Response)[], runsKeyedLayer: boolean) {
    const requests: RecordedRequest[] = [];
    const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if (String(input).endsWith("/healthz")) {
        return jsonResponse({ idempotency_keys: runsKeyedLayer });
      }
      requests.push({ url: String(input), init: init ?? {} });
      const answer = answers[requests.length - 1];
      if (answer === undefined) throw new Error("unexpected request");
      return answer((init?.headers as Record<string, string>)["Idempotency-Key"] ?? "");
    }) as typeof fetch;
    return { fetchImpl, requests };
  }

  /** What `call` threw, which must be `DispatchWriteUnsettledError`. */
  async function unsettled(call: Promise<unknown>): Promise<DispatchWriteUnsettledError> {
    const error = await call.then(
      () => undefined,
      (caught: unknown) => caught
    );
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    return error as DispatchWriteUnsettledError;
  }

  test.each([
    "DATABASE_BUSY",
    "REQUEST_TIMEOUT",
  ])("a GET answered 503 %s succeeds after the Retry-After wait", async (code) => {
    const issue = { key: "DSP-1", title: "Issue" };
    const { fetchImpl, requests } = fakeFetch([refusal(503, code, "2"), jsonResponse(issue)]);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    await expect(client.getIssue("DSP-1")).resolves.toMatchObject(issue);

    expect(requests.map((request) => request.init.method)).toEqual(["GET", "GET"]);
    expect(state.waits).toHaveLength(1);
    expect(state.waits[0]).toBeGreaterThanOrEqual(2_000);
    expect(state.waits[0]).toBeLessThanOrEqual(2_250);
  });

  test.each([
    ["PUT", "OVERLOADED"],
    ["PUT", "EDIT_QUEUE_FULL"],
    ["DELETE", "OVERLOADED"],
    ["DELETE", "EDIT_QUEUE_FULL"],
  ] as const)("a %s answered 429 %s is sent again after its Retry-After", async (method, code) => {
    const { fetchImpl, requests } = fakeFetch([
      refusal(429, code, "2"),
      new Response(null, { status: 204 }),
    ]);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    await (method === "PUT"
      ? client.followAsk("ask-1", "session-1", actor)
      : client.unfollowAsk("ask-1", "session-1", actor));

    expect(requests.map((request) => request.init.method)).toEqual([method, method]);
    expect(keysOf(requests)).toEqual([undefined, undefined]);
    expect(state.waits).toHaveLength(1);
    expect(state.waits[0]).toBeGreaterThanOrEqual(2_000);
    expect(state.waits[0]).toBeLessThanOrEqual(2_250);
  });

  // A PUT or a DELETE carries no key, and a 503 can come after an earlier statement of a write
  // committed, so it comes back to its caller at once.
  test.each([
    ["PUT", "DATABASE_BUSY"],
    ["DELETE", "REQUEST_TIMEOUT"],
  ] as const)("a %s answered 503 %s comes back at once", async (method, code) => {
    const { fetchImpl, requests } = fakeFetch([refusal(503, code, "1")]);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    await expect(
      method === "PUT"
        ? client.followAsk("ask-1", "session-1", actor)
        : client.unfollowAsk("ask-1", "session-1", actor)
    ).rejects.toMatchObject({ name: "DispatchServiceError", code, status: 503, message: "busy" });
    expect(requests).toHaveLength(1);
    expect(state.waits).toEqual([]);
  });

  // A write's 429 is `try-later` in its table: nothing of the write ran, so it is asked again
  // under its one key, after the longer of its schedule's pause (1 s first) and the Retry-After.
  test.each([
    "OVERLOADED",
    "EDIT_QUEUE_FULL",
  ])("a POST answered 429 %s is sent again under its key after its Retry-After", async (code) => {
    const { fetchImpl, requests } = fakeFetch([refusal(429, code, "3"), jsonResponse(posted)]);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    await expect(client.message("DSP-1", { body: "Shipped.", actor })).resolves.toMatchObject(
      posted
    );

    expect(requests.map((request) => request.init.method)).toEqual(["POST", "POST"]);
    expect(requests.map(requestBody)).toEqual([
      { body: "Shipped.", actor },
      { body: "Shipped.", actor },
    ]);
    const keys = keysOf(requests);
    expect(keys[0]).toBeString();
    expect(keys).toEqual([keys[0], keys[0]]);
    expect(state.waits).toEqual([3_000]);
  });

  // One call is one intent, so every send of it carries the one key: the caller's own, where it
  // named one, else the one the call minted. The refusal was answered before Dispatch's keyed
  // layer claimed that key, so the repeat runs the write once rather than replaying an answer.
  test("repeats a write under the one key of its call, the caller's own or the call's", async () => {
    const uploaded = { artifact: { id: "doc-1" } };
    const { fetchImpl, requests } = fakeFetch([
      refusal(429, "EDIT_QUEUE_FULL", "1"),
      jsonResponse(uploaded),
      refusal(429, "OVERLOADED", "1"),
      jsonResponse({ id: "message-1", issue_key: "DSP-1" }),
    ]);
    const { writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);
    const callerKey = "askgate-doc.3f7a91";

    await expect(
      client.projectArtifact(
        "DSP",
        { name: "decisions.md", content: "# Decisions", actor },
        callerKey
      )
    ).resolves.toMatchObject(uploaded);
    await expect(client.message("DSP-1", { body: "Shipped.", actor })).resolves.toMatchObject({
      id: "message-1",
    });

    const keys = keysOf(requests);
    expect(keys.slice(0, 2)).toEqual([callerKey, callerKey]);
    expect(keys[2]).toBeString();
    expect(keys[3]).toBe(keys[2]);
    expect(keys[2]).not.toBe(callerKey);
  });

  // Dispatch sends each of these to a write before the write's COMMIT: admission's 429 OVERLOADED
  // before its keyed layer runs (no key back), the writer gate's 429 EDIT_QUEUE_FULL and a busy
  // bound's 503 DATABASE_BUSY from inside that layer (the key back), a busy bound's 503 before it
  // (no key back), and 503 REQUEST_TIMEOUT, which it sends only to a read, read as any 5xx. The
  // write is asked again under its one key after the Retry-After and returns the 2xx, whether that
  // answer was the first (`nothing-sent`) or came after an attempt that may have landed
  // (`may-have-landed`: a connection reset once the request went out). From there it asks only a
  // Dispatch that runs the keyed layer, as the echo or `/healthz` says.
  test.each([
    [429, "OVERLOADED", false, "nothing-sent"],
    [429, "OVERLOADED", false, "may-have-landed"],
    [429, "EDIT_QUEUE_FULL", true, "nothing-sent"],
    [429, "EDIT_QUEUE_FULL", true, "may-have-landed"],
    [503, "DATABASE_BUSY", true, "nothing-sent"],
    [503, "DATABASE_BUSY", true, "may-have-landed"],
    [503, "DATABASE_BUSY", false, "nothing-sent"],
    [503, "DATABASE_BUSY", false, "may-have-landed"],
    [503, "REQUEST_TIMEOUT", true, "nothing-sent"],
    [503, "REQUEST_TIMEOUT", true, "may-have-landed"],
  ] as const)("a POST answered %d %s (key back: %p) from %s is asked again under its key and returns the 2xx", async (status, code, echoed, from) => {
    const { fetchImpl, requests } = keyedDispatch(
      [
        ...(from === "may-have-landed" ? [reset] : []),
        (key) => refusal(status, code, "5", echoed ? key : undefined),
        created,
      ],
      true
    );
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    await expect(client.message("DSP-1", { body: "Shipped.", actor })).resolves.toMatchObject(
      posted
    );

    const keys = keysOf(requests);
    expect(keys).toHaveLength(from === "nothing-sent" ? 2 : 3);
    expect(keys[0]).toBeString();
    expect(new Set(keys).size).toBe(1);
    expect(state.waits.at(-1)).toBe(5_000);
  });

  // A write's 503 can come after an earlier statement of it committed, its key's claim among them,
  // so its table reads it as `server-error` (`may-have-landed`): a Dispatch not known to run the
  // keyed layer, which would run a repeat as a second write, is not sent it again, and the write
  // ends unsettled, carrying its key.
  test.each([
    "DATABASE_BUSY",
    "REQUEST_TIMEOUT",
  ])("a POST answered 503 %s is not sent again to a Dispatch not known to run the keyed layer", async (code) => {
    const { fetchImpl, requests } = keyedDispatch([() => refusal(503, code, "1")], false);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    const error = await unsettled(client.message("DSP-1", { body: "Shipped.", actor }));

    expect(requests).toHaveLength(1);
    expect(error.key).toBe(keysOf(requests)[0] ?? "");
    expect(error.last).toMatchObject({ name: "DispatchServiceError", code, status: 503 });
    expect(state.waits).toEqual([]);
  });

  // Dispatch answers `500 COMMIT_OUTCOME_UNKNOWN` when it sent the write's COMMIT and heard nothing
  // back, so the write may have landed. Its table reads it as `server-error`, which moves the write
  // to `may-have-landed`, so it is never asked again as if nothing ran: a Dispatch not known to run
  // the keyed layer is not sent it again at all, where its 429 is; one that runs it gets every
  // repeat under the one key, which it answers from what the key holds; and with no ask left the
  // write ends `DispatchWriteUnsettledError`, carrying that key.
  test("a POST answered 500 COMMIT_OUTCOME_UNKNOWN is never asked again as if nothing ran", async () => {
    const unknown = (key?: string) => refusal(500, "COMMIT_OUTCOME_UNKNOWN", undefined, key);
    const message = (client: DispatchClient) =>
      client.message("DSP-1", { body: "Shipped.", actor });

    const bare = keyedDispatch([() => unknown()], false);
    const bareClock = fakeClock();
    const bareEnd = await unsettled(message(retryClient(bare.fetchImpl, bareClock.writeRetry)));
    expect(bare.requests).toHaveLength(1);
    expect(bareEnd.key).toBe(keysOf(bare.requests)[0] ?? "");
    expect(bareEnd.last).toMatchObject({ code: "COMMIT_OUTCOME_UNKNOWN", status: 500 });
    expect(bareClock.state.waits).toEqual([]);

    const busy = keyedDispatch(
      [() => refusal(429, "OVERLOADED", "1"), () => jsonResponse(posted, 201)],
      false
    );
    await expect(
      message(retryClient(busy.fetchImpl, fakeClock().writeRetry))
    ).resolves.toMatchObject(posted);
    expect(busy.requests).toHaveLength(2);

    const keyed = keyedDispatch(
      Array.from({ length: 5 }, () => unknown),
      true
    );
    const keyedClock = fakeClock();
    const keyedEnd = await unsettled(message(retryClient(keyed.fetchImpl, keyedClock.writeRetry)));
    const keys = keysOf(keyed.requests);
    expect(keys).toHaveLength(5);
    expect(new Set(keys).size).toBe(1);
    expect(keyedEnd.key).toBe(keys[0] ?? "");
    expect(keyedEnd.attempts).toBe(5);
    expect(keyedEnd.last).toMatchObject({ code: "COMMIT_OUTCOME_UNKNOWN", status: 500 });
    expect(keyedClock.state.waits).toEqual([1_000, 2_000, 4_000, 8_000]);
  });

  test("answers any other refusal at once, as before", async () => {
    const page = new Response("<html><body>429 Too Many Requests</body></html>", {
      status: 429,
      headers: { "Content-Type": "text/html", "Retry-After": "1" },
    });
    for (const answer of [
      refusal(429, "RATE_LIMITED", "1"),
      refusal(503, "SERVICE_UNAVAILABLE", "1"),
      page,
    ]) {
      const { fetchImpl, requests } = fakeFetch([answer]);
      const { state, writeRetry } = fakeClock();
      const client = retryClient(fetchImpl, writeRetry);

      await expect(client.getIssue("DSP-1")).rejects.toMatchObject({ status: answer.status });
      expect(requests).toHaveLength(1);
      expect(state.waits).toEqual([]);
    }
  });

  // The deadline is the client's (`deadlineMs`), on the monotonic clock, which the fake clock does
  // not move, so these rows give the client a short one rather than moving the clock.
  test("starts no retry whose wait would pass the deadline", async () => {
    const late = fakeFetch([refusal(503, "DATABASE_BUSY", "1")]);
    const lateClock = fakeClock();
    const lateClient = retryClient(late.fetchImpl, lateClock.writeRetry, 1_000);

    await expect(lateClient.getIssue("DSP-1")).rejects.toMatchObject({
      code: "DATABASE_BUSY",
      status: 503,
      message:
        "busy; Dispatch refused 1 attempt with 503 DATABASE_BUSY, and waiting the 1 s it asks " +
        "for (Retry-After) would pass the client's deadline, so wait 1 s and try again",
    });
    expect(late.requests).toHaveLength(1);
    expect(lateClock.state.waits).toEqual([]);

    // The first 1 s wait fits in a 10 s deadline; the 30 s the second asks for would not.
    const { fetchImpl, requests } = fakeFetch([
      refusal(429, "OVERLOADED", "1"),
      refusal(429, "OVERLOADED", "30"),
    ]);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry, 10_000);

    await expect(client.getIssue("DSP-1")).rejects.toMatchObject({
      code: "OVERLOADED",
      status: 429,
      message:
        "busy; Dispatch refused 2 attempts with 429 OVERLOADED, and waiting the 30 s it asks " +
        "for (Retry-After) would pass the client's deadline, so wait 30 s and try again",
    });
    expect(requests).toHaveLength(2);
    expect(state.waits).toHaveLength(1);
  });

  // A write's asking has a bound of its own: no retry starts past `withinMs` (30 s) from its first
  // attempt. The first 30 s wait ends on that bound; the second would end past it, so it never
  // starts, and the write ends with the refusal as Dispatch wrote it, since nothing of it ran.
  test("a write starts no retry past its own bound, and ends with Dispatch's refusal", async () => {
    const { fetchImpl, requests } = fakeFetch([
      refusal(429, "OVERLOADED", "30"),
      refusal(429, "OVERLOADED", "30"),
    ]);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    await expect(client.message("DSP-1", { body: "Shipped.", actor })).rejects.toMatchObject({
      name: "DispatchServiceError",
      code: "OVERLOADED",
      status: 429,
      message: "busy",
    });
    expect(requests).toHaveLength(2);
    expect(state.waits).toEqual([30_000]);
  });

  test("gives up after two retries, its advice naming the wait and the attempts", async () => {
    // No Retry-After, and one that is not whole seconds, both wait the default 1 s.
    const { fetchImpl, requests } = fakeFetch([
      refusal(429, "OVERLOADED"),
      refusal(429, "OVERLOADED", "soon"),
      refusal(429, "OVERLOADED"),
    ]);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    await expect(client.getIssue("DSP-1")).rejects.toMatchObject({
      name: "DispatchServiceError",
      code: "OVERLOADED",
      status: 429,
      message:
        "busy; Dispatch refused 3 attempts with 429 OVERLOADED, waiting out its Retry-After " +
        "between them, so wait 1 s (Retry-After) and try again",
    });
    expect(requests).toHaveLength(3);
    expect(state.waits).toHaveLength(2);
    for (const wait of state.waits) {
      expect(wait).toBeGreaterThanOrEqual(1_000);
      expect(wait).toBeLessThanOrEqual(1_250);
    }
  });

  // A write keeps its own schedule (1, 2, 4 and 8 s apart, five attempts) under its one key, a
  // Retry-After that is missing or not whole seconds leaving the schedule's pause as it is, and
  // with no ask left it ends with the refusal as Dispatch wrote it, since nothing of it ran.
  test("a write refused on every attempt asks five times under its key, then ends with the refusal", async () => {
    const { fetchImpl, requests } = fakeFetch([
      refusal(429, "EDIT_QUEUE_FULL"),
      refusal(429, "EDIT_QUEUE_FULL", "soon"),
      refusal(429, "EDIT_QUEUE_FULL"),
      refusal(429, "EDIT_QUEUE_FULL", "soon"),
      refusal(429, "EDIT_QUEUE_FULL"),
    ]);
    const { state, writeRetry } = fakeClock();
    const client = retryClient(fetchImpl, writeRetry);

    await expect(
      client.docEdit("artifact-1", { ops: [{ op: "delete", find: "same" }], actor })
    ).rejects.toMatchObject({
      name: "DispatchServiceError",
      code: "EDIT_QUEUE_FULL",
      status: 429,
      message: "busy",
    });
    const keys = keysOf(requests);
    expect(keys).toHaveLength(5);
    expect(keys[0]).toBeString();
    expect(new Set(keys).size).toBe(1);
    expect(state.waits).toEqual([1_000, 2_000, 4_000, 8_000]);
  });

  // An attempt cut off for silence may still be running in Dispatch, so the repeat carries the key
  // that attempt carried: Dispatch answers it `409 KEY_IN_FLIGHT` and the call collects that run's
  // answer (dispatch-keyed-writes.test.ts), where a new key could run the write a second time. The
  // 429 after it was answered before the keyed layer ran, so it carries no key back, and the
  // client asks `/healthz` before it asks again; the 201 the keyed layer answers carries it.
  test("repeats a write cut off for silence under the key its first attempt carried", async () => {
    // An answer that never comes: the request waits until its signal aborts, then fails with its
    // reason, as fetch does, which is how `#attempt`'s silence cut ends it.
    const hang = (signal: AbortSignal | null | undefined): Promise<Response> => {
      const { promise, reject } = Promise.withResolvers<Response>();
      signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
      return promise;
    };
    const answers: ((
      signal: AbortSignal | null | undefined,
      key: string
    ) => Response | Promise<Response>)[] = [
      hang,
      () => refusal(429, "OVERLOADED", "1"),
      (_signal, key) => created(key),
    ];
    const keys: (string | undefined)[] = [];
    const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      // A silence cut is no answer at all, so the client asks `/healthz` whether this Dispatch
      // runs the keyed layer before it asks again under the key.
      if (String(input).endsWith("/healthz")) return jsonResponse({ idempotency_keys: true });
      const key = (init?.headers as Record<string, string>)["Idempotency-Key"];
      keys.push(key);
      const answer = answers[keys.length - 1];
      if (answer === undefined) throw new Error("unexpected request");
      return answer(init?.signal, key ?? "");
    }) as typeof fetch;
    const { state, writeRetry } = fakeClock();
    // One ask under the key on the schedule, and a silence bound short enough to cut the first
    // attempt at once: that bound runs on a real timer, unlike the waits, which only move the fake
    // clock.
    const client = retryClient(fetchImpl, {
      ...writeRetry,
      pausesMs: [1_000],
      withinMs: 30_000,
      silenceMs: 20,
    });

    await expect(client.message("DSP-1", { body: "Shipped.", actor })).resolves.toMatchObject(
      posted
    );

    expect(keys).toHaveLength(3);
    expect(keys[0]).toBeString();
    expect(new Set(keys).size).toBe(1);
    // The schedule's 1 s after the cut, then the in-flight pause of 3 s, longer than the 429's
    // Retry-After, since Dispatch may still be running the cut attempt.
    expect(state.waits).toEqual([1_000, 3_000]);
  });
});

// While a release rolls out, an older Dispatch process answers a route only the newer build serves
// with no route. No handler ran, so the client sends the request again, and the next attempt may
// reach the newer process.
describe("a request a Dispatch process answers with no route", () => {
  const noRoute = (method: string, path: string) =>
    jsonResponse(
      {
        code: "NOT_FOUND",
        error: `no route for ${method} ${path}`,
        hint: "GET /api/v1 lists every route",
      },
      404
    );
  const recordedSleep = () => {
    const waits: number[] = [];
    return {
      sleep: async (ms: number) => {
        waits.push(ms);
      },
      waits,
    };
  };

  test("is sent again, and the next answer resolves it", async () => {
    const { fetchImpl, requests } = fakeFetch([
      noRoute("GET", "/api/v1/projects"),
      jsonResponse([{ key: "CORE", name: "Core" }]),
    ]);
    const wait = recordedSleep();
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { sleep: wait.sleep }
    );

    expect(await client.listProjects()).toEqual([{ key: "CORE", name: "Core" }] as never);
    expect(requests).toHaveLength(2);
    // One wait of a second, jittered by a quarter either way.
    expect(wait.waits).toHaveLength(1);
    expect(wait.waits[0]).toBeGreaterThanOrEqual(750);
    expect(wait.waits[0]).toBeLessThanOrEqual(1_250);
  });

  test("throws the message that a release is in progress after four attempts", async () => {
    const { fetchImpl, requests } = fakeFetch(
      Array.from({ length: 4 }, () => noRoute("GET", "/api/v1/projects"))
    );
    const wait = recordedSleep();
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { sleep: wait.sleep }
    );

    const error = await client.listProjects().catch((thrown: unknown) => thrown);
    expect(error).toBeInstanceOf(DispatchNoRouteError);
    expect(error).toMatchObject({
      code: "NOT_FOUND",
      status: 404,
      message: NO_ROUTE_ROLLING_RELEASE_MESSAGE,
    });
    expect(NO_ROUTE_ROLLING_RELEASE_MESSAGE).toBe(
      "The Dispatch process that answered does not offer this action yet, because a new release " +
        "of Dispatch is in progress. Try again in one minute."
    );
    expect(requests).toHaveLength(4);
    // Waits of 1 s, 2 s and 4 s, each jittered by a quarter either way.
    expect(wait.waits).toHaveLength(3);
    for (const [index, base] of [1_000, 2_000, 4_000].entries()) {
      expect(wait.waits[index]).toBeGreaterThanOrEqual(base * 0.75);
      expect(wait.waits[index]).toBeLessThanOrEqual(base * 1.25);
    }
  });

  test("sends a POST again with the same body", async () => {
    const { fetchImpl, requests } = fakeFetch([
      noRoute("POST", "/api/v1/projects"),
      jsonResponse({ key: "CORE", name: "Core" }, 201),
    ]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { sleep: noWait }
    );

    await client.createProject({ key: "CORE", name: "Core" });
    expect(requests).toHaveLength(2);
    expect(requests.map((request) => request.init.method)).toEqual(["POST", "POST"]);
    expect(requestBody(requests[1] as RecordedRequest)).toEqual({ key: "CORE", name: "Core" });
    expect(requests[1]?.init.body).toBe(requests[0]?.init.body);
  });

  test("rejects at once with the abort, and is not sent again, when aborted during the wait", async () => {
    const { fetchImpl, requests } = fakeFetch([
      noRoute("GET", "/api/v1/projects"),
      jsonResponse([{ key: "CORE", name: "Core" }]),
    ]);
    const controller = new AbortController();
    const reason = new Error("a newer read replaced this one");
    // The shared wait, with the caller's abort arriving once the wait has begun.
    const wait: Sleep = (ms, signal) => {
      const waiting = sleep(ms, signal);
      controller.abort(reason);
      return waiting;
    };
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl,
      { signal: controller.signal, sleep: wait }
    );

    await expect(client.listProjects()).rejects.toBe(reason);
    expect(requests).toHaveLength(1);
  });
});

// A broker token the broker ended before it lapsed — a person's revoke of their sign-in does that
// — is answered `401 BROKER_TOKEN_NOT_ACTIVE`. Dispatch authenticates before any handler runs, so
// that answer says it did nothing: the client tells the credential, presents it afresh and sends
// the request once more, a write under its own key.
describe("a broker token Dispatch says is not live", () => {
  const notLive = () =>
    jsonResponse(
      {
        code: "BROKER_TOKEN_NOT_ACTIVE",
        error: "the secrets broker does not hold this access token as live",
      },
      401
    );

  /**
   * A credential like the broker's: each presentation carries the token it holds now, and
   * `stale`, which records what it was told (the session and the presentation it names), moves it
   * to the next of `tokens`. `refusal`, where given, is what a presentation after `stale` throws,
   * as a trade the broker refuses does. `headers` keeps every object it answered, so a row can
   * check that `stale` names the very presentation the dead token went out in.
   */
  function brokered(tokens: readonly string[], refusal?: Error) {
    const stales: (string | undefined)[] = [];
    const told: (Readonly<Record<string, string>> | undefined)[] = [];
    const answered: Readonly<Record<string, string>>[] = [];
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        if (refusal !== undefined && stales.length > 0) throw refusal;
        const headers = { Authorization: `DPoP ${tokens[stales.length]}`, DPoP: "proof" };
        answered.push(headers);
        return headers;
      },
      stale: (session, presented) => {
        stales.push(session);
        told.push(presented);
      },
      secret: () => tokens[stales.length] ?? "",
    };
    return { credential, stales, told, answered };
  }

  /** What each request presented, in order. */
  const presented = (requests: readonly RecordedRequest[]) =>
    requests.map((request) => (request.init.headers as Record<string, string>).Authorization);

  test("a read is sent once more with a fresh token", async () => {
    const listed = [{ key: "DSP-1", title: "DSP-1", status: "todo" }];
    const { fetchImpl, requests } = fakeFetch([notLive(), jsonResponse(listed)]);
    const { credential, stales, told, answered } = brokered(["dead", "fresh"]);
    const client = new DispatchClient("http://dispatch.test", credential, fetchImpl);

    expect(await client.listAllIssues({})).toEqual(listed as unknown as IssueSummary[]);
    expect(stales).toEqual([undefined]);
    // The credential is told which presentation was dead: the first `headers` answer itself.
    expect(told).toHaveLength(1);
    expect(told[0]).toBe(answered[0] as Readonly<Record<string, string>>);
    expect(presented(requests)).toEqual(["DPoP dead", "DPoP fresh"]);
  });

  test("a write is sent once more with a fresh token, under the same Idempotency-Key", async () => {
    const { fetchImpl, requests } = fakeFetch([notLive(), jsonResponse({ id: "message-1" }, 201)]);
    const { credential, stales, told, answered } = brokered(["dead", "fresh"]);
    const client = new DispatchClient("http://dispatch.test", credential, fetchImpl);

    await client.message("DSP-1", { body: "Shipped.", actor });
    expect(stales).toEqual([undefined]);
    expect(told).toHaveLength(1);
    expect(told[0]).toBe(answered[0] as Readonly<Record<string, string>>);
    expect(presented(requests)).toEqual(["DPoP dead", "DPoP fresh"]);
    const keys = requests.map(
      (request) => (request.init.headers as Record<string, string>)["Idempotency-Key"]
    );
    expect(keys[0]).toBeString();
    expect(keys[1]).toBe(keys[0] as string);
    expect(requests.map(requestBody)).toEqual([
      { body: "Shipped.", actor },
      { body: "Shipped.", actor },
    ]);
  });

  test("a second such answer is the call's: no third send", async () => {
    for (const call of [
      (client: DispatchClient) => client.listAllIssues({}),
      (client: DispatchClient) => client.message("DSP-1", { body: "Shipped.", actor }),
    ]) {
      const { fetchImpl, requests } = fakeFetch([notLive(), notLive()]);
      const { credential, stales } = brokered(["dead", "also-dead"]);
      const client = new DispatchClient("http://dispatch.test", credential, fetchImpl);

      await expect(call(client)).rejects.toMatchObject({
        status: 401,
        code: "BROKER_TOKEN_NOT_ACTIVE",
        message: "the secrets broker does not hold this access token as live",
      });
      expect(stales).toHaveLength(1);
      expect(requests).toHaveLength(2);
    }
  });

  test("a 401 with another code is not sent again", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse(
        {
          code: "BROKER_TOKEN_PROOF_INVALID",
          error: "the DPoP proof does not meet RFC 9449",
        },
        401
      ),
    ]);
    const { credential, stales } = brokered(["live", "unused"]);
    const client = new DispatchClient("http://dispatch.test", credential, fetchImpl);

    await expect(client.listAllIssues({})).rejects.toMatchObject({
      status: 401,
      code: "BROKER_TOKEN_PROOF_INVALID",
    });
    expect(stales).toEqual([]);
    expect(requests).toHaveLength(1);
  });

  test("a bearer, which has no other token to present, is not sent again", async () => {
    const { fetchImpl, requests } = fakeFetch([notLive()]);
    const client = new DispatchClient(
      "http://dispatch.test",
      bearerCredential("secret"),
      fetchImpl
    );

    await expect(client.listAllIssues({})).rejects.toMatchObject({
      status: 401,
      code: "BROKER_TOKEN_NOT_ACTIVE",
    });
    expect(requests).toHaveLength(1);
  });

  test("a credential that cannot present again fails the call in its own words", async () => {
    // After a revoke the broker refuses the new trade, and its words name `agent-secrets login`,
    // which Dispatch's 401 does not.
    const refusal = new Error(
      "the secrets broker refused ada@example.com's sign-in on this machine: HTTP 400 invalid_grant: ada@example.com revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with `agent-secrets login`."
    );
    for (const call of [
      (client: DispatchClient) => client.listAllIssues({}),
      (client: DispatchClient) => client.message("DSP-1", { body: "Shipped.", actor }),
    ]) {
      const { fetchImpl, requests } = fakeFetch([notLive()]);
      const { credential, stales } = brokered(["dead"], refusal);
      const client = new DispatchClient("http://dispatch.test", credential, fetchImpl);

      await expect(call(client)).rejects.toThrow(refusal.message);
      expect(stales).toHaveLength(1);
      expect(requests).toHaveLength(1);
    }
  });

  // The two ways one call sends its request again meet in each path's own loop: a token that is
  // not live is traded afresh once a call, and an answer to wait out is asked again after its
  // `Retry-After`. A write's are its table's (`dead-token`, then `try-later` on the write's
  // pacing): every send carries the call's one key whichever of the two repeated it, and a token
  // that is not live again after the wait is the call's answer.
  test("a write that meets a dead token and an overload keeps one Idempotency-Key", async () => {
    const overloaded = () =>
      new Response(JSON.stringify({ error: "busy", code: "OVERLOADED" }), {
        status: 429,
        headers: { "Content-Type": "application/json", "Retry-After": "1" },
      });
    const waits: number[] = [];
    const writeRetry: DispatchWriteRetry = {
      ...DISPATCH_WRITE_RETRY,
      now: () => 0,
      pause: async (ms) => {
        waits.push(ms);
      },
    };

    const sent = fakeFetch([notLive(), overloaded(), jsonResponse({ id: "message-1" }, 201)]);
    const traded = brokered(["dead", "fresh"]);
    await new DispatchClient("http://dispatch.test", traded.credential, sent.fetchImpl, {
      writeRetry,
    }).message("DSP-1", { body: "Shipped.", actor });
    expect(traded.stales).toEqual([undefined]);
    expect(presented(sent.requests)).toEqual(["DPoP dead", "DPoP fresh", "DPoP fresh"]);
    const keys = sent.requests.map(
      (request) => (request.init.headers as Record<string, string>)["Idempotency-Key"]
    );
    expect(keys[0]).toBeString();
    expect(keys).toEqual([keys[0], keys[0], keys[0]]);
    expect(waits).toHaveLength(1);

    const refused = fakeFetch([overloaded(), notLive(), notLive()]);
    const dead = brokered(["dead", "also-dead"]);
    await expect(
      new DispatchClient("http://dispatch.test", dead.credential, refused.fetchImpl, {
        writeRetry,
      }).message("DSP-1", { body: "Shipped.", actor })
    ).rejects.toMatchObject({ status: 401, code: "BROKER_TOKEN_NOT_ACTIVE" });
    expect(dead.stales).toHaveLength(1);
    expect(presented(refused.requests)).toEqual(["DPoP dead", "DPoP dead", "DPoP also-dead"]);
    const refusedKeys = refused.requests.map(
      (request) => (request.init.headers as Record<string, string>)["Idempotency-Key"]
    );
    expect(refusedKeys[0]).toBeString();
    expect(refusedKeys).toEqual([refusedKeys[0], refusedKeys[0], refusedKeys[0]]);
  });

  // A read's are `#send`'s: the credential is told once a `#send`, whichever of its sends drew the
  // dead token, and the wait-out counts its attempts apart from that resend.
  test("a read that meets a dead token and an overload tells the credential once", async () => {
    const overloaded = () =>
      new Response(JSON.stringify({ error: "busy", code: "OVERLOADED" }), {
        status: 429,
        headers: { "Content-Type": "application/json", "Retry-After": "1" },
      });
    const waits: number[] = [];
    const writeRetry: DispatchWriteRetry = {
      ...DISPATCH_WRITE_RETRY,
      pause: async (ms) => {
        waits.push(ms);
      },
    };
    const issue = { key: "DSP-1", title: "Issue" };

    const sent = fakeFetch([notLive(), overloaded(), jsonResponse(issue)]);
    const traded = brokered(["dead", "fresh"]);
    await expect(
      new DispatchClient("http://dispatch.test", traded.credential, sent.fetchImpl, {
        writeRetry,
      }).getIssue("DSP-1")
    ).resolves.toMatchObject(issue);
    expect(traded.stales).toEqual([undefined]);
    expect(presented(sent.requests)).toEqual(["DPoP dead", "DPoP fresh", "DPoP fresh"]);
    expect(waits).toHaveLength(1);

    const refused = fakeFetch([overloaded(), notLive(), notLive()]);
    const dead = brokered(["dead", "also-dead"]);
    await expect(
      new DispatchClient("http://dispatch.test", dead.credential, refused.fetchImpl, {
        writeRetry,
      }).getIssue("DSP-1")
    ).rejects.toMatchObject({ status: 401, code: "BROKER_TOKEN_NOT_ACTIVE" });
    expect(dead.stales).toHaveLength(1);
    expect(presented(refused.requests)).toEqual(["DPoP dead", "DPoP dead", "DPoP also-dead"]);
    expect(waits).toHaveLength(2);
  });
});
