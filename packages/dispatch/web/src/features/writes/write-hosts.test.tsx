import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { quietConsoleError } from "../../__tests__/act-warnings";
import { failingChunkComponent } from "../../__tests__/failing-chunk";
import { keepWrite, keyMinted } from "../../__tests__/kept-sends-harness";
import { ApiError, api } from "../../api/client";
import { chunkLoadsOut } from "../shell/chunk-loads";
import { KeptWritesHost, PendingWritesHost } from "./write-hosts";
import type { KeptWrite } from "./write-storage";
import { kindsRegistered, plainWrite, writesOf } from "./write-store";

// The store's take-up of what a reload kept waits for every kind of write, a chunk of its own
// (`kinds.ts`), and its own ask for that chunk can fail with no reload to follow: the page being
// left, the reader staying at the leave prompt that the writes the reload kept
// (`WriteStorage.load`) or a write pressed meanwhile open, or the tab's recovery reloads spent.
// The writes host asks for the same chunk once the page has drawn, and the evaluation of
// `kinds.ts` that its load brings ends every take-up still waiting (`kindsRegistered`), so the
// write pressed meanwhile goes out with no second press to ask again. The write is one the
// indicator does not show (`composer`): the host draws nothing, and the row reads the store alone.
// The tab keeps a write of alice's from before the reload, one the indicator does not show either,
// since a take-up with nothing kept for the person ends at once and waits for nothing: it goes
// out first, and the press behind it.
//
// Each run holds nothing over into the next, which `bun test --rerun-each` runs in the same
// process with the same modules (the repeated unit lane, dispatch-unit-repeat.yaml): `kinds.ts`
// is evaluated once in that process, so only the first run of a file sees its evaluation end the
// take-up. Every later run, and the second row below, finds `kinds.ts` evaluated before its
// take-up began, while the take-up's own ask is the row's and fails. The app has no such take-up:
// its ask is the host's own load (`loadWriteKinds`), answered from what is in once the kinds are,
// so in the app a take-up that begins after them ends through that ask. In these runs only the
// host's answered load ends it (`prefetchWriteKinds`), which keeps the row's end from turning on
// which load evaluated the module. The press is handled from the start, so the store's clear
// between rows (`__tests__/setup.ts`) rejecting a press a failed run left out reaches no one as an
// unhandled rejection. `kindsRegistered` in `finally` ends a take-up a failed run left waiting,
// and the send is restored only some turns after it, so that a write the take-up lets go meets
// the stub rather than the network guard of a later row or run.
async function takeUpEndsWithTheHost(): Promise<void> {
  const kept = keyMinted(Date.now());
  keepWrite(window.sessionStorage, kept, { presentation: "composer" });
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const send = spyOn(api, "send").mockResolvedValue(null as never);
  const writes = writesOf(client);
  let asked = 0;
  writes.resume("alice", () => {
    asked += 1;
    return Promise.reject(new TypeError("Failed to fetch dynamically imported module"));
  });
  const pressed = writes.run({
    detail: null,
    kind: plainWrite,
    label: "A comment on CORE-1",
    presentation: "composer",
    request: {
      body: { body: "On it" },
      method: "POST",
      path: "/api/v1/issues/CORE-1/comments",
    },
  });
  pressed.catch(() => undefined);
  let view: RenderResult | undefined;
  try {
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
    expect(asked).toBe(1);
    expect(send, "the take-up whose ask failed still holds the write").not.toHaveBeenCalled();

    view = render(
      <QueryClientProvider client={client}>
        <PendingWritesHost />
      </QueryClientProvider>
    );
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls.map(([request]) => request.path)).toEqual([
      "/api/v1/issues/CORE-1",
      "/api/v1/issues/CORE-1/comments",
    ]);
    expect(await pressed).toBeNull();
    expect(asked, "the take-up's own load was not asked for again").toBe(1);
  } finally {
    view?.unmount();
    kindsRegistered();
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
    send.mockRestore();
  }
}

test("the writes host's fetch of the kinds of write ends a take-up whose own ask failed, and the write pressed meanwhile goes out", async () => {
  await takeUpEndsWithTheHost();
});

// `kinds.ts` was evaluated before this take-up began - by the first row's host load when the
// file runs in order, or by this import when this row runs first or alone - so its evaluation
// ended the take-ups waiting at that moment, before this one waited. This take-up's own ask is the
// test's loader, which fails although the kinds are in, so only the writes host's answered load
// can end it.
// The import is a run-time one on purpose: a static import would evaluate `kinds.ts` as the file
// loads, before the first row, which could then no longer see an evaluation end its take-up.
test("a take-up whose own ask failed ends with the writes host's fetch also when another load brought the kinds in before it began", async () => {
  await import("./kinds");
  await takeUpEndsWithTheHost();
});

// The sign-in page's kept list asks for the kinds of write again when the browser comes back
// online, for a take-up of what a reload kept that still waits for them (`KeptWritesHost`): the
// reader presses no write on that page, and a press is what asks again elsewhere (`Writes.run`).
// In a unit run the host's own first ask cannot fail, because the kinds load from disk. So the
// load that failed is the take-up's own ask, the row's loader. The take-up begins only after the
// host's first ask has answered, so that answer cannot end it, and only a later ask of the host
// can. The session lapses before the browser comes back online, as on the sign-in page, so the
// take-up keeps the write it takes up for the sign-in that follows and sends nothing. No focus
// event comes in this row, so the `online` event alone asks again. `kindsRegistered` in `finally`
// ends a take-up that a failed run left waiting.
test("the sign-in page's kept list asks for the kinds of write again when the browser comes back online, which ends a take-up whose own ask failed", async () => {
  const kept = keyMinted(Date.now());
  keepWrite(window.sessionStorage, kept);
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const writes = writesOf(client);
  const view = render(
    <QueryClientProvider client={client}>
      <KeptWritesHost />
    </QueryClientProvider>
  );
  try {
    await waitFor(() =>
      expect(chunkLoadsOut(), "the host's first ask for the kinds is still out").toBe(0)
    );
    let asked = 0;
    const ask = Promise.withResolvers<never>();
    act(() => {
      writes.resume("alice", () => {
        asked += 1;
        return ask.promise;
      });
      writes.signedIn(undefined);
    });
    ask.reject(new TypeError("Failed to fetch dynamically imported module"));
    // The store's handler of the failed ask was attached first, so it has run once this ends.
    await ask.promise.catch(() => undefined);
    expect(asked).toBe(1);
    expect(writes.notTakenUp().map((record) => record.key)).toEqual([kept]);
    expect(writes.rows()).toEqual([]);

    act(() => {
      window.dispatchEvent(new Event("online"));
    });
    await waitFor(() =>
      expect(writes.notTakenUp(), "the take-up still waits for the kinds of write").toEqual([])
    );
    expect(writes.rows().map((row) => row.key)).toEqual([kept]);
    expect(asked, "the take-up's own load was not asked for again").toBe(1);
  } finally {
    view.unmount();
    kindsRegistered();
  }
});

/** A part a list draws in a write's row, in the place of the write's label, that throws: it
 *  stands for any fault of the list's own render. */
function Fault(): never {
  throw new Error("A row of the list failed to render");
}

/** A part of the same place whose chunk fails to download online: it throws into the
 *  indicator's render what the indicator's own `chunkComponent` throws when its chunk fails after
 *  a deploy. It stands for that chunk, which a row cannot fail: once any row of this process has
 *  drawn the indicator, its chunk is in, and `chunk` keeps it. This one keeps no failed download,
 *  so each mount asks again and fails again. The kept list holds no chunk: it is in the entry. */
const NotLoaded = failingChunkComponent();

/** Each list of writes, by its host: the name of the list's own region, and the name of the
 *  alert that stands in its place once its render has thrown. */
const lists = [
  ["pending-writes indicator", PendingWritesHost, "Pending changes", "the changes waiting to save"],
  ["sign-in page's kept list", KeptWritesHost, "Kept changes", "the changes this page kept"],
] as const;

/** Draws `Host` over a store of alice's, and presses one write that the indicator shows and
 *  Dispatch refuses, whose label is `label`: each list draws the label of a refused write, the
 *  kept list as a write of alice's that the page keeps. Answers the view. */
function drawARefusedWrite(Host: () => ReactNode, label: ReactNode): RenderResult {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const view = render(
    <QueryClientProvider client={client}>
      <Host />
    </QueryClientProvider>
  );
  act(() => {
    void writesOf(client)
      .run({
        detail: null,
        kind: plainWrite,
        // A label is text, which a row draws as it is; a part in its place draws in the list's
        // own render.
        label: label as unknown as string,
        method: "PATCH",
        presentation: "indicator",
        write: () => Promise.reject(new ApiError(422, { code: "KEY_REUSED", error: "refused" })),
      })
      .catch(() => undefined);
  });
  return view;
}

// A fault in a list's render draws an alert in the list's place that says which list could not
// be shown, with a Reload: never silence, since the indicator's rows are where a refused write
// offers Retry and Discard, and the sign-in page's list is the reader's one warning of the writes
// a sign-in as someone else drops. The reader did not open the list, so the alert takes no focus.
test.each(
  lists
)("the %s whose render throws draws an alert in its place", async (_list, Host, region, list) => {
  const logged = quietConsoleError();
  const view = drawARefusedWrite(Host, <Fault />);
  try {
    const alert = await screen.findByRole("alert", { name: `Could not show ${list}.` });
    expect(alert).toHaveTextContent(`Could not show ${list}.`);
    expect(within(alert).getByRole("button", { name: "Reload" })).toBeDefined();
    expect(screen.queryByRole("region", { name: region })).not.toBeInTheDocument();
    expect(document.body).toHaveFocus();
    expect(logged).toHaveBeenCalled();
  } finally {
    logged.mockRestore();
    view.unmount();
  }
});

// A chunk that fails to download online under the pending-writes indicator draws no alert, and
// never nothing: a line in the indicator's place, drawn from the store's rows with no chunk
// (`ChangesNotSent`), counts the changes not sent, says "Not saved" once Dispatch refuses one, and
// offers a Reload. The indicator is the one place a reader learns that a write is still out or was
// refused, so its failure is never silent. The line goes with the last write. Alice's write waits
// for her sign-out in another tab (`Writes.holdFor`), so it stays out, and the indicator names it
// by its label: the part that stands for the indicator's chunk draws there, and fails. Released,
// the write goes out and Dispatch refuses it, which draws the indicator afresh (`reset`), and the
// part fails again. The Discard of the refused write, which the indicator alone offers, is the
// store's here.
test("a pending-writes indicator whose chunk fails to download online draws a line in its place that counts the changes not sent, says when one is not saved, reloads, and goes with the last write", async () => {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const writes = writesOf(client);
  writes.holdFor("alice");
  const logged = quietConsoleError();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const view = render(
    <QueryClientProvider client={client}>
      <PendingWritesHost />
    </QueryClientProvider>
  );
  try {
    act(() => {
      void writes
        .run({
          detail: null,
          kind: plainWrite,
          // A label is text, which a row draws as it is; a part in its place draws in the
          // indicator's own render.
          label: (<NotLoaded />) as unknown as string,
          method: "PATCH",
          presentation: "indicator",
          write: () => Promise.reject(new ApiError(422, { code: "KEY_REUSED", error: "refused" })),
        })
        .catch(() => undefined);
    });
    const line = await screen.findByRole("status");
    expect(line).toHaveTextContent(/^1 change not sent$/u);
    expect(screen.queryByRole("region", { name: "Pending changes" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(logged).toHaveBeenCalled();

    act(() => writes.release("alice"));
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/^1 change not sent — Not saved$/u)
    );
    expect(screen.queryByRole("region", { name: "Pending changes" })).not.toBeInTheDocument();

    act(() => screen.getByRole("button", { name: "Reload" }).click());
    expect(reload).toHaveBeenCalledTimes(1);

    const [refused] = writes.rows();
    act(() => {
      expect(writes.discard(refused?.id ?? -1)).toBe(true);
    });
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Reload" })).not.toBeInTheDocument();
  } finally {
    reload.mockRestore();
    logged.mockRestore();
    view.unmount();
  }
});

// A page that loads signed out takes up nothing until someone signs in (`app.tsx`), so the
// sign-in page lists what the reload kept from each write's own record, with no kind of write in
// and no chunk: the list is in the entry. A record kept as refused reads "Not saved" and offers
// Discard, which drops the record from the tab's storage and from the store's records; one kept
// out shows by its label alone, and stays.
test("the sign-in page's kept list shows a write the reload kept as refused as not saved, and its Discard drops that record", async () => {
  const refused = keyMinted(Date.now());
  const out = keyMinted(Date.now() + 1);
  keepWrite(window.sessionStorage, refused, {
    state: { refusal: { kind: "stopped" }, status: "refused" },
  });
  keepWrite(window.sessionStorage, out, { label: "CORE-2 set to todo" });
  const client = new QueryClient();
  const view = render(
    <QueryClientProvider client={client}>
      <KeptWritesHost />
    </QueryClientProvider>
  );
  try {
    const kept = screen.getByRole("region", { name: "Kept changes" });
    expect(kept).toHaveTextContent("Kept until you sign in again as alice:");
    const row = kept.querySelector(`[data-kept-write="${refused}"]`);
    expect(row).toHaveTextContent("CORE-1 set to done — Not saved");
    expect(kept.querySelector(`[data-kept-write="${out}"]`)).toHaveTextContent(
      /^CORE-2 set to todo$/u
    );
    const [discard, ...more] = within(kept).getAllByRole("button", { name: "Discard" });
    expect(more, "the write kept out offers Discard").toEqual([]);
    expect(within(row as HTMLElement).getByRole("button", { name: "Discard" })).toBe(
      discard as HTMLElement
    );

    act(() => discard?.click());
    expect(window.sessionStorage.getItem(`dispatch.write:${refused}`)).toBeNull();
    const records = writesOf(client).notTakenUp();
    expect(records.map((write) => write.key)).toEqual([out]);
    expect(kept.querySelector(`[data-kept-write="${refused}"]`)).toBeNull();
    expect(kept).toHaveTextContent("CORE-2 set to todo");
    expect(window.sessionStorage.getItem(`dispatch.write:${out}`)).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// Alice's write is out as her session lapses, and the sign-in page's kept list shows it by its
// label alone, as a write still on its way: a lapse keeps every write, and the attempt in flight
// still answers for it. Dispatch then refuses that attempt while the list is drawn. The row turns
// to the refused form at once, with no reload: its label, "Not saved", and Discard, because only
// alice can drop it. The list draws from the store's rows, so the refusal reaches it there.
test("a write Dispatch refuses while the sign-in page's kept list is drawn turns there to not saved, with its Discard", async () => {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const writes = writesOf(client);
  const answer = Promise.withResolvers<never>();
  const pressed = writes
    .run({
      detail: null,
      kind: plainWrite,
      label: "CORE-1 set to done",
      method: "PATCH",
      presentation: "indicator",
      write: () => answer.promise,
    })
    .catch((error: unknown) => error);
  writes.signedIn(undefined);
  const view = render(
    <QueryClientProvider client={client}>
      <KeptWritesHost />
    </QueryClientProvider>
  );
  try {
    const [write] = writes.rows();
    const kept = screen.getByRole("region", { name: "Kept changes" });
    const row = () => kept.querySelector(`[data-kept-write="${write?.key}"]`);
    expect(row()).toHaveTextContent(/^CORE-1 set to done$/u);
    expect(within(kept).queryByRole("button", { name: "Discard" })).not.toBeInTheDocument();

    await act(async () => {
      answer.reject(new ApiError(422, { code: "KEY_REUSED", error: "refused" }));
      await pressed;
    });
    expect(row()).toHaveTextContent("CORE-1 set to done — Not saved");
    expect(within(row() as HTMLElement).getByRole("button", { name: "Discard" })).toBeVisible();
  } finally {
    view.unmount();
  }
});

// `KeptWrites` draws kept records that the store has not taken up beside the store's own rows, so
// `KeptWritesHost`'s boundary reset covers both: once a faulty record leaves, the list draws the
// remaining kept records again without waiting for a store row to change.
test("the sign-in page's kept list draws again after a render throw once its kept records change", async () => {
  const bad = keyMinted(Date.now());
  const good = keyMinted(Date.now() + 1);
  keepWrite(window.sessionStorage, bad, {
    label: "CORE-1 set to done",
    state: { refusal: { kind: "stopped" }, status: "refused" },
  });
  keepWrite(window.sessionStorage, good, { label: "CORE-2 set to todo" });
  const client = new QueryClient();
  const writes = writesOf(client);
  const readReal = writes.notTakenUp;
  const mapped = new WeakMap<readonly KeptWrite[], readonly KeptWrite[]>();
  const spy = spyOn(writes, "notTakenUp").mockImplementation(() => {
    const real = readReal();
    const cached = mapped.get(real);
    if (cached !== undefined) return cached;
    const next = real.map((record) =>
      record.key === bad ? { ...record, label: (<Fault />) as unknown as string } : record
    );
    mapped.set(real, next);
    return next;
  });
  const logged = quietConsoleError();
  const view = render(
    <QueryClientProvider client={client}>
      <KeptWritesHost />
    </QueryClientProvider>
  );
  try {
    const alert = await screen.findByRole("alert", {
      name: "Could not show the changes this page kept.",
    });
    expect(alert).toBeVisible();
    expect(screen.queryByRole("region", { name: "Kept changes" })).not.toBeInTheDocument();
    expect(logged).toHaveBeenCalled();

    act(() => {
      expect(writes.discardKept(bad)).toBe(true);
    });
    const kept = await screen.findByRole("region", { name: "Kept changes" });
    expect(kept).toHaveTextContent("CORE-2 set to todo");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    spy.mockRestore();
    logged.mockRestore();
    view.unmount();
  }
});

// Discarding the last row of the sign-in page's kept list removes the list from the page, so
// focus moves to the sign-in page's link instead of dropping to the page body.
test("Discard on the last row of the sign-in page's kept list moves focus to the sign-in link", async () => {
  const user = userEvent.setup();
  const refused = keyMinted(Date.now());
  keepWrite(window.sessionStorage, refused, {
    label: "CORE-1 set to done",
    state: { refusal: { kind: "stopped" }, status: "refused" },
  });
  const client = new QueryClient();
  const view = render(
    <QueryClientProvider client={client}>
      <main>
        <section>
          <a href="/auth/start">Sign in with Google</a>
          <KeptWritesHost />
        </section>
      </main>
    </QueryClientProvider>
  );
  try {
    const signIn = screen.getByRole("link", { name: "Sign in with Google" });
    const kept = screen.getByRole("region", { name: "Kept changes" });
    const discard = within(kept).getByRole("button", { name: "Discard" });
    act(() => discard.focus());
    expect(discard).toHaveFocus();

    await user.keyboard("{Enter}");
    expect(screen.queryByRole("region", { name: "Kept changes" })).not.toBeInTheDocument();
    expect(
      signIn,
      "Discard on the last row left focus on the body instead of the sign-in link"
    ).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// A Discard pressed with the pointer hands the focus on to no other control. Chromium focuses a
// button that the pointer presses and draws it no focus ring, so a hand-off would put the focus,
// unseen, on the next row's Discard, and the next Space or Enter would discard that write too.
// happy-dom matches `:focus-visible` on every focused element, so this row makes the pressed
// button answer as Chromium does after a pointer press.
test("Discard pressed with the pointer on the sign-in page's kept list moves no focus to the next Discard", async () => {
  const user = userEvent.setup();
  const first = keyMinted(Date.now());
  const second = keyMinted(Date.now() + 1);
  for (const [key, label] of [
    [first, "CORE-1 set to done"],
    [second, "CORE-2 set to todo"],
  ] as const) {
    keepWrite(window.sessionStorage, key, {
      label,
      state: { refusal: { kind: "stopped" }, status: "refused" },
    });
  }
  const client = new QueryClient();
  const view = render(
    <QueryClientProvider client={client}>
      <KeptWritesHost />
    </QueryClientProvider>
  );
  try {
    const kept = screen.getByRole("region", { name: "Kept changes" });
    const row = kept.querySelector(`[data-kept-write="${first}"]`) as HTMLElement;
    const discard = within(row).getByRole("button", { name: "Discard" });
    const matches = discard.matches.bind(discard);
    spyOn(discard, "matches").mockImplementation(
      (selectors: string) => selectors !== ":focus-visible" && matches(selectors)
    );

    await user.click(discard);
    expect(kept.querySelector(`[data-kept-write="${first}"]`)).toBeNull();
    const next = within(kept).getByRole("button", { name: "Discard" });
    expect(
      next,
      "a Discard pressed with the pointer moved the focus to the next Discard"
    ).not.toHaveFocus();
  } finally {
    view.unmount();
  }
});
