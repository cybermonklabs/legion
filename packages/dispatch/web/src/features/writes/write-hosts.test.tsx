import { expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ComponentType, ReactNode } from "react";

import { quietConsoleError } from "../../__tests__/act-warnings";
import { settle } from "../../__tests__/fake-clock";
import { keepWrite, keyMinted } from "../../__tests__/kept-sends-harness";
import { withOnLine } from "../../__tests__/on-line";
import { ApiError, api, type WriteRequest } from "../../api/client";
import { chunk } from "../shell/chunk-component";
import { chunkLoadsOut } from "../shell/chunk-loads";
import {
  KeptWritesHost,
  loadWriteKinds,
  PendingWritesHost,
  pendingWritesHost,
} from "./write-hosts";
import type { KeptWrite } from "./write-storage";
import { kindsRegistered, plainWrite, type Writes, writesOf } from "./write-store";

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

// The kinds of write are a chunk of their own (`kinds.ts`), so a write a reload kept finds its
// kind there, whichever page the reader reloads on. The priorities page's kinds are in that chunk
// too (`priority-writes.ts`). This file imports no priorities module, so only the chunk registers
// them here. A new priority that was out as the page went away is taken up once the host's load of
// the chunk answers (`loadWriteKinds`), and goes out again under its key. Without its kind, the
// take-up would drop it unsent (`Writes.resume`). `kindsRegistered` in `finally` ends a take-up
// that a failed run left waiting, and the send is restored some turns after it, as above.
test("a new priority a reload kept goes out through the kinds chunk the host loads, on a page that imports no priorities module", async () => {
  const kept = keyMinted(Date.now());
  const request: WriteRequest = {
    body: { title: "Quarterly goals" },
    method: "POST",
    path: "/api/v1/priorities",
  };
  keepWrite(window.sessionStorage, kept, {
    detail: "Quarterly goals",
    kind: "priority-new",
    label: "New priority “Quarterly goals”",
    request,
  });
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const send = spyOn(api, "send").mockResolvedValue(null as never);
  const writes = writesOf(client);
  try {
    writes.resume("alice", loadWriteKinds);
    await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
    expect(send.mock.calls[0]?.[0]).toMatchObject(request);
    expect(send.mock.calls[0]?.[1]?.key).toBe(kept);
    await waitFor(() => expect(writes.rows()).toEqual([]));
    expect(writes.notTakenUp()).toEqual([]);
  } finally {
    kindsRegistered();
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
    send.mockRestore();
  }
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

/** Each list of writes, by its host: the name of the list's own region, and the name of the
 *  alert that stands in its place once its render has thrown. */
const lists = [
  ["pending-writes indicator", PendingWritesHost, "Pending changes", "the changes waiting to save"],
  ["sign-in page's kept list", KeptWritesHost, "Kept changes", "the changes this page kept"],
] as const;

/** Presses one write that the indicator shows and Dispatch refuses, whose label is `label`. */
function pressARefusedWrite(writes: Writes, label: ReactNode): void {
  act(() => {
    void writes
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
}

/** Draws `Host` over a store of alice's, and presses one write that the indicator shows and
 *  Dispatch refuses, whose label is `label` (`pressARefusedWrite`): each list draws the label of
 *  a refused write, the kept list as a write of alice's that the page keeps. With `held`, the
 *  write waits for alice's sign-out in another tab (`Writes.holdFor`), so it stays out until the
 *  row releases it. Answers the view and the store. */
function drawARefusedWrite(
  Host: () => ReactNode,
  label: ReactNode,
  { held = false } = {}
): { view: RenderResult; writes: Writes } {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const writes = writesOf(client);
  if (held) writes.holdFor("alice");
  const view = render(
    <QueryClientProvider client={client}>
      <Host />
    </QueryClientProvider>
  );
  pressARefusedWrite(writes, label);
  return { view, writes };
}

/** One download of the indicator's chunk, for a row to fail, hold or answer. */
type Download = ReturnType<typeof Promise.withResolvers<ComponentType<object>>>;

/**
 * The indicator's chunk as a row's own: a host of the indicator over it (`pendingWritesHost`),
 * and each download of the chunk in the order the host and the indicator ask for them. The
 * shell's own chunk cannot stand for it: once a row has drawn the indicator, that chunk is in,
 * and `chunk` keeps it. `end` fails every download still out, and each later one as it is asked
 * for, so that a row that fails leaves none out: a download out under `renderChunk` stays counted
 * for the later rows of this file (`chunkLoadsOut`), and an indicator whose download failed
 * offline asks again once the browser is back online (`importWhenOnline`).
 */
function indicatorChunk(): { downloads: Download[]; end: () => void; Host: () => ReactNode } {
  const downloads: Download[] = [];
  let ended = false;
  const end = () => {
    ended = true;
    for (const download of downloads) {
      download.reject(new TypeError("Failed to fetch dynamically imported module"));
    }
  };
  const Host = pendingWritesHost(
    chunk<object>(() => {
      const download = Promise.withResolvers<ComponentType<object>>();
      downloads.push(download);
      if (ended) download.reject(new TypeError("Failed to fetch dynamically imported module"));
      return download.promise;
    })
  );
  return { downloads, end, Host };
}

/** Fails `download` as a deploy fails the indicator's chunk, and lets what that sets draw. */
async function failDownload(download: Download | undefined): Promise<void> {
  await act(async () => {
    download?.reject(new TypeError("Failed to fetch dynamically imported module"));
    await settle();
  });
}

/** Answers `download` with the indicator itself, from the module the shell's chunk loads. The
 *  import is a run-time one, as the shell's is: a static one would load the module before the
 *  file's rows begin, and the cold-load lane would no longer hold the shell's own load of it
 *  (`hold-lazy-loads.ts`). */
async function drawTheIndicator(download: Download | undefined): Promise<void> {
  const { PendingWritesIndicator } = await import("./PendingWritesIndicator");
  await act(async () => {
    download?.resolve(PendingWritesIndicator);
    await settle();
  });
}

/**
 * Draws a host of the indicator over a chunk of the row's own (`indicatorChunk`), presses one
 * write that the indicator shows and Dispatch refuses, held for alice's sign-out in another tab
 * so it stays out (`drawARefusedWrite`), and fails online the download the indicator mounts on:
 * the host asks for the chunk once it has drawn, and the indicator joins that download as the
 * write draws it. React reports the failure the indicator's boundary caught on the console, which
 * stays quiet for the row (`logged`). Answers once the line in the indicator's place counts that
 * write, with the chunk's downloads and `end`, the console's spy, the view and the store; a step
 * that fails here unmounts the view and ends the downloads first.
 */
async function failTheIndicatorOverAHeldWrite(): Promise<{
  downloads: Download[];
  end: () => void;
  logged: Mock<typeof console.error>;
  view: RenderResult;
  writes: Writes;
}> {
  const { downloads, end, Host } = indicatorChunk();
  const logged = quietConsoleError();
  const { view, writes } = drawARefusedWrite(Host, "CORE-1 set to done", { held: true });
  try {
    expect(downloads, "the host and the indicator did not share one download").toHaveLength(1);
    await failDownload(downloads[0]);
    expect(await screen.findByRole("status")).toHaveTextContent(/^1 change not sent$/u);
  } catch (error) {
    view.unmount();
    end();
    throw error;
  }
  return { downloads, end, logged, view, writes };
}

// A fault in a list's render draws an alert in the list's place that says which list could not
// be shown, with a Reload: never silence, since the indicator's rows are where a refused write
// offers Retry and Discard, and the sign-in page's list is the reader's one warning of the writes
// a sign-in as someone else drops. The reader did not open the list, so the alert takes no focus.
test.each(lists)(
  "the %s whose render throws draws an alert in its place",
  async (_list, Host, region, list) => {
    const logged = quietConsoleError();
    const { view } = drawARefusedWrite(Host, <Fault />);
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
  }
);

// A chunk that fails to download online under the pending-writes indicator draws no alert, and
// never nothing: a line in the indicator's place, drawn from the store's rows with no chunk
// (`ChangesNotSent`), counts the changes not sent, says "Not saved" once Dispatch refuses one, and
// offers a Reload. The indicator is the one place a reader learns that a write is still out or was
// refused, so its failure is never silent. The line goes with the last write. Alice's write waits
// for her sign-out in another tab, so it stays out while the indicator's chunk fails
// (`failTheIndicatorOverAHeldWrite`). Released, the write goes out and Dispatch refuses it, which
// draws the indicator afresh (`reset`), and its chunk fails again. The Discard of the refused
// write, which the indicator alone offers, is the store's here.
test("a pending-writes indicator whose chunk fails to download online draws a line in its place that counts the changes not sent, says when one is not saved, reloads, and goes with the last write", async () => {
  const { downloads, end, logged, view, writes } = await failTheIndicatorOverAHeldWrite();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  try {
    expect(screen.queryByRole("region", { name: "Pending changes" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(logged).toHaveBeenCalled();

    act(() => writes.release("alice"));
    await waitFor(() => expect(downloads).toHaveLength(2));
    await failDownload(downloads[1]);
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
    end();
  }
});

// A conflict a page mounted now settles with the reader is that page's (`Writes.claim`), and the
// indicator leaves it out (`indicatorWrites`). The line that stands in the indicator's place while
// its chunk cannot load reads the same writes (`ChangesNotSent`), so it leaves that conflict out
// too: it does not count it, and it does not say "Not saved" for it. Once the page hands the
// conflict back (`unclaim`), the line counts it and says "Not saved". The conflict here is a text
// the page claims with no write of it sent (`Writes.claimUnsent`), so nothing goes out. Alice's
// other write waits for her sign-out in another tab, so it stays out while the indicator's chunk
// fails (`failTheIndicatorOverAHeldWrite`), and the chunk fails again as the page hands the
// conflict back.
test("the line in a pending-writes indicator's place leaves out a conflict a page holds, and counts it once the page hands it back", async () => {
  const { downloads, end, logged, view, writes } = await failTheIndicatorOverAHeldWrite();
  try {
    let claimed = -1;
    act(() => {
      claimed = writes.claimUnsent({
        detail: null,
        kind: plainWrite,
        label: "Rename the priority “P1”",
        refusal: new ApiError(409, { code: "PRIORITY_CHANGED", error: "the title was written" }),
        request: { body: { title: "My title" }, method: "PATCH", path: "/api/v1/priorities/P1" },
      });
    });
    expect(writes.rows().map((row) => row.status)).toEqual(["queued", "refused"]);
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/^1 change not sent$/u)
    );

    act(() => writes.unclaim(claimed));
    await waitFor(() => expect(downloads).toHaveLength(2));
    await failDownload(downloads[1]);
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/^2 changes not sent — Not saved$/u)
    );
    expect(screen.queryByRole("region", { name: "Pending changes" })).not.toBeInTheDocument();
  } finally {
    logged.mockRestore();
    view.unmount();
    end();
  }
});

// Once the indicator's chunk has failed to download online, the line stays in the indicator's
// place through every later ask of the chunk, until the chunk draws the indicator
// (`pendingWritesHost`). A write pressed after the failure draws the indicator afresh
// (`reset`), which asks for the chunk again, and the row holds that download. The line counts the
// new write while the download is out, and once it comes down the indicator takes the line's
// place. With nothing drawn meanwhile, the corner would be empty for as long as the download took.
test("the line in a pending-writes indicator's place stays, and counts a write pressed after the chunk failed online, while the next download of the chunk is out, and the indicator takes its place once that download lands", async () => {
  const { downloads, end, logged, view, writes } = await failTheIndicatorOverAHeldWrite();
  try {
    pressARefusedWrite(writes, "CORE-2 set to done");
    expect(downloads, "the write pressed after the failure did not ask for the chunk").toHaveLength(
      2
    );
    expect(
      screen.queryByRole("status")?.textContent,
      "the line left the indicator's place while the next download was out"
    ).toBe("2 changes not sent");
    expect(screen.queryByRole("region", { name: "Pending changes" })).not.toBeInTheDocument();

    await drawTheIndicator(downloads[1]);
    const indicator = await screen.findByRole("region", { name: "Pending changes" });
    expect(indicator).toHaveTextContent(
      "CORE-2 set to done — Waiting for a sign-out in another tab"
    );
    expect(screen.queryByText(/\bnot sent\b/u)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Reload" })).not.toBeInTheDocument();
  } finally {
    logged.mockRestore();
    view.unmount();
    end();
  }
});

// The reader's focus is on the Reload of the line in the indicator's place when a write pressed
// after the chunk failed online draws the indicator afresh (`reset`). The boundary then swaps what
// it drew for the failure (`NotLoaded`) for the indicator asking for its chunk again, and the row
// holds that download, so the indicator's loading fallback stays drawn (`IndicatorLoading`). The
// host draws the line itself, at one place apart from both (`pendingWritesHost`), so the line
// stays mounted through the swap and counts the new write, and its Reload is the same button, the
// focus still on it. A line drawn by either would be drawn afresh, and the focus would drop to the
// page's body.
test("the Reload of the line in a pending-writes indicator's place keeps the focus while a write pressed after the chunk failed online asks for the chunk again", async () => {
  const { downloads, end, logged, view, writes } = await failTheIndicatorOverAHeldWrite();
  try {
    const reload = screen.getByRole("button", { name: "Reload" });
    act(() => reload.focus());
    expect(reload).toHaveFocus();

    pressARefusedWrite(writes, "CORE-2 set to done");
    expect(downloads, "the write pressed after the failure did not ask for the chunk").toHaveLength(
      2
    );
    expect(
      screen.queryByRole("status")?.textContent,
      "the line left the indicator's place while the next download was out"
    ).toBe("2 changes not sent");
    expect(
      document.activeElement,
      "the focus left the line's Reload as the chunk was asked for again"
    ).toBe(reload);
  } finally {
    logged.mockRestore();
    view.unmount();
    end();
  }
});

// The same failure online, and then the browser goes offline, and Dispatch refuses the held write
// as it goes out. The refusal draws the indicator afresh (`reset`), and its ask for the chunk
// fails offline, so it waits for the network (`importWhenOnline`). The line stays in the
// indicator's place all the while, and says that the change is not saved (`pendingWritesHost`):
// an outage is when the reader needs it most. Once the browser is back online the indicator asks
// again, and draws the refusal, with its Retry and Discard, as that download lands.
test("the line in a pending-writes indicator's place stays while the browser is offline after the chunk failed online, and says the change is not saved once Dispatch refuses it", async () => {
  const { downloads, end, logged, view, writes } = await failTheIndicatorOverAHeldWrite();
  try {
    await withOnLine(false, async () => {
      act(() => writes.release("alice"));
      await waitFor(() => expect(downloads).toHaveLength(2));
      await failDownload(downloads[1]);
      await waitFor(() =>
        expect(
          screen.queryByRole("status")?.textContent,
          "the line left the indicator's place while the browser was offline"
        ).toBe("1 change not sent — Not saved")
      );
      expect(downloads, "the indicator asked for its chunk again while offline").toHaveLength(2);
      expect(screen.queryByRole("region", { name: "Pending changes" })).not.toBeInTheDocument();
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    });
    await act(async () => {
      window.dispatchEvent(new Event("online"));
      await settle();
    });
    expect(downloads, "the indicator did not ask again once back online").toHaveLength(3);
    await drawTheIndicator(downloads[2]);
    const indicator = await screen.findByRole("region", { name: "Pending changes" });
    expect(indicator).toHaveTextContent("Not saved: CORE-1 set to done");
    expect(within(indicator).getByRole("button", { name: "Retry" })).toBeVisible();
    expect(screen.queryByText(/\bnot sent\b/u)).not.toBeInTheDocument();
  } finally {
    logged.mockRestore();
    view.unmount();
    end();
    window.dispatchEvent(new Event("online"));
  }
});

// A kept record whose kinds of write failed to download online stays counted on the line in the
// indicator's place (`ChangesNotSent`) even once a live indicator write mounts the indicator from
// a loaded chunk, and after that live write settles and the indicator draws nothing (its host
// keeps the indicator mounted from the first row on, `reset !== ""`): mounting the indicator must
// never hide the kept record for the life of the page. Once the kinds land and take the record
// up, the line goes.
test("the line counting a kept record while kinds of write failed stays drawn beside a loaded indicator, after its write settles, and goes once the kinds land", async () => {
  const kept = keyMinted(Date.now());
  keepWrite(window.sessionStorage, kept, { presentation: "composer" });
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const send = spyOn(api, "send").mockResolvedValue(null as never);
  const writes = writesOf(client);
  const { downloads, end, Host } = indicatorChunk();
  const view = render(
    <QueryClientProvider client={client}>
      <Host />
    </QueryClientProvider>
  );
  try {
    await waitFor(() => expect(downloads).toHaveLength(1));
    await drawTheIndicator(downloads[0]);
    // The host's initial prefetch of the kinds has settled before this take-up begins, so only
    // the explicit `kindsRegistered()` below ends it.
    await waitFor(() => expect(chunkLoadsOut()).toBe(0));

    act(() => {
      writes.resume("alice", () =>
        Promise.reject(new TypeError("Failed to fetch dynamically imported module"))
      );
    });
    expect(await screen.findByText("1 change not sent")).toBeVisible();

    // A live indicator write mounts the already-loaded indicator beside the line; the take-up
    // holds it queued (`Waiting to send`), so Discard settles it while the kinds stay out.
    act(() => {
      writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "CORE-1 set to done",
          presentation: "indicator",
          request: { body: { status: "done" }, method: "PATCH", path: "/api/v1/issues/CORE-1" },
        })
        .catch(() => undefined);
    });
    const indicator = await screen.findByRole("region", { name: "Pending changes" });
    expect(indicator).toHaveTextContent("CORE-1 set to done — Waiting to send");
    expect(
      screen.getByText("1 change not sent"),
      "mounting the indicator hid the kept record waiting on failed kinds"
    ).toBeVisible();

    await userEvent.setup().click(within(indicator).getByRole("button", { name: "Discard" }));
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Pending changes" })).not.toBeInTheDocument()
    );
    expect(
      screen.getByText("1 change not sent"),
      "settling the indicator's write hid the kept record waiting on failed kinds"
    ).toBeVisible();

    act(() => kindsRegistered());
    await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.queryByText(/\bnot sent\b/u)).not.toBeInTheDocument());
  } finally {
    view.unmount();
    end();
    kindsRegistered();
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
    send.mockRestore();
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
