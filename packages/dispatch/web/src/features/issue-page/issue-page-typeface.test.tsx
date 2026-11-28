import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import { Suspense } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { whoAmIQuery } from "../../api/queries";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { IssuePage } from "./IssuePage";

const ALICE_KEY = userPreferenceStorageKey("alice", "issue.page");

// The test DOM may hold no `document.fonts`; each case puts a stand-in in its place that records
// what is asked of it, and the case after it finds the document, and alice's choice, as they were.
const own = Object.getOwnPropertyDescriptor(document, "fonts");
afterEach(() => {
  if (own === undefined) Reflect.deleteProperty(document, "fonts");
  else Object.defineProperty(document, "fonts", own);
  window.localStorage.removeItem(ALICE_KEY);
});

function fontsAsked(): string[] {
  const asked: string[] = [];
  Object.defineProperty(document, "fonts", {
    configurable: true,
    value: {
      load: (font: string) => {
        asked.push(font);
        return Promise.resolve([]);
      },
    },
  });
  return asked;
}

/** Draws `IssuePage` in a route, as the app does, for alice, signed in, with `choice` stored as
 *  her page, at an address that names no issue: today's page draws its not-found page there, and
 *  the new page's lazy chunk is still loading when the first render ends. The case unmounts it
 *  before that chunk arrives. */
function drawnFor(choice: "outline" | "tabs") {
  window.localStorage.setItem(ALICE_KEY, choice);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  return render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={client}>
        <Suspense fallback={<p>loading</p>}>
          <Routes>
            <Route element={<IssuePage />} path="*" />
          </Routes>
        </Suspense>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("row 1: where the setting selects the new page, its render requests Dispatch Inter while the lazy page is still loading", () => {
  const asked = fontsAsked();
  const view = drawnFor("outline");
  try {
    expect(view.getByText("loading"), "the new page's chunk has not arrived").toBeDefined();
    // React renders a page whose lazy child suspends more than once, and a load already asked is
    // answered from the one the browser holds, so the case reads which fonts were asked.
    expect([...new Set(asked)], "every weight is in the one file `400` requests").toEqual([
      '400 1em "Dispatch Inter"',
    ]);
  } finally {
    view.unmount();
  }
});

test("row 2: where the setting selects today's page, its render requests no font", () => {
  const asked = fontsAsked();
  const view = drawnFor("tabs");
  try {
    expect(asked).toEqual([]);
  } finally {
    view.unmount();
  }
});
