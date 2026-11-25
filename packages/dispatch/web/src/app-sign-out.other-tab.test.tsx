import { expect, jest, test } from "bun:test";
import { act, screen, waitFor } from "@testing-library/react";

import { onRealTurns, realClock, tick } from "./__tests__/fake-clock";
import { keptWrites } from "./__tests__/kept-sends-harness";
import { wholeText } from "./__tests__/whole-text";
import { ApiError } from "./api/client";
import { aliceWrite, otherTab, renderApp } from "./app-sign-out-fixture";
import { SIGN_OUT_SETTLE_MS } from "./features/shell/sign-in-channel";
import { writesOf } from "./features/writes/write-store";

// The sign-out rows of another tab's sign-out, which holds this tab's writes for the person it
// signs out until that tab says how it ended, or goes: the writes are discarded once Dispatch has
// ended the session, and go on once it still names the person. The rows of this tab's own
// sign-out are in `app-sign-out.test.tsx`.

// W57. A tab whose sign-out held this tab's writes went without saying how it ended: it closed,
// crashed or was discarded, so only Dispatch knows whether the sign-out landed. This tab reads
// who is signed in before it decides. Dispatch's `401 NO_IDENTITY` says the sign-out landed, so
// the changes the person chose to discard go here too - keeping them would send, at their next
// sign-in, the very writes they discarded.
test("the writes held for a tab that went are discarded once whoami says the sign-out landed", async () => {
  jest.useFakeTimers();
  let signedIn = true;
  const app = renderApp({
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const signer = otherTab("tab-2");
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() =>
      expect(signer.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(attempts.length).toBeGreaterThan(0));

    signer.say({ signingOut: "alice" });
    await screen.findByText("CORE-1 set to done — Waiting for a sign-out in another tab");
    // Dispatch answers that tab's sign-out, and it goes before it can say so.
    signedIn = false;
    signer.close();

    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    expect(screen.queryByRole("region", { name: "Kept changes" })).not.toBeInTheDocument();
    const made = attempts.length;
    await tick(2_500);
    expect(attempts).toHaveLength(made);
  } finally {
    realClock();
    app.teardown();
  }
});

// W57. The same tab going while the person is still signed in - its sign-out was refused, or
// never answered - leaves their writes exactly where they stood. The first read still names
// them, which settles nothing while that tab's request may still be reaching Dispatch, so the
// hold stays on and nothing of theirs goes out for `SIGN_OUT_SETTLE_MS`; the read after it still
// names them, and only then do the writes go on.
test("the writes held for a tab that went go on once whoami still names the person", async () => {
  jest.useFakeTimers();
  const app = renderApp({
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const signer = otherTab("tab-2");
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() =>
      expect(signer.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(attempts.length).toBeGreaterThan(0));

    signer.say({ signingOut: "alice" });
    await screen.findByText("CORE-1 set to done — Waiting for a sign-out in another tab");
    const held = attempts.length;
    signer.close();

    // Nothing goes out while the question is open, however the first read answered.
    await tick(SIGN_OUT_SETTLE_MS - 500);
    expect(attempts).toHaveLength(held);
    expect(
      screen.getByText("CORE-1 set to done — Waiting for a sign-out in another tab")
    ).toBeTruthy();

    await waitFor(() => expect(attempts.length).toBeGreaterThan(held));
    expect(screen.queryByText(/Waiting for a sign-out in another tab/)).not.toBeInTheDocument();
    expect(
      writesOf(app.queryClient)
        .rows()
        .map((row) => row.label)
    ).toEqual(["CORE-1 set to done"]);
    expect(screen.getByText(wholeText("Signed in as alice"))).toBeTruthy();
  } finally {
    realClock();
    signer.close();
    app.teardown();
  }
});

/** The person this page's whoami names while it answers one. */
type SignedInUser = { event_head: string; kind: "user"; login: string };

/** The app at `/` for alice, holding a write of hers for another tab's sign-out (`tab-2`), its
 *  whoami answered alice until `signedIn` turns false, and then Dispatch's `401 NO_IDENTITY` -
 *  except a read `parked` is set for, which waits for the test to answer it. */
async function holdingForAnotherTab() {
  const state: {
    parked?: { promise: Promise<SignedInUser>; resolve: (user: SignedInUser) => void };
    signedIn: boolean;
  } = { signedIn: true };
  const app = renderApp({
    whoAmI: async () => {
      const parked = state.parked;
      if (parked !== undefined) {
        state.parked = undefined;
        return parked.promise;
      }
      if (!state.signedIn) throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const signer = otherTab("tab-2");
  await screen.findByText(wholeText("Signed in as alice"));
  await waitFor(() => expect(signer.told).toContainEqual(expect.objectContaining({ here: true })));
  const attempts: string[] = [];
  void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
  await waitFor(() => expect(attempts.length).toBeGreaterThan(0));
  signer.say({ signingOut: "alice" });
  await screen.findByText("CORE-1 set to done — Waiting for a sign-out in another tab");
  return { app, attempts, signer, state };
}

/** Whether alice's held write was discarded, here and in what this tab keeps, with nothing of it
 *  sent after the sign-out and no sign-in page offering it back. Its caller holds the fake
 *  clock: the window nothing goes out in is moved past here rather than waited out. */
async function discardedFor(app: ReturnType<typeof renderApp>, attempts: string[]) {
  await screen.findByRole("link", { name: "Sign in with Google" });
  expect(writesOf(app.queryClient).rows()).toEqual([]);
  expect(keptWrites()).toEqual([]);
  expect(screen.queryByRole("region", { name: "Kept changes" })).not.toBeInTheDocument();
  const made = attempts.length;
  await tick(2_500);
  expect(attempts).toHaveLength(made);
}

// A 401 for a login this tab holds writes for, for another tab's sign-out, is that sign-out
// landing: Dispatch ended the session the sign-out asked it to end. It discards alice's write
// whichever reaches this tab first. Here the tab's own sign-in check answers 401 first - Dispatch
// closed its live socket as it ended the session - while the hold is still on.
test("a 401 while another tab's sign-out holds the writes discards them, before that tab goes", async () => {
  jest.useFakeTimers();
  const { app, attempts, signer, state } = await holdingForAnotherTab();
  try {
    state.signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await discardedFor(app, attempts);
    // That tab going afterwards changes nothing: there is nothing of alice's left to hold.
    signer.close();
    await tick(200);
    expect(writesOf(app.queryClient).rows()).toEqual([]);
  } finally {
    realClock();
    signer.close();
    app.teardown();
  }
});

// The other order: the tab goes first, and the whoami read its going starts was sent before
// Dispatch ended the session, so it answers 200 - but only after this tab's own sign-in check,
// read after the session ended, answered 401 under the hold. The 401 decided; the late 200
// lets go of a hold that has nothing left in it.
test("a 401 while another tab's sign-out holds the writes discards them, after that tab goes", async () => {
  jest.useFakeTimers();
  const { app, attempts, signer, state } = await holdingForAnotherTab();
  try {
    const goneRead = Promise.withResolvers<SignedInUser>();
    state.parked = goneRead;
    signer.close();
    await waitFor(() => expect(state.parked).toBeUndefined());
    state.signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await act(async () => goneRead.resolve({ event_head: "0", kind: "user", login: "alice" }));
    await discardedFor(app, attempts);
  } finally {
    realClock();
    signer.close();
    app.teardown();
  }
});

// The third order, the one WebKit takes: the tab goes, its whoami read answers 200 because the
// sign-out's own request had not reached Dispatch yet, the hold is let go - and Dispatch ends the
// session a moment later. That release is unsettled (`Writes.releaseUnsettled`), so the lapse
// that follows it is that sign-out landing late, and the writes it would discard go rather than
// being kept for a sign-in the person discarded them before.
test("a 401 just after a gone tab's hold was let go discards the writes it held", async () => {
  jest.useFakeTimers();
  const { app, attempts, signer, state } = await holdingForAnotherTab();
  try {
    signer.close();
    // Both reads its going makes still name alice, so the hold is let go and the write goes on.
    await waitFor(() =>
      expect(screen.queryByText(/Waiting for a sign-out in another tab/)).not.toBeInTheDocument()
    );
    state.signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await discardedFor(app, attempts);
  } finally {
    realClock();
    signer.close();
    app.teardown();
  }
});

// A release that decides over time ends the hold it was deciding about and no later one. The
// first tab crashes, and while this tab reads who is signed in over `SIGN_OUT_SETTLE_MS` another
// tab signs the same person out: that sign-out is live, so nothing of theirs goes out when the
// read comes back, and it is that tab's to end.
test("a sign-out begun while a gone tab's hold is being decided keeps the writes held", async () => {
  jest.useFakeTimers();
  const { app, attempts, signer } = await holdingForAnotherTab();
  const second = otherTab("tab-3");
  try {
    const held = attempts.length;
    signer.close();
    await waitFor(() =>
      expect(second.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    second.say({ signingOut: "alice" });

    // Well past the settle the first release would have ended at: the second sign-out holds.
    await tick(SIGN_OUT_SETTLE_MS + 1_500);
    expect(attempts).toHaveLength(held);
    expect(
      screen.getByText("CORE-1 set to done — Waiting for a sign-out in another tab")
    ).toBeTruthy();

    // It ends as that tab says, and only then do the writes go on.
    second.say({ stillSignedIn: "alice" });
    await waitFor(() => expect(attempts.length).toBeGreaterThan(held));
    expect(screen.queryByText(/Waiting for a sign-out in another tab/)).not.toBeInTheDocument();
  } finally {
    realClock();
    second.close();
    signer.close();
    app.teardown();
  }
});

// A lapse with no other tab's sign-out holding the writes is a lapse, as before: they are kept
// for the person's next sign-in.
test("a 401 with no other tab's sign-out holding the writes keeps them", async () => {
  jest.useFakeTimers();
  const { app, signer, state } = await holdingForAnotherTab();
  try {
    signer.say({ stillSignedIn: "alice" });
    await waitFor(() =>
      expect(screen.queryByText(/Waiting for a sign-out in another tab/)).not.toBeInTheDocument()
    );
    state.signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    const kept = await screen.findByRole("region", { name: "Kept changes" });
    expect(kept).toHaveTextContent("CORE-1 set to done");
    expect(keptWrites()).toHaveLength(1);
  } finally {
    realClock();
    signer.close();
    app.teardown();
  }
});

// W51. Another tab's sign-out holds the person's writes here as a lease, and each held write says
// what it waits for. A signing-out tab that dies without a word - crashed, killed, frozen - stops
// saying its sign-out is out, and within 6 s of the last time it did this tab takes it for gone;
// the writes go on once the sign-in check, read twice over `SIGN_OUT_SETTLE_MS`, still names the
// person both times.
test("a tab holding writes for another tab's sign-out resumes them once its lease lapses", async () => {
  // On the fake clock, so the lease and the settle after it are moved past in steps of
  // `STEP_MS` rather than waited out, and the time the writes resume is counted in those steps:
  // the steps are the only thing that moves the clock between the hold and the resume, with the
  // real turns each step needs for what it set going (`onRealTurns`), so no poll of `waitFor`,
  // whose count depends on how loaded the machine is, moves it (dispatch://INFRA-1505).
  jest.useFakeTimers();
  const app = renderApp({
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const signer = otherTab("tab-2");
  try {
    await screen.findByText(wholeText("Signed in as alice"));
    await waitFor(() =>
      expect(signer.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(attempts.length).toBeGreaterThan(0));

    signer.say({ signingOut: "alice" });
    const waiting = "CORE-1 set to done — Waiting for a sign-out in another tab";
    await onRealTurns(() => screen.queryByText(waiting) !== null);
    expect(screen.getByText(waiting)).toBeInTheDocument();
    const made = attempts.length;

    const STEP_MS = 100;
    let held = 0;
    while (attempts.length === made && held <= SIGN_OUT_SETTLE_MS + 7_500) {
      act(() => jest.advanceTimersByTime(STEP_MS));
      held += STEP_MS;
      await onRealTurns(() => attempts.length > made, 20);
    }
    expect(attempts.length).toBeGreaterThan(made);
    expect(held).toBeGreaterThanOrEqual(SIGN_OUT_SETTLE_MS + 5_500);
    expect(held).toBeLessThanOrEqual(SIGN_OUT_SETTLE_MS + 6_500);
    expect(screen.queryByText(/Waiting for a sign-out in another tab/)).not.toBeInTheDocument();
  } finally {
    realClock();
    signer.close();
    app.teardown();
  }
});
