import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { type HeldNotifications, holdNotifications } from "../../../__tests__/commits";
import { jump, realClock } from "../../../__tests__/fake-clock";
import { installFakeLiveSockets } from "../../../__tests__/live-socket-harness";
import { ApiError, api, type WriteBounds, type WriteRequest } from "../../../api/client";
import { liveSocketFor } from "../../../api/live-socket";
import { issueCoverageQuery } from "../../../api/queries";
import { MIN_REFETCH_INTERVAL_MS } from "../../../api/query-refresh";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { buildIssueTree, type TreeNode } from "../issue-tree";
import { NodeRow } from "../NodeRow";
import { NoTooltips } from "../NoTooltips";
import { CONTROL_LABELS, SETTLE_ANSWERS } from "../page-words";
import { askRead, issueDetails } from "../spec-filter/spec-fixtures";
import { treeAnswer as fixtureAnswer, treeNode } from "../tree-read/tree-fixtures";
import {
  outlineQuery,
  subscribeTreeFreshness,
  type TreeAnswer,
  viewQuery,
} from "../tree-read/tree-read";
import { closeOvertaken } from "./node-states";
import { SettleIssueProvider } from "./settle-write";

/**
 * Settle on a focused proposed row, drawn by `NodeRow.tsx`'s `RowActs` in the row as the page
 * draws it, beside the reads the page holds while the row is drawn: the outline's read of the
 * tree on Conversation (`OUTLINE`), the coverage read and the one view a slot has on
 * (Verification), each under the query options `tree-read.ts` gives it, with the Spec view's read
 * held from the load and read by nobody now, and the
 * tree read's subscription (`subscribeTreeFreshness`) keeping them fresh as the page does, with
 * the live feed up as the shell's event feed holds it: with the feed down nothing tells what a
 * write did, so the subscription reads the tree and every view itself at each write's settle,
 * whatever its answer. Each read and each write is answered here, so the test reads which went
 * out.
 */

const KEY = "CORE-1";
const REFERENCE = `dispatch://${KEY}/artifact/spec-md#b-r7`;
/** The settle's path: the node's whole address percent-encoded as one segment. */
const SETTLE_PATH =
  "/api/v1/issues/CORE-1/nodes/dispatch%3A%2F%2FCORE-1%2Fartifact%2Fspec-md%23b-r7/settle";
/** The Spec view's read, held from the load and read by nobody now, and the Verification view's,
 *  which a slot has on: each under its key (`viewKey`), as `viewQuery` gives it. */
const HELD_VIEW = viewQuery(KEY, "spec").queryKey;
const OPEN_VIEW = viewQuery(KEY, "verification").queryKey;
/** The read the outline draws its rows from on Conversation, under its own key
 *  (`conversationTreeKey`), as the page holds it: the read a settle's landing is told by. */
const OUTLINE = outlineQuery(KEY, "conversation");

/** Requirement 7, which an agent proposed and nobody has settled: the tree read names it by the
 *  id `readId` holds. */
const PROPOSED: TreeNode = {
  alsoUnder: [],
  children: [],
  chips: [],
  facts: [],
  gaps: [],
  held: 0,
  id: "spec-md#b-r7",
  life: "proposed",
  question: false,
  readId: `${KEY}/spec-md#r7`,
  reference: REFERENCE,
  replaced: false,
  text: "Requirement 7",
  transcluded: false,
  type: "requirement",
  waiting: false,
};

/** Requirement 7 of another issue, which the tree of `KEY` draws: its own address names that
 *  issue, so it settles under it. */
const OTHER_ISSUE: TreeNode = {
  ...PROPOSED,
  id: "core-2-spec-md#b-r7",
  readId: "CORE-2/spec-md#r7",
  reference: "dispatch://CORE-2/artifact/spec-md#b-r7",
};
/** The settle's path of `OTHER_ISSUE`, under the issue its address names. */
const OTHER_SETTLE_PATH =
  "/api/v1/issues/CORE-2/nodes/dispatch%3A%2F%2FCORE-2%2Fartifact%2Fspec-md%23b-r7/settle";

/** One page of the tree read or of a view, which is all of it. */
function treeAnswer(): TreeAnswer {
  return fixtureAnswer([], [], { converted: true });
}

/** Dispatch's 200 to a settle. */
function settled(): unknown {
  return {
    node: { address: REFERENCE, kind: "requirement", state: "settled", visibility: "project" },
    previous: "proposed",
  };
}

/** The reads the page holds while the row is drawn: the tree, the coverage, and the view a slot
 *  has on. */
function PageReads(): ReactNode {
  useQuery(OUTLINE);
  useQuery(issueCoverageQuery(KEY));
  useQuery(viewQuery(KEY, "verification"));
  return null;
}

/** The row of `node` as `IssueTree` draws it, focused where `focused` says, its node as the tree
 *  read the page holds draws it: where the answer holds the node (its `readId`), with the marks
 *  that answer gives it, as the outline stamps them on its rows (`closeOvertaken`), so an answer
 *  that holds it settled draws it with no mark and no act; else as given. The read is watched by
 *  an observer that is never enabled, so it sends no request. */
function TreeRow({ focused, node }: { focused: boolean; node: TreeNode }): ReactNode {
  const answer = useQuery({ ...OUTLINE, enabled: false }).data;
  return (
    <NodeRow
      author={{ kind: "person", name: "bob", reader: false }}
      context={false}
      focused={focused}
      isClosed={false}
      level={1}
      node={
        answer?.nodes?.some((read) => read.id === node.readId) ? closeOvertaken(node, answer) : node
      }
      onFocus={() => {}}
      onToggle={() => {}}
      ownAnswer={false}
      rowKey="row"
      tabStop
    />
  );
}

interface DrawnRow {
  readonly client: QueryClient;
  /** Each read that went out, in order: `tree`, a view's name, or `coverage`. */
  readonly reads: string[];
  /** Whether the focus was on the row as each read of `reads` went out. */
  readonly onRowAtRead: boolean[];
  /** Each settle that went out, as it went, and the key it went under. */
  readonly sent: WriteRequest[];
  readonly keys: string[];
  readonly settle: () => HTMLButtonElement;
  readonly sentence: () => Element | null;
  readonly row: () => HTMLElement;
  /** A control outside the row, where a test puts the focus before a press. */
  readonly elsewhere: () => HTMLButtonElement;
  /** Draws the row unfocused and then focused again, as the tree does when the reader moves the
   *  focus to another row and back (`j`, then `k`): an unfocused row draws no act, so the focused
   *  row draws a new one. */
  readonly refocus: () => void;
  readonly unmount: () => void;
}

/** The live feed of `client` up, as the shell's event feed holds it: its socket open and the
 *  feed's subscription on it started by Dispatch (`events_start`). Answers the end of the feed,
 *  which puts the page's WebSocket back. */
function liveFeedOn(client: QueryClient): () => void {
  const sockets = installFakeLiveSockets();
  const live = liveSocketFor(client);
  live.subscribeEvents({ onEvent: () => undefined, onStart: () => undefined, since: () => null });
  const socket = sockets.latest();
  act(() => socket.open());
  const { sub } = socket.subscribed("events");
  act(() => socket.receive({ type: "events_start", sub, since: "1", replay_complete: true }));
  return () => {
    live.close();
    sockets.restore();
  };
}

/** How a case draws its row: the node, `PROPOSED` where none is given; the live feed, up where
 *  none is said, as the shell's event feed holds it; the number of first reads of the coverage
 *  that fail, none where none is said; the coverage each other read answers, no requirement
 *  where none is given; and what each read of the tree answers, `treeAnswer()` at once where
 *  nothing is given. */
interface RowOptions {
  readonly covered?: unknown;
  readonly failedCoverage?: number;
  readonly feed?: "up" | "down";
  readonly node?: TreeNode;
  readonly tree?: () => Promise<TreeAnswer>;
}

/** The focused proposed row `options.node`, drawn in the tree of `KEY` as `IssueTree` draws its
 *  rows (`SettleIssueProvider`) and as the tree read draws its node (`TreeRow`), its settle
 *  answered by `answer` at each attempt, the first `options.failedCoverage` reads of the coverage
 *  failing, each other read of it answered `options.covered`, and each read of the tree answered
 *  by `options.tree`. `refocus` draws it unfocused and focused again. */
function drawRow(
  answer: (attempt: number) => Promise<unknown>,
  options: RowOptions = {}
): DrawnRow {
  const { covered = { requirements: [] }, failedCoverage = 0 } = options;
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(OUTLINE.queryKey, treeAnswer());
  client.setQueryData(HELD_VIEW, treeAnswer());
  client.setQueryData(OPEN_VIEW, treeAnswer());
  client.setQueryData(issueCoverageQuery(KEY).queryKey, { requirements: [] } as never);
  const reads: string[] = [];
  const onRowAtRead: boolean[] = [];
  const read = (what: string) => {
    reads.push(what);
    onRowAtRead.push(document.activeElement?.hasAttribute("data-node-row") === true);
  };
  const tree = spyOn(api, "getIssueTree").mockImplementation(async (_key, query) => {
    read(query?.view ?? "tree");
    return query?.view === undefined && options.tree !== undefined ? options.tree() : treeAnswer();
  });
  let coverageReads = 0;
  const coverage = spyOn(api, "getIssueCoverage").mockImplementation(async () => {
    read("coverage");
    coverageReads += 1;
    if (coverageReads <= failedCoverage) throw new ApiError(503, { error: "unavailable" });
    return covered as never;
  });
  const sent: WriteRequest[] = [];
  const keys: string[] = [];
  const send = spyOn(api, "send").mockImplementation(((
    request: WriteRequest,
    bounds: WriteBounds
  ) => {
    sent.push(request);
    keys.push(bounds.key);
    return answer(sent.length);
  }) as typeof api.send);
  const endFeed = options.feed === "down" ? () => undefined : liveFeedOn(client);
  const unsubscribe = subscribeTreeFreshness(client, KEY);
  const page = (focused: boolean) => (
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <NoTooltips>
          <PageReads />
          <button data-elsewhere="" type="button">
            Elsewhere
          </button>
          <SettleIssueProvider value={KEY}>
            <TreeRow focused={focused} node={options.node ?? PROPOSED} />
          </SettleIssueProvider>
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(page(true));
  const found = <E extends Element>(selector: string): E => {
    const element = view.container.querySelector<E>(selector);
    if (element === null) throw new Error(`the page draws no ${selector}`);
    return element;
  };
  return {
    client,
    elsewhere: () => found<HTMLButtonElement>("[data-elsewhere]"),
    keys,
    onRowAtRead,
    reads,
    refocus: () => {
      view.rerender(page(false));
      view.rerender(page(true));
    },
    row: () => found<HTMLElement>("[data-node-row]"),
    sent,
    sentence: () => view.container.querySelector("[data-settle-answer]"),
    settle: () => found<HTMLButtonElement>('[data-row-act="settle"]'),
    unmount: () => {
      view.unmount();
      unsubscribe();
      endFeed();
      send.mockRestore();
      coverage.mockRestore();
      tree.mockRestore();
    },
  };
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// Enter or a press sends one `POST /api/v1/issues/{key}/nodes/{id}/settle`, `{id}` the node's
// whole address percent-encoded as one segment, and no body: a cookie proves a person, so the act
// names no actor.
test("Settle sends the address and no body", async () => {
  const user = userEvent.setup();
  const drawn = drawRow(async () => settled());
  try {
    expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle);
    expect(drawn.settle().tabIndex).toBe(-1);
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sent).toHaveLength(1));
    // `toStrictEqual`: the request holds no `body` at all, not even an undefined one.
    expect(drawn.sent).toStrictEqual([{ method: "POST", path: SETTLE_PATH }]);
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(drawn.sent).toHaveLength(1);
  } finally {
    drawn.unmount();
  }
});

/** The settle's path of the ask `ask-1` of `KEY`: its whole address percent-encoded as one
 *  segment. */
const ASK_SETTLE_PATH = "/api/v1/issues/CORE-1/nodes/dispatch%3A%2F%2FCORE-1%2Fask%2Fask-1/settle";

// A question written as an `:::ask` block of the spec is one row, drawn from the block, which
// takes the state of the ask's node, the question itself, and names the question by the ask's
// address: so the question its ask's node holds proposed carries Settle, and one press sends one
// settle of the ask's address.
test("Settle on a question's row sends its ask's address", async () => {
  const user = userEvent.setup();
  const outline = buildIssueTree({
    asks: [
      {
        ...askRead("ask-1", ""),
        answer: null,
        block_artifact: { id: "artifact-spec", primary: true, slug: "spec" },
        block_id: "q1",
        state: "open",
      },
    ],
    issue: issueDetails(),
    tree: fixtureAnswer(
      [
        treeNode(KEY, "issue", null),
        treeNode("q1", "ask", KEY, {
          document: { artifact_id: "artifact-spec", position: 0, slug: "spec" },
        }),
        treeNode("ask-1", "ask", KEY, { state: "proposed" }),
      ],
      [],
      { converted: true }
    ),
  });
  const question = outline.children.find((node) => node.type === "ask");
  expect(question?.readId).toBe(`${KEY}/spec#q1`);
  expect(question?.source).toEqual({ id: "ask-1", issue: KEY, kind: "ask" });
  if (question === undefined) throw new Error("the outline draws no question");
  const drawn = drawRow(async () => settled(), { node: question });
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sent).toHaveLength(1));
    expect(drawn.sent).toStrictEqual([{ method: "POST", path: ASK_SETTLE_PATH }]);
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(drawn.sent).toHaveLength(1);
  } finally {
    drawn.unmount();
  }
});

// On 200 the tree read and the coverage read are read again, the view read held for the
// load is dropped, and the open view is read again; the focus is on the row before any of them.
test("a 200 drops the held views and reads the open one again", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<unknown>();
  const drawn = drawRow(() => landing.promise);
  try {
    act(() => drawn.elsewhere().focus());
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sent).toHaveLength(1));
    // The press leaves the focus where it was, and nothing is read while the write is out.
    expect(document.activeElement).toBe(drawn.elsewhere());
    expect(drawn.reads).toEqual([]);
    await act(async () => landing.resolve(settled()));
    await waitFor(() => expect(drawn.reads).toContain("verification"));
    expect(drawn.reads).toContain("tree");
    expect(drawn.reads).toContain("coverage");
    expect(drawn.reads, "the view nobody reads now was read again").not.toContain("spec");
    expect(drawn.client.getQueryData(HELD_VIEW), "the held view stayed").toBeUndefined();
    await waitFor(() =>
      expect(drawn.client.getQueryData<TreeAnswer>(OPEN_VIEW)).toEqual(treeAnswer())
    );
    expect(drawn.onRowAtRead, "a read went out before the focus was on the row").not.toContain(
      false
    );
    expect(document.activeElement).toBe(drawn.row());
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(drawn.sentence()).toBeNull();
  } finally {
    drawn.unmount();
  }
});

// A coverage read already marked stale - a read of it failed, or a refresh still owes one - is
// read again by the 200 as a fresh one is, and the 200 reads the tree again and drops the held
// views of its own, since the tree read's subscription hears no coverage read.
test("a 200 reads the tree again while the coverage read is marked stale", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<unknown>();
  const drawn = drawRow(() => landing.promise, { failedCoverage: 1 });
  const coverage = issueCoverageQuery(KEY).queryKey;
  // What was read once the settle landed: the first read is the coverage's that failed.
  const landed = () => drawn.reads.slice(1);
  try {
    await act(async () => {
      await drawn.client.refetchQueries({ exact: true, queryKey: coverage });
    });
    expect(drawn.reads).toEqual(["coverage"]);
    expect(drawn.client.getQueryState(coverage)?.isInvalidated).toBe(true);
    act(() => drawn.elsewhere().focus());
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sent).toHaveLength(1));
    expect(drawn.reads).toEqual(["coverage"]);
    await act(async () => landing.resolve(settled()));
    await waitFor(() => expect(landed()).toContain("coverage"));
    await waitFor(() =>
      expect(landed(), "the tree was not read again after the 200").toContain("tree")
    );
    await waitFor(() => expect(landed()).toContain("verification"));
    expect(landed(), "the view nobody reads now was read again").not.toContain("spec");
    expect(drawn.client.getQueryData(HELD_VIEW), "the held view stayed").toBeUndefined();
    expect(
      drawn.onRowAtRead.slice(1),
      "a read went out before the focus was on the row"
    ).not.toContain(false);
    expect(document.activeElement).toBe(drawn.row());
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(drawn.sentence()).toBeNull();
  } finally {
    drawn.unmount();
  }
});

// The coverage is invalidated before the tree is asked for, so its read begins first. The
// settle's callback runs once the tree read's hold has ended (`subscribeTreeFreshness`), and
// nothing asked for a read while the write was out, so the hold reads nothing at its settle and
// the callback reads the tree and the open view once each. The coverage's changed answer asks
// for no read of the tree, which the subscription does not hear, however long the refresh
// scheduler's interval runs.
test("a 200 that changes the coverage reads the coverage, the tree and the open view once each", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<unknown>();
  const drawn = drawRow(() => landing.promise, {
    covered: {
      requirements: [{ block_id: "r7", checks: 0, gaps: [], plan_tasks: 0, ref: REFERENCE }],
    },
  });
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sent).toHaveLength(1));
    expect(drawn.reads).toEqual([]);
    jest.useFakeTimers();
    try {
      await act(async () => landing.resolve(settled()));
      await waitFor(() => expect(drawn.reads).toHaveLength(3));
      await jump(MIN_REFETCH_INTERVAL_MS);
      expect([...drawn.reads].sort()).toEqual(["coverage", "tree", "verification"]);
      expect(drawn.reads[0], "the coverage was not read first").toBe("coverage");
    } finally {
      realClock();
    }
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
  } finally {
    drawn.unmount();
  }
});

// A node whose own address names another issue settles under that issue, and its write is one of
// the issue the tree is drawn for (`WriteSpec.issue`), since the tree read holds for the writes of
// its own issue alone: a read of the tree asked for while the settle is out waits for it, and the
// tree is read once when it settles.
test("a settle of another issue's node holds the reads of the tree it is drawn in", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<unknown>();
  const drawn = drawRow(() => landing.promise, { node: OTHER_ISSUE });
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sent).toHaveLength(1));
    expect(drawn.sent[0]?.path).toBe(OTHER_SETTLE_PATH);
    await act(async () => {
      await drawn.client.invalidateQueries({ exact: true, queryKey: OUTLINE.queryKey });
    });
    expect(drawn.reads, "the tree was read while the settle was out").toEqual([]);
    await act(async () => landing.resolve(settled()));
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    await waitFor(() => expect(drawn.reads).toContain("verification"));
    expect(drawn.reads.filter((what) => what === "tree")).toEqual(["tree"]);
  } finally {
    drawn.unmount();
  }
});

// With the live feed up a refusal sends no read, leaves the row as drawn, its mark included, and
// draws one sentence after the act with the role `status`, read from the refusal's status and
// code and never from its text: the 403 with no code says, in its text, what a
// `SETTLE_NOT_PERMITTED` would, and draws the sentence of every other refusal.
test("each refusal says its sentence and leaves the mark", async () => {
  const user = userEvent.setup();
  const refusals: readonly (readonly [ApiError, string])[] = [
    [
      new ApiError(403, { code: "SETTLE_NOT_PERMITTED", error: "settle not permitted" }),
      SETTLE_ANSWERS.not_permitted,
    ],
    [
      new ApiError(409, { code: "NODE_NOT_PROPOSED", error: "node not proposed" }),
      SETTLE_ANSWERS.not_proposed,
    ],
    [new ApiError(404, { code: "NOT_FOUND", error: "not found" }), SETTLE_ANSWERS.gone],
    [new ApiError(400, { code: "INVALID_JSON", error: "invalid json" }), SETTLE_ANSWERS.failed],
    [new ApiError(403, { error: SETTLE_ANSWERS.not_permitted }), SETTLE_ANSWERS.failed],
  ];
  const drawn = drawRow((attempt) => Promise.reject(refusals[attempt - 1]?.[0]));
  try {
    for (const [index, [refusal, words]] of refusals.entries()) {
      await user.click(drawn.settle());
      await waitFor(() => {
        expect(drawn.sent).toHaveLength(index + 1);
        expect(drawn.sentence()?.textContent, `${refusal.status} ${refusal.code}`).toBe(words);
      });
      const sentence = drawn.sentence();
      expect(sentence?.getAttribute("role")).toBe("status");
      expect(drawn.settle().nextElementSibling).toBe(sentence);
      expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle);
      expect(drawn.row().querySelector('[data-node-mark="proposed"]')).not.toBeNull();
      expect(drawn.reads, "a refusal sent a read").toEqual([]);
      expect(drawn.client.getQueryData<TreeAnswer>(HELD_VIEW)).toEqual(treeAnswer());
    }
  } finally {
    drawn.unmount();
  }
});

// With the live feed down nothing tells what a write did, so the tree read's hold reads the tree
// and its views at the settle of every write, a refused one's included (`subscribeTreeFreshness`).
// The refusal still leaves the row as drawn, its mark included, and its sentence stands.
test("with the live feed down a refusal leaves the row and its mark as drawn", async () => {
  const user = userEvent.setup();
  const refused = new ApiError(403, { code: "SETTLE_NOT_PERMITTED", error: "not permitted" });
  const drawn = drawRow(() => Promise.reject(refused), { feed: "down" });
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sentence()?.textContent).toBe(SETTLE_ANSWERS.not_permitted));
    await waitFor(() =>
      expect(drawn.reads, "the hold read nothing at the settle").toContain("tree")
    );
    expect(drawn.reads).not.toContain("coverage");
    expect(drawn.sentence()?.getAttribute("role")).toBe("status");
    expect(drawn.sentence()?.textContent).toBe(SETTLE_ANSWERS.not_permitted);
    expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle);
    expect(drawn.row().querySelector('[data-node-mark="proposed"]')).not.toBeNull();
  } finally {
    drawn.unmount();
  }
});

// `null` (`409 KEY_COMMITTED`) is a landing, as a 200 is: no sentence, the focus on the row, the
// same reads, and the held view dropped.
test("a 409 KEY_COMMITTED lands as a 200", async () => {
  const user = userEvent.setup();
  const drawn = drawRow(() =>
    Promise.reject(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }))
  );
  try {
    act(() => drawn.elsewhere().focus());
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.reads).toContain("verification"));
    expect(drawn.reads).toContain("tree");
    expect(drawn.reads).toContain("coverage");
    expect(drawn.reads).not.toContain("spec");
    expect(drawn.client.getQueryData(HELD_VIEW)).toBeUndefined();
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(document.activeElement).toBe(drawn.row());
    expect(drawn.sentence()).toBeNull();
    expect(drawn.sent).toHaveLength(1);
  } finally {
    drawn.unmount();
  }
});

// While the write is out the act says "Settling…" and takes no press; a 5xx is made again
// under the write's key, so the act goes on saying it while it is tried again. Pressed from the
// keyboard - `f` leaves the focus on it, and Enter presses it - the act hands the focus to its
// row first, so its taking no press drops no focus out of the tree.
test("Settling… is said while the write is out", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<unknown>();
  const drawn = drawRow((attempt) =>
    attempt === 1 ? Promise.reject(new ApiError(500, { error: "boom" })) : landing.promise
  );
  try {
    act(() => drawn.settle().focus());
    await user.keyboard("{Enter}");
    expect(document.activeElement).toBe(drawn.row());
    await waitFor(() => expect(drawn.sent).toHaveLength(1));
    expect(drawn.settle().textContent).toBe(SETTLE_ANSWERS.settling);
    expect(drawn.settle().disabled).toBe(true);
    await waitFor(() => expect(drawn.sent).toHaveLength(2));
    expect(drawn.keys[1]).toBe(drawn.keys[0] ?? "");
    expect(drawn.settle().textContent).toBe(SETTLE_ANSWERS.settling);
    expect(drawn.settle().disabled).toBe(true);
    expect(drawn.sentence()).toBeNull();
    await act(async () => landing.resolve(settled()));
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(drawn.settle().disabled).toBe(false);
    expect(drawn.sentence()).toBeNull();
  } finally {
    drawn.unmount();
  }
});

// A landing reads the tree again, which draws the row with no mark and no act, and the coverage,
// which the write waits for. Until the tree read answers, the act still says "Settling…" and takes
// no press, though the coverage has answered and the write has settled: a press then would settle
// a node Dispatch holds settled, and be refused. Where the tree read answers with the node still
// proposed, the act takes a press again.
test("after a 200 Settling… is said until the tree read answers", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<TreeAnswer>();
  const drawn = drawRow(async () => settled(), { tree: () => answer.promise });
  const tree = () => drawn.client.getQueryState(OUTLINE.queryKey);
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.client.isMutating(), "the write is still out").toBe(0));
    expect(drawn.reads).toContain("coverage");
    expect(drawn.client.getQueryState(issueCoverageQuery(KEY).queryKey)?.fetchStatus).toBe("idle");
    expect(drawn.reads.filter((what) => what === "tree")).toEqual(["tree"]);
    expect(tree()?.fetchStatus, "the tree read answered").toBe("fetching");
    expect(drawn.settle().textContent).toBe(SETTLE_ANSWERS.settling);
    expect(drawn.settle().disabled).toBe(true);

    await act(async () => answer.resolve(treeAnswer()));
    await waitFor(() => expect(tree()?.fetchStatus).toBe("idle"));
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(drawn.settle().disabled).toBe(false);
    expect(drawn.row().querySelector('[data-node-mark="proposed"]')).not.toBeNull();
    expect(drawn.sentence()).toBeNull();
  } finally {
    drawn.unmount();
  }
});

// The read of the tree that ends the wait after a landing draws its row in the render that ends
// the wait: where it answers with the node settled, the act goes with the row's mark, and is never
// drawn taking a press on a row still marked proposed, where a press would settle a node Dispatch
// holds settled, and be refused. TanStack Query tells the tree read's observers on a timer of its
// own, as a browser does (`holdNotifications`), and React draws an update it is told outside that
// timer at once, so a wait that ended outside it would draw the act before the row.
test("after a 200 the tree read that answers the node settled takes the act and the mark in one render", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<TreeAnswer>();
  const drawn = drawRow(async () => settled(), { tree: () => answer.promise });
  const tree = () => drawn.client.getQueryState(OUTLINE.queryKey);
  // What the act said each time it was drawn taking a press on a row still marked proposed.
  const pressable: string[] = [];
  const watch = new MutationObserver(() => {
    const row = drawn.row();
    const settle = row.querySelector<HTMLButtonElement>('[data-row-act="settle"]');
    if (settle === null || row.querySelector('[data-node-mark="proposed"]') === null) return;
    if (!settle.disabled || settle.textContent === CONTROL_LABELS.settle) {
      pressable.push(settle.textContent ?? "");
    }
  });
  let held: HeldNotifications | undefined;
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.client.isMutating(), "the write is still out").toBe(0));
    expect(tree()?.fetchStatus, "the tree read answered").toBe("fetching");
    expect(drawn.settle().textContent).toBe(SETTLE_ANSWERS.settling);
    expect(drawn.settle().disabled).toBe(true);

    watch.observe(drawn.row(), {
      attributes: true,
      characterData: true,
      childList: true,
      subtree: true,
    });
    held = holdNotifications();
    // The answer holds Requirement 7 by the id `PROPOSED.readId` names, settled.
    const settledNode = treeNode("r7", "requirement", KEY, { in: "spec-md", state: "settled" });
    answer.resolve(fixtureAnswer([settledNode], [], { converted: true }));
    await waitFor(() => expect(tree()?.fetchStatus).toBe("idle"));
    held.tell();
    await waitFor(() => expect(drawn.row().querySelector('[data-row-act="settle"]')).toBeNull());
    expect(pressable, "the act took a press on a row still marked proposed").toEqual([]);
    expect(drawn.row().querySelector("[data-node-mark]")).toBeNull();
    expect(drawn.sent).toHaveLength(1);
  } finally {
    held?.tell();
    watch.disconnect();
    drawn.unmount();
  }
});

// The wait after a landing is the node's, not the act's: an unfocused row draws no act, so a
// reader who moves the focus off the row and back before the tree read answers (`j`, then `k`)
// finds a new act, which goes on saying "Settling…" and takes no press, as the act it replaces
// did, until the tree read answers. A press then would settle a node Dispatch holds settled, and
// be refused. Where the tree read answers with the node still proposed, the act takes a press.
test("after a 200 an act drawn again says Settling… until the tree read answers", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<TreeAnswer>();
  const drawn = drawRow(async () => settled(), { tree: () => answer.promise });
  const tree = () => drawn.client.getQueryState(OUTLINE.queryKey);
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.client.isMutating(), "the write is still out").toBe(0));
    expect(tree()?.fetchStatus, "the tree read answered").toBe("fetching");
    const before = drawn.settle();
    drawn.refocus();
    expect(drawn.settle(), "the act was not drawn again").not.toBe(before);
    expect(drawn.settle().textContent).toBe(SETTLE_ANSWERS.settling);
    expect(drawn.settle().disabled).toBe(true);

    await act(async () => answer.resolve(treeAnswer()));
    await waitFor(() => expect(tree()?.fetchStatus).toBe("idle"));
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(drawn.settle().disabled).toBe(false);
    expect(drawn.row().querySelector('[data-node-mark="proposed"]')).not.toBeNull();
    expect(drawn.sentence()).toBeNull();
  } finally {
    drawn.unmount();
  }
});

// The wait is the node's from the press: a reader who moves the focus off the row and back while
// the write is out finds a new act, which says "Settling…" and takes no press, as the act it
// replaces did - a press then would go after the first, and be refused - and goes on saying it
// once the write has landed, until the tree read answers. Where that read draws the node still
// proposed, the act takes a press again.
test("an act drawn again while the write is out says Settling… until the tree read answers", async () => {
  const user = userEvent.setup();
  const landing = Promise.withResolvers<unknown>();
  const answer = Promise.withResolvers<TreeAnswer>();
  const drawn = drawRow(() => landing.promise, { tree: () => answer.promise });
  const tree = () => drawn.client.getQueryState(OUTLINE.queryKey);
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sent).toHaveLength(1));
    const before = drawn.settle();
    drawn.refocus();
    expect(drawn.settle(), "the act was not drawn again").not.toBe(before);
    expect(drawn.settle().textContent).toBe(SETTLE_ANSWERS.settling);
    expect(drawn.settle().disabled).toBe(true);

    await act(async () => landing.resolve(settled()));
    await waitFor(() => expect(drawn.client.isMutating(), "the write is still out").toBe(0));
    expect(tree()?.fetchStatus, "the tree read answered").toBe("fetching");
    expect(drawn.settle().textContent).toBe(SETTLE_ANSWERS.settling);
    expect(drawn.settle().disabled).toBe(true);

    await act(async () => answer.resolve(treeAnswer()));
    await waitFor(() => expect(tree()?.fetchStatus).toBe("idle"));
    await waitFor(() => expect(drawn.settle().textContent).toBe(CONTROL_LABELS.settle));
    expect(drawn.settle().disabled).toBe(false);
    expect(drawn.row().querySelector('[data-node-mark="proposed"]')).not.toBeNull();
    expect(drawn.sentence()).toBeNull();
    expect(drawn.sent).toHaveLength(1);
  } finally {
    drawn.unmount();
  }
});

// The sentence stands until the reader's next act - a key, a press, a wheel or a touch, the
// four of the tree's `READER_ACTS` - and a pointer passing over the page is none.
test("the refusal's sentence goes at the reader's next act", async () => {
  const user = userEvent.setup();
  const drawn = drawRow(() =>
    Promise.reject(new ApiError(409, { code: "NODE_NOT_PROPOSED", error: "node not proposed" }))
  );
  // user-event has no wheel and no touch, so those two are dispatched as the browser sends them.
  const acts: readonly (readonly [string, () => Promise<void>])[] = [
    ["keydown", () => user.keyboard("j")],
    ["pointerdown", () => user.pointer({ keys: "[MouseLeft]", target: document.body })],
    [
      "wheel",
      async () => {
        act(() => {
          document.body.dispatchEvent(new Event("wheel", { bubbles: true }));
        });
      },
    ],
    [
      "touchstart",
      async () => {
        act(() => {
          document.body.dispatchEvent(new Event("touchstart", { bubbles: true }));
        });
      },
    ],
  ];
  try {
    for (const [name, readerAct] of acts) {
      await user.click(drawn.settle());
      await waitFor(() => expect(drawn.sentence()?.textContent).toBe(SETTLE_ANSWERS.not_proposed));
      await user.hover(drawn.row());
      expect(drawn.sentence(), "a pointer passing over ended the sentence").not.toBeNull();
      await readerAct();
      await waitFor(() => expect(drawn.sentence(), `${name} left the sentence`).toBeNull());
    }
    expect(drawn.reads).toEqual([]);
  } finally {
    drawn.unmount();
  }
});

// The sentence is the node's and not its act's (R4): where the tree read the page holds then
// answers the node settled - another person settled it, which is why the press was refused - the
// row draws no mark and no act, and the sentence stays in its place, saying why the press did
// nothing, until the reader's next act ends it.
test("a refusal's sentence stays once the tree read draws the node settled, until the reader's next act", async () => {
  const user = userEvent.setup();
  const drawn = drawRow(() =>
    Promise.reject(new ApiError(409, { code: "NODE_NOT_PROPOSED", error: "node not proposed" }))
  );
  try {
    await user.click(drawn.settle());
    await waitFor(() => expect(drawn.sentence()?.textContent).toBe(SETTLE_ANSWERS.not_proposed));
    expect(drawn.row().querySelector('[data-node-mark="proposed"]')).not.toBeNull();

    // The answer holds Requirement 7 by the id `PROPOSED.readId` names, settled.
    const settledNode = treeNode("r7", "requirement", KEY, { in: "spec-md", state: "settled" });
    act(() => {
      drawn.client.setQueryData(
        OUTLINE.queryKey,
        fixtureAnswer([settledNode], [], { converted: true })
      );
    });
    expect(
      drawn.row().querySelector("[data-node-mark]"),
      "the settled row keeps a mark"
    ).toBeNull();
    expect(
      drawn.row().querySelector('[data-row-act="settle"]'),
      "the settled row keeps Settle"
    ).toBeNull();
    const sentence = drawn.sentence();
    expect(sentence?.textContent, "the sentence went with the act").toBe(
      SETTLE_ANSWERS.not_proposed
    );
    expect(sentence?.getAttribute("role")).toBe("status");
    expect(sentence?.closest("[data-node-row]"), "the sentence left the row").toBe(drawn.row());

    await user.keyboard("j");
    await waitFor(() => expect(drawn.sentence(), "the reader's act left the sentence").toBeNull());
  } finally {
    drawn.unmount();
  }
});
