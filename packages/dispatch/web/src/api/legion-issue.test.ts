import { afterEach, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../__tests__/live-socket-harness";
import configuredFalseBody from "../features/dashboard/fixtures/legion-issue/configured-false.json";
import planningBody from "../features/dashboard/fixtures/legion-issue/planning.json";
import testingBody from "../features/dashboard/fixtures/legion-issue/testing.json";
import { type LegionIssueResponse, legionIssueQuery } from "./legion-issue";
import { liveSocketFor } from "./live-socket";
import type { DeliveryLiveDelta, LiveSubscribe } from "./types";

// The bodies the server at this branch's head answered (the fixtures' README says how each was
// captured): an issue at planning, one at testing, and one of a project with no Legion daemon.
const PLANNING: LegionIssueResponse = planningBody;
const TESTING: LegionIssueResponse = testingBody;
const CONFIGURED_FALSE: LegionIssueResponse = configuredFalseBody;

let sockets: FakeLiveSockets;
let queryClient: QueryClient;

beforeEach(() => {
  sockets = installFakeLiveSockets();
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
});

afterEach(() => {
  cleanup();
  liveSocketFor(queryClient).close();
  queryClient.clear();
  sockets.restore();
});

function wrapper({ children }: { children: ReactNode }) {
  return createElement(QueryClientProvider, { client: queryClient }, children);
}

/** One part that draws `key`'s Legion state, as Dashboard's track does. */
function observe(key: string) {
  return renderHook(() => useQuery(legionIssueQuery(key)), { wrapper });
}

/** The page's live socket, accepted, and the one legion_issue subscription it sent on it. */
async function subscribed(): Promise<{ socket: FakeLiveSocket; frame: LiveSubscribe }> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  const frame = await waitFor(() => socket.subscribed("legion_issue"));
  return { socket, frame };
}

/** The delta Dispatch sends when the daemon's read moves the issue's phase: `state` set whole,
 *  as the view sends every member of its body (`live_frames.go`, `legionIssueView`). */
function stateDelta(sub: string, state: LegionIssueResponse["state"]): DeliveryLiveDelta {
  return {
    type: "delta",
    sub,
    seq: "2",
    changed_at: "2026-11-25T14:05:00Z",
    set: { state },
    items: {},
    maps: {},
    counts: {},
    revs: { fields: { state: "0000000000000002" } },
  };
}

test('the snapshot of the issue\'s legion_issue view is the data, held under ["legion-issue", key]', async () => {
  const hook = observe(PLANNING.issue);
  const { socket, frame } = await subscribed();
  expect(frame.view).toEqual({ kind: "legion_issue", issue: PLANNING.issue });
  expect(hook.result.current.data).toBeUndefined();

  act(() => socket.receive(snapshotFrame(frame.sub, PLANNING)));
  await waitFor(() => expect(hook.result.current.data).toEqual(PLANNING));
  const cached = queryClient.getQueryData<LegionIssueResponse>(["legion-issue", PLANNING.issue]);
  expect(cached).toEqual(PLANNING);
  // The event feed's refreshes leave a live query alone (`isLiveQuery`).
  expect(legionIssueQuery(PLANNING.issue).meta).toEqual({ live: true });
});

test("a delta that sets state moves the phase and keeps every member it does not name", async () => {
  const hook = observe(PLANNING.issue);
  const { socket, frame } = await subscribed();
  act(() => socket.receive(snapshotFrame(frame.sub, PLANNING)));
  await waitFor(() => expect(hook.result.current.data?.state?.phase).toBe("planning"));
  const before = hook.result.current.data;

  act(() => socket.receive(stateDelta(frame.sub, TESTING.state)));
  await waitFor(() => expect(hook.result.current.data?.state?.phase).toBe("testing"));
  expect(hook.result.current.data).toEqual({ ...PLANNING, state: TESTING.state });
  expect(hook.result.current.data?.read).toBe(before?.read);
  expect(socket.subscribes()).toHaveLength(1);
});

test("two observers of one issue hold one subscription, and the last to go drops it", async () => {
  const first = observe(PLANNING.issue);
  const second = observe(PLANNING.issue);
  const { socket, frame } = await subscribed();
  act(() => socket.receive(snapshotFrame(frame.sub, PLANNING)));
  await waitFor(() => {
    expect(first.result.current.data).toEqual(PLANNING);
    expect(second.result.current.data).toEqual(PLANNING);
  });
  expect(socket.subscribes()).toHaveLength(1);

  first.unmount();
  expect(socket.unsubscribes()).toEqual([]);
  second.unmount();
  expect(socket.unsubscribes()).toEqual([{ type: "unsubscribe", sub: frame.sub }]);
});

test('an invalidation of the issue\'s reads, ["issue", key], sends no second subscription', async () => {
  // A read of the issue's own under ["issue", key], so the row sees the invalidation land.
  let issueReads = 0;
  const issue = renderHook(
    () =>
      useQuery({
        queryKey: ["issue", PLANNING.issue],
        queryFn: async () => {
          issueReads += 1;
          return issueReads;
        },
      }),
    { wrapper }
  );
  const legion = observe(PLANNING.issue);
  const { socket, frame } = await subscribed();
  act(() => socket.receive(snapshotFrame(frame.sub, PLANNING)));
  await waitFor(() => {
    expect(legion.result.current.data).toEqual(PLANNING);
    expect(issue.result.current.data).toBe(1);
  });

  // Not awaited: a second subscription's read would wait for a snapshot no one sends.
  await act(async () => {
    void queryClient.invalidateQueries({ queryKey: ["issue", PLANNING.issue] });
  });
  await waitFor(() => expect(issue.result.current.data).toBe(2));
  expect(socket.subscribes()).toHaveLength(1);
  expect(socket.unsubscribes()).toEqual([]);
  expect(legion.result.current.data).toEqual(PLANNING);
});

test("a configured false body is held as it is: no state, no read and no refusal", async () => {
  const hook = observe(CONFIGURED_FALSE.issue);
  const { socket, frame } = await subscribed();
  expect(frame.view).toEqual({ kind: "legion_issue", issue: CONFIGURED_FALSE.issue });

  act(() => socket.receive(snapshotFrame(frame.sub, CONFIGURED_FALSE)));
  await waitFor(() => expect(hook.result.current.status).toBe("success"));
  expect(hook.result.current.data).toEqual(CONFIGURED_FALSE);
  expect(hook.result.current.error).toBeNull();
});
