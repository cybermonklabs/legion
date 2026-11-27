import { expect, test } from "bun:test";
import { act, waitFor } from "@testing-library/react";

import { renderAddress } from "../../__tests__/render-address";
import { buildInboxPath, type InboxRoute, parseInboxSearch, useInboxRoute } from "./inbox-route";

test("inbox agent filters round-trip through the query string and ignore unknown sections", () => {
  expect(buildInboxPath()).toBe("/");
  expect(buildInboxPath({ agent: "s 1" })).toBe("/?agent=s+1");
  expect(buildInboxPath({ section: "needs-you" })).toBe("/?section=needs-you");
  expect(parseInboxSearch("?agent=s+1")).toEqual({ agent: "s 1" });
  expect(parseInboxSearch("?section=needs-you")).toEqual({ section: "needs-you" });
  // An agent narrowing holds no section: no switch stands beside it.
  expect(parseInboxSearch("?agent=s+1&section=needs-you")).toEqual({ agent: "s 1" });
  expect(parseInboxSearch("?agent=&section=later")).toEqual({});
});

test("inbox assignee filters round-trip as repeated query parameters, after the other filters", () => {
  const route = { assignees: ["ana@example.com", "bo@example.com", "unassigned"] };
  const path = buildInboxPath(route);
  expect(path).toBe("/?assignee=ana%40example.com&assignee=bo%40example.com&assignee=unassigned");
  expect(parseInboxSearch(path.slice(1))).toEqual(route);
  expect(buildInboxPath({ agent: "s1", assignees: ["unassigned"] })).toBe(
    "/?agent=s1&assignee=unassigned"
  );
  expect(parseInboxSearch("?agent=s1&assignee=unassigned")).toEqual({
    agent: "s1",
    assignees: ["unassigned"],
  });
  expect(buildInboxPath({ assignees: ["unassigned"], section: "needs-you" })).toBe(
    "/?section=needs-you&assignee=unassigned"
  );
  expect(parseInboxSearch("?section=needs-you&assignee=unassigned")).toEqual({
    assignees: ["unassigned"],
    section: "needs-you",
  });
});

// `everyone` is how the cleared filter survives a reload: an absent `assignee` is the default,
// the reader's own work, so clearing the filter has to be written down.
test("the cleared inbox filter is `assignee=everyone` and an empty one leaves the field absent", () => {
  expect(buildInboxPath({ assignees: [] })).toBe("/?assignee=everyone");
  expect(parseInboxSearch("?assignee=everyone")).toEqual({ assignees: [] });
  expect(parseInboxSearch("?assignee=everyone&assignee=+")).toEqual({ assignees: [] });
  expect(parseInboxSearch("?assignee=everyone&assignee=ana%40example.com")).toEqual({
    assignees: ["ana@example.com"],
  });
  expect(parseInboxSearch("?assignee=")).toEqual({});
  expect(parseInboxSearch("?view=mine&section=needs-you")).toEqual({ section: "needs-you" });
});

test("inbox assignees fold case and spaces, drop duplicates, keep order, and keep unknowns", () => {
  expect(parseInboxSearch("?assignee=+Ana%40Example.com+&assignee=ana%40example.com")).toEqual({
    assignees: ["ana@example.com"],
  });
  expect(
    parseInboxSearch(
      "?assignee=bo%40example.com&assignee=ana%40example.com&assignee=bo%40example.com"
    )
  ).toEqual({ assignees: ["bo@example.com", "ana@example.com"] });
  // Nothing refuses an email the page does not list: the split is client-side, so it just
  // matches no row.
  expect(parseInboxSearch("?assignee=Nobody%40Example.com")).toEqual({
    assignees: ["nobody@example.com"],
  });
});

// The selected ask rides in the address beside the filters, so a filter change keeps the reader's
// selection and a link carries it.
test("the inbox's selected item round-trips beside the filters and is absent when empty", () => {
  expect(buildInboxPath({ item: "ask-1" })).toBe("/?item=ask-1");
  expect(parseInboxSearch("?item=ask-1")).toEqual({ item: "ask-1" });
  expect(buildInboxPath({ assignees: ["ana@example.com"], item: "ask 2" })).toBe(
    "/?item=ask+2&assignee=ana%40example.com"
  );
  expect(parseInboxSearch("?item=ask+2&assignee=ana%40example.com")).toEqual({
    assignees: ["ana@example.com"],
    item: "ask 2",
  });
  expect(buildInboxPath({ agent: "s1", item: "ask-3" })).toBe("/?agent=s1&item=ask-3");
  expect(parseInboxSearch("?agent=s1&item=+ask-3+")).toEqual({ agent: "s1", item: "ask-3" });
  expect(buildInboxPath({ item: "ask-3", section: "needs-you" })).toBe(
    "/?item=ask-3&section=needs-you"
  );
  expect(parseInboxSearch("?item=+ask-3+&section=needs-you")).toEqual({
    item: "ask-3",
    section: "needs-you",
  });
  expect(parseInboxSearch("?item=")).toEqual({});
  expect(parseInboxSearch("?item=+")).toEqual({});
  expect(buildInboxPath({ assignees: [], item: undefined })).toBe("/?assignee=everyone");
});

// A thread row is named in the address the same way the list names it, so `?item=` carries a
// reply and an agent message as well as an ask.
test("the inbox's selected item carries a thread as well as an ask", () => {
  expect(buildInboxPath({ item: "reply:thread-1" })).toBe("/?item=reply%3Athread-1");
  expect(parseInboxSearch("?item=reply%3Athread-1")).toEqual({ item: "reply:thread-1" });
  expect(parseInboxSearch("?item=agent%3Amessage-1")).toEqual({ item: "agent:message-1" });
});

// Every one of these is absent while it holds its default, so the Inbox as it opens is `/`.
test("the inbox's view and filters round-trip and leave their defaults out of the address", () => {
  const route = {
    filters: { priority: ["P0", "none"], type: ["reply", "agent"] },
    group: "waiting-on",
    sort: "urgency",
    show: "dismissed",
  } as const;
  const path = buildInboxPath(route);
  expect(path).toBe(
    "/?show=dismissed&sort=urgency&group=waiting-on&priority=P0%2Cnone&type=reply%2Cagent"
  );
  expect(parseInboxSearch(path.slice(1))).toEqual(route);

  expect(buildInboxPath({ group: "priority", sort: "newest" })).toBe("/");
  expect(parseInboxSearch("?sort=newest&group=priority")).toEqual({});
  expect(buildInboxPath({ filters: { urgency: [] } })).toBe("/");

  expect(buildInboxPath({ filters: { project: ["CORE", "OPS"], urgency: ["blocking"] } })).toBe(
    "/?urgency=blocking&project=CORE%2COPS"
  );
  expect(parseInboxSearch("?urgency=blocking&project=CORE,OPS")).toEqual({
    filters: { project: ["CORE", "OPS"], urgency: ["blocking"] },
  });
});

// A word this build does not know is dropped rather than refused: an older link still opens.
test("the inbox drops filter values, sorts and groupings it does not know", () => {
  expect(parseInboxSearch("?sort=sideways&group=colour&show=everything")).toEqual({});
  expect(parseInboxSearch("?priority=P9&urgency=mild&type=rumour")).toEqual({});
  expect(parseInboxSearch("?priority=P0,,P0,P1")).toEqual({ filters: { priority: ["P0", "P1"] } });
  // A project key is the workspace's own word, so only an empty one is dropped.
  expect(parseInboxSearch("?project=+,CORE")).toEqual({ filters: { project: ["CORE"] } });
});

// The Inbox's search text rides in the address like its other filters: `usePaneSelection`
// rebuilds the whole query string on every selection change, so anything not in the route is
// lost on the next `j`.
test("the inbox's search text round-trips in the address and is absent when empty", () => {
  expect(buildInboxPath({ q: "two words" })).toBe("/?q=two+words");
  expect(parseInboxSearch("?q=two+words")).toEqual({ q: "two words" });
  expect(parseInboxSearch("?q=")).toEqual({});
  // Nothing trims it: a reader mid-word has a trailing space, and it is part of what they typed.
  expect(parseInboxSearch("?q=two+")).toEqual({ q: "two " });
  const route = {
    agent: "s1",
    assignees: ["ana@example.com"],
    item: "ask-3",
    q: "two words",
  };
  expect(parseInboxSearch(buildInboxPath(route).slice(1))).toEqual(route);
});

test("the inbox's route is read from the address and written back, replacing the entry", () => {
  const hook = renderAddress(useInboxRoute, ["/elsewhere", "/?q=ship&sort=urgency"]);
  try {
    expect(hook.result.current.value[0]).toEqual({ q: "ship", sort: "urgency" });
    const next: InboxRoute = { item: "ask-1", q: "ship", sort: "urgency" };
    act(() => hook.result.current.value[1](next));
    expect(hook.result.current.location.pathname).toBe("/");
    expect(hook.result.current.location.search).toBe("?item=ask-1&q=ship&sort=urgency");
    expect(hook.result.current.value[0]).toEqual(next);
    // A parameter outside the route is dropped, as the whole query string is rebuilt from it.
    act(() => hook.result.current.value[1]({ q: "ship" }));
    expect(hook.result.current.location.search).toBe("?q=ship");
    act(() => hook.result.current.back(-1));
    expect(hook.result.current.location.pathname).toBe("/elsewhere");
  } finally {
    hook.unmount();
  }
});

// The Agents page's Needs you link named `section` beside the agent before it named the agent
// alone (D29): such an address is read as the agent narrowing alone and written back without
// `section`, replacing its entry, so Back leaves for the page before.
test("an address with an agent drops section with a replace", async () => {
  const hook = renderAddress(useInboxRoute, [
    "/agents",
    "/?agent=planner&section=needs-you&q=ship",
  ]);
  try {
    expect(hook.result.current.value[0]).toEqual({ agent: "planner", q: "ship" });
    await waitFor(() => expect(hook.result.current.location.search).toBe("?agent=planner&q=ship"));
    act(() => hook.result.current.back(-1));
    expect(hook.result.current.location.pathname).toBe("/agents");
  } finally {
    hook.unmount();
  }
  // Without an agent the section stands, and the address is left as it is.
  const alone = renderAddress(useInboxRoute, ["/?section=needs-you"]);
  try {
    expect(alone.result.current.value[0]).toEqual({ section: "needs-you" });
    expect(alone.result.current.location.search).toBe("?section=needs-you");
  } finally {
    alone.unmount();
  }
});
