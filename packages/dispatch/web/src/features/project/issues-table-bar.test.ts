import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { IssuesTable } from "./IssuesTable";
import { segmentWidths } from "./issues-table-controls";
import { countBy, type Item, keyColumnWidth } from "./issues-table-model";

test("a segment is as wide as its share of the issues, and the segments fill the bar", () => {
  const widths = segmentWidths([50, 30, 20]);
  expect(widths).toEqual([50, 30, 20]);
  expect(widths.reduce((sum, width) => sum + width, 0)).toBeCloseTo(100, 6);
});

test("a count of one keeps a pressable width, and the bar is still exactly full", () => {
  const widths = segmentWidths([997, 1, 1, 1]);
  expect(widths.slice(1)).toEqual([3, 3, 3]);
  expect(widths.reduce((sum, width) => sum + width, 0)).toBeCloseTo(100, 6);
  // Fifteen phases, the most the Issues page draws: each is at least the floor of the bar, and
  // the one big segment keeps the rest.
  const many = segmentWidths([400, ...Array.from({ length: 14 }, () => 1)]);
  expect(many.length).toBe(15);
  expect(many.reduce((sum, width) => sum + width, 0)).toBeCloseTo(100, 6);
  expect(Math.min(...many)).toBe(3);
  expect(many[0]).toBeCloseTo(58, 6);
});

test("an empty bar divides itself evenly rather than dividing by zero", () => {
  expect(segmentWidths([0, 0])).toEqual([50, 50]);
  expect(segmentWidths([])).toEqual([]);
});

function stageItem(stage: string): Item {
  const [kind, id] = stage.split(":") as ["status" | "phase", string];
  return {
    issue: {
      labels: [],
      status: kind === "status" ? id : "in_progress",
    },
    ...(kind === "phase" ? { legion: { card: {}, id, label: id, order: 0 } } : {}),
  } as unknown as Item;
}

function slotItem(slotUse: string): Item {
  return {
    issue: { labels: [], status: "in_progress" },
    legion: { card: { slot_use: slotUse }, id: "implementing", label: "Implementing", order: 0 },
  } as unknown as Item;
}

test("counts the in-flight stages across every listed row", () => {
  const rows = [
    ...Array.from({ length: 17 }, () => stageItem("status:in_progress")),
    ...Array.from({ length: 79 }, () => stageItem("phase:implementing")),
    ...[
      "testing",
      "reviewing",
      "accepting",
      "merging",
      "awaiting_person",
      "held",
      "waiting_on_children",
    ].flatMap((id) => Array.from({ length: 20 }, () => stageItem(`phase:${id}`))),
    ...Array.from({ length: 20 }, () => stageItem("phase:other")),
  ];

  const counted = countBy(rows, "stage", true);
  const implementing = counted.find((entry) => entry.value === "phase:implementing");
  const implementingIndex = counted.indexOf(implementing as NonNullable<typeof implementing>);

  expect(rows).toHaveLength(256);
  expect(counted).toHaveLength(10);
  expect(implementing?.count).toBe(79);
  expect(segmentWidths(counted.map((entry) => entry.count))[implementingIndex]).toBeCloseTo(
    30.9,
    1
  );
});

test("counts only rows with a slot use in slot-use order", () => {
  const counted = countBy([slotItem("working"), slotItem("held_children")], "slot_use", true);

  expect(counted.map(({ count, label }) => [label, count])).toEqual([
    ["Working", 1],
    ["Held: children", 1],
  ]);
});

const restores: { mockRestore(): void }[] = [];
afterEach(() => {
  cleanup();
  for (const restore of restores.splice(0)) restore.mockRestore();
  localStorage.clear();
});

function issue(key: string, status: IssueSummary["status"]): IssueSummary {
  return {
    assignee: null,
    blocked_by: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-10T00:00:00Z",
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
    status,
    title: `Work ${key}`,
    updated_at: "2026-11-10T00:00:00Z",
  };
}

function renderIssuesTable(issues: IssueSummary[], search: string) {
  restores.push(
    spyOn(api, "listIssues").mockResolvedValue(issues),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "listLegionSettings").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" })
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
    createElement(
      MemoryRouter,
      { initialEntries: [`/projects/CORE/issues${search}`] },
      createElement(
        QueryClientProvider,
        { client },
        createElement(KeymapProvider, null, createElement(IssuesTable, { project: "CORE" }))
      )
    )
  );
  restores.push({
    mockRestore: () => {
      view.unmount();
      client.clear();
    },
  });
}

test("does not draw a bar for the Triage view", async () => {
  renderIssuesTable([issue("CORE-1", "triage")], "?view=triage");

  await screen.findByRole("table", { name: "Project issues table" });
  expect(document.querySelector("[data-issues-bar]")).toBeNull();
});

test("titles the bar with every listed row rather than every loaded row", async () => {
  renderIssuesTable(
    Array.from({ length: 1140 }, (_, index) =>
      issue(`CORE-${index + 1}`, index < 113 ? "triage" : "todo")
    ),
    "?status=triage"
  );

  await screen.findByRole("table", { name: "Project issues table" });
  expect(
    document.querySelector("[data-testid='issues-table-scroll']")?.getAttribute("data-row-count")
  ).toBe("113");
  expect(document.querySelector("[data-issues-counted]")?.textContent).toBe("113 issues");
});

test("the Key column is wide enough for the project's longest key", () => {
  expect(keyColumnWidth("INFRA-1508".length)).toBeGreaterThan(100);
  expect(keyColumnWidth("OSSV-1".length)).toBeLessThan(keyColumnWidth("INFRA-1508".length));
  // A key no project writes never widens the column past the title.
  expect(keyColumnWidth(80)).toBe(176);
});
