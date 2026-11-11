import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import {
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type { IssueSummary, LegionSetting } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { IssuesTable } from "./IssuesTable";
import { boardWithFacts } from "./legion-board.fixture";

let sockets: FakeLiveSockets;
type IssueListQuery = NonNullable<Parameters<typeof api.listIssues>[0]>;

const restores: (() => void)[] = [];

beforeEach(() => {
  sockets = installFakeLiveSockets();
});

afterEach(() => {
  cleanup();
  sockets.restore();
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    assignee: null,
    blocked_by: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-01T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    key,
    labels: [],
    last_seq: 1,
    open_asks: 0,
    parent: null,
    primary_artifact_id: "spec",
    priority: null,
    progress: { children: null, tasks: null },
    rank: key,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    title: `Work ${key}`,
    updated_at: "2026-11-01T00:00:00Z",
    ...overrides,
  };
}

const openIssues = [issue("ACME-9")];
const doneIssues = [
  issue("ACME-1", { closed_at: "2026-11-10T01:00:00Z", status: "done" }),
  issue("ACME-2", { closed_at: "2026-11-09T01:00:00Z", status: "done" }),
  issue("ACME-8", { closed_at: "2026-11-08T01:00:00Z", status: "done" }),
];

function legionSetting(project = "ACME"): LegionSetting {
  return {
    created_by: { id: "alice", kind: "user" },
    project,
    state_url: "http://legion.example:13370",
    updated_at: "2026-11-01T00:00:00Z",
  };
}

function table({
  path,
  settings = Promise.resolve([]),
}: {
  path: string;
  settings?: Promise<LegionSetting[]>;
}) {
  const requests: IssueListQuery[] = [];
  const listing = spyOn(api, "listIssues").mockImplementation(
    async (query: IssueListQuery = {}) => {
      requests.push(query);
      if (query.status === "done") return doneIssues;
      if (query.open) return openIssues;
      return [...openIssues, ...doneIssues];
    }
  );
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const legion = spyOn(api, "listLegionSettings").mockImplementation(() => settings);
  const viewer = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "issues-table-scroll"
        ? 600
        : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <IssuesTable project="ACME" />
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
    legion.mockRestore();
    viewer.mockRestore();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return { requests };
}

async function expectRequest(
  requests: readonly IssueListQuery[],
  expected: IssueListQuery
): Promise<void> {
  await waitFor(() => expect(requests).toContainEqual(expected));
}

// These tests fail if the hook changes back to a full listing or drops one of the closed-window
// bounds before it reaches the list route.
test("the open view reads only the open listing", async () => {
  const { requests } = table({ path: "/projects/ACME/issues?view=open" });
  await screen.findByRole("table", { name: "Project issues table" });
  await waitFor(() => expect(requests).toEqual([{ open: true, project: "ACME" }]));
});

test("Finished reads only done issues closed in its default window", async () => {
  const now = spyOn(Date, "now").mockReturnValue(Date.parse("2026-11-11T01:06:30Z"));
  restores.push(() => now.mockRestore());
  const { requests } = table({ path: "/projects/ACME/issues?view=finished" });
  await screen.findByRole("table", { name: "Project issues table" });
  await expectRequest(requests, {
    closed_since: "2026-10-14T01:06:00.000Z",
    project: "ACME",
    status: "done",
  });
  expect(requests).not.toContainEqual({ project: "ACME" });
});

test("Finished sends both bounds for a one-day closed window", async () => {
  const { requests } = table({
    path: "/projects/ACME/issues?view=finished&f.closed=2026-11-03..2026-11-04",
  });
  await screen.findByRole("table", { name: "Project issues table" });
  await expectRequest(requests, {
    closed_before: "2026-11-04T00:00:00Z",
    closed_since: "2026-11-03T00:00:00Z",
    project: "ACME",
    status: "done",
  });
});

test("no Status pick reads open issues and the default done window", async () => {
  const now = spyOn(Date, "now").mockReturnValue(Date.parse("2026-11-11T01:06:30Z"));
  restores.push(() => now.mockRestore());
  const { requests } = table({ path: "/projects/ACME/issues?nofilter=1" });
  await screen.findByRole("table", { name: "Project issues table" });
  await expectRequest(requests, { open: true, project: "ACME" });
  await expectRequest(requests, {
    closed_since: "2026-10-14T01:06:00.000Z",
    project: "ACME",
    status: "done",
  });
});

test("f.closed=all reads the done listing without a closed bound", async () => {
  const { requests } = table({ path: "/projects/ACME/issues?view=finished&f.closed=all" });
  await screen.findByRole("table", { name: "Project issues table" });
  await expectRequest(requests, { project: "ACME", status: "done" });
});

test("an old status=done link never reads the full listing", async () => {
  const { requests } = table({ path: "/projects/ACME/issues?status=done" });
  await screen.findByRole("table", { name: "Project issues table" });
  await waitFor(() =>
    expect(requests.every((query) => query.open === true || query.status === "done")).toBe(true)
  );
});

test("a pending Legion settings read does not start a listing with the wrong view", async () => {
  const settings = Promise.withResolvers<LegionSetting[]>();
  const { requests } = table({
    path: "/projects/ACME/issues?view=finished",
    settings: settings.promise,
  });
  expect(screen.getByLabelText("Loading issues")).toBeDefined();
  expect(requests).toEqual([]);
  settings.resolve([]);
  await screen.findByRole("table", { name: "Project issues table" });
});

test("Finished draws its done rows while the board is pending, then applies its Legion filter", async () => {
  const { requests } = table({
    path: "/projects/ACME/issues?view=finished",
    settings: Promise.resolve([legionSetting()]),
  });
  await expectRequest(requests, { open: true, project: "ACME" });
  await waitFor(() =>
    expect(requests.some((query) => query.project === "ACME" && query.status === "done")).toBe(true)
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  await waitFor(() => socket.subscribed("legion"));
  await expect(screen.getByText("Waiting for Legion data")).toBeDefined();
  await waitFor(() => expect(document.querySelectorAll("[data-issue-row]")).toHaveLength(3));
  act(() =>
    socket.receive(snapshotFrame(socket.subscribed("legion").sub, boardWithFacts({ columns: [] })))
  );
  await waitFor(() => expect(document.querySelectorAll("[data-issue-row]")).toHaveLength(2));
  expect(screen.queryByText("Waiting for Legion data")).toBeNull();
});
