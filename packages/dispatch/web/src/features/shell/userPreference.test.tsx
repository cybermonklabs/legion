import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { installCountingStorage } from "../../__tests__/counting-storage";
import { api } from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import {
  userPreferenceStorageKey,
  useStoredUserPreference,
  useUserPreference,
} from "./userPreference";

function PreferenceProbe() {
  const [view, setView] = useUserPreference(
    "project.issue-view",
    (stored) => (stored === "board" ? "board" : "list"),
    (next) => next
  );
  return (
    <button onClick={() => setView(view === "list" ? "board" : "list")} type="button">
      {view}
    </button>
  );
}

test("a user preference stays at its caller default until login, then follows that login's storage", async () => {
  const user = userEvent.setup();
  const whoAmIResult = Promise.withResolvers<AuthenticatedUser>();
  const whoAmI = spyOn(api, "whoAmI").mockReturnValue(whoAmIResult.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const aliceKey = userPreferenceStorageKey("alice", "project.issue-view");
  const bobKey = userPreferenceStorageKey("bob", "project.issue-view");
  const undefinedKey = userPreferenceStorageKey("undefined", "project.issue-view");
  window.localStorage.setItem(aliceKey, "board");
  window.localStorage.setItem(bobKey, "list");
  window.localStorage.removeItem(undefinedKey);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PreferenceProbe />
    </QueryClientProvider>
  );

  try {
    expect(screen.getByRole("button").textContent).toBe("list");
    await user.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(aliceKey)).toBe("board");
    expect(window.localStorage.getItem(undefinedKey)).toBeNull();

    await act(async () => whoAmIResult.resolve({ event_head: "0", kind: "user", login: "alice" }));
    // The click made while the identity was pending left the button at "board" already, so its
    // text alone does not say the login has resolved: wait for alice's identity, then her value.
    await waitFor(() =>
      expect(queryClient.getQueryData<AuthenticatedUser>(["whoami"])).toEqual({
        event_head: "0",
        kind: "user",
        login: "alice",
      })
    );
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("board"));

    await user.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(aliceKey)).toBe("list");

    act(() => queryClient.setQueryData(["whoami"], { kind: "user", login: "bob" }));
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("list"));
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(aliceKey);
    window.localStorage.removeItem(bobKey);
    window.localStorage.removeItem(undefinedKey);
  }
});

/** A preference whose `read` folds two stored values into one, as the Board's panel folds an
 *  "expanded" stored while no filter is on into shut. */
function FoldingProbe() {
  const [open, setOpen] = useUserPreference(
    "project.issue-filters",
    () => false,
    (next) => (next ? "expanded" : "collapsed")
  );
  return (
    <button onClick={() => setOpen(!open)} type="button">
      {open ? "open" : "shut"}
    </button>
  );
}

test("a preference write whose value is already stored is not made", async () => {
  const user = userEvent.setup();
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const key = userPreferenceStorageKey("alice", "project.issue-filters");
  // The page's storage, swapped for one that keeps every write made to it.
  const storage = installCountingStorage({ [key]: "expanded" });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <FoldingProbe />
    </QueryClientProvider>
  );

  try {
    // The signed-in identity, which is what lets the hook write at all.
    await waitFor(() => expect(queryClient.getQueryData(["whoami"])).toBeDefined());
    // Shut by its read while "expanded" is stored: opening it would write "expanded" over itself.
    await user.click(screen.getByRole("button", { name: "shut" }));
    expect(storage.writes).toEqual([]);
    // Closing it changes what is stored, so that write is made.
    await user.click(screen.getByRole("button", { name: "open" }));
    expect(storage.writes).toEqual([[key, "collapsed"]]);
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    storage.restore();
  }
});

/** The stored text as it is, module-level since the hook reads again whenever its `read`
 *  changes. */
const storedText = (stored: string | null) => stored ?? "";
const textToStore = (value: string) => value;

/** A preference two tabs each change a part of: a press adds `!` to what is stored. */
function StoredProbe() {
  const [text, change] = useStoredUserPreference("project.saved-views", storedText, textToStore);
  return (
    <button onClick={() => change((stored) => `${stored}!`)} type="button">
      {text}
    </button>
  );
}

// A value that two tabs of one browser each change: another tab's write reaches this tab through
// the window's `storage` event, which the test sends as the browser would, and a change here is
// made to what is stored at the moment it writes, not to the copy this tab drew last.
test("a stored preference draws another tab's write, and a change keeps what is stored now", async () => {
  const user = userEvent.setup();
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const key = userPreferenceStorageKey("alice", "project.saved-views");
  const storage = installCountingStorage({ [key]: "one" });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <StoredProbe />
    </QueryClientProvider>
  );

  try {
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("one"));
    // Another tab writes: the storage holds the value at once, and its event follows.
    window.localStorage.setItem(key, "two");
    act(() => {
      window.dispatchEvent(new StorageEvent("storage", { key, newValue: "two" }));
    });
    expect(screen.getByRole("button").textContent).toBe("two");
    // A write of another tab whose event has not come yet stays under a change made here.
    window.localStorage.setItem(key, "three");
    await user.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(key)).toBe("three!");
    expect(screen.getByRole("button").textContent).toBe("three!");
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    storage.restore();
  }
});
