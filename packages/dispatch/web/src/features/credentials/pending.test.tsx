import { expect, spyOn, test } from "bun:test";
import { type Query, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { CredentialPendingResponse } from "../../api/types";
import { appClient, refocus, restoreWindowFocus } from "../issue-page/window-focus-fixture";
import { credentialPendingQuery, useCredentialRequests } from "./pending";

function renderPending(
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
) {
  const view = renderHook(() => useCredentialRequests(), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    ),
  });
  return { queryClient, view };
}

// A `null`-holding query (no secrets broker) can neither fetch again on its own nor have the poll
// or the focus refetch armed: `enabled`, `refetchInterval` and `refetchOnWindowFocus` all read the
// same `query.state.data !== null` fact, so if one ever answered differently from the others a
// brokerless Dispatch would start asking again.
test("credentialPendingQuery's enabled, refetchInterval and refetchOnWindowFocus agree: armed for a list, off for null", () => {
  const query = credentialPendingQuery();
  const heldNull = { state: { data: null } } as unknown as Query<CredentialPendingResponse | null>;
  const heldList = {
    state: { data: { pending: [] } },
  } as unknown as Query<CredentialPendingResponse | null>;

  expect(query.enabled).toBeInstanceOf(Function);
  expect(query.refetchInterval).toBeInstanceOf(Function);
  const enabled = query.enabled as (query: typeof heldNull) => boolean;
  const refetchInterval = query.refetchInterval as (query: typeof heldNull) => number | false;
  const refetchOnWindowFocus = query.refetchOnWindowFocus as (
    query: typeof heldNull
  ) => boolean | "always";

  expect(enabled(heldNull)).toBe(false);
  expect(refetchInterval(heldNull)).toBe(false);
  expect(enabled(heldList)).toBe(true);
  expect(typeof refetchInterval(heldList)).toBe("number");
  expect(refetchInterval(heldList)).toBeGreaterThan(0);
  expect(refetchOnWindowFocus(heldNull)).toBe(false);
  expect(refetchOnWindowFocus(heldList)).toBe("always");
});

// A secret request's row opens the broker's page in a new tab, so the person decides it there and
// comes back. The return asks for the list again although it is fresher than the app's 30 s
// staleness, so the decided request leaves the Inbox then and not at the next poll.
test("a return to the tab asks for a held list again, however fresh it is", async () => {
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const queryClient = appClient();
  const { view } = renderPending(queryClient);
  try {
    await waitFor(() => expect(view.result.current.status).toBe("listed"));
    expect(getCredentialPending).toHaveBeenCalledTimes(1);

    await act(() => refocus(queryClient));

    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(2));
  } finally {
    view.unmount();
    getCredentialPending.mockRestore();
    restoreWindowFocus();
  }
});

// LEGION-575 round 2. A background poll that fails after a list already loaded must not blank a
// good list: `useCredentialRequests` used to key `status: "failed"` off `isError` alone, which
// TanStack also sets on a *refetch* error with the last good `data` still attached - so the
// Inbox's section, its empty state and the Needs-you count would all drop a real pending request
// for as long as one poll tick failed, self-healing only on the next successful tick.
test("a poll that fails after a list already loaded keeps showing that list, not a failure", async () => {
  const getCredentialPending = spyOn(api, "getCredentialPending")
    .mockResolvedValueOnce({
      pending: [
        {
          identifiers: ["DEMO_API_KEY"],
          kind: "agent_secret",
          record_id: "req-1",
          requested_at: "2026-09-27T00:00:00Z",
          session: { enrollment: null, request: null },
        },
      ],
    })
    .mockRejectedValueOnce(new Error("network"));
  const { queryClient, view } = renderPending();
  try {
    await waitFor(() => expect(view.result.current.status).toBe("listed"));
    expect(view.result.current.requests).toHaveLength(1);

    await queryClient.refetchQueries({ queryKey: ["credential-pending"] });

    expect(getCredentialPending).toHaveBeenCalledTimes(2);
    expect(view.result.current.status).toBe("listed");
    expect(view.result.current.requests).toHaveLength(1);
    expect(view.result.current.requests[0]?.record_id).toBe("req-1");
  } finally {
    view.unmount();
    getCredentialPending.mockRestore();
  }
});
