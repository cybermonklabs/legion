import { expect, jest, spyOn, test } from "bun:test";
import {
  act,
  // biome-ignore lint/style/noRestrictedImports: the bare pointer press below has no user-event form - `click` would also send the click this row proves the page has not had.
  fireEvent,
  renderHook,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { endAsAReload, planner } from "./__tests__/draft-matrix-fixture";
import { onRealTurns, realClock, tick } from "./__tests__/fake-clock";
import { keptWrites } from "./__tests__/kept-sends-harness";
import { answerAgentStates } from "./__tests__/send-answers";
import { userOnFakeClock } from "./__tests__/user";
import { wholeText } from "./__tests__/whole-text";
import { ApiError, api, NetworkError } from "./api/client";
import { aliceWrite, type OtherTab, otherTab, renderApp } from "./app-sign-out-fixture";
import { unsentDraftsOf } from "./features/conversation/unsent-drafts";
import { goingState, resetGoingStore, startRun } from "./features/going/going-store";
import { answerDraft, setAnswerDraft } from "./features/inbox/answer-drafts";
import { setReplyDraft, useReplyDraft } from "./features/inbox/reply-drafts";
import { OTHER_TABS_ANSWER_MS, type UnsentChange } from "./features/shell/sign-in-channel";
import { SEND_DEADLINE_MS, stoppedWriteNote } from "./features/writes/keyed-write";
import { writesOf } from "./features/writes/write-store";
// The margin is a lazy part of the shell (`MarginColumn.tsx`), which loads it at `/` too. No row
// here reads it, and a load that lands between two `act` scopes of a row is React's report of a
// suspended resource outside `act`. So the module is loaded here, with the test file, and no row
// starts or waits for it.
import "./features/margin/Margin";

/** A message the tabs of this browser post to each other, naming `login`, from whichever tab. */
const about = (field: string, login: string) => expect.objectContaining({ [field]: login });

/** A change a sign-out would discard, as a tab names it: by its key and label. */
const change = (label: string, key = `key of ${label}`): UnsentChange => ({ key, label });

// W48. A sign-out discards only once Dispatch has answered it. While it is out nothing more goes
// out here - every write stops trying after the attempt in flight - and every other tab is told to
// hold the person's writes too. Refused, it leaves the person signed in with every write, here and
// in every other tab, and says the sign-out did not go through; its Retry, once it lands, drops
// every write here and tells every other tab whose to drop.
test("a sign-out discards only once Dispatch answers it, and one that fails keeps every write", async () => {
  // On the fake clock from the first render: every timer of the product - the retry's backoff
  // among them - is one this test moves the clock past, rather than one it waits out.
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const first = Promise.withResolvers<{ ok: true }>();
  let signedIn = true;
  let logouts = 0;
  const app = renderApp({
    logout: () => {
      logouts += 1;
      if (logouts === 1) return first.promise;
      signedIn = false;
      return Promise.resolve({ ok: true });
    },
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    void aliceWrite(app.queryClient, attempts, "CORE-2 set to done", true);
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(2));

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.logout).toHaveBeenCalledTimes(1));
    expect(app.asked).toHaveLength(1);
    await waitFor(() => expect(tab.told).toContainEqual(about("signingOut", "alice")));
    const made = attempts.length;
    await tick(2_500);
    expect(attempts).toHaveLength(made);
    expect(writesOf(app.queryClient).rows()).toHaveLength(3);
    expect(keptWrites()).toHaveLength(3);

    act(() => first.reject(new ApiError(403, { code: "FORBIDDEN", error: "not now" })));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("The sign-out did not go through.");
    expect(screen.getByText(wholeText("Signed in as alice"))).toBeTruthy();
    expect(
      writesOf(app.queryClient)
        .rows()
        .map((row) => row.label)
    ).toEqual(["CORE-1 set to done", "CORE-2 set to done"]);
    expect(keptWrites()).toHaveLength(2);
    await waitFor(() => expect(tab.told).toContainEqual(about("stillSignedIn", "alice")));
    expect(tab.told).not.toContainEqual(about("signedOut", "alice"));
    await waitFor(() => expect(attempts.length).toBeGreaterThan(made));

    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    await waitFor(() => expect(tab.told).toContainEqual(about("signedOut", "alice")));
  } finally {
    realClock();
    tab.close();
    app.teardown();
  }
});

// A sign-out the reader stopped may have ended the session, which the page cannot know it did
// not: so it does not say the person is still signed in. It says Dispatch has not confirmed the
// sign-out, that every change is still here, and what its Retry does.
test("a sign-out the reader stopped does not say the person is still signed in", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const app = renderApp({
    logout: () =>
      Promise.reject(new NetworkError("/auth/logout", new TypeError("Failed to fetch"))),
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.logout).toHaveBeenCalledTimes(1));
    await tick(SEND_DEADLINE_MS + 1_000);
    const late = writesOf(app.queryClient)
      .rows()
      .find((row) => row.label === "Sign out");
    expect(late?.status).toBe("late");
    act(() => writesOf(app.queryClient).stop(late?.id ?? 0));
    await tick(60_000);

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      `${stoppedWriteNote("the sign-out", "made", {
        anew: "Every change is still here. Retry ends your session, or finds it ended, and signs you out here.",
      })} Retry`
    );
  } finally {
    realClock();
    app.teardown();
  }
});

// Someone signed in from another tab arrives at this page, which draws them a shell of their own.
// A sign-out Dispatch refused is the person's it was pressed for, so once someone
// else is signed in its refusal - which would tell them they are still signed in, with every
// change - and its Retry, which would sign them out, are gone.
test("a sign-out refused for one person is gone, its Retry with it, once another signs in from another tab", async () => {
  const user = userEvent.setup();
  let login = "alice";
  const app = renderApp({
    logout: () => Promise.reject(new ApiError(403, { code: "FORBIDDEN", error: "not now" })),
    whoAmI: async () => ({ event_head: "0", kind: "user", login }),
  });
  const tab = new BroadcastChannel("dispatch-sign-in");
  const refusal = "The sign-out did not go through. You are still signed in, with every change.";
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByText(refusal)).toBeTruthy();

    login = "bob";
    act(() => tab.postMessage("bob"));
    await screen.findByText(wholeText("Signed in as bob"));
    expect(screen.queryByText(refusal)).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(app.logout).toHaveBeenCalledTimes(1);
  } finally {
    tab.close();
    app.teardown();
  }
});

// A draft is the person's who typed it, and every draft store outlives the shell, which someone
// signed in from another tab arrives in, as the write store does. Once another person is signed
// in, none of the drafts the person before typed and did not send is kept: a composer's, one it
// keeps for another address, an ask's answer and an ask's reply (`DraftStore.signedIn`).
test("another person signed in from another tab finds none of the drafts the person before kept", async () => {
  let login = "alice";
  const app = renderApp({ whoAmI: async () => ({ event_head: "0", kind: "user", login }) });
  const tab = new BroadcastChannel("dispatch-sign-in");
  const unsent = unsentDraftsOf(app.queryClient);
  const draft = { body: "Alice's unsent reply", mentions: [], replacement: "" };
  const reply = renderHook(() => useReplyDraft("ask-1"));
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    act(() => {
      unsent.write("composer", draft);
      unsent.keepAway("composer", {
        draft,
        owner: { issueKey: "CORE-1", kind: "issue" },
        replyTo: null,
      });
      setAnswerDraft("ask-1", "Alice's unsent answer");
      setReplyDraft("ask-1", "Alice's unsent ask reply");
    });
    expect(unsent.read("composer")).toEqual(draft);
    expect(unsent.readAway("composer", null)?.draft).toEqual(draft);
    expect(answerDraft("ask-1")).toBe("Alice's unsent answer");
    expect(reply.result.current[0]).toBe("Alice's unsent ask reply");

    login = "bob";
    act(() => tab.postMessage("bob"));
    await screen.findByText(wholeText("Signed in as bob"));
    await waitFor(() => expect(unsent.read("composer")).toBeUndefined());
    expect(unsent.readAway("composer", null)).toBeUndefined();
    expect(answerDraft("ask-1")).toBe("");
    expect(reply.result.current[0]).toBe("");
  } finally {
    reply.unmount();
    tab.close();
    app.teardown();
  }
});

/** The names of the drafts this tab keeps across a reload, in order. */
function keptDrafts(): string {
  return Array.from({ length: window.sessionStorage.length }, (_, index) =>
    window.sessionStorage.key(index)
  )
    .filter((name): name is string => name?.startsWith("dispatch.draft:") === true)
    .sort()
    .join(", ");
}

// The tab keeps each draft for a reload of the page, stamped with the person who typed it
// (`draft-store.ts`), and another person's sign-in forgets it there as it does in the page: once
// they are signed in, the tab holds none of the person before's drafts, and a reload brings back
// none of them - a composer's, one it keeps for another address, an ask's answer or its reply.
test("after another person's sign-in, a reload of the same tab brings back none of the drafts the person before kept", async () => {
  let login = "alice";
  const whoAmI = async () => ({ event_head: "0", kind: "user" as const, login });
  let app = renderApp({ whoAmI });
  const tab = new BroadcastChannel("dispatch-sign-in");
  const draft = { body: "Alice's unsent reply", mentions: [], replacement: "" };
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    const unsent = unsentDraftsOf(app.queryClient);
    act(() => {
      unsent.write("composer", draft);
      unsent.keepAway("composer", {
        draft,
        owner: { issueKey: "CORE-1", kind: "issue" },
        replyTo: null,
      });
      setAnswerDraft("ask-1", "Alice's unsent answer");
      setReplyDraft("ask-1", "Alice's unsent ask reply");
    });
    const keptForAlice = keptDrafts();

    login = "bob";
    act(() => tab.postMessage("bob"));
    await screen.findByText(wholeText("Signed in as bob"));
    await waitFor(() => expect(unsent.read("composer")).toBeUndefined());
    const keptAfterBob = keptDrafts();

    endAsAReload(() => app.teardown());
    app = renderApp({ whoAmI });
    await screen.findByText(wholeText("Signed in as bob"));
    const reloaded = unsentDraftsOf(app.queryClient);
    expect({
      afterBob: keptAfterBob,
      alice: keptForAlice,
      reloaded: {
        answer: answerDraft("ask-1"),
        away: reloaded.readAway("composer", null),
        composer: reloaded.read("composer"),
        kept: keptDrafts(),
        reply: renderHook(() => useReplyDraft("ask-1")).result.current[0],
      },
    }).toEqual({
      afterBob: "",
      alice:
        "dispatch.draft:answer:ask-1, dispatch.draft:away:composer, dispatch.draft:reply:ask-1, dispatch.draft:unsent:composer",
      reloaded: { answer: "", away: undefined, composer: undefined, kept: "", reply: "" },
    });
  } finally {
    tab.close();
    app.teardown();
  }
});

// What the reader typed and has not sent is theirs alone. A field on screen holds it in the shell
// itself, so another person signed in from another tab is drawn a shell of their own, once every
// store has let go of what the person before kept (`AuthGate`): the first person's words are on
// no screen of the second's.
test("a composer's typed text is not shown to another person signed in from another tab", async () => {
  const user = userEvent.setup();
  let login = "alice";
  const agentReads = [
    spyOn(api, "listAgents").mockResolvedValue([planner]),
    spyOn(api, "listAgentMessages").mockResolvedValue([]),
    spyOn(api, "getMyAgentState").mockResolvedValue({}),
  ];
  answerAgentStates().mockImplementation(async () => ({ unread_replies: 0 }));
  const app = renderApp({
    at: "/agents",
    whoAmI: async () => ({ event_head: "0", kind: "user", login }),
  });
  const tab = new BroadcastChannel("dispatch-sign-in");
  const typed = "Alice's words for the planner";
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    const region = await screen.findByRole("region", { name: "Agents" });
    const row = (await within(region).findByRole("heading", { name: "Planner" })).closest(
      "article"
    );
    if (row === null) throw new Error("the Planner's row is missing");
    await user.click(within(row).getByRole("button", { name: "Planner" }));
    const field = await within(row).findByRole<HTMLTextAreaElement>("textbox", {
      name: "Comment",
    });
    await user.click(field);
    await user.paste(typed);
    expect(field.value).toBe(typed);

    login = "bob";
    act(() => tab.postMessage("bob"));
    await screen.findByText(wholeText("Signed in as bob"));
    await waitFor(() => expect(screen.queryAllByDisplayValue(typed).length).toBe(0));
  } finally {
    tab.close();
    app.teardown();
    for (const read of agentReads) read.mockRestore();
  }
});

// `/auth/logout` sits outside Dispatch's keyed layer: the sign-out is a `POST` that sends no key,
// and its write says so (`Write.keyed`), as a `PUT`'s or a `DELETE`'s does.
test("a sign-out's write says it goes under no key, since /auth/logout sends none", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const app = renderApp({
    logout: () => Promise.withResolvers<{ ok: true }>().promise,
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.logout).toHaveBeenCalledTimes(1));
    const signOut = writesOf(app.queryClient)
      .rows()
      .find((row) => row.label === "Sign out");
    expect(signOut?.keyed).toBe(false);
  } finally {
    realClock();
    app.teardown();
  }
});

// W50, W60. A sign-out Dispatch answers `401 NO_IDENTITY` has landed: the session it would end is
// already gone - its answer was lost and the retry found the session ended, or a sign-out in
// another browser revoked it. The changes the person chose to discard go, here and in every other
// tab: the sign-in page keeps none of them, and none is sent once they sign in again.
test("a sign-out answered 401 discards what it said it would, and nothing goes out at the next sign-in", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  let signedIn = true;
  const app = renderApp({
    logout: async () => {
      signedIn = false;
      throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
    },
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(1));
    await screen.findByRole("link", { name: "Sign in with Google" });
    await waitFor(() => expect(tab.told).toContainEqual(about("signedOut", "alice")));
    expect(tab.told).not.toContainEqual(about("stillSignedIn", "alice"));
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    expect(screen.queryByRole("region", { name: "Kept changes" })).not.toBeInTheDocument();

    signedIn = true;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await screen.findByText(wholeText("Signed in as alice"));
    const made = attempts.length;
    await tick(2_500);
    expect(attempts).toHaveLength(made);
    expect(writesOf(app.queryClient).rows()).toEqual([]);
  } finally {
    realClock();
    tab.close();
    app.teardown();
  }
});

// W50. A session that ends while its sign-out is still out - another browser's sign-out revoked
// it, and this page's sign-in check says so first - has ended as the sign-out asked: it discards,
// rather than keeping every write for a sign-in the person chose to discard them before.
test("a session that ends under its sign-out discards, and keeps nothing for the next sign-in", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const held = Promise.withResolvers<{ ok: true }>();
  let signedIn = true;
  const app = renderApp({
    logout: () => held.promise,
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.logout).toHaveBeenCalledTimes(1));
    signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await screen.findByRole("link", { name: "Sign in with Google" });
    await waitFor(() => expect(tab.told).toContainEqual(about("signedOut", "alice")));
    expect(tab.told).not.toContainEqual(about("stillSignedIn", "alice"));
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    expect(screen.queryByRole("region", { name: "Kept changes" })).not.toBeInTheDocument();

    signedIn = true;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await screen.findByText(wholeText("Signed in as alice"));
    const made = attempts.length;
    await tick(2_500);
    expect(attempts).toHaveLength(made);
  } finally {
    realClock();
    held.resolve({ ok: true });
    tab.close();
    app.teardown();
  }
});

// W50, W60. Dispatch's own 401 is the verdict: the sign-out lands even while the sign-in check
// cannot be read to say the session lapsed, rather than asking again for good with every write
// held.
test("a sign-out answered 401 lands while the sign-in check cannot be read", async () => {
  const user = userEvent.setup();
  let checkable = true;
  const app = renderApp({
    logout: async () => {
      checkable = false;
      throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
    },
    whoAmI: async () => {
      if (!checkable) throw new NetworkError("/auth/whoami", new TypeError("Failed to fetch"));
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(tab.told).toContainEqual(about("signedOut", "alice")));
    expect(app.logout).toHaveBeenCalledTimes(1);
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
  } finally {
    tab.close();
    app.teardown();
  }
});

// A sign-out that lands ends the person's command bar with their session: whoever signs in next in
// this tab finds none of their runs - no chip at the top of the page names one - and none of their
// runs' streams is read any more.
test("a sign-out forgets the command bar's runs and stops reading their streams", async () => {
  const user = userEvent.setup();
  let login: string | undefined = "alice";
  const app = renderApp({
    whoAmI: async () => {
      if (login === undefined) throw new ApiError(401, { error: "unauthorized" });
      return { event_head: "0", kind: "user", login };
    },
    logout: async () => {
      login = undefined;
      return { ok: true };
    },
  });
  let reading: AbortSignal | undefined;
  const start = spyOn(api, "createGoingRun").mockResolvedValue({
    context: null,
    continues: null,
    conversation: "c-1",
    created_at: new Date().toISOString(),
    finished_at: null,
    id: "r-1",
    issues: [],
    level: "standard",
    line: null,
    mode: "ask",
    standing_count: 0,
    state: "queued",
    stop_reason: null,
    undoable: false,
    undoable_count: 0,
    undone_at: null,
    undone_count: 0,
    used: { calls: 0, tokens: 0, working_ms: 0 },
    words: "count the moons",
  });
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, _onFrame, signal) => {
    reading = signal;
    return Promise.withResolvers<void>().promise;
  });
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await act(() => startRun("count the moons", { path: "/" }, "alice"));
    expect(await screen.findByTestId("going-chip")).toHaveTextContent("Working · Count the moons");

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(reading?.aborted).toBe(true);
    expect(goingState().runs.size).toBe(0);

    login = "bob";
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await screen.findByText(wholeText("Signed in as bob"));
    expect(screen.queryByTestId("going-chip")).not.toBeInTheDocument();
  } finally {
    start.mockRestore();
    stream.mockRestore();
    resetGoingStore();
    app.teardown();
  }
});

// W60. A 401 that is not Dispatch's own - an authenticating proxy in front of it refusing the
// request - says nothing about the session: the sign-out never reached Dispatch, so it did not
// go through. The person stays signed in with every change, as they do after any other refusal,
// rather than having their changes discarded by a 401 Dispatch never gave.
test("a 401 that is not Dispatch's own is a sign-out that did not go through", async () => {
  // The second ask waits a second of the product's own: on the fake clock, which this test's
  // `findByRole` moves on, rather than a second of the run's.
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const app = renderApp({
    logout: async () => {
      throw new ApiError(401, { error: "Unauthorized" });
    },
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    // The first 401 may be a session that rotated between the two reads, so the sign-out is
    // asked once more, a second after; the second, with the check still naming alice, is the
    // refusal.
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("The sign-out did not go through.");
    expect(app.logout).toHaveBeenCalledTimes(2);
    expect(screen.getByText(wholeText("Signed in as alice"))).toBeTruthy();
    expect(
      writesOf(app.queryClient)
        .rows()
        .map((row) => row.label)
    ).toEqual(["CORE-1 set to done"]);
    expect(keptWrites()).toHaveLength(1);
    await waitFor(() => expect(tab.told).toContainEqual(about("stillSignedIn", "alice")));
    expect(tab.told).not.toContainEqual(about("signedOut", "alice"));
  } finally {
    realClock();
    tab.close();
    app.teardown();
  }
});

// W45. The sign-out question names every tab's unsent changes: it asks the other tabs of this
// browser first, over the sign-in channel, and lists what each of them would discard beside this
// tab's own. A tab it knows of that does not answer within 300 ms is named as such.
test("the sign-out question names every tab's unsent changes, and says when a tab did not answer", async () => {
  // On the fake clock, so the window the question waits out for the silent tab is moved past in
  // the test's own steps, and the steps that show the question waits for it move no clock but
  // those steps: the other tabs' answers come on real turns (`onRealTurns`), which move no
  // clock. The clock is installed before the user, which captures the timers it will advance
  // (dispatch://INFRA-1505).
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const answer = false;
  const app = renderApp({
    confirm: () => answer,
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const second = otherTab("tab-2", [change("CORE-4 set to backlog"), change("Labels of CORE-4")]);
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() =>
      expect(second.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(1));
    expect(app.asked[0]).toBe(
      "Sign out and discard 3 unsent changes?\n\n• CORE-1 set to done\n• CORE-4 set to backlog\n• Labels of CORE-4"
    );
    expect(second.told).toContainEqual(expect.objectContaining({ unsentOf: "alice" }));
    expect(app.logout).not.toHaveBeenCalled();

    const silent = otherTab("tab-3", "silent");
    try {
      await waitFor(() =>
        expect(silent.told).toContainEqual(expect.objectContaining({ here: true }))
      );
      await user.click(screen.getByRole("button", { name: "Sign out" }));
      // The question waits out the window for the tab that does not answer: 50 ms before the
      // window ends it is not asked, whatever the other tab answered on the real turns since,
      // and once the window has passed it is.
      await tick(OTHER_TABS_ANSWER_MS - 50);
      await onRealTurns(() => app.asked.length > 1);
      expect(app.asked).toHaveLength(1);
      await tick(50);
      await onRealTurns(() => app.asked.length > 1);
      expect(app.asked).toHaveLength(2);
      expect(app.asked[1]).toBe(
        "Sign out and discard 3 unsent changes, and any unsent changes in your other Dispatch tabs?\n\n• CORE-1 set to done\n• CORE-4 set to backlog\n• Labels of CORE-4"
      );
    } finally {
      silent.close();
    }
    expect(app.logout).not.toHaveBeenCalled();
    expect(writesOf(app.queryClient).rows()).toHaveLength(1);
  } finally {
    realClock();
    second.close();
    app.teardown();
  }
});

// W53. The sign-out question names each change once, by key: a tab duplicated from this one, or
// from another, copied its kept writes and resumed them under the same keys, so its copies are the
// same changes. A tab that did not answer the last question is taken for gone, so the next one
// neither waits for it nor says the other tabs may hold more, until it speaks again.
test("the sign-out question names a duplicated tab's copies once, and forgets a tab that missed it", async () => {
  // On the fake clock, held while the second question is asked: that question must come without
  // a timer, so it is waited for on real turns (`onRealTurns`), which carry the other tabs'
  // answers and move no clock. A question that waited out the window for the forgotten tab would
  // come only once the clock passed that window, and here only the click's own zero-delay waits
  // move it, so it would not come at all (dispatch://INFRA-1505). The clock is installed before
  // the user, which captures the timers it will advance.
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const app = renderApp({
    confirm: () => false,
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const tabs: OtherTab[] = [];
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));
    const own = writesOf(app.queryClient).rows()[0]?.key ?? "";
    const copies = [change("CORE-1 set to done", own), change("CORE-4 set to backlog", "k-4")];
    tabs.push(otherTab("tab-2", copies), otherTab("tab-3", copies), otherTab("tab-4", "silent"));
    for (const other of tabs) {
      await waitFor(() =>
        expect(other.told).toContainEqual(expect.objectContaining({ here: true }))
      );
    }
    const named = "2 unsent changes";
    const listed = "\n\n• CORE-1 set to done\n• CORE-4 set to backlog";
    const elsewhere = ", and any unsent changes in your other Dispatch tabs";

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(1));
    expect(app.asked[0]).toBe(`Sign out and discard ${named}${elsewhere}?${listed}`);

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await onRealTurns(() => app.asked.length > 1);
    expect(app.asked).toHaveLength(2);
    expect(app.asked[1]).toBe(`Sign out and discard ${named}?${listed}`);

    const beforeHello = tabs[2]?.told.length ?? 0;
    tabs[2]?.say({ hello: true });
    // The app's answer to this hello, not the one it gave when the tab opened: until the app has
    // heard the tab speak again it is still forgotten, and the question below would be the
    // question above.
    await waitFor(() =>
      expect(tabs[2]?.told.slice(beforeHello)).toContainEqual(
        expect.objectContaining({ here: true })
      )
    );
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(3));
    expect(app.asked[2]).toBe(`Sign out and discard ${named}${elsewhere}?${listed}`);
    expect(app.logout).not.toHaveBeenCalled();
  } finally {
    realClock();
    for (const other of tabs) other.close();
    app.teardown();
  }
});

// W43. A lapsed tab shows the writes it keeps: the sign-in page lists the lapsed person's kept
// writes by name, so they see what waits for them, and a refused one offers Discard there.
test("a lapsed tab's sign-in page lists the writes it keeps, a refused one with Discard", async () => {
  const user = userEvent.setup();
  let signedIn = true;
  const app = renderApp({
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-7 set to todo");
    void aliceWrite(app.queryClient, attempts, "CORE-7 set to backlog", true);
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(2));

    signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    const kept = await screen.findByRole("region", { name: "Kept changes" });
    expect(kept).toHaveTextContent("Kept until you sign in again as alice:");
    expect(kept).toHaveTextContent("CORE-7 set to todo");
    expect(kept).toHaveTextContent("CORE-7 set to backlog");
    const buttons = within(kept).getAllByRole("button", { name: "Discard" });
    expect(buttons).toHaveLength(1);
    await user.click(buttons[0] as HTMLElement);
    await waitFor(() =>
      expect(
        writesOf(app.queryClient)
          .rows()
          .map((row) => row.label)
      ).toEqual(["CORE-7 set to todo"])
    );
    expect(kept.textContent).not.toContain("CORE-7 set to backlog");
    expect(keptWrites()).toHaveLength(1);
  } finally {
    app.teardown();
  }
});

// W44. The sign-in link of a lapsed tab raises no leave prompt: the writes it keeps resume in this
// tab once the person is back, so its own navigation is no leaving. Any other way out still asks.
test("the sign-in link of a lapsed tab raises no leave prompt", async () => {
  const user = userEvent.setup();
  let signedIn = true;
  const app = renderApp({
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const asksBeforeLeaving = () => {
    const leaving = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  };
  // The test's page does not navigate: the link's own handler runs, and the click stops there.
  const stay = (event: Event) => event.preventDefault();
  window.addEventListener("click", stay);
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-7 set to todo");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));
    signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    const link = await screen.findByRole("link", { name: "Sign in with Google" });
    expect(asksBeforeLeaving()).toBe(true);

    await user.click(link);
    expect(asksBeforeLeaving()).toBe(false);
    // Back on the page - the navigation never happened - leaving asks again.
    // A bare pointer press: user-event's `click` would send the click this row proves the page
    // has not had.
    fireEvent.pointerDown(document.body);
    expect(asksBeforeLeaving()).toBe(true);
  } finally {
    window.removeEventListener("click", stay);
    app.teardown();
  }
});
