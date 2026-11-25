import { afterEach, expect, spyOn, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { jump, onFakeClock, settle, tick } from "../../__tests__/fake-clock";
import { answerPriorityPatches, PRIORITY_TITLE_CHANGED } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { Priorities, Priority } from "../../api/types";
import { FIRST_BACKOFF_MS, SEND_DEADLINE_MS } from "../writes/keyed-write";
import { ofKind, type Write, writesOf } from "../writes/write-store";
import {
  aheadOf,
  expectedText,
  pressedText,
  priorityTextWrite,
  settledText,
  type TextChain,
  usePriorityWrites,
} from "./priority-writes";

/** The id the next press a test makes is given. */
let fresh = 0;

/** A press as the hook makes it, where the field last took `from` in from the server and the page
 *  draws the read of the priorities that was the `drawn`th to answer: what the write names as
 *  `expected`, the press, and the chain after it. */
function press(chain: TextChain | undefined, text: string, from: string, drawn = 0) {
  fresh += 1;
  const ahead = aheadOf(chain, from, drawn);
  const expected = expectedText(ahead, from);
  return { chain: pressedText(ahead, fresh, text, expected), expected, press: fresh };
}

test("the first write names the text the field last took in", () => {
  expect(press(undefined, "One", "Old").expected).toBe("Old");
});

// A field takes no text in as it writes: while its writes go on it still holds the text before
// them all, and the server holds the newest of them by the next one's turn.
test("a write pressed while one is out names the text of the one out, which the server holds by its turn", () => {
  const first = press(undefined, "One", "Old");
  const second = press(first.chain, "Two", "Old");
  expect(second.expected).toBe("One");
  // And a third, the second still out after the first landed, names the second's.
  expect(press(settledText(second.chain, first.press, 0), "Three", "Old").expected).toBe("Two");
});

// A write that landed is the server's text until a read that answered after it is drawn: the
// field then took that read's text in, which is the write's own or a later one.
test("once a write lands, the next names its text until a read answered since is drawn, and then the text the field took in from it", () => {
  const first = press(undefined, "One", "Old");
  const landed = settledText(first.chain, first.press, 1);
  expect(press(landed, "Two", "Old", 1).expected).toBe("One");
  expect(press(landed, "Two", "One", 2).expected).toBe("One");
  // Another person set it back after it landed, and the read showed that.
  expect(press(landed, "Two", "Old", 2).expected).toBe("Old");
});

test("once an edit starts from another person's text, its write names that text", () => {
  const first = press(undefined, "One", "Old");
  const landed = settledText(first.chain, first.press, 1);
  expect(press(landed, "Two", "Written elsewhere", 1).expected).toBe("Written elsewhere");
});

// Another person's text reached the field, which took it in, while "One" was out: the server
// holds it once "One" is answered, whichever came first, so a write naming "One" would be refused.
test("an edit started from another person's text while a write is out names that text, and the next goes after it", () => {
  const first = press(undefined, "One", "Old");
  const over = press(first.chain, "Mine", "Written elsewhere");
  expect(over.expected).toBe("Written elsewhere");
  expect(press(over.chain, "Mine, again", "Written elsewhere").expected).toBe("Mine");
});

// A write that ends any way but landing is never named, whether or not it landed: where it did,
// a read brings its text to the field.
test("a write that ends any way but landing leaves the chain", () => {
  const first = press(undefined, "One", "Old");
  const ended = settledText(first.chain, first.press);
  expect(ended).toEqual([]);
  expect(press(ended, "Two", "Old").expected).toBe("Old");
  // A write after it, pressed while it was out, went over its text, which the server never took:
  // it goes behind the field's text too.
  const second = press(first.chain, "Two", "Old");
  expect(press(settledText(second.chain, first.press), "Three", "Old").expected).toBe("Old");
});

const BODY: Priorities = {
  priorities: [],
  work_under_no_priority: { by_project: null, count: 0, issues: null },
  priority_work_in_backlog_or_icebox: { by_priority: null, count: 0, issues: null },
};

const SAVED: Priority = {
  id: "p1",
  title: "One",
  description: "",
  owner: "",
  rank: "n",
  state: "live",
  made_at: "2026-11-10T09:00:00Z",
  made_by: { kind: "user", id: "alice" },
};

/** A read of the priorities where the server holds `title` as `p1`'s title. */
function showing(title: string): Priorities {
  const work = {
    by_status: null,
    claimed: 0,
    closed_last_7_days: 0,
    newest_event_at: null,
    open_asks: 0,
  };
  return { ...BODY, priorities: [{ ...SAVED, title, work }] };
}

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

/**
 * A page's writes of priority `p1`'s title, beside the page's read of the priorities: each write
 * and each read is answered only when the test says, in the order it went out. `sent` is what
 * each write that went out changed. The page shows the priorities `shown` names: the conflicts of
 * those are its to settle with the reader. A read is fresh until something reads the priorities
 * again, so a page drawn later reads nothing on its own, and every read counted is one a write
 * or a claim started. The page's client is `Client`: TanStack's own, which cancels a read still
 * out for one a later refresh starts, or the app's (`DispatchQueryClient`), which lets that read
 * answer and then reads again.
 */
function pageWrites(shown: readonly string[] = [], Client: typeof QueryClient = QueryClient) {
  const whoami = { event_head: "0", kind: "user" as const, login: "alice" };
  const viewer = spyOn(api, "whoAmI").mockResolvedValue(whoami);
  const reads: { resolve: (body: Priorities) => void }[] = [];
  const listing = spyOn(api, "listPriorities").mockImplementation(() => {
    const read = Promise.withResolvers<Priorities>();
    reads.push(read);
    return read.promise;
  });
  const answers: { reject: (refusal: Error) => void; resolve: (saved: Priority) => void }[] = [];
  const patches = answerPriorityPatches().mockImplementation(() => {
    const answer = Promise.withResolvers<Priority>();
    answers.push(answer);
    return answer.promise;
  });
  const newClient = () => {
    const made = new Client({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
    made.setQueryData(["whoami"], whoami);
    return made;
  };
  const client = newClient();
  /** Draws a page showing `ids`, beside its read of the priorities, on the app of `on`. */
  const draw = (ids: readonly string[], on: QueryClient = client) =>
    renderHook(
      () => {
        useQuery(prioritiesQuery());
        return usePriorityWrites(ids);
      },
      {
        wrapper: ({ children }: { children: ReactNode }) =>
          createElement(QueryClientProvider, { client: on }, children),
      }
    );
  const hook = draw(shown);
  const others: { unmount: () => void }[] = [];
  let drawn = true;
  restores.push(() => {
    if (drawn) hook.unmount();
    for (const other of others) other.unmount();
    client.clear();
    for (const spy of [viewer, listing, patches]) spy.mockRestore();
  });
  const sent = () => patches.mock.calls.map(([, input]) => input);
  /** Lands the attempt that went out `index`th, or refuses it: `true` as another person's text
   *  refuses it (`409 PRIORITY_CHANGED`), and an error as that error does. */
  const answer = async (index: number, refused: boolean | ApiError) => {
    await waitFor(() => expect(answers.length).toBeGreaterThan(index));
    await act(async () => {
      if (refused === false) answers[index]?.resolve(SAVED);
      else answers[index]?.reject(refused === true ? PRIORITY_TITLE_CHANGED : refused);
      await settle();
    });
  };
  /** Draws another page, which shows `ids`, on the app of `on`: the same app, unless given. */
  const drawAnother = (ids: readonly string[], on: QueryClient = client) => {
    const other = draw(ids, on);
    others.push(other);
    return other;
  };
  /** A new app over the tab's storage, which takes up the writes it kept (`Writes.resume`), as
   *  the app does once signed in. */
  const reloaded = () => {
    const after = newClient();
    restores.push(() => after.clear());
    const store = writesOf(after);
    act(() => store.resume("alice"));
    return { after, store };
  };
  return {
    answer,
    client,
    /** The page's texts kept beside `p1`'s title, where it reads `held`. */
    kept: (held: string) => hook.result.current.kept("p1", "title", held),
    /** The field keeps `texts`, newest first, beside `p1`'s title. */
    keep: (texts: readonly string[]) => act(() => hook.result.current.keep("p1", "title", texts)),
    drawAnother,
    /** How many times the page has read the priorities. */
    readCount: () => reads.length,
    /** Takes the page away. */
    undraw: () => {
      drawn = false;
      hook.unmount();
    },
    /** Lets the write out go past its deadline, on the fake clock the test holds, and stops it as
     *  the reader's Stop trying does: its attempt in flight, the `index`th to go out, fails, and
     *  the write ends with no verdict. Answers the stopped write as the store holds it. */
    stopLate: async (index: number) => {
      const writes = writesOf(client);
      await tick(SEND_DEADLINE_MS + 1_000);
      const late = writes.rows().find((row) => row.status === "late");
      expect(late?.status).toBe("late");
      act(() => {
        writes.stop(late?.id ?? 0);
      });
      await answer(index, UNAVAILABLE);
      const stopped = writes.rows().find((row) => row.id === late?.id);
      expect(stopped?.status).toBe("refused");
      return stopped;
    },
    /** Answers the page's `index`th read of the priorities, its first being the page's own: with
     *  `title` as the title of `p1`, where given, and else with no priority at all. */
    read: async (index: number, title?: string) => {
      await waitFor(() => expect(reads.length).toBeGreaterThan(index));
      await act(async () => {
        reads[index]?.resolve(title === undefined ? BODY : showing(title));
        await settle();
      });
    },
    /** The tab reloads: a new app, over the tab's storage, takes up the writes it kept
     *  (`Writes.resume`), as the app does once signed in. Answers the new app's store. */
    reload: () => reloaded().store,
    /** The tab reloads (`reload`), and the new app draws a page that shows `ids`. Answers the new
     *  app's store, and that page. */
    reloadShowing: (ids: readonly string[]) => {
      const { after, store } = reloaded();
      return { page: drawAnother(ids, after), store };
    },
    /** The reader writes `title` as priority `id`'s, where its field last took `from` in from the
     *  server - the text it was drawn with, or the last a read brought it - and made the edit on
     *  `on`: `from`, unless given, as a field drawn over `from` holds it. */
    rename: (title: string, from: string, on = from, id = "p1") =>
      act(() => {
        void hook.result.current.rename(id, title, from, on, "Rename the priority");
      }),
    /** What the `index`th write to go out changed, once it went. */
    sentAt: async (index: number) => {
      await waitFor(() => expect(sent().length).toBeGreaterThan(index));
      return sent()[index];
    },
    /** How many writes have gone out. */
    sentCount: () => sent().length,
  };
}

test("a write pressed over another person's text names it, and a write still out before it is no write it goes after", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0);
  rename("One", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "One" });
  // Another person's "Bob" reaches the field, which takes it in, before "One" is answered; the
  // reader writes over it.
  rename("Mine", "Bob");
  // "One" is refused, since the server holds "Bob": that is what "Mine" names.
  await answer(0, true);
  expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "Mine" });
  // The next write goes after "Mine", and names it.
  rename("Mine, again", "Bob");
  await answer(1, false);
  expect(await sentAt(2)).toEqual({ expected: { title: "Mine" }, title: "Mine, again" });
});

// The read the landing started answers with the priorities the page draws already - it showed
// neither - which draws nothing again: the page has read past the landing all the same.
test("a write that landed is named no more once a read answered after it: an edit started from a text before it then names that text", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0);
  rename("One", "Old");
  await answer(0, false);
  // The refresh that landing started answers. Later another person writes "Old" back, the field
  // takes it in, and the reader writes over it: the server holds "Old", and a write naming "One"
  // would be refused.
  await read(1);
  rename("Two", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "Two" });
});

test("a write pressed before the page read what landed goes after it, and that read leaves the write still out to go after", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0, "Old");
  rename("One", "Old");
  await answer(0, false);
  // The page has not read "One" yet, and the field still holds "Old": the next write names "One".
  rename("Two", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "One" }, title: "Two" });
  // The refresh "One" started answers while "Two" is out, and the field takes "One" in: the
  // next write goes after "Two".
  await read(1, "One");
  rename("Three", "One");
  await answer(1, false);
  expect(await sentAt(2)).toEqual({ expected: { title: "Two" }, title: "Three" });
});

// The app's client lets a read already out answer rather than cancel it (`DispatchQueryClient`),
// so a read that left before a write committed can answer after the write landed, with the text
// before it. "A" lands, and the refresh it started is still out when "AB", pressed over it, lands
// too; that refresh then answers showing "A". It does not show "AB", which the next write names.
test("a write that landed while a read was out is named still once that read answers with the text before it", async () => {
  const { answer, read, rename, sentAt } = pageWrites([], DispatchQueryClient);
  await read(0, "Old");
  rename("A", "Old");
  await answer(0, false);
  rename("AB", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "A" }, title: "AB" });
  await answer(1, false);
  await read(1, "A");
  rename("ABC", "A");
  expect(await sentAt(2)).toEqual({ expected: { title: "AB" }, title: "ABC" });
});

// The same where the read out at the landing is no write's: the event feed's, for another event,
// sent before "A" committed.
test("a write that landed while the event feed's read was out is named once that read answers with the text before it", async () => {
  const { answer, client, read, rename, sentAt } = pageWrites([], DispatchQueryClient);
  await read(0, "Old");
  rename("A", "Old");
  await sentAt(0);
  act(() => {
    void client.invalidateQueries({ queryKey: ["priorities"] });
  });
  await answer(0, false);
  await read(1, "Old");
  rename("B", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "A" }, title: "B" });
});

/** A refusal for a reason that is no other person's text: Dispatch refuses the body itself. A
 *  refusal of its own for each write, since the write path marks the one that follows an attempt
 *  that went unanswered (`refusedByDispatch`). */
const inputRefused = () =>
  new ApiError(400, { code: "PRIORITY_INPUT", error: "the title is longer than a title may be" });

/** A refusal of who wrote it: Dispatch refuses the writer. */
const writerRefused = () =>
  new ApiError(403, { code: "FORBIDDEN", error: "this credential may not write priorities" });

/** An answer that is no verdict: the attempt failed, and the write tries again later. */
const UNAVAILABLE = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });

// How a write ends, and what the next write of the same text names. The field takes the server's
// text in only from a read, so the reader's next edit starts from what the read after the ending
// shows: with nobody else writing, the next write names the text the server holds, and lands.
// One row for each way a write ends.

test("a write that landed: the next, pressed once the read shows it, names it", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0, "Old");
  rename("Mine", "Old");
  await answer(0, false);
  await read(1, "Mine");
  rename("Mine, more", "Mine");
  expect(await sentAt(1)).toEqual({ expected: { title: "Mine" }, title: "Mine, more" });
});

// Another person's "Bob" stood when "Mine" reached Dispatch: the page takes the refusal and reads
// the priorities, and the field takes "Bob" in from that read.
test("a write another person's text refused: the next, pressed once the read shows theirs, names it", async () => {
  const { answer, read, rename, sentAt } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  rename("Mine", "Old");
  await answer(0, true);
  await read(1, "Bob");
  rename("Mine, again", "Bob");
  expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "Mine, again" });
});

// Dispatch refused the body or the writer and wrote nothing: the read shows the text before it,
// which the field holds still as the text it took in.
test.each([
  ["its body", inputRefused],
  ["its writer", writerRefused],
] as const)("a write Dispatch refused for %s: the next names the text the field took in", async (_why, refusal) => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0, "Old");
  rename("Mine", "Old");
  await answer(0, refusal());
  await read(1, "Old");
  rename("Mine, again", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "Mine, again" });
});

// Stopped, its key spent, or refused after an attempt that went unanswered, a write ended with no
// verdict, and here it never reached Dispatch. The page reads nothing for it - a landing would
// bring its own event's read - so the field holds the text before it.
test("a write the reader stopped: the next names the text the field took in", async () => {
  const { read, readCount, rename, sentAt, stopLate } = pageWrites();
  await read(0, "Old");
  await onFakeClock(async () => {
    rename("Mine", "Old");
    await stopLate(0);
    rename("Mine, more", "Old");
    expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "Mine, more" });
  });
  expect(readCount()).toBe(1);
});

test("a write whose key Dispatch no longer holds: the next names the text the field took in", async () => {
  const { answer, client, read, readCount, rename, sentAt } = pageWrites();
  await read(0, "Old");
  await onFakeClock(async () => {
    rename("Mine", "Old");
    await answer(0, UNAVAILABLE);
    // The attempt after the wait would go under a key Dispatch has forgotten.
    await jump(IDEMPOTENCY_KEY_TTL_MS);
    expect(writesOf(client).rows()).toMatchObject([
      { refusal: { name: "SendExpiredError" }, status: "refused" },
    ]);
    rename("Mine, more", "Old");
    expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "Mine, more" });
  });
  expect(readCount()).toBe(1);
});

test("a write refused after an attempt of it went unanswered: the next names the text the field took in", async () => {
  const { answer, read, readCount, rename, sentAt } = pageWrites();
  await read(0, "Old");
  await onFakeClock(async () => {
    rename("Mine", "Old");
    await answer(0, UNAVAILABLE);
    await tick(FIRST_BACKOFF_MS);
    await answer(1, inputRefused());
    rename("Mine, more", "Old");
    expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "Mine, more" });
  });
  expect(readCount()).toBe(1);
});

// "B" waited its turn behind "A", and the reader discarded it before it went. "A" lands, and an
// edit made before the page read it - the field still holds "Old" - names "A", which the server
// holds.
test("a write discarded before it went: the next names the write before it that landed", async () => {
  const { answer, client, read, rename, sentAt } = pageWrites();
  await read(0, "Old");
  rename("A", "Old");
  rename("B", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "A" });
  const writes = writesOf(client);
  const [, waiting] = writes.rows();
  expect(waiting?.status).toBe("queued");
  act(() => {
    writes.discard(waiting?.id ?? 0);
  });
  await answer(0, false);
  rename("C", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "A" }, title: "C" });
});

// A write stopped and then discarded on the shell's line, and four renames, each from the text
// the read before it showed. Each names the text the server holds, so each lands.
test("a write stopped and then discarded: each of four renames after it names the text the server holds", async () => {
  const { answer, client, read, rename, sentAt, stopLate } = pageWrites();
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("Mine", "Old");
    const stopped = await stopLate(0);
    act(() => {
      writes.discard(stopped?.id ?? 0);
    });
    expect(writes.rows()).toEqual([]);
    let held = "Old";
    for (const [at, title] of ["One", "Two", "Three", "Four"].entries()) {
      rename(title, held);
      expect(await sentAt(at + 1)).toEqual({ expected: { title: held }, title });
      await answer(at + 1, false);
      held = title;
      await read(at + 1, held);
    }
  });
});

// The landing changing what the next write names. "B" went over "A", which landed, and Dispatch
// refused "B" for its body: the server holds "A".
test("A over Old, B over A, A landed and B refused: C, pressed once the read shows A, names A", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0, "Old");
  rename("A", "Old");
  rename("B", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "A" });
  await answer(0, false);
  expect(await sentAt(1)).toEqual({ expected: { title: "A" }, title: "B" });
  await answer(1, inputRefused());
  await read(1, "A");
  await read(2, "A");
  rename("C", "A");
  expect(await sentAt(2)).toEqual({ expected: { title: "A" }, title: "C" });
});

// The same, where "A" was stopped late: "B", pressed over it while it was out, names "A", a text
// the server never took, and Dispatch refuses it 409, as it refuses a write over another person's
// text, though nobody else wrote - the limit rule 3 accepts. Nothing is lost: the page keeps "B"
// beside the field, and Use my text lands it. The shell's Retry makes "A" again under its key,
// which lands; no press awaits that landing: the store reads the priorities again, and that read
// brings "A" to the field, which takes it in (`arrival`). Use my text, pressed then, names "A".
test("A over Old, B over A, A stopped late: B is refused 409 and kept beside the field, the shell's Retry lands A, and Use my text, pressed once the read shows A, names A and lands B", async () => {
  const { answer, client, keep, kept, read, readCount, rename, sentAt, stopLate } =
    pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("A", "Old");
    rename("B", "Old", "A");
    expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "A" });
    const stopped = await stopLate(0);
    expect(await sentAt(1)).toEqual({ expected: { title: "A" }, title: "B" });
    await answer(1, true);
    act(() => writes.retry(stopped?.id ?? 0));
    await answer(2, false);
  });
  for (let at = 1; at < readCount(); at += 1) await read(at, "A");
  await waitFor(() => expect(kept("A")).toEqual(["B"]));
  expect(writes.rows()).toMatchObject([
    { claimed: true, detail: { text: "B" }, refusal: { status: 409 }, status: "refused" },
  ]);
  keep([]);
  rename("B", "A");
  expect(await sentAt(3)).toEqual({ expected: { title: "A" }, title: "B" });
  await answer(3, false);
  expect(writes.rows()).toEqual([]);
});

// "A" over "Old" was stopped with no verdict and stays on the shell's line. The field still holds
// "A" as the reader edits on, so "B" is made on "A", and it lands. "B" carries "A", so "A" is let
// go: its Retry sends nothing, where a write naming the text "A" went over would be refused and
// offer the older text back as another person's.
test("a stopped write that a newer landed write made on its text carries is let go: its Retry sends nothing", async () => {
  const { answer, client, read, rename, sentAt, sentCount, stopLate } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("A", "Old");
    const stopped = await stopLate(0);
    expect(stopped?.detail).toEqual({
      attribute: "title",
      expected: "Old",
      id: "p1",
      made: { at: expect.any(Number), on: "Old" },
      text: "A",
    });
    rename("B", "Old", "A");
    expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "B" });
    await answer(1, false);
    expect(writes.rows()).toMatchObject([{ id: stopped?.id, status: "refused" }]);
    act(() => writes.retry(stopped?.id ?? 0));
    await turns();
  });
  expect(writes.rows()).toEqual([]);
  expect(sentCount()).toBe(2);
});

// The same through a reload: the tab keeps "A", with the text it was made on, until "B" lands,
// and no longer then, so the page after the reload neither offers it nor sends it.
test("a stopped write that a newer landed write made on its text carries is kept no longer: a reload neither offers it nor sends it", async () => {
  const { answer, read, reload, rename, sentAt, sentCount, stopLate } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  let key = "";
  await onFakeClock(async () => {
    rename("A", "Old");
    key = (await stopLate(0))?.key ?? "";
    expect(JSON.parse(sessionStorage.getItem(`dispatch.write:${key}`) ?? "null")).toMatchObject({
      detail: { expected: "Old", made: { on: "Old" }, text: "A" },
    });
    rename("B", "Old", "A");
    await sentAt(1);
    await answer(1, false);
  });
  expect(sessionStorage.getItem(`dispatch.write:${key}`)).toBeNull();
  const after = reload();
  await turns();
  expect(after.rows()).toEqual([]);
  expect(sentCount()).toBe(2);
});

// A run of stopped writes, each made on the one before: "A" over "Old", and "B", which the field
// made on "A", both stopped; then "C", made on "B", lands. "C" carries "B", which carries "A": both
// are let go, from the line and from the tab, and their Retries send nothing.
test("stopped writes each made on the one before are let go together once a write made on the last of them lands", async () => {
  const { answer, client, read, rename, sentAt, sentCount, stopLate } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  const stopped: (Write | undefined)[] = [];
  await onFakeClock(async () => {
    rename("A", "Old");
    stopped.push(await stopLate(0));
    rename("B", "Old", "A");
    stopped.push(await stopLate(1));
    expect(stopped.map((write) => write?.detail)).toMatchObject([
      { made: { on: "Old" }, text: "A" },
      { made: { on: "A" }, text: "B" },
    ]);
    rename("C", "Old", "B");
    expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "C" });
    await answer(2, false);
    expect(writes.rows().map((write): number | undefined => write.id)).toEqual(
      stopped.map((write) => write?.id)
    );
    for (const write of stopped) act(() => writes.retry(write?.id ?? 0));
    await turns();
  });
  expect(writes.rows()).toEqual([]);
  expect(sentCount()).toBe(3);
  for (const write of stopped) {
    expect(sessionStorage.getItem(`dispatch.write:${write?.key}`)).toBeNull();
  }
});

// The order the reader made her edits in is the writes' own (`made.at`), not the store's: a Retry
// puts a write at the back of its line, and a reload hands the writes out in the order they were
// last pressed. "OldY", made on "Old", is out when she takes the "Y" back, and "Old", made on
// "OldY", waits behind it. Both are stopped; she retries "OldY", and the tab reloads while it is
// out. The app after it holds "Old" ahead of "OldY", which lands there. "OldY" was made before
// "Old", so it carries none of it: her last edit stays on the line and in the tab, and its Retry
// lands it.
test("a stopped write made after one the reader retried, ahead of it after a reload, is not let go when that one lands", async () => {
  const { answer, client, read, reload, rename, sentAt, stopLate, undraw } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("OldY", "Old");
    rename("Old", "Old", "OldY");
    expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "OldY" });
    const first = await stopLate(0);
    expect(await sentAt(1)).toEqual({ expected: { title: "OldY" }, title: "Old" });
    const last = await stopLate(1);
    act(() => writes.retry(first?.id ?? 0));
    expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "OldY" });
    undraw();
    const after = reload();
    expect(after.rows()).toMatchObject([
      { detail: { text: "Old" }, key: last?.key, status: "refused" },
      { detail: { text: "OldY" }, key: first?.key },
    ]);
    expect(await sentAt(3)).toEqual({ expected: { title: "Old" }, title: "OldY" });
    await answer(3, false);
    const [line] = after.rows();
    expect(after.rows()).toMatchObject([
      { detail: { text: "Old" }, key: last?.key, status: "refused" },
    ]);
    expect(sessionStorage.getItem(`dispatch.write:${last?.key}`)).not.toBeNull();
    act(() => after.retry(line?.id ?? 0));
    expect(await sentAt(4)).toEqual({ expected: { title: "OldY" }, title: "Old" });
    await answer(4, false);
    expect(after.rows()).toEqual([]);
  });
});

// The other way round: "B", made on "A", is stopped after "A"; she retries "A", which is stopped
// again, and the tab reloads. The app after it holds "B" ahead of "A". "B"'s Retry lands: it was
// made on "A", and after it, so it carries "A", which is let go though the store holds it behind.
test("a stopped write retried after a newer one made on it, behind it after a reload, is let go when that one lands", async () => {
  const { answer, client, read, reload, rename, sentAt, sentCount, stopLate, undraw } =
    pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("A", "Old");
    const older = await stopLate(0);
    rename("B", "Old", "A");
    expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "B" });
    const newer = await stopLate(1);
    act(() => writes.retry(older?.id ?? 0));
    expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "A" });
    await stopLate(2);
    undraw();
    const after = reload();
    const [first, second] = after.rows();
    expect(after.rows()).toMatchObject([
      { detail: { text: "B" }, key: newer?.key, status: "refused" },
      { detail: { text: "A" }, key: older?.key, status: "refused" },
    ]);
    act(() => after.retry(first?.id ?? 0));
    expect(await sentAt(3)).toEqual({ expected: { title: "Old" }, title: "B" });
    await answer(3, false);
    expect(sessionStorage.getItem(`dispatch.write:${older?.key}`)).toBeNull();
    act(() => after.retry(second?.id ?? 0));
    await turns();
    expect(after.rows()).toEqual([]);
  });
  expect(sentCount()).toBe(4);
});

// A run goes through a write Dispatch refused with a verdict. "A" over "Old" is stopped; "AB",
// made on "A", is refused - as another person's text refuses it, or for its body - and stays on
// the line; "ABC", made on "AB", lands. "ABC" carries "AB", which carries "A": "AB" is no write to
// let go, since a verdict refused it, but "A" goes, from the line and from the tab.
test.each([
  ["another person's text", true],
  ["its body", inputRefused],
] as const)("a stopped write is let go through a write made on it that Dispatch refused for %s, once a write made on that one lands", async (_why, refusal) => {
  const { answer, client, read, rename, sentAt, sentCount, stopLate } = pageWrites();
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("A", "Old");
    const stopped = await stopLate(0);
    rename("AB", "Old", "A");
    expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "AB" });
    await answer(1, refusal === true ? true : refusal());
    rename("ABC", "Old", "AB");
    expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "ABC" });
    await answer(2, false);
    expect(writes.rows()).toMatchObject([
      { detail: { text: "A" }, status: "refused" },
      { detail: { text: "AB" }, status: "refused" },
    ]);
    expect(sessionStorage.getItem(`dispatch.write:${stopped?.key}`)).toBeNull();
    act(() => writes.retry(stopped?.id ?? 0));
    await turns();
  });
  expect(writes.rows()).toMatchObject([{ detail: { text: "AB" }, status: "refused" }]);
  expect(sentCount()).toBe(3);
});

// "L" over "Old", made on "Old", is stopped, and another person's "Bob" arrives, which the field
// takes in, keeping "L" beside it. The reader uses it: "L" again, over "Bob", made on what "L" was
// made on. It lands, and the server holds the very text of the stopped write, which is let go:
// its Retry sends nothing.
test("a stopped write is let go once a newer write of its very text lands", async () => {
  const { answer, client, read, rename, sentAt, sentCount, stopLate } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("L", "Old");
    const stopped = await stopLate(0);
    act(() => {
      void client.invalidateQueries({ queryKey: ["priorities"] });
    });
    await read(1, "Bob");
    rename("L", "Bob", "Old");
    expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "L" });
    await answer(1, false);
    expect(sessionStorage.getItem(`dispatch.write:${stopped?.key}`)).toBeNull();
    act(() => writes.retry(stopped?.id ?? 0));
    await turns();
  });
  expect(writes.rows()).toEqual([]);
  expect(sentCount()).toBe(2);
});

// A reload starts a new app, whose writes must still be newer than every write it took up, though
// the clock that stamped those ran ahead of this one.
test("a write made after a reload is made after every write the reload took up, whatever the clock says", async () => {
  const { read, reloadShowing, rename, sentAt, stopLate, undraw } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  let key = "";
  await onFakeClock(async () => {
    rename("A", "Old");
    key = (await stopLate(0))?.key ?? "";
  });
  const name = `dispatch.write:${key}`;
  const kept = JSON.parse(sessionStorage.getItem(name) ?? "null");
  const ahead = Date.now() + 3_600_000;
  sessionStorage.setItem(
    name,
    JSON.stringify({
      ...kept,
      detail: { ...kept.detail, made: { ...kept.detail.made, at: ahead } },
    })
  );
  undraw();
  const { page, store } = reloadShowing(SHOWS_P1);
  act(() => {
    void page.result.current.rename("p1", "B", "Old", "Old", "Rename the priority");
  });
  expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "B" });
  const records = store.rows().map((write) => ofKind(write, priorityTextWrite)?.detail);
  expect(records).toMatchObject([{ made: { at: ahead }, text: "A" }, { text: "B" }]);
  expect(records[1]?.made?.at).toBeGreaterThan(ahead);
});

// "A" over "Old" was stopped with no verdict and stays on the shell's line. Another person's "Bob"
// arrives, the field takes it in, and the reader edits his text: "Bob, mine" over "Bob" lands. It
// was made on his text, not on "A", so it carries none of "A": "A" stays on the line, and its
// Retry sends it over "Old", which another person's text refuses into the page's keeping, where
// her text is offered once.
test("a stopped write that a newer landed write over another person's text does not supersede stays: its Retry is refused into the page's keeping", async () => {
  const { answer, client, kept, read, readCount, rename, sentAt, stopLate } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("A", "Old");
    const stopped = await stopLate(0);
    act(() => {
      void client.invalidateQueries({ queryKey: ["priorities"] });
    });
    await read(1, "Bob");
    rename("Bob, mine", "Bob");
    expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "Bob, mine" });
    await answer(1, false);
    expect(writes.rows()).toMatchObject([{ id: stopped?.id, status: "refused" }]);
    act(() => writes.retry(stopped?.id ?? 0));
    await turns();
    expect(writes.rows()).toMatchObject([{ id: stopped?.id, status: "sending" }]);
    expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "A" });
    await answer(2, true);
  });
  for (let at = 2; at < readCount(); at += 1) await read(at, "Bob, mine");
  await waitFor(() => expect(kept("Bob, mine")).toEqual(["A"]));
  expect(writes.rows()).toMatchObject([
    { claimed: true, detail: { expected: "Old", text: "A" }, status: "refused" },
  ]);
});

// The same through a reload: the tab still keeps "A" once "Bob, mine" lands, so the line after
// the reload offers it, and its Retry sends it over "Old", which another person's text refuses;
// the line keeps her text.
test("a stopped write that a newer landed write over another person's text does not supersede is kept: a reload offers it, and its Retry sends it", async () => {
  const { answer, client, read, reload, rename, sentAt, stopLate } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  let key = "";
  await onFakeClock(async () => {
    rename("A", "Old");
    key = (await stopLate(0))?.key ?? "";
    act(() => {
      void client.invalidateQueries({ queryKey: ["priorities"] });
    });
    await read(1, "Bob");
    rename("Bob, mine", "Bob");
    expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "Bob, mine" });
    await answer(1, false);
  });
  expect(JSON.parse(sessionStorage.getItem(`dispatch.write:${key}`) ?? "null")).toMatchObject({
    detail: { expected: "Old", text: "A" },
  });
  const after = reload();
  await turns();
  const [line] = after.rows();
  expect(after.rows()).toMatchObject([
    { detail: { expected: "Old", text: "A" }, status: "refused" },
  ]);
  act(() => after.retry(line?.id ?? 0));
  expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "A" });
  await answer(2, true);
  expect(after.rows()).toMatchObject([
    { detail: { text: "A" }, refusal: { status: 409 }, status: "refused" },
  ]);
});

// "A" over "Old" was stopped with no verdict and stays on the shell's line. The reader leaves the
// page and comes back: the field drawn again starts from the server's text, "Old", and her next
// edit, "B", is made on it, naming "Old" as "A" does. It lands, and carries none of "A": "A" stays
// on the line and in the tab's storage, and its Retry sends it over "Old", which "B" now refuses
// into the page's keeping, where her text is offered once.
test("a stopped write that a newer landed write made in the field drawn again does not carry stays: its Retry is refused into the page's keeping", async () => {
  const { answer, client, drawAnother, read, readCount, rename, sentAt, stopLate, undraw } =
    pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  let stopped: Write | undefined;
  await onFakeClock(async () => {
    rename("A", "Old");
    stopped = await stopLate(0);
  });
  undraw();
  const again = drawAnother(SHOWS_P1);
  act(() => {
    void again.result.current.rename("p1", "B", "Old", "Old", "Rename the priority");
  });
  expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "B" });
  await answer(1, false);
  expect(writes.rows()).toMatchObject([{ id: stopped?.id, status: "refused" }]);
  expect(sessionStorage.getItem(`dispatch.write:${stopped?.key}`)).not.toBeNull();
  act(() => writes.retry(stopped?.id ?? 0));
  expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "A" });
  await answer(2, true);
  for (let at = 1; at < readCount(); at += 1) await read(at, "B");
  await waitFor(() => expect(again.result.current.kept("p1", "title", "B")).toEqual(["A"]));
  expect(writes.rows()).toMatchObject([
    { claimed: true, detail: { expected: "Old", text: "A" }, status: "refused" },
  ]);
});

// The same through a reload, which draws the field again: the app after it takes "A" up from the
// tab, with the text it was made on, and the reader's next edit, made on the server's "Old",
// lands. "A" stays on the line and in the tab, and its Retry sends it, refused into the keeping of
// the page the reload drew.
test("a stopped write a reload takes up stays once a newer write made on the server's text lands: its Retry is refused into the page's keeping", async () => {
  const { answer, read, readCount, reloadShowing, rename, sentAt, stopLate, undraw } =
    pageWrites(SHOWS_P1);
  await read(0, "Old");
  let key = "";
  await onFakeClock(async () => {
    rename("A", "Old");
    key = (await stopLate(0))?.key ?? "";
  });
  undraw();
  const { page, store } = reloadShowing(SHOWS_P1);
  const [line] = store.rows();
  expect(store.rows()).toMatchObject([
    { detail: { expected: "Old", made: { on: "Old" }, text: "A" }, status: "refused" },
  ]);
  await read(1, "Old");
  act(() => {
    void page.result.current.rename("p1", "B", "Old", "Old", "Rename the priority");
  });
  expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "B" });
  await answer(1, false);
  expect(store.rows()).toMatchObject([{ id: line?.id, status: "refused" }]);
  expect(sessionStorage.getItem(`dispatch.write:${key}`)).not.toBeNull();
  act(() => store.retry(line?.id ?? 0));
  expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "A" });
  await answer(2, true);
  for (let at = 2; at < readCount(); at += 1) await read(at, "B");
  await waitFor(() => expect(page.result.current.kept("p1", "title", "B")).toEqual(["A"]));
  expect(store.rows()).toMatchObject([
    { claimed: true, detail: { expected: "Old", text: "A" }, status: "refused" },
  ]);
});

// A text a field kept on its own as another person's text arrived over it is held with no record
// of what it was made on or when (`keep`): no write of it was sent. "A" over "Old" is stopped; the
// reader types "AB" over it, and another person's "Bob" arrives, which the field takes in,
// keeping "AB" on its own. A write of that very text, "AB" over "Bob", made on "Bob", lands while
// the store still holds the kept one. The kept text has no time to be older than that write by,
// so the landing does not carry it, and no text it was made on, so it carries nothing: "A" stays
// on the line and in the tab.
test("a text a field kept on its own, with no record of what it was made on or when, neither carries a stopped write nor is carried by one", async () => {
  const { answer, client, keep, read, rename, sentAt, stopLate } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  const writes = writesOf(client);
  await onFakeClock(async () => {
    rename("A", "Old");
    const stopped = await stopLate(0);
    act(() => {
      void client.invalidateQueries({ queryKey: ["priorities"] });
    });
    await read(1, "Bob");
    keep(["AB"]);
    expect(writes.rows().map((write) => ofKind(write, priorityTextWrite)?.detail)).toEqual([
      {
        attribute: "title",
        expected: "Old",
        id: "p1",
        made: { at: expect.any(Number), on: "Old" },
        text: "A",
      },
      { attribute: "title", expected: "Bob", id: "p1", text: "AB" },
    ]);
    rename("AB", "Bob");
    expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "AB" });
    await answer(1, false);
    expect(writes.rows()).toMatchObject([
      { id: stopped?.id, status: "refused" },
      { detail: { text: "AB" }, sent: false, status: "refused" },
    ]);
    expect(sessionStorage.getItem(`dispatch.write:${stopped?.key}`)).not.toBeNull();
  });
});

// A chain's start is a text the server took: an edit started from it after a read showed it -
// another person set it back - names it, whatever became of the chain's own writes.
test("a write from the text another person set back, once the read shows it, names that text", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0, "Old");
  rename("A", "Old");
  rename("B", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "A" });
  await answer(0, false);
  expect(await sentAt(1)).toEqual({ expected: { title: "A" }, title: "B" });
  await answer(1, true);
  await read(1, "Old");
  rename("C", "Old");
  expect(await sentAt(2)).toEqual({ expected: { title: "Old" }, title: "C" });
});

// The chains are the app's, not the page's: the reader leaves the page while "A" over "Old" is out
// and comes back, and the page drawn again still reads "Old" when "A" lands. An edit made there
// before a read shows "A" names "A", which the server holds, so it lands, and is not refused
// against the reader's own write.
test("a page drawn again while a write is out goes after it: an edit before a read shows its landing names its text", async () => {
  const { answer, drawAnother, read, rename, sentAt, undraw } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  rename("A", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "A" });
  undraw();
  const again = drawAnother(SHOWS_P1);
  await answer(0, false);
  act(() => {
    void again.result.current.rename("p1", "AB", "Old", "Old", "Rename the priority");
  });
  expect(await sentAt(1)).toEqual({ expected: { title: "A" }, title: "AB" });
});

/** A page that shows `p1`, as its own page does. */
const SHOWS_P1: readonly string[] = ["p1"];

/** Lets every turn pass that a write would take to go out, or a read to start. */
const turns = () =>
  act(async () => {
    await settle();
  });

// A write the server refused because another person's title stands where the reader's edit
// started is the page's to settle with the reader where the page shows that priority: the store
// holds it, claimed, so nothing stands for it on the shell's line, and the reader's text is kept
// beside the title. The claim reads the priorities once - the refused write wrote nothing - and
// the text waits until that read has answered, since until then the page may still show the text
// the write went over. Until then no write of the title is sent: it would name a text the server
// no longer holds.
test("a page showing a priority takes its refused title, offers it once a read started since has answered, and sends nothing before", async () => {
  const { answer, client, kept, read, readCount, rename, sentAt, sentCount } = pageWrites(SHOWS_P1);
  await read(0);
  rename("Mine", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "Mine" });
  await answer(0, true);
  await turns();
  expect(writesOf(client).rows()).toMatchObject([
    { claimed: true, detail: { text: "Mine" }, status: "refused" },
  ]);
  expect(readCount()).toBe(2);
  expect(kept("Old")).toEqual([]);
  expect(kept("Bob")).toEqual([]);
  // A "Use my text" pressed now would name "Old" again: nothing is sent, the text stays kept,
  // and it waits for a read started now, which cancels the one still out.
  rename("Mine", "Old");
  await turns();
  expect(sentCount()).toBe(1);
  expect(readCount()).toBe(3);
  await read(1);
  expect(kept("Bob")).toEqual([]);
  await read(2);
  await waitFor(() => expect(kept("Bob")).toEqual(["Mine"]));
  // A text the server holds is no text of the reader's to put back.
  expect(kept("Mine")).toEqual([]);
  rename("Mine", "Bob");
  expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "Mine" });
});

// The other person may write back the very text the reader's write went over before the page
// reads again. The read answers with it all the same, and then the reader's text is offered,
// and written over it.
test("a refused title is offered once the read answers, though the text it went over is back on the server", async () => {
  const { answer, kept, read, rename, sentAt } = pageWrites(SHOWS_P1);
  await read(0);
  rename("Mine", "Old");
  await sentAt(0);
  await answer(0, true);
  expect(kept("Old")).toEqual([]);
  await read(1);
  await waitFor(() => expect(kept("Old")).toEqual(["Mine"]));
  rename("Mine", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "Mine" });
});

// The reader edits on in a field that has not taken the other person's title in: its start is
// still the title the refused write went over, which a write names, and never the reader's text
// the server never took. The server holds the other person's title, so it is refused again, and
// the page keeps the reader's text beside the title as it keeps the first: the conflict path, not
// a write over their title.
test("a write from a field that has not taken in the title another person's change put there names the title the field holds, and is refused again into the page's keeping", async () => {
  const { answer, kept, read, rename, sentAt } = pageWrites(SHOWS_P1);
  await read(0);
  rename("Mine", "Old");
  await sentAt(0);
  await answer(0, true);
  await read(1);
  await waitFor(() => expect(kept("Bob")).toEqual(["Mine"]));
  rename("Mine, again", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "Mine, again" });
  await answer(1, true);
  await read(2);
  await waitFor(() => expect(kept("Bob")).toEqual(["Mine, again", "Mine"]));
});

// Two writes of one chain, both refused: "A" over "Old", then "B" over "A". The page still reads
// "Old", which is neither text the second went over, and that says nothing of what it has read:
// each text waits for the read its claim started, and the second's cancels the first's.
test("the texts of a refused chain wait for the last read their claims started, whatever the page reads", async () => {
  const { answer, kept, read, rename, sentAt } = pageWrites(SHOWS_P1);
  await read(0);
  rename("A", "Old");
  rename("B", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "A" });
  await answer(0, true);
  expect(await sentAt(1)).toEqual({ expected: { title: "A" }, title: "B" });
  await answer(1, true);
  expect(kept("Old")).toEqual([]);
  expect(kept("Bob")).toEqual([]);
  await read(1);
  expect(kept("Bob")).toEqual([]);
  await read(2);
  await waitFor(() => expect(kept("Bob")).toEqual(["B", "A"]));
});

// A page that shows other priorities, or is no longer drawn, takes nothing: the refusals stand
// on the shell's line, and nothing reads the priorities again for them. A page drawn later that
// shows their priorities takes them from there, all of them with one read, and the store holds
// them for it.
test("a page not drawn takes no refusal, and one drawn later takes them from the line with one read", async () => {
  const { answer, client, drawAnother, read, readCount, rename, sentAt, undraw } =
    pageWrites(SHOWS_P1);
  const elsewhere = drawAnother(["p3"]);
  await read(0);
  rename("Mine", "Old");
  rename("Yours", "Old", "Old", "p2");
  await sentAt(1);
  undraw();
  await answer(0, true);
  await answer(1, true);
  await turns();
  const writes = writesOf(client);
  expect(writes.rows().map((row) => row.status)).toEqual(["refused", "refused"]);
  expect(elsewhere.result.current.kept("p1", "title", "Bob")).toEqual([]);
  expect(readCount()).toBe(1);
  const later = drawAnother(["p1", "p2"]);
  await waitFor(() =>
    expect(writes.rows().map((row) => row.status === "refused" && row.claimed)).toEqual([
      true,
      true,
    ])
  );
  await turns();
  expect(readCount()).toBe(2);
  expect(later.result.current.kept("p1", "title", "Bob")).toEqual([]);
  await read(1);
  await waitFor(() => expect(later.result.current.kept("p1", "title", "Bob")).toEqual(["Mine"]));
  expect(later.result.current.kept("p2", "title", "Bob")).toEqual(["Yours"]);
});

// The reader's text a conflict brought back lives in the store, not in the page: a page that
// goes hands the write back to the shell's line, and a page drawn later that shows its priority
// takes it again and offers the same text once its own read has answered. Keeping the other
// person's text lets the write go, and nothing of it is kept.
test("a page that goes hands its refused title back, one drawn later offers it again, and keeping theirs lets it go", async () => {
  const { answer, client, drawAnother, kept, read, rename, sentAt, undraw } = pageWrites(SHOWS_P1);
  await read(0);
  rename("Mine", "Old");
  await sentAt(0);
  await answer(0, true);
  await read(1);
  await waitFor(() => expect(kept("Bob")).toEqual(["Mine"]));
  const writes = writesOf(client);
  undraw();
  expect(writes.rows()).toMatchObject([{ claimed: false, status: "refused" }]);
  const later = drawAnother(SHOWS_P1);
  await waitFor(() => expect(writes.rows()).toMatchObject([{ claimed: true }]));
  expect(later.result.current.kept("p1", "title", "Bob")).toEqual([]);
  await read(2);
  await waitFor(() => expect(later.result.current.kept("p1", "title", "Bob")).toEqual(["Mine"]));
  act(() => later.result.current.keep("p1", "title", []));
  expect(writes.rows()).toEqual([]);
  expect(later.result.current.kept("p1", "title", "Bob")).toEqual([]);
});

// The reader's texts beside a field run newest first. Another person's text arriving while the
// reader typed "Typed" has the field keep that ahead of the text a refused write brought back,
// and the refused write's text keeps its place behind it. The store holds both: the field's own
// text is held there too, as a write the page holds, though none of it was sent.
test("a field's kept texts stay newest first, a refused write's text keeping its place among them", async () => {
  const { answer, client, keep, kept, read, rename, sentAt } = pageWrites(SHOWS_P1);
  await read(0);
  rename("Mine", "Old");
  await sentAt(0);
  await answer(0, true);
  await read(1);
  await waitFor(() => expect(kept("Bob")).toEqual(["Mine"]));
  keep(["Typed", "Mine"]);
  expect(kept("Bob")).toEqual(["Typed", "Mine"]);
  expect(writesOf(client).rows()).toMatchObject([
    { claimed: true, detail: { text: "Mine" }, status: "refused" },
    { claimed: true, detail: { text: "Typed" }, sent: false, status: "refused" },
  ]);
});

// A text the field keeps on its own, with no write of it refused, is held in the store as a
// write another person's text refused, and offered at once: the field shows that text already.
// One the field puts ahead of a text a newer write carries is held anew, so the store's order
// stays the field's, and one the field drops lets its write go.
test("a field's own kept texts are held in the store in the field's order, and a dropped one goes", async () => {
  const { client, keep, kept, read, sentCount } = pageWrites(SHOWS_P1);
  await read(0);
  const writes = writesOf(client);
  keep(["A"]);
  keep(["B", "A"]);
  expect(kept("Bob")).toEqual(["B", "A"]);
  keep(["A", "B"]);
  expect(kept("Bob")).toEqual(["A", "B"]);
  expect(writes.rows()).toMatchObject([
    { claimed: true, detail: { text: "B" }, sent: false, status: "refused" },
    { claimed: true, detail: { text: "A" }, sent: false, status: "refused" },
  ]);
  keep(["A"]);
  expect(writes.rows()).toMatchObject([{ detail: { text: "A" } }]);
  keep([]);
  expect(writes.rows()).toEqual([]);
  expect(sentCount()).toBe(0);
});

// Another person's text arriving while the reader's write is out has the field keep the reader's
// text on its own, and the store holds it. That write's refusal then carries the same text: the
// held one goes, and the refused write keeps the text, so the shell's line never names it twice.
// The text waits for the read the refusal's claim started, as a refused write's text does.
test("a held text a refused write carries too goes, and the refused write keeps it", async () => {
  const { answer, client, keep, kept, read, rename, sentAt } = pageWrites(SHOWS_P1);
  await read(0);
  rename("Mine", "Old");
  await sentAt(0);
  keep(["Mine"]);
  const writes = writesOf(client);
  expect(writes.rows()).toMatchObject([
    { sent: true, status: "sending" },
    { claimed: true, detail: { text: "Mine" }, sent: false, status: "refused" },
  ]);
  await answer(0, true);
  await waitFor(() =>
    expect(writes.rows()).toMatchObject([
      { claimed: true, detail: { text: "Mine" }, sent: true, status: "refused" },
    ])
  );
  expect(kept("Bob")).toEqual([]);
  await read(1);
  await waitFor(() => expect(kept("Bob")).toEqual(["Mine"]));
});

// The same, where no page takes the refusal: the reader left for a page that does not show the
// priority, which handed the held text to the shell's line before the write was refused. The
// refused write keeps the text, and the held one goes all the same, so the line names it once,
// and a page drawn later that shows the priority takes the refused write and offers its text.
test("a held text a refused write carries goes though no page takes the refusal, so the line names it once", async () => {
  const { answer, client, drawAnother, keep, read, rename, sentAt, undraw } = pageWrites(SHOWS_P1);
  await read(0);
  rename("Mine", "Old");
  await sentAt(0);
  keep(["Mine"]);
  undraw();
  const writes = writesOf(client);
  expect(writes.rows()).toMatchObject([
    { sent: true, status: "sending" },
    { claimed: false, detail: { text: "Mine" }, sent: false, status: "refused" },
  ]);
  await answer(0, true);
  expect(writes.rows()).toMatchObject([
    { claimed: false, detail: { text: "Mine" }, sent: true, status: "refused" },
  ]);
  const later = drawAnother(SHOWS_P1);
  await read(1);
  await waitFor(() => expect(later.result.current.kept("p1", "title", "Bob")).toEqual(["Mine"]));
});

// The same, where one page holds the text its field kept and another page, drawn first, takes the
// refusal: the held text goes from the page that held it, and the refused write keeps it.
test("a held text a refused write carries goes though another page holds it", async () => {
  const { answer, client, drawAnother, kept, read, rename, sentAt } = pageWrites(SHOWS_P1);
  const other = drawAnother(SHOWS_P1);
  const otherKept = () => other.result.current.kept("p1", "title", "Bob");
  await read(0);
  rename("Mine", "Old");
  await sentAt(0);
  act(() => other.result.current.keep("p1", "title", ["Mine"]));
  expect(otherKept()).toEqual(["Mine"]);
  const writes = writesOf(client);
  await answer(0, true);
  await waitFor(() =>
    expect(writes.rows()).toMatchObject([
      { claimed: true, detail: { text: "Mine" }, sent: true, status: "refused" },
    ])
  );
  expect(otherKept()).toEqual([]);
  await read(1);
  await waitFor(() => expect(kept("Bob")).toEqual(["Mine"]));
  expect(otherKept()).toEqual([]);
});

// The other order: the refusal lands first and the page drawn first takes it, so a second page,
// which cannot take it, keeps the same text on its own as another person's arrives there. Each
// page offers the text while it is drawn; once both have gone, whichever goes first, the shell's
// line names the text once, by the refused write.
test.each([
  ["the page that took the refusal", true],
  ["the page that kept the text", false],
] as const)("a text a second page keeps after another took its refusal is on the line once both go, %s going first", async (_first, refusedFirst) => {
  const { answer, client, drawAnother, kept, read, rename, sentAt, undraw } = pageWrites(SHOWS_P1);
  const other = drawAnother(SHOWS_P1);
  await read(0);
  rename("Mine", "Old");
  await sentAt(0);
  await answer(0, true);
  await read(1);
  await waitFor(() => expect(kept("Bob")).toEqual(["Mine"]));
  act(() => other.result.current.keep("p1", "title", ["Mine"]));
  expect(other.result.current.kept("p1", "title", "Bob")).toEqual(["Mine"]);
  const writes = writesOf(client);
  expect(writes.rows()).toMatchObject([
    { claimed: true, detail: { text: "Mine" }, sent: true, status: "refused" },
    { claimed: true, detail: { text: "Mine" }, sent: false, status: "refused" },
  ]);
  if (refusedFirst) undraw();
  other.unmount();
  if (!refusedFirst) undraw();
  expect(writes.rows()).toMatchObject([
    { claimed: false, detail: { text: "Mine" }, sent: true, status: "refused" },
  ]);
});

// "Mine" over "Old" is out when the event feed brings another person's "Bob": the field takes it
// in and keeps "Mine" beside it, and the reader uses it, which writes "Mine" over "Bob". Then the
// first "Mine" is refused. The reader chose that text already, and its write is out: the refusal
// is settled at once and offered no second time, whatever the reads answer, and the reader's
// write lands.
test("a conflict refusal whose text a later write of the field carries is settled at once, and offered no second time", async () => {
  const { answer, client, keep, kept, read, readCount, rename, sentAt } = pageWrites(SHOWS_P1);
  await read(0, "Old");
  rename("Mine", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "Mine" });
  act(() => {
    void client.invalidateQueries({ queryKey: ["priorities"] });
  });
  await read(1, "Bob");
  keep(["Mine"]);
  expect(kept("Bob")).toEqual(["Mine"]);
  keep([]);
  rename("Mine", "Bob");
  await answer(0, true);
  await turns();
  for (let at = 2; at < readCount(); at += 1) await read(at, "Bob");
  expect(kept("Bob")).toEqual([]);
  const writes = writesOf(client);
  expect(writes.rows()).toMatchObject([{ detail: { expected: "Bob", text: "Mine" } }]);
  expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "Mine" });
  await answer(1, false);
  expect(writes.rows()).toEqual([]);
});
