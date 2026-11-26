import { afterEach, beforeEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import {
  evalItem,
  evalSeries,
  evalsAnswer,
  itemRow,
  itemsPage,
  WINDOW,
} from "../../__tests__/evals-fixture";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type {
  DeliveryLiveDelta,
  EvalDetailResponse,
  EvalItemsResponse,
  EvalLink,
  EvalListItem,
} from "../../api/types";
import { EvalPanelHost, EvalTitle } from "./EvalPanel";
import type { EvalsScope } from "./useEvalsViews";

// The number's panel (dispatch://INFRA-2918/artifact/plan-md, D12, D11a, T16): the eval's method
// word for word, its links and its primary series' links under the method, and the rows behind
// it, each read on a fake live socket, with the detail read answered by a spy.

const SCOPE: EvalsScope = { compare: null, project: "ACME", repos: [], window: { preset: 28 } };

const SLUG = "merge-to-production";

/** A link of relation `rel` to `where` on results.example.com. */
function linkOf(rel: string, where: string): EvalLink {
  return {
    href: `https://results.example.com/${where}/${rel}`,
    label: `${where} ${rel}`,
    recorded_at: "2026-11-01T00:00:00Z",
    rel,
  };
}

/** The eval at `version`, its links and its primary series' links as given. */
function merged(
  version: number,
  links: { eval?: EvalLink[]; series?: EvalLink[] } = {}
): EvalListItem {
  const item = evalItem(
    SLUG,
    "delivery",
    [evalSeries("minutes", "minutes", { is_primary: true, links: links.series ?? [] })],
    version
  );
  return {
    ...item,
    definition: { ...item.definition, links: links.eval ?? [], name: "Merge to production" },
  };
}

let sockets: FakeLiveSockets;
const spies: { mockRestore: () => void }[] = [];
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  sockets.restore();
  for (const spy of spies.splice(0)) spy.mockRestore();
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

/** The detail read, answering the method `method()` names at the time of each read, as the
 *  eval's section of the evals document holds it. */
function answerDetails(method: () => string | null) {
  const getEval = spyOn(api, "getEval").mockImplementation(
    async (): Promise<EvalDetailResponse> => ({
      compare: null,
      doc_text: method(),
      freshness: { refresher_age_seconds: null, rows_updated_at: null },
      items: [],
      limit: 1,
      offset: 0,
      total: 1,
      window: WINDOW,
    })
  );
  spies.push(getEval);
  return getEval;
}

/** A title of the eval inside the panel's host, and the host's live socket, open. */
async function renderTitle(): Promise<FakeLiveSocket> {
  render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <EvalPanelHost kinds={["delivery"]} scope={SCOPE}>
          <EvalTitle slug={SLUG} title="Merge to production" />
        </EvalPanelHost>
      </QueryClientProvider>
    </MemoryRouter>
  );
  await userEvent.setup().click(screen.getByRole("button", { name: "Merge to production" }));
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** Dispatch answers the delivery evals view with `item`, and the eval's rows with `rows`. */
async function serveEval(
  socket: FakeLiveSocket,
  item: EvalListItem,
  rows: EvalItemsResponse = itemsPage([])
): Promise<string> {
  const evals = await waitFor(() => socket.subscribed("evals"));
  act(() => socket.receive(snapshotFrame(evals.sub, evalsAnswer([item]))));
  const items = await waitFor(() => socket.subscribed("items"));
  act(() => socket.receive(snapshotFrame(items.sub, rows)));
  return evals.sub;
}

/** A delta of the evals view `sub` that changes `item`. */
function deltaOf(sub: string, item: EvalListItem): DeliveryLiveDelta {
  return {
    changed_at: null,
    counts: {},
    items: { items: { remove: [], upsert: [item] } },
    maps: {},
    revs: { items: { items: { [item.id]: "00000000000000bb" } } },
    seq: "2",
    set: {},
    sub,
    type: "delta",
  };
}

function panel(): HTMLElement {
  return screen.getByRole("dialog", { name: "Merge to production" });
}

/** The method the panel draws, once it has one. */
function method(): Promise<string | null | undefined> {
  return waitFor(() => {
    const text = panel().querySelector("[data-method-text]");
    if (text === null) throw new Error("the panel draws no method yet");
    return text.textContent;
  });
}

test("the method is the detail's doc_text, word for word", async () => {
  const written =
    "Each merged pull request's minutes from its merge to the deploy that shipped it.\n\n  The median of the window's, *as written*.";
  const getEval = answerDetails(() => written);
  const socket = await renderTitle();
  await serveEval(socket, merged(1));
  expect(await method()).toBe(written);
  expect(getEval.mock.calls).toEqual([["ACME", SLUG, { limit: 1 }]]);
});

test("an open panel reads the detail again when the evals view's delta carries a new version, and shows the new method without a reload", async () => {
  let written = "The first method.";
  const getEval = answerDetails(() => written);
  const socket = await renderTitle();
  const sub = await serveEval(socket, merged(3));
  expect(await method()).toBe("The first method.");
  expect(getEval).toHaveBeenCalledTimes(1);

  // A register write changed the method: the view's delta carries the eval's next version.
  written = "The method as the register write rewrote it.";
  act(() => socket.receive(deltaOf(sub, merged(4))));
  await waitFor(async () => expect(await method()).toBe(written));
  expect(getEval).toHaveBeenCalledTimes(2);
  // The same panel, and the same subscription: nothing was opened again.
  expect(socket.subscribes().filter((frame) => frame.view.kind === "evals")).toHaveLength(1);
});

test("the eval's links and its primary series' links stand under the method", async () => {
  answerDetails(() => "The method.");
  const socket = await renderTitle();
  await serveEval(
    socket,
    merged(1, { eval: [linkOf("describedby", "eval")], series: [linkOf("via", "series")] })
  );
  await method();
  const links = within(panel()).getByRole("list", { name: "Links" });
  expect(
    within(links)
      .getAllByRole("link")
      .map((link) => [link.textContent, link.getAttribute("data-link-kind")])
  ).toEqual([
    ["eval describedby", "describedby"],
    ["series via", "via"],
  ]);
});

test("a row's links stand beside its href, never in its place", async () => {
  answerDetails(() => "The method.");
  const socket = await renderTitle();
  const run = {
    ...itemRow("acme/widgets/501", "run", { conclusion: "success", minutes: 12 }),
    href: "https://ci.example.com/acme/widgets/runs/501",
    links: [linkOf("enclosure", "run-501"), linkOf("related", "run-501")],
  };
  await serveEval(socket, merged(1), itemsPage([run]));
  const row = await waitFor(() => {
    const found = panel().querySelector('[data-eval-row="acme/widgets/501"]');
    if (!(found instanceof HTMLElement)) throw new Error("the panel lists no row yet");
    return found;
  });
  // The row's own address is its key's link, as the items route answers it.
  const own = within(row).getByRole("link", { name: "acme/widgets/501" });
  expect(own).toHaveAttribute("href", "https://ci.example.com/acme/widgets/runs/501");
  // Its links stand beside it, each its own link.
  const links = within(row).getByRole("list", { name: "Links of acme/widgets/501" });
  expect(
    within(links)
      .getAllByRole("link")
      .map((link) => [link.getAttribute("href"), link.getAttribute("data-link-kind")])
  ).toEqual([
    ["https://results.example.com/run-501/enclosure", "enclosure"],
    ["https://results.example.com/run-501/related", "plain"],
  ]);
  expect(links).not.toContainElement(own);
  // A computed row, so it is no reading as logged.
  expect(row).not.toHaveAttribute("data-reading");
});

test("a reading as logged shows its value and the links it carried", async () => {
  answerDetails(() => "The method.");
  const socket = await renderTitle();
  const reading = {
    ...itemRow("widget-1", "row", {
      id: "reading-1",
      n: 40,
      n_unit: "runs",
      note: "measured on the nightly build",
      series: "minutes",
      unit: "minutes",
      value: 3,
    }),
    href: "https://results.example.com/reading-1/via",
    links: [linkOf("via", "reading-1"), linkOf("source-rule", "reading-1")],
    source: "nightly-bench",
  };
  await serveEval(socket, merged(1), itemsPage([reading]));
  const row = await waitFor(() => {
    const found = panel().querySelector('[data-reading="row"]');
    if (!(found instanceof HTMLElement)) throw new Error("the panel lists no reading yet");
    return found;
  });
  expect(row.textContent).toContain("minutes: 3 minutes of 40 runs");
  expect(row.textContent).toContain("measured on the nightly build");
  expect(row.textContent).toContain("source nightly-bench");
  expect(
    within(within(row).getByRole("list", { name: "Links of widget-1" }))
      .getAllByRole("link")
      .map((link) => link.getAttribute("data-link-kind"))
  ).toEqual(["via", "source-rule"]);
});

test("a number with no links draws no link area", async () => {
  answerDetails(() => "The method.");
  const socket = await renderTitle();
  await serveEval(socket, merged(1), itemsPage([itemRow("acme/widgets/7", "pull_request", {})]));
  await method();
  await waitFor(() => expect(panel().querySelector("[data-eval-row]")).not.toBeNull());
  expect(within(panel()).queryByRole("list", { name: "Links" })).toBeNull();
  expect(within(panel()).queryByRole("list", { name: /^Links of/ })).toBeNull();
});
