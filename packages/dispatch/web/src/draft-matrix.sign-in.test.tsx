/**
 * The draft matrix, sign-in (INFRA-1503).
 *
 * What the reader typed and has not sent is theirs alone, and a field on screen holds it in the
 * shell itself (packages/dispatch/AGENTS.md, "A write belongs to the person who pressed it"). So
 * the shell is drawn for the person the session cookie names, and drawn again only when another
 * person is signed in. Each cell has words typed into a composer on screen (an Agents row's),
 * makes the page read who is signed in again, and asserts whose shell is drawn and where the
 * words end.
 *
 * | Read again for                                             | typed    |
 * | ---------------------------------------------------------- | -------- |
 * | Another person's sign-in, the first check still out        | cell (1) |
 * | The same person's sign-in from another tab                 | cell     |
 * | Another person's sign-in from another tab                  | (2)      |
 * | A reload, the composer's reads cached as the shell draws   | cell     |
 *
 * The last row is the shell's first sign-in after a reload. The tab keeps what the reader typed
 * (`draft-store.ts`), and the draft stores take it up when `AuthGate` tells them who is signed
 * in, so the shell is drawn only in the commit after that, the first sign-in's too: a composer
 * whose reads the query client already holds mounts in the shell's first commit and opens on the
 * kept draft (the docked composer, on CORE-1's issue page).
 *
 * 1. Run under each client: TanStack's own, which the other app-shell rows use, and
 *    `DispatchQueryClient`, built as `main.tsx` builds it. While the check holds no answer the
 *    first joins a check still out, and the second waits for it and asks again once it lands, so
 *    a page that only invalidates draws alice under the first, and alice and then bob under the
 *    second.
 * 2. `app-sign-out.test.tsx`, "a composer's typed text is not shown to another person signed in
 *    from another tab", is this cell, and it is not repeated here.
 *
 * The composer census finds `AuthGate`'s own conditionals above every host the app draws
 * (`app.tsx`): which person's shell is drawn (`shellFor`, once the stores were told), the rows
 * above; the check not yet answered (`whoAmI.isPending`), before which no shell, and so no
 * composer, is drawn, which the first row's check still out makes; and:
 * 3. Signed out (`signedOut`), the page takes the shell and every composer away, which
 *    `app-sign-out.test.tsx` holds to the same rule as (2).
 *
 * The forms the composer census found, where the layout draws the page otherwise, which the rows
 * above do not make:
 * 4. In `AppShell`, above every host, the layout draws the shell around the page: the compact
 *    header with its Menu and the Inbox's Peek (whose drawer is the Inbox files' reason), the
 *    navigation drawer and the focus it keeps (`useDialog`), the sidebar or its rail, the main
 *    column's classes, and the keyboard fit of a page that fills the screen (`useKeyboardFit`).
 *    Each draws or moves something beside the route element and the margin a host is in, never
 *    them, so no composer moves, and the rows above make their moves at the test page's width.
 *
 * `draft-matrix.sign-in.coverage.ts` holds each census item this file answers for to a test
 * above or to a numbered reason, and is the source this header is the prose of.
 */
import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";

import { commentA, commentB, endAsAReload, planner } from "./__tests__/draft-matrix-fixture";
import { realClock, settle, tick } from "./__tests__/fake-clock";
import { answerRequests } from "./__tests__/network";
import { answerAgentStates } from "./__tests__/send-answers";
import { userOnFakeClock } from "./__tests__/user";
import { api } from "./api/client";
import { DispatchQueryClient, queryRetry, queryRetryDelay } from "./api/query-client";
import { renderApp } from "./app-sign-out-fixture";
import { issue } from "./features/margin/margin-fixture";
import { buildIssuePath } from "./features/refs/routes";

/** When the page's first sign-in check answers, on the held clock. It is later than every wait
 *  before bob types moves that clock (a `findBy` moves it 50 ms a step), and within the 10 s a
 *  wait gives up after, so a page that waits for this answer draws its shell inside that wait. */
const OLDER_ANSWER_MS = 5_000;

/** The person this page's whoami names while it answers one. */
type SignedInUser = { event_head: string; kind: "user"; login: string };

/** What `/auth/whoami` answers for `login`. */
const named = (login: string): SignedInUser => ({ event_head: "0", kind: "user", login });

/** The reads of the Agents page, answered with the Planner's row, and the row's own state write.
 *  Returns what puts the reads back. */
function answerAgentReads(): () => void {
  const reads = [
    spyOn(api, "listAgents").mockResolvedValue([planner]),
    spyOn(api, "listAgentMessages").mockResolvedValue([]),
    spyOn(api, "getMyAgentState").mockResolvedValue({}),
  ];
  answerAgentStates().mockImplementation(async () => ({ unread_replies: 0 }));
  return () => {
    for (const read of reads) read.mockRestore();
  };
}

/** Opens the Planner row's composer and types `words` into it, returning the row and its field. */
async function typeIntoPlanner(user: UserEvent, words: string) {
  const region = await screen.findByRole("region", { name: "Agents" });
  const row = (await within(region).findByRole("heading", { name: "Planner" })).closest("article");
  if (row === null) throw new Error("the Planner's row is missing");
  await user.click(within(row).getByRole("button", { name: "Planner" }));
  const field = await within(row).findByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
  await user.click(field);
  await user.paste(words);
  expect(field.value).toBe(words);
  return { field, row };
}

/** Every person the shell is drawn for, in order, as its rail names them (`Signed in as`). The
 *  page is read each time its DOM changes, so a shell drawn and replaced between two steps of a
 *  test is named too. `names` gives them space-separated. */
function shellsDrawn() {
  const logins: string[] = [];
  const read = () => {
    for (const line of screen.queryAllByText(/^Signed in as /)) {
      const login = (line.textContent ?? "").replace(/^Signed in as /, "");
      if (logins.at(-1) !== login) logins.push(login);
    }
  };
  const observer = new MutationObserver(read);
  observer.observe(document.body, { characterData: true, childList: true, subtree: true });
  return {
    names: () => {
      read();
      return logins.join(" ");
    },
    stop: () => observer.disconnect(),
  };
}

/** The sign-in check stays fresh for 30 s, as `main.tsx` holds every read, so no part of the shell
 *  mounting asks it again: every check `queryClient` sends is one a cell made the page send. */
function freshFor30s(queryClient: QueryClient): void {
  queryClient.setQueryDefaults(["whoami"], { staleTime: 30_000 });
}

/** TanStack's own client, as `renderApp` builds it, with the sign-in check fresh for 30 s. */
function tanstackClient(): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  freshFor30s(client);
  return client;
}

/** Dispatch's client, built as `main.tsx` builds it. */
function dispatchClient(): QueryClient {
  return new DispatchQueryClient({
    defaultOptions: {
      queries: {
        refetchOnWindowFocus: true,
        retry: queryRetry,
        retryDelay: queryRetryDelay,
        staleTime: 30_000,
      },
    },
  });
}

// The page's first sign-in check is still out when bob signs in from another tab. That check was
// sent while alice was signed in, and its answer, naming her, comes after the one the news starts,
// which names bob at once. The shell is drawn for bob alone, and the older answer, landing once
// bob has typed, changes nothing on screen: alice's shell, and whatever she kept, is never shown
// under bob's cookie.
async function olderCheckAnswersLast(queryClient: QueryClient): Promise<void> {
  // On the held clock from the first render, so the older answer lands where this test moves it.
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const restoreReads = answerAgentReads();
  const shells = shellsDrawn();
  let asked = 0;
  const older = Promise.withResolvers<SignedInUser>();
  let olderLanded = false;
  setTimeout(() => {
    olderLanded = true;
    older.resolve(named("alice"));
  }, OLDER_ANSWER_MS);
  const app = renderApp({
    at: "/agents",
    queryClient,
    whoAmI: () => {
      asked += 1;
      return asked === 1 ? older.promise : Promise.resolve(named("bob"));
    },
  });
  const tab = new BroadcastChannel("dispatch-sign-in");
  // A tab that only listens. Every tab hears a message in the order the tabs opened, and this
  // page's channel opened with the app, so once this one has heard the sign-in the page has too.
  const listener = new BroadcastChannel("dispatch-sign-in");
  const heard = Promise.withResolvers<void>();
  listener.addEventListener("message", (event) => {
    if (event.data === "bob") heard.resolve();
  });
  const typed = "Bob's words for the planner";
  try {
    act(() => tab.postMessage("bob"));
    await act(() => heard.promise);
    // The page settles on whoever its check names. A page that waits on the older check draws
    // alice here, and under Dispatch's client asks again once that check lands and draws bob
    // after her, which the settle below lets run before the shell is read.
    await screen.findByText(/^Signed in as /);
    await act(settle);
    expect(shells.names()).toBe("bob");
    expect(asked).toBe(2);

    const { field, row } = await typeIntoPlanner(user, typed);
    const signedInLine = screen.getByText("Signed in as bob");
    expect(olderLanded).toBe(false);
    await tick(OLDER_ANSWER_MS);
    expect(olderLanded).toBe(true);
    expect(within(row).getByRole("textbox", { name: "Comment" })).toBe(field);
    expect(field.value).toBe(typed);
    expect(screen.getByText("Signed in as bob")).toBe(signedInLine);
    expect(shells.names()).toBe("bob");
  } finally {
    realClock();
    shells.stop();
    tab.close();
    listener.close();
    app.teardown();
    restoreReads();
  }
}

test("an older check answering after another person's sign-in draws no shell for the person before (QueryClient)", () =>
  olderCheckAnswersLast(tanstackClient()));

test("an older check answering after another person's sign-in draws no shell for the person before (DispatchQueryClient)", () =>
  olderCheckAnswersLast(dispatchClient()));

// Alice signs in from another tab of hers while her words are in a composer here. The page reads
// who is signed in again, and the answer, alice still, draws nothing anew: the field she typed
// into stays on screen, the same field, with her words in it.
test("the same person's sign-in from another tab keeps the shell and the words typed in it", async () => {
  const user = userEvent.setup();
  const restoreReads = answerAgentReads();
  const shells = shellsDrawn();
  let asked = 0;
  const app = renderApp({
    at: "/agents",
    whoAmI: async () => {
      asked += 1;
      return named("alice");
    },
  });
  freshFor30s(app.queryClient);
  const tab = new BroadcastChannel("dispatch-sign-in");
  const typed = "Alice's words for the planner";
  try {
    await screen.findByText("Signed in as alice");
    const { field, row } = await typeIntoPlanner(user, typed);
    const signedInLine = screen.getByText("Signed in as alice");
    expect(asked).toBe(1);

    act(() => tab.postMessage("alice"));
    await waitFor(() => expect(asked).toBe(2));
    await waitFor(() => expect(app.queryClient.isFetching({ queryKey: ["whoami"] })).toBe(0));
    expect(within(row).getByRole("textbox", { name: "Comment" })).toBe(field);
    expect(field.value).toBe(typed);
    expect(screen.getByText("Signed in as alice")).toBe(signedInLine);
    expect(shells.names()).toBe("alice");
  } finally {
    shells.stop();
    tab.close();
    app.teardown();
    restoreReads();
  }
});

/** The reads of CORE-1's issue page over Comment A and Comment B, each answered as an issue with
 *  nothing else on it answers. Returns what puts the reads back. */
function answerIssueReads(): () => void {
  const state = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "getMyState").mockResolvedValue({ "CORE-1": state }),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([commentA, commentB]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listHandoffs").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
  ];
  answerRequests(
    http.get("/api/v1/settings/repo-projects", () => Response.json([])),
    http.get("/api/v1/projects/:project/architecture-sources", () => Response.json([]))
  );
  return () => {
    for (const read of reads) read.mockRestore();
  };
}

/** A query client that holds every read `before` holds, as a page whose reads are answered
 *  before its shell is drawn would: each composer can draw in the shell's first commit. */
function holdingTheReadsOf(before: QueryClient): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  for (const { queryKey, state } of before.getQueryCache().getAll()) {
    if (state.data !== undefined) client.setQueryData(queryKey, state.data);
  }
  return client;
}

// The tab keeps what the reader typed for a reload (`draft-store.ts`), and the page that loads
// takes it up as the reader's sign-in is known (`AuthGate`). A composer draws its field from the
// store as it mounts, so the shell is drawn only once the stores have been told who is signed in,
// the first sign-in's too. Here the reads the issue page makes are all in the query client as the
// page after the reload draws its shell, so the docked composer mounts in the shell's first
// commit, and opens on the comment the reader typed before the reload.
test("after a reload, a composer whose reads the query client holds as the shell is drawn opens on the draft the tab kept", async () => {
  const user = userEvent.setup();
  const restoreReads = answerIssueReads();
  const at = buildIssuePath({ key: issue.key, kind: "conversation" });
  const whoAmI = async () => named("alice");
  let app = renderApp({ at, whoAmI });
  const typed = "Alice's comment, typed before the reload";
  try {
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste(typed);
    const before = app.queryClient;
    endAsAReload(() => app.teardown());
    app = renderApp({ at, queryClient: holdingTheReadsOf(before), whoAmI });
    const back = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(back).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value).toBe(
      typed
    );
  } finally {
    app.teardown();
    restoreReads();
  }
});
