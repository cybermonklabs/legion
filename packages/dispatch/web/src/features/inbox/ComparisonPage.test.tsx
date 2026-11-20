/**
 * The comparison page's read sits outside the Inbox's cache prefix (dispatch://INFRA-1152): every
 * optimistic write over the Inbox's asks patches each read under `["inbox"]` as a list of rows, so
 * a comparison cached there would turn each of those writes into a throw for as long as it stays
 * cached.
 */

import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { ComparisonPage } from "./ComparisonPage";
import { issueAsk } from "./inbox-fixture";

afterEach(cleanup);

test("the comparison is cached outside the Inbox's prefix, so a write over the Inbox's rows patches no comparison", async () => {
  const comparison = spyOn(api, "getInboxComparison").mockResolvedValue({ run: null });
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(inboxQuery().queryKey, [issueAsk({ id: "ask-a" })]);
    render(
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <ComparisonPage />
        </QueryClientProvider>
      </MemoryRouter>
    );
    await screen.findByText("The gate is not met, because no comparison ran.");

    const underInbox = client.getQueriesData({ queryKey: inboxQuery().queryKey });
    expect(underInbox.map(([key]) => key)).toEqual([inboxQuery().queryKey]);
    // A snooze's own patch, which a comparison under the prefix would have thrown on.
    expect(() =>
      client.setQueriesData<InboxRow[]>({ queryKey: inboxQuery().queryKey }, (current) =>
        current?.map((row) => ({ ...row, snoozed_until: "2026-10-20T09:00:00Z" }))
      )
    ).not.toThrow();
    expect(client.getQueryData<InboxRow[]>(inboxQuery().queryKey)?.[0]?.snoozed_until).toBe(
      "2026-10-20T09:00:00Z"
    );
  } finally {
    comparison.mockRestore();
  }
});
