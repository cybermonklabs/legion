import { describe, expect, test } from "bun:test";

import { parseInboxSearch } from "../refs/routes";
import { narrowingOf, opensPlain, withRemembered } from "./remembered-view";

describe("narrowingOf", () => {
  test("keeps the filters, the assignee choice, the sort and the grouping", () => {
    expect(
      narrowingOf(
        parseInboxSearch("?urgency=high&assignee=everyone&sort=urgency&group=project&q=x&item=a")
      )
    ).toEqual({
      assignees: [],
      filters: { urgency: ["high"] },
      group: "project",
      sort: "urgency",
    });
  });

  test("leaves out a field that holds its default", () => {
    expect(narrowingOf({ filters: {}, group: "priority", sort: "newest" })).toEqual({});
  });
});

describe("opensPlain", () => {
  test("a bare address and the dismissed list open plain", () => {
    expect(opensPlain(parseInboxSearch(""))).toBe(true);
    expect(opensPlain(parseInboxSearch("?show=dismissed"))).toBe(true);
    expect(opensPlain(parseInboxSearch("?group=priority"))).toBe(true);
  });

  test("an address that names a view, an agent, a row or a search does not", () => {
    for (const search of [
      "?group=project",
      "?priority=P0",
      "?assignee=everyone",
      "?agent=s1",
      "?section=needs-you",
      "?item=a",
      "?q=word",
    ]) {
      expect(opensPlain(parseInboxSearch(search))).toBe(false);
    }
  });
});

describe("withRemembered", () => {
  const remembered = { filters: { urgency: ["high"] }, group: "project" as const };

  test("fills a plain address", () => {
    expect(withRemembered({ show: "dismissed" }, remembered)).toEqual({
      filters: { urgency: ["high"] },
      group: "project",
      show: "dismissed",
    });
  });

  test("leaves an address that names a view as it is, so a link opens on what it names", () => {
    const route = parseInboxSearch("?sort=priority");
    expect(withRemembered(route, remembered)).toBe(route);
  });

  test("leaves the route itself when nothing is remembered", () => {
    const route = {};
    expect(withRemembered(route, {})).toBe(route);
  });
});
