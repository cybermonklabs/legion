import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// stage4b-sandbox-tree.sh has one way to end a check that does not pass: fail, which prints
// `CHECK <check>: FAIL: …` and exits 1. The drivers that tell a read they could not judge from a
// failure print `UNKNOWN: …` and exit 2 (README, step-records-read.sh), so a line of this driver that
// says UNKNOWN and exits 1 says one thing and does another. Each fail line names what failed,
// a check whose count cannot be read among them.
const path = join(import.meta.dir, "..", "stage4b-sandbox-tree.sh");

test("fail prints FAIL and exits 1", () => {
  const run = Bun.spawnSync([
    "bash",
    "-c",
    `${scriptFunctions(path)("fail")}\ncheck=done\nfail "a count cannot be read"`,
  ]);
  expect(run.exitCode).toBe(1);
  // One line, naming the check, FAIL and the words fail was given.
  const out = run.stdout.toString();
  expect(out.endsWith("\n") && !out.slice(0, -1).includes("\n")).toBeTrue();
  for (const part of ["done", "FAIL", "a count cannot be read"]) {
    expect(out).toContain(part);
  }
});

test("no fail line says UNKNOWN", () => {
  const lines = readFileSync(path, "utf8")
    .split("\n")
    .filter((text) => /\bfail "UNKNOWN/.test(text));
  expect(lines).toEqual([]);
});
