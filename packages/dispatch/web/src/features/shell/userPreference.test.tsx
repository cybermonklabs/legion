import { describe, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { installCountingStorage } from "../../__tests__/counting-storage";
import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { AuthenticatedUser } from "../../api/types";
import { type UserPreference, userPreferenceStorageKey, useUserPreference } from "./userPreference";

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

/** A client signed in as alice that never asks who is signed in again. */
function aliceSignedIn(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  return client;
}

/** One reader of a preference: a button named `name` that shows the value the reader holds and
 *  writes `next` when it is pressed. `seen` is told each value the reader draws. */
function Reader<T>({
  name,
  next,
  preference,
  read,
  seen,
  write,
}: {
  name: string;
  next?: T;
  preference: UserPreference;
  read: (stored: string | null) => T;
  seen?: (value: T) => void;
  write: (value: T) => string;
}): ReactNode {
  const [value, set] = useUserPreference(preference, read, write);
  seen?.(value);
  return (
    <button
      aria-label={name}
      onClick={() => {
        if (next !== undefined) set(next);
      }}
      type="button"
    >
      {typeof value === "string" ? value : JSON.stringify(value)}
    </button>
  );
}

function shown(name: string): string | null {
  return screen.getByRole("button", { name }).textContent;
}

/** The project's issue view, as `ProjectPage` reads it: the Board where "board" is stored, else
 *  the List. */
function readView(stored: string | null): "board" | "list" {
  return stored === "board" ? "board" : "list";
}

/** Each preference's key for alice, written out, so a key that changed its form fails here. */
const ALICE_KEYS: Record<UserPreference, string> = {
  "inbox.folded-groups": "dispatch.inbox.folded-groups:alice",
  "inbox.pane-width": "dispatch.inbox.pane-width:alice",
  "inbox.thread-order": "dispatch.inbox.thread-order:alice",
  "issue.details": "dispatch.issue.details:alice",
  "issue.margin-sections": "dispatch.issue.margin-sections:alice",
  "issue.page": "dispatch.issue.page:alice",
  "project.board-edges": "dispatch.project.board-edges:alice",
  "project.board-lanes": "dispatch.project.board-lanes:alice",
  "project.issue-filters": "dispatch.project.issue-filters:alice",
  "project.issue-view": "dispatch.project.issue-view:alice",
  "search.recents": "dispatch.search.recents:alice",
  "shell.margin": "dispatch.shell.margin:alice",
  "shell.margin-width": "dispatch.shell.margin-width:alice",
  "shell.sidebar": "dispatch.shell.sidebar:alice",
};

describe("a preference reaches every reader", () => {
  test("a write by one reader reaches a second reader of the same preference, with neither mounted again", async () => {
    const user = userEvent.setup();
    const storage = installCountingStorage();
    const view = render(
      <QueryClientProvider client={aliceSignedIn()}>
        <Reader
          name="Writer"
          next="board"
          preference="project.issue-view"
          read={readView}
          write={(value) => value}
        />
        <Reader
          name="Other"
          preference="project.issue-view"
          read={readView}
          write={(value) => value}
        />
      </QueryClientProvider>
    );
    try {
      const writer = screen.getByRole("button", { name: "Writer" });
      const other = screen.getByRole("button", { name: "Other" });
      expect([writer.textContent, other.textContent]).toEqual(["list", "list"]);

      await user.click(writer);
      expect([shown("Writer"), shown("Other")]).toEqual(["board", "board"]);
      // The same two buttons: neither reader was mounted again to take the write.
      expect(screen.getByRole("button", { name: "Writer" })).toBe(writer);
      expect(screen.getByRole("button", { name: "Other" })).toBe(other);
      expect(storage.writes).toEqual([[ALICE_KEYS["project.issue-view"], "board"]]);
    } finally {
      view.unmount();
      storage.restore();
    }
  });

  test("a second reader whose read is not the inverse of write holds the value written", async () => {
    const user = userEvent.setup();
    const storage = installCountingStorage();
    const write = (open: boolean) => (open ? "expanded" : "collapsed");
    const view = render(
      <QueryClientProvider client={aliceSignedIn()}>
        <Reader
          name="Opener"
          next={true}
          preference="project.issue-filters"
          read={(stored) => stored === "expanded"}
          write={write}
        />
        {/* The Board's filter strip while no filter is on: its read shuts it whatever is
            stored, so read over "expanded" it would say shut. */}
        <Reader
          name="Folding"
          preference="project.issue-filters"
          read={() => false}
          write={write}
        />
      </QueryClientProvider>
    );
    try {
      expect([shown("Opener"), shown("Folding")]).toEqual(["false", "false"]);
      await user.click(screen.getByRole("button", { name: "Opener" }));
      expect([shown("Opener"), shown("Folding")]).toEqual(["true", "true"]);
      expect(storage.writes).toEqual([[ALICE_KEYS["project.issue-filters"], "expanded"]]);
    } finally {
      view.unmount();
      storage.restore();
    }
  });

  test("a write reaches no reader of another preference", async () => {
    const user = userEvent.setup();
    const storage = installCountingStorage();
    const read = (stored: string | null) => (stored === "shown" ? "shown" : "hidden");
    const view = render(
      <QueryClientProvider client={aliceSignedIn()}>
        <Reader
          name="Edges"
          next="shown"
          preference="project.board-edges"
          read={read}
          write={(value) => value}
        />
        <Reader
          name="Lanes"
          preference="project.board-lanes"
          read={read}
          write={(value) => value}
        />
      </QueryClientProvider>
    );
    try {
      await user.click(screen.getByRole("button", { name: "Edges" }));
      expect(shown("Edges")).toBe("shown");
      // The lanes keep what they drew.
      expect(shown("Lanes")).toBe("hidden");
      expect(storage.writes).toEqual([[ALICE_KEYS["project.board-edges"], "shown"]]);
    } finally {
      view.unmount();
      storage.restore();
    }
  });

  test("an object-valued preference reaches the second reader as the reference written", async () => {
    type Folded = Readonly<Record<string, readonly string[]>>;
    const user = userEvent.setup();
    const storage = installCountingStorage();
    const written: Folded = { status: ["done"] };
    const seenByOther: Folded[] = [];
    const read = (stored: string | null): Folded => (stored === null ? {} : JSON.parse(stored));
    const view = render(
      <QueryClientProvider client={aliceSignedIn()}>
        <Reader
          name="Writer"
          next={written}
          preference="inbox.folded-groups"
          read={read}
          write={(value) => JSON.stringify(value)}
        />
        <Reader
          name="Other"
          preference="inbox.folded-groups"
          read={read}
          seen={(value) => seenByOther.push(value)}
          write={(value) => JSON.stringify(value)}
        />
      </QueryClientProvider>
    );
    try {
      await user.click(screen.getByRole("button", { name: "Writer" }));
      expect(shown("Other")).toBe('{"status":["done"]}');
      // The object the writer handed over, not a copy read back from the stored text.
      expect(seenByOther.at(-1)).toBe(written);
    } finally {
      view.unmount();
      storage.restore();
    }
  });

  test("a reader that mounts while another reads the key runs its own read", async () => {
    const user = userEvent.setup();
    const storage = installCountingStorage();
    const client = aliceSignedIn();
    const tree = (late: ReactNode) => (
      <QueryClientProvider client={client}>
        <Reader
          name="Writer"
          next="board"
          preference="project.issue-view"
          read={(stored) => stored ?? "nothing stored"}
          write={(value) => value}
        />
        {late}
      </QueryClientProvider>
    );
    const view = render(tree(null));
    try {
      await user.click(screen.getByRole("button", { name: "Writer" }));
      expect(shown("Writer")).toBe("board");
      const writer = screen.getByRole("button", { name: "Writer" });

      view.rerender(
        tree(
          <Reader
            name="Late"
            preference="project.issue-view"
            read={(stored) => `its own read of ${stored}`}
            write={(value) => value}
          />
        )
      );
      expect(shown("Late")).toBe("its own read of board");
      // The reader already there keeps the value written, and its mount.
      expect(shown("Writer")).toBe("board");
      expect(screen.getByRole("button", { name: "Writer" })).toBe(writer);
    } finally {
      view.unmount();
      storage.restore();
    }
  });

  test("another tab's storage event moves each reader by its own read", () => {
    const storage = installCountingStorage();
    const key = ALICE_KEYS["project.issue-view"];
    const view = render(
      <QueryClientProvider client={aliceSignedIn()}>
        <Reader
          name="View"
          preference="project.issue-view"
          read={readView}
          write={(value) => value}
        />
        <Reader
          name="Echo"
          preference="project.issue-view"
          read={(stored) => `its own read of ${stored}`}
          write={(value) => value}
        />
        <Reader
          name="Lanes"
          preference="project.board-lanes"
          read={(stored) => stored ?? "nothing stored"}
          write={(value) => value}
        />
      </QueryClientProvider>
    );
    try {
      expect([shown("View"), shown("Echo")]).toEqual(["list", "its own read of null"]);
      // What another tab's write is to this one: the value changes under it, then the browser
      // fires `storage` on this window.
      window.localStorage.setItem(key, "board");
      act(() => {
        window.dispatchEvent(
          new StorageEvent("storage", { key, newValue: "board", oldValue: null })
        );
      });
      expect([shown("View"), shown("Echo")]).toEqual(["board", "its own read of board"]);
      expect(shown("Lanes")).toBe("nothing stored");
    } finally {
      view.unmount();
      storage.restore();
    }
  });

  test("a pending or failed identity writes nothing to storage", async () => {
    const user = userEvent.setup();
    // The unit network answers the sign-in read with Dispatch's own "no identity"
    // (`network.ts`), so this client's identity has failed.
    const failed = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    await failed.prefetchQuery(whoAmIQuery());
    const pending = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    for (const [state, client] of [
      ["pending", pending],
      ["failed", failed],
    ] as const) {
      // For the pending client the sign-in read never answers, so the identity stays pending.
      const held =
        state === "pending"
          ? spyOn(api, "whoAmI").mockReturnValue(Promise.withResolvers<AuthenticatedUser>().promise)
          : undefined;
      const storage = installCountingStorage();
      const view = render(
        <QueryClientProvider client={client}>
          <Reader
            name="Writer"
            next="board"
            preference="project.issue-view"
            read={readView}
            write={(value) => value}
          />
          <Reader
            name="Other"
            preference="project.issue-view"
            read={readView}
            write={(value) => value}
          />
        </QueryClientProvider>
      );
      try {
        await user.click(screen.getByRole("button", { name: "Writer" }));
        // The readers of this tab hold the value written, and the browser keeps nothing of it.
        expect([shown("Writer"), shown("Other")], state).toEqual(["board", "board"]);
        expect(storage.writes, state).toEqual([]);
      } finally {
        view.unmount();
        held?.mockRestore();
        storage.restore();
      }
    }
  });

  test("every preference keeps its key, its stored text and its default", async () => {
    const user = userEvent.setup();
    for (const [preference, key] of Object.entries(ALICE_KEYS) as [UserPreference, string][]) {
      const storage = installCountingStorage();
      const client = aliceSignedIn();
      const reader = (
        <QueryClientProvider client={client}>
          <Reader
            name={preference}
            next={`the text of ${preference}`}
            preference={preference}
            read={(stored) => stored ?? "its default"}
            write={(value) => value}
          />
        </QueryClientProvider>
      );
      const first = render(reader);
      try {
        // Nothing stored: the reader holds what its read gives of nothing.
        expect(shown(preference), preference).toBe("its default");
        await user.click(screen.getByRole("button", { name: preference }));
        // The text its write gives, under the key it has always had.
        expect(storage.writes, preference).toEqual([[key, `the text of ${preference}`]]);
      } finally {
        first.unmount();
      }
      // A reader mounted after the write reads that text back from the same key.
      const again = render(reader);
      try {
        expect(shown(preference), preference).toBe(`the text of ${preference}`);
      } finally {
        again.unmount();
        storage.restore();
      }
    }
  });

  test("a reader's own write never runs its read again", async () => {
    const user = userEvent.setup();
    const storage = installCountingStorage();
    let reads = 0;
    // The Board's filter strip while no filter is on, as `IssueBoard` reads it: shut whatever is
    // stored. Read again after its own write, the strip would shut as soon as it opened.
    const view = render(
      <QueryClientProvider client={aliceSignedIn()}>
        <Reader
          name="Strip"
          next={true}
          preference="project.issue-filters"
          read={() => {
            reads += 1;
            return false;
          }}
          write={(open) => (open ? "expanded" : "collapsed")}
        />
      </QueryClientProvider>
    );
    try {
      expect(shown("Strip")).toBe("false");
      const readsBefore = reads;
      await user.click(screen.getByRole("button", { name: "Strip" }));
      expect(shown("Strip")).toBe("true");
      expect(reads).toBe(readsBefore);
      expect(storage.writes).toEqual([[ALICE_KEYS["project.issue-filters"], "expanded"]]);
    } finally {
      view.unmount();
      storage.restore();
    }
  });
});
