import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import type { IssueListQuery } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { IssueQueryTable } from "./IssueQueryTable";

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key,
    advances: [],
    advances_revision: 0,
    title: `Work ${key}`,
    status: "todo",
    priority: null,
    rank: key,
    labels: ["dispatch-ux"],
    parent: null,
    blocked_by: [],
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    last_seq: 2,
    open_asks: 0,
    progress: { tasks: null, children: null },
    primary_artifact_id: "spec",
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-09-10T00:00:00Z",
    updated_at: "2026-09-10T00:00:00Z",
    closed_at: overrides.status === "done" ? "2026-09-10T00:00:00Z" : null,
    ...overrides,
  };
}

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

/** Draws one list of `query` over `issues`, which the stubbed listing narrows by the statuses it
 *  is asked for, as the server does, in a box `width` px wide: the unit DOM lays nothing out, so
 *  the box's width is given here. */
function list(query: IssueListQuery, issues: IssueSummary[], width = 1280) {
  const listing = spyOn(api, "listIssues").mockImplementation(async (options = {}) =>
    issues.filter((row) => options.status === undefined || options.status.includes(row.status))
  );
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const viewer = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    login: "alice",
    kind: "user",
  });
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "list1-scroll" ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const bounds = HTMLElement.prototype.getBoundingClientRect;
  HTMLElement.prototype.getBoundingClientRect = function (this: HTMLElement) {
    return this.dataset.testid === "list1-scroll"
      ? DOMRect.fromRect({ height: 600, width, x: 0, y: 0 })
      : bounds.call(this);
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", login: "alice", kind: "user" });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <IssueQueryTable query={query} stateKey="list1" />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    listing.mockRestore();
    state.mockRestore();
    agents.mockRestore();
    viewer.mockRestore();
    HTMLElement.prototype.getBoundingClientRect = bounds;
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return { listing };
}

// c-form's rows at the unit level (dispatch://INFRA-4935/spec#b-r-form,
// dispatch://INFRA-4935/spec#b-r-visible-filters): the query's projects and labels stand above
// the rows with no remove control, the five headers each sort and filter, nothing else stands
// above or beside the rows, and the Status filter's picks are what the list reads.
test("a list draws its five columns with their sorts and filters, and no bar above its rows", async () => {
  const user = userEvent.setup();
  const { listing } = list(
    {
      labels: ["dispatch-ux"],
      priorities: [],
      projects: ["INFRA"],
      statuses: ["in_progress", "todo"],
    },
    [
      issue("INFRA-1"),
      issue("INFRA-2", { status: "in_progress", priority: 0 }),
      issue("INFRA-3", { status: "done" }),
    ]
  );
  const section = await screen.findByRole("region", {
    name: "Issues: Project INFRA; Label dispatch-ux",
  });
  await within(section).findByRole("row", { name: "INFRA-1 Work INFRA-1" });
  expect(listing).toHaveBeenCalledWith({
    label: ["dispatch-ux"],
    project: ["INFRA"],
    status: ["in_progress", "todo"],
  });
  expect(within(section).getByText("Project: INFRA")).toBeDefined();
  expect(within(section).getByText("Label: dispatch-ux")).toBeDefined();
  expect(within(section).queryByRole("row", { name: "INFRA-3 Work INFRA-3" })).toBeNull();

  const headers = within(section)
    .getAllByRole("columnheader")
    .map((header) => header.textContent?.trim() ?? "");
  expect(headers.map((text) => text.replace(/Filter.*$/, "").trim())).toEqual([
    "Status",
    "Key",
    "Title",
    "Priority",
    "Open asks",
  ]);
  for (const column of ["Status", "Key", "Title", "Priority", "Open asks"]) {
    const filter = new RegExp(`^Filter ${column}`);
    expect(within(section).getByRole("button", { name: filter })).toBeDefined();
  }
  // Nothing of the Issues tab's bar, its selection or its row actions.
  expect(within(section).queryByRole("searchbox")).toBeNull();
  expect(within(section).queryAllByRole("checkbox")).toEqual([]);
  expect(within(section).queryByText("Group by")).toBeNull();
  expect(within(section).queryByRole("button", { name: /Columns/ })).toBeNull();
  expect(within(section).queryByRole("button", { name: /Start next/ })).toBeNull();
  expect(within(section).queryAllByRole("button", { name: /^Stop showing|remove/i })).toEqual([]);
  // The status mark is named by the status's words, and the priority by its name.
  expect(within(section).getAllByRole("img", { name: "In progress" }).length).toBeGreaterThan(0);
  expect(within(section).getByText("P0")).toBeDefined();

  // Done picked in the Status filter is one more status in the read.
  await user.click(within(section).getByRole("button", { name: /^Filter Status/ }));
  await user.click(screen.getByRole("option", { name: /^Done/ }));
  await waitFor(() =>
    expect(listing).toHaveBeenLastCalledWith({
      label: ["dispatch-ux"],
      project: ["INFRA"],
      status: ["done", "in_progress", "todo"],
    })
  );
  await within(section).findByRole("row", { name: "INFRA-3 Work INFRA-3" });
});

// A query with no status has no Status filter: the list reads every status, done included
// (dispatch://INFRA-4935/spec#b-r-exact-query).
test("a list whose query names no status reads every status", async () => {
  const { listing } = list({ labels: ["overnight"], priorities: [], projects: [], statuses: [] }, [
    issue("INFRA-1"),
    issue("OSSV-2", { status: "done" }),
  ]);
  await screen.findByRole("row", { name: "OSSV-2 Work OSSV-2" });
  expect(listing).toHaveBeenCalledWith({ label: ["overnight"] });
  expect(screen.getByRole("row", { name: "INFRA-1 Work INFRA-1" })).toBeDefined();
});

// On a phone the Key column goes and each row's key stands above its title, so the title keeps
// the width (dispatch://INFRA-4935/spec#b-r-phone-key).
test("in a box 400 px wide a list draws the key above the title, with no Key column", async () => {
  list(
    { labels: ["dispatch-ux"], priorities: [], projects: [], statuses: [] },
    [issue("INFRA-4935")],
    400
  );
  const row = await screen.findByRole("row", { name: "INFRA-4935 Work INFRA-4935" });
  await waitFor(() =>
    expect(
      screen
        .getAllByRole("columnheader")
        .map((header) => header.textContent?.replace(/Filter.*$/, ""))
    ).toEqual(["Status", "Title", "Priority", "Open asks"])
  );
  const title = within(row).getAllByRole("cell")[1] as HTMLElement;
  expect(within(title).getByRole("link", { name: "INFRA-4935" })).toBeDefined();
  expect(within(title).getByText("Work INFRA-4935")).toBeDefined();
});
