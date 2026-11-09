import { expect, test } from "bun:test";
import { completeLines, writtenLines } from "./transcript-lines";

// The defect these exist for: a pane appends to its transcript while a case polls it, so a read
// can land between a line's bytes and its newline. Reverting `completeLines` to a plain
// `split("\n")` turns the first two cases below red (dispatch://INFRA-2188).
test("completeLines keeps every finished line and nothing after the last newline", () => {
  expect(completeLines("")).toEqual([]);
  expect(completeLines("{}")).toEqual([]);
  expect(completeLines("{}\n")).toEqual(["{}"]);
  expect(completeLines('{"a":1}\n\n{"b":2}\n{"c"')).toEqual(['{"a":1}', '{"b":2}']);
});

test("a last line with no newline yet is withheld while the writer is alive", () => {
  expect(writtenLines('{"a":1}\n{"c"', "alive")).toEqual(['{"a":1}']);
  expect(writtenLines('{"a":1}\n{"b":2}\n', "alive")).toEqual(['{"a":1}', '{"b":2}']);
});

test("every line is handed on once the writer is gone, so a fragment is parsed and throws", () => {
  expect(writtenLines('{"a":1}\n{"c"', "gone")).toEqual(['{"a":1}', '{"c"']);
  expect(() => writtenLines('{"c"', "gone").map((line) => JSON.parse(line))).toThrow(
    /JSON Parse error/
  );
});
