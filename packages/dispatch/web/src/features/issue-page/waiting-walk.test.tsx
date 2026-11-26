import { afterEach, beforeAll, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, useQuery } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, type NavigateFunction, useLocation, useNavigate } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { type FakeLiveSockets, installFakeLiveSockets } from "../../__tests__/live-socket-harness";
import { answerRequests } from "../../__tests__/network";
import {
  artifactRecordHandler,
  decisionsAndReadMarkHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import { liveSocketFor } from "../../api/live-socket";
import { issueQuery, myRepliesKey } from "../../api/queries";
import type {
  Comment,
  GraphReferences,
  IssueAskRow,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
  IssueReferences,
  MyRepliesResponse,
} from "../../api/types";
import asksOpenBody from "../dashboard/fixtures/asks-open.json";
import changesBody from "../dashboard/fixtures/changes.json";
import coverageBody from "../dashboard/fixtures/coverage.json";
import decisionsBody from "../dashboard/fixtures/decisions.json";
import handoffsBody from "../dashboard/fixtures/handoffs.json";
import issueBody from "../dashboard/fixtures/issue.json";
import myRepliesBody from "../dashboard/fixtures/my-replies.json";
import { type WaitingItem, waitingItems } from "../dashboard/useWaitingItems";
import { DocumentRuntime } from "../doc/runtime";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { parseIssuePath } from "../refs/routes";
import { presetPath } from "./issue-address";
import { CONTROL_LABELS, DASHBOARD_WORDS } from "./page-words";
import { HeaderSlot } from "./slots/header";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, stubReads } from "./slots/stage-fixtures";

/**
 * The count of what waits on the reader (part N's R5, the plan's ST-3), one case per row: the
 * header's own count, pressed by the pointer or by Enter, over the page's own stage, whose
 * Dashboard draws the list the count is the length of. The reads are the captured answers of the
 * Dashboard seed (`dashboard/fixtures/README.md`), each case holding the six items of the browser
 * row's seed: a question, a decision to review, an approval request of the spec, a stale approval
 * of the Verification document and two unread threads.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const KEY = "ACME-1";
const READER = "alice";

interface Store {
  issue: IssueDetails;
  asks: IssueAskRow[];
  decisions: IssueDecisions;
  changes: Comment[];
  coverage: IssueCoverage;
  replies: MyRepliesResponse;
}

function fixtures(): Store {
  return structuredClone({
    asks: asksOpenBody as unknown as IssueAskRow[],
    changes: changesBody as unknown as Comment[],
    coverage: coverageBody as unknown as IssueCoverage,
    decisions: decisionsBody as unknown as IssueDecisions,
    issue: issueBody as unknown as IssueDetails,
    replies: myRepliesBody as unknown as MyRepliesResponse,
  });
}

/** The items R4's rule makes of what `store` holds, newest first: the list Dashboard draws. */
function itemsOf(store: Store): readonly WaitingItem[] {
  return waitingItems({
    asks: store.asks,
    changes: store.changes,
    coverage: store.coverage,
    decisions: store.decisions,
    issue: store.issue,
    replies: store.replies,
    viewer: READER,
  });
}

/** Each item's key, as the list draws it under its test id. */
function keysOf(store: Store): string[] {
  return itemsOf(store).map((item) => `${item.kind}-${item.id}`);
}

/** The captured reads with one question, one decision to review, the approval request of the
 *  spec, the stale approval of the Verification document and the two threads of the people the
 *  reader waits on: six items. */
function six(): Store {
  const store = fixtures();
  const mine = (ask: IssueAskRow) =>
    ask.kind === "question" && ask.waiting_on === "human" && ask.waiting_on_person === READER;
  const kept = store.asks.find(mine);
  store.asks = store.asks.filter((ask) => !mine(ask) || ask.id === kept?.id);
  const reviewed = itemsOf(store).flatMap((item) =>
    item.kind === "decision" ? [item.decision.block_id] : []
  );
  store.decisions = {
    ...store.decisions,
    decisions: (store.decisions.decisions ?? []).filter(
      (decision) => !reviewed.slice(1).includes(decision.block_id)
    ),
  };
  const kinds = itemsOf(store).map((item) => item.kind);
  expect(kinds.sort(), "the captured reads no longer hold the six items of the seed").toEqual([
    "approval",
    "decision",
    "question",
    "stale",
    "thread",
    "thread",
  ]);
  return store;
}

/** The captured reads with nothing that waits on the reader. */
function nothing(): Store {
  const store = fixtures();
  store.asks = [];
  store.decisions = { ...store.decisions, decisions: [] };
  store.replies = { ...store.replies, rows: [] };
  store.issue = {
    ...store.issue,
    artifacts: (store.issue.artifacts ?? []).map((artifact) => ({
      ...artifact,
      approval: undefined,
    })),
    open_asks: [],
  };
  return store;
}

const restoreReads: { mockRestore(): void }[] = [];
let sockets: FakeLiveSockets | undefined;
const clients: QueryClient[] = [];

beforeEach(() => {
  sockets = installFakeLiveSockets();
});

afterEach(() => {
  for (const read of restoreReads.splice(0)) read.mockRestore();
  for (const client of clients.splice(0)) {
    liveSocketFor(client).close();
    client.clear();
  }
  sockets?.restore();
});

/** The fake client: each read answers what `store` holds when it is made. */
function serve(store: Store): void {
  restoreReads.push(
    ...stubReads(),
    spyOn(api, "getIssue").mockImplementation(async () => store.issue),
    spyOn(api, "listIssueAsks").mockImplementation(async () => store.asks),
    spyOn(api, "getIssueDecisions").mockImplementation(async () => store.decisions),
    spyOn(api, "listComments").mockImplementation(async () => store.changes as never),
    spyOn(api, "getIssueCoverage").mockImplementation(async () => store.coverage),
    spyOn(api, "listMyReplies").mockImplementation(async () => store.replies),
    spyOn(api, "listHandoffs").mockResolvedValue(handoffsBody as never),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "The document",
      token: "sha256:document",
      version: null,
    }),
    spyOn(api, "getComment").mockImplementation(async () => {
      throw new Error("no thread read here");
    }),
    spyOn(api, "getMessage").mockImplementation(async () => {
      throw new Error("no thread read here");
    })
  );
  answerRequests(
    artifactRecordHandler((id) => store.issue.artifacts?.find((each) => each.id === id)),
    http.get("/api/v1/artifacts/:id/blocks", () => Response.json([])),
    http.get("/api/v1/issues/:key/references", () =>
      Response.json({
        linked_from: [],
        links: [],
        members: [],
        truncated: false,
      } satisfies IssueReferences)
    ),
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: [],
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    ),
    ...decisionsAndReadMarkHandlers()
  );
}

/** The router's navigation, which a case moves the address with where no reader's act would. */
let navigateTo: NavigateFunction | undefined;

/** Where the router is, and the history entry it is in. */
function Where(): ReactNode {
  const { key, pathname, search } = useLocation();
  navigateTo = useNavigate();
  return <output data-entry={key} data-testid="where">{`${pathname}${search}`}</output>;
}

/** The header over the stage at the address the router holds, under the page's own providers, as
 *  the page's root draws them, for the issue its own read answers. */
function Page(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  const read = useQuery(issueQuery(route?.key));
  if (route === undefined) throw new Error(`${pathname} names no route`);
  if (read.data === undefined) return null;
  return (
    <ProvidersSlot issue={read.data} route={route}>
      <HeaderSlot issue={read.data} route={route} />
      <StageSlot issue={read.data} route={route} />
      <Where />
    </ProvidersSlot>
  );
}

/** The page at `address` over `store`, with its query client. `configure` changes a read after
 *  the store is served and before the page asks it. */
function drawPage(
  address: string,
  store: Store,
  configure: () => void = () => {}
): RenderResult & { readonly client: QueryClient } {
  serve(store);
  configure();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  clients.push(client);
  const view = render(
    <MemoryRouter initialEntries={[address]}>
      <AppProviders client={client}>
        <DocumentRuntime.Provider value={fakeDocumentRuntime({ text: "The document" }).runtime}>
          <Page />
        </DocumentRuntime.Provider>
      </AppProviders>
    </MemoryRouter>
  );
  return { ...view, client };
}

const BARE = `/issues/${KEY}`;
const DASHBOARD = presetPath(KEY, "dashboard");
const CONVERSATION = presetPath(KEY, "conversation");
const DECISIONS = presetPath(KEY, "decisions");
const VERIFICATION_DOCUMENT = `/issues/${KEY}/artifacts/verification-md`;

/** The address the router is at. */
function address(): string {
  return screen.getByTestId("where").textContent ?? "";
}

/** The history entry the router is in: a press that pushes an address changes it, and one that
 *  only moves the focus leaves it. */
function entry(): string | undefined {
  return screen.getByTestId("where").dataset.entry;
}

/** The header's count of what waits on the reader, once it is drawn. */
function count(): Promise<HTMLElement> {
  return screen.findByRole("button", { name: /^Waiting on you \(/ });
}

/** Presses the count as the reader does: by the pointer, or by Enter with the focus on it. */
async function pressCount(user: UserEvent, by: "pointer" | "Enter"): Promise<void> {
  const control = await count();
  if (by === "pointer") {
    await user.click(control);
    return;
  }
  await act(async () => control.focus());
  await user.keyboard("{Enter}");
}

/** The test ids of the items Dashboard draws, in the order it draws them. */
function drawnItems(): string[] {
  return [...document.querySelectorAll('[data-testid^="dashboard-item-"]')].map(
    (item) => item.getAttribute("data-testid") ?? ""
  );
}

function itemId(key: string): string {
  return `dashboard-item-${key}`;
}

/** The test id of the element that holds the focus, if it has one. */
function focusedTestId(): string {
  return document.activeElement?.getAttribute("data-testid") ?? "";
}

/** Waits for the focus to rest on the item `key`. */
async function focusOn(key: string, what: string): Promise<void> {
  await waitFor(() => expect(focusedTestId(), what).toBe(itemId(key)));
}

/** The header's count and, once Dashboard draws it, the list's own are `length`. */
async function expectCounts(length: number, { list = true }: { list?: boolean } = {}) {
  await waitFor(() => {
    expect(screen.getByTestId("issue-whose-turn").textContent).toBe(`Waiting on you (${length})`);
    if (list) {
      expect(screen.getByTestId("dashboard-waiting-count").textContent).toBe(String(length));
    }
  });
}

/** Presses the count `times` times, each press by `by`, and waits for the focus on each item of
 *  `keys` in turn, round to the first again. */
async function walk(
  user: UserEvent,
  keys: readonly string[],
  times: number,
  by: "pointer" | "Enter" = "pointer"
): Promise<void> {
  for (let press = 0; press < times; press += 1) {
    await pressCount(user, by);
    await focusOn(keys[press % keys.length] ?? "", `press ${press + 1} of the count`);
  }
}

/** The control that draws the next five, which holds how many are left. */
function nextFive(): HTMLElement | null {
  return screen.queryByRole("button", {
    name: (name) => name.startsWith(DASHBOARD_WORDS.next_five),
  });
}

async function refresh(client: QueryClient, queryKey: readonly unknown[]): Promise<void> {
  await act(() => client.invalidateQueries({ queryKey }));
}

/** The server's answer to `item` being answered, which the event refreshes its read by. */
async function answered(client: QueryClient, store: Store, item: WaitingItem): Promise<void> {
  switch (item.kind) {
    case "question":
    case "approval":
      store.asks = store.asks.filter((ask) => ask.id !== item.id);
      return refresh(client, ["asks", KEY]);
    case "decision":
      store.decisions = {
        ...store.decisions,
        decisions: (store.decisions.decisions ?? []).filter(
          (decision) => decision.block_id !== item.id
        ),
      };
      return refresh(client, ["decisions", KEY]);
    case "thread":
      store.replies = {
        ...store.replies,
        rows: (store.replies.rows ?? []).filter((row) => row.thread_id !== item.id),
      };
      return refresh(client, myRepliesKey);
    case "stale":
      store.issue = {
        ...store.issue,
        artifacts: (store.issue.artifacts ?? []).map((artifact) =>
          artifact.id === item.id && artifact.approval !== undefined
            ? {
                ...artifact,
                approval: {
                  ...artifact.approval,
                  state: "approved",
                  version: artifact.approval.latest_version,
                },
              }
            : artifact
        ),
      };
      return refresh(client, ["issue", KEY]);
  }
}

test("any control of the row other than Dashboard drawn: the first press by the pointer pushes Dashboard's address, with the focus on the list's first item", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const view = drawPage(CONVERSATION, store);
  try {
    await expectCounts(keys.length, { list: false });
    const before = entry();
    await pressCount(user, "pointer");
    await waitFor(() => expect(address()).toBe(DASHBOARD));
    expect(entry(), "the press wrote no history entry").not.toBe(before);
    await focusOn(keys[0] ?? "", "the first press left the focus off the list's first item");
    expect(drawnItems(), "Dashboard did not draw the list's first five").toEqual(
      keys.slice(0, 5).map(itemId)
    );
    await expectCounts(keys.length);
    // The push is a link's: the browser's Back returns to the view the press left.
    act(() => {
      navigateTo?.(-1);
    });
    expect(address()).toBe(CONVERSATION);
  } finally {
    view.unmount();
  }
});

test("any control of the row other than Dashboard drawn: Enter on the count does what the pointer does", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const view = drawPage(DECISIONS, store);
  try {
    await expectCounts(keys.length, { list: false });
    await pressCount(user, "Enter");
    await waitFor(() => expect(address()).toBe(DASHBOARD));
    await focusOn(keys[0] ?? "", "Enter on the count left the focus off the list's first item");
    await expectCounts(keys.length);
  } finally {
    view.unmount();
  }
});

test("a document drawn: the first press closes it, opens Dashboard and puts the focus on the list's first item", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const view = drawPage(VERIFICATION_DOCUMENT, store);
  try {
    expect(
      await screen.findByRole("button", { name: CONTROL_LABELS.back_to_documents }),
      "the document did not open"
    ).toBeVisible();
    await expectCounts(keys.length, { list: false });
    await pressCount(user, "pointer");
    await waitFor(() => expect(address()).toBe(DASHBOARD));
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.back_to_documents }),
      "the press left the document open"
    ).toBeNull();
    await focusOn(keys[0] ?? "", "the press from the document left the focus off the first item");
  } finally {
    view.unmount();
  }
});

test("the focus on item k of the five shown, k < 5: the next press puts it on item k + 1", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const view = drawPage(CONVERSATION, store);
  try {
    await expectCounts(keys.length, { list: false });
    await walk(user, keys, 5, "pointer");
    expect(drawnItems(), "a press within the five drew more").toHaveLength(5);
    await expectCounts(keys.length);
  } finally {
    view.unmount();
  }
});

test("the focus on the fifth item, more items left: the next press shows the next five and puts the focus on the sixth", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const view = drawPage(CONVERSATION, store);
  try {
    await expectCounts(keys.length, { list: false });
    await walk(user, keys, 5, "Enter");
    expect(nextFive(), "the list drew no control for the next five").not.toBeNull();
    await pressCount(user, "Enter");
    await focusOn(keys[5] ?? "", "the press past the fifth left the focus off the sixth item");
    expect(drawnItems(), "the next five were not shown").toEqual(keys.map(itemId));
    expect(nextFive(), "the control for the next five stayed with none left").toBeNull();
    await expectCounts(keys.length);
  } finally {
    view.unmount();
  }
});

test("the focus on the last item: the next press puts it on the first item and the list shows its first five again", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const view = drawPage(CONVERSATION, store);
  try {
    await expectCounts(keys.length, { list: false });
    await walk(user, keys, 6);
    await pressCount(user, "pointer");
    await focusOn(keys[0] ?? "", "the press after the last item did not come round to the first");
    expect(drawnItems(), "the list did not show its first five again").toEqual(
      keys.slice(0, 5).map(itemId)
    );
    expect(nextFive(), "the control for the next five did not come back").not.toBeNull();
    // And on round again: the second round walks as the first did.
    await walk(user, [...keys.slice(1), keys[0] ?? ""], 6);
  } finally {
    view.unmount();
  }
});

test("an item answered between presses: the next press goes to the next item still in the list, by the list at the press", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const items = itemsOf(store);
  const view = drawPage(CONVERSATION, store);
  try {
    await expectCounts(keys.length, { list: false });
    await walk(user, keys, 2);
    // The item after the one the focus is on is answered: the next press skips it.
    await answered(view.client, store, items[2] as WaitingItem);
    await expectCounts(keys.length - 1);
    await pressCount(user, "pointer");
    await focusOn(keys[3] ?? "", "the press after an answered item did not go to the next one");
    // The item the focus is on is answered: the next press goes on after it, in the order it had.
    await answered(view.client, store, items[3] as WaitingItem);
    await expectCounts(keys.length - 2);
    await pressCount(user, "pointer");
    await focusOn(keys[4] ?? "", "the press after the answered focused item went back");
  } finally {
    view.unmount();
  }
});

test("Dashboard's list still loading: a press opens Dashboard, and the focus lands on the first item once the list holds it, once", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const asks = Promise.withResolvers<IssueAskRow[]>();
  const view = drawPage(CONVERSATION, store, () => {
    restoreReads.push(spyOn(api, "listIssueAsks").mockReturnValue(asks.promise));
  });
  try {
    const control = await count();
    await pressCount(user, "pointer");
    await waitFor(() => expect(address()).toBe(DASHBOARD));
    expect(drawnItems(), "Dashboard drew items before its reads answered").toEqual([]);
    expect(document.activeElement, "the press moved the focus before the list held an item").toBe(
      control
    );
    await act(async () => asks.resolve([...store.asks]));
    await focusOn(keys[0] ?? "", "the press did not land on the first item once the list held it");
    await expectCounts(keys.length);
    // It landed once: a read that comes again, the focus where the reader put it, moves nothing.
    await act(async () => control.focus());
    await refresh(view.client, ["asks", KEY]);
    await expectCounts(keys.length);
    expect(document.activeElement, "the landed press took the focus again").toBe(control);
  } finally {
    view.unmount();
  }
});

test("a press whose list is still loading does not land after the address has left Dashboard", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const asks = Promise.withResolvers<IssueAskRow[]>();
  const view = drawPage(CONVERSATION, store, () => {
    restoreReads.push(spyOn(api, "listIssueAsks").mockReturnValue(asks.promise));
  });
  try {
    await pressCount(user, "pointer");
    await waitFor(() => expect(address()).toBe(DASHBOARD));
    // Back and Forward are no act of the page the reader's own events could end the press at.
    act(() => {
      navigateTo?.(-1);
    });
    expect(address()).toBe(CONVERSATION);
    act(() => {
      navigateTo?.(1);
    });
    expect(address()).toBe(DASHBOARD);
    await act(async () => asks.resolve([...store.asks]));
    await waitFor(() => expect(drawnItems()).toEqual(keys.slice(0, 5).map(itemId)));
    expect(focusedTestId(), "a press the address left landed on return").not.toMatch(
      /^dashboard-item-/
    );
  } finally {
    view.unmount();
  }
});

test("a press whose list is still loading does not land after the reader's next act", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const asks = Promise.withResolvers<IssueAskRow[]>();
  const view = drawPage(CONVERSATION, store, () => {
    restoreReads.push(spyOn(api, "listIssueAsks").mockReturnValue(asks.promise));
  });
  try {
    await pressCount(user, "pointer");
    await waitFor(() => expect(address()).toBe(DASHBOARD));
    await user.keyboard("{Shift}");
    await act(async () => asks.resolve([...store.asks]));
    await waitFor(() => expect(drawnItems()).toEqual(keys.slice(0, 5).map(itemId)));
    expect(focusedTestId(), "the press landed after the reader's own key").not.toMatch(
      /^dashboard-item-/
    );
  } finally {
    view.unmount();
  }
});

test("Dashboard drawn, at its own address or the bare one: a press moves the focus and writes no history entry", async () => {
  for (const path of [BARE, DASHBOARD]) {
    const user = userEvent.setup();
    const store = six();
    const keys = keysOf(store);
    const view = drawPage(path, store);
    try {
      await expectCounts(keys.length);
      const before = entry();
      await walk(user, keys, 2);
      expect(address(), `a press at ${path} moved the address`).toBe(path);
      expect(entry(), `a press at ${path} wrote a history entry`).toBe(before);
    } finally {
      view.unmount();
    }
  }
});

test("nothing waits: the count reads none and is no button", async () => {
  const store = nothing();
  const view = drawPage(BARE, store);
  try {
    expect((await screen.findByTestId("dashboard-waiting-count")).textContent).toBe("0");
    expect(screen.queryByTestId("issue-whose-turn")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Waiting on you/ })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("the header's count is the length of Dashboard's list at every state", async () => {
  const user = userEvent.setup();
  const store = six();
  const keys = keysOf(store);
  const items = itemsOf(store);
  const view = drawPage(CONVERSATION, store);
  try {
    await expectCounts(keys.length, { list: false });
    await walk(user, keys, 6);
    await expectCounts(keys.length);
    await answered(view.client, store, items[0] as WaitingItem);
    await expectCounts(keys.length - 1);
    await answered(view.client, store, items[5] as WaitingItem);
    await expectCounts(keys.length - 2);
  } finally {
    view.unmount();
  }
});
