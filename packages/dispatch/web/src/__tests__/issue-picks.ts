/** What the unit rows of an issue's picks share (`issue-field-writes.test.tsx`,
 *  `assignee-repo-picks.test.tsx`): the page's query client signed in, and what each save
 *  Dispatch received carried. */
import type { QueryKey } from "@tanstack/react-query";

import { DispatchQueryClient } from "../api/query-client";

/** The page's query client, signed in as alice, holding what Dispatch last answered under each
 *  of `cached`'s keys. */
export function signedInHolding(
  ...cached: readonly (readonly [QueryKey, unknown])[]
): DispatchQueryClient {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  for (const [queryKey, data] of cached) client.setQueryData(queryKey, data);
  return client;
}

/** What each save Dispatch received carried. */
export function bodies(patchIssue: { mock: { calls: unknown[][] } }): unknown[] {
  return patchIssue.mock.calls.map((call) => call[1]);
}
