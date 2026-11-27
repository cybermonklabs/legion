import { afterEach, expect, test } from "bun:test";
import type { Column } from "@tanstack/react-table";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ColumnsMenu } from "./ColumnsMenu";

afterEach(cleanup);

/** A table column as the menu reads one: its header, whether the reader may hide it, whether it
 *  is shown, the method it describes, and the toggles it was asked for. */
function column(
  id: string,
  header: string,
  options: {
    readonly describe?: string;
    readonly hideable?: boolean;
    readonly toggled?: string[];
    readonly visible?: boolean;
  } = {}
): Column<unknown, unknown> {
  return {
    columnDef: {
      header,
      meta: options.describe === undefined ? {} : { describe: options.describe },
    },
    getCanHide: () => options.hideable ?? true,
    getIsVisible: () => options.visible ?? true,
    getToggleVisibilityHandler: () => () => options.toggled?.push(id),
    id,
  } as unknown as Column<unknown, unknown>;
}

const WORK_TIME =
  "Dispatch counts the hours from the first Legion worker event until the issue closes, or until Dispatch last read it. It does not count Backlog or Icebox hours, or hours when Legion has admitted the issue but not started it.";

test("a column that names its method has it as a muted line under its name, its box's description", async () => {
  const user = userEvent.setup();
  const toggled: string[] = [];
  render(
    <ColumnsMenu
      columns={[
        column("select", "", { hideable: false }),
        column("title", "Title"),
        column("work_hours", "Work time", { describe: WORK_TIME, toggled, visible: false }),
      ]}
    />
  );
  await user.click(screen.getByRole("button", { name: "Columns" }));
  const menu = screen.getByRole("dialog", { name: "Columns" });
  // A column the reader may not hide is not offered.
  expect(
    within(menu)
      .getAllByRole("checkbox")
      .map((box) => box.closest("label")?.textContent)
  ).toEqual(["Title", "Work time"]);
  const work = within(menu).getByRole("checkbox", { name: "Work time" });
  expect(work).not.toBeChecked();
  expect(work).toHaveAccessibleDescription(WORK_TIME);
  expect(within(menu).getByText(WORK_TIME).tagName).toBe("P");
  // A column with no method has no line and no description.
  const title = within(menu).getByRole("checkbox", { name: "Title" });
  expect(title).toBeChecked();
  expect(title).not.toHaveAttribute("aria-describedby");
  expect(menu.querySelectorAll("p")).toHaveLength(1);
  await user.click(work);
  expect(toggled).toEqual(["work_hours"]);
});
