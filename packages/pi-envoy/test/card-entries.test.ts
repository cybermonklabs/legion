import { afterAll, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { cardLines, readCardLines } from "./card-entries";

/** Every directory this file made, removed when it has run. */
const scratch: string[] = [];

afterAll(async () => {
  for (const dir of scratch.splice(0)) await rm(dir, { recursive: true, force: true });
});

/** A session file of this file's own, holding `text`. */
async function sessionFile(text: string): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "card-entries-"));
  scratch.push(dir);
  const file = path.join(dir, "session.jsonl");
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
// removed from `readCardLines`, the first assertion below resolves with one card instead of
// rejecting.
test("a card read waits for the file to end on a finished line, and fails when it never does", async () => {
  const torn = await sessionFile(`${card("a")}\n${card("b").slice(0, 60)}`);
  await expect(readCardLines(torn, "alive", 50)).rejects.toThrow(torn);

  const whole = await sessionFile(`${card("a")}\n${card("b")}\n`);
  expect(await readCardLines(whole, "alive", 5_000)).toEqual([card("a"), card("b")]);

  // A writer that is gone is never waited for: nothing will finish its last line, so the
  // fragment is handed on and throws where it is parsed.
  expect(await readCardLines(torn, "gone", 50)).toHaveLength(2);
});
