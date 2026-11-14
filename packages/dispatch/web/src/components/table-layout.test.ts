import { expect, test } from "bun:test";
import { reorder } from "./table-layout";

test("a dropped column lands immediately before the one it was dropped on", () => {
  const order = ["select", "key", "title", "status", "epic"];
  expect(reorder(order, "epic", "title")).toEqual(["select", "key", "epic", "title", "status"]);
  expect(reorder(order, "key", "epic")).toEqual(["select", "title", "status", "key", "epic"]);
  expect(order).toEqual(["select", "key", "title", "status", "epic"]);
});

test("a column dropped on itself, or on one the order does not hold, stays where it is", () => {
  const order = ["select", "key", "title"];
  expect(reorder(order, "title", "title")).toEqual(order);
  expect(reorder(order, "title", "claim")).toEqual(order);
  expect(reorder(order, "claim", "title")).toEqual(order);
});
