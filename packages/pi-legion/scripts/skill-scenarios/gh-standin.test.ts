// The gh stand-in as a skill scenario's agent meets it: through a pipe (`$(gh ...)`, `gh ... | jq`)
// that a loaded machine reads late. Bun drops what a full pipe has not taken when a program calls
// process.exit, so a route's answer longer than the pipe holds must still reach the reader whole.
import { describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { readThroughLatePipe } from "@legion/test-fixtures/late-pipe";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

const standin = path.join(import.meta.dir, "gh-standin.ts");
const scratch = temporaryDirectories("gh-standin");

describe.skipIf(skipWithout("python3"))("gh-standin through a one-page pipe read late", () => {
  test("hands the reader a route's whole answer, past a page, and records the call", async () => {
    const run = scratch();
    const answer = `${"line of a long gh answer\n".repeat(8_000)}the last line\n`;
    writeFileSync(
      path.join(run, "fixtures.json"),
      JSON.stringify({ routes: [{ match: "^gh api repos/o/r/pulls$", stdout: answer, exit: 3 }] })
    );
    const late = await readThroughLatePipe(
      [process.execPath, standin, "gh", "api", "repos/o/r/pulls"],
      { env: { PATH: process.env.PATH, SKILL_SCENARIO_RUN: run } }
    );
    expect(late.pipeBytes).toBe(4_096);
    expect(late.exitCode).toBe(3);
    expect(new TextDecoder().decode(late.stdout)).toBe(answer);
    const [call] = readFileSync(path.join(run, "calls.jsonl"), "utf8").trim().split("\n");
    expect(JSON.parse(call ?? "{}")).toMatchObject({ argv: ["api", "repos/o/r/pulls"], exit: 3 });
  }, 30_000);
});
