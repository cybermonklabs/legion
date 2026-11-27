import { expect, jest, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { NetworkError } from "../../api/client";
import { KeymapProvider } from "../shell/KeymapProvider";
import { writesOf } from "../writes/write-store";
import { AskAssignWritesProvider, AssignToMe } from "./AssignToMe";

// An Assign to me whose save never answers - offline, each try failing at the network - holds the
// button until the save's deadline and no longer: past it the save is still tried, and the button
// still says so, and a press then saves again, behind it on the issue's line.
test("Assign to me lets go at its save's deadline and saves again", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <AskAssignWritesProvider>
            <AssignToMe askId="ask-a" issueKey="CORE-1" viewer="alice" />
          </AskAssignWritesProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const label = "CORE-1 assigned to alice";
  try {
    const button = screen.getByRole<HTMLButtonElement>("button", { name: "Assign CORE-1 to me" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await heldUntilLate(user, button, {
      label,
      name: "Assign to me",
      queryClient,
      sent: () => pressesOf([patchIssue]),
    });
    expect(button, "Assign to me, past the deadline").toHaveTextContent("Assigning…");
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(
      writesOf(queryClient)
        .rows()
        .filter((row) => row.label === label)
        .map((row) => row.status),
      "a press past the deadline saves again, behind the late one"
    ).toEqual(["late", "queued"]);
    expect(button, "Assign to me, behind the late save").not.toHaveAttribute("aria-disabled");
  } finally {
    realClock();
    view.unmount();
    queryClient.clear();
  }
});
