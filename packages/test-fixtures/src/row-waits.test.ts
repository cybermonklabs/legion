import { expect, test } from "bun:test";
import { command, commandMs, rowMs, rowOf, rowRoomMs, type Wait } from "./row-waits";

const check = command("the check");
const write: Wait = { what: "the command's write", ms: 45_000 };

test("a row's limit is its waits, one after another, and the room above them", () => {
  expect(rowMs([check, check, write])).toBe(2 * commandMs + 45_000 + rowRoomMs);
  expect(rowMs([])).toBe(rowRoomMs);
});

test("a row that makes the waits its limit counts passes its count", () => {
  const row = rowOf([check, write, check]);
  row.charge(check);
  row.charge(write);
  row.charge(check);
});

// The control of every limit stated so: a limit one wait short fails the row as it makes that
// wait, naming it, whatever the load and however soon the wait would have ended.
test("a wait the row's limit does not count fails the row, naming the wait", () => {
  const short = rowOf([check, check]);
  short.charge(check);
  expect(() => short.charge(write)).toThrow(
    "the row waits for the command's write, for up to 45 s, and its limit counts none: its limit is below what it waits for"
  );

  const once = rowOf([check, write]);
  once.charge(check);
  expect(() => once.charge(check)).toThrow(
    "the row waits for the check, for up to 60 s, and its limit counts 1, each used by an earlier wait or shorter"
  );
});

test("a wait counted shorter than the row makes it fails the row, naming the wait", () => {
  const row = rowOf([{ what: write.what, ms: 30_000 }]);
  expect(() => row.charge(write)).toThrow(
    "the row waits for the command's write, for up to 45 s, and its limit counts 1, each used by an earlier wait or shorter"
  );
});
