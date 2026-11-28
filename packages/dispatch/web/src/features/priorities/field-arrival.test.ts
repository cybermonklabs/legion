import { expect, test } from "bun:test";
import { answered, arrival, type FieldStand, NOTHING_KEPT, type SentText } from "./field-arrival";

/** A field that took "Old" in and holds nothing of the reader's: nothing out, nothing kept. */
const SETTLED: FieldStand = { base: "Old", kept: NOTHING_KEPT, made: "Old", sent: [] };

/** Texts the field wrote, in order, whose writes the page has not answered. */
function out(...texts: string[]): readonly SentText[] {
  return texts.map((text) => ({ landed: false, text }));
}

test("the text the field holds is no change: nothing is left to write, and nothing is taken in", () => {
  expect(
    arrival("One", "One", { base: "Old", kept: NOTHING_KEPT, made: "Old", sent: out("One") })
  ).toEqual({ base: "One", kept: [], made: "One", sent: [], take: false });
  // The same where another person wrote what the reader holds, typed but not yet written.
  expect(arrival("One", "One", SETTLED)).toEqual({
    base: "One",
    kept: [],
    made: "One",
    sent: [],
    take: false,
  });
});

test("the answer to a write with a later one still out answers that write alone", () => {
  expect(
    arrival("One", "Two", { base: "Two", kept: NOTHING_KEPT, made: "Two", sent: out("One", "Two") })
  ).toEqual({
    base: "Two",
    kept: [],
    made: "Two",
    sent: out("Two"),
    take: false,
  });
});

test("the answer to an earlier write the reader typed back to leaves the later one out, and the text to write again", () => {
  // "Old A", then "Old AB", are out; the reader is back at "Old A" when its answer arrives.
  const next = arrival("Old A", "Old A", {
    base: "Old AB",
    kept: NOTHING_KEPT,
    made: "Old AB",
    sent: out("Old A", "Old AB"),
  });
  // "Old AB" is still out, and the server holds it once it lands: the field compares with it, so
  // the reader's "Old A" differs from it, and the write waiting for the typing to rest goes.
  expect(next).toEqual({
    base: "Old AB",
    kept: [],
    made: "Old AB",
    sent: out("Old AB"),
    take: false,
  });
  // That later answer is the reader's own write coming back, and is not taken in over "Old A".
  expect(arrival("Old AB", "Old A", next).take).toBe(false);
});

// "A" over "Old" is out and "AB" over "A" waits behind it. "AB" ends without landing - discarded
// on the shell's line, or refused for its body - and then "A" lands and a read shows it. The field
// still holds "AB": it compares with "AB", so no save writes it again on its own; the reader
// discarded it, or the shell's line holds it, with its Retry. Typing since is theirs to write.
test("the answer to a write after the later one ended without landing leaves the reader's stopped text unwritten", () => {
  const [a, ab] = out("A", "AB") as [SentText, SentText];
  const sent = answered(answered([a, ab], ab, false), a, true);
  expect(arrival("A", "AB", { base: "AB", kept: NOTHING_KEPT, made: "AB", sent })).toEqual({
    base: "AB",
    kept: [],
    made: "AB",
    sent: [],
    take: false,
  });
  // She types on to "ABC", and a read shows "A". The field compares with "A" from then on, but
  // her typing began on "AB": typing not yet written that survives the arrival keeps the text its
  // edit was made on.
  expect(arrival("A", "ABC", { base: "AB", kept: NOTHING_KEPT, made: "AB", sent })).toMatchObject({
    base: "A",
    made: "AB",
  });
});

test("the text the field compares with, with no write over it out, takes nothing of the reader's typing", () => {
  expect(arrival("Old", "Old, mine", SETTLED)).toEqual({ ...SETTLED, take: false });
});

test("another person's text over a field holding nothing of the reader's is taken in, and nothing kept", () => {
  expect(arrival("Written elsewhere", "Old", SETTLED)).toEqual({
    base: "Written elsewhere",
    kept: [],
    made: "Written elsewhere",
    sent: [],
    take: true,
  });
});

test("another person's text over the reader's typing is taken in, and their text kept beside it", () => {
  expect(arrival("Written elsewhere", "Old, mine", SETTLED)).toEqual({
    base: "Written elsewhere",
    kept: ["Old, mine"],
    made: "Written elsewhere",
    sent: [],
    take: true,
  });
});

test("another person's text over a write still out is taken in, and the text written kept beside it", () => {
  expect(
    arrival("Written elsewhere", "One", {
      base: "One",
      kept: NOTHING_KEPT,
      made: "One",
      sent: out("One"),
    })
  ).toEqual({
    base: "Written elsewhere",
    kept: ["One"],
    made: "Written elsewhere",
    sent: [],
    take: true,
  });
});

// The page said "One" landed before any read showed it, and another person's text replaced it: it
// was the server's text until theirs came, as one a read had shown would have been.
test("another person's text over a write that landed, unseen, is taken in, and nothing kept", () => {
  const one = out("One");
  const sent = answered(one, one[0] as SentText, true);
  expect(
    arrival("Written elsewhere", "One", { base: "One", kept: NOTHING_KEPT, made: "One", sent })
  ).toEqual({
    base: "Written elsewhere",
    kept: [],
    made: "Written elsewhere",
    sent: [],
    take: true,
  });
});

test("a second change while a notice shows keeps both of the reader's texts, newest first", () => {
  // The reader typed over the first change without choosing, and a second arrived.
  const first = arrival("Bob's", "Old, mine", SETTLED);
  const second = arrival("Carol's", "Bob's, mine", first);
  expect(second).toEqual({
    base: "Carol's",
    kept: ["Bob's, mine", "Old, mine"],
    made: "Carol's",
    sent: [],
    take: true,
  });
  // With nothing typed since, the second keeps what was kept.
  expect(arrival("Carol's", "Bob's", first).kept).toEqual(["Old, mine"]);
  // A text kept twice is kept once, as the newest.
  expect(arrival("Dan's", "Old, mine", second).kept).toEqual(["Old, mine", "Bob's, mine"]);
  // And a kept text the server now holds goes, whatever arrived it.
  expect(arrival("Old, mine", "Carol's", second).kept).toEqual(["Bob's, mine"]);
});

// The page answers each write of the field's text: one that landed stays until a read shows it,
// and one that did not goes, since no answer of the server's brings it back. The answer is the
// very write's: of a text written twice, the other write of it stays as it was.
test("the page's answer marks a write that landed, and takes out one that did not", () => {
  const [one, two] = out("One", "Two") as [SentText, SentText];
  expect(answered([one, two], one, false)).toEqual([two]);
  expect(answered([one, two], one, true)).toEqual([{ landed: true, text: "One" }, two]);
  const again: SentText = { landed: false, text: "One" };
  expect(answered([one, two, again], again, false)).toEqual([one, two]);
  const landed = answered([one, two, again], one, true);
  expect(landed).toEqual([{ landed: true, text: "One" }, two, again]);
  expect(answered(landed, again, true)).toEqual([
    { landed: true, text: "One" },
    two,
    { landed: true, text: "One" },
  ]);
  // A write no entry stands for any longer - a read answered it - changes nothing.
  const none = [two];
  expect(answered(none, one, false)).toBe(none);
});

// A write that ended with no verdict of Dispatch's - stopped, or refused after an attempt that
// went unanswered - may have landed, or may land still. The page answers it `undefined`: it stays
// in `sent`, as no write still out, so a read that brings its text later is the reader's own
// write coming back - whatever reads showed meanwhile - and nothing is taken in or kept beside the
// field. Her next write goes over the texts it may find, its own and hers, and lands.
test("a write of unknown end stays the reader's own: its text arriving late comes back, and nothing is kept", () => {
  const [a] = out("Old A") as [SentText];
  const stopped = answered([a], a, undefined);
  expect(stopped).toEqual([{ landed: "unknown", text: "Old A" }]);
  // She types on and her next write, "Old AB", goes; "Old A" lands late, before it.
  const ab: SentText = { landed: false, text: "Old AB" };
  const stand: FieldStand = {
    base: "Old AB",
    kept: NOTHING_KEPT,
    made: "Old AB",
    sent: [...stopped, ab],
  };
  expect(arrival("Old A", "Old AB", stand)).toEqual({
    base: "Old AB",
    kept: [],
    made: "Old AB",
    sent: [ab],
    take: false,
  });
  // Or "Old AB" lands first and Dispatch refuses "Old A", which names only "Old": the read shows
  // "Old AB", hers, and that answers both.
  const landed = answered(stand.sent, ab, true);
  expect(arrival("Old AB", "Old AB", { ...stand, sent: landed })).toEqual({
    base: "Old AB",
    kept: [],
    made: "Old AB",
    sent: [],
    take: false,
  });
  // A write of unknown end is no write still out: another person's text arriving over a field
  // that holds nothing else of hers is taken in, and nothing kept, as with one that landed.
  expect(
    arrival("Bob's", "Old A", { ...SETTLED, base: "Old A", made: "Old A", sent: stopped })
  ).toEqual({ base: "Bob's", kept: [], made: "Bob's", sent: [], take: true });
});
