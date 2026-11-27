import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { deliverySettings } from "../../__tests__/delivery-settings";
import { ApiError, api } from "../../api/client";
import type { LegionSetting, Project, ProjectDeliverySettings } from "../../api/types";
import { DefaultProjectRedirect } from "./DefaultProjectRedirect";

const spies: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

function project(key: string): Project {
  return {
    created_at: "2026-09-10T00:00:00Z",
    gated_steps: [],
    key,
    name: key,
    open_asks: 0,
    proven_sessions: false,
  };
}

function daemon(key: string): LegionSetting {
  return {
    created_by: { id: "alice", kind: "user" },
    project: key,
    state_url: "http://127.0.0.1:9",
    updated_at: "2026-09-10T00:00:00Z",
  };
}

/** Where the redirect sent the reader. */
function Landed(): ReactNode {
  const location = useLocation();
  return (
    <output data-testid="landed">
      {location.pathname}
      {location.search}
    </output>
  );
}

/** A read's answer as a test names it: the row, `null` for none, an `ApiError` for a failure, or
 *  "pending" for a read that never settles. */
type Answer<T> = T | null | ApiError | "pending";

function answer<T>(named: Answer<T> | undefined): Promise<T | null> {
  if (named === "pending") return Promise.withResolvers<never>().promise;
  if (named instanceof ApiError) return Promise.reject(named);
  return Promise.resolve(named ?? null);
}

/** The redirect at `path`, over `projects` in the order the project list answers them (the
 *  sidebar's), each project's daemon read answering what `daemons` names and its delivery
 *  settings read what `deliveries` names, `null` (no row) where it names none. */
function renderRedirect(
  path: string,
  projects: Project[],
  daemons: Record<string, Answer<LegionSetting>>,
  deliveries: Record<string, Answer<ProjectDeliverySettings>> = {}
) {
  spies.push(spyOn(api, "listProjects").mockResolvedValue(projects));
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockImplementation((key) =>
    answer(daemons[key])
  );
  const getProjectDeliverySettings = spyOn(api, "getProjectDeliverySettings").mockImplementation(
    (key) => answer(deliveries[key])
  );
  spies.push(getLegionDaemon, getProjectDeliverySettings);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<DefaultProjectRedirect />} path="/delivery" />
          <Route element={<DefaultProjectRedirect />} path="/pipeline" />
          <Route element={<Landed />} path="/projects/:key/evals" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getLegionDaemon, getProjectDeliverySettings };
}

test("the default project is the first, in the sidebar's order, whose tab has a Legion daemon", async () => {
  const { getLegionDaemon } = renderRedirect(
    "/delivery",
    [project("ZED"), project("MID"), project("ABC")],
    { ZED: null, MID: daemon("MID"), ABC: daemon("ABC") }
  );
  expect((await screen.findByTestId("landed")).textContent).toBe("/projects/MID/evals");
  // Every project's daemon is read at once, rather than one after another.
  expect(getLegionDaemon.mock.calls.map(([key]) => key).sort()).toEqual(["ABC", "MID", "ZED"]);
});

test("with no project that has a daemon, the default project is the first in the sidebar's order", async () => {
  renderRedirect("/pipeline", [project("ZED"), project("ABC")], { ZED: null, ABC: null });
  expect((await screen.findByTestId("landed")).textContent).toBe("/projects/ZED/evals");
});

test("the query string is kept, so an old link opens the same window and filters on the tab", async () => {
  renderRedirect("/delivery?window=84d&repo=acme%2Fwidgets&author=octocat", [project("ACME")], {
    ACME: daemon("ACME"),
  });
  expect((await screen.findByTestId("landed")).textContent).toBe(
    "/projects/ACME/evals?window=84d&repo=acme%2Fwidgets&author=octocat"
  );
});

test("the choice waits for the projects before the first with a daemon, and only for them", async () => {
  renderRedirect("/delivery", [project("ZED"), project("MID"), project("ABC")], {
    ZED: daemon("ZED"),
    MID: "pending",
    ABC: "pending",
  });
  expect((await screen.findByTestId("landed")).textContent).toBe("/projects/ZED/evals");

  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
  renderRedirect("/delivery", [project("ZED"), project("MID")], {
    ZED: "pending",
    MID: daemon("MID"),
  });
  expect(await screen.findByText("Loading projects…")).toBeTruthy();
  expect(screen.queryByTestId("landed")).toBeNull();
});

test("a daemon read that failed is shown with Retry, never passed over to a later project", async () => {
  const user = userEvent.setup();
  const { getLegionDaemon } = renderRedirect("/delivery", [project("ZED"), project("MID")], {
    ZED: new ApiError(500, { code: "INTERNAL", error: "boom" }),
    MID: daemon("MID"),
  });
  expect(await screen.findByText("Could not read whether ZED has a Legion daemon.")).toBeTruthy();
  expect(screen.queryByTestId("landed")).toBeNull();

  getLegionDaemon.mockImplementation((key) => Promise.resolve(key === "ZED" ? null : daemon(key)));
  await user.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.getByTestId("landed").textContent).toBe("/projects/MID/evals"));
});

test("the default project is the first, in the sidebar's order, whose tab has a source: a daemon or delivery on", async () => {
  const { getProjectDeliverySettings } = renderRedirect(
    "/delivery",
    [project("ZED"), project("MID"), project("ABC"), project("XYZ")],
    // ABC's daemon read never answers: its delivery being on is source enough.
    { ZED: null, MID: null, ABC: "pending", XYZ: daemon("XYZ") },
    { ZED: null, MID: deliverySettings("MID", { delivery: false }), ABC: deliverySettings("ABC") }
  );
  expect((await screen.findByTestId("landed")).textContent).toBe("/projects/ABC/evals");
  // Every project's delivery settings are read at once, beside its daemon.
  expect(getProjectDeliverySettings.mock.calls.map(([key]) => key).sort()).toEqual([
    "ABC",
    "MID",
    "XYZ",
    "ZED",
  ]);
});

test("a delivery settings read that failed is shown with Retry, never passed over to a later project", async () => {
  const user = userEvent.setup();
  const { getProjectDeliverySettings } = renderRedirect(
    "/pipeline",
    [project("ZED"), project("MID")],
    { ZED: null, MID: daemon("MID") },
    { ZED: new ApiError(500, { code: "INTERNAL", error: "boom" }) }
  );
  expect(await screen.findByText("Could not read whether ZED has delivery on.")).toBeTruthy();
  expect(screen.queryByTestId("landed")).toBeNull();

  getProjectDeliverySettings.mockImplementation((key) =>
    Promise.resolve(key === "ZED" ? deliverySettings("ZED") : null)
  );
  await user.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.getByTestId("landed").textContent).toBe("/projects/ZED/evals"));
});

test("with no project at all, the reader is told where to create one", async () => {
  renderRedirect("/delivery", [], {});
  expect(await screen.findByText(/No project has numbers yet/)).toBeTruthy();
  expect(screen.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings");
});
