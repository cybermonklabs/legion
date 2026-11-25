import { expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import * as path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { cardLines, readCardLines } from "./card-entries";

/** A directory of this file's own, removed after the test that made it. */
const scratchDir = temporaryDirectories("card-entries");

/** A session file of this file's own, holding `text`. */
async function sessionFile(text: string): Promise<string> {
  const file = path.join(scratchDir(), "session.jsonl");
  await writeFile(file, text);
  return file;
}

/** A card line as the host writes one into the session file. */
const card = (label: string): string =>
  JSON.stringify({
    type: "custom_message",
    customType: "envoy-message",
    details: { handOver: "idle", summary: label },
  });

const other = JSON.stringify({ type: "assistant", content: "not a card" });

test("the cards of a file are its envoy-message lines, in the order it holds them", () => {
  const text = `${card("a")}\n${other}\n${card("b")}\n`;
  expect(cardLines(text, "alive")).toEqual([card("a"), card("b")]);
});

// The control for the withholding rule: the last line is a card the host has not finished
// writing, cut after both markers, which is the read that threw in packages/pi-legion's
// stall-watchdog case and would throw here too (dispatch://INFRA-2188). Reverting
// `writtenLines` to a plain `split("\n")` turns this case red where it compares the lines,
// which then holds a third entry.
test("a card line the host has not finished writing is withheld while the pane runs", () => {
  const torn = `${card("a")}\n${card("b")}\n${card("c").slice(0, 60)}`;
  expect(torn).toContain('"envoy-message"');
  const lines = cardLines(torn, "alive");
  expect(lines).toEqual([card("a"), card("b")]);
  expect(lines.map((line) => JSON.parse(line))).toHaveLength(2);
});

test("the same fragment is selected once the pane has exited, so parsing it throws", () => {
  const torn = `${card("a")}\n${card("c").slice(0, 60)}`;
  const lines = cardLines(torn, "gone");
  expect(lines).toHaveLength(2);
  expect(() => lines.map((line) => JSON.parse(line))).toThrow(SyntaxError);
});

// The control for the wait: every case that reads cards asserts an exact count of them, and a
// read taken while the host is mid-line would count one short rather than throw. With the wait
// removed from `readCardLines`, the first read below prints the one finished card instead of
// failing. That read runs in a Bun driver of its own, whose deadline (`LEGION_PROOF_DEADLINE_MS`,
// as a proof's script sets one) is half a second after it is spawned, so a file nothing finishes
// fails it there rather than at this row's guard.
test("a card read waits for the file to end on a finished line, and fails when it never does", async () => {
  const torn = await sessionFile(`${card("a")}\n${card("b").slice(0, 60)}`);
  const driver = Bun.spawnSync(
    [
      process.execPath,
      "-e",
      `import { readCardLines } from ${JSON.stringify(path.join(import.meta.dir, "card-entries.ts"))};
await readCardLines(${JSON.stringify(torn)}, "alive").then(
  (lines) => console.log(\`read \${lines.length} cards\`),
  (error) => console.log(String(error))
);`,
    ],
    { env: { ...process.env, LEGION_PROOF_DEADLINE_MS: String(Date.now() + 500) } }
  );
  expect(driver.stdout.toString()).toContain(
    `until gave up waiting for the session file ${torn} to end on a finished line`
  );

  const whole = await sessionFile(`${card("a")}\n${card("b")}\n`);
  expect(await readCardLines(whole, "alive")).toEqual([card("a"), card("b")]);

  // A writer that is gone is never waited for: nothing will finish its last line, so the
  // fragment is handed on and throws where it is parsed.
  expect(await readCardLines(torn, "gone")).toHaveLength(2);
});
