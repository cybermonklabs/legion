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
 * | A lapse, then the same person's sign-in                    | cell     |
 * | A sign-out, then the same person's or another's sign-in    | cell     |
 * | A sign-out in another tab, then either person's sign-in    | cell     |
 *
 * The reload row is the shell's first sign-in after a reload. The tab keeps what the reader typed
 * (`draft-store.ts`), and the draft stores take it up when `AuthGate` tells them who is signed
 * in, so the shell is drawn only in the commit after that, the first sign-in's too: a composer
 * whose reads the query client already holds mounts in the shell's first commit and opens on the
 * kept draft (the docked composer, on CORE-1's issue page).
 *
 * A draft the tab kept that this release cannot read is no draft: one an older release kept in
 * another shape, or text that does not parse. The page draws on, and the composer that would have
 * opened on it opens empty. Each of the composers' two stores (`unsent-drafts.ts`) is held to it
 * on a composer that reads it as it mounts: the drafts kept by address on the docked composer,
 * and a thread's reply on Comment A's card, which says nothing is kept while it is closed.
 *
 * A lapse takes the shell away and draws the sign-in page, which lists the writes the lapsed
 * person pressed and Dispatch has not confirmed (`KeptWrites.tsx`). A draft typed and never sent
 * is no write: the page does not list it, the draft store keeps it through the lapse
 * (`DraftStore.signedIn`), and the same person's sign-in draws the shell again with the composer
 * open on it.
 *
 * A sign-out is no lapse. Once it lands - the person's own press of Sign out, which Dispatch
 * answers, or another tab's word that Dispatch signed them out there - every draft store forgets
 * the person's drafts, in the page and in the tab's storage, and takes none until the next
 * sign-in (`DraftStore.signedOut`): the draft the docked composer keeps for its address as the
 * sign-in page takes it away is dropped too. So the tab keeps nothing of theirs for whoever signs
 * in next at this browser, and the same person's sign-in opens the composer empty as another
 * person's does.
 *
 * The composer census finds `AuthGate`'s own conditionals above every host the app draws
 * (`app.tsx`): which person's shell is drawn (`shellFor`, once the stores were told), the rows
 * above; the check not yet answered (`whoAmI.isPending`), before which no shell, and so no
 * composer, is drawn, which the first row's check still out makes; and the page signed out
 * (`signedOut`), which reason 3 answers.
 *
 * The numbered reasons - the first row's two clients, the cell another file makes, the page
 * signed out, and the forms of the layout the composer census found that the rows above do not
 * make - are `REASONS` in `draft-matrix.sign-in.coverage.ts`, which holds each census item this
 * file answers for to a test above or to one of them.
 */
import { expect, jest, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";

import {
  commentA,
  commentB,
  endAsAReload,
  listTurn,
  planner,
  replyField,
} from "./__tests__/draft-matrix-fixture";
import { realClock, settle, tick } from "./__tests__/fake-clock";
import { answerRequests } from "./__tests__/network";
import { answerAgentStates } from "./__tests__/send-answers";
import { userOnFakeClock } from "./__tests__/user";
import { ApiError, api } from "./api/client";
import { DispatchQueryClient, queryRetry, queryRetryDelay } from "./api/query-client";
import { otherTab, renderApp } from "./app-sign-out-fixture";
import { composerNameOf } from "./features/conversation/useKeptDrafts";
import { threadReplySendKey } from "./features/conversation/usePhoneThread";
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

/** The docked composer's send name on CORE-1's issue page (`ConversationTab`'s `sendKey`). */
const DOCKED_KEY: MutationKey = ["conversation-composer", issue.key];

/**
 * Drafts the tab can hold in the envelope this release reads (`draft-store.ts`) whose text this
 * release cannot read as a draft: one an older release kept in another shape, and text that does
 * not parse. A store's text is its own JSON: a composer's draft under its send name (`unsent`),
 * and the list of drafts it keeps by address (`away`). The older shapes are a draft kept by
 * address with no `draft`, and a reply's draft whose body is a number.
 */
const UNREADABLE = [
  {
    away: '[{"owner":{"issueKey":"CORE-1","kind":"issue"},"replyTo":null,"text":"Older words"}]',
    kept: "in an older release's shape",
    unsent: '{"body":42,"mentions":[],"replacement":""}',
  },
  { away: '[{"draft":{"body":"Cut', kept: "as text that does not parse", unsent: '{"body":"Cut' },
] as const;

/** Puts `text` in the tab before the page loads, as alice's draft under `name` in the store
 *  `store`, in the envelope this release reads (`draft-store.ts`). */
function keptBeforeTheLoad(store: "away" | "unsent", name: string, text: string): void {
  sessionStorage.setItem(
    `dispatch.draft:${store}:${name}`,
    JSON.stringify({ login: "alice", text, version: 1 })
  );
}

/** Waits until the page draws the field `field` finds, or a region of it fails to render
 *  (`ErrorBoundary`), lets every task the page started run, and answers what it shows then: the
 *  field's text, or the words of each region that failed. */
async function drawnField(
  field: () => HTMLTextAreaElement
): Promise<{ failed: string[] } | { text: string }> {
  const shown = () => {
    const failed = screen
      .queryAllByText(/^Something went wrong in /)
      .map((heading) => heading.closest("section")?.textContent ?? "");
    return failed.length > 0 ? { failed } : { text: field().value };
  };
  await waitFor(shown);
  await act(settle);
  return shown();
}

/** What the page says it keeps of a draft the reader wrote and that is out of sight. */
const keptLines = () =>
  screen
    .queryAllByRole("status")
    .map((line) => line.textContent ?? "")
    .filter((line) => /\bkept\b/.test(line));

// The drafts kept by address (`away`): the docked composer reads the one kept for the reply it
// shows, none, as it mounts, and the page draws it empty with nothing said to be kept.
for (const { away, kept } of UNREADABLE) {
  test(`after a reload, a draft the tab kept by address ${kept} is none: the docked composer opens empty`, async () => {
    const restoreReads = answerIssueReads();
    keptBeforeTheLoad("away", composerNameOf(DOCKED_KEY), away);
    const app = renderApp({
      at: buildIssuePath({ key: issue.key, kind: "conversation" }),
      whoAmI: async () => named("alice"),
    });
    try {
      const drawn = await drawnField(() =>
        within(screen.getByRole("form", { name: "Comment composer" })).getByRole("textbox", {
          name: "Comment",
        })
      );
      expect({ drawn, said: keptLines() }).toEqual({ drawn: { text: "" }, said: [] });
    } finally {
      app.teardown();
      restoreReads();
    }
  });
}

// A thread's reply (`unsent`): Comment A's card, closed, says whether it keeps one, and its
// reply reads it as Expand thread mounts it, so the card says nothing is kept and its reply opens
// empty.
for (const { kept, unsent } of UNREADABLE) {
  test(`after a reload, a thread's reply the tab kept ${kept} is none: the card's reply opens empty`, async () => {
    const user = userEvent.setup();
    const restoreReads = answerIssueReads();
    keptBeforeTheLoad(
      "unsent",
      composerNameOf(threadReplySendKey(DOCKED_KEY, commentA.id)),
      unsent
    );
    const app = renderApp({
      at: buildIssuePath({ key: issue.key, kind: "conversation" }),
      whoAmI: async () => named("alice"),
    });
    try {
      const expand = await waitFor(() =>
        within(listTurn(commentA.id)).getByRole("button", { name: "Expand thread" })
      );
      const closed = keptLines();
      await user.click(expand);
      const drawn = await drawnField(() => replyField(listTurn(commentA.id)));
      expect({ closed, drawn }).toEqual({ closed: [], drawn: { text: "" } });
    } finally {
      app.teardown();
      restoreReads();
    }
  });
}

// A lapse draws the sign-in page in the shell's place, so the docked composer goes with it. The
// page lists the lapsed person's writes, and a typed draft is none, so it lists nothing; the
// draft store keeps the draft through the lapse, and the same person's sign-in opens the
// composer on it again.
test("a lapse, then the same person's sign-in, opens the composer on the words typed before the lapse", async () => {
  const user = userEvent.setup();
  const restoreReads = answerIssueReads();
  const at = buildIssuePath({ key: issue.key, kind: "conversation" });
  let lapsed = false;
  const app = renderApp({
    at,
    whoAmI: async () => {
      if (lapsed) throw new ApiError(401, { error: "unauthorized" });
      return named("alice");
    },
  });
  const typed = "Alice's comment, typed before the lapse";
  try {
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste(typed);

    lapsed = true;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Kept changes" })).toBeNull();
    expect(screen.queryByText(typed)).toBeNull();

    lapsed = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    const back = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(back).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value).toBe(
      typed
    );
  } finally {
    app.teardown();
    restoreReads();
  }
});

/** Every draft the tab keeps for a reload, as its entry's name and the login it is kept for. */
function draftsKept(): string[] {
  const storage = window.sessionStorage;
  return Array.from({ length: storage.length }, (_, index) => storage.key(index))
    .filter((name): name is string => name?.startsWith("dispatch.draft:") === true)
    .map((name) => {
      const kept: unknown = JSON.parse(storage.getItem(name) ?? "null");
      const login = typeof kept === "object" && kept !== null ? Reflect.get(kept, "login") : "";
      return `${name} (${String(login)})`;
    });
}

/** Where alice's sign-out lands: her own press of Sign out on this page, which Dispatch answers,
 *  or another tab's word on the sign-in channel that Dispatch signed her out there. */
type SignedOutIn = "this tab" | "another tab";

/** Has alice type into the docked composer on CORE-1's issue page, signs her out `where`, and
 *  then has alice sign in again, and, on a page of its own, bob instead. For each it answers what
 *  the tab keeps once she is signed out, what the composer opens on for the person signed in
 *  next, how many fields on the page hold her words then, and what the tab keeps then. */
async function afterASignOut(where: SignedOutIn): Promise<Record<string, unknown>> {
  const typed = "Alice's comment, typed before she signed out";
  const at = buildIssuePath({ key: issue.key, kind: "conversation" });
  const seen: Record<string, unknown> = {};
  for (const next of ["alice", "bob"]) {
    const user = userEvent.setup();
    const restoreReads = answerIssueReads();
    // Who Dispatch names: alice, nobody once she is signed out, and then the person signed in next.
    let login: string | undefined = "alice";
    const app = renderApp({
      at,
      logout: async () => {
        login = undefined;
        return { ok: true as const };
      },
      whoAmI: async () => {
        if (login === undefined) throw new ApiError(401, { error: "unauthorized" });
        return named(login);
      },
    });
    const signer = where === "another tab" ? otherTab("tab-2") : undefined;
    try {
      const composer = await screen.findByRole("form", { name: "Comment composer" });
      const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
      await user.click(field);
      await user.paste(typed);

      if (signer === undefined) {
        await user.click(screen.getByRole("button", { name: "Sign out" }));
      } else {
        login = undefined;
        act(() => signer.say({ signedOut: "alice" }));
      }
      await screen.findByRole("link", { name: "Sign in with Google" });
      const signedOut = draftsKept();

      login = next;
      await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
      await screen.findByText(`Signed in as ${next}`);
      const back = await screen.findByRole("form", { name: "Comment composer" });
      const opened = within(back).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
      seen[next] = {
        composer: opened.value,
        kept: draftsKept(),
        shown: screen.queryAllByDisplayValue(typed).length,
        signedOut,
      };
    } finally {
      signer?.close();
      app.teardown();
      restoreReads();
    }
  }
  return seen;
}

/** What a cell below sees for each person signed in after alice's sign-out: nothing of hers. */
const NOTHING_OF_ALICES = { composer: "", kept: [], shown: 0, signedOut: [] };

// A sign-out is no lapse: the person ended their session at this browser, and whoever signs in
// next here must find nothing they typed. Alice presses Sign out with words in the docked
// composer, and Dispatch answers it. The sign-in page takes the composer away, and the tab keeps
// none of her drafts, the one the composer keeps for its address as it goes among them; her own
// sign-in then opens the composer empty, as bob's does.
test("a sign-out, then the same person's or another person's sign-in, opens the composer empty and leaves the tab none of the person's drafts", async () => {
  expect(await afterASignOut("this tab")).toEqual({
    alice: NOTHING_OF_ALICES,
    bob: NOTHING_OF_ALICES,
  });
});

// The same sign-out, landed in another tab of the browser: that tab says Dispatch signed alice
// out (`{signedOut}` on the sign-in channel), this page reads who is signed in and draws the
// sign-in page, and it keeps nothing of hers either.
test("a sign-out in another tab, then the same person's or another person's sign-in, opens the composer empty and leaves the tab none of the person's drafts", async () => {
  expect(await afterASignOut("another tab")).toEqual({
    alice: NOTHING_OF_ALICES,
    bob: NOTHING_OF_ALICES,
  });
});
