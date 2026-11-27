// The one writer of the tests' records (`../json-line-record`): the channel server's, the card
// wake's, the typed shell's, the model fallback's, the claim's and the session answers' seams are
// each this writer under a variable of their own, so what holds here holds for all six.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { jsonLineRecord } from "../json-line-record";

const scratch = temporaryDirectory("json-line-record");

test("a variable that names no file gives no record, as every deployment runs", () => {
  expect(jsonLineRecord({}, "LEGION_TEST_RECORD")).toBeUndefined();
  expect(jsonLineRecord({ LEGION_TEST_RECORD: "" }, "LEGION_TEST_RECORD")).toBeUndefined();
});

test("a record appends one JSON line for each step, in the order the program wrote them", () => {
  const file = join(scratch, "steps.jsonl");
  const record = jsonLineRecord<{ readonly step: number }>(
    { LEGION_TEST_RECORD: file },
    "LEGION_TEST_RECORD"
  );
  record?.({ step: 1 });
  record?.({ step: 2 });
  expect(readFileSync(file, "utf8")).toBe('{"step":1}\n{"step":2}\n');
});

test("a write the record cannot make is dropped, and never throws into the program", () => {
  // A path in a directory nobody made: the append cannot create it.
  const file = join(scratch, "no-such-directory", "steps.jsonl");
  const record = jsonLineRecord({ LEGION_TEST_RECORD: file }, "LEGION_TEST_RECORD");
  expect(record).toBeDefined();
  expect(() => record?.({ step: 1 })).not.toThrow();
});
