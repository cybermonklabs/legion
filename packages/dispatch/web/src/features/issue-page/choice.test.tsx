import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement, ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { whoAmIQuery } from "../../api/queries";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { useIssuePageChoice } from "./choice";

const ALICE_KEY = userPreferenceStorageKey("alice", "issue.page");

/** One reader of the choice: a button that shows it and turns it to the other page. */
function ChoiceProbe({ name }: { name: string }): ReactNode {
  const [choice, choose] = useIssuePageChoice();
  return (
    <button
      aria-label={name}
      onClick={() => choose(choice === "outline" ? "tabs" : "outline")}
      type="button"
    >
      {choice}
    </button>
  );
}

/** A client that never asks a read again once it holds an answer, and never retries one. */
function newClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** A client whose sign-in read holds alice, as `features/threads/replies-fixture.ts` seeds it,
 *  and never asks again. */
function aliceSignedIn(): QueryClient {
  const client = newClient();
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  return client;
}

function shown(name: string): string | null {
  return screen.getByRole("button", { name }).textContent;
}

/** Draws `ui` under a router at `path`, as every reader of the choice is drawn: the choice reads
 *  the address. */
function drawAt(ui: ReactElement, path = "/"): RenderResult {
  return render(<MemoryRouter initialEntries={[path]}>{ui}</MemoryRouter>);
}

/** Every key this browser holds with its value, so a test can tell that a write stored nothing. */
function storedEntries(): Record<string, string | null> {
  const entries: Record<string, string | null> = {};
  for (let index = 0; index < window.localStorage.length; index += 1) {
    const key = window.localStorage.key(index);
    if (key !== null) entries[key] = window.localStorage.getItem(key);
  }
  return entries;
}

/** What a browser may hold under alice's key, and the page each gives her. */
const STORED = [
  ["a reader who stored nothing has today's page", null, "tabs"],
  ["a reader whose stored word is neither page's has today's page", "Outline", "tabs"],
  ["a reader who stored `outline` has the new page", "outline", "outline"],
] as const;

for (const [title, stored, choice] of STORED) {
  test(title, () => {
    if (stored === null) window.localStorage.removeItem(ALICE_KEY);
    else window.localStorage.setItem(ALICE_KEY, stored);
    const view = drawAt(
      <QueryClientProvider client={aliceSignedIn()}>
        <ChoiceProbe name="Issue page" />
      </QueryClientProvider>
    );
    try {
      expect(shown("Issue page")).toBe(choice);
    } finally {
      view.unmount();
      window.localStorage.removeItem(ALICE_KEY);
    }
  });
}

test("with nobody signed in the choice is today's page, and turning it stores nothing", async () => {
  const user = userEvent.setup();
  const client = newClient();
  // The unit network answers the sign-in read with Dispatch's own "no identity" (`network.ts`).
  await client.prefetchQuery(whoAmIQuery());
  const before = storedEntries();
  const view = drawAt(
    <QueryClientProvider client={client}>
      <ChoiceProbe name="Issue page" />
    </QueryClientProvider>
  );
  try {
    expect(shown("Issue page")).toBe("tabs");
    await user.click(screen.getByRole("button", { name: "Issue page" }));
    expect(shown("Issue page")).toBe("tabs");
    expect(storedEntries()).toEqual(before);
  } finally {
    view.unmount();
  }
});

test("a choice made through one reader is at once the choice of another reader in the same tree", async () => {
  const user = userEvent.setup();
  window.localStorage.removeItem(ALICE_KEY);
  const view = drawAt(
    <QueryClientProvider client={aliceSignedIn()}>
      <ChoiceProbe name="Settings" />
      <ChoiceProbe name="Issue page" />
    </QueryClientProvider>
  );
  try {
    expect(shown("Issue page")).toBe("tabs");
    await user.click(screen.getByRole("button", { name: "Settings" }));
    expect(shown("Issue page")).toBe("outline");
    expect(window.localStorage.getItem(ALICE_KEY)).toBe("outline");

    await user.click(screen.getByRole("button", { name: "Settings" }));
    expect(shown("Issue page")).toBe("tabs");
    expect(window.localStorage.getItem(ALICE_KEY)).toBe("tabs");
  } finally {
    view.unmount();
    window.localStorage.removeItem(ALICE_KEY);
  }
});

test("a choice another tab stores reaches this tab's readers by the window's storage event", () => {
  window.localStorage.removeItem(ALICE_KEY);
  const view = drawAt(
    <QueryClientProvider client={aliceSignedIn()}>
      <ChoiceProbe name="Issue page" />
    </QueryClientProvider>
  );
  try {
    expect(shown("Issue page")).toBe("tabs");
    // What another tab's write is to this one: the value changes under it, then the browser
    // fires `storage` on this window.
    window.localStorage.setItem(ALICE_KEY, "outline");
    act(() => {
      window.dispatchEvent(
        new StorageEvent("storage", {
          key: ALICE_KEY,
          newValue: "outline",
          oldValue: null,
          storageArea: window.localStorage,
        })
      );
    });
    expect(shown("Issue page")).toBe("outline");
  } finally {
    view.unmount();
    window.localStorage.removeItem(ALICE_KEY);
  }
});

/** What a reader whose setting is off may hold under alice's key: nothing, or `tabs`, which
 *  Settings stores when the reader turns the new page off. */
const SETTING_OFF = [
  ["nothing stored", null],
  ["`tabs` stored", "tabs"],
] as const;

/** An address alice reads the choice at, and the page it gives her with the setting off: the
 *  parameter opens the new page with its one value, and at an issue's address alone. */
const ADDRESSED = [
  [
    "an issue's address with ?issue_page=new has the new page",
    "/issues/CORE-1?issue_page=new",
    "outline",
  ],
  ["an issue's address with no parameter has today's page", "/issues/CORE-1", "tabs"],
  [
    "an issue's address with ?issue_page=old has today's page",
    "/issues/CORE-1?issue_page=old",
    "tabs",
  ],
  [
    "an issue's address with an empty ?issue_page= has today's page",
    "/issues/CORE-1?issue_page=",
    "tabs",
  ],
  [
    "an issue's address with ?issue_page=new2 has today's page",
    "/issues/CORE-1?issue_page=new2",
    "tabs",
  ],
  [
    "a page that is no issue's, with ?issue_page=new, has today's page",
    "/inbox?issue_page=new",
    "tabs",
  ],
] as const;

for (const [setting, stored] of SETTING_OFF) {
  for (const [title, path, choice] of ADDRESSED) {
    test(`with ${setting}, ${title}, and reading it stores nothing`, () => {
      if (stored === null) window.localStorage.removeItem(ALICE_KEY);
      else window.localStorage.setItem(ALICE_KEY, stored);
      const before = storedEntries();
      const view = drawAt(
        <QueryClientProvider client={aliceSignedIn()}>
          <ChoiceProbe name="Issue page" />
        </QueryClientProvider>,
        path
      );
      try {
        expect(shown("Issue page")).toBe(choice);
        expect(storedEntries()).toEqual(before);
      } finally {
        view.unmount();
        window.localStorage.removeItem(ALICE_KEY);
      }
    });
  }
}

test("with the setting on, an issue's address with ?issue_page=old still has the new page", () => {
  window.localStorage.setItem(ALICE_KEY, "outline");
  const view = drawAt(
    <QueryClientProvider client={aliceSignedIn()}>
      <ChoiceProbe name="Issue page" />
    </QueryClientProvider>,
    "/issues/CORE-1?issue_page=old"
  );
  try {
    expect(shown("Issue page")).toBe("outline");
  } finally {
    view.unmount();
    window.localStorage.removeItem(ALICE_KEY);
  }
});

test("with nobody signed in, an issue's address with ?issue_page=new has today's page", async () => {
  const client = newClient();
  // The unit network answers the sign-in read with Dispatch's own "no identity" (`network.ts`).
  await client.prefetchQuery(whoAmIQuery());
  const view = drawAt(
    <QueryClientProvider client={client}>
      <ChoiceProbe name="Issue page" />
    </QueryClientProvider>,
    "/issues/CORE-1?issue_page=new"
  );
  try {
    expect(shown("Issue page")).toBe("tabs");
  } finally {
    view.unmount();
  }
});

test("a choice made in Settings stores and reaches every reader as before, and the parameter is never what is stored", async () => {
  const user = userEvent.setup();
  window.localStorage.removeItem(ALICE_KEY);
  const client = aliceSignedIn();
  const before = storedEntries();
  const settings = drawAt(
    <QueryClientProvider client={client}>
      <ChoiceProbe name="Settings" />
    </QueryClientProvider>,
    "/settings"
  );
  const issue = drawAt(
    <QueryClientProvider client={client}>
      <ChoiceProbe name="Issue page" />
    </QueryClientProvider>,
    "/issues/CORE-1?issue_page=new"
  );
  try {
    expect(shown("Issue page")).toBe("outline");
    expect(shown("Settings")).toBe("tabs");
    expect(storedEntries()).toEqual(before);

    await user.click(screen.getByRole("button", { name: "Settings" }));
    expect(window.localStorage.getItem(ALICE_KEY)).toBe("outline");
    expect(shown("Settings")).toBe("outline");
    expect(shown("Issue page")).toBe("outline");

    await user.click(screen.getByRole("button", { name: "Settings" }));
    expect(window.localStorage.getItem(ALICE_KEY)).toBe("tabs");
    expect(shown("Settings")).toBe("tabs");
    expect(shown("Issue page")).toBe("outline");
  } finally {
    issue.unmount();
    settings.unmount();
    window.localStorage.removeItem(ALICE_KEY);
  }
});
