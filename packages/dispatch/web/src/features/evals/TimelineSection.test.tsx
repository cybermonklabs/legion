import { afterEach, beforeEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";

import { deliverySettings } from "../../__tests__/delivery-settings";
import { evalItem, evalsAnswer, WINDOW } from "../../__tests__/evals-fixture";
import {
  type IntersectionControl,
  installIntersectionObserver,
} from "../../__tests__/intersection-observer";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import {
  limitOf,
  mergeRow,
  runRow,
  timelineAnswer,
  timelineGrowth,
  timelineRows,
  timelineSnapshot,
} from "../../__tests__/timeline-fixture";
import { api } from "../../api/client";
import type { EvalKind } from "../../api/queries";
import type { LegionSetting, LiveSubscribe, ProjectTimelineRow } from "../../api/types";
import * as timeline from "../delivery/Timeline";
import { EvalsTab } from "./EvalsTab";

// The merge timeline on the Evals tab (dispatch://INFRA-2918/artifact/plan-md, T14; spec R13, R6,
// R7): closed until it is opened, one project-timeline view that grows by its limit on the one
// subscription, the brush and the Repository facet setting the target cards' days and repository
// (decision 3), the table mode, and no compare range. The chart draws on a canvas the unit DOM
// cannot, so `Timeline` is drawn by a stand-in that records the rows it is handed and brushes on a
// press.

const DAEMON: LegionSetting = {
  created_by: { id: "alice", kind: "user" },
  project: "ACME",
  state_url: "http://127.0.0.1:9",
  updated_at: "2026-10-01T00:00:00Z",
};

/** A brush dragged over part of three UTC days. */
const BRUSH = { end: "2026-11-04T12:00:00.000Z", start: "2026-11-02T06:00:00.000Z" };

/** The rows the stand-in chart was last handed, merges and runs. */
let drawn: { prs: readonly { id: string }[]; runs: readonly { id: number }[] } | undefined;

let sockets: FakeLiveSockets;
let observers: IntersectionControl;
const restores: (() => void)[] = [];
let search = "";

beforeEach(() => {
  sockets = installFakeLiveSockets();
  observers = installIntersectionObserver();
  drawn = undefined;
  const chart = spyOn(timeline, "Timeline").mockImplementation((props) => {
    drawn = { prs: props.prs, runs: props.runs };
    return (
      <div data-testid="delivery-chart">
        <button onClick={() => props.onBrush(BRUSH)} type="button">
          Brush
        </button>
      </div>
    );
  });
  const daemon = spyOn(api, "getLegionDaemon").mockResolvedValue(DAEMON);
  const settings = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(
    deliverySettings("ACME")
  );
  // The tab reads the detail of each eval no section draws by name, which `answerEvals`' are, to
  // learn whether it declares charts: none, and no method.
  const detail = spyOn(api, "getEval").mockResolvedValue({
    compare: null,
    doc_text: null,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items: [],
    limit: 1,
    offset: 0,
    total: 1,
    window: WINDOW,
  });
  restores.push(
    () => chart.mockRestore(),
    () => daemon.mockRestore(),
    () => settings.mockRestore(),
    () => detail.mockRestore()
  );
});
afterEach(() => {
  sockets.restore();
  observers.restore();
  for (const restore of restores.splice(0)) restore();
});

/** A row of this file, its page unmounted inside its body rather than in `afterEach`: the
 *  section's timers are real ones, and Bun gives the event loop a turn between a row's body and
 *  its `afterEach`, where a timer due then would set state with no act scope open. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

/** Keeps the address the page is at in `search`. */
function Address(): null {
  search = useLocation().search;
  return null;
}

function renderTab(query = "?window=28d"): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[`/projects/ACME/evals${query}`]}>
      <QueryClientProvider client={queryClient}>
        <EvalsTab project="ACME" />
        <Address />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

async function openSocket(): Promise<FakeLiveSocket> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

function serve(socket: FakeLiveSocket, frame: Parameters<FakeLiveSocket["receive"]>[0]): void {
  act(() => socket.receive(frame));
}

function region(name: string): HTMLElement {
  return screen.getByRole("region", { name });
}

/** The evals views `socket` holds now: subscribed and not unsubscribed since. */
function heldEvals(socket: FakeLiveSocket): LiveSubscribe[] {
  const dropped = new Set(socket.unsubscribes().map((frame) => frame.sub));
  return socket
    .subscribes()
    .filter((frame) => frame.view.kind === "evals" && !dropped.has(frame.sub));
}

function queryOf(frame: LiveSubscribe): URLSearchParams {
  return new URLSearchParams("query" in frame.view ? frame.view.query : "");
}

/** The project-timeline subscribe frames sent, in order. */
function timelineFrames(socket: FakeLiveSocket): LiveSubscribe[] {
  return socket.subscribes().filter((frame) => frame.view.kind === "project-timeline");
}

/** Dispatch answers every evals view the page holds with one eval of its kind. */
function answerEvals(socket: FakeLiveSocket): void {
  for (const frame of heldEvals(socket)) {
    const kind = queryOf(frame).get("kind") as EvalKind;
    serve(socket, snapshotFrame(frame.sub, evalsAnswer([evalItem(`${kind}-eval`, kind, [])])));
  }
}

/** Opens the merge timeline and waits for the view it subscribes. */
async function openTimeline(socket: FakeLiveSocket): Promise<LiveSubscribe> {
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Show the merge timeline" }));
  return waitFor(() => {
    const frame = timelineFrames(socket).at(-1);
    if (frame === undefined) throw new Error("no project-timeline view was subscribed");
    return frame;
  });
}

/** The end of the rows the timeline holds: the line under them. */
function rowsEnd(): HTMLElement {
  const end = document.querySelector<HTMLElement>("[data-timeline-end]");
  if (end === null) throw new Error("the timeline draws no end of its rows");
  return end;
}

/** The ids of the rows the chart draws, merges then runs, each in the timeline's order. */
function drawnIds(): (string | number)[] {
  return [...(drawn?.prs ?? []).map((pr) => pr.id), ...(drawn?.runs ?? []).map((run) => run.id)];
}

/** The same ids `rows` holds, merges then runs. */
function idsOf(rows: readonly ProjectTimelineRow[]): (string | number)[] {
  return [
    ...rows.filter((row) => row.kind === "merge").map((row) => row.id),
    ...rows.filter((row) => row.kind !== "merge").map((row) => row.id),
  ];
}

/** The growth subscribe frame of `sub` asking for `limit`, once the page has sent it. */
function grownTo(socket: FakeLiveSocket, limit: number): Promise<LiveSubscribe> {
  return waitFor(() => {
    const frame = timelineFrames(socket).find((sent) => limitOf(sent) === limit);
    if (frame === undefined) throw new Error(`no growth to ${limit} was sent`);
    return frame;
  });
}

test("no request before open; opened, the one view of the 200 newest rows; closed, it is let go", async () => {
  const user = userEvent.setup();
  renderTab();
  const socket = await openSocket();
  await waitFor(() => expect(heldEvals(socket).length).toBe(4));
  answerEvals(socket);
  const section = region("Merge timeline");
  expect(section).toHaveAttribute("data-state", "closed");
  expect(timelineFrames(socket)).toEqual([]);
  expect(within(section).queryByTestId("delivery-chart")).toBeNull();

  const first = await openTimeline(socket);
  expect(first.view).toEqual({
    kind: "project-timeline",
    project: "ACME",
    query: "limit=200",
    window: { preset: 28 },
  });
  expect(first.resume).toBeUndefined();
  expect(section).toHaveAttribute("data-state", "subscribed");

  serve(socket, timelineSnapshot(first.sub, timelineAnswer(timelineRows(0, 200), 300)));
  expect(section).toHaveAttribute("data-state", "painted");
  expect(drawnIds()).toEqual(idsOf(timelineRows(0, 200)));
  expect(rowsEnd().textContent).toBe(
    "The newest 200 of 300 rows loaded; older rows load as you scroll."
  );

  await user.click(screen.getByRole("button", { name: "Hide the merge timeline" }));
  await waitFor(() => expect(socket.unsubscribes().map((frame) => frame.sub)).toEqual([first.sub]));
  expect(section).toHaveAttribute("data-state", "closed");
  expect(within(section).queryByTestId("delivery-chart")).toBeNull();
});

test("the second page's rows are appended to the rows on screen", async () => {
  renderTab();
  const socket = await openSocket();
  const first = await openTimeline(socket);
  const snapshot = timelineSnapshot(first.sub, timelineAnswer(timelineRows(0, 200), 300));
  serve(socket, snapshot);

  act(() => observers.show(rowsEnd()));
  const grown = await grownTo(socket, 300);
  expect(grown.sub).toBe(first.sub);
  expect(grown.resume).toEqual({ revs: snapshot.revs, seq: "1" });
  expect(region("Merge timeline")).toHaveAttribute("data-state", "growing");
  // The rows held stay drawn while the growth's rows are out.
  expect(drawnIds()).toEqual(idsOf(timelineRows(0, 200)));

  serve(socket, timelineGrowth(first.sub, timelineRows(200, 300), 300, "2"));
  expect(region("Merge timeline")).toHaveAttribute("data-state", "painted");
  expect(drawnIds()).toEqual(idsOf(timelineRows(0, 300)));
  expect(rowsEnd().textContent).toBe("All 300 rows of the window loaded.");
  expect(socket.unsubscribes()).toEqual([]);
});

test("the growth past 500 rows re-subscribes the one sub with a resume and appends the rows it answers", async () => {
  renderTab();
  const socket = await openSocket();
  const first = await openTimeline(socket);
  serve(socket, timelineSnapshot(first.sub, timelineAnswer(timelineRows(0, 200), 1300)));
  act(() => observers.show(rowsEnd()));

  let seq = 1;
  for (const limit of [400, 600]) {
    const grown = await grownTo(socket, limit);
    expect(grown.sub).toBe(first.sub);
    // Resumed from the last change applied, naming every row the page holds.
    const resume = grown.resume;
    if (resume === undefined || !("revs" in resume)) throw new Error("the growth names no resume");
    expect(resume.seq).toBe(String(seq));
    expect(Object.keys(resume.revs.items.items ?? {}).sort()).toEqual(
      timelineRows(0, limit - 200)
        .map((row) => String(row.id))
        .sort()
    );
    seq += 1;
    serve(socket, timelineGrowth(first.sub, timelineRows(limit - 200, limit), limit, String(seq)));
    expect(drawnIds()).toEqual(idsOf(timelineRows(0, limit)));
  }
  // The end still on screen, the view grows on past 500 as it grew below it.
  await grownTo(socket, 800);
  act(() => observers.hide(rowsEnd()));
  expect(timelineFrames(socket).map(limitOf)).toEqual([200, 400, 600, 800]);
  expect(new Set(timelineFrames(socket).map((frame) => frame.sub))).toEqual(new Set([first.sub]));
  expect(socket.unsubscribes()).toEqual([]);
  expect(rowsEnd().textContent).toBe("The newest 600 of 1300 rows loaded; loading older rows…");
});

test("once its limit reaches the window's total the view grows no more", async () => {
  renderTab();
  const socket = await openSocket();
  const first = await openTimeline(socket);
  serve(socket, timelineSnapshot(first.sub, timelineAnswer(timelineRows(0, 200), 450)));
  act(() => observers.show(rowsEnd()));
  await grownTo(socket, 400);
  serve(socket, timelineGrowth(first.sub, timelineRows(200, 400), 400, "2"));
  // The last growth asks for the total, not 200 more.
  await grownTo(socket, 450);
  serve(socket, timelineGrowth(first.sub, timelineRows(400, 450), 450, "3"));
  expect(region("Merge timeline")).toHaveAttribute("data-state", "painted");

  // The end on screen again: an effect would grow the view in the same act, and none does.
  act(() => observers.hide(rowsEnd()));
  act(() => observers.show(rowsEnd()));
  expect(timelineFrames(socket).map(limitOf)).toEqual([200, 400, 450]);
  expect(drawnIds()).toEqual(idsOf(timelineRows(0, 450)));
  expect(rowsEnd().textContent).toBe("All 450 rows of the window loaded.");
});

test("the brush sets the target cards' window to the whole UTC days it touches, and each card names them", async () => {
  const user = userEvent.setup();
  renderTab();
  const socket = await openSocket();
  await waitFor(() => expect(heldEvals(socket).length).toBe(4));
  const first = await openTimeline(socket);
  serve(socket, timelineSnapshot(first.sub, timelineAnswer(timelineRows(0, 20), 20)));

  await user.click(within(region("Merge timeline")).getByRole("button", { name: "Brush" }));
  await waitFor(() => expect(new URLSearchParams(search).get("ws")).toBe(BRUSH.start));
  expect(new URLSearchParams(search).get("we")).toBe(BRUSH.end);
  const days = { from: "2026-11-02T00:00:00.000Z", to: "2026-11-05T00:00:00.000Z" };
  const brushed = await waitFor(() => {
    const frames = heldEvals(socket).filter(
      (frame) => "window" in frame.view && "from" in frame.view.window
    );
    expect(frames.map((frame) => queryOf(frame).get("kind")).sort()).toEqual([
      "delivery",
      "legion",
    ]);
    return frames;
  });
  for (const frame of brushed) {
    expect(frame.view).toMatchObject({ window: days });
    // Compared, as the header compares a window, with as many days before it.
    expect(queryOf(frame).get("compare_from")).toBe("2026-10-30T00:00:00.000Z");
    expect(queryOf(frame).get("compare_to")).toBe(days.from);
  }
  // The other sections keep the header's window: the delivery view of 28 days is still held.
  expect(
    heldEvals(socket)
      .filter((frame) => "window" in frame.view && "preset" in frame.view.window)
      .map((frame) => queryOf(frame).get("kind"))
      .sort()
  ).toEqual(["ci", "delivery", "experiment", "legion"]);
  const cards = [...region("Target cards").querySelectorAll("[data-card]")];
  expect(cards.length).toBe(10);
  for (const card of cards) {
    expect(card.querySelector("[data-card-days]")?.textContent).toBe(
      "brushed days 2026-11-02 to 2026-11-04"
    );
  }

  await user.click(screen.getByRole("button", { name: "clear brush window" }));
  await waitFor(() => expect(new URLSearchParams(search).has("ws")).toBe(false));
  expect(new URLSearchParams(search).has("we")).toBe(false);
  await waitFor(() => expect(region("Target cards").querySelector("[data-card-days]")).toBeNull());
  expect(
    heldEvals(socket).filter((frame) => "window" in frame.view && "from" in frame.view.window)
  ).toEqual([]);
});

test("the Repository facet sets the target cards' repo, and narrows the timeline", async () => {
  const user = userEvent.setup();
  renderTab();
  const socket = await openSocket();
  await waitFor(() => expect(heldEvals(socket).length).toBe(4));
  const first = await openTimeline(socket);
  serve(socket, timelineSnapshot(first.sub, timelineAnswer(timelineRows(0, 20), 20)));
  const section = region("Merge timeline");

  await user.click(within(section).getByRole("button", { name: /^Repository/ }));
  await user.click(
    within(screen.getByRole("listbox", { name: "Repository options" })).getByRole("option", {
      name: /^acme\/widgets/,
    })
  );
  await waitFor(() => expect(new URLSearchParams(search).getAll("repo")).toEqual(["acme/widgets"]));
  // The cards' views, and every other evals view, read the repository's day rows.
  await waitFor(() => {
    const views = heldEvals(socket);
    expect(views.map((frame) => queryOf(frame).get("kind")).sort()).toEqual([
      "ci",
      "delivery",
      "experiment",
      "legion",
    ]);
    for (const frame of views) expect(queryOf(frame).getAll("repo")).toEqual(["acme/widgets"]);
  });
  // The header's repository filter names it too: the two are one parameter.
  expect(
    within(screen.getByRole("list", { name: "Repositories" })).getByText("acme/widgets")
  ).toBeInTheDocument();
  // The timeline is a view of its own for the facet, of its 200 newest rows.
  const narrowed = await waitFor(() => {
    const frame = timelineFrames(socket).at(-1);
    expect(frame?.sub).not.toBe(first.sub);
    return frame;
  });
  expect(narrowed?.view).toMatchObject({ query: "repo=acme%2Fwidgets&limit=200" });
});

// The search is asked for once the typing rests (`useSettled`): the URL keeps every keystroke, and
// the timeline subscribes the text the typing rested on, never a text it passed through on the way.
test("the search subscribes the text the typing rested on, once", async () => {
  const user = userEvent.setup();
  renderTab();
  const socket = await openSocket();
  const first = await openTimeline(socket);
  serve(socket, timelineSnapshot(first.sub, timelineAnswer(timelineRows(0, 20), 20)));

  await user.type(within(region("Merge timeline")).getByLabelText("Search"), "widget");
  expect(new URLSearchParams(search).get("q")).toBe("widget");
  await waitFor(() =>
    expect(timelineFrames(socket).map((frame) => queryOf(frame).get("q"))).toEqual([null, "widget"])
  );
});

test("the table mode's rows are the loaded rows, one per merge, deploy or failed run, and a run row opens its drill-down", async () => {
  const user = userEvent.setup();
  const run = spyOn(api, "getProjectDeployRun").mockResolvedValue({
    deploy: null,
    failures: [],
    id: 41,
    jobs: [],
    url: "https://github.com/acme/deploys/actions/runs/41",
  });
  restores.push(() => run.mockRestore());
  renderTab();
  const socket = await openSocket();
  const first = await openTimeline(socket);
  const rows: ProjectTimelineRow[] = [
    mergeRow(3, "2026-11-23T10:00:00Z"),
    runRow(41, "2026-11-23T09:00:00Z", "deploy"),
    runRow(42, "2026-11-23T08:00:00Z", "failed_run"),
    mergeRow(2, "2026-11-23T07:00:00Z"),
  ];
  serve(socket, timelineSnapshot(first.sub, timelineAnswer(rows, 4)));

  const chart = document.querySelector<HTMLElement>('[data-chart="timeline"]');
  if (chart === null) throw new Error("no timeline chart");
  await user.click(within(chart).getByRole("button", { name: "Table" }));
  const table = within(chart).getByRole("table", { name: "Merge timeline rows" });
  expect(
    [...table.querySelectorAll("tbody [data-row]")].map((row) => row.getAttribute("data-row"))
  ).toEqual(rows.map((row) => String(row.id)));
  expect(
    [...table.querySelectorAll("tbody [data-row]")].map(
      (row) => row.querySelectorAll("td")[1]?.textContent
    )
  ).toEqual(["Merge", "Deploy", "Failed run", "Merge"]);
  expect(document.querySelector("[data-issue-row]")).toBeNull();

  await user.click(within(table).getByText("Shipped 0 PR(s)"));
  const details = await screen.findByRole("dialog", { name: "Details" });
  expect(within(details).getByText("Deploy: run #41")).toBeInTheDocument();
  // The run is read under the tab's project, from the repository its row names.
  await waitFor(() => expect(run).toHaveBeenCalledWith("ACME", 41, "acme/deploys"));
});

test("the timeline draws no compare control and no compare overlay", async () => {
  const user = userEvent.setup();
  renderTab();
  const socket = await openSocket();
  const first = await openTimeline(socket);
  serve(socket, timelineSnapshot(first.sub, timelineAnswer(timelineRows(0, 20), 20)));
  const chart = document.querySelector<HTMLElement>('[data-chart="timeline"]');
  if (chart === null) throw new Error("no timeline chart");
  expect(within(chart).getByRole("button", { name: "Graph" })).toBeInTheDocument();
  expect(within(chart).queryByRole("checkbox", { name: "Compare" })).toBeNull();
  expect(chart.querySelector("[data-compare-legend]")).toBeNull();

  await user.click(within(chart).getByRole("button", { name: "Table" }));
  expect(within(chart).queryByRole("checkbox", { name: "Compare" })).toBeNull();
  const headers = within(chart)
    .getAllByRole("columnheader")
    .map((header) => header.textContent ?? "");
  expect(headers.filter((header) => /compare|change/i.test(header))).toEqual([]);
  // Its view names no compare range: the header's compare range is the evals views' alone.
  for (const frame of timelineFrames(socket)) {
    expect(queryOf(frame).has("compare_from")).toBe(false);
    expect(queryOf(frame).has("compare_to")).toBe(false);
  }
});
