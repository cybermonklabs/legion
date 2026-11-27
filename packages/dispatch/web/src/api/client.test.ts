import { expect, test } from "bun:test";
import { NO_ROUTE_ROLLING_RELEASE_MESSAGE, type Sleep, sleep } from "@legion/contracts";

import { isPersonDecision } from "../features/issue/decision-entries";
import { ApiError, createApiClient, type FetchImplementation, isNoRoute } from "./client";

// While a release rolls out, an older Dispatch process answers a route only the newer build serves
// with this 404. No handler ran, so the page sends the request again, and the next attempt may
// reach the newer process.
function noRoute(method: string, path: string): Response {
  return Response.json(
    {
      code: "NOT_FOUND",
      error: `no route for ${method} ${path}`,
      hint: "GET /api/v1 lists every route",
    },
    { status: 404 }
  );
}

/** A fetch that answers each request with the next of `answers`, recording what it was sent. */
function scriptedFetch(answers: readonly (() => Response)[]): {
  fetch: FetchImplementation;
  requests: { path: string; init: RequestInit | undefined }[];
} {
  const requests: { path: string; init: RequestInit | undefined }[] = [];
  return {
    fetch: async (input, init) => {
      requests.push({ path: input.toString(), init });
      const answer = answers[requests.length - 1];
      if (answer === undefined) throw new Error(`unexpected request ${requests.length}`);
      return answer();
    },
    requests,
  };
}

test("the decisions read follows the Decisions tree view's cursor", async () => {
  const stub = scriptedFetch([
    () => Response.json({ nodes: [], tuples: [], cursor: "page-2" }),
    () => Response.json({ nodes: [], tuples: [], cursor: null }),
  ]);
  const api = createApiClient(stub.fetch);

  expect((await api.getIssueDecisions("CORE-1")).entries).toEqual([]);
  expect(stub.requests.map(({ path }) => path)).toEqual([
    "/api/v1/issues/CORE-1/tree?view=decisions",
    "/api/v1/issues/CORE-1/tree?view=decisions&cursor=page-2",
  ]);
});

// A server of the release before serves no tree read, and answers its Decisions read with the
// spec's open questions and the sections of its decisions document, with no `entries`: once every
// attempt of the Decisions view met no route, that read is read instead, and each of its questions
// and sections comes back as the entry the page draws.
test("the decisions read falls back to the Decisions read where the tree read met no route", async () => {
  const stub = scriptedFetch([
    ...Array.from({ length: 4 }, () => () => noRoute("GET", "/api/v1/issues/CORE-1/tree")),
    () =>
      Response.json({
        decisions: [
          {
            asks: [],
            block_id: "d1",
            changed_at: "2026-09-09T00:00:00Z",
            changed_version: 2,
            heading: "Store the notices",
            kept: null,
            kind: "agent",
            links: [],
            ref: "dispatch://CORE-1/artifact/choices#b-d1",
            replaced: false,
            text: "Store the notices.",
            text_sha256: "sha256:d1",
            unsure: true,
            waiting_review: false,
          },
        ],
        document: "artifact-choices",
        open_questions: [
          {
            id: "ask-1",
            issue_key: "CORE-1",
            multiple: false,
            options: [{ label: "Ship" }, { label: "Hold" }],
            question: "Ship it?",
            urgency: "normal",
          },
        ],
      }),
  ]);
  const api = createApiClient(stub.fetch, undefined, recordedSleep().sleep);

  const read = await api.getIssueDecisions("CORE-1");
  expect(read.entries).toMatchObject([
    {
      answer_state: "open",
      kind: "person",
      node: { id: "ask-1", kind: "ask" },
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Ship it?",
      ref: "dispatch://CORE-1/ask/ask-1",
      text: "Ship it?",
    },
    {
      answer_state: null,
      changed_version: 2,
      kind: "agent",
      node: { id: "dispatch://CORE-1/artifact/choices#b-d1", kind: "block" },
      question: null,
      ref: "dispatch://CORE-1/artifact/choices#b-d1",
      text: "Store the notices.",
      text_sha256: "sha256:d1",
      uncertainty: "high",
    },
  ]);
  expect(stub.requests.map(({ path }) => path)).toEqual([
    ...Array.from({ length: 4 }, () => "/api/v1/issues/CORE-1/tree?view=decisions"),
    "/api/v1/issues/CORE-1/decisions",
  ]);
});

// The Decisions tab draws a person's decision by the answer's words, from the tree the server read
// them into: the entry carries the tree the tree read answers for the ask's answer, so the page
// parses nothing.
test("an answered ask's entry carries the tree the Decisions view answers for its answer", async () => {
  const answerTree = {
    content: [
      {
        content: [{ marks: [{ type: "strong" }], text: "answer", type: "text" }],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const stub = scriptedFetch([
    () =>
      Response.json({
        cursor: null,
        nodes: [
          {
            ask: {
              answer: "An **answer**.",
              answer_tree: answerTree,
              multiple: false,
              options: [{ label: "Ship" }],
              selected: ["Ship"],
              urgency: "normal",
            },
            attributes: { answer_state: "answered" },
            author: [],
            id: "ask-1",
            kind: "ask",
            made_at: "2026-09-09T00:00:00Z",
            parent: null,
            role: "query",
            state: "live",
            supertag: "ask",
            text: "Ship it?",
          },
        ],
        tuples: [],
      }),
  ]);
  const api = createApiClient(stub.fetch);

  const entry = (await api.getIssueDecisions("CORE-1")).entries?.[0];
  expect(entry).toMatchObject({ answer: "An **answer**.", answer_state: "answered" });
  expect(entry?.answer_tree).toEqual(answerTree);
});

// A `decision` block a person wrote, which nobody kept, is that person's decision: the Decisions
// view answers it kind `agent`, the node's shape, waiting for nobody's review, with its
// `decided_by` tuple to the person, and the entry carries that decider, so the Decisions tab lists
// it under "Your decisions" (`isPersonDecision`). The kind `person` the old `decisions` member of
// `GET /api/v1/issues/{key}/decisions` answers for such a block is for a page of the release
// before, which reads that member alone; the view carries no such kind.
test("a decision block a person decided reads as theirs from its decided_by tuple", async () => {
  const block = { id: "CORE-1/tree#d1", kind: "block" };
  const person = { id: "alice", kind: "person" };
  const stub = scriptedFetch([
    () =>
      Response.json({
        cursor: null,
        nodes: [
          {
            ...block,
            author: [],
            block_id: "d1",
            decision: {
              asks: [],
              block_id: "d1",
              changed_at: "2026-09-09T00:00:00Z",
              changed_version: 1,
              heading: "Cap each table at a million rows.",
              kept: null,
              kind: "agent",
              links: [],
              ref: "dispatch://CORE-1/spec#b-d1",
              replaced: false,
              text: "Cap each table at a million rows.",
              text_sha256: "sha256:d1",
              uncertainty: null,
              uncertainty_reason: null,
              unsure: false,
              waiting_review: false,
            },
            made_at: "2026-09-09T00:00:00Z",
            parent: null,
            role: "query",
            state: "live",
            supertag: "decision",
            text: "Cap each table at a million rows.",
          },
          {
            ...person,
            author: [],
            made_at: "2026-09-09T00:00:00Z",
            parent: null,
            role: "context",
            state: "live",
            supertag: "person",
            text: "",
          },
        ],
        tuples: [{ from: block, kind: "decided_by", to: person }],
      }),
  ]);
  const api = createApiClient(stub.fetch);

  const entries = (await api.getIssueDecisions("CORE-1")).entries ?? [];
  expect(entries).toHaveLength(1);
  expect(entries[0]).toMatchObject({
    decided_by: person,
    kept: null,
    kind: "agent",
    node: block,
    waiting_review: false,
  });
  expect(entries.map(isPersonDecision)).toEqual([true]);
});

/** A sleep that returns at once and records each wait it was asked for. */
function recordedSleep(): { sleep: (ms: number) => Promise<void>; waits: number[] } {
  const waits: number[] = [];
  return {
    sleep: async (ms) => {
      waits.push(ms);
    },
    waits,
  };
}

test("a no-route answer is sent again, and the next answer resolves the request", async () => {
  const stub = scriptedFetch([
    () => noRoute("GET", "/auth/whoami"),
    () => Response.json({ event_head: "0", kind: "user", login: "alice" }),
  ]);
  const wait = recordedSleep();
  const api = createApiClient(stub.fetch, undefined, wait.sleep);

  expect(await api.whoAmI()).toEqual({ event_head: "0", kind: "user", login: "alice" });
  expect(stub.requests.map((request) => request.path)).toEqual(["/auth/whoami", "/auth/whoami"]);
  // One wait of a second, jittered by a quarter either way.
  expect(wait.waits).toHaveLength(1);
  expect(wait.waits[0]).toBeGreaterThanOrEqual(750);
  expect(wait.waits[0]).toBeLessThanOrEqual(1_250);
});

test("four no-route answers throw the message that a release is in progress", async () => {
  const stub = scriptedFetch(Array.from({ length: 4 }, () => () => noRoute("GET", "/auth/whoami")));
  const wait = recordedSleep();
  const api = createApiClient(stub.fetch, undefined, wait.sleep);

  const error = await api.whoAmI().catch((thrown: unknown) => thrown);
  expect(error).toBeInstanceOf(ApiError);
  expect(error).toMatchObject({
    code: "NOT_FOUND",
    message: NO_ROUTE_ROLLING_RELEASE_MESSAGE,
    status: 404,
  });
  expect(NO_ROUTE_ROLLING_RELEASE_MESSAGE).toBe(
    "The Dispatch process that answered does not offer this action yet, because a new release " +
      "of Dispatch is in progress. Try again in one minute."
  );
  expect(isNoRoute(error)).toBe(false);
  expect(stub.requests).toHaveLength(4);
  // Waits of 1 s, 2 s and 4 s, each jittered by a quarter either way.
  expect(wait.waits).toHaveLength(3);
  for (const [index, base] of [1_000, 2_000, 4_000].entries()) {
    expect(wait.waits[index]).toBeGreaterThanOrEqual(base * 0.75);
    expect(wait.waits[index]).toBeLessThanOrEqual(base * 1.25);
  }
});

// A server of the release before serves no tree read and holds no view: once every attempt of the
// view read met no route, the issue is no view, and no failure reaches the page.
test("the view read answers no view once every attempt met no route for the tree read", async () => {
  const stub = scriptedFetch(
    Array.from({ length: 4 }, () => () => noRoute("GET", "/api/v1/issues/CORE-2/tree"))
  );
  const api = createApiClient(stub.fetch, undefined, recordedSleep().sleep);

  expect(await api.getIssueViewOf("CORE-2")).toBeNull();
  expect(stub.requests.map(({ path }) => path)).toEqual(
    Array.from({ length: 4 }, () => "/api/v1/issues/CORE-2/tree?view=spec")
  );
});

test("the view read answers the spec view's view_of, and throws any other refusal", async () => {
  const stub = scriptedFetch([
    () => Response.json({ cursor: null, nodes: [], tuples: [], view: "spec", view_of: "CORE-1" }),
    () => Response.json({ code: "NOT_FOUND", error: "issue not found" }, { status: 404 }),
  ]);
  const api = createApiClient(stub.fetch, undefined, recordedSleep().sleep);

  expect(await api.getIssueViewOf("CORE-2")).toBe("CORE-1");
  await expect(api.getIssueViewOf("CORE-9")).rejects.toMatchObject({
    code: "NOT_FOUND",
    message: "issue not found",
    status: 404,
  });
  expect(stub.requests.map(({ path }) => path)).toEqual([
    "/api/v1/issues/CORE-2/tree?view=spec",
    "/api/v1/issues/CORE-9/tree?view=spec",
  ]);
});

test("a POST answered with no route is sent again with the same body", async () => {
  const stub = scriptedFetch([
    () => noRoute("POST", "/api/v1/issues"),
    () => Response.json({ key: "CORE-1" }, { status: 201 }),
  ]);
  const api = createApiClient(stub.fetch, undefined, recordedSleep().sleep);

  await api.createIssue({ project: "CORE", title: "Ship the widgets" }, { key: "write-1" });
  expect(stub.requests).toHaveLength(2);
  const [first, second] = stub.requests;
  expect(second?.path).toBe("/api/v1/issues");
  expect(second?.init?.method).toBe("POST");
  // The same intent, so the same key: no handler ran under the first attempt's no-route answer.
  expect(second?.init?.headers).toEqual(first?.init?.headers as HeadersInit);
  // The client sends its headers as pairs, and a header name is case-insensitive - Bun's own
  // Headers lower-cases every name while the DOM this suite preloads keeps the case it was given -
  // so the names are folded here rather than written as one runtime spells them.
  const headersOf = (init: RequestInit | undefined): Record<string, string> =>
    Object.fromEntries(
      [...new Headers(init?.headers).entries()].map(([name, value]) => [name.toLowerCase(), value])
    );
  expect(headersOf(second?.init)).toMatchObject({
    "content-type": "application/json",
    "idempotency-key": "write-1",
  });
  expect(JSON.parse(String(second?.init?.body))).toEqual({
    project: "CORE",
    title: "Ship the widgets",
  });
  expect(second?.init?.body).toBe(first?.init?.body);
});

test("a 404 NOT_FOUND for a resource the server does not know is not sent again", async () => {
  const stub = scriptedFetch([
    () => Response.json({ code: "NOT_FOUND", error: "issue not found" }, { status: 404 }),
  ]);
  const wait = recordedSleep();
  const api = createApiClient(stub.fetch, undefined, wait.sleep);

  await expect(api.getIssue("CORE-1")).rejects.toMatchObject({
    code: "NOT_FOUND",
    message: "issue not found",
    status: 404,
  });
  expect(stub.requests).toHaveLength(1);
  expect(wait.waits).toEqual([]);
});

test("a request aborted during a no-route wait rejects at once with the abort and is not sent again", async () => {
  const stub = scriptedFetch([
    () => noRoute("GET", "/api/github/rest/user"),
    () => Response.json({ login: "alice" }),
  ]);
  const controller = new AbortController();
  const reason = new Error("a newer read replaced this one");
  // The shared wait, with the caller's abort arriving once the wait has begun.
  const wait: Sleep = (ms, signal) => {
    const waiting = sleep(ms, signal);
    controller.abort(reason);
    return waiting;
  };
  // `githubRest` hands the caller's init to the one request path every method shares, so the
  // caller's signal is the one the wait is given.
  const api = createApiClient(stub.fetch, undefined, wait);

  await expect(api.githubRest("user", { signal: controller.signal })).rejects.toBe(reason);
  expect(stub.requests).toHaveLength(1);
});
