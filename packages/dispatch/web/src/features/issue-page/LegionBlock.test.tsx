import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import {
  errorFrame,
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import { liveSocketFor } from "../../api/live-socket";
import type { LegionIssueResponse, LegionIssueState } from "../../api/types";
import { LegionBlock } from "./LegionBlock";
import { AUTHOR_TEXT_ATTRIBUTE } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

// The Legion block's own read of the issue's Legion state (dispatch://INFRA-4870/spec#b-r-block):
// the live view `legion_issue` of `/ws/live`, which `legionIssueQuery` subscribes, answered here
// by a socket the test drives. What the block draws of each state is `legion-block.test.ts`'s.

const KEY = "ACME-1";
const LEGION = CONTROL_LABELS.legion;

let sockets: FakeLiveSockets;
const clients: QueryClient[] = [];

beforeEach(() => {
  sockets = installFakeLiveSockets();
});

afterEach(() => {
  cleanup();
  for (const client of clients.splice(0)) liveSocketFor(client).close();
  sockets.restore();
});

/** The state of an issue in `implementing`, with a slot and two roles in a turn. */
const IMPLEMENTING: LegionIssueState = {
  blocked_by: [],
  phase: "implementing",
  phase_since: "2026-11-20T18:35:00Z",
  phase_start_seen: true,
  roles: [
    { role: "architect", state: "working" },
    { role: "implementer", state: "working" },
    { role: "tester", state: "idle" },
  ],
  slot: 0,
  waiting_line: null,
  waiting_on: null,
};

/** The issue's Legion read: `state` over `IMPLEMENTING`, of a project that names a daemon. */
function answerOf(state: Partial<LegionIssueState> = {}): LegionIssueResponse {
  return {
    configured: true,
    issue: KEY,
    project: "ACME",
    read: { at: "2026-11-20T20:22:00Z", error: null, ok: true },
    runs_revision: 2,
    state: { ...IMPLEMENTING, ...state },
  };
}

/** The block alone, as Details draws it while it is open. */
function renderBlock(): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  clients.push(client);
  render(
    <MemoryRouter initialEntries={[`/issues/${KEY}`]}>
      <QueryClientProvider client={client}>
        <LegionBlock issueKey={KEY} />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The page's live socket, accepted, and the `legion_issue` subscription it sent last. */
async function subscribed(): Promise<{ socket: FakeLiveSocket; sub: string }> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  if (socket.readyState !== 1) act(() => socket.open());
  const frame = socket.subscribed("legion_issue");
  expect(frame.view).toEqual({ issue: KEY, kind: "legion_issue" });
  return { socket, sub: frame.sub };
}

/** The `legion_issue` subscribe frames `socket` sent. */
function legionSubscribes(socket: FakeLiveSocket): number {
  return socket.subscribes().filter((frame) => frame.view.kind === "legion_issue").length;
}

/** The block's region, by its heading. */
function block(): HTMLElement {
  return screen.getByRole("region", { name: LEGION.heading });
}

/** Each line of the block's list of roles, as it reads: the role, then its claim's word. */
function roleLines(region: HTMLElement): (string | null)[] {
  return within(region)
    .getAllByRole("listitem")
    .map((item) => item.textContent);
}

/** Dispatch's refusal of the subscription `sub`: a failure of its own, never `INVALID_FRAME`,
 *  which closes the socket rather than the subscription. */
function refusal(sub: string) {
  return errorFrame(sub, "INTERNAL", "the state could not be read: refused by the store");
}

test("Q1: while the first answer is out the block draws nothing", async () => {
  renderBlock();
  const { socket } = await subscribed();
  expect(legionSubscribes(socket)).toBe(1);
  expect(screen.queryByRole("region", { name: LEGION.heading })).toBeNull();
  expect(screen.queryByText(LEGION.heading)).toBeNull();
  expect(screen.queryByText(ANSWER_LINES.legion_unreadable)).toBeNull();
});

test("Q2: a read that failed draws the heading, the error line and Retry", async () => {
  renderBlock();
  const { socket, sub } = await subscribed();
  act(() => socket.receive(refusal(sub)));
  const region = await screen.findByRole("region", { name: LEGION.heading });
  expect(within(region).getByRole("heading", { name: LEGION.heading })).toBeDefined();
  expect(within(region).getByRole("alert").textContent).toBe(ANSWER_LINES.legion_unreadable);
  expect(within(region).getByRole("button", { name: CONTROL_LABELS.retry })).toBeDefined();
  expect(region.textContent, "the block drew Dispatch's error text").not.toContain("refused");
});

test("Q3: Retry subscribes again, and the block draws the answer", async () => {
  renderBlock();
  const { socket, sub } = await subscribed();
  act(() => socket.receive(refusal(sub)));
  const retry = await within(
    await screen.findByRole("region", { name: LEGION.heading })
  ).findByRole("button", { name: CONTROL_LABELS.retry });
  await userEvent.click(retry);
  await waitFor(() => expect(legionSubscribes(socket)).toBe(2));
  const again = socket.subscribed("legion_issue").sub;
  expect(again).not.toBe(sub);
  act(() => socket.receive(snapshotFrame(again, answerOf())));
  await waitFor(() => expect(within(block()).queryByRole("alert")).toBeNull());
  expect(within(block()).getByText("Implementing")).toBeDefined();
  expect(within(block()).queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull();
  // Retry held the focus, and its read answered: the focus goes to the block's heading.
  expect(document.activeElement).toBe(
    within(block()).getByRole("heading", { name: LEGION.heading })
  );
});

test("Q4: a failure after an answer keeps the block and says so", async () => {
  renderBlock();
  const { socket, sub } = await subscribed();
  act(() => socket.receive(snapshotFrame(sub, answerOf())));
  expect(
    within(await screen.findByRole("region", { name: LEGION.heading })).getByText("Implementing")
  ).toBeDefined();
  act(() => socket.receive(refusal(sub)));
  await waitFor(() =>
    expect(within(block()).getByRole("alert").textContent).toBe(ANSWER_LINES.legion_unreadable)
  );
  expect(within(block()).getByText("Implementing")).toBeDefined();
  expect(within(block()).getByText(LEGION.at_work)).toBeDefined();
  expect(within(block()).getByRole("button", { name: CONTROL_LABELS.retry })).toBeDefined();
});

test("the block holds no list of runs or handoffs", async () => {
  const reads = [
    spyOn(api, "listSteps"),
    spyOn(api, "listHandoffs"),
    spyOn(api, "getHandoff"),
    spyOn(api, "getIssueEvents"),
  ];
  try {
    renderBlock();
    const { socket, sub } = await subscribed();
    act(() => socket.receive(snapshotFrame(sub, answerOf())));
    const region = await screen.findByRole("region", { name: LEGION.heading });
    // Its one list is of the roles with a claim, one entry each.
    expect(within(region).getAllByRole("list")).toHaveLength(1);
    expect(roleLines(region)).toEqual([
      "architect in a turn",
      "implementer in a turn",
      "tester idle",
    ]);
    expect(
      region.querySelector("[data-step-run], [data-history-item], [data-event-seq], [data-turn]"),
      "the block holds an entry of a run or a handoff"
    ).toBeNull();
    expect(
      reads.map((read) => read.mock.calls.length),
      "the block read the runs, the handoffs or the log"
    ).toEqual([0, 0, 0, 0]);
  } finally {
    for (const read of reads) read.mockRestore();
  }
});

test("a member of waiting_on is drawn as text", async () => {
  const kind = '<b data-wait-markup="kind">gate</b>';
  const ask = '<img data-wait-markup="ask" src="x" onerror="window.__waited = 1">';
  const url = "https://evil.example/asks/1";
  renderBlock();
  const { socket, sub } = await subscribed();
  act(() =>
    socket.receive(
      snapshotFrame(
        sub,
        answerOf({
          phase: "awaiting_person",
          slot: null,
          waiting_on: { ask, artifact_id: url, kind },
        })
      )
    )
  );
  const region = await screen.findByRole("region", { name: LEGION.heading });
  expect(within(region).getByText(LEGION.waits_on_person)).toBeDefined();
  expect(
    region.querySelector("[data-wait-markup]"),
    "a member of the wait became markup"
  ).toBeNull();
  expect(region.querySelector("a"), "a member of the wait became a link").toBeNull();
  for (const member of [kind, ask, url]) {
    const drawn = within(region).getByText(member);
    expect(drawn.closest(`[${AUTHOR_TEXT_ATTRIBUTE}]`), member).not.toBeNull();
  }
});

test("a delta of the live view changes the words", async () => {
  renderBlock();
  const { socket, sub } = await subscribed();
  act(() => socket.receive(snapshotFrame(sub, answerOf())));
  const region = await screen.findByRole("region", { name: LEGION.heading });
  expect(within(region).getByText("Implementing")).toBeDefined();
  expect(roleLines(region)).toEqual([
    "architect in a turn",
    "implementer in a turn",
    "tester idle",
  ]);
  const testing: LegionIssueState = {
    ...IMPLEMENTING,
    phase: "testing",
    roles: [
      { role: "architect", state: "working" },
      { role: "implementer", state: "idle" },
      { role: "tester", state: "working" },
    ],
  };
  act(() =>
    socket.receive({
      changed_at: null,
      counts: {},
      items: {},
      maps: {},
      revs: { fields: { state: "2" } },
      seq: "2",
      set: { state: testing },
      sub,
      type: "delta",
    })
  );
  await waitFor(() => expect(within(block()).getByText("Testing")).toBeDefined());
  expect(within(block()).queryByText("Implementing")).toBeNull();
  expect(roleLines(block())).toEqual([
    "architect in a turn",
    "implementer idle",
    "tester in a turn",
  ]);
  expect(legionSubscribes(socket), "the delta subscribed the view again").toBe(1);
});
