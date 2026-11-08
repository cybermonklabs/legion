import { expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type {
  ArtifactUploadResponse,
  Issue,
  Message,
  SearchResponse,
  SearchResultsPage,
} from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { firstCriterion } from "./new-issue";
import { SearchPalette } from "./SearchPalette";

const words = "the sextant rota is nowhere written down";

/** No hit at all, which is what makes the bar offer the words as a new issue. */
function noHits(): SearchResponse & SearchResultsPage {
  return { limit: 20, offset: 0, reachable: 0, results: [], took_ms: 1, total: 0 };
}

/** The search catching up with a press: it now matches the issue that press just made, which
 *  is what the pause's own search does when it lands after the create. */
function theMadeIssue(): SearchResponse & SearchResultsPage {
  return {
    limit: 20,
    offset: 0,
    reachable: 1,
    results: [
      {
        href: "/issues/CORE-7",
        id: "CORE-7",
        kind: "issue",
        owner: { key: "CORE-7", kind: "issue", status: "todo", title: words },
        rank: 1,
        snippet: words,
      },
    ],
    took_ms: 1,
    total: 1,
  };
}

function issue(key: string): Issue {
  return {
    closed_at: null,
    created_at: "2026-11-06T00:00:00Z",
    external_links: [],
    key,
    labels: [],
    pinned: false,
    priority: null,
    project: "CORE",
    rank: "a",
    route: null,
    status: "triage",
    title: words,
    updated_at: "2026-11-06T00:00:00Z",
  } as unknown as Issue;
}

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return <output data-testid="current-route">{location.pathname}</output>;
}

interface Stubs {
  readonly createIssue: Mock<typeof api.createIssue>;
  readonly createMessage: Mock<typeof api.createMessage>;
  readonly patchIssue: Mock<typeof api.patchIssue>;
  readonly queryClient: QueryClient;
  readonly restore: () => void;
  readonly search: Mock<typeof api.search>;
  readonly uploadArtifact: Mock<typeof api.uploadArtifact>;
}

/** The bar over a project page, its search answering nothing, with the deployment's two
 *  settings as given and every write it could make stubbed. */
function openBar(settings: { label: string; session: string }): Stubs {
  const search = spyOn(api, "search").mockResolvedValue(noHits());
  const newIssueSettings = spyOn(api, "getNewIssueSettings").mockResolvedValue(settings);
  const createIssue = spyOn(api, "createIssue").mockResolvedValue(issue("CORE-7"));
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(
    {} as ArtifactUploadResponse
  );
  const patchIssue = spyOn(api, "patchIssue").mockResolvedValue(issue("CORE-7"));
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as Message);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/projects/CORE/issues"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <CurrentRoute />
          <SearchPalette onClose={() => {}} open />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    createIssue,
    createMessage,
    patchIssue,
    queryClient,
    restore: () => {
      for (const stub of [
        search,
        newIssueSettings,
        createIssue,
        uploadArtifact,
        patchIssue,
        createMessage,
      ]) {
        stub.mockRestore();
      }
      view.unmount();
      queryClient.clear();
    },
    search,
    uploadArtifact,
  };
}

/** Puts the words in the bar's box and waits for the offer the bar makes of them. */
async function offerFor(user: UserEvent): Promise<HTMLElement> {
  await user.click(screen.getByRole("combobox", { name: "Search" }));
  await user.paste(words);
  return await waitFor(() => screen.getByText(`Make a new issue from “${words}”`));
}

test("offers words that matched nothing as a new issue and writes nothing until it is pressed", async () => {
  const user = userEvent.setup();
  const bar = openBar({ label: "from-the-bar", session: "sess-1" });
  try {
    await offerFor(user);
    expect(bar.createIssue).not.toHaveBeenCalled();
  } finally {
    bar.restore();
  }
});

test("accepting writes the words as the issue's first criterion, labels it, routes it and opens it", async () => {
  const user = userEvent.setup();
  const bar = openBar({ label: "from-the-bar", session: "sess-1" });
  try {
    await user.click(await offerFor(user));

    await waitFor(() => expect(bar.createMessage).toHaveBeenCalled());
    expect(bar.createIssue.mock.calls[0]?.[0]).toEqual({
      labels: ["from-the-bar"],
      project: "CORE",
      title: words,
    });
    expect(bar.uploadArtifact.mock.calls[0]?.[0]).toEqual({ issue: "CORE-7" });
    expect(bar.uploadArtifact.mock.calls[0]?.[1]).toEqual({
      content: `:::check{}\n${words}\n:::\n`,
      name: "verification.md",
    });
    expect(bar.patchIssue.mock.calls[0]?.[1]).toEqual({ route: "session:sess-1" });
    expect(bar.createMessage.mock.calls[0]?.[1]).toEqual({
      body: words,
      delivery: "aside",
      target: "session:sess-1",
    });
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-7")
    );
  } finally {
    bar.restore();
  }
});

test("with no session configured it makes the issue and says no conversation was started", async () => {
  const user = userEvent.setup();
  const bar = openBar({ label: "from-the-bar", session: "" });
  try {
    await user.click(await offerFor(user));

    await waitFor(() =>
      expect(screen.getByTestId("search-new-issue-said").textContent).toContain(
        "No conversation was started"
      )
    );
    expect(screen.getByTestId("search-new-issue-said").textContent).toContain("CORE-7 is made");
    expect(bar.patchIssue).not.toHaveBeenCalled();
    expect(bar.createMessage).not.toHaveBeenCalled();
    // The issue is made and nothing is running on it, so the bar leaves it to the person to
    // open, on the row that made it.
    expect(screen.getByTestId("current-route").textContent).toBe("/projects/CORE/issues");
    await user.click(screen.getByText("Open CORE-7"));
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-7")
    );
  } finally {
    bar.restore();
  }
});

// The search behind the bar re-answers after the create and matches the issue the press just
// made, so the words the person typed no longer match nothing. The row goes, because there is
// no second issue to make; the line stays, because it says what their press did.
test("the search catching up withdraws the row and leaves the line about the press", async () => {
  const user = userEvent.setup();
  const bar = openBar({ label: "from-the-bar", session: "" });
  try {
    await user.click(await offerFor(user));
    await waitFor(() => expect(screen.getByText("Open CORE-7")).toBeDefined());

    bar.search.mockResolvedValue(theMadeIssue());
    await bar.queryClient.refetchQueries();

    await waitFor(() => expect(screen.queryByText("Open CORE-7")).toBeNull());
    expect(screen.getByTestId("search-new-issue-said").textContent).toContain(
      "CORE-7 is made. No conversation was started"
    );
  } finally {
    bar.restore();
  }
});

test("the criterion is the person's words in a check block carrying no attributes", () => {
  expect(firstCriterion("a crew list that reads back")).toBe(
    ":::check{}\na crew list that reads back\n:::\n"
  );
});

// The create lands and a write onto the issue is refused. The issue exists, so the bar reports
// it as made and says which write was refused against its key, and the row it leaves is the
// way to open it: pressing again would make a second issue, which is the husk R14
// (dispatch://INFRA-1042/spec#b-r14) exists to stop.
test("a write refused after the create reports the issue as made and offers to create no more", async () => {
  const user = userEvent.setup();
  const bar = openBar({ label: "from-the-bar", session: "sess-1" });
  try {
    bar.uploadArtifact.mockRejectedValue(
      new ApiError(400, { code: "ARTIFACT_INPUT", error: "the document was refused" })
    );
    await user.click(await offerFor(user));

    await waitFor(() =>
      expect(screen.getByTestId("search-new-issue-said").textContent).toBe(
        "CORE-7 is made; its first criterion was refused: the document was refused"
      )
    );
    // The first refusal ends the filing: nothing was written onto the issue after it, and the
    // bar stayed where the person can read what happened.
    expect(bar.patchIssue).not.toHaveBeenCalled();
    expect(bar.createMessage).not.toHaveBeenCalled();
    expect(screen.getByTestId("current-route").textContent).toBe("/projects/CORE/issues");

    expect(screen.queryByText(`Make a new issue from “${words}”`)).toBeNull();
    await user.click(screen.getByText("Open CORE-7"));
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-7")
    );
    expect(bar.createIssue).toHaveBeenCalledTimes(1);
  } finally {
    bar.restore();
  }
});
