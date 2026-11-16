import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import { http } from "msw";
import type { ReactNode } from "react";

import { MEANING_PAUSE_MS, useTieredSearch } from "../features/search/useTieredSearch";
import { answerRequests } from "./network";

// The keystroke's tier asks for the keyword leg alone (`useTieredSearch`, `keywords_only: true`),
// which `everyBuildReadsSearch` does not keep on `/api/v1/search`, so that tier always asks for
// `/api/v1/search/results` - whatever the reader narrowed by, even nothing. The pause's tier
// sends the request as the chips built it, so a reader who narrowed by nothing leaves it on
// `/api/v1/search`.
test("the keystroke tier asks for the results path, and an unnarrowed pause tier for /api/v1/search", async () => {
  // The paths the two tiers asked for, in the order they were asked. Each is answered an empty
  // page, which is all the hook needs to resolve.
  const asked: string[] = [];
  const record = ({ request }: { request: Request }): Response => {
    asked.push(new URL(request.url).pathname);
    return Response.json({ results: [], took_ms: 1 });
  };
  answerRequests(http.get("/api/v1/search/results", record), http.get("/api/v1/search", record));

  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }): ReactNode => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(
    () => useTieredSearch({ enabled: true, request: {}, words: "astrolabe" }),
    { wrapper }
  );

  try {
    await waitFor(() => expect(asked).toEqual(["/api/v1/search/results", "/api/v1/search"]), {
      // Both tiers' waits, and the answers behind them.
      timeout: MEANING_PAUSE_MS * 8,
    });
  } finally {
    hook.unmount();
    client.clear();
  }
});
