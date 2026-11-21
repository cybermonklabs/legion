import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";

import type { BlockOf } from "./block-cells";
import { TableBlock } from "./TableBlock";

// A table the command bar's agent draws of issues counted by priority, as the server's chain
// answers it: each priority as the number the issue reads carry, and the issues with none in the
// group "none". The person reads each priority as Dispatch writes it everywhere else.
test("a table of issues by priority shows each priority as Dispatch writes it", () => {
  const block: BlockOf<"table"> = {
    kind: "table",
    title: "Open bugs by priority",
    spec: { source: "run", params: {}, table: "the chain's answer", columns: ["priority", "bugs"] },
    rows: [
      { priority: 1, bugs: 5 },
      { priority: 2, bugs: 2 },
      { priority: "none", bugs: 1 },
    ],
  };
  render(<TableBlock block={block} />);
  const rows = within(screen.getByTestId("going-block-table")).getAllByRole("row");
  expect(
    rows.slice(1).map((row) =>
      within(row)
        .getAllByRole("cell")
        .map((cell) => cell.textContent)
    )
  ).toEqual([
    ["P1", "5"],
    ["P2", "2"],
    ["none", "1"],
  ]);
});
