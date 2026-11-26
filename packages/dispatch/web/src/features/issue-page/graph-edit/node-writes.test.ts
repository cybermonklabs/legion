import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { http } from "msw";
import { createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { onFakeClock, settle } from "../../../__tests__/fake-clock";
import { type KeptTab, keptTab, memoryFiles } from "../../../__tests__/kept-sends-harness";
import { answerRequests } from "../../../__tests__/network";
import {
  ApiError,
  api,
  NetworkError,
  RequestTimeoutError,
  type WriteRequest,
  withIfMatch,
  writeRequest,
} from "../../../api/client";
import { newSendKey } from "../../../lib/send-key";
import { plainWrite, Writes } from "../../writes/write-store";
import { ANSWER_LINES, CONTROL_LABELS } from "../page-words";
import { outlineQuery, readNextTreePage, type TreeAnswer } from "../tree-read/tree-read";
import { fixture } from "./graph-edit-fixtures";
import { type NodeWrites, refusalLine, useNodeWrites } from "./node-writes";

/**
 * The writes of Graph's node menu: `graph-edit/node-writes.ts` (R4, R7, R8 of INFRA-5705's spec),
 * and the header the two node routes need, in `api/client.ts` (`withIfMatch`, and the sender that
 * puts the header on the request) and in `features/writes/write-storage.ts` (the kept request).
 *
 * The interface this file expects of `node-writes.ts`. Every answer a case reads is the server's
 * own, from `__fixtures__/graph-edit.json`; the requests are answered at `fetch`, so the header and
 * the body that reach the wire are the ones asserted.
 *
 * - `refusalLine(error: unknown, context?: { held: TreeAnswer; node: string }): RefusalLine`, the
 *   line a row draws for a write that did not go: `error` is what the write store holds, an
 *   `ApiError`, a `NetworkError`, a `RequestTimeoutError`; `context` is the held outline read and
 *   the read id of the node that was written. `RefusalLine` is
 *   `{ text, container?, reads, gone, retry }`:
 *   - `text`: the whole line, in the page's words (`ANSWER_LINES`, key `refused_` and the code in
 *     lower case). Any other code, a 5xx and no answer give `ANSWER_LINES.not_made`, then
 *     `ANSWER_LINES.not_made_code` and the code in brackets where there is one;
 *   - `container`: the read id of the row a `409 CONTAINER_WOULD_EMPTY` links to, where the held
 *     tree shows the node's parent as a list item or a callout. `text` then reads
 *     `ANSWER_LINES.container_empties`, the kind (`CONTROL_LABELS.container_list_item` or
 *     `container_callout`) and the parent's first line; else it reads the code's own words and
 *     quotes the server's message;
 *   - `reads`: the page reads the tree again (412, `NODE_GONE`, 404); `gone`: the row leaves where
 *     the read lacks it (`NODE_GONE`, 404); `retry`: a press on the line tries the act again (the
 *     fallback line alone).
 * - `useNodeWrites(issue: { key: string; status: string; zoomRoot?: string }): NodeWrites`, a hook
 *   under the page's query client and router, whose held tree is Graph's outline read:
 *   - `move(node, entry, row?)` queues `entry` (`indent`, `outdent`, `move_up`, `move_down`) for the
 *     node of that read id (`row` as `placementOf` takes it) and resolves with `{ status:
 *     "taken" }`, `{ status: "refused" }` or `{ status: "dropped", reason }` once it is sent,
 *     refused or dropped. The queue is one for each node; a move's body is made by `placementOf`
 *     when the move leaves the queue, on the held read with each earlier answer of the queue
 *     written into it (`setQueryData`, the answer's `parent`, `after` and `placement_seq`), and
 *     goes as one `PATCH` through the write store with `withIfMatch`;
 *   - `lines: ReadonlyMap<string, { text: string }>`, the line of each row by read id, which a
 *     refusal and a drop write: `refusalLine`'s `text`, then, where moves were dropped, their
 *     count and `ANSWER_LINES.dropped_move` or `dropped_moves` ("2 moves were not sent");
 *   - once a node's queue is empty after a taken answer, the hook reads each outline read the page
 *     holds again, its loaded pages included, once; after a 412, a `NODE_GONE` or a 404 it reads
 *     them once too; after any other refusal it reads nothing.
 * - `withIfMatch<T>(request: WriteRequest<T>, placementSeq: number | string): WriteRequest<T>` in
 *   `api/client.ts`: the request with `headers: { "If-Match": '"<placementSeq>"' }`, the one
 *   precondition header the page sends; `api.send` sets each header the request carries, and a
 *   kept request (`write-storage.ts`) keeps `headers` and gives it back.
 */

const KEY = "CORE-1";
const ROUTE = "/api/v1/issues/{key}/nodes/{id}";

/** The fixture's stable name of the issue, which no route of the page takes for a key. */
const NAMED = "<issue>";

/** `value` with the fixture's name of the issue written as the key the routes take: a name for
 *  a name, no other change to what the server answered. */
function renamed<T>(value: T): T {
  return JSON.parse(JSON.stringify(value).replaceAll(NAMED, KEY)) as T;
}

const converted = renamed(fixture.tree.converted.person.body) as unknown as TreeAnswer;
const moved = fixture.moved;
const refused = fixture.refused;

/** The statement every case moves: the third of the criterion, which the server's own moves of the
 *  fixture are the moves of. */
const STATEMENT = "<statement-3>";

/** A client signed in as alice, whose held reads stay held. */
function signedIn(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return client;
}

/** The answer of one exchange of the fixture, as the route sent it. */
interface Exchange {
  readonly status: number;
  readonly body: unknown;
}

/** One `PATCH` as it reached the wire. */
interface Patch {
  readonly id: string;
  readonly ifMatch: string | null;
  readonly body: unknown;
}

/** Answers each `PATCH` of a node of the issue with the next of `answers`, held until its `gate`
 *  settles where it has one, and keeps each request as it came. */
function answerPatches(...answers: readonly (Exchange & { gate?: Promise<void> })[]): {
  readonly calls: Patch[];
} {
  const calls: Patch[] = [];
  const pending = [...answers];
  answerRequests(
    http.patch(`/api/v1/issues/${KEY}/nodes/:id`, async ({ params, request }) => {
      calls.push({
        body: await request.json(),
        id: decodeURIComponent(String(params.id)),
        ifMatch: request.headers.get("If-Match"),
      });
      const next = pending.shift();
      if (next === undefined) throw new Error("a PATCH came that the case answered nothing for");
      await next.gate;
      return Response.json(renamed(next.body), { status: next.status });
    })
  );
  return { calls };
}

/** Answers each read of the issue's tree with the fixture's: the first page of `pages` where it
 *  names no cursor, the page it names otherwise. Keeps each read's cursor, null where it names
 *  none. */
function answerTreeReads(pages: readonly unknown[] = [fixture.tree.converted.person]): {
  readonly cursors: (string | null)[];
} {
  const cursors: (string | null)[] = [];
  answerRequests(
    http.get(`/api/v1/issues/${KEY}/tree`, ({ request }) => {
      const cursor = new URL(request.url).searchParams.get("cursor");
      cursors.push(cursor);
      const page = cursor === null ? pages[0] : pages[1];
      return Response.json(renamed((page as { body: unknown }).body));
    })
  );
  return { cursors };
}

/** Lets whatever a case's requests would send go out and be answered, for a case that asserts that
 *  nothing more happens. */
async function letRequestsSettle(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, 50);
  await act(() => promise);
}

/** The page's hook under the page's query client and router, over `client`. */
function hookOver(client: QueryClient) {
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(
      QueryClientProvider,
      { client },
      createElement(MemoryRouter, { initialEntries: [`/issues/${KEY}/graph`] }, children)
    );
  return renderHook(() => useNodeWrites({ key: KEY, status: "in_progress" }), { wrapper });
}

/** The page's hook, with the held outline read in its client. */
function mount(held: TreeAnswer = converted) {
  const client = signedIn();
  client.setQueryData(outlineQuery(KEY, "graph").queryKey, held);
  const view = hookOver(client);
  return { client, view, writes: (): NodeWrites => view.result.current };
}

/** The held read's node `id`, as the cache holds it now. */
function heldNode(client: QueryClient, id: string) {
  const held = client.getQueryData<TreeAnswer>(outlineQuery(KEY, "graph").queryKey);
  return held?.nodes?.find((node) => node.id === id);
}

/** The words of `ANSWER_LINES` by key. */
const WORDS: Readonly<Record<string, string>> = ANSWER_LINES;
const LABELS: Readonly<Record<string, string>> = CONTROL_LABELS;

/** What the two node routes may refuse with, read from the contract (`packages/contracts`): every
 *  `[45]\d\d CODE` the description of the `PATCH` and the one of the `DELETE` name, and the
 *  refusal that no description names, `409 NODE_GONE`. A code either route adds fails the
 *  inventory below until it has its words. */
const OPENAPI = new URL(
  "../../../../../../contracts/schemas/dispatch-openapi.json",
  import.meta.url
);

function refusalsOfTheRoutes(): { readonly status: number; readonly code: string }[] {
  const route = (
    JSON.parse(readFileSync(OPENAPI, "utf8")) as {
      paths: Record<string, Record<string, { description?: string }>>;
    }
  ).paths[ROUTE];
  const found = new Map<string, number>([["NODE_GONE", 409]]);
  for (const verb of ["patch", "delete"]) {
    const description = route?.[verb]?.description;
    expect(description, `the contract describes no ${verb} of ${ROUTE}`).toBeString();
    for (const named of (description ?? "").matchAll(/([45]\d\d) ([A-Z_]+)/gu)) {
      found.set(named[2] ?? "", Number(named[1]));
    }
  }
  return [...found].map(([code, status]) => ({ code, status }));
}

// R8: every code of both routes' rows has its own words, and so has NODE_GONE.
test("each refusal code of both node routes and NODE_GONE has its own words", () => {
  const codes = refusalsOfTheRoutes();
  expect(codes.map((one) => one.code)).toEqual(
    expect.arrayContaining([
      "NODE_GONE",
      "NOT_FOUND",
      "PARENT_DOCUMENT_REMOVABLE",
      "DELETE_COUNT_MOVED",
    ])
  );
  const lines = new Map<string, string>();
  for (const { code, status } of codes) {
    const error = new ApiError(status, { code, error: "the server's own message" });
    const line = refusalLine(error);
    const words = WORDS[`refused_${code.toLowerCase()}`];
    expect(words, `${code} has no words in ANSWER_LINES`).toBeString();
    expect(line.text, code).toContain(words ?? "");
    expect(line.text, `${code} draws the fallback line`).not.toContain(ANSWER_LINES.not_made);
    expect(line.retry, `${code} is no line that tries again`).toBe(false);
    lines.set(code, line.text);
  }
  // Each its own: no two codes share a line.
  expect(new Set(lines.values()).size).toBe(codes.length);
  // What the page does after a refusal that says the page read a stale tree or a node that is
  // gone (R8).
  expect(refusalLine(new ApiError(412, { code: "PRECONDITION_FAILED", error: "x" }))).toMatchObject(
    {
      gone: false,
      reads: true,
    }
  );
  for (const [status, code] of [
    [409, "NODE_GONE"],
    [404, "NOT_FOUND"],
  ] as const) {
    expect(refusalLine(new ApiError(status, { code, error: "x" })), code).toMatchObject({
      gone: true,
      reads: true,
    });
  }
  expect(refusalLine(new ApiError(400, { code: "INVALID_POINTER", error: "x" }))).toMatchObject({
    gone: false,
    reads: false,
  });
});

test("any other code, a 5xx and no answer draw the line that the change was not made", () => {
  const path = "/api/v1/issues/CORE-1/nodes/x";
  for (const error of [
    new ApiError(418, { code: "TEAPOT", error: "short and stout" }),
    new ApiError(500, { code: "INTERNAL", error: "the server broke" }),
    new ApiError(503, { error: "the server is down" }),
    new NetworkError(path, new TypeError("Failed to fetch")),
    new RequestTimeoutError(path, 30_000),
  ]) {
    const line = refusalLine(error);
    expect(line.text, String(error)).toContain(ANSWER_LINES.not_made);
    expect(line.retry, "a press on the line tries the act again").toBe(true);
    expect(line.reads).toBe(false);
    if (error instanceof ApiError && error.code !== undefined) {
      expect(line.text).toContain(ANSWER_LINES.not_made_code);
      expect(line.text).toContain(error.code);
    }
    // No code of a refusal that has its own words reads as this one.
    for (const words of Object.entries(WORDS)
      .filter(([key]) => key.startsWith("refused_"))
      .map(([, value]) => value)) {
      expect(line.text).not.toContain(words);
    }
  }
});

test("a container refusal names the container with a link to its row where the held tree shows it, and else quotes the server", () => {
  const listParagraph = "<list-paragraph>";
  const container = converted.nodes?.find((node) => node.id === listParagraph)?.parent;
  expect(container?.id, "the fixture's list paragraph stands under the list").toBe(
    "<tree/bullet_list>"
  );
  for (const exchange of [
    refused.CONTAINER_WOULD_EMPTY,
    fixture.deleted.refused.CONTAINER_WOULD_EMPTY,
  ]) {
    const error = new ApiError(exchange.status, exchange.body);
    // The held tree shows the parent: its kind and its first line, and a link to its row.
    const named = refusalLine(error, { held: converted, node: listParagraph });
    expect(named.container).toBe("<tree/bullet_list>");
    expect(named.text).toContain(ANSWER_LINES.container_empties);
    expect(named.text).toContain(LABELS.container_list_item ?? "");
    expect(named.text).toContain(container?.first_line ?? "");
    expect(named.text, "the server's message is quoted only where no row is named").not.toContain(
      exchange.body.error
    );
    // The held tree does not show the node: the line quotes the server's message.
    const quoted = refusalLine(error);
    expect(quoted.container).toBeUndefined();
    expect(quoted.text).toContain(exchange.body.error);
    expect(quoted.text).toContain(WORDS.refused_container_would_empty ?? "");
    // A node the held tree holds under a parent that is no list item and no callout is quoted too.
    const elsewhere = refusalLine(error, { held: converted, node: "<statement-1>" });
    expect(elsewhere.container).toBeUndefined();
    expect(elsewhere.text).toContain(exchange.body.error);
  }
});

test("If-Match carries the quoted placement_seq", async () => {
  const body = moved.indent.request.body;
  const request = withIfMatch(
    writeRequest("patch", ROUTE, { id: STATEMENT, key: KEY }, body),
    "<seq:statement-3>"
  );
  // The header is the tag, quoted (RFC 9110 section 13.1.1), whatever the sequence is written as.
  expect(request.headers).toStrictEqual({ "If-Match": '"<seq:statement-3>"' });
  expect(withIfMatch(request, 7).headers).toStrictEqual({ "If-Match": '"7"' });
  expect(request.method).toBe("PATCH");
  expect(request.body).toBe(body);
  // On the wire the sender sets it: one header, the quoted tag.
  const seen: (string | null)[] = [];
  answerRequests(
    http.patch(`/api/v1/issues/${KEY}/nodes/:id`, ({ request: sent }) => {
      seen.push(sent.headers.get("If-Match"));
      return Response.json(moved.indent.body);
    })
  );
  await api.send(request, { key: newSendKey() });
  expect(seen).toEqual(['"<seq:statement-3>"']);
});

/** A kept entry of the write `key`, as a page of the release that kept it left it in the tab. */
function keptEntry(tab: KeptTab, key: string, request: WriteRequest): void {
  tab.storage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      detail: null,
      issue: KEY,
      key,
      kind: "write",
      label: "A move",
      login: "alice",
      presentation: "indicator",
      pressedAt: Date.now(),
      request,
      sent: true,
      state: { status: "sending" },
      target: null,
      version: 1,
    })
  );
}

test("a kept move keeps its If-Match across a reload", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const request = withIfMatch(
      writeRequest("patch", ROUTE, { id: STATEMENT, key: KEY }, moved.indent.request.body),
      "<seq:statement-3>"
    );
    const hang = () => Promise.withResolvers<never>().promise;
    const send = spyOn(api, "send").mockImplementation(hang);
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    try {
      void before
        .run({
          detail: null,
          issue: KEY,
          kind: plainWrite,
          label: "A move",
          presentation: "indicator",
          request,
        })
        .catch(() => {});
      await settle();
      const [key] = before.rows().map((write) => write.key);
      // The tab keeps the request with its header.
      expect(tab.kept(key ?? "")).toMatchObject({
        request: {
          body: moved.indent.request.body,
          headers: { "If-Match": '"<seq:statement-3>"' },
          method: "PATCH",
        },
      });
      // A page of the next load takes it up again, and sends the precondition it had.
      send.mockImplementation(async () => moved.indent.body as never);
      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(2);
      expect((send.mock.calls[1]?.[0] as WriteRequest).headers).toEqual({
        "If-Match": '"<seq:statement-3>"',
      });
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

test("a kept write of an older release with no headers loads", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    // The release before this part kept `{ body, method, path }` and no header.
    const old: WriteRequest = {
      body: moved.indent.request.body,
      method: "PATCH",
      path: `/api/v1/issues/${KEY}/nodes/%3Cstatement-3%3E`,
    };
    keptEntry(tab, newSendKey(), old);
    const send = spyOn(api, "send").mockResolvedValue(moved.indent.body as never);
    try {
      const page = new Writes(signedIn(), tab.page(), memoryFiles());
      page.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      const resent = send.mock.calls[0]?.[0] as WriteRequest;
      expect(resent).toMatchObject(old);
      expect(resent.headers).toBeUndefined();
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

afterEach(() => {
  document.body.replaceChildren();
});

/** A case runs on real timers, which the moves' answers settle on. */
beforeEach(() => {
  answerTreeReads();
});

test("a move sends one PATCH with the node's placement_seq", async () => {
  const patches = answerPatches(moved.indent);
  const { client, writes } = mount();
  expect<unknown>(heldNode(client, STATEMENT)?.placement_seq).toBe("<seq:statement-3>");
  let outcome: unknown;
  await act(async () => {
    outcome = await writes().move(STATEMENT, "indent");
  });
  expect(outcome).toMatchObject({ status: "taken" });
  expect(patches.calls).toEqual([
    {
      body: moved.indent.request.body,
      id: STATEMENT,
      ifMatch: moved.indent.request.if_match,
    },
  ]);
});

test("moves of one node wait for the answer before them and take its placement_seq", async () => {
  const gate = Promise.withResolvers<void>();
  const patches = answerPatches({ ...moved.indent, gate: gate.promise }, moved.outdent);
  const { writes } = mount();
  let both: Promise<unknown[]> | undefined;
  await act(async () => {
    both = Promise.all([writes().move(STATEMENT, "indent"), writes().move(STATEMENT, "outdent")]);
  });
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  // The second move waits: nothing more is sent while the first is out.
  await letRequestsSettle();
  expect(patches.calls).toHaveLength(1);
  await act(async () => {
    gate.resolve();
    await both;
  });
  expect(patches.calls).toHaveLength(2);
  // The outdent carries the placement_seq the indent answered, which no read gave the page.
  expect(patches.calls[0]?.ifMatch).toBe(moved.indent.request.if_match);
  expect(patches.calls[1]?.ifMatch).toBe(moved.outdent.request.if_match);
  expect(
    moved.indent.body.placement_seq,
    "the fixture's second move waits for the first's"
  ).not.toBe("<seq:statement-3>");
});

test("the next move takes the answer's placement_seq and place", async () => {
  const gate = Promise.withResolvers<void>();
  const patches = answerPatches(moved.indent, { ...moved.outdent, gate: gate.promise });
  // The re-read once the queue is empty (R7) is held until the case ends, so the fixture's read
  // from before the moves does not take the place of the answers the case reads.
  const reread = Promise.withResolvers<void>();
  answerRequests(
    http.get(`/api/v1/issues/${KEY}/tree`, async () => {
      await reread.promise;
      return Response.json(renamed(fixture.tree.converted.person.body));
    })
  );
  const { client, writes } = mount();
  try {
    let both: Promise<unknown[]> | undefined;
    await act(async () => {
      both = Promise.all([writes().move(STATEMENT, "indent"), writes().move(STATEMENT, "outdent")]);
    });
    await waitFor(() => expect(patches.calls).toHaveLength(2));
    // The first answer is written into the held read while the second move is out: the node
    // stands under the second statement, first among its children, with the placement_seq it
    // answered.
    const standing = heldNode(client, STATEMENT);
    expect(standing?.parent?.id).toBe(moved.indent.body.parent);
    expect<unknown>(standing?.placement_seq).toBe(moved.indent.body.placement_seq);
    // The second move's body was made from the read with the first applied: it leaves the second
    // statement, and stands after it (the old parent is a block of the node's own document).
    expect(patches.calls[1]?.body).toStrictEqual(moved.outdent.request.body);
    await act(async () => {
      gate.resolve();
      await both;
    });
    // And the second answer puts the node back where the read had it, with the newest sequence.
    expect(heldNode(client, STATEMENT)?.parent?.id).toBe("<criterion>");
    expect<unknown>(heldNode(client, STATEMENT)?.placement_seq).toBe(
      moved.outdent.body.placement_seq
    );
  } finally {
    reread.resolve();
  }
});

test("a refusal empties the node's queue and says how many moves were dropped", async () => {
  const patches = answerPatches(refused.INVALID_POINTER);
  const { writes } = mount();
  let outcomes: unknown[] = [];
  await act(async () => {
    outcomes = await Promise.all([
      writes().move(STATEMENT, "indent"),
      writes().move(STATEMENT, "outdent"),
      writes().move(STATEMENT, "move_up"),
    ]);
  });
  expect(patches.calls, "a move was sent after the refusal").toHaveLength(1);
  expect(outcomes[0]).toMatchObject({ status: "refused" });
  expect(outcomes.slice(1)).toMatchObject([{ status: "dropped" }, { status: "dropped" }]);
  const line = writes().lines.get(STATEMENT)?.text ?? "";
  expect(line).toContain(WORDS.refused_invalid_pointer ?? "");
  expect(line).toContain(`2 ${ANSWER_LINES.dropped_moves}`);
  // One move dropped reads in the singular.
  const gate = Promise.withResolvers<void>();
  answerPatches({ ...refused.INVALID_POINTER, gate: gate.promise });
  const again = mount();
  await act(async () => {
    const first = again.writes().move(STATEMENT, "indent");
    const second = again.writes().move(STATEMENT, "outdent");
    gate.resolve();
    await Promise.all([first, second]);
  });
  expect(again.writes().lines.get(STATEMENT)?.text).toContain(`1 ${ANSWER_LINES.dropped_move}`);
});

test("a queued move that turned grey is dropped with its reason", async () => {
  const patches = answerPatches(moved.indent);
  const { writes } = mount();
  let outcomes: unknown[] = [];
  await act(async () => {
    outcomes = await Promise.all([
      writes().move(STATEMENT, "indent"),
      writes().move(STATEMENT, "indent"),
      writes().move(STATEMENT, "outdent"),
    ]);
  });
  // The first indent puts the node first under the second statement: the second indent has no
  // sibling above it, so it is dropped with that reason, and the outdent after it with it.
  expect(patches.calls).toHaveLength(1);
  expect(outcomes[0]).toMatchObject({ status: "taken" });
  expect(outcomes[1]).toMatchObject({ reason: "grey_no_sibling_above", status: "dropped" });
  expect(outcomes[2]).toMatchObject({ reason: "grey_no_sibling_above", status: "dropped" });
  const line = writes().lines.get(STATEMENT)?.text ?? "";
  expect(line).toContain(ANSWER_LINES.grey_no_sibling_above);
  expect(line).toContain(`2 ${ANSWER_LINES.dropped_moves}`);
});

test("the page reads its outline reads once the queue is empty", async () => {
  // The held read is the first page and the second the page has loaded: the reads after the moves
  // are made from the first page as far as that page (R7).
  const pages = fixture.paged.pages;
  const reads = answerTreeReads(pages);
  const client = signedIn();
  await client.fetchQuery(outlineQuery(KEY, "graph"));
  await readNextTreePage(client, KEY, "graph");
  expect(reads.cursors, "the page held both pages before the moves").toHaveLength(2);
  reads.cursors.length = 0;
  const view = hookOver(client);
  const gate = Promise.withResolvers<void>();
  const patches = answerPatches(moved.indent, { ...moved.outdent, gate: gate.promise });
  let both: Promise<unknown[]> | undefined;
  await act(async () => {
    both = Promise.all([
      view.result.current.move(STATEMENT, "indent"),
      view.result.current.move(STATEMENT, "outdent"),
    ]);
  });
  await waitFor(() => expect(patches.calls).toHaveLength(2));
  // No read between the two moves.
  expect(reads.cursors).toEqual([]);
  await act(async () => {
    gate.resolve();
    await both;
  });
  // One read of the outline after the last answer: its first page and the page it had loaded.
  await waitFor(() => expect(reads.cursors).toHaveLength(2));
  expect(reads.cursors.filter((cursor) => cursor === null)).toHaveLength(1);
  expect(reads.cursors.filter((cursor) => cursor !== null)).toHaveLength(1);
  await letRequestsSettle();
  expect(reads.cursors, "the outline was read more than once").toHaveLength(2);
});

test("a 412 says the node moved and reads the tree again", async () => {
  const reads = answerTreeReads();
  const patches = answerPatches(refused.PRECONDITION_FAILED);
  const { writes } = mount();
  await act(async () => {
    await Promise.all([writes().move(STATEMENT, "indent"), writes().move(STATEMENT, "outdent")]);
  });
  expect(patches.calls, "the queue was not emptied").toHaveLength(1);
  const line = writes().lines.get(STATEMENT)?.text ?? "";
  expect(line).toContain(WORDS.refused_precondition_failed ?? "");
  expect(line).toContain(`1 ${ANSWER_LINES.dropped_move}`);
  await waitFor(() => expect(reads.cursors).toHaveLength(1));
});

test("a gone node's move reads the tree again", async () => {
  for (const exchange of [refused.NODE_GONE, refused.NOT_FOUND]) {
    const reads = answerTreeReads();
    answerPatches(exchange);
    const { writes, view } = mount();
    await act(async () => {
      await writes().move(STATEMENT, "indent");
    });
    const code = exchange.body.code.toLowerCase();
    expect(writes().lines.get(STATEMENT)?.text, code).toContain(WORDS[`refused_${code}`] ?? "");
    await waitFor(() => expect(reads.cursors, code).toHaveLength(1));
    view.unmount();
  }
});

test("a refused move keeps the tree and draws one line", async () => {
  const reads = answerTreeReads();
  answerPatches(refused.INVALID_POINTER);
  const { client, writes } = mount();
  const before = heldNode(client, STATEMENT);
  await act(async () => {
    await writes().move(STATEMENT, "indent");
  });
  // The tree is the server's: the node stands where the read had it, with the sequence it had.
  expect(heldNode(client, STATEMENT)).toEqual(before);
  expect(writes().lines.size).toBe(1);
  expect(writes().lines.get(STATEMENT)?.text).toContain(WORDS.refused_invalid_pointer ?? "");
  // And nothing is read for it: the refusal is no taken write and no stale tree.
  await letRequestsSettle();
  expect(reads.cursors).toEqual([]);
});
