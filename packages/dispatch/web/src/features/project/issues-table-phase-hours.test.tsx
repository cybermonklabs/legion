import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import {
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type { IssueSummary, LegionBoardCard, LegionBoardResponse } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { IssuesTable } from "./IssuesTable";

let sockets: FakeLiveSockets;
const spies: { mockRestore(): void }[] = [];
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  cleanup();
  sockets.restore();
  for (const spy of spies.splice(0)) spy.mockRestore();
  localStorage.clear();
});

function issue(key: string): IssueSummary {
  return {
    assignee: null,
    blocked_by: [],
    claim: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    key,
    labels: ["legion"],
    last_seq: 2,
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
    status: "in_progress",
    title: `Work ${key}`,
    updated_at: "2026-11-09T00:00:00Z",
  };
}
function card(key: string): LegionBoardCard {
  return {
    blocked_by: [],
    checks_verdict: null,
    children: 0,
    key,
    phase: "implementing",
    phase_since: "2026-11-09T22:00:00Z",
    priority: 1,
    roles: [],
    slot: null,
    title: `Work ${key}`,
    tree: key,
    tree_slot: null,
    slot_since: null,
    waiting_place: null,
    waiting_on: null,
  };
}
/** Two issues Legion worked and one it did not: ACME-1 has the longer total. */
const BOARD: LegionBoardResponse = {
  columns: [
    {
      cards: [card("ACME-1"), card("ACME-2"), card("ACME-3")],
      id: "implementing",
      label: "Implementing",
    },
  ],
  computed_at: "2026-11-10T00:00:00Z",
  header: {
    queue_length: 0,
    read_at: "2026-11-10T00:00:00Z",
    read_error: null,
    read_ok: true,
    slot_cap: 4,
    slots_in_use: 1,
    trees: 1,
  },
  phase_hours: {
    "ACME-1": { implementing: 6, planning: 2 },
    "ACME-2": { implementing: 1, planning: 1 },
  },
  project: "ACME",
  setting_sources: {},
  settings: {},
};

async function table() {
  spies.push(
    spyOn(api, "listIssues").mockResolvedValue([issue("ACME-1"), issue("ACME-2"), issue("ACME-3")]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "listLegionSettings").mockResolvedValue([
      {
        created_by: { id: "alice", kind: "user" },
        project: "ACME",
        state_url: "http://legion.example:13370",
        updated_at: "2026-11-01T00:00:00Z",
      },
    ])
  );
  // The unit DOM has no layout engine, and the rows are virtual.
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "issues-table-scroll"
        ? 600
        : (descriptor?.get?.call(this) ?? 0);
    },
  });
  spies.push({
    mockRestore: () => {
      if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
      else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
    },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  render(
    <MemoryRouter initialEntries={["/projects/ACME/issues"]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <IssuesTable project="ACME" />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  const frame = await waitFor(() => socket.subscribed("legion"));
  act(() => socket.receive(snapshotFrame(frame.sub, BOARD)));
  await screen.findByRole("table", { name: "Project issues table" });
  spies.push({ mockRestore: () => client.clear() });
}

/** Shows the column the reader has to ask for, and answers with the table. */
async function showTimePerPhase(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "Columns" }));
  await user.click(screen.getByRole("checkbox", { name: "Time per phase" }));
  await user.keyboard("{Escape}");
  return screen.getByRole("table", { name: "Project issues table" });
}

test("Time per phase is offered in the column picker and drawn only once it is picked", async () => {
  const user = userEvent.setup();
  await table();
  expect(document.querySelector("[data-phase-hours-bar]")).toBeNull();
  const grid = await showTimePerPhase(user);
  await waitFor(() => expect(document.querySelectorAll("[data-phase-hours-bar]").length).toBe(2));
  // The issue the board holds no hours for keeps an empty cell.
  expect(
    within(grid)
      .getByRole("row", { name: /ACME-3/ })
      .querySelector("[data-phase-hours-bar]")
  ).toBeNull();
});

test("the column sorts by an issue's total hours", async () => {
  const user = userEvent.setup();
  await table();
  const grid = await showTimePerPhase(user);
  const keys = () =>
    [...grid.querySelectorAll("[data-issue-row]")].map((row) => row.getAttribute("data-issue-row"));
  await user.click(screen.getByRole("button", { name: "Sort by phase_hours" }));
  // Descending first: the issue with the most hours leads, the one with none last.
  expect(keys()).toEqual(["ACME-1", "ACME-2", "ACME-3"]);
  await user.click(screen.getByRole("button", { name: "Sort by phase_hours" }));
  expect(keys()).toEqual(["ACME-2", "ACME-1", "ACME-3"]);
});

test("Hours draws each bar against the longest row; Share draws every bar full length", async () => {
  const user = userEvent.setup();
  await table();
  await showTimePerPhase(user);
  const width = (key: string) =>
    (
      document.querySelector(
        `[data-issue-row="${key}"] [data-phase-hours-bar] > span`
      ) as HTMLElement | null
    )?.style.width;
  // 8 hours against 8, and 2 against the same 8.
  expect(width("ACME-1")).toBe("100%");
  expect(width("ACME-2")).toBe("25%");

  await user.click(screen.getByRole("button", { name: "Show shares" }));
  expect(width("ACME-1")).toBe("100%");
  expect(width("ACME-2")).toBe("100%");
});

test("the header's hours/share switch changes the bars and leaves the sort alone", async () => {
  const user = userEvent.setup();
  await table();
  const grid = await showTimePerPhase(user);
  const keys = () =>
    [...grid.querySelectorAll("[data-issue-row]")].map((row) => row.getAttribute("data-issue-row"));
  const before = keys();
  await user.click(screen.getByRole("button", { name: "Show shares" }));
  expect(
    document.querySelector("[data-phase-hours-bar]")?.getAttribute("data-phase-hours-bar")
  ).toBe("share");
  expect(keys()).toEqual(before);
  await user.click(screen.getByRole("button", { name: "Show hours" }));
  expect(
    document.querySelector("[data-phase-hours-bar]")?.getAttribute("data-phase-hours-bar")
  ).toBe("hours");
  expect(keys()).toEqual(before);
});
