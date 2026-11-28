import { afterEach, beforeEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { lazy, type ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { deliverySettings } from "../../__tests__/delivery-settings";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type { LegionSetting, ProjectDeliverySettings } from "../../api/types";
import { EvalsTab } from "./EvalsTab";
import { EvalsTabRoute } from "./EvalsTabRoute";

// The Evals tab's route opens the views of the tab's first render as it matches, while the page's
// code loads (dispatch://INFRA-2918, T33: the tab's first subscribe came 225 ms after the
// navigation), and the tab takes them over once it mounts (holdFirstPaint, releaseFirstPaint).

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

/** `element` at the Evals tab's route of ACME, at SEARCH. */
function renderRoute(element: ReactNode): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[`/projects/ACME/evals${SEARCH}`]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={element} path="/projects/:key/evals" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
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

test("the route opens the views the tab's first render opens, while the page's code has not loaded", async () => {
  const pending = (): Promise<never> => Promise.withResolvers<never>().promise;
  stubReads(pending(), pending());
  // The tab's first render, its daemon and settings reads unanswered: its four evals views and
  // its header's legion-read view.
  renderRoute(<EvalsTab project="ACME" />);
  const tab = viewsOf(await subscribed(5));
  cleanup();

  const NeverLoaded = lazy(() => Promise.withResolvers<{ default: () => ReactNode }>().promise);
  renderRoute(
    <EvalsTabRoute fallback={<p>Loading…</p>}>
      <NeverLoaded />
    </EvalsTabRoute>
  );
  expect(viewsOf(await subscribed(5, 1))).toEqual(tab);
});

test("the tab takes the route's views over once its code loads, and the route's hold goes with its mount", async () => {
  const daemon = Promise.withResolvers<LegionSetting | null>();
  stubReads(daemon.promise, Promise.resolve(deliverySettings("ACME")));
  const page = Promise.withResolvers<{ default: () => ReactNode }>();
  const Later = lazy(() => page.promise);
  renderRoute(
    <EvalsTabRoute fallback={<p>Loading…</p>}>
      <Later />
    </EvalsTabRoute>
  );
  const socket = await subscribed(5);

  // The page's code loads: the tab subscribes none of its views again, and drops none.
  await act(async () => {
    page.resolve({ default: () => <EvalsTab project="ACME" /> });
    await page.promise;
  });
  await waitFor(() => expect(document.querySelector("[data-section]")).not.toBeNull());
  expect(socket.subscribes()).toHaveLength(5);
  expect(socket.unsubscribes()).toHaveLength(0);

  // The project names no daemon: the tab drops its Legion evals view, which the route's hold,
  // gone with the tab's mount, keeps no more.
  const legion = socket
    .subscribes()
    .find(
      (frame) =>
        frame.view.kind === "evals" &&
        new URLSearchParams(frame.view.query).get("kind") === "legion"
    );
  await act(async () => {
    daemon.resolve(null);
    await daemon.promise;
  });
  await waitFor(() =>
    expect(socket.unsubscribes().map((frame) => frame.sub)).toContain(legion?.sub ?? "none")
  );
});
