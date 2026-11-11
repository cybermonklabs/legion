import { afterEach, expect, test } from "bun:test";
import { type ColumnDef, flexRender, type SortingState } from "@tanstack/react-table";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useEffect } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { DataTable, type GridColumn, gridColumn, useDataTable } from "./DataTable";

interface Row {
  key: string;
  title: string;
  hours: number;
  group: string;
}

const columns: readonly ColumnDef<Row>[] = [
  gridColumn<Row>({ id: "key", header: "Key", value: (row) => row.key }),
  gridColumn<Row>({ id: "title", header: "Title", value: (row) => row.title }),
  gridColumn<Row>({ id: "hours", header: "Hours", value: (row) => row.hours }),
];

const rows: readonly Row[] = [
  { key: "CORE-1", title: "First row", hours: 9, group: "a" },
  { key: "CORE-2", title: "Second row", hours: 2, group: "b" },
  { key: "CORE-3", title: "Third row", hours: 5, group: "a" },
];

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

let measuredWidth = 1000;

async function remeasure(width: number): Promise<void> {
  measuredWidth = width;
  await act(async () => {
    window.dispatchEvent(new Event("resize"));
  });
}

function Location(): ReactNode {
  const location = useLocation();
  return (
    <output data-location={`${location.pathname}${location.search}`} data-testid="data-location" />
  );
}

function CoreTable({
  columns: tableColumns = columns,
  groupBy,
  groupOrder,
  initialSort,
  layoutKey = "T",
  pageSize,
  primary,
  rows: tableRows = rows,
  stateKey,
  columnsOverride,
  fit,
  listedTotal,
}: {
  columns?: readonly ColumnDef<Row>[];
  groupOrder?: (columnId: string, a: string, b: string) => number;
  groupBy?: string;
  initialSort?: SortingState;
  layoutKey?: string;
  pageSize?: number | "all";
  primary?: readonly string[];
  rows?: readonly Row[];
  stateKey?: string;
  columnsOverride?: readonly string[];
  fit?: "window" | "rows";
  listedTotal?: number;
}): ReactNode {
  const model = useDataTable({
    columns: tableColumns,
    columnsOverride,
    fit,
    getRowId: (row) => row.key,
    groupOrder,
    groupValue: (columnId, row) => (columnId === "group" ? row.group : ""),
    initialSort,
    layoutKey,
    pageSize,
    primary,
    listedTotal,
    rows: tableRows,
    stateKey,
  });
  useEffect(() => {
    if (groupBy !== undefined) model.table.setGrouping([groupBy]);
  }, [groupBy, model.table]);
  return (
    <>
      <output
        data-listed={model.listedLeaves.length}
        data-page={model.pageLeaves.length}
        data-testid="model-counts"
      />
      <DataTable
        label="Rows"
        model={model}
        renderGroup={({ entry, index, measure }) => (
          <tr data-group={String(entry.row.groupingValue)} data-index={index} ref={measure}>
            <td>{String(entry.row.groupingValue)}</td>
          </tr>
        )}
        renderRow={({ row, index, measure }) => (
          <tr data-index={index} data-row={row.id} ref={measure}>
            {row.getVisibleCells().map((cell) => (
              <td className="bg-white" key={cell.id}>
                {flexRender(cell.column.columnDef.cell, cell.getContext())}
              </td>
            ))}
          </tr>
        )}
      />
    </>
  );
}

test("the pager names an unfiltered view once and a filtered view against its own total", async () => {
  const view = draw(<CoreTable listedTotal={3} />);
  try {
    expect(screen.getByTestId("issues-table-pager")).toHaveTextContent("3 issues");
    await act(async () => {
      view.rerender(
        <MemoryRouter initialEntries={["/projects/T/issues"]}>
          <CoreTable listedTotal={5} />
        </MemoryRouter>
      );
    });
    expect(screen.getByTestId("issues-table-pager")).toHaveTextContent("3 of 5 issues");
  } finally {
    view.unmount();
  }
});

function draw(tree: ReactNode, boxWidth = 1000) {
  measuredWidth = boxWidth;
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "data-table-scroll" ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const rect = HTMLElement.prototype.getBoundingClientRect;
  HTMLElement.prototype.getBoundingClientRect = function bounds(this: HTMLElement) {
    const measured = rect.call(this) as DOMRect;
    if (this.dataset.testid !== "data-table-scroll") return measured;
    return { ...measured.toJSON(), width: measuredWidth, height: 600 } as DOMRect;
  };
  const view = render(<MemoryRouter initialEntries={["/projects/T/issues"]}>{tree}</MemoryRouter>);
  restores.push(() => {
    view.unmount();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
    HTMLElement.prototype.getBoundingClientRect = rect;
  });
  return view;
}

function rowKeys(): string[] {
  return screen
    .getAllByRole("row")
    .slice(1)
    .map((row) => row.getAttribute("data-row"))
    .filter((key): key is string => key !== null);
}

test("renders the rows it is given with a gridColumn", async () => {
  draw(<CoreTable />);

  expect(document.querySelectorAll("[data-row]")).toHaveLength(3);
  expect(screen.getByTestId("data-table-scroll")).toHaveAttribute("data-row-count", "3");
});

test("sorts in memory from initialSort", async () => {
  draw(<CoreTable initialSort={[{ id: "hours", desc: true }]} />);

  await screen.findByRole("table", { name: "Rows" });
  expect(rowKeys()).toEqual(["CORE-1", "CORE-3", "CORE-2"]);
});

test("keeps the sort in the URL under stateKey", async () => {
  const user = userEvent.setup();
  draw(
    <>
      <Location />
      <CoreTable stateKey="fin" />
    </>
  );

  const table = await screen.findByRole("table", { name: "Rows" });
  await user.click(within(table).getByRole("button", { name: "Sort by hours" }));
  expect(screen.getByTestId("data-location")).toHaveAttribute(
    "data-location",
    "/projects/T/issues?fin.sort=hours"
  );
});

test("listedLeaves counts every page", async () => {
  const many = Array.from({ length: 60 }, (_, index) => ({
    group: "a",
    hours: index,
    key: `CORE-${index + 1}`,
    title: `Row ${index + 1}`,
  }));
  draw(<CoreTable pageSize={25} rows={many} />);

  await screen.findByRole("table", { name: "Rows" });
  const counts = screen.getByTestId("model-counts");
  expect(counts).toHaveAttribute("data-listed", "60");
  expect(counts).toHaveAttribute("data-page", "25");
});

test("columnsOverride shows only the named columns and writes nothing", async () => {
  localStorage.setItem(
    "dispatch.project-table.v2.T",
    JSON.stringify({ order: [], sizing: { hours: 200 }, visibility: {} })
  );
  const before = localStorage.getItem("dispatch.project-table.v2.T");
  draw(<CoreTable columnsOverride={["key"]} />);

  const table = await screen.findByRole("table", { name: "Rows" });
  expect(within(table).getByRole("columnheader", { name: /^Key/ })).toBeDefined();
  expect(within(table).queryByRole("columnheader", { name: /^Hours/ })).toBeNull();
  expect(localStorage.getItem("dispatch.project-table.v2.T")).toBe(before);
});

test("a version-one layout snapshot cannot override current default columns", async () => {
  localStorage.setItem(
    "dispatch.project-table.T",
    JSON.stringify({ order: [], sizing: {}, visibility: { hours: false } })
  );
  const view = draw(<CoreTable />);
  try {
    const table = await screen.findByRole("table", { name: "Rows" });
    expect(within(table).getByRole("columnheader", { name: /^Hours/ })).toBeTruthy();
    expect(localStorage.getItem("dispatch.project-table.v2.T")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("changing layout keys reloads that view's stored columns", async () => {
  localStorage.setItem(
    "dispatch.project-table.v2.T.one",
    JSON.stringify({ order: [], sizing: {}, visibility: { hours: false } })
  );
  localStorage.setItem(
    "dispatch.project-table.v2.T.two",
    JSON.stringify({ order: [], sizing: {}, visibility: {} })
  );
  const view = draw(<CoreTable layoutKey="T.one" />);
  const table = await screen.findByRole("table", { name: "Rows" });
  expect(within(table).queryByRole("columnheader", { name: /^Hours/ })).toBeNull();

  await act(async () => {
    view.rerender(
      <MemoryRouter initialEntries={["/projects/T/issues"]}>
        <CoreTable layoutKey="T.two" />
      </MemoryRouter>
    );
  });

  const changed = screen.getByRole("table", { name: "Rows" });
  expect(within(changed).getByRole("columnheader", { name: /^Hours/ })).toBeDefined();
});

test("the phone layout draws the title under the key", async () => {
  const primaryHours: GridColumn<Row> = {
    header: "Hours",
    id: "hours",
    primary: true,
    value: (row) => row.hours,
  };
  const view = draw(
    <CoreTable
      columns={[
        gridColumn<Row>({ id: "key", header: "Key", value: (row) => row.key }),
        gridColumn<Row>({ id: "title", header: "Title", value: (row) => row.title }),
        gridColumn<Row>(primaryHours),
      ]}
      primary={["hours"]}
      rows={[rows[0] as Row]}
    />,
    390
  );

  const table = await screen.findByRole("table", { name: "Rows" });
  expect(within(table).queryByRole("columnheader", { name: "Title" })).toBeNull();
  const titleRow = view.container.querySelector('[data-grid-title-row="CORE-1"]') as HTMLElement;
  expect(titleRow).toBeTruthy();
  expect(within(titleRow).getByText("First row")).toBeDefined();
  const keyCell = view.container.querySelector('tbody tr:not([aria-hidden]) td') as HTMLElement;
  expect(keyCell.className).not.toContain("border-b");
  expect(titleRow.firstElementChild?.className).toContain("border-b");
  expect(keyCell.className).toContain("bg-white");
  expect(titleRow.firstElementChild?.className).toContain("bg-white");
});

test("groups in the caller's group order", async () => {
  const groupColumn = gridColumn({
    id: "group",
    header: "Group",
    value: (row: Row) => row.group,
  });
  draw(
    <CoreTable
      columns={[...columns, groupColumn]}
      groupBy="group"
      groupOrder={(_column, a, b) => b.localeCompare(a)}
      rows={rows}
    />
  );

  const table = await screen.findByRole("table", { name: "Rows" });
  await screen.findAllByText("b");
  expect(
    [...table.querySelectorAll("[data-group]")].map((row) => row.getAttribute("data-group"))
  ).toEqual(["b", "a"]);
});

test("the empty line has no sentence", async () => {
  const view = draw(<CoreTable rows={[]} />);

  expect(view.container.querySelector("[data-table-empty]")).toHaveTextContent(
    "No matching issues"
  );
});

test("layouts stay separate and fit rows draws every row", async () => {
  const view = draw(
    <>
      <CoreTable fit="rows" layoutKey="T.one" pageSize={10} rows={rows} />
      <CoreTable layoutKey="T.two" rows={rows} />
    </>
  );

  await screen.findAllByRole("table", { name: "Rows" });
  expect(view.container.querySelectorAll("[data-row]")).toHaveLength(6);
  const boxes = view.container.querySelectorAll<HTMLElement>("[data-testid=data-table-scroll]");
  expect(boxes[0]?.style.height).toBe("");
  expect(boxes[0]?.className).toContain("overflow-x-auto");
  expect(boxes[1]?.style.height).not.toBe("");
});

test("a zero-width measurement leaves the layout wide", async () => {
  const view = draw(<CoreTable primary={["hours"]} rows={[rows[0] as Row]} />, 1280);
  const table = await screen.findByRole("table", { name: "Rows" });
  const wide = table.getAttribute("style");

  await remeasure(0);

  expect(within(table).getByRole("columnheader", { name: /^Title/ })).toBeDefined();
  expect(view.container.querySelector("[data-grid-title-row]")).toBeNull();
  expect(table.getAttribute("style")).toBe(wide);
});
