import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { settle } from "../../__tests__/fake-clock";
import { answerPriorityPatches, PRIORITY_TITLE_CHANGED } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import type { Priorities, Priority } from "../../api/types";
import { writesOf } from "../writes/write-store";
import {
  expectedText,
  pressedText,
  settledText,
  type TextChain,
  usePriorityWrites,
} from "./priority-writes";

/** The id the next chain a test starts is given. */
let fresh = 0;

/** A press as the hook makes it: what the write names as `expected`, and the chain after it. */
function press(chain: TextChain | undefined, text: string, held: string) {
  fresh += 1;
  return { chain: pressedText(chain, text, held, fresh), expected: expectedText(chain, held) };
}

/** The chain after one of its writes settled, which a test knows is still there. */
function settledChain(chain: TextChain, refused: boolean): TextChain {
  const next = settledText(chain, refused);
  if (next === undefined) throw new Error("the chain ended");
  return next;
}

test("the first write names the text the page reads", () => {
  expect(press(undefined, "One", "Old").expected).toBe("Old");
});

test("a write pressed while one is out names the text of the one out, which the server holds by its turn", () => {
  const first = press(undefined, "One", "Old");
  const second = press(first.chain, "Two", "Old");
  expect(second.expected).toBe("One");
  // And a third, the second still out after the first landed, names the second's.
  expect(press(settledChain(second.chain, false), "Three", "Old").expected).toBe("Two");
});

test("once the write lands and the page's read has not moved, the next names the text written", () => {
  const first = press(undefined, "One", "Old");
  expect(press(settledChain(first.chain, false), "Two", "Old").expected).toBe("One");
});

test("a read catching up with the writes part way names the last text, and one that caught up ends the chain", () => {
  const first = press(undefined, "One", "Old");
  const second = press(first.chain, "Two", "Old");
  const landed = settledChain(settledChain(second.chain, false), false);
  // The read shows the first write's answer, the second's not yet: the server holds the second.
  const partWay = press(landed, "Three", "One");
  expect(partWay.expected).toBe("Two");
  expect(partWay.chain.id).toBe(landed.id);
  // The read shows the second's answer: the next write starts a chain from it.
  const caughtUp = press(landed, "Three", "Two");
  expect(caughtUp.expected).toBe("Two");
  expect(caughtUp.chain.id).not.toBe(landed.id);
  expect(caughtUp.chain.known).toEqual(["Two", "Three"]);
});

test("once the read moves to another person's text, the next write names that text", () => {
  const first = press(undefined, "One", "Old");
  const landed = settledChain(first.chain, false);
  expect(press(landed, "Two", "Written elsewhere").expected).toBe("Written elsewhere");
});

test("a read showing another person's text while a write is out starts a chain from that text", () => {
  const first = press(undefined, "One", "Old");
  // Another person's text reached the page before "One" was answered: the server holds it, and
  // "One", which names "Old", is refused. A write naming "One" would be refused as well.
  const over = press(first.chain, "Mine", "Written elsewhere");
  expect(over.expected).toBe("Written elsewhere");
  expect(over.chain.id).not.toBe(first.chain.id);
  expect(over.chain.known).toEqual(["Written elsewhere", "Mine"]);
});

test("a read that caught up ends the chain: a text read before it is then another person's", () => {
  const first = press(undefined, "One", "Old");
  // The read caught up with the first write before the second was pressed.
  const second = press(settledChain(first.chain, false), "Two", "One");
  expect(second.expected).toBe("One");
  // Another person writes "Old" again once the second landed: the server holds it, and a write
  // naming "Two" would be refused each time it was pressed.
  expect(press(settledChain(second.chain, false), "Three", "Old").expected).toBe("Old");
});

test("a refused write breaks the chain: the next write names the text read, whether or not one is out", () => {
  const first = press(undefined, "One", "Old");
  const second = press(first.chain, "Two", "Old");
  // The first is refused while the second is out. The second names "One", which the server does
  // not hold, so it is refused too: the next write goes after it naming what the page reads.
  const refused = settledChain(second.chain, true);
  expect(press(refused, "Three", "Written elsewhere").expected).toBe("Written elsewhere");
  expect(press(refused, "Three", "Old").expected).toBe("Old");
  expect(press(refused, "Three", "Old").chain.id).not.toBe(refused.id);
  expect(settledText(refused, false)).toBeUndefined();
  expect(press(settledText(refused, false), "Three", "Written elsewhere").expected).toBe(
    "Written elsewhere"
  );
  // And one refused with nothing else out ends it at once.
  expect(settledText(first.chain, true)).toBeUndefined();
});

const BODY: Priorities = {
  priorities: [],
  work_under_no_priority: { by_project: null, count: 0, issues: null },
  priority_work_parked: { by_priority: null, count: 0, issues: null },
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
 * or a claim started.
 */
function pageWrites(shown: readonly string[] = []) {
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
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], whoami);
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
  /** Draws a page showing `ids`, beside its read of the priorities. */
  const draw = (ids: readonly string[]) =>
    renderHook(
      () => {
        useQuery(prioritiesQuery());
        return usePriorityWrites(ids);
      },
      { wrapper }
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
  return {
    client,
    /** The page's texts kept beside `p1`'s title, where it reads `held`. */
    kept: (held: string) => hook.result.current.kept("p1", "title", held),
    /** The field keeps `texts`, newest first, beside `p1`'s title. */
    keep: (texts: readonly string[]) => act(() => hook.result.current.keep("p1", "title", texts)),
    /** Draws another page, which shows `ids`, on the same app. */
    drawAnother: (ids: readonly string[]) => {
      const other = draw(ids);
      others.push(other);
      return other;
    },
    /** How many times the page has read the priorities. */
    readCount: () => reads.length,
    /** Takes the page away. */
    undraw: () => {
      drawn = false;
      hook.unmount();
    },
    /** Lands or refuses the write that went out `index`th. */
    answer: async (index: number, refused: boolean) => {
      await waitFor(() => expect(answers.length).toBeGreaterThan(index));
      await act(async () => {
        if (refused) answers[index]?.reject(PRIORITY_TITLE_CHANGED);
        else answers[index]?.resolve(SAVED);
        await settle();
      });
    },
    /** Answers the page's `index`th read of the priorities, its first being the page's own. */
    read: async (index: number) => {
      await waitFor(() => expect(reads.length).toBeGreaterThan(index));
      await act(async () => {
        reads[index]?.resolve(BODY);
        await settle();
      });
    },
    /** The reader writes `title` as priority `id`'s, where the page reads `held`. */
    rename: (title: string, held: string, id = "p1") =>
      act(() => hook.result.current.rename(id, title, held, "Rename the priority")),
    /** What the `index`th write to go out changed, once it went. */
    sentAt: async (index: number) => {
      await waitFor(() => expect(sent().length).toBeGreaterThan(index));
      return sent()[index];
    },
    /** How many writes have gone out. */
    sentCount: () => sent().length,
  };
}

test("a write pressed over another person's text starts a chain of its own, which a write still out on the old one leaves alone", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0);
  rename("One", "Old");
  expect(await sentAt(0)).toEqual({ expected: { title: "Old" }, title: "One" });
  // Another person's "Bob" reaches the page before "One" is answered; the reader writes over it.
  rename("Mine", "Bob");
  // "One" is refused, since the server holds "Bob": that is what "Mine" names.
  await answer(0, true);
  expect(await sentAt(1)).toEqual({ expected: { title: "Bob" }, title: "Mine" });
  // The refusal was the old chain's: the next write goes after "Mine", and names it.
  rename("Mine, again", "Bob");
  await answer(1, false);
  expect(await sentAt(2)).toEqual({ expected: { title: "Mine" }, title: "Mine, again" });
});

test("a chain whose writes all landed ends once the page has read them: a text of theirs read again is another person's", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0);
  rename("One", "Old");
  await answer(0, false);
  // The refresh that landing started answers. Later another person writes "Old" back, and the
  // reader writes over it: the server holds "Old", and a write naming "One" would be refused.
  await read(1);
  rename("Two", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "Old" }, title: "Two" });
});

test("a write pressed before the page read what landed goes on the chain, and that read leaves it there", async () => {
  const { answer, read, rename, sentAt } = pageWrites();
  await read(0);
  rename("One", "Old");
  await answer(0, false);
  // The page has not read "One" yet: the next write names it.
  rename("Two", "Old");
  expect(await sentAt(1)).toEqual({ expected: { title: "One" }, title: "Two" });
  // The refresh "One" started answers while "Two" is out: the chain is "Two"'s now, and goes on.
  await read(1);
  rename("Three", "Old");
  await answer(1, false);
  expect(await sentAt(2)).toEqual({ expected: { title: "Two" }, title: "Three" });
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
  rename("Yours", "Old", "p2");
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
