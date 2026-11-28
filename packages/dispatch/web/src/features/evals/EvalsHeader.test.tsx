import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ComponentProps, ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import {
  errorFrame,
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import type { DeliveryLiveDelta, LegionReadResponse } from "../../api/types";
import { EvalsHeader } from "./EvalsHeader";
import { useEvalsUrlState } from "./evals-url";

let sockets: FakeLiveSockets | undefined;

afterEach(() => {
  cleanup();
  sockets?.restore();
  sockets = undefined;
});

/** The address the header wrote, as its query string. */
function Search(): ReactNode {
  return <output data-testid="search">{useLocation().search}</output>;
}

function currentSearch(): URLSearchParams {
  return new URLSearchParams(screen.getByTestId("search").textContent ?? "");
}

/** The header as the tab draws it, over the URL's state and writing it back. */
function Header(props: Omit<ComponentProps<typeof EvalsHeader>, "state" | "onChange">): ReactNode {
  const [state, setState] = useEvalsUrlState();
  return <EvalsHeader {...props} onChange={setState} state={state} />;
}

function renderHeader(
  search = "",
  props: Omit<ComponentProps<typeof EvalsHeader>, "state" | "onChange"> = {}
) {
  render(
    <MemoryRouter initialEntries={[`/projects/ACME/evals${search}`]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Header {...props} />
        <Search />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The page's live socket, once the header has opened one, as Dispatch accepts it. */
async function openSocket(live: FakeLiveSockets): Promise<FakeLiveSocket> {
  await waitFor(() => expect(live.sockets.length).toBeGreaterThan(0));
  const socket = live.latest();
  act(() => socket.open());
  return socket;
}

/** The header's daemon read line, once it draws one. */
function daemonLine(): Promise<HTMLElement> {
  return waitFor(() => {
    const line = document.querySelector<HTMLElement>("[data-daemon-read]");
    if (line === null) throw new Error("the header draws no daemon read line");
    return line;
  });
}

/** A moment `minutes` and ten seconds before now, as Dispatch writes a read's time: ten seconds
 *  past the minute boundary, so `formatAge` reads `<minutes> min ago` even against a `now` captured
 *  when the header mounted milliseconds earlier. */
const minutesAgo = (minutes: number) =>
  new Date(Date.now() - (minutes * 60 + 10) * 1000).toISOString();

/** The header of a project with a daemon, its daemon read view subscribed and answered `read`. */
async function paintedWith(
  read: LegionReadResponse
): Promise<{ socket: FakeLiveSocket; sub: string }> {
  const live = installFakeLiveSockets();
  sockets = live;
  renderHeader("", { daemon: "ACME" });
  const socket = await openSocket(live);
  const { sub, view } = await waitFor(() => socket.subscribed("legion-read"));
  expect(view).toEqual({ kind: "legion-read", project: "ACME" });
  act(() => socket.receive(snapshotFrame(sub, read)));
  return { socket, sub };
}

/** Dispatch's delta of the daemon read view `sub`, setting `set`. */
function readDelta(sub: string, set: Partial<LegionReadResponse>, seq: string): DeliveryLiveDelta {
  return {
    changed_at: null,
    counts: {},
    items: {},
    maps: {},
    revs: {
      fields: Object.fromEntries(Object.keys(set).map((field) => [field, "00000000000000bb"])),
    },
    seq,
    set,
    sub,
    type: "delta",
  };
}

test("each control writes the URL: the window, the compare range and the repositories", async () => {
  const user = userEvent.setup();
  renderHeader();
  const header = screen.getByRole("group", { name: "Window and filters" });

  await user.selectOptions(within(header).getByLabelText("Window"), "84d");
  expect(currentSearch().get("window")).toBe("84d");

  await user.selectOptions(within(header).getByLabelText("Compare with"), "off");
  expect(currentSearch().get("compare")).toBe("off");

  await user.selectOptions(within(header).getByLabelText("Compare with"), "custom");
  await user.click(within(header).getByLabelText("Compare from"));
  await user.paste("2026-01-01");
  await user.click(within(header).getByLabelText("Compare to"));
  await user.paste("2026-01-28");
  await user.click(within(header).getAllByRole("button", { name: "Apply" })[0] as HTMLElement);
  expect(currentSearch().get("compare")).toBeNull();
  expect(currentSearch().get("compare_from")).toBe("2026-01-01T00:00:00.000Z");
  expect(currentSearch().get("compare_to")).toBe("2026-01-29T00:00:00.000Z");

  await user.selectOptions(within(header).getByLabelText("Compare with"), "previous");
  expect(currentSearch().has("compare_from")).toBe(false);
  expect(currentSearch().has("compare")).toBe(false);

  await user.type(within(header).getByLabelText("Repository"), "acme/widgets{Enter}");
  await user.type(within(header).getByLabelText("Repository"), "acme/gadgets{Enter}");
  expect(currentSearch().getAll("repo")).toEqual(["acme/widgets", "acme/gadgets"]);
  await user.click(within(header).getByRole("button", { name: "Remove acme/widgets" }));
  expect(currentSearch().getAll("repo")).toEqual(["acme/gadgets"]);
  // The window kept through every other control's write.
  expect(currentSearch().get("window")).toBe("84d");
});

test("the default compare range is the days before the window, which the URL names by naming nothing", () => {
  renderHeader("?window=14d");
  const compare = screen.getByLabelText("Compare with") as HTMLSelectElement;
  expect(compare.value).toBe("previous");
  expect(compare.selectedOptions[0]?.textContent).toBe("The days before the window");
  expect(currentSearch().toString()).toBe("window=14d");
});

test("a malformed compare range in the URL is dropped, leaving the default", () => {
  renderHeader("?compare_from=not-a-date&compare_to=2026-01-01T00%3A00%3A00Z&window=7d");
  expect((screen.getByLabelText("Compare with") as HTMLSelectElement).value).toBe("previous");
  expect(currentSearch().toString()).toBe("window=7d");
});

test("a preset shows the days Dispatch counts, the answer's from and to, and the compare range's", () => {
  renderHeader("?window=28d", {
    compare: { days: 29, from: "2026-09-27T00:00:00Z", to: "2026-10-26T00:00:00Z" },
    window: { days: 29, from: "2026-10-26T00:00:00Z", to: "2026-11-24T00:00:00Z" },
  });
  expect(screen.getByTestId("evals-days").textContent).toBe(
    "Window 2026-10-26 to 2026-11-23 (UTC), 29 days; compared with 2026-09-27 to 2026-10-25 (UTC), 29 days"
  );
  cleanup();

  renderHeader("?window=7d&compare=off", {
    compare: null,
    window: { days: 8, from: "2026-11-16T00:00:00Z", to: "2026-11-24T00:00:00Z" },
  });
  expect(screen.getByTestId("evals-days").textContent).toBe(
    "Window 2026-11-16 to 2026-11-23 (UTC), 8 days; compared with nothing"
  );
});

test("no days are named before an answer lands, and no freshness line where Dispatch sent none", () => {
  renderHeader();
  expect(screen.queryByTestId("evals-days")).toBeNull();
  expect(screen.queryByRole("region", { name: "Source freshness" })).toBeNull();
  cleanup();

  renderHeader("", {
    freshness: {
      freshness: {
        last_error: null,
        last_event_at: "2026-11-23T10:00:00Z",
        last_reconcile_at: "2026-11-23T10:00:00Z",
        unfetchable_count: 0,
      },
      readAtMs: Date.parse("2026-11-23T10:00:00Z"),
    },
  });
  expect(screen.getByRole("region", { name: "Source freshness" })).toBeTruthy();
  // Unmounted in the row: the line ticks every second on a real timer, which must not fire
  // between the row and its afterEach.
  cleanup();
});

// The state table's daemon-read rows (dispatch://INFRA-2918/artifact/plan-md, "The state table of a
// section", plan D23): the header's line on the project's Legion daemon, from the `legion-read`
// view, in the sources' freshness words and colours, the read's time in full in its title and in
// `data-read-at`. Each row unmounts its header before it ends, since the line ticks every second
// on a real timer, which must not fire between the row and its afterEach.

test("subscribed, a project with a daemon | the legion-read snapshot of a good read | painted: the header's \"Daemon read <age>\", the read's time in data-read-at", async () => {
  const readAt = minutesAgo(5);
  await paintedWith({ error: null, ok: true, read_at: readAt });
  const line = await daemonLine();
  expect(line.textContent).toBe("Daemon read 5 min ago");
  expect(line.getAttribute("data-read-at")).toBe(readAt);
  expect(line.getAttribute("data-red")).toBe("false");
  expect(line.getAttribute("title")).toBe(`Legion daemon: last read at ${readAt}`);
  cleanup();
});

test('painted | the legion-read delta of a failed whole read | painted: "Daemon read failed <age>: <error>"', async () => {
  const { socket, sub } = await paintedWith({ error: null, ok: true, read_at: minutesAgo(5) });
  expect((await daemonLine()).textContent).toBe("Daemon read 5 min ago");

  const failedAt = minutesAgo(2);
  const failure = "the daemon answered 503 Service Unavailable";
  act(() => socket.receive(readDelta(sub, { error: failure, ok: false, read_at: failedAt }, "2")));
  await waitFor(() =>
    expect(document.querySelector("[data-daemon-read]")?.textContent).toBe(
      `Daemon read failed 2 min ago: ${failure}`
    )
  );
  const line = await daemonLine();
  expect(line.getAttribute("data-read-at")).toBe(failedAt);
  expect(line.getAttribute("data-red")).toBe("true");
  expect(line.getAttribute("title")).toBe(
    `Legion daemon: last read at ${failedAt}; failed: ${failure}`
  );
  cleanup();
});

test("painted, the last whole read failed | the delta of a batch applied while the read is owed | painted: still the failed read", async () => {
  const failure = "the daemon answered 503 Service Unavailable";
  const { socket, sub } = await paintedWith({ error: failure, ok: false, read_at: minutesAgo(10) });
  expect((await daemonLine()).textContent).toBe(`Daemon read failed 10 min ago: ${failure}`);

  // A batch while the read is owed moves the read's time alone (readDaemonRead's read_owed).
  const batchAt = minutesAgo(1);
  act(() => socket.receive(readDelta(sub, { read_at: batchAt }, "2")));
  await waitFor(() =>
    expect(document.querySelector("[data-daemon-read]")?.getAttribute("data-read-at")).toBe(batchAt)
  );
  const line = await daemonLine();
  expect(line.textContent).toBe(`Daemon read failed 1 min ago: ${failure}`);
  expect(line.getAttribute("data-red")).toBe("true");
  cleanup();
});

test("a daemon Dispatch never read is named so, in red, with no time", async () => {
  await paintedWith({ error: null, ok: false, read_at: null });
  const line = await daemonLine();
  expect(line.textContent).toBe("Daemon never read");
  expect(line.getAttribute("data-red")).toBe("true");
  expect(line.hasAttribute("data-read-at")).toBe(false);
  cleanup();
});

test("no daemon read line for a project with no daemon", async () => {
  const live = installFakeLiveSockets();
  sockets = live;
  // The tab names no daemon for a project whose daemon read answered none: no view, no line.
  renderHeader("", {
    window: { days: 29, from: "2026-10-26T00:00:00Z", to: "2026-11-24T00:00:00Z" },
  });
  expect(screen.getByTestId("evals-days")).toBeTruthy();
  expect(document.querySelector("[data-daemon-read]")).toBeNull();
  expect(live.sockets).toEqual([]);
  cleanup();

  // Until that read answers the tab opens the view, which Dispatch refuses for a project that
  // names no daemon: still no line.
  renderHeader("", { daemon: "ACME" });
  const socket = await openSocket(live);
  const { sub } = await waitFor(() => socket.subscribed("legion-read"));
  act(() =>
    socket.receive(
      errorFrame(
        sub,
        "LEGION_NOT_CONFIGURED",
        "this project has no Legion daemon; set one through PUT /api/v1/projects/{key}/legion/daemon"
      )
    )
  );
  expect(document.querySelector("[data-daemon-read]")).toBeNull();
  cleanup();
});
