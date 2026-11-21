import { expect, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from "react-router-dom";

import { renderAddress } from "../__tests__/render-address";
import {
  AddressRedirect,
  type AddressRedirectProps,
  addressPath,
  addressWithParam,
  capSearchQuery,
  choiceParam,
  commaListParam,
  copyAddress,
  flagParam,
  numberParam,
  repeatedParam,
  tableAddressGrammar,
  useAddressFields,
  useAddressParam,
  useAddressUpdate,
  useCappedAddressParam,
  useRepeatableAddressParams,
  useTableAddress,
} from "./page-address";

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

// The reader's own edit of the box cuts the search; an unrelated write leaves the address's own
// value whole. The grammar rewrites every field, so a cut on the way out would shorten a longer
// `?q=` a link carried as soon as the reader sorted or paged.
test("a sort leaves a longer ?q= a link carried whole, and the box's own edit still cuts it", () => {
  const long = "z".repeat(SEARCH_QUERY_MAX + 7);
  const hook = renderAddress(() => useTableAddress(PLAIN_TABLE), [`/page?q=${long}`]);
  try {
    expect(hook.result.current.value.view.search).toBe(long);
    act(() => hook.result.current.value.setSorting([{ id: "title", desc: true }]));
    expect(hook.result.current.value.view.search).toBe(long);
    act(() => hook.result.current.value.setSearch(long));
    expect(hook.result.current.value.view.search).toBe("z".repeat(SEARCH_QUERY_MAX));
  } finally {
    hook.unmount();
  }
});

// The one join of a path to a query built here: a link or a navigation takes this rather than
// writing the `?` itself, so an empty query leaves a bare path and the hash stays last.
test("an address built from a path and a query carries its `?` only while the query has one", () => {
  expect(addressPath("/projects/CORE/issues", copyAddress())).toBe("/projects/CORE/issues");
  expect(addressPath("/projects/CORE/issues", copyAddress("only=CORE-1&onlyLabel=Ship"))).toBe(
    "/projects/CORE/issues?only=CORE-1&onlyLabel=Ship"
  );
  expect(addressPath("/page", copyAddress("q=x"), "#anchor")).toBe("/page?q=x#anchor");
  expect(addressPath("/page", copyAddress(), "#anchor")).toBe("/page#anchor");
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
