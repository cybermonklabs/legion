import { expect, test } from "bun:test";
import { arrival, type FieldStand, NOTHING_KEPT } from "./field-arrival";

/** A field that took "Old" in and holds nothing of the reader's: nothing out, nothing kept. */
const SETTLED: FieldStand = { base: "Old", kept: NOTHING_KEPT, sent: [] };

test("the text the field holds is no change: nothing is left to write, and nothing is taken in", () => {
  expect(arrival("One", "One", { base: "Old", kept: NOTHING_KEPT, sent: ["One"] })).toEqual({
    base: "One",
    kept: [],
    sent: [],
    take: false,
  });
  // The same where another person wrote what the reader holds, typed but not yet written.
  expect(arrival("One", "One", SETTLED)).toEqual({ base: "One", kept: [], sent: [], take: false });
});

test("the answer to a write with a later one still out answers that write alone", () => {
  expect(arrival("One", "Two", { base: "Two", kept: NOTHING_KEPT, sent: ["One", "Two"] })).toEqual({
    base: "Two",
    kept: [],
    sent: ["Two"],
    take: false,
  });
});

test("the answer to an earlier write the reader typed back to leaves the later one out, and the text to write again", () => {
  // "Old A", then "Old AB", are out; the reader is back at "Old A" when its answer arrives.
  const next = arrival("Old A", "Old A", {
    base: "Old AB",
    kept: NOTHING_KEPT,
    sent: ["Old A", "Old AB"],
  });
  // "Old AB" is still out, and the server holds it once it lands: the edit starts from it, so
  // the reader's "Old A" differs from it, and the write waiting for the typing to rest goes.
  expect(next).toEqual({ base: "Old AB", kept: [], sent: ["Old AB"], take: false });
  // That later answer is the reader's own write coming back, and is not taken in over "Old A".
  expect(arrival("Old AB", "Old A", next).take).toBe(false);
});

test("the text the edit started from, with no write over it out, takes nothing of the reader's typing", () => {
  expect(arrival("Old", "Old, mine", SETTLED)).toEqual({ ...SETTLED, take: false });
});

test("another person's text over a field holding nothing of the reader's is taken in, and nothing kept", () => {
  expect(arrival("Written elsewhere", "Old", SETTLED)).toEqual({
    base: "Written elsewhere",
    kept: [],
    sent: [],
    take: true,
  });
});

test("another person's text over the reader's typing is taken in, and their text kept beside it", () => {
  expect(arrival("Written elsewhere", "Old, mine", SETTLED)).toEqual({
    base: "Written elsewhere",
    kept: ["Old, mine"],
    sent: [],
    take: true,
  });
});

test("another person's text over a write still out is taken in, and the text written kept beside it", () => {
  expect(
    arrival("Written elsewhere", "One", { base: "One", kept: NOTHING_KEPT, sent: ["One"] })
  ).toEqual({ base: "Written elsewhere", kept: ["One"], sent: [], take: true });
});

test("a second change while a notice shows keeps both of the reader's texts, newest first", () => {
  // The reader typed over the first change without choosing, and a second arrived.
  const first = arrival("Bob's", "Old, mine", SETTLED);
  const second = arrival("Carol's", "Bob's, mine", first);
  expect(second).toEqual({
    base: "Carol's",
    kept: ["Bob's, mine", "Old, mine"],
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
