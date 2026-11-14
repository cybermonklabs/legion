import { expect, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from "react-router-dom";

import {
  AddressRedirect,
  type AddressRedirectProps,
  addressWithParam,
  buildInboxPath,
  capSearchQuery,
  choiceParam,
  commaListParam,
  flagParam,
  type InboxRoute,
  numberParam,
  parseInboxSearch,
  repeatedParam,
  tableAddressGrammar,
  useAddressFields,
  useAddressParam,
  useAddressUpdate,
  useCappedAddressParam,
  useInboxRoute,
  useRepeatableAddressParams,
  useTableAddress,
} from "./page-address";

/** A hook on an address, with where the reader is and a way back through the history the hook
 *  has written: whether a write replaces its entry or pushes one is only visible from a history
 *  that has something underneath it. */
function renderAddress<T>(use: () => T, entries: readonly string[] = ["/page"]) {
  return renderHook(() => ({ back: useNavigate(), location: useLocation(), value: use() }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(MemoryRouter, { initialEntries: [...entries] }, children),
  });
}

/** `AddressRedirect` drawn on the bare project path and nowhere else, as `ProjectPage` draws it,
 *  over a history with an entry beneath the page, so a test can see whether the move replaced
 *  the page's entry or pushed one. */
function renderRedirect(redirect: AddressRedirectProps, from: string) {
  return renderHook(() => ({ back: useNavigate(), location: useLocation() }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(
        MemoryRouter,
        { initialEntries: ["/elsewhere", from], initialIndex: 1 },
        createElement(
          Routes,
          null,
          createElement(Route, {
            element: createElement(AddressRedirect, redirect),
            path: "/projects/:key",
          }),
          createElement(Route, { element: null, path: "*" })
        ),
        children
      ),
  });
}

test("a query under the cap is written as it was typed", () => {
  expect(capSearchQuery("")).toBe("");
  expect(capSearchQuery("two words")).toBe("two words");
  expect(capSearchQuery("x".repeat(SEARCH_QUERY_MAX))).toBe("x".repeat(SEARCH_QUERY_MAX));
});

test("a query over the cap is cut to the cap", () => {
  expect(capSearchQuery("x".repeat(SEARCH_QUERY_MAX + 1))).toBe("x".repeat(SEARCH_QUERY_MAX));
});

test("a cut that would split a surrogate pair drops the lone high surrogate", () => {
  // The emoji starts at the last character the cap allows, so the cut lands inside it: half a
  // code point in the address is what this rule exists to prevent.
  const split = `${"x".repeat(SEARCH_QUERY_MAX - 1)}😀`;
  expect(split.length).toBe(SEARCH_QUERY_MAX + 1);
  expect(capSearchQuery(split)).toBe("x".repeat(SEARCH_QUERY_MAX - 1));

  // A pair that ends exactly on the cap is kept whole.
  const whole = `${"x".repeat(SEARCH_QUERY_MAX - 2)}😀`;
  expect(capSearchQuery(whole)).toBe(whole);
});

// The one rule every page's write holds to: a reader trying filters out must be able to press
// Back once to leave the page, not once per pick.
test("the one write replaces the history entry rather than pushing one", () => {
  const hook = renderAddress(useAddressUpdate, ["/elsewhere", "/page?q=one"]);
  try {
    act(() =>
      hook.result.current.value((params) => {
        params.set("q", "two");
        return params;
      })
    );
    act(() =>
      hook.result.current.value((params) => {
        params.set("q", "three");
        return params;
      })
    );
    expect(hook.result.current.location.search).toBe("?q=three");
    // Both writes replaced their entry, so one Back leaves the page altogether.
    act(() => hook.result.current.back(-1));
    expect(hook.result.current.location.pathname).toBe("/elsewhere");
  } finally {
    hook.unmount();
  }
});

// The pathname and everything else the address holds are the page's, not this write's.
test("a write leaves the pathname and the other parameters alone", () => {
  const hook = renderAddress(useAddressUpdate, ["/projects/CORE/issues?label=a&q=ship"]);
  try {
    act(() =>
      hook.result.current.value((params) => {
        params.set("q", "plan");
        return params;
      })
    );
    expect(hook.result.current.location.pathname).toBe("/projects/CORE/issues");
    expect(hook.result.current.location.search).toBe("?label=a&q=plan");
  } finally {
    hook.unmount();
  }
});

test("a capped parameter cuts what it writes and deletes an empty value", () => {
  const hook = renderAddress(() => useCappedAddressParam("q"));
  try {
    act(() => hook.result.current.value[1]("x".repeat(SEARCH_QUERY_MAX + 5)));
    expect(hook.result.current.value[0]).toBe("x".repeat(SEARCH_QUERY_MAX));
    expect(hook.result.current.location.search).toBe(`?q=${"x".repeat(SEARCH_QUERY_MAX)}`);

    act(() => hook.result.current.value[1](""));
    expect(hook.result.current.value[0]).toBe("");
    expect(hook.result.current.location.search).toBe("");
  } finally {
    hook.unmount();
  }
});

// A value longer than the cap that arrived on a link is read as it arrived: the page filters by
// what it holds rather than rewriting the reader's address behind them.
test("a capped parameter reads back what a link carried, uncut", () => {
  const long = "y".repeat(SEARCH_QUERY_MAX + 3);
  const hook = renderAddress(() => useCappedAddressParam("q"), [`/page?q=${long}`]);
  try {
    expect(hook.result.current.value[0]).toBe(long);
  } finally {
    hook.unmount();
  }
});

const MACHINES = commaListParam();

test("a comma list round-trips and leaves the address when it is emptied", () => {
  const hook = renderAddress(
    () => useAddressParam("machine", MACHINES),
    ["/agents?machine=relay-1"]
  );
  try {
    expect(hook.result.current.value[0]).toEqual(["relay-1"]);
    act(() => hook.result.current.value[1](["relay-1", "relay-2"]));
    expect(hook.result.current.location.search).toBe("?machine=relay-1%2Crelay-2");
    expect(hook.result.current.value[0]).toEqual(["relay-1", "relay-2"]);
    act(() => hook.result.current.value[1]([]));
    expect(hook.result.current.location.search).toBe("");
    expect(hook.result.current.value[0]).toEqual([]);
  } finally {
    hook.unmount();
  }
});

const PAGE_FIELDS = {
  label: repeatedParam(),
  "needs-you": flagParam(),
  size: numberParam(25),
  view: choiceParam(["list", "board"] as const, "list"),
};

test("a repeatable, a flag, a choice and a number round-trip under the page's own names", () => {
  const hook = renderAddress(() => useAddressFields(PAGE_FIELDS));
  try {
    act(() => hook.result.current.value[1]({ label: ["a", "b"] }));
    act(() => hook.result.current.value[1]({ "needs-you": true }));
    act(() => hook.result.current.value[1]({ view: "board" }));
    act(() => hook.result.current.value[1]({ size: 50 }));
    expect(hook.result.current.location.search).toBe(
      "?label=a&label=b&needs-you=1&view=board&size=50"
    );
    expect(hook.result.current.value[0]).toEqual({
      label: ["a", "b"],
      "needs-you": true,
      size: 50,
      view: "board",
    });
  } finally {
    hook.unmount();
  }
});

// A control at its default says nothing, so a shared link carries only what was changed.
test("a parameter is absent from the address until the reader sets it", () => {
  const hook = renderAddress(() => useAddressFields(PAGE_FIELDS));
  try {
    expect(hook.result.current.location.search).toBe("");
    expect(hook.result.current.value[0]).toEqual({
      label: [],
      "needs-you": false,
      size: 25,
      view: "list",
    });
    // Setting a field to its own default writes nothing, and clearing one takes it back out.
    act(() => hook.result.current.value[1]({ size: 25, view: "list" }));
    expect(hook.result.current.location.search).toBe("");
    act(() => hook.result.current.value[1]({ "needs-you": true, label: ["a"] }));
    expect(hook.result.current.location.search).toBe("?label=a&needs-you=1");
    act(() => hook.result.current.value[1]({ "needs-you": false, label: [] }));
    expect(hook.result.current.location.search).toBe("");
  } finally {
    hook.unmount();
  }
});

const FACET_KEYS = ["repo", "parentAgent"] as const;

test("every value of a repeatable parameter is read, and an absent one reads as none", () => {
  const hook = renderAddress(
    () => useRepeatableAddressParams(FACET_KEYS),
    ["/delivery?repo=acme%2Fwidgets&repo=acme%2Fapi"]
  );
  try {
    expect(hook.result.current.value).toEqual({
      parentAgent: [],
      repo: ["acme/widgets", "acme/api"],
    });
    // The same address renders the same arrays, so a query keyed off them is not refetched.
    const first = hook.result.current.value;
    hook.rerender();
    expect(hook.result.current.value).toBe(first);
  } finally {
    hook.unmount();
  }
});

test("an address is rewritten with one parameter replaced, keeping the pathname and hash", () => {
  expect(
    addressWithParam(
      { hash: "#b-1", pathname: "/projects/CORE/documents/spec", search: "?ask=12&version=2" },
      "ask",
      "1234abcd"
    )
  ).toBe("/projects/CORE/documents/spec?ask=1234abcd&version=2#b-1");
});

// The bare project path leaving for its Issues: what a link narrowed the page to goes with it.
test("a redirect that keeps the query carries the page's query onto its path, replacing the entry", () => {
  const hook = renderRedirect(
    { keepQuery: true, to: "/projects/CORE/issues" },
    "/projects/CORE?label=a&q=ship#top"
  );
  try {
    expect(hook.result.current.location.pathname).toBe("/projects/CORE/issues");
    expect(hook.result.current.location.search).toBe("?label=a&q=ship");
    // The query alone goes with it, as the bare project path always carried it; the hash does not.
    expect(hook.result.current.location.hash).toBe("");
    // Back leaves for the page the reader came from, not the address that only sent them on.
    act(() => hook.result.current.back(-1));
    expect(hook.result.current.location.pathname).toBe("/elsewhere");
  } finally {
    hook.unmount();
  }
});

// The Architecture tab carries no query, and a document address rewritten with a whole id is the
// whole address already: either lands on exactly what the caller wrote.
test.each([
  ["/projects/CORE/architecture", "/projects/CORE/architecture", "", ""],
  [
    "/projects/CORE/documents/spec?ask=1234abcd#b-1",
    "/projects/CORE/documents/spec",
    "?ask=1234abcd",
    "#b-1",
  ],
])("a redirect to %s lands on exactly that address, dropping the page's own query and hash", (to, pathname, search, hash) => {
  const hook = renderRedirect({ to }, "/projects/CORE?label=a&q=ship#top");
  try {
    expect(hook.result.current.location.pathname).toBe(pathname);
    expect(hook.result.current.location.search).toBe(search);
    expect(hook.result.current.location.hash).toBe(hash);
    act(() => hook.result.current.back(-1));
    expect(hook.result.current.location.pathname).toBe("/elsewhere");
  } finally {
    hook.unmount();
  }
});

test("inbox agent filters round-trip through the query string and ignore unknown sections", () => {
  expect(buildInboxPath()).toBe("/");
  expect(buildInboxPath({ agent: "s 1", section: "needs-you" })).toBe(
    "/?agent=s+1&section=needs-you"
  );
  expect(parseInboxSearch("?agent=s+1&section=needs-you")).toEqual({
    agent: "s 1",
    section: "needs-you",
  });
  expect(parseInboxSearch("?agent=&section=later")).toEqual({});
});

test("inbox assignee filters round-trip as repeated query parameters, after the other filters", () => {
  const route = { assignees: ["ana@example.com", "bo@example.com", "unassigned"] };
  const path = buildInboxPath(route);
  expect(path).toBe("/?assignee=ana%40example.com&assignee=bo%40example.com&assignee=unassigned");
  expect(parseInboxSearch(path.slice(1))).toEqual(route);
  expect(buildInboxPath({ agent: "s1", assignees: ["unassigned"], section: "needs-you" })).toBe(
    "/?agent=s1&section=needs-you&assignee=unassigned"
  );
  expect(parseInboxSearch("?agent=s1&section=needs-you&assignee=unassigned")).toEqual({
    agent: "s1",
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
  expect(buildInboxPath({ agent: "s1", item: "ask-3", section: "needs-you" })).toBe(
    "/?agent=s1&item=ask-3&section=needs-you"
  );
  expect(parseInboxSearch("?agent=s1&item=+ask-3+&section=needs-you")).toEqual({
    agent: "s1",
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

const PLAIN_TABLE = tableAddressGrammar();
const LEFT_TABLE = tableAddressGrammar({ prefix: "left", pageSize: 50 });
const RIGHT_TABLE = tableAddressGrammar({ prefix: "right", pageSize: 50 });

test("a table's view round-trips under the names a single-table page has always written", () => {
  const hook = renderAddress(() => useTableAddress(PLAIN_TABLE));
  try {
    act(() => hook.result.current.value.setSorting([{ id: "title", desc: true }]));
    act(() => hook.result.current.value.setGrouping(["epic"]));
    act(() => hook.result.current.value.setFilters([{ id: "status", value: ["todo", "done"] }]));
    act(() => hook.result.current.value.setSearch("ship"));
    act(() => hook.result.current.value.setPagination({ pageIndex: 2, pageSize: 100 }));
    expect(hook.result.current.location.search).toBe(
      "?sort=-title&group=epic&q=ship&f.status=todo&f.status=done&page=3&size=100"
    );
    expect(hook.result.current.value.view).toEqual({
      filters: [{ id: "status", value: ["todo", "done"] }],
      grouping: ["epic"],
      page: 2,
      pageSize: 100,
      search: "ship",
      sorting: [{ id: "title", desc: true }],
    });
  } finally {
    hook.unmount();
  }
});

// Every change but the page itself returns to the first page: a filter that leaves fewer rows
// than the page the reader was on would otherwise show nothing.
test("a narrowing change takes the table back to its first page", () => {
  const hook = renderAddress(() => useTableAddress(PLAIN_TABLE), ["/page?page=4"]);
  try {
    expect(hook.result.current.value.view.page).toBe(3);
    act(() => hook.result.current.value.setSearch("ship"));
    expect(hook.result.current.value.view.page).toBe(0);
    expect(hook.result.current.location.search).toBe("?q=ship");
  } finally {
    hook.unmount();
  }
});

// Two tables on one page: each keeps its own sorting, grouping, search, paging and filters, and
// neither reads the other's.
test("two prefixed table addresses on one page do not read each other", () => {
  const hook = renderAddress(() => ({
    left: useTableAddress(LEFT_TABLE),
    right: useTableAddress(RIGHT_TABLE),
  }));
  try {
    act(() => hook.result.current.value.left.setSorting([{ id: "title", desc: false }]));
    act(() => hook.result.current.value.left.setSearch("ship"));
    act(() => hook.result.current.value.right.setGrouping(["repo"]));
    act(() => hook.result.current.value.right.setFilters([{ id: "status", value: ["todo"] }]));
    expect(hook.result.current.location.search).toBe(
      "?left.sort=title&left.q=ship&right.group=repo&right.f.status=todo"
    );

    expect(hook.result.current.value.left.view.sorting).toEqual([{ id: "title", desc: false }]);
    expect(hook.result.current.value.left.view.search).toBe("ship");
    expect(hook.result.current.value.left.view.grouping).toEqual([]);
    expect(hook.result.current.value.left.view.filters).toEqual([]);

    expect(hook.result.current.value.right.view.grouping).toEqual(["repo"]);
    expect(hook.result.current.value.right.view.filters).toEqual([
      { id: "status", value: ["todo"] },
    ]);
    expect(hook.result.current.value.right.view.search).toBe("");
    expect(hook.result.current.value.right.view.sorting).toEqual([]);

    // Each page size is its own, and the one the table opens with stays out of the address.
    expect(hook.result.current.value.left.view.pageSize).toBe(50);
    act(() => hook.result.current.value.right.setPagination({ pageIndex: 1, pageSize: 100 }));
    expect(hook.result.current.value.left.view.pageSize).toBe(50);
    expect(hook.result.current.value.left.view.page).toBe(0);
    expect(hook.result.current.value.right.view.pageSize).toBe(100);
    expect(hook.result.current.value.right.view.page).toBe(1);
  } finally {
    hook.unmount();
  }
});
