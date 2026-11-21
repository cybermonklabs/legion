// What Shift+M runs in the app (`app.tsx`, the shell's `useKeymap("global", …)`). In the new
// issue page's frame it is the frame's own key, which opens and closes Details, and Toggle
// margin is not bound at all, so a key a person gave Toggle margin does nothing there. On today's
// issue page it is Toggle margin, the catalogue's action, whose key a person can change
// (`features/shell/keymap-keys.ts`).
import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { details as issueDetails } from "./__tests__/issue-picks";
import { api, NetworkError } from "./api/client";
import { liveSocketFor } from "./api/live-socket";
import { AuthGate } from "./app";
import type { IssuePageChoice } from "./features/issue-page/choice";
import { setActionKeys, storedPersonKeys } from "./features/shell/keymap-keys";
import { MD_VIEWPORT_QUERY } from "./features/shell/useDialog";
import { userPreferenceStorageKey } from "./features/shell/userPreference";
// The issue page, both of its pages and the margin are lazy parts of the app (`app.tsx`,
// `features/issue-page/IssuePage.tsx`, `MarginColumn.tsx`). A load a row starts and ends before
// lands in a later row as React's report of a suspended resource outside `act`, so the modules
// are loaded here, with the test file.
import "./features/issue-page/IssuePage";
import "./features/issue-page/IssueOutlinePage";
import "./features/margin/Margin";

const PAGE_CHOICE_KEY = userPreferenceStorageKey("alice", "issue.page");
const MARGIN_KEY = userPreferenceStorageKey("alice", "shell.margin");
const PERSON_KEYS_KEY = userPreferenceStorageKey("alice", "shell.keymap");

/** The key alice gives Toggle margin in place of `Shift+M`, pressed below as
 *  `{Shift>}X{/Shift}`: no action of the catalogue and no binding of the app holds it. */
const MARGIN_KEY_OF_ALICE = "Shift+X";

const originalMatchMedia = window.matchMedia;
let teardown: (() => void) | undefined;

afterEach(() => {
  teardown?.();
  teardown = undefined;
  window.matchMedia = originalMatchMedia;
  window.localStorage.removeItem(PAGE_CHOICE_KEY);
  window.localStorage.removeItem(MARGIN_KEY);
  window.localStorage.removeItem(PERSON_KEYS_KEY);
});

/** The app at CORE-1's address, signed in as alice on the page `page` she chose, in a window
 *  1280 px wide: past `md`, where the frame draws its icon rail, and past `xl`, where today's
 *  margin is a column whose preference Toggle margin flips. Returns once the page has named the
 *  loaded issue in the tab's title, as both pages do (`useDocumentTitle`), and the shell has
 *  drawn its margin. */
async function renderIssue(page: IssuePageChoice): Promise<void> {
  window.localStorage.setItem(PAGE_CHOICE_KEY, page);
  // Nothing puts the title back when a page goes, so the row before may have left this one's.
  document.title = "";
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query === MD_VIEWPORT_QUERY,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  // The page and its margin read more of CORE-1 than the issue itself. No row here is about
  // those reads, so they are answered at the client with a dropped connection's failure rather
  // than reaching the network.
  const unread = new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"));
  const spies = [
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "getIssue").mockResolvedValue(issueDetails),
    spyOn(api, "listComments").mockRejectedValue(unread),
    spyOn(api, "listIssueAsks").mockRejectedValue(unread),
    spyOn(api, "listIssueMessages").mockRejectedValue(unread),
    spyOn(api, "getIssueCoverage").mockRejectedValue(unread),
    spyOn(api, "getIssueDecisions").mockRejectedValue(unread),
    spyOn(api, "getIssueResults").mockRejectedValue(unread),
    spyOn(api, "getReferenceTargets").mockRejectedValue(unread),
  ];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  teardown = () => {
    view.unmount();
    liveSocketFor(queryClient).close();
    for (const spy of spies) spy.mockRestore();
  };
  await waitFor(() => expect(document.title).toBe("CORE-1 · Review the spec · Dispatch"));
  await screen.findByTestId("desktop-margin-shell");
}

/** The shell's margin: Details in the frame, the column it always was on today's page. */
const marginShell = () => screen.getByTestId("desktop-margin-shell");

/** The margin preference as alice's browser keeps it; `null` while nothing has written it. */
const storedMargin = () => window.localStorage.getItem(MARGIN_KEY);

test("in the new issue page's frame one Shift+M opens Details and the next closes it, and neither touches the margin preference", async () => {
  const user = userEvent.setup();
  await renderIssue("outline");
  expect(marginShell()).toHaveAttribute("hidden");

  await user.keyboard("{Shift>}M{/Shift}");
  expect(marginShell()).not.toHaveAttribute("hidden");
  expect(storedMargin()).toBeNull();

  await user.keyboard("{Shift>}M{/Shift}");
  expect(marginShell()).toHaveAttribute("hidden");
  expect(storedMargin()).toBeNull();
});

test("in the frame the key alice gave Toggle margin changes neither Details nor the margin preference, and Shift+M still opens Details", async () => {
  const user = userEvent.setup();
  setActionKeys("alice", "global.toggle-margin", [MARGIN_KEY_OF_ALICE]);
  // The refusal check keeps the key: it is what Toggle margin runs on for alice.
  expect(storedPersonKeys("alice")).toEqual({ "global.toggle-margin": [MARGIN_KEY_OF_ALICE] });
  await renderIssue("outline");

  await user.keyboard("{Shift>}X{/Shift}");
  expect(marginShell()).toHaveAttribute("hidden");
  expect(storedMargin()).toBeNull();

  await user.keyboard("{Shift>}M{/Shift}");
  expect(marginShell()).not.toHaveAttribute("hidden");
  expect(storedMargin()).toBeNull();

  // Nor does it with Details open.
  await user.keyboard("{Shift>}X{/Shift}");
  expect(marginShell()).not.toHaveAttribute("hidden");
  expect(storedMargin()).toBeNull();
});

test("on today's issue page the key alice gave Toggle margin flips the margin preference, and Shift+M does nothing", async () => {
  const user = userEvent.setup();
  setActionKeys("alice", "global.toggle-margin", [MARGIN_KEY_OF_ALICE]);
  await renderIssue("tabs");

  await user.keyboard("{Shift>}M{/Shift}");
  expect(storedMargin()).toBeNull();

  await user.keyboard("{Shift>}X{/Shift}");
  expect(storedMargin()).toBe("hidden");

  await user.keyboard("{Shift>}M{/Shift}");
  expect(storedMargin()).toBe("hidden");

  await user.keyboard("{Shift>}X{/Shift}");
  expect(storedMargin()).toBe("shown");
});
