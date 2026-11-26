import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import { legionIssueQuery } from "../../api/legion-issue";
import { myRepliesKey } from "../../api/queries";
import type {
  Comment,
  IssueAskRow,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
  LegionIssueResponse,
  MyRepliesResponse,
} from "../../api/types";
import { anchorQuote, decisionsDocumentId } from "../decision-card/change-anchor";
import { NoTooltips } from "../issue-page/NoTooltips";
import { AppProviders } from "../issue-page/slots/stage-fixtures";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { PublishedReferenceNames } from "../refs/reference-names";
import asksOpenBody from "./fixtures/asks-open.json";
import changesBody from "./fixtures/changes.json";
import coverageBody from "./fixtures/coverage.json";
import decisionsBody from "./fixtures/decisions.json";
import issueBody from "./fixtures/issue.json";
import myRepliesBody from "./fixtures/my-replies.json";
import { PhaseTrack } from "./PhaseTrack";
import { useDashboardNames } from "./useDashboardNames";
import { useWaitingItems, waitingItems } from "./useWaitingItems";
import { WAITING_STEP, WaitingList } from "./WaitingList";

// The timing table of the plan (dispatch://INFRA-5702/spec#b-r-waiting, #b-r-in-place): what
// waits leaves the list and the count as each write lands and the event refreshes the read it
// changed, with no reload, and the header's count, read from the same hook beside the list, is
// the list's length on every render. One `QueryClient`, and a fake client over a store each row
// changes as the server would.

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

/** A store of `count` questions that wait on the reader and nothing else, newest first by id. */
function questions(count: number): Store {
  const store = fixtures();
  const base = store.asks.find(
    (ask) =>
      ask.kind === "question" &&
      ask.waiting_on === "human" &&
      ask.waiting_on_person === READER &&
      (ask.options ?? []).length === 0
  );
  if (base === undefined) throw new Error("the asks fixture holds no question for the reader");
  store.asks = Array.from({ length: count }, (_, index) => ({
    ...base,
    asked_of_at: null,
    created_at: `2026-11-25T10:${String(index).padStart(2, "0")}:00Z`,
    edited_at: null,
    id: `q-${index}`,
    last_reply: null,
  })) as IssueAskRow[];
  store.decisions = { ...store.decisions, decisions: [] };
  store.replies = { ...store.replies, rows: [] };
  store.issue = {
    ...store.issue,
    artifacts: (store.issue.artifacts ?? []).map((artifact) => ({
      ...artifact,
      approval: undefined,
    })),
  };
  return store;
}

/** The items R4's rule makes of what `store` holds, as the server would answer it now. */
function expected(store: Store) {
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

const spies: { mockRestore: () => void }[] = [];
afterEach(() => {
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/** The fake client: each read answers what `store` holds when it is made. */
function serve(store: Store): void {
  spies.push(
    spyOn(api, "whoAmI").mockImplementation(async () => ({ login: READER }) as never),
    spyOn(api, "getIssue").mockImplementation(async () => store.issue),
    spyOn(api, "listIssueAsks").mockImplementation(async () => store.asks),
    spyOn(api, "getIssueDecisions").mockImplementation(async () => store.decisions),
    spyOn(api, "listComments").mockImplementation(async () => store.changes as never),
    spyOn(api, "getIssueCoverage").mockImplementation(async () => store.coverage),
    spyOn(api, "listMyReplies").mockImplementation(async () => store.replies),
    spyOn(api, "getComment").mockImplementation(async () => {
      throw new Error("no thread read here");
    }),
    spyOn(api, "getMessage").mockImplementation(async () => {
      throw new Error("no thread read here");
    })
  );
}

/** The header's count, as the new page's header reads it: the number of the same hook's items. */
function HeaderCount(): ReactNode {
  return <output data-testid="header-count">{useWaitingItems(KEY).items.length}</output>;
}

/** What the list draws, beside the header's count, and a mismatch on any render. */
const mismatches: string[] = [];

function Harness({ store }: { store: Store }): ReactNode {
  const { items } = useWaitingItems(KEY);
  // The names Dashboard publishes for what its items cite, asked before any read answers them,
  // so no text reads a reference of its own (R8); no batched read is made here.
  const names = useDashboardNames(store.issue, {
    references: [],
    settled: false,
    texts: items.flatMap((item) =>
      item.kind === "decision"
        ? [item.decision.heading, item.decision.text]
        : item.kind === "question" || item.kind === "approval"
          ? [item.ask.question]
          : []
    ),
  });
  const [drawn, setDrawn] = useState(WAITING_STEP);
  const header = document.querySelector('[data-testid="header-count"]')?.textContent;
  const listed = document.querySelector('[data-testid="dashboard-waiting-count"]')?.textContent;
  if (header !== undefined && listed !== undefined && header !== listed) {
    mismatches.push(`${header} != ${listed}`);
  }
  return (
    <PublishedReferenceNames published={names.published}>
      <HeaderCount />
      <WaitingList
        decisionsDocument={decisionsDocumentId(store.issue.artifacts, store.decisions)}
        drawn={drawn}
        issue={store.issue}
        items={items}
        names={names}
        onMore={() => setDrawn((count) => count + WAITING_STEP)}
        page="new"
        viewer={READER}
      />
    </PublishedReferenceNames>
  );
}

function draw(store: Store) {
  serve(store);
  mismatches.length = 0;
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <AppProviders client={client}>
        <NoTooltips>
          <Harness store={store} />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
  const origin = performance.timeOrigin;
  return { client, origin, view };
}

function drawnIds(): string[] {
  return [...document.querySelectorAll('[data-testid^="dashboard-item-"]')].map(
    (item) => item.getAttribute("data-testid") ?? ""
  );
}

async function expectCount(count: number): Promise<void> {
  await waitFor(() => {
    expect(screen.getByTestId("dashboard-waiting-count").textContent).toBe(String(count));
    expect(screen.getByTestId("header-count").textContent).toBe(String(count));
  });
}

async function refresh(client: QueryClient, queryKey: readonly unknown[]): Promise<void> {
  await act(() => client.invalidateQueries({ queryKey }));
}

test("K1: a question leaves the list and the count once its answer lands and the ask event refreshes", async () => {
  const store = questions(2);
  const { client, origin, view } = draw(store);
  try {
    await expectCount(2);
    store.asks = store.asks.filter((ask) => ask.id !== "q-1");
    await refresh(client, ["asks", KEY]);
    await expectCount(1);
    expect(drawnIds()).toEqual(["dashboard-item-question-q-0"]);
    expect(performance.timeOrigin).toBe(origin);
    expect(mismatches).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("K2 and K3: a question stays while its answer is out, and stays with the refusal's words", async () => {
  const store = questions(1);
  const answer = Promise.withResolvers<never>();
  spies.push(spyOn(api, "answerAsk").mockImplementation(() => answer.promise));
  const { view } = draw(store);
  try {
    await expectCount(1);
    const item = screen.getByTestId("dashboard-item-question-q-0");
    const user = userEvent.setup();
    const field = item.querySelector<HTMLTextAreaElement>("[data-ask-answer]");
    if (field === null) throw new Error("the question draws no answer box");
    await user.type(field, "Newest first.");
    await user.click(within(item).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(api.answerAsk).toHaveBeenCalledTimes(1));
    await expectCount(1);
    expect(screen.getByTestId("dashboard-item-question-q-0")).toBeTruthy();
    await act(async () => {
      answer.reject(new ApiError(409, { code: "ASK_CLOSED", error: "This question is closed." }));
    });
    await expectCount(1);
    expect(screen.getByTestId("dashboard-item-question-q-0").textContent).not.toBe("");
    expect(mismatches).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("K4: a kept decision leaves even where a decisions read still out answers it waiting", async () => {
  const store = fixtures();
  store.asks = [];
  store.replies = { ...store.replies, rows: [] };
  const { client, view } = draw(store);
  try {
    const before = expected(store);
    await expectCount(before.length);
    const decision = before.flatMap((one) => (one.kind === "decision" ? [one.decision] : []))[0];
    if (decision === undefined) throw new Error("the decisions fixture holds none to review");
    const kept = { block_id: decision.block_id, kept_at: "2026-11-25T12:00:00Z", kept_by: READER };
    spies.push(
      spyOn(api, "keepDecision").mockImplementation(async () => {
        // The server keeps it: every read after the Keep answers it kept.
        store.decisions = {
          ...store.decisions,
          decisions: (store.decisions.decisions ?? []).map((one) =>
            one.block_id === decision.block_id ? { ...one, kept, waiting_review: false } : one
          ),
        } as IssueDecisions;
        return kept as never;
      })
    );
    // A decisions read asked before the Keep, still out as it lands, which answers it waiting.
    const stillWaiting = structuredClone(store.decisions);
    const read = Promise.withResolvers<IssueDecisions>();
    spies.push(
      spyOn(api, "getIssueDecisions")
        .mockImplementationOnce(() => read.promise)
        .mockImplementation(async () => store.decisions)
    );
    void client.invalidateQueries({ queryKey: ["decisions", KEY] });
    const item = screen.getByTestId(`dashboard-item-decision-${decision.block_id}`);
    await userEvent.setup().click(within(item).getByRole("button", { name: "Keep" }));
    await act(async () => {
      read.resolve(stillWaiting);
    });
    await expectCount(before.length - 1);
    expect(screen.queryByTestId(`dashboard-item-decision-${decision.block_id}`)).toBeNull();
    expect(mismatches).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("K5: a thread leaves once the reply lands and the event refreshes the replies read", async () => {
  const store = fixtures();
  store.asks = [];
  store.decisions = { ...store.decisions, decisions: [] };
  const { client, view } = draw(store);
  try {
    const rows = (store.replies.rows ?? []).filter(
      (row) => !(row.root_author?.kind === "session" && row.root_author.id === "legion-daemon:ACME")
    );
    await expectCount(rows.length + 1);
    const answered = rows[0];
    if (answered === undefined) throw new Error("the replies fixture holds no thread");
    store.replies = {
      ...store.replies,
      rows: (store.replies.rows ?? []).filter((row) => row.thread_id !== answered.thread_id),
    };
    await refresh(client, myRepliesKey);
    await expectCount(rows.length);
    expect(screen.queryByTestId(`dashboard-item-thread-${answered.thread_id}`)).toBeNull();
    expect(mismatches).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("K6: a stale approval leaves once the issue read says approved, and holds until then", async () => {
  const store = fixtures();
  store.asks = [];
  store.decisions = { ...store.decisions, decisions: [] };
  store.replies = { ...store.replies, rows: [] };
  const stale = (store.issue.artifacts ?? []).find(
    (artifact) => artifact.approval?.state === "stale"
  );
  if (stale === undefined) throw new Error("the issue fixture holds no stale approval");
  spies.push(spyOn(api, "createArtifactReview").mockImplementation(async () => ({}) as never));
  const { client, view } = draw(store);
  try {
    await expectCount(1);
    const item = screen.getByTestId(`dashboard-item-stale-${stale.id}`);
    await userEvent.setup().click(within(item).getByRole("button", { name: /^Approve v\d+$/ }));
    await expectCount(1);
    store.issue = {
      ...store.issue,
      artifacts: (store.issue.artifacts ?? []).map((artifact) =>
        artifact.id === stale.id && artifact.approval !== undefined
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
    await refresh(client, ["issue", KEY]);
    await expectCount(0);
    expect(mismatches).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("K7: a decision leaves once a Change lands and the comment event refreshes the comments", async () => {
  const store = fixtures();
  store.asks = [];
  store.replies = { ...store.replies, rows: [] };
  store.issue = {
    ...store.issue,
    artifacts: (store.issue.artifacts ?? []).map((artifact) => ({
      ...artifact,
      approval: undefined,
    })),
  };
  const { client, view } = draw(store);
  try {
    const before = expected(store);
    await expectCount(before.length);
    const decision = before.flatMap((one) => (one.kind === "decision" ? [one.decision] : []))[0];
    const change = store.changes[0];
    if (decision === undefined || change === undefined) throw new Error("no decision to change");
    // The person's Change, anchored to the decision's own words, newer than its words.
    store.changes = [
      ...store.changes,
      {
        ...change,
        anchor: { ...change.anchor, quote: anchorQuote(decision) ?? decision.text },
        author: { id: READER, kind: "user" },
        created_at: "2026-11-26T12:00:00Z",
        id: "change-new",
      } as Comment,
    ];
    await refresh(client, ["comments", KEY]);
    await expectCount(before.length - 1);
    expect(mismatches).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("P1: with seven items and five drawn, one leaving draws the sixth and says one is left", async () => {
  const store = questions(7);
  const { client, view } = draw(store);
  try {
    await expectCount(7);
    expect(drawnIds()).toHaveLength(5);
    expect(screen.getByRole("button", { name: /^Show the next five/ }).textContent).toContain("2");
    store.asks = store.asks.filter((ask) => ask.id !== "q-4");
    await refresh(client, ["asks", KEY]);
    await expectCount(6);
    expect(drawnIds()).toHaveLength(5);
    expect(drawnIds()).toContain("dashboard-item-question-q-1");
    expect(screen.getByRole("button", { name: /^Show the next five/ }).textContent).toContain("1");
  } finally {
    view.unmount();
  }
});

test("P2: with eleven items and the next five shown, one leaving draws ten and no control", async () => {
  const store = questions(11);
  const { client, view } = draw(store);
  try {
    await expectCount(11);
    await userEvent.setup().click(screen.getByRole("button", { name: /^Show the next five/ }));
    expect(drawnIds()).toHaveLength(10);
    store.asks = store.asks.filter((ask) => ask.id !== "q-10");
    await refresh(client, ["asks", KEY]);
    await expectCount(10);
    expect(drawnIds()).toHaveLength(10);
    expect(screen.queryByRole("button", { name: /^Show the next five/ })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("L1: a live delta that moves the phase to testing marks Testing with no reload", async () => {
  const client = new QueryClient();
  const state = (phase: string): LegionIssueResponse =>
    ({ configured: true, state: { phase, roles: [] } }) as unknown as LegionIssueResponse;
  // The socket writes each delta of the live view into the query's cache (`applyDelta`); the
  // track reads that cache and subscribes nothing here.
  client.setQueryData(legionIssueQuery(KEY).queryKey, state("planning"));
  function Track(): ReactNode {
    const legion = useQuery({ ...legionIssueQuery(KEY), enabled: false }).data;
    return <PhaseTrack handoffs={[]} legion={legion} />;
  }
  const view = render(
    <AppProviders client={client}>
      <NoTooltips>
        <Track />
      </NoTooltips>
    </AppProviders>
  );
  try {
    const origin = performance.timeOrigin;
    expect(document.querySelector('[aria-current="step"]')?.textContent).toBe("Planning");
    act(() => {
      client.setQueryData(legionIssueQuery(KEY).queryKey, state("testing"));
    });
    expect(performance.timeOrigin).toBe(origin);
    expect(document.querySelector('[aria-current="step"]')?.textContent).toBe("Testing");
  } finally {
    view.unmount();
  }
});

test("H1: the header's count is the list's length on every render through the events", async () => {
  const store = questions(3);
  const { client, view } = draw(store);
  try {
    await expectCount(3);
    for (const id of ["q-2", "q-1", "q-0"]) {
      store.asks = store.asks.filter((ask) => ask.id !== id);
      await refresh(client, ["asks", KEY]);
    }
    await expectCount(0);
    expect(mismatches).toEqual([]);
  } finally {
    view.unmount();
  }
});
