import { expect, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";

import type { TableColumn } from "./priority-table-model";
import { priorityTableGrammar } from "./priority-table-view";

interface Row {
  readonly owner: string;
  readonly title: string;
}

const COLUMNS: readonly TableColumn<Row>[] = [
  { id: "title", header: "Priority", size: 420, min: 180, text: (row) => row.title },
  { id: "owner", header: "Owner", size: 200, min: 160, values: (row) => [row.owner] },
];

const GRAMMAR = priorityTableGrammar("p", COLUMNS, [{ id: "title", desc: false }]);

// An unrelated write must leave the address's own value whole: the search box caps what the
// reader types (`useTableAddress`'s `setSearch`), and this grammar rewrites every field of the
// view, so a cut on the way in would shorten a longer `?p.q=` a link carried as soon as the
// reader pressed a header or stepped a page.
test("a sort or a page step leaves a longer ?p.q= a link carried whole", () => {
  const long = "y".repeat(SEARCH_QUERY_MAX + 42);
  const linked = new URLSearchParams(`p.q=${long}`);
  expect(GRAMMAR.read(linked).search).toBe(long);
  const sorted = GRAMMAR.write(linked, {
    ...GRAMMAR.read(linked),
    sorting: [{ id: "owner", desc: true }],
  });
  expect(sorted.get("p.q")).toBe(long);
  expect(sorted.get("p.sort")).toBe("-owner");
  const paged = GRAMMAR.write(sorted, { ...GRAMMAR.read(sorted), page: 3 });
  expect(paged.get("p.q")).toBe(long);
  expect(paged.get("p.page")).toBe("4");
});
