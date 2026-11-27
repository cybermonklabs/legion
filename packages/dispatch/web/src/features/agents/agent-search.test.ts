import { expect, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { act } from "@testing-library/react";

import { renderAddress } from "../../__tests__/render-address";
import { agentsAddress, useAgentFilters } from "./agent-search";

// The setter takes the fields that changed and writes those alone, so a caller hands it the one
// field its control moves rather than the whole narrowing read back with one field replaced:
// the old setter worked out what had changed by comparing each array's identity, which a caller
// that rebuilt a field it did not change (the group multi-select already does) would defeat, and
// a write of all four cuts `q`, `machine` and `role` at `SEARCH_QUERY_MAX`.
test("the setter writes the field it is handed and leaves every other whole", () => {
  const machines = Array.from({ length: 120 }, (_, index) => `host-${index}`).join(",");
  const query = "p".repeat(SEARCH_QUERY_MAX + 200);
  const hook = renderAddress(() => useAgentFilters(), [`/agents?machine=${machines}&q=${query}`]);
  try {
    const address = () => new URLSearchParams(hook.result.current.location.search);
    expect(hook.result.current.value[0].machines).toHaveLength(120);
    expect(hook.result.current.value[0].search).toBe(query);

    act(() => hook.result.current.value[1]({ roles: ["planner"] }));
    expect(address().get("role")).toBe("planner");
    expect(address().get("machine")).toBe(machines);
    expect(address().get("q")).toBe(query);

    act(() => hook.result.current.value[1]({ groups: ["active"] }));
    expect(address().get("group")).toBe("active");
    expect(address().get("role")).toBe("planner");
    expect(address().get("machine")).toBe(machines);
    expect(address().get("q")).toBe(query);
  } finally {
    hook.unmount();
  }
});

// The reader's own edit of a field is what cuts it, and an empty value takes its parameter out.
test("the field the reader edits is cut at the cap, and emptying it leaves the address", () => {
  const hook = renderAddress(() => useAgentFilters(), ["/agents?group=active"]);
  try {
    const address = () => new URLSearchParams(hook.result.current.location.search);
    act(() => hook.result.current.value[1]({ search: "x".repeat(SEARCH_QUERY_MAX + 5) }));
    expect(address().get("q")).toBe("x".repeat(SEARCH_QUERY_MAX));

    act(() => hook.result.current.value[1]({ groups: [] }));
    expect(address().has("group")).toBe(false);
    expect(address().get("q")).toBe("x".repeat(SEARCH_QUERY_MAX));
  } finally {
    hook.unmount();
  }
});

// Another page links here (the machine page's "Agents on this machine") through the page's own
// grammar rather than spelling `?machine=` itself, so the link is the address the page's setter
// writes for the same narrowing, encoded the same way, and it reads back as exactly that
// narrowing.
test("the link to the Agents page for a machine is the address the page's own grammar writes for it", () => {
  const names = ["relay-1", "sand-devbox.corp.example.com", "build host+1 & co #2"];
  for (const name of names) {
    const link = agentsAddress({ machines: [name] });
    if (link === null) throw new Error(`no link to the Agents page for ${name}`);
    const page = renderAddress(() => useAgentFilters(), ["/agents"]);
    try {
      act(() => page.result.current.value[1]({ machines: [name] }));
      const { pathname, search } = page.result.current.location;
      expect([name, link]).toEqual([name, `${pathname}${search}`]);
    } finally {
      page.unmount();
    }

    const linked = renderAddress(() => useAgentFilters(), [link]);
    try {
      expect([name, linked.result.current.value[0]]).toEqual([
        name,
        { groups: [], machines: [name], roles: [], search: "" },
      ]);
    } finally {
      linked.unmount();
    }
  }
});

// `?machine=` is cut at `SEARCH_QUERY_MAX`, so a link for a longer name would read back as the
// cut: a name no session reports, or another machine's. Such a name has no link, and a name of
// exactly the cap has one that reads back whole.
test("a machine name past the cap has no link to the Agents page, and a name at the cap has", () => {
  expect(agentsAddress({ machines: ["x".repeat(SEARCH_QUERY_MAX + 1)] })).toBeNull();

  const atCap = "x".repeat(SEARCH_QUERY_MAX);
  const link = `/agents?machine=${atCap}`;
  expect(agentsAddress({ machines: [atCap] })).toBe(link);
  const linked = renderAddress(() => useAgentFilters(), [link]);
  try {
    expect(linked.result.current.value[0].machines).toEqual([atCap]);
  } finally {
    linked.unmount();
  }
});
