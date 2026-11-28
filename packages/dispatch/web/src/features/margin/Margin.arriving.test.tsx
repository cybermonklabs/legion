/**
 * The margin is `aria-busy` while it arrives on a document (`useMarginSheet`'s `arriving`): until
 * the document's threads are in and its arrival has shown again what it holds there. A busy flag
 * that never clears takes the margin from a screen-reader user for good, so each row here holds
 * one path on which it must clear, or must not be set at all:
 *
 * - the arrival runs, and what it brings back (a thread whose reply is held) is open as it clears;
 * - the document holds no drafts, so the arrival has nothing to bring back;
 * - the document's threads read fails, and the arrival runs over the failure;
 * - the issue read fails, so there is no document to arrive on, and the margin is never busy;
 * - the reader leaves the document before its arrival runs: the flag is the document's, so the
 *   next document's own arrival clears it, and the first document's late threads set nothing;
 * - an arrival on one document does not stand for the next one;
 * - the margin is taken away while busy. "Clears on unmount" means this in the code: the flag is
 *   the `arrivedOn` state of the sheet's own `useMarginSheet` and the `aria-busy` attribute of the
 *   `<aside>` that `MarginSheet` draws, and nothing else holds it (no store, no document-level
 *   attribute, no effect clean-up). Taking the margin away removes both from the page, so nothing
 *   is left busy, and a margin drawn again starts from no arrival and clears at its own;
 * - the arrival throws. The margin is drawn as the app draws it, inside its own error boundary
 *   (`app.tsx`), which takes the whole margin away for its alert, so nothing is left busy: the
 *   reader loses the margin to the throw, which the alert names, not to a flag left set. A wait for
 *   the margin to arrive names that alert too.
 */
import { expect, spyOn, test } from "bun:test";
import { hashKey, type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import {
  BODY,
  marginCard,
  marginClient,
  noteIn,
  threadA,
  threadB,
} from "../../__tests__/draft-matrix-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { ApiError, api } from "../../api/client";
import type { Comment, IssueDetails } from "../../api/types";
import { heldSends } from "../conversation/held-sends";
import { buildIssuePath } from "../refs/routes";
import { ErrorBoundary } from "../shell/ErrorBoundary";
import { Margin } from "./Margin";
import { MarginProvider, marginComposeKeptKey, marginReplySendKey } from "./margin-context";
import { DESKTOP_WIDTH, issue, specArtifact, stubViewport } from "./margin-fixture";

/** Another issue, whose spec is another document: going there leaves CORE-1's spec. */
const secondIssue: IssueDetails = {
  ...issue,
  artifacts: [{ ...specArtifact, id: "artifact-2", issue_key: "CORE-2" }],
  key: "CORE-2",
  number: 2,
  primary_artifact_id: "artifact-2",
  title: "Second issue",
};

/** The margin as `app.tsx` draws it: inside the margin's own error boundary, reset by the path. */
function MarginAsTheAppDrawsIt(): ReactNode {
  const { pathname } = useLocation();
  return (
    <ErrorBoundary region="the margin" resetKey={pathname}>
      <Margin />
    </ErrorBoundary>
  );
}

/** CORE-1's page: links to CORE-2 and back, and the margin, which a press takes away and draws
 *  again while the page and the margin's provider stay. */
function Page(): ReactNode {
  const navigate = useNavigate();
  const [shown, setShown] = useState(true);
  return (
    <>
      <button
        onClick={() => navigate(buildIssuePath({ key: secondIssue.key, kind: "issue" }))}
        type="button"
      >
        Open second issue
      </button>
      <button onClick={() => setShown((current) => !current)} type="button">
        {shown ? "Take the margin away" : "Draw the margin again"}
      </button>
      {shown ? <MarginAsTheAppDrawsIt /> : null}
    </>
  );
}

interface Drawn {
  /** Answers the threads read of issue `key` with `comments`, or refuses it with `error`. */
  readonly answer: (key: string, comments: readonly Comment[]) => Promise<void>;
  readonly refuse: (key: string, error: unknown) => Promise<void>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/** A threads read the row settles: its promise, and the two ends that settle it. */
interface HeldRead {
  readonly promise: Promise<Comment[]>;
  readonly reject: (error: unknown) => void;
  readonly resolve: (comments: Comment[]) => void;
}

/**
 * CORE-1's page at a desktop width over `queryClient`, whose threads reads (`listComments`) wait
 * until the row answers or refuses them, one read per issue; the issue reads answer CORE-1 and
 * CORE-2 as the cache was seeded, or refuse with `issueRead`.
 */
function drawPage(queryClient: QueryClient, issueRead?: unknown): Drawn {
  const reads = new Map<string, HeldRead>();
  const read = (key: string) => {
    const known = reads.get(key);
    if (known !== undefined) return known;
    const made = Promise.withResolvers<Comment[]>();
    reads.set(key, made);
    return made;
  };
  const listComments = spyOn(api, "listComments").mockImplementation(
    (key: string) => read(key).promise
  );
  const getIssue = spyOn(api, "getIssue").mockImplementation(async (key: string) => {
    if (issueRead !== undefined) throw issueRead;
    return key === secondIssue.key ? secondIssue : issue;
  });
  const restoreViewport = stubViewport(DESKTOP_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Page />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    answer: async (key, comments) => {
      await act(async () => read(key).resolve([...comments]));
    },
    refuse: async (key, error) => {
      await act(async () => read(key).reject(error));
    },
    restore: () => {
      view.unmount();
      restoreViewport();
      listComments.mockRestore();
      getIssue.mockRestore();
    },
    user: userEvent.setup(),
  };
}

/** The reader's query client for CORE-1 and CORE-2 with no threads read yet. */
function threadsUnread(): QueryClient {
  const queryClient = marginClient([]);
  queryClient.removeQueries({ queryKey: ["comments", issue.key] });
  queryClient.setQueryData(["issue", secondIssue.key], secondIssue);
  queryClient.setQueryData(["asks", secondIssue.key], []);
  return queryClient;
}

/** The margin's `aria-busy` now, or `"no margin"` where none is on screen. */
function busy(): string | null {
  const sheet = screen.queryByTestId("margin-sheet");
  return sheet === null ? "no margin" : sheet.getAttribute("aria-busy");
}

/**
 * Waits until the margin is on screen and no longer `aria-busy`, through setup.ts's budget: a
 * margin still busy at the row's deadline fails naming its `aria-busy`, and one the error boundary
 * took away fails naming the boundary's alert.
 */
async function marginArrives(): Promise<void> {
  await waitFor(() => {
    const sheet = screen.queryByTestId("margin-sheet");
    if (sheet === null) {
      const alert = screen.queryByRole("alert")?.textContent ?? "nothing in its place";
      throw new Error(`waited for the margin to arrive, and no margin is on screen: ${alert}`);
    }
    expect(
      sheet.getAttribute("aria-busy"),
      "the margin's aria-busy, which its arrival on the document clears"
    ).toBeNull();
  });
}

test("the margin is aria-busy until its arrival has run, and the thread it opens is open as it clears", async () => {
  const queryClient = threadsUnread();
  const createComment = spyOn(api, "createComment");
  await stopSend(
    queryClient,
    marginReplySendKey(threadA.id),
    sentRequest(BODY, {
      actor: threadA.author,
      excerpt: "",
      id: threadA.id,
      parentKind: "comment",
    }),
    [createComment]
  );
  const page = drawPage(queryClient);
  try {
    expect(busy()).toBe("true");
    await page.answer(issue.key, [threadA, threadB]);
    await marginArrives();
    expect({
      expanded: marginCard(threadA.id).getAttribute("aria-expanded"),
      note: noteIn(marginCard(threadA.id)),
    }).toEqual({ expanded: "true", note: "stopped" });
  } finally {
    page.restore();
    createComment.mockRestore();
  }
});

test("the margin clears its aria-busy on a document that holds no drafts", async () => {
  const page = drawPage(threadsUnread());
  try {
    expect(busy()).toBe("true");
    await page.answer(issue.key, []);
    await marginArrives();
    expect({
      composes: screen.queryAllByRole("textbox", { name: "Comment" }).length,
      empty: screen.queryByText("No comments, asks, or suggestions on this document.") !== null,
    }).toEqual({ composes: 0, empty: true });
  } finally {
    page.restore();
  }
});

test("the margin clears its aria-busy when the document's threads read fails", async () => {
  const page = drawPage(threadsUnread());
  try {
    expect(busy()).toBe("true");
    await page.refuse(issue.key, new ApiError(503, { code: "UNAVAILABLE", error: "down" }));
    await marginArrives();
    expect(screen.queryByText("Could not load this document's comments.")).not.toBeNull();
  } finally {
    page.restore();
  }
});

test("the margin is never aria-busy when the issue read fails, since it has no document to arrive on", async () => {
  const queryClient = threadsUnread();
  queryClient.removeQueries({ queryKey: ["issue", issue.key] });
  const page = drawPage(queryClient, new ApiError(503, { code: "UNAVAILABLE", error: "down" }));
  try {
    await screen.findByText("Could not load this issue's margin.");
    expect(busy()).toBe(null);
  } finally {
    page.restore();
  }
});

test("a document left before its arrival leaves no aria-busy on the next, once that one arrives", async () => {
  const page = drawPage(threadsUnread());
  try {
    expect(busy()).toBe("true");
    await page.user.click(screen.getByRole("button", { name: "Open second issue" }));
    expect(busy()).toBe("true");
    await page.answer(secondIssue.key, []);
    await marginArrives();
    // CORE-1's threads, answered once the reader has left it, are no arrival of CORE-2's.
    await page.answer(issue.key, [threadA]);
    expect(busy()).toBe(null);
  } finally {
    page.restore();
  }
});

test("an arrival on one document does not clear the aria-busy of the next", async () => {
  const page = drawPage(threadsUnread());
  try {
    await page.answer(issue.key, [threadA]);
    await marginArrives();
    await page.user.click(screen.getByRole("button", { name: "Open second issue" }));
    expect(busy()).toBe("true");
    await page.answer(secondIssue.key, []);
    await marginArrives();
  } finally {
    page.restore();
  }
});

test("a margin taken away while aria-busy leaves nothing busy, and drawn again it clears at its own arrival", async () => {
  const page = drawPage(threadsUnread());
  try {
    expect(busy()).toBe("true");
    await page.user.click(screen.getByRole("button", { name: "Take the margin away" }));
    expect(document.querySelectorAll("[aria-busy]").length).toBe(0);
    // The threads the taken margin asked for answer with no margin to arrive.
    await page.answer(issue.key, [threadA]);
    expect({
      busy: document.querySelectorAll("[aria-busy]").length,
      margin: busy(),
    }).toEqual({ busy: 0, margin: "no margin" });
    await page.user.click(screen.getByRole("button", { name: "Draw the margin again" }));
    await marginArrives();
    expect(marginCard(threadA.id)).toBeInTheDocument();
  } finally {
    page.restore();
  }
});

test("an arrival that throws leaves nothing aria-busy: the margin's error boundary shows its alert in its place", async () => {
  const queryClient = threadsUnread();
  const store = heldSends(queryClient);
  const read = store.get.bind(store);
  // The arrival's first read of the held-send store, the stopped send set aside for the
  // document, throws, as a store that cannot read what it keeps would.
  const keptKey = hashKey(marginComposeKeptKey(specArtifact.id));
  const get = spyOn(store, "get").mockImplementation((key) => {
    if (hashKey(key) === keptKey) throw new Error("the held sends could not be read");
    return read(key);
  });
  const page = drawPage(queryClient);
  try {
    expect(busy()).toBe("true");
    await page.answer(issue.key, [threadA]);
    const alert = await screen.findByRole("alert");
    expect({
      alert: [
        alert.textContent?.includes("Something went wrong in the margin.") ?? false,
        alert.textContent?.includes("the held sends could not be read") ?? false,
      ],
      busy: document.querySelectorAll("[aria-busy]").length,
      margin: busy(),
    }).toEqual({ alert: [true, true], busy: 0, margin: "no margin" });
  } finally {
    page.restore();
    get.mockRestore();
  }
});
