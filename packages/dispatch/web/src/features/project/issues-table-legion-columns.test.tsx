import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import {
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { api } from "../../api/client";
import type { IssueSummary, LegionBoardResponse } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { IssuesTable } from "./IssuesTable";
import { boardWithFacts } from "./legion-board.fixture";

let sockets: FakeLiveSockets;
const restores: { mockRestore(): void }[] = [];

beforeEach(() => {
  sockets = installFakeLiveSockets();
});

afterEach(() => {
  cleanup();
  sockets.restore();
  for (const restore of restores.splice(0)) restore.mockRestore();
  localStorage.clear();
});

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    assignee: null,
    blocked_by: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-10T00:00:00Z",
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
    updated_at: "2026-11-10T00:00:00Z",
    ...overrides,
  };
}

async function renderTable({
  board = boardWithFacts(),
  hasLegion = true,
  issues = Array.from({ length: 7 }, (_, index) => issue(`ACME-${index + 1}`)),
}: {
  board?: LegionBoardResponse;
  hasLegion?: boolean;
  issues?: IssueSummary[];
} = {}) {
  restores.push(
    spyOn(api, "listIssues").mockImplementation(async (options = {}) =>
      issues.filter((item) => !options.open || item.status !== "done")
    ),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "listLegionSettings").mockResolvedValue(
      hasLegion
        ? [
            {
              created_by: { id: "alice", kind: "user" },
              project: "ACME",
              state_url: "http://legion.example:13370",
              updated_at: "2026-11-10T00:00:00Z",
            },
          ]
        : []
    )
  );
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "issues-table-scroll"
        ? 600
        : (descriptor?.get?.call(this) ?? 0);
    },
  });
  restores.push({
    mockRestore: () => {
      if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
      else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
    },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={["/projects/ACME/issues"]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <IssuesTable project="ACME" />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  restores.push({ mockRestore: () => client.clear() });
  if (hasLegion) {
    await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
    const socket = sockets.latest();
    act(() => socket.open());
    const frame = await waitFor(() => socket.subscribed("legion"));
    act(() => socket.receive(snapshotFrame(frame.sub, board)));
  }
  await screen.findByRole("table", { name: "Project issues table" });
  return view;
}

async function showColumns(user: UserEvent, names: readonly string[]) {
  await user.click(screen.getByRole("button", { name: "Columns" }));
  for (const name of names) {
    const checkbox = screen.getByRole("checkbox", {
      name: new RegExp(`^${name}$`),
    }) as HTMLInputElement;
    if (!checkbox.checked) await user.click(checkbox);
  }
  await user.keyboard("{Escape}");
}

function cell(key: string, column: string): HTMLElement | null {
  return document.querySelector(`[data-issue-row="${key}"] [data-cell="${column}"]`);
}

test("Place exposes each card's waiting_place rather than the listing order", async () => {
  await renderTable({
    issues: [issue("ACME-3"), issue("ACME-4"), issue("ACME-5")],
  });

  expect(cell("ACME-3", "queue")?.dataset.value).toBe("3");
  expect(cell("ACME-3", "queue")?.textContent).toBe("#3");
  expect(cell("ACME-4", "queue")?.dataset.value).toBe("1");
  expect(cell("ACME-4", "queue")?.textContent).toBe("#1");
  expect(cell("ACME-5", "queue")?.dataset.value).toBe("2");
  expect(cell("ACME-5", "queue")?.textContent).toBe("#2");
});

test("Key exposes its issue key as a cell value", async () => {
  await renderTable({ issues: [issue("ACME-1")] });

  expect(cell("ACME-1", "key")?.dataset.value).toBe("ACME-1");
});

test("Legion cells expose the board values that their row reads", async () => {
  const user = userEvent.setup();
  await renderTable();
  await showColumns(user, [
    "Work time",
    "Rounds",
    "Slot use",
    "Waited",
    "Waiting",
    "Tree",
    "In phase",
    "Stage",
  ]);

  expect(cell("ACME-1", "work_hours")?.dataset.value).toBe("8");
  expect(cell("ACME-1", "work_hours")?.textContent).toBe("8.0 h");
  expect(cell("ACME-1", "review_rounds")?.dataset.value).toBe("1");
  expect(cell("ACME-1", "slot_use")?.dataset.value).toBe("working");
  expect(cell("ACME-1", "slot_use")?.textContent).toBe("Working");
  expect(cell("ACME-6", "waiting_reason")?.dataset.value).toBe("blocked");
  expect(cell("ACME-6", "waiting_reason")?.textContent).toBe("Blocked by ACME-1");
  expect(cell("ACME-1", "legion")?.dataset.value).toBe("ACME-1");
  expect(cell("ACME-1", "legion")?.textContent).toBe("Root");
  expect(cell("ACME-1", "time_in_phase")?.dataset.value).toBe("2026-11-10T00:00:00Z");
  expect(cell("ACME-1", "stage")?.dataset.value).toBe("phase:implementing");
});

test("Phase and In phase fall back to the slot admission time", async () => {
  const base = boardWithFacts();
  const board = {
    ...base,
    columns: base.columns.map((column) => ({
      ...column,
      cards: column.cards.map((card) =>
        card.key === "ACME-1"
          ? { ...card, phase_since: null, slot_since: "2026-11-10T00:00:00Z" }
          : card
      ),
    })),
  };
  const user = userEvent.setup();
  await renderTable({ board });
  await showColumns(user, ["In phase"]);

  expect(cell("ACME-1", "phase")?.textContent).toContain("since admission");
  expect(cell("ACME-1", "time_in_phase")?.textContent).toContain("since admission");
});

test("a project without Legion hides its optional cells while keeping Place", async () => {
  await renderTable({ hasLegion: false });

  for (const name of [
    "Work time",
    "Rounds",
    "Slot use",
    "Waited",
    "Waiting",
    "Tree",
    "In phase",
    "Time per phase",
  ])
    expect(screen.queryByRole("columnheader", { name: new RegExp(name) })).toBeNull();
  expect(cell("ACME-1", "queue")).not.toBeNull();
});

test("Time per phase starts in Share mode and restores Hours for the table layout", async () => {
  const user = userEvent.setup();
  const first = await renderTable();
  await showColumns(user, ["Time per phase"]);

  const mode = () => screen.getByRole("group", { name: "Time per phase mode" });
  expect(within(mode()).getByRole("button", { name: "Share" }).getAttribute("aria-pressed")).toBe(
    "true"
  );
  await user.click(within(mode()).getByRole("button", { name: "Hours" }));
  expect(within(mode()).getByRole("button", { name: "Hours" }).getAttribute("aria-pressed")).toBe(
    "true"
  );

  first.unmount();
  await renderTable();
  await showColumns(user, ["Time per phase"]);
  expect(within(mode()).getByRole("button", { name: "Hours" }).getAttribute("aria-pressed")).toBe(
    "true"
  );
});

test("Hours scales Time per phase to the current page", async () => {
  const user = userEvent.setup();
  const issues = Array.from({ length: 30 }, (_, index) => issue(`ACME-${index + 1}`));
  const facts = Object.fromEntries(
    issues.map((item, index) => [
      item.key,
      {
        phase_hours: { implementing: index + 1 },
        review_rounds: 0,
        root: true,
        tree: item.key,
        work_hours: index + 1,
      },
    ])
  );
  await renderTable({ board: boardWithFacts({ columns: [], issues: facts }), issues });
  await showColumns(user, ["Time per phase"]);
  await user.click(
    within(screen.getByRole("group", { name: "Time per phase mode" })).getByRole("button", {
      name: "Hours",
    })
  );
  await user.selectOptions(screen.getByRole("combobox", { name: "Rows per page" }), "25");
  await waitFor(() =>
    expect(screen.getByTestId("issues-table-pager")).toHaveTextContent("Rows 1–25 of 30")
  );
  expect(
    cell("ACME-25", "phase_hours")
      ?.querySelector("[data-phase-hours-length]")
      ?.getAttribute("data-phase-hours-length")
  ).toBe("100");

  await user.click(screen.getByRole("button", { name: "Next ›" }));
  await waitFor(() =>
    expect(screen.getByTestId("issues-table-pager")).toHaveTextContent("Rows 26–30 of 30")
  );
  expect(
    cell("ACME-30", "phase_hours")
      ?.querySelector("[data-phase-hours-length]")
      ?.getAttribute("data-phase-hours-length")
  ).toBe("100");
});

test("Phase labels and Time per phase bars link to the issue timeline only with Legion data", async () => {
  const user = userEvent.setup();
  await renderTable();
  await showColumns(user, ["Phase", "Time per phase"]);

  expect(
    cell("ACME-1", "phase")?.querySelector("a[data-timeline-link]")?.getAttribute("href")
  ).toBe("/projects/ACME/legion/ACME-1");
  expect(
    cell("ACME-1", "phase_hours")?.querySelector("a[data-timeline-link]")?.getAttribute("href")
  ).toBe("/projects/ACME/legion/ACME-1");
  expect(cell("ACME-6", "phase_hours")?.querySelector("a[data-timeline-link]")).toBeNull();
});
