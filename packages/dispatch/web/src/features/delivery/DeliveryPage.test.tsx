import { afterEach, beforeEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";

import { controlBar } from "../../__tests__/chips-line";
import { drawnSize } from "../../__tests__/control-size";
import { pickIn } from "../../__tests__/issue-picks";
import {
  optionNames as listOptionNames,
  panelOf,
  pickNamed,
  rows,
  textBoxes,
} from "../../__tests__/list-picks";
import {
  errorFrame,
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type {
  DeliveryLiveView,
  DeliveryMeasuresResponse,
  DeliveryPR,
  DeliverySessionGroup,
  DeliverySettings,
  DeliveryTimelineWithMeasures,
  LiveServerFrame,
  LiveSubscribe,
} from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { DeliveryPage } from "./DeliveryPage";

// The list view, so the page renders without the timeline chart, whose library needs a canvas.
const DELIVERY_URL =
  "/delivery?mode=list&from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";
/** The custom window DELIVERY_URL names, as the page subscribes it. */
const URL_WINDOW = { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" };

const emptyCounts = {
  repo: {},
  parent_agent: {},
  session: {},
  issue: {},
  priority: {},
  component: {},
  author: {},
  rework: {},
  deployed: {},
};

const emptyFreshness: DeliveryTimelineWithMeasures["freshness"] = {
  last_error: null,
  last_event_at: null,
  last_reconcile_at: null,
  unfetchable_count: 0,
};

const NOT_CONFIGURED = "DELIVERY_NOT_CONFIGURED";
const notConfiguredMessage =
  "delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery";

/** The measures panel's answer for the page's window: nothing in it, so every test that is not
 *  about the panel renders it without asserting on it. */
const emptyMeasures: DeliveryMeasuresResponse = {
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
  computed_at: "2024-06-02T00:00:00Z",
  measures: {
    deploy_frequency: {
      successful_deploys: 0,
      deploys_with_prs: 0,
      per_day: 0,
      with_prs_per_day: 0,
    },
    lead_time: {
      merge_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      first_commit_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      opened_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      opened_to_merge: { median_minutes: null, p90_minutes: null, max_minutes: null },
    },
    change_failure_rate: {
      per_pr: { confirmed: 0, pending: 0, rejected: 0, reverts: 0, total: 0, rate: 0 },
      per_deploy: {
        confirmed: 0,
        pending: 0,
        rejected: 0,
        reverts: 0,
        total: 0,
        rate: 0,
        confirmed_or_pending: 0,
        upper_bound_rate: 0,
      },
    },
    time_to_restore: { median_minutes: null },
    rework_share: 0,
    deploy_run_success: { concluded: 0, reached_production: 0, cancelled: 0, rate: null },
    daily: [],
  },
  flags_source: "none",
  unowned_p0: [],
  targets: {
    deploys_per_day: 20,
    change_failure_rate: 0.05,
    merge_to_production_minutes: 45,
    opened_to_merge_median_minutes: 60,
    deploy_run_success_rate: 0.9,
    unowned_p0: 0,
  },
  status: {
    deploys_per_day: false,
    change_failure_rate: null,
    change_failure_rate_upper_bound: null,
    merge_to_production_median: null,
    merge_to_production: null,
    opened_to_merge: null,
    deploy_run_success: null,
    unowned_p0: true,
  },
  freshness: emptyFreshness,
};

const emptyTimeline: DeliveryTimelineWithMeasures = {
  color_counts: { repo: {}, author: {}, priority: {}, component: {}, parent_agent: {} },
  components: {},
  facet_counts: emptyCounts,
  freshness: emptyFreshness,
  issue_titles: {},
  measures: emptyMeasures,
  prs: [],
  runs: [],
  session_groups: [],
  session_pr_counts: {},
  session_titles: {},
  waiting: [],
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
};

/** A session group of two sessions, as the page's answer lists it. */
const RELEASE_CREW: DeliverySessionGroup = {
  created_at: "2026-10-11T00:00:00Z",
  created_by: { id: "alice@example.com", kind: "user" },
  id: "g1",
  name: "Release crew",
  sessions: ["s1", "s2"],
  updated_at: "2026-10-11T00:00:00Z",
  updated_by: { id: "alice@example.com", kind: "user" },
};

/** How many pull requests each of `RELEASE_CREW`'s two sessions and one session outside it have,
 *  and their titles, as the page's answer counts and names them. */
const RELEASE_CREW_SESSIONS = {
  session_pr_counts: { s1: 2, s2: 1, s3: 1 },
  session_titles: { s1: "Planner one", s2: "Planner two", s3: "Reviewer" },
};

/** The router's query string, where a test reads what the page wrote to the URL, and the key of
 *  its location, which every write of the address makes anew, even one of the same query. */
function LocationProbe() {
  const location = useLocation();
  return (
    <output data-key={location.key} data-testid="location">
      {location.search}
    </output>
  );
}

/** The unit DOM lays nothing out, so the box the list's rows scroll in reports a height and
 *  each drawn row its own: without them the table has no window to draw and lists no row. */
function measureList(): () => void {
  const box = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  const row = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "delivery-pr-list" ? 600 : (box?.get?.call(this) ?? 0);
    },
  });
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
    configurable: true,
    get() {
      return this.dataset.prRow === undefined ? (row?.get?.call(this) ?? 0) : 36;
    },
  });
  return () => {
    if (box !== undefined) Object.defineProperty(HTMLElement.prototype, "clientHeight", box);
    if (row !== undefined) Object.defineProperty(HTMLElement.prototype, "offsetHeight", row);
  };
}

/** Renders the page at `url`, in a box of its own at the end of the document, else in
 *  `container`. */
function renderPage(url = DELIVERY_URL, { container }: { container?: HTMLElement } = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  restoreLayout = measureList();
  render(
    <MemoryRouter initialEntries={[url]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DeliveryPage />
          <LocationProbe />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>,
    container === undefined ? {} : { container }
  );
  return queryClient;
}

/** The one panel the page's filter trigger opens, which holds the four facets that are no
 *  column of the list. It is portalled out of the page, so its controls are found in the
 *  document rather than inside the row. */
async function openFilterPanel(user: UserEvent): Promise<void> {
  const trigger = screen.getByRole("button", { name: /^Filter(?: · \d+)?$/ });
  if (trigger.getAttribute("aria-expanded") !== "true") await user.click(trigger);
}

/** A facet of that panel, by the concept it picks from. */
async function panelFacet(user: UserEvent, label: string): Promise<HTMLElement> {
  await openFilterPanel(user);
  return screen.getByRole("button", { name: new RegExp(`^${label}: `) });
}

function currentSearch(): URLSearchParams {
  return new URLSearchParams(screen.getByTestId("location").textContent ?? "");
}

let sockets: FakeLiveSockets;
/** Puts back the measurements `measureList` replaced, for a test that rendered the page. */
let restoreLayout: (() => void) | undefined;
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  sockets.restore();
  restoreLayout?.();
  restoreLayout = undefined;
});

/**
 * A row of this file: `bun:test`'s own, but its body ends by unmounting the page inside act
 * (`cleanup`), not in `afterEach`. DeliveryPage's search debounce (`useSettled`, 250 ms from
 * mount) and SourceFreshness's 1 s tick are real timers, and Bun gives the event loop a turn
 * between a row's body and its `afterEach`: a timer due then fires with the act environment on
 * and no act scope open, and React reports its setState as an update outside act. The unmount
 * clears both timers before that turn.
 */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

/** The page's live socket, once the page has opened one, as Dispatch accepts it. */
async function openSocket(): Promise<FakeLiveSocket> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** The newest subscribe frame of `kind` the page has sent on `socket`, once it has sent one. */
function subscription(
  socket: FakeLiveSocket,
  kind: DeliveryLiveView["kind"]
): Promise<LiveSubscribe> {
  return waitFor(() => socket.subscribed(kind));
}

/** The newest subscribe frame of `view`'s kind, once it subscribes `view`. */
function subscriptionTo(socket: FakeLiveSocket, view: DeliveryLiveView): Promise<LiveSubscribe> {
  return waitFor(() => {
    const frame = socket.subscribed(view.kind);
    expect(frame.view).toEqual(view);
    return frame;
  });
}

/** Dispatch sends `frame` to the page. */
function serve(socket: FakeLiveSocket, frame: LiveServerFrame): void {
  act(() => socket.receive(frame));
}

/** The page subscribed its timeline, and Dispatch answered with `body`. */
async function answered(body: DeliveryTimelineWithMeasures): Promise<FakeLiveSocket> {
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, snapshotFrame(sub, body));
  return socket;
}

test("an unconfigured timeline shows the setup form in its place, reading no settings, and once a save configures it the page's own subscription brings the timeline", async () => {
  const user = userEvent.setup();
  const saved: DeliverySettings = {
    deploy_repo: "acme/widgets",
    deploy_workflow_path: ".github/workflows/deploy.yml",
    excluded_repos: [],
    last_error: null,
    last_event_at: null,
    last_reconcile_at: null,
    population_authors: ["octocat"],
    pr_checks_rollup_job_name: null,
    pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
    production_job_names: ["widgets-release"],
    production_apply_step_name: null,
    staging_apply_job_name: null,
    staging_e2e_job_name: null,
    updated_at: "2026-10-07T12:00:00Z",
    updated_by: { id: "alice@example.com", kind: "user" },
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);

  try {
    const queryClient = renderPage();
    const socket = await openSocket();
    const { sub } = await subscription(socket, "timeline");
    serve(socket, errorFrame(sub, NOT_CONFIGURED, notConfiguredMessage));

    expect(
      await screen.findByRole("heading", { name: "Set up the delivery timeline" })
    ).toBeDefined();
    expect(screen.queryByRole("button", { name: "Timeline" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    // DELIVERY_NOT_CONFIGURED already says no record is stored: the form's fields are there at
    // once, empty, with no read of the record and no loading line.
    expect(screen.queryByText("Loading delivery settings…")).not.toBeInTheDocument();
    expect((screen.getByLabelText("Deploy repository") as HTMLInputElement).value).toBe("");

    await user.click(screen.getByLabelText("Deploy repository"));
    await user.paste("acme/widgets");
    await user.click(screen.getByLabelText("Deploy workflow"));
    await user.paste(".github/workflows/deploy.yml");
    await user.click(screen.getByLabelText("Production jobs"));
    await user.paste("widgets-release");
    await user.click(screen.getByLabelText("PR checks workflow"));
    await user.paste(".github/workflows/pr-checks.yml");
    await user.click(screen.getByLabelText("Population authors"));
    await user.paste("octocat");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    // The save reads nothing again: the page stays subscribed, and Dispatch answers that same
    // subscription once a configuration exists.
    expect(socket.subscribes()).toHaveLength(1);
    expect(queryClient.isFetching()).toBe(0);
    serve(socket, snapshotFrame(sub, emptyTimeline));

    expect(await screen.findByText("No PRs in the current filter/window.")).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    expect(getDeliverySettings).not.toHaveBeenCalled();
  } finally {
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("a reconnect while the timeline is unconfigured keeps the setup form and its unsaved draft", async () => {
  const user = userEvent.setup();
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, errorFrame(sub, NOT_CONFIGURED, notConfiguredMessage));
  await screen.findByRole("heading", { name: "Set up the delivery timeline" });
  await user.click(screen.getByLabelText("Deploy repository"));
  await user.paste("acme/widgets");
  const draft = () => (screen.getByLabelText("Deploy repository") as HTMLInputElement).value;

  // Coming back to the tab (TanStack's focus listener is on `window`) reads nothing.
  act(() => {
    window.dispatchEvent(new Event("visibilitychange"));
  });
  expect(socket.subscribes()).toHaveLength(1);

  // Dispatch restarts: the page opens a new socket within the first attempt's spread (its draw
  // pinned to the start of the window here) and subscribes again. Any render of the timeline's
  // body while that waits would unmount the form, so the draft surviving says none happened.
  const random = spyOn(Math, "random").mockReturnValue(0);
  act(() => socket.drop(1012));
  random.mockRestore();
  await waitFor(() => expect(sockets.sockets).toHaveLength(2));
  const again = sockets.latest();
  act(() => again.open());
  expect(again.subscribed("timeline").sub).toBe(sub);
  expect(draft()).toBe("acme/widgets");
  expect(screen.queryByRole("button", { name: "Timeline" })).not.toBeInTheDocument();
  expect(screen.queryByText("Loading delivery timeline…")).not.toBeInTheDocument();

  serve(again, errorFrame(sub, NOT_CONFIGURED, notConfiguredMessage));
  expect(screen.getByRole("heading", { name: "Set up the delivery timeline" })).toBeDefined();
  expect(draft()).toBe("acme/widgets");
  expect(screen.queryByRole("button", { name: "Timeline" })).not.toBeInTheDocument();
});

test("an unconfigured timeline that Dispatch then refuses otherwise shows that failure and Retry, not the setup form", async () => {
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, errorFrame(sub, NOT_CONFIGURED, notConfiguredMessage));
  await screen.findByRole("heading", { name: "Set up the delivery timeline" });

  const random = spyOn(Math, "random").mockReturnValue(0);
  act(() => socket.drop(1012));
  random.mockRestore();
  await waitFor(() => expect(sockets.sockets).toHaveLength(2));
  const again = sockets.latest();
  act(() => again.open());
  serve(again, errorFrame(sub, "INTERNAL", "database unavailable"));

  expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
  expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
  expect(
    screen.queryByRole("heading", { name: "Set up the delivery timeline" })
  ).not.toBeInTheDocument();
});

test("a configured timeline renders with no setup form, subscribing the window and facets the URL names", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);

  try {
    renderPage();
    const socket = await openSocket();
    const { sub } = await subscriptionTo(socket, {
      kind: "timeline",
      window: URL_WINDOW,
      query: "",
    });
    serve(socket, snapshotFrame(sub, emptyTimeline));

    expect(await screen.findByText("No PRs in the current filter/window.")).toBeDefined();
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
    expect(
      screen.queryByRole("heading", { name: "Set up the delivery timeline" })
    ).not.toBeInTheDocument();
    expect(socket.subscribes()).toHaveLength(1);
    expect(getDeliverySettings).not.toHaveBeenCalled();
  } finally {
    getDeliverySettings.mockRestore();
  }
});

test("any other refusal keeps the page and offers Retry, not the setup form, and Retry subscribes again", async () => {
  const user = userEvent.setup();
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, errorFrame(sub, "INTERNAL", "database unavailable"));

  expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
  expect(
    screen.queryByRole("heading", { name: "Set up the delivery timeline" })
  ).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Retry" }));
  const retried = await waitFor(() => {
    const frame = socket.subscribed("timeline");
    expect(frame.sub).not.toBe(sub);
    return frame;
  });
  expect(socket.sent).toContainEqual({ type: "unsubscribe", sub });
  serve(socket, snapshotFrame(retried.sub, emptyTimeline));
  expect(await screen.findByText("No PRs in the current filter/window.")).toBeDefined();
  expect(screen.queryByText("Couldn't load the delivery timeline.")).not.toBeInTheDocument();
});

const listedPR: DeliveryPR = {
  additions: 12,
  author: "octocat",
  components: ["ACME/api"],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 3,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: "ACME-1",
  issue_title: "Ship widgets",
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: "P0",
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: a waiting widget",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

test("the facet column offers each value with its count, subscribes a pick and keeps the view in the URL", async () => {
  const user = userEvent.setup();
  const timeline: DeliveryTimelineWithMeasures = {
    ...emptyTimeline,
    components: { "ACME/api": { parent: null, title: "Public API" } },
    facet_counts: {
      ...emptyCounts,
      component: { "ACME/api": 1 },
      issue: { "ACME-1": 1 },
      priority: { P0: 1, __no_issue__: 3 },
      repo: { "acme/other": 5, "acme/widgets": 1 },
    },
    issue_titles: { "ACME-1": "Ship widgets" },
    prs: [listedPR],
  };

  renderPage();
  const socket = await answered(timeline);

  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  // The four facets that are no column sit behind the row's one trigger.
  expect((await panelFacet(user, "Repository")).textContent).toBe("Any repository");
  await user.click(await panelFacet(user, "Priority"));
  const priorityOptions = within(screen.getByRole("listbox", { name: "Priority options" }));
  const options = priorityOptions.getAllByRole("option").map((option) => option.textContent);
  expect(options).toEqual(["No issue3", "P01"]);
  await user.click(priorityOptions.getByRole("option", { name: /^P0/ }));

  // Dispatch applies the facets: the pick is a view of its own, its query the GET route's.
  await subscriptionTo(socket, { kind: "timeline", window: URL_WINDOW, query: "priority=P0" });
  expect((await panelFacet(user, "Priority")).textContent).toBe("1 selected");

  // The issue is a column of the list, so its filter sits on that column's own header.
  await user.click(screen.getByRole("button", { name: "Filter Issue" }));
  expect(screen.getByRole("option", { name: /ACME-1 — Ship widgets/ })).toBeDefined();

  const colorBy = screen.getByLabelText("Color merges by");
  await user.click(colorBy);
  expect(rows(panelOf(colorBy))).toEqual([
    "✓Repository",
    "Author",
    "Priority",
    "Component",
    "Parent agent",
  ]);
  await user.click(within(panelOf(colorBy)).getByRole("option", { name: "Component" }));
  expect(colorBy).toHaveAccessibleName("Color merges by Component");
  // The list's dot follows the colour-by facet, and the row reads its issue with its priority.
  expect(screen.getByText("ACME-1 · P0")).toBeDefined();
});

test("the header names the window and the freshness row the prototype's six sources", async () => {
  renderPage();
  await answered({
    ...emptyTimeline,
    freshness: { ...emptyTimeline.freshness, last_error: "rate limited" },
  });

  expect(await screen.findByRole("heading", { name: "Delivery timeline" })).toBeDefined();
  const generated = await screen.findByText(/^Generated .* · window/);
  expect(generated.textContent).not.toContain("1970");
  // No brush window in the URL: nothing to clear.
  expect(screen.queryByRole("button", { name: "clear brush window" })).toBeNull();
  const row = screen.getByRole("region", { name: "Source freshness" });
  const sources = [...row.querySelectorAll("[data-source]")].map((node) =>
    node.getAttribute("data-source")
  );
  expect(sources).toEqual(["prs", "runs", "ci", "dispatch", "agents", "events"]);
  expect(within(row).getByText("Deploy runs never checked: rate limited")).toBeDefined();
});

test("while a facet pick waits for its snapshot, the header and freshness keep the last answer's time, never the epoch", async () => {
  const user = userEvent.setup();
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(
    socket,
    snapshotFrame(sub, {
      ...emptyTimeline,
      facet_counts: { ...emptyCounts, priority: { P0: 1 } },
      prs: [listedPR],
    })
  );
  await screen.findByText(/^Generated .* · window/);
  await user.click(await panelFacet(user, "Priority"));
  await user.click(
    within(screen.getByRole("listbox", { name: "Priority options" })).getByRole("option", {
      name: /^P0/,
    })
  );
  const picked = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "priority=P0",
  });

  // The previous answer stays on screen while the new view's snapshot is out.
  expect(screen.getByText(/^Generated .* · window/).textContent).not.toContain("1970");
  const row = screen.getByRole("region", { name: "Source freshness" });
  expect(row.querySelector('[data-source="dispatch"]')?.textContent).toMatch(
    /^Dispatch \d+ s ago$/
  );
  // The snapshot's answer lands a few microtasks after the frame: inside `act`, before the row ends.
  await act(async () => {
    serve(socket, snapshotFrame(picked.sub, { ...emptyTimeline, prs: [listedPR] }));
  });
});

test("the list sorts by its headers and a row opens the PR's details", async () => {
  const user = userEvent.setup();
  const shipped: DeliveryPR = {
    ...listedPR,
    deploy_run: 500,
    deployed_at: "2024-06-01T01:40:00Z",
    deployed_status: "deployed",
    id: "acme/widgets#2",
    merged_at: "2024-06-01T00:30:00Z",
    number: 2,
    title: "feat: a shipped widget",
    url: "https://github.com/acme/widgets/pull/2",
  };
  renderPage();
  await answered({
    ...emptyTimeline,
    components: { "ACME/api": { parent: null, title: "Public API" } },
    prs: [listedPR, shipped],
  });

  const titles = async () =>
    (await screen.findAllByRole("row"))
      .slice(1)
      .map((row) => (row instanceof HTMLTableRowElement ? row.cells[3]?.textContent : undefined));
  expect(await titles()).toEqual(["feat: a waiting widget", "feat: a shipped widget"]);
  await user.click(screen.getByRole("button", { name: /^Merged/ }));
  expect(await titles()).toEqual(["feat: a shipped widget", "feat: a waiting widget"]);

  await user.click(screen.getByText("feat: a shipped widget"));
  const details = await screen.findByRole("dialog", { name: "Details" });
  expect(within(details).getByRole("link", { name: "Open on GitHub" })).toHaveAttribute(
    "href",
    "https://github.com/acme/widgets/pull/2"
  );
  expect(within(details).getByText("ACME-1 — Ship widgets")).toBeDefined();
  expect(within(details).getByText("Public API")).toBeDefined();
  expect(within(details).getByText("70 min")).toBeDefined();
  await user.click(within(details).getByRole("button", { name: "Close details" }));
  expect(screen.queryByRole("dialog", { name: "Details" })).not.toBeInTheDocument();
});

test("the measures panel sits between the freshness row and the view controls and shows the timeline's own measures, subscribing no second view", async () => {
  const user = userEvent.setup();
  const withP0 = (key: string): DeliveryTimelineWithMeasures => ({
    ...emptyTimeline,
    facet_counts: { ...emptyCounts, repo: { "acme/widgets": 1 } },
    prs: [listedPR],
    measures: {
      ...emptyMeasures,
      unowned_p0: [{ key, title: "Production deploy gate flakes" }],
    },
  });

  renderPage();
  const socket = await answered(withP0("ACME-103"));
  const p0 = await screen.findByRole("link", { name: "ACME-103" });
  expect(p0).toHaveAttribute("href", "/issues/ACME-103");
  const freshness = screen.getByRole("region", { name: "Source freshness" });
  const panel = screen.getByLabelText("KPI targets");
  const controls = screen.getByRole("group", { name: "View" });
  expect(freshness.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(panel.compareDocumentPosition(controls) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

  await user.click(await panelFacet(user, "Repository"));
  await user.click(
    within(screen.getByRole("listbox", { name: "Repository options" })).getByRole("option", {
      name: /^acme\/widgets/,
    })
  );
  // The facet subscribes the timeline's new view, and the panel follows that one answer.
  const picked = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "repo=acme%2Fwidgets",
  });
  serve(socket, snapshotFrame(picked.sub, withP0("ACME-104")));
  expect(await screen.findByRole("link", { name: "ACME-104" })).toBeDefined();
  expect(socket.subscribes().filter((frame) => frame.view.kind === "measures")).toHaveLength(0);
});

test("a brush window narrows the measures to it with a view of its own, leaves the timeline's view on the whole window, and clearing it returns to the timeline's measures", async () => {
  const user = userEvent.setup();
  const brushStart = "2024-06-01T06:00:00.000Z";
  const brushEnd = "2024-06-01T12:00:00.000Z";
  renderPage(
    `${DELIVERY_URL}&ws=${encodeURIComponent(brushStart)}&we=${encodeURIComponent(brushEnd)}`
  );
  const socket = await openSocket();
  const timeline = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "",
  });
  const brushed = await subscriptionTo(socket, {
    kind: "measures",
    window: { from: brushStart, to: brushEnd },
    query: "",
  });
  serve(
    socket,
    snapshotFrame(timeline.sub, {
      ...emptyTimeline,
      measures: { ...emptyMeasures, unowned_p0: [{ key: "ACME-1", title: "the whole window" }] },
    })
  );
  serve(
    socket,
    snapshotFrame(brushed.sub, {
      ...emptyMeasures,
      unowned_p0: [{ key: "ACME-2", title: "the brush window" }],
    })
  );
  expect(await screen.findByRole("link", { name: "ACME-2" })).toBeDefined();

  await user.click(await screen.findByRole("button", { name: "clear brush window" }));
  expect(await screen.findByRole("link", { name: "ACME-1" })).toBeDefined();
  // The brush's view ends with it, and the timeline's never moved.
  expect(socket.sent).toContainEqual({ type: "unsubscribe", sub: brushed.sub });
  expect(socket.subscribes()).toHaveLength(2);
});

test("a brushed measures failure keeps the timeline and offers its own Retry", async () => {
  const user = userEvent.setup();
  const brush = `&ws=${encodeURIComponent("2024-06-01T06:00:00.000Z")}&we=${encodeURIComponent("2024-06-01T12:00:00.000Z")}`;
  renderPage(`${DELIVERY_URL}${brush}`);
  const socket = await openSocket();
  const timeline = await subscription(socket, "timeline");
  const brushed = await subscription(socket, "measures");
  serve(socket, snapshotFrame(timeline.sub, emptyTimeline));
  serve(socket, errorFrame(brushed.sub, "INTERNAL", "database unavailable"));

  expect(await screen.findByText("Couldn't load the delivery measures.")).toBeDefined();
  expect(screen.getByText("No PRs in the current filter/window.")).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Retry" }));
  const retried = await waitFor(() => {
    const frame = socket.subscribed("measures");
    expect(frame.sub).not.toBe(brushed.sub);
    return frame;
  });
  serve(socket, snapshotFrame(retried.sub, emptyMeasures));
  expect(await screen.findByLabelText("KPI targets")).toBeDefined();
  expect(screen.queryByText("Couldn't load the delivery measures.")).not.toBeInTheDocument();
});

test("the drill-down is a dialog: focus moves into it, Escape closes it and focus returns to the row", async () => {
  const user = userEvent.setup();
  renderPage();
  await answered({ ...emptyTimeline, prs: [listedPR] });

  const row = await screen.findByRole("row", { name: /feat: a waiting widget/ });
  row.focus();
  await user.keyboard("{Enter}");
  const details = await screen.findByRole("dialog", { name: "Details" });
  await waitFor(() =>
    expect(within(details).getByRole("button", { name: "Close details" })).toHaveFocus()
  );
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("dialog", { name: "Details" })).not.toBeInTheDocument();
  expect(row).toHaveFocus();
});

test("colouring by Parent agent offers the values to give their own colour and lane, kept in the URL", async () => {
  const user = userEvent.setup();
  renderPage(`${DELIVERY_URL}&colorBy=parentAgent&agents=group%3Ag1&agents=group%3Ag1`);
  await answered({
    ...emptyTimeline,
    color_counts: {
      ...emptyTimeline.color_counts,
      parent_agent: { "group:g1": 3, Reviewer: 2, __no_session__: 1 },
    },
    session_groups: [RELEASE_CREW],
  });

  const trigger = await screen.findByRole("button", { name: "Own colour and lane: 1 chosen" });
  await user.click(trigger);
  const listbox = within(screen.getByRole("listbox", { name: "Own colour and lane options" }));
  expect(listbox.getAllByRole("option").map((option) => option.getAttribute("aria-label"))).toEqual(
    ["Release crew, 3 PRs", "Reviewer, 2 PRs"]
  );
  await user.click(listbox.getByRole("option", { name: /^Reviewer/ }));
  await waitFor(() => expect(currentSearch().getAll("agents")).toEqual(["group:g1", "Reviewer"]));

  const colorBy = screen.getByLabelText("Color merges by");
  await pickIn(user, () => colorBy, "Repository");
  expect(screen.queryByRole("button", { name: /^Own colour and lane/ })).not.toBeInTheDocument();
});

// R3: every list of options a reader picks from holds a text box that narrows the options. Color
// merges by is the shared list's single pick of its choices: pressed, it opens with one text box,
// a typed word leaves the choices it matches, and Enter on the one left colours the merges by it,
// kept in the URL, as the select's pick did.
test("the Color merges by list opens with a text box, a typed word narrows the choices, and a pick colours by it", async () => {
  const user = userEvent.setup();
  renderPage();
  await answered(emptyTimeline);
  const colorBy = await screen.findByRole("button", { name: pickNamed("Color merges by") });
  expect(colorBy).toHaveAccessibleName("Color merges by Repository");
  const boxes = textBoxes().length;
  await user.click(colorBy);
  const panel = panelOf(colorBy);
  expect(textBoxes().length).toBe(boxes + 1);
  expect(within(panel).getByRole("combobox", { name: "Search color choices" })).toHaveFocus();
  await user.keyboard("agent");
  expect(rows(panel)).toEqual(["Parent agent"]);
  await user.keyboard("{Enter}");
  await waitFor(() => expect(currentSearch().get("colorBy")).toBe("parentAgent"));
  expect(colorBy).toHaveAccessibleName("Color merges by Parent agent");
  expect(colorBy).toHaveFocus();
  // Parent agent offers its values their own colour and lane.
  expect(screen.getByRole("button", { name: /^Own colour and lane/ })).toBeDefined();
});

// A `<select>` set to the value it holds fires no change when that value is picked again. The
// list opens on the choice it shows, so Enter at once picks that choice, and the page writes
// nothing to the address: the router's location keeps its key (`LocationProbe`).
test("a pick of the choice Color merges by shows writes nothing to the address", async () => {
  const user = userEvent.setup();
  renderPage();
  await answered(emptyTimeline);
  const colorBy = await screen.findByRole("button", { name: pickNamed("Color merges by") });
  const location = screen.getByTestId("location");
  const before = location.getAttribute("data-key");
  await user.click(colorBy);
  await user.keyboard("{Enter}");
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(colorBy).toHaveAccessibleName("Color merges by Repository");
  expect(colorBy).toHaveFocus();
  expect(location.getAttribute("data-key")).toBe(before);
});

// R2: each control takes its size from the page, comfortable or compact, as a value the page
// passes. The Delivery page is compact, as its search box and its Filter trigger are, so its
// Window, Color merges by, Own colour and lane and each facet in its Filter panel draw the
// compact size's room (`controlPadding.compact`), rather than room of their own.
test("R2: the Window, Color merges by, Own colour and lane and the Filter panel's facets draw the Delivery page's compact size", async () => {
  const user = userEvent.setup();
  renderPage(`${DELIVERY_URL}&colorBy=parentAgent`);
  await answered({
    ...emptyTimeline,
    color_counts: { ...emptyTimeline.color_counts, parent_agent: { Reviewer: 2 } },
    facet_counts: { ...emptyCounts, repo: { "acme/widgets": 1 } },
  });
  const window = await screen.findByLabelText("Window");
  const colorBy = await screen.findByRole("button", { name: pickNamed("Color merges by") });
  const ownColour = await screen.findByRole("button", { name: /^Own colour and lane/ });
  const repository = await panelFacet(user, "Repository");
  expect({
    colorBy: drawnSize(colorBy),
    ownColour: drawnSize(ownColour),
    repository: drawnSize(repository),
    window: drawnSize(window),
  }).toEqual({
    colorBy: "compact",
    ownColour: "compact",
    repository: "compact",
    window: "compact",
  });
});

test("a malformed custom window falls back to the 28-day preset and removes its bad parameters", async () => {
  renderPage("/delivery?mode=list&from=not-a-date&to=also-not-a-date");
  const window = await screen.findByLabelText("Window");
  expect(window).toHaveAccessibleName("Window Last 28 days");
  await waitFor(() => {
    expect(currentSearch().has("from")).toBe(false);
    expect(currentSearch().has("to")).toBe(false);
    expect(currentSearch().has("window")).toBe(false);
  });
  const socket = await openSocket();
  await subscription(socket, "timeline");
  for (const frame of socket.subscribes()) {
    expect(frame.view).toEqual({ kind: "timeline", window: { preset: 28 }, query: "" });
  }
});

test("a preset subscribes by its days, and the header names the window Dispatch resolved", async () => {
  renderPage("/delivery?mode=list&window=7d");
  const socket = await openSocket();
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: { preset: 7 },
    query: "",
  });
  const resolved = { from: "2026-10-05T12:00:00Z", to: "2026-10-12T12:00:00Z" };
  serve(socket, snapshotFrame(sub, { ...emptyTimeline, window: resolved }));

  const header = await screen.findByText(/^Generated .* · window/);
  expect(header.textContent).toContain(
    `${new Date(resolved.from).toLocaleDateString()}–${new Date(resolved.to).toLocaleDateString()}`
  );
  expect(currentSearch().get("window")).toBe("7d");
  expect(currentSearch().has("from")).toBe(false);
  expect(currentSearch().has("to")).toBe(false);
});

test("a facet pick leaves the window rolling: it writes no from or to, and subscribes by the preset", async () => {
  const user = userEvent.setup();
  renderPage("/delivery?mode=list");
  const socket = await answered({
    ...emptyTimeline,
    facet_counts: { ...emptyCounts, priority: { P0: 1 } },
  });
  await user.click(await panelFacet(user, "Priority"));
  await user.click(
    within(screen.getByRole("listbox", { name: "Priority options" })).getByRole("option", {
      name: /^P0/,
    })
  );
  await waitFor(() => expect(currentSearch().getAll("priority")).toEqual(["P0"]));
  expect(currentSearch().has("from")).toBe(false);
  expect(currentSearch().has("to")).toBe(false);
  await subscriptionTo(socket, {
    kind: "timeline",
    window: { preset: 28 },
    query: "priority=P0",
  });
});

// The search is asked for once the typing rests (`useSettled`): the URL keeps every keystroke, and
// the page subscribes the text the typing rested on, never a text it passed through on the way.
test("the search subscribes the text the typing rested on, once", async () => {
  const user = userEvent.setup();
  renderPage();
  const socket = await answered(emptyTimeline);
  await user.type(screen.getByLabelText("Search pull requests"), "widget");
  expect(currentSearch().get("q")).toBe("widget");
  await subscriptionTo(socket, { kind: "timeline", window: URL_WINDOW, query: "q=widget" });
  expect(
    socket
      .subscribes()
      .flatMap((frame) => (frame.view.kind === "timeline" ? [frame.view.query] : []))
  ).toEqual(["", "q=widget"]);
});

test("the window control: a preset rolls and clears the brush, a custom range writes its days", async () => {
  const user = userEvent.setup();
  renderPage(
    `/delivery?mode=list&ws=${encodeURIComponent("2024-06-01T06:00:00Z")}&we=${encodeURIComponent("2024-06-01T12:00:00Z")}`
  );
  const socket = await openSocket();
  const brushed = await subscription(socket, "measures");

  const windowList = await screen.findByLabelText("Window");
  await pickIn(user, () => windowList, "Last 7 days");
  await waitFor(() => expect(currentSearch().get("window")).toBe("7d"));
  expect(currentSearch().has("ws")).toBe(false);
  expect(currentSearch().has("we")).toBe(false);
  expect(currentSearch().has("from")).toBe(false);
  await subscriptionTo(socket, { kind: "timeline", window: { preset: 7 }, query: "" });
  expect(socket.sent).toContainEqual({ type: "unsubscribe", sub: brushed.sub });

  await pickIn(user, () => windowList, "Custom range…");
  await user.click(screen.getByLabelText("From"));
  await user.paste("2026-01-01");
  await user.click(screen.getByLabelText("To"));
  await user.paste("2026-01-07");
  await user.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(currentSearch().get("from")).toBe("2026-01-01T00:00:00.000Z"));
  expect(currentSearch().get("to")).toBe("2026-01-08T00:00:00.000Z");
  expect(currentSearch().has("window")).toBe(false);
  await subscriptionTo(socket, {
    kind: "timeline",
    window: { from: "2026-01-01T00:00:00.000Z", to: "2026-01-08T00:00:00.000Z" },
    query: "",
  });
});

// A press that takes its own control away hands the focus on, never to the page's body (ruling
// ff1a60f5). A preset picked from the Window list leaves the list's trigger, the same element,
// holding the focus: the control stays mounted across a change of the window.
test("a preset picked from the Window list keeps its trigger, and the focus on it", async () => {
  const user = userEvent.setup();
  renderPage();
  const trigger = await screen.findByLabelText("Window");
  act(() => trigger.focus());
  await pickIn(user, () => trigger, "Last 84 days");
  await waitFor(() => expect(currentSearch().get("window")).toBe("84d"));
  expect(screen.getByLabelText("Window")).toBe(trigger);
  expect(trigger).toHaveFocus();
});

/** The page over `DELIVERY_URL`'s window with a brush window set in it, as a drag along the
 *  chart's time axis sets one, so the header draws clear brush window. */
const BRUSHED_URL = `${DELIVERY_URL}&ws=${encodeURIComponent("2024-06-01T06:00:00.000Z")}&we=${encodeURIComponent("2024-06-01T12:00:00.000Z")}`;

// Clear brush window goes with the brush it clears, and its focus goes to the Window list's
// trigger, in the reader's sight as happy-dom draws every box: the line it sits in holds no other
// control.
test("clear brush window hands its focus to the Window list's trigger", async () => {
  const user = userEvent.setup();
  renderPage(BRUSHED_URL);
  const socket = await openSocket();
  const timeline = await subscription(socket, "timeline");
  serve(socket, snapshotFrame(timeline.sub, emptyTimeline));
  const clear = await screen.findByRole("button", { name: "clear brush window" });
  clear.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => expect(currentSearch().has("ws")).toBe(false));
  expect(screen.queryByRole("button", { name: "clear brush window" })).toBeNull();
  expect(screen.getByRole("button", { name: pickNamed("Window") })).toHaveFocus();
});

// With the page scrolled below the Window list's trigger, so that the trigger stands wholly above
// the window, clear brush window's focus goes to no control out of the reader's sight: the trigger
// is passed over, and the main content around the page takes the focus at its own stop, with no
// scroll, so the reader stays where they scrolled to.
test("clear brush window with the Window list's trigger scrolled out of sight hands its focus to the main content's own stop, scrolling nothing", async () => {
  const user = userEvent.setup();
  const main = document.createElement("main");
  document.body.append(main);
  renderPage(BRUSHED_URL, { container: main });
  const socket = await openSocket();
  const timeline = await subscription(socket, "timeline");
  serve(socket, snapshotFrame(timeline.sub, emptyTimeline));
  const clear = await screen.findByRole("button", { name: "clear brush window" });
  const trigger = screen.getByLabelText("Window");
  // Wholly above the window's top edge, where the reader's scroll left it: happy-dom lays
  // nothing out.
  trigger.getBoundingClientRect = () => new DOMRect(0, -60, 120, 32);
  clear.focus();
  const focuses = spyOn(HTMLElement.prototype, "focus");
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.keyboard("{Enter}");
    await waitFor(() => expect(currentSearch().has("ws")).toBe(false));
    expect(screen.queryByRole("button", { name: "clear brush window" })).toBeNull();
    expect(main).toHaveFocus();
    expect(focuses.mock.calls).toEqual([[{ preventScroll: true }]]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    focuses.mockRestore();
    scrolls.mockRestore();
  }
});

test("the freshness row says Live with the last change while the socket is open, and Reconnecting once it drops", async () => {
  const now = Date.now();
  const ago = (ms: number) => new Date(now - ms).toISOString();
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(
    socket,
    snapshotFrame(
      sub,
      {
        ...emptyTimeline,
        freshness: {
          last_error: null,
          last_event_at: ago(30_000),
          last_reconcile_at: ago(60_000),
          unfetchable_count: 0,
        },
      },
      { changedAt: ago(4 * 60_000) }
    )
  );

  const row = await screen.findByRole("region", { name: "Source freshness" });
  const prs = row.querySelector('[data-source="prs"]');
  expect(prs?.textContent).toBe("PRs Live · last change 4 min ago");
  expect(row.querySelector('[data-source="events"]')?.textContent).toBe(
    "Events Live · last change 4 min ago"
  );

  act(() => socket.drop(1006));
  expect(prs?.textContent).toMatch(/^PRs Reconnecting, \d+ s$/);
});

/** happy-dom has no canvas, and ECharts throws without a 2D context: a context that accepts every
 *  call lets the timeline mount, though it paints nothing. */
function stubCanvasContext(): () => void {
  const original = HTMLCanvasElement.prototype.getContext;
  const state: Record<string | symbol, unknown> = {};
  const context = new Proxy(state, {
    get(target, key) {
      if (key in target) return target[key];
      if (key === "measureText") return (text: string) => ({ width: text.length * 6 });
      if (key === "getImageData") return () => ({ data: new Uint8ClampedArray(4) });
      return () => ({ addColorStop() {} });
    },
    set(target, key, value) {
      target[key] = value;
      return true;
    },
  });
  HTMLCanvasElement.prototype.getContext = (() => context) as unknown as typeof original;
  return () => {
    HTMLCanvasElement.prototype.getContext = original;
  };
}

test("a brush window narrows the merges shown but the waiting line still counts what waits before it", async () => {
  const restoreCanvas = stubCanvasContext();
  try {
    renderPage(
      "/delivery?from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z&ws=2024-06-01T06%3A00%3A00Z&we=2024-06-01T12%3A00%3A00Z"
    );
    await answered({
      ...emptyTimeline,
      // Merged 28 days before the window and not shipped: the server's `waiting`.
      waiting: [{ merged_at: "2024-05-04T00:00:00Z", deployed_at: null }],
      // Merged inside the window at 01:00, before the brush, and still waiting.
      prs: [listedPR],
    });
    const chart = await screen.findByRole("img", { name: /^Delivery timeline/ });
    expect(chart.getAttribute("aria-label")).toContain(
      "0 merged pull requests, 2 waiting to deploy at the start"
    );
  } finally {
    cleanup();
    restoreCanvas();
  }
});

/** The options of an open filter, by the name each is read out as (`<value>, <n> PR(s)`). */
function optionNames(listbox: string): (string | null)[] {
  return within(screen.getByRole("listbox", { name: listbox }))
    .getAllByRole("option")
    .map((option) => option.getAttribute("aria-label"));
}

// What `FacetPanel.test.tsx` proved before the page drew these controls itself: the counts, the
// wording of a placeholder, what a parent component says it includes, and what a trigger reads.
test("a facet's options sort by count, read placeholders in words, and say a parent includes its sub-components", async () => {
  const user = userEvent.setup();
  renderPage();
  await answered({
    ...emptyTimeline,
    components: {
      "ACME/platform": { parent: null, title: "The platform" },
      "ACME/api": { parent: "ACME/platform", title: "Public API" },
      "ACME/api-auth": { parent: "ACME/api", title: "API auth" },
    },
    facet_counts: {
      ...emptyCounts,
      component: { "ACME/api": 2, "ACME/platform": 1, __no_issue__: 4, __no_component__: 3 },
      parent_agent: { "Agent 1": 2, __no_session__: 5 },
    },
    prs: [listedPR],
  });

  await user.click(await panelFacet(user, "Component"));
  expect(optionNames("Component options")).toEqual([
    "No issue, 4 PRs",
    "No component, 3 PRs",
    "Public API (includes 1 sub-component), 2 PRs",
    "The platform (includes 2 sub-components), 1 PR",
  ]);

  // Parent agent is a column of the list, so its values are offered on that column's header,
  // counted by the same answer.
  await user.keyboard("{Escape}");
  await user.click(screen.getByRole("button", { name: "Filter Parent agent" }));
  expect(optionNames("Parent agent options")).toEqual(["No session, 5 PRs", "Agent 1, 2 PRs"]);
});

test("a facet's trigger names the facet and what is picked", async () => {
  const user = userEvent.setup();
  renderPage(`${DELIVERY_URL}&component=ACME%2Fapi`);
  await answered({
    ...emptyTimeline,
    components: { "ACME/api": { parent: null, title: "Public API" } },
    facet_counts: { ...emptyCounts, component: { "ACME/api": 1 }, repo: { "acme/widgets": 1 } },
    prs: [listedPR],
  });

  expect(await panelFacet(user, "Component")).toHaveAttribute(
    "aria-label",
    "Component: 1 selected"
  );
  expect(await panelFacet(user, "Repository")).toHaveAttribute(
    "aria-label",
    "Repository: Any repository"
  );
  // A picked value is a chip of the bar, which takes it off again.
  await user.click(
    within(controlBar("the Delivery page")).getByRole("button", {
      name: "Remove Component: Public API filter",
    })
  );
  await waitFor(() => expect(currentSearch().has("component")).toBe(false));
});

test("a group's Parent agent value reads as its name, and Edit groups opens the editor", async () => {
  const user = userEvent.setup();
  renderPage();
  await answered({
    ...emptyTimeline,
    facet_counts: { ...emptyCounts, parent_agent: { "group:g1": 3, Reviewer: 1 } },
    prs: [listedPR],
    session_groups: [RELEASE_CREW],
    ...RELEASE_CREW_SESSIONS,
  });

  await user.click(screen.getByRole("button", { name: "Filter Parent agent" }));
  expect(optionNames("Parent agent options")).toEqual(["Release crew, 3 PRs", "Reviewer, 1 PR"]);
  await user.keyboard("{Escape}");

  await openFilterPanel(user);
  await user.click(screen.getByRole("button", { name: "Edit groups" }));
  expect(screen.getByRole("dialog", { name: "Session groups" })).toBeDefined();
});

// R5: the order the list is in rides in the page's address, absent until the reader presses a
// header, so a shared link and a reload open the list ordered as they left it.
test("the list's sort is a parameter of the address: absent until set, and it orders the rows it names", async () => {
  const user = userEvent.setup();
  const shipped: DeliveryPR = {
    ...listedPR,
    author: "aardvark",
    id: "acme/widgets#2",
    merged_at: "2024-06-01T00:30:00Z",
    number: 2,
    title: "feat: a shipped widget",
    url: "https://github.com/acme/widgets/pull/2",
  };
  renderPage();
  await answered({ ...emptyTimeline, prs: [listedPR, shipped] });

  const titles = async () =>
    (await screen.findAllByRole("row"))
      .slice(1)
      .map((row) => (row instanceof HTMLTableRowElement ? row.cells[3]?.textContent : undefined));
  // The list opens newest merge first, and says nothing about it in the address.
  expect(await titles()).toEqual(["feat: a waiting widget", "feat: a shipped widget"]);
  expect(currentSearch().has("sort")).toBe(false);

  await user.click(screen.getByRole("button", { name: /^Author/ }));
  await waitFor(() => expect(currentSearch().get("sort")).toBe("author"));
  expect(await titles()).toEqual(["feat: a shipped widget", "feat: a waiting widget"]);
});

test("a link naming a sort opens the list in that order, and the facets it names narrow it", async () => {
  renderPage(`${DELIVERY_URL}&sort=-author&author=octocat`);
  const socket = await openSocket();
  await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "author=octocat",
  });
  const { sub } = await subscription(socket, "timeline");
  serve(
    socket,
    snapshotFrame(sub, {
      ...emptyTimeline,
      facet_counts: { ...emptyCounts, author: { octocat: 2 } },
      prs: [
        listedPR,
        {
          ...listedPR,
          author: "zebra",
          id: "acme/widgets#3",
          merged_at: "2024-06-01T00:10:00Z",
          number: 3,
          title: "feat: a third widget",
          url: "https://github.com/acme/widgets/pull/3",
        },
      ],
    })
  );

  const titles = async () =>
    (await screen.findAllByRole("row"))
      .slice(1)
      .map((row) => (row instanceof HTMLTableRowElement ? row.cells[3]?.textContent : undefined));
  // Author, descending: the reload reads the order out of the address rather than the default.
  expect(await titles()).toEqual(["feat: a third widget", "feat: a waiting widget"]);
  // And the column's own filter says what the address asked the server for.
  expect(screen.getByRole("button", { name: "Filter Author: 1 picked" })).toBeDefined();
});

// INFRA-3135's review round 1 (thread PRRT_kwDORFy7ds63p6zM): a `?sort=` naming no sortable
// column was kept, TanStack dropped the sort it could not draw, and the list fell back to the
// answer's own order, oldest merge first.
test("a sort naming no column of the list opens it newest merge first, and a column it names keeps its order", async () => {
  const older: DeliveryPR = {
    ...listedPR,
    author: "aardvark",
    id: "acme/widgets#2",
    merged_at: "2024-06-01T00:30:00Z",
    number: 2,
    title: "feat: an older widget",
    url: "https://github.com/acme/widgets/pull/2",
  };
  const titles = async () =>
    (await screen.findAllByRole("row"))
      .slice(1)
      .map((row) => (row instanceof HTMLTableRowElement ? row.cells[3]?.textContent : undefined));

  // An id no column has, and a column's header in place of its id: both are dropped, and the
  // answer, which lists the older merge first, is drawn in the page's opening order.
  renderPage(`${DELIVERY_URL}&sort=foo,-Merged`);
  await answered({ ...emptyTimeline, prs: [older, listedPR] });
  expect(await titles()).toEqual(["feat: a waiting widget", "feat: an older widget"]);
  expect(screen.getByRole("columnheader", { name: /^Merged/ })).toHaveAttribute(
    "aria-sort",
    "descending"
  );
  cleanup();
  restoreLayout?.();

  // Beside an unknown id, the column the address names still orders the list.
  renderPage(`${DELIVERY_URL}&sort=foo,-author`);
  await answered({ ...emptyTimeline, prs: [older, listedPR] });
  expect(await titles()).toEqual(["feat: a waiting widget", "feat: an older widget"]);
  expect(screen.getByRole("columnheader", { name: /^Author/ })).toHaveAttribute(
    "aria-sort",
    "descending"
  );
});

/** The page on the Timeline, the view it opens on, over the window `DELIVERY_URL` names. */
const TIMELINE_URL = "/delivery?from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

/** The nine facets as the page names them, in the order its panel lists them. */
const NINE_FACETS = [
  "Repository",
  "Session",
  "Priority",
  "Component",
  "Issue",
  "Parent agent",
  "Author",
  "Rework",
  "Deployed",
];

/** A window whose answer counts a value of every facet, so the panel draws each of them. */
const everyFacet: DeliveryTimelineWithMeasures = {
  ...emptyTimeline,
  components: { "ACME/api": { parent: null, title: "Public API" } },
  facet_counts: {
    author: { octocat: 1 },
    component: { "ACME/api": 1 },
    deployed: { waiting: 1 },
    issue: { "ACME-1": 1 },
    parent_agent: { __no_session__: 1 },
    priority: { P0: 1 },
    repo: { "acme/widgets": 1 },
    rework: { value: 1 },
    session: { s1: 1 },
  },
  issue_titles: { "ACME-1": "Ship widgets" },
  prs: [listedPR],
};

/** The trigger and every chip of the page's control bar, by the name each is read out as: read
 *  in the bar itself, since a chip is drawn there and, while the Filter panel is open, again at
 *  the panel's top, which is portalled outside the bar. */
function rowControls(): string[] {
  return within(controlBar("the Delivery page"))
    .getAllByRole("button", { name: /^(Filter(?: · \d+)?|Remove .* filter)$/ })
    .map((button) => button.getAttribute("aria-label") ?? button.textContent ?? "");
}

// INFRA-3135's review round 1 (thread PRRT_kwDORFy7ds63p6zB): the row's panel held only the four
// facets that are no column of the list, and the list is drawn in the List view alone, so on the
// Timeline, the view the page opens on, the other five could be neither set nor taken off.
test("on the Timeline the row's panel holds all nine facets, and a value picked there draws its chip", async () => {
  const user = userEvent.setup();
  const restoreCanvas = stubCanvasContext();
  try {
    renderPage(TIMELINE_URL);
    const socket = await answered(everyFacet);
    await screen.findByRole("img", { name: /^Delivery timeline/ });
    await openFilterPanel(user);
    const panel = screen.getByRole("dialog", { name: "Filter" });
    expect(
      within(panel)
        .getAllByRole("button", { name: /^[^:]+: Any / })
        .map((trigger) => trigger.getAttribute("aria-label")?.split(":")[0])
    ).toEqual(NINE_FACETS);

    await user.click(within(panel).getByRole("button", { name: /^Author: / }));
    await user.click(
      within(screen.getByRole("listbox", { name: "Author options" })).getByRole("option", {
        name: /^octocat/,
      })
    );
    await subscriptionTo(socket, { kind: "timeline", window: URL_WINDOW, query: "author=octocat" });
    expect(rowControls()).toEqual(["Filter · 1", "Remove Author: octocat filter"]);
  } finally {
    cleanup();
    restoreCanvas();
  }
});

// The same thread: `/delivery?author=x`, or a facet set in the List and carried back to the
// Timeline, narrowed the chart while the row showed no count, no chip and no way to take it off.
test("on the Timeline a link's facets each draw a chip, counted on the trigger, and a chip's press takes its value off", async () => {
  const user = userEvent.setup();
  const restoreCanvas = stubCanvasContext();
  try {
    renderPage(`${TIMELINE_URL}&author=octocat&deployed=waiting&issue=ACME-1`);
    await answered(everyFacet);
    await screen.findByRole("img", { name: /^Delivery timeline/ });
    expect(rowControls()).toEqual([
      "Filter · 3",
      "Remove Issue: ACME-1 \u2014 Ship widgets filter",
      "Remove Author: octocat filter",
      "Remove Deployed: waiting filter",
    ]);
    await user.click(screen.getByRole("button", { name: "Remove Author: octocat filter" }));
    await waitFor(() => expect(currentSearch().has("author")).toBe(false));
    expect(currentSearch().getAll("deployed")).toEqual(["waiting"]);
    expect(rowControls()).toEqual([
      "Filter · 2",
      "Remove Issue: ACME-1 \u2014 Ship widgets filter",
      "Remove Deployed: waiting filter",
    ]);
  } finally {
    cleanup();
    restoreCanvas();
  }
});

// The same thread, in the List: a value picked on a column's header narrowed the list while the
// row drew no chip for it, and the trigger did not count it.
test("in the List a value picked on a column's header draws its chip in the row, and the chip's press takes it off the header", async () => {
  const user = userEvent.setup();
  renderPage();
  const socket = await answered(everyFacet);
  await user.click(await screen.findByRole("button", { name: "Filter Author" }));
  await user.click(
    within(screen.getByRole("listbox", { name: "Author options" })).getByRole("option", {
      name: /^octocat/,
    })
  );
  await subscriptionTo(socket, { kind: "timeline", window: URL_WINDOW, query: "author=octocat" });
  await user.keyboard("{Escape}");
  expect(screen.getByRole("button", { name: "Filter Author: 1 picked" })).toBeDefined();
  expect(rowControls()).toEqual(["Filter · 1", "Remove Author: octocat filter"]);

  await user.click(screen.getByRole("button", { name: "Remove Author: octocat filter" }));
  await waitFor(() => expect(currentSearch().has("author")).toBe(false));
  expect(screen.getByRole("button", { name: "Filter Author" })).toBeDefined();
  // In the control bar's look the trigger says its count only while a narrowing is on.
  expect(rowControls()).toEqual(["Filter"]);
  // In the List the five stay on their headers: the panel holds the four that are no column.
  await openFilterPanel(user);
  expect(
    within(screen.getByRole("dialog", { name: "Filter" }))
      .getAllByRole("button", { name: /^[^:]+: Any / })
      .map((trigger) => trigger.getAttribute("aria-label")?.split(":")[0])
  ).toEqual(NINE_FACETS.slice(0, 4));
});

// F2 item 2: the row was drawn only once an answer was in, so while the page read - and after a
// first read that failed - `?author=x` narrowed it with no chip, no count and no way off.
test("before its first answer the row draws the link's chips and its count, from the address alone", async () => {
  const user = userEvent.setup();
  renderPage(`${DELIVERY_URL}&author=octocat&issue=ACME-1&rework=rework`);
  const socket = await openSocket();
  await subscription(socket, "timeline");

  // Nothing has answered: the chips are the address's own, an issue reading as the key the
  // address spells, since no title has been read for it.
  expect(await screen.findByText("Loading delivery timeline…")).toBeDefined();
  expect(rowControls()).toEqual([
    "Filter · 3",
    "Remove Issue: ACME-1 filter",
    "Remove Author: octocat filter",
    "Remove Rework: rework filter",
  ]);

  // A chip's press takes its value off while the page is still reading.
  await user.click(screen.getByRole("button", { name: "Remove Author: octocat filter" }));
  await waitFor(() => expect(currentSearch().has("author")).toBe(false));
  expect(currentSearch().get("issue")).toBe("ACME-1");

  // Once the answer is in - the one the press subscribed, since taking a value off asks
  // Dispatch again - a value the answer names reads in its words.
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "issue=ACME-1&rework=rework",
  });
  serve(socket, snapshotFrame(sub, everyFacet));
  await waitFor(() =>
    expect(rowControls()).toEqual([
      "Filter · 2",
      "Remove Issue: ACME-1 \u2014 Ship widgets filter",
      "Remove Rework: rework filter",
    ])
  );
});

test("after a first read that failed the row still draws the link's chips", async () => {
  renderPage(`${DELIVERY_URL}&author=octocat`);
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, errorFrame(sub, "INTERNAL", "the timeline is down"));
  expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
  expect(rowControls()).toEqual(["Filter · 1", "Remove Author: octocat filter"]);
});

/** Each option of the list `trigger` opens, named `listbox`, by the name it is read out as
 *  (`listOptionNames`). Escape closes the list again, so the next read opens its own. */
async function openedOptionNames(
  user: UserEvent,
  trigger: HTMLElement,
  listbox: string
): Promise<string[]> {
  await user.click(trigger);
  const names = listOptionNames(screen.getByRole("listbox", { name: listbox }));
  await user.keyboard("{Escape}");
  return names;
}

/** Each option of the panel's `facet` (`openedOptionNames`). */
async function facetOptionNames(user: UserEvent, facet: string): Promise<string[]> {
  return openedOptionNames(user, await panelFacet(user, facet), `${facet} options`);
}

// INFRA-3135, round 25 (the tester's), by the architect's ruling of 2026-11-24 08:07Z: a count
// beside a value is drawn only once the data it counts has answered. Before the timeline's first
// answer the Timeline's panel offered every verdict of Rework and Deployed at "0 PRs", and a value
// the address picks at 0 too, though nothing had counted them.
test("on the Timeline the Filter panel draws no count beside a value before the first answer, and the answer's counts once it is in", async () => {
  const user = userEvent.setup();
  const restoreCanvas = stubCanvasContext();
  try {
    renderPage(`${TIMELINE_URL}&author=octocat`);
    const socket = await openSocket();
    const { sub } = await subscription(socket, "timeline");
    expect(await screen.findByText("Loading delivery timeline…")).toBeDefined();
    expect(await facetOptionNames(user, "Rework")).toEqual(["value", "rework"]);
    expect(await facetOptionNames(user, "Deployed")).toEqual([
      "deployed",
      "waiting",
      "not tracked",
    ]);
    expect(await facetOptionNames(user, "Author")).toEqual(["\u2713octocat"]);

    serve(socket, snapshotFrame(sub, everyFacet));
    await screen.findByRole("img", { name: /^Delivery timeline/ });
    expect(await facetOptionNames(user, "Rework")).toEqual(["value, 1 PR", "rework, 0 PRs"]);
    expect(await facetOptionNames(user, "Deployed")).toEqual([
      "deployed, 0 PRs",
      "waiting, 1 PR",
      "not tracked, 0 PRs",
    ]);
    expect(await facetOptionNames(user, "Author")).toEqual(["octocat, 1 PR"]);
  } finally {
    cleanup();
    restoreCanvas();
  }
});

test("after a first read that failed the Filter panel offers every verdict with no count beside it", async () => {
  const user = userEvent.setup();
  renderPage(TIMELINE_URL);
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, errorFrame(sub, "INTERNAL", "the timeline is down"));
  expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
  expect(await facetOptionNames(user, "Rework")).toEqual(["value", "rework"]);
  expect(await facetOptionNames(user, "Deployed")).toEqual(["deployed", "waiting", "not tracked"]);
});

// INFRA-3135's R5: every address a reader can write selects the rows it selected at the merge
// base. A facet value the page knows no label for - `constructor`, a key every object inherits
// and none of the labels' own, `bogus`, `P9` - is sent to Dispatch as it was there, which selects
// no row by it, and the page draws it as a chip by its own word, so the reader can take it off.
// Alone it lists nothing, as it did; beside a known value it lists what the known one does.
for (const [facet, query, chip] of [
  ["priority", "priority=P9", "Remove Priority: P9 filter"],
  ["rework", "rework=constructor", "Remove Rework: constructor filter"],
  ["deployed", "deployed=bogus", "Remove Deployed: bogus filter"],
] as const) {
  test(`an unknown ${facet} value alone reaches the subscription and draws its chip`, async () => {
    renderPage(`${DELIVERY_URL}&${query}`);
    const socket = await openSocket();
    const { sub } = await subscriptionTo(socket, { kind: "timeline", window: URL_WINDOW, query });
    serve(socket, snapshotFrame(sub, everyFacet));
    expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
    expect(rowControls()).toEqual(["Filter · 1", chip]);
  });
}

// A word every object inherits names no count: the Filter panel's Priority list draws
// `constructor` with a count of 0, where a plain read of the answer's counts drew Object's own
// function there.
test("an address value every object inherits draws no inherited count in the Filter panel", async () => {
  const user = userEvent.setup();
  renderPage(`${DELIVERY_URL}&priority=constructor`);
  const socket = await openSocket();
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "priority=constructor",
  });
  serve(socket, snapshotFrame(sub, everyFacet));
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  const names = await facetOptionNames(user, "Priority");
  expect(names.filter((name) => name.startsWith("constructor"))).toEqual(["constructor, 0 PRs"]);
});

test("an unknown facet value beside a known one reaches the subscription with it, and each draws its chip", async () => {
  renderPage(
    `${DELIVERY_URL}&priority=P9&priority=P0&rework=constructor&rework=rework&deployed=waiting&deployed=bogus`
  );
  const socket = await openSocket();
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query:
      "priority=P9&priority=P0&rework=constructor&rework=rework&deployed=waiting&deployed=bogus",
  });
  serve(socket, snapshotFrame(sub, everyFacet));
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  expect(rowControls()).toEqual([
    "Filter · 6",
    "Remove Priority: P9 filter",
    "Remove Priority: P0 filter",
    "Remove Rework: constructor filter",
    "Remove Rework: rework filter",
    "Remove Deployed: waiting filter",
    "Remove Deployed: bogus filter",
  ]);
});

// F2 item 3: the count is the answer's for the filter and the window the address names, so the
// previous answer's number is never shown as this filter's.
test("the count says it is counting while a new filter's read is out, and shows the answer's number again", async () => {
  const user = userEvent.setup();
  renderPage();
  const socket = await answered(everyFacet);
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();

  await user.click(await screen.findByRole("button", { name: "Filter Author" }));
  await user.click(
    within(screen.getByRole("listbox", { name: "Author options" })).getByRole("option", {
      name: /^octocat/,
    })
  );
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "author=octocat",
  });
  // The previous answer is still on screen (`keepPreviousData`), so its number is not this
  // filter's and no number is drawn.
  await screen.findByText("Counting PRs in current filter/window…");
  expect(screen.queryByText(/^\d+ PRs in current filter\/window$/)).toBeNull();

  serve(socket, snapshotFrame(sub, { ...everyFacet, prs: [] }));
  expect(await screen.findByText("0 PRs in current filter/window")).toBeDefined();
});

// INFRA-3135, round 26, by the ruling of 2026-11-24 08:07Z: a count beside a value is drawn only
// once the data it counts has answered. After the first answer a pick in one facet sends a new
// read, and while it is out the previous answer stays on screen (`keepPreviousData`): every other
// facet in the open panel drew that answer's counts as the new filter's. Its values are drawn with
// no count until the pick's read answers, as the header's count waits.
test("while a pick's read is out the Filter panel's other facets draw no count beside a value, and the answer's counts once it is in", async () => {
  const user = userEvent.setup();
  renderPage();
  const socket = await answered(everyFacet);
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  expect(await facetOptionNames(user, "Priority")).toEqual(["P0, 1 PR"]);

  await user.click(await panelFacet(user, "Repository"));
  await user.click(
    within(screen.getByRole("listbox", { name: "Repository options" })).getByRole("option", {
      name: /^acme\/widgets/,
    })
  );
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "repo=acme%2Fwidgets",
  });
  // The first answer is still on screen while the pick's read is out.
  await screen.findByText("Counting PRs in current filter/window…");
  expect(await facetOptionNames(user, "Priority")).toEqual(["P0"]);

  serve(
    socket,
    snapshotFrame(sub, {
      ...everyFacet,
      facet_counts: { ...everyFacet.facet_counts, priority: { P0: 1, P1: 2 } },
    })
  );
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  expect(await facetOptionNames(user, "Priority")).toEqual(["P1, 2 PRs", "P0, 1 PR"]);
});

/** Each value a List column's header filter offers (`openedOptionNames`). */
function columnOptionNames(user: UserEvent, column: string): Promise<string[]> {
  return openedOptionNames(
    user,
    screen.getByRole("button", { name: new RegExp(`^Filter ${column}`) }),
    `${column} options`
  );
}

// The same for the List view's column filters, which count each value from the same answer: while
// a pick's read was out, a header's filter drew the previous filter's count beside each value.
test("while a pick's read is out a List column's filter draws no count beside a value, and the answer's counts once it is in", async () => {
  const user = userEvent.setup();
  renderPage();
  const socket = await answered(everyFacet);
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  expect(await columnOptionNames(user, "Author")).toEqual(["octocat, 1 PR"]);

  await user.click(screen.getByRole("button", { name: "Filter Issue" }));
  await user.click(
    within(screen.getByRole("listbox", { name: "Issue options" })).getByRole("option", {
      name: /^ACME-1/,
    })
  );
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "issue=ACME-1",
  });
  await user.keyboard("{Escape}");
  // The first answer is still on screen while the pick's read is out.
  await screen.findByText("Counting PRs in current filter/window…");
  expect(await columnOptionNames(user, "Author")).toEqual(["octocat"]);

  serve(
    socket,
    snapshotFrame(sub, {
      ...everyFacet,
      facet_counts: { ...everyFacet.facet_counts, author: { hubot: 2, octocat: 1 } },
    })
  );
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  expect(await columnOptionNames(user, "Author")).toEqual(["hubot, 2 PRs", "octocat, 1 PR"]);
});

/** Each agent the Own colour list offers (`openedOptionNames`). */
function ownColourNames(user: UserEvent): Promise<string[]> {
  return openedOptionNames(
    user,
    screen.getByRole("button", { name: /^Own colour and lane: / }),
    "Own colour and lane options"
  );
}

// The same, for the Own colour list, which counts each agent from the same answer: while a new
// filter's read was out it drew the previous filter's "N PRs" beside each agent.
test("while a new filter's read is out the Own colour list draws no count beside an agent, and the answer's counts once it is in", async () => {
  const user = userEvent.setup();
  const colouredByAgent: DeliveryTimelineWithMeasures = {
    ...everyFacet,
    color_counts: { ...emptyTimeline.color_counts, parent_agent: { "group:g1": 3, Reviewer: 2 } },
    session_groups: [RELEASE_CREW],
  };
  renderPage(`${DELIVERY_URL}&colorBy=parentAgent`);
  const socket = await answered(colouredByAgent);
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  expect(await ownColourNames(user)).toEqual(["Release crew, 3 PRs", "Reviewer, 2 PRs"]);

  await user.click(screen.getByRole("button", { name: "Filter Author" }));
  await user.click(
    within(screen.getByRole("listbox", { name: "Author options" })).getByRole("option", {
      name: /^octocat/,
    })
  );
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "author=octocat",
  });
  await user.keyboard("{Escape}");
  // The first answer is still on screen while the filter's read is out.
  await screen.findByText("Counting PRs in current filter/window…");
  expect(await ownColourNames(user)).toEqual(["Release crew", "Reviewer"]);

  serve(socket, snapshotFrame(sub, colouredByAgent));
  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  expect(await ownColourNames(user)).toEqual(["Release crew, 3 PRs", "Reviewer, 2 PRs"]);
});

// F2 item 4: `?ws=x&we=y` was kept as written, `Date.parse` gave NaN and every pull request was
// filtered out of the chart, the list and the count. One row per window, through this file's own
// `test`, which unmounts inside act and has no `.each`.
for (const [what, bad] of [
  ["two words", "ws=x&we=y"],
  ["one end alone", "ws=2024-06-01T06%3A00%3A00Z"],
  ["an end before its start", "ws=2024-06-01T18%3A00%3A00Z&we=2024-06-01T06%3A00%3A00Z"],
] as const) {
  test(`a brush window that is ${what} is dropped, and the page shows its whole window`, async () => {
    renderPage(`${DELIVERY_URL}&${bad}`);
    await answered(everyFacet);
    expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
    await waitFor(() => expect(currentSearch().has("ws")).toBe(false));
    expect(currentSearch().has("we")).toBe(false);
    // A window that was never brushed draws no control to clear one.
    expect(screen.queryByRole("button", { name: "clear brush window" })).toBeNull();
  });
}
