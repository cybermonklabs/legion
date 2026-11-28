import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../api/client";
import { whoAmIQuery } from "../api/queries";
import type { AuthenticatedUser } from "../api/types";
import { useHoldsPersonScope } from "./useHoldsPersonScope";

/** `useHoldsPersonScope` drawn while `/auth/whoami` answers `viewer`, and `GET /api/v1/whoami`'s
 *  scopes are `scopes`, or unanswered where `scopes` is `undefined`. */
function holdsFor(viewer: AuthenticatedUser, scopes: readonly string[] | undefined) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, viewer);
  const read = spyOn(api, "whoAmIScopes").mockImplementation(() =>
    scopes === undefined ? Promise.withResolvers<string[]>().promise : Promise.resolve([...scopes])
  );
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const view = renderHook(() => useHoldsPersonScope(), { wrapper });
  return { client, read, view };
}

const agent: AuthenticatedUser = { event_head: "0", kind: "agent", login: "alice", session: null };

// A person's sign-in holds the `person` scope, so their scopes are not read; an agent holds it
// where its credential's scopes name it, and until they are read it holds nothing.
test("a person holds the person scope unread, and an agent only once its scopes name it", async () => {
  const person = holdsFor({ event_head: "0", kind: "user", login: "alice" }, ["person"]);
  try {
    expect(person.view.result.current).toBe(true);
    expect(person.read).not.toHaveBeenCalled();
  } finally {
    person.view.unmount();
    person.read.mockRestore();
  }

  const holding = holdsFor(agent, ["person", "approve"]);
  try {
    expect(holding.view.result.current).toBe(false);
    await waitFor(() => expect(holding.view.result.current).toBe(true));
    expect(holding.client.getQueryData<string[]>(["whoami-scopes"])).toEqual(["person", "approve"]);
  } finally {
    holding.view.unmount();
    holding.read.mockRestore();
  }

  const without = holdsFor(agent, ["approve"]);
  try {
    await waitFor(() =>
      expect(without.client.getQueryData<string[]>(["whoami-scopes"])).toEqual(["approve"])
    );
    expect(without.view.result.current).toBe(false);
  } finally {
    without.view.unmount();
    without.read.mockRestore();
  }

  const loading = holdsFor(agent, undefined);
  try {
    await waitFor(() => expect(loading.read).toHaveBeenCalledTimes(1));
    expect(loading.view.result.current).toBe(false);
  } finally {
    loading.view.unmount();
    loading.read.mockRestore();
  }
});
