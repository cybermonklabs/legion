import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ReplyTurn } from "./ReplyTurn";
import type { TargetedMessageAttempt } from "./TargetedMessageCard";

/** A person's reply delivered to the planner, its one attempt `attempt`, with a retry row to offer. */
function deliveredReply(attempt: TargetedMessageAttempt) {
  return (
    <MemoryRouter>
      <QueryClientProvider client={new QueryClient()}>
        <ul>
          <ReplyTurn
            at="2026-11-15T10:00:00Z"
            author={{ id: "alice", kind: "user" }}
            body={<p>Any update?</p>}
            delivery={{
              attempts: [attempt],
              retry: {
                canAside: true,
                canBtw: true,
                canSteer: true,
                onRetry: () => {},
                retrying: false,
              },
              targetName: "planner",
            }}
            turnID="turn-1"
          />
        </ul>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Inside the stream's duplicate window, where a failed attempt is still offered its Retry. */
const createdAt = new Date(Date.now() - 60_000).toISOString();

// A kept reply waits for the agent on its agent subject, so its mode changes would put a second
// copy in front of the agent when it comes back. A failed reply keeps them.
test("a kept reply offers no retry row, where a failed one offers its retry and mode change", () => {
  const failed = render(
    deliveredReply({ attempt: 1, createdAt, delivery: "steer", error: "refused", state: "failed" })
  );
  try {
    expect(screen.getAllByRole("button").map((button) => button.textContent)).toEqual([
      "Retry",
      "Use BTW instead",
    ]);
  } finally {
    failed.unmount();
  }

  const kept = render(deliveredReply({ attempt: 1, createdAt, delivery: "steer", state: "kept" }));
  try {
    expect(
      screen.getByText("Kept for planner: it is delivered when the agent comes back (Send)")
    ).toBeTruthy();
    expect(screen.queryAllByRole("button").map((button) => button.textContent)).toEqual([]);
  } finally {
    kept.unmount();
  }
});
