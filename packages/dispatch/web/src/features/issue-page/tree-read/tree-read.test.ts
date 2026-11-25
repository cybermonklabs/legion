import { afterEach, beforeEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { http } from "msw";

import {
  type FakeLiveSockets,
  installFakeLiveSockets,
} from "../../../__tests__/live-socket-harness";
import { answerRequests } from "../../../__tests__/network";
import { liveFeedUp, liveSocketFor, onLiveFeedBack } from "../../../api/live-socket";
import { treeAnswer, treeNode } from "./tree-fixtures";
import { readNextTreePage, viewKey, viewQuery } from "./tree-read";

/**
 * How the tree read asks whether the live feed is up (`liveFeedUp`), which it does on a window
 * focus and as the hold of the issue's writes ends (`subscribeTreeFreshness`): up only while the
 * client's socket is open and Dispatch has started the event feed's subscription on it
 * (`events_start`) since the page last sent it, so that while Dispatch cannot start the feed with
 * the socket still open - its database not answering - the page reads the feed down and
 * refreshes the tree as it would with no feed at all. And how it hears the feed come back after a
 * gap (`onLiveFeedBack`), with whether Dispatch replayed the gap whole: a whole replay reads a
 * refused tree again. Neither opens a socket. Each case drives the page's own live socket over
 * the fake one the test installs, as Dispatch would.
 */

let sockets: FakeLiveSockets;
let client: QueryClient;

beforeEach(() => {
  jest.useFakeTimers();
  sockets = installFakeLiveSockets();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
});

afterEach(() => {
  client.clear();
  sockets.restore();
  jest.useRealTimers();
});

/** The client's live socket with the event feed subscribed on it, as the shell's event feed holds
 *  it; ends both once `run` is done. */
function withFeed(run: (stop: () => void) => void): void {
  const live = liveSocketFor(client);
  const stop = live.subscribeEvents({
    onEvent: () => undefined,
    onStart: () => undefined,
    since: () => null,
  });
  try {
    run(stop);
  } finally {
    stop();
    live.close();
  }
}

test("the live feed reads down while its socket is not open", () => {
  // Each reconnect waits the middle of its spread.
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  try {
    withFeed(() => {
      expect(liveFeedUp(client), "a socket still connecting").toBe(false);
      sockets.latest().open();
      sockets.latest().drop(1006);
      expect(liveFeedUp(client), "a socket lost").toBe(false);
    });
  } finally {
    random.mockRestore();
  }
});

test("the live feed reads down while Dispatch has ended its subscription on a socket still open", () => {
  // The backoff before the page sends the feed again: 1 s, at the middle of its spread.
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  try {
    withFeed(() => {
      const socket = sockets.latest();
      socket.open();
      const { sub } = socket.subscribed("events");
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: false });
      // Dispatch ends the feed after its start, the page sends it again at once, and Dispatch
      // ends that send too with no start: the page waits out a backoff, the socket open.
      socket.receive({ type: "ended", sub, reason: "behind" });
      socket.receive({ type: "ended", sub, reason: "behind" });
      expect(socket.readyState, "the socket stays open").toBe(1);
      expect(liveFeedUp(client), "the feed ended and not sent again").toBe(false);
      jest.advanceTimersByTime(1_000);
      expect(socket.subscribes(), "the feed sent again after the backoff").toHaveLength(3);
      expect(liveFeedUp(client), "the feed sent again, not started yet").toBe(false);
      // Dispatch refusing that send is the same: the page waits out a longer backoff, 2 s.
      socket.receive({ type: "error", sub, code: "STREAM_UNAVAILABLE", message: "no feed" });
      expect(liveFeedUp(client), "the feed refused").toBe(false);
      jest.advanceTimersByTime(2_000);
      expect(socket.subscribes(), "the feed sent again after the refusal").toHaveLength(4);
      expect(liveFeedUp(client), "the feed sent again, still not started").toBe(false);
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: false });
      expect(liveFeedUp(client), "the feed Dispatch started again").toBe(true);
    });
  } finally {
    random.mockRestore();
  }
});

// Dispatch can refuse the feed after it started it, the socket staying open: it then holds no
// subscription, and the page sends the feed again only after a backoff. From the refusal until
// Dispatch starts the next send, no event can arrive, so the feed is down.
test("the live feed reads down once Dispatch refuses a subscription it had started", () => {
  // The backoff before the page sends the feed again: 1 s, at the middle of its spread.
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  try {
    withFeed(() => {
      const socket = sockets.latest();
      socket.open();
      const { sub } = socket.subscribed("events");
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: false });
      expect(liveFeedUp(client), "the feed Dispatch started").toBe(true);
      socket.receive({ type: "error", sub, code: "STREAM_UNAVAILABLE", message: "no feed" });
      expect(socket.readyState, "the socket stays open").toBe(1);
      expect(socket.subscribes(), "the feed not sent again yet").toHaveLength(1);
      expect(liveFeedUp(client), "the started feed refused, its next send waiting").toBe(false);
    });
  } finally {
    random.mockRestore();
  }
});

test("the live feed reads up once Dispatch has started its subscription on the open socket", () => {
  // The reconnect after a lost socket waits the middle of its first attempt's spread: 1 s.
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  try {
    withFeed(() => {
      const socket = sockets.latest();
      socket.open();
      expect(liveFeedUp(client), "the feed sent on the open socket, not started yet").toBe(false);
      const { sub } = socket.subscribed("events");
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: false });
      expect(liveFeedUp(client), "the feed started").toBe(true);
      // An end after a start is sent again at once, which is down until Dispatch starts it.
      socket.receive({ type: "ended", sub, reason: "behind" });
      expect(socket.subscribes(), "the feed sent again at once").toHaveLength(2);
      expect(liveFeedUp(client), "the feed sent again, not started yet").toBe(false);
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: true });
      expect(liveFeedUp(client), "the feed started again").toBe(true);
      // A socket lost after a start: the next one sends the feed again, down until its start.
      socket.drop(1006);
      jest.advanceTimersByTime(1_000);
      const next = sockets.latest();
      expect(next, "the socket opened after the loss").not.toBe(socket);
      next.open();
      expect(liveFeedUp(client), "the feed sent on the next socket, not started yet").toBe(false);
      next.receive({ type: "events_start", sub, since: "30", replay_complete: true });
      expect(liveFeedUp(client), "the feed started on the next socket").toBe(true);
    });
  } finally {
    random.mockRestore();
  }
});

test("asking how the live feed stands opens no socket", () => {
  expect(liveFeedUp(client), "a client with no socket").toBe(false);
  expect(sockets.sockets, "the sockets the page opened").toHaveLength(0);
  // A live socket made for the client, even one with nothing to send, watches its cache.
  expect(client.getQueryCache().hasListeners(), "a live socket made for the client").toBe(false);
  withFeed((stop) => {
    sockets.latest().open();
    stop();
    expect(liveFeedUp(client), "a socket that holds no event feed").toBe(false);
  });
});

test("a listener hears the live feed start again after each gap, with whether Dispatch replayed the gap whole", () => {
  // The reconnect after a lost socket waits the middle of its first attempt's spread: 1 s.
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const heard: boolean[] = [];
  const stopListening = onLiveFeedBack(client, (replayComplete) => {
    heard.push(replayComplete);
  });
  try {
    withFeed(() => {
      const socket = sockets.latest();
      socket.open();
      const { sub } = socket.subscribed("events");
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: true });
      expect(heard, "the subscription's first start, which follows no gap").toEqual([]);
      // Dispatch ends the feed after its start, and starts the send that follows with the gap
      // replayed in part.
      socket.receive({ type: "ended", sub, reason: "behind" });
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: false });
      expect(heard, "the start after Dispatch ended the feed").toEqual([false]);
      // The socket is lost, and Dispatch starts the feed on the next one with the gap replayed
      // whole.
      socket.drop(1006);
      jest.advanceTimersByTime(1_000);
      const next = sockets.latest();
      expect(next, "the socket opened after the loss").not.toBe(socket);
      next.open();
      next.receive({ type: "events_start", sub, since: "30", replay_complete: true });
      expect(heard, "the start on the next socket").toEqual([false, true]);
    });
  } finally {
    stopListening();
    random.mockRestore();
  }
});

test("a listener removed hears the live feed come back no more", () => {
  // The reconnect after a lost socket waits the middle of its first attempt's spread: 1 s.
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  const heard: boolean[] = [];
  const stopListening = onLiveFeedBack(client, (replayComplete) => {
    heard.push(replayComplete);
  });
  try {
    withFeed(() => {
      const socket = sockets.latest();
      socket.open();
      const { sub } = socket.subscribed("events");
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: true });
      socket.receive({ type: "ended", sub, reason: "behind" });
      socket.receive({ type: "events_start", sub, since: "30", replay_complete: true });
      expect(heard, "the start after Dispatch ended the feed").toEqual([true]);

      stopListening();
      socket.drop(1006);
      jest.advanceTimersByTime(1_000);
      const next = sockets.latest();
      next.open();
      next.receive({ type: "events_start", sub, since: "30", replay_complete: true });
      expect(heard, "no start heard once the listener is removed").toEqual([true]);
    });
  } finally {
    stopListening();
    random.mockRestore();
  }
});

test("listening for the live feed coming back opens no socket", () => {
  const stopListening = onLiveFeedBack(client, () => undefined);
  try {
    expect(sockets.sockets, "the sockets the page opened").toHaveLength(0);
    // A live socket made for the client, even one with nothing to send, watches its cache.
    expect(client.getQueryCache().hasListeners(), "a live socket made for the client").toBe(false);
  } finally {
    stopListening();
  }
});

// A view read at a version asks the route for the view at that version (`?view=<view>&v=<n>`) on
// each page it reads: its first, the next one a press reads, and each one the walk of a read made
// again reads to get as far. It is a read of its own in the cache, under the view's key, beside
// the view's read at no version, which sends no `v` and keeps the view's own key. The route is
// answered where the page's request reaches the network, so each case reads what was sent.
test("a view read at a version sends v with the view, and none without one", async () => {
  const key = "CORE-1";
  const first = treeAnswer([treeNode("c1", "comment", key)], [], { cursor: "page-2" });
  const second = treeAnswer([treeNode("c2", "comment", key)], [], { cursor: null });
  const current = treeAnswer([treeNode("c9", "comment", key)], [], { cursor: null });
  const sent: string[] = [];
  answerRequests(
    http.get("/api/v1/issues/:key/tree", ({ request }) => {
      const asked = new URL(request.url).searchParams;
      sent.push(asked.toString());
      if (!asked.has("v")) return Response.json(current);
      return Response.json(asked.has("cursor") ? second : first);
    })
  );
  const atVersion = viewQuery(key, "spec", 3);
  const atNone = viewQuery(key, "spec");
  expect<unknown>(atVersion.queryKey, "the read at a version").toEqual([
    ...viewKey(key, "spec"),
    { v: 3 },
  ]);
  expect<unknown>(atNone.queryKey, "the read at no version").toEqual(viewKey(key, "spec"));

  await client.fetchQuery(atVersion);
  expect(sent.splice(0), "the first page at the version").toEqual(["view=spec&v=3"]);
  await readNextTreePage(client, key, "spec", 3);
  expect(sent.splice(0), "the next page the press reads").toEqual(["view=spec&v=3&cursor=page-2"]);
  await client.refetchQueries({ exact: true, queryKey: atVersion.queryKey });
  expect(sent.splice(0), "the read made again, as far as the press read").toEqual([
    "view=spec&v=3",
    "view=spec&v=3&cursor=page-2",
  ]);
  await client.fetchQuery(atNone);
  expect(sent.splice(0), "the read at no version").toEqual(["view=spec"]);

  const views = client.getQueryCache().findAll({ queryKey: viewKey(key, "spec") });
  expect(
    views.map((view) => view.queryKey),
    "two reads under the view's key"
  ).toEqual([atVersion.queryKey, atNone.queryKey]);
  expect(
    client.getQueryData(atVersion.queryKey)?.nodes?.map((node) => node.id),
    "the read at the version"
  ).toEqual(["c1", "c2"]);
  expect(
    client.getQueryData(atNone.queryKey)?.nodes?.map((node) => node.id),
    "the read at no version"
  ).toEqual(["c9"]);
});
