import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { AuthenticatedUser } from "../../api/types";
import { AskHandOn } from "./AskHandOn";
import { answeredAsk, renderCard } from "./ask-card-fixture";

const agent: AuthenticatedUser = {
  event_head: "0",
  kind: "agent",
  login: "agent:planner-session",
  session: "planner-session",
};

/** The hand-on control of an open ask, drawn for an agent viewer whose credential's scopes are
 *  `scopes`, with everyone who has signed in answered as bob, once the scopes have answered. */
async function drawnFor(scopes: string[]) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(whoAmIQuery().queryKey, agent);
  const whoAmIScopes = spyOn(api, "whoAmIScopes").mockResolvedValue(scopes);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "bob" }]);
  const ask = answeredAsk({ answer: null, state: "open" });
  const { view } = renderCard(<AskHandOn assignee={null} ask={ask} />, queryClient);
  const control = await within(view.container).findByLabelText<HTMLSelectElement>(
    "Hand this question to"
  );
  await waitFor(() => expect(whoAmIScopes).toHaveBeenCalled());
  return {
    control,
    listUsers,
    restore: () => {
      view.unmount();
      listUsers.mockRestore();
      whoAmIScopes.mockRestore();
    },
  };
}

// Everyone who has signed in (`GET /api/v1/users`) is read under the `person` scope, once the
// reader reaches for the control: an agent viewer whose credential lacks the scope asks for none
// when it reaches for it, and one whose credential holds it reads them as a person does.
test("an agent viewer without the person scope asks for no people when it reaches for the hand-on control", async () => {
  const user = userEvent.setup();
  const lacking = await drawnFor([]);
  try {
    await user.click(lacking.control);
    await act(async () => {});
    expect(lacking.listUsers).not.toHaveBeenCalled();
    expect(within(lacking.control).queryByRole("option", { name: "bob" })).toBeNull();
  } finally {
    lacking.restore();
  }

  const holding = await drawnFor(["person"]);
  try {
    await user.click(holding.control);
    await waitFor(() =>
      expect(within(holding.control).getByRole("option", { name: "bob" })).toBeTruthy()
    );
    expect(holding.listUsers).toHaveBeenCalledTimes(1);
  } finally {
    holding.restore();
  }
});
