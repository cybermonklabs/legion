import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { projectIssuesQueryKey, useIssueFilters } from "./issue-filters";

function renderFilters(initialEntry = "/projects/CORE") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderHook(() => useIssueFilters(), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(
        MemoryRouter,
        { initialEntries: [initialEntry] },
        createElement(QueryClientProvider, { client }, children)
      ),
  });
}

test("parses every filter from the URL", () => {
  const hook = renderFilters(
    "/projects/CORE?label=a&label=b&q=ship&needs-you=1&unread=1&status=todo&status=done"
  );
  try {
    expect(hook.result.current.labels).toEqual(["a", "b"]);
    // A `?q=` and a `?status=` in the URL are the table's own search and column filter: this
    // hook reads neither.
    expect(hook.result.current.needsYou).toBe(true);
    expect(hook.result.current.unread).toBe(true);
    expect(hook.result.current.unclaimed).toBe(false);
  } finally {
    hook.unmount();
  }
});

test("setters round-trip through the URL and clearing removes the parameter", () => {
  const hook = renderFilters();
  try {
    act(() => hook.result.current.setNeedsYou(true));
    act(() => hook.result.current.setUnread(true));
    act(() => hook.result.current.setLabels(["frontend"]));
    expect(hook.result.current.needsYou).toBe(true);
    expect(hook.result.current.unread).toBe(true);
    expect(hook.result.current.labels).toEqual(["frontend"]);
    act(() => hook.result.current.setNeedsYou(false));
    act(() => hook.result.current.setUnread(false));
    act(() => hook.result.current.setLabels([]));
    expect(hook.result.current.needsYou).toBe(false);
    expect(hook.result.current.unread).toBe(false);
    expect(hook.result.current.labels).toEqual([]);
  } finally {
    hook.unmount();
  }
});

test("the query key is the project's plain list", () => {
  expect(projectIssuesQueryKey("CORE")).toEqual(["issues", "project", "CORE"]);
});
