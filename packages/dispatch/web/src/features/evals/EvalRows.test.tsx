import { afterEach, beforeEach, test as bunTest, expect } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { itemRow, itemsPage } from "../../__tests__/evals-fixture";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import type { LiveSubscribe } from "../../api/types";
import { EvalRows } from "./EvalRows";
import type { EvalsScope } from "./useEvalsViews";

// The rows behind an eval's number (dispatch://INFRA-2918/artifact/plan-md, D21, T39): a logged
// eval's rows are its readings alone (`source=readings`) with no repository, one view at a time,
// paged as a computed eval's rows are; a computed eval's rows keep the header's repositories.

/** The header names a repository, which a logged eval's rows leave out. */
const SCOPE: EvalsScope = {
  compare: null,
  project: "ACME",
  repos: ["acme/widgets"],
  window: { preset: 28 },
};

let sockets: FakeLiveSockets;
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  sockets.restore();
});

/** A row of this file, its page unmounted inside its body. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

/** The eval's rows behind a button that opens and closes them, closed at first unless `open`. */
function Rows({ logged, open = true }: { logged: boolean; open?: boolean }): ReactNode {
  const [shown, setShown] = useState(open);
  return (
    <>
      <button onClick={() => setShown(!shown)} type="button">
        {shown ? "Close the rows" : "Open the rows"}
      </button>
      {shown ? (
        <EvalRows
          label="Rows of widget-size"
          limit={10}
          logged={logged}
          scope={SCOPE}
          slug="widget-size"
        />
      ) : null}
    </>
  );
}

async function renderRows(logged: boolean, open = true): Promise<FakeLiveSocket> {
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <Rows logged={logged} open={open} />
    </QueryClientProvider>
  );
  if (!open) await userEvent.setup().click(screen.getByRole("button", { name: "Open the rows" }));
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** The query of the items view subscribed last. */
function lastQuery(socket: FakeLiveSocket): URLSearchParams {
  const frame: LiveSubscribe = socket.subscribed("items");
  return new URLSearchParams(frame.view.kind === "items" ? frame.view.query : "");
}

/** A reading of `widgets` keyed `key`, of `value`. */
function reading(key: string, value: number) {
  return itemRow(key, "reading", { id: `reading-${key}`, series: "widgets", value });
}

test("a logged eval's rows read readings alone and no repository", async () => {
  const user = userEvent.setup();
  const socket = await renderRows(true);
  const first = await waitFor(() => socket.subscribed("items"));
  const query = lastQuery(socket);
  expect(query.getAll("source")).toEqual(["readings"]);
  expect(query.getAll("repo")).toEqual([]);
  expect([query.get("limit"), query.has("offset"), query.has("buckets")]).toEqual([
    "10",
    false,
    false,
  ]);

  // A page of 10 of its 25 readings, and a step chart's reading before the window, which the
  // read answers on every page and which is no row of the window.
  const before = { ...reading("widget-0", 2), before_window: true };
  const page = Array.from({ length: 10 }, (_, index) => reading(`widget-${index + 1}`, index));
  act(() => socket.receive(snapshotFrame(first.sub, itemsPage([before, ...page], 25))));
  const list = await screen.findByRole("list", { name: "Rows of widget-size" });
  expect(
    [...list.querySelectorAll("[data-eval-row]")].map((row) => row.getAttribute("data-eval-row"))
  ).toEqual(page.map((row) => row.row_key));
  expect(screen.getByText("Rows 1 to 10 of 25")).toBeInTheDocument();

  // Its next page is a view of its own, of readings alone and no repository, the first one left.
  await user.click(screen.getByRole("button", { name: "Later rows" }));
  await waitFor(() => expect(lastQuery(socket).get("offset")).toBe("10"));
  expect(lastQuery(socket).getAll("source")).toEqual(["readings"]);
  expect(lastQuery(socket).getAll("repo")).toEqual([]);
  await waitFor(() => expect(socket.unsubscribes().map((frame) => frame.sub)).toContain(first.sub));
});

test("a computed eval's rows keep the header's repositories and every source", async () => {
  const socket = await renderRows(false);
  await waitFor(() => socket.subscribed("items"));
  expect(lastQuery(socket).getAll("repo")).toEqual(["acme/widgets"]);
  expect(lastQuery(socket).has("source")).toBe(false);
});

// The section state table's row of a logged eval's rows (plan, "The state table of a section").
test("painted, a logged eval's box | its rows opened | subscribed: one rows view (`source=readings`, no repository); closing them unsubscribes it", async () => {
  const socket = await renderRows(true, false);
  const view = await waitFor(() => socket.subscribed("items"));
  expect(socket.subscribes().filter((frame) => frame.view.kind === "items")).toHaveLength(1);
  expect(lastQuery(socket).getAll("source")).toEqual(["readings"]);
  expect(lastQuery(socket).getAll("repo")).toEqual([]);
  act(() => socket.receive(snapshotFrame(view.sub, itemsPage([reading("widget-1", 3)]))));
  const list = await screen.findByRole("list", { name: "Rows of widget-size" });
  expect(list.textContent).toContain("widgets: 3");

  await userEvent.setup().click(screen.getByRole("button", { name: "Close the rows" }));
  await waitFor(() => expect(socket.unsubscribes().map((frame) => frame.sub)).toEqual([view.sub]));
});
