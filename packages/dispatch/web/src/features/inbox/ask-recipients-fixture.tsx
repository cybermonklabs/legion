import { expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
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
  client: queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  }),
  props = {},
  removeFollower = async () => {},
  subscribers = [],
}: {
  /** An Error rejects the agent registry read, as an unreachable Envoy listener does. */
  agents?: Agent[] | Error;
  /** The query client the card reads through: a plain one unless the case needs the app's own
   *  (`DispatchQueryClient`). */
  client?: QueryClient;
  props?: Partial<AskRecipientsProps>;
  removeFollower?: AskRecipientsProps["removeFollower"];
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
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  // Each recipient is an `AgentName`, whose live link needs a router.
  const recipients = (drawn: Partial<AskRecipientsProps>) => {
    const card = (
      <AskRecipients
        askId="ask-1"
        claimant={null}
        followers={followers}
        owner={{ artifact_id: null, issue_key: "CORE-1" }}
        removeFollower={removeFollower}
        route={null}
        routeHolder={null}
        routeStatus={null}
        {...drawn}
      />
    );
    return (
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>{card}</QueryClientProvider>
      </MemoryRouter>
    );
  };
  const view = render(recipients(props));
  const user = userEvent.setup();
  // The toggle names no count while the owner's subscriber read is out, so the harness waits for
  // that read to answer, or to fail, and each test reads the card as that read left it.
  await waitFor(() => expect(queryClient.isFetching({ queryKey: ["subscribers"] })).toBe(0));
  // The routing list is folded at rest; every assertion of the tests that render it is about
  // what it holds, so the harness opens it once and each test reads the open list. The toggle
  // reads `Reaches N`, or `Recipients` where the subscriber read failed.
  await user.click(
    within(screen.getByRole("region", { name: "Recipients" })).getByRole("button", {
      name: /^(?:Reaches \d|Recipients$)/,
    })
  );
  return {
    /** Takes the card off the page and draws it again, its routing list folded, as a host whose
     *  reading view closes and opens again does. */
    drawAgain: () => {
      view.rerender(
        <MemoryRouter>
          <QueryClientProvider client={queryClient}>{null}</QueryClientProvider>
        </MemoryRouter>
      );
      view.rerender(recipients(props));
    },
    getArtifactSubscribers,
    getIssueSubscribers,
    queryClient,
    /** Draws the same card again with `next` over the props it was rendered with, as a host
     *  that draws one card for one ask after another does. */
    rerender: (next: Partial<AskRecipientsProps>) =>
      view.rerender(recipients({ ...props, ...next })),
    unmount: () => {
      view.unmount();
      listAgents.mockRestore();
      getIssueSubscribers.mockRestore();
      getArtifactSubscribers.mockRestore();
    },
    user,
  };
}
