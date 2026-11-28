import { afterEach, expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, waitFor } from "@testing-library/react";
import { http } from "msw";

import { answerRequests } from "../../../__tests__/network";
import { api } from "../../../api/client";
import { SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { PRESET_NAMES, PRESET_ORDER, type Preset } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import {
  answerSeededReads,
  columnOf,
  DECISION,
  type DrawnPage,
  drawPage,
  newClient,
  ON_STATEMENT,
  PLAN_DOCUMENT,
  PLAN_TASK,
  REPLY_ON_STATEMENT,
  rowOf,
  type SeededReads,
  STATEMENT,
  TEXT_THREADS,
} from "./margin-fixtures";

/**
 * R2: the column of comments on text stands in each of the six views of the page, beside the
 * words of the node a thread is on, and in no other view. A view the page adds later (Dashboard,
 * Threads and Documents, as their parts add them) is in neither list below until its part names
 * it, and the test fails until then.
 */

/** The margin's name for its column (`page-words-margin.ts`, R11). */
const COLUMN = "Comments on text";

/** The views whose stage mounts the column, and the views that mount none. */
const WITH_COLUMN: readonly Preset[] = [
  "conversation",
  "spec",
  "verification",
  "plan",
  "decisions",
  "graph",
];
const WITHOUT_COLUMN: readonly Preset[] = [];

/** The node each view draws that holds a seeded thread, by its tree id. */
const NODE_WITH_THREAD: Readonly<Record<Preset, string>> = {
  conversation: blockNodeId(SPEC_DOCUMENT.slug, STATEMENT),
  decisions: blockNodeId(SPEC_DOCUMENT.slug, DECISION),
  graph: blockNodeId(SPEC_DOCUMENT.slug, STATEMENT),
  plan: blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK),
  spec: blockNodeId(SPEC_DOCUMENT.slug, STATEMENT),
  verification: blockNodeId(SPEC_DOCUMENT.slug, STATEMENT),
};

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

test("every view of R2 mounts the column and no other view does", async () => {
  stubs = answerSeededReads({ threads: TEXT_THREADS });
  const unnamed = PRESET_ORDER.filter(
    (preset) => !WITH_COLUMN.includes(preset) && !WITHOUT_COLUMN.includes(preset)
  );
  expect(
    unnamed.map((preset) => PRESET_NAMES[preset]),
    "a view of the page is in neither list of this test: name it"
  ).toEqual([]);
  const lacking: string[] = [];
  const unexpected: string[] = [];
  for (const preset of PRESET_ORDER) {
    const client = newClient();
    const view = drawPage(client, preset);
    try {
      await waitFor(() =>
        expect(
          rowOf(view, NODE_WITH_THREAD[preset]),
          `${PRESET_NAMES[preset]} draws no row for the node that holds the seeded thread`
        ).not.toBeNull()
      );
      const column = await columnOf(view, COLUMN);
      if (WITH_COLUMN.includes(preset) && column === null) lacking.push(PRESET_NAMES[preset]);
      if (WITHOUT_COLUMN.includes(preset) && column !== null) {
        unexpected.push(PRESET_NAMES[preset]);
      }
    } finally {
      view.finish();
      client.clear();
    }
  }
  expect(lacking, "the views whose stage mounts no column with a seeded thread").toEqual([]);
  expect(unexpected, "the views that mount a column and are no view of R2").toEqual([]);
});

/** A spy of the API client's, whose calls are each a read the page made. */
function isSpy(value: unknown): value is { readonly mock: { readonly calls: unknown[][] } } {
  return typeof value === "function" && "mock" in value;
}

/** The route each spied read of the API client was made by: the method, and for the tree read the
 *  view it read, in the order of the method names. */
function spiedRoutes(): string[] {
  return Object.getOwnPropertyNames(api)
    .sort()
    .flatMap((name) => {
      const spy: unknown = Reflect.get(api, name);
      if (!isSpy(spy)) return [];
      return spy.mock.calls.map((args) => {
        const query = args[1];
        const view =
          name === "getIssueTree" && typeof query === "object" && query !== null && "view" in query
            ? ` view=${String(query.view)}`
            : "";
        return `${name}${view}`;
      });
    });
}

/**
 * Every request one load of Conversation sends, by its route, once the page has settled: the
 * reads of the API client the case answers (`answerSeededReads`) and the requests that reach the
 * network under them, which the unit suite's own answers take (`__tests__/network.ts`). The
 * client keeps a read fresh for 30 s, as the app's does (`main.tsx`), so a part drawn later in
 * the load reads nothing a part drawn first holds.
 */
async function loadRoutes(
  reads: SeededReads,
  settled: (view: DrawnPage) => Promise<unknown>
): Promise<string[]> {
  stubs = answerSeededReads(reads);
  const network: string[] = [];
  answerRequests(
    http.all("*", ({ request }) => {
      network.push(`${request.method} ${new URL(request.url).pathname}`);
    })
  );
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const view = drawPage(client, "conversation");
  try {
    await settled(view);
    await waitFor(() => expect(network).toContain("GET /api/v1/me/replies"));
    await act(async () => {});
    return [...spiedRoutes(), ...network].sort();
  } finally {
    view.finish();
    client.clear();
    for (const stub of stubs) stub.mockRestore();
    stubs = [];
  }
}

/** The routes of a load of the issue with the seeded thread on the statement, its reply in it,
 *  and of a load with the same comment and reply as a post with its reply under the issue, all
 *  alice's: both draw one thread with a reply, as rows or in the column, and each marks it read
 *  (`/api/v1/me/replies`). */
async function bothLoads(): Promise<{ readonly with: string[]; readonly without: string[] }> {
  const withThreads = await loadRoutes({ threads: [ON_STATEMENT] }, async (view) => {
    const column = await columnOf(view, COLUMN);
    expect(column, "the load with threads draws no column").not.toBeNull();
    await waitFor(() =>
      expect(column?.textContent, "the column draws no reply").toContain(REPLY_ON_STATEMENT.body)
    );
  });
  const withoutThreads = await loadRoutes(
    { posts: [{ ...ON_STATEMENT.comment, anchor: null }, REPLY_ON_STATEMENT], threads: [] },
    async (view) => {
      await waitFor(() =>
        expect(rowOf(view, REPLY_ON_STATEMENT.id), "the post's reply is no row").not.toBeNull()
      );
      expect(await columnOf(view, COLUMN, 300), "a load with no thread draws a column").toBeNull();
    }
  );
  return { with: withThreads, without: withoutThreads };
}

/** A read of one comment's or one message's own route, which a read for each thread would be. */
const PER_ITEM =
  /^getComment$|\/api\/v1\/comments\/[^/]+(\/blocks)?$|\/api\/v1\/messages\/[^/]+\/blocks$/;

// R9, c-reads: the column draws its threads from the reads the page sends with no thread, and
// sends no read for a node or a thread. Each card's reply composer is open, since the card is
// always expanded (R4), and reads no agents' list for its mention picker until the reader uses it
// (`ThreadCard`'s `fixedOpen`), so the load with threads reads no route the load without them does
// not, and the case under this one asks for the very same requests.
test("the margin adds no read for each node or each thread", async () => {
  const loads = await bothLoads();
  const only = (routes: readonly string[], others: readonly string[]) =>
    [...new Set(routes)].filter((route) => !others.includes(route));
  expect(
    { with: only(loads.with, loads.without), without: only(loads.without, loads.with) },
    "the loads differ by a route"
  ).toEqual({ with: [], without: [] });
  for (const [load, routes] of Object.entries(loads)) {
    expect(
      routes.filter((route) => PER_ITEM.test(route)),
      `the load ${load} threads reads a comment or a message by its id`
    ).toEqual([]);
    expect(
      routes.filter((route) => route === "listComments"),
      `the load ${load} threads sends no comments read, or more than one`
    ).toHaveLength(1);
  }
});

// R9 whole: the load with threads sends each request of the load without them, as many times, and
// no other.
test("the load with threads sends exactly the requests of the load without them", async () => {
  const loads = await bothLoads();
  expect(loads.with, "the load with threads sends other requests").toEqual(loads.without);
});
