import { afterEach, beforeEach, expect, test } from "bun:test";
import { LIVE_SOCKET_MAX_AGE_CLOSE } from "@legion/contracts/dispatch-api-constants";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import {
  errorFrame,
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { liveSocketFor } from "../../api/live-socket";
import type { DeliveryLiveBody, DeliveryLiveView } from "../../api/types";
import { useAgentStream } from "./useAgentStream";

const PLANNER = "01a0e090-4848-7473-acc5-fc96e6a646d3";
const REVIEWER = "01a0e090-4848-7473-acc5-fc96e6a646d4";

let sockets: FakeLiveSockets;
let queryClient: QueryClient;

beforeEach(() => {
  sockets = installFakeLiveSockets();
  queryClient = new QueryClient();
});

afterEach(() => {
  cleanup();
  liveSocketFor(queryClient).close();
  sockets.restore();
});

/** `useAgentStream` on the tab's socket, watching the session `rerender` names. `onRender` is
 *  told each time the hook's component renders. */
function renderStream(session: string, onRender: () => void = () => {}) {
  return renderHook(
    ({ id }: { id: string }) => {
      onRender();
      return useAgentStream(id);
    },
    {
      initialProps: { id: session },
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
      ),
    }
  );
}

/** The id of `session`'s live view subscription, as the page last sent it on `socket`. */
function subscription(socket: FakeLiveSocket, session: string): string {
  const sent = socket
    .subscribes()
    .filter((frame) => frame.view.kind === "agent" && frame.view.session_id === session)
    .at(-1);
  if (sent === undefined) throw new Error(`no live view of ${session} was subscribed`);
  return sent.sub;
}

/** Dispatch answering `session`'s subscription on `socket` with the session's replay, as it does
 *  each time the page sends the subscription. */
function replay(socket: FakeLiveSocket, session: string): void {
  act(() =>
    socket.receive({
      type: "agent",
      sub: subscription(socket, session),
      event: "replay",
      data: { frames: [], session_id: session, v: 1 },
    })
  );
}

/** Dispatch passing on the relay's answer to whether `session` answers on its control subject. */
function responder(socket: FakeLiveSocket, session: string, responding: boolean): void {
  act(() =>
    socket.receive({
      type: "agent",
      sub: subscription(socket, session),
      event: "responder",
      data: { responding, v: 1 },
    })
  );
}

// `replays` is what tells the transcript it was rebuilt, so it counts the first subscription's
// replay and the one that follows each time the socket sends the subscription again - here after
// Dispatch rotated the socket at its age, which the page answers by reopening at once.
test("replays counts the first replay and the one after a rotation", () => {
  const { result } = renderStream(PLANNER);
  const first = sockets.latest();
  act(() => first.open());
  expect(result.current.replays).toBe(0);

  replay(first, PLANNER);
  expect(result.current).toMatchObject({ replays: 1, status: "live" });

  act(() => first.drop(LIVE_SOCKET_MAX_AGE_CLOSE));
  const second = sockets.latest();
  expect(second).not.toBe(first);
  act(() => second.open());
  expect(result.current.replays).toBe(1);
  replay(second, PLANNER);
  expect(result.current).toMatchObject({ replays: 2, status: "live" });
});

// The hook watches the session it is given now: another id lets the old subscription go and sends
// one for the new session, whose view starts over - nothing heard, nothing refused - so a session
// Dispatch refused reads as connecting again when the page comes back to it.
test("another session id unsubscribes the old view, subscribes the new one, and starts it afresh", () => {
  const { rerender, result } = renderStream(PLANNER);
  const socket = sockets.latest();
  act(() => socket.open());
  replay(socket, PLANNER);
  expect(result.current.status).toBe("live");
  const planner = subscription(socket, PLANNER);

  rerender({ id: REVIEWER });
  expect(socket.unsubscribes()).toEqual([{ type: "unsubscribe", sub: planner }]);
  expect(socket.subscribed("agent").view).toEqual({ kind: "agent", session_id: REVIEWER });
  expect(result.current).toMatchObject({ replays: 0, status: "connecting" });

  act(() =>
    socket.receive(
      errorFrame(subscription(socket, REVIEWER), "STREAM_UNAVAILABLE", "the relay is down")
    )
  );
  expect(result.current.status).toBe("unavailable");

  rerender({ id: PLANNER });
  expect(socket.subscribed("agent").view).toEqual({ kind: "agent", session_id: PLANNER });
  expect(result.current).toMatchObject({ replays: 0, status: "connecting" });
});

// The relay's word that nobody answers the session's control subject is a status of its own, and
// the session speaking again - any frame, or a later answer of `true` - takes it back to live
// without a reconnect. A lost socket outranks it, and a refusal outranks everything.
test("a session nobody answers reads not-answering until it speaks, under a lost socket and a refusal", async () => {
  const { result } = renderStream(PLANNER);
  const first = sockets.latest();
  act(() => first.open());

  responder(first, PLANNER, false);
  expect(result.current).toMatchObject({ responding: false, status: "not-answering" });

  act(() =>
    first.receive({
      type: "agent",
      sub: subscription(first, PLANNER),
      event: "frame",
      data: { kind: "context-cleared", seq: 1, v: 1 },
    })
  );
  expect(result.current).toMatchObject({ responding: true, status: "live" });

  responder(first, PLANNER, false);
  expect(result.current.status).toBe("not-answering");
  responder(first, PLANNER, true);
  expect(result.current).toMatchObject({ responding: true, status: "live" });

  responder(first, PLANNER, false);
  act(() => first.restart());
  expect(result.current.status).toBe("reconnecting");
  await waitFor(() => expect(sockets.latest()).not.toBe(first));
  const second = sockets.latest();
  act(() => second.open());
  expect(result.current.status).toBe("not-answering");

  // Refused, the view lets its subscription go and the socket, holding nothing else, closes a
  // microtask later: the act awaits that too.
  await act(async () =>
    second.receive(
      errorFrame(subscription(second, PLANNER), "STREAM_UNAVAILABLE", "the relay is down")
    )
  );
  expect(result.current).toMatchObject({ responding: false, status: "unavailable" });
});

// The socket's state also carries what the delivery pages show - the last change time, how long
// it has been lost - which moves with every delivery delta. The live view reads only whether the
// socket is connected, so none of that re-renders it.
test("a change to the socket's state other than its connection re-renders nothing", () => {
  let renders = 0;
  renderStream(PLANNER, () => {
    renders += 1;
  });
  const socket = sockets.latest();
  act(() => socket.open());
  const live = liveSocketFor(queryClient);
  const view = {
    kind: "timeline",
    query: "",
    window: { preset: 28 },
  } satisfies DeliveryLiveView;
  void live.subscribe(["delivery", "timeline", view], view);
  const before = renders;

  act(() =>
    socket.receive(
      snapshotFrame(socket.subscribed("timeline").sub, {} as DeliveryLiveBody, {
        changedAt: "2026-10-24T10:00:00Z",
      })
    )
  );
  expect(live.state().changedAt).toBe("2026-10-24T10:00:00Z");
  expect(renders).toBe(before);
});
