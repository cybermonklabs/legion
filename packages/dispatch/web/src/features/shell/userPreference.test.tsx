import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { api } from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import { userPreferenceStorageKey, useUserPreference } from "./userPreference";

function PreferenceProbe() {
  const [shown, setShown] = useUserPreference(
    "project.board-edges",
    (stored) => stored === "shown",
    (next) => (next ? "shown" : "hidden")
  );
  return (
    <button onClick={() => setShown(!shown)} type="button">
      {shown ? "shown" : "hidden"}
    </button>
  );
}

test("a user preference stays at its caller default until login, then follows that login's storage", async () => {
  const user = userEvent.setup();
  const whoAmIResult = Promise.withResolvers<AuthenticatedUser>();
  const whoAmI = spyOn(api, "whoAmI").mockReturnValue(whoAmIResult.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const aliceKey = userPreferenceStorageKey("alice", "project.board-edges");
  const bobKey = userPreferenceStorageKey("bob", "project.board-edges");
  const undefinedKey = userPreferenceStorageKey("undefined", "project.board-edges");
  window.localStorage.setItem(aliceKey, "shown");
  window.localStorage.setItem(bobKey, "hidden");
  window.localStorage.removeItem(undefinedKey);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PreferenceProbe />
    </QueryClientProvider>
  );

  try {
    expect(screen.getByRole("button").textContent).toBe("hidden");
    await user.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(aliceKey)).toBe("shown");
    expect(window.localStorage.getItem(undefinedKey)).toBeNull();

    await act(async () => whoAmIResult.resolve({ event_head: "0", kind: "user", login: "alice" }));
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("shown"));

    await user.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(aliceKey)).toBe("hidden");

    act(() => queryClient.setQueryData(["whoami"], { kind: "user", login: "bob" }));
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("hidden"));
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(aliceKey);
    window.localStorage.removeItem(bobKey);
    window.localStorage.removeItem(undefinedKey);
  }
});
