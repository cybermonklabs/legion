import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type { AuthenticatedUser } from "../api/types";
import { AppShell } from "../app";
import { DEFAULT_MARGIN_WIDTH } from "../features/margin/margin-width";
import { KeymapProvider } from "../features/shell/KeymapProvider";

/**
 * What the shell's first commit holds, on a route that draws a margin and on one that does not.
 *
 * The margin's own chunk waits for the route to be drawn (`TheRouteIsDrawn` in `app.tsx`),
 * because a boundary suspended beside the route's own costs the page a 300 ms fallback throttle.
 * Its column must not wait: `theRouteIsDrawn` is turned on by an effect, so while the column
 * waited for it there was a commit - the one the reader's first paint comes from - that laid
 * `<main>` out across the margin's column, and everything in it was re-wrapped as soon as the
 * browser painted that commit (dispatch://INFRA-341, R11).
 *
 * Whether the browser paints it is the scheduler's to decide: one sweep of eight margin routes
 * painted 72 to 149 ms of it and twenty-one later loads painted none, which is why the two
 * `e2e/layout.e2e.ts` rows are guards and this row is the proof. `flushSync` is what makes it
 * exact. It renders and commits in the call, and React schedules passive effects rather than
 * running them, so what the container holds when it returns is the tree with `theRouteIsDrawn`
 * still `false` - the first commit, read before anything can turn it into the second.
 */

const user: AuthenticatedUser = { event_head: "0", kind: "user", login: "alice" };
const originalMatchMedia = window.matchMedia;
const originalApi = { ...api };

afterEach(() => {
  window.matchMedia = originalMatchMedia;
  Object.assign(api, originalApi);
});

/** A read this row makes and never answers: the query is still waiting when the root goes, and
 *  nothing of it is asserted. What matters is that it answers from here rather than from the
 *  network, which would fail the file (`web/src/__tests__/setup.ts`). */
function neverAnswers(): Promise<never> {
  return new Promise<never>(() => {});
}

/** The shell's first commit at `route`, as the markup that commit put in the page. The providers
 *  are the ones `AuthenticatedApp` wraps it in. Nothing fetches in a render, so what the reads
 *  below answer reaches no assertion here; they are answered because the shell's own effects
 *  make them as the root is torn down, and a read that reaches the network fails the file
 *  (`web/src/__tests__/setup.ts`). The viewport is desktop, where the margin is a column
 *  beside the document rather than a sheet over it. */
function firstCommitAt(route: string): string {
  api.getInbox = async () => [];
  api.listIssues = async () => [];
  // The shell records the item the page is on for the search bar to list back
  // (`features/search/recents.ts`), which reads the issue or the project document the route
  // names. Both reads are the page's own in the running app; here the shell stands alone, so
  // they are the shell's and are answered by nothing.
  api.getIssue = neverAnswers;
  api.getProjectArtifact = neverAnswers;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const container = document.createElement("div");
  document.body.append(container);
  const root = createRoot(container);
  try {
    flushSync(() => {
      root.render(
        <QueryClientProvider
          client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
        >
          <MemoryRouter initialEntries={[route]}>
            <KeymapProvider>
              <AppShell user={user} />
            </KeymapProvider>
          </MemoryRouter>
        </QueryClientProvider>
      );
    });
    const drawn = container.innerHTML;
    // Before the scheduler's task runs, so the shell's reads never go out: this row is about
    // one commit, and the page behind it is `responsive-shell.test.tsx`'s.
    flushSync(() => root.unmount());
    return drawn;
  } finally {
    container.remove();
  }
}

test("the margin's column is reserved in the commit the first paint comes from", () => {
  const markup = firstCommitAt("/issues/CORE-1/conversation");

  expect(markup).toContain('data-testid="margin-reservation"');
  expect(markup, "the column is held at the width the margin lays itself out at").toContain(
    `width: ${DEFAULT_MARGIN_WIDTH}px`
  );
  // A reserved column is a page still settling, so the review and the accessibility layers wait
  // it out and read the real margin rather than an empty box (`e2e/review-routes.ts`).
  expect(markup).toContain("data-margin-pending");
});

test("a project document's commit reserves it too, since that route draws a margin", () => {
  expect(firstCommitAt("/projects/CORE/documents/design-language")).toContain(
    'data-testid="margin-reservation"'
  );
});

test("a route that draws no margin reserves nothing in that commit", () => {
  // The reservation answers `routeHasMargin`, so a route with no margin holds no box: reserving
  // one would run the shift backwards, taking 384 px from the page at the first paint and
  // handing it back when the margin arrives with nothing to put there.
  expect(firstCommitAt("/agents")).not.toContain('data-testid="margin-reservation"');
});
