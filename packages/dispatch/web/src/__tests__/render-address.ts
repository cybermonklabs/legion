import { renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

/** A hook on an address, with where the reader is and a way back through the history the hook
 *  has written: whether a write replaces its entry or pushes one is only visible from a history
 *  that has something underneath it. */
export function renderAddress<T>(use: () => T, entries: readonly string[] = ["/page"]) {
  return renderHook(() => ({ back: useNavigate(), location: useLocation(), value: use() }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(MemoryRouter, { initialEntries: [...entries] }, children),
  });
}
