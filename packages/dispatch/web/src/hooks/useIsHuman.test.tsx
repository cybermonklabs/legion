import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import { whoAmIQuery } from "../api/queries";
import type { AuthenticatedUser } from "../api/types";
import { useIsHuman } from "./useIsHuman";

/** What `useIsHuman` answers while `/auth/whoami`'s answer is `viewer`. */
function isHumanFor(viewer: AuthenticatedUser): boolean {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, viewer);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return renderHook(() => useIsHuman(), { wrapper }).result.current;
}

// Every surface behind a route that needs the `person` scope asks only for a person: an agent
// viewer of the dashboard is answered `kind: "agent"`, whatever login label it carries - its
// token's owner included - so none of those surfaces asks for it.
test("a person is human, and an agent viewer is not, whatever its login label", () => {
  expect(isHumanFor({ event_head: "0", kind: "user", login: "alice" })).toBe(true);
  expect(isHumanFor({ event_head: "0", kind: "agent", login: "agent", session: null })).toBe(false);
  expect(isHumanFor({ event_head: "0", kind: "agent", login: "alice", session: null })).toBe(false);
  expect(
    isHumanFor({ event_head: "0", kind: "agent", login: "agent:ses_1", session: "ses_1" })
  ).toBe(false);
});
