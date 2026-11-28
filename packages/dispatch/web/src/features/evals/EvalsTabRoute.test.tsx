import { afterEach, beforeEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { lazy, type ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { deliverySettings } from "../../__tests__/delivery-settings";
import { evalsAnswer } from "../../__tests__/evals-fixture";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type { LegionSetting, ProjectDeliverySettings } from "../../api/types";
import { EvalsTab } from "./EvalsTab";
import { EvalsTabRoute } from "./EvalsTabRoute";

// The Evals tab's layout route opens the views of the tab's first paint as its child route
// matches, while the page's code loads (dispatch://INFRA-2918, T33: the tab's first subscribe came
// 225 ms after the navigation, and its job lists' after the CI evals view's frame was drawn), and
// the tab and its job lists take them over once they mount (holdFirstPaint, releaseFirstPaint,
// releaseJobList). Each row draws the routes as app.tsx's router declares them.

let sockets: FakeLiveSockets;
const spies: { mockRestore: () => void }[] = [];
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  sockets.restore();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/** A row of this file, its page unmounted inside its body, as EvalsTab.test.tsx's rows are: the
 *  tab's timers are real ones. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

const SEARCH = "?window=84d&repo=acme%2Fwidgets";

/** The project's daemon read and delivery settings read answering what the row hands them, each
 *  read of an eval's detail nothing. */
function stubReads(
  daemon: Promise<LegionSetting | null>,
  settings: Promise<ProjectDeliverySettings | null>
): void {
  spies.push(
    spyOn(api, "getLegionDaemon").mockImplementation(() => daemon),
    spyOn(api, "getProjectDeliverySettings").mockImplementation(() => settings),
    spyOn(api, "listIssues"),
    spyOn(api, "getEval").mockImplementation(() => Promise.withResolvers<never>().promise)
  );
}

/** `routes` at the Evals tab's address of ACME, at SEARCH. */
function renderRoutes(routes: ReactNode): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[`/projects/ACME/evals${SEARCH}`]}>
      <QueryClientProvider client={queryClient}>
        <Routes>{routes}</Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** `page` at the Evals tab's route, under the tab's layout route, as app.tsx's router declares
 *  them. */
function renderRoute(page: ReactNode): void {
  renderRoutes(
    <Route element={<EvalsTabRoute fallback={<p>Loading…</p>} />}>
      <Route element={page} path="/projects/:key/evals" />
    </Route>
  );
}

/** The live socket a render opened after the `before` sockets the row's earlier renders opened,
 *  opened as Dispatch accepts it, once it has sent `count` subscribes. */
async function subscribed(count: number, before = 0): Promise<FakeLiveSocket> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(before));
  const socket = sockets.latest();
  act(() => socket.open());
  await waitFor(() => expect(socket.subscribes().length).toBe(count));
  return socket;
}

/** The views `socket` was sent subscribes of, each as its JSON, in order. */
const viewsOf = (socket: FakeLiveSocket): string[] =>
  socket
    .subscribes()
    .map((frame) => JSON.stringify(frame.view))
    .sort();

/** The subscribe `socket` was sent of the evals view of `kind`. */
function evalsSubscribe(socket: FakeLiveSocket, kind: string) {
  return socket
    .subscribes()
    .find(
      (frame) =>
        frame.view.kind === "evals" && new URLSearchParams(frame.view.query).get("kind") === kind
    );
}

/** The subs of the items views `socket` was sent subscribes of. */
const itemsSubs = (socket: FakeLiveSocket): string[] =>
  socket
    .subscribes()
    .filter((frame) => frame.view.kind === "items")
    .map((frame) => frame.sub);

test("the route opens the views the tab's first paint reads, while the page's code has not loaded", async () => {
  const pending = (): Promise<never> => Promise.withResolvers<never>().promise;
  stubReads(pending(), Promise.resolve(deliverySettings("ACME")));
  // The tab, at the tab's route with no layout route over it, its daemon read unanswered: its first
  // render's four evals views and its header's legion-read view, then, once the CI evals view has
  // answered, its two job lists' items views.
  renderRoutes(<Route element={<EvalsTab project="ACME" />} path="/projects/:key/evals" />);
  const socket = await subscribed(5);
  const ci = evalsSubscribe(socket, "ci");
  act(() => socket.receive(snapshotFrame(ci?.sub ?? "none", evalsAnswer([]))));
  await waitFor(() => expect(socket.subscribes()).toHaveLength(7));
  const tab = viewsOf(socket);
  cleanup();

  const NeverLoaded = lazy(() => Promise.withResolvers<{ default: () => ReactNode }>().promise);
  renderRoute(<NeverLoaded />);
  expect(viewsOf(await subscribed(7, 1))).toEqual(tab);
});

test("the tab takes the route's views over once its code loads, and the route's hold goes with its mount", async () => {
  const daemon = Promise.withResolvers<LegionSetting | null>();
  stubReads(daemon.promise, Promise.resolve(deliverySettings("ACME")));
  const page = Promise.withResolvers<{ default: () => ReactNode }>();
  const Later = lazy(() => page.promise);
  renderRoute(<Later />);
  const socket = await subscribed(7);

  // The page's code loads: the tab subscribes none of its views again, and drops none, its job
  // lists' among them, which its CI section draws only once the CI evals view has answered.
  await act(async () => {
    page.resolve({ default: () => <EvalsTab project="ACME" /> });
    await page.promise;
  });
  await waitFor(() => expect(document.querySelector("[data-section]")).not.toBeNull());
  expect(socket.subscribes()).toHaveLength(7);
  expect(socket.unsubscribes()).toHaveLength(0);

  // The CI evals view answers: the job lists take their views over, and the route's hold of them
  // goes, with no subscribe and no unsubscribe.
  const ci = evalsSubscribe(socket, "ci");
  act(() => socket.receive(snapshotFrame(ci?.sub ?? "none", evalsAnswer([]))));
  await waitFor(() => expect(document.querySelectorAll("[data-job-list]")).toHaveLength(2));
  expect(socket.subscribes()).toHaveLength(7);
  expect(socket.unsubscribes()).toHaveLength(0);

  // The project names no daemon: the tab drops its Legion evals view, which the route's hold,
  // gone with the tab's mount, keeps no more.
  const legion = evalsSubscribe(socket, "legion");
  await act(async () => {
    daemon.resolve(null);
    await daemon.promise;
  });
  await waitFor(() =>
    expect(socket.unsubscribes().map((frame) => frame.sub)).toContain(legion?.sub ?? "none")
  );
});

test("a tab that draws no CI section lets go of the job lists' views the route opened", async () => {
  const pending = (): Promise<never> => Promise.withResolvers<never>().promise;
  stubReads(pending(), Promise.resolve({ ...deliverySettings("ACME"), delivery: false }));
  const page = Promise.withResolvers<{ default: () => ReactNode }>();
  const Later = lazy(() => page.promise);
  renderRoute(<Later />);
  const socket = await subscribed(7);
  expect(itemsSubs(socket)).toHaveLength(2);
  // The page's code loads, and its delivery settings say delivery is off: no CI section draws.
  await act(async () => {
    page.resolve({ default: () => <EvalsTab project="ACME" /> });
    await page.promise;
  });
  await waitFor(() =>
    expect(socket.unsubscribes().map((frame) => frame.sub)).toEqual(
      expect.arrayContaining(itemsSubs(socket))
    )
  );
});
