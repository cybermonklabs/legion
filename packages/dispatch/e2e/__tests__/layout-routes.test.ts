import { describe, expect, test } from "bun:test";

import { layoutRoutes, layoutUnread } from "../layout-routes";
import { reviewRoutes } from "../review-routes";

describe("the layout layer's route list", () => {
  test("reads every reviewed page, and every excluded one it does not leave out with a reason", () => {
    const read = new Set(layoutRoutes.map((route) => route.name));
    const missing = reviewRoutes
      .filter((route) => !read.has(route.name) && layoutUnread[route.name] === undefined)
      .map((route) => `${route.name} (${route.kind})`);
    expect(missing).toEqual([]);
    const reviewed = reviewRoutes.filter((route) => route.kind === "reviewed");
    expect(reviewed.filter((route) => layoutUnread[route.name] !== undefined)).toEqual([]);
    const leftOut = Object.entries(layoutUnread).filter(
      ([name, reason]) => read.has(name) || reason.trim() === ""
    );
    expect(leftOut).toEqual([]);
  });

  test("gives every route its own name", () => {
    const names = layoutRoutes.map((route) => route.name);
    expect(names.filter((name, index) => names.indexOf(name) !== index)).toEqual([]);
  });
});
