import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { type QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { http } from "msw";
import { createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { jump, onFakeClock, settle } from "../../../__tests__/fake-clock";
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
import { eventRefreshTargets } from "../../../api/event-refresh";
import { refreshQueries } from "../../../api/query-refresh";
import type { DispatchEvent } from "../../../api/types";
import { newSendKey } from "../../../lib/send-key";
import { plainWrite, Writes } from "../../writes/write-store";
import { KEY } from "../decisions-filter/decisions-fixtures";
import { waitInAct } from "../outline-fixtures";
import { ANSWER_LINES, CONTROL_LABELS } from "../page-words";
import { outlineQuery, readNextTreePage, type TreeAnswer } from "../tree-read/tree-read";
import { fixture, renamed, signedIn } from "./graph-edit-fixtures";
import {
  drawNewRow,
  NEW_ROW_READ_MS,
  NEW_ROW_WAIT_MS,
  type NewRowAsk,
  newRowLine,
  subscribeRefusedPlacings,
  typeNewRow,
  useNewRows,
} from "./new-rows";
import { type NodeWrites, readOutlinesAgain, refusalLine, useNodeWrites } from "./node-writes";
import { placementOf } from "./placement";

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
 *   - `busy(node)`, whether an act of the node of that read id is out or waiting in its queue: from
 *     the press of its first act until its last act is taken, refused or dropped;
 *   - once a node's queue is empty after a taken answer, the hook reads each outline read the page
 *     holds again, its loaded pages included, once; after a 412, a `NODE_GONE` or a 404 it reads
 *     them once too; after any other refusal it reads nothing.
 * - `withIfMatch<T>(request: WriteRequest<T>, placementSeq: number | string): WriteRequest<T>` in
 *   `api/client.ts`: the request with `headers: { "If-Match": '"<placementSeq>"' }`, the one
 *   precondition header the page sends; `api.send` sets each header the request carries, and a
 *   kept request (`write-storage.ts`) keeps `headers` and gives it back.
 */

const ROUTE = "/api/v1/issues/{key}/nodes/{id}";

const converted = renamed(fixture.tree.converted.person.body) as unknown as TreeAnswer;
const moved = fixture.moved;
const refused = fixture.refused;

/** The statement every case moves: the third of the criterion, which the server's own moves of the
 *  fixture are the moves of. */
const STATEMENT = "<statement-3>";

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

/** The page's query client and router, over `client`, as a hook's wrapper. */
function pageWrapper(client: QueryClient) {
  return ({ children }: { children: ReactNode }) =>
    createElement(
      QueryClientProvider,
      { client },
      createElement(MemoryRouter, { initialEntries: [`/issues/${KEY}/graph`] }, children)
    );
}

/** The page's hook under the page's query client and router, over `client`. */
function hookOver(client: QueryClient) {
  return renderHook(() => useNodeWrites({ key: KEY, status: "in_progress" }), {
    wrapper: pageWrapper(client),
  });
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

/** The words of `ANSWER_LINES` by key, for a key a case writes out of a code. */
const WORDS: Readonly<Record<string, string>> = ANSWER_LINES;

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
  const path = `/api/v1/issues/${KEY}/nodes/x`;
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
  // The server reads each item of a list as a block of its own, so the paragraph stands under the
  // list's first item, whose block the seed names.
  const item = converted.nodes?.find((node) => node.block_id === "<list-item>")?.id;
  expect(item, "the fixture holds the list's first item").toBeDefined();
  expect(container?.id, "the fixture's list paragraph stands under its list item").toBe(item);
  for (const exchange of [
    refused.CONTAINER_WOULD_EMPTY,
    fixture.deleted.refused.CONTAINER_WOULD_EMPTY,
  ]) {
    const error = new ApiError(exchange.status, exchange.body);
    // The held tree shows the parent: its kind and its first line, and a link to its row.
    const named = refusalLine(error, { held: converted, node: listParagraph });
    expect(named.container).toBe(item);
    expect(named.text).toContain(ANSWER_LINES.container_empties);
    expect(named.text).toContain(CONTROL_LABELS.container_list_item);
    expect(named.text).toContain(container?.first_line ?? "");
    expect(named.text, "the server's message is quoted only where no row is named").not.toContain(
      exchange.body.error
    );
    // The held tree does not show the node: the line quotes the server's message.
    const quoted = refusalLine(error);
    expect(quoted.container).toBeUndefined();
    expect(quoted.text).toContain(exchange.body.error);
    expect(quoted.text).toContain(ANSWER_LINES.refused_container_would_empty);
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
    // An older release kept `{ body, method, path }` and no header.
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
  await waitInAct(50);
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
  expect(line).toContain(ANSWER_LINES.refused_invalid_pointer);
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

// R4: a node's chain is busy from the press of its first act until its last act ends, and a move
// queued behind one that is out is judged on that move's answer. The third statement is the last
// block of its document under the criterion, so Move down is grey on the read the page holds; behind
// Move up it goes all the same, after the second statement, under the placement_seq Move up
// answered.
test("a node's chain is busy while an act is out, and a move queued behind it is judged on its answer", async () => {
  const gate = Promise.withResolvers<void>();
  const patches = answerPatches({ ...moved.move_up, gate: gate.promise }, moved.move_down);
  const { writes } = mount();
  expect(
    placementOf({ held: converted, node: STATEMENT, status: "in_progress" }).move_down
  ).toEqual({ kind: "grey", reason: "grey_last" });
  expect(writes().busy(STATEMENT), "a node with no act is busy").toBe(false);
  let both: Promise<unknown[]> | undefined;
  await act(async () => {
    both = Promise.all([
      writes().move(STATEMENT, "move_up"),
      writes().move(STATEMENT, "move_down"),
    ]);
  });
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  expect(writes().busy(STATEMENT), "the chain is not busy while its move is out").toBe(true);
  expect(writes().busy("<statement-2>"), "another node's chain is busy").toBe(false);
  let outcomes: unknown[] = [];
  await act(async () => {
    gate.resolve();
    outcomes = (await both) ?? [];
  });
  expect(outcomes).toMatchObject([{ status: "taken" }, { status: "taken" }]);
  expect(patches.calls).toEqual([
    { body: moved.move_up.request.body, id: STATEMENT, ifMatch: '"<seq:statement-3>"' },
    {
      body: moved.move_down.request.body,
      id: STATEMENT,
      ifMatch: `"${moved.move_up.body.placement_seq}"`,
    },
  ]);
  expect(writes().busy(STATEMENT), "the chain is busy once its last act ended").toBe(false);
  expect(writes().lines.get(STATEMENT), "the row draws a line").toBeUndefined();
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
  await waitInAct(50);
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
  expect(line).toContain(ANSWER_LINES.refused_precondition_failed);
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
  expect(writes().lines.get(STATEMENT)?.text).toContain(ANSWER_LINES.refused_invalid_pointer);
  // And nothing is read for it: the refusal is no taken write and no stale tree.
  await waitInAct(50);
  expect(reads.cursors).toEqual([]);
});

// ---------------------------------------------------------------------------------------------
// A new row (R3, its logic): the wait for its node and the one `PATCH` that places it
// (`new-rows.ts`), over the fixture's issue whose tree artifact owes its index - `owed`, the read
// before the block index has read the room's write, which names the tree artifact among
// `owed_documents` and holds no node of the block, and `indexed`, the read after, which holds the
// paragraph the page wrote with a rank and a `placement_seq`, at the end of the issue's own blocks
// under the issue, where the server keeps it.
// ---------------------------------------------------------------------------------------------

const OWED_ISSUE = "<owed-issue>";
const owedRead = renamed(fixture.owed.owed.body, OWED_ISSUE) as unknown as TreeAnswer;
const indexedRead = renamed(fixture.owed.indexed.body, OWED_ISSUE) as unknown as TreeAnswer;

/** The block the page wrote, by the block id it chose for it, which the fixture's server indexed
 *  the paragraph under. */
const WRITTEN = "<owed-paragraph>";

/** The new row New row below makes on the owed issue's criterion, a block of its spec: first among
 *  the issue's own blocks, right after the spec's, under the issue. */
function belowCriterion(): NewRowAsk {
  const from = "<owed-criterion>";
  const verdict = placementOf({ held: owedRead, node: from, status: "in_progress" }).new_below;
  if (verdict.kind !== "new") throw new Error("New row below is not offered on the owed criterion");
  return {
    blockId: WRITTEN,
    entry: "new_below",
    from,
    parent: KEY,
    place: verdict.place,
    under: KEY,
  };
}

/** The server's answer to the one placing `PATCH` of `ask`: the node where its body puts it, under
 *  the next placement_seq, `placementSeq`, as a move's answer is (`fixture.moved`). */
function placedAnswer(ask: NewRowAsk, placementSeq = "<seq:owed-paragraph>@2"): Exchange {
  return { body: { ...ask.place.body, placement_seq: placementSeq }, status: 200 };
}

// The reads a case holds, each put back after the case.
const spies: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/** Answers each read of Graph's tree with `answer`, or with none where none is given, and counts
 *  them. */
function answerOutline(answer?: TreeAnswer): { readonly count: () => number } {
  let count = 0;
  const never = Promise.withResolvers<TreeAnswer>().promise;
  spies.push(
    spyOn(api, "getIssueTree").mockImplementation(() => {
      count += 1;
      return answer === undefined ? never : Promise.resolve(answer);
    })
  );
  return { count: () => count };
}

/** The page's new rows of the issue and its node writes, under a client that holds Graph's read
 *  `held`, observed as Graph observes it, so a refresh of it reads it. */
function mountNewRows(held: TreeAnswer = owedRead) {
  const client = signedIn();
  client.setQueryData(outlineQuery(KEY, "graph").queryKey, held);
  const view = renderHook(
    () => {
      useQuery(outlineQuery(KEY, "graph"));
      return { rows: useNewRows(KEY), writes: useNodeWrites({ key: KEY, status: "in_progress" }) };
    },
    { wrapper: pageWrapper(client) }
  );
  return { client, view };
}

/** Draws the new row `ask` and has its first character written in the room. */
function typed(client: QueryClient, ask: NewRowAsk): void {
  act(() => {
    drawNewRow(client, KEY, ask);
    typeNewRow(client, KEY, ask.blockId, "A");
  });
}

/** The version the room's write publishes for the tree artifact, as the live feed delivers it. */
function treeVersion(): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-01-01T00:00:00Z",
    id: 2,
    issue_key: KEY,
    notify: true,
    payload: { artifact_id: owedRead.tree, name: "tree", version: 2 },
    project: "CORE",
    seq: 2,
    type: "artifact.version",
  } as unknown as DispatchEvent;
}

// The tree is read again on the tree artifact's version event by the page's event refresh, to
// which the new rows add no read of their own; the first character reads nothing.
test("the tree artifact's version reads the tree again", async () => {
  const reads = answerOutline(indexedRead);
  const ask = belowCriterion();
  const patches = answerPatches(placedAnswer(ask));
  const { client } = mountNewRows();
  typed(client, ask);
  await waitInAct(50);
  expect(reads.count(), "the first character read the tree").toBe(0);
  // The feed runs each event's refresh as this does (`api/event-feed.ts`).
  act(() => refreshQueries(client, eventRefreshTargets(treeVersion())));
  await waitFor(() => expect(reads.count(), "the version read no tree").toBe(1));
  // That read holds the node with a rank and a placement_seq, which it places.
  await waitFor(() => expect(patches.calls).toHaveLength(1));
});

// While the read Graph holds names the tree artifact among `owed_documents`, one read goes each 30
// s of the page's clock from the first character, and none at the 2 minutes or past them.
test("an owed fragment is read again every 30 s for at most 2 minutes", async () => {
  await onFakeClock(async () => {
    const reads = answerOutline(owedRead);
    const send = spyOn(api, "send");
    spies.push(send);
    expect(owedRead.owed_documents?.some((owed) => owed.artifact_id === owedRead.tree)).toBe(true);
    const { client } = mountNewRows();
    typed(client, belowCriterion());
    await settle();
    expect(reads.count(), "the first character read the tree").toBe(0);
    for (const period of [1, 2, 3]) {
      await jump(NEW_ROW_READ_MS);
      expect(reads.count(), `the read of the period ending at ${period * 30} s`).toBe(period);
    }
    for (const period of [4, 5, 6]) {
      await jump(NEW_ROW_READ_MS);
      expect(reads.count(), `a read in the period ending at ${period * 30} s`).toBe(3);
    }
    expect(send, "a write went for a node no read holds").not.toHaveBeenCalled();
  });
});

// A read that lacks the node sends nothing; the first that holds it with a rank and a
// placement_seq sends one PATCH to the row's place, under that placement_seq, through the node's
// chain: its answer is written into the held read, the row leaves the store, and the outline is
// read once (R7). A read after it sends no second PATCH.
test("the read that holds the node sends the one placing PATCH", async () => {
  const ask = belowCriterion();
  const patches = answerPatches(placedAnswer(ask));
  const reads = answerOutline();
  const { client, view } = mountNewRows();
  const { queryKey } = outlineQuery(KEY, "graph");
  typed(client, ask);
  act(() => client.setQueryData(queryKey, owedRead));
  await waitInAct(50);
  expect(patches.calls, "a read that lacks the node sent a PATCH").toEqual([]);
  const written = indexedRead.nodes?.find((node) => node.id === WRITTEN);
  expect(written?.rank, "the fixture's indexed read holds the node with no rank").toBeString();
  act(() => client.setQueryData(queryKey, indexedRead));
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  expect(patches.calls[0]).toEqual({
    body: ask.place.body,
    id: WRITTEN,
    ifMatch: `"${String(written?.placement_seq)}"`,
  });
  await waitFor(() => expect(view.result.current.rows).toEqual([]));
  expect<unknown>(heldNode(client, WRITTEN)?.placement_seq).toBe("<seq:owed-paragraph>@2");
  expect(heldNode(client, WRITTEN)?.parent?.id).toBe(KEY);
  await waitFor(() => expect(reads.count(), "the outline was not read after the PATCH").toBe(1));
  act(() => client.setQueryData(queryKey, indexedRead));
  await waitInAct(50);
  expect(patches.calls, "a later read sent a second PATCH").toHaveLength(1);
  expect(reads.count(), "the outline was read more than once").toBe(1);
});

// Past 2 minutes of the page's clock from the first character, the row says that its block is
// saved and not yet placed, and no read and no PATCH goes for it, then or later, though a read
// that holds the node comes.
test("past 2 minutes the row says the block is saved and not yet placed", async () => {
  await onFakeClock(async () => {
    const reads = answerOutline(owedRead);
    const send = spyOn(api, "send");
    spies.push(send);
    const { client, view } = mountNewRows();
    typed(client, belowCriterion());
    await settle();
    // `jump` moves the clock 1 ms past its span: this stands 1 ms before the 2 minutes, and the
    // jump after it at them.
    await jump(NEW_ROW_WAIT_MS - 2);
    expect(view.result.current.rows[0]?.stage, "the row is waited out before 2 minutes").toBe(
      "typed"
    );
    await jump(0);
    expect(view.result.current.rows[0]?.stage).toBe("waited");
    expect(newRowLine(view.result.current.rows, WRITTEN)).toBe(ANSWER_LINES.saved_not_placed);
    const atTwoMinutes = reads.count();
    act(() => client.setQueryData(outlineQuery(KEY, "graph").queryKey, indexedRead));
    await jump(NEW_ROW_READ_MS * 2);
    expect(send, "a PATCH went past the 2 minutes").not.toHaveBeenCalled();
    expect(reads.count(), "a read went past the 2 minutes").toBe(atTwoMinutes);
    expect(newRowLine(view.result.current.rows, WRITTEN)).toBe(ANSWER_LINES.saved_not_placed);
  });
});

// A refusal of the placing PATCH leaves the node where the server keeps it, under the issue with
// its placement_seq, with the refusal's line on its row: the row leaves the store, so the tree
// draws the node there, and the outline hears of it, which keeps the focus on it.
test("a refused placing PATCH keeps the node where the server holds it", async () => {
  const patches = answerPatches(refused.INVALID_POINTER);
  answerOutline();
  const { client, view } = mountNewRows();
  const heard: string[] = [];
  const stop = subscribeRefusedPlacings(client, KEY, (node, blockId) =>
    heard.push(`${node} ${blockId}`)
  );
  try {
    typed(client, belowCriterion());
    act(() => client.setQueryData(outlineQuery(KEY, "graph").queryKey, indexedRead));
    await waitFor(() => expect(patches.calls).toHaveLength(1));
    await waitFor(() => expect(view.result.current.rows).toEqual([]));
    expect(heldNode(client, WRITTEN)).toEqual(
      indexedRead.nodes?.find((node) => node.id === WRITTEN)
    );
    expect(view.result.current.writes.lines.get(WRITTEN)?.text).toContain(
      ANSWER_LINES.refused_invalid_pointer
    );
    expect(heard).toEqual([`${WRITTEN} ${WRITTEN}`]);
  } finally {
    stop();
  }
});

// ---------------------------------------------------------------------------------------------
// A new row on a tree read of several pages (R3, R15): until its PATCH the server keeps the block
// at the end of the issue's own blocks, which the fixture's `paged` read holds on its second page,
// and a read made again reads only the pages the page has loaded. The paragraph of the issue's own
// fragment under the issue that only that page holds, with a rank and a placement_seq, stands for
// the new row's block.
// ---------------------------------------------------------------------------------------------

const firstPage = renamed(fixture.paged.pages[0]?.body) as unknown as TreeAnswer;
const secondPage = renamed(fixture.paged.pages[1]?.body) as unknown as TreeAnswer;

/** The block the page wrote, by the block id it chose for it: the paragraph the second page
 *  holds. */
const PAGED = "<tree/paragraph>";

/** New row under the first statement, a row of the first page whose subtree stands whole there,
 *  made on the first page alone. */
function underFirstStatement(): NewRowAsk {
  const from = "<statement-1>";
  const verdict = placementOf({ held: firstPage, node: from, status: "in_progress" }).new_under;
  if (verdict.kind !== "new")
    throw new Error("New row under is not offered on the first statement");
  return {
    blockId: PAGED,
    entry: "new_under",
    from,
    parent: from,
    place: verdict.place,
    under: from,
  };
}

/** The paged cases' new row under the first statement (`underFirstStatement`), the gate `out`
 *  that holds its placing PATCH's answer until the case resolves it, and the PATCHes sent, each
 *  answered with the paragraph's next placement_seq. */
function pagedPlacing(): {
  readonly ask: NewRowAsk;
  readonly out: { readonly resolve: () => void };
  readonly patches: { readonly calls: Patch[] };
} {
  const ask = underFirstStatement();
  const out = Promise.withResolvers<void>();
  const patches = answerPatches({
    ...placedAnswer(ask, "<seq:tree/paragraph>@2"),
    gate: out.promise,
  });
  return { ask, out, patches };
}

// A typed row whose node only the second page holds, on a first page that owes the tree artifact
// nothing: the page reads the next page once, and that page, merged, holds the node with a rank
// and a placement_seq, so the one placing PATCH goes with the row's body under that placement_seq.
test("a new row's node the first page lacks is read for on the next page and placed", async () => {
  const reads = answerTreeReads(fixture.paged.pages);
  const { ask, out, patches } = pagedPlacing();
  expect(firstPage.owed_documents ?? [], "the first page owes an index").toEqual([]);
  expect(
    firstPage.nodes?.some((node) => node.block_id === PAGED),
    "the first page holds it"
  ).toBe(false);
  const written = secondPage.nodes?.find((node) => node.id === PAGED);
  expect(written?.rank, "the second page holds the node with no rank").toBeString();
  const { client, view } = mountNewRows(firstPage);
  typed(client, ask);
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  expect(reads.cursors, "the reads before the PATCH").toEqual([firstPage.cursor]);
  expect(patches.calls[0]).toEqual({
    body: ask.place.body,
    id: PAGED,
    ifMatch: `"${String(written?.placement_seq)}"`,
  });
  await act(async () => {
    out.resolve();
  });
  await waitFor(() => expect(view.result.current.rows).toEqual([]));
  await waitInAct(50);
  expect(patches.calls, "a later read sent a second PATCH").toHaveLength(1);
});

// While the read Graph holds names the tree artifact among `owed_documents`, the block is not
// indexed yet: no next page is read for its node, however often that read is held again. The
// first read that owes it no more has the next page read, and the node placed.
test("no next page is read for a new row's node while the read owes the tree artifact's index", async () => {
  const reads = answerTreeReads(fixture.paged.pages);
  const { ask, out, patches } = pagedPlacing();
  const { tree } = firstPage;
  if (tree === null) throw new Error("the paged read names no tree artifact");
  const owing: TreeAnswer = { ...firstPage, owed_documents: [{ artifact_id: tree, version: 1 }] };
  const { queryKey } = outlineQuery(KEY, "graph");
  const { client } = mountNewRows(owing);
  typed(client, ask);
  await waitInAct(50);
  act(() => client.setQueryData(queryKey, { ...owing }));
  await waitInAct(50);
  expect(reads.cursors, "a page was read while the read owes the tree's index").toEqual([]);
  expect(patches.calls).toEqual([]);
  act(() => client.setQueryData(queryKey, firstPage));
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  expect(reads.cursors, "the reads before the PATCH").toEqual([firstPage.cursor]);
  await act(async () => {
    out.resolve();
  });
});

// Three pages: when the second page still lacks the node and names a third, the page reads page
// after page until the node is held, one page at a time, then sends the one placing PATCH.
test("a new row's node past the second page is read for page by page and placed", async () => {
  const middleCursor = "<cursor:page-2>";
  const thirdCursor = firstPage.cursor;
  const page1: TreeAnswer = { ...firstPage, cursor: middleCursor };
  const page2: TreeAnswer = {
    ...secondPage,
    cursor: thirdCursor,
    nodes: [],
    remaining: secondPage.nodes?.length ?? 0,
    tuples: [],
  };
  const cursors: (string | null)[] = [];
  answerRequests(
    http.get(`/api/v1/issues/${KEY}/tree`, ({ request }) => {
      const cursor = new URL(request.url).searchParams.get("cursor");
      cursors.push(cursor);
      if (cursor === middleCursor) return Response.json(page2);
      if (cursor === thirdCursor) return Response.json(secondPage);
      return Response.json(page1);
    })
  );
  const { ask, out, patches } = pagedPlacing();
  const { client } = mountNewRows(page1);
  typed(client, ask);
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  expect(cursors, "the reads before the PATCH").toEqual([middleCursor, thirdCursor]);
  await act(async () => {
    out.resolve();
  });
});

// A refused next page leaves the row waiting with no retry loop of its own; the next update of
// Graph's held read within the 2 minutes tries the page again.
test("a refused next page does not retry on its own and leaves the row waiting", async () => {
  const cursors: (string | null)[] = [];
  let refusedOnce = false;
  answerRequests(
    http.get(`/api/v1/issues/${KEY}/tree`, ({ request }) => {
      const cursor = new URL(request.url).searchParams.get("cursor");
      cursors.push(cursor);
      if (cursor !== null && !refusedOnce) {
        refusedOnce = true;
        return Response.json({ code: "UNAVAILABLE", error: "unavailable" }, { status: 503 });
      }
      return Response.json(cursor === null ? firstPage : secondPage);
    })
  );
  const { ask, out, patches } = pagedPlacing();
  const { client, view } = mountNewRows(firstPage);
  typed(client, ask);
  await waitFor(() => expect(cursors).toEqual([firstPage.cursor]));
  await waitInAct(80);
  expect(cursors, "a refused page retried on its own").toEqual([firstPage.cursor]);
  expect(patches.calls).toEqual([]);
  expect(view.result.current.rows[0]?.stage).toBe("typed");
  act(() => client.setQueryData(outlineQuery(KEY, "graph").queryKey, { ...firstPage }));
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  expect(cursors).toEqual([firstPage.cursor, firstPage.cursor]);
  await act(async () => {
    out.resolve();
  });
});

// A read of Graph's tree out as the placing PATCH goes, made again from the first page before the
// page read the node's next page, reads that page again too: its first page answered before the
// move put the node under the first statement and its next page after the move took the node from
// the end of the issue's own blocks, it answers with the node on no page. The page gives that read
// up before the PATCH goes, its request aborted and the answer held before it kept, and owes it,
// so it never takes the place of the read the PATCH's answer was written into.
test("a tree read out as the placing PATCH goes is given up and never lands over its answer", async () => {
  const { ask, out, patches } = pagedPlacing();
  const never = Promise.withResolvers<TreeAnswer>().promise;
  const leftEnd: TreeAnswer = {
    ...secondPage,
    nodes: (secondPage.nodes ?? []).filter((node) => node.id !== PAGED),
  };
  // Whether the PATCH has moved the node: a next page read since lacks it.
  let moved = false;
  // The read out as the PATCH goes, held until the case lets it answer, and the signal it went
  // with. Every read from the first page after it is the one after the answer (R7), which never
  // answers here.
  const spanning = Promise.withResolvers<void>();
  const signals: AbortSignal[] = [];
  spies.push(
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}, init = {}) => {
      if (query.cursor !== undefined) return moved ? leftEnd : secondPage;
      if (init.signal === undefined || signals.length > 0) return never;
      signals.push(init.signal);
      await spanning.promise;
      return firstPage;
    })
  );
  const { queryKey } = outlineQuery(KEY, "graph");
  const { client } = mountNewRows(firstPage);
  act(() => readOutlinesAgain(client, KEY, ["graph"]));
  expect(signals, "Graph's read was not read again").toHaveLength(1);
  typed(client, ask);
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  const state = client.getQueryState(queryKey);
  const asPatchWent = { fetchStatus: state?.fetchStatus, isInvalidated: state?.isInvalidated };
  moved = true;
  await act(async () => {
    out.resolve();
  });
  await waitFor(() =>
    expect<unknown>(heldNode(client, PAGED)?.placement_seq, "the answer was not taken").toBe(
      "<seq:tree/paragraph>@2"
    )
  );
  await act(async () => {
    spanning.resolve();
  });
  await waitInAct(50);
  expect(heldNode(client, PAGED)?.parent?.id, "the held read lost the placed node").toBe(
    "<statement-1>"
  );
  expect<unknown>(heldNode(client, PAGED)?.placement_seq).toBe("<seq:tree/paragraph>@2");
  expect(signals[0]?.aborted, "the read out as the PATCH went was not given up").toBe(true);
  expect(asPatchWent, "the read given up is not owed").toEqual({
    fetchStatus: "idle",
    isInvalidated: true,
  });
});

// R4: a move's PATCH gives up Graph's read out as it goes, as a placing does: that read can read a
// page before the move and another after it, and its answer would land over the held read the
// move's answer was written into. The read given up is owed, and the move's answer stands.
test("a tree read out as a move's PATCH goes is given up and its answer stands", async () => {
  const out = Promise.withResolvers<void>();
  const patches = answerPatches({ ...moved.move_up, gate: out.promise });
  const signals: AbortSignal[] = [];
  spies.push(
    spyOn(api, "getIssueTree").mockImplementation(async (_key, _query = {}, init = {}) => {
      if (init.signal !== undefined) signals.push(init.signal);
      return Promise.withResolvers<TreeAnswer>().promise;
    })
  );
  const { queryKey } = outlineQuery(KEY, "graph");
  const { client, writes } = mount();
  act(() => readOutlinesAgain(client, KEY, ["graph"]));
  expect(signals, "Graph's read was not read again").toHaveLength(1);
  let taken: Promise<unknown> | undefined;
  await act(async () => {
    taken = writes().move(STATEMENT, "move_up");
  });
  await waitFor(() => expect(patches.calls).toHaveLength(1));
  const state = client.getQueryState(queryKey);
  expect(
    { fetchStatus: state?.fetchStatus, isInvalidated: state?.isInvalidated },
    "the read out as the move went is not given up and owed"
  ).toEqual({ fetchStatus: "idle", isInvalidated: true });
  expect(signals[0]?.aborted, "the read out as the move went was not aborted").toBe(true);
  await act(async () => {
    out.resolve();
    await taken;
  });
  expect<unknown>(heldNode(client, STATEMENT)?.placement_seq).toBe(
    moved.move_up.body.placement_seq
  );
});
