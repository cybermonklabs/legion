import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { installCountingStorage } from "../../__tests__/counting-storage";
import { renderAddress } from "../../__tests__/render-address";
import { api } from "../../api/client";
import { inWindow, rangeValue, readWindow } from "../../lib/closed-window";
import { copyAddress, type TableAddressGrammar, useTableAddress } from "../../lib/page-address";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { openIssueStatuses } from "./board-model";
import {
  readSelection,
  readUrl,
  selectIssues,
  tableStorageKey,
  type View,
  writeUrl,
} from "./issues-table-model";
import {
  type ActiveView,
  BUILT_IN_VIEWS,
  builtInAddress,
  checkViewName,
  clearNarrowings,
  compressView,
  currentAddress,
  expandView,
  inExcludedTree,
  isSavedViewChanged,
  isViewChanged,
  legacyOpenLayout,
  matchSavedView,
  NO_SAVED_VIEWS,
  narrowingsDiffer,
  readCols,
  readSavedViews,
  type SavedView,
  savedViewQuery,
  shownColumns,
  useExcludedTrees,
  useSavedViews,
  useViewLayout,
  VIEW_NAME_MAX,
  viewColumns,
  viewFor,
  viewLayoutKey,
  viewsGrammar,
  withoutSavedView,
  withSavedView,
  writeSavedViews,
} from "./issues-views";

const at = (query: string) => copyAddress(query);
const entries = (params: URLSearchParams) => [...params];
/** An address's entries in one order, for two addresses that say the same in another order. */
const sorted = (params: URLSearchParams) => [...params].sort();

const OPEN = viewFor("open");
const DONE = viewFor("done");
const NOW = Date.parse("2026-11-17T12:00:00Z");

/** The project table's own spelling of its whole view, with the views between it and the
 *  address, as the Issues tab reads it. */
const PROJECT_GRAMMAR: TableAddressGrammar<View> = viewsGrammar({
  read: (params) => readUrl(params),
  write: (params, view) => writeUrl(params, view),
});

/** A hook that reads the signed-in login, on `entry`, with `alice` signed in. */
function renderSignedIn<T>(use: () => T, entry: string) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const hook = renderHook(() => ({ location: useLocation(), value: use() }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(
        QueryClientProvider,
        { client: queryClient },
        createElement(MemoryRouter, { initialEntries: [entry] }, children)
      ),
  });
  return {
    ...hook,
    signedIn: () => waitFor(() => expect(queryClient.getQueryData(["whoami"])).toBeDefined()),
    done: () => {
      hook.unmount();
      whoAmI.mockRestore();
    },
  };
}

const OPEN_VIEW: ActiveView = { kind: "built-in", id: "open" };
const DONE_VIEW: ActiveView = { kind: "built-in", id: "done" };
const SAVED_VIEW: ActiveView = { kind: "saved", name: "Mine" };
const CORE_TABLE = tableStorageKey("CORE");
const CORE_DONE_TABLE = `${CORE_TABLE}:view=done`;
const ISSUE_VIEW_KEY = userPreferenceStorageKey("alice", "project.issue-view");

// --- The views and what opens them -------------------------------------------------------------

test("Open and Done are the two built-in views, each with its statuses, sort and columns", () => {
  expect(BUILT_IN_VIEWS.map((view) => [view.id, view.name, view.bar])).toEqual([
    ["open", "Open", true],
    ["done", "Done", false],
  ]);
  expect(OPEN.statuses).toEqual(openIssueStatuses);
  expect(OPEN.statuses).toHaveLength(8);
  expect(OPEN.sort).toBe("queue");
  expect(DONE.statuses).toEqual(["done"]);
  expect(DONE.sort).toBe("-closed");
  expect(viewColumns(OPEN, true)).toEqual([
    "select",
    "priority",
    "key",
    "title",
    "queue",
    "status",
    "author",
    "asks",
    "legion_phase",
    "assignee",
    "labels",
    "claim",
    "created",
  ]);
  expect(viewColumns(DONE, true)).toEqual([
    "select",
    "priority",
    "key",
    "title",
    "closed",
    "phase_hours",
    "work_hours",
    "assignee",
    "author",
    "labels",
  ]);
  // Each view written out: `view=open` with the eight open statuses sorted by Queue, and
  // `view=done` with the done status sorted by the newest close.
  expect(entries(expandView(at("")))).toEqual([
    ["view", "open"],
    ...openIssueStatuses.map((status): [string, string] => ["status", status]),
    ["sort", "queue"],
  ]);
  expect(entries(expandView(at("view=done")))).toEqual([
    ["view", "done"],
    ["status", "done"],
    ["sort", "-closed"],
  ]);
  expect(builtInAddress("open").toString()).toBe("");
  expect(builtInAddress("done").toString()).toBe("view=done");
});

test("in a project with no Legion daemon the defaults leave out Queue and every board-fed column", () => {
  expect(viewColumns(OPEN, false)).toEqual([
    "select",
    "priority",
    "key",
    "title",
    "status",
    "author",
    "asks",
    "assignee",
    "labels",
    "claim",
    "created",
  ]);
  expect(viewColumns(DONE, false)).toEqual([
    "select",
    "priority",
    "key",
    "title",
    "closed",
    "assignee",
    "author",
    "labels",
  ]);
});

test("no view, or a view the project does not have, opens Open unchanged", () => {
  expect(viewFor(null)).toBe(OPEN);
  expect(viewFor("in-flight")).toBe(OPEN);
  expect(expandView(at("view=in-flight")).toString()).toBe(expandView(at("")).toString());
  expect(isViewChanged(at("view=in-flight"))).toBe(false);
  // The next write of the view spells Open as it is spelled now: with no `view`.
  expect(compressView(expandView(at("view=in-flight"))).toString()).toBe("");
});

test("view=queue and view=waiting open Open with f.queue=Queued, which marks it changed and Clear filters takes off", () => {
  for (const id of ["queue", "waiting"]) {
    const old = at(`view=${id}&q=rank`);
    expect(currentAddress(old)?.toString()).toBe("q=rank&f.queue=Queued");
    const spelled = expandView(old);
    expect(spelled.get("view")).toBe("open");
    expect(spelled.getAll("f.queue")).toEqual(["Queued"]);
    expect(spelled.getAll("sort")).toEqual(["queue"]);
    expect(isViewChanged(old)).toBe(true);
    const opened = currentAddress(at(`view=${id}`)) ?? at("");
    expect(opened.toString()).toBe("f.queue=Queued");
    expect(narrowingsDiffer(opened, builtInAddress("open"))).toBe(true);
    expect(clearNarrowings(opened, builtInAddress("open")).toString()).toBe("");
  }
  // A Queue filter the link carries already stays as it is.
  expect(currentAddress(at("view=queue&f.queue=Not+queued"))?.getAll("f.queue")).toEqual([
    "Not queued",
  ]);
  // An address in today's spelling is left alone.
  expect(currentAddress(at("view=done"))).toBeUndefined();
  expect(currentAddress(at("f.queue=Queued&group=status"))).toBeUndefined();
});

test("f.stage, f.epic and group=epic leave the address, and nothing else does", () => {
  const old = at("f.epic=CORE-1&f.stage=build&group=epic&q=crash&label=ui");
  expect(currentAddress(old)?.toString()).toBe("q=crash&label=ui");
  const spelled = expandView(old);
  expect(spelled.has("f.epic")).toBe(false);
  expect(spelled.has("f.stage")).toBe(false);
  expect(spelled.has("group")).toBe(false);
  expect(isViewChanged(at("f.epic=CORE-1&f.stage=build&group=epic"))).toBe(false);
  expect(expandView(at("group=assignee")).getAll("group")).toEqual(["assignee"]);
});

// --- Replace or narrow ---------------------------------------------------------------------------

test("an explicit status or sort replaces the view's value, and every other parameter narrows the view", () => {
  const statuses = expandView(at("view=done&status=todo&status=backlog"));
  expect(statuses.getAll("status")).toEqual(["todo", "backlog"]);
  expect(statuses.getAll("sort")).toEqual(["-closed"]);

  const resorted = expandView(at("view=done&sort=-work_hours"));
  expect(resorted.getAll("sort")).toEqual(["-work_hours"]);
  expect(resorted.getAll("status")).toEqual(["done"]);
  // An empty sort takes the view's sort away.
  expect(expandView(at("view=done&sort=")).getAll("sort")).toEqual([""]);

  const narrowed = expandView(at("view=done&f.closed=7d&label=ui&q=crash&needs-you=1"));
  expect(narrowed.getAll("status")).toEqual(["done"]);
  expect(narrowed.getAll("sort")).toEqual(["-closed"]);
  expect(narrowed.get("f.closed")).toBe("7d");
  expect(narrowed.get("label")).toBe("ui");
  expect(narrowed.get("q")).toBe("crash");
  expect(narrowed.get("needs-you")).toBe("1");

  // `nofilter=1`, and a `status=` with no value, lift the view's statuses.
  for (const lifted of [
    expandView(at("view=done&nofilter=1")),
    expandView(at("view=done&status=")),
  ]) {
    expect(lifted.has("status")).toBe(false);
    expect(lifted.get("nofilter")).toBe("1");
  }

  // The project table reads the written-out view as its own: Done's status, a label beside
  // Open's statuses, and no status filter at all once it is lifted.
  expect(readUrl(expandView(at("view=done&status=todo"))).filters).toContainEqual({
    id: "status",
    value: ["todo"],
  });
  const open = readUrl(expandView(at("label=ui"))).filters;
  expect(open).toContainEqual({ id: "status", value: [...openIssueStatuses] });
  expect(open).toContainEqual({ id: "labels", value: ["ui"] });
  const all = readUrl(expandView(at("view=done&nofilter=1"))).filters;
  expect(all.some((filter) => filter.id === "status")).toBe(false);
});

test("a view written back leaves out what the view says already and writes what differs", () => {
  expect(compressView(at("view=done&status=done&sort=-closed&page=2")).toString()).toBe(
    "view=done&page=2"
  );
  expect(compressView(at("view=done&nofilter=1&sort=-closed")).toString()).toBe(
    "view=done&nofilter=1"
  );
  expect(compressView(at("view=open&status=todo&sort=")).toString()).toBe("status=todo&sort=");
  // The table's spelling leaves out its defaults, Open's statuses and sort, so on Done a full
  // address that names neither has Open's, which differ from Done's and are written.
  const openOnDone = compressView(at("view=done"));
  expect(openOnDone.getAll("status")).toEqual([...openIssueStatuses]);
  expect(openOnDone.getAll("sort")).toEqual(["queue"]);
  // Open written back is the address with no `view`.
  expect(compressView(expandView(at(""))).toString()).toBe("");
});

test("the project table's grammar reads and writes the address through the views", () => {
  const open = renderAddress(() => useTableAddress(PROJECT_GRAMMAR), ["/projects/CORE/issues"]);
  try {
    expect(open.result.current.value.view.filters).toEqual([
      { id: "status", value: [...openIssueStatuses] },
    ]);
    expect(open.result.current.value.view.sorting).toEqual([{ id: "queue", desc: false }]);
    act(() => open.result.current.value.setSearch("crash"));
    expect(open.result.current.location.search).toBe("?q=crash");
  } finally {
    open.unmount();
  }

  const done = renderAddress(
    () => useTableAddress(PROJECT_GRAMMAR),
    ["/projects/CORE/issues?view=done&sort=-created"]
  );
  try {
    expect(done.result.current.value.view.filters).toEqual([{ id: "status", value: ["done"] }]);
    expect(done.result.current.value.view.sorting).toEqual([{ id: "created", desc: true }]);
    act(() => done.result.current.value.setPagination({ pageIndex: 1, pageSize: 50 }));
    expect(sorted(copyAddress(done.result.current.location.search))).toEqual([
      ["page", "2"],
      ["sort", "-created"],
      ["view", "done"],
    ]);
  } finally {
    done.unmount();
  }
});

// --- Columns and layout for one load -------------------------------------------------------------

test("cols= names the whole set of columns shown for the load, Select always among them", () => {
  const link = at("view=done&cols=key,title,work_hours");
  expect(readCols(link)).toEqual(["key", "title", "work_hours"]);
  expect(shownColumns(link, DONE, true)).toEqual(["select", "key", "title", "work_hours"]);
  // A board-fed column does not show in a project with no Legion daemon.
  expect(shownColumns(link, DONE, false)).toEqual(["select", "key", "title"]);
  expect(shownColumns(at("view=done"), DONE, true)).toEqual(viewColumns(DONE, true));
  expect(readCols(at("cols="))).toBeUndefined();
  expect(readCols(at("cols=select,key,key"))).toEqual(["key"]);
});

test("cols= and layout=board hold for the load and write nothing to storage", async () => {
  const storage = installCountingStorage({
    [CORE_TABLE]: JSON.stringify({ layout: "table" }),
    [ISSUE_VIEW_KEY]: "board",
  });
  try {
    const page = renderSignedIn(
      () => useViewLayout(CORE_TABLE, OPEN_VIEW),
      "/projects/CORE/issues?cols=key,title&layout=board"
    );
    try {
      await page.signedIn();
      expect(page.result.current.value[0]).toBe("board");
      act(() => page.result.current.value[1]("table"));
      const address = copyAddress(page.result.current.location.search);
      expect(address.get("layout")).toBe("table");
      expect(address.get("cols")).toBe("key,title");
      expect(page.result.current.value[0]).toBe("table");
    } finally {
      page.done();
    }
    // A load without them draws what the view keeps.
    const reload = renderSignedIn(
      () => useViewLayout(CORE_TABLE, OPEN_VIEW),
      "/projects/CORE/issues"
    );
    try {
      await reload.signedIn();
      expect(reload.result.current.value[0]).toBe("table");
    } finally {
      reload.done();
    }
    expect(storage.writes).toEqual([]);
  } finally {
    storage.restore();
  }
});

test("Open keeps its layout under today's key and Done under its own, each kept when changed", async () => {
  expect(viewLayoutKey(CORE_TABLE, "open")).toBe("dispatch.project-table.CORE");
  expect(viewLayoutKey(CORE_TABLE, "done")).toBe("dispatch.project-table.CORE:view=done");
  const storage = installCountingStorage({
    [CORE_DONE_TABLE]: JSON.stringify({ sizing: { title: 300 } }),
  });
  const done = renderSignedIn(
    () => useViewLayout(CORE_TABLE, DONE_VIEW),
    "/projects/CORE/issues?view=done"
  );
  try {
    await done.signedIn();
    expect(done.result.current.value[0]).toBe("table");
    act(() => done.result.current.value[1]("board"));
    expect(done.result.current.value[0]).toBe("board");
    // Done's layout goes beside the widths kept under its key, and Open's key is not written.
    expect(storage.writes).toEqual([
      [CORE_DONE_TABLE, JSON.stringify({ sizing: { title: 300 }, layout: "board" })],
    ]);
    expect(done.result.current.location.search).toBe("?view=done");
  } finally {
    done.done();
    storage.restore();
  }
});

test("the project.issue-view choice becomes Open's layout while Open keeps none", async () => {
  expect(legacyOpenLayout("board")).toBe("board");
  expect(legacyOpenLayout("list")).toBe("table");
  expect(legacyOpenLayout(null)).toBe("table");

  const storage = installCountingStorage({
    [ISSUE_VIEW_KEY]: "board",
    [CORE_TABLE]: JSON.stringify({ layout: "table" }),
  });
  const page = renderSignedIn(
    () => ({
      // Open in a project that keeps no layout of it, and Done there.
      acme: useViewLayout(tableStorageKey("ACME"), OPEN_VIEW)[0],
      acmeDone: useViewLayout(tableStorageKey("ACME"), DONE_VIEW)[0],
      // Open in a project that keeps one: the kept layout wins over the choice.
      core: useViewLayout(CORE_TABLE, OPEN_VIEW)[0],
    }),
    "/projects/ACME/issues"
  );
  try {
    await waitFor(() => expect(page.result.current.value.acme).toBe("board"));
    expect(page.result.current.value.acmeDone).toBe("table");
    expect(page.result.current.value.core).toBe("table");
    expect(storage.writes).toEqual([]);
  } finally {
    page.done();
    storage.restore();
  }
});

test("a saved view's layout is its query's, so a change of it writes the address", async () => {
  const storage = installCountingStorage();
  const page = renderSignedIn(
    () => useViewLayout(CORE_TABLE, SAVED_VIEW),
    "/projects/CORE/issues?f.author=%40me&layout=table"
  );
  try {
    await page.signedIn();
    expect(page.result.current.value[0]).toBe("table");
    act(() => page.result.current.value[1]("board"));
    expect(copyAddress(page.result.current.location.search).get("layout")).toBe("board");
    expect(page.result.current.value[0]).toBe("board");
    expect(storage.writes).toEqual([]);
  } finally {
    page.done();
    storage.restore();
  }
});

// --- Changed, and Clear filters ------------------------------------------------------------------

test("a built-in view is changed by any parameter but the page, its size, the columns and the layout", () => {
  expect(isViewChanged(at(""))).toBe(false);
  expect(isViewChanged(at("view=done"))).toBe(false);
  expect(isViewChanged(at("view=done&page=3&size=100&cols=key,title&layout=board"))).toBe(false);
  // A link that spells the view's own values says nothing the view does not.
  expect(isViewChanged(at("view=done&status=done&sort=-closed"))).toBe(false);
  for (const changed of [
    "view=done&sort=-work_hours",
    "view=done&status=todo",
    "view=done&nofilter=1",
    "q=crash",
    "f.asks=any",
    "label=ui",
    "needs-you=1",
    "unread=1",
    "unclaimed=1",
    "group=assignee",
    "exclude_tree=CORE-1",
    "only=CORE-1",
  ]) {
    expect({ changed, is: isViewChanged(at(changed)) }).toEqual({ changed, is: true });
  }
});

test("a saved view is changed also when its columns or layout differ from its query", () => {
  const saved: SavedView = {
    name: "Mine",
    query: "f.author=%40me&cols=key%2Ctitle&layout=board",
  };
  expect(isSavedViewChanged(at(saved.query), saved)).toBe(false);
  expect(isSavedViewChanged(at(`${saved.query}&page=2&size=100`), saved)).toBe(false);
  // The columns are a set: another order names the same ones.
  expect(isSavedViewChanged(at("f.author=%40me&cols=title,key&layout=board"), saved)).toBe(false);
  expect(isSavedViewChanged(at("f.author=%40me&cols=key&layout=board"), saved)).toBe(true);
  expect(isSavedViewChanged(at("f.author=%40me&cols=key,title&layout=table"), saved)).toBe(true);
  expect(isSavedViewChanged(at("f.author=%40me&cols=key,title"), saved)).toBe(true);
  expect(isSavedViewChanged(at(`${saved.query}&q=crash`), saved)).toBe(true);
  expect(matchSavedView([saved], at(`${saved.query}&page=2`))).toBe(saved);
  expect(matchSavedView([saved], at("f.author=%40me"))).toBeUndefined();
});

test("Clear filters takes the filters, search, flags and chips back to the view's own and keeps the sort and grouping", () => {
  const reader = at(
    "view=done&f.closed=7d&q=crash&label=ui&needs-you=1&unread=1&unclaimed=1&only=CORE-1&onlyLabel=Picked&nofilter=1&exclude_tree=CORE-9&status=todo&sort=-work_hours&group=assignee&cols=key,title&layout=board&size=100&page=3"
  );
  expect(narrowingsDiffer(reader, builtInAddress("done"))).toBe(true);
  const cleared = clearNarrowings(reader, builtInAddress("done"));
  expect(entries(cleared)).toEqual([
    ["view", "done"],
    ["sort", "-work_hours"],
    ["group", "assignee"],
    ["cols", "key,title"],
    ["layout", "board"],
    ["size", "100"],
  ]);
  expect(narrowingsDiffer(cleared, builtInAddress("done"))).toBe(false);
  // A sort, a grouping or a page is no narrowing: Clear filters does not show for them.
  expect(
    narrowingsDiffer(at("view=done&sort=-work_hours&group=assignee&page=2"), builtInAddress("done"))
  ).toBe(false);

  // A saved view's own narrowings are its query's.
  const own = at("view=done&f.closed=28d&label=ui&sort=-closed&cols=key&layout=table");
  const changed = at("view=done&f.closed=7d&q=crash&sort=-work_hours&cols=key&layout=table");
  expect(narrowingsDiffer(changed, own)).toBe(true);
  expect(entries(clearNarrowings(changed, own))).toEqual([
    ["view", "done"],
    ["f.closed", "28d"],
    ["label", "ui"],
    ["sort", "-work_hours"],
    ["cols", "key"],
    ["layout", "table"],
  ]);
  expect(narrowingsDiffer(at("view=done&f.closed=28d&label=ui&sort=-work_hours"), own)).toBe(false);
});

// --- The Evals links -----------------------------------------------------------------------------

test("an Evals link to one UTC day names it as <day>..<next day>", () => {
  const day = rangeValue("2026-11-03", "2026-11-03");
  expect(day).toBe("2026-11-03..2026-11-04");
  const window = readWindow(day, NOW);
  expect(inWindow("2026-11-03T23:59:59Z", window)).toBe(true);
  expect(inWindow("2026-11-04T00:00:00Z", window)).toBe(false);
  const link = expandView(at("view=done&f.closed=2026-11-03..2026-11-04&sort=-work_hours"));
  expect(link.getAll("status")).toEqual(["done"]);
  expect(link.getAll("sort")).toEqual(["-work_hours"]);
  expect(link.get("f.closed")).toBe("2026-11-03..2026-11-04");
});

test("an Evals link's parameters narrow Done and come back as they were written", () => {
  const link = at(
    "view=done&f.closed=2026-11-03..2026-11-04&f.legion=root&f.legion=child&f.phase_hours=implementing&exclude_tree=CORE-1&exclude_tree=CORE-7&sort=-work_hours&cols=key,title,work_hours"
  );
  const full = expandView(link);
  expect(full.getAll("status")).toEqual(["done"]);
  expect(full.getAll("sort")).toEqual(["-work_hours"]);
  expect(full.getAll("f.legion")).toEqual(["root", "child"]);
  expect(full.getAll("f.phase_hours")).toEqual(["implementing"]);
  expect(full.getAll("exclude_tree")).toEqual(["CORE-1", "CORE-7"]);
  expect(readCols(full)).toEqual(["key", "title", "work_hours"]);
  expect(sorted(compressView(full))).toEqual(sorted(link));
  expect(isViewChanged(link)).toBe(true);
  for (const id of ["closed", "work_hours", "review_rounds", "waited", "time_in_phase", "activity"])
    expect(expandView(at(`view=done&sort=${id}`)).getAll("sort")).toEqual([id]);
});

test("exclude_tree leaves out the rows of each tree it names, and its chip removes one", () => {
  expect(inExcludedTree("CORE-1", undefined, ["CORE-1"])).toBe(true);
  expect(inExcludedTree("CORE-3", "CORE-1", ["CORE-1", "CORE-7"])).toBe(true);
  expect(inExcludedTree("CORE-4", "CORE-2", ["CORE-1", "CORE-7"])).toBe(false);
  expect(inExcludedTree("CORE-5", undefined, [])).toBe(false);

  const page = renderAddress(useExcludedTrees, [
    "/projects/CORE/issues?view=done&exclude_tree=CORE-1&exclude_tree=CORE-7",
  ]);
  try {
    expect(page.result.current.value[0]).toEqual(["CORE-1", "CORE-7"]);
    act(() => page.result.current.value[1]("CORE-1"));
    expect(page.result.current.location.search).toBe("?view=done&exclude_tree=CORE-7");
    expect(page.result.current.value[0]).toEqual(["CORE-7"]);
  } finally {
    page.unmount();
  }
});

test("only= keeps its contract: the keys, onlyLabel, and nofilter=1 lifting the Status filter", () => {
  const link = at("view=done&only=CORE-1,CORE-9&onlyLabel=Open+P0+with+no+claim&nofilter=1");
  const full = expandView(link);
  expect(full.has("status")).toBe(false);
  expect(full.get("nofilter")).toBe("1");
  expect(readSelection(full)).toEqual({
    keys: new Set(["CORE-1", "CORE-9"]),
    label: "Open P0 with no claim",
  });
  expect(readUrl(full).filters.some((filter) => filter.id === "status")).toBe(false);
  expect(sorted(compressView(full))).toEqual(sorted(link));
  // The project's own narrowing to a set of keys, written on a view, stays that view's.
  const picked = compressView(expandView(selectIssues(at("view=done"), ["CORE-2"], "Picked")));
  expect(picked.get("view")).toBe("done");
  expect(picked.get("only")).toBe("CORE-2");
  expect(picked.get("onlyLabel")).toBe("Picked");
  expect(picked.get("nofilter")).toBe("1");
  // It is a chip, so Clear filters takes it off.
  expect(clearNarrowings(link, builtInAddress("done")).toString()).toBe("view=done");
});

// --- Saved views ---------------------------------------------------------------------------------

test("project.saved-views keeps each project's views, and an entry that does not parse is dropped", () => {
  expect(readSavedViews(null)).toEqual(NO_SAVED_VIEWS);
  expect(readSavedViews("not json")).toEqual(NO_SAVED_VIEWS);
  expect(readSavedViews('{"version":2,"projects":{}}')).toEqual(NO_SAVED_VIEWS);
  expect(readSavedViews('{"version":1,"projects":[]}')).toEqual(NO_SAVED_VIEWS);
  expect(readSavedViews('{"version":1}')).toEqual(NO_SAVED_VIEWS);
  const mine = { name: "Mine", query: "f.author=%40me" };
  const queue = { name: "Legion queue", query: "f.queue=Queued&cols=key%2Ctitle&layout=board" };
  const stored = JSON.stringify({
    version: 1,
    projects: {
      CORE: [
        mine,
        { name: "Way too long a name", query: "" },
        { name: "Open", query: "" },
        { name: "mine", query: "q=again" },
        { name: "", query: "" },
        { name: " Padded", query: "" },
        { name: "No query" },
        { name: 7, query: "" },
        "Mine",
        null,
        queue,
      ],
      ACME: "not a list",
    },
  });
  expect(readSavedViews(stored)).toEqual({ version: 1, projects: { CORE: [mine, queue] } });
  expect(readSavedViews(writeSavedViews(readSavedViews(stored)))).toEqual(readSavedViews(stored));
});

test("a saved view's name is at most 14 characters and no built-in view's, and a name in use asks to replace it", () => {
  const saved: SavedView[] = [{ name: "Mine", query: "" }];
  expect(VIEW_NAME_MAX).toBe(14);
  expect(checkViewName("", saved)).toEqual({ kind: "problem", problem: "Type a name." });
  expect(checkViewName("   ", saved)).toEqual({ kind: "problem", problem: "Type a name." });
  expect(checkViewName("Fifteen chars!!", saved)).toEqual({
    kind: "problem",
    problem: "Use 14 characters or fewer.",
  });
  expect(checkViewName("Fourteen chars", saved)).toEqual({ kind: "new", name: "Fourteen chars" });
  expect(checkViewName("open", saved)).toEqual({
    kind: "problem",
    problem: "Open is a built-in view. Type a different name.",
  });
  expect(checkViewName(" DONE ", saved)).toEqual({
    kind: "problem",
    problem: "Done is a built-in view. Type a different name.",
  });
  expect(checkViewName(" mine ", saved)).toEqual({
    kind: "replace",
    name: "mine",
    replaces: "Mine",
  });
  expect(checkViewName("Legion queue", saved)).toEqual({ kind: "new", name: "Legion queue" });

  // A replace takes the saved view's place; a new name goes after the others.
  const two = withSavedView(
    withSavedView(NO_SAVED_VIEWS, "CORE", { name: "Mine", query: "a=1" }),
    "CORE",
    { name: "Triage", query: "status=triage" }
  );
  const replaced = withSavedView(two, "CORE", { name: "mine", query: "a=2" });
  expect(replaced.projects.CORE).toEqual([
    { name: "mine", query: "a=2" },
    { name: "Triage", query: "status=triage" },
  ]);
  expect(withoutSavedView(replaced, "CORE", "MINE").projects.CORE).toEqual([
    { name: "Triage", query: "status=triage" },
  ]);
});

test("a saved view's query holds its parameters, cols= and layout=, and no page", () => {
  expect(
    savedViewQuery(at("view=done&f.closed=7d&page=3&size=100"), {
      cols: ["select", "key", "title", "closed"],
      layout: "table",
    })
  ).toBe("view=done&f.closed=7d&size=100&cols=key%2Ctitle%2Cclosed&layout=table");
  // A `cols=` the address carries is the one kept, and an older link is saved in today's words.
  expect(
    savedViewQuery(at("view=queue&cols=key"), { cols: ["select", "key", "title"], layout: "board" })
  ).toBe("cols=key&f.queue=Queued&layout=board");
});

test("saved views are kept per project for the signed-in login, saved, replaced and deleted", async () => {
  const key = userPreferenceStorageKey("alice", "project.saved-views");
  const acme = { name: "Theirs", query: "q=acme" };
  const triage = { name: "Triage", query: "status=triage" };
  const storage = installCountingStorage({
    [key]: JSON.stringify({ version: 1, projects: { ACME: [acme], CORE: [triage] } }),
  });
  const page = renderSignedIn(() => useSavedViews("CORE"), "/projects/CORE/issues");
  const kept = () => JSON.parse(storage.writes.at(-1)?.[1] ?? "null");
  try {
    // The login's own views, once it is known.
    await waitFor(() => expect(page.result.current.value.views).toEqual([triage]));
    let answer = page.result.current.value.save("Open", "q=x");
    expect(answer.kind).toBe("problem");
    expect(storage.writes).toEqual([]);

    act(() => {
      answer = page.result.current.value.save(" Mine ", "f.author=%40me");
    });
    expect(answer).toEqual({ kind: "new", name: "Mine" });
    const mine = { name: "Mine", query: "f.author=%40me" };
    expect(page.result.current.value.views).toEqual([triage, mine]);
    expect(kept()).toEqual({ version: 1, projects: { ACME: [acme], CORE: [triage, mine] } });

    expect(page.result.current.value.check("MINE")).toEqual({
      kind: "replace",
      name: "MINE",
      replaces: "Mine",
    });
    act(() => {
      answer = page.result.current.value.save("mine", "f.author=%40me&layout=board");
    });
    expect(answer).toEqual({ kind: "replace", name: "mine", replaces: "Mine" });
    expect(page.result.current.value.views).toEqual([
      triage,
      { name: "mine", query: "f.author=%40me&layout=board" },
    ]);

    act(() => page.result.current.value.remove("Mine"));
    expect(page.result.current.value.views).toEqual([triage]);
    expect(kept()).toEqual({ version: 1, projects: { ACME: [acme], CORE: [triage] } });
  } finally {
    page.done();
    storage.restore();
  }
});
