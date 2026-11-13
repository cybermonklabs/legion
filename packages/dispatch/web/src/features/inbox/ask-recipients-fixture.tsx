import { spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, AskFollower, Subscriber } from "../../api/types";
import { AskRecipients, type AskRecipientsProps } from "./AskRecipients";

// The recipients `AskRecipients.test.tsx` and `AskRecipients.stopped-writes.test.tsx` both
// render, with the followers and the subscribers they list.

export const followers: AskFollower[] = [
  { session_id: "0123456789abcdef", since: "2026-09-14T10:00:00Z" },
  { session_id: "fedcba9876543210", since: "2026-09-14T10:05:00Z" },
];

export function subscriber(
  sessionId: string,
  title: string,
  extra: Partial<Subscriber> = {}
): Subscriber {
  return {
    last_seen: Date.now(),
    live: true,
    removable: true,
    session_id: sessionId,
    title,
    topics: ["notifications.dispatch.issue.CORE-1.>"],
    ...extra,
  };
}

export async function renderRecipients({
  agents = [],
  props = {},
  removeFollower = async () => {},
  subscribers = [],
}: {
  /** An Error rejects the agent registry read, as an unreachable Envoy listener does. */
  agents?: Agent[] | Error;
  props?: Partial<AskRecipientsProps>;
  removeFollower?: (askId: string, sessionId: string) => Promise<void>;
  /** An Error rejects the subscribers fetch, as an unreachable Envoy listener does. */
  subscribers?: Subscriber[] | Error;
} = {}) {
  const listAgents = spyOn(api, "listAgents");
  if (agents instanceof Error) {
    listAgents.mockRejectedValue(agents);
  } else {
    listAgents.mockResolvedValue(agents);
  }
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers");
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers");
  if (subscribers instanceof Error) {
    getIssueSubscribers.mockRejectedValue(subscribers);
    getArtifactSubscribers.mockRejectedValue(subscribers);
  } else {
    getIssueSubscribers.mockResolvedValue(subscribers);
    getArtifactSubscribers.mockResolvedValue(subscribers);
  }
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  // Each recipient is an `AgentName`, whose live link needs a router.
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskRecipients
          askId="ask-1"
          claimant={null}
          followers={followers}
          owner={{ artifact_id: null, issue_key: "CORE-1" }}
          removeFollower={removeFollower}
          route={null}
          routeHolder={null}
          routeStatus={null}
          {...props}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const user = userEvent.setup();
  // The routing list is folded at rest; every assertion of the tests that render it is about
  // what it holds, so the harness opens it once and each test reads the open list.
  await user.click(screen.getByRole("button", { name: /^Reaches/ }));
  return {
    getArtifactSubscribers,
    getIssueSubscribers,
    queryClient,
    unmount: () => {
      view.unmount();
      listAgents.mockRestore();
      getIssueSubscribers.mockRestore();
      getArtifactSubscribers.mockRestore();
    },
    user,
  };
}
