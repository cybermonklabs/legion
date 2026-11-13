import { afterEach, beforeEach, jest, type Mock, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { Ask, AskAnswer, AskRead } from "../../api/types";

// The card `AskCard.test.tsx` and `AskCard.stopped-writes.test.tsx` both render, and the reads
// every render of it answers.

/** Installs the mocks both files' cards read through: call once at module scope. */
export function installAskCardApiMocks() {
  let whoAmI: Mock<typeof api.whoAmI>;
  let getReferences: Mock<typeof api.getReferences>;
  let getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;

  beforeEach(() => {
    whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "0",
      kind: "user",
      login: "alice",
    });
    getReferences = spyOn(api, "getReferences").mockResolvedValue({
      edges: [],
      node: graphNode({ id: "ask-1", kind: "ask" }),
    });
    // Every card with an issue names who the ask reaches, which reads the issue's subscribers;
    // nobody is subscribed here, so the card lists its followers alone.
    getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  });

  afterEach(() => {
    whoAmI.mockRestore();
    getReferences.mockRestore();
    getIssueSubscribers.mockRestore();
    jest.useRealTimers();
  });
}

export const firstAnswer: AskAnswer = {
  at: "2026-10-08T01:00:00Z",
  selected: ["Ship"],
  text: "First note",
  user: "alice",
};

export function answeredAsk(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: firstAnswer,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which release path?",
    state: "answered",
    urgency: "med",
    waiting_on: "human",
    ...overrides,
  };
}

export function thread(
  ask: Ask,
  answers: AskAnswer[] = ask.answer === null ? [] : [ask.answer]
): AskRead {
  return {
    answers,
    ask,
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
  };
}

export function renderCard(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, view };
}
